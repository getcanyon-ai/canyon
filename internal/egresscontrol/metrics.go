package egresscontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// reasons are every refusal's, and results every refetch's, so each series is there from the start.
var (
	reasons = []string{"unknown_caller", "invalid_destination", "port", "rate_limited", "not_allowed"}
	results = []string{"known", "unknown", "failed"}
)

// WriteMetrics writes the replica's metrics in Prometheus' text format (design/EGRESS.md §7): its policy's age
// once it holds one, the calls it refused by reason, and its refetches by what they found. Never an app, a
// destination or an address: a scrape carries off nothing a log line would not.
func (c *Control) WriteMetrics(w io.Writer) error {
	c.mu.Lock()
	held, age := c.held != nil, c.now().Sub(c.current)
	refusals, refetched := map[string]uint64{}, map[string]uint64{}
	for k, v := range c.refusals {
		refusals[k] = v
	}
	for k, v := range c.refetched {
		refetched[k] = v
	}
	c.mu.Unlock()
	out := "# HELP canyon_egress_policy_age_seconds Seconds since the server last said the policy this replica holds is current; absent before it holds one.\n" +
		"# TYPE canyon_egress_policy_age_seconds gauge\n"
	if held {
		out += fmt.Sprintf("canyon_egress_policy_age_seconds %.3f\n", age.Seconds())
	}
	out += "# HELP canyon_egress_refusals_total Calls this replica refused, by the Canyon-Egress reason.\n" +
		"# TYPE canyon_egress_refusals_total counter\n"
	for _, r := range reasons {
		out += fmt.Sprintf("canyon_egress_refusals_total{reason=%q} %d\n", r, refusals[r])
	}
	out += "# HELP canyon_egress_refetches_total Refetches for a source address no caller held, by what they found.\n" +
		"# TYPE canyon_egress_refetches_total counter\n"
	for _, r := range results {
		out += fmt.Sprintf("canyon_egress_refetches_total{result=%q} %d\n", r, refetched[r])
	}
	_, err := io.WriteString(w, out)
	return err
}

// ServeMetrics answers GET /metrics on ln until ctx ends, and nothing else.
func ServeMetrics(ctx context.Context, ln net.Listener, c *Control) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = c.WriteMetrics(w)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
