package main

import (
	"net/http"

	"github.com/getcanyon-ai/canyon/events"
)

func routesSessions(api *http.ServeMux, a *app) {
	api.HandleFunc("DELETE /sessions", a.deleteSessions)
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
