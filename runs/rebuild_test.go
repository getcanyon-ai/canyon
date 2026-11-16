//go:build !noee

package runs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// crossSpawn is a counted runtime finding: npm's bundled cross-spawn, which a newer node-engine fixes and no rebase does.
var crossSpawn = PlatformFinding{Name: "cross-spawn", Version: "7.0.3", Vulnerability: "CVE-2024-21538", Fixed: "7.0.5", Class: "runtime", Severity: "high", Owner: "paketo-buildpacks/node-engine"}

// newerBuilder is the builder GET /builder names once Paketo moved: not the one dana's deploy built with.
var newerBuilder = "sha256:" + strings.Repeat("e", 64)

// rebuildable is claims-portal's preview as dana's deploy left it, on a host that rebuilds (contracts/routes.md, A
// rebuild patch): the org's rebuilds ask, a newer builder named now, preview's image counting one high runtime finding,
// and the rebuilt image's read counting none. It answers the image a rebuild builds.
func rebuildable(t *testing.T) (*pgxpool.Pool, context.Context, *executortest.Fake, *imageReads, string) {
	t.Helper()
	p, ctx, f, reads := patchable(t)
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'ask'"); err != nil {
		t.Fatal(err)
	}
	// the deployed image's own scan, which same_inputs reads its app packages from: dana's deploy asked it before the
	// executor had an answer for it
	img, err := imageOf(loadGolden(t).Image)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeImage(ctx, p, project, executortest.Digest, img); err != nil {
		t.Fatal(err)
	}
	f.Builder.Store(&newerBuilder)
	rebuilt := "canyon/" + project + "@sha256:" + strings.Repeat("d", 64)
	f.Image = rebuilt
	t.Cleanup(func() { f.Image = "" })
	reads.set(executortest.Digest, []PlatformFinding{crossSpawn})
	reads.set(ImageDigest(rebuilt), []PlatformFinding{})
	return p, ctx, f, reads, rebuilt
}

// A rebuild patch builds preview's package again on the builder named now, proves same_inputs, finds what it runs with
// unchanged and the runtime finding gone, then /runs the rebuilt image keeping the one it replaces. Preview's row takes
// the build's digest, values-only HMAC, buildpacks and builder; the audit row carries the same_inputs proof and the key
// version it compared under; its watch passes, and production is asked with the rebuild's own fixes (contracts/routes.md,
// Patch runs, A rebuild patch).
func TestARebuildPatchDeploysToPreview(t *testing.T) {
	p, ctx, f, _, rebuilt := rebuildable(t)
	holdsAt(t, f, "cross-spawn", "7.0.6") // newer than the advisory's 7.0.5: to is what the rebuilt image holds (#2631)
	if _, err := p.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT project_id, 'prod', package_id, image, url, gate_run, run_by FROM deploys WHERE project_id = $1 AND env = 'preview'`, project); err != nil {
		t.Fatal(err)
	}
	builds := f.Builds.Load()
	run, state, reason := patched(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the rebuild: %s %s, lines %q", state, reason, lastLines(t, ctx, p, run))
	}
	want := []string{
		"===> DETECTING", "===> BUILDING", // its build's own lines, as a deploy's build writes them
		"Rebuilt with a newer Node.js: 7d3e8f's code, build settings and dependencies are the same",
		"What 7d3e8f runs with is unchanged since it was deployed",
		"Fixed: cross-spawn 7.0.3 → 7.0.6 (CVE-2024-21538)",
		"No new vulnerability at high or above",
	}
	got := lastLines(t, ctx, p, run)
	if len(got) != len(want)+2 || !slices.Equal(got[:len(want)], want) || got[len(want)+1] != "The watch passed: the patched app neither exited nor restarted" {
		t.Fatalf("its lines:\n got %q\nwant %q, then watching and passed", got, want)
	}
	if ran := f.Ran(); ran.Image != rebuilt || !slices.Contains(ran.Keep, executortest.Digest) || f.Rebases.Load() != 0 || f.Builds.Load() != builds+1 {
		t.Fatalf("the /run: %s keeping %v, %d rebases, %d builds", ran.Image, ran.Keep, f.Rebases.Load(), f.Builds.Load()-builds)
	}
	var image, gateRun, builder string
	var digest, values *string
	if err := p.QueryRow(ctx, "SELECT image, gate_run, coalesce(builder, ''), build_digest, values_hmac FROM deploys WHERE project_id = $1 AND env = 'preview'", project).
		Scan(&image, &gateRun, &builder, &digest, &values); err != nil {
		t.Fatal(err)
	}
	if image != rebuilt || gateRun != run || builder != newerBuilder || digest == nil || values == nil {
		t.Fatalf("preview's row: %s, gate run %s, builder %s, build digest %v, values HMAC %v", image, gateRun, builder, digest, values)
	}
	var detail string
	if err := p.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'patch preview' AND actor_kind = 'canyon' AND run_id = $1", run).Scan(&detail); err != nil ||
		!strings.Contains(detail, `"proof": "same_inputs"`) || !strings.Contains(detail, `"same_inputs": {"buildpacks": ["paketo-buildpacks/node-engine", "paketo-buildpacks/npm-install"], "values_key": 1}`) ||
		!strings.Contains(detail, `"class": "runtime"`) {
		t.Fatalf("the patch preview audit row: %s (%v)", detail, err)
	}
	var steps string
	if err := p.QueryRow(ctx, `SELECT string_agg((data->>'step') || ':' || (data->>'status'), ',' ORDER BY id) FROM timeline_rows
		WHERE run_id = $1 AND kind = 'progress' AND data->>'step' IN ('rebase', 'build', 'image-sbom', 'start', 'watch')`, run).Scan(&steps); err != nil ||
		strings.TrimPrefix(steps, "build:queued,") != "build:running,build:done,image-sbom:running,image-sbom:done,start:running,start:done,watch:running,watch:done" { // a build may wait its turn first
		t.Fatalf("its progress: %q (%v)", steps, err)
	}
	var identity, kind, fixes string
	if err := p.QueryRow(ctx, `SELECT a.patch_identity, i.kind, a.fixes::text FROM promotion_approvals a JOIN patch_identities i ON i.id = a.patch_identity
		WHERE a.kind = 'patch' AND a.state = 'pending' AND a.gate_run = $1 AND a.image = $2`, run, rebuilt).Scan(&identity, &kind, &fixes); err != nil ||
		identity != newerBuilder || kind != "rebuild" || !strings.Contains(fixes, `"class": "runtime"`) {
		t.Fatalf("the rebuild's patch approval: identity %s of kind %s, fixes %s (%v)", identity, kind, fixes, err)
	}
}

// Each input that differs, and a rebuild that fixes nothing, ends the run with nothing deployed, its line the last
// build.log row and which input on its row and its run.state (contracts/routes.md, A rebuild patch).
func TestARebuildThatFailsDeploysNothing(t *testing.T) {
	for _, c := range []struct {
		name          string
		setup         func(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake, reads *imageReads, rebuilt string)
		reason, which string
		line          string
	}{
		{"the key rotated since preview's build", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads, _ string) {
			setRow(t, ctx, p, "UPDATE deploys SET values_key = values_key + 1 WHERE env = 'preview'")
		}, "not_same_inputs", "key_rotated", "Canyon can't compare 7d3e8f's build settings: Canyon's key for build settings changed since 7d3e8f was built, so it did not deploy the rebuild"},
		{"other build values", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads, _ string) {
			setRow(t, ctx, p, "UPDATE deploys SET values_hmac = repeat('0', 64) WHERE env = 'preview'")
		}, "not_same_inputs", "values", "Its build settings changed when Canyon rebuilt 7d3e8f, so it did not deploy it"},
		{"other buildpacks", func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake, _ *imageReads, _ string) {
			setRow(t, ctx, p, `UPDATE deploys SET buildpacks = '[{"id": "paketo-buildpacks/node-engine"}, {"id": "paketo-buildpacks/yarn-install"}]' WHERE env = 'preview'`)
		}, "not_same_inputs", "buildpacks", "How it is built changed when Canyon rebuilt 7d3e8f, so it did not deploy it"},
		{"a rebuild that fixes nothing", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, _ *executortest.Fake, reads *imageReads, rebuilt string) {
			reads.set(ImageDigest(rebuilt), []PlatformFinding{crossSpawn})
		}, "patch_ineffective", "", "Canyon rebuilt 7d3e8f with a newer Node.js, but the rebuild fixes none of its findings, so it did not deploy it"},
		{"a build that fails", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads, _ string) {
			f.FailBuild = true
		}, "error", "", "Canyon could not rebuild 7d3e8f: pack: exit status 1, so it did not deploy it"},
		// Canyon's or the executor's own failure in the build: its own line, quoting none of it (#2619's review)
		{"the executor's own failure in the build", func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads, _ string) {
			f.HostFailBuild = true
		}, "error", "", "Canyon could not finish patching 7d3e8f, so it did not deploy it"},
		// an app package that resolved anew: npm installed a newer express
		{"an app package that moved", func(t *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads, _ string) {
			f.ImageAnswer.Store(bumped(t, "express", "4.19.2", "4.21.0"))
		}, "not_same_inputs", "dependencies", "Its dependencies changed when Canyon rebuilt 7d3e8f (express 4.19.2 → 4.21.0), so it did not deploy it: pin them in a lockfile, then deploy again"},
		// a package of class unknown — another buildpack's, as NuGet's or a go build's are — compared as the app's own (#2619's review)
		{"an unknown-class package that moved", func(t *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads, _ string) {
			f.ImageAnswer.Store(bumped(t, "certifi", "2023.7.22", "2024.2.2"))
		}, "not_same_inputs", "dependencies", "Its dependencies changed when Canyon rebuilt 7d3e8f (certifi 2023.7.22 → 2024.2.2), so it did not deploy it: pin them in a lockfile, then deploy again"},
		// the builder moved between the plan and the build: the build asked GET /builder again after a busy answer
		{"the builder moved", func(t *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake, _ *imageReads, _ string) {
			other := "sha256:" + strings.Repeat("f", 64)
			moved := func() { f.Builder.Store(&other) }
			f.OnBuild.Store(&moved)
			f.BuildBusy.Store(1)
			t.Cleanup(func() { f.OnBuild.Store(nil) })
		}, "error", "", "A newer fix from Canyon came out while it patched 7d3e8f, so Canyon did not deploy it: Canyon tries again at its next check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, reads, rebuilt := rebuildable(t)
			c.setup(t, ctx, p, f, reads, rebuilt)
			runs := f.Runs.Load()
			run, state, reason := patched(t, ctx, p)
			var which, said string
			if err := p.QueryRow(ctx, "SELECT coalesce(not_same_inputs, ''), coalesce((SELECT data->>'not_same_inputs' FROM timeline_rows WHERE run_id = $1 AND kind = 'run.state' AND data->>'state' = 'failed'), '') FROM runs WHERE id = $1",
				run).Scan(&which, &said); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || reason != c.reason || which != c.which || said != c.which || f.Runs.Load() != runs {
				t.Fatalf("%s/%s, not_same_inputs %q on its row and %q on its run.state, %d /run(s)", state, reason, which, said, f.Runs.Load()-runs)
			}
			if lines := lastLines(t, ctx, p, run); len(lines) == 0 || lines[len(lines)-1] != c.line {
				t.Fatalf("its last line: %q, want %q", lines, c.line)
			}
			if image, _, _ := previewRow(t, ctx, p); image != "canyon/"+project+"@"+executortest.Digest {
				t.Fatalf("preview runs %s: a rebuild that failed deploys nothing", image)
			}
		})
	}
}

// No rebuild is planned while the org's rebuilds is off, while the builder named now built preview's image, or for an
// image whose build recorded no builder: fail closed, it moves by its next deploy (contracts/routes.md, A rebuild patch).
func TestNoRebuildIsPlannedWithoutItsInputs(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, ctx context.Context, p *pgxpool.Pool, f *executortest.Fake){
		"rebuilds off": func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake) {
			setRow(t, ctx, p, "UPDATE org_preferences SET rebuilds = 'off'")
		},
		"the same builder": func(_ *testing.T, _ context.Context, _ *pgxpool.Pool, f *executortest.Fake) {
			f.Builder.Store(nil)
		},
		// the run that built the image says which builder: one from a release before builders were recorded says none,
		// whatever its row carries (#2619's review)
		"no builder recorded by the run that built it": func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake) {
			setRow(t, ctx, p, "UPDATE runs SET builder = NULL WHERE build_image IS NOT NULL")
		},
		"a builder not named by digest": func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake) {
			setRow(t, ctx, p, "UPDATE runs SET builder = 'canyon-builder:latest' WHERE build_image IS NOT NULL")
		},
		"no values-only HMAC recorded": func(t *testing.T, ctx context.Context, p *pgxpool.Pool, _ *executortest.Fake) {
			setRow(t, ctx, p, "UPDATE deploys SET values_hmac = NULL, values_key = NULL WHERE env = 'preview'")
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, ctx, f, _, _ := rebuildable(t)
			setup(t, ctx, p, f)
			if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
				t.Fatalf("%d patch runs started (%v)", n, err)
			}
			if run, _, _ := patchRun(t, ctx, p, project); run != "" {
				t.Fatalf("a rebuild started: %s", run)
			}
		})
	}
}

// setRow runs sql on p, failing the test on its error.
func setRow(t *testing.T, ctx context.Context, p *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := p.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

// bumped is the golden image answer with the package name moved from one version to another, its purl with it: what a
// rebuild's image scan answers when that package resolved anew.
func bumped(t *testing.T, name, from, to string) *executor.ScanResult {
	t.Helper()
	g := loadGolden(t).Image
	var doc map[string]any
	if err := json.Unmarshal(g.SBOM, &doc); err != nil {
		t.Fatal(err)
	}
	moved := false
	for _, c := range doc["components"].([]any) {
		if m := c.(map[string]any); m["name"] == name && m["version"] == from {
			m["version"], m["purl"], moved = to, strings.Replace(m["purl"].(string), "@"+from, "@"+to, 1), true
		}
	}
	if !moved {
		t.Fatalf("the golden image has no %s %s", name, from)
	}
	g.SBOM = must(json.Marshal(doc))
	return &g
}

// The image's own packages are every library but Canyon's classes: the app's (express, the app itself) and another
// buildpack's (certifi and openssl, class unknown), never os, runtime or lifecycle; an image with no stored scan reads
// as not read, never as none (contracts/routes.md, A rebuild patch, step 4).
func TestAnImagesOwnPackages(t *testing.T) {
	p, ctx, _, _, _ := rebuildable(t)
	deps, ok, err := appDepsOf(ctx, p, project, executortest.Digest)
	if err != nil || !ok {
		t.Fatalf("the deployed image's packages: %v (%v)", ok, err)
	}
	var names []string
	for _, d := range deps {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"certifi", "claims-portal", "express", "openssl"}) {
		t.Fatalf("its own packages: %q", names)
	}
	if _, ok, err := appDepsOf(ctx, p, project, "sha256:"+strings.Repeat("9", 64)); err != nil || ok {
		t.Fatalf("an image with no stored scan read: %v (%v)", ok, err)
	}
}

// The org's rebuilds turned off after the plan stops a rebuild at step 1 and before its /run, as patching off does
// (contracts/routes.md, the preferences row).
func TestARebuildStopsOnceRebuildsIsOff(t *testing.T) {
	t.Run("at step 1", func(t *testing.T) {
		p, ctx, f, _, _ := rebuildable(t)
		release := waitOn(t, &pipe.hold)
		t.Cleanup(func() { pipe.hold.Store(nil) })
		if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
			t.Fatalf("the rebuild: %d started (%v)", n, err)
		}
		run, _, _ := patchRun(t, ctx, p, project)
		setRow(t, ctx, p, "UPDATE org_preferences SET rebuilds = 'off'")
		builds := f.Builds.Load()
		release()
		if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "interrupted" || f.Builds.Load() != builds {
			t.Fatalf("the rebuild: %s %s, %d builds", state, reasonOf(reason), f.Builds.Load()-builds)
		}
	})
	t.Run("before its /run", func(t *testing.T) {
		p, ctx, f, _, _ := rebuildable(t)
		hook := func() {
			if _, err := p.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'off'"); err != nil {
				t.Error(err)
			}
		}
		pipe.patching.Store(&hook)
		t.Cleanup(func() { pipe.patching.Store(nil) })
		runs := f.Runs.Load()
		if _, state, reason := patched(t, ctx, p); state != "failed" || reason != "interrupted" || f.Runs.Load() != runs {
			t.Fatalf("the rebuild: %s %s, %d /runs", state, reason, f.Runs.Load()-runs)
		}
	})
}

// One rebuild of the host before its /run at once: while another app's is, the plan starts none and says why
// (contracts/routes.md, A rebuild patch).
func TestOneRebuildAtATime(t *testing.T) {
	p, ctx, f, _, _ := rebuildable(t)
	storedAs(t, ctx, p, nil, "other-app", sha('9'))
	busy := "sha256:" + strings.Repeat("a1", 32)
	for _, q := range []string{
		"INSERT INTO patch_identities (id, kind, run_image, builder) VALUES ('" + busy + "', 'rebuild', 'canyon-run:noble', '" + busy + "')",
		"INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_otherbuild', 'other-app', 'pkg_other-app', 'running', 'patch', 'canyon', 'patch', 'Canyon')",
		"INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome, run) VALUES ('" + busy + "', 'other-app', 'canyon/other-app@" + sha('9') + "', 0, 'running', 'gr_otherbuild')",
	} {
		setRow(t, ctx, p, q)
	}
	t.Cleanup(func() {
		setRow(t, context.Background(), p, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_otherbuild'")
	})
	builds := f.Builds.Load()
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 || f.Builds.Load() != builds {
		t.Fatalf("%d rebuilds started beside another (%v)", n, err)
	}
	if run, _, _ := patchRun(t, ctx, p, project); run != "" {
		t.Fatalf("a second rebuild started: %s", run)
	}
	var outcome string
	if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = $1", project).Scan(&outcome); err != nil || outcome != PatchPlanned {
		t.Fatalf("claims-portal waits, planned: %s (%v)", outcome, err)
	}
}

// A tick that plans no rebuild unplans what a rebuild identity planned, so no stale app waits in its waves.
func TestATickThatPlansNoRebuildUnplansThem(t *testing.T) {
	p, ctx, _, _, _ := rebuildable(t)
	setRow(t, ctx, p, "INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_blocker', '"+project+"', '"+pkg+"', 'running', 'gate', 'person', 'sub-dana', 'dana')")
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 { // planned, and not started: another run of the app goes first
		t.Fatalf("%d started (%v)", n, err)
	}
	setRow(t, ctx, p, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_blocker'")
	if n := count(t, ctx, p, "SELECT count(*) FROM patch_wave_apps a JOIN patch_identities i ON i.id = a.identity AND i.kind = 'rebuild' WHERE a.outcome = 'planned'"); n != 1 {
		t.Fatalf("%d rebuilds planned", n)
	}
	setRow(t, ctx, p, "UPDATE org_preferences SET rebuilds = 'off'")
	if _, err := pipe.planPatches(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM patch_wave_apps a JOIN patch_identities i ON i.id = a.identity AND i.kind = 'rebuild' WHERE a.outcome = 'planned'"); n != 0 {
		t.Fatalf("%d rebuilds still planned with rebuilds off", n)
	}
}

// The fleet approval never decides a rebuild: no admin's approval can land on a rebuild identity, and the fleet's
// tick decides none of its open approvals (contracts/routes.md, A rebuild patch).
func TestTheFleetNeverDecidesARebuild(t *testing.T) {
	p, ctx, _, _, _ := rebuildable(t)
	id := "sha256:" + strings.Repeat("b2", 32)
	setRow(t, ctx, p, "INSERT INTO patch_identities (id, kind, run_image, builder) VALUES ('"+id+"', 'rebuild', 'canyon-run:noble', '"+id+"')")
	if _, err := p.Exec(ctx, `UPDATE patch_identities SET approved_by_kind = 'person', approved_by_subject = 'sub-dana', approved_by_display = 'dana', approved_at = now(),
		approved_reason = 'every app', approved_by_email = 'dana@meridian.example' WHERE id = $1`, id); err == nil {
		t.Fatal("an admin's approval landed on a rebuild identity")
	}
	decided := 0
	decide := pipe.DecidePatch
	pipe.DecidePatch = func(context.Context, string, events.Actor) (bool, error) { decided++; return true, nil }
	t.Cleanup(func() { pipe.DecidePatch = decide })
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 || decided != 0 {
		t.Fatalf("the fleet decided %d (%d asked, %v)", n, decided, err)
	}
}
