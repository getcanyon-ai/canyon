//go:build !noee

package egresscontrol

import (
	"slices"
	"testing"
	"time"
)

// On Kubernetes a call its pod's address alone names is never intercepted (§4.3): its route's destination passes
// through untouched, while the same env's call its token names is intercepted, as a token_only env's is.
func TestACallerNamedByItsAddressIsNeverIntercepted(t *testing.T) {
	k := newCluster(t, "k1")
	r := onKube(t, kubePolicy(t, 42, newPKI(t).fingerprint, []map[string]any{k.public(t)}))
	at := r.clock.now().Add(-time.Minute)
	for _, c := range []struct{ source, account, env string }{{preview, previewAccount, "preview"}, {prod, prodAccount, "prod"}} {
		v := r.proxied(t, c.source, "Bearer "+k.projected(t, c.account, at, nil), "api.stripe.com:443")
		callerIs(t, c.env+" by its token", v, c.env)
		if v.set["x-canyon-intercept"] != "1" {
			t.Errorf("%s's route, its caller named by its token: %v %v, want it intercepted", c.env, v.set, v.metadata)
		}
	}
	v := r.proxied(t, prod, "", "api.stripe.com:443")
	callerIs(t, "prod, by its pod's address", v, "prod")
	if v.set["x-canyon-intercept"] != "" || v.metadata["destination"] != nil || !slices.Contains(v.remove, "x-canyon-intercept") {
		t.Errorf("prod's route, its caller named by its address: %v %v %v, want it passed through untouched", v.set, v.metadata, v.remove)
	}
}
