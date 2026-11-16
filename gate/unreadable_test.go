package gate_test

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// couldNotCheck is the scanners' run over a tree of files with tool's could-not-check added, derived
// and checked against the canonical snapshot: the result it added.
func couldNotCheck(t *testing.T, files map[string]string, tool string) finding.Result {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := cmp.Or(os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o750), os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	log, err := gate.Scanners(dir, snap, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gate.Walk(dir)
	if err := cmp.Or(err, gate.CouldNotCheck(&log, tree, tool, "did not finish in time"), finding.Derive(&log, snap), finding.Check(&log, snap)); err != nil {
		t.Fatal(err)
	}
	results := log.Runs[0].Results
	return results[len(results)-1]
}

// A tool Canyon runs whose scan did not finish is one blocking gate.unreadable finding in the scanners'
// run (contracts/finding.md, Canyon-run tools): the contract's problem line, at the first file, line 1,
// columns 1–2, its snippet the tool's own form over the digest of the tool's name, then every file,
// framed. So another tool, or the same tool on an edited tree, is another fingerprint.
func TestAToolThatDidNotFinishIsOneBlockingFinding(t *testing.T) {
	files := map[string]string{"package.json": "{}", "src/a.ts": "export const a = 1"}
	trivy := couldNotCheck(t, files, "trivy")
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "5:trivy12:package.json%x8:src/a.ts%x", sha256.Sum256([]byte("{}")), sha256.Sum256([]byte("export const a = 1")))
	region := trivy.Locations[0].PhysicalLocation.Region
	if trivy.RuleID != finding.Unreadable.ID || trivy.Properties.Canyon.Title != "Canyon could not check this package with trivy" ||
		trivy.Properties.Canyon.Problem != "could not check: trivy: did not finish in time" || !*trivy.Properties.Canyon.Blocks ||
		trivy.Locations[0].PhysicalLocation.ArtifactLocation.URI != "package.json" ||
		region.StartLine != 1 || region.StartColumn != 1 || region.EndLine != 1 || region.EndColumn != 2 ||
		region.Snippet.Text != fmt.Sprintf("(trivy could not check this tree; tree sha256:%x)", h.Sum(nil)) {
		t.Fatalf("trivy's could-not-check: %+v %+v", trivy.Properties.Canyon, region)
	}
	grype := couldNotCheck(t, files, "grype")
	files["src/a.ts"] = "export const a = 2"
	if edited := couldNotCheck(t, files, "trivy"); grype.PartialFingerprints == trivy.PartialFingerprints || edited.PartialFingerprints == trivy.PartialFingerprints {
		t.Fatalf("one fingerprint for two tools or two trees: %s %s %s", trivy.PartialFingerprints.CanyonV1, grype.PartialFingerprints.CanyonV1, edited.PartialFingerprints.CanyonV1)
	}
}

// Every tool Canyon runs that can fail to finish has a could-not-check the profile passes: an ESLint scan
// that did not finish once failed the whole run, its snippet refused, instead of blocking it.
func TestEveryCanyonRunToolsCouldNotCheckPassesTheProfile(t *testing.T) {
	for _, tool := range []string{"trivy", "opengrep", "eslint", "grype", "checkov"} {
		if r := couldNotCheck(t, map[string]string{"src/a.ts": "export const a = 1"}, tool); !*r.Properties.Canyon.Blocks {
			t.Errorf("%s's could-not-check does not block", tool)
		}
	}
}

// A could-not-check finding names who may approve it as design/PERMISSIONS.md has it: a person who owns
// or contributes to the app, or an admin — not an admin alone, which sent an owner who could approve it
// to look for one (canyonv2-isj7), and not "the app's team", whose read-only people cannot.
func TestACouldNotCheckFindingNamesWhoMayApproveIt(t *testing.T) {
	r := couldNotCheck(t, map[string]string{"package.json": "{}", "src/a.ts": "export const a = 1"}, "trivy")
	if fix := r.Properties.Canyon.Fix; !strings.Contains(fix, "have an admin or a security responder who didn't start this review approve") {
		t.Errorf("%q", fix)
	}
}
