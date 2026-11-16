package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// harnessHome is a home with ~/.claude and ~/.codex in it and a working directory with ./.cursor: the
// three native harnesses present, no file written yet. Nothing under each but its own empty directory.
func harnessHome(t *testing.T) (home, cwd string) {
	t.Helper()
	home, cwd = t.TempDir(), t.TempDir()
	must(t, os.Mkdir(filepath.Join(home, ".claude"), 0o700), os.Mkdir(filepath.Join(home, ".codex"), 0o700), os.Mkdir(filepath.Join(cwd, ".cursor"), 0o700))
	t.Chdir(cwd)
	return home, cwd
}

func fileBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path this test wrote
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An old block: the same markers, a body no release ships, so a run has something to replace.
func oldBlock() string { return skillBlock("# Canyon\n\nan older text\n") }

// §7.1's transcript, byte for byte, then the same run again: nothing rewritten, `unchanged` in the
// rows. The verb itself reaches no host and reads no config, so a broken config warns nothing;
// the once-a-day notice (§8.5) is silent here, tty being false.
func TestSkillInstallWritesTheHarnessesItFindsAndTouchesNothingElse(t *testing.T) {
	var hits atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer host.Close()
	first, again := golden(t, "skill-install.txt"), golden(t, "skill-install-unchanged.txt") // before the run moves directory
	home, cwd := harnessHome(t)
	conf := filepath.Join(home, ".config", "canyon")
	must(t, os.MkdirAll(conf, 0o700), os.WriteFile(filepath.Join(conf, "config.json"), []byte("not the JSON Canyon writes"), 0o600))
	env := map[string]string{"HOME": home, "CANYON_HOST": host.URL}
	out, errOut, code := run(t, env, false, "skill", "install")
	if code != exitOK || errOut != "" || out != first {
		t.Fatalf("exit %d, stderr %q, rendered\n%s\ntestdata/skill-install.txt wants\n%s", code, errOut, out, first)
	}
	claude, cursor := filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md"), filepath.Join(cwd, ".cursor", "rules", "canyon.mdc")
	codex := filepath.Join(home, ".agents", "skills", "canyon", "SKILL.md") // ~/.agents/skills is made: Codex reads it, and makes no such directory
	for path, front := range map[string]string{claude: claudeFront, codex: claudeFront, cursor: cursorFront} {
		if got := fileBytes(t, path); got != front+skillBlock(skillBody) {
			t.Errorf("%s holds\n%s", path, got)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, err %v; a new file is 0600", path, info.Mode(), err)
		}
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 1 { // no AGENTS.md unasked, no temp file left
		t.Errorf("the working directory holds %v (%v); only .cursor was expected", entries, err)
	}
	// The second run: the bytes and the mtime both stay, and the rows say so.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(claude, old, old), os.Chtimes(codex, old, old), os.Chtimes(cursor, old, old))
	out, errOut, code = run(t, env, false, "skill", "install")
	if code != exitOK || errOut != "" || out != again {
		t.Fatalf("the second run: exit %d, stderr %q, rendered\n%s\ntestdata/skill-install-unchanged.txt wants\n%s", code, errOut, out, again)
	}
	for _, path := range []string{claude, codex, cursor} {
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
			t.Errorf("%s was rewritten: mtime %v, err %v", path, info.ModTime(), err)
		}
	}
	if ai, _, _ := run(t, env, false, "skill", "install", "--format", "ai"); ai != out {
		t.Errorf("--format ai rendered\n%s\nhuman rendered\n%s", ai, out)
	}
	if o, e, code := run(t, env, false, "skill", "install", "--format", "json"); code != exitError || o != "" || !strings.Contains(e, "skill install has no --format json") {
		t.Errorf("--format json: exit %d, stdout %q, stderr %q", code, o, e)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("install reached the host %d times", n)
	}
}

// Nothing found is exit 1 with the hint, and no directory of a harness's own is made — not by
// detection, and not by naming the harness either.
func TestSkillInstallFindsNothingAndCreatesNoDirectory(t *testing.T) {
	want := golden(t, "skill-install-none.txt")
	home, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	env := map[string]string{"HOME": home}
	out, errOut, code := run(t, env, false, "skill", "install")
	if code != exitError || errOut != want || !strings.Contains(out, "Looking for coding agents") || strings.Contains(out, "written") {
		t.Errorf("exit %d, stdout %q, stderr\n%s\ntestdata/skill-install-none.txt wants\n%s", code, out, errOut, want)
	}
	_, errOut, code = run(t, env, false, "skill", "install", "--harness", targetClaude)
	if code != exitError || !strings.Contains(errOut, "Claude Code is not set up here: ~/.claude does not exist. Start Claude Code once, then run this again.") {
		t.Errorf("--harness claude-code with no ~/.claude: exit %d, stderr %q", code, errOut)
	}
	_, errOut, code = run(t, env, false, "skill", "install", "--harness", targetCodex)
	if code != exitError || !strings.Contains(errOut, "Codex is not set up here: ~/.codex does not exist, and Canyon does not create it") {
		t.Errorf("--harness codex with no ~/.codex: exit %d, stderr %q", code, errOut)
	}
	_, errOut, code = run(t, env, false, "skill", "install", "--harness", "emacs")
	if code != exitError || !strings.Contains(errOut, `unknown --harness value "emacs": use claude-code, codex, cursor, agents-md or all`) {
		t.Errorf("an unknown harness: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := run(t, env, false, "skill", "install", "extra"); code != exitError || !strings.Contains(errOut, `skill install takes no arguments; got "extra"`) {
		t.Errorf("an argument: exit %d, stderr %q", code, errOut)
	}
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), filepath.Join(home, ".agents"), filepath.Join(cwd, ".cursor"), filepath.Join(cwd, "AGENTS.md")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s exists after a run that found no harness (%v)", dir, err)
		}
	}
}

// The dotted line names Codex among the agents to give AGENTS.md only where Codex is not set up: once
// ~/.codex is here it leaves Codex out, and points at --harness codex while this run wrote nothing of
// Codex's (#2659: right after a Codex install the line still named Codex).
func TestTheAgentsHintLeavesCodexOutOnceCodexIsHere(t *testing.T) {
	home, _ := harnessHome(t) // ~/.codex is here
	out, _, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetCursor)
	want := "  · Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md:\n" +
		"      canyon skill install --harness agents-md      appends to ./AGENTS.md\n" +
		"      canyon skill install --harness codex          Codex's own skill and hook, once Codex has made ~/.codex\n"
	if code != exitOK || !strings.Contains(out, want) {
		t.Errorf("Codex found and not written: exit %d, rendered\n%s\nwant\n%s", code, out, want)
	}
}

// --harness all is the three rows; AGENTS.md is a person's file, and only the bytes between the
// markers move: their text above and below round-trips, a line they add later survives the next run,
// and a file with no block gets one appended after its trailing newline.
func TestSkillInstallAllAndTheAgentsRoundTrip(t *testing.T) {
	want := golden(t, "skill-install-all.txt")
	home, cwd := harnessHome(t)
	above, below := "# Our repo\n\nRun the tests before you push.\n\n", "\n## Beads\n\nbd ready first.\n"
	agents := filepath.Join(cwd, "AGENTS.md")
	must(t, os.WriteFile(agents, []byte(above+oldBlock()+below), 0o644)) //nolint:gosec // a person's committed file
	env := map[string]string{"HOME": home}
	out, errOut, code := run(t, env, false, "skill", "install", "--harness", "all")
	if code != exitOK || errOut != "" || out != want {
		t.Fatalf("exit %d, stderr %q, rendered\n%s\ntestdata/skill-install-all.txt wants\n%s", code, errOut, out, want)
	}
	if got := fileBytes(t, agents); got != above+skillBlock(skillBody)+below {
		t.Fatalf("AGENTS.md after install:\n%s", got)
	}
	// A person edits outside the block, and the block is already today's: unchanged, and their line stays.
	edited := above + skillBlock(skillBody) + below + "Deploy on Fridays only.\n"
	must(t, os.WriteFile(agents, []byte(edited), 0o644)) //nolint:gosec // as above
	out, _, code = run(t, env, false, "skill", "install", "--harness", targetAgents)
	if code != exitOK || !strings.Contains(out, "./AGENTS.md                               unchanged") || strings.Contains(out, "other AGENTS.md harnesses") {
		t.Errorf("exit %d, rendered\n%s", code, out)
	}
	if got := fileBytes(t, agents); got != edited {
		t.Errorf("a line outside the block did not survive:\n%s", got)
	}
	// No block yet, and no newline at the end: one is put there, then the block.
	must(t, os.WriteFile(agents, []byte("# Our repo"), 0o644)) //nolint:gosec // as above
	if _, errOut, code := run(t, env, false, "skill", "install", "--harness", targetAgents); code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if got := fileBytes(t, agents); got != "# Our repo\n"+skillBlock(skillBody) {
		t.Errorf("appended to a file with no trailing newline:\n%s", got)
	}
}

// A pair findBlock refuses and a block a later release wrote are each a refusal by line, and the
// file is not touched; a write the filesystem refuses leaves the old file whole, because the bytes
// go to a temp file beside it and only a rename would have replaced it.
func TestSkillInstallRefusesAndLeavesTheOldFileWhole(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	agents := filepath.Join(cwd, "AGENTS.md")
	later := "<!-- canyon:skill v:2 hash:0123456789ab -->\nnewer\n" + blockEnd + "\n"
	for name, c := range map[string]struct{ file, at string }{
		"two openers":      {oldBlock() + oldBlock(), "./AGENTS.md:1 and ./AGENTS.md:"},
		"a later release":  {"theirs\n" + later, "./AGENTS.md:2 opens a Canyon block in format v:2"},
		"a closer nothing": {"theirs\n" + blockEnd + "\n", "./AGENTS.md:2 closes"},
	} {
		must(t, os.WriteFile(agents, []byte(c.file), 0o600))
		out, errOut, code := run(t, env, false, "skill", "install", "--harness", targetAgents)
		if code != exitError || strings.Contains(out, "written") || !strings.Contains(errOut, c.at) || !strings.Contains(errOut, "run this again") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
		if got := fileBytes(t, agents); got != c.file {
			t.Errorf("%s: the file was written:\n%s", name, got)
		}
	}
	// The write path: a directory this account cannot add a file to. The old file itself is writable,
	// so a write in place would have gone through and changed it.
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a mode")
	}
	must(t, os.WriteFile(agents, []byte(oldBlock()), 0o600), os.Chmod(cwd, 0o500)) //nolint:gosec // a directory mode: one this user cannot write in
	t.Cleanup(func() { _ = os.Chmod(cwd, 0o700) })                                 //nolint:gosec // and back, so the temp directory can be removed
	out, errOut, code := run(t, env, false, "skill", "install", "--harness", targetAgents)
	if code != exitError || strings.Contains(out, "written") || !strings.Contains(errOut, "./AGENTS.md cannot be written: permission denied") {
		t.Errorf("a refused write: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := fileBytes(t, agents); got != oldBlock() {
		t.Errorf("the old file was changed by a write that failed:\n%s", got)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 2 { // .cursor and AGENTS.md, no temp file
		t.Errorf("the directory holds %v (%v)", entries, err)
	}
}

// A rewritten file keeps the mode it had: a committed AGENTS.md at 0644 stays readable to the
// repo after Canyon's block moves in it. A new file is private. And with no HOME at all,
// ~/.claude is nowhere, not under the working directory: the verb refuses rather than guesses.
func TestSkillInstallKeepsAnExistingFilesModeAndRefusesWithoutHome(t *testing.T) {
	home, cwd := harnessHome(t)
	agents := filepath.Join(cwd, "AGENTS.md")
	must(t, os.WriteFile(agents, []byte("theirs\n"), 0o644)) //nolint:gosec // the repo's own mode is what is under test
	if _, _, code := run(t, map[string]string{"HOME": home}, false, "skill", "install", "--harness", targetAgents); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if info, err := os.Stat(agents); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("AGENTS.md after a rewrite: %v (%v), want 0644 kept", info.Mode(), err)
	}
	must(t, os.MkdirAll(filepath.Join(cwd, ".claude"), 0o700))
	_, errOut, code := run(t, map[string]string{"HOME": ""}, false, "skill", "install")
	if code != exitError || !strings.Contains(errOut, "HOME is not set, so ~/.claude cannot be found") {
		t.Errorf("no HOME: exit %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills")); !os.IsNotExist(err) {
		t.Errorf("a .claude under the working directory was written into (%v)", err)
	}
}

// The two native files are Canyon's whole: a front matter an older release wrote is replaced with
// this one's, since the description is what makes the skill fire. A hand-written SKILL.md with no
// block is replaced entirely, the one unrecoverable thing the verb does, on purpose. A refused
// target does not hide the rows after it. And `all` with neither native directory writes AGENTS.md alone.
func TestSkillInstallOwnsTheNativeFilesWholeAndReportsEveryRow(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	skill := filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md")
	must(t, os.MkdirAll(filepath.Dir(skill), 0o750), os.WriteFile(skill, []byte("---\nname: canyon\ndescription: an older release's words\n---\n\n"+skillBlock(skillBody)), 0o600))
	if _, _, code := run(t, env, false, "skill", "install", "--harness", targetClaude); code != exitOK || fileBytes(t, skill) != claudeFront+skillBlock(skillBody) {
		t.Errorf("an older front matter survived the install: exit %d\n%s", code, fileBytes(t, skill))
	}
	must(t, os.WriteFile(skill, []byte("# My own notes\n"), 0o600))
	if _, _, code := run(t, env, false, "skill", "install", "--harness", targetClaude); code != exitOK || fileBytes(t, skill) != claudeFront+skillBlock(skillBody) {
		t.Errorf("a hand-written SKILL.md is replaced whole: exit %d\n%s", code, fileBytes(t, skill))
	}
	must(t, os.WriteFile(skill, []byte("<!-- canyon:skill v:2 hash:0123456789ab -->\nnewer\n"+blockEnd+"\n"), 0o600))
	out, errOut, code := run(t, env, false, "skill", "install", "--harness", "all")
	if code != exitError || !strings.Contains(errOut, "format v:2") || !strings.Contains(out, "Cursor") || !strings.Contains(out, "AGENTS.md") || strings.Contains(out, "Your agent can now deploy") {
		t.Errorf("a refused first target hid the rows after it: exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	home2, cwd2 := t.TempDir(), t.TempDir()
	t.Chdir(cwd2)
	if out, _, code := run(t, map[string]string{"HOME": home2}, false, "skill", "install", "--harness", "all"); code != exitOK || !strings.Contains(out, "AGENTS.md") || strings.Contains(out, "Claude Code") || strings.Contains(out, "Cursor") {
		t.Errorf("all with neither native directory: exit %d\n%s", code, out)
	}
	for _, dir := range []string{filepath.Join(home2, ".claude"), filepath.Join(cwd2, ".cursor")} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("--harness all created %s, which Canyon does not create", dir)
		}
	}
	_ = cwd
}

// A rename that fails after the temp was written leaves the temp behind unless someone removes
// it: the write path is proven to clean up, with the target a directory so the rename cannot land.
func TestAtomicWriteLeavesNoTempWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "target"), 0o750))
	if err := atomicWrite(filepath.Join(dir, "target"), []byte("x"), false); err == nil {
		t.Fatal("a rename onto a directory succeeded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v after a failed rename (%v); want the target alone", entries, err)
	}
}

// Asking how skill works lists every form, install and uninstall among them (the onboarding test found
// install unlisted), and each one's own --help names the form typed.
func TestSkillHelpListsInstall(t *testing.T) {
	column := func(out, description string) int {
		at := strings.Index(out, description)
		if at < 0 {
			return -1
		}
		return at - strings.LastIndex(out[:at], "\n")
	}
	for _, ask := range []string{"--help", "-h", "help"} {
		out, errOut, code := run(t, nil, false, "skill", ask)
		if code != exitOK || errOut != "" || !strings.Contains(out, "canyon skill install [--harness") || !strings.Contains(out, "canyon skill uninstall [--harness") || !strings.Contains(out, "canyon skill --print") {
			t.Errorf("skill %s: exit %d, stdout %q, stderr %q", ask, code, out, errOut)
		}
		// the forms' descriptions start in one column (#1990)
		if a, b, c := column(out, "Write the skill"), column(out, "Remove the skill"), column(out, "The skill text"); a < 0 || a != b || a != c {
			t.Errorf("skill %s: the descriptions start at columns %d, %d and %d:\n%s", ask, a, b, c, out)
		}
	}
	for _, form := range []string{"install", "uninstall"} {
		if out, _, code := run(t, nil, false, "skill", form, "--help"); code != exitOK || !strings.Contains(out, "\nUSAGE\n  canyon skill "+form+" [flags]\n") {
			t.Errorf("skill %s --help: exit %d, stdout %q", form, code, out)
		}
	}
}

// A file install edits rather than owns may be a person's link: ./AGENTS.md to the project's
// CLAUDE.md, ~/.claude/settings.json into a dotfiles repository. A write by rename would put a copy
// where the link was, so install refuses each one that needs a write, by path, and says what to put
// there by hand: for a hook file, the entry it prints above. The link and its file stay as they were,
// and the rows around a refusal still print. A link whose file already holds today's block needs no
// write and is unchanged.
func TestSkillInstallRefusesToWriteThroughAPersonsLink(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	agents, claude := filepath.Join(cwd, "AGENTS.md"), filepath.Join(cwd, "CLAUDE.md")
	settings, linked := filepath.Join(home, ".claude", "settings.json"), filepath.Join(t.TempDir(), "settings.json")
	must(t, os.WriteFile(claude, []byte("# Our repo\n"), 0o644), os.Symlink("CLAUDE.md", agents), //nolint:gosec // a person's committed file
		os.WriteFile(linked, []byte(personsSettings), 0o600), os.Symlink(linked, settings))
	out, errOut, code := run(t, env, false, "skill", "install", "--harness", "all")
	if code != exitError ||
		!strings.Contains(errOut, "./AGENTS.md is a link to another file, which install does not rewrite: put Canyon's block, from canyon skill --print --harness agents-md, into that file by hand") ||
		!strings.Contains(errOut, "~/.claude/settings.json is a link to another file, which install does not rewrite: add the entry above under hooks.SessionStart there by hand") ||
		!strings.Contains(out, "Canyon's session hook, in ~/.claude/settings.json under hooks.SessionStart:") ||
		!strings.Contains(out, "~/.claude/skills/canyon/SKILL.md          written") || !strings.Contains(out, "./.cursor/hooks.json                      written") {
		t.Errorf("exit %d, stdout\n%s\nstderr\n%s", code, out, errOut)
	}
	for path, want := range map[string]string{claude: "# Our repo\n", linked: personsSettings} {
		if got := fileBytes(t, path); got != want {
			t.Errorf("%s was written through its link:\n%s", path, got)
		}
	}
	for _, link := range []string{agents, settings} {
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer a link (%v)", link, err)
		}
	}
	must(t, os.WriteFile(claude, []byte("# Our repo\n"+skillBlock(skillBody)), 0o644)) //nolint:gosec // as above
	if out, errOut, code := run(t, env, false, "skill", "install", "--harness", targetAgents); code != exitOK || errOut != "" || !strings.Contains(out, "./AGENTS.md                               unchanged") {
		t.Errorf("a linked AGENTS.md already holding today's block: exit %d, stderr %q, rendered\n%s", code, errOut, out)
	}
}
