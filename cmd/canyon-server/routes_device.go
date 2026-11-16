package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// The device flow (design/AUTH.md §4.2, RFC 8628): the CLI mints a grant, a signed-in browser
// approves its user code at /#/device, and the CLI's poll is answered with a session copied from
// the approver's row. The table holds the device code's sha256 only; the insert deletes every expired row.
const (
	deviceCodeLife = 600 * time.Second
	pollInterval   = 5 * time.Second
	crockford      = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeGone       = "that code has expired: run canyon login again"                                    // the terminal's, on the poll
	codeUnknown    = "that code is not valid or has already been used. Check the code in your terminal" // the browser's, on the approval routes
	codeInBrowser  = "open the link canyon login printed in your browser, signed in to Canyon, to approve or deny a CLI sign-in"
)

// grants is what the table does not hold: when a pending code was last polled (slow_down under the
// interval) and the codes a person denied (access_denied on the poll after). This process's memory.
type grants struct {
	mu    sync.Mutex
	marks map[[32]byte]mark
}

type mark struct {
	at     time.Time
	denied bool
}

// set records m for hash and answers what was there; a new key sweeps the marks older than a code's life.
func (g *grants) set(hash [32]byte, m mark) (prev mark, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.marks == nil {
		g.marks = map[[32]byte]mark{}
	}
	if prev, ok = g.marks[hash]; !ok {
		for k, old := range g.marks {
			if m.at.Sub(old.at) > deviceCodeLife {
				delete(g.marks, k)
			}
		}
	}
	g.marks[hash] = m
	return prev, ok
}

func (g *grants) drop(hash [32]byte) (denied bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	denied = g.marks[hash].denied
	delete(g.marks, hash)
	return denied
}

func routesDeviceGrants(api *http.ServeMux, a *app) {
	if a.auth != authOIDC {
		return
	}
	api.HandleFunc("GET /device-grants/{user_code}", browserOnly(a.getDeviceGrant))
	api.HandleFunc("POST /device-grants/{user_code}/approve", browserOnly(a.approveDeviceGrant))
	api.HandleFunc("POST /device-grants/{user_code}/deny", browserOnly(a.denyDeviceGrant))
}

// browserOnly refuses a CLI session before the grant is read (design/AUTH.md §4.2): the session an
// approval copies would be a fresh CLI one, so a stolen CLI token could renew itself forever.
func browserOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromDashboard(r.Context()) {
			writeErr(w, http.StatusForbidden, "dashboard_required", codeInBrowser, nil)
			deny(r.Context(), "dashboard_required", "")
			return
		}
		next(w, r)
	}
}

func userCodeOf(s string) string { // Crockford leaves out I, L and O, so a person who typed them meant 1 and 0
	return strings.NewReplacer("O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s)))
}

func (a *app) deviceStart(w http.ResponseWriter, r *http.Request) {
	var raw [tokenBytes + 8]byte // the device code's bytes, then the user code's
	if _, err := rand.Read(raw[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	code := hex.EncodeToString(raw[:tokenBytes])
	user := make([]byte, 8)
	for i, b := range raw[tokenBytes:] {
		user[i] = crockford[b&31] // 32 symbols divide 256: no bias
	}
	hash := sha256.Sum256([]byte(code))
	if _, err := a.pool.Exec(r.Context(), "WITH gone AS (DELETE FROM device_grants WHERE expires_at <= now()) INSERT INTO device_grants (code_hash, user_code, requested_by, expires_at) VALUES ($1, $2, $3, now() + make_interval(secs => $4))", hash[:], string(user), a.limit.from.source(r), deviceCodeLife.Seconds()); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	shown := string(user[:4]) + "-" + string(user[4:])
	writeJSON(w, http.StatusOK, map[string]any{"device_code": code, "user_code": shown, "verification_uri": a.public + "/#/device", "verification_uri_complete": a.public + "/#/device?code=" + shown, "expires_in": int(deviceCodeLife.Seconds()), "interval": int(pollInterval.Seconds())})
}

func (a *app) deviceToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: device sign in: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	refuse := func(code, message string) { writeErr(w, http.StatusBadRequest, code, message, nil) }
	hash := sha256.Sum256([]byte(body.DeviceCode))
	var approved, expired bool
	err := a.pool.QueryRow(ctx, "SELECT session_id IS NOT NULL, expires_at <= now() FROM device_grants WHERE code_hash = $1", hash[:]).Scan(&approved, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if a.grants.drop(hash) {
			refuse("access_denied", "the person denied that code")
		} else {
			refuse("expired_token", codeGone)
		}
		return
	case err != nil:
		internal(err)
		return
	case expired:
		_, _ = a.pool.Exec(ctx, "DELETE FROM device_grants WHERE code_hash = $1", hash[:]) // the next insert would anyway
		a.grants.drop(hash)
		refuse("expired_token", codeGone)
		return
	case !approved:
		if prev, ok := a.grants.set(hash, mark{at: time.Now()}); ok && time.Since(prev.at) < pollInterval {
			refuse("slow_down", "poll every 5 seconds")
		} else {
			refuse("authorization_pending", "the person has not approved that code yet")
		}
		return
	}
	id, err := token()
	if err != nil {
		internal(err)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var act actor
	var issuer, email, hd string
	var claimedAdmin, claimedResponder, verified bool
	var groupsRead string
	var groups []string // the approver's, as their sign-in stored them: the device session is that person
	// One statement consumes the grant and reads the approver's row: a second redemption, or an approver signed out since, finds nothing.
	if err := tx.QueryRow(ctx, "DELETE FROM device_grants g USING sessions s WHERE g.code_hash = $1 AND g.expires_at > now() AND s.id = g.session_id AND s.revoked_at IS NULL AND s.expires_at > now() RETURNING s.actor_kind, s.actor_subject, s.actor_display, s.issuer, s.email, s.email_verified, s.claimed_admin, s.claimed_responder, s.hosted_domain, s.groups, s.groups_read", hash[:]).Scan(&act.Kind, &act.Subject, &act.Display, &issuer, &email, &verified, &claimedAdmin, &claimedResponder, &hd, &groups, &groupsRead); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			refuse("expired_token", codeGone)
		} else {
			internal(err)
		}
		return
	}
	var expires time.Time
	if err := tx.QueryRow(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, issuer, email, email_verified, flow, claimed_admin, claimed_responder, hosted_domain, groups, groups_read, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, 'device', $8, $9, $10, $11, $12, now() + make_interval(secs => $13)) RETURNING expires_at", id, act.Kind, act.Subject, act.Display, issuer, email, verified, claimedAdmin, claimedResponder, hd, groups, groupsRead, deviceSession.Seconds()).Scan(&expires); err != nil {
		internal(err)
		return
	}
	o, err := a.orgOf(ctx, tx) // the role the log line names: the approver's session passed the stage, so this is a read for the record
	if err != nil {
		internal(err)
		return
	}
	signedIn := events.WithProvenance(ctx, events.Provenance{Via: "cli", Role: a.role(email, claimedAdmin, claimedResponder, o), Session: sessionDigest(id), Source: a.proxies.source(r)}) // the session this redemption mints is the one that acted
	if err := events.AuditTx(signedIn, tx, act, "sign in", "", "", map[string]any{"flow": "device"}); err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	a.grants.drop(hash)
	a.log.Printf("canyon-server: sign in sub=%s %s flow=device role=%s", oneLine(act.Subject), oneLine(email), a.role(email, claimedAdmin, claimedResponder, o))
	writeJSON(w, http.StatusOK, map[string]any{"session": id, "expires_at": expires.UTC().Format(time.RFC3339), "subject": act.Subject, "display": act.Display, "org": o.name})
}

func (a *app) getDeviceGrant(w http.ResponseWriter, r *http.Request) {
	var by string
	var at, expires time.Time
	if err := a.pool.QueryRow(r.Context(), "SELECT requested_by, created_at, expires_at FROM device_grants WHERE user_code = $1 AND session_id IS NULL AND expires_at > now()", userCodeOf(r.PathValue("user_code"))).Scan(&by, &at, &expires); err != nil {
		a.answerGrant(w, 0, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requested_by": by, "requested_at": at.UTC().Format(time.RFC3339), "expires_at": expires.UTC().Format(time.RFC3339), "approver": map[string]string{"display": actorOf(r.Context()).Display, "email": addressOf(r.Context())}}) // approving hands this person's access over, so the page names them
}

func (a *app) approveDeviceGrant(w http.ResponseWriter, r *http.Request) {
	name, _ := sessionCookieOf(a.public)
	c, err := r.Cookie(name) // the session stage accepted this cookie, so it is here; read for what it is all the same
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to Canyon", nil)
		return
	}
	tag, err := a.pool.Exec(r.Context(), "UPDATE device_grants SET session_id = $1 WHERE user_code = $2 AND session_id IS NULL AND expires_at > now()", c.Value, userCodeOf(r.PathValue("user_code")))
	a.answerGrant(w, tag.RowsAffected(), err)
}

func (a *app) denyDeviceGrant(w http.ResponseWriter, r *http.Request) {
	var hash []byte
	err := a.pool.QueryRow(r.Context(), "DELETE FROM device_grants WHERE user_code = $1 AND session_id IS NULL AND expires_at > now() RETURNING code_hash", userCodeOf(r.PathValue("user_code"))).Scan(&hash)
	if err == nil {
		a.grants.set([32]byte(hash), mark{at: time.Now(), denied: true})
		a.log.Printf("canyon-server: device code denied by sub=%s", oneLine(actorOf(r.Context()).Subject))
	}
	a.answerGrant(w, 1, err)
}

func (a *app) answerGrant(w http.ResponseWriter, rows int64, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && rows == 0):
		writeErr(w, http.StatusNotFound, "not_found", codeUnknown, nil)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
