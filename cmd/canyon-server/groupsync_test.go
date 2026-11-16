package main

import (
	"net/http"
	"testing"

	"github.com/getcanyon-ai/canyon/vault"
)

// withoutGroupSync starts the next test servers without Canyon Enterprise's group sync, as a build without it is, until
// the test ends: in a build with it, under the licence the tests' servers hold.
func withoutGroupSync(t *testing.T) {
	t.Helper()
	prior := testEE
	testEE = func(keys *vault.Vault) eeHooks {
		h := prior(keys)
		h.groupSync = nil
		return h
	}
	t.Cleanup(func() { testEE = prior })
}

// THE CUT (design/ARCHITECTURE.md §8), group sync: a server without Canyon Enterprise's group sync, even under a
// licence naming it, adds no group grant, widens none and sets no other groups claim, 409 licence_required, writing
// nothing; a group grant set before keeps granting its level, and repeating it, narrowing it, removing it and clearing
// the claim are taken (contracts/routes.md, Licence, A server built without Canyon Enterprise; #2924).
func TestWithoutGroupSyncNoGroupGainsAccess(t *testing.T) {
	withoutGroupSync(t)
	h := outboundHost(t)
	h.exec(t, "UPDATE org_preferences SET groups_claim = 'groups'",
		`UPDATE packages SET roles = '{"approver": "Approves claims over 10,000 EUR", "auditor": "Reads every claim and changes none"}'`,
		"INSERT INTO project_access (project_id, kind, grantee, level, roles, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'group', 'claims-team', 'contributor', '{approver}', 'person', 'sub-olive', '')")
	gina := signedIn(t, h.ctx, h.pool, h.base, "sub-gina", "gina@meridian.example", false)
	carrying(t, h.ctx, h.pool, "sub-gina", "claims-team")
	if code, _, _ := do(t, gina, http.MethodGet, h.base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Errorf("gina, a contributor through a group grant set before, reads the status: %d %s, want 200", code, lastBody)
	}
	held := func() (grants, claim string) {
		t.Helper()
		if err := h.pool.QueryRow(h.ctx, `SELECT coalesce((SELECT string_agg(grantee || ' ' || level || ' [' || array_to_string(roles, ',') || ']', '; ' ORDER BY grantee)
			FROM project_access WHERE kind = 'group'), ''), groups_claim FROM org_preferences`).Scan(&grants, &claim); err != nil {
			t.Fatal(err)
		}
		return grants, claim
	}
	group := "/api/v1/projects/claims-portal/access/group/"
	for _, c := range []struct {
		what         string
		who          *http.Client
		method, path string
		body         string
	}{
		{"a new group grant", h.olive, http.MethodPut, group + "readers", `{"level":"viewer"}`},
		{"a higher level for the group", h.olive, http.MethodPut, group + "claims-team", `{"level":"owner","roles":["approver"]}`},
		{"a role the group doesn't hold", h.olive, http.MethodPut, group + "claims-team", `{"level":"contributor","roles":["approver","auditor"]}`},
		{"a lower level with a role the group doesn't hold", h.olive, http.MethodPut, group + "claims-team", `{"level":"viewer","roles":["auditor"]}`},
		{"an admin's new group grant", h.dana, http.MethodPut, group + "admins", `{"level":"owner"}`},
		{"another groups claim", h.dana, http.MethodPut, preferencesPath, `{"groups_claim":"roles"}`},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 409 || lastBody != groupsRequired {
			t.Errorf("%s: %d %s, want 409 %s", c.what, code, lastBody, groupsRequired)
		}
		if grants, claim := held(); grants != "claims-team contributor [approver]" || claim != "groups" {
			t.Errorf("%s wrote group grants %q, groups claim %q, want nothing written", c.what, grants, claim)
		}
	}
	for _, c := range []struct {
		what         string
		who          *http.Client
		method, path string
		body, grants string
	}{
		{"the group grant as it stands", h.olive, http.MethodPut, group + "claims-team", `{"level":"contributor","roles":["approver"]}`, "claims-team contributor [approver]"},
		{"the groups claim as it stands", h.dana, http.MethodPut, preferencesPath, `{"groups_claim":"groups"}`, "claims-team contributor [approver]"},
		{"the group without a role", h.olive, http.MethodPut, group + "claims-team", `{"level":"contributor"}`, "claims-team contributor []"},
		{"the group at a lower level", h.olive, http.MethodPut, group + "claims-team", `{"level":"viewer"}`, "claims-team viewer []"},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 200 {
			t.Errorf("%s: %d %s, want 200", c.what, code, lastBody)
		}
		if grants, claim := held(); grants != c.grants || claim != "groups" {
			t.Errorf("%s: group grants %q, groups claim %q, want %q and groups", c.what, grants, claim, c.grants)
		}
	}
	if code, _, _ := do(t, h.olive, http.MethodDelete, h.base+group+"claims-team", ""); code != 204 {
		t.Errorf("removing the group grant: %d %s, want 204", code, lastBody)
	}
	if code, _ := putPreferences(t, h.dana, h.base, `{"groups_claim":""}`); code != 200 {
		t.Errorf("clearing the groups claim: %d %s, want 200", code, lastBody)
	}
	if grants, claim := held(); grants != "" || claim != "" {
		t.Errorf("removed and cleared: group grants %q, groups claim %q, want none", grants, claim)
	}
}
