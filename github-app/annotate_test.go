package githubapp

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/runs"
)

// installed is hooked with the App registered, installation 42 live and the claims-portal project made.
func installed(t *testing.T) chan string {
	t.Helper()
	g, pool, calls := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	linked(t, pool)
	if rec := deliver(t, "installation", "d-install", "installation_created.json"); rec.Code != 202 {
		t.Fatalf("the installation: %d %s", rec.Code, rec.Body)
	}
	return calls
}

// reviewed pushes to claims-portal as delivery id, the fake gate ending the run as given, and answers the check's PATCHes.
func reviewed(t *testing.T, calls chan string, id, state, reason string, blocking int, log any) []CheckRun {
	t.Helper()
	hook(func() { review = func(runs.Input) (string, string, int, any) { return state, reason, blocking, log } })
	return pushed(t, calls, id)
}

// pushed pushes to claims-portal as delivery id, at a sha of 3s when edits ask, and answers the check's PATCHes.
func pushed(t *testing.T, calls chan string, id string, edits ...string) []CheckRun {
	t.Helper()
	if rec := deliver(t, "push", id, "push_default_branch.json", append([]string{`"name":"billing-api"`, `"name":"claims-portal"`}, edits...)...); rec.Code != 202 {
		t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
	}
	var out []CheckRun
	for _, patch := range ended(t, calls, id) {
		var run CheckRun
		if err := json.Unmarshal([]byte(patch[strings.Index(patch, "{"):]), &run); err != nil {
			t.Fatal(err)
		}
		out = append(out, run)
	}
	return out
}

// A pushed canyon.toml whose app line names another app fails the check, saying which, and nothing is
// packaged; one naming this app goes on to the declarations' own check (#1938).
func TestAPushedCanyonTomlNamingAnotherAppFailsTheCheck(t *testing.T) {
	calls := installed(t)
	run := pushed(t, calls, "d-other", "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", strings.Repeat("c", 40))
	if end := run[len(run)-1]; end.Conclusion != "failure" || end.Output.Title != "canyon.toml names other-app, not claims-portal" || end.Output.Summary != "This repository feeds claims-portal. Change the app line to claims-portal, or delete it." {
		t.Fatalf("another app's canyon.toml: %s %q %q", end.Conclusion, end.Output.Title, end.Output.Summary)
	}
	if n := rows(t, courier.Pool, "SELECT count(*) FROM packages"); n != 0 {
		t.Fatalf("packaged anyway: %d", n)
	}
	run = pushed(t, calls, "d-own", "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", strings.Repeat("d", 40))
	if end := run[len(run)-1]; end.Output.Title != "Canyon refused this commit" { // the test's Declare refuses every canyon.toml
		t.Fatalf("this app's canyon.toml went to the declarations: %q", end.Output.Title)
	}
}

// golden is the recorded claims-portal log with a blocking finding and the AI review's two high and
// one medium: its secrets finding's snippet holds the masked sk_live_ literal.
var golden, _ = os.ReadFile("../gate/testdata/review/claims-portal.sarif.json")

// A result is one annotation on its lines, no columns, failure when it blocks, warning at high, else
// notice, worded from its canyon bag; the stored verdict is all that is read, so the secret's literal in
// the snippet reaches GitHub nowhere. A blocked run fails the check with its counts.
func TestAReviewEndsTheCheckAndItsFindingsAreAnnotatedFromTheVerdictAlone(t *testing.T) {
	calls := installed(t)
	patches := reviewed(t, calls, "d-golden", "blocked", "", 1, json.RawMessage(golden))
	body, _ := json.Marshal(patches)
	secret := `{"path":"apps/web/src/lib/payments.ts","start_line":14,"end_line":14,"annotation_level":"failure","title":"Stripe live secret key is bundled into the browser build","message":"payments.ts is imported by CheckoutPanel.tsx, a client component — everything a client component imports ends up in the JavaScript your users download. This key can charge cards and issue refunds.","raw_details":"Move the Stripe call behind a server route and read the key from process.env.STRIPE_SECRET_KEY. Rotate the key in Stripe — it has been in your code — then store the new one: canyon secret set STRIPE_SECRET_KEY.\n\nNo credential literal appears anywhere under apps/web/src/. The Stripe client is constructed only in server-side code, and the key is read from process.env.STRIPE_SECRET_KEY at runtime."}` //nolint:gosec // test: the golden's finding worded, no credential in it
	if strings.Contains(string(body), "sk_live_") || strings.Contains(string(body), "column") || !strings.Contains(string(body), secret) {
		t.Fatalf("the golden's annotations: %s", body)
	}
	if run := patches[0]; len(patches) != 1 || run.Conclusion != "failure" || run.Output.Title != "1 blocking finding" || len(run.Output.Annotations) != 4 ||
		run.Output.Annotations[1].AnnotationLevel != "warning" || run.Output.Annotations[2].AnnotationLevel != "warning" || run.Output.Annotations[3].AnnotationLevel != "notice" {
		t.Fatalf("one PATCH ends it failed, a blocking, two high and a medium finding: %s", body)
	}
}

// 205 findings are 200 annotations, 50 per request, each request appending the next 50 in the log's
// order, the conclusion on the last, and the summary saying where the other five are.
func TestAnnotationsGoFiftyPerRequestAppendedAndStopAtTwoHundred(t *testing.T) {
	var log struct {
		Runs []struct{ Results []map[string]json.RawMessage }
	}
	if err := json.Unmarshal(golden, &log); err != nil {
		t.Fatal(err)
	}
	results := make([]map[string]json.RawMessage, 205) // the secrets finding on lines 1 to 205
	for i := range results {
		results[i] = maps.Clone(log.Runs[0].Results[0])
		results[i]["locations"] = json.RawMessage(fmt.Sprintf(`[{"physicalLocation":{"artifactLocation":{"uri":"a.ts"},"region":{"startLine":%d,"endLine":%d}}}]`, i+1, i+1))
	}
	patches := reviewed(t, installed(t), "d-many", "blocked", "", 205, map[string]any{"runs": []any{map[string]any{"results": results}}})
	lines := 0
	for i, p := range patches {
		for _, a := range p.Output.Annotations {
			if lines++; a.StartLine != lines {
				t.Fatalf("annotation %d is on line %d: the batches are not appended in the log's order", lines, a.StartLine)
			}
		}
		if last := i == len(patches)-1; last != (p.Conclusion == "failure") || !strings.HasSuffix(p.Output.Summary, "…and 5 more findings in Canyon.") {
			t.Errorf("PATCH %d: %+v", i, p)
		}
	}
	if len(patches) != 4 || lines != 200 {
		t.Fatalf("%d PATCH(es) carrying %d annotation(s), want 4 carrying 200", len(patches), lines)
	}
}

// A check never stays in progress, and never passes a review due and not done: a project busy past the retries
// fails, a superseded run fails with its reason, a database erroring mid-review and a GitHub refusing the
// conclusion each fail it unfinished; a project deleted after its package was made is no app, no review due.
func TestACheckAlwaysEndsAndOnlyAFinishedReviewPassesIt(t *testing.T) {
	calls := installed(t)
	pool := courier.Pool
	reviewed(t, calls, "d-first", "clean", "", 0, map[string]any{})
	exec := func(q string) {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Error(err)
		}
	}
	exec("INSERT INTO runs (id, project_id, package_id, state) SELECT 'gr_busy', 'claims-portal', id, 'running' FROM packages")
	busy := pushed(t, calls, "d-busy")
	exec("UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_busy'")
	hook(func() {
		review = func(in runs.Input) (string, string, int, any) { // a person starts a run of the package while the courier's is pending
			hook(func() { review = clean }) // the person's run's: set before the courier's check can end and the next scenario set its own
			tx, _ := pool.Begin(ctx)
			defer func() { _ = tx.Rollback(ctx) }()
			if _, _, err := runs.StartTx(ctx, tx, courier.Durable, runs.StartInput{Project: in.Project, Package: in.Package, Policy: in.Policy}); err != nil || tx.Commit(ctx) != nil {
				t.Error(err)
			}
			return clean(in)
		}
	})
	superseded := pushed(t, calls, "d-superseded")
	// the person's run reads review when it starts, which can be after the courier's check ended: it ends
	// before the next scenario sets review, or it runs that one's too (#2364)
	var person string
	if err := pool.QueryRow(ctx, "SELECT id FROM runs ORDER BY created_at DESC LIMIT 1").Scan(&person); err != nil {
		t.Fatal(err)
	}
	h, err := dbos.RetrieveWorkflow[string](durable, person)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetResult(dbos.WithHandleTimeout(30 * time.Second)); err != nil {
		t.Fatalf("the person's run %s did not end: %v", person, err)
	}
	hook(func() {
		review = func(in runs.Input) (string, string, int, any) {
			exec("ALTER TABLE runs RENAME TO runs_gone")
			return clean(in)
		}
	})
	broken := pushed(t, calls, "d-db-error")
	exec("ALTER TABLE runs_gone RENAME TO runs")
	hook(func() { review, refuse = clean, `"conclusion":"success"` })
	refused := pushed(t, calls, "d-502")
	deleted := pushed(t, calls, "d-deleted", "a1b2c3d4", "33333333")
	var pkg string
	_ = pool.QueryRow(ctx, "SELECT id FROM packages").Scan(&pkg)
	unfinished := "failure Canyon could not finish this review: Push again; if it keeps failing, the Canyon server's log says why."
	for _, c := range []struct {
		got  []CheckRun
		want string
	}{
		{busy, "failure Another Canyon run was in progress: Another Canyon run for claims-portal was in progress; push again."},
		{superseded, "failure The review didn't finish: Canyon's review of " + pkg + " didn't finish: it was cancelled, or another run of this app replaced it."}, {broken, unfinished}, {refused, unfinished},
		{deleted, "neutral This repository isn't linked to a Canyon app: An admin links it in Settings › GitHub."}, // its app was deleted while the check ran (#1917)
	} {
		if run := c.got[0]; len(c.got) != 1 || run.Status != "completed" || run.Conclusion+" "+run.Output.Title+": "+run.Output.Summary != c.want {
			t.Errorf("want %q, the check ended %+v", c.want, c.got)
		}
	}
}

// A push past the project's gate-run limit fails its check at once, naming when to push again, and starts no run.
func TestAPushPastTheRateLimitFailsTheCheckWithoutRetrying(t *testing.T) {
	calls := installed(t)
	reviewed(t, calls, "d-first", "clean", "", 0, map[string]any{})
	if _, err := courier.Pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, failure_reason, created_at)
		SELECT 'gr_old' || n, 'claims-portal', (SELECT id FROM packages LIMIT 1), 'failed', 'interrupted', now() - interval '10 minutes' FROM generate_series(1, 29) n`); err != nil {
		t.Fatal(err)
	}
	got := pushed(t, calls, "d-limited")
	want := regexp.MustCompile(`^Canyon's limit of 30 reviews an hour for this app is reached\. Push again after \d\d:\d\d UTC\.$`)
	if len(got) != 1 || got[0].Conclusion != "failure" || got[0].Output.Title != "Canyon's review limit is reached" || !want.MatchString(got[0].Output.Summary) {
		t.Fatalf("the check past the limit ended %+v", got)
	}
	var n int
	if err := courier.Pool.QueryRow(ctx, "SELECT count(*) FROM runs").Scan(&n); err != nil || n != 30 {
		t.Fatalf("%d runs (%v), want the 30 counted and no more", n, err)
	}
}

// A run blocked only by findings the org approved is one a deploy goes on from (routes.md, the deploys
// row), so its check succeeds and never blocks the merge; the annotations still name the findings (#996).
func TestABlockedRunWhoseEveryBlockingFindingIsApprovedSucceeds(t *testing.T) {
	calls := installed(t)
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID string            `json:"ruleId"`
				Prints map[string]string `json:"partialFingerprints"`
				Props  struct {
					Canyon struct{ Blocks bool } `json:"canyon"`
				} `json:"properties"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(golden, &log); err != nil {
		t.Fatal(err)
	}
	for _, run := range log.Runs {
		for _, r := range run.Results {
			if r.Props.Canyon.Blocks {
				if _, err := courier.Pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
					VALUES ('claims-portal', $1, $2, 'a test key', now() + interval '7 days', 'person', 'dana', 'Dana', true)`, r.RuleID, r.Prints["canyon/v1"]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	patches := reviewed(t, calls, "d-approved", "blocked", "", 1, json.RawMessage(golden))
	if run := patches[len(patches)-1]; run.Conclusion != "success" || run.Output.Title != "Every blocking finding approved" || len(run.Output.Annotations) != 4 {
		t.Fatalf("a blocked run with its blocking finding approved: %s %q, %d annotations", run.Conclusion, run.Output.Title, len(run.Output.Annotations))
	}
}
