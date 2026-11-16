package patching

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
)

// passedSQL is the app whose preview runs a patch whose watch ran to its end ($1, or every such app when $1 is empty): its
// app in an identity's waves passed, preview's row the patch's, and the run's passed line written.
const passedSQL = `SELECT a.project_id FROM patch_wave_apps a JOIN deploys d ON d.project_id = a.project_id AND d.env = 'preview' AND d.gate_run = a.run
	WHERE a.outcome = 'passed' AND ($1 = '' OR a.project_id = $1) AND EXISTS (SELECT 1 FROM timeline_rows t WHERE t.run_id = a.run AND t.dedupe = 'patch:passed')
	ORDER BY 1`

// FileApprovals asks production for each patch whose watch passed in preview, project's alone or every app's for ""
// (design/PATCHING.md §4.5; contracts/routes.md, Production approvals): a watch that passed asks at once, and each
// rescan tick asks again while the plan stands and none is open, so an expiry is never the end. One app's failure is
// logged and the next is asked; it answers how many it filed and the first error.
func (Patching) FileApprovals(ctx context.Context, p *runs.Pipeline, project string) (int, error) {
	if on, err := p.PatchingOn(ctx, p.Pool); err != nil || !on {
		return 0, err
	}
	rows, err := p.Pool.Query(ctx, passedSQL, project)
	if err != nil {
		return 0, err
	}
	apps, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	filed := 0
	var first error
	for _, app := range apps {
		ok, err := fileApproval(ctx, p, app)
		if err != nil {
			log.Printf("runs: the patch approval of %s: %v", app, err)
			first = cmp.Or(first, err)
		}
		if ok {
			filed++
		}
	}
	return filed, first
}

// patchAsked is what a patch approval is filed from: the patch, its identity, what preview runs and what
// production ran before it, and what stands in its way.
type patchAsked struct {
	identity, run, pkg, image, replaced string
	fixed                               json.RawMessage
	held, prodRuns, denied, busy        bool
	rebuild                             bool
}

// patchAskedSQL reads $1's patch whose watch passed, as passedSQL names it: its identity not paused, its run neither
// failed nor reverting, the project live. Production must run the image preview ran before the patch, never be held,
// and have no run of the project but the patch going; a team's denial for the identity stands.
const patchAskedSQL = `SELECT a.identity, a.run, d.package_id, d.image, coalesce(r.patch_replaced->>'image', ''),
	coalesce((SELECT e.detail->'fixed' FROM audit_events e WHERE e.run_id = a.run AND e.action = 'patch preview' LIMIT 1), 'null'),
	EXISTS (SELECT 1 FROM prod_holds h WHERE h.project_id = a.project_id),
	EXISTS (SELECT 1 FROM deploys x WHERE x.project_id = a.project_id AND x.env = 'prod' AND x.image = r.patch_replaced->>'image'),
	EXISTS (SELECT 1 FROM promotion_approvals x WHERE x.project_id = a.project_id AND x.kind = 'patch' AND x.patch_identity = a.identity AND x.state = 'denied'),
	EXISTS (SELECT 1 FROM runs b WHERE b.project_id = a.project_id AND b.state IN ('pending', 'running') AND b.id <> a.run),
	i.kind = 'rebuild'
FROM patch_wave_apps a
JOIN patch_identities i ON i.id = a.identity AND i.paused_at IS NULL
JOIN runs r ON r.id = a.run AND r.kind = 'patch' AND r.state IN ('running', 'clean') AND r.reverting_at IS NULL
JOIN deploys d ON d.project_id = a.project_id AND d.env = 'preview' AND d.gate_run = a.run AND d.run_by = 'canyon'
JOIN projects p ON p.id = a.project_id AND p.deleted_at IS NULL AND p.deleting_since IS NULL
WHERE a.project_id = $1 AND a.outcome = 'passed' AND EXISTS (SELECT 1 FROM timeline_rows t WHERE t.run_id = a.run AND t.dedupe = 'patch:passed')`

// patchFix is one of a patch approval's fixes[] (contracts/routes.md, Production approvals).
type patchFix struct {
	runs.PatchFixed
	Class string `json:"class"`
}

// fileApproval files project's patch approval in one transaction under the ledger lock a promotion and a
// person's request take, so of a request and a filing exactly one wins (contracts/routes.md, Production
// approvals). Nothing is filed while Canyon does not patch the app (Patching), production is held,
// runs another image than preview ran before the patch — preview is ahead, or a rollback or an undeploy moved it —
// or a run of the app goes, nor after a team's denial for the identity. A person's pending approval is never
// superseded; one of theirs that expired is replaced, as their own next request replaces it. The app's open patch
// approval of this patch stands; an older patch's, or one expired, is superseded by the new one. Fail closed: a
// patch whose fixes do not read files nothing. It answers whether it filed one.
func fileApproval(ctx context.Context, p *runs.Pipeline, project string) (bool, error) {
	filed := false
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
		if on, err := p.Patching(ctx, tx, project); err != nil || !on {
			return err
		}
		var f patchAsked
		err := tx.QueryRow(ctx, patchAskedSQL, project).Scan(&f.identity, &f.run, &f.pkg, &f.image, &f.replaced, &f.fixed, &f.held, &f.prodRuns, &f.denied, &f.busy, &f.rebuild)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if f.held || !f.prodRuns || f.denied || f.busy {
			return nil
		}
		if f.rebuild { // the org's rebuilds off files no approval for one (contracts/routes.md, the preferences row)
			if on, err := p.RebuildsOn(ctx, tx); err != nil || !on {
				return err
			}
		}
		var fixed []runs.PatchFixed
		if json.Unmarshal(f.fixed, &fixed) != nil || len(fixed) == 0 {
			return nil
		}
		var code, kind, gateRun, image string
		var open bool
		err = tx.QueryRow(ctx, "SELECT code, kind, gate_run, image, expires_at > now() FROM promotion_approvals WHERE project_id = $1 AND state = 'pending' FOR UPDATE", project).
			Scan(&code, &kind, &gateRun, &image, &open)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case open && kind != "patch": // a person's pending approval: a patch approval never supersedes it
			return nil
		case open && gateRun == f.run && image == f.image: // this patch's own, still open
			return nil
		default: // an older patch's, or one expired: superseded
			if _, err := tx.Exec(ctx, "DELETE FROM promotion_approvals WHERE code = $1", code); err != nil {
				return err
			}
		}
		fixes := make([]patchFix, 0, len(fixed))
		for _, x := range fixed {
			class := cmp.Or(x.Class, "os") // a rebase fixes os packages alone: the plan names no other class (plan); a rebuild's say theirs
			x.Class = ""
			fixes = append(fixes, patchFix{PatchFixed: x, Class: class})
		}
		expires := time.Now().UTC().Truncate(time.Second).Add(runs.ApprovalLife)
		for code = ""; code == ""; { // a code that collides with any approval's is drawn again
			draw, err := runs.ApprovalCode()
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at,
				kind, patch_identity, replaces_image, fixes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'canyon', $9, 'patch', $10, $11, $12) ON CONFLICT (code) DO NOTHING`,
				draw, project, f.run, f.pkg, f.image, runs.PatchActor.Kind, runs.PatchActor.Subject, runs.PatchActor.Display, expires, f.identity, f.replaced, fixes)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				code = draw
			}
		}
		filed = true
		return events.AuditTx(ctx, tx, runs.PatchActor, "request patch approval", project, f.run, map[string]any{"package": f.pkg, "code": runs.ShownCode(code), "image": runs.ImageDigest(f.image)})
	})
	return filed, err
}

// fleetDueSQL is each open patch approval of a platform identity an admin approved for the fleet, $1's alone or every
// approved identity's when $1 is empty, with that admin: its identity not paused, and its production not held, which
// is skipped (contracts/routes.md, the fleet approval).
const fleetDueSQL = `SELECT x.code, i.id, i.approved_by_kind, i.approved_by_subject, i.approved_by_display
FROM promotion_approvals x JOIN patch_identities i ON i.id = x.patch_identity AND i.kind = 'rebase' AND i.approved_at IS NOT NULL AND i.paused_at IS NULL
WHERE x.kind = 'patch' AND x.state = 'pending' AND x.expires_at > now() AND ($1 = '' OR i.id = $1)
	AND NOT EXISTS (SELECT 1 FROM prod_holds h WHERE h.project_id = x.project_id)
ORDER BY x.requested_at, x.code`

// DecideFleet decides each open patch approval of an identity an admin approved for production across the fleet,
// identity's alone or every approved identity's for "" (design/PATCHING.md §4.5; contracts/routes.md, Production
// approvals): through the pipeline's DecidePatch, as that admin while they are still one, so a refusal leaves it
// pending for the app's team, as does an admin no longer one, logged once an identity. The fleet approval's route
// calls it once it is given, and each rescan tick after it files. One app's failure is logged and the next is decided;
// it answers how many it approved and the first error. Fail closed: with Canyon not patching, or no DecidePatch, it
// decides nothing.
func (Patching) DecideFleet(ctx context.Context, p *runs.Pipeline, identity string) (int, error) {
	if on, err := p.PatchingOn(ctx, p.Pool); err != nil || !on || p.DecidePatch == nil {
		return 0, err
	}
	rows, err := p.Pool.Query(ctx, fleetDueSQL, identity)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Code, Identity, Kind, Subject, Display string }])
	if err != nil {
		return 0, err
	}
	decided := 0
	var first error
	gone := map[string]bool{} // the identities whose admin is no longer one: logged once, and none of theirs decided
	for _, d := range due {
		if gone[d.Identity] {
			continue
		}
		ok, err := p.DecidePatch(ctx, d.Code, events.Actor{Kind: d.Kind, Subject: d.Subject, Display: d.Display})
		if errors.Is(err, runs.ErrApproverGone) {
			gone[d.Identity] = true
			log.Printf("runs: the fleet approval of %s decides nothing: Canyon cannot tell that %s, who gave it, is still an admin; its patch approvals stay pending for their teams", d.Identity, d.Display)
			continue
		}
		if err != nil {
			log.Printf("runs: the fleet approval of patch approval %s: %v", runs.ShownCode(d.Code), err)
			first = cmp.Or(first, err)
		}
		if ok {
			decided++
		}
	}
	return decided, first
}
