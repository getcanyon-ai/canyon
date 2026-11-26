package cli

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// importedRow is an imported tool's producer.sarif row (contracts/events.md): the report's category,
// who attached it and its normalised results, and no `sarif` key.
func importedRow(t *testing.T, tool, category string, results ...string) timelineRow {
	t.Helper()
	data := `{"producer":"sarif/` + tool + `","tool":"` + tool + `","category":"` + category + `","version":"1.0.0","digest":"sha256:` +
		strings.Repeat("ab", 32) + `","attached_by":"ada","dropped":0,"results":[` + strings.Join(results, ",") + `]}`
	if !json.Valid([]byte(data)) {
		t.Fatalf("not JSON: %s", data)
	}
	return timelineRow{ID: 70, Kind: "producer.sarif", Data: json.RawMessage(data)}
}

// toolRun is an imported tool's run in the verdict log, with the notes the gate writes (runs/pipeline.go).
func toolRun(tool, category string, notes []string, results ...string) string {
	said := make([]string, 0, len(notes))
	for _, n := range notes {
		said = append(said, `{"message":{"text":"`+n+`"}}`)
	}
	run := `{"tool":{"driver":{"name":"sarif/` + tool + `"}},`
	if category != "" {
		run += `"automationDetails":{"id":"` + category + `"},`
	}
	return run + `"invocations":[{"toolExecutionNotifications":[` + strings.Join(said, ",") + `]}],"results":[` + strings.Join(results, ",") + `]}`
}

func verdictRow(runs ...string) timelineRow {
	return timelineRow{ID: 80, Kind: "verdict", Data: json.RawMessage(`{"state":"blocked","findings":{"version":"2.1.0","runs":[` +
		`{"tool":{"driver":{"name":"canyon/scanners"}},"results":[]},` + strings.Join(runs, ",") + `]}}`)}
}

var snykMissing = toolRun("snyk", "", []string{"snyk · no report for this package"})

// The laptop demo leaves trivy off (#237): its skipped scan is a line among the tools', in Canyon's words, never a
// clean one, under ASCII's glyphs too; a reason the CLI doesn't know reads skipped alone.
func TestASkippedScanIsALineAmongTheTools(t *testing.T) {
	skipped := func(reason string) timelineRow {
		return timelineRow{ID: 70, Kind: "progress", Data: json.RawMessage(`{"step":"scan","tool":"trivy","status":"skipped","reason":"` + reason + `"}`)}
	}
	for _, c := range []struct {
		ascii  bool
		reason string
		want   string
	}{
		{false, "laptop_demo", "    · trivy              skipped: off in the laptop demo; canyon demo up --with-trivy turns it on\n"},
		{true, "laptop_demo", "    - trivy              skipped: off in the laptop demo; canyon demo up --with-trivy turns it on\n"},
		{false, "another_reason", "    · trivy              skipped\n"},
	} {
		var b strings.Builder
		a := &app{out: &b, verb: command{name: "check"}, render: Render{ASCII: c.ascii}}
		run := checkRun{App: "claims-portal", Run: "gr_8f2a", Result: runResult{ID: "gr_8f2a", Package: "pkg_1", State: "clean"}}
		watch, _ := a.narrate(&run)
		for _, row := range []timelineRow{skipped(c.reason), verdictRow(toolRun("opengrep", "", nil))} {
			watch(row)
		}
		a.reviewRows(run)
		if out := b.String(); !strings.Contains(out, c.want) || strings.Index(out, "opengrep") > strings.Index(out, "trivy") {
			t.Errorf("ascii %v, reason %s: the rows read\n%s\nwant, after opengrep's\n%s", c.ascii, c.reason, out, c.want)
		}
	}
}

// An imported tool's line counts what blocks, never what is severe (contracts/finding.md).
const (
	highBlocker      = `{"ruleId":"sarif.codeql.js-sql-injection",` + where + `"properties":{"canyon":{"severity":"high","blocks":true}}}`
	criticalAdvisory = `{"ruleId":"sarif.semgrep.eval",` + where + `"properties":{"canyon":{"severity":"critical","blocks":false}}}`
)

// Two reports attached before the run, each on its own line, and under the review's stage row one line per tool
// the verdict carries: the run reviews the package the upload named, whichever verb started it.
func TestCheckAttachesEachReportThenStartsTheRunOnThatPackage(t *testing.T) {
	semgrep := toolRun("semgrep", "", []string{"semgrep · report attached by ada"}, criticalAdvisory)
	codeql := toolRun("codeql", "javascript", []string{"codeql · report attached by ada", "codeql · dropped 1 result(s) at attach"}, highBlocker, highBlocker)
	rows, err := json.Marshal(timelinePage{Rows: []timelineRow{importedRow(t, "semgrep", "", criticalAdvisory),
		importedRow(t, "codeql", "javascript", highBlocker, highBlocker), verdictRow(semgrep, codeql, snykMissing)}})
	if err != nil {
		t.Fatal(err)
	}
	dir, want := t.TempDir(), golden(t, "sarif-attach.txt")
	writeFile(t, filepath.Join(dir, "semgrep.sarif"), `{"semgrep":1}`)
	writeFile(t, filepath.Join(dir, "codeql.sarif"), `{"codeql":2}`)
	h := &gateHost{page: string(rows), run: aRun("blocked", 2, 1, highBlocker, highBlocker, criticalAdvisory), reports: []reply{
		{http.StatusOK, `{"tool":"semgrep","category":"","results":1,"dropped":0}`},
		{http.StatusOK, `{"tool":"codeql","category":"javascript","results":2,"dropped":1,"first_dropped":"result 3: kind is pass"}`}}}
	out, errOut, code := verbingIn(t, h, projectDir(t), "check", "--sarif", filepath.Join(dir, "semgrep.sarif"), "--sarif", filepath.Join(dir, "codeql.sarif"))
	if head, _, _ := strings.Cut(out, "\n  ────"); head != want || code != exitBlocked {
		t.Errorf("exit %d, stderr %q; the check rendered\n%s\ntestdata/sarif-attach.txt wants\n%s", code, errOut, head, want)
	}
	if !slices.Equal(h.attached, []string{`{"semgrep":1}`, `{"codeql":2}`}) || !slices.Contains(h.asked, "PUT /api/v1/projects/claims-portal/packages/pkg_1/reports") {
		t.Errorf("the reports went as %q to %q", h.attached, h.asked)
	}
	for _, verb := range []string{"check", "deploy"} {
		h := &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true)}
		if _, errOut, code := verbing(t, h, verb); code != exitOK || h.started != `{"package":"pkg_1"}` {
			t.Errorf("%s exited %d (stderr %q) and started its run with %s, not the package it uploaded", verb, code, errOut, h.started)
		}
		nameless := &gateHost{run: aRun("clean", 0, 0), upload: reply{http.StatusCreated, `{"package":{}}`}}
		if _, errOut, code := verbing(t, nameless, verb); code != exitError || nameless.started != "" || !strings.Contains(errOut, "not Canyon") {
			t.Errorf("an upload naming no package: %s exited %d and started %q; stderr %q", verb, code, nameless.started, errOut)
		}
	}
}

// A report that cannot be attached ends the verb with exit 1 and no run: the host's own words for a
// refusal, the file's name for one this machine could not read.
func TestAReportThatCannotBeAttachedStartsNoRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "snyk.sarif"), `{}`)
	for _, c := range []struct {
		file, want string
		answer     reply
	}{
		{"snyk.sarif", golden(t, "sarif-refused.txt"), reply{http.StatusBadRequest, `{"error":"unknown_tool","message":"This report is from \"snyk\", which isn't a tool your admin has added to Canyon. Ask an admin to add it in Guardrails."}`}},
		{"snyk.sarif", "✗ the report is over 10 MiB\n", reply{http.StatusRequestEntityTooLarge, `{"error":"too_large","message":"the report is over 10 MiB"}`}},
		{"snyk.sarif", "✗ Canyon's limit is reached\n", reply{http.StatusTooManyRequests, `{"error":"rate_limited","message":"Canyon's limit is reached","retry_after_s":60}`}},
		{"snyk.sarif", "✗ only the app's team can do this; ask ada\n", reply{http.StatusForbidden, `{"error":"team_required","message":"only the app's team can do this; ask ada"}`}},
		{"snyk.sarif", "✗ internal error\n", reply{http.StatusInternalServerError, `{"error":"internal","message":"internal error"}`}},
		{"missing.sarif", "✗ could not read the report " + filepath.Join(dir, "missing.sarif") + ": no such file or directory\n", reply{}},
	} {
		for _, verb := range []string{"check", "deploy"} {
			h := &gateHost{run: aRun("clean", 0, 0), reports: []reply{c.answer}}
			_, errOut, code := verbingIn(t, h, projectDir(t), verb, "--sarif", filepath.Join(dir, c.file))
			want := c.want
			if verb == "deploy" && c.answer.code == http.StatusForbidden { // canyon.toml named an app whose team this is not (cli.md §9)
				want += "  To make your own app instead, delete the app line from canyon.toml and deploy again.\n"
			}
			if code != exitError || !strings.HasSuffix(errOut, want) || h.started != "" || h.polled() {
				t.Errorf("%s: exit %d, a run started %v; stderr\n%s\nwants to end\n%s", verb, code, h.started != "", errOut, want)
			}
		}
	}
	// The network drops on the second of three: the first stays attached, the third is never sent, no run.
	h := &gateHost{run: aRun("clean", 0, 0), reports: []reply{{http.StatusOK, `{"tool":"snyk","category":"","results":0,"dropped":0}`}, {}, {}}}
	file := filepath.Join(dir, "snyk.sarif")
	out, errOut, code := verbingIn(t, h, projectDir(t), "check", "--sarif", file, "--sarif", file, "--sarif", file)
	if code != exitError || len(h.attached) != 2 || strings.Count(out, "✓ Report") != 1 || h.started != "" || errOut == "" {
		t.Errorf("exit %d, %d PUTs, a run started %v; stdout\n%s\nstderr\n%s", code, len(h.attached), h.started != "", out, errOut)
	}
}

// Reports CI attached before the run count the same, and a tool the package has no report from says
// so: its line is read from its run in the verdict, since it has no producer row.
func TestEachImportedToolPrintsItsLineFromTheVerdict(t *testing.T) {
	var b strings.Builder
	a, run := &app{out: &b}, &checkRun{}
	watch, _ := a.narrate(run)
	rows := func() string { // the rows the stage row prints, alone
		b.Reset()
		a.reviewRows(*run)
		run.Rows = nil
		return b.String()
	}
	watch(importedRow(t, "semgrep", "", advisory))
	watch(verdictRow(toolRun("semgrep", "", []string{"semgrep · report attached by ci-bot"}, advisory), snykMissing))
	if want := golden(t, "review-gate-sarif.txt"); rows() != want {
		t.Errorf("the tools' lines rendered\n%s\ntestdata/review-gate-sarif.txt wants\n%s", b.String(), want)
	}
	// a result that does not say whether it blocks is never a green line: unknown blocks
	watch(verdictRow(toolRun("semgrep", "", []string{"semgrep · report attached by ci-bot"}, silent)))
	if !strings.Contains(rows(), "✗ semgrep            1 blocking finding · attached by ci-bot") {
		t.Errorf("a result with no blocks rendered\n%s", b.String())
	}
	// a tool Canyon ran is one more row of the review, so no one attached it
	watch(verdictRow(toolRun("trivy", "", []string{"trivy · report attached by Canyon"}, advisory)))
	if got := rows(); !strings.Contains(got, "✓ trivy              1 warning\n") || strings.Contains(got, "attached") {
		t.Errorf("a Canyon-run tool rendered\n%s", got)
	}
}

// A tool that could not read a file is never a green "clean": its row says what it missed, and the
// note reaches the run's notes, which an agent reads as review coverage (the test cluster's case-not-checked).
func TestAnImportedToolThatMissedAFileSaysSo(t *testing.T) {
	var b strings.Builder
	a, run := &app{out: &b}, &checkRun{}
	watch, _ := a.narrate(run)
	missed := "eslint · not checked: tools/report.ts was not read whole"
	watch(verdictRow(toolRun("eslint", "", []string{"eslint · report attached by Canyon", missed})))
	a.reviewRows(*run)
	if got := b.String(); strings.Contains(got, "✓ eslint") || !strings.Contains(got, "eslint             0 findings · 1 not checked") || strings.Contains(got, "clean") {
		t.Errorf("a tool that missed a file rendered\n%s", got)
	}
	if !slices.Contains(run.Notes, producerNote{Producer: "sarif/eslint", Text: missed}) || slices.ContainsFunc(run.Notes, func(n producerNote) bool { return strings.Contains(n.Text, "attached by") }) {
		t.Errorf("the run's notes %+v: want the not-checked note alone", run.Notes)
	}
}

// A tool whose scan in this run did not finish is never "clean", though the package had a clean report of
// it from before: its row says it could not check, in red (contracts/finding.md, In: external SARIF;
// #1658: a rerun whose trivy timed out listed "+ trivy clean" above its own could-not-check).
func TestAToolThatDidNotFinishIsNeverClean(t *testing.T) {
	var b strings.Builder
	a, run := &app{out: &b}, &checkRun{}
	watch, _ := a.narrate(run)
	watch(verdictRow(toolRun("trivy", "", []string{"trivy · could not check: did not finish in time", "trivy · report attached by Canyon"})))
	a.reviewRows(*run)
	if got := b.String(); !strings.Contains(got, "✗ trivy              could not check: did not finish in time") || strings.Contains(got, "clean") {
		t.Errorf("a tool that did not finish rendered\n%s", got)
	}
}

// A tool that had no file of the package to read is never "clean": its row says why, dim
// (contracts/finding.md, In: external SARIF; #1742: a Rails app's eslint read "clean").
func TestAToolWithNoFileToReadIsNeverClean(t *testing.T) {
	for _, said := range [][]string{{"eslint · read no file: no JavaScript or TypeScript here", "eslint · report attached by Canyon"},
		{"eslint · no report for this package", "eslint · read no file: no JavaScript or TypeScript here"}} { // the why outranks no report
		var b strings.Builder
		a, run := &app{out: &b}, &checkRun{}
		watch, _ := a.narrate(run)
		watch(verdictRow(toolRun("eslint", "", said)))
		a.reviewRows(*run)
		if got := b.String(); !strings.Contains(got, "– eslint             no JavaScript or TypeScript here") || strings.Contains(got, "clean") {
			t.Errorf("%q rendered\n%s", said, got)
		}
	}
}

// An imported tool's finding says where it came from, as the dashboard's card does: `from <tool> ·
// rule <its rule>` under its file:line, and in the agent's block the tool's rule under found by, its rule
// sarif.<tool> (contracts/finding.md, #1100; cli.md §5.2).
func TestAnImportedFindingSaysWhichToolAndRule(t *testing.T) {
	for format, want := range map[string]string{"human": "  from codeql · rule js-sql-injection\n", "ai": "\nrule: sarif.codeql\nfound by: codeql's rule js-sql-injection\nfile:"} {
		h := &gateHost{run: aRun("blocked", 1, 0, highBlocker)}
		out, errOut, code := verbingIn(t, h, projectDir(t), "check", "--format", format)
		if code != exitBlocked || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, stderr %q; no %q in\n%s", format, code, errOut, want, out)
		}
	}
}
