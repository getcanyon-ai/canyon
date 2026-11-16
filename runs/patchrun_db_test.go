//go:build !noee

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// imageReads is each image's read by digest, which a test changes while a patch run reads it.
type imageReads struct {
	mu    sync.Mutex
	reads map[string][]PlatformFinding
}

// set makes digest's read found, or one that could not check for found nil.
func (r *imageReads) set(digest string, found []PlatformFinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if found == nil {
		delete(r.reads, digest)
		return
	}
	r.reads[digest] = found
}

// patchable is claims-portal's preview as dana's deploy run left it, on a host that plans its patch: sign-in on,
// patching on, the run image stored and matched, and each image's read what reads names for its digest — the
// deployed image's one high libssl3 finding the run image fixes, the rebased image's none — and any other
// image's read one that could not check.
func patchable(t *testing.T) (*pgxpool.Pool, context.Context, *executortest.Fake, *imageReads) {
	t.Helper()
	return patchableAfter(t, nil)
}

// patchableAfter is patchable, with before run ahead of dana's deploy.
func patchableAfter(t *testing.T, before func(ctx context.Context, p *pgxpool.Pool)) (*pgxpool.Pool, context.Context, *executortest.Fake, *imageReads) {
	t.Helper()
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if before != nil {
		before(ctx, p)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("dana's deploy: %s %s", state, reasonOf(reason))
	}
	matchedRunImage(t, ctx, p, fakeRunImage)
	// the patched image's scan and its layers' matches, on one database: a patch run compares the two images'
	// matched layers, and with none matched there is no comparison (oneDatabase). Its database is newer than the
	// run image's above, which shares the system libraries' slices, so the step's match is each slice's newest.
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	rematchingOn(t, f, g, time.Now().Add(-30*time.Minute))
	reads := &imageReads{reads: map[string][]PlatformFinding{executortest.Digest: {libssl3("high", "CVE-2024-6119")}, ImageDigest(executortest.RebasedOf(project, fakeRunImage).Image): {}}}
	readWas := pipe.PlatformRead.Load()
	pipe.Patches.Store(true)
	pipe.PlatformRead.Store(func(_ context.Context, _, digest string) (PlatformImage, error) {
		reads.mu.Lock()
		defer reads.mu.Unlock()
		found, ok := reads.reads[digest]
		if !ok {
			return PlatformImage{Why: "could not check: the image has not been matched yet"}, nil
		}
		return PlatformImage{Checked: true, Findings: found}, nil
	})
	t.Cleanup(func() {
		pipe.Patches.Store(false)
		pipe.PlatformRead.Store(readWas)
		rows, err := p.Query(context.Background(), "SELECT id FROM runs WHERE kind = 'patch'") // none outlives its test to hold the patches queue's slot
		if err != nil {
			t.Error(err)
			return
		}
		for _, id := range must(pgx.CollectRows(rows, pgx.RowTo[string])) {
			Cancel(durable, id)
		}
	})
	return p, ctx, f, reads
}

// holdsAt answers the patched image's scan with the golden's, its package name at version: what a patch put there.
func holdsAt(t *testing.T, f *executortest.Fake, name, version string) {
	t.Helper()
	res := loadGolden(t).Image
	var sbom map[string]any
	if err := json.Unmarshal(res.SBOM, &sbom); err != nil {
		t.Fatal(err)
	}
	for _, c := range sbom["components"].([]any) {
		if c := c.(map[string]any); c["name"] == name {
			c["version"] = version
		}
	}
	res.SBOM = must(json.Marshal(sbom))
	f.ImageAnswer.Store(&res)
}

// patched plans claims-portal's patch and answers its run once it ended, with its state and failure reason.
func patched(t *testing.T, ctx context.Context, p *pgxpool.Pool) (run, state, reason string) {
	t.Helper()
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	run, _, _ = patchRun(t, ctx, p, project)
	// the run's end is the last thing its watch writes (watch.go, finish), through pipe's pool, and this reads it
	// through that pool: every read the run and its watch made of a pipe field is ordered before the test sets it
	// back, which -race checks (#2667)
	s, r := waitEnded(t, ctx, p, run)
	return run, s, reasonOf(r)
}

// waitOn is a hold the fake waits on until release, which the test's end calls too: a request still waiting
// would hold the fake's Close.
func waitOn(t *testing.T, on *atomic.Pointer[chan struct{}]) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	on.Store(&ch)
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// previewRow is preview's image, gate run and deployed_by subject.
func previewRow(t *testing.T, ctx context.Context, p *pgxpool.Pool) (image, gateRun, by string) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT image, gate_run, deployed_by_subject FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image, &gateRun, &by); err != nil {
		t.Fatal(err)
	}
	return image, gateRun, by
}

// A patch run proves only the run image changed, finds what it runs with unchanged and the planned finding
// gone with nothing new, then /runs the rebased image keeping the one it replaces. Preview's row takes the
// image, the patch run and what the /run carried, and keeps its package and deployed_by; one audit row,
// actor Canyon, says what it fixed. Its author is dana, whose deploy it rebased. Its watch passes, and the app's
// patch reads passed (contracts/routes.md, Patch runs).
func TestPatchRunDeploysToPreview(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	_, deployRun, _ := previewRow(t, ctx, p)
	holdsAt(t, f, "libssl3", "3.0.13-0ubuntu3.6") // newer than the advisory's 3.0.13-0ubuntu3.5: to is what the image holds (#2631)
	run, state, reason := patched(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the patch run: %s %s, lines %q", state, reason, lastLines(t, ctx, p, run))
	}
	want := []string{
		"Only the system libraries Canyon adds changed: 7d3e8f's own code, its build settings and how it starts are the same",
		"What 7d3e8f runs with is unchanged since it was deployed",
		"Fixed: libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.6 (CVE-2024-6119)",
		"No new vulnerability at high or above",
	}
	got := lastLines(t, ctx, p, run) // then its watch's: watching, and passed at its end
	if len(got) != len(want)+2 || !slices.Equal(got[:len(want)], want) || !strings.HasPrefix(got[len(want)], "Watching preview until ") ||
		got[len(want)+1] != "The watch passed: the patched app neither exited nor restarted" {
		t.Fatalf("its lines:\n got %q\nwant %q, then watching and passed", got, want)
	}
	rebased := executortest.RebasedOf(project, fakeRunImage).Image
	ran := f.Ran()
	if ran.Image != rebased || !slices.Contains(ran.Keep, executortest.Digest) || f.Rebases.Load() != 1 {
		t.Fatalf("the /run: %s keeping %v, %d rebases", ran.Image, ran.Keep, f.Rebases.Load())
	}
	image, gateRun, by := previewRow(t, ctx, p)
	var pkgID string
	var sha *string
	var buildpacks json.RawMessage
	if err := p.QueryRow(ctx, "SELECT package_id, run_config_sha256, buildpacks FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&pkgID, &sha, &buildpacks); err != nil {
		t.Fatal(err)
	}
	if image != rebased || gateRun != run || by != dana.Subject || pkgID != pkg || sha == nil || buildpacks == nil {
		t.Fatalf("preview's row: %s by %s, gate run %s, package %s, fingerprint %v, buildpacks %s", image, by, gateRun, pkgID, sha, buildpacks)
	}
	var started bool // the patch's own container start, after its run began (#2738): never the replaced row's, never none
	if err := p.QueryRow(ctx, `SELECT d.container_started_at IS NOT NULL AND d.container_started_at >= r.created_at FROM deploys d JOIN runs r ON r.id = d.gate_run
		WHERE d.project_id = $1 AND d.env = 'preview'`, project).Scan(&started); err != nil || !started {
		t.Fatalf("preview's row records no container start of the patch's own: %v", err)
	}
	var detail string
	if err := p.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'patch preview' AND actor_kind = 'canyon' AND run_id = $1", run).Scan(&detail); err != nil ||
		!strings.Contains(detail, `"fixed": [{"to": "3.0.13-0ubuntu3.6", "from": "3.0.13-0ubuntu3.4", "name": "libssl3", "vulnerability": "CVE-2024-6119"}]`) ||
		!strings.Contains(detail, `"replaces": "`+executortest.Digest+`"`) || !strings.Contains(detail, `"proof": "layers"`) {
		t.Fatalf("the patch preview audit row: %s (%v)", detail, err)
	}
	var author, steps string
	var recorded *string
	if err := p.QueryRow(ctx, "SELECT author_kind || '/' || author_subject, rebased_image FROM runs WHERE id = $1", run).Scan(&author, &recorded); err != nil || author != "person/"+dana.Subject {
		t.Fatalf("the patch run's author: %q (%v)", author, err)
	}
	if recorded == nil || *recorded != rebased { // the image the sweep keeps while the run reads it (#2587)
		t.Fatalf("the patch run's rebased image: %v, want %s", recorded, rebased)
	}
	if err := p.QueryRow(ctx, `SELECT string_agg((data->>'step') || ':' || (data->>'status'), ',' ORDER BY id) FROM timeline_rows
		WHERE run_id = $1 AND kind = 'progress' AND data->>'step' IN ('rebase', 'image-sbom', 'start', 'watch')`, run).Scan(&steps); err != nil ||
		steps != "rebase:running,rebase:done,image-sbom:running,image-sbom:done,start:running,start:done,watch:running,watch:done" {
		t.Fatalf("its progress: %q (%v)", steps, err)
	}
	var outcome string
	if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = $1", project).Scan(&outcome); err != nil || outcome != PatchPassed {
		t.Fatalf("its outcome: %s (%v)", outcome, err)
	}
	var newest string // the project's gate_state is its newest gate run's: never the patch run
	if err := p.QueryRow(ctx, "SELECT id FROM runs WHERE project_id = $1 AND kind = 'gate' ORDER BY created_at DESC LIMIT 1", project).Scan(&newest); err != nil || newest != deployRun {
		t.Fatalf("the newest gate run: %s, want dana's deploy %s (%v)", newest, deployRun, err)
	}
}

// A patch behind a front door that has not passed the probe ends at step 5 with nothing deployed, D5's line its last —
// never a raw error — and the /run never asked (#2438, the security review).
func TestAPatchBehindAnUnprobedDoorShowsTheLineAndRunsNothing(t *testing.T) {
	p, ctx, f, _ := patchableAfter(t, func(ctx context.Context, p *pgxpool.Pool) {
		onKubernetes(t)
		door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
		if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := p.Exec(ctx, "UPDATE front_doors SET probed_at = NULL"); err != nil {
		t.Fatal(err)
	}
	runs := f.Runs.Load()
	run, state, reason := patched(t, ctx, p)
	if state != "failed" || reason != "error" || f.Runs.Load() != runs {
		t.Fatalf("a patch behind an unprobed door: %s/%s, %d /run(s) after the deploy's %d", state, reason, f.Runs.Load(), runs)
	}
	if lines := lastLines(t, ctx, p, run); len(lines) == 0 || lines[len(lines)-1] != "The front door hasn't passed Canyon's check of this cluster, so Canyon won't run apps behind it. An admin saves it again in Settings › App hosting." {
		t.Fatalf("its last line: %q", lines)
	}
}

// Each failure of a step ends the patch run with nothing deployed, its line the last build.log row, and the
// app's outcome recorded (contracts/routes.md, Patch runs, steps 2 to 6).
func TestAPatchRunThatFailsDeploysNothing(t *testing.T) {
	newHigh := PlatformFinding{Name: "libc6", Version: "2.39-0ubuntu8.4", Vulnerability: "CVE-2025-7", Fixed: "2.39-0ubuntu8.5", Class: "os", Severity: "high"}
	rebased := ImageDigest(executortest.RebasedOf(project, fakeRunImage).Image)
	for _, c := range []struct {
		name                     string
		setup                    func(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake, reads *imageReads)
		reason, notPlatform, out string
		line                     string
	}{
		{"a changed Entrypoint", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			r := executortest.RebasedOf(project, fakeRunImage)
			r.After.Config = "sha256:" + strings.Repeat("9", 64)
			f.Rebased.Store(&r)
		}, "not_platform_only", "layers_changed", PatchFailed, "More than Canyon's system libraries changed: how the app starts changed too, so Canyon did not deploy it"},
		{"a secret set since the deploy", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads) {
			sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-9c1d")
		}, "not_platform_only", "config_changed", PatchFailed, "The secret STRIPE_SECRET_KEY changed since 7d3e8f was deployed: deploy again to apply it, then Canyon patches."},
		{"a row that recorded no fingerprint", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads) {
			if _, err := p.Exec(ctx, "UPDATE deploys SET run_config = NULL, run_config_sha256 = NULL WHERE env = 'preview'"); err != nil {
				t.Fatal(err)
			}
		}, "not_platform_only", "config_changed", PatchFailed, "Its configuration changed since 7d3e8f was deployed: deploy again to apply it, then Canyon patches."},
		{"a row whose run-config does not read", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads) {
			if _, err := p.Exec(ctx, "UPDATE deploys SET run_config = '[]' WHERE env = 'preview'"); err != nil {
				t.Fatal(err)
			}
		}, "not_platform_only", "config_changed", PatchFailed, "Its configuration changed since 7d3e8f was deployed: deploy again to apply it, then Canyon patches."},
		{"a new high", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, _ *executortest.Fake, reads *imageReads) {
			reads.set(rebased, []PlatformFinding{newHigh})
		}, "patch_regressed", "", PatchFailed, "The patched image has libc6 2.39-0ubuntu8.4 (CVE-2025-7, high), which 7d3e8f did not: Canyon did not deploy it"},
		{"the planned finding still there", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, _ *executortest.Fake, reads *imageReads) {
			reads.set(rebased, []PlatformFinding{libssl3("high", "CVE-2024-6119")})
		}, "patch_ineffective", "", PatchFailed, "The patched image still has every package the patch was for: Canyon did not deploy it"},
		{"a read that could not check", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, _ *executortest.Fake, reads *imageReads) {
			reads.set(rebased, nil)
		}, "error", "", PatchFailed, "Canyon could not check the patched image for vulnerabilities: the image has not been matched yet, so it did not deploy it"},
		{"the run image moved", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			moved := "run_image_moved"
			f.RefuseRebase.Store(&moved)
		}, "error", "", PatchFailed, "A newer fix from Canyon came out while it patched 7d3e8f, so Canyon did not deploy it: Canyon tries again at its next check"},
		{"an image not rebasable", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			refused := "not_rebasable"
			f.RefuseRebase.Store(&refused)
		}, "error", "", PatchRefused, "Canyon can't patch 7d3e8f: how it was built doesn't let Canyon swap in new system libraries, so its next deploy picks up the fix"},
		{"a /run that fails", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			f.FailRun = true
		}, "error", "", PatchFailed, "Canyon could not finish patching 7d3e8f, so it did not deploy it"},
		{"an app that exits", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			f.Exited = &executor.Exited{Reason: "the app exited (code 1) before it answered on port 3000", Output: "listening on 3000\nError: libssl.so.3: cannot open\n"}
		}, "start", "", PatchFailedStart, "the app exited (code 1) before it answered on port 3000"},
		{"a rebase the executor answers 404", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			f.FailRebase.Store(404)
		}, "error", "", PatchFailed, "Canyon could not apply its fix to 7d3e8f: executor: /rebase: no such image: the rebase answered 404, so it did not deploy it"},
		{"a rebase the executor answers 502", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads) {
			f.FailRebase.Store(502)
		}, "error", "", PatchFailed, "Canyon could not apply its fix to 7d3e8f: executor: /rebase answered 502: the rebase answered 502, so it did not deploy it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, reads := patchable(t)
			c.setup(t, ctx, p, f, reads)
			run, state, reason := patched(t, ctx, p)
			var notPlatform, said string
			if err := p.QueryRow(ctx, "SELECT coalesce(not_platform_only, ''), coalesce((SELECT data->>'not_platform_only' FROM timeline_rows WHERE run_id = $1 AND kind = 'run.state' AND data->>'state' = 'failed'), '') FROM runs WHERE id = $1",
				run).Scan(&notPlatform, &said); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || reason != c.reason || notPlatform != c.notPlatform || said != c.notPlatform {
				t.Fatalf("%s/%s, not_platform_only %q on its row and %q on its run.state", state, reason, notPlatform, said)
			}
			if lines := lastLines(t, ctx, p, run); len(lines) == 0 || lines[len(lines)-1] != c.line {
				t.Fatalf("its last line: %q, want %q", lines, c.line)
			}
			tried := int32(1) // dana's deploy, and the patch's /run where it got that far
			if c.name == "a /run that fails" || c.name == "an app that exits" {
				tried = 2
			}
			if image, _, _ := previewRow(t, ctx, p); f.Runs.Load() != tried || image != "canyon/"+project+"@"+executortest.Digest {
				t.Fatalf("deployed: %d /runs, preview runs %s", f.Runs.Load(), image)
			}
			var outcome string
			if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = $1", project).Scan(&outcome); err != nil || outcome != c.out {
				t.Fatalf("its outcome: %s (%v), want %s", outcome, err, c.out)
			}
		})
	}
}

// A person's deploy cancels a patch run at its rebase: the patch run ends failed/interrupted and never
// /runs, the person's deploy goes ahead, and the app is planned again (contracts/routes.md, Patch runs).
func TestPersonDeployCancelsPatchAtRebase(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	release := waitOn(t, &f.HoldRebase)
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	patch, _, _ := patchRun(t, ctx, p, project)
	for deadline := time.Now().Add(15 * time.Second); f.Rebases.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the patch run never asked for its rebase")
		}
	}
	person, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatalf("dana's deploy over a patch run at its rebase: %v", err)
	}
	if state, reason := waitEnded(t, ctx, p, patch); state != "failed" || reasonOf(reason) != "interrupted" {
		t.Fatalf("the patch run: %s %s", state, reasonOf(reason))
	}
	release()
	if state, _ := waitEnded(t, ctx, p, person.ID); state != "clean" {
		t.Fatalf("dana's deploy: %s", state)
	}
	if _, gateRun, _ := previewRow(t, ctx, p); gateRun != person.ID || f.Runs.Load() != 2 {
		t.Fatalf("preview's gate run %s, %d /runs: dana's deploy alone ran after the first", gateRun, f.Runs.Load())
	}
	var kinds []string // gate_state reads the newest gate run: the patch run is none
	rows, err := p.Query(ctx, "SELECT kind FROM runs WHERE project_id = $1 ORDER BY created_at", project)
	if err == nil {
		kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || !slices.Equal(kinds, []string{"gate", "patch", "gate"}) {
		t.Fatalf("the runs: %v (%v)", kinds, err)
	}
}

// During the patch's /run a person's start is 409 run_in_progress, and the patch goes ahead (contracts/routes.md,
// Patch runs).
func TestAStartDuringAPatchRunsRunIs409(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	release := waitOn(t, &f.HoldRun)
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	patch, _, _ := patchRun(t, ctx, p, project)
	for deadline := time.Now().Add(15 * time.Second); f.Runs.Load() < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the patch run never reached its /run")
		}
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana}); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("a start during the patch's /run: %v", err)
	}
	release()
	if state, reason := waitEnded(t, ctx, p, patch); state != "clean" {
		t.Fatalf("the patch run: %s %s", state, reasonOf(reason))
	}
}

// An undeploy of preview during the patch's /run is recorded on the patch run, which writes preview, and the run
// stops preview as it ends (contracts/routes.md, The brake): Input.writes, Writes and ShownDeploys agree.
func TestAnUndeployDuringAPatchRunsRunStopsIt(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	release := waitOn(t, &f.HoldRun)
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	patch, _, _ := patchRun(t, ctx, p, project)
	for deadline := time.Now().Add(15 * time.Second); f.Runs.Load() < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the patch run never reached its /run")
		}
	}
	var b Braked
	if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { b, err = BrakeTx(ctx, tx, project, "preview", dana, false); return err }); err != nil || b.Run == nil || b.Run.ID != patch || !b.Stopping {
		t.Fatalf("an undeploy during the patch's /run: %+v (%v)", b, err)
	}
	release()
	if state, reason := waitEnded(t, ctx, p, patch); state != "failed" || reasonOf(reason) != "interrupted" {
		t.Fatalf("the patch run: %s %s", state, reasonOf(reason))
	}
	var rows int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&rows); err != nil || rows != 0 || f.Stopped().Target != "preview" {
		t.Fatalf("preview after the undeploy: %d rows, stopped %+v (%v)", rows, f.Stopped(), err)
	}
}

// A patch test sets pipe fields back in its cleanup, and a workflow still under way would read them unordered,
// which -race reports when it meets it (#2664, #2667). So patchable and patched hand the test a host where every
// workflow they started has ended on its own: the deploy's image scan, the patch run, and its watch. What an
// earlier test left is that test's to end (endWorkflows), never this one's to read (#2687).
func TestAPatchTestsWorkflowsHaveEndedBeforeItGoesOn(t *testing.T) {
	began := time.Now()
	under := func(t *testing.T) []string {
		left, err := dbos.ListWorkflows(durable, dbos.WithFilterCreatedAfter(began), dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(left))
		for i, w := range left {
			ids[i] = w.ID
		}
		return ids
	}
	p, ctx, _, _ := patchable(t)
	if left := under(t); len(left) != 0 {
		t.Errorf("patchable left workflows under way: %v", left)
	}
	if run, state, reason := patched(t, ctx, p); state != "clean" {
		t.Fatalf("the patch run: %s %s", state, reason)
	} else if left := under(t); len(left) != 0 {
		t.Errorf("patched left workflows under way, %s's watch among them: %v", run, left)
	}
}

// The patch's /run carries the egress spec preview's row recorded, never the host's now (design/EGRESS.md §2.5).
func TestPatchRunCarriesTheRecordedEgressSpec(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	var recorded Egress
	if err := p.QueryRow(ctx, "SELECT egress FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&recorded); err != nil || recorded.Via != "off" {
		t.Fatalf("dana's deploy recorded %+v (%v)", recorded, err)
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if run, state, reason := patched(t, ctx, p); state != "clean" || f.Ran().Egress != nil {
		t.Fatalf("the patch run: %s %s, its /run carried %+v, lines %q", state, reason, f.Ran().Egress, lastLines(t, ctx, p, run))
	}
}

// Step 1 refuses a patch run whose identity paused, or stalled naming its app, after it was enqueued: it ends
// failed/interrupted before its gate, and the app is planned again, none started while the pause or the stall
// stands (design/PATCHING.md §4.2).
func TestStepOneRefusesAPausedOrStalledPatchRun(t *testing.T) {
	for name, mark := range map[string]string{
		"paused":            "UPDATE patch_identities SET paused_at = now(), paused_projects = '{other-app}'",
		"stalled on itself": "UPDATE patch_identities SET stalled_at = now(), stalled_projects = '{" + project + "}'",
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx, f, _ := patchable(t)
			release := waitOn(t, &pipe.hold)
			t.Cleanup(func() { pipe.hold.Store(nil) })
			if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
				t.Fatalf("the patch run: %d started (%v)", n, err)
			}
			patch, _, _ := patchRun(t, ctx, p, project)
			if _, err := p.Exec(ctx, mark); err != nil {
				t.Fatal(err)
			}
			release()
			if state, reason := waitEnded(t, ctx, p, patch); state != "failed" || reasonOf(reason) != "interrupted" {
				t.Fatalf("the patch run: %s %s", state, reasonOf(reason))
			}
			if k := kinds(t, ctx, p, patch); slices.Contains(k, "verdict") || f.Rebases.Load() != 0 {
				t.Fatalf("it went on: %v, %d rebases", k, f.Rebases.Load())
			}
			var outcome string // planned again, and left out while paused or stalled naming it
			n, err := pipe.planPatches(ctx)
			if err == nil {
				err = p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = $1", project).Scan(&outcome)
			}
			if err != nil || n != 0 || outcome != PatchPlanned {
				t.Fatalf("the next plan: %d started, the app %s (%v)", n, outcome, err)
			}
		})
	}
}

// The release before this one ran a patch run as its gate alone and recorded no outcome, so the app stayed
// running in its waves after the run ended. The plan sweeps it: planned again when its run ended clean, as a
// person's run interrupting it before its /run is; ended_early when a request ended it at its /run, which proves
// nothing; failed when it ended any other way. One whose patch deployed is left as it is, for its watch (#2379's
// review; #2100).
func TestTheSweepEndsCanaryRowsLeftRunning(t *testing.T) {
	for _, c := range []struct {
		name, ended string // the run's end, as SQL setting it
		want        string // the app's outcome after the plan
		started     int
	}{
		{"clean", "state = 'clean'", PatchRunning, 1}, // planned again, and started
		{"interrupted before its /run", "state = 'failed', failure_reason = 'interrupted'", PatchRunning, 1},
		{"blocked", "state = 'blocked'", PatchFailed, 0},
		{"failed", "state = 'failed', failure_reason = 'error'", PatchFailed, 0},
		{"interrupted at its /run", "state = 'failed', failure_reason = 'interrupted', deploy_started_at = now()", PatchEndedEarly, 0},
		{"deployed", "state = 'clean', deploy_started_at = now()", PatchRunning, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f := patching(t)
			matchedRunImage(t, ctx, p, fakeRunImage)
			runningPreview(t, ctx, p, f, "patchy", sha('a'))
			first := startedPatch(t, ctx, p, "patchy")
			if _, err := p.Exec(ctx, "UPDATE runs SET "+c.ended+", ended_at = now() WHERE id = $1", first); err != nil {
				t.Fatal(err)
			}
			n, err := pipe.planPatches(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var outcome, run string
			if err := p.QueryRow(ctx, "SELECT outcome, coalesce(run, '') FROM patch_wave_apps WHERE project_id = 'patchy'").Scan(&outcome, &run); err != nil {
				t.Fatal(err)
			}
			if n != c.started || outcome != c.want || (c.started == 0) != (run == first) {
				t.Fatalf("a patch run ended %s with no outcome, swept: %d started, the app %s with run %s", c.name, n, outcome, run)
			}
		})
	}
}

// Patching is off until the operator turns it on (CANYON_PATCHING): off, a project whose platform.known-vuln is
// on and whose preview a rebase would patch gets no patch run and no plan, while the rescan still reads its
// image; and a patch run queued while it was on ends at step 1 (contracts/routes.md, Patch runs).
func TestPatchingOffStartsNoPatchRun(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	pipe.Patches.Store(false)
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("with patching off: %d started (%v)", n, err)
	}
	var planned int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM patch_wave_apps").Scan(&planned); err != nil || planned != 0 {
		t.Fatalf("with patching off, %d apps planned (%v)", planned, err)
	}
	if plan, err := pipe.rescanPlan(ctx); err != nil || !plan.RuleOn || !slices.ContainsFunc(plan.Read, func(in imageScan) bool { return in.Project == project }) {
		t.Fatalf("with patching off the rescan still reads the image: %+v (%v)", plan, err)
	}
	pipe.Patches.Store(true)
	release := waitOn(t, &pipe.hold)
	t.Cleanup(func() { pipe.hold.Store(nil) })
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("with patching on: %d started (%v)", n, err)
	}
	patch, _, _ := patchRun(t, ctx, p, project)
	pipe.Patches.Store(false)
	release()
	if state, reason := waitEnded(t, ctx, p, patch); state != "failed" || reasonOf(reason) != "interrupted" || f.Rebases.Load() != 0 {
		t.Fatalf("a patch run queued before patching turned off: %s %s, %d rebases", state, reasonOf(reason), f.Rebases.Load())
	}
}

// A secret set between the run-config step and the /run is caught inside RunApp, before the executor is
// touched: the timeline reads unchanged, then changed, and nothing is deployed (contracts/routes.md, Patch
// runs, step 5).
func TestPatchRunConfigChangedAtItsRun(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	hook := func() { // on the workflow's goroutine: an error, never a Fatal
		keyRef, key, val, err := pipe.Vault.Seal(project, "preview", "STRIPE_SECRET_KEY", []byte("value-runtime-9c1d"))
		if err == nil {
			_, err = p.Exec(ctx, "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ($1, 'preview', 'STRIPE_SECRET_KEY', 'runtime', $2, $3, $4, 'person', 'anonymous')",
				project, keyRef, key, val)
		}
		if err != nil {
			t.Error(err)
		}
	}
	pipe.patching.Store(&hook)
	t.Cleanup(func() { pipe.patching.Store(nil) })
	run, state, reason := patched(t, ctx, p)
	lines := lastLines(t, ctx, p, run)
	unchanged := slices.Index(lines, "What 7d3e8f runs with is unchanged since it was deployed")
	changed := slices.Index(lines, "The secret STRIPE_SECRET_KEY changed since 7d3e8f was deployed: deploy again to apply it, then Canyon patches.")
	if state != "failed" || reason != "not_platform_only" || unchanged < 0 || changed != len(lines)-1 || f.Runs.Load() != 1 {
		t.Fatalf("a secret set at the /run: %s %s, %d /runs, lines %q", state, reason, f.Runs.Load(), lines)
	}
}

// An executor out of reach at the rebase is the line rebase-failed, its error as Canyon got it.
func TestARebaseOutOfReachSaysSo(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	f.FailRebase.Store(-1)
	run, state, reason := patched(t, ctx, p)
	lines := lastLines(t, ctx, p, run)
	last := lines[len(lines)-1]
	if state != "failed" || reason != "error" || !strings.HasPrefix(last, "Canyon could not apply its fix to 7d3e8f: executor: ") || !strings.HasSuffix(last, ", so it did not deploy it") {
		t.Fatalf("an executor out of reach at the rebase: %s %s, %q", state, reason, last)
	}
}

// A promotion of a patched preview carries the spec the patch ran with, as one of a deploy run's does: on
// Kubernetes a recorded canyon is never the host's off now (design/EGRESS.md §2.5; contracts/routes.md, Patch runs).
func TestAPromotionOfAPatchedPreviewKeepsItsEgress(t *testing.T) {
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	p, ctx, _, _ := patchableAfter(t, func(ctx context.Context, p *pgxpool.Pool) {
		if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
			t.Fatal(err)
		}
	})
	run, state, reason := patched(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the patch run: %s %s, lines %q", state, reason, lastLines(t, ctx, p, run))
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'off'"); err != nil {
		t.Fatal(err)
	}
	accessWas := pipe.Access
	pipe.Access = func(context.Context, string) (json.RawMessage, error) { return json.RawMessage(`{}`), nil } // a Kubernetes host
	t.Cleanup(func() { pipe.Access = accessWas })
	spec, err := pipe.egressFor(ctx, Input{Project: project, Promote: &Promotion{GateRun: run}}, "prod")
	if err != nil || spec.Via != "canyon" {
		t.Fatalf("a promotion of the patched preview carries %+v (%v), want via canyon", spec, err)
	}
}

// Patching turned off — by the operator or the org's admin — or the identity paused, after step 1 stops the patch before its /run, read live as the
// /run is about to start: nothing runs, its /run never began, and the app is planned again (contracts/routes.md,
// Patch runs).
func TestAPatchStopsBeforeItsRunOncePatchingIsOffOrPaused(t *testing.T) {
	for name, stop := range map[string]func(ctx context.Context, p *pgxpool.Pool) error{
		"patching off": func(context.Context, *pgxpool.Pool) error { pipe.Patches.Store(false); return nil },
		"the org's patching off": func(ctx context.Context, p *pgxpool.Pool) error {
			_, err := p.Exec(ctx, "UPDATE org_preferences SET patching = 'off'")
			return err
		},
		"paused": func(ctx context.Context, p *pgxpool.Pool) error {
			_, err := p.Exec(ctx, "UPDATE patch_identities SET paused_at = now(), paused_projects = '{other-app}'")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx, f, _ := patchable(t)
			hook := func() { // on the workflow's goroutine: an error, never a Fatal
				if err := stop(ctx, p); err != nil {
					t.Error(err)
				}
			}
			pipe.patching.Store(&hook)
			t.Cleanup(func() { pipe.patching.Store(nil) })
			run, state, reason := patched(t, ctx, p)
			var began bool
			if err := p.QueryRow(ctx, "SELECT deploy_started_at IS NOT NULL FROM runs WHERE id = $1", run).Scan(&began); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || reason != "interrupted" || f.Runs.Load() != 1 || began {
				t.Fatalf("stopped before its /run: %s %s, %d /runs, its /run began %v", state, reason, f.Runs.Load(), began)
			}
		})
	}
}

// A scan of the rebased image that fails never passes on what an earlier attempt stored: a recovery that asks
// again and is not answered reads not checked (contracts/routes.md, Patch runs, step 3).
func TestAScanErrorNeverPassesOnACachedRead(t *testing.T) {
	_, ctx, _, _ := patchable(t)
	executorWas := pipe.Executor
	pipe.Executor = &executor.Client{Base: "http://127.0.0.1:1"} // out of reach
	t.Cleanup(func() { pipe.Executor = executorWas })
	in := Input{Run: "gr_recovered", Project: project, Patch: &Patch{Image: "canyon/" + project + "@" + executortest.Digest}}
	read, err := pipe.readPatched(ctx, in, executortest.RebasedOf(project, fakeRunImage).Image) // the read says checked, as a stored scan would
	if err != nil || read.Checked || read.Why != "could not check: its image scan did not finish" {
		t.Fatalf("a scan that failed: %+v (%v)", read, err)
	}
}

// Both images are matched and read at the step on one database: a database update between the plan and the
// run that publishes an advisory both images carry, or raises one to high, is no regression (contracts/routes.md,
// Patch runs, step 6).
func TestADatabaseUpdateBetweenPlanAndRunIsNoRegression(t *testing.T) {
	newHigh := PlatformFinding{Name: "libc6", Version: "2.39-0ubuntu8.4", Vulnerability: "CVE-2025-7", Fixed: "2.39-0ubuntu8.5", Class: "os", Severity: "high"}
	raised := PlatformFinding{Name: "libc6", Version: "2.39-0ubuntu8.4", Vulnerability: "CVE-2025-9", Fixed: "2.39-0ubuntu8.5", Class: "os", Severity: "high"}
	for name, c := range map[string]struct{ planned, now []PlatformFinding }{
		"an advisory published since": {[]PlatformFinding{libssl3("high", "CVE-2024-6119")}, []PlatformFinding{newHigh}},
		"an advisory raised to high":  {[]PlatformFinding{libssl3("high", "CVE-2024-6119"), {Name: "libc6", Version: "2.39-0ubuntu8.4", Vulnerability: "CVE-2025-9", Class: "os", Severity: "medium"}}, []PlatformFinding{raised}},
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx, f, reads := patchable(t)
			reads.set(executortest.Digest, c.planned)
			release := waitOn(t, &f.HoldRebase)
			if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
				t.Fatalf("the patch run: %d started (%v)", n, err)
			}
			run, _, _ := patchRun(t, ctx, p, project)
			for deadline := time.Now().Add(15 * time.Second); f.Rebases.Load() == 0; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the patch run never asked for its rebase")
				}
			}
			reads.set(executortest.Digest, append([]PlatformFinding{libssl3("high", "CVE-2024-6119")}, c.now...)) // the database moved on
			reads.set(ImageDigest(executortest.RebasedOf(project, fakeRunImage).Image), c.now)
			release()
			if state, reason := waitEnded(t, ctx, p, run); state != "clean" {
				t.Fatalf("the patch run: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
		})
	}
}

// A patch that runs and that Canyon cannot record says so truthfully: preview runs it (contracts/routes.md,
// Patch runs, the unrecorded line).
func TestAPatchThatRunsButIsNotRecordedSaysSo(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	hook := func() { // preview's row moves under the /run: the record finds it not the patch's to write
		if _, err := p.Exec(ctx, "UPDATE deploys SET image = 'canyon/"+project+"@sha256:"+strings.Repeat("7", 64)+"' WHERE env = 'preview'"); err != nil {
			t.Error(err)
		}
	}
	pipe.patching.Store(&hook)
	t.Cleanup(func() { pipe.patching.Store(nil) })
	run, state, reason := patched(t, ctx, p)
	lines := lastLines(t, ctx, p, run)
	if state != "failed" || reason != "error" || f.Runs.Load() != 2 || lines[len(lines)-1] != "Preview runs the patched 7d3e8f, but Canyon could not record it: deploy again so Canyon records what runs there" {
		t.Fatalf("a patch that ran and was not recorded: %s %s, %d /runs, %q", state, reason, f.Runs.Load(), lines)
	}
}

// A patch's /run is marked begun in the deploy step itself, in one transaction with what may stop it, never by
// an earlier step a replay reads back: until that step, a person's start goes first; from it, during the /run,
// it is 409 (contracts/routes.md, Patch runs).
func TestAPatchsRunIsBegunInItsOwnStep(t *testing.T) {
	p, ctx, f, _ := patchable(t)
	var hooked, before atomic.Bool // on the workflow's goroutine: read here only once the run ended
	hook := func() {               // just ahead of beginRun: nothing marked it begun yet
		var begun bool
		if err := p.QueryRow(ctx, "SELECT deploy_started_at IS NOT NULL FROM runs WHERE project_id = $1 AND kind = 'patch'", project).Scan(&begun); err != nil {
			t.Error(err)
		}
		before.Store(begun)
		hooked.Store(true)
	}
	pipe.patching.Store(&hook)
	t.Cleanup(func() { pipe.patching.Store(nil) })
	release := waitOn(t, &f.HoldRun)
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	run, _, _ := patchRun(t, ctx, p, project)
	for deadline := time.Now().Add(15 * time.Second); f.Runs.Load() < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the patch run never reached its /run")
		}
	}
	var during *time.Time
	if err := p.QueryRow(ctx, "SELECT deploy_started_at FROM runs WHERE id = $1", run).Scan(&during); err != nil {
		t.Fatal(err)
	}
	release()
	if state, reason := waitEnded(t, ctx, p, run); state != "clean" || !hooked.Load() || before.Load() || during == nil {
		t.Fatalf("the patch run: %s %s, begun before its deploy step %v (asked %v), during its /run %v", state, reasonOf(reason), before.Load(), hooked.Load(), during)
	}
}

// A recovery that finds a patch's /run begun asks the executor what preview runs before it reads any stop: the
// rebased image running is recorded as it is, patching turned off since included; an executor that cannot tell
// is unrecorded — never ended as if nothing ran; and another image running means that /run never took, so it
// runs again only as one not begun would: with patching off it stops, its /run marked not begun (contracts/
// routes.md, Patch runs, step 7).
func TestARecoveryAfterAPatchsRunRecordsIt(t *testing.T) {
	rebased := executortest.RebasedOf(project, fakeRunImage).Image
	old := "canyon/" + project + "@" + executortest.Digest
	for _, c := range []struct {
		name, running string
		cannotTell    bool
		patching      bool
		want          string // recorded, may run, stopped or run again
	}{
		{"preview runs it", rebased, false, false, "recorded"},
		{"the executor cannot tell", rebased, true, false, "may run"},
		{"preview runs the image it replaces, patching off", old, false, false, "stopped"},
		{"preview runs the image it replaces, patching on", old, false, true, "run again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, _ := patchable(t)
			waitOn(t, &pipe.hold)
			t.Cleanup(func() { pipe.hold.Store(nil) })
			if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
				t.Fatalf("the patch run: %d started (%v)", n, err)
			}
			run, _, _ := patchRun(t, ctx, p, project)
			t.Cleanup(func() { Cancel(durable, run) })                                                             // before the hold is released: its workflow never goes on
			if _, err := p.Exec(ctx, `UPDATE runs SET deploy_started_at = now() WHERE id = $1`, run); err != nil { // its /run began, then the server crashed
				t.Fatal(err)
			}
			if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', $1, $2, 'verdict', 'verdict', '{"state": "clean"}')`, run, project); err != nil {
				t.Fatal(err)
			}
			f.StartAs(project, "preview", c.running)
			f.FailRunning.Store(c.cannotTell)
			pipe.Patches.Store(c.patching) // as it reads at the recovery
			in := Input{Run: run, Project: project, Package: pkg, Actor: PatchActor, Patch: &Patch{Identity: fakeRunImage, RunImage: fakeRunImage, Image: old}}
			d, err := pipe.deployPatch(ctx, in, rebased, configCheck{Spec: Egress{Via: "off"}})
			var begun bool
			if err == nil {
				err = p.QueryRow(ctx, "SELECT deploy_started_at IS NOT NULL FROM runs WHERE id = $1", run).Scan(&begun)
			}
			got := map[bool]string{true: "recorded"}[d.URL != "" && f.Runs.Load() == 1]
			switch {
			case d.MayRun:
				got = "may run"
			case d.Stopped:
				got = "stopped"
			case d.URL != "" && f.Runs.Load() == 2:
				got = "run again"
			}
			if err != nil || got != c.want || begun != (c.want != "stopped") {
				t.Fatalf("the recovered /run: %s, want %s: %+v (%v), %d /runs, begun %v", got, c.want, d, err, f.Runs.Load(), begun)
			}
			if c.want != "recorded" {
				return
			}
			if _, err := pipe.recordPatch(ctx, in, rebased, d, Egress{Via: "off"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if image, gateRun, _ := previewRow(t, ctx, p); image != rebased || gateRun != run {
				t.Fatalf("recorded: preview runs %s under %s", image, gateRun)
			}
		})
	}
}

// A replaced image whose read does not check at the step is its own line, never the patched image's (contracts/
// routes.md, Patch runs, step 3, line unread).
func TestTheReplacedImageUnreadSaysSo(t *testing.T) {
	p, ctx, f, reads := patchable(t)
	release := waitOn(t, &f.HoldRebase)
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	run, _, _ := patchRun(t, ctx, p, project)
	for deadline := time.Now().Add(15 * time.Second); f.Rebases.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the patch run never asked for its rebase")
		}
	}
	reads.set(executortest.Digest, nil) // read after the plan: could not check
	release()
	state, reason := waitEnded(t, ctx, p, run)
	lines := lastLines(t, ctx, p, run)
	if want := "Canyon could not check the image 7d3e8f runs for vulnerabilities: the image has not been matched yet, so it did not deploy the patch"; state != "failed" || reasonOf(reason) != "error" || lines[len(lines)-1] != want {
		t.Fatalf("the replaced image unread: %s %s, %q", state, reasonOf(reason), lines)
	}
}

// Both images' compared layers name one database, or the reads are not compared: a layer left on another
// database is never one with them (contracts/routes.md, Patch runs, steps 3 and 6). Only the platform layers the
// patch run matched are compared: an app layer deps.known-vuln once had matched on an older database, since
// taken out of production or matched by another project sharing its slice, is none of the patch's (#2613).
func TestTwoDatabasesAreNeverOne(t *testing.T) {
	p, ctx := seeded(t)
	layer := func(diff, part byte) storedLayer {
		return storedLayer{DiffID: sha(diff), Part: sha(part), Components: json.RawMessage(`[]`)}
	}
	for digest, layers := range map[string][]storedLayer{sha('a'): {layer('1', 'd'), layer('2', 'e'), layer('4', '9')}, sha('b'): {layer('1', 'd'), layer('3', 'f'), layer('4', '9')}} {
		img := storedImage{Unplaced: json.RawMessage(`[]`), Layers: layers}
		for _, l := range layers {
			img.DiffIDs = append(img.DiffIDs, l.DiffID)
		}
		if err := storeImage(ctx, p, project, digest, img); err != nil {
			t.Fatal(err)
		}
	}
	match := func(part byte, built string) {
		if _, err := p.Exec(ctx, `INSERT INTO part_matches (part_sha, db_built_at, matched_at, report) VALUES ($1, $2, now(), '{}')`, sha(part), built); err != nil {
			t.Fatal(err)
		}
	}
	matched := func(diffs ...byte) (out []rescanLayer) {
		for _, d := range diffs {
			out = append(out, rescanLayer{DiffID: sha(d)})
		}
		return out
	}
	images := []rescanImage{{Project: project, Digest: sha('a'), Layers: matched('1', '2')}, {Project: project, Digest: sha('b'), Layers: matched('1', '3')}}
	const a, b, older = "2026-10-19T06:00:00Z", "2026-10-20T06:00:00Z", "2026-10-15T06:00:00Z"
	match('d', a)
	match('e', a)
	match('f', b)     // the database refreshed before the replaced image's own layer was matched
	match('9', older) // the app layer both images hold, matched days before for deps.known-vuln
	if compared, one, err := oneDatabase(ctx, p, project, images); err != nil || !compared || one {
		t.Fatalf("layers matched on two databases read as one: %v %v (%v)", compared, one, err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM part_matches WHERE part_sha = $1", sha('f')); err != nil {
		t.Fatal(err)
	}
	match('f', a)
	if compared, one, err := oneDatabase(ctx, p, project, images); err != nil || !compared || !one {
		t.Fatalf("every platform layer the patch matched on one database, its app layer on an older one: %v %v (%v)", compared, one, err)
	}
	if compared, one, err := oneDatabase(ctx, p, project, append(slices.Clone(images), images[0])); err != nil || !compared || !one { // a layer pair counted once
		t.Fatalf("an image named twice: %v %v (%v)", compared, one, err)
	}
	// nothing to compare, or a platform layer with no match, is no comparison: never a pass
	if compared, _, err := oneDatabase(ctx, p, project, nil); err != nil || compared {
		t.Fatalf("no layer matched compares: %v (%v)", compared, err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM part_matches WHERE part_sha = $1", sha('e')); err != nil {
		t.Fatal(err)
	}
	if compared, _, err := oneDatabase(ctx, p, project, images); err != nil || compared {
		t.Fatalf("a platform layer with no match compares: %v (%v)", compared, err)
	}
}

// A watch that runs to its end is when its patch approval comes due: Canyon asks for production at once, for the
// image the patch put in preview, while production runs the image preview ran before it (design/PATCHING.md §4.5).
func TestAPassedWatchAsksProduction(t *testing.T) {
	p, ctx, _, _ := patchable(t)
	if _, err := p.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT project_id, 'prod', package_id, image, url, gate_run, run_by FROM deploys WHERE project_id = $1 AND env = 'preview'`, project); err != nil {
		t.Fatal(err)
	}
	run, state, reason := patched(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the patch run: %s %s, lines %q", state, reason, lastLines(t, ctx, p, run))
	}
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM promotion_approvals a JOIN deploys d ON d.project_id = a.project_id AND d.env = 'prod' AND d.image = a.replaces_image
		WHERE a.kind = 'patch' AND a.state = 'pending' AND a.gate_run = $1 AND a.requested_via = 'canyon' AND a.image = $2`, run, executortest.RebasedOf(project, fakeRunImage).Image).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d patch approvals filed as the watch passed (%v)", n, err)
	}
}
