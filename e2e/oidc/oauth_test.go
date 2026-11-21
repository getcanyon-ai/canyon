//go:build oidc

package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The consent journey (design/AUTH.md §4.7, design/specs/mcp.md §6 item 6): an MCP client registers, sends a person's
// browser to authorize, and the person allows it, or doesn't, on the dashboard's consent page in Chromium (e2e/ui's
// consent project). Run as the admin and as a member: each allows, the code redeems for a session the person then sees
// in their sessions list under the client's name, and the member also refuses one, which goes back as access_denied.
func TestAnMCPClientSignsInThroughConsent(t *testing.T) {
	onboard(t, signIn(t, admin))
	for _, c := range []struct {
		who     string
		answers []string
	}{{admin, []string{"allow"}}, {teammate, []string{"allow", "deny"}}} {
		browser := signIn(t, c.who)
		for _, answer := range c.answers {
			const redirect = "http://127.0.0.1:47123/callback"
			res, err := http.Post(host+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Canyon e2e agent","redirect_uris":["`+redirect+`"]}`)) //nolint:noctx,gosec // the stack's own host
			must(t, err)
			var client struct {
				ID string `json:"client_id"`
			}
			raw, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &client) != nil {
				t.Fatalf("register: %d %s", res.StatusCode, raw)
			}
			verifier := strings.Repeat("e2e-verifier-", 4)
			sum := sha256.Sum256([]byte(verifier))
			q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
				"code_challenge_method": {"S256"}, "resource": {host + "/mcp"}, "state": {answer}}
			staying := &http.Client{Jar: browser.Jar, Transport: browser.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err = staying.Get(host + "/oauth/authorize?" + q.Encode()) //nolint:noctx // the client's timeout bounds it
			must(t, err)
			_ = res.Body.Close()
			at, err := res.Location()
			if err != nil || !strings.HasPrefix(at.Fragment, "/oauth?request=") {
				t.Fatalf("authorize sent %s to %v (%d)", c.who, at, res.StatusCode)
			}
			back := consent(t, browser, strings.TrimPrefix(at.Fragment, "/oauth?request="), c.who, answer)
			if answer == "deny" {
				if back.Query().Get("error") != "access_denied" || back.Query().Get("state") != "deny" {
					t.Errorf("%s refused, and the client was sent %s", c.who, back)
				}
				continue
			}
			res, err = http.PostForm(host+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client.ID}, //nolint:noctx,gosec // the stack's own host
				"redirect_uri": {redirect}, "code_verifier": {verifier}})
			must(t, err)
			raw, _ = io.ReadAll(res.Body)
			_ = res.Body.Close()
			var tok struct {
				Access string `json:"access_token"`
				Scope  string `json:"scope"`
			}
			if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &tok) != nil || tok.Access == "" || tok.Scope != "read" {
				t.Fatalf("%s's token: %d %s", c.who, res.StatusCode, raw)
			}
			var list struct {
				Sessions []struct {
					Flow   string `json:"flow"`
					Client string `json:"client"`
				} `json:"sessions"`
			}
			if code := get(t, browser, host+"/api/v1/sessions", &list); code != http.StatusOK || !strings.Contains(strings.Join(func() []string {
				var out []string
				for _, s := range list.Sessions {
					out = append(out, s.Flow+":"+s.Client)
				}
				return out
			}(), ","), "oauth:Canyon e2e agent") {
				t.Errorf("%s's sessions: %d %+v, want the agent among them", c.who, code, list)
			}
		}
	}
}

// consent runs e2e/ui's consent project in Chromium as who, their session cookie handed over: it reads the consent
// page for request and answers it, and the client's redirect it was sent to comes back.
func consent(t *testing.T, browser *http.Client, request, who, answer string) *url.URL {
	t.Helper()
	u, err := url.Parse(host)
	must(t, err)
	var session *http.Cookie
	for _, c := range browser.Jar.Cookies(u) {
		if strings.HasSuffix(c.Name, "canyon_session") {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("%s's browser holds no session", who)
	}
	out := filepath.Join(t.TempDir(), "redirect")
	cmd := exec.Command("npx", "playwright", "test", "--project=consent") //nolint:gosec // the harness's own runner
	cmd.Dir = filepath.Join("..", "ui")
	cmd.Env = append(os.Environ(), "E2E_CANYON_URL="+host, "CONSENT_COOKIE_NAME="+session.Name, "CONSENT_COOKIE_VALUE="+session.Value,
		"CONSENT_REQUEST="+request, "CONSENT_WHO="+who, "CONSENT_ANSWER="+answer, "CONSENT_OUT="+out)
	logged, err := cmd.CombinedOutput()
	t.Logf("playwright, the consent page as %s (%s):\n%s", who, answer, logged)
	if err != nil {
		t.Fatalf("the consent page as %s: %v", who, err)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // the test's own temp file
	must(t, err)
	back, err := url.Parse(strings.TrimSpace(string(raw)))
	must(t, err)
	return back
}
