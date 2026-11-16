package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func TestAuthModeIsOneOfThreeAndZeroIsOff(t *testing.T) {
	for in, want := range map[string]authMode{"oidc": authOIDC, "": authOIDC, "off": authOff, "demo": authDemo} { // unset is oidc: an unconfigured host does not start open
		if got, err := authModeOf(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := authModeOf("on"); err == nil || err.Error() != "CANYON_AUTH=on is not a mode: oidc, off, demo" {
		t.Errorf("a wrong value is refused by name: %v", err)
	}
	if (app{}).auth != authOff || authOff.String() != "off" { // every test builds an app directly: the value that is safe to forget
		t.Fatal("the zero value must be off")
	}
}

// The demo's mode was called off-and-exposed until #2416. A compose or values file that still names
// it is refused at boot, fail closed, by an error that names the value to set instead.
func TestAuthRefusesTheDemoModesOldNameNamingDemo(t *testing.T) {
	if _, err := authModeOf("off-and-exposed"); err == nil || err.Error() != "CANYON_AUTH=off-and-exposed is now called demo: set CANYON_AUTH=demo" {
		t.Errorf("the old name is refused, naming demo: %v", err)
	}
}

func TestOffIsLoopbackAtBothEndsDemoServesLoopbackAndOIDCServesHTTPS(t *testing.T) {
	for _, c := range []struct {
		mode      authMode
		addr, url string
		ok        bool
	}{
		{authOff, "127.0.0.1:7777", "http://localhost:7777", true}, {authOff, "[::1]:7777", "http://127.0.0.1:7777", true},
		{authOff, ":7777", "http://localhost:7777", false}, {authOff, "0.0.0.0:7777", "http://localhost:7777", false},
		{authOff, "127.0.0.1:7777", "https://canyon.example.com", false},                                   // bound to loopback behind a public proxy: still exposed
		{authDemo, ":7777", "http://localhost:7777", true}, {authDemo, ":7777", "http://[::1]:7777", true}, // the container's network, the laptop's loopback
		{authDemo, ":7777", "https://canyon.example.com", false}, {authDemo, ":7777", "http://10.0.0.5:7777", false}, // every caller an admin, at an address other people reach
		{authOIDC, ":7777", "https://canyon.example.com", true}, {authOIDC, ":7777", "http://localhost:7777", true}, // a loopback http address never crosses a network
		{authOIDC, ":7777", "http://canyon.example.com", false}, {authOIDC, ":7777", "http://10.0.0.5:7777", false}, // the session cookie, in the clear
		{authDemo, ":7777", "http://localhost.evil.com:7777", false}, {authOff, "127.0.0.1:7777", "http://localhost.evil.com", false}, // a lookalike is not loopback
		{authOIDC, ":7777", "http://localhost.evil.com", false},
	} {
		public, err := publicURLOf(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseExposed(c.mode, c.addr, public); (err == nil) != c.ok {
			t.Errorf("%s %s %s: %v", c.mode, c.addr, c.url, err)
		}
	}
}

// A refusal names the setting and what to set, as every boot refusal does.
func TestAPublicDemoAndAnHTTPOIDCAreRefusedByName(t *testing.T) {
	for _, c := range []struct {
		mode authMode
		url  string
		want string
	}{
		{authDemo, "https://canyon.example.com", "CANYON_AUTH=demo serves loopback only, and CANYON_PUBLIC_URL names canyon.example.com: set CANYON_AUTH=oidc for a Canyon other people reach"},
		{authOIDC, "http://canyon.example.com:8080", "CANYON_AUTH=oidc needs an https CANYON_PUBLIC_URL, and it is http://canyon.example.com:8080: set the https address people open Canyon at"},
	} {
		public, err := publicURLOf(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseExposed(c.mode, ":7777", public); err == nil || err.Error() != c.want {
			t.Errorf("%s %s: %v", c.mode, c.url, err)
		}
	}
}

// Unset, CANYON_PUBLIC_URL is this machine's for off and demo, and refused under oidc: a Canyon that
// signs people in at localhost boots, and nobody can sign in to it.
func TestOIDCRefusesAnUnsetPublicURLAndTheOthersServeLocalhost(t *testing.T) {
	if _, err := publicURLFor(authOIDC, ""); err == nil || err.Error() != "CANYON_AUTH=oidc needs CANYON_PUBLIC_URL: set it to the https address people open Canyon at, such as https://canyon.example.com" {
		t.Errorf("oidc, unset: %v", err)
	}
	for _, mode := range []authMode{authOff, authDemo} {
		if u, err := publicURLFor(mode, ""); err != nil || u.String() != "http://localhost:7777" {
			t.Errorf("%s, unset: %v %v", mode, u, err)
		}
	}
	if u, err := publicURLFor(authOIDC, "https://canyon.example.com/"); err != nil || u.String() != "https://canyon.example.com" {
		t.Errorf("oidc, set: %v %v", u, err)
	}
}

// A CANYON_PUBLIC_URL that does not parse is refused by the variable's name alone: the value can carry a
// credential, and the refusal is a log line.
func TestAPublicURLThatDoesNotParseIsRefusedWithoutItsValue(t *testing.T) {
	for _, mode := range []authMode{authOIDC, authOff, authDemo} {
		for _, v := range []string{"https://dana:hunter2@[canyon.example.com", "http://dana:hunter2@canyon.example.com:77x77", "https://dana:hunter2@canyon.example.com"} {
			_, err := publicURLFor(mode, v)
			if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "dana") || !strings.HasPrefix(err.Error(), "CANYON_PUBLIC_URL ") {
				t.Errorf("%s %q: %v", mode, v, err)
			}
		}
	}
}

// With sign-in off every caller is an admin, and a page a browser loaded from any name can be pointed
// back at this loopback port by DNS rebinding: the request then carries that page's name as its Host.
// So off and demo serve a loopback Host, or a name under localhost, which browsers resolve to loopback
// themselves (the apps' <app>.<env>.localhost), and answer anything else 421. /ready alone answers
// any Host: a cluster's readiness probe names the pod's address, and it reads nothing.
func TestSignInOffServesLoopbackNamesAloneAgainstDNSRebinding(t *testing.T) {
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	serveAs := func(mode authMode, method, host, path string) int {
		req := httptest.NewRequest(method, "http://placeholder"+path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		(&app{auth: mode}).loopbackOnly(served).ServeHTTP(rec, req)
		return rec.Code
	}
	serve := func(mode authMode, host, path string) int { return serveAs(mode, http.MethodGet, host, path) }
	for _, mode := range []authMode{authOff, authDemo} {
		for _, host := range []string{"localhost:7777", "localhost", "LocalHost:7777", "localhost.:7777", "127.0.0.1:7777", "127.0.0.2:7777", "[::1]:7777", "[::1]",
			"claims-portal.preview.localhost:7777", "claims-portal.prod.localhost"} {
			if code := serve(mode, host, "/api/v1/fleet"); code != http.StatusNoContent {
				t.Errorf("%s, Host %q: %d, want it served", mode, host, code)
			}
		}
		for _, host := range []string{"evil.example.com:7777", "localhost.evil.com:7777", "127.0.0.1.nip.io:7777", "canyon-server:7777", "server:7777", "10.214.2.1:7777",
			"192.168.1.20:7777", "[fe80::1]:7777", "", "localhost:7777.evil.com"} {
			if code := serve(mode, host, "/api/v1/fleet"); code != http.StatusMisdirectedRequest {
				t.Errorf("%s, Host %q: %d, want 421", mode, host, code)
			}
			if code := serve(mode, host, "/"); code != http.StatusMisdirectedRequest {
				t.Errorf("%s, Host %q, the dashboard: %d, want 421", mode, host, code)
			}
		}
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} { // /ready has no exception: a probe asks it at loopback (#2449)
			if code := serveAs(mode, method, "10.244.0.7:7777", "/ready"); code != http.StatusMisdirectedRequest {
				t.Errorf("%s, %s /ready by the pod's address: %d, want 421", mode, method, code)
			}
		}
	}
	if code := serve(authOIDC, "canyon.example.com", "/api/v1/fleet"); code != http.StatusNoContent { // sign-in decides there, on any name
		t.Errorf("oidc: %d", code)
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var goodOIDC = map[string]string{"CANYON_OIDC_ISSUER": "https://login.example.com/v2.0", "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "Meridian.example", "CANYON_ADMINS": "Dana@meridian.example"}

func TestOIDCShapeIsRefusedByName(t *testing.T) {
	for name, c := range map[string]struct {
		set  map[string]string
		want string
	}{
		"nothing set":         {nil, "CANYON_AUTH=oidc needs CANYON_OIDC_ISSUER, CANYON_OIDC_CLIENT_ID, CANYON_ADMINS; set CANYON_AUTH=off for a local try"},
		"issuer not https":    {map[string]string{"CANYON_OIDC_ISSUER": "example.com"}, "CANYON_OIDC_ISSUER=example.com is not an https URL"},
		"issuer is http":      {map[string]string{"CANYON_OIDC_ISSUER": "http://login.example.com/v2.0"}, "CANYON_OIDC_ISSUER=http://login.example.com/v2.0 is not an https URL"},
		"two @ in an address": {map[string]string{"CANYON_ADMINS": "a@b@meridian.example"}, `CANYON_ADMINS: "a@b@meridian.example" is not an email address`},
		"space for a comma":   {map[string]string{"CANYON_OIDC_ALLOWED_EMAILS": "dana@meridian.example kim@meridian.example"}, `CANYON_OIDC_ALLOWED_EMAILS: "dana@meridian.example kim@meridian.example" is not an email address`},
		"claim is blank":      {map[string]string{"CANYON_ADMIN_CLAIM": "  ", "CANYON_ADMIN_VALUE": "platform"}, "CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE are set together or not at all"},
		"domain is a word":    {map[string]string{"CANYON_OIDC_ALLOWED_DOMAINS": "meridian"}, `CANYON_OIDC_ALLOWED_DOMAINS: "meridian" is not a domain`},
		"admin is a word":     {map[string]string{"CANYON_ADMINS": "ops"}, `CANYON_ADMINS: "ops" is not an email address`},
		"admin on no list":    {map[string]string{"CANYON_ADMINS": "ops@partner.example", "CANYON_OIDC_ALLOWED_EMAILS": "kim@meridian.example"}, "CANYON_ADMINS: ops@partner.example is on neither CANYON_OIDC_ALLOWED_DOMAINS nor CANYON_OIDC_ALLOWED_EMAILS"}, // both lists pinned by the environment: the row cannot add ops
		"claim without value": {map[string]string{"CANYON_ADMIN_CLAIM": "groups"}, "CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE are set together or not at all"},
		"value without claim": {map[string]string{"CANYON_ADMIN_VALUE": "platform"}, "CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE are set together or not at all"},
	} {
		env := map[string]string{}
		if c.set != nil {
			for k, v := range goodOIDC {
				env[k] = v
			}
			for k, v := range c.set {
				env[k] = v
			}
		}
		if _, err := oidcConfigOf(envOf(env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}

func TestOIDCShapeAcceptsEitherListLowerCasedAndNoSecret(t *testing.T) {
	c, err := oidcConfigOf(envOf(goodOIDC))
	if err != nil || c.clientSecret != "" || strings.Join(c.domains, ",") != "meridian.example" || strings.Join(c.admins, ",") != "dana@meridian.example" {
		t.Fatalf("a domain list alone, lower-cased, and no secret means a public client: %+v %v", c, err)
	}
	c, err = oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": "https://accounts.google.com", "CANYON_OIDC_CLIENT_ID": "x", "CANYON_OIDC_CLIENT_SECRET": "secret", "CANYON_OIDC_ALLOWED_EMAILS": " Dana@gmail.com, kim@gmail.com ", "CANYON_ADMINS": "dana@gmail.com", "CANYON_ADMIN_CLAIM": "groups", "CANYON_ADMIN_VALUE": "platform"}))
	if err != nil || c.clientSecret != "secret" || len(c.domains) != 0 || strings.Join(c.emails, ",") != "dana@gmail.com,kim@gmail.com" || c.adminValue != "platform" {
		t.Fatalf("an address list alone is a team without a domain: %+v %v", c, err)
	}
	if !memberOf("kim@gmail.com", c.domains, c.emails) || memberOf("someone@gmail.com", c.domains, c.emails) {
		t.Fatal("membership by address does not admit the whole issuer")
	}
	// No list at all, and an admin on neither: the row carries the lists once somebody has onboarded the host (design/ONBOARDING.md §1).
	if c, err = oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": "https://accounts.google.com", "CANYON_OIDC_CLIENT_ID": "x", "CANYON_ADMINS": "ana@gmail.com"})); err != nil || c.domains != nil || c.emails != nil {
		t.Fatalf("the bootstrap admin alone boots: %+v %v", c, err)
	}
	env := map[string]string{}
	for k, v := range goodOIDC {
		env[k] = v
	}
	env["CANYON_OIDC_ISSUER"] = "https://login.example.com/v2.0/"
	if c, err = oidcConfigOf(envOf(env)); err != nil || c.issuer != "https://login.example.com/v2.0" { // the document names it without the slash
		t.Fatalf("a trailing slash on the issuer is dropped, not left to fail every discovery match: %q %v", c.issuer, err)
	}
}

// The fetch is not fatal: a provider that is down at boot leaves the host unready, with oidc in
// waiting_on, and the retry lands the document when the provider comes back.
func TestReadyWaitsOnOIDCUntilDiscoveryLands(t *testing.T) {
	var calls atomic.Int32
	var issuer string // the test server's own URL, known once it listens
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/other/.well-known/openid-configuration":
			_, _ = io.WriteString(w, `{"issuer":"https://other.example"}`)
		case r.URL.Path == "/plain/.well-known/openid-configuration":
			_, _ = io.WriteString(w, `{"issuer":"`+issuer+`/plain","authorization_endpoint":"https://idp.example/auth","token_endpoint":"https://idp.example/token","jwks_uri":"http://idp.example/keys"}`)
		case r.URL.Path == "/hop/.well-known/openid-configuration":
			http.Redirect(w, r, issuer+"/.well-known/openid-configuration", http.StatusFound)
		case r.URL.Path != "/.well-known/openid-configuration" || calls.Add(1) == 1:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = io.WriteString(w, `{"issuer":"`+issuer+`","authorization_endpoint":"https://idp.example/auth","token_endpoint":"https://idp.example/token","jwks_uri":"https://idp.example/keys"}`)
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	if _, err := fetchDiscovery(context.Background(), idp.Client(), idp.URL+"/other"); err == nil || !strings.Contains(err.Error(), `("`+idp.URL+`/other") did not match the issuer URL returned by provider ("https://other.example")`) {
		t.Fatalf("a document naming another issuer is refused, not stored: %v", err)
	}
	if _, err := fetchDiscovery(context.Background(), idp.Client(), idp.URL+"/plain"); err == nil || err.Error() != "discovery document's jwks_uri is not an https URL" {
		t.Fatalf("a plain-http endpoint in the document is refused: %v", err)
	}
	if _, err := fetchDiscovery(context.Background(), discoveryClient(), idp.URL+"/hop"); err == nil || !strings.HasPrefix(err.Error(), "302 Found") {
		t.Fatalf("a redirecting issuer is not followed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuffer
	r := newReadiness(ctx, nil, nil, time.Hour, newTestLogger(&logs), discover(ctx, idp.Client(), &oidcConfig{issuer: idp.URL, clientID: "canyon"}, newTestLogger(&logs)))
	if w := r.waitingOn(); strings.Join(w, ",") != "postgres,durable_runs,oidc" {
		t.Fatalf("before discovery lands the host waits on oidc too, got %v", w)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !r.oidc.discovered() {
		if time.Now().After(deadline) {
			t.Fatalf("a failed fetch is retried until the provider answers, never fatal; after 4s: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w := r.waitingOn(); calls.Load() != 2 || !strings.Contains(logs.String(), "readiness: oidc: 502 Bad Gateway") || strings.Join(w, ",") != "postgres,durable_runs" {
		t.Fatalf("one failure logged, one retry landed, oidc gone from waiting_on: calls=%d waiting=%v logs=%s", calls.Load(), w, logs.String())
	}
}

// testIDP is an identity provider over TLS: a discovery document naming itself, a JWKS served from
// whatever keys the test has published so far, a signer for each key it holds, and the code flow:
// /auth records the request and redirects back with a code, /token redeems that code once the
// PKCE verifier matches and answers an ID token carrying the request's nonce, with bend over it.
// Keys are generated here, at test time.
type testIDP struct {
	*httptest.Server
	mu     sync.Mutex
	keys   jose.JSONWebKeySet
	priv   map[string]*rsa.PrivateKey
	jwks   atomic.Int32 // JWKS fetches: what proves on-demand and refetch-on-unknown-kid
	nonce  string
	secret string         // the client secret /token demands, "" for a public client
	auth   neturl.Values  // the last authorization request
	code   string         // the code it issued
	bend   map[string]any // over the ID token /token signs
	last   string         // the last ID token /token signed: what must never reach a log
}

func newTestIDP(t *testing.T) *testIDP {
	idp := &testIDP{priv: map[string]*rsa.PrivateKey{}, nonce: "n-" + t.Name()}
	idp.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = io.WriteString(w, `{"issuer":"`+idp.URL+`","authorization_endpoint":"`+idp.URL+`/auth","token_endpoint":"`+idp.URL+`/token","jwks_uri":"`+idp.URL+`/keys"}`)
		case "/keys":
			idp.jwks.Add(1)
			idp.mu.Lock()
			defer idp.mu.Unlock()
			_ = json.NewEncoder(w).Encode(idp.keys)
		case "/auth":
			code := "code-" + t.Name()
			idp.mu.Lock()
			idp.auth, idp.code = r.URL.Query(), code
			idp.mu.Unlock()
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code="+code+"&state="+neturl.QueryEscape(r.URL.Query().Get("state")), http.StatusFound) //nolint:gosec // the fake issuer sends the browser wherever the client registered, as a real one does
		case "/token":
			idp.mu.Lock()
			sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
			id, authed := r.FormValue("client_id"), false
			if u, pw, ok := r.BasicAuth(); ok { // a confidential client authenticates in the header, a public one names itself in the body and sends no secret
				id, authed = u, pw == idp.secret && idp.secret != ""
			} else {
				authed = idp.secret == "" && r.FormValue("client_secret") == ""
			}
			good := authed && id == "canyon" && r.FormValue("grant_type") == "authorization_code" && r.FormValue("code") == idp.code && r.FormValue("redirect_uri") == idp.auth.Get("redirect_uri") && base64.RawURLEncoding.EncodeToString(sum[:]) == idp.auth.Get("code_challenge")
			bend := map[string]any{"nonce": idp.auth.Get("nonce")}
			for k, v := range idp.bend {
				bend[k] = v
			}
			idp.code = "" // single use
			idp.mu.Unlock()
			if !good {
				writeErr(w, http.StatusBadRequest, "invalid_grant", "", nil)
				return
			}
			raw := idp.token(t, "a", bend)
			idp.mu.Lock()
			idp.last = raw
			idp.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": raw})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *testIDP) lastToken() string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.last
}

// key generates a signing key under kid; published puts it in the JWKS, unpublished is a forger's key.
func (idp *testIDP) key(t *testing.T, kid string, published bool) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.priv[kid] = priv
	if published {
		idp.keys.Keys = append(idp.keys.Keys, jose.JSONWebKey{Key: &priv.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
	}
}

// token signs a good ID token for the client "canyon" with the named key, with bend applied over it.
func (idp *testIDP) token(t *testing.T, kid string, bend map[string]any) string {
	c := map[string]any{"iss": idp.URL, "aud": "canyon", "sub": "u1", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": idp.nonce, "email": "Dana@Meridian.example"}
	for k, v := range bend {
		c[k] = v
	}
	idp.mu.Lock()
	priv := idp.priv[kid]
	idp.mu.Unlock()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: priv}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(c)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (idp *testIDP) discovered(t *testing.T) (context.Context, *provider) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var logs syncBuffer
	p := discover(ctx, idp.Client(), &oidcConfig{issuer: idp.URL, clientID: "canyon"}, newTestLogger(&logs))
	for deadline := time.Now().Add(10 * time.Second); !p.discovered(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("discovery did not land: %s", logs.String())
		}
	}
	return ctx, p
}

func TestOIDCIDTokenIsCheckedOnSignatureIssuerAudienceExpiryAndNonce(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	idp.key(t, "forged", false)
	ctx, p := idp.discovered(t)
	if _, err := (&provider{}).verifyIDToken(ctx, idp.token(t, "a", nil), idp.nonce); err == nil || err.Error() != "oidc discovery has not completed" {
		t.Fatalf("before discovery nothing validates: %v", err)
	}
	c, err := p.verifyIDToken(ctx, idp.token(t, "a", nil), idp.nonce)
	if err != nil || c["sub"] != "u1" || c["email"] != "Dana@Meridian.example" {
		t.Fatalf("a good token yields its claims untouched: %v %v", c, err)
	}
	for name, bad := range map[string]struct {
		kid   string
		bend  map[string]any
		nonce string
		want  string
	}{
		"forged signature":    {"forged", nil, idp.nonce, "failed to verify id token signature"},
		"another issuer":      {"a", map[string]any{"iss": "https://other.example"}, idp.nonce, "issued by a different provider"},
		"another audience":    {"a", map[string]any{"aud": "someone-else"}, idp.nonce, "expected audience"},
		"expired past leeway": {"a", map[string]any{"exp": time.Now().Add(-10 * time.Minute).Unix()}, idp.nonce, "token is expired"},
		"replayed nonce":      {"a", nil, "n-another-sign-in", "nonce is not the one this sign-in issued"},
		"no nonce to compare": {"a", nil, "", "nonce is not the one this sign-in issued"},
	} {
		if _, err := p.verifyIDToken(ctx, idp.token(t, bad.kid, bad.bend), bad.nonce); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: want %q, got %v", name, bad.want, err)
		}
	}
	skewed := map[string]any{"exp": time.Now().Add(-10 * time.Second).Unix(), "nbf": time.Now().Add(10 * time.Second).Unix()}
	if _, err := p.verifyIDToken(ctx, idp.token(t, "a", skewed), idp.nonce); err != nil { // a host ten seconds off its tenant still signs people in
		t.Errorf("a token ten seconds past exp and ten seconds before nbf is within the leeway: %v", err)
	}
}

func TestOIDCKeysAreFetchedOnDemandAndRefetchedOnAnUnknownKeyID(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	ctx, p := idp.discovered(t)
	if n := idp.jwks.Load(); n != 0 {
		t.Errorf("no key is copied at boot, got %d fetches", n)
	}
	if _, err := p.verifyIDToken(ctx, idp.token(t, "a", nil), idp.nonce); err != nil || idp.jwks.Load() != 1 {
		t.Fatalf("the first token fetches the keys once: %v fetches=%d", err, idp.jwks.Load())
	}
	idp.key(t, "b", true) // the provider rotates, on its own schedule, after the first fetch
	if _, err := p.verifyIDToken(ctx, idp.token(t, "b", nil), idp.nonce); err != nil || idp.jwks.Load() != 2 {
		t.Fatalf("a key introduced after the first fetch validates, by one refetch on the unknown kid: %v fetches=%d", err, idp.jwks.Load())
	}
	// "a" and not "b": go-oidc caches a refetched set in a goroutine after the verification returns, so a second "b" could refetch once more.
	if _, err := p.verifyIDToken(ctx, idp.token(t, "a", nil), idp.nonce); err != nil || idp.jwks.Load() != 2 {
		t.Fatalf("a known kid is served from the set already held: %v fetches=%d", err, idp.jwks.Load())
	}
}

func TestOIDCClaimReadersOverEntraOktaAndGoogle(t *testing.T) {
	entra := &oidcConfig{domains: []string{"meridian.example"}, adminClaim: "groups", adminValue: "6d1e2b0a-5c3f-4a7e-9b8d-0f1c2e3a4b5c"}
	okta := &oidcConfig{domains: []string{"meridian.example"}, emails: []string{"kim@partner.example"}, adminClaim: "groups", adminValue: "platform-admins"}
	google := &oidcConfig{emails: []string{"dana@gmail.com"}} // a team without a domain: no domain list at all
	rolled := &oidcConfig{domains: []string{"meridian.example"}, adminClaim: "role", adminValue: "platform"}
	for name, c := range map[string]struct {
		cfg            *oidcConfig
		claims         claims
		email, matched string // "" email: refused
		member         bool
		admin          string
	}{
		"entra, no email claim enabled, groups hit by object id": {entra, claims{"sub": "s1", "preferred_username": "Dana@Meridian.example", "name": "Dana Kim", "tid": "t1", "groups": []any{"1a2b3c4d-0000-4000-8000-000000000001", "6d1e2b0a-5c3f-4a7e-9b8d-0f1c2e3a4b5c"}}, "dana@meridian.example", "preferred_username", true, "hit"},
		"entra, optional email claim on, groups miss":            {entra, claims{"sub": "s2", "email": "Kim@meridian.example", "preferred_username": "kim_meridian.example#EXT#@tenant.onmicrosoft.com", "groups": []any{"1a2b3c4d-0000-4000-8000-000000000001"}}, "kim@meridian.example", "email", true, "miss"},
		"entra, group overage sends no groups claim":             {entra, claims{"sub": "s3", "preferred_username": "ola@meridian.example", "_claim_names": map[string]any{"groups": "src1"}}, "ola@meridian.example", "preferred_username", true, "absent"},
		"entra, guest from an unlisted domain":                   {entra, claims{"sub": "s4", "preferred_username": "guest@partner.example", "groups": []any{"6d1e2b0a-5c3f-4a7e-9b8d-0f1c2e3a4b5c"}}, "guest@partner.example", "preferred_username", false, "hit"},
		"okta, verified, groups hit by name":                     {okta, claims{"sub": "00u1a", "email": "dana@meridian.example", "email_verified": true, "groups": []any{"Everyone", "platform-admins"}}, "dana@meridian.example", "email", true, "hit"},
		"okta, contractor listed by address":                     {okta, claims{"sub": "00u1b", "email": "Kim@Partner.example", "email_verified": true, "groups": []any{"Everyone"}}, "kim@partner.example", "email", true, "miss"},
		"okta, email_verified false at a listed domain":          {okta, claims{"sub": "00u1c", "email": "dana@meridian.example", "email_verified": false}, "", "", false, "unset"},
		"okta, email_verified false for a listed address":        {okta, claims{"sub": "00u1d", "email": "kim@partner.example", "email_verified": false, "groups": []any{"platform-admins"}}, "", "", false, "unset"},
		"google, address on the list, no domain list configured": {google, claims{"sub": "1078", "email": "Dana@gmail.com", "email_verified": true, "name": "Dana", "hd": ""}, "dana@gmail.com", "email", true, "unset"},
		"google, another gmail account":                          {google, claims{"sub": "1079", "email": "someone@gmail.com", "email_verified": true}, "someone@gmail.com", "email", false, "unset"},
		"google, email_verified false for the listed address":    {google, claims{"sub": "1080", "email": "dana@gmail.com", "email_verified": false}, "", "", false, "unset"},
		"string claim, equality":                                 {rolled, claims{"sub": "s5", "email": "dana@meridian.example", "role": "platform"}, "dana@meridian.example", "email", true, "hit"},
		"string claim, another value":                            {rolled, claims{"sub": "s6", "email": "dana@meridian.example", "role": "viewer"}, "dana@meridian.example", "email", true, "miss"},
		"neither email claim":                                    {entra, claims{"sub": "s7", "name": "Nobody"}, "", "", false, "unset"},
		"string claim, right value in another case":              {rolled, claims{"sub": "s8", "email": "dana@meridian.example", "role": "Platform"}, "dana@meridian.example", "email", true, "miss"},
		"entra, email present but empty":                         {entra, claims{"sub": "s9", "email": "", "preferred_username": "ola@meridian.example"}, "ola@meridian.example", "preferred_username", true, "absent"},
		"preferred_username that is not an address":              {entra, claims{"sub": "s10", "preferred_username": "dana"}, "", "", false, "unset"},
		"an address carrying a line break":                       {entra, claims{"sub": "s11", "email": "dana@meridian.example\nadmin_claim hit"}, "", "", false, "unset"},
		"email_verified as the string true":                      {okta, claims{"sub": "00u1e", "email": "dana@meridian.example", "email_verified": "true"}, "", "", false, "unset"},
	} {
		email, matched, err := emailOf(c.claims)
		if (err == nil) != (c.email != "") || email != c.email || matched != c.matched {
			t.Errorf("%s: email %q via %q %v, want %q via %q", name, email, matched, err, c.email, c.matched)
			continue
		}
		if err != nil {
			continue
		}
		if got := memberOf(email, c.cfg.domains, c.cfg.emails); got != c.member {
			t.Errorf("%s: member %v, want %v", name, got, c.member)
		}
		if got := adminClaimOf(c.claims, c.cfg.adminClaim, c.cfg.adminValue); got != c.admin {
			t.Errorf("%s: admin claim %s, want %s", name, got, c.admin)
		}
	}
	if got := adminClaimOf(claims{"groups": []any{"platform-admins"}}, "", ""); got != "unset" { // Google Workspace: no group claim exists, CANYON_ADMINS only
		t.Fatalf("no claim configured is unset whatever the token carries, got %s", got)
	}
}

// go-oidc puts a provider's response body in its error, whole: the log line takes 200 bytes of it
// and never a second line, so a WAF's HTML page cannot fill the log or forge a line every retry.
func TestADiscoveryErrorIsOneBoundedLogLine(t *testing.T) {
	got := oneLine("502 Bad Gateway: <html>\n" + strings.Repeat("x", 300) + "\r\nreadiness: forged")
	if strings.ContainsAny(got, "\n\r") || len(got) != 200 || !strings.HasPrefix(got, "502 Bad Gateway: <html> x") {
		t.Fatalf("%q", got)
	}
}

// Nothing in the suite defended the domain comparison: changing it to a suffix match broke no
// test, and with that fault in place a registrable lookalike is admitted as a member.
func TestMemberOfMatchesTheWholeDomainLabelNotASuffix(t *testing.T) {
	for _, c := range []struct {
		email string
		want  bool
	}{
		{"dana@meridian.example", true},
		{"dana@sub.meridian.example", false},          // a subdomain is not the domain
		{"dana@evilmeridian.example", false},          // a suffix match, not a label match
		{"dana@meridian.example.evil.example", false}, // the domain is a suffix of the attacker's, not the reverse
	} {
		if got := memberOf(c.email, []string{"meridian.example"}, nil); got != c.want {
			t.Errorf("%s: member %v, want %v", c.email, got, c.want)
		}
	}
}

// memberOf compares lower-cased strings, and strings.ToLower is not a case fold: the Kelvin sign
// lower-cases to a plain k. The guard is the validator every address passes on the way in — the
// environment's lists, the PUT's, and the id token's claim — so such a domain never reaches a list.
func TestADomainIsASCIIOrItIsNotADomain(t *testing.T) {
	const kelvin = "Kiosk.example"
	if isDomain(kelvin) || isEmail("dana@"+kelvin) {
		t.Fatalf("%q lower-cases onto a listed kiosk.example and must not pass as a domain", kelvin)
	}
}

// An address reaches audit rows, canyon share, the server's log and the deploy output's access-request
// lines, and preferred_username is user-settable at some providers (#2499): a control character, or a
// bidi or zero-width format character as the CLI's visible() lists them, refuses it, before the @ or
// after it — at sign-in, in the environment's lists, and wherever isEmail stands guard.
func TestEmailRefusesControlAndFormatCharacters(t *testing.T) {
	if !isEmail("dana@meridian.example") {
		t.Fatal("a plain address is an address")
	}
	if email, _, err := emailOf(claims{"preferred_username": "Dana@Meridian.example"}); err != nil || email != "dana@meridian.example" {
		t.Fatalf("a plain preferred_username signs in: %q %v", email, err)
	}
	for name, r := range map[string]string{"ESC": "\x1b", "BEL": "\x07", "DEL": "\x7f", "U+202E": "\u202e", "U+200B": "\u200b", "U+0085": "\u0085", "U+2066": "\u2066", "U+FEFF": "\ufeff"} {
		for _, addr := range []string{"da" + r + "na@meridian.example", "dana@meridian" + r + ".example", "dana" + r + "[31m@meridian.example"} {
			if isEmail(addr) {
				t.Errorf("%s: isEmail(%q) accepts it", name, addr)
			}
			for _, claim := range []string{"email", "preferred_username"} {
				if email, _, err := emailOf(claims{claim: addr}); err == nil {
					t.Errorf("%s: the %s claim %q signs in as %q", name, claim, addr, email)
				}
			}
			if _, err := listOf("CANYON_ADMINS", "olive@meridian.example,"+addr, "an email address", isEmail); err == nil {
				t.Errorf("%s: CANYON_ADMINS accepts %q", name, addr)
			}
		}
	}
}

// Zitadel's project roles are an object keyed by role, not an array: its keys are the groups a sign-in
// stores, in name order, and the admin claim matches one of them (contracts/routes.md, App teams; #1662).
func TestTheGroupsClaimReadsAnObjectsKeys(t *testing.T) {
	const roles = "urn:zitadel:iam:org:project:roles"
	c := claims{"sub": "z1", "email": "dana@meridian.example", roles: map[string]any{
		"platform":    map[string]any{"341234": "meridian.zitadel.cloud"},
		"claims-team": map[string]any{"341234": "meridian.zitadel.cloud"},
	}}
	if groups, read := groupsClaimOf(c, roles); read != "ok" || !slices.Equal(groups, []string{"claims-team", "platform"}) {
		t.Errorf("groups %q (%s)", groups, read)
	}
	if got := adminClaimOf(c, roles, "platform"); got != "hit" {
		t.Errorf("admin by a role: %s", got)
	}
	if got := adminClaimOf(c, roles, "viewer"); got != "miss" {
		t.Errorf("admin by a role not held: %s", got)
	}
	many := map[string]any{}
	for i := range 201 { // identity.MaxGroups + 1
		many[fmt.Sprintf("role-%03d", i)] = map[string]any{}
	}
	if groups, read := groupsClaimOf(claims{roles: many}, roles); read != "over-200" || len(groups) != 0 {
		t.Errorf("over 200 roles: %d (%s)", len(groups), read)
	}
}
