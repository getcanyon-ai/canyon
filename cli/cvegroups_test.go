package cli

import (
	"bytes"
	"strings"
	"testing"
)

// lodashBlocked is a blocked run whose blocking findings are one lodash's three CVEs and one other
// finding (the golden's advisory), with one lodash CVE that only warns: as the QA rerun's strict-approve-pushback app had it.
func lodashBlocked(t *testing.T) checkRun {
	t.Helper()
	run := goldenRun(t) // one critical blocker, one advisory
	yes, no := true, false
	cve := func(id, severity, fixed string, blocks *bool) finding {
		f := run.Result.Findings[0]
		f.RuleID, f.Tool = "deps.known-vuln.trivy."+id, "sarif/trivy"
		f.Locations = []location{{PhysicalLocation: physicalLocation{ArtifactLocation: artifactLocation{URI: "package-lock.json"},
			Region: region{StartLine: 17, StartColumn: 1, EndLine: 17, EndColumn: 2, Snippet: sarifText{Text: "••••    }"}}}}}
		c := &f.Properties.Canyon
		c.Severity, c.Blocks, c.SecretName, c.Shown = severity, blocks, nil, nil
		c.Title = "lodash: " + id + " title"
		c.Problem = c.Title + ". Package: lodash\nInstalled Version: 4.17.4\nVulnerability " + id + "\nSeverity: " + strings.ToUpper(severity) + "\nFixed Version: " + fixed
		c.RequiredEndState = "trivy no longer reports " + id + " for this code when it runs again, and the app still does what it did."
		c.Fix = "Upgrade the package to a version the advisory lists as fixed, then update the lockfile."
		c.DoNot = []string{"Silence it in trivy's settings or with an ignore comment — Canyon does not read those."}
		c.PolicyName = "No known-vulnerable dependencies"
		return f
	}
	other := run.Result.Findings[0]
	run.Result.Findings = []finding{
		cve("CVE-2018-16487", "high", ">=4.17.11", &yes),
		other,
		cve("CVE-2019-10744", "critical", "4.17.12", &yes),
		cve("CVE-2021-23337", "high", "4.17.21", &yes),
		cve("CVE-2018-3721", "low", ">=4.17.5", &no),
	}
	run.Result.Blocking, run.Result.Warnings = 0, 0
	for _, f := range run.Result.Findings {
		if blocks, _ := f.Blocking(); blocks {
			run.Result.Blocking++
		} else {
			run.Result.Warnings++
		}
	}
	return run
}

// A blocked run's blocking CVEs of one package are one entry, as its advisories are (cli.md §5.2; the
// 2026-10-08 QA rerun: five full blocks for one lodash): the group's line and highest severity, each
// finding a line under it, each host line once. The finding of another tool, and a lone advisory CVE,
// stay their own.
func TestABlockedRunGroupsAPackagesBlockingCVEs(t *testing.T) {
	var out strings.Builder
	aiApp(&out, "check").verdict(lodashBlocked(t))
	got := out.String()
	for _, want := range []string{
		"findings: 3 · blocking: 1\n", // the entries the list numbers (QA round 9)
		"--- FINDING 1 of 3 ",
		"title: lodash 4.17.4: 3 known vulnerabilities, fixed in 4.17.21\nseverity: critical\nblocks: yes\nrule: deps.known-vuln\nfound by: trivy\nfile: package-lock.json\nline: 17\ncolumn: 1\n" +
			"findings: 3, one per CVE:\n" +
			"  - CVE-2018-16487 (high): lodash: CVE-2018-16487 title · fixed in >=4.17.11\n" +
			"  - CVE-2019-10744 (critical): lodash: CVE-2019-10744 title · fixed in 4.17.12\n" +
			"  - CVE-2021-23337 (high): lodash: CVE-2021-23337 title · fixed in 4.17.21\n" +
			"\nrequired end state:\n" +
			"  trivy no longer reports CVE-2018-16487 for this code when it runs again, and the app still does what it did.\n" +
			"  trivy no longer reports CVE-2019-10744 for this code when it runs again, and the app still does what it did.\n" +
			"  trivy no longer reports CVE-2021-23337 for this code when it runs again, and the app still does what it did.\n" +
			"\nsuggested fix:\n  Upgrade the package to a version the advisory lists as fixed, then update the lockfile.\n" +
			"\ndo not:\n  - Silence it in trivy's settings or with an ignore comment — Canyon does not read those.\n  " + aiSameFinding + "\n",
		"--- FINDING 2 of 3 ",
		"--- FINDING 3 of 3 ",
		"rule: deps.known-vuln\nfound by: trivy's rule CVE-2018-3721\n",
		"--- END OF FINDINGS",
		"\nCANYON REVIEW — RESULT: BLOCKED\nblocking: deps.known-vuln · package-lock.json:17\n\nexit code: 2\n", // the recap: one line for the group
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want\n%s\nin\n%s", want, got)
		}
	}
	failed := lodashBlocked(t) // a run that failed after approvals counts the blocking entries, as the blocked block does (QA round 9)
	failed.Result.State, failed.Result.FailureReason = "failed", "build"
	out.Reset()
	aiApp(&out, "deploy").verdict(failed)
	if want := "\nreview: approved · 1 blocking, 2 warnings\n"; !strings.Contains(out.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, out.String())
	}
	var human bytes.Buffer
	a := &app{out: &human, err: &human, render: Render{ASCII: true}}
	a.findings(lodashBlocked(t))
	if got := human.String(); !strings.Contains(got, "  critical  lodash 4.17.4: 3 known vulnerabilities, fixed in 4.17.21\n  package-lock.json:17:1\n\n"+
		"  CVE-2018-16487  high      lodash: CVE-2018-16487 title\n  CVE-2019-10744  critical  lodash: CVE-2019-10744 title\n  CVE-2021-23337  high      lodash: CVE-2021-23337 title\n") ||
		strings.Count(got, "Upgrade the package") != 2 {
		t.Errorf("the terminal's blocked findings:\n%s", got)
	}
}
