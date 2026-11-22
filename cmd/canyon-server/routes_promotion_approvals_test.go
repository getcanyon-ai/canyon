package main

import (
	"context"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cliIn is a client holding a live CLI session — the device flow's row — for the person, as canyon login leaves one.
func cliIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, sub, email string) *http.Client {
	t.Helper()
	id, err := token()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, email, flow, expires_at) VALUES ($1, 'person', $2, $2, $3, 'device', now() + interval '1 hour')", id, sub, email); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	u, _ := neturl.Parse(base)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: id}}) //nolint:gosec // test: the row just inserted
	return c
}

// approvalHost is a host with sign-in on where dana (an admin) runs a clean preview of claims-portal from her
// browser, and holds a CLI session too; on says whether production is approved in the dashboard.
func approvalHost(t *testing.T, on bool) (pool *pgxpool.Pool, ctx context.Context, base string, browser, cli *http.Client, deploy string) {
	t.Helper()
	withOIDC(t) // CANYON_ADMINS is dana
	cleanGate(t)
	appAnswersOK(t)
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	browser = signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	cli = cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	if code, _, _ := do(t, browser, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	upload(t, browser, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, browser, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	deploy, _ = runOf(started)["id"].(string)
	if waitVerdict(t, browser, base, deploy)["state"] != "clean" {
		t.Fatalf("the clean deploy: %s", lastBody)
	}
	pipe.Gate = runstest.Golden
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = $1", on); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, browser, cli, deploy
}

// humanApproval is the receipt's prod.human-approval row on the status read.
func humanApproval(t *testing.T, c *http.Client, base string) map[string]any {
	t.Helper()
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	for _, r := range status["receipt"].(map[string]any)["rules"].([]any) {
		if row := r.(map[string]any); row["id"] == "prod.human-approval" {
			return row
		}
	}
	t.Fatalf("no prod.human-approval row: %s", lastBody)
	return nil
}

// askApproval is the CLI's promotion of run while production is approved in the dashboard: the code it files.
func askApproval(t *testing.T, cli *http.Client, base, run string) string {
	t.Helper()
	got, body, _ := do(t, cli, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
	approval, _ := body["approval"].(map[string]any)
	code, _ := approval["code"].(string)
	if got != 202 || code == "" {
		t.Fatalf("the CLI asks: %d %s", got, lastBody)
	}
	return code
}

const (
	whyOff = "a person promotes, or an agent they ask, signed in as them"
	whyOn  = "a person approves it in Canyon's dashboard; an agent or the CLI only asks"
)

var shownCode = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

// Off, the CLI promotes as it always did, and the receipt says so rather than claiming agents can't.
func TestWithProductionApprovalOffTheCLIPromotes(t *testing.T) {
	_, _, base, _, cli, deploy := approvalHost(t, false)
	if row := humanApproval(t, cli, base); row["passed"] != true || row["blocks"] != true || row["why"] != whyOff {
		t.Fatalf("off, the CLI's receipt: %s", mustJSON(row))
	}
	if run := promoted(t, cli, base, deploy); run["state"] != "clean" {
		t.Fatalf("off, the CLI promotes: %s", mustJSON(run))
	}
}

// On, the CLI's promotion is a request: an approval with its link, no run, one row via the CLI; the next
// request replaces it. The receipt says why to every reader, and only the browser's passes.
func TestWithProductionApprovalOnTheCLIFilesAnApprovalAndPromotesNothing(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	if row := humanApproval(t, cli, base); row["passed"] != false || row["blocks"] != true || row["why"] != whyOn {
		t.Fatalf("on, the CLI's receipt: %s", mustJSON(row))
	}
	if row := humanApproval(t, browser, base); row["passed"] != true || row["why"] != whyOn {
		t.Fatalf("on, the browser's receipt: %s", mustJSON(row))
	}
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/policies/prod.human-approval?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("the rule off: %d %s", code, lastBody)
	}
	if row := humanApproval(t, cli, base); row["blocks"] != true || row["passed"] != false {
		t.Fatalf("the switch holds whatever the rule's state: %s", mustJSON(row))
	}
	ask := func() map[string]any {
		code, body, _ := do(t, cli, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
		approval, _ := body["approval"].(map[string]any)
		if code != 202 || len(body) != 1 || len(approval) != 3 || !shownCode.MatchString(approval["code"].(string)) ||
			approval["url"] != "http://localhost:7777/#/approve?code="+approval["code"].(string) {
			t.Fatalf("the CLI asks: %d %s", code, lastBody)
		}
		at, err := time.Parse(time.RFC3339, approval["expires_at"].(string))
		if err != nil || time.Until(at) < 23*time.Hour || time.Until(at) > 24*time.Hour {
			t.Fatalf("expires in 24 hours: %s", lastBody)
		}
		return approval
	}
	first := ask()
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'"); n != 0 {
		t.Fatalf("a request started %d promote runs", n)
	}
	asked := "SELECT count(*) FROM audit_events WHERE action = 'request promotion' AND via = 'cli' AND project_id = 'claims-portal' AND detail->>'gate_run' = '" + deploy + "' AND detail->>'code' = '" + first["code"].(string) + "' AND detail ? 'package' AND detail->>'expires_at' = '" + first["expires_at"].(string) + "' AND NOT detail ? 'replaced'"
	if n := count(t, ctx, pool, asked); n != 1 {
		t.Fatalf("one request row via the CLI, got %d", n)
	}
	second := ask()
	if code, _, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/"+first["code"].(string), ""); code != 404 {
		t.Fatalf("a replaced code reads 404: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request promotion' AND detail->>'replaced' = '"+first["code"].(string)+"' AND detail->>'code' = '"+second["code"].(string)+"'"); n != 1 {
		t.Fatalf("the second request names the one it replaced: %d", n)
	}
	if code, body, _ := do(t, browser, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); code != 202 || runOf(body)["id"] == nil {
		t.Fatalf("the dashboard's Promote button still promotes: %d %s", code, lastBody)
	}
}

// Only a browser session decides: the CLI that asked is refused before the code is read, with a denied row,
// and the approval is untouched.
func TestOnlyABrowserSessionDecidesAnApproval(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	code := askApproval(t, cli, base, deploy)
	for _, verb := range []string{"approve", "deny"} {
		got, _, _ := do(t, cli, http.MethodPost, base+"/api/v1/promotion-approvals/"+code+"/"+verb, "")
		if got != 403 || lastBody != `{"error":"dashboard_required","message":"approve it in Canyon's dashboard"}` {
			t.Fatalf("the CLI %ss: %d %s", verb, got, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'"); n != 2 {
		t.Fatalf("two denied rows, got %d", n)
	}
	_, read, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/"+code, "")
	if read["state"] != "pending" || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatalf("untouched: %s", lastBody)
	}
}

// The page's read names who asked and from where, what goes live, and who is approving; approving is the
// Promote button, as the approver, and the approval keeps its run. A second click finds nothing pending.
func TestApprovingInTheBrowserRunsThePromotionAsTheApprover(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_display = 'Dana Whitfield' WHERE flow = 'browser'"); err != nil {
		t.Fatal(err)
	}
	code := strings.ToLower(strings.ReplaceAll(askApproval(t, cli, base, deploy), "-", "")) // read as a device code is
	at := base + "/api/v1/promotion-approvals/" + code
	_, read, _ := do(t, browser, http.MethodGet, at, "")
	if read["app"] != "claims-portal" || read["state"] != "pending" || read["requested_via"] != "cli" || read["replaces"] != nil ||
		read["requested_by"].(map[string]any)["subject"] != "sub-dana" || read["receipt"] == nil || read["run"] != nil ||
		string(mustJSON(read["approver"])) != `{"display":"Dana Whitfield","email":"dana@meridian.example"}` || !shownCode.MatchString(read["code"].(string)) {
		t.Fatalf("the page's read: %s", lastBody)
	}
	if _, poll, _ := do(t, cli, http.MethodGet, at, ""); poll["state"] != "pending" || poll["receipt"] != nil {
		t.Fatalf("the CLI's poll carries no receipt: %s", lastBody)
	}
	got, started, _ := do(t, browser, http.MethodPost, at+"/approve", "")
	run, _ := runOf(started)["id"].(string)
	if got != 202 || run == "" || waitVerdict(t, browser, base, run)["state"] != "clean" {
		t.Fatalf("approve: %d %s", got, lastBody)
	}
	_, read, _ = do(t, browser, http.MethodGet, at, "")
	if read["state"] != "approved" || read["run"] != run || read["decided_by"].(map[string]any)["display"] != "Dana Whitfield" || read["decided_at"] == nil {
		t.Fatalf("approved, naming its run: %s", lastBody)
	}
	approved := "SELECT count(*) FROM audit_events WHERE action = 'approve promotion' AND via = 'dashboard' AND run_id = '" + run + "' AND detail->>'gate_run' = '" + deploy + "' AND detail->'requested_by'->>'subject' = 'sub-dana'"
	if count(t, ctx, pool, approved) != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/promotions'") != 0 {
		t.Fatalf("one approve row in place of the promotion's")
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 404 {
		t.Fatalf("a second click: %d %s", got, lastBody)
	}
}

func TestDenyingAnApprovalPromotesNothing(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	at := base + "/api/v1/promotion-approvals/" + askApproval(t, cli, base, deploy)
	if got, _, _ := do(t, browser, http.MethodPost, at+"/deny", ""); got != 204 {
		t.Fatalf("deny: %d %s", got, lastBody)
	}
	if _, read, _ := do(t, cli, http.MethodGet, at, ""); read["state"] != "denied" || read["decided_by"] == nil {
		t.Fatalf("denied: %s", lastBody)
	}
	if count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'deny promotion' AND via = 'dashboard' AND detail->>'gate_run' = '"+deploy+"'") != 1 {
		t.Fatal("one deny row")
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 404 || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatalf("approve after deny: %d %s", got, lastBody)
	}
}

// A person approves the version the page showed: once preview runs another, the approval is outdated and
// starts nothing, though it can still be denied; past its 24 hours it is expired and gone.
func TestAnApprovalNamesTheVersionItWasAskedFor(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	at := base + "/api/v1/promotion-approvals/" + askApproval(t, cli, base, deploy)
	if _, err := pool.Exec(ctx, "UPDATE deploys SET image = image || '-other' WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "outdated" || read["held"] != nil || read["rollback"] != nil { // a promotion of preview's, not a put-back
		t.Fatalf("outdated by preview, no hold: %s", lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 409 || lastBody != `{"error":"outdated","message":"preview runs another version now: ask again from the CLI"}` {
		t.Fatalf("approving an outdated approval: %d %s", got, lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/deny", ""); got != 204 {
		t.Fatalf("an outdated approval is denied: %d %s", got, lastBody)
	}
	at = base + "/api/v1/promotion-approvals/" + askApproval(t, cli, base, deploy)
	if _, err := pool.Exec(ctx, "UPDATE promotion_approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "expired" {
		t.Fatalf("expired: %s", lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 404 || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatalf("approving an expired approval: %d %s", got, lastBody)
	}
}

// An approval is at the promotions route's level: a code nobody has is 404, a reader below contributor is
// refused as the app's routes refuse them, and a deleted app takes its approvals with it.
func TestAnApprovalIsReadAndDecidedByTheAppsTeam(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	at := base + "/api/v1/promotion-approvals/" + askApproval(t, cli, base, deploy)
	if got, _, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/ZZZZ-ZZZZ", ""); got != 404 {
		t.Fatalf("no such code: %d %s", got, lastBody)
	}
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', 'erin@meridian.example', 'viewer', 'person', 'sub-dana', 'dana')`); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ method, path string }{{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"}} {
		if got, refused, _ := do(t, erin, step.method, at+step.path, ""); got != 403 || refused["error"] != "team_required" {
			t.Fatalf("a viewer %s %s: %d %s", step.method, step.path, got, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE project_access SET level = 'contributor' WHERE grantee = 'erin@meridian.example'"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := do(t, erin, http.MethodGet, at, ""); got != 200 {
		t.Fatalf("a contributor reads it: %d %s", got, lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); got != 204 && got != 202 {
		t.Fatalf("delete: %d %s", got, lastBody)
	}
	if count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals") != 0 {
		t.Fatal("a deleted app keeps no approval")
	}
}

// The switch is the org's: off by default, changed only from a browser session — an admin's CLI session could
// otherwise turn it off and promote — on the trail with its before and after, and never on without sign-in.
func TestTheProductionApprovalSwitch(t *testing.T) {
	pool, ctx, base, browser, cli, _ := approvalHost(t, false)
	if got := getPreferences(t, browser, base)["prod_approval"]; got != false {
		t.Fatalf("off by default: %v", got)
	}
	if code, _ := putPreferences(t, cli, base, `{"prod_approval":true}`); code != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` {
		t.Fatalf("from the CLI: %d %s", code, lastBody)
	}
	if code, out := putPreferences(t, browser, base, `{"prod_approval":true}`); code != 200 || out["prod_approval"] != true {
		t.Fatalf("from the browser: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail = '{"prod_approval":{"before":false,"after":true}}'::jsonb`); n != 1 {
		t.Fatalf("one row with its before and after, got %d", n)
	}
	if code, _ := putPreferences(t, browser, base, `{"prod_approval":"yes"}`); code != 400 {
		t.Fatalf("a boolean: %d %s", code, lastBody)
	}
	if _, m := skillManifest(t, cli, base); m["prod_approval"] != true {
		t.Fatalf("the manifest says it: %s", lastBody)
	}
}

func TestWithSignInOffProductionApprovalIsNeverOn(t *testing.T) {
	_, _, base, c := withProject(t)
	if got := getPreferences(t, c, base)["prod_approval"]; got != false {
		t.Fatalf("off: %v", got)
	}
	if code, _ := putPreferences(t, c, base, `{"prod_approval":true}`); code != 409 || !strings.HasPrefix(lastBody, `{"error":"signin_required","message":`) {
		t.Fatalf("turning it on with sign-in off: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/promotion-approvals/ABCD-EFGH", ""); code != 404 {
		t.Fatalf("not routed: %d %s", code, lastBody)
	}
}

// No page frames the dashboard: a preview under its registrable domain could otherwise put the device or the
// approval page under a decoy for a signed-in person to click.
func TestTheDashboardIsNeverFramed(t *testing.T) {
	rec, _ := openGroup(t, bundle, http.MethodGet, "/")
	if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("the dashboard's headers: %d %v", rec.Code, rec.Header())
	}
}

// The edges of a decision: an expired request is replaced and named; a contributor's approve refused for a
// run in the way leaves the approval pending and writes nothing; after the switch is turned off it can still
// be approved; an unknown code is the contract's 404; and with nothing in preview nothing is filed.
func TestAnApprovalsEdges(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	first := askApproval(t, cli, base, deploy)
	if _, err := pool.Exec(ctx, "UPDATE promotion_approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	second := askApproval(t, cli, base, deploy)
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request promotion' AND detail->>'replaced' = '"+first+"'"); n != 1 {
		t.Fatalf("an expired request it replaced is named too: %d", n)
	}
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', 'erin@meridian.example', 'contributor', 'person', 'sub-dana', 'dana')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) SELECT 'gr_busy00000', project_id, package_id, 'running', 'promote' FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	at := base + "/api/v1/promotion-approvals/" + second
	got, body, _ := do(t, erin, http.MethodPost, at+"/approve", "")
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	if got != 409 || body["error"] != "run_in_progress" || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending'") != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve promotion'") != 0 {
		t.Fatalf("a refused approve changes nothing: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = false"); err != nil {
		t.Fatal(err)
	}
	got, started, _ := do(t, erin, http.MethodPost, at+"/approve", "")
	if run, _ := runOf(started)["id"].(string); got != 202 || waitVerdict(t, erin, base, run)["state"] != "clean" {
		t.Fatalf("a contributor approves after the switch is off: %d %s", got, lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/ZZZZ-ZZZZ", ""); got != 404 || lastBody != `{"error":"not_found","message":"no approval has that code"}` {
		t.Fatalf("an unknown code: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true; DELETE FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if got, body, _ := do(t, cli, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); got != 409 || body["error"] != "nothing_in_preview" || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'pending'") != 0 {
		t.Fatalf("nothing in preview files nothing: %d %s", got, lastBody)
	}
}

// While production is approved in the dashboard, so is a finding (#1824): the CLI's approve, one finding or a
// dependency's, is refused before the body is read, naming the app's page for a person, with one denied row
// each and nothing approved; the browser of an admin who did not start the review approves. Off, the CLI is
// still refused a blocking finding, which only a browser approves (#2204).
func TestWithProductionApprovalOnOnlyTheDashboardApprovesAFinding(t *testing.T) {
	pool, ctx, base, _, cli, _ := approvalHost(t, true)
	run := blockedRun(t, cli, base)
	var fp string // read off the verdict, never written into a test: a fingerprint literal is secret-shaped
	if err := pool.QueryRow(ctx, `SELECT f->'partialFingerprints'->>'canyon/v1' FROM timeline_rows t,
		jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f WHERE t.run_id = $1 AND t.kind = 'verdict'`, run).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	terms := `"reason":"Reviewed by hand","expires_at":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `"`
	one, many := base+"/api/v1/gate-runs/"+run+"/findings/"+fp+"/approve", base+"/api/v1/gate-runs/"+run+"/findings/approve"
	want := `{"error":"dashboard_required","message":"While production is approved in Canyon's dashboard, a finding is approved there too. Fix the code and deploy again, or ask a person to approve it in the dashboard: http://localhost:7777/#/apps/claims-portal?run=` + run + `"}`
	for _, tc := range [][2]string{{one, "{" + terms + "}"}, {many, `{"fingerprints":["` + fp + `"],` + terms + "}"}, {one, "{}"}} { // {}: refused before the body is read
		if got, _, _ := do(t, cli, http.MethodPost, tc[0], tc[1]); got != 403 || lastBody != want {
			t.Fatalf("the CLI approves %s: %d %s", tc[0], got, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND project_id = 'claims-portal' AND detail->>'error' = 'dashboard_required'"); n != 3 {
		t.Fatalf("three denied rows, got %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals"); n != 0 {
		t.Fatalf("a refused approval wrote %d rows", n)
	}
	if got, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-sam", "sam@meridian.example", true), http.MethodPost, one, "{"+terms+"}"); got != 200 {
		t.Fatalf("the dashboard approves: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = false"); err != nil {
		t.Fatal(err)
	}
	samCLI := signedIn(t, ctx, pool, base, "sub-sam", "sam@meridian.example", true)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET flow = 'device' WHERE id = (SELECT id FROM sessions WHERE actor_subject = 'sub-sam' ORDER BY created_at DESC LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := do(t, samCLI, http.MethodPost, one, "{"+terms+"}"); got != 403 || lastBody != approveInBrowser {
		t.Fatalf("off, the CLI approves a blocking finding: %d %s", got, lastBody)
	}
}

// While production is approved in the dashboard, so is a policy (#1966): the CLI's change, preview or apply,
// is refused before the body is read, naming the Policies page, with one denied row each and nothing written —
// an admin's agent could otherwise turn a blocking rule off and promote past it. So is what a rule checks: a
// custom rule's instructions, and the buildpacks stack.blessed approves. The browser changes them; off, the CLI
// changes them as it always did.
func TestWithProductionApprovalOnOnlyTheDashboardChangesAPolicy(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	browser := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	cli := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true"); err != nil {
		t.Fatal(err)
	}
	want := `{"error":"dashboard_required","message":"While production is approved in Canyon's dashboard, policies are changed there too: http://localhost:7777/#/guardrails/policies"}`
	for _, tc := range [][2]string{{"?apply=true", `{"state":"off"}`}, {"", `{"state":"off"}`}, {"?apply=true", "{}"}} { // {}: refused before the body is read
		if got, _, _ := do(t, cli, http.MethodPut, base+policyPath+tc[0], tc[1]); got != 403 || lastBody != want {
			t.Fatalf("the CLI changes a policy %s %s: %d %s", tc[0], tc[1], got, lastBody)
		}
	}
	if got, _, _ := do(t, cli, http.MethodPut, base+emailPath, emailBody); got != 403 || lastBody != want {
		t.Fatalf("the CLI writes a custom rule: %d %s", got, lastBody)
	}
	if got, _ := putPreferences(t, cli, base, `{"approved_buildpacks":[]}`); got != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` {
		t.Fatalf("the CLI empties the approved buildpacks: %d %s", got, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND project_id IS NULL AND detail->>'error' = 'dashboard_required'"); n != 5 {
		t.Fatalf("five denied rows, got %d", n)
	}
	if n := policyWrites(t, ctx, pool) + customWrites(t, ctx, pool) + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences'"); n != 0 {
		t.Fatalf("a refused change wrote %d rows", n)
	}
	changes := func(c *http.Client, who string) {
		t.Helper()
		if got, _, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"off"}`); got != 200 {
			t.Fatalf("%s changes a policy: %d %s", who, got, lastBody)
		}
		if got, _, _ := do(t, c, http.MethodPut, base+emailPath, emailBody); got != 200 {
			t.Fatalf("%s writes a custom rule: %d %s", who, got, lastBody)
		}
		if got, _ := putPreferences(t, c, base, `{"approved_buildpacks":[]}`); got != 200 {
			t.Fatalf("%s sets the approved buildpacks: %d %s", who, got, lastBody)
		}
	}
	changes(browser, "the dashboard")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = false"); err != nil {
		t.Fatal(err)
	}
	changes(cli, "off, the CLI")
}

// While production is approved in the dashboard, so is the design system (as the buildpacks, #1966): a
// cleared one leaves design.org-system nothing to check, so a Blocking one passes every app. The CLI's
// preferences PUT naming it and its setup chat turn are refused with one denied row each and nothing
// written; the browser changes it, and with the switch off the CLI does as it always did.
func TestWithProductionApprovalOnOnlyTheDashboardChangesTheDesignSystem(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	withModel(t, &scripted{line: "Done."})
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	browser := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	cli := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true"); err != nil {
		t.Fatal(err)
	}
	want := `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}`
	if got, _ := putPreferences(t, cli, base, `{"design_system_id":null}`); got != 403 || lastBody != want {
		t.Fatalf("the CLI clears the design system: %d %s", got, lastBody)
	}
	if got, _ := postSetup(t, cli, base, oneMessage); got != 403 || lastBody != want {
		t.Fatalf("the CLI's setup chat: %d %s", got, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'"); n != 2 {
		t.Fatalf("two denied rows, got %d", n)
	}
	if got, _ := putPreferences(t, browser, base, `{"design_system_id":null}`); got != 200 {
		t.Fatalf("the dashboard clears the design system: %d %s", got, lastBody)
	}
	if got, _ := postSetup(t, browser, base, oneMessage); got != 200 {
		t.Fatalf("the dashboard's setup chat: %d %s", got, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := putPreferences(t, cli, base, `{"design_system_id":null}`); got != 200 {
		t.Fatalf("off, the CLI clears the design system: %d %s", got, lastBody)
	}
}

// patchHost is approvalHost with production running preview's image, platform.known-vuln on, and this server
// patching (CANYON_PATCHING on, sign-in on): what a patch approval is filed on. An approved patch's promotion
// watches production for a second.
func patchHost(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, base string, browser, cli *http.Client, deploy string) {
	t.Helper()
	pool, ctx, base, browser, cli, deploy = approvalHost(t, false)
	if promoted(t, browser, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/policies/platform.known-vuln?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("platform.known-vuln on: %d %s", code, lastBody)
	}
	pipe.Patches.Store(true)
	pipe.PatchWatch.Store(time.Second)
	t.Cleanup(func() { pipe.Patches.Store(false); pipe.PatchWatch.Store(0) })
	return pool, ctx, base, browser, cli, deploy
}

// passedPatch makes run a patch of the image claims-portal's preview runs now, as a patch run whose watch ran to
// its end leaves it: its verdict deploy's, its patch preview row naming what it fixed, its passed line, its app
// passed in patchIdentity's waves, and preview's row and container the patch's, on the image fill names. It answers
// that image.
func passedPatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deploy, run, fill string) string {
	t.Helper()
	patched := "canyon/claims-portal@sha256:" + strings.Repeat(fill, 32)
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO patch_identities (id, run_image) VALUES ($1, 'canyon-run:noble') ON CONFLICT DO NOTHING`, []any{patchIdentity}},
		{`INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display, author_kind, author_subject, author_display, patch_replaced, watch_until)
			SELECT $1, d.project_id, d.package_id, 'clean', now(), 'patch', 'canyon', 'patch', 'Canyon', 'person', 'sub-dana', 'dana', to_jsonb(d), '2026-10-29T14:35:00Z'
			FROM deploys d WHERE d.project_id = 'claims-portal' AND d.env = 'preview'`, []any{run}},
		{`INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', $1, project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = $2 AND kind IN ('verdict', 'deploy.completed')`, []any{run, deploy}},
		{`INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', $1, 'claims-portal', 'build.log', 'patch:passed', '{"line": "The watch passed: the patched app neither exited nor restarted"}')`, []any{run}},
		{`INSERT INTO audit_events (id, actor_kind, actor_subject, actor_display, action, project_id, run_id, detail) VALUES ('ev_' || substr(md5($1), 1, 12), 'canyon', 'patch', 'Canyon', 'patch preview', 'claims-portal', $1,
			'{"fixed": [{"name": "libssl3", "from": "3.0.13-0ubuntu3.4", "to": "3.0.13-0ubuntu3.5", "vulnerability": "CVE-2024-5535"}]}')`, []any{run}},
		{`INSERT INTO patch_wave_apps (identity, project_id, image, wave, outcome, run) SELECT $1, project_id, image, 0, 'passed', $2 FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'
			ON CONFLICT (identity, project_id) DO UPDATE SET image = EXCLUDED.image, outcome = 'passed', run = EXCLUDED.run`, []any{patchIdentity, run}},
		{`UPDATE deploys SET image = $2, gate_run = $1, build_digest = NULL, values_hmac = NULL, values_key = NULL WHERE project_id = 'claims-portal' AND env = 'preview'`, []any{run, patched}},
	} {
		if _, err := pool.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("%v: %s", err, s.q)
		}
	}
	fake.StartAs("claims-portal", "preview", patched)
	return patched
}
