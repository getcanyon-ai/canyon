package runs

import (
	"strings"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/getcanyon-ai/canyon/runs/runstest"
)

func TestReconcileEndsARunWhoseWorkflowWasCancelled(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if err := dbos.CancelWorkflow(durable, run.ID); err != nil {
		t.Fatal(err)
	}
	n, err := pipe.Reconcile(ctx, durable)
	if err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	if state != "failed" || reason == nil || *reason != "interrupted" {
		t.Fatalf("a run DBOS will not drive ends interrupted: %s %v", state, reason)
	}
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != "run.state:pending,run.state:failed" {
		t.Fatalf("ledger: %s", got)
	}
	close(held)
}

func TestReconcileEndsARunWithNoWorkflow(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_orphan00', $1, $2, 'running')", project, pkg); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if state, reason := waitEnded(t, ctx, p, "gr_orphan00"); state != "failed" || reason == nil || *reason != "interrupted" {
		t.Fatalf("a live run nothing drives ends interrupted: %s %v", state, reason)
	}
}

func TestReconcileLeavesARunDBOSWillDriveAlone(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 0 {
		t.Fatalf("reconcile touched a run DBOS is about to drive: %d %v", n, err)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", run.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("still pending: %s (%v)", state, err)
	}
	close(held)
	if state, _ := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("and it then ran to its verdict: %s", state)
	}
}

// The case the sweep exists for: DBOS gave up on the workflow after repeated recovery crashes.
// Staged directly in DBOS's table (the package owns its schema) rather than through three
// crashes; the assertion also pins the status string a version bump could drift.
func TestReconcileEndsARunDBOSGaveUpOn(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded); s != "MAX_RECOVERY_ATTEMPTS_EXCEEDED" {
		t.Fatalf("DBOS's dead-letter status is now %q; the sweep keys on it", s)
	}
	if _, err := p.Exec(ctx, "UPDATE "+durableSchema+".workflow_status SET status = $2 WHERE workflow_uuid = $1", run.ID, string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded)); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "interrupted" {
		t.Fatalf("a run DBOS gave up on ends interrupted: %s %v", state, reason)
	}
	close(held)
}

// A workflow that errored out (the guard's read failing, say) is one DBOS will not drive again.
func TestReconcileEndsARunWhoseWorkflowErrored(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE "+durableSchema+".workflow_status SET status = $2 WHERE workflow_uuid = $1", run.ID, string(dbos.WorkflowStatusError)); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "interrupted" {
		t.Fatalf("%s %v", state, reason)
	}
	close(held)
}

// Every run that needs ending is ended in one sweep, not the first one found.
func TestReconcileEndsEveryRunItShould(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('quote-engine')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_quote00', 'quote-engine', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_orphan01', '" + project + "', '" + pkg + "', 'running')",
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_orphan02', 'quote-engine', 'pkg_quote00', 'pending')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 2 {
		t.Fatalf("two orphans, two ends: %d %v", n, err)
	}
	var live int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM runs WHERE state IN ('pending', 'running')").Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d run(s) still live (%v)", live, err)
	}
}
