package runs

import (
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/policy"
)

// A laptop host's run says trivy is skipped while its rule is on and its run off, as the laptop demo leaves it (#237):
// never on another host, where an admin's off is the not-checked line alone, nor while trivy runs or its rule is off.
func TestALaptopHostSaysTrivyIsSkippedWhileItsRunIsOff(t *testing.T) {
	snap := func(state policy.State, run bool) policy.Snapshot {
		return policy.Snapshot{Rules: []policy.Rule{
			{ID: "deps.known-vuln", State: policy.Advisory, Producer: policy.Scanner},
			{ID: "sarif.trivy", State: state, Producer: policy.Sarif, Run: run},
		}}
	}
	for _, c := range []struct {
		why    string
		snap   policy.Snapshot
		laptop bool
		want   []string
	}{
		{"the demo, trivy off", snap(policy.Advisory, false), true, []string{"trivy"}},
		{"another host, trivy off", snap(policy.Advisory, false), false, nil},
		{"the demo, trivy on", snap(policy.Advisory, true), true, nil},
		{"the demo, trivy's rule off", snap(policy.Off, false), true, nil},
		{"the demo, no trivy rule", policy.Snapshot{}, true, nil},
	} {
		if got := skippedScans(c.snap, c.laptop); !slices.Equal(got, c.want) {
			t.Errorf("%s: skipped %q, want %q", c.why, got, c.want)
		}
	}
}
