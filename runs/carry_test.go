package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// paymentsLine is the code the first review blocks on, payments.ts's one line in live's tree.
const paymentsLine = "const stripe = new Stripe(process.env.STRIPE_SECRET_KEY);"

// once is a host's model that reports a high perf.unbounded-refetch on payments.ts at line on its first call, and
// nothing after: a model that leaves out, in the next review, a finding it made.
type once struct {
	calls atomic.Int32
	line  int
}

func (m *once) Review(context.Context, gate.Request) (gate.Response, error) {
	if m.calls.Add(1) > 1 {
		return gate.Response{}, nil
	}
	return gate.Response{Results: []gate.Candidate{{Rule: "perf.unbounded-refetch", File: "apps/web/src/lib/payments.ts", StartLine: m.line, EndLine: m.line,
		Severity: "high", Title: "Stripe is built on each import", Problem: "A client per module load.", Fix: "Build it once."}}}, nil
}

// retree makes the package the next run reviews the tree given, path to contents: a change to the app.
func retree(t *testing.T, ctx context.Context, p *pgxpool.Pool, tree map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	if err := tarInto(&buf, tree); err != nil {
		t.Fatal(err)
	}
	row, err := ingest.Ingest(ctx, filepath.Join(pipe.Repos, project+".git"), &buf, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE packages SET commit = $1, tree_hash = $2 WHERE id = $3", row.Commit, row.TreeHash, pkg); err != nil {
		t.Fatal(err)
	}
}

// reviewedRun is reviewed with the run's id, started by a person, as an approval needs a run's author it can
// tell from its approver (CoversBlocking).
func reviewedRun(t *testing.T, ctx context.Context, p *pgxpool.Pool, snap policy.Snapshot) (id, state string, reason *string, review finding.Run) {
	t.Helper()
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	state, reason = waitEnded(t, ctx, p, run.ID)
	_ = p.QueryRow(ctx, "SELECT data->'sarif'->'runs'->0 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'sarif:canyon-review'", run.ID).Scan(&review)
	return run.ID, state, reason, review
}

// firstBlocks is the first review, which blocks on the model's one finding: its run's id and that finding.
func firstBlocks(t *testing.T, ctx context.Context, p *pgxpool.Pool, snap policy.Snapshot) (string, finding.Result) {
	t.Helper()
	id, state, reason, r := reviewedRun(t, ctx, p, snap)
	if state != "blocked" || len(r.Results) != 1 || r.Results[0].Properties.Canyon.CarriedFrom != "" {
		t.Fatalf("the first review blocks on its own finding: %s %s %+v", state, why(reason), r.Results)
	}
	return id, r.Results[0]
}

// (i) A blocking finding the next review leaves out, its code unchanged and the app changed only elsewhere, still
// blocks: carried, naming the review that found it, its fingerprint the same, and the run says so (#2900).
func TestALeftOutFindingOnUnchangedCodeStillBlocks(t *testing.T) {
	p, ctx := live(t, nil)
	m := &once{line: 1}
	pipe.Gate = Gate{Model: m, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	first, found := firstBlocks(t, ctx, p, snap)
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": paymentsLine, "package.json": `{"name": "claims-portal"}`})
	_, state, reason, r := reviewedRun(t, ctx, p, snap)
	if m.calls.Load() != 2 || state != "blocked" || len(r.Results) != 1 {
		t.Fatalf("the left-out finding on unchanged code blocks: %d calls, %s %s, %+v", m.calls.Load(), state, why(reason), r.Results)
	}
	got := r.Results[0]
	if got.Properties.Canyon.CarriedFrom != first || got.PartialFingerprints != found.PartialFingerprints || got.Locations[0].PhysicalLocation.Region != found.Locations[0].PhysicalLocation.Region {
		t.Fatalf("carried from %q as it was: %+v, want from %s %+v", got.Properties.Canyon.CarriedFrom, got, first, found)
	}
	if !slices.Contains(notes(r), gate.CarriedOne) {
		t.Fatalf("the run says it carried one: %v", notes(r))
	}
}

// (ii) The same, but its code changed: the finding is not carried, and the review passes.
func TestALeftOutFindingOnChangedCodeIsNotCarried(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Model: &once{line: 1}, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	firstBlocks(t, ctx, p, snap)
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": "export const stripe = getStripe();", "package.json": "{}"})
	if _, state, reason, r := reviewedRun(t, ctx, p, snap); state != "clean" || len(r.Results) != 0 || slices.Contains(notes(r), gate.CarriedOne) {
		t.Fatalf("changed code carries nothing: %s %s, %+v %v", state, why(reason), r.Results, notes(r))
	}
}

// (iii) Code that only moved in the file still stands: lines added above it, the finding is carried where the code
// now is, its fingerprint the same.
func TestALeftOutFindingOnMovedCodeIsCarriedWhereItIs(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Model: &once{line: 1}, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	_, found := firstBlocks(t, ctx, p, snap)
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": "import Stripe from \"stripe\";\n\n" + paymentsLine, "package.json": "{}"})
	_, state, reason, r := reviewedRun(t, ctx, p, snap)
	if state != "blocked" || len(r.Results) != 1 {
		t.Fatalf("moved code still blocks: %s %s, %+v", state, why(reason), r.Results)
	}
	g := r.Results[0].Locations[0].PhysicalLocation.Region
	if g.StartLine != 3 || g.EndLine != 3 || g.Snippet != found.Locations[0].PhysicalLocation.Region.Snippet || r.Results[0].PartialFingerprints != found.PartialFingerprints {
		t.Fatalf("carried at line 3, the same fingerprint: %+v, want %+v", r.Results[0], found)
	}
}

// (iv) An approval of the finding covers it carried: nothing it carries is left unapproved.
func TestAnApprovedFindingCarriedIsCovered(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Model: &once{line: 1}, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	_, found := firstBlocks(t, ctx, p, snap)
	if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		VALUES ($1, $2, $3, 'built once at the edge', now() + interval '30 days', 'person', 'dana@meridian.example', 'Dana Whitfield', true)`,
		project, found.RuleID, found.PartialFingerprints.CanyonV1); err != nil {
		t.Fatal(err)
	}
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": paymentsLine, "package.json": `{"name": "claims-portal"}`})
	id, _, _, r := reviewedRun(t, ctx, p, snap)
	var raw json.RawMessage
	if err := p.QueryRow(ctx, "SELECT data->'findings' FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if approved, unapproved, err := Approvals(ctx, p, project, id, raw); err != nil || len(r.Results) != 1 || len(approved) != 1 || unapproved != 0 {
		t.Fatalf("the approval covers the carried finding: %+v approved, %d not (%v), %+v", approved, unapproved, err, r.Results)
	}
}

// (v) A rule that no longer blocks carries nothing: the rule made advisory, the review passes.
func TestALeftOutFindingWhoseRuleNoLongerBlocksIsNotCarried(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Model: &once{line: 1}, ModelID: "claude-opus-5", Pool: p}.Review
	firstBlocks(t, ctx, p, blockingRefetch(t))
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": paymentsLine, "package.json": `{"name": "claims-portal"}`})
	if _, state, reason, r := reviewedRun(t, ctx, p, testPolicy()); state != "clean" || len(r.Results) != 0 {
		t.Fatalf("an advisory rule carries nothing: %s %s, %+v", state, why(reason), r.Results)
	}
}

// (vi) Fail closed: a file that tells nothing — no longer valid UTF-8 — carries the finding at its own lines.
func TestALeftOutFindingInAFileThatTellsNothingIsCarried(t *testing.T) {
	p, ctx := live(t, nil)
	pipe.Gate = Gate{Model: &once{line: 1}, ModelID: "claude-opus-5", Pool: p}.Review
	snap := blockingRefetch(t)
	first, found := firstBlocks(t, ctx, p, snap)
	retree(t, ctx, p, map[string]string{"apps/web/src/lib/payments.ts": paymentsLine + "\n// \xff", "package.json": "{}"})
	_, state, reason, r := reviewedRun(t, ctx, p, snap)
	if state != "blocked" || len(r.Results) != 1 || r.Results[0].Properties.Canyon.CarriedFrom != first ||
		r.Results[0].Locations[0].PhysicalLocation.Region != found.Locations[0].PhysicalLocation.Region {
		t.Fatalf("a file that tells nothing carries it at its own lines: %s %s, %+v", state, why(reason), r.Results)
	}
}
