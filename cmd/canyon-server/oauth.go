package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
)

// Canyon as the OAuth authorization server for MCP clients (design/AUTH.md §4.7; design/specs/mcp.md §2.2;
// contracts/routes.md, OAuth). A client names itself by a Client ID Metadata Document at its own https URL, or
// registers (RFC 7591); it sends the person's browser to /oauth/authorize, the person allows it on the dashboard's
// consent page, and /oauth/token turns the one-minute code into a session of its own, flow oauth, bound to the client
// and to the MCP resource, as the device flow turns a code into a CLI session. Served only with sign-in on, and only on
// an https public URL or a loopback one: anywhere else no route of it is registered, and each answers the catch-all's
// 404.

const (
	oauthRequestLife = 10 * time.Minute    // a consent the person hasn't given yet
	oauthCodeLife    = time.Minute         // a code the client hasn't redeemed yet
	oauthAccessLife  = time.Hour           // an access token: a bearer a client keeps on disk, renewed by its refresh token
	oauthRefreshLife = 30 * 24 * time.Hour // the session's whole life, from the consent: no refresh outlives it
	oauthDocumentMax = 64 << 10            // a client's metadata document
	oauthFetchLimit  = 5 * time.Second     // and how long its fetch may take
)

// oauthScopes are the scopes Canyon grants, in this order everywhere it names them (mcp.md §2.2): read, and act.
var oauthScopes = []string{"read", "act"}

// oauthServed is whether this host serves the authorization server: sign-in on, behind TLS or on loopback alone,
// as OAuth 2.1 allows a native client's loopback.
func (a *app) oauthServed() bool {
	u, err := neturl.Parse(a.public)
	return a.auth == authOIDC && err == nil && (u.Scheme == "https" || u.Scheme == "http" && loopbackHost(u.Hostname()))
}

// mcpResource is the one resource Canyon issues tokens for (RFC 8707).
func (a *app) mcpResource() string { return a.public + "/mcp" }

func routesOAuth(mux *http.ServeMux, a *app) {
	if !a.oauthServed() {
		return
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.oauthMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.resourceMetadata)
	mux.HandleFunc("GET /oauth/authorize", a.limit.limit(a.oauthAuthorize))
	mux.HandleFunc("POST /oauth/register", a.limit.limit(a.oauthRegister))
	mux.HandleFunc("POST /oauth/token", a.oauthToken) // bounded by its single-use code and 32-byte refresh token, never per address: behind an ingress every client shares one
}

// routesOAuthRequests are the consent page's: a person's, in a browser, as a device code's approval is.
func routesOAuthRequests(api *http.ServeMux, a *app) {
	if !a.oauthServed() {
		return
	}
	api.HandleFunc("GET /oauth/requests/{id}", a.consentOnly(a.getOAuthRequest))
	api.HandleFunc("POST /oauth/requests/{id}/approve", a.consentOnly(a.approveOAuthRequest))
	api.HandleFunc("POST /oauth/requests/{id}/deny", a.consentOnly(a.denyOAuthRequest))
}

// consentOnly is the consent page's guard: a browser session's alone (browserOnly), never framed, and its answers
// only from this host's own page. The session stage lets a request that names no origin through, as a CLI's does;
// consent is a browser's, so here a request must say it came from Canyon.
func (a *app) consentOnly(next http.HandlerFunc) http.HandlerFunc {
	return browserOnly(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Method != http.MethodGet && !saidSameOrigin(r, a.public) {
			writeErr(w, http.StatusForbidden, "cross_origin", "this request did not come from Canyon", nil)
			return
		}
		next(w, r)
	})
}

// saidSameOrigin is whether the browser says the request came from this host's own page: Sec-Fetch-Site same-origin,
// or, from a browser that sends none, an Origin that is the public URL's. A request that says neither is refused.
func saidSameOrigin(r *http.Request, public string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	return origin != "" && originOf(origin) == originOf(public)
}

// oauthMetadata is the authorization server's metadata (RFC 8414).
func (a *app) oauthMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": a.public, "authorization_endpoint": a.public + "/oauth/authorize", "token_endpoint": a.public + "/oauth/token",
		"registration_endpoint": a.public + "/oauth/register", "response_types_supported": []string{"code"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": oauthScopes,
		"client_id_metadata_document_supported": true, "authorization_response_iss_parameter_supported": true,
	})
}

// resourceMetadata is the MCP server's protected-resource metadata (RFC 9728).
func (a *app) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": a.mcpResource(), "authorization_servers": []string{a.public},
		"scopes_supported": oauthScopes, "bearer_methods_supported": []string{"header"}, "resource_name": "Canyon"})
}

// redirectURI is whether s is a redirect Canyon sends a person's browser to, by an allowlist: https, or http on a
// loopback host (RFC 8252 §7.3), and nothing else, never with credentials or a fragment, which a code must not travel in.
func redirectURI(s string) bool {
	u, err := neturl.Parse(s)
	if err != nil || len(s) > 2048 || u.Fragment != "" || u.User != nil || strings.ContainsAny(s, "\r\n\t ") {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		return loopbackHost(u.Hostname())
	}
	return false
}

// redirectMatches is whether asked is one of registered: exactly, but for a loopback http redirect's port, which a
// native client picks at each sign-in (RFC 8252 §7.3).
func redirectMatches(registered []string, asked string) bool {
	if slices.Contains(registered, asked) {
		return true
	}
	a, err := neturl.Parse(asked)
	if err != nil || a.Scheme != "http" || !loopbackHost(a.Hostname()) {
		return false
	}
	return slices.ContainsFunc(registered, func(r string) bool {
		u, err := neturl.Parse(r)
		return err == nil && u.Scheme == "http" && u.Hostname() == a.Hostname() && u.Path == a.Path && u.RawQuery == a.RawQuery
	})
}

// oauthClient is who asks: its id, its name and the host it is shown by, and whether a document at its own https
// URL named it, rather than a registration's own word.
type oauthClient struct {
	id, name, host string
	verified       bool
	redirects      []string
}

var (
	errNoClient   = errors.New("its client isn't registered")
	errNoRedirect = errors.New("it names a redirect the client didn't register")
)

// clientOf resolves client_id: a document's URL, fetched now, or a registration's id.
func (a *app) clientOf(ctx context.Context, id string) (oauthClient, error) {
	if strings.HasPrefix(id, "https://") {
		return fetchClientDocument(ctx, a.documents(), id)
	}
	var c oauthClient
	err := a.pool.QueryRow(ctx, "UPDATE oauth_clients SET used_at = coalesce(used_at, now()) WHERE id = $1 RETURNING id, name, redirect_uris", id).Scan(&c.id, &c.name, &c.redirects)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errNoClient
	}
	return c, err
}

// documents is the client a metadata document is fetched with: a test's own, else clientDocumentClient's.
func (a *app) documents() *http.Client {
	if a.clientDocuments != nil {
		return a.clientDocuments
	}
	return clientDocumentClient()
}

// clientDocumentClient fetches a client's metadata document, from a public address alone: a document's URL is the
// client's word, and the server must never be made to reach the org's own network for it.
func clientDocumentClient() *http.Client {
	dialer := &net.Dialer{Timeout: oauthFetchLimit, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !publicAddress(ip) {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}}
	return &http.Client{Timeout: oauthFetchLimit, Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: oauthFetchLimit},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// notPublic are the prefixes IsGlobalUnicast lets through that still reach an org's own network: carrier-grade NAT,
// and NAT64's (RFC 6052, RFC 8215), which carry an IPv4 address of any kind inside them.
var notPublic = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}

// publicAddress is a global unicast address on the public internet: none of the org's own, mapped or not. Loopback,
// unspecified, link-local and every multicast address are not global unicast; private and ULA (fc00::/7) addresses
// are IsPrivate; carrier-grade NAT and NAT64 are notPublic.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !slices.ContainsFunc(notPublic, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// fetchClientDocument reads a Client ID Metadata Document: its client_id must be its own URL, its name plain text,
// and each redirect one Canyon sends a browser to.
func fetchClientDocument(ctx context.Context, c *http.Client, id string) (oauthClient, error) {
	u, err := neturl.Parse(id)
	if err != nil || u.Path == "" || u.Path == "/" || u.User != nil || u.Fragment != "" || len(id) > 2048 {
		return oauthClient{}, errNoClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return oauthClient{}, errNoClient
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return oauthClient{}, errNoClient
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, oauthDocumentMax+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(body) > oauthDocumentMax || json.Unmarshal(body, &doc) != nil ||
		doc.ClientID != id || !plainText(doc.ClientName, 100) || len(doc.RedirectURIs) == 0 || len(doc.RedirectURIs) > 10 ||
		slices.ContainsFunc(doc.RedirectURIs, func(r string) bool { return !redirectURI(r) }) {
		return oauthClient{}, errNoClient
	}
	return oauthClient{id: id, name: doc.ClientName, host: u.Hostname(), verified: true, redirects: doc.RedirectURIs}, nil
}

// oauthInvalid sends the browser to the consent page's refusal: Canyon can't send it back to a client it can't
// verify, or to a redirect the client didn't register, so it never redirects there.
func oauthInvalid(w http.ResponseWriter, r *http.Request, why error) {
	reason := map[error]string{errNoClient: "client", errNoRedirect: "redirect"}[why]
	http.Redirect(w, r, "/#/oauth?invalid="+reason, http.StatusFound)
}

// oauthBack sends the browser back to the client's redirect with params and the issuer (RFC 9207).
func (a *app) oauthBack(redirect, state string, params neturl.Values) string {
	u, _ := neturl.Parse(redirect) // registered, and checked when it was
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", a.public)
	u.RawQuery = q.Encode()
	return u.String()
}

// scopesOf is the scope parameter as Canyon's scopes, read when none is asked for; ok is false for one it doesn't have.
func scopesOf(s string) (scopes []string, ok bool) {
	asked := strings.Fields(s)
	if len(asked) == 0 {
		return []string{"read"}, true
	}
	for _, scope := range oauthScopes {
		if slices.Contains(asked, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes, !slices.ContainsFunc(asked, func(x string) bool { return !slices.Contains(oauthScopes, x) })
}

// oauthAuthorize begins an authorization (RFC 6749 §4.1, PKCE S256 required, RFC 8707's resource required): it
// checks the client and its redirect first, answering Canyon's own refusal page for either; any other fault goes back
// to the client as OAuth says. A valid request waits for the person's consent on the dashboard.
func (a *app) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	c, err := a.clientOf(ctx, q.Get("client_id"))
	if err != nil {
		if !errors.Is(err, errNoClient) {
			a.log.Printf("canyon-server: oauth client: %v", err)
		}
		oauthInvalid(w, r, errNoClient)
		return
	}
	redirect := q.Get("redirect_uri")
	if !redirectURI(redirect) || !redirectMatches(c.redirects, redirect) {
		oauthInvalid(w, r, errNoRedirect)
		return
	}
	if c.host == "" { // a registration is shown by where it sends the person back
		if u, err := neturl.Parse(redirect); err == nil {
			c.host = cmpHost(u)
		}
	}
	state := q.Get("state")
	back := func(code, description string) {
		http.Redirect(w, r, a.oauthBack(redirect, state, neturl.Values{"error": {code}, "error_description": {description}}), http.StatusFound)
	}
	scopes, ok := scopesOf(q.Get("scope"))
	switch {
	case q.Get("response_type") != "code":
		back("unsupported_response_type", "Canyon answers response_type=code alone")
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
		back("invalid_request", "Canyon needs a PKCE challenge, S256")
	case q.Get("resource") != a.mcpResource():
		back("invalid_target", "the resource is "+a.mcpResource())
	case !ok:
		back("invalid_scope", "Canyon's scopes are read and act")
	case len(state) > 1024:
		back("invalid_request", "the state is over 1,024 characters")
	default:
		id, err := token()
		if err == nil {
			_, err = a.pool.Exec(ctx, `WITH gone AS (DELETE FROM oauth_requests WHERE expires_at <= now() AND (code_expires_at IS NULL OR code_expires_at <= now()))
				INSERT INTO oauth_requests (id, client_id, client_name, client_host, client_verified, redirect_uri, state, code_challenge, scope, resource, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now() + make_interval(secs => $11))`,
				id, c.id, c.name, c.host, c.verified, redirect, state, q.Get("code_challenge"), scopes, a.mcpResource(), oauthRequestLife.Seconds())
		}
		if err != nil {
			a.log.Printf("canyon-server: oauth authorize: %v", err)
			back("server_error", "Canyon could not take the request")
			return
		}
		http.Redirect(w, r, "/#/oauth?request="+id, http.StatusFound)
	}
}

func cmpHost(u *neturl.URL) string { return u.Hostname() }

// oauthRegister registers a public client (RFC 7591): no secret, PKCE alone, each redirect https, or http on a
// loopback host. A registration never used within a day is deleted by the next one, so the table cleans itself.
func (a *app) oauthRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		GrantTypes   []string `json:"grant_types"`
		Response     []string `json:"response_types"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	refuse := func(description string) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata", "error_description": description})
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, oauthDocumentMax))
	switch {
	case dec.Decode(&body) != nil:
		refuse("the body is one JSON object of at most 64 KiB")
		return
	case body.AuthMethod != "" && body.AuthMethod != "none":
		refuse("Canyon registers public clients alone: token_endpoint_auth_method none")
		return
	case slices.ContainsFunc(body.GrantTypes, func(g string) bool { return g != "authorization_code" && g != "refresh_token" }):
		refuse("Canyon grants authorization_code and refresh_token")
		return
	case slices.ContainsFunc(body.Response, func(t string) bool { return t != "code" }):
		refuse("Canyon answers response_type code alone")
		return
	case len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > 10 || slices.ContainsFunc(body.RedirectURIs, func(u string) bool { return !redirectURI(u) }):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri", "error_description": "one to ten redirects, each https, or http on a loopback host"})
		return
	}
	if body.ClientName == "" { // shown by where it sends the person back
		if u, err := neturl.Parse(body.RedirectURIs[0]); err == nil {
			body.ClientName = cmpHost(u)
		}
	}
	if !plainText(body.ClientName, 100) {
		refuse("client_name is 1 to 100 characters of plain text")
		return
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	id := "dcr_" + hex.EncodeToString(raw[:])
	var at time.Time
	if err := a.pool.QueryRow(r.Context(), `WITH gone AS (DELETE FROM oauth_clients WHERE used_at IS NULL AND created_at < now() - interval '1 day')
		INSERT INTO oauth_clients (id, name, redirect_uris) VALUES ($1, $2, $3) RETURNING created_at`, id, body.ClientName, body.RedirectURIs).Scan(&at); err != nil {
		a.log.Printf("canyon-server: oauth register: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	a.log.Printf("canyon-server: oauth client %s registered", id)
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "client_name": body.ClientName, "redirect_uris": body.RedirectURIs,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none",
		"client_id_issued_at": at.Unix()})
}

// getOAuthRequest is the consent page's read: who asks, for what, as whom.
func (a *app) getOAuthRequest(w http.ResponseWriter, r *http.Request) {
	var name, host, redirect string
	var verified bool
	var scopes []string
	var expires time.Time
	err := a.pool.QueryRow(r.Context(), `SELECT client_name, client_host, client_verified, redirect_uri, scope, expires_at FROM oauth_requests
		WHERE id = $1 AND session_id IS NULL AND expires_at > now()`, r.PathValue("id")).Scan(&name, &host, &verified, &redirect, &scopes, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "This request has expired. Go back to the app and connect again.", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	back := ""
	if u, err := neturl.Parse(redirect); err == nil {
		back = cmpHost(u)
	}
	public, _ := neturl.Parse(a.public)
	writeJSON(w, http.StatusOK, map[string]any{"client": map[string]any{"name": name, "host": host, "verified": verified}, "redirect_host": back,
		"scopes": scopes, "host": public.Host, "expires_at": expires.UTC().Format(time.RFC3339),
		"account": map[string]string{"display": actorOf(r.Context()).Display, "email": addressOf(r.Context())}})
}

// approveOAuthRequest is the person's consent: the request takes their browser session and a one-minute code, and the
// page sends the browser back to the client with it.
func (a *app) approveOAuthRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, _ := sessionCookieOf(a.public)
	c, err := r.Cookie(name) // the session stage accepted this cookie, so it is here
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to Canyon", nil)
		return
	}
	code, err := token()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	hash := sha256.Sum256([]byte(code))
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var redirect, state, client, host string
	var scopes []string
	err = tx.QueryRow(ctx, `UPDATE oauth_requests SET session_id = $2, code_hash = $3, code_expires_at = now() + make_interval(secs => $4)
		WHERE id = $1 AND session_id IS NULL AND expires_at > now() RETURNING redirect_uri, state, client_name, client_host, scope`,
		r.PathValue("id"), c.Value, hash[:], oauthCodeLife.Seconds()).Scan(&redirect, &state, &client, &host, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "This request has expired. Go back to the app and connect again.", nil)
		return
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "connect agent", "", "", map[string]any{"client": client, "client_host": host, "scopes": scopes})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": a.oauthBack(redirect, state, neturl.Values{"code": {code}})})
}

// denyOAuthRequest ends the request, and the page sends the browser back with access_denied.
func (a *app) denyOAuthRequest(w http.ResponseWriter, r *http.Request) {
	var redirect, state string
	err := a.pool.QueryRow(r.Context(), "DELETE FROM oauth_requests WHERE id = $1 AND session_id IS NULL AND expires_at > now() RETURNING redirect_uri, state", r.PathValue("id")).Scan(&redirect, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "This request has expired. Go back to the app and connect again.", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": a.oauthBack(redirect, state, neturl.Values{"error": {"access_denied"}, "error_description": {"the person didn't allow it"}})})
}

// tokenError is the token route's refusal (RFC 6749 §5.2).
func tokenError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": description})
}

// pkceMatches is RFC 7636's S256: the challenge is the verifier's SHA-256, base64url without padding.
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// approver is the person whose session an OAuth session copies: who they are as their sign-in stored them.
type approver struct {
	act                                      actor
	issuer, email, hd, groupsRead            string
	verified, claimedAdmin, claimedResponder bool
	groups                                   []string
}

const approverColumns = "s.actor_kind, s.actor_subject, s.actor_display, s.issuer, s.email, s.email_verified, s.claimed_admin, s.claimed_responder, s.hosted_domain, s.groups, s.groups_read"

func (p *approver) dest() []any {
	return []any{&p.act.Kind, &p.act.Subject, &p.act.Display, &p.issuer, &p.email, &p.verified, &p.claimedAdmin, &p.claimedResponder, &p.hd, &p.groups, &p.groupsRead}
}

// oauthActor is Canyon, as the trail names it when it ends a session whose refresh token came back after it rotated.
var oauthActor = actor{Kind: "canyon", Subject: "oauth", Display: "Canyon"}

// oauthToken redeems a code into a session of the client's own, one row per consent (RFC 6749 §4.1.3), or rotates a
// refresh token on that row in place (§6). A refresh token already rotated away, presented again, ends the session:
// one of the two holders is not the client (OAuth 2.1 §4.3.1).
func (a *app) oauthToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") == "" {
		tokenError(w, "invalid_request", "the body is a form with client_id")
		return
	}
	f := r.PostForm
	internal := func(err error) {
		a.log.Printf("canyon-server: oauth token: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	access, err := token()
	refresh, err2 := token()
	if err != nil || err2 != nil {
		internal(errors.Join(err, err2))
		return
	}
	accessHash, refreshHash := sha256.Sum256([]byte(access)), sha256.Sum256([]byte(refresh))
	var p approver
	var id, client string
	var scopes []string
	switch f.Get("grant_type") {
	case "authorization_code":
		hash := sha256.Sum256([]byte(f.Get("code")))
		var clientName, redirect, challenge, resource string
		// One statement consumes the code and reads its approver: a second redemption, or an approver signed out since, finds nothing.
		err := tx.QueryRow(ctx, `DELETE FROM oauth_requests q USING sessions s WHERE q.code_hash = $1 AND q.code_expires_at > now()
			AND s.id = q.session_id AND s.revoked_at IS NULL AND s.expires_at > now()
			RETURNING q.client_id, q.client_name, q.redirect_uri, q.code_challenge, q.scope, q.resource, `+approverColumns, hash[:]).
			Scan(append([]any{&client, &clientName, &redirect, &challenge, &scopes, &resource}, p.dest()...)...)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			tokenError(w, "invalid_grant", "the code is unknown, used or expired")
			return
		case err != nil:
			internal(err)
			return
		case client != f.Get("client_id") || redirect != f.Get("redirect_uri") || challenge == "" || !pkceMatches(f.Get("code_verifier"), challenge) ||
			f.Get("resource") != "" && f.Get("resource") != resource:
			_ = tx.Commit(ctx) // the code is spent whatever it was redeemed with
			tokenError(w, "invalid_grant", "the code was issued for another client, redirect, verifier or resource")
			return
		}
		if ok, err := a.stillMember(ctx, tx, p.email); err != nil || !ok {
			if err != nil {
				internal(err)
				return
			}
			_ = tx.Commit(ctx)
			tokenError(w, "invalid_grant", "the person who allowed it can no longer sign in to Canyon")
			return
		}
		if id, err = token(); err != nil {
			internal(err)
			return
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, issuer, email, email_verified, flow, claimed_admin, claimed_responder,
			hosted_domain, groups, groups_read, expires_at, oauth_client, oauth_client_name, resource, scope, access_hash, access_expires_at, refresh_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'oauth', $8, $9, $10, $11, $12, now() + make_interval(secs => $13), $14, $15, $16, $17, $18,
				now() + make_interval(secs => $19), $20)`,
			id, p.act.Kind, p.act.Subject, p.act.Display, p.issuer, p.email, p.verified, p.claimedAdmin, p.claimedResponder, p.hd, p.groups, p.groupsRead,
			oauthRefreshLife.Seconds(), client, clientName, resource, scopes, accessHash[:], oauthAccessLife.Seconds(), refreshHash[:]); err != nil {
			internal(err)
			return
		}
		o, err := a.orgOf(ctx, tx)
		if err != nil {
			internal(err)
			return
		}
		role := a.role(p.email, p.claimedAdmin, p.claimedResponder, a.withEnvironment(o))
		signedIn := events.WithProvenance(ctx, events.Provenance{Via: "mcp", Role: role, Session: sessionDigest(id), Source: a.proxies.source(r)})
		if err := events.AuditTx(signedIn, tx, p.act, "sign in", "", "", map[string]any{"flow": "oauth", "client": clientName}); err != nil {
			internal(err)
			return
		}
		a.log.Printf("canyon-server: sign in sub=%s %s flow=oauth role=%s", oneLine(p.act.Subject), oneLine(p.email), role)
	case "refresh_token":
		hash := sha256.Sum256([]byte(f.Get("refresh_token")))
		var current bool // the token is the session's refresh token now, not the one it replaced
		err := tx.QueryRow(ctx, `SELECT s.id, s.oauth_client, s.scope, s.refresh_hash = $1, `+approverColumns+` FROM sessions s
			WHERE (s.refresh_hash = $1 OR s.refresh_prev_hash = $1) AND s.flow = 'oauth' AND s.revoked_at IS NULL AND s.expires_at > now() FOR UPDATE`, hash[:]).
			Scan(append([]any{&id, &client, &scopes, &current}, p.dest()...)...)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			tokenError(w, "invalid_grant", "the refresh token is unknown, revoked or expired")
			return
		case err != nil:
			internal(err)
			return
		case client != f.Get("client_id"):
			tokenError(w, "invalid_grant", "the refresh token was issued to another client")
			return
		case !current: // a rotated token came back: one of its two holders isn't the client, so neither keeps the session
			if _, err := tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1", id); err != nil {
				internal(err)
				return
			}
			ended := events.WithProvenance(ctx, events.Provenance{Session: sessionDigest(id)})
			if err := events.AuditTx(ended, tx, oauthActor, "disconnect agent", "", "", map[string]any{"reason": "refresh token reused", "subject": p.act.Subject}); err != nil {
				internal(err)
				return
			}
			_ = tx.Commit(ctx)
			a.log.Printf("canyon-server: oauth session %s ended: its refresh token was used again", sessionDigest(id))
			tokenError(w, "invalid_grant", "the refresh token was already used, so Canyon ended the session")
			return
		}
		if ok, err := a.stillMember(ctx, tx, p.email); err != nil || !ok {
			if err != nil {
				internal(err)
				return
			}
			if _, err := tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1", id); err != nil {
				internal(err)
				return
			}
			_ = tx.Commit(ctx)
			tokenError(w, "invalid_grant", "the person who allowed it can no longer sign in to Canyon")
			return
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET access_hash = $2, access_expires_at = now() + make_interval(secs => $3), refresh_prev_hash = refresh_hash, refresh_hash = $4
			WHERE id = $1`, id, accessHash[:], oauthAccessLife.Seconds(), refreshHash[:]); err != nil {
			internal(err)
			return
		}
	default:
		tokenError(w, "unsupported_grant_type", "Canyon grants authorization_code and refresh_token")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(oauthAccessLife.Seconds()),
		"refresh_token": refresh, "scope": strings.Join(scopes, " ")})
}

// stillMember is whether the person whose session a token copies can still sign in to Canyon.
func (a *app) stillMember(ctx context.Context, q querier, email string) (bool, error) {
	o, err := a.orgOf(ctx, q)
	if err != nil {
		return false, err
	}
	return a.member(email, a.withEnvironment(o)), nil
}
