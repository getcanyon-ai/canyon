package finding

import (
	"fmt"
	"strings"

	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// Entries numbers a log's results as a person counts them (contracts/finding.md, One problem, one
// entry): for each result, runs[].results[] flattened in log order, the index of the first result of
// its entry. A package's CVEs join by Canyon rule, dependency and file; any other tool's finding by
// Canyon rule, file and start line; Canyon's own never join; blocking and advisory never share one.
func Entries(log Log) []int {
	var out []int
	first := map[string]int{}
	for _, run := range log.Runs {
		for _, r := range run.Results {
			key, joins := entryKey(run.Tool.Driver.Name, r)
			if i, seen := first[key]; joins && seen {
				out = append(out, i)
				continue
			}
			if joins {
				first[key] = len(out)
			}
			out = append(out, len(out))
		}
	}
	return out
}

// Count is how many entries of a log block and how many do not: the verdict's blocking and warnings.
func Count(log Log) (blocking, warnings int) {
	entries, i := Entries(log), 0
	for _, run := range log.Runs {
		for _, r := range run.Results {
			if entries[i] == i {
				if r.Properties.Canyon.Blocks != nil && *r.Properties.Canyon.Blocks {
					blocking++
				} else {
					warnings++
				}
			}
			i++
		}
	}
	return blocking, warnings
}

// entryKey is what a result joins on, and false for one that never joins: Canyon's own finding.
func entryKey(tool string, r Result) (string, bool) {
	rule, by := canyonRule(tool, r.RuleID)
	if by == "" || len(r.Locations) == 0 {
		return "", false
	}
	blocks := r.Properties.Canyon.Blocks != nil && *r.Properties.Canyon.Blocks
	where := r.Locations[0].PhysicalLocation
	if strings.Contains(r.RuleID, ".trivy.") {
		if a, ok := trivy.Read(r.Properties.Canyon.Problem); ok {
			return fmt.Sprintf("cve\x00%t\x00%s\x00%s\x00%s\x00%s", blocks, rule, a.Name, a.Installed, where.ArtifactLocation.URI), true
		}
	}
	return fmt.Sprintf("line\x00%t\x00%s\x00%s\x00%d", blocks, rule, where.ArtifactLocation.URI, where.Region.StartLine), true
}

// canyonRule is the Canyon rule a result is under and the tool that found it (contracts/snapshot.md):
// sarif.<tool> for an imported result no policy covers (sarif.<tool>.<its rule>), the policy a tool's
// result is mapped to (<policy>.<tool>.<its rule>), else the result's own rule and no tool.
func canyonRule(driver, ruleID string) (rule, tool string) {
	if space, rest, _ := strings.Cut(ruleID, "."); space == "sarif" {
		if tool, _, ok := strings.Cut(rest, "."); ok {
			return "sarif." + tool, tool
		}
	}
	tool, ok := strings.CutPrefix(driver, "sarif/")
	if !ok {
		return ruleID, ""
	}
	if policy, _, mapped := strings.Cut(ruleID, "."+tool+"."); mapped {
		return policy, tool
	}
	return ruleID, ""
}
