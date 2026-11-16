package egresscontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// withCaps is raw, a policy, carrying caps as the server writes them (#2691); caps nil leaves the key out, as a
// server from before it does.
func withCaps(t *testing.T, raw []byte, caps any) []byte {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p["caps"] = caps
	if caps == nil {
		delete(p, "caps")
	}
	return marshal(t, p)
}

// The policy's caps.new_names is the rate limit (design/EGRESS.md §3.3; #2691): each replica lets through its share,
// the cap over the most replicas that run at once, rounded down, and a refusal's body names the cap. A policy naming
// no caps, a server's from before them, reads as the default, 30, said once. A cap out of its range refuses the
// policy whole, the one held serving on. A cap below the replicas leaves each a share of none, said once: no new
// name gets through, never more than the cap.
func TestEgressCaps(t *testing.T) {
	t.Run("the policy's cap, shared", func(t *testing.T) {
		r := newReplica(t, 2, &server{policy: withCaps(t, claims(t, 42), map[string]int{"new_names": 7})})
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		for i := range 3 { // 7 over 2, rounded down
			allowed(t, fmt.Sprintf("new name %d of 3", i+1), r.connect(t, preview, fmt.Sprintf("n%d.example.com:443", i)))
		}
		a := r.connect(t, preview, "one-too-many.example.com:443")
		refused(t, "past the share", "rate_limited", a)
		if a.body != "Canyon refused this call: claims-portal preview called more than 7 new destinations this minute. It can call them from the next minute, or an admin can raise the limit in Settings › Outbound." {
			t.Errorf("body %q, want it to name the policy's cap", a.body)
		}
		if strings.Contains(r.logs.String(), "sends no caps") {
			t.Errorf("logged %q for a policy naming its caps", r.logs.String())
		}
	})
	t.Run("no caps: the default, said once", func(t *testing.T) {
		r := newReplica(t, 1, &server{policy: withCaps(t, claims(t, 42), nil)})
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		for i := range FleetNames {
			allowed(t, fmt.Sprintf("new name %d", i+1), r.connect(t, preview, fmt.Sprintf("n%d.example.com:443", i)))
		}
		refused(t, "past the default", "rate_limited", r.connect(t, preview, "one-too-many.example.com:443"))
		r.srv.set(false, withCaps(t, claims(t, 43), nil))
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(r.logs.String(), "egress-control: the server sends no caps (it predates them), so this uses the default of 30 new names a minute"); n != 1 {
			t.Errorf("logged the default %d times, want once: %q", n, r.logs.String())
		}
	})
	t.Run("a cap out of range: refused whole", func(t *testing.T) {
		for _, caps := range []any{map[string]int{"new_names": 0}, map[string]int{"new_names": 601}, map[string]any{"new_names": "30"}, map[string]any{}, map[string]any{"new_names": 30.5}} {
			r := started(t, 1)
			r.srv.set(false, withCaps(t, claims(t, 43), caps))
			if err := r.pull(context.Background(), ""); err == nil || r.held.seq != 42 || r.held.newNames != FleetNames {
				t.Errorf("caps %v: %v, holding seq %d and %d new names, want refused whole and the one held serving", caps, err, r.held.seq, r.held.newNames)
			}
		}
	})
	t.Run("a cap below the replicas: none through, said once", func(t *testing.T) {
		r := newReplica(t, 3, &server{policy: withCaps(t, claims(t, 42), map[string]int{"new_names": 2})})
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		refused(t, "a share of none", "rate_limited", r.connect(t, preview, "first.example.com:443"))
		allowed(t, "a name on the env's known[]", r.connect(t, preview, "registry.npmjs.org:443"))
		r.srv.set(false, withCaps(t, claims(t, 43), map[string]int{"new_names": 2}))
		if err := r.pull(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		const want = "egress-control: the new-name cap, 2 a minute, is shared by 3 replicas, so this one lets no new name through: raise the cap to 3 or more"
		if n := strings.Count(r.logs.String(), want); n != 1 {
			t.Errorf("logged %d times, want once: %q", n, r.logs.String())
		}
	})
}
