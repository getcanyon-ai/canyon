package intercept

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// secret is a route value's shape, synthetic: what no error may carry.
var secret = "sk_live_" + strings.Repeat("v", 24)

// TestAValueErrorNamesNoByteOfIt (design/EGRESS.md §4.1): an answer that is no value, and a value under a rev other
// than the policy's, are each an error naming no byte of what the server answered.
func TestAValueErrorNamesNoByteOfIt(t *testing.T) {
	for _, raw := range []string{`{"rev":1,"value":"` + secret + "\n" + `"}`, `{"rev":"` + secret + `"}`, `{"rev":0,"value":"` + secret + `"}`, secret} {
		if _, err := parseValue([]byte(raw)); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("parseValue(%q) answered %v, want an error naming no byte of it", raw, err)
		}
	}
	i := New(Config{ValueAsk: func(context.Context, string, string, string) ([]byte, error) {
		return []byte(`{"rev":999,"value":"` + secret + `"}`), nil
	}, Log: log.New(io.Discard, "", 0)})
	if _, err := i.Value(time.Now().Add(time.Second), "claims-portal", "prod", "hooks.slack.com", "bearer", 3); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("a value under another rev: %v, want an error naming no byte of it", err)
	}
}

// TestAValueIsHeldOnlyWhileThePolicyNamesItsRoute (contracts/routes.md, Interception, The value): a value is held
// under the rev the installed policy names its route under, and dropped when the route goes, its rev changes or the
// epoch does; one the installed policy never named is asked for every time and held for none.
func TestAValueIsHeldOnlyWhileThePolicyNamesItsRoute(t *testing.T) {
	asks := 0
	i := New(Config{ValueAsk: func(context.Context, string, string, string) ([]byte, error) {
		asks++
		return []byte(`{"rev":2,"value":"` + secret + `"}`), nil
	}, Log: log.New(io.Discard, "", 0)})
	k := routeKey{"claims-portal", "prod", "api.stripe.com"}
	value := func() {
		t.Helper()
		if v, err := i.Value(time.Now().Add(time.Second), k[0], k[1], k[2], "bearer", 2); err != nil || v != "Bearer "+secret {
			t.Fatalf("the value: %v", err)
		}
	}
	epoch := strings.Repeat("a", 32)
	value()
	value()
	if asks != 2 {
		t.Fatalf("a route no policy names was asked for %d times in two checks, want each time: held for none", asks)
	}
	i.Installed(epoch, map[[3]string]int64{k: 2})
	value()
	value()
	if asks != 3 {
		t.Fatalf("asked %d times, want once more and then held", asks)
	}
	for _, c := range []struct {
		name   string
		epoch  string
		routes map[[3]string]int64
	}{
		{"the route gone", epoch, map[[3]string]int64{}},
		{"its rev changed", epoch, map[[3]string]int64{k: 3}},
		{"the epoch changed", strings.Repeat("b", 32), map[[3]string]int64{k: 2}},
	} {
		i.Installed(epoch, map[[3]string]int64{k: 2})
		value()
		i.Installed(c.epoch, c.routes)
		if _, held := i.heldValue(k, 2); held {
			t.Errorf("%s: the value is still held", c.name)
		}
	}
}
