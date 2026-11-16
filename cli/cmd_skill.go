package cli

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"strings"
)

// skillBody is §7.2's text from `# Canyon` onward, shipped in the binary. skilltext_test.go holds
// it byte-equal to the spec's own fence, so the two cannot drift apart without a red.
//
//go:embed skill/canyon.md
var skillBody string

// The front matter is per target, because the body is not: three harnesses read keys above the text
// and AGENTS.md is one file many tools share, so Canyon puts nothing of its own above it.
const skillDescription = `description: Ship and re-ship this app through Canyon. Use when the user asks to
  deploy, ship, publish, preview, share, or send someone a link to an app — and
  whenever a Canyon gate has blocked a deploy and needs fixing.
`

const (
	claudeFront = "---\nname: canyon\n" + skillDescription + "---\n\n" // Codex's skills take the same keys (cli.md §7.1)
	// alwaysApply is a .mdc mechanism and not text an agent reads: false is the one that matches
	// the description above it, which says when to reach for the skill rather than always.
	cursorFront = "---\n" + skillDescription + "alwaysApply: false\n---\n\n"
	agentsFront = ""
)

const (
	targetClaude = "claude-code"
	targetCodex  = "codex"
	targetCursor = "cursor"
	targetAgents = "agents-md"
)

var skillFronts = map[string]string{targetClaude: claudeFront, targetCodex: claudeFront, targetCursor: cursorFront, targetAgents: agentsFront}

// skillText is the whole file for one target: its front matter, then the body between Canyon's
// two marker lines, which is the block install writes and finds. It reads no file, resolves
// no host and consults no config — the text a release ships is the same text on every machine,
// which is what makes `--print` an audit of the binary rather than of a setup.
func skillText(harness string) (string, error) {
	front, ok := skillFronts[harness]
	if !ok {
		return "", fmt.Errorf("unknown --harness value %q: use %s, %s, %s or %s", harness, targetClaude, targetCodex, targetCursor, targetAgents)
	}
	return front + skillBlock(skillBody), nil
}

// runSkill prints under every format alike: the text is the artifact a reader audits, not a
// verdict a script parses, so json and ai hand over the same bytes rather than a wrapped copy.
func runSkill(_ context.Context, a *app, args []string) int {
	if len(args) > 0 && args[0] == "install" {
		a.form = "skill install" // its --help names the form typed, not the verb it is dispatched under
		return a.skillInstall(args[1:])
	}
	if len(args) > 0 && args[0] == "uninstall" {
		a.form = "skill uninstall"
		return a.skillUninstall(args[1:])
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help") { // asking how it works lists every form, install and uninstall included
		a.skillForms(a.out)
		return exitOK
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") { // before a.parse, which would refuse the flag after it and name neither the verb nor its form
		a.errf("\n  %s skill takes no arguments, and got %q\n", a.render.glyph(stateFail), args[0])
		a.skillForms(a.err)
		return exitError
	}
	var toStdout bool
	var harness string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.BoolVar(&toStdout, "print", false, "Write the skill text to stdout, and no file")
		fs.StringVar(&harness, "harness", targetClaude, targetClaude+" | "+targetCodex+" | "+targetCursor+" | "+targetAgents)
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("skill takes no arguments, and got %q", rest[0]))
	}
	if !toStdout {
		a.skillForms(a.out)
		return exitOK // the map, for someone who typed the verb to find out what it does
	}
	text, err := skillText(harness)
	if err != nil {
		return a.usage(err)
	}
	a.printf("%s", text)
	return exitOK
}

// skillForms is the forms this binary runs. `canyon --help` prints one row per verb, so this is the
// only place they are spelled out.
func (a *app) skillForms(w io.Writer) {
	harnesses := targetClaude + "|" + targetCodex + "|" + targetCursor + "|" + targetAgents
	forms := [][2]string{
		{"canyon skill install [--harness " + harnesses + "|all]", "Write the skill and its session hook into the coding agents here"},
		{"canyon skill uninstall [--harness " + harnesses + "|all]", "Remove the skill and its session hook from the coding agents here"},
		{"canyon skill --print [--harness " + harnesses + "]", "The skill text, on stdout"},
	}
	width := 0 // the descriptions start in one column
	for _, f := range forms {
		width = max(width, len(f[0]))
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", a.render.paint(ansiBold, "USAGE"))
	for _, f := range forms {
		_, _ = fmt.Fprintf(w, "  %-*s    %s\n", width, f[0], f[1])
	}
}
