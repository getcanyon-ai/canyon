package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const linkRequired = `{"error":"github_link_required","message":"Meridian asks whoever approves a push's blocking findings to link their GitHub account first: link it, then approve"}`

// With Guardrails' "Approvers of a push must link GitHub" on (#2342), a push run's blocking finding is
// approved only by an admin or a responder who linked a GitHub account: dana, an admin who linked none, is
// refused github_link_required and told why on the run read, and the approval she filed while it was off,
// or files on another run, covers the push's finding nowhere once it is on; rhea, a responder who linked
// an account that didn't push, approves it. A run no push started is approved as before. Only an admin
// turns it on, and only from a browser.
func TestPushApproversMustLinkGitHub(t *testing.T) {
	pool, ctx, base := approverHost(t)
	fps := approverFindings
	if _, err := pool.Exec(ctx, `UPDATE runs SET started_by_github = 583231 WHERE id = 'gr_push';
		INSERT INTO github_links (github_id, subject, login) VALUES (777, 'sub-rhea', 'rhea-gh')`); err != nil {
		t.Fatal(err)
	}
	dana, rhea := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	approve := func(c *http.Client, run, fp string) int {
		t.Helper()
		code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+run+"/findings/"+fp+"/approve", approveBody())
		return code
	}
	covered := func(run, fp string) bool {
		t.Helper()
		_, body, _ := do(t, rhea, http.MethodGet, base+"/api/v1/gate-runs/"+run, "")
		for _, f := range runOf(body)["findings"].([]any) {
			if f := f.(map[string]any); f["partialFingerprints"].(map[string]any)["canyon/v1"] == fp {
				return f["approval"] != nil
			}
		}
		t.Fatalf("no finding %s on %s: %s", fp, run, lastBody)
		return false
	}
	setting := func(c *http.Client, on bool) (int, string) {
		t.Helper()
		code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/preferences", map[bool]string{true: `{"push_approvers_linked":true}`, false: `{"push_approvers_linked":false}`}[on])
		return code, lastBody
	}
	if code := approve(dana, "gr_push", fps[0]); code != 200 || !covered("gr_push", fps[0]) { // off, as by default: unchanged
		t.Fatalf("with the setting off the admin approves the push: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"a member": signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), "the admin from the CLI": cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")} {
		if code, body := setting(c, true); code != 403 {
			t.Errorf("%s turns it on: %d %s", who, code, body)
		}
	}
	if code, body := setting(dana, true); code != 200 || !strings.Contains(body, `"push_approvers_linked":true`) ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE actor_subject = 'sub-dana' AND detail->'push_approvers_linked' = '{"before": false, "after": true}'`) != 1 {
		t.Fatalf("the admin turns it on from a browser, audited: %d %s", code, body)
	}
	if covered("gr_push", fps[0]) {
		t.Fatal("the approval the admin, who linked no GitHub account, filed while it was off still covers the push")
	}
	if code := approve(dana, "gr_push", fps[4]); code != 403 || lastBody != linkRequired {
		t.Fatalf("the admin who linked no GitHub account approves the push: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"the admin": dana, "the responder": rhea} {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_push", "")
		if linked := who == "the responder"; (runOf(body)["approvable"] == true) != linked || (runOf(body)["link_github"] == true) == linked {
			t.Errorf("%s reads gr_push approvable %v, link_github %v", who, runOf(body)["approvable"], runOf(body)["link_github"])
		}
	}
	if code := approve(dana, "gr_carl", fps[4]); code != 200 || !covered("gr_carl", fps[4]) || covered("gr_push", fps[4]) {
		t.Fatalf("the admin's approval on carl's run, no push's: %d, carl's %t, the push's %t", code, covered("gr_carl", fps[4]), covered("gr_push", fps[4]))
	}
	if code := approve(rhea, "gr_push", fps[0]); code != 200 || !covered("gr_push", fps[0]) {
		t.Fatalf("the responder who linked a GitHub account approves the push: %d %s", code, lastBody)
	}
	if code, body := setting(dana, false); code != 200 || !covered("gr_push", fps[4]) {
		t.Fatalf("turned off, the admin's approval covers the push again: %d %s", code, body)
	}
}

// With sign-in off nobody links a GitHub account, so the setting can't be turned on there (#2342).
func TestPushApproversMustLinkGitHubNeedsSignIn(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodPut, base+"/api/v1/preferences", `{"push_approvers_linked":true}`); code != 409 ||
		lastBody != `{"error":"signin_required","message":"asking approvers of a push to link GitHub needs sign-in on: without it nobody can link an account"}` {
		t.Fatalf("turned on with sign-in off: %d %s", code, lastBody)
	}
}
