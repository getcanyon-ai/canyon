//go:build cluster

package cluster

import (
	"fmt"
	"testing"
	"time"
)

// waitFor polls every 2 s until poll answers ok, and answers its last observation. Past within it
// fails naming what it waited for and the last thing poll saw (README, Waits).
func waitFor(t *testing.T, within time.Duration, what string, poll func() (observation string, ok bool)) string {
	t.Helper()
	for until := time.Now().Add(within); ; time.Sleep(2 * time.Second) {
		observation, ok := poll()
		if ok {
			return observation
		}
		if time.Now().After(until) {
			t.Fatal(waitTimeout(within, what, observation, time.Now()))
		}
	}
}

// waitTimeout is waitFor's failure: within in seconds, as the deadlines are written, and the time
// of the last observation.
func waitTimeout(within time.Duration, what, last string, at time.Time) string {
	return fmt.Sprintf("waited %gs for %s: last answer %s at %s", within.Seconds(), what, last, at.Format(time.TimeOnly))
}

func TestUnitWaitTimeout(t *testing.T) {
	at := time.Date(2026, 9, 27, 10, 4, 59, 0, time.Local)
	want := "waited 60s for bob to reach e2e-app: last answer 302 /#/no-access/e2e-app at 10:04:59"
	if got := waitTimeout(60*time.Second, "bob to reach e2e-app", "302 /#/no-access/e2e-app", at); got != want {
		t.Errorf("waitTimeout is %q; want %q", got, want)
	}
}
