package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every file the repository tracks is a check's, by its paths or an always check's covers, or on a nothing line that
// says why: a new file can't arrive without one. Every path the file names matches a file, so none is a typo.
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
		for _, p := range strings.Fields(spec) {
			if one, err := git("ls-files", "--", ":(glob)"+p); err != nil || one == "" {
				t.Errorf("%s in qa/release/checks.txt matches no file (%v)", p, err)
			}
		}
		files, err := git(append(args[:1], append([]string{"-z"}, args[1:]...)...)...)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range strings.Split(files, "\x00") {
			covered[f] = true
		}
	}
	files, err := git("ls-files", "-z")
	if err != nil {
		t.Fatal(err)
	}
	gaps := map[string]int{}
	for _, f := range strings.Split(files, "\x00") {
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

// Only main's own runs count, a push's, a schedule's or one run by hand, each listed apart, the newest of each workflow;
// check runs count through them. A check run of a stranger's comment run, failed, passes when GitHub says it is one,
// however many there are; one of any other run, or one GitHub can't say, refuses. A cancelled nightly with a newer
// green one passes; a newest still pending refuses; a listing short of its count, or holding another event, refuses.
// The asking run is its own.
func TestOnlyMainsNewestRunsCount(t *testing.T) {
	type wf struct{ id, workflow, event, status, conclusion, created string }
	listing := func(total int, rs []wf) string {
		var out []string
		for _, r := range rs {
			c := "null"
			if r.conclusion != "" {
				c = `"` + r.conclusion + `"`
			}
			out = append(out, `{"id":`+r.id+`,"workflow_id":`+r.workflow+`,"name":"w`+r.workflow+`","event":"`+r.event+`","status":"`+r.status+`","conclusion":`+c+`,"created_at":"`+r.created+`","html_url":"https://github.com/o/r/actions/runs/`+r.id+`"}`)
		}
		return `[{"total_count":` + strconv.Itoa(total) + `,"workflow_runs":[` + strings.Join(out, ",") + `]}]`
	}
	// runs are the three listings GitHub answers: each of main's events alone
	runs := func(rs ...wf) [][]byte {
		var out [][]byte
		for _, e := range mainEvents {
			var of []wf
			for _, r := range rs {
				if r.event == e {
					of = append(of, r)
				}
			}
			out = append(out, []byte(listing(len(of), of)))
		}
		return out
	}
	check := func(run, conclusion string) string {
		return `{"name":"c","status":"completed","conclusion":"` + conclusion + `","details_url":"https://github.com/o/r/actions/runs/` + run + `/job/1"}`
	}
	checks := func(cs ...string) string {
		return `[{"total_count":` + strconv.Itoa(len(cs)) + `,"check_runs":[` + strings.Join(cs, ",") + `]}]`
	}
	// events is GitHub's answer for a run no listing holds: its event, or no answer
	events := map[string]string{"2": "issue_comment", "1500": "issue_comment", "6": "pull_request_target", "7": "pull_request", "8": "push"}
	event := func(id string) (string, error) {
		if e, ok := events[id]; ok {
			return e, nil
		}
		return "", errors.New("HTTP 502")
	}
	ci := wf{"1", "10", "push", "completed", "success", "2026-11-24T10:00:00Z"}
	self := wf{"9", "90", "push", "in_progress", "", "2026-11-24T12:00:00Z"}
	for name, c := range map[string]struct {
		runs   [][]byte
		checks string
		ok     bool
	}{
		"good":                                   {runs(ci, self), checks(check("1", "success"), check("9", "")), true},
		"a stranger's failed cla comment run":    {runs(ci, self), checks(check("1", "success"), check("2", "failure")), true},
		"more comment runs than a listing holds": {runs(ci, self), checks(check("1", "success"), check("1500", "skipped")), true},
		"a pull_request_target run":              {runs(ci, self), checks(check("1", "success"), check("6", "failure")), true},
		"a cancelled nightly, a newer green one": {runs(ci, self, wf{"3", "30", "schedule", "completed", "cancelled", "2026-11-23T03:00:00Z"}, wf{"4", "30", "schedule", "completed", "success", "2026-11-24T03:00:00Z"}),
			checks(check("1", "success"), check("3", "cancelled"), check("4", "success")), true},
		"the newest pending": {runs(ci, self, wf{"3", "30", "schedule", "completed", "success", "2026-11-23T03:00:00Z"}, wf{"4", "30", "schedule", "in_progress", "", "2026-11-24T03:00:00Z"}),
			checks(check("1", "success")), false},
		"a pending run with no jobs":            {runs(ci, self, wf{"5", "50", "push", "pending", "", "2026-11-24T10:00:00Z"}), checks(check("1", "success")), false},
		"a startup_failure":                     {runs(ci, self, wf{"5", "50", "push", "completed", "startup_failure", "2026-11-24T10:00:00Z"}), checks(check("1", "success")), false},
		"a failed check run of main's":          {runs(ci, self), checks(check("1", "failure")), false},
		"a check run of a pull_request run":     {runs(ci, self), checks(check("1", "success"), check("7", "success")), false},
		"a check run of a push run not listed":  {runs(ci, self), checks(check("1", "success"), check("8", "success")), false},
		"a check run GitHub can't look up":      {runs(ci, self), checks(check("1", "success"), check("77", "success")), false},
		"a check run naming no run":             {runs(ci, self), checks(`{"name":"app","status":"completed","conclusion":"success","details_url":"https://example.test/x"}`), false},
		"no run but self":                       {runs(self), checks(check("9", "")), false},
		"a page short":                          {[][]byte{[]byte(listing(3, []wf{ci, self})), []byte(listing(0, nil)), []byte(listing(0, nil))}, checks(check("1", "success")), false},
		"a listing holding another event's run": {[][]byte{[]byte(listing(3, []wf{ci, self, {"2", "20", "issue_comment", "completed", "failure", "2026-11-24T11:00:00Z"}})), []byte(listing(0, nil)), []byte(listing(0, nil))}, checks(check("1", "success")), false},
		"not GitHub's answer":                   {[][]byte{[]byte(`{"message":"Bad credentials"}`), []byte(listing(0, nil)), []byte(listing(0, nil))}, checks(check("1", "success")), false},
		"a listing missing":                     {runs(ci, self)[:2], checks(check("1", "success")), false},
	} {
		if bad := badRuns([]byte(c.checks), c.runs, "9", event); (len(bad) == 0) != c.ok {
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
