package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lostThrough runs canyon with args in the working directory against host, the client's transport wrapped by wrap,
// on a clock that moves only when the CLI sleeps; it answers what was printed, the exit and each wait slept.
func lostThrough(t *testing.T, host string, wrap func(http.RoundTripper) http.RoundTripper, args ...string) (out, errs string, code int, slept []time.Duration) {
	t.Helper()
	tick := at
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	var o, e strings.Builder
	a := &app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] },
		clock: clock{now: func() time.Time { return tick }, sleep: func(d time.Duration) { slept = append(slept, d); tick = tick.Add(d) }}}
	a.http = newClient(host)
	a.http.HTTP.Transport = wrap(a.http.HTTP.Transport)
	code = a.main(args)
	named := strings.NewReplacer(host, "http://localhost:7777", strings.TrimPrefix(host, "http://"), "localhost:7777")
	return named.Replace(o.String()), named.Replace(e.String()), code, slept
}

// refusing refuses every timeline poll as a host that is down refuses a connection.
func refusing(rt http.RoundTripper) http.RoundTripper {
	return &flaky{next: rt, down: func(int) bool { return true }, err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
}

// The host's own answer while it restarts is not the run's (#2730): a gateway's 502, 503 or 504 from the proxy in front
// of it, a connection reset or cut mid-answer, or an HTTP/2 GOAWAY. Each poll it cuts short is sent again a second
// later, and so is the run's read after the poll, and the deploy ends as it would have: live, exit 0.
func TestATransientAnswerIsRetried(t *testing.T) {
	answering := func(status int) func(*gateHost) http.Handler {
		return func(h *gateHost) http.Handler {
			done := false
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/timeline") && !done {
					done = true
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "<html><body>"+http.StatusText(status)+"</body></html>") // the proxy's page, no envelope
					return
				}
				h.ServeHTTP(w, r)
			})
		}
	}
	through := func(err error) func(http.RoundTripper) http.RoundTripper {
		return func(rt http.RoundTripper) http.RoundTripper {
			return &flaky{next: rt, down: func(n int) bool { return n == 1 }, err: err}
		}
	}
	direct := func(rt http.RoundTripper) http.RoundTripper { return rt }
	for name, c := range map[string]struct {
		host func(*gateHost) http.Handler
		wrap func(http.RoundTripper) http.RoundTripper
	}{
		"a 502 from the proxy in front": {answering(http.StatusBadGateway), direct},
		"a 503 from the proxy in front": {answering(http.StatusServiceUnavailable), direct},
		"a 504 from the proxy in front": {answering(http.StatusGatewayTimeout), direct},
		"a connection reset":            {nil, through(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)})},
		"an answer cut short":           {nil, through(io.ErrUnexpectedEOF)},
		"a 503 on the run's read after it": {func(h *gateHost) http.Handler {
			done := false
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/gate-runs/gr_8f2a" && !done {
					done = true
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				h.ServeHTTP(w, r)
			})
		}, direct},
	} {
		t.Run(name, func(t *testing.T) {
			h := cleanDeploy(t)
			var host http.Handler = h
			if c.host != nil {
				host = c.host(h)
			}
			out, errOut, code, slept := deployingThrough(t, host, c.wrap)
			if code != exitOK || !strings.Contains(out, "✓ Deploy") || !strings.Contains(out, liveURL) || errOut != "" || !slices.Equal(slept, []time.Duration{time.Second}) {
				t.Errorf("exit %d, slept %v; stdout:\n%s\nstderr:\n%s", code, slept, out, errOut)
			}
		})
	}
}

// A promotion that loses its host goes on in Canyon, and production changes when it ends (#2730): exit 3, never 1,
// and never the demo's "Is the demo running?" for a host that answered a moment before. A person reads what to run
// and not to do on stderr; an agent reads one block.
func TestAPromotionThatLosesItsHost(t *testing.T) {
	for _, format := range []string{"human", "ai"} {
		t.Run(format, func(t *testing.T) {
			want := golden(t, "deploy-prod-ai-still-running.txt") // before the run moves into the app's directory
			h := clean(t, page(t), http.StatusAccepted, started)
			server := h.serve(t)
			t.Chdir(appDir(t))
			h.pages = listed(t, page(t), "pkg_7a1c", appTree(t))
			out, errs, code, _ := lostThrough(t, server.URL, refusing, "deploy", "--env", "prod", "--yes", "--format", format)
			if code != exitStillRunning || strings.Contains(out+errs, "Is the demo running?") {
				t.Fatalf("exit %d; stdout:\n%s\nstderr:\n%s", code, out, errs)
			}
			if format == "ai" && (out != want || errs != "") {
				t.Errorf("stderr %q; rendered\n%s\ntestdata/deploy-prod-ai-still-running.txt wants\n%s", errs, out, want)
			}
			if lines := "\n  Canyon didn't answer for 2 minutes (can't reach a Canyon host at http://localhost:7777).\n" +
				"  The promotion was still running when Canyon last answered, and canyon status says how it ends.\n" +
				"  Don't promote again or take production offline for this.\n  http://localhost:7777/#/apps/claims-portal\n"; format == "human" &&
				!strings.HasSuffix(strings.ReplaceAll(errs, server.URL, "http://localhost:7777"), lines) {
				t.Errorf("stderr:\n%s\nwant it to end\n%s", errs, lines)
			}
		})
	}
}

// A check that loses its host changes nothing: exit 3, and run canyon check again once Canyon answers, never a
// failure of the app's code (#2730).
func TestACheckThatLosesItsHost(t *testing.T) {
	for _, format := range []string{"human", "ai"} {
		t.Run(format, func(t *testing.T) {
			want := golden(t, "ai-check-still-running.txt")
			h := cleanDeploy(t)
			h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}],"next":41}`
			server := httptest.NewServer(h)
			t.Cleanup(server.Close)
			t.Chdir(projectDir(t))
			out, errs, code, _ := lostThrough(t, server.URL, func(rt http.RoundTripper) http.RoundTripper {
				return &flaky{next: rt, down: func(n int) bool { return n > 1 }, err: h2Timeout{}}
			}, "check", "--format", format)
			if code != exitStillRunning {
				t.Fatalf("exit %d; stdout:\n%s\nstderr:\n%s", code, out, errs)
			}
			if format == "ai" && (out != want || errs != "") {
				t.Errorf("stderr %q; rendered\n%s\ntestdata/ai-check-still-running.txt wants\n%s", errs, out, want)
			}
			if lines := "\n  Canyon didn't answer for 2 minutes (http://localhost:7777: no answer in 30s).\n" +
				"  The review was still running when Canyon last answered. A check changes nothing, so run canyon check again once Canyon answers.\n" +
				"  http://localhost:7777/#/apps/claims-portal\n"; format == "human" && errs != lines {
				t.Errorf("stderr:\n%q\nwant\n%q", errs, lines)
			}
		})
	}
}

// A rollback whose review loses its host puts nothing back: exit 3, production as it was, and no promotion sent
// (#2730).
func TestARollbackReviewThatLosesItsHost(t *testing.T) {
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")}
	server := h.serve(t)
	t.Chdir(appDir(t))
	out, errs, code, _ := lostThrough(t, server.URL, refusing, "deploy", "--env", "prod", "--version", "v1.3", "--yes")
	lines := "\n  Canyon didn't answer for 2 minutes (can't reach a Canyon host at http://localhost:7777).\n" +
		"  Nothing was put back in production: the review of v1.3 was still running when Canyon last answered.\n" +
		"  Run canyon deploy --env prod --version v1.3 again once Canyon answers.\n  http://localhost:7777/#/apps/claims-portal\n"
	if code != exitStillRunning || errs != lines || slices.ContainsFunc(h.posts, func(p string) bool { return strings.HasPrefix(p, "promotions") }) {
		t.Errorf("exit %d, sent %q; stdout:\n%s\nstderr:\n%q\nwant\n%q", code, h.posts, out, errs, lines)
	}
}

// A real HTTP/2 server that sends GOAWAY and then closes the connection under a request it had taken breaks it with
// net/http's own GOAWAY error, which is an interruption to wait out (#2730).
func TestAnHTTP2GoAwayIsAnInterruption(t *testing.T) {
	taken, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(taken)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	failed := make(chan error, 1)
	go func() {
		resp, err := server.Client().Get(server.URL + "/api/v1/gate-runs/gr_8f2a/timeline")
		if err == nil {
			_ = resp.Body.Close()
		}
		failed <- err
	}()
	<-taken
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _ = server.Config.Shutdown(ctx) }() // the GOAWAY, while the request is in flight
	<-ctx.Done()
	server.CloseClientConnections() // then the connection closes under it
	close(release)
	err := <-failed
	var cut *Interrupted
	if !strings.Contains(fmt.Sprintf("%T", errors.Unwrap(err)), "GoAway") || !errors.As(transportError("https://h", requestTimeout, err), &cut) {
		t.Errorf("%T %v: want net/http's GOAWAY error, read as an interruption", errors.Unwrap(err), err)
	}
}

// A certificate that doesn't verify is the connection's answer, never a restart: a poll it refuses is not sent again,
// and the deploy fails as it did (#2730). The error is a real handshake's, against a server no root here signs.
func TestATLSVerifyFailureIsNotRetried(t *testing.T) {
	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)
	_, refused := http.Get(untrusted.URL) //nolint:noctx // a handshake's error is all this asks for
	var verify *tls.CertificateVerificationError
	if !errors.As(refused, &verify) {
		t.Fatalf("the handshake ended %v, want a certificate that doesn't verify", refused)
	}
	h := cleanDeploy(t)
	h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}],"next":41}`
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	out, errs, code, slept := lostThrough(t, server.URL, func(rt http.RoundTripper) http.RoundTripper {
		return &flaky{next: rt, down: func(n int) bool { return n > 1 }, err: verify}
	}, "deploy")
	if code != exitError || len(slept) != 1 || !strings.Contains(errs, "certificate") || strings.Contains(out+errs, "didn't answer") {
		t.Errorf("exit %d, slept %v (the poll's own second, then nothing); stdout:\n%s\nstderr:\n%s", code, slept, out, errs)
	}
}

// A gateway's status is said beside the host that answered it, as every other reason names its host (#2730).
func TestAProxysStatusIsSaidBesideItsHost(t *testing.T) {
	want := golden(t, "deploy-still-running-503.txt")
	h := cleanDeploy(t)
	h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}],"next":41}`
	polls := 0
	_, errs, code, _ := deployingThrough(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			if polls++; polls > 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		h.ServeHTTP(w, r)
	}), func(rt http.RoundTripper) http.RoundTripper { return rt })
	if code != exitStillRunning || errs != want {
		t.Errorf("exit %d; stderr:\n%s\ntestdata/deploy-still-running-503.txt wants\n%s", code, errs, want)
	}
}

// Canyon said the run ended, then its result read went unanswered until the 2 minutes ran out: nothing says it still
// runs, so the lines say what is known, and the deploy exits 3, never 1 (#2730).
func TestARunWhoseResultCantBeReadEndedUnread(t *testing.T) {
	for _, c := range []struct{ golden, format string }{{"deploy-ended-unread.txt", "human"}, {"ai-ended-unread.txt", "ai"}} {
		t.Run(c.format, func(t *testing.T) {
			want := golden(t, c.golden)
			h := cleanDeploy(t)
			out, errs, code, _ := deployingThrough(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/gate-runs/gr_8f2a" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				h.ServeHTTP(w, r)
			}), func(rt http.RoundTripper) http.RoundTripper { return rt }, "--format", c.format)
			printed := errs
			if c.format == "ai" {
				printed = out
			}
			if code != exitStillRunning || printed != want || strings.Contains(out+errs, "still running") {
				t.Errorf("exit %d; printed:\n%s\ntestdata/%s wants\n%s", code, printed, c.golden, want)
			}
		})
	}
}

// canyon status reads the patch run's timeline for its platform row and waits on nobody (#2730): a page it isn't
// answered is asked once, never the 2 minutes a run's poll waits out, and a timeline that never ends is read to the
// ceiling a run's poll has, never on and on.
func TestStatusAsksThePatchTimelineOnce(t *testing.T) {
	for name, c := range map[string]struct {
		answer func(http.ResponseWriter)
		most   int // asks at most, on a clock that moves only when it sleeps
	}{
		"a page it isn't answered": {func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, 1},
		"a timeline that never ends": {func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"rows":[{"id":1,"ts":"2026-09-02T09:31:04Z","kind":"build.log","data":{"line":"x"}}],"next":1}`)
		}, int(platformCeiling/pollEvery) + 1},
	} {
		asked := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			asked++
			c.answer(w)
		}))
		tick := at
		clk := clock{now: func() time.Time { return tick }, sleep: func(d time.Duration) { tick = tick.Add(d) }}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := platformFactsOf(ctx, newClient(server.URL), clk, "/api/v1/projects/claims-portal", "prod", envPatch{State: "reverted", Run: "gr_p1"})
		timedOut := ctx.Err() != nil // the real-time guard ran out: it never stopped on its own
		cancel()
		server.Close()
		if err == nil || asked > c.most || timedOut || c.most > 1 && err.Error() != "the patch run's timeline didn't end within 10 seconds" {
			t.Errorf("%s: asked %d times (at most %d), ended %v", name, asked, c.most, err)
		}
	}
}

// A person's approval waits on nobody but them: a read the host cuts short while it restarts is asked again, and the
// promotion goes on once they approve, never exit 1 (#2730).
func TestAnApprovalWaitRidesOutATransientRead(t *testing.T) {
	h := approvalOn(t, page(t))
	h.reads = []string{`{"state":"pending"}`, `{"state":"approved","run":"gr_9c41","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`}
	server := h.serve(t)
	t.Chdir(appDir(t))
	h.pages = listed(t, page(t), "pkg_7a1c", appTree(t))
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	out, errs, code, _ := lostThrough(t, server.URL, func(rt http.RoundTripper) http.RoundTripper {
		return &flaky{next: rt, on: "/api/v1/promotion-approvals/", down: func(n int) bool { return n == 1 }, err: reset}
	}, "deploy", "--env", "prod", "--wait")
	if code != exitOK || !strings.Contains(out, "\n  ✓ Approved by Dana Whitfield\n") {
		t.Errorf("exit %d, stderr %q\n%s", code, errs, out)
	}
}

// A host lost while a person's approval is awaited leaves what production runs unknown: an approval starts the
// promotion on the host, so a person may have approved it in the silence. Exit 3, canyon status says, and never the
// demo's "Is the demo running?" for a host that answered a moment before, nor a bare gateway status (#2730).
func TestAnApprovalWaitThatLosesItsHost(t *testing.T) {
	for _, c := range []struct{ golden, format string }{{"deploy-prod-wait-lost.txt", "human"}, {"deploy-prod-ai-wait-lost.txt", "ai"}} {
		t.Run(c.format, func(t *testing.T) {
			want := golden(t, c.golden) // before the run moves into the app's directory
			h := approvalOn(t, page(t))
			h.reads = []string{`{"state":"pending"}`}
			server := h.serve(t)
			t.Chdir(appDir(t))
			h.pages = listed(t, page(t), "pkg_7a1c", appTree(t))
			out, errs, code, _ := lostThrough(t, server.URL, func(rt http.RoundTripper) http.RoundTripper {
				return &flaky{next: rt, on: "/api/v1/promotion-approvals/", down: func(int) bool { return true }, err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
			}, "deploy", "--env", "prod", "--wait", "--format", c.format)
			printed := errs
			if c.format == "ai" {
				printed = out
			}
			if code != exitStillRunning || printed != want || strings.Contains(out+errs, "Is the demo running?") {
				t.Errorf("exit %d; stdout:\n%s\nstderr:\n%q\ntestdata/%s wants\n%q", code, out, errs, c.golden, want)
			}
		})
	}
}
