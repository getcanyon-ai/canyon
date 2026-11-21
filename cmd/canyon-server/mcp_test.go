package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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

// A read token lists every tool but calls none that changes what runs: 403 insufficient_scope with the scope to ask
// for. The scope is read off the JSON-RPC message itself, and a body that isn't one message is refused.
func TestMCPRemoteActsOnlyWithTheActScope(t *testing.T) {
	base, _, _, gina := oauthHost(t)
	const redirect = "https://claude.ai/cb"
	id := register(t, base, redirect)
	read, _ := signedInOver(t, base, gina, id, redirect)
	if code, _, body := mcpCall(t, base, read, mcpList, nil); code != http.StatusOK || !strings.Contains(body, "canyon_promote") {
		t.Errorf("a read token's list: %d %s", code, body)
	}
	code, h, body := mcpCall(t, base, read, mcpPromote, nil)
	if code != http.StatusForbidden || !strings.Contains(body, `"error":"insufficient_scope"`) ||
		!strings.Contains(h.Get("WWW-Authenticate"), `error="insufficient_scope", scope="act"`) {
		t.Errorf("a read token's promote: %d %q %s", code, h.Get("WWW-Authenticate"), body)
	}
	for name, msg := range map[string]string{"a batch": "[" + mcpPromote + "]", "not JSON": "promote please", "a name of another type": `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":7}}`} {
		if code, _, _ := mcpCall(t, base, read, msg, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	act, _ := signedInOver(t, base, gina, id, redirect, "read", "act")
	if code, _, body := mcpCall(t, base, act, mcpPromote, nil); code == http.StatusForbidden {
		t.Errorf("an act token's promote stopped at the scope: %s", body)
	}
}
