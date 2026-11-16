// Package events is the run and org timeline of contracts/events.md: the ledger the dashboard
// and the CLI poll. A row is appended in its own short transaction, so a reader sees it as it
// happens, under one advisory lock, so commit order is id order in every process.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind is a row kind from the catalogue in contracts/events.md.
type Kind string

const (
	RunState        Kind = "run.state"
	CheckStarted    Kind = "check.started"
	CheckProgress   Kind = "check.progress"
	CheckCompleted  Kind = "check.completed"
	ProducerSARIF   Kind = "producer.sarif"
	ScanWaiting     Kind = "scan.waiting"
	Progress        Kind = "progress"
	Verdict         Kind = "verdict"
	BuildImage      Kind = "build.image"
	BuildLog        Kind = "build.log"
	DeployCompleted Kind = "deploy.completed"
	AppUpdated      Kind = "app.updated"
	TilesUpdated    Kind = "tiles.updated"
	RunTransition   Kind = "run.transition"
	AuditAppended   Kind = "audit.appended"
)

// RunKinds and OrgKinds are the two catalogue tables; a test holds them to the schema's enums.
var (
	RunKinds = []Kind{RunState, CheckStarted, CheckProgress, CheckCompleted, ProducerSARIF, Progress, ScanWaiting, Verdict, BuildImage, BuildLog, DeployCompleted}
	OrgKinds = []Kind{AppUpdated, TilesUpdated, RunTransition, AuditAppended}
)

// appendLock serialises appends across every server process: a sequence value is taken before
// commit, so without it two rows can commit out of id order and a poller that already passed
// the higher id never sees the lower one, and the table refuses the repair. Not db's migrate lock.
const appendLock int64 = 0x6576656e7473 // "events"

var (
	ErrNoRun = errors.New("events: no such run")
	ErrEnded = errors.New("events: run has ended") // an append after End: a reader was told nothing follows
)

// Row is one timeline row as the API serves it.
type Row struct {
	ID      int64           `json:"id"`
	TS      string          `json:"ts"`
	Scope   string          `json:"scope"`
	Run     string          `json:"run,omitempty"` // present exactly when scope = run
	Project string          `json:"project"`
	Kind    Kind            `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

// AppendRun writes one run-scope row; data must be a JSON object of the kind's shape.
func AppendRun(ctx context.Context, pool *pgxpool.Pool, run, project string, kind Kind, data any) (Row, error) {
	if !slices.Contains(RunKinds, kind) {
		return Row{}, fmt.Errorf("events: %q is not a run-scope kind", kind)
	}
	return appendRow(ctx, pool, Row{Scope: "run", Run: run, Project: project, Kind: kind}, "", data)
}

// LockTx takes the append lock on the caller's transaction. A writer that locks a runs row and
// then appends must call it first: every append path takes this lock and then touches the runs
// row (End, the ended check), so the other order deadlocks. Advisory locks re-enter within a
// transaction, so the appends that follow take it again for free.
func LockTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", appendLock)
	return err
}

// AppendRunTx writes one run-scope row inside the caller's transaction, for a writer whose
// row must commit with its other writes (the run driver). dedupe names a once-per-run row
// ("state:pending", "verdict"): written twice, the second call returns the first row instead
// of appending, so a replayed step cannot duplicate the ledger. Empty means no such key.
func AppendRunTx(ctx context.Context, tx pgx.Tx, run, project string, kind Kind, dedupe string, data any) (Row, error) {
	if !slices.Contains(RunKinds, kind) {
		return Row{}, fmt.Errorf("events: %q is not a run-scope kind", kind)
	}
	return appendRowTx(ctx, tx, Row{Scope: "run", Run: run, Project: project, Kind: kind}, dedupe, data)
}

// AppendOrg writes one org-scope row.
func AppendOrg(ctx context.Context, pool *pgxpool.Pool, project string, kind Kind, data any) (Row, error) {
	if !slices.Contains(OrgKinds, kind) {
		return Row{}, fmt.Errorf("events: %q is not an org-scope kind", kind)
	}
	return appendRow(ctx, pool, Row{Scope: "org", Project: project, Kind: kind}, "", data)
}

// AppendOrgTx writes one org-scope row inside the caller's transaction (the audit stage: the
// audit row and its audit.appended commit together or not at all).
func AppendOrgTx(ctx context.Context, tx pgx.Tx, project string, kind Kind, data any) (Row, error) {
	if !slices.Contains(OrgKinds, kind) {
		return Row{}, fmt.Errorf("events: %q is not an org-scope kind", kind)
	}
	return appendRowTx(ctx, tx, Row{Scope: "org", Project: project, Kind: kind}, "", data)
}

// End marks the run ended, under the append lock, once its last row has committed. From here
// AppendRun refuses the run, so a reader that saw ended_at set and no overflow has seen every
// row there will ever be. Calling it again is a no-op; an unknown run is ErrNoRun.
func End(ctx context.Context, pool *pgxpool.Pool, run string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := EndTx(ctx, tx, run); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EndTx is End inside the caller's transaction: the run driver's terminal step sets the state,
// appends the terminal row and ends the run as one write. Idempotent, like End.
func EndTx(ctx context.Context, tx pgx.Tx, run string) error {
	if err := LockTx(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE runs SET ended_at = coalesce(ended_at, now()) WHERE id = $1", run)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoRun
	}
	return nil
}

func appendRow(ctx context.Context, pool *pgxpool.Pool, row Row, dedupe string, data any) (Row, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Row{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err = appendRowTx(ctx, tx, row, dedupe, data)
	if err != nil {
		return Row{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Row{}, err
	}
	return row, nil
}

func appendRowTx(ctx context.Context, tx pgx.Tx, row Row, dedupe string, data any) (Row, error) {
	raw, err := json.Marshal(data)
	if err != nil || len(raw) == 0 || raw[0] != '{' { // the table cannot be corrected afterwards
		return Row{}, fmt.Errorf("events: %s data must be a JSON object", row.Kind)
	}
	row.Data = raw
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", appendLock); err != nil {
		return Row{}, err
	}
	if dedupe != "" { // a replayed step finds its row first, ended run or not: that is what the key is for
		var ts time.Time
		err := tx.QueryRow(ctx, `SELECT id, ts, kind, data FROM timeline_rows WHERE run_id = $1 AND dedupe = $2`, row.Run, dedupe).Scan(&row.ID, &ts, &row.Kind, &row.Data)
		if err == nil {
			row.TS = ts.UTC().Format(time.RFC3339)
			return row, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Row{}, err
		}
	}
	var runID *string
	if row.Scope == "run" {
		runID = &row.Run
		var ended bool
		if err := tx.QueryRow(ctx, "SELECT ended_at IS NOT NULL FROM runs WHERE id = $1", row.Run).Scan(&ended); errors.Is(err, pgx.ErrNoRows) {
			return Row{}, ErrNoRun
		} else if err != nil {
			return Row{}, err
		} else if ended {
			return Row{}, ErrEnded
		}
	}
	var key *string
	if dedupe != "" {
		key = &dedupe
	}
	var ts time.Time
	// clock_timestamp(), not now(): a waiting appender's transaction began before the lock was granted
	if err := tx.QueryRow(ctx, `INSERT INTO timeline_rows (ts, scope, run_id, project_id, kind, data, dedupe) VALUES (clock_timestamp(), $1, $2, $3, $4, $5, $6) RETURNING id, ts`,
		row.Scope, runID, row.Project, string(row.Kind), raw, key).Scan(&row.ID, &ts); err != nil {
		return Row{}, err
	}
	row.TS = ts.UTC().Format(time.RFC3339)
	return row, nil
}
