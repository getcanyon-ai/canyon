package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5/pgxpool"
)

type obj = map[string]any

// reportOf is a SARIF report of one run by driver carrying results.
func reportOf(driver string, results ...any) string {
	return string(mustJSON(obj{"version": "2.1.0", "runs": []any{obj{"tool": obj{"driver": obj{"name": driver, "version": "1.86.0"}}, "results": results}}}))
}

// semgrepGolden is semgrep's result on the claims-portal app as the contract's imported golden came from it,
// and that golden's result as attach stores it: no partialFingerprints, blocks not yet derived.
func semgrepGolden(t *testing.T) (obj, finding.Result) {
	t.Helper()
	var log finding.Log
	raw, err := os.ReadFile("../../contracts/testdata/finding/imported-semgrep.sarif.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &log)); err != nil {
		t.Fatal(err)
	}
	want, blocks := log.Runs[0].Results[0], true
	want.PartialFingerprints, want.Properties.Canyon.Blocks = finding.Fingerprints{}, &blocks
	return obj{"ruleId": "typescript.nextjs.unvalidated-search-param", "level": "warning", "message": obj{"text": want.Properties.Canyon.Problem},
		"locations": []any{obj{"physicalLocation": obj{"artifactLocation": obj{"uri": "apps/web/src/app/api/claims/route.ts"},
			"region": obj{"startLine": 8, "startColumn": 51, "endLine": 8, "endColumn": 84}}}}}, want
}

// withReports is withProject holding the claims-portal app as its package, semgrep added as a tool; it
// answers the package's id and its reports URL.
func withReports(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client, string, string) {
	t.Helper()
	pool, ctx, base, c := withProject(t)
	files, app := map[string]string{}, os.DirFS("../../gate/testdata/apps/claims-portal")
	if err := fs.WalkDir(app, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(app, p)
		files[p] = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, body := upload(t, c, base, "claims-portal", tarOf(t, files), "", "")
	if code, _, _ := do(t, c, http.MethodPut, base+toolPath+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("add semgrep: %d %s", code, lastBody)
	}
	pkg, _ := packageOf(body)["id"].(string)
	return pool, ctx, base, c, pkg, base + "/api/v1/projects/claims-portal/packages/" + pkg + "/reports"
}

func digestOf(report string) string {
	sum := sha256.Sum256([]byte(report))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The contract's golden report attaches to the claims-portal package: the answer counts it, the row
// keeps its digest, the attacher and its result as the golden's, and one audit row replaces nothing.
// A second attach, to the package once it is a pull request's, replaces it and names what it replaced.
func TestTheGoldenReportAttachesAndASecondReplacesIt(t *testing.T) {
	pool, ctx, _, c, pkg, reports := withReports(t)
	result, want := semgrepGolden(t)
	first := reportOf("Semgrep", result)
	if code, _, _ := do(t, c, http.MethodPut, reports, first); code != 200 || lastBody != `{"tool":"semgrep","category":"","results":1,"dropped":0}` {
		t.Fatalf("the golden report: %d %s", code, lastBody)
	}
	var digest, by string
	var stored []finding.Result
	if err := pool.QueryRow(ctx, "SELECT digest, attached_by_kind || '/' || attached_by_subject, results FROM package_reports").Scan(&digest, &by, &stored); err != nil ||
		digest != digestOf(first) || by != "person/anonymous" || len(stored) != 1 || !reflect.DeepEqual(stored[0], want) {
		t.Fatalf("the stored row: %s %s %+v (%v)", digest, by, stored, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE packages SET source_kind = 'repo-pr'"); err != nil {
		t.Fatal(err)
	}
	pass := maps.Clone(result)
	pass["kind"] = "pass"
	second := reportOf("semgrep", result, pass)
	if code, _, _ := do(t, c, http.MethodPut, reports, second); code != 200 || lastBody != `{"tool":"semgrep","category":"","results":1,"dropped":1,"first_dropped":"result 1: kind is pass"}` {
		t.Fatalf("the second report: %d %s", code, lastBody)
	}
	var details string
	err := pool.QueryRow(ctx, "SELECT string_agg(detail::text, '\n' ORDER BY ts) FROM audit_events WHERE action = 'attach report'").Scan(&details)
	wantDetails := `{"tool": "semgrep", "digest": "` + digestOf(first) + `", "package": "` + pkg + `", "category": "", "replaced": null}` + "\n" +
		`{"tool": "semgrep", "digest": "` + digestOf(second) + `", "package": "` + pkg + `", "category": "", "replaced": "` + digestOf(first) + `"}`
	if err != nil || details != wantDetails {
		t.Fatalf("the audit rows:\n got %q\nwant %q (%v)", details, wantDetails, err)
	}
}

// Every refusal answers the contract's code and stores nothing: no row, no audit row.
func TestARefusedReportStoresNothing(t *testing.T) {
	pool, ctx, base, c, pkg, reports := withReports(t)
	result, _ := semgrepGolden(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"payroll"}`)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	ok := reportOf("semgrep", result)
	for name, c2 := range map[string]struct {
		url, body string
		code      int
		want      obj
	}{
		"over 10 MiB":        {reports, ok + strings.Repeat(" ", maxReport-len(ok)+1), 413, obj{"error": "too_large"}},
		"not SARIF":          {reports, "{}", 400, obj{"error": "invalid_report", "message": "the report is not SARIF 2.1.0 JSON"}},
		"an unknown tool":    {reports, reportOf("Snyk Code", result), 400, obj{"error": "unknown_tool", "message": `This report is from "snyk-code", which isn't a tool your admin has added to Canyon. Ask an admin to add it in Guardrails.`}},
		"over 1,000 results": {reports, reportOf("semgrep", slices.Repeat([]any{result}, 1001)...), 400, obj{"error": "too_many_results", "message": "semgrep reported 1,001 results; Canyon takes 1,000 per tool. Narrow the scan to this app's source."}},
		"another project's":  {base + "/api/v1/projects/payroll/packages/" + pkg + "/reports", ok, 404, obj{"error": "not_found"}},
		"an unknown package": {base + "/api/v1/projects/claims-portal/packages/pkg_none/reports", ok, 404, obj{"error": "not_found"}},
		"an unknown project": {base + "/api/v1/projects/nothing/packages/" + pkg + "/reports", ok, 404, obj{"error": "not_found"}},
		"a deleted project":  {base + "/api/v1/projects/gone/packages/" + pkg + "/reports", ok, 404, obj{"error": "not_found"}},
	} {
		code, body, _ := do(t, c, http.MethodPut, c2.url, c2.body)
		matches := code == c2.code
		for k, v := range c2.want {
			matches = matches && body[k] == v
		}
		if !matches {
			t.Errorf("%s: %d %s, want %d %v", name, code, lastBody, c2.code, c2.want)
		}
	}
	req, _ := http.NewRequest(http.MethodPut, reports, strings.NewReader(ok))
	req.Header["Content-Encoding"] = []string{"identity", "gzip"} // compressed, said on a second line
	if code := putStatus(c, req); code != 400 {
		t.Errorf("gzip on a second Content-Encoding line: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleting_since = now()"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPut, reports, ok); code != 409 || body["error"] != "deleting" {
		t.Errorf("a project being deleted: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM package_reports") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'attach report'"); n != 0 {
		t.Fatalf("the refusals stored %d row(s)", n)
	}
}

// Attaching is not an org-wide write: a member and an agent on the app's team and an admin each
// attach, as themselves.
func TestAMemberAnAdminAndAnAgentAttach(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	agent := signedIn(t, ctx, pool, base, "sub-bot", "bot@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-bot'"); err != nil {
		t.Fatal(err)
	}
	do(t, admin, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	_, body := upload(t, admin, base, "claims-portal", tarOf(t, map[string]string{"a.ts": "const a = 1;\n"}), "", "")
	do(t, admin, http.MethodPut, base+toolPath+"?apply=true", `{"state":"advisory"}`)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		SELECT 'claims-portal', 'email', g, 'contributor', 'person', 'sub-dana', '' FROM unnest(ARRAY['carol@meridian.example', 'bot@meridian.example']) g`); err != nil {
		t.Fatal(err)
	}
	reports := base + "/api/v1/projects/claims-portal/packages/" + packageOf(body)["id"].(string) + "/reports"
	for _, who := range []*http.Client{member, admin, agent} {
		if code, _, _ := do(t, who, http.MethodPut, reports, reportOf("semgrep", obj{"ruleId": "r", "message": obj{"text": "m"},
			"locations": []any{obj{"physicalLocation": obj{"artifactLocation": obj{"uri": "a.ts"}, "region": obj{"startLine": 1, "startColumn": 1, "endColumn": 6}}}}})); code != 200 {
			t.Fatalf("attach: %d %s", code, lastBody)
		}
	}
	var got string
	if err := pool.QueryRow(ctx, "SELECT string_agg(actor_kind || '/' || actor_subject, ',' ORDER BY ts) FROM audit_events WHERE action = 'attach report'").Scan(&got); err != nil || got != "person/sub-carol,person/sub-dana,agent/sub-bot" {
		t.Fatalf("the attachers: %v (%v)", got, err)
	}
}

// seedRuns writes n gate runs of the package started ten minutes ago by kind/subject.
func seedRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pkg, kind, subject string, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, started_by_kind, started_by_subject, started_by_display, created_at)
		SELECT 'gr_' || $2 || n, 'claims-portal', $1, 'failed', 'interrupted', $2, $3, '', now() - interval '10 minutes' FROM generate_series(1, $4) n`, pkg, kind, subject, n); err != nil {
		t.Fatal(err)
	}
}

// A PUT counts toward the actor's gate-run limit with their runs, and past it a person or an agent is
// refused as a run start is, before the body is read (the agent's is over 10 MiB), with nothing stored.
func TestAPersonOrAnAgentPastTheLimitIs429(t *testing.T) {
	pool, ctx, _, c, pkg, reports := withReports(t)
	result, _ := semgrepGolden(t)
	seedRuns(t, ctx, pool, pkg, "person", "anonymous", 59)
	if code, _, _ := do(t, c, http.MethodPut, reports, reportOf("semgrep", result)); code != 200 {
		t.Fatalf("the 60th: %d %s", code, lastBody)
	}
	seedRuns(t, ctx, pool, pkg, "agent", "ci", 60)
	for kind, report := range map[string]string{"person": reportOf("semgrep", result), "agent": strings.Repeat(" ", maxReport+1)} {
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1, actor_subject = CASE WHEN $1 = 'agent' THEN 'ci' ELSE 'anonymous' END", kind); err != nil {
			t.Fatal(err)
		}
		code, body, _ := do(t, c, http.MethodPut, reports, report)
		if retry, _ := body["retry_after_s"].(float64); code != 429 || body["error"] != "rate_limited" || retry < 2900 || retry > 3000 ||
			body["message"] != "Limit reached: 60 reviews an hour for you. Try again in 50 minutes." {
			t.Fatalf("a %s past the limit: %d %s", kind, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM package_reports"); n != 1 {
		t.Fatalf("the 429s stored %d row(s)", n-1)
	}
}

// Canyon's own scans attach through the same core as actor Canyon and are never rate limited: two
// attaches under a limit of one both store, as Canyon, and none is an audit row. A scan's report is the tool scanned, whatever
// its driver is named: "Semgrep OSS" from semgrep's scan is semgrep's.
func TestCanyonsOwnAttachIsNotRateLimited(t *testing.T) {
	pool, ctx, base, _, pkg, _ := withReports(t)
	result, _ := semgrepGolden(t)
	boot := testPolicy()
	boot.Limits.GateRunsPerActorPerHour = 1
	core := &app{pool: pool, repos: reposOf(t, base), boot: boot, vault: vaultOf(t, base)}
	for i := range 2 {
		if answer, err := core.attachReport(ctx, "claims-portal", pkg, []byte(reportOf("semgrep", result)), actor{Kind: "canyon", Subject: "scan", Display: "Canyon"}, "semgrep", nil, nil, runs.Evidence{}); err != nil || answer.Results != 1 {
			t.Fatalf("Canyon's attach %d: %+v %v", i+1, answer, err)
		}
	}
	if answer, err := core.attachReport(ctx, "claims-portal", pkg, []byte(reportOf("Semgrep OSS", result)), actor{Kind: "canyon", Subject: "scan", Display: "Canyon"}, "semgrep", nil, nil, runs.Evidence{}); err != nil || answer.Tool != "semgrep" {
		t.Fatalf("semgrep's scan under another driver name: %+v %v", answer, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM package_reports WHERE attached_by_kind = 'canyon' AND attached_by_display = 'Canyon'"); n != 3 {
		t.Fatalf("Canyon's attaches stored %d row(s)", n)
	}
	// and write no audit row: the run's verdict rules on them, its timeline holds them (#1704)
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'attach report' AND actor_kind = 'canyon'"); n != 0 {
		t.Fatalf("Canyon's attaches wrote %d audit row(s)", n)
	}
}

// An uploaded ESLint report names each file of the package its ESL0999 notifications name, as Canyon's
// own scan does, and Canyon's attach stores them beside the files the executor's answer names, one
// path list in path order (contracts/finding.md, In a run).
func TestAnESLintReportsUnparsedFilesAreStored(t *testing.T) {
	pool, ctx, base, c, pkg, reports := withReports(t)
	note := func(uri string) obj {
		return obj{"descriptor": obj{"id": "ESL0999"}, "locations": []any{obj{"physicalLocation": obj{"artifactLocation": obj{"uri": uri}}}}}
	}
	report := string(mustJSON(obj{"version": "2.1.0", "runs": []any{obj{"tool": obj{"driver": obj{"name": "ESLint", "version": "9.39.5"}}, "results": []any{},
		"artifacts":   []any{obj{"location": obj{"uri": "file:///scan/tree/apps/web/src/app/api/claims/route.ts"}}}, // Canyon's names each file it linted
		"invocations": []any{obj{"toolConfigurationNotifications": []any{note("apps/web/src/app/api/claims/route.ts"), note("apps/gone.ts")}}}}}}))
	if code, _, _ := do(t, c, http.MethodPut, reports, report); code != 200 {
		t.Fatalf("the upload: %d %s", code, lastBody)
	}
	core := &app{pool: pool, repos: reposOf(t, base), boot: testPolicy(), vault: vaultOf(t, base)}
	if _, err := core.attachReport(ctx, "claims-portal", pkg, []byte(report), actor{Kind: "canyon", Subject: "scan", Display: "Canyon"}, "eslint", nil, []string{"apps/a.ts"}, runs.Evidence{}); err != nil {
		t.Fatal(err)
	}
	var got string
	err := pool.QueryRow(ctx, "SELECT string_agg(array_to_string(unparsed, ','), ' | ' ORDER BY id) FROM package_reports WHERE tool = 'eslint'").Scan(&got)
	if want := "apps/web/src/app/api/claims/route.ts | apps/a.ts,apps/web/src/app/api/claims/route.ts"; err != nil || got != want {
		t.Fatalf("stored unparsed:\n got %q\nwant %q (%v)", got, want, err)
	}
}

// putStatus sends req and answers its status, 0 when it was not answered: safe off the test's goroutine.
func putStatus(c *http.Client, req *http.Request) int {
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Every PUT spends one of the actor's attempts, refused or not: past the budget a probe that is only
// ever refused (its second result is level none) is refused 429 like any other, and nothing is stored.
func TestEveryPUTCountsRefusedOrNot(t *testing.T) {
	pool, ctx, _, c, _, reports := withReports(t)
	result, _ := semgrepGolden(t)
	none := maps.Clone(result)
	none["level"] = "none"
	for i := range 60 {
		if code, _, _ := do(t, c, http.MethodPut, reports, reportOf("semgrep", result, none)); code != 400 {
			t.Fatalf("probe %d: %d %s", i+1, code, lastBody)
		}
	}
	if code, body, _ := do(t, c, http.MethodPut, reports, reportOf("semgrep", result)); code != 429 ||
		body["message"] != "Limit reached: 60 reviews an hour for you. Try again in 1 minute." || count(t, ctx, pool, "SELECT count(*) FROM package_reports") != 0 {
		t.Fatalf("the 61st PUT: %d %s", code, lastBody)
	}
}

// At most two attaches read a package tree at once: a third waits until one of them is done.
func TestAThirdConcurrentAttachWaits(t *testing.T) {
	_, _, _, c, _, reports := withReports(t)
	result, _ := semgrepGolden(t)
	normalizing <- struct{}{}
	normalizing <- struct{}{}
	t.Cleanup(func() { <-normalizing }) // the other is released below
	done := make(chan int, 1)
	req, _ := http.NewRequest(http.MethodPut, reports, strings.NewReader(reportOf("semgrep", result)))
	go func() { done <- putStatus(c, req) }()
	select {
	case code := <-done:
		t.Fatalf("the third attach answered %d while two read their trees", code)
	case <-time.After(500 * time.Millisecond):
	}
	<-normalizing
	if code := <-done; code != 200 {
		t.Fatalf("the third attach once one was done: %d", code)
	}
}
