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
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/cli"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// mcpNeedsSignIn is /mcp's answer on a host with sign-in off (design/EXPERIENCE.md, D5).
const mcpNeedsSignIn = "Connecting by URL needs sign-in, and it's off on this Canyon. On this machine, use canyon mcp instead."

// actTools are the tools a token needs the act scope for: each changes what runs (design/specs/mcp.md §2.2).
var actTools = []string{mcptools.Promote, mcptools.CheckRef, mcptools.DeployRef, mcptools.CreateApp}

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
	bearer := auth.RequireBearerToken(a.mcpToken, nil)
	mux.Handle("/mcp", a.mcpOrigin(a.mcpChallenge(bearer(a.mcpStepUp(served)))))
}

// mcpChallenge names, on a 401, where a client learns to get a token and the scopes it may ask for (RFC 6750 §3,
// RFC 9728 §5.1).
func (a *app) mcpChallenge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(challenging{ResponseWriter: w, header: fmt.Sprintf(`Bearer resource_metadata=%q, scope="read act"`, a.public+"/.well-known/oauth-protected-resource/mcp")}, r)
	})
}

type challenging struct {
	http.ResponseWriter
	header string
}

func (c challenging) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		c.Header().Set("WWW-Authenticate", c.header)
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c challenging) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// mcpStepUp answers a call to an act tool made with a read token 403 insufficient_scope, with the scope to ask for, so a
// client can ask the person for more (MCP authorization, scope challenge). It's a courtesy, never the guard: it reads the
// message with the SDK's own decoder and passes on anything it can't read, and what refuses is the tool's own check
// (scopeRefusal) and the transport's list of what each scope reaches (mcpAdmits).
func (a *app) mcpStepUp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if r.Method != http.MethodPost || info == nil || slices.Contains(info.Scopes, "act") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "the request is over 4 MiB", nil)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var params struct {
			Name string `json:"name"`
		}
		if msg, err := jsonrpc.DecodeMessage(body); err == nil {
			if call, ok := msg.(*jsonrpc.Request); ok && call.Method == "tools/call" && json.Unmarshal(call.Params, &params) == nil && slices.Contains(actTools, params.Name) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="read act", resource_metadata=%q`, a.public+"/.well-known/oauth-protected-resource/mcp"))
				writeErr(w, http.StatusForbidden, "insufficient_scope", scopeLine(params.Name), nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// scopeLine is what a person is told of an act tool their connection wasn't allowed (design/EXPERIENCE.md, D5).
func scopeLine(tool string) string {
	return tool + " needs permission to act for you. Connect Canyon again and allow it to check, deploy and promote."
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
	if _, err := a.pool.Exec(ctx, "UPDATE sessions SET rotated_unused = false WHERE id = $1 AND rotated_unused", id); err != nil { // the pair served: a return of the token it replaced is a second holder's
		return nil, err
	}
	return &auth.TokenInfo{Scopes: scopes, Expiration: expires, UserID: sessionDigest(id), Extra: map[string]any{"caller": mcpCaller{session: s, scopes: scopes}}}, nil
}

// mcpServer is one request's server: the CLI's own tools and words (cli.RemoteServer), every request they make served
// in-process as the caller, and the act scope judged on each call the SDK dispatches.
func (a *app) mcpServer(r *http.Request, api http.Handler) *mcp.Server {
	c := auth.TokenInfoFromContext(r.Context()).Extra["caller"].(mcpCaller)                          // the bearer stage put it there, or the request never got here
	s := cli.RemoteServer(r.Context(), &mcpTransport{a: a, r: r, c: c, api: api}, a.public, version) // the public URL: the links tools answer name it, and nothing dials it
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok {
				if refused := scopeRefusal(call.Params.Name, call); refused != nil {
					return refused, nil
				}
			}
			return next(ctx, method, req)
		}
	})
	return s
}

// scopeRefusal is the insufficient_scope answer of an act tool called with a token that wasn't given act, read off
// the call the SDK dispatched, so what decides the tool and what decides the scope are one parse; nil where it may run.
func scopeRefusal(tool string, req *mcp.CallToolRequest) *mcp.CallToolResult {
	if !slices.Contains(actTools, tool) || req.Extra != nil && req.Extra.TokenInfo != nil && slices.Contains(req.Extra.TokenInfo.Scopes, "act") {
		return nil
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: scopeLine(tool)}}}
}

// mcpTransport is the round tripper the CLI's tool handlers get on /mcp: each request served by the API mux in-process
// as the caller, never sent anywhere, and only under /api/v1, so a tool has no other way to the network.
type mcpTransport struct {
	a   *app
	r   *http.Request // the /mcp request, for its source address
	c   mcpCaller
	api http.Handler

	mu   sync.Mutex
	made map[string]bool // the apps this request made (canyon_create_app): the only ones a tool may delete
}

// mcpReads and mcpActs are every request a tool on /mcp may make, the second needing the act scope (design/specs/mcp.md
// §2.2): reads are GETs alone, and act adds the four POSTs that check, deploy and promote, and canyon_create_app's
// create, link and, of an app that request made alone, delete. Nothing else reaches /api/v1 from /mcp, whatever a tool
// asks: no secret is set or read, nothing is shared, approved or changed in settings.
var (
	mcpReads = []string{"GET /skill/manifest", "GET /playbooks/{slug}", "GET /egress", "GET /front-door", "GET /fleet", "GET /preferences",
		"GET /scanners/data", "GET /projects/{id}", "GET /projects/{id}/status", "GET /projects/{id}/gate-runs", "GET /projects/{id}/packages",
		"GET /projects/{id}/secrets", "GET /projects/{id}/logs", "GET /projects/{id}/egress", "GET /gate-runs/{run}", "GET /gate-runs/{run}/timeline",
		"GET /gate-runs/{run}/sarif", "GET /promotion-approvals/{code}"}
	mcpActs = []string{"POST /projects/{id}/packages/from-repository", "POST /projects/{id}/gate-runs", "POST /projects/{id}/deploys", "POST /projects/{id}/promotions",
		"POST /projects", "PUT /projects/{id}/repository", "DELETE /projects/{id}"}

	mcpDelete = mcpRoutesOf([]string{"DELETE /projects/{id}"})

	mcpReadMux, mcpActMux = mcpRoutesOf(mcpReads), mcpRoutesOf(append(slices.Clone(mcpReads), mcpActs...))
)

func mcpRoutesOf(patterns []string) *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range patterns {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}

// RoundTrip serves a tool's request by /api/v1 in-process, through the session stage as the token's person, read again
// for each request as a cookie's is: still live, still on the membership lists, the host set up. Only a request
// mcpReads names reaches it, or with act one mcpActs names; anything else is refused before any handler.
func (t *mcpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rest, ok := strings.CutPrefix(req.URL.Path, "/api/v1/")
	if !ok || req.URL.Scheme+"://"+req.URL.Host != t.a.public || path.Clean("/"+rest) != "/"+rest {
		return nil, fmt.Errorf("an MCP tool reaches this Canyon's /api/v1 alone, not %s", req.URL.Redacted())
	}
	in := req.Clone(req.Context())
	in.URL.Path, in.URL.RawPath, in.RequestURI = "/"+rest, strings.TrimPrefix(req.URL.RawPath, "/api/v1"), ""
	in.Header.Del("Cookie")
	in.Header.Del("Authorization")
	in.Header.Set("Canyon-Transport", transportMCPRemote) // the host's word, whatever the handler set
	w := httptest.NewRecorder()
	signed, setup, flow, live := t.a.sessionFor(req.Context(), t.c.session.id)
	switch status, code, message := mcpAdmits(t.c.scopes, in); {
	case !live || flow != "oauth":
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "this connection's sign-in has ended: connect Canyon again", nil)
	case status != 0:
		writeErr(w, status, code, message, nil)
	case routed(mcpDelete, in) && !t.madeHere(in):
		writeErr(w, http.StatusForbidden, "not_allowed", "a tool deletes only an app it made in that call", nil)
	case in.Method == http.MethodPost && in.URL.Path == "/projects":
		t.make(w, in, signed, setup)
	default:
		t.a.serveSigned(w, in, signed, setup, events.Provenance{Via: "mcp", Role: signed.role, Session: sessionDigest(signed.id), Source: t.a.proxies.source(t.r)}, t.api)
	}
	resp := w.Result()
	resp.Request = req
	return resp, nil
}

// make serves canyon_create_app's POST /projects under the person's lock, held on its own connection across every
// replica: the day's count and the create are one step, so no two calls at once make an eleventh app.
func (t *mcpTransport) make(w *httptest.ResponseRecorder, in *http.Request, signed signedSession, setup bool) {
	ctx := in.Context()
	conn, err := t.a.pool.Acquire(ctx)
	if err != nil {
		t.a.internal(w, "mcp apps a day", err)
		return
	}
	defer conn.Release()
	key := "mcp apps a day " + signed.act.Subject
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key); err != nil {
		t.a.internal(w, "mcp apps a day", err)
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key)
	}()
	if !t.mayMake(w, signed) {
		return
	}
	t.a.serveSigned(w, in, signed, setup, events.Provenance{Via: "mcp", Role: signed.role, Session: sessionDigest(signed.id), Source: t.a.proxies.source(t.r)}, t.api)
	if w.Code != http.StatusCreated {
		return
	}
	var made struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if json.Unmarshal(w.Body.Bytes(), &made) == nil {
		t.mu.Lock()
		if t.made == nil {
			t.made = map[string]bool{}
		}
		t.made[made.Project.ID] = true
		t.mu.Unlock()
	}
}

// madeHere is whether r deletes an app this request made.
func (t *mcpTransport) madeHere(r *http.Request) bool {
	_, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/") // projects/<id>
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.made[rest]
}

// mcpAppsADay is how many apps a person makes over /mcp in a day (design/specs/mcp.md §2.3).
const mcpAppsADay = 10

// mayMake is whether the person may make another app over /mcp today, the refusal written where not.
func (t *mcpTransport) mayMake(w http.ResponseWriter, signed signedSession) bool {
	var made int
	if err := t.a.pool.QueryRow(t.r.Context(), `SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects' AND via = 'mcp'
		AND actor_subject = $1 AND ts > now() - interval '1 day'`, signed.act.Subject).Scan(&made); err != nil {
		t.a.internal(w, "mcp apps a day", err)
		return false
	}
	if made >= mcpAppsADay {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("You've made %d apps through Canyon's tools today, the most for one day. Make the next one tomorrow, or from a laptop with the Canyon CLI.", mcpAppsADay), nil)
		return false
	}
	return true
}

// mcpAdmits is whether a token with scopes may make r over /mcp: 0, or the refusal's status, code and message.
func mcpAdmits(scopes []string, r *http.Request) (status int, code, message string) {
	allowed := mcpReadMux
	if slices.Contains(scopes, "act") {
		allowed = mcpActMux
	}
	switch {
	case !routed(allowed, r) && routed(mcpActMux, r):
		return http.StatusForbidden, "insufficient_scope", "this connection may only read: connect Canyon again and allow it to check, deploy and promote"
	case !routed(allowed, r):
		return http.StatusForbidden, "not_allowed", "a tool can't ask that of Canyon"
	case strings.HasSuffix(r.URL.Path, "/logs") && r.URL.Query().Get("redaction") != "masked": // every secret's value shown as its name, always (design/specs/mcp.md §1.2)
		return http.StatusForbidden, "not_allowed", "a tool reads logs masked alone"
	}
	return 0, "", ""
}

// routed is whether m has a pattern for r: its method and its path.
func routed(m *http.ServeMux, r *http.Request) bool {
	_, pattern := m.Handler(r)
	return pattern != ""
}
