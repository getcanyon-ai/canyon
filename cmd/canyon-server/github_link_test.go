package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	githubapp "github.com/getcanyon-ai/canyon/github-app"
)

const (
	linkCallback  = "/api/v1/github/link/callback"
	notConfigured = `{"error":"not_configured","message":"no GitHub App here can link accounts: none was registered since Canyon could link them"}`
	linkInBrowser = `{"error":"dashboard_required","message":"link your GitHub account in Canyon's dashboard"}`
	linkFailed    = `{"error":"auth_failed","message":"your GitHub account could not be linked"}`
)

// fakeGitHubSignIn is GitHub as a person linking their account meets it: the code "good", exchanged with
// acme's App's client id and the secret an admin supplied and this host's callback, is a user token whose GET /user is dana-gh,
// 583231; any other exchange is refused as GitHub refuses one, with 200 and an error.
func fakeGitHubSignIn(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["client_id"] != "Iv1" || in["client_secret"] != "client-secret-of-acme" || in["code"] != "good" ||
			in["redirect_uri"] != testPublic+linkCallback || r.Header.Get("Accept") != "application/json" {
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"user token of dana","token_type":"bearer"}`))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user token of dana" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":583231,"login":"dana-gh"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	testGitHubAPI = srv.URL
	t.Cleanup(func() { testGitHubAPI = "" })
}

// noFollow is c answering each redirect rather than following it.
func noFollow(c *http.Client) *http.Client {
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// A person links their own GitHub account by signing in to GitHub through Canyon's App (#2228): only from a
// browser, only through an App that kept its client secret, and only with the state the link began with; a
// person who cancels on GitHub links nothing. The link is one audit row, and the fleet's viewer names it.
func TestAPersonLinksTheirOwnGitHubAccount(t *testing.T) {
	fakeGitHubSignIn(t)
	pool, ctx, base := approverHost(t)
	installedApp(t, ctx, pool, vaultOf(t, base)) // registered before #2228: no client secret
	dana := noFollow(signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false))
	link := base + "/api/v1/github/link"
	viewer := func() string {
		t.Helper()
		_, page, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", "")
		return string(mustJSON(page["viewer"]))
	}
	if code, _, _ := do(t, dana, http.MethodGet, link, ""); code != 409 || lastBody != notConfigured || viewer() != `{"display":"sub-dana","github_link":false,"github_logins":[],"role":"admin"}` {
		t.Fatalf("an App with no client secret links nothing: %d %s, viewer %s", code, lastBody, viewer())
	}
	// the App was registered before Canyon kept its client secret: an admin supplies one its owner generated on GitHub
	secretAt := base + "/api/v1/github/orgs/acme/client-secret"
	for _, c := range []struct {
		who, path, body string
		c               *http.Client
		code            int
		want            string
	}{
		{"a member", secretAt, `{"client_secret":"a-secret-carl-generated"}`, noFollow(signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)), 403, "admin_required"},
		{"a secret with a space", secretAt, `{"client_secret":"client secret of acme"}`, dana, 400, "invalid_secret"},
		{"no secret", secretAt, `{"client_secret":""}`, dana, 400, "invalid_secret"},
		{"an org with no App", base + "/api/v1/github/orgs/globex/client-secret", `{"client_secret":"x"}`, dana, 404, "not_found"},
	} {
		if code, body, _ := do(t, c.c, http.MethodPut, c.path, c.body); code != c.code || body["error"] != c.want {
			t.Errorf("%s: %d %s", c.who, code, lastBody)
		}
	}
	if code, body, _ := do(t, dana, http.MethodPut, secretAt, `{"client_secret":"client-secret-of-acme"}`); code != 200 || body["org"].(map[string]any)["links_accounts"] != true || strings.Contains(lastBody, "client-secret-of-acme") {
		t.Fatalf("the admin supplies the App's client secret: %d %s", code, lastBody)
	}
	app, err := githubapp.LoadApp(ctx, pool, vaultOf(t, base), 42)
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'set github client secret' AND actor_subject = 'sub-dana' AND detail = '{"account": "acme"}'`); err != nil || string(app.WebhookSecret) != "whsec" || n != 1 {
		t.Fatalf("the App's other seals open after the secret is sealed beside them (%v), and one audit row names the org alone: %d", err, n)
	}
	if code, _, _ := do(t, cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), http.MethodGet, link, ""); code != 403 || lastBody != linkInBrowser {
		t.Fatalf("a CLI session links: %d %s", code, lastBody)
	}
	begin := func() string {
		t.Helper()
		code, _, resp := do(t, dana, http.MethodGet, link, "")
		to, err := neturl.Parse(resp.Header.Get("Location"))
		q := to.Query()
		if code != 302 || err != nil || to.Path != "/login/oauth/authorize" || q.Get("client_id") != "Iv1" || q.Get("redirect_uri") != testPublic+linkCallback || len(q.Get("state")) != 64 {
			t.Fatalf("the link sends the person to GitHub's sign-in for acme's App: %d %s", code, resp.Header.Get("Location"))
		}
		return q.Get("state")
	}
	callback := func(query string) (int, string) {
		t.Helper()
		code, _, resp := do(t, dana, http.MethodGet, base+linkCallback+"?"+query, "")
		return code, resp.Header.Get("Location")
	}
	begin()
	if code, _ := callback("code=good&state=" + strings.Repeat("ab", 32)); code != 400 || lastBody != linkFailed {
		t.Fatalf("a state not the link's: %d %s", code, lastBody)
	}
	if code, to := callback("error=access_denied&state=" + begin()); code != 302 || to != "/" {
		t.Fatalf("cancelled on GitHub: %d %s", code, to)
	}
	if code, _ := callback("code=stale&state=" + begin()); code != 400 || lastBody != linkFailed {
		t.Fatalf("a code GitHub refuses: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM github_links"); n != 0 {
		t.Fatalf("a refused or cancelled link wrote %d link(s)", n)
	}
	state := begin()
	if code, to := callback("code=good&state=" + state); code != 302 || to != "/" {
		t.Fatalf("the link: %d %s %s", code, to, lastBody)
	}
	if code, _ := callback("code=good&state=" + state); code != 400 || lastBody != linkFailed {
		t.Fatalf("the same state again, its cookie spent: %d %s", code, lastBody)
	}
	if code, to := callback("code=good&state=" + begin()); code != 302 || to != "/" {
		t.Fatalf("the link again: %d %s %s", code, to, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM github_links WHERE github_id = 583231 AND subject = 'sub-dana' AND login = 'dana-gh'`) +
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'link github account' AND actor_subject = 'sub-dana' AND detail = '{"github_id": 583231, "login": "dana-gh"}'`); n != 2 {
		t.Fatalf("one link and one audit row, however often it is made: %d", n)
	}
	if got := viewer(); got != `{"display":"sub-dana","github_link":true,"github_logins":["dana-gh"],"role":"admin"}` {
		t.Fatalf("the fleet's viewer: %s", got)
	}
}

// A push is authored by every person who linked the GitHub account that pushed (#2228): dana, an admin who
// linked the account behind gr_push, is author_cannot_approve on it and is not offered to approve it, and
// the approval she files of the same finding on carl's run covers it there and not on the push; rhea, a
// responder who linked nothing, approves the push.
func TestALinkedPushIsItsLinkersOwn(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	if _, err := pool.Exec(ctx, `UPDATE runs SET started_by_github = 583231 WHERE id = 'gr_push';
		INSERT INTO github_links (github_id, subject, login) VALUES (583231, 'sub-dana', 'dana-gh')`); err != nil {
		t.Fatal(err)
	}
	dana, rhea := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	approve := func(c *http.Client, run string) int {
		t.Helper()
		code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/"+fps[0]+"/approve", approveBody())
		return code
	}
	coveredAt := func(run, fp string) bool {
		t.Helper()
		_, body, _ := do(t, rhea, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
		for _, f := range runOf(body)["findings"].([]any) {
			if f := f.(map[string]any); f["partialFingerprints"].(map[string]any)["canyon/v1"] == fp {
				return f["approval"] != nil
			}
		}
		t.Fatalf("no finding %s on %s: %s", fp, run, lastBody)
		return false
	}
	covered := func(run string) bool { t.Helper(); return coveredAt(run, fps[0]) }
	if code := approve(dana, "gr_push"); code != 403 || lastBody != authorRefused {
		t.Fatalf("the admin approves a push by the account she linked: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"the admin who linked its account": dana, "the responder": rhea} {
		if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_push", ""); code != 200 || (runOf(body)["approvable"] == true) != (who == "the responder") {
			t.Errorf("%s reads gr_push approvable %v", who, runOf(body)["approvable"])
		}
	}
	if code := approve(dana, "gr_carl"); code != 200 || !covered("gr_carl") || covered("gr_push") {
		t.Fatalf("the admin's approval on carl's run covers the push's finding too: %d, carl's %t, the push's %t", code, covered("gr_carl"), covered("gr_push"))
	}
	if code := approve(rhea, "gr_push"); code != 200 || !covered("gr_push") {
		t.Fatalf("the responder, who linked nothing, approves the push: %d %s", code, lastBody)
	}
	// a signed-in person whose subject is "anonymous" is no exception: the account they linked is still theirs
	if _, err := pool.Exec(ctx, "INSERT INTO github_links (github_id, subject, login) VALUES (583231, 'anonymous', 'anon-gh')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		VALUES ('claims-portal', 'deps.known-vuln.trivy.CVE-2018-16487', $1, 'filed on carl''s run', now() + interval '1 day', 'person', 'anonymous', 'anonymous', true)`, fps[4]); err != nil {
		t.Fatal(err)
	}
	if !coveredAt("gr_carl", fps[4]) || coveredAt("gr_push", fps[4]) {
		t.Fatalf("an approval by the subject anonymous, who linked the pushing account, covers the push: carl's %t, the push's %t", coveredAt("gr_carl", fps[4]), coveredAt("gr_push", fps[4]))
	}
}
