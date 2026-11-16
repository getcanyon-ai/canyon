package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// A package trivy names no layer of the image for is never matched, as a package of a buildpack outside Canyon's
// builder with no match is not: the built app never reads clean over it, and names it as not checked
// (contracts/finding.md, The built app).
func TestDepsKnownVulnBuiltAppNamesAnUnplacedPackage(t *testing.T) {
	g := loadBuiltAppGolden(t)
	clean := func(f imageFacts) imageFacts {
		for d, m := range f.newest {
			m.report.Results = nil
			f.newest[d] = m
		}
		return f
	}
	with := readBuiltApp(clean(g.facts(t)), g.reported(), g.ReadAt)
	without := clean(g.facts(t))
	without.unplaced = slices.DeleteFunc(without.unplaced, func(c trivy.Component) bool { return c.Name == "left-pad" })
	if with.unchecked != readBuiltApp(without, g.reported(), g.ReadAt).unchecked+1 {
		t.Fatalf("a package with no layer is not counted as not checked: %d", with.unchecked)
	}
	if _, why := with.row("v1.4", "v1.4 passed review: no blocking finding"); !strings.HasSuffix(why, "; Canyon couldn't check 3 packages in it") {
		t.Fatalf("the clean row: %q", why)
	}
}

// A failed match of the app layers deps.known-vuln alone reads marks its read of the built app, never the platform
// rule's read of the same image (design/PATCHING.md §3.2).
func TestAnAppLayersFailedRescanMarksTheBuiltAppAlone(t *testing.T) {
	g := loadBuiltAppGolden(t)
	f := g.facts(t)
	f.appRescanFailed = true
	if read := readImage(f, "preview", "", gatepolicy.Advisory, "", g.ReadAt); read.State != "checked" {
		t.Fatalf("the platform read: %s %q", read.State, read.Why)
	}
	if app := readBuiltApp(f, g.reported(), g.ReadAt); app.why != "its last rescan did not finish" {
		t.Fatalf("the built app: %+v", app)
	}
}

// Placing deps.known-vuln before production asks the images the apps run read at once (design/PATCHING.md §3.6):
// an apply in force now asks image.sbom of preview's image, as a deploy asks it; one with a grace asks nothing,
// and its images wait for the next rescan tick.
func TestPlacingDepsBeforeProductionAsksTheBuiltApp(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	asked := func() int {
		t.Helper()
		ws, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDPrefix("claims-portal@"+executortest.Digest+"@policy@"))
		if err != nil {
			t.Fatal(err)
		}
		return len(ws)
	}
	before := asked()
	defer builtAppAsked(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"advisory","grace_days":3}`); code != 200 {
		t.Fatalf("placing it with a grace: %d %s", code, lastBody)
	}
	if n := asked(); n != before {
		t.Fatalf("an edit with a grace asked %d image reads", n-before)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing it now: %d %s", code, lastBody)
	}
	if n := asked(); n != before+1 {
		t.Fatalf("placing it now asked %d image reads of preview's image, want 1", n-before)
	}
}

// The promotion's deploy step clears a review only as the receipt would (contracts/routes.md, the approve rows):
// with deps.known-vuln placed blocking before production, a built app it counts a package in, or one it could not
// check, clears nothing; a clean one clears.
func TestClearsRefusesOnTheBuiltApp(t *testing.T) {
	pool, ctx, base, c, run := previewed(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing deps.known-vuln blocking before production: %d %s", code, lastBody)
	}
	clears := func() bool {
		t.Helper()
		ok, err := pipe.Clears(ctx, "claims-portal", run)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	unseed := func() { // the project's scan, and the matches of its slices
		t.Helper()
		for _, q := range []string{"DELETE FROM part_matches WHERE part_sha IN (SELECT part_sha FROM image_layers WHERE project_id = 'claims-portal')",
			"DELETE FROM scanned_images WHERE project_id = 'claims-portal'"} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	builtAppAsked(t) // the apply's own read, ended before the store is set by hand
	unseed()
	if clears() {
		t.Fatal("a built app no scan checked clears the promotion")
	}
	seedPlatform(t, ctx, pool, loadBuiltAppGolden(t).platformGolden, "claims-portal", executortest.Digest)
	if clears() {
		t.Fatal("a built app holding packages its review didn't see clears the promotion")
	}
	unseed()
	g := loadPlatformGolden(t) // the platform golden, its app layers matched clean
	for _, d := range []string{"sha256:" + strings.Repeat("4", 64), "sha256:" + strings.Repeat("6", 64)} {
		m := g.Matches[0]
		m.DiffID, m.Answer.Report, m.Answer.Unscanned = d, json.RawMessage(`{"SchemaVersion":2,"Results":[]}`), nil
		g.Matches = append(g.Matches, m)
	}
	seedPlatform(t, ctx, pool, g, "claims-portal", executortest.Digest)
	if !clears() {
		t.Fatal("a clean built app clears nothing")
	}
}

// builtAppAsked waits for every image read an apply asked of claims-portal's preview image to end.
func builtAppAsked(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		ws, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDPrefix("claims-portal@"+executortest.Digest+"@policy@"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(ws, func(w dbos.WorkflowStatus) bool {
			return w.Status == dbos.WorkflowStatusEnqueued || w.Status == dbos.WorkflowStatusPending
		}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the image read the apply asked never ended")
		}
	}
}
