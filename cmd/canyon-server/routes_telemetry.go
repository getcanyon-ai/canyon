package main

import (
	"net/http"

	"github.com/getcanyon-ai/canyon/events"
)

func routesTelemetry(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /telemetry", a.adminOnly(a.getTelemetry))
	api.HandleFunc("PUT /telemetry", a.adminOnly(a.putTelemetry))
}

// getTelemetry answers the next report exactly as it would be sent (contracts/routes.md, Telemetry). A read: it marks
// nothing seen, so a page on another site that has an admin's browser fetch it starts no sends.
func (a *app) getTelemetry(w http.ResponseWriter, r *http.Request) {
	s, err := a.telemetryStateOf(r.Context(), a.pool)
	if err != nil {
		a.internal(w, "telemetry", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *app) putTelemetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is {\"enabled\": true} or {\"enabled\": false}", nil)
		return
	}
	if *body.Enabled && a.refuseWidening(w, r) {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "telemetry", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, _, _, err = telemetryRow(ctx, tx); err == nil {
		// On again where no report has ever reached the collector starts the tries afresh (contracts/routes.md, Sending).
		_, err = tx.Exec(ctx, `UPDATE telemetry SET off_because = CASE WHEN $1 THEN NULL ELSE 'admin' END,
			claimed_at = CASE WHEN $1 AND off_because IS NOT NULL AND sent_at IS NULL THEN NULL ELSE claimed_at END,
			tried_since = CASE WHEN $1 AND off_because IS NOT NULL THEN NULL ELSE tried_since END WHERE one`, *body.Enabled)
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), action(r), "", "", map[string]bool{"enabled": *body.Enabled})
	}
	var s telemetryState
	if err == nil {
		s, err = a.telemetryStateOf(ctx, tx)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "telemetry", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}
