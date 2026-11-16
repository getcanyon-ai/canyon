//go:build !noee

package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
)

// THE FAIL-CLOSED RULE (#2879), group sync: a group grant made under a licence still grants its level once it has
// lapsed, and the groups claim stays as set; narrowing the grant, removing it and clearing the claim stay free (the
// lead, #2881's review), while another claim, a new group grant, or one widened in level or roles is 409
// licence_required.
func TestALapsedLicenceKeepsGroupGrants(t *testing.T) {
	h := outboundHost(t)
	if code, _ := putPreferences(t, h.dana, h.base, `{"groups_claim":"groups"}`); code != 200 {
		t.Fatalf("the groups claim under the licence: %d %s", code, lastBody)
	}
	h.exec(t, `UPDATE packages SET roles = '{"approver": "Approves claims over 10,000 EUR", "auditor": "Reads every claim and changes none"}'`)
	grant := h.base + "/api/v1/projects/claims-portal/access/group/claims-team"
	if code, _, _ := do(t, h.olive, http.MethodPut, grant, `{"level":"contributor","roles":["approver"]}`); code != 200 {
		t.Fatalf("a group grant under the licence: %d %s", code, lastBody)
	}
	gina := signedIn(t, h.ctx, h.pool, h.base, "sub-gina", "gina@meridian.example", false)
	carrying(t, h.ctx, h.pool, "sub-gina", "groups-of-gina", "claims-team")

	licenceAt(t, h.base, testExpiry.Add(licence.Grace+time.Hour)) // lapsed: an hour past grace

	if code, _, _ := do(t, gina, http.MethodGet, h.base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Errorf("lapsed: gina, a contributor through her group, reads the status: %d %s, want 200", code, lastBody)
	}
	if prefs := getPreferences(t, h.dana, h.base); prefs["groups_claim"] != "groups" {
		t.Errorf("lapsed: groups claim %v, want as set", prefs["groups_claim"])
	}
	for _, c := range []struct {
		what         string
		who          *http.Client
		method, path string
		body         string
	}{
		{"another groups claim", h.dana, http.MethodPut, preferencesPath, `{"groups_claim":"roles"}`},
		{"a new group grant", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/readers", `{"level":"viewer"}`},
		{"a higher level for the group", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"owner","roles":["approver"]}`},
		{"a role the group doesn't hold", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"contributor","roles":["approver","auditor"]}`},
		{"a lower level with a role the group doesn't hold", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"viewer","roles":["auditor"]}`},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 409 || lastBody != groupsRequired {
			t.Errorf("lapsed, %s: %d %s, want 409 %s", c.what, code, lastBody, groupsRequired)
		}
	}
	for _, c := range []struct {
		what         string
		who          *http.Client
		method, path string
		body         string
	}{
		{"the group grant as it stands", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"contributor","roles":["approver"]}`},
		{"the group without a role", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"contributor"}`},
		{"the group at a lower level", h.olive, http.MethodPut, "/api/v1/projects/claims-portal/access/group/claims-team", `{"level":"viewer"}`},
		{"the groups claim as it stands", h.dana, http.MethodPut, preferencesPath, `{"groups_claim":"groups"}`},
	} {
		if code, _, _ := do(t, c.who, c.method, h.base+c.path, c.body); code != 200 {
			t.Errorf("lapsed, %s: %d %s, want 200", c.what, code, lastBody)
		}
	}
	if code, _, _ := do(t, h.olive, http.MethodDelete, grant, ""); code != 204 {
		t.Errorf("lapsed, removing the group grant: %d %s, want 204", code, lastBody)
	}
	if code, _ := putPreferences(t, h.dana, h.base, `{"groups_claim":""}`); code != 200 {
		t.Errorf("lapsed, clearing the groups claim: %d %s, want 200", code, lastBody)
	}
}

// A licence in grace still unlocks what it names: a groups claim is set.
func TestAGroupsClaimIsSetInGrace(t *testing.T) {
	withLicenceEnv(t, signedLicence(testExpiry, "group_sync"))
	h := outboundHost(t)
	licenceAt(t, h.base, testExpiry.Add(time.Hour)) // grace: still in force
	if code, _ := putPreferences(t, h.dana, h.base, `{"groups_claim":"groups"}`); code != 200 {
		t.Errorf("in grace, the groups claim: %d %s, want 200", code, lastBody)
	}
}

// Moved from routes_access_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// The highest grant wins: a member whose group holds contributor, on an app shared with everyone, reads
// it whole as a contributor and reaches its contributor routes.
func TestAGroupContributorOnAnAppSharedWithEveryoneIsAContributor(t *testing.T) {
	pool, ctx, base, _, _, _, nora, dana := teamOf(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET groups_claim = 'groups'"); err != nil {
		t.Fatal(err)
	}
	for grant, level := range map[string]string{"/everyone/everyone": "viewer", "/group/claims-engineering": "contributor"} {
		if code, _, _ := do(t, dana, http.MethodPut, base+accessAt+grant, `{"level":"`+level+`"}`); code != 200 {
			t.Fatalf("share %s: %d %s", grant, code, lastBody)
		}
	}
	carrying(t, ctx, pool, "sub-nora", "claims-engineering")
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); code != 200 || !strings.Contains(lastBody, `"access":{"level":"contributor","everyone":true,"people":2,"groups":1},"stack"`) {
		t.Fatalf("the read: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", ""); code != 200 {
		t.Fatalf("a contributor through their group reads the packages: %d %s", code, lastBody)
	}
}

// Moved from routes_access_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// A share writes the grant whole and audits its before and after; the same share again writes nothing;
// an email grant takes the name and subject of that address's request and ends it; everyone is read-only
// and has its own place on the read; a group is written once the host reads groups.
func TestShareWritesTheGrantWholeAndAuditsTheChange(t *testing.T) {
	pool, ctx, base, olive, _, _, nora, dana := teamOf(t)
	if code, _, _ := do(t, nora, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
		t.Fatal(code)
	}
	for range 2 {
		if code, body, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/Nora@Meridian.example", `{"level":"viewer","roles":["auditor","auditor"]}`); code != 200 || len(body["requests"].([]any)) != 0 {
			t.Fatalf("share: %d %s", code, lastBody)
		}
	}
	var subject, display string
	var roles []string
	if err := pool.QueryRow(ctx, "SELECT subject, display, roles FROM project_access WHERE grantee = 'nora@meridian.example'").Scan(&subject, &display, &roles); err != nil ||
		subject != "sub-nora" || display != "sub-nora" || !reflect.DeepEqual(roles, []string{"auditor"}) {
		t.Fatalf("nora's grant: %q %q %v %v", subject, display, roles, err)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 403 {
		t.Fatalf("a viewer reads the status: %d", code)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+accessAt+"/email/nora@meridian.example", `{"level":"contributor"}`); code != 200 {
		t.Fatalf("an admin raises nora: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Fatalf("a contributor reads the status: %d", code)
	}
	if code, body, _ := do(t, olive, http.MethodPut, base+accessAt+"/everyone/everyone", `{"level":"viewer","roles":["approver"]}`); code != 200 || fmt.Sprint(body["everyone"]) != "map[roles:[approver]]" {
		t.Fatalf("share with everyone: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET groups_claim = 'groups'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/group/claims%2Feu", `{"level":"owner","roles":[]}`); code != 200 || !strings.Contains(lastBody, `{"kind":"group","grantee":"claims/eu","display":"claims/eu","level":"owner"`) {
		t.Fatalf("share with a group: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/olive@meridian.example", `{"level":"contributor"}`); code != 200 {
		t.Fatalf("an owner steps down beside a group at owner: %d %s", code, lastBody)
	}
	sameTrail(t, accessTrail(t, ctx, pool), trailRow("sub-nora", "request access", `{"email":"nora@meridian.example"}`),
		trailRow("sub-olive", "share app", `{"kind":"email","grantee":"nora@meridian.example","level":{"before":null,"after":"viewer"},"roles":{"before":null,"after":["auditor"]}}`),
		trailRow("sub-dana", "share app", `{"kind":"email","grantee":"nora@meridian.example","level":{"before":"viewer","after":"contributor"},"roles":{"before":["auditor"],"after":[]}}`),
		trailRow("sub-olive", "share app", `{"kind":"everyone","grantee":"everyone","level":{"before":null,"after":"viewer"},"roles":{"before":null,"after":["approver"]}}`),
		trailRow("sub-olive", "share app", `{"kind":"group","grantee":"claims/eu","level":{"before":null,"after":"owner"},"roles":{"before":null,"after":[]}}`),
		trailRow("sub-olive", "share app", `{"kind":"email","grantee":"olive@meridian.example","level":{"before":"owner","after":"contributor"},"roles":{"before":[],"after":[]}}`))
}

// Moved from groups_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// groups_claim is a preference an admin sets and clears, audited; CANYON_GROUPS_CLAIM overrides it, is
// named in overridden, refuses the PUT, and is the claim a group share is judged by.
func TestTheGroupsClaimIsAPreferenceTheEnvironmentOverrides(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, step := range []struct{ body, want string }{{`{"groups_claim":"groups"}`, "groups"}, {`{"groups_claim":"groups"}`, "groups"}, {`{"groups_claim":""}`, ""}} {
		if code, body := putPreferences(t, c, base, step.body); code != 200 || body["groups_claim"] != step.want {
			t.Fatalf("PUT %s: %d %s", step.body, code, lastBody)
		}
	}
	var details string
	if err := pool.QueryRow(ctx, "SELECT string_agg(detail::text, ' ' ORDER BY ts) FROM audit_events WHERE action = 'PUT /api/v1/preferences'").Scan(&details); err != nil ||
		details != `{"groups_claim": {"after": "groups", "before": ""}} {"groups_claim": {"after": "", "before": "groups"}}` {
		t.Fatalf("the audit rows, the repeat writing none: %s %v", details, err)
	}
	for _, bad := range []string{`{"groups_claim":7}`, `{"groups_claim":null}`, `{"groups_claim":"a\tb"}`, `{"groups_claim":"` + strings.Repeat("g", 257) + `"}`} {
		if code, body := putPreferences(t, c, base, bad); code != 400 || body["error"] != "invalid_body" {
			t.Errorf("PUT %.40s: %d %s", bad, code, lastBody)
		}
	}
	if _, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": "https://login.example.com", "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_ADMINS": "dana@meridian.example", "CANYON_GROUPS_CLAIM": "a\tb"})); err == nil || err.Error() != "CANYON_GROUPS_CLAIM is a claim name of 1 to 256 characters of plain text" {
		t.Errorf("a control character in CANYON_GROUPS_CLAIM boots: %v", err)
	}

	env := map[string]string{"CANYON_GROUPS_CLAIM": "roles"}
	for k, v := range goodOIDC {
		env[k] = v
	}
	cfg, err := oidcConfigOf(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	testAuth, testOIDC = authOIDC, cfg
	t.Cleanup(func() { testAuth, testOIDC = authOff, nil })
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if prefs := getPreferences(t, dana, base); prefs["groups_claim"] != "roles" || !strings.Contains(lastBody, `"overridden":["allowed_domains","groups_claim"]`) {
		t.Fatalf("the read under CANYON_GROUPS_CLAIM: %s", lastBody)
	}
	if code, _ := putPreferences(t, dana, base, `{"groups_claim":"groups"}`); code != 409 || lastBody != `{"error":"set_by_operator","keys":["groups_claim"],"message":"the environment sets these keys, so a PUT cannot"}` {
		t.Fatalf("a PUT of the pinned claim: %d %s", code, lastBody)
	}
	seedTeamApp(t, ctx, pool, base, dana)
	if code, _, _ := do(t, dana, http.MethodPut, base+accessAt+"/group/claims-team", `{"level":"viewer"}`); code != 200 {
		t.Fatalf("a group share with the claim set by the environment: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'share app'"); n != 1 {
		t.Fatalf("share rows: %d", n)
	}
}

// Moved from routes_access_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// Every project read answers the caller's own level and, from contributor, who the app is shared with;
// the count of requests is its owners' and admins' alone, and below contributor the row says only the
// level. An everyone grant makes every member a viewer: they read it reduced, and are on the team.
func TestTheProjectReadsSayWhoTheAppIsSharedWith(t *testing.T) {
	pool, _, base, olive, carl, vic, nora, dana := teamOf(t)
	sees := func(who string, c *http.Client, want string) {
		t.Helper()
		for _, path := range []string{"/api/v1/projects/claims-portal", "/api/v1/projects", "/api/v1/fleet"} {
			if code, _, _ := do(t, c, http.MethodGet, base+path, ""); code != 200 || !strings.Contains(lastBody, `"access":`+want) {
				t.Errorf("%s, %s: %d %s\nwant access %s", who, path, code, lastBody, want)
			}
		}
	}
	if code, _, _ := do(t, nora, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
		t.Fatal(code)
	}
	sees("the owner of a private app", olive, `{"level":"owner","everyone":false,"people":2,"groups":0,"requests":1}`)
	if _, err := pool.Exec(context.Background(), "UPDATE org_preferences SET groups_claim = 'groups'"); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []string{"/everyone/everyone", "/group/claims-engineering"} {
		if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+grant, `{"level":"viewer"}`); code != 200 {
			t.Fatalf("share %s: %d %s", grant, code, lastBody)
		}
	}
	sees("the owner", olive, `{"level":"owner","everyone":true,"people":2,"groups":1,"requests":1}`)
	sees("a contributor", carl, `{"level":"contributor","everyone":true,"people":2,"groups":1}`)
	sees("an admin, a member", dana, `{"level":"viewer","everyone":true,"people":2,"groups":1,"requests":1}`)
	sees("a viewer", vic, `{"level":"viewer"},"envs":{}}`)
	sees("a member, through everyone", nora, `{"level":"viewer"},"envs":{}}`)
	if code, _, _ := do(t, carl, http.MethodPost, base+accessAt+"-requests", ""); code != 409 { // a viewer may ask to change it (#1706); a contributor can already
		t.Errorf("a member who can change the app asks: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", ""); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Errorf("a viewer through everyone reads the packages: %d %s", code, lastBody)
	}
}
