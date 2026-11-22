package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// A label is free text, so one may be spelled like an id; only the id branch trims the prefix.
func TestAVersionKeepsALabelThatLooksLikeAnId(t *testing.T) {
	if got := (pkgRef{id: "pkg_9e40", label: "pkg_evil"}).version(); got != "pkg_evil" {
		t.Fatalf("a label is answered as written: %s", got)
	}
	if got := (pkgRef{id: "pkg_9e40"}).version(); got != "9e40" {
		t.Fatalf("no label, so the id without its prefix: %s", got)
	}
}

// While the org has a person approve production in the browser, a decision of the fleet approval passes
// prod.human-approval as the admin who gave it there, and only as a person: an agent under the fleet's mark fails it,
// as does the same admin outside it on a session that is not a browser's (contracts/routes.md, The promote receipt).
func TestTheReceiptReadsTheFleetApprovalsAdmin(t *testing.T) {
	dana := actor{Kind: "person", Subject: "sub-dana", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana, approvalOn: true, fleet: true,
		run: &runFacts{id: "gr_patch", state: "clean", pkg: v14, newest: "gr_patch"}}
	if r := evaluate(in); !r.Rules[1].Passed || r.Rules[1].Why != "a person approves it in Canyon's dashboard; an agent or the CLI only asks" || len(r.BlockedBy) != 0 {
		t.Errorf("the fleet's admin: %s", mustJSON(r))
	}
	in.actor = actor{Kind: "agent", Subject: "agent:claude-code/dana"}
	if r := evaluate(in); r.Rules[1].Passed || fmt.Sprint(r.BlockedBy) != "[prod.human-approval]" {
		t.Errorf("an agent under the fleet's mark: %s", mustJSON(r))
	}
	in.actor, in.fleet = dana, false
	if r := evaluate(in); r.Rules[1].Passed || fmt.Sprint(r.BlockedBy) != "[prod.human-approval]" {
		t.Errorf("the admin's CLI: %s", mustJSON(r))
	}
}

func TestTheReceiptFoldsTheFourRulesInTheSnapshotsOrder(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	agent := actor{Kind: "agent", Subject: "agent:claude-code/dana"}
	v14, v13 := pkgRef{id: "pkg_9e40", label: "v1.4"}, pkgRef{id: "pkg_7a1c", label: "v1.3"}
	unlabelled := pkgRef{id: "pkg_5c02"} // a package nobody labelled reads as its id without the prefix
	run := func(state, reason string) *runFacts {
		return &runFacts{id: "gr_8f2a", state: state, failureReason: reason, pkg: v14, newest: "gr_8f2a"}
	}
	base := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana}
	// the whole shape once: order, names, blocks, and the three lines the other cases leave alone
	in := base
	in.run = run("clean", "")
	if got, want := evaluate(in), `{"org":"Meridian","blocked_by":[],"rules":[{"id":"prod.clean-gate","name":"Production needs a passing review","blocks":true,"passed":true,"why":"v1.4 passed review: no blocking finding"},{"id":"prod.human-approval","name":"A person promotes","blocks":true,"passed":true,"why":"a person promotes, or an agent they ask, signed in as them"},{"id":"stack.blessed","name":"Approved stacks only","blocks":false,"passed":false,"why":"no approved stacks set","unset":true},{"id":"owner.required","name":"Every app has an owner","blocks":false,"passed":true,"why":"owned by Dana Whitfield"}]}`; string(mustJSON(got)) != want {
		t.Fatalf("clean and owned:\n got %s\nwant %s", mustJSON(got), want)
	}
	for _, c := range []struct {
		name       string
		run        *runFacts
		unapproved int
		preview    *pkgRef
		why        string
	}{
		{"no run", nil, 0, &v14, "no run has looked at this app"},
		{"running", run("running", ""), 0, &v14, "v1.4's review is running"},
		{"failed build", run("failed", "build"), 0, &v14, "v1.4's run failed: the package did not build"},
		{"failed detect", run("failed", "detect"), 0, &v14, "v1.4's run failed: no buildpack on this host recognised the package"},
		{"failed, a reason without words", run("failed", "unheard-of"), 0, &v14, "v1.4's run failed: unheard-of"},
		{"blocked, one unapproved", run("blocked", ""), 1, &v14, "v1.4's review found 1 blocking finding, not approved"},
		{"blocked, two unapproved", run("blocked", ""), 2, &v14, "v1.4's review found 2 blocking findings, not approved"},
		// a reason names a version, never a pkg_ id: the label when there is one, else the id without the prefix
		{"clean, preview runs another package", run("clean", ""), 0, &v13, "the review named is v1.4's; preview runs v1.3"},
		{"clean, preview runs an unlabelled package", run("clean", ""), 0, &unlabelled, "the review named is v1.4's; preview runs 5c02"},
		{"clean, a newer run of the package", &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_9b01"}, 0, &v14, "v1.4 has a newer review than the one named"},
		{"clean, a newer run of an unlabelled package", &runFacts{id: "gr_8f2a", state: "clean", pkg: unlabelled, newest: "gr_9b01"}, 0, &unlabelled, "5c02 has a newer review than the one named"},
	} {
		in := base
		in.run, in.unapproved, in.preview = c.run, c.unapproved, c.preview
		if r := evaluate(in); r.Rules[0].Passed || r.Rules[0].Why != c.why || fmt.Sprint(r.BlockedBy) != "[prod.clean-gate]" {
			t.Errorf("%s: %s", c.name, mustJSON(r))
		}
	}
	in = base
	in.run = run("blocked", "") // every blocking finding under an unexpired approval: the one place an approval changes an answer
	if r := evaluate(in); !r.Rules[0].Passed || r.Rules[0].Why != "v1.4 passed review: every blocking finding approved" || len(r.BlockedBy) != 0 {
		t.Errorf("blocked, approved: %s", mustJSON(r))
	}
	in.actor = agent // an agent fails the human rule, which states itself whoever is reading
	if r := evaluate(in); r.Rules[1].Passed || r.Rules[1].Why != "a person promotes, or an agent they ask, signed in as them" || fmt.Sprint(r.BlockedBy) != "[prod.human-approval]" {
		t.Errorf("an agent: %s", mustJSON(r))
	}
	for owners, why := range map[int]string{0: "no owner has claimed this app", 2: "owned by Dana Whitfield and 1 other", 3: "owned by Dana Whitfield and 2 others"} {
		in := base
		in.run, in.owners = run("clean", ""), []actor{dana, agent, {Kind: "canyon", Subject: "reconcile"}}[:owners]
		if r := evaluate(in); r.Rules[3].Passed != (owners > 0) || r.Rules[3].Why != why || len(r.BlockedBy) != 0 { // advisory: never in blocked_by
			t.Errorf("%d owners: %s", owners, mustJSON(r))
		}
	}
}

// An admin may set stack.blessed blocking; with nothing to check against it is still neutral, and a
// neutral rule never refuses a promotion (contracts/routes.md, The promote receipt; decision D4).
func TestAnUnsetRuleBlocksNothingWhateverItsState(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana,
		run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_8f2a"}}
	for i := range in.rules {
		if in.rules[i].ID == "stack.blessed" {
			in.rules[i].State = gatepolicy.Blocking
		}
	}
	if r := evaluate(in); len(r.BlockedBy) != 0 || !r.Rules[2].Unset || !r.Rules[2].Blocks {
		t.Fatalf("a blocking rule with nothing set: %s", mustJSON(r))
	}
}

// Where sign-in is off no person signs in to own an app, so owner.required asks for what nobody there can
// do: it is neutral on its one line, and blocks nothing at any state (contracts/routes.md).
func TestOwnerRequiredIsNeutralWhereSignInIsOff(t *testing.T) {
	anonymous := actor{Kind: "person", Subject: "anonymous"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, actor: anonymous, signInOff: true,
		run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_8f2a"}}
	r := evaluate(in) // owner.required at its default, Advisory
	owner := r.Rules[3]
	if owner.ID != "owner.required" || !owner.Unset || owner.Passed || owner.Why != "no owner needed: sign-in is off on this Canyon" || slices.Contains(r.BlockedBy, "owner.required") {
		t.Fatalf("owner.required with sign-in off: %s", mustJSON(r))
	}
	in.owners = []actor{{Kind: "person", Subject: "anonymous", Display: "Dana Whitfield"}} // claimed all the same: owned
	if owner := evaluate(in).Rules[3]; owner.Unset || !owner.Passed || owner.Why != "owned by Dana Whitfield" {
		t.Errorf("a claimed app with sign-in off: %s", mustJSON(owner))
	}
	in.owners = nil
	for i := range in.rules { // an org that turned the rule Blocking asked for an owner: the block stands, sign-in or not
		if in.rules[i].ID == "owner.required" {
			in.rules[i].State = gatepolicy.Blocking
		}
	}
	if r := evaluate(in); r.Rules[3].Unset || !slices.Contains(r.BlockedBy, "owner.required") {
		t.Errorf("owner.required turned Blocking with sign-in off: %s", mustJSON(r))
	}
	in.signInOff = false // where people sign in, an app nobody claimed is a missing owner
	if owner := evaluate(in).Rules[3]; owner.Unset || owner.Why != "no owner has claimed this app" {
		t.Errorf("owner.required with sign-in on: %s", mustJSON(owner))
	}
}

// With nothing in preview the receipt is not evaluated: it waits, with no rules — an app live in
// production with an empty preview is failing nothing (contracts/routes.md, canyonv2-5r11.20).
func TestNothingInPreviewIsAWaitingReceipt(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), owners: []actor{dana}, actor: dana,
		run: &runFacts{id: "gr_8f2a", state: "clean", pkg: pkgRef{id: "pkg_9e40", label: "v1.4"}, newest: "gr_8f2a"}}
	if got, want := string(mustJSON(evaluate(in))), `{"org":"Meridian","blocked_by":[],"rules":[],"waiting":"Deploy a new version to preview to promote it."}`; got != want {
		t.Fatalf("nothing in preview:\n got %s\nwant %s", got, want)
	}
	// Why nothing is there, when the run says: deploying again would only repeat the block or the failure.
	for _, c := range []struct {
		state, reason string
		unapproved    int
		want          string
	}{
		{"blocked", "", 1, "Nothing is in preview: v1.4 was blocked at review. Fix the finding, or have an admin or a security responder who didn't start this review approve it with a reason, then deploy."},
		{"blocked", "", 2, "Nothing is in preview: v1.4 was blocked at review. Fix the findings, or have an admin or a security responder who didn't start this review approve them with a reason, then deploy."},
		{"blocked", "", 0, waitingOnPreview}, // every blocker approved: deploying again goes ahead
		{"failed", "build", 0, "Nothing is in preview: the build of v1.4 failed."},
		{"failed", "start", 0, "Nothing is in preview: v1.4 didn't stay up after it started."},
		{"failed", "detect", 0, "Nothing is in preview: this host can't build v1.4's kind of app."},
		{"failed", "scan_no_room", 0, "Nothing is in preview: the cluster had no room to review v1.4. Deploy it again once it has."},
		{"failed", "error", 0, waitingOnPreview},
	} {
		in.run.state, in.run.failureReason, in.unapproved = c.state, c.reason, c.unapproved
		if got := evaluate(in); got.Waiting != c.want || len(got.Rules) != 0 {
			t.Errorf("%s/%s, %d unapproved: %q, want %q", c.state, c.reason, c.unapproved, got.Waiting, c.want)
		}
	}
}

// A preview Canyon runs that isn't running waits as an empty one does: no promotion is made from it,
// and the line says which of the two it is (contracts/routes.md, The promote receipt, #1557).
func TestAPreviewThatIsNotRunningIsAWaitingReceipt(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana,
		run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_8f2a"}}
	for state, want := range map[string]string{
		"restarting": "v1.4 restarted since it was deployed; deploy again once it's fixed (the build is reused).", // no count read
		"stopped":    "v1.4 isn't running in preview: canyon logs says why.",
		"unknown":    "Canyon couldn't check whether v1.4 runs in preview.",
		"paused":     "Canyon couldn't check whether v1.4 runs in preview.", // a state this server does not know fails closed
	} {
		in.previewState = state
		if got := evaluate(in); got.Waiting != want || len(got.Rules) != 0 || len(got.BlockedBy) != 0 {
			t.Errorf("%s: %s", state, mustJSON(got))
		}
	}
	in.previewState = "restarting"
	for n, want := range map[int]string{1: "v1.4 restarted once since it was deployed; deploy again once it's fixed (the build is reused).", 7: "v1.4 restarted 7 times since it was deployed; deploy again once it's fixed (the build is reused)."} {
		in.restarts = n
		if got := evaluate(in); got.Waiting != want {
			t.Errorf("restarted %d times: %q", n, got.Waiting)
		}
	}
	in.restarts = 0
	for _, state := range []string{"running", ""} { // "": the org runs it, or nobody asked
		in.previewState = state
		if got := evaluate(in); got.Waiting != "" || len(got.Rules) != 4 {
			t.Errorf("%q: %s", state, mustJSON(got))
		}
	}
}

// A rule placed before production follows the four, with its own line (contracts/routes.md, The
// promote receipt): blocking and an unapproved finding refuses; every finding approved, or none, passes;
// advisory blocks nothing; and it fails closed — a run with no verdict, of another package than preview
// runs or not its newest says what clean-gate says, never a count read from another version's review; a
// rule that was not on when the run started, or whose AI review was skipped — no model, or out of room —
// was never looked at.
func TestPolicyPlacedRuleIsReadFromTheRun(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	auth := gatepolicy.Rule{ID: "auth.unauthenticated-route", Name: "All app routes behind SSO", State: gatepolicy.Advisory, Production: gatepolicy.Blocking}
	for _, c := range []struct {
		name  string
		state string
		run   runFacts // its id, package and newest run, when not the newest of preview's
		p     placedRule
		row   string
		block bool
	}{
		{"another package's run", "clean", runFacts{id: "gr_7a00", pkg: pkgRef{id: "pkg_7a1c", label: "v1.3"}, newest: "gr_7a00"}, placedRule{rule: auth, journaled: true}, `"blocks":true,"passed":false,"why":"the review named is v1.3's; preview runs v1.4"`, true},
		{"an older run of preview's package", "clean", runFacts{id: "gr_7a00", pkg: v14, newest: "gr_8f2a"}, placedRule{rule: auth, journaled: true}, `"blocks":true,"passed":false,"why":"v1.4 has a newer review than the one named"`, true},
		{"the AI review skipped", "clean", runFacts{}, placedRule{rule: auth, journaled: true, unread: "'s AI review did not run; check it again once a model is configured"}, `"blocks":true,"passed":false,"why":"v1.4's AI review did not run; check it again once a model is configured"`, true},
		{"the AI review out of room", "clean", runFacts{}, placedRule{rule: auth, journaled: true, unread: "'s AI review ran out of room for its answer; check it again"}, `"blocks":true,"passed":false,"why":"v1.4's AI review ran out of room for its answer; check it again"`, true},
		{"advisory, the AI review out of room", "clean", runFacts{}, placedRule{rule: gatepolicy.Rule{ID: auth.ID, Name: auth.Name, Production: gatepolicy.Advisory}, journaled: true, unread: "'s AI review ran out of room for its answer; check it again"}, `"blocks":false,"passed":false,"why":"v1.4's AI review ran out of room for its answer; check it again"`, false},
		{"the AI review over its source cap", "clean", runFacts{}, placedRule{rule: auth, journaled: true, unread: unreadWhy[gate.TooLarge]}, `"blocks":true,"passed":false,"why":"v1.4's AI review did not run: the source is larger than it accepts (16 MiB)"`, true},
		{"a review stored over the old 2 MiB", "clean", runFacts{}, placedRule{rule: auth, journaled: true, unread: unreadWhy[gate.TooLargeBefore]}, `"blocks":true,"passed":false,"why":"v1.4's AI review did not run: the source was over the 2 MiB it took then; check it again, as a larger app is now reviewed in parts"`, true},
		{"two unapproved", "blocked", runFacts{}, placedRule{rule: auth, journaled: true, blocking: 2, unapproved: 2}, `"blocks":true,"passed":false,"why":"v1.4's review found 2 blocking findings, not approved"`, true},
		{"every one approved", "clean", runFacts{}, placedRule{rule: auth, journaled: true, blocking: 2}, `"blocks":true,"passed":true,"why":"v1.4 passed review: every blocking finding approved"`, false},
		{"none", "clean", runFacts{}, placedRule{rule: auth, journaled: true}, `"blocks":true,"passed":true,"why":"v1.4 passed review: no blocking finding"`, false},
		{"not on when the run started", "clean", runFacts{}, placedRule{rule: auth}, `"blocks":true,"passed":false,"why":"v1.4 was reviewed before this check ran; check it again"`, true},
		{"no verdict yet", "running", runFacts{}, placedRule{rule: auth, journaled: true}, `"blocks":true,"passed":false,"why":"v1.4's review is running"`, true},
		{"advisory", "blocked", runFacts{}, placedRule{rule: gatepolicy.Rule{ID: auth.ID, Name: auth.Name, Production: gatepolicy.Advisory}}, `"blocks":false,"passed":false,"why":"v1.4 was reviewed before this check ran; check it again"`, false},
	} {
		in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana,
			run: &runFacts{id: "gr_8f2a", state: c.state, pkg: v14, newest: "gr_8f2a"}, placed: []placedRule{c.p}}
		if c.run.id != "" {
			c.run.state = c.state
			in.run = &c.run
		}
		r := evaluate(in)
		got := string(mustJSON(r.Rules[len(r.Rules)-1]))
		if want := `{"id":"auth.unauthenticated-route","name":"All app routes behind SSO",` + c.row + `}`; len(r.Rules) != 5 || got != want || slices.Contains(r.BlockedBy, auth.ID) != c.block {
			t.Errorf("%s:\n got %s %v\nwant %s", c.name, got, r.BlockedBy, want)
		}
	}
}

// prod.clean-gate fails on what its placed rules block on before production (contracts/routes.md, The
// promote receipt, #1559): a rule advisory before preview and blocking before production that found
// something fails it too, so it never reads "passed review: no blocking finding" above the rule's own
// refusal. It points at the rule's row and never counts its findings again, which read as two (#2723),
// whether the row counts findings or fails with no count — the review ran before the rule, or its AI
// review was skipped; an approved one, or one advisory before production, leaves it as it was; and the
// run's own checks come first, so a count is never read from another version's review.
func TestCleanGateCountsWhatBlocksBeforeProduction(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	sql := gatepolicy.Rule{ID: "sql.hand-built", Name: "No hand-built SQL from user input", State: gatepolicy.Advisory, Production: gatepolicy.Blocking}
	auth := gatepolicy.Rule{ID: "auth.unauthenticated-route", Name: "All app routes behind SSO", State: gatepolicy.Advisory, Production: gatepolicy.Blocking}
	advisory := gatepolicy.Rule{ID: auth.ID, Name: auth.Name, State: gatepolicy.Advisory, Production: gatepolicy.Advisory}
	for _, c := range []struct {
		name       string
		run        runFacts
		unapproved int // before preview
		placed     []placedRule
		gate       string
		by         string
	}{
		{"the test cluster's case-sqli-blocked", runFacts{state: "clean"}, 0, []placedRule{{rule: sql, journaled: true, blocking: 1, unapproved: 1}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate sql.hand-built]"},
		{"two rules, neither counted again", runFacts{state: "blocked"}, 0, []placedRule{{rule: sql, journaled: true, blocking: 1, unapproved: 1}, {rule: auth, journaled: true, blocking: 2, unapproved: 2}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate sql.hand-built auth.unauthenticated-route]"},
		{"every one approved", runFacts{state: "clean"}, 0, []placedRule{{rule: sql, journaled: true, blocking: 1}},
			`"passed":true,"why":"v1.4 passed review: no blocking finding"`, "[]"},
		{"advisory before production", runFacts{state: "clean"}, 0, []placedRule{{rule: advisory, journaled: true}},
			`"passed":true,"why":"v1.4 passed review: no blocking finding"`, "[]"},
		{"blocking before preview comes first", runFacts{state: "blocked"}, 1, []placedRule{{rule: sql, journaled: true, blocking: 1, unapproved: 1}},
			`"passed":false,"why":"v1.4's review found 1 blocking finding, not approved"`, "[prod.clean-gate sql.hand-built]"},
		{"another package's run", runFacts{id: "gr_7a00", state: "clean", pkg: pkgRef{id: "pkg_7a1c", label: "v1.3"}, newest: "gr_7a00"}, 0, []placedRule{{rule: sql, journaled: true, blocking: 1, unapproved: 1}},
			`"passed":false,"why":"the review named is v1.3's; preview runs v1.4"`, "[prod.clean-gate sql.hand-built]"},
		{"reviewed before the rule ran", runFacts{state: "clean"}, 0, []placedRule{{rule: sql}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate sql.hand-built]"},
		{"its AI review skipped", runFacts{state: "clean"}, 0, []placedRule{{rule: auth, journaled: true, unread: "'s AI review did not run; check it again once a model is configured"}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate auth.unauthenticated-route]"},
		{"a stale count on a row reviewed before the rule ran", runFacts{state: "clean"}, 0, []placedRule{{rule: sql, blocking: 1, unapproved: 1}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate sql.hand-built]"},
		{"a stale count on a row whose AI review was skipped", runFacts{state: "clean"}, 0, []placedRule{{rule: auth, journaled: true, unread: "'s AI review did not run; check it again once a model is configured", blocking: 1, unapproved: 1}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate auth.unauthenticated-route]"},
		{"a counted row and an uncounted one", runFacts{state: "clean"}, 0, []placedRule{{rule: sql}, {rule: auth, journaled: true, blocking: 1, unapproved: 1}},
			`"passed":false,"why":"v1.4's review isn't clean for production. Fix its findings that block production, then deploy to preview again."`, "[prod.clean-gate sql.hand-built auth.unauthenticated-route]"},
		{"advisory before production, its AI review skipped", runFacts{state: "clean"}, 0, []placedRule{{rule: advisory, journaled: true, unread: "'s AI review ran out of room for its answer; check it again"}},
			`"passed":true,"why":"v1.4 passed review: no blocking finding"`, "[]"},
	} {
		run := &runFacts{id: "gr_8f2a", state: c.run.state, pkg: v14, newest: "gr_8f2a"}
		if c.run.id != "" {
			run = &c.run
		}
		in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana, run: run, unapproved: c.unapproved, placed: c.placed}
		r := evaluate(in)
		got := string(mustJSON(r.Rules[0]))
		if want := `{"id":"prod.clean-gate","name":"Production needs a passing review","blocks":true,` + c.gate + `}`; got != want || fmt.Sprint(r.BlockedBy) != c.by {
			t.Errorf("%s:\n got %s %v\nwant %s %s", c.name, got, r.BlockedBy, want, c.by)
		}
	}
}

// The promote receipt counts no result of checkov's dockerfile checks among iac.misconfig's blocking
// findings: Canyon builds from no Dockerfile, so they never block (#1881).
func TestTheReceiptCountsNoDockerfileCheck(t *testing.T) {
	var results []finding.Result
	for _, id := range []string{"iac.misconfig.checkov.CKV_DOCKER_2", "iac.misconfig.checkov.CKV2_DOCKER_1", "iac.misconfig.checkov.CKV_AWS_24"} {
		r := finding.Result{RuleID: id}
		r.Properties.Canyon.Severity = finding.High
		results = append(results, r)
	}
	if _, n := blockingUnder(finding.Log{Runs: []finding.Run{{Results: results}}}, "iac.misconfig"); n != 1 {
		t.Fatalf("%d blocking under iac.misconfig, want the Terraform result alone", n)
	}
}

// Every note of a review that read nothing has its why on a placed row: one with none would read the
// version alone (#2280).
func TestEveryUnreadNoteHasAWhy(t *testing.T) {
	for _, note := range gate.UnreadNotes {
		if unreadWhy[note] == "" {
			t.Errorf("no why for %q", note)
		}
	}
}
