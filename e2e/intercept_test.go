//go:build stack

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Interception on the stack (design/EGRESS.md §4.2; contracts/routes.md, Interception): Envoy subscribes to the
// listener egress-control serves by LDS and its leaves by SDS; a CONNECT the check marks for interception goes to the
// one internal listener, where the app's TLS ends with a leaf under Canyon's root and ext_authz #2 decides each
// request inside. The destinations are names no other test calls, so routing them changes no other test's call; none
// resolves, so a request the inner check lets on fails before it is sent, upstream_failed, and nothing leaves — but
// verifyName, a TLS stand-in whose certificate Envoy must refuse.

const (
	injectA = "inject-a.canyon-floor.test"    // routed
	injectB = "inject-b.canyon-floor.test"    // routed
	injectC = "inject-c.canyon-floor.test"    // routed, called by InnerRequestsAreTheirCallersCalls alone, so its row holds that test's calls
	noChain = "inject-none.canyon-floor.test" // allowed, no route: a name the internal listener has no chain for
	envoyC  = "canyon-egress-envoy"
	// verifyName is routed and resolves to 198.51.100.9 (e2e/compose.e2e.yml), where UpstreamVerificationNeverSkipped
	// stands up TLS under a certificate no root in Envoy's image signs, answering "reached" to any request.
	verifyName = "verify.canyon-floor.test"
	socat      = "alpine/socat:1.8.0.3" // e2e/compose.e2e.yml's
	// mismatchName is routed and resolves to 198.51.100.9 too, where UpstreamNameAndSNI's stand-in answers it under a
	// certificate the test CA signs for another name.
	mismatchName = "mismatch.canyon-floor.test"
	caddy        = "docker.io/library/caddy:2@sha256:0c994536bddb66445885237f1a5dcc1916bccea922661c76b4e9fc24061f9b52" // deploy/docker/team.yml's
)

// TestForgedInterceptHeaderRejected: a request that already carries x-canyon-intercept, the header that sends a
// CONNECT to the internal listener, is refused by the RBAC filter before any check, a CONNECT and a plain request
// alike, its 403 Envoy's own, with no Canyon-Egress header (contracts/routes.md, Interception).
func TestForgedInterceptHeaderRejected(t *testing.T) {
	app := routedApp(t)
	controlUp(t)
	for _, call := range [][]string{
		{"--proxy-header", "x-canyon-intercept: 1", "https://" + publicName + "/"},
		{"-H", "x-canyon-intercept: 1", "http://" + publicName + "/etc/dnsd.conf"},
	} {
		r := throughEnvoyFrom(t, app, "", 3128, call...)
		if code := r.connect; strings.HasPrefix(call[len(call)-1], "https") && code != "403" {
			t.Errorf("a CONNECT carrying x-canyon-intercept was answered %s, want 403 by RBAC: %s", code, r)
		}
		if strings.HasPrefix(call[len(call)-1], "http:") && r.status != "403" {
			t.Errorf("a request carrying x-canyon-intercept was answered %s, want 403 by RBAC: %s", r.status, r)
		}
		if r.egress != "" {
			t.Errorf("the forged header's 403 carries Canyon-Egress %q, want Envoy's own: the RBAC filter answered it, not the check", r.egress)
		}
	}
}

// TestInterception routes destinations of the routed app, whose deploy row trusts the signing root as its own /run
// recorded it (#2143), and holds Envoy to each refusal the design names, once Envoy holds the chains.
func TestInterception(t *testing.T) {
	app, certs := intercepting(t)

	t.Run("ReachesTheInnerCheck", func(t *testing.T) {
		r := throughEnvoyFrom(t, app, certs, 3128, "https://"+injectA+"/v1/charges")
		if r.connect != "200" || r.status != "502" || r.egress != "upstream_failed" ||
			r.body != "Canyon could not connect to "+injectA+" or verify its certificate, so it did not send this call." {
			t.Errorf("a call to %s: %s, want its TLS ended by Canyon's leaf, the inner check's OK, and the unresolvable name upstream_failed", injectA, r)
		}
	})
	t.Run("ApprovalBindsTheTunnel", func(t *testing.T) { // a tunnel approved for A carrying a request for B, B's own chain matching
		r := throughEnvoyFrom(t, app, certs, 3128, "--connect-to", injectB+":443:"+injectA+":443", "https://"+injectB+"/v1/charges")
		if r.connect != "200" || r.status != "421" || r.egress != "misdirected" ||
			r.body != "Canyon refused this call: this connection goes to "+injectA+", but the request is for another host." {
			t.Errorf("a tunnel approved for %s carrying %s: %s, want 421 misdirected naming %s", injectA, injectB, r, injectA)
		}
	})
	t.Run("LoopbackAuthorityRefused", func(t *testing.T) { // egress-control's own port, where the leaves' keys are served
		for _, proto := range []string{"--http1.1", "--http2"} {
			r := throughEnvoyFrom(t, app, certs, 3128, proto, "-H", "Host: 127.0.0.1:7783", "https://"+injectA+"/")
			if r.connect != "200" || r.status != "421" || r.egress != "misdirected" {
				t.Errorf("%s with :authority 127.0.0.1:7783: %s, want the listener's 421 misdirected", proto, r)
			}
		}
	})
	t.Run("NoChainClosesTheConnection", func(t *testing.T) { // what a dropped chain is to Envoy: no chain for the name
		r := throughEnvoyFrom(t, app, certs, 3128, "--connect-to", noChain+":443:"+injectA+":443", "https://"+noChain+"/")
		if r.connect != "200" || r.exit != 35 || r.status != "000" {
			t.Errorf("TLS for %s inside a tunnel the check intercepted: %s, want the handshake failed (curl exit 35), no chain for the name", noChain, r)
		}
	})
	scratch := scratchEnvoy(t)
	t.Run("ForgedHeadersWithoutFilterStateRefused", func(t *testing.T) {
		forged := []string{"-H", "x-canyon-caller: " + routed.project + "/preview", "-H", "x-canyon-destination: " + injectA}
		for _, c := range []struct {
			port          int
			missing, want string
		}{{scratch.none, "both objects", "unknown_caller"}, {scratch.callerOnly, "canyon.destination", "misdirected"}, {scratch.destinationOnly, "canyon.caller", "unknown_caller"}} {
			for _, proto := range []string{"--http1.1", "--http2"} {
				r := throughEnvoyFrom(t, app, certs, c.port, append([]string{proto}, append(forged, "https://"+injectA+"/v1/charges")...)...)
				if r.connect != "200" || r.egress != c.want {
					t.Errorf("%s, %s missing from the tunnel's filter state, the app forging both headers: %s, want refused %s", proto, c.missing, r, c.want)
				}
			}
		}
	})
	t.Run("InnerRequestsAreTheirCallersCalls", func(t *testing.T) {
		// Each inner request is a call of its caller's row, as canyon.caller names it, and the tunnel one more (design/EGRESS.md
		// §2.4; contracts/routes.md, Interception, A record): one tunnel carrying three requests is four calls of the app's
		// preview row of the name. A request for B through a tunnel approved for A, ApprovalBindsTheTunnel's, is B's
		// row alone, refused, intercepted: no tunnel to B ever opened.
		calls := func(d string) string {
			return ledger(t, "SELECT coalesce(sum(calls), 0) FROM egress_destinations WHERE app = '"+routed.project+"' AND env = 'preview' AND destination = '"+d+"' AND port = 443")
		}
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
			r := throughEnvoyFrom(t, app, certs, 3128, "https://"+injectC+"/")
			if r.connect == "200" && r.egress == "upstream_failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Envoy holds no chain for %s 90 s after its route: %s", injectC, r)
			}
		}
		settled := func() int { // two reads a flush apart agree: every entry before this is written
			last := ""
			for range 10 {
				time.Sleep(12 * time.Second)
				n := calls(injectC)
				if n == last {
					v, _ := strconv.Atoi(n)
					return v
				}
				last = n
			}
			t.Fatalf("%s's calls never settled: %s", injectC, last)
			return 0
		}
		before := settled()
		since := ledger(t, "SELECT now()")
		if exit, out := exitOf("run", "--rm", "--network", "container:"+app, "-v", certs+":/certs:ro", curl, "-s", "-m", "20", "-x", "http://canyon-egress:3128",
			"--cacert", "/certs/root.pem", "https://"+injectC+"/1", "https://"+injectC+"/2", "https://"+injectC+"/3"); exit != 0 {
			t.Fatalf("three requests through one tunnel to %s: curl exit %d\n%s", injectC, exit, out)
		}
		// The four calls within a bound, then nothing more (#35): a late entry is waited for, never read as missing, and
		// one that never comes says what the receiver saw, so a lost entry is told from a slow one.
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(3 * time.Second) {
			n, _ := strconv.Atoi(calls(injectC))
			if n-before >= 4 {
				break
			}
			if time.Now().After(deadline) {
				s := egressSetting(t)
				t.Fatalf("one tunnel carrying three requests to %s added %d calls to %s's preview row in 90 s, want 4: the tunnel and each request; "+
					"the receiver holds %d streams, its last entry at %v, and the gaps since are %q", injectC, n-before, routed.project, s.Streams, s.LastEntryAt,
					ledger(t, "SELECT string_agg(reason || ':' || coalesce(calls::text, '-'), ' ' ORDER BY from_at) FROM egress_gaps WHERE to_at >= '"+since+"'"))
			}
		}
		if after := settled(); after-before != 4 {
			t.Errorf("one tunnel carrying three requests to %s added %d calls to %s's preview row, want 4: the tunnel and each request", injectC, after-before, routed.project)
		}
		if got := ledger(t, "SELECT last_outcome || ' ' || last_protocol FROM egress_destinations WHERE app = '"+routed.project+"' AND env = 'preview' AND destination = '"+injectB+"' AND port = 443"); got != "refused intercepted" {
			t.Errorf("%s's row, asked for only inside a tunnel approved for %s: %q, want refused intercepted", injectB, injectA, got)
		}
	})
	t.Run("UpstreamVerificationNeverSkipped", func(t *testing.T) {
		// Envoy originates TLS to a route's destination and never skips checking its chain (design/EGRESS.md ⓔ;
		// contracts/routes.md, Interception, Upstream): an upstream whose certificate no root in Envoy's image signs is
		// 502 upstream_failed, naming the destination, and the request — the credential in it — is never sent. Both
		// ways a credential leaves: an intercepted request, and an absolute-form https:// one the check injects itself.
		// The chain alone: a self-signed certificate fails before its name or SNI matter, which this does not test.
		tlsStandIn(t)
		var r proxied
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) { // until Envoy holds its chain
			if r = throughEnvoyFrom(t, app, certs, 3128, "https://"+verifyName+"/v1/charges"); r.connect == "200" && r.status != "000" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Envoy holds no chain for %s 90 s after its route: %s", verifyName, r)
			}
		}
		want := "Canyon could not connect to " + verifyName + " or verify its certificate, so it did not send this call."
		if r.status != "502" || r.egress != "upstream_failed" || r.body != want {
			t.Errorf("an intercepted call to %s, its certificate signed by no root Envoy trusts: %s, want 502 upstream_failed and nothing sent", verifyName, r)
		}
		r = throughEnvoyFrom(t, app, "", 3128, "--request-target", "https://"+verifyName+"/v1/charges", "http://"+verifyName+"/")
		if r.status != "502" || r.egress != "upstream_failed" || r.body != want {
			t.Errorf("an absolute-form https:// call to %s: %s, want 502 upstream_failed and nothing sent", verifyName, r)
		}
	})
	t.Run("UpstreamNameAndSNI", func(t *testing.T) {
		// Envoy checks the upstream certificate's name against the route's destination and sends that name as SNI
		// (auto_san_validation, auto_sni; contracts/routes.md, Interception, Upstream). A scratch Envoy on the gateway's
		// own bootstrap, but for its upstream clusters trusting a CA this test makes, which no image and no shipped file
		// ever holds, reaches a stand-in under that CA: a certificate for verifyName gets the request through, with the
		// route's header and verifyName as its SNI; one for another name, behind mismatchName, is 502 upstream_failed,
		// nothing sent. Both ways a credential leaves: an intercepted request, and an absolute-form https:// one.
		ca, caKey, caPEM, _ := certFor(t, "Canyon e2e test CA", true, nil, nil)
		_, _, verifyPEM, verifyKey := certFor(t, verifyName, false, ca, caKey)
		_, _, otherPEM, otherKey := certFor(t, "other.canyon-floor.test", false, ca, caKey)
		publicStandIn(t, "names", filesIn(t, map[string][]byte{"verify.pem": verifyPEM, "verify.key": verifyKey, "other.pem": otherPEM, "other.key": otherKey,
			"Caddyfile": []byte(namesCaddyfile)}), caddy, "caddy", "run", "--config", "/s/Caddyfile", "--adapter", "caddyfile")
		port := trustingEnvoy(t, caPEM)
		want := "sni=" + verifyName + " auth=Bearer " + routeValue()
		var r proxied
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) { // until the scratch holds the chain
			if r = throughEnvoyFrom(t, app, certs, port, "https://"+verifyName+"/v1/charges"); r.connect == "200" && r.status != "000" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the trusting scratch Envoy holds no chain for %s in 90 s: %s", verifyName, r)
			}
		}
		if r.status != "200" || r.body != want {
			t.Errorf("an intercepted call to %s, its certificate the test CA's for that name: %s, want 200 %q: through, with the route's header and the name as SNI", verifyName, r, want)
		}
		if r = throughEnvoyFrom(t, app, "", port, "--request-target", "https://"+verifyName+"/v1/charges", "http://"+verifyName+"/"); r.status != "200" || r.body != want {
			t.Errorf("an absolute-form https:// call to %s: %s, want 200 %q", verifyName, r, want)
		}
		mismatch := "Canyon could not connect to " + mismatchName + " or verify its certificate, so it did not send this call."
		if r = throughEnvoyFrom(t, app, certs, port, "https://"+mismatchName+"/v1/charges"); r.status != "502" || r.egress != "upstream_failed" || r.body != mismatch {
			t.Errorf("an intercepted call to %s, its certificate the test CA's for another name: %s, want 502 upstream_failed and nothing sent", mismatchName, r)
		}
		if r = throughEnvoyFrom(t, app, "", port, "--request-target", "https://"+mismatchName+"/v1/charges", "http://"+mismatchName+"/"); r.status != "502" || r.egress != "upstream_failed" || r.body != mismatch {
			t.Errorf("an absolute-form https:// call to %s: %s, want 502 upstream_failed and nothing sent", mismatchName, r)
		}
	})
	t.Run("RemoveOnlyResponseReroutes", func(t *testing.T) {
		// Envoy 1.39.3 clears the route cache on a remove-only OK although ext_authz.proto says only an added or altered
		// header does (design/EGRESS.md §4.2): the probe's route for a request carrying x-canyon-intercept answers 418,
		// any other 200, and the check's OK of a call to a destination with no route removes the header.
		r := throughEnvoyFrom(t, app, "", scratch.probe, "-H", "x-canyon-intercept: 1", "http://"+noChain+"/")
		if r.status != "200" || r.body != "rerouted" {
			t.Errorf("a remove-only OK: %s, want the request routed again past the header the check removed", r)
		}
	})
}

// intercepting sets the routes up once for t, a top-level test, and undoes them as it ends: via canyon, whose host
// the routes are live on; the names allowed; routes to four. The routed app's deploy row trusts the signing root as
// its /run recorded it, which it checks. It waits until Envoy holds A's chain, and answers the app's container and a
// directory holding the root.
func intercepting(t *testing.T) (app, certs string) {
	t.Helper()
	app = routedApp(t)
	controlUp(t)
	project := routed.project
	setVia(t, "canyon")
	t.Cleanup(func() { setVia(t, "off") })
	setCredentialRoutes(t, true)
	t.Cleanup(func() { setCredentialRoutes(t, false) })
	base := "/api/v1/projects/" + project + "/egress/preview/"
	for _, d := range []string{injectA, injectB, injectC, verifyName, mismatchName, noChain} {
		if code, body := api(t, "PUT", base+"destinations/"+d, nil); code/100 != 2 {
			t.Fatalf("allowing %s: %d %s", d, code, body)
		}
		t.Cleanup(func() { api(t, "DELETE", base+"destinations/"+d, nil) })
	}
	for _, d := range []string{injectA, injectB, injectC, verifyName, mismatchName} {
		if code, body := api(t, "PUT", base+"routes/"+d, map[string]any{"header": "authorization", "scheme": "bearer", "value": routeValue()}); code/100 != 2 {
			t.Fatalf("routing %s: %d %s", d, code, body)
		}
		t.Cleanup(func() { api(t, "DELETE", base+"routes/"+d, nil) })
	}
	root := strings.TrimSpace(ledger(t, "SELECT fingerprint FROM egress_ca_roots ORDER BY created_at LIMIT 1"))
	if len(root) != 64 {
		t.Fatalf("the stack holds no interception root: %q", root)
	}
	if got := ledger(t, "SELECT coalesce(array_to_string(egress_roots, ','), '') FROM deploys WHERE project_id = '"+project+"' AND env = 'preview'"); !strings.Contains(got, root) {
		t.Fatalf("%s preview's deploy row trusts %q; want the interception root %s, as its own /run carried it (#2143)", project, got, root)
	}
	certs = t.TempDir()
	if err := os.Chmod(certs, 0o755); err != nil { //nolint:gosec // read by curl's own user in a container
		t.Fatal(err)
	}
	pem := ledger(t, "SELECT cert FROM egress_ca_roots WHERE fingerprint = '"+root+"'")
	if err := os.WriteFile(filepath.Join(certs, "root.pem"), []byte(strings.TrimSpace(pem)+"\n"), 0o644); err != nil { //nolint:gosec // a certificate, read by curl in a container
		t.Fatal(err)
	}
	var last proxied
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if last = throughEnvoyFrom(t, app, certs, 3128, "https://"+injectA+"/"); last.connect == "200" && last.egress == "upstream_failed" {
			return app, certs
		}
	}
	t.Fatalf("Envoy holds no chain for %s 90 s after its route and trust: %s; egress-control's log:\n%s", injectA, last, logTail(t, checker))
	return "", ""
}

// proxied is one curl through Envoy: what it answered the CONNECT and the request, its Canyon-Egress header and body,
// and curl's exit.
type proxied struct {
	connect, status, egress, body string
	exit                          int
}

func (r proxied) String() string {
	return fmt.Sprintf("CONNECT %s, status %s, Canyon-Egress %q, body %q, curl exit %d", r.connect, r.status, r.egress, r.body, r.exit)
}

const codesMark = "\n--codes-- "

// throughEnvoyFrom is curl with args from app's own namespace through the proxy on port of canyon-egress, trusting
// the root in certs when certs is not "".
func throughEnvoyFrom(t *testing.T, app, certs string, port int, args ...string) proxied {
	t.Helper()
	run := []string{"run", "--rm", "--network", "container:" + app}
	if certs != "" {
		run = append(run, "-v", certs+":/certs:ro")
	}
	run = append(run, curl, "-s", "-m", "10", "-D", "-", "-x", fmt.Sprintf("http://canyon-egress:%d", port), "-w", codesMark+"%{http_connect} %{http_code}")
	if certs != "" {
		run = append(run, "--cacert", "/certs/root.pem")
	}
	exit, out := exitOf(append(run, args...)...)
	head, codes, found := strings.Cut(out, codesMark)
	r := proxied{exit: exit}
	if !found {
		t.Fatalf("curl %v through :%d answered no codes (exit %d)\n%s", args, port, exit, out)
	}
	r.connect, r.status, _ = strings.Cut(strings.TrimSpace(codes), " ")
	blocks := strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n\n")
	for i, b := range blocks {
		for _, line := range strings.Split(b, "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "canyon-egress") {
				r.egress = strings.TrimSpace(v)
				r.body = strings.TrimSpace(strings.Join(blocks[i+1:], "\n\n"))
			}
		}
	}
	if r.egress == "" && len(blocks) > 1 {
		r.body = strings.TrimSpace(blocks[len(blocks)-1])
	}
	return r
}

// logTail is a container's last lines.
func logTail(t *testing.T, container string) string {
	t.Helper()
	_, out := exitOf("logs", "--tail", "40", container)
	return out
}

// scratch is a second Envoy in canyon-egress's namespace, on the gateway's own bootstrap but for its listeners, asking
// the real egress-control and served its interception listener: three on that bootstrap's :3128 with set_filter_state
// cut down — no object, the caller alone, the destination alone — and a probe of the route cache.
type scratch struct{ none, callerOnly, destinationOnly, probe int }

// scratchEnvoy starts it until t ends.
func scratchEnvoy(t *testing.T) scratch {
	t.Helper()
	_, filled := exitOf("exec", envoyC, "cat", "/var/run/envoy/envoy.yaml")
	var b map[string]any
	if err := yaml.Unmarshal([]byte(filled), &b); err != nil {
		t.Fatalf("the gateway's bootstrap does not parse: %v", err)
	}
	s := scratch{none: 3129, callerOnly: 3130, destinationOnly: 3131, probe: 3132}
	b["admin"] = map[string]any{"address": map[string]any{"pipe": map[string]any{"path": "/tmp/scratch-admin.sock"}}}
	statics := b["static_resources"].(map[string]any)
	listener := statics["listeners"].([]any)[0].(map[string]any)
	hcmOf := func(l map[string]any) map[string]any {
		return l["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	}
	listenerAt := func(port int, edit func(m map[string]any)) map[string]any {
		var l map[string]any
		raw, _ := yaml.Marshal(listener)
		if err := yaml.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		l["name"] = fmt.Sprintf("scratch-%d", port)
		l["address"] = map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": port}}
		m := hcmOf(l)
		delete(m, "access_log") // none of the scratch's calls reaches the records
		delete(m, "access_log_options")
		edit(m)
		return l
	}
	keep := func(objects ...string) func(map[string]any) {
		return func(m map[string]any) {
			for _, f := range m["http_filters"].([]any) {
				f := f.(map[string]any)
				if f["name"] != "envoy.filters.http.set_filter_state" {
					continue
				}
				cfg := f["typed_config"].(map[string]any)
				var kept []any
				for _, o := range cfg["on_request_headers"].([]any) {
					if slices.Contains(objects, o.(map[string]any)["object_key"].(string)) {
						kept = append(kept, o)
					}
				}
				cfg["on_request_headers"] = kept
			}
			var filters []any
			for _, f := range m["http_filters"].([]any) {
				if f.(map[string]any)["name"] != "envoy.filters.http.set_filter_state" || len(f.(map[string]any)["typed_config"].(map[string]any)["on_request_headers"].([]any)) > 0 {
					filters = append(filters, f)
				}
			}
			m["http_filters"] = filters
		}
	}
	probe := func(m map[string]any) {
		var filters []any
		for _, f := range m["http_filters"].([]any) {
			if name := f.(map[string]any)["name"]; name == "envoy.filters.http.ext_authz" || name == "envoy.filters.http.router" {
				filters = append(filters, f)
			}
		}
		m["http_filters"] = filters
		m["route_config"] = map[string]any{"name": "probe", "virtual_hosts": []any{map[string]any{"name": "probe", "domains": []any{"*"}, "routes": []any{
			map[string]any{"match": map[string]any{"prefix": "/", "headers": []any{map[string]any{"name": "x-canyon-intercept", "present_match": true}}},
				"direct_response": map[string]any{"status": 418, "body": map[string]any{"inline_string": "stale"}}},
			map[string]any{"match": map[string]any{"prefix": "/"}, "direct_response": map[string]any{"status": 200, "body": map[string]any{"inline_string": "rerouted"}}},
		}}}}
	}
	statics["listeners"] = []any{listenerAt(s.none, keep()), listenerAt(s.callerOnly, keep("canyon.caller")),
		listenerAt(s.destinationOnly, keep("canyon.destination")), listenerAt(s.probe, probe)}
	raw, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // read by Envoy's own user in a container
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "envoy.yaml"), raw, 0o644); err != nil { //nolint:gosec // the stack's own synthetic token, read by Envoy in a container
		t.Fatal(err)
	}
	_, image := exitOf("inspect", "-f", "{{.Config.Image}}", envoyC)
	name := "canyon-scratch-envoy-" + tag
	if code, out := exitOf("run", "-d", "--name", name, "--network", "container:"+holder, "-v", dir+":/etc/scratch:ro", strings.TrimSpace(image),
		"envoy", "-c", "/etc/scratch/envoy.yaml", "--concurrency", "1", "--base-id", "7"); code != 0 {
		t.Fatalf("the scratch Envoy did not start: %s", out)
	}
	t.Cleanup(func() { exitOf("rm", "-f", name) })
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if r := throughEnvoyFrom(t, routedApp(t), "", s.probe, "http://"+noChain+"/"); r.status == "200" {
			return s
		}
	}
	t.Fatalf("the scratch Envoy answers nothing on :%d within 60 s:\n%s", s.probe, logTail(t, name))
	return s
}

// tlsStandIn stands up 198.51.100.9 — verifyName, by e2e/compose.e2e.yml — on a public network the holder joins,
// serving TLS on 443 under a certificate for verifyName that it signs itself, so no root in Envoy's image does, and
// answering any request that reaches it "reached", until t ends.
func tlsStandIn(t *testing.T) {
	t.Helper()
	_, _, cert, key := certFor(t, verifyName, false, nil, nil) // a server's, self-signed: its chain fails, never its name
	dir := filesIn(t, map[string][]byte{"cert.pem": cert, "key.pem": key, "reply": []byte("HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nreached")})
	publicStandIn(t, "verify", dir, socat, "-d", "OPENSSL-LISTEN:443,reuseaddr,fork,cert=/s/cert.pem,key=/s/key.pem,verify=0", "SYSTEM:cat /s/reply")
}

// routeValue is the value of every route intercepting sets: a credential's shape, never one, built here so no file
// holds it.
func routeValue() string { return "sk_live_" + strings.Repeat("e", 24) }

// certFor is a certificate for name — a CA's when ca, else a server's naming it — signed by parent with parentKey, by
// itself when parent is nil, and it and its key as PEM, valid an hour either side of now.
func certFor(t *testing.T, name string, ca bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	if ca {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else {
		tmpl.DNSNames, tmpl.ExtKeyUsage, tmpl.KeyUsage = []string{name}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, x509.KeyUsageDigitalSignature
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// filesIn writes files into a directory of their own that a container's user may read, and answers it.
func filesIn(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // read by a container's own user
		t.Fatal(err)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { //nolint:gosec // a throwaway stand-in's, read in a container
			t.Fatal(err)
		}
	}
	return dir
}

// publicStandIn runs image with args as 198.51.100.9 — verifyName and mismatchName, by e2e/compose.e2e.yml — on a
// public network the holder joins, dir mounted at /s, until t ends.
func publicStandIn(t *testing.T, name, dir, image string, args ...string) {
	t.Helper()
	n := floorPrefix + tag + "-" + name
	docker(t, "network", "create", "--subnet", "198.51.100.0/24", n)
	// one teardown, in order: the holder off the network, the stand-in gone, then the network, which the stand-in's
	// endpoint can outlive a moment
	t.Cleanup(func() {
		exitOf("network", "disconnect", n, holder)
		if code, out := exitOf("rm", "-f", n); code != 0 {
			t.Errorf("the stand-in %s was not removed: %s", n, out)
		}
		for range 10 {
			if code, _ := exitOf("network", "rm", n); code == 0 {
				return
			}
			time.Sleep(time.Second)
		}
		_, out := exitOf("network", "inspect", "-f", "{{json .Containers}}", n)
		t.Errorf("the network %s outlived its stand-in 10 s, still holding %s", n, out)
	})
	if code, out := exitOf(append([]string{"run", "-d", "--name", n, "--network", n, "--ip", "198.51.100.9", "-v", dir + ":/s:ro", image}, args...)...); code != 0 {
		t.Fatalf("the stand-in %s did not start: %s", n, out)
	}
	docker(t, "network", "connect", n, holder)
}

// namesCaddyfile is UpstreamNameAndSNI's stand-in: verifyName under its own certificate, answering the SNI it was
// asked for and the Authorization it was sent; mismatchName under a certificate for another name, which is also
// the one a handshake asking for no name it holds gets.
const namesCaddyfile = `{
	admin off
	auto_https off
	default_sni other.canyon-floor.test
	skip_install_trust
}
https://verify.canyon-floor.test {
	tls /s/verify.pem /s/verify.key
	respond "sni={http.request.tls.server_name} auth={header.Authorization}" 200
}
https://mismatch.canyon-floor.test {
	tls /s/other.pem /s/other.key
	respond "reached" 200
}
`

// trustingEnvoy starts a second Envoy in canyon-egress's namespace on the gateway's own bootstrap, its :3128
// listener alone, on another port, and its two upstream clusters, tls and intercepted, trusting ca alone in place
// of the image's roots; egress-control serves it the interception listener as it serves the gateway. It answers
// the port, until t ends. Those clusters are the shipped ones, read at test time: the gateway runs
// deploy/egress/envoy.yaml as envoy.sh filled it, and trustingEnvoy fails unless its tls and intercepted equal that
// file's, the resolvers envoy.sh fills aside, before it swaps trusted_ca and nothing else.
func trustingEnvoy(t *testing.T, ca []byte) int {
	t.Helper()
	const port = 3134
	_, filled := exitOf("exec", envoyC, "cat", "/var/run/envoy/envoy.yaml")
	var b map[string]any
	if err := yaml.Unmarshal([]byte(filled), &b); err != nil {
		t.Fatalf("the gateway's bootstrap does not parse: %v", err)
	}
	b["admin"] = map[string]any{"address": map[string]any{"pipe": map[string]any{"path": "/tmp/trusting-admin.sock"}}}
	statics := b["static_resources"].(map[string]any)
	listener := statics["listeners"].([]any)[0].(map[string]any)
	listener["name"] = "trusting"
	listener["address"] = map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": port}}
	m := listener["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	delete(m, "access_log") // none of its calls reaches the records but the interception listener's own
	delete(m, "access_log_options")
	statics["listeners"] = []any{listener}
	shipped, err := os.ReadFile(filepath.Join("..", "deploy", "egress", "envoy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal(shipped, &file); err != nil {
		t.Fatalf("deploy/egress/envoy.yaml does not parse: %v", err)
	}
	want := map[any]string{}
	for _, c := range file["static_resources"].(map[string]any)["clusters"].([]any) {
		want[c.(map[string]any)["name"]] = withoutResolvers(t, c)
	}
	trusted := 0
	for _, c := range statics["clusters"].([]any) {
		c := c.(map[string]any)
		if c["name"] != "tls" && c["name"] != "intercepted" {
			continue
		}
		if got := withoutResolvers(t, c); got != want[c["name"]] {
			t.Fatalf("the gateway's cluster %s is not deploy/egress/envoy.yaml's:\n%s\nwant:\n%s", c["name"], got, want[c["name"]])
		}
		v := c["transport_socket"].(map[string]any)["typed_config"].(map[string]any)["common_tls_context"].(map[string]any)["validation_context"].(map[string]any)
		v["trusted_ca"] = map[string]any{"filename": "/etc/scratch/ca.pem"}
		trusted++
	}
	if trusted != 2 {
		t.Fatalf("the gateway's bootstrap has %d of the upstream clusters tls and intercepted, want both", trusted)
	}
	raw, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	dir := filesIn(t, map[string][]byte{"envoy.yaml": raw, "ca.pem": ca})
	_, image := exitOf("inspect", "-f", "{{.Config.Image}}", envoyC)
	name := "canyon-trusting-envoy-" + tag
	if code, out := exitOf("run", "-d", "--name", name, "--network", "container:"+holder, "-v", dir+":/etc/scratch:ro", strings.TrimSpace(image),
		"envoy", "-c", "/etc/scratch/envoy.yaml", "--concurrency", "1", "--base-id", "8"); code != 0 {
		t.Fatalf("the trusting scratch Envoy did not start: %s", out)
	}
	t.Cleanup(func() { exitOf("rm", "-f", name) })
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if r := throughEnvoyFrom(t, routedApp(t), "", port, "http://"+noChain+"/"); r.status != "000" {
			return port
		}
	}
	t.Fatalf("the trusting scratch Envoy answers nothing on :%d within 60 s:\n%s", port, logTail(t, name))
	return port
}

// withoutResolvers is a cluster as YAML, every resolvers key dropped: the one value envoy.sh fills in a cluster.
func withoutResolvers(t *testing.T, cluster any) string {
	t.Helper()
	var drop func(v any) any
	drop = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, x := range v {
				if k != "resolvers" {
					out[k] = drop(x)
				}
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, x := range v {
				out[i] = drop(x)
			}
			return out
		}
		return v
	}
	raw, err := yaml.Marshal(drop(cluster))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
