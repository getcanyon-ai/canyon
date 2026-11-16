package egresscontrol

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The metrics (design/EGRESS.md §7): the policy's age once one is held, the calls refused by reason, and the
// refetches by what they found — and never an app, a destination or an address.
func TestMetricsTellThePolicysAgeRefusalsAndRefetches(t *testing.T) {
	r := newReplica(t, 1, &server{policy: claims(t, 42)})
	scrape := func() string {
		t.Helper()
		var b strings.Builder
		if err := r.WriteMetrics(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if m := scrape(); strings.Contains(m, "\ncanyon_egress_policy_age_seconds ") {
		t.Errorf("a policy's age before any is held:\n%s", m)
	}
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r.clock.add(7 * time.Second)
	if m := scrape(); !strings.Contains(m, "\ncanyon_egress_policy_age_seconds 7.000\n") {
		t.Errorf("the policy's age 7 s after the server's answer:\n%s", m)
	}
	refused(t, "an address no one holds", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	refused(t, "an IP literal", "invalid_destination", r.connect(t, preview, "10.0.0.1:443"))
	refused(t, "an IP literal again", "invalid_destination", r.connect(t, preview, "10.0.0.2:443"))
	refused(t, "off the allowlist", "not_allowed", r.connect(t, prod, "api.openai.com:443"))
	r.srv.set(true, nil)
	refused(t, "an address no one holds, the server down", "unknown_caller", r.connect(t, "172.28.9.10", "api.stripe.com:443"))
	m := scrape()
	for _, line := range []string{
		`canyon_egress_refusals_total{reason="unknown_caller"} 2`,
		`canyon_egress_refusals_total{reason="invalid_destination"} 2`,
		`canyon_egress_refusals_total{reason="not_allowed"} 1`,
		`canyon_egress_refusals_total{reason="rate_limited"} 0`,
		`canyon_egress_refusals_total{reason="port"} 0`,
		`canyon_egress_refetches_total{result="unknown"} 1`,
		`canyon_egress_refetches_total{result="failed"} 1`,
		`canyon_egress_refetches_total{result="known"} 0`,
		"# TYPE canyon_egress_refusals_total counter",
	} {
		if !strings.Contains(m, "\n"+line+"\n") {
			t.Errorf("the metrics lack %q:\n%s", line, m)
		}
	}
	for _, never := range []string{"claims-portal", "api.openai.com", "10.0.0.1", nobody, preview, prod} {
		if strings.Contains(m, never) {
			t.Errorf("the metrics carry %q:\n%s", never, m)
		}
	}
}

// ServeMetrics answers GET /metrics and nothing else, and stops when its context ends.
func TestServeMetricsAnswersMetricsAlone(t *testing.T) {
	r := started(t, 1)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- ServeMetrics(ctx, ln, r.Control) }()
	base := "http://" + ln.Addr().String()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + path) //nolint:noctx // the test's own listener
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get("/metrics"); code != http.StatusOK || !strings.Contains(body, "canyon_egress_policy_age_seconds ") {
		t.Errorf("GET /metrics: %d\n%s", code, body)
	}
	if code, _ := get("/"); code != http.StatusNotFound {
		t.Errorf("GET /: %d, want 404", code)
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

// A replica whose context ended — a pod's SIGTERM — answers no check: its Envoy answers 503 to every call from
// then, so a replica a rollout ends lets no name through while its successor starts, and the replicas that
// answer at once are never more than CANYON_EGRESS_REPLICAS counts (deploy/k8s/egress.yaml).
func TestAStoppedReplicaAnswersNoCheck(t *testing.T) {
	r := started(t, 1)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- Serve(ctx, ln, r.Control) }()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	check := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{})
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("a running replica: %v", err)
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	if err := check(); err == nil {
		t.Error("a stopped replica answered a check")
	}
}
