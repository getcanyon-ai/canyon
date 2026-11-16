package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// signedIn is a client holding a live session row as the callback writes it: the subject, the
// stored email and the claimed_admin flag, which is all the role is computed from — its address one the
// provider asserted verified and its groups read whole, as a sign-in records them.
func signedIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, sub, email string, claimedAdmin bool) *http.Client {
	t.Helper()
	id, err := token()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, email_verified, groups_read, claimed_admin, flow, expires_at) VALUES ($1, 'person', $2, $2, $3, true, 'ok', $4, 'browser', now() + interval '1 hour')", id, sub, email, claimedAdmin); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	u, _ := neturl.Parse(base)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: id}}) //nolint:gosec // test: the row just inserted
	return c
}

const (
	stacksBody      = `{"stacks":[{"language":"TypeScript","frameworks":["React"]}]}`
	adminRequired   = `{"error":"admin_required","message":"only an admin can do this: ask dana@meridian.example"}`
	registryDSBody  = `{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://registry.meridian.example/r"}`
	preferencesPath = "/api/v1/preferences"
)

// The role is CANYON_ADMINS contains the stored email OR claimed_admin, read on every request and
// never stored (design/AUTH.md §4.4). CANYON_ADMINS is environment, so a change to it is a restart:
// two servers over one schema, the same three sessions against each.
func TestTheRoleIsComputedPerRequestFromTheLiveListAndTheClaimedFlag(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	listed, _ := start(t, pool, time.Hour)
	env := map[string]string{}
	for k, v := range goodOIDC {
		env[k] = v
	}
	env["CANYON_ADMINS"] = "erin@meridian.example"
	demoted, err := oidcConfigOf(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	prior := testOIDC
	testOIDC = demoted
	t.Cleanup(func() { testOIDC = prior }) // a later start in this test would otherwise get the demoted list
	unlisted, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, listed, "sub-dana", "Dana@Meridian.Example", false) // on the list at the first host, stored in the issuer's case
	bob := signedIn(t, ctx, pool, listed, "sub-bob", "bob@meridian.example", true)     // the admin group at sign-in, never on the list
	erin := signedIn(t, ctx, pool, listed, "sub-erin", "erin@meridian.example", false) // promoted onto the list after signing in
	carol := signedIn(t, ctx, pool, listed, "sub-carol", "carol@meridian.example", false)
	accepted := 0
	for i, c := range []struct {
		who  string
		c    *http.Client
		base string
		want int
	}{
		{"dana on the list", dana, listed, 200}, {"bob by the claimed flag", bob, listed, 200}, {"carol", carol, listed, 403},
		{"dana after the list changed", dana, unlisted, 403}, {"bob by the claimed flag", bob, unlisted, 200}, {"carol", carol, unlisted, 403},
		{"erin before her promotion", erin, listed, 403}, {"erin after it, on the same session", erin, unlisted, 200},
	} {
		code, _, _ := do(t, c.c, http.MethodPut, c.base+preferencesPath, strings.Replace(stacksBody, "React", "React"+strconv.Itoa(i), 1)) // a changed value each time: an unchanged PUT is audited as nothing
		refused := adminRequired                                                                                                           // the refusal asks the host's first admin: dana on the first, erin on the second
		if c.base == unlisted {
			refused = strings.Replace(adminRequired, "dana@", "erin@", 1)
		}
		if code != c.want || (code == 403 && lastBody != refused) {
			t.Fatalf("%s: %d %s, want %d", c.who, code, lastBody, c.want)
		}
		if code == 200 {
			accepted++
		}
	}
	if n, d := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'"), count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'"); n != accepted || d != 8-accepted {
		t.Fatalf("the accepted writes are audited and each refused one is one denied row: %d rows for %d accepted, %d denied", n, accepted, d)
	}
}

// A member is refused each org-wide write before the handler reads the body: no row, no audit row,
// and the setup route never reaches the model.
func TestAMemberIsRefusedTheOrgWideWritesBeforeAnythingIsWritten(t *testing.T) {
	withOIDC(t)
	m := withModel(t, &scripted{line: "never reached"})
	var conversions atomic.Int32 // the GitHub callback below holds a matching state: only the admin check keeps it from GitHub
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { conversions.Add(1) }))
	t.Cleanup(api.Close)
	testGitHubAPI = api.URL
	t.Cleanup(func() { testGitHubAPI = "" })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	state := strings.Repeat("ab", 32)
	member.Jar.SetCookies(&neturl.URL{Scheme: "http", Host: strings.TrimPrefix(base, "http://")}, []*http.Cookie{{Name: githubStateCookie, Value: state}}) //nolint:gosec // test: a state the member holds
	for _, w := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/github", ""},
		{http.MethodGet, "/api/v1/github/app/new", ""},
		{http.MethodGet, "/api/v1/github/app/callback?code=c&state=" + state, ""},
		{http.MethodPut, preferencesPath, stacksBody},
		{http.MethodPost, "/api/v1/design-systems", registryDSBody},
		{http.MethodPost, "/api/v1/guardrails/setup", oneMessage},
		{http.MethodPut, policyPath + "?apply=true", `{"state":"blocking"}`},
	} {
		if code, _, _ := do(t, member, w.method, base+w.path, w.body); code != 403 || lastBody != adminRequired {
			t.Errorf("%s %s as a member: %d %s", w.method, w.path, code, lastBody) // and go on: the rows it left are the second half of the fault
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'") + count(t, ctx, pool, "SELECT count(*) FROM design_systems WHERE source_type = 'registry'") + count(t, ctx, pool, "SELECT count(*) FROM org_preferences WHERE stacks <> '[]'::jsonb") + count(t, ctx, pool, "SELECT count(*) FROM policy_edits"); n != 0 || m.turns != 0 || conversions.Load() != 0 {
		t.Fatalf("seven refusals left %d row(s), reached the model %d time(s) and GitHub %d", n, m.turns, conversions.Load())
	}
	if d := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'admin_required'"); d != 7 {
		t.Fatalf("seven refusals are seven denied rows: %d", d)
	}
	if code, _, _ := do(t, member, http.MethodGet, base+preferencesPath, ""); code != 200 {
		t.Fatalf("a member still reads the org's settings: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, member, http.MethodGet, base+"/api/v1/policies", ""); code != 200 {
		t.Fatalf("a member still reads the policies: %d %s", code, lastBody)
	}
}

// The reader's proof against the fake issuer: sign in as someone on neither CANYON_ADMINS nor in
// the admin group and the org-wide write is 403; as someone on the list, or in the group, it is not.
func TestSignedInThroughTheIssuerAMemberIs403OnTheOrgWideWriteAndAnAdminIsNot(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	testOIDC.domains = []string{"meridian.example"} // before start: bob and erin are members of the domain, and only dana is on CANYON_ADMINS
	pool, _ := migrated(t)
	base, logs := start(t, pool, time.Hour)
	for _, c := range []struct {
		sub, email string
		groups     []any
		want       int
		line       string
	}{
		{"u2", "bob@meridian.example", nil, 403, "sub=u2 matched=email bob@meridian.example admin_claim=absent role=member"},
		{"u1", "dana@meridian.example", nil, 200, "sub=u1 matched=email dana@meridian.example admin_claim=absent role=admin"},
		{"u3", "erin@meridian.example", []any{"platform-admins"}, 200, "sub=u3 matched=email erin@meridian.example admin_claim=hit role=admin"},
	} {
		idp.mu.Lock()
		idp.bend = map[string]any{"sub": c.sub, "email": c.email}
		if c.groups != nil {
			idp.bend["groups"] = c.groups
		}
		idp.mu.Unlock()
		br := browser(t)
		callback, _, _, _ := startSignIn(t, br, base, idp)
		if resp := fetch(t, br, callback); resp.StatusCode != 302 {
			t.Fatalf("%s: the callback answered %d %s", c.email, resp.StatusCode, lastBody)
		}
		if code, _, _ := do(t, br, http.MethodPut, base+preferencesPath, stacksBody); code != c.want || (code == 403 && lastBody != adminRequired) {
			t.Fatalf("%s: PUT /api/v1/preferences is %d %s, want %d", c.email, code, lastBody, c.want)
		}
		if !strings.Contains(logs.String(), "canyon-server: sign in "+c.line+"\n") {
			t.Fatalf("the sign-in line names the role the routes use:\n%s", logs.String())
		}
	}
}

// adminOnly fails closed: a context that carries no role at all, which is what a handler run
// outside the session stage would see, is a member.
func TestAdminOnlyRefusesAContextWithNoRole(t *testing.T) {
	pool, _ := migrated(t) // the refusal reads the org's admins to say whom to ask
	called := false
	h := (&app{auth: authOIDC, pool: pool}).adminOnly(func(http.ResponseWriter, *http.Request) { called = true })
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPut, "/x", nil))
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("no role in the context: %d, handler ran %v; want 403 and never", w.Code, called)
	}
}

// A request refused 403 writes one denied row once it is answered: its actor the one refused, the app the
// route names, the key the request would have written and the refusal's code; the refused request itself
// still changes nothing (contracts/routes.md, the audit read).
func TestARefusedRequestWritesOneDeniedRow(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPost, base+"/api/v1/projects", `{"id":"danas"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, erin, http.MethodPut, base+preferencesPath, stacksBody); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's preferences edit: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, erin, http.MethodGet, base+"/api/v1/audit?project=danas", ""); code != 403 || body["error"] != "team_required" {
		t.Fatalf("a member's read of an app they are not on: %d %s", code, lastBody)
	}
	for _, want := range []string{
		"sub-erin  PUT /api/v1/preferences admin_required",
		"sub-erin danas GET /api/v1/audit team_required",
	} {
		if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND concat_ws(' ', actor_subject, coalesce(project_id, ''), detail->>'action', detail->>'error') = '"+want+"'"); n != 1 {
			t.Errorf("%d denied rows %q, want 1", n, want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action NOT IN ('denied', 'POST /api/v1/projects')"); n != 0 {
		t.Fatalf("a refused request wrote %d other row(s)", n)
	}
	// who was refused what, org-wide, is an admin's to read; a member reads their own refusal naming no app, and no denied row of an app they are not on
	for who, want := range map[*http.Client]int{dana: 2, erin: 1} {
		if _, page, _ := do(t, who, http.MethodGet, base+"/api/v1/audit?action=denied", ""); len(page["events"].([]any)) != want {
			t.Errorf("denied rows read: %s, want %d", lastBody, want)
		}
	}
	// a refused sign-in names someone the provider vouched for: an admin's alone, in the page and its total
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, actor_kind, actor_subject, action, detail) VALUES ('ev_refused', 'person', 'sub-zed', 'sign in refused', '{"flow": "browser", "email": "zed@elsewhere.example", "reason": "not_a_member"}')`); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[*http.Client]float64{dana: 1, erin: 0} {
		if _, page, _ := do(t, who, http.MethodGet, base+"/api/v1/audit?action=sign+in+refused", ""); len(page["events"].([]any)) != int(want) || page["total"] != want || strings.Contains(lastBody, "zed@") != (want == 1) {
			t.Errorf("sign in refused rows read: %s, want %v", lastBody, want)
		}
	}
}
