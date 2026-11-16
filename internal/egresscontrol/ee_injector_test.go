//go:build !noee

package egresscontrol

import (
	"context"
	"log"
	"slices"
	"testing"
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/getcanyon-ai/canyon/ee/outboundkeys/intercept"
)

// In a build with Canyon Enterprise each test replica intercepts as a released one does, with
// ee/outboundkeys/intercept on the replica's clock; the interception tests (inject_test.go, xds_test.go) are this
// build's alone.
func init() {
	testInjector = func(srv *server, clk *clock, logs *syncBuffer) Injector {
		return intercept.New(intercept.Config{LeafOf: srv.leaf, ValueAsk: srv.value, Token: receiverToken, Log: log.New(logs, "", 0), Now: clk.now})
	}
}

// The interception's bounds, ee/outboundkeys/intercept's, which these tests hold its behaviour to.
const (
	valueWait   = 2 * time.Second // one ask for a value
	leafWait    = 2 * time.Second // one ask for a leaf
	leafBackoff = 5 * time.Minute // the longest a leaf that cannot be read waits to be asked again
)

// withLeafRound gives r an interception whose builds' asks take round at most.
func (r replica) withLeafRound(round time.Duration) {
	r.inj = intercept.New(intercept.Config{LeafOf: r.srv.leaf, ValueAsk: r.srv.value, Token: receiverToken, Log: log.New(r.logs, "", 0), Now: r.clock.now, LeafRound: round})
}

// leavesHeld is the inject destinations whose leaf r serves Envoy, sorted.
func (r replica) leavesHeld(t *testing.T) []string {
	t.Helper()
	s, err := r.snapshots.GetSnapshot(envoyNode)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range s.GetResources(resource.SecretType) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// holds is whether r serves Envoy destination's leaf.
func (r replica) holds(t *testing.T, destination string) bool {
	t.Helper()
	return slices.Contains(r.leavesHeld(t), destination)
}

// chainOf is destination's filter chain as an interception serving its leaf builds it.
func chainOf(t *testing.T, destination string) *listenerv3.FilterChain {
	t.Helper()
	p, now := newPKI(t), time.Now()
	i := intercept.New(intercept.Config{LeafOf: func(context.Context, string) ([]byte, error) { return p.leaf(t, destination, now), nil }, Token: receiverToken, Log: log.Default()})
	l, ok := i.Snapshot(context.Background(), p.fingerprint, []string{destination}).Resources[types.Listener].Items[InterceptListener].Resource.(*listenerv3.Listener)
	if !ok || len(l.GetFilterChains()) != 1 {
		t.Fatalf("no chain for %s", destination)
	}
	return l.GetFilterChains()[0]
}

// localReplies is destination's chain's own local replies.
func localReplies(t *testing.T, destination string) *hcmv3.LocalReplyConfig {
	t.Helper()
	hcm := &hcmv3.HttpConnectionManager{}
	if err := chainOf(t, destination).GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	return hcm.GetLocalReplyConfig()
}
