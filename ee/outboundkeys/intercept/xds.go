package intercept

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	mutationv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	grpclogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/grpc/v3"
	dfpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_forward_proxy/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	headermutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	tlsinspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Interception (design/EGRESS.md §4.2, §4.5; contracts/routes.md, Interception): egress-control serves the Envoy
// beside it, over ADS on the address it answers the check on, one internal listener by LDS — a filter chain per
// inject destination, matched by the name the app's TLS asks for — and each chain's leaf by SDS, and nothing else:
// no clusters, routes or endpoints of their own, so a new env needs no push. Both are built from the policy held
// and the leaves fetched alone, so replicas holding the same policy serve the same listener; their secrets differ, as
// the server mints a leaf for each answer.

// The names Envoy's bootstrap and the receiver know (internal/egresscontrol's interceptListener, interceptedCluster,
// interceptLog and callerObject, which the core's tests hold these to), and how leaves are asked for.
const (
	interceptListener  = "intercept"     // the one internal listener, which Envoy's one internal cluster names
	interceptedCluster = "intercepted"   // Envoy's static cluster an inner request leaves by: TLS it originates and verifies
	interceptLog       = "intercept"     // the chains' access log, by whose name the receiver tells an inner request's entry
	callerObject       = "canyon.caller" // the filter-state object holding the caller ext_authz #1 named, <app>/<env>
	pollEvery          = 5 * time.Second // egress-control's poll, which a leaf's waits are counted in
	leafWait           = 2 * time.Second // one leaf's ask
	leafRound          = 4 * time.Second // every ask of one build: a hung server holds a build no longer
	leafBackoff        = 5 * time.Minute // the longest a leaf that cannot be read waits to be asked again, doubling from pollEvery
)

// leaf is one inject destination's leaf as GET /egress/leaf answered it: the signing root the answer named, what
// Envoy sends (the certificate, then the chain), its key, held in this memory and Envoy's SDS alone, and when it is
// valid.
type leaf struct {
	root     string
	chain    []byte
	key      []byte
	from, to time.Time
}

// renewBy is when the leaf is asked for again, half its life gone, so a server down for less than that never
// costs a destination its chain.
func (l *leaf) renewBy() time.Time { return l.from.Add(l.to.Sub(l.from) / 2) }

var errLeafNotJSON = errors.New("a leaf that is not JSON") // never the decoder's own, which may quote a byte of the key

// parseLeaf takes GET /egress/leaf's 200 for destination only when it is what Envoy may serve at now: an answer
// naming signing, the policy's signing root, as its root — the server's word, as the chain stops below the root and
// the policy carries only its fingerprint; one certificate naming destination alone, signed by the chain's first and
// valid now; the chain's certificates each signed by the next; and a key that is the certificate's. No error names a
// byte of the answer.
func parseLeaf(raw []byte, destination, signing string, now time.Time) (*leaf, error) {
	var a struct {
		Root     string    `json:"root"`
		Cert     string    `json:"cert"`
		Key      string    `json:"key"`
		Chain    string    `json:"chain"`
		NotAfter time.Time `json:"not_after"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil, errLeafNotJSON
	}
	if a.Root != signing {
		return nil, errors.New("a leaf under a root other than the policy's signing root")
	}
	certs, err := certificates(a.Cert)
	if err != nil || len(certs) != 1 {
		return nil, errors.New("a leaf that is not one certificate")
	}
	cert := certs[0]
	chain, err := certificates(a.Chain)
	switch {
	case err != nil || len(chain) == 0 || len(chain) > 4:
		return nil, errors.New("a chain that is not one to four certificates")
	case !slices.Equal(cert.DNSNames, []string{destination}) || len(cert.IPAddresses)+len(cert.EmailAddresses)+len(cert.URIs) != 0:
		return nil, fmt.Errorf("a leaf naming other than %s alone", destination)
	case now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) || !a.NotAfter.Equal(cert.NotAfter):
		return nil, errors.New("a leaf not valid now, or whose not_after is not its end")
	case cert.CheckSignatureFrom(chain[0]) != nil:
		return nil, errors.New("a leaf the chain's first certificate did not sign")
	}
	for i := range len(chain) - 1 {
		if chain[i].CheckSignatureFrom(chain[i+1]) != nil {
			return nil, errors.New("a chain whose certificates do not sign one another in order")
		}
	}
	block, rest := pem.Decode([]byte(a.Key))
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		return nil, errors.New("a key that is not one PKCS #8 PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("a key that is not PKCS #8")
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("a key that cannot sign")
	}
	if public, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !public.Equal(cert.PublicKey) {
		return nil, errors.New("a key that is not the leaf's")
	}
	return &leaf{root: a.Root, chain: []byte(a.Cert + a.Chain), key: []byte(a.Key), from: cert.NotBefore, to: cert.NotAfter}, nil
}

// certificates parses PEM that is certificates and nothing else.
func certificates(s string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for rest := []byte(s); len(rest) > 0; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("not certificates alone")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// asking is when a destination whose leaf could not be read is asked again: under the signing root it failed under,
// not before next, wait doubling at each failure up to leafBackoff; and the failure last logged.
type asking struct {
	root   string
	next   time.Time
	wait   time.Duration
	failed string
}

// Snapshot builds the interception listener again for signing and inject, egress-control's policy held's — none of
// either for no policy or no receiver token — called by one goroutine at a time: each inject destination keeps the
// leaf it holds under the signing root until half its life is gone, then asks for another, keeping the one it holds
// while the server cannot answer and it is still valid. A leaf that cannot be read is asked for again from pollEvery
// later, doubling to leafBackoff but never later than pollEvery before the end of a leaf held, at once under another
// signing root, and from pollEvery again once one is read; every ask of a build ends within leafRound, the rest left
// to the next. A destination with no valid leaf has no chain, and with no chain there is no listener — so no policy,
// no signing root, an env trusting none, or a leaf unreadable intercepts nothing, and an intercepted call finds no
// chain (its connection closed) or no listener (503): never forwarded without its check, or in clear. A failure is
// logged once until it changes, and a chain dropped once, never with a byte of the leaf.
func (i *Interceptor) Snapshot(ctx context.Context, signing string, inject []string) *cache.Snapshot {
	round, cancel := context.WithTimeout(ctx, i.leafRound)
	defer cancel()
	kept, asks := map[string]*leaf{}, map[string]asking{}
	for _, d := range inject {
		l, a, now := i.leaves[d], i.asks[d], i.now()
		if a.root != signing {
			a = asking{root: signing}
		}
		if (l == nil || l.root != signing || !now.Before(l.renewBy())) && !now.Before(a.next) && round.Err() == nil {
			fresh, err := i.askLeaf(round, d, signing)
			if err == nil {
				l, a = fresh, asking{root: signing}
			} else {
				a.wait = min(max(2*a.wait, pollEvery), leafBackoff)
				a.next = i.now().Add(a.wait)
				if l != nil && l.root == signing && a.next.After(l.to.Add(-pollEvery)) {
					a.next = l.to.Add(-pollEvery) // a held leaf is asked for again before its end, never lost to the wait
				}
				if msg := err.Error(); msg != a.failed {
					a.failed = msg
					if l != nil && l.root == signing && now.Before(l.to) {
						i.log.Printf("egress-control: no new leaf for %q, so Envoy keeps the one it holds until %s: %v", d, l.to.UTC().Format(time.RFC3339), err)
					} else {
						i.log.Printf("egress-control: no leaf for %q, so it has no chain: %v", d, err)
					}
				}
			}
		}
		if now := i.now(); l != nil && l.root == signing && !now.Before(l.from) && now.Before(l.to) {
			kept[d] = l
		} else if i.leaves[d] != nil {
			i.log.Printf("egress-control: the leaf for %q ended, or was signed under a root no longer signing, so its chain is dropped", d)
		}
		if a != (asking{root: signing}) {
			asks[d] = a
		}
	}
	i.leaves, i.asks = kept, asks
	return snapshotOf(inject, kept, i.token)
}

func (i *Interceptor) askLeaf(ctx context.Context, destination, signing string) (*leaf, error) {
	ask, cancel := context.WithTimeout(ctx, leafWait)
	defer cancel()
	raw, err := i.leafOf(ask, destination)
	if err != nil {
		return nil, err
	}
	return parseLeaf(raw, destination, signing, i.now())
}

// snapshotOf is what Envoy is served: the listener with a chain for each of inject that holds a leaf, in inject's
// order, each logging to the receiver with token, and those leaves; none of either when no destination holds one.
// Each type's version is a hash of what it serves, so a policy changing nothing they hold — a new env with no new
// destination — changes neither, and the secrets' hashes no key.
func snapshotOf(inject []string, leaves map[string]*leaf, token string) *cache.Snapshot {
	var chains []*listenerv3.FilterChain
	var secrets []types.Resource
	sum := sha256.New()
	for _, d := range inject {
		l := leaves[d]
		if l == nil {
			continue
		}
		chains = append(chains, chainOf(d, token))
		secrets = append(secrets, &tlsv3.Secret{Name: d, Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: l.chain}},
			PrivateKey:       &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: l.key}},
		}}})
		sum.Write([]byte(d + "\x00"))
		sum.Write(l.chain)
	}
	var listeners []types.Resource
	if len(chains) > 0 {
		listeners = append(listeners, &listenerv3.Listener{
			Name:              interceptListener,
			ListenerSpecifier: &listenerv3.Listener_InternalListener{InternalListener: &listenerv3.Listener_InternalListenerConfig{}},
			ListenerFilters:   []*listenerv3.ListenerFilter{{Name: "envoy.filters.listener.tls_inspector", ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: typed(&tlsinspectorv3.TlsInspector{})}}},
			FilterChains:      chains,
		})
	}
	s := &cache.Snapshot{}
	s.Resources[types.Listener] = cache.NewResources(hashOf(listeners), listeners)
	s.Resources[types.Secret] = cache.NewResources(hex.EncodeToString(sum.Sum(nil)[:16]), secrets)
	return s
}

func hashOf(resources []types.Resource) string {
	sum := sha256.New()
	for _, r := range resources {
		raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(r) // a message built here: never an error
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// typed packs m deterministically, so the same message is the same bytes on every replica.
func typed(m proto.Message) *anypb.Any {
	a := &anypb.Any{}
	_ = anypb.MarshalFrom(a, m, proto.MarshalOptions{Deterministic: true}) // a message built here: never an error
	return a
}

// chainOf is destination's filter chain (contracts/routes.md, Interception): the app's TLS ends here, by the leaf
// SDS names after it, only for a client asking for that name; the caller is x-canyon-caller and the destination
// ext_authz #1 approved x-canyon-destination, each overwritten from the tunnel's filter state, canyon.caller and
// canyon.destination, whatever the app sent, empty without one — kept empty, as Envoy skips a mutation of an empty
// value otherwise, which would leave the app's own header; ext_authz #2 asks before anything moves, its context
// naming the destination, and no answer is a 503; then the request leaves by the static intercepted cluster, which retries
// nothing past a connect failure. The timeouts are :3128's. A request whose authority is not the destination — the
// trailing dot one Envoy drops first, then exactly the name or the name and :443, case aside — never reaches the
// upstream however ext_authz #2 answers it: the listener answers it 421 itself, asking nothing, as the check would.
// Every request is one entry of the access log interceptLog, sent to the receiver with its token as :3128's are, and
// carrying callerObject, by which the receiver names its caller, as its source is no app's address.
func chainOf(destination, token string) *listenerv3.FilterChain {
	fromState := func(header, object string) *mutationv3.HeaderMutation {
		return &mutationv3.HeaderMutation{Action: &mutationv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header:         &corev3.HeaderValue{Key: header, Value: "%FILTER_STATE(" + object + ":PLAIN)%"},
			AppendAction:   corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			KeepEmptyValue: true}}}
	}
	filter := func(name string, m proto.Message) *hcmv3.HttpFilter {
		return &hcmv3.HttpFilter{Name: name, ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: typed(m)}}
	}
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix:        interceptListener,
		CodecType:         hcmv3.HttpConnectionManager_AUTO,
		SkipXffAppend:     true,
		GenerateRequestId: wrapperspb.Bool(false),
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{IdleTimeout: durationpb.New(60 * time.Second),
			MaxStreamDuration: durationpb.New(900 * time.Second)},
		RequestHeadersTimeout: durationpb.New(10 * time.Second),
		StreamIdleTimeout:     durationpb.New(300 * time.Second),
		StripTrailingHostDot:  true, // api.stripe.com. is api.stripe.com, to the routes and to ext_authz #2 alike
		LocalReplyConfig:      localReplies(destination),
		AccessLog: []*accesslogv3.AccessLog{{Name: "envoy.access_loggers.http_grpc", ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: typed(&grpclogv3.HttpGrpcAccessLogConfig{
			CommonConfig: &grpclogv3.CommonGrpcAccessLogConfig{
				LogName:             interceptLog,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcService: &corev3.GrpcService{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "receiver"}},
					InitialMetadata: []*corev3.HeaderValue{{Key: "authorization", Value: "Bearer " + token}}},
				FilterStateObjectsToLog: []string{callerObject},
			}})}}},
		HttpFilters: []*hcmv3.HttpFilter{
			filter("envoy.filters.http.header_mutation", &headermutationv3.HeaderMutation{Mutations: &headermutationv3.Mutations{
				RequestMutations: []*mutationv3.HeaderMutation{fromState("x-canyon-caller", callerObject), fromState("x-canyon-destination", "canyon.destination")}}}),
			filter("envoy.filters.http.ext_authz", &extauthzv3.ExtAuthz{
				TransportApiVersion: corev3.ApiVersion_V3,
				Services: &extauthzv3.ExtAuthz_GrpcService{GrpcService: &corev3.GrpcService{
					TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "control"}},
					Timeout:         durationpb.New(5 * time.Second)}},
				FailureModeAllow: false,
				StatusOnError:    &typev3.HttpStatus{Code: typev3.StatusCode_ServiceUnavailable},
				AllowedHeaders: &matcherv3.ListStringMatcher{Patterns: []*matcherv3.StringMatcher{
					{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: "x-canyon-caller"}}, {MatchPattern: &matcherv3.StringMatcher_Exact{Exact: "x-canyon-destination"}}}},
			}),
			filter("envoy.filters.http.dynamic_forward_proxy", &dfpv3.FilterConfig{ImplementationSpecifier: &dfpv3.FilterConfig_SubClusterConfig{
				SubClusterConfig: &dfpv3.SubClusterConfig{ClusterInitTimeout: durationpb.New(5 * time.Second)}}}),
			filter("envoy.filters.http.router", &routerv3.Router{SuppressEnvoyHeaders: true}),
		},
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name:                   destination,
			RequestHeadersToRemove: []string{"x-request-id", "x-forwarded-proto"},
			VirtualHosts: []*routev3.VirtualHost{{
				Name:    destination,
				Domains: []string{destination, destination + ":443"},
				TypedPerFilterConfig: map[string]*anypb.Any{"envoy.filters.http.ext_authz": typed(&extauthzv3.ExtAuthzPerRoute{
					Override: &extauthzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &extauthzv3.CheckSettings{
						ContextExtensions: map[string]string{"destination": destination}}}})},
				Routes: []*routev3.Route{{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: interceptedCluster},
						Timeout:          durationpb.New(900 * time.Second),
						RetryPolicy:      &routev3.RetryPolicy{RetryOn: "connect-failure,reset-before-request"},
					}},
				}},
			}, {
				Name:    "misdirected",
				Domains: []string{"*"},
				TypedPerFilterConfig: map[string]*anypb.Any{"envoy.filters.http.ext_authz": typed(&extauthzv3.ExtAuthzPerRoute{
					Override: &extauthzv3.ExtAuthzPerRoute_Disabled{Disabled: true}})},
				ResponseHeadersToAdd: []*corev3.HeaderValueOption{
					{Header: &corev3.HeaderValue{Key: "Canyon-Egress", Value: "misdirected"}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
					{Header: &corev3.HeaderValue{Key: "content-type", Value: "text/plain; charset=utf-8"}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}},
				Routes: []*routev3.Route{{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 421, Body: &corev3.DataSource{
						Specifier: &corev3.DataSource_InlineString{InlineString: "Canyon refused this call: this connection goes to " + destination + ", but the request is for another host."}}}},
				}},
			}},
		}},
	}
	return &listenerv3.FilterChain{
		Name:             destination,
		FilterChainMatch: &listenerv3.FilterChainMatch{ServerNames: []string{destination}},
		TransportSocket: &corev3.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: typed(&tlsv3.DownstreamTlsContext{CommonTlsContext: &tlsv3.CommonTlsContext{
				AlpnProtocols: []string{"h2", "http/1.1"},
				TlsCertificateSdsSecretConfigs: []*tlsv3.SdsSecretConfig{{Name: destination, SdsConfig: &corev3.ConfigSource{
					ResourceApiVersion:    corev3.ApiVersion_V3,
					ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}}}}},
			}}),
		}},
		Filters: []*listenerv3.Filter{{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: typed(hcm)}}},
	}
}

// localReplies is a chain's own local replies (design/EGRESS.md §6; contracts/routes.md, The check, Interception):
// ext_authz #2 not answering is :3128's 503 with Canyon's line, and a connection, handshake, certificate or name that
// fails before the request is sent (UF, DF, and UH, as the dynamic forward proxy's sub-clusters answer a name that
// does not resolve no healthy upstream) is 502 upstream_failed, naming the destination, as every request here is to a
// destination with a route. Any other reply is Envoy's own.
func localReplies(destination string) *hcmv3.LocalReplyConfig {
	header := func(k, v string) *corev3.HeaderValueOption {
		return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: k, Value: v}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}
	}
	flags := func(f ...string) *accesslogv3.AccessLogFilter {
		return &accesslogv3.AccessLogFilter{FilterSpecifier: &accesslogv3.AccessLogFilter_ResponseFlagFilter{ResponseFlagFilter: &accesslogv3.ResponseFlagFilter{Flags: f}}}
	}
	unanswered := &accesslogv3.AccessLogFilter{FilterSpecifier: &accesslogv3.AccessLogFilter_AndFilter{AndFilter: &accesslogv3.AndFilter{Filters: []*accesslogv3.AccessLogFilter{
		{FilterSpecifier: &accesslogv3.AccessLogFilter_StatusCodeFilter{StatusCodeFilter: &accesslogv3.StatusCodeFilter{Comparison: &accesslogv3.ComparisonFilter{
			Op: accesslogv3.ComparisonFilter_EQ, Value: &corev3.RuntimeUInt32{DefaultValue: 503}}}}},
		flags("UAEX"),
	}}}}
	return &hcmv3.LocalReplyConfig{Mappers: []*hcmv3.ResponseMapper{
		{Filter: unanswered, Body: &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: "Canyon's egress check did not answer, so this call was not made."}}},
		{Filter: flags("UF", "DF", "UH"), StatusCode: wrapperspb.UInt32(502),
			Body: &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{
				InlineString: "Canyon could not connect to " + destination + " or verify its certificate, so it did not send this call."}},
			HeadersToAdd: []*corev3.HeaderValueOption{header("Canyon-Egress", "upstream_failed"), header("content-type", "text/plain; charset=utf-8")}},
	}}
}
