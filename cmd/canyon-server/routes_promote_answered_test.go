package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// appAnswersOK has every deployed app answer Canyon's one GET / with 200 for the test, as a working app
// does: with none, the ask finds no such host and the preview answered nothing.
func appAnswersOK(t *testing.T) {
	t.Helper()
	pipe.Ask = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	})
	t.Cleanup(func() { pipe.Ask = nil })
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answeredInPreview records what preview's app answered after its deploy, as the deploy step would have.
func answeredInPreview(t *testing.T, ctx context.Context, pool *pgxpool.Pool, answer string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE deploys SET answered = $1 WHERE project_id = 'claims-portal' AND env = 'preview'", answer); err != nil {
		t.Fatal(err)
	}
}

// goldenAnsweredRefusal is the app-page fixture's refusal of a preview that answered 400 (v1.3).
func goldenAnsweredRefusal(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/app-page/claims-portal.json")
	var doc map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	var refusal any
	if err := json.Unmarshal(doc["refusal_answered"], &refusal); err != nil {
		t.Fatal(err)
	}
	return string(mustJSON(refusal))
}

const promotionAsked = "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/promotions'"

// A version whose preview answered an error is not promoted from the CLI, whatever it sends; a person in
// the browser promotes it anyway, and the trail says they did despite the answer (contracts/routes.md, the
// promotions row's preview_answered; QA round 4, django-400).
func TestAVersionWhosePreviewAnsweredAnErrorIsNotPromotedFromTheCLI(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, false)
	goldenLabels(t, ctx, pool)
	answeredInPreview(t, ctx, pool, `{"status":400,"said":"Bad Request (400)"}`)
	ran := fake.Runs.Load()
	for _, asked := range []struct {
		who  string
		c    *http.Client
		body string
	}{
		{"the CLI", cli, `{"gate_run":"` + deploy + `"}`},
		{"the CLI overriding", cli, `{"gate_run":"` + deploy + `","promote_anyway":true}`},
		{"the browser without the override", browser, `{"gate_run":"` + deploy + `"}`},
	} {
		if code, body, _ := do(t, asked.c, http.MethodPost, base+promotions, asked.body); code != 409 || string(mustJSON(body)) != goldenAnsweredRefusal(t) {
			t.Fatalf("%s: %d %s\nwant 409 %s", asked.who, code, lastBody, goldenAnsweredRefusal(t))
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, promotionAsked); n != 0 || fake.Runs.Load() != ran {
		t.Fatalf("a refused promotion wrote %d rows and started %d containers", n, fake.Runs.Load()-ran)
	}
	// the override is the promotions route's, so its level is too: a viewer is refused, a contributor in the browser promotes
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'vic@meridian.example', 'viewer', 'person', 'sub-dana', 'dana'), ('claims-portal', 'email', 'erin@meridian.example', 'contributor', 'person', 'sub-dana', 'dana')`); err != nil {
		t.Fatal(err)
	}
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	if code, body, _ := do(t, vic, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`","promote_anyway":true}`); code != 403 || body["error"] != "team_required" {
		t.Fatalf("a viewer's override: %d %s", code, lastBody)
	}
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	code, body, _ := do(t, erin, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`","promote_anyway":true}`)
	if code != 202 || waitVerdict(t, erin, base, runOf(body)["id"].(string))["state"] != "clean" {
		t.Fatalf("a contributor promotes it anyway: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, promotionAsked+` AND detail->'answered' = '{"status":400,"said":"Bad Request (400)"}'::jsonb`); n != 1 {
		t.Fatalf("the override's row names the answer it overrode: %d rows", n)
	}
}

// Only an answer to say beside readiness refuses: a 401 at / can be the app's own sign-in, and promoting it
// anyway where nothing refuses records nothing more. Nothing answering refuses in its own words.
func TestOnlyAnAnswerToSayBesideReadinessRefusesAPromotion(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, false)
	answeredInPreview(t, ctx, pool, `{"error":"refused"}`)
	_, status, _ := do(t, browser, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	version := status["envs"].(map[string]any)["preview"].(map[string]any)["package"].(string)[len("pkg_"):]
	if code, body, _ := do(t, cli, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); code != 409 || body["error"] != "preview_answered" ||
		body["message"] != "Not promoted: "+version+" gives no answer at / on preview (refused). Fix the app and deploy again, or have a person promote it from the dashboard after checking it." {
		t.Fatalf("nothing answered: %d %s", code, lastBody)
	}
	for _, answer := range []string{`{"status":401,"said":""}`, `{"status":404,"said":""}`, `{"status":302,"said":""}`, `{"status":200,"said":""}`, ""} { // "": a row with no answer recorded
		if answer == "" {
			if _, err := pool.Exec(ctx, "UPDATE deploys SET answered = NULL WHERE env = 'preview'"); err != nil {
				t.Fatal(err)
			}
		} else {
			answeredInPreview(t, ctx, pool, answer)
		}
		if run := promoted(t, cli, base, deploy); run["state"] != "clean" {
			t.Fatalf("%q from the CLI promotes: %s", answer, mustJSON(run))
		}
	}
	answeredInPreview(t, ctx, pool, `{"status":401,"said":""}`)
	code, body, _ := do(t, browser, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`","promote_anyway":true}`)
	if code != 202 || waitVerdict(t, browser, base, runOf(body)["id"].(string))["state"] != "clean" || count(t, ctx, pool, promotionAsked+" AND detail ? 'answered'") != 0 {
		t.Fatalf("an override of nothing records nothing: %d %s", code, lastBody)
	}
}

// The override names the version the person saw: a run of another package does not carry it past
// preview's answer, even where an admin made a passing review advisory.
func TestTheOverrideCountsOnlyForTheVersionPreviewRuns(t *testing.T) {
	pool, ctx, base, browser, _, _ := approvalHost(t, false)
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/policies/prod.clean-gate?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("a passing review advisory: %d %s", code, lastBody)
	}
	uploadPulled(t, browser, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "b": "2"}))
	newer := blockedRun(t, browser, base)
	answeredInPreview(t, ctx, pool, `{"status":500,"said":""}`)
	if code, body, _ := do(t, browser, http.MethodPost, base+promotions, `{"gate_run":"`+newer+`","promote_anyway":true}`); code != 409 || body["error"] != "preview_answered" {
		t.Fatalf("an override naming another version: %d %s", code, lastBody)
	}
}

// With production approved in the dashboard the CLI's ask still files an approval; the page reads what
// preview answered, the approve the page sends without the override is refused and leaves it pending, and
// the person's Approve with it promotes, its row naming the answer.
func TestAnApprovalOfAVersionWhosePreviewAnsweredAnErrorIsThePersonsOverride(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, true)
	answeredInPreview(t, ctx, pool, `{"status":400,"said":"Bad Request (400)"}`)
	code := askApproval(t, cli, base, deploy)
	at := base + "/api/v1/promotion-approvals/" + code
	if got, body, _ := do(t, browser, http.MethodPost, base+"/api/v1/promotion-approvals/ZZZZ-ZZZZ/approve", "{"); got != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown code is 404 before its body is read: %d %s", got, lastBody)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); string(mustJSON(read["answered"])) != `{"said":"Bad Request (400)","status":400}` {
		t.Fatalf("the page reads what preview answered: %s", lastBody)
	}
	if _, read, _ := do(t, cli, http.MethodGet, at, ""); read["answered"] != nil {
		t.Fatalf("the CLI's poll carries no answer: %s", lastBody)
	}
	var image string
	if err := pool.QueryRow(ctx, "UPDATE deploys SET image = image || '-other' WHERE env = 'preview' RETURNING image").Scan(&image); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "outdated" || read["answered"] != nil {
		t.Fatalf("preview runs another image: the answer is not the approval's: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET image = left(image, length(image) - 6) WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if got, body, _ := do(t, browser, http.MethodPost, at+"/approve", ""); got != 409 || body["error"] != "preview_answered" {
		t.Fatalf("an approve without the override: %d %s", got, lastBody)
	}
	if _, read, _ := do(t, browser, http.MethodGet, at, ""); read["state"] != "pending" {
		t.Fatalf("a refused approve leaves it pending: %s", lastBody)
	}
	got, body, _ := do(t, browser, http.MethodPost, at+"/approve", `{"promote_anyway":true}`)
	if got != 202 || waitVerdict(t, browser, base, runOf(body)["id"].(string))["state"] != "clean" {
		t.Fatalf("the person's Approve promotes anyway: %d %s", got, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'approve promotion' AND detail->'answered' = '{"status":400,"said":"Bad Request (400)"}'::jsonb`); n != 1 {
		t.Fatalf("the approval's row names the answer it overrode: %d rows", n)
	}
}

// With sign-in off no session tells the dashboard from the CLI, so the override counts from any caller;
// without it the refusal stands there too.
func TestWithSignInOffTheOverrideCountsFromAnyCaller(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	answeredInPreview(t, ctx, pool, `{"status":400,"said":""}`)
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`); code != 409 || body["error"] != "preview_answered" {
		t.Fatalf("without the override: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`","promote_anyway":true}`)
	if code != 202 || waitVerdict(t, c, base, runOf(body)["id"].(string))["state"] != "clean" || count(t, ctx, pool, promotionAsked+" AND detail ? 'answered'") != 1 {
		t.Fatalf("with it: %d %s", code, lastBody)
	}
}
