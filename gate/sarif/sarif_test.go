package sarif

import (
	"archive/zip"
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

type obj = map[string]any

func snapshot(t *testing.T) *policy.Snapshot {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.semgrep", Name: "Findings from semgrep", State: policy.Advisory, Producer: policy.Sarif})
	return &snap
}

func at(uri string, startLine, startColumn, endLine, endColumn int) obj {
	return obj{"ruleId": "r", "message": obj{"text": "m"}, "locations": []any{obj{"physicalLocation": obj{
		"artifactLocation": obj{"uri": uri},
		"region":           obj{"startLine": startLine, "startColumn": startColumn, "endLine": endLine, "endColumn": endColumn}}}}}
}

func region(r obj) obj {
	return r["locations"].([]any)[0].(obj)["physicalLocation"].(obj)["region"].(obj)
}

// sarifOf is a semgrep report of one run carrying results, run's other fields set by edit.
func sarifOf(t *testing.T, edit func(run obj), results ...obj) []byte {
	run := obj{"tool": obj{"driver": obj{"name": "semgrep", "version": "1.86.0", "rules": []any{}}}, "results": results}
	if edit != nil {
		edit(run)
	}
	raw, err := json.Marshal(obj{"version": "2.1.0", "runs": []any{run}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A report of a tool Canyon ran is that tool's, whatever its driver is named: opengrep 1.30.0 writes
// "Opengrep OSS", which as an upload is a tool no admin added, and as the scan of semgrep is semgrep's.
func TestAScannedReportIsTheToolScanned(t *testing.T) {
	raw := sarifOf(t, func(run obj) { run["tool"].(obj)["driver"].(obj)["name"] = "Opengrep OSS" }, at("a.ts", 1, 7, 1, 8))
	if _, err := Normalize(raw, snapshot(t), files(t)); err != (UnknownTool{"opengrep-oss"}) {
		t.Fatalf("as an upload: %v", err)
	}
	rep, err := NormalizeAs(raw, snapshot(t), files(t), "semgrep")
	if err != nil || rep.Tool != "semgrep" || len(rep.Results) != 1 || rep.Results[0].RuleID != "sarif.semgrep.r" {
		t.Fatalf("as semgrep's scan: %+v %v", rep, err)
	}
}

// The contract's imported golden is semgrep's result on the claims-portal app as Canyon keeps it:
// normalised at attach, then derived and fingerprinted in the run under the test-only key.
func TestTheContractGoldenIsSemgrepsReportNormalised(t *testing.T) {
	var golden finding.Log
	raw, err := os.ReadFile("../../contracts/testdata/finding/imported-semgrep.sarif.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &golden)); err != nil {
		t.Fatal(err)
	}
	semgrep := at("apps/web/src/app/api/claims/route.ts", 8, 51, 8, 84)
	semgrep["ruleId"], semgrep["level"] = "typescript.nextjs.unvalidated-search-param", "warning"
	semgrep["message"] = obj{"text": golden.Runs[0].Results[0].Properties.Canyon.Problem}
	tree, snap := treeAt(t, "../testdata/apps/claims-portal"), snapshot(t)
	rep, err := Normalize(sarifOf(t, nil, semgrep), snap, tree)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tool != "semgrep" || rep.Category != "" || rep.Version != "1.86.0" || rep.Dropped != 0 || len(rep.Results) != 1 {
		t.Fatalf("the report: %+v", rep)
	}
	run := golden.Runs[0]
	run.Results = rep.Results
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{run}}
	if err := cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)), finding.Check(&log, *snap)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(log.Runs[0].Results, golden.Runs[0].Results) {
		got, _ := json.Marshal(log)
		t.Fatalf("normalised, derived and fingerprinted, the result is not the golden's:\n got: %s\nwant: %s", got, raw)
	}
}

// An error in the report refuses it whole, naming the first, and never a value from the report.
func TestAReportIsRefusedWhole(t *testing.T) {
	ok := at("a.ts", 1, 7, 1, 8)
	result := func(edit func(r obj)) []byte { // result 1 is the whole of a.ts's first line
		r := at("a.ts", 1, 1, 1, 13)
		edit(r)
		return sarifOf(t, nil, ok, r)
	}
	withRule := func(rule obj) []byte {
		return sarifOf(t, func(run obj) { run["tool"].(obj)["driver"].(obj)["rules"] = []any{rule} }, at("a.ts", 1, 7, 1, 8))
	}
	for name, c := range map[string]struct {
		raw  []byte
		want error
	}{
		"not JSON":                   {[]byte("\x1f\x8b\x08"), Invalid{"the report is not SARIF 2.1.0 JSON"}},
		"not 2.1.0":                  {[]byte(`{"version":"2.0.0","runs":[]}`), Invalid{"the report is not SARIF 2.1.0 JSON"}},
		"two runs":                   {[]byte(`{"version":"2.1.0","runs":[{},{}]}`), Invalid{"the report is not exactly one run"}},
		"no tool name":               {sarifOf(t, func(run obj) { run["tool"] = obj{"driver": obj{"name": "--"}} }), Invalid{"tool.driver.name is not 1 to 40 characters once normalised"}},
		"a tool name over 40":        {sarifOf(t, func(run obj) { run["tool"] = obj{"driver": obj{"name": strings.Repeat("s", 41)}} }), Invalid{"tool.driver.name is not 1 to 40 characters once normalised"}},
		"a category over 128":        {sarifOf(t, func(run obj) { run["automationDetails"] = obj{"id": strings.Repeat("c", 129)} }), Invalid{"automationDetails.id is over 128 characters once normalised"}},
		"an unknown tool":            {sarifOf(t, func(run obj) { run["tool"] = obj{"driver": obj{"name": "Snyk Code"}} }), UnknownTool{"snyk-code"}},
		"over 1,000 results":         {sarifOf(t, nil, slices.Repeat([]obj{ok}, 1001)...), TooManyResults{"semgrep", 1001}},
		"9 newlineSequences":         {sarifOf(t, func(run obj) { run["newlineSequences"] = strings.Split("abcdefghi", "") }, ok), Invalid{"newlineSequences is not up to 8 distinct sequences of 1 to 8 bytes"}},
		"an empty newlineSequence":   {sarifOf(t, func(run obj) { run["newlineSequences"] = []string{"\n", ""} }, ok), Invalid{"newlineSequences is not up to 8 distinct sequences of 1 to 8 bytes"}},
		"a newlineSequence over 8":   {sarifOf(t, func(run obj) { run["newlineSequences"] = []string{"\n", "123456789"} }, ok), Invalid{"newlineSequences is not up to 8 distinct sequences of 1 to 8 bytes"}},
		"a repeated newlineSequence": {sarifOf(t, func(run obj) { run["newlineSequences"] = []string{"\n", "\n"} }, ok), Invalid{"newlineSequences is not up to 8 distinct sequences of 1 to 8 bytes"}},
		"a columnKind":               {sarifOf(t, func(run obj) { run["columnKind"] = "bytes" }, ok), Invalid{"columnKind is not one SARIF 2.1.0 names"}},
		"no ruleId":                  {result(func(r obj) { delete(r, "ruleId") }), Invalid{"result 1: no ruleId of 1 to 128 printable ASCII characters without whitespace"}},
		"a ruleId with space":        {result(func(r obj) { r["ruleId"] = "a b" }), Invalid{"result 1: no ruleId of 1 to 128 printable ASCII characters without whitespace"}},
		"a ruleId over 128":          {result(func(r obj) { r["ruleId"] = strings.Repeat("r", 129) }), Invalid{"result 1: no ruleId of 1 to 128 printable ASCII characters without whitespace"}},
		"level none":                 {result(func(r obj) { r["level"] = "none" }), Invalid{"result 1: level is none"}},
		"level none by rule":         {withRule(obj{"id": "r", "defaultConfiguration": obj{"level": "none"}}), Invalid{"result 0: level is none"}},
		"a level SARIF lacks":        {result(func(r obj) { r["level"] = "fatal" }), Invalid{"result 1: level is not one SARIF 2.1.0 names"}},
		"a severity not 0-10":        {withRule(obj{"id": "r", "properties": obj{"security-severity": "11"}}), Invalid{"result 0: the rule's security-severity is not a number from 0 to 10 or a severity word"}},
		"no message.text":            {result(func(r obj) { r["message"] = obj{"markdown": "m"} }), Invalid{"result 1: no message.text"}},
		"a message over 4,096":       {result(func(r obj) { r["message"] = obj{"text": strings.Repeat("é", 4097)} }), Invalid{"result 1: message.text is over 4,096 characters"}},
		"a fix over 4,096":           {result(func(r obj) { r["fixes"] = []any{obj{"description": obj{"text": strings.Repeat("é", 4097)}}} }), Invalid{"result 1: its fix is over 4,096 characters"}},
		"a help over 4,096":          {withRule(obj{"id": "r", "help": obj{"text": strings.Repeat("é", 4097)}}), Invalid{"result 0: its fix is over 4,096 characters"}},
		"no title":                   {result(func(r obj) { r["message"] = obj{"text": ". m"} }), Invalid{`result 1: message.text has nothing before its first ". "`}},
		"no location":                {result(func(r obj) { r["locations"] = []any{} }), Invalid{"result 1: no locations[0].physicalLocation with an artifactLocation.uri, or one whose region has no startLine"}},
		"a region by offsets":        {result(func(r obj) { delete(region(r), "startLine"); region(r)["charOffset"] = 6 }), Invalid{"result 1: no locations[0].physicalLocation with an artifactLocation.uri, or one whose region has no startLine"}},
		"a line of 0":                {result(func(r obj) { region(r)["startLine"] = 0 }), Invalid{"result 1: a region number is below 1 or above 1,000,000,000"}},
		"a column over 10^9":         {result(func(r obj) { region(r)["endColumn"] = 1_000_000_001 }), Invalid{"result 1: a region number is below 1 or above 1,000,000,000"}},
		"an end line before":         {result(func(r obj) { region(r)["startLine"] = 2 }), Invalid{"result 1: the region ends before it starts"}},
		"an end column before":       {result(func(r obj) { region(r)["startColumn"] = 14 }), Invalid{"result 1: the region ends before it starts"}},
		"a live key in a message":    {result(func(r obj) { r["message"] = obj{"text": liveKey("sk")} }), Invalid{"result 1: message.text carries a live key"}},
		"a live key in the help":     {withRule(obj{"id": "r", "help": obj{"text": liveKey("rk")}}), Invalid{"result 0: its fix carries a live key"}},
		"a live key in a ruleId":     {result(func(r obj) { r["ruleId"] = liveKey("sk") }), Invalid{"result 1: ruleId carries a live key"}},
		"a ruleId quoting 8 of its hidden region": {sarifOf(t, func(run obj) { // a credential's region is hidden whole, so an id may not name it
			run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": "someIdentifier", "properties": obj{"tags": []any{"secret"}}}}
		}, ok, func() obj { r := at("w.ts", 1, 1, 1, 13); r["ruleId"] = "someIdentifier"; return r }()), Invalid{"result 1: ruleId carries 8 or more characters of the code it flags"}},
		"a driver.version over 128":    {sarifOf(t, func(run obj) { run["tool"].(obj)["driver"].(obj)["version"] = strings.Repeat("v", 129) }, ok), Invalid{"driver.version is over 128 characters or carries a live key"}},
		"a live key in driver.version": {sarifOf(t, func(run obj) { run["tool"].(obj)["driver"].(obj)["version"] = liveKey("sk") }, ok), Invalid{"driver.version is over 128 characters or carries a live key"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize(c.raw, snapshot(t), files(t)); err != c.want {
				t.Fatalf("got %#v, want %#v", err, c.want)
			}
		})
	}
	nameless := snapshot(t)
	nameless.Rules[len(nameless.Rules)-1].Name = ""
	if _, err := Normalize(sarifOf(t, nil, ok), nameless, files(t)); err != (Invalid{"result 0: the rule has no namespace or no name"}) {
		t.Errorf("finding.New's refusal: got %#v", err)
	}
	if msg := (TooManyResults{"semgrep", 12345}).Error(); !strings.HasPrefix(msg, "semgrep reported 12,345 results;") {
		t.Errorf("thousands are not comma'd: %s", msg)
	}
}

// A result the input rules drop is counted, never refused, and the report's other results stand; the
// run's newlineSequences and columnKind are the ones its regions are read under.
func TestAResultIsDroppedAndCounted(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(r obj)
		run  func(run obj)
		want string
	}{
		"a kind other than fail": {func(r obj) { r["kind"] = "pass" }, nil, "kind is pass"},
		"a kind SARIF lacks":     {func(r obj) { r["kind"] = "bogus\n" }, nil, "kind is not one SARIF names"},
		"a suppression":          {func(r obj) { r["suppressions"] = []any{obj{"kind": "inSource"}} }, nil, "it is suppressed"},
		"no such file":           {func(r obj) { setURI(r, "b.ts") }, nil, "uri is not a file of the package"},
		"UTF-16 by default":      {func(r obj) { setURI(r, "u.ts"); region(r)["startColumn"] = 3 }, nil, "the region is past its file"},
		"the run's newlines": {func(r obj) { r["locations"] = at("cr.ts", 2, 5, 2, 9)["locations"] },
			func(run obj) { run["newlineSequences"] = crAsLine }, ""},
		"the run's columnKind": {func(r obj) { setURI(r, "u.ts"); region(r)["startColumn"] = 3 },
			func(run obj) { run["columnKind"] = "unicodeCodePoints" }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := at("a.ts", 1, 7, 1, 8)
			c.edit(r)
			rep, err := Normalize(sarifOf(t, c.run, at("a.ts", 1, 7, 1, 8), r), snapshot(t), files(t))
			if err != nil {
				t.Fatal(err)
			}
			if want := "result 1: " + c.want; c.want == "" && (len(rep.Results) != 2 || rep.Dropped != 0) ||
				c.want != "" && (len(rep.Results) != 1 || rep.Dropped != 1 || rep.FirstDropped != want) {
				t.Fatalf("kept %d, dropped %d, first %q; want %q", len(rep.Results), rep.Dropped, rep.FirstDropped, c.want)
			}
		})
	}
}

// A tool's region is shown as a Canyon finding's is, and hidden whole only where it must be: on a
// file-level result and where a line ends on a lone CR; past three lines it shows its first three
// (contracts/finding.md, In: external SARIF). A key in it is masked either way; secrets.* hidden whole is the mapped golden's.
func TestAToolsRegionIsShownUnlessItMustBeHidden(t *testing.T) {
	fileLevel := at("blank.ts", 1, 1, 1, 1) // no endColumn on an empty line 1: file-level by its offsets, not its numbers
	delete(fileLevel["locations"].([]any)[0].(obj)["physicalLocation"].(obj)["region"].(obj), "endColumn")
	for name, c := range map[string]struct {
		r    obj
		want string
	}{
		"a region on one line":     {at("a.ts", 1, 7, 1, 8), "const x = 1;"},
		"file-level by offsets":    {fileLevel, "••••"},
		"a file-level result":      {at("a.ts", 1, 1, 1, 1), "••••"},
		"past three lines":         {at("five.ts", 1, 1, 5, 5), "1\n2\n3"}, // its first three, never a mask over code (#1834)
		"a line that carries a CR": {at("cr.ts", 1, 1, 1, 4), "•••• = f(x);"},
	} {
		t.Run(name, func(t *testing.T) {
			rep, err := Normalize(sarifOf(t, nil, c.r), snapshot(t), files(t))
			if err != nil || len(rep.Results) != 1 {
				t.Fatalf("kept %d (%v), dropped %q", len(rep.Results), err, rep.FirstDropped)
			}
			if got := rep.Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text; got != c.want {
				t.Fatalf("snippet %q, want %q", got, c.want)
			}
		})
	}
}

// A region its tool's own tags call a credential is hidden whole, whatever policy it maps under: the
// tool said it is a secret, and gitleaks might not mask it.
func TestACredentialsRegionIsHiddenWhole(t *testing.T) {
	r := at("a.ts", 1, 7, 1, 8)
	tagged := func(run obj) {
		run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": r["ruleId"], "properties": obj{"tags": []any{"secret"}}}}
	}
	rep, err := Normalize(sarifOf(t, tagged, r), snapshot(t), files(t))
	if err != nil || len(rep.Results) != 1 || rep.Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text != "const •••• = 1;" {
		t.Fatalf("%v %+v", err, rep.Results)
	}
}

func setURI(r obj, uri string) {
	r["locations"].([]any)[0].(obj)["physicalLocation"].(obj)["artifactLocation"].(obj)["uri"] = uri
}

// Severity is the rule's security-severity by GitHub's bands, else the level; the title is the
// message's first sentence clipped to 120 code points, an ellipsis last; the fix the tool's, else its rule's help, else a
// pointer to the rule; results are sorted so the tool's order cannot move a fingerprint.
func TestAResultTakesTheProfilesFields(t *testing.T) {
	score := func(id, s string) obj { return obj{"id": id, "properties": obj{"security-severity": s}} }
	rules := []any{score("s9", "9.0"), obj{"id": "s7", "properties": obj{"security-severity": 7}}, score("s4", "4.0"), score("s3", "3.9"),
		obj{"id": "d", "defaultConfiguration": obj{"level": "error"}}, obj{"id": "h", "help": obj{"text": "Read the rule's help."}},
		obj{"id": "f", "help": obj{"text": "Read the rule's help."}}, obj{"id": "h", "help": obj{"text": "A second rule h."}}}
	var results []obj
	for i, id := range []string{"s9", "s7", "s4", "s3", "d", "h", "e", "w", "n", "f"} {
		r := at("a.ts", 2-i%2, 1+i, 2-i%2, 2+i)
		r["ruleId"] = id
		results = append(results, r)
	}
	results[7]["level"], results[6]["level"], results[8]["level"] = "warning", "error", "note"
	results[6]["message"] = obj{"text": "Hi. There."}
	results[9]["fixes"] = []any{obj{"description": obj{"text": "Validate it."}}}
	results[9]["message"] = obj{"text": strings.Repeat("é", 121) + ". More."}
	results[9]["locations"] = results[7]["locations"] // one place, two rules: the rule id decides
	rep, err := Normalize(sarifOf(t, func(run obj) { run["tool"].(obj)["driver"].(obj)["rules"] = rules }, results...), snapshot(t), files(t))
	if err != nil {
		t.Fatal(err)
	}
	got, order := map[string]string{}, []string{}
	for _, r := range rep.Results {
		id, c := strings.TrimPrefix(r.RuleID, "sarif.semgrep."), r.Properties.Canyon
		got[id] = string(c.Severity) + " · " + c.Title + " · " + c.Fix
		order = append(order, fmt.Sprintf("%s@%d", id, r.Locations[0].PhysicalLocation.Region.StartLine))
	}
	unfixed := func(id string) string { // a rule with no fix of semgrep's: the profile's (contracts/finding.md)
		return "Change this code so semgrep no longer reports " + id + " here; the line above says what it found."
	}
	for id, want := range map[string]string{
		"s9": "critical · m · " + unfixed("s9"), "s7": "high · m · " + unfixed("s7"), "s4": "medium · m · " + unfixed("s4"),
		"s3": "low · m · " + unfixed("s3"), "d": "high · m · " + unfixed("d"), "h": "medium · m · Read the rule's help.",
		"e": "high · Hi · " + unfixed("e"), "w": "medium · m · " + unfixed("w"), "n": "low · m · " + unfixed("n"),
		"f": "medium · " + strings.Repeat("é", 119) + "… · Validate it.",
	} {
		if got[id] != want {
			t.Errorf("%s: got %q, want %q", id, got[id], want)
		}
	}
	if want := []string{"s7@1", "s3@1", "h@1", "f@1", "w@1", "s9@2", "s4@2", "d@2", "e@2", "n@2"}; !reflect.DeepEqual(order, want) {
		t.Errorf("sorted %v, want %v", order, want)
	}
}

// CodeQL defines its rules in the query pack, tool.extensions[], not the driver, and writes most security
// results at warning. A result's rule is the one its rule.toolComponent names, by index or by name, so its
// security-severity rates it and its help is its fix, and a high one blocks under a blocking rule; a
// result naming an extension the report lacks has no rule, and its level rates it.
func TestACodeQLResultTakesItsExtensionsRule(t *testing.T) {
	pack := obj{"name": "codeql/javascript-queries", "version": "1.1.0", "rules": []any{
		obj{"id": "js/sql-injection", "help": obj{"text": "Use a parameterised query."}, "properties": obj{"security-severity": "8.8"}},
		obj{"id": "js/code-injection", "properties": obj{"security-severity": "9.8"}},
		obj{"id": "js/path-injection", "properties": obj{"security-severity": 7.5}}}}
	var results []obj
	for i, ref := range []struct {
		id        string
		component obj
	}{{"js/sql-injection", obj{"index": 0}}, {"js/code-injection", obj{"index": 0}},
		{"js/path-injection", obj{"name": "codeql/javascript-queries"}}, {"js/sql-injection", obj{"index": 1}}} {
		r := at("a.ts", 1, 1+i, 1, 2+i)
		r["ruleId"], r["ruleIndex"], r["level"] = ref.id, 0, "warning"
		r["rule"] = obj{"id": ref.id, "index": 0, "toolComponent": ref.component}
		results = append(results, r)
	}
	snap := snapshot(t)
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.codeql", Name: "Findings from CodeQL", State: policy.Blocking, Producer: policy.Sarif})
	tree := files(t)
	rep, err := Normalize(sarifOf(t, func(run obj) {
		run["tool"] = obj{"driver": obj{"name": "CodeQL", "version": "2.19.0", "rules": []any{}}, "extensions": []any{pack}}
	}, results...), snap, tree)
	if err != nil {
		t.Fatal(err)
	}
	driver := finding.Driver{Name: "sarif/codeql", Version: rep.Version}
	for _, id := range []string{"js/code-injection", "js/path-injection", "js/sql-injection"} {
		driver.Rules = append(driver.Rules, finding.Rule{ID: "sarif.codeql." + id, Name: "Findings from CodeQL"})
	}
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: driver}, Results: rep.Results}}}
	if err := cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)), finding.Check(&log, *snap)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range log.Runs[0].Results {
		c := r.Properties.Canyon
		got = append(got, fmt.Sprintf("%s %s blocks=%t · %s", r.RuleID, c.Severity, *c.Blocks, c.Fix))
	}
	want := []string{
		"sarif.codeql.js/sql-injection high blocks=true · Use a parameterised query.",
		"sarif.codeql.js/code-injection critical blocks=true · Change this code so codeql no longer reports js/code-injection here; the line above says what it found.",
		"sarif.codeql.js/path-injection high blocks=true · Change this code so codeql no longer reports js/path-injection here; the line above says what it found.",
		"sarif.codeql.js/sql-injection medium blocks=false · Change this code so codeql no longer reports js/sql-injection here; the line above says what it found.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A toolComponent index below 0 or past the extensions names no component, never the driver and never a
// panic, so the level rates the result; of two extensions with one name, the name is the first's.
func TestAToolComponentNamesOneExtensionOrNone(t *testing.T) {
	pack := func(severity string) obj {
		return obj{"name": "pack", "rules": []any{obj{"id": "r", "properties": obj{"security-severity": severity}}}}
	}
	for name, c := range map[string]struct {
		component obj
		want      finding.Severity
	}{
		"no toolComponent: the driver's":    {nil, finding.High},
		"index 1: the second pack":          {obj{"index": 1}, finding.Low},
		"a repeated name: the first pack":   {obj{"name": "pack"}, finding.Critical},
		"index -1: none":                    {obj{"index": -1}, finding.Medium},
		"index -2: none":                    {obj{"index": -2}, finding.Medium},
		"index 2, past the packs: none":     {obj{"index": 2}, finding.Medium},
		"index math.MaxInt64: none":         {obj{"index": math.MaxInt64}, finding.Medium},
		"a name no extension carries: none": {obj{"name": "codeql/javascript-queries"}, finding.Medium},
	} {
		t.Run(name, func(t *testing.T) {
			r := at("a.ts", 1, 7, 1, 8)
			r["level"], r["rule"] = "warning", obj{"id": "r", "toolComponent": c.component}
			rep, err := Normalize(sarifOf(t, func(run obj) {
				run["tool"] = obj{"driver": obj{"name": "semgrep", "rules": []any{obj{"id": "r", "properties": obj{"security-severity": "7.0"}}}},
					"extensions": []any{pack("9.0"), pack("2.0")}}
			}, r), snapshot(t), files(t))
			if err != nil {
				t.Fatal(err)
			}
			if got := rep.Results[0].Properties.Canyon.Severity; got != c.want {
				t.Errorf("severity %s, want %s", got, c.want)
			}
		})
	}
}

// Trivy places each CVE at its lockfile, file-level: uriBaseId ROOTPATH and an empty region at 1:1, or
// no region at all. Each is kept at the lockfile's first non-empty line, hidden whole, its severity the
// rule's; a lockfile with a blank first line keeps its findings too.
func TestATrivyCVEIsKeptAtItsLockfile(t *testing.T) {
	dir := t.TempDir()
	lock := "{\n  \"name\": \"expenses\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\n    \"node_modules/lodash\": {\n      \"version\": \"4.17.20\"\n    }\n  }\n}\n"
	for name, body := range map[string]string{"package-lock.json": lock, "blank/package-lock.json": "\n" + lock} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cve := func(id, uri string) obj {
		return obj{"ruleId": id, "ruleIndex": 0, "level": "error",
			"message": obj{"text": "Package: lodash\nInstalled Version: 4.17.20\nVulnerability " + id + "\nSeverity: HIGH\nFixed Version: 4.17.21"},
			"locations": []any{obj{"physicalLocation": obj{
				"artifactLocation": obj{"uri": uri, "uriBaseId": "ROOTPATH"},
				"region":           obj{"startLine": 1, "startColumn": 1, "endLine": 1, "endColumn": 1}},
				"message": obj{"text": uri + ": lodash@4.17.20"}}}}
	}
	regionless := cve("CVE-2020-28500", "package-lock.json")
	delete(regionless["locations"].([]any)[0].(obj)["physicalLocation"].(obj), "region")
	rules := []any{obj{"id": "CVE-2021-23337", "properties": obj{"security-severity": "7.2"}}, obj{"id": "CVE-2020-28500", "properties": obj{"security-severity": "5.3"}}}
	snap := snapshot(t)
	snap.Rules[slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif.trivy" })].State = policy.Blocking
	raw := sarifOf(t, func(run obj) { run["tool"] = obj{"driver": obj{"name": "Trivy", "version": "0.56.2", "rules": rules}} },
		cve("CVE-2021-23337", "package-lock.json"), regionless, cve("CVE-2021-23337", "blank/package-lock.json"))
	tree := treeAt(t, dir)
	rep, err := Normalize(raw, snap, tree)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rep.Results {
		g := r.Locations[0].PhysicalLocation
		got = append(got, fmt.Sprintf("%s %s at %s %d:%d-%d:%d %s", r.RuleID, r.Properties.Canyon.Severity, g.ArtifactLocation.URI,
			g.Region.StartLine, g.Region.StartColumn, g.Region.EndLine, g.Region.EndColumn, g.Region.Snippet.Text))
	}
	want := []string{
		"sarif.trivy.CVE-2021-23337 high at blank/package-lock.json 2:1-2:2 ••••",
		"sarif.trivy.CVE-2020-28500 medium at package-lock.json 1:1-1:2 ••••",
		"sarif.trivy.CVE-2021-23337 high at package-lock.json 1:1-1:2 ••••",
	}
	if rep.Dropped != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped %d (first %q), kept:\n%s\nwant:\n%s", rep.Dropped, rep.FirstDropped, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Under a blocking rule the two high CVEs block, and the run's log passes the profile: each carries
	// the governance prose a finding that blocks requires, never a run that fails over its absence.
	driver := finding.Driver{Name: "sarif/trivy", Version: rep.Version}
	for _, id := range []string{"sarif.trivy.CVE-2020-28500", "sarif.trivy.CVE-2021-23337"} {
		driver.Rules = append(driver.Rules, finding.Rule{ID: id, Name: "Findings from trivy"})
	}
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: driver}, Results: rep.Results}}}
	if err := cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)), finding.Check(&log, *snap)); err != nil {
		t.Fatalf("a blocking imported result: %v", err)
	}
	if blocking := slices.IndexFunc(log.Runs[0].Results, func(r finding.Result) bool { return !*r.Properties.Canyon.Blocks }); blocking != 1 {
		t.Errorf("the result that does not block is %d, want 1: the medium CVE", blocking)
	}
}

// Grype places a dependency it finds inside a jar on the jar, file-level. A jar is not valid UTF-8, so
// the result names the file only: kept at the placeholder, 1:1-1:2, hidden whole, nothing of the jar
// read, and its fingerprint frames an empty region, so an edit to the jar does not move it. A region
// result on the jar is still dropped.
func TestAGrypeCVEIsKeptAtItsJar(t *testing.T) {
	jar := func(version string) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for _, e := range [][2]string{{"META-INF/MANIFEST.MF", "Manifest-Version: 1.0\n"},
			{"META-INF/maven/org.apache.logging.log4j/log4j-core/pom.properties", "groupId=org.apache.logging.log4j\nartifactId=log4j-core\nversion=" + version + "\n"},
			{"org/apache/logging/log4j/core/lookup/JndiLookup.class", "\xca\xfe\xba\xbe\x00\x00\x004"}} {
			f, err := zw.Create(e[0])
			if err == nil {
				_, err = f.Write([]byte(e[1]))
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if utf8.Valid(b.Bytes()) {
			t.Fatal("the jar is valid UTF-8; the test needs one that is not")
		}
		return b.Bytes()
	}
	tree := func(version string) gate.Tree {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lib", "log4j-core-2.14.1.jar"), jar(version), 0o600); err != nil {
			t.Fatal(err)
		}
		return treeAt(t, dir)
	}
	const id, uri = "GHSA-jfh8-c2jp-5v3q-log4j-core", "lib/log4j-core-2.14.1.jar"
	cve := func(g ...int) obj {
		r := obj{"ruleId": id, "level": "error",
			"message":   obj{"text": "A critical vulnerability in java-archive package: log4j-core, version 2.14.1 was found at: /" + uri},
			"locations": []any{obj{"physicalLocation": obj{"artifactLocation": obj{"uri": uri}}}}}
		if g != nil {
			r["locations"].([]any)[0].(obj)["physicalLocation"].(obj)["region"] = obj{"startLine": g[0], "startColumn": g[1], "endLine": g[2], "endColumn": g[3]}
		}
		return r
	}
	snap := snapshot(t)
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.grype", Name: "Findings from grype", State: policy.Blocking, Producer: policy.Sarif})
	raw := sarifOf(t, func(run obj) {
		run["tool"] = obj{"driver": obj{"name": "grype", "version": "0.80.0", "rules": []any{obj{"id": id, "properties": obj{"security-severity": "10.0"}}}}}
	}, cve(1, 1, 1, 1), cve(), cve(1, 3, 1, 5))
	fingerprints := map[string][]string{}
	for _, version := range []string{"2.14.1", "2.14.0"} {
		files := tree(version)
		rep, err := Normalize(raw, snap, files)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Dropped != 1 || rep.FirstDropped != "result 2: the file is not valid UTF-8" || len(rep.Results) != 2 {
			t.Fatalf("kept %d, dropped %d, first %q; want two kept and the region result dropped", len(rep.Results), rep.Dropped, rep.FirstDropped)
		}
		for _, r := range rep.Results {
			g := r.Locations[0].PhysicalLocation
			if r.RuleID != "sarif.grype."+id || r.Properties.Canyon.Severity != finding.Critical || g.ArtifactLocation.URI != uri ||
				[4]int{g.Region.StartLine, g.Region.StartColumn, g.Region.EndLine, g.Region.EndColumn} != [4]int{1, 1, 1, 2} || g.Region.Snippet.Text != "••••" {
				t.Fatalf("got %s %s at %s %+v; want sarif.grype.%s critical at %s 1:1-1:2 hidden whole", r.RuleID, r.Properties.Canyon.Severity, g.ArtifactLocation.URI, g.Region, id, uri)
			}
		}
		log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints,
			Tool: finding.Tool{Driver: finding.Driver{Name: "sarif/grype", Version: rep.Version, Rules: []finding.Rule{{ID: "sarif.grype." + id, Name: "Findings from grype"}}}}, Results: rep.Results}}}
		if err := cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, files, make([]byte, 32)), finding.Check(&log, *snap)); err != nil {
			t.Fatalf("Log4Shell under a blocking rule: %v", err)
		}
		for _, r := range log.Runs[0].Results {
			if !*r.Properties.Canyon.Blocks {
				t.Error("Log4Shell does not block under a blocking rule")
			}
			fingerprints[version] = append(fingerprints[version], r.PartialFingerprints.CanyonV1)
		}
	}
	// canyon/v1 by finding.md's recipe, the region's digest over no bytes: the same for either jar.
	frame := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	empty := hex.EncodeToString(hmac.New(sha256.New, make([]byte, 32)).Sum(nil))
	var want []string
	for n := range 2 {
		want = append(want, fmt.Sprintf("f_%x", sha256.Sum256([]byte(frame("sarif.grype."+id)+frame(uri)+frame("••••")+frame("1")+frame(strconv.Itoa(n))+frame(empty)))))
	}
	for version, got := range fingerprints {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("jar %s: fingerprints %v, want %v, the region's digest over no bytes", version, got, want)
		}
	}
}

// Text a report gives that shares 8 or more code points with a region hidden whole — here a credential's,
// by its rule's tag — is masked, each stretch one mask, so the hidden region never reaches stored text
// and the report is not refused for it; a run of 7 is kept, and an id is refused instead
// (TestAReportIsRefusedWhole), as one is never rewritten.
func TestTextQuotingItsRegionIsMasked(t *testing.T) {
	r := at("a.ts", 1, 1, 1, 13) // the region is a.ts's first line, "const x = 1;"
	r["message"] = obj{"text": "const x is flagged; const x = 1; and const x."}
	r["fixes"] = []any{obj{"description": obj{"text": "drop const x = and keep const"}}}
	tagged := func(run obj) {
		run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": r["ruleId"], "properties": obj{"tags": []any{"CWE-798: Use of Hard-coded Credentials"}}}}
	}
	rep, err := Normalize(sarifOf(t, tagged, r), snapshot(t), files(t))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Results[0].Properties.Canyon
	if c.Problem != "••••is flagged; •••• and const x." || c.Fix != "drop ••••and keep const" || c.Title != c.Problem {
		t.Fatalf("problem %q, fix %q, title %q", c.Problem, c.Fix, c.Title)
	}
}

// maskQuoted's edges: a stretch at the end, multi-byte runes, text masked whole, and text shorter
// than a run, which no region can share 8 code points with.
func TestMaskQuotedEdges(t *testing.T) {
	for _, c := range []struct{ s, hidden, want string }{
		{"see héllo wörld", "x = héllo wörld;", "see••••"},
		{"héllo wörld", "héllo wörld", "••••"},
		{"short", "short", "short"},
		{"abcdefg h", "abcdefg", "abcdefg h"},
		{"€€€€€€€€ and 😀😀😀😀😀😀😀😀", "😀😀😀😀😀😀😀😀€€€€€€€€", "•••• and ••••"},
	} {
		if got := maskQuoted(c.s, c.hidden); got != c.want {
			t.Errorf("maskQuoted(%q, %q) = %q, want %q", c.s, c.hidden, got, c.want)
		}
	}
	if got := clip(strings.Repeat("a", 78)+" bb", 80); got != strings.Repeat("a", 78)+"…" {
		t.Errorf("clip drops the space before its ellipsis: %q", got)
	}
}
