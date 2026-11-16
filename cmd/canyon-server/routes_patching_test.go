//go:build !noee

package main

import (
	"net/http"
	"strings"
	"testing"
)

const (
	identitiesAt  = "/api/v1/patching/identities"
	approveReason = `{"reason":"libssl3 fixes CVE-2024-5535; the canary ran clean"}`
)

// The identities read, the fleet approval and a resume are an admin's, and each change is made from a browser
// session (design/PERMISSIONS.md, Platform patches; contracts/routes.md, the Platform rows): an owner, a contributor,
// a read-only member and one with no access are refused 403 admin_required on all three; the admin's CLI reads the
// identities, and is refused a change 403 dashboard_required with one denied row each, before the body is read. In
// the browser the admin is refused an unknown identity and an empty reason, then approves it once: a second approval
// answers it and writes nothing.
func TestFleetApprovalAdminBrowserOnly(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	at := base + identitiesAt + "/" + patchIdentity
	members := map[string]*http.Client{
		"an owner":           teamMember(t, ctx, pool, base, "sub-olga", "olga@meridian.example", "owner"),
		"a contributor":      teamMember(t, ctx, pool, base, "sub-erin", "erin@meridian.example", "contributor"),
		"a read-only member": teamMember(t, ctx, pool, base, "sub-vic", "vic@meridian.example", "viewer"),
		"no access":          signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false),
	}
	for who, c := range members {
		for _, step := range []struct{ method, url, body string }{{http.MethodGet, base + identitiesAt, ""}, {http.MethodPost, at + "/approve", approveReason}, {http.MethodPost, at + "/resume", ""}} {
			if got, _, _ := do(t, c, step.method, step.url, step.body); got != 403 || lastBody != adminRequired {
				t.Fatalf("%s %s %s: %d %s", who, step.method, step.url, got, lastBody)
			}
		}
	}
	if got, read, _ := do(t, cli, http.MethodGet, base+identitiesAt, ""); got != 200 {
		t.Fatalf("the admin's CLI reads the identities: %d %s", got, lastBody)
	} else {
		patchShape(t, "identities_read", read)
	}
	for _, step := range []struct{ verb, body, refused string }{
		{"approve", "{", `{"error":"dashboard_required","message":"approve it in Canyon's dashboard"}`},
		{"resume", "", `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}`},
	} {
		if got, _, _ := do(t, cli, http.MethodPost, at+"/"+step.verb, step.body); got != 403 || lastBody != step.refused {
			t.Fatalf("the admin's CLI %ss: %d %s", step.verb, got, lastBody)
		}
		if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required' AND detail->>'action' = 'POST /api/v1/patching/identities/{id}/"+step.verb+"'"); n != 1 {
			t.Fatalf("one denied row for the CLI's %s, got %d", step.verb, n)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM patch_identities WHERE approved_at IS NOT NULL"); n != 0 {
		t.Fatalf("a refusal approved %d identities", n)
	}
	if got, _, _ := do(t, browser, http.MethodPost, base+identitiesAt+"/sha256:"+strings.Repeat("0a", 32)+"/approve", approveReason); got != 404 || lastBody != `{"error":"not_found","message":"no platform identity has that id"}` {
		t.Fatalf("an unknown identity: %d %s", got, lastBody)
	}
	for _, body := range []string{`{"reason":""}`, `{"reason":"two\nlines"}`, `{}`} {
		if got, refused, _ := do(t, browser, http.MethodPost, at+"/approve", body); got != 400 || refused["error"] != "invalid_reason" {
			t.Fatalf("reason %s: %d %s", body, got, lastBody)
		}
	}
	got, approved, _ := do(t, browser, http.MethodPost, at+"/approve", approveReason)
	ident, _ := approved["identity"].(map[string]any)
	if got != 200 || ident["id"] != patchIdentity || ident["approved"] == nil {
		t.Fatalf("the admin approves it in the browser: %d %s", got, lastBody)
	}
	patchShape(t, "identity", ident)
	if by := ident["approved"].(map[string]any); by["reason"] != "libssl3 fixes CVE-2024-5535; the canary ran clean" || string(mustJSON(by["by"])) != `{"display":"sub-dana","kind":"person","subject":"sub-dana"}` {
		t.Fatalf("the approval says who and why: %s", lastBody)
	}
	if got, again, _ := do(t, browser, http.MethodPost, at+"/approve", `{"reason":"once more"}`); got != 200 || string(mustJSON(again)) != string(mustJSON(approved)) {
		t.Fatalf("approving an approved identity answers it: %d %s", got, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve patch identity' AND actor_subject = 'sub-dana' AND via = 'dashboard' AND detail = jsonb_build_object('identity', '"+patchIdentity+"', 'run_image', 'canyon-run:noble', 'reason', 'libssl3 fixes CVE-2024-5535; the canary ran clean')"); n != 1 {
		t.Fatalf("one approve patch identity row, got %d", n)
	}
}

// With sign-in off nobody can decide a patch approval, so the Platform rows are not served.
func TestPatchingIdentitiesNeedSignIn(t *testing.T) {
	_, _, base, c := withProject(t)
	for _, step := range []struct{ method, path string }{{http.MethodGet, ""}, {http.MethodPost, "/" + patchIdentity + "/approve"}, {http.MethodPost, "/" + patchIdentity + "/resume"}} {
		if got, _, _ := do(t, c, step.method, base+identitiesAt+step.path, approveReason); got != 404 {
			t.Fatalf("%s %s with sign-in off: %d %s", step.method, step.path, got, lastBody)
		}
	}
}

// The fleet approval decides each open patch approval of its identity, at once and then as each comes due, through the
// approve route's checks under its lock and with no promote_anyway (contracts/routes.md, Production approvals): even
// while the org has a person approve production in the browser, the admin's approval there decides it. Approved, it is
// decided_by the admin and its promote run is the admin's, with one approve promotion row, actor the admin, via canyon,
// naming the identity. A refusal — a run of the app in the way — leaves it pending and writes nothing, and the next
// tick decides it; a held production files and decides nothing.
func TestFleetApprovalDecidesAsItComesDue(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true"); err != nil {
		t.Fatal(err)
	}
	patched := passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	first := patchCode(t, ctx, pool)
	if got, _, _ := do(t, browser, http.MethodPost, base+identitiesAt+"/"+patchIdentity+"/approve", approveReason); got != 200 {
		t.Fatalf("the fleet approval: %d %s", got, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request promotion'") + count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE kind = 'promotion'"); n != 0 {
		t.Fatalf("a fleet decision never files a person's approval: %d rows", n)
	}
	var run string
	if err := pool.QueryRow(ctx, "SELECT run_id FROM promotion_approvals WHERE code = $1 AND state = 'approved' AND decided_by_kind = 'person' AND decided_by_subject = 'sub-dana'", first).Scan(&run); err != nil {
		t.Fatalf("the open patch approval is decided by the admin at once: %v", err)
	}
	if waitVerdict(t, browser, base, run)["state"] != "clean" || fake.Ran().Target != "prod" || fake.Ran().Image != patched {
		t.Fatalf("its promotion ships preview's patched image: %s, ran %+v", lastBody, fake.Ran())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+run+"' AND kind = 'promote' AND started_by_subject = 'sub-dana'"); n != 1 {
		t.Fatal("the promote run is the admin's")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve promotion' AND run_id = '"+run+"' AND actor_subject = 'sub-dana' AND via = 'canyon' AND detail->>'identity' = '"+patchIdentity+"' AND detail ?& array['package', 'gate_run', 'code', 'requested_by']"); n != 1 {
		t.Fatalf("one approve promotion row, actor the admin, via canyon, naming the identity: %d", n)
	}
	// a later patch of the identity comes due: a run in the way refuses its decision, which stays pending, and the next tick decides it
	passedPatch(t, ctx, pool, deploy, "gr_patchb", "8c")
	second := patchCode(t, ctx, pool)
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) SELECT 'gr_busy00000', project_id, package_id, 'running', 'promote' FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE code = '"+second+"' AND state = 'pending'") != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve promotion'") != 1 {
		t.Fatalf("a refused decision leaves it pending and writes nothing: %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 1 {
		t.Fatalf("the next tick decides it: %d, %v", n, err)
	}
	if err := pool.QueryRow(ctx, "SELECT run_id FROM promotion_approvals WHERE code = $1 AND state = 'approved' AND decided_by_subject = 'sub-dana'", second).Scan(&run); err != nil || waitVerdict(t, browser, base, run)["state"] != "clean" {
		t.Fatalf("decided by the admin: %v %s", err, lastBody)
	}
	// a held production is never asked, so the fleet approval decides nothing there
	if got, _, _ := do(t, browser, http.MethodPost, base+holdPath, `{"reason":"incident 42"}`); got != 204 {
		t.Fatalf("hold: %d %s", got, lastBody)
	}
	passedPatch(t, ctx, pool, deploy, "gr_patchc", "9d")
	if filed := filePatches(t, ctx); filed != 0 {
		t.Fatalf("a held production is asked: %d", filed)
	}
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 {
		t.Fatalf("a held production is decided: %d, %v", n, err)
	}
}

// The fleet approval never expires, yet decides only while the person who gave it is still an admin (contracts/routes.md,
// the fleet approval): once they are demoted, the next due app's patch approval stays pending for its team and nothing is
// written; made an admin again, the next tick decides it as them.
func TestFleetApprovalNeedsAStillAdmin(t *testing.T) {
	pool, ctx, base, _, _, deploy := patchHost(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET admin_emails = '{erin@meridian.example}'"); err != nil {
		t.Fatal(err)
	}
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if got, _, _ := do(t, erin, http.MethodPost, base+identitiesAt+"/"+patchIdentity+"/approve", approveReason); got != 200 {
		t.Fatalf("an admin's fleet approval: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET admin_emails = '{}'"); err != nil {
		t.Fatal(err)
	}
	code := patchCode(t, ctx, pool)
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE code = '"+code+"' AND state = 'pending'") != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve promotion'") != 0 {
		t.Fatalf("a demoted admin's fleet approval decides nothing: %d, %v", n, err)
	}
	// a sign-in of theirs that claimed admin, minted through another issuer, is no sign-in here (#2306): nothing is decided
	if _, err := pool.Exec(ctx, `UPDATE org_preferences SET admin_claim = 'canyon_role', admin_value = 'admin';
		INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, email_verified, groups_read, claimed_admin, flow, issuer, expires_at)
		VALUES ('elsewhere-session', 'person', 'sub-erin', 'sub-erin', 'erin@meridian.example', true, 'ok', true, 'browser', 'https://elsewhere.example', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE code = '"+code+"' AND state = 'pending'") != 1 {
		t.Fatalf("another issuer's sign-in claiming admin decides nothing: %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM sessions WHERE id = 'elsewhere-session'"); err != nil {
		t.Fatal(err)
	}
	// an admin again, but with no address recorded for the approval: nothing to judge them by, so nothing is decided
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET admin_emails = '{erin@meridian.example}'; UPDATE patch_identities SET approved_by_email = ''"); err != nil {
		t.Fatal(err)
	}
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 0 || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE code = '"+code+"' AND state = 'pending'") != 1 {
		t.Fatalf("an approval with no address decides nothing: %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE patch_identities SET approved_by_email = 'Erin@Meridian.example'"); err != nil { // the admin list is compared case-insensitively
		t.Fatal(err)
	}
	var run string
	if n, err := pipe.Patcher.DecideFleet(ctx, pipe, ""); err != nil || n != 1 ||
		pool.QueryRow(ctx, "SELECT run_id FROM promotion_approvals WHERE code = $1 AND state = 'approved' AND decided_by_subject = 'sub-erin'", code).Scan(&run) != nil {
		t.Fatalf("an admin again, the next tick decides it as them: %d, %v", n, err)
	}
	if waitVerdict(t, erin, base, run)["state"] != "clean" {
		t.Fatalf("its promotion: %s", lastBody)
	}
}

// A resume clears a stalled or paused identity (contracts/routes.md, the Platform rows): the apps that reverted stay
// out, and its breaker counts afresh, so one more revert after it stalls the identity again rather than pausing it.
// One resume row each; an identity neither paused nor stalled is 409 not_paused, an unknown one 404.
func TestResumeIdentity(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if got, _, _ := do(t, browser, http.MethodPost, base+"/api/v1/projects", `{"id":"intake-forms"}`); got != 201 {
		t.Fatalf("create: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome) VALUES ($1, 'intake-forms', 'canyon/intake-forms@sha256:'||repeat('1a', 32), 1, 'running')", patchIdentity); err != nil {
		t.Fatal(err)
	}
	at := base + identitiesAt + "/" + patchIdentity
	read := func() map[string]any {
		t.Helper()
		_, all, _ := do(t, browser, http.MethodGet, base+identitiesAt, "")
		patchShape(t, "identities_read", all)
		list, _ := all["identities"].([]any)
		if len(list) != 1 {
			t.Fatalf("one identity: %s", lastBody)
		}
		return list[0].(map[string]any)
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/resume", ""); got != 409 || lastBody != `{"error":"not_paused","message":"this platform identity is neither paused nor stalled"}` {
		t.Fatalf("resuming a running identity: %d %s", got, lastBody)
	}
	if err := pipe.RecordPatchOutcome(ctx, patchIdentity, "claims-portal", "reverted"); err != nil {
		t.Fatal(err)
	}
	if ident := read(); string(mustJSON(ident["stalled"].(map[string]any)["projects"])) != `["claims-portal"]` || ident["paused"] != nil || ident["apps"].(map[string]any)["reverted"] != float64(1) {
		t.Fatalf("one revert stalls it: %s", mustJSON(ident))
	}
	got, resumed, _ := do(t, browser, http.MethodPost, at+"/resume", "")
	if ident, _ := resumed["identity"].(map[string]any); got != 200 || ident["stalled"] != nil || ident["paused"] != nil {
		t.Fatalf("resume clears a stalled identity: %d %s", got, lastBody)
	}
	if err := pipe.RecordPatchOutcome(ctx, patchIdentity, "intake-forms", "failed_start"); err != nil {
		t.Fatal(err)
	}
	if ident := read(); ident["paused"] != nil || string(mustJSON(ident["stalled"].(map[string]any)["projects"])) != `["intake-forms"]` {
		t.Fatalf("the breaker counts afresh after a resume: %s", mustJSON(ident))
	}
	if err := pipe.RecordPatchOutcome(ctx, patchIdentity, "claims-portal", "reverted"); err != nil {
		t.Fatal(err)
	}
	if ident := read(); ident["paused"] == nil || ident["stalled"] != nil || ident["apps"].(map[string]any)["reverted"] != float64(1) || ident["apps"].(map[string]any)["held"] != float64(1) {
		t.Fatalf("a second since pauses it, and the pause clears the stall (#2595): %s", mustJSON(ident))
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/resume", ""); got != 200 {
		t.Fatalf("resume clears a paused identity: %d %s", got, lastBody)
	}
	if ident := read(); ident["paused"] != nil || ident["stalled"] != nil || ident["wave"] != float64(0) {
		t.Fatalf("resumed, its waves go on from the one it stopped in: %s", mustJSON(ident))
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'resume patching' AND actor_subject = 'sub-dana' AND detail = jsonb_build_object('identity', '"+patchIdentity+"', 'run_image', 'canyon-run:noble')"); n != 2 {
		t.Fatalf("one resume row each, got %d", n)
	}
	if got, _, _ := do(t, browser, http.MethodPost, base+identitiesAt+"/sha256:"+strings.Repeat("0a", 32)+"/resume", ""); got != 404 {
		t.Fatalf("an unknown identity: %d %s", got, lastBody)
	}
}

// An identity's apps are counted by the state each app's environments read, so an app deleted counts in none: its
// patch_wave_apps rows stay, and paused keeps the names that paused it (contracts/routes.md, the identities row; #2596).
func TestPatchingIdentitiesCountNoDeletedApp(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if got, _, _ := do(t, browser, http.MethodPost, base+"/api/v1/projects", `{"id":"intake-forms"}`); got != 201 {
		t.Fatalf("create: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome) VALUES ($1, 'intake-forms', 'canyon/intake-forms@sha256:'||repeat('1a', 32), 1, 'running')", patchIdentity); err != nil {
		t.Fatal(err)
	}
	for _, end := range []struct{ app, outcome string }{{"claims-portal", "reverted"}, {"intake-forms", "failed_start"}} {
		if err := pipe.RecordPatchOutcome(ctx, patchIdentity, end.app, end.outcome); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		_, all, _ := do(t, browser, http.MethodGet, base+identitiesAt, "")
		patchShape(t, "identities_read", all)
		return all["identities"].([]any)[0].(map[string]any)
	}
	if apps := read()["apps"].(map[string]any); apps["reverted"] != float64(1) || apps["held"] != float64(1) {
		t.Fatalf("the paused identity's apps: %s", mustJSON(apps))
	}
	if got, _, _ := do(t, browser, http.MethodDelete, base+"/api/v1/projects/intake-forms?confirm=intake-forms", ""); got != 204 {
		t.Fatalf("delete: %d %s", got, lastBody)
	}
	ident := read()
	if apps := ident["apps"].(map[string]any); apps["reverted"] != float64(1) || apps["held"] != float64(0) || string(mustJSON(ident["paused"].(map[string]any)["projects"])) != `["claims-portal","intake-forms"]` {
		t.Fatalf("a deleted app is counted, or its name left the pause: %s", mustJSON(ident))
	}
}

// The identities read says what the newest tick's patch plan said, at the read's own level (contracts/routes.md, the
// identities row's plan; #2588): null before any tick planned; then, to an admin, the identity it planned onto and each
// app it read with its cause — an app whose run image fixes nothing among them — sorted, an app no longer there left
// out. A contributor of the app is refused the read 403 admin_required, as before, and reads no cause.
func TestPatchingIdentitiesSayWhyAnAppWasNotPlanned(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	read := func() map[string]any {
		t.Helper()
		got, all, _ := do(t, browser, http.MethodGet, base+identitiesAt, "")
		if got != 200 {
			t.Fatalf("the admin's identities read: %d %s", got, lastBody)
		}
		patchShape(t, "identities_read", all)
		return all
	}
	if plan, ok := read()["plan"]; !ok || plan != nil {
		t.Fatalf("before any tick planned, plan is %v (%v), want null", plan, ok)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO patch_plan_said (said_at, identity, apps) VALUES ('2026-10-29T14:40:00Z', $1, '{"claims-portal": "no_fix", "gone": "wave"}')`, patchIdentity); err != nil {
		t.Fatal(err)
	}
	if plan := string(mustJSON(read()["plan"])); plan != `{"apps":[{"project":"claims-portal","why":"no_fix"}],"at":"2026-10-29T14:40:00Z","identity":"`+patchIdentity+`"}` {
		t.Fatalf("the admin's plan: %s", plan)
	}
	if _, err := pool.Exec(ctx, `UPDATE patch_plan_said SET identity = NULL, why = 'org_off', apps = '{}'`); err != nil {
		t.Fatal(err)
	}
	if plan := string(mustJSON(read()["plan"])); plan != `{"apps":[],"at":"2026-10-29T14:40:00Z","identity":null,"why":"org_off"}` {
		t.Fatalf("a plan that read no app: %s", plan)
	}
	erin := teamMember(t, ctx, pool, base, "sub-erin", "erin@meridian.example", "contributor")
	if got, _, _ := do(t, erin, http.MethodGet, base+identitiesAt, ""); got != 403 || lastBody != adminRequired {
		t.Fatalf("a contributor's identities read: %d %s", got, lastBody)
	}
}
