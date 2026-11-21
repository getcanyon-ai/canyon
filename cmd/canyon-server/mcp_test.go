package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
)

// mcpCall posts one JSON-RPC message to /mcp with token as the bearer ("" for none) and the headers given.
func mcpCall(t *testing.T, base, token, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

const (
	mcpList    = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	mcpPromote = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_promote","arguments":{"app":"claims-portal"}}}`
)

// With sign-in off every caller is the anonymous admin, so /mcp hands nothing out: it names canyon mcp instead.
func TestMCPRemoteNeedsSignIn(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, body := mcpCall(t, base, "", mcpList, nil); code != http.StatusNotFound || !strings.Contains(body, `"error":"mcp_needs_sign_in"`) || !strings.Contains(body, "canyon mcp") {
		t.Errorf("sign-in off: %d %s", code, body)
	}
}

// /mcp takes an access token its own authorization server issued for it, live, and nothing else: no token is 401
// with where to get one (RFC 9728), and a revoked session's token stops at once.
func TestMCPRemoteTakesItsOwnTokensAlone(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	access, _ := signedInOver(t, base, gina, register(t, base, redirect), redirect)
	code, h, _ := mcpCall(t, base, "", mcpList, nil)
	if code != http.StatusUnauthorized || h.Get("WWW-Authenticate") != `Bearer resource_metadata="http://localhost:7777/.well-known/oauth-protected-resource/mcp", scope="read act"` {
		t.Errorf("no token: %d %q", code, h.Get("WWW-Authenticate"))
	}
	cookie := gina.Jar.Cookies(mustURL(t, base))[0].Value // a browser session's id is never a bearer
	for name, token := range map[string]string{"a browser session's id": cookie, "a made-up token": strings.Repeat("a", 64)} {
		if code, _, _ := mcpCall(t, base, token, mcpList, nil); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, body := mcpCall(t, base, access, mcpList, nil); code != http.StatusOK || !strings.Contains(body, "canyon_rules") {
		t.Fatalf("the token: %d %s", code, body)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET access_expires_at = now() - interval '1 second' WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("an expired access token: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET access_expires_at = now() + interval '1 hour', resource = 'http://localhost:7777/other' WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("a token for another resource: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET resource = 'http://localhost:7777/mcp', revoked_at = now() WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("a revoked session's token: %d", code)
	}
}

// A browser page on another site never reaches /mcp; a native client, which sends no Origin, does. The transport is
// stateless: GET and DELETE have no session to serve.
func TestMCPRemoteRefusesAnotherSitesOrigin(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	access, _ := signedInOver(t, base, gina, register(t, base, redirect), redirect)
	for origin, want := range map[string]int{"": http.StatusOK, "http://localhost:7777": http.StatusOK, "https://evil.example": http.StatusForbidden} {
		if code, _, body := mcpCall(t, base, access, mcpList, map[string]string{"Origin": origin}); code != want {
			t.Errorf("Origin %q: %d %s, want %d", origin, code, body, want)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(t.Context(), method, base+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: %d, want 405", method, resp.StatusCode)
		}
	}
}

// A read token lists every tool but runs none that changes what runs: each act tool answers insufficient_scope, judged
// inside the tool from the call the SDK dispatched, so no spelling of the message reaches one by another parse.
func TestMCPRemoteActsOnlyWithTheActScope(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	id := register(t, base, redirect)
	read, _ := signedInOver(t, base, gina, id, redirect)
	if code, _, body := mcpCall(t, base, read, mcpList, nil); code != http.StatusOK || !strings.Contains(body, "canyon_promote") || !strings.Contains(body, "canyon_deploy_ref") {
		t.Errorf("a read token's list: %d %s", code, body)
	}
	call := func(tool string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"app":"claims-portal","ref":"main"}}}`
	}
	refused := func(name, msg string) {
		t.Helper()
		if code, _, body := mcpCall(t, base, read, msg, nil); code == http.StatusOK && !strings.Contains(body, "needs permission to act for you") {
			t.Errorf("%s ran with a read token: %s", name, body)
		}
	}
	for _, tool := range []string{"canyon_promote", "canyon_check_ref", "canyon_deploy_ref"} {
		refused(tool, call(tool))
		code, h, body := mcpCall(t, base, read, call(tool), nil) // the step-up a client can act on: which scope to ask the person for
		if code != http.StatusForbidden || !strings.Contains(h.Get("WWW-Authenticate"), `error="insufficient_scope", scope="read act"`) ||
			!strings.Contains(body, tool+" needs permission to act for you.") {
			t.Errorf("%s with a read token: %d %q %s", tool, code, h.Get("WWW-Authenticate"), body)
		}
	}
	refused("a batch", "["+call("canyon_promote")+"]")
	refused("a mixed-case duplicate key", `{"jsonrpc":"2.0","id":2,"method":"tools/call","Method":"tools/list","params":{"name":"canyon_promote","Name":"canyon_rules","arguments":{"app":"claims-portal"}}}`)
	refused("a duplicate name", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_rules","name":"canyon_promote","arguments":{"app":"claims-portal"}}}`)
	act, _ := signedInOver(t, base, gina, id, redirect, "read", "act")
	if _, _, body := mcpCall(t, base, act, call("canyon_promote"), nil); strings.Contains(body, "needs permission to act") {
		t.Errorf("an act token's promote stopped at the scope: %s", body)
	}
}

// The CLI's tool handlers on /mcp reach this Canyon's /api/v1 alone, in-process: no other host, scheme or path.
func TestMCPRemoteToolsReachTheAPIAlone(t *testing.T) {
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the API was reached") })
	const public = "http://localhost:7777"
	c := &http.Client{Transport: &mcpTransport{a: &app{public: public}, r: httptest.NewRequest(http.MethodPost, "/mcp", nil), api: api}}
	for _, at := range []string{public + "/oauth/token", public + "/api/v1/../oauth/token", public + "/api/v1x/fleet",
		"https://localhost:7777/api/v1/fleet", "http://169.254.169.254/api/v1/fleet", "http://localhost:7778/api/v1/fleet", "http://canyon.mcp.invalid/api/v1/fleet"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, at, nil)
		if resp, err := c.Do(req); err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: answered %d", at, resp.StatusCode)
		}
	}
}

// What a token may ask of /api/v1 over /mcp is a fixed list of methods and routes, fail closed: read is GETs alone, act
// adds the four POSTs that check, deploy and promote, and nothing else is reachable with either, a secret's PUT first.
func TestMCPRemoteAsksOnlyWhatItsScopeAllows(t *testing.T) {
	read, act := []string{"read"}, []string{"read", "act"}
	for _, c := range []struct {
		scopes       []string
		method, path string
		want         string // "" admitted, else the refusal's code
	}{
		{read, "GET", "/projects/claims-portal/status", ""},
		{read, "GET", "/gate-runs/gr_1/sarif", ""},
		{read, "GET", "/projects/claims-portal/logs?env=preview&redaction=masked", ""},
		{read, "GET", "/projects/claims-portal/logs?env=preview", "not_allowed"},
		{act, "GET", "/projects/claims-portal/logs?env=preview&redaction=raw", "not_allowed"},
		{read, "POST", "/projects/claims-portal/promotions", "insufficient_scope"},
		{read, "POST", "/projects/claims-portal/packages/from-repository", "insufficient_scope"},
		{read, "POST", "/projects/claims-portal/deploys", "insufficient_scope"},
		{act, "POST", "/projects/claims-portal/promotions", ""},
		{act, "POST", "/projects/claims-portal/gate-runs", ""},
		{act, "PUT", "/projects/claims-portal/secrets/STRIPE_KEY", "not_allowed"},
		{act, "DELETE", "/projects/claims-portal/secrets/STRIPE_KEY", "not_allowed"},
		{act, "POST", "/projects", ""}, // canyon_create_app's, at most ten a day (mayMake)
		{read, "POST", "/projects", "insufficient_scope"},
		{act, "PUT", "/projects/claims-portal/repository", ""},
		{act, "DELETE", "/projects/claims-portal/repository", "not_allowed"},
		{act, "DELETE", "/projects/claims-portal", ""}, // of an app that request made alone (madeHere)
		{read, "DELETE", "/projects/claims-portal", "insufficient_scope"},
		{act, "PUT", "/projects/claims-portal/access", "not_allowed"},
		{act, "POST", "/gate-runs/gr_1/findings/approve", "not_allowed"},
		{act, "DELETE", "/sessions/ses_0123456789abcdef", "not_allowed"},
		{act, "PUT", "/preferences", "not_allowed"},
		{act, "POST", "/projects/claims-portal/packages", "not_allowed"},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if _, code, _ := mcpAdmits(c.scopes, r); code != c.want {
			t.Errorf("%v %s %s: %q, want %q", c.scopes, c.method, c.path, code, c.want)
		}
	}
}

// A review /mcp starts is kept as mcp-remote by its session's channel, which only the bearer stage sets: a client's
// header never claims it, and canyon mcp's own header is still mcp-local.
func TestMCPRemoteReviewsAreKeptAsMCPRemote(t *testing.T) {
	for name, c := range map[string]struct {
		via, header, want string
	}{
		"through /mcp":                 {"mcp", "", transportMCPRemote},
		"through /mcp, a header aside": {"mcp", transportMCPLocal, transportMCPRemote},
		"canyon mcp":                   {"cli", transportMCPLocal, transportMCPLocal},
		"a client claiming /mcp":       {"cli", transportMCPRemote, ""},
		"a browser claiming /mcp":      {"dashboard", transportMCPRemote, ""},
		"the CLI":                      {"cli", "", ""},
	} {
		r := httptest.NewRequestWithContext(events.WithProvenance(t.Context(), events.Provenance{Via: c.via}), http.MethodPost, "/projects/x/gate-runs", nil)
		r.Header.Set("Canyon-Transport", c.header)
		if got := transportOf(r); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A tool on /mcp is the person: the route it calls judges their level as it would a browser's, an act token or not,
// and its refusal is the route's own words, on the trail as theirs, via mcp, under their session's digest.
func TestMCPRemoteToolsActAsThePerson(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	const redirect = "https://claude.ai/cb"
	act, _ := signedInOver(t, base, gina, register(t, base, redirect), redirect, "read", "act")
	code, _, body := mcpCall(t, base, act, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_deploy_ref","arguments":{"app":"claims-portal","ref":"main"}}}`, nil)
	if code != http.StatusOK || !strings.Contains(body, `"isError":true`) {
		t.Fatalf("a member's deploy_ref on an app they can't change: %d %s", code, body)
	}
	var session string
	if err := pool.QueryRow(ctx, "SELECT id FROM sessions WHERE flow = 'oauth' AND actor_subject = 'sub-gina'").Scan(&session); err != nil {
		t.Fatal(err)
	}
	var via, digest string
	if err := pool.QueryRow(ctx, `SELECT via, session FROM audit_events WHERE action = 'denied' AND actor_subject = 'sub-gina' AND project_id = 'claims-portal'`).Scan(&via, &digest); err != nil {
		t.Fatalf("no denied row for gina's refused deploy_ref: %v", err)
	}
	if via != "mcp" || digest != sessionDigest(session) {
		t.Errorf("the denied row: via %q session %q, want mcp and %q", via, digest, sessionDigest(session))
	}
}

// Where a person approves production in the dashboard, a promote over /mcp is the CLI's: a request with its link, no
// promotion, on the trail via mcp. The tool's session is never the dashboard's, whatever a client sends.
func TestMCPRemotePromoteWithProductionApprovalOnPromotesNothing(t *testing.T) {
	pool, ctx, base, browser, _, _ := approvalHost(t, true)
	const redirect = "https://claude.ai/cb"
	act, _ := signedInOver(t, base, browser, register(t, base, redirect), redirect, "read", "act")
	code, _, body := mcpCall(t, base, act, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_promote","arguments":{"app":"claims-portal","confirm":true}}}`,
		map[string]string{"Sec-Fetch-Site": "same-origin", "Canyon-Transport": "dashboard"})
	if code != http.StatusOK || !strings.Contains(body, "http://localhost:7777/#/approve?code=") {
		t.Fatalf("a remote promote with approval on: %d %s", code, body)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'"); n != 0 {
		t.Errorf("a remote promote started %d promote runs past the dashboard's approval", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request promotion' AND via = 'mcp' AND project_id = 'claims-portal'"); n != 1 {
		t.Errorf("request promotion rows via mcp: %d, want 1", n)
	}
}

// A tool deletes only an app its own request made: canyon_create_app's undo of a link refused, never another app.
func TestMCPRemoteDeletesOnlyAnAppItMade(t *testing.T) {
	tr := &mcpTransport{made: map[string]bool{"quiet-river-4k2": true}}
	for path, want := range map[string]bool{"/projects/quiet-river-4k2": true, "/projects/claims-portal": false, "/projects/quiet-river-4k": false} {
		if got := tr.madeHere(httptest.NewRequest(http.MethodDelete, path+"?confirm=x", nil)); got != want {
			t.Errorf("DELETE %s: %v, want %v", path, got, want)
		}
	}
}

// canyon_create_app at its boundary (design/specs/mcp.md §2.3): a member whose GitHub account reads the repository
// makes an app they own, linked to it; one with no GitHub account linked gets their link page and keeps no app; a read
// token makes nothing; and the eleventh app a day is refused before anything is made.
func TestMCPRemoteCreateApp(t *testing.T) {
	fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	installedApp(t, ctx, pool, vaultOf(t, base))
	if _, err := pool.Exec(ctx, "INSERT INTO github_links (github_id, subject, login) VALUES (7002, 'sub-carol', 'carol-gh')"); err != nil {
		t.Fatal(err)
	}
	const redirect = "https://claude.ai/cb"
	connect := func(sub, email string, scope ...string) string {
		browser := signedIn(t, ctx, pool, base, sub, email, false)
		access, _ := signedInOver(t, base, browser, register(t, base, redirect), redirect, scope...)
		return access
	}
	create := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_create_app","arguments":{"repository":"acme/claims-portal"}}}`
	carol := connect("sub-carol", "carol@meridian.example", "read", "act")
	if code, _, body := mcpCall(t, base, carol, create, nil); code != http.StatusOK || strings.Contains(body, `"isError":true`) || !strings.Contains(body, "linked to acme/claims-portal") {
		t.Fatalf("carol makes an app from a repository she reads: %d %s", code, body)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM projects p JOIN project_repositories l ON l.project_id = p.id AND l.full_name = 'acme/claims-portal'
		JOIN project_access a ON a.project_id = p.id AND a.level = 'owner' AND a.subject = 'sub-carol' WHERE p.deleted_at IS NULL`); n != 1 {
		t.Errorf("carol's app, linked and hers: %d", n)
	}
	var made string
	if err := pool.QueryRow(ctx, "SELECT project_id FROM project_repositories WHERE full_name = 'acme/claims-portal'").Scan(&made); err != nil {
		t.Fatal(err)
	}
	link := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"canyon_secret_link","arguments":{"app":"` + made + `","name":"STRIPE_KEY"}}}`
	if _, _, body := mcpCall(t, base, carol, link, nil); !strings.Contains(body, "http://localhost:7777/#/apps/"+made+"/secrets?env=preview") || !strings.Contains(body, "name=STRIPE_KEY") {
		t.Errorf("carol's secret link: %s", body)
	}
	unseen := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"canyon_create_app","arguments":{"repository":"acme/secret"}}}`
	if _, _, body := mcpCall(t, base, carol, unseen, nil); !strings.Contains(body, "Ask an admin to give Canyon's GitHub App access to it") ||
		count(t, ctx, pool, "SELECT count(*) FROM projects p JOIN project_access a ON a.project_id = p.id AND a.subject = 'sub-carol' WHERE p.deleted_at IS NULL") != 1 {
		t.Errorf("a repository no App sees: %s", body)
	}
	gina := connect("sub-gina", "gina@meridian.example", "read", "act")
	if _, _, body := mcpCall(t, base, gina, create, nil); !strings.Contains(body, "http://localhost:7777/api/v1/github/link") {
		t.Errorf("gina, no GitHub account linked: %s", body)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects p JOIN project_access a ON a.project_id = p.id AND a.subject = 'sub-gina' WHERE p.deleted_at IS NULL"); n != 0 {
		t.Errorf("gina's refused link left %d app(s)", n)
	}
	read := connect("sub-vic", "vic@meridian.example")
	if code, _, _ := mcpCall(t, base, read, create, nil); code != http.StatusForbidden {
		t.Errorf("a read token's create: %d", code)
	}
	// the day's count and the create are one step: five calls at once at nine made make at most one more, and the pool serves on
	madeToday := "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects' AND via = 'mcp' AND actor_subject = 'sub-carol'"
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, actor_kind, actor_subject, action, via) SELECT 'ev_cap' || n, 'person', 'sub-carol', 'POST /api/v1/projects', 'mcp'
		FROM generate_series(1, $1) n`, 9-count(t, ctx, pool, madeToday)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	said := make(chan string, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, body := mcpCall(t, base, carol, create, nil)
			said <- body
		}()
	}
	wg.Wait()
	close(said)
	for body := range said { // each made the one, or was told why not: the cap, another under way, or a link refused
		if !strings.Contains(body, "the most for one day") && !strings.Contains(body, "Another app is being made for you right now") && !strings.Contains(body, "Nothing was made") && !strings.Contains(body, "Made ") {
			t.Errorf("a call at once: %s", body)
		}
	}
	if n := count(t, ctx, pool, madeToday); n > 10 {
		t.Errorf("five calls at once at nine made: %d made today, want at most 10", n)
	}
	if code, _, _ := mcpCall(t, base, carol, mcpList, nil); code != http.StatusOK || count(t, ctx, pool, "SELECT 1") != 1 { // the pool serves on
		t.Errorf("after the calls at once, /mcp answered %d", code)
	}
}
