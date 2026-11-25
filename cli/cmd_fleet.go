package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type fleetPage struct {
	Tiles map[string]int `json:"tiles"`
	Apps  []fleetApp     `json:"apps"`
	Next  *string        `json:"next"`
}

// fleetApp is a fleet row, whole or reduced: below contributor the reader is answered only its name,
// owners and summary (contracts/routes.md, App teams), and a reduced row never carries latest.
type fleetApp struct {
	ID        string                              `json:"id"`
	Name      string                              `json:"name"`
	Owners    []struct{ Subject, Display string } `json:"owners"`
	Summary   string                              `json:"summary"`
	Stack     string                              `json:"stack"`
	GateState string                              `json:"gate_state"`
	Envs      map[string]json.RawMessage          `json:"envs"`
	Latest    *struct{ Since, Stage string }      `json:"latest"`
}

// tiles are the five counts above the table, in the dashboard's order, wording and tone.
var tiles = []struct {
	key, one, many string
	blocker        bool
}{
	{"secrets_exposed", "app with secrets in code", "apps with secrets in code", true},
	{"never_reviewed", "app never reviewed", "apps never reviewed", false},
	{"unowned", "app with no owner", "apps with no owner", false},
	{"blocked", "app blocked", "apps blocked", true},
	{"approved", "app cleared with an exception", "apps cleared with exceptions", false},
}

func runFleet(ctx context.Context, a *app, args []string) int {
	var unowned, never bool
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.BoolVar(&unowned, "unowned", false, "Only the apps nobody owns")
		fs.BoolVar(&never, "never-reviewed", false, "Only apps Canyon has never reviewed")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("fleet takes no arguments, and got %q", rest[0]))
	}
	// Each filter is a tile the server selects; the terminal never narrows a page it was handed.
	filter := ""
	switch {
	case unowned && never:
		return a.usage(errors.New("--unowned and --never-reviewed are two tiles; ask for one"))
	case unowned:
		filter = "unowned"
	case never:
		filter = "never_reviewed"
	}
	query := url.Values{}
	if filter != "" {
		query.Set("tile", filter)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	page, err := readFleet(ctx, client, query)
	if err != nil {
		return a.fail(err)
	}
	a.renderFleet(page, time.Now(), filter, client.page())
	return exitOK
}

// maxFleetPages bounds the read: the cursor is the server's and a client parses none of it.
const maxFleetPages = 100

// readFleet follows the server's own cursor. A client that re-sorts sorts one page.
func readFleet(ctx context.Context, c *Client, query url.Values) (fleetPage, error) {
	var all fleetPage
	seen := map[string]bool{} // any cursor twice is the same page again, however far apart
	for read := 1; ; read++ {
		var page fleetPage
		if err := c.get(ctx, "/api/v1/fleet?"+query.Encode(), &page); err != nil {
			return all, err
		}
		if read == 1 {
			all.Tiles = page.Tiles // whole-fleet counts, and no filter narrows them
		}
		all.Apps = append(all.Apps, page.Apps...)
		switch {
		case page.Next == nil:
			return all, nil
		case seen[*page.Next]:
			return all, errors.New("the host keeps answering the same page")
		case read == maxFleetPages:
			return all, fmt.Errorf("the fleet has not ended after %d pages", maxFleetPages)
		}
		seen[*page.Next] = true
		query.Set("cursor", *page.Next)
	}
}

// renderFleet prints cli.md §6.2: the count, the tiles, the team's table, then the rows read reduced.
func (a *app) renderFleet(page fleetPage, now time.Time, filter, host string) {
	// Under a filter the header speaks its words and the tiles go: four org counts above one row lies.
	head := plural(len(page.Apps), "app", "apps")
	for _, t := range tiles {
		if t.key == filter {
			head = plural(len(page.Apps), t.one, t.many)
		}
	}
	a.printf("\n  %s\n", a.render.paint(ansiBold, head))
	accused := false
	for _, t := range tiles {
		n := page.Tiles[t.key]
		if filter != "" || n == 0 {
			continue // a count of nothing is not an accusation
		}
		if !accused { // its own paragraph, and none at all for an org with nothing wrong
			a.printf("\n")
			accused = true
		}
		glyph, colour := a.render.glyph(stateWarn), ansiAmber
		if t.blocker {
			glyph, colour = a.render.glyph(stateFail), ansiRed
		}
		a.printf("  %s %s\n", a.render.paint(colour, glyph), plural(n, t.one, t.many))
	}
	a.printf("\n")
	if len(page.Apps) == 0 {
		// The dashboard's own words; a column header over no rows says neither.
		if filter == "" {
			a.printf("  No apps yet\n")
		} else {
			a.printf("  No apps match\n")
		}
		return
	}
	named := map[string]int{} // names repeat and ids never do: two apps alike say their ids too (#1911)
	for _, app := range page.Apps {
		named[app.Name]++
	}
	label := func(app fleetApp) string {
		if named[app.Name] > 1 {
			return app.Name + " (" + app.ID + ")"
		}
		return app.Name
	}
	team := [][]string{{"APP", "OWNER", "STACK", "ENV", "REVIEW", "UPDATED"}}
	teamColours := [][]string{{ansiDim, ansiDim, ansiDim, ansiDim, ansiDim, ansiDim}}
	others := [][]string{{"APP", "OWNER", "SUMMARY"}} // the rows read reduced, which the host sorts last: nothing of a run, a stage or an env
	othersColours := [][]string{{ansiDim, ansiDim, ansiDim}}
	for _, app := range page.Apps {
		if app.Latest == nil {
			summary := app.Summary
			if summary == "" {
				summary = "No description yet"
			}
			others = append(others, []string{visible(label(app)), visible(a.owner(app)), cell(summary)})
			othersColours = append(othersColours, []string{"", "", ansiDim})
			continue
		}
		state := app.GateState
		switch {
		case app.GateState == "blocked" && app.Latest.Stage == "approved": // D1: the approved package's run, never another run of any other state
			state = "approved"
		case app.Latest.Stage == "awaiting_review" && app.GateState != "never_reviewed": // a newer version nobody has reviewed: an older run's clean is not this one's
			state = "awaiting_review"
		}
		gate, colour := a.gateCell(state)
		where, whereColour := a.env(app, now)
		row := []string{label(app), a.owner(app), a.orElse(app.Stack), where, gate, a.age(app.Latest.Since, now)}
		for i := range row {
			row[i] = visible(row[i]) // the org's data: a control byte in a name must not drive the terminal
		}
		team = append(team, row)
		teamColours = append(teamColours, []string{"", "", "", whereColour, colour, ansiDim}) // ages are dim, like timings
	}
	if len(team) > 1 {
		a.table(team, teamColours)
	}
	if len(others) > 1 {
		if len(team) > 1 {
			a.printf("\n")
		}
		a.table(others, othersColours)
	}
	if filter != "" {
		return
	}
	// §6.2's call to action: the counts are already computed, so they will return something.
	a.printf("\n")
	for _, hint := range []struct{ flag, key string }{{"--unowned", "unowned"}, {"--never-reviewed", "never_reviewed"}} {
		if n := page.Tiles[hint.key]; n > 0 {
			a.printf("  canyon fleet %-16s %s\n", hint.flag, plural(n, "app", "apps"))
		}
	}
	a.printf("  Open in Canyon %s/#/fleet\n", host)
}

// table prints rows padded by rune (%-9s is short on a ✓), each cell in its colour.
func (a *app) table(rows, colours [][]string) {
	width := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			width[i] = max(width[i], utf8.RuneCountInString(cell))
		}
	}
	for r, row := range rows {
		line := make([]string, len(row))
		for i, cell := range row {
			line[i] = a.render.paint(colours[r][i], cell) + strings.Repeat(" ", width[i]-utf8.RuneCountInString(cell))
		}
		a.printf("  %s\n", strings.TrimRight(strings.Join(line, "  "), " "))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// gateCell is the state word the read answered, never a count: the row carries none.
func (a *app) gateCell(gate string) (word, colour string) {
	switch gate {
	case "clean":
		return a.render.glyph(statePass) + " passed", ansiGreen // the dashboard's word for a clean review
	case "blocked", "failed":
		return a.render.glyph(stateFail) + " " + gate, ansiRed
	case "approved":
		return a.render.glyph(stateWarn) + " approved", ansiAmber
	case "pending", "running":
		return a.render.glyph(statePending) + " reviewing", ansiDim
	case "awaiting_review":
		return a.render.punct("△", "^") + " awaiting review", ansiDim // the dashboard's word and glyph (Stage.tsx)
	default: // never_reviewed, and any state a newer server answers: render the word, invent nothing
		if gate == "never_reviewed" {
			gate = "never reviewed"
		}
		return a.render.glyph(statePending) + " " + cell(gate), ansiDim
	}
}

// dash is a fact the row does not carry; orElse is a cell that may be one.
func (a *app) dash() string { return a.render.punct("—", "-") }

func (a *app) orElse(s string) string {
	if s == "" {
		return a.dash()
	}
	return s
}

// owner is the first owner by display, as the dashboard names them (cli.md §6.2, canyonv2-e69b):
// an identity provider's subject is an id, not a person. With no display, the local part of an
// email subject, its trailing @ saying an address was cut.
func (a *app) owner(app fleetApp) string {
	if len(app.Owners) == 0 {
		return a.dash()
	}
	if d := app.Owners[0].Display; d != "" { // before the subject: one granted by address has none until they sign in
		return cell(d)
	}
	if app.Owners[0].Subject == "" {
		return a.dash()
	}
	if at := strings.IndexByte(app.Owners[0].Subject, '@'); at >= 0 {
		return cell(app.Owners[0].Subject[:at+1])
	}
	return cell(app.Owners[0].Subject)
}

// env is the environment the app is deployed to, prod before preview, and whether it runs now
// (contracts/routes.md, envs.<env>.state): never a stopped app's name alone (#1241); where the org runs it,
// whether its pods still check in, against now (#2455).
func (a *app) env(app fleetApp, now time.Time) (string, string) {
	for _, name := range []string{"prod", "preview"} {
		var e env
		if json.Unmarshal(app.Envs[name], &e) != nil || e.running() == nil { // one waiting on its first release runs nothing
			continue
		}
		switch e.State {
		case "restarting":
			return name + " restarting", ansiRed
		case "stopped":
			return name + " stopped", ansiRed
		case "starting": // a deploy or a promotion starting it: not down (#230)
			return name + " starting", ansiAmber
		case "running":
			return name, ""
		case "": // the org runs it: its pods' check-in says what Canyon can know of it (envs.<env>.seen_at)
			if e.RunBy == "org" && e.silent(now) {
				return name + " no check-in", ansiAmber
			}
			return name, ""
		}
		return name + " couldn't check", ansiAmber // unknown, or a state this build does not know: never named alone
	}
	return a.dash(), ""
}

func (a *app) age(since string, now time.Time) string {
	at, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return a.dash()
	}
	switch d := now.Sub(at); {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
