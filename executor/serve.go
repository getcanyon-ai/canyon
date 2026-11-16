package executor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Serve serves on ln until ctx is done, then drains the calls in flight — a build, a scan — for up to
// 30s before returning: both executors' binaries end this way.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second) // the drain outlives the signal that started it
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained // Serve returns the moment the listener closes; the builds are still finishing
	return nil
}
