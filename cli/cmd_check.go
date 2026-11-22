package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	answers "github.com/getcanyon-ai/canyon/internal/answered"
)

// runResult is GET /api/v1/gate-runs/:id, the contracted state read a verdict comes from — not
// the timeline's verdict row, which is the audit copy of the same thing.
type runResult struct {
	ID            string    `json:"id"`
	Project       string    `json:"project"` // the run's app
	Deploys       bool      `json:"deploys"` // a deploy started it, not a check
	Package       string    `json:"package"`
	State         string    `json:"state"`
	FailureReason string    `json:"failure_reason"`
	FailedIn      string    `json:"failed_in"`   // the step a run Canyon itself failed in (contracts/routes.md, A failure reason in words)
	NotChecked    bool      `json:"not_checked"` // the review names what it did not read: never "Passed"
	Builds        []string  `json:"builds"`      // a failed detect's: what the host builds (contracts/routes.md, the run read)
	Errored       bool      `json:"errored"`     // a failed detect's: a buildpack errored while detecting
	Fix           *runFix   `json:"fix"`         // a failed detect's or start's: the file a stock starter is missing
	Blocking      int       `json:"blocking"`
	Warnings      int       `json:"warnings"`
	Findings      []finding `json:"findings"`
}

// runFix is the one file a stock starter is missing, and what to put in it (contracts/routes.md,
// What a starter is missing).
type runFix struct {
	File     string `json:"file"`
	Why      string `json:"why"`
	Examples []struct {
		For  string `json:"for"`
		Text string `json:"text"`
	} `json:"examples"`
}

// checkRun is everything one canyon check learned, and the one value a renderer takes.
type checkRun struct {
	App, Host, Run, StartedAt string
	Force                     bool     // deploy --force: send this tree even if the app's current package is not this copy's base
	Create                    bool     // deploy --create, or §3.2's setup: an app this host doesn't have is made, never refused
	Base                      string   // the package .canyon/base names for this app, which a deploy's upload sends; "" for none
	Reports                   []string // --sarif: the reports to attach to the package before the run
	Package                   string   // the package the upload named: the one the reports attach to and the run reviews
	Files                     int
	Bytes                     int64
	Elapsed                   time.Duration
	BuildFor                  time.Duration // the build's own span, nothing when the run did not build
	Prepared                  time.Duration // the wait for Canyon's images before it, its build.image row's (#2711)
	Deploy                    *deployed     // the deploy.completed row, nothing when nothing was deployed
	Access                    []string      // the build.log rows after it: who can open the app, in the server's words (contracts/events.md)
	Build                     []string      // the build.log rows before it, held until the build or the app fails (cli.md §3.1)
	AppLines                  int           // the index in Build of the start step's first line: the app's own, then why it stopped (cli.md §5.2)
	Built                     bool          // a progress row of the build step came: pack ran, whatever an earlier line said
	NotPacked                 []string      // the paths the package left home, git's record aside: a failed build names them (cli.md §9)
	Dockerfile                bool          // the package root holds a Dockerfile, said to be unread once (cli.md §3.1)
	ReviewHeader              bool          // whether Review printed as its phase started: off a terminal, where no spinner names it
	Reviewed                  bool          // whether the review's stage row printed as its verdict arrived, before the build (reviewNow)
	Team                      *teamAccess   // the project read's access, read only for §5.3's ai header
	SignInOff                 bool          // the host's /auth/config said nobody signs in, read with Team
	Prod                      *receipt      // the status read's promote receipt, read only for §5.3's step to production
	Source                    *repoSource   // the project read's repository, read only after a deploy to preview (§3.1's source line)
	AppRules                  []appAddition // the app's additions that block where the org's state does not, read only for §5.2's blocked block
	Rollback                  *putBack      // deploy --env prod --version: the version a rollback review puts back; nil on any other run
	Result                    runResult
	Notes                     []producerNote  // what each producer said about its own run, in the order they arrived
	Rows                      []func()        // the review's rows as they resolved, held for the stage row the verdict prints above them
	SARIF                     json.RawMessage // GET /gate-runs/:id/sarif, read only under --format sarif and only on a verdict
}

// deployed is the deploy.completed row (contracts/events.md). It is the only place a run learns
// that an app is live, so whatever reads it reads it there and never from a second request.
// `canyon check` never builds, so its runs carry none.
type deployed struct {
	Env         string    `json:"env"`
	URL         string    `json:"url"`
	Package     string    `json:"package"`
	ImageDigest string    `json:"image_digest"`
	Answered    *answered `json:"answered"` // absent on a row written before, or when the deploy started nothing
}

// answered is what the app answered Canyon's one GET / of its url after the deploy (contracts/routes.md,
// envs.<env>.answered): a status and its body's first line, or why nothing answered.
type answered struct { // either {status, said} or {error}, and encoded back so: status's json carries it (§5.4)
	Status int    `json:"status,omitempty"`
	Said   string `json:"said,omitempty"`
	Error  string `json:"error,omitempty"`
}

// notOK is " · <words>" for an answer to say beside readiness (contracts/routes.md, envs.<env>.answered):
// nothing answered, or a status outside 2xx and 3xx but 401 and 404, which can be right at /. Said beside
// the step to production, so it never reads clean beside a 400; nothing blocks. "" otherwise.
func (a *app) notOK(w *answered) string {
	if said := w.beside(); said != "" {
		return " " + a.dot() + " " + said
	}
	return ""
}

// beside is the words of an answer to say beside readiness, "" for any other or none.
func (w *answered) beside() string {
	if w == nil || !w.answer().Flagged() {
		return ""
	}
	return cell(w.words())
}

// answer is the shared shape the one rule and the promotion's refusal read (internal/answered).
func (w answered) answer() answers.Answer {
	return answers.Answer{Status: w.Status, Said: w.Said, Error: w.Error}
}

// words is what it answered at /, the standard reason phrase and none for a code with none, never the
// app's own: "answered 400 Bad Request at /", or "no answer at /: refused".
func (w answered) words() string {
	if w.Error != "" {
		return "no answer at /: " + w.Error
	}
	said := strings.TrimSpace(fmt.Sprintf("answered %d %s", w.Status, http.StatusText(w.Status))) + " at /"
	if w.Status == http.StatusUnauthorized { // Canyon's one GET / carries no identity token (contracts/routes.md, envs.<env>.answered)
		said += " (Canyon's check sends no identity token, so an app that checks it answers 401 here)"
	}
	return said
}

// validRun is the seam's run id (contracts/schema/timeline-row.schema.json), capped to one URL
// segment — not the generator's width: narrowing what a conforming host may answer is a seam PR.
var validRun = regexp.MustCompile(`^gr_[a-z0-9]{4,61}$`)

func runCheck(ctx context.Context, a *app, args []string) int {
	var explain string
	var reports reportFiles
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&explain, "explain", "", "") // no usage: refused by name, never offered
		fs.Var(&reports, "sarif", sarifUsage)
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("check takes no arguments, and got %q", rest[0]))
	}
	if explain != "" { // registered so the refusal is a Canyon line, not Go's flag report
		return a.usage(errors.New("--explain has no source yet; the why line names the policy"))
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
	if missing := (*NoProjectFile)(nil); errors.As(err, &missing) { // deploy writes the file check only reads (§3.2): the one way out, named (#873)
		return a.usage(fmt.Errorf("%w: run canyon deploy here, which makes the app and writes its id, or run check in your app's directory", err))
	}
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	began := time.Now()
	run := checkRun{App: p.App, Host: client.page(), Reports: reports}
	a.header(run.App, "")
	if err := a.packaged(ctx, client, dir, &run); err != nil {
		code := a.fail(err)
		a.askS3(ctx, client, dir, err)
		return code
	}
	started, err := startRun(ctx, client, run.App, run.Package, "gate-runs")
	if err != nil {
		return a.fail(err)
	}
	return a.followCheck(ctx, client, run, started, began)
}

// followCheck follows the review started, from its first row, to its verdict, or hands it off where a tool stops
// following: canyon check's, and canyon_review's for a check's run it was handed (mcp_remote.go).
func (a *app) followCheck(ctx context.Context, client *Client, run checkRun, started string, began time.Time) int {
	each, stop := a.narrate(&run)
	seen, err := a.poll(ctx, client, started, checkCeiling, each)
	stop()
	if code, still := a.reviewStillRunning(run, started, seen, err); still { // the review goes on in Canyon (#2730)
		return code
	}
	if err != nil {
		code := a.fail(err)
		a.errf("\n  The review keeps going without this terminal:\n  %s/#/apps/%s\n", run.Host, run.App) // its page, never its run id
		return code
	}
	if seen.Unknown > 0 {
		a.dim("%s this build does not understand", plural(seen.Unknown, "row", "rows"))
	}
	var read struct {
		Run runResult `json:"run"`
	}
	if err := a.readRun(ctx, client, started, &read); err != nil {
		if code, unread := a.endedUnread(run, started, seen, "review", err); unread {
			return code
		}
		return a.fail(err)
	}
	run.Run, run.StartedAt, run.Elapsed, run.Result = read.Run.ID, seen.StartedAt, time.Since(began), read.Run
	if a.render.Format == FormatSARIF && (read.Run.State == "clean" || read.Run.State == "blocked") {
		if err := client.getUpTo(ctx, "/api/v1/gate-runs/"+started+"/sarif", timelineLimit, &run.SARIF); err != nil { // a whole SARIF log, as a timeline page carries
			return a.fail(err)
		}
	}
	a.appRules(ctx, client, &run)
	return a.verdict(run)
}

// prologue is everything a verb does before a run exists: the project, the package, the row that
// says what was uploaded, and the reports --sarif attaches to it. One package per invocation,
// whichever verb asked for it.
// A deploy records the package the host named as the base, forced or not, so one refusal never
// becomes a loop; a base that cannot be written is a warning, since the upload succeeded. A check's
// upload is review-only (cli.md §4.1): it never moves the app's current package, so it sends no base
// and records none.
func (a *app) prologue(ctx context.Context, client *Client, dir string, run *checkRun) error {
	made, err := ensureProject(ctx, client, run.App, run.Create)
	if err != nil {
		return err
	}
	check, env := a.verb.name == "check", "preview"
	if check {
		env = ""
	}
	if run.Base, err = a.checkBase(ctx, client, dir, run.App, env, made); err != nil { // guard (b), before anything is uploaded (§6.6)
		return err
	}
	skip, sent, err := sendPackage(ctx, client, dir, run, check, "")
	var stale *staleBase
	if errors.As(err, &stale) && a.onThePush(ctx, client, dir, stale) { // this copy holds the push that moved the app: built on it
		var head string
		_ = json.Unmarshal(stale.api.Extra["head_package"], &head)
		skip, sent, err = sendPackage(ctx, client, dir, run, check, head)
	}
	if err != nil {
		return err
	}
	run.Package, run.NotPacked, run.Dockerfile = sent, skip.NotPacked, skip.Dockerfile
	if !check {
		if err := writeBase(dir, run.App, sent); err != nil {
			a.errf("\n  %s The base could not be recorded, so the next deploy from here will ask you to pull or use --force: %v\n", a.render.glyph(stateWarn), err)
		}
	}
	trust := " " + a.dot() // the trust statement ends at the host it went to, as dialled, whatever matched (cli.md §3.1)
	if named := excludedNames(skip.Excluded); named != "" {
		trust += " " + named
	}
	trust += " " + a.render.punct("→", "->") + " " + cell(client.name())
	a.printf("\n  %s Package    %s %s %s%s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)),
		plural(run.Files, "file", "files"), a.dot(), size(run.Bytes), trust)
	if skip.Paths > 0 { // what was not uploaded, said out loud, every time
		paths := ""
		if len(skip.NotPacked) > 0 { // git's record alone stays a count
			paths = ": " + andMore(skip.NotPacked, excludedShown)
		}
		a.dim("%s not packed%s", plural(skip.Paths, "path", "paths"), paths)
	}
	if run.Dockerfile {
		a.dim("%s", dockerfileNote)
		if a.firstAsk(run.App, ownDockerfileIgnored) {
			a.dim("%s", askFor(ownDockerfileIgnored))
		}
	}
	return a.attach(ctx, client, run)
}

func (a *app) dim(f string, v ...any) {
	a.printf("  %s %s\n", a.render.paint(ansiDim, a.render.glyph(statePending)), a.render.paint(ansiDim, fmt.Sprintf(f, v...)))
}

// ensureProject is guard (a), cli.md §3.2: the host must already have the app canyon.toml names, or
// a checkout signed in to two Canyons would make it on whichever answered. The read answers every
// signed-in caller, a reduced row below contributor, so the upload's own team check still decides.
// A 200 must be that app's row, or it is not Canyon answering. Only the contract's 404 is an app to
// make, and only when create says so; made is whether this call made it.
func ensureProject(ctx context.Context, c *Client, app string, create bool) (made bool, err error) {
	var read struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	err = c.get(ctx, "/api/v1/projects/"+url.PathEscape(app), &read)
	if err == nil && read.Project.ID != app {
		return false, &NotCanyon{Host: c.Base, Status: http.StatusOK}
	}
	if _, lacks := refusal(err, http.StatusNotFound, "not_found"); !lacks {
		return false, err
	}
	if !create {
		return false, &notOnHost{app: app, host: c.Base}
	}
	err = c.postJSON(ctx, "/api/v1/projects", map[string]string{"id": app}, http.StatusCreated, nil)
	if _, exists := refusal(err, http.StatusConflict, "exists"); exists { // made in between: the 409 rolls back, no audit row
		return false, nil
	}
	if api, named := refusal(err, http.StatusBadRequest, "invalid_id"); named {
		return false, fmt.Errorf("%s names the app %q, which this host will not take: %s", projectFile, app, api.Message)
	}
	return err == nil, err
}

// notOnHost is guard (a)'s refusal: the app, and the host that doesn't have it, as dialled. env is
// secret set's and undeploy's --env, for its ai block; the deploy forms name their own.
type notOnHost struct{ app, host, env string }

func (e *notOnHost) Error() string { return e.app + " isn't on " + e.host }

// lacking is guard (a)'s refusal from a read's own 404 for the app canyon.toml names, in place of
// the host's "Deploy it first: canyon deploy" (§3.2, §4.3); any other answer is err as it was.
func lacking(err error, app string, c *Client) error {
	if _, lacks := refusal(err, http.StatusNotFound, "not_found"); lacks {
		return &notOnHost{app: app, host: c.Base}
	}
	return err
}

// lackingAsked is lacking for a route whose 404 not_found also means something else — nothing
// deployed, no package yet, no such grant — so the app's own read says which (#2195): its 404 is
// guard (a)'s refusal, and any other answer leaves err as it was, the route's own line.
func lackingAsked(ctx context.Context, err error, app string, c *Client) error {
	if _, unsure := refusal(err, http.StatusNotFound, "not_found"); !unsure {
		return err
	}
	var read struct{}
	if lacks := lacking(c.get(ctx, "/api/v1/projects/"+url.PathEscape(app), &read), app, c); errors.As(lacks, new(*notOnHost)) {
		return lacks
	}
	return err
}

// lacked is §3.2's refusal, exit 1, from fail. Promotion has nothing here to promote and offers no
// --create; under ai the block carries the verb's own header and leaves --create to the person.
func (a *app) lacked(e *notOnHost) int {
	prod := strings.HasPrefix(a.verb.name, "deploy --env prod") // a rollback's too
	line := e.Error() + ". If it should be, run: canyon deploy --create"
	if prod {
		line = e.Error() + ", so there is nothing here to promote."
	}
	if a.render.Format != FormatAI {
		a.errf("\n  %s %s\n", a.render.glyph(stateFail), line)
		return exitError
	}
	env, outcome := e.env, e.Error()+"."
	switch {
	case prod:
		env, outcome = "prod", line
	case a.form == "secret set":
		outcome += " Nothing was set."
	case a.form == "secret list", a.verb.name == "status", a.verb.name == "share", a.verb.name == "logs", a.verb.name == "pull": // reads: the block ends at the host
	case a.verb.name == "undeploy":
		outcome += " Nothing was taken offline."
	case a.verb.name == "delete":
		outcome += " Nothing was deleted."
	case a.verb.name == "deploy":
		env, outcome = "preview", outcome+" Nothing was sent."
	default: // check
		outcome += " Nothing was sent."
	}
	a.printf("%s — RESULT: REFUSED\n\napp: %s\n", a.head(), cell(e.app))
	if env != "" {
		a.printf("env: %s\n", cell(env))
	}
	a.printf("host: %s\noutcome: %s\n\n", cell(e.host), cell(outcome))
	switch {
	case a.remote && prod: // on /mcp: no canyon.toml, no host to change
		a.printf("NEXT STEPS:\n1. Tell the user the outcome line.\n2. Stop.\n")
	case a.remote:
		a.printf("NEXT STEPS:\n1. Tell the user the outcome line, and ask whether %s is new on this Canyon.\n"+
			"2. Only if the user says it is, call canyon_create_app with its repository, then deploy it with canyon_deploy_ref.\n"+
			"3. Do not make an app unless the user says this one is new on this Canyon.\n", cell(e.app))
	case prod:
		a.printf("NEXT STEPS:\n1. Tell the user the outcome line.\n2. Stop. Do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n")
	default:
		a.printf("NEXT STEPS:\n1. Tell the user the outcome line, and ask whether %s is new on this Canyon.\n"+
			"2. Only if the user says it is, run: canyon deploy --create\n"+
			"3. Do not add --create unless the user says this app is new on this Canyon, and do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n", cell(e.app))
	}
	a.printf("\nexit code: %d\n", exitError)
	return exitError
}

// head is the ai block's header for a refusal before anything was sent: the one the verb's result
// already uses, and for a verb whose result prints no block, its own name (cli.md §3.2).
func (a *app) head() string {
	switch verb := cmp.Or(a.form, a.verb.name); {
	case verb == "deploy", verb == "check":
		return "CANYON REVIEW"
	case strings.HasPrefix(verb, "deploy --env prod"): // a rollback's too: it ends in a promotion
		return "CANYON PROMOTE"
	default:
		return "CANYON " + strings.ToUpper(verb)
	}
}

// refusal is one answer of the contract's, code and status together: 400 exists is not it.
func refusal(err error, status int, code string) (*APIError, bool) {
	var api *APIError
	if errors.As(err, &api) && api.Status == status && api.Code == code {
		return api, true
	}
	return nil, false
}

// staleBase is the upload's 409 (routes.md row 57): the host's evidence, and the base this copy
// sent, which the envelope does not repeat.
type staleBase struct {
	app, base string
	api       *APIError
	commit    string // the pushed head this working tree lacks, when a repository feeds the app: git's to settle (§3.6)
}

func (e *staleBase) Error() string { return e.api.Error() }

// sendPackage writes the tree into a temp file and posts it: plain tar, a length the server can read.
// A file, not a pipe, so a pack that fails fails before a byte is sent. A deploy sends the base this
// directory recorded, when it has one, and sent is the new base; a check sends the review-only query
// and no base. sent is the package the 201 named.
// on, when not "", is the base to send in place of the recorded one.
func sendPackage(ctx context.Context, c *Client, dir string, run *checkRun, check bool, on string) (skip packSkip, sent string, err error) {
	f, err := os.CreateTemp("", "canyon-package-*.tar")
	if err != nil {
		return packSkip{}, "", err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	run.Files, run.Bytes, skip, err = pack(dir, f)
	if err != nil {
		return packSkip{}, "", err
	}
	info, err := f.Stat()
	if err != nil {
		return packSkip{}, "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return packSkip{}, "", err
	}
	base, path := cmp.Or(on, run.Base), "/api/v1/projects/"+url.PathEscape(run.App)+"/packages"
	if check {
		base, path = "", path+"?check=true"
	}
	var answer struct {
		Package struct {
			ID string `json:"id"`
		} `json:"package"`
	}
	err = c.upload(ctx, path, f, info.Size(), base, run.Force, &answer)
	if api, stale := refusal(err, http.StatusConflict, "stale_base"); stale {
		return packSkip{}, "", &staleBase{app: run.App, base: base, api: api}
	}
	if err == nil && answer.Package.ID == "" { // no package to name: the run would review whatever is current
		return packSkip{}, "", &NotCanyon{Host: c.Base, Status: http.StatusCreated}
	}
	return skip, answer.Package.ID, err
}

// excludedShown caps the names on one line: the list matches in a dozen places in a monorepo,
// and a Package line that wraps says less than one that does not.
const excludedShown = 3

// excludedNames is design/specs/cli.md §3.1's trust statement — "a governance tool that uploads
// your code needs to say what it didn't upload" — in the words of the tree just walked:
// `.env and node_modules excluded`. Nothing matched, nothing to say, and no empty clause.
func excludedNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return andMore(names, excludedShown) + " excluded"
}

// andMore is names as a sentence, `a, b and c`, the first shown of them and then how many more.
func andMore(names []string, shown int) string {
	parts := make([]string, 0, shown+1)
	for _, name := range names {
		// The cap hides two names or more. Hiding exactly one would spend the slot it saved.
		if len(parts) == shown && len(names) > shown+1 {
			parts = append(parts, strconv.Itoa(len(names)-shown)+" more")
			break
		}
		parts = append(parts, cell(name)) // a filename reaches the line as text, never as control bytes
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// staleBase is cli.md §3.6 on stderr: the app moved under this copy. Exit 1 and not 2, since
// nothing ran and there is no finding to fix. The first exit is pull --force, because the
// directory the 409 fires in is not empty; the second names this verb. The host's strings go
// through cell, and moved_at through age, which is a dash for a time this build cannot read.
func (a *app) movedBlock(e *staleBase) {
	glyph := a.render.glyph(stateFail)
	var head, by, at string
	_ = json.Unmarshal(e.api.Extra["head_package"], &head)
	_ = json.Unmarshal(e.api.Extra["moved_by"], &by)
	_ = json.Unmarshal(e.api.Extra["moved_at"], &at)
	force := "canyon " + a.verb.name + " --force"
	if e.commit != "" { // a push this working tree doesn't have: git pull brings it, never canyon pull (§3.6)
		a.errf("\n  %s %s pushed %s to main %s, and this working tree doesn't have it.\n\n", glyph, cell(cmp.Or(by, "Someone")), e.commit[:7], a.movedAgo(at))
		a.errf("    Take it:             git pull, then canyon %s\n", a.verb.name)
		a.errf("    Send yours anyway:   %s\n", force)
		a.aiAsk(by)
		return
	}
	when := a.movedAgo(at)
	if by != "" {
		when = cell(by) + ", " + when
	}
	if e.base == "" { // a fresh clone: this copy records no base, so there is no Yours line to print
		a.errf("\n  %s %s already has a version this copy didn't start from.\n\n", glyph, e.app)
		a.errf("    Theirs   %s   %s\n\n", cell(head), when)
	} else {
		a.errf("\n  %s The current package of %s is not the one this copy started from.\n\n", glyph, e.app)
		a.errf("    Theirs   %s   %s\n", cell(head), when)
		a.errf("    Yours    %s\n\n", cell(e.base))
	}
	a.errf("    Take their work:     canyon pull --force   (writes their files over yours, deletes nothing)\n")
	a.errf("    Send yours anyway:   %s\n", force)
	a.aiAsk(by)
}

// aiAsk is §5.2's last line of a refusal under ai: an agent shows the two exits and asks; it never picks one.
func (a *app) aiAsk(by string) {
	if a.render.Format != FormatAI {
		return
	}
	theirs := "the version deployed there now" // whoever deployed it, the person's own earlier deploy included (#1833)
	if by != "" {
		theirs = "the version " + cell(by) + " deployed"
	}
	a.errf("\n    Do not run either. Show the person both lines and ask: one writes over %s, the other over the person's own.\n", theirs)
}

// movedAgo is moved_at as the refusals say it: "14m ago", "now", or a dash for a time this build cannot read.
func (a *app) movedAgo(at string) string {
	when := a.age(at, a.clock.now())
	if when != a.dash() && when != "now" { // "now ago" and "— ago" both read as nonsense
		when += " ago"
	}
	return when
}

// startPaths are the routes that start a run, and the verb each one offers again. A deploy IS a
// gate run (cmd/canyon-server/routes_gate.go), so only the route and that line differ.
var startPaths = map[string]string{"gate-runs": "check", "deploys": "deploy"}

// startRun names the package the upload answered, so the run reviews that package and the reports
// attached to it, never one another upload made current in between.
func startRun(ctx context.Context, c *Client, app, pkg, path string) (string, error) {
	var started struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	err := c.postJSON(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/"+path, map[string]string{"package": pkg}, http.StatusAccepted, &started)
	switch api, busy := refusal(err, http.StatusConflict, "run_in_progress"); {
	case busy && watchUntil(api.Extra["watch"]) != "": // Canyon's patch watch, which a deploy ends (contracts/routes.md, the gate-runs row; D5)
		return "", errors.New("Canyon is watching its patch of " + app + "'s preview until " + watchUntil(api.Extra["watch"]) +
			" UTC, so it can't review now: run canyon check again after that, or canyon deploy, which ends the watch")
	case busy:
		return "", fmt.Errorf("a review of %s is already running%s; let it finish, then run canyon %s again", app, runNamed(api.Extra["run"]), startPaths[path])
	case err != nil:
		return "", err
	case !validRun.MatchString(started.Run.ID): // naming no run, or naming one this CLI will not put in a URL
		return "", &NotCanyon{Host: c.Base, Status: http.StatusAccepted}
	}
	return started.Run.ID, nil
}

// watchUntil is a refusal's watch.until as HH:MM in UTC, "" when the envelope carried none or one that is no
// RFC 3339 time.
func watchUntil(raw json.RawMessage) string {
	var watch struct {
		Until string `json:"until"`
	}
	if json.Unmarshal(raw, &watch) != nil {
		return ""
	}
	at, err := time.Parse(time.RFC3339, watch.Until)
	if err != nil {
		return ""
	}
	return at.UTC().Format("15:04")
}

// runNamed is the id out of a refusal's evidence, when the envelope carried one.
func runNamed(raw json.RawMessage) string {
	var run struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &run) != nil || !validRun.MatchString(run.ID) {
		return ""
	}
	return " (" + run.ID + ")"
}

// verdict is what check prints and the code an agent branches on: 0 clean, 2 blocked, 1 anything
// with no answer. The CLI derives no verdict, and refuses one that argues with its own findings:
// a passing exit on a run carrying a blocker is the one failure this product cannot have.
func (a *app) verdict(run checkRun) int {
	res := run.Result
	if !a.agrees(run) {
		return exitError
	}
	if res.State == "failed" && a.render.Format == FormatAI { // §5.2: its own block replaces the transcript, and stderr keeps errors only
		a.unpark(false)
		return a.aiFailed(run)
	}
	if res.State == "failed" && a.render.Format != FormatJSON && a.render.Format != FormatSARIF { // the transcript keeps them
		a.failedReview(run)
	}
	code, ok := a.outcome(run)
	if !ok { // no verdict at all: nothing on stdout under any format
		return exitError
	}
	if a.render.Format == FormatSARIF { // cli.md §5.4: the host's log and one newline, only on a verdict
		if code != exitError {
			a.unpark(false)
			_, _ = a.out.Write(append(run.SARIF, '\n'))
		}
		return code
	}
	if a.render.Format == FormatJSON { // between the two returns on purpose: a failed run still gets its object, a refusal none
		return a.jsonVerdict(run, code)
	}
	if code == exitError { // a failed run: its lines are on stderr, and there is no verdict to render
		return code
	}
	if a.render.Format == FormatAI { // §5.2 and §5.3: the block replaces the parked transcript, whichever way the run ended
		a.unpark(false)
		return a.aiVerdict(run, code)
	}
	a.reviewStage(run)
	switch {
	case res.State == "clean":
		a.shippedRows(run)
		if warnings := entriesOf(res, false); len(warnings) > 0 { // §3.1: the warning is repeated last, because it scrolled away during the build; §3.3's entries
			named := make([]string, 0, len(warnings))
			for _, e := range warnings {
				rule, _ := e.named()
				named = append(named, cell(rule))
			}
			a.printf("\n  %s\n", a.render.paint(ansiAmber, fmt.Sprintf("%s %s didn't block %s %s %s canyon check",
				a.render.glyph(stateWarn), plural(len(warnings), "warning", "warnings"), a.dash(), strings.Join(named, ", "), a.dot())))
		}
	case approved(res): // D1: neither blocked nor clean, and amber, on check as on deploy
		if a.shipped(run) {
			a.printf("\n  Every blocking problem is approved, so this version goes to preview.\n")
		}
		a.findings(run)
		a.shippedRows(run)
	default:
		if a.shipped(run) { // it shipped under approvals that have expired since: the run read no longer carries them
			a.printf("\n  Deployed anyway: every blocking finding carries an approval.\n")
		} else if a.verb.name == "deploy" || run.Rollback != nil { // D5: check never deploys, so only a deploy says it did not
			a.printf("\n  Nothing was built and nothing was deployed.\n")
		}
		a.findings(run)
		a.shippedRows(run)
	}
	if res.State == "clean" && a.shipped(run) && run.Deploy.Env == "preview" { // the verdict, what shipped, then the one step left (cli.md §3.1)
		a.printf("\n  Next: canyon deploy --env prod%s\n", a.notOK(run.Deploy.Answered))
	}
	done := "Done in " + since(run.Elapsed)
	if run.Run != "" { // every outcome names its run, so a passing one can be cited too (#1674)
		done += " " + a.dot() + " run " + visible(run.Run)
	}
	a.printf("\n  %s\n", a.render.paint(ansiDim, done))
	return code
}

// agrees is whether the host's verdict is its findings' own, each saying whether it blocks and where;
// the line that says why not goes to stderr.
func (a *app) agrees(run checkRun) bool {
	if why := a.disagrees(run); why != "" {
		a.errf("%s", why)
		return false
	}
	return true
}

// disagrees is the line saying why the host's verdict is not its findings' own, "" when it is.
func (a *app) disagrees(run checkRun) string {
	res, blocking, warnings := run.Result, 0, 0
	for _, f := range res.Findings {
		blocks, said := f.Blocking()
		if !said { // never false, never re-derived (contracts/finding.md)
			return fmt.Sprintf("\n  %s %s answered a finding (%s) that does not say whether it blocks.\n", a.render.glyph(stateFail), run.Host, visible(f.RuleID))
		}
		if len(f.Locations) == 0 { // a finding without a location cannot be rendered (contracts/finding.md)
			return fmt.Sprintf("\n  %s %s answered a finding (%s) that names no location.\n", a.render.glyph(stateFail), run.Host, visible(f.RuleID))
		}
		if blocks {
			blocking++
		} else {
			warnings++
		}
	}
	if res.Blocking != blocking || res.Warnings != warnings ||
		(res.State == "clean" && blocking > 0) || (res.State == "blocked" && blocking == 0) {
		return fmt.Sprintf("\n  %s the host's verdict and its findings disagree (run %s)\n", a.render.glyph(stateFail), visible(run.Run))
	}
	return ""
}

// reviewStage is the review as one stage row, as Package is the upload's: the verdict, then each
// producer's row under it in plain words (cli.md §3.1). Once only: a deploy prints it as its verdict arrives.
func (a *app) reviewStage(run checkRun) {
	if run.Reviewed {
		return
	}
	res := run.Result
	switch {
	case res.Blocking == 0:
		passed := "Review passed"
		if res.NotChecked { // never "passed" while the review names what it did not read (contracts/finding.md)
			passed = "Review found nothing blocking in what was checked"
		}
		a.printf("\n  %s\n", a.render.paint(ansiBold+ansiGreen, fmt.Sprintf("%s %s %s %d blocking, %s",
			a.render.glyph(statePass), passed, a.dot(), res.Blocking, plural(len(entriesOf(res, false)), "warning", "warnings")))) // §3.3's entries
	case everyBlockerApproved(res):
		a.printf("\n  %s\n", a.render.paint(ansiBold+ansiAmber, a.approvedLine(res)))
		if f := soleApproval(res); f != nil {
			a.printf("    %s\n", cell(strconv.Quote(f.Reason)))
		}
	default:
		a.printf("\n  %s\n", a.render.paint(ansiBold+ansiRed, fmt.Sprintf("%s Review blocked %s %s", // the entries the list shows (§3.3)
			a.render.glyph(stateFail), a.dot(), plural(blockingEntries(res), "blocking finding", "blocking findings"))))
	}
	a.reviewRows(run)
}

// failedReview is a failed run's review. A build or an app that failed ran after a review that
// passed, outright or under approvals, so it has its stage row (§9); any other failure stopped the
// review itself, and its rows print under the phase's heading, which a spinner leaves unprinted. A review a
// deploy printed as its verdict arrived is not printed again, whatever stopped the run after it.
func (a *app) failedReview(run checkRun) {
	if run.Reviewed {
		return
	}
	if reviewedFirst(run.Result.FailureReason) {
		a.reviewStage(run)
		return
	}
	if !run.ReviewHeader {
		a.printf("\n  %s\n", a.render.paint(ansiDim, "Review"))
	}
	a.reviewRows(run)
}

// reviewedFirst is whether a run that failed for reason had passed its review, outright or under
// approvals: the build, the start and the room for them come only after it.
func reviewedFirst(reason string) bool {
	return reason == "build" || reason == "detect" || reason == "start" || reason == "no_room"
}

func (a *app) reviewRows(run checkRun) {
	for _, row := range run.Rows {
		row()
	}
}

// shippedRows is what a deploy did after the verdict: its Build and Deploy rows, then where it runs.
func (a *app) shippedRows(run checkRun) {
	a.deployRows(run)
	a.liveURLs(run)
}

// outcome is the exit code a run earns, and false when there is no verdict to render: a clean deploy
// that shipped nothing, or a state this build does not know. A failed run's lines go to stderr here.
func (a *app) outcome(run checkRun) (int, bool) {
	res := run.Result
	switch res.State {
	case "clean":
		if a.verb.name == "deploy" && !a.shipped(run) { // a 202, a clean gate, and nothing live: the contract was not kept
			a.errf("\n  %s The review passed and %s deployed nothing (run %s).\n", a.render.glyph(stateFail), run.Host, visible(run.Run))
			return exitError, false
		}
		return exitOK, true
	case "blocked":
		switch {
		case a.shipped(run):
			return exitOK, true
		case approved(res) && a.verb.name == "deploy": // approved and nothing live: the contract was not kept
			a.errf("\n  %s Every blocking finding is approved and %s deployed nothing (run %s).\n", a.render.glyph(stateFail), run.Host, visible(run.Run))
			return exitError, false
		case approved(res): // a check of approved code: the code ran and the answer was yes
			return exitOK, true
		}
		return exitBlocked, true // nothing shipped, so the agent that asked has work to do
	case "failed":
		if a.verb.name == "deploy" && res.FailureReason != "detect" { // a deploy's failure says why in its lines: the build's, the app's, a driver's, a missing front door; a failed detect's are each buildpack declining, kept on the run, never printed (§9)
			a.buildLog(run)
		}
		if a.verb.name == "deploy" && res.FailureReason == "detect" && res.Fix != nil {
			a.errf("\n  %s The app is missing a %s\n", a.render.glyph(stateFail), cell(res.Fix.File))
			a.missing(res.Fix)
			return exitError, true
		}
		if a.verb.name == "deploy" && res.FailureReason == "detect" {
			a.undetected(run)
			return exitError, true
		}
		if a.verb.name == "deploy" && res.FailureReason == "no_room" { // the scheduler's message is the last line above (#1637)
			a.errf("\n  %s The cluster had no room to build it\n", a.render.glyph(stateFail))
			a.errf("  That's the cluster's capacity, not the app's code or Canyon: an admin adds nodes, or lowers what apps request (CANYON_KUBE_APP_CPU_REQUEST).\n")
			a.errf("  Then run: canyon deploy\n")
			return exitError, true
		}
		if run.Rollback != nil && res.FailureReason == "image_gone" && len(run.Build) > 0 { // the server's line names the version and the way to a rebuilt one
			a.errf("\n  %s %s\n", a.render.glyph(stateFail), cell(ansi.Strip(run.Build[len(run.Build)-1])))
			return exitError, true
		}
		if res.FailureReason == "scan_no_room" { // before any review, so never a block to fix or approve: a deploy's scheduler lines are above
			a.errf("\n  %s The cluster had no room to run Canyon's checks\n", a.render.glyph(stateFail))
			a.errf("  Nothing was reviewed, so there is nothing to fix or approve: it's the cluster's capacity, not the app's code or a policy.\n")
			a.errf("  Run it again in a few minutes: canyon %s\n", a.verb.name)
			return exitError, true
		}
		if a.verb.name == "deploy" && res.FailureReason == "build" {
			// The build's own last line — the executor's failure line, the step and its exit code — and no
			// culprit: the app, a buildpack or the host's image, a build failed on what Canyon cannot know (#1734).
			failed := ""
			if len(run.Build) > 0 {
				failed = strings.TrimSpace(cell(ansi.Strip(run.Build[len(run.Build)-1])))
			}
			why := "The review was clean" // a build runs on a clean review or on approvals, and the two read differently
			if res.Blocking > 0 {
				why = "The review passed under approvals"
			}
			if failed == "" { // a build that wrote nothing: there are no lines to point at
				a.errf("\n  %s Build failed\n", a.render.glyph(stateFail))
				a.errf("  %s, so this is not a policy block. The build wrote no lines.\n", why)
			} else {
				a.errf("\n  %s Build failed: %s\n", a.render.glyph(stateFail), failed)
				a.errf("  %s, so this is not a policy block: the build's own lines above say what failed.\n", why)
				if _, ok := pinned(run.Build); ok { // the app's to change, never the host's to add (#2950)
					a.errf("  The app pins a version this build doesn't offer: change it to one listed above.\n")
				}
			}
			if len(run.NotPacked) > 0 { // Canyon took these, so the builder hears it before reading anything into the failure
				a.errf("  Not packed, in case the build needs one: %s\n", andMore(run.NotPacked, excludedShown))
			}
			a.errf("  Then run: canyon deploy\n")
			return exitError, true
		}
		if a.verb.name == "deploy" && res.FailureReason == "start" { // §9: the app's own last lines above, the reason last
			a.errf("\n  %s The app did not start\n", a.render.glyph(stateFail))
			if res.Fix != nil {
				a.missing(res.Fix)
				return exitError, true
			}
			a.errf("  The build passed %s the app stopped as it started, which is its own error, not a policy block.\n", a.dash())
			a.errf("  Fix it, or set a value it needs with canyon secret set, and run: canyon deploy\n")
			return exitError, true
		}
		unfinished := "The review did not finish"
		if run.Reviewed { // its review printed above as it passed: what stopped is the build that followed (#2697)
			unfinished = "The build did not finish"
		}
		a.errf("\n  %s %s: %s\n", a.render.glyph(stateFail), unfinished, failureReason(res.FailureReason, res.FailedIn))
		if res.FailedIn != "" { // the server logged why under the run's id (contracts/routes.md, A failure reason in words)
			a.errf("  %s\n", againLine(run.Run))
		}
		return exitError, true
	default:
		a.errf("\n  %s The review ended in the state %q, without a verdict.\n", a.render.glyph(stateFail), res.State)
		return exitError, false
	}
}

// undetected is §9's failed detect: nothing was compiled, and there is no "Fix it". The host does not
// build this kind of app, and says what it does build, the run read's builds. When a buildpack errored
// while detecting, a malformed manifest is possible, so the screen never says the code is fine; paths
// that stayed home are named, as a failed build names them, and the last line asks for it (#2800).
func (a *app) undetected(run checkRun) {
	a.errf("\n  %s This host can't build this kind of app.\n", a.render.glyph(stateFail))
	if len(run.Result.Builds) == 0 {
		a.errf("  Its builder doesn't say what it builds.\n")
	} else { // one name a line: a name is never broken across two
		a.errf("  It builds:\n")
		for _, b := range run.Result.Builds {
			a.errf("    %s\n", cell(b))
		}
	}
	if run.Result.Errored {
		a.errf("  A buildpack also errored while checking. If this app is meant to be one of those stacks, check that its manifest is valid (for example package.json).\n")
	} else {
		a.errf("  No buildpack recognised the package, so nothing was compiled: not an error in the code, not a policy block.\n")
	}
	if len(run.NotPacked) > 0 {
		a.errf("  Not packed, in case the build needs one: %s\n", andMore(run.NotPacked, excludedShown))
	}
	a.errf("  %s\n", askFor(languageUnrecognised))
}

// missing says which file the package lacks, why, and each example of what goes in it, indented.
func (a *app) missing(fix *runFix) {
	a.errf("  %s\n  Add a file named %s at the app's root, holding the lines for this app.\n", cell(fix.Why), cell(fix.File))
	for _, e := range fix.Examples {
		a.errf("  For %s:\n", cell(e.For))
		for _, line := range strings.Split(strings.TrimSuffix(e.Text, "\n"), "\n") {
			a.errf("%s\n", strings.TrimRight("      "+cell(line), " "))
		}
	}
	a.errf("  Then run: canyon deploy\n")
}

// builds is a failed detect's run read's builds, each made safe to print, joined by ", "; "" when none.
func builds(res runResult) string {
	named := make([]string, len(res.Builds))
	for i, b := range res.Builds {
		named[i] = cell(b)
	}
	return strings.Join(named, ", ")
}

var failureReasons = map[string]string{
	"interrupted":  "it was cancelled, or another run of this app replaced it",
	"error":        "Canyon itself failed",
	"model":        "the reviewer's model failed",
	"build":        "the package did not build",
	"detect":       "no buildpack on this host recognised the package",
	"start":        "the app did not stay up",
	"no_room":      "the cluster had no room to run the build",
	"scan_no_room": "the cluster had no room to scan the package",
	"image_gone":   "the image production ran is no longer on this Canyon",
}

// stepWords is each step a run Canyon itself failed in, as the reason's words name it.
var stepWords = map[string]string{
	"scan":      "scanning the package",
	"review":    "reviewing the package",
	"resources": "making the app's resources",
	"build":     "building the package",
	"deploy":    "starting the app",
}

func failureReason(reason, step string) string {
	if reason == "" {
		return "the host named none"
	}
	if why, ok := failureReasons[reason]; ok {
		return reason + " (" + why + inStep(step) + ")"
	}
	return visible(reason)
}

// inStep is " while <the step's words>", or "" for none; a step this build does not know reads as itself.
func inStep(step string) string {
	if step == "" {
		return ""
	}
	return " while " + visible(cmp.Or(stepWords[step], step))
}

// againLine is what a run Canyon itself failed in tells its owner to do next.
func againLine(run string) string { return "Run it again. If it fails again, " + lookUp(run) }

// lookUp is where an admin finds why: the server logs the run's step and error under its id.
func lookUp(run string) string {
	return "an admin can look up run " + visible(run) + " in Canyon's server log."
}

// gateRows are the check rows of cli.md §2: one per check, in the order the checks started, each
// resolved when its check.completed arrives and never before the row above it — the work may be
// parallel, the output is not. A resolved row is held on the run, for the stage row the verdict
// prints above it. Nothing is flushed after the poll: a check that started and never completed
// belongs to a run that failed, and the run's failure is what prints.
type gateRows struct {
	a       *app
	run     *checkRun
	order   []string
	done    map[string]gateDone
	printed int
}

// gateDone is check.completed's data (contracts/events.md); check.started carries only the name.
type gateDone struct {
	Check    string `json:"check"`
	Status   string `json:"status"`
	Findings int    `json:"findings"`
	Duration int64  `json:"duration_ms"`
}

func (g *gateRows) row(completed bool, d gateDone) {
	if !slices.Contains(g.order, d.Check) { // a completion with no start still gets its row, never dropped
		g.order = append(g.order, d.Check)
	}
	if completed {
		g.done[d.Check] = d
	}
	for ; g.printed < len(g.order); g.printed++ {
		above, ok := g.done[g.order[g.printed]]
		if !ok {
			return // the row above has not finished; this one waits behind it
		}
		g.run.Rows = append(g.run.Rows, func() { g.a.checkRow(above, g.run.Notes) }) // the notes arrive after the row, and are all in by the verdict
	}
}

// saysOff reports whether n is D10's note for a check whose held row already says it, in Canyon's
// words, or the scanners' note that an AI review rule went unread for the same want of a model. Asked as the rows print, so a note whose row never resolved is never dropped with it.
func (g *gateRows) saysOff(n producerNote) bool {
	producer := n.Producer
	if n.Why == unreadWithoutModel { // the scanners' note on an AI review rule: the reviewer's off row says it
		producer = "canyon-review"
	} else if n.Text != offNotes[producer] {
		return false
	}
	i := slices.Index(g.order, producer)
	return i >= 0 && i < g.printed && offRow(g.done[producer], g.run.Notes)
}

// unreadWithoutModel is gate/coverage.go's reason for an AI review rule nothing read on a host with no
// model (cli.md §3.1): a person reads it once, on the reviewer's off row.
const unreadWithoutModel = "no model is configured on this Canyon host, so the AI review read nothing"

// checkNames are the checks a person reads under a name of its own (cli.md §3.1: plain names).
var checkNames = map[string]string{"canyon-review": "AI review", "custom-rules": "custom rules"}

// offNotes are D10's exact words for a reviewer that read nothing because the host has no model
// (contracts/finding.md): its skipped row says so in Canyon's words, and the note is not said twice.
var offNotes = map[string]string{
	"canyon-review": "ai review · skipped (no model configured on this Canyon host)",
	"custom-rules":  "custom rules · skipped (no model configured on this Canyon host)",
}

// offRow reports whether a check's row says, in Canyon's words, that it was off for want of a model.
func offRow(d gateDone, notes []producerNote) bool {
	return d.Status == "skipped" && offNotes[d.Check] != "" && slices.Contains(notes, producerNote{Producer: d.Check, Text: offNotes[d.Check]})
}

// checkRow is §3.1's row: glyph, the check in plain words, what it found, a right-aligned dim
// timing. findings is neutral, never amber: the row cannot know whether they block — the verdict
// above it does — and an amber row under a red Blocked read as a warning that was not one. A check that could not cover everything is not fully
// checked, dim, never a `! warning`: a warning is a finding, and the verdict counts those.
func (a *app) checkRow(d gateDone, notes []producerNote) {
	said, glyph, colour := cell(d.Status), a.render.glyph(statePending), ansiDim
	switch d.Status {
	case "clean":
		glyph, colour = a.render.glyph(statePass), ansiGreen
	case "findings":
		said = plural(d.Findings, "finding", "findings")
	case "warning":
		said = "not fully checked"
	case "failed":
		glyph, colour = a.render.glyph(stateFail), ""
	} // skipped renders visibly and dim, and so does a status a newer server invents
	if offRow(d, notes) {
		said = "off (no AI model set up on this Canyon)"
	}
	if d.Status != "findings" && d.Findings > 0 {
		said += " " + a.dot() + " " + plural(d.Findings, "finding", "findings")
	}
	name := cmp.Or(checkNames[d.Check], cell(d.Check))
	if d.Status == "skipped" || d.Duration < 100 { // a skipped check took no time, and under a tenth of a second reads [0.0s]
		a.gateLine(glyph, colour, name, said, "", 0)
	} else {
		took := "[" + since(time.Duration(d.Duration)*time.Millisecond) + "]"
		a.gateLine(glyph, colour, name, said, a.render.paint(ansiDim, took), len(took))
	}
	for _, n := range notes {
		if how, ok := folded(n); ok && n.Producer == d.Check {
			a.printf("      %s\n", a.render.paint(ansiDim, cell(how)))
		}
	}
}

// folded is a reviewer's own note on how it ran, without the reviewer's name, which prints under the
// reviewer's row and not as a row of its own (cli.md §3.1); false for any other note.
func folded(n producerNote) (string, bool) {
	name, named := checkNames[n.Producer]
	if !named || !n.Aside {
		return "", false
	}
	return strings.CutPrefix(n.Text, strings.ToLower(name)+" · ")
}

// foldsUnder reports whether n prints under its reviewer's row, which this list holds and has printed.
func (g *gateRows) foldsUnder(n producerNote) bool {
	_, ok := folded(n)
	i := slices.Index(g.order, n.Producer)
	return ok && i >= 0 && i < g.printed
}

// gateLine is one row under the review's stage row, a check's, a tool's or a producer's note: glyph,
// name, what it found, and on a check its timing, right-aligned to the rule's width.
func (a *app) gateLine(glyph, colour, name, said, took string, tookWidth int) {
	left := fmt.Sprintf("%-18s %s", name, said)
	gap := ""
	if tookWidth > 0 {
		gap = strings.Repeat(" ", max(proseWidth-6-utf8.RuneCountInString(left)-tookWidth, 1))
	}
	a.printf("    %s %s%s%s\n", a.render.paint(colour, glyph), left, gap, took)
}

// size is the package in the units a builder reads, to one decimal.
func size(b int64) string {
	unit, n := "B", float64(b)
	for _, next := range []string{"KB", "MB", "GB"} {
		if n < 1024 {
			break
		}
		unit, n = next, n/1024
	}
	if unit == "B" {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", n, unit)
}
