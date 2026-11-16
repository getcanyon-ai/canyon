package main

import (
	"context"
	_ "embed"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// demoSeed is the demo fleet roster, shared with the fleet tests so the host a reviewer boots and
// the golden page cannot drift apart.
//
//go:embed demo_seed.sql
var demoSeed string

// seedDemo writes the demo fleet roster and reports whether it did. Guard one is the variable:
// CANYON_DEMO_SEED=1 is set only by the demo compose file, so no company's first boot gets fake rows.
func seedDemo(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	if os.Getenv("CANYON_DEMO_SEED") != "1" {
		return false, nil
	}
	return seedWith(ctx, pool, demoSeed)
}

// seedWith applies roster in one transaction, and only to a database holding no projects at all —
// guard two, because the roster's ids are fixed while the Postgres volume outlives `docker compose
// down`. Soft-deleted rows count: a deleted project still holds its primary key. The count takes no
// lock and two boots racing here would both pass it; the demo stack runs one server, whose fixed host
// port binding forbids a second. A roster that cannot be applied is an error, not a log line.
func seedWith(ctx context.Context, pool *pgxpool.Pool, roster string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed
	var projects int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&projects); err != nil {
		return false, err
	}
	if projects > 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, roster); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
