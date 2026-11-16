package egresscontrol

import (
	"context"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

// A route's value and the interception listener are Canyon Enterprise's (ee/outboundkeys/intercept; design/EGRESS.md
// §4.1, §4.2; #2922), which a replica reaches through its Injector alone. The check gives a value valueBudget from when
// it began, a refetch of the caller included, inside Envoy's 5 s, so a value not read in time is inject_unavailable,
// never the 503. Without an Injector a replica serves no interception listener and refuses every call the policy
// routes inject_unavailable: never forwarded without its credential (contracts/routes.md, Licence, A server built
// without Canyon Enterprise).

const (
	valueBudget = 4500 * time.Millisecond
	valueBytes  = 64 << 10 // one answer, its value 8,192 characters at most by the schema
)

// Injector is Canyon Enterprise's interception in a replica.
type Injector interface {
	// Value is the header's value of app env's route to destination under rev, made by scheme, by deadline; an error
	// refuses the call inject_unavailable.
	Value(deadline time.Time, app, env, destination, scheme string, rev int64) (string, error)
	// Installed is told each policy the replica installs, under its lock before it decides with it: its epoch and
	// every route it names, by app, env and destination, under its rev.
	Installed(epoch string, routes map[[3]string]int64)
	// Snapshot is what Envoy is served for signing and inject, built by one goroutine at a time.
	Snapshot(ctx context.Context, signing string, inject []string) *cache.Snapshot
}

// routeKey names one route: the app env holding it and its destination.
type routeKey struct {
	ae          appEnv
	destination string
}
