// Package intercept is Canyon Enterprise's interception in egress-control (design/EGRESS.md §4.1, §4.2; contracts/
// routes.md, Interception; #2922): a route's value, asked of the server on first use and held in memory alone, and
// the interception listener Envoy is served, a chain and a leaf for each inject destination. egress-control reaches it
// through its Injector alone; without it a replica serves no listener and refuses a call the policy routes
// inject_unavailable.
package intercept

import (
	"context"
	"log"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Interceptor is one replica's interception: what it asks the server, the receiver's token its chains' access log
// sends, its values, and its leaves.
type Interceptor struct {
	leafOf    func(ctx context.Context, destination string) ([]byte, error)           // GET /egress/leaf: a 200's body
	valueAsk  func(ctx context.Context, app, env, destination string) ([]byte, error) // GET /egress/route-value: a 200's body
	token     string
	log       *log.Logger
	now       func() time.Time
	leafRound time.Duration // how long one build's asks take at most

	mu           sync.Mutex
	epoch        string             // the policy installed's epoch
	routes       map[routeKey]int64 // the policy installed's routes, under their revs
	values       map[routeKey]value // route values, in this memory alone
	valueFlights singleflight.Group

	leaves map[string]*leaf  // by inject destination, each named under the signing root and valid; Snapshot's alone
	asks   map[string]asking // by inject destination whose leaf could not be read; Snapshot's alone
}

// Config is an Interceptor's: Now is the replica's clock, time.Now when nil, and LeafRound a build's asks' bound,
// leafRound when zero.
type Config struct {
	LeafOf    func(ctx context.Context, destination string) ([]byte, error)
	ValueAsk  func(ctx context.Context, app, env, destination string) ([]byte, error)
	Token     string
	Log       *log.Logger
	Now       func() time.Time
	LeafRound time.Duration
}

// New is a replica's interception, asking the server through c's LeafOf and ValueAsk.
func New(c Config) *Interceptor {
	i := &Interceptor{leafOf: c.LeafOf, valueAsk: c.ValueAsk, token: c.Token, log: c.Log, now: c.Now, leafRound: c.LeafRound, values: map[routeKey]value{}}
	if i.now == nil {
		i.now = time.Now
	}
	if i.leafRound == 0 {
		i.leafRound = leafRound
	}
	return i
}
