package runs

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// counting is a host's model that counts its calls: it answers one finding on the live tree's
// payments.ts, or fails when fail is set, or runs out of room when room is.
type counting struct {
	calls atomic.Int32
	fail  bool
	room  bool // the answer runs out of room
}

func (c *counting) Review(context.Context, gate.Request) (gate.Response, error) {
	c.calls.Add(1)
	if c.fail {
		return gate.Response{}, errors.New("overloaded")
	}
	if c.room {
		return gate.Response{}, gate.ErrOutOfRoom
	}
	return gate.Response{Results: []gate.Candidate{{Rule: "perf.unbounded-refetch", File: "apps/web/src/lib/payments.ts", StartLine: 1, EndLine: 1,
		Severity: "medium", Title: "Stripe is built on each import", Problem: "A client per module load.", Fix: "Build it once."}}}, nil
}

// reviewed runs the live package once under snap and answers the run's end and its reviewer row.
func reviewed(t *testing.T, ctx context.Context, p *pgxpool.Pool, snap policy.Snapshot) (state string, reason *string, review finding.Run) {
	t.Helper()
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	state, reason = waitEnded(t, ctx, p, run.ID)
	_ = p.QueryRow(ctx, "SELECT data->'sarif'->'runs'->0 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'sarif:canyon-review'", run.ID).Scan(&review)
	return state, reason, review
}

func notes(r finding.Run) []string {
	var out []string
	for _, inv := range r.Invocations {
		for _, n := range inv.ToolExecutionNotifications {
			out = append(out, n.Message.Text)
		}
	}
	return out
}

func cached(t *testing.T, ctx context.Context, p *pgxpool.Pool) (n int) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT count(*) FROM review_cache").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The same tree, snapshot, model and agent reviewed again replays the first review, no model call,
// and says it was reused; a changed snapshot is a new review.
func TestAnUnchangedReviewIsReusedAndAChangedSnapshotIsNot(t *testing.T) {
	p, ctx := live(t, nil)
	m := &counting{}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	_, _, first := reviewed(t, ctx, p, testPolicy())
	state, reason, again := reviewed(t, ctx, p, testPolicy())
	if n := m.calls.Load(); n != 1 || reason != nil || len(again.Results) != 1 || again.Results[0].RuleID != "perf.unbounded-refetch" ||
		again.Results[0].PartialFingerprints != first.Results[0].PartialFingerprints || !slices.Equal(notes(again), []string{gate.SecondOpinion, gate.Reused}) || !slices.Equal(notes(first), []string{gate.SecondOpinion}) {
		t.Fatalf("an unchanged review is replayed with no model call: %d calls, %s %s, first %v, again %v %+v", n, state, why(reason), notes(first), notes(again), again.Results)
	}
	changed, err := policy.WithStates(testPolicy(), map[string]policy.State{"perf.unbounded-refetch": policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, r := reviewed(t, ctx, p, changed); m.calls.Load() != 2 || !slices.Equal(notes(r), []string{gate.SecondOpinion}) {
		t.Fatalf("a changed snapshot calls the model again: %d calls, notes %v", m.calls.Load(), notes(r))
	}
}

// A model that failed leaves nothing to reuse: the next run calls it again.
func TestAFailedReviewIsNotCached(t *testing.T) {
	p, ctx := live(t, nil)
	m := &counting{fail: true}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	for range 2 {
		if state, reason, _ := reviewed(t, ctx, p, testPolicy()); state != "failed" || reason == nil || *reason != "model" {
			t.Fatalf("the model's failure fails the run: %s %s", state, why(reason))
		}
	}
	if n := m.calls.Load(); n != 2 || cached(t, ctx, p) != 0 {
		t.Fatalf("a failed review is never cached: %d calls, %d cached", n, cached(t, ctx, p))
	}
}

// A host with no model skips the review, and a skip is never cached.
func TestASkippedReviewIsNotCached(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Pool: p}.Review
	if _, _, r := reviewed(t, ctx, p, testPolicy()); len(notes(r)) != 1 || notes(r)[0] != gate.Skipped || cached(t, ctx, p) != 0 {
		t.Fatalf("no model: skipped, nothing cached: %v, %d cached", notes(r), cached(t, ctx, p))
	}
}

// A model that ran out of room read nothing, as on a host with no model: the check ends skipped, so a
// rule placed blocking before production holds the promotion (the receipt), the run is never failed,
// and nothing is cached, so the next run calls the model again (#1500).
func TestAReviewOutOfRoomIsSkippedAndNotCached(t *testing.T) {
	p, ctx := live(t, nil)
	m := &counting{room: true}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	for range 2 {
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: testPolicy()})
		if err != nil {
			t.Fatal(err)
		}
		var status string
		if state, reason := waitEnded(t, ctx, p, run.ID); state == "failed" || p.QueryRow(ctx, `SELECT data->>'status' FROM timeline_rows
			WHERE run_id = $1 AND kind = 'check.completed' AND data->>'check' = 'canyon-review'`, run.ID).Scan(&status) != nil || status != "skipped" {
			t.Fatalf("out of room: %s %s, the check %q", state, why(reason), status)
		}
	}
	if n := m.calls.Load(); n != 2 || cached(t, ctx, p) != 0 {
		t.Fatalf("a review out of room is never cached: %d calls, %d cached", n, cached(t, ctx, p))
	}
}

// An AI review that read nothing never passes a rule of it blocking before preview: the run is blocked
// on one gate.unreadable finding that says why, with no model or out of room; advisory, it goes on (#1555).
func TestAReviewThatReadNothingNeverPassesABlockingRule(t *testing.T) {
	p, ctx := live(t, nil)
	blocking, err := policy.WithStates(testPolicy(), map[string]policy.State{"perf.unbounded-refetch": policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		gate    Gate
		snap    policy.Snapshot
		state   string
		problem string // the gate.unreadable finding's, none when empty
	}{
		{"no model, blocking", Gate{Pool: p}, blocking, "blocked", "could not check: the AI review: no model is configured on this Canyon host"},
		{"out of room, blocking", Gate{Model: &counting{room: true}, ModelID: "claude-opus-5", Pool: p}, blocking, "blocked", "could not check: the AI review: the model ran out of room for its answer"},
		{"no model, advisory", Gate{Pool: p}, testPolicy(), "clean", ""},
	} {
		pipe.Gate = c.gate.Review
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: c.snap})
		if err != nil {
			t.Fatal(err)
		}
		state, reason := waitEnded(t, ctx, p, run.ID)
		var problems []string
		rows, err := p.Query(ctx, `SELECT f->'properties'->'canyon'->>'problem' FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.ruleId == "gate.unreadable")') f
			WHERE t.run_id = $1 AND t.kind = 'verdict'`, run.ID)
		if err == nil {
			problems, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			t.Fatal(err)
		}
		if state != c.state || reason != nil || !slices.Equal(problems, slices.DeleteFunc([]string{c.problem}, func(s string) bool { return s == "" })) {
			t.Errorf("%s: %s %s, gate.unreadable %q", c.name, state, why(reason), problems)
		}
	}
}

// An admin set perf.unbounded-refetch to blocking (canyonv2-iic1): the reviewer's candidate at high,
// carrying its governance prose, is a blocking finding and the run ends blocked on it — never failed.
func TestABlockingAIFindingBlocksTheRun(t *testing.T) {
	p, ctx := live(t, nil)
	c := gate.Candidate{Rule: "perf.unbounded-refetch", File: "apps/web/src/lib/payments.ts", StartLine: 1, EndLine: 1, Severity: "high",
		Title: "Stripe is built on each import", Problem: "A client per module load.", Fix: "Build it once.",
		RequiredEndState: "One Stripe client serves the process.", DoNot: []string{"build a client per request"}}
	pipe.Gate = Gate{Model: &answering{resp: gate.Response{Results: []gate.Candidate{c}}}, ModelID: "claude-opus-5", Pool: p}.Review
	snap, err := policy.WithStates(testPolicy(), map[string]policy.State{c.Rule: policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	state, reason, review := reviewed(t, ctx, p, snap)
	if state != "blocked" || reason != nil || len(review.Results) != 1 || review.Results[0].Properties.Canyon.Blocks == nil || !*review.Results[0].Properties.Canyon.Blocks {
		t.Fatalf("a blocking AI finding blocks the run: %s %s, %+v", state, why(reason), review.Results)
	}
}

// A file too large for any call, or not valid UTF-8, is the tree's own outcome: the review is cached, and
// replayed it still reads as one that left a file unread — its check skipped, its findings kept (#2274, #2280).
func TestAReviewThatLeftAFileUnreadIsCachedAndStillUnread(t *testing.T) {
	for _, c := range []struct{ note, tree, body string }{
		{gate.TooLargeForACall, "tree-too-large-for-a-call", strings.Repeat("x", 3<<20)},
		{gate.NotUTF8, "tree-not-utf-8", "// caf\xe9\n"},
	} {
		t.Run(c.tree, func(t *testing.T) {
			p, ctx := live(t, nil)
			dir := t.TempDir()
			for name, body := range map[string]string{"apps/web/src/lib/payments.ts": "export const stripe = new Stripe();\n", "apps/web/src/generated.ts": c.body} {
				if err := cmp.Or(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700), os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)); err != nil {
					t.Fatal(err)
				}
			}
			m := &counting{}
			g := Gate{Model: m, ModelID: "claude-opus-5", Pool: p}
			for i := range 2 {
				var done gate.Check
				report := func(_ context.Context, c gate.Check) error { done = c; return nil }
				log, err := g.agent(ctx, dir, c.tree, testPolicy(), "canyon-review", finding.Log{}, report)
				if err != nil {
					t.Fatal(err)
				}
				run := log.Runs[len(log.Runs)-1]
				if done.Status != "skipped" || len(run.Results) != 1 || gate.Unread(run) != c.note || (i == 1) != slices.Contains(notes(run), gate.Reused) {
					t.Fatalf("review %d: the check %q, %d result(s), unread %q, notes %q", i, done.Status, len(run.Results), gate.Unread(run), notes(run))
				}
			}
			if n := m.calls.Load(); n != 1 || cached(t, ctx, p) != 1 {
				t.Fatalf("cached once: %d call(s), %d cached", n, cached(t, ctx, p))
			}
		})
	}
}
