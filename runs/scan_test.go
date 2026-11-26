package runs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/opengrep"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// trivySARIF is a trivy answer: one high vulnerability, file-level at package.json, as trivy places one.
const trivySARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Trivy","version":"0.74.0","rules":[{"id":"CVE-2026-0001","properties":{"security-severity":"7.5"}}]}},
"results":[{"ruleId":"CVE-2026-0001","level":"error","message":{"text":"Package: left-pad. Installed Version: 1.0.0. Fixed Version: 1.0.1."},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"package.json"}}}]}]}]}`

// opengrepSARIF is an opengrep 1.30.0 answer, its driver named "Opengrep OSS": one XSS in the package.
const opengrepSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Opengrep OSS","rules":[{"id":"canyon.xss-raw-html","defaultConfiguration":{"level":"error"}}]}},"results":[{"ruleId":"canyon.xss-raw-html",
"message":{"text":"Request data is rendered as raw HTML."},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"apps/web/src/lib/payments.ts"},"region":{"startLine":1,"startColumn":1,"endLine":1,"endColumn":6}}}]}]}]}`

// scanning is deploying with the server's attach, the fake answering trivySARIF, and a snapshot adding
// trivy, blocking, run on with settings, and grype advisory, run off.
func scanning(t *testing.T) (*pgxpool.Pool, context.Context, *executortest.Fake, policy.Snapshot) {
	t.Helper()
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.SARIF = trivySARIF
	snap := testPolicy()
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.trivy", Name: "Findings from trivy", State: policy.Blocking, Producer: policy.Sarif, Run: true,
		Settings: policy.Settings{Scanners: []string{"vuln"}, SkipPaths: []string{"docs"}}},
		policy.Rule{ID: "sarif.grype", Name: "Findings from grype", State: policy.Advisory, Producer: policy.Sarif})
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	return p, ctx, f, snap
}

// attachAs is the server's attach as a run's scans reach it (cmd/canyon-server, attachReport): the
// report normalised as the tool scanned against snap and the package's tree, and stored
// under the ledger lock beside the project's report key.
func attachAs(t *testing.T, snap policy.Snapshot) func(context.Context, string, string, string, []byte, []string, []string, Evidence, events.Actor) (int64, error) {
	return func(ctx context.Context, project, pkg, tool string, raw []byte, unloaded, unparsed []string, ev Evidence, by events.Actor) (int64, error) {
		var commit string
		if err := pipe.Pool.QueryRow(ctx, "SELECT commit FROM packages WHERE id = $1", pkg).Scan(&commit); err != nil {
			return 0, err
		}
		dir := t.TempDir()
		if err := ingest.Extract(ctx, filepath.Join(pipe.Repos, project+".git"), commit, dir); err != nil {
			return 0, err
		}
		tree, err := gate.Walk(dir)
		if err != nil {
			return 0, err
		}
		rep, err := sarif.NormalizeAs(raw, &snap, tree, tool)
		if err != nil {
			return 0, err
		}
		rep.Unparsed = slices.Compact(slices.Sorted(slices.Values(append(rep.Unparsed, unparsed...))))
		tx, err := pipe.Pool.Begin(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := events.LockTx(ctx, tx); err != nil {
			return 0, err
		}
		if _, err := ReportKey(ctx, tx, pipe.Vault, project); err != nil {
			return 0, err
		}
		id, _, err := AttachReportTx(ctx, tx, project, pkg, Report{Report: rep, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), AttachedBy: by, Unloaded: unloaded, Unparsed: rep.Unparsed, Evidence: ev})
		if err != nil {
			return 0, err
		}
		return id, tx.Commit(ctx)
	}
}

// importedLines is each imported run of the verdict: its producer, its result count and its lines.
func importedLines(log finding.Log) []string {
	var lines []string
	for _, r := range log.Runs {
		if p, _ := policy.ByDriver(r.Tool.Driver.Name); p == policy.Sarif {
			line := producer(r) + ": " + strconv.Itoa(len(r.Results))
			for _, n := range r.Invocations[0].ToolExecutionNotifications {
				line += " | " + n.Level + " " + n.Message.Text
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// A run asks the executor to scan with each tool whose rule has run on, sending the tool's settings, and
// attaches the answer as Canyon: it replaces the report uploaded before the start, its finding blocks as
// any does, and the verdict's line for the tool reads attached by Canyon.
func TestARunScansWithEachToolWithRunOnAndAttachesAsCanyon(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	attach(t, ctx, p, "trivy", "", "ci-bot", 0) // uploaded before the start: the run's own scan replaces it
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || reason != nil {
		t.Fatalf("trivy's high finding under a blocking rule: %s %s", state, why(reason))
	}
	want := executor.ScanRequest{App: project, Package: pkg, Tool: "trivy", Settings: &executor.ScanSettings{Scanners: []string{"vuln"}, SkipPaths: []string{"docs"}}}
	if got, rules := f.Scanned(); f.Scans.Load() != 1 || !reflect.DeepEqual(got, want) || len(rules) != 0 {
		t.Fatalf("%d scan(s), the last %+v %+v, rules %v", f.Scans.Load(), got, got.Settings, rules)
	}
	verdict := verdictOf(t, ctx, p, run.ID)
	if got, want := importedLines(verdict), []string{"sarif/trivy: 1 | note trivy · report attached by Canyon",
		"sarif/grype: 0 | warning grype · no report for this package"}; !slices.Equal(got, want) {
		t.Fatalf("imported runs:\n got %q\nwant %q", got, want)
	}
	var by events.Actor
	var image, built, uploaded *string // the scan's evidence: the image that ran and its database's date (#1114); an upload has neither
	if err := p.QueryRow(ctx, `SELECT attached_by_kind, attached_by_subject, attached_by_display, image, to_char(db_built_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		(SELECT coalesce(image, '') || coalesce(db_built_at::text, '') FROM package_reports ORDER BY id LIMIT 1) FROM package_reports ORDER BY id DESC LIMIT 1`).Scan(&by.Kind, &by.Subject, &by.Display, &image, &built, &uploaded); err != nil || by != scanner ||
		image == nil || *image != "ghcr.io/aquasecurity/trivy@"+executortest.Digest || built == nil || *built != executortest.DBBuiltAt || uploaded == nil || *uploaded != "" {
		t.Fatalf("the scan's report attached by %+v, image %v, db built %v; the upload's %v (%v)", by, image, built, uploaded, err)
	}
}

// While sarif.gitleaks is not Off a run runs gitleaks over the package in process, no executor asked,
// and attaches its report as Canyon's: a key in the tree is a secrets.client-bundle finding, critical,
// so it blocks; no byte of the stored report is the key.
func TestGitleaksAttached(t *testing.T) {
	key := "AKIA" + "QJ5K7MZ2RT4WX6YB"
	p, ctx := liveTree(t, runstest.CleanGolden, map[string]string{"config.py": "AWS_ACCESS_KEY_ID = \"" + key + "\"\n", "package.json": "{}"})
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
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("a key in the tree: %s %s", state, why(reason))
	}
	verdict := verdictOf(t, ctx, p, run.ID)
	if got := importedLines(verdict); !slices.Equal(got, []string{"sarif/gitleaks: 1 | note gitleaks · report attached by Canyon"}) {
		t.Fatalf("imported runs: %q", got)
	}
	var stored string
	if err := p.QueryRow(ctx, "SELECT results::text FROM package_reports WHERE tool = 'gitleaks'").Scan(&stored); err != nil || !strings.Contains(stored, "secrets.client-bundle.gitleaks.aws-access-token") || strings.Contains(stored, key) {
		t.Fatalf("the stored report (%v): %s", err, stored)
	}
}

// notChecked is each gate.unreadable finding of the verdict: its run, its problem and whether it blocks.
func notChecked(log finding.Log) []string {
	var out []string
	for _, r := range log.Runs {
		for _, res := range r.Results {
			if c := res.Properties.Canyon; res.RuleID == finding.Unreadable.ID {
				out = append(out, fmt.Sprintf("%s: %s (blocks %v)", r.Tool.Driver.Name, c.Problem, *c.Blocks))
			}
		}
	}
	return out
}

// notFinishedIs runs trivy under set and wants the scan that did not finish to be one blocking
// gate.unreadable finding in the scanners' run, never a clean line, its problem as the
// contract words it with reason (contracts/finding.md, Canyon-run tools): the run blocks, nothing attached.
func notFinishedIs(t *testing.T, set func(*executortest.Fake), reason string) {
	t.Helper()
	p, ctx, f, snap := scanning(t)
	set(f)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEnded(t, ctx, p, run.ID); state != "blocked" || failure != nil {
		t.Fatalf("the run ended %s %s, want blocked", state, why(failure))
	}
	var stored int
	verdict := verdictOf(t, ctx, p, run.ID)
	got, want := notChecked(verdict), []string{"canyon/scanners: could not check: trivy: " + reason + " (blocks true)"}
	if err := p.QueryRow(ctx, "SELECT count(*) FROM package_reports").Scan(&stored); err != nil || stored != 0 || !slices.Equal(got, want) {
		t.Fatalf("%d stored (%v):\n got %q\nwant %q", stored, err, got, want)
	}
	if line := "sarif/trivy: 0 | warning trivy · could not check: " + reason; !slices.Contains(importedLines(verdict), line) { // in place of no report (#1658)
		t.Fatalf("trivy's run: %q, want %q", importedLines(verdict), line)
	}
}

func TestANotFinishedAnswerIsOneBlockingFinding(t *testing.T) {
	notFinishedIs(t, func(f *executortest.Fake) { f.NotFinished = "db_missing" }, "its vulnerability database could not be downloaded")
}

// Opengrep that could not load every rules file it was handed is one blocking finding, never clean.
func TestARulesErrorIsOneBlockingFinding(t *testing.T) {
	notFinishedIs(t, func(f *executortest.Fake) { f.NotFinished = "rules_error" }, "could not load all of its rules")
}

// A busy executor is waited out inside the scan's slot, never a finding however long it lasts (contracts/
// finding.md, Canyon-run tools; #1652): the scan runs once it answers, and its progress says it waited.
func TestABusyExecutorIsWaitedOutNeverAFinding(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.Busy.Store(4)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEndedWithin(t, ctx, p, run.ID, 30*time.Second); state == "failed" {
		t.Fatalf("the run ended %s %s", state, why(failure))
	}
	if got := notChecked(verdictOf(t, ctx, p, run.ID)); len(got) != 0 {
		t.Fatalf("a wait became a finding: %q", got)
	}
	if got := progressOf(t, ctx, p, run.ID, "trivy"); !slices.Contains(got, "queued") || got[len(got)-1] != "done" {
		t.Fatalf("trivy's progress: %q", got)
	}
}

// The laptop demo leaves trivy's run off (#237): a laptop host's run writes its scan skipped, with laptop_demo, once,
// asks the executor for no scan, and names deps.known-vuln not checked on this host, never clean; another host writes
// no skipped row, only that line.
func TestALaptopRunSaysTrivySkippedAndItsRuleNotChecked(t *testing.T) {
	for _, laptop := range []bool{true, false} {
		p, ctx, f, snap := scanning(t)
		i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif.trivy" })
		snap.Rules[i].Run, snap.Rules[i].State = false, policy.Advisory
		pipe.Laptop = laptop
		t.Cleanup(func() { pipe.Laptop = false })
		before := f.Scans.Load()
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
		if err != nil {
			t.Fatal(err)
		}
		if state, failure := waitEnded(t, ctx, p, run.ID); state == "failed" {
			t.Fatalf("laptop %v: the run ended %s %s", laptop, state, why(failure))
		}
		var reasons []string
		rows, err := p.Query(ctx, "SELECT data->>'reason' FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'tool' = 'trivy' AND data->>'status' = 'skipped'", run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r string
			must(t, rows.Scan(&r))
			reasons = append(reasons, r)
		}
		rows.Close()
		if want := map[bool][]string{true: {"laptop_demo"}, false: nil}[laptop]; !slices.Equal(reasons, want) || f.Scans.Load() != before {
			t.Fatalf("laptop %v: skipped rows %q, want %q; %d scans asked", laptop, reasons, want, f.Scans.Load()-before)
		}
		said := false
		for _, r := range verdictOf(t, ctx, p, run.ID).Runs {
			for _, inv := range r.Invocations {
				for _, n := range inv.ToolExecutionNotifications {
					said = said || strings.Contains(n.Message.Text, "not checked on this host: ") && strings.Contains(n.Message.Text, "deps.known-vuln")
				}
			}
		}
		if !said {
			t.Fatalf("laptop %v: deps.known-vuln was not named not checked on this host", laptop)
		}
	}
}

// progressOf is a scan's progress statuses, oldest first (contracts/events.md, progress).
func progressOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, run, tool string) []string {
	t.Helper()
	rows, err := p.Query(ctx, "SELECT data->>'status' FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'tool' = $2 ORDER BY id", run, tool)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A scan the executor answers in a way the contract does not name is Canyon's own failure, and the run
// names the step: scanning the package (contracts/routes.md, A failure reason in words; #1645).
func TestAScanCanyonFailsInEndsTheRunNamingTheScan(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.NotFinished = "not_a_reason"
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || failedIn(t, ctx, p, run.ID) != "scan" {
		t.Fatalf("a scan Canyon failed in: %s %s", state, why(reason))
	}
}

// A scan with no answer in its step's time did not finish, as a tool past its own deadline: one blocking
// finding, never the run failing as Canyon's own error (contracts/finding.md; #1643).
func TestAScanWithNoAnswerInTimeIsOneBlockingFinding(t *testing.T) {
	was := scanCeiling
	scanCeiling = time.Second
	t.Cleanup(func() { scanCeiling = was })
	notFinishedIs(t, func(f *executortest.Fake) { f.Stall.Store(true) }, "did not finish in time")
}

// A scan the cluster had no room to run is capacity, never a policy block nor Canyon failing: the run
// ends failed scan_no_room before the review, with no verdict and nothing to approve, and the scheduler's
// own words are the tool's build.log row (contracts/finding.md, Canyon-run tools; the 2026-10-08 QA
// rerun: as a blocking could-not-check it offered an approval of a review that never ran).
func TestAScanTheClusterHadNoRoomForFailsTheRun(t *testing.T) {
	const said = "0/4 nodes are available: 4 Insufficient cpu."
	p, ctx, f, snap := scanning(t)
	f.NotFinished, f.Reason = "no_room", "0/4 nodes are available:\n  4 Insufficient cpu.\u202e\x1b" // quoted cleaned: no line break, bidi override or escape
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEnded(t, ctx, p, run.ID); state != "failed" || failure == nil || *failure != "scan_no_room" {
		t.Fatalf("the run ended %s %s, want failed scan_no_room", state, why(failure))
	}
	var verdicts int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'", run.ID).Scan(&verdicts); err != nil || verdicts != 0 {
		t.Fatalf("%d verdict rows (%v), want none: nothing was reviewed", verdicts, err)
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' ORDER BY id DESC LIMIT 1", run.ID).Scan(&line); err != nil ||
		line != "trivy: the cluster had no room to run it ("+said+")" {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
}

// A cancel asked while a scan the cluster has no room for is in flight is honoured first: the run ends
// interrupted, never scan_no_room (contracts/routes.md, the cancel row).
func TestACancelBeforeANoRoomScanEndsInterrupted(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.NotFinished, f.Reason, f.ScanFor = "no_room", "0/4 nodes are available: 4 Insufficient cpu.", 2*time.Second
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); f.Scans.Load() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no scan was asked")
		}
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", run.ID); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("a cancel during a no-room scan: %s %s", state, why(reason))
	}
}

func TestSARIFTheAttachRefusesIsOneBlockingFinding(t *testing.T) {
	notFinishedIs(t, func(f *executortest.Fake) { f.SARIF = `{"version":"2.0.0","runs":[]}` }, "the report is not SARIF 2.1.0 JSON")
}

// A scan that did not finish attaches nothing, so the package keeps the report it had: the verdict
// reads the older trivy report's result beside the blocking could-not-check, and that run says first
// that this run's trivy could not check, so no screen reads it as this run's clean scan (#1658).
func TestAScanThatDidNotFinishKeepsTheReportThePackageHad(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	if _, err := pipe.Attach(ctx, project, pkg, "trivy", []byte(trivySARIF), nil, nil, Evidence{}, events.Actor{Kind: "agent", Subject: "ci-bot", Display: "ci-bot"}); err != nil {
		t.Fatal(err)
	}
	f.NotFinished = "timeout"
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEnded(t, ctx, p, run.ID); state != "blocked" || failure != nil {
		t.Fatalf("the run ended %s %s, want blocked", state, why(failure))
	}
	verdict := verdictOf(t, ctx, p, run.ID)
	if got, unchecked := importedLines(verdict), notChecked(verdict); len(got) == 0 || got[0] != "sarif/trivy: 1 | warning trivy · could not check: did not finish in time | note trivy · report attached by ci-bot" ||
		!slices.Equal(unchecked, []string{"canyon/scanners: could not check: trivy: did not finish in time (blocks true)"}) {
		t.Fatalf("the verdict:\n imported %q\n could not check %q", got, unchecked)
	}
}

// With opengrep's run on, its scan carries the vendored sast-rules as the rules part, and its report,
// whose driver opengrep 1.30.0 names "Opengrep OSS", is opengrep's.
func TestOpengrepScansWithTheVendoredRules(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.SARIF = opengrepSARIF
	snap.Rules = append(slices.Clone(snap.Rules[:len(snap.Rules)-2]), policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true,
		Settings: policy.Settings{SkipPaths: []string{"docs"}}})
	pipe.Attach = attachAs(t, snap)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEnded(t, ctx, p, run.ID); state != "clean" || failure != nil {
		t.Fatalf("an advisory opengrep finding: %s %s", state, why(failure))
	}
	asked, rules := f.Scanned()
	if got := importedLines(verdictOf(t, ctx, p, run.ID)); asked.Tool != "opengrep" || !slices.Equal(asked.Settings.SkipPaths, []string{"docs"}) ||
		!maps.Equal(rules, shipped(t)) || !slices.Equal(got, []string{"sarif/opengrep: 1 | note opengrep · report attached by Canyon"}) {
		t.Fatalf("asked %+v with rules %q; imported %q", asked, slices.Collect(maps.Keys(rules)), got)
	}
}

// Opengrep, whose rules read no Ruby, says it had no file of a Ruby app to read, whatever its report
// held: an empty run is no clean line (contracts/finding.md; #1742).
func TestAToolWithNoFileToReadSaysSo(t *testing.T) {
	p, ctx := liveTree(t, runstest.CleanGolden, map[string]string{"app/models/user.rb": "class User\nend\n", "Gemfile": "source \"https://rubygems.org\"\n"})
	f := executortest.New(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	pipe.Executor, pipe.PublicURL, pipe.JWKSURL, pipe.Vault = &executor.Client{Base: f.URL}, "http://localhost:7777", "http://canyon-server:7780/.well-known/jwks.json", keys
	f.SARIF = opengrepSARIF
	snap := testPolicy()
	snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true})
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	got := importedLines(verdictOf(t, ctx, p, run.ID))
	if len(got) != 1 || !strings.HasPrefix(got[0], "sarif/opengrep: 0 | note opengrep · read no file: no language its rules read is here | note opengrep · report attached by Canyon") {
		t.Fatalf("opengrep's run: %q", got)
	}
}

// shipped is the vendored rule files as the rules part carries them, path to content.
func shipped(t *testing.T) map[string]string {
	t.Helper()
	rules, err := opengrep.Rules()
	if err != nil || len(rules) == 0 {
		t.Fatalf("the bundle has %d rules: %v", len(rules), err)
	}
	out := map[string]string{}
	for name, b := range rules {
		out[name] = string(b)
	}
	return out
}

// A tool whose rule is Off, or has run off, is never scanned.
func TestAToolOffOrWithRunOffIsNotScanned(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	snap.Rules[len(snap.Rules)-2].State = policy.Off // trivy, run on
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || f.Scans.Load() != 0 {
		t.Fatalf("trivy off, grype with run off: %s %s after %d scan(s)", state, why(reason), f.Scans.Load())
	}
}

// A busy executor is asked again once its Retry-After has passed, inside the scan's own task: the wait a
// durable sleep between the two asks, said once as queued, and the one answer attached once.
func TestABusyExecutorIsAskedAgainAfterItsWait(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.Busy.Store(1)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("the scan after the wait: %s %s", state, why(reason))
	}
	steps, err := dbos.GetWorkflowSteps(durable, run.ID+"-scan-trivy")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	var stored int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM package_reports").Scan(&stored); err != nil || stored != 1 || f.Scans.Load() != 2 ||
		!slices.Equal(names, []string{"cancelled", "progress", "scan", "progress", "DBOS.sleep", "cancelled", "scan", "progress"}) {
		t.Fatalf("%d scan(s), %d report(s) stored (%v), steps %q", f.Scans.Load(), stored, err, names)
	}
}

// A tool waiting on its database says so once per run: two busy answers with reason database, then
// SARIF, are one scan.waiting row (contracts/events.md).
func TestAScanWaitingOnItsDatabaseSaysSoOnce(t *testing.T) {
	waiting(t, "database", []string{`{"note": "downloading its vulnerability database — the first scan on this host", "tool": "trivy"}`})
}

// A busy answer with no reason is the shared work limit, and says nothing.
func TestABusyScanWithNoReasonSaysNothing(t *testing.T) {
	waiting(t, "", nil)
}

// waiting runs a scan that answers busy twice with reason, then SARIF, and checks the run's
// scan.waiting rows are want. Two tests, not subtests: pgtest names a test's schema after t.Name(),
// and a subtest's "/" is no schema name.
func waiting(t *testing.T, reason string, want []string) {
	t.Helper()
	p, ctx, f, snap := scanning(t)
	f.Busy.Store(2)
	f.BusyReason = reason
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, r := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("the scan after the waits: %s %s", state, why(r))
	}
	rows, err := p.Query(ctx, "SELECT data::text FROM timeline_rows WHERE run_id = $1 AND kind = 'scan.waiting'", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	said, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(said, want) || f.Scans.Load() != 3 {
		t.Errorf("%d scans, rows %q (%v), want %q", f.Scans.Load(), said, err, want)
	}
}

// A cancel asked during the scans lands before the next step: the scans in flight end, nothing is
// asked again, the gate never runs, and the run ends interrupted. The busy answer waits for the cancel, so the busy
// tool's next ask, a Retry-After later, always finds it: without that, an attach later than the Retry-After let the
// retry ask before the cancel landed, a third scan (#25), which the attach's own delay below makes certain.
func TestACancelDuringTheScansEndsTheRunBeforeTheGate(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	f.Busy.Store(1) // the first ask answers busy, so a run the cancel missed would ask again
	attach, attaching, cancelled := pipe.Attach, make(chan struct{}, 2), make(chan struct{})
	f.HoldBusy.Store(&cancelled)
	pipe.Attach = func(c context.Context, project, pkg, tool string, raw []byte, unloaded, unparsed []string, ev Evidence, by events.Actor) (int64, error) {
		time.Sleep(1500 * time.Millisecond) // the race at its worst (#25): this scan reaches its attach after the busy one's Retry-After of 1 s
		attaching <- struct{}{}
		<-cancelled // the scan is mid-step until the cancel is asked
		return attach(c, project, pkg, tool, raw, unloaded, unparsed, ev, by)
	}
	snap.Rules[len(snap.Rules)-1].Run = true // grype beside trivy: one of the two is busy
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	<-attaching
	if _, err := p.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", run.ID); err != nil {
		t.Fatal(err)
	}
	close(cancelled)
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "interrupted" || f.Scans.Load() != 2 {
		t.Fatalf("a cancel during the scans: %s %s after %d scan(s)", state, why(reason), f.Scans.Load())
	}
}

// bothAttached is the verdict's imported runs, in the snapshot's order, when trivy and grype both
// scanned, the fake answering each.
var bothAttached = []string{"sarif/trivy: 1 | note trivy · report attached by Canyon", "sarif/grype: 1 | note grype · report attached by Canyon"}

// The tools scan at once, each in its own step: a run with trivy and grype has both attaches in flight
// together, and the gate reads both reports.
func TestScanAllParallel(t *testing.T) {
	p, ctx, _, snap := scanning(t)
	snap.Rules[len(snap.Rules)-1].Run = true // grype beside trivy
	var in atomic.Int32
	attach, both, together := pipe.Attach, make(chan struct{}), make(chan bool, 2)
	pipe.Attach = func(c context.Context, project, pkg, tool string, raw []byte, unloaded, unparsed []string, ev Evidence, by events.Actor) (int64, error) {
		if in.Add(1) == 2 {
			close(both)
		}
		select { // each attach waits for the other: one after another, the first waits alone
		case <-both:
			together <- true
		case <-time.After(5 * time.Second):
			together <- false
		}
		return attach(c, project, pkg, tool, raw, unloaded, unparsed, ev, by)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("the run: %s %s", state, why(reason))
	}
	if got := importedLines(verdictOf(t, ctx, p, run.ID)); !slices.Equal(got, bothAttached) {
		t.Fatalf("imported runs: %q", got)
	}
	for range 2 {
		if !<-together {
			t.Fatal("the two scans attached one after the other")
		}
	}
	steps, err := dbos.GetWorkflowSteps(durable, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	if len(names) < 4 || !slices.Equal(names[:4], []string{"running", "cancelled", "scan", "scan"}) { // both queued before either is waited on
		t.Fatalf("steps %q", names)
	}
}

// A promote run ships preview's image and reviews nothing: it never scans, whatever its snapshot runs.
func TestAPromoteRunNeverScans(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	gated, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: testPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, gated.ID)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Policy: snap,
		Promote: &Promotion{Image: "canyon/" + project + "@" + executortest.Digest, GateRun: gated.ID, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || f.Scans.Load() != 0 {
		t.Fatalf("the promotion: %s %s after %d scan(s)", state, why(reason), f.Scans.Load())
	}
}

// A crash during the gate replays the scans from their checkpoints: the recovered run asks the executor
// nothing again and reads the reports the first attempt attached.
func TestARecoveredRunNeverScansAgain(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	snap.Rules[len(snap.Rules)-1].Run = true // grype beside trivy: two steps of one round replay
	schema := fmt.Sprintf("dbos_scan_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	launch := func(g GateFunc) dbos.Context {
		dc, err := dbos.NewContext(ctx, dbos.Config{AppName: "canyon-runs-test", DatabaseURL: os.Getenv("CANYON_TEST_DATABASE_URL"), DatabaseSchema: schema, ApplicationVersion: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&Pipeline{Pool: p, Repos: pipe.Repos, Gate: g, Executor: pipe.Executor, Vault: pipe.Vault, Attach: pipe.Attach}).Register(dc); err != nil {
			t.Fatal(err)
		}
		if err := dbos.Launch(dc); err != nil {
			t.Fatal(err)
		}
		return dc
	}
	inGate := make(chan struct{}, 1)
	crashing := launch(func(c context.Context, _, _ string, _ policy.Snapshot, _ gate.Report) (finding.Log, error) {
		inGate <- struct{}{}
		<-c.Done()
		return finding.Log{}, c.Err()
	})
	run, err := Start(ctx, p, crashing, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	<-inGate
	if err := dbos.Shutdown(crashing, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	recovered := launch(runstest.CleanGolden)
	t.Cleanup(func() { _ = dbos.Shutdown(recovered, 10*time.Second) })
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" || f.Scans.Load() != 2 {
		t.Fatalf("the recovered run: %s %s after %d scan(s)", state, why(reason), f.Scans.Load())
	}
	if got := importedLines(verdictOf(t, ctx, p, run.ID)); !slices.Equal(got, bothAttached) {
		t.Fatalf("imported runs: %q", got)
	}
}

// Each rule the executor's opengrep answer says it could not load, its JSON errors' rule_ids, is named on
// its run — opengrep · not checked: rule <id> could not load — so the verdict is never a clean line for it;
// an id not printable ASCII of at most 128 characters, or holding a live key, reads "a rule", and is never
// stored; twenty are named, in name order, and the rest counted. The SARIF's own Rule parse error
// notification names nothing (contracts/finding.md, In a run).
func TestARuleOpengrepCouldNotLoadIsNamedOnTheRun(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	raw, err := os.ReadFile("../gate/sarif/testdata/opengrep-1.30.0-unloaded.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	f.SARIF = string(raw)
	t.Cleanup(func() { f.Unloaded = nil })
	snap.Rules = append(slices.Clone(snap.Rules[:len(snap.Rules)-2]), policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true})
	pipe.Attach = attachAs(t, snap)
	lines := func() []string {
		t.Helper()
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
		if err != nil {
			t.Fatal(err)
		}
		if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
			t.Fatalf("an advisory tool's run: %s %s", state, why(reason))
		}
		return importedLines(verdictOf(t, ctx, p, run.ID))
	}
	// The answer names no rule: the SARIF's message is never read. Its result is at a file this package does not hold.
	attached := "sarif/opengrep: 0 | note opengrep · report attached by Canyon | warning opengrep · dropped 1 result(s) at attach"
	if got := lines(); !slices.Equal(got, []string{attached}) {
		t.Fatalf("imported runs: %q", got)
	}
	key := "sk_" + "live_" + "51MZq8fQ2xYv7Kp3Lw9Rt4Nb"
	f.Unloaded = []string{"", "bad id", "org-bad-regex", key, "r\u00e8gle", strings.Repeat("x", 129)}
	if got, want := lines(), []string{attached + " | warning opengrep · not checked: a rule could not load | warning opengrep · not checked: rule org-bad-regex could not load"}; !slices.Equal(got, want) {
		t.Fatalf("imported runs:\n got %q\nwant %q", got, want)
	}
	var stored []string
	if err := p.QueryRow(ctx, "SELECT unloaded FROM package_reports ORDER BY id DESC LIMIT 1").Scan(&stored); err != nil || !slices.Equal(stored, []string{"", "org-bad-regex"}) {
		t.Fatalf("stored unloaded %q (%v)", stored, err)
	}
	f.Unloaded = nil
	for i := range 21 {
		f.Unloaded = append(f.Unloaded, fmt.Sprintf("r%02d", i))
	}
	if got := lines(); len(got) != 1 || !strings.HasSuffix(got[0], "rule r19 could not load | warning opengrep · not checked: 1 more rules could not load") || strings.Contains(got[0], "rule r20 ") {
		t.Fatalf("imported runs: %q", got)
	}
}

// Each file the executor's opengrep answer says was not read whole, its JSON errors' paths, is named
// on its run when it is exactly a file of the package — opengrep · not checked: <path> was not read
// whole — so the verdict is never a clean line for it; a path the package does not hold names nothing
// (contracts/finding.md, In a run).
func TestAFileOpengrepDidNotReadWholeIsNamedOnTheRun(t *testing.T) {
	_, _, f, snap := scanning(t)
	tree := map[string]string{"package.json": "{}", "src/a.tsx": "const x = <div>{", "src/odd name/c.js": "const s = 'unterminated;"}
	for i := range 21 {
		tree[fmt.Sprintf("src/f%02d.js", i)] = "f(\n"
	}
	p, ctx := liveTree(t, runstest.CleanGolden, tree)
	f.SARIF = ""
	f.Unparsed = []string{"src", "src/a.tsx", "src/gone.js", "src/odd name/c.js"}
	t.Cleanup(func() { f.Unparsed = nil })
	snap.Rules = append(slices.Clone(snap.Rules[:len(snap.Rules)-2]), policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true})
	pipe.Attach = attachAs(t, snap)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("an advisory tool's run: %s %s", state, why(reason))
	}
	if got, want := importedLines(verdictOf(t, ctx, p, run.ID)), []string{"sarif/opengrep: 0 | note opengrep · report attached by Canyon" +
		" | warning opengrep · not checked: src/a.tsx was not read whole | warning opengrep · not checked: src/odd name/c.js was not read whole"}; !slices.Equal(got, want) {
		t.Fatalf("imported runs:\n got %q\nwant %q", got, want)
	}
	// Twenty are named, in path order, and the rest counted; a path the package does not hold takes no place.
	f.Unparsed = []string{"src/0-gone.js"}
	for i := range 21 {
		f.Unparsed = append(f.Unparsed, fmt.Sprintf("src/f%02d.js", i))
	}
	if run, err = Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap}); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("an advisory tool's run: %s %s", state, why(reason))
	}
	got := importedLines(verdictOf(t, ctx, p, run.ID))
	if len(got) != 1 || !strings.Contains(got[0], "Canyon | warning opengrep · not checked: src/f00.js was not read whole |") ||
		!strings.HasSuffix(got[0], "src/f19.js was not read whole | warning opengrep · not checked: 1 more files were not read whole") || strings.Contains(got[0], "gone") {
		t.Fatalf("imported runs: %q", got)
	}
}

// Each file ESLint's report names in an ESL0999 notification — its own word that it could not parse
// the file — is named on its run when it is exactly a file of the package, eslint · not checked:
// <path> was not read whole, beside the result of the file it did read; twenty in path order, the
// rest counted (contracts/finding.md, In a run).
func TestAFileESLintCouldNotParseIsNamedOnTheRun(t *testing.T) {
	_, _, f, snap := scanning(t)
	tree := map[string]string{"package.json": "{}", "src/broken.ts": "const a = {\n", "src/broken.tsx": "const x = <div>{</div>;\n", "src/fine.ts": "export const ok = 1;\n", "src/flagged.js": "eval(input);\n"}
	for i := range 21 {
		tree[fmt.Sprintf("src/f%02d.ts", i)] = "f(\n"
	}
	p, ctx := liveTree(t, runstest.CleanGolden, tree)
	raw, err := os.ReadFile("../gate/sarif/testdata/eslint-9.39.5-unparsed.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	f.SARIF = string(raw)
	snap.Rules = append(slices.Clone(snap.Rules[:len(snap.Rules)-2]), policy.Rule{ID: "sarif.eslint", Name: "Findings from eslint", State: policy.Advisory, Producer: policy.Sarif, Run: true})
	pipe.Attach = attachAs(t, snap)
	lines := func() []string {
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
		if err != nil {
			t.Fatal(err)
		}
		waitEnded(t, ctx, p, run.ID)
		return importedLines(verdictOf(t, ctx, p, run.ID))
	}
	if got, want := lines(), []string{"sarif/eslint: 1 | note eslint · report attached by Canyon" +
		" | warning eslint · not checked: src/broken.ts was not read whole | warning eslint · not checked: src/broken.tsx was not read whole"}; !slices.Equal(got, want) {
		t.Fatalf("imported runs:\n got %q\nwant %q", got, want)
	}
	notes := []string{`{"descriptor":{"id":"ESL0999"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/0-gone.ts"}}}]}`}
	for i := range 21 {
		notes = append(notes, fmt.Sprintf(`{"descriptor":{"id":"ESL0999"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/f%02d.ts"}}}]}`, i))
	}
	f.SARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"ESLint"}},"artifacts":[{"location":{"uri":"file:///scan/tree/src/fine.ts"}}],"invocations":[{"executionSuccessful":false,"toolConfigurationNotifications":[` + strings.Join(notes, ",") + `]}],"results":[]}]}`
	got := lines()
	if len(got) != 1 || !strings.Contains(got[0], "Canyon | warning eslint · not checked: src/f00.ts was not read whole |") ||
		!strings.HasSuffix(got[0], "src/f19.ts was not read whole | warning eslint · not checked: 1 more files were not read whole") || strings.Contains(got[0], "gone") {
		t.Fatalf("imported runs: %q", got)
	}
}

// The host runs at most its scan capacity at once — 2, Docker's, here, one of them grype's — and a scan
// past it waits its turn: its progress says it is queued, and its wait never counts toward its 15
// minutes, so three scans of 1.5 s each in the one shared slot finish under a ceiling shorter than all
// three end to end, grype's beside them (contracts/finding.md, Canyon-run tools; #1652).
func TestScansQueueAtTheHostsCapacityAndTheWaitNeverCounts(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.ScanFor = 1500 * time.Millisecond
	was := scanCeiling
	scanCeiling = 3 * time.Second // each scan's own 1.5 s and its attach fit, with room for a slow host; the third's 4.5 s of waiting and running would not
	t.Cleanup(func() { scanCeiling = was })
	snap := testPolicy()
	for _, tool := range []string{"checkov", "opengrep", "trivy", "grype"} {
		snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif." + tool, Name: "Findings from " + tool, State: policy.Advisory, Producer: policy.Sarif, Run: true})
	}
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, failure := waitEndedWithin(t, ctx, p, run.ID, 60*time.Second); state != "clean" {
		t.Fatalf("the run ended %s %s", state, why(failure))
	}
	if most := f.MostScans.Load(); most != 2 || f.Scans.Load() != 4 {
		t.Fatalf("%d scans ran at once, %d in all; want 2 at once, 4 in all", most, f.Scans.Load())
	}
	for _, tool := range []string{"checkov", "opengrep", "trivy", "grype"} {
		if got := progressOf(t, ctx, p, run.ID, tool); got[len(got)-1] != "done" {
			t.Fatalf("%s's progress: %q", tool, got)
		}
	}
	var positions int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'status' = 'queued' AND data ? 'position'", run.ID).Scan(&positions); err != nil || positions == 0 {
		t.Fatalf("the waiting scan's place in line was never written: %d (%v)", positions, err)
	}
}

// A cancel takes a run's waiting scans out of the queue at once: the run ends interrupted without waiting
// for their turn, and the scans that waited never run (contracts/finding.md, Canyon-run tools; #1652).
// A place in line read before a scan began never lands over its running.
func TestACancelTakesAWaitingScanOutOfTheQueue(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	f.ScanFor = 3 * time.Second
	snap := testPolicy()
	for _, tool := range []string{"checkov", "opengrep", "trivy"} {
		snap.Rules = append(snap.Rules, policy.Rule{ID: "sarif." + tool, Name: "Findings from " + tool, State: policy.Advisory, Producer: policy.Sarif, Run: true})
	}
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	var running string
	for deadline := time.Now().Add(10 * time.Second); running == ""; time.Sleep(50 * time.Millisecond) { // one running in the one shared slot, two waiting
		if time.Now().After(deadline) {
			t.Fatalf("%d scans started", f.Scans.Load())
		}
		_ = p.QueryRow(ctx, "SELECT data->>'tool' FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'status' = 'running'", run.ID).Scan(&running)
	}
	if err := pipe.queued(ctx, run.ID, "scan", running, 1); err != nil { // the reporter, a read late
		t.Fatal(err)
	}
	if got := progressOf(t, ctx, p, run.ID, running); got[len(got)-1] != "running" {
		t.Fatalf("%s's progress after a late place in line: %q", running, got)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1", run.ID); err != nil { // as the cancel route asks it
		t.Fatal(err)
	}
	if err := Dequeue(durable, run.ID); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEndedWithin(t, ctx, p, run.ID, 20*time.Second); state != "failed" || why(reason) != "interrupted" {
		t.Fatalf("a cancelled run with a waiting scan: %s %s", state, why(reason))
	}
	if n := f.Scans.Load(); n != 1 {
		t.Fatalf("the waiting scans ran after the cancel: %d scans", n)
	}
}
