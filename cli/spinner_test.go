package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The spinner's line is the phase and the whole seconds it has waited, it is wiped before anything
// else prints, and a phase younger than 150ms never draws (cli.md §2).
func TestTheSpinnerSaysWhichPhaseWaitsAndForHowLong(t *testing.T) {
	var b strings.Builder
	now := at
	s := &spinner{out: &b, frames: []string{"⠋", "⠙", "⠹"}, now: func() time.Time { return now }, done: make(chan struct{})}
	s.phase("Review gate")
	now = now.Add(100 * time.Millisecond)
	if s.draw(2); b.String() != "" {
		t.Errorf("a phase 100ms old drew %q", b.String())
	}
	now = now.Add(41 * time.Second)
	if s.draw(2); b.String() != "\r  ⠹ Review gate   41s\x1b[K" {
		t.Errorf("41s in, the line is %q", b.String())
	}
	if _, err := s.Write([]byte("  ✓ secrets\n")); err != nil || b.String() != "\r  ⠹ Review gate   41s\x1b[K\r\x1b[K  ✓ secrets\n" {
		t.Errorf("a row printed over the line as %q", b.String())
	}
	if waitedFor(75*time.Second) != "1m 15s" {
		t.Errorf("75s reads %q", waitedFor(75*time.Second))
	}
}

// Off a terminal, or under a format an agent or a script reads, nothing animates: a phase is the
// one line narrate prints as it starts.
func TestOnlyAHumanAtATerminalGetsASpinner(t *testing.T) {
	ci := func(k string) string { return map[string]string{"CI": "true"}[k] }
	for _, a := range []*app{
		{tty: false, render: Render{Format: FormatHuman}},
		{tty: true, render: Render{Format: FormatAI}},
		{tty: true, render: Render{Format: FormatJSON}},
		{tty: true, render: Render{Format: FormatHuman}, getenv: ci}, // a runner that hands the command a terminal
	} {
		if s := a.spinner(); s != nil {
			s.stop(a)
			t.Errorf("tty %v, format %s: a spinner", a.tty, a.render.Format)
		}
	}
}

// On a terminal the spinner carries Build, so no header prints as it starts; the held log prints
// under its own header once the build fails.
func TestOnATerminalAFailedBuildPrintsItsLogUnderItsHeader(t *testing.T) {
	var b strings.Builder
	a := &app{out: &b, err: &b, tty: true, verb: command{name: "deploy"}, render: Render{Format: FormatHuman},
		clock: clock{now: func() time.Time { return at }}} // a clock that never moves: no frame draws
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	var page timelinePage
	must(t, json.Unmarshal([]byte(buildPage(t, buildLines, false)), &page))
	each, stop := a.narrate(&run)
	for _, row := range page.Rows {
		each(row)
	}
	stop()
	if strings.Contains(b.String(), "Build") || strings.Contains(b.String(), "│") {
		t.Errorf("the build printed while it ran:\n%s", b.String())
	}
	a.verdict(run)
	if !strings.Contains(b.String(), "\n  ✓ Review passed · 0 blocking, 3 warnings\n\n  Build\n  │ Paketo Buildpack for Node Engine 1.2.3\n") || strings.Contains(b.String(), "\n  Review\n") {
		t.Errorf("the failed build's log is not under its header after the review's stage row, or the review's phase line printed beside the spinner:\n%s", b.String())
	}
}

// On a terminal the spinner carries Review, so a run that failed in its review prints the heading
// its rows sit under when it ends, there being no stage row to put them under.
func TestOnATerminalAFailedReviewPrintsItsHeading(t *testing.T) {
	var b strings.Builder
	a := &app{out: &b, err: &b, tty: true, verb: command{name: "check"}, render: Render{Format: FormatHuman},
		clock: clock{now: func() time.Time { return at }}}
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "model"
	var page timelinePage
	must(t, json.Unmarshal([]byte(gatePage(checkStarted(41, "sql-injection"), checkCompleted(42, "sql-injection", "clean", 0, 800))), &page))
	each, stop := a.narrate(&run)
	for _, row := range page.Rows {
		each(row)
	}
	stop()
	a.verdict(run)
	if !strings.Contains("\n"+b.String(), "\n\n  Review\n    ✓ sql-injection      clean") {
		t.Errorf("the failed review's rows are not under their heading:\n%s", b.String())
	}
}

// A tool waiting on its database says so as its scan.waiting row arrives (contracts/events.md): off a
// terminal one dim line under Review; on a terminal the spinner's phase; under ai nothing mid-run.
func TestAScanWaitingOnItsDatabaseSaysSo(t *testing.T) {
	row := timelineRow{Kind: "scan.waiting", Data: json.RawMessage(`{"tool":"trivy","note":"downloading its vulnerability database — the first scan on this host"}`)}
	var b strings.Builder
	watch, _ := (&app{out: &b}).narrate(&checkRun{})
	watch(row)
	if want := "\n  Review\n    · trivy: downloading its vulnerability database — the first scan on this host\n"; b.String() != want {
		t.Errorf("off a terminal: %q, want %q", b.String(), want)
	}
	a := &app{out: &b, tty: true, render: Render{Format: FormatHuman}, clock: clock{now: func() time.Time { return at }}}
	watch, stop := a.narrate(&checkRun{})
	watch(row)
	s := a.out.(*spinner)
	s.mu.Lock()
	label := s.label
	s.mu.Unlock()
	stop()
	if label != "Review · trivy: downloading its vulnerability database — the first scan on this host" {
		t.Errorf("on a terminal the spinner says %q", label)
	}
	var live strings.Builder
	ai := &app{out: &live, render: Render{Format: FormatAI}}
	ai.park()
	watch, _ = ai.narrate(&checkRun{})
	watch(row)
	if live.Len() != 0 {
		t.Errorf("under ai a line printed mid-run: %q", live.String())
	}
}

// A scan or the build waiting its turn says where it stands as its progress rows arrive (contracts/
// events.md, progress; #1652): on a terminal the spinner's phase; off one a dim line the first time a
// step waits, never again for a later place; a scan running moves the spinner on.
func TestAWaitingScanSaysItsPlaceInLine(t *testing.T) {
	queued := func(step, tool string, at int) timelineRow {
		d, _ := json.Marshal(map[string]any{"step": step, "tool": tool, "status": "queued", "position": at})
		if tool == "" {
			d, _ = json.Marshal(map[string]any{"step": step, "status": "queued", "position": at})
		}
		return timelineRow{Kind: "progress", Data: d}
	}
	var b strings.Builder
	watch, _ := (&app{out: &b}).narrate(&checkRun{})
	for _, r := range []timelineRow{queued("scan", "trivy", 3), queued("scan", "trivy", 3), queued("scan", "trivy", 2), queued("build", "", 1)} {
		watch(r)
	}
	if want := "\n  Review\n    · trivy: waiting for a scanner slot (3rd in line)\n    · waiting for a build slot (1st in line)\n"; b.String() != want {
		t.Errorf("off a terminal: %q, want %q", b.String(), want)
	}
	a := &app{out: &b, tty: true, render: Render{Format: FormatHuman}, clock: clock{now: func() time.Time { return at }}}
	watch, stop := a.narrate(&checkRun{})
	label := func() string {
		s := a.out.(*spinner)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.label
	}
	watch(queued("scan", "trivy", 2))
	if got := label(); got != "Review · trivy: waiting for a scanner slot (2nd in line)" {
		t.Errorf("on a terminal the spinner says %q", got)
	}
	watch(timelineRow{Kind: "progress", Data: json.RawMessage(`{"step":"scan","tool":"trivy","status":"running"}`)})
	if got := label(); got != "Review · trivy: running" {
		t.Errorf("a scan running: the spinner says %q", got)
	}
	stop()
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 112: "112th"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %s", n, got)
		}
	}
}
