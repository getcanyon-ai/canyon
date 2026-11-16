package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// routesPromote registers the receipt at rest, the promotion and the approval (contracts/routes.md
// "Deploy & promote" and "Gate"). The read writes no audit row; the other two write one each.
func routesPromote(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /projects/{id}/status", a.reviewOnly(a.status)) // a responder's too: the receipt beside the findings they approve
	api.HandleFunc("POST /projects/{id}/promotions", a.teamOnly(a.promote))
	// only a person approves, and a blocking finding only an admin or a responder who did not start the run
	api.HandleFunc("POST /gate-runs/{run}/findings/{fingerprint}/approve", a.runReviewOnly(personOnly("only a person can approve a finding", a.findingInDashboard(a.approve))))
	api.HandleFunc("POST /gate-runs/{run}/findings/approve", a.runReviewOnly(personOnly("only a person can approve a finding", a.findingInDashboard(a.approveFindings))))
}

// status composes the project read — its envs and owners — with the newest run, and the receipt
// evaluated for promote_run and the actor reading: the newest run of the package preview runs, the one
// the promote button would send, so a later review of another package never stands in for it.
func (a *app) status(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	read, err := a.readReceipt(ctx, id, r.URL.Query().Get("run"))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	case errors.Is(err, errNoSuchRun):
		writeErr(w, http.StatusNotFound, "not_found", "no such run of this project", nil)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	p, in := read.project, read.in
	var earlier []runs.Earlier // production's earlier versions, on this read alone: after an undeploy too
	if earlier, err = runs.ReadEarlier(ctx, a.pool, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	rec := evaluate(in)
	needs, err := a.statusNeeds(ctx, read) // what needs a person (contracts/routes.md, *What needs a person*): a responder's, the hold alone
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out := map[string]any{"project": id, "envs": p.Envs, "last_run": runID(read.newest), "promote_run": runID(in.run), "receipt": rec, "earlier": earlier, "needs": needs}
	if !p.team && roleOf(ctx) == "responder" { // a responder with no grant reads what approving needs and nothing more (contracts/routes.md, the status row; #2204)
		envs := map[string]any{}
		if prod := viewerEnvOf(p.Envs["prod"]); prod != nil {
			envs["prod"] = prod
		}
		out = map[string]any{"project": id, "envs": envs, "last_run": out["last_run"], "promote_run": out["promote_run"], "receipt": rec.forResponder(), "needs": needs}
	}
	if p.Hold != nil {
		out["hold"] = p.Hold
	}
	writeJSON(w, http.StatusOK, out)
}

// sayMissing sets production's missing on the status read (contracts/routes.md, the status row; #2786): while Canyon runs
// production, the names, sorted, of the runtime values a person set for preview that production has none of — the
// rule a promotion's "set for preview but not production" says it by — [] when none is. Names, never values.
func sayMissing(ctx context.Context, q querier, project string, envs map[string]any) error {
	prod, ok := envs["prod"].(map[string]any)
	if !ok || prod["run_by"] != "canyon" || prod["url"] == nil {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT p.name FROM secrets p WHERE p.project_id = $1 AND p.env = 'preview' AND p.scope = 'runtime'
		AND coalesce(p.origin, 'user') = 'user'
		AND NOT EXISTS (SELECT 1 FROM secrets r WHERE r.project_id = $1 AND r.env = 'prod' AND r.scope = 'runtime' AND r.name = p.name) ORDER BY 1`, project)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	prod["missing"] = append([]string{}, names...)
	return nil
}

// errNoSuchRun is readReceipt's answer to a run named that is not the project's.
var errNoSuchRun = errors.New("no such run of this project")

// receiptRead is what the status read evaluates its receipt from: the project as it reads it, the receipt's input,
// and the project's newest run.
type receiptRead struct {
	project project
	in      receiptInput
	newest  *runFacts
}

// readReceipt reads the project and the receipt's input as the status read evaluates it (contracts/routes.md, the
// status row): for named, or with none for the run a promotion of preview names, its package's newest. A project
// that is not live is pgx.ErrNoRows, a run named that is not the project's errNoSuchRun.
func (a *app) readReceipt(ctx context.Context, id, named string) (receiptRead, error) {
	var out receiptRead
	p, err := project1(ctx, a.pool, id)
	if err != nil {
		return out, err
	}
	snap, err := a.projectPolicy(ctx, a.pool, id) // the app's effective snapshot: its additions are its receipt's too
	if err != nil {
		return out, err
	}
	a.sayWhy(ctx, p.Envs, a.sayRunning(ctx, map[string]map[string]any{id: p.Envs}, true, true))
	if err := sayAnswered(ctx, a.pool, id, p.Envs); err != nil {
		return out, err
	}
	if err := sayMissing(ctx, a.pool, id, p.Envs); err != nil { // a responder's trimmed read leaves it out, as it leaves answered out
		return out, err
	}
	if p.team { // the patch state, and on this read alone wait and could_not_rescan (contracts/routes.md, envs.<env>.patch)
		facts, err := a.patchFactsOne(ctx, p.Envs, id)
		if err == nil {
			err = a.sayPatchNow(ctx, id, p.Envs, facts)
		}
		if err != nil {
			return out, err
		}
	}
	in := receiptInput{org: a.orgNow(ctx).name, rules: promoteRules(snap), approved: snap.ApprovedBuildpacks, owners: p.Owners, actor: actorOf(ctx), dashboard: fromDashboard(ctx), signInOff: a.auth != authOIDC}
	if in.approvalOn, err = a.prodApproval(ctx, a.pool); err == nil {
		in.held, err = runs.ReadHold(ctx, a.pool, id)
	}
	if err != nil {
		return out, err
	}
	var image string                                                            // the image the receipt is for: preview's, or the one a rollback review ships
	if env, ok := p.Envs["preview"].(map[string]any); ok && env["url"] != nil { // the env row names its own version (contracts/routes.md); one waiting on its first release runs nothing
		pkg, _ := env["package"].(string)
		label, _ := env["label"].(string)
		in.preview = &pkgRef{id: pkg, label: label}
		in.previewState, _ = env["state"].(string) // none where the org runs it
		in.restarts = restartsOf(env)
		if err := a.pool.QueryRow(ctx, "SELECT d.buildpacks, d.image FROM "+runs.ShownDeploys+" d WHERE d.project_id = $1 AND d.env = 'preview'", id).Scan(&in.ran, &image); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	newest, unapproved, err := receiptFacts(ctx, a.pool, id, "", "", 0)
	in.run, in.unapproved = newest, unapproved
	switch {
	case err != nil:
	case named != "": // the receipt for that run, as a promotion naming it reads it (contracts/routes.md, the status row)
		if in.run, in.unapproved, err = receiptFacts(ctx, a.pool, id, named, "", 0); err == nil && in.run == nil {
			return out, errNoSuchRun
		}
	case in.preview != nil: // the run a promotion of preview names: its package's newest
		in.run, in.unapproved, err = receiptFacts(ctx, a.pool, id, "", in.preview.id, 0)
	}
	if err == nil && in.run != nil && in.run.rollback { // a rollback review's stack is the image it ships
		image = in.run.ships
		in.ran, err = runs.ImageBuildpacks(ctx, a.pool, id, image)
	}
	if err == nil && (in.preview != nil || in.run != nil && in.run.rollback) { // with nothing in preview the receipt reads no rule
		in.placed, err = a.placedRules(ctx, a.pool, id, in.run, snap, 0)
		if err == nil {
			err = readBuiltApps(ctx, a.pool, id, runIDOf(in.run), image, in.placed, 0)
		}
	}
	if err == nil && (in.preview != nil || in.run != nil && in.run.rollback) {
		in.platform, err = platformOf(ctx, a.pool, snap, id, image)
	}
	if err == nil && (in.preview != nil || in.run != nil && in.run.rollback) {
		in.egress, err = a.egressOf(ctx, a.pool, snap, id, in.run != nil && in.run.rollback)
	}
	out.project, out.in, out.newest = p, in, newest
	return out, err
}

// runID is a run's id, or null for none.
func runID(r *runFacts) *string {
	if r == nil {
		return nil
	}
	return &r.id
}

// promote ships the image preview runs to prod when the receipt for the run named and the actor
// asking blocks nothing — the same digest, never a rebuild (contracts/executor.md invariant 4).
func (a *app) promote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GateRun       string `json:"gate_run"`
		PromoteAnyway bool   `json:"promote_anyway"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.promoteRun(w, r, r.PathValue("id"), body.GateRun, nil, body.PromoteAnyway)
}

// errNoProject is promotionFacts' answer for an unknown or deleted project: the 404, where any other
// row a read misses is the server's failure.
var errNoProject = errors.New("no such project")

// promotionFacts is the receipt's input for promoting the run named as the request's actor, read on q,
// with the preview row it read: the package the receipt checks and the image a promotion ships. seen is
// the project as read before the lock, its preview's state the executor's. A nil run is no such run of
// the app; a nil preview, nothing deployed there.
func (a *app) promotionFacts(ctx context.Context, q querier, id, gateRun, orgName string, seen project) (in receiptInput, pkg pkgRef, ship runs.Promotion, err error) {
	p, err := project1(ctx, q, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, pkg, ship, errNoProject
	}
	if err != nil {
		return in, pkg, ship, err
	}
	snap, err := a.projectPolicy(ctx, q, id)
	if err != nil {
		return in, pkg, ship, err
	}
	in = receiptInput{org: orgName, rules: promoteRules(snap), approved: snap.ApprovedBuildpacks, owners: p.Owners, actor: actorOf(ctx), dashboard: fromDashboard(ctx), fleet: fleetDecides(ctx), signInOff: a.auth != authOIDC}
	if in.approvalOn, err = a.prodApproval(ctx, q); err != nil {
		return in, pkg, ship, err
	}
	if in.held, err = runs.ReadHold(ctx, q, id); err != nil {
		return in, pkg, ship, err
	}
	if gateRun != "" { // the loader reads "" as the newest run: a promotion names its run, never "latest"
		if in.run, in.unapproved, err = receiptFacts(ctx, q, id, gateRun, "", 0); err != nil {
			return in, pkg, ship, err
		}
	}
	if in.run != nil && in.run.rollback { // a rollback review ships the image it recorded, and preview is not read (contracts/routes.md, the promotions row)
		pkg, ship = in.run.pkg, runs.Promotion{Image: in.run.ships, GateRun: in.run.id, DeployedBy: in.run.startedBy, Rollback: true, Reason: in.run.reason}
		if in.ran, err = runs.ImageBuildpacks(ctx, q, id, ship.Image); err != nil {
			return in, pkg, ship, err
		}
		if in.platform, err = platformOf(ctx, q, snap, id, ship.Image); err != nil {
			return in, pkg, ship, err
		}
		if in.egress, err = a.egressOf(ctx, q, snap, id, true); err != nil {
			return in, pkg, ship, err
		}
		if in.placed, err = a.placedRules(ctx, q, id, in.run, snap, 0); err != nil {
			return in, pkg, ship, err
		}
		err = readBuiltApps(ctx, q, id, runIDOf(in.run), ship.Image, in.placed, 0)
		return in, pkg, ship, err
	}
	by := &ship.DeployedBy // the zero actor on a row written before the columns
	err = q.QueryRow(ctx, `SELECT d.package_id, COALESCE(p.label, ''), d.image, d.gate_run, COALESCE(d.deployed_by_kind, ''), COALESCE(d.deployed_by_subject, ''), COALESCE(d.deployed_by_display, ''), d.buildpacks, d.answered
		FROM `+runs.ShownDeploys+` d LEFT JOIN packages p ON p.project_id = d.project_id AND p.id = d.package_id
		WHERE d.project_id = $1 AND d.env = 'preview'`, id).
		Scan(&pkg.id, &pkg.label, &ship.Image, &ship.GateRun, &by.Kind, &by.Subject, &by.Display, &in.ran, &in.answered)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, pkg, ship, nil
	}
	if err != nil {
		return in, pkg, ship, err
	}
	in.preview = &pkg
	asked, _ := seen.Envs["preview"].(map[string]any) // the preview the executor was asked about
	in.previewState, _ = asked["state"].(string)
	in.restarts = restartsOf(asked)
	if asked["package"] != pkg.id || asked["gate_run"] != ship.GateRun { // preview changed since: its state is not known, so it fails closed
		in.previewState = "unknown"
	}
	if in.platform, err = platformOf(ctx, q, snap, id, ship.Image); err != nil {
		return in, pkg, ship, err
	}
	if in.egress, err = a.egressOf(ctx, q, snap, id, false); err != nil {
		return in, pkg, ship, err
	}
	if in.placed, err = a.placedRules(ctx, q, id, in.run, snap, 0); err != nil {
		return in, pkg, ship, err
	}
	err = readBuiltApps(ctx, q, id, runIDOf(in.run), ship.Image, in.placed, 0)
	return in, pkg, ship, err
}

// promoteRun is the promotions route for the run named, asked by the request's actor. It starts a
// promote run, reading all it checks under the lock the run starts in; the run records the promotion
// when it goes live. A refusal writes nothing. asked is the production approval a person is approving
// in the dashboard (contracts/routes.md, Production approvals), nil for the Promote button and the CLI;
// while the org has a person approve production, the CLI's ask files one instead of a run. anyway is the
// request's promote_anyway: a browser session's override of what preview answered, for the version it runs.
func (a *app) promoteRun(w http.ResponseWriter, r *http.Request, id, gateRun string, asked *approval, anyway bool) {
	ctx, act := r.Context(), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: promote %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	seen, err := project1(ctx, a.pool, id) // preview's running state, asked of the executor once and before the lock (contracts/routes.md)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(err)
		return
	}
	// counted, never asked why (only the status read asks it): a preview that keeps restarting is refused with its count
	a.sayRunning(ctx, map[string]map[string]any{id: seen.Envs}, false, true)
	orgName := a.orgNow(ctx).name // before the lock: it reads on a connection of its own, which a transaction holding the ledger lock must never ask for (#1693)
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	held, err := runs.ReadHold(ctx, tx, id) // under the lock the hold is placed under, before anything of the promotion is read (contracts/routes.md, The hold)
	if err != nil {
		internal(err)
		return
	}
	if held != nil {
		a.refuseHeld(ctx, w, tx, *held)
		return
	}
	if asked != nil { // under the lock: of a request, an approve and a deny exactly one wins
		var pending bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM promotion_approvals WHERE code = $1 AND state = 'pending' AND expires_at > clock_timestamp())", asked.code).Scan(&pending); err != nil {
			internal(err)
			return
		}
		if !pending {
			writeErr(w, http.StatusNotFound, "not_found", noApproval, nil)
			return
		}
		if asked.patch != nil { // a patch never ships over a person's rollback, nor past an admin's stop (contracts/routes.md, Production approvals)
			moved, err := a.patchMoved(ctx, tx, *asked)
			if err != nil {
				internal(err)
				return
			}
			if moved != "" {
				patchOutdated(w, moved)
				return
			}
		}
	}
	in, pkg, ship, err := a.promotionFacts(ctx, tx, id, gateRun, orgName, seen)
	if errors.Is(err, errNoProject) {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if err != nil {
		internal(err)
		return
	}
	var fromPR bool // a pull request's version is never promoted, whatever the run named, a pull request's own among them (#1947)
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM packages WHERE project_id = $1 AND id = $2 AND source_kind = 'repo-pr')", id, pkg.id).Scan(&fromPR); err != nil {
		internal(err)
		return
	}
	if fromPR {
		writeErr(w, http.StatusConflict, "pull_request", "Preview runs a pull request's version. Merge it, then deploy main.", nil)
		return
	}
	if in.run == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such run of this project", nil)
		return
	}
	if asked != nil && !ship.Rollback && (in.preview == nil || pkg.id != asked.pkg || ship.Image != asked.image) { // a person approves the version the page showed
		writeErr(w, http.StatusConflict, "outdated", "preview runs another version now: ask again from the CLI", nil)
		return
	}
	if ship.Rollback && (in.run.state == "pending" || in.run.state == "running") { // still reviewing: nothing is recorded yet, and the review is the project's active run
		writeErr(w, http.StatusConflict, "run_in_progress", "a run of this project is running", map[string]any{"run": runs.Run{ID: in.run.id, State: in.run.state}})
		return
	}
	if ship.Rollback && ship.Image == "" { // whatever the rules' states: there is nothing to start (contracts/routes.md, the promotions row)
		writeErr(w, http.StatusConflict, "nothing_to_ship", in.run.pkg.version()+"'s review recorded no image to put back; review it again", nil)
		return
	}
	if ship.Rollback { // production still runs what the review would replace, and an approval names the image it ships
		current, err := prodRuns(ctx, tx, id, in.run.replaces)
		if err != nil {
			internal(err)
			return
		}
		switch {
		case asked != nil && (!current || ship.Image != asked.image):
			writeErr(w, http.StatusConflict, "outdated", "production runs another version now: ask again from the CLI", nil)
			return
		case !current:
			writeErr(w, http.StatusConflict, "outdated", "production runs another version now: review it again", nil)
			return
		}
	}
	rec := evaluate(in)
	// The CLI's ask while the org has a person approve production: the one rule left is the person. Deciding an
	// approval — the fleet approval's among them — never files one.
	file := asked == nil && !in.fleet && in.approvalOn && !in.dashboard && slices.Equal(rec.BlockedBy, []string{"prod.human-approval"})
	if rec.Waiting != "" { // nothing runs in preview: nothing to promote, and nothing changes
		writeErr(w, http.StatusConflict, "nothing_in_preview", rec.Waiting, map[string]any{"receipt": rec})
		return
	}
	if len(rec.BlockedBy) > 0 && !file {
		// approval_required is the one refusal a person clears by clicking promote, so it is only
		// the human-approval rule blocking alone. Any other rule an admin set blocking — owner.required
		// among them, since a policy edit can turn any rule blocking — is gate_not_clean, and the
		// receipt's blocked_by names which. (contracts/routes.md, the promotions row)
		code, message := "gate_not_clean", "a rule the org set blocking refuses this promotion"
		switch {
		case len(rec.BlockedBy) == 1 && rec.BlockedBy[0] == "prod.human-approval":
			code, message = "approval_required", "a person must click promote"
		case slices.Contains(rec.BlockedBy, "prod.clean-gate"):
			message = "the gate run is not clean"
		}
		writeErr(w, http.StatusConflict, code, message, map[string]any{"receipt": rec})
		return
	}
	var deleting bool // under the ledger lock the delete marks the project under: a promotion never starts after the mark
	if err := tx.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1", id).Scan(&deleting); err != nil {
		internal(err)
		return
	}
	if deleting {
		writeErr(w, http.StatusConflict, "deleting", "this project is being deleted", nil)
		return
	}
	if file {
		a.fileApproval(w, r, tx, id, in.run.id, pkg.id, ship.Image)
		return
	}
	// What preview answered is refused outside the receipt, and only a person in the browser overrides it,
	// for the version preview runs; with sign-in off no session tells the browser from the CLI.
	var despite *answered.Answer
	if in.answered != nil && in.answered.Flagged() {
		if !anyway || (!in.dashboard && a.auth == authOIDC) || in.run.pkg.id != pkg.id {
			writeErr(w, http.StatusConflict, "preview_answered", in.answered.Refused(pkg.version()), map[string]any{"version": pkg.version(), "answered": in.answered})
			return
		}
		despite = in.answered
	}
	if in.run != nil { // the review the receipt just accepted it under, cleared again before production starts
		ship.ReceiptRun = in.run.id
	}
	run, superseded, err := runs.StartTx(ctx, tx, a.durable, runs.StartInput{Project: id, Package: pkg.id, Actor: act, Promote: &ship})
	if errors.Is(err, runs.ErrUndeploying) {
		no := undeploying(id)
		writeErr(w, no.status, no.code, no.message, nil)
		return
	}
	if errors.Is(err, runs.ErrRunInProgress) {
		_ = tx.Rollback(ctx) // the index's refusal aborted it
		var busy runs.Run
		if err := a.pool.QueryRow(ctx, "SELECT id, state FROM runs WHERE project_id = $1 AND state IN ('pending', 'running')", id).Scan(&busy.ID, &busy.State); err != nil {
			internal(err)
			return
		}
		writeErr(w, http.StatusConflict, "run_in_progress", "a run of this project is running", map[string]any{"run": busy})
		return
	}
	if err != nil {
		internal(err)
		return
	}
	if asked != nil {
		err = asked.approved(ctx, tx, act, run.ID, despite)
	} else {
		detail := map[string]any{"package": pkg.id, "gate_run": in.run.id}
		if despite != nil {
			detail["answered"] = despite
		}
		err = events.AuditTx(ctx, tx, act, action(r), id, run.ID, detail)
	}
	if errors.Is(err, errNotPending) { // rolled back with the run it would have started
		writeErr(w, http.StatusNotFound, "not_found", noApproval, nil)
		return
	}
	if err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	runs.Cancel(a.durable, superseded)
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// approvableSQL finds the fingerprint among the findings of the run's verdict: the run names the
// project, the finding its rule, whether it blocks before preview — a verdict that does not say blocks,
// fail closed — and its severity. No verdict — a run still reviewing, or one that failed before its
// verdict — finds nothing; a deploy that failed its build has a verdict, and its findings are approvable.
const approvableSQL = `SELECT r.project_id, f->>'ruleId', COALESCE((f->'properties'->'canyon'->>'blocks')::boolean, true),
  COALESCE(f->'properties'->'canyon'->>'severity', '')
FROM runs r JOIN timeline_rows t ON t.run_id = r.id AND t.kind = 'verdict',
  jsonb_path_query(t.data, '$.findings.runs[*].results[*]') f
WHERE r.id = $1 AND f->'partialFingerprints'->>'canyon/v1' = $2 LIMIT 1`

// runAuthor is a run's author as the approver rule reads it, for the caller: by, the actor whose request
// started it (contracts/routes.md, the runs list's started_by), or of a patch run the author of the run whose
// image it rebases (Patch runs), nil for a run that recorded none; patch, whether it is a patch run; linked,
// whether the caller linked the GitHub account whose push started it, or the patch's (#2228); and unlinked,
// whether the run is a push's, the org asks its approvers to link GitHub, and the caller linked no account
// (#2342).
type runAuthor struct {
	by       *actor
	patch    bool
	linked   bool
	unlinked bool
}

// authorOf is run's author, as the caller reads it.
func authorOf(ctx context.Context, q rower, run string) (runAuthor, error) {
	var kind *string
	var subject, display string
	var out runAuthor
	github := runs.Author("runs", "github")
	err := q.QueryRow(ctx, `SELECT `+runs.Author("runs", "kind")+`, COALESCE(`+runs.Author("runs", "subject")+`, ''), COALESCE(`+runs.Author("runs", "display")+`, ''), kind = 'patch',
		EXISTS (SELECT 1 FROM github_links l WHERE l.github_id = `+github+` AND l.subject = $2),
		`+github+` IS NOT NULL AND (SELECT push_approvers_linked FROM org_preferences) AND NOT EXISTS (SELECT 1 FROM github_links l WHERE l.subject = $2)
		FROM runs WHERE id = $1`, run, actorOf(ctx).Subject).Scan(&kind, &subject, &display, &out.patch, &out.linked, &out.unlinked)
	if kind != nil {
		out.by = &actor{Kind: *kind, Subject: subject, Display: display}
	}
	return out, err
}

// approverRefusal is why the caller may not approve a blocking finding of a run author authored, by who
// they are — "" when they may (design/PERMISSIONS.md; #2204): an admin or a responder, never the run's
// author, the person whose session started it or who linked the GitHub account whose push did (#2228), nor,
// through a patch run Canyon started, the author of the run whose image it rebases; and of a push's run, while
// the org asks it, only one who linked a GitHub account (#2342). A run that recorded no author is approved by
// nobody, fail closed, a patch run whose image's run recorded none among them. With sign-in off every caller is
// the anonymous admin, and nothing is refused.
func (a *app) approverRefusal(ctx context.Context, author runAuthor) (code, message string) {
	if a.auth != authOIDC {
		return "", ""
	}
	if role := roleOf(ctx); role != "admin" && role != "responder" {
		return "approver_required", "only an admin or a security responder can approve a blocking finding: ask " + a.adminToAsk(ctx)
	}
	authored := author.by != nil && (author.linked || author.by.Kind == "person" && author.by.Subject == actorOf(ctx).Subject)
	switch {
	case author.by == nil && author.patch, author.by != nil && author.by.Kind == "canyon": // the run whose image it rebases recorded no author (contracts/routes.md, Patch runs), or an author no person is: fail closed
		return "author_cannot_approve", "Canyon started this review of its fix and didn't record who wrote the code it fixes, so nobody approves its blocking findings: deploy it again"
	case author.by == nil:
		return "author_cannot_approve", "Canyon didn't record who started this review, so nobody approves its blocking findings: deploy it again"
	case authored && author.patch:
		return "author_cannot_approve", "you shipped the code Canyon's fix is for, so you can't approve its blocking findings: ask another admin or a security responder"
	case authored:
		return "author_cannot_approve", "you started this review, so you can't approve its blocking findings: ask another admin or a security responder"
	case author.unlinked:
		return "github_link_required", a.orgNow(ctx).name + " asks whoever approves a push's blocking findings to link their GitHub account first: link it, then approve"
	}
	return "", ""
}

// approvesBlocking answers 403 to a caller who may not approve a blocking finding of a run author started:
// who they are, then the session, a browser's alone while sign-in is on — the agent working under a
// person's CLI session never approves its own way past the gate. One denied row; true means they may.
func (a *app) approvesBlocking(w http.ResponseWriter, r *http.Request, project string, author runAuthor) bool {
	ctx := r.Context()
	code, message := a.approverRefusal(ctx, author)
	if code == "" && !inBrowser(a, ctx) {
		code, message = "dashboard_required", "approve it in Canyon's dashboard"
	}
	if code == "" {
		return true
	}
	writeErr(w, http.StatusForbidden, code, message, nil)
	deny(ctx, code, project)
	return false
}

// inBrowser is whether the request comes from a browser session, which every request is with sign-in off:
// no session tells them apart there.
func inBrowser(a *app, ctx context.Context) bool { return a.auth != authOIDC || fromDashboard(ctx) }

// underApproverRule is whether an approval the caller files of a run author started is filed under the
// approver rule (design/PERMISSIONS.md; #2204): who they are, and a browser. Only such an approval covers a
// blocking finding, so one filed of an advisory finding by anyone else covers it while it stays advisory alone.
func (a *app) underApproverRule(ctx context.Context, author runAuthor) bool {
	code, _ := a.approverRefusal(ctx, author)
	return code == "" && inBrowser(a, ctx)
}

// voidApprovals, as the server starts, voids the approvals filed with sign-in off once sign-in is on — the
// anonymous person approved them, whom no session is under it (runs.CoversBlocking) — and answers how many
// unexpired approvals cover no blocking finding: those, and every one filed before the approver rule
// (contracts/routes.md, the approve rows; #2204), which an admin or a responder approves again.
func voidApprovals(ctx context.Context, q *pgxpool.Pool, signIn bool) (int, error) {
	if signIn {
		if _, err := q.Exec(ctx, "UPDATE approvals SET approves_blocking = false WHERE approves_blocking AND approved_by_kind = 'person' AND approved_by_subject = 'anonymous'"); err != nil {
			return 0, err
		}
	}
	var n int
	err := q.QueryRow(ctx, "SELECT count(*) FROM approvals WHERE NOT approves_blocking AND expires_at > now() AND approved_by_kind <> 'canyon'").Scan(&n)
	return n, err
}

// productionOf is the rules the app's effective snapshot places blocking before production now: a finding
// blocks when its verdict says it blocks before preview, or it blocks under one of them.
func (a *app) productionOf(ctx context.Context, project string) ([]string, error) {
	snap, err := a.projectPolicy(ctx, a.pool, project)
	return productionRules(snap), err
}

// approveSQL records an override of one finding under (project, rule, fingerprint), replacing an
// earlier one, as the single approve and the dependency's approve both write it, with whether it was filed
// under the approver rule (underApproverRule): only then does it cover a blocking finding.
const approveSQL = `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (project_id, rule, fingerprint) DO UPDATE SET reason = EXCLUDED.reason, expires_at = EXCLUDED.expires_at, approved_at = now(),
		approved_by_kind = EXCLUDED.approved_by_kind, approved_by_subject = EXCLUDED.approved_by_subject, approved_by_display = EXCLUDED.approved_by_display, shown_digest = NULL,
		approves_blocking = EXCLUDED.approves_blocking
	WHERE approvals.approves_blocking <= EXCLUDED.approves_blocking
	RETURNING true`

// file writes one approval as approveSQL does and answers it as it stands, and whether it was written: one
// filed outside the approver rule never replaces one filed under it (contracts/routes.md, the approve rows),
// which stands, and is answered, in its place.
func file(ctx context.Context, tx pgx.Tx, project, rule, fingerprint, reason, expires string, act actor, under bool) (map[string]any, bool, error) {
	approval := map[string]any{"rule": rule, "fingerprint": fingerprint, "reason": reason, "expires_at": expires, "approved_by": act}
	var wrote bool
	err := tx.QueryRow(ctx, approveSQL, project, rule, fingerprint, reason, expires, act.Kind, act.Subject, act.Display, under).Scan(&wrote)
	if !errors.Is(err, pgx.ErrNoRows) {
		return approval, wrote, err
	}
	var by actor
	err = tx.QueryRow(ctx, `SELECT reason, to_char(expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), approved_by_kind, approved_by_subject, approved_by_display
		FROM approvals WHERE project_id = $1 AND rule = $2 AND fingerprint = $3`, project, rule, fingerprint).Scan(&reason, &expires, &by.Kind, &by.Subject, &by.Display)
	approval["reason"], approval["expires_at"], approval["approved_by"] = reason, expires, by
	return approval, false, err
}

// grantless is whether the caller reaches the app as a responder with no grant on it: every approval of
// theirs, an advisory finding's too, passes the approver rule's checks (design/PERMISSIONS.md).
func (a *app) grantless(ctx context.Context, project string) (bool, error) {
	if roleOf(ctx) != "responder" {
		return false, nil
	}
	level, err := levelOf(ctx, a.pool, project)
	return err == nil && level != "owner" && level != "contributor", err
}

// approvalTerms answers 400 to a reason or an expiry the approve routes refuse, and is true when both are
// as they take them: a reason of 1 to 1024 characters, and an expiry in RFC 3339, UTC at whole seconds,
// after now and at most 90 days after it — taken only as it is answered, so what comes back is what went in.
func approvalTerms(w http.ResponseWriter, reason, expires string) bool {
	now := time.Now()
	if n := utf8.RuneCountInString(reason); n == 0 || n > 1024 || strings.ContainsRune(reason, 0) { // Postgres text refuses a NUL
		writeErr(w, http.StatusBadRequest, "invalid_reason", "a reason is 1 to 1024 characters", nil)
		return false
	}
	if t, err := time.Parse(time.RFC3339, expires); err != nil || t.UTC().Format(time.RFC3339) != expires || !t.After(now) || t.After(now.Add(90*24*time.Hour)) {
		writeErr(w, http.StatusBadRequest, "invalid_expires_at", "expires_at is RFC 3339 in UTC at whole seconds, after now and at most 90 days after it", nil)
		return false
	}
	return true
}

// findingInDashboard refuses a finding's approval from any session but a browser's while production is
// approved in the dashboard (contracts/routes.md, the approve rows; #1824): the agent replaying the CLI's
// session never approves its own way past the gate. It names the run's page, where a person approves it.
func (a *app) findingInDashboard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, run := r.Context(), r.PathValue("run")
		if fromDashboard(ctx) {
			next(w, r)
			return
		}
		on, err := a.prodApproval(ctx, a.pool)
		if err != nil {
			a.log.Printf("canyon-server: production approval: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if !on {
			next(w, r)
			return
		}
		var project string
		err = a.pool.QueryRow(ctx, "SELECT project_id FROM runs WHERE id = $1", run).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
			return
		}
		if err != nil {
			a.log.Printf("canyon-server: the app of run %s: %v", run, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		writeErr(w, http.StatusForbidden, "dashboard_required", "While production is approved in Canyon's dashboard, a finding is approved there too. Fix the code and deploy again, or ask a person to approve it in the dashboard: "+
			a.public+"/#/apps/"+project+"?run="+run, nil)
		deny(ctx, "dashboard_required", project)
	}
}

// approve records an override of one finding under (project, rule, fingerprint), replacing an
// earlier one — one an earlier Canyon's AI wrote too — with one audit row every call:
// the override is never silent (D6). expires_at is taken only as it is answered — UTC at whole
// seconds — so what comes back is what went in.
func (a *app) approve(w http.ResponseWriter, r *http.Request) {
	ctx, run, fingerprint, act := r.Context(), r.PathValue("run"), r.PathValue("fingerprint"), actorOf(r.Context())
	var body struct {
		Reason    *string `json:"reason"`
		ExpiresAt *string `json:"expires_at"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()                                                                                       // the schema's approve_request carries these two and nothing else
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || body.Reason == nil || body.ExpiresAt == nil { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "invalid_body", "the body is one JSON object with reason and expires_at", nil)
		return
	}
	reason, expires := *body.Reason, *body.ExpiresAt
	if !approvalTerms(w, reason, expires) {
		return
	}
	var project, rule string
	var preview bool
	var severity finding.Severity
	err := a.pool.QueryRow(ctx, approvableSQL, run, fingerprint).Scan(&project, &rule, &preview, &severity)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such finding in this run's verdict", nil)
		return
	}
	var author runAuthor
	var production []string
	var grantless bool
	if err == nil {
		author, err = authorOf(ctx, a.pool, run)
	}
	if err == nil {
		production, err = a.productionOf(ctx, project)
	}
	if err == nil {
		grantless, err = a.grantless(ctx, project)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if (preview || blocksProduction(production, rule, severity) || grantless) && !a.approvesBlocking(w, r, project, author) {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	approval, wrote, err := file(ctx, tx, project, rule, fingerprint, reason, expires, act, a.underApproverRule(ctx, author))
	if err == nil && wrote { // one that stands in its place is no override: nothing to record
		err = events.AuditTx(ctx, tx, act, action(r), project, run, map[string]any{"rule": rule, "fingerprint": fingerprint, "reason": reason, "expires_at": expires, "author": author.by}) // the approver is the row's actor
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval": approval})
}

// verdictFindingsSQL is each of the fingerprints among the findings of the run's verdict, with its rule,
// the tool.driver.name of the log's run it is in, its problem, which names its dependency (dependencyOf),
// whether it blocks before preview, fail closed as approvableSQL reads it, and its severity.
const verdictFindingsSQL = `SELECT r.project_id, f->'partialFingerprints'->>'canyon/v1', f->>'ruleId',
  COALESCE(l.run->'tool'->'driver'->>'name', ''), COALESCE(f->'properties'->'canyon'->>'problem', ''),
  COALESCE((f->'properties'->'canyon'->>'blocks')::boolean, true), COALESCE(f->'properties'->'canyon'->>'severity', '')
FROM runs r JOIN timeline_rows t ON t.run_id = r.id AND t.kind = 'verdict',
  jsonb_array_elements(t.data->'findings'->'runs') l(run), jsonb_array_elements(l.run->'results') f
WHERE r.id = $1 AND f->'partialFingerprints'->>'canyon/v1' = ANY($2)`

// fingerprintShape is a full canyon/v1 fingerprint (contracts/finding.md).
var fingerprintShape = regexp.MustCompile(`^f_[0-9a-f]{64}$`)

// approveFindings approves one dependency's findings from one tool together: each as approve records
// it, in one transaction, and one audit row naming the dependency and listing the findings
// (contracts/routes.md, POST /gate-runs/:run/findings/approve). The tool and the dependency are read off
// the verdict, never the request; every fingerprint is found before they are compared. A fingerprint the verdict
// does not hold is found in the built app the run's receipt reads (builtAppCounted): a package only the image holds,
// deps.known-vuln's, canyon/promote-gate's to name, approved under the same rules.
func (a *app) approveFindings(w http.ResponseWriter, r *http.Request) {
	ctx, run, act := r.Context(), r.PathValue("run"), actorOf(r.Context())
	var body struct {
		Fingerprints *[]string `json:"fingerprints"`
		Reason       *string   `json:"reason"`
		ExpiresAt    *string   `json:"expires_at"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()                                                                                                                    // the schema's approve_findings_request carries these three and nothing else
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || body.Fingerprints == nil || body.Reason == nil || body.ExpiresAt == nil || //nolint:errorlint // sentinel from the decoder itself
		len(*body.Fingerprints) == 0 || len(*body.Fingerprints) > 200 || slices.ContainsFunc(*body.Fingerprints, func(fp string) bool { return !fingerprintShape.MatchString(fp) }) ||
		len(slices.Compact(slices.Sorted(slices.Values(*body.Fingerprints)))) != len(*body.Fingerprints) {
		writeErr(w, http.StatusBadRequest, "invalid_body", "the body is one JSON object with 1 to 200 distinct fingerprints, a reason and expires_at", nil)
		return
	}
	fingerprints, reason, expires := *body.Fingerprints, *body.Reason, *body.ExpiresAt
	if !approvalTerms(w, reason, expires) {
		return
	}
	type found struct {
		rule, tool, problem string
		preview             bool
		severity            finding.Severity
		built               *appPackage // the built app's package it is a fingerprint of, nil for a verdict's finding
	}
	var project string
	err := a.pool.QueryRow(ctx, "SELECT project_id FROM runs WHERE id = $1", run).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such finding in this run's verdict", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	verdict := map[string]found{}
	rows, err := a.pool.Query(ctx, verdictFindingsSQL, run, fingerprints)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	for rows.Next() {
		var fp string
		var f found
		if err := rows.Scan(&project, &fp, &f.rule, &f.tool, &f.problem, &f.preview, &f.severity); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		verdict[fp] = f
	}
	if rows.Err() != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if len(verdict) != len(fingerprints) {
		built, err := a.builtAppCounted(ctx, project, run)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		for _, fp := range fingerprints {
			if p, ok := built[fp]; ok && verdict[fp].rule == "" {
				verdict[fp] = found{rule: depsRule, tool: builtAppTool, severity: p.severity(), built: &p}
			}
		}
	}
	if len(verdict) != len(fingerprints) {
		writeErr(w, http.StatusNotFound, "not_found", "no such finding in this run's verdict", nil)
		return
	}
	dependency := func(f found) (string, string, bool) {
		if f.built != nil {
			return f.built.name, f.built.version, true
		}
		d, ok := dependencyOf(f.tool, f.problem)
		return d.Name, d.Installed, ok
	}
	first := verdict[fingerprints[0]]
	name, version, _ := dependency(first)
	for _, fp := range fingerprints {
		f := verdict[fp]
		if n, v, ok := dependency(f); !ok || f.tool != first.tool || n != name || v != version {
			writeErr(w, http.StatusBadRequest, "not_one_dependency", "these findings are not one tool's findings of one dependency", nil)
			return
		}
	}
	author, err := authorOf(ctx, a.pool, run)
	var production []string
	if err == nil {
		production, err = a.productionOf(ctx, project)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	var grantless bool
	if grantless, err = a.grantless(ctx, project); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	blocks := slices.ContainsFunc(fingerprints, func(fp string) bool {
		f := verdict[fp]
		return f.preview || blocksProduction(production, f.rule, f.severity)
	})
	if (blocks || grantless) && !a.approvesBlocking(w, r, project, author) {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	listed, approvals := make([]map[string]any, 0, len(fingerprints)), make([]map[string]any, 0, len(fingerprints))
	under := a.underApproverRule(ctx, author)
	for _, fp := range fingerprints {
		rule := verdict[fp].rule
		approval, wrote, err := file(ctx, tx, project, rule, fp, reason, expires, act, under)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if wrote {
			listed = append(listed, map[string]any{"rule": rule, "fingerprint": fp})
		}
		approvals = append(approvals, approval)
	}
	detail := map[string]any{"tool": first.tool, "dependency": map[string]any{"name": name, "version": version}, "findings": listed, "reason": reason, "expires_at": expires, "author": author.by}
	if len(listed) > 0 { // the findings it wrote: one that stands in another's place is no override
		if err := events.AuditTx(ctx, tx, act, action(r), project, run, detail); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
}

// builtAppTool names the producer of a built app's package in an approval's audit row: the promote receipt's, as
// a platform finding's (contracts/finding.md, Canyon's layers).
const builtAppTool = "canyon/promote-gate"

// builtAppCounted is each fingerprint deps.known-vuln's row lists in its built_app, counted or approved, in the
// receipt the status read answers for run, and the package it names (contracts/routes.md, the approve rows): only
// for the run that receipt is read for — the newest run of preview's package, or a rollback review that is its
// package's newest — and only what that row lists, so none where it lists none: its review half failed, the built
// app could not be checked, or the rule is not placed before production.
func (a *app) builtAppCounted(ctx context.Context, project, run string) (map[string]appPackage, error) {
	read, err := a.readReceipt(ctx, project, "")
	if err == nil && (read.in.run == nil || read.in.run.id != run) { // not the receipt's run: one only a rollback review that is its package's newest
		read, err = a.readReceipt(ctx, project, run)
		if err == nil && (!read.in.run.rollback || read.in.run.newest != run) {
			return nil, nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errNoSuchRun) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := evaluate(read.in)
	listed := map[string]bool{}
	for _, r := range rec.Rules {
		for _, p := range r.BuiltApp {
			for _, fp := range p.Fingerprints {
				listed[fp] = true
			}
		}
	}
	out := map[string]appPackage{}
	for _, p := range read.in.placed {
		if p.rule.ID != depsRule || p.app == nil {
			continue
		}
		for _, pkg := range slices.Concat(p.app.packages, p.app.approved) {
			for _, fp := range pkg.fingerprints {
				if listed[fp] {
					out[fp] = pkg
				}
			}
		}
	}
	return out, nil
}

// dependencyOf is a finding's dependency as trivy wrote it (contracts/finding.md, A dependency's name and
// version): only a sarif/trivy run's findings name one.
func dependencyOf(tool, problem string) (trivy.Advisory, bool) {
	if tool != "sarif/trivy" {
		return trivy.Advisory{}, false
	}
	return trivy.Read(problem)
}

// restartsOf is how often an environment the status read found restarting was started again since its deploy
// (contracts/routes.md, envs.<env>.restarting); 0 when the read carries no count.
func restartsOf(env map[string]any) int {
	r, _ := env["restarting"].(map[string]any)
	n, _ := r["restarts"].(int)
	return n
}
