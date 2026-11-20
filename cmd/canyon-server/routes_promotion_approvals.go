package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// Production approvals (design/AUTH.md §4.5, contracts/routes.md): while the org has a person approve
// production, the CLI's promotion files one, and a person on a browser session approves or denies it.
// errNotPending is an approval that stopped being pending under the lock: the 404, and the run never starts.
var errNotPending = errors.New("the approval is no longer pending")

const noApproval = "no approval has that code"

// patchProof is a patch approval's proof, exactly (contracts/routes.md, Production approvals).
const patchProof = "The app's own files are byte for byte the ones in production."

// approval is one row of promotion_approvals: what was asked, by whom, and what became of it.
type approval struct {
	code, project, gateRun, pkg, image, state, via string
	requestedBy                                    actor
	requestedAt, expiresAt                         time.Time
	decidedBy                                      *actor
	decidedAt                                      *time.Time
	run                                            *string
	rollback                                       bool      // of a rollback review's run: it ships that run's ships, and preview is not read
	replaces                                       *string   // the image production ran as that review started, nil for none
	held                                           *heldBody // the hold that outdated it, nil for none
	patch                                          *patchAsk // a patch approval's own, nil on a person's
}

// patchAsk is what a patch approval adds (contracts/routes.md, Production approvals): its platform identity, the
// image production ran when Canyon asked, what the patch fixed, and when its preview watch ended.
type patchAsk struct {
	identity, replaces string
	fixes              json.RawMessage
	watchUntil         *time.Time
	rebuild            bool   // a rebuild patch's (contracts/routes.md, A rebuild patch): its own proof
	rebuiltWith        string // what it was rebuilt with, as its line same named it; "" when its run recorded none
}

// proof is the approval read's proof (contracts/routes.md, Production approvals): a rebase's, or a rebuild's naming what
// it was rebuilt with as its run's line same did — newer build tools when the run recorded nothing, never a guess.
func (p *patchAsk) proof() string {
	if !p.rebuild {
		return patchProof
	}
	return runs.RebuildProof(cmp.Or(p.rebuiltWith, "newer build tools"))
}

// patchMoved is why a patch approval no longer stands, read on q (contracts/routes.md, Production approvals): ""
// while it does; "preview" once preview runs another image than it names; "production" once production runs
// another image than the one it replaces, or nothing — a rollback, an undeploy — or Canyon no longer patches the
// app (runs' Patching: patching off, or the app's own platform.known-vuln), or its identity is paused, so an admin's
// stop stops what is asked too. Fail closed: what cannot be read is moved.
func (a *app) patchMoved(ctx context.Context, q querier, ap approval) (string, error) {
	var preview, prod, live bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'preview' AND package_id = $2 AND image = $3),
		EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'prod' AND image = $4),
		EXISTS (SELECT 1 FROM patch_identities WHERE id = $5 AND paused_at IS NULL)`,
		ap.project, ap.pkg, ap.image, ap.patch.replaces, ap.patch.identity).Scan(&preview, &prod, &live)
	var on bool
	if err == nil {
		on, err = a.pipe.Patching(ctx, q, ap.project)
	}
	if err == nil && on && ap.patch.rebuild { // a rebuild ships only while the org's rebuilds is on (contracts/routes.md, the preferences row)
		on, err = a.pipe.RebuildsOn(ctx, q)
	}
	switch {
	case err != nil:
		return "", err
	case !preview:
		return "preview", nil
	case !prod || !live || !on:
		return "production", nil
	}
	return "", nil
}

// patchOutdated is the 409 of approving a patch approval that moved (patchMoved).
func patchOutdated(w http.ResponseWriter, moved string) {
	writeErr(w, http.StatusConflict, "outdated", moved+" runs another version now: Canyon asks again if the patch still applies", nil)
}

// prodRuns is whether production runs image, nothing when nil: a rollback review's promotion and its
// approval are outdated once production no longer runs what the review would replace (contracts/routes.md,
// the promotions row and Production approvals).
func prodRuns(ctx context.Context, q querier, project string, image *string) (bool, error) {
	var current bool
	err := q.QueryRow(ctx, "SELECT (SELECT image FROM deploys WHERE project_id = $1 AND env = 'prod') IS NOT DISTINCT FROM $2::text", project, image).Scan(&current)
	return current, err
}

func routesPromotionApprovals(api *http.ServeMux, a *app) {
	if a.auth != authOIDC { // with sign-in off nothing tells the dashboard from the CLI, so the switch is never on
		return
	}
	api.HandleFunc("GET /promotion-approvals/{code}", a.getApproval)
	api.HandleFunc("POST /promotion-approvals/{code}/approve", a.approveApproval)
	api.HandleFunc("POST /promotion-approvals/{code}/deny", a.denyApproval)
}

// fromDashboard is whether the request's session is a browser sign-in's: its audit rows' channel.
func fromDashboard(ctx context.Context) bool {
	return events.ProvenanceOf(ctx).Via == "dashboard"
}

// prodApproval is whether the org has a person approve production in the dashboard, read on q: never
// with sign-in off.
func (a *app) prodApproval(ctx context.Context, q querier) (bool, error) {
	if a.auth != authOIDC {
		return false, nil
	}
	var on bool
	err := q.QueryRow(ctx, "SELECT prod_approval FROM org_preferences").Scan(&on)
	return on, err
}

// dashboardRequired answers 403 dashboard_required to a session that is not a browser's while production
// is approved in the dashboard: what a blocking rule checks is changed where a policy is (design/AUTH.md
// §4.5). true means the response is written.
func (a *app) dashboardRequired(w http.ResponseWriter, r *http.Request) bool {
	if fromDashboard(r.Context()) {
		return false
	}
	on, err := a.prodApproval(r.Context(), a.pool)
	if err != nil {
		a.log.Printf("canyon-server: production approval: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return true
	}
	if on {
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(r.Context(), "dashboard_required", "")
	}
	return on
}

// shown is a code as a person reads it: XXXX-XXXX.
func shown(code string) string { return code[:4] + "-" + code[4:] }

// fileApproval answers the CLI's promotion with a production approval in its place, inside the
// promotion's transaction: it replaces the app's pending one, and its row is the request's.
func (a *app) fileApproval(w http.ResponseWriter, r *http.Request, tx pgx.Tx, id, gateRun, pkg, image string) {
	ctx, act := r.Context(), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: production approval for %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	var replaced string
	err := tx.QueryRow(ctx, "DELETE FROM promotion_approvals WHERE project_id = $1 AND state = 'pending' RETURNING code", id).Scan(&replaced)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(err)
		return
	}
	expires := time.Now().UTC().Truncate(time.Second).Add(runs.ApprovalLife)
	var code string
	for code == "" { // a code that collides with any approval's is drawn again
		draw, err := runs.ApprovalCode()
		if err != nil {
			internal(err)
			return
		}
		tag, err := tx.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (code) DO NOTHING`,
			draw, id, gateRun, pkg, image, act.Kind, act.Subject, act.Display, events.ProvenanceOf(ctx).Via, expires)
		if err != nil {
			internal(err)
			return
		}
		if tag.RowsAffected() == 1 {
			code = draw
		}
	}
	at := expires.Format(time.RFC3339)
	detail := map[string]any{"package": pkg, "gate_run": gateRun, "code": shown(code), "expires_at": at}
	if replaced != "" {
		detail["replaced"] = shown(replaced)
	}
	if err := events.AuditTx(ctx, tx, act, "request promotion", id, "", detail); err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"approval": map[string]any{"code": shown(code), "url": a.public + "/#/approve?code=" + shown(code), "expires_at": at}})
}

// approvalOf is the approval the request's {code} names, read as a device code is, after the app's
// team check: 404 when no approval has it, then a caller below contributor on its app is refused as
// the app's routes refuse them. ok false means the response is written.
func (a *app) approvalOf(w http.ResponseWriter, r *http.Request) (ap approval, ok bool) {
	ctx := r.Context()
	var by actor
	var decidedKind, decidedSubject, decidedDisplay, heldKind, heldSubject, heldDisplay, heldReason, identity, replaces *string
	var heldAt, watchUntil *time.Time
	var fixes json.RawMessage
	var rebuild bool
	var rebuiltWith string
	err := a.pool.QueryRow(ctx, `SELECT code, project_id, gate_run, package_id, image, state, requested_via, requested_by_kind, requested_by_subject, requested_by_display,
		requested_at, expires_at, decided_by_kind, decided_by_subject, decided_by_display, decided_at, run_id,
		COALESCE((SELECT rollback_to IS NOT NULL FROM runs WHERE id = gate_run), false), (SELECT rollback_replaces FROM runs WHERE id = gate_run),
		held_by_kind, held_by_subject, held_by_display, held_at, held_reason, patch_identity, replaces_image, fixes, (SELECT watch_until FROM runs WHERE id = gate_run AND kind = 'patch'),
		coalesce((SELECT kind = 'rebuild' FROM patch_identities WHERE id = patch_identity), false),
		coalesce((SELECT rebuilt_with FROM runs WHERE id = gate_run AND kind = 'patch'), '')
		FROM promotion_approvals WHERE code = $1`, userCodeOf(r.PathValue("code"))).
		Scan(&ap.code, &ap.project, &ap.gateRun, &ap.pkg, &ap.image, &ap.state, &ap.via, &by.Kind, &by.Subject, &by.Display,
			&ap.requestedAt, &ap.expiresAt, &decidedKind, &decidedSubject, &decidedDisplay, &ap.decidedAt, &ap.run, &ap.rollback, &ap.replaces,
			&heldKind, &heldSubject, &heldDisplay, &heldAt, &heldReason, &identity, &replaces, &fixes, &watchUntil, &rebuild, &rebuiltWith)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", noApproval, nil)
		return ap, false
	}
	if err != nil {
		a.log.Printf("canyon-server: production approval: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return ap, false
	}
	ap.requestedBy = by
	if decidedKind != nil {
		ap.decidedBy = &actor{Kind: *decidedKind, Subject: *decidedSubject, Display: *decidedDisplay}
	}
	if heldAt != nil { // the row's check keeps the five together
		ap.held = &heldBody{By: actor{Kind: *heldKind, Subject: *heldSubject, Display: *heldDisplay}, At: heldAt.UTC().Format(time.RFC3339), Reason: *heldReason}
	}
	if identity != nil { // the row's check keeps a patch's three together
		ap.patch = &patchAsk{identity: *identity, replaces: *replaces, fixes: fixes, watchUntil: watchUntil, rebuild: rebuild, rebuiltWith: rebuiltWith}
	}
	return ap, !a.teamRequired(ctx, w, a.pool, ap.project)
}

// inDashboard refuses a decision from any session but a browser's, before the code is read, so the
// approval is untouched: the agent that asked can never answer its own request. ok false means the
// response is written.
func inDashboard(w http.ResponseWriter, r *http.Request) bool {
	if fromDashboard(r.Context()) {
		return true
	}
	writeErr(w, http.StatusForbidden, "dashboard_required", "approve it in Canyon's dashboard", nil)
	deny(r.Context(), "dashboard_required", "")
	return false
}

func (a *app) getApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ap, ok := a.approvalOf(w, r)
	if !ok {
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: production approval %s: %v", ap.code, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	version, prod := pkgRef{id: ap.pkg}, pkgRef{} // what goes live, and what production runs: none, nothing
	if err := a.pool.QueryRow(ctx, `SELECT COALESCE((SELECT label FROM packages WHERE project_id = $1 AND id = $2), ''),
		COALESCE((SELECT package_id FROM deploys WHERE project_id = $1 AND env = 'prod'), ''),
		COALESCE((SELECT p.label FROM deploys d JOIN packages p ON p.project_id = d.project_id AND p.id = d.package_id WHERE d.project_id = $1 AND d.env = 'prod'), '')`,
		ap.project, ap.pkg).Scan(&version.label, &prod.id, &prod.label); err != nil {
		internal(err)
		return
	}
	var replaces *string
	if prod.id != "" {
		v := prod.version()
		replaces = &v
	}
	state := ap.state
	if state == "pending" {
		var current bool // preview still runs what it names; a rollback's, production still runs what it replaces; a patch's, both
		var err error
		switch {
		case ap.patch != nil:
			var moved string
			moved, err = a.patchMoved(ctx, a.pool, ap)
			current = moved == ""
		case ap.rollback:
			current, err = prodRuns(ctx, a.pool, ap.project, ap.replaces)
		default:
			err = a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'preview' AND package_id = $2 AND image = $3)", ap.project, ap.pkg, ap.image).Scan(&current)
		}
		if err != nil {
			internal(err)
			return
		}
		switch {
		case !ap.expiresAt.After(time.Now()):
			state = "expired"
		case !current:
			state = "outdated"
		}
	}
	out := map[string]any{"code": shown(ap.code), "app": ap.project, "version": version.version(), "replaces": replaces, "requested_by": ap.requestedBy, "requested_via": ap.via,
		"requested_at": ap.requestedAt.UTC().Format(time.RFC3339), "expires_at": ap.expiresAt.UTC().Format(time.RFC3339), "state": state,
		"approver": map[string]any{"display": actorOf(ctx).Display, "email": addressOf(ctx)}}
	if ap.decidedBy != nil {
		out["decided_by"], out["decided_at"] = ap.decidedBy, ap.decidedAt.UTC().Format(time.RFC3339)
	}
	if ap.run != nil {
		out["run"] = *ap.run
	}
	if ap.rollback { // the page asks the put-back question (contracts/routes.md)
		out["rollback"] = true
	}
	if ap.held != nil {
		out["held"] = ap.held
	}
	if p := ap.patch; p != nil { // a person's approval carries none of these
		out["kind"], out["patch_run"], out["identity"], out["replaces_image"], out["image"], out["fixes"], out["proof"] = "patch", ap.gateRun, p.identity, runs.ImageDigest(p.replaces), runs.ImageDigest(ap.image), p.fixes, p.proof()
		if p.rebuild {
			out["rebuild"] = true
		}
		if p.watchUntil != nil {
			out["watch"] = map[string]any{"until": p.watchUntil.UTC().Format(time.RFC3339), "result": "passed"}
		}
	}
	if fromDashboard(ctx) { // the page's: what a promotion this reader asked now would check. The CLI's poll never asks the executor
		seen, err := project1(ctx, a.pool, ap.project)
		if err != nil {
			internal(err)
			return
		}
		a.sayRunning(ctx, map[string]map[string]any{ap.project: seen.Envs}, false, true)
		in, pkg, ship, err := a.promotionFacts(ctx, a.pool, ap.project, ap.gateRun, a.orgNow(ctx).name, seen)
		if err != nil {
			internal(err)
			return
		}
		out["receipt"] = evaluate(in)
		if in.answered != nil && pkg.id == ap.pkg && ship.Image == ap.image { // what the version asked for answered, while preview runs it
			out["answered"] = in.answered
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) approveApproval(w http.ResponseWriter, r *http.Request) {
	if !inDashboard(w, r) {
		return
	}
	ap, ok := a.approvalOf(w, r)
	if !ok {
		return
	}
	var body struct { // empty, or the page's override of what preview answered: read once the code and the team check have answered
		PromoteAnyway bool `json:"promote_anyway"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is empty or one JSON object of at most 1 MiB", nil)
		return
	}
	a.promoteRun(w, r, ap.project, ap.gateRun, &ap, body.PromoteAnyway)
}

func (a *app) denyApproval(w http.ResponseWriter, r *http.Request) {
	if !inDashboard(w, r) {
		return
	}
	ap, ok := a.approvalOf(w, r)
	if !ok {
		return
	}
	ctx, act := r.Context(), actorOf(r.Context())
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE promotion_approvals SET state = 'denied', decided_by_kind = $2, decided_by_subject = $3, decided_by_display = $4, decided_at = now()
			WHERE code = $1 AND state = 'pending' AND expires_at > clock_timestamp()`, ap.code, act.Kind, act.Subject, act.Display)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return events.AuditTx(ctx, tx, act, "deny promotion", ap.project, "", ap.detail())
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", noApproval, nil)
	case err != nil:
		a.log.Printf("canyon-server: deny production approval %s: %v", ap.code, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// approved records, in the transaction that started the promote run, that act approved the approval
// with that run, and the transaction's one audit row (contracts/routes.md, Production approvals): despite
// is what preview answered when the approver promoted anyway, nil otherwise.
func (ap approval) approved(ctx context.Context, tx pgx.Tx, act actor, run string, despite *answered.Answer) error {
	tag, err := tx.Exec(ctx, `UPDATE promotion_approvals SET state = 'approved', decided_by_kind = $2, decided_by_subject = $3, decided_by_display = $4, decided_at = now(), run_id = $5
		WHERE code = $1 AND state = 'pending' AND expires_at > clock_timestamp()`, ap.code, act.Kind, act.Subject, act.Display, run)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 { // it expired while the run was being started: nothing starts
		return errNotPending
	}
	detail := ap.detail()
	if fleetDecides(ctx) { // the fleet approval's decision names the identity it approved
		detail["identity"] = ap.patch.identity
	}
	if despite != nil {
		detail["answered"] = despite
	}
	return events.AuditTx(ctx, tx, act, "approve promotion", ap.project, run, detail)
}

// detail is a decision's audit detail: what was asked for, and by whom.
func (ap approval) detail() map[string]any {
	return map[string]any{"package": ap.pkg, "gate_run": ap.gateRun, "code": shown(ap.code), "requested_by": ap.requestedBy}
}
