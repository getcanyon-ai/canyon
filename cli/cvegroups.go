package cli

import (
	"fmt"
	"slices"
	"strings"

	gatefinding "github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// severities in the order a group's highest is read: the four contracts/finding.md allows.
var severities = []string{"low", "medium", "high", "critical"}

// cveGroup is one advisory entry: a finding alone, or a package's trivy CVEs together, keyed by the
// rule without its CVE id, the file, the package and its installed version (design/specs/cli.md §5.3).
type cveGroup struct {
	first          finding
	entry          int       // its first finding's entry among the findings it was grouped from (gatefinding.Entries)
	members        []finding // each CVE's finding, once, the first among them
	rule, severity string
	pkg, installed string
	cves           []string
	fixes          [][]string // each CVE's fixed versions as trivy names them, for the CVEs naming any
	listed         []string   // every fixed version any of them names, once, as trivy wrote it
	unfixed        []string   // the CVEs naming none
	tools          []finding  // a tool's findings the entry credits under found by: its own, or one Canyon rule's on its file and line by several tools' rules (§5.3); nil for Canyon's own and a package's CVEs
}

// cveGroups is findings in order, each trivy CVE joined to the first of its entry: its package's.
func cveGroups(findings []finding) []*cveGroup {
	var out []*cveGroup
	entries, at := entryOf(findings), map[int]int{} // a package's entry, where it stands in out
	for n, f := range findings {
		e := &cveGroup{first: f, entry: entries[n], severity: f.Properties.Canyon.Severity}
		pkg, installed, fixed, ok := trivyPackage(f)
		if !ok {
			out = append(out, e)
			continue
		}
		rule, id, _ := strings.Cut(f.RuleID, ".trivy.") // trivy's own id may carry dots: pyup.io-38834
		if rule == "sarif" {                            // no policy covers it: the tool's rule (contracts/snapshot.md)
			rule = "sarif.trivy"
		}
		i, seen := at[e.entry]
		if !seen {
			e.rule, e.pkg, e.installed = rule, pkg, installed
			out, i, at[e.entry] = append(out, e), len(out), len(out)
		}
		g := out[i]
		if slices.Contains(g.cves, id) { // one vulnerability, however often trivy names it
			continue
		}
		g.cves, g.members = append(g.cves, id), append(g.members, f)
		if slices.Index(severities, f.Properties.Canyon.Severity) > slices.Index(severities, g.severity) {
			g.severity = f.Properties.Canyon.Severity
		}
		if fixed == "" {
			g.unfixed = append(g.unfixed, id)
			continue
		}
		list := strings.Split(fixed, ", ")
		g.fixes = append(g.fixes, list)
		for _, v := range list {
			if !slices.Contains(g.listed, v) {
				g.listed = append(g.listed, v)
			}
		}
	}
	return out
}

// byLine credits each tool's finding to its tool's rule, and joins entries of one finding each into the
// first of their entry: one Canyon rule on one file and line several tools' rules found, its severity the
// highest of theirs (cli.md §5.2, §5.3).
func byLine(entries []*cveGroup) []*cveGroup {
	var out []*cveGroup
	first := map[int]*cveGroup{}
	for _, e := range entries {
		if _, _, own := toolRule(e.first); own != "" && len(e.cves) <= 1 {
			e.tools = []finding{e.first}
		}
		g, seen := first[e.entry]
		if !seen {
			out = append(out, e)
			first[e.entry] = e
			continue
		}
		g.tools = append(g.tools, e.first)
		if slices.Index(severities, e.severity) > slices.Index(severities, g.severity) {
			g.severity = e.severity
		}
	}
	return out
}

// entryOf numbers findings by the one rule the server counts by, gatefinding.Entries (contracts/finding.md,
// One problem, one entry): each finding is a run of its own tool, which is all that rule reads of a log.
func entryOf(findings []finding) []int {
	var log gatefinding.Log
	for _, f := range findings {
		r := gatefinding.Result{RuleID: f.RuleID}
		r.Properties.Canyon.Blocks, r.Properties.Canyon.Problem = f.Properties.Canyon.Blocks, f.Properties.Canyon.Problem
		if len(f.Locations) > 0 {
			where := f.Locations[0].PhysicalLocation
			r.Locations = []gatefinding.Location{{PhysicalLocation: gatefinding.PhysicalLocation{
				ArtifactLocation: gatefinding.ArtifactLocation{URI: where.ArtifactLocation.URI}, Region: gatefinding.Region{StartLine: where.Region.StartLine}}}}
		}
		log.Runs = append(log.Runs, gatefinding.Run{Tool: gatefinding.Tool{Driver: gatefinding.Driver{Name: f.Tool}}, Results: []gatefinding.Result{r}})
	}
	return gatefinding.Entries(log)
}

// toolRule is the Canyon rule a finding is under and, when a tool found it, the tool and its own rule:
// the policy its result is mapped to (<policy>.<tool>.<its rule>), sarif.<tool> for one no policy covers
// (sarif.<tool>.<its rule>), or the finding's rule for Canyon's own, with no tool's (contracts/snapshot.md).
func toolRule(f finding) (rule, tool, own string) {
	if tool, own, ok := f.imported(); ok {
		return "sarif." + tool, tool, own
	}
	tool, ok := strings.CutPrefix(f.Tool, "sarif/")
	if !ok {
		return f.RuleID, "", ""
	}
	if policy, own, mapped := strings.Cut(f.RuleID, "."+tool+"."); mapped {
		return policy, tool, own
	}
	return f.RuleID, "", ""
}

// top is the first of an entry's tools' findings at its highest severity: the one whose title, place and
// problem it reads.
func (e *cveGroup) top() finding {
	return e.tools[slices.IndexFunc(e.tools, func(f finding) bool { return f.Properties.Canyon.Severity == e.severity })]
}

// named is the rule a finding entry's rule: line names and the finding whose place it prints (cli.md §5.2):
// a package's CVEs' rule at the first, a tool's Canyon rule at the top of its findings, else the finding's own.
func (e *cveGroup) named() (string, finding) {
	switch {
	case len(e.members) > 1:
		return e.rule, e.first
	case e.tools != nil:
		rule, _, _ := toolRule(e.top())
		return rule, e.top()
	}
	return e.first.RuleID, e.first
}

// toolFixes is each distinct fix among an entry's tools' findings, in order.
func (e *cveGroup) toolFixes() []string {
	var fixes []string
	for _, f := range e.tools {
		if !slices.Contains(fixes, f.Properties.Canyon.Fix) {
			fixes = append(fixes, f.Properties.Canyon.Fix)
		}
	}
	return fixes
}

// foundTools is a person's found-by line (cli.md §3.3): the tools in order, and how many of their rules
// agreed when more than one did; the rules' ids are ai's and json's.
func (e *cveGroup) foundTools() string {
	var tools, rules []string
	for _, f := range e.tools {
		_, tool, own := toolRule(f)
		if !slices.Contains(tools, tool) {
			tools = append(tools, tool)
		}
		if credit := tool + "\x00" + own; !slices.Contains(rules, credit) {
			rules = append(rules, credit)
		}
	}
	for i := range tools {
		tools[i] = cell(tools[i])
	}
	said := strings.Join(tools, ", ")
	if n := len(tools); n > 1 {
		said = strings.Join(tools[:n-1], ", ") + " and " + tools[n-1]
	}
	if len(rules) > 1 {
		said += fmt.Sprintf(" (%d rules)", len(rules))
	}
	return said
}

// foundBy is every tool's rule an entry credits, `<tool>'s rule <its rule>`, once each, in order: one rule
// matching twice on a line is credited once.
func (e *cveGroup) foundBy() string {
	var by []string
	for _, f := range e.tools {
		_, tool, own := toolRule(f)
		if credit := cell(tool) + "'s rule " + cell(own); !slices.Contains(by, credit) {
			by = append(by, credit)
		}
	}
	return strings.Join(by, ", ")
}

// trivyPackage reads a trivy result's package, installed version and fixed versions from trivy's own
// message lines, which a finding's problem carries after the rule's description (contracts/finding.md,
// read by internal/trivy). ok is false for any other finding.
func trivyPackage(f finding) (pkg, installed, fixed string, ok bool) {
	if !strings.Contains(f.RuleID, ".trivy.") {
		return "", "", "", false
	}
	a, ok := trivy.Read(f.Properties.Canyon.Problem)
	return a.Name, a.Installed, a.Fixed, ok
}

// summary is a group's one line: the package, how many CVEs, the one version that fixes every CVE naming
// a fix (trivy.FixedIn), or every version trivy names when there is no such plain version, and how many name none.
func (g *cveGroup) summary() string {
	said := fmt.Sprintf("%s %s: %d known vulnerabilities", g.pkg, g.installed, len(g.cves))
	if len(g.fixes) == 0 {
		return said + ", no fixed version yet"
	}
	if v, ok := trivy.FixedIn(g.installed, g.fixes); ok {
		said += ", fixed in " + v
	} else {
		said += ", fixed versions " + strings.Join(g.listed, ", ")
	}
	if len(g.unfixed) > 0 {
		said += fmt.Sprintf(" (%d with none)", len(g.unfixed))
	}
	return said
}
