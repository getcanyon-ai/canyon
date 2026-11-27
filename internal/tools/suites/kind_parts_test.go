package suites

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// calicoParts is .github/kind/calico-parts.txt: each kube test's part, and the parts in order.
func calicoParts(t *testing.T, top string) (partOf map[string]int, parts []int) {
	t.Helper()
	f, err := os.Open(filepath.Join(top, ".github", "kind", "calico-parts.txt")) //nolint:gosec // this tree's file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	partOf = map[string]int{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		line := s.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		part, err := strconv.Atoi(fields[0])
		if len(fields) != 2 || err != nil || part < 1 || fields[0] != strconv.Itoa(part) {
			t.Errorf("calico-parts.txt:%d: %q is not <part> <test>, a part a whole number from 1", n, line)
			continue
		}
		if was, ok := partOf[fields[1]]; ok {
			t.Errorf("calico-parts.txt:%d: %s is in part %d and part %d: each test runs in one part", n, fields[1], was, part)
		}
		partOf[fields[1]] = part
		if !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(parts)
	for i, p := range parts {
		if p != i+1 {
			t.Errorf("calico-parts.txt names parts %v: want 1 to %d, none empty", parts, len(parts))
			break
		}
	}
	return partOf, parts
}

// kubeTests are the tests make test-kube RUN=TestKind runs: each top-level Test function of executor/kube that the kube
// build tag compiles and the -run pattern TestKind matches, read with Go's own parser.
func kubeTests(t *testing.T, top string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(top, "executor", "kube", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	run := regexp.MustCompile("TestKind") // go test -run's, matched anywhere in the name
	var tests []string
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if !compiledWithKube(f) {
			continue
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && run.MatchString(fn.Name.Name) {
				tests = append(tests, fn.Name.Name)
			}
		}
	}
	slices.Sort(tests)
	return tests
}

// compiledWithKube is whether go test -tags kube compiles f, on Linux, where the suite runs.
func compiledWithKube(f *ast.File) bool {
	for _, group := range f.Comments {
		if group.Pos() > f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				return false
			}
			return expr.Eval(func(tag string) bool { return tag == "kube" || tag == "linux" || tag == "unix" })
		}
	}
	return true
}

// workflowStep is what the check reads of a workflow's step.
type workflowStep struct {
	If   string `yaml:"if"`
	Uses string `yaml:"uses"`
}

// The kind suite's parts on Calico run every test the whole suite ran, each in exactly one part, and name no other;
// the selector knows each part, and kube.yml runs each.
func TestTheCalicoPartsCoverTheKindTests(t *testing.T) {
	top := root(t)
	partOf, parts := calicoParts(t, top)
	tests := kubeTests(t, top)
	if len(tests) == 0 {
		t.Fatal("executor/kube has no kube-tagged test the pattern TestKind matches")
	}
	for _, name := range tests {
		if _, ok := partOf[name]; !ok {
			t.Errorf("%s is in no part of .github/kind/calico-parts.txt: no kind-calico run would run it", name)
		}
	}
	for name := range partOf {
		if !slices.Contains(tests, name) {
			t.Errorf(".github/kind/calico-parts.txt lists %s, which is no kube-tagged test the pattern TestKind matches", name)
		}
	}

	var want []string
	for _, p := range parts {
		want = append(want, fmt.Sprintf("kind-calico-%d", p))
	}
	names, _ := selectSuites(t, top, "go.mod")
	var known []string
	for _, n := range names {
		if strings.HasPrefix(n, "kind-calico-") {
			known = append(known, n)
		}
	}
	if !slices.Equal(known, want) {
		t.Errorf("scripts/ci-select.sh knows the parts %v, want %v", known, want)
	}

	raw, err := os.ReadFile(filepath.Join(top, ".github", "workflows", "kube.yml")) //nolint:gosec // this tree's workflow
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Name     string `yaml:"name"`
			Strategy struct {
				Matrix struct {
					Part []int `yaml:"part"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	job := wf.Jobs["kind-calico"]
	if !slices.Equal(job.Strategy.Matrix.Part, parts) {
		t.Errorf("kube.yml's kind-calico job runs the parts %v, want %v", job.Strategy.Matrix.Part, parts)
	}
	if n := len(parts); job.Name != fmt.Sprintf("kind (calico ${{ matrix.part }}/%d)", n) {
		t.Errorf("kube.yml's kind-calico job is named %q, want it to count %d parts", job.Name, n)
	}
	journey := fmt.Sprintf("matrix.part == %d", len(parts)) // the hand-off journey's set-up, in the last part alone
	if !slices.ContainsFunc(job.Steps, func(s workflowStep) bool {
		return strings.HasPrefix(s.Uses, "actions/setup-node@") && strings.HasPrefix(s.If, journey)
	}) {
		t.Errorf("kube.yml's kind-calico job sets up Node for the hand-off journey not if %s, the last part", journey)
	}
}
