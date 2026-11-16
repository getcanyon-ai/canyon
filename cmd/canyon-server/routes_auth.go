package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// signInCookie holds one sign-in's PKCE verifier, state and nonce between /auth/start and
// /auth/callback (design/AUTH.md §4.1). It is named by the session cookie's builder, so it takes
// the __Host- prefix and Secure under https exactly when the session cookie does, and it is
// SameSite=Lax, never Strict, because it must survive the top-level navigation back from the issuer.
const (
	signInCookie = "canyon_signin"
	signInWindow = 10 * time.Minute

	// Where a refused sign-in lands. Both are ordinary hash routes of the dashboard, and the
	// redirect carries no query and no fragment of its own beyond the route: the screen renders
	// from its own reads and is told nothing about why (contracts/routes.md, the callback).
	signInRoute        = "/#/signin"
	setupRequiredRoute = "/#/setup-required"
)

func signInCookieOf(public string) (name string, secure bool) {
	name, secure = sessionCookieOf(public)
	return strings.Replace(name, sessionCookie, signInCookie, 1), secure
}

// routesAuth registers the browser sign-in (contracts/routes.md, Open): only under oidc, so with
// auth off the catch-all answers the contract's 404.
func routesAuth(mux *http.ServeMux, a *app) {
	if a.auth != authOIDC {
		return
	}
	mux.HandleFunc("GET /auth/start", a.limit.limit(a.authStart)) // the three browser and mint routes share one bucket per source address (limiter.go)
	mux.HandleFunc("GET /auth/callback", a.limit.limit(a.authCallback))
	mux.HandleFunc("POST /auth/device", a.limit.limit(a.deviceStart))
	mux.HandleFunc("POST /auth/device/token", a.deviceToken) // bounded per grant by its interval (slow_down), never per address: behind an ingress every CLI shares one
	mux.HandleFunc("GET /auth/app", a.appSignIn)             // bounded per Canyon session in the handler: each signed-in call writes a row
}

// oauth is the client the two routes share: this host's registration at the issuer. Without a
// secret it is a public PKCE client and sends its id in the body and nothing else.
func (a *app) oauth(ep oauth2.Endpoint) *oauth2.Config {
	if a.oidc.clientSecret == "" {
		ep.AuthStyle = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{ClientID: a.oidc.clientID, ClientSecret: a.oidc.clientSecret, Endpoint: ep, RedirectURL: a.public + "/auth/callback", Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
}

func (a *app) authStart(w http.ResponseWriter, r *http.Request) {
	ep, ok := a.idp.endpoint()
	if !ok { // the same answer /ready gives while discovery has not landed
		writeErr(w, http.StatusServiceUnavailable, "not_ready", "not ready", map[string]any{"waiting_on": []string{"oidc"}})
		return
	}
	opts := []oauth2.AuthCodeOption{}
	switch p := r.URL.Query()["prompt"]; { // the sign-in after a sign-out asks the issuer which account, since its own session outlives Canyon's; nothing else a caller writes reaches the issuer
	case len(p) == 0:
	case len(p) == 1 && p[0] == "select_account":
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "select_account"))
	default:
		writeErr(w, http.StatusBadRequest, "invalid_prompt", "prompt is select_account or absent", nil)
		return
	}
	var parts [3]string // verifier, state, nonce: 32 random bytes each, hex, so the cookie's shape is checked by length alone
	for i := range parts {
		var err error
		if parts[i], err = token(); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	name, secure := signInCookieOf(a.public)
	http.SetCookie(w, &http.Cookie{Name: name, Value: strings.Join(parts[:], "."), Path: "/", MaxAge: int(signInWindow.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // Secure follows the public URL's scheme, as the session cookie's does
	http.Redirect(w, r, a.oauth(ep).AuthCodeURL(parts[1], append(opts, oidc.Nonce(parts[2]), oauth2.S256ChallengeOption(parts[0]))...), http.StatusFound)
}

// authCallback finishes a sign-in. Every refusal is the same 400 with no detail, and the reason
// goes to the log: never the code, the verifier, the state, the nonce, a token or the secret. The
// membership check comes before the session row: a person the provider vouched for and Canyon refused
// writes one sign in refused row once answered, and every other failure nothing (design/AUTH.md §9).
func (a *app) authCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, secure := signInCookieOf(a.public)
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // single use: cleared on every answer, as above
	// A refusal answers a redirect, never a body: the caller is a browser following a top-level
	// navigation, and a page of machine output is not something to show a person. The reason goes
	// to the log, as it did before; none of it is echoed to the caller.
	refuseTo := func(route, reason string) {
		a.log.Printf("canyon-server: sign in refused: %s", reason)
		http.Redirect(w, r, route, http.StatusFound)
	}
	refuse := func(reason string) { refuseTo(signInRoute, reason) }
	internal := func(err error) { // the sign-in was good and the host failed it: a server error, not a refusal, so not a redirect either
		a.log.Printf("canyon-server: sign in: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	c, err := r.Cookie(name)
	if err != nil || len(c.Value) != 3*2*tokenBytes+2 {
		refuse("no sign-in cookie, or not one of ours")
		return
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(parts[1])) != 1 {
		refuse("state is not the one this sign-in issued")
		return
	}
	verifier, nonce := parts[0], parts[2]
	if r.URL.Query().Get("code") == "" {
		refuse("the issuer sent no code: error=" + oneLine(r.URL.Query().Get("error")))
		return
	}
	ep, ok := a.idp.endpoint()
	if !ok {
		refuse("oidc discovery has not completed")
		return
	}
	tok, err := a.oauth(ep).Exchange(oidc.ClientContext(ctx, a.idp.client), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		refuse("code exchange: " + oneLine(err.Error()))
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	cl, err := a.idp.verifyIDToken(ctx, raw, nonce)
	if err != nil {
		refuse("id token: " + oneLine(err.Error()))
		return
	}
	email, matched, err := emailOf(cl)
	if err != nil {
		refuse(err.Error())
		return
	}
	sub, _ := cl["sub"].(string)
	if sub == "" {
		refuse("id token has no sub")
		return
	}
	hd, _ := cl["hd"].(string)
	// An identifier names one person whole or not at all: never cut to one that names another — a
	// 200-character address with a domain after it would be cut to someone else's (only the display name,
	// which names nobody, is cut to fit).
	for name, v := range map[string]string{"sub": sub, "email": email, "hd": hd} {
		if utf8.RuneCountInString(v) > 200 {
			refuse("id token's " + name + " is over 200 characters")
			return
		}
	}
	display, _ := cl["name"].(string)
	if display = strings.TrimSpace(display); display == "" {
		display = email
	}
	display = bounded(display) // the provider's text, bounded before it becomes a column: every identifier above is whole
	id, err := token()
	if err != nil {
		internal(err)
		return
	}
	act := actor{Kind: "person", Subject: sub, Display: display}
	var turnedAway string // the reason a person the provider vouched for was refused: its row is written once the answer is, outside the transaction below (contracts/routes.md, the audit read)
	defer func() {
		if turnedAway == "" {
			return
		}
		quiet := events.WithProvenance(context.WithoutCancel(ctx), events.Provenance{Source: a.proxies.source(r)}) // no session yet: the address alone
		if err := a.inMutation(quiet, func(tx pgx.Tx) error {
			return events.AuditTx(quiet, tx, act, "sign in refused", "", "", map[string]any{"flow": "browser", "email": email, "reason": turnedAway})
		}); err != nil {
			a.log.Printf("canyon-server: the refused sign-in's row: %v", err)
		}
	}()
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := a.orgOf(ctx, tx) // after the lock a.mutation took, never before it: the preferences write takes the same lock, so read before it a write could remove the signer before the insert and land a session and a "sign in" row for somebody the row no longer admits (routes_device.go reads it the same way)
	if err != nil {
		internal(err)
		return
	}
	if !a.member(email, o) { // in setup only an admin may sign in (design/ONBOARDING.md §2): everyone else is refused here, before any session row
		if o.setup { // not this person's turn yet, and "try again" is the one instruction that cannot work: the setup screen says so instead
			refuseTo(setupRequiredRoute, oneLine(email)+" is not a bootstrap admin, and Canyon is being set up")
			turnedAway = "setup"
		} else {
			refuse(oneLine(email) + " is on neither membership list")
			turnedAway = "not_a_member"
		}
		return
	}
	// the row's claim, not the environment's: an org that names its admin group through the
	// product is governed by what it named
	admin, responder := adminClaimOf(cl, o.adminClaim, o.adminValue), adminClaimOf(cl, o.responderClaim, o.responderValue) == "hit" // the responder claim tests as the admin one
	groups, read := groupsClaimOf(cl, o.groupsClaim)
	verified := verifiedAddress(cl, matched)
	if _, err := tx.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, issuer, email, email_verified, flow, claimed_admin, claimed_responder, hosted_domain, groups, groups_read, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, 'browser', $8, $9, $10, $11, $12, now() + make_interval(secs => $13))", id, act.Kind, act.Subject, act.Display, a.oidc.issuer, email, verified, admin == "hit", responder, hd, groups, read, browserSession.Seconds()); err != nil { // the database's clock, which sessionOf compares against
		internal(err)
		return
	}
	// A grant made to this address before its person signed in is bound to their subject now, under this
	// sign-in's issuer, so it names them whatever address the identity provider gives them next, the hold's
	// team record among what reads it — but only on an address the provider says is verified, in its email
	// claim: an absent email_verified, or an address from preferred_username, is let in and binds nothing.
	// A grant bound already under this issuer, or under '' by a binary that recorded none, is never touched;
	// one bound under another is not yet bound here and is bound again, the trail naming whom it was bound to
	// (design/AUTH.md §3).
	// The first sign-in this process verifies under the configured issuer records it on every subject with none
	// (stampIssuer): a token it signed is the proof the variable names the issuer people sign in through.
	stamping, stamped := !a.idp.stamped.Load(), int64(0)
	if stamping {
		if stamped, err = stampIssuer(ctx, tx, a.oidc.issuer); err != nil {
			internal(err)
			return
		}
	}
	var bound []string
	var rebound []map[string]any
	if verified {
		rows, err := tx.Query(ctx, `WITH was AS (SELECT project_id, subject, subject_issuer FROM project_access
				WHERE kind = 'email' AND grantee = $2 AND (subject = '' OR subject_issuer NOT IN ($3, '')) FOR UPDATE)
			UPDATE project_access p SET subject = $1, subject_issuer = $3 FROM was WHERE p.project_id = was.project_id AND p.kind = 'email' AND p.grantee = $2
			RETURNING p.project_id, was.subject, was.subject_issuer`, act.Subject, email, a.oidc.issuer)
		var was []struct{ Project, Subject, Issuer string }
		if err == nil {
			was, err = pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Project, Subject, Issuer string }])
		}
		if err != nil {
			internal(err)
			return
		}
		slices.SortFunc(was, func(x, y struct{ Project, Subject, Issuer string }) int { return strings.Compare(x.Project, y.Project) })
		for _, w := range was {
			if bound = append(bound, w.Project); w.Subject != "" {
				rebound = append(rebound, map[string]any{"project": w.Project, "subject": w.Subject, "issuer": w.Issuer})
			}
		}
	}
	if err := noteBinding(ctx, tx, act.Subject, email, groups); err != nil { // a hold that recorded this address or one of these groups records whoever signs in, verified or not: under the ledger lock a.mutation took, so no hold lands between
		internal(err)
		return
	}
	signedIn := events.WithProvenance(ctx, events.Provenance{Via: "dashboard", Role: a.role(email, admin == "hit", responder, o), Session: sessionDigest(id), Source: a.proxies.source(r)}) // the session this sign-in mints is the one that acted
	if err := events.AuditTx(signedIn, tx, act, "sign in", "", "", map[string]any{"flow": "browser"}); err != nil {
		internal(err)
		return
	}
	if len(bound) > 0 { // one row for the batch, naming the apps whose grant to this address now names this person, and the issuer it is bound under
		detail := map[string]any{"email": email, "issuer": a.oidc.issuer, "projects": bound}
		if len(rebound) > 0 { // who a grant bound under another issuer named until now (contracts/routes.md, App teams)
			detail["rebound"] = rebound
		}
		if err := events.AuditTx(signedIn, tx, act, "bind grants", "", "", detail); err != nil {
			internal(err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	if stamping {
		a.idp.stamped.Store(true)
		a.log.Printf("canyon-server: %d grant and request subject(s) recorded under %s", stamped, a.oidc.issuer)
	}
	a.log.Printf("canyon-server: sign in sub=%s matched=%s %s admin_claim=%s role=%s", oneLine(sub), matched, oneLine(email), admin, a.role(email, admin == "hit", responder, o))
	if read != "ok" && read != "unset" { // the groups were dropped, and a person missing from a group grant should be traceable to why
		a.log.Printf("canyon-server: sign in sub=%s groups claim %s is %s: no groups stored", oneLine(sub), oneLine(o.groupsClaim), read)
	}
	sessionName, _ := sessionCookieOf(a.public)
	setSessionCookie(w, sessionName, id, secure)
	http.Redirect(w, r, a.afterSignIn(w, r), http.StatusFound) // never a next parameter
}

// afterSignIn is where a sign-in lands: back on GET /auth/app while an app sign-in's cookie is set
// (contracts/routes.md), clearing it, and the dashboard otherwise. Only its to and flow go back,
// and /auth/app judges both again.
func (a *app) afterSignIn(w http.ResponseWriter, r *http.Request) string {
	name, secure := appToCookieOf(a.public)
	c, err := r.Cookie(name)
	if err != nil {
		return "/"
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // single use: cleared once it has been followed
	v, err := neturl.ParseQuery(c.Value)
	if err != nil || v.Get("to") == "" || v.Get("flow") == "" {
		return "/"
	}
	return "/auth/app?" + neturl.Values{"to": {v.Get("to")}, "flow": {v.Get("flow")}}.Encode()
}
