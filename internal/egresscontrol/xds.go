package egresscontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/sotw/v3"
)

// Interception (design/EGRESS.md §4.2, §4.5; contracts/routes.md, Interception): egress-control serves the Envoy
// beside it, over ADS on the address it answers the check on, one internal listener by LDS — a filter chain per
// inject destination, matched by the name the app's TLS asks for — and each chain's leaf by SDS, and nothing else:
// no clusters, routes or endpoints of their own, so a new env needs no push. Both are Canyon Enterprise's Injector's
// (#2922), built from the policy held and the leaves fetched alone, so replicas holding the same policy serve the same
// listener; their secrets differ, as the server mints a leaf for each answer.

const (
	InterceptListener  = "intercept"     // the one internal listener, which Envoy's one internal cluster names
	InterceptedCluster = "intercepted"   // Envoy's static cluster an inner request leaves by: TLS it originates and verifies
	InterceptLog       = "intercept"     // the chains' access log, by whose name the receiver tells an inner request's entry
	CallerObject       = "canyon.caller" // the filter-state object holding the caller ext_authz #1 named, <app>/<env>
	envoyNode          = "envoy"         // the one Envoy a replica serves, whatever node it names
)

// intercept serves Envoy the interception listener again from the policy held, called by one goroutine at a time (New,
// then intercepting): the Injector's, for the policy's signing root and inject destinations — none of either with no
// policy or no receiver token, as an inner request would go unrecorded — and without an Injector none, so an
// intercepted call finds no listener: never forwarded without its check, or in clear.
func (c *Control) intercept(ctx context.Context) {
	c.mu.Lock()
	h := c.held
	c.mu.Unlock()
	var signing string
	var inject []string
	if h != nil && c.token != "" {
		signing, inject = h.signing, h.inject
	}
	s := noListener()
	if c.inj != nil {
		s = c.inj.Snapshot(ctx, signing, inject)
	}
	if err := c.snapshots.SetSnapshot(ctx, envoyNode, s); err != nil {
		c.log.Printf("egress-control: the interception listener not served: %v", err)
	}
}

// intercepting builds the listener again whenever a policy is installed, and every pollEvery for the leaves' ends,
// until ctx ends.
func (c *Control) intercepting(ctx context.Context) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.installs:
		case <-tick.C:
		}
		c.intercept(ctx)
	}
}

// noListener is what Envoy is served with no interception: no listener and no secret, each under the version of
// nothing, as an Injector's with no chain is.
func noListener() *cache.Snapshot {
	none := hex.EncodeToString(sha256.New().Sum(nil)[:16])
	s := &cache.Snapshot{}
	s.Resources[types.Listener] = cache.NewResources(none, nil)
	s.Resources[types.Secret] = cache.NewResources(none, nil)
	return s
}

// ads answers Envoy's aggregated discovery stream from the snapshot cache: LDS and SDS, all it holds.
type ads struct {
	discoveryv3.UnimplementedAggregatedDiscoveryServiceServer
	sotw sotw.Server
}

func (a ads) StreamAggregatedResources(s discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesServer) error {
	return a.sotw.StreamHandler(s, resource.AnyType)
}

// refusals logs each answer Envoy refused, by its type and version alone: Envoy keeps what it held before, so a
// refused listener or leaf is interception that did not move, which only this line tells. Envoy's own words about
// it are left out, as they may quote what it was sent.
type refusals struct{ log *log.Logger }

func (refusals) OnStreamOpen(context.Context, int64, string) error { return nil }
func (refusals) OnStreamClosed(int64, *corev3.Node)                {}
func (refusals) OnStreamResponse(context.Context, int64, *discoveryv3.DiscoveryRequest, *discoveryv3.DiscoveryResponse) {
}

func (r refusals) OnStreamRequest(_ int64, req *discoveryv3.DiscoveryRequest) error {
	if req.GetErrorDetail() != nil {
		r.log.Printf("egress-control: Envoy refused an answer of %s and keeps version %q: code %d", req.GetTypeUrl(), req.GetVersionInfo(), req.GetErrorDetail().GetCode())
	}
	return nil
}

// oneEnvoy keys every node to the one snapshot: a replica answers on loopback, the Envoy beside it alone.
type oneEnvoy struct{}

func (oneEnvoy) ID(*corev3.Node) string { return envoyNode }
