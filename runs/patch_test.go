//go:build !noee

package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// fakeRunImage is the fake executor's GET /builder run_image, and so the identity the plan names.
var fakeRunImage = "sha256:" + strings.Repeat("c", 64)

// patching is a host that plans patches: sign-in on, the fake executor, and every image's read checked with
// one high libssl3 finding Canyon fixes; the workflows a start enqueues wait (seeded), and are cancelled at
// the test's end, so none holds the patches queue's slot for the tests after it.
func patching(t *testing.T) (*pgxpool.Pool, context.Context, *executortest.Fake) {
	t.Helper()
	p, ctx := seeded(t)
	f := executortest.New(t)
	executorWas, readWas := pipe.Executor, pipe.PlatformRead.Load()
	pipe.Executor = &executor.Client{Base: f.URL}
	pipe.Patches.Store(true)
	pipe.PlatformRead.Store(func(context.Context, string, string) (PlatformImage, error) {
		return PlatformImage{Checked: true, Why: "1 known-vulnerable package with a published fix, in the system libraries and build tools Canyon adds", Findings: []PlatformFinding{libssl3("high", "CVE-2024-6119")}}, nil
	})
	t.Cleanup(func() {
		pipe.Executor = executorWas
		pipe.Patches.Store(false)
		pipe.PlatformRead.Store(readWas)
		rows, err := p.Query(context.Background(), "SELECT id FROM runs")
		if err != nil {
			t.Error(err)
			return
		}
		for _, id := range must(pgx.CollectRows(rows, pgx.RowTo[string])) {
			Cancel(durable, id)
		}
	})
	return p, ctx, f
}

// matchedRunImage stores the golden's run image as the rescan does under digest, every layer matched on a
// database an hour old with nothing found: it carries every fix.
func matchedRunImage(t *testing.T, ctx context.Context, p *pgxpool.Pool, digest string) {
	t.Helper()
	img, err := imageOf(loadGolden(t).runImageOf(t, "3.0.13-0ubuntu3.5"))
	if err != nil {
		t.Fatal(err)
	}
	if err := storeRunImage(ctx, p, digest, digest, img); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO part_matches (part_sha, db_built_at, matched_at, report) SELECT part_sha, now() - interval '1 hour', now(), $2
		FROM run_image_layers WHERE digest = $1`, digest, json.RawMessage(`{"SchemaVersion":2,"Results":[]}`)); err != nil {
		t.Fatal(err)
	}
}

// runningPreview is app's preview running image digest, as a deploy left it.
func runningPreview(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake, app, digest string) {
	t.Helper()
	storedAs(t, ctx, p, nil, app, digest)
	f.Start(app, "preview")
}

func sha(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// canaryOrder is the order Canyon Enterprise's plan takes an identity's canary in: a hash of the app's id
// (design/PATCHING.md §4.2).
func canaryOrder(project string) string {
	sum := sha256.Sum256([]byte(project))
	return hex.EncodeToString(sum[:])
}

// patchRun is app's newest patch run: its id, state and failure reason.
func patchRun(t *testing.T, ctx context.Context, p *pgxpool.Pool, app string) (id, state, reason string) {
	t.Helper()
	err := p.QueryRow(ctx, "SELECT id, state, coalesce(failure_reason, '') FROM runs WHERE project_id = $1 AND kind = 'patch' ORDER BY created_at DESC LIMIT 1", app).Scan(&id, &state, &reason)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return id, state, reason
}

// A patch run starts only while no other run of the project is queued or running, and any start of a
// person's run ends one before its /run, as a pending run is ended, and goes ahead: a person's run always
// goes first. Its app is planned again, and patched once the person's run has ended (design/PATCHING.md
// §4.3; contracts/routes.md, Patch runs).
func TestPersonRunStartsAheadOfPatch(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "patchy", sha('a'))
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the canary's patch run: %d started, %v", n, err)
	}
	first, state, _ := patchRun(t, ctx, p, "patchy")
	var by, pkgID string
	if err := p.QueryRow(ctx, "SELECT started_by_kind || '/' || started_by_subject, package_id FROM runs WHERE id = $1", first).Scan(&by, &pkgID); err != nil || by != "canyon/patch" || pkgID != "pkg_patchy" || state != "pending" {
		t.Fatalf("the patch run %s: %s by %s on %s (%v)", first, state, by, pkgID, err)
	}
	person, err := Start(ctx, p, durable, StartInput{Project: "patchy", Package: "pkg_patchy", Actor: dana, Policy: testPolicy()})
	if err != nil {
		t.Fatalf("a person's start over a pending patch run: %v", err)
	}
	if _, state, reason := patchRun(t, ctx, p, "patchy"); state != "failed" || reason != "interrupted" {
		t.Fatalf("the pending patch run after a person's start: %s %s", state, reason)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("while the person's run is under way: %d started, %v", n, err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = $1", person.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("once the person's run ended: %d started, %v", n, err)
	}
	second, _, _ := patchRun(t, ctx, p, "patchy")
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'running' WHERE id = $1", second); err != nil { // its gate under way
		t.Fatal(err)
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: "patchy", Package: "pkg_patchy", Actor: dana, Policy: testPolicy()}); err != nil {
		t.Fatalf("a person's start over a running patch run: %v", err)
	}
	if id, state, reason := patchRun(t, ctx, p, "patchy"); id != second || state != "failed" || reason != "interrupted" {
		t.Fatalf("the running patch run after a person's start: %s %s %s", id, state, reason)
	}
}

// endAll ends every live run of app clean, as if each had finished.
func endAll(t *testing.T, ctx context.Context, p *pgxpool.Pool, app string) {
	t.Helper()
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE project_id = $1 AND ended_at IS NULL", app); err != nil {
		t.Fatal(err)
	}
}

// startedPatch plans until app's patch run starts, marks it running — its gate under way — and answers it.
func startedPatch(t *testing.T, ctx context.Context, p *pgxpool.Pool, app string) string {
	t.Helper()
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("%s's patch run: %d started (%v)", app, n, err)
	}
	id, _, _ := patchRun(t, ctx, p, app)
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'running' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A promotion, an undeploy of preview and the requests that call EndPatchTx — a delete, a recreation of
// the app's network — each end a running patch run before its /run and go ahead, its workflow cancelled
// and its waiting tasks taken out of their queue (contracts/routes.md, Patch runs).
func TestEveryPersonsRequestEndsARunningPatchRun(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "patchy", sha('a'))
	ended := func(what, id string) {
		t.Helper()
		if got, state, reason := patchRun(t, ctx, p, "patchy"); got != id || state != "failed" || reason != "interrupted" {
			t.Errorf("%s: the patch run %s is %s %s", what, got, state, reason)
		}
		if st := settled(t, id); st != dbos.WorkflowStatusCancelled {
			t.Errorf("%s: the patch run's workflow is %s, not cancelled", what, st)
		}
	}
	id := startedPatch(t, ctx, p, "patchy")
	promotion := StartInput{Project: "patchy", Package: "pkg_patchy", Actor: dana, Policy: testPolicy(),
		Promote: &Promotion{Image: "canyon/patchy@" + sha('a'), GateRun: "gr_patchy", DeployedBy: dana}}
	if _, err := Start(ctx, p, durable, promotion); err != nil {
		t.Fatalf("a promotion over a running patch run: %v", err)
	}
	ended("a promotion", id)
	endAll(t, ctx, p, "patchy")
	id = startedPatch(t, ctx, p, "patchy")
	if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		b, err := BrakeTx(ctx, tx, "patchy", "preview", dana, false)
		if err == nil && b.Ended != id {
			t.Errorf("an undeploy of preview ended %q, not the patch run %s", b.Ended, id)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	Cancel(durable, id)
	ended("an undeploy of preview", id)
	endAll(t, ctx, p, "patchy")
	id = startedPatch(t, ctx, p, "patchy")
	var got string
	if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { got, err = EndPatchTx(ctx, tx, "patchy"); return err }); err != nil || got != id {
		t.Fatalf("EndPatchTx ended %q, not %s (%v)", got, id, err)
	}
	Cancel(durable, id)
	ended("a delete or a recreation", id)
}

// Two reverts or failed starts within one identity pause it fleet-wide, with one audit row, actor Canyon;
// one stalls it. A paused identity starts no patch run, and after an admin's resume its breaker counts
// afresh (design/PATCHING.md §4.2).
func TestBreakerTwoRevertsPauseIdentity(t *testing.T) {
	p, ctx, f := patching(t)
	identity := fakeRunImage
	matchedRunImage(t, ctx, p, identity)
	if _, err := p.Exec(ctx, "INSERT INTO patch_identities (id, run_image) VALUES ($1, $1)", identity); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"rev-a", "rev-b", "rev-c", "rev-d"} {
		storedAs(t, ctx, p, nil, app, sha('a'))
		if _, err := p.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome) VALUES ($1, $2, $3, 1, 'running')", identity, app, "canyon/"+app+"@"+sha('a')); err != nil {
			t.Fatal(err)
		}
	}
	breaker := func() (stalled, paused []string) {
		t.Helper()
		if err := p.QueryRow(ctx, "SELECT stalled_projects, paused_projects FROM patch_identities WHERE id = $1", identity).Scan(&stalled, &paused); err != nil {
			t.Fatal(err)
		}
		return stalled, paused
	}
	pauses := func() []string {
		t.Helper()
		rows, err := p.Query(ctx, "SELECT actor_kind || ' ' || (detail->>'identity') || ' ' || (detail->'projects')::text FROM audit_events WHERE action = 'pause patching'")
		if err != nil {
			t.Fatal(err)
		}
		return must(pgx.CollectRows(rows, pgx.RowTo[string]))
	}
	if err := pipe.RecordPatchOutcome(ctx, identity, "rev-a", PatchReverted); err != nil {
		t.Fatal(err)
	}
	if stalled, paused := breaker(); !slices.Equal(stalled, []string{"rev-a"}) || paused != nil || len(pauses()) != 0 {
		t.Fatalf("one revert: stalled %v, paused %v, %v", stalled, paused, pauses())
	}
	if err := pipe.RecordPatchOutcome(ctx, identity, "rev-b", PatchFailedStart); err != nil {
		t.Fatal(err)
	}
	if stalled, paused := breaker(); !slices.Equal(paused, []string{"rev-a", "rev-b"}) || stalled != nil || !slices.Equal(pauses(), []string{`canyon ` + identity + ` ["rev-a", "rev-b"]`}) {
		t.Fatalf("a revert and a failed start: paused %v, the stall it replaces %v (#2595), audit %v", paused, stalled, pauses())
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM patch_identities WHERE id = '"+identity+"' AND stalled_at IS NULL AND paused_at IS NOT NULL"); n != 1 {
		t.Fatalf("the pause clears the stall's time too: %d", n)
	}
	if err := pipe.RecordPatchOutcome(ctx, identity, "rev-c", PatchReverted); err != nil || len(pauses()) != 1 {
		t.Fatalf("a third, already paused: %d pause rows (%v)", len(pauses()), err)
	}
	runningPreview(t, ctx, p, f, "rev-e", sha('a'))
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 || count(t, ctx, p, "SELECT count(*) FROM patch_wave_apps WHERE project_id = 'rev-e' AND outcome = 'planned'") != 1 {
		t.Fatalf("a paused identity's plan: %d started (%v), want rev-e planned and none started", n, err)
	}
	if _, err := p.Exec(ctx, "UPDATE patch_wave_apps SET wave = 0 WHERE project_id = 'rev-e'"); err != nil { // due before the pause landed
		t.Fatal(err)
	}
	if why, err := pipe.StartPatch(ctx, "rev-e", Patch{Identity: identity, RunImage: identity, Image: "canyon/rev-e@" + sha('a')}); err != nil || why != WhyMoved {
		t.Fatalf("a patch run once its identity paused: %s (%v)", why, err)
	}
	if _, err := p.Exec(ctx, "UPDATE patch_identities SET paused_at = NULL, paused_projects = NULL, stalled_at = NULL, stalled_projects = NULL, counted_since = now() WHERE id = $1", identity); err != nil {
		t.Fatal(err)
	}
	if err := pipe.RecordPatchOutcome(ctx, identity, "rev-d", PatchReverted); err != nil {
		t.Fatal(err)
	}
	if stalled, paused := breaker(); !slices.Equal(stalled, []string{"rev-d"}) || paused != nil {
		t.Fatalf("after a resume, one revert — those before it not counted: stalled %v, paused %v", stalled, paused)
	}
}

// A pause ends every other patch run of the identity before its /run, pending or under way,
// failed/interrupted, its workflow cancelled: a wave already queued goes no further (design/PATCHING.md
// §4.2, "pause it fleet-wide").
func TestAPauseEndsTheIdentitysQueuedPatchRuns(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	if _, err := p.Exec(ctx, "INSERT INTO patch_identities (id, run_image, wave) VALUES ($1, $1, 1)", fakeRunImage); err != nil {
		t.Fatal(err)
	}
	apps := []string{"wave-a", "wave-b", "wave-c"}
	for _, app := range apps {
		runningPreview(t, ctx, p, f, app, sha('a'))
		if _, err := p.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave) VALUES ($1, $2, $3, 1)", fakeRunImage, app, "canyon/"+app+"@"+sha('a')); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 3 {
		t.Fatalf("the wave's patch runs: %d started (%v)", n, err)
	}
	queued, _, _ := patchRun(t, ctx, p, "wave-c")
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "wave-a", PatchReverted); err != nil {
		t.Fatal(err)
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "wave-b", PatchFailedStart); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := patchRun(t, ctx, p, "wave-c"); state != "failed" || reason != "interrupted" {
		t.Fatalf("the queued patch run after the pause: %s %s", state, reason)
	}
	if st := settled(t, queued); st != dbos.WorkflowStatusCancelled {
		t.Fatalf("the queued patch run's workflow after the pause: %s", st)
	}
}

// The image scans after a deploy or a tick never wait on a patch run: they have a queue of their own, so a
// patch run holding every slot of the patches queue keeps none of them (design/PATCHING.md §3.1: production
// waits the scan's length, never an interval).
func TestAnImageSBOMNeverWaitsOnAPatchRun(t *testing.T) {
	p, ctx, f := patching(t)
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	rematching(t, f, g)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "patchy", sha('a'))
	startedPatch(t, ctx, p, "patchy") // its workflow waits (seeded), holding the patches queue's one slot
	tick(t, time.Now())               // reads patchy's image, which no scan has read
	if asked := imageScans(t, "patchy", sha('a')); len(asked) != 1 || asked[0].Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("the image's SBOM behind a patch run: %+v", asked)
	}
}

// plannedCanary is the patch a plan made of app's preview: its identity's canary, due now.
func plannedCanary(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake, app string) Patch {
	t.Helper()
	runningPreview(t, ctx, p, f, app, sha('a'))
	image := "canyon/" + app + "@" + sha('a')
	if _, err := p.Exec(ctx, "INSERT INTO patch_identities (id, run_image) VALUES ($1, $1) ON CONFLICT DO NOTHING", fakeRunImage); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave) VALUES ($1, $2, $3, 0)", fakeRunImage, app, image); err != nil {
		t.Fatal(err)
	}
	return Patch{Identity: fakeRunImage, RunImage: fakeRunImage, Image: image}
}

// A patch run's start composes the project's policy on its own transaction, under the ledger lock it holds:
// never through another connection of the pool, which a person's start waiting on that lock may have taken
// the last of — then neither would ever move (#2379's review, R2-1). With a pool of two, one held by such a
// waiter, the start still goes through.
func TestAPatchStartComposesItsPolicyOnItsOwnTransaction(t *testing.T) {
	p, ctx, f := patching(t)
	pl := plannedCanary(t, ctx, p, f, "pooled")
	cfg := p.Config().Copy()
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	poolWas, policyWas := pipe.Pool, pipe.ProjectPolicy.Load()
	t.Cleanup(func() { pipe.Pool = poolWas; pipe.ProjectPolicy.Store(policyWas) })
	waited := make(chan error, 1)
	pipe.Pool = small
	pipe.ProjectPolicy.Store(func(ctx context.Context, q Querier, project string) (policy.Snapshot, error) {
		held, err := small.Acquire(ctx) // the pool's other connection, now a start waiting on the ledger lock
		if err != nil {
			return policy.Snapshot{}, err
		}
		waiting := context.WithoutCancel(ctx) // it outlives the start's call
		go func() {
			defer held.Release()
			waited <- pgx.BeginFunc(waiting, held, func(tx pgx.Tx) error { return events.LockTx(waiting, tx) })
		}()
		var one int
		if err := q.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return policy.Snapshot{}, err
		}
		return platformOn(ctx, q, project)
	})
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if why, err := pipe.StartPatch(bounded, "pooled", pl); err != nil || why != WhyStarted {
		t.Fatalf("a patch start with the pool's last connection waiting on its lock: %s (%v)", why, err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the waiter, once the start let go of the lock: %v", err)
	}
}

// A patch run starts only while the project's platform.known-vuln is on, read again as it starts: an admin
// who turned it off since the plan stops it (design/PATCHING.md §3.3).
func TestAPatchStartRereadsTheRule(t *testing.T) {
	p, ctx, f := patching(t)
	pl := plannedCanary(t, ctx, p, f, "ruled")
	was := pipe.ProjectPolicy.Load()
	t.Cleanup(func() { pipe.ProjectPolicy.Store(was) })
	pipe.ProjectPolicy.Store(func(context.Context, Querier, string) (policy.Snapshot, error) { return testPolicy(), nil }) // the rule off
	if why, err := pipe.StartPatch(ctx, "ruled", pl); err != nil || why != WhyRuleOff {
		t.Fatalf("a patch run with its rule turned off: %s (%v)", why, err)
	}
	pipe.ProjectPolicy.Store(was)
	if why, err := pipe.StartPatch(ctx, "ruled", pl); err != nil || why != WhyStarted {
		t.Fatalf("the rule on again: %s (%v)", why, err)
	}
}

// A revert and a failed start stay as they ended for their identity: a redeploy of the app plans it again
// for no patch of that identity, so the breaker counts it and a second revert pauses the identity, with one
// audit row. An app whose watch ended early is planned again, in no wave yet and never as the canary
// (design/PATCHING.md §4.2).
func TestARedeployNeverUndoesARevert(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	if _, err := p.Exec(ctx, "INSERT INTO patch_identities (id, run_image, wave) VALUES ($1, $1, 1)", fakeRunImage); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"redo-a", "redo-b", "redo-c"} {
		runningPreview(t, ctx, p, f, app, sha('a'))
		if _, err := p.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome) VALUES ($1, $2, $3, 1, 'running')", fakeRunImage, app, "canyon/"+app+"@"+sha('a')); err != nil {
			t.Fatal(err)
		}
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "redo-c", PatchEndedEarly); err != nil {
		t.Fatal(err)
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "redo-a", PatchReverted); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1 WHERE project_id IN ('redo-a', 'redo-c') AND env = 'preview'", "canyon/redo-x@"+sha('b')); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("the reverted app, redeployed: %d started (%v)", n, err)
	}
	var outcome string
	if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = 'redo-a'").Scan(&outcome); err != nil || outcome != PatchReverted {
		t.Fatalf("the reverted app's patch after a redeploy: %q (%v)", outcome, err)
	}
	var wave *int
	var noCanary bool
	if err := p.QueryRow(ctx, "SELECT outcome, wave, no_canary FROM patch_wave_apps WHERE project_id = 'redo-c'").Scan(&outcome, &wave, &noCanary); err != nil ||
		outcome != PatchPlanned || wave != nil || !noCanary {
		t.Fatalf("the app whose watch ended early, redeployed: %q, wave %v, no canary %v (%v); want planned again, in no wave yet, never the canary", outcome, wave, noCanary, err)
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "redo-b", PatchReverted); err != nil {
		t.Fatal(err)
	}
	var paused []string
	var pauses int
	if err := p.QueryRow(ctx, "SELECT paused_projects, (SELECT count(*) FROM audit_events WHERE action = 'pause patching') FROM patch_identities WHERE id = $1", fakeRunImage).Scan(&paused, &pauses); err != nil ||
		!slices.Equal(paused, []string{"redo-a", "redo-b"}) || pauses != 1 {
		t.Fatalf("the second revert: paused %v, %d pause row(s) (%v)", paused, pauses, err)
	}
}

// The canary is the first app in canary order whose preview runs: a stopped one holds no wave, so the next is
// the canary; a canary whose watch ended early is replaced, at the next tick, by the next app in canary
// order, which may be one whose preview runs again (design/PATCHING.md §4.2).
func TestTheCanaryIsReplacedNeverWaitedOn(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	apps := []string{"canary-a", "canary-b", "canary-c"}
	slices.SortFunc(apps, func(x, y string) int { return strings.Compare(canaryOrder(x), canaryOrder(y)) })
	for _, app := range apps {
		storedAs(t, ctx, p, nil, app, sha('a'))
		if app != apps[0] {
			f.Start(app, "preview")
		}
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("with the first in canary order stopped: %d started (%v)", n, err)
	}
	if id, _, _ := patchRun(t, ctx, p, apps[1]); id == "" {
		t.Fatalf("the canary is not %s, the next in canary order whose preview runs", apps[1])
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, apps[1], PatchEndedEarly); err != nil {
		t.Fatal(err)
	}
	endAll(t, ctx, p, apps[1])
	f.Start(apps[0], "preview")
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("after the canary's watch ended early: %d started (%v)", n, err)
	}
	if id, _, _ := patchRun(t, ctx, p, apps[0]); id == "" {
		t.Fatalf("the replacement canary is not %s", apps[0])
	}
}

// A rebase refused for what the images are is permanent for that pair, the image's digest and the run
// image the plan named: the plan never plans it again while the environment runs another image, nor when
// a deploy brings the refused image back, and the app is out of its wave. run_image_moved is not such a
// refusal (#2089; design/PATCHING.md §4.2).
func TestARefusedPairIsNeverPlannedAgain(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "refused-a", sha('a'))
	if err := RecordPatchRefusal(ctx, p, fakeRunImage, "refused-a", sha('a'), fakeRunImage, "run_image_moved"); err == nil {
		t.Fatal("run_image_moved was recorded as a refusal for good")
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the first plan: %d started (%v)", n, err)
	}
	if err := RecordPatchRefusal(ctx, p, fakeRunImage, "refused-a", sha('a'), fakeRunImage, "another_run_image"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'failed', ended_at = now() WHERE project_id = 'refused-a' AND kind = 'patch'"); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a refused pair planned again: %d started (%v)", n, err)
	}
	if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = 'refused-a'").Scan(&outcome); err != nil || outcome != PatchRefused {
		t.Fatalf("the refused app's patch: %q (%v)", outcome, err)
	}
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1 WHERE project_id = 'refused-a' AND env = 'preview'", "canyon/refused-a@"+sha('b')); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("another image in preview: %d started (%v)", n, err)
	}
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, "refused-a", PatchPassed); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE project_id = 'refused-a' AND kind = 'patch' AND ended_at IS NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1 WHERE project_id = 'refused-a' AND env = 'preview'", "canyon/refused-a@"+sha('a')); err != nil { // the refused image, reused
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("the refused image back in preview planned again: %d started (%v)", n, err)
	}
}

// The plan rebases nothing onto a run image whose match it cannot read: none stored, one stored for a run
// image GET /builder no longer names, a stale one or one dated in the future; nor while the executor names
// no run image, the project is being deleted, or sign-in is off (design/PATCHING.md §3.2, §4.5).
func TestThePlanFailsClosedOnTheRunImage(t *testing.T) {
	p, ctx, f := patching(t)
	runningPreview(t, ctx, p, f, "closed-a", sha('a'))
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("no run image stored: %d started (%v)", n, err)
	}
	matchedRunImage(t, ctx, p, sha('d'))
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a run image GET /builder no longer names: %d started (%v)", n, err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM part_matches"); err != nil {
		t.Fatal(err)
	}
	matchedRunImage(t, ctx, p, fakeRunImage)
	if _, err := p.Exec(ctx, "UPDATE part_matches SET db_built_at = now() - interval '6 days'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a stale run image match: %d started (%v)", n, err)
	}
	if _, err := p.Exec(ctx, "UPDATE part_matches SET db_built_at = now() + interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a run image match from the future: %d started (%v)", n, err)
	}
	if _, err := p.Exec(ctx, "UPDATE part_matches SET db_built_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	f.NoBuilder.Store(true)
	if n, err := pipe.planPatches(ctx); err == nil || n != 0 {
		t.Fatalf("an executor that names no run image: %d started (%v)", n, err)
	}
	f.NoBuilder.Store(false)
	if _, err := p.Exec(ctx, "UPDATE projects SET deleting_since = now() WHERE id = 'closed-a'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a project being deleted: %d started (%v)", n, err)
	}
	if _, err := p.Exec(ctx, "UPDATE projects SET deleting_since = NULL WHERE id = 'closed-a'"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE part_matches SET db_built_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("a fresh match: %d started (%v)", n, err)
	}
	pipe.Patches.Store(false)
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("sign-in off: %d started (%v)", n, err)
	}
}

// The rescan's sweep keeps the scan of the image a patch run rebased while the run is pending or running — no
// deploy row names it between the run's step 3 and its deploy — and drops it once the run ended, clean or failed,
// as it drops any image nothing runs or keeps (#2587; design/PATCHING.md §4.3).
func TestTheSweepKeepsAPatchRunsRebasedImage(t *testing.T) {
	p, ctx, _ := patching(t)
	img, err := imageOf(loadGolden(t).Image)
	if err != nil {
		t.Fatal(err)
	}
	storedAs(t, ctx, p, nil, "swept", sha('a')) // preview runs sha('a'); sha('b') is what its patch run rebased
	if err := storeImage(ctx, p, "swept", sha('b'), img); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, rebased_image) VALUES ('gr_sweptpatch', 'swept', 'pkg_swept', 'running', 'patch', $1)`,
		"canyon/swept@"+sha('b')); err != nil {
		t.Fatal(err)
	}
	stored := func() (scan bool, parts int) {
		t.Helper()
		if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scanned_images WHERE project_id = 'swept' AND digest = $1),
			(SELECT count(*) FROM layer_parts s JOIN image_layers l ON l.part_sha = s.part_sha WHERE l.project_id = 'swept' AND l.digest = $1)`, sha('b')).Scan(&scan, &parts); err != nil {
			t.Fatal(err)
		}
		return scan, parts
	}
	if scan, parts := stored(); !scan || parts == 0 {
		t.Fatalf("before the sweep: scan %t, %d slices", scan, parts)
	}
	for _, state := range []string{"running", "pending"} {
		if _, err := p.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = 'gr_sweptpatch'", state); err != nil {
			t.Fatal(err)
		}
		if err := pipe.retain(ctx); err != nil {
			t.Fatal(err)
		}
		if scan, parts := stored(); !scan || parts == 0 {
			t.Fatalf("a sweep while the patch run is %s dropped its rebased image: scan %t, %d slices", state, scan, parts)
		}
	}
	for _, end := range []string{"failed", "clean"} {
		if _, err := p.Exec(ctx, "UPDATE runs SET state = $1, ended_at = now() WHERE id = 'gr_sweptpatch'", end); err != nil {
			t.Fatal(err)
		}
		if err := storeImage(ctx, p, "swept", sha('b'), img); err != nil { // stored again for this end's sweep
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, "UPDATE scanned_images SET seen_at = now() - interval '1 minute' WHERE project_id = 'swept'"); err != nil {
			t.Fatal(err)
		}
		if err := pipe.retain(ctx); err != nil {
			t.Fatal(err)
		}
		if scan, _ := stored(); scan {
			t.Fatalf("a sweep after the patch run ended %s kept its rebased image", end)
		}
	}
}

// logged is what the log says while it is captured, guarded: a workflow's goroutine may log meanwhile.
type logged struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// saidTick is one tick's plan: how many patch runs it started, and the one line it logged of it.
func saidTick(t *testing.T, ctx context.Context) (int, string) {
	t.Helper()
	out, flags := log.Writer(), log.Flags()
	l := &logged{}
	log.SetOutput(l)
	log.SetFlags(0)
	n, err := pipe.planPatches(ctx)
	log.SetOutput(out)
	log.SetFlags(flags)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.HasPrefix(line, "runs: the patch plan onto ") || strings.HasPrefix(line, "runs: the patch plan started ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("a tick logged %d lines of its plan, want one: %q", len(lines), lines)
	}
	return n, lines[0]
}

// A tick's plan logs one line: the patch runs it started, and why each other app it read started none, each
// cause its own; before it reads an app, why it read none. A cause it does not know is never read as nothing
// to do (#2575; design/PATCHING.md §4.1-4.2).
func TestPatchTickSaysWhyNothingWasPlanned(t *testing.T) {
	p, ctx, f := patching(t)
	pipe.Patches.Store(false)
	if n, line := saidTick(t, ctx); n != 0 || line != "runs: the patch plan started no patch run: this server does not patch: CANYON_PATCHING is not on, or sign-in is off" {
		t.Fatalf("patching off on the server: %d, %s", n, line)
	}
	pipe.Patches.Store(true)
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET patching = 'off'"); err != nil {
		t.Fatal(err)
	}
	if n, line := saidTick(t, ctx); n != 0 || line != "runs: the patch plan started no patch run: the org's patching is off" {
		t.Fatalf("the org's patching off: %d, %s", n, line)
	}
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET patching = 'ask'"); err != nil {
		t.Fatal(err)
	}
	onto := "runs: the patch plan onto " + fakeRunImage + " started "
	if n, line := saidTick(t, ctx); n != 0 || line != onto+"no patch run: no app has a preview" {
		t.Fatalf("no preview: %d, %s", n, line)
	}

	high := libssl3("high", "CVE-2024-6119")
	reads := map[string]PlatformImage{
		"nofix":  {Checked: true, Findings: []PlatformFinding{{Name: "libbrotli1", Version: "1.1.0-2build2", Vulnerability: "CVE-2025-6176", Fixed: "1.1.0-2ubuntu0.1", Class: "os", Severity: "high"}}},
		"low":    {Checked: true, Findings: []PlatformFinding{libssl3("medium", "CVE-2024-6119")}},
		"unread": {Why: "could not check: the image has not been matched yet"},
		"npm":    {Checked: true, Findings: []PlatformFinding{{Name: "npm", Version: "10.8.2", Vulnerability: "CVE-2024-1", Fixed: "10.9.0", Class: "runtime", Severity: "high"}}},
	}
	readWas, policyWas := pipe.PlatformRead.Load(), pipe.ProjectPolicy.Load()
	t.Cleanup(func() { pipe.PlatformRead.Store(readWas); pipe.ProjectPolicy.Store(policyWas) })
	pipe.PlatformRead.Store(func(_ context.Context, project, _ string) (PlatformImage, error) {
		if read, ok := reads[project]; ok {
			return read, nil
		}
		return PlatformImage{Checked: true, Findings: []PlatformFinding{high}}, nil
	})
	pipe.ProjectPolicy.Store(func(ctx context.Context, q Querier, project string) (policy.Snapshot, error) {
		switch project {
		case "ruleoff":
			return testPolicy(), nil
		case "uncomposed":
			return policy.Snapshot{}, errors.New("the policy does not compose")
		}
		return platformOn(ctx, q, project)
	})
	probes := []string{"probe-a", "probe-b"}
	slices.SortFunc(probes, func(x, y string) int { return strings.Compare(canaryOrder(x), canaryOrder(y)) })
	canary, next := probes[0], probes[1]
	for _, app := range []string{"probe-a", "probe-b", "nofix", "low", "unread", "npm", "ruleoff", "uncomposed", "refused"} {
		runningPreview(t, ctx, p, f, app, sha('a'))
	}
	storedAs(t, ctx, p, nil, "stopped", sha('a'))
	if n, line := saidTick(t, ctx); n != 0 || !strings.HasPrefix(line, onto+"0 patch run(s): ") ||
		!strings.Contains(line, "5 not, the run image's own scan could not be read (nofix, probe-a, probe-b, refused, stopped)") {
		t.Fatalf("no run image match stored: %d, %s", n, line)
	}

	matchedRunImage(t, ctx, p, fakeRunImage)
	if err := RecordPatchRefusal(ctx, p, fakeRunImage, "refused", sha('a'), fakeRunImage, "another_run_image"); err != nil {
		t.Fatal(err)
	}
	want := onto + "1 patch run(s) (" + canary + ")" +
		": 1 not, a rebase of its image onto this run image was refused for good (refused)" +
		"; 1 not, it waits for its wave (" + next + ")" +
		"; 1 not, its counted findings are in runtimes or build tools, which a rebase does not fix (npm)" +
		"; 1 not, its image's read could not check (unread)" +
		"; 1 not, its policy could not be composed (uncomposed)" +
		"; 1 not, its preview is not running (stopped)" +
		"; 1 not, no counted finding at high or above (low)" +
		"; 1 not, platform.known-vuln is off (ruleoff)" +
		"; 1 not, the run image fixes none of its counted findings (nofix)"
	if n, line := saidTick(t, ctx); n != 1 || line != want {
		t.Fatalf("the fleet's plan: %d\n got %s\nwant %s", n, line, want)
	}
	if n, line := saidTick(t, ctx); n != 0 || !strings.Contains(line, "1 not, a patch run of it is under way ("+canary+")") {
		t.Fatalf("the canary's patch run under way: %d, %s", n, line)
	}

	// the canary reverts: it is not retried, and its stall holds the next wave until an admin resumes it
	if err := pipe.RecordPatchOutcome(ctx, fakeRunImage, canary, PatchReverted); err != nil {
		t.Fatal(err)
	}
	endAll(t, ctx, p, canary)
	if n, line := saidTick(t, ctx); n != 0 || !strings.Contains(line, "1 not, its patch onto this run image reverted, and is not retried ("+canary+")") ||
		!strings.Contains(line, "1 not, it waits for its wave, which a stall holds until an admin resumes it ("+next+")") {
		t.Fatalf("after the canary reverted: %d, %s", n, line)
	}
	if _, err := p.Exec(ctx, "UPDATE patch_identities SET paused_at = now(), paused_projects = ARRAY[$2::text] WHERE id = $1", fakeRunImage, canary); err != nil {
		t.Fatal(err)
	}
	if n, line := saidTick(t, ctx); n != 0 || !strings.Contains(line, "1 not, patching onto this run image is paused until an admin resumes it ("+next+")") {
		t.Fatalf("a paused identity: %d, %s", n, line)
	}

	// resumed, the next app is the canary, behind a person's run of it
	if _, err := p.Exec(ctx, "UPDATE patch_identities SET paused_at = NULL, paused_projects = NULL, stalled_at = NULL, stalled_projects = NULL, counted_since = now() WHERE id = $1", fakeRunImage); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) VALUES ('person_' || $1, $1, 'pkg_' || $1, 'pending', 'gate')", next); err != nil {
		t.Fatal(err)
	}
	if n, line := saidTick(t, ctx); n != 0 || !strings.Contains(line, "1 not, another run of it goes first ("+next+")") {
		t.Fatalf("a person's run first: %d, %s", n, line)
	}
}

// keptSaid is the patch_plan_said row: the identity, why the plan read no app, and each app's cause.
func keptSaid(t *testing.T, ctx context.Context, p *pgxpool.Pool) (identity, why string, apps map[string]string) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT coalesce(identity, ''), coalesce(why, ''), apps FROM patch_plan_said").Scan(&identity, &why, &apps); err != nil {
		t.Fatal(err)
	}
	return identity, why, apps
}

// Each tick keeps what its plan said for the identities read, in the contract's words, replacing the last: why it
// read no app, or the identity it planned onto and each app's cause; a plan that fails keeps no app's cause, its
// own or a tick's before it (#2588; contracts/routes.md, the identities row's plan).
func TestATickKeepsWhatItSaid(t *testing.T) {
	p, ctx, f := patching(t)
	pipe.Patches.Store(false)
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("patching off on the server: %d (%v)", n, err)
	}
	if identity, why, apps := keptSaid(t, ctx, p); identity != "" || why != "server_off" || len(apps) != 0 {
		t.Fatalf("patching off on the server kept %q %q %v", identity, why, apps)
	}
	pipe.Patches.Store(true)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "nofix", sha('a'))
	readWas := pipe.PlatformRead.Load()
	t.Cleanup(func() { pipe.PlatformRead.Store(readWas) })
	pipe.PlatformRead.Store(func(context.Context, string, string) (PlatformImage, error) {
		return PlatformImage{Checked: true, Findings: []PlatformFinding{{Name: "libbrotli1", Version: "1.1.0-2build2", Vulnerability: "CVE-2025-6176", Fixed: "1.1.0-2ubuntu0.1", Class: "os", Severity: "high"}}}, nil
	})
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("an app the run image fixes nothing of: %d (%v)", n, err)
	}
	if identity, why, apps := keptSaid(t, ctx, p); identity != fakeRunImage || why != "" || len(apps) != 1 || apps["nofix"] != "no_fix" {
		t.Fatalf("the plan onto %s kept %q %q %v", fakeRunImage, identity, why, apps)
	}
	f.NoBuilder.Store(true)
	t.Cleanup(func() { f.NoBuilder.Store(false) })
	if _, err := pipe.planPatches(ctx); err == nil {
		t.Fatal("a plan with no run image named did not fail")
	}
	if identity, why, apps := keptSaid(t, ctx, p); identity != "" || why != "failed" || len(apps) != 0 {
		t.Fatalf("a failed plan kept %q %q %v", identity, why, apps)
	}
}

// A tick that reaches no plan keeps why all the same, so Settings never reads the causes of a tick before it under its
// time (#2588, the review of #2607): once platform.known-vuln is off on every app, the next tick reads rule_off, no
// identity and no app.
func TestATickWithTheRuleOffEverywhereKeepsNoCause(t *testing.T) {
	p, ctx, f := patching(t)
	matchedRunImage(t, ctx, p, fakeRunImage)
	runningPreview(t, ctx, p, f, "nofix", sha('a'))
	readWas, policyWas := pipe.PlatformRead.Load(), pipe.ProjectPolicy.Load()
	t.Cleanup(func() { pipe.PlatformRead.Store(readWas); pipe.ProjectPolicy.Store(policyWas) })
	pipe.PlatformRead.Store(func(context.Context, string, string) (PlatformImage, error) {
		return PlatformImage{Checked: true, Findings: []PlatformFinding{{Name: "libbrotli1", Version: "1.1.0-2build2", Vulnerability: "CVE-2025-6176", Fixed: "1.1.0-2ubuntu0.1", Class: "os", Severity: "high"}}}, nil
	})
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("the tick before: %d (%v)", n, err)
	}
	if identity, _, apps := keptSaid(t, ctx, p); identity != fakeRunImage || apps["nofix"] != "no_fix" {
		t.Fatalf("the tick before kept %q %v", identity, apps)
	}
	pipe.ProjectPolicy.Store(func(context.Context, Querier, string) (policy.Snapshot, error) { return testPolicy(), nil }) // the rule off on every app
	tick(t, time.Now())
	if identity, why, apps := keptSaid(t, ctx, p); identity != "" || why != "rule_off" || len(apps) != 0 {
		t.Fatalf("the tick with the rule off on every app kept %q %q %v", identity, why, apps)
	}
}
