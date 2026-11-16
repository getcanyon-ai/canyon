package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
)

// sidecarActor is Canyon itself on the trail when it rolls an app's sign-in sidecar after an upgrade.
var sidecarActor = events.Actor{Kind: "canyon", Subject: "upgrade", Display: "Canyon"}

// sidecarsCeiling bounds one POST /sidecars, a list and at most ten Deployment writes; recordCeiling one
// roll's row.
const (
	sidecarsCeiling = time.Minute
	recordCeiling   = 10 * time.Second
)

// rollSidecars asks the Kubernetes executor to roll every app's sign-in sidecar onto its own image when
// the server starts and every `every` after, until ctx ends (contracts/executor.md, POST /sidecars): an
// executor upgraded after the server, or one not up yet, is caught within a tick. Each roll the executor
// answers is one `update sidecar` row on its app, by Canyon, written the first time an answer names it:
// the executor answers it on every call until the environment's next /run, so a lost answer or a failed
// write is written on a later tick. A roll is upkeep, not a deploy: no review, version, run or deploy row,
// and the app's status reads as before.
func (a *app) rollSidecars(ctx context.Context, logger *log.Logger, every time.Duration) {
	for {
		call, cancel := context.WithTimeout(ctx, sidecarsCeiling)
		res, err := a.pipe.Executor.Sidecars(call)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			logger.Print("canyon-server: sign-in sidecars: the server is stopping; what the executor rolled is answered again to the next server")
		case err != nil:
			logger.Printf("canyon-server: sign-in sidecars: %v", err)
		default:
			// its own context: rows for what the executor answered are written even as the server stops
			written := a.recordSidecars(context.WithoutCancel(ctx), logger, res)
			if written > 0 || res.Failed > 0 {
				logger.Printf("canyon-server: sign-in sidecars on %s: %d recorded, %d not moved (the executor's log names each)", res.Image, written, res.Failed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// recordSidecars writes the row of each roll in res the trail does not hold yet, and answers how many it
// wrote. One read of exactly those rolls finds the ones it holds; each new one is written under the append
// lock, read again there.
func (a *app) recordSidecars(ctx context.Context, logger *log.Logger, res executor.Sidecars) int {
	if len(res.Rolled) == 0 {
		return 0
	}
	read, cancel := context.WithTimeout(ctx, recordCeiling)
	defer cancel()
	var apps, envs, names []string
	for _, r := range res.Rolled {
		apps, envs, names = append(apps, r.App), append(envs, r.Target), append(names, r.Roll)
	}
	rows, err := a.pool.Query(read, `SELECT k.app, k.env, k.roll FROM unnest($1::text[], $2::text[], $3::text[]) AS k(app, env, roll)
		WHERE EXISTS (SELECT 1 FROM audit_events e WHERE e.action = 'update sidecar' AND e.project_id = k.app AND e.detail->>'env' = k.env AND e.detail->>'roll' = k.roll)`,
		apps, envs, names)
	if err != nil {
		logger.Printf("canyon-server: sign-in sidecars: the rolls recorded so far: %v", err)
		return 0
	}
	recorded := map[executor.SidecarRoll]bool{}
	var r executor.SidecarRoll
	if _, err := pgx.ForEachRow(rows, []any{&r.App, &r.Target, &r.Roll}, func() error { recorded[r] = true; return nil }); err != nil {
		logger.Printf("canyon-server: sign-in sidecars: the rolls recorded so far: %v", err)
		return 0
	}
	written := 0
	for _, r := range res.Rolled {
		if recorded[r] {
			continue
		}
		switch fresh, err := a.recordSidecar(ctx, r, res.Image); {
		case err != nil:
			logger.Printf("canyon-server: the sign-in sidecar of %s-%s is on %s, not recorded yet: %v", r.App, r.Target, res.Image, err)
		case fresh:
			written++
		}
	}
	return written
}

// recordSidecar writes the `update sidecar` row of roll r onto image unless the trail holds it, answering
// whether it wrote it.
func (a *app) recordSidecar(ctx context.Context, r executor.SidecarRoll, image string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, recordCeiling)
	defer cancel()
	tx, err := a.mutation(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var held, app bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE action = 'update sidecar' AND project_id = $1 AND detail->>'env' = $2 AND detail->>'roll' = $3),
		EXISTS (SELECT 1 FROM projects WHERE id = $1)`, r.App, r.Target, r.Roll).Scan(&held, &app); err != nil {
		return false, err
	}
	switch {
	case held:
		return false, nil
	case !app:
		return false, fmt.Errorf("this host has no app %s: the trail names only apps Canyon has", r.App)
	}
	if err := events.AuditTx(ctx, tx, sidecarActor, "update sidecar", r.App, "", map[string]any{"env": r.Target, "image": image, "roll": r.Roll}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
