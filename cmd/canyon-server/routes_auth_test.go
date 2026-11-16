package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
)

// withIssuer starts the next test server under CANYON_AUTH=oidc against idp, discovered: the
// address list names dana (no domain list at all, a team without a domain), dana is the admin, and
// the groups claim means admin on platform-admins.
func withIssuer(t *testing.T, idp *testIDP, secret string) {
	t.Helper()
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_CLIENT_SECRET": secret, "CANYON_OIDC_ALLOWED_EMAILS": "dana@meridian.example", "CANYON_ADMINS": "dana@meridian.example", "CANYON_ADMIN_CLAIM": "groups", "CANYON_ADMIN_VALUE": "platform-admins"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	idp.secret = secret
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
}

// browser is a client with a jar that stops at every 302, so each hop is inspected.
func browser(t *testing.T) *http.Client {
	t.Helper()
	c := client(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func fetch(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	_, _, resp := do(t, c, http.MethodGet, url, "")
	return resp
}

// startSignIn walks /auth/start and the issuer's redirect back, as the browser would: the callback
// URL it lands on, the sign-in cookie, and the authorization request the issuer saw.
func startSignIn(t *testing.T, c *http.Client, base string, idp *testIDP) (callback, code string, cookie *http.Cookie, auth *neturl.URL) {
	t.Helper()
	resp := fetch(t, c, base+"/auth/start")
	auth, err := resp.Location()
	if resp.StatusCode != 302 || err != nil || len(resp.Cookies()) != 1 {
		t.Fatalf("/auth/start: %d %v, %d cookie(s)", resp.StatusCode, err, len(resp.Cookies()))
	}
	at := browser(t)
	at.Transport = idp.Client().Transport
	back, err := fetch(t, at, auth.String()).Location()
	if err != nil {
		t.Fatal(err)
	}
	return base + back.RequestURI(), back.Query().Get("code"), resp.Cookies()[0], auth
}

// The reader's proof against the fake issuer: design/AUTH.md §4.1, step by step.
func TestBrowserSignInWalksTheIssuerAndMintsOneSessionAndOneAuditRow(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	idp.bend = map[string]any{"name": "Dana Whitfield", "groups": []any{"platform-admins"}, "hd": "meridian.example"}
	withIssuer(t, idp, "")
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := browser(t)
	callback, code, cookie, auth := startSignIn(t, c, base, idp)
	q := auth.Query()
	if "https://"+auth.Host != idp.URL || auth.Path != "/auth" || q.Get("response_type") != "code" || q.Get("client_id") != "canyon" || q.Get("redirect_uri") != "http://localhost:7777/auth/callback" || q.Get("scope") != "openid profile email" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) != 64 || len(q.Get("nonce")) != 64 {
		t.Fatalf("the redirect to the issuer: %s", auth)
	}
	parts := strings.Split(cookie.Value, ".")
	sum := sha256.Sum256([]byte(parts[0]))
	if cookie.Name != "canyon_signin" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 || cookie.Path != "/" || cookie.Secure || len(parts) != 3 || parts[1] != q.Get("state") || parts[2] != q.Get("nonce") || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Fatalf("one short-lived Lax cookie carrying the verifier the challenge is S256 of, the state and the nonce: %s", cookie)
	}
	resp := fetch(t, c, callback)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
		t.Fatalf("the callback lands on /, never a next parameter: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), lastBody)
	}
	var session string
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case sessionCookie:
			session = ck.Value
			if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != 0 || ck.Path != "/" || ck.Domain != "" {
				t.Fatalf("the session cookie is the demo mint's: %s", ck)
			}
		case signInCookie:
			if ck.MaxAge >= 0 {
				t.Fatalf("the sign-in cookie is single use and cleared: %s", ck)
			}
		}
	}
	if len(session) != 2*tokenBytes {
		t.Fatalf("no session cookie: %v", resp.Cookies())
	}
	var sub, display, issuer, email, flow, hd string
	var claimed bool
	var exp time.Time
	if err := pool.QueryRow(ctx, "SELECT actor_subject, actor_display, issuer, email, flow, claimed_admin, hosted_domain, expires_at FROM sessions WHERE id = $1 AND actor_kind = 'person'", session).Scan(&sub, &display, &issuer, &email, &flow, &claimed, &hd, &exp); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(exp); sub != "u1" || display != "Dana Whitfield" || issuer != idp.URL || email != "dana@meridian.example" || flow != "browser" || !claimed || hd != "meridian.example" || left > browserSession+time.Minute || left < browserSession-time.Minute { // the database's clock sets it, so a moment past 12h is fine
		t.Fatalf("the row: sub=%s display=%q issuer=%s email=%s flow=%s claimed_admin=%v hd=%s expires in %s", sub, display, issuer, email, flow, claimed, hd, left)
	}
	var action, who, how string
	if err := pool.QueryRow(ctx, "SELECT action, actor_subject, concat_ws(' ', via, role <> '', session LIKE 'ses_%', source <> '') FROM audit_events").Scan(&action, &who, &how); err != nil || action != "sign in" || who != "u1" || how != "dashboard t t t" {
		t.Fatalf("one audit row naming the person, via the dashboard with a role, a session digest and an address: %s %s %q %v", action, who, how, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 2 {
		t.Fatalf("one session row and one audit row, got %d", n)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("the cookie is a live session: %d %s", code, lastBody)
	}
	if !strings.Contains(logs.String(), "canyon-server: sign in sub=u1 matched=email dana@meridian.example admin_claim=hit role=admin\n") {
		t.Fatalf("one log line naming subject, matched claim, admin claim and role:\n%s", logs.String())
	}
	for _, secret := range append(parts, code, idp.lastToken()) {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a secret reached the log: %s\n%s", secret, logs.String())
		}
	}
}

func TestBrowserSignInRefusalsRedirectToSignInAndWriteNothingAndLogNoSecret(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "secret")
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	for _, c := range []struct {
		name   string
		bend   map[string]any
		rewire func(callback string) string
		fresh  bool // the callback arrives without the sign-in cookie
		logged string
		row    string // the sign in refused row's reason: a person the provider vouched for and Canyon turned away
	}{
		{"a state the cookie did not issue", nil, func(s string) string { return strings.Replace(s, "state=", "state=x", 1) }, false, "state is not the one this sign-in issued", ""},
		{"the person cancelled at the issuer", nil, func(s string) string { return strings.Replace(s, "code=", "error=access_denied&was=", 1) }, false, "the issuer sent no code: error=access_denied", ""},
		{"no sign-in cookie", nil, nil, true, "no sign-in cookie", ""},
		{"a code the issuer did not issue", nil, func(s string) string { return strings.Replace(s, "code=", "code=x", 1) }, false, "code exchange: ", ""},
		{"a replayed nonce", map[string]any{"nonce": "n-another-sign-in"}, nil, false, "id token: id token nonce is not the one this sign-in issued", ""},
		{"email_verified false for the listed address", map[string]any{"email_verified": false}, nil, false, "email_verified is false", ""},
		{"an address carrying an escape sequence", map[string]any{"email": "dana\x1b[2J@meridian.example"}, nil, false, "the email claim is not an email address", ""},
		{"an address on neither list", map[string]any{"email": "someone@elsewhere.example"}, nil, false, "someone@elsewhere.example is on neither membership list", "not_a_member"},
		{"no sub", map[string]any{"sub": ""}, nil, false, "id token has no sub", ""},
	} {
		idp.mu.Lock()
		idp.bend = c.bend
		idp.mu.Unlock()
		cl := browser(t)
		callback, code, cookie, _ := startSignIn(t, cl, base, idp)
		if c.rewire != nil {
			callback = c.rewire(callback)
		}
		if c.fresh {
			cl = browser(t)
		}
		resp := fetch(t, cl, callback)
		if resp.StatusCode != 302 || resp.Header.Get("Location") != signInRoute { // a browser following a navigation is sent to a screen, never handed a body
			t.Fatalf("%s: %d %s %s", c.name, resp.StatusCode, resp.Header.Get("Location"), lastBody)
		}
		cleared := false
		for _, ck := range resp.Cookies() {
			if ck.Name == sessionCookie {
				t.Fatalf("%s: a session cookie was set", c.name)
			}
			cleared = cleared || (ck.Name == "canyon_signin" && ck.MaxAge < 0)
		}
		if !cleared { // a refusal ends the sign-in it belongs to: the next attempt starts clean
			t.Fatalf("%s: the sign-in cookie was not cleared", c.name)
		}
		if n := count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'sign in refused'"); n != 0 {
			t.Fatalf("%s: a refused sign-in left %d row(s)", c.name, n)
		}
		refused := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'sign in refused' AND detail->>'reason' = '"+c.row+"' AND detail->>'flow' = 'browser'")
		if c.row == "" && refused != 0 || c.row != "" && (refused != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'sign in refused' AND detail->>'email' = 'someone@elsewhere.example' AND actor_kind = 'person'") != 1) {
			t.Fatalf("%s: %d sign in refused rows, want one only for a person turned away", c.name, refused)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM audit_events"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "canyon-server: sign in refused: "+c.logged) {
			t.Fatalf("%s: the reason goes to the log, want %q:\n%s", c.name, c.logged, logs.String())
		}
		for _, secret := range append(strings.Split(cookie.Value, "."), code, idp.lastToken(), "secret") {
			if secret != "" && strings.Contains(logs.String(), secret) {
				t.Fatalf("%s: a secret reached the log: %s\n%s", c.name, secret, logs.String())
			}
		}
	}
	// A refused address is the provider's text: isDomain refuses a control character in it and the log site spells the rest out on one line.
	idp.bend = map[string]any{"email": "mallory@partner.example\ncanyon-server:sign-in-forged"}
	cl := browser(t)
	callback, _, _, _ := startSignIn(t, cl, base, idp)
	if resp := fetch(t, cl, callback); resp.StatusCode != 302 || strings.Contains(logs.String(), "\ncanyon-server:sign-in-forged") || count(t, ctx, pool, "SELECT count(*) FROM sessions") != 0 {
		t.Fatalf("a newline in the refused address: %d, log:\n%s", resp.StatusCode, logs.String())
	}
}

// The callback reads the row after it takes the append lock the preferences write also takes
// (canyonv2-prsf): a write that removes the signer while the callback waits on the lock refuses
// the sign-in. Read before the lock, the row would admit dana and the session would land after it.
func TestBrowserSignInReadsTheRowAfterTheLockSoAWriteThatRemovesTheSignerRefusesIt(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	testOIDC.emails = nil // membership comes from the row, or no row write could drop anybody
	pool, ctx := migrated(t)
	setRow(t, ctx, pool, "allowed_emails = '{dana@meridian.example}'")
	base, _ := start(t, pool, time.Hour)
	c := browser(t)
	callback, _, _, _ := startSignIn(t, c, base, idp)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil { // the lock a.mutation takes: the test holds it now
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	answered := make(chan *http.Response, 1)
	go func() { // not fetch: t.Fatal is not allowed off the test goroutine
		resp, err := c.Get(callback)
		if err != nil {
			resp = &http.Response{StatusCode: -1}
		} else {
			_ = resp.Body.Close()
		}
		answered <- resp
	}()
	// Never a sleep: one too short lets the write below land before a read that precedes the lock,
	// and the buggy callback refuses too. Wait until the callback is queued behind this lock.
	for count(t, ctx, pool, fmt.Sprintf("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND %d = ANY(pg_blocking_pids(pid))", pid)) == 0 {
		if len(answered) > 0 {
			t.Fatal("the callback answered without waiting on the append lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, "UPDATE org_preferences SET allowed_emails = '{}'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	resp := <-answered
	if resp.StatusCode != 302 || resp.Header.Get("Location") != signInRoute {
		t.Fatalf("the row the callback reads no longer admits dana: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'sign in refused'"); n != 0 {
		t.Fatalf("a refused sign-in left %d row(s)", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'sign in refused' AND detail->>'reason' = 'not_a_member'"); n != 1 { // written once the lock it waited on is gone
		t.Fatalf("%d sign in refused rows, want 1", n)
	}
}

// After a sign-out the dashboard asks the issuer to let the person choose an account, since the
// issuer's own session outlives Canyon's; every other sign-in stays silent, and select_account is
// the one value passed on (design/AUTH.md §4.1, step 5).
func TestAuthStartPassesOnlySelectAccountAsThePrompt(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for path, want := range map[string]string{"/auth/start": "", "/auth/start?prompt=select_account": "select_account"} {
		resp := fetch(t, browser(t), base+path)
		auth, err := resp.Location()
		if resp.StatusCode != 302 || err != nil || auth.Query().Get("prompt") != want || auth.Query().Has("prompt") != (want != "") {
			t.Fatalf("%s: %d %v, the issuer is sent %q", path, resp.StatusCode, err, auth)
		}
	}
	for _, prompt := range []string{"login", "none", "select_account%20login", "consent", "", "select_account&prompt=login"} {
		code, _, resp := do(t, browser(t), http.MethodGet, base+"/auth/start?prompt="+prompt, "")
		if code != 400 || lastBody != `{"error":"invalid_prompt","message":"prompt is select_account or absent"}` || len(resp.Cookies()) != 0 {
			t.Fatalf("prompt=%s: %d %s, %d cookie(s)", prompt, code, lastBody, len(resp.Cookies()))
		}
	}
}

func TestAuthStartIs503UntilDiscoveryLandsAndTheCookieFollowsTheSessionCookiesName(t *testing.T) {
	withOIDC(t)
	testProvider = &provider{}
	t.Cleanup(func() { testProvider = nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, browser(t), http.MethodGet, base+"/auth/start", ""); code != 503 || lastBody != `{"error":"not_ready","message":"not ready","waiting_on":["oidc"]}` {
		t.Fatalf("%d %s", code, lastBody)
	}
	if code, _, _ := do(t, browser(t), http.MethodGet, base+"/auth/config", ""); code != 200 || lastBody != `{"auth":"oidc","canyon":true,"cli":"dev","device":true,"org":"Meridian","public":"http://localhost:7777","setup":false}` { // the truth about this host, not PR 2's placeholder
		t.Fatalf("/auth/config under oidc: %d %s", code, lastBody)
	}
	inSetup(t, ctx, pool) // a host in setup says so, so a CLI whose code expired can tell the person why
	if code, _, _ := do(t, browser(t), http.MethodGet, base+"/auth/config", ""); code != 200 || lastBody != `{"auth":"oidc","canyon":true,"cli":"dev","device":true,"org":"Meridian","public":"http://localhost:7777","setup":true}` {
		t.Fatalf("/auth/config in setup: %d %s", code, lastBody)
	}
	if name, secure := signInCookieOf("https://canyon.example"); name != "__Host-canyon_signin" || !secure {
		t.Fatalf("under https the sign-in cookie takes the __Host- prefix and Secure like the session cookie: %s %v", name, secure)
	}
	if name, secure := signInCookieOf("http://localhost:7777"); name != "canyon_signin" || secure {
		t.Fatalf("under http neither: %s %v", name, secure)
	}
}
