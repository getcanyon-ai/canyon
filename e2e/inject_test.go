//go:build stack

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestARouteCredentialReachesItsUpstream: #2143's turn-on, end to end (design/EGRESS.md §4.2, §4.4; contracts/routes.md,
// Interception, Trust). The routed app's own /run carried the host's root — its deploy row trusts it and its container
// holds it at /etc/canyon-egress, named in SSL_CERT_DIR — with no test writing either. Then a route's credential reaches
// e2e/echo (echoName, under the test CA the gateway trusts in the stack), which answers what reached it, for its own
// app env alone:
//   - a route on prod alone: preview's call passes through untouched, never decrypted, and carries no credential;
//   - a route on preview: preview's call, trusting what its container holds, reaches the upstream with the route's value,
//     the placeholder the app sent overwritten, prod's value never;
//   - a container on the app's network at no app's address calls the same destination: unknown_caller, nothing sent.
func TestARouteCredentialReachesItsUpstream(t *testing.T) {
	app := routedApp(t)
	controlUp(t)
	project := routed.project
	setVia(t, "canyon")
	t.Cleanup(func() { setVia(t, "off") })
	setCredentialRoutes(t, true)
	t.Cleanup(func() { setCredentialRoutes(t, false) })

	root := strings.TrimSpace(ledger(t, "SELECT fingerprint FROM egress_ca_roots ORDER BY created_at DESC LIMIT 1"))
	if got := ledger(t, "SELECT coalesce(array_to_string(egress_roots, ','), '') FROM deploys WHERE project_id = '"+project+"' AND env = 'preview'"); !strings.Contains(got, root) {
		t.Fatalf("%s preview's deploy row trusts %q; want the signing root %s, as its own /run carried it", project, got, root)
	}
	if env := docker(t, "inspect", "-f", "{{json .Config.Env}}", app); !strings.Contains(env, `"SSL_CERT_DIR=/etc/ssl/certs:/etc/canyon-egress"`) ||
		!strings.Contains(env, `"NODE_EXTRA_CA_CERTS=/etc/canyon-egress/bundle.pem"`) || !strings.Contains(env, `"REQUESTS_CA_BUNDLE=/etc/canyon-egress/system-and-canyon.pem"`) {
		t.Fatalf("the app's environment names no trust of /etc/canyon-egress: %s", env)
	}
	held := t.TempDir()
	if err := os.Chmod(held, 0o755); err != nil { //nolint:gosec // read by curl's own user in a container
		t.Fatal(err)
	}
	docker(t, "cp", app+":/etc/canyon-egress/bundle.pem", filepath.Join(held, "root.pem"))                                                                                                                               // the app's own trust, as its /run laid it
	if bundle, err := os.ReadFile(filepath.Join(held, "root.pem")); err != nil || strings.TrimSpace(string(bundle)) != strings.TrimSpace(ledger(t, "SELECT cert FROM egress_ca_roots WHERE fingerprint = '"+root+"'")) { //nolint:gosec // the test's own temp dir
		t.Fatalf("the app's /etc/canyon-egress/bundle.pem: %v; want the signing root as stored", err)
	}

	fixtures, err := filepath.Abs(filepath.Join(".work", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}

	base := "/api/v1/projects/" + project + "/egress/"
	for _, env := range []string{"preview", "prod"} {
		if code, body := api(t, "PUT", base+env+"/destinations/"+echoName, nil); code/100 != 2 {
			t.Fatalf("allowing %s for %s: %d %s", echoName, env, code, body)
		}
		t.Cleanup(func() { api(t, "DELETE", base+env+"/destinations/"+echoName, nil) })
	}
	prodValue, previewValue := "sk_live_"+strings.Repeat("p", 24), "sk_live_"+strings.Repeat("q", 24) // credentials' shape, never one
	route := func(env, value string) {
		t.Helper()
		if code, body := api(t, "PUT", base+env+"/routes/"+echoName, map[string]any{"header": "authorization", "scheme": "bearer", "value": value}); code/100 != 2 {
			t.Fatalf("routing %s for %s: %d %s", echoName, env, code, body)
		}
		t.Cleanup(func() { api(t, "DELETE", base+env+"/routes/"+echoName, nil) })
	}
	placeholder := []string{"-H", "Authorization: Bearer canyon-adds-the-real-key", "https://" + echoName + "/v1/charges"}

	// A route on prod alone: preview's call is never intercepted, its TLS the upstream's own end to end.
	route("prod", prodValue)
	upstreamCA := filepath.Join(fixtures, "echo-ca.pem")
	ca := t.TempDir()
	if err := os.Chmod(ca, 0o755); err != nil { //nolint:gosec // read by curl's own user in a container
		t.Fatal(err)
	}
	raw, err := os.ReadFile(upstreamCA) //nolint:gosec // the stack's own fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ca, "root.pem"), raw, 0o644); err != nil { //nolint:gosec // a certificate, read by curl in a container
		t.Fatal(err)
	}
	converged(t) // prod's route is in the policy every replica holds: had it been preview's too, its call would be intercepted now
	if r := throughEnvoyFrom(t, app, ca, 3128, placeholder...); r.connect != "200" || r.status != "200" ||
		!strings.HasSuffix(r.body, " auth=Bearer canyon-adds-the-real-key") || strings.Contains(r.body, prodValue) {
		t.Fatalf("preview's call, prod alone routed, trusting the echo's own CA alone: %s; want it through untouched, never decrypted, the app's own header and never prod's credential", r)
	}

	// A route on preview: its call, trusting what its container holds, is intercepted and the upstream gets its value.
	route("preview", previewValue)
	var r proxied
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
		if r = throughEnvoyFrom(t, app, held, 3128, placeholder...); r.connect == "200" && r.status == "200" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview's call, trusting its container's /etc/canyon-egress, 90 s after its route: %s; want it intercepted and through", r)
		}
	}
	if !strings.HasSuffix(r.body, " auth=Bearer "+previewValue) || strings.Contains(r.body, prodValue) {
		t.Errorf("the echo saw from preview, routed: %s; want preview's credential alone, the placeholder overwritten, never prod's", r.body)
	}

	// A container on the app's network at no app's fixed address: no caller, so nothing is sent (the CONNECT refused).
	if exit, out := exitOf("run", "--rm", "--network", "canyon-app-"+project, curl, "-s", "-m", "10", "-o", "/dev/null", "-w", "%{http_connect}", "-x", "http://canyon-egress:3128", "-k",
		"https://"+echoName+"/v1/charges"); strings.TrimSpace(out) != "403" {
		t.Errorf("a container at no app's address through Envoy: curl exit %d, CONNECT %q; want 403 unknown_caller", exit, out)
	}
}

// converged waits until every egress-control replica seen in the last 15 s holds the policy's current version, as a
// /run's convergence does (contracts/routes.md, GET /egress/policy).
func converged(t *testing.T) {
	t.Helper()
	const behind = "SELECT count(*) FROM egress_replicas r, egress_policy p WHERE r.seen_at > now() - interval '15 seconds' AND (r.have_epoch IS DISTINCT FROM p.epoch OR r.have_seq IS NULL OR r.have_seq < p.seq)"
	for deadline := time.Now().Add(60 * time.Second); strings.TrimSpace(ledger(t, behind)) != "0"; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("an egress-control replica still holds an older policy a minute on")
		}
	}
}
