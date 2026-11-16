//go:build stack

// Package e2e is the journeys (the E2E campaign, canyonv2-k6bk) against a stack `make e2e-up`
// started: the real canyon CLI, built once here, and HTTP to the server, the apps, the stub's
// /_control and its request log. The tests never start or stop compose; J5 kills and restarts the
// server, as the journey it checks is a server killed mid-run. `make e2e` runs them.
package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate"
)

const (
	host      = "http://localhost:7777"
	buildTime = 15 * time.Minute // one deploy: the review, a buildpack build, the container's settle window
)

var cli string // the canyon binary TestMain builds

// Every run names its projects afresh: a deleted project's id is never free again (routes.md), so
// a fixed name works once per stack.
var (
	tag       = suffix()
	cpApp     = "e2e-cp-" + tag
	cleanApp  = "e2e-clean-" + tag
	viteApp   = "e2e-vite-" + tag
	refuseApp = "e2e-refuse-" + tag
	sqlApp    = "e2e-sql-" + tag
	vulnApp   = "e2e-vuln-" + tag
)

func suffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "canyon-e2e-")
	if err != nil {
		panic(err)
	}
	cli = filepath.Join(dir, "canyon")
	build := exec.Command("go", "build", "-o", cli, "./cmd/canyon") //nolint:gosec // make cli's build, into a temp dir
	build.Dir, build.Stdout, build.Stderr = "..", os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("building the canyon CLI: " + err.Error())
	}
	code := m.Run()
	forgetRouted()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestStack is the journeys in order on one stack. J4, J5, J7, J14, J12 and J6 are one app's life, each
// starting where the last left it, so a failed one stops the rest.
func TestStack(t *testing.T) {
	if os.Getenv("MODEL") != "" {
		t.Skip("this stack's reviewer is the stub model: J9's (TestModel); these journeys expect none")
	}
	if !ready() {
		t.Skipf("no Canyon stack answers %s/ready: start one with make e2e-up", host)
	}
	for _, id := range []string{cpApp, cleanApp, viteApp, refuseApp, sqlApp, vulnApp, coreApp, scanApp} {
		t.Cleanup(func() { forget(t, id) })
	}
	t.Run("J1 stack up", j1StackUp)
	t.Run("J2 upload and review", j2Review)
	t.Run("J3 approvals", j3Approvals)
	t.Run("J8 GitHub", j8GitHub)
	t.Run("J11 Canyon's scanners on a fresh install", j11Scanners)
	t.Cleanup(func() { dropProdDB(t) })
	core := fixture(t, "f-core", coreApp)
	_ = t.Run("J4 deploy with postgres", func(t *testing.T) { j4Deploy(t, core) }) &&
		t.Run("J5 promote", func(t *testing.T) { j5Promote(t, core) }) &&
		t.Run("J7 secrets and key rotation", func(t *testing.T) { j7Secrets(t, core) }) &&
		t.Run("J14 a checkout's host", func(t *testing.T) { j14Host(t, core) }) &&
		t.Run("J12 rollback", func(t *testing.T) { j12Rollback(t, core) }) &&
		t.Run("J6 undeploy and delete", func(t *testing.T) { j6Delete(t, core) }) &&
		t.Run("J10 audit trail", j10Audit)
}

// The probes (#1235) never read clean: where Canyon's rules read the language, the SQL built from a
// request blocks; the pinned advisory is always found; and Java's SQL, which no rule reads, is named
// as not checked (contracts/finding.md).
func TestProbesNeverReadClean(t *testing.T) {
	if !ready() {
		t.Skipf("no Canyon stack answers %s/ready: start one with make e2e-up", host)
	}
	for _, p := range []struct{ fixture, sql, deps, note string }{
		{"f-flask", "app.py", "requirements.txt", ""},
		{"f-go", "main.go", "go.mod", ""},
		{"f-java", "", "pom.xml", "not checked: sql.string-concat: Java"},
	} {
		t.Run(p.fixture, func(t *testing.T) {
			app := "e2e-" + strings.TrimPrefix(p.fixture, "f-") + "-" + tag
			t.Cleanup(func() { forget(t, app) })
			out, code := check(t, fixture(t, p.fixture, app))
			found := func(rule, file string, blocks bool) bool { // rule is the policy: <policy>.<tool>.<ruleId>
				return slices.ContainsFunc(out.Findings, func(f cliFinding) bool {
					return strings.HasPrefix(f.Rule, rule+".") && f.File == file && f.Blocks == blocks
				})
			}
			noted := slices.ContainsFunc(out.Notes, func(n struct{ Producer, Text string }) bool { return strings.Contains(n.Text, p.note) })
			switch {
			case !found("deps.known-vuln", p.deps, false):
				t.Errorf("no advisory deps.known-vuln in %s", p.deps)
			case p.sql != "" && (code != 2 || !found("sql.string-concat", p.sql, true)):
				t.Errorf("exit %d; want 2, blocked by sql.string-concat in %s", code, p.sql)
			case p.sql == "" && !noted:
				t.Errorf("no note %q", p.note)
			default:
				return
			}
			t.Logf("%s with %+v, notes %+v", out.Result, out.Findings, out.Notes)
		})
	}
}

func j1StackUp(t *testing.T) {
	for _, path := range []string{"/health", "/ready"} {
		if code, body := api(t, "GET", path, nil); code != http.StatusOK {
			t.Fatalf("%s answered %d %s", path, code, body)
		}
	}
	var conf struct {
		Canyon bool   `json:"canyon"`
		Auth   string `json:"auth"`
		Device bool   `json:"device"`
	}
	read(t, "/auth/config", &conf)
	if !conf.Canyon || conf.Auth != "off" || conf.Device {
		t.Fatalf("/auth/config is %+v; want canyon, auth off, no device flow", conf)
	}
	var fleet struct{ Apps []struct{ ID string } }
	read(t, "/api/v1/fleet?limit=200", &fleet)
	for _, demo := range []string{"commission-calc", "claims-portal", "quote-engine", "claims-archive", "fnol-intake", "endorsement-hub", "premium-ledger"} {
		if !slices.ContainsFunc(fleet.Apps, func(a struct{ ID string }) bool { return a.ID == demo }) {
			t.Errorf("the fleet has no %s: the demo seed did not run", demo)
		}
	}
}

func j2Review(t *testing.T) {
	t.Run("F-cp: the key and the producers' notes", func(t *testing.T) {
		out, code := check(t, fixture(t, "f-cp", cpApp))
		if code != 2 || out.Result != "blocked" || len(out.Findings) != 1 || out.Findings[0].Rule != gitleaksStripe {
			t.Fatalf("exit %d, %s with %+v; want exit 2, blocked by the key alone", code, out.Result, out.Findings)
		}
		// The routes are the AI review's, and this host has no model: the scanners' run says auth was
		// not checked, and the reviewer's run that it was skipped.
		notes := []string{}
		for _, n := range out.Notes {
			notes = append(notes, n.Text)
		}
		if want := []string{"not checked: auth.unauthenticated-route: no model is configured on this Canyon host, so the AI review read nothing", gate.Skipped}; !slices.Equal(notes, want) {
			t.Errorf("notes %q; want %q", notes, want)
		}
	})
	t.Run("F-clean: nothing found", func(t *testing.T) {
		if out, code := check(t, fixture(t, "f-clean", cleanApp)); code != 0 || out.Result != "passed" || len(out.Findings) != 0 {
			t.Fatalf("exit %d, %s with %+v; want exit 0, passed, no findings", code, out.Result, out.Findings)
		}
	})
	t.Run("F-vite: a Vite app is checked whole, and nothing found", func(t *testing.T) {
		if out, code := check(t, fixture(t, "f-vite", viteApp)); code != 0 || out.Result != "passed" || len(out.Findings) != 0 {
			t.Fatalf("exit %d, %s with %+v; want exit 0, passed, no findings", code, out.Result, out.Findings)
		}
	})
	t.Run("F-sql: SQL built from user input is what blocks", func(t *testing.T) {
		out, code := check(t, fixture(t, "f-sql", sqlApp))
		blocking := slices.DeleteFunc(slices.Clone(out.Findings), func(f cliFinding) bool { return !f.Blocks })
		if code != 2 || out.Result != "blocked" || len(blocking) != 1 || blocking[0].Rule != "sql.string-concat.opengrep.canyon_sql_rule-request-in-sql-text" || blocking[0].File != "src/app/api/claims/route.ts" {
			t.Fatalf("exit %d, %s with %+v; want exit 2, blocked by Canyon's opengrep SQL rule alone, in the claims route", code, out.Result, out.Findings)
		}
	})
	t.Run("F-vuln: known-vulnerable pins warn, never block", func(t *testing.T) {
		out, code := check(t, fixture(t, "f-vuln", vulnApp))
		lines := []int{}
		for _, f := range out.Findings { // one finding per advisory trivy reads, each under deps.known-vuln
			if strings.HasPrefix(f.Rule, "deps.known-vuln.trivy.") && !f.Blocks && f.File == "package-lock.json" {
				lines = append(lines, f.Line)
			}
		}
		if code != 0 || out.Result != "passed" || len(lines) != len(out.Findings) || !slices.Equal(slices.Compact(slices.Sorted(slices.Values(lines))), []int{20, 24}) {
			t.Fatalf("exit %d, %s with %+v; want exit 0, passed, advisory findings for lodash (line 20) and xlsx (24)", code, out.Result, out.Findings)
		}
	})
	t.Run("upload refusals", func(t *testing.T) {
		if code, body := api(t, "POST", "/api/v1/projects", map[string]string{"id": refuseApp}); code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, body)
		}
		toml := func(resources string) map[string]string {
			return map[string]string{"canyon.toml": "app = \"" + refuseApp + "\"\n\n" + resources}
		}
		for name, c := range map[string]struct {
			files map[string]string
			code  string
		}{
			"an entry escaping the tree": {map[string]string{"../escape": "x"}, "bad_request"},
			"a [resources.db] table":     {toml("[resources.db]\ntype = \"postgres\"\n"), "invalid_resources"},
			"two postgres":               {toml("[resources]\na = { type = \"postgres\" }\nb = { type = \"postgres\" }\n"), "invalid_resources"},
		} {
			code, body := api(t, "POST", "/api/v1/projects/"+refuseApp+"/packages", tarOf(t, c.files))
			if code != http.StatusBadRequest || errorCode(body) != c.code {
				t.Errorf("%s: %d %s; want 400 %s", name, code, body, c.code)
			}
		}
	})
}

// gitleaksStripe is the claims portal's planted key as gitleaks reports it, under secrets.client-bundle.
const gitleaksStripe = "secrets.client-bundle.gitleaks.stripe-access-token"

func j3Approvals(t *testing.T) {
	// J2 left the F-cp project with a blocked run; J3 checks a fresh copy of the same tree.
	cp := fixture(t, "f-cp", cpApp)
	out, _ := check(t, cp)
	secret := only(t, runRead(t, out.GateRun), gitleaksStripe)
	fp := secret.fingerprint()
	reason, expires := "e2e: the fixture's key is not a real key", time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339)
	if code, body := approve(t, out.GateRun, fp, reason, expires); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	type event struct {
		Detail struct{ Fingerprint, Reason string }
	}
	var audit struct{ Events []event }
	read(t, "/api/v1/audit?project="+cpApp+"&limit=200", &audit)
	if !slices.ContainsFunc(audit.Events, func(e event) bool { return e.Detail.Fingerprint == fp && e.Detail.Reason == reason }) {
		t.Errorf("no audit row names the approval of %s", fp)
	}

	appendTo(t, filepath.Join(cp, "apps/web/src/app/globals.css"), "\n/* an unrelated edit */\n")
	out, _ = check(t, cp)
	again := only(t, runRead(t, out.GateRun), gitleaksStripe)
	if again.fingerprint() != fp || again.Approval == nil || again.Approval.Reason != reason {
		t.Errorf("after an unrelated edit the finding is %s with approval %+v; want %s, approved", again.fingerprint(), again.Approval, fp)
	}

	for name, c := range map[string]struct {
		fp, reason, expires string
		status              int
		code                string
	}{
		"past 90 days":        {fp, reason, time.Now().Add(91 * 24 * time.Hour).UTC().Format(time.RFC3339), http.StatusBadRequest, "invalid_expires_at"},
		"an empty reason":     {fp, "", expires, http.StatusBadRequest, "invalid_reason"},
		"unknown fingerprint": {"f_" + strings.Repeat("0", 64), reason, expires, http.StatusNotFound, "not_found"},
	} {
		code, body := approve(t, runOf(t, cpApp), c.fp, c.reason, c.expires)
		if code != c.status || errorCode(body) != c.code {
			t.Errorf("%s: %d %s; want %d %s", name, code, body, c.status, c.code)
		}
	}
}

// helpers

type cliFinding struct {
	Rule, File string
	Line       int
	Blocks     bool
}

// cliRun is `canyon --format json check|deploy` (cli.md §5.4), the keys the journeys read.
type cliRun struct {
	Result     string
	GateRun    string `json:"gate_run"`
	Findings   []cliFinding
	Notes      []struct{ Producer, Text string }
	Deployment *struct{ URL, Env string }
}

// result is one finding of GET /api/v1/gate-runs/:id, a SARIF result plus the approval covering it.
type result struct {
	RuleID              string `json:"ruleId"`
	PartialFingerprints map[string]string
	Properties          struct{ Canyon struct{ Blocks bool } }
	Approval            *struct{ Reason string }
}

func (r result) fingerprint() string { return r.PartialFingerprints["canyon/v1"] }

// canyon runs the CLI in dir against the stack, with a config home of its own and no harness
// variable, so it neither reads this machine's session nor switches to the agent format.
func canyon(t *testing.T, dir, stdin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...) //nolint:gosec // the CLI this test built, with the journey's arguments
	cmd.Dir, cmd.Stdin, cmd.Env = dir, strings.NewReader(stdin), cliEnv(t)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("canyon %v: %v", args, err)
	}
	t.Logf("canyon %s → exit %d\n%s%s", strings.Join(args, " "), cmd.ProcessState.ExitCode(), stdout.String(), stderr.String())
	return stdout.String(), cmd.ProcessState.ExitCode()
}

func cliEnv(t *testing.T) []string {
	home := t.TempDir()
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT", "HOME", "XDG_CONFIG_HOME"}, name)
	})
	return append(env, "CANYON_HOST="+host, "HOME="+home, "XDG_CONFIG_HOME="+home, "CANYON_NO_UPDATE_NOTICE=1", "NO_COLOR=1", "CANYON_ASCII=1")
}

// check is `canyon --format json check` in dir, and its exit code.
func check(t *testing.T, dir string) (cliRun, int) {
	t.Helper()
	return cliJSON(t, dir, "check")
}

func cliJSON(t *testing.T, dir, verb string) (cliRun, int) {
	t.Helper()
	stdout, code := canyon(t, dir, "", "--format", "json", verb)
	var out cliRun
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("canyon %s answered no JSON (exit %d): %v", verb, code, err)
	}
	return out, code
}

// fixture copies e2e/.work/fixtures/<name> (make e2e-up's gen.sh) into a fresh directory and names
// the app id in its canyon.toml, keeping whatever else the file declares.
func fixture(t *testing.T, name, app string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(".work/fixtures", name))); err != nil {
		t.Fatalf("fixture %s (run make e2e-up, which generates it): %v", name, err)
	}
	path := filepath.Join(dir, "canyon.toml")
	body, err := os.ReadFile(path) //nolint:gosec // the fixture copy this test made
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`(?m)^app = ".*"\n`).ReplaceAllString(string(body), "")
	if err := os.WriteFile(path, []byte(`app = "`+app+"\"\n"+named), 0o600); err != nil { //nolint:gosec // the fixture copy this test made
		t.Fatal(err)
	}
	// canyon.toml names the app, so check and deploy refuse one the host lacks (cli.md §3.2): make it
	// first, as e2e/ui/harness/seed.ts does. A 409 exists is an app an earlier journey made.
	if code, out := api(t, "POST", "/api/v1/projects", map[string]string{"id": app}); code != http.StatusCreated && !madeAlready(code, out) {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, code, out)
	}
	return dir
}

// madeAlready is POST /api/v1/projects's 409 exists (contracts/routes.md): the id is taken already.
func madeAlready(code int, body []byte) bool {
	var refused struct{ Error string }
	return code == http.StatusConflict && json.Unmarshal(body, &refused) == nil && refused.Error == "exists"
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // a file of the fixture copy this test made
	if err == nil {
		_, err = f.WriteString(text)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
}

// api is one request to the server: body nil, raw bytes (a tar), or a value sent as JSON.
func api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var in io.Reader
	var kind string
	switch b := body.(type) {
	case nil:
	case []byte:
		in, kind = bytes.NewReader(b), "application/x-tar"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		in, kind = bytes.NewReader(raw), "application/json"
	}
	return send(t, method, host+path, kind, in)
}

func send(t *testing.T, method, url, kind string, in io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, in)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

// read GETs path, which must answer 200, into v.
func read(t *testing.T, path string, v any) {
	t.Helper()
	code, body := api(t, "GET", path, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func errorCode(body []byte) string {
	var e struct{ Error string }
	_ = json.Unmarshal(body, &e) // not JSON is no code, which the caller's comparison reports
	return e.Error
}

// forget deletes a project the journeys made, whatever it holds; one already gone is fine.
func forget(t *testing.T, id string) {
	t.Helper()
	if code, body := api(t, "DELETE", "/api/v1/projects/"+id+"?confirm="+id, nil); code != http.StatusNoContent && code != http.StatusNotFound {
		t.Errorf("deleting %s: %d %s", id, code, body)
	}
}

func runRead(t *testing.T, run string) []result {
	t.Helper()
	var r struct {
		Run struct{ Findings []result } `json:"run"`
	}
	read(t, "/api/v1/gate-runs/"+run, &r)
	return r.Run.Findings
}

// only is the one finding under rule.
func only(t *testing.T, findings []result, rule string) result {
	t.Helper()
	of := slices.DeleteFunc(slices.Clone(findings), func(r result) bool { return r.RuleID != rule })
	if len(of) != 1 {
		t.Fatalf("%d %s findings; want one", len(of), rule)
	}
	return of[0]
}

// runOf is the project's newest run.
func runOf(t *testing.T, project string) string {
	t.Helper()
	var runs struct{ Runs []struct{ ID, State string } }
	read(t, "/api/v1/projects/"+project+"/gate-runs?limit=1", &runs)
	if len(runs.Runs) == 0 {
		t.Fatalf("%s has no run", project)
	}
	return runs.Runs[0].ID
}

func approve(t *testing.T, run, fp, reason, expires string) (int, []byte) {
	t.Helper()
	return api(t, "POST", "/api/v1/gate-runs/"+run+"/findings/"+fp+"/approve", map[string]string{"reason": reason, "expires_at": expires})
}

// tarOf is a tar of files, names written as given: the refusals need names a real tree cannot have.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for name, body := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// J4–J6: e2e-core's life. Three builds in all: J4's deploy and redeploy, J6's redeploy.

func ready() bool {
	res, err := http.Get(host + "/ready") //nolint:noctx // one probe of the local stack
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}
