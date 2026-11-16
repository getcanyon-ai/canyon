package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A person deploys a pull request's version to preview from the browser once its review passed (#1947):
// the CLI's ask is 404 as before, an unreviewed one is review_not_passed; preview then names the pull
// request, its runs do, and nothing promotes it, whichever run the promotion names.
func TestAPersonDeploysAPullRequestToPreview(t *testing.T) {
	pool, ctx, base, browser, cli, mainRun := approvalHost(t, false) // the gate is the golden log from here: blocked
	_, checked := upload(t, browser, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export const fix = 1\n"}), "", "?check=true")
	pr, _ := packageOf(checked)["id"].(string)
	if _, err := pool.Exec(ctx, "UPDATE packages SET source_kind = 'repo-pr', source_ref = '9f1c2e3d4b5a69788796a5b4c3d2e1f00f1e2d3c', pr_number = 12, pr_branch = 'fix-login' WHERE id = $1", pr); err != nil { // as a pull request's push packages it
		t.Fatal(err)
	}
	deploys, body := base+"/api/v1/projects/claims-portal/deploys", `{"package":"`+pr+`"}`
	if code, out, _ := do(t, browser, http.MethodPost, deploys, body); code != 409 || out["error"] != "review_not_passed" {
		t.Fatalf("a pull request no run has passed: %d %s", code, lastBody)
	}
	_, started, _ := do(t, browser, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", body)
	reviewed, _ := runOf(started)["id"].(string)
	verdict := waitVerdict(t, browser, base, reviewed)
	if verdict["state"] != "blocked" {
		t.Fatalf("the pull request's review: %s", lastBody)
	}
	if code, out, _ := do(t, browser, http.MethodPost, deploys, body); code != 409 || out["error"] != "review_not_passed" {
		t.Fatalf("a pull request its review blocked: %d %s", code, lastBody)
	}
	approvedOf := func() any { // the runs list's word for the review, as the dashboard's button reads it (#1955)
		t.Helper()
		_, list, _ := do(t, browser, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
		for _, r := range list["runs"].([]any) {
			if r := r.(map[string]any); r["id"] == reviewed {
				return r["approved"]
			}
		}
		t.Fatalf("no row for %s: %s", reviewed, lastBody)
		return nil
	}
	if got := approvedOf(); got != nil {
		t.Fatalf("a blocked run nobody approved reads approved %v", got)
	}
	var blocking []string // approved, every one: the run a deploy goes on by
	findings, _ := verdict["findings"].([]any)
	for _, f := range findings {
		f, _ := f.(map[string]any)
		canyon, _ := f["properties"].(map[string]any)["canyon"].(map[string]any)
		if canyon["blocks"] == true {
			blocking = append(blocking, f["partialFingerprints"].(map[string]any)["canyon/v1"].(string))
		}
	}
	if len(blocking) == 0 {
		t.Fatalf("the golden log blocks on nothing: %s", lastBody)
	}
	another := signedIn(t, ctx, pool, base, "sub-sam", "sam@meridian.example", true) // an admin who did not start the review (#2204)
	for _, fp := range blocking {
		if code, _, _ := do(t, another, http.MethodPost, base+"/api/v1/gate-runs/"+reviewed+"/findings/"+url.PathEscape(fp)+"/approve", `{"reason":"Reviewed with security","expires_at":"`+time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)+`"}`); code != 200 && code != 201 {
			t.Fatalf("approving %s: %d %s", fp, code, lastBody)
		}
	}
	if got := approvedOf(); got != true {
		t.Fatalf("a blocked run every blocking finding of which is approved reads approved %v", got)
	}
	if code, out, _ := do(t, cli, http.MethodPost, deploys, body); code != 404 || out["error"] != "not_found" {
		t.Fatalf("the CLI deploying a pull request: %d %s", code, lastBody)
	}
	code, started, _ := do(t, browser, http.MethodPost, deploys, body)
	run, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, browser, base, run)["state"] != "blocked" {
		t.Fatalf("the browser deploying a pull request whose every blocking finding is approved: %d %s", code, lastBody)
	}
	if do(t, browser, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); !strings.Contains(lastBody, `"pull_request":{"branch":"fix-login","number":12}`) || !strings.Contains(lastBody, `"gate_run":"`+run+`"`) {
		t.Errorf("preview names the pull request it runs: %s", lastBody)
	}
	if do(t, browser, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", ""); strings.Count(lastBody, `"pull_request":{"number":12,"branch":"fix-login"}`) != 2 {
		t.Errorf("the pull request's two runs name it: %s", lastBody)
	}
	for _, gateRun := range []string{run, mainRun} {
		if code, out, _ := do(t, browser, http.MethodPost, base+promotions, `{"gate_run":"`+gateRun+`"}`); code != 409 || out["error"] != "pull_request" {
			t.Errorf("promoting %s while preview runs a pull request: %d %s", gateRun, code, lastBody)
		}
	}
	// the CLI's ask while a person approves production: refused before any approval is filed
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET prod_approval = true"); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := do(t, cli, http.MethodPost, base+promotions, `{"gate_run":"`+mainRun+`"}`); code != 409 || out["error"] != "pull_request" || count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals") != 0 {
		t.Errorf("the CLI's ask while preview runs a pull request: %d %s", code, lastBody)
	}
}
