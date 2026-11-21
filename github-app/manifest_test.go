package githubapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

const public = "https://canyon.acme.example"

// conversion is the App the fake GitHub converts the code "good" into: a key and secrets drawn per test.
type conversion struct {
	pem, secret, clientSecret string
	calls                     atomic.Int32
}

// registration is a Registration over its own schema and key file, against a fake GitHub that
// converts "good" as canyon-acme, "badslug" as an App whose slug no URL can carry, and nothing else.
func registration(t *testing.T) (*Registration, *conversion, *pgxpool.Pool) {
	t.Helper()
	pool, _ := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := &conversion{pem: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), secret: rand.Text(), clientSecret: rand.Text()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		app, found := map[string]struct {
			id                    int
			slug, owner, contents string
		}{"/app-manifests/good/conversions": {9001, "canyon-acme", "acme", "read"}, "/app-manifests/badslug/conversions": {9001, "canyon/../acme", "acme", "read"},
			"/app-manifests/keeper/conversions": {9001, "canyon-acme", "acme", "write"}, "/app-manifests/personal/conversions": {9004, "canyon-dana", "acme", "read"},
			"/app-manifests/labs/conversions": {9002, "canyon-acme-labs", "acme-labs", "read"}, "/app-manifests/again/conversions": {9003, "canyon-acme-2", "Acme", "read"}}[r.URL.Path]
		if !found || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		kind := map[bool]string{true: "User", false: "Organization"}[app.id == 9004]
		_ = json.NewEncoder(w).Encode(map[string]any{"id": app.id, "slug": app.slug, "owner": map[string]any{"login": app.owner, "id": map[string]int{"acme": 4242, "acme-labs": 4243}[strings.ToLower(app.owner)], "type": kind},
			"permissions": map[string]string{"checks": "write", "contents": app.contents, "metadata": "read"},
			"client_id":   "Iv1.acme", "client_secret": c.clientSecret, "pem": c.pem, "webhook_secret": c.secret})
	}))
	t.Cleanup(srv.Close)
	return &Registration{Pool: pool, Vault: keys, API: srv.URL, Public: public, Cookie: "__Host-canyon_github_state", Secure: true,
		Actor: func(context.Context) events.Actor { return events.Actor{Kind: "person", Subject: "sub-dana"} },
		Log:   log.New(&bytes.Buffer{}, "", 0)}, c, pool
}

// begin asks for the form for acme, an org that holds only Canyon apps and keeps nothing, as a request
// whose Host is not this host's, and answers the cookie's value.
func begin(g *Registration) (*httptest.ResponseRecorder, string) {
	return beginFor(g, "org=acme&imports_new=true&keeps_code=false")
}

func beginFor(g *Registration, query string) (*httptest.ResponseRecorder, string) {
	rec := httptest.NewRecorder()
	g.New(rec, httptest.NewRequest(http.MethodGet, "http://evil.example/api/v1/github/app/new?"+query, nil))
	for _, c := range rec.Result().Cookies() {
		return rec, c.Value
	}
	return rec, ""
}

// finish returns to the callback with the cookie holding cookieState and the query naming state and code.
func finish(g *Registration, cookieState, state, code string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/app/callback?code="+code+"&state="+state, nil)
	if cookieState != "" {
		req.AddCookie(&http.Cookie{Name: g.Cookie, Value: cookieState}) //nolint:gosec // test: a request's cookie carries no attributes
	}
	rec := httptest.NewRecorder()
	g.Callback(rec, req)
	return rec
}

// The form posts the manifest to GitHub with the state its cookie holds; every URL in the manifest is
// the host's public URL whatever Host the request named, and the org's name is escaped into the page
// and cut on a rune boundary to GitHub's 34-character cap.
func TestTheFormPostsAManifestBuiltFromThePublicURLNeverTheRequestsHost(t *testing.T) {
	g, _, _ := registration(t)
	rec, state := begin(g)
	page, c := rec.Body.String(), rec.Result().Cookies()[0]
	state, _, _ = strings.Cut(state, ".") // the cookie carries the registration's two answers after the state
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || len(state) != 64 {
		t.Fatalf("GET new: %d %q, state %q", rec.Code, rec.Header().Get("Content-Type"), state)
	}
	if c.Name != g.Cookie || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("the state cookie carries the session cookie's attributes: %+v", c)
	}
	if !strings.Contains(page, `action="https://github.com/organizations/acme/settings/apps/new?state=`+state+`"`) || strings.Contains(page, "evil.example") {
		t.Fatalf("the form posts elsewhere than the org's own registration, or names the request's Host: %s", page)
	}
	_, value, _ := strings.Cut(page, `name="manifest" value="`)
	value, _, _ = strings.Cut(value, `"`)
	var got map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(value)), &got); err != nil {
		t.Fatalf("the manifest field is not JSON: %v in %s", err, page)
	}
	want := map[string]any{
		"name": "Canyon — acme", "url": public, "redirect_url": public + "/api/v1/github/app/callback",
		"hook_attributes": map[string]any{"url": public + "/webhooks/github"}, "public": false,
		"callback_urls":       []any{public + "/api/v1/github/link/callback"},
		"default_permissions": map[string]any{"checks": "write", "contents": "read", "metadata": "read", "pull_requests": "read", "security_events": "read"},
		"default_events":      []any{"pull_request", "push", "repository"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest\n got %v\nwant %v", got, want)
	}
}

// The App is the org's (#1914): an org that keeps code asks to write its contents and make its
// repositories, and never workflows; a name past GitHub's 34 characters is cut.
func TestTheManifestPostsToTheOrgAndAsksToWriteOnlyWhereItKeepsCode(t *testing.T) {
	g, _, _ := registration(t)
	long := "a" + strings.Repeat("-b", 19) // 39 characters, the most a login has
	for query, want := range map[string]map[string]any{
		"org=acme&imports_new=false&keeps_code=false":       {"checks": "write", "contents": "read", "metadata": "read", "pull_requests": "read", "security_events": "read"},
		"org=" + long + "&imports_new=true&keeps_code=true": {"checks": "write", "contents": "write", "administration": "write", "metadata": "read", "pull_requests": "read", "security_events": "read"},
	} {
		org, _, _ := strings.Cut(strings.TrimPrefix(query, "org="), "&")
		rec, _ := beginFor(g, query)
		m := manifestOf(t, rec.Body.String())
		if rec.Code != 200 || !reflect.DeepEqual(m["default_permissions"], want) || m["name"] != string([]rune("Canyon — " + org)[:min(34, len([]rune("Canyon — "+org)))]) ||
			!strings.Contains(rec.Body.String(), `action="https://github.com/organizations/`+org+`/settings/apps/new?state=`) {
			t.Errorf("%s: %d %v", query, rec.Code, m)
		}
	}
}

// GitHub refuses a manifest that subscribes to an event its permissions don't cover ("Default events are not supported
// by permissions: pull_request"), so no one could register the App. Each event the manifest names needs the permission
// GitHub's "Webhook events and payloads" lists for a GitHub App, at read or above; an event missing here fails too.
func TestEveryEventTheManifestNamesHasThePermissionGitHubRequires(t *testing.T) {
	needs := map[string]string{ // docs.github.com/en/webhooks/webhook-events-and-payloads, each event's Availability
		"pull_request": "pull_requests",
		"push":         "contents",
		"repository":   "metadata",
	}
	for _, keeps := range []bool{false, true} {
		var m struct {
			Permissions map[string]string `json:"default_permissions"`
			Events      []string          `json:"default_events"`
		}
		if err := json.Unmarshal(manifest("https://canyon.acme.example", "acme", keeps), &m); err != nil || len(m.Events) == 0 {
			t.Fatalf("keeps code %v: %v, %+v", keeps, err, m)
		}
		for _, e := range m.Events {
			permission, known := needs[e]
			if level := m.Permissions[permission]; !known || (level != "read" && level != "write") {
				t.Errorf("keeps code %v: the event %s needs the %q permission (known %v), and the manifest grants %v", keeps, e, permission, known, m.Permissions)
			}
		}
	}
}

// What the form refuses, it refuses before a cookie is set or a page served.
func TestTheFormRefusesAnOrgItCannotNameAndAHostGitHubCannotReach(t *testing.T) {
	g, _, _ := registration(t)
	for query, want := range map[string]string{
		"org=-acme&imports_new=true&keeps_code=false": "invalid_org", "org=ac/me&imports_new=true&keeps_code=false": "invalid_org",
		"org=" + strings.Repeat("a", 40) + "&imports_new=true&keeps_code=false": "invalid_org", "imports_new=true&keeps_code=false": "invalid_org",
		"org=acme&keeps_code=false": "bad_request", "org=acme&imports_new=yes&keeps_code=false": "bad_request",
	} {
		rec, cookie := beginFor(g, query)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"error":"`+want+`"`) || cookie != "" {
			t.Errorf("%s: %d %s", query, rec.Code, rec.Body)
		}
	}
	for at, reachable := range map[string]bool{"http://localhost:7777": false, "http://claims.localhost:7777": false, "http://127.0.0.1:7777": false,
		"http://[::1]:7777": false, "https://canyon.acme.example": true, "http://10.0.0.5:7777": true} {
		g.Public = at
		if rec, cookie := begin(g); Reachable(at) != reachable || rec.Code != 200 || cookie == "" { // the dashboard's hint, never the route's refusal: a stub GitHub reaches localhost
			t.Errorf("%s: reachable %v, %d %s", at, Reachable(at), rec.Code, rec.Body)
		}
	}
}

// GitHub's answer is the org the form named, or nothing is stored: an App finished on a personal
// account, or on another org, is the registration this route exists to replace.
func TestTheCallbackRefusesAnAppNotOwnedByTheOrg(t *testing.T) {
	g, _, pool := registration(t)
	for _, query := range []string{"org=acme-labs&imports_new=false&keeps_code=false", "org=someone&imports_new=false&keeps_code=false"} {
		_, cookie := beginFor(g, query)
		if rec := finish(g, cookie, onlyState(cookie), "good"); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"auth_failed"`) {
			t.Errorf("%s answered by acme's App: %d %s", query, rec.Code, rec.Body)
		}
	}
	_, cookie := begin(g)
	if rec := finish(g, cookie, onlyState(cookie), "personal"); rec.Code != 400 {
		t.Errorf("an App on a person's account named acme: %d %s", rec.Code, rec.Body)
	}
	if n := rows(t, pool, "SELECT count(*) FROM github_app"); n != 0 {
		t.Fatalf("%d App(s) stored", n)
	}
}

// One App per org, and one org keeps code: a second registration of either is 409 at the form and at
// the callback, and another org registers beside the first.
func TestOneAppPerOrgAndOneOrgKeepsCode(t *testing.T) {
	g, _, pool := registration(t)
	_, meanwhile := beginFor(g, "org=Acme&imports_new=false&keeps_code=false") // a second admin's form, before acme has an App
	_, state := beginFor(g, "org=acme&imports_new=true&keeps_code=true")
	if rec := finish(g, state, onlyState(state), "keeper"); rec.Code != 302 { // keeps_code is what GitHub granted: contents write
		t.Fatalf("acme: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := beginFor(g, "org=ACME&imports_new=true&keeps_code=false"); rec.Code != 409 || !strings.Contains(rec.Body.String(), `"error":"exists"`) {
		t.Errorf("acme again, in capitals: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := beginFor(g, "org=acme-labs&imports_new=false&keeps_code=true"); rec.Code != 409 || !strings.Contains(rec.Body.String(), `"error":"keeping_org_exists"`) {
		t.Errorf("a second keeping org: %d %s", rec.Code, rec.Body)
	}
	_, state = beginFor(g, "org=acme-labs&imports_new=false&keeps_code=false")
	if rec := finish(g, state, onlyState(state), "labs"); rec.Code != 302 || rec.Header().Get("Location") != "https://github.com/apps/canyon-acme-labs/installations/new" {
		t.Fatalf("acme-labs: %d %s", rec.Code, rec.Body)
	}
	if rec := finish(g, meanwhile, onlyState(meanwhile), "again"); rec.Code != 409 || !strings.Contains(rec.Body.String(), `"error":"exists"`) {
		t.Errorf("the second admin's callback, for an org that has an App by then: %d %s", rec.Code, rec.Body)
	}
	var got string
	if err := pool.QueryRow(ctx, "SELECT string_agg(account || ' ' || keeps_code || ' ' || imports_new, ', ' ORDER BY app_id) FROM github_app").Scan(&got); err != nil || got != "acme true true, acme-labs false false" {
		t.Fatalf("the Apps: %q %v", got, err)
	}
}

// onlyState is the state the query carries: the cookie's value before its answers.
func onlyState(cookie string) string {
	state, _, _ := strings.Cut(cookie, ".")
	return state
}

func manifestOf(t *testing.T, page string) map[string]any {
	t.Helper()
	_, value, _ := strings.Cut(page, `name="manifest" value="`)
	value, _, _ = strings.Cut(value, `"`)
	var m map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(value)), &m); err != nil {
		t.Fatalf("the manifest field is not JSON: %v in %s", err, page)
	}
	return m
}

// Only the cookie's whole state reaches GitHub: a prefix, another value, none, or no cookie is the same
// 400 with no detail and exchanges nothing, as is a conversion GitHub refuses or one with an unusable
// slug. None writes a row, and the cookie is cleared on every answer.
func TestARefusedRegistrationIsOne400AndWritesNothing(t *testing.T) {
	g, conv, pool := registration(t)
	_, cookie := begin(g)
	state := onlyState(cookie)
	for name, c := range map[string]struct{ cookie, state, code string }{
		"a prefix": {cookie, state[:32], "good"}, "another": {cookie, rand.Text(), "good"}, "none": {cookie, "", "good"}, "no cookie": {"", state, "good"},
		"the whole cookie":   {cookie, cookie, "good"},
		"refused conversion": {cookie, state, "stale"}, "unusable slug": {cookie, state, "badslug"},
	} {
		rec := finish(g, c.cookie, c.state, c.code)
		if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != `{"error":"auth_failed","message":"the GitHub App registration could not be finished"}` {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if cl := rec.Result().Cookies(); len(cl) != 1 || cl[0].Name != g.Cookie || cl[0].MaxAge >= 0 {
			t.Errorf("%s: the state cookie is not cleared: %v", name, cl)
		}
	}
	if n := conv.calls.Load(); n != 2 {
		t.Fatalf("the code was exchanged %d time(s), want only for the two good states", n)
	}
	if n := rows(t, pool, "SELECT count(*) FROM github_app") + rows(t, pool, "SELECT count(*) FROM audit_events"); n != 0 {
		t.Fatalf("a refused registration left %d row(s)", n)
	}
}

// The registration stores the key, the webhook secret and the client secret sealed (#2228), audits once,
// sends the admin to install the App, and opens again for the courier. A second one is 409.
func TestARegistrationIsStoredSealedAuditedAndSentToTheInstallPage(t *testing.T) {
	g, conv, pool := registration(t)
	_, cookie := begin(g)
	if rec := finish(g, cookie, onlyState(cookie), "good"); rec.Code != 302 || rec.Header().Get("Location") != "https://github.com/apps/canyon-acme/installations/new" {
		t.Fatalf("callback: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	var row string
	var keyRef int
	var pemKey, sealedPEM, hookKey, sealedSecret, clientKey, sealedClient []byte
	if err := pool.QueryRow(ctx, "SELECT row_to_json(github_app)::text, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, sealed_client_key, sealed_client_secret FROM github_app").Scan(&row, &keyRef, &pemKey, &sealedPEM, &hookKey, &sealedSecret, &clientKey, &sealedClient); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealedPEM, []byte("PRIVATE KEY")) || bytes.Contains(sealedSecret, []byte(conv.secret)) || strings.Contains(row, conv.clientSecret) || !strings.Contains(row, `"app_id":9001,"slug":"canyon-acme","client_id":"Iv1.acme"`) {
		t.Fatalf("the row holds a credential in the clear, or not the App: %s", row)
	}
	key, err := g.Vault.Open(keyRef, "github", "app", "private_key", pemKey, sealedPEM)
	secret, err2 := g.Vault.Open(keyRef, "github", "app", "webhook_secret", hookKey, sealedSecret)
	client, err3 := g.Vault.Open(keyRef, "github", "app", "client_secret", clientKey, sealedClient)
	if err != nil || err2 != nil || err3 != nil || string(key) != conv.pem || string(secret) != conv.secret || string(client) != conv.clientSecret {
		t.Fatalf("the seals do not open to what GitHub answered: %v %v %v", err, err2, err3)
	}
	var audit string
	if err := pool.QueryRow(ctx, "SELECT action || ' ' || actor_subject || ' ' || detail::text FROM audit_events").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != `github app registered sub-dana {"slug": "canyon-acme", "app_id": 9001, "account": "acme", "keeps_code": false, "imports_new": true}` {
		t.Fatalf("audit row: %s", audit)
	}
	if rec, _ := begin(g); rec.Code != 409 || strings.TrimSpace(rec.Body.String()) != `{"error":"exists","message":"Canyon's App is already registered on acme"}` {
		t.Fatalf("a second form: %d %s", rec.Code, rec.Body)
	}
	if rec := finish(g, cookie, onlyState(cookie), "good"); rec.Code != 409 || rows(t, pool, "SELECT count(*) FROM audit_events") != 1 {
		t.Fatalf("a second callback: %d %s", rec.Code, rec.Body)
	}
}

func rows(t *testing.T, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An App registered before Canyon kept its client secret takes one an admin supplies (#2228), sealed under
// the active key version with the App's key and webhook secret re-wrapped to it, so all three open under the
// row's one key_ref after a new key version too, and again when the secret is regenerated on GitHub.
func TestASuppliedClientSecretOpensBesideTheAppsOtherSealsUnderOneVersion(t *testing.T) {
	g, conv, pool := registration(t)
	path := filepath.Join(t.TempDir(), "kek")
	keys, err := vault.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g.Vault = keys
	if _, cookie := begin(g); finish(g, cookie, onlyState(cookie), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	if _, err := pool.Exec(ctx, "UPDATE github_app SET sealed_client_key = NULL, sealed_client_secret = NULL"); err != nil { // as an App from before #2228
		t.Fatal(err)
	}
	if _, err := keys.AddVersion(path); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"supplied-secret-1", "regenerated-secret-2"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/github/orgs/Acme/client-secret", strings.NewReader(`{"client_secret":"`+secret+`"}`))
		req.SetPathValue("account", "Acme")
		g.SetClientSecret(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"links_accounts":true`) || strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("supplying %s: %d %s", secret, rec.Code, rec.Body)
		}
		var keyRef int
		var pemKey, sealedPEM, hookKey, sealedHook, clientKey, sealedClient []byte
		if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, sealed_client_key, sealed_client_secret FROM github_app").
			Scan(&keyRef, &pemKey, &sealedPEM, &hookKey, &sealedHook, &clientKey, &sealedClient); err != nil {
			t.Fatal(err)
		}
		key, err1 := keys.Open(keyRef, "github", "app", "private_key", pemKey, sealedPEM)
		hook, err2 := keys.Open(keyRef, "github", "app", "webhook_secret", hookKey, sealedHook)
		client, err3 := keys.Open(keyRef, "github", "app", "client_secret", clientKey, sealedClient)
		if keyRef != 2 || err1 != nil || err2 != nil || err3 != nil || string(key) != conv.pem || string(hook) != conv.secret || string(client) != secret {
			t.Fatalf("after %s the row is under version %d and its seals open %v %v %v", secret, keyRef, err1, err2, err3)
		}
	}
	if n := rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'set github client secret' AND detail = '{\"account\": \"acme\"}'"); n != 2 {
		t.Fatalf("each supply is one audit row naming the org as stored, and never the secret: %d", n)
	}
}
