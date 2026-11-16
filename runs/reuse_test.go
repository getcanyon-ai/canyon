package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const reusing = "Reusing the image built for 7d3e8f (no build value changed)"

// buildValue is the build value these tests seal, looked for whole in a stored digest or HMAC: its last four
// characters alone are hex, which a 64-character hex string under a test's random key holds about once in 1,074
// (#2713).
const buildValue = "value-build-7f3a"

// deployed starts a deploy run of the package and waits for it to end clean.
func deployed(t *testing.T, ctx context.Context, p *pgxpool.Pool, pkgID string) (run string) {
	t.Helper()
	r, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkgID, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, r.ID); state != "clean" || reason != nil {
		t.Fatalf("a deploy run ends clean: %s %s", state, why(reason))
	}
	return r.ID
}

// A redeploy of the same package with the same build values builds nothing: /run gets the image the
// first deploy built, and the run's build.log says so in one line (contracts/routes.md, the deploys row;
// events.md). The row keeps a digest, never a value; a promotion's prod row carries preview's.
func TestARedeployOfTheSamePackageAndBuildValuesReusesItsImage(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", buildValue)
	deployed(t, ctx, p, pkg)
	var image, digest string
	if err := p.QueryRow(ctx, "SELECT image, build_digest FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image, &digest); err != nil || len(digest) != 64 || strings.Contains(digest, buildValue) {
		t.Fatalf("the deploy row keeps the build digest: %q (%v)", digest, err)
	}
	run := deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 1 || f.Runs.Load() != 2 || f.Ran().Image != image {
		t.Fatalf("the redeploy builds nothing and runs the same image: %d builds, %d runs, %s", f.Builds.Load(), f.Runs.Load(), f.Ran().Image)
	}
	if lines := buildLogLines(t, ctx, p, run); !slices.Equal(lines, []string{reusing}) {
		t.Fatalf("the reuse is one build.log line: %q", lines)
	}
	if records, hits := journaled(t, ctx, p, run, buildValue); records == 0 || hits != 0 {
		t.Fatalf("%d step records journaled, the value in %d of them", records, hits)
	}
	if _, state, reason := promote(t, ctx, p, run, image); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	var prod *string
	if err := p.QueryRow(ctx, "SELECT build_digest FROM deploys WHERE project_id = $1 AND env = 'prod'", project).Scan(&prod); err != nil || prod == nil || *prod != digest {
		t.Fatalf("prod's row carries the digest of the preview image it ships: %v (%v)", prod, err)
	}
}

// The first deploy on a Docker host waits while GET /builder builds Canyon's images, before the build writes a line,
// so the build's own clock leaves it out (#2711): the run writes one build.image row with the wait, before its first
// build.log row, though both its reuse step and its build step asked. A deploy whose GET /builder did not wait
// writes none.
func TestADeployThatWaitedForCanyonsBuildImageSaysHowLong(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	kinds := func(run string) (got []string) {
		t.Helper()
		rows, err := p.Query(ctx, "SELECT kind || coalesce(' ' || (data->>'waited_ms'), '') FROM timeline_rows WHERE run_id = $1 AND kind IN ('build.image', 'build.log') ORDER BY id", run)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 || got[len(got)-1] != k {
				got = append(got, k)
			}
		}
		return got
	}
	f.Waited.Store(278000)
	if got := kinds(deployed(t, ctx, p, pkg)); len(got) < 2 || got[0] != "build.image 278000" || slices.Contains(got[1:], "build.image 278000") {
		t.Fatalf("a deploy that waited: %q, want one build.image row before its build.log rows", got)
	}
	f.Waited.Store(0)
	if got := kinds(deployed(t, ctx, p, pkg)); slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "build.image") }) {
		t.Fatalf("a deploy that did not wait: %q", got)
	}
}

// A build step resumed after an upgrade that changed one of Canyon's images asks GET /builder again after its first
// attempt's build.log rows, which the build's own clock already spans: a wait it answers then writes no build.image
// row, or the CLI would count it twice (contracts/events.md, build.image: before the first build.log row).
func TestAWaitAfterTheBuildsFirstLineWritesNoRow(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	run := deployed(t, ctx, p, pkg)                                                              // its build wrote its lines, and its GET /builder waited for nothing
	if _, err := p.Exec(ctx, "UPDATE runs SET ended_at = NULL WHERE id = $1", run); err != nil { // as it was while it built
		t.Fatal(err)
	}
	if err := pipe.prepared(ctx, Input{Run: run, Project: project, Package: pkg, Deploy: true}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'build.image'", run).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a wait answered after the build's first line wrote %d build.image rows (%v)", n, err)
	}
}

// The buildpacks a build ran are kept on the run that built and travel with its image: the preview row
// the build made, a redeploy's row that reuses the image and the prod row a promotion ships it to all
// carry them (contracts/routes.md, the deploys row; #636).
func TestTheBuildpacksThatRanTravelWithTheImage(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	want := string(mustJSONOf(t, executortest.Buildpacks))
	carried := func(sql string) string { // the column as JSON, keys sorted
		t.Helper()
		var got any
		if err := p.QueryRow(ctx, sql, project).Scan(&got); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if s, ok := got.(string); ok {
			return s
		}
		return string(mustJSONOf(t, got))
	}
	first := deployed(t, ctx, p, pkg)
	if got := carried("SELECT buildpacks FROM runs WHERE project_id = $1 AND id = '" + first + "'"); got != want {
		t.Fatalf("the run that built: %s, want %s", got, want)
	}
	if got := carried("SELECT buildpacks FROM deploys WHERE project_id = $1 AND env = 'preview'"); got != want {
		t.Fatalf("the preview row: %s", got)
	}
	run := deployed(t, ctx, p, pkg) // reuses the image: no build this time
	if got := carried("SELECT buildpacks FROM deploys WHERE project_id = $1 AND env = 'preview'"); got != want {
		t.Fatalf("the reusing redeploy's row: %s", got)
	}
	image := carried("SELECT image FROM deploys WHERE project_id = $1 AND env = 'preview'")
	if _, state, reason := promote(t, ctx, p, run, image); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	if got := carried("SELECT buildpacks FROM deploys WHERE project_id = $1 AND env = 'prod'"); got != want {
		t.Fatalf("the prod row: %s", got)
	}
}

func mustJSONOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A build value changed since the image was built: the redeploy builds, with the new value.
func TestARedeployAfterABuildValueChangedBuilds(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", buildValue)
	deployed(t, ctx, p, pkg)
	if _, err := p.Exec(ctx, "DELETE FROM secrets"); err != nil {
		t.Fatal(err)
	}
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", "value-build-8e4b")
	deployed(t, ctx, p, pkg)
	if build, _ := f.Handed(); f.Builds.Load() != 2 || build["NEXT_PUBLIC_API_URL"] != "value-build-8e4b" {
		t.Fatalf("a changed build value builds again, with it: %d builds, %v", f.Builds.Load(), build)
	}
}

// Another package of the project, and rows written before the digest, build.
func TestANewPackageOrARowWithNoDigestBuilds(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	if _, err := p.Exec(ctx, "INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) SELECT 'pkg_9c1d2e', project_id, tree_hash, commit, size_bytes, source_kind FROM packages WHERE id = $1", pkg); err != nil {
		t.Fatal(err)
	}
	deployed(t, ctx, p, "pkg_9c1d2e")
	if f.Builds.Load() != 2 {
		t.Fatalf("a new package builds: %d builds", f.Builds.Load())
	}
	for _, q := range []string{"UPDATE deploys SET build_digest = NULL", "UPDATE runs SET build_digest = NULL"} { // as written before the column
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deployed(t, ctx, p, "pkg_9c1d2e")
	if f.Builds.Load() != 3 {
		t.Fatalf("a row with no digest is never reused: %d builds", f.Builds.Load())
	}
}

// The image is gone from the host: /run answers 404, and the run builds, once, and runs the new image. The
// start goes on after the build, so it never reads failed meanwhile (#1752).
func TestAReusedImageRunReportsMissingIsBuiltOnce(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	f.NoImage.Store(1)
	run := deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 2 || f.Runs.Load() != 3 {
		t.Fatalf("a gone image is built once and run: %d builds, %d runs", f.Builds.Load(), f.Runs.Load())
	}
	if lines := buildLogLines(t, ctx, p, run); len(lines) != 3 || lines[0] != reusing || lines[1] != "===> DETECTING" {
		t.Fatalf("the reuse line, then the build's: %q", lines)
	}
	if kinds := strings.Join(kinds(t, ctx, p, run), ","); strings.Count(kinds, "deploy.completed") != 1 {
		t.Fatalf("one deploy recorded: %s", kinds)
	}
	var steps string
	if err := p.QueryRow(ctx, `SELECT string_agg((data->>'step') || ':' || (data->>'status'), ',' ORDER BY id) FROM timeline_rows
		WHERE run_id = $1 AND kind = 'progress' AND data->>'step' IN ('build', 'start')`, run).Scan(&steps); err != nil ||
		strings.Replace(steps, "build:queued,", "", 1) != "start:running,build:running,build:done,start:done" { // queued only if the build waited at all
		t.Fatalf("the start runs, the build runs and is done, and the start is done, one row per status: %q (%v)", steps, err)
	}
}

// A row of another project with the same package id is never reused: a package id is unique only
// within its project, and an image is never shared between apps.
func TestAnotherProjectsRowWithTheSamePackageIsNeverReused(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	other := "canyon/quote-engine@sha256:" + strings.Repeat("b", 64)
	valid, err := pipe.Vault.BuildDigest(project, other, "sha256:"+strings.Repeat("b", 64), map[string]string{}) // a digest that would match were the row this project's
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('quote-engine')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('" + pkg + "', 'quote-engine', 'aa', 'bb', 1, 'upload')",
		"UPDATE deploys SET project_id = 'quote-engine', image = '" + other + "', build_digest = '" + valid + "'", // the same package id, another app's
		"UPDATE runs SET build_digest = NULL", // this app's own build is no candidate
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 2 || f.Ran().Image != "canyon/claims-portal@"+executortest.Digest {
		t.Fatalf("another app's image is never run: %d builds, %s", f.Builds.Load(), f.Ran().Image)
	}
}

// A row whose image another writer changed under it — an org's report, an older server — is never
// reused: the digest covers the image it was built into (contracts/routes.md, the deploys row).
func TestARowWhoseImageChangedUnderItIsNeverReused(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1", "canyon/claims-portal@sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	deployed(t, ctx, p, pkg)
	if f.Ran().Image != "canyon/claims-portal@"+executortest.Digest {
		t.Fatalf("an image the digest was not made for is never run: %s", f.Ran().Image)
	}
}

// A preview row the org's platform reported, then promoted: prod's row takes no digest from it, and a
// later deploy never runs the reported image (the image Canyon built, its run's, stays a candidate).
func TestAReportedRowFollowedByAPromotionIsNeverReused(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	run := deployed(t, ctx, p, pkg)
	reported := "canyon/claims-portal@sha256:" + strings.Repeat("d", 64)
	if _, err := p.Exec(ctx, "UPDATE deploys SET run_by = 'org', image = $1", reported); err != nil { // as a report wrote it before it cleared the digest
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, run, reported); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	var prod *string
	if err := p.QueryRow(ctx, "SELECT build_digest FROM deploys WHERE env = 'prod'").Scan(&prod); err != nil || prod != nil {
		t.Fatalf("prod's row takes no digest from a row the org reported: %v (%v)", prod, err)
	}
	deployed(t, ctx, p, pkg)
	if f.Ran().Image != "canyon/claims-portal@"+executortest.Digest {
		t.Fatalf("the reported image is never reused: %s", f.Ran().Image)
	}
}

// A deploy whose app did not stay up still built its image, and the run keeps it: once a runtime value
// is set, the redeploy reuses it rather than build the same package again.
func TestARedeployAfterAFailedStartReusesTheImageItBuilt(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.Exited = &executor.Exited{Reason: "the app did not stay up: its container is exited", Output: "STRIPE_SECRET_KEY is not set\n"}
	r, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, r.ID); state != "failed" || why(reason) != "start" {
		t.Fatalf("the start fails: %s %s", state, why(reason))
	}
	f.Exited = nil
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-9c1d")
	run := deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 1 || f.Runs.Load() != 2 || !slices.Equal(buildLogLines(t, ctx, p, run), []string{reusing}) {
		t.Fatalf("the redeploy reuses the image the failed start built: %d builds, %d runs, %q", f.Builds.Load(), f.Runs.Load(), buildLogLines(t, ctx, p, run))
	}
}

// The reused image's 404 is journaled as a result: a run that crashes after it, while the fresh build is
// under way, resumes and builds once more — never running the gone image again — and records one deploy.
func TestARunRecoveredAfterAReusedImageWasGoneBuildsOnceAndNeverRunsIt(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	fresh := "canyon/claims-portal@sha256:" + strings.Repeat("e", 64)
	f.Image = fresh
	t.Cleanup(func() { f.Image = "" })
	f.NoImage.Store(1)
	hold := make(chan struct{})
	f.HoldBuild.Store(&hold)
	t.Cleanup(sync.OnceFunc(func() { close(hold) }))
	schema := fmt.Sprintf("dbos_reuse_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	launch := func() dbos.Context {
		dc, err := dbos.NewContext(ctx, dbos.Config{AppName: "canyon-runs-test", DatabaseURL: os.Getenv("CANYON_TEST_DATABASE_URL"), DatabaseSchema: schema, ApplicationVersion: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&Pipeline{Pool: p, Repos: pipe.Repos, Gate: runstest.CleanGolden, Executor: pipe.Executor, Vault: pipe.Vault, PublicURL: pipe.PublicURL, JWKSURL: pipe.JWKSURL, Org: pipe.Org}).Register(dc); err != nil {
			t.Fatal(err)
		}
		if err := dbos.Launch(dc); err != nil {
			t.Fatal(err)
		}
		return dc
	}
	crashing := launch()
	run, err := Start(ctx, p, crashing, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for f.Builds.Load() < 2 { // the gone image's /run answered 404 and the fresh build is under way
		time.Sleep(10 * time.Millisecond)
	}
	if err := dbos.Shutdown(crashing, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	f.HoldBuild.Store(nil)
	recovered := launch()
	t.Cleanup(func() { _ = dbos.Shutdown(recovered, 10*time.Second) })
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the recovered run: %s %s", state, why(reason))
	}
	if f.Builds.Load() != 3 || f.Runs.Load() != 3 || f.Ran().Image != fresh {
		t.Fatalf("one build after the crash, and only the fresh image run: %d builds, %d runs, last %s", f.Builds.Load(), f.Runs.Load(), f.Ran().Image)
	}
	if kinds := strings.Join(kinds(t, ctx, p, run.ID), ","); strings.Count(kinds, "deploy.completed") != 1 {
		t.Fatalf("one deploy recorded: %s", kinds)
	}
}

// A reused image /run refuses — an executor from before the 404, a registry that changed, a host that
// moved between Docker and Kubernetes — is built once and run, never a package that can no longer deploy.
func TestAReusedImageRunRefusesIsBuiltOnce(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	f.RefuseRun.Store(1)
	deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 2 || f.Runs.Load() != 3 {
		t.Fatalf("a refused reuse is built once and run: %d builds, %d runs", f.Builds.Load(), f.Runs.Load())
	}
}

// The builder is part of the digest: a patched or replaced builder rebuilds, the same one reuses.
func TestANewBuilderRebuildsAndTheSameOneReuses(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 1 {
		t.Fatalf("the same builder reuses: %d builds", f.Builds.Load())
	}
	patched := "sha256:" + strings.Repeat("c", 64)
	f.Builder.Store(&patched)
	deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 2 {
		t.Fatalf("a new builder rebuilds: %d builds", f.Builds.Load())
	}
}

// The run image is never part of the digest (design/PATCHING.md §4.1; contracts/executor.md, GET /builder): a
// redeploy after GET /builder's run_image moved reuses the image built onto the older one, which a rebase moves.
func TestAMovedRunImageStillReusesTheImage(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	var digest string
	if err := p.QueryRow(ctx, "SELECT build_digest FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	moved := "sha256:" + strings.Repeat("d", 64)
	f.RunImage.Store(&moved)
	run := deployed(t, ctx, p, pkg)
	var again string
	if err := p.QueryRow(ctx, "SELECT build_digest FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&again); err != nil || f.Builds.Load() != 1 || again != digest {
		t.Fatalf("a moved run image: %d builds, the digest %s then %s (%v); want one build and the same digest", f.Builds.Load(), digest, again, err)
	}
	if lines := buildLogLines(t, ctx, p, run); !slices.Equal(lines, []string{reusing}) {
		t.Fatalf("the reuse is one build.log line: %q", lines)
	}
}

// An executor from before GET /builder answers 404: nothing is reused and no digest is recorded, so the
// server builds as it always did.
func TestAnExecutorWithoutGetBuilderNeverReuses(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.NoBuilder.Store(true)
	deployed(t, ctx, p, pkg)
	deployed(t, ctx, p, pkg)
	var recorded int
	if err := p.QueryRow(ctx, "SELECT (SELECT count(build_digest) FROM runs) + (SELECT count(build_digest) FROM deploys)").Scan(&recorded); err != nil || f.Builds.Load() != 2 || recorded != 0 {
		t.Fatalf("no builder, no reuse and no digest: %d builds, %d digests (%v)", f.Builds.Load(), recorded, err)
	}
}

// The values-only HMAC (design/PATCHING.md §5) is stored on every build with its key version: on the run
// that built, on the preview row, on a redeploy's row that reuses the image, and on the prod row a
// promotion copies from preview's. It is never a value, never the build digest, and an executor from
// before GET /builder, which records no digest, still gets it.
func TestTheValuesHMACIsStoredOnEveryBuild(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", buildValue)
	first := deployed(t, ctx, p, pkg)
	var keyed, digest string
	var key int
	if err := p.QueryRow(ctx, "SELECT values_hmac, values_key, build_digest FROM runs WHERE id = $1", first).Scan(&keyed, &key, &digest); err != nil ||
		len(keyed) != 64 || key < 1 || keyed == digest || strings.Contains(keyed, buildValue) {
		t.Fatalf("the run that built keeps the values hmac and its key: %q v%d, digest %q (%v)", keyed, key, digest, err)
	}
	row := func(env string) (string, int) {
		t.Helper()
		var h *string
		var k *int
		if err := p.QueryRow(ctx, "SELECT values_hmac, values_key FROM deploys WHERE project_id = $1 AND env = $2", project, env).Scan(&h, &k); err != nil || h == nil || k == nil {
			t.Fatalf("%s's row keeps the values hmac and its key: %v %v (%v)", env, h, k, err)
		}
		return *h, *k
	}
	if h, k := row("preview"); h != keyed || k != key {
		t.Fatalf("preview's row: %s v%d, the build's %s v%d", h, k, keyed, key)
	}
	run := deployed(t, ctx, p, pkg)
	if f.Builds.Load() != 1 {
		t.Fatalf("the redeploy reuses: %d builds", f.Builds.Load())
	}
	if h, k := row("preview"); h != keyed || k != key {
		t.Fatalf("the reused image's row: %s v%d, the build's %s v%d", h, k, keyed, key)
	}
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, run, image); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	if h, k := row("prod"); h != keyed || k != key {
		t.Fatalf("prod's row: %s v%d, preview's %s v%d", h, k, keyed, key)
	}
	f.NoBuilder.Store(true)
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_FLAG", "value-build-9c1d")
	deployed(t, ctx, p, pkg)
	if h, _ := row("preview"); h == keyed || len(h) != 64 {
		t.Fatalf("another value, no builder: the values hmac is still made, and moves: %s", h)
	}
}
