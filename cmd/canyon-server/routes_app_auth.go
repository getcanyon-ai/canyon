package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"time"
)

// The app sign-in (contracts/routes.md, GET /auth/app): an app surface sends a browser without the
// app's cookie here with the app it asked for and the hash of the flow cookie it set; a signed-in
// person at viewer or above leaves with a single-use code for that app, bound to that flow.
const (
	appToCookie   = "canyon_app_to" // the app sign-in a browser without a session is on, carried through /auth/start and the callback
	appCodeWindow = 60 * time.Second
	appSessionTTL = 12 * time.Hour
)

var flowHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// invalidTo is the one refusal's message: to names an app of this host, flow the hash of its cookie.
const invalidTo = "to is an app of this host, as app.env.host, and flow the sha256 of the app's flow cookie"

// appToCookieOf names the app sign-in cookie as the sign-in state cookie is named: the __Host-
// prefix and Secure exactly when the session cookie takes them.
func appToCookieOf(public string) (name string, secure bool) {
	name, secure = sessionCookieOf(public)
	return strings.Replace(name, sessionCookie, appToCookie, 1), secure
}

// publicHost is the host, port included, of the public URL: the only host an app surface's to names.
func publicHost(public string) string {
	return strings.TrimPrefix(strings.TrimPrefix(public, "https://"), "http://")
}

// appSignIn is GET /auth/app. to must be exactly <app>.<env>.<public host> and flow 64 lower-case
// hex, else 400 invalid_to, judged before anything is read. Without a live session the pair rides a
// short-lived cookie through /auth/start, and the callback sends the browser back here, never
// through a query next. A known app is checked only for a signed-in person, so the route tells
// nobody else which apps exist. Level none goes to the no-access screen; anyone else gets a code
// good for 60 seconds, once, from the browser holding the flow cookie. No audit row.
func (a *app) appSignIn(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	to, flow := q.Get("to"), q.Get("flow")
	app, env, ok := appSurface(to)
	if !ok || to != app+"."+env+"."+publicHost(a.public) || !flowHash.MatchString(flow) {
		writeErr(w, http.StatusBadRequest, "invalid_to", invalidTo, nil)
		return
	}
	name, _ := sessionCookieOf(a.public)
	signed, _, _, signedIn, _ := a.sessionOf(ctx, r, name) // one it cannot read signs in again
	if !signedIn {
		toName, secure := appToCookieOf(a.public)
		http.SetCookie(w, &http.Cookie{Name: toName, Value: neturl.Values{"to": {to}, "flow": {flow}}.Encode(), Path: "/", MaxAge: int(signInWindow.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // Secure follows the public URL's scheme, as the session cookie's does
		http.Redirect(w, r, "/auth/start", http.StatusFound)
		return
	}
	session, _ := r.Cookie(name) // sessionOf has just read it
	if wait, ok := a.signIns.take(session.Value); !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many app sign-ins from this session", map[string]any{"retry_after_s": int(math.Ceil(wait.Seconds()))})
		return
	}
	ctx = withActor(ctx, signed)
	var live bool
	if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", app).Scan(&live); err != nil {
		a.log.Printf("canyon-server: app sign-in to %s: %v", app, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !live {
		writeErr(w, http.StatusBadRequest, "invalid_to", invalidTo, nil)
		return
	}
	level, err := levelOf(ctx, a.pool, app)
	if err != nil {
		a.log.Printf("canyon-server: app sign-in to %s: %v", app, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if level == "none" && signed.role != "admin" {
		http.Redirect(w, r, "/#/no-access/"+app, http.StatusFound) //nolint:gosec // a relative path on this host; app matched the project id pattern above
		return
	}
	code, err := token()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	id, err := token()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	codeSum, flowSum := sha256.Sum256([]byte(code)), mustHex(flow)
	if _, err := a.pool.Exec(ctx, `INSERT INTO app_sessions (id, session_id, project_id, code_hash, flow_hash, code_expires_at, expires_at)
		SELECT $1, s.id, $3, $4, $5, now() + make_interval(secs => $6), LEAST(now() + make_interval(secs => $7), s.expires_at) FROM sessions s WHERE s.id = $2`,
		id, session.Value, app, codeSum[:], flowSum, appCodeWindow.Seconds(), appSessionTTL.Seconds()); err != nil {
		a.log.Printf("canyon-server: app sign-in to %s: %v", app, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	scheme := strings.TrimSuffix(a.public, "://"+publicHost(a.public))
	http.Redirect(w, r, scheme+"://"+to+"/.canyon/callback?code="+code, http.StatusFound) //nolint:gosec // to is exactly <app>.<env>.<public host>, checked above: never another host
}

// mustHex decodes what flowHash has already matched.
func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}
