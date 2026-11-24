package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every top-level directory, and each under cmd/, deploy/ and ee/, is a check's or has nothing to check and why: a new
// area can't arrive without one. An always check covers nothing here, or this would hold for any directory.
func TestEveryDirectoryHasACheckOrSaysWhyNot(t *testing.T) {
	t.Chdir("../../..")
	text, err := os.ReadFile("qa/release/checks.txt")
	if err != nil {
		t.Fatal(err)
	}
	checks, nothing, err := parseChecks(string(text))
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, c := range checks {
		args := []string{"ls-files", "--"}
		for _, p := range c.paths {
			args = append(args, ":(glob)"+p)
		}
		if c.paths == nil {
			continue
		}
		files, err := git(args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range strings.Fields(files) {
			covered[unit(f)] = true
		}
	}
	files, err := git("ls-files")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(files) {
		if u := unit(f); u != "" && !covered[u] && nothing[u] == "" {
			t.Errorf("%s has no check in qa/release/checks.txt and no nothing line saying why", u)
			covered[u] = true
		}
	}
	for dir := range nothing {
		if covered[dir] {
			t.Errorf("%s has a check and a nothing line", dir)
		}
	}
}

// unit is the directory a path is held to: its top-level one, or the one under cmd/, deploy/ or ee/; "" for a file
// beside those directories.
func unit(path string) string {
	parts := strings.SplitN(path, "/", 3)
	switch {
	case len(parts) == 1, len(parts) == 2 && (parts[0] == "cmd" || parts[0] == "deploy" || parts[0] == "ee"):
		return "" // a file, not a directory
	case len(parts) == 3 && (parts[0] == "cmd" || parts[0] == "deploy" || parts[0] == "ee"):
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// A results file is read strictly.
func TestAResultsFileIsReadStrictly(t *testing.T) {
	checks := []check{{id: "upgrade", when: "before"}}
	commit := strings.Repeat("a", 40)
	for line, ok := range map[string]bool{
		"pass | upgrade | " + commit + " | two apps":                               true,
		"skip | upgrade | " + commit + " | no cluster this round":                  true,
		"skip | upgrade | " + commit + " |":                                        false,
		"passed | upgrade | " + commit + " | two apps":                             false,
		"pass | upgrades | " + commit + " | two apps":                              false,
		"pass | upgrade | " + commit[:12] + " | two apps":                          false,
		"pass | upgrade | " + commit + " | a\npass | upgrade | " + commit + " | b": false,
		"pass | upgrade": false,
	} {
		if _, err := parseResults(line, checks); (err == nil) != ok {
			t.Errorf("%q: %v, want taken %v", line, err, ok)
		}
	}
}

// Only a check run that is done and good passes, the asking run's own aside; an answer that can't be read, or holds none
// but the asker's, refuses.
func TestOnlyGoodCheckRunsPass(t *testing.T) {
	run := func(conclusion, status, id string) string {
		return `{"name":"x","status":"` + status + `","conclusion":"` + conclusion + `","details_url":"https://github.com/o/r/actions/runs/` + id + `/job/1"}`
	}
	for name, c := range map[string]struct {
		raw string
		ok  bool
	}{
		"good":                {`[{"total_count":3,"check_runs":[` + run("success", "completed", "1") + `,` + run("skipped", "completed", "1") + `,` + run("", "in_progress", "9") + `]}]`, true},
		"failed":              {`[{"total_count":1,"check_runs":[` + run("failure", "completed", "1") + `]}]`, false},
		"cancelled":           {`[{"total_count":1,"check_runs":[` + run("cancelled", "completed", "1") + `]}]`, false},
		"pending":             {`[{"total_count":1,"check_runs":[` + run("", "in_progress", "1") + `]}]`, false},
		"none":                {`[{"total_count":0,"check_runs":[]}]`, false},
		"the asker's alone":   {`[{"total_count":1,"check_runs":[` + run("", "in_progress", "9") + `]}]`, false},
		"a page short":        {`[{"total_count":2,"check_runs":[` + run("success", "completed", "1") + `]}]`, false},
		"not GitHub's answer": {`{"message":"Bad credentials"}`, false},
	} {
		if bad := badRuns([]byte(c.raw), "9"); (len(bad) == 0) != c.ok {
			t.Errorf("%s: %v, want passed %v", name, bad, c.ok)
		}
	}
}

// A check done at a commit is stale once what it checks changes after it: a code change after the round refuses the
// tag, a docs-only one doesn't. A commit not on main refuses, and the last release's after checks refuse the next tag
// until they're ticked.
func TestAResultHoldsUntilWhatItChecksChanges(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	g := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		g("add", path)
		g("commit", "-qm", path)
	}
	refused := func(tag string) []string {
		t.Helper()
		checks, err := readChecks("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		before, err := last("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		problems, err := missing(checks, "before", tag, before, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if before != unheld {
			older, _ := last(before)
			after, err := missing(checks, "after", before, older, before)
			if err != nil {
				t.Fatal(err)
			}
			problems = append(problems, after...)
		}
		return problems
	}
	g("init", "-q", "-b", "main")
	write("qa/release/checks.txt", "code | before | src/**\nfirst-report | after | always\n")
	write("src/a.go", "1")
	g("tag", unheld)
	write("src/a.go", "2")
	round := g("rev-parse", "HEAD")
	results := "pass | code | " + round + " | done\n"
	if p := refused("v0.11.2"); len(p) != 1 || !strings.Contains(p[0], "isn't in the tagged commit") {
		t.Errorf("no results file: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", results)
	write("docs/a.md", "docs after the round")
	if p := refused("v0.11.2"); len(p) != 0 {
		t.Errorf("a docs-only change after the round: %v", p)
	}
	write("src/a.go", "3")
	if p := refused("v0.11.2"); len(p) != 1 || !strings.Contains(p[0], "changed after") {
		t.Errorf("a code change after the round: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", "pass | code | "+strings.Repeat("b", 40)+" | done\n")
	if p := refused("v0.11.2"); len(p) != 1 || !strings.Contains(p[0], "isn't on main") {
		t.Errorf("a commit that isn't on main: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", "pass | code | "+g("rev-parse", "HEAD~1")+" | done again\n")
	g("tag", "v0.11.2")
	tagged := g("rev-parse", "HEAD")
	write("src/a.go", "4")
	write("qa/release/results/v0.11.3.txt", "pass | code | "+g("rev-parse", "HEAD")+" | done\n")
	if p := refused("v0.11.3"); len(p) != 1 || !strings.Contains(p[0], "v0.11.2.txt: no line for first-report") {
		t.Errorf("the last release's after check unticked: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", "pass | code | "+g("rev-parse", "v0.11.2~1")+" | done again\npass | first-report | "+tagged+" | arrived\n")
	if p := refused("v0.11.3"); len(p) != 0 {
		t.Errorf("the last release's after check ticked: %v", p)
	}
}
