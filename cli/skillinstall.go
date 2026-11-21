package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// skillTarget is one place the skill can go (cli.md §7.1). Two harnesses are found by their own
// directory being here, which Canyon never creates: a ~/.claude it made would be a harness that is
// not installed. AGENTS.md is opt-in, because one file many tools share says nothing about which
// of them is here.
type skillTarget struct {
	name, harness, shown string // the row's name, its --harness word, and the path as the row shows it
	own, ownShown        string // the harness's own directory; "" for AGENTS.md, which has none
	path                 string
	whole                bool         // Canyon owns the file: a copy with no block is replaced, never appended to
	hook                 *sessionHook // the session hook beside the skill (hooks.go); nil for AGENTS.md, which the skill's own sentence reaches
	mcp                  *mcpConfig   // canyon mcp in the harness's MCP config (mcpconfig.go); nil for AGENTS.md, which names no harness
}

// startClaude is how a machine with no harness gets one: Claude Code makes ~/.claude on its first start,
// and Canyon creates no harness's directory (cli.md §7.1).
const startClaude = "Start Claude Code once, then run this again."

// Self is this binary as the hook in ~/.claude/settings.json runs it: the path install was run
// from, which main sets. A bare canyon would need canyon on the PATH every Claude Code session
// starts with, and fails there with exit 127 when it is not. "canyon" only when main could find no
// path at all.
var Self = "canyon"

// Locate is the path the shell ran argv0 from, made absolute: a bare name through PATH, as the
// shell found it — a relative PATH entry included, which Go answers with exec.ErrDot beside the
// file the shell did run — and a path against the working directory. A symlink is kept, not
// resolved, so a binary reached through one keeps the stable name an upgrade replaces in place.
func Locate(argv0 string) (string, error) {
	if !strings.ContainsRune(argv0, filepath.Separator) {
		found, err := exec.LookPath(argv0)
		if err != nil && !errors.Is(err, exec.ErrDot) {
			return "", err
		}
		argv0 = found
	}
	return filepath.Abs(argv0)
}

// goos is the platform this canyon runs on; a var so a test can be on Windows.
var goos = runtime.GOOS

func (a *app) skillTargets() []skillTarget {
	home := a.getenv("HOME")
	// Claude Code's hook is global and this machine's alone, so it runs this binary by path, in exec
	// form: Claude Code spawns command with args and no shell, so no path needs quoting on any
	// platform. Codex runs a hook as one shell command and ignores args (cli.md §7.1, checked on Codex
	// 0.164.0), so its entry is the same path quoted for a POSIX shell; a path no POSIX shell can spell
	// is run by name, as when main found no path at all. Cursor's ./.cursor/hooks.json is the project's,
	// often committed, and one machine's path would break the hook for everyone else who opens it:
	// there the bare name stays.
	self, _ := encodeJSON(Self, false) // a string, so the encoder cannot fail
	quoted, err := syntax.Quote(Self, syntax.LangPOSIX)
	if err != nil {
		quoted = "canyon"
	}
	codexCommand, _ := encodeJSON(quoted+" manifest --harness "+targetCodex, false) // a string, as above
	claude := &sessionHook{"~/.claude/settings.json", filepath.Join(home, ".claude", "settings.json"), "SessionStart",
		`{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":` + strings.TrimSuffix(string(self), "\n") +
			`,"args":["manifest","--harness","` + targetClaude + `"]}]}`, "{}",
		"It runs this canyon by its path: if you move it, run canyon skill install again from the new place."}
	codex := &sessionHook{"~/.codex/hooks.json", filepath.Join(home, ".codex", "hooks.json"), "SessionStart",
		`{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":` + strings.TrimSuffix(string(codexCommand), "\n") + `}]}`, "{}",
		"It runs this canyon by its path: if you move it, run canyon skill install again from the new place.\n    Codex asks you to trust it the next time it starts; /hooks in Codex shows it any time."}
	if goos == "windows" { // a POSIX-quoted command is no command a Windows shell reads: the skill's own sentence asks for the manifest there
		codex = nil
	}
	cursor := &sessionHook{"./.cursor/hooks.json", filepath.Join(".cursor", "hooks.json"), "sessionStart",
		`{"command":"canyon manifest --harness ` + targetCursor + `"}`, `{"version":1}`, ""}
	// The MCP server, the same way: this binary by its path in the machine's own files, by name in Cursor's
	// project file (design/specs/mcp.md §1.4).
	claudeMCP := &mcpConfig{"~/.claude.json", filepath.Join(home, ".claude.json"), Self, true, false}
	codexMCP := &mcpConfig{"~/.codex/config.toml", filepath.Join(home, ".codex", "config.toml"), Self, false, true}
	cursorMCP := &mcpConfig{"./.cursor/mcp.json", filepath.Join(".cursor", "mcp.json"), "canyon", true, false}
	return []skillTarget{
		{"Claude Code", targetClaude, "~/.claude/skills/canyon/SKILL.md", filepath.Join(home, ".claude"), "~/.claude", filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md"), true, claude, claudeMCP},
		// Codex is found by ~/.codex, and reads its skills from ~/.agents/skills, which it does not make
		{"Codex", targetCodex, "~/.agents/skills/canyon/SKILL.md", filepath.Join(home, ".codex"), "~/.codex", filepath.Join(home, ".agents", "skills", "canyon", "SKILL.md"), true, codex, codexMCP},
		{"Cursor", targetCursor, "./.cursor/rules/canyon.mdc", ".cursor", "./.cursor", filepath.Join(".cursor", "rules", "canyon.mdc"), true, cursor, cursorMCP},
		{"AGENTS.md", targetAgents, "./AGENTS.md", "", "", "AGENTS.md", false, nil, nil},
	}
}

// chosen is what one run writes: without --harness, the harnesses whose directory is here; with
// all, those and AGENTS.md; with a name, that one, refused when its directory is not here.
func (a *app) chosen(pick string) ([]skillTarget, error) {
	if a.getenv("HOME") == "" { // ~/.claude would otherwise resolve under the working directory
		return nil, errors.New("HOME is not set, so ~/.claude cannot be found")
	}
	var picked []skillTarget
	for _, t := range a.skillTargets() {
		here := t.own != "" && isDir(t.own)
		switch {
		case pick == t.harness && t.own != "" && !here && t.harness == targetClaude: // Claude Code makes its directory on its first start
			return nil, fmt.Errorf("%s is not set up here: %s does not exist. %s", t.name, t.ownShown, startClaude)
		case pick == t.harness && t.own != "" && !here:
			return nil, fmt.Errorf("%s is not set up here: %s does not exist, and Canyon does not create it", t.name, t.ownShown)
		case pick == t.harness, here && (pick == "" || pick == "all"), t.own == "" && pick == "all":
			picked = append(picked, t)
		}
	}
	if pick != "" && pick != "all" && len(picked) == 0 {
		return nil, unknownHarness(pick)
	}
	return picked, nil
}

// unknownHarness refuses a --harness value install and uninstall do not know, with the ones they do.
func unknownHarness(pick string) error {
	return fmt.Errorf("unknown --harness value %q: use %s, %s, %s, %s or all", pick, targetClaude, targetCodex, targetCursor, targetAgents)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// skillInstall is `canyon skill install`: §7.1's transcript, one row per target. It has no json
// rendering, as no verb that writes files has — the text a script would read is `--print`'s.
func (a *app) skillInstall(args []string) int {
	var pick string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&pick, "harness", "", targetClaude+" | "+targetCodex+" | "+targetCursor+" | "+targetAgents+" | all    (default: the coding agents found here)")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("skill install takes no arguments; got %q", rest[0]))
	}
	if a.render.Format == FormatJSON {
		return a.usage(errors.New("skill install has no --format json; canyon skill --print is the text a script reads"))
	}
	targets, err := a.chosen(pick)
	if err != nil {
		return a.usage(err)
	}
	a.printf("\n  Looking for coding agents in this environment%s\n\n", a.render.punct("…", "..."))
	if len(targets) == 0 {
		a.errf("  %s No coding agent found: neither ~/.claude, ~/.codex nor ./.cursor exists here. %s\n", a.render.glyph(stateFail), startClaude)
		a.agentsHint(a.errf, false)
		return exitError
	}
	agents, codex, failed, denied := false, false, false, false
	row := func(name, shown, status string, err error) {
		failed = a.installRow(name, shown, status, err) || failed
		denied = denied || errors.Is(err, os.ErrPermission)
	}
	for _, t := range targets { // a refused file does not hide the rows after it: the run reports every file, then exits 1
		status, err := installSkill(t)
		row(t.name, t.shown, status, err)
		if t.hook == nil && t.harness == targetCodex && err == nil {
			a.printf("  %s Codex's session hook is not set up on Windows: the agent runs canyon manifest itself, as the skill asks.\n", a.render.glyph(statePending))
		}
		if t.hook != nil {
			status, err := a.installHook(t.hook)
			row(t.name, t.hook.shown, status, err)
		}
		if t.mcp != nil {
			status, err := a.installMCP(t.mcp)
			row(t.name, t.mcp.shown, status, err)
		}
		agents = agents || (t.own == "" && err == nil) // a refused AGENTS.md is still offered the hint
		codex = codex || t.harness == targetCodex
	}
	a.sandboxed(denied)
	if !agents {
		a.agentsHint(a.printf, codex)
	}
	if failed {
		return exitError
	}
	a.printf("\n  Your agent can now deploy. Try asking it:\n    \"deploy this app\"\n")
	return exitOK
}

// installRow is one row of §7.1's transcript, install's or uninstall's, or its refusal on stderr, which
// is what it answers. A row that wrote or removed Canyon's is ticked; any other is dotted.
func (a *app) installRow(name, shown, status string, err error) (failed bool) {
	if err != nil {
		a.errf("\n  %s %s\n", a.render.glyph(stateFail), err)
		return true
	}
	glyph := a.render.glyph(statePending)
	if status == "written" || strings.HasSuffix(status, "removed") {
		glyph = a.render.paint(ansiGreen, a.render.glyph(statePass))
	}
	a.printf("  %s %s   %s   %s\n", glyph, pad(name, 12), pad(shown, 39), status)
	return false
}

// agentsHint is the dotted line: AGENTS.md, and how to opt in to it. It names only agents whose own
// docs say they read AGENTS.md as it is; one that needs a setting first is the docs site's. Codex is
// among them only while it is not set up here, neither written by this run nor ~/.codex found, since
// Codex has its own skill and hook (#2659); where this run wrote nothing of Codex's, it points a Codex
// user at them.
func (a *app) agentsHint(p func(string, ...any), codex bool) {
	agents := "Codex, Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md"
	if codex || isDir(filepath.Join(a.getenv("HOME"), ".codex")) {
		agents = "Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md"
	}
	p("  %s %s:\n      canyon skill install --harness %s      appends to ./AGENTS.md\n", a.render.glyph(statePending), agents, targetAgents)
	if !codex {
		p("      canyon skill install --harness %s          Codex's own skill and hook, once Codex has made ~/.codex\n", targetCodex)
	}
}

// installSkill writes t's file and answers written or unchanged. An existing file keeps every byte
// outside Canyon's block: the block's bytes are replaced, a file with none is appended to after a
// trailing newline is ensured, or replaced when Canyon owns it whole. A block a later release wrote,
// a pair findBlock refuses, and an AGENTS.md that is a link to another file and needs a write are
// each refused with nothing written, and a file whose bytes would not change is not touched.
func installSkill(t skillTarget) (string, error) {
	block := skillBlock(skillBody)
	next := skillFronts[t.harness] + block
	old, err := os.ReadFile(t.path) //nolint:gosec // one of the three paths above, none of them from input
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("%s cannot be read: %s", t.shown, plainly(err))
	}
	if err == nil {
		file := string(old)
		ref, found, err := findBlock(t.shown, file)
		switch {
		case err != nil:
			return "", err
		case found && ref.Version != blockVersion:
			return "", newerBlock(t.shown, file, ref)
		case found && t.whole: // Canyon owns the file: the front matter is this release's too, since the description is what makes the skill fire
			next = skillFronts[t.harness] + block
		case found:
			next = file[:ref.Start] + block + file[ref.End:]
		case !t.whole:
			next = file + block
			if file != "" && !strings.HasSuffix(file, "\n") {
				next = file + "\n" + block
			}
		}
		if next == file {
			return "unchanged", nil
		}
	}
	if !t.whole && linked(t.path) { // a file Canyon edits and does not own: the rename below would leave a copy where the person's link was
		return "", fmt.Errorf("%s is a link to another file, which install does not rewrite: put Canyon's block, from canyon skill --print --harness %s, into that file by hand", t.shown, t.harness)
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o750); err != nil { // below the harness's own directory, which chosen saw, or Codex's ~/.agents/skills, which is no harness's own
		return "", refused(t.shown, "written", err)
	}
	if err := atomicWrite(t.path, []byte(next), false); err != nil {
		return "", refused(t.shown, "written", err)
	}
	return "written", nil
}

// linked is whether path is a symbolic link: a person's, to a file of their own (an AGENTS.md that
// points at CLAUDE.md, a settings.json in a dotfiles repository), which atomicWrite's rename would
// replace with a copy. A file Canyon only edits is never written through one.
func linked(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// newerBlock refuses a block a later release wrote, by its line: this build does not know its format,
// so it neither rewrites nor removes it.
func newerBlock(shown, file string, ref skillBlockRef) error {
	return fmt.Errorf("%s:%d opens a Canyon block in format v:%s, which this build writes as v:%s. Upgrade canyon, and run this again",
		shown, strings.Count(file[:ref.Start], "\n")+1, ref.Version, blockVersion)
}

// refused is a write or a removal the filesystem would not do, said by the path the row shows and the
// half of why a person reads (plainly), with the error kept so a run can ask whether it was a
// permission, which is what Codex's sandbox answers (sandboxed).
func refused(shown, done string, err error) error {
	var path *fs.PathError
	if errors.As(err, &path) {
		err = path.Err
	}
	return fmt.Errorf("%s cannot be %s: %w", shown, done, err)
}

// sandboxed is the line under a run whose write a permission refused, inside Codex: Codex sets
// CODEX_SANDBOX for the commands it runs in its sandbox (§5.1), which keeps them out of ~/.claude,
// ~/.codex and ~/.agents, and a bare "operation not permitted" says neither that nor how to get past
// it. It repeats the command as it was typed, so the agent that ran it can run that again.
func (a *app) sandboxed(denied bool) {
	if denied && a.getenv("CODEX_SANDBOX") != "" {
		a.errf("\n  %s This ran inside Codex's sandbox, which refuses writes outside the project. Run %s again with permission to leave the sandbox (the user approves it in Codex), or ask the user to run it in a terminal outside Codex.\n",
			a.render.glyph(statePending), a.commandLine())
	}
}

// commandLine is this run's command line as a POSIX shell reads it back: canyon, then each argument as it
// arrived, quoted only where a shell would otherwise read it differently.
func (a *app) commandLine() string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-" // a word of only these needs no quotes (shlex's set)
	words := []string{"canyon"}
	for _, arg := range a.args {
		quoted, err := arg, error(nil)
		if arg == "" || strings.Trim(arg, plain) != "" {
			quoted, err = syntax.Quote(arg, syntax.LangPOSIX)
		}
		if err != nil { // a byte no POSIX shell can spell: Go's quoting says it plainly instead
			quoted = strconv.Quote(arg)
		}
		words = append(words, quoted)
	}
	return strings.Join(words, " ")
}
