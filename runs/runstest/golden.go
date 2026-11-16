// Package runstest holds the gates tests run the pipeline with: the recorded claims-portal log,
// whatever the tree.
package runstest

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Golden answers the recorded claims-portal verdict, whatever the tree: the scanners' run, the
// reviewer's three, and gitleaks' report of the Stripe key at Runs[2], the one blocking finding.
func Golden(context.Context, string, string, policy.Snapshot, gate.Report) (finding.Log, error) {
	var l finding.Log
	if err := json.Unmarshal(contracts.GoldenLog, &l); err != nil {
		return finding.Log{}, err
	}
	return l, nil
}

// CleanGolden is the gate's part of Golden — the scanners' and the reviewer's runs, gitleaks' report
// left to the run's own tools — without a blocking finding: the fixed claims-portal, for tests that
// need a clean verdict.
func CleanGolden(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
	log, err := Golden(ctx, dir, tree, snap, report)
	if err != nil {
		return log, err
	}
	log.Runs = slices.DeleteFunc(log.Runs, func(r finding.Run) bool { by, _ := policy.ByDriver(r.Tool.Driver.Name); return by == policy.Sarif })
	for i := range log.Runs {
		kept := log.Runs[i].Results[:0]
		for _, r := range log.Runs[i].Results {
			if !*r.Properties.Canyon.Blocks {
				kept = append(kept, r)
			}
		}
		log.Runs[i].Results = kept
	}
	return log, nil
}
