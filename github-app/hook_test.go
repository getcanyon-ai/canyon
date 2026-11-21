package githubapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The fixtures as GitHub sends them, signed with a test word (no credential), pinned: `openssl dgst -sha256 -hmac canyon-test-hook-key < testdata/<file>`.
const hookKey = "canyon-test-hook-key"

var fixtures = map[string]string{
	"installation_created.json": "04490b8dd7908ea5a3b836e3b88907a168bc5b2b752bc7b6fbaf20266e1f0058",
	"pull_request_opened.json":  "fc2b323bafda45b18a27e226350a2dc185a596b6d381058095f3b538b9c42b5e",
	"push_default_branch.json":  "195196207fb663b6e67b785d3c71ee729daa0e55b2b9de28c58f91f7e0a054de",
}

// durable drives the courier; each test run points it at its own schema and delivery ids (DBOS's journal outlives a run).
var (
	durable dbos.Context
	courier = &Courier{Log: log.New(io.Discard, "", 0)}
	run     string
)

func TestMain(m *testing.M) {
	if url := os.Getenv("CANYON_TEST_DATABASE_URL"); url != "" {
		schema := fmt.Sprintf("dbos_github_%d", os.Getpid())
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			durable, err = dbos.NewContext(ctx, dbos.Config{AppName: "canyon-github-test", SystemDBPool: pool, DatabaseSchema: schema, ApplicationVersion: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		}
		if courier.Register(durable); err == nil { // and the gate runs' queue and a gate, which the server's pipeline registers
			dbos.RegisterWorkflow(durable, gate, dbos.WithWorkflowName(runs.Workflow))
			if _, err = dbos.RegisterQueue(durable, runs.Queue); err == nil {
				err = dbos.Launch(durable)
			}
		}
		if err != nil {
			panic(err)
		}
		defer func() {
			_ = dbos.Shutdown(durable, 10*time.Second)
			if admin, err := pgx.Connect(ctx, url); err == nil {
				_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			}
		}()
	}
	poll, busyWait, reportWait = 10*time.Millisecond, time.Millisecond, time.Millisecond
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
}

// review is how the fake gate ends a run; refuse, a text the fake GitHub answers a PATCH carrying with 502;
// scanning, how it answers claims-portal's code scanning; gated, the pool the fake gate writes to. A test
// sets the next scenario's while a workflow or a handler of the last may still read them, so every read
// and write goes through hook (#1128, #2304).
var (
	hooks    sync.Mutex
	review   = clean
	refuse   string
	scanning http.HandlerFunc
	gated    *pgxpool.Pool
)

// hook runs f under the hooks' lock: a test setting them, or a reader taking its copy.
func hook(f func()) {
	hooks.Lock()
	defer hooks.Unlock()
	f()
}

func clean(runs.Input) (string, string, int, any) { return "clean", "", 0, map[string]any{} }

// gate is the gate-run workflow in the server's stead: the verdict row, unless the run failed, then the end.
func gate(ctx dbos.Context, in runs.Input) (string, error) {
	var end func(runs.Input) (string, string, int, any)
	var pool *pgxpool.Pool
	hook(func() { end, pool = review, gated })
	state, reason, blocking, log := end(in)
	if state != "failed" {
		if _, err := events.AppendRun(ctx, pool, in.Run, in.Project, events.Verdict, map[string]any{"state": state, "blocking": blocking, "warnings": 0, "findings": log}); err != nil {
			return "", err
		}
	}
	return "", (&runs.Pipeline{Pool: pool}).End(ctx, in, state, reason, "")
}

// post sends body to acme's App, 9001, as event and delivery id, under the signature header sig.
func post(event, id, sig string, body []byte) *httptest.ResponseRecorder {
	return postTo("9001", event, id, sig, body)
}

// postTo is post to the App the target id header names.
func postTo(target, event, id, sig string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", target)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", run+id)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	courier.ServeHTTP(rec, req)
	return rec
}

// deliver posts testdata/<name> under its pinned signature or, edited by (old, new) pairs, re-signed.
func deliver(t *testing.T, event, id, name string, edits ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name) //nolint:gosec // test: one of the three fixtures
	if err != nil {
		t.Fatal(err)
	}
	sig := fixtures[name]
	for i := 1; i < len(edits); i += 2 {
		body = bytes.Replace(body, []byte(edits[i-1]), []byte(edits[i]), 1)
		mac := hmac.New(sha256.New, []byte(hookKey))
		mac.Write(body)
		sig = hex.EncodeToString(mac.Sum(nil))
	}
	return post(event, id, "sha256="+sig, body)
}

// hooked points the courier at a fresh schema and a fake GitHub: check runs recorded as "METHOD repo body",
// more than 50 annotations in one request refused as GitHub does; claims-portal's tarball a symlink at a
// sha of 5s, a canyon.toml (refused) at 7s, 404 at 4s, another file at 9s, else one file.
func hooked(t *testing.T) (*Registration, *pgxpool.Pool, chan string) {
	t.Helper()
	g, conv, pool := registration(t)
	if durable == nil {
		t.Skip("no DBOS context without a database")
	}
	calls := make(chan string, 8)
	record := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var run CheckRun
		if json.Unmarshal(body, &run) != nil || run.Output != nil && len(run.Output.Annotations) > 50 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var refused string
		hook(func() { refused = refuse })
		if refused != "" && strings.Contains(string(body), refused) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		calls <- r.Method + " " + r.PathValue("repo") + " " + string(body)
		_, _ = io.WriteString(w, `{"id":7}`)
	}
	fake := http.NewServeMux()
	fake.HandleFunc("POST /repos/acme/{repo}/check-runs", record)
	fake.HandleFunc("PATCH /repos/acme/{repo}/check-runs/7", record)
	fake.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":"fake-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/tarball/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha")[0] == '4' {
			http.NotFound(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		root, sha := "acme-claims-portal-"+r.PathValue("sha")[:7]+"/", r.PathValue("sha")
		_ = tw.WriteHeader(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755})
		if sha[0] == '3' { // a project deleted while its package is made
			_, _ = courier.Pool.Exec(r.Context(), "UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'")
		}
		if sha[0] == '6' { // a monorepo: two apps' folders (#1917)
			for _, name := range []string{"apps/billing/index.js", "apps/other/x.js"} {
				_ = tw.WriteHeader(&tar.Header{Name: root + name, Mode: 0o644, Size: 2})
				_, _ = tw.Write([]byte("1\n"))
			}
			_, _ = tw.Close(), gz.Close()
			return
		}
		if app := map[byte]string{'c': "other-app", 'd': "claims-portal"}[sha[0]]; app != "" { // canyon.toml naming another app, or this one (#1938)
			line := "app = \"" + app + "\"\n"
			_ = tw.WriteHeader(&tar.Header{Name: root + "canyon.toml", Mode: 0o644, Size: int64(len(line))})
			_, _ = tw.Write([]byte(line))
			_, _ = tw.Close(), gz.Close()
			return
		}
		h := map[byte]*tar.Header{'5': {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, '7': {Name: "canyon.toml", Mode: 0o644, Size: 2}, '8': {Name: "checked.js", Mode: 0o644, Size: 2}, '9': {Name: "pr.js", Mode: 0o644, Size: 2}}[sha[0]]
		if h == nil {
			h = &tar.Header{Name: "index.js", Mode: 0o644, Size: 2}
		}
		h.Name = root + h.Name
		_ = tw.WriteHeader(h)
		_, _ = tw.Write([]byte("1\n"))
		_, _ = tw.Close(), gz.Close()
	})
	fake.HandleFunc("GET /repos/acme/{repo}/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) { // apps/billing's tree is billing1 in every commit but 60…, which has no apps/billing
		sha := r.PathValue("sha")
		tree := map[string]string{"apps1": `[{"path":"billing","type":"tree","sha":"billing1"},{"path":"other","type":"tree","sha":"other1"}]`, "apps0": `[{"path":"other","type":"tree","sha":"other1"}]`,
			"apps9": `[{"path":"billing","type":"tree","sha":"billing9"},{"path":"other","type":"tree","sha":"other9"}]`}[sha]
		if len(sha) == 40 {
			tree = `[{"path":"apps","type":"tree","sha":"apps1"}]`
			if strings.HasPrefix(sha, "60") {
				tree = `[{"path":"apps","type":"tree","sha":"apps0"}]`
			}
			if strings.HasPrefix(sha, "9f") { // the pull request's head: both folders moved
				tree = `[{"path":"apps","type":"tree","sha":"apps9"}]`
			}
		}
		_, _ = io.WriteString(w, `{"tree":`+tree+`,"truncated":false}`)
	})
	fake.HandleFunc("GET /repos/acme/{repo}/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) { // a person's own read (TestPackRef)
		permission := map[string]string{"carol-gh": "read", "nobody-gh": "none"}[r.PathValue("login")]
		if permission == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"permission":"`+permission+`"}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) { // the refs PackRef resolves (TestPackRef)
		sha := refs[r.PathValue("ref")]
		if sha == "" && len(r.PathValue("ref")) == 40 {
			sha = r.PathValue("ref")
		}
		if sha == "" {
			http.Error(w, `{"message":"No commit found for SHA: `+r.PathValue("ref")+`"}`, http.StatusUnprocessableEntity)
			return
		}
		_, _ = io.WriteString(w, `{"sha":"`+sha+`"}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/branches/{name...}", func(w http.ResponseWriter, r *http.Request) { // the repository's own branches: every ref of refs but the tag
		sha := refs[r.PathValue("name")]
		if sha == "" || r.PathValue("name") == "v1.0" {
			http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"name":"`+r.PathValue("name")+`","commit":{"sha":"`+sha+`"}}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/git/ref/tags/{name...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "v1.0" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"ref":"refs/tags/v1.0","object":{"type":"commit","sha":"`+refs["v1.0"]+`"}}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"full_name":"acme/claims-portal","default_branch":"main"}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) { // main...<sha>: a commit of refs is on main; any other, a fork's, diverged
		base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
		status := "diverged"
		switch {
		case base == "main" && head == refs["main"]:
			status = "identical"
		case base == "main" && slices.Contains(slices.Collect(maps.Values(refs)), head):
			status = "behind"
		}
		_, _ = io.WriteString(w, `{"status":"`+status+`"}`)
	})
	fake.HandleFunc("GET /repos/acme/claims-portal/code-scanning/", func(w http.ResponseWriter, r *http.Request) {
		var answer http.HandlerFunc
		hook(func() { answer = scanning })
		answer(w, r)
	})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	courier.Pool, courier.Vault, courier.API, courier.Repos, courier.Durable = pool, g.Vault, srv.URL, t.TempDir(), durable
	courier.Declare = func(tree string) (map[string]string, map[string]string, *string, error) {
		if _, err := os.Stat(tree + "/canyon.toml"); err == nil {
			return nil, nil, nil, errors.New("canyon.toml is not TOML")
		}
		description := "Files and tracks insurance claims"
		return map[string]string{"db": "postgres"}, map[string]string{"approver": "Approves claims over 10,000 EUR"}, &description, nil
	}
	courier.FirstDeclarations = func(_ context.Context, _ pgx.Tx, _ string, d map[string]string) ([]string, error) {
		return slices.Sorted(maps.Keys(d)), nil
	}
	courier.Policy = func(context.Context, pgx.Tx, string) (policy.Snapshot, error) {
		return policy.Snapshot{Limits: policy.Limits{GateRunsPerProjectPerHour: 30, GateRunsPerActorPerHour: 60}}, nil
	}
	hook(func() { review, refuse, scanning, gated = clean, "", http.NotFound, pool })
	conv.secret, run = hookKey, t.Name()+rand.Text()
	return g, pool, calls
}

// linked makes the claims-portal app and links it to the fixtures' repository, 81723, at its root,
// seen by installation 42 of acme's App (#1917).
func linked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')",
		"INSERT INTO github_installations (installation_id, account, app_id) VALUES (42, 'acme', 9001) ON CONFLICT DO NOTHING",
		"INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('claims-portal', 81723, '', 42, 'acme/claims-portal')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// ended waits for delivery id's workflow, whatever it returned, and answers how the check ended: the PATCHes after the POST.
func ended(t *testing.T, calls chan string, id string) []string {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[string](durable, run+id+"/claims-portal") // a linked app's check (#1917)
	if err != nil {
		h, err = dbos.RetrieveWorkflow[string](durable, run+id) // a repository no app is linked to
	}
	if err != nil {
		t.Fatalf("the check of %s: %v", id, err)
	}
	_, _ = h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	if len(calls) < 2 || !strings.Contains(<-calls, ` {"name":"Canyon gate`) { // returned: every call was made, or never will be
		t.Fatalf("the check of %s was not opened as the Canyon gate and then ended (%d call(s) left)", id, len(calls))
	}
	patches := make([]string, len(calls))
	for i := range patches {
		patches[i] = <-calls
	}
	return patches
}

// Judged in the contract's order, an event not served (a fork's pull request included) is 204 and writes
// nothing; a push to the default branch becomes the current package once however often delivered, with
// its first declarations audited, and the check ends as its review did, failing on a refusal, or neutral
// naming the app to create. A pull request's head is reviewed and never current until a push brings its tree.
func TestAPushToTheDefaultBranchIsPackagedOnceAndTheCheckEnds(t *testing.T) {
	g, pool, calls := hooked(t)
	if rec := post("push", "d-big", "", make([]byte, 5<<20+1)); rec.Code != 413 || !strings.Contains(rec.Body.String(), `"too_large"`) {
		t.Fatalf("past 5 MiB, before anything else: %d %s", rec.Code, rec.Body)
	}
	if rec := deliver(t, "push", "d-1", "push_default_branch.json"); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"not_configured"`) {
		t.Fatalf("no App: %d %s", rec.Code, rec.Body)
	}
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	push := fixtures["push_default_branch.json"]
	for name, sig := range map[string]string{"another body's": "sha256=" + fixtures["installation_created.json"], "no": "", "a bare hex": push, "a prefix of the": "sha256=" + push[:32]} {
		body, _ := os.ReadFile("testdata/push_default_branch.json") //nolint:gosec // test: a fixture
		if rec := post("push", "d-1", sig, body); rec.Code != 401 || !strings.Contains(rec.Body.String(), `"bad_signature"`) {
			t.Errorf("%s signature: %d %s", name, rec.Code, rec.Body)
		}
	}
	linked(t, pool)
	// the app's name, and the sender's numeric id as GitHub sends it
	app := []string{`"name":"billing-api"`, `"name":"claims-portal"`, `"login":"dana-acme","type":"User"`, `"login":"dana-acme","id":583231,"type":"User"`}
	for _, d := range [][]string{ // in order: code, event, delivery id, fixture, edits; the push twice is one delivery
		{"202", "installation", "d-install", "installation_created.json"}, {"202", "installation", "d-uninstall", "installation_created.json", `"created"`, `"deleted"`},
		{"204", "push", "a push after the uninstall", "push_default_branch.json"}, {"202", "installation", "d-reinstall", "installation_created.json"},
		{"204", "pull_request", "a closed pull request", "pull_request_opened.json", `"opened"`, `"closed"`}, {"204", "pull_request", "a fork's", "pull_request_opened.json", `"acme/claims-portal"}}`, `"mallory/claims-portal"}}`},
		{"204", "issues", "an issue", "push_default_branch.json"}, {"204", "push", "a push to another branch", "push_default_branch.json", "refs/heads/main", "refs/heads/x"},
		{"204", "push", "a push from an installation never live", "push_default_branch.json", `"id":42`, `"id":43`},
		{"202", "push", "d-push", "push_default_branch.json", app[0], app[1], app[2], app[3]}, {"202", "push", "d-push", "push_default_branch.json", app[0], app[1], app[2], app[3]},
	} {
		if rec := deliver(t, d[1], d[2], d[3], d[4:]...); strconv.Itoa(rec.Code) != d[0] || rec.Code == 202 && rec.Body.String() != "{}\n" {
			t.Fatalf("%s: %d %q, want %s", d[2], rec.Code, rec.Body, d[0])
		}
	}
	patch := ended(t, calls, "d-push")[0]
	if n := rows(t, pool, "SELECT count(*) FROM runs WHERE started_by_subject = 'github:dana-acme' AND started_by_github = 583231"); n != 1 {
		t.Fatalf("the push's run keeps the pushing account's numeric id, whoever linked it its author (#2228): %d run(s)", n)
	}
	var pkg, who string
	if err := pool.QueryRow(ctx, `SELECT p.id, p.source_kind || ' ' || p.source_ref || ' ' || p.source_actor || ' ' || p.resources::text || ' ' || p.roles::text || ' ' || p.description || ' ' || a.actor_kind || ' ' || a.actor_display || ' ' || (a.detail->>'package')
		FROM packages p, audit_events a WHERE a.action = 'package from github' ORDER BY p.created_at DESC LIMIT 1`).Scan(&pkg, &who); err != nil {
		t.Fatal(err)
	}
	if who != `repo a1b2c3d4e5f60718293a4b5c6d7e8f9001122334 github:dana-acme {"db": "postgres"} {"approver": "Approves claims over 10,000 EUR"} Files and tracks insurance claims agent dana-acme `+pkg ||
		!strings.Contains(patch, `"status":"completed","conclusion":"success","output":{"title":"No blocking findings","summary":"Canyon reviewed `+pkg+`: no blocking findings, 0 warnings."}`) ||
		rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource' AND detail->>'package' = '"+pkg+"'") != 1 {
		t.Fatalf("the current package, its audit row and the check: %s\n%s", who, patch)
	}
	if mine := "action IN ('package from github', 'declare resource', 'gate run from github')"; rows(t, pool, "SELECT count(*) FROM audit_events WHERE "+mine+" AND via = 'github'") < 2 ||
		rows(t, pool, "SELECT count(*) FROM audit_events WHERE "+mine+" AND via IS DISTINCT FROM 'github'") != 0 { // the App's work, never Canyon's own
		t.Fatal("the App's package, resource and gate run rows say via github")
	}
	if n := rows(t, pool, "SELECT count(*) FROM github_deliveries") + rows(t, pool, "SELECT count(*) FROM packages"); n != 5 {
		t.Fatalf("three installation deliveries, and one delivery and one package however often the push came: %d row(s)", n)
	}
	head := func() string {
		out, _ := exec.Command("git", "--git-dir="+courier.Repos+"/claims-portal.git", "rev-parse", "main").Output() //nolint:gosec // test: the courier's own repos dir
		return string(out)
	}
	pushed, pr := head(), "9f1c2e3d4b5a69788796a5b4c3d2e1f00f1e2d3c"
	for _, d := range [][]string{{"pull_request", "d-pr", "pull_request_opened.json", "repo-pr"}, {"push", "d-merge", "push_default_branch.json", "repo", app[0], app[1], "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", pr}} {
		if rec := deliver(t, d[0], d[1], d[2], d[4:]...); rec.Code != 202 {
			t.Fatalf("%s: %d %s", d[1], rec.Code, rec.Body)
		}
		patch, kind := ended(t, calls, d[1])[0], ""
		if err := pool.QueryRow(ctx, "SELECT source_kind FROM packages WHERE source_ref = $1", pr).Scan(&kind); err != nil || kind != d[3] || !strings.Contains(patch, `"conclusion":"success"`) {
			t.Fatalf("%s: the package is %q (%v), want %s; %s", d[1], kind, err, d[3], patch)
		}
		if d[1] == "d-pr" && (head() != pushed || rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'") != 1) {
			t.Fatalf("a pull request moved main (%s, was %s) or declared for the project", head(), pushed)
		}
		if d[1] == "d-pr" && rows(t, pool, "SELECT count(*) FROM packages WHERE source_kind = 'repo-pr' AND pr_number = 12 AND pr_branch = 'fix-query'") != 1 {
			t.Fatal("a pull request's package does not name its number and branch (#1947)")
		}
	}
	var current string // headPackage's rule: the newest package not a pull request's
	if err := pool.QueryRow(ctx, "SELECT source_ref FROM packages WHERE source_kind <> 'repo-pr' ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&current); err != nil || current != pr ||
		rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'") != 2 {
		t.Fatalf("the merge makes the pull request's package current (%s, %v) and declares what it declares", current, err)
	}
	for _, d := range [][]string{ // the delivery id, how the check ends, the edits
		{"d-symlink", `"conclusion":"failure","output":{"title":"Canyon refused this commit","summary":"bad package: entry \"acme-claims-portal-5555555/link\": only files and directories are accepted"}`, app[0], app[1], "a1b2c3d4", "55555555"},
		{"d-toml", `"conclusion":"failure","output":{"title":"Canyon refused this commit","summary":"canyon.toml is not TOML"}`, app[0], app[1], "a1b2c3d4", "77777777"},
		{"d-no-app", `"conclusion":"neutral","output":{"title":"This repository isn't linked to a Canyon app","summary":"An admin links it in Settings › GitHub."}`, `"id":81723`, `"id":999`},
	} {
		if rec := deliver(t, "push", d[0], "push_default_branch.json", d[2:]...); rec.Code != 202 {
			t.Fatalf("%s: %d %s", d[0], rec.Code, rec.Body)
		} else if patch := ended(t, calls, d[0])[0]; !strings.Contains(patch, d[1]) {
			t.Errorf("%s ends the check %s", d[0], patch)
		}
	}
	// a push of a tree only a check packaged (#1251) makes that package current as repo, as the merge does a pull request's,
	// and declares for the project, once, what the check's row holds: the known tree is not read again
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	_ = tw.WriteHeader(&tar.Header{Name: "acme-claims-portal-8888888/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "acme-claims-portal-8888888/checked.js", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("1\n"))
	_ = tw.Close()
	checked, err := ingest.IngestChecked(ctx, courier.Repos+"/claims-portal.git", &tree, courier.Exclusions, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, resources) VALUES ($1, 'claims-portal', $2, $3, 1, 'check', '{\"cache\": \"s3\"}')", checked.ID, checked.TreeHash, checked.Commit); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, "push", "d-checked", "push_default_branch.json", app[0], app[1], "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", "8888888888888888888888888888888888888888"); rec.Code != 202 {
		t.Fatalf("d-checked: %d %s", rec.Code, rec.Body)
	}
	ended(t, calls, "d-checked")
	var kind string
	if err := pool.QueryRow(ctx, "SELECT id, source_kind FROM packages WHERE source_kind NOT IN ('repo-pr', 'check') ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&current, &kind); err != nil || current != checked.ID || kind != "repo" ||
		rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource' AND detail->>'name' = 'cache' AND detail->>'package' = '"+checked.ID+"'") != 1 {
		t.Fatalf("after a push of a checked tree the current package is %s (%s, %v), want %s as repo, its cache declared once", current, kind, err, checked.ID)
	}
}

// The hook opens only the webhook secret: a private key that will not open does not stop a delivery.
// The check still ends when the commit cannot be fetched: failing, the reason left to the log.
func TestTheHookNeedsOnlyItsSecretAndAnUnfetchedCommitEndsTheCheck(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	var pem []byte
	if err := pool.QueryRow(ctx, "UPDATE github_app SET sealed_pem = 'unopenable' RETURNING (SELECT sealed_pem FROM github_app)").Scan(&pem); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, "installation", "d-install", "installation_created.json"); rec.Code != 202 {
		t.Fatalf("with the private key unopenable: %d %s", rec.Code, rec.Body)
	}
	if _, err := pool.Exec(ctx, "UPDATE github_app SET sealed_pem = $1", pem); err != nil {
		t.Fatal(err)
	}
	linked(t, pool)
	if rec := deliver(t, "push", "d-404", "push_default_branch.json", `"name":"billing-api"`, `"name":"claims-portal"`, "a1b2c3d4", "44444444"); rec.Code != 202 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	} else if patch := ended(t, calls, "d-404")[0]; !strings.Contains(patch, `"conclusion":"failure","output":{"title":"Canyon could not finish this review"`) {
		t.Fatalf("an unfetched commit ends the check %s", patch)
	}
}

// Each org's App is told apart by the target id header (#1914): a delivery is checked under the secret
// of the App it names, and an installation is that App's alone.
func TestTheHookPicksTheAppByTargetID(t *testing.T) {
	g, pool, _ := hooked(t)
	for _, step := range []struct{ query, code string }{{"org=acme&imports_new=true&keeps_code=false", "good"}, {"org=acme-labs&imports_new=false&keeps_code=false", "labs"}} {
		if _, cookie := beginFor(g, step.query); finish(g, cookie, onlyState(cookie), step.code).Code != 302 {
			t.Fatalf("%s did not register", step.code)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE github_app SET (sealed_hook_key, sealed_hook_secret) = (SELECT sealed_hook_key, sealed_hook_secret FROM github_app WHERE app_id = 9001) WHERE app_id = 9002"); err != nil {
		t.Fatal(err) // both Apps sign with the test word, so only the header tells them apart
	}
	body, err := os.ReadFile("testdata/installation_created.json")
	if err != nil {
		t.Fatal(err)
	}
	sig := "sha256=" + fixtures["installation_created.json"]
	for _, target := range []string{"", "9999", "nine", "-9001"} {
		if rec := postTo(target, "installation", "d-"+target, sig, body); rec.Code != 401 || !strings.Contains(rec.Body.String(), `"bad_signature"`) {
			t.Errorf("target %q: %d %s", target, rec.Code, rec.Body)
		}
	}
	if rec := postTo("9002", "installation", "d-labs-on-acme", sig, body); rec.Code != 204 {
		t.Errorf("acme-labs' App installed on acme, which it is not: %d %s", rec.Code, rec.Body)
	}
	labs := bytes.Replace(bytes.Replace(body, []byte(`"login":"acme"`), []byte(`"login":"acme-labs"`), 1), []byte(`"id":4242`), []byte(`"id":4243`), 1)
	mac := hmac.New(sha256.New, []byte(hookKey))
	mac.Write(labs)
	if rec := postTo("9002", "installation", "d-labs", "sha256="+hex.EncodeToString(mac.Sum(nil)), labs); rec.Code != 202 {
		t.Fatalf("installed under acme-labs' App, on acme-labs: %d %s", rec.Code, rec.Body)
	}
	if rec := postTo("9001", "installation", "d-steal", sig, body); rec.Code != 204 { // installation 42 is acme-labs' App's, and stays so
		t.Errorf("acme's App naming acme-labs' installation: %d %s", rec.Code, rec.Body)
	}
	push, err := os.ReadFile("testdata/push_default_branch.json")
	if err != nil {
		t.Fatal(err)
	}
	if rec := postTo("9001", "push", "d-acme", "sha256="+fixtures["push_default_branch.json"], push); rec.Code != 204 {
		t.Errorf("a push to acme's App from an installation of acme-labs' App: %d %s", rec.Code, rec.Body)
	}
	if n := rows(t, pool, "SELECT count(*) FROM github_installations WHERE app_id = 9002") + rows(t, pool, "SELECT count(*) FROM github_deliveries WHERE app_id = 9002"); n != 2 {
		t.Errorf("the installation and its delivery are acme-labs' App's: %d", n)
	}
}

// A repository named like an app that is not linked to it changes nothing of it: the match is by
// link, never by name (#1917's regression test). Its push is one neutral check.
func TestASameNamedRepoThatIsNotLinkedChangesNothing(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	if _, err := pool.Exec(ctx, "DELETE FROM project_repositories"); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, "push", "d-named", "push_default_branch.json", `"name":"billing-api"`, `"name":"claims-portal"`); rec.Code != 202 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	if patch := ended(t, calls, "d-named")[0]; !strings.Contains(patch, `"conclusion":"neutral","output":{"title":"This repository isn't linked to a Canyon app"`) {
		t.Errorf("the check: %s", patch)
	}
	if n := rows(t, pool, "SELECT count(*) FROM packages") + rows(t, pool, "SELECT count(*) FROM audit_events WHERE project_id = 'claims-portal'"); n != 0 {
		t.Fatalf("an unlinked repository named claims-portal left %d row(s) on the claims-portal app", n)
	}
}

// A folder link packages its folder alone, as its own check, and only when the folder's tree moved
// since the link's last push; a commit without the folder fails (#1917).
func TestAFolderLinkPackagesItsFolderOnlyWhenItChanged(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE project_repositories SET folder = 'apps/billing'"); err != nil {
		t.Fatal(err)
	}
	name := `"name":"billing-api"`
	for _, step := range []struct{ id, sha, want string }{
		{"d-first", "6666666666666666666666666666666666666666", `"conclusion":"success"`},
		{"d-same", "6767676767676767676767676767676767676767", `"conclusion":"neutral","output":{"title":"No change under apps/billing","summary":"Nothing under apps/billing changed since this app's last version, so there is nothing new to review."}`},
		{"d-gone", "6060606060606060606060606060606060606060", `"conclusion":"failure","output":{"title":"apps/billing isn't in this commit","summary":"Canyon packages this app from apps/billing. Move it back, or an admin links the app to its new folder."}`},
	} {
		if rec := deliver(t, "push", step.id, "push_default_branch.json", name, `"name":"claims-portal"`, "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", step.sha); rec.Code != 202 {
			t.Fatalf("%s: %d %s", step.id, rec.Code, rec.Body)
		}
		if patch := ended(t, calls, step.id)[0]; !strings.Contains(patch, step.want) {
			t.Errorf("%s ends the check %s", step.id, patch)
		}
	}
	var files string
	if err := pool.QueryRow(ctx, "SELECT p.commit FROM packages p WHERE p.project_id = 'claims-portal'").Scan(&files); err != nil {
		t.Fatalf("one package: %v", err)
	}
	out, err := exec.Command("git", "--git-dir="+courier.Repos+"/claims-portal.git", "ls-tree", "-r", "--name-only", files).Output() //nolint:gosec // test: the courier's own repos dir
	if err != nil || string(out) != "index.js\n" {
		t.Fatalf("the package is the folder's alone, at its root: %q %v", out, err)
	}
	if n := rows(t, pool, "SELECT count(*) FROM project_repositories WHERE last_tree = 'billing1'"); n != 1 {
		t.Fatalf("the link's last tree: %d", n)
	}
}

// What GitHub says of a repository keeps its links: its new name, archived and back, out of the App's
// reach and back, deleted; each change of state is one audit row on the app (#1917).
func TestRepositoryEventsKeepTheLinks(t *testing.T) {
	g, pool, _ := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	repo := `"repository":{"id":81723,"name":"claims","full_name":"acme/claims","owner":{"login":"acme"}},"installation":{"id":42},"sender":{"login":"dana-acme"}`
	list := func(key string) string {
		return `"` + key + `":[{"id":81723,"full_name":"acme/claims"}],"installation":{"id":42},"sender":{"login":"dana-acme"}`
	}
	for _, step := range []struct{ event, id, body, want string }{
		{"repository", "r-rename", `{"action":"renamed",` + repo + `}`, "acme/claims linked"},
		{"repository", "r-archive", `{"action":"archived",` + repo + `}`, "acme/claims archived"},
		{"repository", "r-unarchive", `{"action":"unarchived",` + repo + `}`, "acme/claims linked"},
		{"installation_repositories", "r-remove", `{"action":"removed",` + list("repositories_removed") + `}`, "acme/claims removed"},
		{"installation_repositories", "r-add", `{"action":"added",` + list("repositories_added") + `}`, "acme/claims linked"},
		{"repository", "r-delete", `{"action":"deleted",` + repo + `}`, "acme/claims deleted"},
		{"repository", "r-edit", `{"action":"edited",` + repo + `}`, "acme/claims deleted"}, // not served: 204, nothing written
	} {
		mac := hmac.New(sha256.New, []byte(hookKey))
		mac.Write([]byte(step.body))
		rec := post(step.event, step.id, "sha256="+hex.EncodeToString(mac.Sum(nil)), []byte(step.body))
		var got string
		if err := pool.QueryRow(ctx, "SELECT full_name || ' ' || state FROM project_repositories").Scan(&got); err != nil || got != step.want || (rec.Code != 202) != (step.id == "r-edit") {
			t.Fatalf("%s: %d, the link %q (%v), want %q", step.id, rec.Code, got, err, step.want)
		}
	}
	var states string
	if err := pool.QueryRow(ctx, "SELECT string_agg(detail->>'state', ' ' ORDER BY ts, id) FROM audit_events WHERE action = 'repository changed' AND project_id = 'claims-portal' AND actor_subject = 'github:dana-acme'").Scan(&states); err != nil || states != "archived linked removed linked deleted" {
		t.Fatalf("the audit rows: %q %v", states, err)
	}
}

// One repository, two apps in two folders: each its own workflow and its own check, by folder; and a
// pull request never moves a folder's tree, so the push that merges it is packaged (#1917).
func TestTwoAppsInOneRepositoryAreCheckedApart(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	for _, q := range []string{
		"UPDATE project_repositories SET folder = 'apps/billing'",
		"INSERT INTO projects (id, name) VALUES ('other-app', 'other')",
		"INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('other-app', 81723, 'apps/other', 42, 'acme/claims-portal')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	sha := "6666666666666666666666666666666666666666"
	if rec := deliver(t, "push", "d-two", "push_default_branch.json", `"name":"billing-api"`, `"name":"claims-portal"`, "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", sha); rec.Code != 202 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	for _, app := range []string{"claims-portal", "other-app"} {
		h, err := dbos.RetrieveWorkflow[string](durable, run+"d-two/"+app)
		if err != nil {
			t.Fatalf("%s's check: %v", app, err)
		}
		_, _ = h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	}
	var names []string
	for len(calls) > 0 {
		if call := <-calls; strings.HasPrefix(call, "POST ") {
			var opened CheckRun
			_ = json.Unmarshal([]byte(call[strings.Index(call, "{"):]), &opened)
			names = append(names, opened.Name)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "Canyon gate · apps/billing,Canyon gate · apps/other" {
		t.Fatalf("the checks opened: %q", names)
	}
	if n := rows(t, pool, "SELECT count(*) FROM packages WHERE project_id IN ('claims-portal', 'other-app')"); n != 2 {
		t.Fatalf("one package each: %d", n)
	}
	if rec := deliver(t, "pull_request", "d-pr-folder", "pull_request_opened.json"); rec.Code != 202 {
		t.Fatalf("pull request: %d %s", rec.Code, rec.Body)
	}
	for _, app := range []string{"claims-portal", "other-app"} {
		if h, err := dbos.RetrieveWorkflow[string](durable, run+"d-pr-folder/"+app); err == nil {
			_, _ = h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
		}
	}
	for len(calls) > 0 {
		<-calls
	}
	if n := rows(t, pool, "SELECT count(*) FROM project_repositories WHERE last_tree IN ('billing1', 'other1')"); n != 2 {
		t.Fatalf("each folder's tree is the push's, never the pull request's: %d", n)
	}
}

// An installation is the App's org's by GitHub's numeric id, never its login: an org renamed on GitHub
// keeps its App, and an org that took the old name gets nothing (#1914's security review, P3-b).
func TestAnInstallationIsTheOrgsByItsIdNotItsLogin(t *testing.T) {
	g, pool, _ := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	for _, step := range []struct{ id, body, want string }{
		{"i-renamed", `{"action":"created","installation":{"id":42,"account":{"login":"acme-corp","id":4242}},"sender":{"login":"dana-acme"}}`, "202 acme-corp"},
		{"i-squatter", `{"action":"created","installation":{"id":43,"account":{"login":"acme-corp","id":9999}},"sender":{"login":"mallory"}}`, "204 acme-corp"},
	} {
		rec := signedPost("installation", step.id, step.body)
		var account string
		if err := pool.QueryRow(ctx, "SELECT account FROM github_app").Scan(&account); err != nil || fmt.Sprint(rec.Code)+" "+account != step.want {
			t.Fatalf("%s: %d, the App's org %q (%v), want %s", step.id, rec.Code, account, err, step.want)
		}
	}
	if n := rows(t, pool, "SELECT count(*) FROM github_installations WHERE installation_id = 43"); n != 0 {
		t.Fatal("the org that took the name was installed")
	}
}

// A folder's tree moves only once a review lets it through: a review that did not finish, or blocked
// it, leaves it, so the next push of the same folder is reviewed again (#1917's security re-checks).
func TestAFolderReviewThatDidNotFinishIsReviewedAgain(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE project_repositories SET folder = 'apps/billing'"); err != nil {
		t.Fatal(err)
	}
	hook(func() { review = func(runs.Input) (string, string, int, any) { return "failed", "error", 0, nil } })
	defer hook(func() { review = clean })
	push := func(id, sha string) string {
		if rec := deliver(t, "push", id, "push_default_branch.json", `"name":"billing-api"`, `"name":"claims-portal"`, "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", sha); rec.Code != 202 {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
		}
		return ended(t, calls, id)[0]
	}
	if patch := push("d-failed", "6666666666666666666666666666666666666666"); !strings.Contains(patch, `"conclusion":"failure"`) {
		t.Fatalf("the failed review's check: %s", patch)
	}
	hook(func() {
		review = func(runs.Input) (string, string, int, any) { return "blocked", "", 1, json.RawMessage(golden) }
	})
	if patch := push("d-blocked", "6565656565656565656565656565656565656565"); strings.Contains(patch, `"conclusion":"neutral"`) {
		t.Fatalf("a folder whose review did not finish is reviewed again, and blocks: %s", patch)
	}
	if patch := push("d-still-blocked", "6464646464646464646464646464646464646464"); strings.Contains(patch, `"conclusion":"neutral"`) {
		t.Fatalf("a folder its last review blocked is never passed as unchanged: %s", patch)
	}
	hook(func() { review = clean })
	if patch := push("d-again", "6767676767676767676767676767676767676767"); !strings.Contains(patch, `"conclusion":"success"`) {
		t.Fatalf("the same folder after a review that did not finish is reviewed again: %s", patch)
	}
	if patch := push("d-then", "6868686868686868686868686868686868686868"); !strings.Contains(patch, `"title":"No change under apps/billing"`) {
		t.Fatalf("once reviewed, the same folder is not: %s", patch)
	}
}

// signedPost posts body to acme's App as event, signed with the test word.
func signedPost(event, id, body string) *httptest.ResponseRecorder {
	mac := hmac.New(sha256.New, []byte(hookKey))
	mac.Write([]byte(body))
	return post(event, id, "sha256="+hex.EncodeToString(mac.Sum(nil)), []byte(body))
}

// In an org whose new repositories become apps, a repository created, or added to the App's reach,
// becomes an app linked at its root, unless a link already names it; elsewhere nothing is made (#1923).
func TestANewRepositoryInAnImportingOrgBecomesAnApp(t *testing.T) {
	g, pool, _ := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 { // begin: acme holds only Canyon apps
		t.Fatal("the App did not register")
	}
	linked(t, pool) // claims-portal is linked to 81723
	tail := `"installation":{"id":42},"sender":{"login":"dana-acme"}`
	if rec := signedPost("repository", "n-created", `{"action":"created","repository":{"id":9001001,"name":"new-thing","full_name":"acme/new-thing","owner":{"login":"acme"}},`+tail+`}`); rec.Code != 202 {
		t.Fatalf("created: %d %s", rec.Code, rec.Body)
	}
	if rec := signedPost("installation_repositories", "n-added", `{"action":"added","repositories_added":[{"id":81723,"full_name":"acme/claims-portal"},{"id":9001002,"full_name":"acme/old-thing"}],`+tail+`}`); rec.Code != 202 {
		t.Fatalf("added: %d %s", rec.Code, rec.Body)
	}
	var made string
	if err := pool.QueryRow(ctx, `SELECT string_agg(p.name || ' ' || l.full_name || ' ' || l.folder || ' ' || (SELECT count(*) FROM project_access a WHERE a.project_id = p.id), ', ' ORDER BY p.name)
		FROM projects p JOIN project_repositories l ON l.project_id = p.id WHERE p.id <> 'claims-portal'`).Scan(&made); err != nil || made != "new-thing acme/new-thing  0, old-thing acme/old-thing  0" {
		t.Fatalf("the apps made: %q %v", made, err)
	}
	if n := rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'import repository' AND actor_subject = 'github:dana-acme'"); n != 2 {
		t.Fatalf("import audit rows: %d", n)
	}
	// a repository the App made is one Canyon keeps, linked as it is made: never an app of its own (#1926)
	if rec := signedPost("repository", "n-kept", `{"action":"created","repository":{"id":9001004,"name":"claims-app-k33p","full_name":"acme/claims-app-k33p","owner":{"login":"acme"}},"installation":{"id":42},"sender":{"login":"canyon-acme[bot]"}}`); rec.Code != 204 || rows(t, pool, "SELECT count(*) FROM projects WHERE name = 'claims-app-k33p'") != 0 {
		t.Fatalf("the App's own repository: %d %s", rec.Code, rec.Body)
	}
	if _, err := pool.Exec(ctx, "UPDATE github_app SET imports_new = false"); err != nil {
		t.Fatal(err)
	}
	if rec := signedPost("repository", "n-elsewhere", `{"action":"created","repository":{"id":9001003,"name":"other","full_name":"acme/other","owner":{"login":"acme"}},`+tail+`}`); rec.Code != 204 {
		t.Fatalf("created where an admin picks: %d %s", rec.Code, rec.Body)
	}
	if n := rows(t, pool, "SELECT count(*) FROM projects") + rows(t, pool, "SELECT count(*) FROM github_deliveries WHERE delivery_id LIKE '%n-elsewhere'"); n != 3 {
		t.Fatalf("a repository where an admin picks made an app, or its delivery was kept: %d", n)
	}
}

// A push to an app whose owners turned auto-deploy on is deployed to preview, reviewed first; its
// check ends on the verdict. A pull request is still only reviewed (#1923).
func TestAPushDeploysWhereAutoDeployIsOn(t *testing.T) {
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE projects SET auto_deploy = true"); err != nil {
		t.Fatal(err)
	}
	if rec := deliver(t, "push", "d-auto", "push_default_branch.json", `"name":"billing-api"`, `"name":"claims-portal"`); rec.Code != 202 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	if patch := ended(t, calls, "d-auto")[0]; !strings.Contains(patch, `"conclusion":"success"`) {
		t.Fatalf("the check: %s", patch)
	}
	if rec := deliver(t, "pull_request", "d-auto-pr", "pull_request_opened.json"); rec.Code != 202 {
		t.Fatalf("pull request: %d %s", rec.Code, rec.Body)
	}
	ended(t, calls, "d-auto-pr")
	var actions string
	if err := pool.QueryRow(ctx, "SELECT string_agg(action, ', ' ORDER BY ts, id) FROM audit_events WHERE action IN ('deploy from github', 'gate run from github')").Scan(&actions); err != nil || actions != "deploy from github, gate run from github" {
		t.Fatalf("the push deploys and the pull request reviews: %q %v", actions, err)
	}
}

// A push's deploy is the switch's and the link's as they stand when its run starts: a push from a
// repository the app has since been relinked away from is reviewed, never deployed (#1923).
func TestAPushFromARepositoryNoLongerLinkedIsReviewedNotDeployed(t *testing.T) {
	g, pool, _ := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	for _, q := range []string{
		"UPDATE projects SET auto_deploy = true",
		"UPDATE project_repositories SET repo_id = 555", // relinked to another repository since the push
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_ab12', 'claims-portal', 't', 'c', 1, 'repo')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	end, err := courier.start(ctx, Check{Installation: 42, Owner: "acme", Repo: "claims-portal", Login: "dana-acme", Project: "claims-portal", RepoID: 81723}, "pkg_ab12")
	if err != nil || end.Deploy || end.Run == "" {
		t.Fatalf("the start: %+v %v", end, err)
	}
	if n := rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'gate run from github'"); n != 1 {
		t.Fatalf("a review, not a deploy: %d", n)
	}
}
