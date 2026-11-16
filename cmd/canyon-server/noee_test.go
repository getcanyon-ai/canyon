//go:build noee

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A server built without Canyon Enterprise has no licence (contracts/routes.md, Licence, A server built without
// Canyon Enterprise): every member reads none, and an admin's PUT is 409 enterprise_absent after the 403s every
// other caller gets, nothing stored.
func TestWithoutEnterpriseThereIsNoLicence(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if h, err := wireEnterprise(func(string) string { return "" }, nil); err != nil || h != (eeHooks{}) {
		t.Fatalf("a build without Canyon Enterprise wired %+v (%v), want no hook", h, err)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	agent := signedIn(t, ctx, pool, base, "sub-dana-agent", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-dana-agent'"); err != nil {
		t.Fatal(err)
	}
	at := base + "/api/v1/licence"
	for who, c := range map[string]*http.Client{"the admin": dana, "a member": carol} {
		if code, _, _ := do(t, c, http.MethodGet, at, ""); code != 200 || lastBody != `{"state":"none","allows":[]}` {
			t.Errorf("%s reads the licence: %d %s, want none", who, code, lastBody)
		}
	}
	raw, _ := json.Marshal(map[string]string{"licence": `{"payload":"e30=","signature":"AA=="}`})
	for _, c := range []struct {
		who  string
		c    *http.Client
		code int
		want string
	}{
		{"a member", carol, 403, adminRequired},
		{"an agent under the admin", agent, 403, `{"error":"person_required","message":"A person sets Canyon's licence, in the dashboard: Settings › Licence."}`},
		{"the admin's CLI session", danaCLI, 403, dashboardRequired},
		{"the admin", dana, 409, `{"error":"enterprise_absent","message":"This Canyon is built without Canyon Enterprise, so it can't use a licence."}`},
	} {
		if code, _, _ := do(t, c.c, http.MethodPut, at, string(raw)); code != c.code || lastBody != c.want {
			t.Errorf("%s sets a licence: %d %s, want %d %s", c.who, code, lastBody, c.code, c.want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM licence"); n != 0 {
		t.Errorf("%d licences stored", n)
	}
}

// A server built without Canyon Enterprise answers each Enterprise change as a server with no licence does, 409
// licence_required with the same body, while what is set stays readable and removable (contracts/routes.md, Licence).
func TestWithoutEnterpriseEveryChangeIsUnlicensed(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, "UPDATE org_preferences SET patching = 'off', groups_claim = 'groups'", "UPDATE egress_settings SET credential_routes = false",
		"INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'group', 'claims-team', 'viewer', 'person', 'sub-olive', '')")
	for _, c := range []struct {
		what         string
		who          *http.Client
		method, path string
		body, want   string
	}{
		{"a credential", h.carl, http.MethodPut, routePath("preview", "api.stripe.com"), routeBody("Authorization", "bearer", routeValue), outboundKeysRequired},
		{"credentials turned on", h.dana, http.MethodPut, "/api/v1/egress", `{"credential_routes":true}`, outboundKeysRequired},
		{"another groups claim", h.dana, http.MethodPut, preferencesPath, `{"groups_claim":"roles"}`, groupsRequired},
		{"a group grant", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/readers", `{"level":"viewer"}`, groupsRequired},
		{"patching turned on", h.dana, http.MethodPut, preferencesPath, `{"patching":"ask"}`, patchingRequired},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 409 || lastBody != c.want {
			t.Errorf("%s: %d %s, want 409 %s", c.what, code, lastBody, c.want)
		}
	}
	if code, _, _ := do(t, h.olive, http.MethodDelete, h.base+"/api/v1/projects/claims-portal/access/group/claims-team", ""); code != 204 {
		t.Errorf("removing a group grant set before: %d %s, want 204", code, lastBody)
	}
	if code, _ := putPreferences(t, h.dana, h.base, `{"groups_claim":""}`); code != 200 {
		t.Errorf("clearing the groups claim set before: %d %s, want 200", code, lastBody)
	}
	h.exec(t, "UPDATE egress_settings SET credential_routes = true") // as a server with Canyon Enterprise left it
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"credential_routes":false}`); code != 200 {
		t.Errorf("turning credentials off: %d %s, want 200", code, lastBody)
	}
}
