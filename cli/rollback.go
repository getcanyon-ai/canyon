package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// rollback is design/OPERATIONS.md §1: `deploy --env prod --version` puts an earlier version back in
// production. The version resolves through the status read's earlier[] to a package and the image
// production ran, or is refused before anything is sent; that package is reviewed again under
// today's policy, and the image its review recorded is promoted through the promotions route and its
// receipt, as preview's image is.

const versionUsage = "--version puts an earlier version back in production: canyon deploy --env prod --version <version>"

// versionFlag is deploy's --version: given records that it appeared, so `--version=` is a version
// left out, never none asked for.
type versionFlag struct {
	given bool
	name  string
}

func (v *versionFlag) String() string     { return v.name }
func (v *versionFlag) Set(s string) error { v.given, v.name = true, s; return nil }

// versionShaped is a version as a host can name one: a label (routes.md, the packages row's
// Canyon-Package-Label) or a package's id, with or without pkg_. Only such a name is ever printed
// into a command a person or an agent runs.
var versionShaped = regexp.MustCompile(`^(pkg_)?[a-z0-9][a-z0-9._-]{0,63}$`)

// againWith is the command that runs this rollback again, the version in it only when it is shaped like one.
func againWith(named string) string {
	if !versionShaped.MatchString(named) {
		named = "<version>"
	}
	return "deploy --env prod --version " + named
}

// earlier is one of production's earlier versions on the status read (contracts/routes.md, earlier[]).
type earlier struct {
	Package     string `json:"package"`
	Version     string `json:"version"`
	ImageDigest string `json:"image_digest"`
	DeployedAt  string `json:"deployed_at"`
	Patched     bool   `json:"patched,omitempty"`
	Kept        bool   `json:"kept"`
}

// shipsImage is a rollback review's receipt's ships: the version and the image a promotion of it ships.
type shipsImage struct {
	Version     string `json:"version"`
	ImageDigest string `json:"image_digest"`
}

// matching is the rows a name names: those of the package it names by id, with or without pkg_, else those its label
// names, newest first.
func matching(rows []earlier, named string) []int {
	var matched []int
	for i, r := range rows {
		if r.Package == named || r.Package == "pkg_"+named {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		for i, r := range rows {
			if r.Version == named {
				matched = append(matched, i)
			}
		}
	}
	return matched
}

// earlierNamed is the row a name puts back: of the rows it names (matching), the newest whose image Canyon kept, else
// the newest (the list is newest first). nil when it names none; ambiguous when it names rows of two packages.
func earlierNamed(rows []earlier, named string) (row *earlier, ambiguous bool) {
	matched := matching(rows, named)
	for _, i := range matched {
		if rows[i].Package != rows[matched[0]].Package {
			return nil, true
		}
	}
	for _, i := range matched {
		if rows[i].Kept {
			return &rows[i], false
		}
	}
	if len(matched) > 0 {
		return &rows[matched[0]], false
	}
	return nil, false
}

// putBack is what a rollback review renders beside its verdict: the version, when production ran it,
// and what production runs now, "" for nothing.
type putBack struct {
	version, ranOn, replaces string
}

func (a *app) rollback(ctx context.Context, client *Client, at, name string, read statusRead, named, reason string, yes, wait bool) int {
	if read.Earlier == nil { // a host from before earlier[]: it can say no version, so none is sent
		return a.refuseVersion(name, client.page(), "This Canyon doesn't list production's earlier versions; it needs updating.",
			"this Canyon doesn't list production's earlier versions; it needs updating.", nil,
			"Stop: whoever runs this Canyon updates it, and then this command works. Do not look for another way to put a version back.",
			"Whoever runs this Canyon updates it; then this command works.")
	}
	row, ambiguous := earlierNamed(read.Earlier, named)
	switch {
	case ambiguous:
		ids := idsNamed(read.Earlier, named)
		return a.refuseVersion(name, client.page(),
			cell(named)+" names more than one of production's earlier versions, so Canyon can't tell which to put back. Name one by its id: "+ids+".",
			cell(named)+" names more than one of production's earlier versions.", []string{"versions it names: " + ids},
			"Ask the user which one they mean, then run canyon deploy --env prod --version <its id>, with an id from the versions it names line.", "")
	case row == nil: // nothing was sent
		const fix, then = "Stop: no earlier version can be put back. A fix goes to production as a new version: canyon deploy, then canyon deploy --env prod.",
			"A fix goes to production as a new version: canyon deploy, then canyon deploy --env prod."
		back := puttable(read.Earlier)
		switch prod := read.Envs["prod"].running(); {
		case prod != nil && (prod.Package == named || prod.Package == "pkg_"+named || prod.Label == named): // named as earlierNamed names a row
			line := cell(named) + " is what production runs now: there's nothing to put back."
			return a.refuseVersion(name, client.page(), line, line, nil, "Stop: production already runs "+cell(named)+".", "")
		case len(read.Earlier) == 0:
			return a.refuseVersion(name, client.page(), "Production has no earlier version to put back.",
				"production has no earlier version to put back.", nil, fix, then)
		case len(back) == 0:
			line := cell(named) + " isn't one of production's last five versions, and none of those can be put back: Canyon no longer has their images."
			return a.refuseVersion(name, client.page(), line, line, nil, fix, then)
		}
		return a.refuseVersion(name, client.page(),
			cell(named)+" isn't one of production's last five versions. You can put back: "+strings.Join(back, ", ")+".",
			cell(named)+" isn't one of production's last five versions.", []string{"can put back: " + strings.Join(back, ", ")},
			"If the user meant a version on the can put back line, run canyon deploy --env prod --version <it>. "+
				"Otherwise stop: Canyon puts back only production's last five versions, and only those whose image it kept.", "")
	case row.Package == "" || row.ImageDigest == "": // nothing to name in the review: never a review of whatever is current
		return a.fail(fmt.Errorf("%s lists %s without its package or its image, so it can't be put back", client.Base, cell(named)))
	}
	back := &putBack{version: cell(row.Version), ranOn: shortDay(row.DeployedAt)}
	if prod := read.Envs["prod"].running(); prod != nil {
		back.replaces = a.version(prod)
	}
	asked := struct {
		Package    string `json:"package"`
		RollbackTo string `json:"rollback_to"`
		Reason     string `json:"reason,omitempty"`
	}{row.Package, row.ImageDigest, reason}
	var started struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	began := a.clock.now()
	err := client.postJSON(ctx, at+"/gate-runs", asked, http.StatusAccepted, &started)
	switch api, busy := refusal(err, http.StatusConflict, "run_in_progress"); {
	case busy:
		return a.fail(fmt.Errorf("another run of %s is under way%s; let it finish, then run canyon %s again", cell(name), runNamed(api.Extra["run"]), a.verb.name)) // the version as typed, shaped as one
	case err != nil: // not_earlier, invalid_reason, rate_limited: the host's words
		return a.fail(err)
	case !validRun.MatchString(started.Run.ID):
		return a.fail(&NotCanyon{Host: client.Base, Status: http.StatusAccepted})
	}
	a.printf("\n  Reviewing %s again under today's rules. Preview is not touched.\n", back.version) // once the host took it
	run := checkRun{App: name, Host: client.page(), Package: row.Package, Rollback: back}
	each, stop := a.narrate(&run)
	seen, err := a.poll(ctx, client, started.Run.ID, checkCeiling, each)
	stop()
	if code, still := a.reviewStillRunning(run, started.Run.ID, seen, err); still { // nothing goes back while it runs (#2730)
		return code
	}
	if err != nil {
		code := a.fail(err)
		a.errf("\n  The review keeps going without this terminal:\n  %s/#/apps/%s\n", run.Host, cell(name))
		return code
	}
	var got struct {
		Run runResult `json:"run"`
	}
	if err := a.readRun(ctx, client, started.Run.ID, &got); err != nil {
		if code, unread := a.endedUnread(run, started.Run.ID, seen, "review", err); unread {
			return code
		}
		return a.fail(err)
	}
	// the id this CLI checked, never one a read answered
	run.Run, run.StartedAt, run.Elapsed, run.Result = started.Run.ID, seen.StartedAt, a.clock.now().Sub(began), got.Run
	if res := run.Result; res.State != "clean" && !approved(res) { // blocked, failed or unknown: the verdict, and nothing promoted
		a.appRules(ctx, client, &run)
		return a.verdict(run)
	}
	if !a.agrees(run) {
		return exitError
	}
	a.reviewStage(run)
	var receipt statusRead
	if err := client.get(ctx, at+"/status?run="+url.QueryEscape(run.Run), &receipt); err != nil {
		return a.fail(err)
	}
	r := receipt.Receipt
	if r.Held != nil { // held while the review ran: nothing goes back (contracts/routes.md, The hold)
		return a.heldReceipt(ctx, client, name, *r.Held)
	}
	asks := slices.Equal(r.BlockedBy, []string{"prod.human-approval"})
	if len(r.BlockedBy) > 0 && !asks {
		return a.refused(r, client.page(), name)
	}
	if r.Ships == nil || r.Ships.ImageDigest != row.ImageDigest { // the image production ran, or nothing goes back
		return a.fail(errors.New(back.version + "'s review doesn't name the image production ran, so nothing was promoted"))
	}
	shipping := cell(r.Ships.Version)
	replaces, replaced := "Nothing runs in production yet.", "nothing, production runs nothing now"
	left := "."
	if back.replaces != "" {
		replaces, replaced = "It replaces "+back.replaces+".", back.replaces
		left = ": " + shipping + " runs against production as " + back.replaces + " left it."
	}
	a.printf("\n  Putting %s back in production. %s\n", shipping, replaces)
	a.printf("  %s\n", a.faint("Production gets the image it ran on "+back.ranOn+"."))
	a.printf("  Canyon puts back the code, not the data, the secrets or the outbound rules%s\n", left)
	for _, rule := range advisories(r) { // what production refuses of what a person dismissed: said before anything is sent
		if rule.ID == egressAllowed {
			a.printf("  %s\n", a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+cell(rule.Why)))
		}
	}
	out := promoted{app: name, host: client.page(), version: shipping + cleared(r), replaced: replaced, putBack: named, advisory: advisories(r)}
	if preview, prod := read.Envs["preview"].running(), read.Envs["prod"].running(); preview != nil && prod != nil && prod.Package != "" && preview.Package == prod.Package {
		out.keeps = back.replaces // what this takes out of production preview still runs: the next deploy --env prod ships it again (#2787)
	}
	out.ask = "Ask the user whether to put back " + shipping + " in production; if they say yes, run canyon " + againWith(named) + " --yes." // under ai, where nothing here can answer (#2873)
	out.question, out.ships = "Put "+cell(name)+" "+shipping+" back in production?", r.Ships.ImageDigest                                     // its review is a new run each time: the image is what stays
	if back.replaces != "" {
		out.question = "Put " + cell(name) + " " + shipping + " back in production, replacing " + back.replaces + "?"
	}
	return a.promote(ctx, client, at, read, out, run.Run, asks, yes, wait)
}

// refuseVersion is a version a rollback refuses before it promotes anything (cli.md §6.3.1): for a person its line,
// then, as the CLI's other refusals end, then: what to do next, where the line doesn't already say it; under ai a
// REFUSED block whose outcome and steps say what the agent can do instead, with keyed, each "key: value", between them
// (#2785): an agent that read the line alone retried it, then said Canyon contradicted itself.
func (a *app) refuseVersion(name, host, line, outcome string, keyed []string, step, then string) int {
	if a.render.Format != FormatAI {
		a.printf("\n  %s\n", line)
		if then != "" {
			a.printf("  %s\n", then)
		}
		return exitError
	}
	a.aiPromoteHead("REFUSED", name, host)
	a.printf("outcome: nothing was promoted: %s\n", outcome)
	for _, k := range keyed {
		a.printf("%s\n", k)
	}
	a.printf("fleet url: %s\n", cell(host+"/#/apps/"+name))
	a.printf("\nNEXT STEPS:\n1. Tell the user the outcome line, in its own words.\n2. %s\n\nexit code: %d\n", step, exitError)
	return exitError
}

// idsNamed is the packages a name names more than one of, each by its id without pkg_, as --version takes it.
func idsNamed(rows []earlier, named string) string {
	var ids []string
	for _, i := range matching(rows, named) {
		if id := cell(strings.TrimPrefix(rows[i].Package, "pkg_")); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ", ")
}

// puttable is each earlier version --version puts back as it ran, newest first: one Canyon kept the image of, shaped as
// a version, whose name resolves to that kept row's package; one that is not kept cannot be put back as it ran.
func puttable(rows []earlier) []string {
	var named []string
	for _, row := range rows {
		got, _ := earlierNamed(rows, row.Version)
		if row.Kept && versionShaped.MatchString(row.Version) && got != nil && got.Kept && got.Package == row.Package && !slices.Contains(named, cell(row.Version)) {
			named = append(named, cell(row.Version))
		}
	}
	return named
}

// cleared is " (passed review)" when the receipt's prod.clean-gate passed: an advisory one lets a failed
// review through, and then nothing says a review cleared it.
func cleared(r receipt) string {
	for _, rule := range r.Rules {
		if rule.ID == cleanGateRule && rule.Passed {
			return " (passed review)"
		}
	}
	return ""
}

// blocked is the line under a rollback review's blocked verdict (design/OPERATIONS.md §1.7).
func (b *putBack) blocked() string {
	return b.version + " went to production on " + b.ranOn + "; today's review blocks it. " +
		"An admin or a security responder who didn't start this review can approve each blocking finding with a reason in Canyon's dashboard; then run this again."
}

// aiSteps are a blocked rollback review's, in place of the fix loop: the version is not the agent's to change.
func (b *putBack) aiSteps() string {
	runs := "production still runs " + b.replaces
	if b.replaces == "" {
		runs = "production runs nothing"
	}
	return "rollback: " + b.version + " → prod; " + runs + "\n" + numbered([]string{
		"Tell the user today's review blocks " + b.version + ": each blocking line above, in its own words.",
		"Do not change " + b.version + "'s code to get it through: a fix is a new version, shipped with canyon deploy.",
		"An admin or a security responder who didn't start this review may approve each blocking finding in Canyon's dashboard; neither you nor the user can. Then run this command again.",
	})
}

// earlierLines is §6.1's end: production's earlier versions, and the command that puts the newest one
// still kept back; none when production ran nothing before, and no command while production is held.
// An earlier image no patch made of the package prod runs is a rebuild of it, named apart from it (#2300).
func (a *app) earlierLines(rows []earlier, prod *env, held bool) {
	if len(rows) == 0 {
		return
	}
	said, named := make([]string, len(rows)), make([]string, len(rows))
	for i, row := range rows {
		named[i] = cell(row.Version)
		if row.Patched {
			named[i] += " patched"
		} else if prod != nil && row.Package == prod.Package {
			named[i] += " earlier build"
		}
		said[i] = named[i] + " (" + shortDay(row.DeployedAt) + ")"
	}
	back := ""
	if !held {
		back = putBackCommand(rows)
	}
	if a.render.Format == FormatAI {
		a.printf("earlier in production: %s\n", strings.Join(named, ", "))
		if back != "" {
			a.printf("put one back: %s\n", back)
		}
		return
	}
	a.printf("\n  Earlier in production  %s\n", strings.Join(said, " "+a.dot()+" "))
	if back != "" {
		a.printf("  Put one back: %s\n", back)
	}
}

// putBackCommand names the newest earlier version that --version puts back kept, "" when none is: one
// that is not kept cannot be put back as it ran, and a version shaped like none is never a command.
func putBackCommand(rows []earlier) string {
	if back := puttable(rows); len(back) > 0 {
		return "canyon " + againWith(back[0])
	}
	return ""
}
