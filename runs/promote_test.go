package runs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// previewedWithDB is the package declaring db = postgres deployed to preview, prod's driver d:
// the deploy run and the image a promotion ships.
func previewedWithDB(t *testing.T, d *answers) (p *pgxpool.Pool, ctx context.Context, f *executortest.Fake, deployRun, image string) {
	t.Helper()
	p, ctx, f = declaringDB(t, ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		return map[string]string{"DATABASE_URL": "value-preview-db"}, nil
	}))
	d.f, pipe.Drivers[[2]string{"postgres", "prod"}] = f, d
	was := pendingEvery
	pendingEvery = 10 * time.Millisecond
	t.Cleanup(func() { pendingEvery = was })
	deployRun, state, reason := deployOnce(t, ctx, p)
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	return p, ctx, f, deployRun, image
}

func promote(t *testing.T, ctx context.Context, p *pgxpool.Pool, deployRun, image string) (id, state string, reason *string) {
	t.Helper()
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	state, reason = waitEnded(t, ctx, p, run.ID)
	return run.ID, state, reason
}

var prodReady = provisioners.Result{State: provisioners.Ready, Outputs: map[string]string{"DATABASE_URL": "value-prod-db"}}

// A database the platform is still creating holds the promotion: no prod container until the driver
// answers ready, one waiting line however often it was asked, then prod starts on its own database.
// A replay of the steps neither asks the driver nor starts a container again.
func TestAPromotionWaitsForAPendingResourceBeforeTouchingProd(t *testing.T) {
	d := &answers{results: []provisioners.Result{{State: provisioners.Pending}, {State: provisioners.Pending}, prodReady}}
	p, ctx, f, deployRun, image := previewedWithDB(t, d)
	run, state, reason := promote(t, ctx, p, deployRun, image)
	if state != "clean" || d.calls.Load() != 3 || d.started.Load() != 1 {
		t.Fatalf("a promotion past a pending database: %s %s, %d asks, %d containers by the last ask", state, reasonOf(reason), d.calls.Load(), d.started.Load())
	}
	var got string
	if err := p.QueryRow(ctx, "SELECT string_agg(data->>'line', '|' ORDER BY id) FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log'", run).Scan(&got); err != nil ||
		got != "postgres db: creating|postgres db: waiting on the platform|postgres db: ready|deploying to production|live at http://claims-portal.prod.localhost:7777" {
		t.Fatalf("the timeline: %s (%v)", got, err)
	}
	if r := f.Ran(); r.Target != "prod" || r.Image != image || r.Env["DATABASE_URL"] != "value-prod-db" || f.Runs.Load() != 2 {
		t.Fatalf("prod runs preview's image on its own database: %+v, %d containers", r, f.Runs.Load())
	}
	var promoted bool
	if err := p.QueryRow(ctx, "SELECT gate_run = $1 AND promoted_by_subject = $2 AND deployed_by_subject = $2 FROM deploys WHERE env = 'prod'", deployRun, dana.Subject).Scan(&promoted); err != nil || !promoted {
		t.Fatalf("the prod row repeats preview's and names who promoted: %v (%v)", promoted, err)
	}
	// the steps again, as a recovery after a crash would run them: the ready record and the recorded row answer
	in := Input{Run: run, Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}}
	if res, err := pipe.ensure(ctx, in, "prod", nil, nil); err != nil || res.Org == nil || *res.Org || res != (ensured{Org: res.Org, Via: "webhook"}) || d.calls.Load() != 3 {
		t.Fatalf("the replayed ensure: %+v %v, %d asks", res, err, d.calls.Load())
	}
	var once bool
	url, err := pipe.deploy(ctx, in, "prod", image, built{})
	if qerr := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM timeline_rows WHERE kind = 'deploy.completed' AND data->>'env' = 'prod') = 1 AND (SELECT count(*) FROM audit_events WHERE action = 'deploy prod') = 1").Scan(&once); qerr != nil ||
		err != nil || url != "http://claims-portal.prod.localhost:7777" || f.Runs.Load() != 2 || !once {
		t.Fatalf("the replayed deploy: %q %v, %d containers, recorded once %v (%v)", url, err, f.Runs.Load(), once, qerr)
	}
	// a crash between the container's start and its record: the step again finds prod running the
	// image and records it, starting nothing (a promotion that is not a retry always starts it)
	held, held2 := make(chan struct{}), make(chan struct{})
	pipe.hold.Store(&held)
	again, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: in.Promote})
	if err != nil {
		t.Fatal(err)
	}
	in.Run = again.ID
	if err := pipe.record(ctx, in, provisioners.Key{}, "", "", "", "", "deploying:prod", "deploying to production"); err != nil { // the first attempt got this far
		t.Fatal(err)
	}
	_, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	if state, _ := waitEnded(t, ctx, p, again.ID); err != nil || state != "clean" || f.Runs.Load() != 2 {
		t.Fatalf("the step after a crash: %v, %s, %d containers", err, state, f.Runs.Load())
	}
	// the same retry while the executor cannot tell what runs (contracts/executor.md, /running 502):
	// the step fails and starts nothing — a guess of "nothing running" would start prod a second time
	f.FailRunning.Store(true)
	pipe.hold.Store(&held2)
	third, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: in.Promote})
	if err != nil {
		t.Fatal(err)
	}
	in.Run = third.ID
	if err := pipe.record(ctx, in, provisioners.Key{}, "", "", "", "", "deploying:prod", "deploying to production"); err != nil {
		t.Fatal(err)
	}
	_, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held2)
	pipe.hold.Store(nil)
	state, _ = waitEnded(t, ctx, p, third.ID) // its own attempt meets the same answer
	if want := "deploying to production: failed: Canyon could not tell whether this version is already running; nothing was started"; err == nil || state != "failed" || f.Runs.Load() != 2 || lastLine(t, ctx, p, third.ID) != want {
		t.Fatalf("a retry the executor cannot answer: %v, %s, %d containers, last line %q", err, state, f.Runs.Load(), lastLine(t, ctx, p, third.ID))
	}
}

// A retry rebuilds prod's URL from the front door the first attempt ran under, never one an admin
// changed meanwhile: a running app keeps the front door it was deployed with (contracts/executor.md,
// The front door).
func TestARetriedProdDeployKeepsTheFrontDoorItStartedWith(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"old.meridian.example","form":"app.env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the preview deploy: %s %s", state, reasonOf(reason))
	}
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil {
		t.Fatal(err)
	}
	firstRun, state, reason := promote(t, ctx, p, deployRun, image)
	if state != "clean" {
		t.Fatalf("the first promotion: %s %s", state, reasonOf(reason))
	}
	const firstURL = "https://claims-portal.prod.old.meridian.example"
	var url string
	if err := p.QueryRow(ctx, "SELECT url FROM deploys WHERE env = 'prod'").Scan(&url); err != nil || url != firstURL {
		t.Fatalf("the first attempt's url: %q (%v)", url, err)
	}
	// the write side: the first attempt itself recorded the door it ran under, in its own
	// deploying:prod row, not only the fixture the retry below plants for a new run.
	var storedDomain, storedForm string
	if err := p.QueryRow(ctx, "SELECT data->>'domain', data->>'form' FROM timeline_rows WHERE run_id = $1 AND dedupe = 'deploying:prod'", firstRun).Scan(&storedDomain, &storedForm); err != nil ||
		storedDomain != "old.meridian.example" || storedForm != "app.env.domain" {
		t.Fatalf("the first attempt did not store the door it ran under: domain %q form %q (%v)", storedDomain, storedForm, err)
	}
	if _, err := p.Exec(ctx, `UPDATE front_doors SET door = jsonb_set(door, '{domain}', '"new.meridian.example"')`); err != nil { // an admin changes it before the retry
		t.Fatal(err)
	}
	// a crash between the container's start and its record, as the existing crash-retry test does:
	// a new run of the same promotion, whose own first attempt (simulated here) reached the
	// container and recorded the door it ran under — the old one.
	held := make(chan struct{})
	pipe.hold.Store(&held)
	again, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Run: again.ID, Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}}
	if err := pipe.write(ctx, func(tx pgx.Tx) error {
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "deploying:prod", map[string]any{"line": "deploying to production", "domain": "old.meridian.example", "form": "app.env.domain"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	url, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	if state, _ = waitEnded(t, ctx, p, again.ID); err != nil || state != "clean" || url != firstURL || f.Runs.Load() != 2 {
		t.Fatalf("a retry after the front door changed: %q %v, %s, %d containers, want %q and 2", url, err, state, f.Runs.Load(), firstURL)
	}
}

// A retry whose recorded deploying:prod row carries a domain but no form (form is SQL NULL) builds
// the URL from the domain alone, as appURL's default form does, instead of a nil-pointer dereference
// on *form.
func TestARetryWithNoFormInTheRecordedDoorBuildsFromDomainAlone(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"old.meridian.example","form":"app.env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the preview deploy: %s %s", state, reasonOf(reason))
	}
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil {
		t.Fatal(err)
	}
	if _, state, reason = promote(t, ctx, p, deployRun, image); state != "clean" {
		t.Fatalf("the first promotion: %s %s", state, reasonOf(reason))
	}
	held := make(chan struct{})
	pipe.hold.Store(&held)
	again, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Run: again.ID, Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}}
	if err := pipe.write(ctx, func(tx pgx.Tx) error { // domain, no form: what a nil-form retry sees
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "deploying:prod", map[string]any{"line": "deploying to production", "domain": "old.meridian.example"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	url, err := pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	const want = "https://claims-portal.prod.old.meridian.example"
	if state, _ = waitEnded(t, ctx, p, again.ID); err != nil || state != "clean" || url != want || f.Runs.Load() != 2 {
		t.Fatalf("a retry with no form recorded: %q %v, %s, %d containers, want %q and 2", url, err, state, f.Runs.Load(), want)
	}
}

// A first attempt whose front-door read fails (the door existed when the run started but is gone by
// the time deploy's prod step reads it — the doorSet check earlier in the run only proves a door
// existed then) writes the same prod failure line every other prod failure does, instead of ending
// with neither an opening nor a failure line on the timeline. Calls deploy() directly, as the crash-
// retry cases above do, so nothing else of the run's own workflow races this row.
func TestAFirstAttemptWhoseFrontDoorReadFailsRecordsTheFailureLine(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	onKubernetes(t) // p.Access set, no front_doors row: p.frontDoor errors "this Kubernetes host has no front door"
	const run = "gr_noford001"
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ($1, $2, $3, 'running')", run, project, pkg); err != nil {
		t.Fatal(err)
	}
	image := "canyon/claims-portal@sha256:" + strings.Repeat("a", 64)
	in := Input{Run: run, Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: "gr_upstream0", DeployedBy: dana}}
	_, deployErr := pipe.deploy(ctx, in, "prod", image, built{})
	const want = "deploying to production: failed: runs: this Kubernetes host has no front door; the version running there is unchanged"
	if deployErr == nil || lastLine(t, ctx, p, run) != want {
		t.Fatalf("a first attempt with no front door: %v, last line %q, want %q", deployErr, lastLine(t, ctx, p, run), want)
	}
}

// A promotion that fails ends before prod changes: the version running there keeps running, its row
// is as it was, and the reason is the last line. A prod database that failed starts nothing; a
// container that did not start leaves the executor to keep the old one.
func TestAFailedPromotionLeavesProdAsItWas(t *testing.T) {
	d := &answers{results: []provisioners.Result{{State: provisioners.Failed, Message: "quota exceeded"}}}
	p, ctx, f, deployRun, image := previewedWithDB(t, d)
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) VALUES ($1, 'prod', $2, 'canyon/claims-portal@sha256:old', 'http://old', $3)", project, pkg, deployRun); err != nil {
		t.Fatal(err)
	}
	unchanged := func(run, want string) {
		t.Helper()
		var url string
		var audits int
		if err := p.QueryRow(ctx, "SELECT url, (SELECT count(*) FROM audit_events WHERE action = 'deploy prod') FROM deploys WHERE env = 'prod'").Scan(&url, &audits); err != nil || url != "http://old" || audits != 0 || f.Stops.Load() != 0 {
			t.Fatalf("prod changed: %q, %d audit rows, %d stops (%v)", url, audits, f.Stops.Load(), err)
		}
		if got := lastLine(t, ctx, p, run); got != want {
			t.Fatalf("the last line:\n got %s\nwant %s", got, want)
		}
	}
	run, state, reason := promote(t, ctx, p, deployRun, image)
	if state != "failed" || reasonOf(reason) != "error" || f.Runs.Load() != 1 {
		t.Fatalf("a failed prod database: %s %s, %d containers", state, reasonOf(reason), f.Runs.Load())
	}
	unchanged(run, "postgres db: failed: quota exceeded")
	d.results, f.FailRun = []provisioners.Result{prodReady}, true
	run, state, _ = promote(t, ctx, p, deployRun, image)
	if state != "failed" {
		t.Fatalf("a container that did not start: %s", state)
	}
	unchanged(run, "deploying to production: failed: docker run: exit status 125; the version running there is unchanged") // the executor's own words (#2539)
}

// A promotion cancelled while the platform makes its database ends interrupted within one more ask:
// prod untouched, no deploy begun, and the record stays, so the next promotion carries on with it.
func TestACancelDuringAPendingResourceEndsThePromotionBeforeProd(t *testing.T) {
	d := &answers{results: []provisioners.Result{{State: provisioners.Pending}}}
	p, ctx, f, deployRun, image := previewedWithDB(t, d)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting bool
		if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'waiting:prod:db')", run.ID).Scan(&waiting); err != nil || waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the promotion never waited on the platform")
		}
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", run.ID); err != nil {
		t.Fatal(err)
	}
	asked := d.calls.Load()
	state, reason := waitEnded(t, ctx, p, run.ID)
	var prod, begun bool
	if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deploys WHERE env = 'prod'), (SELECT deploy_started_at IS NOT NULL FROM runs WHERE id = $1)", run.ID).Scan(&prod, &begun); err != nil ||
		state != "failed" || reasonOf(reason) != "interrupted" || d.calls.Load() > asked+1 || f.Runs.Load() != 1 || prod || begun || lastLine(t, ctx, p, run.ID) != "postgres db: waiting on the platform" {
		t.Fatalf("a cancelled promotion: %s %s, %d asks after the cancel, %d containers, prod %v, begun %v (%v)", state, reasonOf(reason), d.calls.Load()-asked, f.Runs.Load(), prod, begun, err)
	}
	d.results = []provisioners.Result{prodReady}
	if _, state, reason = promote(t, ctx, p, deployRun, image); state != "clean" || f.Ran().Env["DATABASE_URL"] != "value-prod-db" {
		t.Fatalf("the next promotion: %s %s", state, reasonOf(reason))
	}
	var records int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM resources WHERE env = 'prod' AND name = 'db' AND state = 'ready'").Scan(&records); err != nil || records != 1 {
		t.Fatalf("one prod record, carried on: %d (%v)", records, err)
	}
}

// A promotion clears its review again in its deploy step, live, before /run (#2204): one whose review no
// longer clears it — an approval expired or voided while it waited — starts nothing, ends interrupted, and
// production runs what it ran, its line saying why.
func TestAPromotionItsReviewNoLongerClearsStartsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	uncleared.Store(true)
	t.Cleanup(func() { uncleared.Store(false) })
	ran := f.Runs.Load()
	run, state, reason := promote(t, ctx, p, deployRun, image)
	var prod int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys WHERE env = 'prod'").Scan(&prod); err != nil {
		t.Fatal(err)
	}
	var lines string
	if err := p.QueryRow(ctx, "SELECT string_agg(data->>'line', '|' ORDER BY id) FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log'", run).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || reasonOf(reason) != "interrupted" || f.Runs.Load() != ran || prod != 0 ||
		lines != "deploying to production|deploying to production: failed: its review no longer clears it, an approval expired or withdrawn; the version running there is unchanged" {
		t.Fatalf("a promotion its review no longer clears: %s %s, %d /run, %d prod rows, lines %q", state, reasonOf(reason), f.Runs.Load()-ran, prod, lines)
	}
}

// The check is never a journaled answer (#2204): a promotion that crashed after it cleared its review, its
// /run never answered, clears it again as it recovers — after the approvals were voided as the server
// started — and starts nothing.
func TestARecoveredPromotionClearsItsReviewAgain(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	var clears atomic.Bool
	clears.Store(true)
	schema := fmt.Sprintf("dbos_clears_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	launch := func() dbos.Context {
		dc, err := dbos.NewContext(ctx, dbos.Config{AppName: "canyon-runs-test", DatabaseURL: os.Getenv("CANYON_TEST_DATABASE_URL"), DatabaseSchema: schema, ApplicationVersion: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&Pipeline{Pool: p, Repos: pipe.Repos, Gate: runstest.CleanGolden, Executor: pipe.Executor, Vault: pipe.Vault, PublicURL: pipe.PublicURL, JWKSURL: pipe.JWKSURL, Org: pipe.Org,
			Clears: func(context.Context, string, string) (bool, error) { return clears.Load(), nil }}).Register(dc); err != nil {
			t.Fatal(err)
		}
		if err := dbos.Launch(dc); err != nil {
			t.Fatal(err)
		}
		return dc
	}
	holdRun(t, f) // released at cleanup: the crashed attempt's /run never answers it
	ran := f.Runs.Load()
	crashing := launch()
	run, err := Start(ctx, p, crashing, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, ReceiptRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); f.Runs.Load() == ran; time.Sleep(10 * time.Millisecond) { // cleared, and at /run
		if time.Now().After(deadline) {
			t.Fatal("the promotion never reached /run")
		}
	}
	if err := dbos.Shutdown(crashing, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	clears.Store(false) // the approvals voided as the server started again
	recovered := launch()
	t.Cleanup(func() { _ = dbos.Shutdown(recovered, 10*time.Second) })
	state, reason := waitEnded(t, ctx, p, run.ID)
	var prod int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys WHERE env = 'prod'").Scan(&prod); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || reasonOf(reason) != "interrupted" || f.Runs.Load() != ran+1 || prod != 0 {
		t.Fatalf("the recovered promotion: %s %s, %d /run in all, %d prod rows", state, reasonOf(reason), f.Runs.Load()-ran, prod)
	}
}
