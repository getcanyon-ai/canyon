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
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
			back, client, verifier := authorized(t, browser, c.who, answer, "")
			if answer == "deny" {
				if back.Query().Get("error") != "access_denied" || back.Query().Get("state") != "deny" {
					t.Errorf("%s refused, and the client was sent %s", c.who, back)
				}
				continue
			}
			if _, scope := redeemed(t, back, client, verifier); scope != "read" {
				t.Errorf("%s's token is for %q, want read", c.who, scope)
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

// agentRedirect is where the e2e agent receives its sign-in: a loopback port nothing listens on.
const agentRedirect = "http://127.0.0.1:47123/callback"

// authorized registers an MCP client, sends who's browser to authorize for scope ("" asking none) with answer as the
// state, and has them answer the consent page: the client's redirect they were sent back to, the client and the verifier.
func authorized(t *testing.T, browser *http.Client, who, answer, scope string) (back *url.URL, client, verifier string) {
	t.Helper()
	res, err := http.Post(host+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Canyon e2e agent","redirect_uris":["`+agentRedirect+`"]}`)) //nolint:noctx,gosec // the stack's own host
	must(t, err)
	var registered struct {
		ID string `json:"client_id"`
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &registered) != nil {
		t.Fatalf("register: %d %s", res.StatusCode, raw)
	}
	verifier = strings.Repeat("e2e-verifier-", 4)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {registered.ID}, "redirect_uri": {agentRedirect}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}, "resource": {host + "/mcp"}, "state": {answer}}
	if scope != "" {
		q.Set("scope", scope)
	}
	staying := &http.Client{Jar: browser.Jar, Transport: browser.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = staying.Get(host + "/oauth/authorize?" + q.Encode()) //nolint:noctx // the client's timeout bounds it
	must(t, err)
	_ = res.Body.Close()
	at, err := res.Location()
	if err != nil || !strings.HasPrefix(at.Fragment, "/oauth?request=") {
		t.Fatalf("authorize sent %s to %v (%d)", who, at, res.StatusCode)
	}
	return consent(t, browser, strings.TrimPrefix(at.Fragment, "/oauth?request="), who, answer), registered.ID, verifier
}

// redeemed is the token the code back carries redeems for: the access token and its scope.
func redeemed(t *testing.T, back *url.URL, client, verifier string) (access, scope string) {
	t.Helper()
	res, err := http.PostForm(host+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client}, //nolint:noctx,gosec // the stack's own host
		"redirect_uri": {agentRedirect}, "code_verifier": {verifier}})
	must(t, err)
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var tok struct {
		Access string `json:"access_token"`
		Scope  string `json:"scope"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &tok) != nil || tok.Access == "" {
		t.Fatalf("the token: %d %s", res.StatusCode, raw)
	}
	return tok.Access, tok.Scope
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

// bearer is a round tripper that carries an access token, as an MCP client does on every request to /mcp.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// Canyon's own /mcp over the stack with sign-in (design/specs/mcp.md §6 item 7): an MCP client signs in through
// consent, then lists the tools and reads the org's rules as the person; with read alone, promote is refused for the
// scope, before anything is asked of the app.
func TestMCPRemoteOverTheStack(t *testing.T) {
	onboard(t, signIn(t, admin))
	browser := signIn(t, teammate)
	connect := func(scope string) *mcp.ClientSession {
		t.Helper()
		back, client, verifier := authorized(t, browser, teammate, "allow", scope)
		access, _ := redeemed(t, back, client, verifier)
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "canyon-e2e"}, nil).Connect(t.Context(),
			&mcp.StreamableClientTransport{Endpoint: host + "/mcp", HTTPClient: &http.Client{Transport: bearer(access)}}, nil)
		must(t, err)
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	cs := connect("read")
	tools, err := cs.ListTools(t.Context(), nil)
	must(t, err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"canyon_rules", "canyon_status", "canyon_logs", "canyon_promote", "canyon_check_ref", "canyon_deploy_ref"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %q, want %s among them", names, want)
		}
	}
	rules, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "canyon_rules", Arguments: map[string]any{}})
	must(t, err)
	if rules.IsError || len(rules.Content) == 0 || !strings.Contains(rules.Content[0].(*mcp.TextContent).Text, "Canyon's settings for this org.") {
		t.Errorf("canyon_rules: %+v", rules)
	}
	if promote, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "canyon_promote", Arguments: map[string]any{"app": "claims-portal"}}); err == nil {
		t.Errorf("a read token's promote was answered: %+v", promote) // 403 insufficient_scope, the step-up
	}
}
