package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// personsSettings is a person's ~/.claude/settings.json in the layout install writes back, keys sorted
// and two-space indented, before Canyon's hook is in it.
const personsSettings = "{\n  \"model\": \"opus\",\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(ls:*)\"\n    ]\n  }\n}\n"

// Install everywhere, then uninstall everywhere, then a plain uninstall, under a home of the test's
// own: the transcript byte for byte each time. Canyon's whole files go with the skill's own directory;
// every file a person had before install is back as it was, byte for byte; a hook file install made
// stays, holding the document it started from. The second run finds nothing, touches nothing, and
// exits 0.
func TestSkillUninstallRemovesWhatInstallWroteAndNothingElse(t *testing.T) {
	first, again := golden(t, "skill-uninstall.txt"), golden(t, "skill-uninstall-again.txt") // before the run moves directory
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	settings, agents := filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, "AGENTS.md")
	theirs := "# Our repo\n\nRun the tests before you push.\n"
	must(t, os.WriteFile(settings, []byte(personsSettings), 0o600), os.WriteFile(agents, []byte(theirs), 0o644)) //nolint:gosec // a person's committed file
	if _, errOut, code := run(t, env, false, "skill", "install", "--harness", "all"); code != exitOK || errOut != "" {
		t.Fatalf("install: exit %d, stderr %q", code, errOut)
	}
	out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", "all")
	if code != exitOK || errOut != "" || out != first {
		t.Fatalf("exit %d, stderr %q, rendered\n%s\ntestdata/skill-uninstall.txt wants\n%s", code, errOut, out, first)
	}
	for path, want := range map[string]string{
		settings: personsSettings, agents: theirs, // a person's, as they were before install
		filepath.Join(home, ".codex", "hooks.json"): "{}\n", // install made these, from the document a missing file starts from, and they stay
		filepath.Join(cwd, ".cursor", "hooks.json"): "{\n  \"version\": 1\n}\n",
	} {
		if got := fileBytes(t, path); got != want {
			t.Errorf("%s after uninstall:\n%s\nwant\n%s", path, got, want)
		}
	}
	for _, gone := range []string{
		filepath.Join(home, ".claude", "skills", "canyon"), // the directory install made for the skill goes with it
		filepath.Join(home, ".agents", "skills", "canyon"),
		filepath.Join(cwd, ".cursor", "rules", "canyon.mdc"),
	} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	for _, own := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), filepath.Join(cwd, ".cursor")} {
		if !isDir(own) {
			t.Errorf("%s was removed: Canyon removes no harness's own directory, as it creates none", own)
		}
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(settings, old, old), os.Chtimes(agents, old, old))
	out, errOut, code = run(t, env, false, "skill", "uninstall")
	if code != exitOK || errOut != "" || out != again {
		t.Fatalf("the second run: exit %d, stderr %q, rendered\n%s\ntestdata/skill-uninstall-again.txt wants\n%s", code, errOut, out, again)
	}
	for _, path := range []string{settings, agents} {
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
			t.Errorf("%s was rewritten by a run with nothing to remove (%v)", path, err)
		}
	}
	if o, e, code := run(t, env, false, "skill", "uninstall", "--format", "json"); code != exitError || o != "" || !strings.Contains(e, "skill uninstall has no --format json") {
		t.Errorf("--format json: exit %d, stdout %q, stderr %q", code, o, e)
	}
}

// A plain uninstall takes what a plain install writes: the three agents' files, never ./AGENTS.md,
// which is the project's and is reached only by naming it or all. Each run's last line is the
// install that puts back what it removed.
func TestSkillUninstallScopeIsInstallsAndItsLastLinePutsItBack(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	agents := filepath.Join(cwd, "AGENTS.md")
	if _, _, code := run(t, env, false, "skill", "install", "--harness", "all"); code != exitOK {
		t.Fatalf("install: exit %d", code)
	}
	out, errOut, code := run(t, env, false, "skill", "uninstall")
	if code != exitOK || errOut != "" || strings.Contains(out, "AGENTS.md") || !strings.HasSuffix(out, "\n  To reinstall: canyon skill install\n") {
		t.Errorf("a plain run: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
	if got := fileBytes(t, agents); got != skillBlock(skillBody) {
		t.Errorf("a plain run touched ./AGENTS.md:\n%s", got)
	}
	out, _, code = run(t, env, false, "skill", "uninstall", "--harness", targetAgents)
	if code != exitOK || !strings.Contains(out, "./AGENTS.md                               removed") || !strings.HasSuffix(out, "\n  To put it back: canyon skill install --harness agents-md\n") {
		t.Errorf("--harness agents-md: exit %d, rendered\n%s", code, out)
	}
	if _, err := os.Stat(agents); !os.IsNotExist(err) {
		t.Errorf("an AGENTS.md that held Canyon's block alone is still there (%v)", err)
	}
}

// Canyon's entry is cut out of the bytes where it stands, with the comma that joined it, and nothing
// else moves: not a key's order, not a space, not a `<` or an `&`. An entry under hooks.<event> that
// is alone takes its event with it, an event alone in hooks takes hooks with it, and an older entry of
// ours goes too. A group a person built around Canyon's command is theirs and stays. The key is read
// by a JSON decoder, so "hooks" spelled with an escape is still hooks.
func TestSkillUninstallCutsCanyonsEntryAndKeepsEveryOtherByte(t *testing.T) {
	canyon, older := claudeHookEntry, `{"matcher":"resume","hooks":[{"type":"command","command":"/opt/canyon/bin/canyon manifest --harness claude-code"}]}`
	theirs := `{"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi & bye <x>"}]}`
	mine := `{"matcher":"clear","hooks":[{"type":"command","command":"canyon manifest --harness claude-code"},{"type":"command","command":"echo mine"}]}`
	for name, c := range map[string]struct {
		in, want string
		removed  bool
	}{
		"between two of theirs, in another layout": {
			"{\n  \"permissions\": {\"allow\": [\"Bash(ls:*)\"]},\n  \"hooks\": {\n    \"SessionStart\": [\n      " + theirs + ",\n      " + canyon + ",\n      " + mine + "\n    ],\n    \"PreToolUse\": []\n  },\n  \"model\": \"opus\"\n}\n",
			"{\n  \"permissions\": {\"allow\": [\"Bash(ls:*)\"]},\n  \"hooks\": {\n    \"SessionStart\": [\n      " + theirs + ",\n      " + mine + "\n    ],\n    \"PreToolUse\": []\n  },\n  \"model\": \"opus\"\n}\n",
			true,
		},
		"first":                                  {`{"hooks":{"SessionStart":[` + canyon + `, ` + theirs + `]}}`, `{"hooks":{"SessionStart":[` + theirs + `]}}`, true},
		"last, with an older one of ours before": {"{\"hooks\":{\"SessionStart\":[" + theirs + ",\n" + older + ",\r\n\t" + canyon + "]}}", "{\"hooks\":{\"SessionStart\":[" + theirs + "]}}", true},
		"alone under its event":                  {`{"hooks":{"PreToolUse":[` + theirs + `],"SessionStart":[ ` + canyon + ` ]},"z":1}`, `{"hooks":{"PreToolUse":[` + theirs + `]},"z":1}`, true},
		"alone in hooks":                         {"{\n  \"hooks\": {\n    \"SessionStart\": [\n      " + canyon + "\n    ]\n  },\n  \"model\": \"opus\"\n}\n", "{\n  \"model\": \"opus\"\n}\n", true},
		"alone in the file":                      {"{\"hooks\":{\"SessionStart\":[" + canyon + "]}}\n", "{}\n", true},
		"hooks spelled with an escape":           {`{"ho\` + `u006fks":{"SessionStart":[` + canyon + `]},"a":1}`, `{"a":1}`, true}, // its o escaped, which a JSON decoder reads as o
		"a group a person built around ours":     {`{"hooks":{"SessionStart":[` + mine + `]}}`, `{"hooks":{"SessionStart":[` + mine + `]}}`, false},
		"only another event's":                   {`{"hooks":{"PreToolUse":[` + canyon + `]}}`, `{"hooks":{"PreToolUse":[` + canyon + `]}}`, false},
		"no hooks":                               {`{"model":"opus"}`, `{"model":"opus"}`, false},
		"hooks null":                             {`{"hooks":null}`, `{"hooks":null}`, false},
	} {
		got, removed, err := withoutHook([]byte(c.in), "SessionStart")
		if err != nil || string(got) != c.want || removed != c.removed {
			t.Errorf("%s: removed %v, err %v, left\n%s\nwant (removed %v)\n%s", name, removed, err, got, c.removed, c.want)
		}
	}
	for _, bad := range []string{"{not json", "", "[]", `{"hooks":3}`, `{"hooks":{"SessionStart":3}}`} {
		if _, _, err := withoutHook([]byte(bad), "SessionStart"); err == nil {
			t.Errorf("%q was read as a hook file", bad)
		}
	}
}

// A person's links and hooks are theirs. Through a skill directory that is a link to their own,
// Canyon's SKILL.md goes and the link stays. A hook file that is a link into a dotfiles repository,
// which a rename would replace with a copy, is refused while it holds Canyon's entry, as the session
// hook's own move leaves it (migrateHook), and is not installed when it holds none; so is an
// ./AGENTS.md that is a link to CLAUDE.md. Canyon's command inside a hook group a person built is
// theirs to take out: the row says so, and the file stays as it is.
func TestSkillUninstallLeavesAPersonsLinksAndHooks(t *testing.T) {
	home, _ := harnessHome(t)
	env := map[string]string{"HOME": home}
	dotfiles := t.TempDir()
	skills, settings := filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".claude", "settings.json")
	must(t, os.MkdirAll(skills, 0o750), os.Symlink(dotfiles, filepath.Join(skills, "canyon")))
	if _, errOut, code := run(t, env, false, "skill", "install", "--harness", targetClaude); code != exitOK {
		t.Fatalf("install: exit %d, stderr %q", code, errOut)
	}
	linked := filepath.Join(dotfiles, "settings.json")
	body := fileBytes(t, settings)
	must(t, os.Remove(settings), os.WriteFile(linked, []byte(body), 0o600), os.Symlink(linked, settings))
	out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetClaude)
	if code != exitError || !strings.Contains(out, "~/.claude/skills/canyon/SKILL.md          removed") ||
		!strings.Contains(errOut, "~/.claude/settings.json is a link to another file, which uninstall does not rewrite: remove the entry that runs canyon manifest from hooks.SessionStart there by hand") {
		t.Errorf("a linked hook file: exit %d, stdout\n%s\nstderr\n%s", code, out, errOut)
	}
	if info, err := os.Lstat(filepath.Join(skills, "canyon")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the person's linked skill directory was removed (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dotfiles, "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("Canyon's SKILL.md stayed behind the link (%v)", err)
	}
	if info, err := os.Lstat(settings); err != nil || info.Mode()&os.ModeSymlink == 0 || fileBytes(t, linked) != body {
		t.Errorf("the linked hook file was replaced or rewritten (%v)", err)
	}
	must(t, os.WriteFile(linked, []byte(personsSettings), 0o600))
	if out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetClaude); code != exitOK || errOut != "" || !strings.Contains(out, "~/.claude/settings.json                   not installed") {
		t.Errorf("a linked hook file with nothing of Canyon's: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
	theirs := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"canyon manifest --harness claude-code"},{"type":"command","command":"echo mine"}]}]}}`
	must(t, os.Remove(settings), os.WriteFile(settings, []byte(theirs), 0o600))
	out, errOut, code = run(t, env, false, "skill", "uninstall", "--harness", targetClaude)
	if code != exitOK || errOut != "" || !strings.Contains(out, "Claude Code    ~/.claude/settings.json                   in a hook you built: remove canyon manifest from it by hand") ||
		strings.Contains(out, "✓ Claude Code    ~/.claude/settings.json") || strings.Contains(out, "To put it back") {
		t.Errorf("a group a person built: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
	if got := fileBytes(t, settings); got != theirs {
		t.Errorf("a group a person built was rewritten:\n%s", got)
	}
	// ./AGENTS.md as a link to the project's CLAUDE.md: Canyon's block in it is refused, not rewritten
	// through a rename that would leave a copy where the link was, nor removed with the link when the
	// block is all CLAUDE.md holds; with no block in it, it is not installed.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	agents, claude := filepath.Join(cwd, "AGENTS.md"), filepath.Join(cwd, "CLAUDE.md")
	must(t, os.Symlink("CLAUDE.md", agents))
	for _, held := range []string{"# Our repo\n\n" + skillBlock(skillBody), skillBlock(skillBody)} {
		must(t, os.WriteFile(claude, []byte(held), 0o644)) //nolint:gosec // a person's committed file
		out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetAgents)
		if code != exitError || strings.Contains(out, "removed") ||
			!strings.Contains(errOut, "./AGENTS.md is a link to another file, which uninstall does not rewrite: delete Canyon's block, markers included, there by hand") {
			t.Errorf("a linked AGENTS.md holding %q: exit %d, stdout\n%s\nstderr\n%s", held, code, out, errOut)
		}
		if info, err := os.Lstat(agents); err != nil || info.Mode()&os.ModeSymlink == 0 || fileBytes(t, claude) != held {
			t.Errorf("a linked AGENTS.md holding %q was replaced, removed or rewritten (%v)", held, err)
		}
	}
	must(t, os.WriteFile(claude, []byte("# Our repo\n"), 0o644)) //nolint:gosec // as above
	if out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetAgents); code != exitOK || errOut != "" || !strings.Contains(out, "./AGENTS.md                               not installed") {
		t.Errorf("a linked AGENTS.md with no block: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
}

// What uninstall cannot read it refuses by path, as install does, and leaves the file as it was: a
// hook file that is not JSON, a block findBlock refuses, a block a later release wrote. The rows after
// a refusal still print, and the run exits 1. A SKILL.md with no block in it is not Canyon's to remove.
func TestSkillUninstallRefusesWhatItCannotReadAndLeavesIt(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	settings, agents := filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, "AGENTS.md")
	skill := filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md")
	later := "theirs\n<!-- canyon:skill v:2 hash:0123456789ab -->\nnewer\n" + blockEnd + "\n"
	must(t, os.WriteFile(settings, []byte("{not json\n"), 0o600), os.WriteFile(agents, []byte(later), 0o600),
		os.MkdirAll(filepath.Dir(skill), 0o750), os.WriteFile(skill, []byte("# My own notes\n"), 0o600))
	out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", "all")
	if code != exitError || !strings.Contains(errOut, "~/.claude/settings.json cannot be read as JSON") ||
		!strings.Contains(errOut, "./AGENTS.md:2 opens a Canyon block in format v:2") || !strings.Contains(out, "./.cursor/hooks.json") ||
		!strings.Contains(out, "~/.claude/skills/canyon/SKILL.md          not installed") || strings.Contains(out, "To put them back") {
		t.Errorf("exit %d, stdout\n%s\nstderr\n%s", code, out, errOut)
	}
	for path, want := range map[string]string{settings: "{not json\n", agents: later, skill: "# My own notes\n"} {
		if got := fileBytes(t, path); got != want {
			t.Errorf("%s was touched:\n%s", path, got)
		}
	}
	must(t, os.WriteFile(agents, []byte(oldBlock()+oldBlock()), 0o600))
	if _, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetAgents); code != exitError || !strings.Contains(errOut, "./AGENTS.md:1 and ./AGENTS.md:") {
		t.Errorf("two openers: exit %d, stderr %q", code, errOut)
	}
}

// --harness names one agent, and only its files are read; a name uninstall does not know is refused
// with the names it does. Without HOME, ~ is nowhere: refused, never guessed under the working directory.
func TestSkillUninstallTakesOneHarnessAndNeedsHome(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	if _, _, code := run(t, env, false, "skill", "install"); code != exitOK {
		t.Fatalf("install: exit %d", code)
	}
	out, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", targetCodex)
	if code != exitOK || errOut != "" || strings.Contains(out, "Claude Code") || strings.Contains(out, "Cursor") ||
		!strings.Contains(out, "~/.codex/hooks.json                       hook removed") || !strings.HasSuffix(out, "\n  To put it back: canyon skill install --harness codex\n") {
		t.Errorf("--harness codex: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
	for _, kept := range []string{filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md"), filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, ".cursor", "hooks.json")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("--harness codex removed %s (%v)", kept, err)
		}
	}
	if _, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", "emacs"); code != exitError || !strings.Contains(errOut, `unknown --harness value "emacs": use claude-code, codex, cursor, agents-md or all`) {
		t.Errorf("an unknown harness: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := run(t, env, false, "skill", "uninstall", "extra"); code != exitError || !strings.Contains(errOut, `skill uninstall takes no arguments; got "extra"`) {
		t.Errorf("an argument: exit %d, stderr %q", code, errOut)
	}
	must(t, os.MkdirAll(filepath.Join(cwd, ".claude", "skills", "canyon"), 0o700), os.WriteFile(filepath.Join(cwd, ".claude", "skills", "canyon", "SKILL.md"), []byte(skillBlock(skillBody)), 0o600))
	if _, errOut, code := run(t, map[string]string{"HOME": ""}, false, "skill", "uninstall"); code != exitError || !strings.Contains(errOut, "HOME is not set, so ~/.claude cannot be found") {
		t.Errorf("no HOME: exit %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "canyon", "SKILL.md")); err != nil {
		t.Errorf("a .claude under the working directory was read as ~/.claude (%v)", err)
	}
}

// Under Codex (CODEX_SANDBOX, §5.1), a write the filesystem refuses for permission is followed by one
// line that names Codex's sandbox and the two ways past it, repeating the command as it was typed,
// flags included. Outside Codex the same refusals stand alone. A directory mode stands in for the
// sandbox: both are EPERM or EACCES, which Go reads as fs.ErrPermission.
func TestCodexSandboxIsNamedWhenItRefusesAWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a mode")
	}
	home, _ := harnessHome(t)
	codex, agents := filepath.Join(home, ".codex"), filepath.Join(home, ".agents")
	must(t, os.Mkdir(agents, 0o700), os.Chmod(agents, 0o500), os.Chmod(codex, 0o500)) //nolint:gosec // directory modes this user cannot write in
	t.Cleanup(func() { _, _ = os.Chmod(agents, 0o700), os.Chmod(codex, 0o700) })      //nolint:gosec // and back, so the temp directory can be removed
	hint := func(typed string) string {
		return "This ran inside Codex's sandbox, which refuses writes outside the project. Run " + typed + " again with permission to leave the sandbox (the user approves it in Codex), or ask the user to run it in a terminal outside Codex."
	}
	inCodex := map[string]string{"HOME": home, "CODEX_SANDBOX": "seatbelt"}
	_, errOut, code := run(t, inCodex, false, "skill", "install", "--harness", targetCodex)
	if code != exitError || !strings.Contains(errOut, "~/.agents/skills/canyon/SKILL.md cannot be written: permission denied") ||
		!strings.Contains(errOut, "~/.codex/hooks.json cannot be written: permission denied") || strings.Count(errOut, hint("canyon skill install --harness codex")) != 1 {
		t.Errorf("install in Codex: exit %d, stderr\n%s", code, errOut)
	}
	if _, errOut, _ := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetCodex); strings.Contains(errOut, "sandbox") {
		t.Errorf("install outside Codex named a sandbox:\n%s", errOut)
	}
	must(t, os.Chmod(codex, 0o700), os.WriteFile(filepath.Join(codex, "hooks.json"), []byte(`{"hooks":{"SessionStart":[`+codexHookEntry+`]}}`), 0o600), os.Chmod(codex, 0o500)) //nolint:gosec // as above
	_, errOut, code = run(t, inCodex, false, "--format", "ai", "skill", "uninstall", "--harness="+targetCodex)
	if code != exitError || !strings.Contains(errOut, "~/.codex/hooks.json cannot be written: permission denied") || strings.Count(errOut, hint("canyon --format ai skill uninstall --harness=codex")) != 1 {
		t.Errorf("uninstall in Codex: exit %d, stderr\n%s", code, errOut)
	}
	if got := fileBytes(t, filepath.Join(codex, "hooks.json")); !strings.Contains(got, "manifest --harness codex") {
		t.Errorf("a refused write changed the file:\n%s", got)
	}
}
