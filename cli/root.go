// Package cli is the canyon binary: one verb per file, one line in the dispatch slice, and a help
// rendered from that slice so the two cannot drift.
package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Version is what the User-Agent and `canyon --version` say; cmd/canyon stamps it.
var Version = "dev"

// cli.md §3.5: 2 is separate from 1 so a leaked key and a down network read differently, and 3 from 1 so a deploy
// still running in Canyon is not taken down as a failed one (#2721).
const (
	exitOK           = 0
	exitError        = 1
	exitBlocked      = 2
	exitStillRunning = 3
)

const defaultHost = "http://localhost:7777"

// command is one verb. json says it renders `--format json`; one that does not refuses the flag.
type command struct {
	name, group, blurb string
	json               bool
	run                func(context.Context, *app, []string) int
}

var commands = []command{
	{"check", "SHIP", "Review this app's code without deploying it", true, runCheck},
	{"deploy", "SHIP", "Review, build and run this app in preview (--env prod to promote)", true, runDeploy},
	{"status", "SHIP", "Show what this app runs and what it needs to reach production", true, runStatus},
	{"logs", "SHIP", "Stream this app's logs from preview or production", false, runLogs},
	{"secret", "SHIP", "Set the keys, passwords and settings this app reads when it runs", false, runSecret},
	{"share", "THIS APP", "Who can open this app", false, runShare},
	{"rename", "THIS APP", "Give this app another name", false, runRename},
	{"pull", "THIS APP", "Download the code an app is running", false, runPull},
	{"db", "THIS APP", "Query this app's database, and list or end database sessions", true, runDB},
	{"undeploy", "THIS APP", "Take preview or production offline (data is kept)", false, runUndeploy},
	{"delete", "THIS APP", "Delete this app and destroy its data", false, runDelete},
	{"fleet", "YOUR ORG", "List every app in your org and what needs attention", false, runFleet},
	{"resources", "YOUR ORG", "List the databases and services this Canyon can create for apps", false, runResources},
	{"manifest", "YOUR ORG", "Show the instructions this Canyon gives coding agents", false, runManifest},
	{"login", "SETUP", "Sign this machine in to a Canyon host", false, runLogin},
	{"logout", "SETUP", "Revoke this machine's session on a Canyon host", false, runLogout},
	{"skill", "SETUP", "Teach your coding agent to use Canyon", true, runSkill},
	{"demo", "SETUP", "Start, stop or take a tour of Canyon's demo on this machine", false, runDemo},
	{"version", "SETUP", "Show the CLI version and which Canyon it talks to", true, runVersion},
	{"feedback", "SETUP", "Ask for a feature or report a problem", false, runFeedback},
	{"telemetry", "SETUP", "See or turn off the daily report this Canyon sends", true, runTelemetry},
	{"licence", "SETUP", "Show this Canyon's Enterprise licence", true, runLicence},
}

// app is one invocation. A verb's rendering goes to out and every error to err, so a piped
// `--format json` stream carries only the answer.
type app struct {
	args     []string // the command line after canyon, as it arrived: what a line asking for the same command again repeats
	out, err io.Writer
	in       io.Reader
	getenv   func(string) string
	tty      bool
	render   Render
	hostFlag string
	verb     command
	form     string          // a verb's form under it, "secret rm", which its --help names; "" for a verb with none
	ctx      context.Context // the invocation's, handed to the verb: a line fail() adds asks the host within it
	clock    clock           // what the elapsed timing reads: wall time in the binary, a counter in a test
	declined bool            // an exit 1 the person chose, not one a host handed back
	http     *Client
	conf     *config
	other    *config          // the hosts/ file http's session came from: nil when config.json holds that host, or hosts/ holds no file for it this CLI will use
	ignored  map[string]bool  // the files ignore() has named this invocation, each once
	live     io.Writer        // stdout while park() has the narration parked (park.go)
	parked   *strings.Builder // the parked transcript under ai; nil under json, which discards it
	wrote    string           // what §3.2's setup wrote this run, for the ai block to say
	moved    string           // §3.6's main-moved line when the deploy went on, for the ai block to say
	notice   *noticeCheck     // the once-a-day check in flight, nil when none is due (notice.go)
	noticed  bool             // whether this invocation has decided about the notice: once, at the verb's parse
	// transport is how this invocation's requests were asked for, sent as Canyon-Transport: canyon mcp sets
	// transportMCPLocal before its first call; every other verb leaves it "", the CLI's (design/specs/mcp.md §7).
	transport string
}

// printf writes a verb's rendering. It does not escape, because Canyon's own colour codes are
// control bytes: a value the server filled is spelled out where it enters a cell instead.
func (a *app) printf(f string, v ...any) { _, _ = fmt.Fprintf(a.out, f, v...) }
func (a *app) errf(f string, v ...any)   { _, _ = io.WriteString(a.err, visible(fmt.Sprintf(f, v...))) }

// visible spells a control byte out, so a value that reached an error line — a flag name, an
// argument, a host — cannot drive the terminal, whichever path formatted it. The three Unicode
// line breaks that are not '\n' are spelled out too: a terminal ignores them, but a markdown
// renderer and a model's tokenizer read U+2028 as a new line, and a host string that ends up in
// an agent's context must not be able to start a line of its own. So are the characters that
// draw nothing yet change what is read: the bidi controls, which reorder the text around them,
// the zero-width ones, and the tag characters a model reads as text a person never sees.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029, // the C1 controls too: U+009B is an 8-bit CSI on a terminal that honours them
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069),
			(r >= 0x200b && r <= 0x200f) || r == 0x2060 || r == 0xfeff:
			fmt.Fprintf(&b, "\\u%04x", r)
		case r >= 0xe0000 && r <= 0xe007f:
			fmt.Fprintf(&b, "\\U%08x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// globals are registered twice, before the verb and again on its own set: `canyon --format ai
// check` is the order an agent types.
type globals struct{ format, host *string }

func addGlobals(fs *flag.FlagSet) globals {
	return globals{fs.String("format", "", "auto | human | ai | json | sarif | csv"), fs.String("host", "", "Canyon host")}
}

// Main runs one invocation and answers the process's exit code.
func Main(args []string, stdout, stderr io.Writer, stdin io.Reader, getenv func(string) string, tty bool) int {
	return (&app{args: args, out: stdout, err: stderr, in: stdin, getenv: getenv, tty: tty, clock: wallClock}).main(args)
}

func (a *app) main(args []string) int {
	render, err := resolveRender("", a.getenv, a.tty) // before anything prints, the help included
	if err != nil {
		return a.usage(err)
	}
	a.render = render
	fs := flag.NewFlagSet("canyon", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag's report is Go-shaped; §9 owns what an error looks like
	g := addGlobals(fs)
	full, short := fs.Bool("version", false, ""), fs.Bool("v", false, "")
	if err := fs.Parse(args); err != nil { // Parse stops at the verb, so the rest begins with it
		if errors.Is(err, flag.ErrHelp) {
			return a.help(a.out)
		}
		return a.usage(err)
	}
	if err := a.adopt(g); err != nil {
		return a.usage(err)
	}
	switch rest := fs.Args(); {
	case *full || *short:
		a.printf("canyon %s\n", Version) // the flag resolves no host, so it cannot fail
	case len(rest) == 0, rest[0] == "help":
		return a.help(a.out)
	case rest[0] == "promote": // for an agent still carrying a skill from before cli.md §6.3's change
		return a.usage(errors.New("canyon promote is now: canyon deploy --env prod"))
	case rest[0] == "rollback", rest[0] == "revert": // no verb: putting a version back is a promotion (design/OPERATIONS.md §1.7)
		return a.usage(errors.New("canyon " + rest[0] + " is: canyon deploy --env prod --version <version>. canyon status lists them.")) // D5's line, its period included
	default:
		for _, c := range commands {
			if c.name == rest[0] {
				a.verb, a.ctx = c, context.Background()
				code := c.run(a.ctx, a, rest[1:])
				// A host that refused the verb has not earned a rewrite, but a person who said no to
				// one that answered has: prod's question is the first whose exit 1 is nobody's failure.
				if code != exitError || a.declined {
					a.remember() // a session it rotated outlives the call that earned it
				}
				a.noticeEnd() // after the verb's own output, and only what has already arrived
				return code
			}
		}
		a.errf("\n  Unknown command %q.\n", rest[0])
		a.help(a.err)
		return exitError
	}
	return exitOK
}

// adopt applies the globals wherever on the line they were given.
func (a *app) adopt(g globals) error {
	if *g.format != "" {
		render, err := resolveRender(*g.format, a.getenv, a.tty)
		if err != nil {
			return err
		}
		a.render = render
	}
	if *g.host != "" {
		a.hostFlag = *g.host
	}
	return nil
}

// verbArgs is what a verb's --help names before its flags, for a verb that takes an argument.
var verbArgs = map[string]string{"login": "[<url>] ", "logout": "[<url>] ", "secret set": "NAME ", "secret rm": "NAME ", "rename": "NAME ", "db query": "SQL ", "db end": "EMAIL|ID "} // NAME as `canyon secret --help` writes it

// parse gives the verb the globals again and refuses both an argument it cannot read and a format
// it cannot render.
func (a *app) parse(args []string, own func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet("canyon "+a.verb.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g := addGlobals(fs)
	own(fs)
	// Flags are read wherever they stand, before or after an argument; flag.Parse alone stops at the
	// first argument and would drop every later flag unread. After "--" everything is an argument.
	head, tail := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	var positional []string
	for {
		if err := fs.Parse(head); err != nil {
			if errors.Is(err, flag.ErrHelp) { // §8.3's layout: PrintDefaults writes single-dash Go style
				name := cmp.Or(a.form, a.verb.name) // the form typed, `secret rm`, never only the verb it is dispatched under
				a.printf("\nUSAGE\n  canyon %s %s[flags]\n\n%s\n", name, verbArgs[name], a.render.paint(ansiBold, "FLAGS"))
				fs.VisitAll(func(f *flag.Flag) {
					if f.Usage != "" { // a flag with no usage is refused by name and never offered
						a.printf("  --%-15s %s\n", f.Name, f.Usage)
					}
				})
			}
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		head = fs.Args()[1:]
	}
	positional = append(positional, tail...)
	if err := a.adopt(g); err != nil {
		return nil, err
	}
	if a.render.Format == FormatJSON && !a.verb.json {
		return nil, fmt.Errorf("%s has no json rendering", a.verb.name)
	}
	if a.render.Format == FormatSARIF && a.verb.name != "check" {
		return nil, fmt.Errorf("%s has no sarif rendering", a.verb.name)
	}
	if a.render.Format == FormatCSV && a.form != "db query" {
		return nil, fmt.Errorf("%s has no csv rendering", cmp.Or(a.form, a.verb.name))
	}
	a.noticeStart() // now that the verb's own --format is known: a harness's run sends nothing
	return positional, nil
}

// host is resolved when a verb needs to reach one and not before, so a machine whose CANYON_HOST
// is wrong can still be asked for help.
func (a *app) host() (string, error) { return resolveHost(a.hostFlag, a.getenv, a.config()) }

// client is the one client this invocation uses, built on first need: a verb that makes two calls
// keeps one session across them.
func (a *app) client() (*Client, error) {
	if a.http == nil {
		host, err := a.host()
		if err != nil {
			return nil, err
		}
		a.http = newClient(host)
		a.http.Harness = harnessOf(a.getenv)
		a.http.Transport = a.transport
		c := a.config()
		if c.Host != host { // config.json is authoritative for its host; every other is in hosts/ (cli.md §8.4)
			c = a.hostConfig(host)
			a.other = c
		}
		if c != nil {
			a.http.Public = c.Public
			a.http.seed(c.Session) // this host's session and no other's: --host b never carries a's
		}
	}
	return a.http, nil
}

// header is the dim first line of every verb that changes an app or its secrets, printed once the app
// and host are known, before any guard or read: this binary, the app, what the verb moves it to, and
// the host as dialled. A checkout no guard can catch — no base, an app both hosts have — still says which Canyon it
// is about to change (design/OPERATIONS.md §2.3).
func (a *app) header(app, moves string) {
	a.printf("\n  %s\n", a.render.paint(ansiDim, fmt.Sprintf("canyon %s %s %s%s %s %s", Version, a.dot(), cell(app), moves, a.dot(), cell(a.http.name()))))
}

// resolveHost is the precedence, weakest rung first: the demo, the host `canyon login` saved,
// CANYON_HOST, --host. canyon.toml never names a host. The config arrives as an argument rather
// than being read here, so a path that resolves no host — `canyon --help` — touches no file.
func resolveHost(flagged string, getenv func(string) string, saved *config) (string, error) {
	pick, from := defaultHost, "the default host"
	if saved.Host != "" {
		pick, from = saved.Host, "the host canyon login saved"
	}
	if v := getenv("CANYON_HOST"); v != "" {
		pick, from = v, "CANYON_HOST"
	}
	if flagged != "" {
		pick, from = flagged, "--host"
	}
	return validHost(from, pick)
}

// validHost is the shape a Canyon host has to have, whichever rung named it — `canyon login` checks
// the URL it is handed against these same rules. The rule is canyon-server's own publicURLOf, so a
// username or password can neither be printed here nor sent as basic auth; a lone trailing slash is
// forgiven. Every refusal quotes the value, or escape bytes in it would rewrite the terminal. What
// it returns is cli.md §8.2's one canonical form, from the URL alone: it names a host's file and
// settles every comparison of hosts, so a session is offered only where that form is the file's.
func validHost(from, pick string) (string, error) {
	u, err := url.Parse(pick)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return "", fmt.Errorf("%s %q must be http(s)://host[:port]", from, pick)
	case u.User != nil:
		return "", fmt.Errorf("%s %q carries a username or password, which a Canyon host never takes", from, pick)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("%s %q must be a bare host, with no path, query or fragment", from, pick)
	}
	host := strings.ToLower(u.Hostname()) // one host, however it was typed: the stored host is compared to it byte-for-byte
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal, which Hostname unwrapped
	}
	if u.Port() != "" { // a number, so :0443 is :443; the default port, or a bare colon, names the host without one
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("%s %q must be http(s)://host[:port]", from, pick)
		}
		if port != defaultPorts[u.Scheme] {
			host += ":" + strconv.Itoa(port)
		}
	}
	u.Host, u.Path = host, ""
	return u.String(), nil
}

// defaultPorts is the port each scheme names when it names none, which the canonical form drops.
var defaultPorts = map[string]int{"http": 80, "https": 443}

// usage answers a bad invocation on stderr: one line, no stack trace, exit 1.
func (a *app) usage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK // --help already printed the flags, and is not a failure
	}
	a.unpark(false) // a refusal stands alone under ai, as fail's do: the parked first line is dropped
	a.errf("\n  %s %s\n", a.render.glyph(stateFail), err)
	return exitError
}
