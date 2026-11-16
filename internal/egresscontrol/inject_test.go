//go:build !noee

package egresscontrol

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

// The check's interception (contracts/routes.md, Interception): ext_authz #1 on a destination with a route, and
// ext_authz #2 on each request inside an intercepted tunnel, with the route's value.

// routesOf calls each with every route of app prod in p.
func routesOf(p map[string]any, app string, each func(env map[string]any, i int, r map[string]any)) {
	for _, e := range p["envs"].([]any) {
		if env := e.(map[string]any); env["app"] == app {
			for i, r := range env["routes"].([]any) {
				each(env, i, r.(map[string]any))
			}
		}
	}
}

// routeSet changes the route of app prod to destination in policy to header, scheme and rev, under the next seq.
func routeSet(t *testing.T, policy []byte, app, destination, header, scheme string, rev int) []byte {
	t.Helper()
	return policyEdit(t, policy, func(p map[string]any) {
		routesOf(p, app, func(_ map[string]any, _ int, r map[string]any) {
			if r["destination"] == destination {
				r["header"], r["scheme"], r["rev"] = header, scheme, rev
			}
		})
	})
}

// valueOf is GET /egress/route-value's 200.
func valueAnswer(t *testing.T, rev int, v string) []byte {
	return marshal(t, map[string]any{"rev": rev, "value": v})
}

// injecting is a replica holding routed's policy under a signing root, every route's value readable: claims-portal
// prod's to api.stripe.com, rev 2, is secret.
func injecting(t *testing.T) replica {
	t.Helper()
	p := newPKI(t)
	r := newReplica(t, 1, &server{policy: routed(t, 42, p.fingerprint, inject), values: map[string][]byte{}})
	for i, app := range slices.Sorted(func(yield func(string) bool) {
		for a := range inject {
			if !yield(a) {
				return
			}
		}
	}) {
		for j, d := range inject[app] {
			r.srv.values[app+"/prod/"+d] = valueAnswer(t, 100*i+j+1, secret)
		}
	}
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return r
}

// injected is an OK that overwrites header with want, whatever the app sent, and removes remove.
func injectedAs(t *testing.T, what string, v verdict, header, want, remove string) {
	t.Helper()
	removes := []string{remove}
	if remove == "x-canyon-caller" {
		removes = []string{"x-canyon-caller", "x-canyon-destination"}
	}
	if v.code != int32(codes.OK) || v.appended || len(v.set) != 1 || v.set[header] != want || !slices.Equal(v.remove, removes) || v.metadata["outcome"] != "allowed" {
		t.Errorf("%s: answered %d, set %v (appended %v), removed %v, %v; want OK overwriting %s and removing %s", what, v.code, v.set, v.appended, v.remove, v.metadata, header, remove)
	}
}

// TestMisdirectedAuthorityDenied: ext_authz #2 sends a request on only when its :authority or Host, as Envoy hands it
// over — its one trailing dot dropped already, by the chain's connection manager — is the destination of the chain
// it came in by, compared canonically: lower-case, a :443 dropped. Any other is misdirected, 421: another name,
// another port, an address, egress-control's own loopback port where the leaves' keys are served, a dot Envoy left, a
// name that folds into the destination only past ASCII. It refuses so on its own, whatever the listener's routes do
// (TestTheListenerAnswersAMisdirectedAuthority421), and the two agree (TestTheListenerAndTheCheckAgree).
func TestMisdirectedAuthorityDenied(t *testing.T) {
	r := injecting(t)
	for _, authority := range []string{"api.stripe.com", "API.Stripe.COM", "api.stripe.com:443"} {
		injectedAs(t, "the authority "+authority, r.inner(t, "api.stripe.com", "claims-portal/prod", authority), "authorization", "Bearer "+secret, "x-canyon-caller")
	}
	body := "Canyon refused this call: this connection goes to api.stripe.com, but the request is for another host."
	for _, authority := range []string{"127.0.0.1:7783", "127.0.0.1", "localhost:7783", "[::1]:7783", "api.stripe.co", "api.stripe.com:8443",
		"api.stripe.com:0443", "api.stripe.com:80", "api.stripe.com.", "api.stripe.com.:443", "api.stripe.com..", "evil.example.com", "stripe.com", "x.api.stripe.com", "",
		"api.stripe.Kom", "аpi.stripe.com", "api.stripe.com:443:443", "api.stripe.com\x00"} {
		refusedAs(t, "the authority "+authority, r.inner(t, "api.stripe.com", "claims-portal/prod", authority), "misdirected", 421, body)
	}
	if n := len(r.srv.valueAsks); n != 1 {
		t.Errorf("%d values asked for, want one: a misdirected request asks for none", n)
	}
	// the Kelvin sign, which lower-cases to k: hooks.slack.com spelt with it twice is no name of the chain's
	refusedAs(t, "hoo\u212as.slac\u212a.com", r.inner(t, "hooks.slack.com", "claims-portal/prod", "hoo\u212as.slac\u212a.com"), "misdirected", 421,
		"Canyon refused this call: this connection goes to hooks.slack.com, but the request is for another host.")
}

// envoyStrips is the authority as the chain's connection manager hands it on, strip_trailing_host_dot set: one
// trailing dot of the host dropped, before a port too.
func envoyStrips(a string) string {
	host, port := a, ""
	if i := strings.LastIndex(a, ":"); i >= 0 && !strings.Contains(a[i:], "]") {
		host, port = a[:i], a[i:]
	}
	return strings.TrimSuffix(host, ".") + port
}

// TestTheListenerAndTheCheckAgree: for every authority an app may send inside a tunnel, the chain's routes send it on
// exactly when ext_authz #2 takes it, both judging what Envoy hands on, its one trailing dot dropped: so a request the
// check would take is never answered 421 by the routes, nor the other way about (design/EGRESS.md §4.1).
func TestTheListenerAndTheCheckAgree(t *testing.T) {
	r := injecting(t)
	hcm := &hcmv3.HttpConnectionManager{}
	if err := chainOf(t, "api.stripe.com").GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	if !hcm.GetStripTrailingHostDot() {
		t.Fatal("the chain keeps the trailing dot: this test judges what Envoy would not hand on")
	}
	for _, sent := range []string{"api.stripe.com", "API.Stripe.COM", "api.stripe.com:443", "api.stripe.com.", "api.stripe.com.:443", "API.STRIPE.COM.:443",
		"api.stripe.com..", "api.stripe.com..:443", "api.stripe.com:8443", "api.stripe.com.:8443", "127.0.0.1:7783", "localhost.:7783", "api.stripe.co.",
		"x.api.stripe.com.", ".", ""} {
		handed := envoyStrips(sent)
		routes := virtualHost(hcm.GetRouteConfig(), handed) == "api.stripe.com"
		check := r.inner(t, "api.stripe.com", "claims-portal/prod", handed).code == int32(codes.OK)
		if routes != check {
			t.Errorf("%q, handed on as %q: the routes send it on %v and the check takes it %v, want the two to agree", sent, handed, routes, check)
		}
	}
}

// TestInjectRequiresTLS: a call to a destination with a route goes on only over TLS — a CONNECT to 443, to be
// intercepted, or an absolute-form https://, injected on the check's own answer — and any other, a plain http://
// or a CONNECT to 80, is refused inject_requires_tls, 403, never forwarded without the credential or in clear. A
// destination with no route is unchanged.
func TestInjectRequiresTLS(t *testing.T) {
	r := injecting(t)
	body := "Canyon refused this call: use https:// for api.stripe.com. Canyon adds the app's credential only over https."
	refusedAs(t, "http:// to a destination with a route", r.outer(t, claimsProd, "GET", "http", "api.stripe.com"), "inject_requires_tls", 403, body)
	refusedAs(t, "http:// to it on 80", r.outer(t, claimsProd, "POST", "http", "api.stripe.com:80"), "inject_requires_tls", 403, body)
	refusedAs(t, "a CONNECT to it on 80", r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:80"), "inject_requires_tls", 403, body)
	refusedAs(t, "http:// to it naming 443", r.outer(t, claimsProd, "GET", "http", "api.stripe.com:443"), "inject_requires_tls", 403, body)
	refusedAs(t, "no scheme, not a CONNECT, naming 443", r.outer(t, claimsProd, "GET", "", "api.stripe.com:443"), "inject_requires_tls", 403, body)
	injectedAs(t, "https:// to it", r.outer(t, claimsProd, "GET", "https", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-intercept")
	if v := r.outer(t, claimsProd, "GET", "http", "api.adyen.com"); v.code != int32(codes.OK) || len(v.set) != 0 || !slices.Equal(v.remove, []string{"x-canyon-intercept"}) {
		t.Errorf("http:// to a destination claims-portal prod holds no route to: %v, want it let through as before, x-canyon-intercept removed", v)
	}
}

// TestInnerHopWithoutCallerRefused: ext_authz #2's caller is x-canyon-caller alone, which Envoy overwrites from the
// tunnel's filter state: "-", what an inner hop that skipped ext_authz #1 carries, none at all, or a caller the
// policy does not name is unknown_caller — whatever the request's source, which is Envoy's own, or an app's address
// that a caller holds.
func TestInnerHopWithoutCallerRefused(t *testing.T) {
	r := injecting(t)
	body := "Canyon refused this call: Canyon does not know which app is calling."
	for _, source := range []string{"envoy://internal_client_address/", claimsProd} {
		for _, caller := range []string{"-", "", "claims-portal", "claims-portal/", "/prod", "claims-portal/preview", "nobody/prod", "claims-portal/prod/x",
			"claims-portal\x00/prod", " claims-portal/prod"} {
			v := r.innerOf(t, "api.stripe.com", caller, "api.stripe.com", source)
			refusedAs(t, "x-canyon-caller "+caller+" from "+source, v, "unknown_caller", 403, body)
			if v.metadata["app"] != nil {
				t.Errorf("x-canyon-caller %q: an unknown_caller names the app %v", caller, v.metadata["app"])
			}
		}
	}
	injectedAs(t, "claims-portal/prod", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
}

// TestInnerHopRefusedPastTheCallerBound: an inner request's caller expires with the policy as an outer call's does
// (design/EGRESS.md §4.1, decided 2026-11-08): once the policy held is more than 30 s old, ext_authz #2 refuses every
// request inside every tunnel unknown_caller, its value held or not, so no tunnel outlives the bound; the server's
// next answer makes the caller known again.
func TestInnerHopRefusedPastTheCallerBound(t *testing.T) {
	r := injecting(t)
	body := "Canyon refused this call: Canyon does not know which app is calling."
	injectedAs(t, "the policy fresh", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
	r.srv.set(true, nil)
	r.clock.add(callersKept)
	r.poll(context.Background())
	injectedAs(t, "the policy 30 s old", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
	r.clock.add(time.Second)
	refusedAs(t, "the policy 31 s old, the value held", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "unknown_caller", 403, body)
	refusedAs(t, "the outer hop, the policy 31 s old", r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:443"), "unknown_caller", 403, body)
	r.srv.set(false, nil)
	r.poll(context.Background())
	injectedAs(t, "the server back", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
}

// TestInterceptDecidedByThePolicysRoutes: ext_authz #1 intercepts a CONNECT to a destination its caller holds a route
// to, as the policy names it, with x-canyon-intercept and the caller in the metadata for canon.caller — never by the
// chains the listener serves: a destination whose leaf could not be read, its chain dropped, is still sent to the
// internal listener and fails there, never through the plain tunnel without its credential. Every other OK removes
// x-canyon-intercept, and a route of another env intercepts nothing of this one's.
func TestInterceptDecidedByThePolicysRoutes(t *testing.T) {
	r := injecting(t)
	r.srv.leaves = leavesOf(t, newPKI(t), injected(), start) // under another root than the policy's: every chain dropped
	r.intercept(context.Background())
	if held := r.leavesHeld(t); len(held) != 0 {
		t.Fatalf("leaves held for %v, want none: this test needs every chain dropped", held)
	}
	v := r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:443")
	if v.code != int32(codes.OK) || v.appended || len(v.set) != 1 || v.set["x-canyon-intercept"] != "1" || len(v.remove) != 0 ||
		v.metadata["caller"] != "claims-portal/prod" || v.metadata["destination"] != "api.stripe.com" || v.metadata["app"] != "claims-portal" || v.metadata["env"] != "prod" {
		t.Errorf("a CONNECT to a destination with a route, its chain dropped: %v, want it intercepted, x-canyon-intercept 1, the caller claims-portal/prod and the destination api.stripe.com", v)
	}
	if v := r.outer(t, claimsProd, "CONNECT", "", "API.Stripe.COM.:443"); v.metadata["destination"] != "api.stripe.com" {
		t.Errorf("a CONNECT to API.Stripe.COM.:443 approved %v, want the canonical name, api.stripe.com, the chain's", v.metadata["destination"])
	}
	for _, call := range []struct{ source, authority string }{{claimsProd, "api.adyen.com:443"}, {ledgerProd, "hooks.slack.com:443"}, {claimsProd, "api.example.net:443"}} {
		v := r.outer(t, call.source, "CONNECT", "", call.authority)
		if v.code != int32(codes.OK) && call.authority != "api.example.net:443" {
			t.Errorf("a CONNECT from %s to %s, no route of its env: %v, want it let through", call.source, call.authority, v)
		}
		if len(v.set) != 0 || v.metadata["caller"] != nil || (v.code == int32(codes.OK) && !slices.Equal(v.remove, []string{"x-canyon-intercept"})) {
			t.Errorf("a CONNECT from %s to %s, no route of its env: set %v, removed %v, caller %v, want x-canyon-intercept removed and no caller", call.source, call.authority, v.set, v.remove, v.metadata["caller"])
		}
	}
	if n := len(r.srv.valueAsks); n != 0 {
		t.Errorf("%d values asked for, want none: a CONNECT is intercepted, its value read inside", n)
	}
}

// TestHeaderOverwrittenByScheme: the route's header is overwritten — whatever the app sent, its placeholder
// included, and never appended to — by the route's scheme: bearer, Bearer and the value; basic, Basic and the value in
// base64; raw, the value.
func TestHeaderOverwrittenByScheme(t *testing.T) {
	for _, c := range []struct{ header, scheme, want string }{
		{"authorization", "bearer", "Bearer " + secret},
		{"authorization", "basic", "Basic " + base64.StdEncoding.EncodeToString([]byte(secret))},
		{"x-api-key", "raw", secret},
	} {
		r := injecting(t)
		r.srv.policy = routeSet(t, r.srv.policy, "claims-portal", "api.stripe.com", c.header, c.scheme, 77)
		r.srv.values["claims-portal/prod/api.stripe.com"] = valueAnswer(t, 77, secret)
		r.poll(context.Background())
		injectedAs(t, c.scheme+" inner", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), c.header, c.want, "x-canyon-caller")
		v := r.outer(t, claimsProd, "GET", "https", "api.stripe.com")
		injectedAs(t, c.scheme+" absolute-form", v, c.header, c.want, "x-canyon-intercept")
		if v.metadata["destination"] != "api.stripe.com" || v.metadata["caller"] != nil {
			t.Errorf("%s absolute-form: metadata %v, want the destination alone, by which Envoy's local reply tells a destination with a route", c.scheme, v.metadata)
		}
	}
}

// TestValueFetchedOnceAndCached: a route's value is asked for once, however many checks wait on it at once, and held
// under the rev the policy names: a new rev of the route, the route gone, or another epoch drops it, and it is asked
// for again; an answer under a rev other than the policy's is no value, inject_unavailable, and is held for nothing.
func TestValueFetchedOnceAndCached(t *testing.T) {
	r := injecting(t)
	r.srv.valueHang = make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			injectedAs(t, "one of eight at once", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
		}()
	}
	for len(r.srv.valuesAsked()) == 0 {
		time.Sleep(time.Millisecond)
	}
	close(r.srv.valueHang)
	wg.Wait()
	r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com")
	r.outer(t, claimsProd, "GET", "https", "api.stripe.com")
	if asks := r.srv.valuesAsked(); !slices.Equal(asks, []string{"claims-portal/prod/api.stripe.com"}) {
		t.Errorf("asked for %v, want claims-portal prod's value once", asks)
	}

	unavailable := "Canyon could not add the app's credential for api.stripe.com, so it did not send this call. Try again. If this persists, ask an admin."
	r.srv.policy = routeSet(t, r.srv.policy, "claims-portal", "api.stripe.com", "authorization", "bearer", 500) // the route written again
	r.poll(context.Background())
	refusedAs(t, "the server answering the rev before the policy's", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	r.srv.values["claims-portal/prod/api.stripe.com"] = valueAnswer(t, 500, "sk_live_"+strings.Repeat("n", 24))
	injectedAs(t, "under the new rev", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer sk_live_"+strings.Repeat("n", 24), "x-canyon-caller")
	if n := len(r.srv.valuesAsked()); n != 3 {
		t.Errorf("asked %d times, want once more for the answer under the old rev, held for nothing, and once for the new", n)
	}

	r.srv.set(false, policyEdit(t, r.srv.policy, func(p map[string]any) { p["epoch"], p["seq"] = strings.Repeat("e", 32), 1 }))
	r.poll(context.Background())
	if r.held.epoch != strings.Repeat("e", 32) {
		t.Fatalf("the replica holds the epoch %s, want the server's new one", r.held.epoch)
	}
	r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com")
	if n := len(r.srv.valuesAsked()); n != 4 {
		t.Errorf("asked %d times, want once more under the server's new epoch", n)
	}

	r.srv.set(false, policyEdit(t, r.srv.policy, func(p map[string]any) { // the route deleted
		for _, e := range p["envs"].([]any) {
			if env := e.(map[string]any); env["app"] == "claims-portal" {
				env["routes"] = slices.DeleteFunc(env["routes"].([]any), func(r any) bool { return r.(map[string]any)["destination"] == "api.stripe.com" })
			}
		}
	}))
	r.poll(context.Background())
	refusedAs(t, "a route gone", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	if n := len(r.srv.valuesAsked()); n != 4 {
		t.Errorf("asked %d times, want none for a route gone", n)
	}
}

// valuesAsked is every value asked for, in order.
func (s *server) valuesAsked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.valueAsks)
}

// TestValueNotReadInTimeIsInjectUnavailable: a value the server does not answer within the check's time — 4.5 s from
// when it began, a refetch of its caller included, inside Envoy's 5 s — is inject_unavailable, 502, never the 503 of
// an unanswered check; so is a server that answers anything but a value.
func TestValueNotReadInTimeIsInjectUnavailable(t *testing.T) {
	if valueBudget >= 5*time.Second || valueWait > valueBudget {
		t.Fatalf("a value has %s of a check and each ask %s, want both inside Envoy's 5 s", valueBudget, valueWait)
	}
	unavailable := "Canyon could not add the app's credential for api.stripe.com, so it did not send this call. Try again. If this persists, ask an admin."
	r := injecting(t)
	r.budget = 200 * time.Millisecond
	r.srv.valueHang = make(chan struct{})
	defer close(r.srv.valueHang)
	began := time.Now()
	refusedAs(t, "a value not answered", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	refusedAs(t, "a value not answered, absolute-form", r.outer(t, claimsProd, "GET", "https", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	if took := time.Since(began); took > 2*r.budget+time.Second {
		t.Errorf("two checks took %s, want each within the check's time, %s", took, r.budget)
	}
	r2 := injecting(t)
	for _, answer := range [][]byte{nil, []byte("<html>"), valueAnswer(t, 2, ""), []byte(`{"rev":2}`), valueAnswer(t, 2, "a\r\nx-injected: 1"),
		valueAnswer(t, 2, strings.Repeat("v", 8193)), []byte(`{"rev":0,"value":"v"}`)} {
		r2.srv.values["claims-portal/prod/api.stripe.com"] = answer
		refusedAs(t, "the server answering "+string(answer), r2.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "inject_unavailable", 502, unavailable)
	}
}

// TestValueNeverLogged: a route's value is held in memory alone and is in no log line, no error and no refusal,
// whatever the server answers.
func TestValueNeverLogged(t *testing.T) {
	r := injecting(t)
	r.srv.values["claims-portal/prod/api.openai.com"] = []byte(`{"rev":1,"value":"` + secret + "\n" + `"}`) // not JSON: a raw line break
	r.srv.values["claims-portal/prod/hooks.slack.com"] = valueAnswer(t, 999, secret)                        // under another rev
	r.srv.values["claims-portal/prod/sqs.eu-west-1.amazonaws.com"] = []byte(`{"rev":"` + secret + `"}`)
	var answers []string
	for _, d := range inject["claims-portal"] {
		v := r.inner(t, d, "claims-portal/prod", d)
		answers = append(answers, v.body, v.reason)
		answers = append(answers, r.outer(t, claimsProd, "GET", "https", d).body)
	}
	if logged := r.logs.String(); strings.Contains(logged, secret) || !strings.Contains(logged, "reason inject_unavailable") {
		t.Errorf("logged %q, want the refusals named and no value", logged)
	}
	if all := strings.Join(answers, "\n"); strings.Contains(all, secret) {
		t.Errorf("a refusal carries the value: %q", all)
	}
	// the errors themselves name no byte of it: ee/outboundkeys/intercept's TestAValueErrorNamesNoByteOfIt
}

// TestRouteValueAskedAsThePolicyIs: Client.RouteValue asks GET /egress/route-value?app=&env=&destination= at the
// policy's listener, authenticated as GET /egress/policy is — on Kubernetes the projected token over TLS pinned to
// the server's certificate — and answers a 200's body alone: any other answer is an error naming its status, never
// its body.
func TestRouteValueAskedAsThePolicyIs(t *testing.T) {
	var asked *http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r
		if r.URL.Query().Get("destination") == "api.stripe.com" {
			_, _ = w.Write(valueAnswer(t, 2, secret))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal","value":"` + secret + `"}`))
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("projected-token-of-egress-control\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert := pemOf(srv.Certificate())
	c, err := NewClient(srv.URL+"/egress/policy", token, []byte(cert))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.RouteValue(context.Background(), "claims-portal", "prod", "api.stripe.com")
	if err != nil || asked.URL.Path != "/egress/route-value" || asked.URL.RawQuery != "app=claims-portal&destination=api.stripe.com&env=prod" ||
		asked.Header.Get("Authorization") != "Bearer projected-token-of-egress-control" || asked.Method != http.MethodGet {
		t.Fatalf("RouteValue asked %s %v with %q: %v", asked.Method, asked.URL, asked.Header.Get("Authorization"), err)
	}
	if string(raw) != string(valueAnswer(t, 2, secret)) {
		t.Errorf("RouteValue answered %d bytes, want the 200's body, the value under rev 2", len(raw))
	}
	if _, err := c.RouteValue(context.Background(), "claims-portal", "prod", "hooks.slack.com"); err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 answered %v, want an error naming the status alone", err)
	}
	unpinned, err := NewClient(srv.URL+"/egress/policy", token, []byte(pemOf(newPKI(t).inter)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unpinned.RouteValue(context.Background(), "claims-portal", "prod", "api.stripe.com"); err == nil {
		t.Error("a server whose certificate is not the pinned one was asked for a value")
	}
}

// TestInnerHopBoundToTheOuterApproval: a request inside a tunnel goes on only to the destination ext_authz #1
// approved that tunnel for, x-canyon-destination, which Envoy overwrites from the tunnel's filter state: a tunnel
// approved for api.openai.com whose inner SNI and :authority are api.stripe.com — routed and allowed for the same env,
// its own chain matching — is misdirected, 421, naming what the tunnel goes to, and so is a tunnel with no approval.
func TestInnerHopBoundToTheOuterApproval(t *testing.T) {
	r := injecting(t)
	if v := r.outer(t, claimsProd, "CONNECT", "", "api.openai.com:443"); v.metadata["destination"] != "api.openai.com" {
		t.Fatalf("the CONNECT to api.openai.com approved %v, want api.openai.com", v.metadata["destination"])
	}
	refusedAs(t, "a tunnel approved for api.openai.com carrying api.stripe.com",
		r.innerApproved(t, "api.stripe.com", "api.openai.com", "claims-portal/prod", "api.stripe.com", "envoy://internal_client_address/"), "misdirected", 421,
		"Canyon refused this call: this connection goes to api.openai.com, but the request is for another host.")
	for _, approved := range []string{"-", "", "API.STRIPE.COM", "api.stripe.com.", "api.stripe.com:443"} {
		refusedAs(t, "a tunnel approved for "+approved, r.innerApproved(t, "api.stripe.com", approved, "claims-portal/prod", "api.stripe.com", "envoy://internal_client_address/"),
			"misdirected", 421, "Canyon refused this call: this connection goes to api.stripe.com, but the request is for another host.")
	}
	if n := len(r.srv.valuesAsked()); n != 0 {
		t.Errorf("%d values asked for, want none: a request the tunnel was not approved for asks for none", n)
	}
	injectedAs(t, "a tunnel approved for api.stripe.com", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
}

// TestInnerHopReChecksTheDecision: ext_authz #2 makes ext_authz #1's decision again, the same function, of the
// chain's destination: a route outlives the allowlist entry that let its destination through, so a tunnel opened
// before the entry was removed, or a mode made stricter since, is refused for the outer check's reason at its next
// request, its value asked for no more; a platform destination and observe stay allowed.
func TestInnerHopReChecksTheDecision(t *testing.T) {
	r := injecting(t)
	injectedAs(t, "allowed", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
	edit := func(change func(env map[string]any)) {
		r.srv.set(false, policyEdit(t, r.srv.policy, func(p map[string]any) {
			for _, e := range p["envs"].([]any) {
				if env := e.(map[string]any); env["app"] == "claims-portal" {
					change(env)
				}
			}
		}))
		r.poll(context.Background())
	}
	edit(func(env map[string]any) { // the allowlist entry removed, the route kept
		env["allowed"] = slices.DeleteFunc(slices.Clone(env["allowed"].([]any)), func(a any) bool { return a == "api.stripe.com" })
	})
	notAllowed := "Canyon refused this call: api.stripe.com is not on claims-portal prod's outbound allowlist. A person on the app's team can allow it in Canyon: https://canyon.meridian.example/#/apps/claims-portal/outbound"
	refusedAs(t, "its allowlist entry removed", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "not_allowed", 403, notAllowed)
	refusedAs(t, "its allowlist entry removed, the outer hop", r.outer(t, claimsProd, "CONNECT", "", "api.stripe.com:443"), "not_allowed", 403, notAllowed)
	edit(func(env map[string]any) { env["mode"] = "observe" })
	injectedAs(t, "observe", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+secret, "x-canyon-caller")
	edit(func(env map[string]any) { env["mode"], env["allowed"] = "allowlist", []any{} })
	refusedAs(t, "allowlist, empty", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "not_allowed", 403, notAllowed)
	if n := len(r.srv.valuesAsked()); n != 1 {
		t.Errorf("%d values asked for, want the first alone: a refused request asks for none", n)
	}
}

// valuesAt answers each route value of the stripe chain as given: app/env to its answer.
func (r replica) valuesAt(t *testing.T, answers map[string][]byte) {
	t.Helper()
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	for k, v := range answers {
		r.srv.values[k] = v
	}
}

// waitAsks waits up to a second for n values to have been asked for, answering whether they were.
func (r replica) waitAsks(n int) bool {
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if len(r.srv.valuesAsked()) >= n {
			return true
		}
	}
	return false
}

// TestValueFlightPerRev: an ask under one rev serves no check under another: a check under rev 500, made while an ask
// under rev 2 is still out, asks for itself and gets rev 500's value, never rev 2's.
func TestValueFlightPerRev(t *testing.T) {
	r := injecting(t)
	r.valuesAt(t, map[string][]byte{"claims-portal/prod/api.stripe.com": valueAnswer(t, 2, "sk_live_"+strings.Repeat("o", 24))})
	r.srv.valueHang = make(chan struct{})
	first := make(chan verdict, 1)
	go func() { first <- r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com") }()
	if !r.waitAsks(1) {
		t.Fatal("rev 2's value never asked for")
	}
	r.srv.set(false, routeSet(t, r.srv.policy, "claims-portal", "api.stripe.com", "authorization", "bearer", 500))
	r.poll(context.Background())
	r.valuesAt(t, map[string][]byte{"claims-portal/prod/api.stripe.com": valueAnswer(t, 500, "sk_live_"+strings.Repeat("n", 24))})
	second := make(chan verdict, 1)
	go func() { second <- r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com") }()
	r.waitAsks(2)
	close(r.srv.valueHang)
	<-first
	injectedAs(t, "a check under rev 500 while rev 2's ask was out", <-second, "authorization", "Bearer sk_live_"+strings.Repeat("n", 24), "x-canyon-caller")
}

// TestValueAskBounded: each ask for a value has valueWait, 2 s, of its own, inside the check's 4.5 s, whatever the
// check has left.
func TestValueAskBounded(t *testing.T) {
	if valueWait != 2*time.Second || valueBudget != 4500*time.Millisecond {
		t.Fatalf("an ask has %s and a check %s, want 2 s and 4.5 s (contracts/routes.md, Interception, The value)", valueWait, valueBudget)
	}
	r := injecting(t)
	r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com")
	r.srv.mu.Lock()
	times := slices.Clone(r.srv.valueTimes)
	r.srv.mu.Unlock()
	if len(times) != 1 || times[0] <= valueWait-time.Second || times[0] > valueWait {
		t.Errorf("the ask had %v, want valueWait, %s, of its own", times, valueWait)
	}
}

// TestValuesKeptPerAppEnv: a value is asked for, held and shared by app env and destination: two envs routing
// api.stripe.com under the same rev, asked for at once, each get their own; an env holding no route to the chain's
// destination gets inject_unavailable with no ask, and another env routing it gets its own value.
func TestValuesKeptPerAppEnv(t *testing.T) {
	r := injecting(t)
	r.srv.set(false, policyEdit(t, r.srv.policy, func(p map[string]any) {
		routesOf(p, "ledger", func(_ map[string]any, _ int, rt map[string]any) {
			if rt["destination"] == "api.stripe.com" {
				rt["rev"] = 2 // claims-portal's own rev for it
			}
		})
	}))
	r.poll(context.Background())
	claims, ledger := "sk_live_"+strings.Repeat("c", 24), "sk_live_"+strings.Repeat("l", 24)
	r.valuesAt(t, map[string][]byte{"claims-portal/prod/api.stripe.com": valueAnswer(t, 2, claims), "ledger/prod/api.stripe.com": valueAnswer(t, 2, ledger)})
	r.srv.valueHang = make(chan struct{})
	got := make(chan verdict, 1)
	go func() { got <- r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com") }()
	if !r.waitAsks(1) {
		t.Fatal("claims-portal's value never asked for")
	}
	other := make(chan verdict, 1)
	go func() { other <- r.inner(t, "api.stripe.com", "ledger/prod", "api.stripe.com") }()
	r.waitAsks(2)
	close(r.srv.valueHang)
	injectedAs(t, "claims-portal prod, asked for at once with ledger's", <-got, "authorization", "Bearer "+claims, "x-canyon-caller")
	injectedAs(t, "ledger prod, asked for at once with claims-portal's", <-other, "authorization", "Bearer "+ledger, "x-canyon-caller")
	injectedAs(t, "claims-portal prod, held", r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com"), "authorization", "Bearer "+claims, "x-canyon-caller")
	injectedAs(t, "ledger prod, held", r.inner(t, "api.stripe.com", "ledger/prod", "api.stripe.com"), "authorization", "Bearer "+ledger, "x-canyon-caller")
	asked := len(r.srv.valuesAsked())
	refusedAs(t, "intake-bot prod, no route to the chain's destination", r.inner(t, "api.stripe.com", "intake-bot/prod", "api.stripe.com"), "inject_unavailable", 502,
		"Canyon could not add the app's credential for api.stripe.com, so it did not send this call. Try again. If this persists, ask an admin.")
	if n := len(r.srv.valuesAsked()); n != asked {
		t.Errorf("intake-bot prod's request asked for %d values, want none", n-asked)
	}
}

// TestValueOfAnEarlierEpochNotHeld: a value an ask began for under one epoch is answered to its checks but held for
// none once the server's epoch has changed meanwhile, so the next check asks again under the new one.
func TestValueOfAnEarlierEpochNotHeld(t *testing.T) {
	r := injecting(t)
	r.srv.valueHang = make(chan struct{})
	first := make(chan verdict, 1)
	go func() { first <- r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com") }()
	if !r.waitAsks(1) {
		t.Fatal("the value never asked for")
	}
	r.srv.set(false, policyEdit(t, r.srv.policy, func(p map[string]any) { p["epoch"], p["seq"] = strings.Repeat("e", 32), 1 }))
	r.poll(context.Background())
	close(r.srv.valueHang)
	injectedAs(t, "the check that asked", <-first, "authorization", "Bearer "+secret, "x-canyon-caller")
	r.srv.valueHang = nil
	r.inner(t, "api.stripe.com", "claims-portal/prod", "api.stripe.com")
	if n := len(r.srv.valuesAsked()); n != 2 {
		t.Errorf("%d asks, want the value asked for again under the new epoch", n)
	}
}

// TestForgedHeadersWithoutFilterStateRefused: with neither filter-state object — an inner hop that skipped ext_authz
// #1, or a set_filter_state that missed one — Envoy's header_mutation overwrites the app's own x-canyon-caller and
// x-canyon-destination with empty values (keep_empty_value), and ext_authz #2 refuses the pair, whatever the app
// forged: an empty caller unknown_caller, a caller with an empty approval misdirected.
func TestForgedHeadersWithoutFilterStateRefused(t *testing.T) {
	r := injecting(t)
	inner := func(headers map[string]string) verdict {
		t.Helper()
		resp, err := r.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			ContextExtensions: map[string]string{"destination": "api.stripe.com"},
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Method: "POST", Scheme: "https", Host: "api.stripe.com",
				Headers: headers}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return verdictOf(t, resp)
	}
	refusedAs(t, "both objects missing: both headers empty", inner(map[string]string{"x-canyon-caller": "", "x-canyon-destination": ""}),
		"unknown_caller", 403, "Canyon refused this call: Canyon does not know which app is calling.")
	refusedAs(t, "the approval's object missing", inner(map[string]string{"x-canyon-caller": "ledger/prod", "x-canyon-destination": ""}),
		"misdirected", 421, "Canyon refused this call: this connection goes to api.stripe.com, but the request is for another host.")
	if n := len(r.srv.valuesAsked()); n != 0 {
		t.Errorf("%d values asked for, want none", n)
	}
}
