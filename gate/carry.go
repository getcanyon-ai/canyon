package gate

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Verdict is one of the project's earlier completed reviews: its run's id and the log its verdict holds.
type Verdict struct {
	Run string
	Log finding.Log
}

// Carry adds to log's agents' runs each blocking result earlier — the project's last completed reviews, newest
// first — holds that this review left out, while its code stands in the package at root (contracts/finding.md, A
// blocking finding an earlier review made stays while its code stands; #2900). A result is carried when no result of
// its agent in log has its canyon/v1, its rule is in snap, granted to the agent, and blocks at its severity in either
// gate, and its code stands: the package holds its file and, at some line, the snippet the reviewer would build for as
// many lines at the same columns is byte for byte its own. Its lines are then where its code is now. A file that
// tells nothing — unreadable, not valid UTF-8, or no snippet could be built — carries it at its own lines (fail
// closed). The carried result is the newest one, at the highest severity earlier gave it, with carried_from the run
// whose review reported it; each run that carries any says how many in one note. Call it after Floor, before blocks
// is derived.
func Carry(log *finding.Log, earlier []Verdict, snap policy.Snapshot, root string) {
	type key struct{ agent, fingerprint string }
	have := map[key]bool{}
	for _, run := range log.Runs {
		for _, r := range run.Results {
			have[key{run.Tool.Driver.Name, r.PartialFingerprints.CanyonV1}] = true
		}
	}
	type candidate struct {
		result finding.Result
		from   string
		rule   policy.Rule // its rule in snap, which the filter below finds
	}
	var order []key
	found := map[key]*candidate{}
	for _, v := range earlier {
		for _, run := range v.Log.Runs {
			if !slices.ContainsFunc(snap.Agents, func(a policy.Agent) bool { return a.ID == run.Tool.Driver.Name }) {
				continue
			}
			for _, r := range run.Results {
				k := key{run.Tool.Driver.Name, r.PartialFingerprints.CanyonV1}
				if k.fingerprint == "" || have[k] {
					continue
				}
				severity := r.Properties.Canyon.Severity
				if _, err := severity.Level(); err != nil {
					severity = finding.Critical
				}
				if c, ok := found[k]; ok { // a newer verdict holds it: its words, at the highest severity any gave
					if rank(severity) > rank(c.result.Properties.Canyon.Severity) {
						c.result.Properties.Canyon.Severity = severity
					}
					continue
				}
				r.Properties.Canyon.Severity = severity
				found[k] = &candidate{result: r, from: cmp.Or(r.Properties.Canyon.CarriedFrom, v.Run)}
				order = append(order, k)
			}
		}
	}
	order = slices.DeleteFunc(order, func(k key) bool { // only what still blocks under its rule is carried
		c := found[k]
		rule, ok := granted(snap, k.agent, c.result.RuleID)
		c.rule = rule
		return !ok || !finding.Blocks(rule.ID, heldTo(snap, rule.ID), c.result.Properties.Canyon.Severity)
	})
	if len(order) == 0 {
		return
	}
	stands := codeStands(root)
	carried := map[string]int{}
	for _, k := range order {
		c := found[k]
		r := c.result
		at, ok := stands(r)
		if !ok {
			continue // its code changed
		}
		r.Locations = slices.Clone(r.Locations) // its own: the earlier verdict's result stays as it was
		g := &r.Locations[0].PhysicalLocation.Region
		g.EndLine += at - g.StartLine
		g.StartLine = at
		r.Level, _ = r.Properties.Canyon.Severity.Level() // one of the four, read above
		r.Properties.Canyon.CarriedFrom = c.from
		r.Properties.Canyon.Blocks = nil // the server derives it, after this
		run := agentRun(log, k.agent, snap)
		declare(run, c.rule)
		run.Results = append(run.Results, r)
		carried[k.agent]++
	}
	for i := range log.Runs {
		run := &log.Runs[i]
		n := carried[run.Tool.Driver.Name]
		if n == 0 {
			continue
		}
		text := CarriedOne
		if n > 1 {
			text = fmt.Sprintf(CarriedMany, n)
		}
		if len(run.Invocations) == 0 {
			run.Invocations = []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: []finding.Notification{}}}
		}
		inv := &run.Invocations[0]
		inv.ToolExecutionNotifications = append(inv.ToolExecutionNotifications, finding.Notification{Level: "note", Message: finding.Message{Text: Note(run.Tool.Driver.Name, text)}})
	}
}

// codeStands answers, for an earlier reviewer result, the first line its code stands at in the package at root, and
// whether it stands at all (Carry). The files are read once, masked as the reviewer shows them (excerpts). A
// package with no file at the result's uri changed its code; one whose files cannot be read, or whose file is not
// valid UTF-8, tells nothing, so the result stands at its own lines (fail closed).
func codeStands(root string) func(finding.Result) (int, bool) {
	tree, walkErr := Walk(root)
	source, shownText, _, err := excerpts(root)
	return func(r finding.Result) (int, bool) {
		g := r.Locations[0].PhysicalLocation.Region
		uri := r.Locations[0].PhysicalLocation.ArtifactLocation.URI
		if walkErr != nil {
			return g.StartLine, true
		}
		if !slices.Contains(tree.Files(), uri) {
			return 0, false
		}
		text, ok := source[uri]
		if err != nil || !ok { // unread, or not shown as UTF-8: nothing tells whether it changed
			return g.StartLine, true
		}
		lines, shown := strings.Split(text, "\n"), strings.Split(shownText[uri], "\n")
		height := g.EndLine - g.StartLine
		for at := 1; at+height <= len(lines); at++ {
			span, joined, from, to, ok := reviewSpan(lines, shown, at, at+height)
			if !ok || span.StartColumn != g.StartColumn || span.EndColumn != g.EndColumn {
				continue
			}
			probe := finding.Result{Locations: []finding.Location{{PhysicalLocation: finding.PhysicalLocation{Region: finding.Region{StartColumn: span.StartColumn, EndColumn: span.EndColumn}}}}}
			probe.Show(joined, from, to)
			if probe.Locations[0].PhysicalLocation.Region.Snippet.Text == g.Snippet.Text && sameShown(probe.Properties.Canyon.Shown, r.Properties.Canyon.Shown) {
				return at, true
			}
		}
		return 0, false
	}
}

// sameShown is whether two carets on a snippet are the same: absent alike, or at the same columns.
func sameShown(a, b *finding.Shown) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// granted is rule as snap holds it, and whether snap holds it and grants agent its namespace.
func granted(snap policy.Snapshot, agent, rule string) (policy.Rule, bool) {
	ns, _, _ := strings.Cut(rule, ".")
	a := slices.IndexFunc(snap.Agents, func(a policy.Agent) bool { return a.ID == agent })
	r := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == rule })
	if a < 0 || r < 0 || !slices.Contains(snap.Agents[a].Namespace, ns) {
		return policy.Rule{}, false
	}
	return snap.Rules[r], true
}

// agentRun is log's run of agent, a new one at the end when the review left it none.
func agentRun(log *finding.Log, agent string, snap policy.Snapshot) *finding.Run {
	if i := slices.IndexFunc(log.Runs, func(r finding.Run) bool { return r.Tool.Driver.Name == agent }); i >= 0 {
		return &log.Runs[i]
	}
	log.Runs = append(log.Runs, finding.Run{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: finding.Driver{Name: agent, Version: DriverVersion, Rules: []finding.Rule{}}}, Results: []finding.Result{}})
	return &log.Runs[len(log.Runs)-1]
}

// declare adds rule, the snapshot's, to run's driver's rules unless it is there.
func declare(run *finding.Run, rule policy.Rule) {
	if !slices.ContainsFunc(run.Tool.Driver.Rules, func(r finding.Rule) bool { return r.ID == rule.ID }) {
		run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, finding.Rule{ID: rule.ID, Name: rule.Name})
	}
}
