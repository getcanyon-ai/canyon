//go:build stack

package e2e

import (
	"slices"
	"testing"
)

var scanApp = "e2e-scan-" + tag

// timelineRow is a run's timeline row as J11 reads it: a producer.sarif row names the producer and,
// for a report, who attached it (contracts/events.md).
type timelineRow struct {
	Kind string
	Data struct {
		Producer   string
		AttachedBy string `json:"attached_by"`
	}
}

// j11Scanners is a fresh install's first review with Canyon's OSS tools, on by default (#1380): trivy,
// opengrep and checkov read it all — opengrep's image built, trivy's database downloaded — and no tool
// answers could not check, which would block every review (contracts/finding.md, Canyon-run tools). It
// is the first use only on a stack make e2e-up just made: on a reused one the image and database are
// already there.
func j11Scanners(t *testing.T) {
	tools := []string{"trivy", "opengrep", "checkov"}
	out, code := check(t, fixture(t, "f-vuln", scanApp))
	if unread := slices.IndexFunc(out.Findings, func(f cliFinding) bool { return f.Rule == "gate.unreadable" }); code != 0 && code != 2 || unread >= 0 { // 1 is a run that failed
		t.Fatalf("exit %d: a tool could not check the first review: %+v", code, out.Findings)
	}
	var timeline struct{ Rows []timelineRow }
	read(t, "/api/v1/gate-runs/"+out.GateRun+"/timeline?after=0&limit=200", &timeline)
	for _, tool := range tools {
		if !slices.ContainsFunc(timeline.Rows, func(r timelineRow) bool {
			return r.Kind == "producer.sarif" && r.Data.Producer == "sarif/"+tool && r.Data.AttachedBy == "Canyon"
		}) {
			t.Errorf("no report of %s attached by Canyon in the run", tool)
		}
	}
}
