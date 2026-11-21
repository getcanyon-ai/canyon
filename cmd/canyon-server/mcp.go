package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// mcpNeedsSignIn is /mcp's answer on a host with sign-in off (design/EXPERIENCE.md, D5).
const mcpNeedsSignIn = "Connecting by URL needs sign-in, and it's off on this Canyon. On this machine, use canyon mcp instead."

// actTools are the tools a token needs the act scope for: each changes what runs (design/specs/mcp.md §2.2).
var actTools = []string{mcptools.Promote, mcptools.CheckRef, mcptools.DeployRef}

// mcpCaller is who an MCP request is, as the bearer's session says: the session the API mux serves its tools as, and
// the scopes the person allowed.
type mcpCaller struct {
	session signedSession
	scopes  []string
}

// routesMCP serves /mcp (design/specs/mcp.md §2): MCP's streamable HTTP transport, an OAuth 2.1 protected resource
// whose tokens Canyon's own authorization server issues (oauth.go). Each tool is a request to the API mux in-process,
// as the token's person, so every permission check, refusal and audit row is the route's own.
func routesMCP(mux *http.ServeMux, api http.Handler, a *app) {
	if !a.oauthServed() {
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
			if a.auth != authOIDC { // every caller is the anonymous admin: that power never goes to whatever reaches the port
				writeErr(w, http.StatusNotFound, "mcp_needs_sign_in", mcpNeedsSignIn, nil)
				return
			}
			writeErr(w, http.StatusNotFound, "not_found", "not found", nil)
		})
		return
	}
	// stateless (design/specs/mcp.md §2.1): any replica answers any request, and GET and DELETE are 405
	served := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return a.mcpServer(r, api) }, &mcp.StreamableHTTPOptions{Stateless: true})
	bearer := auth.RequireBearerToken(a.mcpToken, &auth.RequireBearerTokenOptions{ResourceMetadataURL: a.public + "/.well-known/oauth-protected-resource/mcp"})
	mux.Handle("/mcp", a.mcpOrigin(bearer(served)))
}

// mcpOrigin refuses a request whose Origin is another site's, before its token is read: a browser page elsewhere
// never reaches /mcp. A native client sends no Origin, and passes.
func (a *app) mcpOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && originOf(o) != originOf(a.public) {
			writeErr(w, http.StatusForbidden, "cross_origin", "this request did not come from Canyon", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mcpToken resolves an access token to its session: flow oauth, for this resource, its access unexpired, and the
// person still on the membership lists, read as every request's session is (sessionFor).
func (a *app) mcpToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	sum := sha256.Sum256([]byte(token))
	var id string
	var scopes []string
	var expires time.Time
	if err := a.pool.QueryRow(ctx, `SELECT id, scope, access_expires_at FROM sessions WHERE access_hash = $1 AND flow = 'oauth'
		AND resource = $2 AND access_expires_at > now() AND revoked_at IS NULL AND expires_at > now()`, sum[:], a.mcpResource()).Scan(&id, &scopes, &expires); err != nil {
		return nil, auth.ErrInvalidToken
	}
	s, _, _, ok := a.sessionFor(ctx, id)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{Scopes: scopes, Expiration: expires, UserID: sessionDigest(id), Extra: map[string]any{"caller": mcpCaller{session: s, scopes: scopes}}}, nil
}

// asCaller serves one API request in-process as the MCP caller: the session stage's work done already, by the
// bearer, so the route sees the person and the channel, mcp, as a cookie's request would carry them.
func (a *app) asCaller(ctx context.Context, r *http.Request, c mcpCaller, api http.Handler, method, path string, body any) (int, json.RawMessage) {
	var in io.Reader
	if body != nil {
		raw, _ := json.Marshal(body) // the tools' own argument structs
		in = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(events.WithProvenance(withActor(ctx, c.session), events.Provenance{Via: "mcp", Role: c.session.role,
		Session: sessionDigest(c.session.id), Source: a.proxies.source(r)}), method, path, in)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveNoting(w, req, api)
	return w.Code, w.Body.Bytes()
}

// mcpServer is one request's server, as its caller sees it: the instructions and the tools.
func (a *app) mcpServer(r *http.Request, api http.Handler) *mcp.Server {
	c := auth.TokenInfoFromContext(r.Context()).Extra["caller"].(mcpCaller) // the bearer stage put it there, or the request never got here
	s := mcp.NewServer(&mcp.Implementation{Name: "canyon", Title: "Canyon", Version: version}, &mcp.ServerOptions{Instructions: mcptools.Preamble})
	for _, t := range append(mcptools.Tools(), mcptools.RemoteOnly()...) {
		t := mcptools.Remote(t)
		s.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if refused := scopeRefusal(t.Name, req); refused != nil {
				return refused, nil
			}
			code, body := a.asCaller(ctx, r, c, api, http.MethodGet, "/skill/manifest", nil)
			return &mcp.CallToolResult{IsError: code != http.StatusOK, Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
		})
	}
	return s
}

// scopeRefusal is the insufficient_scope answer of an act tool called with a token that wasn't given act, read off
// the call the SDK dispatched, so what decides the tool and what decides the scope are one parse; nil where it may run.
func scopeRefusal(tool string, req *mcp.CallToolRequest) *mcp.CallToolResult {
	if !slices.Contains(actTools, tool) || req.Extra != nil && req.Extra.TokenInfo != nil && slices.Contains(req.Extra.TokenInfo.Scopes, "act") {
		return nil
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "insufficient_scope: " + tool + " needs the act scope. Connect again and allow it to check, deploy and promote."}}}
}
