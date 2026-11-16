package main

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// carrying makes the signed-in subject's sessions carry groups, as a sign-in with the claim stores them.
func carrying(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sub string, groups ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE sessions SET groups = $2 WHERE actor_subject = $1", sub, groups); err != nil {
		t.Fatal(err)
	}
}

// A group grant is the level of a member whose session carries its group; the highest of a person's
// grants wins; a group at owner manages the app (contracts/routes.md, App teams).
func TestAGroupGrantIsTheLevelOfAMemberWhoseSessionCarriesItsGroup(t *testing.T) {
	pool, ctx, base, _, _, vic, nora, _ := teamOf(t) // vic holds an email grant at viewer, nora none
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		('claims-portal', 'group', 'claims-team', 'contributor', 'person', 'sub-olive', ''), ('claims-portal', 'group', 'readers', 'viewer', 'person', 'sub-olive', ''),
		('claims-portal', 'group', 'platform', 'owner', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	gina := signedIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example", false)
	rita := signedIn(t, ctx, pool, base, "sub-rita", "rita@meridian.example", false)
	pat := signedIn(t, ctx, pool, base, "sub-pat", "pat@meridian.example", false)
	carrying(t, ctx, pool, "sub-gina", "claims-team")
	carrying(t, ctx, pool, "sub-rita", "readers", "elsewhere")
	carrying(t, ctx, pool, "sub-vic", "claims-team")
	carrying(t, ctx, pool, "sub-nora", "claims-team-eu", "Claims-Team") // a group is its exact name
	carrying(t, ctx, pool, "sub-pat", "platform")
	for _, c := range []struct {
		who         string
		c           *http.Client
		status      int
		level       string
		deleteCode  int
		deleteError string
	}{
		{"gina, in claims-team", gina, 200, "", 403, "owner_required"},
		{"vic, a viewer by address and in claims-team", vic, 200, "", 403, "owner_required"},
		{"rita, in readers", rita, 403, "viewer", 403, "owner_required"},
		{"nora, in no group granted", nora, 403, "none", 403, "owner_required"},
		{"pat, in platform", pat, 200, "", 409, "confirm_required"},
	} {
		if code, _, _ := do(t, c.c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != c.status {
			t.Errorf("%s reads the status: %d %s, want %d", c.who, code, lastBody, c.status)
		}
		if c.level != "" {
			envs := map[string]string{"viewer": `,"envs":{}`}[c.level] // a viewer's row carries where the app is live: nowhere, here
			if do(t, c.c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); !strings.HasSuffix(lastBody, `"access":{"level":"`+c.level+`"}`+envs+`}}`) {
				t.Errorf("%s, the reduced read: %s, want level %s", c.who, lastBody, c.level)
			}
		}
		if code, body, _ := do(t, c.c, http.MethodDelete, base+"/api/v1/projects/claims-portal", ""); code != c.deleteCode || body["error"] != c.deleteError {
			t.Errorf("%s deletes: %d %s, want %d %s", c.who, code, lastBody, c.deleteCode, c.deleteError)
		}
	}
	if _, page, _ := do(t, gina, http.MethodGet, base+"/api/v1/audit", ""); len(page["events"].([]any)) == 0 || !strings.Contains(lastBody, `"project":"claims-portal"`) {
		t.Errorf("gina's trail carries her group's app: %s", lastBody)
	}
}

// signInWith is one browser sign-in as dana with the ID token bent by bend: the session it minted.
func signInWith(t *testing.T, base string, idp *testIDP, bend map[string]any) string {
	t.Helper()
	idp.bend = bend
	c := browser(t)
	callback, _, _, _ := startSignIn(t, c, base, idp)
	for _, ck := range fetch(t, c, callback).Cookies() {
		if ck.Name == sessionCookie {
			return ck.Value
		}
	}
	t.Fatalf("no session: %s", lastBody)
	return ""
}

// A sign-in stores the groups claim's array, and none — with one log line — past 200 entries, when the
// claim is absent (Entra's overage) and when it is not an array of strings; none, silently, when no
// claim is set.
func TestASignInStoresTheGroupsItsClaimCarries(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	withIssuer(t, idp, "")
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	many := make([]any, 201)
	for i := range many {
		many[i] = "g" + strconv.Itoa(i)
	}
	groupsOfSession := func(id string) []string {
		var groups []string
		if err := pool.QueryRow(ctx, "SELECT groups FROM sessions WHERE id = $1", id).Scan(&groups); err != nil {
			t.Fatal(err)
		}
		return groups
	}
	unset := signInWith(t, base, idp, map[string]any{"groups": []any{"claims-team"}})
	if got := groupsOfSession(unset); len(got) != 0 || strings.Contains(logs.String(), "groups claim") {
		t.Fatalf("with no claim set: %v\n%s", got, logs.String())
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET groups_claim = 'roles'"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what string
		bend map[string]any
		want []string
		log  string
	}{
		{"the claim's array", map[string]any{"roles": []any{"claims-team", "platform"}}, []string{"claims-team", "platform"}, ""},
		{"201 groups", map[string]any{"roles": many}, []string{}, "groups claim roles is over-200: no groups stored"},
		{"no claim at all", map[string]any{"_claim_names": map[string]any{"roles": "src1"}, "_claim_sources": map[string]any{"src1": map[string]any{"endpoint": "https://graph.example/v1.0/users/u1/getMemberObjects"}}}, []string{}, "groups claim roles is absent: no groups stored"},
		{"a string", map[string]any{"roles": "claims-team"}, []string{}, "groups claim roles is not-a-list: no groups stored"},
		{"a number among the strings", map[string]any{"roles": []any{"claims-team", 7}}, []string{}, "groups claim roles is not-a-list: no groups stored"},
	} {
		before := strings.Count(logs.String(), "groups claim")
		got := groupsOfSession(signInWith(t, base, idp, c.bend))
		if lines := strings.Count(logs.String(), "groups claim") - before; !reflect.DeepEqual(got, c.want) || c.log == "" && lines != 0 || c.log != "" && (lines != 1 || !strings.Contains(logs.String(), c.log)) {
			t.Errorf("%s: stored %v, want %v; the log line %q:\n%s", c.what, got, c.want, c.log, logs.String())
		}
	}
}

// A device session is its approver: it carries the groups their browser sign-in stored.
func TestADeviceSessionCarriesTheApproversGroups(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	device, user, _ := mint(t, base)
	approver := signedIn(t, ctx, pool, base, "sub-gina", "gina@meridian.example", false)
	carrying(t, ctx, pool, "sub-gina", "claims-team", "platform")
	if code, _, _ := do(t, approver, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	code, body := poll(t, base, device)
	var groups []string
	if err := pool.QueryRow(ctx, "SELECT groups FROM sessions WHERE id = $1 AND flow = 'device'", body["session"]).Scan(&groups); code != 200 || err != nil || !reflect.DeepEqual(groups, []string{"claims-team", "platform"}) {
		t.Fatalf("the device session: %d %v %v", code, groups, err)
	}
}
