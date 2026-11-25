package suites

import (
	"bufio"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// selectSuites is scripts/ci-select.sh's answer for these changed paths: every suite it knows, in its order, and
// whether it runs.
func selectSuites(t *testing.T, top string, files ...string) (names []string, run map[string]bool) {
	t.Helper()
	cmd := exec.Command(filepath.Join(top, "scripts", "ci-select.sh")) //nolint:gosec // this tree's script
	cmd.Dir = top
	var in strings.Builder
	for _, f := range files {
		in.WriteString(f + "\x00")
	}
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ci-select.sh %q: %v", files, err)
	}
	run = map[string]bool{}
	for _, line := range strings.Fields(string(out)) {
		name, v, ok := strings.Cut(line, "=")
		if !ok || (v != "true" && v != "false") || run[name] {
			t.Fatalf("ci-select.sh %q: the line %q", files, line)
		}
		names = append(names, name)
		run[name] = v == "true"
	}
	if len(names) == 0 {
		t.Fatalf("ci-select.sh %q named no suite", files)
	}
	return names, run
}

// row is one line of scripts/ci-select.txt: a pattern, and the suites it runs as shell patterns of their names.
type row struct {
	pattern string
	names   []string
}

func table(t *testing.T, top string) []row {
	t.Helper()
	f, err := os.Open(filepath.Join(top, "scripts", "ci-select.txt")) //nolint:gosec // this tree's table
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var rows []row
	for s := bufio.NewScanner(f); s.Scan(); {
		line, _, _ := strings.Cut(s.Text(), "#")
		if fields := strings.Fields(line); len(fields) > 0 {
			rows = append(rows, row{fields[0], fields[1:]})
		}
	}
	return rows
}

// always is what every change runs.
var always = []string{"lint", "tidy", "web", "test-rest", "gitleaks"}

// A change runs the five, and what its paths reach; a change the table can't place runs every suite.
func TestSelectPicksTheSuitesAChangeReaches(t *testing.T) {
	top := root(t)
	for _, c := range []struct {
		why   string
		files []string
		want  []string // beside the five, as patterns of suite names; nil: every suite
	}{
		{"a dashboard screen", []string{"web/src/screens/Versions.tsx"}, []string{"journeys-*"}},
		{"the fleet, which the menus check opens", []string{"web/src/screens/Fleet.tsx"}, []string{"journeys-*", "oidc"}},
		{"a docs page", []string{"docs/src/content/docs/org/usage-reports.md"}, []string{"docs"}},
		{"the tour's page", []string{"docs/src/content/docs/start/take-the-tour.md"}, []string{"docs", "tour*"}}, // tour and tour-classic
		{"a page the server's tests read", []string{"docs/src/content/docs/org/who-can-do-what.md"}, []string{"docs", "server-*", "noee-*"}},
		{"a design note", []string{"design/EGRESS.md"}, []string{"docs"}},
		{"the CLI", []string{"cli/cmd_status.go"}, []string{"server-*", "noee-*"}},
		{"canyon mcp", []string{"cli/mcp.go"}, []string{"server-*", "noee-*", "tour*", "oidc"}},
		{"the CLI's binary", []string{"cmd/canyon/main.go"}, []string{"noee-rest", "oidc"}},
		{"a route", []string{"cmd/canyon-server/routes_gate.go"}, []string{"server-*", "noee-*", "probes"}},
		{"the server's usage report", []string{"cmd/canyon-server/telemetry_v2.go"}, []string{"server-*", "noee-*"}},
		{"the executor on Kubernetes", []string{"executor/kube/run.go"}, []string{"server-*", "noee-*", "runs-*", "kind-*", "handoff-*", "build-arch"}},
		{"the Docker executor", []string{"executor/executor.go"}, []string{"server-*", "noee-*", "runs-*", "provisioners-*", "compose-up", "egress-*", "kind-*", "handoff-*", "build-arch"}},
		{"Canyon Enterprise's patching", []string{"ee/patching/plan.go"}, []string{"server-*", "runs-*", "patch"}},
		{"a provisioner", []string{"provisioners/builtin.go"}, []string{"server-*", "noee-*", "runs-*", "provisioners-*", "kind-*", "handoff-*"}},
		{"the chart", []string{"deploy/helm/canyon/values.yaml"}, []string{"server-*", "noee-*", "deploy-bundle", "helm-chart", "kind-*", "handoff-*", "backup"}},
		{"the notices", []string{"THIRD_PARTY_NOTICES"}, []string{"images"}},
		{"the linters' settings", []string{".golangci.yml"}, []string{}},
		{"two at once", []string{"web/src/screens/Versions.tsx", "docs/src/content/docs/org/usage-reports.md"}, []string{"journeys-*", "docs"}},
		// every suite
		{"go.mod", []string{"go.mod"}, nil},
		{"a workflow", []string{".github/workflows/docs.yml"}, nil},
		{"the suite steps", []string{"scripts/ci-suite.sh"}, nil},
		{"the selector", []string{"scripts/ci-select.sh"}, nil},
		{"its table", []string{"scripts/ci-select.txt"}, nil},
		{"a .github/suites list", []string{".github/suites/journeys.txt"}, nil},
		{"a Dockerfile", []string{"Dockerfile.executor"}, nil},
		{"an image's Dockerfile in the executor", []string{"executor/run.Dockerfile"}, nil},
		{"a migration", []string{"db/migrations/0200_next.sql"}, nil},
		{"an end-to-end test", []string{"e2e/ui/journeys/fleet.spec.ts"}, nil},
		{"the server's core", []string{"cmd/canyon-server/main.go"}, nil},
		{"a path no line matches", []string{"web/src/screens/Versions.tsx", "nowhere/new.txt"}, nil},
		{"a new file at the top", []string{"Taskfile.yml"}, nil},
		{"no changed path", []string{}, nil},
		{"a name git can't hold", []string{"web/src/a.tsx", "../outside"}, nil},
		{"a file and a directory of one name", []string{"web/src/a.tsx", "web/src/a.tsx/b.tsx"}, nil},
	} {
		names, run := selectSuites(t, top, c.files...)
		for _, name := range names {
			want := c.want == nil || slices.Contains(always, name) || slices.ContainsFunc(c.want, func(p string) bool {
				ok, err := path.Match(p, name)
				return err == nil && ok
			})
			if run[name] != want {
				t.Errorf("%s (%q): %s=%v, want %v", c.why, c.files, name, run[name], want)
			}
		}
	}
}

// Every tracked file is on a line of the table, so only a path new to the tree runs every suite for not being on one;
// every line names suites the selector knows.
func TestSelectTableCoversTheTree(t *testing.T) {
	top := root(t)
	rows := table(t, top)
	if len(rows) == 0 {
		t.Fatal("scripts/ci-select.txt holds no line")
	}
	names, _ := selectSuites(t, top, "go.mod")
	args := []string{"ls-files", "--"}
	for _, r := range rows {
		if strings.HasPrefix(r.pattern, "/") || strings.HasPrefix(r.pattern, ":") || strings.HasPrefix(r.pattern, "!") {
			t.Errorf("scripts/ci-select.txt: %q is not one relative glob", r.pattern)
		}
		if len(r.names) == 0 {
			t.Errorf("scripts/ci-select.txt: %s names no suite (- for none)", r.pattern)
		}
		for _, n := range r.names {
			if (n == "*" || n == "-") && len(r.names) > 1 {
				t.Errorf("scripts/ci-select.txt: %s names %s beside other suites", r.pattern, n)
			}
			if n != "*" && n != "-" && !slices.ContainsFunc(names, func(s string) bool { ok, err := path.Match(n, s); return err == nil && ok }) {
				t.Errorf("scripts/ci-select.txt: %s names %s, which matches no suite of scripts/ci-select.sh", r.pattern, n)
			}
		}
		args = append(args, ":(glob,exclude)"+r.pattern)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // the table's own patterns
	cmd.Dir = top
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		t.Errorf("%s is on no line of scripts/ci-select.txt: every change to it runs every suite", f)
	}
}

// selectCompiles is what each Go suite's tests compile, as scripts/ci-suite.sh runs them: server-shard's and runs-shard's
// package, provisioners', and for noee-rest, every package once ee/ is gone (ci-suite.sh's noee).
var selectCompiles = map[string]struct{ tags, pkgs string }{
	"server-1":        {"", "./cmd/canyon-server"},
	"noee-server-1":   {"noee", "./cmd/canyon-server"},
	"runs-1":          {"", "./runs"},
	"provisioners-14": {"", "./provisioners"},
	"noee-rest":       {"noee", "./..."},
}

// The Go each suite's tests compile is on lines of the table that name the suite, or every suite.
func TestSelectTableCoversWhatTheSuitesCompile(t *testing.T) {
	top := root(t)
	rows := table(t, top)
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
	for suite, c := range selectCompiles {
		args := []string{"ls-files", "--"}
		for _, r := range rows {
			if slices.ContainsFunc(r.names, func(n string) bool { ok, err := path.Match(n, suite); return n == "*" || (err == nil && ok) }) {
				args = append(args, ":(glob)"+r.pattern)
			}
		}
		ls := exec.Command("git", args...) //nolint:gosec // the table's own patterns
		ls.Dir = top
		out, err := ls.Output()
		if err != nil {
			t.Fatalf("git ls-files: %v", err)
		}
		covered := map[string]bool{}
		for _, f := range strings.Fields(string(out)) {
			covered[f] = true
		}
		list := []string{"list", "-deps", "-test", "-f", "{{if and .Module .Module.Main}}{{.Dir}}{{range .GoFiles}} {{.}}{{end}}{{range .TestGoFiles}} {{.}}{{end}}{{range .XTestGoFiles}} {{.}}{{end}}{{end}}"}
		if c.tags != "" {
			list = append(list, "-tags", c.tags)
		}
		golist := exec.Command("go", append(list, c.pkgs)...) //nolint:gosec // this table's packages
		golist.Dir = top
		pkgs, err := golist.Output()
		if err != nil {
			t.Fatalf("%s: go list %s: %v", suite, c.pkgs, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(pkgs)), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			rel, err := filepath.Rel(top, fields[0])
			if err != nil {
				t.Fatal(err)
			}
			rel = filepath.ToSlash(rel)
			if c.tags == "noee" && (rel == "ee" || strings.HasPrefix(rel, "ee/")) {
				continue // the noee suites delete ee/ first
			}
			for _, f := range fields[1:] {
				// a test binary's generated main is in the build cache, not the tree: only the tree's files count
				if name := path.Join(rel, f); !strings.Contains(f, "/") && tracked[name] && !covered[name] && !base[name] {
					t.Errorf("%s compiles %s (go list -deps -test -tags %q %s), which no line of scripts/ci-select.txt naming it reaches", suite, name, c.tags, c.pkgs)
				}
			}
		}
	}
}
