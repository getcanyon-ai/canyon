package main

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The sign-in flow routes are bounded per source address (contracts/routes.md, the Open paragraph): a token
// bucket of flowBurst refilled at flowRate a second, in this process's memory. Why these numbers: design/AUTH.md's threat table.
const (
	flowBurst = 120
	flowRate  = 2.0 // tokens a second

	// GET /auth/app is bounded per Canyon session, never per address (contracts/routes.md, its row):
	// behind an ingress every browser shares one address, and a page whose app session lapsed sends
	// its every request there. 60 at once, then one a minute: a session writes at most that many rows.
	appSignInBurst = 60
	appSignInRate  = 1.0 / 60
)

type limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket

	burst, rate float64 // a bucket's tokens when full, and the tokens it gains a second
	from        proxies // whose word on a request's source is taken (limit keys by it)
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(now func() time.Time, from proxies) *limiter {
	return &limiter{now: now, burst: flowBurst, rate: flowRate, buckets: map[string]*bucket{}, from: from}
}

// take spends one token for addr (or any key), or says how long until the next one when there is none.
func (l *limiter) take(addr string) (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, found := l.buckets[addr]
	if !found {
		for k, old := range l.buckets { // a full bucket is an address with nothing in flight: forget it, so the map holds the addresses in play and not every address ever seen
			if old.tokens+l.rate*now.Sub(old.at).Seconds() >= l.burst {
				delete(l.buckets, k)
			}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[addr] = b
	}
	b.tokens = min(l.burst, b.tokens+l.rate*now.Sub(b.at).Seconds())
	b.at = now
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / l.rate * float64(time.Second)), false
	}
	b.tokens--
	return 0, true
}

func (l *limiter) limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if wait, ok := l.take(l.from.source(r)); !ok {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in requests from this address", map[string]any{"retry_after_s": int(math.Ceil(wait.Seconds()))})
			return
		}
		next(w, r)
	}
}

// peerAddr is the connection's peer without its port.
func peerAddr(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// proxies is CANYON_TRUSTED_PROXIES: the proxies in front of this server, the only ones whose word on
// where a request came from is taken (design/AUTH.md §4.2, decision D3). Unset, none is.
type proxies []netip.Prefix

// proxiesOf reads the setting: prefixes or addresses, comma-separated. One it cannot read refuses the
// boot rather than trusting less, or more, than was meant.
func proxiesOf(s string) (proxies, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var p proxies
	for _, one := range strings.Split(s, ",") {
		one = strings.TrimSpace(one)
		prefix, err := netip.ParsePrefix(one)
		if err != nil {
			addr, aerr := netip.ParseAddr(one)
			if aerr != nil {
				return nil, fmt.Errorf("CANYON_TRUSTED_PROXIES: %q is neither a prefix nor an address", one)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Is4In6() { // trusts() unmaps the peer, so a mapped entry would never match
			return nil, fmt.Errorf("CANYON_TRUSTED_PROXIES: write %q as plain IPv4", one)
		}
		p = append(p, prefix.Masked())
	}
	return p, nil
}

func (p proxies) trusts(addr netip.Addr) bool {
	for _, prefix := range p {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// source is where a request came from — the bucket's key and a grant's requested_by: its peer,
// unless the peer is a trusted proxy, and then the right-most X-Forwarded-For address that is not
// one, which is what the outermost proxy saw. A hop that is no address ends the walk at the peer.
func (p proxies) source(r *http.Request) string {
	peer := peerAddr(r)
	if addr, err := netip.ParseAddr(peer); err != nil || !p.trusts(addr) {
		return peer
	}
	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer
		}
		if !p.trusts(addr) {
			return addr.Unmap().String()
		}
	}
	return peer
}
