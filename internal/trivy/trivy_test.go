package trivy

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Read takes trivy's own lines as contracts/finding.md words them: the last "Package: " before the first
// "\nInstalled Version: " with no line break between them, then the last line of each kind after it,
// and a link only under its own id over https.
func TestReadTakesTrivysOwnLines(t *testing.T) {
	lodash := "Lodash: x. Package: lodash\nInstalled Version: 4.17.4\nVulnerability CVE-2018-16487\nSeverity: HIGH\nFixed Version: 4.17.11\nLink: [CVE-2018-16487](https://avd.aquasec.com/nvd/cve-2018-16487)"
	for _, tc := range []struct{ problem, want string }{
		{lodash, "lodash|4.17.4|CVE-2018-16487|4.17.11|https://avd.aquasec.com/nvd/cve-2018-16487"},
		{"Package: a says Package: lodash\nInstalled Version: 4.17.4", "lodash|4.17.4|||"},
		{"Package: lodash\nInstalled Version: 4.17.4\nFixed Version: 1.0.0\nFixed Version: 2.0.0", "lodash|4.17.4||2.0.0|"}, // the last of each kind
		{strings.Replace(lodash, "(https://avd", "(javascript:alert(1)//avd", 1), "lodash|4.17.4|CVE-2018-16487|4.17.11|"},
		{strings.Replace(lodash, "[CVE-2018-16487](", "[CVE-2099-0001](", 1), "lodash|4.17.4|CVE-2018-16487|4.17.11|"},
		{strings.Replace(lodash, "https://avd.aquasec.com", "https://a b", 1), "lodash|4.17.4|CVE-2018-16487|4.17.11|"},
		{strings.Replace(lodash, "https://avd.aquasec.com/nvd/cve-2018-16487", "https://", 1), "lodash|4.17.4|CVE-2018-16487|4.17.11|"},
		{strings.Replace(lodash, "https://avd.aquasec.com", "https://avd.aquasec.com@evil.example", 1), "lodash|4.17.4|CVE-2018-16487|4.17.11|"}, // no user part
		{"Package: lodash\nSeverity: HIGH\nInstalled Version: 4.17.4", ""},                                                                       // a line break between them
		{"Package: \nInstalled Version: 4.17.4", ""},
		{"Package: lodash\nInstalled Version: \nFixed Version: 1", ""},
		{"Package: lodash", ""},
	} {
		a, ok := Read(tc.problem)
		got := ""
		if ok {
			got = strings.Join([]string{a.Name, a.Installed, a.Vulnerability, a.Fixed, a.Link}, "|")
		}
		if got != tc.want {
			t.Errorf("%q: %q, want %q", tc.problem, got, tc.want)
		}
	}
}

// FixedIn is cli.md §5.3's version: the lowest listed at or above the installed one that fixes every
// list, a list fixing a version in its own MAJOR.MINOR or at or above its highest; plain X.Y.Z only.
func TestFixedInIsTheLowestVersionThatFixesEveryList(t *testing.T) {
	for _, tc := range []struct {
		installed string
		lists     [][]string
		want      string
	}{
		{"4.17.4", [][]string{{"4.17.21", "5.0.1"}, {"4.17.12"}}, "4.17.21"},
		{"4.17.4", [][]string{{">=4.17.11"}, {"4.17.12"}, {"4.18.0"}}, "4.18.0"},
		{"5.1.4", [][]string{{"5.1.5", "5.0.11", "4.2.18"}, {"4.2.24", "5.1.12", "5.2.6"}}, "5.1.12"}, // stays in the installed major
		{"5.1.4", [][]string{{"5.1.5", "5.0.11"}, {"6.0.0"}}, "6.0.0"},
		{"1.0.0", [][]string{{"1.0.1-1+deb12u1"}}, ""},
		{"2.0.0", [][]string{{"1.0.1"}}, ""}, // none at or above the installed one
		{"v1.0.0", [][]string{{"1.0.1"}}, ""},
		{"1.0.0", slices.Repeat([][]string{{"1.0.1"}}, 201), ""}, // past 200 lists: none, rather than a long read
		{"1.0.0", [][]string{versions(33)}, ""},                  // past 32 versions in a list
	} {
		if got, ok := FixedIn(tc.installed, tc.lists); got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s %v: %q %v, want %q", tc.installed, tc.lists, got, ok, tc.want)
		}
	}
}

// versions is n plain versions, 1.0.1 up.
func versions(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("1.%d.%d", i/10, i%10+1))
	}
	return out
}

// The worst a report can shape within the bounds — 200 lists of 32 versions, one of them needing a
// version no other names — still answers at once (the security review of #1875: 30 lists of 457 took
// a second a call, and the run read made one call a finding).
func TestFixedInIsFastAtItsBounds(t *testing.T) {
	lists := slices.Repeat([][]string{versions(32)}, 199)
	lists = append(lists, []string{"9.9.9"})
	start := time.Now()
	if got, ok := FixedIn("1.0.0", lists); !ok || got != "9.9.9" {
		t.Fatalf("FixedIn: %q %v", got, ok)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("FixedIn at its bounds took %s", took)
	}
}
