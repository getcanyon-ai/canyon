// Command release holds a release to its checks by hand (qa/release/README.md):
//
//	release needs FROM TO | RELEASES  the checks FROM..TO calls for, or the last release's to HEAD
//	release check TAG DIR RUN_ID      refuses HEAD as TAG: a check with no result or a stale one, the last release's
//	                                  after checks unticked, a run of the commit that isn't done and good, or an open
//	                                  ci issue. RUN_ID is the workflow run that asks.
//
// GitHub's answers are files, each `gh api --paginate --slurp`: RELEASES (and DIR/releases.json) of
// repos/getcanyon-ai/canyon-releases/releases, and in DIR check-runs.json and runs.json of the commit's
// check-runs?filter=latest and actions/runs?head_sha=, and issues.json of issues?labels=ci&state=open.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// unheld is the last release from before results: its after checks are nobody's.
const unheld = "v0.11.1"

type check struct {
	id, when string
	paths    []string // nil is always
	covers   []string // an always check's: the paths it answers for
}

type result struct{ id, commit string }

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// git is git's output, trimmed.
func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output() //nolint:gosec // this repository's own refs and paths
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// yes is a git command that answers by its exit: 0 yes, 1 no, anything else an error.
func yes(args ...string) (bool, error) {
	err := exec.Command("git", args...).Run() //nolint:gosec // this repository's own refs and paths
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// lines are text's lines cut at |, trimmed, comments and blank ones dropped.
func lines(text string) (out [][]string) {
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line == "" || line[0] == '#' {
			continue
		}
		var cells []string
		for _, c := range strings.SplitN(line, "|", 4) {
			cells = append(cells, strings.TrimSpace(c))
		}
		out = append(out, cells)
	}
	return out
}

// parseChecks reads checks.txt: the checks, and the paths with nothing to check, each line's pathspecs by why.
func parseChecks(text string) (checks []check, nothing map[string]string, err error) {
	nothing = map[string]string{}
	seen := map[string]bool{}
	for _, l := range lines(text) {
		switch {
		case len(l) != 3 || slices.Contains(l, ""):
			return nil, nil, fmt.Errorf("checks.txt: %q is not three cells", l)
		case l[0] == "nothing":
			nothing[l[1]] = l[2]
		case l[1] != "before" && l[1] != "after" || seen[l[0]]:
			return nil, nil, fmt.Errorf("checks.txt: %q is no before or after, or a second %s", l, l[0])
		default:
			c := check{id: l[0], when: l[1]}
			if f := strings.Fields(l[2]); f[0] == "always" {
				c.covers = f[1:]
			} else {
				c.paths = f
			}
			checks, seen[c.id] = append(checks, c), true
		}
	}
	return checks, nothing, nil
}

// parseResults reads a results file, strictly: an unknown word or id, a second line for one, a short commit or a skip
// with no reason is an error.
func parseResults(text string, checks []check) (map[string]result, error) {
	out := map[string]result{}
	for _, l := range lines(text) {
		switch {
		case len(l) != 4 || l[0] != "pass" && l[0] != "skip":
			return nil, fmt.Errorf("%q is not pass or skip, an id, a commit and a note", l)
		case !slices.ContainsFunc(checks, func(c check) bool { return c.id == l[1] }):
			return nil, fmt.Errorf("%q names no check", l)
		case out[l[1]] != result{}:
			return nil, fmt.Errorf("a second line for %s", l[1])
		case !commitID.MatchString(l[2]):
			return nil, fmt.Errorf("%q: the commit is its whole id", l)
		case l[0] == "skip" && l[3] == "":
			return nil, fmt.Errorf("%q skips and doesn't say why", l)
		}
		out[l[1]] = result{l[1], l[2]}
	}
	return out, nil
}

// changed reports whether from..to changes what c is called for by.
func (c check) changed(from, to string) (bool, error) {
	args := []string{"diff", "--quiet", from, to, "--"}
	if c.paths == nil {
		args = append(args, ".", ":(exclude)docs", ":(exclude)design", ":(exclude)qa/release")
	}
	for _, p := range c.paths {
		args = append(args, ":(glob)"+p)
	}
	same, err := yes(args...)
	return !same, err
}

// needed are the when checks from..to calls for: every always one, and those whose paths changed.
func needed(checks []check, when, from, to string) (out []check, err error) {
	for _, c := range checks {
		if c.when != when {
			continue
		}
		change := c.paths == nil
		if !change {
			if change, err = c.changed(from, to); err != nil {
				return nil, err
			}
		}
		if change {
			out = append(out, c)
		}
	}
	return out, nil
}

// last is the release before commit: the nearest tag published on getcanyon-ai/canyon-releases, so a tag refused, or
// one whose release never went out, never shortens the next one's range.
func last(commit string, published []string) (string, error) {
	if len(published) == 0 {
		return "", errors.New("no published release is named")
	}
	args := []string{"describe", "--tags", "--abbrev=0"}
	for _, tag := range published {
		args = append(args, "--match", tag)
	}
	return git(append(args, commit+"^")...)
}

// pages decodes a gh api --paginate --slurp answer: a list of pages, each list or object a T.
func pages[T any](raw []byte) ([]T, error) {
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil, fmt.Errorf("GitHub's answer can't be read (%v)", err)
	}
	return out, nil
}

// releasesOf are the published releases' tags, release candidates aside.
func releasesOf(raw []byte) ([]string, error) {
	all, err := pages[[]struct {
		Tag               string `json:"tag_name"`
		Draft, Prerelease bool
	}](raw)
	var tags []string
	for _, page := range all {
		for _, r := range page {
			if !r.Draft && !r.Prerelease && !strings.Contains(r.Tag, "-") {
				tags = append(tags, r.Tag)
			}
		}
	}
	return tags, err
}

// missing is what from..to calls for and tag's results, as HEAD has them, don't hold.
func missing(checks []check, when, tag, from, to string) (problems []string, err error) {
	file := "qa/release/results/" + tag + ".txt"
	text, err := git("show", "HEAD:"+file)
	if err != nil {
		return []string{file + " isn't in the tagged commit"}, nil
	}
	results, err := parseResults(text, checks)
	if err != nil {
		return []string{file + ": " + err.Error()}, nil
	}
	for id, r := range results { // every line's commit, called for or not
		if on, err := yes("merge-base", "--is-ancestor", r.commit, "HEAD"); err != nil || !on {
			problems = append(problems, fmt.Sprintf("%s: %s's commit %s isn't in the tagged commit's history", file, id, r.commit))
		}
	}
	need, err := needed(checks, when, from, to)
	if err != nil || problems != nil {
		return problems, err
	}
	for _, c := range need {
		r, done := results[c.id]
		if !done {
			problems = append(problems, fmt.Sprintf("%s: no line for %s", file, c.id))
			continue
		}
		if stale, err := c.changed(r.commit, to); err != nil {
			return nil, err
		} else if stale {
			problems = append(problems, fmt.Sprintf("%s: %s was done at %s, and what it checks changed after", file, c.id, r.commit))
		}
	}
	return problems, nil
}

// good are the conclusions a run passes with; a run not done has none.
var good = []string{"success", "skipped", "neutral"}

// runID is the workflow run a check run's details_url names.
var runID = regexp.MustCompile(`/actions/runs/([0-9]+)/`)

// badRuns are the commit's runs that aren't done and good. Of its workflow runs, only main's own count, a push's, a
// schedule's or one run by hand (a stranger's comment starts a run on main's head too), and of those the newest of each
// workflow, so a later run passes for an earlier one that failed; a newest that hasn't finished refuses. A check run
// counts through its workflow run; one whose run isn't among the commit's refuses. self, the run that asks, is aside.
// An answer that doesn't parse, or holds less than it says, refuses, and so does one with no run but self.
func badRuns(checkRuns, workflowRuns []byte, self string) (problems []string) {
	runs, err := pages[struct {
		Total int `json:"total_count"`
		Runs  []struct {
			ID                       int64
			WorkflowID               int64 `json:"workflow_id"`
			Name, Status, Conclusion string
			Event                    string
			Created                  string `json:"created_at"` // RFC 3339 in UTC, so it sorts as text
			URL                      string `json:"html_url"`
		} `json:"workflow_runs"`
	}](workflowRuns)
	checks, cerr := pages[struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Name, Status, Conclusion string
			URL                      string `json:"details_url"`
		} `json:"check_runs"`
	}](checkRuns)
	if err = errors.Join(err, cerr); err != nil {
		return []string{"the commit's runs: " + err.Error()}
	}
	listed, newest := map[string]bool{}, map[int64]int{} // every run's id; each workflow's newest of main's own, by index
	type run struct{ id, line, conclusion, created string }
	var all []run
	total := 0
	for _, p := range runs {
		for _, r := range p.Runs {
			total++
			id := strconv.FormatInt(r.ID, 10)
			listed[id] = true
			if !slices.Contains([]string{"push", "schedule", "workflow_dispatch"}, r.Event) {
				continue
			}
			all = append(all, run{id, fmt.Sprintf("workflow run %s: %s %s %s", r.Name, r.Status, r.Conclusion, r.URL), r.Conclusion, r.Created})
			if at, seen := newest[r.WorkflowID]; !seen || r.Created > all[at].created {
				newest[r.WorkflowID] = len(all) - 1
			}
		}
	}
	if total != runs[0].Total {
		problems = append(problems, fmt.Sprintf("GitHub listed %d of the commit's %d workflow runs", total, runs[0].Total))
	}
	counted := map[string]bool{}
	for _, at := range newest {
		if r := all[at]; r.id != self {
			counted[r.id] = true
			if !slices.Contains(good, r.conclusion) {
				problems = append(problems, r.line)
			}
		}
	}
	if len(counted) == 0 {
		problems = append(problems, "the commit has no run of main's own but this one")
	}
	total = 0
	for _, p := range checks {
		for _, c := range p.Runs {
			total++
			m := runID.FindStringSubmatch(c.URL)
			switch {
			case m == nil || !listed[m[1]]:
				problems = append(problems, fmt.Sprintf("check run %s belongs to no workflow run of the commit's: %s", c.Name, c.URL))
			case counted[m[1]] && !slices.Contains(good, c.Conclusion):
				problems = append(problems, fmt.Sprintf("check run %s: %s %s %s", c.Name, c.Status, c.Conclusion, c.URL))
			}
		}
	}
	if total != checks[0].Total {
		problems = append(problems, fmt.Sprintf("GitHub listed %d of the commit's %d check runs", total, checks[0].Total))
	}
	return problems
}

// openCI are the open issues labelled ci, which main-health opens for a red run on main: one earlier in the range
// isn't the tagged commit's own.
func openCI(raw []byte) (problems []string) {
	all, err := pages[[]struct {
		Number int
		Title  string
		PR     any `json:"pull_request"`
	}](raw)
	if err != nil {
		return []string{"the open ci issues: " + err.Error()}
	}
	for _, page := range all {
		for _, i := range page {
			if i.PR == nil {
				problems = append(problems, fmt.Sprintf("open ci issue #%d: %s", i.Number, i.Title))
			}
		}
	}
	return problems
}

func readChecks(ref string) ([]check, error) {
	text, err := git("show", ref+":qa/release/checks.txt")
	if err != nil {
		return nil, err
	}
	checks, _, err := parseChecks(text)
	return checks, err
}

// held is what the results files hold against tag: this release's before checks, and the last release's after ones.
func held(tag string, published []string) (problems []string, err error) {
	checks, err := readChecks("HEAD")
	if err != nil {
		return nil, err
	}
	before, err := last("HEAD", published)
	if err != nil {
		return nil, err
	}
	if problems, err = missing(checks, "before", tag, before, "HEAD"); err != nil || before == unheld {
		return problems, err
	}
	older, err := last(before, published)
	if err != nil {
		return nil, err
	}
	after, err := missing(checks, "after", before, older, before)
	return append(problems, after...), err
}

// read is a file of DIR's.
func read(dir, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the files the workflow wrote, in the directory named on the command line
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "needs" || len(args) == 3 && args[0] == "needs":
		from, to := "", "HEAD"
		if len(args) == 3 {
			from, to = args[1], args[2]
		} else {
			raw, err := os.ReadFile(args[1]) //nolint:gosec // named on the command line
			if err != nil {
				return err
			}
			published, err := releasesOf(raw)
			if err == nil {
				from, err = last(to, published)
			}
			if err != nil {
				return err
			}
		}
		checks, err := readChecks("HEAD")
		for _, when := range []string{"before", "after"} {
			var need []check
			if err == nil {
				need, err = needed(checks, when, from, to)
			}
			for _, c := range need {
				fmt.Printf("%s the tag: %s\n", when, c.id)
			}
		}
		return err
	case len(args) == 4 && args[0] == "check":
		var raw [4][]byte
		for i, name := range []string{"releases.json", "check-runs.json", "runs.json", "issues.json"} {
			var err error
			if raw[i], err = read(args[2], name); err != nil {
				return err
			}
		}
		published, err := releasesOf(raw[0])
		if err != nil {
			return err
		}
		problems, err := held(args[1], published)
		if err != nil {
			return err
		}
		problems = append(append(problems, badRuns(raw[1], raw[2], args[3])...), openCI(raw[3])...)
		if len(problems) > 0 {
			return fmt.Errorf("%s is refused (qa/release/README.md):\n  %s", args[1], strings.Join(problems, "\n  "))
		}
		return nil
	}
	return errors.New("usage: release needs FROM TO | release needs RELEASES | release check TAG DIR RUN_ID")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}
