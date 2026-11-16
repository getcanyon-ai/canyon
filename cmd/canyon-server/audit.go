package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// actor is the ledger's typed triple; the session stage sets it, the audit rows carry it.
type actor = events.Actor

// mutation begins the transaction a state-changing handler works in and takes the timeline
// append lock first, the order every appender keeps (events.LockTx). The change and its audit
// row (events.AuditTx) commit together or neither does.
func (a *app) mutation(ctx context.Context) (pgx.Tx, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := events.LockTx(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// action is the audit row's name for what happened: the route pattern the request matched.
func action(r *http.Request) string {
	return r.Method + " /api/v1" + strings.TrimPrefix(r.Pattern, r.Method+" ")
}
