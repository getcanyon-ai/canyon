package main

import (
	"errors"
	"net/http"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// tooLate is the 409's sentence once a run's deploy has begun, by the run's kind: a promote run is
// switching production, a deploy run preview.
var tooLate = map[string]string{
	"promote": "Too late to cancel: production is already switching to this version.",
	"gate":    "Too late to cancel: preview is already switching to this version.",
}

// cancelRun ends a run nobody wants any more (contracts/routes.md): a pending run at once, failed and
// interrupted; a running one is asked to end itself so before its next step, never mid-way, and the
// project stays busy until the step in flight finishes. Once its deploy has begun it is too late.
// Cancelling a promotion takes who may promote. The audit row names who started the run; a cancel
// already asked of it answers the same and writes nothing.
func (a *app) cancelRun(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: cancel %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	var kind, project string // a run's kind never changes: read before the lock, so the policy read is not under it
	err := a.pool.QueryRow(ctx, "SELECT kind, project_id FROM runs WHERE id = $1", id).Scan(&kind, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
		return
	}
	if err != nil {
		internal(err)
		return
	}
	if kind == "promote" && a.personRequired(ctx, w, act, project, "only a person can cancel a promotion") {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pkg, state string
	var ended, begun, asked bool
	var byKind, bySubject, byDisplay *string
	if err := tx.QueryRow(ctx, `SELECT project_id, package_id, state, ended_at IS NOT NULL, deploy_started_at IS NOT NULL, cancel_requested_at IS NOT NULL,
		started_by_kind, started_by_subject, started_by_display FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&project, &pkg, &state, &ended, &begun, &asked, &byKind, &bySubject, &byDisplay); err != nil {
		internal(err)
		return
	}
	switch {
	case ended:
		writeErr(w, http.StatusConflict, "ended", "this run has ended", nil)
		return
	case begun:
		writeErr(w, http.StatusConflict, "too_late", tooLate[kind], nil)
		return
	case asked:
		if err := runs.Dequeue(a.durable, id); err != nil { // asked again: a task queued since, or a dequeue that failed
			a.log.Printf("canyon-server: cancel %s: taking its tasks out of the queue: %v", id, err)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"run": runs.Run{ID: id, State: state}})
		return
	case state == "pending":
		if err := runs.EndInterrupted(ctx, tx, id, project); err != nil {
			internal(err)
			return
		}
		state = "failed"
	default:
		if _, err := tx.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", id); err != nil {
			internal(err)
			return
		}
	}
	var startedBy *actor // a run from before the column has no actor
	if byKind != nil {
		startedBy = &actor{Kind: *byKind, Subject: *bySubject, Display: *byDisplay}
	}
	if err := events.AuditTx(ctx, tx, act, action(r), project, id, map[string]any{"package": pkg, "kind": kind, "started_by": startedBy}); err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	if state == "failed" {
		runs.Cancel(a.durable, id)
		writeJSON(w, http.StatusOK, map[string]any{"run": runs.Run{ID: id, State: state}})
		return
	}
	if err := runs.Dequeue(a.durable, id); err != nil { // its waiting scans and build leave the queue, so the cancel lands now (contracts/finding.md)
		a.log.Printf("canyon-server: cancel %s: taking its tasks out of the queue: %v", id, err)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": runs.Run{ID: id, State: state}})
}
