// Command release holds a release to its checks by hand (qa/release/README.md):
//
//	release needs [FROM TO]        the checks FROM..TO calls for; the last release to HEAD without them
//	release check TAG RUNS RUN_ID  refuses HEAD as TAG: a check with no result or a stale one, the last release's after
//	                               checks unticked, or a check run in RUNS (gh api --paginate --slurp of the commit's
//	                               check-runs) that isn't done and good; RUN_ID is the workflow run that asks
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// unheld is the last release from before results: its after checks are nobody's.
const unheld = "v0.11.1"

type check struct {
	id, when string
	paths    []string // nil is always
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
	err := exec.Command("git", args...).Run() //nolint:gosec // as git's
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

// parseChecks reads checks.txt: the checks, and the directories with nothing to check and why.
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
			if l[2] != "always" {
				c.paths = strings.Fields(l[2])
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

// last is the release before commit, release candidates aside.
func last(commit string) (string, error) {
	return git("describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "--exclude", "*-*", commit+"^")
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
			problems = append(problems, fmt.Sprintf("%s: %s's commit %s isn't on main", file, id, r.commit))
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

// badRuns are the commit's check runs that aren't done and good, the workflow run that asks aside. An answer that
// doesn't parse, that holds no run or not all of them is bad.
func badRuns(raw []byte, self string) (problems []string) {
	var pages []struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Name, Status, Conclusion string
			URL                      string `json:"details_url"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil || len(pages) == 0 {
		return []string{fmt.Sprintf("GitHub's answer for the commit's check runs can't be read (%v)", err)}
	}
	total, others := 0, 0
	for _, p := range pages {
		for _, r := range p.Runs {
			total++
			if strings.Contains(r.URL, "/actions/runs/"+self+"/") {
				continue
			}
			if others++; r.Status != "completed" || !slices.Contains([]string{"success", "skipped", "neutral"}, r.Conclusion) {
				problems = append(problems, fmt.Sprintf("check run %s: %s %s %s", r.Name, r.Status, r.Conclusion, r.URL))
			}
		}
	}
	if total != pages[0].Total || others == 0 {
		problems = append(problems, fmt.Sprintf("GitHub listed %d of the commit's %d check runs, %d of them not this run's", total, pages[0].Total, others))
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

func refuse(tag string, runs []byte, self string) (problems []string, err error) {
	checks, err := readChecks("HEAD")
	if err != nil {
		return nil, err
	}
	before, err := last("HEAD")
	if err != nil {
		return nil, err
	}
	if problems, err = missing(checks, "before", tag, before, "HEAD"); err != nil {
		return nil, err
	}
	if before != unheld {
		older, err := last(before)
		if err != nil {
			return nil, err
		}
		after, err := missing(checks, "after", before, older, before)
		if err != nil {
			return nil, err
		}
		problems = append(problems, after...)
	}
	return append(problems, badRuns(runs, self)...), nil
}

func run(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "needs" || len(args) == 3 && args[0] == "needs":
		from, to := "", "HEAD"
		var err error
		if len(args) == 3 {
			from, to = args[1], args[2]
		} else if from, err = last(to); err != nil {
			return err
		}
		checks, err := readChecks("HEAD")
		for _, when := range []string{"before", "after"} {
			need, nerr := needed(checks, when, from, to)
			if err = errors.Join(err, nerr); err != nil {
				return err
			}
			for _, c := range need {
				fmt.Printf("%s the tag: %s\n", when, c.id)
			}
		}
		return nil
	case len(args) == 4 && args[0] == "check":
		runs, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		problems, err := refuse(args[1], runs, args[3])
		if err == nil && len(problems) > 0 {
			err = fmt.Errorf("%s is refused (qa/release/README.md):\n  %s", args[1], strings.Join(problems, "\n  "))
		}
		return err
	}
	return errors.New("usage: release needs [FROM TO] | release check TAG RUNS RUN_ID")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}
