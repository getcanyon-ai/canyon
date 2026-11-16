package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// envPatch is envs.<env>.patch (contracts/routes.md, Patch runs): the environment's patch state, on the status
// read from contributor up; nil below it, and from a host before it.
type envPatch struct {
	State    string `json:"state"`
	Since    string `json:"since"`
	Identity string `json:"identity,omitempty"`
	Run      string `json:"run,omitempty"`
	Approval string `json:"approval,omitempty"`
	Why      string `json:"why,omitempty"`
}

// patched is the env row's provenance while the environment runs Canyon's patch (design/PATCHING.md §8):
// preview's patched_preview, production's patched.
func (e *env) patched() bool {
	return e != nil && e.Patch != nil && (e.Patch.State == "patched_preview" || e.Patch.State == "patched")
}

// platformOrder is which environment's state the platform line says: the most pressing first.
var platformOrder = []string{"asked", "reverted", "held", "denied", "could_not_rescan", "wait", "planned", "patched_preview", "patched", "none"}

// pickPlatform is the environment the platform line is of, and its state: "" when neither carries one.
func pickPlatform(envs map[string]*env) (string, *envPatch) {
	best, at := "", len(platformOrder)
	for _, key := range []string{"prod", "preview"} {
		if e := envs[key]; e != nil && e.Patch != nil {
			if i := slices.Index(platformOrder, e.Patch.State); i >= 0 && i < at {
				best, at = key, i
			}
		}
	}
	if best == "" {
		return "", nil
	}
	return best, envs[best].Patch
}

// patchFix is one fix of a patch approval (contracts/routes.md, Production approvals).
type patchFix struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// platformFacts is what the platform line reads beyond the status read, each fetched only for the state that
// says it: the asked approval's fixes, the run's last build.log line — the patch run's, or production's revert the
// promote run's, whichever the state names — and the environment's image read, with how many findings it counts.
type platformFacts struct {
	Fixes     []patchFix
	Rebuild   bool // the asked fix is a rebuild (contracts/routes.md, A rebuild patch): the app is built again, never unchanged
	Line      string
	Checked   bool
	Read      string // the image read's state: checked, could_not_check or not_scanned
	Why       string
	CheckedAt string
	Counted   int
}

// platformLine is the status page's platform row (design/PATCHING.md §8): a glyph, its words, and the approve
// link under it while asked; Unread is why a read it needed failed, which the row says in its place.
type platformLine struct {
	State   state
	Text    string
	Approve string
	Unread  string
}

// couldNotCheck is D5's prefix of a read that could not check (contracts/routes.md, the image read's why).
const couldNotCheck = "could not check: "

// platformLineOf is the platform row for env's state p, from the facts fetched for it; nil where it says nothing:
// an image Canyon has not checked, under a state that only says it was checked.
func platformLineOf(env string, p envPatch, f platformFacts, s statusRead, page string, now time.Time, ago func(string, time.Time) string) *platformLine {
	where := map[string]string{"preview": "preview", "prod": "production"}[env]
	version := func(key string) string {
		if e := s.Envs[key]; e != nil {
			return cmpVersion(e)
		}
		return "it"
	}
	switch p.State {
	case "asked":
		var fixes []string // each package once: the approval lists one fix per vulnerability (#2631)
		for _, x := range f.Fixes {
			if one := cell(x.Name) + " " + cell(x.From) + " → " + cell(x.To); !slices.Contains(fixes, one) {
				fixes = append(fixes, one)
			}
		}
		app := ", app unchanged"
		if f.Rebuild {
			app = ", app rebuilt"
		}
		return &platformLine{State: stateWarn, Text: "a fix is ready for production: " + strings.Join(fixes, ", ") + app,
			Approve: page + "/#/approve?code=" + url.QueryEscape(p.Approval)}
	case "denied":
		var kept []string // each package once, as production runs it: the approval lists one fix per vulnerability
		for _, x := range f.Fixes {
			if one := cell(x.Name) + " " + cell(x.From); !slices.Contains(kept, one) {
				kept = append(kept, one)
			}
		}
		return &platformLine{State: stateWarn, Text: "Your team turned down Canyon's fix for production. Production still has " + strings.Join(kept, ", ") + ". Canyon will ask again when a newer fix is out."}
	case "reverted":
		return &platformLine{State: stateFail, Text: cmp.Or(cell(f.Line), "Canyon reverted its patch of "+where)}
	case "held":
		why := map[string]string{
			"blocked":         version(env) + " is blocked by its review",
			"production_held": "production is held",
			"paused":          "Canyon paused this fix until an admin resumes it",
			"preview_ahead":   "promote " + version("preview") + " to take the fix to production",
		}[p.Why]
		if why == "" { // a patch run's end: its own last line says what stopped it
			why = cmp.Or(cell(f.Line), "Canyon could not finish patching "+version(env)+", so it did not deploy it")
		}
		return &platformLine{State: stateWarn, Text: "held: " + why}
	case "could_not_rescan":
		why := "Canyon couldn't check which fixes its system libraries carry"
		if !f.Checked {
			why = strings.TrimPrefix(cell(f.Why), couldNotCheck)
		}
		return &platformLine{State: stateWarn, Text: "couldn't rescan: " + why}
	case "wait":
		return &platformLine{State: statePending, Text: "Ubuntu has a fix; Canyon hasn't picked it up yet"}
	case "planned":
		return &platformLine{State: statePending, Text: "Canyon is patching it"}
	}
	if f.Read == "could_not_check" { // never nothing to say: a check that did not finish is no pass (#2079)
		return &platformLine{State: stateFail, Text: cell(f.Why)}
	}
	if !f.Checked { // none, or a patch running: the line says it was checked, and only a checked read says so
		return nil
	}
	text, glyph := "checked: the system libraries and build tools Canyon adds", statePass
	if f.Counted > 0 { // checked, and not clean: its count in the read's own words, never a pass
		text, glyph = "checked: "+cell(f.Why), statePending
	}
	if when := ago(f.CheckedAt, now); when != "" { // the vulnerability database's age, never the image's: a promotion since leaves it be (#2579)
		text += " · vulnerability data from " + when
	}
	return &platformLine{State: glyph, Text: text}
}

// cmpVersion is an environment's version as the rows name it, never the dash.
func cmpVersion(e *env) string {
	if e.Label != "" {
		return cell(e.Label)
	}
	return cell(strings.TrimPrefix(e.Package, "pkg_"))
}

// platformFactsOf fetches what the platform line of env's state p reads: the approval while asked, the patch run's
// last build.log line while reverted or held by its end, and the environment's image read otherwise.
func platformFactsOf(ctx context.Context, c *Client, clk clock, at, env string, p envPatch) (platformFacts, error) {
	var f platformFacts
	switch {
	case p.State == "asked", p.State == "denied": // the fixes production waits on, or declined
		var ap struct {
			Fixes   []patchFix `json:"fixes"`
			Rebuild bool       `json:"rebuild"`
		}
		err := c.get(ctx, "/api/v1/promotion-approvals/"+url.PathEscape(p.Approval), &ap)
		f.Fixes, f.Rebuild = ap.Fixes, ap.Rebuild
		return f, err
	case p.Run != "" && (p.State == "reverted" || p.State == "held" && !slices.Contains([]string{"blocked", "production_held", "paused", "preview_ahead"}, p.Why)):
		_, err := follow(ctx, c, p.Run, clk, platformCeiling, func(row timelineRow) { // a status read waits on nobody: asked once (#2730)
			if row.Kind == "build.log" {
				var data struct {
					Line string `json:"line"`
				}
				if json.Unmarshal(row.Data, &data) == nil {
					f.Line = data.Line
				}
			}
		}, askedOnce)
		if late := (*pastCeiling)(nil); errors.As(err, &late) { // a patch run, never "the review"
			err = fmt.Errorf("the patch run's timeline didn't end within %s", plural(int(platformCeiling.Seconds()), "second", "seconds"))
		}
		return f, err
	case p.State == "reverted", p.State == "held", p.State == "wait", p.State == "planned":
		return f, nil
	}
	var read struct {
		State     string            `json:"state"`
		Why       string            `json:"why"`
		DBBuiltAt string            `json:"db_built_at"`
		Findings  []json.RawMessage `json:"findings"`
	}
	if err := c.get(ctx, at+"/envs/"+url.PathEscape(env)+"/image", &read); err != nil {
		return f, err
	}
	f.Checked, f.Read, f.Why, f.CheckedAt, f.Counted = read.State == "checked", read.State, read.Why, read.DBBuiltAt, len(read.Findings)
	return f, nil
}

// platformOf is the status page's platform row: nil where neither environment carries a patch state (below
// contributor, or a host before it), and a row that says a read failed rather than one drawn from nothing.
func platformOf(ctx context.Context, c *Client, clk clock, at string, s statusRead, now time.Time, ago func(string, time.Time) string) *platformLine {
	env, p := pickPlatform(s.Envs)
	if p == nil {
		return nil
	}
	f, err := platformFactsOf(ctx, c, clk, at, env, *p)
	if err != nil {
		return &platformLine{Unread: fmt.Sprint(err)}
	}
	return platformLineOf(env, *p, f, s, c.page(), now, ago)
}

// platformRow prints the platform row under the environments, and the approve link under it while asked.
func (a *app) platformRow(l *platformLine) {
	switch {
	case l == nil:
	case l.Unread != "":
		a.row("platform", a.render.paint(ansiAmber, "couldn't read: "+cell(l.Unread)))
	default:
		colour := map[state]string{statePass: ansiGreen, stateFail: ansiRed, stateWarn: ansiAmber}[l.State]
		a.row("platform", a.render.paint(colour, a.render.glyph(l.State)+" "+l.Text))
		if l.Approve != "" {
			a.row("", a.faint("approve it in Canyon: "+l.Approve))
		}
	}
}

// patchedBy is the env row's provenance while it runs Canyon's patch (design/PATCHING.md §8), "" otherwise.
func (a *app) patchedBy(e *env, now time.Time) string {
	if !e.patched() {
		return ""
	}
	return a.faint(strings.TrimSpace("patched by Canyon " + a.ago(e.Patch.Since, now)))
}
