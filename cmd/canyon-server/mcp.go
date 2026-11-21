package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/events"
)

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
	served := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return a.mcpServer(r, api) },
		&mcp.StreamableHTTPOptions{Stateless: true, CrossOriginProtection: a.mcpOrigins()})
	mux.Handle("/mcp", auth.RequireBearerToken(a.mcpToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: a.public + "/.well-known/oauth-protected-resource/mcp"})(a.mcpScoped(served)))
}

// mcpOrigins trusts the public URL's origin alone; a request naming none, a client's, passes.
func (a *app) mcpOrigins() *http.CrossOriginProtection {
	c := http.NewCrossOriginProtection()
	_ = c.AddTrustedOrigin(originOf(a.public)) // the public URL was parsed at boot
	return c
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

// mcpScoped refuses a tools/call the token's scopes don't reach with 403 insufficient_scope and the scope it needs,
// so the client can ask the person for more (MCP authorization, scope challenge). Every tool is listed either way.
func (a *app) mcpScoped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "the request is over 1 MiB", nil)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &call) == nil && call.Method == "tools/call" && slices.Contains(actTools, call.Params.Name) &&
			!slices.Contains(auth.TokenInfoFromContext(r.Context()).Scopes, "act") {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="act", resource_metadata=%q`, a.public+"/.well-known/oauth-protected-resource/mcp"))
			writeErr(w, http.StatusForbidden, "insufficient_scope", call.Params.Name+" needs the act scope", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// asCaller serves one API request in-process as the MCP caller: the session stage's work done already, by the
// bearer, so the route sees the person and the channel, mcp, as a cookie's request would carry them.
func (a *app) asCaller(r *http.Request, api http.Handler, method, path string, body any) (int, json.RawMessage) {
	c := auth.TokenInfoFromContext(r.Context()).Extra["caller"].(mcpCaller)
	var in io.Reader
	if body != nil {
		raw, _ := json.Marshal(body) // the tools' own argument structs
		in = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(events.WithProvenance(withActor(r.Context(), c.session), events.Provenance{Via: "mcp", Role: c.session.role,
		Session: sessionDigest(c.session.id), Source: a.proxies.source(r)}), method, path, in)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveNoting(w, req, api)
	return w.Code, w.Body.Bytes()
}
