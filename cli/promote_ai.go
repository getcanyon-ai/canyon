package cli

import (
	"slices"
	"strings"
)

// promoted is what a promotion learned, for §6.3's ai block: the app and host, the run it started, the
// version it shipped and the one it replaced, how it ended, who can open it and what production lacks.
type promoted struct {
	app, host, run    string
	version, replaced string
	live              *deployed // the deploy.completed row; nil when the promotion ended without one
	last              string    // the run's last line, its reason when nothing went live
	access            string
	missing           []string
	unchecked         string        // why production's secrets could not be read, "" when they were
	preview           string        // what preview answered, when it is an answer to say beside readiness (contracts/routes.md)
	putBack           string        // the version a rollback was asked to put back, as named; "" for preview's
	keeps             string        // the version a rollback took out of production that preview still runs; "" otherwise (#2787)
	advisory          []receiptRule // the receipt's rules that failed and block nothing, as canyon status says them (#2300)
	ask               string        // the step that has the agent ask the user, where nothing here can answer the question (#2875)
}

// again is the command that asks for this promotion once more.
func (p promoted) again() string {
	if p.putBack != "" {
		return "canyon " + againWith(p.putBack)
	}
	return "canyon deploy --env prod"
}

const (
	aiMissingStep  = "The names on the not set for production line are set for preview only. If the app needs one, ask the user to run canyon secret set NAME --env prod in their own terminal. Never ask for the value in the chat."
	aiAdvisoryStep = "Tell the user each advisory line in its own words: it did not stop production."
	aiRefusedSteps = "NEXT STEPS:\n" +
		"1. Tell the user the outcome line, in its own words.\n" +
		"2. Stop. Never look for a way around this refusal: do not run this command again for this version, call Canyon's API, or open the dashboard. " +
		"Production changes when the app is fixed and deployed again with canyon deploy, or when a person promotes it from the dashboard after checking it.\n"
	aiPromoteBlockedSteps = "NEXT STEPS:\n" +
		"1. Tell the user what stops production: each blocking line above, in its own words, and the fleet url, where the app's page says the same.\n" +
		"2. Production runs what it ran before.\n"
)

// aiAnswered is what the app answered at / as §5.3 prints it under ai: `400 Bad Request at / · <said>`.
func aiAnswered(w answered) string {
	said := strings.TrimPrefix(w.words(), "answered ")
	if w.Said != "" {
		said += " · " + w.Said
	}
	return cell(said)
}

// aiPromoteHead is every promotion block's first lines: the result, the app, prod and the host.
func (a *app) aiPromoteHead(result, app, host string) {
	a.unpark(false) // the block replaces the parked transcript (§6.3)
	a.printf("CANYON PROMOTE — RESULT: %s\n\napp: %s\nenv: prod\nhost: %s\n", result, cell(app), cell(host))
}

func (a *app) aiPromoted(p promoted) int {
	a.aiPromoteHead("PROMOTED", p.app, p.host)
	a.printf("run: %s\nversion: %s\nreplaced: %s\n", cell(p.run), cell(p.version), cell(p.replaced))
	if p.keeps != "" {
		a.printf("preview still runs: %s\n", cell(p.keeps))
	}
	a.printf("live url: %s\n", cell(p.live.URL))
	if w := p.live.Answered; w != nil {
		a.printf("answered: %s\n", aiAnswered(*w))
	}
	a.printf("fleet url: %s\n", cell(p.host+"/#/apps/"+p.app))
	if p.access != "" {
		a.printf("access: %s\n", p.access)
	}
	var steps []string
	switch w := p.live.Answered.beside(); { // first, never below the url it qualifies (#1833)
	case w != "":
		steps = append(steps, "Tell the user first: production "+w+", so the app may not work there. Check it.")
	case p.preview != "" && p.live.Answered == nil: // production said nothing of its own: preview's is all there is
		steps = append(steps, "Tell the user first: preview "+p.preview+" before this promotion. Check the app.")
	}
	steps = append(steps, aiReportStep)
	if p.keeps != "" {
		steps = append(steps, "Tell the user preview still runs "+cell(p.keeps)+", the version this rollback took out of production: the next canyon deploy --env prod would ship it again. Fix it in preview first, with canyon deploy.")
	}
	steps = append(steps, a.aiLacks(p)...)
	a.printf("\n%s\nexit code: %d\n", numbered(steps), exitOK)
	return exitOK
}

// aiLacks prints what production lacks and the advisories that stop nothing, and returns their steps.
func (a *app) aiLacks(p promoted) []string {
	var steps []string
	switch {
	case p.unchecked != "": // never read as nothing missing
		a.printf("not set for production: couldn't check: %s\n", p.unchecked)
		steps = append(steps, aiMissingStep)
	case len(p.missing) > 0:
		a.printf("not set for production: %s\n", strings.Join(p.missing, ", "))
		steps = append(steps, aiMissingStep)
	}
	for _, rule := range p.advisory {
		a.printf("advisory: %s (%s): %s\n", cell(rule.Name), cell(rule.ID), cell(rule.Why))
		a.aiPackages(rule)
	}
	if len(p.advisory) > 0 {
		steps = append(steps, aiAdvisoryStep)
	}
	return steps
}

// aiUnasked is the promotion under ai that nothing here can answer the question of (cli.md §6.3, #2875): what would
// ship, what it replaces and what production lacks, and the step that has the agent ask the user.
func (a *app) aiUnasked(p promoted, prod *env) int {
	a.aiPromoteHead("REFUSED", p.app, p.host)
	a.printf("outcome: nothing was promoted: %s asks before it changes production.\nversion: %s\n", a.verb.name, cell(p.version))
	if prod != nil {
		a.printf("replaces: %s\n", a.version(prod))
	}
	a.printf("fleet url: %s\n", cell(p.host+"/#/apps/"+p.app))
	steps := append([]string{"Tell the user the outcome line, in its own words."}, a.aiLacks(p)...) // a preview answer to say is refused before (refusedAnswer) unless a person approves
	a.printf("\n%s\nexit code: %d\n", numbered(append(steps, p.ask)), exitError)
	return exitError
}

func (a *app) aiPromoteFailed(p promoted) int {
	a.aiPromoteHead("FAILED", p.app, p.host)
	a.printf("run: %s\nreason: %s\noutcome: nothing was promoted. Production runs what it ran before.\n", cell(p.run), cell(p.last))
	a.printf("\nNEXT STEPS:\n1. Tell the user the promotion failed, and quote the reason line.\n"+
		"2. If the reason says the app did not stay up, it is the app's: ask the user to run canyon secret set NAME --env prod in their own terminal for a value production lacks. Never ask for the value in the chat. Then run this command again, "+
		"or fix the code, deploy it with canyon deploy, then run this command again. Otherwise stop: an admin can look up run %s in Canyon's server log.\n", cell(p.run))
	a.printf("\nexit code: %d\n", exitError)
	return exitError
}

// aiPromoteBlocked is a receipt that refuses: its checklist, the steps and the exit.
func (a *app) aiPromoteBlocked(r receipt, host, app string) int {
	a.aiPromoteHead("BLOCKED", app, host)
	a.printf("outcome: nothing was promoted.\nfleet url: %s\n\n", cell(host+"/#/apps/"+app))
	a.aiChecklist(r)
	switch {
	case r.egressUnchecked():
		a.printf("\n%s", aiOutboundUnchecked)
	case slices.Contains(r.BlockedBy, egressAllowed): // the fleet url alone doesn't say who allows a destination, or where
		a.printf("\n%s", strings.ReplaceAll(aiOutboundHeld, "<url>", outboundCard(app, host)))
	}
	a.printf("\n%s\nexit code: %d\n", aiPromoteBlockedSteps, exitBlocked)
	return exitBlocked
}

// aiChecklist is the receipt under ai (cli.md §6.3): its rules in the server's order and words, one line
// each, with the state a person reads off the glyph said as a word; with nothing in preview, its one
// line, keyed so a host's line never starts one of the block's own.
func (a *app) aiChecklist(r receipt) {
	if r.Held != nil {
		a.printf("held: %s\n", cell(r.Held.line()))
		return
	}
	if r.Waiting != "" {
		a.printf("waiting: %s\n", cell(r.Waiting))
		return
	}
	a.printf("production checklist:\n")
	for _, rule := range r.Rules {
		state := "blocking"
		switch {
		case rule.Passed:
			state = "passed"
		case rule.Unset:
			state = "not set"
		case !rule.Blocks:
			state = "advisory"
		}
		a.printf("  %s: %s (%s): %s\n", state, cell(rule.Name), cell(rule.ID), cell(rule.Why))
		a.aiPackages(rule)
	}
}
