package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A Canyon finding is a SARIF 2.1.0 result in the profile of contracts/finding.md: plain SARIF
// plus a required properties.canyon bag. These are the fields the surfaces render and hand to
// an agent. `level` is deliberately absent — it is a projection of severity for other tools,
// and Canyon never reads it back.
type finding struct {
	RuleID              string       `json:"ruleId"`
	Message             sarifText    `json:"message"`
	Locations           []location   `json:"locations"`
	RelatedLocations    []location   `json:"relatedLocations"` // further spans of the same finding; kept, not yet rendered
	PartialFingerprints fingerprints `json:"partialFingerprints"`
	Properties          properties   `json:"properties"`
	Approval            *approval    `json:"approval"`   // present only while an unexpired approval covers it (contracts/routes.md, GET /gate-runs/:id)
	Tool                string       `json:"tool"`       // the run read's: the tool.driver.name of the run it is in
	Dependency          *dependency  `json:"dependency"` // the run read's: the dependency its tool named (contracts/finding.md)
}

// dependency is a finding's dependency as its tool wrote it (contracts/routes.md, the run read).
type dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// approved is decision D1's run: it ended blocked and every blocking finding carries an unexpired
// approval, so it is neither blocked nor clean to whoever reads it.
func approved(res runResult) bool { return res.State == "blocked" && everyBlockerApproved(res) }

// everyBlockerApproved is true when the run has a blocking finding and each carries an unexpired
// approval: D1's run, or one that built under approvals and failed after.
func everyBlockerApproved(res runResult) bool {
	n := 0
	for _, f := range res.Findings {
		if blocks, _ := f.Blocking(); blocks {
			if f.Approval == nil {
				return false
			}
			n++
		}
	}
	return n > 0
}

// approvedLine is D1's verdict (EXPERIENCE.md D5): one approval by name and date, more than one
// counted, each then named on its own finding.
func (a *app) approvedLine(res runResult) string {
	only := soleApproval(res)
	if only == nil { // the entries the list shows (§3.3), each then naming its approval
		if n := blockingEntries(res); n > 1 {
			return fmt.Sprintf("%s Review approved %s %d blocking findings, each approved", a.render.glyph(stateWarn), a.dot(), n)
		}
		return fmt.Sprintf("%s Review approved %s 1 blocking finding, approved", a.render.glyph(stateWarn), a.dot()) // its findings' approvals differ, each named on the entry
	}
	return fmt.Sprintf("%s Review approved %s 1 blocking finding, approved by %s until %s", a.render.glyph(stateWarn), a.dot(), cell(only.by()), shortDay(only.ExpiresAt))
}

// soleApproval is the approval of a run's one blocking entry (§3.3), nil when the list shows more than
// one, or one whose findings carry different approvals.
func soleApproval(res runResult) *approval {
	var only *approval
	seen := false
	for _, f := range res.Findings {
		if blocks, _ := f.Blocking(); blocks {
			if seen && (only == nil || f.Approval == nil || *f.Approval != *only) {
				return nil
			}
			only, seen = f.Approval, true
		}
	}
	if blockingEntries(res) > 1 {
		return nil
	}
	return only
}

// shortDay is an approval's expiry as the verdict says it, `7 Oct`; a time this build cannot read is
// said as the host sent it.
func shortDay(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return cell(at)
	}
	return t.UTC().Format("2 Jan")
}

// approval is who let a blocking finding through, until when and why: the org's decision, which a
// check nets as a deploy does (cli.md §3.5, decision D1).
type approval struct {
	Reason     string   `json:"reason"`
	ExpiresAt  string   `json:"expires_at"`
	ApprovedBy actorRef `json:"approved_by"`
}

// by is who approved it, as every actor is named (actorRef.name): with sign-in off, someone not signed in (#2705).
func (ap *approval) by() string { return ap.ApprovedBy.name() }

// Blocking is the only way to ask whether a finding stops the build: said is false when the
// host did not carry `blocks` at all, and a caller that gets that refuses the run. Never
// re-derived from severity, never defaulted to false — the golden's high advisory says
// blocks: false itself, and a silent finding is not the same answer.
func (f finding) Blocking() (blocks, said bool) {
	if f.Properties.Canyon.Blocks == nil {
		return false, false
	}
	return *f.Properties.Canyon.Blocks, true
}

type sarifText struct { // SARIF's {"text": …}, on a message and on a snippet
	Text string `json:"text"`
}

type location struct {
	PhysicalLocation physicalLocation `json:"physicalLocation"`
}

type physicalLocation struct {
	ArtifactLocation artifactLocation `json:"artifactLocation"`
	Region           region           `json:"region"`
}

type artifactLocation struct {
	URI string `json:"uri"`
}

// region is the span: columns count code points, 1-based, EndColumn exclusive. On a secrets
// finding the snippet arrives already masked — a credential is never in one.
type region struct {
	StartLine   int       `json:"startLine"`
	StartColumn int       `json:"startColumn"`
	EndLine     int       `json:"endLine"`
	EndColumn   int       `json:"endColumn"`
	Snippet     sarifText `json:"snippet"`
}

// shown is the caret on a masked or windowed snippet, whose columns are not the region's: start on
// the snippet's first line, end on its last (contracts/finding.md).
type shown struct {
	StartColumn int `json:"startColumn"`
	EndColumn   int `json:"endColumn"`
}

type fingerprints struct {
	CanyonV1 string `json:"canyon/v1"`
}

type properties struct {
	Canyon canyonBag `json:"canyon"`
}

// canyonBag is what Canyon requires and other tools ignore. Blocks is a pointer because it is
// the one field agents and this CLI key off, and absent is not false; read it through Blocking.
type canyonBag struct {
	Severity         string   `json:"severity"`
	Blocks           *bool    `json:"blocks"`
	Title            string   `json:"title"`
	Problem          string   `json:"problem"`
	Fix              string   `json:"fix"`
	PolicyName       string   `json:"policy_name"`
	SecretName       *string  `json:"secret_name,omitempty"`
	Shown            *shown   `json:"shown,omitempty"`
	RequiredEndState string   `json:"required_end_state,omitempty"`
	DoNot            []string `json:"do_not,omitempty"`
}

// §3.3's block: a dim rule per finding, the span with a caret under it, the problem, then fix
// and why. Blocking first, advisory after, wire order within each, nothing wider than the rule.
const proseWidth = 76

func (a *app) findings(run checkRun) {
	var blocking, advisory []finding
	for _, f := range run.Result.Findings {
		if blocks, _ := f.Blocking(); blocks { // verdict has already refused a finding that says nothing
			blocking = append(blocking, f)
		} else {
			advisory = append(advisory, f)
		}
	}
	headline := approved(run.Result) && soleApproval(run.Result) != nil // one approval is the verdict's own line
	// a package's CVEs are one entry, and so is one Canyon rule on one line, blocking and advisory apart (§3.3)
	blockingEntries, advisoryEntries := byLine(cveGroups(blocking)), byLine(cveGroups(advisory))
	for _, e := range slices.Concat(blockingEntries, advisoryEntries) {
		a.rule()
		if len(e.members) > 1 {
			a.group(e, !headline)
			continue
		}
		credited := []finding{e.first}
		if len(e.tools) > 1 {
			credited = e.tools
			a.entry(e.top(), e.toolFixes(), e.foundTools())
		} else {
			a.finding(e.first)
		}
		var said []approval
		for _, f := range credited {
			if !headline && f.Approval != nil && !slices.Contains(said, *f.Approval) { // every other approval is said on its entry, once, a run still blocked included
				said = append(said, *f.Approval)
				a.approvedBy(f)
			}
		}
	}
	a.rule()
	a.footer(run, len(blockingEntries), len(advisoryEntries))
}

// blockingEntries is how many entries a person's blocked list shows (§3.3), which its stage row counts.
func blockingEntries(res runResult) int { return len(entriesOf(res, true)) }

// entriesOf is a run's blocking or non-blocking findings as a person's list shows them (§3.3): a
// package's CVEs one entry, and one Canyon rule on one line one entry.
func entriesOf(res runResult, blocking bool) []*cveGroup {
	var picked []finding
	for _, f := range res.Findings {
		if blocks, _ := f.Blocking(); blocks == blocking {
			picked = append(picked, f)
		}
	}
	return byLine(cveGroups(picked))
}

// approvedBy is the line an approved finding carries under why when the verdict counts more than
// one: who, until when, and the reason they gave (cli.md §3.5).
func (a *app) approvedBy(f finding) {
	if f.Approval == nil {
		return
	}
	a.labelled("", fmt.Sprintf("approved by %s until %s · %s", f.Approval.by(), shortDay(f.Approval.ExpiresAt), f.Approval.Reason))
}

// cell is visible for a host's string, which shares its line with Canyon's own: visible keeps
// \n and \t for an error message, and a finding carrying either would forge a verdict line.
func cell(s string) string { return strings.NewReplacer("\n", `\n`, "\t", `\t`).Replace(visible(s)) }

func (a *app) rule() {
	a.printf("\n  %s\n", a.render.paint(ansiDim, strings.Repeat(a.render.punct("─", "-"), proseWidth)))
}

func (a *app) finding(f finding) { a.entry(f, []string{f.Properties.Canyon.Fix}, "") }

// entry is one finding's block, or one Canyon rule's on one line that several tools' rules found:
// foundBy names them under the place, and each distinct fix prints once (§3.3).
func (a *app) entry(f finding, fixes []string, foundBy string) {
	bag, where := f.Properties.Canyon, f.Locations[0].PhysicalLocation
	colour := ansiAmber // red is for blockers, so an advisory's caret is not red
	if blocks, _ := f.Blocking(); blocks {
		colour = ansiRed
	}
	a.printf("\n  %s  %s\n", a.render.paint(colour, cell(bag.Severity)), a.render.paint(ansiBold, cell(bag.Title))) // what it is; the rule id is json, ai and sarif's
	a.printf("  %s\n", a.render.paint(ansiDim, fmt.Sprintf("%s:%d:%d", cell(where.ArtifactLocation.URI), where.Region.StartLine, where.Region.StartColumn)))
	if from := f.from(); foundBy != "" {
		a.printf("  %s\n", a.render.paint(ansiDim, "found by "+foundBy))
	} else if from != "" {
		a.printf("  %s\n", a.render.paint(ansiDim, from))
	}
	caret := where.Region
	if at := bag.Shown; at != nil {
		caret.StartColumn, caret.EndColumn = at.StartColumn, at.EndColumn
		caret.EndLine = caret.StartLine + strings.Count(caret.Snippet.Text, "\n")
	}
	a.snippet(caret, colour)
	a.printf("\n")
	for _, line := range prose(bag.Problem, proseWidth) {
		a.printf("%s\n", strings.TrimRight("  "+line, " "))
	}
	a.printf("\n")
	for _, fix := range fixes {
		a.labelled("fix", fix)
	}
	a.labelled("why", fmt.Sprintf("Policy %q", bag.PolicyName)) // --explain has no source yet
}

// group is a package's CVEs as one entry (§3.3): the group's line, its place, a line per finding with
// its approval under it, then each distinct fix and the policy. No excerpt: a lockfile's line says
// nothing the CVE ids do not.
func (a *app) group(g *cveGroup, approvals bool) {
	where := g.first.Locations[0].PhysicalLocation
	colour := ansiAmber
	if blocks, _ := g.first.Blocking(); blocks {
		colour = ansiRed
	}
	a.printf("\n  %s  %s\n", a.render.paint(colour, cell(g.severity)), a.render.paint(ansiBold, cell(g.summary())))
	a.printf("  %s\n\n", a.render.paint(ansiDim, fmt.Sprintf("%s:%d:%d", cell(where.ArtifactLocation.URI), where.Region.StartLine, where.Region.StartColumn)))
	width := len(slices.MaxFunc(g.cves, func(x, y string) int { return len(x) - len(y) }))
	var fixes []string
	for i, f := range g.members {
		bag := f.Properties.Canyon
		a.printf("  %-*s  %-8s  %s\n", width, cell(g.cves[i]), cell(bag.Severity), cell(bag.Title))
		if approvals {
			a.approvedBy(f)
		}
		if !slices.Contains(fixes, bag.Fix) {
			fixes = append(fixes, bag.Fix)
		}
	}
	a.printf("\n")
	for _, fix := range fixes {
		a.labelled("fix", fix)
	}
	a.labelled("why", fmt.Sprintf("Policy %q", g.first.Properties.Canyon.PolicyName))
}

// from is an imported tool's finding's origin, `from <tool> · rule <its rule>`, "" for any other finding.
func (f finding) from() string {
	tool, rule, ok := f.imported()
	if !ok {
		return ""
	}
	return "from " + cell(tool) + " · rule " + cell(rule)
}

// imported is the tool and the tool's own rule of a finding no Canyon policy covers, whose rule id is
// sarif.<tool>.<its rule> (contracts/finding.md, snapshot.md); false for any other finding.
func (f finding) imported() (tool, rule string, ok bool) {
	space, rest, _ := strings.Cut(f.RuleID, ".")
	tool, rule, ok = strings.Cut(rest, ".")
	return tool, rule, ok && space == "sarif"
}

// snippet: the span's lines behind a dim gutter, the caret under the first of them.
func (a *app) snippet(r region, colour string) {
	a.printf("\n")
	lines := strings.Split(r.Snippet.Text, "\n")
	width := max(4, len(strconv.Itoa(r.StartLine+len(lines)-1))) // §3.3's gutter, widened only by a long file
	gutter := a.render.punct("│", "|")
	for i, line := range lines {
		head := a.render.paint(ansiDim, fmt.Sprintf("%*d %s", width, r.StartLine+i, gutter))
		a.printf("%s\n", strings.TrimRight("  "+head+" "+cell(line), " "))
		if i > 0 {
			continue
		}
		before, span := caretSpan(r, line)
		runes := []rune(line) // the caret sits under the line as printed, so it is measured on the escaped form
		head, body := cell(string(runes[:before])), cell(string(runes[before:before+span]))
		a.printf("  %s %s%s\n", a.render.paint(ansiDim, strings.Repeat(" ", width)+" "+gutter),
			strings.Repeat(" ", utf8.RuneCountInString(head)), a.render.paint(colour, strings.Repeat("^", max(1, utf8.RuneCountInString(body)))))
	}
}

// caretSpan places the caret by code point: SARIF columns are 1-based, end exclusive, and count
// code points (contracts/finding.md), so a byte offset drifts across the •••• of a mask. The span
// is clamped to the line. On a masked or windowed snippet it is canyon.shown's, not the region's.
func caretSpan(r region, line string) (before, span int) {
	n := utf8.RuneCountInString(line)
	start := min(max(r.StartColumn, 1), n+1)
	end := n + 1
	if r.EndLine == r.StartLine {
		end = min(max(r.EndColumn, start+1), n+1)
	}
	return start - 1, max(end-start, 1)
}

// labelled aligns fix and why on the same six-column label.
func (a *app) labelled(label, text string) {
	head := a.render.paint(ansiDim, label) + strings.Repeat(" ", 6-len(label))
	for _, line := range prose(text, proseWidth-6) {
		a.printf("%s\n", strings.TrimRight("  "+head+line, " "))
		head = strings.Repeat(" ", 6)
	}
}

// prose is a finding's text refilled to width. A rule's message is its author's wrapped text, so a
// line break in it is a space: never printed as an escape, and never a line of its own, which a
// server string could make read as a line the gate said.
func prose(s string, width int) []string {
	return wrap(strings.ReplaceAll(visible(s), "\t", `\t`), width) // wrap splits on every space, a line break among them
}

// wrap refills s to at most width code points. No terminal width is asked for: one shape in a
// terminal, a pipe and a CI log.
func wrap(s string, width int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			out, line = append(out, line), word
		}
	}
	return append(out, line)
}

// The footer's two exits: check again, or open the app in Canyon. The host serves no per-run screen.
func (a *app) footer(run checkRun, blockers, warnings int) {
	a.printf("\n  %s %s %s\n", plural(blockers, "blocking finding", "blocking findings"), a.dot(),
		plural(warnings, "warning", "warnings")) // a run id is not something a person says
	if a.shipped(run) { // the live app and its page print below; the fix loop is what exit 0 exists to avoid
		return
	}
	if approved(run.Result) { // nothing is left to fix: the next deploy goes ahead under the approvals
		a.printf("\n  Ship it: canyon deploy\n")
		a.history(run.Host, run.App)
		return
	}
	if run.Rollback != nil { // a version that shipped before is approved or left as it is, never fixed (design/OPERATIONS.md §1.7)
		a.printf("\n  %s\n", run.Rollback.blocked())
	} else {
		a.printf("\n  Fix it, then check again without deploying:\n    canyon check\n")
		a.printf("\n  %s\n", approvers)
	}
	a.history(run.Host, run.App)
}

// approvers is who approves a blocking finding instead of a fix (EXPERIENCE.md D5; design/PERMISSIONS.md, #2204).
const approvers = "Only an admin or a security responder can approve a blocking finding, and never their own."

// history is the app's page: one sentence in one place, so check and status point at it alike.
func (a *app) history(host, app string) {
	a.printf("\n  Open in Canyon %s/#/apps/%s\n", host, cell(app))
}

func (a *app) dot() string { return a.render.punct("·", "|") }

func since(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}
