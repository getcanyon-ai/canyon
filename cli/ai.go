package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	aiRuleWidth      = 78
	aiNotPackedShown = 20 // an agent reads a longer list than a person scans
	aiLastLines      = 20 // the end of a failed build's log, where its error is: never the whole log
	aiTailLines      = 2  // after an npm window, the build's own last lines: the lifecycle's and Canyon's
)

// Canyon's own multi-line copy keeps hand-authored line breaks, never wrap(): no line of the ai
// output begins with `blocks:` unless it is a finding's own (TestAIHostProseCannotForgeABlocksLine).
const (
	aiBlockedOutcome = "outcome: nothing was built and nothing was deployed.\n" +
		"Every finding with blocks: yes must be fixed before this app can deploy.\n"
	aiTellFirst = "NEXT STEPS — do these in order:\n" + // an agent went from the block straight to the edit; told to say what blocked first, it read the file and said nothing (#2694)
		"1. Before your next tool call, tell the user this, in your own words if you like:\n" +
		"   %s\n%s"
	aiBlockedSteps = "2. Fix each finding with `blocks: yes` that a change to the code can fix. For\n" + // a could-not-check line is no code's to fix (contracts/finding.md, gate.unreadable)
		"   a line in step 1 that starts `could not check`, do what its suggested fix says.\n" +
		"   Findings with `blocks: no` do not block; mention them to the user but do not\n" +
		"   fix them unless the user asks.\n" +
		"3. %s\n" + // a check's runs it again; a check that said Run: canyon check named itself as if new (#2890)
		"   This runs the identical review without deploying. It takes about a minute.\n" +
		"4. If it prints RESULT: PASSED, run: canyon deploy\n" +
		"5. Report the \"live url\" from the deploy output to the user.\n" +
		"\n" +
		"If canyon check still prints RESULT: BLOCKED after two full rounds of fixes,\n" +
		"stop and show the user the remaining findings. Do not keep looping.\n"
	aiApproveStep = "\n" + // a block is the org's to lift as well as the code's to fix, never its author's (design/PERMISSIONS.md; routes.md, the approve rows)
		"Fixing is not the only way through. An admin or a security responder can\n" +
		"approve a blocking finding with a reason on the app's page in Canyon:\n" +
		"  %s\n%s" +
		"Nobody approves a blocking finding of a review they started, so neither you\n" +
		"nor the user can approve these. If the user wants to ship without the fix,\n" +
		"tell them to ask an admin or a security responder to approve it. That ends\n" +
		"the fix loop above: stop, and do not run canyon check or canyon deploy again,\n" +
		"or look for another way through, until the user says it is approved. Never\n" +
		"say the finding cannot be skipped.\n"
	aiRotateStep = "\n" + // names the places: an agent piping the output through tail never sees the findings (cli.md §5.2)
		"A blocking finding is a secret that was in the code, so it has leaked: %s.\n" +
		"Before the next deploy, ask the user to revoke it. If nothing reads it, delete it\n" +
		"from the code; otherwise make a new value and store it with canyon secret set NAME,\n" +
		"NAME being the variable the code reads.\n"
	aiRollbackRotateStep = "\n" + // a rollback's: the deletion is a new version's, never the one put back (its step 2)
		"A blocking finding is a secret that was in the code, so it has leaked: %s.\n" +
		"Ask the user to revoke it. If nothing reads it, delete it from the code in a new\n" +
		"version shipped with canyon deploy, never in %s; otherwise make a new value and\n" +
		"store it with canyon secret set NAME, NAME being the variable the code reads.\n"
	aiShippedOutcome = "outcome: this app is deployed and serving traffic. Every blocking finding carries an\n" +
		"approval, which is the only way a blocked run deploys.\n"
	aiShippedSteps = "NEXT STEPS:\n" +
		"1. Report the live url to the user, and tell them the review blocked and an\n" +
		"   approval let it ship anyway.\n" +
		"2. Do not change this code for these findings on your own. The approval is\n" +
		"   the org's decision and the app is already live.\n" +
		"3. If the user asks for the findings to be fixed, fix them and run: canyon check\n" +
		"\n" +
		"This output replaces the fix loop in your Canyon skill for this run.\n"
	aiReportStep      = "Report the live url to the user and say who can open it (the access line). Do not share it unless the user asks."
	aiApprovedOutcome = "outcome: nothing was deployed. Every blocking finding carries an approval, so canyon deploy\n" +
		"ships this code.\n"
	aiApprovedSteps = "NEXT STEPS:\n" +
		"1. Run: canyon deploy\n" +
		"2. Do not change this code for these findings on your own. The approval is\n" +
		"   the org's decision.\n"
	aiCheckedOutcome = "outcome: nothing was deployed. canyon check reviews the package; canyon deploy ships it.\n" +
		"There is no live url in this output.\n"
	aiCheckedSteps = "NEXT STEPS:\n" +
		"1. Run: canyon deploy\n" +
		"2. Report the \"live url\" from the deploy output to the user.\n"
	aiFailedOutcome = "outcome: nothing was deployed.\n"
	aiBuildSteps    = "NEXT STEPS:\n" +
		"1. Read the last lines above: they are the build's own and say what failed. Fix the app's code if the error is in it; if the build lacked a library or tool, tell the user, since the host may have to add it. It is not a policy block.\n" +
		"2. Run: canyon deploy\n"
	aiStartSteps = "NEXT STEPS:\n" +
		"1. Fix why the app stopped (the lines above), or set a value it needs with canyon secret set.\n" +
		"2. Run: canyon deploy\n"
	aiBuildQuietSteps = "NEXT STEPS:\n" +
		"1. The build failed without output: run canyon deploy again in a terminal to see the log, then fix it.\n" +
		"2. Run: canyon deploy\n"
	aiStartQuietSteps = "NEXT STEPS:\n" +
		"1. The app stopped without output: run canyon deploy again in a terminal to see its log, then fix it, or set a value it needs with canyon secret set.\n" +
		"2. Run: canyon deploy\n"
	aiAgainSteps = "NEXT STEPS:\n" +
		"1. Run the same command again.\n"
	aiNoRoomSteps = "NEXT STEPS:\n" +
		"1. Stop. Tell the user the cluster had no room to build the app (the last line above is the scheduler's): an admin adds nodes, or lowers what apps request (CANYON_KUBE_APP_CPU_REQUEST).\n" +
		"2. Do not change the app's code for this. Once the admin has, run: canyon deploy\n"
	aiCanyonFailedSteps = "NEXT STEPS:\n" +
		"1. Stop. Tell the user Canyon itself failed, and quote the reason line and any last lines above.\n" +
		"2. Do not change the app's code for this. The failure is Canyon's, not the app's.\n"
)

const (
	aiAdvisory    = "This does not block. Mention it to the user; fix only if asked."
	aiOwnDeps     = "This is in the app's own dependencies. Canyon fixes the system libraries under the app, never these." // a deps.known-vuln advisory's (design/PATCHING.md §7)
	aiNotRequired = "Not required. This severity does not block deployment."
	aiUnsaid      = "(the host did not say)" // a blocking finding without its prose: K5 makes the host refuse it
	aiSameFinding = "Each of these produces the same finding on the next run."
	// aiCarried is a finding an earlier review made, carried while its code is unchanged (contracts/finding.md; #2900).
	aiCarried = "This was found in an earlier review and this review didn't report it.\n" +
		"The code hasn't changed, so it still blocks: change the code so the rule\n" +
		"holds, or ask for an approval. Don't wait for another review to drop it."
	aiCoverage = "These are the host's notes on its own review. Mention them to the user.\n"
)

// aiVerdict prints §5.2's block for a blocked run and §5.3's for a passed one; code is what
// outcome decided, printed as text because a harness may show an agent stdout and not the status.
func (a *app) aiVerdict(run checkRun, code int) int {
	res := run.Result
	result := "PASSED"
	switch {
	case approved(res): // approved is not clean (cli.md §5.2)
		result = "APPROVED"
	case res.State == "blocked":
		result = "BLOCKED"
	}
	a.aiHead(run, result)
	var recap []string
	if a.shipped(run) {
		recap = aiLive(run)
	}
	if res.State == "clean" {
		return a.aiEnd(a.aiPassed(run), result, recap, code)
	}
	var blocking, advisory []finding
	for _, f := range res.Findings {
		if blocks, _ := f.Blocking(); blocks {
			blocking = append(blocking, f)
		} else {
			advisory = append(advisory, f)
		}
	}
	// a package's CVEs are one entry, and so is one Canyon rule on one line, blocking and advisory apart (cli.md §5.2)
	blockingEntries := byLine(cveGroups(blocking))
	entries := slices.Concat(blockingEntries, byLine(cveGroups(advisory)))
	a.aiGateRun(run)
	a.printf("findings: %d · blocking: %d\n", len(entries), len(blockingEntries)) // the entries the list shows, as it numbers them
	for _, add := range run.AppRules {
		a.printf("app rule: %s (%s) blocks here because this app holds it stricter than the org; set by %s.\n", cell(add.Name), cell(add.ID), setter(add.SetBy))
	}
	a.aiPackage(run)
	var steps string
	switch {
	case a.shipped(run): // routes.md row 62: every blocking finding approved, so the fix loop is not the agent's to run
		a.printf(aiShippedOutcome)
		a.aiURLs(run)
		steps = aiShippedSteps
	case approved(res): // a check of approved code: the next step is the deploy, never a fix
		a.printf(aiApprovedOutcome)
		steps = aiApprovedSteps
	case run.Rollback != nil: // a version that shipped before: never the agent's to change (design/OPERATIONS.md §1.7)
		a.printf("outcome: nothing was built and nothing was deployed.\n")
		steps = run.Rollback.aiSteps()
	default:
		a.printf(aiBlockedOutcome)
		steps = a.tellFirst(blockingEntries) + a.blockedSteps()
	}
	a.aiNotes(run)
	if !a.shipped(run) && !approved(res) { // the blocking lines the steps act on
		var leaked []string
		for _, f := range blocking {
			if place := placeOf(f); f.Properties.Canyon.SecretName != nil && !slices.Contains(leaked, place) {
				leaked = append(leaked, place)
			}
		}
		if run.Rollback == nil {
			steps += fmt.Sprintf(aiApproveStep, cell(run.Host+"/#/apps/"+run.App), together(blocking))
		}
		switch {
		case len(leaked) > 0 && run.Rollback != nil: // a version put back's leaked as much as any
			steps += fmt.Sprintf(aiRollbackRotateStep, strings.Join(leaked, ", "), run.Rollback.version)
		case len(leaked) > 0: // a fixed leak still leaked (#1675)
			steps += fmt.Sprintf(aiRotateStep, strings.Join(leaked, ", "))
		}
		var rules []string // one line a rule, its places beside it in the block's order: one a place repeated the id (#2890)
		places := map[string][]string{}
		for _, e := range blockingEntries {
			rule, f := e.named()
			if _, seen := places[rule]; !seen {
				rules = append(rules, rule)
			}
			places[rule] = append(places[rule], placeOf(f))
		}
		for _, rule := range rules {
			recap = append(recap, "blocking: "+cell(rule)+" · "+strings.Join(places[rule], ", "))
		}
	}
	for i, e := range entries {
		a.aiRule(fmt.Sprintf("FINDING %d of %d", i+1, len(entries)))
		if len(e.members) > 1 {
			a.aiGroup(e)
		} else {
			a.aiFinding(e)
		}
	}
	a.aiRule("END OF FINDINGS")
	return a.aiEnd(steps, result, recap, code)
}

// blockedSteps is a blocked block's steps after the first: a check runs the check again, a deploy names it.
func (a *app) blockedSteps() string {
	run := "Run: canyon check"
	if a.verb.name == "check" {
		run = "Run canyon check again"
	}
	return fmt.Sprintf(aiBlockedSteps, run)
}

// tellFirst is a blocked block's first step: what the agent tells the user before anything else, one line
// per blocking entry, written out so the agent relays it rather than composes it (#2694).
func (a *app) tellFirst(blocking []*cveGroup) string {
	head := "Canyon blocked this deploy:"
	if a.verb.name == "check" { // a check deploys nothing
		head = "Canyon's check blocks this code from deploying:"
	}
	var lines string
	for _, e := range blocking {
		lines += "   - " + e.told() + "\n"
	}
	return fmt.Sprintf(aiTellFirst, head, lines)
}

// told is an entry's line in that step: its title, where it is and the first line of its suggested fix. A
// finding no line of the code holds — gate.unreadable, or a custom rule checked by code, each at the
// package's first file, line 1, as a placeholder (contracts/finding.md) — says its problem instead of a
// title and a place. Which it is is its rule's and its run's, never read from its line.
func (e *cveGroup) told() string {
	rule, f := e.named()
	members := []finding{e.first}
	if e.tools != nil {
		members = e.tools
	}
	first := func(text string) string { line, _, _ := strings.Cut(text, "\n"); return cell(line) }
	fix := ". Suggested fix: " + cmp.Or(first(members[0].Properties.Canyon.Fix), aiUnsaid)
	switch {
	case rule == "gate.unreadable" || strings.HasPrefix(rule, "custom.") && f.Tool == "canyon/scanners":
		return strings.TrimSuffix(first(f.Properties.Canyon.Problem), ".") + fix
	case len(e.members) > 1: // a package's CVEs: the group's line, as its title: line reads
		return cell(e.summary()) + " (" + placeOf(f) + ")" + fix
	}
	return cell(f.Properties.Canyon.Title) + " (" + placeOf(f) + ")" + fix
}

// aiEnd is every block's last lines: its steps, then its result again with the lines the steps act on,
// as the block printed them higher up, so an agent that reads only the output's tail has both (cli.md §5.2).
func (a *app) aiEnd(steps, result string, recap []string, code int) int {
	a.printf("\n%s\nCANYON REVIEW — RESULT: %s\n", steps, result)
	for _, line := range recap {
		a.printf("%s\n", line)
	}
	a.printf("\nexit code: %d\n", code)
	return code
}

// aiLive is a deploy's lines for its recap: the live url and, when the head said it, who can open it.
func aiLive(run checkRun) []string {
	live := []string{"live url: " + cell(run.Deploy.URL)}
	if run.Team != nil {
		live = append(live, "access: "+accessOf(run))
	}
	return live
}

// placeOf is where a finding is, as its entry's file: and line: name it.
// window is what LAST LINES shows of a failed run's lines: the last aiLastLines, unless npm wrote an error before
// them. npm writes each line at its level, `npm error` (`npm ERR!` before npm 10), and its cause first, then its
// usage and notices (#2950): then it is aiLastLines from npm's first error, how many lines it leaves out, and the
// build's last aiTailLines.
func window(lines []string) (shown []string, left int, tail []string) {
	from := slices.IndexFunc(lines, func(line string) bool {
		line = strings.TrimLeft(ansi.Strip(line), " \t")
		return strings.HasPrefix(line, "npm error") || strings.HasPrefix(line, "npm ERR!")
	})
	switch {
	case from < 0 || from >= len(lines)-aiLastLines:
		return lines[max(0, len(lines)-aiLastLines):], 0, nil
	case len(lines)-from <= aiLastLines+aiTailLines:
		return lines[from:], 0, nil
	}
	return lines[from : from+aiLastLines], len(lines) - aiTailLines - from - aiLastLines, lines[len(lines)-aiTailLines:]
}

// pinned is whether a build failed on a version the app pins and the build doesn't offer, in the words of packit's
// dependency resolver (paketo-buildpacks/packit, postal), the only signal a build gives for it: `failed to satisfy
// "<dep>" dependency version constraint "<pin>": … Supported versions are: [...]`. what is `a version of <dep>`, ""
// when the line names none.
func pinned(lines []string) (what string, ok bool) {
	for _, line := range lines {
		line = ansi.Strip(line)
		if !strings.Contains(line, " dependency version constraint ") || !strings.Contains(line, "Supported versions are: [") {
			continue
		}
		if _, rest, found := strings.Cut(line, `failed to satisfy "`); found {
			if dep, _, closed := strings.Cut(rest, `"`); closed && dep != "" {
				return "a version of " + cell(dep), true
			}
		}
		return "", true
	}
	return "", false
}

func placeOf(f finding) string {
	where := f.Locations[0].PhysicalLocation
	return fmt.Sprintf("%s:%d", cell(where.ArtifactLocation.URI), where.Region.StartLine)
}

// aiFailed is §5.2's block for a run that ended failed: the reason in plain words, the last lines
// the run streamed whatever the reason (a failure that is Canyon's says why there too), and the
// reason's steps, which point above only when there are lines to point at.
func (a *app) aiFailed(run checkRun) int {
	reason := run.Result.FailureReason
	a.aiHead(run, "FAILED")
	a.aiGateRun(run)
	a.aiPackage(run)
	var blocking, warnings []finding
	for _, f := range run.Result.Findings {
		if blocks, _ := f.Blocking(); blocks {
			blocking = append(blocking, f)
		} else {
			warnings = append(warnings, f)
		}
	}
	if reviewedFirst(reason) { // the agent reads that the code passed, so it fixes what stopped the build or the app, not a finding
		res, said := run.Result, "passed"
		switch {
		case res.Blocking > 0:
			said = "approved"
		case res.NotChecked: // never "passed" while the review names what it did not read (contracts/finding.md)
			said = "found nothing blocking in what was checked"
		}
		// entries, as the blocked block counts them and the advisories below are numbered (cli.md §5.2)
		a.printf("review: %s · %d blocking, %s\n", said, len(byLine(cveGroups(blocking))), plural(len(byLine(cveGroups(warnings))), "warning", "warnings"))
	}
	said := failureReason(reason, run.Result.FailedIn)
	if why, ok := failureReasons[reason]; ok {
		said = reason + " — " + why + inStep(run.Result.FailedIn)
	}
	recap := []string{"reason: " + cell(said)} // a reason this build does not know is the host's string
	a.printf("%s\n", recap[0])
	last := run.Build
	if reason == "start" { // the app's own lines and why it stopped: the build before them passed, and its chatter buries them
		last = run.Build[min(run.AppLines, len(run.Build)):]
	}
	quiet, steps := len(last) == 0, aiCanyonFailedSteps
	if fix := run.Result.Fix; fix != nil {
		a.printf("missing: %s\nwhy: %s\n", cell(fix.File), cell(fix.Why))
	}
	if reason == "detect" { // what the host builds, never the detector's lines: each is a buildpack declining the package (§9)
		recap = append(recap, "builds: "+cmp.Or(builds(run.Result), "(its builder doesn't say)"))
		a.printf("%s\n", recap[1])
		if len(run.NotPacked) > 0 { // its steps name the paths that stayed home
			recap = append(recap, "not packed: "+andMore(run.NotPacked, aiNotPackedShown))
		}
		quiet = true
	}
	a.printf("%s", aiFailedOutcome)
	if reviewedFirst(reason) { // the warnings the review line counts, never only their number
		a.aiAdvisories(warnings)
	}
	switch {
	case reason == "detect":
		steps = aiDetectSteps(run.Result.Errored, len(run.NotPacked) > 0)
	case reason == "build" && quiet:
		steps = aiBuildQuietSteps
	case reason == "build":
		steps = aiBuildSteps
		if dep, ok := pinned(last); ok {
			steps = "NEXT STEPS:\n1. The app pins " + cmp.Or(dep, "a version") + " that this build doesn't offer; the line above lists the ones it does. Change the pin to one of them. This is not a policy block.\n2. Run: canyon deploy\n"
		}
	case reason == "start" && quiet:
		steps = aiStartQuietSteps
	case reason == "start":
		steps = aiStartSteps
	case reason == "no_room":
		steps = aiNoRoomSteps
	case reason == "scan_no_room":
		steps = "NEXT STEPS:\n1. Tell the user the cluster had no room to run Canyon's checks (the last lines above are the scheduler's). Nothing was reviewed, so there is nothing to fix or approve.\n" +
			"2. Do not change the app's code for this. In a few minutes, run: canyon " + a.verb.name + "\n" +
			"3. If it fails the same way again, stop: tell the user an admin needs to add room to the cluster.\n"
	case reason == "image_gone": // a rollback never rebuilds: the last line says the way to a rebuilt version
		steps = "NEXT STEPS:\n1. Tell the user the last line above, in its own words. Production runs what it ran before.\n" +
			"2. Stop. Deploy that version to preview and promote it only if the user asks for it rebuilt.\n"
	case reason == "interrupted":
		steps = aiAgainSteps
	case run.Result.FailedIn != "": // the server logged why under the run's id: a retry, then an admin
		steps = "NEXT STEPS:\n1. Run the same command again.\n2. If it fails again, stop. Tell the user Canyon itself failed, quote the reason line, and say " +
			lookUp(run.Run) + "\n3. Do not change the app's code for this. The failure is Canyon's, not the app's.\n"
	}
	if !quiet {
		a.aiRule("LAST LINES")
		shown, left, tail := window(last)
		for _, line := range shown { // indented two like host prose: an app's line cannot start one of the block's own
			a.printf("  %s\n", cell(ansi.Strip(line)))
		}
		if left > 0 {
			a.printf("  (%d lines left out)\n", left)
		}
		for _, line := range tail {
			a.printf("  %s\n", cell(ansi.Strip(line)))
		}
	}
	if fix := run.Result.Fix; fix != nil && (reason == "detect" || reason == "start") {
		a.aiRule("ADD " + strings.ToUpper(cell(fix.File)))
		for _, e := range fix.Examples {
			a.printf("for %s:\n", cell(e.For))
			for _, line := range strings.Split(strings.TrimSuffix(e.Text, "\n"), "\n") {
				a.printf("%s\n", strings.TrimRight("  "+cell(line), " "))
			}
		}
		steps = "NEXT STEPS:\n1. Add a file named " + cell(fix.File) + " at the app's root holding the lines for this app from the ADD " + strings.ToUpper(cell(fix.File)) +
			" section above, each without its two-space indent.\n2. Run: canyon deploy\n"
	}
	return a.aiEnd(steps, "FAILED", recap, exitError)
}

// aiDetectSteps are a failed detect's: never "fix the error" — nothing was compiled — but a buildpack
// that errored while detecting may mean a malformed manifest, and a path that stayed home may be one a
// buildpack needed, so those say so and only a plain 20 with everything packed says the code is fine.
func aiDetectSteps(errored, notPacked bool) string {
	var steps []string
	if errored {
		steps = append(steps, "If the app is meant to be one of the stacks in the builds line, check that its manifest is valid (for example package.json), then run: canyon deploy")
	}
	if notPacked {
		steps = append(steps, "The not packed line names paths that stayed home: if a buildpack needs one of them to recognise the app, tell the user which.")
	}
	ask := "Then give the user this line as it is: " + askFor(languageUnrecognised)
	if errored || notPacked {
		steps = append(steps, "Otherwise tell the user this host can't build this kind of app, and name what it builds (the builds line). Do not change the app's code for this.", ask)
	} else {
		steps = append(steps, "Stop. Tell the user this host can't build this kind of app, and name what it builds (the builds line).", ask,
			"Do not change the app's code for this. Nothing was compiled, so there is no error in it to fix.")
	}
	return numbered(steps)
}

// numbered is a NEXT STEPS block: each step on its own line, numbered in order.
func numbered(steps []string) string {
	said := "NEXT STEPS:\n"
	for i, step := range steps {
		said += fmt.Sprintf("%d. %s\n", i+1, step)
	}
	return said
}

// aiHead is every block's first lines: the result, the app, where it went and what setup wrote.
func (a *app) aiHead(run checkRun, result string) {
	a.printf("CANYON REVIEW — RESULT: %s\n\napp: %s\n", result, cell(run.App))
	if a.shipped(run) {
		a.printf("env: %s\n", cell(run.Deploy.Env))
		if v, ok := reusedOf(run); ok { // why the build took no time (#1666)
			a.printf("build: reused the image built for %s\n", v)
		}
	} else if a.verb.name == "deploy" { // the target the deploy header names; a check has none
		a.printf("env: preview\n")
	} else if run.Rollback != nil {
		a.printf("env: prod\n")
	}
	if run.Team != nil {
		a.printf("access: %s\n", accessOf(run))
	}
	if a.wrote != "" { // §3.2's setup ran, and its human block did not print
		a.printf("wrote: %s\n", cell(a.wrote))
	}
	if a.moved != "" { // §3.6: the app moved under this copy, onto a push it holds
		a.printf("%s\n", a.moved)
	}
}

// accessOf is who can open the app (§5.3): private, everyone, or its people and groups; anyone who
// reaches the host where nobody signs in.
func accessOf(run checkRun) string {
	t := run.Team
	switch counts := t.counts(", "); {
	case run.SignInOff:
		return accessSignInOff
	case t.Everyone:
		return "everyone"
	case counts != "":
		return counts
	}
	return "private"
}

// aiGateRun names the host and the run, with when it started when the timeline said.
func (a *app) aiGateRun(run checkRun) {
	when := ""
	if run.StartedAt != "" {
		when = " (" + cell(run.StartedAt) + ")"
	}
	a.printf("host: %s\ngate run: %s%s\n", cell(run.Host), cell(run.Run), when)
}

// aiPassed is §5.3: the counts, the urls when something shipped, one advisory per warning with
// its severity, title and Canyon's constant under it; it returns the steps. A check has no url, and says so.
func (a *app) aiPassed(run checkRun) string {
	res := run.Result
	a.aiGateRun(run)                                                                              // a passing run is cited as any other (#1674)
	a.printf("findings: %d · blocking: %d\n", len(byLine(cveGroups(res.Findings))), res.Blocking) // the advisories' entries, as they are numbered
	a.aiPackage(run)
	steps := aiCheckedSteps
	if a.shipped(run) {
		a.aiURLs(run)
		steps = aiPassedSteps(run)
	} else {
		a.printf(aiCheckedOutcome)
	}
	a.aiNotes(run)
	a.aiAdvisories(res.Findings)
	return steps
}

// aiAdvisories is one entry per advisory: its severity, rule and place, its title, its problem and
// fix as --format json carries them, then Canyon's constant. A package's trivy CVEs are one entry, and
// so is one Canyon rule on one file and line, found by several tools' rules.
func (a *app) aiAdvisories(findings []finding) {
	entries := byLine(cveGroups(findings))
	for i, e := range entries {
		f := e.first
		where := f.Locations[0].PhysicalLocation
		at := fmt.Sprintf(" at %s:%d", cell(where.ArtifactLocation.URI), where.Region.StartLine)
		switch {
		case e.tools != nil:
			a.aiLine(i, len(entries), e)
		case len(e.cves) > 1: // a group is trivy's results by construction (cveGroups)
			a.printf("\nadvisory %d of %d (%s): %s%s\n  %s\n  found by: trivy\n  %s\n", i+1, len(entries), cell(e.severity), cell(e.rule), at, cell(e.summary()), advised(e))
		default:
			a.printf("\nadvisory %d of %d (%s): %s%s\n  %s\n", i+1, len(entries), cell(e.severity), cell(f.RuleID), at, cell(f.Properties.Canyon.Title))
			a.aiField("problem", f.Properties.Canyon.Problem)
			a.aiField("fix", f.Properties.Canyon.Fix)
			a.printf("  %s\n", advised(e))
		}
	}
}

// advised is an advisory's closing words: a dependency's says first that it is the app's own, which Canyon's fix of
// the system libraries under the app never touches (design/PATCHING.md §7).
func advised(e *cveGroup) string {
	if rule, _ := e.named(); rule == "deps.known-vuln" {
		return aiOwnDeps + "\n  " + aiAdvisory
	}
	return aiAdvisory
}

// aiLine is a tool's advisory (cli.md §5.3): the Canyon rule its result is mapped to at the highest
// severity of the tools' rules that found it on that file and line, the title and problem of the first
// finding at it, each distinct fix, and every tool's rule under found by.
func (a *app) aiLine(i, n int, e *cveGroup) {
	top := e.top()
	where := top.Locations[0].PhysicalLocation
	head := fmt.Sprintf(" at %s:%d", cell(where.ArtifactLocation.URI), where.Region.StartLine)
	rule, _, _ := toolRule(top) // sarif.<tool> when no Canyon policy covers it, never the tool's rule as if it were Canyon's: that is under found by (#2950)
	head = cell(rule) + head + "\n  " + cell(top.Properties.Canyon.Title)
	a.printf("\nadvisory %d of %d (%s): %s\n", i+1, n, cell(e.severity), head)
	a.aiField("problem", top.Properties.Canyon.Problem)
	for _, fix := range e.toolFixes() {
		a.aiField("fix", fix)
	}
	a.printf("  found by: %s\n  %s\n", e.foundBy(), advised(e))
}

// aiField is a host string after its label, its line breaks kept and each line after the first
// indented under it.
func (a *app) aiField(label, text string) {
	for i, line := range strings.Split(visible(text), "\n") {
		lead := "    "
		if i == 0 {
			lead = "  " + label + ": "
		}
		a.printf("%s\n", strings.TrimRight(lead+strings.ReplaceAll(line, "\t", `\t`), " "))
	}
}

// aiPassedSteps are a passed deploy's: an answer to say beside readiness first, never a footnote of the
// step to production, which names each rule the production checklist will refuse it by (#1833). The
// person's rule is the promotion's own question, said when it asks.
func aiPassedSteps(run checkRun) string {
	var steps []string
	if w := run.Deploy.Answered.beside(); w != "" {
		steps = append(steps, "Tell the user first: "+cell(run.Deploy.Env)+" "+w+"; check the app before promoting it.")
	}
	steps = append(steps, aiReportStep)
	var refusing []string
	if r := run.Prod; r != nil {
		for _, rule := range r.Rules {
			if slices.Contains(r.BlockedBy, rule.ID) && rule.ID != "prod.human-approval" {
				refusing = append(refusing, cell(rule.Name)+" ("+cell(rule.ID)+"): "+cell(rule.Why))
			}
		}
		// the promotion refuses an answer to say beside readiness, but where a person approves production
		if w := run.Deploy.Answered.beside(); w != "" && !slices.Contains(r.BlockedBy, "prod.human-approval") {
			refusing = append(refusing, cell(run.Deploy.Env)+" "+w)
		}
	}
	if len(refusing) > 0 {
		steps = append(steps, "Production will refuse this version: "+strings.Join(refusing, "; ")+". Tell the user before they ask for production.")
	} else {
		steps = append(steps, "To put this in production later: canyon deploy --env prod")
	}
	return numbered(steps)
}

// aiNotes is what each producer said about its own run, one line each and above the findings: a
// skipped review reaches the agent as it reaches a person (EXPERIENCE.md D10). A note at level note
// says how the review ran, not what it missed, and stays a person's. Nothing said, no block.
func (a *app) aiNotes(run checkRun) {
	notes := slices.DeleteFunc(slices.Clone(run.Notes), func(n producerNote) bool { return n.Aside || saidOnPackage(run.Dockerfile, n) })
	if len(notes) == 0 {
		return
	}
	a.printf("\nreview coverage:\n")
	for _, n := range notes {
		a.printf("  %s\n", cell(n.Text))
	}
	a.printf(aiCoverage)
}

// aiPackage is the Package row an agent never sees (§5.2): the paths that stayed home, so a
// build that then fails is not read as the code's fault, git's record alone no line; and a root
// Dockerfile, which the build never reads (#1243).
func (a *app) aiPackage(run checkRun) {
	if len(run.NotPacked) > 0 {
		a.printf("not packed: %s\n", andMore(run.NotPacked, aiNotPackedShown))
	}
	if run.Dockerfile {
		a.printf("dockerfile: %s\n", dockerfileNote) // no ask: nothing was refused, and an agent would relay it every run (#2800)
	}
}

func (a *app) aiURLs(run checkRun) {
	a.printf("live url: %s\n", cell(run.Deploy.URL))
	if w := run.Deploy.Answered; w != nil { // a fact for the agent to report, never a verdict (contracts/routes.md, envs.<env>.answered)
		said := strings.TrimPrefix(w.words(), "answered ")
		if w.Said != "" {
			said += " · " + w.Said
		}
		a.printf("answered: %s\n", cell(said))
	}
	a.printf("fleet url: %s\n", cell(run.Host+"/#/apps/"+run.App))
	for _, line := range run.Access {
		a.printf("%s\n", cell(line))
	}
	if said, why := run.Source.words(); said != "" {
		a.printf("%s\n", said)
		if why != "" {
			a.printf("%s\n", why)
		}
	}
}

func (a *app) aiRule(label string) {
	head := "--- " + label + " "
	a.printf("\n%s%s\n", head, strings.Repeat("-", aiRuleWidth-len(head)))
}

// aiFinding is one finding's entry, or one Canyon rule's on one file and line that several tools' rules
// found (cli.md §5.2): a tool's names the Canyon rule its result is mapped to, every tool's rule under
// found by, and reads the first finding at the highest severity, then each distinct end state, fix and
// do-not of them all once.
func (a *app) aiFinding(e *cveGroup) {
	rule, f := e.named()
	members := []finding{e.first}
	if e.tools != nil {
		members = e.tools
	}
	bag, where := f.Properties.Canyon, f.Locations[0].PhysicalLocation
	blocks, _ := f.Blocking()
	yes := "no"
	if blocks {
		yes = "yes"
	}
	a.printf("title: %s\nseverity: %s\nblocks: %s\nrule: %s\n", cell(bag.Title), cell(e.severity), yes, cell(rule))
	if e.tools != nil {
		a.printf("found by: %s\n", e.foundBy())
	}
	a.printf("file: %s\nline: %d\ncolumn: %d\n", cell(where.ArtifactLocation.URI), where.Region.StartLine, where.Region.StartColumn)
	lines := strings.Split(where.Region.Snippet.Text, "\n")
	a.printf("source: %s\n", cell(lines[0]))
	for _, line := range lines[1:] { // a span of several lines keeps them, under the label's width
		a.printf("        %s\n", cell(line))
	}
	if bag.SecretName != nil {
		a.printf("note: the secret value is redacted here. Canyon never prints it.\n")
	}
	a.aiProse("problem", bag.Problem)
	if bag.CarriedFrom != "" {
		a.aiProse("earlier review", aiCarried)
	}
	var ends, fixes, doNots []string
	for _, m := range members {
		c := m.Properties.Canyon
		if end := cmp.Or(c.RequiredEndState, aiUnsaid); !slices.Contains(ends, end) {
			ends = append(ends, end)
		}
		if !slices.Contains(fixes, c.Fix) {
			fixes = append(fixes, c.Fix)
		}
		for _, move := range c.DoNot {
			if !slices.Contains(doNots, move) {
				doNots = append(doNots, move)
			}
		}
	}
	if !blocks {
		ends = []string{aiNotRequired}
	}
	a.aiProse("required end state", strings.Join(ends, "\n"))
	a.aiProse("suggested fix", strings.Join(fixes, "\n"))
	if !blocks {
		return
	}
	a.printf("\ndo not:\n")
	if len(doNots) == 0 {
		a.printf("  %s\n", aiUnsaid)
		return
	}
	for _, move := range doNots { // one line each, verbatim: never re-flowed
		a.printf("  - %s\n", cell(move))
	}
	a.printf("  %s\n", aiSameFinding)
}

// aiGroup is a package's CVEs as one finding entry (cli.md §5.2): the group's line, highest severity and
// the first's place, a line per finding, then each host line of their required end states, fixes and
// do-nots once. Each finding stays its own: json carries each one whole.
func (a *app) aiGroup(g *cveGroup) {
	where := g.first.Locations[0].PhysicalLocation
	blocks, _ := g.first.Blocking()
	yes := "no"
	if blocks {
		yes = "yes"
	}
	a.printf("title: %s\nseverity: %s\nblocks: %s\nrule: %s\nfound by: trivy\nfile: %s\nline: %d\ncolumn: %d\nfindings: %d, one per CVE:\n",
		cell(g.summary()), cell(g.severity), yes, cell(g.rule), cell(where.ArtifactLocation.URI), where.Region.StartLine, where.Region.StartColumn, len(g.members))
	var ends, fixes, doNots []string
	for i, f := range g.members {
		bag := f.Properties.Canyon
		_, _, fixed, _ := trivyPackage(f)
		said := "no fixed version yet"
		if fixed != "" {
			said = "fixed in " + fixed
		}
		a.printf("  - %s (%s): %s · %s\n", cell(g.cves[i]), cell(bag.Severity), cell(bag.Title), cell(said))
		ends = once(ends, cmp.Or(bag.RequiredEndState, aiUnsaid))
		fixes = once(fixes, bag.Fix)
		if len(bag.DoNot) == 0 {
			doNots = once(doNots, aiUnsaid)
		}
		for _, move := range bag.DoNot {
			doNots = once(doNots, move)
		}
	}
	if !blocks {
		ends = []string{aiNotRequired}
	}
	a.aiProse("required end state", strings.Join(ends, "\n"))
	a.aiProse("suggested fix", strings.Join(fixes, "\n"))
	if !blocks {
		return
	}
	a.printf("\ndo not:\n")
	for _, move := range doNots {
		a.printf("  - %s\n", cell(move))
	}
	a.printf("  %s\n", aiSameFinding)
}

// once is lines with each line of text added that is not among them yet, in order.
func once(lines []string, text string) []string {
	for _, line := range strings.Split(text, "\n") {
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	return lines
}

// aiProse is a host string under its label, host line breaks kept and every line indented two.
func (a *app) aiProse(label, text string) {
	a.printf("\n%s:\n", label)
	for _, line := range strings.Split(visible(text), "\n") {
		a.printf("  %s\n", strings.ReplaceAll(line, "\t", `\t`)) // visible keeps a tab for a terminal; an agent's block carries none
	}
}

// together is the blocked steps' line naming each dependency whose unapproved blocking findings from one
// tool are two to 200 — what the app page approves together, grouped as it groups them by the run read's
// tool and dependency (cli.md §5.2) — or nothing when there is none.
func together(blocking []finding) string {
	type key struct{ tool, name, version string }
	var order []key
	n := map[key]int{}
	for _, f := range blocking {
		if f.Dependency == nil || f.Approval != nil {
			continue
		}
		k := key{f.Tool, f.Dependency.Name, f.Dependency.Version}
		if n[k] == 0 {
			order = append(order, k)
		}
		n[k]++
	}
	var named []string
	for _, k := range order {
		if n[k] >= 2 && n[k] <= 200 {
			named = append(named, fmt.Sprintf("%s %s (%d)", cell(k.name), cell(k.version), n[k]))
		}
	}
	if len(named) == 0 {
		return ""
	}
	return "Findings of one dependency can be approved together there, with one reason: " + strings.Join(named, ", ") + ".\n"
}
