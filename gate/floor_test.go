package gate

import (
	"encoding/json"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// review is the golden reviewer log (testdata/review) with its canyon-review results rated as severities
// says, in order, each without governance prose: a model's answer, before Floor.
func review(t *testing.T, severities ...finding.Severity) finding.Log {
	t.Helper()
	g, _ := golden(t)
	raw, err := json.Marshal(g) // a deep copy: each call its own log
	if err != nil {
		t.Fatal(err)
	}
	var l finding.Log
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	for i, s := range severities {
		r := &l.Runs[1].Results[i]
		r.Properties.Canyon.Severity, r.Properties.Canyon.RequiredEndState, r.Properties.Canyon.DoNot = s, "", nil
		r.Level, _ = s.Level()
	}
	return l
}

// floorSnap is the default snapshot with perf.unbounded-refetch, the golden's third reviewer result, blocking.
func floorSnap(t *testing.T) policy.Snapshot {
	t.Helper()
	snap, err := (&policy.Builder{}).Build(nil)
	if err == nil {
		snap, err = policy.WithStates(snap, map[string]policy.State{"perf.unbounded-refetch": policy.Blocking})
	}
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Unchanged code the review before rated high, rated medium now, stays high: it blocks as it did, with
// Canyon's governance prose where the model gave none (#2872).
func TestAResultTheReviewBeforeRatedHigherKeepsThatSeverity(t *testing.T) {
	earlier, now := review(t, finding.High, finding.High, finding.High), review(t, finding.High, finding.High, finding.Medium)
	Floor(&now, earlier, floorSnap(t))
	r := now.Runs[1].Results[2]
	if c := r.Properties.Canyon; c.Severity != finding.High || r.Level != "error" || c.RequiredEndState == "" || len(c.DoNot) == 0 {
		t.Fatalf("medium after high stays high, level error, with governance prose: %s %s %q %q", c.Severity, r.Level, c.RequiredEndState, c.DoNot)
	}
	if !finding.Blocks(r.RuleID, policy.Blocking, r.Properties.Canyon.Severity) {
		t.Fatal("the kept severity blocks under the blocking rule")
	}
}

// A higher rating now is the model's own: the floor never lowers a result.
func TestAResultRatedHigherNowKeepsTheModelsSeverity(t *testing.T) {
	earlier, now := review(t, finding.Medium, finding.Medium, finding.Medium), review(t, finding.Critical, finding.High, finding.High)
	Floor(&now, earlier, floorSnap(t))
	for i, want := range []finding.Severity{finding.Critical, finding.High, finding.High} {
		if got := now.Runs[1].Results[i].Properties.Canyon.Severity; got != want {
			t.Fatalf("result %d: a raise is never undone: %s, want %s", i, got, want)
		}
	}
}

// The floor is the same agent's rating of the same canyon/v1: another run's result with that fingerprint,
// or a result of new code, raises nothing; an earlier severity outside the four reads as critical.
func TestTheFloorIsTheSameAgentsRatingOfTheSameCode(t *testing.T) {
	earlier, now := review(t, finding.High, finding.High, finding.High), review(t, finding.Low, finding.Low, finding.Low)
	earlier.Runs[1].Tool.Driver.Name = "custom-rules"                                           // another agent rated this code
	earlier.Runs[0].Results[0].PartialFingerprints = now.Runs[1].Results[0].PartialFingerprints // the scanners' run, not an agent's
	Floor(&now, earlier, floorSnap(t))
	for i, r := range now.Runs[1].Results {
		if r.Properties.Canyon.Severity != finding.Low {
			t.Fatalf("result %d: only the same agent's rating floors it: %s", i, r.Properties.Canyon.Severity)
		}
	}
	earlier = review(t, "severe", finding.High, finding.High)
	now = review(t, finding.Low, finding.Low, finding.Low)
	now.Runs[1].Results[1].PartialFingerprints.CanyonV1 = "f_" + "0000000000000000000000000000000000000000000000000000000000000000" // its code changed
	Floor(&now, earlier, floorSnap(t))
	if got := []finding.Severity{now.Runs[1].Results[0].Properties.Canyon.Severity, now.Runs[1].Results[1].Properties.Canyon.Severity}; got[0] != finding.Critical || got[1] != finding.Low {
		t.Fatalf("an unknown earlier severity floors at critical, changed code is rated afresh: %v", got)
	}
}
