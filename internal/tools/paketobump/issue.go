package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// result is a bump: the pins it moves and the scan diff's lines by class.
type result struct {
	Moves []move              `json:"moves"`
	Lines map[string][]string `json:"lines"`
}

// key names the pins the bump moves to: the open issue for the same key is the same bump.
func (r result) key() string {
	var pins strings.Builder
	for _, m := range r.Moves {
		fmt.Fprintf(&pins, "%s@%s\n", m.Repo, m.New)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(pins.String())))[:12]
}

// branch is where the bump's pins are pushed: a function of the pins alone, never read from an issue,
// which anyone who may edit it can change.
func (r result) branch() string { return "ci/paketo-bump-" + r.key() }

// markerRE is the line the bump's issue opens with, naming its key and nothing else.
var markerRE = regexp.MustCompile(`(?m)^<!-- paketo-bump key=([0-9a-f]{12}) -->$`)

// bot is the author of the issues the workflow files, as gh names github.token's app.
const bot = "app/github-actions"

// issue is the bump's issue: a person opens the pull request from the compare link to its branch, so the
// pull request's own checks run (Actions cannot open pull requests in this repository; design/PATCHING.md §5).
func (r result) issue(repo string) (title, body string) {
	branch := r.branch()
	n := 0
	for _, lines := range r.Lines {
		n += len(lines)
	}
	var s strings.Builder
	fmt.Fprintf(&s, "<!-- paketo-bump key=%s -->\n", r.key())
	fmt.Fprintf(&s, "Paketo's newest images move Canyon's pins. The branch `%s` moves them: open its pull request, which runs the checks.\n\nhttps://github.com/%s/compare/main...%s?expand=1\n\n", branch, repo, branch)
	fmt.Fprintf(&s, "The fixture apps (%s) were built and their images scanned on the old pins and on the new, as Canyon scans an image after a deploy (design/PATCHING.md §3.1, §5).\n\n", strings.Join(apps, ", "))
	s.WriteString("## The scan diff\n\nEvery package of the system libraries and build tools Canyon adds whose version moves; an app's own dependencies are the team's and left out.\n")
	for _, class := range classes {
		own, modules := r.Lines[class], r.Lines[class+goModules]
		if len(own)+len(modules) == 0 {
			continue
		}
		fmt.Fprintf(&s, "\n**%s**\n\n", class)
		if len(own) > 0 {
			fmt.Fprintf(&s, "%s\n", strings.Join(own, "\n"))
		}
		if len(modules) > 0 {
			fmt.Fprintf(&s, "\n<details><summary>Go modules of its binaries (%d)</summary>\n\n%s\n\n</details>\n", len(modules), strings.Join(modules, "\n"))
		}
	}
	if n == 0 {
		s.WriteString("\nNo package's version moves.\n")
	}
	s.WriteString("\n## The pins\n\n")
	for _, m := range r.Moves {
		if m.OldVersion != "" {
			fmt.Fprintf(&s, "- `paketobuildpacks/%s` %s → %s (`%.19s` → `%.19s`)\n", m.Repo, m.OldVersion, m.NewVersion, m.Old, m.New)
		} else {
			fmt.Fprintf(&s, "- `paketobuildpacks/%s` `%.19s` → `%.19s`\n", m.Repo, m.Old, m.New)
		}
	}
	s.WriteString("\nFiled by .github/workflows/paketo-bump.yml (#2114).\n")
	return fmt.Sprintf("chore(ci): Paketo pins are behind (%d fixes)", n), s.String()
}

// tracker is the repository's issues, as the bump reads and writes them.
type tracker interface {
	open() ([]issue, error) // the open issues the workflow filed, labelled patching
	create(title, body string) error
	edit(number int, title, body string) error
}

type issue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

// file pushes the bump's branch and files its issue, never a second one: the open issue the workflow
// filed for the same pins is updated; one it filed for older pins is updated to these; with none, one is
// opened. Only the workflow's own issues are read, and only for their key: the branch and the link are
// always the pins' own. A branch that does not push files nothing.
func file(r result, repo string, gh tracker, push func(branch string) error) error {
	if len(r.Moves) == 0 {
		return errors.New("the bump moves no pin")
	}
	if err := push(r.branch()); err != nil {
		return fmt.Errorf("push %s: %w", r.branch(), err)
	}
	open, err := gh.open()
	if err != nil {
		return err
	}
	var found *issue
	for i := range open {
		m := markerRE.FindStringSubmatch(open[i].Body)
		if m == nil || open[i].Author.Login != bot || !open[i].Author.IsBot {
			continue
		}
		if found == nil || m[1] == r.key() {
			found = &open[i]
		}
		if m[1] == r.key() {
			break
		}
	}
	title, body := r.issue(repo)
	if found != nil {
		return gh.edit(found.Number, title, body)
	}
	return gh.create(title, body)
}

// ghCLI is the repository's issues through gh, as GH_TOKEN allows.
type ghCLI struct{ repo string }

func (g ghCLI) open() ([]issue, error) {
	out, err := exec.Command("gh", "issue", "list", "-R", g.repo, "--author", bot, "--label", "patching", "--state", "open", "--limit", "1000", "--json", "number,body,author").Output() //nolint:gosec // the workflow's own repository
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	var open []issue
	return open, json.Unmarshal(out, &open)
}

// labels are the issue's: gh issue create refuses a label the repository lacks, after the branch is pushed (#132)
var labels = []struct{ name, color, description string }{
	{"patching", "0e8a16", "Newer system libraries or build tools to take"},
	{"chore", "ededed", "Not a bug or a feature: tests, docs, tooling"},
	{"P3", "0e8a16", "when there is room"},
}

// create makes each label first, as main-health.yml makes ci: one that is there already refuses, which is no error
// here, and a label still missing after makes the issue's own create refuse.
func (g ghCLI) create(title, body string) error {
	args := []string{"issue", "create", "-R", g.repo, "--title", title, "--body-file", "-"}
	for _, l := range labels {
		_ = exec.Command("gh", "label", "create", l.name, "-R", g.repo, "--color", l.color, "--description", l.description).Run() //nolint:gosec // this file's labels
		args = append(args, "--label", l.name)
	}
	return g.gh(body, args...)
}

func (g ghCLI) edit(number int, title, body string) error {
	return g.gh(body, "issue", "edit", fmt.Sprint(number), "-R", g.repo, "--title", title, "--body-file", "-")
}

func (g ghCLI) gh(stdin string, args ...string) error {
	cmd := exec.Command("gh", args...) //nolint:gosec // the workflow's own repository and the bump's text
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), os.Stdout, os.Stderr
	return cmd.Run()
}

// pushBranch commits the bump job's pins.patch on branch and pushes it, unless the branch is there: its
// name is the pins', so an earlier run pushed the same bump.
func pushBranch(dir, branch string) error {
	ls := exec.Command("git", "ls-remote", "--exit-code", "--heads", "origin", branch) //nolint:gosec // the bump's branch
	ls.Stderr = os.Stderr
	var exit *exec.ExitError
	switch err := ls.Run(); {
	case err == nil:
		fmt.Fprintf(os.Stderr, "%s is there already\n", branch)
		return nil
	case !errors.As(err, &exit) || exit.ExitCode() != 2: // 2: no such branch
		return fmt.Errorf("git ls-remote: %w", err)
	}
	for _, args := range [][]string{
		{"switch", "-c", branch},
		{"apply", filepath.Join(dir, "pins.patch")},
		{"commit", "-s", "-am", "chore(executor): move the Paketo pins to Paketo's newest images"},
		{"push", "origin", branch},
	} {
		cmd := exec.Command("git", args...) //nolint:gosec // the bump's branch and patch
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return nil
}
