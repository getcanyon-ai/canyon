package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// One bucket per source address, 120 tokens refilled at 2.0 a second, and a full bucket forgotten:
// the bound the three browser and mint flow routes share (design/AUTH.md §4.2).
func TestTheLimiterBoundsOneAddressAndRefills(t *testing.T) {
	now := time.Now()
	l := newLimiter(func() time.Time { return now }, nil)
	for i := 0; i < flowBurst; i++ {
		if _, ok := l.take("10.4.2.19"); !ok {
			t.Fatalf("take %d refused under the bound", i)
		}
	}
	if wait, ok := l.take("10.4.2.19"); ok || wait <= 0 || wait > time.Second/flowRate {
		t.Fatalf("past the bound: ok=%v wait=%s", ok, wait)
	}
	if _, ok := l.take("10.4.2.20"); !ok {
		t.Fatal("another address shares the bucket")
	}
	now = now.Add(time.Second / flowRate)
	if _, ok := l.take("10.4.2.19"); !ok {
		t.Fatal("one interval later the bucket has not refilled one token")
	}
}

// The middleware keys its bucket by the request's peer: two addresses are two buckets at the HTTP
// layer, not only in the limiter's own take.
func TestTheLimitMiddlewareKeysByThePeer(t *testing.T) {
	l := newLimiter(time.Now, nil)
	h := l.limit(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for i := 0; i < flowBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/auth/start", nil)
		req.RemoteAddr = "10.4.2.19:5000"
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("request %d from the first address: %d", i, w.Code)
		}
	}
	first, second := httptest.NewRequest(http.MethodGet, "/auth/start", nil), httptest.NewRequest(http.MethodGet, "/auth/start", nil)
	first.RemoteAddr, second.RemoteAddr = "10.4.2.19:5001", "10.4.2.20:5000"
	w1, w2 := httptest.NewRecorder(), httptest.NewRecorder()
	h(w1, first)
	h(w2, second)
	if w1.Code != http.StatusTooManyRequests || w2.Code != http.StatusNoContent {
		t.Fatalf("the drained address: %d, another address: %d; want 429 and 204", w1.Code, w2.Code)
	}
}

// Behind a trusted proxy the requester is the right-most X-Forwarded-For address that is not a
// proxy; a request whose peer is not trusted is its peer, whatever it claims; and with no proxies
// set nothing is trusted (design/AUTH.md §4.2, decision D3).
func TestTheSourceIsTheRequesterBehindATrustedProxyAndThePeerOtherwise(t *testing.T) {
	trusted, err := proxiesOf("10.0.48.0/20, 192.0.2.7") // the ALB's subnets, never the VPC
	if err != nil {
		t.Fatal(err)
	}
	ask := func(peer string, forwarded ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/auth/device", nil)
		r.RemoteAddr = peer + ":5000"
		for _, f := range forwarded {
			r.Header.Add("X-Forwarded-For", f)
		}
		return r
	}
	for _, c := range []struct {
		name string
		p    proxies
		r    *http.Request
		want string
	}{
		{"the ALB's client", trusted, ask("10.0.49.214", "198.51.100.4"), "198.51.100.4"},
		{"a spoofed hop before the real client", trusted, ask("10.0.49.214", "6.6.6.6, 198.51.100.4"), "198.51.100.4"},
		{"two proxies, two headers", trusted, ask("10.0.49.214", "198.51.100.4, 192.0.2.7", "10.0.50.3"), "198.51.100.4"},
		{"a peer nobody trusts", trusted, ask("203.0.113.9", "198.51.100.4"), "203.0.113.9"},
		{"no proxies set", nil, ask("10.0.49.214", "198.51.100.4"), "10.0.49.214"},
		{"every hop a proxy", trusted, ask("10.0.49.214", "10.0.51.1"), "10.0.49.214"},
		{"a hop that is no address", trusted, ask("10.0.49.214", "198.51.100.4, unknown"), "10.0.49.214"},
	} {
		if got := c.p.source(c.r); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "a proxy", "10.0.0.0/16,,", "::ffff:10.0.0.1"} {
		if _, err := proxiesOf(bad); err == nil {
			t.Errorf("CANYON_TRUSTED_PROXIES=%q booted", bad)
		}
	}
}
