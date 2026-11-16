package runs

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deploying is live() with the fake executor and a fresh vault wired and a gate that returns a
// clean log unless told otherwise.
func deploying(t *testing.T, gate GateFunc) (*pgxpool.Pool, context.Context, *executortest.Fake) {
	t.Helper()
	p, ctx := live(t, gate)
	f := executortest.New(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	pipe.Executor, pipe.PublicURL, pipe.JWKSURL, pipe.Vault = &executor.Client{Base: f.URL}, "http://localhost:7777", "http://canyon-server:7780/.well-known/jwks.json", keys
	return p, ctx, f
}

// sealed writes one secrets row the way the PUT route does, under the test server's vault.
func sealed(t *testing.T, ctx context.Context, p *pgxpool.Pool, env, scope, name, value string) {
	t.Helper()
	keyRef, key, val, err := pipe.Vault.Seal(project, env, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ($1, $2, $3, $4, $5, $6, $7, 'person', 'anonymous')", project, env, name, scope, keyRef, key, val); err != nil {
		t.Fatal(err)
	}
}

func reasonOf(r *string) string {
	if r == nil {
		return "<nil>"
	}
	return *r
}

// The split: preview build values reach the build and nothing else, preview runtime values
// reach the container and nothing else, prod rows reach neither, and no value is journaled.
func TestADeployRunHandsEachScopeItsValuesAndJournalsNone(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", "value-build-7f3a")
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-9c1d\nsecond line")
	sealed(t, ctx, p, "prod", "runtime", "DATABASE_URL", "value-prod-2b8e")
	sealed(t, ctx, p, "prod", "build", "NEXT_PUBLIC_API_URL", "value-prod-build-4e6f")
	pipe.DatabaseCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n" // a Kubernetes /run's alone, whatever the pipeline holds
	t.Cleanup(func() { pipe.DatabaseCA = "" })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("a clean deploy run ends clean: %s %v", state, reasonOf(reason))
	}
	build, env := f.Handed()
	if !reflect.DeepEqual(build, map[string]string{"NEXT_PUBLIC_API_URL": "value-build-7f3a"}) {
		t.Fatalf("the build gets exactly the preview build values: %v", build)
	}
	if !reflect.DeepEqual(env, map[string]string{"STRIPE_SECRET_KEY": "value-runtime-9c1d\nsecond line"}) { //nolint:gosec // a fixture name and a synthetic value, not a credential
		t.Fatalf("the container gets exactly the preview runtime values: %v", env)
	}
	for _, literal := range []string{"value-build-7f3a", "value-runtime-9c1d", "value-prod-2b8e", "value-prod-build-4e6f"} {
		if records, hits := journaled(t, ctx, p, run.ID, literal); records == 0 || hits != 0 {
			t.Fatalf("%d step records journaled, %q in %d of them", records, literal, hits)
		}
	}
	if f.Builds.Load() != 1 || f.Runs.Load() != 1 {
		t.Fatalf("one build, one run: %d %d", f.Builds.Load(), f.Runs.Load())
	}
	if id := f.Ran().Identity; id != (executor.Identity{Issuer: "http://localhost:7777", Audience: project, JWKSURL: "http://canyon-server:7780/.well-known/jwks.json"}) {
		t.Fatalf("the container is told how to verify its tokens, the app its audience: %+v", id)
	}
	if ran := f.Ran(); ran.Access != nil || ran.FrontDoor != nil || ran.DatabaseCA != nil { // the Docker executor refuses each
		t.Fatalf("a Docker host's /run carries no access file, no front door and no database CA: %s %s %v", ran.Access, ran.FrontDoor, ran.DatabaseCA)
	}
}

// A row that does not open under the server's key fails the run closed before any container
// is touched, naming the row and never a value.
func TestADeployRunFailsClosedOnARowItCannotOpen(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-9c1d")
	if _, err := p.Exec(ctx, "UPDATE secrets SET name = 'STRIPE_KEY'"); err != nil { // the row moved: the binding no longer holds
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a row that does not open fails the run: %s %v", state, reasonOf(reason))
	}
	if f.Runs.Load() != 0 {
		t.Fatalf("no container for a run that could not read its secrets: %d runs", f.Runs.Load())
	}
	if _, hits := journaled(t, ctx, p, run.ID, "value-runtime-9c1d"); hits != 0 {
		t.Fatalf("the value reached the journal %d times", hits)
	}
	if _, hits := journaled(t, ctx, p, run.ID, "claims-portal/preview/STRIPE_KEY"); hits == 0 {
		t.Fatal("the error names the row that did not open")
	}
	var keyHex, valueHex string // the ciphertext is not a secret, but it has no place in the journal either: names, never bytes
	if err := p.QueryRow(ctx, "SELECT encode(sealed_key, 'hex'), encode(sealed_value, 'hex') FROM secrets").Scan(&keyHex, &valueHex); err != nil {
		t.Fatal(err)
	}
	for _, hex := range []string{keyHex[:16], valueHex[:16]} {
		if _, hits := journaled(t, ctx, p, run.ID, hex); hits != 0 {
			t.Fatalf("the sealed bytes reached the journal %d times", hits)
		}
	}
}

// A row from before the label (key_ref NULL) opens under version 1 beside a row sealed under
// version 2 in one deploy: each row opens under its own key.
func TestADeployRunOpensEachRowUnderItsOwnKeyVersion(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	keyFile := filepath.Join(t.TempDir(), "kek")
	one, err := vault.Load(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	pipe.Vault = one
	sealed(t, ctx, p, "preview", "runtime", "DATABASE_URL", "value-before-1a2b")
	if _, err := p.Exec(ctx, "UPDATE secrets SET key_ref = NULL"); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 32)
	if _, err := rand.Read(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile+".2", second, 0o600); err != nil {
		t.Fatal(err)
	}
	if pipe.Vault, err = vault.Load(keyFile); err != nil {
		t.Fatal(err)
	}
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-after-3c4d")
	if n := 0; p.QueryRow(ctx, "SELECT count(*) FROM secrets WHERE key_ref = 2").Scan(&n) != nil || n != 1 {
		t.Fatalf("one row under 2: %d", n)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("both rows open: %s %v", state, reasonOf(reason))
	}
	if _, env := f.Handed(); !reflect.DeepEqual(env, map[string]string{"DATABASE_URL": "value-before-1a2b", "STRIPE_SECRET_KEY": "value-after-3c4d"}) { //nolint:gosec // fixture names and synthetic values
		t.Fatalf("the container gets both: %v", env)
	}
}

var dana = events.Actor{Kind: "person", Subject: "anonymous", Display: "Dana Whitfield"}

func TestADeployRunBuildsTheCleanPackageAndRecordsThePreview(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("a clean deploy run ends clean: %s %v", state, reason)
	}
	want := "run.state:pending,run.state:running,producer.sarif,producer.sarif,verdict,build.log,build.log,deploy.completed,build.log,build.log,run.state:clean" // its access after it: Private, and No description yet (access_test.go)
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != want {
		t.Fatalf("the story, the terminal row last:\n got %s\nwant %s", got, want)
	}
	// the build waits its turn, runs and is done; then the start runs and is done (contracts/events.md, progress)
	var steps string
	if err := p.QueryRow(ctx, `SELECT string_agg((data->>'step') || ':' || (data->>'status'), ',' ORDER BY id) FROM timeline_rows
		WHERE run_id = $1 AND kind = 'progress' AND data->>'step' IN ('build', 'start')`, run.ID).Scan(&steps); err != nil ||
		strings.TrimPrefix(steps, "build:queued,") != "build:running,build:done,start:running,start:done" { // queued only if it waited at all
		t.Fatalf("the build's and the start's progress: %q (%v)", steps, err)
	}
	var by events.Actor // deployed_by is the run's actor, recorded when the row is written (contracts/routes.md row 47)
	if err := p.QueryRow(ctx, "SELECT deployed_by_kind, deployed_by_subject, deployed_by_display FROM deploys WHERE project_id = $1", project).Scan(&by.Kind, &by.Subject, &by.Display); err != nil || by != dana {
		t.Fatalf("deployed_by: %+v (%v), want %+v", by, err, dana)
	}
	var env, pkgID, image, url, gateRun string
	if err := p.QueryRow(ctx, "SELECT env, package_id, image, url, gate_run FROM deploys WHERE project_id = $1", project).Scan(&env, &pkgID, &image, &url, &gateRun); err != nil ||
		env != "preview" || pkgID != pkg || image != "canyon/claims-portal@sha256:"+strings.Repeat("a", 64) || url != "http://claims-portal.preview.localhost:7777" || gateRun != run.ID {
		t.Fatalf("the deploys row: %s %s %s %s %s (%v)", env, pkgID, image, url, gateRun, err)
	}
	var id, action, subject, display string
	var auditRun *string
	var detail map[string]any
	if err := p.QueryRow(ctx, "SELECT id, action, actor_subject, actor_display, run_id, detail FROM audit_events WHERE actor_kind <> 'canyon'").Scan(&id, &action, &subject, &display, &auditRun, &detail); err != nil ||
		action != "deploy preview" || subject != "anonymous" || display != "Dana Whitfield" || auditRun == nil || *auditRun != run.ID || detail["url"] != url || detail["image_digest"] != "sha256:"+strings.Repeat("a", 64) || detail["package"] != pkg {
		t.Fatalf("the deploy's own audit row names who triggered the run and what was deployed: %s %s/%s %v %v (%v)", action, subject, display, auditRun, detail, err)
	}
	// and the org's feed says so, under the app, so an open audit page reads it (contracts/events.md, audit.appended; #728)
	if n := 0; p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE scope = 'org' AND kind = 'audit.appended' AND project_id = $1 AND data->>'event_id' = $2", project, id).Scan(&n) != nil || n != 1 {
		t.Fatalf("the deploy's audit row has %d audit.appended rows in the org's feed, want 1", n)
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var deployed map[string]any
	for _, r := range page.Rows {
		if r.Kind == events.DeployCompleted {
			_ = json.Unmarshal(r.Data, &deployed)
		}
	}
	if deployed["env"] != "preview" || deployed["url"] != url || deployed["package"] != pkg || deployed["image_digest"] != "sha256:"+strings.Repeat("a", 64) || page.Next != nil {
		t.Fatalf("deploy.completed %v, next %v", deployed, page.Next)
	}
	if f.Builds.Load() != 1 || f.Runs.Load() != 1 {
		t.Fatalf("one build, one run: %d %d", f.Builds.Load(), f.Runs.Load())
	}
}

// A build the executor answers busy is asked again once its Retry-After has passed, the wait a durable
// sleep between the two asks, and the run deploys; one still busy at the last ask fails the run as
// Canyon's own error, never as the package's build failing (contracts/executor.md, Busy).
func TestABusyBuildIsAskedAgain(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.BuildBusy.Store(1)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil || f.Builds.Load() != 2 || f.Runs.Load() != 1 {
		t.Fatalf("the build after the wait: %s %s after %d build(s)", state, why(reason), f.Builds.Load())
	}
	// a busy executor is waited out, however long: a wait never fails the run (#1652)
	f.BuildBusy.Store(5)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", "value-build-7f3a") // a build value changed: the package builds again (#1244)
	run, err = Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEndedWithin(t, ctx, p, run.ID, 30*time.Second); state != "clean" || reason != nil || f.Builds.Load() != 8 {
		t.Fatalf("a build busy five times: %s %s after %d build(s)", state, why(reason), f.Builds.Load())
	}
}

// A cancel asked while a busy build waits lands before the next ask: nothing more is built.
func TestACancelDuringABusyBuildsWaitBuildsNothingMore(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.BuildBusy.Store(1)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for f.Builds.Load() == 0 { // the busy answer is in once the fake counts the ask; the run then sleeps its 1 s
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", run.ID); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "interrupted" || f.Builds.Load() != 1 {
		t.Fatalf("a cancel during the wait: %s %s after %d build(s)", state, why(reason), f.Builds.Load())
	}
}

func TestABlockedDeployRunBuildsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.Golden) // the golden log blocks
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("blocked, got %s", state)
	}
	if n := f.Builds.Load(); n != 0 {
		t.Fatalf("a blocked verdict builds nothing: %d builds", n)
	}
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); strings.Contains(got, "build.log") || strings.Contains(got, "deploy.completed") {
		t.Fatalf("no build rows on a blocked run: %s", got)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys").Scan(&n); err != nil || n != 0 {
		t.Fatalf("deploys rows: %d (%v)", n, err)
	}
}

// A deploy that goes ahead under approvals reads them again before it writes, never trusting its journaled
// verdict (#2204): an approval voided while it builds — as the approver rule's migration voids every earlier
// one, which a recovered run replays past — ends it before its deploy, nothing deployed.
func TestADeployUnderApprovalsReadsThemAgainBeforeItDeploys(t *testing.T) {
	p, ctx, f := deploying(t, runstest.Golden)
	log, err := runstest.Golden(ctx, "", "", policy.Snapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range log.Runs {
		for _, res := range r.Results {
			if Blocks(res.Properties.Canyon.Blocks) {
				if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
					VALUES ($1, $2, $3, 'rotation is ticketed', now() + interval '1 hour', 'person', 'erin@meridian.example', 'Erin', true)`, project, res.RuleID, res.PartialFingerprints.CanyonV1); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	void := func() {
		if _, err := p.Exec(context.Background(), "UPDATE approvals SET approves_blocking = false"); err != nil {
			t.Error(err)
		}
	}
	f.OnBuild.Store(&void)
	t.Cleanup(func() { f.OnBuild.Store(nil) })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	var deployed int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys").Scan(&deployed); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || reasonOf(reason) != "interrupted" || f.Builds.Load() != 1 || deployed != 0 {
		t.Fatalf("a deploy whose approvals were voided while it built: %s %v, %d builds, %d deploys", state, reasonOf(reason), f.Builds.Load(), deployed)
	}
}

// Two blocking findings: a deploy goes ahead only when both carry an unexpired approval, lists them
// on its verdict and still ends blocked; a replayed verdict step answers from the row it stored.
func TestADeployRunGoesAheadOnlyWhenEveryBlockingFindingIsApproved(t *testing.T) {
	twice := func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) { // the golden secret finding, and again under a second fingerprint
		l, err := runstest.Golden(ctx, dir, tree, snap, report)
		if err != nil {
			return l, err
		}
		again := l.Runs[2].Results[0] // gitleaks' key
		fp := again.PartialFingerprints.CanyonV1
		again.PartialFingerprints.CanyonV1 = fp[:2] + fp[3:] + fp[2:3]
		l.Runs[2].Results = append(l.Runs[2].Results, again)
		return l, nil
	}
	p, ctx, f := deploying(t, twice)
	log, err := twice(ctx, "", "", policy.Snapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := log.Runs[2].Results
	first, second := results[0], results[len(results)-1]
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	approve := func(on string, r finding.Result) {
		if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
			VALUES ($1, $2, $3, 'rotation is ticketed', $4, $5, $6, $7, true)`, on, r.RuleID, r.PartialFingerprints.CanyonV1, expires, dana.Kind, dana.Subject, dana.Display); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func() (string, map[string]any) {
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
		if err != nil {
			t.Fatal(err)
		}
		if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
			t.Fatalf("a deploy with blocking findings ends blocked, approved or not: %s %v", state, reasonOf(reason))
		}
		var v map[string]any
		if err := p.QueryRow(ctx, "SELECT data FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return run.ID, v
	}
	previews := func() (n int) {
		if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := p.Exec(ctx, "INSERT INTO projects (id) VALUES ('quote-engine')"); err != nil {
		t.Fatal(err)
	}
	approve("quote-engine", first) // another app's approvals of the same findings unlock nothing here
	approve("quote-engine", second)
	for i, approved := range [][]finding.Result{nil, {first}} {
		for _, r := range approved {
			approve(project, r)
		}
		run, v := deploy()
		if _, listed := v["approved"]; listed || f.Builds.Load() != 0 || previews() != 0 || strings.Contains(strings.Join(kinds(t, ctx, p, run), ","), "build.log") {
			t.Fatalf("%d of 2 approved: nothing built, previewed or listed: %v, %d builds, %d previews", i, v["approved"], f.Builds.Load(), previews())
		}
	}
	approve(project, second)
	run, v := deploy()
	want := "run.state:pending,run.state:running,producer.sarif,producer.sarif,verdict,build.log,build.log,deploy.completed,build.log,build.log,run.state:blocked"
	if got := strings.Join(kinds(t, ctx, p, run), ","); got != want {
		t.Fatalf("both approved, the deploy goes ahead:\n got %s\nwant %s", got, want)
	}
	var gateRun string
	if err := p.QueryRow(ctx, "SELECT gate_run FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&gateRun); err != nil || gateRun != run || f.Builds.Load() != 1 || f.Runs.Load() != 1 {
		t.Fatalf("one build, one container, a preview naming %s: %s, %d builds, %d runs (%v)", run, gateRun, f.Builds.Load(), f.Runs.Load(), err)
	}
	entry := func(r finding.Result) any {
		return map[string]any{"rule": r.RuleID, "fingerprint": r.PartialFingerprints.CanyonV1, "reason": "rotation is ticketed", "expires_at": expires,
			"approved_by": map[string]any{"kind": dana.Kind, "subject": dana.Subject, "display": dana.Display}}
	}
	// gitleaks' two findings are on one line under one rule: one entry, so one blocking finding (#2013)
	if !reflect.DeepEqual(v["approved"], []any{entry(first), entry(second)}) || v["state"] != "blocked" || v["blocking"] != float64(1) {
		t.Fatalf("the verdict lists the approvals it went ahead under: %v", v)
	}
	check, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, check.ID)
	var listed bool
	if err := p.QueryRow(ctx, "SELECT data ? 'approved' FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", check.ID).Scan(&listed); err != nil || listed || f.Builds.Load() != 1 {
		t.Fatalf("a check-only run goes nowhere, so its verdict lists no approvals: listed %v, %d builds (%v)", listed, f.Builds.Load(), err)
	}
	if _, err := p.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	before := strings.Join(kinds(t, ctx, p, run), ",")
	ruled, err := pipe.verdict(ctx, Input{Run: run, Project: project, Package: pkg, Deploy: true, Actor: dana}, log, "blocked", 2, 2)
	if after := strings.Join(kinds(t, ctx, p, run), ","); err != nil || !ruled.Approved || after != before {
		t.Fatalf("replayed after both expired, the stored verdict answers: %+v (%v)\n before %s\n after  %s", ruled, err, before, after)
	}
}

// A build the executor went away under — its stream cut before a final status, as a restart, an upgrade or a node
// drain cuts it — is started again once the executor is back, and the run deploys; the person reads that Canyon is
// restarting its builder, never that the build failed. Lost twice, the run fails as a lost build always did: Canyon's
// own error (#2671).
func TestABuildTheExecutorWentAwayUnderIsStartedAgainOnce(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.DropBuilds.Store(1)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil || f.Builds.Load() != 2 || f.Runs.Load() != 1 {
		t.Fatalf("a build lost once: %s %s after %d build(s)", state, why(reason), f.Builds.Load())
	}
	if lines := lastLines(t, ctx, p, run.ID); !slices.Contains(lines, BuilderRestarting) {
		t.Fatalf("the build log says Canyon is restarting its builder: %q", lines)
	}
	f.DropBuilds.Store(2)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", "value-build-2671") // a build value changed: the package builds again
	run, err = Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" || f.Builds.Load() != 4 || f.Runs.Load() != 1 {
		t.Fatalf("a build lost twice fails as Canyon's own: %s %s after %d build(s)", state, why(reason), f.Builds.Load())
	}
}

// A build the executor's own host failed (contracts/executor.md, The executor's own failure; #2489) fails the run as
// Canyon's own, failure_reason error, never build: the executor's line is still the last build.log line, and nothing
// is deployed.
func TestABuildTheHostFailedFailsTheRunAsCanyonsOwn(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.HostFailBuild = true
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a build the executor's host failed is Canyon's own: %s %v", state, reason)
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var last map[string]any
	for _, r := range page.Rows {
		if r.Kind == events.BuildLog {
			_ = json.Unmarshal(r.Data, &last)
		}
	}
	if last["line"] != executortest.HostFailLine {
		t.Fatalf("the last build.log line: %v, want the executor's", last)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys").Scan(&n); err != nil || n != 0 || f.Runs.Load() != 0 {
		t.Fatalf("nothing deployed, nothing run: %d rows, %d runs (%v)", n, f.Runs.Load(), err)
	}
}

func TestAFailedBuildFailsTheRunAsTheUsersAndLeavesNoDeploy(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.FailBuild = true
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "build" {
		t.Fatalf("a build the executor refused is the user's fix: %s %v", state, reason)
	}
	want := "run.state:pending,run.state:running,producer.sarif,producer.sarif,verdict,build.log,build.log,build.log,run.state:failed"
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != want {
		t.Fatalf("the executor's message is the last build.log line:\n got %s\nwant %s", got, want)
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var last map[string]any
	for _, r := range page.Rows {
		if r.Kind == events.BuildLog {
			_ = json.Unmarshal(r.Data, &last)
		}
	}
	if last["line"] != "pack: exit status 1" {
		t.Fatalf("the last build.log line: %v", last)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM deploys) + (SELECT count(*) FROM audit_events WHERE action = 'deploy preview')").Scan(&n); err != nil || n != 0 || f.Runs.Load() != 0 {
		t.Fatalf("nothing deployed, nothing audited, nothing run: %d rows, %d runs (%v)", n, f.Runs.Load(), err)
	}
	failedAudited(t, ctx, p, run.ID, "build failed")
}

// A build the cluster had no room to run ends the run failed no_room, never build nor error: the
// scheduler's message is its last build.log line, and nothing deploys (contracts/routes.md, the deploys
// row; #1637).
func TestABuildTheClusterHadNoRoomForIsCapacity(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.NoRoom = "0/4 nodes are available: 4 Insufficient cpu."
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "no_room" {
		t.Fatalf("a build with no room: %s %s", state, why(reason))
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' ORDER BY id DESC LIMIT 1", run.ID).Scan(&line); err != nil || line != f.NoRoom {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
	if f.Runs.Load() != 0 {
		t.Fatalf("%d runs after a build with no room", f.Runs.Load())
	}
	failedAudited(t, ctx, p, run.ID, "build had no room")
}

// failedAudited is the one row a run that failed after its review writes on the trail: Canyon's, the
// action naming what broke, the package in its detail (contracts/routes.md, the audit read).
func failedAudited(t *testing.T, ctx context.Context, p *pgxpool.Pool, run, action string) {
	t.Helper()
	var rows []string
	r, err := p.Query(ctx, "SELECT action || ' ' || actor_kind || ' ' || actor_subject || ' ' || coalesce(run_id, '') || ' ' || detail::text FROM audit_events WHERE action LIKE '% failed' OR action = 'build had no room'")
	if err == nil {
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
	}
	if want := []string{action + " canyon gate " + run + ` {"package": "` + pkg + `"}`}; err != nil || !slices.Equal(rows, want) {
		t.Fatalf("the trail's failed row: %q (%v), want %q", rows, err, want)
	}
}

// A package no buildpack on the host detects ends its run detect, never build: nothing was compiled
// (contracts/routes.md, the deploys row; #1242). The detector's lines, then the executor's message, are
// the build.log rows, and the run row keeps what the host builds, and whether a buildpack errored while
// detecting, for the run read.
func TestAFailedDetectFailsTheRunAsDetectAndKeepsWhatTheHostBuilds(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.Undetected, f.UndetectedErrored = []string{"paketo-buildpacks/ruby", "paketo-buildpacks/nodejs"}, true
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "detect" {
		t.Fatalf("a package no buildpack detects is not a compile error: %s %s", state, *cmp.Or(reason, new(string)))
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var last map[string]any
	for _, r := range page.Rows {
		if r.Kind == events.BuildLog {
			_ = json.Unmarshal(r.Data, &last)
		}
	}
	if last["line"] != "no buildpack group passed detection" {
		t.Fatalf("the executor's message is the last build.log line: %v", last)
	}
	var builds []string
	var errored bool
	if err := p.QueryRow(ctx, "SELECT builds, detect_errored FROM runs WHERE id = $1", run.ID).Scan(&builds, &errored); err != nil || !slices.Equal(builds, f.Undetected) || !errored {
		t.Fatalf("the run keeps what the host builds, and that a buildpack errored: %q %v (%v)", builds, errored, err)
	}
	if f.Runs.Load() != 0 {
		t.Fatalf("nothing runs after a failed detect: %d runs", f.Runs.Load())
	}
	failedAudited(t, ctx, p, run.ID, "detect failed")
}

func TestAFailedContainerStartAfterABuildFailsTheRunAsCanyons(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.FailRun = true
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" || failedIn(t, ctx, p, run.ID) != "deploy" {
		t.Fatalf("a container that fails to start is Canyon's failure, in the deploy: %s %v", state, reason)
	}
	if line := lastLine(t, ctx, p, run.ID); line != "deploying to preview: failed: docker run: exit status 125" {
		t.Fatalf("the last build.log line says why, in the executor's own words (#2539): %q", line)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM deploys) + (SELECT count(*) FROM audit_events WHERE action = 'deploy preview')").Scan(&n); err != nil || n != 0 {
		t.Fatalf("nothing deployed, nothing audited: %d rows (%v)", n, err)
	}
}

// An app that built and did not stay up is the user's fix: the run ends failed/start, its last lines
// and then the executor's reason are the last build.log rows, and none of the lines is journaled.
func TestAnAppThatDoesNotStayUpFailsTheRunAsTheUsers(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.Exited = &executor.Exited{Reason: "the app did not stay up: its container is exited", Output: "> next start\n\nnode: libatomic.so.1: cannot open shared object file\n"}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reasonOf(reason) != "start" {
		t.Fatalf("an app that did not stay up is the user's fix: %s %s", state, reasonOf(reason))
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var lines []string
	for _, r := range page.Rows {
		var d struct{ Line string }
		if r.Kind == events.BuildLog && json.Unmarshal(r.Data, &d) == nil {
			lines = append(lines, d.Line)
		}
	}
	want := []string{"> next start", "", "node: libatomic.so.1: cannot open shared object file", "the app did not stay up: its container is exited"}
	if len(lines) < len(want) || !reflect.DeepEqual(lines[len(lines)-len(want):], want) {
		t.Fatalf("the last build.log rows are the app's lines, then the reason: %q", lines)
	}
	failedAudited(t, ctx, p, run.ID, "start failed")
	if records, hits := journaled(t, ctx, p, run.ID, "libatomic"); records == 0 || hits != 0 {
		t.Fatalf("%d step records journaled, the app's lines in %d of them", records, hits)
	}
	// The step again, as a recovery runs it after a crash before its answer was journaled: /run starts
	// the app again, and that start prints more lines than the first, or fewer. Either way the block
	// the first attempt wrote stands whole: nothing of the replay's is appended, nothing of the first's dropped.
	in := Input{Run: run.ID, Project: project, Package: pkg, Deploy: true, Actor: dana}
	for name, output := range map[string]string{"more lines": "one\ntwo\nthree\nfour\nfive\nsix\n", "fewer lines": "one\n"} {
		f.Exited = &executor.Exited{Reason: "the app did not stay up: its container is exited", Output: output}
		if url, err := pipe.deployPreview(ctx, in, built{Image: "canyon/claims-portal@sha256:" + strings.Repeat("a", 64)}); url != "" || err != nil {
			t.Fatalf("%s: the replayed step: %q (%v)", name, url, err)
		}
		page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
		var again []string
		for _, r := range page.Rows {
			var d struct{ Line string }
			if r.Kind == events.BuildLog && json.Unmarshal(r.Data, &d) == nil {
				again = append(again, d.Line)
			}
		}
		if !reflect.DeepEqual(again, lines) {
			t.Fatalf("%s: the rows after the replay:\n%q\nbefore it:\n%q", name, again, lines)
		}
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM deploys) + (SELECT count(*) FROM audit_events WHERE action = 'deploy preview')").Scan(&n); err != nil || n != 0 {
		t.Fatalf("nothing deployed, nothing audited: %d rows (%v)", n, err)
	}
}

func TestAGateRunWithoutTheFlagBuildsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, run.ID); state != "clean" || f.Builds.Load() != 0 {
		t.Fatalf("a gate run stops at the verdict: %s, %d builds", state, f.Builds.Load())
	}
}

// The build step again, as a recovery after a crash before its answer was journaled would run it: a
// second attempt, and the ledger says so. Its lines follow the first's, the run row names the image it
// made, and it starts nothing: the start is its own step (#728).
func TestAReplayedBuildStepIsASecondAttempt(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("the deploy run: %s %v", state, reasonOf(reason))
	}
	lines := func() []string {
		rows, err := p.Query(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' ORDER BY id", run.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := lines()
	// the run as a recovery finds it: its build answered and was not journaled, so nothing after it ran and nothing ended
	if _, err := p.Exec(ctx, "UPDATE runs SET ended_at = NULL WHERE id = $1", run.ID); err != nil {
		t.Fatal(err)
	}
	b, err := pipe.build(ctx, Input{Run: run.ID, Project: project, Package: pkg, Deploy: true, Actor: dana})
	image := "canyon/claims-portal@sha256:" + strings.Repeat("a", 64)
	if err != nil || b.Image != image || f.Builds.Load() != 2 || f.Runs.Load() != 1 {
		t.Fatalf("the replayed build: %+v (%v), %d builds, %d containers", b, err, f.Builds.Load(), f.Runs.Load())
	}
	if want := append(slices.Clone(first), "===> DETECTING", "===> BUILDING"); !slices.Equal(lines(), want) {
		t.Fatalf("the build.log rows after the replay:\n%q\nwant the first attempt's, then the second's:\n%q", lines(), want)
	}
	var built string
	var deploys int
	if err := p.QueryRow(ctx, "SELECT build_image, (SELECT count(*) FROM deploys) FROM runs WHERE id = $1", run.ID).Scan(&built, &deploys); err != nil || built != image || deploys != 1 {
		t.Fatalf("the run row names %q, %d deploys rows (%v)", built, deploys, err)
	}
}

// The step again, as a recovery after a crash between its commit and the journal would run it:
// the recorded row answers, the container is not replaced (replacing it is destructive, and the
// recorded image is the one preview must keep serving), and nothing is written twice.
func TestAReplayedDeployStepRecordsOnceAndTouchesNoContainer(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	in := Input{Run: run.ID, Project: project, Package: pkg, Deploy: true, Actor: dana}
	url, err := pipe.deploy(ctx, in, "preview", "canyon/claims-portal@sha256:"+strings.Repeat("a", 64), built{})
	if err != nil || url != "http://claims-portal.preview.localhost:7777" || f.Runs.Load() != 1 {
		t.Fatalf("the replayed step answers the recorded URL and runs no container: %q (%v), %d runs", url, err, f.Runs.Load())
	}
	var deploys, rows, audits int
	if err := p.QueryRow(ctx, "SELECT (SELECT count(*) FROM deploys), (SELECT count(*) FROM timeline_rows WHERE kind = 'deploy.completed'), (SELECT count(*) FROM audit_events WHERE action = 'deploy preview')").Scan(&deploys, &rows, &audits); err != nil || deploys != 1 || rows != 1 || audits != 1 {
		t.Fatalf("recorded once: %d deploys, %d rows, %d audit rows (%v)", deploys, rows, audits, err)
	}
}

// A build printing past the cap leaves buildLogCap rows, one saying the log was cut, and the
// executor's failure message last: a post-install script's half hour is not millions of rows
// under the append lock, and the contract still reads the failure as the last line.
func TestABuildLogPastTheCapIsTruncatedAndTheFailureIsStillLast(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.FailBuild = true
	f.Lines.Store(3000)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	// 2,001 build.log rows, each its own append: 16 s on a loaded CI runner, past waitEnded's 15 (#1033)
	if state, reason := waitEndedWithin(t, ctx, p, run.ID, time.Minute); state != "failed" || reason == nil || *reason != "build" {
		t.Fatalf("a build the executor refused is the user's fix: %s %v", state, reason)
	}
	rows, err := p.Query(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' ORDER BY id", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(lines) != buildLogCap+2 {
		t.Fatalf("%d build.log rows (%v), want the cap, the truncation row and the failure", len(lines), err)
	}
	if lines[buildLogCap-1] != "line 2000" || !strings.Contains(lines[buildLogCap], "truncated after 2000 lines") || lines[buildLogCap+1] != "pack: exit status 1" {
		t.Fatalf("the last three rows: %q", lines[buildLogCap-1:])
	}
}

func TestADeployRunWithNoExecutorOrABadPublicURLFailsClosedAndTouchesNoContainer(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.Executor = nil
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("no executor: the run fails closed as Canyon's failure, never a panic: %s %v", state, reasonOf(reason))
	}
	pipe.Executor, pipe.Vault = &executor.Client{Base: f.URL}, nil
	run, err = Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" || f.Builds.Load() != 0 {
		t.Fatalf("no vault: the run fails closed before the build, never a panic: %s %v, %d builds", state, reasonOf(reason), f.Builds.Load())
	}
	if _, hits := journaled(t, ctx, p, run.ID, "no vault configured"); hits == 0 {
		t.Fatal("the step says why it refused, in words, not a recovered panic")
	}
	// the record is checked before the container is touched
	pipe.Executor = &executor.Client{Base: f.URL}
	pipe.PublicURL = "not a url"
	in := Input{Run: run.ID, Project: project, Package: pkg, Deploy: true, Actor: dana}
	if _, err := pipe.deploy(ctx, in, "preview", "canyon/claims-portal@sha256:"+strings.Repeat("a", 64), built{}); err == nil || f.Runs.Load() != 0 {
		t.Fatalf("a bad public url: %v, containers started %d", err, f.Runs.Load())
	}
	pipe.PublicURL = "http://localhost:7777"
	if _, err := pipe.deploy(ctx, in, "preview", "canyon/claims-portal:latest", built{}); err == nil || f.Runs.Load() != 0 {
		t.Fatalf("an image that is not a digest reference: %v, containers started %d", err, f.Runs.Load())
	}
	// a promotion's start has no build before it to refuse first: the start refuses too, never a panic (#728)
	pipe.Executor = nil
	if _, err := pipe.deploy(ctx, in, "prod", "canyon/claims-portal@sha256:"+strings.Repeat("a", 64), built{}); err == nil || !strings.Contains(err.Error(), "no executor configured") {
		t.Fatalf("no executor at the start: %v", err)
	}
}

// appAnswer is a transport whose one request is answered by answer, never the network.
type appAnswer func(*http.Request) (*http.Response, error)

func (v appAnswer) RoundTrip(r *http.Request) (*http.Response, error) { return v(r) }

// Once the container is up, the deploy on Docker asks the app one GET / past Canyon's sign-in — its
// container, as the proxy reaches it, with the app's address as Host, no cookie, no redirect followed —
// and its deploy.completed row and deploy row say what it answered: the status and the body's first
// non-empty line, masked by the app's values; or, when nothing answered, why. A stock Django answering
// 400 DisallowedHost reads as such, never as a clean Live (contracts/routes.md, envs.<env>.answered; #1740).
func TestADeployAsksTheAppOnceAndSaysWhatItAnswered(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "runtime", "DJANGO_SECRET", "value-in-the-page-9f1e")
	t.Cleanup(func() { pipe.Ask = nil })
	var asked []*http.Request
	for name, tc := range map[string]struct {
		answer func(*http.Request) (*http.Response, error)
		want   string
	}{
		"a 400": {func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader("\n  <h1>Bad Request (400)</h1> value-in-the-page-9f1e\n<p>more</p>")), Request: r}, nil
		}, `{"said":"<h1>Bad Request (400)</h1> <DJANGO_SECRET>","status":400}`},
		"a long line that hides text": {func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("\u202e\x1b[2J" + strings.Repeat("x", 300))), Request: r}, nil
		}, `{"said":"[2J` + strings.Repeat("x", 197) + `","status":200}`},
		"a page, whose markup is never said": {func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader("<!DOCTYPE html>\n<h1>Bad Request (400)</h1>")), Request: r}, nil
		}, `{"said":"","status":400}`},
		"a redirect, never followed": {func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"/login"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, `{"said":"","status":302}`},
		"nothing answered": {func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}, `{"error":"refused"}`},
	} {
		asked = nil
		pipe.Ask = appAnswer(func(r *http.Request) (*http.Response, error) { asked = append(asked, r); return tc.answer(r) })
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
		if err != nil {
			t.Fatal(err)
		}
		if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
			t.Fatalf("%s: what the app answered never fails the deploy: %s %v", name, state, reasonOf(reason))
		}
		var same bool
		if err := p.QueryRow(ctx, "SELECT data->'answered' = $2::jsonb AND (SELECT answered FROM deploys WHERE project_id = $3 AND env = 'preview') = $2::jsonb FROM timeline_rows WHERE run_id = $1 AND kind = 'deploy.completed'", run.ID, tc.want, project).Scan(&same); err != nil || !same {
			t.Errorf("%s: the row and the deploy do not both say %s (%v)", name, tc.want, err)
		}
		if len(asked) != 1 || asked[0].Method != http.MethodGet || asked[0].URL.String() != "http://claims-portal-preview:3000/" || asked[0].Host != "claims-portal.preview.localhost:7777" || asked[0].Header.Get("Cookie") != "" || asked[0].Header.Get("Authorization") != "" {
			t.Errorf("%s: asked %d times: %v; want one GET / of the container past the sign-in, as the app's address, with no credential", name, len(asked), asked)
		}
	}
}

// Each deploy row Canyon writes says when its container was started, the moment before its /run (contracts/routes.md,
// the refused predicate; #2738): after the run began, by the review and build in preview's case, and before the row
// says it went live. A refusal counts against the version from then, never from the run's start.
func TestADeployRowSaysWhenItsContainerStarted(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	run := deployed(t, ctx, p, pkg)
	started := func(env string) {
		t.Helper()
		var ok bool
		if err := p.QueryRow(ctx, `SELECT container_started_at IS NOT NULL AND container_started_at >= run_started_at AND container_started_at <= deployed_at
			FROM deploys WHERE project_id = $1 AND env = $2`, project, env).Scan(&ok); err != nil || !ok {
			t.Fatalf("%s's row: container_started_at between the run's start and its going live %v (%v)", env, ok, err)
		}
	}
	started("preview")
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, run, image); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	started("prod")
}

// lastLines is the run's build.log lines, in order.
func lastLines(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) []string {
	t.Helper()
	rows, err := p.Query(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' ORDER BY id", run)
	if err != nil {
		t.Fatal(err)
	}
	return must(pgx.CollectRows(rows, pgx.RowTo[string]))
}
