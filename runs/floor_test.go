package runs

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rating is a host's model that answers counting's one finding on the live tree's payments.ts, rated
// severities[n] on its nth call, the last again after them: a model that rates the same code differently.
// "room" is an answer out of room, a review that read nothing.
type rating struct {
	calls      atomic.Int32
	severities []string
}

func (m *rating) Review(context.Context, gate.Request) (gate.Response, error) {
	n := int(m.calls.Add(1)) - 1
	severity := m.severities[min(n, len(m.severities)-1)]
	if severity == "room" {
		return gate.Response{}, gate.ErrOutOfRoom
	}
	return gate.Response{Results: []gate.Candidate{{Rule: "perf.unbounded-refetch", File: "apps/web/src/lib/payments.ts", StartLine: 1, EndLine: 1,
		Severity: severity, Title: "Stripe is built on each import", Problem: "A client per module load.", Fix: "Build it once."}}}, nil
}

// blockingRefetch is the test policy with perf.unbounded-refetch blocking: its high finding blocks.
func blockingRefetch(t *testing.T) policy.Snapshot {
	t.Helper()
	snap, err := policy.WithStates(testPolicy(), map[string]policy.State{"perf.unbounded-refetch": policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// severityOf is the one result's severity in a reviewer run, "" when it has none.
func severityOf(r finding.Run) finding.Severity {
	if len(r.Results) != 1 {
		return ""
	}
	return r.Results[0].Properties.Canyon.Severity
}

// changedElsewhere stands in for an app changed outside the finding's code: the review cache, keyed on the
// whole tree, cannot answer the next review, so the model rates the same code again.
func changedElsewhere(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	if _, err := p.Exec(ctx, "DELETE FROM review_cache"); err != nil {
		t.Fatal(err)
	}
}

// The same code rated high, then medium by the next review, stays high and still blocks (#2872): the
// tester journey's server.js was rated differently once another file changed.
func TestUnchangedCodeRatedLowerKeepsTheSeverityThatBlocked(t *testing.T) {
	p, ctx := live(t, nil)
	m := &rating{severities: []string{"high", "medium"}}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	if state, reason, r := reviewed(t, ctx, p, snap); state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("the first review blocks on high: %s %s %s", state, why(reason), severityOf(r))
	}
	changedElsewhere(t, ctx, p)
	state, reason, r := reviewed(t, ctx, p, snap)
	if m.calls.Load() != 2 || state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("medium after high stays high and blocks: %d calls, %s %s, %s", m.calls.Load(), state, why(reason), severityOf(r))
	}
	if c := r.Results[0].Properties.Canyon; c.RequiredEndState == "" || len(c.DoNot) == 0 {
		t.Fatalf("a kept severity that blocks carries governance prose: %q %q", c.RequiredEndState, c.DoNot)
	}
}

// The model rating the same code higher is the model's own: medium, then high, is high and blocks.
func TestUnchangedCodeRatedHigherTakesTheHigherSeverity(t *testing.T) {
	p, ctx := live(t, nil)
	m := &rating{severities: []string{"medium", "high"}}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	if state, reason, r := reviewed(t, ctx, p, snap); state != "clean" || severityOf(r) != finding.Medium {
		t.Fatalf("medium does not block: %s %s %s", state, why(reason), severityOf(r))
	}
	changedElsewhere(t, ctx, p)
	if state, reason, r := reviewed(t, ctx, p, snap); state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("high after medium is high and blocks: %s %s %s", state, why(reason), severityOf(r))
	}
}

// A review the cache replays is floored too: the tree's cached medium, reviewed again after another
// model's review rated the same code high, is high.
func TestAReplayedReviewIsRaisedToTheLastReviewsSeverity(t *testing.T) {
	p, ctx := live(t, nil)
	m := &rating{severities: []string{"medium", "high"}}
	snap := blockingRefetch(t)
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	if _, _, r := reviewed(t, ctx, p, snap); severityOf(r) != finding.Medium {
		t.Fatalf("the cached review is medium: %s", severityOf(r))
	}
	pipe.Gate = Gate{Model: m, ModelID: "another-model", Pool: p}.Review // a cache miss: another model rates it high
	if _, _, r := reviewed(t, ctx, p, snap); severityOf(r) != finding.High {
		t.Fatalf("the other model rates it high: %s", severityOf(r))
	}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	state, reason, r := reviewed(t, ctx, p, snap)
	if m.calls.Load() != 2 || !slices.Contains(notes(r), gate.Reused) || state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("the replayed medium is raised to high and blocks: %d calls, notes %v, %s %s, %s", m.calls.Load(), notes(r), state, why(reason), severityOf(r))
	}
}

// The last review's findings that cannot be read fail the run: it never goes on unrated.
func TestALastReviewThatCannotBeReadFailsTheRun(t *testing.T) {
	p, ctx := live(t, nil)
	m := &rating{severities: []string{"high"}}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	if state, reason, _ := reviewed(t, ctx, p, snap); state != "blocked" {
		t.Fatalf("the first review blocks: %s %s", state, why(reason))
	}
	// The newest verdict of the project, after the first run's: findings that are not a log (rows are append-only).
	if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, data)
		SELECT 'run', run_id, project_id, 'verdict', '{"state": "blocked", "blocking": 1, "warnings": 0, "findings": "not a log"}' FROM timeline_rows WHERE kind = 'verdict'`); err != nil {
		t.Fatal(err)
	}
	if state, reason, _ := reviewed(t, ctx, p, snap); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("an unreadable last review fails the run: %s %s", state, why(reason))
	}
}

// A review that read nothing in between — its verdict holds no result of the code — never resets the
// floor: high, then a review out of room, then medium, is high and blocks. So with a branch check or a
// rollback review in between: the floor is the highest over the project's last 20 verdicts.
func TestAReviewThatReadNothingInBetweenKeepsTheSeverity(t *testing.T) {
	p, ctx := live(t, nil)
	m := &rating{severities: []string{"high", "room", "medium"}}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	if state, reason, r := reviewed(t, ctx, p, snap); state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("the first review blocks on high: %s %s %s", state, why(reason), severityOf(r))
	}
	changedElsewhere(t, ctx, p)
	if state, reason, r := reviewed(t, ctx, p, snap); state != "blocked" || len(r.Results) != 0 {
		t.Fatalf("a review out of room reads nothing and blocks on gate.unreadable: %s %s %+v", state, why(reason), r.Results)
	}
	state, reason, r := reviewed(t, ctx, p, snap)
	if m.calls.Load() != 3 || state != "blocked" || severityOf(r) != finding.High {
		t.Fatalf("medium after high and a review that read nothing stays high and blocks: %d calls, %s %s, %s", m.calls.Load(), state, why(reason), severityOf(r))
	}
}

// An earlier verdict whose findings are null or an empty object is no log in Canyon's profile: it fails
// the run, never floors nothing.
func TestAnEarlierVerdictWithNoLogFailsTheRun(t *testing.T) {
	for _, findings := range []string{"null", "{}"} {
		t.Run(findings, func(t *testing.T) {
			p, ctx := live(t, nil)
			pipe.Gate = Gate{Model: &rating{severities: []string{"high"}}, ModelID: "claude-opus-5", Pool: p}.Review
			snap := blockingRefetch(t)
			if state, reason, _ := reviewed(t, ctx, p, snap); state != "blocked" {
				t.Fatalf("the first review blocks: %s %s", state, why(reason))
			}
			if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, data)
				SELECT 'run', run_id, project_id, 'verdict', jsonb_build_object('state', 'blocked', 'blocking', 1, 'warnings', 0, 'findings', $1::jsonb) FROM timeline_rows WHERE kind = 'verdict'`, findings); err != nil {
				t.Fatal(err)
			}
			if state, reason, _ := reviewed(t, ctx, p, snap); state != "failed" || reason == nil || *reason != "error" {
				t.Fatalf("findings %s fail the run: %s %s", findings, state, why(reason))
			}
		})
	}
}
