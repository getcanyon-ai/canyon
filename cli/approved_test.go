package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// approvedRun is the golden run with its blocking finding approved; with two, the high advisory
// blocks as well and only the first blocking finding carries an approval.
func approvedRun(t *testing.T, both bool) checkRun {
	t.Helper()
	run := goldenRun(t)
	approved := false
	for i := range run.Result.Findings {
		f := &run.Result.Findings[i]
		blocks, _ := f.Blocking()
		if both && !blocks {
			yes := true
			f.Properties.Canyon.Blocks, blocks = &yes, true
			run.Result.Blocking, run.Result.Warnings, both = run.Result.Blocking+1, run.Result.Warnings-1, false
		}
		if blocks && !approved {
			f.Approval = &approval{Reason: "Test key on a demo branch; rotated before launch", ExpiresAt: "2026-10-07T00:00:00Z"}
			f.Approval.ApprovedBy = actorRef{Kind: "person", Subject: "sub-dana", Display: "Dana Whitfield"}
			approved = true
		}
	}
	return run
}

// D1: a run whose every blocking finding carries an unexpired approval is approved, not blocked, on
// check and on deploy alike, and both exit 0 (cli.md §3.5); one blocking finding without one is
// still blocked and still 2.
func TestAnApprovedRunIsApprovedOnCheckAndOnDeploy(t *testing.T) {
	head := "\n  ! Review approved · 1 blocking finding, approved by Dana Whitfield until 7 Oct\n    \"Test key on a demo branch; rotated before launch\"\n"
	checked, code := deployTail(t, "check", approvedRun(t, false), buildPage(t, nil, false))
	if code != exitOK || !strings.Contains(checked, head) || !strings.Contains(checked, "\n  Ship it: canyon deploy\n") ||
		strings.Contains(checked, "Review blocked") || strings.Contains(checked, "Fix it, then check again") || strings.Contains(checked, "Nothing was built") {
		t.Errorf("an approved check exited %d:\n%s", code, checked)
	}
	shipped, code := deployTail(t, "deploy", approvedRun(t, false), buildPage(t, buildLines, true))
	if code != exitOK || !strings.Contains(shipped, head+"\n  Every blocking problem is approved, so this version goes to preview.\n") ||
		strings.Contains(shipped, "Deployed anyway") || strings.Contains(shipped, "Review blocked") {
		t.Errorf("an approved deploy exited %d:\n%s", code, shipped)
	}
	half, code := deployTail(t, "check", approvedRun(t, true), buildPage(t, nil, false))
	if code != exitBlocked || !strings.Contains(half, "✗ Review blocked · 2 blocking findings") || strings.Contains(half, "Review approved") {
		t.Errorf("one of two blocking findings approved exited %d:\n%s", code, half)
	}
}

// Approved is not clean: an agent reads RESULT: APPROVED, never PASSED, is told to deploy rather
// than fix, and json keeps the run's own state with the exit code it earned (cli.md §5.2–5.3).
func TestAnAgentReadsApprovedNotPassed(t *testing.T) {
	var ai strings.Builder
	a := aiApp(&ai, "check")
	if code := a.verdict(approvedRun(t, false)); code != exitOK || !strings.HasPrefix(ai.String(), "CANYON REVIEW — RESULT: APPROVED\n") ||
		!strings.Contains(ai.String(), "outcome: nothing was deployed. Every blocking finding carries an approval, so canyon deploy\nships this code.\n") ||
		!strings.Contains(ai.String(), "1. Run: canyon deploy\n2. Do not change this code for these findings on your own. The approval is\n   the org's decision.\n") ||
		!strings.HasSuffix(ai.String(), "exit code: 0\n") {
		t.Errorf("exit %d:\n%s", code, ai.String())
	}
	var js strings.Builder
	j := &app{out: &js, render: Render{Format: FormatJSON}, verb: command{name: "check"}}
	var got struct {
		Result   string `json:"result"`
		ExitCode int    `json:"exit_code"`
	}
	if code := j.verdict(approvedRun(t, false)); code != exitOK || json.Unmarshal([]byte(js.String()), &got) != nil || got.Result != "blocked" || got.ExitCode != 0 {
		t.Errorf("json exited %d: %s", code, js.String())
	}
}

// With sign-in off, a host's one caller is the anonymous admin, with no display (contracts/routes.md, person /
// anonymous): the CLI names it someone not signed in, as the dashboard does (web/src/ui/Actor.tsx), everywhere it
// prints an actor, never its subject as if it were a name (#2705). Each surface's lines naming it, against
// testdata/anonymous-actor.txt.
func TestAnAnonymousActorReadsAsSomeoneNotSignedIn(t *testing.T) {
	want := golden(t, "anonymous-actor.txt") // before the directory moves
	anon := actorRef{Kind: "person", Subject: "anonymous"}
	var got strings.Builder
	say := func(surface, out string, marks ...string) {
		got.WriteString(surface + "\n")
		for line := range strings.Lines(out) {
			for _, mark := range marks {
				if strings.Contains(line, mark) {
					got.WriteString(line)
					break
				}
			}
		}
	}
	var verdict checkRun
	for _, c := range []struct {
		surface string
		both    bool
	}{{"canyon check, its one blocker approved", false}, {"canyon check, one of two blockers approved", true}} {
		verdict = approvedRun(t, c.both)
		for i := range verdict.Result.Findings {
			if f := verdict.Result.Findings[i].Approval; f != nil {
				f.ApprovedBy = anon
			}
		}
		out, _ := deployTail(t, "check", verdict, buildPage(t, nil, false))
		say(c.surface, out, "approved by")
	}
	verdict.AppRules = []appAddition{{ID: "sql.string-concat", Name: "No hand-built SQL from user input", State: "blocking", SetBy: anon}}
	var ai strings.Builder
	aiApp(&ai, "check").verdict(verdict)
	say("canyon check --format ai", ai.String(), "approved by", "set by")

	say("canyon secret list", renderedSecrets(t, []secretRow{{Name: "STRIPE_SECRET_KEY", Env: "preview", Where: "runtime", SetBy: anon,
		SetAt: "2026-09-01T10:00:00Z", Origin: "user"}}), "STRIPE_SECRET_KEY")

	const decided = `"decided_by":{"kind":"person","subject":"anonymous","display":""}`
	for _, c := range []struct {
		read string
		args []string
	}{
		{`{"state":"approved","run":"gr_9c41",` + decided + `}`, []string{"--wait"}},
		{`{"state":"denied",` + decided + `}`, []string{"--wait"}},
		{`{"state":"denied",` + decided + `}`, []string{"--wait", "--format", "ai"}},
	} {
		h := approvalOn(t, page(t))
		h.reads = []string{c.read}
		out, _, _ := promoting(t, h, "", false, c.args...)
		say("canyon deploy --env prod "+strings.Join(c.args, " "), out, "Approved by", "Denied by", "denied the approval")
	}

	held := strings.NewReplacer(`"rhea@meridian.example"`, `"anonymous"`, `"Rhea Okafor"`, `""`).Replace(string(heldShape(t, "claims-portal-held-status.json")))
	for _, args := range [][]string{{"--yes"}, {"--yes", "--format", "ai"}} {
		out, errs, _ := promoting(t, &promoteHost{status: json.RawMessage(held), prefs: heldAdmins}, "", false, args...)
		say("canyon deploy --env prod "+strings.Join(args, " ")+", production held", out+errs, "stopped by", "held by")
	}

	body, err := json.Marshal(manifest{Host: "http://h", AppAdditions: []appAddition{
		{ID: "auth.unauthenticated-route", Name: "Every route needs sign-in", State: "blocking", SetBy: anon}}})
	if err != nil {
		t.Fatal(err)
	}
	env := isolated(t)
	base, _ := serveManifest(t, http.StatusOK, body)
	inProject(t, "claims-portal")
	for _, args := range [][]string{{"--format", "human"}, {"--format", "ai"}} {
		out, _, _ := run(t, env, false, append([]string{"manifest", "--host", base}, args...)...)
		say("canyon manifest "+strings.Join(args, " "), out, "set by")
	}

	if got.String() != want {
		t.Errorf("rendered\n%s\ntestdata/anonymous-actor.txt wants\n%s", got.String(), want)
	}
}

// An approved deploy that names no deployment kept nothing of the contract, as a clean one would not;
// and with another blocker still open, the approved one still says who let it through.
func TestApprovedButUnshippedFailsAndPartialApprovalsStillShow(t *testing.T) {
	_, code := deployTail(t, "deploy", approvedRun(t, false), buildPage(t, nil, false))
	if code != exitError {
		t.Errorf("an approved deploy with no deploy.completed exited %d", code)
	}
	half, _ := deployTail(t, "check", approvedRun(t, true), buildPage(t, nil, false))
	if !strings.Contains(half, "approved by Dana Whitfield until 7 Oct · Test key on a demo branch") {
		t.Errorf("the approved one of two blockers does not say so:\n%s", half)
	}
}
