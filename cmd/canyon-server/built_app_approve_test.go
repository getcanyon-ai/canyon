package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// depsRow is deps.known-vuln's row of the receipt the status read answers.
func depsRow(t *testing.T, c *http.Client, base string) receiptRule {
	t.Helper()
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Fatalf("the status read: %d %s", code, lastBody)
	}
	var read struct{ Receipt receipt }
	if err := json.Unmarshal([]byte(lastBody), &read); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(read.Receipt.Rules, func(r receiptRule) bool { return r.ID == depsRule })
	if i < 0 {
		t.Fatalf("the receipt has no %s row: %s", depsRule, lastBody)
	}
	return read.Receipt.Rules[i]
}

// A package only the built app holds is approved through the dependency's approve, by the fingerprints its row
// lists (#2610): with deps.known-vuln placed blocking before production, the row and the promotion's deploy step
// pass only once every package it counts is approved, the row naming what was accepted; two packages at once are
// not one dependency, a fingerprint the row does not list is not found, and an approval nearer its end than the
// ship margin clears nothing.
func TestDepsImageApprovalClearsTheReceiptBeforeProduction(t *testing.T) {
	pool, ctx, base, c, run := previewed(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing deps.known-vuln blocking before production: %d %s", code, lastBody)
	}
	builtAppAsked(t)
	for _, q := range []string{"DELETE FROM part_matches WHERE part_sha IN (SELECT part_sha FROM image_layers WHERE project_id = 'claims-portal')",
		"DELETE FROM scanned_images WHERE project_id = 'claims-portal'"} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	seedPlatform(t, ctx, pool, loadBuiltAppGolden(t).platformGolden, "claims-portal", executortest.Digest)
	clears := func() bool {
		t.Helper()
		ok, err := pipe.Clears(ctx, "claims-portal", run)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	row := depsRow(t, c, base)
	if row.Passed || len(row.BuiltApp) < 2 || clears() {
		t.Fatalf("a built app holding packages its review didn't see: %+v, clears %v", row, clears())
	}
	approve := func(fps []string) (int, string) {
		t.Helper()
		code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/approve", approveBody(fps...))
		e, _ := body["error"].(string)
		return code, e
	}
	if code, e := approve(slices.Concat(row.BuiltApp[0].Fingerprints, row.BuiltApp[1].Fingerprints)); code != 400 || e != "not_one_dependency" {
		t.Fatalf("two packages at once: %d %s", code, lastBody)
	}
	if code, _ := approve([]string{fmt.Sprintf("f_%064x", 7)}); code != 404 {
		t.Fatalf("a fingerprint the row does not list: %d %s", code, lastBody)
	}
	first := row.BuiltApp[0]
	if code, _ := approve(first.Fingerprints); code != 200 {
		t.Fatalf("approving %s %s: %d %s", first.Name, first.Version, code, lastBody)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/gate-runs/{run}/findings/approve' AND detail->>'tool' = 'canyon/promote-gate'
		AND detail->'dependency' = jsonb_build_object('name', $1::text, 'version', $2::text)`, first.Name, first.Version).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("%d audit rows naming %s %s from the promote receipt (%v)", audited, first.Name, first.Version, err)
	}
	if again := depsRow(t, c, base); again.Passed || clears() || !again.BuiltApp[len(again.BuiltApp)-1].Approved {
		t.Fatalf("one package of %d approved: %+v", len(row.BuiltApp), again)
	}
	for _, p := range row.BuiltApp[1:] {
		if code, _ := approve(p.Fingerprints); code != 200 {
			t.Fatalf("approving %s %s: %d %s", p.Name, p.Version, code, lastBody)
		}
	}
	all := depsRow(t, c, base)
	want := fmt.Sprintf("; its built app's known-vulnerable packages its review didn't see are approved: %s %s and %d more", first.Name, first.Version, len(row.BuiltApp)-1)
	if !all.Passed || !strings.Contains(all.Why, want) || !clears() {
		t.Fatalf("every package approved: %+v, clears %v; want a pass naming %q", all, clears(), want)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() + interval '5 minutes' WHERE rule = $1", depsRule); err != nil {
		t.Fatal(err)
	}
	if clears() {
		t.Fatal("approvals ending within the ship margin clear the promotion")
	}
}

// builtAppHost is an oidc host (approverHost's people: dana the admin, rhea and rex responders, olive the owner, carl a
// contributor, vic read-only) whose preview runs the review olive started, clean, with deps.known-vuln placed blocking
// before production and the built app built-app.json reads: what the receipt lists is approvable, by the review's run.
// With reviewFails the review also found a high deps.known-vuln finding, advisory before preview, which blocks once the
// rule is placed before production: the row's review half fails, so the receipt lists no built_app.
func builtAppHost(t *testing.T, reviewFails bool) (pool *pgxpool.Pool, ctx context.Context, base, run string, dana *http.Client) {
	t.Helper()
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "rex@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	cleanGate(t)
	if reviewFails {
		pipe.Gate = func(ctx context.Context, dir, tree string, snap gatepolicy.Snapshot, report gate.Report) (finding.Log, error) {
			g, err := runstest.Golden(ctx, dir, tree, snap, report)
			if err != nil {
				return g, err
			}
			l, err := runstest.CleanGolden(ctx, dir, tree, snap, report)
			if err != nil {
				return l, err
			}
			cve, advisory := g.Runs[2].Results[0], false // gitleaks' key, as Canyon's own scanners' dependency finding
			cve.RuleID, cve.PartialFingerprints.CanyonV1 = depsRule, "f_"+strings.Repeat("c", 64)
			cve.Properties.Canyon.Blocks, cve.Properties.Canyon.Severity, cve.Properties.Canyon.PolicyName = &advisory, "high", "No known-vulnerable dependencies"
			cve.Properties.Canyon.SecretName, cve.Level = nil, "error"
			l.Runs[0].Results = append(l.Runs[0].Results, cve)
			l.Runs[0].Tool.Driver.Rules = append(l.Runs[0].Tool.Driver.Rules, finding.Rule{ID: depsRule, Name: "No known-vulnerable dependencies"})
			return l, nil
		}
	}
	appAnswersOK(t)
	_, started, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ = runOf(started)["id"].(string)
	if waitVerdict(t, olive, base, run)["state"] != "clean" {
		t.Fatalf("olive's deploy: %s", lastBody)
	}
	dana = signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing deps.known-vuln blocking before production: %d %s", code, lastBody)
	}
	builtAppAsked(t)
	for _, q := range []string{"DELETE FROM part_matches WHERE part_sha IN (SELECT part_sha FROM image_layers WHERE project_id = 'claims-portal')",
		"DELETE FROM scanned_images WHERE project_id = 'claims-portal'"} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	seedPlatform(t, ctx, pool, loadBuiltAppGolden(t).platformGolden, "claims-portal", executortest.Digest)
	return pool, ctx, base, run, dana
}

// listedFingerprints is the fingerprints of name in deps.known-vuln's built_app on the receipt c reads.
func listedFingerprints(t *testing.T, c *http.Client, base, name string) []string {
	t.Helper()
	row := depsRow(t, c, base)
	i := slices.IndexFunc(row.BuiltApp, func(p builtAppPackage) bool { return p.Name == name })
	if i < 0 {
		t.Fatalf("the receipt lists no %s: %+v", name, row)
	}
	return row.BuiltApp[i].Fingerprints
}

// A package only the built app holds blocks as a blocking finding does while deps.known-vuln is placed blocking
// before production, and is approved as one is (design/PERMISSIONS.md, Approve findings; #2610), on the review the
// receipt is read for: the owner and a contributor are approver_required, read-only and no access team_required, an
// admin from the CLI dashboard_required, an agent person_required; an admin is refused the review she started, a
// review that recorded no author is approved by nobody, and a push's, while the org asks approvers of a push to
// link GitHub, by nobody who didn't. Each refusal is one denied row and approves nothing. An admin who didn't start
// it approves from a browser, and so does a responder on no team; the approval covers until it expires.
func TestDepsImageApprovalIsAnAdminsOrAResponders(t *testing.T) {
	pool, ctx, base, run, dana := builtAppHost(t, false)
	browser := func(sub, email string) *http.Client { return signedIn(t, ctx, pool, base, sub, email, false) }
	urllib3 := listedFingerprints(t, dana, base, "urllib3")
	approve := func(c *http.Client) (int, string) {
		t.Helper()
		code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/approve", approveBody(urllib3...))
		e, _ := body["error"].(string)
		return code, e
	}
	startedBy := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE runs SET "+q+" WHERE id = '"+run+"'", args...); err != nil {
			t.Fatal(err)
		}
	}
	agent := cliIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example")
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-rex'"); err != nil {
		t.Fatal(err)
	}
	denied := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'")
	for _, c := range []struct {
		who   string
		c     *http.Client
		setup func()
		want  string
	}{
		{"the owner, who started it", browser("sub-olive", "olive@meridian.example"), nil, "approver_required"},
		{"a contributor", browser("sub-carl", "carl@meridian.example"), nil, "approver_required"},
		{"a read-only member", browser("sub-vic", "vic@meridian.example"), nil, "team_required"},
		{"a member with no access", browser("sub-nora", "nora@meridian.example"), nil, "team_required"},
		{"an admin from the CLI", cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), nil, "dashboard_required"},
		{"an agent", agent, nil, "person_required"},
		{"an admin, of the review she started", dana, func() { startedBy("started_by_kind = 'person', started_by_subject = 'sub-dana'") }, "author_cannot_approve"},
		{"an admin, of a review that recorded no author", dana, func() { startedBy("started_by_kind = NULL, started_by_subject = NULL, started_by_display = NULL") }, "author_cannot_approve"},
		{"an admin who linked no GitHub account, of a push, while approvers of a push link GitHub", dana, func() {
			startedBy("started_by_kind = 'agent', started_by_subject = 'github:carl', started_by_display = 'github:carl', started_by_github = 583231")
			if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/preferences", `{"push_approvers_linked":true}`); code != 200 {
				t.Fatalf("turning push_approvers_linked on: %d %s", code, lastBody)
			}
		}, "github_link_required"},
	} {
		if c.setup != nil {
			c.setup()
		}
		if code, e := approve(c.c); code != 403 || e != c.want {
			t.Errorf("%s approves urllib3: %d %s, want 403 %s", c.who, code, lastBody, c.want)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/preferences", `{"push_approvers_linked":false}`); code != 200 {
		t.Fatalf("turning push_approvers_linked off: %d %s", code, lastBody)
	}
	startedBy("started_by_kind = 'person', started_by_subject = 'sub-olive', started_by_display = 'sub-olive', started_by_github = NULL")
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals"); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != denied+9 {
		t.Fatalf("the refusals wrote %d approval(s), or not one denied row each", n)
	}
	for _, c := range []struct {
		who string
		c   *http.Client
	}{{"an admin who didn't start it", dana}, {"a responder on no team", browser("sub-rex", "rex@meridian.example")}} {
		if code, _ := approve(c.c); code != 200 {
			t.Errorf("%s approves urllib3: %d %s", c.who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE rule = '"+depsRule+"' AND approves_blocking"); n != len(urllib3) {
		t.Fatalf("%d approvals under the approver rule, want urllib3's %d", n, len(urllib3))
	}
	listed := func() bool {
		t.Helper()
		row := depsRow(t, dana, base)
		i := slices.IndexFunc(row.BuiltApp, func(p builtAppPackage) bool { return p.Name == "urllib3" })
		return i >= 0 && row.BuiltApp[i].Approved
	}
	if !listed() {
		t.Fatal("the receipt lists urllib3 as not approved after an admin approved it")
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 minute' WHERE rule = $1", depsRule); err != nil {
		t.Fatal(err)
	}
	if listed() {
		t.Fatal("an expired approval still covers urllib3")
	}
}

// Only the review the receipt is read for has its built app approved (contracts/routes.md, the approve rows; #2610): an
// admin posting the fingerprints the receipt lists to another, older review of the app is not found, and nothing is
// approved or audited.
func TestDepsImageApprovalIsTheReceiptsReviewsAlone(t *testing.T) {
	pool, ctx, base, run, dana := builtAppHost(t, false)
	urllib3 := listedFingerprints(t, dana, base, "urllib3")
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, started_by_kind, started_by_subject, started_by_display, created_at)
		SELECT 'gr_older', project_id, package_id, 'clean', now(), 'person', 'sub-carl', 'sub-carl', created_at - interval '1 hour' FROM runs WHERE id = $1)
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_older', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = $1 AND kind = 'verdict'`, run); err != nil {
		t.Fatal(err)
	}
	none := func(what string) {
		t.Helper()
		if n := count(t, ctx, pool, "SELECT count(*) FROM approvals") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%/approve'"); n != 0 {
			t.Fatalf("%s wrote %d approval or audit rows", what, n)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+"/api/v1/gate-runs/gr_older/findings/approve", approveBody(urllib3...)); code != 404 {
		t.Fatalf("the receipt's fingerprints posted to an older review: %d %s", code, lastBody)
	}
	none("an older review's approval")
}

// A fingerprint the receipt's deps.known-vuln row doesn't list is not found (contracts/routes.md, the approve rows;
// #2610), though the built app holds it: here the row's review half fails, so it lists no built_app, and nothing is
// approved or audited.
func TestDepsImageApprovalIsWhatTheRowLists(t *testing.T) {
	pool, ctx, base, run, dana := builtAppHost(t, true)
	if row := depsRow(t, dana, base); row.Passed || len(row.BuiltApp) != 0 {
		t.Fatalf("a row whose review half fails: %+v", row)
	}
	g := loadBuiltAppGolden(t)
	app := readBuiltApp(g.facts(t), nil, g.ReadAt) // what the built app holds, which the row doesn't list
	i := slices.IndexFunc(app.packages, func(p appPackage) bool { return p.name == "urllib3" })
	if i < 0 {
		t.Fatalf("no urllib3 in the built app: %v", app.names())
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/approve", approveBody(app.packages[i].fingerprints...)); code != 404 {
		t.Fatalf("fingerprints the receipt doesn't list: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%/approve'"); n != 0 {
		t.Fatalf("an approval of what the receipt doesn't list wrote %d approval or audit rows", n)
	}
}
