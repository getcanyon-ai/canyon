package patching

import "testing"

// Fail closed (#2575): an outcome the plan does not name, and an app it gave no cause, are never read as nothing to do.
func TestAnUnknownCauseIsNeverNothingToDo(t *testing.T) {
	if why := whyWaiting("sideways", 0, 0, false, false); why != whyUnknown {
		t.Fatalf("an outcome the plan does not name: %q", why)
	}
	if line, want := (planSaid{run: "r", apps: map[string]string{"x": ""}}).String(), "runs: the patch plan onto r started 0 patch run(s): 1 not, for a reason the plan does not name (x)"; line != want {
		t.Fatalf("an app given no cause:\n got %s\nwant %s", line, want)
	}
}
