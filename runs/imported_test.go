package runs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// flagged is the claims-portal file the contract's imported golden result flags.
const flagged = "apps/web/src/app/api/claims/route.ts"

// importing is a live pipeline over flagged, reviewed by the clean golden log, with a vault for the
// project's report key, and a snapshot that adds semgrep at state, snyk advisory and trivy off. It
// answers the snapshot and the golden semgrep result.
func importing(t *testing.T, state policy.State) (*pgxpool.Pool, context.Context, policy.Snapshot, finding.Result) {
	t.Helper()
	src, err := os.ReadFile("../gate/testdata/apps/claims-portal/" + flagged)
	if err != nil {
		t.Fatal(err)
	}
	p, ctx := liveTree(t, runstest.CleanGolden, map[string]string{flagged: string(src)})
	if pipe.Vault, err = vault.Load(filepath.Join(t.TempDir(), "kek")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../contracts/testdata/finding/imported-semgrep.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden finding.Log
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	snap := testPolicy()
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.semgrep", Name: "Findings from semgrep", State: state, Producer: policy.Sarif},
		policy.Rule{ID: "sarif.snyk", Name: "Findings from snyk", State: policy.Advisory, Producer: policy.Sarif},
		policy.Rule{ID: "sarif.trivy", Name: "Findings from trivy", State: policy.Off, Producer: policy.Sarif})
	return p, ctx, snap, golden.Runs[0].Results[0]
}

// attach stores a report of tool under category as by attached it, with dropped results dropped, and
// makes the project's report key when it has none, as the route's attach does.
func attach(t *testing.T, ctx context.Context, p *pgxpool.Pool, tool, category, by string, dropped int, results ...finding.Result) {
	t.Helper()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rep := sarif.Report{Tool: tool, Category: category, Version: "1.86.0", Results: append([]finding.Result{}, results...), Dropped: dropped}
	if dropped > 0 {
		rep.FirstDropped = "result 3: kind is pass"
	}
	if err := events.LockTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportKey(ctx, tx, pipe.Vault, project); err != nil { // as the attach route does
		t.Fatal(err)
	}
	if _, _, err := AttachReportTx(ctx, tx, project, pkg, Report{Report: rep, Digest: "sha256:" + strings.Repeat("a", 64), AttachedBy: events.Actor{Kind: "agent", Subject: by, Display: by}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// verdictOf is the run's verdict log as stored.
func verdictOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) finding.Log {
	t.Helper()
	var l finding.Log
	if err := p.QueryRow(ctx, "SELECT data->'findings' FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run).Scan(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

// A run includes the reports attached before it started, read by the ids it journaled: one run per
// (tool, category) with its lines, snyk's no-report run, nothing of trivy, which is off, and nothing of
// an attach after the start. One tool's identical result under two categories is two fingerprints, each
// FingerprintImported's under the project's key; each report is one producer.sarif row of the schema's,
// snyk none; and the gate step replayed later, with another report current, answers the same log.
func TestARunReadsTheReportsAttachedBeforeItStarted(t *testing.T) {
	p, ctx, snap, result := importing(t, policy.Advisory)
	attach(t, ctx, p, "semgrep", "", "ci-bot", 0, result)
	attach(t, ctx, p, "semgrep", "js", "ci-bot", 2, result)
	attach(t, ctx, p, "trivy", "", "ci-bot", 0, result)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	attach(t, ctx, p, "semgrep", "", "dana", 0) // after the start: the next run's
	close(held)
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("advisory findings leave the run clean: %s %s", state, why(reason))
	}
	verdict := verdictOf(t, ctx, p, run.ID)
	var got []string
	for _, r := range verdict.Runs[2:] {
		line := producer(r) + ": " + strconv.Itoa(len(r.Results))
		for _, n := range r.Invocations[0].ToolExecutionNotifications {
			line += " | " + n.Level + " " + n.Message.Text
		}
		got = append(got, line)
	}
	if want := []string{"sarif/semgrep: 1 | note semgrep · report attached by ci-bot",
		"sarif/semgrep js: 1 | note semgrep · report attached by ci-bot | warning semgrep · dropped 2 result(s) at attach",
		"sarif/snyk: 0 | warning snyk · no report for this package"}; !slices.Equal(got, want) {
		t.Fatalf("imported runs:\n got %q\nwant %q", got, want)
	}

	tree, err := gate.Walk("../gate/testdata/apps/claims-portal") // flagged as the package holds it
	if err != nil {
		t.Fatal(err)
	}
	key, err := openReportKey(ctx, p, pipe.Vault, project)
	if err != nil {
		t.Fatal(err)
	}
	want := verdictOf(t, ctx, p, run.ID)
	if err := gate.FingerprintImported(&want, tree, key); err != nil {
		t.Fatal(err)
	}
	a, b := verdict.Runs[2].Results[0].PartialFingerprints, verdict.Runs[3].Results[0].PartialFingerprints
	if a == b || a != want.Runs[2].Results[0].PartialFingerprints || b != want.Runs[3].Results[0].PartialFingerprints {
		t.Fatalf("fingerprints %s and %s, want two distinct, FingerprintImported's under the key", a, b)
	}

	schema, err := jsonschema.NewCompiler().Compile("../contracts/schema/timeline-row.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	page, err := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range page.Rows {
		wire, _ := json.Marshal(r)
		row, err := jsonschema.UnmarshalJSON(bytes.NewReader(wire))
		if err := cmp.Or(err, schema.Validate(row)); err != nil {
			t.Fatalf("row %s: %v", wire, err)
		}
		var d struct {
			Producer, Category string
			AttachedBy         string `json:"attached_by"`
			FirstDropped       string `json:"first_dropped"`
		}
		if r.Kind == events.ProducerSARIF && json.Unmarshal(r.Data, &d) == nil && strings.HasPrefix(d.Producer, "sarif/") {
			rows = append(rows, strings.Join([]string{d.Producer, d.Category, d.AttachedBy, d.FirstDropped}, " "))
		}
	}
	if want := []string{"sarif/semgrep  ci-bot ", "sarif/semgrep js ci-bot result 3: kind is pass"}; !slices.Equal(rows, want) {
		t.Fatalf("imported producer.sarif rows:\n got %q\nwant %q", rows, want)
	}

	ws, err := durable.ListWorkflows(durable, dbos.WithFilterWorkflowIDs(run.ID), dbos.WithFilterLoadInput(true))
	if err != nil || len(ws) != 1 {
		t.Fatalf("the run's workflow: %d, %v", len(ws), err)
	}
	var in Input
	if raw, ok := ws[0].Input.(string); !ok || json.Unmarshal([]byte(raw), &in) != nil {
		t.Fatalf("the journaled input: %T", ws[0].Input)
	}
	again, err := pipe.gate(ctx, in, nil)
	if err != nil || mustJSON(t, again) != mustJSON(t, verdict) {
		t.Fatalf("the replayed gate step: %v\n got %s\nwant %s", err, mustJSON(t, again), mustJSON(t, verdict))
	}
}

// An imported finding blocks as any finding does, state == blocking && severity ≥ high: under a blocking
// semgrep a medium result is a warning, and a high one is one blocking finding, carrying the
// required_end_state and do_not the attach fills in (finding.md, the import table).
func TestAnImportedFindingBlocksPerTheRuleState(t *testing.T) {
	p, ctx, snap, result := importing(t, policy.Blocking)
	for severity, want := range map[finding.Severity]string{finding.Medium: "blocking 0, warnings 4: <nil>",
		finding.High: "blocking 1, warnings 3: <nil>"} {
		result.Properties.Canyon.Severity = severity
		result.Level, _ = severity.Level()
		attach(t, ctx, p, "semgrep", "", "ci-bot", 0, result)
		ids, err := CurrentReports(ctx, p, pkg, importedTools(snap))
		if err != nil {
			t.Fatal(err)
		}
		log, err := pipe.gate(ctx, Input{Run: "gr_gate", Project: project, Package: pkg, Policy: snap, Reports: ids}, nil)
		blocking, warnings, _ := counts(log)
		if got := fmt.Sprintf("blocking %d, warnings %d: %v", blocking, warnings, err); got != want {
			t.Errorf("a %s result under a blocking rule: %s, want %s", severity, got, want)
		}
	}
}

// The gate opens the project's report key and never makes one: an attach does. A report with no key
// beside it fails the step closed, and the step writes no key.
func TestTheGateOpensTheReportKeyAndNeverMakesOne(t *testing.T) {
	p, ctx, snap, result := importing(t, policy.Advisory)
	attach(t, ctx, p, "semgrep", "", "ci-bot", 0, result)
	if _, err := p.Exec(ctx, "UPDATE projects SET report_key_ref = NULL, report_sealed_key = NULL, report_sealed_value = NULL"); err != nil {
		t.Fatal(err)
	}
	ids, err := CurrentReports(ctx, p, pkg, importedTools(snap))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipe.gate(ctx, Input{Run: "gr_gate", Project: project, Package: pkg, Policy: snap, Reports: ids}, nil)
	var written bool
	if p.QueryRow(ctx, "SELECT report_sealed_value IS NOT NULL FROM projects").Scan(&written) != nil || written || err == nil || !strings.Contains(err.Error(), "has no report key") {
		t.Fatalf("a report with no key: %v, a key written %v", err, written)
	}
}

// The actor's attaches of the hour count toward its gate-run limit (contracts/routes.md, the gate-runs
// row): 60 attaches and the start is refused as the actor's, while another actor starts.
func TestAttachesCountTowardTheActorsRunLimit(t *testing.T) {
	p, ctx := seeded(t)
	if _, err := p.Exec(ctx, `INSERT INTO package_reports (project_id, package_id, tool, category, digest, version, results, dropped, attached_by_kind, attached_by_subject, attached_by_display)
		SELECT $1, $2, 'semgrep', '', 'sha256:' || repeat('a', 64), '1', '[]', 0, 'person', 'dana', 'Dana' FROM generate_series(1, 60)`, project, pkg); err != nil {
		t.Fatal(err)
	}
	var limit *RateLimited
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: events.Actor{Kind: "person", Subject: "dana", Display: "Dana"}}); !errors.As(err, &limit) || !limit.PerActor || limit.Limit != 60 {
		t.Fatalf("dana's start after 60 attaches: %v", err)
	}
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: events.Actor{Kind: "person", Subject: "sam", Display: "Sam"}}); err != nil {
		t.Fatalf("another actor's start: %v", err)
	}
}
