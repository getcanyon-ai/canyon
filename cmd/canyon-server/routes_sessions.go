package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
)

func routesSessions(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /sessions", a.listSessions)
	api.HandleFunc("DELETE /sessions", a.deleteSessions)
	api.HandleFunc("DELETE /sessions/{id}", browserOnly(a.deleteSession))
}

// deleteSession ends one live session by the digest the list shows: the reader's own, or anyone's to an admin, from
// a browser alone, so neither a CLI nor an MCP client ever ends a session. Another person's, or none, is 404.
func (a *app) deleteSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if actorOf(ctx).Kind != "person" {
		writeErr(w, http.StatusForbidden, "person_required", "A person disconnects a session, in Canyon's dashboard.", nil)
		deny(ctx, "person_required", "")
		return
	}
	admin := roleOf(ctx) == "admin"
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var flow, subject string
	var client *string
	err = tx.QueryRow(ctx, `UPDATE sessions SET revoked_at = now() WHERE revoked_at IS NULL AND expires_at > now() AND ($2 OR actor_subject = $3)
		AND 'ses_' || left(encode(sha256(convert_to(id, 'UTF8')), 'hex'), 16) = $1 RETURNING flow, actor_subject, oauth_client_name`,
		r.PathValue("id"), admin, actorOf(ctx).Subject).Scan(&flow, &subject, &client)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such session", nil)
		return
	}
	detail := map[string]any{"session": r.PathValue("id"), "flow": flow}
	if client != nil {
		detail["client"] = *client
	}
	if subject != actorOf(ctx).Subject { // an admin's, of someone else's
		detail["subject"] = subject
	}
	action := "revoke session"
	if flow == "oauth" {
		action = "disconnect agent"
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), action, "", "", detail)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listedSession is one live session as the list answers it (contracts/routes.md, the sessions list): named by its
// digest, never its secret; how it signed in, and an OAuth client's name; and, to an admin, whose it is.
type listedSession struct {
	ID        string         `json:"id"`
	Flow      string         `json:"flow"`
	Client    *string        `json:"client,omitempty"`
	CreatedAt string         `json:"created_at"`
	LastSeen  *string        `json:"last_seen_at"`
	ExpiresAt string         `json:"expires_at"`
	Current   bool           `json:"current"`
	Person    *sessionPerson `json:"person,omitempty"`
}

type sessionPerson struct {
	Display string `json:"display"`
	Email   string `json:"email"`
}

// listSessions is the reader's own live sessions, and every one to an admin, newest first.
func (a *app) listSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := roleOf(ctx) == "admin"
	rows, err := a.pool.Query(ctx, `SELECT id, flow, oauth_client_name, created_at, last_seen_at, expires_at, actor_display, email FROM sessions
		WHERE revoked_at IS NULL AND expires_at > now() AND ($1 OR actor_subject = $2) ORDER BY created_at DESC, id`, admin, actorOf(ctx).Subject)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	current := events.ProvenanceOf(ctx).Session
	out := []listedSession{}
	var id, flow, display, email string
	var client *string
	var created, expires time.Time
	var seen *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &flow, &client, &created, &seen, &expires, &display, &email}, func() error {
		s := listedSession{ID: sessionDigest(id), Flow: flow, Client: client, CreatedAt: stamp(created), ExpiresAt: stamp(expires)}
		s.Current = s.ID == current
		if seen != nil {
			at := stamp(*seen)
			s.LastSeen = &at
		}
		if admin {
			s.Person = &sessionPerson{Display: display, Email: email}
		}
		out = append(out, s)
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// Without subject the caller signs out; with one an admin revokes every live session whose actor
// subject or email is it, and the row records the act, not the count (contracts/routes.md).
func (a *app) deleteSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := r.URL.Query().Get("subject")
	if subject != "" && roleOf(ctx) != "admin" {
		a.adminRequired(w, r, "")
		return
	}
	name, secure := sessionCookieOf(a.public)
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if subject == "" {
		id := "" // a demo host's very first request minted its session on the way in and carries no cookie yet: nothing to revoke, the answer is the same 204
		if c, cerr := r.Cookie(name); cerr == nil {
			id = c.Value
		}
		_, err = tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", id)
		if err == nil {
			err = events.AuditTx(ctx, tx, actorOf(ctx), "sign out", "", "", nil)
		}
	} else {
		_, err = tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE revoked_at IS NULL AND (actor_subject = $1 OR lower(email) = lower($1))", subject)
		if err == nil {
			err = events.AuditTx(ctx, tx, actorOf(ctx), "revoke sessions", "", "", map[string]any{"subject": subject})
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if subject == "" {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}) //nolint:gosec // the session cookie's own attributes, as the callback clears its sign-in cookie
	}
	w.WriteHeader(http.StatusNoContent)
}
