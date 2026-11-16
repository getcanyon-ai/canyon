package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"
)

// noRebuild is D5's line, byte for byte: the one sentence that answers "what am I about to ship".
const noRebuild = "Production runs the same image as preview, with production's own secrets. Nothing is rebuilt."

// deployProd is `canyon deploy --env prod` (cli.md §6.3): the image preview runs, into prod, after
// the receipt and one question. It never sends this directory, so it first proves this directory is
// the tree preview runs, or what it ships is not what the person is looking at. Where the org has a
// person approve production, it asks nothing and the host answers with a production approval: the
// one confirmation is that person's, in the dashboard, and wait waits for it.
func deployProd(ctx context.Context, a *app, yes, wait bool, version, reason string) int {
	a.verb.name = "deploy --env prod" // this invocation's copy: every line that says what to run again names prod
	if version != "" {
		a.verb.name = againWith(version) // and the version: plain --env prod would promote preview instead
	}
	if a.render.Format == FormatAI { // §6.3: the transcript is held, and a result's block replaces it
		a.park()
		defer a.unpark(true)
	}
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	p, err := readProject(dir) // prod reads the file; writing it is a preview deploy's alone (§3.2)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	moves := " preview " + a.render.punct("→", "->") + " prod"
	if version != "" { // an earlier version goes back, and preview is not touched (design/OPERATIONS.md §1.7)
		moves = " " + cell(version) + " " + a.render.punct("→", "->") + " prod"
	}
	a.header(p.App, moves)
	if _, err := ensureProject(ctx, client, p.App, false); err != nil { // guard (a): nothing here to promote (§3.2)
		return a.fail(err)
	}
	if _, err := a.checkBase(ctx, client, dir, p.App, "prod", false); err != nil { // guard (b), before production's question (§6.6)
		return a.fail(err)
	}
	at := "/api/v1/projects/" + url.PathEscape(p.App)
	var read statusRead
	if err := client.get(ctx, at+"/status", &read); err != nil {
		return a.fail(err)
	}
	if h := read.Receipt.Held; h != nil { // held: no version goes to production, preview's or an earlier one, and nothing is sent (contracts/routes.md, The hold)
		return a.heldReceipt(ctx, client, p.App, *h)
	}
	if version != "" {
		return a.rollback(ctx, client, at, p.App, read, version, reason, yes, wait)
	}
	preview := read.Envs["preview"]
	if preview.running() != nil { // with nothing running in preview the receipt says so
		same, err := sameTree(ctx, client, at, dir, preview.Package)
		if err != nil {
			return a.fail(err)
		}
		if !same {
			a.printf("\n  Your code has changed since preview's %s. Deploy it to preview first: canyon deploy\n", a.version(preview))
			return exitError // no gate or policy answered: a directory ahead of preview is a stale base's kin (§3.5)
		}
	}
	// The person's rule failing alone is the org having a person approve production in the dashboard:
	// this terminal can only ask (contracts/routes.md, the promote receipt).
	asks := slices.Equal(read.Receipt.BlockedBy, []string{"prod.human-approval"})
	// Nothing is sent while the receipt says no: a promotion naming no run is answered 404 `no such
	// run of this project`, a worse sentence than the receipt's `no run has looked at this app`.
	if len(read.Receipt.BlockedBy) > 0 && !asks || read.Receipt.Waiting != "" || read.PromoteRun == nil {
		if run := reviewUnderWay(ctx, client, read); run != "" {
			return a.busy(promoted{app: p.App, host: client.page()}, run)
		}
		return a.refused(read.Receipt.withPatch(preview), client.page(), p.App)
	}
	// A version that answers an error on preview is not promoted from here, whatever the flags: the host
	// refuses it too, and only a person in the dashboard overrides it. Where a person approves production,
	// the approval is asked for and its page says what preview answered (contracts/routes.md, preview_answered).
	if w := preview.running(); w != nil && w.Answered != nil && w.Answered.answer().Flagged() && !asks {
		v := a.version(preview)
		return a.refusedAnswer(p.App, client.page(), v, *w.Answered, w.Answered.answer().Refused(v))
	}
	replaces, replaced := "Nothing runs in production yet.", "nothing, production ran nothing yet"
	if prod := read.Envs["prod"].running(); prod != nil { // a prod waiting on its first release replaces nothing
		replaces, replaced = "It replaces "+a.version(prod)+".", a.version(prod)
	}
	ships := a.version(preview) + cleared(read.Receipt)
	a.printf("\n  Promoting %s from preview to production. %s\n", ships, replaces) // a version, never a run id (cli.md §6.3)
	a.printf("  %s\n", a.faint(noRebuild))
	out := promoted{app: p.App, host: client.page(), version: ships, replaced: replaced, preview: preview.running().answered(), advisory: advisories(read.Receipt.withPatch(preview))}
	if out.preview != "" { // before the question, never a block: governance is the admin's (#1833)
		a.printf("  %s Preview %s; check the app before promoting.\n", a.render.glyph(stateWarn), out.preview)
	}
	out.missing, out.unchecked = a.missingInProd(ctx, client, at)
	out.ask = "Ask the user whether to put " + a.version(preview) + " in production; if they say yes, run canyon deploy --env prod --yes."
	return a.promote(ctx, client, at, read, out, *read.PromoteRun, asks, yes, wait) // the run the status read answered, never "latest" and never omitted (routes.md)
}

// promote asks, unless a person approves production in the dashboard, then sends the promotion of
// gateRun and follows it: preview's run, or a rollback review's.
func (a *app) promote(ctx context.Context, client *Client, at string, read statusRead, out promoted, gateRun string, asks, yes, wait bool) int {
	if !asks {
		if !yes && a.render.Format == FormatAI { // nothing here can answer the question: the agent asks the user (#2875)
			return a.aiUnasked(out, read.Envs["prod"].running())
		}
		if code, ok := a.confirm(yes, "Promote "+cell(out.app)+" to production?", "Nothing was promoted."); !ok {
			return code
		}
	}
	body := struct {
		GateRun string `json:"gate_run"`
	}{gateRun}
	var started struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		Approval *approvalAsked `json:"approval"`
	}
	began := a.clock.now() // the seconds a promotion took, never the ones spent before the question
	if err := client.postJSON(ctx, at+"/promotions", body, http.StatusAccepted, &started); err != nil {
		var api *APIError
		var refusal receipt
		if errors.As(err, &api) && api.Status == http.StatusConflict && json.Unmarshal(api.Extra["receipt"], &refusal) == nil {
			return a.refused(refusal, client.page(), out.app) // the refusal's own rows, never its one-line code
		}
		var version string
		var w answered
		if api != nil && api.Status == http.StatusConflict && api.Code == "preview_answered" && // preview was deployed again since the status read
			json.Unmarshal(api.Extra["version"], &version) == nil && json.Unmarshal(api.Extra["answered"], &w) == nil {
			return a.refusedAnswer(out.app, client.page(), cell(version), w, api.Message)
		}
		if api != nil && api.Status == http.StatusConflict && api.Code == "run_in_progress" && out.putBack == "" { // a rollback's promotion keeps the host's line on purpose: its 409 is its own review still under way, and rollback.go meets another run at the review (cli.md §6.3.1)
			return a.busy(out, runNamed(api.Extra["run"]))
		}
		return a.fail(err)
	}
	if started.Approval != nil {
		return a.awaitApproval(ctx, client, at, read, out, *started.Approval, wait)
	}
	if asks { // the switch went off between the status read and the promotion: the host promoted, and no question was asked
		a.printf("\n  This org stopped having a person approve production while this ran, so production was promoted directly.\n")
	}
	return a.followPromotion(ctx, client, at, read, out, started.Run.ID, began)
}

// followPromotion follows the promote run the host started to its end: live, failed, or a terminal
// that stopped following while the promotion keeps going.
func (a *app) followPromotion(ctx context.Context, client *Client, at string, read statusRead, out promoted, run string, began time.Time) int {
	if !validRun.MatchString(run) { // naming no run, or one this CLI will not put in a URL
		return a.fail(&NotCanyon{Host: client.Base, Status: http.StatusAccepted})
	}
	out.run = run
	a.printf("\n  %s\n", a.render.paint(ansiDim, "Promote"))
	var last string
	var live *deployed
	if _, err := poll(ctx, client, run, a.clock, deployCeiling, func(row timelineRow) {
		switch row.Kind { // the run's own lines, each as the server writes it: a resource, the start, live at
		case "build.log":
			var logged struct {
				Line string `json:"line"`
			}
			if json.Unmarshal(row.Data, &logged) == nil {
				last = logged.Line
				a.printf("  %s\n", a.render.paint(ansiDim, a.render.punct("│", "|")+" "+cell(last)))
			}
		case "deploy.completed":
			_ = json.Unmarshal(row.Data, &live) // unreadable reads as not live: the run's last line says what happened
		}
	}); err != nil {
		if code, still := a.promotionStillRunning(out, err); still { // production changes when it ends (#2730)
			return code
		}
		code := a.fail(err)
		a.errf("\n  The promotion keeps going without this terminal:\n  %s/#/apps/%s\n", client.page(), cell(out.app)) // its page, never its run id
		return code
	}
	if live == nil {
		out.last = cmp.Or(last, "the promotion ended before production changed")
		if a.render.Format == FormatAI {
			return a.aiPromoteFailed(out)
		}
		a.printf("\n  Nothing was promoted.\n")
		return a.fail(errors.New(out.last))
	}
	out.live = live
	out.access = a.promotedAccess(ctx, client, at, read) // who can open what just went live, as a preview deploy says it (#1679)
	if a.render.Format == FormatAI {
		return a.aiPromoted(out)
	}
	a.printf("\n")
	a.stage("Promote", "prod")
	a.printf("\n")
	a.liveLine(*live) // the surface the run recorded, never one this binary builds
	a.link("Fleet", client.page()+"/#/apps/"+cell(out.app))
	if out.access != "" {
		a.printf("  %-6s %s\n", "Access", out.access) // beside Live and Fleet, as they are printed
	}
	if out.keeps != "" { // a rollback leaves preview on what it took out of production (#2787)
		a.printf("\n  Preview still runs %s, the version this rollback took out of production: fix it there with canyon deploy first, or the next canyon deploy --env prod ships it again.\n", cell(out.keeps))
	}
	a.printf("\n  %s\n", a.faint("Done in "+since(a.clock.now().Sub(began))))
	return exitOK
}

// promotedAccess is status's access line under a promotion: the project read's access and whether
// anyone signs in at all. Production is live by now, so a read that fails says why and ends nothing.
func (a *app) promotedAccess(ctx context.Context, client *Client, at string, read statusRead) string {
	var project struct {
		Project struct {
			Access *teamAccess `json:"access"`
		} `json:"project"`
	}
	var auth authConfig
	if err := client.get(ctx, at, &project); err != nil {
		a.fail(err)
		return ""
	}
	if err := client.get(ctx, "/auth/config", &auth); err != nil {
		a.fail(err)
		return ""
	}
	read.Access, read.SignInOff = project.Project.Access, auth.Auth == "off"
	return a.accessLine(read)
}

// missingInProd names each runtime value set for preview and not for production (#1698): production
// runs preview's image with its own secrets, so one set only for preview is missing there. A value a
// declared resource gives is made for production by the promotion itself, and a build value is in the
// image already. Said before the question, never a refusal: production may lack one on purpose. It
// answers the names it said, or why it could not check.
func (a *app) missingInProd(ctx context.Context, client *Client, at string) (missing []string, unchecked string) {
	var read secretsList
	if err := client.get(ctx, at+"/secrets", &read); err != nil {
		a.printf("  %s Couldn't check production's secrets: %s\n", a.render.glyph(stateWarn), cell(err.Error()))
		return nil, cell(err.Error())
	}
	prod := map[string]bool{}
	for _, row := range read.Secrets {
		if row.Env == "prod" && row.Where == "runtime" {
			prod[row.Name] = true
		}
	}
	for _, row := range read.Secrets {
		if row.Env == "preview" && row.Where == "runtime" && row.Origin == "user" && !prod[row.Name] {
			a.printf("  %s %s is set for preview but not production. Set it with: canyon secret set %s --env prod\n", a.render.glyph(stateWarn), cell(row.Name), cell(row.Name))
			missing = append(missing, cell(row.Name))
		}
	}
	return missing, ""
}

// sameTree is whether dir is the tree of pkg, the package preview runs: this directory's hash
// (tree.go) against pkg's tree_hash, which only the packages list carries (contracts/routes.md).
func sameTree(ctx context.Context, client *Client, at, dir, pkg string) (bool, error) {
	mine, err := treeHash(dir)
	if err != nil {
		return false, err
	}
	query := url.Values{"limit": {"200"}}
	for {
		var page struct {
			Packages []struct {
				ID       string `json:"id"`
				TreeHash string `json:"tree_hash"`
			} `json:"packages"`
			Next *string `json:"next"`
		}
		if err := client.get(ctx, at+"/packages?"+query.Encode(), &page); err != nil {
			return false, err
		}
		for _, row := range page.Packages {
			if row.ID == pkg {
				return row.TreeHash == mine, nil
			}
		}
		if page.Next == nil {
			return false, fmt.Errorf("the host lists no package %s, which it says preview runs", cell(pkg))
		}
		query.Set("cursor", *page.Next)
	}
}

// egressAllowed is the go-live rule on what preview connects to (contracts/routes.md, The promote receipt).
const egressAllowed = "prod.egress-allowed"

// readsNoRun are the go-live rules a review under way can't change the answer of.
var readsNoRun = []string{"prod.human-approval", "stack.blessed", "owner.required", "platform.known-vuln", egressAllowed}

// reviewUnderWay is " (<run>)" when the receipt blocks only because the review of preview's own package,
// promote_run, is still pending or running — a teammate's canyon check of the same code — so the
// promotion is refused as the promotions route's 409 run_in_progress is, never as a blocking finding
// (cli.md §6.3, #2203). Every rule the receipt names must read that run: readsNoRun's read none, so any
// of them keeps the receipt. "" otherwise, and when the run's own read can't say: the receipt refuses in
// its own words.
func reviewUnderWay(ctx context.Context, client *Client, read statusRead) string {
	if read.Receipt.Waiting != "" || read.PromoteRun == nil || !validRun.MatchString(*read.PromoteRun) {
		return ""
	}
	for _, rule := range read.Receipt.BlockedBy {
		if slices.Contains(readsNoRun, rule) {
			return ""
		}
	}
	var got struct {
		Run struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"run"`
	}
	if client.get(ctx, "/api/v1/gate-runs/"+*read.PromoteRun, &got) != nil || got.Run.ID != *read.PromoteRun || got.Run.State != "pending" && got.Run.State != "running" {
		return ""
	}
	return " (" + got.Run.ID + ")"
}

// refused is the ending both refusals share — the one at rest and the one that raced a newer run.
func (a *app) refused(r receipt, host, name string) int {
	if a.render.Format == FormatAI {
		return a.aiPromoteBlocked(r, host, name)
	}
	a.checklist(r)
	a.printf("\n  Nothing was promoted.\n")
	a.history(host, name)
	return exitBlocked
}

// refusedAnswer is the refusal of a version whose preview answered an error (cli.md §6.3): the sentence and
// the app's page, where a person can promote it anyway; under ai, a block whose steps stop the agent.
func (a *app) refusedAnswer(name, host, version string, w answered, message string) int {
	page, message := host+"/#/apps/"+cell(name), cell(message) // the host's words, or an answer the host sent
	if a.render.Format == FormatAI {
		a.aiPromoteHead("REFUSED", name, host)
		a.printf("version: %s\npreview answered: %s\noutcome: %s\nfleet url: %s\n", version, aiAnswered(w), message, cell(page))
		a.printf("\n%s\nexit code: %d\n", aiRefusedSteps, exitBlocked)
		return exitBlocked
	}
	a.printf("\n  %s\n  %s\n", message, page)
	return exitBlocked
}

// busy is the promotion refused while another run of the app is under way (cli.md §6.3, #2200): that run
// named, and what to run once it has finished; under ai, a block whose steps say the same.
func (a *app) busy(out promoted, run string) int {
	if a.render.Format != FormatAI {
		return a.fail(fmt.Errorf("another run of %s is under way%s; let it finish, then run canyon %s again", cell(out.app), run, a.verb.name))
	}
	a.aiPromoteHead("REFUSED", out.app, out.host)
	a.printf("outcome: nothing was promoted: another run of %s is under way%s.\nfleet url: %s\n", cell(out.app), run, cell(out.host+"/#/apps/"+out.app))
	a.printf("\nNEXT STEPS:\n1. Tell the user the outcome line, in its own words.\n"+
		"2. Let that run finish, then run canyon %s again. Do not cancel it, and do not look for another way to promote.\n", a.verb.name)
	a.printf("\nexit code: %d\n", exitError)
	return exitError
}

// confirm is §6.3's question, asked by every verb that changes production. Nothing outward-facing
// happens on a default, so a machine that cannot answer is refused by the name of the flag that
// answers for it, never answered for.
func (a *app) confirm(yes bool, question, declined string) (int, bool) {
	switch {
	case yes:
		return exitOK, true
	case !a.canPrompt():
		return a.fail(fmt.Errorf("%s asks before it changes production, and nothing here can answer: re-run it with --yes", a.verb.name)), false
	}
	a.errf("\n  %s [y/N] ", question)
	if !confirmed(a.in) {
		a.printf("\n  %s\n", declined)
		a.declined = true
		return exitError, false
	}
	return exitOK, true
}
