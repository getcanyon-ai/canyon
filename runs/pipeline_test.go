package runs

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// live seeds a project whose package is a real commit in a real bare repo, sets the gate, and
// lets workflows run (no hold).
func live(t *testing.T, gate GateFunc) (*pgxpool.Pool, context.Context) {
	t.Helper()
	return liveTree(t, gate, map[string]string{"apps/web/src/lib/payments.ts": "const stripe = new Stripe(process.env.STRIPE_SECRET_KEY);", "package.json": "{}"})
}

// liveTree is live over the package tree given, path to contents.
func liveTree(t *testing.T, gate GateFunc, tree map[string]string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	p, ctx := pgtestSeeded(t)
	pipe.Repos = t.TempDir()
	var buf bytes.Buffer
	if err := tarInto(&buf, tree); err != nil {
		t.Fatal(err)
	}
	pkgRow, err := ingest.Ingest(ctx, filepath.Join(pipe.Repos, project+".git"), &buf, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE packages SET commit = $1, tree_hash = $2 WHERE id = $3", pkgRow.Commit, pkgRow.TreeHash, pkg); err != nil {
		t.Fatal(err)
	}
	pipe.Gate = gate
	pipe.hold.Store(nil)
	return p, ctx
}

func pgtestSeeded(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	return seeded(t)
}

// why renders a run's failure_reason for a failure message. The column is nullable and %v on a
// *string prints an address, which tells the reader nothing about why the run ended as it did.
func why(reason *string) string {
	if reason == nil {
		return "no reason"
	}
	return *reason
}

func waitEnded(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) (state string, reason *string) {
	t.Helper()
	return waitEndedWithin(t, ctx, p, run, 15*time.Second)
}

// waitEndedWithin is waitEnded for a run whose work outgrows the usual 15 s on a loaded runner: a
// build of thousands of log lines, each its own row under the append lock.
func waitEndedWithin(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string, within time.Duration) (state string, reason *string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var ended *time.Time
		if err := p.QueryRow(ctx, "SELECT state, failure_reason, ended_at FROM runs WHERE id = $1", run).Scan(&state, &reason, &ended); err != nil {
			t.Fatal(err)
		}
		if ended != nil {
			return state, reason
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never ended (state %s)", run, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// failedIn is the step a run Canyon itself failed in, on its row and on its run.state row, which agree,
// or "" for none (contracts/routes.md, A failure reason in words; #1645).
func failedIn(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) string {
	t.Helper()
	var row, said string
	if err := p.QueryRow(ctx, `SELECT COALESCE(failed_in, ''), COALESCE((SELECT data->>'failed_in' FROM timeline_rows WHERE run_id = $1 AND kind = 'run.state' AND data->>'state' = 'failed'), '') FROM runs WHERE id = $1`, run).Scan(&row, &said); err != nil || row != said {
		t.Fatalf("failed_in: the run's %q, its run.state row's %q (%v)", row, said, err)
	}
	return row
}

func kinds(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) []string {
	t.Helper()
	page, err := events.RunTimeline(ctx, p, run, 0, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range page.Rows {
		if r.Kind == events.Progress { // the progress rows are their own story (queue tests, progressOf)
			continue
		}
		k := string(r.Kind)
		var d struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(r.Data, &d)
		if r.Kind == events.RunState {
			k += ":" + d.State
		}
		out = append(out, k)
	}
	return out
}

func TestARunGoesPendingRunningVerdictTerminal(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	if state != "blocked" || reason != nil {
		t.Fatalf("the golden log blocks: %s %s", state, why(reason))
	}
	want := "run.state:pending,run.state:running,producer.sarif,producer.sarif,verdict,run.state:blocked"
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != want {
		t.Fatalf("ledger:\n got %s\nwant %s", got, want)
	}
	page, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	var v struct {
		Blocking, Warnings int
		State              string
	}
	_ = json.Unmarshal(page.Rows[4].Data, &v)
	if v.State != "blocked" || v.Blocking != 1 || v.Warnings != 3 || page.Next != nil {
		t.Fatalf("verdict %+v, next %v: the golden log has one blocking and three advisory findings and the ledger is closed", v, page.Next)
	}
	if s := settled(t, run.ID); s != dbos.WorkflowStatusSuccess {
		t.Fatalf("workflow %v", s)
	}
}

// The verdict is Canyon's own act, so its audit row names Canyon, never the one who started the
// run, and commits with the verdict row: the xmin of both is the one transaction's.
func TestAVerdictIsOnTheAuditTrailAsCanyon(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: events.Actor{Kind: "person", Subject: "dana", Display: "Dana Whitfield"}})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	var kind, subject, display, action string
	var detail map[string]any
	var together bool
	if err := p.QueryRow(ctx, `SELECT actor_kind, actor_subject, actor_display, action, detail,
		xmin = (SELECT xmin FROM timeline_rows WHERE run_id = $1 AND dedupe = 'verdict') FROM audit_events WHERE run_id = $1`, run.ID).Scan(&kind, &subject, &display, &action, &detail, &together); err != nil {
		t.Fatalf("a finished run leaves one Canyon audit row: %v", err)
	}
	if kind != "canyon" || subject != "gate" || display != "Canyon" || action != "verdict" || !together {
		t.Fatalf("the verdict's row: %s %s/%s %s, same transaction %v", kind, subject, display, action, together)
	}
	if got, _ := json.Marshal(detail); string(got) != `{"blocking":1,"package":"`+pkg+`","state":"blocked","warnings":3}` {
		t.Fatalf("the verdict's detail: %s", got)
	}
}

func TestAGateErrorFailsTheRunClosed(t *testing.T) {
	p, ctx := live(t, func(context.Context, string, string, policy.Snapshot, gate.Report) (finding.Log, error) {
		return finding.Log{}, errors.New("the scanner is down")
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	if state != "failed" || reason == nil || *reason != "error" || failedIn(t, ctx, p, run.ID) != "review" {
		t.Fatalf("a gate error fails the run closed with reason error, in the review: %s %s", state, why(reason))
	}
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != "run.state:pending,run.state:running,run.state:failed" {
		t.Fatalf("ledger: %s", got)
	}
}

// A reviewer's model that fails is the model's failure, not Canyon's: failure_reason model. The
// reason is the gate step's checkpoint, so a replay after a crash ends the run the same way.
func TestAModelFailureFailsTheRunAsTheModels(t *testing.T) {
	p, ctx := live(t, func(context.Context, string, string, policy.Snapshot, gate.Report) (finding.Log, error) {
		return finding.Log{}, fmt.Errorf("%w: timeout", gate.ErrModel)
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "model" {
		t.Fatalf("a model failure fails the run with reason model: %s %s", state, why(reason))
	}
}

// canyonv2-1x5f: the API answer stays failure_reason model alone (no body, no key), but the wrapped
// error text — status, type, request id only — must reach the server log, or the next such failure
// is undiagnosable again.
func TestAModelFailureLogsTheWrappedError(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	p, ctx := live(t, func(context.Context, string, string, policy.Snapshot, gate.Report) (finding.Log, error) {
		return finding.Log{}, fmt.Errorf("%w: anthropic: status 429 rate_limit_error (request req_test987)", gate.ErrModel)
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "model" {
		t.Fatalf("a model failure still fails the run with reason model: %s %s", state, why(reason))
	}
	logged := buf.String()
	if !strings.Contains(logged, run.ID) || !strings.Contains(logged, project) || !strings.Contains(logged, "status 429 rate_limit_error (request req_test987)") {
		t.Fatalf("the server log must carry the run, the project and the wrapped error's text, got: %s", logged)
	}
}

// The real gate over the fixture app, gitleaks on, writes one producer row per producer — the key is
// gitleaks', and blocks — and with no model on this host the reviewer's row carries the skipped
// notification the timeline shows (D10). Before them, the agent's check.started then check.completed,
// every row the schema's; a second write under the same dedupe keys, as a replayed gate step
// makes, adds nothing (canyonv2-yw5.13).
func TestTheRealGateLeavesTheSkippedReviewOnTheLedger(t *testing.T) {
	app := os.DirFS("../gate/testdata/apps/claims-portal")
	tree := map[string]string{}
	if err := fs.WalkDir(app, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(app, name)
		tree[name] = string(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, ctx := liveTree(t, Gate{}.Review, tree)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek")) // the attach seals under the project's report key
	if err != nil {
		t.Fatal(err)
	}
	pipe.Vault = keys
	snap := testPolicy()
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.gitleaks", Name: "Findings from gitleaks", State: policy.Advisory, Producer: policy.Sarif})
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
		t.Fatalf("the fixture app's leaked key blocks: %s %s", state, why(reason))
	}
	want := "run.state:pending,run.state:running,check.started,check.completed,producer.sarif,producer.sarif,producer.sarif,verdict,run.state:blocked"
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); got != want {
		t.Fatalf("ledger:\n got %s\nwant %s", got, want)
	}
	var skipped string
	if err := p.QueryRow(ctx, `SELECT data->'sarif'->'runs'->0->'invocations'->0->'toolExecutionNotifications'->0->'message'->>'text'
		FROM timeline_rows WHERE run_id = $1 AND dedupe = 'sarif:canyon-review'`, run.ID).Scan(&skipped); err != nil || skipped != gate.Skipped {
		t.Fatalf("the reviewer's producer row says it was skipped: %q %v", skipped, err)
	}
	raw, err := os.ReadFile("../contracts/schema/timeline-row.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	c := jsonschema.NewCompiler()
	if err := cmp.Or(err, c.AddResource("row.json", doc)); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("row.json")
	if err != nil {
		t.Fatal(err)
	}
	page, err := events.RunTimeline(ctx, p, run.ID, 0, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	var checks []string
	for _, r := range page.Rows {
		if !strings.HasPrefix(string(r.Kind), "check.") {
			continue
		}
		var d struct {
			Check, Status string
			Findings      *int
			DurationMS    *int64 `json:"duration_ms"`
		}
		wire, _ := json.Marshal(r)
		row, err := jsonschema.UnmarshalJSON(bytes.NewReader(wire))
		if err := cmp.Or(err, schema.Validate(row), json.Unmarshal(r.Data, &d)); err != nil {
			t.Fatalf("row %s: %v", wire, err)
		}
		if r.Kind == events.CheckCompleted {
			if d.Findings == nil || d.DurationMS == nil {
				t.Fatalf("a completed check carries its count and time: %s", r.Data)
			}
			d.Check += " " + d.Status
		}
		checks = append(checks, string(r.Kind)+" "+d.Check)
	}
	if want := "check.started canyon-review,check.completed canyon-review skipped"; strings.Join(checks, ",") != want {
		t.Fatalf("check rows:\n got %s\nwant %s", strings.Join(checks, ","), want)
	}
	report := pipe.checked(Input{Run: run.ID, Project: project})
	if err := cmp.Or(report(ctx, gate.Check{Name: "canyon-review"}), report(ctx, gate.Check{Name: "canyon-review", Status: "skipped"})); err != nil {
		t.Fatalf("a replayed check finds its rows, ended run or not: %v", err)
	}
	if again, _ := events.RunTimeline(ctx, p, run.ID, 0, 100, true); len(again.Rows) != len(page.Rows) {
		t.Fatalf("a replay wrote %d rows", len(again.Rows)-len(page.Rows))
	}
}

func TestALogTheProfileRefusesFailsTheRunClosed(t *testing.T) {
	p, ctx := live(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		l.Runs[2].Results[0].Properties.Canyon.RequiredEndState = "" // a blocking finding with no required_end_state: the profile refuses it, and derivation repairs nothing here
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("an invalid log never becomes a verdict: %s %s", state, why(reason))
	}
}

func TestACleanLogEndsClean(t *testing.T) {
	p, ctx := live(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		for i := range l.Runs {
			kept := l.Runs[i].Results[:0]
			for _, r := range l.Runs[i].Results {
				if !*r.Properties.Canyon.Blocks {
					kept = append(kept, r)
				}
			}
			l.Runs[i].Results = kept
		}
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("no blocking finding: clean, got %s", state)
	}
}

func TestReplayedStepsWriteNothingTwice(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	before := kinds(t, ctx, p, run.ID)
	in := Input{Run: run.ID, Project: project, Package: pkg}
	log, _ := runstest.Golden(ctx, "", "", policy.Snapshot{}, nil)
	for name, step := range map[string]func() error{
		"running": func() error { return pipe.running(ctx, in) },
		"verdict": func() error {
			_, err := pipe.verdict(ctx, in, log, "blocked", 1, 2)
			return err
		},
		"terminal": func() error { return pipe.End(ctx, in, "blocked", "", "") },
	} {
		if err := step(); err != nil {
			t.Fatalf("%s replayed after the run ended: %v", name, err)
		}
	}
	if after := kinds(t, ctx, p, run.ID); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("a replayed step appended:\n before %v\n after  %v", before, after)
	}
	var audits int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE run_id = $1", run.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("a replayed verdict audits once: %d rows (%v)", audits, err)
	}
}

func TestTheWorkflowSkipsARunThatIsNoLongerLive(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	held := make(chan struct{})
	pipe.hold.Store(&held)
	first, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg}) // supersedes first
	if err != nil {
		t.Fatal(err)
	}
	close(held)
	pipe.hold.Store(nil)
	if state, _ := waitEnded(t, ctx, p, second.ID); state != "blocked" {
		t.Fatalf("the live run completes: %s", state)
	}
	time.Sleep(300 * time.Millisecond) // give a not-cancelled first workflow every chance to misbehave
	if got := strings.Join(kinds(t, ctx, p, first.ID), ","); got != "run.state:pending,run.state:failed" {
		t.Fatalf("the superseded run's workflow must write nothing: %s", got)
	}
}

func tarInto(w *bytes.Buffer, entries map[string]string) error {
	tw := tar.NewWriter(w)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// A shutdown during the gate is not a verdict: the run stays running for the next launch to
// resume, and no terminal row is written. (A CancelWorkflow only lands at a step boundary, so
// the shutdown is the case that interrupts a step.)
func TestAShutdownDuringTheGateWritesNoVerdict(t *testing.T) {
	inGate := make(chan struct{}, 1)
	p, ctx := live(t, runstest.Golden)
	schema := fmt.Sprintf("dbos_shut_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	other, err := dbos.NewContext(ctx, dbos.Config{AppName: "canyon-runs-test", DatabaseURL: os.Getenv("CANYON_TEST_DATABASE_URL"), DatabaseSchema: schema, ApplicationVersion: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	own := &Pipeline{Pool: p, Repos: pipe.Repos, Gate: func(c context.Context, _, _ string, _ policy.Snapshot, _ gate.Report) (finding.Log, error) {
		inGate <- struct{}{}
		<-c.Done()
		return finding.Log{}, c.Err()
	}}
	if err := own.Register(other); err != nil {
		t.Fatal(err)
	}
	if err := dbos.Launch(other); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, other, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	<-inGate
	if err := dbos.Shutdown(other, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var state string
	var ended *time.Time
	if err := p.QueryRow(ctx, "SELECT state, ended_at FROM runs WHERE id = $1", run.ID).Scan(&state, &ended); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kinds(t, ctx, p, run.ID), ","); state != "running" || ended != nil || got != "run.state:pending,run.state:running" {
		t.Fatalf("an interrupted gate must leave the run running with no terminal row: state %s ended %v ledger %s", state, ended, got)
	}
}

// The gate's log is the step's checkpoint, journaled by DBOS in its own tables: a log the
// profile refuses — here one that still carries a credential — must be refused inside the
// step, so the journal holds the refusal and never the log.
func TestARefusedLogIsNeverJournaled(t *testing.T) {
	literal := "sk_live_" + strings.Repeat("Q", 24) // built at runtime: no key-shaped token in the source
	p, ctx := live(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		l.Runs[2].Results[0].Message.Text = "the key " + literal + " is in the bundle"
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a log with a literal fails the run closed: %s %s", state, why(reason))
	}
	if records, hits := journaled(t, ctx, p, run.ID, literal); records == 0 || hits != 0 {
		t.Fatalf("%d step records journaled, the literal in %d of them", records, hits)
	}
}

// journaled counts the run's step records in the DBOS journal and how many carry literal in
// their output or error (outputs are base64 JSON).
func journaled(t *testing.T, ctx context.Context, p *pgxpool.Pool, run, literal string) (records, hits int) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE convert_from(decode(coalesce(output, ''), 'base64'), 'UTF8') LIKE '%' || $2 || '%' OR coalesce(error, '') LIKE '%' || $2 || '%') FROM "+durableSchema+".operation_outputs WHERE workflow_uuid = $1", run, literal).Scan(&records, &hits); err != nil {
		t.Fatal(err)
	}
	return records, hits
}

func TestALogOnlyTheSchemasRefuseFailsTheRunClosed(t *testing.T) {
	p, ctx := live(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		l.Runs[2].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI = "" // finding.Validate accepts this; the profile schema does not
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("both halves of the profile run before a log becomes a verdict: %s %s", state, why(reason))
	}
}

func TestAGateThatPanicsFailsTheRunClosed(t *testing.T) {
	p, ctx := live(t, func(context.Context, string, string, policy.Snapshot, gate.Report) (finding.Log, error) {
		panic("nil map in the reviewer")
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a panicking gate must not leave the run running forever: %s %s", state, why(reason))
	}
}

func TestAProjectIdThatIsNotOneNeverNamesADirectory(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	_ = p
	_, err := pipe.gate(ctx, Input{Run: "gr_x", Project: "../etc", Package: pkg}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a project id") {
		t.Fatalf("the project id names a directory, so it is checked before anything is built from it: %v", err)
	}
}

// The guard is the workflow's first action: a run that is no longer live (the cancel after a
// supersede did not land) gets its workflow anyway, and that workflow must write nothing.
func TestTheGuardWritesNothingForARunThatIsNotLive(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, failure_reason, ended_at) VALUES ('gr_dead0000', $1, $2, 'failed', 'interrupted', now())", project, pkg); err != nil {
		t.Fatal(err)
	}
	h, err := dbos.Enqueue[any](durable, Queue, Workflow, Input{Run: "gr_dead0000", Project: project, Package: pkg}, dbos.WithEnqueueWorkflowID("gr_dead0000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetResult(); err != nil {
		t.Fatalf("the workflow returns clean: %v", err)
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = 'gr_dead0000'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a workflow for a run that is not live wrote %d row(s) (%v)", n, err)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = 'gr_dead0000'").Scan(&state); err != nil || state != "failed" {
		t.Fatalf("the run's state was touched: %s (%v)", state, err)
	}
}

func TestRunningIsANoOpOnARunThatIsNotPending(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	if err := pipe.running(ctx, Input{Run: run.ID, Project: project, Package: pkg}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", run.ID).Scan(&state); err != nil || state != "blocked" {
		t.Fatalf("a replayed running step must not pull an ended run back to running: %s (%v)", state, err)
	}
}

func TestALogNamingAProducerTwiceFailsTheRunClosed(t *testing.T) {
	p, ctx := live(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		l.Runs[1].Tool.Driver.Name = l.Runs[0].Tool.Driver.Name // the ledger holds one row per producer; a second would be lost silently
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("%s %v", state, reason)
	}
}

// A producer that writes blocks false on a critical finding whose rule blocks is corrected, not
// refused: the server derives blocks from the snapshot's state and the finding's severity, so
// the run ends blocked with that finding counted, and a deploy run builds nothing.
func TestTheServerDerivesBlocksFromTheSnapshotNotTheProducer(t *testing.T) {
	p, ctx, f := deploying(t, func(ctx context.Context, dir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
		l, _ := runstest.Golden(ctx, dir, tree, snap, report)
		l.Runs[2].Results[0].Properties.Canyon.Blocks = new(bool) // the critical secrets.client-bundle finding, under a blocking rule, claimed not to block
		return l, nil
	})
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
		t.Fatalf("a lying producer is corrected, not refused: blocked, got %s %v after %d builds", state, reason, f.Builds.Load())
	}
	var v struct {
		Blocking, Warnings int
	}
	if err := p.QueryRow(ctx, "SELECT data FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run.ID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v.Blocking != 1 || v.Warnings != 3 {
		t.Fatalf("the verdict counts the derived value: %+v", v)
	}
	if n := f.Builds.Load(); n != 0 {
		t.Fatalf("a blocked deploy run builds nothing: %d builds", n)
	}
}

// A blocking rule's finding at high severity blocks, the same as critical: the wiring's own proof
// of the threshold, since the golden's one blocking finding is critical. auth.unauthenticated-route
// set blocking, the reviewer's high routes block.
func TestAHighSeverityBlockingFindingBlocksThroughThePipeline(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	snap, err := policy.WithStates(testPolicy(), map[string]policy.State{"auth.unauthenticated-route": policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
		t.Fatalf("a blocking rule at high severity blocks: got %s %v after %d builds", state, reason, f.Builds.Load())
	}
}

// counts is where an undrived log would be read; it refuses one rather than counting an absent
// blocks as a warning.
func TestCountsRefusesAnAbsentBlocks(t *testing.T) {
	l, err := runstest.Golden(context.Background(), "", "", policy.Snapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if blocking, warnings, err := counts(l); err != nil || blocking != 1 || warnings != 3 {
		t.Fatalf("the golden log: %d blocking, %d warnings (%v)", blocking, warnings, err)
	}
	l.Runs[2].Results[0].Properties.Canyon.Blocks = nil
	if _, _, err := counts(l); err == nil || !strings.Contains(err.Error(), "blocks is missing") {
		t.Fatalf("an absent blocks is an error, never a warning: %v", err)
	}
}

// The verdict counts entries, not results (contracts/finding.md, One problem, one entry; #2013): two
// opengrep rules on one line under one Canyon rule are one blocking finding, as the CLI says.
func TestCountsCountEntriesNotResults(t *testing.T) {
	yes := true
	on := func(rule string) finding.Result {
		r := finding.Result{RuleID: rule, Locations: []finding.Location{{PhysicalLocation: finding.PhysicalLocation{ArtifactLocation: finding.ArtifactLocation{URI: "server.js"}, Region: finding.Region{StartLine: 10}}}}}
		r.Properties.Canyon.Blocks = &yes
		return r
	}
	l := finding.Log{Runs: []finding.Run{{Tool: finding.Tool{Driver: finding.Driver{Name: "sarif/opengrep"}}, Results: []finding.Result{on("sql.injection.opengrep.tainted-sql"), on("sql.injection.opengrep.express-sqli")}}}}
	if blocking, warnings, err := counts(l); err != nil || blocking != 1 || warnings != 0 {
		t.Fatalf("one rule on one line: %d blocking, %d warnings (%v)", blocking, warnings, err)
	}
}

// A run enqueued before the release that journals the policy snapshot is recovered by dbos.Launch
// with an Input carrying no policy. The gate refuses it by name — the journaled step error tells
// whoever reads it to start the run again — instead of erroring inside Derive against a zero
// snapshot, which says only that a log could not be derived.
func TestARunEnqueuedBeforeThisReleaseIsRefusedByName(t *testing.T) {
	live(t, runstest.Golden)
	_, err := pipe.gate(t.Context(), Input{Run: "gr_upgrade", Project: project, Package: pkg}, nil)
	if err == nil || !strings.Contains(err.Error(), "start it again") {
		t.Fatalf("a run from before the policy snapshot was journaled: %v", err)
	}
}

// A policy applied while a run is in flight does not reach that run. The verdict derives against
// the snapshot the run started with — journaled in the run's Input, so a replay reads back the
// same one — never against a snapshot recomposed from the policy edits table when the workflow
// resumes. The run is held before its first write, the edit lands while it waits, and the verdict
// still counts the one blocking finding the golden log had at start.
func TestAPolicyAppliedMidRunDoesNotReachThatRun(t *testing.T) {
	p, ctx := live(t, runstest.Golden)
	hold := make(chan struct{})
	pipe.hold.Store(&hold)
	t.Cleanup(func() { pipe.hold.Store(nil) })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg})
	if err != nil {
		t.Fatal(err)
	}
	// the apply, as PUT /policies/auth.unauthenticated-route?apply=true writes it: advisory in the
	// run's snapshot, blocking from now on, so a recomposed snapshot would block the golden log's
	// second finding too.
	if _, err := p.Exec(ctx, `INSERT INTO policy_edits (rule, state, version, effective_at, set_by_kind, set_by_subject, set_by_display)
		VALUES ('auth.unauthenticated-route', 'blocking', 1, now(), 'person', 'dana@meridian.example', 'Dana Whitfield')`); err != nil {
		t.Fatal(err)
	}
	close(hold)
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
		t.Fatalf("the golden log blocks: %s %s", state, why(reason))
	}
	var blocking int
	if err := p.QueryRow(ctx, "SELECT (data->>'blocking')::int FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run.ID).Scan(&blocking); err != nil {
		t.Fatal(err)
	}
	if blocking != 1 {
		t.Fatalf("%d blocking finding(s), want the 1 the run started with: the verdict derived against the policy applied mid-run", blocking)
	}
}
