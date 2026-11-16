package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// actorRef is who a row names: the kind and the subject every filter and ownership check keys on,
// plus the display name that is only ever printed.
type actorRef struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Display string `json:"display"`
}

// secretRow is routes.md row 176's shape, whole. Fields, never a map: a server that renames one
// leaves a blank cell here rather than a line of JSON on the screen.
type secretRow struct {
	Name    string   `json:"name"`
	Env     string   `json:"env"`
	Where   string   `json:"where"`
	Version int      `json:"version"`
	SetBy   actorRef `json:"set_by"`
	SetAt   string   `json:"set_at"`
	Origin  string   `json:"origin"`
}

type secretsList struct {
	Secrets []secretRow `json:"secrets"`
}

type secretPut struct {
	Value string `json:"value"`
	Env   string `json:"env"`
	Where string `json:"where"`
}

// runSecret reads the subcommand itself, before a.parse, which refuses a flag that follows a
// positional: a later form's own flag — `canyon secret set NAME --env prod` — has to reach the
// form that owns it, not die on the way in.
func runSecret(ctx context.Context, a *app, args []string) int {
	if len(args) == 0 {
		a.secretForms(a.out)
		return exitOK // the map, for someone who typed the verb to find out what it does
	}
	switch args[0] {
	case "-h", "-help", "--help", "help": // asking how it works is no unknown command
		a.secretForms(a.out)
		return exitOK
	case "list":
		a.form = "secret list" // each form's --help names it, not the verb it is dispatched under (#888)
		return runSecretList(ctx, a, args[1:])
	case "set":
		a.form = "secret set"
		return runSecretSet(ctx, a, args[1:])
	case "rm":
		a.form = "secret rm"
		return runSecretRm(ctx, a, args[1:])
	}
	a.errf("\n  Unknown secret command %q.\n", args[0])
	a.secretForms(a.err)
	return exitError
}

// secretForms is the three forms, and that a setting goes here as a key does. `canyon --help` prints
// one row per verb, so this is the only place they are listed, and the one that is not built says
// so: a form this binary refuses must not read as one it will run.
func (a *app) secretForms(w io.Writer) {
	p := func(f string, v ...any) { _, _ = fmt.Fprintf(w, f, v...) }
	p("\n%s\n", a.render.paint(ansiBold, "USAGE"))
	p("  canyon secret list          The names this app reads, never the values\n")
	p("  canyon secret set NAME      Store one value, read from a prompt or a pipe\n")
	p("  canyon secret rm NAME       Remove one name from one environment\n")
	p("\n  Any setting the app reads goes here, not only keys and passwords.\n")
}

func runSecretList(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("secret list takes no arguments, and got %q", rest[0]))
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	p, err := readProject(dir) // list reads the file; writing it is deploy's alone (§3.2)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	a.header(p.App, "")
	var read secretsList
	err = lacking(client.get(ctx, "/api/v1/projects/"+url.PathEscape(p.App)+"/secrets", &read), p.App, client) // guard (a), from the list's own read (§4.3)
	if err != nil {
		return a.fail(err)
	}
	a.renderSecrets(p.App, read.Secrets, time.Now())
	return exitOK // a read carries no verdict
}

// renderSecrets prints the list the host answered, in the order it answered it: the server sorts
// by env then name (routes_secrets.go), and a client that re-sorts sorts one answer.
//
// Five columns. There is no VALUE column and no mask, because no route reads a value back and a
// row of bullets would claim this binary had seen one. There is no VERSION column — v1 keeps no
// history and reads no version — and no ORIGIN column: a row Canyon made from a declared resource
// is said by its ENV, "each env", and its SET BY, Canyon. The dashboard keeps both columns.
func (a *app) renderSecrets(app string, secrets []secretRow, now time.Time) {
	a.printf("\n  %s\n\n", a.render.paint(ansiBold, cell(app)))
	if len(secrets) == 0 {
		a.printf("  No secrets set\n\n  canyon secret set NAME\n")
		return
	}
	rows := [][]string{{"NAME", "ENV", "WHERE", "SET BY", "SET"}}
	byHand, made := map[string]bool{}, map[string]bool{}
	for _, s := range secrets {
		byHand[s.Name] = byHand[s.Name] || s.Origin != "provisioned"
	}
	for _, s := range secrets {
		env := cell(s.Env)
		// Canyon makes it on each environment's deploy: one row, never a preview-only one — unless a
		// person set the same name somewhere, and then every row prints as it is.
		if s.Origin == "provisioned" && !byHand[s.Name] {
			if made[s.Name] {
				continue
			}
			made[s.Name], env = true, "each env"
		}
		// The org's data: a control byte in a name must not drive the terminal, and it is spelled
		// out before the width is measured or the column moves under it.
		rows = append(rows, []string{cell(s.Name), env, cell(s.Where), a.setBy(s.SetBy), a.ago(s.SetAt, now)})
	}
	width := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, one := range row {
			width[i] = max(width[i], utf8.RuneCountInString(one))
		}
	}
	for r, row := range rows {
		line := make([]string, len(row))
		for i, one := range row {
			line[i] = pad(one, width[i])
		}
		text := strings.TrimRight(strings.Join(line, "  "), " ")
		if r == 0 {
			text = a.render.paint(ansiDim, text)
		}
		a.printf("  %s\n", text)
	}
}

// setBy is the name the dashboard shows for the same row (web/src/ui/Actor.tsx, actorRef.name; #2705).
func (a *app) setBy(who actorRef) string {
	return a.orElse(cell(who.name()))
}

// runSecretRm removes one name from one environment.
//
// Nothing is asked first and there is no --yes. The line already names the one row it removes, so
// a question would only be a second default to learn; --yes belongs to `canyon deploy --env
// prod`, which moves an image nobody named (§6.3). And nothing is checked locally: a name that is not there, an
// env that is not one of the two, a project that is gone — each is the host's refusal, printed in
// the host's own words.
func runSecretRm(ctx context.Context, a *app, args []string) int {
	var env string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&env, "env", "preview", "preview | prod")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) != 1 {
		return a.usage(errors.New("secret rm takes one NAME: canyon secret rm DATABASE_URL"))
	}
	if env == "" { // routes.md row 177's own default, so the line below names the row that went
		env = "preview"
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	p, err := readProject(dir) // rm reads the file; writing it is deploy's alone (§3.2)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	a.header(p.App, "")
	if _, err := a.checkBase(ctx, client, dir, p.App, env, false); err != nil { // guard (b) (§6.6)
		return a.fail(err)
	}
	// env is always sent, never left for the host to default: a verb that deletes a row sends the
	// environment it is about to print.
	at := "/api/v1/projects/" + url.PathEscape(p.App) + "/secrets/" + url.PathEscape(rest[0]) + "?env=" + url.QueryEscape(env)
	if err := client.del(ctx, at); err != nil {
		return a.fail(err)
	}
	a.printf("\n  %s Removed %s from %s %s %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)),
		cell(rest[0]), cell(p.App), a.render.punct("·", "|"), cell(env))
	return exitOK
}

func runSecretSet(ctx context.Context, a *app, args []string) int {
	var env, where string
	var gate given
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&env, "env", "preview", "preview | prod")
		fs.StringVar(&where, "where", "runtime", "runtime | build")
		fs.Var(&gate, "from-gate", "") // refused by name, never listed: a flag that does nothing is no flag to offer (#1990)
	})
	if bool(gate) {
		return a.fromGate()
	}
	if err != nil {
		return a.usage(err)
	}
	switch {
	case len(rest) > 1: // never %q the argument: printing it here is the leak this verb exists to prevent
		return a.usage(errors.New("a value is never given on the command line, where the shell records it: canyon secret set NAME reads one from a prompt, or from whatever is piped in"))
	case len(rest) == 0:
		return a.usage(errors.New("secret set takes one NAME: canyon secret set DATABASE_URL"))
	}
	if env == "" { // routes.md row 175's own defaults, sent rather than left out, so the line below names the row that was written
		env = "preview"
	}
	if where == "" {
		where = "runtime"
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	p, err := readProject(dir)
	var missing *NoProjectFile
	if err != nil && !errors.As(err, &missing) {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	if missing != nil { // set works before the first deploy (#1672), so it makes the app as a first deploy does (§3.2)
		if p, err = a.setup(ctx, client, dir, "", missing.NoApp); err != nil {
			return a.fail(err)
		}
	}
	a.header(p.App, "")
	if missing == nil {
		_, err := ensureProject(ctx, client, p.App, false) // guards (a) and (b), before any value is asked for or sent (§3.2, §6.6)
		if lacks := (*notOnHost)(nil); errors.As(err, &lacks) {
			lacks.env = env
		}
		if err == nil {
			_, err = a.checkBase(ctx, client, dir, p.App, env, false)
		}
		if err != nil {
			return a.fail(err)
		}
	}
	if clearNetwork(client.Base) {
		a.errf("\n  %s %s is plain http, so this value crosses the network in clear text.\n",
			a.render.glyph(stateWarn), client.Base)
	}
	value, err := a.value(rest[0]) // last: nobody should type a credential this run was going to refuse anyway
	if err != nil {
		return a.usage(err)
	}
	// The name, the env and the where are sent exactly as typed. PORT, --env production, a name with
	// a slash: each is the host's to refuse, in the host's words, and a rule repeated here is a rule
	// that can disagree with the one that counts.
	at := "/api/v1/projects/" + url.PathEscape(p.App) + "/secrets/" + url.PathEscape(rest[0])
	if err := client.put(ctx, at, secretPut{Value: value, Env: env, Where: where}, http.StatusNoContent, nil); err != nil {
		return a.fail(err)
	}
	dot := a.render.punct("·", "|")
	a.printf("\n  %s Set %s for %s %s %s %s %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)),
		cell(rest[0]), cell(p.App), dot, cell(env), dot, cell(where))
	effect := "Takes effect at the next deploy: canyon deploy." // the command that makes it take effect (#2873)
	if env == "prod" {
		effect = "Takes effect at the next promotion: canyon deploy --env prod."
	}
	a.printf("    %s\n", a.faint(effect))
	return exitOK
}

// fromGate is the named refusal. Agents learned this flag from older fix text, and the route that
// would answer it is not registered in v1 (contracts/routes.md row 178), so the word meets a Canyon
// line rather than a parser's. It is printed nowhere else: a refusal is not a suggestion.
func (a *app) fromGate() int {
	a.errf("\n  %s canyon secret set --from-gate is not available yet.\n\n", a.render.glyph(stateFail))
	a.errf("  The gate finds the value while it scans, but nothing hands one over yet, so\n")
	a.errf("  there is nothing here to adopt.\n\n")
	a.errf("  Set the value yourself instead, and rotate the key that leaked:\n")
	a.errf("    canyon secret set NAME\n")
	return exitError
}

// clearNetwork is the fleet banner's distinction, said once to whoever is about to put a credential on the wire. Never a refusal: a trusted internal network is the customer's call.
func clearNetwork(host string) bool {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "http" {
		return false
	}
	name := u.Hostname()
	return name != "localhost" && !net.ParseIP(name).IsLoopback()
}
