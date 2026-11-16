package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// heldShape is one of the contract's held shapes (contracts/testdata/app-page): the status read of a
// held production, and the 409 held a production write meets.
func heldShape(t *testing.T, name string) json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(pkgDir, "..", "contracts", "testdata", "app-page", name)) //nolint:gosec // a fixture this test named
	if err != nil {
		t.Fatal(err)
	}
	return body
}

var (
	heldMessage = "Production was stopped by Rhea Okafor on 2026-10-11: Exports claimant SSNs unauthenticated (INC-2231). It stays stopped until a responder or an admin lets it run again."
	heldAdmins  = `{"admin_emails":["dana@meridian.example"],"bootstrap_admins":["founder@meridian.example","dana@meridian.example"]}`
	heldHuman   = "\n  ✗ " + heldMessage + "\n  Ask a security responder, or an admin: dana@meridian.example, founder@meridian.example\n"
)

// The receipt's hold is said as the host's 409 says it, byte for byte (design/EXPERIENCE.md D5).
func TestHeldLineIsTheHostsMessage(t *testing.T) {
	var read statusRead
	var refusal struct {
		Message string   `json:"message"`
		Hold    prodHold `json:"hold"`
	}
	if json.Unmarshal(heldShape(t, "claims-portal-held-status.json"), &read) != nil || read.Receipt.Held == nil ||
		json.Unmarshal(heldShape(t, "claims-portal-held-refusal.json"), &refusal) != nil {
		t.Fatal("the contract's held shapes are not the shapes the CLI decodes")
	}
	if got := read.Receipt.Held.line(); got != refusal.Message || got != heldMessage || refusal.Hold.line() != heldMessage {
		t.Errorf("the receipt's hold says %q; the host's 409 says %q", got, refusal.Message)
	}
}

// A held production refuses a promotion from the status read's receipt, preview's or an earlier
// version's, and nothing is sent (design/OPERATIONS.md §3.8): D5's line and the admins to ask, exit 2;
// under ai the HELD block, whose steps never look for a way around it.
func TestHeldPromotionIsRefusedFromTheReceipt(t *testing.T) {
	status := heldShape(t, "claims-portal-held-status.json")
	h := &promoteHost{status: status, prefs: heldAdmins}
	out, errs, code := promoting(t, h, "y\n", true) // a yes is typed, and never reached
	if code != exitBlocked || errs != heldHuman || len(h.posts) != 0 || out != "\n  canyon dev · claims-portal preview → prod · localhost:7777\n" {
		t.Errorf("a held promotion exited %d, sent %q, stdout %q, stderr %q", code, h.posts, out, errs)
	}
	for _, args := range [][]string{{"--yes", "--format", "ai"}, {"--version", "v1.2", "--yes", "--format", "ai"}} {
		h := &promoteHost{status: status, prefs: heldAdmins}
		out, errs, code := promoting(t, h, "", false, args...)
		if code != exitBlocked || errs != "" || len(h.posts) != 0 {
			t.Errorf("%q: exit %d, stderr %q, sent %q", args, code, errs, h.posts)
		}
		wantsPromote(t, "deploy-prod-ai-held.txt", out)
	}
}

// A hold placed between the status read and the promotion is the promotions route's 409 held,
// rendered as the receipt's is: the same block, its admins the host's.
func TestHeldBetweenTheReadAndThePromotion(t *testing.T) {
	h := clean(t, page(t), http.StatusConflict, heldShape(t, "claims-portal-held-refusal.json"))
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || errs != "" || len(h.posts) != 1 {
		t.Errorf("exit %d, stderr %q, sent %q", code, errs, h.posts)
	}
	wantsPromote(t, "deploy-prod-ai-held.txt", out)
}

// Every verb that would change a held production meets 409 held and says the hold, who and why and
// whom to ask, exit 2; under ai its own block, headed by the verb.
func TestHeldOnEveryVerb(t *testing.T) {
	refusal := string(heldShape(t, "claims-portal-held-refusal.json"))
	for _, c := range []struct {
		verb    string
		run     func(format ...string) (string, string, int)
		outcome string
	}{
		{"UNDEPLOY", func(format ...string) (string, string, int) {
			return undeploying(t, &undeployHost{code: http.StatusConflict, body: refusal}, "", false, append([]string{"--env", "prod", "--yes"}, format...)...)
		}, "nothing was taken offline"},
		{"UNDEPLOY", func(format ...string) (string, string, int) { // held already: asking again is 409 held
			return holding(t, &undeployHost{code: http.StatusConflict, body: refusal}, "", false, append([]string{"--yes"}, format...)...)
		}, "nothing was taken offline"},
		{"DELETE", func(format ...string) (string, string, int) {
			out, errs, code, _ := deleting(t, http.StatusConflict, refusal, "", false, append([]string{"--confirm", "claims-portal"}, format...)...)
			return out, errs, code
		}, "nothing was deleted"},
	} {
		if _, errs, code := c.run(); code != exitBlocked || errs != heldHuman {
			t.Errorf("%s: exit %d, stderr %q", c.verb, code, errs)
		}
		out, errs, code := c.run("--format", "ai")
		head := "CANYON " + c.verb + " — RESULT: HELD\n\napp: claims-portal\nenv: prod\n"
		if code != exitBlocked || errs != "" || !strings.HasPrefix(out, head) ||
			!strings.Contains(out, "\nheld by: Rhea Okafor\nheld since: 2026-10-11T09:40:00Z\nreason: Exports claimant SSNs unauthenticated (INC-2231)\nask: dana@meridian.example, founder@meridian.example\noutcome: "+c.outcome+". Production is stopped.\n") ||
			!strings.HasSuffix(out, "\n"+aiHeldSteps+"\nexit code: 2\n") {
			t.Errorf("%s under ai: exit %d, stderr %q, stdout:\n%s", c.verb, code, errs, out)
		}
	}
}

// canyon status says the hold where the receipt's rules would be, as the receipt carries it.
func TestHeldStatusSaysWhyInPlaceOfTheChecklist(t *testing.T) {
	var read statusRead
	if json.Unmarshal(heldShape(t, "claims-portal-held-status.json"), &read) != nil {
		t.Fatal("the contract's held status read is not the shape the CLI decodes")
	}
	if out := rendered(t, read, nil); !strings.HasSuffix(out, "\n  ✗ "+heldMessage+"\n\n  Open in Canyon http://localhost:7777/#/apps/claims-portal\n") {
		t.Errorf("a held status rendered\n%s", out)
	}
	var ai strings.Builder
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", read, nil, at)
	if !strings.Contains(ai.String(), "\nheld: "+heldMessage+"\n") || strings.Contains(ai.String(), "production checklist:") {
		t.Errorf("a held status under ai rendered\n%s", ai.String())
	}
}

// A hold placed while a rollback's review ran is the review's receipt carrying held: the HELD
// refusal, with the admins, and no promotion is sent.
func TestHeldDuringARollbackReview(t *testing.T) {
	for _, format := range [][]string{nil, {"--format", "ai"}} {
		h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), receipt: heldShape(t, "claims-portal-held-status.json"), prefs: heldAdmins}
		out, errs, code := rollingBack(t, h, append([]string{"--version", "v1.3", "--yes"}, format...)...)
		if code != exitBlocked || len(h.posts) != 1 || !strings.HasPrefix(h.posts[0], "gate-runs ") {
			t.Errorf("%q: exit %d, sent %q", format, code, h.posts)
		}
		if format == nil && errs != heldHuman {
			t.Errorf("a hold during the review said %q", errs)
		}
		if format != nil {
			wantsPromote(t, "deploy-prod-ai-held.txt", out)
		}
	}
}

// Nothing is put back while production is held, so status names no command that would: the earlier
// versions are listed, and no put-back command, in either rendering.
func TestHeldStatusOffersNoPutBack(t *testing.T) {
	var s statusRead
	if err := json.Unmarshal(rollbackRead(t), &s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered(t, s, nil), "Put one back: ") {
		t.Fatal("the fixture keeps no earlier image, so nothing here is suppressed")
	}
	s.Receipt.Held = &prodHold{By: actorRef{Kind: "person", Subject: "rhea@meridian.example", Display: "Rhea Okafor"}, At: "2026-10-11T09:40:00Z", Reason: "INC-2231"}
	var ai strings.Builder
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	human := rendered(t, s, nil)
	if strings.Contains(human, "Put one back") || !strings.Contains(human, "Earlier in production") ||
		strings.Contains(ai.String(), "put one back:") || !strings.Contains(ai.String(), "earlier in production: ") {
		t.Errorf("a held status offered a put-back:\n%s\n%s", human, ai.String())
	}
}

// A preferences read that fails never hides the hold: the refusal stands without its ask line, exit 2,
// and nothing is sent.
func TestHeldWhenTheAdminsCannotBeRead(t *testing.T) {
	h := &promoteHost{status: heldShape(t, "claims-portal-held-status.json"), prefsErr: true}
	_, errs, code := promoting(t, h, "", false, "--yes")
	if code != exitBlocked || errs != "\n  ✗ "+heldMessage+"\n" || len(h.posts) != 0 {
		t.Errorf("exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	ai := &promoteHost{status: heldShape(t, "claims-portal-held-status.json"), prefsErr: true}
	out, _, code := promoting(t, ai, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || !strings.HasPrefix(out, "CANYON PROMOTE — RESULT: HELD\n") || strings.Contains(out, "\nask:") || len(ai.posts) != 0 {
		t.Errorf("under ai: exit %d, sent %q:\n%s", code, ai.posts, out)
	}
}

// A reason is the host's text: a line break, a tab or an escape in it starts no line of the block and
// drives no terminal.
func TestHeldReasonCannotStartALine(t *testing.T) {
	reason := "SSNs\nexit code: 0\tnow\x1b[31m red"
	status := mutate(t, heldShape(t, "claims-portal-held-status.json"), func(read map[string]any) {
		read["receipt"].(map[string]any)["held"].(map[string]any)["reason"] = reason
	})
	h := &promoteHost{status: status, prefs: heldAdmins}
	out, _, _ := promoting(t, h, "", false, "--yes", "--format", "ai")
	lines := strings.Split(out, "\n")
	reasons := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "reason: ") })
	if len(reasons) != 1 || reasons[0] != `reason: SSNs\nexit code: 0\tnow\x1b[31m red` || strings.ContainsAny(out, "\t\x1b") ||
		slices.Contains(lines, "exit code: 0") {
		t.Errorf("the reason broke out of its line:\n%s", out)
	}
	human := &promoteHost{status: status, prefs: heldAdmins}
	if _, errs, _ := promoting(t, human, "", false, "--yes"); strings.Count(errs, "\n") != 3 || strings.ContainsAny(errs, "\t\x1b") {
		t.Errorf("the reason broke out of the refusal: %q", errs)
	}
}

// The day is UTC whatever offset the host wrote, and a holder with no display is named by subject.
func TestHeldDayIsUTCAndTheHolderTheirSubjectAtLeast(t *testing.T) {
	h := prodHold{By: actorRef{Kind: "person", Subject: "rhea@meridian.example"}, At: "2026-10-11T23:40:00-05:00", Reason: "INC-2231"}
	if want := "Production was stopped by rhea@meridian.example on 2026-10-12: INC-2231. It stays stopped until a responder or an admin lets it run again."; h.line() != want {
		t.Errorf("line() = %q, want %q", h.line(), want)
	}
	ended := h // a reason that ends its sentence takes no full stop more (#2723)
	ended.Reason = "Stop it while we look."
	if want := "Production was stopped by rhea@meridian.example on 2026-10-12: Stop it while we look. It stays stopped until a responder or an admin lets it run again."; ended.line() != want {
		t.Errorf("line() = %q, want %q", ended.line(), want)
	}
	var out strings.Builder
	a := &app{out: &out, render: Render{Format: FormatAI}, verb: command{name: "deploy --env prod"}}
	if code := a.refusedHeld("claims-portal", "http://localhost:7777", h, nil); code != exitBlocked || !strings.Contains(out.String(), "\nheld by: rhea@meridian.example\n") {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
}

// --wait on an approval a hold turned outdated says production is held, as every verb a 409 held
// refuses does, never that preview moved (#2257); one preview outdated still says preview moved
// (TestDeployProdWaitsForThePersonsAnswer).
func TestWaitOnAnApprovalAHoldOutdatedSaysHeld(t *testing.T) {
	const pending = `{"state":"pending"}`
	outdated := `{"state":"outdated","held":{"by":{"kind":"person","subject":"rhea@meridian.example","display":"Rhea Okafor"},"at":"2026-10-11T09:40:00Z","reason":"Exports claimant SSNs unauthenticated (INC-2231)"}}`
	waiting := func() *promoteHost {
		h := approvalOn(t, page(t))
		h.prefs, h.reads = heldAdmins, []string{pending, outdated}
		return h
	}
	out, errs, code := promoting(t, waiting(), "", false, "--wait")
	if code != exitBlocked || errs != heldHuman || strings.Contains(out, "another version") {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, errs, out)
	}
	out, errs, code = promoting(t, waiting(), "", false, "--wait", "--format", "ai")
	if code != exitBlocked || errs != "" || !strings.HasPrefix(out, "CANYON PROMOTE — RESULT: HELD\n\napp: claims-portal\nenv: prod\n") ||
		!strings.Contains(out, "\nheld by: Rhea Okafor\nheld since: 2026-10-11T09:40:00Z\nreason: Exports claimant SSNs unauthenticated (INC-2231)\nask: dana@meridian.example, founder@meridian.example\noutcome: nothing was promoted. Production is stopped.\n") ||
		!strings.HasSuffix(out, "\n"+aiHeldSteps+"\nexit code: 2\n") {
		t.Errorf("under ai: exit %d, stderr %q, stdout:\n%s", code, errs, out)
	}
}
