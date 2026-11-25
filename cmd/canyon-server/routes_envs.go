package main

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/getcanyon-ai/canyon/events"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

func routesEnvs(api *http.ServeMux, a *app) {
	api.HandleFunc("DELETE /projects/{id}/envs/{env}", a.teamOnly(a.undeploy))
}

// undeploy takes env offline and keeps its data: the project's secret rows are not touched, so the
// next deploy starts with them (contracts/routes.md). Who may is who may put something there: any
// session for preview, as a deploy; for prod, the actor promote's prod.human-approval asks for while
// that rule is blocking. It is never refused for a run (The brake): on one connection, holding the
// project's undeploy lock to the end so no run of the project starts meanwhile (runs.StartTx), the run
// writing env is dealt with and committed first; then, with no stop pending on it, the executor's /stop
// — the ledger lock never held across it — and the row's delete with the audit row, or the remove
// release when the org's platform runs env or has a release of it open. The container stops before the
// row goes: a write that fails after the stop leaves the env recorded and stopped until a retry.
func (a *app) undeploy(w http.ResponseWriter, r *http.Request) {
	ctx, id, env, act := r.Context(), r.PathValue("id"), r.PathValue("env"), actorOf(r.Context())
	if env != "preview" && env != "prod" {
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: undeploy %s/%s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	if !a.projectLive(ctx, w, id) {
		return
	}
	if env == "prod" && a.personRequired(ctx, w, act, id, "only a person can take production down") {
		return
	}
	// An undeploy asked again asks again of its remove's pull request (#2470), before it takes its one connection:
	// the ask reads the pool.
	a.recheckRepoRemoves(ctx, id, env)
	conn, err := a.pool.Acquire(ctx) // one connection for the whole undeploy: it never holds one while it waits for another
	if err != nil {
		internal(err)
		return
	}
	defer conn.Release()
	unlock, err := runs.LockUndeploy(ctx, conn.Conn(), id)
	if err != nil {
		if !undeployBusy(w, err) {
			internal(err)
		}
		return
	}
	defer unlock()
	if env == "prod" { // nothing runs in a held production (contracts/routes.md, The hold), read under the lock the hold is placed under
		h, err := runs.ReadHold(ctx, conn, id)
		if err != nil {
			internal(err)
			return
		}
		if h != nil {
			a.refuseHeld(ctx, w, conn, *h)
			return
		}
	}
	braked, err := a.brake(ctx, conn.Conn(), id, env, act, nil)
	if err != nil {
		internal(err)
		return
	}
	if braked.Stopping {
		writeJSON(w, http.StatusAccepted, map[string]any{"stopping": map[string]any{"run": braked.Run}})
		return
	}
	var pkg *string // read under the undeploy lock, outside a transaction: none is open across the executor's stop
	var runBy string
	if err := conn.QueryRow(ctx, "SELECT package_id, run_by FROM deploys WHERE project_id = $1 AND env = $2", id, env).Scan(&pkg, &runBy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(err)
		return
	}
	open, err := runs.OpenRelease(ctx, conn.Conn(), id, env)
	if err != nil {
		internal(err)
		return
	}
	handedOff := runBy == "org" || open.ID != "" // the org's platform runs it, or has a release of it: a remove (design/HAND-OFF.md §7)
	if pkg == nil && !handedOff && braked.Run == nil {
		writeErr(w, http.StatusNotFound, "not_found", "nothing deployed in "+env, nil)
		return
	}
	detail := map[string]any{"env": env, "package": pkg, "stop": runs.StopOf(runBy == "canyon", handedOff)} // package null when only the run it ended or asked to end was writing env
	if pkg == nil && open.Event == "deploy" {
		detail["package"] = open.Package
	}
	if braked.Run != nil {
		detail["run"] = braked.Run.ID
	}
	if runBy == "canyon" { // Canyon runs the container, a takeover by the org's platform under way or not
		if a.pipe == nil || a.pipe.Executor == nil {
			internal(errors.New("no executor configured"))
			return
		}
		// The reads leave env out while it stops (runs.ShownDeploys): marked under the undeploy lock, which
		// cleared any mark a dead holder left as it was taken (runs.LockUndeploy).
		if _, err := conn.Exec(ctx, "UPDATE deploys SET stopping_since = now() WHERE project_id = $1 AND env = $2", id, env); err != nil {
			internal(err)
			return
		}
		if err := a.pipe.Executor.Stop(ctx, id, env); err != nil { // the container is <project>-<env>: the path's id
			if _, uerr := conn.Exec(context.WithoutCancel(ctx), "UPDATE deploys SET stopping_since = NULL WHERE project_id = $1 AND env = $2", id, env); uerr != nil {
				err = errors.Join(err, uerr)
			}
			internal(err)
			return
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		internal(err)
		return
	}
	if _, err := tx.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1 AND env = $2 AND run_by = 'canyon'", id, env); err != nil {
		internal(err)
		return
	}
	var release string
	audit := runBy == "canyon" || !handedOff // a row Canyon ran is gone, or only the run it ended or asked to end was writing env
	if handedOff {
		var sent bool
		if release, sent, err = runs.SendRemoveTx(ctx, tx, a.durable, id, env); err != nil {
			internal(err)
			return
		}
		audit = audit || sent // an open remove answered again changes nothing: no audit row
	}
	if audit {
		if err := events.AuditTx(ctx, tx, act, action(r), id, "", detail); err != nil {
			internal(err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	if handedOff { // the environment stays on the project read with its pending release until the org reports removed
		writeJSON(w, http.StatusAccepted, map[string]any{"release": release})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// brake is the undeploy's first step on conn, committed before anything else (runs.BrakeTx), with what
// hold writes ahead of it in the same transaction, under the ledger lock — a hold's brake writes no request
// undeploy row; nil for an undeploy;
// the run it ended has its workflow cancelled, and the run it asked to end its waiting tasks taken out of the queue.
func (a *app) brake(ctx context.Context, conn *pgx.Conn, id, env string, act actor, hold func(pgx.Tx) error) (runs.Braked, error) {
	var braked runs.Braked
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) (err error) {
		if hold != nil {
			if err := events.LockTx(ctx, tx); err != nil {
				return err
			}
			if err := hold(tx); err != nil {
				return err
			}
		}
		braked, err = runs.BrakeTx(ctx, tx, id, env, act, hold != nil)
		return err
	}); err != nil {
		return runs.Braked{}, err
	}
	runs.Cancel(a.durable, braked.Ended)
	if braked.Asked {
		if err := runs.Dequeue(a.durable, braked.Run.ID); err != nil { // its waiting scans and build leave the queue, so the ask lands now
			a.log.Printf("canyon-server: undeploy %s: taking %s's tasks out of the queue: %v", id, braked.Run.ID, err)
		}
	}
	return braked, nil
}

// personRequired answers 403 person_required, saying why, to a non-person while prod.human-approval
// blocks: who may put something in production is who may take it down, or stop it getting there.
// true means the response is written.
func (a *app) personRequired(ctx context.Context, w http.ResponseWriter, act actor, project, why string) bool {
	needed, err := a.personNeeded(ctx, act)
	if err != nil {
		a.log.Printf("canyon-server: policy: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return true
	}
	if needed {
		a.refusePerson(ctx, w, project, why)
	}
	return needed
}

// personNeeded is whether act may not take production down: not a person, while prod.human-approval blocks.
func (a *app) personNeeded(ctx context.Context, act actor) (bool, error) {
	if act.Kind == "person" {
		return false, nil
	}
	snap, err := a.effectivePolicy(ctx, a.pool)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(snap.Rules, func(r gatepolicy.Rule) bool { return r.ID == "prod.human-approval" && r.State == gatepolicy.Blocking }), nil
}

// refusePerson answers 403 person_required with why, and writes its denied row.
func (a *app) refusePerson(ctx context.Context, w http.ResponseWriter, project, why string) {
	writeErr(w, http.StatusForbidden, "person_required", why, nil)
	deny(ctx, "person_required", project)
}

// undeployBusy answers 409 busy for a wait on the project's undeploy lock that ran past its bound
// (runs.UndeployWait): another change of the app holds it — an undeploy, a hold, a delete or a recreation of
// its network. true means the response is written.
func undeployBusy(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, runs.ErrUndeployBusy) {
		return false
	}
	writeErr(w, http.StatusConflict, "busy", "another change to this app is under way: try again in a minute", nil)
	return true
}

// runInProgress answers 409 run_in_progress while a run of the project is pending or running: a
// deploy mid-way would start again what the caller is stopping. true means the response is written.
func (a *app) runInProgress(ctx context.Context, w http.ResponseWriter, q querier, id string) bool {
	var busy runs.Run
	err := q.QueryRow(ctx, "SELECT id, state FROM runs WHERE project_id = $1 AND state IN ('pending', 'running')", id).Scan(&busy.ID, &busy.State)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		a.log.Printf("canyon-server: runs of %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		writeErr(w, http.StatusConflict, "run_in_progress", "a run of this project is running", map[string]any{"run": busy})
	}
	return true
}
