package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	approverRequired = `{"error":"approver_required","message":"only an admin or a security responder can approve a blocking finding: ask dana@meridian.example"}`
	authorRefused    = `{"error":"author_cannot_approve","message":"you started this review, so you can't approve its blocking findings: ask another admin or a security responder"}`
	unrecorded       = `{"error":"author_cannot_approve","message":"Canyon didn't record who started this review, so nobody approves its blocking findings: deploy it again"}`
	patchUnrecorded  = `{"error":"author_cannot_approve","message":"Canyon started this review of its fix and didn't record who wrote the code it fixes, so nobody approves its blocking findings: deploy it again"}`
	patchAuthor      = `{"error":"author_cannot_approve","message":"you shipped the code Canyon's fix is for, so you can't approve its blocking findings: ask another admin or a security responder"}`
	approveInBrowser = `{"error":"dashboard_required","message":"approve it in Canyon's dashboard"}`
)

// approverFindings are approverHost's verdict, by index: a secret blocking before preview; a
// high SQL finding and a medium one, advisory before preview; checkov's high Dockerfile check, advisory; two of
// lodash's CVEs, blocking, then two more, advisory. The fingerprints are generated, never literals: a fingerprint
// literal is secret-shaped.
var approverFindings = func() (fps []string) {
	for n := range 8 {
		fps = append(fps, fmt.Sprintf("f_%064x", n+1))
	}
	return fps
}()

// approverHost is an oidc host whose admin is dana and whose responders are rhea, a contributor on
// claims-portal, and rex, on no team; olive owns the app, carl contributes, vic reads. Each of its runs
// ended blocked on approverFindings, started by the person its id names: gr_old by nobody recorded, gr_push
// by a GitHub push.
func approverHost(t *testing.T) (*pgxpool.Pool, context.Context, string) {
	t.Helper()
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "rex@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false))
	fps := approverFindings
	result := func(rule, fp, severity string, blocks bool, problem string) string {
		return fmt.Sprintf(`{"ruleId": %q, "partialFingerprints": {"canyon/v1": %q}, "properties": {"canyon": {"blocks": %t, "severity": %q, "problem": %q}}}`, rule, fp, blocks, severity, problem)
	}
	verdict := `{"state": "blocked", "blocking": 3, "warnings": 3, "findings": {"runs": [{"tool": {"driver": {"name": "canyon-review"}}, "results": [` +
		result("secrets.client-bundle.stripe-key", fps[0], "critical", true, "a key") + ", " +
		result("sql.string-concat", fps[1], "high", false, "a query built from input") + ", " +
		result("sql.string-concat", fps[2], "medium", false, "a second query built from input") +
		`]}, {"tool": {"driver": {"name": "sarif/checkov"}}, "results": [` + result("iac.misconfig.checkov.CKV_DOCKER_2", fps[3], "high", false, "no HEALTHCHECK") +
		`]}, {"tool": {"driver": {"name": "sarif/trivy"}}, "results": [` +
		trivyResult("CVE-2018-16487", "lodash", "4.17.4", "4.17.11", fps[4], true) + ", " + trivyResult("CVE-2019-10744", "lodash", "4.17.4", "4.17.12", fps[5], true) + ", " +
		trivyResult("CVE-2020-8203", "lodash", "4.17.4", "4.17.19", fps[6], false) + ", " + trivyResult("CVE-2021-23337", "lodash", "4.17.4", "4.17.21", fps[7], false) +
		`]}]}}`
	for _, r := range []struct{ id, kind, subject string }{{"gr_carl", "person", "sub-carl"}, {"gr_rhea", "person", "sub-rhea"}, {"gr_dana", "person", "sub-dana"}, {"gr_old", "", ""}, {"gr_push", "agent", "github:carl"}} {
		if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, started_by_kind, started_by_subject, started_by_display)
			SELECT $1, 'claims-portal', package_id, 'blocked', now(), NULLIF($2, ''), NULLIF($3, ''), NULLIF($3, '') FROM runs WHERE id = 'gr_seed')
			INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', $1, 'claims-portal', 'verdict', 'verdict', $4)`, r.id, r.kind, r.subject, verdict); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', $1, 'contributor', 'person', 'sub-olive', '')`, responderEmail); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base
}

// approveBody is an approval's terms, a day long, with the fingerprints of the dependency route when any are named.
func approveBody(fps ...string) string {
	terms := `"reason":"Reviewed by hand","expires_at":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `"`
	if len(fps) == 0 {
		return "{" + terms + "}"
	}
	return `{"fingerprints":["` + strings.Join(fps, `","`) + `"],` + terms + "}"
}

// A blocking finding is approved only by an admin or a responder, never by the author of the run being
// approved, and only from a browser (design/PERMISSIONS.md; #2204): the owner and the contributor who
// started it are approver_required, read-only and no access are team_required, an admin and a responder —
// on the team or on none — are dashboard_required from the CLI and approve from a browser, and neither
// approves a run they started, nor a patch run Canyon started of code they shipped — its author the rebased run's
// — and nobody a patch run whose rebased run recorded no author. Each refusal writes one denied row and approves nothing; the
// approval's row names the approver and the author. An advisory finding is approved at contributor, as before.
func TestABlockingFindingIsApprovedByAnAdminOrAResponderNeverItsAuthor(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	one := func(run, fp string) string { return base + "/api/v1/gate-runs/" + run + "/findings/" + fp + "/approve" }
	browser := func(sub, email string) *http.Client { return signedIn(t, ctx, pool, base, sub, email, false) }
	olive, carl, rhea, dana := browser("sub-olive", "olive@meridian.example"), browser("sub-carl", "carl@meridian.example"), browser("sub-rhea", responderEmail), browser("sub-dana", "dana@meridian.example")
	for _, c := range []struct {
		who  string
		c    *http.Client
		want string
	}{
		{"the owner", olive, approverRequired},
		{"the contributor who started it", carl, approverRequired},
		{"a read-only member", browser("sub-vic", "vic@meridian.example"), "team_required"},
		{"a member with no access", browser("sub-nora", "nora@meridian.example"), "team_required"},
		{"a responder on no team, from the CLI", cliIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example"), approveInBrowser},
		{"the responder from the CLI", cliIn(t, ctx, pool, base, "sub-rhea", responderEmail), approveInBrowser},
		{"an admin from the CLI", cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), approveInBrowser},
	} {
		code, body, _ := do(t, c.c, http.MethodPost, one("gr_carl", fps[0]), approveBody())
		if code != 403 || lastBody != c.want && body["error"] != c.want {
			t.Errorf("%s approves carl's blocking finding: %d %s", c.who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals"); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != 7 {
		t.Fatalf("the refusals wrote %d approval(s), or not one denied row each", n)
	}
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display)
		SELECT 'gr_patch', project_id, package_id, 'blocked', now(), 'patch', 'canyon', 'patch', 'Canyon' FROM runs WHERE id = 'gr_carl')
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_patch', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = 'gr_carl' AND kind = 'verdict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display, author_kind, author_subject, author_display)
		SELECT 'gr_patch_dana', project_id, package_id, 'blocked', now(), 'patch', 'canyon', 'patch', 'Canyon', 'person', 'sub-dana', 'Dana' FROM runs WHERE id = 'gr_carl')
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_patch_dana', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = 'gr_carl' AND kind = 'verdict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display, author_kind, author_subject, author_display)
		SELECT 'gr_patch_carl', project_id, package_id, 'blocked', now(), 'patch', 'canyon', 'patch', 'Canyon', 'person', 'sub-carl', 'Carl' FROM runs WHERE id = 'gr_carl')
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_patch_carl', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = 'gr_carl' AND kind = 'verdict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH l AS (INSERT INTO github_links (github_id, subject, login) VALUES (777, 'sub-dana', 'dana-gh')),
		r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display, author_kind, author_subject, author_display, author_github)
		SELECT 'gr_patch_push', project_id, package_id, 'blocked', now(), 'patch', 'canyon', 'patch', 'Canyon', 'person', 'github:dana-gh', 'dana-gh', 777 FROM runs WHERE id = 'gr_carl')
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_patch_push', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = 'gr_carl' AND kind = 'verdict'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who, run string
		c        *http.Client
		want     string
	}{
		{"the responder, of a run they started", "gr_rhea", rhea, authorRefused},
		{"the admin, of a run they started", "gr_dana", dana, authorRefused},
		{"the admin, of a run nobody recorded starting", "gr_old", dana, unrecorded},
		{"the admin, of a patch run, whose code's author is not recorded yet", "gr_patch", dana, patchUnrecorded},
		{"the responder, of a patch run", "gr_patch", rhea, patchUnrecorded},
		{"the admin, of a patch run of code she shipped", "gr_patch_dana", dana, patchAuthor},
		{"the admin, of a patch run of a push by the GitHub account she linked", "gr_patch_push", dana, patchAuthor},
	} {
		if code, _, _ := do(t, c.c, http.MethodPost, one(c.run, fps[0]), approveBody()); code != 403 || lastBody != c.want {
			t.Errorf("%s approves: %d %s", c.who, code, lastBody)
		}
	}
	for _, c := range []struct {
		who, run string
		c        *http.Client
	}{{"the responder", "gr_carl", rhea}, {"the admin", "gr_carl", dana}, {"the admin, of the responder's run", "gr_rhea", dana}, {"the responder, of a push", "gr_push", rhea},
		{"the responder, of a patch run of code dana shipped", "gr_patch_dana", rhea},
		{"the admin, of a patch run of code carl shipped", "gr_patch_carl", dana},
		{"a responder on no team", "gr_carl", browser("sub-rex", "rex@meridian.example")}} {
		if code, _, _ := do(t, c.c, http.MethodPost, one(c.run, fps[0]), approveBody()); code != 200 {
			t.Errorf("%s approves %s: %d %s", c.who, c.run, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve' AND run_id = 'gr_carl'
		AND actor_subject = 'sub-rhea' AND detail->'author' = '{"kind":"person","subject":"sub-carl","display":"sub-carl"}'`); n != 1 {
		t.Fatalf("the responder's approval names the author: %d row(s)", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE run_id = 'gr_push' AND action LIKE '%approve' AND detail->'author'->>'subject' = 'github:carl'`); n != 1 {
		t.Fatalf("an approval of a push names its actor: %d row(s)", n)
	}
	for _, c := range []struct {
		who string
		c   *http.Client
	}{{"the contributor who started it", carl}, {"the owner", olive}} { // advisory: unchanged
		if code, _, _ := do(t, c.c, http.MethodPost, one("gr_carl", fps[2]), approveBody()); code != 200 {
			t.Errorf("%s approves an advisory finding: %d %s", c.who, code, lastBody)
		}
	}
	many := base + "/api/v1/gate-runs/gr_carl/findings/approve"
	for _, c := range []struct {
		who  string
		c    *http.Client
		code int
		want string
	}{
		{"the owner", olive, 403, approverRequired},
		{"the responder from the CLI", cliIn(t, ctx, pool, base, "sub-rhea", responderEmail), 403, approveInBrowser},
		{"the responder, of carl's run", rhea, 200, ""},
	} {
		if code, _, _ := do(t, c.c, http.MethodPost, many, approveBody(fps[4], fps[5])); code != c.code || c.want != "" && lastBody != c.want {
			t.Errorf("%s approves lodash: %d %s", c.who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/gate-runs/{run}/findings/approve' AND actor_subject = 'sub-rhea'
		AND detail->'author'->>'subject' = 'sub-carl'`); n != 1 {
		t.Fatalf("lodash's approval names the author: %d row(s)", n)
	}
}

// The run read says whether its reader may approve its blocking findings, by who they are (routes.md, the run
// read): an admin or a responder who did not start the run; never its author, nor the team below them.
func TestTheRunReadSaysWhoApproves(t *testing.T) {
	pool, ctx, base := approverHost(t)
	browser := func(sub, email string) *http.Client { return signedIn(t, ctx, pool, base, sub, email, false) }
	rhea, dana := browser("sub-rhea", responderEmail), browser("sub-dana", "dana@meridian.example")
	for _, c := range []struct {
		who, run string
		c        *http.Client
		want     any
	}{
		{"the responder", "gr_carl", rhea, true},
		{"the admin", "gr_carl", dana, true},
		{"the admin, from the CLI", "gr_carl", cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), true}, // who they are; the session is the route's
		{"the contributor who started it", "gr_carl", browser("sub-carl", "carl@meridian.example"), nil},
		{"the owner", "gr_carl", browser("sub-olive", "olive@meridian.example"), nil},
		{"the responder, of a run they started", "gr_rhea", rhea, nil},
		{"the admin, of a run nobody recorded starting", "gr_old", dana, nil},
	} {
		if code, body, _ := do(t, c.c, http.MethodGet, base+"/api/v1/gate-runs/"+c.run, ""); code != 200 || runOf(body)["approvable"] != c.want {
			t.Errorf("%s reads %s: %d approvable %v", c.who, c.run, code, runOf(body)["approvable"])
		}
	}
}

// A rollback review's blocking finding is approved as any run's (#2198): the run read lets a responder
// approve it, who does from a browser, and its author, the contributor who asked for it, cannot.
func TestARollbackReviewsBlockingFindingIsApprovedAsAnyRuns(t *testing.T) {
	pool, ctx, base := approverHost(t)
	if _, err := pool.Exec(ctx, "UPDATE runs SET rollback_to = $1, rollback_reason = 'INC-2231' WHERE id = 'gr_carl'", "sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	rhea, carl := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, body, _ := do(t, rhea, http.MethodGet, base+"/api/v1/gate-runs/gr_carl", ""); code != 200 || runOf(body)["approvable"] != true {
		t.Fatalf("the responder reads the rollback review: %d %s", code, lastBody)
	}
	one := base + "/api/v1/gate-runs/gr_carl/findings/" + approverFindings[0] + "/approve"
	if code, _, _ := do(t, carl, http.MethodPost, one, approveBody()); code != 403 || lastBody != approverRequired {
		t.Fatalf("its author approves it: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, rhea, http.MethodPost, one, approveBody()); code != 200 || count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE approves_blocking AND approved_by_subject = 'sub-rhea'") != 1 {
		t.Fatalf("the responder approves the rollback review's blocking finding: %d %s", code, lastBody)
	}
}

// A finding the app's rules block before production is blocking to approve, and the run read marks it, as
// the receipt counts it (#2206): a high finding under a rule placed blocking there, never a medium one, nor
// a Dockerfile check's under a rule placed so too. Before it is placed, the owner approves it as advisory.
func TestAFindingBlockingBeforeProductionIsApprovedAsBlocking(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	olive, dana := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	one := func(fp string) string { return base + "/api/v1/gate-runs/gr_carl/findings/" + fp + "/approve" }
	production := func() []string {
		t.Helper()
		_, body, _ := do(t, olive, http.MethodGet, base+"/api/v1/gate-runs/gr_carl", "")
		var marked []string
		for _, f := range runOf(body)["findings"].([]any) {
			if f := f.(map[string]any); f["blocks_production"] == true {
				marked = append(marked, f["partialFingerprints"].(map[string]any)["canyon/v1"].(string))
			}
		}
		return marked
	}
	if marked := production(); len(marked) != 0 {
		t.Fatalf("nothing placed before production, the read marks %v", marked)
	}
	if code, _, _ := do(t, olive, http.MethodPost, one(fps[1]), approveBody()); code != 200 {
		t.Fatalf("the owner approves an advisory finding: %d %s", code, lastBody)
	}
	for _, rule := range []string{"sql.string-concat", "iac.misconfig"} {
		if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+rule+"?apply=true", `{"production":"blocking"}`); code != 200 {
			t.Fatalf("placing %s blocking before production: %d %s", rule, code, lastBody)
		}
	}
	if marked := production(); len(marked) != 1 || marked[0] != fps[1] {
		t.Fatalf("the read marks %v, want the high query alone", marked)
	}
	if code, _, _ := do(t, olive, http.MethodPost, one(fps[1]), approveBody()); code != 403 || lastBody != approverRequired {
		t.Fatalf("the owner approves a finding blocking before production: %d %s", code, lastBody)
	}
	for _, fp := range []string{fps[2], fps[3]} { // a medium finding, a Dockerfile check: advisory still
		if code, _, _ := do(t, olive, http.MethodPost, one(fp), approveBody()); code != 200 {
			t.Errorf("the owner approves an advisory finding under a placed rule: %d %s", code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPost, one(fps[1]), approveBody()); code != 200 {
		t.Fatalf("the admin approves it: %d %s", code, lastBody)
	}
}

// An approval covers a blocking finding only when it was filed under the approver rule, and every read says so
// alike (contracts/routes.md, the approve rows; #2204): one filed before the rule, or by the owner while the
// finding was advisory, is on no blocking finding of the run read, suppresses nothing in its SARIF, and leaves
// the finding open where the receipt counts it, before preview and before production; one an admin or a
// responder files covers it in all of them.
func TestAnApprovalOutsideTheApproverRuleCoversNoBlockingFinding(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	olive, dana, rhea := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	one := func(fp string) string { return base + "/api/v1/gate-runs/gr_carl/findings/" + fp + "/approve" }
	if code, _, _ := do(t, olive, http.MethodPost, one(fps[1]), approveBody()); code != 200 { // advisory: the owner approves it
		t.Fatalf("the owner approves an advisory finding: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display)
		VALUES ('claims-portal', 'secrets.client-bundle.stripe-key', $1, 'filed before the approver rule', now() + interval '1 day', 'person', 'sub-dana', 'sub-dana'),
		('claims-portal', 'deps.known-vuln.trivy.CVE-2018-16487', $2, 'filed before the approver rule', now() + interval '1 day', 'person', 'sub-dana', 'sub-dana'),
		('claims-portal', 'deps.known-vuln.trivy.CVE-2019-10744', $3, 'filed before the approver rule', now() + interval '1 day', 'person', 'sub-dana', 'sub-dana')`, fps[0], fps[4], fps[5]); err != nil {
		t.Fatal(err)
	}
	// approvedRun is the runs list's word for a run: every blocking finding covered now (#1955).
	approvedRun := func(id string) bool {
		t.Helper()
		_, list, _ := do(t, olive, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
		for _, r := range list["runs"].([]any) {
			if r := r.(map[string]any); r["id"] == id {
				return r["approved"] == true
			}
		}
		t.Fatalf("no row for %s: %s", id, lastBody)
		return false
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/sql.string-concat?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing sql.string-concat blocking before production: %d %s", code, lastBody)
	}
	// open is what each read leaves unapproved: the run read's findings without an approval, the SARIF's without
	// a suppression, and the receipt's count before preview and under the placed rule.
	open := func() (read, sarif []string, preview, production int) {
		t.Helper()
		_, body, _ := do(t, olive, http.MethodGet, base+"/api/v1/gate-runs/gr_carl", "")
		for _, f := range runOf(body)["findings"].([]any) {
			if f := f.(map[string]any); f["approval"] == nil {
				read = append(read, f["partialFingerprints"].(map[string]any)["canyon/v1"].(string))
			} else if _, leaked := f["approval"].(map[string]any)["approves_blocking"]; leaked {
				t.Fatalf("the run read answers whether an approval was filed under the rule: %s", lastBody)
			}
		}
		_, log, _ := do(t, olive, http.MethodGet, base+"/api/v1/gate-runs/gr_carl/sarif", "")
		for _, r := range log["runs"].([]any) {
			for _, res := range r.(map[string]any)["results"].([]any) {
				if res := res.(map[string]any); res["suppressions"] == nil {
					sarif = append(sarif, res["partialFingerprints"].(map[string]any)["canyon/v1"].(string))
				}
			}
		}
		facts, unapproved, err := receiptFacts(ctx, pool, "claims-portal", "gr_carl", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var log2 finding.Log
		if err := json.Unmarshal(facts.findings, &log2); err != nil {
			t.Fatal(err)
		}
		placed, _ := blockingUnder(log2, "sql.string-concat")
		if _, production, err = runs.Approvals(ctx, pool, "claims-portal", "gr_carl", placed); err != nil {
			t.Fatal(err)
		}
		return read, sarif, unapproved, production
	}
	read, sarif, preview, production := open()
	if strings.Join(read, " ") != strings.Join(fps, " ") || strings.Join(sarif, " ") != strings.Join(fps, " ") || preview != 3 || production != 1 || approvedRun("gr_carl") { // the key, and each of lodash's two CVEs
		t.Fatalf("outside the rule: the read leaves %d open, the SARIF %d, the receipt %d before preview and %d before production", len(read), len(sarif), preview, production)
	}
	for _, c := range []struct {
		c  *http.Client
		fp string
	}{{dana, fps[1]}, {rhea, fps[0]}} {
		if code, _, _ := do(t, c.c, http.MethodPost, one(c.fp), approveBody()); code != 200 {
			t.Fatalf("approving under the rule: %d %s", code, lastBody)
		}
	}
	if code, _, _ := do(t, rhea, http.MethodPost, base+"/api/v1/gate-runs/gr_carl/findings/approve", approveBody(fps[4], fps[5])); code != 200 {
		t.Fatalf("approving lodash under the rule: %d %s", code, lastBody)
	}
	read, sarif, preview, production = open()
	if want := []string{fps[2], fps[3], fps[6], fps[7]}; strings.Join(read, " ") != strings.Join(want, " ") || strings.Join(sarif, " ") != strings.Join(want, " ") || preview != 0 || production != 0 || !approvedRun("gr_carl") {
		t.Fatalf("under the rule: the read leaves %v open, the SARIF %v, the receipt %d before preview and %d before production", read, sarif, preview, production)
	}
}

// A responder approves on any app, as a security team does (design/PERMISSIONS.md, Security responders; #2204):
// with no grant they read what approving needs — the runs list, a run's findings and SARIF, the status read
// and its receipt — and approve a blocking finding from a browser; nothing else of the app is theirs, a secret,
// a log, a deploy or a promotion; and never a run they started.
func TestAResponderOnNoTeamReadsWhatApprovingNeedsAndApproves(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	rex := signedIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example", false)
	for _, path := range []string{"/api/v1/projects/claims-portal/gate-runs", "/api/v1/gate-runs/gr_carl", "/api/v1/gate-runs/gr_carl/sarif", "/api/v1/projects/claims-portal/status"} {
		if code, _, _ := do(t, rex, http.MethodGet, base+path, ""); code != 200 {
			t.Errorf("the responder reads %s: %d %s", path, code, lastBody)
		}
	}
	if _, body, _ := do(t, rex, http.MethodGet, base+"/api/v1/gate-runs/gr_carl", ""); runOf(body)["approvable"] != true || len(runOf(body)["findings"].([]any)) != len(fps) {
		t.Fatalf("the responder's run read: %s", lastBody)
	}
	// The status read, trimmed to what approving needs: the receipt, its runs, production as a viewer reads it.
	for _, env := range []string{"preview", "prod"} {
		if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display)
			SELECT project_id, $1, id, 'canyon/claims-portal@'||$2, 'http://claims-portal.'||$1||'.localhost:7777', 'gr_seed', 'person', 'sub-carl', 'sub-carl' FROM packages WHERE project_id = 'claims-portal'`, env, executortest.Digest); err != nil {
			t.Fatal(err)
		}
		fake.Start("claims-portal", env)
	}
	_, st, _ := do(t, rex, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	keys := slices.Sorted(maps.Keys(st))
	envs, _ := st["envs"].(map[string]any)
	if strings.Join(keys, " ") != "envs last_run needs project promote_run receipt" || len(envs) != 1 || envs["prod"] == nil ||
		strings.Contains(lastBody, "said") || strings.Contains(lastBody, "answered") || strings.Contains(lastBody, "deployed_by") || strings.Contains(lastBody, "preview.localhost") {
		t.Fatalf("the responder's status read: %s", lastBody)
	}
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/projects/claims-portal/secrets", ""},
		{http.MethodGet, "/api/v1/projects/claims-portal/logs?env=prod", ""},
		{http.MethodGet, "/api/v1/projects/claims-portal/packages", ""},
		{http.MethodGet, "/api/v1/gate-runs/gr_carl/timeline", ""},
		{http.MethodPost, "/api/v1/projects/claims-portal/deploys", "{}"},
		{http.MethodPost, "/api/v1/projects/claims-portal/promotions", `{"gate_run":"gr_carl"}`},
		{http.MethodPost, "/api/v1/gate-runs/gr_carl/cancel", ""},
	} {
		if code, body, _ := do(t, rex, r.method, base+r.path, r.body); code != 403 || body["error"] != "team_required" {
			t.Errorf("the responder, %s %s: %d %s, want team_required", r.method, r.path, code, lastBody)
		}
	}
	if code, _, _ := do(t, rex, http.MethodPost, base+"/api/v1/gate-runs/gr_carl/findings/"+fps[0]+"/approve", approveBody()); code != 200 {
		t.Fatalf("the responder approves carl's blocking finding: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET started_by_subject = 'sub-rex', started_by_display = 'sub-rex' WHERE id = 'gr_dana'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, rex, http.MethodPost, base+"/api/v1/gate-runs/gr_dana/findings/"+fps[0]+"/approve", approveBody()); code != 403 || lastBody != authorRefused {
		t.Fatalf("the responder approves a run they started: %d %s", code, lastBody)
	}
	// The approval rex filed of carl's run carries the fingerprint rex's own run has too: it covers that
	// finding nowhere on rex's run (B1, the read-time half of "never the author").
	_, read, _ := do(t, rex, http.MethodGet, base+"/api/v1/gate-runs/gr_dana", "")
	if f := runOf(read)["findings"].([]any)[0].(map[string]any); f["approval"] != nil || runOf(read)["approvable"] != nil {
		t.Fatalf("rex's own run reads rex's approval: %s", lastBody)
	}
	var findings json.RawMessage
	if err := pool.QueryRow(ctx, "SELECT data->'findings' FROM timeline_rows WHERE run_id = 'gr_dana' AND kind = 'verdict'").Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if _, open, err := runs.Approvals(ctx, pool, "claims-portal", "gr_dana", findings); err != nil || open != 3 {
		t.Fatalf("rex's own run: %d blocking entries open (%v), want the key's and lodash's two", open, err)
	}
	_, list, _ := do(t, rex, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	for _, r := range list["runs"].([]any) {
		if r := r.(map[string]any); r["id"] == "gr_dana" && r["approved"] != nil {
			t.Fatalf("the runs list reads rex's own run approved: %s", lastBody)
		}
	}
}

// One dependency's findings approved together follow the single approve's rules when any of them blocks
// (#2204): the owner is approver_required for a blocking and an advisory finding together, and approves
// advisory ones alone; an admin and a responder are author_cannot_approve on a run they started.
func TestOneDependencysFindingsFollowTheApproverRule(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	many := func(run string) string { return base + "/api/v1/gate-runs/" + run + "/findings/approve" }
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	for _, c := range []struct {
		who, run string
		c        *http.Client
		fps      []string
		want     string
	}{
		{"the owner, a blocking and an advisory finding", "gr_carl", olive, []string{fps[5], fps[6]}, approverRequired},
		{"the responder, of a run they started", "gr_rhea", signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), []string{fps[4], fps[5]}, authorRefused},
		{"the admin, of a run they started", "gr_dana", signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), []string{fps[4], fps[5]}, authorRefused},
	} {
		if code, _, _ := do(t, c.c, http.MethodPost, many(c.run), approveBody(c.fps...)); code != 403 || lastBody != c.want {
			t.Errorf("%s: %d %s", c.who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM approvals"); n != 0 {
		t.Fatalf("the refusals wrote %d approval(s)", n)
	}
	if code, _, _ := do(t, olive, http.MethodPost, many("gr_carl"), approveBody(fps[6], fps[7])); code != 200 || count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE NOT approves_blocking") != 2 {
		t.Fatalf("the owner approves two advisory findings together: %d %s", code, lastBody)
	}
}

// An approval filed outside the approver rule never replaces one filed under it (#2204): the owner approving
// an advisory finding an admin approved is answered the admin's approval, which stands, and records nothing.
// A responder with no grant approves even an advisory finding from a browser alone; one with a grant, as the
// grant lets them.
func TestAnApprovalUnderTheRuleStandsAndAGrantlessResponderNeedsABrowser(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	one := base + "/api/v1/gate-runs/gr_carl/findings/" + fps[6] + "/approve"
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, one, approveBody()); code != 200 {
		t.Fatalf("the admin approves: %d %s", code, lastBody)
	}
	trail := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%approve'")
	code, body, _ := do(t, signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false), http.MethodPost, one, approveBody())
	by, _ := body["approval"].(map[string]any)["approved_by"].(map[string]any)
	if code != 200 || by["subject"] != "sub-dana" || count(t, ctx, pool, "SELECT count(*) FROM approvals WHERE approves_blocking AND approved_by_subject = 'sub-dana'") != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%approve'") != trail {
		t.Fatalf("the owner's approval over the admin's: %d %s", code, lastBody)
	}
	advisory := base + "/api/v1/gate-runs/gr_carl/findings/" + fps[2] + "/approve"
	if code, _, _ := do(t, cliIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example"), http.MethodPost, advisory, approveBody()); code != 403 || lastBody != approveInBrowser {
		t.Fatalf("a responder with no grant approves an advisory finding from the CLI: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, cliIn(t, ctx, pool, base, "sub-rhea", responderEmail), http.MethodPost, advisory, approveBody()); code != 200 {
		t.Fatalf("a responder on the team approves an advisory finding from the CLI: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example", false), http.MethodGet, base+"/api/v1/gate-runs/gr_carl", ""); code != 404 {
		t.Fatalf("a responder with no grant reads a deleted app's run: %d %s", code, lastBody)
	}
}

// With sign-in on, an approval the anonymous person filed with it off covers nothing, voided as the server
// starts and counted with every approval filed before the approver rule; with it off, nothing is voided.
func TestTheServerVoidsAnonymousApprovalsOnceSignInIsOn(t *testing.T) {
	pool, ctx := migrated(t)
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id) VALUES ('claims-portal');
		INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		VALUES ('claims-portal', 'secrets.client-bundle', 'f_1', 'demo', now() + interval '1 day', 'person', 'anonymous', '', true),
		('claims-portal', 'secrets.client-bundle', 'f_2', 'old', now() + interval '1 day', 'person', 'sub-dana', 'Dana', false),
		('claims-portal', 'secrets.client-bundle', 'f_3', 'by rule', now() + interval '1 day', 'person', 'sub-dana', 'Dana', true)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		signIn bool
		want   int
	}{{false, 1}, {true, 2}} {
		if n, err := voidApprovals(ctx, pool, c.signIn); err != nil || n != c.want {
			t.Errorf("sign-in %v: %d voided (%v), want %d", c.signIn, n, err, c.want)
		}
	}
}

// With sign-in off every caller is the anonymous admin, who starts the run and approves it: nothing is refused,
// and the approval covers the finding wherever it is read (contracts/routes.md, the approve rows).
func TestWithSignInOffTheRunsAuthorApprovesIt(t *testing.T) {
	_, _, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
	for _, f := range runOf(body)["findings"].([]any) {
		f := f.(map[string]any)
		if f["properties"].(map[string]any)["canyon"].(map[string]any)["blocks"] != true {
			continue
		}
		if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/"+f["partialFingerprints"].(map[string]any)["canyon/v1"].(string)+"/approve", approveBody()); code != 200 {
			t.Fatalf("the anonymous author approves: %d %s", code, lastBody)
		}
	}
	_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	if r := list["runs"].([]any)[0].(map[string]any); r["id"] != run || r["approved"] != true || r["started_by"].(map[string]any)["subject"] != "anonymous" {
		t.Fatalf("the run its author approved with sign-in off: %s", lastBody)
	}
}

// The promotion's deploy step clears the review its receipt accepted, as the receipt judges it, read now
// (contracts/routes.md, the approve rows; #2204): every finding blocking before preview, and one only a rule
// placed before production blocks, each covered by an unexpired approval — one expiring while the promotion
// waits clears nothing; and a review with no verdict, or none at all, clears nothing.
func TestTheReceiptsReviewClearsAPromotionOnlyWhileItsApprovalsStand(t *testing.T) {
	pipe.Gate = func(ctx context.Context, dir, tree string, snap gatepolicy.Snapshot, report gate.Report) (finding.Log, error) { // the golden log, and a high CVE advisory before preview
		l, err := runstest.Golden(ctx, dir, tree, snap, report)
		if err != nil {
			return l, err
		}
		cve := l.Runs[2].Results[0] // gitleaks' key, as Canyon's own scanners' dependency finding
		advisory := false
		cve.RuleID, cve.PartialFingerprints.CanyonV1 = "deps.known-vuln", "f_"+strings.Repeat("c", 64)
		cve.Properties.Canyon.Blocks, cve.Properties.Canyon.Severity, cve.Properties.Canyon.PolicyName = &advisory, "high", "No known-vulnerable dependencies"
		cve.Properties.Canyon.SecretName, cve.Level = nil, "error"
		l.Runs[0].Results = append(l.Runs[0].Results, cve)
		l.Runs[0].Tool.Driver.Rules = append(l.Runs[0].Tool.Driver.Rules, finding.Rule{ID: "deps.known-vuln", Name: "No known-vulnerable dependencies"})
		return l, nil
	}
	t.Cleanup(func() { pipe.Gate = runstest.Golden })
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	run := blockedRun(t, c, base)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/deps.known-vuln?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing deps.known-vuln blocking before production: %d %s", code, lastBody)
	}
	// placed before production, deps.known-vuln reads the built app too (design/PATCHING.md §3.6): preview runs an
	// image whose built app holds nothing its review didn't see, its own layers matched clean
	g := loadPlatformGolden(t)
	for _, d := range []string{"sha256:" + strings.Repeat("4", 64), "sha256:" + strings.Repeat("6", 64)} {
		m := g.Matches[0]
		m.DiffID, m.Answer.Report, m.Answer.Unscanned = d, json.RawMessage(`{"SchemaVersion":2,"Results":[]}`), nil
		g.Matches = append(g.Matches, m)
	}
	seedPlatform(t, ctx, pool, g, "claims-portal", executortest.Digest)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run)
		SELECT project_id, 'preview', package_id, 'canyon/claims-portal@'||$1, 'http://claims-portal.preview.localhost:7777', id FROM runs WHERE id = $2`, executortest.Digest, run); err != nil {
		t.Fatal(err)
	}
	clears := func(run string) bool {
		t.Helper()
		ok, err := pipe.Clears(ctx, "claims-portal", run)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if clears(run) {
		t.Fatal("a review whose blocking findings nobody approved clears a promotion")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'reviewed', now() + interval '1 hour', 'person', 'anonymous', '', true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true || @.ruleId starts with "deps.")') f
		WHERE t.run_id = $1 AND t.kind = 'verdict'`, run); err != nil {
		t.Fatal(err)
	}
	if !clears(run) {
		t.Fatal("a review every blocking finding of which is approved, before preview and before production, clears nothing")
	}
	// Within runs.ShipMargin of expiry an approval clears no deploy, though the page still shows it approved.
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() + interval '5 minutes' WHERE rule LIKE 'deps.%'"); err != nil {
		t.Fatal(err)
	}
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
	shown := 0
	for _, f := range runOf(read)["findings"].([]any) {
		if f := f.(map[string]any); f["ruleId"] == "deps.known-vuln" && f["approval"] != nil {
			shown++
		}
	}
	if clears(run) || shown != 1 {
		t.Fatalf("an approval 5 minutes from expiry: clears %v, shown on %d finding(s), want no and 1", clears(run), shown)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 second' WHERE rule LIKE 'deps.%'"); err != nil { // expired while the promotion waited
		t.Fatal(err)
	}
	if clears(run) {
		t.Fatal("a review whose production-only blocker's approval expired clears a promotion")
	}
	if clears("gr_noverdict000") || clears("") {
		t.Fatal("no review, or one with no verdict, clears a promotion")
	}
}

// A promotion records the review its receipt accepted it under, never preview's own deploy review: a newer
// review of preview's version, blocked and approved, is the one its deploy step clears, so that review's
// approval expiring clears nothing, whatever the older, clean one says (#2204).
func TestAPromotionClearsTheReviewItsReceiptAccepted(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t) // preview runs a version its review, A, passed clean
	review := blockedRun(t, c, base)           // B: the golden gate, of the same version
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'reviewed', now() + interval '1 hour', 'person', 'anonymous', '', true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.run_id = $1 AND t.kind = 'verdict'`, review); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+review+`"}`)
	promoted, _ := runOf(body)["id"].(string)
	var recorded string
	if code != 202 || pool.QueryRow(ctx, "SELECT COALESCE(receipt_run, '') FROM runs WHERE id = $1", promoted).Scan(&recorded) != nil || recorded != review {
		t.Fatalf("the promotion under B records %q (%d %s), want B %s, never A %s", recorded, code, lastBody, review, deploy)
	}
	waitVerdict(t, c, base, promoted)
	if _, err := pool.Exec(ctx, "UPDATE approvals SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]bool{deploy: true, review: false} {
		if ok, err := pipe.Clears(ctx, "claims-portal", run); err != nil || ok != want {
			t.Errorf("review %s clears %v (%v), want %v", run, ok, err, want)
		}
	}
}
