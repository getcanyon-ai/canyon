package patching

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/runs"
)

// rebuildSaid is the rebuild plan's server log line (contracts/routes.md, A rebuild patch), its causes as the patch
// plan's line gives them.
func rebuildSaid(builder string, apps map[string]string) string {
	line := planSaid{run: builder, apps: apps}.String()
	line = strings.Replace(line, "runs: the patch plan onto", "runs: the rebuild plan onto", 1)
	return strings.Replace(line, " patch run(s)", " rebuild(s)", 1)
}

// builtWithSQL is the builder that built preview's image $2 of project $1, and whether its row records a values-only
// HMAC: the run that built it says which builder — none when that run recorded none, as a release before the builder
// was recorded never does, however stale a builder its row carries — and a rebased image, which no run built, carries
// its build's on its row (contracts/routes.md, the deploys row).
const builtWithSQL = `SELECT coalesce(CASE WHEN EXISTS (SELECT 1 FROM runs r WHERE r.project_id = d.project_id AND r.build_image = d.image)
		THEN (SELECT r.builder FROM runs r WHERE r.project_id = d.project_id AND r.build_image = d.image ORDER BY r.created_at DESC LIMIT 1) ELSE d.builder END, ''),
	d.values_hmac IS NOT NULL AND d.values_key IS NOT NULL
	FROM deploys d WHERE d.project_id = $1 AND d.env = 'preview' AND d.image = $2 AND d.run_by = 'canyon'`

// sweepRebuilds unplans every app a rebuild identity planned and no rebuild of which started: a tick that plans none.
const sweepRebuilds = `DELETE FROM patch_wave_apps a USING patch_identities i WHERE i.id = a.identity AND i.kind = 'rebuild' AND a.outcome = 'planned'`

// Why an app the rebuild plan read got no rebuild, beside the patch plan's causes.
const (
	whyNoBuilder   = "its image's build recorded no builder by digest, or no values-only HMAC"
	whySameBuilder = "the builder that built its image is the one Canyon names now"
)

// planRebuilds is a tick's rebuild plan, after the rebase plan (contracts/routes.md, A rebuild patch): each app it
// planned no rebase for whose preview counts a runtime or lifecycle finding at or above high, while the org's rebuilds
// reads ask, when the builder that built preview's image is not the one GET /builder names now. Fail closed: a row that
// recorded no builder or values-only HMAC is never planned. It writes the plan under the builder's identity, in waves,
// and starts what is due, at most one rebuild at a time.
func planRebuilds(ctx context.Context, p *runs.Pipeline, run string, candidates map[string]planned) (started int, err error) {
	if on, err := p.RebuildsOn(ctx, p.Pool); err != nil || !on || len(candidates) == 0 {
		if err == nil { // nothing to plan: the rebuild identities' planned apps are no longer planned
			_, err = p.Pool.Exec(ctx, sweepRebuilds)
		}
		return 0, err
	}
	builder, err := p.Executor.BuilderImage(ctx)
	identity := runs.RunImageDigest(builder)
	if err != nil || identity == "" {
		return 0, cmp.Or(err, fmt.Errorf("GET /builder names no builder by digest (%q): no rebuild is planned", builder))
	}
	said := map[string]string{}
	for project, pl := range candidates {
		var was string
		var values bool
		err := p.Pool.QueryRow(ctx, builtWithSQL, project, pl.image).Scan(&was, &values)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			said[project] = runs.WhyMoved
			delete(candidates, project)
		case err != nil:
			return 0, err
		case runs.RunImageDigest(was) == "" || !values: // none recorded, or one not named by digest: fail closed
			said[project] = whyNoBuilder
			delete(candidates, project)
		case runs.RunImageDigest(was) == identity:
			said[project] = whySameBuilder
			delete(candidates, project)
		}
	}
	if err := runningOnly(ctx, p, candidates, said); err != nil {
		return 0, err
	}
	if err := recordedOnly(ctx, p, candidates, said); err != nil {
		return 0, err
	}
	var due []string
	if err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		var err error
		if due, err = writePlan(ctx, tx, "rebuild", identity, run, builder, candidates); err != nil {
			return err
		}
		return whyNotDue(ctx, tx, identity, candidates, said)
	}); err != nil {
		return 0, err
	}
	for _, project := range due {
		pl := candidates[project]
		why, err := p.StartPatch(ctx, project, runs.Patch{Identity: identity, Rebuild: true, Builder: builder, Image: pl.image, Fixes: pl.fixes})
		if err != nil {
			return started, err
		}
		if said[project] = why; why == runs.WhyStarted {
			started++
		}
	}
	log.Print(rebuildSaid(builder, said))
	return started, nil
}
