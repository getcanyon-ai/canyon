package runs

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gatedRun is a run of project carl started, whose findings an approval covers: none of carl's own (#2204).
func gatedRun(t *testing.T, ctx context.Context, p *pgxpool.Pool) string {
	t.Helper()
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display)
		VALUES ('gr_gated', $1, $2, 'blocked', 'person', 'carl', 'Carl') ON CONFLICT (id) DO NOTHING`, project, pkg); err != nil {
		t.Fatal(err)
	}
	return "gr_gated"
}

// Only a person's approval covers a blocking finding: a row Canyon itself once wrote (approved_by_kind
// canyon) covers nothing on the deploy's verdict step and the promote receipt.
func TestAnApprovalByCanyonCoversNothing(t *testing.T) {
	p, ctx := seeded(t)
	raw, err := os.ReadFile("../contracts/testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	var log finding.Log
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	var res *finding.Result
	for _, run := range log.Runs {
		for i := range run.Results {
			if b := run.Results[i].Properties.Canyon.Blocks; b != nil && *b {
				res = &run.Results[i]
			}
		}
	}
	if res == nil {
		t.Fatal("the golden has no blocking finding")
	}
	approve := func(kind, subject, display string) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
			VALUES ($1, $2, $3, 'a test key', now() + interval '30 days', $4, $5, $6, true)
			ON CONFLICT (project_id, rule, fingerprint) DO UPDATE SET approved_by_kind = EXCLUDED.approved_by_kind, approved_by_subject = EXCLUDED.approved_by_subject,
				approved_by_display = EXCLUDED.approved_by_display`,
			project, res.RuleID, res.PartialFingerprints.CanyonV1, kind, subject, display); err != nil {
			t.Fatal(err)
		}
	}
	approve("canyon", "ai", "Canyon's AI")
	if approved, unapproved, err := Approvals(ctx, p, project, gatedRun(t, ctx, p), raw); err != nil || len(approved) != 0 || unapproved != 1 {
		t.Fatalf("Canyon's approval covered the finding: %+v, %d uncovered (%v)", approved, unapproved, err)
	}
	approve("person", "dana@meridian.example", "Dana Whitfield")
	if approved, unapproved, err := Approvals(ctx, p, project, gatedRun(t, ctx, p), raw); err != nil || len(approved) != 1 || unapproved != 0 {
		t.Fatalf("a person's approval covers it: %+v, %d uncovered (%v)", approved, unapproved, err)
	}
}

// Only an approval filed under the approver rule covers a blocking finding, and never one by the run's own
// author (contracts/routes.md, the approve rows; #2204): one filed before the rule covers it nowhere this
// predicate is read — the deploy's verdict step, the promote receipt in both gates, the runs list and the
// GitHub check — and one carl filed, under the rule, on another run with the same fingerprint, never covers
// carl's. A result that does not say whether it blocks reads as blocking.
func TestAnApprovalOutsideTheApproverRuleCoversNoBlockingFinding(t *testing.T) {
	p, ctx := seeded(t)
	raw := []byte(`{"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "sarif/gitleaks"}}, "results": [
	  {"ruleId": "secrets.client-bundle.gitleaks.stripe-access-token", "partialFingerprints": {"canyon/v1": "bbbbbbbbbbbbbbbb"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "pay.js"}, "region": {"startLine": 3}}}], "properties": {"canyon": {}}}]}]}`)
	if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display)
		VALUES ($1, 'secrets.client-bundle.gitleaks.stripe-access-token', 'bbbbbbbbbbbbbbbb', 'filed before the approver rule', now() + interval '30 days', 'person', 'dana', 'Dana')`, project); err != nil {
		t.Fatal(err)
	}
	run := gatedRun(t, ctx, p)
	for _, c := range []struct {
		why, update string
		covered     bool
	}{
		{"filed before the rule", "", false},
		{"filed under the rule by another person", "UPDATE approvals SET approves_blocking = true", true},
		{"filed under the rule by the run's author", "UPDATE approvals SET approved_by_subject = 'carl'", false},
	} {
		if c.update != "" {
			if _, err := p.Exec(ctx, c.update); err != nil {
				t.Fatal(err)
			}
		}
		if approved, unapproved, err := Approvals(ctx, p, project, run, raw); err != nil || (len(approved) == 1) != c.covered || (unapproved == 0) != c.covered {
			t.Errorf("an approval %s: %+v, %d uncovered (%v), want covered %v", c.why, approved, unapproved, err, c.covered)
		}
	}
}

// An entry is open while any of its findings has no approval (contracts/finding.md, One problem, one
// entry; #2013): two opengrep rules on one line are one blocking finding uncovered, and stay one until
// both are approved.
func TestApprovalsCountOpenEntries(t *testing.T) {
	p, ctx := seeded(t)
	raw := []byte(`{"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "sarif/opengrep"}}, "results": [
	  {"ruleId": "sql.injection.opengrep.tainted-sql", "partialFingerprints": {"canyon/v1": "aaaaaaaaaaaaaaaa"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "server.js"}, "region": {"startLine": 10}}}], "properties": {"canyon": {"blocks": true}}},
	  {"ruleId": "sql.injection.opengrep.express-sqli", "partialFingerprints": {"canyon/v1": "bbbbbbbbbbbbbbbb"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "server.js"}, "region": {"startLine": 10}}}], "properties": {"canyon": {"blocks": true}}}]}]}`)
	approve := func(rule, fingerprint string) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
			VALUES ($1, $2, $3, 'parameterised upstream', now() + interval '30 days', 'person', 'dana@meridian.example', 'Dana Whitfield', true)`, project, rule, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	if _, unapproved, err := Approvals(ctx, p, project, gatedRun(t, ctx, p), raw); err != nil || unapproved != 1 {
		t.Fatalf("one rule on one line, nothing approved: %d open (%v)", unapproved, err)
	}
	approve("sql.injection.opengrep.tainted-sql", "aaaaaaaaaaaaaaaa")
	if approved, unapproved, err := Approvals(ctx, p, project, gatedRun(t, ctx, p), raw); err != nil || len(approved) != 1 || unapproved != 1 {
		t.Fatalf("one of its two approved: %+v, %d open (%v)", approved, unapproved, err)
	}
	approve("sql.injection.opengrep.express-sqli", "bbbbbbbbbbbbbbbb")
	if approved, unapproved, err := Approvals(ctx, p, project, gatedRun(t, ctx, p), raw); err != nil || len(approved) != 2 || unapproved != 0 {
		t.Fatalf("both approved: %+v, %d open (%v)", approved, unapproved, err)
	}
}

// A ship's own check reads an approval as standing only ShipMargin ahead (#2204): one five minutes from expiry
// still covers its finding where it is read, and clears no deploy.
func TestAnApprovalNearItsExpiryClearsNoShip(t *testing.T) {
	p, ctx := seeded(t)
	run := gatedRun(t, ctx, p)
	raw := `{"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "sarif/gitleaks"}}, "results": [
	  {"ruleId": "secrets.client-bundle.gitleaks.stripe-access-token", "partialFingerprints": {"canyon/v1": "cccccccccccccccc"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "pay.js"}, "region": {"startLine": 3}}}], "properties": {"canyon": {"blocks": true}}}]}]}`
	if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', $1, $2, 'verdict', 'verdict', jsonb_build_object('state', 'blocked', 'blocking', 1, 'warnings', 0, 'findings', $3::jsonb))`, run, project, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		VALUES ($1, 'secrets.client-bundle.gitleaks.stripe-access-token', 'cccccccccccccccc', 'rotation is ticketed', now() + interval '5 minutes', 'person', 'dana', 'Dana', true)`, project); err != nil {
		t.Fatal(err)
	}
	if _, open, err := Approvals(ctx, p, project, run, []byte(raw)); err != nil || open != 0 {
		t.Fatalf("where it is read, an approval five minutes from expiry covers its finding: %d open (%v)", open, err)
	}
	if ok, err := Cleared(ctx, p, project, run); err != nil || ok {
		t.Fatalf("an approval five minutes from expiry clears the ship: %v (%v)", ok, err)
	}
	if _, err := p.Exec(ctx, "UPDATE approvals SET expires_at = now() + interval '11 minutes'"); err != nil {
		t.Fatal(err)
	}
	if ok, err := Cleared(ctx, p, project, run); err != nil || !ok {
		t.Fatalf("an approval eleven minutes from expiry clears nothing: %v (%v)", ok, err)
	}
}

// A patch run's blocking finding is covered only by an approval of someone other than its author, the author of
// the run whose image it rebases (contracts/routes.md, Patch runs): no one approves, through a patch Canyon
// started, a blocking finding of code they shipped, a push by a GitHub account they linked included. A patch run
// whose rebased run recorded no author is covered by none (fail closed), and Cleared reads each so.
func TestAnApprovalCoversAPatchRunAgainstItsAuthor(t *testing.T) {
	p, ctx := seeded(t)
	raw := []byte(`{"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "sarif/gitleaks"}}, "results": [
	  {"ruleId": "secrets.client-bundle.gitleaks.stripe-access-token", "partialFingerprints": {"canyon/v1": "cccccccccccccccc"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "pay.js"}, "region": {"startLine": 3}}}], "properties": {"canyon": {"blocks": true}}}]}]}`)
	if _, err := p.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
		VALUES ($1, 'secrets.client-bundle.gitleaks.stripe-access-token', 'cccccccccccccccc', 'reviewed on carl''s run', now() + interval '30 days', 'person', 'dana', 'Dana', true)`, project); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display)
		VALUES ('gr_patched', $1, $2, 'blocked', 'patch', 'canyon', 'patch', 'Canyon')`, project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data)
		VALUES ('run', 'gr_patched', $1, 'verdict', 'verdict', jsonb_build_object('state', 'blocked', 'findings', $2::jsonb))`, project, string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, author string
		covered      bool
	}{
		{"no author recorded", "author_kind = NULL, author_subject = NULL, author_display = NULL, author_github = NULL", false},
		{"carl's code", "author_kind = 'person', author_subject = 'carl', author_display = 'Carl', author_github = NULL", true},
		{"dana's code", "author_kind = 'person', author_subject = 'dana', author_display = 'Dana', author_github = NULL", false},
		{"a push by the GitHub account dana linked", "author_kind = 'person', author_subject = 'github:erin', author_display = 'erin', author_github = 42", false},
	} {
		if _, err := p.Exec(ctx, "UPDATE runs SET "+c.author+" WHERE id = 'gr_patched'"); err != nil {
			t.Fatal(err)
		}
		if c.name == "a push by the GitHub account dana linked" {
			if _, err := p.Exec(ctx, "INSERT INTO github_links (github_id, subject, login) VALUES (42, 'dana', 'erin')"); err != nil {
				t.Fatal(err)
			}
		}
		approved, unapproved, err := Approvals(ctx, p, project, "gr_patched", raw)
		cleared, cerr := Cleared(ctx, p, project, "gr_patched")
		if err != nil || cerr != nil || (unapproved == 0) != c.covered || (len(approved) == 1) != c.covered || cleared != c.covered {
			t.Errorf("dana's approval of a patch run of %s: %d approved, %d uncovered, cleared %v, want covered %v (%v %v)", c.name, len(approved), unapproved, cleared, c.covered, err, cerr)
		}
	}
}
