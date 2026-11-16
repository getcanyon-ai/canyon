package cli

import (
	"fmt"
	"io"
)

// releases is where the CLI is downloaded: the one place the help and the once-a-day notice (§8.5)
// take the address from, so moving the repository is one line. install is §8.1's one-liner, the
// release's own install.sh.
const (
	releases = "https://github.com/getcanyon-ai/canyon-releases/releases"
	install  = "curl -fsSL " + releases + "/latest/download/install.sh -o install.sh && sh install.sh" // never | sh: sh runs the empty script a 404 leaves and exits 0
)

// help is cli.md §8.3's map, grouped by intent, its verb list rendered from the dispatch slice so
// a verb this binary cannot run can never appear. Stdout when asked for, stderr beside an error.
func (a *app) help(w io.Writer) int {
	p := func(f string, v ...any) { _, _ = fmt.Fprintf(w, f, v...) }
	p("\nCanyon: review, ship and govern the apps your teams build with AI.\n\n%s\n  canyon <command> [flags]\n",
		a.render.paint(ansiBold, "USAGE"))
	for _, group := range []string{"SHIP", "THIS APP", "YOUR ORG", "SETUP"} {
		printed := false
		for _, c := range commands {
			if c.group != group {
				continue
			}
			if !printed {
				p("\n%s\n", a.render.paint(ansiBold, group))
				printed = true
			}
			p("  %-11s %s\n", c.name, c.blurb)
		}
	}
	p("\n%s\n  --format <fmt>    auto | human | ai | json | sarif | csv    (default: auto)\n"+
		"  --host <url>      Which Canyon to use (default: $CANYON_HOST, then your saved host)\n  --version, -v     Print the version; given alone, before any command\n",
		a.render.paint(ansiBold, "FLAGS"))
	p("\n%s\n  %d  success        %d  error        %d  blocked by the review or by policy\n  %d  this command doesn't know how a run in Canyon ends or ended: run canyon status\n",
		a.render.paint(ansiBold, "EXIT CODES"), exitOK, exitError, exitBlocked, exitStillRunning)
	p("\n%s\n", a.render.paint(ansiDim, "Install   "+install))
	return exitOK
}
