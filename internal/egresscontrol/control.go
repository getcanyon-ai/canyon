package egresscontrol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"golang.org/x/sync/singleflight"

	"github.com/getcanyon-ai/canyon/internal/destination"
)

const (
	pollEvery    = 5 * time.Second
	callersKept  = 30 * time.Second       // callers are forgotten once the policy is older: six missed polls (§3.2)
	refetchWait  = 4 * time.Second        // a refetch, inside Envoy's 5 s for the check: past it the caller is unknown, never a 503
	refetchEvery = 250 * time.Millisecond // a refetch whose answer names no caller asks again this often, refetchWait/refetchEvery times at most
	unknownFor   = 15 * time.Second       // an address the server's answers named no caller for is unknown_caller at once: the server's confirm interval
	unknownKept  = 1 << 16                // addresses kept as unknown at once; past them one more is asked about as before
	maxFlights   = 64                     // refetches running at once; a check past them is unknown_caller at once
	FleetNames   = 30                     // new names an app env may call a minute, across every replica (§3.3), while a policy names no cap of its own
	namesKept    = 500                    // names an app env keeps as let through: the records' cap (§2.4)
	alarmEvery   = time.Minute
	streamIDSize = 64 // Envoy's stream id, past which a denial's log line names none
)

// errSuperseded drops an answer to an ask made before the policy held was installed.
var errSuperseded = errors.New("an answer to an ask made before the policy held was installed, dropped")

// Control is one egress-control replica.
type Control struct {
	authv3.UnimplementedAuthorizationServer
	kube        bool                                                           // on Kubernetes, from its own settings (an https policy URL and its token file), never the policy's
	fetch       func(ctx context.Context, have, caller string) ([]byte, error) // GET /egress/policy: a 200's body, nil for a 304
	inj         Injector                                                       // Canyon Enterprise's interception (values.go); nil without it
	replicas    int                                                            // the most that run at once, which share the policy's new names
	log         *log.Logger
	now         func() time.Time
	wait, every time.Duration // a refetch's refetchWait and refetchEvery

	mu        sync.Mutex
	held      *held     // nil until the server's first 200
	current   time.Time // when the server last said held is current
	installed uint64    // how many policies were installed: an ask made under an older count is dropped
	failed    error     // the last poll's, for the minute's alarm
	names     map[appEnv]*names
	refusals  map[string]uint64    // calls refused, by reason, for the metrics
	refetched map[string]uint64    // refetches, by what they found: known, unknown or failed
	unknown   map[string]time.Time // an address the server's answers named no caller for, until when it is unknown_caller at once
	flights   int                  // refetches running

	refetches singleflight.Group

	budget time.Duration // a check's time for its value: valueBudget, shorter in tests

	// The interception listener (xds.go), built by one goroutine at a time.
	snapshots cache.SnapshotCache // what Envoy is served by LDS and SDS
	token     string              // the receiver's token, which the chains' access log sends; "" for none, and then no listener
	installs  chan struct{}       // a policy installed: the listener is built again
}

// names counts an app env's new names this minute and keeps the ones this replica let through.
type names struct {
	minute  time.Time
	fresh   int
	through map[string]bool
}

// New is a replica pulling the policy through fetch, intercepting through inj — Canyon Enterprise's, nil without it —
// letting each app env call the policy's new names over replicas a minute, rounded down: replicas is the most that
// run at once, so all of them together never let more than the policy's cap through. Until its first policy it serves
// Envoy no interception listener, and with no receiver token, token "", none ever: an inner request would go
// unrecorded, so a call to a destination with a route finds no listener and is 503, while every other call is checked
// as ever.
func New(fetch func(context.Context, string, string) ([]byte, error), inj Injector, replicas int, token string, kube bool, logger *log.Logger) (*Control, error) {
	if replicas < 1 || replicas > FleetNames {
		return nil, fmt.Errorf("the replicas, %d, are not 1 to %d", replicas, FleetNames)
	}
	c := &Control{kube: kube, fetch: fetch, inj: inj, budget: valueBudget, replicas: replicas, log: logger, now: time.Now, wait: refetchWait, every: refetchEvery, names: map[appEnv]*names{},
		refusals: map[string]uint64{}, refetched: map[string]uint64{}, unknown: map[string]time.Time{},
		snapshots: cache.NewSnapshotCache(true, oneEnvoy{}, nil), token: token, installs: make(chan struct{}, 1)}
	c.intercept(context.Background()) // no policy: nothing to fetch, and a snapshot with no listener
	return c, nil
}

// Ready is whether the replica holds a policy no more than callersKept old, which only the server's 200 gives
// it (design/EGRESS.md §3.1): one cut off from the server leaves the gateway's Service, and the others serve.
func (c *Control) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held != nil && c.now().Sub(c.current) <= callersKept
}

// Run polls every 5 s and alarms once a minute while the policy is stale, and builds the interception listener
// again as each policy is installed and as its leaves age, until ctx ends.
func (c *Control) Run(ctx context.Context) {
	go c.intercepting(ctx)
	poll, alarm := time.NewTicker(pollEvery), time.NewTicker(alarmEvery)
	defer poll.Stop()
	defer alarm.Stop()
	c.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			c.poll(ctx)
		case <-alarm.C:
			c.alarm()
		}
	}
}

func (c *Control) poll(ctx context.Context) {
	ask, cancel := context.WithTimeout(ctx, pollEvery)
	defer cancel()
	err := c.pull(ask, "")
	c.mu.Lock()
	c.failed = err
	c.mu.Unlock()
}

func (c *Control) alarm() {
	c.mu.Lock()
	h, current, failed := c.held, c.current, c.failed
	c.mu.Unlock()
	switch {
	case h == nil:
		c.log.Printf("egress-control: no policy, so every call out is refused: %v", failed)
	case c.now().Sub(current) > callersKept:
		c.log.Printf("egress-control: policy %s old, so no caller is known and every call out is refused: %v", c.now().Sub(current).Round(time.Second), failed)
	}
}

// pull asks the server once, saying the version held, none before the first: a 304 makes what it holds
// current, a 200 replaces it unless it is refused. An answer to an ask made before the policy held was
// installed is dropped, so asks never race a newer policy back out.
func (c *Control) pull(ctx context.Context, caller string) error {
	c.mu.Lock()
	var have string
	if c.held != nil {
		have = c.held.version()
	}
	asked := c.installed
	c.mu.Unlock()
	raw, err := c.fetch(ctx, have, caller)
	if err != nil {
		return err
	}
	var h *held
	if raw != nil {
		if h, err = parse(raw, c.kube); err != nil {
			return fmt.Errorf("a policy refused, whole: %w", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.installed != asked:
		return errSuperseded
	case h == nil: // a 304: have is the current version
		if c.held != nil {
			c.current = c.now()
		}
		return nil
	case c.held != nil && h.epoch == c.held.epoch && h.seq < c.held.seq:
		return fmt.Errorf("a policy refused, whole: %s is older than %s, held", h.version(), c.held.version())
	}
	if c.inj != nil { // before any check decides with h: a value it does not name under the same rev is no value
		routes := map[[3]string]int64{}
		for ae, e := range h.envs {
			for d, r := range e.routes {
				routes[[3]string{ae.app, ae.env, d}] = r.rev
			}
		}
		c.inj.Installed(h.epoch, routes)
	}
	if c.held == nil || h.newNames/c.replicas != c.held.newNames/c.replicas || h.capless != c.held.capless { // once, as it changes
		switch {
		case h.newNames/c.replicas == 0:
			c.log.Printf("egress-control: the new-name cap, %d a minute, is shared by %d replicas, so this one lets no new name through: raise the cap to %d or more", h.newNames, c.replicas, c.replicas)
		case h.capless:
			c.log.Printf("egress-control: the server sends no caps (it predates them), so this uses the default of %d new names a minute", h.newNames)
		}
	}
	c.held, c.current = h, c.now()
	c.installed++
	select {
	case c.installs <- struct{}{}:
	default: // one is waiting already, and the build it starts reads this policy
	}
	return nil
}

// caller is the app env holding source in the policy held, while the policy is at most 30 s old; a caller of
// an app env the policy does not name is no one's.
func (c *Control) caller(source string) (*held, appEnv, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.held
	if h == nil || c.now().Sub(c.current) > callersKept {
		return h, appEnv{}, false
	}
	ae, ok := h.callers[source]
	if _, named := h.envs[ae]; !named {
		return h, appEnv{}, false
	}
	return h, ae, ok
}

// refetch asks the server for source, however many checks wait on it, and again every refetchEvery while the
// policy names no caller for it, at most refetchWait/refetchEvery times inside refetchWait: a new pod calls out
// before its address reaches the cluster's API, about a second, so the first answer may rightly name no one. An
// address the server's answers named no caller for is asked about no more for unknownFor, and past maxFlights running
// none is asked about at all: a no-one's address costs one refetch per unknownFor (contracts/routes.md, The check).
func (c *Control) refetch(source string) {
	if a, err := netip.ParseAddr(source); err != nil || !a.Is4() {
		return
	}
	c.mu.Lock()
	until, known := c.unknown[source]
	c.mu.Unlock()
	if known && c.now().Before(until) {
		return
	}
	_, _, _ = c.refetches.Do(source, func() (any, error) {
		c.mu.Lock()
		if c.flights >= maxFlights {
			c.mu.Unlock()
			return nil, nil
		}
		c.flights++
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), c.wait)
		defer cancel()
		tick := time.NewTicker(c.every)
		defer tick.Stop()
		var err error
		result := "unknown"
	ask:
		for range int(c.wait / c.every) {
			err = c.pull(ctx, source)
			if _, _, ok := c.caller(source); ok {
				result = "known"
				break
			}
			result = "unknown"
			if err != nil && !errors.Is(err, errSuperseded) {
				result = "failed"
			}
			select {
			case <-ctx.Done():
				break ask
			case <-tick.C:
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.flights--
		c.refetched[result]++
		if result == "unknown" { // the server answered and named no one: a refetch that failed is asked again at the next check
			c.forgetUnknown()
			if len(c.unknown) < unknownKept {
				c.unknown[source] = c.now().Add(unknownFor)
			}
		}
		return nil, err
	})
}

// forgetUnknown drops the addresses whose unknownFor has passed, once the map is full; c.mu is held.
func (c *Control) forgetUnknown() {
	if len(c.unknown) < unknownKept {
		return
	}
	now := c.now()
	for a, until := range c.unknown {
		if !now.Before(until) {
			delete(c.unknown, a)
		}
	}
}

// admit counts name against ae's new names this minute and answers whether the rate limit lets it on, and
// whether it took one of the minute's: a name the env knows — on its known[], allowed by its allowlist, a
// platform destination, or let through by this replica already — is not new.
func (c *Control) admit(h *held, ae appEnv, name string) (ok, fresh bool) {
	env := h.envs[ae]
	if env.known[name] || h.platform[name] {
		return true, false
	}
	for _, entry := range env.allowed {
		if destination.Allows(entry, name) {
			return true, false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.names[ae]
	if n == nil {
		n = &names{through: map[string]bool{}}
		c.names[ae] = n
	}
	if n.through[name] {
		return true, false
	}
	if minute := c.now().Truncate(time.Minute); !minute.Equal(n.minute) {
		n.minute, n.fresh = minute, 0
	}
	if n.fresh >= h.newNames/c.replicas { // this replica's share of the policy's cap, rounded down
		return false, false
	}
	n.fresh++
	return true, true
}

// settle keeps a name let through as known to this replica, and gives back the minute's count a name took
// and was refused: a name refused was never let through, so it counts nothing.
func (c *Control) settle(ae appEnv, name string, fresh, through bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.names[ae]
	switch {
	case n == nil:
	case through && len(n.through) < namesKept:
		n.through[name] = true
	case fresh && !through && n.minute.Equal(c.now().Truncate(time.Minute)):
		n.fresh--
	}
}
