package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func migrated(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if testAuth == authOIDC { // an onboarded host, as an upgrade leaves one (0018): a test that wants the setup state enters it by name, inSetup
		if _, err := pool.Exec(ctx, "UPDATE org_preferences SET onboarded_at = now()"); err != nil {
			t.Fatal(err)
		}
	}
	return pool, ctx
}

// inSetup puts the host into the setup state a fresh install starts in: onboarded_at null (design/ONBOARDING.md §2).
func inSetup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET onboarded_at = NULL"); err != nil {
		t.Fatal(err)
	}
}

func client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	// A connection dialed for a request that another, freed first, then served waits in the idle pool
	// having sent nothing. The server counts it as new, not idle, for five seconds, longer than serve's
	// shutdown waits, and the test fails on its own teardown (#1901). Closed here, ahead of the server's
	// cleanup, it never reaches shutdown.
	t.Cleanup(c.CloseIdleConnections)
	return c
}

var lastBody string // the raw body of the last do(), for byte-exact checks

func do(t *testing.T, c *http.Client, method, url, body string) (int, map[string]any, *http.Response) {
	t.Helper()
	return doWith(t, c, method, url, body, nil)
}

// doWith is do with the headers a browser would have added, which Go's client does not.
func doWith(t *testing.T, c *http.Client, method, url, body string, hdr map[string]string) (int, map[string]any, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	lastBody = strings.TrimSpace(string(raw))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A demo host serves every caller without a session as the anonymous admin and stores nothing: no
// row, no cookie, however many calls, and a stale cookie is only a caller without a session.
func TestADemoHostStoresNoSession(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for range 3 {
		if code, body, resp := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); len(resp.Cookies()) != 0 || (code != 201 && code != 409) {
			t.Fatalf("create: %d %v, cookies %v", code, body, resp.Cookies())
		}
	}
	stale := client(t)
	stale.Jar.SetCookies(mustURL(t, base), []*http.Cookie{{Name: sessionCookie, Value: strings.Repeat("ab", tokenBytes)}}) //nolint:gosec // test: a cookie no row answers
	if _, _, resp := do(t, stale, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); resp.StatusCode != 200 || len(resp.Cookies()) != 0 {
		t.Fatalf("a stale cookie: %d, cookies %v; want the anonymous caller, no cookie", resp.StatusCode, resp.Cookies())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("%d session rows; a demo host stores none", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE actor_subject = 'anonymous' AND action = 'POST /api/v1/projects'"); n != 1 {
		t.Fatalf("%d create rows by anonymous; want the one create, attributed to the anonymous caller", n)
	}
}

func TestOpenRoutesIssueNoSessionAndWriteNoAuditRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, path := range []string{"/health", "/ready", "/auth/config"} {
		if _, _, resp := do(t, c, http.MethodGet, base+path, ""); len(resp.Cookies()) != 0 {
			t.Fatalf("%s issued a cookie", path)
		}
	}
	if count(t, ctx, pool, "SELECT count(*) FROM sessions")+count(t, ctx, pool, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatal("the Open group leaves no session and no audit row")
	}
}

func TestAMutationWritesOneAuditRowAndReadsWriteNone(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	for range 3 {
		do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "") // the once-a-second poll
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 1 {
		t.Fatalf("one mutation, three reads: %d audit rows", n)
	}
	var action, kind, subject string
	var proj *string
	if err := pool.QueryRow(ctx, "SELECT action, actor_kind, actor_subject, project_id FROM audit_events").Scan(&action, &kind, &subject, &proj); err != nil || action != "POST /api/v1/projects" || kind != "person" || subject != "anonymous" || proj == nil || *proj != "claims-portal" {
		t.Fatalf("the audit row names what it produced: %s %s/%s %v (%v)", action, kind, subject, proj, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE scope = 'org' AND project_id = 'claims-portal' AND kind = 'audit.appended'"); n != 1 {
		t.Fatalf("the new project's first fleet row is its creation: %d org rows", n)
	}
}

func TestCreateProjectRejectsAnIdThePatternRefusesAndADuplicate(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, name := range []string{"../etc", "Claims", "claims portal", "1abc", strings.Repeat("a", 65)} {
		if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`); code != 400 || body["error"] != "invalid_id" {
			t.Fatalf("%q: %d %v", name, code, body)
		}
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 409 || body["error"] != "exists" {
		t.Fatalf("duplicate: %d %v", code, body)
	}
}

// A name alone is a display name, and the host picks the id (contracts/routes.md, POST /projects;
// #1911): a second person's first deploy of claims-app is its own app, and the first is untouched.
func TestANameAloneGetsAnIdTheHostPicksAndNamesMayRepeat(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	picked := map[string]bool{}
	for _, name := range []string{"claims-app", "claims-app", "My_App", "Ünïcode app"} {
		code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"name":"`+name+`"}`)
		p, _ := body["project"].(map[string]any)
		id, _ := p["id"].(string)
		if code != 201 || p["name"] != name || !regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z0-9]{3}$`).MatchString(id) || picked[id] {
			t.Fatalf("%q: %d %s", name, code, lastBody)
		}
		picked[id] = true
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects WHERE name = 'claims-app'"); n != 2 {
		t.Fatalf("two apps named claims-app: %d", n)
	}
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"claims-app","id":"claims-app"}`} {
		if code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", body); code != 400 || out["error"] != "bad_request" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	for _, name := range []string{" ", `a\nb`, "a\u202eb", strings.Repeat("é", 101)} {
		if code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"name":"`+name+`"}`); code != 400 || out["error"] != "invalid_name" {
			t.Fatalf("%q: %d %v", name, code, out)
		}
	}
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"name":"`+strings.Repeat("é", 100)+`"}`); code != 201 {
		t.Fatalf("100 characters, not bytes: %d %s", code, lastBody)
	}
}

// The words never name an app the host has had, live or deleted: a pick that is taken is picked again.
func TestAPickedIdThatIsTakenIsPickedAgain(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name, deleted_at) VALUES ('amber-otter-aaa', 'gone', now())"); err != nil {
		t.Fatal(err)
	}
	picks := []string{"amber-otter-aaa", "amber-otter-aab"}
	was := pickAppID
	pickAppID = func() string { id := picks[0]; picks = picks[1:]; return id }
	t.Cleanup(func() { pickAppID = was })
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"name":"claims-app"}`); code != 201 || body["project"].(map[string]any)["id"] != "amber-otter-aab" {
		t.Fatalf("%d %s", code, lastBody)
	}
}

func TestProjectShapeMatchesTheSeamAndGateStateFollowsTheLatestRun(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	var created time.Time // no package yet: latest is `created` since the project was
	if err := pool.QueryRow(ctx, "SELECT created_at FROM projects WHERE id = 'claims-portal'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if lastBody != `{"project":{"id":"claims-portal","name":"claims-portal","owners":[],"owner_groups":[],"summary":"","claimable":true,"access":{"level":"none","everyone":false,"people":0,"groups":0,"requests":0},"stack":"","gate_state":"never_reviewed","envs":{},"latest":{"package":null,"stage":"created","since":"`+created.UTC().Format(time.RFC3339)+`"}}}` {
		t.Fatalf("project on the wire: %s", lastBody)
	}
	var body map[string]any
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', 'claims-portal', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state, ended_at, created_at) VALUES ('gr_old00000', 'claims-portal', 'pkg_7d3e8f', 'clean', now(), now() - interval '1 hour')",
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_new00000', 'claims-portal', 'pkg_7d3e8f', 'blocked', now())",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, body, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); body["project"].(map[string]any)["gate_state"] != "blocked" {
		t.Fatalf("gate_state is the newest run's: %v", body)
	}
	// two runs in the same second (fast automation): the answer is deterministic, the higher id
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, ended_at, created_at) VALUES ('gr_tie0000a', 'claims-portal', 'pkg_7d3e8f', 'clean', now(), '2026-09-10T12:00:00Z'), ('gr_tie0000b', 'claims-portal', 'pkg_7d3e8f', 'failed', now(), '2026-09-10T12:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET created_at = '2026-09-10T11:00:00Z' WHERE id IN ('gr_old00000', 'gr_new00000')"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, body, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); body["project"].(map[string]any)["gate_state"] != "failed" {
			t.Fatalf("a tie on created_at is broken by id, every time: %v", body["project"])
		}
	}
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/nothing", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("unknown project: %d %v", code, body)
	}
}

func TestOversizeAndTrailingDataBodiesAreRefused(t *testing.T) {
	for name, body := range map[string]string{"trailing": `{"name":"a"} {"name":"b"}`, "oversize": `{"name":"` + strings.Repeat("x", 1<<20) + `"}`, "not json": `nope`} {
		rec := httptest.NewRecorder()
		var v struct{ Name string }
		if decodeJSON(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), &v) || rec.Code != 400 || !strings.Contains(rec.Body.String(), `"error":"bad_request"`) {
			t.Fatalf("%s: accepted or wrong shape: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestARefusedMutationLeavesNoAuditRowAndNoOrgRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	rows := count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM timeline_rows")
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, "/projects", `{"id":"claims-portal"}`},       // 409
		{http.MethodPost, "/projects", `{"id":"Claims"}`},              // 400
		{http.MethodPut, "/projects/nothing/owner", `{"display":"x"}`}, // 404
		{http.MethodPut, "/projects/claims-portal/owner", `{}`},        // 400
		{http.MethodDelete, "/projects/nothing", ""},                   // 404
	} {
		if code, _, _ := do(t, c, step.method, base+"/api/v1"+step.path, step.body); code < 400 {
			t.Fatalf("%s %s answered %d, the step is meant to be refused", step.method, step.path, code)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM timeline_rows"); n != rows {
		t.Fatalf("five refused requests left %d rows: audit is not logging", n-rows)
	}
}

func TestCrossOriginMutationsAreRefusedAndLeaveNothingBehind(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	rows := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM projects")
	}
	for _, c := range []struct {
		name string
		hdr  map[string]string
	}{
		{"a preview page's fetch", map[string]string{"Origin": "https://claims-portal.preview.localhost:7777"}},
		{"the browser calling it same-site", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"the browser calling it cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"a sandboxed frame", map[string]string{"Origin": "null"}},
	} {
		before := rows()
		code, body, _ := doWith(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`, c.hdr)
		if code != 403 || body["error"] != "cross_origin" {
			t.Fatalf("%s: %d %v", c.name, code, body)
		}
		if n := rows(); n != before {
			t.Fatalf("%s: refused, and left %d row(s) behind", c.name, n-before)
		}
	}
	if lastBody != `{"error":"cross_origin","message":"this request did not come from Canyon"}` {
		t.Fatalf("the refusal on the wire: %s", lastBody)
	}
}

func TestTheDashboardTheDevProxyAndTheCliAreNotRefused(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, c := range []struct {
		name, project string
		hdr           map[string]string
	}{
		{"the dashboard", "dashboard", map[string]string{"Origin": "http://localhost:7777", "Sec-Fetch-Site": "same-origin"}},
		{"make web-dev's vite proxy", "devproxy", map[string]string{"Origin": "http://localhost:5173", "Sec-Fetch-Site": "same-origin"}},
		{"the cli, which is no browser", "cli", nil},
	} {
		if code, body, _ := doWith(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"`+c.project+`"}`, c.hdr); code != 201 {
			t.Fatalf("%s: %d %v", c.name, code, body)
		}
	}
	if code, _, _ := doWith(t, client(t), http.MethodGet, base+"/api/v1/projects/dashboard", "", map[string]string{"Origin": "https://claims-portal.preview.localhost:7777"}); code != 200 {
		t.Fatalf("a read carrying a preview's origin answered %d: reads are untouched", code)
	}
}

func TestTheSchemesDefaultPortIsNotADifferentOrigin(t *testing.T) {
	for _, c := range []struct {
		origin, public string
		want           bool
	}{
		{"https://canyon.acme:443", "https://canyon.acme", true},
		{"https://canyon.acme", "https://canyon.acme:443", true},
		{"http://localhost:80", "http://localhost", true},
		{"http://localhost", "http://localhost:80", true},
		{"https://CANYON.acme", "https://canyon.acme", true},
		{"https://canyon.acme:8443", "https://canyon.acme", false},
		{"http://canyon.acme", "https://canyon.acme", false},
		{"https://claims-portal.preview.canyon.acme", "https://canyon.acme", false},
		{"null", "https://canyon.acme", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/projects", nil)
		r.Header.Set("Origin", c.origin)
		if got := fromCanyon(r, c.public); got != c.want {
			t.Errorf("Origin %s against %s: %v", c.origin, c.public, got)
		}
	}
}

func TestTheSessionCookieCannotBePlantedByASubdomain(t *testing.T) {
	for _, c := range []struct {
		public, name string
		secure       bool
	}{
		{"https://canyon.acme", "__Host-canyon_session", true},
		{"http://localhost:7777", "canyon_session", false},
	} {
		name, secure := sessionCookieOf(c.public)
		rec := httptest.NewRecorder()
		setSessionCookie(rec, name, "id", secure)
		// no Domain, ever: with one, a preview could shadow the real session, and __Host- would be invalid
		if ck := rec.Result().Cookies()[0]; ck.Name != c.name || ck.Secure != c.secure || ck.Path != "/" || ck.Domain != "" || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
			t.Fatalf("%s: name=%s secure=%v path=%s domain=%q", c.public, ck.Name, ck.Secure, ck.Path, ck.Domain)
		}
	}
}

func TestAnAuditRowNamingAProjectThatDoesNotExistIsAnErrorAndLeavesNothing(t *testing.T) {
	pool, ctx := migrated(t)
	a := &app{pool: pool}
	tx, err := a.mutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// the org row's foreign key is what replaced the existence check: a change that names a project
	// the transaction cannot see fails, and the audit row written before it does not survive the failure
	if err := events.AuditTx(ctx, tx, actor{Kind: "person", Subject: "anonymous"}, "PUT /api/v1/projects/{id}/owner", "nothere", "", nil); err == nil {
		t.Fatal("an audit row named a project that does not exist")
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("the failed transaction committed")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM timeline_rows"); n != 0 {
		t.Fatalf("the failure left %d rows", n)
	}
}

func TestTheLogLineNamesTheActorAndTheRefusal(t *testing.T) {
	pool, _ := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/nothing/owner", `{"display":"x"}`)
	do(t, c, http.MethodGet, base+"/health", "")
	for _, want := range []string{"PUT /api/v1/projects/nothing/owner 404 ", " person/anonymous", "GET /health 200 "} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log line %q missing (a refusal is a log line, and it says who):\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "GET /health 200 0s person") {
		t.Fatalf("an Open request names no actor:\n%s", logs.String())
	}
}

func TestUnroutedApiPathsAnswerTheContractsShape(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, body, _ := do(t, client(t), http.MethodGet, base+"/api/v1/nothing", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestTheAuditedActionNeverPanicsOnAPatternWithoutAMethod(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/projects", nil)
	r.Pattern = "/projects"
	if got := action(r); got != "POST /api/v1/projects" {
		t.Fatalf("%q", got)
	}
	r.Pattern = "POST /projects/{id}/owner"
	if got := action(r); got != "POST /api/v1/projects/{id}/owner" {
		t.Fatalf("%q", got)
	}
}

func TestAMutationIsNotDurableBeforeItsAnswerIsComplete(t *testing.T) {
	pool, ctx := migrated(t)
	a := &app{pool: pool}
	tx, err := a.mutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	if err := events.AuditTx(ctx, tx, actor{Kind: "person", Subject: "anonymous"}, "POST /api/v1/projects", "claims-portal", "", nil); err != nil {
		t.Fatal(err)
	}
	// the handler's own read of what it made happens inside the transaction, so a failure there still answers with nothing durable
	if p, err := project1(ctx, tx, "claims-portal"); err != nil || p.ID != "claims-portal" {
		t.Fatalf("the read inside the transaction: %+v %v", p, err)
	}
	if p, err := project1(ctx, pool, "claims-portal"); err == nil {
		t.Fatalf("the project is visible outside its transaction before the commit: %+v", p)
	}
	_ = tx.Rollback(ctx)
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects") + count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 0 {
		t.Fatalf("a rolled-back mutation left %d rows", n)
	}
}

// withOIDC starts the next test server under CANYON_AUTH=oidc with goodOIDC's lists: the domain
// meridian.example, admin dana.
func withOIDC(t *testing.T) {
	t.Helper()
	c, err := oidcConfigOf(envOf(goodOIDC))
	if err != nil {
		t.Fatal(err)
	}
	testAuth, testOIDC = authOIDC, c
	t.Cleanup(func() { testAuth, testOIDC = authOff, nil })
}

func TestUnderOIDCASessionlessRequestIs401BeforeTheHandlerAndWritesNothing(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, step := range []struct{ method, path string }{{http.MethodGet, "/api/v1/fleet"}, {http.MethodGet, "/api/v1/nothing"}, {http.MethodPost, "/api/v1/projects"}} {
		code, body, resp := do(t, client(t), step.method, base+step.path, `{"name":"claims-portal"}`)
		if code != 401 || body["error"] != "unauthenticated" || len(resp.Cookies()) != 0 {
			t.Fatalf("%s %s: %d %v, %d cookie(s)", step.method, step.path, code, body, len(resp.Cookies()))
		}
	}
	if lastBody != `{"error":"unauthenticated","message":"sign in to Canyon"}` {
		t.Fatalf("the refusal on the wire: %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM projects"); n != 0 {
		t.Fatalf("three refused requests left %d row(s)", n)
	}
	for _, path := range []string{"/health", "/auth/config"} {
		if code, _, _ := do(t, client(t), http.MethodGet, base+path, ""); code != 200 {
			t.Fatalf("%s is Open and answered %d", path, code)
		}
	}
}

func TestUnderOIDCASessionIsValidOnlyWhileLiveUnrevokedAndStillAMember(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, c := range []struct {
		name, email, row string
		want             int
	}{
		{"a live member", "dana@meridian.example", "now() + interval '1 hour', NULL", 201},
		{"an expired session", "dana@meridian.example", "now() - interval '1 second', NULL", 401},
		{"a revoked session", "dana@meridian.example", "now() + interval '1 hour', now()", 401},
		{"an email no longer on either list", "dana@elsewhere.example", "now() + interval '1 hour', NULL", 401},
		{"a demo row from before the mode changed", "", "NULL, NULL", 401},
		{"a member row that was given no expiry", "dana@meridian.example", "NULL, NULL", 401},
		{"a member whose stored address differs in case", "Dana@Meridian.Example", "now() + interval '1 hour', NULL", 201},
		{"a stored address with a second at sign", "x@evil.example@meridian.example", "now() + interval '1 hour', NULL", 401},
	} {
		id, err := token()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, expires_at, revoked_at) VALUES ($1, 'person', 'sub-'||$2, 'Dana Whitfield', $2, "+c.row+")", id, c.email); err != nil {
			t.Fatal(err)
		}
		cl := client(t)
		u, _ := neturl.Parse(base)
		cl.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: id}}) //nolint:gosec // test: the row just inserted
		code, body, resp := do(t, cl, http.MethodPost, base+"/api/v1/projects", `{"id":"`+strings.ReplaceAll(c.name, " ", "-")+`"}`)
		if code != c.want || len(resp.Cookies()) != 0 {
			t.Fatalf("%s: %d %v, %d cookie(s); want %d and no cookie", c.name, code, body, len(resp.Cookies()), c.want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE actor_subject LIKE 'sub-%'"); n != 2 {
		t.Fatalf("the two accepted mutations are recorded as their sessions' subjects: %d rows", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions"); n != 8 {
		t.Fatalf("a refusal mints nothing: %d sessions", n)
	}
}

func TestExpiredSessionsAreSweptAtBootAndLiveOnesStay(t *testing.T) {
	pool, ctx := migrated(t)
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, expires_at) VALUES ('gone', 'person', 'a', now() - interval '1 second'), ('live', 'person', 'b', now() + interval '1 hour'), ('demo', 'person', 'anonymous', NULL)"); err != nil {
		t.Fatal(err)
	}
	if n, err := sweepSessions(ctx, pool); err != nil || n != 1 || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE id IN ('live', 'demo')") != 2 {
		t.Fatalf("swept %d (%v)", n, err)
	}
}

// The two lifetimes are design/AUTH.md §5's table, pinned before anything mints them.
func TestSessionLifetimesAreTheDesignsTable(t *testing.T) {
	if browserSession != 12*time.Hour || deviceSession != 7*24*time.Hour {
		t.Fatalf("lifetimes: browser %s, device %s", browserSession, deviceSession)
	}
}

// setRow writes the org's row directly, for the states a route cannot reach from outside.
func setRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, set string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET "+set); err != nil {
		t.Fatal(err)
	}
}

func mustURL(t *testing.T, raw string) *neturl.URL {
	t.Helper()
	u, err := neturl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
