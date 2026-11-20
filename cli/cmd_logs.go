package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// logsPiece is the most of one line held before it is printed: a longer line reaches the screen
// in pieces, never buffered whole, because the stream is the app's bytes and an app can write a
// line of any length.
const logsPiece = 64 << 10

// logsLines is how many of the last lines come first when --lines is not given, and the most it
// may ask for (contracts/routes.md, the logs row).
const (
	logsLines    = 200
	maxLogsLines = 10000
)

// runLogs prints the container's stdout+stderr for one environment (cli.md §6.4). The route is a
// raw passthrough (contracts/routes.md): no timestamp, no framing, the last lines and then, when
// it follows, live until this end closes it. It follows on a person's terminal, or when --follow
// says so; an agent or a pipe gets the last lines and the end, because a stream that never ends
// hangs whatever is waiting on it.
func runLogs(ctx context.Context, a *app, args []string) int {
	var env, named, redaction string
	var follow bool
	var lines int
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&env, "env", "preview", "preview | prod")
		fs.StringVar(&named, "app", "", "The app to read, when it is not the one "+projectFile+" names")
		fs.IntVar(&lines, "lines", logsLines, "How many of the last lines to print first")
		fs.BoolVar(&follow, "follow", false, "Keep printing new lines until Ctrl-C (the default on a terminal)")
		fs.BoolVar(&follow, "f", false, "")           // --follow's short form, never offered: the skill text named it
		fs.StringVar(&redaction, "redaction", "", "") // canyon_logs's masked read (design/specs/mcp.md §1.2), never offered: the host refuses any other
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("logs takes no arguments, and got %q", rest[0]))
	}
	// Unlike secret rm, the env is checked here: the heading below is built from it before a byte
	// arrives, and a stream is opened on the host's side for it, so a word that is not one of the two
	// never reaches the host.
	if env != "preview" && env != "prod" {
		return a.usage(fmt.Errorf("--env is preview or prod, and got %q", env))
	}
	if lines < 1 || lines > maxLogsLines {
		return a.usage(fmt.Errorf("--lines is 1 to %d, and got %d", maxLogsLines, lines))
	}
	follow = follow || a.tty && a.render.Format == FormatHuman
	app := named
	if app == "" {
		dir, err := a.getwd()
		if err != nil {
			return a.usage(err)
		}
		p, err := readProject(dir) // logs reads the file; writing it is deploy's alone (§3.2)
		if err != nil {
			return a.usage(err)
		}
		app = p.App
	} else if len(app) > maxProjectName || !names.ValidProject(app) { // the same two rules readProject applies to canyon.toml
		return a.usage(fmt.Errorf("--app %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter, at most %d characters", app, maxProjectName))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	// Ctrl-c ends the stream, not the process: the context cancels, the read returns, nothing more
	// prints, and the exit is 0, because stopping a tail is what the person asked for.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	q := url.Values{"env": {env}, "lines": {strconv.Itoa(lines)}, "follow": {strconv.FormatBool(follow)}}
	if redaction != "" {
		q.Set("redaction", redaction)
	}
	at := "/api/v1/projects/" + url.PathEscape(app) + "/logs?" + q.Encode()
	body, header, err := client.stream(ctx, at, "text/plain")
	if err != nil {
		if ctx.Err() != nil {
			return exitOK
		}
		if named == "" { // canyon.toml names the app: guard (a), when its own read says the host lacks it (§3.2)
			err = lackingAsked(ctx, err, app, client)
		}
		return a.logsFail(err, app, env)
	}
	defer func() { _ = body.Close() }()
	dot := a.dot()
	what := fmt.Sprintf("last %d lines", lines)
	if follow {
		what = "live " + dot + " Ctrl-C to stop"
	}
	a.printf("\n  %s\n\n", a.render.paint(ansiBold, cell(app)+" "+dot+" "+env+" "+dot+" "+what))
	if redaction := header.Get("Canyon-Log-Redaction"); redaction != "" && redaction != "none" { // a day-2 shipper; absent reads as none
		a.printf("  %s\n\n", a.faint("the host reports redaction "+cell(redaction)))
	}
	project := "/api/v1/projects/" + url.PathEscape(app)
	if follow { // a followed stream never ends on its own: what Canyon refused comes first (cli.md §6.4.1)
		if said := a.logsRefused(ctx, client, project, env); said != "" {
			a.printf("%s\n", said)
		}
	}
	code, said := a.tail(ctx, client.Base, body)
	if !follow && code == exitOK && !said && ctx.Err() == nil { // the heading alone read as a failure to read (#2890)
		a.printf("  No log lines yet.\n")
	}
	if !follow && code == exitOK && ctx.Err() == nil { // ctrl-c prints nothing more
		if said := a.logsRefused(ctx, client, project, env); said != "" {
			a.printf("\n%s", said)
		}
	}
	return code
}

// logsRefused is the destinations Canyon refused the env named which since its running version went live, the cause
// beside the symptom: a client may log a refused call only as a failure, as Node's fetch logs "Request was cancelled"
// (#2708). A request's count is every refusal since Canyon first refused the destination, so the window names the
// destinations, never the calls. The reads are status's: the app's two, a contributor's and up as the log is, and the
// host's via and runtime, every member's. The log names refusals only where status lists them (outboundHidden,
// outboundSaid), so it never sends an agent to a status that says nothing of them. A read that fails says so, and the
// log keeps its exit; one ctrl-c ended says nothing. "" when nothing was refused.
func (a *app) logsRefused(ctx context.Context, c *Client, project, which string) string {
	var read struct {
		Project struct {
			Envs map[string]*env `json:"envs"`
		} `json:"project"`
	}
	var door struct {
		Runtime string `json:"runtime"`
	}
	var s statusRead
	err := c.get(ctx, project, &read)
	if err == nil {
		if s.Outbound = outboundOf(ctx, c, project); s.Outbound.Unread != "" {
			err = errors.New(s.Outbound.Unread)
		}
	}
	if err == nil {
		err = c.get(ctx, "/api/v1/front-door", &door)
	}
	switch {
	case err != nil && ctx.Err() != nil: // ctrl-c prints nothing more
		return ""
	case err != nil:
		return "  canyon: couldn't read what Canyon refused: " + cell(err.Error()) + "\n"
	}
	s.Kube = door.Runtime == "kubernetes"
	ran, rec := read.Project.Envs[which], s.Outbound.Envs[which]
	if ran == nil || s.Outbound.outboundHidden() || outboundSaid(s, ran, rec) != "" { // "" is an env through Canyon, rec its records
		return ""
	}
	refused, allowed := rec.refusedSince(ran.DeployedAt), rec.allowedNow(ran.DeployedAt, "running")
	if len(allowed) > 0 { // the app's read carries no state: only the status read says the env runs, and only then do its calls go through (#2722)
		var now statusRead
		state := ""
		err := c.get(ctx, project+"/status", &now)
		if ctx.Err() != nil { // ctrl-c prints nothing more, the refusals read before included
			return ""
		}
		if err == nil && now.Envs[which] != nil {
			state = now.Envs[which].State
		}
		allowed = rec.allowedNow(ran.DeployedAt, state) // a read that fails names none: it never says calls go through
	}
	var said strings.Builder
	asOf := a.clock.now().UTC().Format(time.RFC3339)
	for _, r := range refused {
		said.WriteString("  canyon: refused " + refusedCalls(r, a.dot()) + "\n")
	}
	if len(refused) > 0 {
		said.WriteString("  canyon: As of " + asOf + ", Canyon has refused calls to these destinations since " + a.version(ran) +
			" went live. The app may log them only as failed or cancelled. Canyon also refuses any call out to the internet to an IP address or a port other than 80 or 443, which isn't listed here. Run canyon status for what to do.\n")
	}
	for _, al := range allowed { // so an agent can tell the app's failures from before the Allow from its calls since (#2722)
		said.WriteString("  canyon: allowed " + allowedBy(al, a.dot()) + " " + a.dot() + " last refused " + cell(*al.LastRefusedAt) + "\n")
	}
	if len(allowed) > 0 {
		said.WriteString("  canyon: As of " + asOf + ", a person has allowed these destinations in " + envNames[which] + " since " + a.version(ran) +
			" went live, and " + envNames[which] + "'s calls to them go through now, with no deploy. The app's log may still show failures from before each was allowed.\n")
	}
	return said.String()
}

// tail prints the stream as it arrives: each line indented two and made visible, because the
// server sends the app's bytes untouched and the terminal must not take the app's control bytes
// raw. A line longer than logsPiece is printed as it is read, in pieces.
// tail prints the stream as it comes: every byte but the newline is the app's and is spelled out
// by visible (a CR ends up as \x0d, whether alone or before the newline), a line longer than a
// piece prints in pieces and is never held whole, and a last line without a newline gets one. said is whether any
// line of the app's printed.
// keepalive is the line the server writes through a quiet stretch (contracts/routes.md, the logs
// row): the stream's, not the app's, so it is never printed.
const keepalive = "canyon: keepalive\n"

func (a *app) tail(ctx context.Context, host string, body io.Reader) (int, bool) {
	r, indent, said := bufio.NewReaderSize(body, logsPiece), "  ", false
	for {
		piece, err := r.ReadSlice('\n')
		if n := len(piece); n > 0 && (indent != "  " || string(piece) != keepalive) {
			if !said && a.render.Format == FormatAI { // an agent reads the app's own lines as data, each indented two below
				a.printf("  %s\n", appSays)
			}
			said = true
			if piece[n-1] == '\n' {
				a.printf("%s%s\n", indent, visible(string(piece[:n-1])))
				indent = "  "
			} else {
				a.printf("%s%s", indent, visible(string(piece)))
				indent = ""
			}
		}
		switch {
		case err == nil || errors.Is(err, bufio.ErrBufferFull): // a whole line, or one piece of a long one
			continue
		case ctx.Err() != nil: // ctrl-c: the read ended because this end ended it
			return exitOK, said
		case errors.Is(err, io.EOF): // the container stopped, or the host closed a whole stream
			if indent == "" {
				a.printf("\n")
			}
			return exitOK, said
		}
		return a.fail(fmt.Errorf("%s: the log stream ended: %w", host, err)), said
	}
}

// logsFail is the two refusals this route has its own words for; the rest print as every verb's do.
func (a *app) logsFail(err error, app, env string) int {
	glyph := a.render.glyph(stateFail)
	if _, gone := refusal(err, http.StatusNotFound, "not_found"); gone {
		a.errf("\n  %s Nothing is deployed in %s for %s, or there is no such app.\n", glyph, env, cell(app))
		return exitError
	}
	if _, gone := refusal(err, http.StatusBadGateway, "app_unreachable"); gone {
		a.errf("\n  %s %s isn't running in %s any more. Deploy it again, or take %s offline on its page.\n", glyph, cell(app), envNames[env], envNames[env])
		return exitError
	}
	return a.fail(err)
}
