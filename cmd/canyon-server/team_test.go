package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// teamRoutes is every route whose minimum level is contributor (contracts/routes.md, App teams), each
// asked so that a caller who passes gets the handler's own answer without changing anything: a gate
// refusal is 403 team_required, so any other answer is the handler's.
var teamRoutes = []struct {
	method, path, body string
	passed             int
}{
	{http.MethodPost, "/api/v1/projects/claims-portal/packages", "not a tar", 409}, // no base on the seeded current package: stale_base (#1503)
	{http.MethodGet, "/api/v1/projects/claims-portal/packages/current", "", 200},
	{http.MethodGet, "/api/v1/projects/claims-portal/packages", "", 200},
	{http.MethodGet, "/api/v1/projects/claims-portal/packages/pkg_nope", "", 404},
	{http.MethodPut, "/api/v1/projects/claims-portal/packages/pkg_nope/reports", "{}", 404},
	{http.MethodPut, "/api/v1/projects/claims-portal/images/sha256:0000000000000000000000000000000000000000000000000000000000000000/reports", "{}", 404}, // an image it neither runs nor keeps
	{http.MethodPost, "/api/v1/projects/claims-portal/gate-runs", `{"package":"pkg_nope"}`, 404},
	{http.MethodPost, "/api/v1/projects/claims-portal/deploys", `{"package":"pkg_nope"}`, 404},
	{http.MethodGet, "/api/v1/projects/claims-portal/gate-runs", "", 200},
	{http.MethodGet, "/api/v1/projects/claims-portal/status", "", 200},
	{http.MethodGet, "/api/v1/projects/claims-portal/status?run=gr_seed", "", 200},
	{http.MethodPost, "/api/v1/projects/claims-portal/gate-runs", `{"rollback_to":"sha256:` + strings.Repeat("c", 64) + `"}`, 409},
	{http.MethodPost, "/api/v1/projects/claims-portal/promotions", `{"gate_run":"gr_nope"}`, 404},
	{http.MethodDelete, "/api/v1/projects/claims-portal/envs/nope", "", 400},
	{http.MethodGet, "/api/v1/projects/claims-portal/envs/preview/image", "", 404}, // nothing deployed there
	{http.MethodGet, "/api/v1/projects/claims-portal/logs?env=nope", "", 400},
	{http.MethodGet, "/api/v1/projects/claims-portal/logs?redaction=masked&follow=true", "", 400}, // the dashboard's masked read, a contributor's as the raw one (#2744)
	{http.MethodPut, "/api/v1/projects/claims-portal/secrets/1bad", `{"value":"x"}`, 400},
	{http.MethodGet, "/api/v1/projects/claims-portal/secrets", "", 200},
	{http.MethodGet, "/api/v1/projects/claims-portal/egress", "", 200},
	{http.MethodDelete, "/api/v1/projects/claims-portal/secrets/NOPE", "", 404},
	{http.MethodGet, "/api/v1/gate-runs/gr_seed", "", 200},
	{http.MethodPost, "/api/v1/gate-runs/gr_seed/cancel", "", 409},
	{http.MethodGet, "/api/v1/gate-runs/gr_seed/sarif", "", 409},
	{http.MethodGet, "/api/v1/gate-runs/gr_seed/timeline", "", 200},
	{http.MethodPost, "/api/v1/gate-runs/gr_seed/findings/nope/approve", "{}", 400},
	{http.MethodPost, "/api/v1/gate-runs/gr_seed/findings/approve", "{}", 400},
}

// approverReads are the routes of teamRoutes a responder reaches with no grant: what approving a blocking
// finding needs (design/PERMISSIONS.md, Security responders; #2204).
var approverReads = []string{
	"GET /api/v1/projects/claims-portal/gate-runs", "GET /api/v1/projects/claims-portal/status", "GET /api/v1/projects/claims-portal/status?run=gr_seed",
	"GET /api/v1/gate-runs/gr_seed", "GET /api/v1/gate-runs/gr_seed/sarif",
	"POST /api/v1/gate-runs/gr_seed/findings/nope/approve", "POST /api/v1/gate-runs/gr_seed/findings/approve",
}

// seedTeamApp is claims-portal with one package and one ended run, owned by whoever created it, plus
// grants at contributor and viewer.
func seedTeamApp(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base string, creator *http.Client) {
	t.Helper()
	if code, _, _ := do(t, creator, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	code, body := upload(t, creator, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, ended_at) VALUES ('gr_seed', 'claims-portal', $1, 'failed', 'interrupted', now())`, packageOf(body)["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-olive', ''), ('claims-portal', 'email', 'vic@meridian.example', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
}

const teamRequiredBody = `{"error":"team_required","message":"Ask sub-olive to add you to claims-portal's team: on its page in Canyon, use Ask for access.","owner_groups":[],"owners":[{"kind":"person","subject":"sub-olive","display":"sub-olive"}]}`

// viewerTeamRequiredBody is teamRequiredBody to a read-only member, whose page has the button that asks them.
const viewerTeamRequiredBody = `{"error":"team_required","message":"Ask sub-olive to add you to claims-portal's team: on its page in Canyon, use Ask to join its team.","owner_groups":[],"owners":[{"kind":"person","subject":"sub-olive","display":"sub-olive"}]}`

// teamRequiredTo is the refusal a caller below contributor reads: a read-only member's, or one with no access's.
func teamRequiredTo(viewer bool) string {
	if viewer {
		return viewerTeamRequiredBody
	}
	return teamRequiredBody
}

// Every contributor route answers an owner, a contributor and an admin with the handler, and refuses a
// viewer and a caller with no access with team_required naming the owners and nothing else, writing
// nothing (contracts/routes.md, App teams).
func TestEveryTeamRoutePassesAContributorAndRefusesAViewer(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	callers := []struct {
		who    string
		c      *http.Client
		passes bool
	}{
		{"the owner", olive, true},
		{"the owner under a new address, known by subject", signedIn(t, ctx, pool, base, "sub-olive", "olive.new@meridian.example", false), true},
		{"a contributor", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), true},
		{"an admin with no grant", signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), true},
		{"a viewer", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), false},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), false},
	}
	trail := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'")
	for _, c := range callers {
		for _, r := range teamRoutes {
			code, _, _ := do(t, c.c, r.method, base+r.path, r.body)
			if c.passes && code != r.passed {
				t.Errorf("%s, %s %s: %d %s, want the handler's %d", c.who, r.method, r.path, code, lastBody, r.passed)
			}
			if !c.passes && (code != 403 || lastBody != teamRequiredTo(c.who == "a viewer")) {
				t.Errorf("%s, %s %s: %d %s, want team_required", c.who, r.method, r.path, code, lastBody)
			}
		}
		if !c.passes && count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'") != trail {
			t.Fatalf("%s: a refusal wrote an audit row beside its denied row", c.who)
		}
		trail = count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'")
	}
	nora := callers[5].c
	for path, want := range map[string]string{
		"/api/v1/projects/nope/status": `{"error":"not_found","message":"nope isn't on this Canyon yet. Deploy it first: canyon deploy"}`,
		"/api/v1/gate-runs/gr_nope":    `{"error":"not_found","message":"no such run"}`,
	} {
		if code, _, _ := do(t, nora, http.MethodGet, base+path, ""); code != 404 || lastBody != want {
			t.Errorf("%s, unknown to everyone: %d %s, want the 404 before the refusal", path, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now()"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, nora, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 404 || lastBody != `{"error":"not_found","message":"claims-portal isn't on this Canyon yet. Deploy it first: canyon deploy"}` {
		t.Errorf("a deleted app: %d %s, want the 404 before the refusal", code, lastBody)
	}
}

// With no owner the refusal says to ask an admin, and names nobody.
func TestTeamRequiredOnAnAppWithNoOwnerNamesAnAdmin(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false))
	if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE level = 'owner'"); err != nil {
		t.Fatal(err)
	}
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	if code, _, _ := do(t, vic, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 403 ||
		lastBody != `{"error":"team_required","message":"Ask an admin to add you to claims-portal's team: on its page in Canyon, use Ask to join its team.","owner_groups":[],"owners":[]}` {
		t.Fatalf("%d %s", code, lastBody)
	}
}

// A group-owned app's refusals ask a member of the owning group, never an admin (contracts/routes.md,
// App teams): owners[] names email grants alone, so owner_groups carries the group.
func TestARefusalOnAGroupOwnedAppAsksTheGroup(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false))
	if _, err := pool.Exec(ctx, `DELETE FROM project_access WHERE level = 'owner'; INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'group', 'platform', 'owner', 'person', 'sub-dana', ''), ('claims-portal', 'group', 'claims-team', 'owner', 'person', 'sub-dana', '')`); err != nil {
		t.Fatal(err)
	}
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	if code, _, _ := do(t, vic, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 403 ||
		lastBody != `{"error":"team_required","message":"Ask a member of the claims-team group to add you to claims-portal's team: on its page in Canyon, use Ask to join its team.","owner_groups":["claims-team","platform"],"owners":[]}` {
		t.Fatalf("team_required: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, vic, http.MethodDelete, base+"/api/v1/projects/claims-portal", ""); code != 403 || lastBody != ownerRequiredBody("claims-portal", "a member of the claims-team group") {
		t.Fatalf("owner_required: %d %s", code, lastBody)
	}
	if _, body, _ := do(t, vic, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); fmt.Sprint(body["project"].(map[string]any)["owner_groups"]) != "[claims-team platform]" {
		t.Fatalf("the read names the owning groups, so the no-access screen can: %s", lastBody)
	}
}

// Deleting an app is its owners' and admins': a contributor is refused owner_required before anything
// is read, and an owner and an admin reach the handler (its confirm_required).
func TestOnlyAnOwnerOrAnAdminDeletesAnApp(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	for _, c := range []struct {
		who  string
		c    *http.Client
		want int
	}{
		{"a contributor", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), 403},
		{"a viewer", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), 403},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), 403},
		{"the owner", olive, 409},
		{"an admin", signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), 409},
	} {
		code, body, _ := do(t, c.c, http.MethodDelete, base+"/api/v1/projects/claims-portal", "")
		if code != c.want || (code == 403 && lastBody != ownerRequiredBody("claims-portal", "sub-olive")) || (code == 409 && body["error"] != "confirm_required") {
			t.Errorf("%s: %d %s, want %d", c.who, code, lastBody, c.want)
		}
	}
}

// A demo host's every caller is the anonymous admin: an app nobody holds a grant on refuses nothing.
func TestADemoHostRefusesNoTeamRoute(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	seedTeamApp(t, ctx, pool, base, c)
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE level = 'owner'"); n != 0 {
		t.Fatalf("%d owner grants on a demo host", n)
	}
	for _, r := range teamRoutes {
		if code, _, _ := do(t, c, r.method, base+r.path, r.body); code != r.passed {
			t.Errorf("%s %s: %d %s, want the handler's %d", r.method, r.path, code, lastBody, r.passed)
		}
	}
}

// Below contributor an app reads as its name, its owners, its summary and the caller's own level, and
// nothing else but a viewer's live addresses, none while nothing is deployed — on the single read, the
// list and the fleet; from contributor, and for an admin, whole.
func TestBelowContributorAnAppReadsReduced(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false))
	if _, err := pool.Exec(ctx, "UPDATE packages SET description = 'File and track motor claims'"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ sub, email, level, envs string }{{"sub-vic", "vic@meridian.example", "viewer", `,"envs":{}`}, {"sub-nora", "nora@meridian.example", "none", ""}} {
		reader := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		row := `{"id":"claims-portal","name":"claims-portal","owners":[{"kind":"person","subject":"sub-olive","display":"sub-olive"}],"owner_groups":[],"summary":"File and track motor claims","claimable":false,"access":{"level":"` + c.level + `"}` + c.envs + `}`
		for path, want := range map[string]string{"/api/v1/projects/claims-portal": `{"project":` + row + `}`, "/api/v1/projects": `{"projects":[` + row + `]}`} {
			if code, _, _ := do(t, reader, http.MethodGet, base+path, ""); code != 200 || lastBody != want {
				t.Errorf("%s, %s: %d %s\nwant %s", c.level, path, code, lastBody, want)
			}
		}
		if do(t, reader, http.MethodGet, base+"/api/v1/fleet", ""); !strings.Contains(lastBody, `"apps":[`+row+`]`) {
			t.Errorf("%s, the fleet: %s", c.level, lastBody)
		}
	}
	for _, c := range []struct{ sub, email string }{{"sub-carl", "carl@meridian.example"}, {"sub-dana", "dana@meridian.example"}} {
		reader := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		if _, body, _ := do(t, reader, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); body["project"].(map[string]any)["gate_state"] != "failed" || body["project"].(map[string]any)["resources"] == nil {
			t.Errorf("%s reads the app whole: %s", c.sub, lastBody)
		}
	}
}

// A viewer reads where the app runs and whether it runs, and nothing else of it (#1521): an environment
// Canyon runs, with the state the executor answers in one call per read however many apps — on the
// single read and the fleet (the list carries none) — and one the org runs, or a release is open over,
// with its address alone. One with no address, or waiting on its first release, is left out. A caller
// at none reads no environment at all, and is never asked about.
func TestAViewerReadsTheAppsLiveAddressesAndWhetherEachRuns(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	if code, _, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-archive"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	if code, _ := upload(t, olive, base, "claims-archive", tarOf(t, map[string]string{"a": "1"}), "", ""); code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-archive', 'email', 'vic@meridian.example', 'viewer', 'person', 'sub-olive', '');
		INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT p.project_id, e.env, p.id, 'app@sha256:cfd86d789f71ac8e', e.url, 'gr_seed', e.by FROM packages p JOIN (VALUES
		('claims-portal', 'preview', 'http://claims-portal.preview.localhost:7777', 'canyon'), ('claims-portal', 'prod', 'https://claims.apps.meridian.example', 'org'),
		('claims-archive', 'preview', 'http://claims-archive.preview.localhost:7777', 'canyon')) e(app, env, url, by) ON e.app = p.project_id`); err != nil {
		t.Fatal(err)
	}
	fake.Start("claims-portal", "preview") // claims-archive's preview, which nothing started, reads stopped
	row := func(id, level, envs string) string {
		return `{"id":"` + id + `","name":"` + id + `","owners":[{"kind":"person","subject":"sub-olive","display":"sub-olive"}],"owner_groups":[],"summary":"","claimable":false,"access":{"level":"` + level + `"}` + envs + `}`
	}
	portal := func(envs string) string { return row("claims-portal", "viewer", `,"envs":`+envs) }
	const portalPreview, portalProd = `"preview":{"url":"http://claims-portal.preview.localhost:7777"`, `"prod":{"url":"https://claims.apps.meridian.example"}`
	live := portal(`{` + portalPreview + `,"state":"running"},` + portalProd + `}`)
	reads := func(who string, c *http.Client, path, want string, contains bool, calls int32) {
		t.Helper()
		asked := fake.RunningOfs.Load()
		code, _, _ := do(t, c, http.MethodGet, base+path, "")
		if code != 200 || (contains && !strings.Contains(lastBody, want)) || (!contains && lastBody != want) {
			t.Errorf("%s, %s: %d %s\nwant %s", who, path, code, lastBody, want)
		}
		if n := fake.RunningOfs.Load() - asked; n != calls {
			t.Errorf("%s, %s asked the executor %d times, want %d", who, path, n, calls)
		}
	}
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	reads("a viewer", vic, "/api/v1/projects/claims-portal", `{"project":`+live+`}`, false, 1)
	reads("a viewer", vic, "/api/v1/fleet", `"apps":[`+row("claims-archive", "viewer", `,"envs":{"preview":{"url":"http://claims-archive.preview.localhost:7777","state":"stopped"}}`)+`,`+live+`]`, true, 1)
	reads("a viewer", vic, "/api/v1/projects", `{"projects":[`+row("claims-archive", "viewer", `,"envs":{"preview":{"url":"http://claims-archive.preview.localhost:7777"}}`)+`,`+portal(`{`+portalPreview+`},`+portalProd+`}`)+`]}`, false, 0)
	nora := signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	nones := row("claims-archive", "none", "") + `,` + row("claims-portal", "none", "")
	reads("none", nora, "/api/v1/projects/claims-portal", `{"project":`+row("claims-portal", "none", "")+`}`, false, 0)
	reads("none", nora, "/api/v1/projects", `{"projects":[`+nones+`]}`, false, 0)
	reads("none", nora, "/api/v1/fleet", `"apps":[`+nones+`]`, true, 0)
	fake.FailRunning.Store(true)
	reads("a viewer, the executor failing", vic, "/api/v1/projects/claims-portal", `{"project":`+portal(`{`+portalPreview+`,"state":"unknown"},`+portalProd+`}`)+`}`, false, 1)
	fake.FailRunning.Store(false)
	step := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	const release = `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
		SELECT 'rel_3d9a', 'claims-portal', 'preview', 'gr_seed', id, 'deploy', 'app@sha256:cfd86d789f71ac8e', gen_random_uuid(), 'accepted', now() FROM packages WHERE project_id = 'claims-portal'`
	step(release) // open over Canyon's preview: the org runs it now, so its address and no state
	reads("a viewer, a release open over preview", vic, "/api/v1/projects/claims-portal", `{"project":`+portal(`{`+portalPreview+`},`+portalProd+`}`)+`}`, false, 0)
	step(`DELETE FROM releases; UPDATE deploys SET url = '' WHERE project_id = 'claims-portal'`) // no address, whoever runs it
	reads("a viewer, no address", vic, "/api/v1/projects/claims-portal", `{"project":`+portal(`{}`)+`}`, false, 1)
	step(`DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'; ` + release) // waiting on its first release
	reads("a viewer, waiting on a first release", vic, "/api/v1/projects/claims-portal", `{"project":`+portal(`{}`)+`}`, false, 0)
}

// A member's fleet lists their team apps first and the rest after them by id; its tiles count only the
// team apps, where an admin's count every app; q and owner match every row, the other filters only the
// team rows, and a cursor walks both without a repeat or a gap.
func TestAMembersFleetCountsAndFiltersTheirTeamApps(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	nora := signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	for _, c := range []struct {
		c   *http.Client
		app string
	}{{olive, "beta"}, {olive, "zed"}, {olive, "orphan"}, {nora, "alpha"}} { // in this order: a stage's rows sort by since, the oldest first
		if code, _, _ := do(t, c.c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+c.app+`"}`); code != 201 {
			t.Fatalf("create %s: %d %s", c.app, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM project_access WHERE project_id = 'orphan'; UPDATE projects SET stack = 'Remix';
		INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		('zed', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-olive', ''), ('beta', 'email', 'carl@meridian.example', 'viewer', 'person', 'sub-olive', ''),
		('beta', 'everyone', '', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, c := range []struct {
		who   string
		c     *http.Client
		query string
		want  string
	}{
		{"carl", carl, "", "zed,alpha,beta,orphan"},
		{"carl", carl, "tile=never_reviewed", "zed"}, {"carl", carl, "tile=unowned", ""}, {"dana", dana, "tile=unowned", "orphan"},
		{"carl", carl, "stage=created", "zed"}, {"carl", carl, "running=nothing", "zed"}, {"carl", carl, "needs_you=true", "zed"}, {"carl", carl, "stack=remix", "zed"},
		{"carl", carl, "access=shared", "zed"}, {"carl", carl, "access=everyone", ""}, {"carl", carl, "access=private", ""},
		{"dana", dana, "access=shared", "zed"}, {"dana", dana, "access=everyone", "beta"}, {"dana", dana, "access=private", "orphan,alpha"},
		{"carl", carl, "q=a", "alpha,beta,orphan"}, {"carl", carl, "owner=sub-nora", "alpha"}, {"carl", carl, "owner=nora@meridian.example", "alpha"},
		{"carl", carl, "owner=Nora", ""}, {"carl", carl, "scope=team", "zed"}, {"dana", dana, "scope=team", ""}, // "Your apps": a reduced row is never one, nor an admin's app by role alone
		{"carl", carl, "sort=changed", "zed,alpha,beta,orphan"}, {"carl", carl, "sort=name", "alpha,beta,orphan,zed"}, // reduced rows after the rest by id, but by name among them
		{"carl", carl, "limit=1&page=2", "alpha"}, {"carl", carl, "sort=name&limit=1&page=3", "orphan"}, {"carl", carl, "scope=team&page=5", "zed"}, // a numbered page offsets into the reduced rows too
	} {
		code, page, _ := do(t, c.c, http.MethodGet, base+"/api/v1/fleet?"+c.query, "")
		if got := strings.Join(ids(t, page), ","); code != 200 || got != c.want {
			t.Errorf("%s ?%s: %d %q, want %q", c.who, c.query, code, got, c.want)
		}
	}
	for _, sort := range []string{"risk", "changed", "name"} { // one row a page: every cursor crosses from the team rows to the reduced ones
		_, whole, _ := do(t, carl, http.MethodGet, base+"/api/v1/fleet?sort="+sort, "")
		var walked []string
		for url := base + "/api/v1/fleet?limit=1&sort=" + sort; ; {
			_, page, _ := do(t, carl, http.MethodGet, url, "")
			walked = append(walked, ids(t, page)...)
			next, ok := page["next"].(string)
			if !ok {
				break
			}
			url = base + "/api/v1/fleet?limit=1&sort=" + sort + "&cursor=" + next
		}
		if want := ids(t, whole); !slices.Equal(walked, want) {
			t.Errorf("carl walking %s one row a page: %v, want %v", sort, walked, want)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, project_id) VALUES
	  ('ev_m1', now(), 'person', 'sub-olive', 'deploy preview', 'zed'), ('ev_m2', now(), 'person', 'sub-nora', 'deploy prod', 'alpha')`); err != nil {
		t.Fatal(err)
	}
	if _, page, _ := do(t, carl, http.MethodGet, base+"/api/v1/fleet", ""); string(mustJSON(page["stats"])) != `{"apps":1,"deploys_7d":1,"owners":1,"passed":0,"preview":0,"production":0,"production_7d":0}` {
		t.Errorf("carl's stats count his team app alone: %s", mustJSON(page["stats"]))
	}
	for who, want := range map[*http.Client]string{carl: `{"approved":0,"blocked":0,"failed":0,"needs_you":1,"never_reviewed":1,"refused":0,"secrets_exposed":0,"unowned":0}`, dana: `{"approved":0,"blocked":0,"failed":0,"needs_you":5,"never_reviewed":4,"refused":0,"secrets_exposed":0,"unowned":1}`} {
		if _, page, _ := do(t, who, http.MethodGet, base+"/api/v1/fleet?q=zed", ""); string(mustJSON(page["tiles"])) != want {
			t.Errorf("tiles: %s, want %s", mustJSON(page["tiles"]), want)
		}
	}
	var walked []string
	for url := base + "/api/v1/fleet?limit=1"; ; {
		_, page, _ := do(t, carl, http.MethodGet, url, "")
		walked = append(walked, ids(t, page)...)
		next, ok := page["next"].(string)
		if !ok {
			break
		}
		url = base + "/api/v1/fleet?limit=1&cursor=" + next
	}
	if got := strings.Join(walked, ","); got != "zed,alpha,beta,orphan" {
		t.Fatalf("walking carl's fleet at limit=1: %s", got)
	}
}

// orgRowsOf is each row on a trail page naming no app, as its action and actor subject, sorted.
func orgRowsOf(page map[string]any) string {
	var rows []string
	events, _ := page["events"].([]any)
	for _, e := range events {
		row := e.(map[string]any)
		if _, ok := row["project"]; !ok {
			rows = append(rows, row["action"].(string)+" "+row["actor"].(map[string]any)["subject"].(string))
		}
	}
	slices.Sort(rows)
	return strings.Join(rows, ",")
}

// projectsOf is the distinct project of each row on a trail or timeline page, "" for an org row, sorted.
func projectsOf(page map[string]any, key string) string {
	seen := map[string]bool{}
	rows, _ := page[key].([]any)
	for _, row := range rows {
		p, _ := row.(map[string]any)["project"].(string)
		seen[p] = true
	}
	return strings.Join(slices.Sorted(maps.Keys(seen)), ",")
}

// A member reads their team apps' audit rows and the rows naming no app that they made, never the org's
// governance rows or another person's sign-in, and the org timeline's rows of their team apps; naming an
// app they are below contributor on is team_required, an id no app has carried is an empty page, and a
// deleted app they were on stays readable. An admin reads every row.
func TestAMemberReadsTheTrailOfTheirTeamAppsAndTheirOwn(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, app := range []string{"beta", "zed"} {
		if code, _, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects", `{"id":"`+app+`"}`); code != 201 {
			t.Fatalf("create %s: %d %s", app, code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, stacksBody); code != 200 {
		t.Fatalf("an org row: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('zed', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-olive', ''), ('beta', 'email', 'carl@meridian.example', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	nora := signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	// each one's sign-in; a policy carl changed while he was an admin, an admin's to read now; and an agent's
	// sign-in carrying carl's subject: a member's own rows are a person's
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, actor_kind, actor_subject, action, detail) VALUES
		('ev_in_carl', 'person', 'sub-carl', 'sign in', '{}'), ('ev_in_dana', 'person', 'sub-dana', 'sign in', '{}'), ('ev_in_nora', 'person', 'sub-nora', 'sign in', '{}'),
		('ev_policy', 'person', 'sub-carl', 'policy.set', '{}'), ('ev_agent', 'agent', 'sub-carl', 'sign out', '{}')`); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[*http.Client]string{
		carl: "sign in sub-carl", nora: "sign in sub-nora",
		dana: "PUT /api/v1/preferences sub-dana,policy.set sub-carl,sign in sub-carl,sign in sub-dana,sign in sub-nora,sign out sub-carl",
	} {
		if _, page, _ := do(t, who, http.MethodGet, base+"/api/v1/audit?limit=200", ""); orgRowsOf(page) != want {
			t.Errorf("the rows naming no app: %q, want %q", orgRowsOf(page), want)
		}
	}
	for _, c := range []struct {
		who       string
		c         *http.Client
		path, key string
		want      string
	}{
		{"carl", carl, "/api/v1/audit", "events", ",zed"}, {"dana", dana, "/api/v1/audit", "events", ",beta,zed"},
		{"nora, on no app", nora, "/api/v1/audit", "events", ""}, {"nora, on no app", nora, "/api/v1/timeline", "rows", ""},
		{"carl", carl, "/api/v1/audit?project=zed", "events", "zed"},
		{"carl", carl, "/api/v1/timeline", "rows", "zed"}, {"dana", dana, "/api/v1/timeline", "rows", "beta,zed"},
	} {
		if code, page, _ := do(t, c.c, http.MethodGet, base+c.path, ""); code != 200 || projectsOf(page, c.key) != c.want {
			t.Errorf("%s %s: %d, the rows' projects %q, want %q: %s", c.who, c.path, code, projectsOf(page, c.key), c.want, lastBody)
		} else if events, ok := page["events"].([]any); ok && page["total"] != float64(len(events)) { // the total counts only the rows the member reads
			t.Errorf("%s %s: total %v over the %d rows they read", c.who, c.path, page["total"], len(events))
		}
	}
	if code, _, _ := do(t, carl, http.MethodGet, base+"/api/v1/audit?project=beta", ""); code != 403 ||
		lastBody != `{"error":"team_required","message":"Ask sub-olive to add you to beta's team: on its page in Canyon, use Ask to join its team.","owner_groups":[],"owners":[{"kind":"person","subject":"sub-olive","display":"sub-olive"}]}` {
		t.Errorf("an app carl only views: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodGet, base+"/api/v1/audit?project=never", ""); code != 200 || lastBody != `{"events":[],"next":null,"page":1,"total":0}` {
		t.Errorf("an id no app has carried: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'zed'"); err != nil {
		t.Fatal(err)
	}
	if code, page, _ := do(t, carl, http.MethodGet, base+"/api/v1/audit?project=zed", ""); code != 200 || projectsOf(page, "events") != "zed" {
		t.Errorf("a deleted app carl was on: %d %s", code, lastBody)
	}
}

// gatedElsewhere is each app route registered without a team check — teamOnly, runTeamOnly, ownerOnly,
// reviewOnly, runReviewOnly or outboundTeamOnly — and where its check lives instead.
var gatedElsewhere = map[string]string{
	"GET /projects/{id}":                   "a viewer reads it too: getProject trims what one below the team sees",
	"PUT /projects/{id}/owner":             "claimProject: anyone claims an app nobody owns, else its owners and admins",
	"DELETE /projects/{id}":                "deleteProject asks ownerRequired itself",
	"POST /projects/{id}/access-requests":  "a member at level none asks for access: that is the route",
	"PUT /projects/{id}/repository":        "linkGuard: an admin's, never a level (#1917)",
	"DELETE /projects/{id}/repository":     "linkGuard: an admin's, never a level (#1917)",
	"POST /projects/{id}/envs/prod/hold":   "hold: a responder's or an admin's, never a level (contracts/routes.md, The hold)",
	"DELETE /projects/{id}/envs/prod/hold": "lift: a responder off the app's team, or an admin, never a level",
}

// Every route under /projects/{id} and /gate-runs/ is registered through a team check — teamOnly,
// runTeamOnly, ownerOnly, reviewOnly, runReviewOnly or outboundTeamOnly — or is named in gatedElsewhere with
// where its check lives: a route added with neither fails here instead of shipping open to every signed-in
// person (#1117). Read off the package's source with go/ast, since http.ServeMux does not list its patterns; a
// name in gatedElsewhere no route has is stale.
func TestEveryAppRouteIsGatedOrNamed(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset, seen := gotoken.NewFileSet(), map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			sel, isSel := callee(call)
			if !ok || !isSel || sel != "HandleFunc" && sel != "Handle" || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			_, path, _ := strings.Cut(pattern, " ")
			if err != nil || !strings.HasPrefix(path, "/projects/{") && !strings.HasPrefix(path, "/gate-runs/{") {
				return true
			}
			seen[pattern] = true
			wrap, _ := call.Args[1].(*ast.CallExpr)
			if name, ok := callee(wrap); (!ok || !slices.Contains([]string{"teamOnly", "runTeamOnly", "ownerOnly", "reviewOnly", "runReviewOnly", "outboundTeamOnly"}, name)) && gatedElsewhere[pattern] == "" {
				t.Errorf("%s: %s registers it with no team check and gatedElsewhere does not name it", fset.Position(call.Pos()), pattern)
			}
			return true
		})
	}
	for pattern := range gatedElsewhere {
		if !seen[pattern] {
			t.Errorf("gatedElsewhere names %s, which no route registers", pattern)
		}
	}
}

// callee is the method or function name call calls, as in a.teamOnly(…) or api.HandleFunc(…).
func callee(call *ast.CallExpr) (string, bool) {
	if call == nil {
		return "", false
	}
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name, true
	case *ast.Ident:
		return f.Name, true
	}
	return "", false
}
