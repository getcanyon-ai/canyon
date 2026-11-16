package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// previewAnswering is a clean host whose preview answered the given answer after its deploy.
func previewAnswering(t *testing.T, answer string) *promoteHost {
	t.Helper()
	h := clean(t, page(t), http.StatusAccepted, started)
	h.status = mutate(t, h.status, func(read map[string]any) {
		var a any
		if err := json.Unmarshal([]byte(answer), &a); err != nil {
			t.Fatal(err)
		}
		read["envs"].(map[string]any)["preview"].(map[string]any)["answered"] = a
	})
	return h
}

// A version that answers an error on preview is not promoted from the terminal, with or without --yes:
// the refusal is said before the version lines and the question, and nothing is sent (cli.md §6.3; QA
// round 4, django-400 was promoted with --yes answering 400).
func TestAVersionThatAnswersAnErrorOnPreviewIsNotPromoted(t *testing.T) {
	for _, args := range [][]string{nil, {"--yes"}} {
		h := previewAnswering(t, `{"status":400,"said":"Bad Request (400)"}`)
		out, errs, code := promoting(t, h, "y\n", true, args...)
		if code != exitBlocked || len(h.posts) != 0 || errs != "" {
			t.Errorf("%v: exit %d, sent %q, stderr %q", args, code, h.posts, errs)
		}
		wantsPromote(t, "deploy-prod-refused.txt", out)
	}
	h := previewAnswering(t, `{"status":400,"said":"Bad Request (400)"}`)
	out, _, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || len(h.posts) != 0 {
		t.Errorf("ai: exit %d, sent %q", code, h.posts)
	}
	wantsPromote(t, "deploy-prod-ai-refused.txt", out)
}

// The host's refusal, met after the status read (preview was deployed again in between), is said the
// same way, from its own version and answer.
func TestTheHostsPreviewAnsweredRefusalIsSaidAsTheTerminalsOwn(t *testing.T) {
	h := clean(t, page(t), http.StatusConflict, page(t)["refusal_answered"])
	out, _, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || len(h.posts) != 1 {
		t.Errorf("exit %d, sent %q", code, h.posts)
	}
	wantsPromote(t, "deploy-prod-ai-refused.txt", out)
}

// Only an answer to say beside readiness refuses: a 401 at / can be the app's own sign-in. Where the org
// has a person approve production, the CLI still asks, and the person who approves sees the answer.
func TestOnlyAnErrorRefusesAndAnApprovalIsStillAskedFor(t *testing.T) {
	h := previewAnswering(t, `{"status":401,"said":""}`)
	if out, _, code := promoting(t, h, "", false, "--yes"); code != exitOK || len(h.posts) != 1 || strings.Contains(out, "Not promoted") {
		t.Errorf("a 401: exit %d, sent %q\n%s", code, h.posts, out)
	}
	waiting := approvalOn(t, page(t))
	waiting.status = mutate(t, waiting.status, func(read map[string]any) {
		read["envs"].(map[string]any)["preview"].(map[string]any)["answered"] = map[string]any{"error": "refused"}
	})
	if out, _, code := promoting(t, waiting, "", false, "--yes", "--format", "ai"); code != exitBlocked || len(waiting.posts) != 1 || !strings.HasPrefix(out, "CANYON PROMOTE — RESULT: WAITING FOR APPROVAL\n") {
		t.Errorf("approval on: exit %d, sent %q\n%s", code, waiting.posts, out)
	}
}

// A passed deploy's step to production says production will refuse a version that answered an error,
// while the org promotes directly; where a person approves production, the step stays the command.
func TestAPassedStepToProductionSaysAnErrorAnswerIsRefused(t *testing.T) {
	for _, c := range []struct{ answered, blockedBy, want string }{
		{`{"status":400,"said":""}`, `[]`, "\n3. Production will refuse this version: preview answered 400 Bad Request at /. Tell the user before they ask for production.\n"},
		{`{"status":400,"said":""}`, `["prod.human-approval"]`, "\n3. To put this in production later: canyon deploy --env prod\n"},
		{`{"status":401,"said":""}`, `[]`, "\n2. To put this in production later: canyon deploy --env prod\n"},
	} {
		page, blockedBy, want := strings.Replace(buildPage(t, nil, true), `"image_digest":"sha256:`, `"answered":`+c.answered+`,"image_digest":"sha256:`, 1), c.blockedBy, c.want
		h := &gateHost{run: aRun("clean", 0, 0), page: page, status: reply{http.StatusOK, `{"receipt":{"org":"Meridian","blocked_by":` + blockedBy + `,"rules":[]}}`}}
		if out, _, code := verbingIn(t, h, projectDir(t), "deploy", "--format", "ai"); code != exitOK || !strings.Contains(out, want) {
			t.Errorf("blocked_by %s: exit %d, want %q in\n%s", blockedBy, code, want, out)
		}
	}
}

// Another run of the app under way refuses the promotion: the line names that run and what to run once
// it has finished, as a rollback's does, and under ai the block says it (#2200: the host's bare line
// named no run and no next step).
func TestAPromotionRefusedForAnotherRunNamesItAndTheNextStep(t *testing.T) {
	busy := json.RawMessage(`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"}}`)
	h := clean(t, page(t), http.StatusConflict, busy)
	_, errs, code := promoting(t, h, "", false, "--yes")
	if want := "\n  ✗ another run of claims-portal is under way (gr_9a1c); let it finish, then run canyon deploy --env prod again\n"; code != exitError || errs != want {
		t.Errorf("exit %d, stderr %q; want %q", code, errs, want)
	}
	h = clean(t, page(t), http.StatusConflict, busy)
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitError || errs != "" || len(h.posts) != 1 {
		t.Errorf("ai: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-busy.txt", out)
}

// reviewing is a host whose receipt blocks only because the review of preview's own package is still
// under way — a teammate's canyon check of the same code — with promote_run that review, gr_9a1c.
func reviewing(t *testing.T, state string, blockedBy ...string) *promoteHost {
	t.Helper()
	whole := page(t)
	h := &promoteHost{run: reply{http.StatusOK, `{"run":{"id":"gr_9a1c","state":"` + state + `"}}`}, status: mutate(t, blocked(t, whole), func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["blocked_by"] = blockedBy
		r["rules"].([]any)[0].(map[string]any)["why"] = "v1.3's review is " + state
		read["promote_run"] = "gr_9a1c"
	})}
	return h
}

// While the review of preview's own package is pending or running, the receipt's rules that read it
// cannot pass yet: the promotion is refused as another run under way is, naming it, exit 1 — never
// BLOCKED, exit 2, which is a blocking finding (#2203). A receipt that also blocks on a rule that reads
// no run, or a review that has ended, still renders the receipt.
func TestAPromotionWhilePreviewsReviewRunsNamesTheRun(t *testing.T) {
	for _, state := range []string{"running", "pending"} {
		h := reviewing(t, state, "prod.clean-gate", "sql.string-concat")
		_, errs, code := promoting(t, h, "", false, "--yes")
		if want := "\n  ✗ another run of claims-portal is under way (gr_9a1c); let it finish, then run canyon deploy --env prod again\n"; code != exitError || errs != want || len(h.posts) != 0 {
			t.Errorf("%s: exit %d, sent %q, stderr %q; want %q", state, code, h.posts, errs, want)
		}
	}
	h := reviewing(t, "running", "prod.clean-gate")
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitError || errs != "" || len(h.posts) != 0 {
		t.Errorf("ai: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-busy.txt", out)
}

// The receipt refuses in its own words, BLOCKED, exit 2, whenever the run's read can't say the review
// of preview's package is all that stops it: a rule that reads no run blocks too — the person's
// among them, as cli.md §6.3 says — the review has ended, the read fails or names another run, or
// promote_run is no run id, which is then never asked for and never printed.
func TestAPromotionKeepsTheReceiptUnlessOnlyTheReviewUnderWayStopsIt(t *testing.T) {
	hostile := reviewing(t, "running", "prod.clean-gate")
	hostile.status = mutate(t, hostile.status, func(read map[string]any) { read["promote_run"] = "gr_x\n\x1b[31m" })
	for name, h := range map[string]*promoteHost{
		"prod.human-approval": reviewing(t, "pending", "prod.clean-gate", "prod.human-approval"),
		"owner.required":      reviewing(t, "running", "prod.clean-gate", "owner.required"),
		"stack.blessed":       reviewing(t, "running", "prod.clean-gate", "stack.blessed"),
		"platform.known-vuln": reviewing(t, "running", "prod.clean-gate", "platform.known-vuln"),
		"prod.egress-allowed": reviewing(t, "running", "prod.clean-gate", "prod.egress-allowed"),
		"ended":               reviewing(t, "blocked", "prod.clean-gate"),
		"a 500":               withRun(reviewing(t, "running", "prod.clean-gate"), reply{http.StatusInternalServerError, `{"error":"internal","message":"internal error"}`}),
		"a 404":               withRun(reviewing(t, "running", "prod.clean-gate"), reply{http.StatusNotFound, `{"error":"not_found","message":"no such run"}`}),
		"another run":         withRun(reviewing(t, "running", "prod.clean-gate"), reply{http.StatusOK, `{"run":{"id":"gr_7eaf","state":"running"}}`}),
		"no run id":           hostile,
	} {
		out, errs, code := promoting(t, h, "", false, "--yes")
		if code != exitBlocked || len(h.posts) != 0 || errs != "" || !strings.Contains(out, "Nothing was promoted.") || strings.Contains(out, "under way") {
			t.Errorf("%s: exit %d, sent %q, stderr %q; want the receipt in\n%s", name, code, h.posts, errs, out)
		}
	}
	for _, path := range hostile.gets {
		if strings.HasPrefix(path, "/api/v1/gate-runs/") {
			t.Errorf("no run id: asked %q", path)
		}
	}
	h := withRun(reviewing(t, "running", "prod.clean-gate"), hostile.run)
	h.status = hostile.status
	if out, _, _ := promoting(t, h, "", false, "--yes", "--format", "ai"); strings.ContainsAny(out, "\x1b") || strings.Contains(out, "gr_x") {
		t.Errorf("no run id: printed it\n%q", out)
	}
}

func withRun(h *promoteHost, run reply) *promoteHost {
	h.run = run
	return h
}

// A run id the host sent that is no run id — a newline, an escape — is left out of the line, never
// printed; an app's name goes through cell() in both renderings (#2200).
func TestABusyPromotionPrintsNoHostileRunOrName(t *testing.T) {
	hostile := json.RawMessage(`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c\n\u001b[2J","state":"running"}}`)
	h := clean(t, page(t), http.StatusConflict, hostile)
	out, errs, code := promoting(t, h, "", false, "--yes")
	if want := "\n  ✗ another run of claims-portal is under way; let it finish, then run canyon deploy --env prod again\n"; code != exitError || errs != want {
		t.Errorf("exit %d, stderr %q; want %q", code, errs, want)
	}
	if strings.Contains(out, "under way") || !strings.HasSuffix(out, noRebuild+"\n") {
		t.Errorf("stdout is not the transcript up to the refusal:\n%q", out)
	}
	h = clean(t, page(t), http.StatusConflict, hostile)
	out, errs, code = promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitError || errs != "" || strings.ContainsAny(out, "\x1b") || !strings.Contains(out, "\noutcome: nothing was promoted: another run of claims-portal is under way.\n") {
		t.Errorf("ai: exit %d, stderr %q\n%q", code, errs, out)
	}
	for _, format := range []Format{FormatHuman, FormatAI} {
		var o, e strings.Builder
		a := &app{out: &o, err: &e, render: Render{Format: format}, verb: command{name: "deploy --env prod"}}
		if a.busy(promoted{app: "claims\nportal", host: "http://localhost:7777"}, "") != exitError || strings.Contains(o.String()+e.String(), "claims\nportal") ||
			!strings.Contains(o.String()+e.String(), `another run of claims\nportal is under way`) {
			t.Errorf("%v: stdout %q, stderr %q", format, o.String(), e.String())
		}
	}
}
