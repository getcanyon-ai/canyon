package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every file the repository tracks is a check's, by its paths or an always check's covers, or on a nothing line that
// says why: a new file can't arrive without one.
func TestEveryFileHasACheckOrSaysWhyNot(t *testing.T) {
	t.Chdir("../../..")
	text, err := os.ReadFile("qa/release/checks.txt")
	if err != nil {
		t.Fatal(err)
	}
	checks, nothing, err := parseChecks(string(text))
	if err != nil {
		t.Fatal(err)
	}
	specs := slices.Collect(maps.Keys(nothing))
	for _, c := range checks {
		specs = append(specs, strings.Join(append(c.paths, c.covers...), " "))
	}
	covered := map[string]bool{}
	for _, spec := range specs {
		if spec == "" {
			continue
		}
		args := []string{"ls-files", "--"}
		for _, p := range strings.Fields(spec) {
			args = append(args, ":(glob)"+p)
		}
		files, err := git(args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range strings.Fields(files) {
			covered[f] = true
		}
	}
	files, err := git("ls-files")
	if err != nil {
		t.Fatal(err)
	}
	gaps := map[string]int{}
	for _, f := range strings.Fields(files) {
		if !covered[f] {
			gaps[unit(f)]++
		}
	}
	for _, u := range slices.Sorted(maps.Keys(gaps)) {
		t.Errorf("%s: %d files with no check in qa/release/checks.txt and no nothing line saying why", u, gaps[u])
	}
}

// unit names a file's place for the message: its top-level directory, the one under cmd/, deploy/, ee/ or internal/,
// or the file itself at the top.
func unit(path string) string {
	parts := strings.SplitN(path, "/", 3)
	switch {
	case len(parts) == 1:
		return path
	case len(parts) == 3 && slices.Contains([]string{"cmd", "deploy", "ee", "internal"}, parts[0]):
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
		cmd := exec.Command("git", args...) //nolint:gosec // test: git, in its own temporary repository
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
	published := []string{unheld, "v0.11.2"}
	refused := func(tag string) []string {
		t.Helper()
		problems, err := held(tag, published)
		if err != nil {
			t.Fatal(err)
		}
		return problems
	}
	g("init", "-q", "-b", "main")
	write("qa/release/checks.txt", "code | before | src/**\nround | before | always\nfirst-report | after | always\n")
	write("src/a.go", "1")
	g("tag", unheld)
	write("src/a.go", "2")
	round := g("rev-parse", "HEAD")
	results := "pass | code | " + round + " | done\npass | round | " + round + " | done\n"
	if p := refused("v0.11.2"); len(p) != 1 || !strings.Contains(p[0], "isn't in the tagged commit") {
		t.Errorf("no results file: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", results)
	write("docs/a.md", "docs after the round")
	if p := refused("v0.11.2"); len(p) != 0 {
		t.Errorf("a docs-only change after the round: %v", p)
	}
	write("src/a.go", "3")
	if p := refused("v0.11.2"); len(p) != 2 || !strings.Contains(p[0], "changed after") || !strings.Contains(p[1], "changed after") {
		t.Errorf("a code change after the round: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", "pass | code | "+strings.Repeat("b", 40)+" | done\npass | round | "+strings.Repeat("b", 40)+" | done\n")
	if p := refused("v0.11.2"); len(p) != 2 || !strings.Contains(p[0], "isn't in the tagged commit's history") {
		t.Errorf("a commit that isn't on main: %v", p)
	}
	again := g("rev-parse", "HEAD~1")
	write("qa/release/results/v0.11.2.txt", "pass | code | "+again+" | done again\npass | round | "+again+" | done again\n")
	g("tag", "v0.11.2")
	tagged := g("rev-parse", "HEAD")
	write("src/a.go", "4")
	now := g("rev-parse", "HEAD")
	write("qa/release/results/v0.11.3.txt", "pass | code | "+now+" | done\npass | round | "+now+" | done\n")
	if p := refused("v0.11.3"); len(p) != 1 || !strings.Contains(p[0], "v0.11.2.txt: no line for first-report") {
		t.Errorf("the last release's after check unticked: %v", p)
	}
	write("qa/release/results/v0.11.2.txt", "pass | code | "+again+" | done again\npass | round | "+again+" | done again\npass | first-report | "+tagged+" | arrived\n")
	if p := refused("v0.11.3"); len(p) != 0 {
		t.Errorf("the last release's after check ticked: %v", p)
	}
	// v0.11.3 refused and never published: v0.11.4's range starts at v0.11.2, so the code v0.11.3 changed is asked for
	g("tag", "v0.11.3")
	write("qa/release/results/v0.11.4.txt", "pass | round | "+g("rev-parse", "HEAD")+" | done\n")
	if p := refused("v0.11.4"); len(p) != 1 || !strings.Contains(p[0], "v0.11.4.txt: no line for code") {
		t.Errorf("a tag between that was never published: %v", p)
	}
}

// A workflow run of the commit that isn't done and good refuses, though it has no check run to show it: one that never
// started, or waits on its concurrency group. The asking run is its own.
func TestEveryWorkflowRunMustBeDoneAndGood(t *testing.T) {
	run := func(id, status, conclusion string) string {
		return `{"id":` + id + `,"name":"CI","status":"` + status + `","conclusion":` + conclusion + `,"html_url":"https://github.com/o/r/actions/runs/` + id + `"}`
	}
	for name, c := range map[string]struct {
		raw string
		ok  bool
	}{
		"good":                      {`[{"total_count":3,"workflow_runs":[` + run("1", "completed", `"success"`) + `,` + run("2", "completed", `"skipped"`) + `,` + run("9", "in_progress", "null") + `]}]`, true},
		"pending, no jobs yet":      {`[{"total_count":1,"workflow_runs":[` + run("1", "pending", "null") + `]}]`, false},
		"queued behind its group":   {`[{"total_count":1,"workflow_runs":[` + run("1", "queued", "null") + `]}]`, false},
		"never started":             {`[{"total_count":1,"workflow_runs":[` + run("1", "completed", `"startup_failure"`) + `]}]`, false},
		"cancelled by a newer main": {`[{"total_count":1,"workflow_runs":[` + run("1", "completed", `"cancelled"`) + `]}]`, false},
		"a page short":              {`[{"total_count":2,"workflow_runs":[` + run("1", "completed", `"success"`) + `]}]`, false},
		"not GitHub's answer":       {`{"message":"Bad credentials"}`, false},
	} {
		if bad := badWorkflows([]byte(c.raw), "9"); (len(bad) == 0) != c.ok {
			t.Errorf("%s: %v, want passed %v", name, bad, c.ok)
		}
	}
}

// An open issue labelled ci refuses, and is named; a pull request with the label isn't one.
func TestAnOpenCIIssueRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		raw  string
		want []string
	}{
		"none":           {`[[]]`, nil},
		"one":            {`[[{"number":124,"title":"ci: server shards time out"}]]`, []string{"open ci issue #124: ci: server shards time out"}},
		"a pull request": {`[[{"number":125,"title":"ci: a longer timeout","pull_request":{"url":"x"}}]]`, nil},
		"unreadable":     {`{"message":"Not Found"}`, []string{"the open ci issues: GitHub's answer can't be read (json: cannot unmarshal object into Go value of type []main.page)"}},
	} {
		got := openCI([]byte(c.raw))
		if len(got) != len(c.want) || (len(got) > 0 && name != "unreadable" && got[0] != c.want[0]) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// Only published releases, candidates aside, are the last release.
func TestTheLastReleaseIsAPublishedOne(t *testing.T) {
	got, err := releasesOf([]byte(`[[{"tag_name":"v0.11.2","draft":false,"prerelease":false},{"tag_name":"v0.11.3-rc.1","draft":false,"prerelease":true},{"tag_name":"v0.11.3","draft":true,"prerelease":false}],[{"tag_name":"v0.11.1"}]]`))
	if err != nil || !slices.Equal(got, []string{"v0.11.2", "v0.11.1"}) {
		t.Errorf("published: %v %v", got, err)
	}
	if _, err := releasesOf([]byte(`{"message":"rate limited"}`)); err == nil {
		t.Error("an answer that isn't a list was read")
	}
}
