package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// blockedRun starts a gate run of the current package under the golden gate and waits for its verdict: blocked, one blocking finding.
func blockedRun(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	if got := waitVerdict(t, c, base, run); got["state"] != "blocked" {
		t.Fatalf("the golden gate blocks: %s", lastBody)
	}
	return run
}

// The golden's story: preview runs a version its review passed, and a newer version the gate stopped
// never reached it, so the receipt answers for preview's run and the newer block does not stand in.
func TestTheStatusReceiptIsTheGoldenShape(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_display = 'Dana Whitfield'"); err != nil { // the golden's reader has a name; an anonymous session carries none
		t.Fatal(err)
	}
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "b": "2"}))
	run := blockedRun(t, c, base)
	goldenLabels(t, ctx, pool)
	raw, err := os.ReadFile("../../contracts/testdata/app-page/claims-portal.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if wantJSON := strings.ReplaceAll(string(mustJSON(doc["status"].(map[string]any)["receipt"])), "gr_6c12", deploy); code != 200 || string(mustJSON(got["receipt"])) != wantJSON {
		t.Fatalf("the golden receipt:\n got %s\nwant %s", mustJSON(got["receipt"]), wantJSON)
	}
	if got["project"] != "claims-portal" || got["last_run"] != run || got["promote_run"] != deploy || len(got) != 7 || len(got["earlier"].([]any)) != 0 || got["needs"] == nil {
		t.Fatalf("{project, envs, last_run, promote_run, receipt, earlier, needs}: %s", lastBody)
	}
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/nothing/status", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("unknown project: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%status%'"); n != 0 {
		t.Fatalf("a read wrote %d audit rows", n)
	}
}

func TestAnUnexpiredApprovalNetsTheBlockingFindingOutOfTheReceipt(t *testing.T) {
	cleanGate(t)
	pool, ctx, base, c := withProject(t)
	_, uploaded := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	if deploy, _ := runOf(started)["id"].(string); waitVerdict(t, c, base, deploy)["state"] != "clean" { // preview runs the package
		t.Fatalf("the clean deploy: %s", lastBody)
	}
	pipe.Gate = runstest.Golden // the gate blocks the same package now
	blockedRun(t, c, base)
	gate := func() (string, map[string]any) {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		r := body["receipt"].(map[string]any)
		return fmt.Sprint(r["blocked_by"]), r["rules"].([]any)[0].(map[string]any)
	}
	version := strings.TrimPrefix(packageOf(uploaded)["id"].(string), "pkg_") // unlabelled: a reason names it by its id without the prefix, never by the run
	unapproved := version + "'s review found 1 blocking finding, not approved"
	if by, g := gate(); by != "[prod.clean-gate]" || g["why"] != unapproved {
		t.Fatalf("before the approval: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approves_blocking)
		VALUES ('claims-portal', 'secrets.client-bundle', 'f_0000000000000000000000000000000000000000000000000000000000000000', 'another finding', now() + interval '1 hour', 'person', 'dana@meridian.example', true)`); err != nil {
		t.Fatal(err)
	}
	if by, _ := gate(); by != "[prod.clean-gate]" { // an approval follows one finding, never its rule
		t.Fatalf("under an approval of another finding of the same rule: %s", lastBody)
	}
	approve(t, ctx, pool, "now() + interval '1 hour'")
	if by, g := gate(); by != "[]" || g["passed"] != true || g["why"] != version+" passed review: every blocking finding approved" {
		t.Fatalf("under an unexpired approval: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if by, g := gate(); by != "[prod.clean-gate]" || g["why"] != unapproved {
		t.Fatalf("after the approval expired: %s", lastBody)
	}
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if preview := body["envs"].(map[string]any)["preview"].(map[string]any); preview["package"] != packageOf(uploaded)["id"] {
		t.Fatalf("envs is the project read's: %s", lastBody)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quiet"}`)                                                                                                                                              // another project's runs never answer for this one
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/quiet/status", ""); body["last_run"] != nil || body["receipt"].(map[string]any)["waiting"] != "Deploy a new version to preview to promote it." { // nothing in its preview: it waits
		t.Fatalf("a project no run has looked at, beside one with runs: %s", lastBody)
	}
}

// goldenLabels names the two packages as the golden does: v1.3, which preview runs, and v1.4, which
// the gate stopped — the receipt's reasons name versions (contracts/routes.md, canyonv2-5r11.12).
func goldenLabels(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE packages p SET label = CASE WHEN EXISTS (SELECT 1 FROM deploys d
		WHERE d.project_id = p.project_id AND d.env = 'preview' AND d.package_id = p.id) THEN 'v1.3' ELSE 'v1.4' END WHERE p.project_id = 'claims-portal'`); err != nil {
		t.Fatal(err)
	}
}

// approve writes the approval by SQL — the route arrives with the promote PR — for the golden log's one blocking finding.
func approve(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expires string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'demo fixture', `+expires+`, 'person', 'dana@meridian.example', true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.project_id = 'claims-portal' AND t.kind = 'verdict'`); err != nil { // the fingerprint comes off the verdict, as the approve route keys it
		t.Fatal(err)
	}
}

// The run read after the approval is the golden run_approved: the approved finding carries who,
// why and until when; the others carry nothing more. The fixture's expiry is a date that passes,
// so the approval is written an hour ahead and the read's expiry stands in for the fixture's.
func TestTheApprovedRunReadIsTheGoldenShape(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, uploaded := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run, doc := blockedRun(t, c, base), appPage(t)
	by := doc["approved"].(map[string]any)["approval"].(map[string]any)
	who := by["approved_by"].(map[string]any)
	var expires string
	if err := pool.QueryRow(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', $1, date_trunc('second', now()) + interval '1 hour', 'person', $2, $3, true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.run_id = $4 AND t.kind = 'verdict'
		RETURNING to_char(expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`, by["reason"], who["subject"], who["display"], run).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer("gr_8f2a", run, "pkg_9e40", packageOf(uploaded)["id"].(string), by["expires_at"].(string), expires).Replace(string(mustJSON(doc["run_approved"])))
	if _, got, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, ""); string(mustJSON(got)) != want {
		t.Fatalf("the golden run read after the approval:\n got %s\nwant %s", mustJSON(got), want)
	}
}

func TestAnExpiredApprovalIsNotOnTheFinding(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	approve(t, ctx, pool, "now() - interval '1 hour'")
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
	findings, _ := runOf(body)["findings"].([]any)
	for _, f := range findings {
		if _, on := f.(map[string]any)["approval"]; on {
			t.Fatalf("an expired approval is on the finding: %s", lastBody)
		}
	}
	if len(findings) != 4 {
		t.Fatalf("the golden log's four findings: %s", lastBody)
	}
}

// An approval Canyon's AI wrote before it stopped approving shows on no finding, even an older
// verdict's at the digest it stored: only a person's approval covers one (routes.md, the run read).
func TestOnlyAPersonsApprovalShowsOnTheRunRead(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if _, err := pool.Exec(ctx, `
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_ai', 'claims-portal', 't', 'c', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_ai', 'claims-portal', 'pkg_ai', 'blocked', now(), 'person', 'anonymous', '');
INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', 'gr_ai', 'claims-portal', 'verdict', '{"state": "blocked", "blocking": 1, "warnings": 0,
  "findings": {"runs": [{"results": [{"ruleId": "auth.unauthenticated-route", "partialFingerprints": {"canyon/v1": "route-a"},
  "properties": {"canyon": {"severity": "high", "blocks": true}}}]}]}}');
INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, shown_digest)
VALUES ('claims-portal', 'auth.unauthenticated-route', 'route-a', 'Public by design.', now() + interval '1 hour', 'canyon', 'ai', 'Canyon''s AI', 'sha256:edited')`); err != nil {
		t.Fatal(err)
	}
	approval := func() any {
		t.Helper()
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_ai", "")
		findings, _ := runOf(body)["findings"].([]any)
		if len(findings) != 1 {
			t.Fatalf("the one finding: %s", lastBody)
		}
		return findings[0].(map[string]any)["approval"]
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET shown_digest = 'sha256:shown'"); err != nil {
		t.Fatal(err)
	}
	if got := approval(); got != nil {
		t.Fatalf("Canyon's AI's approval at the finding's digest shows: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET approved_by_kind = 'person', approved_by_subject = 'dana@meridian.example', approved_by_display = 'Dana Whitfield', approves_blocking = true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := approval().(map[string]any); got["approved_by"].(map[string]any)["kind"] != "person" {
		t.Fatalf("a person's approval: %s", lastBody)
	}
}

// A person approving a finding Canyon's AI approved replaces the approval and clears its digest.
func TestAPersonsApprovalReplacesCanyonsAndClearsItsDigest(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	var fp string // read off the verdict, never written into a test: a fingerprint literal is secret-shaped
	if err := pool.QueryRow(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, shown_digest)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'judged fine', now() + interval '1 hour', 'canyon', 'ai', 'Canyon''s AI', 'sha256:shown'
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.run_id = $1 AND t.kind = 'verdict' RETURNING fingerprint`, run).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/"+fp+"/approve", `{"reason":"Reviewed by hand","expires_at":"`+expires+`"}`); code != 200 {
		t.Fatalf("the person's approval: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE approved_by_kind = 'person' AND reason = 'Reviewed by hand' AND shown_digest IS NULL"); n != 1 {
		t.Fatalf("the person's approval kept Canyon's digest: %d row(s) replaced and cleared", n)
	}
}

// /status answers envs.preview by the same label the project read does, and when the newest run
// reviews a package other than the one preview runs, the clean-gate rule's reason names preview's
// package by its label too (receipt.go, pkgRef.version) — neither was ever asserted.
func TestStatusNamesTheLabelInEnvsAndInTheReceiptsReason(t *testing.T) {
	cleanGate(t)
	_, _, base, c := withProject(t)
	uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.4"})
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", `{"package":"v1.4"}`)
	deploy, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("deploy the labelled upload: %d %s", code, lastBody)
	}
	_, newer := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"})) // a newer, unlabelled package, reviewed but never deployed
	_, started, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("review the newer package: %s", lastBody)
	}
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if got := body["envs"].(map[string]any)["preview"].(map[string]any)["label"]; got != "v1.4" || body["promote_run"] != deploy {
		t.Fatalf("/status envs.preview.label %v and promote_run %v, want v1.4 and %s: the newer review never stands in for preview's", got, body["promote_run"], deploy)
	}
	_, body, _ = do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`) // a promotion naming the newer review is refused, and says why
	why, _ := body["receipt"].(map[string]any)["rules"].([]any)[0].(map[string]any)["why"].(string)
	if want := "the review named is " + strings.TrimPrefix(packageOf(newer)["id"].(string), "pkg_") + "'s; preview runs v1.4"; why != want {
		t.Fatalf("the receipt's reason names preview's package by its label:\n got %q\nwant %q", why, want)
	}
}

// previewed is claims-portal with its one package deployed to preview through a clean gate, and the deploy run; the gate blocks from here on.
func previewed(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client, string) {
	t.Helper()
	cleanGate(t)
	appAnswersOK(t)
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("the clean deploy: %s", lastBody)
	}
	pipe.Gate = runstest.Golden
	return pool, ctx, base, c, run
}

const promotions = "/api/v1/projects/claims-portal/promotions"

func TestPromotingABlockedRunAnswersTheReceiptAndChangesNothing(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "b": "2"}))
	run := blockedRun(t, c, base)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_display = 'Dana Whitfield'"); err != nil {
		t.Fatal(err)
	}
	goldenLabels(t, ctx, pool)
	raw, err := os.ReadFile("../../contracts/testdata/app-page/claims-portal.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	ran := fake.Runs.Load()
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
	if want := strings.ReplaceAll(string(mustJSON(doc["refusal"])), "gr_8f2a", run); code != 409 || string(mustJSON(body)) != want {
		t.Fatalf("the golden refusal:\n got %d %s\nwant %s", code, mustJSON(body), want)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); code != 409 || body["error"] != "approval_required" || body["message"] != "a person must click promote" || fmt.Sprint(body["receipt"].(map[string]any)["blocked_by"]) != "[prod.human-approval]" {
		t.Fatalf("a clean run an agent asks to promote: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quiet"}`)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	for _, req := range [][2]string{{"/api/v1/projects/nothing/promotions", deploy}, {"/api/v1/projects/gone/promotions", deploy}, {"/api/v1/projects/quiet/promotions", deploy}, {promotions, ""}, {promotions, "latest"}} { // unknown or deleted project, another project's run, no run, never "latest"
		if code, got, _ := do(t, c, http.MethodPost, base+req[0], `{"gate_run":"`+req[1]+`"}`); code != 404 || got["error"] != "not_found" {
			t.Fatalf("%v: %d %s", req, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%promotions'"); n != 0 || fake.Runs.Load() != ran {
		t.Fatalf("a refused promotion wrote %d rows and started %d containers", n, fake.Runs.Load()-ran)
	}
}

// A preview Canyon runs that isn't running leaves the receipt waiting on the status read, and the
// promote route, asking the executor once, refuses it with the same line and changes nothing (#1557).
func TestAPreviewThatIsNotRunningWaitsAndIsNeverPromoted(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	pkg, _ := status["envs"].(map[string]any)["preview"].(map[string]any)["package"].(string)
	if r := status["receipt"].(map[string]any); r["waiting"] != nil || len(r["rules"].([]any)) != 4 {
		t.Fatalf("a running preview is evaluated: %s", lastBody)
	}
	if err := (&executor.Client{Base: fake.URL}).Stop(ctx, "claims-portal", "preview"); err != nil { // the app exited and nothing started it again
		t.Fatal(err)
	}
	v, ran := strings.TrimPrefix(pkg, "pkg_"), fake.Runs.Load()
	for _, tc := range []struct{ state, line string }{
		{"stopped", v + " isn't running in preview: canyon logs says why."},
		{"unknown", "Canyon couldn't check whether " + v + " runs in preview."},
	} {
		fake.FailRunning.Store(tc.state == "unknown")
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		if r := status["receipt"].(map[string]any); r["waiting"] != tc.line || len(r["rules"].([]any)) != 0 || len(r["blocked_by"].([]any)) != 0 {
			t.Errorf("%s, the status read: %s", tc.state, lastBody)
			continue
		}
		asked := fake.RunningOfs.Load()
		code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
		if code != 409 || body["error"] != "nothing_in_preview" || body["message"] != tc.line || body["receipt"].(map[string]any)["waiting"] != tc.line {
			t.Fatalf("%s, the promotion: %d %s", tc.state, code, lastBody)
		}
		if n := fake.RunningOfs.Load() - asked; n != 1 {
			t.Fatalf("the promotion asked the executor %d times", n)
		}
	}
	fake.FailRunning.Store(false)
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%promotions'"); n != 0 || fake.Runs.Load() != ran {
		t.Fatalf("a refused promotion wrote %d rows and started %d containers", n, fake.Runs.Load()-ran)
	}
}

// A preview that has restarted since its deploy reads restarting, never running, even while it is up —
// the test cluster's case-dies-later, promotable in each 40 s window of its backoff: the status read says how
// often, the fleet row says restarting with no count, the receipt waits, and the promotion is refused
// with the same line and changes nothing (contracts/routes.md, envs.<env>.state; #1722).
func TestAPreviewThatKeepsRestartingWaitsAndIsNeverPromoted(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	pkg, _ := status["envs"].(map[string]any)["preview"].(map[string]any)["package"].(string)
	fake.Restarting = []executor.Restarting{{App: "claims-portal", Target: "preview", Restarts: 7}} // up now, in one of its windows
	t.Cleanup(func() { fake.Restarting = nil })
	line, ran := strings.TrimPrefix(pkg, "pkg_")+" restarted 7 times since it was deployed; deploy again once it's fixed (the build is reused).", fake.Runs.Load()
	_, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	preview := status["envs"].(map[string]any)["preview"].(map[string]any)
	if preview["state"] != "restarting" || fmt.Sprint(preview["restarting"]) != "map[restarts:7]" {
		t.Fatalf("the status read's preview: %s", lastBody)
	}
	if r := status["receipt"].(map[string]any); r["waiting"] != line || len(r["rules"].([]any)) != 0 {
		t.Fatalf("the receipt: %s", lastBody)
	}
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	for _, a := range fleet["apps"].([]any) {
		if row := a.(map[string]any); row["id"] == "claims-portal" {
			if env := row["envs"].(map[string]any)["preview"].(map[string]any); env["state"] != "restarting" || env["restarting"] != nil {
				t.Fatalf("the fleet row's preview: %v", env)
			}
		}
	}
	asked := fake.WhyAsked.Load()
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
	if code != 409 || body["error"] != "nothing_in_preview" || body["message"] != line || fake.WhyAsked.Load() != asked {
		t.Fatalf("the promotion, which counts the restarts and never asks why: %d %s, why asked %d times", code, lastBody, fake.WhyAsked.Load()-asked)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%promotions'"); n != 0 || fake.Runs.Load() != ran {
		t.Fatalf("a refused promotion wrote %d rows and started %d containers", n, fake.Runs.Load()-ran)
	}
}

// A stopped preview says why on the status read alone: the executor's raw last line masked with that
// environment's runtime values by /run's rule, a URL's password too, what could hide text removed, cut
// to 200 characters; the exit code alone once a secret was set or removed since the run that started
// it began, or that start is not recorded; never on a running one; and the fleet never asks why (#1597).
func TestAStoppedPreviewSaysWhyOnTheStatusReadAlone(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	t.Cleanup(func() { fake.Why = nil })
	const dsn = "postgres://app:pw-7f3a@db:5432/app" //nolint:gosec // a test value, masked below
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"`+dsn+`"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"tok-91c2"}`)
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys d JOIN runs r ON r.id = d.gate_run WHERE d.run_started_at = r.created_at"); n != 1 {
		t.Fatalf("the deploy row records when its run started: %d rows do", n)
	}
	exec := func(sql string) {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	redeployed := func() {
		exec("UPDATE deploys SET run_started_at = now(), deployed_at = now() WHERE project_id = 'claims-portal'")
	} // a run that read these values
	redeployed()
	line := "Error: " + dsn + " refused tok-91c2 (pw-7f3a) \u202e\x1b[31m<b>3000</b> "
	fake.Why = []executor.Stopped{{App: "claims-portal", Target: "preview", ExitCode: 1, Said: line + strings.Repeat("x", 300)}, {App: "claims-portal", Target: "prod", ExitCode: 2, Said: "not asked"}}
	stopped := func() any {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		envs := status["envs"].(map[string]any)
		if envs["prod"] != nil {
			t.Fatalf("an environment nothing is deployed in: %s", lastBody)
		}
		return envs["preview"].(map[string]any)["stopped"]
	}
	if got := stopped(); got != nil {
		t.Fatalf("a running preview says why it stopped: %v", got)
	}
	if err := (&executor.Client{Base: fake.URL}).Stop(ctx, "claims-portal", "preview"); err != nil {
		t.Fatal(err)
	}
	masked := "Error: <DATABASE_URL> refused <API_TOKEN> (<DATABASE_URL password>) [31m<b>3000</b> "
	if got, want := fmt.Sprint(stopped()), fmt.Sprint(map[string]any{"exit_code": 1.0, "said": masked + strings.Repeat("x", 200-len(masked))}); got != want {
		t.Fatalf("a stopped preview says\n %s\nwant\n %s", got, want)
	}
	asked := fake.WhyAsked.Load()
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if strings.Contains(lastBody, "exit_code") || strings.Contains(lastBody, "Error: ") || fake.WhyAsked.Load() != asked { // the line, never the refused tile's key (#2723)
		t.Fatalf("the fleet asked why %d times: %v", fake.WhyAsked.Load()-asked, fleet)
	}
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"tok-new"}`)
	if got := fmt.Sprint(stopped()); got != fmt.Sprint(map[string]any{"exit_code": 1.0, "said": ""}) {
		t.Fatalf("a secret set after the deploy: %s, want the exit code alone", got)
	}
	exec("UPDATE deploys SET run_started_at = now() - interval '1 hour', deployed_at = now() WHERE project_id = 'claims-portal'")
	exec("UPDATE secrets SET set_at = now() - interval '30 minutes' WHERE project_id = 'claims-portal' AND name = 'API_TOKEN'")
	if got := fmt.Sprint(stopped()); got != fmt.Sprint(map[string]any{"exit_code": 1.0, "said": ""}) {
		t.Fatalf("a secret set after the run began, before its deploy row: %s, want the exit code alone", got)
	}
	redeployed()
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", "")
	if got := fmt.Sprint(stopped()); got != fmt.Sprint(map[string]any{"exit_code": 1.0, "said": ""}) {
		t.Fatalf("a secret removed after the deploy: %s, want the exit code alone", got)
	}
	redeployed()
	exec("UPDATE deploys SET run_started_at = NULL WHERE project_id = 'claims-portal'")
	if got := fmt.Sprint(stopped()); got != fmt.Sprint(map[string]any{"exit_code": 1.0, "said": ""}) {
		t.Fatalf("a row written before its run's start was recorded: %s, want the exit code alone", got)
	}
}

// A restarting preview says how it last exited on the status read, as a stopped one says why, masked by
// the same rule: the test cluster's case-dies-later restarts for ever on Kubernetes, so it never reads stopped.
// The fleet row never says it (#1597).
func TestARestartingPreviewSaysHowItLastExited(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"tok-91c2"}`)
	if _, err := pool.Exec(ctx, "UPDATE deploys SET run_started_at = now(), deployed_at = now() WHERE project_id = 'claims-portal'"); err != nil { // a run that read the value
		t.Fatal(err)
	}
	fake.Restarting = []executor.Restarting{{App: "claims-portal", Target: "preview", Restarts: 7}}
	fake.Why = []executor.Stopped{{App: "claims-portal", Target: "preview", ExitCode: 1, Said: "lost the queue at tok-91c2: exiting"}}
	t.Cleanup(func() { fake.Restarting, fake.Why = nil, nil })
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	preview := status["envs"].(map[string]any)["preview"].(map[string]any)
	if got, want := fmt.Sprint(preview["restarting"]), fmt.Sprint(map[string]any{"restarts": 7.0, "exit_code": 1.0, "said": "lost the queue at <API_TOKEN>: exiting"}); preview["state"] != "restarting" || got != want || preview["stopped"] != nil {
		t.Fatalf("the status read's restarting preview: %s, want restarting %s", lastBody, want)
	}
	do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if strings.Contains(lastBody, "exit_code") || strings.Contains(lastBody, "lost the queue") {
		t.Fatalf("the fleet says how it last exited: %s", lastBody)
	}
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"tok-new"}`)
	_, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if got, want := fmt.Sprint(status["envs"].(map[string]any)["preview"].(map[string]any)["restarting"]), fmt.Sprint(map[string]any{"restarts": 7.0, "exit_code": 1.0, "said": ""}); got != want {
		t.Fatalf("a secret set after the deploy: %s, want the exit code alone, %s", got, want)
	}
}

// The executor's answer is about the preview it was asked about: a preview that changed before the
// promote route's lock reads unknown, so the promotion fails closed (#1557).
func TestAPreviewThatChangedSinceTheExecutorWasAskedIsNotPromoted(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.Gate = runstest.CleanGolden
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	other, _ := runOf(started)["id"].(string) // a newer clean review of the package preview runs
	if waitVerdict(t, c, base, other)["state"] != "clean" {
		t.Fatalf("the second review: %s", lastBody)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	pkg, _ := status["envs"].(map[string]any)["preview"].(map[string]any)["package"].(string)
	hold := make(chan struct{})
	fake.HoldRunning.Store(&hold)
	t.Cleanup(func() { fake.HoldRunning.Store(nil) })
	asked, swapped := fake.RunningOfs.Load(), make(chan error, 1)
	go func() { // while the executor answers, preview's row changes under the route
		for fake.RunningOfs.Load() == asked {
			time.Sleep(time.Millisecond)
		}
		_, err := pool.Exec(ctx, "UPDATE deploys SET gate_run = $1 WHERE project_id = 'claims-portal' AND env = 'preview'", other)
		swapped <- err
		close(hold)
	}()
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+other+`"}`)
	if err := <-swapped; err != nil {
		t.Fatal(err)
	}
	if line := "Canyon couldn't check whether " + strings.TrimPrefix(pkg, "pkg_") + " runs in preview."; code != 409 || body["error"] != "nothing_in_preview" || body["message"] != line {
		t.Fatalf("a preview changed since the executor was asked: %d %s", code, lastBody)
	}
}

// What the app answered after its deploy is on the status read of an environment Canyon runs, and on no
// other read; an environment the org runs carries none (contracts/routes.md, envs.<env>.answered; #1740).
func TestTheStatusReadSaysWhatTheAppAnswered(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if _, err := pool.Exec(ctx, `UPDATE deploys SET answered = '{"status":400,"said":"<h1>Bad Request (400)</h1>"}' WHERE project_id = 'claims-portal'`); err != nil {
		t.Fatal(err)
	}
	answered := func(path string) any {
		t.Helper()
		_, read, _ := do(t, c, http.MethodGet, base+path, "")
		if project, ok := read["project"].(map[string]any); ok { // the project read wraps its row
			read = project
		}
		envs, _ := read["envs"].(map[string]any)
		preview, _ := envs["preview"].(map[string]any)
		if preview == nil {
			t.Fatalf("%s names no preview: %s", path, lastBody)
		}
		return preview["answered"]
	}
	if got := fmt.Sprint(answered("/api/v1/projects/claims-portal/status")); got != fmt.Sprint(map[string]any{"status": 400.0, "said": "<h1>Bad Request (400)</h1>"}) {
		t.Fatalf("the status read: %s", lastBody)
	}
	if got := answered("/api/v1/projects/claims-portal"); got != nil {
		t.Fatalf("the project read carries answered: %v", got)
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if got := answered("/api/v1/projects/claims-portal/status"); got != nil {
		t.Fatalf("an environment the org runs: %v", got)
	}
}

// A preview the org runs carries no state: the executor is never asked about it, and the receipt is
// evaluated on the status read and in the promote route alike (#1557).
func TestAPreviewTheOrgRunsNeverWaitsOnItsState(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	if err := (&executor.Client{Base: fake.URL}).Stop(ctx, "claims-portal", "preview"); err != nil { // asked, it would read stopped
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	asked := fake.RunningOfs.Load()
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if r := status["receipt"].(map[string]any); r["waiting"] != nil || len(r["rules"].([]any)) != 4 {
		t.Fatalf("the status read: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent'"); err != nil { // refused by a rule, so nothing is promoted
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); code != 409 || body["error"] != "approval_required" {
		t.Fatalf("the promotion is evaluated, never waiting: %d %s", code, lastBody)
	}
	if n := fake.RunningOfs.Load() - asked; n != 0 {
		t.Fatalf("the executor was asked %d times about a preview the org runs", n)
	}
}

// A rule placed Blocking before production after the run joins the receipt the refusal carries,
// contracts/testdata/app-page/claims-portal.json's placed_receipt: the run named is v1.4's while preview
// runs v1.3, so the placed row reads clean-gate's reason, never v1.4's findings. At rest the receipt
// reads v1.3's newest run, whose two high auth.unauthenticated-route findings, advisory before preview,
// block before production until a person approves them — an approval covers its finding in both gates.
// It fails closed on what that run could not read: a rule that was off when it ran, and a rule whose
// AI review was skipped. The rule is placed by the org, or by the app's owner as an addition.
func TestPolicyPlacedRuleRefusesThePromotion(t *testing.T) { placedRefusal(t, "/api/v1/policies/") }

func TestPolicyAdditionPlacedRefusesThePromotion(t *testing.T) { placedRefusal(t, additionAt) }

// placedRefusal is the story above, auth.unauthenticated-route placed through place.
func placedRefusal(t *testing.T, place string) {
	t.Helper()
	withModel(t, &scripted{}) // placing an AI rule needs a model on the host
	pool, ctx, base, c, _ := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/design.org-system?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("design.org-system off: %d %s", code, lastBody)
	}
	atRest := blockedRun(t, c, base) // v1.3 again, design.org-system off: the run the receipt at rest reads
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "b": "2"}))
	run := blockedRun(t, c, base)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_display = 'Dana Whitfield'"); err != nil {
		t.Fatal(err)
	}
	goldenLabels(t, ctx, pool)
	raw, err := os.ReadFile("../../contracts/testdata/app-page/claims-portal.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+place+"auth.unauthenticated-route?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing auth.unauthenticated-route: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
	if want := string(mustJSON(doc["placed_receipt"])); code != 409 || body["error"] != "gate_not_clean" || string(mustJSON(body["receipt"])) != want {
		t.Fatalf("the placed receipt:\n got %d %s\nwant %s", code, mustJSON(body["receipt"]), want)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/design.org-system?apply=true", `{"production":"advisory"}`); code != 200 {
		t.Fatalf("placing design.org-system: %d %s", code, lastBody)
	}
	rows := func() (map[string]string, string) {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		rec, _ := status["receipt"].(map[string]any)
		rules, _ := rec["rules"].([]any)
		why := map[string]string{}
		for _, r := range rules {
			why[r.(map[string]any)["id"].(string)], _ = r.(map[string]any)["why"].(string)
		}
		if status["promote_run"] != atRest {
			t.Fatalf("the receipt at rest reads %v, want %s", status["promote_run"], atRest)
		}
		return why, fmt.Sprint(rec["blocked_by"])
	}
	if why, by := rows(); why["auth.unauthenticated-route"] != "v1.3's review found 2 blocking findings, not approved" || why["design.org-system"] != "v1.3 was reviewed before this check ran; check it again" || by != "[prod.clean-gate auth.unauthenticated-route]" {
		t.Fatalf("the receipt at rest: %v %s", why, by)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'served behind the company proxy', now() + interval '1 day', 'person', 'dana@meridian.example', true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.ruleId == "auth.unauthenticated-route")') f
		WHERE t.run_id = $1 AND t.kind = 'verdict'`, atRest); err != nil { // the two findings, as the approve route keys them
		t.Fatal(err)
	}
	if why, by := rows(); why["auth.unauthenticated-route"] != "v1.3 passed review: every blocking finding approved" || by != "[prod.clean-gate]" {
		t.Fatalf("both approved: %v %s", why, by)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', $1, 'claims-portal', 'check.completed', '{"check": "canyon-review", "status": "skipped", "findings": 0, "duration_ms": 1}')`, atRest); err != nil {
		t.Fatal(err)
	}
	if why, by := rows(); why["auth.unauthenticated-route"] != "v1.3's AI review did not run; check it again once a model is configured" || by != "[prod.clean-gate auth.unauthenticated-route]" {
		t.Fatalf("the AI review skipped: %v %s", why, by)
	}
	// The verdict as a review out of room leaves it: canyon-review's run has no results and says why.
	// timeline_rows is append-only, so its triggers are off for this one write (a superuser test role).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, `UPDATE timeline_rows SET data = jsonb_set(data, '{findings,runs}', (SELECT jsonb_agg(CASE WHEN r#>>'{tool,driver,name}' = 'canyon-review'
		THEN r || jsonb_build_object('results', '[]'::jsonb, 'invocations', jsonb_build_array(jsonb_build_object('executionSuccessful', true,
			'toolExecutionNotifications', jsonb_build_array(jsonb_build_object('level', 'warning', 'message', jsonb_build_object('text', $2::text))))))
		ELSE r END ORDER BY i) FROM jsonb_array_elements(data#>'{findings,runs}') WITH ORDINALITY x(r, i)))
		WHERE run_id = $1 AND kind = 'verdict' AND data#>'{findings,runs}' @> '[{"tool": {"driver": {"name": "canyon-review"}}}]'`, atRest, gate.OutOfRoom); err != nil {
		t.Fatal(err)
	} else if tag.RowsAffected() != 1 {
		t.Fatalf("the verdict has no canyon-review run to run out of room: %d rows", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if why, by := rows(); why["auth.unauthenticated-route"] != "v1.3's AI review ran out of room for its answer; check it again" || by != "[prod.clean-gate auth.unauthenticated-route]" {
		t.Fatalf("the AI review out of room: %v %s", why, by)
	}
	// A split review that left a file unread, its other parts' findings approved: the rule still fails (#2274).
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE timeline_rows SET data = jsonb_set(data, '{findings,runs}', (SELECT jsonb_agg(CASE WHEN r#>>'{tool,driver,name}' = 'canyon-review'
		THEN r || jsonb_build_object('invocations', jsonb_build_array(jsonb_build_object('executionSuccessful', true,
			'toolExecutionNotifications', jsonb_build_array(jsonb_build_object('level', 'warning', 'message', jsonb_build_object('text', $2::text))))))
		ELSE r END ORDER BY i) FROM jsonb_array_elements(data#>'{findings,runs}') WITH ORDINALITY x(r, i)))
		WHERE run_id = $1 AND kind = 'verdict'`, atRest, fmt.Sprintf(gate.TooLargeForACall, `"src/generated/schema.ts"`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if why, by := rows(); why["auth.unauthenticated-route"] != "v1.3's AI review could not read a file too large for one model call; the review names it" || by != "[prod.clean-gate auth.unauthenticated-route]" {
		t.Fatalf("the AI review with a file unread: %v %s", why, by)
	}
}

// A placed row reads only the run clean-gate passes on: with clean-gate advisory, a promotion naming a
// review of another version, or an older review of the version preview runs, that found nothing of the
// placed rule is still refused, its row saying clean-gate's reason — never a pass read off the wrong run
// while the review of what preview runs found two blocking findings (QA's reproduction, #1405).
func TestPolicyPlacedRuleReadsOnlyPreviewsNewestRun(t *testing.T) {
	withModel(t, &scripted{})
	cleanGate(t)
	pool, ctx, base, c := withProject(t)
	pipe.Gate = func(ctx context.Context, dir, tree string, snap gatepolicy.Snapshot, report gate.Report) (finding.Log, error) {
		log, err := runstest.CleanGolden(ctx, dir, tree, snap, report)
		for i := range log.Runs { // nothing of auth.unauthenticated-route
			log.Runs[i].Results = slices.DeleteFunc(log.Runs[i].Results, func(r finding.Result) bool { return strings.HasPrefix(r.RuleID, "auth.") })
		}
		return log, err
	}
	start := func(path string) string {
		t.Helper()
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/"+path, "")
		run, _ := runOf(started)["id"].(string)
		if waitVerdict(t, c, base, run)["state"] != "clean" {
			t.Fatalf("%s: %s", path, lastBody)
		}
		return run
	}
	uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.3"})
	other := start("gate-runs")
	uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), map[string]string{"Canyon-Package-Label": "v1.4"})
	older := start("deploys")
	pipe.Gate = runstest.CleanGolden // the newest review of v1.4: two high auth.unauthenticated-route findings
	start("gate-runs")
	for path, body := range map[string]string{"prod.clean-gate": `{"state":"advisory"}`, "auth.unauthenticated-route": `{"production":"blocking"}`} {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+path+"?apply=true", body); code != 200 {
			t.Fatalf("%s: %d %s", path, code, lastBody)
		}
	}
	for run, want := range map[string]string{other: "the review named is v1.3's; preview runs v1.4", older: "v1.4 has a newer review than the one named"} {
		code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
		rec, _ := body["receipt"].(map[string]any)
		rules, _ := rec["rules"].([]any)
		if code != 409 || len(rules) != 5 || rules[4].(map[string]any)["why"] != want || fmt.Sprint(rec["blocked_by"]) != "[auth.unauthenticated-route]" {
			t.Errorf("promoting %s: %d %s, want the placed row %q", run, code, lastBody, want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'"); n != 0 {
		t.Fatalf("a refused promotion went live: %d", n)
	}
}

// The test cluster's case-sqli-blocked (#1559): a rule advisory before preview and blocking before production
// found one thing, so preview's review passed and the placed row refuses. prod.clean-gate counts that
// finding too, at rest and in the refusal — never "passed review: no blocking finding" above it — until
// a person approves it: then both rows pass and the promotion starts.
func TestCleanGateCountsAPlacedRulesFindingBeforeProduction(t *testing.T) {
	withModel(t, &scripted{})
	appAnswersOK(t)
	pool, ctx, base, c := withProject(t)
	pipe.Gate = func(ctx context.Context, dir, tree string, snap gatepolicy.Snapshot, report gate.Report) (finding.Log, error) {
		log, err := runstest.CleanGolden(ctx, dir, tree, snap, report)
		kept := false
		for i := range log.Runs { // one high auth.unauthenticated-route finding, advisory before preview
			log.Runs[i].Results = slices.DeleteFunc(log.Runs[i].Results, func(r finding.Result) bool {
				if !strings.HasPrefix(r.RuleID, "auth.") || kept {
					return strings.HasPrefix(r.RuleID, "auth.")
				}
				kept = true
				return false
			})
		}
		return log, err
	}
	t.Cleanup(func() { pipe.Gate = runstest.Golden })
	uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.4"})
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("the deploy: %s", lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/auth.unauthenticated-route?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing auth.unauthenticated-route: %d %s", code, lastBody)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	atRest := string(mustJSON(status["receipt"]))
	rec, _ := status["receipt"].(map[string]any)
	rules, _ := rec["rules"].([]any)
	why := func(i int) any { return rules[i].(map[string]any)["why"] }
	if len(rules) != 5 || why(0) != "v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again." || rules[0].(map[string]any)["passed"] != false ||
		why(4) != "v1.4's review found 1 blocking finding, not approved" || fmt.Sprint(rec["blocked_by"]) != "[prod.clean-gate auth.unauthenticated-route]" {
		t.Fatalf("the receipt at rest: %s", atRest)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
	if code != 409 || body["error"] != "gate_not_clean" || body["message"] != "the gate run is not clean" || string(mustJSON(body["receipt"])) != atRest {
		t.Fatalf("the refusal: %d %s\nwant the receipt at rest %s", code, lastBody, atRest)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'"); n != 0 {
		t.Fatalf("a refused promotion went live: %d", n)
	}
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "") // a person approves the finding: both rows pass, and the promotion starts
	fp := ""
	for _, f := range runOf(read)["findings"].([]any) {
		if f := f.(map[string]any); strings.HasPrefix(f["ruleId"].(string), "auth.") {
			fp, _ = f["partialFingerprints"].(map[string]any)["canyon/v1"].(string)
		}
	}
	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/"+fp+"/approve", `{"reason":"every query is parameterised upstream","expires_at":"`+expires+`"}`); code != 200 {
		t.Fatalf("approving %q: %d %s", fp, code, lastBody)
	}
	_, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	rec, _ = status["receipt"].(map[string]any)
	rules, _ = rec["rules"].([]any)
	if len(rules) != 5 || why(0) != "v1.4 passed review: no blocking finding" || rules[0].(map[string]any)["passed"] != true ||
		why(4) != "v1.4 passed review: every blocking finding approved" || fmt.Sprint(rec["blocked_by"]) != "[]" {
		t.Fatalf("the receipt once approved: %s", mustJSON(rec))
	}
	if shipped := promoted(t, c, base, run); shipped["state"] != "clean" {
		t.Fatalf("the promotion once approved: %s", mustJSON(shipped))
	}
}

// A promotion names a run. An older clean run of the package preview runs is not enough once a newer
// run of that same package has blocked: the block is never hidden behind the earlier pass.
func TestPromotingAnOlderRunPastANewerBlockedRunIsRefused(t *testing.T) {
	_, _, base, c, older := previewed(t)
	newer := blockedRun(t, c, base) // no new upload: the golden gate re-reviews the package preview runs
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	pkg, _ := status["envs"].(map[string]any)["preview"].(map[string]any)["package"].(string)
	version := strings.TrimPrefix(pkg, "pkg_") // this upload carries no label, so the reason names it by its id without the prefix
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+older+`"}`)
	why := ""
	if rc, ok := body["receipt"].(map[string]any); ok {
		why, _ = rc["rules"].([]any)[0].(map[string]any)["why"].(string)
	}
	if want := version + " has a newer review than the one named"; code != 409 || body["error"] != "gate_not_clean" || why != want {
		t.Fatalf("promoting %s after %s blocked: %d %s, want 409 gate_not_clean %q", older, newer, code, lastBody, want)
	}
}

// promoted posts a promotion of run, answered 202 with a pending run, and waits for that run to end.
func promoted(t *testing.T, c *http.Client, base, run string) map[string]any {
	t.Helper()
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`)
	if code != 202 || runOf(body)["state"] != "pending" || len(body) != 1 || len(runOf(body)) != 2 {
		t.Fatalf("a promotion starts a run: %d %s", code, lastBody)
	}
	return waitVerdict(t, c, base, runOf(body)["id"].(string))
}

func TestAPromotionShipsThePreviewImageWithNoRebuild(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_display = 'Dana Whitfield'"); err != nil { // who clicks is not who deployed preview
		t.Fatal(err)
	}
	builds, ran := fake.Builds.Load(), fake.Runs.Load()
	run := promoted(t, c, base, deploy)
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	envs := read["project"].(map[string]any)["envs"].(map[string]any)
	prod, preview := envs["prod"].(map[string]any), envs["preview"].(map[string]any)
	if run["state"] != "clean" || prod["url"] != "http://claims-portal.prod.localhost:7777" ||
		prod["package"] != preview["package"] || prod["image_digest"] != preview["image_digest"] || prod["gate_run"] != deploy || !reflect.DeepEqual(prod["deployed_by"], preview["deployed_by"]) ||
		!reflect.DeepEqual(prod["promoted_by"], map[string]any{"kind": "person", "subject": "anonymous", "display": "Dana Whitfield"}) || prod["run_by"] != "canyon" || len(prod) != 8 {
		t.Fatalf("the promotion is the project read's envs.prod:\n%s\n%s", mustJSON(run), mustJSON(envs))
	}
	if r := fake.Ran(); fake.Builds.Load() != builds || fake.Runs.Load() != ran+1 || r.Target != "prod" || r.Image != "canyon/claims-portal@"+executortest.Digest {
		t.Fatalf("one /run of preview's image in prod and no build: %d builds, %d runs, %+v", fake.Builds.Load()-builds, fake.Runs.Load()-ran, r)
	}
	asked := "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/promotions' AND run_id = '" + run["id"].(string) + "' AND detail = '{\"package\":\"" + preview["package"].(string) + "\",\"gate_run\":\"" + deploy + "\"}'::jsonb"
	shipped := "SELECT count(*) FROM audit_events WHERE action = 'deploy prod' AND detail->>'url' = 'http://claims-portal.prod.localhost:7777'"
	var first, last time.Time
	if err := pool.QueryRow(ctx, "SELECT deployed_at FROM deploys WHERE env = 'prod'").Scan(&first); err != nil || count(t, ctx, pool, asked) != 1 || count(t, ctx, pool, shipped) != 1 {
		t.Fatalf("one audit row asking, one shipping: %v", err)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if status["last_run"] != deploy || len(status["receipt"].(map[string]any)["blocked_by"].([]any)) != 0 {
		t.Fatalf("a promote run is not the gate run the receipt reads: %s", lastBody)
	}
	_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if listed := list["runs"].([]any); len(listed) != 1 || listed[0].(map[string]any)["id"] != deploy || fleet["apps"].([]any)[0].(map[string]any)["last_run"] != deploy {
		t.Fatalf("the gate-runs list and the fleet read gate runs only:\n%s\n%s", mustJSON(list), mustJSON(fleet))
	}
	if again := promoted(t, c, base, deploy); again["state"] != "clean" || fake.Runs.Load() != ran+2 { // started again: the prod values may have changed
		t.Fatalf("the same package again: %s, %d runs", mustJSON(again), fake.Runs.Load()-ran)
	}
	if err := pool.QueryRow(ctx, "SELECT deployed_at FROM deploys WHERE env = 'prod'").Scan(&last); err != nil || count(t, ctx, pool, shipped) != 2 || !last.After(first) {
		t.Fatalf("promoted twice, one row: deployed_at %s then %s (%v)", first, last, err)
	}
}

// A promotion never starts beside a running one: 409 with the run in its way.
func TestAPromotionWhileARunIsActiveIsRefused(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) SELECT 'gr_busy00000', project_id, package_id, 'running', 'promote' FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	if code != 409 || body["error"] != "run_in_progress" || runOf(body)["id"] != "gr_busy00000" || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 1 {
		t.Fatalf("a promotion beside a running run: %d %s", code, lastBody)
	}
}

func TestAPromotionThatCannotStartTheContainerRecordsNothing(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	fake.FailRun = true
	if run := promoted(t, c, base, deploy); run["state"] != "failed" || run["failure_reason"] != "error" {
		t.Fatalf("the executor refused: %s", mustJSON(run))
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'deploy prod'"); n != 0 {
		t.Fatalf("a promotion whose container never started wrote %d rows", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'preview' AND gate_run = '"+deploy+"'"); n != 1 {
		t.Fatalf("preview moved: %d", n)
	}
}

func TestApproveValidatesWhoApprovesTheBodyAndTheFinding(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	var fp string // read off the verdict, never written into a test: a fingerprint literal is secret-shaped
	if err := pool.QueryRow(ctx, `SELECT f->'partialFingerprints'->>'canyon/v1' FROM timeline_rows t,
		jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f WHERE t.run_id = $1 AND t.kind = 'verdict'`, run).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	url := base + "/api/v1/gate-runs/" + run + "/findings/" + fp + "/approve"
	good := func(reason string) string {
		return `{"reason":"` + reason + `","expires_at":"` + at(90*24*time.Hour) + `"}`
	}
	for i, tc := range []struct {
		url, body string
		code      int
		want      string
	}{
		{url, `{"reason":"","expires_at":"` + at(time.Hour) + `"}`, 400, "invalid_reason"},
		{url, good(strings.Repeat("é", 1025)), 400, "invalid_reason"},
		{url, `{"reason":"r","expires_at":"next week"}`, 400, "invalid_expires_at"},
		{url, `{"reason":"r","expires_at":"` + at(-time.Hour) + `"}`, 400, "invalid_expires_at"},
		{url, `{"reason":"r","expires_at":"` + at(91*24*time.Hour) + `"}`, 400, "invalid_expires_at"},
		{url, `{"reason":"r","expires_at":"` + time.Now().Add(time.Hour).In(time.FixedZone("", 2*3600)).Format(time.RFC3339) + `"}`, 400, "invalid_expires_at"},
		{url, `{"reason":"r"}`, 400, "invalid_body"},
		{url, `{"reason":"r","expires_at":"` + at(time.Hour) + `","rule":"secrets.client-bundle"}`, 400, "invalid_body"}, // the schema allows no other field
		{url, `{"reason":"a\u0000b","expires_at":"` + at(time.Hour) + `"}`, 400, "invalid_reason"},                       // Postgres text refuses a NUL
		{url, good("r") + `{}`, 400, "invalid_body"},
		{base + "/api/v1/gate-runs/gr_nothing/findings/" + fp + "/approve", good("r"), 404, "not_found"},
		{strings.Replace(url, fp, "f_"+strings.Repeat("0", 64), 1), good("r"), 404, "not_found"},
	} {
		if code, body, _ := do(t, c, http.MethodPost, tc.url, tc.body); code != tc.code || body["error"] != tc.want {
			t.Fatalf("case %d, %s: %d %v", i, tc.body, code, body["error"])
		}
	}
	for _, kind := range []string{"agent", "canyon"} { // only a person approves: the agent a gate stopped cannot approve its own way past it
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1", kind); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := do(t, c, http.MethodPost, url, good("r")); code != 403 || body["error"] != "person_required" {
			t.Fatalf("%s approving: %d %s", kind, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person'"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%approve'"); n != 0 {
		t.Fatalf("a refused approval wrote %d rows", n)
	}
	reason := strings.Repeat("é", 1024) // characters, not bytes
	expires := at(90 * 24 * time.Hour)
	want := `{"approval":{"approved_by":{"kind":"person","subject":"anonymous","display":""},"expires_at":"` + expires + `","fingerprint":"<the verdict's>","reason":"` + reason + `","rule":"secrets.client-bundle.gitleaks.stripe-access-token"}}`
	if code, _, _ := do(t, c, http.MethodPost, url, `{"reason":"`+reason+`","expires_at":"`+expires+`"}`); code != 200 || strings.ReplaceAll(lastBody, fp, "<the verdict's>") != want {
		t.Fatalf("the approval:\n got %d %s\nwant %s", code, strings.ReplaceAll(lastBody, fp, "<the verdict's>"), want)
	}
	do(t, c, http.MethodPost, url, `{"reason":"Placeholder key in a demo fixture, not a live credential; rotation is ticketed","expires_at":"`+expires+`"}`)
	stored := count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE project_id = 'claims-portal' AND rule = 'secrets.client-bundle.gitleaks.stripe-access-token' AND fingerprint = '"+fp+"' AND reason LIKE 'Placeholder%' AND expires_at = '"+expires+"' AND approved_by_subject = 'anonymous'")
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve' AND run_id = '"+run+"' AND detail ?& array['rule', 'fingerprint', 'reason', 'expires_at']")
	if stored != 1 || count(t, ctx, pool, "SELECT count(*) FROM approvals") != 1 || audited != 2 {
		t.Fatalf("approved twice: %d rows under the key, %d audit rows", stored, audited)
	}
}

// approval_required is the refusal a person clears by clicking promote, so it is only the
// human-approval rule blocking alone. An unowned app under owner.required set blocking is refused
// too — a click gives no app an owner — and that is gate_not_clean, with blocked_by naming it.
func TestARuleTheOrgTurnedBlockingIsNotTheApprovalAClickClears(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t) // previewed sets no owner
	edit(t, ctx, pool, "owner.required", "blocking", "0")
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
	blockedBy := ""
	if rec, ok := body["receipt"].(map[string]any); ok {
		blockedBy = fmt.Sprint(rec["blocked_by"])
	}
	if code != 409 || body["error"] != "gate_not_clean" || blockedBy != "[owner.required]" {
		t.Fatalf("an unowned app under owner.required blocking: %d %s", code, lastBody)
	}
}

// stack.blessed reads the buildpacks preview's image ran against the approved buildpacks an admin set,
// by id, naming each by its recorded name: the stacks agents are told set nothing to check against, a
// buildpack off the list fails it, all on the list pass it, and a deploy that recorded none says so
// (contracts/routes.md, The promote receipt; #636). GET /preferences offers what the deploys ran.
func TestStackBlessedReadsTheBuildpacksPreviewRan(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t) // the fake build ran node-engine and npm-install
	blessed := func() map[string]any {
		t.Helper()
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		rule := body["receipt"].(map[string]any)["rules"].([]any)[2].(map[string]any)
		if rule["id"] != "stack.blessed" {
			t.Fatalf("the third rule: %s", mustJSON(rule))
		}
		return rule
	}
	for _, c2 := range []struct {
		put, why string
		passed   bool
		unset    any
	}{
		{`{"stacks":[{"language":"TypeScript","frameworks":["React"]}]}`, "no approved stacks set", false, true},
		{`{"approved_buildpacks":["paketo-buildpacks/node-engine"]}`, "not approved: Paketo Buildpack for NPM Install", false, nil},
		{`{"approved_buildpacks":["paketo-buildpacks/npm-install","paketo-buildpacks/node-engine"]}`, "built with Paketo Buildpack for Node Engine, Paketo Buildpack for NPM Install", true, nil},
	} {
		if code, _ := putPreferences(t, c, base, c2.put); code != 200 {
			t.Fatalf("%s: %d %s", c2.put, code, lastBody)
		}
		if rule := blessed(); rule["why"] != c2.why || rule["passed"] != c2.passed || rule["unset"] != c2.unset {
			t.Fatalf("after %s: %s", c2.put, mustJSON(rule))
		}
	}
	prefs := getPreferences(t, c, base)
	if got := string(mustJSON(prefs["approved_buildpacks"])); got != `["paketo-buildpacks/node-engine","paketo-buildpacks/npm-install"]` {
		t.Fatalf("the approved list is kept sorted by id: %s", got)
	}
	seen := prefs["buildpacks_seen"]
	if got := string(mustJSON(seen)); got != `[{"id":"paketo-buildpacks/node-engine","name":"Paketo Buildpack for Node Engine"},{"id":"paketo-buildpacks/npm-install","name":"Paketo Buildpack for NPM Install"}]` {
		t.Fatalf("buildpacks_seen: %s", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE deploys SET buildpacks = NULL`); err != nil { // a deploy from before the column
		t.Fatal(err)
	}
	if rule := blessed(); rule["why"] != "no stack recorded" || rule["passed"] != false {
		t.Fatalf("a deploy that recorded none: %s", mustJSON(rule))
	}
}
