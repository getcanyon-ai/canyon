package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// The contract's imported golden (contracts/testdata/finding/imported-semgrep.sarif.json) is what the
// gate makes of semgrep's result on the claims-portal app, as contracts/validate.py reads it: New,
// showing the region as a Canyon finding's, builds the result from the fixture file; canyon/v1 frames the region's
// HMAC-SHA-256 under the test-only key of 32 zero bytes; and the log passes Check against a snapshot
// an admin added sarif.semgrep to — the one rule every sarif.semgrep.<ruleId> reads.
func TestAnImportedFindingIsTheContractGolden(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/finding/imported-semgrep.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	golden := decode(t, raw)
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.semgrep", Name: "Findings from semgrep", State: policy.Advisory, Producer: policy.Sarif})
	if err := finding.Check(&golden, snap); err != nil {
		t.Fatalf("the imported golden against a snapshot carrying sarif.semgrep: %v", err)
	}

	tree, err := Walk(filepath.Join("testdata", "apps", "claims-portal"))
	if err != nil {
		t.Fatal(err)
	}
	run := golden.Runs[0]
	want := run.Results[0]
	pl, c := want.Locations[0].PhysicalLocation, want.Properties.Canyon
	src, err := tree.Read(pl.ArtifactLocation.URI)
	if err != nil {
		t.Fatal(err)
	}
	g := pl.Region
	got, err := finding.New(finding.Input{Rule: run.Tool.Driver.Rules[0], Severity: c.Severity, Title: c.Title, Problem: c.Problem, Fix: c.Fix,
		RequiredEndState: c.RequiredEndState, DoNot: c.DoNot,
		Location: finding.Span{URI: pl.ArtifactLocation.URI, StartLine: g.StartLine, StartColumn: g.StartColumn, EndLine: g.EndLine, EndColumn: g.EndColumn,
			Lines: strings.Split(string(src), "\n")[g.StartLine-1 : g.EndLine]}})
	if err != nil {
		t.Fatal(err)
	}
	got.RelatedLocations = nil // New writes [], the golden omits the field: one thing to SARIF and the profile
	run.Results = []finding.Result{got}
	built := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{run}}
	if err := finding.Derive(&built, snap); err != nil {
		t.Fatal(err)
	}
	if err := FingerprintImported(&built, tree, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built, golden) {
		out, _ := json.Marshal(built)
		t.Fatalf("the gate's imported result is not the contract's:\n got: %s\nwant: %s", out, raw)
	}

	// The exception is sarif.<tool> read for sarif/<tool>'s own results, and nothing wider.
	for name, c := range map[string]struct {
		mutate func(*finding.Log)
		want   string
	}{
		"another tool's rule in semgrep's run": {func(l *finding.Log) {
			l.Runs[0].Results[0].RuleID = "sarif.gitleaks.generic-api-key"
			l.Runs[0].Tool.Driver.Rules[0].ID = "sarif.gitleaks.generic-api-key"
		}, "outside the namespace"},
		"semgrep's rule in the scanners' run": {func(l *finding.Log) {
			l.Runs[0].Tool.Driver.Name = policy.Scanner.Driver()
		}, "not in the policy snapshot"},
	} {
		t.Run(name, func(t *testing.T) {
			l := decode(t, raw)
			c.mutate(&l)
			if err := finding.Check(&l, snap); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
	// One tool's two categories carrying one identical result: the occurrence counts across the
	// log's runs, so the second is n = 1 and a fingerprint of its own, never the first one again.
	two := decode(t, raw)
	two.Runs = append(two.Runs, decode(t, raw).Runs[0])
	two.Runs[0].AutomationDetails, two.Runs[1].AutomationDetails = &finding.AutomationDetails{ID: "a"}, &finding.AutomationDetails{ID: "b"}
	if err := FingerprintImported(&two, tree, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := finding.Check(&two, snap); err != nil {
		t.Fatalf("two categories carrying one identical result: %v", err)
	}
	if two.Runs[0].Results[0].PartialFingerprints != want.PartialFingerprints {
		t.Error("the first category's result is not the golden's fingerprint")
	}
	// A fingerprint the key or the file cannot vouch for is never derived.
	for name, key := range map[string][]byte{"no key": nil, "a short key": make([]byte, 16)} {
		l := decode(t, raw)
		if err := FingerprintImported(&l, tree, key); err == nil {
			t.Errorf("%s: derived a fingerprint", name)
		}
	}
	past := decode(t, raw)
	past.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.EndColumn = 200
	if err := FingerprintImported(&past, tree, make([]byte, 32)); err == nil {
		t.Error("derived a fingerprint over a region past its line")
	}
}

func decode(t *testing.T, raw []byte) finding.Log {
	t.Helper()
	var l finding.Log
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}
