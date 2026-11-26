package runs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/policy"
)

// With trivy's run off, as the laptop demo leaves it (#237), nothing on the host fills deps.known-vuln: the scanners'
// run names it not checked on this host, so the verdict never reads it as checked; with trivy's run on it does not.
func TestTrivyOffNamesItsRuleNotCheckedOnThisHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, run := range []bool{false, true} {
		snap := policy.Snapshot{Rules: []policy.Rule{
			{ID: "deps.known-vuln", State: policy.Advisory, Producer: policy.Scanner},
			{ID: "sarif.trivy", State: policy.Advisory, Producer: policy.Sarif, Run: run},
		}}
		log, err := Gate{}.Review(context.Background(), dir, "", snap, nop)
		if err != nil {
			t.Fatal(err)
		}
		said := false
		for _, inv := range log.Runs[0].Invocations {
			for _, n := range inv.ToolExecutionNotifications {
				said = said || n.Message.Text == "not checked on this host: deps.known-vuln"
			}
		}
		if said == run {
			t.Errorf("trivy's run %v: deps.known-vuln named not checked on this host %v", run, said)
		}
	}
}

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
