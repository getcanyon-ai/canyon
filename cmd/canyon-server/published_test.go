package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// selfID is this test server's container: Docker's host name is its first 12 hex.
const selfID, self = "3f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c", "3f1c0b9a7e2a"

func bound(pairs ...string) []executor.Binding {
	b := []executor.Binding{}
	for pair := range slices.Chunk(pairs, 2) { // port, host address
		b = append(b, executor.Binding{Port: pair[0], HostIP: pair[len(pair)-1], HostPort: "7777"})
	}
	return b
}

// both is an answer whose created and held bindings are the same, as a running container's are.
func both(b []executor.Binding) executor.Published {
	return executor.Published{ID: selfID, Created: b, Held: b}
}

// Under demo, the gateway is served only when Docker holds a binding of the server's port now, and every binding
// of it, created or held, is on a loopback IP: every address, written "" or 0.0.0.0 or ::, a LAN address or a
// name is refused, naming the binding, and so is a port Docker holds no binding of (#2491).
func TestPublishedOnLoopbackAlone(t *testing.T) {
	for _, p := range []executor.Published{
		both(bound("7777/tcp", "127.0.0.1")),
		both(bound("7777/tcp", "127.0.0.2")),
		both(bound("7777/tcp", "::1")),
		both(bound("7777/tcp", "::ffff:127.0.0.1")), // a loopback address, as an IPv4-mapped IPv6 one
		both(bound("7777/tcp", "127.0.0.1", "7777/tcp", "::1")),
		both(bound("7777/tcp", "127.0.0.1", "7780/tcp", "0.0.0.0")), // another port's: no connection to the listener arrives through it
	} {
		if err := publishedOnLoopback(p, 7777); err != nil {
			t.Errorf("%+v: %v, want served", p, err)
		}
	}
	for _, c := range []struct {
		published executor.Published
		at        string
	}{
		{both(bound("7777/tcp", "0.0.0.0")), "on 0.0.0.0 (host port 7777)"},
		{both(bound("7777/tcp", "::")), "on :: (host port 7777)"},
		{both(bound("7777/tcp", "::ffff:0.0.0.0")), "on ::ffff:0.0.0.0"},
		{both(bound("7777/tcp", "")), "on every address (host port 7777)"},
		{both(bound("7777/tcp", "127.0.0.1", "7777/tcp", "0.0.0.0")), "on 0.0.0.0"}, // one beyond loopback is enough
		{both(bound("7777/tcp", "192.168.1.20")), "on 192.168.1.20"},
		{both(bound("7777/tcp", "localhost")), "on localhost"},
		// either list beyond loopback is enough: created on every address, held on loopback alone
		{executor.Published{ID: selfID, Created: bound("7777/tcp", ""), Held: bound("7777/tcp", "127.0.0.1")}, "on every address"},
		{executor.Published{ID: selfID, Created: bound("7777/tcp", "127.0.0.1"), Held: bound("7777/tcp", "127.0.0.1", "7777/tcp", "::")}, "on ::"},
	} {
		err := publishedOnLoopback(c.published, 7777)
		if err == nil || !strings.HasPrefix(err.Error(), "CANYON_AUTH=demo serves this machine alone, but Docker publishes its port 7777/tcp "+c.at) || !strings.Contains(err.Error(), "publish it on 127.0.0.1 alone") {
			t.Errorf("%+v: %v, want refused, naming the binding and the fix", c.published, err)
		}
	}
	for _, p := range []executor.Published{
		both(bound()),
		both(bound("7780/tcp", "127.0.0.1")),
		both(bound("7777/udp", "127.0.0.1")),
		{ID: selfID, Created: bound("7777/tcp", "127.0.0.1"), Held: bound()}, // created on loopback, held nowhere: nothing vouches for now
	} {
		if err := publishedOnLoopback(p, 7777); err == nil || !strings.Contains(err.Error(), "holds no binding of its port 7777/tcp") {
			t.Errorf("%+v: %v, want refused: CANYON_PUBLISHED_ADDR names a publish Docker does not hold", p, err)
		}
	}
}

// confirming runs the boot's check against ask onto open, answering the first refusal and the log.
func confirming(t *testing.T, open *atomic.Bool, limit time.Duration, ask func(context.Context) (executor.Published, error)) (<-chan error, *lockedLog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	refused, logs := make(chan error, 1), &lockedLog{}
	var done sync.WaitGroup
	done.Go(func() {
		confirmLoopback(ctx, ask, self, 7777, open, log.New(logs, "", 0), func(err error) { refused <- err }, time.Millisecond, limit)
	})
	t.Cleanup(func() { cancel(); done.Wait() })
	return refused, logs
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
	}
}

// An executor that cannot answer — not up, a 404 from one without the call or the Kubernetes executor's, a 502, an
// answer out of the contract's shape — keeps the gateway closed, never read as loopback, and is asked again until
// it answers; then a loopback publish opens it. Each reason is logged once.
func TestPublishedGatewayStaysClosedUntilTheExecutorAnswers(t *testing.T) {
	var asked atomic.Int32
	answers := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no such call"}`)
		},
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no such call"}`)
		},
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"docker"}`)
		},
		func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"bindings":[{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777"}]}`)
		},
		func(w http.ResponseWriter) { _, _ = io.WriteString(w, `127.0.0.1:7777`) },
	}
	lo := `[{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777"}]`
	var early atomic.Bool // the gateway open while the executor still answered out of shape
	open := new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(asked.Add(1)) - 1
		if n < len(answers) {
			if open.Load() {
				early.Store(true)
			}
			answers[n](w)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+selfID+`","created":`+lo+`,"held":`+lo+`}`)
	}))
	defer srv.Close()
	refused, logs := confirming(t, open, time.Second, (&executor.Client{Base: srv.URL}).Published)
	eventually(t, "the executor asked until it answers in shape", func() bool { return int(asked.Load()) > len(answers) })
	eventually(t, "the gateway open once the executor answers loopback", open.Load)
	if early.Load() {
		t.Error("the gateway opened before the executor answered in the contract's shape")
	}
	select {
	case err := <-refused:
		t.Errorf("refused: %v; an executor that cannot answer is asked again, never refused", err)
	default:
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 5 || !strings.HasSuffix(lines[0], "GET /published answered 404: no such call (#2491)") || !strings.HasSuffix(lines[1], "GET /published answered 502: docker (#2491)") ||
		lines[4] != "canyon-server: Docker publishes port 7777 on loopback alone: the host's connections through it are served (#2491)" {
		t.Errorf("the log:\n%s\nwant each reason once, then the opening", logs)
	}

	never := new(atomic.Bool)
	_, _ = confirming(t, never, time.Second, (&executor.Client{Base: "http://127.0.0.1:1"}).Published) // an executor that never comes up
	time.Sleep(50 * time.Millisecond)
	if never.Load() {
		t.Error("the gateway opened with no executor to answer")
	}
}

// An answer about another container — a canyon-server that is not this one, or a host name unread — keeps the
// gateway closed, logged once, and is asked again: it never vouches for this server, and never stops it.
func TestPublishedGatewayTakesOnlyItsOwnContainersAnswer(t *testing.T) {
	other := "9d2c41f07b3e8a5c6d1f0e2b4a7c9e8d3f5b1a6c0e4d2f8b7a9c3e5d1f0b2a4c"
	var asked atomic.Int32
	open := new(atomic.Bool)
	refused, logs := confirming(t, open, time.Second, func(context.Context) (executor.Published, error) {
		asked.Add(1)
		return executor.Published{ID: other, Created: bound("7777/tcp", "127.0.0.1"), Held: bound("7777/tcp", "127.0.0.1")}, nil
	})
	eventually(t, "another container's answer asked again, or taken", func() bool { return open.Load() || asked.Load() > 2 })
	if open.Load() {
		t.Error("the gateway opened on another container's answer")
	}
	select {
	case err := <-refused:
		t.Errorf("refused: %v; another container's answer is asked again", err)
	default:
	}
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `the executor answered for container 9d2c41f07b3e, and this server's host name is "3f1c0b9a7e2a": not this container`) {
		t.Errorf("the log:\n%s\nwant the mismatch once", got)
	}
	for _, host := range []string{"", "3f1c0b9a", "canyon-server", selfID} {
		if ours(selfID, host) == nil {
			t.Errorf("host name %q: taken as the container %s", host, selfID)
		}
	}
	if err := ours(selfID, self); err != nil {
		t.Errorf("its own container: %v", err)
	}
}

// Each ask is bounded: an executor or daemon that hangs is given up on and asked again, never waited on for good.
func TestPublishedGatewayAsksAgainPastAHungExecutor(t *testing.T) {
	var asked atomic.Int32
	open := new(atomic.Bool)
	_, logs := confirming(t, open, 20*time.Millisecond, func(ctx context.Context) (executor.Published, error) {
		if asked.Add(1) == 1 {
			<-ctx.Done() // the first ask hangs until its bound
			return executor.Published{}, ctx.Err()
		}
		return both(bound("7777/tcp", "127.0.0.1")), nil
	})
	eventually(t, "the gateway open once an ask after the hung one answers", open.Load)
	if !strings.Contains(logs.String(), "context deadline exceeded") {
		t.Errorf("the log:\n%s\nwant the hung ask named", logs)
	}
}

// A binding beyond loopback stops the server, naming it, and the gateway is never opened.
func TestPublishedGatewayRefusesABindingBeyondLoopback(t *testing.T) {
	open := new(atomic.Bool)
	refused, _ := confirming(t, open, time.Second, func(context.Context) (executor.Published, error) {
		return both(bound("7777/tcp", "127.0.0.1", "7777/tcp", "::")), nil
	})
	select {
	case err := <-refused:
		if !strings.Contains(err.Error(), "Docker publishes its port 7777/tcp on ::") {
			t.Errorf("refused with %v, want the binding named", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a publish on :: was not refused")
	}
	if open.Load() {
		t.Error("the gateway opened on a publish beyond loopback")
	}
}

// The check runs only where a gateway would be served, under demo behind a published port: elsewhere there is no
// gateway, and the executor is never asked.
func TestPublishedGatewayAskedOnlyBehindAPublish(t *testing.T) {
	ask := func(context.Context) (executor.Published, error) { return executor.Published{}, errors.New("asked") }
	if got := publishedGatewayOpen(t.Context(), false, ask, self, 7777, log.New(io.Discard, "", 0), func(error) {}); got != nil {
		t.Errorf("unpublished: %v, want no gateway", got)
	}
	got := publishedGatewayOpen(t.Context(), true, func(context.Context) (executor.Published, error) { return both(bound("7777/tcp", "127.0.0.1")), nil }, self, 7777, log.New(io.Discard, "", 0), func(error) {})
	if got == nil {
		t.Fatal("published: no gateway")
	}
	eventually(t, "the gateway open behind a loopback publish", got.Load)
}
