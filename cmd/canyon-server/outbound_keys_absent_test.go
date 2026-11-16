package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/vault"
)

// withoutOutboundKeys starts the next test servers as a build without Canyon Enterprise answers for outbound keys,
// with its outbound hook nil, until the test ends; in a build without it, every server is one already.
func withoutOutboundKeys(t *testing.T) {
	t.Helper()
	prior := testEE
	testEE = func(keys *vault.Vault) eeHooks {
		h := prior(keys)
		h.outbound = nil
		return h
	}
	t.Cleanup(func() { testEE = prior })
}

// contracts/routes.md, Licence, A server built without Canyon Enterprise, outbound keys (#2922): a
// credential an Enterprise server set — a route and credentials turned on, written here as that server wrote them —
// is listed, waiting for the host, removable, and named by no policy; no change to it, or to the interception root,
// is written: a new credential, a value replaced, a secret moved in, credentials turned on and a rotation are each
// 409 licence_required, nothing written; credentials turn off. No route answers a value or a leaf, and the admin's
// read names no root.
func TestWithoutEnterpriseOutboundKeysChangeNothingAndAddNoCredential(t *testing.T) {
	withoutOutboundKeys(t)
	h := outboundHost(t)
	keys := vaultOf(t, h.base)
	root := h.trusting(t, "prod")
	version, sealedKey, sealedValue, err := keys.Seal("claims-portal", "prod", "route:api.stripe.com", []byte(routeValue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO egress_routes (project_id, env, destination, header, scheme, placeholder_env, from_secret, rev, key_ref, sealed_key, sealed_value)
		VALUES ('claims-portal', 'prod', 'api.stripe.com', 'authorization', 'bearer', 'STRIPE_SECRET_KEY', false, nextval('egress_route_rev'), $1, $2, $3)`, version, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	held := func() string {
		t.Helper()
		var row string
		if err := h.pool.QueryRow(h.ctx, "SELECT rev || ':' || encode(sealed_value, 'hex') FROM egress_routes WHERE project_id = 'claims-portal' AND env = 'prod' AND destination = 'api.stripe.com'").Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := held()
	if code, _, _ := do(t, h.olive, http.MethodPut, h.base+"/api/v1/projects/claims-portal/secrets/STRIPE_KEY", `{"value":"`+routeValue+`","env":"prod"}`); code != 204 {
		t.Fatalf("a runtime secret to move: %d %s", code, lastBody)
	}

	for _, c := range []struct {
		name         string
		who          *http.Client
		method, path string
		body         string
	}{
		{"a new credential", h.carl, http.MethodPut, routePath("prod", "api.openai.com"), routeBody("Authorization", "bearer", routeValue)},
		{"a held credential's value replaced", h.olive, http.MethodPut, routePath("prod", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue+"-rotated")},
		{"a secret moved into a credential", h.olive, http.MethodPost, routePath("prod", "api.openai.com") + "/from-secret", `{"secret":"STRIPE_KEY","header":"Authorization","scheme":"bearer"}`},
		{"the interception root rotated", h.dana, http.MethodPost, rotatePathCore, `{}`},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 409 || lastBody != outboundKeysRequired {
			t.Errorf("%s: %d %s, want 409 %s", c.name, code, lastBody, outboundKeysRequired)
		}
	}
	if after := held(); after != before || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 1 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 1 ||
		count(t, h.ctx, h.pool, "SELECT count(*) FROM secrets WHERE name = 'STRIPE_KEY'") != 1 {
		t.Fatalf("a refused change wrote: the route %s (was %s), the routes, the roots or the secret moved", after, before)
	}

	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":false}`); code != 200 {
		t.Fatalf("credentials turned off: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":true}`); code != 409 || lastBody != outboundKeysRequired {
		t.Fatalf("credentials turned back on: %d %s, want 409 %s", code, lastBody, outboundKeysRequired)
	}
	if count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_settings WHERE credential_routes") != 0 {
		t.Fatal("the refused turn on wrote credential_routes")
	}
	h.exec(t, "UPDATE egress_settings SET credential_routes = true") // as the Enterprise server left it

	if code, _, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 || !strings.Contains(lastBody, `"ca":{"signing":null,"roots":[],"rotates_at":null}`) {
		t.Fatalf("the admin's read, a root held: %d %s; want no root named", code, lastBody)
	}
	if code, _, _ := do(t, h.olive, http.MethodGet, h.base+outboundApp, ""); code != 200 ||
		!strings.Contains(lastBody, `"routes":[{"destination":"api.stripe.com","header":"authorization","scheme":"bearer","placeholder_env":"STRIPE_SECRET_KEY","from_secret":false,"waiting":{"reason":"host","secret":null}}]`) {
		t.Fatalf("the app's read: %d %s; want the credential listed, waiting for the host", code, lastBody)
	}

	egressWait = time.Second // the replica the policy is read as below never asks again: the removal waits on it no longer
	t.Cleanup(func() { egressWait = egressDefault })
	s, policy := policyAt(t, h.pool)
	s.ee = appOf(t, h.base).ee
	raw, p := rawPolicy(t, policy)
	if p.SigningRoot != nil || len(routesOf(p, "prod")) != 0 || strings.Contains(raw, root) {
		t.Fatalf("the policy, prod trusting the root that would sign: signing_root %v, prod's routes %v; want none", p.SigningRoot, routesOf(p, "prod"))
	}
	if code, body, _ := routeValueAt(t, policy, "app=claims-portal&env=prod&destination=api.stripe.com"); code != 404 || !strings.Contains(body, `"error":"no_route"`) || strings.Contains(body, routeValue) {
		t.Fatalf("a held route's value: %d %s, want 404 no_route", code, body)
	}
	if code, body := leafAtCore(t, policy, "destination=api.stripe.com"); code != 404 || !strings.Contains(body, `"error":"no_route"`) {
		t.Fatalf("a held route's leaf: %d %s, want 404 no_route", code, body)
	}

	if code, _, _ := do(t, h.carl, http.MethodDelete, h.base+routePath("prod", "api.stripe.com"), ""); code != 204 || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_routes") != 0 {
		t.Fatalf("the credential removed: %d %s", code, lastBody)
	}
}

// rotatePathCore is POST /api/v1/egress/ca/rotate.
const rotatePathCore = "/api/v1/egress/ca/rotate"

// leafAtCore is one GET /egress/leaf as replica canyon-egress-a: its status and its body.
func leafAtCore(t *testing.T, base, query string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/leaf?"+query, nil)
	req.Header.Set("X-Replica", "canyon-egress-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, string(body)
}
