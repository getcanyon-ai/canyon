package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
)

type (
	actorKey    struct{}
	roleKey     struct{}
	emailKey    struct{}
	groupsKey   struct{}
	verifiedKey struct{}
	readKey     struct{}
	issuerKey   struct{}
	signinKey   struct{}
	deniedKey   struct{}
)

// refusal is a 403 the request was answered with that the trail keeps (contracts/routes.md, the audit
// read: denied): its code and the app the route names, "" for none.
type refusal struct{ code, project, kind, id string }

// deny notes on the request that it was refused code: the session stage writes its row once the request
// is answered, in a transaction of its own — the refusing handler may hold the ledger lock its append takes.
func deny(ctx context.Context, code, project string) {
	if r, ok := ctx.Value(deniedKey{}).(*refusal); ok {
		*r = refusal{code: code, project: project}
	}
}

// denyOn is deny about a target that is no app, kind and id: its row names it as detail.target and no project, so
// one whose id is an app's never reads as that app's (contracts/routes.md, the audit read; #2821).
func denyOn(ctx context.Context, code, kind, id string) {
	if r, ok := ctx.Value(deniedKey{}).(*refusal); ok {
		*r = refusal{code: code, kind: kind, id: id}
	}
}

// serveNoting serves the request with room for a refusal and, when it was refused, writes the denied row
// after: its actor and provenance the request's, its action the route pattern (contracts/routes.md, the audit read).
func (a *app) serveNoting(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var refused refusal
	req := r.WithContext(context.WithValue(r.Context(), deniedKey{}, &refused))
	next.ServeHTTP(w, req)
	if refused.code == "" {
		return
	}
	ctx := context.WithoutCancel(req.Context()) // written before the buffered answer leaves, so who reads the answer can read the row; a client gone since does not unwrite the attempt
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		detail := map[string]any{"action": action(req), "error": refused.code}
		if refused.kind != "" {
			detail["target"] = map[string]string{"kind": refused.kind, "id": refused.id}
		}
		return events.AuditTx(ctx, tx, actorOf(ctx), "denied", refused.project, "", detail)
	})
	if err != nil {
		a.log.Printf("canyon-server: the denied row of %s %s: %v", req.Method, req.URL.Path, err)
	}
}

// actorOf is the request's actor, set by the session stage.
func actorOf(ctx context.Context) actor {
	a, _ := ctx.Value(actorKey{}).(actor)
	return a
}

// roleOf is the request's role, set beside the actor by the session stage.
func roleOf(ctx context.Context) string {
	role, _ := ctx.Value(roleKey{}).(string)
	return role
}

// addressOf is the address the request's session stores, carried beside the role because the role is
// computed from it and GET /preferences answers both (contracts/routes.md row 219). Empty on a demo
// host, whose sessions store no address: there is no viewer to answer, and the read omits the key.
func addressOf(ctx context.Context) string {
	email, _ := ctx.Value(emailKey{}).(string)
	return email
}

// issuerOf is the issuer the request's subject is read under, Canyon's own sign-in's, or noIssuer with sign-in
// off: a grant bound under any other is read as not yet bound, and one with none recorded as bound under it
// (design/PERMISSIONS.md, Grants).
func issuerOf(ctx context.Context) string {
	issuer, _ := ctx.Value(issuerKey{}).(string)
	return issuer
}

// groupsOf is the identity-provider groups the request's session stored at sign-in, never nil: a
// group grant matches a person whose session carries its group (contracts/routes.md, App teams).
func groupsOf(ctx context.Context) []string {
	if groups, _ := ctx.Value(groupsKey{}).([]string); groups != nil {
		return groups
	}
	return []string{}
}

// org is the preferences row as a request is judged by it: the display name, the two membership
// lists, the admin list, the claim that means admin, the responder list and claim, and whether the host
// is still in setup (onboarded_at null, design/ONBOARDING.md §2).
type org struct {
	name                           string
	domains, emails, admins        []string
	adminClaim, adminValue         string
	responders                     []string
	responderClaim, responderValue string
	groupsClaim                    string
	setup                          bool
}

// orgOf reads the row for the sign-in routes; the session stage reads the same columns beside the
// session in its own SELECT (design/AUTH.md §3: a join on every request, never a cache).
func (a *app) orgOf(ctx context.Context, q querier) (org, error) {
	var o org
	err := q.QueryRow(ctx, "SELECT org_name, allowed_domains, allowed_emails, admin_emails, coalesce(admin_claim, ''), coalesce(admin_value, ''), responder_emails, coalesce(responder_claim, ''), coalesce(responder_value, ''), groups_claim, onboarded_at IS NULL FROM org_preferences").
		Scan(&o.name, &o.domains, &o.emails, &o.admins, &o.adminClaim, &o.adminValue, &o.responders, &o.responderClaim, &o.responderValue, &o.groupsClaim, &o.setup)
	return a.withEnvironment(o), err
}

// withEnvironment is the row with the environment's value over every key it sets: the variables
// are bootstrap and override (design/ONBOARDING.md §1), so a list the operator pinned is the list.
// The admin list is the exception, and a union (canyonv2-cr23): admin is granted by CANYON_ADMINS,
// by the row's list, or by the claim, and neither source replaces the other. Overriding it left
// onboarding's "add an admin" read-only on every oidc host; bootstrap-only would take away the
// environment edit design/AUTH.md documents as the recovery from a wrong list.
func (a *app) withEnvironment(o org) org {
	if a.orgEnv != "" {
		o.name = a.orgEnv
	}
	if o.name == "" { // the column defaults to empty, and the receipt's org is never blank
		o.name = defaultOrg
	}
	if c := a.oidc; c != nil {
		if c.domains != nil {
			o.domains = c.domains
		}
		if c.emails != nil {
			o.emails = c.emails
		}
		o.admins = union(o.admins, c.admins)
		if c.adminClaim != "" {
			o.adminClaim, o.adminValue = c.adminClaim, c.adminValue
		}
		if c.responderClaim != "" {
			o.responderClaim, o.responderValue = c.responderClaim, c.responderValue
		}
		if c.groupsClaim != "" {
			o.groupsClaim = c.groupsClaim
		}
	}
	return o
}

// orgNow is the row as it stands, for its display name — the row's, CANYON_ORG's where that is set,
// the default where neither names one — and whether the host is in setup. Read per request rather
// than stamped at boot — an org that onboards as Acme must not get a promote receipt saying
// Meridian (design/ONBOARDING.md §1).
func (a *app) orgNow(ctx context.Context) org {
	o, err := a.orgOf(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: org name: %v", err)
		return a.withEnvironment(org{}) // the configured name, and not in setup: a display string is not worth failing the read for
	}
	return o
}

// member is whether email may use this host: on one of the two membership lists — or, while the
// host is in setup, on CANYON_ADMINS, the only people who may sign in until somebody has said who
// else may (design/ONBOARDING.md §2).
func (a *app) member(email string, o org) bool {
	if o.setup {
		return slices.Contains(o.admins, strings.ToLower(email))
	}
	return memberOf(email, o.domains, o.emails)
}

// role is the actor's role, computed on every request and never stored (design/AUTH.md §4.4):
// admin when the mode is not oidc, as every caller on a demo host is; under oidc, admin when the
// session's stored email is on the admin list or the row's claimed_admin is true; else responder when
// it is on the responder list or the row's claimed_responder is true (design/PERMISSIONS.md, Security
// responders); member otherwise. The lists are read live, so a demotion lands on the person's next request.
func (a *app) role(email string, claimedAdmin, claimedResponder bool, o org) string {
	switch {
	case a.auth != authOIDC || claimedAdmin || slices.Contains(o.admins, strings.ToLower(email)):
		return "admin"
	case claimedResponder || slices.Contains(o.responders, strings.ToLower(email)):
		return "responder"
	}
	return "member"
}

// adminOnly is the org-wide writes' refusal (contracts/routes.md): a member is answered before the
// handler reads anything, so nothing is written but the denied row after the answer.
func (a *app) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if roleOf(r.Context()) != "admin" {
			a.adminRequired(w, r, "")
			return
		}
		next(w, r)
	}
}

// adminRequired answers 403 admin_required to a member, asking the first of the org's admin addresses,
// which the preferences read answers every member (contracts/routes.md, App teams).
func (a *app) adminRequired(w http.ResponseWriter, r *http.Request, project string) {
	writeErr(w, http.StatusForbidden, "admin_required", "only an admin can do this: ask "+a.adminToAsk(r.Context()), nil)
	deny(r.Context(), "admin_required", project)
}

// adminToAsk is the admin a refusal names (contracts/routes.md, admin_required): the first address, sorted, of
// admin_emails and bootstrap_admins together, else "an admin".
func (a *app) adminToAsk(ctx context.Context) string {
	if admins := a.orgNow(ctx).admins; len(admins) > 0 {
		return slices.Min(admins)
	}
	return "an admin"
}

const sessionCookie = "canyon_session"

// sessionCookieOf names the session cookie for this host and says whether it carries Secure. Both
// follow the public URL's scheme and never r.TLS: this server terminates no TLS — production ends
// it at the ingress — so the request would say plain on every deployment. Under https the name
// takes the __Host- prefix, which the browser itself refuses to let a subdomain set for the parent,
// so a preview can neither plant a session nor shadow the real one; under http the prefix is
// invalid without Secure, so the demo host keeps the plain name. One function, so the name a
// request is read under is the name it was set under.
func sessionCookieOf(public string) (name string, secure bool) {
	if strings.HasPrefix(strings.ToLower(public), "https://") {
		return "__Host-" + sessionCookie, true
	}
	return sessionCookie, false
}

// unsafeMethod is a method that changes something. Reads are left alone: the refusal below is about
// the mutation a page on another origin can have the browser send with the cookie attached, and a
// GET or HEAD changes nothing here.
func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// fromCanyon is whether the browser says this request came from Canyon's own pages
// (contracts/routes.md). Sec-Fetch-Site decides alone when it is present: it is a forbidden header
// name, so page script cannot set it, and the browser's own site computation beats any we write —
// it is also what calls the vite dev proxy's request from :5173 same-origin, which an Origin check
// would refuse. Only when the browser sent none does Origin have to be this host's public URL; a
// sandboxed frame's `null` is present and unequal, so it is refused. A client that sends neither is
// not a browser: the CLI, and the GitHub courier, which is signature-authenticated on a route
// outside /api/v1 regardless.
func fromCanyon(r *http.Request, public string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return originOf(origin) == originOf(public)
	}
	return true
}

// originOf lowercases an origin and drops the port its scheme implies, so an operator who wrote
// CANYON_PUBLIC_URL=https://canyon.acme:443 — publicURLOf permits the explicit port and a browser
// omits it — does not have every mutation refused.
func originOf(origin string) string {
	origin = strings.ToLower(origin)
	if strings.HasPrefix(origin, "https://") {
		return strings.TrimSuffix(origin, ":443")
	}
	if strings.HasPrefix(origin, "http://") {
		return strings.TrimSuffix(origin, ":80")
	}
	return origin
}

// session is the pipeline's session stage for /api/v1: a mutation the browser reports as coming
// from anywhere but this origin is refused here, before anything is read or written; a request with
// no valid session is refused with sign-in on, and with it off (off, demo) served as the anonymous
// admin, no session row or cookie made (contracts/routes.md); the actor rides in the context from
// here on.
// Session lifetimes (design/AUTH.md §5): absolute, no idle timeout. A CLI session is a bearer
// token in a file on a laptop, and how soon it dies is the only bound on a stolen one.
const (
	browserSession = 12 * time.Hour
	deviceSession  = 7 * 24 * time.Hour
)

func (a *app) session(next http.Handler) http.Handler {
	name, _ := sessionCookieOf(a.public)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unsafeMethod(r.Method) && !fromCanyon(r, a.public) { // before the cookie read and before any SQL: a refused request is a log line, never a session row
			writeErr(w, http.StatusForbidden, "cross_origin", "this request did not come from Canyon", nil)
			return
		}
		ctx := r.Context()
		signed, setup, prov, ok, err := a.sessionOf(ctx, r, name)
		if ok {
			a.serveSigned(w, r, signed, setup, prov, next)
			return
		}
		if err != nil && a.auth == authOIDC { // the database didn't answer: the session may live, so never the 401 that says it ended (#153); with sign-in off every caller is the anonymous admin, as below
			a.log.Printf("canyon-server: session: %v", err)
			writeErr(w, http.StatusServiceUnavailable, "session_unread", "Canyon couldn't check your sign-in just now. Try again in a moment.", nil)
			return
		}
		if a.auth == authOIDC {
			if r.Method == http.MethodDelete && r.URL.Path == "/sessions" && !r.URL.Query().Has("subject") {
				clearSession(w, a.public) // a sign-out of a session that ended: the browser keeps nothing of it
			} // sessions are minted only by the sign-in routes (contracts/routes.md): the refusal comes before the handler and before the catch-all, with no row of any kind
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to Canyon", nil)
			return
		}
		// A host with sign-in off (off, demo) serves a caller with no session as the anonymous admin and
		// stores nothing: every caller is the same actor, so a row or a cookie would add nothing but a
		// table anyone can grow.
		role := a.role("", false, false, org{}) // no session, so no channel to name: its rows carry no via
		a.serveNoting(w, r.WithContext(events.WithProvenance(withActor(ctx, signedSession{act: actor{Kind: "person", Subject: anonymousSubject}, role: role, issuer: a.bindingIssuer()}), events.Provenance{Role: role, Source: a.proxies.source(r)})), next)
	})
}

// serveSigned is the session stage's for a request whose session it has read, valid: the person and how they came
// ride in the context from here on, and until setup is complete only the preferences read and the PUT that completes
// it are served. The cookie's requests and /mcp's tools (mcp.go) both come through here.
func (a *app) serveSigned(w http.ResponseWriter, r *http.Request, signed signedSession, setup bool, prov events.Provenance, next http.Handler) {
	ctx := events.WithProvenance(withActor(r.Context(), signed), prov)
	if setup && a.auth == authOIDC && (r.URL.Path != "/preferences" || (r.Method != http.MethodGet && r.Method != http.MethodPut)) { // the fleet is not served until setup is complete (contracts/routes.md, the setup state): every route but the preferences read and the PUT that completes it, after the session, before the handler, no audit row, the pipeline's one log line naming the person
		writeErr(w, http.StatusConflict, "setup_required", "Canyon is being set up", nil)
		return
	}
	a.serveNoting(w, r.WithContext(ctx), next)
}

// setSessionCookie sets the session cookie the browser sign-in issues: no
// Max-Age, since the row's expiry bounds it, and never a Domain: host-only is half of what stops a
// preview planting one.
func setSessionCookie(w http.ResponseWriter, name, id string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // Secure follows the public URL's scheme, never the request's: demo hosts serve plain HTTP on loopback, where a Secure cookie would never come back (design/ARCHITECTURE.md)
}

// sessionOf is the request's session, valid: the row exists, is not revoked, has not expired,
// and under oidc its stored email is still on one of the two membership lists. The lists are read
// on every request, joined from the one-row preferences table in the same SELECT, so a person
// taken off one is out of Canyon on their next call, on every session they hold, with no revoke
// step to remember and no cache to invalidate (design/AUTH.md §5). setup is the host's state;
// groups are the session's, stored at sign-in.
func (a *app) sessionOf(ctx context.Context, r *http.Request, name string) (s signedSession, setup bool, prov events.Provenance, ok bool, err error) {
	c, err := r.Cookie(name)
	if err != nil || len(c.Value) != 2*tokenBytes { // a cookie of any other length is not one of ours: it never reaches the database
		return s, false, prov, false, nil
	}
	s, setup, flow, ok, err := a.readSession(ctx, c.Value)
	if flow == "oauth" { // bound to the MCP resource and its client (design/AUTH.md §4.7): never a cookie's, anywhere
		return signedSession{}, false, prov, false, nil
	}
	// how and from where the request's audit rows were done (contracts/routes.md, the audit read)
	return s, setup, events.Provenance{Via: viaOf[flow], Role: s.role, Session: sessionDigest(c.Value), Source: a.proxies.source(r)}, ok, err
}

// clearSession clears the session cookie, with its own attributes.
func clearSession(w http.ResponseWriter, public string) {
	name, secure := sessionCookieOf(public)
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // the session cookie's own attributes, as the callback clears its sign-in cookie
}

// signedSession is who a session is, as the session stage carries it beside the request: the actor, their
// role, the address the session stores, whether the provider asserted it verified at sign-in, their groups,
// and the issuer their subject is read under.
type signedSession struct {
	act      actor
	role     string
	email    string
	verified bool
	groups   []string
	read     bool   // the sign-in read its groups whole: an overage or an absent claim did not
	issuer   string // Canyon's sign-in's, or noIssuer (issuerOf)
	id       string // the session row's id, its secret; "" for the anonymous admin with sign-in off, who has none
}

// viaOf is the channel a session's flow is (contracts/routes.md, the audit read): a browser's sign-in is the
// dashboard, a device grant's the CLI, an OAuth client's MCP.
var viaOf = map[string]string{"browser": "dashboard", "device": "cli", "oauth": "mcp"}

// sessionDigest names a session on the trail without its secret: ses_ and the first 16 hex of its sha256.
func sessionDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "ses_" + hex.EncodeToString(sum[:8])
}

// sessionFor is sessionOf for a session named by its id: an app session's (surfaces.go). One it can't read is no
// session to its callers, which fail closed.
func (a *app) sessionFor(ctx context.Context, id string) (s signedSession, setup bool, flow string, ok bool) {
	s, setup, flow, ok, _ = a.readSession(ctx, id)
	return s, setup, flow, ok
}

// readSession reads the session named by its id: err when the database answered an error, never when it has no such
// session, so the stage tells a failed read from a session that ended (#153).
func (a *app) readSession(ctx context.Context, id string) (s signedSession, setup bool, flow string, ok bool, err error) {
	var claimedAdmin, claimedResponder, stale bool
	var o org
	if err := a.pool.QueryRow(ctx, "SELECT s.actor_kind, s.actor_subject, s.actor_display, s.email, s.email_verified, s.claimed_admin, s.claimed_responder, s.groups, s.groups_read = 'ok', s.flow, s.issuer, o.allowed_domains, o.allowed_emails, o.admin_emails, o.responder_emails, o.onboarded_at IS NULL, coalesce(s.last_seen_at < now() - interval '1 hour', true) FROM sessions s CROSS JOIN org_preferences o WHERE s.id = $1 AND s.revoked_at IS NULL AND s.expires_at > now()", id).
		Scan(&s.act.Kind, &s.act.Subject, &s.act.Display, &s.email, &s.verified, &claimedAdmin, &claimedResponder, &s.groups, &s.read, &flow, &s.issuer, &o.domains, &o.emails, &o.admins, &o.responders, &o.setup, &stale); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		}
		return signedSession{}, false, "", false, err
	}
	if a.auth == authOIDC && s.issuer != "" && s.issuer != a.oidc.issuer { // minted through another issuer: its subject may be another person's under this one, so it signs in again (#2306); one recording none is this issuer's, as a grant's is
		return signedSession{}, false, "", false, nil
	}
	o = a.withEnvironment(o)
	if a.auth == authOIDC && !a.member(s.email, o) {
		return signedSession{}, false, "", false, nil
	}
	s.role, s.issuer, s.id = a.role(s.email, claimedAdmin, claimedResponder, o), a.bindingIssuer(), id
	if stale { // seen, at most hourly, for the usage report's people and agents active (#2953): a failed touch costs that count alone, never the request
		_, _ = a.pool.Exec(ctx, "UPDATE sessions SET last_seen_at = now() WHERE id = $1", id)
	}
	return s, o.setup, flow, true, nil
}

// sweepSessions deletes the expired rows: it runs on the boot path that runs migrations, and
// nothing else sweeps them (design/AUTH.md §5).
func sweepSessions(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= now()")
	return tag.RowsAffected(), err
}

// withActor puts the actor where actorOf finds it and, when the pipeline left a holder, where
// the log line finds it. The role, the address, the groups, the issuer and the row's id ride beside it: all
// but the issuer are read off the session row the stage has just read, and none is worth a second query in a handler.
func withActor(ctx context.Context, s signedSession) context.Context {
	if who, ok := ctx.Value(whoKey{}).(*actor); ok {
		*who = s.act
	}
	for _, kv := range []struct{ key, value any }{{actorKey{}, s.act}, {roleKey{}, s.role}, {emailKey{}, s.email}, {groupsKey{}, s.groups}, {verifiedKey{}, s.verified}, {readKey{}, s.read}, {issuerKey{}, s.issuer}, {signinKey{}, s.id}} {
		ctx = context.WithValue(ctx, kv.key, kv.value)
	}
	return ctx
}

// signinOf is the id of the request's session row: what a database session records its sign-ins by
// (design/DATABASE-SESSIONS.md §12). "" with sign-in off, where the anonymous admin has no row. Never
// logged or answered: it is the session's secret.
func signinOf(ctx context.Context) string {
	id, _ := ctx.Value(signinKey{}).(string)
	return id
}

// groupsReadOf is whether the request's session read its groups whole at sign-in.
func groupsReadOf(ctx context.Context) bool {
	read, _ := ctx.Value(readKey{}).(bool)
	return read
}

// verifiedAddress is the one rule for whether a sign-in's address may name its person by subject: the
// email claim matched and the provider asserted it verified, email_verified the boolean true. An absent
// email_verified (Entra) or an address from preferred_username signs in, and binds nothing
// (design/AUTH.md §3).
func verifiedAddress(cl claims, matched string) bool {
	verified, _ := cl["email_verified"].(bool)
	return verified && matched == "email"
}

// anonymousSubject is the subject of the one caller a host with sign-in off has, its anonymous admin.
const anonymousSubject = "anonymous"

// noIssuer is what a subject is bound under with sign-in off: never an issuer, which is an https URL, so a
// grant bound there is read as not yet bound once sign-in is on, and the anonymous admin's subject never
// names an identity provider's account (design/PERMISSIONS.md, Grants).
const noIssuer = "none"

// A grant's or access request's subject with no issuer recorded (0091) — bound before issuers were, or by a
// binary from before them mid-upgrade — is read as the current issuer's, as that binary reads it. The two stamps
// record that on it where it is certain, so a later switch of CANYON_OIDC_ISSUER reads it as the old issuer's:
// at the start for the anonymous admin's, and at the first sign-in a process verifies for every other — never on
// discovery alone, which a wrong but real issuer passes (design/PERMISSIONS.md, Grants).

// stampAnonymous records noIssuer on the anonymous admin's subjects, which only sign-in off binds: on the boot
// path, before the listener, whatever the mode, so sign-in never reads one as an identity provider's.
func stampAnonymous(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `WITH g AS (UPDATE project_access SET subject_issuer = $2 WHERE subject = $1 AND subject_issuer = '' RETURNING 1)
		UPDATE access_requests SET subject_issuer = $2 WHERE subject = $1 AND subject_issuer = ''`, anonymousSubject, noIssuer)
	return err
}

// stampIssuer records issuer on every other subject with none, and answers how many: in the transaction of the
// first sign-in this process verifies an ID token for under issuer (authCallback).
func stampIssuer(ctx context.Context, q querier, issuer string) (int64, error) {
	var stamped int64
	err := q.QueryRow(ctx, `WITH g AS (UPDATE project_access SET subject_issuer = CASE WHEN subject = $2 THEN $3 ELSE $1 END WHERE subject <> '' AND subject_issuer = '' RETURNING 1),
		r AS (UPDATE access_requests SET subject_issuer = CASE WHEN subject = $2 THEN $3 ELSE $1 END WHERE subject <> '' AND subject_issuer = '' RETURNING 1)
		SELECT (SELECT count(*) FROM g) + (SELECT count(*) FROM r)`, issuer, anonymousSubject, noIssuer).Scan(&stamped)
	return stamped, err
}

// signinIssuer is the issuer of Canyon's own sign-in, CANYON_OIDC_ISSUER, which every subject it binds is
// from; "" with sign-in off.
func (a *app) signinIssuer() string {
	if a.oidc == nil {
		return ""
	}
	return a.oidc.issuer
}

// bindingIssuer is what this host binds a subject under: Canyon's sign-in's issuer, noIssuer with sign-in off.
func (a *app) bindingIssuer() string {
	return cmp.Or(a.signinIssuer(), noIssuer)
}

// boundIssuer is the issuer a grant written for the request's person records beside boundSubject: issuerOf's
// with a subject, none without one.
func (a *app) boundIssuer(ctx context.Context) string {
	if a.boundSubject(ctx) == "" {
		return ""
	}
	return issuerOf(ctx)
}

// boundSubject is the subject a grant written for the request's person may carry: theirs when their
// session's address is verified (verifiedAddress, stored at sign-in), on a host with sign-in off the
// anonymous admin's, else "" — the grant names them by address alone. Every path that writes a grant's
// subject takes it from here, or from the sign-in's own verifiedAddress.
func (a *app) boundSubject(ctx context.Context) string {
	if verified, _ := ctx.Value(verifiedKey{}).(bool); verified || a.auth != authOIDC {
		return actorOf(ctx).Subject
	}
	return ""
}

const tokenBytes = 32

func token() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
