package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/cli"
	"github.com/jackc/pgx/v5/pgxpool"
)

// accessRow is one row of the Access group's trail: who, what, and the detail as decoded JSON.
type accessRow struct {
	who, action string
	detail      any
}

func trailRow(who, action, detail string) accessRow {
	var d any
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		panic(err)
	}
	return accessRow{who, action, d}
}

// accessTrail is the Access group's audit rows on claims-portal, in the order they were written.
func accessTrail(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []accessRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT actor_subject, action, detail FROM audit_events WHERE project_id = 'claims-portal'
		AND action IN ('share app', 'unshare app', 'request access', 'dismiss access request') ORDER BY ts, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []accessRow{}
	for rows.Next() {
		var r accessRow
		var raw []byte
		if err := rows.Scan(&r.who, &r.action, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func sameTrail(t *testing.T, got []accessRow, want ...accessRow) {
	t.Helper()
	if len(want) == 0 {
		want = []accessRow{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the trail is\n  %v\nwant\n  %v", got, want)
	}
}

// teamOf is claims-portal's team as seedTeamApp leaves it (olive owns it, carl contributes, vic reads),
// plus nora with no access and dana the admin, each signed in.
func teamOf(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, base string, olive, carl, vic, nora, dana *http.Client) {
	t.Helper()
	withOIDC(t) // dana is the admin
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	olive = signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	if _, err := pool.Exec(ctx, `UPDATE packages SET roles = '{"approver": "Approves claims over 10,000 EUR", "auditor": "Reads every claim and changes none"}'`); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, olive, signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false),
		signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false),
		signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false),
		signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
}

const accessAt = "/api/v1/projects/claims-portal/access"

// Every route of the Access group but the request is its owners' and admins': each answers an owner and
// an admin with the handler, and refuses a contributor, a viewer and a member with no access
// owner_required, writing nothing. An unknown app is 404 before the refusal.
func TestOnlyOwnersAndAdminsManageAccess(t *testing.T) {
	pool, ctx, base, olive, carl, vic, nora, dana := teamOf(t)
	routes := []struct {
		method, path, body string
		passed             int
	}{
		{http.MethodGet, accessAt, "", 200},
		{http.MethodPut, accessAt + "/email/sam@meridian.example", `{"level":"admin"}`, 400},
		{http.MethodDelete, accessAt + "/email/sam@meridian.example", "", 404},
		{http.MethodDelete, accessAt + "-requests/sam@meridian.example", "", 404},
	}
	for _, c := range []struct {
		who    string
		c      *http.Client
		passes bool
	}{{"the owner", olive, true}, {"an admin", dana, true}, {"a contributor", carl, false}, {"a viewer", vic, false}, {"a member with no access", nora, false}} {
		for _, r := range routes {
			code, _, _ := do(t, c.c, r.method, base+r.path, r.body)
			if c.passes && code != r.passed || !c.passes && (code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive")) {
				t.Errorf("%s, %s %s: %d %s", c.who, r.method, r.path, code, lastBody)
			}
		}
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/nope/access", ""); code != 404 || lastBody != `{"error":"not_found","message":"nope isn't on this Canyon yet. Deploy it first: canyon deploy"}` {
		t.Errorf("an unknown app: %d %s, want the 404 before the refusal", code, lastBody)
	}
	sameTrail(t, accessTrail(t, ctx, pool))
}

// A read-only member asks to change the app (#1706): the request is the owners' to see, saying they are read-only now,
// and sharing the app at contributor answers it; then they are on the team and asking is refused. A member read-only
// through the org's grant alone asks as read-only too: the server resolves the level, never the dashboard (#2774).
func TestAReadOnlyMemberAsksToChangeTheApp(t *testing.T) {
	pool, ctx, base, olive, _, vic, nora, _ := teamOf(t)
	if code, _, _ := do(t, vic, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
		t.Fatalf("a viewer asks: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodGet, base+accessAt, ""); code != 200 || !strings.Contains(lastBody, `"requests":[{"email":"vic@meridian.example"`) || !strings.Contains(lastBody, `"level":"viewer"}]`) {
		t.Fatalf("the owner sees the viewer's request, read-only: %d %s", code, lastBody)
	}
	asked := "SELECT count(*) FROM audit_events WHERE action = 'request access' AND detail->>'email' = 'nora@meridian.example'"
	if code, _, _ := do(t, nora, http.MethodPost, base+accessAt+"-requests", ""); code != 201 || count(t, ctx, pool, asked) != 1 {
		t.Fatalf("a member with no access asks: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'everyone', '', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	// asking again once the org's grant made them read-only records the new level, with one more row (routes.md, S1)
	for range 2 {
		if code, _, _ := do(t, nora, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
			t.Fatalf("a member read-only through the org's grant asks: %d %s", code, lastBody)
		}
	}
	if level := count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'nora@meridian.example' AND level = 'viewer'"); level != 1 {
		t.Fatalf("the org's grant's member asked as %d read-only rows, want 1", level)
	}
	if n := count(t, ctx, pool, asked); n != 2 {
		t.Fatalf("%d request access rows for nora, want 2: one when she asked, one when her level changed", n)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/vic@meridian.example", `{"level":"contributor"}`); code != 200 ||
		!strings.Contains(lastBody, `"requests":[{"email":"nora@meridian.example"`) || strings.Count(lastBody, `"requested_at"`) != 1 { // vic's answered, nora's still pending
		t.Fatalf("sharing at contributor answers the request: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, vic, http.MethodPost, base+accessAt+"-requests", ""); code != 409 {
		t.Fatalf("a contributor asks: %d %s", code, lastBody)
	}
}

// A member with no access asks once and an owner sees it on the access read; asking again changes
// nothing; anyone who can already change the app, an admin too, is already on the team; a dismissal ends it.
func TestAMemberWithoutAccessAsksAndAnOwnerSeesItOrDismissesIt(t *testing.T) {
	pool, ctx, base, olive, carl, _, nora, dana := teamOf(t)
	for _, c := range []*http.Client{nora, nora} {
		if code, _, _ := do(t, c, http.MethodPost, base+accessAt+"-requests", ""); code != 201 || lastBody != "" {
			t.Fatalf("ask: %d %s", code, lastBody)
		}
	}
	for who, c := range map[string]*http.Client{"the owner": olive, "a contributor": carl, "an admin": dana} {
		if code, _, _ := do(t, c, http.MethodPost, base+accessAt+"-requests", ""); code != 409 || lastBody != `{"error":"already_on_team","message":"you can already change this app"}` {
			t.Errorf("%s asks: %d %s", who, code, lastBody)
		}
	}
	if code, _, _ := do(t, nora, http.MethodPost, base+"/api/v1/projects/nope/access-requests", ""); code != 404 {
		t.Errorf("asking for an unknown app: %d %s", code, lastBody)
	}
	code, body := uploadPulled(t, olive, base, "claims-portal", tarOf(t, map[string]string{"b": "2"}))
	if code != 201 {
		t.Fatal(code)
	}
	if _, err := pool.Exec(ctx, `UPDATE packages SET roles = '{"auditor": "Reads every claim"}' WHERE id = $1`, packageOf(body)["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE project_access SET granted_at = '2026-09-01T10:00:00Z'; UPDATE access_requests SET requested_at = '2026-09-02T08:00:00.9Z'"); err != nil {
		t.Fatal(err)
	}
	byOlive := `{"kind":"person","subject":"sub-olive","display":"sub-olive"}`
	seeded := `{"kind":"person","subject":"sub-olive","display":""}`
	want := `{"everyone":null,"entries":[` +
		`{"kind":"email","grantee":"carl@meridian.example","display":"carl@meridian.example","level":"contributor","roles":[],"granted_by":` + seeded + `,"granted_at":"2026-09-01T10:00:00Z"},` +
		`{"kind":"email","grantee":"olive@meridian.example","display":"sub-olive","level":"owner","roles":[],"granted_by":` + byOlive + `,"granted_at":"2026-09-01T10:00:00Z"},` +
		`{"kind":"email","grantee":"vic@meridian.example","display":"vic@meridian.example","level":"viewer","roles":[],"granted_by":` + seeded + `,"granted_at":"2026-09-01T10:00:00Z"}],` +
		`"roles":[{"name":"approver","description":"Approves claims over 10,000 EUR"},{"name":"auditor","description":"Reads every claim"}],` +
		`"requests":[{"email":"nora@meridian.example","display":"sub-nora","requested_at":"2026-09-02T08:00:00Z","level":"none"}]}`
	if code, _, _ := do(t, olive, http.MethodGet, base+accessAt, ""); code != 200 || lastBody != want {
		t.Fatalf("the access read: %d\n%s\nwant\n%s", code, lastBody, want)
	}
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"-requests/Nora@meridian.example", ""); code != 204 {
		t.Fatalf("dismiss: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"-requests/nora@meridian.example", ""); code != 404 || lastBody != `{"error":"not_found","message":"no such request"}` {
		t.Fatalf("dismiss again: %d %s", code, lastBody)
	}
	sameTrail(t, accessTrail(t, ctx, pool), trailRow("sub-nora", "request access", `{"email":"nora@meridian.example"}`),
		trailRow("sub-olive", "dismiss access request", `{"email":"nora@meridian.example"}`))
}

// The audit read answers an app's access request rows, who asked and when, to its owners and admins
// alone (design/PERMISSIONS.md, Access requests; #2500): each of them and a contributor read the app's
// other rows, the contributor none of these, its total counting none of them, though he owns another app
// and reads its requests; a viewer and a member with no access are refused the app's trail, and read
// none of its rows, their own requests among them.
func TestTheAuditReadShowsAccessRequestsToOwnersAndAdminsAlone(t *testing.T) {
	_, _, base, olive, carl, vic, nora, dana := teamOf(t)
	for _, c := range []*http.Client{nora, vic} {
		if code, _, _ := do(t, c, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
			t.Fatalf("ask: %d %s", code, lastBody)
		}
	}
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"-requests/nora@meridian.example", ""); code != 204 {
		t.Fatalf("dismiss: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodPost, base+"/api/v1/projects", `{"id":"zed"}`); code != 201 {
		t.Fatalf("carl creates zed, its owner: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nora, http.MethodPost, base+"/api/v1/projects/zed/access-requests", ""); code != 201 {
		t.Fatalf("ask for zed: %d %s", code, lastBody)
	}
	requestsOf := func(page map[string]any) (rows []string) {
		events, _ := page["events"].([]any)
		for _, e := range events {
			row := e.(map[string]any)
			if a := row["action"].(string); a == "request access" || a == "dismiss access request" {
				rows = append(rows, a+" "+row["detail"].(map[string]any)["email"].(string))
			}
		}
		slices.Sort(rows)
		return rows
	}
	asked := "dismiss access request nora@meridian.example,request access nora@meridian.example,request access vic@meridian.example"
	for _, c := range []struct {
		who  string
		c    *http.Client
		want string
	}{{"the owner", olive, asked}, {"an admin", dana, asked}, {"a contributor", carl, ""}} {
		for i, path := range []string{"/api/v1/audit?project=claims-portal&action=request+access,dismiss+access+request", "/api/v1/audit?project=claims-portal&limit=200"} {
			code, page, _ := do(t, c.c, http.MethodGet, base+path, "")
			events, _ := page["events"].([]any)
			if got := strings.Join(requestsOf(page), ","); code != 200 || got != c.want || page["total"] != float64(len(events)) || i == 1 && len(events) == len(requestsOf(page)) {
				t.Errorf("%s reads %s: %d, the request rows %q (want %q), total %v over %d rows", c.who, path, code, got, c.want, page["total"], len(events))
			}
		}
	}
	if code, page, _ := do(t, carl, http.MethodGet, base+"/api/v1/audit?project=zed&action=request+access", ""); code != 200 || strings.Join(requestsOf(page), ",") != "request access nora@meridian.example" {
		t.Errorf("carl reads the requests of zed, which he owns: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"a viewer": vic, "a member with no access": nora} {
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?project=claims-portal", ""); code != 403 || lastBody != teamRequiredTo(c == vic) {
			t.Errorf("%s reads the app's trail: %d %s", who, code, lastBody)
		}
		if code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?limit=200", ""); code != 200 || projectsOf(page, "events") != "" {
			t.Errorf("%s reads the trail: %d, the rows' projects %q", who, code, projectsOf(page, "events"))
		}
	}
}

// The audit read answers an app's grants and refusals — its share app and unshare app rows, who holds
// what on it, and the denied rows naming it, who was turned away — to its owners and admins alone
// (design/PERMISSIONS.md, The org's own records; #2505): each of them reads them, a contributor none,
// its total counting none of them, though he owns another app and reads that app's; a viewer and a
// member with no access read none of the app's rows.
func TestTheAuditReadShowsAnAppsGrantsAndRefusalsToOwnersAndAdminsAlone(t *testing.T) {
	_, _, base, olive, carl, vic, nora, dana := teamOf(t)
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/sam@meridian.example", `{"level":"viewer"}`); code != 200 {
		t.Fatalf("share: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"/email/sam@meridian.example", ""); code != 204 {
		t.Fatalf("unshare: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"the contributor": carl, "the viewer": vic, "a member with no access": nora} {
		if code, _, _ := do(t, c, http.MethodGet, base+accessAt, ""); code != 403 {
			t.Fatalf("%s reads the access list: %d %s", who, code, lastBody)
		}
	}
	if code, _, _ := do(t, carl, http.MethodPost, base+"/api/v1/projects", `{"id":"zed"}`); code != 201 {
		t.Fatalf("carl creates zed, its owner: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodPut, base+"/api/v1/projects/zed/access/email/sam@meridian.example", `{"level":"viewer"}`); code != 200 {
		t.Fatalf("carl shares zed: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/zed/access", ""); code != 403 {
		t.Fatalf("nora reads zed's access list: %d %s", code, lastBody)
	}
	// each row once, by its action and who: the grantee of a share, the refused person of a denied row
	grantsOf := func(page map[string]any) string {
		seen := map[string]bool{}
		events, _ := page["events"].([]any)
		for _, e := range events {
			row := e.(map[string]any)
			switch a := row["action"].(string); a {
			case "share app", "unshare app":
				seen[a+" "+row["detail"].(map[string]any)["grantee"].(string)] = true
			case "denied":
				if row["project"] != nil {
					seen[a+" "+row["actor"].(map[string]any)["subject"].(string)] = true
				}
			}
		}
		return strings.Join(slices.Sorted(maps.Keys(seen)), ",")
	}
	const (
		filtered = "/api/v1/audit?project=claims-portal&action=share+app,unshare+app,denied&limit=200"
		whole    = "/api/v1/audit?project=claims-portal&limit=200"
		held     = "denied sub-carl,denied sub-nora,denied sub-vic,share app sam@meridian.example,unshare app sam@meridian.example"
	)
	for _, c := range []struct {
		who  string
		c    *http.Client
		want string
	}{{"the owner", olive, held}, {"an admin", dana, held}, {"a contributor", carl, ""}} {
		for _, path := range []string{filtered, whole} {
			code, page, _ := do(t, c.c, http.MethodGet, base+path, "")
			events, _ := page["events"].([]any)
			if got := grantsOf(page); code != 200 || got != c.want || page["total"] != float64(len(events)) || path == whole && len(events) == 0 {
				t.Errorf("%s reads %s: %d, the grant and refusal rows %q (want %q), total %v over %d rows", c.who, path, code, got, c.want, page["total"], len(events))
			}
		}
	}
	if code, page, _ := do(t, carl, http.MethodGet, base+"/api/v1/audit?project=zed&action=share+app,denied", ""); code != 200 || grantsOf(page) != "denied sub-nora,share app sam@meridian.example" {
		t.Errorf("carl reads the grants and refusals of zed, which he owns: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"a viewer": vic, "a member with no access": nora} {
		if code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?limit=200", ""); code != 200 || grantsOf(page) != "" || page["total"] != float64(len(page["events"].([]any))) {
			t.Errorf("%s reads the trail: %d, the grant and refusal rows %q", who, code, grantsOf(page))
		}
	}
}

// Every refusal of a share is answered before anything is written, byte for byte as the contract has it.
func TestEveryShareRefusal(t *testing.T) {
	pool, ctx, base, olive, _, _, _, _ := teamOf(t)
	code, body := uploadPulled(t, olive, base, "claims-portal", tarOf(t, map[string]string{"b": "2"}))
	if code != 201 {
		t.Fatal(code)
	}
	if _, err := pool.Exec(ctx, `UPDATE packages SET source_kind = 'repo-pr', roles = '{"merger": "a pull request says so"}' WHERE id = $1`, packageOf(body)["id"]); err != nil {
		t.Fatal(err)
	}
	const (
		grantee = `{"error":"invalid_grantee","message":"a grant names an email address, a group, or everyone"}`
		badBody = `{"error":"invalid_body","message":"the body is one JSON object with level and roles"}`
		level   = `{"error":"invalid_level","message":"a level is owner, contributor or viewer, and everyone's is viewer"}`
	)
	for _, c := range []struct {
		path, body string
		code       int
		want       string
	}{
		{"/team/ops", `{"level":"viewer"}`, 400, grantee},
		{"/email/not-an-address", `{"level":"viewer"}`, 400, grantee},
		{"/everyone/all", `{"level":"viewer"}`, 400, grantee},
		{"/email/a%00b@meridian.example", `{"level":"viewer"}`, 400, grantee},
		{"/email/sam%1B%5B31m@meridian.example", `{"level":"viewer"}`, 400, grantee},
		{"/email/sam%E2%80%AE@meridian.example", `{"level":"viewer"}`, 400, grantee},
		{"/email/sam@meridian.example", `not json`, 400, badBody},
		{"/email/sam@meridian.example", `null`, 400, badBody},
		{"/email/sam@meridian.example", `{"level":"viewer","admin":true}`, 400, badBody},
		{"/email/sam@meridian.example", `{"level":"viewer","roles":"auditor"}`, 400, badBody},
		{"/email/sam@meridian.example", `{"level":"viewer","roles":null}`, 400, badBody},
		{"/email/sam@meridian.example", `{"level":"viewer"} {}`, 400, badBody},
		{"/email/sam@meridian.example", `{"roles":[]}`, 400, level},
		{"/everyone/everyone", `{"level":"contributor"}`, 400, level},
		{"/email/sam@meridian.example", `{"level":"viewer","roles":["auditor","root"]}`, 400, `{"error":"unknown_role","message":"no package of claims-portal declares the role \"root\""}`},
		{"/email/sam@meridian.example", `{"level":"viewer","roles":["merger"]}`, 400, `{"error":"unknown_role","message":"no package of claims-portal declares the role \"merger\""}`},
		{"/group/claims-engineering", `{"level":"viewer"}`, 409, `{"error":"groups_unsupported","message":"This Canyon doesn't read groups from your sign-in yet. An admin sets the groups claim in Settings."}`},
		{"/email/olive@meridian.example", `{"level":"contributor"}`, 409, `{"error":"last_owner","message":"add a successor first"}`},
	} {
		if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+c.path, c.body); code != c.code || lastBody != c.want {
			t.Errorf("PUT %s %s: %d %s\nwant %s", c.path, c.body, code, lastBody, c.want)
		}
	}
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"/email/olive@meridian.example", ""); code != 409 || lastBody != `{"error":"last_owner","message":"add a successor first"}` {
		t.Errorf("unshare the last owner: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE grantee <> 'olive@meridian.example' OR level <> 'owner'"); n != 2 {
		t.Errorf("%d grants beside olive's, want carl's and vic's", n)
	}
	sameTrail(t, accessTrail(t, ctx, pool))
}

// Unsharing removes the grant and audits what it held; a second owner lets the first one go, and then the
// one left is the last.
func TestUnshareAuditsWhatTheGrantHeldAndKeepsTheLastOwner(t *testing.T) {
	pool, ctx, base, olive, carl, _, _, _ := teamOf(t)
	for _, c := range []struct {
		as                 *http.Client
		method, path, body string
		want               int
	}{
		{olive, http.MethodPut, "/email/carl@meridian.example", `{"level":"owner","roles":["approver"]}`, 200},
		{olive, http.MethodPut, "/everyone/everyone", `{"level":"viewer"}`, 200},
		{olive, http.MethodDelete, "/everyone/everyone", "", 204},
		{carl, http.MethodDelete, "/email/Olive@meridian.example", "", 204},
		{carl, http.MethodDelete, "/email/olive@meridian.example", "", 404},
		{carl, http.MethodDelete, "/team/ops", "", 404},
		{carl, http.MethodDelete, "/email/carl@meridian.example", "", 409},
	} {
		if code, _, _ := do(t, c.as, c.method, base+accessAt+c.path, c.body); code != c.want {
			t.Fatalf("%s %s: %d %s", c.method, c.path, code, lastBody)
		}
	}
	sameTrail(t, accessTrail(t, ctx, pool),
		trailRow("sub-olive", "share app", `{"kind":"email","grantee":"carl@meridian.example","level":{"before":"contributor","after":"owner"},"roles":{"before":[],"after":["approver"]}}`),
		trailRow("sub-olive", "share app", `{"kind":"everyone","grantee":"everyone","level":{"before":null,"after":"viewer"},"roles":{"before":null,"after":[]}}`),
		trailRow("sub-olive", "unshare app", `{"kind":"everyone","grantee":"everyone","level":"viewer","roles":[]}`),
		trailRow("sub-carl", "unshare app", `{"kind":"email","grantee":"olive@meridian.example","level":"owner","roles":[]}`))
}

// Nothing is shared where nobody signs in: a demo host routes none of the group.
func TestADemoHostRoutesNoAccessRoute(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedTeamApp(t, ctx, pool, base, client(t))
	for _, r := range [][2]string{{http.MethodGet, accessAt}, {http.MethodPut, accessAt + "/everyone/everyone"}, {http.MethodPost, accessAt + "-requests"}} {
		if code, _, _ := do(t, client(t), r[0], base+r[1], `{"level":"viewer"}`); code != 404 || lastBody != `{"error":"not_found","message":"no such route"}` {
			t.Errorf("%s %s: %d %s", r[0], r[1], code, lastBody)
		}
	}
}

// canyon share, run as the CLI runs, against these handlers: every form it sends is answered as it
// expects, the team it prints is the one written, and each refusal reaches the person in the host's words.
func TestTheCLIsShareAgainstTheAccessRoutes(t *testing.T) {
	pool, ctx, base, olive, carl, _, nora, _ := teamOf(t)
	if code, _, _ := do(t, nora, http.MethodPost, base+accessAt+"-requests", ""); code != 201 {
		t.Fatal(code)
	}
	share := func(as *http.Client, args ...string) (string, string, int) {
		t.Helper()
		u, _ := url.Parse(base)
		home := t.TempDir()
		conf, _ := json.Marshal(map[string]string{"host": base, "session": as.Jar.Cookies(u)[0].Value})
		if err := os.MkdirAll(filepath.Join(home, "canyon"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "canyon", "config.json"), conf, 0o600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"XDG_CONFIG_HOME": home, "NO_COLOR": "1"}
		var out, errOut strings.Builder
		code := cli.Main(append([]string{"share", "--app", "claims-portal"}, args...), &out, &errOut, strings.NewReader(""), func(k string) string { return env[k] }, false)
		return out.String(), errOut.String(), code
	}
	row := func(who, level, rest string) string {
		return strings.TrimRight(fmt.Sprintf("  %-29s  %-12s  %s", who, level, rest), " ") + "\n"
	}
	for _, c := range []struct {
		as       *http.Client
		args     []string
		code     int
		has, not string
	}{
		{olive, nil, 0, "\n  Requests:\n  nora@meridian.example", ""},
		{olive, []string{"nora@meridian.example", "--role", "auditor"}, 0, row("nora@meridian.example", "read-only", "auditor"), "Requests:"},
		{olive, []string{"everyone", "--role", "approver"}, 0, row("everyone at Meridian", "read-only", "approver"), ""},
		{olive, []string{"--remove", "nora@meridian.example"}, 0, row("carl@meridian.example", "contributor", ""), row("nora@meridian.example", "read-only", "auditor")}, // her row is gone; the Removed line names her
		{olive, []string{"group:ops"}, 1, "This Canyon doesn't read groups from your sign-in yet.", ""},
		{olive, []string{"--remove", "olive@meridian.example"}, 1, "add a successor first", ""},
		{carl, []string{"vic@meridian.example", "--level", "owner"}, 1, "only the owners of claims-portal and admins can do this: ask sub-olive", ""},
	} {
		out, errOut, code := share(c.as, c.args...)
		if code != c.code || !strings.Contains(out+errOut, c.has) || c.not != "" && strings.Contains(out, c.not) {
			t.Errorf("canyon share %s: exit %d\n%s%s\nwant exit %d with %q", strings.Join(c.args, " "), code, out, errOut, c.code, c.has)
		}
	}
	sameTrail(t, accessTrail(t, ctx, pool), trailRow("sub-nora", "request access", `{"email":"nora@meridian.example"}`),
		trailRow("sub-olive", "share app", `{"kind":"email","grantee":"nora@meridian.example","level":{"before":null,"after":"viewer"},"roles":{"before":null,"after":["auditor"]}}`),
		trailRow("sub-olive", "share app", `{"kind":"everyone","grantee":"everyone","level":{"before":null,"after":"viewer"},"roles":{"before":null,"after":["approver"]}}`),
		trailRow("sub-olive", "unshare app", `{"kind":"email","grantee":"nora@meridian.example","level":"viewer","roles":["auditor"]}`))
}
