package gate

import (
	"slices"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Floor raises each result of log's agents' runs to the highest severity earlier, the runs of the project's
// last completed reviews, gave a result of the same agent with the same canyon/v1, and never lowers one
// (contracts/finding.md, A region reviewed before keeps its severity; #2872). The model rates unchanged code
// afresh whenever the app changes elsewhere, and a rating that dropped below what blocks would pass code the
// review before blocked. An earlier severity outside the four is read as critical. A result the raise makes
// block under its rule, in either gate, with no required_end_state or no do_not takes Canyon's own, as a
// candidate does (candidateResult).
func Floor(log *finding.Log, earlier finding.Log, snap policy.Snapshot) {
	agent := func(r finding.Run) bool {
		return slices.ContainsFunc(snap.Agents, func(a policy.Agent) bool { return a.ID == r.Tool.Driver.Name })
	}
	type key struct{ agent, fingerprint string }
	rated := map[key]finding.Severity{}
	for _, run := range earlier.Runs {
		if !agent(run) {
			continue
		}
		for _, r := range run.Results {
			k, severity := key{run.Tool.Driver.Name, r.PartialFingerprints.CanyonV1}, r.Properties.Canyon.Severity
			if _, err := severity.Level(); err != nil {
				severity = finding.Critical
			}
			if k.fingerprint != "" && rank(severity) > rank(rated[k]) {
				rated[k] = severity
			}
		}
	}
	for i := range log.Runs {
		run := &log.Runs[i]
		if !agent(*run) {
			continue
		}
		for j := range run.Results {
			r := &run.Results[j]
			c := &r.Properties.Canyon
			severity, ok := rated[key{run.Tool.Driver.Name, r.PartialFingerprints.CanyonV1}]
			if !ok || rank(severity) <= rank(c.Severity) {
				continue
			}
			c.Severity = severity
			r.Level, _ = severity.Level() // one of the four, read above
			if finding.Blocks(r.RuleID, heldTo(snap, r.RuleID), severity) && (c.RequiredEndState == "" || len(c.DoNot) == 0) {
				c.RequiredEndState, c.DoNot = canyonsGovernance(r.RuleID)
			}
		}
	}
}

// heldTo is the state a reviewer's result under rule is held to: blocking when the rule blocks before
// production — a finding that will block there is held to what blocks (contracts/finding.md) — else its own.
func heldTo(snap policy.Snapshot, rule string) policy.State {
	i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == rule })
	if i < 0 {
		return ""
	}
	if snap.Rules[i].Production == policy.Blocking {
		return policy.Blocking
	}
	return snap.Rules[i].State
}
