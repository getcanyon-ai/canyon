package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

// reportFiles is --sarif, repeatable: the reports to attach to the package, in the order given (cli.md §4.1).
type reportFiles []string

func (r *reportFiles) String() string        { return strings.Join(*r, ",") }
func (r *reportFiles) Set(file string) error { *r = append(*r, file); return nil }

const sarifUsage = "Attach a tool's SARIF report to this review; repeatable"

// attach is cli.md §4.1: each report, in the order given, attached to the package just uploaded, one
// line each. A file it cannot read, or a report the host refuses, ends the verb before a run exists: a
// check asked to include a report never runs without it.
func (a *app) attach(ctx context.Context, c *Client, run *checkRun) error {
	for _, file := range run.Reports {
		raw, err := os.ReadFile(file) //nolint:gosec // the file the person named
		if err != nil {
			return fmt.Errorf("could not read the report %s: %w", file, errors.Unwrap(err))
		}
		var got struct {
			Tool         string `json:"tool"`
			Category     string `json:"category"`
			Results      int    `json:"results"`
			Dropped      int    `json:"dropped"`
			FirstDropped string `json:"first_dropped"`
		}
		path := "/api/v1/projects/" + url.PathEscape(run.App) + "/packages/" + url.PathEscape(run.Package) + "/reports"
		if err := c.do(ctx, http.MethodPut, path, "application/sarif+json", bytes.NewReader(raw), int64(len(raw)), nil, http.StatusOK, requestTimeout, answerLimit, &got); err != nil {
			return err
		}
		line := named(got.Tool, got.Category) + " " + a.dot() + " " + plural(got.Results, "result", "results")
		if got.Dropped > 0 {
			line += fmt.Sprintf(" %s %d dropped: %s", a.dot(), got.Dropped, cell(got.FirstDropped))
		}
		a.printf("  %s Report     %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), line)
	}
	return nil
}

// named is an imported tool as its lines name it: the category in parentheses when it has one.
func named(tool, category string) string {
	if category == "" {
		return cell(tool)
	}
	return cell(tool) + " (" + cell(category) + ")"
}

// toolLine is an imported tool's row under the review's stage row, read from its run in the verdict log
// (contracts/finding.md, In: external SARIF): the run's own note says whether the package had a
// report and who attached it, and its results whether any blocks.
func (a *app) toolLine(tool string, r sarifRun) {
	said := r.said()
	name := named(tool, r.AutomationDetails.ID)
	for _, s := range said { // this run's scan did not finish: never clean, whatever report the package had (#1658)
		if why, ok := strings.CutPrefix(s, tool+" · could not check: "); ok {
			a.gateLine(a.render.glyph(stateFail), ansiRed, name, "could not check: "+cell(why), "", 0)
			return
		}
	}
	for _, s := range said { // nothing to read: never clean, and it says more than no report (#1742)
		if why, ok := strings.CutPrefix(s, tool+" · read no file: "); ok {
			a.gateLine(a.render.punct("–", "-"), ansiDim, name, cell(why), "", 0)
			return
		}
	}
	if slices.Contains(said, tool+" · no report for this package") {
		a.gateLine(a.render.punct("–", "-"), ansiDim, name, "no report for this package", "", 0)
		return
	}
	by := ""
	for _, s := range said {
		if who, ok := strings.CutPrefix(s, tool+" · report attached by "); ok {
			by = who
		}
	}
	var blockers, rest []finding
	for _, f := range r.Results {
		f.Tool = r.Tool.Driver.Name                        // the verdict log's results carry no run read's tool: this run's is theirs
		if blocks, said := f.Blocking(); blocks || !said { // unknown blocks: fail closed, never a green line
			blockers = append(blockers, f)
		} else {
			rest = append(rest, f)
		}
	}
	// the entries this tool contributed to, as the list shows them (cli.md §3.3)
	blocking, warnings := len(byLine(cveGroups(blockers))), len(byLine(cveGroups(rest)))
	glyph, colour, found := a.render.glyph(statePass), ansiGreen, "clean"
	if blocking+warnings > 0 { // what does not block still counts, as the verdict counts it (cli.md §3.1)
		found = plural(blocking+warnings, "warning", "warnings")
	}
	if missed := len(r.missed()); missed > 0 { // what it could not read is never a green line, nor "clean" (cli.md §3.1)
		if len(r.Results) == 0 {
			found = "0 findings"
		}
		glyph, colour, found = a.render.glyph(statePending), ansiDim, found+" "+a.dot()+" "+fmt.Sprintf("%d not checked", missed)
	}
	if blocking > 0 {
		glyph, colour, found = a.render.glyph(stateFail), ansiRed, plural(blocking, "blocking finding", "blocking findings")
	}
	if by != "" && by != "Canyon" { // a tool Canyon ran itself is one more row of the review; a report a person attached names them
		found += " " + a.dot() + " attached by " + cell(by)
	}
	a.gateLine(glyph, colour, name, found, "", 0)
}
