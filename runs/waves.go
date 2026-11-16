package runs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
)

// What became of an app's patch in its identity's waves (contracts/routes.md, the identities row). planned
// and running are under way; the rest are ended. passed alone is a watch that ran to its end; ended_early a
// watch a request ended, which proves nothing; reverted and failed_start trip the breaker; refused a rebase
// refused for what the images are; failed any other end before a watch.
const (
	PatchPlanned     = "planned"
	PatchRunning     = "running"
	PatchPassed      = "passed"
	PatchEndedEarly  = "ended_early"
	PatchReverted    = "reverted"
	PatchFailedStart = "failed_start"
	PatchRefused     = "refused"
	PatchFailed      = "failed"
)

// RecordPatchOutcome records what became of project's patch in identity's waves: the patch run's steps
// (#2098) and its watch (#2100) call it. A revert or a failed start trips the breaker (design/PATCHING.md
// §4.2): the first since the identity was last resumed stalls it, holding the next wave back; the second
// pauses it fleet-wide, with one audit row, actor Canyon, pause patching, and ends every other patch run of
// the identity before its /run, pending or under way, failed/interrupted, its workflow cancelled after the
// commit: a wave already queued goes no further. An admin's resume clears either, and the breaker counts
// afresh.
func (p *Pipeline) RecordPatchOutcome(ctx context.Context, identity, project, outcome string) error {
	if !slices.Contains([]string{PatchPassed, PatchEndedEarly, PatchReverted, PatchFailedStart, PatchFailed}, outcome) {
		return fmt.Errorf("runs: %q is not an end of a patch", outcome)
	}
	var ended []string
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if err := events.LockTx(ctx, tx); err != nil { // the audit row's order
			return err
		}
		var runImage string
		var paused bool
		if err := tx.QueryRow(ctx, "SELECT run_image, paused_at IS NOT NULL FROM patch_identities WHERE id = $1 FOR UPDATE", identity).Scan(&runImage, &paused); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE patch_wave_apps SET outcome = $3, outcome_at = now() WHERE identity = $1 AND project_id = $2", identity, project, outcome)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("runs: %s has no patch in identity %s", project, identity)
		}
		if paused || (outcome != PatchReverted && outcome != PatchFailedStart) {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT a.project_id FROM patch_wave_apps a JOIN patch_identities i ON i.id = a.identity
			WHERE a.identity = $1 AND a.outcome IN ('reverted', 'failed_start') AND a.outcome_at >= i.counted_since ORDER BY a.outcome_at, a.project_id`, identity)
		if err != nil {
			return err
		}
		tripped, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(tripped) == 0 {
			return err
		}
		if len(tripped) == 1 {
			_, err := tx.Exec(ctx, "UPDATE patch_identities SET stalled_at = now(), stalled_projects = $2 WHERE id = $1", identity, tripped)
			return err
		}
		// the pause is what holds the identity now: the stall the first of them left goes with it (#2595)
		if _, err := tx.Exec(ctx, "UPDATE patch_identities SET paused_at = now(), paused_projects = $2, stalled_at = NULL, stalled_projects = NULL WHERE id = $1", identity, tripped); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT r.id, r.project_id FROM runs r JOIN patch_wave_apps a ON a.run = r.id
			WHERE a.identity = $1 AND a.project_id <> $2 AND r.state IN ('pending', 'running') AND r.deploy_started_at IS NULL ORDER BY r.id FOR UPDATE OF r`, identity, project)
		if err != nil {
			return err
		}
		live, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Run, Project string }])
		if err != nil {
			return err
		}
		for _, l := range live {
			if err := EndInterrupted(ctx, tx, l.Run, l.Project); err != nil {
				return err
			}
			ended = append(ended, l.Run)
		}
		return events.AuditTx(ctx, tx, PatchActor, "pause patching", "", "", map[string]any{"identity": identity, "run_image": runImage, "projects": tripped})
	})
	if err != nil {
		return err
	}
	for _, run := range ended {
		Cancel(p.dc, run)
	}
	return nil
}

// refusedForGood are the /rebase refusals that are permanent for an image and the run image the plan named
// (contracts/executor.md, POST /rebase; #2089): run_image_moved is not among them, the plan being stale.
var refusedForGood = []string{"not_rebasable", "another_run_image", "another_stack"}

// RecordPatchRefusal records a rebase of project's image digest onto runImage refused for what the images
// are, which the plan never plans again (design/PATCHING.md §4.2), and ends that app's patch in identity's
// waves refused, out of its wave's count. The patch run's rebase step (#2098) calls it.
func RecordPatchRefusal(ctx context.Context, pool *pgxpool.Pool, identity, project, digest, runImage, reason string) error {
	if !slices.Contains(refusedForGood, reason) {
		return errors.New("runs: only not_rebasable, another_run_image and another_stack are refusals for good")
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO patch_refusals (project_id, digest, run_image, reason) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
			project, digest, runImage, reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE patch_wave_apps SET outcome = $3, outcome_at = now() WHERE identity = $1 AND project_id = $2", identity, project, PatchRefused)
		return err
	})
}
