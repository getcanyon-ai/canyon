package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// egress-control does not start on a setting out of shape (fail closed), naming the setting: a check address
// other than loopback, a metrics address that is no IPv4 address and port or, on Docker, every address, no count of replicas to share the rate
// limit by or one outside 1 to 30, a policy URL that is not the server's, an https:// one without the token's file
// or the pinned certificate, a token file that cannot be read, or a certificate that is no PEM.
func TestEgressControlRefusesSettingsOutOfShape(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("a-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := map[string]string{
		"CANYON_EGRESS_CONTROL_ADDR": "127.0.0.1:7783",
		"CANYON_EGRESS_REPLICAS":     "3",
		"CANYON_EGRESS_POLICY_URL":   "http://10.214.0.2:7782/egress/policy",
	}
	kube := "https://canyon-server.canyon.svc:7782/egress/policy"
	for _, c := range []struct {
		set  map[string]string
		want string
	}{
		{map[string]string{"CANYON_EGRESS_CONTROL_ADDR": "0.0.0.0:7783"}, "CANYON_EGRESS_CONTROL_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_ADDR": "10.214.0.3:7783"}, "CANYON_EGRESS_CONTROL_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_ADDR": "[::1]:7783"}, "CANYON_EGRESS_CONTROL_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "localhost:7784"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "[::]:7784"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "0.0.0.0:0"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "7784"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"},
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "0.0.0.0:7784"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"},                                                                     // on Docker, the http:// URL's
		{map[string]string{"CANYON_EGRESS_CONTROL_METRICS_ADDR": "0.0.0.0:7784", "CANYON_EGRESS_POLICY_URL": "HTTP://10.214.3.2:7782/egress/policy"}, "CANYON_EGRESS_CONTROL_METRICS_ADDR"}, // a scheme in capitals is Docker's too
		{map[string]string{"CANYON_EGRESS_REPLICAS": ""}, "CANYON_EGRESS_REPLICAS"},
		{map[string]string{"CANYON_EGRESS_REPLICAS": "0"}, "CANYON_EGRESS_REPLICAS"},
		{map[string]string{"CANYON_EGRESS_REPLICAS": "31"}, "CANYON_EGRESS_REPLICAS"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": ""}, "CANYON_EGRESS_POLICY_URL"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": "http://10.214.0.2:7782/"}, "CANYON_EGRESS_POLICY_URL"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": kube}, "CANYON_EGRESS_POLICY_URL"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": kube, "CANYON_EGRESS_POLICY_CERT_FILE": filepath.Join(dir, "none")}, "CANYON_EGRESS_POLICY_CERT_FILE"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": kube, "CANYON_EGRESS_POLICY_CERT_FILE": notPEM, "CANYON_EGRESS_POLICY_TOKEN_FILE": token}, "CANYON_EGRESS_POLICY_CERT_FILE"},
		{map[string]string{"CANYON_EGRESS_POLICY_URL": kube, "CANYON_EGRESS_POLICY_CERT_FILE": notPEM, "CANYON_EGRESS_POLICY_TOKEN_FILE": filepath.Join(dir, "none")}, "CANYON_EGRESS_POLICY_TOKEN_FILE"},
	} {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		for k, v := range c.set {
			env[k] = v
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := egressControl(ctx, func(k string) string { return env[k] }, nil)
		if err == nil || !strings.HasPrefix(err.Error(), c.want+":") {
			t.Errorf("%v: %v, want refused naming %s", c.set, err, c.want)
		}
	}
}

// With its settings in shape egress-control starts, pulls, tells the probe it is ready once the server has
// answered, serves its metrics where CANYON_EGRESS_CONTROL_METRICS_ADDR says, and stops when its context ends.
func TestEgressControlStartsAndStops(t *testing.T) {
	policy, _ := json.Marshal(map[string]any{"format": 1, "epoch": strings.Repeat("c", 32), "seq": 1, "public_url": "https://canyon.meridian.example",
		"callers": []any{}, "envs": []any{}, "platform": []any{}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(policy) }))
	defer srv.Close()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, metrics := ln.Addr().String(), mln.Addr().String()
	_, _ = ln.Close(), mln.Close()
	env := map[string]string{"CANYON_EGRESS_CONTROL_ADDR": addr, "CANYON_EGRESS_REPLICAS": "1", "CANYON_EGRESS_POLICY_URL": srv.URL + "/egress/policy",
		"CANYON_EGRESS_CONTROL_METRICS_ADDR": metrics}
	getenv := func(k string) string { return env[k] }
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- egressControl(ctx, getenv, nil) }()
	deadline := time.Now().Add(10 * time.Second)
	for egressControl(context.Background(), getenv, []string{"probe"}) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the probe never said ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + metrics + "/metrics") //nolint:noctx // the test's own listener
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "\ncanyon_egress_policy_age_seconds ") {
		t.Errorf("GET /metrics on %s: %d\n%s", metrics, resp.StatusCode, body)
	}
	if err := egressControl(context.Background(), getenv, []string{"probe", "now"}); err == nil {
		t.Error("an argument but probe: taken, want refused")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("stopped with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
}

// The policy's listener (contracts/routes.md, Egress control): CANYON_EGRESS_POLICY_ADDR when set; unset, on Docker
// with the receiver on, the receiver's address on 7782, which the bundled compose file and the holder's floor rely on;
// on Kubernetes, or with no receiver, none. A receiver on 7782 leaves the policy no port: the boot fails, naming it.
func TestEgressPolicyListensBesideTheReceiverOnDocker(t *testing.T) {
	for _, c := range []struct {
		set, receiver string
		kube          bool
		want          string
	}{
		{"", "10.214.3.2:7781", false, "10.214.3.2:7782"},
		{"10.214.3.2:7790", "10.214.3.2:7781", false, "10.214.3.2:7790"},
		{"", "", false, ""},
		{"", "0.0.0.0:7781", true, ""},
		{"0.0.0.0:7782", "0.0.0.0:7781", true, "0.0.0.0:7782"},
	} {
		got, err := egressPolicyAddr(c.set, c.receiver, c.kube)
		if err != nil || got != c.want {
			t.Errorf("set %q, receiver %q, kube %v: %q %v; want %q", c.set, c.receiver, c.kube, got, err, c.want)
		}
	}
	if got, err := egressPolicyAddr("", "10.214.3.2:7782", false); err == nil || !strings.HasPrefix(err.Error(), "CANYON_EGRESS_POLICY_ADDR:") {
		t.Errorf("a receiver on 7782: %q %v; want refused, naming CANYON_EGRESS_POLICY_ADDR", got, err)
	}
}
