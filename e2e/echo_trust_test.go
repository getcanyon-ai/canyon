//go:build stack

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// e2e/echo's two names (e2e/compose.e2e.yml): one under the test CA the gateway's bundle holds beside its image's
// roots, the other under a CA nothing trusts.
const (
	echoName      = "echo.canyon-floor.test"
	echoUntrusted = "echo-untrusted.canyon-floor.test"
)

// TestEnvoyTrustsTheEchosTestCAAndNoOther: the stack's gateway verifies e2e/echo under the test CA its bundle adds,
// and an intercepted call to it arrives with the route's value as its Authorization and the echo's name as its SNI;
// the same echo under a CA the bundle does not hold is 502 upstream_failed, nothing sent, so the mount widens trust
// by that one CA and no more (#2144).
func TestEnvoyTrustsTheEchosTestCAAndNoOther(t *testing.T) {
	app := routedApp(t)
	controlUp(t)
	setVia(t, "canyon")
	t.Cleanup(func() { setVia(t, "off") })
	certs := trustedRoutes(t, routed.project, echoName, echoUntrusted)
	var r proxied
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) { // until Envoy holds the chain
		if r = throughEnvoyFrom(t, app, certs, 3128, "https://"+echoName+"/echo"); r.connect == "200" && r.status != "000" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Envoy holds no chain for %s 90 s after its route and trust: %s", echoName, r)
		}
	}
	if r.status != "200" || !strings.HasPrefix(r.body, "method=GET path=/echo sni="+echoName+" ") || !strings.HasSuffix(r.body, " auth=Bearer "+routeValue()) {
		t.Errorf("an intercepted call to %s: %s, want 200 with the route's value and the echo's name as SNI", echoName, r)
	}
	refused := "Canyon could not connect to " + echoUntrusted + " or verify its certificate, so it did not send this call."
	if r = throughEnvoyFrom(t, app, certs, 3128, "https://"+echoUntrusted+"/echo"); r.status != "502" || r.egress != "upstream_failed" || r.body != refused {
		t.Errorf("an intercepted call to %s, under a CA no bundle holds: %s, want 502 upstream_failed and nothing sent", echoUntrusted, r)
	}
}

// trustedRoutes turns credential routes on (#2143), allows and routes each name for project's preview with
// routeValue(), all undone as t ends, checks its deploy row trusts the signing root as its own /run recorded it, and
// answers a directory holding that root as root.pem for curl.
func trustedRoutes(t *testing.T, project string, names ...string) string {
	t.Helper()
	setCredentialRoutes(t, true)
	t.Cleanup(func() { setCredentialRoutes(t, false) })
	base := "/api/v1/projects/" + project + "/egress/preview/"
	for _, d := range names {
		if code, body := api(t, http.MethodPut, base+"destinations/"+d, nil); code/100 != 2 {
			t.Fatalf("allowing %s: %d %s", d, code, body)
		}
		t.Cleanup(func() { api(t, http.MethodDelete, base+"destinations/"+d, nil) })
		if code, body := api(t, http.MethodPut, base+"routes/"+d, map[string]any{"header": "authorization", "scheme": "bearer", "value": routeValue()}); code/100 != 2 {
			t.Fatalf("routing %s: %d %s", d, code, body)
		}
		t.Cleanup(func() { api(t, http.MethodDelete, base+"routes/"+d, nil) })
	}
	root := strings.TrimSpace(ledger(t, "SELECT fingerprint FROM egress_ca_roots ORDER BY created_at LIMIT 1"))
	if len(root) != 64 {
		t.Fatalf("the stack holds no interception root: %q", root)
	}
	if got := ledger(t, "SELECT coalesce(array_to_string(egress_roots, ','), '') FROM deploys WHERE project_id = '"+project+"' AND env = 'preview'"); !strings.Contains(got, root) {
		t.Fatalf("%s preview's deploy row trusts %q; want the interception root %s, as its own /run carried it (#2143)", project, got, root)
	}
	return filesIn(t, map[string][]byte{"root.pem": []byte(strings.TrimSpace(ledger(t, "SELECT cert FROM egress_ca_roots WHERE fingerprint = '"+root+"'")) + "\n")})
}
