package finding

import (
	"reflect"
	"testing"
)

// One problem is one entry (contracts/finding.md, One problem, one entry): two opengrep rules on one
// line under one Canyon rule, whatever the tool, and one package's CVEs; never Canyon's own two on one
// line, another line, another file, another rule, or a blocking finding with an advisory one.
func TestEntriesJoinOneProblemAsAPersonCountsIt(t *testing.T) {
	yes, no := true, false
	at := func(rule, uri string, line int, blocks *bool, problem string) Result {
		r := Result{RuleID: rule, Locations: []Location{{PhysicalLocation: PhysicalLocation{ArtifactLocation: ArtifactLocation{URI: uri}, Region: Region{StartLine: line}}}}}
		r.Properties.Canyon.Blocks, r.Properties.Canyon.Problem = blocks, problem
		return r
	}
	cve := func(id string) string { return "Package: lodash\nInstalled Version: 4.17.4\nVulnerability " + id }
	log := Log{Runs: []Run{
		{Tool: Tool{Driver: Driver{Name: "sarif/opengrep"}}, Results: []Result{
			at("sql.injection.opengrep.tainted-sql", "server.js", 10, &yes, ""),  // 0
			at("sql.injection.opengrep.express-sqli", "server.js", 10, &yes, ""), // 1: the line of 0
			at("sql.injection.opengrep.tainted-sql", "server.js", 11, &yes, ""),  // 2: another line
			at("sql.injection.opengrep.tainted-sql", "db.js", 10, &yes, ""),      // 3: another file
			at("xss.reflected.opengrep.express-xss", "server.js", 10, &yes, ""),  // 4: another rule
			at("sql.injection.opengrep.tainted-sql", "server.js", 10, &no, ""),   // 5: advisory
			at("sarif.opengrep.unmapped-rule", "server.js", 10, &yes, ""),        // 6: no policy covers it
		}},
		{Tool: Tool{Driver: Driver{Name: "sarif/eslint"}}, Results: []Result{
			at("sql.injection.eslint.no-raw-query", "server.js", 10, &yes, ""), // 7: another tool, the line of 0
		}},
		{Tool: Tool{Driver: Driver{Name: "sarif/trivy"}}, Results: []Result{
			at("deps.known-vuln.trivy.CVE-2018-16487", "package-lock.json", 17, &yes, cve("CVE-2018-16487")), // 8
			at("deps.known-vuln.trivy.CVE-2019-10744", "package-lock.json", 40, &yes, cve("CVE-2019-10744")), // 9: its package's
		}},
		{Tool: Tool{Driver: Driver{Name: "canyon-review"}}, Results: []Result{
			at("auth.unauthenticated-route", "server.js", 10, &yes, ""), // 10
			at("auth.unauthenticated-route", "server.js", 10, &yes, ""), // 11: Canyon's own, never joined
		}},
	}}
	if got, want := Entries(log), []int{0, 0, 2, 3, 4, 5, 6, 0, 8, 8, 10, 11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entries: %v, want %v", got, want)
	}
	if blocking, warnings := Count(log); blocking != 8 || warnings != 1 {
		t.Fatalf("counted %d blocking, %d warnings; want 8 and 1", blocking, warnings)
	}
}
