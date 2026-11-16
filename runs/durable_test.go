package runs

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// serverOn launches a run driver as canyon-server does, on schema: NewDurable, the pipeline
// registered, Launch, under version, which names the pipeline's workflows: canyon-server's without the
// GitHub App's, which this package cannot register. life is the process's: cancelled, its server says
// nothing more, as a killed one. listen, named, is the only queue it takes work from until it listens
// again.
func serverOn(t *testing.T, life context.Context, p *pgxpool.Pool, schema, version string, listen ...string) (dbos.Context, func()) {
	t.Helper()
	recorded(t, version, slices.DeleteFunc(slices.Clone(workflowSets[durableVersion]), func(name string) bool { return strings.HasPrefix(name, "github-") })...)
	t.Setenv("DBOS__APPVERSION", version)
	dc, err := NewDurable(t.Context(), os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Pipeline{Pool: p, Repos: pipe.Repos, Gate: runstest.CleanGolden, Executor: pipe.Executor, Vault: pipe.Vault, PublicURL: pipe.PublicURL, JWKSURL: pipe.JWKSURL, Org: pipe.Org}).Register(dc); err != nil {
		t.Fatal(err)
	}
	if len(listen) > 0 {
		dc.ListenQueues(dc, listen...)
	}
	stop, err := Launch(life, p, dc, schema)
	if err != nil {
		t.Fatal(err)
	}
	return dc, sync.OnceFunc(stop)
}

// twoServers answers the schema the servers share and a function that waits until a run's build is
// under way: the executor answers it busy for some seconds, so the run is in flight on whichever
// servers drive it, and then builds.
func twoServers(t *testing.T) (*pgxpool.Pool, context.Context, string, func()) {
	t.Helper()
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.BuildBusy.Store(6) // each busy answer is asked again after its Retry-After, a second
	t.Cleanup(func() { f.BuildBusy.Store(0) })
	schema := fmt.Sprintf("dbos_two_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	return p, ctx, schema, func() {
		deadline := time.Now().Add(15 * time.Second)
		for f.Builds.Load() < 1 {
			if time.Now().After(deadline) {
				t.Fatal("the run's build never began")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// drivenBy fails the test unless dc's server is the one driving the run.
func drivenBy(t *testing.T, ctx context.Context, p *pgxpool.Pool, schema, run string, dc dbos.Context) {
	t.Helper()
	var executor string
	if err := p.QueryRow(ctx, "SELECT executor_id FROM "+schema+".workflow_status WHERE workflow_uuid = $1", run).Scan(&executor); err != nil || executor != dc.GetExecutorID() {
		t.Fatalf("run %s is driven by %q, want %q (%v)", run, executor, dc.GetExecutorID(), err)
	}
}

// A server starting beside another — a rolling update's new pod, a second replica — never runs what
// the other is driving, and once the first stops, the one still up finishes its run (#1715): a clean
// stop hands it over at once, never only once the stopped server's row is stale (#2271).
func TestAServerStartingBesideAnotherNeverTakesItsRuns(t *testing.T) {
	p, ctx, schema, building := twoServers(t)
	first, stopFirst := serverOn(t, ctx, p, schema, "runs-v1")
	t.Cleanup(stopFirst)
	run, err := Start(ctx, p, first, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	building()
	_, stopSecond := serverOn(t, ctx, p, schema, "runs-v1")
	t.Cleanup(stopSecond)
	time.Sleep(2 * time.Second) // the run reads its build every waitPoll: a second driver of it conflicts within this
	drivenBy(t, ctx, p, schema, run.ID, first)
	stopFirst()
	stopped := time.Now()
	waitRow(t, ctx, p, schema, run.ID, "left the stopped server", func(status dbos.WorkflowStatusType, _, executor string) bool {
		return status != dbos.WorkflowStatusPending || executor != first.GetExecutorID()
	})
	if moved := time.Since(stopped); moved >= staleAfter {
		t.Fatalf("the cleanly stopped server's run moved %s after its stop, only once its row was stale (%s)", moved, staleAfter)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the run, finished by the server still up: %s %s", state, why(reason))
	}
	if kinds := strings.Join(kinds(t, ctx, p, run.ID), ","); strings.Count(kinds, "deploy.completed") != 1 {
		t.Fatalf("one deploy recorded: %s", kinds)
	}
}

// A server killed mid-run never says it stopped: the one still up finishes the run once the dead
// one's row has gone stale.
func TestAKilledServersRunCompletesOnTheSurvivor(t *testing.T) {
	p, ctx, schema, building := twoServers(t)
	life, kill := context.WithCancel(ctx)
	defer kill()
	first, _ := serverOn(t, life, p, schema, "runs-v1")
	second, stopSecond := serverOn(t, ctx, p, schema, "runs-v1", "none") // up before the run, taking nothing, and leaving it to the first
	t.Cleanup(stopSecond)
	run, err := Start(ctx, p, first, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	building()
	drivenBy(t, ctx, p, schema, run.ID, first)
	second.ListenQueues(second)                                  // every queue again
	kill()                                                       // its row stays behind
	if err := dbos.Shutdown(first, 10*time.Second); err != nil { // and its runs stop where they were, unrecorded
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the killed server's run, on the survivor: %s %s", state, why(reason))
	}
}

// recorded records names as version's set for the test, as a release records its own in workflowSets.
func recorded(t *testing.T, version string, names ...string) {
	t.Helper()
	slices.Sort(names)
	workflowSets[version] = names
	t.Cleanup(func() { delete(workflowSets, version) })
}

// probeWorkflow and controlWorkflow are two workflows of the probe queue, functions of their own so DBOS
// registers each: "probe", the one a release adds, and "control", one a server runs to show it read the
// queue.
func probeWorkflow(_ dbos.Context, in string) (string, error)   { return in, nil }
func controlWorkflow(_ dbos.Context, in string) (string, error) { return in, nil }

var probes = map[string]func(dbos.Context, string) (string, error){"probe": probeWorkflow, "control": controlWorkflow}

// probeServer launches a server of version that runs workflows, none of the pipeline's, on a queue of
// their own with one slot, as a release adding a workflow would register it. listen as serverOn's.
func probeServer(t *testing.T, ctx context.Context, p *pgxpool.Pool, schema, version string, workflows []string, listen ...string) (dbos.Context, func()) {
	t.Helper()
	recorded(t, version, slices.Clone(workflows)...)
	t.Setenv("DBOS__APPVERSION", version)
	dc, err := NewDurable(t.Context(), os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range workflows {
		dbos.RegisterWorkflow(dc, probes[name], dbos.WithWorkflowName(name))
	}
	if _, err := dbos.RegisterQueue(dc, "probe", dbos.WithGlobalConcurrency(1)); err != nil {
		t.Fatal(err)
	}
	if len(listen) > 0 {
		dc.ListenQueues(dc, listen...)
	}
	stop, err := Launch(ctx, p, dc, schema)
	if err != nil {
		t.Fatal(err)
	}
	return dc, sync.OnceFunc(stop)
}

// workflowRow answers a journaled workflow's status, version and executor.
func workflowRow(t *testing.T, ctx context.Context, p *pgxpool.Pool, schema, id string) (status dbos.WorkflowStatusType, version, executor string) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT status, application_version, executor_id FROM "+schema+".workflow_status WHERE workflow_uuid = $1", id).Scan(&status, &version, &executor); err != nil {
		t.Fatal(err)
	}
	return status, version, executor
}

// waitRow waits up to 15 s for a journaled workflow's row to be as ok says, and answers it.
func waitRow(t *testing.T, ctx context.Context, p *pgxpool.Pool, schema, id, want string, ok func(dbos.WorkflowStatusType, string, string) bool) (dbos.WorkflowStatusType, string, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, version, executor := workflowRow(t, ctx, p, schema, id)
		if ok(status, version, executor) {
			return status, version, executor
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never %s: %s, stamped %s, under %q", id, want, status, version, executor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// beats waits until dc's server has written its row n times more. Each write is followed by its recovery
// and hand-over, so after two, one whole round of them has run since the call.
func beats(t *testing.T, ctx context.Context, p *pgxpool.Pool, dc dbos.Context, n int) {
	t.Helper()
	seen := func() (at time.Time) {
		if err := p.QueryRow(ctx, "SELECT seen_at FROM durable_executors WHERE id = $1", dc.GetExecutorID()).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	last, deadline := seen(), time.Now().Add(10*time.Second)
	for n > 0 {
		if at := seen(); at.After(last) {
			last, n = at, n-1
		}
		if time.Now().After(deadline) {
			t.Fatal("the server stopped writing its row")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// polled shows dc's server has read the probe queue, and run a round of recovery and hand-over, since the
// call: a control workflow stamped with its version, enqueued now, finishes under it.
func polled(t *testing.T, ctx context.Context, p *pgxpool.Pool, schema string, dc dbos.Context) {
	t.Helper()
	id := fmt.Sprintf("control-%d", time.Now().UnixNano())
	if _, err := dbos.Enqueue[any](dc, "probe", "control", "ping", dbos.WithEnqueueWorkflowID(id)); err != nil {
		t.Fatal(err)
	}
	waitRow(t, ctx, p, schema, id, "ran on the server that read the queue", func(status dbos.WorkflowStatusType, _, executor string) bool {
		return status == dbos.WorkflowStatusSuccess && executor == dc.GetExecutorID()
	})
	beats(t, ctx, p, dc, 2)
}

// A recorded set never changes: a changed set is a new version's (#2271).
func TestARecordedWorkflowSetNeverChanges(t *testing.T) {
	if want := []string{"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}; !slices.Equal(workflowSets["canyon-v1"], want) {
		t.Fatalf("canyon-v1 names %q, but recorded %q: a recorded set never changes. Bump durableVersion in runs/durable.go and record the new set under it in workflowSets, leaving canyon-v1's as it is", workflowSets["canyon-v1"], want)
	}
	for version, names := range workflowSets {
		if !slices.IsSorted(names) {
			t.Errorf("%s's set is not sorted, so Launch, which compares sorted names, refuses every server of it: %q", version, names)
		}
	}
	if workflowSets[durableVersion] == nil {
		t.Errorf("durableVersion %s names no recorded set, so Launch refuses every server", durableVersion)
	}
}

// A server whose workflows are not the set its version names never launches, and enters no row (#2271).
func TestLaunchRefusesWorkflowsItsVersionDoesNotName(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	recorded(t, "probe-v1", "probe")
	t.Setenv("DBOS__APPVERSION", "probe-v1")
	dc, err := NewDurable(t.Context(), os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbos.Shutdown(dc, 10*time.Second) })
	for name, fn := range probes { // control too: a workflow added under the version
		dbos.RegisterWorkflow(dc, fn, dbos.WithWorkflowName(name))
	}
	if _, err := Launch(ctx, p, dc, schema); err == nil || !strings.Contains(err.Error(), "Bump durableVersion in runs/durable.go and record the new set under it in workflowSets") {
		t.Fatalf("a server registering a workflow its version does not name launched: %v", err)
	}
	if n := 0; p.QueryRow(ctx, "SELECT count(*) FROM durable_executors WHERE id = $1", dc.GetExecutorID()).Scan(&n) != nil || n != 0 {
		t.Fatalf("the refused server entered %d row(s)", n)
	}
}

// A workflow a release adds waits for a server that runs it: a server still on the release before, on
// the version before, never claims it — not while a server of the new release is up, nor once none is —
// and the next server of the new release runs it (#2271).
func TestANewWorkflowWaitsForAServerThatRunsIt(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	older, stopOlder := probeServer(t, ctx, p, schema, "probe-v0", []string{"control"})
	t.Cleanup(stopOlder)
	newer, stopNewer := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"}, "none") // up, and leaving the probe queued
	t.Cleanup(stopNewer)
	if _, err := dbos.Enqueue[any](newer, "probe", "probe", "hello", dbos.WithEnqueueWorkflowID("probe-1")); err != nil {
		t.Fatal(err)
	}
	queued := func(while string) {
		polled(t, ctx, p, schema, older)
		if status, version, executor := workflowRow(t, ctx, p, schema, "probe-1"); status != dbos.WorkflowStatusEnqueued || version != "probe-v1" {
			t.Fatalf("%s, the probe is %s, stamped %s, under %q: the older server took a workflow it has no handler for", while, status, version, executor)
		}
	}
	queued("with a server of its version up")
	stopNewer()
	deadline := time.Now().Add(15 * time.Second)
	for n := 1; n > 0; time.Sleep(20 * time.Millisecond) { // the stopped server's row goes once stale
		if err := p.QueryRow(ctx, "SELECT count(*) FROM durable_executors WHERE id = $1", newer.GetExecutorID()).Scan(&n); err != nil || time.Now().After(deadline) {
			t.Fatalf("the stopped server's row never went stale and went (%v)", err)
		}
	}
	queued("with no server of its version up")
	again, stopAgain := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"})
	t.Cleanup(stopAgain)
	waitRow(t, ctx, p, schema, "probe-1", "ran on the new release's next server", func(status dbos.WorkflowStatusType, _, executor string) bool {
		return status == dbos.WorkflowStatusSuccess && executor == again.GetExecutorID()
	})
}

// Two versions up that both run a workflow never pass it back and forth: the older leaves the newer's
// queued work to the newer, which drains it (#2271).
func TestARunningVersionKeepsItsQueuedWork(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	older, stopOlder := probeServer(t, ctx, p, schema, "probe-v1", []string{"control", "probe"})
	t.Cleanup(stopOlder)
	newer, stopNewer := probeServer(t, ctx, p, schema, "probe-v2", []string{"control", "probe"}, "none")
	t.Cleanup(stopNewer)
	if _, err := dbos.Enqueue[any](newer, "probe", "probe", "hello", dbos.WithEnqueueWorkflowID("probe-2")); err != nil {
		t.Fatal(err)
	}
	polled(t, ctx, p, schema, older)
	if status, version, executor := workflowRow(t, ctx, p, schema, "probe-2"); status != dbos.WorkflowStatusEnqueued || version != "probe-v2" {
		t.Fatalf("with its version up, the newer server's queued probe is %s, stamped %s, under %q: the older server took it over", status, version, executor)
	}
}

// A release rolled back takes a newer one's queued work only once the newer one's row is stale, never at
// its clean stop: a single-replica rollout or a restart would otherwise hand a newer workflow to older
// code at once (#2271).
func TestAStoppedVersionsQueuedWorkMovesOnceItsRowIsStale(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	older, stopOlder := probeServer(t, ctx, p, schema, "probe-v1", []string{"control", "probe"})
	t.Cleanup(stopOlder)
	newer, stopNewer := probeServer(t, ctx, p, schema, "probe-v2", []string{"control", "probe"}, "none")
	t.Cleanup(stopNewer)
	if _, err := dbos.Enqueue[any](newer, "probe", "probe", "hello", dbos.WithEnqueueWorkflowID("probe-3")); err != nil {
		t.Fatal(err)
	}
	stopped := time.Now()
	stopNewer()
	waitRow(t, ctx, p, schema, "probe-3", "moved off the stopped version", func(_ dbos.WorkflowStatusType, version, _ string) bool { return version != "probe-v2" })
	if moved := time.Since(stopped); moved < staleAfter-beatEvery {
		t.Fatalf("the older server took the stopped version's queued probe %s after its clean stop, before its row was stale (%s)", moved, staleAfter)
	}
	waitRow(t, ctx, p, schema, "probe-3", "ran on the older server", func(status dbos.WorkflowStatusType, _, executor string) bool {
		return status == dbos.WorkflowStatusSuccess && executor == older.GetExecutorID()
	})
}

// Recovery and hand-over move only what is still to run: a stopped server's workflow cancelled, failed or
// dead-lettered stays as it ended, under its version, while its PENDING one goes back on its queue.
func TestAStoppedServersEndedWorkflowsStayEnded(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	dc, stop := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"}, "none")
	t.Cleanup(stop)
	ended := map[string]dbos.WorkflowStatusType{"ended-cancelled": dbos.WorkflowStatusCancelled, "ended-error": dbos.WorkflowStatusError, "ended-dead": dbos.WorkflowStatusMaxRecoveryAttemptsExceeded, "ended-pending": dbos.WorkflowStatusPending}
	for id, status := range ended {
		if _, err := dbos.Enqueue[any](dc, "probe", "probe", "hello", dbos.WithEnqueueWorkflowID(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, "UPDATE "+schema+".workflow_status SET status = $2, executor_id = 'gone', application_version = 'probe-gone' WHERE workflow_uuid = $1", id, status); err != nil {
			t.Fatal(err)
		}
	}
	waitRow(t, ctx, p, schema, "ended-pending", "went back on its queue, handed over", func(status dbos.WorkflowStatusType, version, _ string) bool {
		return status == dbos.WorkflowStatusEnqueued && version == "probe-v1"
	})
	beats(t, ctx, p, dc, 2)
	for id, want := range ended {
		if status, version, executor := workflowRow(t, ctx, p, schema, id); want != dbos.WorkflowStatusPending && (status != want || version != "probe-gone" || executor != "gone") {
			t.Errorf("%s: %s, stamped %s, under %q; it ended %s under the stopped server", id, status, version, executor, want)
		}
	}
}

// A rollback: a server of the version before finishes the run a newer one was driving when it was
// killed. The run goes back on its queue stamped with the newer version, and once no server of that
// version is up, the older one, which runs the gate-run workflow too, takes it over (#2271).
func TestAnOlderServerFinishesAKilledNewerServersRun(t *testing.T) {
	p, ctx, schema, building := twoServers(t)
	older, stopOlder := serverOn(t, ctx, p, schema, "runs-v1", "none") // up first, taking nothing, and leaving the run to the newer
	t.Cleanup(stopOlder)
	life, kill := context.WithCancel(ctx)
	defer kill()
	newer, _ := serverOn(t, life, p, schema, "runs-v2")
	run, err := Start(ctx, p, newer, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	building()
	drivenBy(t, ctx, p, schema, run.ID, newer)
	older.ListenQueues(older)                                    // every queue again
	kill()                                                       // its row stays behind
	if err := dbos.Shutdown(newer, 10*time.Second); err != nil { // and its runs stop where they were, unrecorded
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the newer server's run, on the older: %s %s", state, why(reason))
	}
	drivenBy(t, ctx, p, schema, run.ID, older)
	if kinds := strings.Join(kinds(t, ctx, p, run.ID), ","); strings.Count(kinds, "deploy.completed") != 1 {
		t.Fatalf("one deploy recorded: %s", kinds)
	}
}

// A server restarting under the executor id of one stopped, which only DBOS__VMID can give it, takes that
// row over; one sharing the id of a server still running never launches (#2271).
func TestLaunchTakesOverOnlyAStoppedRowOfItsID(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	t.Setenv("DBOS__VMID", fmt.Sprintf("vm-%d", time.Now().UnixNano()))
	_, stopFirst := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"})
	t.Cleanup(stopFirst)
	stopFirst()
	again, stopAgain := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"}) // the stopped server's row, taken over
	t.Cleanup(stopAgain)
	var stopped bool
	if err := p.QueryRow(ctx, "SELECT stopped_at IS NOT NULL FROM durable_executors WHERE id = $1", again.GetExecutorID()).Scan(&stopped); err != nil || stopped {
		t.Fatalf("the restarted server's row: stopped %v (%v); want running", stopped, err)
	}
	twin, err := NewDurable(t.Context(), os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbos.Shutdown(twin, 10*time.Second) })
	dbos.RegisterWorkflow(twin, probeWorkflow, dbos.WithWorkflowName("probe"))
	if twin.GetExecutorID() != again.GetExecutorID() {
		t.Fatalf("DBOS__VMID gave the twin %q, the running server %q", twin.GetExecutorID(), again.GetExecutorID())
	}
	if _, err := Launch(ctx, p, twin, schema); err == nil || !strings.Contains(err.Error(), "two processes never share one") {
		t.Fatalf("a server sharing a running server's executor id launched: %v", err)
	}
}

// A server launching under the executor id of one stopped puts that one's journaled workflows back on
// their queues, whatever their version, each keeping its own: no one else will, its id being live again
// and DBOS's own recovery taking only the new version's (#2271).
func TestLaunchUnderAStoppedIDPutsItsWorkflowsBack(t *testing.T) {
	p, ctx, schema, _ := twoServers(t)
	enqueuer, stopEnqueuer := probeServer(t, ctx, p, schema, "probe-v0", []string{"control"}, "none")
	t.Cleanup(stopEnqueuer)
	if _, err := dbos.Enqueue[any](enqueuer, "probe", "control", "hello", dbos.WithEnqueueWorkflowID("taken-1")); err != nil {
		t.Fatal(err)
	}
	stopEnqueuer() // no server up to recover what follows
	id := fmt.Sprintf("vm-%d", time.Now().UnixNano())
	if _, err := p.Exec(ctx, "UPDATE "+schema+".workflow_status SET status = $2, executor_id = $3 WHERE workflow_uuid = $1", "taken-1", dbos.WorkflowStatusPending, id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO durable_executors (id, version, stopped_at) VALUES ($1, 'probe-v0', now())", id); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBOS__VMID", id)
	_, stop := probeServer(t, ctx, p, schema, "probe-v1", []string{"probe"}, "none")
	t.Cleanup(stop)
	if status, version, executor := workflowRow(t, ctx, p, schema, "taken-1"); status != dbos.WorkflowStatusEnqueued || version != "probe-v0" {
		t.Fatalf("the stopped server's workflow, once its id launched again: %s, stamped %s, under %q; want back on its queue, stamped probe-v0", status, version, executor)
	}
}
