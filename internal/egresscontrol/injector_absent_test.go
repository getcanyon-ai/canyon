package egresscontrol

import (
	"context"
	"testing"

	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc/codes"
)

// contracts/routes.md, Licence, A server built without Canyon Enterprise (#2922): a replica without Canyon
// Enterprise's Injector serves no interception listener, and refuses every call to a destination the policy routes
// inject_unavailable — a CONNECT, an absolute-form https:// and a request inside a tunnel alike — never sending it
// without its credential, while a call to a destination with no route goes on as ever.
func TestWithoutEnterpriseARoutedCallIsNeverSentWithoutItsCredential(t *testing.T) {
	r := newReplica(t, 1, &server{policy: routed(t, 42, newPKI(t).fingerprint, inject)})
	r.inj = nil
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r.intercept(context.Background())
	unavailable := "Canyon could not add the app's credential for api.stripe.com, so it did not send this call. Try again. If this persists, ask an admin."
	refusedAs(t, "a CONNECT to a routed destination", r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:443"), "inject_unavailable", 502, unavailable)
	refusedAs(t, "https:// to a routed destination", r.outer(t, claimsProd, "GET", "https", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	refusedAs(t, "http:// to a routed destination", r.outer(t, claimsProd, "GET", "http", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	refusedAs(t, "a request inside a tunnel to it", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	if v := r.outer(t, claimsProd, "CONNECT", "", "api.adyen.com:443"); v.code != int32(codes.OK) || v.set["x-canyon-intercept"] != "" {
		t.Errorf("a CONNECT to a destination with no route: %d %v, want it let through, not intercepted", v.code, v.set)
	}
	s, err := r.snapshots.GetSnapshot(envoyNode)
	if err != nil {
		t.Fatal(err)
	}
	if l, sec := s.GetResources(resource.ListenerType), s.GetResources(resource.SecretType); len(l) != 0 || len(sec) != 0 {
		t.Errorf("Envoy is served %d listeners and %d secrets, want none", len(l), len(sec))
	}
}
