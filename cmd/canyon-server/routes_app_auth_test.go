package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	appFlow   = strings.Repeat("ab", 32)       // an app's flow cookie
	appFlowOf = sha256.Sum256([]byte(appFlow)) // what the app surface sends as flow
	appTo     = "claims-portal.preview.localhost:7777"
)

func appSignInURL(base, to, flow string) string {
	return base + "/auth/app?" + neturl.Values{"to": {to}, "flow": {flow}}.Encode()
}

// to names exactly one app surface of this host and flow is the hash of a flow cookie, else 400
// invalid_to before anything is read; an app that does not exist is invalid_to too, to a signed-in
// person alone. Every refusal writes nothing.
func TestAnAppSignInRefusesEveryToButThisHostsOwnApp(t *testing.T) {
	pool, ctx, base, olive, _, _, _, _ := teamOf(t)
	flow := hex.EncodeToString(appFlowOf[:])
	for _, c := range []struct{ to, flow string }{
		{"claims-portal.preview.evil.example", flow}, {"claims-portal.preview.localhost:7777.evil.example", flow},
		{"claims-portal.preview.localhost", flow}, {"claims-portal.staging.localhost:7777", flow}, {"Claims-Portal.preview.localhost:7777", flow},
		{"claims-portal.preview.localhost:7777/x", flow}, {"a@claims-portal.preview.localhost:7777", flow}, {"evil.example", flow}, {"", flow},
		{appTo, ""}, {appTo, strings.ToUpper(flow)}, {appTo, flow[:62]}, {appTo, flow + "00"},
		{"nope.preview.localhost:7777", flow},
	} {
		if code, _, _ := do(t, noRedirect(olive), http.MethodGet, appSignInURL(base, c.to, c.flow), ""); code != 400 || lastBody != `{"error":"invalid_to","message":"to is an app of this host, as app.env.host, and flow the sha256 of the app's flow cookie"}` {
			t.Errorf("to=%q flow=%q: %d %s", c.to, c.flow, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM app_sessions"); n != 0 {
		t.Fatalf("%d app sessions after refusals", n)
	}
	if code, _, resp := do(t, noRedirect(client(t)), http.MethodGet, appSignInURL(base, "nope.preview.localhost:7777", flow), ""); code != 302 || resp.Header.Get("Location") != "/auth/start" {
		t.Fatalf("an unknown app to a browser with no session: %d %s; the route says nothing about which apps exist", code, resp.Header.Get("Location"))
	}
}

// A person at viewer or above — by address, by a share with everyone, or an admin with no grant —
// leaves with a code for exactly the app asked for, bound to its flow, good for 60 seconds, its
// session no longer than the Canyon session's; someone with no access goes to the no-access screen
// and gets nothing. No audit row.
func TestAnAppSignInGivesAViewerACodeAndSomeoneWithNoAccessTheNoAccessScreen(t *testing.T) {
	pool, ctx, base, olive, _, vic, nora, dana := teamOf(t)
	flow := hex.EncodeToString(appFlowOf[:])
	trail := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	code, _, resp := do(t, noRedirect(nora), http.MethodGet, appSignInURL(base, appTo, flow), "")
	if code != 302 || resp.Header.Get("Location") != "/#/no-access/claims-portal" || count(t, ctx, pool, "SELECT count(*) FROM app_sessions") != 0 {
		t.Fatalf("no access: %d %q", code, resp.Header.Get("Location"))
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() + CASE actor_subject WHEN 'sub-vic' THEN interval '1 hour' ELSE interval '1 day' END"); err != nil {
		t.Fatal(err)
	}
	callback := regexp.MustCompile(`^http://claims-portal\.preview\.localhost:7777/\.canyon/callback\?code=([0-9a-f]{64})$`)
	for _, who := range []struct {
		name string
		c    *http.Client
		ttl  time.Duration
	}{{"vic, a viewer", vic, time.Hour}, {"olive, the owner", olive, appSessionTTL}, {"dana, an admin with no grant", dana, appSessionTTL}} {
		code, _, resp := do(t, noRedirect(who.c), http.MethodGet, appSignInURL(base, appTo, flow), "")
		m := callback.FindStringSubmatch(resp.Header.Get("Location"))
		if code != 302 || m == nil {
			t.Fatalf("%s: %d %q", who.name, code, resp.Header.Get("Location"))
		}
		sum := sha256.Sum256([]byte(m[1]))
		var project string
		var flowHash []byte
		var codeLeft, left time.Duration
		if err := pool.QueryRow(ctx, "SELECT project_id, flow_hash, code_expires_at - now(), expires_at - now() FROM app_sessions WHERE code_hash = $1", sum[:]).Scan(&project, &flowHash, &codeLeft, &left); err != nil ||
			project != "claims-portal" || hex.EncodeToString(flowHash) != flow || codeLeft > appCodeWindow || codeLeft < appCodeWindow-10*time.Second || left > who.ttl || left < who.ttl-time.Minute {
			t.Fatalf("%s's app session: %s %x code %s session %s (%v)", who.name, project, flowHash, codeLeft, left, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'everyone', '', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	if code, _, resp := do(t, noRedirect(nora), http.MethodGet, appSignInURL(base, appTo, flow), ""); code != 302 || !callback.MatchString(resp.Header.Get("Location")) {
		t.Fatalf("nora, once the app is shared with everyone: %d %q", code, resp.Header.Get("Location"))
	}
	if code, _, _ := do(t, nora, http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 201 { // read-only through everyone, she may ask to change it (#1706)
		t.Fatalf("nora asks for access to an app shared with everyone: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'request access'"); n != trail {
		t.Fatalf("the app sign-in wrote %d audit row(s)", n-trail)
	}
}

// noRedirect is c stopping at the first 302, sharing its cookies.
func noRedirect(c *http.Client) *http.Client {
	return &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// A browser without a session carries the app sign-in through /auth/start in a short-lived cookie
// built as the sign-in cookie is, and the callback lands back on /auth/app with that to and flow —
// never through a query next — and clears it.
func TestAnAppSignInWithoutASessionSignsInFirstAndComesBack(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	flow := hex.EncodeToString(appFlowOf[:])
	c := browser(t)
	resp := fetch(t, c, appSignInURL(base, appTo, flow))
	var toCookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == appToCookie {
			toCookie = ck
		}
	}
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/auth/start" || toCookie == nil || !toCookie.HttpOnly || toCookie.SameSite != http.SameSiteLaxMode || toCookie.MaxAge != 600 || toCookie.Path != "/" || toCookie.Domain != "" {
		t.Fatalf("to sign-in first: %d %q %v", resp.StatusCode, resp.Header.Get("Location"), toCookie)
	}
	callback, _, _, _ := startSignIn(t, c, base, idp)
	resp = fetch(t, c, callback)
	cleared := false
	for _, ck := range resp.Cookies() {
		cleared = cleared || ck.Name == appToCookie && ck.MaxAge < 0
	}
	if want := "/auth/app?" + (neturl.Values{"flow": {flow}, "to": {appTo}}).Encode(); resp.StatusCode != 302 || resp.Header.Get("Location") != want || !cleared {
		t.Fatalf("the callback: %d %q, want %q and the cookie cleared", resp.StatusCode, resp.Header.Get("Location"), want)
	}
}

// Under https the app sign-in's cookie takes the __Host- prefix and Secure, as the sign-in cookie does.
func TestUnderHTTPSTheAppSignInCookieIsHostPrefixed(t *testing.T) {
	if name, secure := appToCookieOf("https://canyon.acme.example"); name != "__Host-canyon_app_to" || !secure {
		t.Fatalf("under https: %s %v", name, secure)
	}
}

// With auth off there is no app sign-in: every visitor is the anonymous admin and reaches the app.
func TestADemoHostRoutesNoAppSignIn(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodGet, appSignInURL(base, appTo, hex.EncodeToString(appFlowOf[:])), ""); code != 404 {
		t.Fatalf("GET /auth/app on a demo host: %d %s", code, lastBody)
	}
}

// GET /auth/app is bounded per Canyon session, never per address: 60 at once, then one a minute. A
// burst on one session is refused past it; another session from the same address, and a browser
// with no session at all, are not counted against it.
func TestAnAppSignInIsBoundedPerSessionNotPerAddress(t *testing.T) {
	_, _, base, olive, carl, _, _, _ := teamOf(t)
	url := appSignInURL(base, appTo, hex.EncodeToString(appFlowOf[:]))
	for i := range appSignInBurst {
		if code, _, _ := do(t, noRedirect(olive), http.MethodGet, url, ""); code != 302 {
			t.Fatalf("olive's sign-in %d: %d %s", i, code, lastBody)
		}
	}
	if code, body, _ := do(t, noRedirect(olive), http.MethodGet, url, ""); code != 429 || body["error"] != "rate_limited" || body["retry_after_s"] != 60.0 {
		t.Fatalf("olive past her session's bound: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"carl, another session": carl, "a browser with no session": client(t)} {
		if code, _, _ := do(t, noRedirect(c), http.MethodGet, url, ""); code != 302 {
			t.Fatalf("%s from the same address: %d %s", who, code, lastBody)
		}
	}
}
