package cli

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// receipt is the promote receipt (contracts/routes.md): one shape, at rest on the status read and inside the refusal, so both are the same thing.
type receipt struct {
	Org       string        `json:"org"`
	BlockedBy []string      `json:"blocked_by"`
	Rules     []receiptRule `json:"rules"`
	Waiting   string        `json:"waiting,omitempty"` // nothing runs in preview: the receipt is this one line, not evaluated (contracts/routes.md)
	Ships     *shipsImage   `json:"ships,omitempty"`   // a rollback review's alone: what a promotion of it ships
	Held      *prodHold     `json:"held,omitempty"`    // production is held: blocked by prod.held alone, and nothing else evaluated (contracts/routes.md, The hold)
}

type receiptRule struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Blocks bool   `json:"blocks"`
	Passed bool   `json:"passed"`
	Why    string `json:"why"`
	Unset  bool   `json:"unset"` // nothing set to check against (contracts/routes.md): neutral, never a failure
	// platform.known-vuln's alone: the packages it counts (contracts/routes.md, The promote receipt)
	Packages []platformPackage `json:"packages,omitempty"`
	patching bool              // never on the wire: preview's patch is planned (withPatch)
}

const advisoryChip = "advisory" // the dashboard's chip for a rule that failed and blocks nothing

// betaMark is said once on the row of a feature that works but isn't complete or fully tested yet, plain and a
// space after the row's own words, and betaRules are the rules it marks: the dashboard's Beta chip
// (EXPERIENCE.md D5, #2384).
const betaMark = "(beta)"

var betaRules = []string{"platform.known-vuln"}

// cleanGateRule is the one rule whose lead line the receipt carries (Receipt.tsx:26).
const cleanGateRule = "prod.clean-gate"

// checklist renders the server's rules, in the server's order, with the dashboard's five fields each: nothing counts, reorders, filters or rewords, every `why` byte-exact (D6).
func (a *app) checklist(r receipt) {
	if r.Held != nil { // held: D5's line in place of rules the host did not evaluate
		a.printf("\n  %s\n", a.render.paint(ansiRed, a.render.glyph(stateFail)+" "+cell(r.Held.line())))
		return
	}
	if r.Waiting != "" { // nothing in preview: one neutral line, never a checklist that reads as the app failing
		a.printf("\n  %s %s\n", a.render.paint(ansiDim, a.render.glyph(statePending)), cell(r.Waiting))
		return
	}
	if slices.Contains(r.BlockedBy, cleanGateRule) {
		a.printf("\n  %s\n", a.faint(cell(r.Org)+" requires a passing review to reach production."))
	}
	names := 0
	advisories := false
	for _, rule := range r.Rules {
		names = max(names, utf8.RuneCountInString(rule.label()))
		advisories = advisories || rule.advisory()
	}
	a.printf("\n")
	for _, rule := range r.Rules {
		glyph, colour := a.render.glyph(stateFail), ansiRed // red is a blocker, so only a rule that blocks is red
		switch {
		case rule.Passed:
			glyph, colour = a.render.glyph(statePass), ansiGreen
		case rule.Unset:
			glyph, colour = a.render.glyph(statePending), ansiDim
		case !rule.Blocks: // an advisory miss stops nothing: an amber !, never the ✗ of a rule that blocks
			glyph, colour = a.render.glyph(stateWarn), ansiAmber
		}
		cells := []string{a.render.paint(colour, glyph) + " " + pad(rule.label(), names)}
		if advisories { // the column exists only when a rule is in it
			chip := strings.Repeat(" ", len(advisoryChip))
			if rule.advisory() {
				chip = a.render.paint(ansiAmber, advisoryChip)
			}
			cells = append(cells, chip)
		}
		a.printf("  %s\n", strings.TrimRight(strings.Join(cells, "  "), " ")) // the rule's name, never its id: ids are for json and ai
		if rule.Why == "" {
			continue
		}
		for _, line := range wrap(cell(rule.Why), proseWidth-6) { // the server's own line, wrapped and never rewritten
			a.printf("  %s\n", a.faint("    "+line))
		}
		a.packageLines(rule)
	}
}

// label is the rule's name as the checklist says it: a beta rule's ends (beta).
func (r receiptRule) label() string {
	if slices.Contains(betaRules, r.ID) {
		return cell(r.Name) + " " + betaMark
	}
	return cell(r.Name)
}

// advisory is a rule that failed and blocks nothing: it is never red, and it is never a blocker.
func (r receiptRule) advisory() bool { return !r.Passed && !r.Blocks && !r.Unset } // an unset rule failed nothing

// advisories are r's rules that failed and block nothing, in the server's order.
func advisories(r receipt) []receiptRule {
	var out []receiptRule
	for _, rule := range r.Rules {
		if rule.advisory() {
			out = append(out, rule)
		}
	}
	return out
}

// faint paints what is there and nothing when there is not: painting "" is escapes around no text, and the row would print a cell that is not one.
func (a *app) faint(s string) string {
	if s == "" {
		return ""
	}
	return a.render.paint(ansiDim, s)
}

func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-utf8.RuneCountInString(s)))
}
