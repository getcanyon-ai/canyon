package executor

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// A build that is running when SIGTERM arrives finishes; the process does not exit under it.
func TestServeDrainsAnInFlightBuildBeforeReturning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("built"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln) }()
	type reply struct {
		code int
		err  error
	}
	got := make(chan reply, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/build")
		if err != nil {
			got <- reply{err: err}
			return
		}
		_ = resp.Body.Close()
		got <- reply{code: resp.StatusCode}
	}()
	<-started
	cancel() // SIGTERM
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while a build was still in flight: the process would exit and kill it", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if r := <-got; r.err != nil || r.code != 200 {
		t.Fatalf("the in-flight build must complete under SIGTERM: %v %d", r.err, r.code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
