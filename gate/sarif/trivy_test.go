package sarif

import (
	"os"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// lockfile is the tree of the lockfile the trivy fixture was scanned on.
func lockfile(t *testing.T) gate.Tree {
	t.Helper()
	return treeAt(t, "testdata/trivy-lockfile")
}

// A dependency result — one of trivy's whose rule's properties.tags carries vulnerability — is not
// refused for quoting its region: trivy's advisory link and the lockfile's resolved URL share
// https:// (finding.md, Normalised at attach; canyonv2-633i). The fixture is trivy 0.74.0's real
// report on testdata/trivy-lockfile, whose 7 results it places at the package entries' lines.
// The policy table maps each under deps.known-vuln, with its fix; the advisory leads the problem.
func TestATrivyDependencyResultKeepsItsPackageEntry(t *testing.T) {
	raw, err := os.ReadFile("testdata/trivy-0.74.0-must-fail.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := NormalizeAs(raw, snapshot(t), lockfile(t), "trivy")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tool != "trivy" || rep.Version != "0.74.0" || len(rep.Results) != 7 || rep.Dropped != 0 {
		t.Fatalf("tool %s %s, %d results, %d dropped", rep.Tool, rep.Version, len(rep.Results), rep.Dropped)
	}
	packages := map[string]int{}
	for _, r := range rep.Results {
		pl := r.Locations[0].PhysicalLocation
		_, pkg, _ := strings.Cut(r.Properties.Canyon.Problem, ". Package: ")
		pkg, _, _ = strings.Cut(pkg, "\n")
		packages[pkg]++
		want := map[string][2]int{"lodash": {20, 23}, "xlsx": {24, 27}}[pkg]
		if !strings.HasPrefix(r.RuleID, "deps.known-vuln.trivy.CVE-") || r.Properties.Canyon.PolicyName != "No known-vulnerable dependencies" ||
			pl.ArtifactLocation.URI != "package-lock.json" || pl.Region.StartLine != want[0] || pl.Region.EndLine != want[1] || r.Properties.Canyon.Fix != policy.Table[2].Fix {
			t.Errorf("%s at %s:%d-%d, fix %q", r.RuleID, pl.ArtifactLocation.URI, pl.Region.StartLine, pl.Region.EndLine, r.Properties.Canyon.Fix)
		}
	}
	if packages["lodash"] != 5 || packages["xlsx"] != 2 {
		t.Errorf("packages named: %v", packages)
	}
}

// Only the region-text half of the check stands down, and only on a dependency result: a live key
// in a trivy vulnerability's message is refused as any other; a hidden region of a result that is not
// trivy's, or not tagged vulnerability, still keeps no 8 code points of its region — they come back
// masked. Past three lines such a region is shown by its first three (#1834), so these are hidden by
// their credential tag.
func TestOnlyATrivyDependencyResultMayQuoteItsRegion(t *testing.T) {
	masked := Invalid{"masked, not refused"}       // the case's text comes back masked
	entry := at("package-lock.json", 20, 1, 23, 1) // the lodash entry
	entry["ruleId"] = "CVE-2021-23337"
	tagged := func(tool string, tags ...string) func(run obj) {
		return func(run obj) {
			run["tool"].(obj)["driver"] = obj{"name": tool, "rules": []any{obj{"id": "CVE-2021-23337", "properties": obj{"tags": tags}}}}
		}
	}
	quoting := func(text string) obj {
		r := at("package-lock.json", 20, 1, 23, 1)
		r["ruleId"] = "CVE-2021-23337"
		r["message"] = obj{"text": text}
		return r
	}
	for name, c := range map[string]struct {
		tool string
		edit func(run obj)
		r    obj
		want error
	}{
		"trivy's vulnerability quoting its region": {"trivy", tagged("Trivy", "vulnerability", "security", "HIGH"), quoting(`Link: https://avd.aquasec.com/nvd/cve-2021-23337`), nil},
		"trivy's vulnerability with a live key":    {"trivy", tagged("Trivy", "vulnerability", "security", "HIGH"), quoting(liveKey("sk")), Invalid{"result 0: message.text carries a live key"}},
		"trivy's misconfiguration quoting":         {"trivy", tagged("Trivy", "misconfiguration", "CWE-798", "HIGH"), quoting(`"resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.20.tgz"`), masked},
		"trivy's result not tagged vulnerability":  {"trivy", tagged("Trivy", "CWE-798"), quoting(`https://registry`), masked},
		"trivy's result with no rule, shown":       {"trivy", nil, quoting(`https://registry`), nil}, // past three lines, its first three are its snippet: quoting them hides nothing
		"checkov's result quoting":                 {"checkov", tagged("Checkov", "vulnerability", "CWE-798"), quoting(`https://registry`), masked},
		"a semgrep upload tagged like trivy's":     {"", tagged("semgrep", "vulnerability", "CWE-798"), quoting(`https://registry`), masked},
		"trivy's vulnerability with a quoting help": {"trivy", func(run obj) {
			tagged("Trivy", "vulnerability")(run)
			run["tool"].(obj)["driver"].(obj)["rules"].([]any)[0].(obj)["help"] = obj{"text": `Link: https://avd.aquasec.com/nvd/cve-2021-23337`}
		}, entry, nil},
	} {
		t.Run(name, func(t *testing.T) {
			raw := sarifOf(t, c.edit, c.r)
			rep, err := NormalizeAs(raw, snapshot(t), lockfile(t), c.tool)
			if c.want == masked {
				quoted := c.r["message"].(obj)["text"].(string)
				if err != nil || len(rep.Results) != 1 || !strings.Contains(rep.Results[0].Properties.Canyon.Problem, "••••") ||
					strings.Contains(rep.Results[0].Properties.Canyon.Problem, quoted) {
					t.Fatalf("the quoted text is not masked: %+v %v", rep.Results, err)
				}
				return
			}
			if err != c.want {
				t.Fatalf("got %#v, want %#v", err, c.want)
			}
		})
	}
}

// Trivy's title is its advisory database's, and some read the package twice — `lodash: lodash:
// Arbitrary code execution…`, in the fixture as trivy wrote it: the first copy is left out, once, in
// any ASCII case, and the problem and title both read it once (contracts/finding.md, the tester run).
func TestATitleThatNamesItsPackageTwiceNamesItOnce(t *testing.T) {
	for in, want := range map[string]string{
		"lodash: lodash: Arbitrary code execution": "lodash: Arbitrary code execution. m",
		"lodash: Lodash: Prototype pollution":      "Lodash: Prototype pollution. m",
		"lodash: lodash: lodash: thrice":           "lodash: lodash: thrice. m",
		"nodejs-lodash: command injection":         "nodejs-lodash: command injection. m",
		"lodash: lodashx: not the same":            "lodash: lodashx: not the same. m",
		": : empty":                                ": : empty. m",
		"no colon at all.":                         "no colon at all. m",
	} {
		if got := described(in, "m"); got != want {
			t.Errorf("described(%q) = %q, want %q", in, got, want)
		}
	}
	raw, err := os.ReadFile("testdata/trivy-0.74.0-must-fail.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := NormalizeAs(raw, snapshot(t), lockfile(t), "trivy")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) == 0 {
		t.Fatal("the fixture's report normalised to no result")
	}
	for _, r := range rep.Results {
		if strings.Contains(r.Properties.Canyon.Title, "lodash: lodash:") || strings.Contains(r.Properties.Canyon.Problem, "lodash: lodash:") {
			t.Errorf("%s still names its package twice: %q", r.RuleID, r.Properties.Canyon.Title)
		}
	}
}
