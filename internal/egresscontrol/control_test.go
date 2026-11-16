package egresscontrol

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

const (
	epoch   = "5f0c2a9e41d7b38c6e1f0a2b9d4c7e18"
	preview = "172.28.3.10" // claims-portal preview, observe
	prod    = "172.28.3.11" // claims-portal prod, allowlist
	bot     = "172.28.4.11" // intake-bot prod, an env the policy does not name
	nobody  = "172.28.9.9"
)

type caller struct{ Address, App, Env string }

type env struct {
	App     string   `json:"app"`
	Env     string   `json:"env"`
	Mode    string   `json:"mode"`
	Allowed []string `json:"allowed"`
	Known   []string `json:"known"`
}

func policyOf(t *testing.T, ep string, seq int, callers []caller, envs []env) []byte {
	t.Helper()
	cs := []map[string]string{}
	if envs == nil {
		envs = []env{}
	}
	for i := range envs {
		if envs[i].Known == nil {
			envs[i].Known = []string{}
		}
	}
	for _, c := range callers {
		cs = append(cs, map[string]string{"address": c.Address, "app": c.App, "env": c.Env})
	}
	raw, err := json.Marshal(map[string]any{"format": 1, "epoch": ep, "seq": seq, "public_url": "https://canyon.meridian.example",
		"callers": cs, "envs": envs, "platform": []string{"login.example-idp.com"}, "caps": map[string]int{"new_names": FleetNames}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func claims(t *testing.T, seq int) []byte {
	return policyOf(t, epoch, seq,
		[]caller{{preview, "claims-portal", "preview"}, {prod, "claims-portal", "prod"}, {bot, "intake-bot", "prod"}},
		[]env{{"claims-portal", "preview", "observe", []string{}, []string{"registry.npmjs.org"}},
			{"claims-portal", "prod", "allowlist", []string{"*.slack.com", "api.stripe.com"}, []string{"api.stripe.com"}}})
}

// server stands in for GET /egress/policy: a 304 only on an exact version, as the contract says.
type server struct {
	mu      sync.Mutex
	down    bool
	policy  []byte
	asks    []string // have, then caller
	resolve func(caller string) []byte
	hang    chan struct{} // closed, or nil: an ask waits on it, past its context

	leaves   map[string][]byte // GET /egress/leaf's 200 by destination; nil, a 503; absent, a 404
	leafAsks []string

	values     map[string][]byte // GET /egress/route-value's 200 by app/env/destination; nil, a 500; absent, a 404
	valueAsks  []string
	valueTimes []time.Duration // how long each ask had, its context's deadline from when it was asked; 0 for none
	valueHang  chan struct{}   // closed, or nil: a value's ask waits on it, past its context
}

// value stands in for GET /egress/route-value.
// It answers what it held when asked, however long it then hangs, and records how long each ask had.
func (s *server) value(ctx context.Context, app, env, destination string) ([]byte, error) {
	s.mu.Lock()
	k := app + "/" + env + "/" + destination
	s.valueAsks = append(s.valueAsks, k)
	if until, bounded := ctx.Deadline(); bounded {
		s.valueTimes = append(s.valueTimes, time.Until(until))
	} else {
		s.valueTimes = append(s.valueTimes, 0)
	}
	hang, down := s.valueHang, s.down
	raw, ok := s.values[k]
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	switch {
	case down:
		return nil, errors.New("connection refused")
	case !ok:
		return nil, errors.New("the server answered 404")
	case raw == nil:
		return nil, errors.New("the server answered 500")
	}
	return raw, nil
}

// leaf stands in for GET /egress/leaf.
func (s *server) leaf(ctx context.Context, destination string) ([]byte, error) {
	s.mu.Lock()
	s.leafAsks = append(s.leafAsks, destination)
	hang := s.hang
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.leaves[destination]
	switch {
	case s.down:
		return nil, errors.New("connection refused")
	case !ok:
		return nil, errors.New("the server answered 404")
	case raw == nil:
		return nil, errors.New("the server answered 503")
	}
	return raw, nil
}

func (s *server) fetch(ctx context.Context, have, caller string) ([]byte, error) {
	s.mu.Lock()
	s.asks = append(s.asks, have+" "+caller)
	hang := s.hang
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("connection refused")
	}
	if caller != "" && s.resolve != nil {
		s.policy = s.resolve(caller)
	}
	var p struct {
		Epoch string `json:"epoch"`
		Seq   int    `json:"seq"`
	}
	if err := json.Unmarshal(s.policy, &p); err == nil && have == fmt.Sprintf("%s:%d", p.Epoch, p.Seq) {
		return nil, nil
	}
	return s.policy, nil
}

func (s *server) set(down bool, policy []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
	if policy != nil {
		s.policy = policy
	}
}

func (s *server) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks[len(s.asks)-1]
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type replica struct {
	*Control
	srv   *server
	clock *clock
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// testInjector is Canyon Enterprise's interception for a test replica over srv on clk, logging to logs: set in a
// build with it (ee_injector_test.go), nil without it.
var testInjector func(srv *server, clk *clock, logs *syncBuffer) Injector

func newReplica(t *testing.T, replicas int, srv *server) replica {
	t.Helper()
	return newReplicaOn(t, replicas, srv, false)
}

// newReplicaOn is newReplica on Kubernetes when kube is true.
func newReplicaOn(t *testing.T, replicas int, srv *server, kube bool) replica {
	t.Helper()
	logs := &syncBuffer{}
	clk := &clock{at: time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC)}
	var inj Injector
	if testInjector != nil {
		inj = testInjector(srv, clk, logs)
	}
	c, err := New(srv.fetch, inj, replicas, receiverToken, kube, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	c.now = clk.now
	c.wait, c.every = 40*time.Millisecond, 10*time.Millisecond // the refetch's real bound has tests of its own, below
	return replica{c, srv, clk, logs}
}

func started(t *testing.T, replicas int) replica {
	t.Helper()
	r := newReplica(t, replicas, &server{policy: claims(t, 42)})
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return r
}

type answer struct {
	code     int32
	reason   string
	body     string
	headers  map[string]string
	metadata map[string]any
}

func (r replica) check(t *testing.T, source, method, scheme, host, path string) answer {
	t.Helper()
	return r.checkAs(t, source, method, scheme, host, path, "8121937455563917113")
}

func (r replica) checkAs(t *testing.T, source, method, scheme, host, path, stream string) answer {
	t.Helper()
	resp, err := r.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: source, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 41234}}}}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Id: stream, Method: method, Scheme: scheme, Host: host, Path: path,
			Headers: map[string]string{"authorization": "Bearer sk_live_" + strings.Repeat("h", 24)}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := answer{code: resp.GetStatus().GetCode(), metadata: resp.GetDynamicMetadata().AsMap(), headers: map[string]string{}}
	if d := resp.GetDeniedResponse(); d != nil {
		a.body = d.GetBody()
		for _, h := range d.GetHeaders() {
			a.headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
		a.reason = a.headers["Canyon-Egress"]
		if d.GetStatus().GetCode() != 403 {
			t.Errorf("a refusal answered %d, want 403", d.GetStatus().GetCode())
		}
	}
	return a
}

func (r replica) connect(t *testing.T, source, authority string) answer {
	t.Helper()
	return r.check(t, source, "CONNECT", "", authority, "")
}

func allowed(t *testing.T, what string, a answer) {
	t.Helper()
	if a.code != int32(codes.OK) || a.metadata["outcome"] != "allowed" {
		t.Errorf("%s: %v %s %v, want allowed", what, a.code, a.reason, a.metadata)
	}
}

func refused(t *testing.T, what, reason string, a answer) {
	t.Helper()
	if a.code != int32(codes.PermissionDenied) || a.reason != reason || a.metadata["outcome"] != "refused" || a.metadata["reason"] != reason {
		t.Errorf("%s: %v %q %v, want refused %s", what, a.code, a.reason, a.metadata, reason)
	}
}

// design/EGRESS.md §3.2, static stability — 30 s: a replica serves its last policy with no expiry for the envs
// and allowlists it knows, but forgets callers once it is more than 30 s old, so past 30 s without the server
// every call is unknown_caller; the envs and allowlists held decide again the moment the server answers.
func TestServesLastPolicyWhenServerDown(t *testing.T) {
	r := started(t, 1)
	r.srv.set(true, nil)
	r.clock.add(25 * time.Second)
	r.poll(context.Background())
	allowed(t, "prod's allowlist, the server down", r.connect(t, prod, "api.stripe.com:443"))
	allowed(t, "prod's wildcard, the server down", r.connect(t, prod, "hooks.slack.com:443"))
	refused(t, "a name off prod's allowlist, the server down", "not_allowed", r.connect(t, prod, "api.openai.com:443"))
	allowed(t, "preview's observe, the server down", r.connect(t, preview, "api.openai.com:443"))

	r.clock.add(6 * time.Second)
	refused(t, "31 s without the server", "unknown_caller", r.connect(t, prod, "api.stripe.com:443"))
	r.clock.add(2 * time.Hour)
	r.poll(context.Background())
	refused(t, "two hours without the server", "unknown_caller", r.connect(t, preview, "api.openai.com:443"))
	if r.Ready() || r.held.version() != epoch+":42" {
		t.Fatalf("two hours without the server: ready %v, want the policy held, with no expiry, and not ready: a replica cut off leaves the Service", r.Ready())
	}

	r.srv.set(false, nil) // back, unchanged: a 304 makes what the replica held current again
	r.poll(context.Background())
	if !r.Ready() {
		t.Error("the server back: not ready, want ready, its policy current again")
	}
	if got := r.srv.last(); got != epoch+":42 " {
		t.Errorf("the replica asked %q, want the version it held", got)
	}
	allowed(t, "prod's allowlist once the server answers", r.connect(t, prod, "api.stripe.com:443"))
	refused(t, "off prod's allowlist once the server answers", "not_allowed", r.connect(t, prod, "api.openai.com:443"))
}

// A replica keeps nothing across a start: it asks with have empty, holds no policy and is not ready until the
// server's 200, and refuses every call unknown_caller meanwhile; Probe tells an exec probe so.
func TestReadyOnlyOnceTheServerAnswers(t *testing.T) {
	r := newReplica(t, 1, &server{policy: claims(t, 42), down: true})
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r.Control) }()
	r.poll(context.Background())
	probe := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return Probe(ctx, ln.Addr().String())
	}
	if r.Ready() || probe() == nil {
		t.Error("no answer from the server: ready, want not")
	}
	refused(t, "no policy", "unknown_caller", r.connect(t, prod, "api.stripe.com:443"))
	r.srv.set(false, nil)
	r.poll(context.Background())
	if got := r.srv.last(); got != " " {
		t.Errorf("the first ask said %q, want have empty", got)
	}
	if !r.Ready() || probe() != nil {
		t.Error("the server's 200: not ready, want ready")
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve stopped with %v", err)
	}
}

// It denies only what it cannot answer: a caller of an env the policy does not name, and an address no caller
// holds, are unknown_caller, the policy fresh or stale.
func TestDeniesUnknownEnvEvenWhenStale(t *testing.T) {
	r := started(t, 1)
	refused(t, "a caller of an env the policy does not name", "unknown_caller", r.connect(t, bot, "api.stripe.com:443"))
	refused(t, "an address no caller holds", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	r.srv.set(true, nil)
	for _, after := range []time.Duration{10 * time.Second, 15 * time.Second, time.Hour} {
		r.clock.add(after)
		r.poll(context.Background())
		refused(t, "a caller of an env the policy does not name, stale", "unknown_caller", r.connect(t, bot, "api.stripe.com:443"))
		refused(t, "an address no caller holds, stale", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	}
	if a := r.connect(t, bot, "api.stripe.com:443"); a.metadata["app"] != nil || a.metadata["env"] != nil {
		t.Errorf("unknown_caller's metadata names %v %v, want no caller", a.metadata["app"], a.metadata["env"])
	}
}

// In every mode the check refuses IP literals, single labels, over-long names and ports but 80 and 443.
func TestRefusesIPLiteralInObserve(t *testing.T) {
	r := started(t, 1)
	for _, authority := range []string{"10.0.0.1:443", "169.254.169.254:443", "167772161:443", "0x7f.0.0.1:443", "0300.0250.1.1:443",
		"[::1]:443", "[::ffff:10.0.0.1]:443", "localhost:443", "api.stripe.com\u200b:443", "user@api.stripe.com:443",
		strings.Repeat("a", 64) + ".example.com:443"} {
		refused(t, authority, "invalid_destination", r.connect(t, preview, authority))
	}
	refused(t, "http://169.254.169.254/", "invalid_destination", r.check(t, preview, "GET", "http", "169.254.169.254", "/latest/meta-data/"))
	refused(t, "port 8443", "port", r.connect(t, preview, "api.stripe.com:8443"))
	refused(t, "port 0443", "port", r.connect(t, preview, "api.stripe.com:0443"))
	refused(t, "http on 8080", "port", r.check(t, preview, "GET", "http", "api.stripe.com:8080", "/"))
	allowed(t, "a name on 443", r.connect(t, preview, "api.stripe.com:443"))
	allowed(t, "a name on 80", r.check(t, preview, "GET", "http", "api.stripe.com", "/"))
	allowed(t, "an absolute-form https:// name", r.check(t, preview, "GET", "https", "api.stripe.com", "/v1/charges"))
}

// contracts/routes.md, The check: each refusal is 403, Canyon-Egress, text/plain and its one line, byte for byte.
func TestRefusalBodiesAreTheContracts(t *testing.T) {
	r := started(t, 1)
	for _, c := range []struct {
		a      answer
		reason string
		body   string
	}{
		{r.connect(t, prod, "api.openai.com:443"), "not_allowed", "Canyon refused this call: api.openai.com is not on claims-portal prod's outbound allowlist. A person on the app's team can allow it in Canyon: https://canyon.meridian.example/#/apps/claims-portal/outbound"},
		{r.connect(t, nobody, "api.openai.com:443"), "unknown_caller", "Canyon refused this call: Canyon does not know which app is calling."},
		{r.connect(t, prod, "user@10.0.0.1:443"), "invalid_destination", "Canyon refused this call: an app may call a host name on port 80 or 443, never an IP address."},
		{r.connect(t, prod, "api.stripe.com:22"), "port", "Canyon refused this call: an app may call out on port 80 or 443 only."},
	} {
		refused(t, c.reason, c.reason, c.a)
		if c.a.body != c.body || c.a.headers["content-type"] != "text/plain; charset=utf-8" || len(c.a.headers) != 2 {
			t.Errorf("%s: %q %v, want %q, text/plain", c.reason, c.a.body, c.a.headers, c.body)
		}
	}
}

// Callers are not static: once the policy is more than 30 s old a replica names no caller, so the refetch
// decides, and while the server cannot answer every call is unknown_caller (#2113).
func TestCallersAreForgottenOnceThePolicyIsThirtySecondsOld(t *testing.T) {
	r := started(t, 1)
	r.srv.set(true, nil)
	r.clock.add(30 * time.Second)
	allowed(t, "30 s old", r.connect(t, prod, "api.stripe.com:443"))
	r.clock.add(time.Second)
	refused(t, "31 s old, the server down", "unknown_caller", r.connect(t, prod, "api.stripe.com:443"))
	if got := r.srv.last(); got != epoch+":42 "+prod {
		t.Errorf("the refetch asked %q, want the version held and the caller", got)
	}
	r.srv.set(false, nil)
	allowed(t, "the server back, the refetch's 304 makes the policy current", r.connect(t, prod, "api.stripe.com:443"))
}

// An address the policy does not name is one refetch, however many calls wait on it; the server answers it a
// caller, under a new version, and the call is decided under it.
func TestUnknownCallerIsOneRefetch(t *testing.T) {
	r := started(t, 1)
	gate, asked, first := make(chan struct{}), make(chan struct{}), sync.Once{}
	r.srv.resolve = func(address string) []byte {
		first.Do(func() { close(asked) })
		<-gate
		return policyOf(t, epoch, 43, []caller{{address, "claims-portal", "preview"}}, []env{{"claims-portal", "preview", "observe", []string{}, nil}})
	}
	var wg sync.WaitGroup
	answers := make([]answer, 8)
	wg.Go(func() { answers[0] = r.connect(t, nobody, "api.stripe.com:443") })
	<-asked // the first is asking: every other joins it
	for i := 1; i < len(answers); i++ {
		wg.Go(func() { answers[i] = r.connect(t, nobody, "api.stripe.com:443") })
	}
	close(gate)
	wg.Wait()
	refetches := 0
	for _, a := range r.srv.asks {
		if strings.HasSuffix(a, " "+nobody) {
			refetches++
		}
	}
	if refetches != 1 {
		t.Errorf("%d refetches for one address, want one", refetches)
	}
	allowed(t, "the refetched caller", answers[0])
}

// A new pod calls out before its address reaches the cluster's API, so the server's first answers may rightly
// name no caller: the refetch asks again every 250 ms, and the call is allowed once an answer names it — here
// the third — never refused on the first.
func TestARefetchAsksAgainUntilTheCallerIsNamed(t *testing.T) {
	r := started(t, 1)
	r.wait, r.every = refetchWait, refetchEvery
	asks := 0
	r.srv.resolve = func(address string) []byte {
		if asks++; asks < 3 {
			return claims(t, 42)
		}
		return policyOf(t, epoch, 43, []caller{{address, "claims-portal", "preview"}}, []env{{"claims-portal", "preview", "observe", []string{}, nil}})
	}
	start := time.Now()
	allowed(t, "a caller named on the third ask", r.connect(t, nobody, "api.stripe.com:443"))
	if took := time.Since(start); asks != 3 || took < 2*refetchEvery || took > refetchWait {
		t.Errorf("%d asks in %v; want the third, two of %v apart, inside %v", asks, took, refetchEvery, refetchWait)
	}
}

// An address no one holds stops at the bound: at most 16 asks, single-flight, in 4 s, then unknown_caller, a
// 403, never Envoy's 503 (contracts/routes.md, The check).
func TestAnUnknownSourceStopsAtTheBound(t *testing.T) {
	r := started(t, 1)
	r.wait, r.every = refetchWait, refetchEvery
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			refused(t, "an address no one holds", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
		})
	}
	wg.Wait()
	asks := 0
	for _, a := range r.srv.asks {
		if strings.HasSuffix(a, " "+nobody) {
			asks++
		}
	}
	if took := time.Since(start); asks < 2 || asks > 16 || refetchWait/refetchEvery != 16 || took > refetchWait+500*time.Millisecond {
		t.Errorf("%d asks for one address from four checks in %v; want more than one, at most 16, inside %v", asks, took, refetchWait)
	}
}

// An address the server's answers named no one for is unknown_caller at once for 15 s, the server's confirm
// interval, with no ask: an address no one holds — a via-off app's, looping — costs one refetch per 15 s, never
// one per call (contracts/routes.md, The check). A refetch that failed is asked again at the next check.
func TestAnUnknownAddressCostsOneRefetchPer15s(t *testing.T) {
	r := started(t, 1)
	asks := func() int {
		n := 0
		for _, a := range r.srv.asks {
			if strings.HasSuffix(a, " "+nobody) {
				n++
			}
		}
		return n
	}
	refused(t, "an address no one holds", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	first := asks()
	for range 10 {
		refused(t, "the same address again", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	}
	r.clock.add(unknownFor - time.Second)
	refused(t, "14 s on", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	if first == 0 || asks() != first {
		t.Fatalf("%d asks after the first refetch's %d: want none in the 15 s after it", asks(), first)
	}
	r.clock.add(time.Second)
	refused(t, "15 s on", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	if asks() == first {
		t.Error("no ask 15 s on: want a refetch again")
	}
	r.srv.set(true, nil) // a refetch that failed leaves the address to be asked about at the next check
	refused(t, "another address, the server down", "unknown_caller", r.connect(t, "172.28.9.10", "api.stripe.com:443"))
	r.srv.set(false, nil)
	r.srv.resolve = func(address string) []byte {
		return policyOf(t, epoch, 43, []caller{{address, "claims-portal", "preview"}}, []env{{"claims-portal", "preview", "observe", []string{}, nil}})
	}
	allowed(t, "that address, the server back", r.connect(t, "172.28.9.10", "api.stripe.com:443"))
}

// A refetch the server answers 503 caller_unresolved — the executor could not be asked — is no answer: the
// address is asked about again at the next check, never remembered as no one's, so a new app is let through the
// moment the executor is back (contracts/routes.md, Egress control).
func TestCallerUnresolvedIsNeverRememberedAsNoOnes(t *testing.T) {
	var resolved atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address := r.URL.Query().Get("caller")
		switch {
		case address != "" && !resolved.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"caller_unresolved"}`))
		case address != "":
			_, _ = w.Write(policyOf(t, epoch, 43, []caller{{address, "claims-portal", "preview"}}, []env{{"claims-portal", "preview", "observe", []string{}, nil}}))
		default:
			_, _ = w.Write(claims(t, 42))
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := newReplica(t, 1, &server{})
	r.fetch = client.Fetch
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	refused(t, "a new app, the executor not answering", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	resolved.Store(true)
	allowed(t, "the same app at once, the executor back", r.connect(t, nobody, "api.stripe.com:443"))
}

// Past maxFlights refetches running at once a check is unknown_caller at once, with no ask: an app spreading its
// calls over many addresses never makes a replica ask the server for all of them together.
func TestRefetchesPastTheCapAreUnknownAtOnce(t *testing.T) {
	r := started(t, 1)
	r.mu.Lock()
	r.flights = maxFlights
	r.mu.Unlock()
	before := len(r.srv.asks)
	refused(t, "a refetch past the cap", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	if len(r.srv.asks) != before {
		t.Errorf("%d asks past the cap of %d refetches running, want none", len(r.srv.asks)-before, maxFlights)
	}
}

// A refetch the server does not answer is given up inside Envoy's 5 s for the check (deploy/egress/envoy.yaml): the
// call is unknown_caller, a 403, never Envoy's 503 — and a cold path to the executor has 4 s of it.
func TestARefetchIsBoundedInsideEnvoysWait(t *testing.T) {
	const envoyWaits = 5 * time.Second
	r := started(t, 1)
	r.wait, r.every = refetchWait, refetchEvery
	r.srv.hang = make(chan struct{})
	defer close(r.srv.hang)
	start := time.Now()
	refused(t, "the server silent", "unknown_caller", r.connect(t, nobody, "api.stripe.com:443"))
	if took := time.Since(start); took < refetchWait || took > envoyWaits-500*time.Millisecond {
		t.Errorf("the check took %v, want the refetch's %v and no more than Envoy's %v allows", took, refetchWait, envoyWaits)
	}
}

// A policy failing the fields egress-control knows, of a format it does not know, naming an address or an app
// env twice, or whose epoch is the one held and whose seq is lower, is refused whole, and the one held serves
// on; a field it does not know is ignored; another epoch is a restored database, taken whatever its seq.
func TestRefusesAPolicyWhole(t *testing.T) {
	with := func(f func(map[string]any)) []byte {
		var p map[string]any
		if err := json.Unmarshal(claims(t, 43), &p); err != nil {
			t.Fatal(err)
		}
		f(p)
		raw, _ := json.Marshal(p)
		return raw
	}
	for what, raw := range map[string][]byte{
		"a format it does not know":     with(func(p map[string]any) { p["format"] = 3 }),
		"no format":                     with(func(p map[string]any) { delete(p, "format") }),
		"a mode neither of the two":     policyOf(t, epoch, 44, nil, []env{{"claims-portal", "prod", "off", []string{}, nil}}),
		"an IP literal on an allowlist": policyOf(t, epoch, 45, nil, []env{{"claims-portal", "prod", "allowlist", []string{"10.0.0.1"}, nil}}),
		"an address named twice": policyOf(t, epoch, 46, []caller{{prod, "claims-portal", "prod"}, {prod, "claims-portal", "preview"}},
			[]env{{"claims-portal", "prod", "observe", []string{}, nil}, {"claims-portal", "preview", "observe", []string{}, nil}}),
		"an app env named twice":       policyOf(t, epoch, 47, nil, []env{{"claims-portal", "prod", "allowlist", []string{}, nil}, {"claims-portal", "prod", "observe", []string{}, nil}}),
		"an older seq, the same epoch": policyOf(t, epoch, 41, nil, nil),
		"Callers, which a case-blind decoder reads as callers": with(func(p map[string]any) {
			p["Callers"] = []any{map[string]any{"address": nobody, "app": "claims-portal", "env": "preview"}}
		}),
		"callers given twice": func() []byte {
			b := with(func(map[string]any) {})
			return append(b[:len(b)-1], []byte(`,"callers":[{"address":"`+nobody+`","app":"claims-portal","env":"preview"}]}`)...)
		}(),
		"not JSON": []byte("{"),
	} {
		r := started(t, 1)
		r.srv.set(false, raw)
		if err := r.pull(context.Background(), ""); err == nil || r.held.version() != epoch+":42" {
			t.Errorf("%s: %v, holding %s: want refused whole, the policy held before serving on", what, err, r.held.version())
		}
	}
	r := started(t, 1)
	r.srv.set(false, with(func(p map[string]any) {
		p["routes"] = []any{map[string]any{"destination": "api.stripe.com"}}
		p["envs"].([]any)[0].(map[string]any)["later"] = true
	}))
	if err := r.pull(context.Background(), ""); err != nil || r.held.version() != epoch+":43" {
		t.Errorf("fields it does not know: %v, want taken, ignoring them", err)
	}
	restored := strings.Repeat("a", 32)
	r.srv.set(false, policyOf(t, restored, 3, nil, nil))
	if err := r.pull(context.Background(), ""); err != nil || r.held.version() != restored+":3" {
		t.Errorf("another epoch, a lower seq: %v, want taken", err)
	}
}

// An answer to an ask made before the policy held was installed is dropped, whatever its version: a slow ask of
// the epoch before a restore never puts its callers back over the one a later ask took.
func TestAnAnswerToAnOlderAskIsDropped(t *testing.T) {
	r := started(t, 1)
	r.srv.hang = make(chan struct{})
	slow := make(chan error, 1)
	go func() { slow <- r.pull(context.Background(), "") }()
	for r.srv.asked() < 2 { // the slow ask is out
		time.Sleep(time.Millisecond)
	}
	restored := strings.Repeat("b", 32)
	fast := newReplica(t, 1, &server{policy: policyOf(t, restored, 1, nil, nil)})
	r.fetch = fast.srv.fetch
	if err := r.pull(context.Background(), ""); err != nil || r.held.version() != restored+":1" {
		t.Fatalf("the later ask: %v", err)
	}
	r.srv.set(false, policyOf(t, epoch, 50, []caller{{prod, "claims-portal", "prod"}}, []env{{"claims-portal", "prod", "observe", []string{}, nil}}))
	close(r.srv.hang)
	if err := <-slow; !errors.Is(err, errSuperseded) || r.held.version() != restored+":1" {
		t.Errorf("the earlier ask's answer: %v, holding %s, want dropped", err, r.held.version())
	}
}

func (s *server) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asks)
}

// 30 new names an app env a minute across every replica, never 30 for each: each replica lets through its
// share, 30 over the most replicas that run at once. A name the env knows — on its known[], allowed by its
// allowlist, a platform destination, or let through already — is not new, and a name refused counts nothing.
func TestRateLimitsNewNamesAcrossReplicas(t *testing.T) {
	for _, replicas := range []int{1, 3} {
		r := started(t, replicas)
		share := FleetNames / replicas
		for i := range share {
			allowed(t, fmt.Sprintf("new name %d of %d", i+1, share), r.connect(t, preview, fmt.Sprintf("n%d.example.com:443", i)))
		}
		a := r.connect(t, preview, "one-too-many.example.com:443")
		refused(t, "past the share", "rate_limited", a)
		if a.body != "Canyon refused this call: claims-portal preview called more than 30 new destinations this minute. It can call them from the next minute, or an admin can raise the limit in Settings › Outbound." {
			t.Errorf("body %q", a.body)
		}
		allowed(t, "a name let through before", r.connect(t, preview, "n0.example.com:443"))
		allowed(t, "a name on the env's known[]", r.connect(t, preview, "registry.npmjs.org:443"))
		allowed(t, "a platform destination", r.connect(t, preview, "login.example-idp.com:443"))
		r.clock.add(time.Minute)
		allowed(t, "the next minute", r.connect(t, preview, "one-too-many.example.com:443"))

		for i := range share + 3 { // prod: names its allowlist refuses take none of the minute's
			refused(t, "a name off prod's allowlist", "not_allowed", r.connect(t, prod, fmt.Sprintf("r%d.example.com:443", i)))
		}
		for i := range 5 {
			allowed(t, "prod's wildcard, never new", r.connect(t, prod, fmt.Sprintf("h%d.slack.com:443", i)))
		}
		allowed(t, "prod's allowlist, never new", r.connect(t, prod, "api.stripe.com:443"))
	}
	if _, err := New(nil, nil, 0, receiverToken, false, log.Default()); err == nil {
		t.Error("no replicas: started, want refused")
	}
}

// A replica as New makes it refuses not_allowed, now the platform destinations carry the front door's every
// endpoint (#2344): the 403, its metadata naming the caller, one log line. A mode that is neither is refused
// too, and a platform destination is allowed under the same allowlist.
func TestNotAllowedIsRefused(t *testing.T) {
	r := started(t, 1)
	a := r.connect(t, prod, "api.openai.com:443")
	refused(t, "off prod's allowlist", "not_allowed", a)
	if a.metadata["app"] != "claims-portal" || a.metadata["env"] != "prod" {
		t.Errorf("metadata %v, want the caller's app and env", a.metadata)
	}
	if want := `egress-control: refused: app "claims-portal" env "prod" destination "api.openai.com" reason not_allowed stream "8121937455563917113"`; strings.TrimSpace(r.logs.String()) != want {
		t.Errorf("logged %q, want %q", r.logs.String(), want)
	}
	r.held.envs[appEnv{"claims-portal", "preview"}] = envPolicy{mode: "off"}
	refused(t, "a mode that is neither", "not_allowed", r.connect(t, preview, "api.openai.com:443"))
	allowed(t, "a platform destination", r.connect(t, prod, "login.example-idp.com:443"))
}

// One log line per denial — app, env, destination, reason, Envoy's stream id, none past 64 bytes — and never
// the CheckRequest, whole or in part: an absolute-form call's path and query, its headers.
func TestCheckRequestNeverLogged(t *testing.T) {
	r := started(t, 1)
	token := "sk_live_" + strings.Repeat("q", 24)
	path := "/v1/charges?key=" + token
	for _, c := range []struct{ source, scheme, host string }{
		{prod, "https", "api.openai.com"}, {prod, "http", "10.0.0.1"}, {prod, "http", "api.openai.com:8080"}, {nobody, "https", "api.stripe.com"},
		{preview, "https", "api.stripe.com"},
	} {
		r.check(t, c.source, "POST", c.scheme, c.host, path)
	}
	r.checkAs(t, prod, "POST", "https", "api.openai.com", path, strings.Repeat("9", streamIDSize+1))
	logs := r.logs.String()
	for _, secret := range []string{token, "/v1/charges", "key=", "Bearer", "41234", strings.Repeat("9", streamIDSize+1)} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries %q:\n%s", secret, logs)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 5 || lines[0] != `egress-control: refused: app "claims-portal" env "prod" destination "api.openai.com" reason not_allowed stream "8121937455563917113"` ||
		lines[4] != `egress-control: refused: app "claims-portal" env "prod" destination "api.openai.com" reason not_allowed stream ""` {
		t.Errorf("want one line a denial, naming its app, env, destination, reason and stream, none past 64 bytes:\n%s", logs)
	}
}

// On Kubernetes the policy is asked over TLS to the certificate the server minted alone, with the projected
// token, read again at every ask; a server holding another certificate is never asked. No ask follows a
// redirect or goes through a proxy the environment names.
func TestClientAsksWithTheTokenOverThePinnedCertificate(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization")+" "+r.URL.RawQuery)
		switch r.URL.Query().Get("have") {
		case epoch + ":42":
			w.WriteHeader(http.StatusNotModified)
		case "redirect":
			http.Redirect(w, r, "/egress/policy?have=", http.StatusFound)
		default:
			_, _ = w.Write(claims(t, 42))
		}
	}))
	defer srv.Close()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	token := t.TempDir() + "/token"
	writeFile(t, token, "first\n")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := NewClient(srv.URL+"/egress/policy", token, cert)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := c.Fetch(context.Background(), "", ""); err != nil || !bytes.Equal(raw, claims(t, 42)) {
		t.Fatalf("a first ask: %s, %v", raw, err)
	}
	writeFile(t, token, "rotated")
	if raw, err := c.Fetch(context.Background(), epoch+":42", prod); err != nil || raw != nil {
		t.Fatalf("an ask of the version held: %s, %v, want a 304", raw, err)
	}
	if _, err := c.Fetch(context.Background(), "redirect", ""); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("a redirect: %v, want the 302 refused, never followed", err)
	}
	if want := []string{"Bearer first have=", "Bearer rotated caller=" + prod + "&have=" + epoch + "%3A42", "Bearer rotated have=redirect"}; !slices.Equal(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a server holding another certificate was asked") }))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()
	c.URL = other.URL + "/egress/policy"
	if _, err := c.Fetch(context.Background(), "", ""); err == nil {
		t.Error("another certificate: answered, want refused")
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(claims(t, 42)) }))
	defer plain.Close()
	docker, err := NewClient(plain.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker.Fetch(context.Background(), "", ""); err != nil {
		t.Errorf("Docker's ask, HTTP_PROXY set: %v, want it asked directly", err)
	}
	for what, err := range map[string]error{
		"no token file": func() error { _, err := NewClient(srv.URL+"/egress/policy", t.TempDir()+"/none", cert); return err }(),
		"no PEM":        func() error { _, err := NewClient(srv.URL+"/egress/policy", token, []byte("x")); return err }(),
		"https without": func() error { _, err := NewClient(srv.URL+"/egress/policy", "", nil); return err }(),
	} {
		if err == nil {
			t.Errorf("%s: a client, want refused", what)
		}
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// selfSigned is a certificate for 127.0.0.1 other than httptest's own.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
