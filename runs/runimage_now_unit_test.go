package runs

import "testing"

// An app waits on a newer run image when a counted os finding at or above high is fixed in no version the run image
// Canyon names carries (design/PATCHING.md §0.4); one it carries a fix for is a rebase, never wait; a run image whose
// match does not read, or did not match the package, is never read as wait, and its os finding reads unchecked (contracts/routes.md, envs.<env>.patch).
func TestNamedRunImageWaits(t *testing.T) {
	libssl := PlatformFinding{Name: "libssl3", Version: "3.0.13-0ubuntu3.4", Vulnerability: "CVE-2024-6119", Fixed: "3.0.13-0ubuntu3.5", Class: "os", Severity: "high"}
	low := libssl
	low.Severity = "low"
	runtime := libssl
	runtime.Class = "runtime"
	behind := NamedRunImage{Image: "canyon-run:noble", vulns: &runImageVulns{held: map[string]bool{"libssl3": true}, matched: map[string][]string{"libssl3": {"CVE-2024-6119"}}, unchecked: map[string]bool{}}}
	ahead := NamedRunImage{Image: "canyon-run:noble", vulns: &runImageVulns{held: map[string]bool{"libssl3": true}, matched: map[string][]string{}, unchecked: map[string]bool{}}}
	unread := NamedRunImage{Image: "canyon-run:noble"}
	unmatched := NamedRunImage{Image: "canyon-run:noble", vulns: &runImageVulns{held: map[string]bool{"libssl3": true}, matched: map[string][]string{}, unchecked: map[string]bool{"libssl3": true}}}
	for _, c := range []struct {
		name            string
		named           NamedRunImage
		findings        []PlatformFinding
		waits, unchecks bool
	}{
		{"the run image has the same vulnerable libssl3", behind, []PlatformFinding{libssl}, true, false},
		{"the run image carries the fix: a rebase", ahead, []PlatformFinding{libssl}, false, false},
		{"a low finding plans nothing", behind, []PlatformFinding{low}, false, false},
		{"a runtime finding is a bump", behind, []PlatformFinding{runtime}, false, false},
		{"a match that does not read never waits", unread, []PlatformFinding{libssl}, false, true},
		{"a match that does not read, and nothing counted", unread, []PlatformFinding{runtime}, false, false},
		{"a package the run image's match did not match never waits", unmatched, []PlatformFinding{libssl}, false, true},
		{"an unmatched package only a low finding names", unmatched, []PlatformFinding{low}, false, false},
		{"nothing counted", behind, nil, false, false},
	} {
		if got := c.named.Waits(c.findings); got != c.waits {
			t.Errorf("%s: waits %v, want %v", c.name, got, c.waits)
		}
		if got := c.named.Unchecked(c.findings); got != c.unchecks {
			t.Errorf("%s: unchecked %v, want %v", c.name, got, c.unchecks)
		}
	}
	if unread.Reads() || !behind.Reads() {
		t.Error("Reads is whether the run image's match reads")
	}
}
