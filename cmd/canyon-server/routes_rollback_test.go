package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// twiceInProduction is claims-portal promoted twice: its first package's image a, then a second package's
// image b, which production runs now. It answers the two packages and images.
func twiceInProduction(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, base string, c *http.Client, first, a, second, b string) {
	t.Helper()
	pool, ctx, base, c, deploy := previewed(t)
	cleanGate(t)
	a = "canyon/claims-portal@" + executortest.Digest
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the first promotion: %s", lastBody)
	}
	if err := pool.QueryRow(ctx, "SELECT package_id FROM deploys WHERE env = 'prod'").Scan(&first); err != nil {
		t.Fatal(err)
	}
	b = "canyon/claims-portal@sha256:" + strings.Repeat("b", 64)
	fake.Image = b
	t.Cleanup(func() { fake.Image = "" })
	_, uploaded := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export const v = 2\n"}))
	second, _ = packageOf(uploaded)["id"].(string)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	again, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, again)["state"] != "clean" || promoted(t, c, base, again)["state"] != "clean" {
		t.Fatalf("the second version: %s", lastBody)
	}
	return pool, ctx, base, c, first, a, second, b
}

// A rollback review starts only on one of production's earlier versions, past the hour's limit; its
// receipt ships the image production ran, and its promotion puts exactly that image back with nothing
// built, the trail saying what it replaced and why.
func TestRollbackPutsTheImageProductionRanBack(t *testing.T) {
	pool, ctx, base, c, first, a, second, b := twiceInProduction(t)
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	earlier, _ := status["earlier"].([]any)
	digestA := strings.TrimPrefix(a, "canyon/claims-portal@")
	if len(earlier) != 1 || earlier[0].(map[string]any)["package"] != first || earlier[0].(map[string]any)["image_digest"] != digestA || earlier[0].(map[string]any)["kept"] != true ||
		earlier[0].(map[string]any)["version"] != strings.TrimPrefix(first, "pkg_") {
		t.Fatalf("earlier: %s", lastBody)
	}
	for _, refused := range []struct{ route, body, code string }{
		{"gate-runs", `{"package":"` + first + `","rollback_to":"sha256:` + strings.Repeat("c", 64) + `"}`, "not_earlier"},
		{"gate-runs", `{"package":"` + second + `","rollback_to":"` + digestA + `"}`, "not_earlier"}, // production ran it, but as the other package
		{"gate-runs", `{"package":"` + first + `","reason":"INC-2231"}`, "invalid_reason"},
		{"gate-runs", `{"package":"` + first + `","rollback_to":"` + digestA + `","reason":""}`, "invalid_reason"},
		{"gate-runs", `{"package":"` + first + `","rollback_to":"` + digestA + `","reason":"INC-2231 \u202e"}`, "invalid_reason"},
		{"deploys", `{"package":"` + first + `","rollback_to":"` + digestA + `"}`, "bad_request"},
	} {
		if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/"+refused.route, refused.body); body["error"] != refused.code || code == 202 {
			t.Fatalf("%s %s: %d %s, want %s", refused.route, refused.body, code, lastBody, refused.code)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, kind, ended_at)
		SELECT 'gr_hour' || n, 'claims-portal', $1, 'failed', 'interrupted', 'gate', now() FROM generate_series(1, 30) n`, second); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", ""); code != 429 || body["error"] != "rate_limited" {
		t.Fatalf("the hour's 31st review: %d %s", code, lastBody)
	}
	builds, ran := fake.Builds.Load(), fake.Runs.Load()
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`","rollback_to":"`+digestA+`","reason":"INC-2231: v2 corrupts exports"}`)
	review, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, review)["state"] != "clean" {
		t.Fatalf("a rollback review past the hour's limit: %d %s", code, lastBody)
	}
	_, plain, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if plain["last_run"] == review || plain["promote_run"] == review || plain["last_run"] == nil || fleet["apps"].([]any)[0].(map[string]any)["last_run"] == review {
		t.Fatalf("a rollback review stands in for no run of preview's: status %s, fleet %s", mustJSON(plain), mustJSON(fleet["apps"]))
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/gate-runs' AND run_id = '`+review+`' AND detail = '{"package":"`+first+`","rollback_to":"`+digestA+`","reason":"INC-2231: v2 corrupts exports"}'`); n != 1 {
		t.Fatalf("the review's audit row: %d", n)
	}
	_, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status?run="+review, "")
	receipt, _ := status["receipt"].(map[string]any)
	if ships, _ := receipt["ships"].(map[string]any); status["promote_run"] != review || len(receipt["blocked_by"].([]any)) != 0 || receipt["waiting"] != nil ||
		ships["version"] != strings.TrimPrefix(first, "pkg_") || ships["image_digest"] != digestA {
		t.Fatalf("the review's receipt: %s", lastBody)
	}
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status?run=gr_nope", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("a run that is not the app's: %d %s", code, lastBody)
	}
	if done := promoted(t, c, base, review); done["state"] != "clean" {
		t.Fatalf("the rollback's promotion: %s", lastBody)
	}
	if r := fake.Ran(); fake.Builds.Load() != builds || fake.Runs.Load() != ran+1 || r.Target != "prod" || r.Image != a || !slices.Contains(r.Keep, strings.TrimPrefix(b, "canyon/claims-portal@")) {
		t.Fatalf("one /run of the image production ran, keeping the one it replaces, and no build: %d builds, %d runs, %+v", fake.Builds.Load()-builds, fake.Runs.Load()-ran, r)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM deploys WHERE env = 'prod' AND package_id = '`+first+`' AND image = '`+a+`' AND gate_run = '`+review+`'`); n != 1 {
		t.Fatalf("production's row: %d", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'deploy prod' AND detail @> '{"replaces":"`+second+`","rollback":true,"reason":"INC-2231: v2 corrupts exports"}'`); n != 1 {
		t.Fatalf("the trail: %d rows saying it was put back", n)
	}
	_, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if earlier, _ := status["earlier"].([]any); len(earlier) != 1 || earlier[0].(map[string]any)["package"] != second || earlier[0].(map[string]any)["kept"] != true {
		t.Fatalf("the version it replaced is the earlier one now, kept: %s", lastBody)
	}
}

// While production is approved in the dashboard the CLI's ask of a rollback files an approval; a person's
// approval in the browser puts the image back, and one asked before production changed is outdated.
func TestRollbackIsApprovedInTheDashboardUntilProductionChanges(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, false)
	cleanGate(t)
	if promoted(t, browser, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	var pkg, image string
	if err := pool.QueryRow(ctx, "SELECT package_id, image FROM deploys WHERE env = 'prod'").Scan(&pkg, &image); err != nil {
		t.Fatal(err)
	}
	b := "canyon/claims-portal@sha256:" + strings.Repeat("b", 64)
	for _, q := range []string{
		"INSERT INTO deploy_history (project_id, package_id, image, replaces, gate_run, run) VALUES ('claims-portal', '" + pkg + "', '" + b + "', '" + image + "', '" + deploy + "', '" + deploy + "')",
		"UPDATE deploys SET image = '" + b + "' WHERE env = 'prod'",
		"UPDATE org_preferences SET prod_approval = true",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	review := func(to string) string {
		_, started, _ := do(t, cli, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+pkg+`","rollback_to":"`+strings.TrimPrefix(to, "canyon/claims-portal@")+`"}`)
		run, _ := runOf(started)["id"].(string)
		if waitVerdict(t, cli, base, run)["state"] != "clean" {
			t.Fatalf("the review: %s", lastBody)
		}
		return run
	}
	code := askApproval(t, cli, base, review(image))
	got, body, _ := do(t, browser, http.MethodPost, base+"/api/v1/promotion-approvals/"+code+"/approve", "")
	run, _ := runOf(body)["id"].(string)
	if got != 202 || waitVerdict(t, browser, base, run)["state"] != "clean" || fake.Ran().Image != image {
		t.Fatalf("the approved rollback: %d %s, ran %s", got, lastBody, fake.Ran().Image)
	}
	code = askApproval(t, cli, base, review(b))
	if _, read, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/"+code, ""); read["state"] != "pending" || read["rollback"] != true {
		t.Fatalf("the approval while production runs what it replaces: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET image = $1 WHERE env = 'prod'", "canyon/claims-portal@sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/"+code, ""); read["state"] != "outdated" {
		t.Fatalf("the approval once production changed: %s", lastBody)
	}
	if got, body, _ := do(t, browser, http.MethodPost, base+"/api/v1/promotion-approvals/"+code+"/approve", ""); got != 409 || body["error"] != "outdated" ||
		body["message"] != "production runs another version now: ask again from the CLI" || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 2 {
		t.Fatalf("approving it: %d %s", got, lastBody)
	}
}

// A rollback review still under way is the project's active run: promoting it is 409 run_in_progress. One
// that blocked — here of the version production runs, put back to an image it ran before (a patch's) — is
// kept out of the fleet's tiles and gate state and of what a policy would block. One that recorded nothing to ship
// — its findings approved only after it ended — says so on its receipt, and is never promoted, whatever the
// rules' states: an admin who made prod.clean-gate advisory still meets 409 nothing_to_ship, and no run starts.
func TestRollbackWithNothingToShipIsRefused(t *testing.T) {
	pool, ctx, base, c, first, a, second, _ := twiceInProduction(t)
	patched := "canyon/claims-portal@sha256:" + strings.Repeat("d", 64)
	if _, err := pool.Exec(ctx, "INSERT INTO deploy_history (project_id, package_id, image, gate_run, run, deployed_at, patched) SELECT 'claims-portal', $1, $2, gate_run, gate_run, now() - interval '1 hour', true FROM deploys WHERE env = 'prod'", second, patched); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, rollback_to) VALUES ('gr_rbbusy0000', 'claims-portal', $1, 'running', $2)", first, strings.TrimPrefix(a, "canyon/claims-portal@")); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"gr_rbbusy0000"}`)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_rbbusy0000'"); err != nil {
		t.Fatal(err)
	}
	if code != 409 || body["error"] != "run_in_progress" || runOf(body)["id"] != "gr_rbbusy0000" || runOf(body)["state"] != "running" {
		t.Fatalf("promoting a rollback review still under way: %d %s", code, lastBody)
	}
	pipe.Gate = runstest.Golden
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+second+`","rollback_to":"`+strings.TrimPrefix(patched, "canyon/claims-portal@")+`"}`)
	review, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, review)["state"] != "blocked" {
		t.Fatalf("the review: %s", lastBody)
	}
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if tiles, _ := fleet["tiles"].(map[string]any); tiles["blocked"] != 0.0 || tiles["secrets_exposed"] != 0.0 || fleet["apps"].([]any)[0].(map[string]any)["gate_state"] != "clean" {
		t.Fatalf("a blocked rollback review makes the app read blocked on the fleet: %s %s", mustJSON(fleet["tiles"]), mustJSON(fleet["apps"]))
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/secrets.client-bundle", `{"state":"blocking","production":"blocking"}`); code != 200 || lastBody != `{"would_block":[]}` {
		t.Fatalf("a blocked rollback review counts in what a policy would block: %d %s", code, lastBody)
	}
	approve(t, ctx, pool, "now() + interval '1 day'")
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status?run="+review, "")
	if rule := status["receipt"].(map[string]any)["rules"].([]any)[0].(map[string]any); rule["passed"] != false || rule["why"] != strings.TrimPrefix(second, "pkg_")+"'s review recorded no image to put back; review it again" {
		t.Fatalf("the receipt: %s", lastBody)
	}
	edit(t, ctx, pool, "prod.clean-gate", "advisory", "0")
	promotes := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'")
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+review+`"}`); code != 409 || body["error"] != "nothing_to_ship" ||
		count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != promotes {
		t.Fatalf("promoting it with clean-gate advisory: %d %s", code, lastBody)
	}
}

// A rollback review is promoted only while it is its package's newest review: a newer one that blocked
// is never hidden behind it. Once it put the image back, its receipt says so, and a promotion of it while
// production runs another version is outdated.
func TestRollbackReviewIsPromotedOnlyWhileNewestAndOnce(t *testing.T) {
	pool, ctx, base, c, first, a, _, _ := twiceInProduction(t)
	digestA := strings.TrimPrefix(a, "canyon/claims-portal@")
	rollback := func() string {
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`","rollback_to":"`+digestA+`"}`)
		run, _ := runOf(started)["id"].(string)
		if waitVerdict(t, c, base, run)["state"] != "clean" {
			t.Fatalf("the review: %s", lastBody)
		}
		return run
	}
	stale := rollback()
	pipe.Gate = runstest.Golden
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`"}`)
	if newer, _ := runOf(started)["id"].(string); waitVerdict(t, c, base, newer)["state"] != "blocked" {
		t.Fatalf("the newer review: %s", lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+stale+`"}`)
	if receipt, _ := body["receipt"].(map[string]any); code != 409 || body["error"] != "gate_not_clean" ||
		receipt["rules"].([]any)[0].(map[string]any)["why"] != strings.TrimPrefix(first, "pkg_")+" has a newer review than the one named" {
		t.Fatalf("promoting a review a newer one blocked: %d %s", code, lastBody)
	}
	pipe.Gate = runstest.CleanGolden
	shipped := rollback()
	if promoted(t, c, base, shipped)["state"] != "clean" {
		t.Fatalf("the rollback: %s", lastBody)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status?run="+shipped, "")
	if rule := status["receipt"].(map[string]any)["rules"].([]any)[0].(map[string]any); rule["passed"] != false || rule["why"] != strings.TrimPrefix(first, "pkg_")+"'s review has put it back already; review it again" {
		t.Fatalf("the receipt of a review that shipped: %s", lastBody)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+shipped+`"}`); code != 409 || body["error"] != "outdated" || body["message"] != "production runs another version now: review it again" {
		t.Fatalf("promoting it again once production changed: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote' AND state = 'clean'"); n != 3 {
		t.Fatalf("%d promotions went live, want the two versions and the one rollback", n)
	}
}

// Under hand-off the org's report of a rollback's release writes production's history — kept, the image in
// the org's registry — and its row says what it put back, what it replaced and why.
func TestAnOrgsRollbackReportSaysWhatItPutBack(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	for _, q := range []string{
		"UPDATE runs SET state = 'clean', ended_at = now(), rollback_to = 'sha256:" + reportDigest + "', rollback_reason = 'INC-2231' WHERE id = 'gr_8f2a'",
		"INSERT INTO runs (id, project_id, package_id, state, kind) VALUES ('gr_prom01', 'claims-portal', 'pkg_7d3e8f', 'running', 'promote')",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run) VALUES ('rel_prod1', 'claims-portal', 'prod', 'gr_prom01', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'accepted', now(), 'gr_8f2a')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploy_history WHERE image = '"+reportImage+"' AND gate_run = 'gr_8f2a' AND run = 'gr_prom01' AND kept AND egress IS NULL"); n != 1 {
		t.Fatalf("the history row: %d", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'platform reported deployed' AND detail @> '{"replaces":null,"rollback":true,"reason":"INC-2231"}'`); n != 1 {
		t.Fatalf("the report's row: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod' AND run_started_at IS NULL AND answered IS NULL AND run_by = 'org'"); n != 1 {
		t.Fatalf("the org's row carries none of Canyon's run: %d", n)
	}
}
