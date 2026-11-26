// Package runs starts gate runs: the row in the runs table (the governance record) and the
// DBOS workflow that drives it (the machinery), committed together.
package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	Queue    = "gate"     // one queue, no limits: one-active-run-per-project is the runs table's partial index
	Workflow = "gate-run" // the workflow name the driver registers
)

var (
	ErrRunInProgress = errors.New("runs: a run of this project is running")                                     // 409 run_in_progress
	ErrUndeploying   = errors.New("runs: an undeploy of this project is in progress")                           // 409 undeploying
	ErrUndeployBusy  = errors.New("runs: another change of this project holds its undeploy lock past the wait") // 409 busy
)

// UndeployWait bounds a wait on a project's undeploy lock (contracts/routes.md, 409 busy): the waiter holds a
// database connection meanwhile, and waiters without end behind a recreation of the app's network could take
// every connection of the pool that recreation needs to finish.
const UndeployWait = 20 * time.Second

// waitBounded is the statement that bounds the lock waits after it, local to the transaction or not.
func waitBounded(local bool) string {
	scope := ""
	if local {
		scope = "LOCAL "
	}
	return fmt.Sprintf("SET %slock_timeout = %d", scope, UndeployWait.Milliseconds())
}

// busy is ErrUndeployBusy for a lock wait that ran past its bound (SQLSTATE 55P03, lock_not_available).
func busy(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "55P03" {
		return ErrUndeployBusy
	}
	return err
}

// The project's undeploy lock: an undeploy holds it from its run check to its commit, a start tries it.
const (
	// Advisory locks are database-wide, so the key names the schema too: two Canyons (or two test
	// packages) sharing one database never hold each other's projects.
	lockUndeploy = "SELECT pg_advisory_xact_lock(hashtext(current_schema() || ':undeploy:' || $1))"
	tryUndeploy  = "SELECT pg_try_advisory_xact_lock_shared(hashtext(current_schema() || ':undeploy:' || $1))" // shared: starts never hold each other, only an undeploy's exclusive hold refuses them
)

// clearStopping clears the project's stopping_since marks, which only the lock's holder sets: one still
// there when the lock is taken is a dead holder's, and ShownDeploys must not read it as this one's (#2202).
const clearStopping = "UPDATE deploys SET stopping_since = NULL WHERE project_id = $1 AND stopping_since IS NOT NULL"

// LockUndeploy takes the project's undeploy lock on conn until unlock: an undeploy commits its first
// step and calls the executor while it holds it, so it outlives a transaction. A lock unlock cannot let
// go of goes with its connection, closed, so the pool never hands it on. Taken, a mark a dead holder
// left is cleared before the caller's work. It waits UndeployWait at most, then is ErrUndeployBusy.
func LockUndeploy(ctx context.Context, conn *pgx.Conn, project string) (unlock func(), err error) {
	if _, err := conn.Exec(ctx, waitBounded(false)); err != nil {
		return nil, err
	}
	_, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext(current_schema() || ':undeploy:' || $1))", project)
	if _, rerr := conn.Exec(context.WithoutCancel(ctx), "RESET lock_timeout"); rerr != nil { // the pool never hands on a bounded connection: it goes, and a lock it took with it
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(err, rerr)
	}
	if err != nil {
		return nil, busy(err)
	}
	unlock = func() {
		ctx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext(current_schema() || ':undeploy:' || $1))", project); err != nil {
			_ = conn.Close(ctx)
		}
	}
	if _, err := conn.Exec(ctx, clearStopping, project); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// UndeployingTx is ErrUndeploying while an undeploy or a hold of the project holds its undeploy lock: it
// tries the lock shared, never waits on it, and holds it to the transaction's end, so one that starts
// after sees what this transaction did.
func UndeployingTx(ctx context.Context, tx pgx.Tx, project string) error {
	var free bool
	if err := tx.QueryRow(ctx, tryUndeploy, project).Scan(&free); err != nil {
		return err
	}
	if !free {
		return ErrUndeploying
	}
	return nil
}

// LockUndeployTx takes the project's undeploy lock for the transaction: a start of a run of the
// project answers ErrUndeploying until it ends. It waits only on another holder of the project's lock, for
// UndeployWait at most (then ErrUndeployBusy), and clears a mark a dead holder left, as LockUndeploy does.
func LockUndeployTx(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, waitBounded(true)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, lockUndeploy, project); err != nil {
		return busy(err)
	}
	_, err := tx.Exec(ctx, clearStopping, project)
	return err
}

// RateLimited is a start past the policy snapshot's gate-run limits (contracts/routes.md, 429
// rate_limited): the project's or the actor's runs of the last hour. RetryAfter is when the
// oldest counted run ages out of the hour, never under a second.
type RateLimited struct {
	PerActor   bool // the actor's limit, else the project's
	Limit      int
	RetryAfter time.Duration
}

func (e *RateLimited) Error() string {
	return fmt.Sprintf("runs: %d gate runs an hour reached, retry in %s", e.Limit, e.RetryAfter)
}

// StartInput is a request to review a package.
type StartInput struct {
	Project string
	Package string       // must be the project's own package (the runs table enforces it)
	Deploy  bool         // continue into build and run on a clean verdict (contracts/routes.md row 62)
	Actor   events.Actor // who triggered the run: the deploy's audit row names them
	GitHub  int64        // the numeric id of the GitHub account whose push started the run, 0 for none: whoever linked it authors the run (#2228)
	Promote *Promotion   // set, the run promotes what preview runs to prod instead of reviewing Package

	RollbackTo string // set, a rollback review of Package: the digest production ran, which it ships once its review passes (contracts/routes.md, the gate-runs row)
	Reason     string // the rollback's incident or change reference, recorded on the deploy prod row that ships it

	Policy policy.Snapshot // composed once at run start, under the ledger lock a policy apply also takes: the gate derives every finding's blocks from it, and a replay reads it back from the journal

	Requests bool // the caller owns the app or is an admin, judged by the request that starts the run: its deploy names the app's pending access requests

	// Laptop is a host whose CANYON_INSTALL_MODE is laptop, the demo's: a scan the demo leaves off is said as skipped
	// as the run starts (contracts/events.md, progress; #237). Zero, any other host.
	Laptop bool
}

// Input is what the gate-run workflow receives: everything a replay needs, frozen at start.
type Input struct {
	Run     string       `json:"run"`
	Project string       `json:"project"`
	Package string       `json:"package"`
	Deploy  bool         `json:"deploy,omitempty"`
	Actor   events.Actor `json:"actor"`
	Promote *Promotion   `json:"promote,omitempty"`

	RollbackTo string `json:"rollback_to,omitempty"`

	Policy policy.Snapshot `json:"policy"`

	// Reports are the ids of the package's current reports of every imported tool that is on, read at
	// start under the ledger lock every attach takes: the gate reads these, never whatever is newest.
	Reports []int64 `json:"reports,omitempty"`

	Patch *Patch `json:"patch,omitempty"` // set, a patch run of preview's package (design/PATCHING.md §4.3), on the patches queue

	Requests bool `json:"requests,omitempty"` // StartInput's: the deploy's access lines name the app's pending requests (contracts/events.md, build.log)
}

// Promotion is the preview row a promote run ships, read under the lock its run started in: the
// image by digest, never a rebuild, and the run and actor behind it, which prod's row repeats.
type Promotion struct {
	Image      string       `json:"image"`
	GateRun    string       `json:"gate_run"`
	DeployedBy events.Actor `json:"deployed_by"`
	Rollback   bool         `json:"rollback,omitempty"`    // GateRun is a rollback review and Image its ships: an image production ran before
	Reason     string       `json:"reason,omitempty"`      // that review's reason
	Restart    bool         `json:"restart,omitempty"`     // a lift's start of the held image (contracts/routes.md, The hold): its deploy prod row says so
	ReceiptRun string       `json:"receipt_run,omitempty"` // the review the receipt accepted it under, cleared again before production starts (Pipeline.Clears)
}

// Run is the row a start answers with (contracts/routes.md).
type Run struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Start records a pending run and enqueues its workflow in one transaction, so a crash between
// the two cannot leave a run nobody will ever execute. A pending run of the project, and a patch run
// before its /run, pending or running, is superseded — failed, interrupted, its workflow cancelled — a patch
// run, or a promote run shipping a patch, whose watch runs has its watch ended clean (endWatchTx) and is
// cancelled too, and any other running one,
// a patch run during its /run or reverting among them, makes this request ErrRunInProgress, as
// a pending promote run does, and any active run but a patch run before its /run a promotion
// (contracts/routes.md).
// Cancel is the best-effort cancel of a superseded or cancelled pending run's workflow, after the
// commit that ended it, and of a superseded patch run's under way with the tasks it queued: a crash before
// it leaves an enqueued workflow whose run is already failed, so the driver reads the run's state before it
// does anything.
func Cancel(dc dbos.Client, ended string) {
	if ended != "" {
		_ = dbos.CancelWorkflows(dc, []string{ended, watchID(ended)}) // a patch run's watch is a workflow of its own (#2567)
		_ = Dequeue(dc, ended)
	}
}

// StartTx records the run and enqueues its workflow inside the caller's transaction, so what
// the caller commits alongside (its audit row) lands with them or not at all. It takes the
// append lock first, the order every appender uses; the lock is re-entrant within the
// transaction. The superseded run, if any, is returned for Cancel after the commit. An undeploy
// of the project in progress makes it ErrUndeploying: it tries the undeploy lock, never waits on it,
// and holds it to its commit, so an undeploy that starts after it sees its run. A run that writes
// production while it is held is ErrHeld, the hold read under the ledger lock the hold is written under
// (contracts/routes.md, The hold): a lift's start deletes the hold in its transaction first.
func StartTx(ctx context.Context, tx pgx.Tx, dc dbos.Client, in StartInput) (run Run, superseded string, err error) {
	if err := events.LockTx(ctx, tx); err != nil {
		return Run{}, "", err
	}
	if err := UndeployingTx(ctx, tx, in.Project); errors.Is(err, ErrUndeploying) {
		// a patch run, or a promote run shipping a patch, past its /run whose revert holds the undeploy lock across its
		// checks and its /run, its compare-and-set committed or not yet: a run is in progress, never an undeploy
		// (contracts/routes.md, Patch runs)
		var patching bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE project_id = $1 AND `+shipsPatchSQL+` AND ended_at IS NULL
			AND EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = runs.id AND dedupe IN ('deploy:preview', 'deploy:prod')))`, in.Project).Scan(&patching); err != nil {
			return Run{}, "", err
		}
		if patching {
			return Run{}, "", ErrRunInProgress
		}
		return Run{}, "", err
	} else if err != nil {
		return Run{}, "", err
	}
	kind := "gate"
	if in.Promote != nil {
		kind = "promote"
	}
	if Writes(kind, in.Deploy) == "prod" {
		if err := held(ctx, tx, in.Project); err != nil {
			return Run{}, "", err
		}
	}
	if in.Promote == nil { // a promotion is not a gate run: neither limited nor counted
		if err := limited(ctx, tx, in); err != nil {
			return Run{}, "", err
		}
	}
	var activeID, activeState, activeKind string
	var deploying, watching bool // its /run has begun: a patch run is ended only before it; recorded and not reverting, its watch runs, a promotion shipping a patch's too
	err = tx.QueryRow(ctx, `SELECT id, state, kind, deploy_started_at IS NOT NULL, `+watchingSQL+`
		FROM runs WHERE project_id = $1 AND state IN ('pending', 'running') FOR UPDATE`, in.Project).Scan(&activeID, &activeState, &activeKind, &deploying, &watching)
	switch {
	case err == nil && activeKind == "patch" && !deploying: // a person's run goes first: a patch run is ended before its /run, as a pending run is, and during it the start is 409 (contracts/routes.md, Patch runs)
		if err := EndInterrupted(ctx, tx, activeID, in.Project); err != nil {
			return Run{}, "", err
		}
	case err == nil && watching && (in.Deploy || in.Promote != nil || in.RollbackTo != ""): // after it, a start that changes what runs ends the watch clean and goes ahead; one that deploys nothing is 409 below
		if err := endWatchTx(ctx, tx, activeID, in.Project, in.Actor, "started another run of this app"); err != nil {
			return Run{}, "", err
		}
	case err == nil && (activeState == "running" || activeKind == "promote" || in.Promote != nil): // a promotion is never dropped, and never drops another run
		return Run{}, "", ErrRunInProgress
	case err == nil:
		if err := EndInterrupted(ctx, tx, activeID, in.Project); err != nil {
			return Run{}, "", err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return Run{}, "", err
	}
	id, err := newID()
	if err != nil {
		return Run{}, "", err
	}
	// the actor lands on the row (contracts/routes.md, started_by); a zero actor stores a null kind (the CHECK refuses an empty one), and the list reads a null kind as no actor
	var ships, receipt *string // a promote run's image, on its row as in its input: the reconcile reads no input; and the review its receipt accepted
	if in.Promote != nil {
		ships = &in.Promote.Image
		if in.Promote.ReceiptRun != "" {
			receipt = &in.Promote.ReceiptRun
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display, kind, deploys, rollback_to, rollback_reason, rollback_replaces, promote_image, receipt_run, started_by_github)
		VALUES ($1, $2, $3, 'pending', NULLIF($4, ''), $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), CASE WHEN $9 <> '' THEN (SELECT image FROM deploys WHERE project_id = $2 AND env = 'prod') END, $11, $12, NULLIF($13, 0))`,
		id, in.Project, in.Package, in.Actor.Kind, in.Actor.Subject, in.Actor.Display, kind, in.Deploy, in.RollbackTo, in.Reason, ships, receipt, in.GitHub); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.ConstraintName == "runs_one_active_per_project" { // two first starts saw no active run: the index decides, the loser answers 409
			return Run{}, "", ErrRunInProgress
		}
		return Run{}, "", fmt.Errorf("runs: %w", err)
	}
	if _, err := events.AppendRunTx(ctx, tx, id, in.Project, events.RunState, "state:pending", map[string]any{"state": "pending"}); err != nil {
		return Run{}, "", err
	}
	if in.Promote == nil { // a review's scans: one the demo leaves off is said now, in the start's own transaction, so no workflow step writes it
		for _, tool := range skippedScans(in.Policy, in.Laptop) {
			if _, err := events.AppendRunTx(ctx, tx, id, in.Project, events.Progress, "skipped:"+tool, map[string]any{"step": "scan", "tool": tool, "status": "skipped", "reason": "laptop_demo"}); err != nil {
				return Run{}, "", err
			}
		}
	}
	var reports []int64
	if in.Promote == nil {
		if reports, err = CurrentReports(ctx, tx, in.Package, importedTools(in.Policy)); err != nil {
			return Run{}, "", err
		}
	}
	if _, err := dbos.Enqueue[any](dc, Queue, Workflow, Input{Run: id, Project: in.Project, Package: in.Package, Deploy: in.Deploy, Actor: in.Actor, Promote: in.Promote, RollbackTo: in.RollbackTo, Policy: in.Policy, Reports: reports, Requests: in.Requests}, dbos.WithEnqueueWorkflowID(id), dbos.WithEnqueueTransaction(tx)); err != nil {
		return Run{}, "", fmt.Errorf("runs: enqueue: %w", err)
	}
	return Run{ID: id, State: "pending"}, activeID, nil
}

// limited counts the gate runs of the last hour, superseded ones too, for the project, and for the
// actor (a start with none has no actor count) with its attaches, under the lock every start takes. A
// rollback review is counted with rollback reviews alone, by the same numbers, so a busy hour of reviews
// never holds one back during an incident (contracts/routes.md, the gate-runs row).
func limited(ctx context.Context, tx pgx.Tx, in StartInput) error {
	rollback := in.RollbackTo != ""
	if err := over(ctx, tx, false, in.Policy.Limits.GateRunsPerProjectPerHour, "SELECT created_at FROM runs WHERE kind = 'gate' AND (rollback_to IS NOT NULL) = $2 AND project_id = $1", in.Project, rollback); err != nil || in.Actor.Kind == "" {
		return err
	}
	if rollback {
		return over(ctx, tx, true, in.Policy.Limits.GateRunsPerActorPerHour, "SELECT created_at FROM runs WHERE kind = 'gate' AND rollback_to IS NOT NULL AND started_by_kind = $1 AND started_by_subject = $2", in.Actor.Kind, in.Actor.Subject)
	}
	return ActorLimited(ctx, tx, in.Policy.Limits.GateRunsPerActorPerHour, in.Actor)
}

// ActorLimited counts the actor's gate runs and the reports it attached in the last hour against the
// per-actor limit (contracts/routes.md, the gate-runs and reports rows): a start and an attach ask it.
func ActorLimited(ctx context.Context, q querier, limit int, by events.Actor) error {
	return over(ctx, q, true, limit, `SELECT created_at FROM runs WHERE kind = 'gate' AND rollback_to IS NULL AND started_by_kind = $1 AND started_by_subject = $2
		UNION ALL SELECT attached_at FROM package_reports WHERE attached_by_kind = $1 AND attached_by_subject = $2`, by.Kind, by.Subject)
}

// over is RateLimited when limit or more of the instants counted selects fall in the last hour.
func over(ctx context.Context, q querier, perActor bool, limit int, counted string, args ...any) error {
	var n, retry int
	if err := q.QueryRow(ctx, `SELECT count(*), COALESCE(GREATEST(1, ceil(extract(epoch FROM min(at) + interval '1 hour' - now())))::int, 0)
		FROM (`+counted+`) c(at) WHERE at > now() - interval '1 hour'`, args...).Scan(&n, &retry); err != nil {
		return fmt.Errorf("runs: rate limit: %w", err)
	}
	if n >= limit {
		return &RateLimited{PerActor: perActor, Limit: limit, RetryAfter: time.Duration(retry) * time.Second}
	}
	return nil
}

// EndPatchTx ends the project's active patch run failed/interrupted before its /run, under the ledger lock,
// inside the caller's transaction: a person's request that changes the app always goes first (contracts/routes.md,
// Patch runs). It answers the run it ended, "" for none, for Cancel after the commit.
func EndPatchTx(ctx context.Context, tx pgx.Tx, project string) (string, error) {
	if err := events.LockTx(ctx, tx); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id FROM runs WHERE project_id = $1 AND kind = 'patch' AND state IN ('pending', 'running') AND deploy_started_at IS NULL FOR UPDATE", project).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, EndInterrupted(ctx, tx, id, project)
}

// EndInterrupted ends a pending run failed/interrupted — superseded by a newer start, or cancelled —
// with its terminal row and ended_at, inside the caller's transaction under the ledger lock. A run
// that has already ended writes nothing and is events.ErrEnded. Cancel its workflow after the commit.
func EndInterrupted(ctx context.Context, tx pgx.Tx, id, project string) error {
	tag, err := tx.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted' WHERE id = $1 AND ended_at IS NULL", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return events.ErrEnded
	}
	if _, err := events.AppendRunTx(ctx, tx, id, project, events.RunState, "state:failed", map[string]any{"state": "failed", "failure_reason": "interrupted"}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE runs SET ended_at = now() WHERE id = $1", id)
	return err
}

// newID is 64 random bits: the run id is also the DBOS workflow id, and DBOS answers a colliding
// enqueue with success on the existing workflow, so a collision would be a run nobody executes.
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "gr_" + hex.EncodeToString(b[:]), nil
}
