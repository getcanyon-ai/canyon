package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// The app's own cookies, on its host alone: the app session, and the flow that binds a sign-in's code
// to the browser that started it. Both are named by the session cookie's builder, so under https they
// take the __Host- prefix and Secure, and no sibling app can plant one on this app's host.
const (
	appCookie   = "canyon-app"
	flowCookie  = "canyon_app_flow"
	appCallback = "/.canyon/callback"
)

func appCookieOf(public, name string) (string, bool) {
	session, secure := sessionCookieOf(public)
	return strings.Replace(session, sessionCookie, name, 1), secure
}

type tokenKey struct{}

// surfaces is the host-routed app surface of contracts/routes.md (App surfaces): a request whose
// Host is <app>.<env>.<anything> is proxied to the app's container, <app>-<env>:3000 on the app
// network, which the executor named (contracts/executor.md); every other Host, the control
// plane's own (a.own, the public URL's host) included, falls through to the server's routes. It
// sits inside the pipeline (recovery, the log line) and outside the session stage. Under oidc every
// request passes admit first and reaches the app with the identity token and without the browser's
// credentials; with auth off every visitor is the anonymous admin and reaches the app as before.
func (a *app) surfaces(next http.Handler, transport http.RoundTripper) http.Handler {
	own := strings.ToLower(a.own)
	strip := map[string]bool{}
	for _, name := range []string{appCookie, flowCookie} {
		n, _ := appCookieOf(a.public, name)
		strip[n] = true
	}
	scheme := "" // the scheme people open apps at, CANYON_PUBLIC_URL's: behind a proxy that ends TLS, every request reaches this server as http
	if u, err := url.Parse(a.public); err == nil {
		scheme = u.Scheme
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			app, env, _ := appSurface(pr.In.Host)
			pr.SetURL(&url.URL{Scheme: "http", Host: app + "-" + env + ":3000"})
			for h := range pr.Out.Header { // a client's own claims, which the app must never take for the proxy's: Canyon's identity token, set below, alone says who the person is
				if identity.ForgedHeader(h) {
					delete(pr.Out.Header, h) // the key as it is: Header.Del would canonicalise it, and miss one sent otherwise
				}
			}
			pr.SetXForwarded() // the app can attribute the request
			if scheme != "" {  // and build links with the scheme people use, never the request's
				pr.Out.Header.Set("X-Forwarded-Proto", scheme)
			}
			pr.Out.Host = pr.In.Host // the app sees the name it was published under
			if tok, ok := pr.In.Context().Value(tokenKey{}).(string); ok {
				pr.Out.Header.Set("Authorization", "Bearer "+tok) // replacing whatever the browser sent
				var kept []string                                 // the app's own cookies, as the browser sent them; Canyon's and an oauth2-proxy's never reach it
				for _, line := range pr.In.Header.Values("Cookie") {
					for _, pair := range strings.Split(line, ";") {
						name, _, _ := strings.Cut(strings.TrimSpace(pair), "=")
						if pair = strings.TrimSpace(pair); pair != "" && !strip[name] && !strings.HasPrefix(name, "_oauth2_proxy") {
							kept = append(kept, pair)
						}
					}
				}
				pr.Out.Header.Del("Cookie")
				if len(kept) > 0 {
					pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
				}
			}
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeErr(w, http.StatusBadGateway, "app_unreachable", "the app is not answering", nil)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, env, ok := appSurface(r.Host)
		if !ok || strings.ToLower(r.Host) == own { // a control plane published under an app-shaped name must not proxy itself away
			next.ServeHTTP(w, r)
			return
		}
		if a.auth == authOIDC {
			tok, ok := a.admit(w, r, app, env)
			if !ok {
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok))
		}
		proxy.ServeHTTP(w, r)
	})
}

// admit is the front door's check on every request to an app (design/PERMISSIONS.md, choice 3): the
// app's cookie names an exchanged app session of this app whose Canyon session is live, unrevoked and
// a member's — read now, so a revocation or a leaver lands on the next request — and whose person is
// at viewer or above, or an admin; then the identity token for this app, audience the app routed to.
// Level none goes to the no-access screen. Without a good cookie the browser is sent to sign in; the
// callback path exchanges the code it comes back with. false means the response is written.
func (a *app) admit(w http.ResponseWriter, r *http.Request, app, env string) (string, bool) {
	ctx := r.Context()
	if r.URL.Path == appCallback {
		a.exchange(w, r, app)
		return "", false
	}
	if unsafeMethod(r.Method) && !fromCanyon(r, strings.TrimSuffix(a.public, publicHost(a.public))+r.Host) { // every app shares one site, so a sibling app's page could send this browser's app cookie along (SameSite=Lax): a change is this app's own pages' alone
		writeErr(w, http.StatusForbidden, "cross_origin", "this request did not come from this app", nil)
		return "", false
	}
	name, _ := appCookieOf(a.public, appCookie)
	var session string
	if c, err := r.Cookie(name); err == nil && len(c.Value) == 2*tokenBytes {
		err := a.pool.QueryRow(ctx, `SELECT s.session_id FROM app_sessions s JOIN projects p ON p.id = s.project_id AND p.deleted_at IS NULL
			WHERE s.id = $1 AND s.project_id = $2 AND s.code_hash IS NULL AND s.expires_at > now()`, c.Value, app).Scan(&session) // a deleted app admits nobody, as GET /auth/app refuses it
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.surfaceFailed(w, app, err)
			return "", false
		}
	}
	if session == "" {
		a.startAppSignIn(w, r, app, env)
		return "", false
	}
	signed, _, _, ok := a.sessionFor(ctx, session)
	if !ok { // revoked, expired, or no longer a member: sign in again, which refuses a leaver
		a.startAppSignIn(w, r, app, env)
		return "", false
	}
	ctx = withActor(ctx, signed)
	level, err := levelOf(ctx, a.pool, app)
	if err != nil {
		a.surfaceFailed(w, app, err)
		return "", false
	}
	if level == "none" && signed.role != "admin" {
		http.Redirect(w, r, a.public+"/#/no-access/"+app, http.StatusFound) //nolint:gosec // this host's dashboard; app matched the project id pattern
		return "", false
	}
	roles, err := rolesOf(ctx, a.pool, app)
	if err != nil {
		a.surfaceFailed(w, app, err)
		return "", false
	}
	tok, err := a.idKey.Mint(identity.Claims{Issuer: a.public, Subject: signed.act.Subject, Email: signed.email, Name: signed.act.Display, Roles: roles, Audience: app, Env: env})
	if err != nil {
		a.surfaceFailed(w, app, err)
		return "", false
	}
	return tok, true
}

// startAppSignIn sets a new flow cookie on the app's host and sends the browser to GET /auth/app for
// this app, with the flow's hash: the code it comes back with is good only beside this cookie.
func (a *app) startAppSignIn(w http.ResponseWriter, r *http.Request, app, env string) {
	flow, err := token()
	if err != nil {
		a.surfaceFailed(w, app, err)
		return
	}
	name, secure := appCookieOf(a.public, flowCookie)
	http.SetCookie(w, &http.Cookie{Name: name, Value: flow, Path: "/", MaxAge: int(signInWindow.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // Secure follows the public URL's scheme, as the session cookie's does
	sum := sha256.Sum256([]byte(flow))
	http.Redirect(w, r, a.public+"/auth/app?"+url.Values{"to": {app + "." + env + "." + publicHost(a.public)}, "flow": {hex.EncodeToString(sum[:])}}.Encode(), http.StatusFound) //nolint:gosec // this host's own route; app matched the project id pattern
}

// exchange is the app's callback: the code is spent on its first presentation, whatever the outcome,
// and becomes the app's cookie only for this app, in time, from the browser holding the flow cookie
// the sign-in began with. The flow cookie is cleared either way.
func (a *app) exchange(w http.ResponseWriter, r *http.Request, app string) {
	ctx := r.Context()
	flowName, secure := appCookieOf(a.public, flowCookie)
	http.SetCookie(w, &http.Cookie{Name: flowName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // single use: cleared on every answer
	refuse := func() {
		writeErr(w, http.StatusBadRequest, "invalid_code", "this sign-in did not finish: open the app again", nil)
	}
	code := r.URL.Query().Get("code")
	if len(code) != 2*tokenBytes {
		refuse()
		return
	}
	codeSum := sha256.Sum256([]byte(code))
	var id, project string
	var bound []byte
	var fresh bool
	err := a.pool.QueryRow(ctx, `WITH spent AS (SELECT id, project_id, flow_hash, code_expires_at > now() AS fresh FROM app_sessions WHERE code_hash = $1 FOR UPDATE),
		used AS (UPDATE app_sessions s SET code_hash = NULL, code_expires_at = NULL, flow_hash = NULL FROM spent WHERE s.id = spent.id)
		SELECT id, project_id, flow_hash, fresh FROM spent`, codeSum[:]).Scan(&id, &project, &bound, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		refuse()
		return
	}
	if err != nil {
		a.surfaceFailed(w, app, err)
		return
	}
	flow, noFlow := r.Cookie(flowName)
	if noFlow != nil || project != app || !fresh || subtle.ConstantTimeCompare(bound, sha256Of(flow.Value)) != 1 {
		if _, err := a.pool.Exec(ctx, "DELETE FROM app_sessions WHERE id = $1", id); err != nil { // spent and never the browser's: nothing may ever present it
			a.surfaceFailed(w, app, err)
			return
		}
		refuse()
		return
	}
	name, _ := appCookieOf(a.public, appCookie)
	http.SetCookie(w, &http.Cookie{Name: name, Value: id, Path: "/", MaxAge: int(appSessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // Secure follows the public URL's scheme; host-only, no Domain
	http.Redirect(w, r, "/", http.StatusFound)
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func (a *app) surfaceFailed(w http.ResponseWriter, app string, err error) {
	a.log.Printf("canyon-server: app surface %s: %v", app, err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// appSurface reads <app>.<env>.<rest> off a Host, case not mattering as it does not in DNS: the
// app a project id, the env preview or prod. A port rides in the third label and is nobody's
// concern here. Anything else — an IP, a bare <app>.<env>, the control plane's own name — is
// not an app surface.
func appSurface(host string) (app, env string, ok bool) {
	labels := strings.SplitN(strings.ToLower(host), ".", 3)
	if len(labels) < 3 || !runs.ValidProject.MatchString(labels[0]) || (labels[1] != "preview" && labels[1] != "prod") {
		return "", "", false
	}
	return labels[0], labels[1], true
}
