package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/contracts"
)

func runOf(body map[string]any) map[string]any {
	r, _ := body["run"].(map[string]any)
	return r
}

// waitVerdict polls the run read until the run leaves pending and running.
func waitVerdict(t *testing.T, c *http.Client, base, run string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
		if s := runOf(body)["state"]; s != "pending" && s != "running" {
			return runOf(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached a verdict: %s", run, lastBody)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStartingAGateRunAnswers202AuditsTheRunAndDefaultsToTheCurrentPackage(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	_, second := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}))
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(body)["id"].(string)
	if code != 202 || !strings.HasPrefix(run, "gr_") || runOf(body)["state"] != "pending" || len(runOf(body)) != 2 {
		t.Fatalf("start: %d %s", code, lastBody)
	}
	var pkg string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&pkg); err != nil || pkg != packageOf(second)["id"] {
		t.Fatalf("the run reviews the current package: %q (%v), want %v", pkg, err, packageOf(second)["id"])
	}
	var auditRun, auditPkg *string
	if err := pool.QueryRow(ctx, "SELECT run_id, detail->>'package' FROM audit_events WHERE action = $1", "POST /api/v1/projects/{id}/gate-runs").Scan(&auditRun, &auditPkg); err != nil || auditRun == nil || *auditRun != run || auditPkg == nil || *auditPkg != pkg {
		t.Fatalf("the audit row names the run and its package: %v %v (%v)", auditRun, auditPkg, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE scope = 'org' AND kind = 'audit.appended' AND project_id = 'claims-portal'"); n != 4 {
		t.Fatalf("org rows (create, two uploads, the start): %d", n)
	}
	waitVerdict(t, c, base, run) // let the workflow finish before the schema goes away
}

func TestStartingAGateRunWithANamedPackageUsesItAndRefusesAnotherProjects(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"payroll"}`)
	_, ours := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	_, theirs := upload(t, c, base, "payroll", tarOf(t, map[string]string{"b": "2"}), "", "")
	if code, _ := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"})); code != 201 { // the head moves on; the named package is still reviewable
		t.Fatalf("the head moving on: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+packageOf(theirs)["id"].(string)+`"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("another project's package: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+packageOf(ours)["id"].(string)+`"}`)
	run, _ := runOf(body)["id"].(string)
	var pkg string
	if code != 202 || pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&pkg) != nil || pkg != packageOf(ours)["id"] {
		t.Fatalf("the named package: %d %s → %q", code, lastBody, pkg)
	}
	waitVerdict(t, c, base, run)
}

func TestStartingAGateRunRefusesAnUnknownADeletedAnEmptyAndAMalformedProjectAndLeavesNoAuditRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	_, gonePkg := upload(t, c, base, "gone", tarOf(t, map[string]string{"a": "1"}), "", "")
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	// a deleted project's own package, named: refused, and no run row commits behind the 404
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/gone/gate-runs", `{"package":"`+packageOf(gonePkg)["id"].(string)+`"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("a deleted project's package by name: %d %s", code, lastBody)
	}
	for id, message := range map[string]string{"nothing": "nothing isn't on this Canyon yet. Deploy it first: canyon deploy", "gone": "gone isn't on this Canyon yet. Deploy it first: canyon deploy", "claims-portal": "no package yet", "Claims": `"Claims" isn't on this Canyon yet. Deploy it first: canyon deploy`, "%1B%5B2J": `"\x1b[2J" isn't on this Canyon yet. Deploy it first: canyon deploy`} {
		if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/"+id+"/gate-runs", ""); code != 404 || body["error"] != "not_found" || body["message"] != message {
			t.Fatalf("%s: %d %s, want %q", id, code, lastBody, message)
		}
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":`); code != 400 || body["error"] != "bad_request" {
		t.Fatalf("a broken body: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events") + count(t, ctx, pool, "SELECT count(*) FROM runs"); n != before {
		t.Fatalf("refused starts left %d rows", n-before)
	}
}

func TestARunInProgressAnswers409WithTheRunAndAPendingOneIsSuperseded(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	pkg, _ := packageOf(body)["id"].(string)
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_busy00000', 'claims-portal', $1, 'running')", pkg); err != nil {
		t.Fatal(err)
	}
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", ""); code != 409 || lastBody != `{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_busy00000","state":"running"}}` {
		t.Fatalf("a run in progress: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("the 409 left %d audit rows", n-before)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'pending' WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(body)["id"].(string)
	if code != 202 || run == "gr_busy00000" {
		t.Fatalf("a pending run is superseded by the new one: %d %s", code, lastBody)
	}
	var state string
	var reason *string
	var ended bool
	if err := pool.QueryRow(ctx, "SELECT state, failure_reason, ended_at IS NOT NULL FROM runs WHERE id = 'gr_busy00000'").Scan(&state, &reason, &ended); err != nil || state != "failed" || reason == nil || *reason != "interrupted" || !ended {
		t.Fatalf("the superseded run: %s %v ended=%v (%v)", state, reason, ended, err)
	}
	waitVerdict(t, c, base, run)
}

func TestAGateRunReachesItsVerdictAndTheRunReadShowsItsFindings(t *testing.T) {
	_, _, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "package.json": "{}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	got := waitVerdict(t, c, base, run)
	findings, _ := got["findings"].([]any)
	if got["state"] != "blocked" || got["blocking"] != 1.0 || got["warnings"] != 3.0 || len(findings) != 4 || got["package"] != packageOf(body)["id"] {
		t.Fatalf("the golden log blocks with one blocking and three advisory findings: %s", lastBody)
	}
	if _, has := got["failure_reason"]; has || len(got) != 9 || got["project"] != "claims-portal" || got["approvable"] != true { // sign-in off: the anonymous admin approves; a check's: no deploys
		t.Fatalf("the run read's keys: %s", lastBody)
	}
	last, _ := findings[3].(map[string]any) // the reviewer's three, then gitleaks' key
	canyon, _ := last["properties"].(map[string]any)["canyon"].(map[string]any)
	if last["ruleId"] != "secrets.client-bundle.gitleaks.stripe-access-token" || canyon["blocks"] != true {
		t.Fatalf("findings[] are the verdict log's results in log order: %v", last)
	}
}

// A failed detect's read names what the host builds, [] when its builder names none, and says when a
// buildpack errored; no other read carries either, even off a row that kept them (contracts/routes.md,
// the run read; #1242).
func TestAFailedDetectsRunReadNamesWhatTheHostBuilds(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	pkg, _ := packageOf(body)["id"].(string)
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, builds, ended_at)
		VALUES ('gr_detect0000', 'claims-portal', $1, 'failed', 'detect', '{paketo-buildpacks/ruby,paketo-buildpacks/nodejs}', now())`, pkg); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_detect0000", "")
		return runOf(body)
	}
	if got := read(); got["failure_reason"] != "detect" || fmt.Sprint(got["builds"]) != "[paketo-buildpacks/ruby paketo-buildpacks/nodejs]" || strings.Contains(lastBody, "errored") {
		t.Fatalf("a failed detect names what the host builds: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET detect_errored = true WHERE id = 'gr_detect0000'"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got["errored"] != true {
		t.Fatalf("a buildpack that errored while detecting is said: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET builds = NULL WHERE id = 'gr_detect0000'"); err != nil { // a row the build step never wrote builds onto
		t.Fatal(err)
	}
	if read(); !strings.Contains(lastBody, `"builds":[]`) {
		t.Fatalf("a failed detect always carries builds, [] when nothing names any: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET failure_reason = 'interrupted' WHERE id = 'gr_detect0000'"); err != nil { // builds and detect_errored stay on the row
		t.Fatal(err)
	}
	if read(); strings.Contains(lastBody, "builds") || strings.Contains(lastBody, "errored") {
		t.Fatalf("only a failed detect carries builds or errored: %s", lastBody)
	}
}

// A failed run's read names the file a stock starter is missing from the run's facts: the package's
// root file names for a failed detect, the buildpacks that ran for a failed start, a reused image's
// from its package's build (contracts/routes.md, What a starter is missing; #1654).
func TestAStartersRunReadNamesTheMissingFile(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	fixOf := func(run string) map[string]any {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
		fix, _ := runOf(body)["fix"].(map[string]any)
		return fix
	}
	insert := func(run, pkg, state, reason, buildpacks string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, buildpacks, ended_at)
			VALUES ($1, 'claims-portal', $2, $3, nullif($4, ''), nullif($5, '')::jsonb, now())`, run, pkg, state, reason, buildpacks); err != nil {
			t.Fatal(err)
		}
	}
	_, body := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"index.html": "<h1>hi</h1>\n"}))
	static, _ := packageOf(body)["id"].(string)
	insert("gr_static0000", static, "failed", "detect", "")
	if fix := fixOf("gr_static0000"); fix["file"] != "project.toml" || !strings.Contains(lastBody, `value = \".\"`) {
		t.Fatalf("a static site that no buildpack detected is missing a project.toml: %s", lastBody)
	}
	_, body = uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"index.html": "<h1>hi</h1>\n", "project.toml": "[_]\n"}))
	toml, _ := packageOf(body)["id"].(string)
	insert("gr_toml00000", toml, "failed", "detect", "")
	if fix := fixOf("gr_toml00000"); fix != nil {
		t.Fatalf("a package with a project.toml is never told to add one: %s", lastBody)
	}
	python := `[{"id": "paketo-buildpacks/cpython"}, {"id": "paketo-buildpacks/python-start"}]`
	insert("gr_python000", static, "failed", "start", python)
	insert("gr_reused000", static, "failed", "start", "") // the next deploy of the package reused its image
	for _, run := range []string{"gr_python000", "gr_reused000"} {
		if fix := fixOf(run); fix["file"] != "Procfile" || !strings.Contains(lastBody, "web: uvicorn main:app --host 0.0.0.0 --port $PORT") {
			t.Fatalf("%s: a Python build with no Procfile is missing one: %s", run, lastBody)
		}
	}
	insert("gr_clean0000", static, "clean", "", python)
	if fix := fixOf("gr_clean0000"); fix != nil || strings.Contains(lastBody, "fix") {
		t.Fatalf("only a failed run names a missing file: %s", lastBody)
	}
}

// A run Canyon itself failed in names the step on its read and on the runs list, and the receipt's
// reason says it in words (contracts/routes.md, A failure reason in words; #1645).
func TestARunCanyonFailedInNamesTheStep(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	pkg, _ := packageOf(body)["id"].(string)
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, failed_in, ended_at)
		VALUES ('gr_scanfail00', 'claims-portal', $1, 'failed', 'error', 'scan', now())`, pkg); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_scanfail00", ""); runOf(body)["failed_in"] != "scan" {
		t.Fatalf("the run read: %s", lastBody)
	}
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", ""); !strings.Contains(lastBody, `"failure_reason":"error","failed_in":"scan"`) {
		t.Fatalf("the runs list: %s", lastBody)
	}
	facts, _, err := receiptFacts(ctx, pool, "claims-portal", "gr_scanfail00", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, why := cleanGateRule(receiptInput{run: facts, preview: &pkgRef{id: pkg}}); !strings.HasSuffix(why, "'s run failed: Canyon itself failed while scanning the package") {
		t.Fatalf("the receipt's reason: %q", why)
	}
}

func TestTheRunReadAnswersTheSeamShapeBeforeAVerdictAndAFailedRunShowsItsReason(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	pkg, _ := packageOf(body)["id"].(string)
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, deploys) VALUES ('gr_pend000000', 'claims-portal', $1, 'pending', false)", pkg); err != nil {
		t.Fatal(err)
	}
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_pend000000", ""); lastBody != `{"run":{"id":"gr_pend000000","package":"`+pkg+`","state":"pending","project":"claims-portal","approvable":true,"blocking":0,"warnings":0,"progress":[],"findings":[]}}` {
		t.Fatalf("before the verdict: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_pend000000'"); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_pend000000", ""); runOf(body)["state"] != "failed" || runOf(body)["failure_reason"] != "interrupted" {
		t.Fatalf("a failed run shows its reason: %s", lastBody)
	}
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_nothing", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown run: %d %s", code, lastBody)
	}
}

func TestUploadPullAndAGateRunAgreeOnTheCurrentPackage(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, first := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	_, second := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}))
	_, pulled, _ := pull(t, c, base, "claims-portal")
	_, stale := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), packageOf(first)["id"].(string), "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	var reviewed string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&reviewed); err != nil {
		t.Fatal(err)
	}
	if want := packageOf(second)["id"]; pulled != want || stale["head_package"] != want || reviewed != want {
		t.Fatalf("one rule for current: pulled %q, stale-base head %v, reviewed %q, want %v", pulled, stale["head_package"], reviewed, want)
	}
	waitVerdict(t, c, base, run)
}

func TestStartingAGateRunWithAnEmptyChunkedBodyOrAnEmptyObjectUsesTheCurrentPackage(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", struct{ io.Reader }{strings.NewReader("")}) // an unknown-length body: the CLI's, or curl's, with nothing in it
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	run, _ := runOf(out)["id"].(string)
	if resp.StatusCode != 202 || run == "" {
		t.Fatalf("an empty chunked body: %d %v", resp.StatusCode, out)
	}
	var pkg string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&pkg); err != nil || pkg != packageOf(body)["id"] {
		t.Fatalf("the current package: %q (%v)", pkg, err)
	}
	waitVerdict(t, c, base, run)
	code, out2, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{}`)
	run2, _ := runOf(out2)["id"].(string)
	if code != 202 || run2 == "" {
		t.Fatalf("an empty object: %d %s", code, lastBody)
	}
	waitVerdict(t, c, base, run2)
}

// The 31st gate run of a project within the hour answers 429 with when to try again, on gate-runs and deploys
// alike; superseded runs count, promotions do not, and another project is not held to it.
func TestTheThirtyFirstRunOfAProjectWithinTheHourAnswers429(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"payroll"}`)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	upload(t, c, base, "payroll", tarOf(t, map[string]string{"b": "1"}), "", "")
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, kind, created_at)
		SELECT 'gr_old' || n, 'claims-portal', $1, 'failed', 'interrupted', CASE WHEN n > 29 THEN 'promote' ELSE 'gate' END, now() - interval '10 minutes' FROM generate_series(1, 32) n`, packageOf(body)["id"]); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	thirtieth, _ := runOf(body)["id"].(string)
	if code != 202 {
		t.Fatalf("the 30th run, three promotions not counted: %d %s", code, lastBody)
	}
	for _, route := range []string{"gate-runs", "deploys"} {
		code, body, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/"+route, "")
		if retry, _ := body["retry_after_s"].(float64); code != 429 || body["error"] != "rate_limited" || retry < 2900 || retry > 3000 ||
			body["message"] != "Limit reached: 30 reviews an hour for claims-portal. Try again in 50 minutes." {
			t.Fatalf("the 31st %s: %d %s", route, code, lastBody)
		}
	}
	code, body, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/payroll/gate-runs", "")
	other, _ := runOf(body)["id"].(string)
	if code != 202 {
		t.Fatalf("another project: %d %s", code, lastBody)
	}
	// counted whatever their state, which the worker moves on meanwhile: the 30th, and nothing a 429 started
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE project_id = 'claims-portal' AND id NOT LIKE 'gr_old%'"); n != 1 {
		t.Fatalf("a 429 started a run: %d runs", n)
	}
	if verdict := waitVerdict(t, c, base, thirtieth); verdict["failure_reason"] == "interrupted" {
		t.Fatalf("a 429 superseded the 30th run: %v", verdict)
	}
	waitVerdict(t, c, base, other)
}

// sarifOf reads the run's SARIF export: its status, its content type, and the body as sent.
func sarifOf(t *testing.T, c *http.Client, base, run string) (int, string, string) {
	t.Helper()
	code, _, resp := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/sarif", "")
	return code, resp.Header.Get("Content-Type"), lastBody
}

// The export is the verdict log, still SARIF 2.1.0 in Canyon's profile; an unexpired approval
// travels as a suppression carrying its reason and nothing of who approved; a read writes no row.
func TestTheSARIFExportCarriesApprovalsAsSuppressionsWithTheReasonOnly(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	code, kind, body := sarifOf(t, c, base, run)
	if code != 200 || kind != "application/sarif+json" || strings.Contains(body, "suppressions") {
		t.Fatalf("before any approval: %d %s %s", code, kind, body)
	}
	if err := contracts.CheckFindingSchemas([]byte(body)); err != nil {
		t.Fatalf("the export is not SARIF in the profile: %v", err)
	}
	approve(t, ctx, pool, "now() + interval '1 hour'")
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	code, _, body = sarifOf(t, c, base, run)
	if err := contracts.CheckFindingSchemas([]byte(body)); code != 200 || err != nil {
		t.Fatalf("with a suppression the export is still SARIF in the profile: %d %v", code, err)
	}
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID       string           `json:"ruleId"`
				Suppressions []map[string]any `json:"suppressions"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatal(err)
	}
	suppressed := 0
	for _, r := range log.Runs {
		for _, res := range r.Results {
			if res.RuleID != "secrets.client-bundle.gitleaks.stripe-access-token" { // the golden log's one blocking finding is the one approved
				if res.Suppressions != nil {
					t.Fatalf("a finding nothing approves carries a suppression: %s", body)
				}
				continue
			}
			if len(res.Suppressions) != 1 || res.Suppressions[0]["kind"] != "external" || res.Suppressions[0]["justification"] != "demo fixture" || len(res.Suppressions[0]) != 2 {
				t.Fatalf("the approved finding's suppression: %v", res.Suppressions)
			}
			suppressed++
		}
	}
	if suppressed != 1 {
		t.Fatalf("one finding is approved, %d suppressed: %s", suppressed, body)
	}
	for _, who := range []string{"dana@meridian.example", "approved_by", `"person"`} {
		if strings.Contains(body, who) {
			t.Fatalf("the export names who approved (%q): %s", who, body)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != audited {
		t.Fatalf("a read wrote %d audit rows", n-audited)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, _, body = sarifOf(t, c, base, run); strings.Contains(body, "suppressions") {
		t.Fatalf("an expired approval suppresses nothing: %s", body)
	}
}

func TestTheSARIFExportRefusesAnUnknownRunAndOneWithNoVerdict(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_pend000000', 'claims-portal', $1, 'pending')", packageOf(body)["id"]); err != nil {
		t.Fatal(err)
	}
	if code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_pend000000/sarif", ""); code != 409 || got["error"] != "no_verdict" {
		t.Fatalf("a run with no verdict: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_nothing/sarif", ""); code != 404 || got["error"] != "not_found" {
		t.Fatalf("an unknown run: %d %s", code, lastBody)
	}
}

// A clean run whose verdict log names what nothing read is never read as clean: the run read says
// not_checked, and a clean run without such a note carries no key (contracts/routes.md, the run read).
func TestARunReadSaysWhenSomethingWasNotChecked(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedVerdict(t, ctx, pool, "claims-portal", "gr_plain")
	if _, err := pool.Exec(ctx, `
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_gr_unread', 'claims-portal', 't', 'c', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_unread', 'claims-portal', 'pkg_gr_unread', 'clean', now());
INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', 'gr_unread', 'claims-portal', 'verdict', '{"state": "clean", "findings": {"runs": [{"results": [],
  "invocations": [{"executionSuccessful": true, "toolExecutionNotifications": [{"level": "warning", "message": {"text": "not checked: sql.string-concat: Ruby (3 files) — no rule Canyon runs reads Ruby"}}]}]}]}}')`); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]any{"gr_plain": nil, "gr_unread": true} {
		code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
		read, _ := got["run"].(map[string]any)
		if code != 200 || read["state"] != "clean" || read["not_checked"] != want {
			t.Errorf("%s: %d %s", run, code, lastBody)
		}
	}
}

// Each finding carries its entry, the index of its entry's first finding (contracts/finding.md, One
// problem, one entry; #2013): two opengrep rules on server.js:10 are one entry, the reviewer's finding
// on that line its own, so every screen and the CLI count one problem as one. The read's own counts
// stay its findings' — a CLI released before entries checks them against the list — whatever the
// verdict row counts: the entries, 2 here.
func TestTheRunReadNumbersEachFindingsEntry(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if _, err := pool.Exec(ctx, `
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_gr_joined', 'claims-portal', 't', 'c', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_joined', 'claims-portal', 'pkg_gr_joined', 'blocked', now());
INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', 'gr_joined', 'claims-portal', 'verdict', '{"state": "blocked", "blocking": 2, "warnings": 0, "findings": {"runs": [
  {"tool": {"driver": {"name": "sarif/opengrep"}}, "results": [
    {"ruleId": "sql.injection.opengrep.tainted-sql", "partialFingerprints": {"canyon/v1": "aaaaaaaaaaaaaaaa"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "server.js"}, "region": {"startLine": 10}}}], "properties": {"canyon": {"blocks": true}}},
    {"ruleId": "sql.injection.opengrep.express-sqli", "partialFingerprints": {"canyon/v1": "bbbbbbbbbbbbbbbb"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "server.js"}, "region": {"startLine": 10}}}], "properties": {"canyon": {"blocks": true}}}]},
  {"tool": {"driver": {"name": "canyon-review"}}, "results": [
    {"ruleId": "auth.unauthenticated-route", "partialFingerprints": {"canyon/v1": "cccccccccccccccc"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "server.js"}, "region": {"startLine": 10}}}], "properties": {"canyon": {"blocks": true}}}]}]}}')`); err != nil {
		t.Fatal(err)
	}
	_, got, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_joined", "")
	findings, _ := got["run"].(map[string]any)["findings"].([]any)
	var entries []any
	for _, f := range findings {
		entries = append(entries, f.(map[string]any)["entry"])
	}
	if want := []any{0.0, 0.0, 2.0}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries %v, want %v: %s", entries, want, lastBody)
	}
	if read := got["run"].(map[string]any); read["blocking"] != 3.0 || read["warnings"] != 0.0 {
		t.Fatalf("the read counts its findings, three: %s", lastBody)
	}
}
