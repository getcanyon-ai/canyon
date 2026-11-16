package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// goldenRun flattens the seam's own SARIF golden into the run read check renders from, so the
// CLI's golden cannot drift from contracts/testdata: one critical blocker and one advisory.
func goldenRun(t *testing.T) checkRun {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "contracts", "testdata", "finding", "claims-portal.sarif.json"))
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Runs []struct {
			Tool struct {
				Driver struct {
					Name string `json:"name"`
				} `json:"driver"`
			} `json:"tool"`
			Results []finding `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	res := runResult{ID: "gr_8f2a", Package: "pkg_1", State: "blocked"}
	for _, r := range log.Runs {
		for _, f := range r.Results {
			f.Tool = r.Tool.Driver.Name // the run read names each finding's tool (contracts/routes.md)
			res.Findings = append(res.Findings, f)
		}
	}
	for _, f := range res.Findings {
		if blocks, _ := f.Blocking(); blocks {
			res.Blocking++
		} else {
			res.Warnings++
		}
	}
	return checkRun{App: "claims-portal", Host: "http://localhost:7777", Run: "gr_8f2a",
		StartedAt: "2026-09-02T09:31:04Z", Files: 241, Bytes: 1_468_006, Elapsed: 14700 * time.Millisecond, Result: res}
}

// The whole blocked rendering, byte for byte: the verdict, the typography, which finding is a
// blocker, the caret across the redaction mask, the counts, the footer.
func TestTheBlockedCheckRendersItsGolden(t *testing.T) {
	var out strings.Builder
	if code := (&app{out: &out}).verdict(goldenRun(t)); code != exitBlocked {
		t.Errorf("the blocked verdict exited %d, want %d", code, exitBlocked)
	}
	want, err := os.ReadFile("testdata/check-blocked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("canyon check rendered\n%s\ntestdata/check-blocked.txt wants\n%s", out.String(), want)
	}
}

// A rule's message is its author's wrapped text: a line break is a space, never a backslash-n (the
// test cluster audit: opengrep's SQL message read "…can\\nrewrite the statement…").
func TestAProblemsLineBreaksAreItsParagraphs(t *testing.T) {
	run := goldenRun(t)
	run.Result.Findings[0].Properties.Canyon.Problem = "A value reaches the statement, so whoever sends it can\nrewrite the statement.\n\nPass it as a bound parameter."
	var out strings.Builder
	(&app{out: &out}).verdict(run)
	if got := out.String(); strings.Contains(got, `\n`) || !strings.Contains(got, "can rewrite the") || !strings.Contains(got, "statement. Pass it as a bound parameter.\n") {
		t.Fatalf("the problem as printed:\n%s", got)
	}
}

// A row this build cannot render is counted and said out loud, never dropped, and never stops
// the rest of the run rendering (contracts/events.md).
// The fix loop and the history block belong to a run that shipped nothing. A deploy that
// shipped prints the live app and its page instead, and must not be sent round the loop that
// exiting 0 exists to avoid.
func TestTheFooterLeavesAShippedRunAlone(t *testing.T) {
	run := goldenRun(t)
	var out strings.Builder
	(&app{out: &out}).footer(run, 1, 2)
	if !strings.Contains(out.String(), "Open in Canyon") || !strings.Contains(out.String(), "canyon check") {
		t.Errorf("a check's own footer lost a block:\n%s", out.String())
	}
	run.Deploy = &deployed{Env: "preview", URL: "http://claims-portal.preview.localhost:7777",
		Package: "pkg_1", ImageDigest: "sha256:" + strings.Repeat("ab", 32)}
	var shipped strings.Builder
	(&app{out: &shipped, verb: command{name: "deploy"}}).footer(run, 1, 2)
	if strings.Contains(shipped.String(), "Open in Canyon") || strings.Contains(shipped.String(), "canyon check") {
		t.Errorf("a run that deployed was sent round the fix loop:\n%s", shipped.String())
	}
}

func TestARowThisBuildDoesNotUnderstandIsSaidOutLoud(t *testing.T) {
	row := func(id int, kind string) string {
		return fmt.Sprintf(`{"id":%d,"ts":"2026-09-02T09:31:04Z","scope":"run","run":"gr_8f2a",`+
			`"project":"claims-portal","kind":%q,"data":{}}`, id, kind)
	}
	h := &gateHost{run: aRun("clean", 0, 0), page: `{"rows":[` +
		row(41, "run.state") + "," + row(42, "policy.waived") + `],"next":null}`}
	out, _, code := checking(t, h)
	if code != exitOK || !strings.Contains(out, "· 1 row this build does not understand") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// The package line says what went up before the gate speaks; the run ends on its own total.
func TestThePackageAndDoneLinesFrameTheRun(t *testing.T) {
	out, _, _ := checking(t, &gateHost{run: aRun("clean", 0, 0)})
	if !strings.Contains(out, "✓ Package    2 files · 38 B") || !strings.Contains(out, "\n  Done in ") {
		t.Errorf("the package and done lines:\n%s", out)
	}
	for b, want := range map[int64]string{512: "512 B", 2048: "2.0 KB", 1_468_006: "1.4 MB"} {
		if got := size(b); got != want {
			t.Errorf("size(%d) = %q, want %q", b, got, want)
		}
	}
	if got := since(75 * time.Second); got != "1m 15s" {
		t.Errorf("since(75s) = %q", got)
	}
}

// --explain is a Canyon refusal, not Go's flag report, and it names what does answer instead.
func TestExplainIsRefusedInOneCanyonLine(t *testing.T) {
	out, errOut, code := run(t, nil, false, "check", "--explain", "secrets.client-bundle")
	if code != exitError || out != "" || !strings.Contains(errOut, "--explain has no source yet; the why line names the policy") {
		t.Errorf("exit %d, stdout %q, stderr:\n%s", code, out, errOut)
	}
	if strings.Contains(errOut, "flag provided") || strings.Contains(errOut, "Usage of") {
		t.Errorf("Go's own flag report reached the user:\n%s", errOut)
	}
}

// The caret is placed and sized in code points: a byte offset drifts the moment a •••• mask, or
// any other multi-byte rune, lies inside the span.
func TestTheCaretIsPlacedByCodePoint(t *testing.T) {
	line := `const stripe = new Stripe("sk_live_51MZq8f••••7Kx2");`
	before, span := caretSpan(region{StartLine: 14, StartColumn: 27, EndLine: 14, EndColumn: 63}, line)
	if before != 26 || before+span != utf8.RuneCountInString(line) {
		t.Errorf("caret at %d spanning %d, want it to start at 26 and stop at the line's end (%d runes)",
			before, span, utf8.RuneCountInString(line))
	}
	if before, span := caretSpan(region{StartLine: 8, StartColumn: 10, EndLine: 8, EndColumn: 67},
		"  return NextResponse.json(await db.policies.forOrg(params.orgId));"); before != 9 || span != 57 {
		t.Errorf("an unredacted span: caret at %d spanning %d, want 9 and 57", before, span)
	}
}

// Every string the host wrote shares its line with one Canyon wrote, so a newline in it would
// print a line the gate never said — a passing verdict, for instance.
func TestAServerStringCannotForgeALine(t *testing.T) {
	forged := `a.ts\n  ✓ Passed — 0 blocking findings, 0 warnings\n  x`
	h := &gateHost{run: `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"blocked","blocking":1,"warnings":0,"findings":[` +
		`{"ruleId":"secrets.client-bundle","locations":[{"physicalLocation":{"artifactLocation":{"uri":"` + forged + `"},` +
		`"region":{"startLine":1,"startColumn":1,"endLine":1,"endColumn":6,"snippet":{"text":"x = 1"}}}}],` +
		`"properties":{"canyon":{"severity":"critical","blocks":true,"problem":"p\nq","fix":"f\ng","policy_name":"n"}}}]}}`}
	out, _, code := checking(t, h)
	if code != exitBlocked {
		t.Fatalf("exit %d, stdout:\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "✓ Passed") {
			t.Errorf("a uri forged a verdict line: %q", line)
		}
	}
	if !strings.Contains(out, `a.ts\n  ✓ Passed`) || !strings.Contains(out, "  p q\n") || !strings.Contains(out, "f g\n") { // a uri escaped; prose's line break a space
		t.Errorf("the escaped strings are not in the block:\n%s", out)
	}
}

// A passing run counts its warnings as the list's entries, as a blocked one counts its blockers (cli.md
// §3.3): two opengrep rules on one line are one warning on the stage row, the tool's row and the line
// that names what didn't block.
func TestAPassingRunCountsWarningsAsEntries(t *testing.T) {
	twice := func(rule string) string {
		return `{"ruleId":"code.injection.opengrep.` + rule + `","tool":"sarif/opengrep",` + where + `"properties":{"canyon":{"severity":"medium","blocks":false}}}`
	}
	out, _, code := checking(t, &gateHost{run: aRun("clean", 0, 2, twice("a"), twice("b"))})
	if code != exitOK || !strings.Contains(out, "✓ Review passed · 0 blocking, 1 warning\n") ||
		!strings.Contains(out, "! 1 warning didn't block — code.injection · canyon check") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// A warning that blocked nothing is still named, or the next run's blocker is a surprise.
func TestACleanRunNamesTheWarningsThatDidNotBlock(t *testing.T) {
	out, _, code := checking(t, &gateHost{run: aRun("clean", 0, 2, advisory, advisory)})
	if code != exitOK || !strings.Contains(out, "! 2 warnings didn't block — deps.known-vuln, deps.known-vuln · canyon check") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
	if out, _, _ := checking(t, &gateHost{run: aRun("clean", 0, 0)}); strings.Contains(out, "didn't block") {
		t.Errorf("a clean run with nothing to name said it anyway:\n%s", out)
	}
}

// The caret is measured on the line as printed: a tab before the span is two columns of \t once escaped.
func TestTheCaretFollowsTheEscapedLine(t *testing.T) {
	var out bytes.Buffer
	a := &app{out: &out, render: Render{ASCII: true}}
	a.snippet(region{StartLine: 7, EndLine: 7, StartColumn: 2, EndColumn: 4, Snippet: sarifText{Text: "\tab"}}, "")
	if !strings.Contains(out.String(), "|   ^^\n") {
		t.Errorf("the caret drifted:\n%s", out.String())
	}
}

// The check.* rows a gate writes (runs/pipeline.go), on one page that ends the run.
func gatePage(rows ...string) string {
	return `{"rows":[` + strings.Join(rows, ",") + `],"next":null}`
}

func checkStarted(id int, check string) string {
	return fmt.Sprintf(`{"id":%d,"ts":"2026-09-02T09:31:0%dZ","kind":"check.started","data":{"check":%q}}`, id, id%10, check)
}

func checkCompleted(id int, check, status string, findings, ms int) string {
	return fmt.Sprintf(`{"id":%d,"ts":"2026-09-02T09:31:0%dZ","kind":"check.completed",`+
		`"data":{"check":%q,"status":%q,"findings":%d,"duration_ms":%d}}`, id, id%10, check, status, findings, ms)
}

// Checks may finish in any order; the rows do not. auth-surface and the reviewer both complete
// before secrets, and both wait behind it.
func TestAGateRowNeverResolvesBeforeTheRowAboveIt(t *testing.T) {
	h := &gateHost{run: aRun("blocked", 1, 1, blocker, advisory), page: gatePage(
		checkStarted(41, "secrets"), checkStarted(42, "dependencies"), checkStarted(43, "auth-surface"), checkStarted(44, "canyon-review"),
		checkCompleted(45, "auth-surface", "clean", 0, 700),
		checkCompleted(46, "canyon-review", "skipped", 0, 0),
		checkCompleted(47, "secrets", "findings", 1, 900),
		checkCompleted(48, "dependencies", "warning", 1, 1600),
	)}
	out, _, code := checking(t, h)
	want := []string{
		"  ✗ Review blocked · 1 blocking finding",
		"    · secrets            1 finding                                    [0.9s]", // neutral: the row cannot know whether it blocks (C24)
		"    · dependencies       not fully checked · 1 finding                [1.6s]",
		"    ✓ auth-surface       clean                                        [0.7s]",
		"    · AI review          skipped", // a skipped check took no time
	}
	if code != exitBlocked {
		t.Fatalf("exit %d, want %d; stdout:\n%s", code, exitBlocked, out)
	}
	if rows := strings.Index(out, want[0]); rows < 0 || !strings.HasPrefix(out[rows:], strings.Join(want, "\n")+"\n") {
		t.Errorf("the gate rows read\n%s\nwant\n%s", out, strings.Join(want, "\n"))
	}
	for _, line := range want[1:4] {
		if n := utf8.RuneCountInString(line); n != proseWidth {
			t.Errorf("a gate row is %d columns wide, not %d: %q", n, proseWidth, line)
		}
	}
}

// A completion for a check that opened no row is still a row, never dropped in silence.
func TestACompletionNothingStartedStillGetsItsRow(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), page: gatePage(
		checkStarted(41, "secrets"), checkCompleted(42, "secrets", "clean", 0, 800),
		checkCompleted(43, "canyon-review", "clean", 0, 900),
	)}
	out, _, code := checking(t, h)
	if code != exitOK || !strings.Contains(out, "✓ secrets") || !strings.Contains(out, "✓ AI review") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// The custom-rules agent's row reads in plain words too (contracts/finding.md, Custom rules).
func TestTheCustomRulesRowReadsCustomRules(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), page: gatePage(checkStarted(41, "custom-rules"), checkCompleted(42, "custom-rules", "clean", 0, 900))}
	out, _, code := checking(t, h)
	if code != exitOK || !strings.Contains(out, "✓ custom rules") || strings.Contains(out, "custom-rules") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// A failed run has no verdict to print its rows under, and prints them still: what ran is what its
// failure is read against.
func TestAFailedRunStillPrintsItsReviewRows(t *testing.T) {
	h := &gateHost{run: `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"model","blocking":0,"warnings":0,"findings":[]}}`,
		page: gatePage(checkStarted(41, "sql-injection"), checkCompleted(42, "sql-injection", "clean", 0, 800))}
	out, _, code := checking(t, h)
	if code != exitError || !strings.Contains(out, "\n    ✓ sql-injection      clean") {
		t.Errorf("exit %d, and the failed run's rows are not on screen:\n%s", code, out)
	}
}

// A completion resolves its row the moment it arrives, held for the stage row the verdict prints, and
// nothing is flushed after the poll: a check that started and never completed has no row of its own.
func TestACompletionResolvesItsRowWithNoFlushAfterThePoll(t *testing.T) {
	var b strings.Builder
	run := &checkRun{}
	watch, _ := (&app{out: &b}).narrate(run)
	if b.String() != "\n  Review\n" { // off a terminal the phase is its line as the run starts (cli.md §2)
		t.Errorf("a run that has written no row printed %q, want the phase line alone", b.String())
	}
	feed := func(raw string) int {
		var row timelineRow
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		watch(row)
		return len(run.Rows)
	}
	if got := feed(checkStarted(41, "secrets")); got != 0 {
		t.Errorf("a started check resolved %d rows, want none until it completes", got)
	}
	if got := feed(checkCompleted(42, "secrets", "clean", 0, 800)); got != 1 {
		t.Errorf("the completion resolved %d rows, want its own", got)
	}
	if got := feed(checkStarted(43, "dependencies")); got != 1 {
		t.Errorf("a second start resolved %d rows, want nothing until it completes", got)
	}
	if b.String() != "\n  Review\n" {
		t.Errorf("a row printed before the verdict:\n%s", b.String())
	}

	h := &gateHost{run: aRun("clean", 0, 0), page: gatePage(checkStarted(41, "secrets"), checkCompleted(42, "secrets", "clean", 0, 800), checkStarted(43, "dependencies"))}
	out, _, code := checking(t, h)
	if code != exitOK || strings.Contains(out, "dependencies") || !strings.Contains(out, "✓ secrets") {
		t.Errorf("exit %d, and the poll flushed a check that never completed:\n%s", code, out)
	}
}

// A person's blocked list joins one Canyon rule on one file and line as --format ai does (cli.md §3.3;
// the demo's claims-portal-next: two opengrep rules on server.js:10 were two entries and "2 blocking
// findings" for one hand-built query): one entry, every tool's rule under found by, and the stage row
// and footer count the entries.
func TestAPersonsBlockedListJoinsOneRuleOnOneLine(t *testing.T) {
	run := goldenRun(t)
	base := run.Result.Findings[3]
	yes := true
	sql := `  const { rows } = await pool.query("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'");`
	at := func(rule string, column int, title string) finding {
		f := base
		f.RuleID, f.Tool = "sql.string-concat.opengrep."+rule, "sarif/opengrep"
		f.Locations = []location{{PhysicalLocation: physicalLocation{ArtifactLocation: artifactLocation{URI: "server.js"},
			Region: region{StartLine: 10, StartColumn: column, EndLine: 10, EndColumn: 112, Snippet: sarifText{Text: sql}}}}}
		c := &f.Properties.Canyon
		c.Severity, c.Blocks, c.SecretName, c.Shown, c.Title, c.Problem = "critical", &yes, nil, nil, title, title+"."
		c.Fix, c.PolicyName = "Pass the user's input to the query as a bound parameter instead of building the SQL text from it.", "No hand-built SQL from user input"
		return f
	}
	run.Result.Findings = []finding{
		at("rules_lgpl_javascript_database_rule-node-sqli-injection", 26, "Improper neutralization of special elements used in an SQL command (SQL Injection)"),
		at("canyon_sql_rule-request-in-sql-text", 37, "Improper neutralization of special elements used in an SQL command ('SQL Injection')"),
	}
	run.Result.Blocking, run.Result.Warnings = 2, 0
	var out strings.Builder
	(&app{out: &out}).verdict(run)
	got := out.String()
	for _, want := range []string{
		"✗ Review blocked · 1 blocking finding\n",
		"  server.js:10:26\n  found by opengrep (2 rules)\n",
		"\n  1 blocking finding · 0 warnings\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want\n%s\nin\n%s", want, got)
		}
	}
	if n := strings.Count(got, "critical  "); n != 1 {
		t.Errorf("%d entries for one rule on one line, want 1:\n%s", n, got)
	}
}

// One approval of a joined entry reads as one finding's: the verdict names it once with its reason,
// and the entry carries no approved-by line of its own (cli.md §3.3, §3.5).
func TestAnApprovedJoinedEntryIsOneApproval(t *testing.T) {
	run := goldenRun(t)
	base := run.Result.Findings[3]
	yes := true
	ok := &approval{Reason: "Parameterised in the next release", ExpiresAt: "2026-10-20T00:00:00Z"}
	ok.ApprovedBy.Display = "Dana Whitfield"
	at := func(rule string, column int) finding {
		f := base
		f.RuleID, f.Tool, f.Approval = "sql.string-concat.opengrep."+rule, "sarif/opengrep", ok
		f.Locations = []location{{PhysicalLocation: physicalLocation{ArtifactLocation: artifactLocation{URI: "server.js"},
			Region: region{StartLine: 10, StartColumn: column, EndLine: 10, EndColumn: 40, Snippet: sarifText{Text: "  pool.query(sql)"}}}}}
		c := &f.Properties.Canyon
		c.Severity, c.Blocks, c.SecretName, c.Shown = "critical", &yes, nil, nil
		return f
	}
	run.Result.Findings = []finding{at("a", 3), at("b", 8)}
	run.Result.Blocking, run.Result.Warnings = 2, 0
	var out strings.Builder
	(&app{out: &out}).verdict(run)
	got := out.String()
	other := *ok // a second person's approval of the other rule: one entry, two approvals, each said on it once
	other.ApprovedBy.Display, other.Reason = "Priya Raman", "Covered by the bound parameter in #42"
	run.Result.Findings[1].Approval = &other
	var two strings.Builder
	(&app{out: &two}).verdict(run)
	if got := two.String(); !strings.Contains(got, "! Review approved · 1 blocking finding, approved\n") ||
		strings.Count(got, "approved by Dana Whitfield until 20 Oct") != 1 || strings.Count(got, "approved by Priya Raman until 20 Oct") != 1 {
		t.Errorf("one entry with two approvals:\n%s", got)
	}
	if !strings.Contains(got, "! Review approved · 1 blocking finding, approved by Dana Whitfield until 20 Oct\n    \"Parameterised in the next release\"\n") ||
		strings.Contains(got, "approved by Dana Whitfield until 20 Oct ·") || !strings.Contains(got, "\n  1 blocking finding · 0 warnings\n") {
		t.Errorf("an approved joined entry:\n%s", got)
	}
}

// The app's page is linked at the host's public URL, as login read it, not the address this CLI
// dials: the demo's sample apps reach the server as http://server:7777, a name only its network
// resolves (#2019).
func TestTheAppsPageIsLinkedAtTheHostsPublicURL(t *testing.T) {
	h := &gateHost{run: aRun("blocked", 1, 0, blocker), page: buildPage(t, nil, false)}
	h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
	h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	conf := t.TempDir()
	writeFile(t, filepath.Join(conf, "canyon", "config.json"), `{"host":"`+server.URL+`","public":"http://localhost:7777"}`)
	t.Chdir(projectDir(t))
	out, errOut, code := run(t, map[string]string{"XDG_CONFIG_HOME": conf}, false, "check")
	if code != exitBlocked || errOut != "" || !strings.Contains(out, "\n  Open in Canyon http://localhost:7777/#/apps/claims-portal\n") {
		t.Errorf("exit %d, stderr %q, want the page at the public URL in\n%s", code, errOut, out)
	}
}
