package runs

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// The proof (contracts/routes.md, Patch runs, step 4): only the run image under the app's layers changed. Any
// other fact that differs or is missing names the first of the layers above it, the build metadata, how the app
// starts and the stack, in the line's words; another run image under the rebased image is the plan's, moved.
func TestPatchRunChangedEntrypointLayersChanged(t *testing.T) {
	run := "sha256:" + strings.Repeat("c", 64)
	good := executortest.RebasedOf("claims-portal", run)
	if what, moved := proofOf(good.Before, good.After, run); what != "" || moved {
		t.Fatalf("only the run image moved: %q, moved %v", what, moved)
	}
	for name, c := range map[string]struct {
		change func(r *rebasedFacts)
		want   string
	}{
		"a changed Entrypoint": {func(r *rebasedFacts) { r.after.Config = "sha256:" + strings.Repeat("9", 64) }, "how the app starts"},
		"a layer above it changed": {func(r *rebasedFacts) {
			r.after.DiffIDs = []string{r.after.DiffIDs[0], "sha256:" + strings.Repeat("1", 64), r.after.DiffIDs[2]}
		}, "the app's own files"},
		"a layer above it more":        {func(r *rebasedFacts) { r.after.DiffIDs = append(r.after.DiffIDs, "sha256:"+strings.Repeat("2", 64)) }, "the app's own files"},
		"no top layer":                 {func(r *rebasedFacts) { r.before.TopLayer = "" }, "the app's own files"},
		"a top layer that never moved": {func(r *rebasedFacts) { r.after.TopLayer, r.after.DiffIDs[0] = r.before.TopLayer, r.before.TopLayer }, "the app's own files"},
		"the build metadata":           {func(r *rebasedFacts) { r.after.BuildMetadata = "sha256:" + strings.Repeat("3", 64) }, "the build settings"},
		"no build metadata":            {func(r *rebasedFacts) { r.before.BuildMetadata, r.after.BuildMetadata = "", "" }, "the build settings"},
		"another stack":                {func(r *rebasedFacts) { r.after.StackID = "io.buildpacks.stacks.jammy" }, "the Ubuntu release it's built on"},
		"a config and a stack":         {func(r *rebasedFacts) { r.after.Config, r.after.StackID = "sha256:"+strings.Repeat("9", 64), "x" }, "how the app starts"},
	} {
		f := rebasedFacts{good.Before, good.After}
		f.after.DiffIDs = append([]string{}, good.After.DiffIDs...)
		c.change(&f)
		if what, _ := proofOf(f.before, f.after, run); what != c.want {
			t.Errorf("%s: %q differs, want %q", name, what, c.want)
		}
	}
	if what, moved := proofOf(good.Before, good.After, "sha256:"+strings.Repeat("d", 64)); what != "" || !moved {
		t.Fatalf("another run image than the plan's: %q, moved %v", what, moved)
	}
}

// A refused patch says why in words a developer knows, never the executor's message, which names the label, run
// image or stack (#2614); a code this build does not know names no reason, and each says its next deploy fixes it.
func TestARefusedPatchSaysItInPlainWords(t *testing.T) {
	for code, want := range map[string]string{
		"not_rebasable":     "Canyon can't patch 7d3e8f: how it was built doesn't let Canyon swap in new system libraries, so its next deploy picks up the fix",
		"another_run_image": "Canyon can't patch 7d3e8f: it was built on different system libraries from the ones Canyon fixes, so its next deploy picks up the fix",
		"another_stack":     "Canyon can't patch 7d3e8f: it was built on another Ubuntu release, so its next deploy picks up the fix",
		"a_new_code":        "Canyon can't patch 7d3e8f, so its next deploy picks up the fix",
	} {
		if got := refusedLine("7d3e8f", code); got != want {
			t.Errorf("%s: %q, want %q", code, got, want)
		}
	}
}

// rebasedFacts are a rebase's two images' facts, for a case to change.
type rebasedFacts struct{ before, after executor.RebaseFacts }

// What changed is named by the first part that differs, in the changed line's order; the fingerprint alone
// decides that something did.
func TestPatchRunConfigChangedNamesWhat(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	was := func() RunConfig {
		return RunConfig{Egress: Egress{Via: "canyon"}, RunBy: "canyon", FrontDoor: &SavedDoor{Door: json.RawMessage(`{"domain":"apps.example"}`), SetAt: at},
			Secrets: map[string]Secret{"DATABASE_URL": {Version: 2, SetAt: at}, "STRIPE_SECRET_KEY": {Version: 1, SetAt: at}}, Resources: []string{"db"}}
	}
	for want, change := range map[string]func(*RunConfig){
		"Its outbound setting":         func(c *RunConfig) { c.Egress.Via = "off"; c.Secrets["DATABASE_URL"] = Secret{Version: 3, SetAt: at} },
		"Who runs it":                  func(c *RunConfig) { c.RunBy = "org" },
		"Its front door":               func(c *RunConfig) { c.FrontDoor.SetAt = at.Add(time.Second) },
		"The secret DATABASE_URL":      func(c *RunConfig) { c.Secrets["DATABASE_URL"] = Secret{Version: 3, SetAt: at} },
		"The secret STRIPE_SECRET_KEY": func(c *RunConfig) { delete(c.Secrets, "STRIPE_SECRET_KEY") },
		"The secret API_TOKEN":         func(c *RunConfig) { c.Secrets["API_TOKEN"] = Secret{Version: 1, SetAt: at}; c.Resources = nil },
		"The resource db":              func(c *RunConfig) { c.Resources = nil },
		"Its configuration":            func(*RunConfig) {},
	} {
		now := was()
		change(&now)
		if got := changedWhat(was(), now); got != want {
			t.Errorf("changed %q, want %q", got, want)
		}
	}
}

// Regression (contracts/routes.md, Patch runs, step 6): a counted finding at or above high the replaced image
// lacked regresses; a patch that fixed none of the plan's is ineffective; else each it fixed, in the plan's order.
func TestPatchRunRegressed(t *testing.T) {
	old := []PlatformFinding{libssl3("high", "CVE-2024-6119"), libssl3("critical", "CVE-2024-5535"), libssl3("medium", "CVE-2024-1")}
	fixes := old[:2]
	if r, fixed := regressionOf(old, []PlatformFinding{libssl3("high", "CVE-2024-5535")}, fixes); r != nil || len(fixed) != 1 || fixed[0].Vulnerability != "CVE-2024-6119" {
		t.Fatalf("one of two fixed: %+v %+v", r, fixed)
	}
	newHigh := PlatformFinding{Name: "libc6", Version: "2.39-0ubuntu8.4", Vulnerability: "CVE-2025-7", Fixed: "2.39-0ubuntu8.5", Class: "os", Severity: "high"}
	if r, _ := regressionOf(old, []PlatformFinding{newHigh}, fixes); r == nil || *r != newHigh {
		t.Fatalf("a new high: %+v", r)
	}
	if r, _ := regressionOf(old, []PlatformFinding{{Name: "libc6", Vulnerability: "CVE-2025-8", Severity: "medium"}}, fixes); r != nil {
		t.Fatalf("a new medium is no regression: %+v", r)
	}
	if r, _ := regressionOf(old, []PlatformFinding{libssl3("medium", "CVE-2024-1")}, fixes); r != nil {
		t.Fatalf("a finding the replaced image had is none: %+v", r)
	}
	if r, fixed := regressionOf(old, old, fixes); r != nil || len(fixed) != 0 {
		t.Fatalf("nothing fixed is ineffective: %+v %+v", r, fixed)
	}
}

// Step 6's fixed list (contracts/routes.md, Patch runs): each to is the version the patched image holds of its package, by
// name and class, never the advisory's fixed-in list; removed when it holds none; a class on a rebuild's alone; and one
// line per package, its vulnerabilities in the plan's order (#2631).
func TestFixedReadsWhatThePatchedImageHolds(t *testing.T) {
	containerd := "github.com/containerd/containerd/v2"
	fixes := []PlatformFinding{
		{Name: "tar", Version: "7.5.11", Vulnerability: "CVE-2026-1", Fixed: "7.5.19", Class: "runtime"},
		{Name: containerd, Version: "v2.3.3", Vulnerability: "CVE-2026-2", Fixed: "2.2.7, 2.3.4", Class: "runtime"},
		{Name: "tar", Version: "7.5.11", Vulnerability: "CVE-2026-3", Fixed: "7.5.18", Class: "runtime"},
		{Name: "google.golang.org/grpc", Version: "v1.83.0", Vulnerability: "CVE-2026-4", Fixed: "1.83.2", Class: "runtime"},
	}
	after := []heldPackage{{Name: "tar", Class: "os", Version: "1.35"}, {Name: "tar", Class: "runtime", Version: "7.5.20"}, {Name: containerd, Class: "runtime", Version: "v2.3.4"}}
	got := fixedOf(fixes, after, true)
	want := []PatchFixed{
		{Name: "tar", From: "7.5.11", To: "7.5.20", Vulnerability: "CVE-2026-1", Class: "runtime"},
		{Name: containerd, From: "v2.3.3", To: "v2.3.4", Vulnerability: "CVE-2026-2", Class: "runtime"},
		{Name: "tar", From: "7.5.11", To: "7.5.20", Vulnerability: "CVE-2026-3", Class: "runtime"},
		{Name: "google.golang.org/grpc", From: "v1.83.0", To: "removed", Vulnerability: "CVE-2026-4", Class: "runtime"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a rebuild's fixed:\n got %+v\nwant %+v", got, want)
	}
	lines := []string{
		"Fixed: tar 7.5.11 → 7.5.20 (CVE-2026-1, CVE-2026-3)",
		"Fixed: " + containerd + " v2.3.3 → v2.3.4 (CVE-2026-2)",
		"Fixed: google.golang.org/grpc v1.83.0 → removed (CVE-2026-4)",
	}
	if l := fixedLines(got); !slices.Equal(l, lines) {
		t.Fatalf("its lines:\n got %q\nwant %q", l, lines)
	}
	twice := append(after, heldPackage{Name: containerd, Class: "runtime", Version: "v2.2.7"})
	if x := fixedOf(fixes[1:2], twice, false); len(x) != 1 || x[0].To != "v2.3.4, v2.2.7" || x[0].Class != "" {
		t.Fatalf("a rebase's fix of a package the image holds twice: %+v", x)
	}
}

// A deploy row recorded what a patch's config check compares against only with a fingerprint, its spec, a via this
// server runs and parts that read (#265): any one missing fails that check every time, so the plan never names the
// preview (design/PATCHING.md §4.2), and the check reads it as changed.
func TestADeployRowWithNoFingerprintRecordedNothingToCompare(t *testing.T) {
	print, off, parts := strings.Repeat("a", 64), &Egress{Via: "off"}, []byte(`{"egress":{"via":"off"},"run_by":"canyon"}`)
	for _, c := range []struct {
		why         string
		spec        *Egress
		parts       []byte
		fingerprint *string
		want        bool
	}{
		{"recorded", off, parts, &print, true},
		{"through Canyon's gateway", &Egress{Via: "canyon"}, parts, &print, true},
		{"deployed before the fingerprint", off, nil, nil, false},
		{"parts but no fingerprint", off, parts, nil, false},
		{"no spec", nil, parts, &print, false},
		{"a via this server does not run", &Egress{Via: "elsewhere"}, parts, &print, false},
		{"parts that do not read", off, []byte(`[]`), &print, false},
	} {
		if got := ConfigRecorded(c.spec, c.parts, c.fingerprint); got != c.want {
			t.Errorf("%s: recorded %v, want %v", c.why, got, c.want)
		}
	}
}

// A patch run that ends failed says so in the server's log, with its run, its app and why (#265).
func TestAFailedPatchRunSaysWhyInTheLog(t *testing.T) {
	if got, want := patchFailedLine("gr_81f332d7e3b88a27", "case-next-live", "not_platform_only", "config_changed"),
		"runs: run gr_81f332d7e3b88a27 (case-next-live): the patch run failed: not_platform_only (config_changed)"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got, want := patchFailedLine("gr_1", "app", "patch_regressed", ""), "runs: run gr_1 (app): the patch run failed: patch_regressed"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
