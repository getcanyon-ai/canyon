package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readiness is the cached probe behind GET /ready (contracts/routes.md): Postgres reachable and
// migrated, and the durable-run store (DBOS's schema in the same database) answering. It is
// probed in the background so a burst of readiness checks never becomes a burst of database
// queries, and it fails closed: until a probe has said otherwise, and whenever one fails, the
// server is waiting. Failure detail goes to the log, never to the response.
type readiness struct {
	mu      sync.Mutex
	waiting []string
	oidc    *provider // nil unless the host runs oidc: then it also waits on discovery
}

func newReadiness(ctx context.Context, pool *pgxpool.Pool, durable dbos.Context, every time.Duration, logger *log.Logger, oidc *provider) *readiness {
	r := &readiness{waiting: []string{"postgres", "durable_runs"}, oidc: oidc}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			w := probe(ctx, pool, durable, logger)
			r.mu.Lock()
			r.waiting = w
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return r
}

// waitingOn is the closed set contracts/routes.md allows: postgres | durable_runs | oidc.
func (r *readiness) waitingOn() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := append([]string(nil), r.waiting...)
	if r.oidc != nil && !r.oidc.discovered() {
		w = append(w, "oidc")
	}
	return w
}

func probe(ctx context.Context, pool *pgxpool.Pool, durable dbos.Context, logger *log.Logger) []string {
	if pool == nil { // only tests serve without a database; the binary refuses to start without one
		return []string{"postgres", "durable_runs"}
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pending, err := db.Pending(pctx, pool)
	switch {
	case err != nil:
		logger.Printf("readiness: postgres: %v", err)
		return []string{"postgres", "durable_runs"} // the same database: nothing durable answers either
	case pending > 0:
		logger.Printf("readiness: postgres: %d migrations pending", pending)
		return []string{"postgres", "durable_runs"}
	}
	if durable == nil {
		return []string{"durable_runs"}
	}
	dctx, cancelDurable := dbos.WithTimeout(durable, 2*time.Second) // DBOS retries a failing database with backoff; the probe must not wait for that
	defer cancelDurable()
	if _, err := dctx.ListWorkflows(dctx, dbos.WithFilterLimit(1)); err != nil { // one bounded read of DBOS's own tables
		logger.Printf("readiness: durable_runs: %v", err)
		return []string{"durable_runs"}
	}
	return nil
}
