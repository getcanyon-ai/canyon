//go:build !noee

package egresscontrol

import (
	"bytes"
	"context"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	mutationv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	grpclogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/grpc/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	headermutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// leaf is destination's leaf as the server mints it at at: from 5 minutes before, a day long.
func (p *pki) leaf(t *testing.T, destination string, at time.Time) []byte {
	return marshal(t, p.answer(t, at.Add(-5*time.Minute), at.Add(24*time.Hour), destination))
}

func leavesOf(t *testing.T, p *pki, names []string, at time.Time) map[string][]byte {
	out := map[string][]byte{}
	for _, d := range names {
		out[d] = p.leaf(t, d, at)
	}
	return out
}

// interceptor is a replica holding policy, its listener built: as Envoy would be served it.
func interceptor(t *testing.T, policy []byte, leaves map[string][]byte) replica {
	t.Helper()
	r := newReplica(t, 1, &server{policy: policy, leaves: leaves})
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r.intercept(context.Background())
	return r
}

// serving starts r's gRPC server on loopback, as egress-control does, until the test ends.
func serving(t *testing.T, r replica) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r.Control) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve stopped with %v", err)
		}
	})
	return ln.Addr().String()
}

// envoy subscribes to a replica's aggregated discovery stream as Envoy does: LDS first, then SDS for the secrets
// the listener names, acknowledging each answer.
type envoy struct {
	t       *testing.T
	stream  discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	answers chan *discoveryv3.DiscoveryResponse
}

func subscribe(t *testing.T, addr string) *envoy {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = conn.Close() })
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := &envoy{t: t, stream: stream, answers: make(chan *discoveryv3.DiscoveryResponse, 16)}
	go func() {
		defer close(e.answers)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			e.answers <- resp
		}
	}()
	return e
}

// ask asks for typeURL's names, acknowledging after when it is not nil, and naming the node on a first ask.
func (e *envoy) ask(typeURL string, after *discoveryv3.DiscoveryResponse, names ...string) {
	e.t.Helper()
	req := &discoveryv3.DiscoveryRequest{TypeUrl: typeURL, ResourceNames: names, VersionInfo: after.GetVersionInfo(), ResponseNonce: after.GetNonce()}
	if after == nil {
		req.Node = &corev3.Node{Id: "canyon-egress", Cluster: "canyon-egress"}
	}
	if err := e.stream.Send(req); err != nil {
		e.t.Fatal(err)
	}
}

// next is the next answer of typeURL within 5 s, past any of another type: a snapshot that drops chains answers
// LDS and SDS both, in either order.
func (e *envoy) next(typeURL string) *discoveryv3.DiscoveryResponse {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if resp := e.pushed(time.Until(deadline)); resp.GetTypeUrl() == typeURL {
			return resp
		}
	}
	e.t.Fatalf("no %s answer in 5 s", typeURL)
	return nil
}

// pushed is the next answer within wait, nil when none came.
func (e *envoy) pushed(wait time.Duration) *discoveryv3.DiscoveryResponse {
	select {
	case resp := <-e.answers:
		return resp
	case <-time.After(wait):
		return nil
	}
}

// served is what an Envoy subscribing is answered: LDS, then SDS for the secrets its listener names.
type served struct {
	listeners *discoveryv3.DiscoveryResponse
	secrets   *discoveryv3.DiscoveryResponse // nil when the listener names none
	listener  *listenerv3.Listener           // nil when there is none
}

func (e *envoy) take() served {
	e.t.Helper()
	e.ask(resource.ListenerType, nil)
	var s served
	s.listeners = e.next(resource.ListenerType)
	e.ask(resource.ListenerType, s.listeners)
	s.listener = listenerIn(e.t, s.listeners)
	if names := sdsNames(e.t, s.listener); len(names) > 0 {
		e.ask(resource.SecretType, nil, names...)
		s.secrets = e.next(resource.SecretType)
		e.ask(resource.SecretType, s.secrets, names...)
	}
	return s
}

// listenerIn is the one listener an LDS answer holds, nil for none.
func listenerIn(t *testing.T, resp *discoveryv3.DiscoveryResponse) *listenerv3.Listener {
	t.Helper()
	switch len(resp.GetResources()) {
	case 0:
		return nil
	case 1:
	default:
		t.Fatalf("%d listeners served, want one at most", len(resp.GetResources()))
	}
	l := &listenerv3.Listener{}
	if err := resp.GetResources()[0].UnmarshalTo(l); err != nil {
		t.Fatal(err)
	}
	return l
}

// sdsNames is the secrets l's chains ask SDS for, in order.
func sdsNames(t *testing.T, l *listenerv3.Listener) []string {
	t.Helper()
	var names []string
	for _, c := range l.GetFilterChains() {
		for _, s := range tlsOf(t, c).GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
			names = append(names, s.GetName())
		}
	}
	return names
}

func tlsOf(t *testing.T, c *listenerv3.FilterChain) *tlsv3.DownstreamTlsContext {
	t.Helper()
	if c.GetTransportSocket().GetName() != "envoy.transport_sockets.tls" {
		t.Fatalf("chain %s: transport %q, want TLS", c.GetName(), c.GetTransportSocket().GetName())
	}
	ctx := &tlsv3.DownstreamTlsContext{}
	if err := c.GetTransportSocket().GetTypedConfig().UnmarshalTo(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func secretsOf(t *testing.T, resp *discoveryv3.DiscoveryResponse) map[string]*tlsv3.Secret {
	t.Helper()
	out := map[string]*tlsv3.Secret{}
	for _, a := range resp.GetResources() {
		s := &tlsv3.Secret{}
		if err := a.UnmarshalTo(s); err != nil {
			t.Fatal(err)
		}
		out[s.GetName()] = s
	}
	return out
}

// TestTwoReplicasServeIdenticalSnapshots: replicas holding the same policy and the same leaves serve their Envoys
// the same listener and secrets, byte for byte and under the same versions (design/EGRESS.md §4.2: built
// deterministically from the policy version so replicas never disagree), each inject destination once, in order,
// and the snapshot holds listener and secret resources alone.
func TestTwoReplicasServeIdenticalSnapshots(t *testing.T) {
	p := newPKI(t)
	policy, leaves := routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start)
	a, b := interceptor(t, policy, leaves), interceptor(t, policy, leaves)
	sa, sb := subscribe(t, serving(t, a)).take(), subscribe(t, serving(t, b)).take()

	if got := sdsNames(t, sa.listener); !slices.Equal(got, injected()) {
		t.Errorf("the listener's chains ask for %v, want each inject destination once, sorted: %v", got, injected())
	}
	if sa.listeners.GetVersionInfo() != sb.listeners.GetVersionInfo() || sa.secrets.GetVersionInfo() != sb.secrets.GetVersionInfo() {
		t.Errorf("the replicas serve versions %s/%s and %s/%s, want the same", sa.listeners.GetVersionInfo(), sa.secrets.GetVersionInfo(),
			sb.listeners.GetVersionInfo(), sb.secrets.GetVersionInfo())
	}
	if !bytes.Equal(sa.listeners.GetResources()[0].GetValue(), sb.listeners.GetResources()[0].GetValue()) {
		t.Errorf("the replicas serve different listeners, chains %v and %v", sdsNames(t, sa.listener), sdsNames(t, sb.listener))
	}
	secrets := secretsOf(t, sb.secrets)
	for name, s := range secretsOf(t, sa.secrets) {
		if !proto.Equal(s, secrets[name]) {
			t.Errorf("the replicas serve different secrets for %s", name)
		}
	}
	for _, r := range []replica{a, b} {
		got, err := r.snapshots.GetSnapshot(envoyNode)
		if err != nil {
			t.Fatal(err)
		}
		for typ, res := range got.(*cache.Snapshot).Resources {
			if n := len(res.Items); n != 0 && types.ResponseType(typ) != types.Listener && types.ResponseType(typ) != types.Secret {
				t.Errorf("the snapshot holds %d resources of type %d, want listener and secret resources alone", n, typ)
			}
		}
	}
}

// TestANewEnvWithNoNewDestinationLeavesTheListener: a new env, one whose routes name destinations routed already and
// one with none, moves the policy's version and nothing Envoy is served (design/EGRESS.md §4.2: a new env needs no
// xDS push), and asks for no leaf.
func TestANewEnvWithNoNewDestinationLeavesTheListener(t *testing.T) {
	p := newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	e := subscribe(t, serving(t, r))
	before := e.take()
	asked := len(r.srv.leafAsks)

	more := maps.Clone(inject)
	more["onboarding"], more["reports"] = []string{"api.stripe.com", "hooks.slack.com"}, []string{}
	r.srv.set(false, routed(t, 43, p.fingerprint, more))
	r.poll(context.Background())
	if r.held.version() != epoch+":43" {
		t.Fatalf("the replica holds %s, want the new envs' version", r.held.version())
	}
	r.intercept(context.Background())
	if resp := e.pushed(300 * time.Millisecond); resp != nil {
		t.Errorf("a new env with no new destination pushed %s version %s, want nothing pushed", resp.GetTypeUrl(), resp.GetVersionInfo())
	}
	after := subscribe(t, serving(t, r)).take()
	if after.listeners.GetVersionInfo() != before.listeners.GetVersionInfo() || !bytes.Equal(after.listeners.GetResources()[0].GetValue(), before.listeners.GetResources()[0].GetValue()) {
		t.Errorf("the listener moved from version %s to %s, want it unchanged", before.listeners.GetVersionInfo(), after.listeners.GetVersionInfo())
	}
	if after.secrets.GetVersionInfo() != before.secrets.GetVersionInfo() || len(r.srv.leafAsks) != asked {
		t.Errorf("the secrets moved from %s to %s and %d leaves were asked for, want neither", before.secrets.GetVersionInfo(), after.secrets.GetVersionInfo(), len(r.srv.leafAsks)-asked)
	}
}

// TestNoInterceptionListenerWithoutRootOrLeaf: nothing is intercepted without a policy, a signing root, an env
// trusting it (its routes named) and a leaf Envoy may serve under it. A destination whose leaf is unreadable has no
// chain, so an intercepted call finds none and its connection closes; with no chain there is no listener, and such a
// call is 503: never forwarded without its check, or in clear.
func TestNoInterceptionListenerWithoutRootOrLeaf(t *testing.T) {
	p, other := newPKI(t), newPKI(t)
	all, stripe := injected(), without(injected(), "api.stripe.com")
	t.Run("no policy", func(t *testing.T) {
		r := newReplica(t, 1, &server{policy: claims(t, 42), down: true})
		if s := subscribe(t, serving(t, r)).take(); s.listener != nil {
			t.Errorf("a replica holding no policy serves a listener: %v", sdsNames(t, s.listener))
		}
	})
	t.Run("no receiver token: an inner request would go unrecorded", func(t *testing.T) {
		r := newReplica(t, 1, &server{policy: routed(t, 42, p.fingerprint, inject), leaves: leavesOf(t, p, all, start)})
		r.token = ""
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		r.intercept(context.Background())
		if s := subscribe(t, serving(t, r)).take(); s.listener != nil || s.secrets != nil || len(r.srv.leafAsks) != 0 {
			t.Errorf("a replica with no receiver token serves chains for %v, and asked for leaves %v; want neither", sdsNames(t, s.listener), r.srv.leafAsks)
		}
		if v := r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:443"); v.set["x-canyon-intercept"] != "1" {
			t.Errorf("a CONNECT to a destination with a route, no listener served: %v, want it intercepted all the same, so it finds none and is 503", v)
		}
	})
	// stripe is every leaf readable but api.stripe.com's, which is the answer given.
	stripeIs := func(answer []byte) map[string][]byte {
		l := leavesOf(t, p, all, start)
		l["api.stripe.com"] = answer
		return l
	}
	edited := func(a map[string]string, k, v string) []byte { a[k] = v; return marshal(t, a) }
	stranger := other.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com")
	stranger["root"] = p.fingerprint // under the signing root by its word, and signed by no intermediate of it
	for _, c := range []struct {
		what    string
		policy  []byte
		leaves  map[string][]byte
		chained []string
		logged  string // the reason the leaf refused is logged for, "" when no leaf is asked for
	}{
		{"no signing root: the policy's routes intercept nothing", routed(t, 42, "", inject), leavesOf(t, p, all, start), nil, ""},
		{"no env trusts the signing root: the policy names no route", routed(t, 42, p.fingerprint, map[string][]string{"claims-portal": {}, "ledger": {}}), leavesOf(t, p, all, start), nil, ""},
		{"no leaf readable", routed(t, 42, p.fingerprint, inject), map[string][]byte{}, nil, "the server answered 404"},
		{"every leaf under another root", routed(t, 42, p.fingerprint, inject), leavesOf(t, other, all, start), nil, "a leaf under a root other than the policy's signing root"},
		{"one leaf the server cannot answer", routed(t, 42, p.fingerprint, inject), stripeIs(nil), stripe, "the server answered 503"},
		{"one leaf naming another destination", routed(t, 42, p.fingerprint, inject), stripeIs(p.leaf(t, "api.stripe.co", start)), stripe, "a leaf naming other than api.stripe.com alone"},
		{"one leaf naming a second name", routed(t, 42, p.fingerprint, inject),
			stripeIs(marshal(t, p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com", "dashboard.stripe.com"))), stripe, "a leaf naming other than api.stripe.com alone"},
		{"one leaf past its end", routed(t, 42, p.fingerprint, inject),
			stripeIs(marshal(t, p.answer(t, start.Add(-25*time.Hour), start.Add(-time.Second), "api.stripe.com"))), stripe, "a leaf not valid now, or whose not_after is not its end"},
		{"one leaf not valid yet", routed(t, 42, p.fingerprint, inject),
			stripeIs(marshal(t, p.answer(t, start.Add(time.Minute), start.Add(time.Hour), "api.stripe.com"))), stripe, "a leaf not valid now, or whose not_after is not its end"},
		{"one leaf whose not_after is not its end", routed(t, 42, p.fingerprint, inject),
			stripeIs(edited(p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com"), "not_after", start.Add(48*time.Hour).Format(time.RFC3339))), stripe,
			"a leaf not valid now, or whose not_after is not its end"},
		{"one leaf whose key is not its own", routed(t, 42, p.fingerprint, inject),
			stripeIs(edited(p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com"), "key", stranger["key"])), stripe, "a key that is not the leaf's"},
		{"one leaf another intermediate signed", routed(t, 42, p.fingerprint, inject),
			stripeIs(edited(maps.Clone(stranger), "chain", pemOf(p.inter))), stripe, "a leaf the chain's first certificate did not sign"},
		{"one chain whose certificates do not sign one another", routed(t, 42, p.fingerprint, inject),
			stripeIs(edited(p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com"), "chain", pemOf(p.inter)+pemOf(other.inter))), stripe,
			"a chain whose certificates do not sign one another in order"},
		{"one leaf with no chain", routed(t, 42, p.fingerprint, inject),
			stripeIs(edited(p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.com"), "chain", "")), stripe, "a chain that is not one to four certificates"},
		{"one leaf that is not JSON", routed(t, 42, p.fingerprint, inject), stripeIs([]byte("<html>")), stripe, "a leaf that is not JSON"},
	} {
		t.Run(c.what, func(t *testing.T) {
			r := interceptor(t, c.policy, c.leaves)
			s := subscribe(t, serving(t, r)).take()
			if got := sdsNames(t, s.listener); !slices.Equal(got, c.chained) {
				t.Errorf("chains for %v, want %v", got, c.chained)
			}
			if c.chained == nil && (s.listener != nil || s.secrets != nil) {
				t.Error("a listener served, want none")
			}
			if c.chained != nil {
				intercepted(t, s, c.chained)
			}
			if c.logged == "" && len(r.srv.leafAsks) != 0 {
				t.Errorf("leaves asked for %v, want none: the policy intercepts nothing", r.srv.leafAsks)
			}
			if want := `no leaf for "api.stripe.com", so it has no chain: ` + c.logged; c.logged != "" && !strings.Contains(r.logs.String(), want) {
				t.Errorf("logged %q, want %q", r.logs.String(), want)
			}
		})
	}
}

func without(all []string, d string) []string {
	return slices.DeleteFunc(slices.Clone(all), func(x string) bool { return x == d })
}

// intercepted holds of every chain s serves: TLS ends there for its destination alone, by the leaf SDS serves under
// its name, ALPN h2 and http/1.1, and its request goes on only past ext_authz #2 — the caller overwritten before
// it, no answer a 503, its context the destination — and then by TLS Envoy originates (contracts/routes.md,
// Interception). No chain matches anything else, and there is no default chain: a client speaking no TLS, or
// asking for another name, finds none.
func intercepted(t *testing.T, s served, destinations []string) {
	t.Helper()
	l := s.listener
	if l.GetName() != InterceptListener || l.GetInternalListener() == nil || l.GetAddress() != nil || l.GetDefaultFilterChain() != nil ||
		len(l.GetListenerFilters()) != 1 || l.GetListenerFilters()[0].GetName() != "envoy.filters.listener.tls_inspector" {
		t.Fatalf("the listener is %v, want the internal listener %s, the TLS inspector its one listener filter, and no default chain", l, InterceptListener)
	}
	secrets := secretsOf(t, s.secrets)
	if len(secrets) != len(destinations) {
		t.Errorf("%d secrets served, want one a destination: %d", len(secrets), len(destinations))
	}
	for i, c := range l.GetFilterChains() {
		d := destinations[i]
		if !proto.Equal(c.GetFilterChainMatch(), &listenerv3.FilterChainMatch{ServerNames: []string{d}}) {
			t.Errorf("chain %d matches %v, want server_names [%s] alone", i, c.GetFilterChainMatch(), d)
		}
		tls := tlsOf(t, c).GetCommonTlsContext()
		if sds := tls.GetTlsCertificateSdsSecretConfigs(); len(sds) != 1 || sds[0].GetName() != d || sds[0].GetSdsConfig().GetAds() == nil ||
			len(tls.GetTlsCertificates()) != 0 || !slices.Equal(tls.GetAlpnProtocols(), []string{"h2", "http/1.1"}) {
			t.Errorf("chain %s: TLS %v, want its leaf by SDS over ADS, named after it, and ALPN h2, http/1.1", d, tls)
		}
		if leaf := secrets[d].GetTlsCertificate(); !bytes.HasPrefix(leaf.GetCertificateChain().GetInlineBytes(), []byte("-----BEGIN CERTIFICATE-----")) ||
			!bytes.HasPrefix(leaf.GetPrivateKey().GetInlineBytes(), []byte("-----BEGIN PRIVATE KEY-----")) {
			t.Errorf("chain %s: no leaf served under its name", d)
		}
		if len(c.GetFilters()) != 1 || c.GetFilters()[0].GetName() != "envoy.filters.network.http_connection_manager" {
			t.Fatalf("chain %s: filters %v, want the connection manager alone", d, c.GetFilters())
		}
		hcm := &hcmv3.HttpConnectionManager{}
		if err := c.GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
			t.Fatal(err)
		}
		var filters []string
		for _, f := range hcm.GetHttpFilters() {
			filters = append(filters, f.GetName())
		}
		if want := []string{"envoy.filters.http.header_mutation", "envoy.filters.http.ext_authz", "envoy.filters.http.dynamic_forward_proxy", "envoy.filters.http.router"}; !slices.Equal(filters, want) {
			t.Fatalf("chain %s: filters %v, want %v", d, filters, want)
		}
		mutation := &headermutationv3.HeaderMutation{}
		if err := hcm.GetHttpFilters()[0].GetTypedConfig().UnmarshalTo(mutation); err != nil {
			t.Fatal(err)
		}
		overwrite := func(header, object string) *mutationv3.HeaderMutation {
			return &mutationv3.HeaderMutation{Action: &mutationv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
				Header:         &corev3.HeaderValue{Key: header, Value: "%FILTER_STATE(" + object + ":PLAIN)%"},
				AppendAction:   corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				KeepEmptyValue: true}}} // an object missing is an empty header, which still overwrites the app's
		}
		if want := (&headermutationv3.HeaderMutation{Mutations: &headermutationv3.Mutations{RequestMutations: []*mutationv3.HeaderMutation{
			overwrite("x-canyon-caller", "canyon.caller"), overwrite("x-canyon-destination", "canyon.destination")}}}); !proto.Equal(mutation, want) {
			t.Errorf("chain %s: header_mutation %v, want x-canyon-caller and x-canyon-destination overwritten from canyon.caller and canyon.destination alone, whatever the app sent", d, mutation)
		}
		check := &extauthzv3.ExtAuthz{}
		if err := hcm.GetHttpFilters()[1].GetTypedConfig().UnmarshalTo(check); err != nil {
			t.Fatal(err)
		}
		if check.GetFailureModeAllow() || check.GetStatusOnError().GetCode() != 503 || check.GetGrpcService().GetEnvoyGrpc().GetClusterName() != "control" {
			t.Errorf("chain %s: ext_authz %v, want egress-control asked, no answer a 503", d, check)
		}
		if want := (&matcherv3.ListStringMatcher{Patterns: []*matcherv3.StringMatcher{{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: "x-canyon-caller"}},
			{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: "x-canyon-destination"}}}}); !proto.Equal(check.GetAllowedHeaders(), want) {
			t.Errorf("chain %s: ext_authz #2 is sent the headers %v, want x-canyon-caller and x-canyon-destination alone", d, check.GetAllowedHeaders())
		}
		bound(t, d, hcm)
		if !proto.Equal(hcm.GetLocalReplyConfig(), localReplies(t, d)) || len(hcm.GetLocalReplyConfig().GetMappers()) != 2 {
			t.Errorf("chain %s: local replies %v, want the check's 503 line and upstream_failed alone", d, hcm.GetLocalReplyConfig())
		}
		vhosts := hcm.GetRouteConfig().GetVirtualHosts()
		per := &extauthzv3.ExtAuthzPerRoute{}
		if vhosts[0].GetTypedPerFilterConfig()["envoy.filters.http.ext_authz"].UnmarshalTo(per) != nil ||
			!maps.Equal(per.GetCheckSettings().GetContextExtensions(), map[string]string{"destination": d}) {
			t.Fatalf("chain %s: ext_authz #2's context %v, want {destination: %s} alone", d, per, d)
		}
		for _, rt := range vhosts[0].GetRoutes() {
			if rt.GetRoute().GetCluster() != InterceptedCluster || len(rt.GetTypedPerFilterConfig()) != 0 {
				t.Errorf("chain %s: a route to %q, want every request to %s, past the check", d, rt.GetRoute().GetCluster(), InterceptedCluster)
			}
			if !proto.Equal(rt.GetRoute().GetRetryPolicy(), &routev3.RetryPolicy{RetryOn: "connect-failure,reset-before-request"}) {
				t.Errorf("chain %s: retries %v, want connect-failure and reset-before-request alone: a request that reached the destination is never sent twice", d, rt.GetRoute().GetRetryPolicy())
			}
		}
	}
}

// TestServerGoneKeepsTheListenerAndLeaves: the server gone, a replica keeps serving the listener and the leaves it
// holds while they are valid, asking for each again from half its life on, and drops them only at their end
// (design/EGRESS.md §6: xDS gone keeps the last listeners and certificates; every call still checked).
func TestServerGoneKeepsTheListenerAndLeaves(t *testing.T) {
	p := newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	e := subscribe(t, serving(t, r))
	e.take()
	asked := len(r.srv.leafAsks)
	r.srv.set(true, nil)
	for _, at := range []time.Duration{time.Minute, 6 * time.Hour, 13 * time.Hour, 23 * time.Hour} {
		r.clock.add(start.Add(at).Sub(r.clock.now()))
		r.poll(context.Background())
		r.intercept(context.Background())
		if resp := e.pushed(100 * time.Millisecond); resp != nil {
			t.Errorf("%s without the server: %s version %s pushed, want what was served kept", at, resp.GetTypeUrl(), resp.GetVersionInfo())
		}
	}
	if got := len(r.srv.leafAsks) - asked; got != 2*len(injected()) {
		t.Errorf("%d leaves asked for without the server, want each again from half its life on, at 13 h and 23 h: %d", got, 2*len(injected()))
	}
	if want := `no new leaf for "api.stripe.com", so Envoy keeps the one it holds until 2026-10-20T10:00:30Z: connection refused`; strings.Count(r.logs.String(), want) != 1 {
		t.Errorf("logged %q, want %q once until it changes", r.logs.String(), want)
	}

	r.clock.add(time.Hour) // every leaf's end
	r.intercept(context.Background())
	if l := listenerIn(t, e.next(resource.ListenerType)); l != nil {
		t.Errorf("at its leaves' end the listener is still served, chains %v, want none", sdsNames(t, l))
	}
	if want := `the leaf for "api.stripe.com" ended, or was signed under a root no longer signing, so its chain is dropped`; strings.Count(r.logs.String(), want) != 1 {
		t.Errorf("logged %q, want %q once", r.logs.String(), want)
	}
	r.clock.add(time.Hour)
	r.intercept(context.Background())
	if n := strings.Count(r.logs.String(), "so its chain is dropped"); n != len(injected()) {
		t.Errorf("%d chains logged as dropped, want each once: %d", n, len(injected()))
	}
}

// TestLeavesFollowTheSigningRoot: a renewed leaf moves the secrets alone, never the listener; under a new signing
// root each leaf is asked for again, and one held under the old root is dropped at once, whatever its end.
func TestLeavesFollowTheSigningRoot(t *testing.T) {
	p, next := newPKI(t), newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	e := subscribe(t, serving(t, r))
	before := e.take()

	r.clock.add(13 * time.Hour) // past half of every leaf's life
	r.srv.mu.Lock()
	r.srv.leaves = leavesOf(t, p, injected(), r.clock.now())
	r.srv.mu.Unlock()
	r.intercept(context.Background())
	if renewed := e.next(resource.SecretType); renewed.GetVersionInfo() == before.secrets.GetVersionInfo() {
		t.Error("renewed leaves served under the version of the ones before")
	}
	if resp := e.pushed(100 * time.Millisecond); resp != nil {
		t.Errorf("renewed leaves pushed %s too, want the secrets alone", resp.GetTypeUrl())
	}

	r.srv.set(false, routed(t, 43, next.fingerprint, inject)) // a rotation: leaves are signed under the new root
	r.srv.mu.Lock()
	r.srv.leaves = map[string][]byte{"api.stripe.com": next.leaf(t, "api.stripe.com", r.clock.now())}
	asked := len(r.srv.leafAsks)
	r.srv.mu.Unlock()
	r.poll(context.Background())
	r.intercept(context.Background())
	if got := len(r.srv.leafAsks) - asked; got != len(injected()) {
		t.Errorf("under the new root %d leaves asked for, want each again: %d", got, len(injected()))
	}
	if got := sdsNames(t, listenerIn(t, e.next(resource.ListenerType))); !slices.Equal(got, []string{"api.stripe.com"}) {
		t.Errorf("under the new root the listener chains %v, want the one destination whose leaf is under it", got)
	}
}

// TestLeafKeyNeverLogged: no log line of egress-control carries a byte of a leaf's key, whatever the server answers
// (contracts/routes.md, GET /egress/leaf: no log line, error or audit row carries a key).
func TestLeafKeyNeverLogged(t *testing.T) {
	p, other := newPKI(t), newPKI(t)
	answers := map[string]map[string]string{
		"api.openai.com":    other.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.openai.com"),
		"api.stripe.com":    p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.stripe.co"),
		"hooks.slack.com":   p.answer(t, start.Add(-2*time.Hour), start.Add(-time.Hour), "hooks.slack.com"),
		"api.anthropic.com": p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.anthropic.com"),
		"api.twilio.com":    p.answer(t, start.Add(-time.Minute), start.Add(time.Hour), "api.twilio.com"),
	}
	answers["api.anthropic.com"]["key"] = answers["api.openai.com"]["key"]
	leaves := map[string][]byte{"sqs.eu-west-1.amazonaws.com": []byte(`{"root":"` + p.fingerprint + `","key":"sk_live_` + strings.Repeat("k", 24) + "\n" + `"}`)}
	keys := []string{"sk_live_" + strings.Repeat("k", 24)}
	for d, a := range answers {
		leaves[d] = marshal(t, a)
		keys = append(keys, a["key"])
	}
	logged := interceptor(t, routed(t, 42, p.fingerprint, inject), leaves).logs.String()
	if strings.Count(logged, "no leaf for ") != len(injected())-1 {
		t.Fatalf("logged %q, want each leaf refused named", logged)
	}
	for _, k := range keys {
		body := strings.TrimSuffix(strings.TrimPrefix(k, "-----BEGIN PRIVATE KEY-----\n"), "-----END PRIVATE KEY-----\n")
		for _, line := range strings.Split(body, "\n") {
			if len(line) >= 16 && strings.Contains(logged, line) {
				t.Errorf("a log line carries a line of a leaf's key: %q", logged)
			}
		}
	}
}

// TestADestinationWithoutARoutePassesThrough: a route changes what the check answers of its own destination alone:
// a call to a destination its env holds no route to, under a policy whose other destinations have routes, is answered
// exactly as under one with no routes at all — let through, nothing set, x-canyon-intercept removed, no caller — so
// it passes through as it did, never decrypted.
func TestADestinationWithoutARoutePassesThrough(t *testing.T) {
	p := newPKI(t)
	routes := map[string][]string{"claims-portal": {"api.stripe.com", "hooks.slack.com"}}
	with := interceptor(t, routed(t, 42, p.fingerprint, routes), leavesOf(t, p, routes["claims-portal"], start))
	if s := subscribe(t, serving(t, with)).take(); s.listener == nil {
		t.Fatal("the policy with routes serves no listener: this test would prove nothing")
	}
	without := interceptor(t, routed(t, 42, p.fingerprint, map[string][]string{"claims-portal": {}}), nil)
	for _, call := range []struct{ method, scheme, authority string }{
		{"CONNECT", "", "api.openai.com:443"}, {"GET", "https", "api.openai.com"}, {"GET", "http", "api.openai.com"}, {"CONNECT", "", "registry.npmjs.org:443"}} {
		req := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
				Address: "172.28.3.11", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 41234}}}}},
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Method: call.method, Scheme: call.scheme, Host: call.authority}},
		}}
		a, err := with.Check(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := without.Check(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(a, b) {
			t.Errorf("%v: beside routes answered %v, with none %v: want the same", call, a, b)
		}
		if ok := a.GetOkResponse(); ok != nil && (len(ok.GetHeaders()) != 0 || !slices.Equal(ok.GetHeadersToRemove(), []string{"x-canyon-intercept"}) ||
			a.GetDynamicMetadata().AsMap()["caller"] != nil || a.GetDynamicMetadata().AsMap()["destination"] != nil) {
			t.Errorf("%v: let through setting %v, removing %v, want nothing set and x-canyon-intercept removed", call, ok.GetHeaders(), ok.GetHeadersToRemove())
		}
	}
}

// TestLeafAsksTheLeafRoute: Client.Leaf asks GET /egress/leaf?destination= at the policy's listener and answers a
// 200's body alone: any other answer is an error naming its status, never its body.
func TestLeafAsksTheLeafRoute(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.String()
		if r.URL.Query().Get("destination") == "api.stripe.com" {
			_, _ = w.Write([]byte(`{"key":"x"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no_signing_root","key":"sk_live_` + strings.Repeat("k", 24) + `"}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := c.Leaf(context.Background(), "api.stripe.com"); err != nil || string(raw) != `{"key":"x"}` || asked != "GET /egress/leaf?destination=api.stripe.com" {
		t.Errorf("Leaf asked %q and answered %q %v", asked, raw, err)
	}
	if _, err := c.Leaf(context.Background(), "hooks.slack.com"); err == nil || strings.Contains(err.Error(), "sk_live_") || !strings.Contains(err.Error(), "503") {
		t.Errorf("a 503 answered %v, want an error naming the status alone", err)
	}
}

// TestEnvoysRefusalIsLogged: an answer Envoy refuses is a log line naming its type and the version Envoy keeps, and
// none of Envoy's words, which may quote what it was sent.
func TestEnvoysRefusalIsLogged(t *testing.T) {
	p := newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	e := subscribe(t, serving(t, r))
	s := e.take()
	nack := &discoveryv3.DiscoveryRequest{TypeUrl: resource.SecretType, ResourceNames: injected(), VersionInfo: s.secrets.GetVersionInfo(),
		ResponseNonce: s.secrets.GetNonce(), ErrorDetail: &rpcstatus.Status{Code: 3, Message: "Failed to load private key: sk_live_" + strings.Repeat("k", 24)}}
	if err := e.stream.Send(nack); err != nil {
		t.Fatal(err)
	}
	want := `egress-control: Envoy refused an answer of ` + resource.SecretType + ` and keeps version "` + s.secrets.GetVersionInfo() + `": code 3`
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(r.logs.String(), want) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if logged := r.logs.String(); !strings.Contains(logged, want) || strings.Contains(logged, "sk_live_") {
		t.Errorf("logged %q, want %q and none of Envoy's words", logged, want)
	}
}

// TestRunServesTheListenerAsThePolicyArrives: a running replica builds the listener the moment a policy is
// installed, not at its next tick: an Envoy that asked before the first policy is answered no listener, then is
// pushed the policy's.
func TestRunServesTheListenerAsThePolicyArrives(t *testing.T) {
	p := newPKI(t)
	r := newReplica(t, 1, &server{policy: routed(t, 42, p.fingerprint, inject), leaves: leavesOf(t, p, injected(), start), down: true})
	e := subscribe(t, serving(t, r))
	if s := e.take(); s.listener != nil {
		t.Fatalf("before any policy a listener is served, chains %v", sdsNames(t, s.listener))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.srv.set(false, nil)
	go r.Run(ctx)
	for deadline := time.Now().Add(pollEvery / 2); ; {
		resp := e.pushed(time.Until(deadline))
		if resp == nil {
			t.Fatalf("no listener pushed within %s of the first policy, want it built as the policy is installed", pollEvery/2)
		}
		if resp.GetTypeUrl() == resource.ListenerType {
			if got := sdsNames(t, listenerIn(t, resp)); !slices.Equal(got, injected()) {
				t.Errorf("pushed chains %v, want %v", got, injected())
			}
			return
		}
	}
}

// TestADroppedChainFallsThroughToNothing: per destination, a leaf that cannot be read drops that destination's chain
// alone, and nothing else catches it: the listener has no default chain and no chain matching any name but its own,
// so Envoy closes a CONNECT routed there for it. The destination stays an inject destination of the policy held, which
// the check decides by — never the listener served — so it is still routed to the internal listener and fails there,
// never to the plain tunnel without its credential (design/EGRESS.md §6, row 3).
func TestADroppedChainFallsThroughToNothing(t *testing.T) {
	p := newPKI(t)
	policy, leaves := routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start)
	full := interceptor(t, policy, leaves)
	leaves = maps.Clone(leaves)
	leaves["api.stripe.com"] = nil
	r := interceptor(t, policy, leaves)
	l := subscribe(t, serving(t, r)).take().listener
	if l.GetDefaultFilterChain() != nil {
		t.Errorf("the listener has a default chain %q: a destination with no chain of its own would fall into it", l.GetDefaultFilterChain().GetName())
	}
	for _, c := range l.GetFilterChains() {
		if !proto.Equal(c.GetFilterChainMatch(), &listenerv3.FilterChainMatch{ServerNames: []string{c.GetName()}}) || !slices.Contains(r.held.inject, c.GetName()) {
			t.Errorf("chain %q matches %v, want its own inject destination's name and nothing else", c.GetName(), c.GetFilterChainMatch())
		}
	}
	if slices.Contains(sdsNames(t, l), "api.stripe.com") {
		t.Error("the destination whose leaf is unreadable has a chain")
	}
	if !slices.Equal(r.held.inject, full.held.inject) || !slices.Contains(r.held.inject, "api.stripe.com") {
		t.Errorf("the policy's inject destinations are %v with a leaf unreadable, want %v: the check decides by the policy, never by the listener", r.held.inject, full.held.inject)
	}
	req := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "172.28.3.11", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 41234}}}}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Method: "CONNECT", Host: "api.stripe.com:443"}},
	}}
	a, err := r.Check(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := full.Check(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(a, b) {
		t.Errorf("with the leaf unreadable the check answered %v, with it %v: want the same, the listener read by nothing the check decides by", a, b)
	}
}

// TestALeafThatCannotBeReadIsAskedLessOften: a leaf the server never answers readably is asked for again pollEvery
// later, then twice that, and so on to leafBackoff — not every build — while the other destinations' leaves are
// asked for no more than their lives call for; under another signing root it is asked for at once, and once read it
// waits no more.
func TestALeafThatCannotBeReadIsAskedLessOften(t *testing.T) {
	p, next := newPKI(t), newPKI(t)
	leaves := leavesOf(t, p, injected(), start)
	leaves["api.stripe.com"] = []byte("<html>")
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leaves)
	asked := func(d string) int {
		r.srv.mu.Lock()
		defer r.srv.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(r.srv.leafAsks), func(x string) bool { return x != d }))
	}
	var at []time.Duration
	for i := 1; i <= 120; i++ { // ten minutes of builds, pollEvery apart
		r.clock.add(pollEvery)
		before := asked("api.stripe.com")
		r.intercept(context.Background())
		if asked("api.stripe.com") > before {
			at = append(at, time.Duration(i)*pollEvery)
		}
	}
	if want := []time.Duration{5 * time.Second, 15 * time.Second, 35 * time.Second, 75 * time.Second, 155 * time.Second, 315 * time.Second}; !slices.Equal(at, want) {
		t.Errorf("in ten minutes the unreadable leaf was asked for again at %v, want at %v: doubling from %s to %s", at, want, pollEvery, leafBackoff)
	}
	if n := asked("api.openai.com"); n != 1 {
		t.Errorf("a readable leaf asked for %d times in ten minutes, want once", n)
	}

	// Read once under the same root — a leaf already past half its life, so it is asked for at every build — then
	// unreadable again: its wait starts over at pollEvery, and the failure is logged anew.
	r.srv.mu.Lock()
	r.srv.leaves["api.stripe.com"] = marshal(t, p.answer(t, r.clock.now().Add(-23*time.Hour), r.clock.now().Add(time.Hour), "api.stripe.com"))
	r.srv.mu.Unlock()
	for range leafBackoff / pollEvery {
		if r.holds(t, "api.stripe.com") {
			break
		}
		r.clock.add(pollEvery)
		r.intercept(context.Background())
	}
	if !r.holds(t, "api.stripe.com") {
		t.Fatalf("the leaf made readable was not read within %s", leafBackoff)
	}
	logged := r.logs.String()
	r.srv.mu.Lock()
	r.srv.leaves["api.stripe.com"] = []byte("<html>")
	r.srv.mu.Unlock()
	before := asked("api.stripe.com")
	for range 2 {
		r.clock.add(pollEvery)
		r.intercept(context.Background())
	}
	if got := asked("api.stripe.com") - before; got != 2 {
		t.Errorf("once read, then unreadable again, the leaf was asked for %d times in %s, want twice: its wait starts over at %s", got, 2*pollEvery, pollEvery)
	}
	if again := strings.TrimPrefix(r.logs.String(), logged); !strings.Contains(again, `no new leaf for "api.stripe.com"`) {
		t.Errorf("once read, then unreadable again, logged %q, want the failure logged anew", again)
	}

	r.srv.mu.Lock()
	r.srv.leaves = map[string][]byte{"api.stripe.com": next.leaf(t, "api.stripe.com", r.clock.now())}
	r.srv.policy = routed(t, 43, next.fingerprint, inject)
	r.srv.mu.Unlock()
	r.poll(context.Background())
	before = asked("api.stripe.com")
	r.intercept(context.Background())
	if asked("api.stripe.com") != before+1 || !r.holds(t, "api.stripe.com") {
		t.Error("under a new signing root the leaf waited out its backoff, want it asked for at once and served")
	}
}

// TestAHungServerHoldsABuildNoLongerThanItsRound: every ask of one build ends within leafRound, however many inject
// destinations a hung server leaves unanswered, so a policy installed and a leaf's end wait on no more than that; a
// leaf held past its end is dropped in that build all the same.
func TestAHungServerHoldsABuildNoLongerThanItsRound(t *testing.T) {
	p := newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	const round = 300 * time.Millisecond
	r.withLeafRound(round)
	r.srv.mu.Lock()
	r.srv.hang = make(chan struct{}) // never closed: every ask waits out its context
	r.srv.mu.Unlock()
	r.clock.add(24 * time.Hour) // every leaf's end: each is asked for again, and none answers
	began := time.Now()
	r.intercept(context.Background())
	if took := time.Since(began); took > round+time.Second {
		t.Errorf("a build took %s with the server hung, want %s at most: %d destinations of %s each would be %s",
			took, round, len(injected()), leafWait, time.Duration(len(injected()))*leafWait)
	}
	if held := r.leavesHeld(t); len(held) != 0 {
		t.Errorf("leaves past their end still held for %v, want each dropped in the build that found it", held)
	}
}

// TestAHeldLeafIsAskedForBeforeItsEnd: however long a leaf that cannot be renewed has waited, a leaf still held is
// asked for again pollEvery before its end, so a server back a minute before it costs the destination no chain.
func TestAHeldLeafIsAskedForBeforeItsEnd(t *testing.T) {
	p := newPKI(t)
	routes := map[string][]string{"claims-portal": {"api.stripe.com"}}
	r := interceptor(t, routed(t, 42, p.fingerprint, routes), leavesOf(t, p, routes["claims-portal"], start))
	r.srv.set(true, nil)
	r.clock.add(12 * time.Hour) // past half the leaf's life: asked for again, the server down
	asks := func() int { r.srv.mu.Lock(); defer r.srv.mu.Unlock(); return len(r.srv.leafAsks) }
	var at []time.Time
	for range 3 * leafBackoff / pollEvery { // the waits double from pollEvery: 15 minutes see one of leafBackoff
		r.clock.add(pollEvery)
		before := asks()
		r.intercept(context.Background())
		if asks() > before {
			at = append(at, r.clock.now())
		}
	}
	if n := len(at); n < 2 || at[n-1].Sub(at[n-2]) != leafBackoff {
		t.Fatalf("the leaf was asked for at %v, want the last two asks %s apart: this test proves nothing short of the longest wait", at, leafBackoff)
	}
	r.clock.add(start.Add(24*time.Hour - 3*time.Minute).Sub(r.clock.now())) // three minutes before the leaf's end
	r.intercept(context.Background())
	r.srv.mu.Lock()
	r.srv.down, r.srv.leaves = false, leavesOf(t, p, routes["claims-portal"], start.Add(24*time.Hour-2*time.Minute)) // back two minutes before it
	r.srv.mu.Unlock()
	for r.clock.now().Before(start.Add(24*time.Hour + pollEvery)) {
		r.clock.add(pollEvery)
		r.intercept(context.Background())
		if !r.holds(t, "api.stripe.com") {
			t.Fatalf("at %s the chain is dropped, the server back since %s: want the held leaf asked for again before its end at %s",
				r.clock.now().Sub(start), 24*time.Hour-2*time.Minute, 24*time.Hour)
		}
	}
}

// bound holds of d's connection manager: Envoy drops a trailing dot of the authority before routing and before
// ext_authz #2, and the routes bind the authority to d themselves, whatever the check answers — one virtual host for
// exactly d and d:443 (Envoy lower-cases the authority to match it), and every other authority to one that answers
// 421 itself, asking nothing (contracts/routes.md, Interception; design/EGRESS.md §4.1).
func bound(t *testing.T, d string, hcm *hcmv3.HttpConnectionManager) {
	t.Helper()
	if !hcm.GetStripTrailingHostDot() {
		t.Errorf("chain %s: strip_trailing_host_dot off: %s. would be misdirected to the routes and %s to the check", d, d, d)
	}
	vhosts := hcm.GetRouteConfig().GetVirtualHosts()
	if len(vhosts) != 2 {
		t.Fatalf("chain %s: %d virtual hosts, want the destination's and the catch-all's", d, len(vhosts))
	}
	if got := vhosts[0].GetDomains(); !slices.Equal(got, []string{d, d + ":443"}) {
		t.Errorf("chain %s: the destination's virtual host matches %v, want exactly [%s %s:443]", d, got, d, d)
	}
	all := vhosts[1]
	off := &extauthzv3.ExtAuthzPerRoute{}
	if !slices.Equal(all.GetDomains(), []string{"*"}) || all.GetTypedPerFilterConfig()["envoy.filters.http.ext_authz"].UnmarshalTo(off) != nil || !off.GetDisabled() {
		t.Errorf("chain %s: the catch-all matches %v and asks %v, want every other authority, ext_authz disabled: its answer rests on nothing", d, all.GetDomains(), off)
	}
	want := (decision{reason: "misdirected", destination: d}).body()
	if rts := all.GetRoutes(); len(rts) != 1 || rts[0].GetMatch().GetPrefix() != "/" || rts[0].GetDirectResponse().GetStatus() != 421 ||
		rts[0].GetDirectResponse().GetBody().GetInlineString() != want || rts[0].GetRoute() != nil {
		t.Errorf("chain %s: the catch-all routes %v, want every request answered 421 %q, none sent on", d, rts, want)
	}
	headers := map[string]string{}
	for _, h := range all.GetResponseHeadersToAdd() {
		if h.GetAppendAction() == corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
		}
	}
	if !maps.Equal(headers, map[string]string{"Canyon-Egress": "misdirected", "content-type": "text/plain; charset=utf-8"}) {
		t.Errorf("chain %s: the catch-all's 421 carries %v, want Canyon-Egress misdirected and the check's content-type", d, headers)
	}
}

// virtualHost is the virtual host Envoy picks of routes for authority: one naming it exactly, Envoy lower-casing it
// first, else the one naming *.
func virtualHost(routes *routev3.RouteConfiguration, authority string) string {
	authority = strings.ToLower(authority)
	for _, v := range routes.GetVirtualHosts() {
		if slices.Contains(v.GetDomains(), authority) {
			return v.GetName()
		}
	}
	for _, v := range routes.GetVirtualHosts() {
		if slices.Contains(v.GetDomains(), "*") {
			return v.GetName()
		}
	}
	return ""
}

// TestTheListenerAnswersAMisdirectedAuthority421: the chain's own routes, whatever ext_authz #2 answers, send only its
// destination's authority on — the name, the name and :443, in any case — and answer any other 421 themselves, the
// loopback address of egress-control's own port, where the leaves' keys are served, among them. The check refuses
// the same on its own (TestMisdirectedAuthorityDenied).
func TestTheListenerAnswersAMisdirectedAuthority421(t *testing.T) {
	hcm := &hcmv3.HttpConnectionManager{}
	if err := chainOf(t, "api.stripe.com").GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	bound(t, "api.stripe.com", hcm)
	for _, authority := range []string{"api.stripe.com", "API.Stripe.COM", "api.stripe.com:443"} {
		if got := virtualHost(hcm.GetRouteConfig(), authority); got != "api.stripe.com" {
			t.Errorf("%q goes to the virtual host %q, want the destination's", authority, got)
		}
	}
	for _, authority := range []string{"127.0.0.1:7783", "127.0.0.1", "localhost:7783", "[::1]:7783", "api.stripe.co", "api.stripe.com:8443",
		"api.stripe.com:0443", "api.stripe.com:80", "evil.example.com", "stripe.com", "x.api.stripe.com", ""} {
		if got := virtualHost(hcm.GetRouteConfig(), authority); got != "misdirected" {
			t.Errorf("%q goes to the virtual host %q, want the catch-all's 421", authority, got)
		}
	}
}

// TestTheChainsLocalRepliesAreTheChecks: an inner request whose check does not answer is :3128's 503 with Canyon's
// line, and one whose connection, handshake, certificate or name fails before it is sent is 502 upstream_failed,
// naming the destination; nothing else is Canyon's (design/EGRESS.md §6; contracts/routes.md, The check).
func TestTheChainsLocalRepliesAreTheChecks(t *testing.T) {
	m := localReplies(t, "api.stripe.com").GetMappers()
	if len(m) != 2 {
		t.Fatalf("%d local replies, want two", len(m))
	}
	and := m[0].GetFilter().GetAndFilter().GetFilters()
	if len(and) != 2 || and[0].GetStatusCodeFilter().GetComparison().GetValue().GetDefaultValue() != 503 ||
		and[0].GetStatusCodeFilter().GetComparison().GetOp() != accesslogv3.ComparisonFilter_EQ ||
		!slices.Equal(and[1].GetResponseFlagFilter().GetFlags(), []string{"UAEX"}) || m[0].GetStatusCode() != nil ||
		m[0].GetBody().GetInlineString() != "Canyon's egress check did not answer, so this call was not made." {
		t.Errorf("the first local reply is %v, want the check's 503 (UAEX) with Canyon's line", m[0])
	}
	headers := map[string]string{}
	for _, h := range m[1].GetHeadersToAdd() {
		headers[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
	}
	if !slices.Equal(m[1].GetFilter().GetResponseFlagFilter().GetFlags(), []string{"UF", "DF", "UH"}) || m[1].GetStatusCode().GetValue() != 502 ||
		m[1].GetBody().GetInlineString() != "Canyon could not connect to api.stripe.com or verify its certificate, so it did not send this call." ||
		!maps.Equal(headers, map[string]string{"Canyon-Egress": "upstream_failed", "content-type": "text/plain; charset=utf-8"}) {
		t.Errorf("the second local reply is %v, want UF, DF and UH alone answered 502 upstream_failed, naming the destination", m[1])
	}
}

// TestTheInnerChainLogsItsCaller: every chain egress-control serves logs each inner request to the receiver, as
// :3128 does, under the log name InterceptLog, by which the receiver tells an inner entry, with the receiver's token
// and the filter-state object canyon.caller, which names the caller; nothing else of the tunnel's state is logged
// (design/EGRESS.md §2.4, §4.2; contracts/routes.md, Interception, A record).
func TestTheInnerChainLogsItsCaller(t *testing.T) {
	p := newPKI(t)
	s := subscribe(t, serving(t, interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start)))).take()
	want := &grpclogv3.HttpGrpcAccessLogConfig{CommonConfig: &grpclogv3.CommonGrpcAccessLogConfig{
		LogName:             InterceptLog,
		TransportApiVersion: corev3.ApiVersion_V3,
		GrpcService: &corev3.GrpcService{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "receiver"}},
			InitialMetadata: []*corev3.HeaderValue{{Key: "authorization", Value: "Bearer " + receiverToken}}},
		FilterStateObjectsToLog: []string{"canyon.caller"},
	}}
	if len(s.listener.GetFilterChains()) == 0 {
		t.Fatal("no chain served")
	}
	for _, c := range s.listener.GetFilterChains() {
		hcm := &hcmv3.HttpConnectionManager{}
		if err := c.GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
			t.Fatal(err)
		}
		logs := hcm.GetAccessLog()
		got := &grpclogv3.HttpGrpcAccessLogConfig{}
		if len(logs) != 1 || logs[0].GetName() != "envoy.access_loggers.http_grpc" || logs[0].GetFilter() != nil || logs[0].GetTypedConfig().UnmarshalTo(got) != nil || !proto.Equal(got, want) {
			t.Errorf("chain %s logs %v, want every request to the receiver as %s, with its token and canyon.caller alone", c.GetName(), logs, InterceptLog)
		}
	}
	if InterceptLog == "egress" {
		t.Errorf("the chains log as %q, :3128's own name: the receiver could not tell an inner entry", InterceptLog)
	}
}

// TestTheReceiverTokenIsInTheLDSAnswerAlone: egress-control holds the receiver's token for the chains' access log and
// serves it Envoy in the LDS answer, and nowhere else: no log line, whatever Envoy's refusal of that answer quotes, no
// metric and no secret carries it (design/EGRESS.md §4.2).
func TestTheReceiverTokenIsInTheLDSAnswerAlone(t *testing.T) {
	p := newPKI(t)
	r := interceptor(t, routed(t, 42, p.fingerprint, inject), leavesOf(t, p, injected(), start))
	e := subscribe(t, serving(t, r))
	s := e.take()
	if !bytes.Contains(s.listeners.GetResources()[0].GetValue(), []byte(receiverToken)) {
		t.Fatal("the LDS answer carries no receiver token: the chains could not log")
	}
	for _, a := range s.secrets.GetResources() {
		if bytes.Contains(a.GetValue(), []byte(receiverToken)) {
			t.Error("an SDS answer carries the receiver token")
		}
	}
	nack := &discoveryv3.DiscoveryRequest{TypeUrl: resource.ListenerType, VersionInfo: s.listeners.GetVersionInfo(), ResponseNonce: s.listeners.GetNonce(),
		ErrorDetail: &rpcstatus.Status{Code: 3, Message: `Proto constraint validation failed: initial_metadata {key: "authorization" value: "Bearer ` + receiverToken + `"}`}}
	if err := e.stream.Send(nack); err != nil {
		t.Fatal(err)
	}
	want := `egress-control: Envoy refused an answer of ` + resource.ListenerType
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(r.logs.String(), want) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	var metrics bytes.Buffer
	if err := r.WriteMetrics(&metrics); err != nil {
		t.Fatal(err)
	}
	if logged := r.logs.String(); !strings.Contains(logged, want) || strings.Contains(logged, receiverToken) || strings.Contains(metrics.String(), receiverToken) {
		t.Errorf("logged %q and metrics %q, want the refusal logged and the token in neither", logged, metrics.String())
	}
}
