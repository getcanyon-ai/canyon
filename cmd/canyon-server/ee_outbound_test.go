//go:build !noee

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
)

// THE FAIL-CLOSED RULE (#2879), outbound keys: a credential Canyon adds under a licence is still in the egress policy
// once it has lapsed, and rotating it, removing it and turning credentials off stay free (#2881's review: a
// paywall never stands between an admin and a security fix), while a new credential, moving a secret into one and
// turning credentials back on are 409 licence_required.
func TestALapsedLicenceKeepsCredentials(t *testing.T) {
	h := outboundHost(t)
	egressWait = time.Second // rawPolicy reads the policy as a replica that never asks again: a change no longer waits it out (#136)
	t.Cleanup(func() { egressWait = egressDefault })
	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("a credential under the licence: %d %s", code, lastBody)
	}
	h.trusting(t, "preview")
	_, policy := policyAt(t, h.pool)
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/STRIPE_KEY", `{"value":"`+routeValue+`","env":"prod"}`); code != 204 {
		t.Fatalf("the secret to move: %d %s", code, lastBody)
	}

	licenceAt(t, h.base, testExpiry.Add(licence.Grace+time.Hour)) // lapsed: an hour past grace

	if _, p := rawPolicy(t, policy); len(routesOf(p, "preview")) != 1 {
		t.Errorf("lapsed: the policy names %v, want the credential still added", routesOf(p, "preview"))
	}
	for _, c := range []struct{ what, body string }{
		{"rotating the credential's value", routeBody("Authorization", "bearer", routeValue+"-rotated")},
		{"replacing it under another header", routeBody("X-Api-Key", "raw", routeValue+"-rotated")},
	} {
		if code, _, _ := do(t, h.carl, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), c.body); code != 204 {
			t.Errorf("lapsed, %s: %d %s, want 204: a leaked key can always be rotated", c.what, code, lastBody)
		}
	}
	if _, p := rawPolicy(t, policy); len(routesOf(p, "preview")) != 1 {
		t.Errorf("lapsed, rotated: the policy names %v, want the credential still added", routesOf(p, "preview"))
	}
	for _, c := range []struct {
		what               string
		who                *http.Client
		method, path, body string
	}{
		{"a credential for prod", h.carl, http.MethodPut, routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)},
		{"moving a secret into a credential", h.olive, http.MethodPost, routePath("prod", "api.stripe.com") + "/from-secret", `{"secret":"STRIPE_KEY","header":"Authorization","scheme":"bearer"}`},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 409 || lastBody != outboundKeysRequired {
			t.Errorf("lapsed, %s: %d %s, want 409 %s", c.what, code, lastBody, outboundKeysRequired)
		}
	}
	if code, _, _ := do(t, h.carl, http.MethodDelete, h.base+routePath("preview", "api.stripe.com"), ""); code != 204 {
		t.Errorf("lapsed, removing the credential: %d %s, want 204", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":false}`); code != 200 {
		t.Errorf("lapsed, turning credentials off: %d %s, want 200", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":true}`); code != 409 || lastBody != outboundKeysRequired {
		t.Errorf("lapsed, turning credentials back on: %d %s, want 409 %s", code, lastBody, outboundKeysRequired)
	}
}

// A licence in force that names no outbound_keys adds no credential: 409 licence_required.
func TestACredentialNeedsALicenceNamingOutboundKeys(t *testing.T) {
	withLicenceEnv(t, signedLicence(testExpiry, "group_sync"))
	h := outboundHost(t)
	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 409 || lastBody != outboundKeysRequired {
		t.Errorf("a credential under a licence naming no outbound_keys: %d %s, want 409 %s", code, lastBody, outboundKeysRequired)
	}
}

// Moved from egress_taken_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// #2789: an outbound rule change answers once every live egress-control replica holds the version it made, as a
// deploy's converge waits before its /run, so a call the change refuses is refused from the answer on. A replica is
// live while it asked within 30 s, as long as one cut off still serves its policy; one not seen for longer is not
// waited for. Past the wait the change stands and the answer names the first replica behind in Canyon-Egress-Pending;
// one that answers in time carries none.
func TestAnOutboundChangeAnswersOnceEveryReplicaHoldsIt(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, `INSERT INTO egress_replicas (replica, have_epoch, have_seq, seen_at) SELECT 'canyon-egress-a', epoch, seq, now() FROM egress_policy`,
		`INSERT INTO egress_replicas (replica, have_epoch, have_seq, seen_at) VALUES ('canyon-egress-gone', NULL, NULL, now() - interval '1 hour')`)
	type answer struct {
		code    int
		pending string
		err     error
	}
	send := func(c *http.Client, method, path, body string) <-chan answer {
		done := make(chan answer, 1)
		go func() {
			req, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.Do(req)
			if err != nil {
				done <- answer{err: err}
				return
			}
			_ = resp.Body.Close()
			done <- answer{code: resp.StatusCode, pending: resp.Header.Get("Canyon-Egress-Pending")}
		}()
		return done
	}
	caughtUp := "UPDATE egress_replicas SET have_epoch = p.epoch, have_seq = p.seq, seen_at = now() FROM egress_policy p WHERE replica = 'canyon-egress-a'"
	// waits answers that the write did not answer while canyon-egress-a held the version before it, and answered
	// want, with no header, once it took it
	waits := func(what string, done <-chan answer, want int) {
		t.Helper()
		select {
		case a := <-done:
			t.Fatalf("%s answered %d %+v while canyon-egress-a held the version before it", what, a.code, a)
		case <-time.After(1500 * time.Millisecond):
		}
		h.exec(t, caughtUp)
		select {
		case a := <-done:
			if a.err != nil || a.code != want || a.pending != "" {
				t.Errorf("%s once canyon-egress-a took it: %d, Canyon-Egress-Pending %q, %v; want %d and none", what, a.code, a.pending, a.err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never answered once canyon-egress-a took it", what)
		}
	}

	// the owner narrows preview to allowlist; the admin sets a limit; the owner sets a route on production, which
	// follows the org's observe, and removes it
	waits("the mode change", send(h.olive, http.MethodPut, outboundApp+"/preview/mode", `{"mode":"allowlist"}`), http.StatusNoContent)
	waits("PUT /egress", send(h.dana, http.MethodPut, "/api/v1/egress", `{"caps":{"new_names":60}}`), http.StatusOK)
	waits("a route set", send(h.olive, http.MethodPut, outboundApp+"/prod/routes/api.stripe.com", `{"header":"authorization","scheme":"bearer","value":"synthetic-route-value"}`), http.StatusNoContent)
	waits("the route's removal", send(h.olive, http.MethodDelete, outboundApp+"/prod/routes/api.stripe.com", ""), http.StatusNoContent)

	// a replica last seen 20 s ago may still serve the policy before: waited for, and named past the wait
	egressWait = time.Second
	t.Cleanup(func() { egressWait = egressDefault })
	h.exec(t, "UPDATE egress_replicas SET have_seq = 0, seen_at = now() - interval '20 seconds' WHERE replica = 'canyon-egress-a'")
	a := <-send(h.olive, http.MethodPut, outboundApp+"/preview/destinations/api.stripe.com", "")
	if a.err != nil || a.code != http.StatusNoContent || a.pending != "canyon-egress-a" {
		t.Errorf("an allow canyon-egress-a, seen 20 s ago, never takes: %d, Canyon-Egress-Pending %q, %v; want 204 naming canyon-egress-a", a.code, a.pending, a.err)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_allowed WHERE project_id = 'claims-portal' AND env = 'preview' AND entry = 'api.stripe.com'"); n != 1 {
		t.Errorf("%d entries for api.stripe.com: the change must stand past the wait", n)
	}

	// a wait that cannot be read never says every replica holds the change
	h.exec(t, "ALTER TABLE egress_replicas RENAME TO egress_replicas_away")
	t.Cleanup(func() { _, _ = h.pool.Exec(h.ctx, "ALTER TABLE egress_replicas_away RENAME TO egress_replicas") })
	a = <-send(h.olive, http.MethodDelete, outboundApp+"/preview/destinations/api.stripe.com", "")
	if a.err != nil || a.code != http.StatusNoContent || a.pending != "unread" {
		t.Errorf("a removal whose wait could not be read: %d, Canyon-Egress-Pending %q, %v; want 204 and unread", a.code, a.pending, a.err)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, Interception, Trust: a route and a secret carrying the app's own trust never both land. A
// route, then such a secret, is 409 route_trust in the secrets PUT, byte for byte; a runtime one alone, of the env
// holding a route: preview's is taken. And each write takes the app env's advisory lock, as the receiver's flush
// does, before its check: a secret written under it and not yet committed holds a route's write until it commits,
// which then sees it (trust_secret); a route written under it holds a secret's write the same way (route_trust).
func TestARouteAndATrustSecretNeverBothLand(t *testing.T) {
	h := outboundHost(t)
	stripe := h.base + routePath("prod", "api.stripe.com")
	if code, _, _ := do(t, h.olive, http.MethodPut, stripe, routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	secret := func(name, body string) int {
		t.Helper()
		code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/"+name, body)
		return code
	}
	if code := secret("SSL_CERT_DIR", `{"value":"/etc/ssl/own","env":"prod"}`); code != 409 ||
		lastBody != `{"error":"route_trust","message":"remove the app's credentials in production first: Canyon needs to set SSL_CERT_DIR itself for them"}` {
		t.Fatalf("a trust variable on an env holding a route: %d %s", code, lastBody)
	}
	if code := secret("REQUESTS_CA_BUNDLE", `{"value":"/etc/ssl/own"}`); code != 204 {
		t.Fatalf("on preview, which holds none: %d %s", code, lastBody)
	}
	if code := secret("SSL_CERT_FILE", `{"value":"/etc/ssl/own","env":"prod"}`); code != 204 {
		t.Fatalf("a variable that is none of the three: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM egress_routes", "DELETE FROM secrets")

	code := h.whileLocked(t, envLock("prod"), "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('claims-portal', 'prod', 'SSL_CERT_DIR', 'runtime', 1, '', '', 'person', 'sub-olive')",
		func() int {
			c, _, _ := do(t, h.olive, http.MethodPut, stripe, routeBody("Authorization", "bearer", routeValue))
			return c
		})
	if code != 409 || !strings.Contains(lastBody, `"error":"trust_secret"`) || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 0 {
		t.Fatalf("a route written while a secret's write held the lock: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM secrets")
	code = h.whileLocked(t, envLock("prod"), "INSERT INTO egress_routes (project_id, env, destination, header, scheme, from_secret, rev, key_ref, sealed_key, sealed_value) VALUES ('claims-portal', 'prod', 'api.stripe.com', 'authorization', 'bearer', false, nextval('egress_route_rev'), 1, '', '')",
		func() int { return secret("NODE_EXTRA_CA_CERTS", `{"value":"/etc/ssl/own","env":"prod"}`) })
	if code != 409 || !strings.Contains(lastBody, `"error":"route_trust"`) || count(t, h.ctx, h.pool, "SELECT count(*) FROM secrets") != 0 {
		t.Fatalf("a secret written while a route's write held the lock: %d %s", code, lastBody)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/PERMISSIONS.md, Outbound: setting a route is a widening, a person's from a browser at every level that may
// widen — a contributor's, an owner's and an admin's CLI sessions are 403 dashboard_required, an agent 403
// person_required — and below contributor every route is 403 team_required, each one denied row; removing one works
// from any session. A route a secret placed is an owner's: moving a secret in, and replacing or removing what it
// placed, is 403 owner_required to a contributor and taken from its owner.
func TestARouteIsAContributorsAndASecretsRouteAnOwners(t *testing.T) {
	h := outboundHost(t)
	stripe := h.base + routePath("preview", "api.stripe.com")
	body := routeBody("Authorization", "bearer", routeValue)
	for name, c := range map[string]*http.Client{"a contributor's CLI": h.carlCLI, "an owner's CLI": h.oliveCLI, "an admin's CLI": h.danaCLI} {
		if code, _, _ := do(t, c, http.MethodPut, stripe, body); code != 403 || lastBody != dashboardRequired {
			t.Errorf("%s: %d %s, want %s", name, code, lastBody, dashboardRequired)
		}
	}
	for name, c := range map[string]*http.Client{"a read-only member": h.vic, "a member with no access": h.nina} {
		for _, call := range [][2]string{{http.MethodPut, stripe}, {http.MethodDelete, stripe}, {http.MethodPost, stripe + "/from-secret"}} {
			if code, _, _ := do(t, c, call[0], call[1], body); code != 403 || lastBody != teamRequiredTo(c == h.vic) {
				t.Errorf("%s's %s %s: %d %s, want team_required", name, call[0], call[1], code, lastBody)
			}
		}
	}
	h.asAgent(t, "sub-carl", true)
	if code, _, _ := do(t, h.carl, http.MethodPut, stripe, body); code != 403 || lastBody != personRequired {
		t.Errorf("a contributor's agent: %d %s, want %s", code, lastBody, personRequired)
	}
	h.asAgent(t, "sub-carl", false)
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes"); n != 0 {
		t.Fatalf("%d routes set by refused callers", n)
	}
	for code, want := range map[string]int{"dashboard_required": 3, "person_required": 1, "team_required": 6} {
		if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = '"+code+"'"); n != want {
			t.Errorf("%d denied rows of %s, want %d", n, code, want)
		}
	}
	if code, _, _ := do(t, h.carl, http.MethodPut, stripe, body); code != 204 {
		t.Fatalf("a contributor's route from the dashboard: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.carlCLI, http.MethodDelete, stripe, ""); code != 204 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 0 ||
		count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.route.remove' AND detail = '{"env": "preview", "destination": "api.stripe.com"}'`) != 1 {
		t.Fatalf("a contributor's removal from the CLI: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.carlCLI, http.MethodDelete, stripe, ""); code != 204 || count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.route.remove'") != 1 {
		t.Fatalf("removing what is not there: %d, and no second row", code)
	}

	h.trusting(t, "preview")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"`+routeValue+`"}`); code != 204 {
		t.Fatalf("the secret: %d %s", code, lastBody)
	}
	move := `{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer"}`
	if code, _, _ := do(t, h.carl, http.MethodPost, stripe+"/from-secret", move); code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive") {
		t.Fatalf("a contributor moving a secret in: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodPost, stripe+"/from-secret", move); code != 204 {
		t.Fatalf("an owner moving a secret in: %d %s", code, lastBody)
	}
	for _, call := range [][3]string{{http.MethodPut, stripe, body}, {http.MethodDelete, stripe, ""}} {
		if code, _, _ := do(t, h.carl, call[0], call[1], call[2]); code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive") {
			t.Fatalf("a contributor's %s of a secret's route: %d %s", call[0], code, lastBody)
		}
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes WHERE from_secret"); n != 1 {
		t.Fatalf("the secret's route after a contributor's refusals: %d", n)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, stripe, body); code != 204 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes WHERE NOT from_secret") != 1 {
		t.Fatalf("an admin replacing a secret's route: %d %s; a PUT makes it one no secret placed", code, lastBody)
	}
	if code, _, _ := do(t, h.carlCLI, http.MethodDelete, stripe, ""); code != 204 {
		t.Fatalf("a contributor removing the route a PUT replaced: %d %s", code, lastBody)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, the route rows: a PUT's refusals, in order — :destination and the body before any state, then
// 409 egress_external while the org runs the apps, 409 held for a held production, 409 destination_not_allowed unless
// the check lets the env reach it, and 409 trust_secret naming the first trust variable by name — each byte for byte,
// nothing written; then 204.
func TestARouteIsRefusedInTheContractsOrder(t *testing.T) {
	h := outboundHost(t)
	var out map[string]any
	put := func(path, body string) int {
		t.Helper()
		var code int
		code, out, _ = do(t, h.olive, http.MethodPut, h.base+path, body)
		return code
	}
	prod := routePath("prod", "api.stripe.com")
	ok := routeBody("Authorization", "bearer", routeValue)
	for _, dest := range []string{"*.stripe.com", "10.0.0.1", "stripe", "api.stripe.com:443"} {
		if code := put(routePath("prod", dest), ok); code != 400 || lastBody != `{"error":"invalid_destination","message":"`+invalidRouteDestination+`"}` {
			t.Errorf("%s: %d %s", dest, code, lastBody)
		}
	}
	for field, bodies := range map[string][]string{
		"body":            {`[]`, `{"header":"x-api-key","scheme":"raw","value":"v","extra":1}`, `{"header":"x-api-key","header":"x-api-key","scheme":"raw","value":"v"}`, routeBody("x-api-key", "raw", "v") + `{}`, `{"header":"x-api-key","scheme":"raw","value":"v","secret":"S"}`},
		"header":          {`{"scheme":"raw","value":"v"}`, routeBody("Host", "raw", "v"), routeBody("X-Envoy-Original-Path", "raw", "v"), routeBody("Canyon-Caller", "raw", "v"), routeBody("Proxy-Authorization", "raw", "v"), routeBody("X Token", "raw", "v"), routeBody("Transfer-Encoding", "raw", "v")},
		"scheme":          {routeBody("x-api-key", "digest", "v"), `{"header":"x-api-key","value":"v"}`},
		"value":           {routeBody("x-api-key", "raw", ""), routeBody("x-api-key", "raw", `x\n`), routeBody("x-api-key", "raw", `x\r\nHost: evil.example`), routeBody("x-api-key", "basic", "no-colon"), routeBody("x-api-key", "raw", strings.Repeat("v", 8193)), `{"header":"x-api-key","scheme":"raw"}`},
		"placeholder_env": {`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"PORT"}`, `{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"SSL_CERT_DIR"}`, `{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"HTTPS_PROXY"}`, `{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"1KEY"}`, `{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"HOME"}`},
	} {
		for _, b := range bodies {
			if code := put(prod, b); code != 400 || out["error"] != "invalid_route" || out["field"] != field || out["message"] != routeFields[field] || len(out) != 3 {
				t.Errorf("%s: %d %s, want invalid_route %s", b, code, lastBody, field)
			}
		}
	}
	for _, q := range []string{
		"UPDATE org_preferences SET run_by = 'org'",
		`INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2231', 'person', 'sub-rhea', 'Rhea Okafor')`,
		"INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
	} {
		h.exec(t, q)
	}
	for _, name := range []string{"SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/"+name, `{"value":"/etc/ssl/own","env":"prod"}`); code != 204 {
			t.Fatalf("the secret %s: %d %s", name, code, lastBody)
		}
	}
	for _, step := range []struct{ want, then string }{
		{`{"error":"egress_external","message":"this Canyon hands its apps to your own platform, so their calls don't go through Canyon"}`, "UPDATE org_preferences SET run_by = 'canyon'"},
		{`"error":"held"`, "DELETE FROM prod_holds"},
		{`{"error":"destination_not_allowed","message":"allow api.stripe.com before you add a credential for it"}`, "INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', '*.stripe.com')"},
		{`{"error":"trust_secret","message":"delete the secret NODE_EXTRA_CA_CERTS first: it would replace the certificates Canyon needs to add this credential"}`, "DELETE FROM secrets WHERE name = 'NODE_EXTRA_CA_CERTS'"},
		{`{"error":"trust_secret","message":"delete the secret SSL_CERT_DIR first: it would replace the certificates Canyon needs to add this credential"}`, "DELETE FROM secrets"},
	} {
		if code := put(prod, ok); code != 409 || !strings.Contains(lastBody, step.want) || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 0 {
			t.Fatalf("%d %s, want %s, nothing written", code, lastBody, step.want)
		}
		h.exec(t, step.then)
	}
	if code := put(prod, ok); code != 204 {
		t.Fatalf("a route every refusal lets through: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM egress_allowed", "INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'login.example-idp.com')")
	if code := put(routePath("prod", "hooks.slack.com"), ok); code != 409 || !strings.Contains(lastBody, "destination_not_allowed") {
		t.Fatalf("a name the allowlist does not allow: %d %s", code, lastBody)
	}
	if code := put(routePath("preview", "hooks.slack.com"), ok); code != 204 {
		t.Fatalf("under observe any name: %d %s", code, lastBody)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/PERMISSIONS.md, Outbound, A secret's route is an owner's: a contributor's PUT or DELETE that finds a route
// no secret placed, and then waits on the locks every route write takes while an owner moves a secret into it, is
// 403 owner_required once it goes on, and the moved key stays, from_secret, its value whole: it can never be read
// back, so a write that replaced or removed it would lose it for good.
func TestASecretMovedInWhileAContributorWaitsIsAnOwners(t *testing.T) {
	h := outboundHost(t)
	stripe := h.base + routePath("prod", "api.stripe.com")
	moved := "UPDATE egress_routes SET from_secret = true, rev = nextval('egress_route_rev') WHERE destination = 'api.stripe.com'" // what an owner's from-secret writes
	for name, write := range map[string]func() int{
		"a contributor's PUT": func() int {
			c, _, _ := do(t, h.carl, http.MethodPut, stripe, routeBody("Authorization", "bearer", "a-value-the-contributor-typed"))
			return c
		},
		"a contributor's DELETE": func() int { c, _, _ := do(t, h.carlCLI, http.MethodDelete, stripe, ""); return c },
	} {
		h.exec(t, "DELETE FROM egress_routes")
		if code, _, _ := do(t, h.carl, http.MethodPut, stripe, routeBody("Authorization", "bearer", routeValue)); code != 204 {
			t.Fatalf("the route no secret placed: %d %s", code, lastBody)
		}
		var rev int
		if err := h.pool.QueryRow(h.ctx, "SELECT rev FROM egress_routes").Scan(&rev); err != nil {
			t.Fatal(err)
		}
		code := h.whileLocked(t, ledgerLock, moved, write)
		if code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive") {
			t.Errorf("%s, gone on after an owner moved a secret in: %d %s, want owner_required", name, code, lastBody)
		}
		if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes WHERE from_secret AND rev > "+strconv.Itoa(rev)); n != 1 {
			t.Errorf("%s: the moved key is gone or replaced (%d rows from the secret)", name, n)
		}
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, POST /api/v1/egress/ca/rotate: a body out of shape is 400 invalid_ca with its reason; a
// sub-CA with no name constraints 409 ca_unconstrained, D5's line, before a rotation under way, which is 409 rotating
// for one that has them; a sub-CA that excludes a route's destination 409 ca_excludes, naming it. Each writes no root
// and no egress.ca.rotate row.
func TestBYOIsRefusedInTheContractsOrder(t *testing.T) {
	h := outboundHost(t)
	h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	now := time.Now()
	if code, out, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{"cert":"x"}`); code != 400 || out["error"] != "invalid_ca" || out["reason"] != "body" {
		t.Fatalf("a body with no key: %d %s", code, lastBody)
	}
	unconstrained := `{"error":"ca_unconstrained","message":"Canyon takes only a sub-CA that is name-constrained to the destinations it adds credentials for, and that allows no IP addresses."}`
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, newOrgCA(t, now, nil).body(t)); code != 409 || lastBody != unconstrained {
		t.Fatalf("a sub-CA with no name constraints: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, newOrgCA(t, now, nil, "slack.com").body(t)); code != 409 || out["error"] != "ca_excludes" ||
		!strings.Contains(lastBody, `"destinations":["api.stripe.com"]`) {
		t.Fatalf("a sub-CA excluding the route's destination: %d %s", code, lastBody)
	}
	if count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 1 || count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.ca.rotate'") != 0 {
		t.Fatal("a refused sub-CA wrote a root or a rotation row")
	}
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 200 {
		t.Fatalf("a rotation: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, newOrgCA(t, now, nil).body(t)); code != 409 || lastBody != unconstrained {
		t.Fatalf("a sub-CA with no name constraints while a rotation is under way: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, newOrgCA(t, now, nil, "stripe.com").body(t)); code != 409 || out["error"] != "rotating" {
		t.Fatalf("a sub-CA while a rotation is under way: %d %s", code, lastBody)
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/EGRESS.md §4.5: an admin's sub-CA's key is sealed as a root's is, under ("egress", "root", <fingerprint>),
// and never read back: no answer, read or audit row carries it. Once every env holding a route trusts the sub-CA it
// signs, and a leaf's chain — the intermediate and the sub-CA, the org's root left out — verifies to the org's root;
// the key dates name it an admin's, which never rotates by itself.
func TestBYOKeyIsSealedAndNeverReadBack(t *testing.T) {
	h := outboundHost(t)
	old := h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	org := newOrgCA(t, time.Now(), nil, "stripe.com")
	pkcs8, err := x509.MarshalPKCS8PrivateKey(org.key)
	if err != nil {
		t.Fatal(err)
	}
	keyBody := base64.StdEncoding.EncodeToString(pkcs8)[:40]
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, org.body(t)); code != 200 || strings.Contains(lastBody, keyBody) || strings.Contains(lastBody, "PRIVATE KEY") {
		t.Fatalf("the admin's sub-CA: %d, the answer carrying its key %t", code, strings.Contains(lastBody, keyBody))
	}
	fp := org.fingerprint()
	var byo bool
	var certPEM, chain string
	var keyRef int
	var sealedKey, sealedValue []byte
	if err := h.pool.QueryRow(h.ctx, "SELECT byo, cert, chain, key_ref, sealed_key, sealed_value FROM egress_ca_roots WHERE fingerprint = $1", fp).
		Scan(&byo, &certPEM, &chain, &keyRef, &sealedKey, &sealedValue); err != nil {
		t.Fatal(err)
	}
	if !byo || certPEM != pemOf(org.sub) || chain != pemOf(org.root) || bytes.Contains(sealedValue, pkcs8) {
		t.Fatalf("the sub-CA's row: byo %t, its certificate %t, the root above it %t, the key in the clear %t", byo, certPEM == pemOf(org.sub), chain == pemOf(org.root), bytes.Contains(sealedValue, pkcs8))
	}
	if opened, err := vaultOf(t, h.base).Open(keyRef, "egress", "root", fp, sealedKey, sealedValue); err != nil || !bytes.Equal(opened, pkcs8) {
		t.Fatalf("the key is not sealed under the root's own row: %v", err)
	}
	for _, path := range []string{"/api/v1/egress", "/api/v1/audit"} {
		if code, _, _ := do(t, h.dana, http.MethodGet, h.base+path, ""); code != 200 || strings.Contains(lastBody, keyBody) || strings.Contains(lastBody, "PRIVATE KEY") {
			t.Fatalf("%s: %d, carrying the key %t", path, code, strings.Contains(lastBody, keyBody))
		}
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.ca.rotate' AND detail->>'root' = '"+fp+"' AND (detail->>'byo')::boolean"); n != 1 {
		t.Fatalf("%d egress.ca.rotate rows name the sub-CA", n)
	}
	ca := h.caOf(t)
	if !ca.Roots[0].BYO || ca.Roots[0].Fingerprint != fp || ca.RotatesAt != nil || !ca.Roots[0].NotAfter.Equal(org.sub.NotAfter.Truncate(time.Second)) {
		t.Fatalf("the key dates: %+v, rotating by itself at %v; an admin's sub-CA, newest, which never rotates by itself", ca.Roots[0], ca.RotatesAt)
	}
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+old+"', '"+fp+"'] WHERE env = 'prod'")
	s, base := policyAt(t, h.pool)
	s.ee = appOf(t, h.base).ee
	answer, leaf, between := leafFor(t, base, "api.stripe.com")
	if answer.Root != fp || len(between) != 2 || !between[1].Equal(org.sub) {
		t.Fatalf("the leaf under %s, its chain %d certificates: the sub-CA's, the intermediate and the sub-CA", answer.Root, len(between))
	}
	if err := verifies(leaf, "api.stripe.com", org.root, time.Now(), between...); err != nil {
		t.Fatalf("the leaf does not verify to the org's root: %v", err)
	}
}

// Moved from egress_credential_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// The owner's decision (2026-11-09, #2143; contracts/routes.md, the egress setting row; design/PERMISSIONS.md,
// Outbound, Credential routes): credential_routes is an admin's, from a browser, off until turned on.
//   - every member reads it;
//   - while it is off a route's write is 409 credential_routes_off, a contributor's and an owner's from-secret alike,
//     and removing one still works;
//   - an admin turning it either way writes egress.set naming it, a contributor is admin_required, a CLI session
//     dashboard_required;
//   - while it is on the policy names a trusting env's route; turning it off moves seq and the policy names none at
//     once, the route kept.
func TestCredentialRoutesGateRoutesAndTrust(t *testing.T) {
	h := outboundHost(t)
	egressWait = time.Second // rawPolicy reads the policy as a replica that never asks again: a change no longer waits it out (#136)
	t.Cleanup(func() { egressWait = egressDefault })
	h.exec(t, "UPDATE egress_settings SET credential_routes = false")
	for who, s := range map[string]*http.Client{"a contributor": h.carl, "the admin": h.dana} {
		if code, out, _ := do(t, s, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 || out["credential_routes"] != false {
			t.Errorf("%s reads the setting: %d %s, want credential_routes false", who, code, lastBody)
		}
	}
	const off = `{"error":"credential_routes_off","message":"Canyon adds no credentials to apps' calls until an admin turns them on in Settings › Outbound"}`
	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 409 || lastBody != off {
		t.Errorf("a route while credential routes are off: %d %s, want 409 %s", code, lastBody, off)
	}
	// a secret the vault opens, set as its owner sets one: the from-secret's own checks (contracts/routes.md, its row) come
	// before the setting's, and one it cannot open is a 500, nothing written
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/STRIPE_KEY", `{"value":"`+routeValue+`","env":"prod"}`); code != 204 {
		t.Fatalf("setting the secret: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodPost, h.base+routePath("prod", "api.stripe.com")+"/from-secret", `{"secret":"STRIPE_KEY","header":"Authorization","scheme":"bearer"}`); code != 409 || lastBody != off {
		t.Errorf("a from-secret while credential routes are off: %d %s, want 409 %s", code, lastBody, off)
	}

	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":true}`); code != 403 {
		t.Errorf("a contributor turning credential routes on: %d %s, want 403 admin_required", code, lastBody)
	}
	if code, _, _ := do(t, h.danaCLI, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":true}`); code != 403 || lastBody != dashboardRequired {
		t.Errorf("the admin from the CLI: %d %s, want 403 dashboard_required", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":"yes"}`); code != 400 {
		t.Errorf("credential_routes not a boolean: %d %s, want 400 invalid_via", code, lastBody)
	}
	if code, out, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":true}`); code != 200 || out["credential_routes"] != true {
		t.Fatalf("the admin turning credential routes on: %d %s", code, lastBody)
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.set' AND detail = '{"credential_routes": {"after": true, "before": false}}'`); n != 1 {
		t.Errorf("%d egress.set rows naming credential routes turned on, want 1", n)
	}

	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("a route once credential routes are on: %d %s", code, lastBody)
	}
	h.trusting(t, "preview")
	_, base := policyAt(t, h.pool)
	if _, p := rawPolicy(t, base); len(routesOf(p, "preview")) != 1 {
		t.Fatalf("credential routes on, preview trusting the signing root: the policy names %v, want its route", routesOf(p, "preview"))
	}
	before := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":false}`); code != 200 {
		t.Fatalf("the admin turning credential routes off: %d %s", code, lastBody)
	}
	if after := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy"); after <= before {
		t.Error("turning credential routes off moved no seq: a replica would keep naming the route")
	}
	if _, p := rawPolicy(t, base); len(routesOf(p, "preview")) != 0 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 1 {
		t.Errorf("credential routes off: the policy names %v, want none at once and the route kept", routesOf(p, "preview"))
	}
	if code, _, _ := do(t, h.carl, http.MethodDelete, h.base+routePath("preview", "api.stripe.com"), ""); code != 204 {
		t.Errorf("removing a route while credential routes are off: %d %s, want 204", code, lastBody)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, credentials_exist: a route works only through Canyon, so an admin's via to off is refused
// while any app env holds one, naming the apps, byte for byte, before anything is written; so is run_by to org. Once
// none holds one, both are taken.
func TestCredentialsKeepAppsCallingOutThroughCanyon(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, "UPDATE egress_settings SET via = 'canyon'", "UPDATE projects SET name = 'Claims portal' WHERE id = 'claims-portal'")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	exists := `{"apps":["Claims portal"],"error":"credentials_exist","message":"Claims portal has credentials that work only through Canyon. Its team needs to set those keys as secrets again and deploy, then remove the credentials."}`
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"via":"off"}`); code != 409 || lastBody != exists {
		t.Fatalf("via to off while an app holds a credential: %d %s", code, lastBody)
	}
	if via := count(t, h.ctx, h.pool, "SELECT (via = 'canyon')::int FROM egress_settings"); via != 1 || count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.set'") != 0 {
		t.Fatal("a refused via wrote something")
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"mode":"allowlist"}`); code != 200 {
		t.Fatalf("a mode alone is no move off Canyon: %d %s", code, lastBody)
	}

	onKubernetes(t)
	kube, _ := start(t, h.pool, time.Hour)
	dana := signedIn(t, h.ctx, h.pool, kube, "sub-dana", "dana@meridian.example", false)
	servesHandOff(t)
	withWebhookRow(t, h.ctx, h.pool)
	withDoorRow(t, h.ctx, h.pool, 1)
	if code, _ := putPreferences(t, dana, kube, `{"run_by":"org"}`); code != 409 || lastBody != exists {
		t.Fatalf("run_by to org while an app holds a credential: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM egress_routes")
	if code, body := putPreferences(t, dana, kube, `{"run_by":"org"}`); code != 200 || body["run_by"] != "org" {
		t.Fatalf("run_by to org once none does: %d %s", code, lastBody)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// A project's delete removes its routes with its secret rows (contracts/routes.md, the route rows).
func TestDeletingAnAppRemovesItsRoutes(t *testing.T) {
	h := outboundHost(t)
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("x-api-key", "raw", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodDelete, h.base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes"); n != 0 {
		t.Fatalf("%d routes outlive their app", n)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, the from-secret row: an owner moves a runtime secret into a route only while the route would
// be live at once — else 409 would_wait, byte for byte, reason host while via is off and redeploy while the env lacks
// the signing root, the secret where it was — and then, in one transaction, the route is set from the secret's value,
// from_secret, its placeholder the secret's own name, and the secret's row gone; one egress.route.set row names the
// secret and the destination, never the value. No such runtime secret is 404, a provisioned one 409 provisioned, and
// a value no header may carry 400 invalid_route.
func TestFromSecretMovesASecretOnlyIntoARouteLiveAtOnce(t *testing.T) {
	h := outboundHost(t)
	stripe := h.base + routePath("prod", "api.stripe.com") + "/from-secret"
	move := `{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer"}`
	if code, _, _ := do(t, h.olive, http.MethodPost, stripe, move); code != 404 || !strings.Contains(lastBody, `"error":"not_found"`) {
		t.Fatalf("no such secret: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"`+routeValue+`","env":"prod"}`); code != 204 {
		t.Fatalf("the secret: %d %s", code, lastBody)
	}
	still := func(what string) {
		t.Helper()
		if count(t, h.ctx, h.pool, "SELECT count(*) FROM secrets WHERE name = 'STRIPE_SECRET_KEY'") != 1 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 0 {
			t.Fatalf("%s: the secret moved", what)
		}
	}
	if code, _, _ := do(t, h.olive, http.MethodPost, stripe, move); code != 409 ||
		lastBody != `{"error":"would_wait","message":"ask an admin to turn on Through Canyon first: apps call out directly here, so Canyon can't add this credential yet","reason":"host"}` {
		t.Fatalf("under via off: %d %s", code, lastBody)
	}
	still("under via off")
	root := h.trusting(t, "prod")
	h.exec(t, "UPDATE deploys SET egress_roots = '{}' WHERE env = 'prod'")
	if code, _, _ := do(t, h.olive, http.MethodPost, stripe, move); code != 409 ||
		lastBody != `{"error":"would_wait","message":"the version running in production doesn't trust Canyon's certificate until its next deploy or promotion, so the secret stays where it is","reason":"redeploy"}` {
		t.Fatalf("while prod lacks the signing root: %d %s", code, lastBody)
	}
	still("while prod lacks the signing root")
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+root+"'] WHERE env = 'prod'")
	if code, _, _ := do(t, h.olive, http.MethodPost, stripe, move); code != 204 {
		t.Fatalf("a route live at once: %d %s", code, lastBody)
	}
	if count(t, h.ctx, h.pool, "SELECT count(*) FROM secrets") != 0 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes WHERE from_secret AND placeholder_env = 'STRIPE_SECRET_KEY' AND header = 'authorization'") != 1 ||
		count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.route.set' AND detail = '{"env": "prod", "secret": "STRIPE_SECRET_KEY", "destination": "api.stripe.com"}'`) != 1 {
		t.Fatal("the move: the secret gone, the route from the secret named for its placeholder, one row naming the secret")
	}
	s, base := policyAt(t, h.pool)
	s.ee = appOf(t, h.base).ee
	if code, body, _ := routeValueAt(t, base, "app=claims-portal&env=prod&destination=api.stripe.com"); code != 200 || !strings.Contains(body, `"value":"`+routeValue+`"`) {
		t.Fatalf("the moved value: %d", code)
	}
	for name, seed := range map[string]string{
		"a build value":     "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('claims-portal', 'preview', 'OPENAI_API_KEY', 'build', 1, '', '', 'person', 'sub-olive')",
		"a provisioned one": "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, origin) VALUES ('claims-portal', 'preview', 'OPENAI_API_KEY', 'runtime', 1, '', '', 'canyon', 'provisioner', 'provisioned')",
	} {
		h.exec(t, "DELETE FROM secrets", seed)
		code, _, _ := do(t, h.olive, http.MethodPost, h.base+routePath("preview", "api.openai.com")+"/from-secret", `{"secret":"OPENAI_API_KEY","header":"Authorization","scheme":"bearer","placeholder_env":null}`)
		if want := map[string]int{"a build value": 404, "a provisioned one": 409}[name]; code != want {
			t.Errorf("%s: %d %s, want %d", name, code, lastBody, want)
		}
	}
	h.exec(t, "DELETE FROM secrets")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/OPENAI_API_KEY", `{"value":"line one\nline two","env":"prod"}`); code != 204 {
		t.Fatalf("a secret of two lines: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, h.olive, http.MethodPost, h.base+routePath("prod", "api.stripe.com")+"/from-secret", `{"secret":"OPENAI_API_KEY","header":"Authorization","scheme":"bearer"}`); code != 400 ||
		out["error"] != "invalid_route" || out["field"] != "value" || out["message"] != routeFields["value"] {
		t.Fatalf("a value no header may carry: %d %s", code, lastBody)
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, GET /egress/leaf: egress-control alone, behind the policy's proof — else 401, no body — asks
// for exactly one destination, 400 otherwise, and 404 for one no route names; for a route's destination it gets a
// leaf minted for that answer, never cached, under the signing root the policy names, its chain verifying to the
// root, and the leaf's key its own. Its one log line names who asked and the destination, and no log line or audit
// row carries a key. With no root live there is no leaf, 503, and the policy names no route.
func TestLeafAnswersEgressControlAlone(t *testing.T) {
	h := outboundHost(t)
	root := h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	s, base := policyAt(t, h.pool)
	var logs bytes.Buffer
	s.ee, s.log = appOf(t, h.base).ee, log.New(&logs, "", 0)
	s.replica = func(r *http.Request) (string, bool) { id := r.Header.Get("X-Replica"); return id, id != "" }
	if code, body, _ := leafAt(t, base, "", "destination=api.stripe.com"); code != 401 || body != "" {
		t.Fatalf("an ask without the policy's proof: %d %q", code, body)
	}
	for _, q := range []string{"", "destination=api.stripe.com&destination=api.stripe.com", "destination=api.stripe.com&app=claims-portal", "dest=api.stripe.com", "destination=%zz"} {
		if code, body, _ := leafAt(t, base, "canyon-egress-a", q); code != 400 || !strings.Contains(body, `"error":"invalid_request"`) {
			t.Errorf("the query %q: %d %s", q, code, body)
		}
	}
	if code, body, _ := leafAt(t, base, "canyon-egress-a", "destination=hooks.slack.com"); code != 404 || !strings.Contains(body, `"error":"no_route"`) {
		t.Fatalf("a destination no route names: %d %s", code, body)
	}
	answer, leaf, chain := leafFor(t, base, "api.stripe.com")
	_, p := rawPolicy(t, base)
	if answer.Root != root || p.SigningRoot == nil || *p.SigningRoot != root || len(chain) != 1 {
		t.Fatalf("the leaf under %s, the policy's signing root %v, its chain %d certificates: the root's, and the intermediate alone", answer.Root, deref(p.SigningRoot), len(chain))
	}
	if err := verifies(leaf, "api.stripe.com", h.certOf(t, root), time.Now(), chain...); err != nil {
		t.Fatalf("the leaf does not verify to the root: %v", err)
	}
	if !slices.Equal(chain[0].PermittedDNSDomains, []string{"api.stripe.com"}) || !answer.NotAfter.Equal(leaf.NotAfter) {
		t.Fatalf("the intermediate permits %v, the leaf ends %s, answered %s", chain[0].PermittedDNSDomains, leaf.NotAfter, answer.NotAfter)
	}
	block, _ := pem.Decode([]byte(answer.Key))
	if priv, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil || !priv.(*ecdsa.PrivateKey).PublicKey.Equal(leaf.PublicKey) {
		t.Fatalf("the key is not the leaf's: %v", err)
	}
	keyBody := base64.StdEncoding.EncodeToString(block.Bytes)[:40]
	if line := logs.String(); !strings.Contains(line, `leaf of "api.stripe.com" for "canyon-egress-a"`) || strings.Contains(line, "PRIVATE KEY") || strings.Contains(line, keyBody) {
		t.Fatalf("one log line names who asked and the destination, never a key: %q", line)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE detail::text LIKE '%PRIVATE KEY%' OR detail::text LIKE '%"+keyBody+"%'"); n != 0 {
		t.Fatalf("%d audit rows carry a key", n)
	}
	h.exec(t, "UPDATE egress_ca_roots SET retires_at = now() - interval '1 second'")
	if code, body, _ := leafAt(t, base, "canyon-egress-a", "destination=api.stripe.com"); code != 503 || !strings.Contains(body, `"error":"no_signing_root"`) {
		t.Fatalf("no root live: %d %s", code, body)
	}
	if _, p := rawPolicy(t, base); p.SigningRoot != nil || len(routesOf(p, "prod")) != 0 {
		t.Fatalf("no root live: the policy names the signing root %v and prod's routes %v", deref(p.SigningRoot), routesOf(p, "prod"))
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/EGRESS.md §4.5: the intermediate is "re-minted on any route change". Two leaves for one destination share
// the intermediate, each with a key of its own; a route to another destination mints it again, permitting both, and
// its removal again, permitting the one left, whose leaf the other destination no longer gets.
func TestLeafIntermediateFollowsTheRoutes(t *testing.T) {
	h := outboundHost(t)
	h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	s, base := policyAt(t, h.pool)
	s.ee = appOf(t, h.base).ee
	first, _, _ := leafFor(t, base, "api.stripe.com")
	second, _, chain := leafFor(t, base, "api.stripe.com")
	if first.Chain != second.Chain || first.Key == second.Key || first.Cert == second.Cert {
		t.Fatal("two leaves for one destination: one intermediate, two leaves, two keys")
	}
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("preview", "hooks.slack.com"), routeBody("X-Slack-Token", "raw", routeValue)); code != 204 {
		t.Fatalf("preview's route: %d %s", code, lastBody)
	}
	again, _, both := leafFor(t, base, "api.stripe.com")
	if again.Chain == first.Chain || !slices.Equal(both[0].PermittedDNSDomains, []string{"api.stripe.com", "hooks.slack.com"}) || chain[0].Equal(both[0]) {
		t.Fatalf("a route to another destination: the intermediate permits %v", both[0].PermittedDNSDomains)
	}
	if _, leaf, _ := leafFor(t, base, "hooks.slack.com"); !slices.Equal(leaf.DNSNames, []string{"hooks.slack.com"}) {
		t.Fatalf("the new destination's leaf names %v", leaf.DNSNames)
	}
	if code, _, _ := do(t, h.olive, http.MethodDelete, h.base+routePath("preview", "hooks.slack.com"), ""); code != 204 {
		t.Fatalf("preview's route removed: %d %s", code, lastBody)
	}
	if _, _, one := leafFor(t, base, "api.stripe.com"); !slices.Equal(one[0].PermittedDNSDomains, []string{"api.stripe.com"}) {
		t.Fatalf("the route removed: the intermediate permits %v", one[0].PermittedDNSDomains)
	}
	if code, _, _ := leafAt(t, base, "canyon-egress-a", "destination=hooks.slack.com"); code != 404 {
		t.Fatalf("a destination whose route is gone: %d", code)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/PERMISSIONS.md, Outbound: "No route, row or setting returns a route's value, to any level, an admin's
// included." A route set from the dashboard is read back by every reader — the app's egress read, to its owner, a
// contributor and an admin, the secrets list, the audit trail and the policy — and none carries its value; only GET
// /egress/route-value, egress-control's, answers it, never to a cache, under the rev the policy names, and its one
// log line names who asked and what, never the value.
func TestNoRouteAnswersAValue(t *testing.T) {
	h := outboundHost(t)
	h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "API.Stripe.com"), `{"header":"Authorization","scheme":"bearer","value":"`+routeValue+`","placeholder_env":"STRIPE_SECRET_KEY"}`); code != 204 {
		t.Fatalf("an owner's route from the dashboard: %d %s", code, lastBody)
	}
	for name, c := range map[string]*http.Client{"the owner": h.olive, "a contributor": h.carl, "an admin": h.dana} {
		if code, _, _ := do(t, c, http.MethodGet, h.base+outboundApp, ""); code != 200 || strings.Contains(lastBody, routeValue) {
			t.Fatalf("%s's egress read: %d, carrying the value %t", name, code, strings.Contains(lastBody, routeValue))
		}
		egressShape(t, "project_read", lastBody)
	}
	env := h.envRead(t, "prod")
	if routes := env["routes"].([]any); len(routes) != 1 || !mapsEqual(routes[0].(map[string]any), map[string]any{"destination": "api.stripe.com", "header": "authorization", "scheme": "bearer",
		"placeholder_env": "STRIPE_SECRET_KEY", "from_secret": false, "waiting": nil}) {
		t.Fatalf("prod's routes: %v", routes)
	}
	for _, path := range []string{"/api/v1/projects/claims-portal/secrets", "/api/v1/audit?project=claims-portal", "/api/v1/egress", "/api/v1/egress/destinations"} {
		if code, _, _ := do(t, h.dana, http.MethodGet, h.base+path, ""); code != 200 || strings.Contains(lastBody, routeValue) {
			t.Fatalf("%s: %d, carrying the value %t", path, code, strings.Contains(lastBody, routeValue))
		}
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.route.set' AND detail = '{"env": "prod", "destination": "api.stripe.com"}'`); n != 1 {
		t.Fatalf("%d egress.route.set rows naming the env and the destination alone", n)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE detail::text LIKE '%"+routeValue+"%'"); n != 0 {
		t.Fatalf("%d audit rows carry the value", n)
	}
	s, base := policyAt(t, h.pool)
	var logs bytes.Buffer
	s.ee, s.log = appOf(t, h.base).ee, log.New(&logs, "", 0)
	raw, p := rawPolicy(t, base)
	routes := routesOf(p, "prod")
	if strings.Contains(raw, routeValue) || len(routes) != 1 || routes[0].Destination != "api.stripe.com" || routes[0].Header != "authorization" || routes[0].Scheme != "bearer" {
		t.Fatalf("the policy names prod's route, never its value: %s", raw)
	}
	code, body, cache := routeValueAt(t, base, "app=claims-portal&env=prod&destination=api.stripe.com")
	if code != 200 || cache != "no-store" {
		t.Fatalf("the route's value to egress-control: %d %q", code, cache)
	}
	egressShape(t, "route_value", body)
	var answer struct {
		Rev   int64  `json:"rev"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Value != routeValue || answer.Rev != routes[0].Rev {
		t.Fatalf("the value under the policy's rev %d: rev %d, the value %t (%v)", routes[0].Rev, answer.Rev, answer.Value == routeValue, err)
	}
	if line := logs.String(); !strings.Contains(line, `route value of "claims-portal" "prod" "api.stripe.com" for "canyon-egress-a"`) || strings.Contains(line, routeValue) {
		t.Fatalf("one log line names who asked and what, never the value: %q", line)
	}
}

// Moved from egress_policy_pull_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// Every write that can change what the policy says moves its version, in the writing transaction: the org's mode,
// an app env's, an entry allowed and removed, a route set and removed, a root minted, an env's trusted roots
// recorded, the front door saved, an env Canyon starts and stops running — its
// deploy row under via canyon names it, under via off, handed to the org or gone drops it, its callers with it —
// a caller resolved, and a TRUNCATE of any of those tables, which fires no row trigger. Requests, which the
// policy does not carry, a refusal counted and a replica seen do not.
func TestPolicyVersionMovesOnEveryWrite(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, "UPDATE egress_settings SET via = 'canyon'", `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'preview', 'api.openai.com', 'pending', now(), now(), 1), ('claims-portal', 'preview', 'hooks.slack.com', 'pending', now(), now(), 1)`)
	seq := func() int64 {
		var s int64
		if err := h.pool.QueryRow(h.ctx, "SELECT seq FROM egress_policy").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	route := func(c *http.Client, method, path, body string) func() {
		return func() {
			if code, _, _ := do(t, c, method, h.base+path, body); code != 200 && code != 204 {
				t.Fatalf("%s %s: %d %s", method, path, code, lastBody)
			}
		}
	}
	named := func(env string) int {
		return count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_envs WHERE env = '"+env+"'") + count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_callers WHERE env = '"+env+"'")
	}
	if named("preview") != 1 || named("prod") != 1 {
		t.Fatalf("the deploy rows under via canyon name %d preview and %d prod row(s), want each env", named("preview"), named("prod"))
	}
	for _, step := range []struct {
		name  string
		do    func()
		names int // the preview env and its callers after it, -1 unchecked
	}{
		{"the org's mode", route(h.dana, http.MethodPut, "/api/v1/egress", `{"mode":"allowlist"}`), -1},
		{"an app env's mode", route(h.olive, http.MethodPut, outboundApp+"/preview/mode", `{"mode":"allowlist"}`), -1},
		{"an entry allowed", route(h.carl, http.MethodPut, destinationPath("preview", "api.stripe.com"), ""), -1},
		{"a route set", route(h.carl, http.MethodPut, routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)), -1},
		{"the route removed", route(h.carl, http.MethodDelete, routePath("preview", "api.stripe.com"), ""), -1},
		{"a root minted", func() { seedRoot(t, h.ctx, h.pool, vaultOf(t, h.base)) }, -1},
		{"an env's trusted roots recorded", func() { h.exec(t, "UPDATE deploys SET egress_roots = ARRAY[repeat('a', 64)] WHERE env = 'preview'") }, -1},
		{"an env's trusted roots narrowed to none", func() { h.exec(t, "UPDATE deploys SET egress_roots = '{}' WHERE env = 'preview'") }, -1},
		{"the entry removed", route(h.carl, http.MethodDelete, destinationPath("preview", "api.stripe.com"), ""), -1},
		{"the front door saved", func() {
			h.exec(t, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), '{"choice": 1}', 'person', 'dana', 'Dana')`)
		}, -1},
		{"a caller resolved", func() {
			h.exec(t, "INSERT INTO egress_callers (address, project_id, env) VALUES ('172.28.3.10', 'claims-portal', 'preview')")
		}, 2},
		{"a redeploy under via off", func() { h.exec(t, `UPDATE deploys SET egress = '{"via": "off"}' WHERE env = 'preview'`) }, 0},
		{"a redeploy under via canyon", func() { h.exec(t, `UPDATE deploys SET egress = '{"via": "canyon"}' WHERE env = 'preview'`) }, 1},
		{"a caller resolved again", func() {
			h.exec(t, "INSERT INTO egress_callers (address, project_id, env) VALUES ('172.28.3.10', 'claims-portal', 'preview')")
		}, 2},
		{"handed to the org", func() { h.exec(t, "UPDATE deploys SET run_by = 'org' WHERE env = 'preview'") }, 0},
		{"run by Canyon again", func() { h.exec(t, "UPDATE deploys SET run_by = 'canyon' WHERE env = 'preview'") }, 1},
		{"undeployed", func() { h.exec(t, "DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'") }, 0},
		{"the allowlists emptied by a TRUNCATE, which fires no row trigger", func() { h.exec(t, "TRUNCATE egress_allowed") }, -1},
		{"the routes emptied by a TRUNCATE", func() { h.exec(t, "TRUNCATE egress_routes") }, -1},
		{"the roots emptied by a TRUNCATE", func() { h.exec(t, "TRUNCATE egress_ca_roots") }, -1},
		{"every deploy row emptied by a TRUNCATE", func() { h.exec(t, "TRUNCATE deploys") }, -1},
	} {
		was := seq()
		step.do()
		if seq() <= was {
			t.Errorf("%s: seq stayed %d", step.name, was)
		}
		if n := named("preview"); step.names >= 0 && n != step.names {
			t.Errorf("%s: the preview env and its callers are %d row(s), want %d", step.name, n, step.names)
		}
	}
	if n := named("preview") + named("prod"); n != 0 {
		t.Errorf("%d env(s) and caller(s) left after the deploy rows went, want none", n)
	}
	was := seq()
	route(h.carl, http.MethodPost, outboundApp+"/preview/requests/hooks.slack.com/dismiss", "")()
	h.exec(t, "UPDATE egress_requests SET refusals = refusals + 1, last_refused_at = now()", "INSERT INTO egress_replicas (replica, seen_at) VALUES ('r', now())",
		"INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com')")
	was++ // the entry, a row written
	h.exec(t, "INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com') ON CONFLICT DO NOTHING")
	if seq() != was {
		t.Errorf("seq %d, want %d: a request dismissed, a refusal counted, a replica seen or an entry allowed again moved it", seq(), was)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, the policy row: rev is drawn for each write of a route, a PUT or a from-secret, from one
// sequence over every route write of the host, so it never repeats — a route deleted and set again included — and a
// route written moves seq, as a row of any table the policy is read from does.
func TestRevComesFromOneSequenceAndNeverRepeats(t *testing.T) {
	h := outboundHost(t)
	seq := func() int {
		t.Helper()
		return count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	}
	rev := func(env string) int {
		t.Helper()
		return count(t, h.ctx, h.pool, "SELECT coalesce((SELECT rev FROM egress_routes WHERE env = '"+env+"' AND destination = 'api.stripe.com'), 0)")
	}
	var seen []int
	for _, step := range []struct{ method, env string }{{http.MethodPut, "prod"}, {http.MethodPut, "preview"}, {http.MethodPut, "prod"}, {http.MethodDelete, "prod"}, {http.MethodPut, "prod"}} {
		before := seq()
		if code, _, _ := do(t, h.olive, step.method, h.base+routePath(step.env, "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
			t.Fatalf("%s %s: %d %s", step.method, step.env, code, lastBody)
		}
		if seq() <= before {
			t.Fatalf("%s %s moved no seq", step.method, step.env)
		}
		if step.method == http.MethodPut {
			r := rev(step.env)
			if r < 1 || slices.Contains(seen, r) || len(seen) > 0 && r <= slices.Max(seen) {
				t.Fatalf("%s %s drew rev %d after %v: one sequence, never repeating", step.method, step.env, r, seen)
			}
			seen = append(seen, r)
		}
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, Interception, Trust: an env holding a route holds the old root back only while Canyon runs
// it. Preview, holding a route and trusting nothing, keeps the old root signing while its deploy row is Canyon's;
// handed to the org, or with no deploy row, it holds nothing back — the new root signs — and its routes wait for a
// deploy, as it trusts nothing.
func TestRotationCountsOnlyEnvsCanyonRuns(t *testing.T) {
	h := outboundHost(t)
	old := h.trusting(t, "prod")
	for _, env := range []string{"prod", "preview"} {
		if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath(env, "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
			t.Fatalf("%s's route: %d %s", env, code, lastBody)
		}
	}
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 200 {
		t.Fatalf("the rotation: %d %s", code, lastBody)
	}
	newer := h.caOf(t).Roots[0].Fingerprint
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+old+"', '"+newer+"'] WHERE env = 'prod'", "UPDATE deploys SET egress_roots = NULL WHERE env = 'preview'")
	for _, c := range []struct {
		what, q, want string
	}{
		{"preview Canyon's, trusting nothing", "UPDATE deploys SET run_by = 'canyon' WHERE env = 'preview'", old},
		{"preview the org's", "UPDATE deploys SET run_by = 'org' WHERE env = 'preview'", newer},
		{"preview Canyon's again", "UPDATE deploys SET run_by = 'canyon' WHERE env = 'preview'", old},
		{"preview with no deploy row", "DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'", newer},
	} {
		h.exec(t, c.q)
		if ca := h.caOf(t); ca.Signing == nil || *ca.Signing != c.want {
			t.Fatalf("%s: %v signs, want %s", c.what, deref(ca.Signing), c.want)
		}
	}
	if waiting := h.envRead(t, "preview")["routes"].([]any)[0].(map[string]any)["waiting"]; !mapsEqualAny(waiting, map[string]any{"reason": "redeploy", "secret": nil}) {
		t.Fatalf("preview with no deploy row waits %v, want redeploy", waiting)
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// design/PERMISSIONS.md, Host settings: a rotation is an admin's, a person's, from a browser — an owner is 403
// admin_required, the admin's CLI session dashboard_required, an agent person_required, each one denied row — and
// the admin's {} starts one: a new root, newest, the old one ending 60 days on and signing until every env holding a
// route trusts the new, one egress.ca.rotate row naming both and no key, and the key dates in the answer. A second
// while it is under way is 409 rotating.
func TestRotationIsAnAdminsFromABrowser(t *testing.T) {
	h := outboundHost(t)
	old := h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodPost, h.base+rotatePath, `{}`); code != 403 || !strings.Contains(lastBody, `"error":"admin_required"`) {
		t.Fatalf("an owner's rotation: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.danaCLI, http.MethodPost, h.base+rotatePath, `{}`); code != 403 || lastBody != dashboardRequired {
		t.Fatalf("the admin's rotation from the CLI: %d %s", code, lastBody)
	}
	h.asAgent(t, "sub-dana", true)
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 403 || lastBody != personRequired {
		t.Fatalf("an agent's rotation: %d %s", code, lastBody)
	}
	h.asAgent(t, "sub-dana", false)
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' IN ('admin_required', 'dashboard_required', 'person_required')"); n != 3 {
		t.Fatalf("%d denied rows, want one a refusal", n)
	}
	if count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 1 {
		t.Fatal("a refused rotation wrote a root")
	}
	before := time.Now()
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 200 {
		t.Fatalf("the admin's rotation: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	ca := h.caOf(t)
	if len(ca.Roots) != 2 || ca.Roots[1].Fingerprint != old || ca.Roots[0].Fingerprint == old || ca.Roots[0].BYO || ca.Signing == nil || *ca.Signing != old {
		t.Fatalf("after the rotation: %+v, signing %v; the new root newest, the old one signing while prod lacks the new", ca.Roots, deref(ca.Signing))
	}
	newer, ends := ca.Roots[0].Fingerprint, ca.Roots[1].EndsAt
	if ends.Before(before.Add(rotationOverlap).Add(-time.Minute)) || ends.After(time.Now().Add(rotationOverlap).Add(time.Minute)) || ca.RotatesAt == nil || !ca.RotatesAt.Equal(ca.Roots[0].EndsAt.Add(-rotationOverlap)) {
		t.Fatalf("the old root ends %s, Canyon rotates by itself at %v: 60 days on, and 60 days before the new one's end", ends, ca.RotatesAt)
	}
	var detail string
	if err := h.pool.QueryRow(h.ctx, "SELECT detail::text FROM audit_events WHERE action = 'egress.ca.rotate' AND actor_kind = 'person'").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Root     string `json:"root"`
		BYO      bool   `json:"byo"`
		Previous []struct {
			Root   string    `json:"root"`
			EndsAt time.Time `json:"ends_at"`
		} `json:"previous"`
	}
	if err := json.Unmarshal([]byte(detail), &row); err != nil || row.Root != newer || row.BYO || len(row.Previous) != 1 || row.Previous[0].Root != old || !row.Previous[0].EndsAt.Equal(ends) {
		t.Fatalf("one egress.ca.rotate row names the new root and the old one's end: %s", detail)
	}
	if code, out, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 409 || out["error"] != "rotating" || out["ends_at"] != ends.Format(time.RFC3339) {
		t.Fatalf("a rotation while one is under way: %d %s", code, lastBody)
	}
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+old+"', '"+newer+"'] WHERE env = 'prod'")
	if ca := h.caOf(t); deref(ca.Signing) != newer {
		t.Fatalf("every env holding a route trusts the new root: %s signs", deref(ca.Signing))
	}
}

// Moved from egress_ca_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, the policy row: a root passing its end changes the signing root with no row written, so the
// policy's next answer moves seq first — a replica holding the version before is answered 200, never 304, naming the
// new signing root and no route of an env that lacks it — and then 304 for the new version.
func TestRotationMovesSeqWhenARootPassesItsEnd(t *testing.T) {
	h := outboundHost(t)
	old := h.trusting(t, "prod")
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPost, h.base+rotatePath, `{}`); code != 200 {
		t.Fatalf("the rotation: %d %s", code, lastBody)
	}
	newer := h.caOf(t).Roots[0].Fingerprint
	h.exec(t, "UPDATE egress_ca_roots SET retires_at = now() + interval '2 seconds' WHERE fingerprint = '"+old+"'")
	_, base := policyAt(t, h.pool)
	code, before := pullPolicy(t, base, "canyon-egress-a", "have=")
	if code != 200 || before.SigningRoot == nil || *before.SigningRoot != old || len(routesOf(before, "prod")) != 1 {
		t.Fatalf("before the old root's end: %d, signing %v, prod's routes %v", code, deref(before.SigningRoot), routesOf(before, "prod"))
	}
	time.Sleep(2500 * time.Millisecond)
	code, after := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(before))
	if code != 200 || after.SigningRoot == nil || *after.SigningRoot != newer || len(routesOf(after, "prod")) != 0 || after.Seq <= before.Seq {
		t.Fatalf("past the old root's end, no row written: %d, signing %v, prod's routes %v, seq %d after %d", code, deref(after.SigningRoot), routesOf(after, "prod"), after.Seq, before.Seq)
	}
	if code, _ := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(after)); code != 304 {
		t.Fatalf("the new version again: %d, want 304", code)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, GET /egress/route-value: egress-control's alone, under the policy's proof (401, no body,
// without it); exactly app, env and destination, each once, else 400 invalid_request; 404 no_route for an env the
// policy does not name or a destination it holds no route to.
func TestRouteValueAnswersEgressControlAlone(t *testing.T) {
	h := outboundHost(t)
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("x-api-key", "raw", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	s, base := policyAt(t, h.pool)
	s.ee = appOf(t, h.base).ee
	for _, q := range []string{"", "app=claims-portal&env=prod", "app=claims-portal&env=prod&destination=api.stripe.com&have=1", "app=claims-portal&app=claims-portal&env=prod&destination=api.stripe.com", "app=claims-portal&env=prod&destination=api.stripe.com&%zz"} {
		if code, body, _ := routeValueAt(t, base, q); code != 400 || strings.TrimSpace(body) != `{"error":"invalid_request","message":"the query is app, env and destination, each once"}` {
			t.Errorf("%q: %d %s", q, code, body)
		}
	}
	for _, q := range []string{"app=claims-portal&env=prod&destination=hooks.slack.com", "app=claims-portal&env=preview&destination=api.stripe.com", "app=intake-bot&env=prod&destination=api.stripe.com"} {
		if code, body, _ := routeValueAt(t, base, q); code != 404 || !strings.Contains(body, `"error":"no_route"`) {
			t.Errorf("%q: %d %s", q, code, body)
		}
	}
	h.exec(t, "DELETE FROM egress_envs WHERE env = 'prod'")
	if code, body, _ := routeValueAt(t, base, "app=claims-portal&env=prod&destination=api.stripe.com"); code != 404 || strings.Contains(body, routeValue) {
		t.Fatalf("an env the policy does not name: %d", code)
	}
	s.replica = func(*http.Request) (string, bool) { return "", false }
	if code, body, _ := routeValueAt(t, base, "app=claims-portal&env=prod&destination=api.stripe.com"); code != 401 || body != "" {
		t.Fatalf("without the policy's proof: %d %q", code, body)
	}
}

// Moved from routes_egress_routes_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// contracts/routes.md, Interception, Trust: the policy names an env's routes only while the signing root is among
// its trusted roots, the host's via canyon and its run_by canyon; the read says why each waits otherwise, the first
// that holds — host, then a trust secret, then redeploy. Nothing recorded is the empty set. A write of an env's trusted
// roots moves seq. With a second, newer root, the old signs until every env holding a route trusts the new, and at
// the old root's end the new signs whoever lacks it.
func TestThePolicyNamesRoutesOnlyWhileTheEnvTrustsTheSigningRoot(t *testing.T) {
	h := outboundHost(t)
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("the route: %d %s", code, lastBody)
	}
	_, base := policyAt(t, h.pool)
	waiting := func(env string) any {
		t.Helper()
		routes := h.envRead(t, env)["routes"].([]any)
		if len(routes) != 1 {
			t.Fatalf("%s's routes: %v", env, routes)
		}
		return routes[0].(map[string]any)["waiting"]
	}
	named := func(env string) bool {
		t.Helper()
		_, p := rawPolicy(t, base)
		return len(routesOf(p, env)) == 1
	}
	wait := func(reason string, secret any) map[string]any {
		return map[string]any{"reason": reason, "secret": secret}
	}
	check := func(what string, want any) {
		t.Helper()
		if got := waiting("prod"); !mapsEqualAny(got, want) || named("prod") != (want == nil) {
			t.Fatalf("%s: prod waits %v, want %v; the policy names it %t", what, got, want, named("prod"))
		}
	}
	check("via off", wait("host", nil))
	root := h.trusting(t, "prod")
	h.exec(t, "UPDATE org_preferences SET run_by = 'org'")
	check("run_by org", wait("host", nil))
	h.exec(t, "UPDATE org_preferences SET run_by = 'canyon'", "UPDATE deploys SET egress_roots = NULL")
	check("nothing recorded", wait("redeploy", nil))
	h.exec(t, "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('claims-portal', 'prod', 'SSL_CERT_DIR', 'runtime', 1, '', '', 'person', 'sub-olive'), ('claims-portal', 'prod', 'NODE_EXTRA_CA_CERTS', 'runtime', 1, '', '', 'person', 'sub-olive')")
	check("a trust secret", wait("secret", "NODE_EXTRA_CA_CERTS"))
	h.exec(t, "DELETE FROM secrets", "UPDATE deploys SET egress_roots = ARRAY['"+strings.Repeat("0", 64)+"'] WHERE env = 'prod'")
	check("another root trusted", wait("redeploy", nil))
	before := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+root+"'] WHERE env = 'prod'")
	if count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy") <= before {
		t.Fatal("a write of an env's trusted roots moved no seq")
	}
	check("the signing root trusted", nil)

	newer := strings.Repeat("e", 64) // a rotation's root, newer than the minted one
	h.exec(t, "INSERT INTO egress_ca_roots (fingerprint, cert, key_ref, sealed_key, sealed_value, created_at, not_after) VALUES ('"+newer+"', '', 1, '', '', now() + interval '1 second', now() + interval '5 years')")
	check("a rotation prod has not taken", nil)
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+root+"', '"+newer+"'] WHERE env = 'prod'")
	check("a rotation every env holding a route has taken", nil)
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue)); code != 204 {
		t.Fatalf("preview's route: %d %s", code, lastBody)
	}
	h.exec(t, "UPDATE deploys SET egress_roots = ARRAY['"+root+"'] WHERE env = 'preview'")
	if waiting("preview") != nil || waiting("prod") != nil || !named("preview") {
		t.Fatal("an env holding a route that lacks the new root keeps the old one signing, for every env that trusts it")
	}
	h.exec(t, "UPDATE egress_ca_roots SET not_after = now() - interval '1 second' WHERE fingerprint = '"+root+"'")
	if !mapsEqualAny(waiting("preview"), wait("redeploy", nil)) || waiting("prod") != nil || named("preview") || !named("prod") {
		t.Fatal("at the old root's end the new one signs: preview, lacking it, waits")
	}
}
