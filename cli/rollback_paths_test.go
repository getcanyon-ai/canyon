package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// withEarlier is the rollback read with its earlier[] replaced by rows.
func withEarlier(t *testing.T, rows ...map[string]any) json.RawMessage {
	t.Helper()
	list := make([]any, len(rows))
	for i, row := range rows {
		list[i] = row
	}
	return mutate(t, rollbackRead(t), func(read map[string]any) { read["earlier"] = list })
}

// row is one earlier version, gone live on 2 Oct, kept unless the caller says otherwise.
func row(pkg, version, digest string, kept bool) map[string]any {
	return map[string]any{"package": pkg, "version": version, "image_digest": digest, "deployed_at": "2026-10-02T14:40:00Z", "promoted_by": nil, "kept": kept}
}

// receiptOf is the rollback read's receipt for the review, changed by change.
func receiptOf(t *testing.T, change func(r map[string]any)) json.RawMessage {
	t.Helper()
	return mutate(t, rollbackRead(t), func(read map[string]any) { change(read["receipt"].(map[string]any)) })
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// A rollback's retry and redirect lines name the version: plain `deploy --env prod` would promote
// preview instead.
func TestARollbacksLinesNameItsVersion(t *testing.T) {
	again := "canyon deploy --env prod --version v1.3"
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("failed", "scan_no_room")}
	_, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
	if code != exitError || !strings.Contains(errs, "Run it again in a few minutes: "+again+"\n") {
		t.Errorf("scan_no_room: exit %d, stderr %q", code, errs)
	}
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("failed", "scan_no_room")}
	out, _, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	if code != exitError || !strings.Contains(out, "In a few minutes, run: "+again+"\n") {
		t.Errorf("scan_no_room under ai: exit %d\n%s", code, out)
	}
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")} // nothing can answer, and --yes was not given
	_, errs, code = rollingBack(t, h, "--version", "v1.3")
	if code != exitError || len(h.posts) != 1 || !strings.Contains(errs, "deploy --env prod --version v1.3 asks before it changes production, and nothing here can answer: re-run it with --yes") {
		t.Errorf("no --yes off a terminal: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")} // under ai, the block an agent acts on (#2873)
	out, errs, code = rollingBack(t, h, "--version", "v1.3", "--format", "ai")
	if code != exitError || len(h.posts) != 1 || errs != "" {
		t.Errorf("no --yes under ai: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-ask.txt", out)
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), tty: true, input: "n\n"}
	out, _, code = rollingBack(t, h, "--version", "v1.3")
	if code != exitError || len(h.posts) != 1 || !strings.HasSuffix(out, "\n  Nothing was promoted.\n") {
		t.Errorf("a no: exit %d, sent %q\n%s", code, h.posts, out)
	}
}

// What a promotion ships is the image production ran: a receipt that names another, or none, promotes
// nothing; a receipt that blocks is the refusal it always is.
func TestARollbackShipsOnlyTheImageProductionRan(t *testing.T) {
	for _, c := range []struct {
		name    string
		receipt json.RawMessage
		code    int
		want    string
	}{
		{"another image", receiptOf(t, func(r map[string]any) { r["ships"].(map[string]any)["image_digest"] = digestA }), exitError,
			"v1.3's review doesn't name the image production ran, so nothing was promoted"},
		{"no image", receiptOf(t, func(r map[string]any) { delete(r, "ships") }), exitError,
			"v1.3's review doesn't name the image production ran, so nothing was promoted"},
		{"a rule blocks", receiptOf(t, func(r map[string]any) {
			r["blocked_by"] = []string{"prod.clean-gate"}
			rule := r["rules"].([]any)[0].(map[string]any)
			rule["passed"], rule["why"] = false, "v1.3 has a newer review than the one named"
		}), exitBlocked, "v1.3 has a newer review than the one named"},
	} {
		h := &rollbackHost{status: rollbackRead(t), receipt: c.receipt, review: reviewed("clean", "")}
		out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
		if code != c.code || len(h.posts) != 1 || !strings.Contains(out+errs, c.want) {
			t.Errorf("%s: exit %d, sent %q, stderr %q\n%s", c.name, code, h.posts, errs, out)
		}
	}
}

// A name is a package id before it is a label, and names one package; of that package's rows the
// newest kept one goes back. Status's command is what the resolver puts back.
func TestTheVersionResolvesByIdThenLabel(t *testing.T) {
	for _, c := range []struct {
		name   string
		status json.RawMessage
		named  string
		sent   string // the gate-runs body; "" when nothing is sent
	}{
		{"a label that is another package's id", withEarlier(t, row("pkg_aaaa1111", "bbbb2222", digestA, true), row("pkg_bbbb2222", "v1.2", digestB, true)),
			"bbbb2222", `{"package":"pkg_bbbb2222","rollback_to":"` + digestB + `"}`},
		{"a labelled version by its pkg_ id", rollbackRead(t), "pkg_7a1c", `{"package":"pkg_7a1c","rollback_to":"` + keptDigest + `"}`},
		{"a version twice, the newer not kept", withEarlier(t, row("pkg_9e40", "v1.4", digestA, false), row("pkg_9e40", "v1.4", digestB, true)),
			"v1.4", `{"package":"pkg_9e40","rollback_to":"` + digestB + `"}`},
		{"a label on two packages", withEarlier(t, row("pkg_aaaa1111", "v1.4", digestA, true), row("pkg_bbbb2222", "v1.4", digestB, true)), "v1.4", ""},
		{"a row with no package", withEarlier(t, row("", "v1.3", keptDigest, true)), "v1.3", ""},
		{"a row with no image", withEarlier(t, row("pkg_7a1c", "v1.3", "", true)), "v1.3", ""},
	} {
		h := &rollbackHost{status: c.status, review: reviewed("clean", "")}
		out, errs, code := rollingBack(t, h, "--version", c.named, "--yes")
		switch {
		case c.sent == "" && (code != exitError || len(h.posts) != 0):
			t.Errorf("%s: exit %d and sent %q; want nothing sent\n%s%s", c.name, code, h.posts, out, errs)
		case c.sent != "" && (len(h.posts) == 0 || h.posts[0] != "gate-runs "+c.sent):
			t.Errorf("%s: sent %q, want %s", c.name, h.posts, c.sent)
		}
	}
	twice := withEarlier(t, row("pkg_aaaa1111", "v1.4", digestA, true), row("pkg_bbbb2222", "v1.4", digestB, true))
	h := &rollbackHost{status: twice}
	if out, _, _ := rollingBack(t, h, "--version", "v1.4", "--yes"); !strings.HasSuffix(out, "\n  v1.4 names more than one of production's earlier versions, so Canyon can't tell which to put back. Name one by its id: aaaa1111, bbbb2222.\n") {
		t.Errorf("a label on two packages printed\n%s", out)
	}
	h = &rollbackHost{status: twice} // under ai, a REFUSED block naming each by its id (#2785)
	out, _, code := rollingBack(t, h, "--version", "v1.4", "--yes", "--format", "ai")
	if code != exitError || len(h.posts) != 0 {
		t.Errorf("a label on two packages, ai: exit %d, sent %q", code, h.posts)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-ambiguous.txt", out)
	var s statusRead
	if err := json.Unmarshal(withEarlier(t, row("pkg_9e40", "v1.4", digestA, false), row("pkg_9e40", "v1.4", digestB, true)), &s); err != nil {
		t.Fatal(err)
	}
	if out := rendered(t, s, nil); !strings.Contains(out, "\n  Put one back: canyon deploy --env prod --version v1.4\n") {
		t.Errorf("status names no command for the kept v1.4:\n%s", out)
	}
	if err := json.Unmarshal(withEarlier(t, row("pkg_9e40", "v1.4", digestA, false), row("pkg_5b20", "v1.2", digestB, false)), &s); err != nil {
		t.Fatal(err)
	}
	var ai strings.Builder
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	if out := rendered(t, s, nil) + ai.String(); !strings.Contains(out, "Earlier in production") || strings.Contains(strings.ToLower(out), "put one back") {
		t.Errorf("nothing kept, and a command printed:\n%s", out)
	}
}

// A version the host sends is printed as text, and a command is printed only for one shaped like a
// version, so nothing the host names becomes a line an agent runs.
func TestAHostileVersionIsNeverACommand(t *testing.T) {
	var s statusRead
	hostile := row("pkg_7a1c", "v1 3; rm -rf ~ \x1b[2J\nx", keptDigest, true)
	hostile["deployed_at"] = "2026-10-02\x1b[31m\nlater"
	if err := json.Unmarshal(withEarlier(t, hostile), &s); err != nil {
		t.Fatal(err)
	}
	var ai strings.Builder
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	for _, out := range []string{rendered(t, s, nil), ai.String()} {
		if strings.Contains(out, "\x1b") || strings.Contains(out, "\nx") || strings.Contains(out, "\nlater") || strings.Contains(strings.ToLower(out), "put one back") {
			t.Errorf("a hostile version printed\n%q", out)
		}
	}
	// Named by its id, a row whose label is hostile: the line that says what to run again names the id.
	h := &rollbackHost{status: withEarlier(t, row("pkg_7a1c", "v1 3; rm -rf ~", keptDigest, true)), review: reviewed("clean", ""),
		gate: &routeAnswer{http.StatusConflict, `{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"}}`}}
	_, errs, code := rollingBack(t, h, "--version", "pkg_7a1c", "--yes")
	if code != exitError || strings.Contains(errs, "rm -rf") || !strings.Contains(errs, "; let it finish, then run canyon deploy --env prod --version pkg_7a1c again") {
		t.Errorf("a run under way: exit %d, stderr %q", code, errs)
	}
}

// A review that blocks with every blocking finding approved goes on to the promotion.
func TestAnApprovedRollbackReviewIsPromoted(t *testing.T) {
	res := approvedRun(t, false).Result
	res.ID, res.Package = "gr_rb01", "pkg_7a1c"
	body, err := json.Marshal(map[string]any{"run": res})
	if err != nil {
		t.Fatal(err)
	}
	h := &rollbackHost{status: rollbackRead(t), review: body}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
	if code != exitOK || len(h.posts) != 2 || !strings.Contains(out, "Putting v1.3 back in production.") {
		t.Errorf("exit %d, sent %q, stderr %q\n%s", code, h.posts, errs, out)
	}
}

// Every refusal the two routes answer is the host's, or the run's, in words, exit 1; nothing after it is
// sent, and nothing claims a review started before the host took it.
func TestTheRoutesRefusalsStopTheRollback(t *testing.T) {
	team := `{"error":"team_required","message":"Only claims-portal's team can do this. Ask an owner to add you."}`
	for _, c := range []struct {
		name        string
		gate, promo *routeAnswer
		want        string
	}{
		{"a run under way", &routeAnswer{http.StatusConflict, `{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"}}`}, nil,
			"another run of claims-portal is under way (gr_9a1c); let it finish, then run canyon deploy --env prod --version v1.3 again"},
		{"not earlier", &routeAnswer{http.StatusConflict, `{"error":"not_earlier","message":"production never ran that image of v1.3"}`}, nil, "production never ran that image of v1.3"},
		{"a reason refused", &routeAnswer{http.StatusBadRequest, `{"error":"invalid_reason","message":"a reason is 1 to 1024 characters of plain text"}`}, nil, "a reason is 1 to 1024 characters of plain text"},
		{"rate limited", &routeAnswer{http.StatusTooManyRequests, `{"error":"rate_limited","message":"Too many rollback reviews this hour","retry_after_s":60}`}, nil, "Too many rollback reviews this hour"},
		{"demoted before the review", &routeAnswer{http.StatusForbidden, team}, nil, "Only claims-portal's team can do this."},
		{"nothing to ship", nil, &routeAnswer{http.StatusConflict, `{"error":"nothing_to_ship","message":"v1.3's review recorded no image to put back; review it again"}`}, "v1.3's review recorded no image to put back"},
		{"outdated", nil, &routeAnswer{http.StatusConflict, `{"error":"outdated","message":"production runs another version now: review it again"}`}, "production runs another version now: review it again"},
		{"the review still running", nil, &routeAnswer{http.StatusConflict, `{"error":"run_in_progress","message":"v1.3's review is still under way","run":{"id":"gr_rb01","state":"running"}}`}, "v1.3's review is still under way"},
		{"demoted before the promotion", nil, &routeAnswer{http.StatusForbidden, team}, "Only claims-portal's team can do this."},
	} {
		h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), gate: c.gate, promo: c.promo}
		out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
		sent := 2
		if c.gate != nil {
			sent = 1
		}
		if code != exitError || len(h.posts) != sent || !strings.Contains(errs, c.want) || c.gate != nil && strings.Contains(out, "Reviewing") {
			t.Errorf("%s: exit %d, sent %q, stderr %q\n%s", c.name, code, h.posts, errs, out)
		}
	}
}

// Where a person approves production in the dashboard, a rollback asks for that approval as a
// promotion does, and every line that says what to run again names the version.
func TestARollbackAsksForTheApprovalAPromotionDoes(t *testing.T) {
	approval := &routeAnswer{http.StatusAccepted, `{"approval":{"code":"K7QP-4RXM","url":"http://localhost:7777/#/approve?code=K7QP-4RXM","expires_at":"2026-09-03T12:00:00Z"}}`}
	asks := func() *rollbackHost {
		return &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), promo: approval, receipt: receiptOf(t, func(r map[string]any) {
			r["blocked_by"] = []string{"prod.human-approval"}
			rule := r["rules"].([]any)[1].(map[string]any)
			rule["passed"], rule["why"] = false, "a person approves it in Canyon's dashboard; an agent or the CLI only asks"
		})}
	}
	out, _, code := rollingBack(t, asks(), "--version", "v1.3")
	if code != exitBlocked || !strings.Contains(out, "\n  Nothing was promoted yet. canyon deploy --env prod --version v1.3 --wait waits here for the answer.\n") {
		t.Errorf("exit %d\n%s", code, out)
	}
	out, _, code = rollingBack(t, asks(), "--version", "v1.3", "--format", "ai")
	if code != exitBlocked {
		t.Errorf("ai: exit %d", code)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-approval.txt", out)
	const pending = `{"state":"pending"}`
	for _, c := range []struct {
		read string
		code int
		want string
	}{
		{`{"state":"approved","run":"gr_9c41","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`, exitOK, "  Live   http://claims-portal.prod.localhost:7777\n"},
		{`{"state":"outdated"}`, exitBlocked, "\n  Production runs another version now. Nothing was promoted. Run: canyon deploy --env prod --version v1.3\n"},
		{`{"state":"expired"}`, exitBlocked, "\n  The approval expired. Nothing was promoted. Run: canyon deploy --env prod --version v1.3\n"},
	} {
		h := asks()
		h.reads = []string{pending, c.read}
		if out, _, code := rollingBack(t, h, "--version", "v1.3", "--wait"); code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %q in\n%s", c.read, code, c.want, out)
		}
	}
}

// Under ai with --yes, the rollback ends as a promotion does: the PROMOTED block.
func TestARollbackUnderAIEndsPromoted(t *testing.T) {
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	if code != exitOK || errs != "" {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-done.txt", out)
}

// A rollback that goes ahead past an advisory rule says it, from the review's own receipt (#2300).
func TestARollbackUnderAISaysItsAdvisoryLines(t *testing.T) {
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")}
	h.receipt = mutate(t, rollbackRead(t), func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["rules"] = append(r["rules"].([]any), map[string]any{"id": "platform.known-vuln", "name": "No known-vulnerable system libraries or build tools", "blocks": false, "passed": false,
			"why": "6 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds", "unset": false})
	})
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	line := "\nadvisory: No known-vulnerable system libraries or build tools (platform.known-vuln): 6 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds\n"
	if code != exitOK || errs != "" || !strings.Contains(out, line) || !strings.Contains(out, ". "+aiAdvisoryStep+"\n\nexit code: 0\n") {
		t.Errorf("exit %d, stderr %q: want %q and the step in\n%s", code, errs, line, out)
	}
}

// --reason says why a version goes back, so it goes with --version alone.
func TestReasonGoesWithVersion(t *testing.T) {
	for _, args := range [][]string{{"deploy", "--reason", "INC-1"}, {"deploy", "--env", "prod", "--reason", "INC-1"}} {
		_, errs, code := run(t, nil, false, args...)
		if code != exitError || errs != "\n  ✗ --reason says why an earlier version goes back: canyon deploy --env prod --version <version> --reason <why>\n" {
			t.Errorf("%q: exit %d, stderr %q", args, code, errs)
		}
	}
}
