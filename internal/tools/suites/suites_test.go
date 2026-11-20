package suites

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// compiles is what each suite's Go code compiles: its tests, by their build tags, and the binaries it builds besides
// the server. Every package of this module in their dependencies must be inside the suite's paths or the foundation's,
// or a change to it would reach the suite unseen. A suite with no Go code of its own (journeys, backup, old-cli, helm,
// tour) has no row.
var compiles = map[string][]struct{ tags, pkgs string }{
	"kind": {
		{"kube", "./executor/kube"},
		{"handoff,handoffrepo,kindcanyon", "./e2e/handoff"},
		{"", "./cmd/canyon-executor-kube ./cmd/canyon-identity"},
	},
	"probes":       {{"stack", "./e2e"}},
	"egress":       {{"stack", "./e2e"}},
	"oidc":         {{"oidc", "./e2e/oidc"}},
	"patch":        {{"oidc,patch", "./e2e/oidc"}},
	"build-arch":   {{"kube", "./executor"}},
	"executor-e2e": {{"e2e", "./executor"}},
}

// foundation is what every suite must run on: what every stack is built from, and what decides what runs.
var foundation = []string{"go.mod", "go.sum", "Makefile", ".github/workflows/**", ".github/actions/**", ".github/suites/**",
	"contracts/*.go", "contracts/schema/**", "contracts/testdata/**", "cmd/canyon-server/*.go"}

func root(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// patterns are a suite file's lines, comments and blank lines dropped.
func patterns(t *testing.T, top, suite string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(top, ".github", "suites", suite+".txt")) //nolint:gosec // a suite file this test named
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	for s := bufio.NewScanner(f); s.Scan(); {
		line, _, _ := strings.Cut(s.Text(), "#")
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// matched is every tracked file the suite's patterns name, as git's own :(glob) pathspecs match it: reach.sh's diff.
func matched(t *testing.T, top, suite string) map[string]bool {
	t.Helper()
	args := []string{"ls-files", "--"}
	for _, p := range patterns(t, top, suite) {
		if rest, ok := strings.CutPrefix(p, "!"); ok {
			args = append(args, ":(glob,exclude)"+rest)
		} else {
			args = append(args, ":(glob)"+p)
		}
	}
	cmd := exec.Command("git", args...) //nolint:gosec // the suite file's own patterns
	cmd.Dir = top
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files for %s: %v", suite, err)
	}
	files := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		files[f] = true
	}
	return files
}

func suites(t *testing.T, top string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(top, ".github", "suites", "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no suite files: %v", err)
	}
	var names []string
	for _, f := range files {
		if name := strings.TrimSuffix(filepath.Base(f), ".txt"); name != "foundation" {
			names = append(names, name)
		}
	}
	return names
}

// Every pattern is one word (reach.sh splits them on space), and the foundation holds the shared foundation.
func TestTheSuiteFilesAreWellFormed(t *testing.T) {
	top := root(t)
	for _, suite := range append(suites(t, top), "foundation") {
		lines := patterns(t, top, suite)
		if len(lines) == 0 || strings.HasPrefix(lines[0], "!") {
			t.Errorf("%s: a suite file starts with a path it includes", suite)
		}
		for _, p := range lines {
			if strings.ContainsAny(p, " \t") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, ":") {
				t.Errorf("%s: %q is not one relative glob", suite, p)
			}
		}
	}
	got := patterns(t, top, "foundation")
	for _, p := range foundation {
		if !slices.Contains(got, p) {
			t.Errorf("foundation.txt lacks %s", p)
		}
	}
}

// Each suite's paths cover what its Go code compiles: a change to a package its tests or its binaries build reaches it.
func TestEachSuiteCoversWhatItCompiles(t *testing.T) {
	top := root(t)
	base := matched(t, top, "foundation")
	ls := exec.Command("git", "ls-files")
	ls.Dir = top
	all, err := ls.Output()
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	for _, f := range strings.Fields(string(all)) {
		tracked[f] = true
	}
	for suite, roots := range compiles {
		covered := matched(t, top, suite)
		for _, r := range roots {
			args := []string{"list", "-deps", "-test", "-f", "{{if and .Module .Module.Main}}{{.Dir}}{{end}}"}
			if r.tags != "" {
				args = append(args, "-tags", r.tags)
			}
			cmd := exec.Command("go", append(args, strings.Fields(r.pkgs)...)...) //nolint:gosec // this table's packages
			cmd.Dir = top
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: go list %s: %v", suite, r.pkgs, err)
			}
			dirs := strings.Fields(string(out))
			slices.Sort(dirs)
			for _, dir := range slices.Compact(dirs) {
				rel, err := filepath.Rel(top, dir)
				if err != nil {
					t.Fatal(err)
				}
				gofiles, _ := filepath.Glob(filepath.Join(dir, "*.go"))
				for _, f := range gofiles {
					name := path.Join(filepath.ToSlash(rel), filepath.Base(f))
					if tracked[name] && !covered[name] && !base[name] {
						t.Errorf("%s compiles %s (go list -deps -test -tags %q %s), which .github/suites/%s.txt doesn't reach", suite, name, r.tags, r.pkgs, suite)
					}
				}
			}
		}
	}
}

// reachCall is a step's run of reach.sh: the suites it names.
var reachCall = regexp.MustCompile(`\.github/suites/reach\.sh((?:[ \t]+[a-z0-9-]+)+)`)

// Every suite is asked for by a workflow, every suite a workflow asks for has a file, and a workflow that asks
// reach.sh filters no pull request by paths of its own: one map decides, and nothing beside it.
func TestTheWorkflowsAskForTheSuites(t *testing.T) {
	top := root(t)
	files, err := filepath.Glob(filepath.Join(top, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the repository's own workflows
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			On map[string]yaml.Node `yaml:"on"`
		}
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var calls [][]byte
		for _, m := range reachCall.FindAllSubmatch(raw, -1) {
			calls = append(calls, m[1])
		}
		if len(calls) == 0 {
			continue
		}
		var pr struct {
			Paths []string `yaml:"paths"`
		}
		if node, ok := wf.On["pull_request"]; ok {
			_ = node.Decode(&pr)
		}
		if len(pr.Paths) > 0 {
			t.Errorf("%s asks reach.sh and also filters pull requests by paths of its own", filepath.Base(f))
		}
		for _, c := range calls {
			for _, name := range strings.Fields(string(bytes.TrimSpace(c))) {
				asked[name] = true
				if _, err := os.Stat(filepath.Join(top, ".github", "suites", name+".txt")); err != nil || name == "foundation" { //nolint:gosec // a name the workflow's own regexp matched, [a-z0-9-]
					t.Errorf("%s asks for suite %s, which .github/suites has no file for", filepath.Base(f), name)
				}
			}
		}
	}
	for _, name := range suites(t, top) {
		if !asked[name] {
			t.Errorf(".github/suites/%s.txt is a suite no workflow asks for", name)
		}
	}
}

// reach.sh answers what a change reaches, as a pull request's paths job asks it: in a scratch repository holding this
// tree's suite files, one commit per case on a common base.
func TestReachAnswersWhatAChangeReaches(t *testing.T) {
	top, scratch := root(t), t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // this test's own git commands
		cmd.Dir = scratch
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.CopyFS(filepath.Join(scratch, ".github", "suites"), os.DirFS(filepath.Join(top, ".github", "suites"))); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	names := suites(t, top)
	reach := func(file, event string, base string) map[string]string {
		t.Helper()
		cmd := exec.Command(filepath.Join(scratch, ".github", "suites", "reach.sh"), names...) //nolint:gosec // this tree's script
		cmd.Dir = scratch
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_NAME="+event, "GITHUB_OUTPUT=", "SUITES_BASE="+base, "SUITES_HEAD=HEAD")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("reach.sh after %s: %v", file, err)
		}
		got := map[string]string{}
		for _, line := range strings.Fields(string(out)) {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		return got
	}
	for _, c := range []struct {
		file string
		want map[string]string // the suites named true; every other is false
	}{
		{"web/src/screens/Fleet.tsx", map[string]string{"journeys": "true"}},
		{"cmd/canyon-server/telemetry_v2.go", map[string]string{"old-cli": "true"}}, // every stack but old-cli's, which reads all the server answers, turns the report off
		{"cmd/canyon-server/routes_gate.go", map[string]string{"probes": "true", "old-cli": "true"}},
		{"executor/kube/run.go", map[string]string{"kind": "true", "build-arch": "true", "executor-e2e": "true", "old-cli": "true"}},
		{"docs/src/content/docs/org/usage-reports.md", map[string]string{}},
		{"go.mod", nil},                    // the foundation: every suite
		{"cmd/canyon-server/main.go", nil}, // the server's core
		{"contracts/schema/finding.schema.json", nil},
		{".github/workflows/docs.yml", nil},
	} {
		path := filepath.Join(scratch, filepath.FromSlash(c.file))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(c.file+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "-q", "-m", c.file)
		got := reach(c.file, "pull_request", "HEAD^1")
		for _, name := range names {
			want := "false"
			if c.want == nil || c.want[name] == "true" {
				want = "true"
			}
			if got[name] != want {
				t.Errorf("a change to %s: %s=%s, want %s", c.file, name, got[name], want)
			}
		}
		git("reset", "-q", "--hard", "HEAD^1")
	}
	// A pull request can't edit the lists to skip a suite: they are read as the base has them. One that empties the
	// foundation and journeys' list beside a change to the dashboard still runs every suite, and a suite the base has
	// no list for runs.
	for _, f := range []string{"foundation", "journeys"} {
		if err := os.WriteFile(filepath.Join(scratch, ".github", "suites", f+".txt"), []byte("nothing/**\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(scratch, ".github", "suites", "fresh.txt"), []byte("nothing/**\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "web"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "web", "index.html"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "the lists edited beside the dashboard")
	names = append(names, "fresh")
	for name, v := range reach("the lists edited beside the dashboard", "pull_request", "HEAD^1") {
		if v != "true" {
			t.Errorf("a pull request that edits the lists: %s=%s, want true (the base's lists decide)", name, v)
		}
	}
	git("reset", "-q", "--hard", "HEAD^1")
	names = names[:len(names)-1]
	for name, v := range reach("a push", "push", "HEAD") { // any other event runs every suite
		if v != "true" {
			t.Errorf("a push: %s=%s", name, v)
		}
	}
	for name, v := range reach("a base git can't read", "pull_request", "0000000000000000000000000000000000000000") { // fail closed
		if v != "true" {
			t.Errorf("an unreadable base: %s=%s", name, v)
		}
	}
}
