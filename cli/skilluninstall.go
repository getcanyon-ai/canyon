package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	notInstalled = "not installed"                                               // the file holds nothing of Canyon's, or is not there
	builtByHand  = "in a hook you built: remove canyon manifest from it by hand" // Canyon's command, inside a hook group a person built around it
)

// skillUninstall is `canyon skill uninstall`: what skill install wrote, taken back out, one row per
// file install writes (cli.md §7.1). Its reach is install's: without --harness, the three agents'
// files, and ./AGENTS.md, the project's, only with all or by name; with a name, that agent's files
// alone, whether or not its directory is here. It creates nothing and removes no harness's own
// directory. A second run finds nothing, changes nothing, and exits 0. Its last line is the install
// that puts back what it removed.
func (a *app) skillUninstall(args []string) int {
	var pick string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&pick, "harness", "", targetClaude+" | "+targetCodex+" | "+targetCursor+" | "+targetAgents+" | all    (default: "+targetClaude+", "+targetCodex+" and "+targetCursor+")")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("skill uninstall takes no arguments; got %q", rest[0]))
	}
	if a.render.Format == FormatJSON {
		return a.usage(errors.New("skill uninstall has no --format json"))
	}
	if a.getenv("HOME") == "" { // ~/.claude would otherwise resolve under the working directory
		return a.usage(errors.New("HOME is not set, so ~/.claude cannot be found"))
	}
	var targets []skillTarget
	for _, t := range a.skillTargets() {
		if (pick == "" && t.harness != targetAgents) || pick == "all" || pick == t.harness {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return a.usage(unknownHarness(pick))
	}
	a.printf("\n  Removing Canyon from the coding agents in this environment%s\n\n", a.render.punct("…", "..."))
	failed, denied, removed := false, false, false
	row := func(name, shown, status string, err error) {
		failed = a.installRow(name, shown, status, err) || failed
		denied = denied || errors.Is(err, os.ErrPermission)
		removed = removed || (err == nil && strings.HasSuffix(status, "removed"))
	}
	for _, t := range targets { // a refused file does not hide the rows after it, as in install
		status, err := uninstallSkill(t)
		row(t.name, t.shown, status, err)
		if t.hook != nil {
			status, err := uninstallHook(t.hook)
			row(t.name, t.hook.shown, status, err)
		}
	}
	a.sandboxed(denied)
	switch {
	case failed:
		return exitError
	case !removed:
	case pick == "":
		a.printf("\n  To reinstall: canyon skill install\n")
	case pick == "all":
		a.printf("\n  To put them back: canyon skill install --harness all\n")
	default:
		a.printf("\n  To put it back: canyon skill install --harness %s\n", pick)
	}
	return exitOK
}

// uninstallSkill takes Canyon's block out of t's file and answers what it did. A file Canyon owns
// whole goes, front matter and all, with the canyon directory install made for it when that is a real
// directory with nothing else in it, never a person's link to their own; in AGENTS.md only the block's
// bytes go, and the file with them when nothing else was in it. A file with no block is not Canyon's
// and stays. What findBlock refuses, a block a later release wrote, and an AGENTS.md that is a link to
// another file (CLAUDE.md, commonly) holding the block are refused as install refuses them, with
// nothing removed.
func uninstallSkill(t skillTarget) (string, error) {
	old, err := os.ReadFile(t.path) //nolint:gosec // one of the paths skillTargets builds, none from input
	if os.IsNotExist(err) {
		return notInstalled, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %s", t.shown, plainly(err))
	}
	file := string(old)
	ref, found, err := findBlock(t.shown, file)
	switch {
	case err != nil:
		return "", err
	case !found:
		return notInstalled, nil
	case ref.Version != blockVersion:
		return "", newerBlock(t.shown, file, ref)
	case !t.whole && linked(t.path): // a file Canyon edits and does not own: a rename would leave a copy where the link was, and a removal would take the link
		return "", fmt.Errorf("%s is a link to another file, which uninstall does not rewrite: delete Canyon's block, markers included, there by hand", t.shown)
	}
	if rest := file[:ref.Start] + file[ref.End:]; !t.whole && rest != "" {
		if err := atomicWrite(t.path, []byte(rest), false); err != nil {
			return "", refused(t.shown, "written", err)
		}
		return "block removed", nil
	}
	if err := os.Remove(t.path); err != nil {
		return "", refused(t.shown, "removed", err)
	}
	if dir := filepath.Dir(t.path); filepath.Base(dir) == "canyon" {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			_ = os.Remove(dir) // fails, and stays, when anything else is in it
		}
	}
	return "removed", nil
}
