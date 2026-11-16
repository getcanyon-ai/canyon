package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/go-jose/go-jose/v4"
)

func newKey() *ecdsa.PrivateKey { k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); return k }

func sign(key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
	payload, _ := json.Marshal(claims)
	jws, _ := signer.Sign(payload)
	s, _ := jws.CompactSerialize()
	return s
}

// gateway is the identity provider in front of the app: discovery, a key set, and how often it is fetched.
type gateway struct {
	srv     *httptest.Server
	key     *ecdsa.PrivateKey
	mu      sync.Mutex
	keys    jose.JSONWebKeySet
	fetches atomic.Int32
}

func newGateway(t *testing.T) *gateway {
	g := &gateway{key: newKey()}
	g.keys.Keys = []jose.JSONWebKey{{Key: &g.key.PublicKey, KeyID: "k1", Algorithm: "ES256", Use: "sig"}}
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.URL.Path == "/keys" {
			g.fetches.Add(1)
			_ = json.NewEncoder(w).Encode(g.keys)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": g.srv.URL, "jwks_uri": g.srv.URL + "/keys", "authorization_endpoint": g.srv.URL + "/auth"})
	}))
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

// claims are an ID token's of the gateway for this person, good for an hour.
func (g *gateway) claims(email string, groups ...string) map[string]any {
	return map[string]any{"iss": g.srv.URL, "aud": "gateway-client", "sub": "u-" + strings.ToLower(email), "email": email, "name": "Dana Whitfield",
		"groups": groups, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
}

func (g *gateway) token(email string, groups ...string) string {
	return sign(g.key, "k1", g.claims(email, groups...))
}

// rig is a sidecar built from its environment as the executor writes it, in front of an app that
// passes on the headers of every request that reaches it.
type rig struct {
	g     *gateway
	h     http.Handler
	key   *identity.Key
	file  string
	write func([]byte) // where access lands: the file, or pull mode's stub Canyon
	seen  chan http.Header
	from  string // the issuer the file says recorded its subjects: the gateway's own, unless a test says otherwise
}

func newRig(t *testing.T, door string) *rig {
	r := &rig{g: newGateway(t), file: filepath.Join(t.TempDir(), "access.json"), seen: make(chan http.Header, 64)}
	r.write = func(b []byte) {
		if err := os.WriteFile(r.file, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		req.Header.Set("Host", req.Host)
		r.seen <- req.Header
	}))
	t.Cleanup(app.Close)
	env := map[string]string{"CANYON_IDENTITY_ISSUER": "https://canyon.meridian.example", "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview",
		"CANYON_FRONT_DOOR": strings.ReplaceAll(door, "GW", r.g.srv.URL)}
	c, err := configOf(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	v, err := verifierOf(context.Background(), c.door, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	r.from = r.g.srv.URL
	r.key, _ = identity.Generate()
	upstream, _ := url.Parse(app.URL)
	r.h = newSidecar(c, v, r.key, &accessReader{path: r.file}, upstream)
	r.access(t, "2026-09-25T10:00:00Z", identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana@meridian.example", Level: "owner", Roles: []string{"approver"}},
		identity.Grant{Kind: "group", Grantee: "claims-team", Level: "viewer"})
	return r
}

const (
	choice1 = `{"choice":1,"gateway":"oidc","issuer":"GW","audience":"gateway-client","jwks_url":"GW/keys","cookie":"_pomerium","domain":"meridian.example","form":"app.env.domain"}`
	choice2 = `{"choice":2,"signin":{"issuer":"GW","client_id":"gateway-client"},"domain":"meridian.example","form":"app-env.domain"}`
)

// access writes the app's access file as the executor's ConfigMap carries it.
func (r *rig) access(t *testing.T, updatedAt string, entries ...identity.Grant) {
	b, _ := json.Marshal(map[string]any{"updated_at": updatedAt, "groups_claim": "groups", "subject_issuer": r.from, "roles": map[string]string{"approver": "Approves claims"},
		"members": map[string]any{"domains": []string{"meridian.example"}, "emails": []string{"contractor@gmail.com"}},
		"admins":  map[string]any{"emails": []string{}, "claim": "groups", "value": "canyon-admins"}, "entries": entries})
	r.write(b)
}

// do sends one request as the router would, with tok as the bearer ("" none) and headers in pairs.
func (r *rig) do(method, path, tok string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://claims-portal.preview.meridian.example"+path, nil)
	if tok != "" {
		headers = append(headers, "Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// The app is handed Canyon's token — the sidecar's key, the app as audience, the target, the person's
// roles — and none of the front door's credentials: not its token, no X-Amzn-Oidc-*, no session cookie,
// even when a Connection header names Authorization and Cookie as hop-by-hop.
func TestTheAppGetsCanyonsTokenAndNoFrontDoorCredential(t *testing.T) {
	r := newRig(t, choice1)
	rec := r.do("GET", "/claims", r.g.token("Dana@Meridian.example"), "X-Amzn-Oidc-Data", "x", "x-amzn-oidc-accesstoken", "y",
		"Cookie", "_pomerium=s; app_session=1", "Cookie", "_pomerium_csrf=c", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "evil.example", "Connection", "Authorization, Cookie")
	if rec.Code != http.StatusOK || len(r.seen) != 1 {
		t.Fatalf("%d %s, app reached %d times", rec.Code, rec.Body, len(r.seen))
	}
	got := <-r.seen
	if got.Get("X-Amzn-Oidc-Data")+got.Get("X-Amzn-Oidc-Accesstoken")+got.Get("X-Forwarded-Host") != "" {
		t.Errorf("an X-Amzn-Oidc-* or X-Forwarded-Host header reached the app: %v", got)
	}
	if got.Get("Cookie") != "app_session=1" || got.Get("X-Forwarded-Proto") != "https" || got.Get("Host") != "claims-portal.preview.meridian.example" {
		t.Errorf("cookie %q, proto %q, host %q", got.Get("Cookie"), got.Get("X-Forwarded-Proto"), got.Get("Host"))
	}
	doc, _ := r.key.JWKS()
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(doc) }))
	defer keys.Close()
	minted, err := oidc.NewVerifier("https://canyon.meridian.example", oidc.NewRemoteKeySet(context.Background(), keys.URL), &oidc.Config{ClientID: "claims-portal", SupportedSigningAlgs: []string{oidc.ES256}}).
		Verify(context.Background(), strings.TrimPrefix(got.Get("Authorization"), "Bearer "))
	if err != nil {
		t.Fatalf("the app's Authorization is not Canyon's token for it: %v", err)
	}
	var c identity.Claims
	_ = minted.Claims(&c)
	if c.Email != "dana@meridian.example" || c.Subject != "u-dana@meridian.example" || c.Env != "preview" || c.Name != "Dana Whitfield" || len(c.Roles) != 1 || c.Roles[0] != "approver" {
		t.Fatalf("claims %+v", c)
	}
}

// A token that fails is 401 and nothing of it reaches the app: one the gateway minted for another
// client, an expired one under choice 1 (no sign-in is the sidecar's to start), and an Authorization
// the browser made up — another key under the gateway's kid, not a token, a second header beside a
// good one, no Bearer — or none at all.
func TestAWrongAudienceOrAForgedAuthorizationIs401AndNeverReachesTheApp(t *testing.T) {
	r := newRig(t, choice1)
	other, expired, good := r.g.claims("dana@meridian.example"), r.g.claims("dana@meridian.example"), r.g.token("dana@meridian.example")
	other["aud"], expired["exp"] = "another-client", time.Now().Add(-time.Minute).Unix()
	for name, h := range map[string][]string{
		"wrong audience": {"Authorization", "Bearer " + sign(r.g.key, "k1", other)},
		"expired":        {"Authorization", "Bearer " + sign(r.g.key, "k1", expired)},
		"forged":         {"Authorization", "Bearer " + sign(newKey(), "k1", r.g.claims("dana@meridian.example"))},
		"not a token":    {"Authorization", "Bearer not-a-token"},
		"two headers":    {"Authorization", "Bearer " + good, "Authorization", "Bearer " + good},
		"no Bearer":      {"Authorization", good}, "no Authorization": {},
	} {
		if rec := r.do("GET", "/", "", h...); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(r.seen) != 0 {
		t.Fatalf("the app was reached %d times", len(r.seen))
	}
}

// A browser reaching the app with no sign-in the sidecar accepts gets a page saying what the visitor
// can do, still 401 and never the app, and the gateway to check goes to the log alone, never the
// token; a script, or HTML refused at q=0, keeps the JSON (canyonv2-8c2q).
func TestABrowserWithNoSignInGetsAPageAndAScriptTheJSON(t *testing.T) {
	r := newRig(t, choice1)
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	forged := sign(newKey(), "k1", r.g.claims("dana@meridian.example"))
	for name, tok := range map[string]string{"no token": "", "a forged token": forged} {
		logged.Reset()
		rec := r.do("GET", "/claims", tok, "Accept", browser)
		page := rec.Body.String()
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
			!strings.Contains(page, "You're not signed in to this app. Whoever shared the link with you can help.") || strings.Contains(page, "gateway") {
			t.Errorf("a browser with %s: %d %q %s", name, rec.Code, rec.Header().Get("Content-Type"), page)
		}
		if !strings.Contains(logged.String(), "check that the gateway's sign-in rule covers this app's address") || strings.Contains(logged.String(), forged) {
			t.Errorf("a browser with %s: the log %q", name, logged.String())
		}
	}
	for name, accept := range map[string]string{"a script": "application/json", "no Accept": "*/*", "HTML refused": "text/html;q=0, application/json"} {
		rec := r.do("GET", "/claims", "", "Accept", accept)
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":"unauthenticated","message":"sign in to open this app"}`+"\n" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(r.seen) != 0 {
		t.Fatalf("the app was reached %d times", len(r.seen))
	}
}

// Level none is the no-access screen: a member no grant names — one holding the address of a grant bound
// to another person's subject among them — and anyone off the membership lists even when the app is shared
// with everyone. A group grant, everyone, the admin claim, and the bound person at a new address open it.
func TestSomeoneNotOnTheAccessListGoesToNoAccess(t *testing.T) {
	r := newRig(t, choice1)
	unverified, reused, moved := r.g.claims("dana@meridian.example"), r.g.claims("dana@meridian.example"), r.g.claims("dana.w@meridian.example")
	unverified["email_verified"], reused["sub"], moved["sub"] = false, "u-someone-new", "u-dana@meridian.example"
	for _, c := range []struct {
		name, tok string
		code      int
	}{
		{"a member no grant names", r.g.token("sam@meridian.example"), http.StatusFound},
		{"a bound grant's address under another subject", sign(r.g.key, "k1", reused), http.StatusFound},
		{"the bound subject at a new address", sign(r.g.key, "k1", moved), http.StatusOK},
		{"their group", r.g.token("sam@meridian.example", "claims-team"), http.StatusOK},
		{"the admin claim", r.g.token("sam@meridian.example", "canyon-admins"), http.StatusOK},
		{"an admin off the membership lists", r.g.token("sam@partner.example", "canyon-admins"), http.StatusFound},
		{"email_verified false", sign(r.g.key, "k1", unverified), http.StatusUnauthorized},
	} {
		rec := r.do("GET", "/", c.tok)
		if rec.Code != c.code || c.code == http.StatusFound && rec.Header().Get("Location") != "https://canyon.meridian.example/#/no-access/claims-portal" {
			t.Errorf("%s: %d %s %s", c.name, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
	}
	r.access(t, "2026-09-25T11:00:00Z", identity.Grant{Kind: "everyone", Level: "viewer"})
	for email, code := range map[string]int{"sam@meridian.example": 200, "contractor@gmail.com": 200, "someone@gmail.com": 302} {
		if rec := r.do("GET", "/", r.g.token(email)); rec.Code != code {
			t.Errorf("everyone, %s: %d, want %d", email, rec.Code, code)
		}
	}
}

// A bound grant opens the app to its subject only on a token from the issuer the file names, whatever
// client it was issued to: its azp, or its audiences. Under any other issuer, or a file that names none,
// it names nobody, its person's group and a grant bound to no one still opening it (#2220).
func TestABoundGrantNamesNobodyOffCanyonsSignIn(t *testing.T) {
	for name, door := range map[string]string{"choice 1": choice1, "choice 2": choice2} {
		r := newRig(t, door)
		azp, audiences := r.g.claims("dana@meridian.example"), r.g.claims("dana@meridian.example")
		azp["aud"], azp["azp"] = []string{"gateway-client", "another-api"}, "another-api"
		audiences["aud"] = []string{"gateway-client", "another-api"}
		dana, team, priya := r.g.token("dana@meridian.example"), r.g.token("dana@meridian.example", "claims-team"), r.g.token("priya@meridian.example")
		for i, c := range []struct {
			name string
			from string
			tok  string
			code int
		}{
			{"Canyon's sign-in", r.from, dana, http.StatusOK},
			{"Canyon's issuer, another azp", r.from, sign(r.g.key, "k1", azp), http.StatusOK},
			{"Canyon's issuer, two audiences and no azp", r.from, sign(r.g.key, "k1", audiences), http.StatusOK},
			{"another issuer", "https://login.meridian.example", dana, http.StatusFound},
			{"a file from before the issuer was named", "", dana, http.StatusFound},
			{"another issuer, through the group", "https://login.meridian.example", team, http.StatusOK},
			{"another issuer, a grant bound to no one", "https://login.meridian.example", priya, http.StatusOK},
		} {
			r.from = c.from
			r.access(t, fmt.Sprintf("2026-09-25T12:00:%02dZ", i), identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana@meridian.example", Level: "owner"},
				identity.Grant{Kind: "group", Grantee: "claims-team", Level: "viewer"}, identity.Grant{Kind: "email", Grantee: "priya@meridian.example", Level: "viewer"})
			if rec := r.do("GET", "/", c.tok); rec.Code != c.code {
				t.Errorf("%s, %s: %d, want %d", name, c.name, rec.Code, c.code)
			}
		}
	}
}

// Google signs some ID tokens with iss accounts.google.com, which go-oidc takes for its issuer
// https://accounts.google.com: a bearer token is from the issuer its verifier checked, so it compares
// with the access file's exactly (#2220).
func TestABearerTokenIsFromTheIssuerItsVerifierChecked(t *testing.T) {
	key := newKey()
	const google = "https://accounts.google.com"
	v := bearer(oidc.NewVerifier(google, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, &oidc.Config{ClientID: "gateway-client", SupportedSigningAlgs: []string{oidc.ES256}}), google)
	for _, iss := range []string{google, "accounts.google.com"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+sign(key, "k1", map[string]any{"iss": iss, "aud": "gateway-client", "sub": "u-dana", "exp": time.Now().Add(time.Hour).Unix()}))
		if _, from, err := v(req); err != nil || from != google {
			t.Errorf("iss %s: %q %v, want %s", iss, from, err, google)
		}
	}
}

// A token from an issuer other than the access file's is logged once for that issuer, the two issuers
// and nothing of the person: an operator sees why a bound grant opens nothing (#2220).
func TestAnotherIssuerIsLoggedOnce(t *testing.T) {
	r := newRig(t, choice1)
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dana := r.g.token("dana@meridian.example")
	for i, from := range []string{r.from, r.from, "https://login.meridian.example", "https://login.meridian.example"} {
		r.from = from
		r.access(t, fmt.Sprintf("2026-09-25T12:00:%02dZ", i), identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana@meridian.example", Level: "owner"})
		r.do("GET", "/", dana)
	}
	want := fmt.Sprintf("canyon-identity: a token from %q, not Canyon's sign-in issuer %q: a grant bound to a person matches nobody on it, a group or everyone grant still does", r.g.srv.URL, "https://login.meridian.example")
	if got := logged.String(); strings.Count(got, want) != 1 || strings.Contains(got, "dana") {
		t.Fatalf("want %q once and nothing of dana, got %q", want, got)
	}
}

// The file is read on every request, so an unshare lands on the next one — but a file older than one
// already read is never read after it: the newer stands. A file that does not parse refuses with 503.
func TestAnOlderAccessFileIsNeverReadAfterANewer(t *testing.T) {
	r := newRig(t, choice1)
	tok := r.g.token("dana@meridian.example")
	for _, step := range []struct {
		file string
		code int
	}{{"", 200}, {"2026-09-25T09:00:00Z", 200}, {"2026-09-25T10:00:01Z", 302}, {"{", 503}} {
		if step.file == "{" {
			_ = os.WriteFile(r.file, []byte("{"), 0o600)
		} else if step.file != "" {
			r.access(t, step.file) // no grant: an unshare of everyone
		}
		if rec := r.do("GET", "/", tok); rec.Code != step.code {
			t.Fatalf("after %q: %d, want %d", step.file, rec.Code, step.code)
		}
	}
}

// Choice 2: the token oauth2-proxy passes on is verified against signin's issuer through its discovery,
// its session cookie never reaches the app, and an expired token goes to a fresh sign-in, back here.
func TestChoice2VerifiesTheForwardedTokenAndSignsAnExpiredOneInAgain(t *testing.T) {
	r := newRig(t, choice2)
	rec := r.do("GET", "/", r.g.token("dana@meridian.example"), "Cookie", "_oauth2_proxy=s; _oauth2_proxy_csrf=c; app=1")
	if rec.Code != http.StatusOK || (<-r.seen).Get("Cookie") != "app=1" {
		t.Fatalf("%d, or the front door's cookie reached the app", rec.Code)
	}
	expired := r.g.claims("dana@meridian.example")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	if rec = r.do("GET", "/claims?id=5", sign(r.g.key, "k1", expired)); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/oauth2/start?rd=%2Fclaims%3Fid%3D5" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A change another site's page sends is 403 before anything else; the app's own pages pass.
func TestACrossOriginChangeIs403(t *testing.T) {
	r := newRig(t, choice1)
	tok := r.g.token("dana@meridian.example")
	for h, code := range map[[2]string]int{{"Sec-Fetch-Site", "same-site"}: 403, {"Origin", "https://quote-engine.preview.meridian.example"}: 403,
		{"Origin", "https://claims-portal.preview.meridian.example"}: 200, {"Sec-Fetch-Site", "same-origin"}: 200} {
		if rec := r.do("POST", "/", tok, h[0], h[1]); rec.Code != code {
			t.Errorf("%v: %d, want %d", h, rec.Code, code)
		}
	}
}

// A kid the key set lacks fetches it again at most once a minute; after that minute, a rotated key verifies.
func TestAnUnknownKidFetchesTheKeySetAtMostOnceAMinute(t *testing.T) {
	g := newGateway(t)
	keys := &keySet{url: g.srv.URL + "/keys", client: http.DefaultClient}
	if err := keys.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated := newKey()
	g.mu.Lock()
	g.keys.Keys = append(g.keys.Keys, jose.JSONWebKey{Key: &rotated.PublicKey, KeyID: "k2", Algorithm: "ES256", Use: "sig"})
	g.mu.Unlock()
	tok := sign(rotated, "k2", g.claims("dana@meridian.example"))
	for range 3 {
		if _, err := keys.VerifySignature(context.Background(), tok); err == nil {
			t.Fatal("verified with a key set fetched before the rotation")
		}
	}
	keys.fetched = time.Now().Add(-time.Minute)
	if _, err := keys.VerifySignature(context.Background(), tok); err != nil {
		t.Fatalf("a minute on, the rotated key: %v", err)
	}
	if n := g.fetches.Load(); n != 2 {
		t.Fatalf("%d fetches, want 2", n)
	}
}

// The environment holds what the sidecar reads and nothing else: oauth2-proxy's secret refuses the start,
// as does a load balancer outside partition aws or whose region is not one host label, or a gateway on choice 2.
func TestConfigRefusesWhatTheSidecarDoesNotRead(t *testing.T) {
	for _, door := range []string{`{"choice":2,"signin":{"issuer":"https://login.example","client_id":"c","client_secret":"s"}}`,
		`{"choice":1,"gateway":"oidc","issuer":"https://login.example","audience":"a","jwks_url":"https://login.example/keys"}`,
		`{"choice":1,"gateway":"alb","alb_arn":"` + strings.Replace(albArn, "arn:aws:", "arn:aws-cn:", 1) + `"}`,
		`{"choice":1,"gateway":"alb","alb_arn":"` + strings.Replace(albArn, "eu-central-1", "evil.example/x", 1) + `"}`,
		`{"choice":2,"gateway":"alb","alb_arn":"` + albArn + `","signin":{"issuer":"https://login.example","client_id":"c"}}`} {
		env := map[string]string{"CANYON_IDENTITY_ISSUER": "https://canyon.example", "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview", "CANYON_FRONT_DOOR": door}
		if _, err := configOf(func(k string) string { return env[k] }); err == nil {
			t.Errorf("started with %s", door)
		}
	}
}

// The sidecar reads the groups claim as the server does: an object's keys, Zitadel's project roles,
// are the person's groups, and the admin claim matches one of them (contracts/executor.md; #1662).
func TestTheSidecarReadsAnObjectClaimsKeys(t *testing.T) {
	const roles = "urn:zitadel:iam:org:project:roles"
	f := &access{GroupsClaim: roles}
	f.Admins.Claim, f.Admins.Value = roles, "platform"
	c := map[string]any{roles: map[string]any{"platform": map[string]any{}, "claims-team": map[string]any{}}}
	if got := f.groups(c); !slices.Equal(got, []string{"claims-team", "platform"}) {
		t.Errorf("groups %q", got)
	}
	if !f.admin("dana@meridian.example", c) {
		t.Error("an admin by a role is not admin")
	}
	if f.Admins.Value = "viewer"; f.admin("dana@meridian.example", c) {
		t.Error("a role not held made an admin")
	}
	many := map[string]any{}
	for i := range 201 {
		many[fmt.Sprintf("team-%03d", i)] = map[string]any{}
	}
	unset := &access{} // no groups_claim: a token's claim named "" is never read as groups
	if got := unset.groups(map[string]any{"": []any{"claims-team"}}); len(got) != 0 {
		t.Errorf("an unset groups_claim read %q", got)
	}
	for name, claim := range map[string]any{"over 200 roles": many, "a list with a number": []any{"claims-team", 7.0}} {
		if got := f.groups(map[string]any{roles: claim}); len(got) != 0 { // none, as the server stores none, never half a list
			t.Errorf("%s: groups %q", name, got)
		}
	}
}

// GET /answered asks the app one fixed GET / as the address the executor set, whatever the request that
// asked carried: its Host, path, query, headers and body never reach the app. Any other path or method is
// refused (contracts/executor.md, What the app answered; #1740).
func TestTheAnsweredEndpointAsksTheAppOneFixedGET(t *testing.T) {
	var asked []*http.Request
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h1>Bad Request (400)</h1>\n"))
	}))
	defer app.Close()
	srv := httptest.NewServer(answering("claims-portal.preview.canyon.meridian.example", app.URL+"/"))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/answered?url=http://169.254.169.254/&host=evil.example", strings.NewReader("body"))
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer stolen")
	req.Header.Set("Cookie", "_oauth2_proxy=x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil || resp.StatusCode != 200 || len(got) != 3 || got["host"] != "claims-portal.preview.canyon.meridian.example" || got["said"] != "<h1>Bad Request (400)</h1>" || got["status"] != 400.0 {
		t.Fatalf("answered %d %s", resp.StatusCode, body)
	}
	if len(asked) != 1 || asked[0].Host != "claims-portal.preview.canyon.meridian.example" || asked[0].URL.RequestURI() != "/" || asked[0].Header.Get("Authorization") != "" || asked[0].Header.Get("Cookie") != "" {
		t.Fatalf("the app was asked %d times: %+v", len(asked), asked)
	}
	for method, path := range map[string]string{http.MethodPost: "/answered", http.MethodGet: "/answered/x"} {
		r, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != map[string]int{http.MethodPost: 405, http.MethodGet: 404}[method] || len(asked) != 1 {
			t.Errorf("%s %s: %d, the app asked %d times", method, path, resp.StatusCode, len(asked))
		}
	}
}

// Identity headers a client sent, X-Forwarded-Email and its kin from oauth2-proxy and nginx's auth_request,
// never reach the app, nor any header whose name holds an underscore, which CGI-style stacks (Rack, Django,
// PHP) read as the dashed one: Canyon's token is what says who the person is (contracts/executor.md, The
// front door). What the sidecar sets for the app, and the app's other headers, still reach it.
func TestClientSentIdentityHeadersNeverReachTheApp(t *testing.T) {
	r := newRig(t, choice1)
	req := httptest.NewRequest("GET", "http://claims-portal.preview.meridian.example/", nil)
	sent := []string{"X-Forwarded-Email", "X-Forwarded-User", "X-Forwarded-Groups", "X-Forwarded-Preferred-Username", "X-Forwarded-Access-Token", "X-Auth-Request-Email",
		"X-Auth-Request-Groups", "X-Amzn-Oidc-Data", "Remote-User", "X-Real-Ip", "Client-Ip", "X-Client-Ip", "True-Client-Ip", "Forwarded",
		"X-Scheme", "Front-End-Https", "X-Url-Scheme", "X-Webauth-User", "X-Remote-User", "Cf-Connecting-Ip", "x-cluster-CLIENT-ip",
		"x-FORWARDED-email", "X_Forwarded_Email", "x_FORWARDED_user", "X-Forwarded_Groups", "X_AUTH_REQUEST_USER", "authorization", "cookie"}
	for _, h := range sent {
		req.Header[h] = []string{"boss@meridian.example"} // as the wire spelled it, never canonicalised
	}
	req.Header.Set("Authorization", "Bearer "+r.g.token("dana@meridian.example"))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Request-Id", "r-1")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := <-r.seen
	for name, v := range got {
		if strings.Contains(name, "_") || slices.Contains(v, "boss@meridian.example") {
			t.Errorf("%s reached the app: %q", name, v)
		}
	}
	if got.Get("X-Forwarded-Proto") != "https" || got.Get("X-Request-Id") != "r-1" || len(got.Values("Authorization")) != 1 {
		t.Errorf("what the app should get: %v", got)
	}
	doc, _ := r.key.JWKS()
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(doc) }))
	defer keys.Close()
	if _, err := oidc.NewVerifier("https://canyon.meridian.example", oidc.NewRemoteKeySet(context.Background(), keys.URL), &oidc.Config{ClientID: "claims-portal", SupportedSigningAlgs: []string{oidc.ES256}}).
		Verify(context.Background(), strings.TrimPrefix(got.Get("Authorization"), "Bearer ")); err != nil {
		t.Errorf("the app's Authorization is not Canyon's token: %v", err)
	}
}

// A key set that answers 200 with no keys never replaces the keys held: a token under them still verifies.
func TestAnEmptyKeySetNeverReplacesTheKeysHeld(t *testing.T) {
	g := newGateway(t)
	keys := &keySet{url: g.srv.URL + "/keys", client: http.DefaultClient}
	if err := keys.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.keys.Keys = []jose.JSONWebKey{}
	g.mu.Unlock()
	keys.fetched = time.Now().Add(-time.Minute)
	_, _ = keys.VerifySignature(context.Background(), sign(newKey(), "k9", g.claims("dana@meridian.example"))) // an unknown kid fetches again
	if n := g.fetches.Load(); n != 2 {
		t.Fatalf("%d fetches, want 2", n)
	}
	if _, err := keys.VerifySignature(context.Background(), g.token("dana@meridian.example")); err != nil {
		t.Fatalf("after an empty key set, a token under the keys held: %v", err)
	}
}

// Other gateways' word on who the person is never reaches the app, by each one's prefix, however a client spells it
// (#2460): Google IAP's, Cloudflare Access's, Azure App Service's and Pomerium's. An app moved from behind one may
// still trust them; only Canyon's token says who the person is.
func TestOtherGatewaysIdentityHeadersNeverReachTheApp(t *testing.T) {
	for prefix, sent := range map[string][]string{
		"X-Goog-Authenticated-User-":   {"X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id", "x-goog-authenticated-user-EMAIL"},
		"Cf-Access-":                   {"Cf-Access-Authenticated-User-Email", "Cf-Access-Jwt-Assertion", "CF-ACCESS-JWT-ASSERTION"},
		"X-Ms-Client-Principal":        {"X-Ms-Client-Principal", "X-Ms-Client-Principal-Name", "X-MS-CLIENT-PRINCIPAL-ID", "x-ms-client-principal-idp"},
		"X-Pomerium-":                  {"X-Pomerium-Jwt-Assertion", "X-Pomerium-Claim-Email", "X-POMERIUM-CLAIM-GROUPS"},
		"X-Vouch-":                     {"X-Vouch-User", "X-Vouch-Idp-Claims-Email", "x-vouch-success"},
		"Teleport-Jwt-Assertion":       {"Teleport-Jwt-Assertion", "teleport-jwt-assertion", "TELEPORT-JWT-ASSERTION"},
		"X-Goog-Iap-Jwt-Assertion":     {"X-Goog-Iap-Jwt-Assertion", "x-goog-iap-jwt-assertion", "X-GOOG-IAP-JWT-ASSERTION"},
		"X-Ms-Token-":                  {"X-Ms-Token-Aad-Id-Token", "X-Ms-Token-Aad-Access-Token", "x-ms-token-github-access-token"},
		"X-Authentik-":                 {"X-Authentik-Username", "X-Authentik-Email", "X-AUTHENTIK-GROUPS", "x-authentik-jwt"},
		"Tailscale-User-":              {"Tailscale-User-Login", "Tailscale-User-Name", "TAILSCALE-USER-PROFILE-PIC"},
		"Remote-Email, -Groups, -Name": {"Remote-Email", "Remote-Groups", "Remote-Name", "REMOTE-EMAIL", "remote-groups"},
	} {
		t.Run(prefix, func(t *testing.T) {
			r := newRig(t, choice1)
			req := httptest.NewRequest("GET", "http://claims-portal.preview.meridian.example/", nil)
			for _, h := range sent {
				req.Header[h] = []string{"boss@meridian.example"} // as the wire spelled it, never canonicalised
			}
			req.Header.Set("Authorization", "Bearer "+r.g.token("dana@meridian.example"))
			rec := httptest.NewRecorder()
			r.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			for name, v := range <-r.seen {
				if slices.Contains(v, "boss@meridian.example") {
					t.Errorf("%s reached the app: %q", name, v)
				}
			}
		})
	}
}
