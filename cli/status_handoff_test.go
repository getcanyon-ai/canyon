package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handedOff is claims-archive's status read from the contract (contracts/testdata/app-page/
// claims-archive-handoff-status.json): production the org runs, a pod of it checked in, its removal
// waiting on the platform. The clock is UTC so the goldens read the same on every machine.
func handedOff(t *testing.T) statusRead {
	t.Helper()
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	raw, err := os.ReadFile(filepath.Join(pkgDir, "..", "contracts", "testdata", "app-page", "claims-archive-handoff-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s statusRead
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	s.Access = &teamAccess{Level: "owner", People: 1, Groups: 1}
	return s
}

func renderedAt(s statusRead, format Format, now time.Time) string {
	var out strings.Builder
	a := &app{out: &out}
	if format == FormatAI {
		a.render = Render{Format: FormatAI}
	}
	a.renderStatus("claims-archive", "http://localhost:7777", s, &runRow{ID: "gr_2e88", State: "clean", EndedAt: "2026-08-24T13:40:00Z"}, now)
	return out.String()
}

// A pod's check-in is said under what the org runs and above what it waits on (cli.md §6.1; contracts/routes.md,
// envs.<env>.seen_at): two minutes ago, in words, faint; never "running"; and --format ai says the same sentence,
// never the timestamp. While the environment waits on a remove its pods pull a list that admits nobody, so a fresh
// check-in is amber, a pod the platform has not taken down yet (#2450): the contract's read is such a one, and the
// whole page is the golden.
func TestStatusSaysAPodCheckedIn(t *testing.T) {
	s := handedOff(t)
	s.Envs["prod"].SeenAt = "2026-09-02T11:58:00Z"
	wants(t, "status-handoff-seen.txt", renderedAt(s, FormatHuman, at))
	ai := renderedAt(s, FormatAI, at)
	if line := "\nproduction: v3.1 · http://claims-archive.prod.localhost:7777 · deployed 2026-08-24T13:55:02Z · still running: your platform last reported it 2m ago (your platform's pods: nothing to do in Canyon) · being removed by your platform\n"; !strings.Contains(ai, line) || strings.Contains(ai, "11:58:00") {
		t.Errorf("under --format ai, a stopped environment's status said\n%s", ai)
	}
	s.Envs["prod"].SeenAt = "2026-09-02T12:03:00Z" // ahead of this clock: now
	if got := renderedAt(s, FormatHuman, at); !strings.Contains(got, "\n                      ! still running: your platform last reported it just now\n") {
		t.Errorf("a check-in ahead of the clock rendered\n%s", got)
	}
	s.Envs["prod"].SeenAt, s.Envs["prod"].Pending = "2026-09-02T11:58:00Z", nil
	if got := renderedAt(s, FormatHuman, at); !strings.Contains(got, "\n                      your platform reported it running 2m ago\n  access") {
		t.Errorf("with no remove pending, the check-in rendered\n%s", got)
	}
	if ai := renderedAt(s, FormatAI, at); !strings.Contains(ai, " · your platform reported it running 2m ago\n") || strings.Contains(ai, "nothing to do") {
		t.Errorf("with no remove pending, under --format ai:\n%s", ai)
	}
}

// No pod for five minutes, or none at all ten minutes after the report, is an amber line — the day too
// when the last one is not today — and under --format ai the same words and that it is the platform's
// to act on; within the first ten minutes nothing is said yet, as before the check-in existed.
func TestStatusSaysNoPodHasCheckedIn(t *testing.T) {
	s := handedOff(t)
	s.Envs["prod"].SeenAt = ""
	wants(t, "status-handoff-unseen.txt", renderedAt(s, FormatHuman, at))
	for _, c := range []struct{ seen, human string }{
		{"2026-09-02T11:29:12Z", "! your platform hasn't reported v3.1 running since 11:29"},
		{"2026-09-01T23:50:00Z", "! your platform hasn't reported v3.1 running since 1 Sep 23:50"},
		{"", "! your platform reported v3.1 deployed, but not running"},
	} {
		s.Envs["prod"].SeenAt = c.seen
		if got := renderedAt(s, FormatHuman, at); !strings.Contains(got, "\n                      "+c.human+"\n                      being removed by your platform\n") {
			t.Errorf("seen %q rendered\n%s", c.seen, got)
		}
		if ai := renderedAt(s, FormatAI, at); !strings.Contains(ai, " · "+strings.TrimPrefix(c.human, "! ")+" (your platform's pods: nothing to do in Canyon) · being removed by your platform\n") {
			t.Errorf("seen %q under --format ai:\n%s", c.seen, ai)
		}
	}
	s.Envs["prod"].SeenAt, s.Envs["prod"].DeployedAt, s.Envs["prod"].Pending = "", "2026-09-02T11:52:00Z", nil
	want := "  production   v3.1   http://claims-archive.prod.localhost:7777   8m ago\n  access"
	if got := renderedAt(s, FormatHuman, at); !strings.Contains(got, want) {
		t.Errorf("eight minutes after the report, nothing new is said:\n%s", got)
	}
	s.Envs["prod"].RunBy, s.Envs["prod"].State, s.Envs["prod"].SeenAt = "canyon", "running", "2026-09-02T11:58:00Z"
	if got := renderedAt(s, FormatHuman, at) + renderedAt(s, FormatAI, at); strings.Contains(got, "checked in") {
		t.Errorf("an environment Canyon runs said a check-in:\n%s", got)
	}
}

// The release a report left resources out of says what the platform has to send (contracts/routes.md,
// envs.<env>.pending.missing): is for one, are for more, in place of waiting since.
func TestAReportShortOfAResourceIsThePendingLine(t *testing.T) {
	for missing, want := range map[string]string{`["db"]`: "Your platform's report left out where db is", `["db","files"]`: "Your platform's report left out where db, files are"} {
		var waiting env
		if err := json.Unmarshal([]byte(`{"run_by":"org","pending":{"release":"rel_8f2a","event":"deploy","sent_at":"2026-09-02T10:00:00Z","state":"accepted","missing":`+missing+`}}`), &waiting); err != nil {
			t.Fatal(err)
		}
		if got := waiting.Pending.line(); got != want {
			t.Errorf("missing %s: %q, want %q", missing, got, want)
		}
	}
}

// cli.md §6.1 shows the production rows the two goldens hold, the check-in between what runs and what it
// waits on.
func TestTheSpecSaysTheCheckInRows(t *testing.T) {
	spec, err := os.ReadFile(filepath.Join("..", "design", "specs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"status-handoff-seen.txt", "status-handoff-unseen.txt"} { // an indented block: §6.1's one fence is the transcript
		_, rows, _ := strings.Cut(golden(t, name), "\n  production ")
		rows, _, _ = strings.Cut(rows, "\n  access ")
		if rows = "\n      production " + strings.ReplaceAll(rows, "\n", "\n    ") + "\n"; !strings.Contains(string(spec), rows) {
			t.Errorf("cli.md §6.1 does not show %s's rows:\n%s", name, rows)
		}
	}
}
