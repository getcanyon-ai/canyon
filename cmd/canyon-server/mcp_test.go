package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
)

// mcpCall posts one JSON-RPC message to /mcp with token as the bearer ("" for none) and the headers given.
func mcpCall(t *testing.T, base, token, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

const (
	mcpList    = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	mcpPromote = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_promote","arguments":{"app":"claims-portal"}}}`
)

// With sign-in off every caller is the anonymous admin, so /mcp hands nothing out: it names canyon mcp instead.
func TestMCPRemoteNeedsSignIn(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, body := mcpCall(t, base, "", mcpList, nil); code != http.StatusNotFound || !strings.Contains(body, `"error":"mcp_needs_sign_in"`) || !strings.Contains(body, "canyon mcp") {
		t.Errorf("sign-in off: %d %s", code, body)
	}
}

// /mcp takes an access token its own authorization server issued for it, live, and nothing else: no token is 401
// with where to get one (RFC 9728), and a revoked session's token stops at once.
func TestMCPRemoteTakesItsOwnTokensAlone(t *testing.T) {
	base, pool, ctx, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	access, _ := signedInOver(t, base, gina, register(t, base, redirect), redirect)
	code, h, _ := mcpCall(t, base, "", mcpList, nil)
	if code != http.StatusUnauthorized || h.Get("WWW-Authenticate") != `Bearer resource_metadata="http://localhost:7777/.well-known/oauth-protected-resource/mcp"` {
		t.Errorf("no token: %d %q", code, h.Get("WWW-Authenticate"))
	}
	cookie := gina.Jar.Cookies(mustURL(t, base))[0].Value // a browser session's id is never a bearer
	for name, token := range map[string]string{"a browser session's id": cookie, "a made-up token": strings.Repeat("a", 64)} {
		if code, _, _ := mcpCall(t, base, token, mcpList, nil); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, body := mcpCall(t, base, access, mcpList, nil); code != http.StatusOK || !strings.Contains(body, "canyon_rules") {
		t.Fatalf("the token: %d %s", code, body)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET access_expires_at = now() - interval '1 second' WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("an expired access token: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET access_expires_at = now() + interval '1 hour', resource = 'http://localhost:7777/other' WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("a token for another resource: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET resource = 'http://localhost:7777/mcp', revoked_at = now() WHERE flow = 'oauth'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpCall(t, base, access, mcpList, nil); code != http.StatusUnauthorized {
		t.Errorf("a revoked session's token: %d", code)
	}
}

// A browser page on another site never reaches /mcp; a native client, which sends no Origin, does. The transport is
// stateless: GET and DELETE have no session to serve.
func TestMCPRemoteRefusesAnotherSitesOrigin(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	access, _ := signedInOver(t, base, gina, register(t, base, redirect), redirect)
	for origin, want := range map[string]int{"": http.StatusOK, "http://localhost:7777": http.StatusOK, "https://evil.example": http.StatusForbidden} {
		if code, _, body := mcpCall(t, base, access, mcpList, map[string]string{"Origin": origin}); code != want {
			t.Errorf("Origin %q: %d %s, want %d", origin, code, body, want)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(t.Context(), method, base+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: %d, want 405", method, resp.StatusCode)
		}
	}
}

// A read token lists every tool but runs none that changes what runs: each act tool answers insufficient_scope, judged
// inside the tool from the call the SDK dispatched, so no spelling of the message reaches one by another parse.
func TestMCPRemoteActsOnlyWithTheActScope(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	id := register(t, base, redirect)
	read, _ := signedInOver(t, base, gina, id, redirect)
	if code, _, body := mcpCall(t, base, read, mcpList, nil); code != http.StatusOK || !strings.Contains(body, "canyon_promote") || !strings.Contains(body, "canyon_deploy_ref") {
		t.Errorf("a read token's list: %d %s", code, body)
	}
	call := func(tool string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"app":"claims-portal","ref":"main"}}}`
	}
	refused := func(name, msg string) {
		t.Helper()
		if code, _, body := mcpCall(t, base, read, msg, nil); code == http.StatusOK && !strings.Contains(body, "insufficient_scope") {
			t.Errorf("%s ran with a read token: %s", name, body)
		}
	}
	for _, tool := range []string{"canyon_promote", "canyon_check_ref", "canyon_deploy_ref"} {
		refused(tool, call(tool))
	}
	refused("a batch", "["+call("canyon_promote")+"]")
	refused("a mixed-case duplicate key", `{"jsonrpc":"2.0","id":2,"method":"tools/call","Method":"tools/list","params":{"name":"canyon_promote","Name":"canyon_rules","arguments":{"app":"claims-portal"}}}`)
	refused("a duplicate name", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canyon_rules","name":"canyon_promote","arguments":{"app":"claims-portal"}}}`)
	act, _ := signedInOver(t, base, gina, id, redirect, "read", "act")
	if _, _, body := mcpCall(t, base, act, call("canyon_promote"), nil); strings.Contains(body, "insufficient_scope") {
		t.Errorf("an act token's promote stopped at the scope: %s", body)
	}
}

// The CLI's tool handlers on /mcp reach this Canyon's /api/v1 alone, in-process, as the caller over MCP: no other
// host, no other path, and no credential or transport of the handler's own rides along.
func TestMCPRemoteToolsReachTheAPIAlone(t *testing.T) {
	var seen *http.Request
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r; w.WriteHeader(http.StatusNoContent) })
	caller := mcpCaller{session: signedSession{act: actor{Kind: "person", Subject: "sub-gina", Display: "Gina"}, role: "member", id: "s"}}
	c := &http.Client{Transport: mcpTransport{a: &app{}, r: httptest.NewRequest(http.MethodPost, "/mcp", nil), c: caller, api: api}}
	for _, at := range []string{"http://" + mcpHost + "/oauth/token", "http://" + mcpHost + "/api/v1/../oauth/token", "http://" + mcpHost + "/api/v1x/fleet",
		"https://" + mcpHost + "/api/v1/fleet", "http://169.254.169.254/api/v1/fleet", "http://localhost:7777/api/v1/fleet"} {
		seen = nil
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, at, nil)
		if resp, err := c.Do(req); err == nil || seen != nil {
			_ = resp.Body.Close()
			t.Errorf("%s: reached the API (%v)", at, seen != nil)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+mcpHost+"/api/v1/projects/claims-portal/status", nil)
	req.Header.Set("Canyon-Transport", "mcp-local")
	req.Header.Set("Cookie", "canyon_session=x")
	req.Header.Set("Authorization", "Bearer x")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if seen == nil || seen.URL.Path != "/projects/claims-portal/status" || seen.Header.Get("Canyon-Transport") != transportMCPRemote ||
		seen.Header.Get("Cookie") != "" || seen.Header.Get("Authorization") != "" || actorOf(seen.Context()).Subject != "sub-gina" ||
		events.ProvenanceOf(seen.Context()).Via != "mcp" {
		t.Errorf("the API saw %+v", seen)
	}
}
