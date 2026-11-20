package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	claudeHookEntry = `{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"/usr/local/bin/canyon","args":["manifest","--harness","claude-code"]}]}`
	codexHookEntry  = `{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"/usr/local/bin/canyon manifest --harness codex"}]}`
	cursorHookEntry = `{"command":"canyon manifest --harness cursor"}`
)

// hookFile is the two hook files read back: Cursor's version, and the entries under each event.
type hookFile struct {
	Version int
	Hooks   map[string][]json.RawMessage
}

func readHooks(t *testing.T, path string) hookFile {
	t.Helper()
	var f hookFile
	if err := json.Unmarshal([]byte(fileBytes(t, path)), &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// In exec form Canyon's entry is its arguments, manifest --harness <harness>, whatever the program is
// called; in shell form the command's fields, the program's base name canyon and then manifest. A
// substring is not a mark, and a command that is not two fields is someone else's.
func TestCanyonCommandReadsTheProgramAndItsFirstArgument(t *testing.T) {
	for _, c := range []struct {
		command string
		args    []string
		want    bool
	}{
		{"canyon manifest", nil, true}, {"canyon manifest --harness claude-code", nil, true}, {"/opt/canyon/bin/canyon   manifest", nil, true},
		{"/usr/local/bin/canyon", []string{"manifest", "--harness", "claude-code"}, true}, {"/Users/a b/it's/canyon", []string{"manifest", "--harness", "claude-code"}, true},
		{"/opt/x/canyon-dev", []string{"manifest", "--harness", "claude-code"}, true}, {`C:\Tools\CANYON.EXE`, []string{"manifest", "--harness", "claude-code"}, true},
		{"canyon.exe manifest", nil, true},
		// Codex's shell form names the path quoted: a shell's words, never a split on spaces.
		{`'/Users/a b/canyon' manifest --harness codex`, nil, true}, {`"/Users/a b/it's/canyon" manifest --harness codex`, nil, true},
		{`/Users/a\ b/canyon manifest`, nil, true}, {`/Users/a b/canyon manifest`, nil, false},
		{`canyon manifest; rm -rf ~`, nil, false}, {`canyon manifest && echo hi`, nil, false}, {`'unterminated manifest`, nil, false},
		{"", nil, false}, {"canyon", nil, false}, {"manifest canyon", nil, false}, {"mycanyon manifest", nil, false}, {"echo canyon manifest", nil, false},
		{"/usr/local/bin/canyon", []string{}, false}, {"/usr/local/bin/canyon", []string{"manifest"}, false}, {"canyon manifest", []string{"fleet"}, false},
		{"/usr/local/bin/canyon", []string{"fleet", "--harness", "claude-code"}, false}, {"./scripts/hi.sh", []string{"manifest", "--force", "x"}, false},
	} {
		if got := canyonCommand(c.command, c.args); got != c.want {
			t.Errorf("canyonCommand(%q, %q) = %v, want %v", c.command, c.args, got, c.want)
		}
	}
}

// canyonIn reads args from the file as Claude Code does: missing or null is shell form, the command's
// fields; [] is exec form with no arguments, which runs canyon bare and prints the help — never Canyon's.
func TestCanyonInReadsArgsMissingNullAndEmpty(t *testing.T) {
	for entry, want := range map[string]bool{
		`{"hooks":[{"type":"command","command":"/usr/local/bin/canyon","args":["manifest","--harness","claude-code"]}]}`: true,
		`{"hooks":[{"type":"command","command":"canyon manifest --harness claude-code"}]}`:                               true,
		`{"hooks":[{"type":"command","command":"canyon manifest --harness claude-code","args":null}]}`:                   true,
		`{"hooks":[{"type":"command","command":"/usr/local/bin/canyon","args":null}]}`:                                   false,
		`{"hooks":[{"type":"command","command":"/usr/local/bin/canyon","args":[]}]}`:                                     false,
		`{"hooks":[{"type":"command","command":"/usr/local/bin/canyon"}]}`:                                               false,
		`{"command":"canyon manifest --harness cursor"}`:                                                                 true,
	} {
		if all, _ := canyonIn(json.RawMessage(entry)); all != want {
			t.Errorf("canyonIn(%s) = %v, want %v", entry, all, want)
		}
	}
}

// One entry, however many runs: a missing file is created beside the skill, a second run finds
// its own bytes and writes nothing, and a file someone re-indented is written back once with the
// entry still alone. The transcript's rows are the goldens the install tests hold.
func TestSkillInstallRegistersTheHookOnceAcrossRuns(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	settings, hooks := filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, ".cursor", "hooks.json")
	for n := 1; n <= 2; n++ {
		out, errOut, code := run(t, env, false, "skill", "install")
		if code != exitOK || errOut != "" {
			t.Fatalf("run %d: exit %d, stderr %q", n, code, errOut)
		}
		claude, cursor := readHooks(t, settings), readHooks(t, hooks)
		if len(claude.Hooks["SessionStart"]) != 1 || compact(t, claude.Hooks["SessionStart"][0]) != claudeHookEntry {
			t.Errorf("run %d: ~/.claude/settings.json holds\n%s", n, fileBytes(t, settings))
		}
		if cursor.Version != 1 || len(cursor.Hooks["sessionStart"]) != 1 || compact(t, cursor.Hooks["sessionStart"][0]) != cursorHookEntry {
			t.Errorf("run %d: ./.cursor/hooks.json holds\n%s", n, fileBytes(t, hooks))
		}
		want := "~/.claude/settings.json                   unchanged"
		if n == 1 {
			want = "~/.claude/settings.json                   written"
		}
		if !strings.Contains(out, want) {
			t.Errorf("run %d rendered\n%s\nwant a row %q", n, out, want)
		}
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(settings, old, old))
	if _, _, code := run(t, env, false, "skill", "install"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if info, err := os.Stat(settings); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("an unchanged settings.json was rewritten: mtime %v, err %v", info.ModTime(), err)
	}
	var wide bytes.Buffer
	must(t, json.Indent(&wide, []byte(fileBytes(t, settings)), "", "    "), os.WriteFile(settings, wide.Bytes(), 0o600))
	out, _, code := run(t, env, false, "skill", "install", "--harness", targetClaude)
	if code != exitOK || !strings.Contains(out, "~/.claude/settings.json                   written") || len(readHooks(t, settings).Hooks["SessionStart"]) != 1 {
		t.Errorf("a re-indented file: exit %d, rendered\n%s\nfile\n%s", code, out, fileBytes(t, settings))
	}
}

// Keys Canyon does not own survive, byte for byte: another SessionStart hook before ours, one
// after, a one-field command that is nobody's mark, another event's hooks, and unrelated keys
// with a `<`, an `&` and a number literal in them. An older entry of ours, found by its command
// under another path and matcher, is replaced where it stands. Cursor's file merges the same way.
func TestSkillInstallKeepsEveryOtherKeyInTheHookFile(t *testing.T) {
	want := golden(t, "settings-merged.json")
	home, cwd := harnessHome(t)
	settings, hooks := filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, ".cursor", "hooks.json")
	must(t, os.WriteFile(settings, []byte(`{"permissions":{"allow":["Bash(ls:*)"]},"pad":1.50,"model":"opus",`+
		`"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo pre <tool>"}]}],`+
		`"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hello & welcome"}]},`+
		`{"matcher":"resume","hooks":[{"type":"command","command":"/opt/canyon/bin/canyon manifest --harness claude-code"}]},`+
		`{"hooks":[{"type":"command","command":"canyon"}]},`+
		`{"matcher":"clear","hooks":[{"type":"command","command":"canyon manifest --harness claude-code"},{"type":"command","command":"echo mine"}]}]}}`), 0o600))
	theirs := `{"command":"./scripts/hi.sh"}`
	must(t, os.WriteFile(hooks, []byte(`{"version":1,"hooks":{"sessionStart":[`+theirs+`],"beforeShellExecution":[{"command":"./scripts/audit.sh"}]}}`), 0o600))
	if _, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install"); code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if got := fileBytes(t, settings); got != want {
		t.Errorf("~/.claude/settings.json after install:\n%s\ntestdata/settings-merged.json wants\n%s", got, want)
	}
	cursor := readHooks(t, hooks)
	start := cursor.Hooks["sessionStart"]
	if cursor.Version != 1 || len(start) != 2 || compact(t, start[0]) != theirs || compact(t, start[1]) != cursorHookEntry || len(cursor.Hooks["beforeShellExecution"]) != 1 {
		t.Errorf("./.cursor/hooks.json after install:\n%s", fileBytes(t, hooks))
	}
}

// No ~/.claude, so no Claude Code: neither its skill nor its hook has a row, and the directory
// is not made for the hook's sake. Cursor, whose directory is here, gets both.
func TestSkillInstallCreatesNoHarnessDirectoryForTheHook(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	must(t, os.Mkdir(filepath.Join(cwd, ".cursor"), 0o700))
	t.Chdir(cwd)
	out, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install")
	if code != exitOK || errOut != "" || strings.Contains(out, "Claude Code") || !strings.Contains(out, "./.cursor/hooks.json                      written") {
		t.Errorf("exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("~/.claude exists after a run that found no Claude Code (%v)", err)
	}
}

// A settings.json that is not JSON is refused by path and left byte for byte: malformed, empty,
// or with a "hooks" that is not an object. The skill row before it and Cursor's rows after it
// still print, and the run exits 1.
func TestSkillInstallRefusesAHookFileThatIsNotJSON(t *testing.T) {
	home, _ := harnessHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{"{not json\n", "", `{"hooks":3}`} {
		must(t, os.WriteFile(settings, []byte(body), 0o600), os.Chtimes(settings, old, old))
		out, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install")
		if code != exitError || !strings.Contains(errOut, "~/.claude/settings.json cannot be read as JSON") {
			t.Errorf("%q: exit %d, stderr %q", body, code, errOut)
		}
		if !strings.Contains(out, "~/.claude/skills/canyon/SKILL.md") || !strings.Contains(out, "./.cursor/hooks.json") || strings.Contains(out, "Your agent can now deploy") {
			t.Errorf("%q: a refused hook hid the rows around it:\n%s", body, out)
		}
		if info, err := os.Stat(settings); err != nil || fileBytes(t, settings) != body || !info.ModTime().Equal(old) {
			t.Errorf("%q: the file was touched: %q, mtime %v, err %v", body, fileBytes(t, settings), info.ModTime(), err)
		}
	}
}

// A hook of ours that differs from today's by one byte at the same length is still rewritten:
// unchanged is a comparison of the bytes, not of their count. The stale file is the installer's
// own output with one byte changed, so the two are the same length by construction.
func TestSkillInstallRewritesAStaleHookOfEqualLength(t *testing.T) {
	home, _ := harnessHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	env := map[string]string{"HOME": home}
	if _, _, code := run(t, env, false, "skill", "install"); code != exitOK {
		t.Fatalf("the first install exited %d", code)
	}
	canonical := fileBytes(t, settings)
	must(t, os.WriteFile(settings, []byte(strings.Replace(canonical, "clear|compact", "clear|compacx", 1)), 0o600))
	out, _, code := run(t, env, false, "skill", "install")
	if code != exitOK || !strings.Contains(out, "settings.json") || strings.Contains(out, "settings.json                   unchanged") {
		t.Errorf("a stale hook of equal length was reported unchanged:\n%s", out)
	}
	if got := fileBytes(t, settings); got != canonical {
		t.Errorf("the stale matcher was not corrected:\n%s", got)
	}
}

// The hook runs this binary by its path in exec form, as Claude Code spawns it — no shell, and no
// canyon on the PATH: the hook the onboarding test saw fail with exit 127 from ~/Downloads. A path
// with a space and a quote in it needs no quoting, and a second install finds the entry and leaves it.
func TestTheClaudeHookRunsThisBinaryByPathWithoutPATH(t *testing.T) {
	home, _ := harnessHome(t)
	bin := filepath.Join(t.TempDir(), "it's my tools")
	must(t, os.Mkdir(bin, 0o700), os.WriteFile(filepath.Join(bin, "canyon"), []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o700)) //nolint:gosec // a stand-in binary the hook runs
	was := Self
	Self = filepath.Join(bin, "canyon")
	t.Cleanup(func() { Self = was })
	settings := filepath.Join(home, ".claude", "settings.json")
	for n := 1; n <= 2; n++ {
		if _, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetClaude); code != exitOK || errOut != "" {
			t.Fatalf("run %d: exit %d, stderr %q", n, code, errOut)
		}
	}
	start := readHooks(t, settings).Hooks["SessionStart"]
	var entry struct {
		Hooks []struct {
			Command string
			Args    []string
		}
	}
	if len(start) != 1 || json.Unmarshal(start[0], &entry) != nil || len(entry.Hooks) != 1 {
		t.Fatalf("two installs left\n%s", fileBytes(t, settings))
	}
	hook := exec.Command(entry.Hooks[0].Command, entry.Hooks[0].Args...) //nolint:gosec // the hook install just wrote, which is what is under test
	hook.Env = []string{"PATH=/nonexistent"}
	if out, err := hook.CombinedOutput(); err != nil || string(out) != "ran manifest --harness claude-code\n" {
		t.Errorf("%q %q: %v, printed %q", entry.Hooks[0].Command, entry.Hooks[0].Args, err, out)
	}
}

// Codex runs a hook as one shell command and ignores args (checked on Codex 0.164.0), so its entry
// names this canyon's path shell-quoted. A path with a space and a quote in it runs with no canyon on
// the PATH, a re-install finds the entry and leaves it, a move replaces it where it stands, and
// another tool's SessionStart hook and every other key in the file survive.
func TestTheCodexHookIsOneShellCommandThatSurvivesASpace(t *testing.T) {
	home, _ := harnessHome(t)
	hooks := filepath.Join(home, ".codex", "hooks.json")
	theirs := `{"matcher":"startup","hooks":[{"type":"command","command":"echo theirs & more"}]}`
	must(t, os.WriteFile(hooks, []byte(`{"hooks":{"SessionStart":[`+theirs+`],"Stop":[{"hooks":[{"type":"command","command":"./notify.sh"}]}]},"x":1}`), 0o600))
	was := Self
	t.Cleanup(func() { Self = was })
	for _, dir := range []string{"it's my tools", "moved here"} {
		bin := filepath.Join(t.TempDir(), dir)
		must(t, os.Mkdir(bin, 0o700), os.WriteFile(filepath.Join(bin, "canyon"), []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o700)) //nolint:gosec // a stand-in binary the hook runs
		Self = filepath.Join(bin, "canyon")
		for n := 1; n <= 2; n++ {
			out, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetCodex)
			if code != exitOK || errOut != "" {
				t.Fatalf("%s, run %d: exit %d, stderr %q", dir, n, code, errOut)
			}
			if want := "~/.codex/hooks.json                       unchanged"; n == 2 && !strings.Contains(out, want) {
				t.Errorf("%s, the second run rendered\n%s\nwant a row %q", dir, out, want)
			}
		}
		f := readHooks(t, hooks)
		start := f.Hooks["SessionStart"]
		if len(start) != 2 || compact(t, start[0]) != theirs || len(f.Hooks["Stop"]) != 1 || !strings.Contains(fileBytes(t, hooks), `"x": 1`) {
			t.Fatalf("installed from %s, ~/.codex/hooks.json holds\n%s", dir, fileBytes(t, hooks))
		}
		var entry struct {
			Matcher string
			Hooks   []struct {
				Type, Command string
				Args          []string
			}
		}
		if json.Unmarshal(start[1], &entry) != nil || entry.Matcher != "startup|resume|clear|compact" || len(entry.Hooks) != 1 || entry.Hooks[0].Type != "command" || entry.Hooks[0].Args != nil {
			t.Fatalf("Canyon's entry: %s", start[1])
		}
		hook := exec.Command("/bin/sh", "-c", entry.Hooks[0].Command) //nolint:gosec // the hook install just wrote, which is what is under test
		hook.Env = []string{"PATH=/nonexistent"}
		if out, err := hook.CombinedOutput(); err != nil || string(out) != "ran manifest --harness codex\n" {
			t.Errorf("%q: %v, printed %q", entry.Hooks[0].Command, err, out)
		}
	}
}

// Codex's entry in a fresh file is the group the transcript prints, and the skill install wrote beside it
// is Claude Code's text: the same front matter and block.
func TestTheCodexHookAndSkillAreClaudeCodesShape(t *testing.T) {
	home, _ := harnessHome(t)
	if _, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetCodex); code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if start := readHooks(t, filepath.Join(home, ".codex", "hooks.json")).Hooks["SessionStart"]; len(start) != 1 || compact(t, start[0]) != codexHookEntry {
		t.Errorf("~/.codex/hooks.json holds\n%s", fileBytes(t, filepath.Join(home, ".codex", "hooks.json")))
	}
	// Install never trusts its own hook: Codex's trust lives in ~/.codex/config.toml, and only the person,
	// answering Codex's question at its next start or in /hooks, writes it there (cli.md §7.1). What install
	// puts in config.toml is its MCP server's table, and nothing else.
	if got := fileBytes(t, filepath.Join(home, ".codex", "config.toml")); got != "[mcp_servers.canyon]\ncommand = '/usr/local/bin/canyon'\nargs = ['mcp']\n" {
		t.Errorf("~/.codex/config.toml after install holds\n%s", got)
	}
	text, err := skillText(targetCodex)
	if err != nil || text != claudeFront+skillBlock(skillBody) || fileBytes(t, filepath.Join(home, ".agents", "skills", "canyon", "SKILL.md")) != text {
		t.Errorf("Codex's skill: %v\n%s", err, text)
	}
	if out, _, code := run(t, nil, false, "skill", "--print", "--harness", targetCodex); code != exitOK || out != text {
		t.Errorf("skill --print --harness codex: exit %d\n%s", code, out)
	}
}

// On Windows Codex gets its skill and no hook: a POSIX-quoted command is no command a Windows shell
// reads, so the row says the agent runs canyon manifest itself, as the skill asks (cli.md §7.1).
func TestOnWindowsCodexGetsItsSkillAndNoHook(t *testing.T) {
	home, _ := harnessHome(t)
	was := goos
	goos = "windows"
	t.Cleanup(func() { goos = was })
	out, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetCodex)
	if code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := "  ✓ Codex          ~/.agents/skills/canyon/SKILL.md          written\n" +
		"  · Codex's session hook is not set up on Windows: the agent runs canyon manifest itself, as the skill asks.\n"
	if !strings.Contains(out, want) || strings.Contains(out, "hooks.json") {
		t.Errorf("rendered\n%s\nwant\n%s", out, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("~/.codex/hooks.json was written on Windows (%v)", err)
	}
	if fileBytes(t, filepath.Join(home, ".agents", "skills", "canyon", "SKILL.md")) != claudeFront+skillBlock(skillBody) {
		t.Error("Codex's skill was not written on Windows")
	}
}

// A binary not named canyon, run from one place and then another, keeps one entry: install finds its
// own by the arguments, so a rename or a move replaces the entry rather than adding a second, and
// the path that is gone does not stay behind failing every session start.
func TestTheClaudeHookIsOneEntryAcrossARenameAndAMove(t *testing.T) {
	home, _ := harnessHome(t)
	was := Self
	t.Cleanup(func() { Self = was })
	settings := filepath.Join(home, ".claude", "settings.json")
	for _, self := range []string{"/opt/x/canyon-dev", "/opt/x/canyon-dev", "/Users/me/Downloads/canyon_v0.1.4_darwin_arm64/canyon", "/usr/local/bin/Canyon"} {
		Self = self
		if _, errOut, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetClaude); code != exitOK || errOut != "" {
			t.Fatalf("%s: exit %d, stderr %q", self, code, errOut)
		}
		start := readHooks(t, settings).Hooks["SessionStart"]
		if len(start) != 1 || !strings.Contains(compact(t, start[0]), `"command":"`+self+`"`) {
			t.Fatalf("installed from %s, the file holds\n%s", self, fileBytes(t, settings))
		}
	}
}

// Locate is where the shell ran the binary from: a bare name through PATH, a relative path made
// absolute, and a symlink kept as the name it was run by, not the file it points at.
func TestLocateIsThePathTheShellRanItFrom(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "canyon-v0.1.4"), filepath.Join(dir, "canyon")
	must(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o700), os.Symlink(real, link)) //nolint:gosec // an executable stand-in
	t.Setenv("PATH", dir)
	t.Chdir(dir)
	for argv0, want := range map[string]string{"canyon": link, "./canyon": link, link: link} {
		if got, err := Locate(argv0); err != nil || got != want {
			t.Errorf("Locate(%q) = %q, %v; want %q", argv0, got, err, want)
		}
	}
	// A relative PATH entry is where the shell found it too (Windows searches . first); Go says ErrDot.
	for _, path := range []string{".", ":", "./"} { // ":" is two empty entries, each the working directory
		t.Setenv("PATH", path)
		if got, err := Locate("canyon"); err != nil || got != link {
			t.Errorf("PATH=%q: Locate(canyon) = %q, %v; want %q", path, got, err, link)
		}
	}
	t.Setenv("PATH", dir)
	if got, err := Locate("not-on-path"); err == nil {
		t.Errorf("Locate found a name nothing on PATH holds: %q", got)
	}
}
