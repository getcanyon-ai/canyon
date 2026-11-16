package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// durable is one DBOS context for the package: its migrations take seconds per schema, so the
// tests share one schema, named per run so two runs on one database never clash, and dropped
// afterwards. Nil without a database URL.
var durable dbos.Context

// durableDB is TestMain's pool, where start's cleanup reads durableSchema: never a test's own pool,
// which a test may close. Nil without a database URL.
var durableDB *pgxpool.Pool

var (
	durableSchema = fmt.Sprintf("dbos_t_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	pipe          = &runs.Pipeline{Gate: runstest.Golden} // its Pool and Repos are the test server's, set by start
	fake          *executortest.Fake                      // the test server's executor, set by start
	testModel     model                                   // the test server's model, set by withModel before start; nil is a host with none
	testReviews   bool                                    // the test server reviews with a model though it has no setup chat (an OpenAI host)
	testTranslate custom.Model                            // the test server's model asked how a custom rule is checked, set by withTranslator before start; nil is none
	testAuth      authMode                                // the test server's mode, set by withOIDC before start; the zero value is off
	testOIDC      *oidcConfig                             // and its membership lists under oidc
	testProvider  *provider                               // and its issuer as discovered, set by withIssuer
	testGitHubAPI string                                  // the test server's CANYON_GITHUB_API; "" reaches nothing
	testKube      bool                                    // the test server's CANYON_RUNTIME is kubernetes, set by onKubernetes before start
	testBehind    string                                  // the test server's CANYON_KUBE_GATEWAY_SIGNIN host, set before start; "" unset
	testPublic    = "http://localhost:7777"               // the test server's public URL
	testNow       func() time.Time                        // the test server's limiters' clock, set before start; nil is time.Now
	testShipped   bool                                    // the test server boots the fixture as it ships, the default tools on
	testDiscovery http.RoundTripper                       // the test server's discovery fetches, set by withDiscovery before start; nil is idpAt(nil)
)

// withDiscovery has the next test server's discovery fetches answered by rt until the test ends.
func withDiscovery(t *testing.T, rt http.RoundTripper) {
	testDiscovery = rt
	t.Cleanup(func() { testDiscovery = nil })
}

// idpAt answers a discovery fetch from its documents, by issuer. Nil answers every issuer, naming its endpoints
// on the issuer's own host, as most providers do: a test saving a choice 2 front door reaches no network.
type idpAt map[string]string

func (docs idpAt) RoundTrip(r *http.Request) (*http.Response, error) {
	issuer, asked := strings.CutSuffix(r.URL.String(), "/.well-known/openid-configuration")
	doc, listed := docs[issuer]
	if docs == nil {
		doc, listed = `{"issuer":"`+issuer+`","authorization_endpoint":"`+issuer+`/auth","token_endpoint":"`+issuer+`/token","jwks_uri":"`+issuer+`/keys"}`, true
	}
	status := http.StatusOK
	if !asked || !listed {
		status, doc = http.StatusNotFound, "not found"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(doc)), Request: r}, nil
}

// shipped has the test server boot the fixture as it ships, its default tools on, until the test ends.
func shipped(t *testing.T) {
	testShipped = true
	t.Cleanup(func() { testShipped = false })
}

// testPolicy is the fixture as the server boots it; a fixture the server would refuse fails every test here.
func testPolicy() gatepolicy.Snapshot {
	raw := contracts.Snapshot
	if !testShipped { // the default tools off — gitleaks, run in process, by its state — so a test scans with the tools it turns on, and no other
		var s gatepolicy.Snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			panic(err)
		}
		for i := range s.Rules {
			if s.Rules[i].Run = false; s.Rules[i].ID == "sarif.gitleaks" {
				s.Rules[i].State = gatepolicy.Off
			}
		}
		raw, _ = json.Marshal(s) // a snapshot that just unmarshaled marshals
	}
	s, err := loadPolicySnapshot(raw)
	if err != nil {
		panic(err)
	}
	return s
}

func TestMain(m *testing.M) {
	url := os.Getenv("CANYON_TEST_DATABASE_URL")
	if url != "" {
		ctx := context.Background()                               // DBOS keeps this as its parent for the whole run; the bound below is only on the setup step
		setup, cancel := context.WithTimeout(ctx, 30*time.Second) // an unreachable host fails fast, not at the OS timeout
		defer cancel()
		admin, err := pgxpool.New(setup, url)
		if err != nil {
			panic(err)
		}
		durableDB = admin
		if _, err := admin.Exec(setup, "DROP SCHEMA IF EXISTS "+durableSchema+" CASCADE"); err != nil {
			panic(err)
		}
		if durable, err = runs.NewDurable(ctx, url, durableSchema, io.Discard); err != nil {
			panic(err)
		}
		if err := pipe.Register(durable); err != nil { // the gate-run workflow, with the golden log as its gate: a started run reaches a verdict
			panic(err)
		}
		// The rescan's clock is the tests' to hold: its schedule falls due every 6 h from 00:00 UTC, and a tick then
		// writes into whichever test's schema pipe reads (#2731). Paused before the scheduler starts, it never ticks;
		// a test that needs a tick enqueues one, and an apply of the schedule keeps it paused.
		if err := dbos.PauseSchedule(durable, runs.RescanWorkflow); err != nil {
			panic(err)
		}
		if err := dbos.Launch(durable); err != nil {
			panic(err)
		}
		defer func() {
			_ = dbos.Shutdown(durable, 10*time.Second)
			_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+durableSchema+" CASCADE")
			admin.Close()
		}()
	}
	code := m.Run()
	if code != 0 {
		os.Exit(code) // deferred cleanup runs only on success: a failed run keeps its schema for a look
	}
}

// syncBuffer is a log sink the server's goroutines write while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var (
	reposByBase sync.Map // test server address → its repos directory
	poolByBase  sync.Map // test server address → its pool
	vaultByBase sync.Map // test server address → its vault
	appByBase   sync.Map // test server address → its app, whose passes a test runs itself (the database sessions' checks)
	testKeyFile string   // the key file the next start loads; blank is a fresh one
)

// poolOf is the pool of the test server at base.
func poolOf(t *testing.T, base string) *pgxpool.Pool {
	t.Helper()
	v, ok := poolByBase.Load(strings.TrimPrefix(base, "http://"))
	if !ok {
		t.Fatalf("no test server at %s", base)
	}
	return v.(*pgxpool.Pool)
}

// vaultOf is the vault of the test server at base.
func vaultOf(t *testing.T, base string) *vault.Vault {
	t.Helper()
	v, ok := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	if !ok {
		t.Fatalf("no test server at %s", base)
	}
	return v.(*vault.Vault)
}

// appOf is the app of the test server at base.
func appOf(t *testing.T, base string) *app {
	t.Helper()
	v, ok := appByBase.Load(strings.TrimPrefix(base, "http://"))
	if !ok {
		t.Fatalf("no test server at %s", base)
	}
	return v.(*app)
}

// reposOf is the repos directory of the test server at base.
func reposOf(t *testing.T, base string) string {
	t.Helper()
	v, ok := reposByBase.Load(strings.TrimPrefix(base, "http://"))
	if !ok {
		t.Fatalf("no test server at %s", base)
	}
	return v.(string)
}

// start serves on a loopback port until the test ends and returns the base URL and the log.
func start(t *testing.T, pool *pgxpool.Pool, probeEvery time.Duration) (string, *syncBuffer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := loadSnapshot(contracts.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	repos := t.TempDir()
	keyFile := testKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(t.TempDir(), "kek")
	}
	keys, err := vault.Load(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	reposByBase.Store(ln.Addr().String(), repos)
	poolByBase.Store(ln.Addr().String(), pool)
	vaultByBase.Store(ln.Addr().String(), keys)
	pipe.Pool, pipe.Repos = pool, repos // tests run one at a time: the workflow serves the server under test
	fake = executortest.New(t)
	pipe.Executor, pipe.PublicURL, pipe.Vault, pipe.Drivers = &executor.Client{Base: fake.URL}, "http://localhost:7777", keys, nil
	t.Cleanup(func() {
		reposByBase.Delete(ln.Addr().String())
		poolByBase.Delete(ln.Addr().String())
		vaultByBase.Delete(ln.Addr().String())
	})
	ctx, cancel := context.WithCancel(context.Background())
	var logs syncBuffer
	done := make(chan error, 1)
	discovery := testDiscovery
	if discovery == nil {
		discovery = idpAt(nil)
	}
	m, mode, lists, idp, kube, public, now, behind, hooks := testModel, testAuth, testOIDC, testProvider, testKube, testPublic, testNow, testBehind, testEE(keys) // read here, not in the goroutine: this server's model and mode are the ones set when it was started
	a := &app{pool: pool, repos: repos, snap: snap, boot: testPolicy(), vault: keys, durable: durable, pipe: pipe, model: m, reviews: m != nil || testReviews, translator: testTranslate, modelID: "test-model", github: testGitHubAPI, ghWeb: testGitHubAPI, public: public, auth: mode, oidc: lists, idp: idp, kube: kube, behind: behind, now: now, ee: hooks, discovery: discovery, ready: func(l *log.Logger) *readiness { return newReadiness(ctx, pool, durable, probeEvery, l, nil) }}
	appByBase.Store(ln.Addr().String(), a)
	t.Cleanup(func() { appByBase.Delete(ln.Addr().String()) })
	pipe.Access = nil // as main wires it: a Kubernetes host's /run carries the access file and the front door
	pipe.Inject = hooks.outbound
	pipe.Org = func(ctx context.Context) string { return a.orgNow(ctx).name }
	pipe.Clears = a.clearsReview
	pipe.DecidePatch = a.decidePatch
	pipe.Patcher = hooks.patching // as main wires it: nil in a build without Canyon Enterprise
	pipe.ProjectPolicy.Store(func(ctx context.Context, _ runs.Querier, project string) (gatepolicy.Snapshot, error) {
		return a.projectPolicy(ctx, pool, project)
	})
	if kube {
		pipe.Access = a.runAccess
	}
	go func() { done <- serve(ctx, ln, &logs, a) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v after cancel, want nil", err)
		}
		if durableDB == nil {
			return
		}
		// A workflow still queued or running reads and writes through pipe, which the next test's start
		// re-points at its own schema (#1494): a run's, and a remove release's. Each is waited for here;
		// its answer is not this test's, that it ended is. One that won't end is cancelled, so the
		// next test is not failed for it.
		for _, id := range inFlight(t) {
			if h, err := dbos.RetrieveWorkflow[any](durable, id); err == nil {
				_, _ = h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
			}
		}
		if open := inFlight(t); len(open) > 0 {
			for _, id := range open {
				_ = dbos.CancelWorkflow(durable, id)
			}
			t.Errorf("workflow(s) %v still in flight 30 s after the test", open)
		}
	})
	return "http://" + ln.Addr().String(), &logs
}

// inFlight is the ids of the package's DBOS workflows still queued or running.
func inFlight(t *testing.T) []string {
	t.Helper()
	rows, err := durableDB.Query(context.Background(), "SELECT workflow_uuid FROM "+durableSchema+".workflow_status WHERE status IN ('ENQUEUED', 'PENDING') ORDER BY workflow_uuid")
	if err != nil {
		t.Error(err)
		return nil
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Error(err)
	}
	return ids
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: not JSON: %q", url, raw)
	}
	return resp.StatusCode, body
}

func TestHealthAnswersWithoutPostgresAndReadyDoesNot(t *testing.T) {
	base, logs := start(t, nil, time.Hour)
	if code, body := get(t, base+"/health"); code != 200 || body["status"] != "ok" {
		t.Fatalf("/health: %d %v", code, body)
	}
	if code, body := get(t, base+"/ready"); code != 503 || body["error"] != "not_ready" || body["message"] != "not ready" || !strings.Contains(string(mustJSON(body["waiting_on"])), `"postgres"`) {
		t.Fatalf("/ready without a database: %d %v", code, body)
	}
	for _, want := range []string{"canyon-server dev listening on", "Canyon is up: open http://localhost:7777\n", "GET /health 200", "GET /ready 503"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log line %q missing:\n%s", want, logs.String())
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestReadyFailsClosedWhenPostgresIsDown(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/none?connect_timeout=1") // nothing listens on port 1
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	base, logs := start(t, pool, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // several probes
	if code, body := get(t, base+"/ready"); code != 503 || body["error"] != "not_ready" {
		t.Fatalf("an unreachable Postgres must read as not ready, never as ready: %d %v", code, body)
	}
	if !strings.Contains(logs.String(), "readiness: postgres:") {
		t.Fatalf("the contract sends failure detail to the server's log, not the response; the log has none:\n%s", logs.String())
	}
}

func TestReadyIsNotReadyOnAnUnmigratedSchemaAndReadyAfter(t *testing.T) {
	pool, ctx := pgtest.Schema(t)
	base, _ := start(t, pool, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if code, _ := get(t, base+"/ready"); code != 503 {
		t.Fatalf("an unmigrated schema must not be ready: %d", code)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code, body := get(t, base+"/ready"); code == 200 && body["status"] == "ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("still not ready three seconds after the migration")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAuthConfigIsRoutedAndErrorsCarryTheContractShape(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	const notFound = `{"error":"not_found","message":"no such route"}`
	for _, r := range []struct {
		method, path string
		code         int
		body         string
	}{
		// The one route that proves a host is Canyon. This binary mints anonymous sessions for every caller, so auth is off and there is no device flow.
		{http.MethodGet, "/auth/config", 200, `{"auth":"off","canyon":true,"cli":"dev","device":false,"org":"Meridian","public":"http://localhost:7777","setup":false}`},
		// The four flow routes: 404 with auth off (routes_auth_test.go has the browser pair live under oidc, routes_device_test.go the device pair). A plain typo: no server-side fallback, the same 404.
		{http.MethodGet, "/auth/start", 404, notFound},
		{http.MethodGet, "/auth/callback", 404, notFound},
		{http.MethodPost, "/auth/device", 404, notFound},
		{http.MethodPost, "/auth/device/token", 404, notFound},
		{http.MethodGet, "/nope", 404, notFound},
		// GitHub's deliveries, Open: this host has registered no App.
		{http.MethodPost, "/webhooks/github", 503, `{"error":"not_configured","message":"no GitHub App is registered on this host"}`},
		// The GitHub App's registration, under the session: every caller is an admin here, and a callback with no state cookie is refused.
		{http.MethodGet, "/api/v1/github/app/callback?code=c&state=s", 400, `{"error":"auth_failed","message":"the GitHub App registration could not be finished"}`},
	} {
		req, err := http.NewRequestWithContext(context.Background(), r.method, base+r.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != r.code || strings.TrimSpace(string(raw)) != r.body || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s %s answers the contract's %d %s: got %d %s %q", r.method, r.path, r.code, r.body, resp.StatusCode, raw, resp.Header.Get("Content-Type"))
		}
	}
}

var bundle = fstest.MapFS{ // the shape `make web` leaves in web/dist, hashes shortened
	"dist/index.html":           {Data: []byte(`<!doctype html><div id="root"></div><script src="/assets/app-a1b2c3.js"></script>`)},
	"dist/assets/app-a1b2c3.js": {Data: []byte("console.log('canyon')")},
}

// openGroup registers the Open group over one bundle and answers a request against it.
func openGroup(t *testing.T, files fs.FS, method, target string) (*httptest.ResponseRecorder, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var logs syncBuffer
	mux := http.NewServeMux()
	routesOpen(mux, newReadiness(ctx, nil, nil, time.Hour, newTestLogger(&logs), nil), newTestLogger(&logs), files, &app{}) // the bundle routes only: /auth/config has its own test, with a row to read
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec, &logs
}

func TestTheDashboardIsServedFromTheBinaryAndNothingElseIs(t *testing.T) {
	rec, _ := openGroup(t, bundle, http.MethodGet, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<div id="root">`) ||
		rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("GET / is the dashboard's index.html, revalidated every load: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec, _ = openGroup(t, bundle, http.MethodGet, "/assets/app-a1b2c3.js")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("a hashed asset is cached forever: %d %v", rec.Code, rec.Header())
	}
	// Hash routing: the bundle falls back for nothing — a typo, a miss, a path climbing out of it.
	for _, target := range []string{"/nope", "/assets/nothing.js", "/assets/..%2f..%2findex.html"} {
		rec, _ := openGroup(t, bundle, http.MethodGet, target)
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"error":"not_found","message":"no such route"}` {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
}

func TestWithNoBundleEmbeddedTheRootIsJustAnotherUnroutedPath(t *testing.T) {
	rec, logs := openGroup(t, fstest.MapFS{}, http.MethodGet, "/")
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"error":"not_found","message":"no such route"}` {
		t.Fatalf("a binary built without `make web` fails closed on the root: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "make web") {
		t.Fatalf("the boot log must say the bundle is missing and how to build it:\n%s", logs.String())
	}
}

func TestAPanicAnswers500AndIsLogged(t *testing.T) {
	var logs syncBuffer
	h := pipeline(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), newTestLogger(&logs))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error":"internal"`) || !strings.Contains(logs.String(), "panic GET /x: boom") || !strings.Contains(logs.String(), "GET /x 500") {
		t.Fatalf("a panic answers 500 in the contract's shape and is logged: %d %s\n%s", rec.Code, rec.Body.String(), logs.String())
	}
}

func newTestLogger(w io.Writer) *log.Logger { return log.New(w, "", 0) }

// A deploy polls its run's timeline every second, the dashboard the org's, and an open app page its project, its
// status, its runs and the newest run every 2 seconds: answered 200, a poll writes no log line, or a first start
// buries the line saying the demo is ready under a thousand of them (#2697), and an open tab adds 90 a minute
// (#2711). Any other answer of a poll is logged, and so is every other request.
func TestPollsLogOnlyFailures(t *testing.T) {
	for _, c := range []struct {
		method, path string
		status       int
		logged       bool
	}{
		{http.MethodGet, "/api/v1/gate-runs/gr_8f2a/timeline", 200, false},
		{http.MethodGet, "/api/v1/timeline", 200, false},
		{http.MethodGet, "/api/v1/projects/claims-portal", 200, false},
		{http.MethodGet, "/api/v1/projects/claims-portal/status", 200, false},
		{http.MethodGet, "/api/v1/projects/claims-portal/gate-runs", 200, false},
		{http.MethodGet, "/api/v1/gate-runs/gr_8f2a", 200, false},
		{http.MethodGet, "/api/v1/gate-runs/gr_8f2a/timeline", 404, true},
		{http.MethodGet, "/api/v1/timeline", 403, true},
		{http.MethodGet, "/api/v1/projects/claims-portal", 404, true},
		{http.MethodGet, "/api/v1/projects/claims-portal/status", 403, true},
		{http.MethodGet, "/api/v1/projects/claims-portal/gate-runs", 500, true},
		{http.MethodGet, "/api/v1/gate-runs/gr_8f2a", 404, true},
		{http.MethodGet, "/api/v1/gate-runs/gr_8f2a/timeline/more", 200, true},
		{http.MethodGet, "/api/v1/projects", 200, true},
		{http.MethodGet, "/api/v1/projects/claims-portal/logs", 200, true},
		{http.MethodPost, "/api/v1/timeline", 200, true},
		{http.MethodDelete, "/api/v1/projects/claims-portal", 200, true},
		{http.MethodPost, "/api/v1/projects/claims-portal/gate-runs", 200, true},
	} {
		var logs syncBuffer
		h := pipeline(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) }), newTestLogger(&logs))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if logged := strings.Contains(logs.String(), c.method+" "+c.path+" "+strconv.Itoa(c.status)); logged != c.logged {
			t.Errorf("%s %s answered %d: logged %v, want %v:\n%s", c.method, c.path, c.status, logged, c.logged, logs.String())
		}
	}
}

// An argument canyon-server does not know prints its usage and exits 2 before anything opens the database: `version`
// in a live pod booted the whole server, which migrated, drew an egress epoch, swept sessions and registered a durable
// executor before it failed to bind (#2693). The binary is this test's own, run again as main; its database is a
// listener that counts every dial, set up so that a boot reaches it.
func TestAnUnknownSubcommandTouchesNothing(t *testing.T) {
	if arg := os.Getenv("CANYON_TEST_MAIN_ARG"); arg != "" { // the run again: main, as canyon-server <arg>
		os.Args = []string{"canyon-server", arg}
		main()
		os.Exit(0) // main came back: it ran something
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var dialed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialed.Add(1) // before the close the dialler waits on, so a run that dialled has counted when it ends
			_ = c.Close()
		}
	}()
	for _, arg := range []string{"version", "nonsense"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAnUnknownSubcommandTouchesNothing$") //nolint:gosec // this test's own binary
		cmd.Env = append(os.Environ(), "CANYON_TEST_MAIN_ARG="+arg, "CANYON_TEST_DATABASE_URL=", "CANYON_AUTH=off",
			"CANYON_DATABASE_URL=postgres://canyon:canyon@"+ln.Addr().String()+"/canyon?sslmode=disable", "CANYON_KEY_FILE="+filepath.Join(t.TempDir(), "kek"))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), `canyon-server: unknown command "`+arg+`"`) {
			t.Errorf("canyon-server %s: %v, want exit 2 and its usage:\n%s", arg, err, out)
		}
	}
	if n := dialed.Load(); n != 0 {
		t.Errorf("the database was dialled %d time(s): an unknown command touched it", n)
	}
}

// The chunked log route and the executor proxy flush as they go; the status wrapper must not
// hide that from net/http's ResponseController.
func TestThePipelineStillFlushes(t *testing.T) {
	h := pipeline(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
			return
		}
		_, _ = w.Write([]byte("flushed"))
	}), newTestLogger(io.Discard))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 200 || rec.Body.String() != "flushed" || !rec.Flushed {
		t.Fatalf("flush through the pipeline: %d %q flushed=%v", rec.Code, rec.Body.String(), rec.Flushed)
	}
}

// Until the first probe has answered, the server is not ready: a client that asks in the first
// milliseconds after boot must not be told otherwise.
func TestReadyIsNotReadyBeforeTheFirstProbeAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // accepts and never speaks: the probe hangs until its timeout
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@"+ln.Addr().String()+"/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newReadiness(ctx, pool, durable, time.Hour, newTestLogger(io.Discard), nil)
	if w := r.waitingOn(); strings.Join(w, ",") != "postgres,durable_runs" {
		t.Fatalf("before any probe has answered the server must wait on both, got %v", w)
	}
}

func TestReadyWaitsOnDurableRunsWithoutDBOS(t *testing.T) {
	pool, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := newReadiness(rctx, pool, nil, 20*time.Millisecond, newTestLogger(io.Discard), nil)
	time.Sleep(150 * time.Millisecond)
	if w := r.waitingOn(); strings.Join(w, ",") != "durable_runs" {
		t.Fatalf("Postgres migrated but no durable-run store: the server waits on durable_runs alone, got %v", w)
	}
}

func TestDurableLaunchCreatesItsSchemaAndShutdownReturnsNil(t *testing.T) {
	pool, ctx := pgtest.Schema(t)
	if durable == nil {
		t.Skip("no DBOS context without a database")
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+durableSchema+".workflow_status").Scan(&n); err != nil {
		t.Fatalf("DBOS's own tables must exist in its schema after launch: %v", err)
	}
	schema := "dbos_" + strings.ToLower(t.Name())
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	other, err := runs.NewDurable(ctx, os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbos.Launch(other); err != nil {
		t.Fatal(err)
	}
	if err := dbos.Shutdown(other, 10*time.Second); err != nil {
		t.Fatalf("a launched context shuts down clean: %v", err)
	}
	if _, err := other.ListWorkflows(other, dbos.WithFilterLimit(1)); err == nil {
		t.Fatal("after shutdown the context must not answer as if it were live")
	}
}

// slowDurable is a durable store that never answers: ListWorkflows blocks until the context
// it was given expires. The probe must give it a deadline or the prober goroutine hangs.
type slowDurable struct{ dbos.Context }

func (s slowDurable) WithTimeout(_ dbos.Context, d time.Duration) (dbos.Context, context.CancelFunc) {
	child, cancel := s.Context.WithTimeout(s.Context, d)
	return slowDurable{child}, cancel
}

func (s slowDurable) ListWorkflows(_ dbos.Client, _ ...dbos.ListWorkflowsOption) ([]dbos.WorkflowStatus, error) {
	<-s.Done()
	return nil, s.Err()
}

func TestTheDurableProbeIsBounded(t *testing.T) {
	if durable == nil {
		t.Skip("no DBOS context without a database")
	}
	pool, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	newReadiness(rctx, pool, slowDurable{durable}, time.Hour, newTestLogger(&logs), nil)
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(logs.String(), "readiness: durable_runs:") {
		if time.Now().After(deadline) {
			t.Fatal("a durable store that never answers must not hang the prober: no probe result within 4s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The host's capacities are whole numbers from 1 to 100, unset the queues' Docker defaults; anything
// else stops the server at boot, named (contracts/finding.md, Canyon-run tools; #1652).
func TestCapacitiesAreReadOrRefusedByName(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if s, b, p, err := capacities(env(nil)); err != nil || s != 0 || b != 0 || p != 0 {
		t.Fatalf("unset: %d %d %d %v", s, b, p, err)
	}
	if s, b, p, err := capacities(env(map[string]string{"CANYON_SCAN_CONCURRENCY": "10", "CANYON_BUILD_CONCURRENCY": "4", "CANYON_PATCH_CONCURRENCY": "2"})); err != nil || s != 10 || b != 4 || p != 2 {
		t.Fatalf("set: %d %d %d %v", s, b, p, err)
	}
	for _, kv := range []map[string]string{{"CANYON_SCAN_CONCURRENCY": "1"}, {"CANYON_SCAN_CONCURRENCY": "101"}, {"CANYON_BUILD_CONCURRENCY": "four"}, {"CANYON_PATCH_CONCURRENCY": "0"}, {"CANYON_PATCH_CONCURRENCY": "-1"},
		{"CANYON_PATCH_CONCURRENCY": "101"}, {"CANYON_PATCH_CONCURRENCY": "two"}} {
		for name := range kv {
			if _, _, _, err := capacities(env(kv)); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%v: %v", kv, err)
			}
		}
	}
}

// CANYON_PATCH_INTERVAL is a duration every replica ticks on at the same instants: unset is the default, and
// one that divides no minute, hour or day evenly, or is no duration, is refused naming it.
func TestPatchIntervalIsReadOrRefusedByName(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if d, err := patchInterval(env("")); err != nil || d != 0 {
		t.Fatalf("unset: %s %v", d, err)
	}
	if d, err := patchInterval(env("30m")); err != nil || d != 30*time.Minute {
		t.Fatalf("set: %s %v", d, err)
	}
	for _, v := range []string{"7h", "45m", "six hours", "-6h"} {
		if _, err := patchInterval(env(v)); err == nil || !strings.Contains(err.Error(), "CANYON_PATCH_INTERVAL") {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// CANYON_PATCH_WATCH is how long a patched preview is watched: unset is the default, and anything but a
// duration from a minute to a day is refused naming it.
func TestPatchWatchIsReadOrRefusedByName(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if d, err := patchWatch(env("")); err != nil || d != 0 {
		t.Fatalf("unset: %s %v", d, err)
	}
	if d, err := patchWatch(env("1m")); err != nil || d != time.Minute {
		t.Fatalf("set: %s %v", d, err)
	}
	for _, v := range []string{"30s", "25h", "fifteen minutes", "-15m"} {
		if _, err := patchWatch(env(v)); err == nil || !strings.Contains(err.Error(), "CANYON_PATCH_WATCH") {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// A server patches only once the operator turns patching on (CANYON_PATCHING, beta), and only with sign-in on:
// unset and off are off, on is on, any other value is refused naming it, and with sign-in off on is off too.
func TestPatchingIsOffUntilTheOperatorTurnsItOn(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, c := range []struct {
		v    string
		mode authMode
		want bool
	}{{"", authOIDC, false}, {"off", authOIDC, false}, {"on", authOIDC, true}, {"on", authOff, false}, {"on", authDemo, false}} {
		if on, err := patchesOn(env(c.v), c.mode); err != nil || on != c.want {
			t.Errorf("%q, sign-in %s: %v %v", c.v, c.mode, on, err)
		}
	}
	for _, v := range []string{"true", "ON", "ask", "1"} {
		if _, err := patchesOn(env(v), authOIDC); err == nil || !strings.Contains(err.Error(), "CANYON_PATCHING") {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// The server's pool is sized on purpose: 20 unset, the setting read by name, and a value outside 4 to
// 500 refused naming it (#1693).
func TestPoolSizeIsReadOrRefusedByName(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if n, err := poolSize(env("")); err != nil || n != 20 {
		t.Fatalf("unset: %d %v", n, err)
	}
	if n, err := poolSize(env("40")); err != nil || n != 40 {
		t.Fatalf("set: %d %v", n, err)
	}
	for _, v := range []string{"3", "501", "many"} {
		if _, err := poolSize(env(v)); err == nil || !strings.Contains(err.Error(), "CANYON_DATABASE_MAX_CONNS") {
			t.Errorf("%s: %v", v, err)
		}
	}
}
