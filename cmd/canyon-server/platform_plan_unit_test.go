package main

import (
	"testing"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
)

// The patch plan takes the receipt's own read of an image (design/PATCHING.md §4.2): only a read that
// checked, each counted finding by its package, vulnerability, fix, class and severity, whatever the rule's
// state; a read that could not check plans nothing.
func TestThePlanTakesTheReceiptsRead(t *testing.T) {
	c := trivy.Component{Name: "libssl3", Version: "3.0.13-0ubuntu3.4", PURL: "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?distro=ubuntu-24.04"}
	v := trivy.Vuln{VulnerabilityID: "CVE-2024-6119", FixedVersion: "3.0.13-0ubuntu3.5", Severity: "HIGH"}
	read := imageRead{State: "checked", Findings: []platformFinding{platformFindingOf(v, c, "os", "sha256:1", trivy.RunImage, gatepolicy.Off, platformRule)}}
	got := platformImage(read)
	want := runs.PlatformFinding{Name: "libssl3", Version: "3.0.13-0ubuntu3.4", Vulnerability: "CVE-2024-6119", Fixed: "3.0.13-0ubuntu3.5", Class: "os", Severity: "high"}
	if !got.Checked || len(got.Findings) != 1 || got.Findings[0] != want {
		t.Fatalf("a read that checked: %+v", got)
	}
	read.State, read.Why = "could_not_check", "could not check: the image has not been matched yet"
	if got := platformImage(read); got.Checked || got.Findings != nil || got.Why != read.Why {
		t.Fatalf("a read that could not check: %+v", got)
	}
}
