package cli

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// spinner is cli.md §2's one spinner: a line under the rows naming the phase that is waiting and how
// long it has waited, redrawn every 80ms and wiped before anything else prints, so a review whose
// scanners write nothing for a minute never reads as a hang. It draws only on a terminal under the
// human format, and not under CI; anywhere else the review is the one line narrate prints as it
// starts, and the build is its heading over a failed build's log.
type spinner struct {
	mu     sync.Mutex
	out    io.Writer // the terminal: every print reaches it through Write
	render Render
	frames []string
	now    func() time.Time
	label  string // the phase waiting, "" for none
	began  time.Time
	drawn  bool
	done   chan struct{}
}

const (
	spinEvery = 80 * time.Millisecond  // §2's cadence: alive, never a strobe
	spinAfter = 150 * time.Millisecond // a phase shorter than this never animates
)

// spinner starts one over a.out and puts it in a.out's place until stop. Nil off a terminal, under
// any format but human, and under CI, which a runner may give a terminal (cli.md §2); a nil
// spinner's methods do nothing.
func (a *app) spinner() *spinner {
	if !a.tty || a.render.Format != FormatHuman || (a.getenv != nil && a.getenv("CI") != "") {
		return nil
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	if a.render.ASCII {
		frames = []string{"|", "/", "-", "\\"}
	}
	s := &spinner{out: a.out, render: a.render, frames: frames, now: a.clock.now, done: make(chan struct{})}
	if s.now == nil {
		s.now = time.Now
	}
	a.out = s
	go s.run()
	return s
}

// Write wipes the spinner's line before anything else prints on it; the next tick draws it again
// under what printed.
func (s *spinner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wipe()
	return s.out.Write(p)
}

func (s *spinner) wipe() {
	if s.drawn {
		_, _ = io.WriteString(s.out, "\r\x1b[K")
		s.drawn = false
	}
}

// phase names what is waiting from now on; "" draws nothing.
func (s *spinner) phase(label string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.label, s.began = label, s.now()
	s.wipe()
}

func (s *spinner) run() {
	tick := time.NewTicker(spinEvery)
	defer tick.Stop()
	for n := 0; ; n++ {
		select {
		case <-s.done:
			return
		case <-tick.C:
			s.draw(n)
		}
	}
}

// draw is frame n: the glyph, the phase, and the whole seconds it has waited, in one write so it
// never flickers.
func (s *spinner) draw(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done: // a tick that waited on stop's lock draws nothing after it
		return
	default:
	}
	waited := s.now().Sub(s.began)
	if s.label == "" || waited < spinAfter {
		return
	}
	_, _ = fmt.Fprintf(s.out, "\r  %s %s   %s\x1b[K", s.render.paint(ansiDim, s.frames[n%len(s.frames)]), s.label, s.render.paint(ansiDim, waitedFor(waited)))
	s.drawn = true
}

// waitedFor is the time a phase has waited in whole seconds: `41s`, then `1m 15s`.
func waitedFor(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// stop ends the spinner, wipes its line and gives the terminal back to a.
func (s *spinner) stop(a *app) {
	if s == nil {
		return
	}
	close(s.done)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wipe()
	a.out = s.out
}
