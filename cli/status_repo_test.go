package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A delivery: repo release is said in the deploy repo's words (design/EXPERIENCE.md, the environment's pending
// line under delivery: repo), with the pull request it waits on under it; the webhook's lines never.
func TestARepoReleaseIsSaidInTheDeployRepoWords(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	sentAt := "2026-09-02T10:00:00Z"
	at, err := time.Parse(time.RFC3339, sentAt)
	if err != nil {
		t.Fatal(err)
	}
	const pr = "https://github.com/meridian/deploy/pull/42"
	for _, c := range []struct{ event, state, pull, line string }{
		{"deploy", "accepted", "", "your platform hasn't reported it running since " + at.Local().Format("15:04")},
		{"deploy", "accepted", pr, "waiting on your merge since " + at.Local().Format("15:04")},
		{"remove", "accepted", pr, "being removed once you merge"},
		{"remove", "accepted", "", "being removed by your platform"},
		{"deploy", "undelivered", "", "Canyon couldn't commit the release"},
		{"deploy", "refused", "", "Your deploy repo refused the release"},
	} {
		body := `{"run_by":"org","pending":{"release":"rel_8f2a","via":"repo","event":"` + c.event + `","sent_at":"` + sentAt + `","state":"` + c.state + `"`
		if c.pull != "" {
			body += `,"pull_request":{"url":"` + c.pull + `","number":42}`
		}
		var waiting env
		if err := json.Unmarshal([]byte(body+"}}"), &waiting); err != nil {
			t.Fatal(err)
		}
		s.Envs["prod"] = &waiting
		out := rendered(t, s, latest)
		if !strings.Contains(out, "   "+c.line+"\n") || strings.Contains(out, "your platform since") {
			t.Errorf("%s %s: rendered\n%s", c.event, c.state, out)
		}
		if c.pull != "" && !strings.Contains(out, "pull request: "+pr) {
			t.Errorf("%s %s: no pull request line\n%s", c.event, c.state, out)
		}
	}
}
