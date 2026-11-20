package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// oauthHost is a host with sign-in on (dana its admin): gina, a member, signed in in a browser, and the server's pool.
func oauthHost(t *testing.T) (base string, pool *pgxpool.Pool, ctx context.Context, gina *http.Client) {
	t.Helper()
	withOIDC(t)
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	return base, pool, ctx, signedIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example", false)
}

// staying is c without following redirects: the test reads where each one goes.
func staying(c *http.Client) *http.Client {
	return &http.Client{Jar: c.Jar, Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// pkce is a verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 43) + "-canyon-test-verifier"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// register registers a public client sending people back to redirect, and answers its id.
func register(t *testing.T, base, redirect string) string {
	t.Helper()
	code, body, _ := do(t, client(t), http.MethodPost, base+"/oauth/register", `{"client_name":"Claude Code","redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none"}`)
	if code != http.StatusCreated || !strings.HasPrefix(body["client_id"].(string), "dcr_") {
		t.Fatalf("register: %d %s", code, lastBody)
	}
	return body["client_id"].(string)
}

// authorize begins an authorization for client as c's person and answers where Canyon sent the browser.
func authorize(t *testing.T, base string, c *http.Client, params neturl.Values) *neturl.URL {
	t.Helper()
	resp, err := staying(c).Get(base + "/oauth/authorize?" + params.Encode()) //nolint:noctx // the client's timeout bounds it
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	u, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// requestOf is the consent page's request id in where authorize sent the browser.
func requestOf(t *testing.T, at *neturl.URL) string {
	t.Helper()
	q, err := neturl.ParseQuery(strings.TrimPrefix(at.Fragment, "/oauth?"))
	if err != nil || at.Path != "/" || !strings.HasPrefix(at.Fragment, "/oauth?") || q.Get("request") == "" {
		t.Fatalf("authorize sent the browser to %s, want the consent page", at)
	}
	return q.Get("request")
}

// fromPage is the header the dashboard's own page sends with a consent POST.
var fromPage = map[string]string{"Sec-Fetch-Site": "same-origin"}

func tokenCall(t *testing.T, base string, form neturl.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(base+"/oauth/token", form) //nolint:noctx,gosec // test: the server under test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	lastBody = string(raw)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// The whole flow (design/AUTH.md §4.7): a registered client sends gina's browser to authorize, the consent page reads
// who asks and as whom, she allows it, and the code it carries back redeems once, with its verifier, for a session of
// the client's own, flow oauth, bound to the client and to /mcp, which no cookie route takes. Its refresh token
// rotates; the session it replaced ends, and so does the refresh once an admin revokes her sessions.
func TestOAuthAClientSignsInThroughConsent(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	const redirect = "http://127.0.0.1:33418/callback"
	id := register(t, base, redirect)
	verifier, challenge := pkce()
	asked := neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:41999/callback"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}, "scope": {"read act"}, "state": {"st-1"}}
	req := requestOf(t, authorize(t, base, gina, asked)) // a loopback redirect on another port: a native client picks its port at each sign-in
	if code, body, _ := do(t, gina, http.MethodGet, base+"/api/v1/oauth/requests/"+req, ""); code != 200 ||
		!marshalsTo(body["client"], `{"host":"127.0.0.1","name":"Claude Code","verified":false}`) || body["redirect_host"] != "127.0.0.1" ||
		!marshalsTo(body["scopes"], `["read","act"]`) || !marshalsTo(body["account"], `{"display":"sub-gina","email":"gina@meridian.example"}`) || body["host"] != "localhost:7777" {
		t.Fatalf("the consent read: %d %s", code, lastBody)
	}
	code, body, _ := doWith(t, gina, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/approve", "", fromPage)
	back, err := neturl.Parse(body["redirect"].(string))
	if code != 200 || err != nil || back.Host != "127.0.0.1:41999" || back.Query().Get("state") != "st-1" || back.Query().Get("iss") != "http://localhost:7777" || back.Query().Get("code") == "" {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	redeem := neturl.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:41999/callback"},
		"code_verifier": {verifier}, "resource": {"http://localhost:7777/mcp"}}
	status, tok := tokenCall(t, base, redeem)
	if status != 200 || tok["token_type"] != "Bearer" || tok["scope"] != "read act" || tok["access_token"] == nil || tok["refresh_token"] == nil {
		t.Fatalf("the token: %d %s", status, lastBody)
	}
	access := tok["access_token"].(string)
	hash := func(v string) []byte { sum := sha256.Sum256([]byte(v)); return sum[:] }
	var row, flow, clientID, clientName, resource string
	var scopes []string
	var hours, days float64
	if err := pool.QueryRow(ctx, `SELECT id, flow, oauth_client, oauth_client_name, resource, scope, extract(epoch FROM access_expires_at - now()) / 3600,
		extract(epoch FROM expires_at - now()) / 86400 FROM sessions WHERE access_hash = $1`, hash(access)).
		Scan(&row, &flow, &clientID, &clientName, &resource, &scopes, &hours, &days); err != nil || flow != "oauth" || clientID != id || clientName != "Claude Code" ||
		resource != "http://localhost:7777/mcp" || strings.Join(scopes, " ") != "read act" || hours < 0.99 || hours > 1 || days < 29.9 || days > 30 || row == access {
		t.Fatalf("the session: %s %s %s %s %v, access %.2f hours, the session %.2f days, %v", flow, clientID, clientName, resource, scopes, hours, days, err)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE (action = 'connect agent' AND detail = '{"client": "Claude Code", "client_host": "127.0.0.1", "scopes": ["read", "act"]}')
		OR (action = 'sign in' AND detail = '{"flow": "oauth", "client": "Claude Code"}' AND via = 'mcp')`); n != 2 {
		t.Errorf("%d audit rows for the consent and the sign-in, want 2", n)
	}
	if status, _ := tokenCall(t, base, redeem); status != 400 || !strings.Contains(lastBody, `"invalid_grant"`) {
		t.Errorf("the code redeemed twice: %d %s", status, lastBody)
	}
	asCookie := client(t)
	u, _ := neturl.Parse(base)
	asCookie.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: row}}) //nolint:gosec // test: the row just made
	if code, _, _ := do(t, asCookie, http.MethodGet, base+"/api/v1/fleet", ""); code != http.StatusUnauthorized {
		t.Errorf("the OAuth session's row as a cookie: %d, want 401: it is bound to /mcp", code)
	}
	first := tok["refresh_token"].(string)
	status, renewed := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {first}, "client_id": {id}})
	if status != 200 || renewed["access_token"] == access || renewed["refresh_token"] == first || renewed["expires_in"] != 3600.0 {
		t.Fatalf("the refresh: %d %s", status, lastBody)
	}
	var same int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE id = $1 AND access_hash = $2 AND refresh_hash = $3 AND revoked_at IS NULL",
		row, hash(renewed["access_token"].(string)), hash(renewed["refresh_token"].(string))).Scan(&same); err != nil || same != 1 || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'oauth'") != 1 {
		t.Errorf("the refresh rotated onto another row, or none: %v", err)
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {renewed["refresh_token"].(string)}, "client_id": {"dcr_" + strings.Repeat("0", 32)}}); status != 400 {
		t.Errorf("a refresh by another client: %d %s", status, lastBody)
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {first}, "client_id": {id}}); status != 400 || !strings.Contains(lastBody, "already used") {
		t.Errorf("the rotated refresh token used again: %d %s", status, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM sessions WHERE id = '`+row+`' AND revoked_at IS NOT NULL`) +
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'disconnect agent' AND actor_kind = 'canyon' AND detail = '{"reason": "refresh token reused", "subject": "sub-gina"}'`); n != 2 {
		t.Errorf("a reused refresh token left the session live, or wrote no row")
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {renewed["refresh_token"].(string)}, "client_id": {id}}); status != 400 {
		t.Errorf("the current refresh token of a session reuse ended: %d %s", status, lastBody)
	}
}

// signedInOver is a whole consent for client, redeemed: its access and refresh tokens.
func signedInOver(t *testing.T, base string, c *http.Client, client, redirect string) (access, refresh string) {
	t.Helper()
	verifier, challenge := pkce()
	req := requestOf(t, authorize(t, base, c, neturl.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}}))
	_, body, _ := doWith(t, c, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/approve", "", fromPage)
	back, _ := neturl.Parse(body["redirect"].(string))
	status, tok := tokenCall(t, base, neturl.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
	if status != 200 {
		t.Fatalf("the token: %d %s", status, lastBody)
	}
	return tok["access_token"].(string), tok["refresh_token"].(string)
}

// Revoking a session ends its refresh token with it: an admin's revoke-all of a person, and a person disconnecting
// one agent from the list, by its digest, in a browser. A CLI session can't disconnect one, and nobody but an admin
// ends another person's.
func TestOAuthRevokingASessionEndsItsTokens(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	id := register(t, base, redirect)
	_, refresh := signedInOver(t, base, gina, id, redirect)
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, admin, http.MethodDelete, base+"/api/v1/sessions?subject=gina@meridian.example", ""); code != http.StatusNoContent {
		t.Fatalf("an admin revoking gina's sessions: %d %s", code, lastBody)
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {id}}); status != 400 {
		t.Errorf("a revoked session's refresh: %d %s", status, lastBody)
	}
	gina = signedIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example", false)
	_, refresh = signedInOver(t, base, gina, id, redirect)
	var digest string
	if err := pool.QueryRow(ctx, "SELECT 'ses_' || left(encode(sha256(convert_to(id, 'UTF8')), 'hex'), 16) FROM sessions WHERE flow = 'oauth' AND revoked_at IS NULL").Scan(&digest); err != nil {
		t.Fatal(err)
	}
	ana := signedIn(t, ctx, pool, base, "sub-ana", "ana@meridian.example", false)
	if code, _, _ := do(t, ana, http.MethodDelete, base+"/api/v1/sessions/"+digest, ""); code != http.StatusNotFound {
		t.Errorf("another member disconnecting gina's agent: %d", code)
	}
	if code, body, _ := do(t, cliIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example"), http.MethodDelete, base+"/api/v1/sessions/"+digest, ""); code != http.StatusForbidden || body["error"] != "dashboard_required" {
		t.Errorf("a CLI session disconnecting it: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, gina, http.MethodDelete, base+"/api/v1/sessions/"+digest, ""); code != http.StatusNoContent {
		t.Fatalf("gina disconnecting her agent: %d %s", code, lastBody)
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {id}}); status != 400 {
		t.Errorf("a disconnected agent's refresh: %d %s", status, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'disconnect agent' AND actor_subject = 'sub-gina' AND detail->>'client' = 'Claude Code' AND NOT detail ? 'subject'`); n != 1 {
		t.Errorf("%d rows for gina's disconnect, want 1, with no subject: it was her own", n)
	}
}

// A person no longer on the membership lists gets no new token: a code they allowed before, and a refresh, are
// refused, and the refresh ends the session.
func TestOAuthAPersonOffTheListsGetsNoToken(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	id := register(t, base, redirect)
	_, refresh := signedInOver(t, base, gina, id, redirect)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email = 'gina@elsewhere.example' WHERE flow = 'oauth'"); err != nil { // as the lists changing under her would leave her
		t.Fatal(err)
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {id}}); status != 400 || !strings.Contains(lastBody, "no longer sign in") {
		t.Errorf("a refresh off the lists: %d %s", status, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'oauth' AND revoked_at IS NULL"); n != 0 {
		t.Errorf("the session of a person off the lists is still live")
	}
}

// A code redeems only with what it was issued for: the client, the redirect, the verifier and the resource. A
// mismatch spends it all the same, so a guessed verifier gets one try.
func TestOAuthACodeRedeemsOnlyForWhatItWasIssued(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	id := register(t, base, redirect)
	verifier, challenge := pkce()
	issue := func() string {
		req := requestOf(t, authorize(t, base, gina, neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirect}, "code_challenge": {challenge},
			"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}}))
		_, body, _ := doWith(t, gina, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/approve", "", fromPage)
		back, _ := neturl.Parse(body["redirect"].(string))
		return back.Query().Get("code")
	}
	for name, change := range map[string]func(neturl.Values){
		"another client":   func(f neturl.Values) { f.Set("client_id", "dcr_"+strings.Repeat("0", 32)) },
		"another redirect": func(f neturl.Values) { f.Set("redirect_uri", "https://claude.ai/other") },
		"another verifier": func(f neturl.Values) { f.Set("code_verifier", strings.Repeat("x", 43)) },
		"no verifier":      func(f neturl.Values) { f.Del("code_verifier") },
		"another resource": func(f neturl.Values) { f.Set("resource", "http://localhost:7777/api") },
	} {
		form := neturl.Values{"grant_type": {"authorization_code"}, "code": {issue()}, "client_id": {id}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
		change(form)
		if status, _ := tokenCall(t, base, form); status != 400 || !strings.Contains(lastBody, `"invalid_grant"`) {
			t.Errorf("%s: %d %s", name, status, lastBody)
		}
		form.Set("client_id", id) // as it was: the code is spent

		form.Set("redirect_uri", redirect)
		form.Set("code_verifier", verifier)
		form.Del("resource")
		if status, _ := tokenCall(t, base, form); status != 400 {
			t.Errorf("%s, then the right one: %d %s, want the code spent", name, status, lastBody)
		}
	}
	if status, _ := tokenCall(t, base, neturl.Values{"grant_type": {"client_credentials"}, "client_id": {id}}); status != 400 || !strings.Contains(lastBody, `"unsupported_grant_type"`) {
		t.Errorf("client_credentials: %d %s", status, lastBody)
	}
}

// What Canyon can't send back to is its own page, never a redirect: an unknown client, a client document that names
// another, and a redirect the client didn't register. Everything else goes back to the client as OAuth says.
func TestOAuthAuthorizeRefusesWhereItCanSendBack(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	id := register(t, base, redirect)
	_, challenge := pkce()
	good := func() neturl.Values {
		return neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirect}, "code_challenge": {challenge},
			"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}, "state": {"s"}}
	}
	for name, c := range map[string]struct {
		change func(neturl.Values)
		page   string // the refusal page's reason, or "" for the client's redirect
		error  string
	}{
		"an unknown client":           {func(q neturl.Values) { q.Set("client_id", "dcr_"+strings.Repeat("a", 32)) }, "client", ""},
		"an unregistered redirect":    {func(q neturl.Values) { q.Set("redirect_uri", "https://evil.example/cb") }, "redirect", ""},
		"no PKCE":                     {func(q neturl.Values) { q.Del("code_challenge") }, "", "invalid_request"},
		"plain PKCE":                  {func(q neturl.Values) { q.Set("code_challenge_method", "plain") }, "", "invalid_request"},
		"another resource":            {func(q neturl.Values) { q.Set("resource", "http://localhost:7777/api") }, "", "invalid_target"},
		"no resource":                 {func(q neturl.Values) { q.Del("resource") }, "", "invalid_target"},
		"a scope Canyon doesn't have": {func(q neturl.Values) { q.Set("scope", "read admin") }, "", "invalid_scope"},
		"a token response":            {func(q neturl.Values) { q.Set("response_type", "token") }, "", "unsupported_response_type"},
	} {
		q := good()
		c.change(q)
		at := authorize(t, base, gina, q)
		if c.page != "" {
			if at.Path != "/" || at.Fragment != "/oauth?invalid="+c.page || at.Query().Get("code") != "" {
				t.Errorf("%s: sent to %s, want Canyon's page for %s", name, at, c.page)
			}
			continue
		}
		if at.Host != "claude.ai" || at.Query().Get("error") != c.error || at.Query().Get("state") != "s" || at.Query().Get("iss") != "http://localhost:7777" {
			t.Errorf("%s: sent to %s, want the client's redirect with %s", name, at, c.error)
		}
	}
}

// Registration takes public clients alone, each redirect one Canyon sends a browser to.
func TestOAuthRegistersPublicClientsAlone(t *testing.T) {
	base, _, _, _ := oauthHost(t)
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"client_name":"Claude Code","redirect_uris":["http://localhost:33418/callback","http://[::1]:33418/callback"]}`, 201},
		{`{"redirect_uris":["https://vscode.dev/redirect"]}`, 201},                                                           // no name: shown by its redirect's host
		{`{"client_name":"Cursor","redirect_uris":["cursor://anysphere.cursor-retrieval/oauth/user-canyon/callback"]}`, 201}, // cursor, by name
		{`{"client_name":"x","redirect_uris":["myapp://callback"]}`, 400},                                                    // a scheme no name allows
		{`{"client_name":"x","redirect_uris":["cursor:callback"]}`, 400},                                                     // cursor with no host
		{`{"client_name":"x","redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, 400},
		{`{"client_name":"x","redirect_uris":["javascript:alert(1)"]}`, 400},
		{`{"client_name":"x","redirect_uris":["http://canyon.meridian.example/cb"]}`, 400}, // plain http off loopback
		{`{"client_name":"x","redirect_uris":["https://a.example/cb#frag"]}`, 400},
		{`{"client_name":"x","redirect_uris":[]}`, 400},
		{`{"client_name":"x","redirect_uris":["https://a.example/cb"],"grant_types":["client_credentials"]}`, 400},
		{"{\"client_name\":\"a\u202ex\",\"redirect_uris\":[\"https://a.example/cb\"]}", 400},
	} {
		if code, _, _ := do(t, client(t), http.MethodPost, base+"/oauth/register", c.body); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.body, code, lastBody, c.code)
		}
	}
}

// Cursor receives its sign-in on cursor://, a scheme Canyon allows by name: it registers, authorizes and redeems as any
// client does, matched exactly, and the consent page names cursor as where it sends the person back. The redirect is a
// cursor:// shape, not one read from Cursor's documentation.
func TestOAuthCursorSignsInOnItsOwnScheme(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "cursor://anysphere.cursor-retrieval/oauth/user-canyon/callback"
	id := register(t, base, redirect)
	_, challenge := pkce()
	q := neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {redirect}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}}
	req := requestOf(t, authorize(t, base, gina, q))
	if code, body, _ := do(t, gina, http.MethodGet, base+"/api/v1/oauth/requests/"+req, ""); code != 200 || body["redirect_host"] != "cursor" {
		t.Errorf("the consent read: %d %s, want cursor as where it goes back", code, lastBody)
	}
	_, body, _ := doWith(t, gina, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/approve", "", fromPage)
	if back, _ := body["redirect"].(string); !strings.HasPrefix(back, redirect+"?") || !strings.Contains(back, "code=") {
		t.Errorf("approve sent the browser to %q", back)
	}
	q.Set("redirect_uri", "cursor://anysphere.cursor-retrieval/other")
	if at := authorize(t, base, gina, q); at.Fragment != "/oauth?invalid=redirect" {
		t.Errorf("another cursor:// redirect than the one registered: sent to %s", at)
	}
	signedInOver(t, base, gina, id, redirect)
}

// A client that names itself by a metadata document at its own https URL is fetched there: shown by that URL's host
// and as checked, its redirects the document's. A document naming another client is no client.
func TestOAuthAClientDocumentNamesItsClient(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	var doc string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, doc) }))
	defer srv.Close()
	appOf(t, base).clientDocuments = srv.Client()
	id := srv.URL + "/oauth/client.json"
	doc = `{"client_id":"` + id + `","client_name":"VS Code","redirect_uris":["http://127.0.0.1:33418/"]}`
	_, challenge := pkce()
	q := neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:33418/"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}}
	req := requestOf(t, authorize(t, base, gina, q))
	if code, body, _ := do(t, gina, http.MethodGet, base+"/api/v1/oauth/requests/"+req, ""); code != 200 || !marshalsTo(body["client"], `{"host":"127.0.0.1","name":"VS Code","verified":true}`) {
		t.Errorf("the consent read: %d %s", code, lastBody)
	}
	doc = `{"client_id":"https://other.example/client.json","client_name":"VS Code","redirect_uris":["http://127.0.0.1:33418/"]}`
	if at := authorize(t, base, gina, q); at.Fragment != "/oauth?invalid=client" {
		t.Errorf("a document naming another client: sent to %s", at)
	}
}

// A client document is fetched from a public address alone, whatever its name resolves to: the org's own network,
// loopback and the cloud's metadata service among them, is never reached for one.
func TestOAuthDocumentsComeFromPublicAddressesAlone(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer srv.Close()
	c := clientDocumentClient()
	c.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	if _, err := fetchClientDocument(t.Context(), c, srv.URL+"/client.json"); err == nil {
		t.Error("a document on loopback was fetched")
	}
	for addr, public := range map[string]bool{"8.8.8.8": true, "2606:4700::1111": true, "10.0.0.1": false, "172.16.4.2": false, "192.168.1.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "127.0.0.1": false, "::1": false, "fd00::1": false, "fe80::1": false, "0.0.0.0": false, "::ffff:10.0.0.1": false, "224.0.0.1": false,
		"ff02::1": false, "::": false, "64:ff9b::a00:1": false, "64:ff9b::808:808": false, "64:ff9b:1::1": false} {
		if got := publicAddress(netip.MustParseAddr(addr)); got != public {
			t.Errorf("%s: public %v, want %v", addr, got, public)
		}
	}
}

// Consent is a person's, in a browser (design/PERMISSIONS.md, MCP): a CLI session can't read a request or allow it,
// since the session it hands over would let a stolen CLI token mint itself more.
func TestOAuthConsentIsABrowsers(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	id := register(t, base, "https://claude.ai/cb")
	_, challenge := pkce()
	req := requestOf(t, authorize(t, base, gina, neturl.Values{"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"https://claude.ai/cb"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {"http://localhost:7777/mcp"}}))
	cli := cliIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example")
	for _, path := range []string{"", "/approve", "/deny"} {
		method := http.MethodPost
		if path == "" {
			method = http.MethodGet
		}
		if code, body, _ := do(t, cli, method, base+"/api/v1/oauth/requests/"+req+path, ""); code != http.StatusForbidden || body["error"] != "dashboard_required" {
			t.Errorf("a CLI session, %s %s: %d %s", method, path, code, lastBody)
		}
	}
	for name, hdr := range map[string]map[string]string{"no origin": nil, "another site": {"Sec-Fetch-Site": "cross-site"}, "another origin": {"Origin": "https://evil.example"}} {
		if code, body, _ := doWith(t, gina, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/approve", "", hdr); code != http.StatusForbidden || body["error"] != "cross_origin" {
			t.Errorf("an approve with %s: %d %s", name, code, lastBody)
		}
	}
	if _, _, resp := do(t, gina, http.MethodGet, base+"/api/v1/oauth/requests/"+req, ""); resp.Header.Get("Content-Security-Policy") != "frame-ancestors 'none'" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("the consent read may be framed: %v", resp.Header)
	}
	code, body, _ := doWith(t, gina, http.MethodPost, base+"/api/v1/oauth/requests/"+req+"/deny", "", fromPage)
	if back, err := neturl.Parse(body["redirect"].(string)); code != 200 || err != nil || back.Query().Get("error") != "access_denied" {
		t.Errorf("deny: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, gina, http.MethodGet, base+"/api/v1/oauth/requests/"+req, ""); code != http.StatusNotFound {
		t.Errorf("a denied request read again: %d", code)
	}
}

// The authorization server is served only with sign-in on, and on an https public URL or a loopback one: with
// sign-in off, or on plain http elsewhere, every route of it is the catch-all's 404 (design/specs/mcp.md §2.2).
func TestOAuthIsServedOnlyWithSignInOnOverTLS(t *testing.T) {
	served := func(base string) (int, map[string]any) {
		t.Helper()
		code, body, _ := do(t, client(t), http.MethodGet, base+"/.well-known/oauth-authorization-server", "")
		return code, body
	}
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour) // sign-in off
	if code, _ := served(base); code != http.StatusNotFound {
		t.Errorf("sign-in off: %d", code)
	}
	base, _, _, _ = oauthHost(t) // loopback
	if code, body := served(base); code != 200 || body["issuer"] != "http://localhost:7777" || !marshalsTo(body["code_challenge_methods_supported"], `["S256"]`) ||
		body["client_id_metadata_document_supported"] != true || !marshalsTo(body["scopes_supported"], `["read","act"]`) {
		t.Errorf("loopback: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, client(t), http.MethodGet, base+"/.well-known/oauth-protected-resource/mcp", ""); code != 200 || body["resource"] != "http://localhost:7777/mcp" ||
		!marshalsTo(body["authorization_servers"], `["http://localhost:7777"]`) {
		t.Errorf("the resource's metadata: %d %s", code, lastBody)
	}
	onKubernetes(t) // a plain-http public URL off loopback
	base, _, _, _ = oauthHost(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp", "/oauth/authorize"} {
		if code, _, _ := do(t, client(t), http.MethodGet, base+path, ""); code != http.StatusNotFound {
			t.Errorf("plain http, %s: %d", path, code)
		}
	}
}

// The sessions list is a person's own live sessions, each by its digest and how it signed in, an OAuth one by its
// client's name, the reader's own marked; an admin's is everyone's, each with whose it is.
func TestTheSessionsListIsYoursAndAnAdminsAll(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	admin := signedIn(t, ctx, pool, base, "sub-ana", "ana@meridian.example", true)
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, flow, expires_at, oauth_client, oauth_client_name, resource, scope,
		access_hash, access_expires_at, refresh_hash)
		VALUES ('`+strings.Repeat("ab", 32)+`', 'person', 'sub-gina', 'sub-gina', 'gina@meridian.example', 'oauth', now() + interval '1 day', 'dcr_x', 'Claude Code', 'http://localhost:7777/mcp', '{read}',
		'\x01', now() + interval '1 hour', '\x02')`); err != nil {
		t.Fatal(err)
	}
	var mine struct {
		Sessions []listedSession `json:"sessions"`
	}
	if code, _, _ := do(t, gina, http.MethodGet, base+"/api/v1/sessions", ""); code != 200 || json.Unmarshal([]byte(lastBody), &mine) != nil || len(mine.Sessions) != 2 {
		t.Fatalf("gina's sessions: %d %s", code, lastBody)
	}
	if s := mine.Sessions[0]; s.Flow != "oauth" || s.Client == nil || *s.Client != "Claude Code" || s.Current || s.Person != nil || !strings.HasPrefix(s.ID, "ses_") {
		t.Errorf("gina's OAuth session: %+v", s)
	}
	if s := mine.Sessions[1]; s.Flow != "browser" || !s.Current {
		t.Errorf("gina's own browser session: %+v", s)
	}
	var all struct {
		Sessions []listedSession `json:"sessions"`
	}
	if code, _, _ := do(t, admin, http.MethodGet, base+"/api/v1/sessions", ""); code != 200 || json.Unmarshal([]byte(lastBody), &all) != nil || len(all.Sessions) != 3 {
		t.Fatalf("an admin's sessions: %d %s", code, lastBody)
	}
	for _, s := range all.Sessions {
		if s.Person == nil || s.Person.Email == "" {
			t.Errorf("an admin's row names no person: %+v", s)
		}
	}
}

// marshalsTo is whether v marshals to want.
func marshalsTo(v any, want string) bool {
	raw, err := json.Marshal(v)
	return err == nil && string(raw) == want
}
