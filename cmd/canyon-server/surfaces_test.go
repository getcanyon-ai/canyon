package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// frontDoor is the app surface under oidc in front of one fake app that records what reached it:
// claims-portal owned by olive with the role approver, viewed by the group claims-team with the role
// auditor; quote-engine beside it. dana is the admin (withOIDC).
type frontDoor struct {
	pool    *pgxpool.Pool
	ctx     context.Context
	h       http.Handler
	key     *identity.Key
	reached []*http.Request
}

func newFrontDoor(t *testing.T) *frontDoor {
	t.Helper()
	withOIDC(t)
	pool, ctx := migrated(t)
	key, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f := &frontDoor{pool: pool, ctx: ctx, key: key}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached = append(f.reached, r)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	a := &app{pool: pool, auth: authOIDC, oidc: testOIDC, public: "http://localhost:7777", own: "localhost:7777", idKey: key, log: log.New(&strings.Builder{}, "", 0)}
	f.h = a.surfaces(http.NotFoundHandler(), transport)
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal'), ('quote-engine', 'quote-engine');
		INSERT INTO project_access (project_id, kind, grantee, level, subject, roles, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		('claims-portal', 'email', 'olive@meridian.example', 'owner', 'sub-olive', '{approver}', 'person', 'sub-olive', ''),
		('claims-portal', 'group', 'claims-team', 'viewer', '', '{auditor}', 'person', 'sub-olive', ''),
		('quote-engine', 'email', 'nora@meridian.example', 'owner', '', '{}', 'person', 'sub-nora', '');
		INSERT INTO projects (id, name, deleted_at) VALUES ('gone-app', 'gone-app', now());
		INSERT INTO project_access (project_id, kind, grantee, level, subject, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		('gone-app', 'email', 'olive@meridian.example', 'owner', 'sub-olive', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	return f
}

// session is a live Canyon session row, as a sign-in writes one, and its id.
func (f *frontDoor) session(t *testing.T, sub, email, display string, groups ...string) string {
	t.Helper()
	id, _ := token()
	if _, err := f.pool.Exec(f.ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, flow, groups, expires_at) VALUES ($1, 'person', $2, $3, $4, 'browser', $5, now() + interval '1 hour')", id, sub, display, email, append([]string{}, groups...)); err != nil {
		t.Fatal(err)
	}
	return id
}

// signedInto is an exchanged app session of project under session: the app cookie's value.
func (f *frontDoor) signedInto(t *testing.T, session, project string) string {
	t.Helper()
	id, _ := token()
	if _, err := f.pool.Exec(f.ctx, "INSERT INTO app_sessions (id, session_id, project_id, expires_at) VALUES ($1, $2, $3, now() + interval '12 hours')", id, session, project); err != nil {
		t.Fatal(err)
	}
	return id
}

// code is a code /auth/app handed out for project under session, bound to flow, live for in, and the
// id of its app session.
func (f *frontDoor) code(t *testing.T, session, project, flow, in string) (string, string) {
	t.Helper()
	code, _ := token()
	id, _ := token()
	c, fl := sha256.Sum256([]byte(code)), sha256.Sum256([]byte(flow))
	if _, err := f.pool.Exec(f.ctx, "INSERT INTO app_sessions (id, session_id, project_id, code_hash, flow_hash, code_expires_at, expires_at) VALUES ($1, $2, $3, $4, $5, now() + $6::interval, now() + interval '12 hours')", id, session, project, c[:], fl[:], in); err != nil {
		t.Fatal(err)
	}
	return code, id
}

// codeOf is code's code alone.
func (f *frontDoor) codeOf(t *testing.T, session, project, flow, in string) string {
	t.Helper()
	code, _ := f.code(t, session, project, flow, in)
	return code
}

func (f *frontDoor) serve(host, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://placeholder"+target, nil)
	req.Host, req.Header = host, header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// cookieHeader is a Cookie header of name=value pairs, name and value alternating.
func cookieHeader(pairs ...string) http.Header {
	var cs []string
	for pair := range slices.Chunk(pairs, 2) {
		cs = append(cs, strings.Join(pair, "="))
	}
	return http.Header{"Cookie": {strings.Join(cs, "; ")}}
}

const portal = "claims-portal.preview.localhost:7777"

// Without a good app cookie — none, another app's, one whose Canyon session is revoked, or a leaver's —
// the browser gets a new flow cookie on the app's host and goes to GET /auth/app for exactly this app
// with the flow's hash; nothing reaches the app.
func TestAnAppRequestWithoutAGoodAppCookieGoesToSignIn(t *testing.T) {
	f := newFrontDoor(t)
	olive := f.session(t, "sub-olive", "olive@meridian.example", "Olive")
	leaver := f.session(t, "sub-lee", "lee@elsewhere.example", "Lee")
	revoked := f.session(t, "sub-olive", "olive@meridian.example", "Olive")
	if _, err := f.pool.Exec(f.ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1", revoked); err != nil {
		t.Fatal(err)
	}
	deleted := f.signedInto(t, olive, "gone-app")
	_, pending := f.code(t, olive, "claims-portal", "x", "60 seconds")
	for what, h := range map[string]http.Header{
		"no cookie":                   nil,
		"another app's app session":   cookieHeader(appCookie, f.signedInto(t, olive, "quote-engine")),
		"a revoked Canyon session":    cookieHeader(appCookie, f.signedInto(t, revoked, "claims-portal")),
		"a leaver's":                  cookieHeader(appCookie, f.signedInto(t, leaver, "claims-portal")),
		"a code not yet exchanged":    cookieHeader(appCookie, pending),
		"a value that is not a token": cookieHeader(appCookie, "' OR 1=1 --"),
		"a deleted app's":             cookieHeader(appCookie, deleted),
	} {
		host := portal
		if what == "a deleted app's" {
			host = "gone-app.preview.localhost:7777"
		}
		rec := f.serve(host, "/claims/42", h)
		loc, _ := neturl.Parse(rec.Header().Get("Location"))
		resp := http.Response{Header: rec.Header()}
		var flow *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == flowCookie {
				flow = c
			}
		}
		if rec.Code != 302 || loc == nil || loc.Scheme+"://"+loc.Host+loc.Path != "http://localhost:7777/auth/app" || loc.Query().Get("to") != host || flow == nil {
			t.Fatalf("%s: %d %q", what, rec.Code, rec.Header().Get("Location"))
		}
		sum := sha256.Sum256([]byte(flow.Value))
		if loc.Query().Get("flow") != hex.EncodeToString(sum[:]) || len(flow.Value) != 64 || !flow.HttpOnly || flow.SameSite != http.SameSiteLaxMode || flow.MaxAge != 600 || flow.Path != "/" || flow.Domain != "" {
			t.Fatalf("%s: the flow cookie %v against %q", what, flow, loc.Query().Get("flow"))
		}
	}
	if len(f.reached) != 0 {
		t.Fatalf("%d request(s) reached the app", len(f.reached))
	}
}

// The code is spent on its first presentation, whatever happens: it becomes the app cookie only at its
// own app's host, within its minute, beside the flow cookie it was bound to. The callback never
// reaches the app.
func TestTheCallbackSpendsTheCodeOnceAndOnlyForTheBrowserThatAskedForIt(t *testing.T) {
	f := newFrontDoor(t)
	olive := f.session(t, "sub-olive", "olive@meridian.example", "Olive")
	flow := strings.Repeat("f", 64)
	good, row := f.code(t, olive, "claims-portal", flow, "60 seconds")
	rec := f.serve(portal, appCallback+"?code="+good, cookieHeader(flowCookie, flow))
	var app, cleared *http.Cookie
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		switch c.Name {
		case appCookie:
			app = c
		case flowCookie:
			cleared = c
		}
	}
	if rec.Code != 302 || rec.Header().Get("Location") != "/" || app == nil || app.Value != row || !app.HttpOnly || app.SameSite != http.SameSiteLaxMode || app.MaxAge != 43200 || app.Path != "/" || app.Domain != "" || cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("the exchange: %d %q app %v flow %v", rec.Code, rec.Header().Get("Location"), app, cleared)
	}
	stolen := f.codeOf(t, olive, "claims-portal", flow, "60 seconds")
	for _, c := range []struct{ what, code, flow string }{ // in order: the stolen code is presented wrongly first
		{"the same code again", good, flow},
		{"beside another browser's flow", stolen, strings.Repeat("e", 64)},
		{"then beside the right one", stolen, flow},
		{"a code past its minute", f.codeOf(t, olive, "claims-portal", flow, "-1 second"), flow},
		{"another app's code", f.codeOf(t, olive, "quote-engine", flow, "60 seconds"), flow},
		{"no flow cookie", f.codeOf(t, olive, "claims-portal", flow, "60 seconds"), ""},
		{"a code this host never handed", strings.Repeat("0", 64), flow},
	} {
		what := c.what
		h := cookieHeader(flowCookie, c.flow)
		if c.flow == "" {
			h = nil
		}
		if rec := f.serve(portal, appCallback+"?code="+c.code, h); rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != `{"error":"invalid_code","message":"this sign-in did not finish: open the app again"}` || strings.Contains(rec.Header().Get("Set-Cookie"), appCookie+"=") {
			t.Errorf("%s: %d %s %q", what, rec.Code, rec.Body.String(), rec.Header().Get("Set-Cookie"))
		}
	}
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM app_sessions"); n != 1 {
		t.Fatalf("%d app sessions, want the one good exchange: every code presented is spent, and a failed one leaves nothing behind", n)
	}
	if len(f.reached) != 0 {
		t.Fatalf("the callback reached the app %d time(s)", len(f.reached))
	}
}

// An admitted request reaches the app with the identity token — audience this app, the person, the
// union of their grants' roles — and with neither the browser's Authorization nor Canyon's or an
// oauth2-proxy's cookies; the app's own cookies pass.
func TestAnAdmittedRequestReachesTheAppWithTheTokenAndWithoutTheBrowsersCredentials(t *testing.T) {
	f := newFrontDoor(t)
	olive := f.session(t, "sub-olive", "olive@meridian.example", "Olive Park", "claims-team")
	h := cookieHeader(appCookie, f.signedInto(t, olive, "claims-portal"), flowCookie, "x", "_oauth2_proxy", "a", "_oauth2_proxy_csrf", "b", "theme", "dark", "pref", `"a b"`)
	h.Set("Authorization", "Bearer forged")
	h.Set("Proxy-Authorization", "Basic Zm9yZ2Vk")
	if rec := f.serve(portal, "/claims/42?tab=notes", h); rec.Code != http.StatusTeapot || len(f.reached) != 1 {
		t.Fatalf("an owner's request: %d %s", rec.Code, rec.Body.String())
	}
	got := f.reached[0]
	if got.URL.RequestURI() != "/claims/42?tab=notes" || got.Header.Get("Cookie") != `theme=dark; pref="a b"` || len(got.Header.Values("Authorization")) != 1 || got.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("what reached the app: %s cookie %q authorization %q proxy %q", got.URL.RequestURI(), got.Header.Get("Cookie"), got.Header.Values("Authorization"), got.Header.Get("Proxy-Authorization"))
	}
	doc, _ := f.key.JWKS()
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(doc) }))
	defer keys.Close()
	verify := func(audience string) (*oidc.IDToken, error) {
		return oidc.NewVerifier("http://localhost:7777", oidc.NewRemoteKeySet(context.Background(), keys.URL), &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.ES256}}).
			Verify(context.Background(), strings.TrimPrefix(got.Header.Get("Authorization"), "Bearer "))
	}
	tok, err := verify("claims-portal")
	var claims identity.Claims
	if err == nil {
		err = tok.Claims(&claims)
	}
	if err != nil || claims.Subject != "sub-olive" || claims.Email != "olive@meridian.example" || claims.Name != "Olive Park" || claims.Env != "preview" || !reflect.DeepEqual(claims.Roles, []string{"approver", "auditor"}) || claims.Expires-claims.IssuedAt != 300 {
		t.Fatalf("the token: %+v (%v)", claims, err)
	}
	if _, err := verify("quote-engine"); err == nil {
		t.Fatalf("the token verifies for another app")
	}
}

// The level is read on every request: a person whose last grant goes is sent to the no-access screen
// on their next request, a revoked session to sign in, and an admin with no grant is admitted.
func TestTheLevelIsReadOnEveryRequest(t *testing.T) {
	f := newFrontDoor(t)
	vic := f.session(t, "sub-vic", "vic@meridian.example", "Vic", "claims-team")
	cookie := cookieHeader(appCookie, f.signedInto(t, vic, "claims-portal"))
	if rec := f.serve(portal, "/", cookie); rec.Code != http.StatusTeapot {
		t.Fatalf("a viewer by group: %d", rec.Code)
	}
	if _, err := f.pool.Exec(f.ctx, "UPDATE sessions SET groups = '{}' WHERE id = $1", vic); err != nil {
		t.Fatal(err)
	}
	if rec := f.serve(portal, "/", cookie); rec.Code != 302 || rec.Header().Get("Location") != "http://localhost:7777/#/no-access/claims-portal" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("with no grant left: %d %q %q", rec.Code, rec.Header().Get("Location"), rec.Header().Get("Set-Cookie"))
	}
	dana := f.session(t, "sub-dana", "dana@meridian.example", "Dana")
	danaCookie := cookieHeader(appCookie, f.signedInto(t, dana, "claims-portal"))
	if rec := f.serve(portal, "/", danaCookie); rec.Code != http.StatusTeapot {
		t.Fatalf("an admin with no grant: %d", rec.Code)
	}
	if _, err := f.pool.Exec(f.ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1", dana); err != nil {
		t.Fatal(err)
	}
	if rec := f.serve(portal, "/", danaCookie); rec.Code != 302 || !strings.HasPrefix(rec.Header().Get("Location"), "http://localhost:7777/auth/app?") {
		t.Fatalf("the admin's session revoked: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(f.reached) != 2 {
		t.Fatalf("%d request(s) reached the app, want the viewer's and the admin's", len(f.reached))
	}
}

// Every app shares one site, so a sibling app's page can make this browser send its app cookie along
// (SameSite=Lax): a change reaches an app only from its own pages, or from a client that is no
// browser, and a read from anywhere.
func TestAChangeFromAnotherOriginNeverReachesTheApp(t *testing.T) {
	f := newFrontDoor(t)
	cookie := f.signedInto(t, f.session(t, "sub-olive", "olive@meridian.example", "Olive"), "claims-portal")
	for _, c := range []struct {
		method, site, origin string
		want                 int
	}{
		{http.MethodPost, "same-site", "", 403}, {http.MethodDelete, "cross-site", "", 403},
		{http.MethodPost, "", "http://evil.preview.localhost:7777", 403},
		{http.MethodPost, "same-origin", "http://" + portal, http.StatusTeapot}, {http.MethodPost, "", "http://" + portal, http.StatusTeapot},
		{http.MethodPost, "", "", http.StatusTeapot}, {http.MethodGet, "same-site", "", http.StatusTeapot},
	} {
		req := httptest.NewRequest(c.method, "http://placeholder/claims/42", nil)
		req.Host, req.Header = portal, cookieHeader(appCookie, cookie)
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != c.want || c.want == 403 && strings.TrimSpace(rec.Body.String()) != `{"error":"cross_origin","message":"this request did not come from this app"}` {
			t.Errorf("%s from site %q origin %q: %d %s, want %d", c.method, c.site, c.origin, rec.Code, rec.Body.String(), c.want)
		}
	}
	if len(f.reached) != 4 {
		t.Fatalf("%d request(s) reached the app, want the four from its own pages, a client that is no browser, and a read", len(f.reached))
	}
}

// Two browsers presenting one code at once: one exchange, never two.
func TestAConcurrentExchangeSpendsTheCodeOnce(t *testing.T) {
	f := newFrontDoor(t)
	flow := strings.Repeat("f", 64)
	code := f.codeOf(t, f.session(t, "sub-olive", "olive@meridian.example", "Olive"), "claims-portal", flow, "60 seconds")
	codes := make(chan int, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { codes <- f.serve(portal, appCallback+"?code="+code, cookieHeader(flowCookie, flow)).Code })
	}
	wg.Wait()
	close(codes)
	var exchanged int
	for c := range codes {
		if c == 302 {
			exchanged++
		}
	}
	if exchanged != 1 {
		t.Fatalf("%d of 8 concurrent presentations exchanged the code", exchanged)
	}
}

// Under https the app's two cookies take the __Host- prefix and Secure, as the session cookie does, so
// a sibling app cannot plant either on this app's host.
func TestUnderHTTPSTheAppsCookiesAreHostPrefixed(t *testing.T) {
	for _, name := range []string{appCookie, flowCookie} {
		if got, secure := appCookieOf("https://canyon.acme.example", name); got != "__Host-"+name || !secure {
			t.Errorf("%s under https: %s %v", name, got, secure)
		}
		if got, secure := appCookieOf("http://localhost:7777", name); got != name || secure {
			t.Errorf("%s under http: %s %v", name, got, secure)
		}
	}
}
