package runs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lee takes the environment offline in these tests.
var lee = events.Actor{Kind: "person", Subject: "lee@meridian.example", Display: "Lee Okafor"}

// brakeAs is an undeploy's first step as the route takes it, committed on its own (contracts/routes.md, The brake).
func brakeAs(t *testing.T, ctx context.Context, p *pgxpool.Pool, env string) Braked {
	t.Helper()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := BrakeTx(ctx, tx, project, env, lee, false)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requested counts the request undeploy rows on the trail.
func requested(t *testing.T, ctx context.Context, p *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 'request undeploy'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// previewImage is the deploy run of a clean preview deploy, and the image a promotion ships.
func previewImage(t *testing.T, ctx context.Context, p *pgxpool.Pool) (deployRun, image string) {
	t.Helper()
	deployRun, state, reason := deployOnce(t, ctx, p)
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	return deployRun, image
}

// holdRun parks every /run until the returned release, called once whatever happens, or at cleanup.
func holdRun(t *testing.T, f *executortest.Fake) (release func()) {
	t.Helper()
	held := make(chan struct{})
	f.HoldRun.Store(&held)
	var once sync.Once
	release = func() { once.Do(func() { f.HoldRun.Store(nil); close(held) }) }
	t.Cleanup(release)
	return release
}

// midSwitch starts in and answers it once it reaches /run, held there: its deploy step begun, its row unwritten.
func midSwitch(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake, in StartInput) (run string, release func()) {
	t.Helper()
	ran := f.Runs.Load()
	release = holdRun(t, f)
	started, err := Start(ctx, p, durable, in)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); f.Runs.Load() == ran; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never reached /run")
		}
	}
	return started.ID, release
}

// A promotion still pending when production is taken offline is ended there and then, interrupted,
// and the trail names the run the undeploy ended.
func TestUndeployProdCancelsAPendingPromotion(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	held := make(chan struct{})
	var once sync.Once
	pipe.hold.Store(&held)
	t.Cleanup(func() { pipe.hold.Store(nil); once.Do(func() { close(held) }) })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	if b := brakeAs(t, ctx, p, "preview"); b.Run != nil {
		t.Fatalf("a preview undeploy met the promotion: %+v", b)
	}
	b := brakeAs(t, ctx, p, "prod")
	Cancel(durable, b.Ended)
	once.Do(func() { close(held) })
	if b.Run == nil || b.Run.ID != run.ID || b.Ended != run.ID || b.Stopping {
		t.Fatalf("the brake on a pending promotion: %+v", b)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("the pending promotion: %s %s", state, why(reason))
	}
	var detail string
	if err := p.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'request undeploy' AND actor_subject = $1 AND run_id = $2", lee.Subject, run.ID).Scan(&detail); err != nil || detail != `{"env": "prod", "run": "`+run.ID+`"}` {
		t.Fatalf("the request undeploy row: %q (%v)", detail, err)
	}
}

// A promotion switching production when the undeploy lands — its deploy step begun, its row not yet
// written — carries the stop out itself once the switch lands: the container stops, production leaves
// envs, nothing says it went live, and the undeploy's audit row names who asked. Asking again while
// the stop is pending answers the same and writes nothing.
func TestStopMidSwitchStopsAfterIt(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	run, release := midSwitch(t, ctx, p, f, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	b := brakeAs(t, ctx, p, "prod")
	if b.Run == nil || b.Run.ID != run || !b.Stopping || b.Ended != "" || b.Asked {
		t.Fatalf("the brake mid-switch: %+v", b)
	}
	if again := brakeAs(t, ctx, p, "prod"); !again.Stopping || requested(t, ctx, p) != 1 {
		t.Fatalf("asked again: %+v, %d request rows", again, requested(t, ctx, p))
	}
	release()
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("the promotion that met the brake: %s %s", state, why(reason))
	}
	if got := f.Stopped(); f.Stops.Load() != 1 || got.Target != "prod" {
		t.Fatalf("the run stopped %+v, %d time(s)", got, f.Stops.Load())
	}
	var rows, live, deployed int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM deploys WHERE env = 'prod'), (SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'deploy.completed'),
		(SELECT count(*) FROM audit_events WHERE action = 'deploy prod')`, run).Scan(&rows, &live, &deployed); err != nil || rows+live+deployed != 0 {
		t.Fatalf("production recorded: %d rows, %d deploy.completed, %d deploy prod (%v)", rows, live, deployed, err)
	}
	if line := lastLine(t, ctx, p, run); line != "Taken offline while deploying, as Lee Okafor asked: Canyon stopped production" {
		t.Fatalf("the run's last line: %q", line)
	}
	var detail string
	if err := p.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = $1 AND actor_subject = $2 AND run_id = $3", UndeployAction, lee.Subject, run).Scan(&detail); err != nil ||
		detail != `{"env": "prod", "run": "`+run+`", "package": "`+pkg+`"}` {
		t.Fatalf("the undeploy's audit row: %q (%v)", detail, err)
	}
}

// However a switch the brake met ends — a preview deploy that lands, a container that did not stay up,
// a /run that failed — the run stops the environment before it ends, interrupted, and nothing of it is
// recorded as live.
func TestAStopMidSwitchIsCarriedOutHoweverTheSwitchEnds(t *testing.T) {
	for _, c := range []struct {
		name, env string
		fault     func(f *executortest.Fake)
	}{
		{"a preview deploy that lands", "preview", func(*executortest.Fake) {}},
		{"a container that did not stay up", "preview", func(f *executortest.Fake) {
			f.Exited = &executor.Exited{Reason: "the app did not stay up: its container is exited"}
		}},
		{"a /run that failed", "prod", func(f *executortest.Fake) { f.FailRun = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f := deploying(t, runstest.CleanGolden)
			deployRun, image := previewImage(t, ctx, p)
			in := StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana}
			if c.env == "prod" {
				in = StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}}
			}
			run, release := midSwitch(t, ctx, p, f, in)
			if b := brakeAs(t, ctx, p, c.env); !b.Stopping {
				t.Fatalf("the brake mid-switch: %+v", b)
			}
			c.fault(f)
			release()
			if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "interrupted" {
				t.Fatalf("the run: %s %s", state, why(reason))
			}
			if got := f.Stopped(); f.Stops.Load() != 1 || got.Target != c.env {
				t.Fatalf("the run stopped %+v, %d time(s)", got, f.Stops.Load())
			}
			if n := countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = '"+c.env+"'") + countOf(t, ctx, p, "SELECT count(*) FROM timeline_rows WHERE run_id = '"+run+"' AND kind = 'deploy.completed'"); n != 0 {
				t.Fatalf("%s still recorded, or recorded live: %d", c.env, n)
			}
		})
	}
}

// A deploy step run again with a stop recorded on the run — a replay after a crash — records what runs,
// so a stop that fails leaves no container unrecorded, and nothing that says it went live.
func TestAReplayedDeployStepWithAStopRecordsOnlyWhatRuns(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	held := make(chan struct{})
	var once sync.Once
	pipe.hold.Store(&held)
	t.Cleanup(func() { pipe.hold.Store(nil); once.Do(func() { close(held) }) })
	promotion := &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: promotion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET deploy_started_at = now(), stop_requested_at = now(), stop_requested_by_kind = 'person', stop_requested_by_subject = $2 WHERE id = $1", run.ID, lee.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.deploy(ctx, Input{Run: run.ID, Project: project, Package: pkg, Actor: dana, Promote: promotion}, "prod", image, built{}); err != nil {
		t.Fatal(err)
	}
	if f.Runs.Load() == 0 || countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = 'prod' AND image = '"+image+"'") != 1 {
		t.Fatal("the row saying what runs in production was not written")
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM timeline_rows WHERE run_id = '"+run.ID+"' AND (kind = 'deploy.completed' OR dedupe = 'live:prod')") + countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = 'deploy prod'"); n != 0 {
		t.Fatalf("the deploy step said production went live: %d rows", n)
	}
}

// A stop recorded on a run as it reads whether one was, on its way to its end, is never lost between
// the two: a run already ending is left to the undeploy, which records nothing on it and stops what is
// there itself.
func TestAStopAfterTheRunReadItIsLeftToTheUndeploy(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at) VALUES ('gr_ending00000', $1, $2, 'running', 'promote', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	if ask, err := pipe.ending(ctx, "gr_ending00000"); err != nil || ask.Asked {
		t.Fatalf("the run's read on its way to its end: %+v (%v)", ask, err)
	}
	b := brakeAs(t, ctx, p, "prod")
	if b.Run == nil || b.Stopping || countOf(t, ctx, p, "SELECT count(*) FROM runs WHERE stop_requested_at IS NOT NULL") != 0 || requested(t, ctx, p) != 0 {
		t.Fatalf("a stop was recorded on a run already ending: %+v", b)
	}
}

// The server-start reconcile ending a run DBOS gave up on carries out the stop recorded on it first.
func TestTheReconcileCarriesOutAStopRecordedOnARunItEnds(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	previewImage(t, ctx, p)
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at, stop_requested_at, stop_requested_by_kind, stop_requested_by_subject, stop_requested_by_display)
		VALUES ('gr_givenup0000', $1, $2, 'running', 'promote', now(), now(), 'person', $3, $4)`, project, pkg, lee.Subject, lee.Display); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, url, gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	f.Start(project, "prod")
	if _, err := pipe.Reconcile(ctx, durable); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, "gr_givenup0000"); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("the run the reconcile ended: %s %s", state, why(reason))
	}
	if got := f.Stopped(); f.Stops.Load() != 1 || got.Target != "prod" || countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 0 {
		t.Fatalf("production was not stopped: %+v, %d stops", got, f.Stops.Load())
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = '"+UndeployAction+"' AND actor_subject = '"+lee.Subject+"' AND run_id = 'gr_givenup0000'"); n != 1 {
		t.Fatalf("the undeploy's audit row: %d", n)
	}
}

// A deploy run asked to end before its deploy step — its build waiting its turn when the undeploy lands —
// never deploys.
func TestADeployRunAskedBeforeItsDeployStepNeverDeploys(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	previewImage(t, ctx, p)
	builder := "sha256:" + strings.Repeat("d", 64) // a new builder: the run builds rather than reuse preview's image
	f.Builder.Store(&builder)
	f.BuildBusy.Store(1000)
	builds, ran := f.Builds.Load(), f.Runs.Load()
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); f.Builds.Load() == builds; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the deploy run never asked to build")
		}
	}
	if b := brakeAs(t, ctx, p, "preview"); !b.Asked || b.Stopping {
		t.Fatalf("the brake before the deploy step: %+v", b)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "interrupted" || f.Runs.Load() != ran {
		t.Fatalf("the asked run: %s %s, %d containers started", state, why(reason), f.Runs.Load()-ran)
	}
}

// A stop that the run cannot carry out past its retries leaves the row saying what runs there: the run
// ends failed/error in deploy, and its line says so.
func TestAStopThatFailsLeavesTheRowSayingWhatRuns(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	was := stopRetries
	stopRetries = 1
	t.Cleanup(func() { stopRetries = was })
	run, release := midSwitch(t, ctx, p, f, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	brakeAs(t, ctx, p, "prod")
	f.FailStop.Store(true)
	release()
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "error" {
		t.Fatalf("a stop that failed: %s %s", state, why(reason))
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = 'prod' AND image = '"+image+"'"); n != 1 {
		t.Fatalf("production's row: %d", n)
	}
	want := "Taken offline while deploying, as Lee Okafor asked, but Canyon could not stop production: " + pkg[4:] + " runs there. Take it offline again with canyon undeploy --env prod"
	if line := lastLine(t, ctx, p, run); line != want {
		t.Fatalf("the run's last line: %q", line)
	}
}

// countOf is one count.
func countOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sendRemove is the undeploy's remove under run_by: org, committed on its own.
func sendRemove(t *testing.T, ctx context.Context, p *pgxpool.Pool) string {
	t.Helper()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	remove, sent, err := SendRemoveTx(ctx, tx, durable, project, "preview")
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil || !sent {
		t.Fatalf("the remove: %q %v (%v)", remove, sent, err)
	}
	t.Cleanup(func() { // its send, a workflow of its own, ends before the test's timing is put back
		for deadline := time.Now().Add(20 * time.Second); countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE id = '"+remove+"' AND state = 'sent'") == 1; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the remove's send never ended")
			}
		}
	})
	return remove
}

// Under run_by: org, a deploy waiting on the org's report ends at once when an undeploy's remove
// supersedes its release — never waiting out the bound — and the remove names the open release, the
// org's first deploy there having no row.
func TestAnUndeploysRemoveEndsTheRunWaitingOnItsRelease(t *testing.T) {
	p, ctx, run, release := waitingOn(t, time.Minute) // a bound the superseded run must never wait out
	b := brakeAs(t, ctx, p, "preview")
	if b.Run == nil || b.Run.ID != run || b.Stopping {
		t.Fatalf("the brake on a run waiting on its report: %+v", b)
	}
	remove := sendRemove(t, ctx, p)
	if state, reason := waitEndedWithin(t, ctx, p, run, 10*time.Second); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("the superseded run: %s %s", state, why(reason))
	}
	if line := lastLine(t, ctx, p, run); line != "Taken offline while your platform was deploying, as Lee Okafor asked: Canyon sent it a remove" {
		t.Fatalf("the run's last line: %q", line)
	}
	var named bool
	if err := p.QueryRow(ctx, "SELECT r.package_id = d.package_id AND r.image = d.image FROM releases r, releases d WHERE r.id = $1 AND d.id = $2", remove, release).Scan(&named); err != nil || !named {
		t.Fatalf("the remove names the open release's package and image: %v (%v)", named, err)
	}
}

// A run still delivering its release when an undeploy's remove supersedes it stops retrying: no later
// attempt may put the app back, and the run ends through the brake.
func TestARunDeliveringASupersededReleaseStopsRetrying(t *testing.T) {
	platform := newStubPlatform(t, 500)
	p, ctx := releasing(t, "wh_org_pipeline_secret_20", platform) //nolint:gosec // a test fixture, not a real secret
	started, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(platform.requests()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the release was never delivered")
		}
	}
	deliveries := func() int {
		n := 0
		for _, r := range platform.requests() {
			if strings.Contains(string(r.Body), `"event":"deploy"`) {
				n++
			}
		}
		return n
	}
	brakeAs(t, ctx, p, "preview")
	sendRemove(t, ctx, p)
	before := deliveries()
	// the next retry is at most releaseFastRetry away; the hour's bound (releaseCeiling here) is further
	if state, reason := waitEndedWithin(t, ctx, p, started.ID, releaseFastRetry+time.Second); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("the run delivering a superseded release: %s %s", state, why(reason))
	}
	if after := deliveries(); after > before+1 {
		t.Fatalf("the release was delivered again after the remove: %d, then %d", before, after)
	}
}

// The record of a stop the run carried out is written once: a replay of it writes no second audit row.
func TestARecordedStopIsRecordedOnce(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	previewImage(t, ctx, p)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at) VALUES ('gr_replayed000', $1, $2, 'running', 'promote', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	in := Input{Run: "gr_replayed000", Project: project, Package: pkg, Actor: dana, Promote: &Promotion{}}
	for range 2 { // the record, then a replay of it
		if err := pipe.stopped(ctx, durable, in, "prod", stopAsk{Asked: true, By: lee}, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = '"+UndeployAction+"'"); n != 1 {
		t.Fatalf("the undeploy's audit rows: %d", n)
	}
}

// An org run stopped before its release is sent sends none; its end removes what the org runs there,
// names who asked once, and a replay of that record writes nothing more.
func TestAnOrgRunStoppedBeforeItsReleaseSendsNone(t *testing.T) {
	p, ctx := releasing(t, "wh_org_pipeline_secret_21", newStubPlatform(t, 200)) //nolint:gosec // a test fixture, not a real secret
	image := "registry.example/canyon/claims-portal@sha256:" + strings.Repeat("c", 64)
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state, kind, deploys, deploy_started_at, stop_requested_at, stop_requested_by_kind, stop_requested_by_subject, stop_requested_by_display) VALUES ('gr_orgstop0000', '" + project + "', '" + pkg + "', 'running', 'gate', true, now(), now(), 'person', '" + lee.Subject + "', '" + lee.Display + "')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('" + project + "', 'preview', '" + pkg + "', '" + image + "', 'https://claims.apps.meridian.example', 'gr_orgstop0000', 'org')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{Run: "gr_orgstop0000", Project: project, Package: pkg, Deploy: true, Actor: dana}
	if rel, err := pipe.createRelease(ctx, in, "preview", image, "webhook"); err != nil || !rel.Stopped || countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE run_id = 'gr_orgstop0000' AND event = 'deploy'") != 0 {
		t.Fatalf("the release step with a stop recorded: %+v (%v)", rel, err)
	}
	ask := stopAsk{Asked: true, By: lee}
	for range 2 { // the record, then a replay of it
		if err := pipe.stopped(ctx, durable, in, "preview", ask, true); err != nil {
			t.Fatal(err)
		}
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'preview'"); n != 1 || countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = '"+UndeployAction+"'") != 1 {
		t.Fatalf("removes %d, or not one audit row", n)
	}
	if line := lastLine(t, ctx, p, "gr_orgstop0000"); line != "Taken offline before the release was sent, as Lee Okafor asked: nothing was handed to your platform" {
		t.Fatalf("the run's last line: %q", line)
	}
}

// A run's record of its stop holds one connection: with the pool's one taken by it, a read on a second —
// under the ledger lock, every other writer waiting — would never come.
func TestARecordedStopNeedsOneConnection(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	previewImage(t, ctx, p)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at) VALUES ('gr_oneconn0000', $1, $2, 'running', 'promote', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	cfg := p.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	was := pipe.Pool
	pipe.Pool = one
	t.Cleanup(func() { pipe.Pool = was })
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in := Input{Run: "gr_oneconn0000", Project: project, Package: pkg, Actor: dana, Promote: &Promotion{}}
	if err := pipe.stopped(bounded, durable, in, "prod", stopAsk{Asked: true, By: lee}, true); err != nil {
		t.Fatalf("the record on a pool of one: %v", err)
	}
}

// A run stopped before its release is sent, while an earlier run's release of the environment is still
// open — its wait over, nothing reported, no row — sends the remove that supersedes it, as the undeploy
// route would: a late deployed report on it can never put the environment back.
func TestAStopRemovesAnEarlierRunsOpenRelease(t *testing.T) {
	p, ctx := releasing(t, "wh_org_pipeline_secret_22", newStubPlatform(t, 200)) //nolint:gosec // a test fixture, not a real secret
	image := "registry.example/canyon/claims-portal@sha256:" + strings.Repeat("e", 64)
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state, failure_reason, ended_at) VALUES ('gr_earlier000', '" + project + "', '" + pkg + "', 'failed', 'error', now())",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_earlier00', '" + project + "', 'preview', 'gr_earlier000', '" + pkg + "', 'deploy', '" + image + "', '8f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4e', 'accepted', now())",
		"INSERT INTO runs (id, project_id, package_id, state, kind, deploys, deploy_started_at, stop_requested_at, stop_requested_by_kind, stop_requested_by_subject, stop_requested_by_display) VALUES ('gr_later00000', '" + project + "', '" + pkg + "', 'running', 'gate', true, now(), now(), 'person', '" + lee.Subject + "', '" + lee.Display + "')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{Run: "gr_later00000", Project: project, Package: pkg, Deploy: true, Actor: dana}
	if err := pipe.stopped(ctx, durable, in, "preview", stopAsk{Asked: true, By: lee}, true); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'preview' AND image = '"+image+"'"); n != 1 || countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = '"+UndeployAction+"'") != 1 {
		t.Fatalf("removes of the earlier release: %d, or not one audit row", n)
	}
}
