package runs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// image is this app's image of the digest whose hex is 64 times c.
func image(c string) string { return "canyon/" + project + "@sha256:" + strings.Repeat(c, 64) }

func digestOf(image string) string { _, d, _ := strings.Cut(image, "@"); return d }

// inProduction is production's history as two promotions leave it: pkg (7d3e8f) ran image a, then v1.4
// (pkg_9e40) ran image b in its place and runs there now.
func inProduction(t *testing.T, ctx context.Context, p *pgxpool.Pool) (a, b string) {
	t.Helper()
	a, b = image("a"), image("b")
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, label) VALUES ('pkg_9e40', '" + project + "', 'cc', 'dd', 1, 'upload', 'v1.4')",
		"INSERT INTO runs (id, project_id, package_id, state, kind, created_at, ended_at) VALUES ('gr_a', '" + project + "', '" + pkg + "', 'clean', 'gate', now() - interval '2 days', now()), ('gr_b', '" + project + "', 'pkg_9e40', 'clean', 'gate', now() - interval '1 day', now())",
		"INSERT INTO deploy_history (project_id, package_id, image, replaces, gate_run, run, deployed_at, promoted_by_kind, promoted_by_subject, promoted_by_display) VALUES " +
			"('" + project + "', '" + pkg + "', '" + a + "', NULL, 'gr_a', 'gr_a', now() - interval '2 days', 'person', 'dana', 'Dana'), " +
			"('" + project + "', 'pkg_9e40', '" + b + "', '" + a + "', 'gr_b', 'gr_b', now() - interval '1 day', 'person', 'dana', 'Dana')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('" + project + "', 'prod', 'pkg_9e40', '" + b + "', 'http://claims-portal.prod.localhost:7777', 'gr_b', 'canyon')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// The history v1.3, v1.4, v1.4 patched, v1.3 again, then v1.5 running: three earlier versions, each
// (package, image) once at its newest, newest first, the patched image marked — never v1.4 · v1.4.
func TestEarlierCollapsesPatchedAndRepeated(t *testing.T) {
	p, ctx := seeded(t)
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, label) VALUES ('pkg_13', '" + project + "', '13', '13', 1, 'upload', 'v1.3'), ('pkg_14', '" + project + "', '14', '14', 1, 'upload', 'v1.4'), ('pkg_15', '" + project + "', '15', '15', 1, 'upload', 'v1.5')",
		"INSERT INTO runs (id, project_id, package_id, state, kind, ended_at) VALUES ('gr_15', '" + project + "', 'pkg_15', 'clean', 'gate', now())",
		"INSERT INTO deploy_history (project_id, package_id, image, gate_run, run, deployed_at, patched) VALUES " +
			"('" + project + "', 'pkg_13', '" + image("3") + "', 'gr_x', 'gr_x', '2026-10-01T10:00:00Z', false), " +
			"('" + project + "', 'pkg_14', '" + image("4") + "', 'gr_x', 'gr_x', '2026-10-02T10:00:00Z', false), " +
			"('" + project + "', 'pkg_14', '" + image("c") + "', 'gr_x', 'gr_x', '2026-10-04T10:00:00Z', true), " +
			"('" + project + "', 'pkg_13', '" + image("3") + "', 'gr_x', 'gr_x', '2026-10-05T10:00:00Z', false), " +
			"('" + project + "', 'pkg_15', '" + image("5") + "', 'gr_x', 'gr_x', '2026-10-06T10:00:00Z', false)",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('" + project + "', 'prod', 'pkg_15', '" + image("5") + "', 'http://claims-portal.prod.localhost:7777', 'gr_15', 'canyon')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	earlier, err := ReadEarlier(ctx, p, project)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range earlier {
		got = append(got, e.Version+" "+e.Digest[7:8]+" "+e.DeployedAt+map[bool]string{true: " patched"}[e.Patched])
	}
	if want := []string{"v1.3 3 2026-10-05T10:00:00Z", "v1.4 c 2026-10-04T10:00:00Z patched", "v1.4 4 2026-10-02T10:00:00Z"}; !slices.Equal(got, want) {
		t.Fatalf("earlier:\n got %q\nwant %q", got, want)
	}
}

// The read is served by deploy_history_newest, never a scan of every app's history.
func TestEarlierUsesItsIndex(t *testing.T) {
	p, ctx := seeded(t)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil { // an empty table is scanned whole, whatever its indexes: the plan shows which one serves it
		t.Fatal(err)
	}
	var plan []string
	rows, err := tx.Query(ctx, "EXPLAIN "+earlierSQL, project)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "deploy_history_newest on deploy_history") {
		t.Fatalf("the earlier read does not use its index:\n%s", strings.Join(plan, "\n"))
	}
}

// A rollback review of v1.3's digest reviews it, builds nothing, and records the image production ran;
// its promotion starts exactly that image in production, keeping the one it replaces, and the history,
// the deploy row and the trail say it was put back.
func TestRollbackShipsTheDigestProductionRan(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	a, b := inProduction(t, ctx, p)
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a), Reason: "INC-2231: v1.4 corrupts exports"})
	if err != nil {
		t.Fatal(err)
	}
	var ships *string
	if state, reason := waitEnded(t, ctx, p, review.ID); state != "clean" || f.Builds.Load() != 0 || f.Runs.Load() != 0 ||
		p.QueryRow(ctx, "SELECT ships FROM runs WHERE id = $1", review.ID).Scan(&ships) != nil || ships == nil || *ships != a {
		t.Fatalf("the review: %s %s, %d builds, %d runs, ships %v", state, reasonOf(reason), f.Builds.Load(), f.Runs.Load(), ships)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: lee, Promote: &Promotion{Image: a, GateRun: review.ID, DeployedBy: dana, Rollback: true, Reason: "INC-2231: v1.4 corrupts exports"}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the promotion: %s %s, last line %q", state, reasonOf(reason), lastLine(t, ctx, p, run.ID))
	}
	if r := f.Ran(); r.Target != "prod" || r.Image != a || !slices.Contains(r.Keep, digestOf(b)) || f.Builds.Load() != 0 {
		t.Fatalf("prod starts the digest it ran, keeping the one it replaces: %+v, %d builds", r, f.Builds.Load())
	}
	var row, history, audit int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM deploys WHERE env = 'prod' AND image = $1 AND package_id = $2 AND gate_run = $3 AND promoted_by_subject = $4 AND deployed_by_subject = $5),
		(SELECT count(*) FROM deploy_history WHERE image = $1 AND replaces = $6 AND gate_run = $3 AND run = $7 AND kept),
		(SELECT count(*) FROM audit_events WHERE action = 'deploy prod' AND run_id = $7 AND detail @> jsonb_build_object('replaces', 'pkg_9e40', 'rollback', true, 'reason', 'INC-2231: v1.4 corrupts exports'))`,
		a, pkg, review.ID, lee.Subject, dana.Subject, b, run.ID).Scan(&row, &history, &audit); err != nil || row != 1 || history != 1 || audit != 1 {
		t.Fatalf("the deploy row %d, the history row %d, the trail %d (%v)", row, history, audit, err)
	}
	earlier, err := ReadEarlier(ctx, p, project)
	if err != nil || len(earlier) != 1 || earlier[0].Version != "v1.4" || earlier[0].Digest != digestOf(b) || !earlier[0].Kept {
		t.Fatalf("v1.4 is production's earlier version now, kept so the rollback can be undone: %+v (%v)", earlier, err)
	}
}

// A rollback to an image a patch made keeps it patched (contracts/routes.md, Patch runs; #2390): the mark is
// the image's, so the history row the rollback appends is patched, and earlier[] still reads it patched once
// production moves on; the trail is a person's deploy prod, rollback and patched, never patch prod.
func TestARollbackToAPatchedImageKeepsItPatched(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	a, b := inProduction(t, ctx, p)
	if _, err := p.Exec(ctx, "UPDATE deploy_history SET patched = true WHERE image = $1", a); err != nil {
		t.Fatal(err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a), Reason: "INC-2240: v1.4 drops uploads"})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the review: %s %s", state, reasonOf(reason))
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: lee, Promote: &Promotion{Image: a, GateRun: review.ID, DeployedBy: dana, Rollback: true, Reason: "INC-2240: v1.4 drops uploads"}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the promotion: %s %s, last line %q", state, reasonOf(reason), lastLine(t, ctx, p, run.ID))
	}
	var patched bool
	var deployProd, patchProd int
	if err := p.QueryRow(ctx, `SELECT (SELECT patched FROM deploy_history WHERE run = $1),
		(SELECT count(*) FROM audit_events WHERE action = 'deploy prod' AND run_id = $1 AND detail @> '{"rollback": true, "patched": true}'),
		(SELECT count(*) FROM audit_events WHERE action = 'patch prod' AND run_id = $1)`, run.ID).Scan(&patched, &deployProd, &patchProd); err != nil {
		t.Fatal(err)
	}
	if !patched || deployProd != 1 || patchProd != 0 {
		t.Errorf("the rollback's history row patched %t; deploy prod rows with rollback and patched: %d; patch prod rows: %d", patched, deployProd, patchProd)
	}
	// production moves on: v1.4 again, so the rollback's row is the newest of v1.3's image
	if _, err := p.Exec(ctx, `UPDATE deploys SET package_id = 'pkg_9e40', image = $2 WHERE project_id = $1 AND env = 'prod'`, project, b); err != nil {
		t.Fatal(err)
	}
	earlier, err := ReadEarlier(ctx, p, project)
	if err != nil || len(earlier) != 1 || earlier[0].Digest != digestOf(a) || !earlier[0].Patched {
		t.Errorf("once production moves on, earlier[] reads the patched image %+v (%v); want it patched", earlier, err)
	}
}

// An image no /run kept is gone: the review ends failed, says so, records nothing and builds nothing.
func TestRollbackRefusesWhenTheImageIsGone(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	a, _ := inProduction(t, ctx, p)
	if _, err := p.Exec(ctx, "UPDATE deploy_history SET kept = false WHERE image = $1", a); err != nil {
		t.Fatal(err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	var ships *string
	state, reason := waitEnded(t, ctx, p, review.ID)
	if err := p.QueryRow(ctx, "SELECT ships FROM runs WHERE id = $1", review.ID).Scan(&ships); err != nil || state != "failed" || reasonOf(reason) != "image_gone" || ships != nil || f.Builds.Load() != 0 || f.Runs.Load() != 0 {
		t.Fatalf("a gone image: %s %s, ships %v, %d builds, %d runs (%v)", state, reasonOf(reason), ships, f.Builds.Load(), f.Runs.Load(), err)
	}
	if want := "7d3e8f's image is no longer on this Canyon, so it can't be put back as it ran. Deploy 7d3e8f to preview and promote it if you want it rebuilt."; lastLine(t, ctx, p, review.ID) != want {
		t.Fatalf("the last line: %q", lastLine(t, ctx, p, review.ID))
	}
}

// A kept image the executor no longer has fails the promotion before production changes, says why, and
// is no longer kept, so the next rollback to it refuses.
func TestRollbackWhoseKeptImageIsGoneLeavesProductionAsItWas(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	a, b := inProduction(t, ctx, p)
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the review: %s", state)
	}
	f.NoImage.Store(1)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: a, GateRun: review.ID, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	var kept bool
	var prod string
	if err := p.QueryRow(ctx, "SELECT bool_or(kept) FROM deploy_history WHERE image = $1", a).Scan(&kept); err != nil || state != "failed" || reasonOf(reason) != "error" || kept {
		t.Fatalf("the promotion: %s %s, still kept %v (%v)", state, reasonOf(reason), kept, err)
	}
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'prod'").Scan(&prod); err != nil || prod != b || failedIn(t, ctx, p, run.ID) != "" ||
		lastLine(t, ctx, p, run.ID) != "7d3e8f's kept image is gone, so it can't be put back as it ran." {
		t.Fatalf("production %s, failed in %q, last line %q (%v)", prod, failedIn(t, ctx, p, run.ID), lastLine(t, ctx, p, run.ID), err)
	}
}

// Thirty reviews of the hour hold back the next review, never a rollback review, which counts toward neither limit.
func TestRollbackReviewIsNotRateLimited(t *testing.T) {
	p, ctx := live(t, runstest.CleanGolden)
	a, _ := inProduction(t, ctx, p)
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, kind, started_by_kind, started_by_subject, ended_at)
		SELECT 'gr_hour' || n, $1, $2, 'failed', 'interrupted', 'gate', $3, $4, now() FROM generate_series(1, 30) n`, project, pkg, dana.Kind, dana.Subject); err != nil {
		t.Fatal(err)
	}
	var limit *RateLimited
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana}); !errors.As(err, &limit) {
		t.Fatalf("the 31st review: %v", err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatalf("a rollback review past the limit: %v", err)
	}
	waitEnded(t, ctx, p, review.ID)
	if _, err := p.Exec(ctx, "DELETE FROM runs WHERE id = 'gr_hour1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana}); err != nil {
		t.Fatalf("29 reviews and a rollback review leave room for one more: %v", err)
	}
}

// Today's review blocks v1.3: the run ends blocked and records nothing to ship.
func TestRollbackBlockedTodayShipsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.Golden)
	a, _ := inProduction(t, ctx, p)
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	var ships *string
	state, _ := waitEnded(t, ctx, p, review.ID)
	if err := p.QueryRow(ctx, "SELECT ships FROM runs WHERE id = $1", review.ID).Scan(&ships); err != nil || state != "blocked" || ships != nil || f.Builds.Load() != 0 || f.Runs.Load() != 0 {
		t.Fatalf("a blocked rollback review: %s, ships %v, %d builds, %d runs (%v)", state, ships, f.Builds.Load(), f.Runs.Load(), err)
	}
}

// Every /run keeps its own image, production's previous one and each deploy row's, and an image of
// the history it leaves out stops reading kept before the call: here the one two promotions back.
func TestEveryRunKeepsProductionsPreviousImage(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	_, b := inProduction(t, ctx, p)
	c, d := image("c"), image("d")
	if _, err := p.Exec(ctx, `INSERT INTO deploy_history (project_id, package_id, image, replaces, gate_run, run, deployed_at) VALUES ($1, $2, $3, $4, 'gr_b', 'gr_b', now() - interval '2 hours'), ($1, $2, $5, $3, 'gr_b', 'gr_b', now() - interval '1 hour')`,
		project, pkg, c, b, d); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1 WHERE env = 'prod'", d); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	keep := slices.Sorted(slices.Values(f.Ran().Keep))
	if want := (executor.Digests{executortest.Digest, digestOf(c), digestOf(d)}); !slices.Equal(keep, want) {
		t.Fatalf("keep: %q, want preview's own image, production's and the one it replaced, %q", keep, want)
	}
	var keptB, keptC bool
	if err := p.QueryRow(ctx, "SELECT bool_and(kept) FILTER (WHERE image = $1), bool_and(kept) FILTER (WHERE image = $2) FROM deploy_history", b, c).Scan(&keptB, &keptC); err != nil || keptB || !keptC {
		t.Fatalf("kept: v1.4's %v, the previous image's %v (%v)", keptB, keptC, err)
	}
}

// Rollback reviews count in a limit of their own, by the same numbers: thirty of the hour hold back the
// next one, never an ordinary review.
func TestRollbackReviewsHaveTheirOwnLimit(t *testing.T) {
	p, ctx := live(t, runstest.CleanGolden)
	a, _ := inProduction(t, ctx, p)
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, kind, started_by_kind, started_by_subject, ended_at, rollback_to)
		SELECT 'gr_back' || n, $1, $2, 'failed', 'interrupted', 'gate', $3, $4, now(), $5 FROM generate_series(1, 30) n`, project, pkg, dana.Kind, dana.Subject, digestOf(a)); err != nil {
		t.Fatal(err)
	}
	var limit *RateLimited
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)}); !errors.As(err, &limit) || limit.PerActor || limit.Limit != 30 {
		t.Fatalf("the hour's 31st rollback review: %v", err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana})
	if err != nil {
		t.Fatalf("an ordinary review past thirty rollback reviews: %v", err)
	}
	waitEnded(t, ctx, p, review.ID)
}

// A review whose blocking findings a person approved before it ran ends blocked and ships, as a deploy
// so approved goes ahead.
func TestRollbackShipsWhenItsBlockingFindingsWereApproved(t *testing.T) {
	p, ctx, f := deploying(t, runstest.Golden)
	a, _ := inProduction(t, ctx, p)
	first, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, first.ID)
	if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		SELECT $1, f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'INC-2231: accepted until the fix', now() + interval '1 day', $2, $3, $4, true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.run_id = $5 AND t.kind = 'verdict'`, project, dana.Kind, dana.Subject, dana.Display, first.ID); err != nil {
		t.Fatal(err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	var ships *string
	state, _ := waitEnded(t, ctx, p, review.ID)
	if err := p.QueryRow(ctx, "SELECT ships FROM runs WHERE id = $1", review.ID).Scan(&ships); err != nil || state != "blocked" || ships == nil || *ships != a || f.Builds.Load() != 0 {
		t.Fatalf("an approved rollback review: %s, ships %v, %d builds (%v)", state, ships, f.Builds.Load(), err)
	}
}

// A rollback review's recorded ships is kept across another run's /run while it waits for its promotion,
// and left out once a promotion of the app starts.
func TestKeepHoldsAReviewsShipsUntilAPromotionStarts(t *testing.T) {
	p, ctx := live(t, runstest.CleanGolden)
	a, b := inProduction(t, ctx, p)
	c := image("c")
	if _, err := p.Exec(ctx, "INSERT INTO deploy_history (project_id, package_id, image, replaces, gate_run, run) VALUES ($1, $2, $3, $4, 'gr_b', 'gr_b')", project, pkg, c, b); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE deploys SET image = $1 WHERE env = 'prod'", c); err != nil {
		t.Fatal(err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the review: %s", state)
	}
	keep, err := pipe.keep(ctx, project, image("d")) // a preview deploy's /run meanwhile
	if err != nil || !slices.Contains(keep, digestOf(a)) {
		t.Fatalf("a /run while the review waits keeps what it ships: %q (%v)", keep, err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, failure_reason, ended_at) VALUES ('gr_prom', $1, $2, 'failed', 'promote', 'error', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if keep, err = pipe.keep(ctx, project, image("d")); err != nil || slices.Contains(keep, digestOf(a)) ||
		p.QueryRow(ctx, "SELECT bool_or(kept) FROM deploy_history WHERE image = $1", a).Scan(&kept) != nil || kept {
		t.Fatalf("once a promotion started: keep %q, a still kept %v (%v)", keep, kept, err)
	}
}

// Production has one writer: the deploy-row writer the org's report uses refuses production.
func TestWritePreviewTxRefusesProduction(t *testing.T) {
	p, ctx := seeded(t)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) VALUES ('gr_x', $1, $2, 'running', 'promote')", project, pkg); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := WritePreviewTx(ctx, tx, Deployed{Project: project, Env: "prod", Package: pkg, Image: image("a"), URL: "https://claims.apps.meridian.example", GateRun: "gr_x", Run: "gr_x", RunBy: "org"}); !errors.Is(err, ErrNotPreview) {
		t.Fatalf("a write of production around WriteProdTx: %v", err)
	}
}

// The org's report goes through the production writer: its history row is kept — no /run cleans the org's
// registry — and a stop pending on the run writes the row and no history, nothing going live.
func TestWriteProdTxUnderTheOrg(t *testing.T) {
	p, ctx := seeded(t)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, failure_reason, ended_at) VALUES ('gr_prom', $1, $2, 'failed', 'promote', 'interrupted', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) VALUES ('gr_stop', $1, $2, 'running', 'promote')", project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET stop_requested_at = now() WHERE id = 'gr_stop'"); err != nil {
		t.Fatal(err)
	}
	write := func(run, img string) (bool, error) {
		tx, err := p.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, live, err := WriteProdTx(ctx, tx, Deployed{Project: project, Package: pkg, Image: img, URL: "https://claims.apps.meridian.example", GateRun: run, Run: run, RunBy: "org"})
		if err != nil {
			return false, err
		}
		return live, tx.Commit(ctx)
	}
	if live, err := write("gr_prom", image("a")); err != nil || !live {
		t.Fatalf("the org's report: live %v (%v)", live, err)
	}
	if live, err := write("gr_stop", image("b")); err != nil || live {
		t.Fatalf("a report with a stop pending: live %v (%v)", live, err)
	}
	var rows int
	var kept bool
	var prod string
	if err := p.QueryRow(ctx, "SELECT count(*), bool_or(kept), (SELECT image FROM deploys WHERE env = 'prod') FROM deploy_history").Scan(&rows, &kept, &prod); err != nil || rows != 1 || !kept || prod != image("b") {
		t.Fatalf("history %d row(s), kept %v, production %s (%v)", rows, kept, prod, err)
	}
}

// Under run_by: org production's history comes from the org's reports, and a rollback ships the digest it
// recorded: the review records the org-run image, and its promotion's release names exactly that image.
func TestARollbackUnderTheOrgShipsTheRecordedDigest(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_rollback_secret_11", platform) //nolint:gosec // a test fixture, not a real secret
	a, b := image("a"), image("b")
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, label) VALUES ('pkg_9e40', '" + project + "', 'cc', 'dd', 1, 'upload', 'v1.4')",
		"INSERT INTO runs (id, project_id, package_id, state, kind, created_at, ended_at) VALUES ('gr_a', '" + project + "', '" + pkg + "', 'clean', 'promote', now() - interval '2 days', now()), ('gr_b', '" + project + "', 'pkg_9e40', 'clean', 'promote', now() - interval '1 day', now())",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ run, pkg, image string }{{"gr_a", pkg, a}, {"gr_b", "pkg_9e40", b}} { // the org's two deployed reports
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := WriteProdTx(ctx, tx, Deployed{Project: project, Package: r.pkg, Image: r.image, URL: "https://claims.apps.meridian.example", GateRun: r.run, Run: r.run, RunBy: "org"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	var ships *string
	if state, reason := waitEnded(t, ctx, p, review.ID); state != "clean" || p.QueryRow(ctx, "SELECT ships FROM runs WHERE id = $1", review.ID).Scan(&ships) != nil || ships == nil || *ships != a {
		t.Fatalf("the review under the org: %s %s, ships %v", state, why(reason), ships)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: lee, Promote: &Promotion{Image: a, GateRun: review.ID, DeployedBy: dana, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	var release, image string
	for deadline := time.Now().Add(10 * time.Second); ; {
		if p.QueryRow(ctx, "SELECT id, image FROM releases WHERE run_id = $1 AND env = 'prod' AND state = 'accepted'", run.ID).Scan(&release, &image) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rollback's release was never accepted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if image != a || len(platform.requests()) == 0 || !strings.Contains(string(platform.requests()[len(platform.requests())-1].Body), digestOf(a)) {
		t.Fatalf("the release names %s, want the recorded %s", image, a)
	}
	if err := reportOn(ctx, p, release, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the rollback under the org: %s %s", state, why(reason))
	}
}
