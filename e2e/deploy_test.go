//go:build stack

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const control = "http://127.0.0.1:8090/_control"

var (
	coreApp = "e2e-core-" + tag
	prodDB  = "e2e_core_prod_" + tag // J5's stand-in for the adapter's database, in app-postgres
)

// j4Deploy approves the claims portal's blocking findings, deploys with a preview database, and
// redeploys a newer version, promoting the first while the second runs.
func j4Deploy(t *testing.T, core string) {
	out, _ := check(t, core)
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, f := range runRead(t, out.GateRun) {
		if f.Properties.Canyon.Blocks {
			if code, body := approve(t, out.GateRun, f.fingerprint(), "e2e: the fixture's findings are planted", expires); code != http.StatusOK {
				t.Fatalf("approve %s: %d %s", f.RuleID, code, body)
			}
		}
	}
	deployed, code := cliJSON(t, core, "deploy")
	if code != 0 || deployed.Deployment == nil || deployed.Deployment.URL != "http://"+coreApp+".preview.localhost:7777" {
		t.Fatalf("deploy: exit %d, %s, deployment %+v; want exit 0 and the preview surface", code, deployed.Result, deployed.Deployment)
	}
	first := deployed.GateRun
	lines := waitRun(t, first, time.Minute)
	// a fresh app's access, after deploy.completed (contracts/events.md, build.log): on this sign-in-off stack (demo)
	// anyone who reaches it opens it, so D5's line says so, never Private (#2498)
	access := "Sign-in is off on this Canyon, so anyone who can reach it can open every app."
	for _, want := range []string{"postgres db: creating", "postgres db: ready", access} {
		if !slices.Contains(lines, want) {
			t.Errorf("no %q on the deploy's timeline: %q", want, lines)
		}
	}
	if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Private: ") }) {
		t.Errorf("the deploy on a sign-in-off stack says the app is private: %q", lines)
	}
	for want := 1; want <= 2; want++ {
		if n := visits(t, coreApp, "preview"); n != want {
			t.Fatalf("preview /db counted %d; want %d", n, want)
		}
	}
	role := ident(coreApp, "preview", "db")
	verifier := psql(t, "SELECT rolpassword FROM pg_authid WHERE rolname = '"+role+"'")
	if verifier == "" {
		t.Fatalf("app-postgres has no login %s", role)
	}
	if s := secretOf(t, coreApp, "preview", "DATABASE_URL"); s == nil || s.Origin != "provisioned" {
		t.Errorf("the preview DATABASE_URL row is %+v; want provisioned", s)
	}
	if code, body := api(t, "DELETE", "/api/v1/projects/"+coreApp+"/secrets/DATABASE_URL?env=preview", nil); code != http.StatusConflict || errorCode(body) != "provisioned" {
		t.Errorf("removing the provisioned DATABASE_URL: %d %s; want 409 provisioned", code, body)
	}

	// A newer version, deployed while the first is promoted: the promote is refused run_in_progress.
	// The first run is still its package's newest and preview runs it, so the receipt passes and
	// the refusal is the run in flight's, not the gate's.
	version := filepath.Join(core, "apps/web/src/app/version/page.tsx")
	if err := os.WriteFile(version, []byte("export default function Version() {\n  return <p id=\"version\">2</p>;\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	redeploy := exec.CommandContext(ctx, cli, "deploy") //nolint:gosec // the CLI this test built
	redeploy.Dir, redeploy.Env, redeploy.Stdout, redeploy.Stderr = core, cliEnv(t), &stdout, os.Stderr
	if err := redeploy.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Minute, "the redeploy's run to start", func() bool { return runOf(t, coreApp) != first })
	if code, body := api(t, "POST", "/api/v1/projects/"+coreApp+"/promotions", map[string]string{"gate_run": first}); code != http.StatusConflict || errorCode(body) != "run_in_progress" {
		t.Errorf("promote during a deploy: %d %s; want 409 run_in_progress", code, body)
	}
	if err := redeploy.Wait(); err != nil {
		t.Fatalf("the redeploy: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "\n  "+access+"\n") { // the deploy's output prints the server's access line under Live (cli.md §3.1)
		t.Errorf("the redeploy's output does not say who can open the app:\n%s", stdout.String())
	}
	if n := visits(t, coreApp, "preview"); n != 3 {
		t.Errorf("after the redeploy preview /db counted %d; want 3, the same database", n)
	}
	if again := psql(t, "SELECT rolpassword FROM pg_authid WHERE rolname = '"+role+"'"); again != verifier {
		t.Error("the redeploy issued the preview login a new password")
	}
}

// j5Promote promotes into a prod database the stub's adapter hands out, refusing a hand-set
// DATABASE_URL first, surviving a server killed while the adapter is pending, then keeping prod
// through a version that crashes on boot.
func j5Promote(t *testing.T, core string) {
	var status struct {
		PromoteRun string `json:"promote_run"`
	}
	read(t, "/api/v1/projects/"+coreApp+"/status", &status)
	promote := func() string {
		t.Helper()
		code, body := api(t, "POST", "/api/v1/projects/"+coreApp+"/promotions", map[string]string{"gate_run": status.PromoteRun})
		var started struct{ Run struct{ ID string } }
		if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil {
			t.Fatalf("promote: %d %s", code, body)
		}
		return started.Run.ID
	}

	// A person's DATABASE_URL in prod: the promotion fails naming it and leaves the row.
	if code, body := api(t, "PUT", "/api/v1/projects/"+coreApp+"/secrets/DATABASE_URL", map[string]string{"value": "set-by-hand", "env": "prod"}); code != http.StatusNoContent {
		t.Fatalf("hand-set DATABASE_URL: %d %s", code, body)
	}
	run := promote()
	lines := append([]string{""}, waitRun(t, run, 2*time.Minute)...)
	if state, last := stateOf(t, run), lines[len(lines)-1]; state != "failed" || !strings.Contains(last, "DATABASE_URL is set by hand") {
		t.Fatalf("promote over a hand-set DATABASE_URL: %s, last line %q; want failed, naming it", state, last)
	}
	if s := secretOf(t, coreApp, "prod", "DATABASE_URL"); s == nil || s.Origin != "user" {
		t.Fatalf("the hand-set row is %+v after the refusal; want it as it was", s)
	}
	if code, body := api(t, "DELETE", "/api/v1/projects/"+coreApp+"/secrets/DATABASE_URL?env=prod", nil); code != http.StatusNoContent {
		t.Fatalf("rm the hand-set DATABASE_URL: %d %s", code, body)
	}

	// The adapter answers pending four times, then a database of app-postgres's this test made.
	password := rand.Text() // base32: nothing to quote in SQL or a URL
	psql(t, "CREATE ROLE "+prodDB+" LOGIN PASSWORD '"+password+"'")
	psql(t, "CREATE DATABASE "+prodDB+" OWNER "+prodDB)
	stub(t, "adapter", http.StatusAccepted, `{}`, 4)
	stub(t, "adapter", http.StatusOK, fmt.Sprintf(`{"outputs":{"DATABASE_URL":"postgres://%s:%s@app-postgres:5432/%s"}}`, prodDB, password, prodDB), 1)
	run = promote()
	waitFor(t, 2*time.Minute, "the promotion to wait on the adapter", func() bool {
		return slices.Contains(timeline(t, run), "postgres db: waiting on the platform")
	})
	compose(t, "kill", "-s", "SIGKILL", "server")
	compose(t, "up", "-d", "server")
	compose(t, "up", "-d", "--no-deps", "--force-recreate", "adapter") // the killed server took the sidecar's namespace with it
	waitFor(t, 2*time.Minute, "the restarted server to be ready", ready)
	lines = waitRun(t, run, 5*time.Minute)
	live := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "live at ") })
	if state := stateOf(t, run); state != "clean" || len(live) != 1 {
		t.Fatalf("the promotion across a server kill: %s with live lines %q; want clean and exactly one", state, live)
	}
	if n := visits(t, coreApp, "prod"); n != 1 {
		t.Errorf("prod /db counted %d; want 1, a database of its own", n)
	}
	var project struct {
		Project struct {
			Envs map[string]struct {
				ImageDigest string `json:"image_digest"`
			}
		}
	}
	read(t, "/api/v1/projects/"+coreApp, &project)
	if p, q := project.Project.Envs["preview"].ImageDigest, project.Project.Envs["prod"].ImageDigest; p == "" || p != q {
		t.Errorf("preview runs %q and prod %q; want the same image", p, q)
	}

	// A version that exits on boot: the promotion fails and prod keeps its container.
	container := docker(t, "inspect", "-f", "{{.Id}}", coreApp+"-prod")
	if _, code := canyon(t, core, "1\n", "secret", "set", "--env", "prod", "CRASH_ON_BOOT"); code != 0 {
		t.Fatalf("secret set: exit %d", code)
	}
	if stdout, code := canyon(t, core, "", "deploy", "--env", "prod", "--yes"); code == 0 || !strings.Contains(stdout, "Nothing was promoted.") {
		t.Errorf("promoting a crash on boot: exit %d; want a failure and nothing promoted", code)
	}
	if again := docker(t, "inspect", "-f", "{{.Id}}", coreApp+"-prod"); again != container {
		t.Errorf("prod runs container %s after the failed promotion; want %s, the one it ran", again, container)
	}
	if n := visits(t, coreApp, "prod"); n != 2 {
		t.Errorf("prod /db counted %d after the failed promotion; want 2", n)
	}
	if _, code := canyon(t, core, "", "secret", "rm", "--env", "prod", "CRASH_ON_BOOT"); code != 0 {
		t.Fatalf("secret rm: exit %d", code)
	}
	if stdout, code := canyon(t, core, "", "deploy", "--env", "prod", "--yes"); code != 0 || !strings.Contains(stdout, "live at http://"+coreApp+".prod.localhost:7777") {
		t.Fatalf("the recovery promotion: exit %d; want 0 and live at the prod surface", code)
	}
	if n := visits(t, coreApp, "prod"); n != 3 {
		t.Errorf("prod /db counted %d after the recovery; want 3", n)
	}
}

// j12Rollback puts an earlier version back in production (design/OPERATIONS.md §1.9): v1 is what J5 and J7 left
// in production, v2 a newer version with an advisory finding, promoted over it. v1 goes back as the image
// production ran; v2, blocked by today's rules once an admin turns its finding's rule blocking, stays out; and once
// three promotions of v2 leave v1's image unkept, its rollback refuses and nothing is built.
func j12Rollback(t *testing.T, core string) {
	v1 := statusOf(t).Envs["prod"].ImageDigest
	version := filepath.Join(core, "apps/web/src/app/version/page.tsx")
	// code.injection, advisory as installed, at high: the vendored eval rule (CWE-95) is opengrep's ERROR, and only a
	// finding at high or above blocks (contracts/snapshot.md)
	calc := filepath.Join(core, "apps/web/src/lib/calc.ts")
	for path, body := range map[string]string{
		version: "export default function Version() {\n  return <p id=\"version\">3</p>;\n}\n",
		calc:    "export function calc(req: { query: { expr: string } }, res: { send: (v: unknown) => void }) {\n  res.send(eval(req.query.expr));\n}\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, code := cliJSON(t, core, "deploy"); code != 0 || out.Deployment == nil {
		t.Fatalf("deploying v2: exit %d, %s", code, out.Result)
	}
	promoteV2 := func() {
		t.Helper()
		if _, code := canyon(t, core, "", "deploy", "--env", "prod", "--yes"); code != 0 {
			t.Fatalf("promoting v2: exit %d", code)
		}
	}
	promoteV2()
	v2 := statusOf(t).Envs["prod"].ImageDigest
	if v2 == v1 {
		t.Fatalf("production runs %s after v2's promotion; want another image", v2)
	}

	// v1 back: the image its history row names, kept, reviewed again and promoted.
	row := earlierOf(t, v1)
	if !row.Kept {
		t.Fatalf("v1 (%s) is not kept while it is production's previous image", row.Version)
	}
	reason := "e2e: J12 puts v1 back"
	began := time.Now()
	if _, code := canyon(t, core, "", "deploy", "--env", "prod", "--version", row.Version, "--reason", reason, "--yes"); code != 0 {
		t.Fatalf("the rollback to v1: exit %d", code)
	}
	t.Logf("the rollback to v1 with its image kept took %s, wall-clock", time.Since(began).Round(time.Second))
	if now := statusOf(t).Envs["prod"].ImageDigest; now != row.ImageDigest {
		t.Errorf("production runs %s after the rollback; want %s, the image v1's history row names", now, row.ImageDigest)
	}
	var audit struct {
		Events []struct {
			Action string
			Detail map[string]any
		}
	}
	read(t, "/api/v1/audit?project="+coreApp+"&limit=50", &audit)
	var deployed map[string]any // the newest deploy prod row's, newest first on the wire
	for _, e := range audit.Events {
		if e.Action == "deploy prod" {
			deployed = e.Detail
			break
		}
	}
	if deployed["rollback"] != true || deployed["replaces"] == nil || deployed["reason"] != reason {
		t.Errorf("the newest deploy prod row holds %v; want rollback, replaces and the reason", deployed)
	}

	// v2, kept as production's previous image, blocked today: an admin turns code.injection blocking.
	policy := func(state string) {
		t.Helper()
		if code, body := api(t, "PUT", "/api/v1/policies/code.injection?apply=true", map[string]string{"state": state}); code != http.StatusOK {
			t.Fatalf("code.injection to %s: %d %s", state, code, body)
		}
	}
	policy("blocking")
	t.Cleanup(func() { policy("advisory") })
	if out, code := canyon(t, core, "", "deploy", "--env", "prod", "--version", earlierOf(t, v2).Version, "--yes"); code != 2 || !strings.Contains(out, `Policy "No user input reaching commands, files, pages or URLs"`) {
		t.Errorf("the rollback to v2, blocked today: exit %d; want 2, its finding under code.injection's policy", code)
	}
	policy("advisory")
	if now := statusOf(t).Envs["prod"].ImageDigest; now != v1 {
		t.Errorf("production runs %s after the blocked rollback; want %s, v1 unchanged", now, v1)
	}

	// v2 promoted three times: a /run keeps the image the newest history row replaced (runs/prod.go, keepSQL), v1
	// through the second, whose row replaced it, so the third keeps v2 alone: v1's image is no longer kept, and its
	// rollback refuses with nothing built and production as it was.
	for range 3 {
		promoteV2()
	}
	if row = earlierOf(t, v1); row.Kept {
		t.Fatalf("v1 (%s) is still kept after three promotions of v2", row.Version)
	}
	began = time.Now()
	since := began.Add(-time.Second).Unix()
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	refused := exec.CommandContext(ctx, cli, "deploy", "--env", "prod", "--version", row.Version, "--yes") //nolint:gosec // the CLI this test built
	refused.Dir, refused.Env = core, cliEnv(t)
	out, _ := refused.CombinedOutput() // the run's last line is the verdict's, on stderr
	t.Logf("canyon deploy --env prod --version %s --yes → exit %d\n%s", row.Version, refused.ProcessState.ExitCode(), out)
	if gone := row.Version + "'s image is no longer on this Canyon, so it can't be put back as it ran."; refused.ProcessState.ExitCode() != 1 || !strings.Contains(string(out), gone) {
		t.Errorf("the rollback to v1 once its image is gone: exit %d; want 1 and %q", refused.ProcessState.ExitCode(), gone)
	}
	t.Logf("the rollback to v1 with its image gone took %s, wall-clock", time.Since(began).Round(time.Second))
	built := docker(t, "events", "--since", strconv.FormatInt(since, 10), "--until", strconv.FormatInt(time.Now().Unix()+1, 10),
		"--filter", "type=container", "--filter", "event=create", "--filter", "label=canyon.build", "--format", "{{.Actor.Attributes.name}}")
	if built != "" { // the executor's build holder, one a build (executor/buildnet.go)
		t.Errorf("the refused rollback built: %s", built)
	}
	if now := statusOf(t).Envs["prod"].ImageDigest; now != v2 {
		t.Errorf("production runs %s after the refused rollback; want %s, unchanged", now, v2)
	}
}

// status is the status read's fields J12 reads (contracts/routes.md, the status row).
type status struct {
	Envs map[string]struct {
		ImageDigest string `json:"image_digest"`
	}
	Earlier []earlier
}

type earlier struct {
	Package, Version string
	ImageDigest      string `json:"image_digest"`
	Kept             bool
}

func statusOf(t *testing.T) status {
	t.Helper()
	var s status
	read(t, "/api/v1/projects/"+coreApp+"/status", &s)
	return s
}

// earlierOf is production's earlier version whose image is digest.
func earlierOf(t *testing.T, digest string) earlier {
	t.Helper()
	rows := statusOf(t).Earlier
	for _, e := range rows {
		if e.ImageDigest == digest {
			return e
		}
	}
	t.Fatalf("production's earlier versions %+v hold no %s", rows, digest)
	return earlier{}
}

// j6Delete undeploys preview and redeploys onto the data it kept, then deletes the project
// through an adapter that answers pending, then refuses, then deletes.
func j6Delete(t *testing.T, core string) {
	before := visits(t, coreApp, "preview")
	if _, code := canyon(t, core, "", "undeploy", "--env", "preview"); code != 0 {
		t.Fatalf("undeploy: exit %d", code)
	}
	var project struct {
		Project struct{ Envs map[string]json.RawMessage }
	}
	read(t, "/api/v1/projects/"+coreApp, &project)
	if _, ok := project.Project.Envs["preview"]; ok {
		t.Error("preview is still on the project after the undeploy")
	}
	db := ident(coreApp, "preview", "db")
	if s := secretOf(t, coreApp, "preview", "DATABASE_URL"); s == nil || psql(t, "SELECT datname FROM pg_database WHERE datname = '"+db+"'") != db {
		t.Fatal("the undeploy lost the preview DATABASE_URL or its database")
	}
	if out, code := cliJSON(t, core, "deploy"); code != 0 || out.Deployment == nil {
		t.Fatalf("redeploy: exit %d, %s", code, out.Result)
	}
	if n := visits(t, coreApp, "preview"); n != before+1 {
		t.Errorf("preview /db counted %d after the redeploy; want %d, the kept database", n, before+1)
	}

	code, body := api(t, "DELETE", "/api/v1/projects/"+coreApp, nil)
	var listed struct{ Resources []struct{ Name, Env string } }
	_ = json.Unmarshal(body, &listed) // no list is the failure below
	if code != http.StatusConflict || errorCode(body) != "confirm_required" || len(listed.Resources) != 2 {
		t.Fatalf("delete without confirm: %d %s; want 409 confirm_required listing the two databases", code, body)
	}
	seen := len(stubLog(t))
	del := func() (int, []byte) { return api(t, "DELETE", "/api/v1/projects/"+coreApp+"?confirm="+coreApp, nil) }
	stub(t, "adapter", http.StatusAccepted, `{}`, 1)
	if code, body := del(); code != http.StatusAccepted {
		t.Fatalf("delete, the adapter pending: %d %s; want 202", code, body)
	}
	if code, body := api(t, "POST", "/api/v1/projects/"+coreApp+"/deploys", map[string]string{}); code != http.StatusConflict || errorCode(body) != "deleting" {
		t.Errorf("deploy while deleting: %d %s; want 409 deleting", code, body)
	}
	stub(t, "adapter", http.StatusBadRequest, `{"message":"e2e refuses"}`, 1)
	if code, body := del(); code != http.StatusBadGateway || errorCode(body) != "teardown_failed" {
		t.Fatalf("delete, the adapter refusing: %d %s; want 502 teardown_failed", code, body)
	}
	if code, body := del(); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s; want 204", code, body)
	}
	if code, _ := api(t, "GET", "/api/v1/projects/"+coreApp, nil); code != http.StatusNotFound {
		t.Errorf("the deleted project answers %d; want 404", code)
	}
	if psql(t, "SELECT datname FROM pg_database WHERE datname = '"+db+"'") != "" {
		t.Errorf("the preview database %s outlived the delete", db)
	}
	var audit struct{ Events []json.RawMessage }
	read(t, "/api/v1/audit?project="+coreApp+"&limit=1", &audit)
	if len(audit.Events) == 0 {
		t.Error("the delete took the project's ledger with it")
	}
	if code, _ := del(); code != http.StatusNotFound {
		t.Errorf("deleting again answers %d; want 404", code)
	}
	var deletes int
	for _, r := range stubLog(t)[seen:] {
		if r.Method == http.MethodDelete && r.Path == "/adapter/"+coreApp+"/prod/db" {
			deletes++
		}
	}
	if deletes != 3 {
		t.Errorf("the adapter was asked to delete prod's database %d times; want 3: pending, refused, gone", deletes)
	}
}

// visits loads the app's /db page in env, which counts one visit in its database, and answers
// the count.
func visits(t *testing.T, app, env string) int {
	t.Helper()
	n, _ := strconv.Atoi(appPage(t, app, env, "/db", "visits")) // \d+
	return n
}

// appPage loads path of the app in env and answers the text of the element with id, which must be
// there. The request goes to the server's port under the app's host name, with the cookie the
// claims portal's middleware wants.
func appPage(t *testing.T, app, env, path, id string) string {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+app+"."+env+".localhost:7777"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "cp_session=x")
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:7777")
	}
	res, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DialContext: dial}}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", env, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	m := regexp.MustCompile(`id="` + id + `"[^>]*>([^<]*)<`).FindSubmatch(body)
	if err != nil || res.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("%s %s: %d %.300s", env, path, res.StatusCode, body)
	}
	return string(m[1])
}

// timeline is a run's build.log lines so far.
func timeline(t *testing.T, run string) []string {
	lines, _ := timelinePage(t, run)
	return lines
}

func timelinePage(t *testing.T, run string) (lines []string, ended bool) {
	t.Helper()
	var after int64
	for {
		var page struct {
			Rows []struct {
				Kind string
				Data struct{ Line string }
			}
			Next *int64
		}
		read(t, fmt.Sprintf("/api/v1/gate-runs/%s/timeline?after=%d&limit=200", run, after), &page)
		for _, r := range page.Rows {
			if r.Kind == "build.log" {
				lines = append(lines, r.Data.Line)
			}
		}
		if page.Next == nil || len(page.Rows) == 0 {
			return lines, page.Next == nil
		}
		after = *page.Next
	}
}

// waitRun polls the run's timeline until it ends, and answers its build.log lines.
func waitRun(t *testing.T, run string, within time.Duration) []string {
	t.Helper()
	var lines []string
	waitFor(t, within, "run "+run+" to end", func() bool {
		var ended bool
		lines, ended = timelinePage(t, run)
		return ended
	})
	return lines
}

func stateOf(t *testing.T, run string) string {
	t.Helper()
	var r struct{ Run struct{ State string } }
	read(t, "/api/v1/gate-runs/"+run, &r)
	return r.Run.State
}

func waitFor(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	for until := time.Now().Add(within); !done(); time.Sleep(2 * time.Second) {
		if time.Now().After(until) {
			t.Fatalf("waited %s for %s", within, what)
		}
	}
}

type secretRow struct{ Name, Env, Origin string }

func secretOf(t *testing.T, project, env, name string) *secretRow {
	t.Helper()
	var list struct{ Secrets []secretRow }
	read(t, "/api/v1/projects/"+project+"/secrets", &list)
	for _, s := range list.Secrets {
		if s.Env == env && s.Name == name {
			return &s
		}
	}
	return nil
}

// ident is the built-in driver's login and database name for a key (provisioners/builtin.go).
func ident(project, env, name string) string {
	sum := sha256.Sum256([]byte(project + "/" + env + "/" + name))
	return "c_" + hex.EncodeToString(sum[:12])
}

// stub scripts the stub's handler to answer status and body the next times requests.
func stub(t *testing.T, handler string, status int, body string, times int) {
	t.Helper()
	raw := fmt.Sprintf(`{"handler":%q,"status":%d,"body":%s,"times":%d}`, handler, status, body, times)
	if code, out := send(t, "POST", control, "application/json", strings.NewReader(raw)); code != http.StatusNoContent {
		t.Fatalf("scripting the stub: %d %s", code, out)
	}
}

type logged struct{ Method, Path, Body string }

// stubLog is every request the stub has logged since the stack came up.
func stubLog(t *testing.T) []logged {
	t.Helper()
	f, err := os.Open(".work/log/requests.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []logged
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 64<<20) // a model request carries the package's source
	for lines.Scan() {
		var r logged
		if err := json.Unmarshal(lines.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// compose runs docker compose on the stack's files from the repo root, as make e2e-up does.
func compose(t *testing.T, args ...string) string {
	t.Helper()
	files := []string{"compose", "-f", "docker-compose.yml", "-f", "e2e/compose.e2e.yml"}
	if os.Getenv("MODEL") != "" {
		files = append(files, "-f", "e2e/compose.model.yml")
	}
	return dockerIn(t, "..", append(files, args...)...)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	return dockerIn(t, "", args...)
}

func dockerIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...) //nolint:gosec // the journey's own docker arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// psql runs one statement in app-postgres as its superuser and answers the unaligned result.
func psql(t *testing.T, sql string) string {
	t.Helper()
	return compose(t, "exec", "-T", "app-postgres", "psql", "-U", "canyon", "-d", "canyon", "-v", "ON_ERROR_STOP=1", "-tAc", sql)
}

// dropProdDB removes the database J5 stands in for the adapter's; the adapter's DELETE is the stub's.
func dropProdDB(t *testing.T) {
	psql(t, "DROP DATABASE IF EXISTS "+prodDB+" WITH (FORCE)")
	psql(t, "DROP ROLE IF EXISTS "+prodDB)
}
