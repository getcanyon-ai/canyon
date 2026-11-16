package policy

import (
	"regexp"
	"strings"
)

// Mapping is one row of the policy table (contracts/snapshot.md, Tool results mapped to a policy):
// a tool's results that carry one of Tags in their rule's properties.tags — every result when Tags is
// empty — are Policy's findings, at Severity when it is not empty, with Fix, or the tool's own fix when
// Fix is empty; but a result of the tool's rule Spared in a documentation file, as Linguist classifies its path, is not the row's.
type Mapping struct {
	Policy, Tool string
	Tags         []string
	Spared       string
	Severity     string
	Fix          string
}

// injection is code.injection's CWEs: command, code and path injection (relative path traversal too),
// cross-site scripting, open redirect, server-side request forgery, XML external entities and unsafe
// deserialization.
var injection = []string{"CWE-78", "CWE-77", "CWE-94", "CWE-95", "CWE-22", "CWE-23", "CWE-79", "CWE-601", "CWE-918", "CWE-611", "CWE-502"}

const injectionFix = "Keep the user's input data: pass it as an argument or a parameter, never as part of a command, path, page, URL or code, and check it against the values allowed."

// Table is the policy table, fixed per release, in its order: the first row that selects a result wins.
var Table = []Mapping{
	{Policy: "secrets.client-bundle", Tool: "gitleaks", Spared: "generic-api-key", Severity: "critical",
		Fix: "Rotate this credential — it has been in your code. If nothing reads it, delete it from the code; otherwise store the new one with canyon secret set <SECRET_NAME> and read it from the environment."},
	{Policy: "sql.string-concat", Tool: "opengrep", Tags: []string{"CWE-89"},
		Fix: "Pass the user's input to the query as a bound parameter instead of building the SQL text from it."},
	{Policy: "deps.known-vuln", Tool: "trivy", Tags: []string{"vulnerability"},
		Fix: "Upgrade the package to a version the advisory lists as fixed, then update the lockfile."},
	{Policy: "code.injection", Tool: "opengrep", Tags: injection, Fix: injectionFix},
	{Policy: "code.injection", Tool: "eslint", Fix: injectionFix},
	{Policy: "iac.misconfig", Tool: "checkov"}, // the tool's fix: its rule's help, the check and the resource it names
	{Policy: "iac.misconfig", Tool: "trivy", Tags: []string{"misconfiguration"}, Fix: "Set what the check names to the value it asks for, in the file at this line."},
}

// Selects reports whether the row selects a result of its tool whose rule carries tags: a tag equal
// to one of the row's, or, for a CWE, one that starts with it and a colon (CWE-89: Improper
// Neutralization…). It reads the tags alone, never a state, so a result is mapped once and keeps its
// id in every run.
func (m Mapping) Selects(tags []string) bool {
	if len(m.Tags) == 0 {
		return true
	}
	for _, t := range tags {
		for _, want := range m.Tags {
			if t == want || strings.HasPrefix(want, "CWE-") && strings.HasPrefix(t, want+":") {
				return true
			}
		}
	}
	return false
}

// Mapped is the row and the tool's own rule id of a ruleId a run of tool carries under the table's
// shape, <policy>.<tool>.<ruleId>; false for any other, sarif.<tool>.<ruleId> included.
func Mapped(tool, ruleID string) (Mapping, string, bool) {
	for _, m := range Table {
		if own, ok := strings.CutPrefix(ruleID, m.Policy+"."+tool+"."); ok && own != "" && m.Tool == tool {
			return m, own, true
		}
	}
	return Mapping{}, "", false
}

var notName = regexp.MustCompile(`[^A-Z0-9]+`)

// SecretName is the table's <SECRET_NAME> of a tool's rule id: upper-cased, each run outside A-Z and
// 0-9 one _, the ends' _ dropped, SECRET_ in front unless it starts with a letter — always a name
// canyon secret set takes, and nothing else.
func SecretName(ruleID string) string {
	name := strings.Trim(notName.ReplaceAllString(strings.ToUpper(ruleID), "_"), "_")
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return "SECRET_" + name
	}
	return name
}
