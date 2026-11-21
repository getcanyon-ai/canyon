package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// forceUsage is what --force means on deploy: not a suppression, the gate still runs; it writes over
// a teammate's package (cli.md §3.6).
const forceUsage = "Upload even if someone deployed a newer version since you last pulled"

func runDeploy(ctx context.Context, a *app, args []string) int {
	var named, env, reason string
	var force, yes, wait, create bool
	var reports reportFiles
	var earlier versionFlag
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&named, "app", "", "The app id a first deploy writes into "+projectFile)
		fs.StringVar(&env, "env", "preview", "preview (default) or prod. prod promotes what preview runs, with no rebuild")
		fs.BoolVar(&create, "create", false, "Make the app "+projectFile+" names on a host that doesn't have it")
		fs.BoolVar(&force, "force", false, forceUsage)
		fs.Var(&reports, "sarif", sarifUsage)
		fs.BoolVar(&yes, "yes", false, "Promote without asking (for CI)")
		fs.BoolVar(&wait, "wait", false, "Where a person approves production, wait for their answer")
		fs.Var(&earlier, "version", "With --env prod: put an earlier version back in production (canyon status lists them)")
		fs.StringVar(&reason, "reason", "", "With --version: why, an incident or change reference the audit trail records")
	})
	if err != nil && err.Error() == "flag needs an argument: -version" { // --version with nothing after it
		return a.usage(errors.New(versionUsage))
	}
	if err != nil {
		return a.usage(err)
	}
	switch {
	case earlier.given && (env != "prod" || earlier.name == "" || strings.HasPrefix(earlier.name, "-")): // a flag after it is no version
		return a.usage(errors.New(versionUsage))
	case len(rest) > 0:
		return a.usage(fmt.Errorf("deploy takes no arguments, and got %q", rest[0]))
	case envNames[env] == "":
		return a.usage(fmt.Errorf("deploy takes --env preview or --env prod, and got %q", env))
	case reason != "" && !earlier.given:
		return a.usage(errors.New("--reason says why an earlier version goes back: canyon deploy --env prod --version <version> --reason <why>"))
	case env == "preview" && yes:
		return a.usage(errors.New("--yes answers the question deploy --env prod asks, and a preview deploy asks none"))
	case env == "preview" && wait:
		return a.usage(errors.New("--wait waits for a person to approve deploy --env prod, and a preview deploy waits on nobody"))
	case env == "prod" && (named != "" || create || force || len(reports) > 0):
		return a.usage(errors.New("deploy --env prod ships what preview runs and sends no tree, so --app, --create, --force and --sarif change nothing"))
	case env == "prod" && a.render.Format == FormatJSON:
		return a.usage(errors.New("deploy --env prod has no json rendering: use --format ai, which prints its result for an agent")) // json is CI's (cli.md §5.4); an agent reached for it (#2787)
	case env == "prod":
		return deployProd(ctx, a, yes, wait, earlier.name, reason)
	}
	a.park() // after parse, so --help prints in every format; every return unparks
	defer a.unpark(true)
	var dir string
	var p project
	if a.ref != "" { // on /mcp: the app the tool names, and no directory
		p, err = a.project()
	} else if dir, err = a.getwd(); err == nil {
		p, err = readProject(dir)
	}
	var missing *NoProjectFile
	switch {
	case errors.As(err, &missing): // deploy is the verb that makes the app and writes the file check only reads (§3.2)
	case err != nil:
		return a.usage(err)
	case named != "": // a flag that changes nothing is a typo, not a preference
		return a.usage(fmt.Errorf("%s already names the app %q, so --app would change nothing: edit the file instead", projectFile, p.App))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	if missing != nil {
		if p, err = a.setup(ctx, client, dir, named, missing.NoApp); err != nil {
			return a.fail(err)
		}
	}
	began := time.Now()
	run := checkRun{App: p.App, Host: client.page(), Force: force, Reports: reports, Create: create || missing != nil}
	a.header(run.App, " "+a.render.punct("→", "->")+" preview")
	if err := a.packaged(ctx, client, dir, &run); err != nil {
		code := a.fail(err)
		a.askS3(ctx, client, dir, err)
		if _, theirs := refusal(err, http.StatusForbidden, "team_required"); theirs && missing == nil { // canyon.toml named it: copied from someone else's, most likely (§9)
			a.errf("  To make your own app instead, delete the app line from %s and deploy again.\n", projectFile)
		}
		return code
	}
	started, err := startRun(ctx, client, run.App, run.Package, "deploys")
	if err != nil {
		return a.fail(err)
	}
	each, stop := a.narrate(&run)
	if a.render.Format == FormatHuman {
		each = a.reviewNow(ctx, client, started, &run, each)
	}
	seen, err := a.poll(ctx, client, started, deployCeiling, each)
	stop()
	if code, still := a.stillRunning(run, started, seen, err); still {
		return code
	}
	if err != nil {
		code := a.fail(err)
		a.errf("\n  The deploy keeps going without this terminal:\n  %s/#/apps/%s\n", run.Host, run.App) // its page, never its run id
		return code
	}
	if seen.Unknown > 0 {
		a.dim("%s this build does not understand", plural(seen.Unknown, "row", "rows"))
	}
	var read struct {
		Run runResult `json:"run"`
	}
	if err := a.readRun(ctx, client, started, &read); err != nil {
		if code, unread := a.endedUnread(run, started, seen, "deploy", err); unread {
			return code
		}
		return a.fail(err)
	}
	run.Run, run.StartedAt, run.Elapsed, run.Result = read.Run.ID, seen.StartedAt, time.Since(began), read.Run
	// §5.3's access header under ai, and §3.1's source line after a deploy to preview
	sourced := run.Deploy != nil && run.Deploy.Env == "preview" && (a.render.Format == FormatAI || a.render.Format == FormatHuman)
	if run.Deploy != nil && a.render.Format == FormatAI || sourced {
		var read struct {
			Project struct {
				Access     *teamAccess `json:"access"`
				Repository *repoSource `json:"repository"`
			} `json:"project"`
		}
		if err := client.get(ctx, "/api/v1/projects/"+url.PathEscape(run.App), &read); err != nil {
			a.fail(err) // the app is live: the reason goes to stderr, and the block still prints, without the header
		}
		if sourced {
			run.Source = read.Project.Repository
		}
		if a.render.Format == FormatAI {
			run.Team = read.Project.Access
			off, err := a.signInOff(ctx, client) // where nobody signs in, the team says nothing about who opens the app (#2019)
			if err != nil {
				a.fail(err)
			}
			run.SignInOff = off
		}
		if a.render.Format == FormatAI && run.Result.State == "clean" && run.Deploy.Env == "preview" { // §5.3: what production's checklist will say of this version
			var status statusRead
			if err := client.get(ctx, "/api/v1/projects/"+url.PathEscape(run.App)+"/status", &status); err != nil {
				a.fail(err) // the app is live: the step to production prints without the rules
			} else {
				run.Prod = &status.Receipt
			}
		}
	}
	a.appRules(ctx, client, &run)
	return a.verdict(run)
}

// reviewNow prints the review's stage row as the verdict row arrives, from the run read then, rather than
// once the app is live: a deploy's build can take many minutes — the first on a Docker host waits while the
// executor builds canyon-builder — and the review is done before it starts (cli.md §3.1; #2697). A read that
// fails, or a verdict that argues with its findings, prints nothing here, and the end prints the review as before.
func (a *app) reviewNow(ctx context.Context, client *Client, id string, run *checkRun, each func(timelineRow)) func(timelineRow) {
	return func(row timelineRow) {
		each(row)
		if row.Kind != "verdict" || run.Reviewed {
			return
		}
		var read struct {
			Run runResult `json:"run"`
		}
		if client.get(ctx, "/api/v1/gate-runs/"+url.PathEscape(id), &read) != nil {
			return
		}
		now := *run
		now.Result = read.Run
		if a.disagrees(now) != "" {
			return
		}
		a.reviewStage(now)
		run.Reviewed = true
	}
}

// narrate holds each check's and each producer's row as it resolves, for the stage row the verdict
// prints above them, and records the deploy. The build's lines are held, not printed: a
// build that passes says nothing worth reading, and a failed one prints them (buildLog, cli.md
// §3.1). On a terminal the spinner says which phase waits and for how long; stop ends it. The span
// is the contract's own clock, first build.log ts to deploy.completed ts (contracts/routes.md).
func (a *app) narrate(run *checkRun) (func(timelineRow), func()) {
	var first string
	building := false           // the verdict let the build go on: the spinner says Build from then, not from the build's first line
	waited := map[string]bool{} // each step whose wait was said off a terminal
	gate := gateRows{a: a, run: run, done: map[string]gateDone{}}
	spin := a.spinner()
	if spin == nil { // off a terminal the review is one line as it starts; the build's heading waits for its log (buildLog)
		a.printf("\n  %s\n", a.render.paint(ansiDim, "Review"))
		run.ReviewHeader = true
	}
	spin.phase("Review")
	return func(row timelineRow) {
		switch row.Kind {
		case "check.started", "check.completed":
			var d gateDone
			if json.Unmarshal(row.Data, &d) != nil || d.Check == "" {
				return
			}
			gate.row(row.Kind == "check.completed", d)
		case "producer.sarif":
			var logged producerLog
			if json.Unmarshal(row.Data, &logged) != nil {
				return
			}
			for _, n := range logged.notes() {
				run.Notes = append(run.Notes, n)
				run.Rows = append(run.Rows, func() {
					if !gate.saysOff(n) && !saidOnPackage(run.Dockerfile, n) && !gate.foldsUnder(n) {
						a.note(n)
					}
				})
			}
		case "scan.waiting": // a tool waits on its database: the spinner's phase says so, off a terminal one dim line
			var w struct {
				Tool string `json:"tool"`
				Note string `json:"note"`
			}
			if json.Unmarshal(row.Data, &w) != nil || w.Tool == "" {
				return
			}
			if said := cell(w.Tool + ": " + w.Note); spin != nil {
				spin.phase("Review " + a.dot() + " " + said)
			} else {
				a.printf("    %s\n", a.render.paint(ansiDim, a.dot()+" "+said))
			}
		case "progress": // a scan or the build waiting its turn says where it stands: the spinner's phase, off a terminal one dim line, the first time a step waits
			var p progressRow
			if json.Unmarshal(row.Data, &p) != nil {
				return
			}
			run.Built = run.Built || p.Step == "build" // every build reports its progress (contracts/events.md)
			if p.Step == "start" && p.Status == "running" {
				run.AppLines = len(run.Build) // what the start writes from here is the app's and why it stopped; a start after a rebuild begins again
			}
			said := p.words()
			switch {
			case said == "":
			case spin != nil:
				spin.phase(map[bool]string{true: "Build", false: "Review"}[p.Step == "build"] + " " + a.dot() + " " + cell(said))
			case p.Status == "queued" && !waitSaid(waited, p):
				a.printf("    %s\n", a.render.paint(ansiDim, a.dot()+" "+cell(said)))
			}
		case "verdict":
			var v struct {
				State    string            `json:"state"`
				Approved []json.RawMessage `json:"approved"`
				Findings struct {
					Runs []sarifRun `json:"runs"`
				} `json:"findings"`
			}
			if json.Unmarshal(row.Data, &v) != nil {
				return
			}
			if a.verb.name == "deploy" && run.Rollback == nil && (v.State == "clean" || len(v.Approved) > 0) { // a deploy builds next, and the review is done
				building = true
				spin.phase("Build")
			}
			for _, r := range v.Findings.Runs {
				if tool, imported := strings.CutPrefix(r.Tool.Driver.Name, "sarif/"); imported {
					run.Rows = append(run.Rows, func() { a.toolLine(tool, r) })
					for _, text := range r.missed() { // its producer row is a summary: what it could not read is said here alone
						run.Notes = append(run.Notes, producerNote{Producer: r.Tool.Driver.Name, Text: text})
					}
				}
			}
		case "build.image": // the run waited for Canyon to build its own images before the build wrote a line (#2711)
			var waited struct {
				Ms int64 `json:"waited_ms"`
			}
			if json.Unmarshal(row.Data, &waited) == nil && waited.Ms > 0 {
				run.Prepared = time.Duration(waited.Ms) * time.Millisecond
			}
		case "build.log":
			var logged struct {
				Line string `json:"line"`
			}
			if json.Unmarshal(row.Data, &logged) != nil {
				return
			}
			if run.Deploy != nil { // after deploy.completed the rows say who can open the app, not what the build did
				run.Access = append(run.Access, logged.Line)
				return
			}
			if first == "" {
				first = row.TS
				if !building {
					spin.phase("Build")
				}
			}
			run.Build = append(run.Build, logged.Line)
		case "deploy.completed":
			var done deployed
			if json.Unmarshal(row.Data, &done) == nil {
				run.Deploy, run.BuildFor = &done, between(first, row.TS)
			}
			spin.phase("")
		}
	}, func() { spin.stop(a) }
}

// buildLog prints the lines narrate held, now that the build or the app failed: under Build, colour
// codes stripped, because spelled out they read as `\x1b[34m` (cli.md §3.1).
func (a *app) buildLog(run checkRun) {
	if len(run.Build) == 0 {
		return
	}
	a.printf("\n  %s\n", a.render.paint(ansiDim, "Build"))
	for _, line := range run.Build {
		a.printf("  %s\n", a.render.paint(ansiDim, a.render.punct("│", "|")+" "+cell(ansi.Strip(line))))
	}
}

// producerLog is the producer.sarif row (contracts/events.md): who produced it and its log, or for an
// imported tool its report's category and normalised results, with no log. Only the count and what the
// producer said about its own run are read; a finding is the verdict's to show.
type producerLog struct {
	Producer string    `json:"producer"`
	Category string    `json:"category"`
	Results  []finding `json:"results"`
	SARIF    struct {
		Runs []sarifRun `json:"runs"`
	} `json:"sarif"`
}

// sarifRun is one run of a SARIF log: who produced it, what it said about its own run, and its results.
type sarifRun struct {
	Tool struct {
		Driver struct {
			Name string `json:"name"`
		} `json:"driver"`
	} `json:"tool"`
	AutomationDetails struct {
		ID string `json:"id"`
	} `json:"automationDetails"`
	Invocations []struct {
		Notifications []struct {
			Level      string    `json:"level"`
			Message    sarifText `json:"message"`
			Properties struct {
				Canyon struct {
					Why string `json:"why"`
				} `json:"canyon"`
			} `json:"properties"`
		} `json:"toolExecutionNotifications"`
	} `json:"invocations"`
	Results []finding `json:"results"`
}

// missed is what the run said it did not check, in the words the server reads a run's not_checked by
// (contracts/finding.md, What nothing read is named).
func (r sarifRun) missed() []string {
	return slices.DeleteFunc(r.said(), func(text string) bool { return !strings.Contains(text, "not checked") })
}

func (r sarifRun) said() []string {
	var said []string
	for _, inv := range r.Invocations {
		for _, n := range inv.Notifications {
			said = append(said, n.Message.Text)
		}
	}
	return said
}

// producerNote is one thing a producer said about its own run; --format json prints it as is.
// Aside says it is a note (SARIF's level), how the review ran rather than what it could not read,
// which the ai block leaves to a person (cli.md §5.2).
type producerNote struct {
	Producer string `json:"producer"`
	Text     string `json:"text"`
	Aside    bool   `json:"-"`
	Why      string `json:"-"` // a coverage note's reason (properties.canyon.why), which saysOff reads
}

func (l producerLog) notes() []producerNote {
	var notes []producerNote
	for _, r := range l.SARIF.Runs {
		for _, inv := range r.Invocations {
			for _, n := range inv.Notifications {
				notes = append(notes, producerNote{Producer: l.Producer, Text: n.Message.Text, Aside: n.Level == "note", Why: n.Properties.Canyon.Why})
			}
		}
	}
	return notes
}

// note is what a producer said about its own run, one dim row in its own words: what nothing read,
// or a review that did not run (EXPERIENCE.md D10). A row of the list, never indented under the row
// above, which it is not about. A producer's results are the rows' and the verdict's to count, so
// it repeats none (cli.md §3.1).
func (a *app) note(n producerNote) {
	a.printf("    %s\n", a.render.paint(ansiDim, a.render.glyph(statePending)+" "+cell(n.Text)))
}

// dockerfileNote is the Package row's line for a Dockerfile at the package root, and the scanners' run's
// note in the same words (contracts/finding.md, #1243).
const dockerfileNote = "Canyon builds with buildpacks and does not read this Dockerfile"

// saidOnPackage reports whether n is the scanners' Dockerfile note the Package row already said, so
// the review's rows do not say it twice.
func saidOnPackage(dockerfile bool, n producerNote) bool {
	return dockerfile && n.Producer == "canyon/scanners" && n.Text == dockerfileNote
}

// shipped is true only when this verb deployed and the run's rows say so: the row alone is not
// a deploy, and an exit read off it would not know that.
func (a *app) shipped(run checkRun) bool { return a.verb.name == "deploy" && run.Deploy != nil }

// deployRows are §3.1's two stage rows, printed from the rows the poll saw. The deploy is
// untimed because no row starts it, and inventing one would be a second vocabulary.
func (a *app) deployRows(run checkRun) {
	if !a.shipped(run) {
		return
	}
	took := ""
	if run.BuildFor > 0 { // a timing only when both of the contract's timestamps read
		took = since(run.BuildFor + run.Prepared)
		if run.Prepared > 0 { // the wait came before the build's first line, so its clock leaves it out (cli.md §3.1, #2711)
			took += ", " + since(run.Prepared) + " of it preparing Canyon's images"
		}
		took = a.render.paint(ansiDim, "["+took+"]")
	}
	if v, ok := reusedOf(run); ok {
		took = strings.TrimSpace("reused the image built for " + v + "  " + took)
	}
	a.printf("\n")
	a.stage("Build", took)
	a.stage("Deploy", cell(run.Deploy.Env))
}

// reused is the build.log line a deploy that reuses an image writes in place of a build's.
var reused = regexp.MustCompile(`^Reusing the image built for (.+) \(no build value changed\)$`)

// reusedOf is the version whose image a deploy reused: its first build.log line, the contract's exact
// words (contracts/events.md, build.log), made safe to print — none when the build ran after all, the
// image being gone; a declared resource's lines may follow it.
func reusedOf(run checkRun) (string, bool) {
	if len(run.Build) == 0 || run.Built {
		return "", false
	}
	m := reused.FindStringSubmatch(run.Build[0])
	if m == nil {
		return "", false
	}
	return cell(m[1]), true
}

func (a *app) stage(label, value string) {
	row := fmt.Sprintf("  %s %-11s%s", a.render.paint(ansiGreen, a.render.glyph(statePass)), label, value)
	a.printf("%s\n", strings.TrimRight(row, " "))
}

// liveURLs are §3.1's two URLs, off the deploy.completed row: the live app, and the fleet page
// that is how a CLI user finds out the dashboard exists.
func (a *app) liveURLs(run checkRun) {
	if !a.shipped(run) {
		return
	}
	a.printf("\n")
	a.liveLine(*run.Deploy)
	a.link("Fleet", run.Host+"/#/apps/"+run.App)
	for _, line := range run.Access {
		a.printf("  %s\n", cell(line))
	}
	if said, why := run.Source.words(); said != "" {
		a.printf("  %s\n", said)
		if why != "" {
			a.dim("%s", why)
		}
	}
}

// repoSource is the project read's repository (contracts/routes.md, Kept in GitHub): where the app's code lives.
type repoSource struct {
	FullName  string `json:"full_name"`
	Folder    string `json:"folder"`
	State     string `json:"state"`
	Keeps     bool   `json:"keeps"`
	KeptError string `json:"kept_error"`
}

// words is cli.md §3.1's source line and the kept_error under it, as the host words it; "" for an app no repository feeds.
func (r *repoSource) words() (string, string) {
	switch {
	case r == nil:
		return "", ""
	case r.State == "pending":
		return "source: kept by Canyon in GitHub · the repository isn't made yet", cell(r.KeptError)
	case r.Keeps:
		return "source: kept by Canyon in github.com/" + cell(r.FullName), cell(r.KeptError)
	case r.Folder != "":
		return "source: github.com/" + cell(r.FullName) + " · " + cell(r.Folder), ""
	}
	return "source: github.com/" + cell(r.FullName), ""
}

// liveLine is the Live line, and what the app answered at / beside it: a fact, never a verdict, since a 401
// or a 302 there can be right, so it is neither red nor a warning; the body's first line under it.
func (a *app) liveLine(d deployed) {
	if d.Answered == nil {
		a.link("Live", d.URL)
		return
	}
	a.printf("  %-6s %s · %s\n", "Live", a.render.paint(ansiCyan+ansiUnderline, cell(d.URL)), cell(d.Answered.words()))
	if d.Answered.Said != "" {
		a.printf("  %-6s %s\n", "", cell(d.Answered.Said))
	}
}

// link is a URL and its label: cyan and underlined as the one thing on screen to click, and
// escaped, because the live URL is a string the host sent.
func (a *app) link(label, url string) {
	a.printf("  %-6s %s\n", label, a.render.paint(ansiCyan+ansiUnderline, cell(url)))
}

// between is the gap between two of the contract's timestamps, and nothing when either is not
// one: a time this build cannot read gets no timing rather than a wrong one.
func between(from, to string) time.Duration {
	began, fromErr := time.Parse(time.RFC3339, from)
	ended, toErr := time.Parse(time.RFC3339, to)
	if fromErr != nil || toErr != nil || ended.Before(began) {
		return 0
	}
	return ended.Sub(began)
}

// progressRow is a progress row (contracts/events.md, progress): a scan's or the build's or the start's
// status as it changes.
type progressRow struct {
	Step     string `json:"step"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	Position int    `json:"position"`
}

func (p progressRow) key() string { return p.Step + "/" + p.Tool }

// waitSaid is whether the step's wait was already said off a terminal, noting it when it was not: a
// step's first place in line is its one line, and a later place moves nothing (cli.md §3.1).
func waitSaid(waited map[string]bool, p progressRow) bool {
	if waited[p.key()] {
		return true
	}
	waited[p.key()] = true
	return false
}

// words is what a person reads while a step waits or runs: "trivy: waiting for a scanner slot (2nd in
// line)", "trivy: running", "waiting for a build slot"; "" for an end, which the review's or the
// build's own lines say.
func (p progressRow) words() string {
	who, slot := p.Tool+": ", "a scanner slot"
	switch p.Step {
	case "build":
		who, slot = "", "a build slot"
	case "start":
		who = "starting the app: "
	}
	switch p.Status {
	case "queued":
		if p.Position > 0 {
			return who + "waiting for " + slot + " (" + ordinal(p.Position) + " in line)"
		}
		return who + "waiting for " + slot
	case "running":
		if p.Step == "scan" {
			return who + "running"
		}
	}
	return ""
}

// ordinal is n as a place in line: 1st, 2nd, 3rd, 4th, 11th, 21st.
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}
