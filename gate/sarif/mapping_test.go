package sarif

import (
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// mapped is the snapshot, which carries every tool the policy table names, with rule's state set.
func mapped(t *testing.T, rule string, state policy.State) *policy.Snapshot {
	snap := snapshot(t)
	for i := range snap.Rules {
		if snap.Rules[i].ID == rule {
			snap.Rules[i].State = state
		}
	}
	return snap
}

// derived is a report's results as a run of tool carries them: derived, fingerprinted under the
// test-only key, and checked against snap, as the gate does.
func derived(t *testing.T, tool string, rep Report, snap *policy.Snapshot, tree gate.Tree) finding.Log {
	t.Helper()
	driver := finding.Driver{Name: "sarif/" + tool, Version: rep.Version}
	for _, r := range rep.Results {
		driver.Rules = append(driver.Rules, finding.Rule{ID: r.RuleID, Name: r.Properties.Canyon.PolicyName})
	}
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: driver}, Results: rep.Results}}}
	if err := cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)), finding.Check(&log, *snap)); err != nil {
		t.Fatal(err)
	}
	return log
}

// The contract's mapped golden is gitleaks 8.30.1's real stripe-access-token result on the claims
// portal, its region the whole line as Canyon's gitleaks run writes it, normalised under the policy
// table: secrets.client-bundle, critical, the table's fix and secret_name, the description first.
func TestTheMappedGoldenIsGitleaksReportNormalised(t *testing.T) {
	var golden finding.Log
	want, err := os.ReadFile("../../contracts/testdata/finding/mapped-gitleaks.sarif.json")
	if err := cmp.Or(err, json.Unmarshal(want, &golden)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/gitleaks-8.30.1-claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "../testdata/apps/claims-portal"), mapped(t, "", "")
	rep, err := NormalizeAs(raw, snap, tree, "gitleaks")
	if err != nil {
		t.Fatal(err)
	}
	log := derived(t, "gitleaks", rep, snap, tree)
	if !reflect.DeepEqual(log.Runs[0].Results, golden.Runs[0].Results) {
		got, _ := json.Marshal(log.Runs[0].Results)
		t.Fatalf("normalised, derived and fingerprinted, the result is not the golden's:\n got: %s\nwant: %s", got, want)
	}
}

// GitLab's sast-rules rate in words, which opengrep copies into security-severity: each is its band,
// in any ASCII case, info is low, and any other word refuses the report as a bad number does.
func TestASeverityWordIsItsBand(t *testing.T) {
	for word, want := range map[string]finding.Severity{"CRITICAL": finding.Critical, "High": finding.High, "medium": finding.Medium,
		"Low": finding.Low, "Info": finding.Low} {
		raw := sarifOf(t, func(run obj) {
			run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": "r", "properties": obj{"security-severity": word}}}
		}, at("a.ts", 1, 7, 1, 8))
		rep, err := Normalize(raw, snapshot(t), files(t))
		if err != nil || rep.Results[0].Properties.Canyon.Severity != want {
			t.Errorf("%s: %+v %v, want %s", word, rep.Results, err, want)
		}
	}
	for _, word := range []string{"severe", "HIGH ", "ＨＩＧＨ"} {
		raw := sarifOf(t, func(run obj) {
			run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": "r", "properties": obj{"security-severity": word}}}
		}, at("a.ts", 1, 7, 1, 8))
		if _, err := Normalize(raw, snapshot(t), files(t)); err != (Invalid{"result 0: the rule's security-severity is not a number from 0 to 10 or a severity word"}) {
			t.Errorf("%q: %v", word, err)
		}
	}
}

// opengrep 1.30.0's real report on a Flask probe with the GitLab sast-rules v2.10.1 Python rules, run
// with the executor's --no-rewrite-rule-ids, its driver's rules cut to the one reported: its
// SQL injection rule is tagged CWE-89 and rated High in words at level warning, so the table maps it
// under sql.string-concat at high, and under that blocking policy it blocks — the reason M1 exists.
func TestOpengrepsSQLInjectionIsSQLStringConcatAndBlocks(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-flask-sqli.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "testdata/opengrep-flask"), mapped(t, "sql.string-concat", policy.Blocking)
	rep, err := NormalizeAs(raw, snap, tree, "opengrep")
	if err != nil {
		t.Fatal(err)
	}
	log := derived(t, "opengrep", rep, snap, tree)
	r := log.Runs[0].Results[0]
	c := r.Properties.Canyon
	if r.RuleID != "sql.string-concat.opengrep.python_sql_rule-hardcoded-sql-expression" || c.Severity != finding.High || !*c.Blocks ||
		c.PolicyName != "No hand-built SQL from user input" || c.Fix != policy.Table[1].Fix || c.SecretName != nil ||
		c.Title != "Improper neutralization of special elements used in an SQL Command ('SQL Injection')" ||
		!strings.HasPrefix(c.Problem, "Improper neutralization of special elements used in an SQL Command ('SQL Injection'). SQL Injection is") {
		t.Fatalf("%s: %+v", r.RuleID, c)
	}
	// The message is the rule's whole document (its fullDescription): the problem is its first paragraph,
	// the rule's YAML wrapping undone, never 36 lines broken mid-sentence (the QA batch).
	if want := "Improper neutralization of special elements used in an SQL Command ('SQL Injection'). SQL Injection is a critical vulnerability that can lead to data or system compromise. By dynamically generating SQL query strings, user input may be able to influence the logic of the SQL statement. This could lead to an adversary accessing information they should not have access to, or in some circumstances, being able to execute OS functionality or code."; c.Problem != want {
		t.Fatalf("problem %q\nwant    %q", c.Problem, want)
	}
}

// opengrep 1.30.0's real report on a Go server calling http.ListenAndServe, with the GitLab sast-rules
// v2.10.1 Go rules and the executor's flags, its driver's rules cut to the one reported (QA round 5,
// go-build-fails). CWE-770 is no row's, so the result keeps sarif.opengrep's id. Its message and its
// help are both the rule's whole document: the problem is the document's first paragraph, and the fix,
// the document again and no fix of the tool's own, is the contract's reference to the rule, never the
// problem once more wrapped where the rule's YAML wrapped.
func TestAnOpengrepRuleWhoseHelpIsItsDocumentOffersNoFix(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-go-http-serve.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "testdata/opengrep-go-http"), snapshot(t)
	rep, err := NormalizeAs(raw, snap, tree, "opengrep")
	if err != nil {
		t.Fatal(err)
	}
	r := derived(t, "opengrep", rep, snap, tree).Runs[0].Results[0]
	c := r.Properties.Canyon
	problem := "Allocation of resources without limits or throttling. Go's `net/http` serve functions may be vulnerable to resource consumption attacks if timeouts are not properly configured prior to starting the HTTP server. An adversary may open up thousands of connections but never complete sending all data, or never terminate the connections. This may lead to the server no longer accepting new connections."
	if r.RuleID != "sarif.opengrep.go_http_rule-http-serve" || c.Severity != finding.Low || c.Problem != problem || c.Fix != "Change this code so opengrep no longer reports go_http_rule-http-serve here; the line above says what it found." {
		t.Fatalf("%s, %s:\nproblem %q\nfix %q", r.RuleID, c.Severity, c.Problem, c.Fix)
	}
}

// The vendored GitLab rule generic-path-traversal is tagged CWE-23, relative path traversal, where its
// siblings on the same line carry CWE-22: it is code.injection's as they are, never a raw sarif.opengrep
// id (the QA batch's node-secret-client, server.js:36).
func TestOpengrepsRelativePathTraversalIsCodeInjection(t *testing.T) {
	r := at("a.ts", 1, 1, 1, 13)
	r["ruleId"] = "rules_lgpl_javascript_traversal_rule-generic-path-traversal"
	r["message"] = obj{"text": "Untrusted user input in readFile()/readFileSync() can endup in Directory Traversal Attacks."}
	raw := sarifOf(t, func(run obj) {
		run["tool"].(obj)["driver"] = obj{"name": "Opengrep OSS", "version": "1.30.0", "rules": []any{obj{
			"id": "rules_lgpl_javascript_traversal_rule-generic-path-traversal", "shortDescription": obj{"text": "Relative path traversal"},
			"properties": obj{"security-severity": "MEDIUM", "tags": []any{"CWE-23", "OWASP-A01:2021-Broken Access Control",
				"OWASP-A01:2025-Broken Access Control", "OWASP-A5:2017-Broken Access Control", "security"}}}}}
	}, r)
	rep, err := NormalizeAs(raw, snapshot(t), files(t), "opengrep")
	if err != nil {
		t.Fatal(err)
	}
	derived(t, "opengrep", rep, snapshot(t), files(t))
	if got := rep.Results[0]; got.RuleID != "code.injection.opengrep.rules_lgpl_javascript_traversal_rule-generic-path-traversal" ||
		got.Properties.Canyon.Fix != policy.Table[3].Fix {
		t.Fatalf("%s, fix %q", got.RuleID, got.Properties.Canyon.Fix)
	}
}

// opengrep 1.30.0's real report, pretty-printed, of Canyon's own rule (gate/opengrep/canyon.yaml) on F-sql,
// run with the executor's arguments: one result for its one splice, tagged CWE-89 and rated Critical,
// so the table maps it under sql.string-concat at critical, and under that blocking policy it blocks.
func TestCanyonsSQLRuleIsSQLStringConcatAndBlocks(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-canyon-sql.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "../opengrep/testdata/canyon/flagged/next-app"), mapped(t, "sql.string-concat", policy.Blocking)
	rep, err := NormalizeAs(raw, snap, tree, "opengrep")
	if err != nil {
		t.Fatal(err)
	}
	log := derived(t, "opengrep", rep, snap, tree)
	if n := len(log.Runs[0].Results); n != 1 {
		t.Fatalf("%d results, want 1", n)
	}
	r := log.Runs[0].Results[0]
	if c := r.Properties.Canyon; r.RuleID != "sql.string-concat.opengrep.canyon_sql_rule-request-in-sql-text" || c.Severity != finding.Critical || !*c.Blocks ||
		r.Locations[0].PhysicalLocation.ArtifactLocation.URI != "src/app/api/claims/route.ts" || r.Locations[0].PhysicalLocation.Region.StartLine != 6 {
		t.Fatalf("%s at %+v: %+v", r.RuleID, r.Locations[0].PhysicalLocation, c)
	}
}

// A row selects by tags alone, so a result under a policy that is Off is still that policy's, with
// the id an approval follows: in the run it blocks nothing, as no finding of an Off rule does.
func TestAResultUnderAnOffPolicyKeepsItsIdAndBlocksNothing(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-flask-sqli.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "testdata/opengrep-flask"), mapped(t, "sql.string-concat", policy.Off)
	rep, err := NormalizeAs(raw, snap, tree, "opengrep")
	if err != nil {
		t.Fatal(err)
	}
	r := derived(t, "opengrep", rep, snap, tree).Runs[0].Results[0]
	if r.RuleID != "sql.string-concat.opengrep.python_sql_rule-hardcoded-sql-expression" || *r.Properties.Canyon.Blocks {
		t.Fatalf("%s blocks=%v", r.RuleID, *r.Properties.Canyon.Blocks)
	}
}

// Only the rows' tags select: trivy's misconfiguration is no dependency, but iac.misconfig's.
func TestATrivyMisconfigurationIsIaCMisconfig(t *testing.T) {
	r := at("a.ts", 1, 7, 1, 8)
	r["ruleId"] = "AVD-KSV-0001"
	raw := sarifOf(t, func(run obj) {
		run["tool"].(obj)["driver"] = obj{"name": "Trivy", "rules": []any{obj{"id": "AVD-KSV-0001", "properties": obj{"tags": []any{"misconfiguration", "vulnerability:no"}}}}}
	}, r)
	rep, err := NormalizeAs(raw, snapshot(t), files(t), "trivy")
	if err != nil || rep.Results[0].RuleID != "iac.misconfig.trivy.AVD-KSV-0001" {
		t.Fatalf("%+v %v", rep.Results, err)
	}
}

// checkov's result reads once and fixes as checkov says: its message is its check's description, so the
// problem is that description alone, and its rule's help — the check and the resource it names — is
// its fix, not the table's (the QA batch's "Ensure … images. Ensure … images"). Its shape is checkov
// 3.3.19's own on a Dockerfile exposing port 22.
func TestACheckovResultReadsOnceAndFixesAsCheckovSays(t *testing.T) {
	r := at("Dockerfile", 1, 1, 1, 10)
	r["ruleId"] = "CKV_DOCKER_1"
	r["message"] = obj{"text": "Ensure port 22 is not exposed"}
	raw := sarifOf(t, func(run obj) {
		run["tool"].(obj)["driver"] = obj{"name": "Checkov", "version": "3.3.19", "rules": []any{obj{"id": "CKV_DOCKER_1",
			"name": "Ensure port 22 is not exposed", "shortDescription": obj{"text": "Ensure port 22 is not exposed"},
			"fullDescription": obj{"text": "Ensure port 22 is not exposed"}, "help": obj{"text": "Ensure port 22 is not exposed\nResource: /Dockerfile.EXPOSE"},
			"defaultConfiguration": obj{"level": "error"}}}}
	}, r)
	rep, err := NormalizeAs(raw, snapshot(t), files(t), "checkov")
	if err != nil {
		t.Fatal(err)
	}
	derived(t, "checkov", rep, snapshot(t), files(t)) // and the run's check holds it to its row, a fix that is the tool's
	c := rep.Results[0].Properties.Canyon
	if rep.Results[0].RuleID != "iac.misconfig.checkov.CKV_DOCKER_1" || c.Title != "Ensure port 22 is not exposed" || c.Problem != "Ensure port 22 is not exposed"+notReadDockerfile ||
		c.Fix != "Ensure port 22 is not exposed\nResource: /Dockerfile.EXPOSE" {
		t.Fatalf("%s: title %q, problem %q, fix %q", rep.Results[0].RuleID, c.Title, c.Problem, c.Fix)
	}
}

// A title over 120 code points is cut at the last space in its first 119, never mid-word (the QA batch).
func TestAnImportedTitleIsCutAtASpace(t *testing.T) {
	r := at("a.ts", 1, 1, 1, 13)
	r["message"] = obj{"text": "Relative path traversal: a long description of what the rule found in the request handler that reads a file named by the user here"}
	rep, err := Normalize(sarifOf(t, nil, r), snapshot(t), files(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Results[0].Properties.Canyon.Title; got != "Relative path traversal: a long description of what the rule found in the request handler that reads a file named by…" {
		t.Fatalf("title %q", got)
	}
}

// A rule's shortDescription is text the report gives and the problem carries: it is checked like
// every other — refused carrying a live key or over 1,024 characters, masked where it shares 8 code
// points with its region.
func TestAShortDescriptionIsCheckedLikeAnyReportText(t *testing.T) {
	described := func(text string) []byte {
		return sarifOf(t, func(run obj) {
			run["tool"].(obj)["driver"].(obj)["rules"] = []any{obj{"id": "r", "shortDescription": obj{"text": text}}}
		}, at("a.ts", 1, 1, 1, 13))
	}
	for name, c := range map[string]struct {
		text, title string
		want        error
	}{
		"a live key":        {"Found " + liveKey("sk"), "", Invalid{"result 0: its rule's shortDescription carries a live key"}},
		"over 1,024":        {strings.Repeat("d", 1025), "", Invalid{"result 0: its rule's shortDescription is over 1,024 characters"}},
		"its region's text": {"Flags const x = 1", "Flags const x = 1", nil}, // the region is shown, so the text quoting it is too
		"a plain one":       {"Hard-coded value. Read on", "Hard-coded value", nil},
	} {
		rep, err := Normalize(described(c.text), snapshot(t), files(t))
		if err != c.want {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if err == nil && (rep.Results[0].Properties.Canyon.Title != c.title || !strings.HasPrefix(rep.Results[0].Properties.Canyon.Problem, c.title+". ")) {
			t.Errorf("%s: %+v", name, rep.Results[0].Properties.Canyon)
		}
	}
}

// A run is confined to its own tool's table rows: a semgrep run carrying a gitleaks-shaped secrets id,
// or a gitleaks run carrying a policy the table does not map it to, fails the gate's check closed.
func TestAMappedIdOutsideItsToolsRowsFailsTheCheck(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitleaks-8.30.1-claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "../testdata/apps/claims-portal"), mapped(t, "", "")
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.semgrep-x", Name: "Findings from semgrep-x", State: policy.Advisory, Producer: policy.Sarif})
	for name, c := range map[string]struct{ tool, id string }{
		"another tool's run":       {"semgrep-x", "secrets.client-bundle.gitleaks.stripe-access-token"},
		"a policy gitleaks is not": {"gitleaks", "sql.string-concat.gitleaks.stripe-access-token"},
	} {
		rep, err := NormalizeAs(raw, snap, tree, "gitleaks")
		if err != nil {
			t.Fatal(err)
		}
		rep.Results[0].RuleID, rep.Results[0].Properties.Canyon.SecretName = c.id, nil
		rep.Results[0].Properties.Canyon.PolicyName = "Findings from " + c.tool // the name its tool's rule gives: confinement alone refuses it
		driver := finding.Driver{Name: "sarif/" + c.tool, Version: "1", Rules: []finding.Rule{{ID: c.id, Name: rep.Results[0].Properties.Canyon.PolicyName}}}
		log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: driver}, Results: rep.Results}}}
		_ = cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)))
		if err := finding.Check(&log, *snap); err == nil || !strings.Contains(err.Error(), "outside the namespace") {
			t.Errorf("%s: %s: %v", name, c.id, err)
		}
	}
}

// The gate's check holds a mapped result to its row as the contract's validator does: the row's
// severity where it names one, its fix, and the secret_name its rule id gives.
func TestTheCheckHoldsAMappedResultToItsRow(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitleaks-8.30.1-claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "../testdata/apps/claims-portal"), mapped(t, "", "")
	other := "OTHER_NAME"
	for name, edit := range map[string]func(c *finding.Canyon){
		"another severity":    func(c *finding.Canyon) { c.Severity = finding.High },
		"its own fix":         func(c *finding.Canyon) { c.Fix = "Delete the key." },
		"another secret_name": func(c *finding.Canyon) { c.SecretName = &other },
	} {
		rep, err := NormalizeAs(raw, snap, tree, "gitleaks")
		if err != nil {
			t.Fatal(err)
		}
		edit(&rep.Results[0].Properties.Canyon)
		rep.Results[0].Level, _ = rep.Results[0].Properties.Canyon.Severity.Level()
		driver := finding.Driver{Name: "sarif/gitleaks", Version: "1", Rules: []finding.Rule{{ID: rep.Results[0].RuleID, Name: rep.Results[0].Properties.Canyon.PolicyName}}}
		log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: driver}, Results: rep.Results}}}
		_ = cmp.Or(finding.Derive(&log, *snap), gate.FingerprintImported(&log, tree, make([]byte, 32)))
		if err := finding.Check(&log, *snap); err == nil || !strings.Contains(err.Error(), "the policy table's") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// liveKey is a Stripe-shaped key gitleaks finds, of kind sk or rk, assembled at run time so no source
// file of the repo carries one whole.
func liveKey(kind string) string { return kind + "_" + "live_" + "51MZq8fQ2xYv7Kp3Lw9Rt4Nb" }

// opengrep 1.30.0's real report on the TSX probe (gate/opengrep/testdata/probes/tsx) with the vendored
// sast-rules, its driver's rules cut to the one reported: the bare id
// javascript_react_rule-dangerouslysetinnerhtml shares "dangerously" with the flagged
// dangerouslySetInnerHTML. Canyon ran the scan, so the id names a rule Canyon sent and the report
// attaches; uploaded, the same report is refused for it (contracts/finding.md).
func TestACanyonRunReportsIdIsNotCheckedAgainstItsRegion(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-tsx-xss.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree, snap := treeAt(t, "../opengrep/testdata/probes/tsx"), mapped(t, "sql.string-concat", policy.Advisory)
	rep, err := NormalizeAs(raw, snap, tree, "opengrep")
	if err != nil || len(rep.Results) != 1 || rep.Results[0].RuleID != "code.injection.opengrep.javascript_react_rule-dangerouslysetinnerhtml" { // CWE-79: code.injection's
		t.Fatalf("Canyon's scan: %v %+v", err, rep.Results)
	}
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.opengrep-oss", Name: "Findings from opengrep-oss", State: policy.Advisory, Producer: policy.Sarif}) // its driver's name, uploaded
	if _, err := Normalize(raw, snap, tree); err != nil {                                                                                                      // its region is shown, so an id quoting it names nothing hidden
		t.Fatalf("the upload: %v", err)
	}
}

// ESLint 9.39.5's real report, from the image Canyon builds (executor/eslint.Dockerfile), its uris as the
// executor answers them: each of its four rules' results is a code.injection finding with the table's
// fix (contracts/snapshot.md, Tool results mapped to a policy).
func TestESLintsResultsAreCodeInjection(t *testing.T) {
	raw, err := os.ReadFile("testdata/eslint-9.39.5-run.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := NormalizeAs(raw, snapshot(t), treeAt(t, "testdata/eslint-run"), "eslint")
	if err != nil || len(rep.Results) != 4 {
		t.Fatalf("%v %+v", err, rep.Results)
	}
	for _, r := range rep.Results {
		if m, _, ok := policy.Mapped("eslint", r.RuleID); !ok || m.Policy != "code.injection" || r.Properties.Canyon.Fix != m.Fix {
			t.Errorf("%s: %+v", r.RuleID, r.Properties.Canyon)
		}
	}
}

// Opengrep 1.30.0's own report of a run that dropped a rule with an invalid regex and ran the rest
// (exit 2, executor.md), uploaded: the rule it ran reports, and the report names no rule it could not
// load — its Rule parse error notification is message text, never read; only the executor's answer
// names one (contracts/finding.md, In a run).
func TestAnUploadedReportNamesNoRuleItCouldNotLoad(t *testing.T) {
	raw, err := os.ReadFile("testdata/opengrep-1.30.0-unloaded.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"name": "Opengrep OSS"`), []byte(`"name": "opengrep"`), 1) // the tool an upload names is its driver's
	rep, err := Normalize(raw, snapshot(t), treeAt(t, "testdata/opengrep-unloaded"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].RuleID != "sarif.opengrep.org-eval" || !reflect.DeepEqual(rep, Report{Tool: "opengrep", Version: rep.Version, Results: rep.Results}) {
		t.Fatalf("%+v", rep)
	}
}

// ESLint 9.39.5's real report of a tree with two files it could not parse (the image Canyon builds,
// its uris as the executor answers them): each ESL0999 notification names its file by its first
// location, for the run to say it was not read whole, and the result of the file it did read is
// kept — whether Canyon ran ESLint or the report was uploaded (contracts/finding.md, In a run).
func TestAFileESLintCouldNotParseIsNamed(t *testing.T) {
	raw, err := os.ReadFile("testdata/eslint-9.39.5-unparsed.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	tree := treeAt(t, "testdata/eslint-unparsed")
	for _, tool := range []string{"eslint", ""} {
		rep, err := NormalizeAs(raw, snapshot(t), tree, tool)
		if err != nil || len(rep.Results) != 1 || rep.Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI != "src/flagged.js" ||
			!slices.Equal(rep.Unparsed, []string{"src/broken.ts", "src/broken.tsx"}) {
			t.Fatalf("%q: %v, results %+v, unparsed %q", tool, err, rep.Results, rep.Unparsed)
		}
	}
	// A notification names a file only when it is ESL0999 and its uri is exactly a file of the package.
	for name, edit := range map[string]func(n obj){
		"another descriptor":     func(n obj) { n["descriptor"] = obj{"id": "ESL0001"} },
		"no descriptor":          func(n obj) { delete(n, "descriptor") },
		"the tree's root":        func(n obj) { noteURI(n)["uri"] = "file:///scan/tree/src/broken.ts" },
		"a directory":            func(n obj) { noteURI(n)["uri"] = "src" },
		"a file the tree lacks":  func(n obj) { noteURI(n)["uri"] = "src/gone.ts" },
		"no locations":           func(n obj) { delete(n, "locations") },
		"as an execution notice": nil,
	} {
		var doc obj
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		inv := doc["runs"].([]any)[0].(obj)["invocations"].([]any)[0].(obj)
		notes := inv["toolConfigurationNotifications"].([]any)
		want := []string{"src/broken.tsx"}
		if edit == nil { // ESLint writes them as configuration notifications; an execution one names its file the same
			inv["toolExecutionNotifications"], inv["toolConfigurationNotifications"] = notes, nil
			want = []string{"src/broken.ts", "src/broken.tsx"}
		} else {
			edit(notes[0].(obj))
		}
		edited, _ := json.Marshal(doc)
		if rep, err := NormalizeAs(edited, snapshot(t), tree, "eslint"); err != nil || !slices.Equal(rep.Unparsed, want) {
			t.Errorf("%s: %v, unparsed %q", name, err, rep.Unparsed)
		}
	}
}

// noteURI is a notification's first location's artifactLocation.
func noteURI(n obj) obj {
	return n["locations"].([]any)[0].(obj)["physicalLocation"].(obj)["artifactLocation"].(obj)
}

// Canyon's ESLint names every file it linted as an artifact: a run that names none read none of the
// tree's JavaScript or TypeScript, so each is not checked, never a clean zero (the test cluster's #1553 scare). An
// uploaded report, whose formatter need not list artifacts, is read as it is.
func TestAnESLintRunThatLintedNoFileNamesEveryFileItReads(t *testing.T) {
	raw, err := os.ReadFile("testdata/eslint-9.39.5-run.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc obj
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	run := doc["runs"].([]any)[0].(obj)
	run["results"], run["artifacts"] = []any{}, []any{}
	none, _ := json.Marshal(doc)
	tree := treeAt(t, "testdata/eslint-run")
	if rep, err := NormalizeAs(none, snapshot(t), tree, "eslint"); err != nil || !slices.Equal(rep.Unparsed, []string{"src/run.ts"}) {
		t.Fatalf("a run that linted nothing: %v, unparsed %q", err, rep.Unparsed)
	}
	if rep, err := NormalizeAs(raw, snapshot(t), tree, "eslint"); err != nil || len(rep.Unparsed) != 0 {
		t.Fatalf("a run that linted the file: %v, unparsed %q", err, rep.Unparsed)
	}
	if rep, err := NormalizeAs(none, snapshot(t), tree, ""); err != nil || len(rep.Unparsed) != 0 {
		t.Fatalf("an uploaded report: %v, unparsed %q", err, rep.Unparsed)
	}
}

// A message ending in a full stop with no description to lead it keeps its title the problem's first
// sentence once Canyon's sentence follows it (review: a trailing "." moved the first sentence).
func TestTheDockerfileSentenceKeepsTheTitleTheFirstSentence(t *testing.T) {
	r := at("Dockerfile", 1, 1, 1, 10)
	r["ruleId"] = "CKV_DOCKER_1"
	r["message"] = obj{"text": "Ensure port 22 is not exposed."}
	raw := sarifOf(t, func(run obj) {
		run["tool"].(obj)["driver"] = obj{"name": "Checkov", "rules": []any{obj{"id": "CKV_DOCKER_1"}}}
	}, r)
	rep, err := NormalizeAs(raw, snapshot(t), files(t), "checkov")
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Results[0].Properties.Canyon
	if first, _, _ := strings.Cut(c.Problem, ". "); c.Title != first || rep.Results[0].Message.Text != c.Title {
		t.Fatalf("title %q, problem %q", c.Title, c.Problem)
	}
}

// A result of checkov's dockerfile checks never blocks, on the root Dockerfile or any other, whatever
// iac.misconfig's state: Canyon builds from no Dockerfile, and its problem says so. Another checkov
// result under the same rule held blocking still blocks (#1881; the QA batch's strict-iac).
func TestACheckovDockerfileResultNeverBlocks(t *testing.T) {
	strict := mapped(t, "iac.misconfig", policy.Blocking)
	for _, c := range []struct {
		uri, id string
		blocks  bool
	}{{"Dockerfile", "CKV_DOCKER_1", false}, {"docker/Dockerfile", "CKV2_DOCKER_1", false}, {"a.ts", "CKV_AWS_24", true}} {
		r := at(c.uri, 1, 1, 1, 10)
		r["ruleId"], r["level"] = c.id, "error"
		r["message"] = obj{"text": "Ensure port 22 is not exposed"}
		raw := sarifOf(t, func(run obj) {
			run["tool"].(obj)["driver"] = obj{"name": "Checkov", "rules": []any{obj{"id": c.id, "shortDescription": obj{"text": "Ensure port 22 is not exposed"}}}}
		}, r)
		rep, err := NormalizeAs(raw, strict, files(t), "checkov")
		if err != nil {
			t.Fatal(err)
		}
		got := derived(t, "checkov", rep, strict, files(t)).Runs[0].Results[0].Properties.Canyon
		want := "Ensure port 22 is not exposed"
		if !c.blocks {
			want += ". Canyon builds this app with buildpacks and never builds from this Dockerfile, so this does not block; it matters only where something else builds from it."
		}
		if *got.Blocks != c.blocks || got.Problem != want || got.Title != "Ensure port 22 is not exposed" {
			t.Errorf("%s on %s, severity %s: blocks %v, problem %q, title %q", c.id, c.uri, got.Severity, *got.Blocks, got.Problem, got.Title)
		}
	}
}
