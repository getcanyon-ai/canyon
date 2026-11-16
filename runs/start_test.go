package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// durable is one DBOS context for the package, in a schema of its own, driving pipe. Start's
// tests hold every workflow before its first write, so a started run stays pending to be
// looked at or cancelled; the pipeline's tests release it.
var (
	durable       dbos.Context
	durableSchema = fmt.Sprintf("dbos_runs_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	pipe          = &Pipeline{Gate: runstest.Golden, Clears: func(context.Context, string, string) (bool, error) { return !uncleared.Load(), nil },
		Org: func(context.Context) string { return "Meridian" }} // as main wires it: the org's name, never ""
	// uncleared stands in for the server's receipt (Pipeline.Clears), which this package cannot evaluate: set,
	// no promotion's review clears it any more. The receipt's own judgement is the server's tests'.
	uncleared atomic.Bool
)

// testPolicy is the snapshot fixture the golden log was recorded against, its tools left out.
func testPolicy() policy.Snapshot {
	var s policy.Snapshot
	if err := json.Unmarshal(contracts.Snapshot, &s); err != nil {
		panic(err)
	}
	// without the tools Canyon runs by default: a test adds the tools it attaches or scans as, and no other
	s.Rules = slices.DeleteFunc(s.Rules, func(r policy.Rule) bool { return r.Producer == policy.Sarif })
	return s
}

func TestMain(m *testing.M) {
	url := os.Getenv("CANYON_TEST_DATABASE_URL")
	if url != "" {
		ctx := context.Background()
		schema := durableSchema
		admin, err := pgxpool.New(ctx, url)
		if err != nil {
			panic(err)
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			panic(err)
		}
		if durable, err = dbos.NewContext(ctx, dbos.Config{AppName: "canyon-runs-test", SystemDBPool: pool, DatabaseSchema: schema, ApplicationVersion: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
			panic(err)
		}
		waitPoll = 200 * time.Millisecond       // a test's waits are short: read its place in line often
		watchEvery.Store(50 * time.Millisecond) // a patch's watch too: a test that watches longer sets its own
		pipe.ProjectPolicy.Store(platformOn)
		pipe.PatchWatch.Store(500 * time.Millisecond)
		pipe.Patcher = testPatcher // as main wires it: Canyon Enterprise's, nil in a build without it
		beatEvery, staleAfter = 250*time.Millisecond, 2*time.Second
		if err := pipe.Register(durable); err != nil {
			panic(err)
		}
		if err := dbos.Launch(durable); err != nil {
			panic(err)
		}
		reporting, stop := context.WithCancel(context.Background())
		defer stop()
		go pipe.ReportQueues(reporting, durable)
		defer func() {
			_ = dbos.Shutdown(durable, 10*time.Second)
			_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			admin.Close()
		}()
	}
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
}

const project, pkg = "claims-portal", "pkg_7d3e8f"

func seeded(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	began := time.Now()
	p, ctx := pgtest.Schema(t)
	if durable == nil {
		t.Skip("no DBOS context without a database")
	}
	t.Cleanup(func() { endWorkflows(t, began) })
	if _, err := db.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	pipe.Pool = p
	held := make(chan struct{})
	pipe.hold.Store(&held)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('" + project + "')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('" + pkg + "', '" + project + "', 'aa', 'bb', 1, 'upload')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return p, ctx
}

// endWorkflows cancels every workflow created since began that is still under way, at the end of the test that
// started it. seeded holds each run, and nothing closes a hold a test leaves: a run left queued would start under
// the next test's hold, pool and fields, parked for good or reading another test's database (#2687).
func endWorkflows(t *testing.T, began time.Time) {
	t.Helper()
	left, err := dbos.ListWorkflows(durable, dbos.WithFilterCreatedAfter(began),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		t.Error(err)
		return
	}
	ids := make([]string, len(left))
	for i, w := range left {
		ids[i] = w.ID
	}
	if len(ids) > 0 {
		if err := dbos.CancelWorkflows(durable, ids); err != nil {
			t.Error(err)
		}
	}
}

func status(t *testing.T, id string) dbos.WorkflowStatusType {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[any](durable, id)
	if err != nil {
		t.Fatalf("workflow %s: %v", id, err)
	}
	st, err := h.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	return st.Status
}

// settled is the workflow's status once DBOS has recorded its end: the run's terminal step
// commits before the workflow function returns, so a reader that saw ended_at can still find
// the status PENDING for a few milliseconds.
func settled(t *testing.T, id string) dbos.WorkflowStatusType {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := status(t, id)
		if (s != dbos.WorkflowStatusPending && s != dbos.WorkflowStatusEnqueued) || time.Now().After(deadline) {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartRecordsTheRunAndItsWorkflowTogether(t *testing.T) {
	p, ctx := seeded(t)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil || run.State != "pending" || !strings.HasPrefix(run.ID, "gr_") {
		t.Fatalf("start: %+v %v", run, err)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", run.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("runs row: %q %v", state, err)
	}
	page, err := events.RunTimeline(ctx, p, run.ID, 0, 10, true)
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Kind != events.RunState || !strings.Contains(string(page.Rows[0].Data), `"pending"`) {
		t.Fatalf("the pending row: %+v %v", page, err)
	}
	if s := status(t, run.ID); s != dbos.WorkflowStatusEnqueued && s != dbos.WorkflowStatusPending {
		t.Fatalf("the workflow exists and waits: %v", s)
	}
}

func TestStartLeavesNoWorkflowWhenTheRunCannotBeRecorded(t *testing.T) {
	p, ctx := seeded(t)
	before, err := durable.ListWorkflows(durable, dbos.WithFilterLimit(1000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: "pkg_nothere"}); err == nil {
		t.Fatal("a package that is not the project's must be refused")
	}
	after, err := durable.ListWorkflows(durable, dbos.WithFilterLimit(1000))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the run was never recorded but a workflow was enqueued: %d before, %d after — the enqueue must ride the same transaction", len(before), len(after))
	}
}

func TestStartRefusesWhileARunIsRunning(t *testing.T) {
	p, ctx := seeded(t)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_busy00', $1, $2, 'running')", project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg}); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("a running run answers run_in_progress, got %v", err)
	}
}

func TestStartSupersedesAPendingRun(t *testing.T) {
	p, ctx := seeded(t)
	first, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil || second.ID == first.ID {
		t.Fatalf("second start: %+v %v", second, err)
	}
	var state, reason string
	var ended *time.Time
	if err := p.QueryRow(ctx, "SELECT state, failure_reason, ended_at FROM runs WHERE id = $1", first.ID).Scan(&state, &reason, &ended); err != nil || state != "failed" || reason != "interrupted" || ended == nil {
		t.Fatalf("the superseded run: %s/%s ended=%v (%v)", state, reason, ended, err)
	}
	page, _ := events.RunTimeline(ctx, p, first.ID, 0, 10, true)
	if len(page.Rows) != 2 || page.Rows[1].Kind != events.RunState || !strings.Contains(string(page.Rows[1].Data), `"interrupted"`) || page.Next != nil {
		t.Fatalf("the superseded run's ledger ends with its terminal row and is closed: %+v", page)
	}
	if s := status(t, first.ID); s != dbos.WorkflowStatusCancelled {
		t.Fatalf("the superseded run's workflow is cancelled, got %v", s)
	}
}

// A promotion is never dropped and never drops someone else's run: a pending promote run refuses
// every start, and a promotion refuses to start over a pending run; both stay as they were.
func TestAPromotionNeitherSupersedesNorIsSuperseded(t *testing.T) {
	promote := StartInput{Project: project, Package: pkg, Promote: &Promotion{Image: "canyon/claims-portal@sha256:" + strings.Repeat("a", 64)}}
	for _, tc := range []struct {
		name          string
		first, second StartInput
	}{
		{"a review over a pending promotion", promote, StartInput{Project: project, Package: pkg}},
		{"a promotion over a pending promotion", promote, promote},
		{"a promotion over a pending review", StartInput{Project: project, Package: pkg}, promote},
	} {
		p, ctx := seeded(t)
		first, err := Start(ctx, p, durable, tc.first)
		if err != nil {
			t.Fatal(err)
		}
		var state string
		if _, err := Start(ctx, p, durable, tc.second); !errors.Is(err, ErrRunInProgress) || p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", first.ID).Scan(&state) != nil || state != "pending" {
			t.Fatalf("%s: %v, the first run %s", tc.name, err, state)
		}
	}
}

func TestAppendRunTxDedupes(t *testing.T) {
	p, ctx := seeded(t)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range 2 {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		row, err := events.AppendRunTx(ctx, tx, run.ID, project, events.Verdict, "verdict", map[string]any{"state": "clean", "blocking": 0, "warnings": 0, "findings": map[string]any{"version": "2.1.0", "runs": []any{}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run.ID).Scan(&n); err != nil || n != 1 || ids[0] != ids[1] {
		t.Fatalf("a replayed step must find its row, not append another: %d rows, ids %v (%v)", n, ids, err)
	}
	// the terminal step replays after the run has ended: its row is found, not refused
	if err := events.End(ctx, p, run.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if row, err := events.AppendRunTx(ctx, tx, run.ID, project, events.Verdict, "verdict", map[string]any{"state": "clean"}); err != nil || row.ID != ids[0] {
		t.Fatalf("a replay on an ended run must hand back the row it wrote before, got %+v %v", row, err)
	}
}

func TestTwoFirstStartsAtOnceLeaveOnePendingRun(t *testing.T) {
	p, ctx := seeded(t)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrRunInProgress) {
			t.Fatalf("start %d: a starter that loses the race answers run_in_progress, never a server error: %v", i, err)
		}
	}
	var pending int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM runs WHERE project_id = $1 AND state = 'pending'", project).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("exactly one pending run survives: %d (%v)", pending, err)
	}
}

// Both starts of one project succeed only when the second supersedes the first while it is pending.
// The first run's workflow may already have read it pending: superseded between that read and its
// first write, it writes nothing more and never reaches the executor, even when the cancel after the
// supersede never lands.
func TestARunSupersededAfterItsWorkflowReadItPendingNeverDeploys(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	cfg := p.Config() // the workflow's pool, named, so the test can see it wait
	cfg.ConnConfig.RuntimeParams["application_name"] = "canyon-runs-superseded"
	wp, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wp.Close)
	pipe.Pool = wp
	held := make(chan struct{})
	pipe.hold.Store(&held)
	first, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil { // the first write waits here, after the read
		t.Fatal(err)
	}
	pipe.hold.Store(nil)
	close(held)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting bool
		if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name = 'canyon-runs-superseded' AND wait_event = 'advisory')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run's workflow never reached its first write")
		}
	}
	second, superseded, err := StartTx(ctx, tx, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: testPolicy()})
	if err != nil || superseded != first.ID {
		t.Fatalf("the second start supersedes the first: %v, superseded %q", err, superseded)
	}
	if err := tx.Commit(ctx); err != nil { // and no Cancel: a crash between the commit and the cancel
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, second.ID); state != "clean" {
		t.Fatalf("the second run: %s %s", state, why(reason))
	}
	settled(t, first.ID)
	if n := f.Runs.Load(); n != 1 {
		t.Fatalf("the superseded run and the run that superseded it both reached the executor: %d /runs", n)
	}
}

func TestRunIDsAreWideEnoughToNeverCollide(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := newID()
		if err != nil || len(id) != len("gr_")+16 || seen[id] {
			t.Fatalf("run ids are 64 random bits (the DBOS workflow id, which swallows a collision): %q %v", id, err)
		}
		seen[id] = true
	}
}

// An appender holds the append lock and then touches the runs row (End does exactly that);
// Start must take the append lock before it locks the row, or the two deadlock.
func TestStartTakesTheAppendLockBeforeTheRunRow(t *testing.T) {
	p, ctx := seeded(t)
	first, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	appender := make(chan error, 1)
	held := make(chan struct{})
	go func() {
		tx, err := p.Begin(ctx)
		if err != nil {
			appender <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := events.LockTx(ctx, tx); err != nil {
			appender <- err
			return
		}
		close(held)
		time.Sleep(300 * time.Millisecond) // long enough for Start to be waiting
		if _, err := tx.Exec(ctx, "UPDATE runs SET state = state WHERE id = $1", first.ID); err != nil {
			appender <- err
			return
		}
		appender <- tx.Commit(ctx)
	}()
	<-held
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg}); err != nil {
		t.Fatalf("start against a live appender: %v", err)
	}
	if err := <-appender; err != nil {
		t.Fatalf("the appender against a live start: %v", err)
	}
}

// Start begins a transaction around StartTx, commits it and cancels the superseded run: the
// tests' way to start a run with nothing else to commit alongside it.
func Start(ctx context.Context, pool *pgxpool.Pool, dc dbos.Client, in StartInput) (Run, error) {
	if len(in.Policy.Rules) == 0 { // the fixture stands in for the snapshot the server composes at run start
		in.Policy = testPolicy()
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, superseded, err := StartTx(ctx, tx, dc, in)
	if err != nil {
		return Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, err
	}
	Cancel(dc, superseded)
	return run, nil
}
