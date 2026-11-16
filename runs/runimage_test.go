package runs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// runImageOf is the golden image's run image as its own scan answers it: the run image's layers, the two at
// the bottom, with the operating system and the packages they hold, libssl3 at version.
func (g platformGolden) runImageOf(t *testing.T, version string) executor.ScanResult {
	t.Helper()
	res := g.withSBOM(t, func(cs []any) []any {
		var out []any
		for _, c := range cs {
			m := c.(map[string]any)
			switch m["name"] {
			case "libssl3":
				m["version"] = version
				out = append(out, m)
			case "ubuntu", "libc6":
				out = append(out, m)
			}
		}
		return out
	})
	res.DiffIDs, res.Lifecycle = g.Image.DiffIDs[:2], ""
	return res
}

// runImageScan is what is stored of the run image's scan of digest: the run image it was read as, why it did
// not finish if it did not, whether its layers were matched, and each layer's slice; ok false when none is.
func runImageScan(t *testing.T, ctx context.Context, p *pgxpool.Pool, digest string) (run, notFinished string, matched bool, parts map[string]string, ok bool) {
	t.Helper()
	err := p.QueryRow(ctx, "SELECT run_image, coalesce(not_finished, ''), rematched_at IS NOT NULL FROM run_image_scans WHERE digest = $1", digest).Scan(&run, &notFinished, &matched)
	if err == pgx.ErrNoRows {
		return "", "", false, nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return run, notFinished, matched, slicesOf(t, ctx, p, "SELECT diff_id, part_sha FROM run_image_layers WHERE digest = $1", digest), true
}

// slicesOf is each diff ID's slice as query answers them.
func slicesOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var d, part string
	if _, err := pgx.ForEachRow(rows, []any{&d, &part}, func() error { out[d] = part; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each tick reads the run image GET /builder names while no finished scan of it is stored — one that did not
// finish is asked again — keeps it under its digest, and matches its layers with the apps': a slice an app
// on it holds is the same bytes, matched once for both. A finished scan is never read again, and while no
// image runs in a project whose rule is on, nothing is read (design/PATCHING.md §3.2).
func TestRescanScansAndMatchesTheRunImage(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	asked := rematching(t, f, g)
	run := "sha256:" + strings.Repeat("c", 64) // the fake's GET /builder run_image
	f.RunImageAnswer.Store(&executor.ScanResult{Tool: "trivy", NotFinished: "timeout"})
	tick(t, time.Now())
	if f.RunImageScans.Load() != 0 {
		t.Fatalf("no image runs, yet the run image was read")
	}
	digest := freshImage(t, f)
	storedGolden(t, ctx, p, &g, digest)
	was := pipe.ProjectPolicy.Load()
	pipe.ProjectPolicy.Store(func(context.Context, Querier, string) (policy.Snapshot, error) { return testPolicy(), nil })
	tick(t, time.Now())
	pipe.ProjectPolicy.Store(was)
	if f.RunImageScans.Load() != 0 {
		t.Fatalf("the rule off, yet the run image was read")
	}
	tick(t, time.Now())
	if _, notFinished, _, _, ok := runImageScan(t, ctx, p, run); !ok || notFinished != "timeout" || f.RunImageScans.Load() != 1 {
		t.Fatalf("a scan that did not finish: %q stored %v, %d reads", notFinished, ok, f.RunImageScans.Load())
	}
	answer := g.runImageOf(t, "3.0.13-0ubuntu3.4")
	f.RunImageAnswer.Store(&answer)
	before := len(asked())
	tick(t, time.Now())
	gotRun, notFinished, matched, parts, ok := runImageScan(t, ctx, p, run)
	if !ok || gotRun != run || notFinished != "" || !matched || f.RunImageScans.Load() != 2 {
		t.Fatalf("read again and matched: %q %q matched %v, %d reads", gotRun, notFinished, matched, f.RunImageScans.Load())
	}
	apps := slicesOf(t, ctx, p, "SELECT diff_id, part_sha FROM image_layers WHERE project_id = $1 AND digest = $2", project, digest)
	if len(parts) != 2 || parts[g.Image.DiffIDs[0]] != apps[g.Image.DiffIDs[0]] || parts[g.Image.DiffIDs[1]] != apps[g.Image.DiffIDs[1]] {
		t.Fatalf("the run image's slices %v are the app's own %v", parts, apps)
	}
	if again := asked()[before:]; len(again) != len(g.covered(t)) {
		t.Fatalf("the run image's slices are the app's, matched once in the tick: asked %v, the app covers %v", again, g.covered(t))
	}
	tick(t, time.Now())
	if f.RunImageScans.Load() != 2 {
		t.Fatalf("a finished scan read again: %d reads", f.RunImageScans.Load())
	}
}

// A run image GET /builder no longer names is dropped at the next tick, and the new one read and kept under its
// own digest, in Kubernetes' form; a slice of the old one that no app holds goes with the sweep after, while a
// slice an app holds stays (design/PATCHING.md §3.2).
func TestRescanDropsAMovedRunImage(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	rematching(t, f, g)
	storedGolden(t, ctx, p, &g, freshImage(t, f))
	old := g.runImageOf(t, "3.0.13-0ubuntu3.3") // its libssl3 layer a slice of its own
	f.RunImageAnswer.Store(&old)
	tick(t, time.Now())
	first := "sha256:" + strings.Repeat("c", 64)
	_, _, _, oldParts, ok := runImageScan(t, ctx, p, first)
	if !ok {
		t.Fatal("the first run image was not kept")
	}
	moved := "registry.example/canyon-builder-run-image@sha256:" + strings.Repeat("d", 64)
	f.RunImage.Store(&moved)
	newer := g.runImageOf(t, "3.0.13-0ubuntu3.5")
	f.RunImageAnswer.Store(&newer)
	tick(t, time.Now())
	if _, _, _, _, ok := runImageScan(t, ctx, p, first); ok {
		t.Fatal("the run image GET /builder no longer names was kept")
	}
	run, notFinished, matched, _, ok := runImageScan(t, ctx, p, "sha256:"+strings.Repeat("d", 64))
	if !ok || run != moved || notFinished != "" || !matched {
		t.Fatalf("the moved run image: %q %q matched %v, stored %v", run, notFinished, matched, ok)
	}
	tick(t, time.Now())
	var left []string
	for _, d := range g.Image.DiffIDs[:2] {
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM layer_parts WHERE part_sha = $1", oldParts[d]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			left = append(left, d)
		}
	}
	if !slices.Equal(left, g.Image.DiffIDs[:1]) {
		t.Fatalf("after the sweep the old run image's slices of %v are left, want the app's shared one alone, %v", left, g.Image.DiffIDs[:1])
	}
	if img, whole, err := pipe.runImageToMatch(ctx, "sha256:"+strings.Repeat("d", 64)); err != nil || !whole || img == nil || len(img.Layers) != 2 {
		t.Fatalf("the sweep left the run image GET /builder names unread: %+v %v", img, err)
	}
	f.NoBuilder.Store(true) // an executor that names no run image: nothing kept can be read stale
	tick(t, time.Now())
	var kept int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM run_image_scans").Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("with no run image named, %d run image scans kept (%v)", kept, err)
	}
}

// mainsDropParts is the sweep of the release before this table (runs/rescan.go at 9eda55f8), which knows no
// run image: it runs on this schema during a rolling upgrade and after a rollback.
const mainsDropParts = "DELETE FROM layer_parts s WHERE s.seen_at < now() AND NOT EXISTS (SELECT 1 FROM image_layers l WHERE l.part_sha = s.part_sha)"

// The release before sweeps a slice only the run image holds, as it sweeps any slice no app's scan names: its
// sweep succeeds on this schema, so its ticks go on, and the run image's scan, missing that layer, is read
// again at this release's next tick, never matched in part nor left unread.
func TestThePreviousReleasesSweepLeavesTheRescanWhole(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	rematching(t, f, g)
	storedGolden(t, ctx, p, &g, freshImage(t, f))
	answer := g.runImageOf(t, "3.0.13-0ubuntu3.5") // its libssl3 layer a slice no app holds
	f.RunImageAnswer.Store(&answer)
	tick(t, time.Now())
	run := "sha256:" + strings.Repeat("c", 64)
	if _, _, matched, _, ok := runImageScan(t, ctx, p, run); !ok || !matched {
		t.Fatalf("the run image's first read: stored %v, matched %v", ok, matched)
	}
	if _, err := p.Exec(ctx, mainsDropParts); err != nil {
		t.Errorf("the release before's sweep fails on this schema: %v", err)
	}
	tick(t, time.Now())
	_, notFinished, matched, parts, ok := runImageScan(t, ctx, p, run)
	if !ok || notFinished != "" || !matched || len(parts) != 2 || f.RunImageScans.Load() != 2 {
		t.Errorf("after the sweep: stored %v %q, matched %v, %d layers, %d reads; want it read again, whole and matched", ok, notFinished, matched, len(parts), f.RunImageScans.Load())
	}
}

// GET /builder's run_image names its digest in either form, the image ID on Docker and <repository>@sha256 on
// Kubernetes; a tag or a short digest names none.
func TestRunImageDigestReadsBothForms(t *testing.T) {
	hex := strings.Repeat("c", 64)
	for run, want := range map[string]string{
		"sha256:" + hex: "sha256:" + hex, "registry.example/canyon-builder-run-image@sha256:" + hex: "sha256:" + hex,
		"canyon-run:noble": "", "sha256:" + hex[:63]: "", "": "",
	} {
		if got := RunImageDigest(run); got != want {
			t.Errorf("%q: %q, want %q", run, got, want)
		}
	}
}

// The run image's read never stands in for an app's own: it runs after every app's match, so it skips,
// replaces or ends none of them, and it marks its own scan alone. An app whose own match failed still reads
// could not check after the run image matched the slices they share. A run image that cannot be read or
// matched leaves each app's match as the app's own tick made it.
func TestARunImageNeverChecksAnApp(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	digest := freshImage(t, f)
	storedGolden(t, ctx, p, &g, digest)
	covered := g.covered(t)
	rematching(t, f, g, covered[len(covered)-1]) // a layer above the run image's: the app's alone
	answer := g.runImageOf(t, "3.0.13-0ubuntu3.4")
	f.RunImageAnswer.Store(&answer)
	tick(t, time.Now())
	if _, _, matched, _, ok := runImageScan(t, ctx, p, "sha256:"+strings.Repeat("c", 64)); !ok || !matched {
		t.Fatalf("the run image's own layers matched: stored %v, matched %v", ok, matched)
	}
	if _, matched, failed := scanOf(t, ctx, p, project, digest); matched || !failed {
		t.Fatalf("the run image's match checked the app: matched %v, failed %v", matched, failed)
	}
	moved := "sha256:" + strings.Repeat("e", 64)
	f.RunImage.Store(&moved)
	f.RunImageAnswer.Store(&executor.ScanResult{Tool: "trivy", NotFinished: "timeout"})
	rematching(t, f, g)
	tick(t, time.Now())
	if _, matched, failed := scanOf(t, ctx, p, project, digest); !matched || failed {
		t.Fatalf("a run image that did not read kept the app from its own match: matched %v, failed %v", matched, failed)
	}
	if _, notFinished, _, _, ok := runImageScan(t, ctx, p, moved); !ok || notFinished != "timeout" {
		t.Fatalf("the moved run image's scan: %q, stored %v", notFinished, ok)
	}
}
