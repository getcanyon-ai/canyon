package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const setupRequired = `{"error":"setup_required","message":"Canyon is being set up"}`

// A fresh oidc host serves nothing but the two preferences routes until somebody has finished
// setup: every other /api/v1 route, routed or not, is 409 after the session stage and before the
// handler, with no audit row and the pipeline's one log line naming the person. No session is
// still 401, and a member who is not a bootstrap admin has no session at all.
func TestAFreshHostAnswersSetupRequiredOnEveryRouteButThePreferences(t *testing.T) {
	withOIDC(t) // the domain meridian.example, the bootstrap admin dana
	pool, ctx := migrated(t)
	inSetup(t, ctx, pool)
	base, logs := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/fleet", ""}, {http.MethodGet, "/projects", ""}, {http.MethodPost, "/projects", `{"name":"claims-portal"}`},
		{http.MethodGet, "/audit", ""}, {http.MethodGet, "/policies", ""}, {http.MethodGet, "/timeline", ""}, {http.MethodGet, "/skill/manifest", ""},
		{http.MethodGet, "/design-systems", ""}, {http.MethodGet, "/component-types", ""}, {http.MethodDelete, "/sessions", ""}, {http.MethodGet, "/nothing", ""},
	} {
		if code, _, _ := do(t, dana, r.method, base+"/api/v1"+r.path, r.body); code != 409 || lastBody != setupRequired {
			t.Fatalf("%s %s in setup: %d %s", r.method, r.path, code, lastBody)
		}
	}
	if code, _, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet", ""); code != 401 {
		t.Fatalf("no session is still 401, not 409: %d", code)
	}
	if code, _, _ := do(t, carol, http.MethodGet, base+"/api/v1/preferences", ""); code != 401 {
		t.Fatalf("a member of the domain who is not a bootstrap admin has no session in setup: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodGet, base+"/api/v1/preferences", ""); code != 200 || body["onboarded_at"] != nil {
		t.Fatalf("the read is served: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/preferences", `{"org_name":"Meridian Insurance"}`); code != 200 {
		t.Fatalf("the PUT is served: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'PUT /api/v1/preferences'"); n != 0 {
		t.Fatalf("the refusals left %d row(s)", n)
	}
	if !strings.Contains(logs.String(), "POST /api/v1/projects 409 ") || !strings.Contains(logs.String(), " person/sub-dana") {
		t.Fatalf("a refusal is one log line, and it says who:\n%s", logs.String())
	}
}

// /ready answers 200 while the host is in setup (design/ONBOARDING.md §2, contracts/routes.md): the
// process is healthy, migrated and connected, and what is missing is a person's decision. A 503
// would make a load balancer pull the host out of rotation, and then nobody could reach the
// onboarding screen to fix it.
func TestReadyAnswers200WhileTheHostIsInSetup(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	inSetup(t, ctx, pool)
	base, _ := start(t, pool, 20*time.Millisecond)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		code, body := get(t, base+"/ready")
		if code == 200 && body["status"] == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("in setup /ready answered %d %v; onboarding must stay reachable", code, body)
		}
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); code != 409 { // the same host, in setup: ready is about the process
		t.Fatalf("the fleet on a ready host in setup: %d %s", code, lastBody)
	}
}

// The setup state end to end against the fake issuer (design/ONBOARDING.md §2 and §4): no
// membership list in the environment, only the bootstrap admin. Bob, on no list, is refused and
// leaves nothing; Dana signs in, is served the preferences and nothing else, cannot finish with
// nobody allowed, names the two of them and finishes — audited by name — and the host opens: Bob
// is in, and taken off the row's list he is out on his next request.
func TestOnlyABootstrapAdminSignsInDuringSetupAndFinishingItOpensTheHost(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	testOIDC.emails = nil // the row carries membership once Dana has set it: §4's team without a domain
	pool, ctx := migrated(t)
	inSetup(t, ctx, pool)
	base, logs := start(t, pool, time.Hour)
	signIn := func(sub, email string) (*http.Client, int, string) {
		idp.mu.Lock()
		idp.bend = map[string]any{"sub": sub, "email": email}
		idp.mu.Unlock()
		br := browser(t)
		callback, _, _, _ := startSignIn(t, br, base, idp)
		resp := fetch(t, br, callback)
		return br, resp.StatusCode, resp.Header.Get("Location")
	}
	setRow(t, ctx, pool, "allowed_emails = '{bob@meridian.example}'") // bob is on the row's list and is not a bootstrap admin: during setup the ordinary lists do not admit him
	// Bob is refused because nobody has finished installing, not because he got it wrong: the one
	// screen that says so is the one he has to land on. "Try again" would be the wrong instruction.
	if _, code, where := signIn("u2", "bob@meridian.example"); code != 302 || where != setupRequiredRoute {
		t.Fatalf("bob in setup: %d %s %s", code, where, lastBody)
	}
	setRow(t, ctx, pool, "allowed_emails = '{}'")
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'sign in refused'"); n != 0 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'sign in refused' AND detail->>'reason' = 'setup'") != 1 {
		t.Fatalf("a refused sign-in left %d row(s) beside its one sign in refused row", n)
	}
	if !strings.Contains(logs.String(), "canyon-server: sign in refused: bob@meridian.example is not a bootstrap admin, and Canyon is being set up\n") {
		t.Fatalf("the reason goes to the log:\n%s", logs.String())
	}
	dana, code, where := signIn("u1", "dana@meridian.example")
	if code != 302 || where != "/" {
		t.Fatalf("the bootstrap admin in setup: %d %s %s", code, where, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); code != 409 {
		t.Fatalf("the fleet in setup: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodGet, base+preferencesPath, ""); code != 200 || string(mustJSON(body["overridden"])) != `["admin_claim","admin_value"]` || string(mustJSON(body["admin_emails"])) != `[]` || string(mustJSON(body["bootstrap_admins"])) != `["dana@meridian.example"]` {
		t.Fatalf("the environment grants its admin without pinning the key, and sets the claim: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"onboarded_at":"2026-09-16T09:00:00Z"}`); code != 400 || body["error"] != "no_members" {
		t.Fatalf("finishing with nobody allowed: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["dana@meridain.example"],"onboarded_at":"2026-09-16T09:00:00Z"}`); code != 400 || body["error"] != "admin_locked_out" || !strings.Contains(lastBody, "dana@meridian.example is on neither membership list") {
		t.Fatalf("one transposed letter would end setup with nobody able to sign in: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["dana@meridian.example","bob@meridian.example"],"onboarded_at":"2026-09-16T09:00:00Z"}`); code != 200 {
		t.Fatalf("finishing: %d %s", code, lastBody)
	}
	var act string
	if err := pool.QueryRow(ctx, "SELECT action FROM audit_events ORDER BY ts DESC, id DESC LIMIT 1").Scan(&act); err != nil || act != "complete setup" {
		t.Fatalf("the write that ends the setup state is on the trail by name: %q %v", act, err)
	}
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("the host leaves setup on that write: %d %s", code, lastBody)
	}
	bob, code, where := signIn("u2", "bob@meridian.example")
	if code != 302 || where != "/" {
		t.Fatalf("bob after setup, on the row's list: %d %s %s", code, where, lastBody)
	}
	if code, _, _ := do(t, bob, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("bob's session: %d %s", code, lastBody)
	}
	do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["dana@meridian.example"]}`)
	if code, _, _ := do(t, bob, http.MethodGet, base+"/api/v1/fleet", ""); code != 401 {
		t.Fatalf("taken off the row's list, bob is out on his next request: %d", code)
	}
}

// The admin claim the row carries governs sign-in, not only the environment's. An org that says
// "anyone in this group administers Canyon" through the product got a screen that echoed it back
// and a server that ignored it forever, with no error anywhere (canyonv2-cr23).
func TestTheAdminClaimTheRowCarriesGrantsAdmin(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	testOIDC.emails, testOIDC.adminClaim, testOIDC.adminValue = nil, "", "" // the environment names no claim and no list: both are the row's
	pool, ctx := migrated(t)
	setRow(t, ctx, pool, "allowed_domains = '{meridian.example}', admin_claim = 'groups', admin_value = 'platform-admins'")
	base, _ := start(t, pool, time.Hour)
	idp.mu.Lock()
	idp.bend = map[string]any{"sub": "u3", "email": "bob@meridian.example", "groups": []any{"platform-admins"}}
	idp.mu.Unlock()
	br := browser(t)
	callback, _, _, _ := startSignIn(t, br, base, idp)
	if code := fetch(t, br, callback).StatusCode; code != 302 {
		t.Fatalf("bob is on the row's domain: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, br, http.MethodPut, base+preferencesPath, stacksBody); code != 200 { // CANYON_ADMINS names dana, so only the row's claim can have made bob an admin
		t.Fatalf("the row's admin claim: %d %s", code, lastBody)
	}
}
