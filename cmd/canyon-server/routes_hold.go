package main

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
)

// routesHold registers the hold and the lift (contracts/routes.md, The hold): a responder or an admin
// stops an app's production and holds it, and lifts the hold from the dashboard. Neither is a level's: a
// responder reaches them with no grant on the app, and an owner who is neither is refused.
func routesHold(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects/{id}/envs/prod/hold", a.hold)
	api.HandleFunc("DELETE /projects/{id}/envs/prod/hold", a.lift)
}

// errDeleting is a hold or a start again of an app being deleted: 409 deleting, as a promotion's.
var errDeleting = errors.New("the project is being deleted")

// heldBody is a hold as the receipt and the 409 carry it: who, when, why.
type heldBody struct {
	By     actor  `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason"`
}

func heldOf(h runs.Hold) *heldBody {
	return &heldBody{By: h.By, At: h.At.UTC().Format(time.RFC3339), Reason: h.Reason}
}

// refuseHeld answers 409 held for h, naming the admins, who can lift it, read on q: the caller may hold the
// ledger lock, and a second connection waited for under it would starve the pool.
func (a *app) refuseHeld(ctx context.Context, w http.ResponseWriter, q querier, h runs.Hold) {
	o, err := a.orgOf(ctx, q)
	if err != nil {
		a.log.Printf("canyon-server: the admins: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeErr(w, http.StatusConflict, "held", h.Line(), map[string]any{"hold": heldOf(h), "admins": o.admins})
}

// responderRequired answers 403 responder_required with message, naming the admins read on q, and notes
// the refusal.
func (a *app) responderRequired(ctx context.Context, w http.ResponseWriter, q querier, id, message string) {
	o, err := a.orgOf(ctx, q)
	if err != nil {
		a.log.Printf("canyon-server: the admins: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeErr(w, http.StatusForbidden, "responder_required", message, map[string]any{"admins": o.admins})
	deny(ctx, "responder_required", id)
}

// hold stops the app's production and holds it (contracts/routes.md, The hold). Who, then the actor, then
// the body, and only then the hold, so a caller refused for who they are never learns of one. On one
// connection under the undeploy lock, as an undeploy: the hold row — what production ran — the brake on the
// run writing production, and the app's pending approvals turned outdated commit together; then, with no
// stop pending on that run, the executor's /stop or the org's remove, the ledger lock never held across
// it; then the row's delete and the stop app row. A /stop that fails keeps the hold, and a retry resumes.
func (a *app) hold(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: hold %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	if !a.projectLive(ctx, w, id) {
		return
	}
	if role := roleOf(ctx); role != "responder" && role != "admin" {
		a.responderRequired(ctx, w, a.pool, id, "only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.")
		return
	}
	if act.Kind != "person" { // a person's act, whatever the policies
		writeErr(w, http.StatusForbidden, "person_required", "only a person can hold production", nil)
		deny(ctx, "person_required", id)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !plainText(body.Reason, 1024) {
		writeErr(w, http.StatusBadRequest, "invalid_reason", "reason is 1 to 1024 characters of plain text", nil)
		return
	}
	conn, err := a.pool.Acquire(ctx)
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
	var placed bool // this request wrote the hold: its stop app row is written even with nothing to stop
	braked, err := a.brake(ctx, conn.Conn(), id, "prod", act, func(tx pgx.Tx) error {
		h, err := runs.ReadHold(ctx, tx, id)
		if err != nil || h != nil {
			return err
		}
		var deleting bool // under the ledger lock the delete marks the project under, as a promotion reads it
		if err := tx.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1", id).Scan(&deleting); err != nil || deleting {
			return cmp.Or(err, errDeleting)
		}
		placed = true
		// what production runs and who deployed it, and the team now: every owner and contributor address,
		// bound or not; every subject bound to one of them, or carried by a session of one or in one of its
		// groups; and its groups
		if _, err := tx.Exec(ctx, `WITH team AS (SELECT grantee, subject FROM project_access WHERE project_id = $1 AND kind = 'email' AND level IN ('owner', 'contributor'))
			INSERT INTO prod_holds (project_id, package_id, image, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display, reason, held_by_kind, held_by_subject, held_by_display, team_emails, team_subjects, team_groups)
			SELECT $1, d.package_id, d.image, d.gate_run, d.deployed_by_kind, d.deployed_by_subject, d.deployed_by_display, $2, $3, $4, $5,
				ARRAY(SELECT grantee FROM team ORDER BY grantee),
				ARRAY(SELECT subject FROM team WHERE subject <> '' UNION SELECT actor_subject FROM sessions WHERE actor_kind = 'person' AND actor_subject <> ''
					AND (lower(email) IN (SELECT grantee FROM team) OR groups && ARRAY(SELECT grantee FROM project_access WHERE project_id = $1 AND kind = 'group' AND level IN ('owner', 'contributor'))) ORDER BY 1),
				ARRAY(SELECT grantee FROM project_access WHERE project_id = $1 AND kind = 'group' AND level IN ('owner', 'contributor') ORDER BY grantee)
			FROM (SELECT) one LEFT JOIN deploys d ON d.project_id = $1 AND d.env = 'prod'`,
			id, body.Reason, act.Kind, act.Subject, act.Display); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE promotion_approvals a SET state = 'outdated', held_by_kind = h.held_by_kind, held_by_subject = h.held_by_subject,
			held_by_display = h.held_by_display, held_at = h.held_at, held_reason = h.reason
			FROM prod_holds h WHERE h.project_id = a.project_id AND a.project_id = $1 AND a.state = 'pending'`, id) // each records the hold, which its page says
		return err
	})
	if errors.Is(err, errDeleting) {
		writeErr(w, http.StatusConflict, "deleting", "this project is being deleted", nil)
		return
	}
	if err != nil {
		internal(err)
		return
	}
	if braked.Stopping {
		writeJSON(w, http.StatusAccepted, map[string]any{"stopping": map[string]any{"run": braked.Run}})
		return
	}
	var pkg, image *string // read under the undeploy lock, outside a transaction: none is open across the executor's stop
	var runBy string
	if err := conn.QueryRow(ctx, "SELECT package_id, image, run_by FROM deploys WHERE project_id = $1 AND env = 'prod'", id).Scan(&pkg, &image, &runBy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(err)
		return
	}
	open, err := runs.OpenRelease(ctx, conn.Conn(), id, "prod")
	if err != nil {
		internal(err)
		return
	}
	handedOff := runBy == "org" || open.ID != ""
	if !placed && pkg == nil && !handedOff && braked.Run == nil { // held and stopped already: nothing to resume
		h, err := runs.ReadHold(ctx, conn, id)
		if err != nil || h == nil {
			internal(errors.Join(err, errors.New("the hold is gone")))
			return
		}
		a.refuseHeld(ctx, w, conn, *h)
		return
	}
	if runBy == "canyon" {
		if a.pipe == nil || a.pipe.Executor == nil {
			internal(errors.New("no executor configured"))
			return
		}
		// The reads leave production out while it stops, as an undeploy's own stop (runs.ShownDeploys).
		if _, err := conn.Exec(ctx, "UPDATE deploys SET stopping_since = now() WHERE project_id = $1 AND env = 'prod'", id); err != nil {
			internal(err)
			return
		}
		if err := a.pipe.Executor.Stop(ctx, id, "prod"); err != nil {
			if _, uerr := conn.Exec(context.WithoutCancel(ctx), "UPDATE deploys SET stopping_since = NULL WHERE project_id = $1 AND env = 'prod'", id); uerr != nil {
				err = errors.Join(err, uerr)
			}
			internal(err)
			return
		}
	}
	detail := map[string]any{"env": "prod", "reason": body.Reason, "cancelled": []string{}}
	if pkg != nil {
		_, digest, _ := strings.Cut(*image, "@")
		detail["package"], detail["image_digest"] = *pkg, digest
	}
	if braked.Run != nil && (braked.Ended != "" || braked.Asked) {
		detail["cancelled"] = []string{braked.Run.ID}
	}
	var release string
	err = pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
		h, err := runs.ReadHold(ctx, tx, id)
		if err != nil || h == nil {
			return errors.Join(err, errors.New("the hold is gone"))
		}
		detail["reason"] = h.Reason // a retry's reason is not the hold's: the first one's stands
		if _, err := tx.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1 AND env = 'prod' AND run_by = 'canyon'", id); err != nil {
			return err
		}
		audit := placed || runBy == "canyon"
		if handedOff {
			var sent bool
			if release, sent, err = runs.SendRemoveTx(ctx, tx, a.durable, id, "prod"); err != nil {
				return err
			}
			audit = audit || sent
		}
		if !audit {
			return nil
		}
		return events.AuditTx(ctx, tx, act, "stop app", id, "", detail)
	})
	if err != nil {
		internal(err)
		return
	}
	if handedOff {
		writeJSON(w, http.StatusAccepted, map[string]any{"release": release})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lift lifts the app's hold (contracts/routes.md, The hold): the one its caller read (held_at), by a
// responder off the app's team, or an admin, from a browser while sign-in is on. The role is read before the session, so an owner lifting from the CLI is
// responder_required. With start=true the held image and its gate run are promoted again in the same
// transaction — the one production start a responder or an admin makes — once the hold row is gone, so
// the run's start reads no hold.
func (a *app) lift(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: lift %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	if !a.projectLive(ctx, w, id) {
		return
	}
	const offTheTeam = "only a security responder who is not on this app's team, or an admin, can lift its hold."
	role := roleOf(ctx)
	if role != "responder" && role != "admin" {
		a.responderRequired(ctx, w, a.pool, id, offTheTeam)
		return
	}
	start := r.URL.Query().Get("start") == "true"
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	h, err := runs.ReadHold(ctx, tx, id) // read first, under the ledger lock the hold is written under; judged after who is asking
	if err != nil {
		internal(err)
		return
	}
	if role == "responder" { // the team never lifts its own app's hold, a responder on it included
		level, err := levelOf(ctx, tx, id)
		var grouped bool // the app has a team group now, which a session that could not read its groups may be in
		if err == nil {
			err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM project_access WHERE project_id = $1 AND kind = 'group' AND level IN ('owner', 'contributor'))", id).Scan(&grouped)
		}
		if err != nil {
			internal(err)
			return
		}
		if !liftsOffTeam(ctx, level, h, grouped) {
			a.responderRequired(ctx, w, tx, id, offTheTeam)
			return
		}
	}
	if a.auth == authOIDC && !fromDashboard(ctx) { // with sign-in off no session tells the browser from the CLI
		writeErr(w, http.StatusForbidden, "dashboard_required", "lift it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", id)
		return
	}
	if err := runs.UndeployingTx(ctx, tx, id); errors.Is(err, runs.ErrUndeploying) { // an undeploy or a hold of the app under way: the lift waits for neither
		no := undeploying(id)
		writeErr(w, no.status, no.code, no.message, nil)
		return
	} else if err != nil {
		internal(err)
		return
	}
	if h == nil {
		writeErr(w, http.StatusNotFound, "not_found", "production is not held", nil)
		return
	}
	// The lift acts on the hold its caller read, never on one placed since: a stale read lifts, or starts
	// again, nothing. The reads answer at to the second, so it is compared to the second.
	read, err := time.Parse(time.RFC3339, r.URL.Query().Get("held_at"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_held_at", "held_at is the hold's at, an RFC 3339 instant", nil)
		return
	}
	if !read.Equal(h.At.Truncate(time.Second)) {
		writeErr(w, http.StatusConflict, "hold_changed", "production's hold changed since you read it: read it again", map[string]any{"hold": heldOf(*h)})
		return
	}
	if start && h.Image == "" {
		writeErr(w, http.StatusConflict, "nothing_held", "production ran nothing when it was held: lift it without starting it", nil)
		return
	}
	var deleting bool // under the ledger lock the delete marks the project under: nothing starts on an app being deleted
	if err := tx.QueryRow(ctx, "SELECT $2 AND deleting_since IS NOT NULL FROM projects WHERE id = $1", id, start).Scan(&deleting); err != nil {
		internal(err)
		return
	}
	if deleting {
		writeErr(w, http.StatusConflict, "deleting", "this project is being deleted", nil)
		return
	}
	if _, err := tx.Exec(ctx, "DELETE FROM prod_holds WHERE project_id = $1", id); err != nil {
		internal(err)
		return
	}
	var run runs.Run
	var superseded string
	if start { // a promotion supersedes only a patch run, cancelled after the commit (contracts/routes.md, Patch runs)
		run, superseded, err = runs.StartTx(ctx, tx, a.durable, runs.StartInput{Project: id, Package: h.Package, Actor: act, Promote: &runs.Promotion{Image: h.Image, GateRun: h.GateRun, DeployedBy: h.DeployedBy, Restart: true}})
		switch {
		case errors.Is(err, runs.ErrRunInProgress):
			_ = tx.Rollback(ctx) // the index's refusal aborted it
			var busy runs.Run
			if err := a.pool.QueryRow(ctx, "SELECT id, state FROM runs WHERE project_id = $1 AND state IN ('pending', 'running')", id).Scan(&busy.ID, &busy.State); err != nil {
				internal(err)
				return
			}
			writeErr(w, http.StatusConflict, "run_in_progress", "a run of this project is running", map[string]any{"run": busy})
			return
		case err != nil:
			internal(err)
			return
		}
	}
	detail := map[string]any{"held_by": h.By, "held_at": h.At.UTC().Format(time.RFC3339), "reason": h.Reason, "started": start}
	if start { // the version it started, which the trail names (#2244)
		detail["package"] = h.Package
	}
	if err := events.AuditTx(ctx, tx, act, "lift hold", id, run.ID, detail); err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	runs.Cancel(a.durable, superseded)
	if start {
		writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// noteBinding records subject on every hold whose team record names its address or one of its groups, in
// the caller's transaction, which holds the ledger lock the hold is written under (a.mutation): anyone who
// signs in to a recorded address or in a recorded group after production was held, verified or not, or is
// bound to the address by a grant, stays on that record by subject when the grant or the group goes and
// their address changes (design/PERMISSIONS.md). Refusing more people is the safe way. An empty subject
// writes nothing.
func noteBinding(ctx context.Context, tx pgx.Tx, subject, address string, groups []string) error {
	if subject == "" {
		return nil
	}
	_, err := tx.Exec(ctx, "UPDATE prod_holds SET team_subjects = array_append(team_subjects, $1) WHERE (lower($2) = ANY(team_emails) OR team_groups && $3::text[]) AND NOT $1 = ANY(team_subjects)",
		subject, address, groups)
	return err
}

// liftsOffTeam is whether a responder lifts hold h of an app they read at level, grouped while it has a team group now:
// off its team now and on the team h recorded (onTheTeam), their groups read whole wherever a team group could hold them.
// The lift's rule, and what needs a person says of the hold (needs.go), so the two never differ.
func liftsOffTeam(ctx context.Context, level string, h *runs.Hold, grouped bool) bool {
	return !onTheTeam(level, h, addressOf(ctx), actorOf(ctx).Subject, groupsOf(ctx), groupsReadOf(ctx) || !grouped && (h == nil || len(h.TeamGroups) == 0))
}

// onTheTeam is whether the caller is on the app's team for its hold h (design/PERMISSIONS.md): an owner or
// contributor grant now (level), to them directly or through a group their session carries, or on the team
// h recorded — their subject among its subjects, their address among its addresses, one of their groups
// among its groups — or a session whose groups cannot be trusted (groupsKnown false: they were not read
// whole, an overage or a claim absent, while a team group exists now or on the record), which may be in one.
func onTheTeam(level string, h *runs.Hold, email, subject string, groups []string, groupsKnown bool) bool {
	if level == "owner" || level == "contributor" || !groupsKnown {
		return true
	}
	return h != nil && (slices.Contains(h.TeamEmails, strings.ToLower(email)) || subject != "" && slices.Contains(h.TeamSubjects, subject) ||
		slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(h.TeamGroups, g) }))
}
