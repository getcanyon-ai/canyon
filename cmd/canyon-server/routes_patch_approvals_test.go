//go:build !noee

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// filePatches is a rescan tick's filing: how many patch approvals it filed.
func filePatches(t *testing.T, ctx context.Context) int {
	t.Helper()
	n, err := pipe.Patcher.FileApprovals(ctx, pipe, "")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// patchCode is the app's one pending patch approval's code, after a tick that filed it.
func patchCode(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	if n := filePatches(t, ctx); n != 1 {
		t.Fatalf("the tick filed %d patch approvals, want 1", n)
	}
	var code string
	if err := pool.QueryRow(ctx, "SELECT code FROM promotion_approvals WHERE kind = 'patch' AND state = 'pending'").Scan(&code); err != nil {
		t.Fatal(err)
	}
	return code
}

const (
	approvalsAt       = "/api/v1/promotion-approvals/"
	patchOutdatedBody = `{"error":"outdated","message":"production runs another version now: Canyon asks again if the patch still applies"}`
)

// One open patch approval per environment: a tick asks once for a passed patch, and again finds it open; the read
// says what the patch is. A newer patch's supersedes it, and the app's team approves that one in the browser: the
// promote run ships preview's patched image to production, nothing rebuilt (contracts/routes.md, Production approvals).
func TestPatchApprovalOnePerEnvironment(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	var prod string
	if err := pool.QueryRow(ctx, "SELECT image FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'").Scan(&prod); err != nil {
		t.Fatal(err)
	}
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	first := patchCode(t, ctx, pool)
	if n := filePatches(t, ctx); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending' AND code = '"+first+"'") != 1 {
		t.Fatalf("a second tick filed %d over the open one", n)
	}
	_, read, _ := do(t, browser, http.MethodGet, base+approvalsAt+first, "")
	fixes, _ := read["fixes"].([]any)
	if read["kind"] != "patch" || read["patch_run"] != "gr_patcha" || read["identity"] != patchIdentity || read["replaces_image"] != prod[strings.Index(prod, "@")+1:] ||
		read["image"] != "sha256:"+strings.Repeat("6a", 32) || // the patched image that goes live, never the run image (#2593)
		read["proof"] != "The app's own files are byte for byte the ones in production." || read["state"] != "pending" || read["requested_via"] != "canyon" ||
		string(mustJSON(read["requested_by"])) != `{"display":"Canyon","kind":"canyon","subject":"patch"}` || len(fixes) != 1 ||
		string(mustJSON(fixes[0])) != `{"class":"os","from":"3.0.13-0ubuntu3.4","name":"libssl3","to":"3.0.13-0ubuntu3.5","vulnerability":"CVE-2024-5535"}` ||
		string(mustJSON(read["watch"])) != `{"result":"passed","until":"2026-10-29T14:35:00Z"}` {
		t.Fatalf("the patch approval's read: %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request patch approval' AND actor_kind = 'canyon' AND run_id = 'gr_patcha' AND detail ?& array['package', 'code', 'image']"); n != 1 {
		t.Fatalf("one request patch approval row, got %d", n)
	}
	// a person deploys and promotes another version, and Canyon patches that: the newer patch's approval supersedes the first
	if _, err := pool.Exec(ctx, "UPDATE deploys SET image = 'canyon/claims-portal@sha256:' || repeat('7b', 32) WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if _, read, _ = do(t, browser, http.MethodGet, base+approvalsAt+first, ""); read["state"] != "outdated" {
		t.Fatalf("production moved: %s", lastBody)
	}
	patched := passedPatch(t, ctx, pool, deploy, "gr_patchb", "8c")
	second := patchCode(t, ctx, pool)
	if code, _, _ := do(t, browser, http.MethodGet, base+approvalsAt+first, ""); second == first || code != 404 ||
		count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE project_id = 'claims-portal' AND state = 'pending'") != 1 {
		t.Fatalf("the newer patch's approval supersedes the first: %d %s", code, lastBody)
	}
	owner := teamMember(t, ctx, pool, base, "sub-olga", "olga@meridian.example", "owner")
	got, started, _ := do(t, owner, http.MethodPost, base+approvalsAt+second+"/approve", "")
	run, _ := runOf(started)["id"].(string)
	if got != 202 || waitVerdict(t, owner, base, run)["state"] != "clean" || fake.Ran().Target != "prod" || fake.Ran().Image != patched {
		t.Fatalf("an owner approves the patch: %d %s, ran %+v", got, lastBody, fake.Ran())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'patch prod' AND run_id = '"+run+"'"); n != 1 || filePatches(t, ctx) != 0 {
		t.Fatalf("the approved patch is production's, and nothing is asked again: %d patch prod rows", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE run_id = '"+run+"' AND dedupe IN ('patch:watching', 'patch:passed')"); n != 2 {
		t.Fatalf("the approved patch's promotion watched production and passed: %d of its two lines", n)
	}
}

// teamMember is a browser session of a person granted level on claims-portal.
func teamMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, sub, email, level string) *http.Client {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', $1, $2, 'person', 'sub-dana', 'dana')`, email, level); err != nil {
		t.Fatal(err)
	}
	return signedIn(t, ctx, pool, base, sub, email, false)
}

// A patch approval is decided as any production approval is: contributor and above, a person, from the browser.
// An owner, a contributor and an admin read it; a read-only member and one with no access are refused it, the
// admin's own CLI is refused a decision with one denied row each, and a contributor's denial stands: nothing is
// asked again of the app for that identity (contracts/routes.md, Production approvals; design/PATCHING.md §9).
func TestPatchApprovalDecidedByTheTeamInTheBrowser(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	at := base + approvalsAt + patchCode(t, ctx, pool)
	owner := teamMember(t, ctx, pool, base, "sub-olga", "olga@meridian.example", "owner")
	contributor := teamMember(t, ctx, pool, base, "sub-erin", "erin@meridian.example", "contributor")
	viewer := teamMember(t, ctx, pool, base, "sub-vic", "vic@meridian.example", "viewer")
	nobody := signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	for who, c := range map[string]*http.Client{"an owner": owner, "a contributor": contributor, "an admin": browser} {
		if got, read, _ := do(t, c, http.MethodGet, at, ""); got != 200 || read["kind"] != "patch" {
			t.Fatalf("%s reads it: %d %s", who, got, lastBody)
		}
	}
	for who, c := range map[string]*http.Client{"a read-only member": viewer, "no access": nobody} {
		for _, step := range []struct{ method, path string }{{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"}} {
			if got, refused, _ := do(t, c, step.method, at+step.path, ""); got != 403 || refused["error"] != "team_required" {
				t.Fatalf("%s %s %s: %d %s", who, step.method, step.path, got, lastBody)
			}
		}
	}
	for _, verb := range []string{"approve", "deny"} {
		if got, _, _ := do(t, cli, http.MethodPost, at+"/"+verb, ""); got != 403 || lastBody != `{"error":"dashboard_required","message":"approve it in Canyon's dashboard"}` {
			t.Fatalf("the admin's CLI %ss: %d %s", verb, got, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'"); n != 2 {
		t.Fatalf("two denied rows from the CLI, got %d", n)
	}
	if got, _, _ := do(t, contributor, http.MethodPost, at+"/deny", ""); got != 204 {
		t.Fatalf("a contributor denies: %d %s", got, lastBody)
	}
	if n := filePatches(t, ctx); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending'") != 0 {
		t.Fatalf("a denial stands, yet the tick filed %d", n)
	}
	// production keeps the unpatched image, and reads denied, never none (#2601): a contributor reads it, with the
	// approval denied; a read-only member's row carries no patch state; the identities read counts it denied
	_, st, _ := do(t, contributor, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	prod, _ := st["envs"].(map[string]any)["prod"].(map[string]any)["patch"].(map[string]any)
	if prod["state"] != "denied" || prod["approval"] != shown(strings.TrimPrefix(at, base+approvalsAt)) || prod["identity"] != patchIdentity || prod["run"] != "gr_patcha" {
		t.Fatalf("production after the denial: %s", mustJSON(prod))
	}
	patchShape(t, "env_patch", prod)
	if _, row, _ := do(t, viewer, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); strings.Contains(string(mustJSON(row)), `"patch"`) {
		t.Fatalf("a read-only member reads a patch state: %s", lastBody)
	}
	_, all, _ := do(t, browser, http.MethodGet, base+identitiesAt, "")
	if apps := all["identities"].([]any)[0].(map[string]any)["apps"].(map[string]any); apps["denied"] != float64(1) || apps["patched_preview"] != float64(0) || apps["held"] != float64(0) {
		t.Fatalf("the identity's apps after the denial: %s", mustJSON(apps))
	}
}

// A patch approval never supersedes a person's: none is filed while a person's is pending. Once theirs expired,
// Canyon asks in its place, and a person's next request deletes the patch approval as it deletes any.
func TestPatchApprovalNeverOverPerson(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true"); err != nil {
		t.Fatal(err)
	}
	person := askApproval(t, cli, base, "gr_patcha")
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("a patch approval was filed over a person's pending one: %d", n)
	}
	if _, read, _ := do(t, browser, http.MethodGet, base+approvalsAt+person, ""); read["state"] != "pending" || read["kind"] != nil {
		t.Fatalf("the person's approval stands: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE promotion_approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	patch := patchCode(t, ctx, pool)
	if got, _, _ := do(t, browser, http.MethodGet, base+approvalsAt+person, ""); got != 404 {
		t.Fatalf("the expired request is replaced: %d %s", got, lastBody)
	}
	again := askApproval(t, cli, base, "gr_patcha")
	if got, _, _ := do(t, browser, http.MethodGet, base+approvalsAt+patch, ""); got != 404 ||
		count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending' AND kind = 'promotion' AND code = '"+strings.ReplaceAll(again, "-", "")+"'") != 1 {
		t.Fatalf("a person's request deletes the patch approval: %d %s", got, lastBody)
	}
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("filed %d over the person's new request", n)
	}
}

// Held production is never patched: the hold outdates the open patch approval, approving it is 409 held, and no
// tick files another while production is held (contracts/routes.md, Production approvals; OPERATIONS.md §3.4).
func TestHeldProductionFilesNone(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	at := base + approvalsAt + patchCode(t, ctx, pool)
	if code, _, _ := do(t, browser, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "outdated" || read["held"] == nil {
		t.Fatalf("the hold outdates it: %s", lastBody)
	}
	if got, body, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 409 || body["error"] != "held" {
		t.Fatalf("approving it while held: %d %s", got, lastBody)
	}
	if n := filePatches(t, ctx); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending'") != 0 ||
		count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote' AND created_at > (SELECT held_at FROM prod_holds)") != 0 {
		t.Fatalf("a held production is asked nothing and promotes nothing: filed %d", n)
	}
}

// An expired patch approval is asked again while the plan stands, so 24 hours is never the end. One reads
// outdated once patching is off or its identity paused, and approving it is then refused; once neither holds an
// admin approves it, and once production runs the patch nothing is asked again.
func TestPatchApprovalRefiledAfterExpiry(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	patched := passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	first := patchCode(t, ctx, pool)
	if _, err := pool.Exec(ctx, "UPDATE promotion_approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, base+approvalsAt+first, ""); read["state"] != "expired" {
		t.Fatalf("expired: %s", lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, base+approvalsAt+first+"/approve", ""); got != 404 {
		t.Fatalf("approving an expired one: %d %s", got, lastBody)
	}
	second := patchCode(t, ctx, pool)
	if got, _, _ := do(t, browser, http.MethodGet, base+approvalsAt+first, ""); second == first || got != 404 {
		t.Fatalf("re-filed in its place: %d %s", got, lastBody)
	}
	at := base + approvalsAt + second
	for _, stop := range []struct{ what, on, off string }{
		{"patching off", "UPDATE org_preferences SET patching = 'off'", "UPDATE org_preferences SET patching = 'ask'"},
		{"the identity paused", "UPDATE patch_identities SET paused_at = now(), paused_projects = '{claims-portal}'", "UPDATE patch_identities SET paused_at = NULL, paused_projects = NULL"},
	} {
		if _, err := pool.Exec(ctx, stop.on); err != nil {
			t.Fatal(err)
		}
		if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "outdated" {
			t.Fatalf("%s: %s", stop.what, lastBody)
		}
		if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 409 || lastBody != patchOutdatedBody {
			t.Fatalf("%s, approving: %d %s", stop.what, got, lastBody)
		}
		if n := filePatches(t, ctx); n != 0 {
			t.Fatalf("%s, the tick filed %d", stop.what, n)
		}
		if _, err := pool.Exec(ctx, stop.off); err != nil {
			t.Fatal(err)
		}
	}
	got, started, _ := do(t, browser, http.MethodPost, at+"/approve", "")
	run, _ := runOf(started)["id"].(string)
	if got != 202 || waitVerdict(t, browser, base, run)["state"] != "clean" || fake.Ran().Target != "prod" || fake.Ran().Image != patched {
		t.Fatalf("an admin approves it: %d %s, ran %+v", got, lastBody, fake.Ran())
	}
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("production runs the patch, yet the tick filed %d", n)
	}
}

// With sign-in off nobody can decide a patch approval: patching reads off, and a PUT of ask is refused.
func TestWithSignInOffPatchingIsOff(t *testing.T) {
	_, _, base, c := withProject(t)
	if got := getPreferences(t, c, base)["patching"]; got != "off" {
		t.Fatalf("off: %v", got)
	}
	if code, _ := putPreferences(t, c, base, `{"patching":"ask"}`); code != 409 || !strings.HasPrefix(lastBody, `{"error":"signin_required","message":`) {
		t.Fatalf("ask with sign-in off: %d %s", code, lastBody)
	}
}

// An app is patched only while its own platform.known-vuln is on: once the org turns the rule off for an app with
// no addition of its own, its open patch approval reads outdated, approving it is refused, and nothing is asked
// again (contracts/routes.md, the preferences row; the security review of #2572).
func TestAnAppsRuleOffOutdatesItsPatchApproval(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	at := base + approvalsAt + patchCode(t, ctx, pool)
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/policies/platform.known-vuln?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("platform.known-vuln off: %d %s", code, lastBody)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "outdated" {
		t.Fatalf("the app's rule off: %s", lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 409 || lastBody != patchOutdatedBody ||
		count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 1 {
		t.Fatalf("approving it once the app's rule is off: %d %s", got, lastBody)
	}
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("the app's rule off, the tick filed %d", n)
	}
}

// An owner's stricter addition turns platform.known-vuln on for their app alone while the org's is off: Canyon
// patches that app, and the org's switch reads ask as saved — never off for the org's rule (contracts/routes.md,
// the preferences row; the security review of #2572).
func TestAnAppsAdditionPatchesThatApp(t *testing.T) {
	pool, ctx, base, browser, _, deploy := approvalHost(t, false)
	if promoted(t, browser, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	pipe.Patches.Store(true)
	t.Cleanup(func() { pipe.Patches.Store(false) })
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/projects/claims-portal/policies/platform.known-vuln?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the app's addition: %d %s", code, lastBody)
	}
	if got := getPreferences(t, browser, base)["patching"]; got != "ask" {
		t.Fatalf("the org's switch reads as saved: %v", got)
	}
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if n := filePatches(t, ctx); n != 1 {
		t.Fatalf("the app whose own rule is on is asked for: filed %d", n)
	}
}

// The admin's CLI is refused patching before its body is read: a value the PUT would refuse is still 403
// dashboard_required, with one denied row (design/PERMISSIONS.md, Platform patches).
func TestACLIPatchingPutIsRefusedBeforeItsBody(t *testing.T) {
	pool, ctx, base, _, cli, _ := approvalHost(t, false)
	if code, _ := putPreferences(t, cli, base, `{"patching":"auto"}`); code != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'") != 1 {
		t.Fatalf("the admin's CLI with a bad body: %d %s", code, lastBody)
	}
}
