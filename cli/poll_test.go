package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// h2Timeout is the error the test cluster's poll met (#2721): HTTP/2's wait for the response headers, a net.Error that timed out.
type h2Timeout struct{}

func (h2Timeout) Error() string   { return "http2: timeout awaiting response headers" }
func (h2Timeout) Timeout() bool   { return true }
func (h2Timeout) Temporary() bool { return true }

// Any wait the connection gave up on is no answer, HTTP/2's for the headers among them: it is what a poll sends
// again (cli.md §3.5, #2721). It read as the host's own words before, which nothing sent again.
func TestEveryTimeoutTheConnectionReportsIsNoAnswer(t *testing.T) {
	for name, err := range map[string]error{
		"HTTP/2's wait for the headers": h2Timeout{},
		"a read that timed out":         &net.OpError{Op: "read", Net: "tcp", Err: h2Timeout{}},
	} {
		var silent *NoAnswer
		if got := transportError("http://localhost:7777", requestTimeout, err); !errors.As(got, &silent) || got.Error() != "http://localhost:7777: no answer in 30s" {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// flaky lets every request through to the host but the timeline polls down says fail, the n-th poll counted from 1,
// which it fails with err.
type flaky struct {
	next  http.RoundTripper
	down  func(n int) bool
	err   error
	polls []string // each poll's after=, in order
	on    string   // the path it fails, by its start or end: "" a timeline's
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.on == "" && !strings.HasSuffix(r.URL.Path, "/timeline") || f.on != "" && !strings.HasPrefix(r.URL.Path, f.on) {
		return f.next.RoundTrip(r)
	}
	f.polls = append(f.polls, r.URL.Query().Get("after"))
	if f.down(len(f.polls)) {
		return nil, f.err
	}
	return f.next.RoundTrip(r)
}

// deployingThrough runs canyon deploy against h, the client's transport wrapped by wrap, on a clock that moves
// only when the CLI sleeps; it answers what the deploy printed, its exit and each wait it slept.
func deployingThrough(t *testing.T, h http.Handler, wrap func(http.RoundTripper) http.RoundTripper, args ...string) (out, errOut string, code int, slept []time.Duration) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	tick := time.Date(2026, 9, 2, 9, 31, 4, 0, time.UTC)
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir()}
	var o, e strings.Builder
	a := &app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] },
		clock: clock{now: func() time.Time { return tick }, sleep: func(d time.Duration) { slept = append(slept, d); tick = tick.Add(d) }}}
	a.http = newClient(server.URL)
	a.http.HTTP.Transport = wrap(a.http.HTTP.Transport)
	code = a.main(append([]string{"deploy"}, args...))
	named := strings.NewReplacer(server.URL, "http://localhost:7777", strings.TrimPrefix(server.URL, "http://"), "localhost:7777")
	return named.Replace(o.String()), named.Replace(e.String()), code, slept
}

// cleanDeploy is a host whose run passes its review and goes live in preview, its timeline one page.
func cleanDeploy(t *testing.T) *gateHost {
	t.Helper()
	return &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true),
		project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}, start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}}
}

// Two minutes into a deploy on the test cluster, one poll got no answer, the CLI exited 1, and the runner took the app
// offline, which stopped a deploy that was going fine (#2721). A poll the host doesn't answer, or whose
// connection is refused, is sent again after a second, and the deploy then ends as it would have: live, exit 0.
func TestADeployRidesOutOneSlowPoll(t *testing.T) {
	t.Run("a poll the host stalls past the wait for its headers", func(t *testing.T) {
		h := cleanDeploy(t)
		var stalled atomic.Bool // the poll sent again may reach the host before the stalled one's handler returns
		out, errOut, code, slept := deployingThrough(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/timeline") && stalled.CompareAndSwap(false, true) {
				<-r.Context().Done() // until the client gives up on it and hangs up
				return
			}
			h.ServeHTTP(w, r)
		}), func(rt http.RoundTripper) http.RoundTripper {
			rt.(*http.Transport).ResponseHeaderTimeout = 500 * time.Millisecond // the client's own 30 s, made short: every read of the deploy has it, so not so short a loaded runner times out an ordinary one
			return rt
		})
		if code != exitOK || !strings.Contains(out, "✓ Deploy") || !strings.Contains(out, liveURL) || errOut != "" || !slices.Equal(slept, []time.Duration{time.Second}) {
			t.Errorf("exit %d, slept %v; stdout:\n%s\nstderr:\n%s", code, slept, out, errOut)
		}
	})
	for name, err := range map[string]error{
		"a poll HTTP/2 waited out on":         h2Timeout{},
		"a poll whose connection was refused": &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	} {
		t.Run(name, func(t *testing.T) {
			f := &flaky{down: func(n int) bool { return n == 1 }, err: err}
			out, errOut, code, slept := deployingThrough(t, cleanDeploy(t), func(rt http.RoundTripper) http.RoundTripper { f.next = rt; return f })
			if code != exitOK || !strings.Contains(out, "✓ Deploy") || !strings.Contains(out, liveURL) || errOut != "" {
				t.Errorf("exit %d; stdout:\n%s\nstderr:\n%s", code, out, errOut)
			}
			if !slices.Equal(f.polls, []string{"0", "0"}) || !slices.Equal(slept, []time.Duration{time.Second}) {
				t.Errorf("polled %q and slept %v: the unanswered poll is sent again, the same, a second later", f.polls, slept)
			}
		})
	}
}

// A host that answers with an error status did answer: the poll is not sent again, and the deploy fails as before.
func TestAPollTheHostRefusedWithAStatusIsNotSentAgain(t *testing.T) {
	h := cleanDeploy(t)
	polls := 0
	_, errOut, code, slept := deployingThrough(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			polls++
			h.answer(w, reply{http.StatusInternalServerError, `{"error":"internal","message":"internal error"}`})
			return
		}
		h.ServeHTTP(w, r)
	}), func(rt http.RoundTripper) http.RoundTripper { return rt })
	if code != exitError || polls != 1 || len(slept) != 0 || !strings.Contains(errOut, "internal error") {
		t.Errorf("exit %d, %d polls, slept %v; stderr:\n%s", code, polls, slept, errOut)
	}
}

// When the host still doesn't answer after 2 minutes of asking again, the deploy goes on in Canyon: deploy exits
// 3, never 1, and says so, a person's lines on stderr and an agent's one block on stdout (cli.md §3.5, #2721).
func TestADeployThatLosesItsHostIsStillRunning(t *testing.T) {
	waits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for _, c := range []struct{ golden, format string }{{"deploy-still-running.txt", "human"}, {"ai-still-running.txt", "ai"}} {
		t.Run(c.format, func(t *testing.T) {
			want := golden(t, c.golden) // before deployingThrough leaves this directory
			f := &flaky{down: func(n int) bool { return n > 1 }, err: h2Timeout{}}
			h := cleanDeploy(t)
			h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}],"next":41}` // the review goes on
			out, errOut, code, slept := deployingThrough(t, h, func(rt http.RoundTripper) http.RoundTripper { f.next = rt; return f }, "--format", c.format)
			printed, quiet := errOut, out
			if c.format == "ai" {
				printed, quiet = out, errOut
			}
			if code != exitStillRunning || printed != want {
				t.Errorf("exit %d, want %d; printed:\n%s\ntestdata/%s wants\n%s", code, exitStillRunning, printed, c.golden, want)
			}
			if c.format == "ai" && quiet != "" {
				t.Errorf("stderr under ai:\n%s", quiet)
			}
			if c.format == "human" && strings.Contains(out+errOut, "Is the demo running?") {
				t.Errorf("a host that answered a minute ago is not a demo that never started:\n%s", errOut)
			}
			if !slices.Equal(slept[1:], waits) { // the first is the poll's own second, after the page that answered
				t.Errorf("slept %v, want a second and then %v", slept, waits)
			}
			if len(f.polls) != 1+len(waits)+1 || slices.ContainsFunc(f.polls[1:], func(after string) bool { return after != "41" }) {
				t.Errorf("polled %q: each unanswered poll is the same one, sent again", f.polls)
			}
		})
	}
}

// Past the deploy's ceiling the deploy goes on in Canyon too, before its review is in as after: exit 3, an agent's
// block saying why, a person's lines as before (cli.md §3.5, #2721).
func TestADeployPastItsCeilingIsStillRunning(t *testing.T) {
	for _, format := range []string{"human", "ai"} {
		t.Run(format, func(t *testing.T) {
			h := cleanDeploy(t)
			h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}],"next":41}` // the review goes on
			server := httptest.NewServer(h)
			t.Cleanup(server.Close)
			t.Chdir(projectDir(t))
			tick := time.Date(2026, 9, 2, 9, 31, 4, 0, time.UTC)
			env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
			var o, e strings.Builder
			a := &app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] },
				clock: clock{now: func() time.Time { return tick }, sleep: func(time.Duration) { tick = tick.Add(10 * time.Minute) }}}
			code := a.main([]string{"deploy", "--format", format})
			said, quiet := e.String(), o.String()
			want := "\n  The deploy keeps going without this terminal:\n"
			if format == "ai" {
				said, quiet, want = o.String(), e.String(), "\nreason: the deploy is still running after 35 minutes, the longest this command waits.\n"
			}
			if code != exitStillRunning || !strings.Contains(said, want) || format == "ai" && (quiet != "" || !strings.HasSuffix(said, "\nexit code: 3\n")) {
				t.Errorf("exit %d; stdout:\n%s\nstderr:\n%s", code, o.String(), e.String())
			}
		})
	}
}

// A host that answers between two stalls starts the next one afresh: the waits begin at a second again, and the 2
// minutes run from the first poll of the second stall, never from the first stall's (#2721).
func TestAPollThatAnswersBetweenTwoStallsStartsTheSecondAfresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":41}`)
	}))
	t.Cleanup(server.Close)
	c := newClient(server.URL)
	f := &flaky{next: c.HTTP.Transport, down: func(n int) bool { return n != 2 }, err: h2Timeout{}} // the second poll alone is answered
	c.HTTP.Transport = f
	tick := time.Date(2026, 9, 2, 9, 31, 4, 0, time.UTC)
	var slept []time.Duration
	clk := clock{now: func() time.Time { return tick }, sleep: func(d time.Duration) { slept = append(slept, d); tick = tick.Add(d) }}
	_, err := poll(context.Background(), c, "gr_8f2a", clk, deployCeiling, nil)
	var lost *lostHost
	want := []time.Duration{time.Second, time.Second, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !errors.As(err, &lost) || !slices.Equal(slept, want) {
		t.Errorf("ended %v having slept %v, want the host lost after %v: one retry, the poll's own second, then the second stall from a second", err, slept, want)
	}
}
