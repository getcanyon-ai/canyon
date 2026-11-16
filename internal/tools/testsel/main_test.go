package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestEveryTestIsInExactlyOneShard(t *testing.T) {
	// TestA is a prefix of TestAB and TestAB_x: only an anchored pattern keeps each to its own shard
	names := []string{"TestAB_x", "TestA", "TestAB", "ExampleA", "FuzzA", "TestB", "TestC"}
	for n := 1; n <= len(names)+1; n++ {
		groups := group(names, n)
		patterns := make([]string, n)
		count := map[int]bool{}
		for i, g := range groups {
			patterns[i] = pattern(g)
			count[len(g)] = true
		}
		if err := check(names, patterns); err != nil {
			t.Errorf("%d shards: %v", n, err)
		}
		if len(count) > 2 {
			t.Errorf("%d shards: sizes uneven %v", n, groups)
		}
		if again := group([]string{"TestC", "TestB", "FuzzA", "ExampleA", "TestAB", "TestA", "TestAB_x"}, n); !reflect.DeepEqual(again, groups) {
			t.Errorf("%d shards: the same tests in another order split differently: %v, %v", n, groups, again)
		}
	}
}

func TestCheckRefusesATestInNoShardOrInTwo(t *testing.T) {
	names := []string{"TestA", "TestB", "TestC"}
	for _, c := range []struct {
		patterns []string
		want     string
	}{
		{[]string{pattern([]string{"TestA"}), pattern([]string{"TestB"})}, "TestC in 0 shards"},
		{[]string{pattern([]string{"TestA", "TestB"}), pattern([]string{"TestB", "TestC"})}, "TestB in 2 shards [1 2]"},
		{[]string{"TestA|TestB", pattern([]string{"TestC"})}, ""},           // unanchored, yet each in one
		{[]string{"Test", pattern([]string{"TestC"})}, "TestC in 2 shards"}, // unanchored: TestC twice
		{[]string{"^(TestA|TestB|TestC)$/x"}, "'/'"},
	} {
		err := check(names, c.patterns)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%q: %v, want %q", c.patterns, err, c.want)
		}
	}
}

// fixture is testdata/<name>, a module of its own, as an absolute path.
func fixture(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The shardpkg fixture holds each kind of top-level test go test runs (a test, a prefix of another,
// a subtest's parent, an example with output, a fuzz target) next to a benchmark it doesn't, and a
// TestMain. Every run here is a real go test -json.
func TestShardsRunEachTestExactlyOnce(t *testing.T) {
	dir := fixture(t, "shardpkg")
	all := []string{"ExampleA", "FuzzA", "TestA", "TestAB", "TestFails"}
	ran, err := shards(io.Discard, dir, ".", []string{"-count=1"}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"ExampleA": 1, "FuzzA": 1, "TestA": 1, "TestAB": 1, "TestFails": 1}; !reflect.DeepEqual(ran, want) {
		t.Errorf("3 shards ran %v, want each test once and no benchmark: %v", ran, want)
	}
	ran, err = shards(io.Discard, dir, ".", []string{"-count=1"}, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name, n := range ran {
		for range n {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	if want := group(all, 3)[1]; !reflect.DeepEqual(got, want) {
		t.Errorf("shard 2/3 ran %v, want its own %v alone", got, want)
	}
}

// A shard whose run is not its list fails, whichever way the two came apart.
func TestAShardThatRunsOtherThanItsListFails(t *testing.T) {
	dir := fixture(t, "shardpkg")
	for _, c := range []struct {
		run  []string
		want []string
		says string
	}{
		{[]string{"TestA"}, []string{"TestA", "TestAB"}, "missing [TestAB]"},
		{[]string{"TestA", "TestAB"}, []string{"TestA"}, "extra [TestAB]"},
	} {
		if _, _, err := runShard(dir, ".", []string{"-count=1"}, pattern(c.run), c.want); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("ran %v as a shard of %v: %v", c.run, c.want, err)
		}
	}
}

func TestAFailingTestFailsItsShard(t *testing.T) {
	t.Setenv("TESTSEL_FAIL", "1")
	var out bytes.Buffer
	ran, err := shards(&out, fixture(t, "shardpkg"), ".", []string{"-count=1"}, 3, 0)
	if err == nil {
		t.Fatalf("a failing test passed its shard:\n%s", out.String())
	}
	if ran["TestFails"] != 1 || !strings.Contains(out.String(), "asked to fail") {
		t.Errorf("the failure is not shown: ran %v\n%s", ran, out.String())
	}
}

func TestShardsRefuseNoTestsAndAChosenRun(t *testing.T) {
	if _, err := shards(io.Discard, fixture(t, "racegraph"), "./a", nil, 2, 0); err == nil || !strings.Contains(err.Error(), "lists no tests") {
		t.Errorf("a package with no tests: %v", err)
	}
	for _, f := range []string{"-run=TestA", "-skip", "--test.run=x", "-test.skip=TestA"} {
		if _, err := shards(io.Discard, fixture(t, "shardpkg"), ".", []string{f}, 2, 0); err == nil || !strings.Contains(err.Error(), "the shards choose the tests") {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestAffectedFollowsTheTestGraph(t *testing.T) {
	m := "example.com/m/"
	pkgs := []pkg{ // go list's order is no order: the result is sorted all the same
		{ImportPath: m + "d", Dir: "d", Match: []string{"./..."}, GoFiles: []string{"d.go"}, TestGoFiles: []string{"d_test.go"}},
		{ImportPath: m + "d.test", Deps: []string{m + "d [" + m + "d.test]"}},
		{ImportPath: m + "a", Dir: "a", Match: []string{"./..."}, GoFiles: []string{"a.go"}, TestGoFiles: []string{"a_test.go"}},
		{ImportPath: m + "a [" + m + "a.test]", Dir: "a", Match: []string{"./..."}, ForTest: m + "a"},
		{ImportPath: m + "a.test", Deps: []string{m + "a [" + m + "a.test]"}},
		// b's tests alone import a: b itself does not
		{ImportPath: m + "b", Dir: "b", Match: []string{"./..."}, GoFiles: []string{"b.go"}, XTestGoFiles: []string{"b_test.go"}},
		{ImportPath: m + "b.test", Deps: []string{m + "a", m + "b", m + "b_test [" + m + "b.test]"}},
		{ImportPath: m + "c", Dir: "c", Match: []string{"./..."}, GoFiles: []string{"c.go"}, CgoFiles: []string{"c_cgo.go"}, Deps: []string{m + "b"}}, // no tests
		// h's test binary holds a only recompiled for it ("a [h.test]"): h's tests import a package that imports a
		{ImportPath: m + "h", Dir: "h", Match: []string{"./..."}, TestGoFiles: []string{"h_test.go"}},
		{ImportPath: m + "h.test", Deps: []string{m + "a [" + m + "h.test]", m + "h [" + m + "h.test]"}},
		// e's tests read d's Go source as data (readers below), as deploy/k8s's read the server's
		{ImportPath: m + "e", Dir: "e", Match: []string{"./..."}, TestGoFiles: []string{"e_test.go"}},
		{ImportPath: m + "e.test", Deps: []string{m + "e [" + m + "e.test]"}},
		{ImportPath: "fmt", Dir: "../go/src/fmt", GoFiles: []string{"print.go"}}, // a dependency outside the module
	}
	readers := map[string][]string{"e": {"d"}}
	all := []string{m + "a", m + "b", m + "c", m + "d", m + "e", m + "h"}
	gone := func(f string) bool { return f != "d/old.go" && f != "b/old_test.go" }
	for _, c := range []struct {
		files []string
		want  []string
		why   string
	}{
		{nil, nil, ""},
		{[]string{"a/a.go"}, []string{m + "a", m + "b", m + "h"}, ""},
		{[]string{"a/a_test.go"}, []string{m + "a"}, ""}, // a test file builds into its own package's test binary alone
		{[]string{"b/b_test.go"}, []string{m + "b"}, ""},
		{[]string{"b/b.go"}, []string{m + "b", m + "c"}, ""},
		{[]string{"c/c_cgo.go"}, []string{m + "c"}, ""},
		{[]string{"d/d.go"}, []string{m + "d", m + "e"}, ""},
		{[]string{"d/d_test.go"}, []string{m + "d", m + "e"}, ""},
		{[]string{"d/old.go"}, []string{m + "d", m + "e"}, ""}, // deleted: its directory's package
		{[]string{"b/old_test.go"}, []string{m + "b"}, ""},
		{[]string{"d/d.go", "c/c.go"}, []string{m + "c", m + "d", m + "e"}, ""},
		{[]string{"d/d.go", "d/testdata/golden.txt"}, all, "d/testdata/golden.txt changed and is not Go source"},
		{[]string{"design/PERMISSIONS.md"}, all, "not Go source"},
		{[]string{"go.mod"}, all, "not Go source"},
		{[]string{"db/migrations/0002_x.sql"}, all, "not Go source"},
		{[]string{"d/testdata/app/main.go"}, all, "no listed package builds"},
		{[]string{"a/tagged.go"}, all, "no listed package builds"}, // on disk, yet in no package's list
	} {
		got, why := affected(pkgs, c.files, gone, readers)
		if !reflect.DeepEqual(got, c.want) || c.why == "" && why != "" || !strings.Contains(why, c.why) {
			t.Errorf("%v: %v (%q), want %v (%q)", c.files, got, why, c.want, c.why)
		}
	}
}

// racegraph's b imports a from a test file built under -race alone: the graph must be the run's.
func TestTheGraphIsTheRunsBuildFlags(t *testing.T) {
	dir := fixture(t, "racegraph")
	for _, c := range []struct {
		flags []string
		want  []string
	}{
		{[]string{"-race"}, []string{"example.com/racegraph/a", "example.com/racegraph/b"}},
		{nil, []string{"example.com/racegraph/a"}},
	} {
		pkgs, err := loadPackages(dir, c.flags)
		if err != nil {
			t.Fatal(err)
		}
		if got, why := affected(pkgs, []string{"a/a.go"}, func(string) bool { return true }, nil); !reflect.DeepEqual(got, c.want) || why != "" {
			t.Errorf("%v: a/a.go changed: %v (%q), want %v", c.flags, got, why, c.want)
		}
	}
}

func TestEveryDeclaredReaderAndSourceExists(t *testing.T) {
	for reader, sources := range readers {
		for _, dir := range append([]string{reader}, sources...) {
			if fi, err := os.Stat(filepath.Join("..", "..", "..", dir)); err != nil || !fi.IsDir() {
				t.Errorf("readers names %s, which is no directory of this repository: %v", dir, err)
			}
		}
	}
}

// changedFiles over a repository whose main moved on after the branch: the merge base's diff, a
// rename as both its names, a deletion, a staged and an untracked file, never an ignored one.
func TestChangedFilesAreTheBranchsAndTheWorkingTrees(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // the test's own git steps
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	for _, f := range []string{".gitignore", "keep.go", "gone.go", "old/moved.go", "same.go"} {
		write(f)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("switch", "-q", "-c", "branch")
	if err := os.Mkdir(filepath.Join(dir, "new"), 0o750); err != nil {
		t.Fatal(err)
	}
	git("mv", "old/moved.go", "new/moved.go")
	git("rm", "-q", "gone.go")
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "branch")
	git("switch", "-q", "main")
	write("mainonly.go")
	git("add", "mainonly.go")
	git("commit", "-q", "-m", "main moved on")
	git("switch", "-q", "branch")
	write("staged.go")
	git("add", "staged.go")
	write("sub/untracked.go")
	write("ignored.go")
	files, err := changedFiles(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if want := []string{"gone.go", "keep.go", "new/moved.go", "old/moved.go", "staged.go", "sub/untracked.go"}; !reflect.DeepEqual(files, want) {
		t.Errorf("changed %v, want %v", files, want)
	}
}

// make test-affected itself, in a worktree of HEAD where nothing changed: the Makefile's own argv
// reaches testsel and Go's graph under the run's flags, and nothing runs.
func TestMakeTestAffectedRunsAsTheMakefileCallsIt(t *testing.T) {
	top, err := run("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "wt")
	if _, err := run(strings.TrimSpace(top), "git", "worktree", "add", "-q", "--detach", dir, "HEAD"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = run(strings.TrimSpace(top), "git", "worktree", "remove", "--force", dir) })
	cmd := exec.Command("make", "--no-print-directory", "test-affected", "BASE=HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "no package affected" {
		t.Errorf("make test-affected BASE=HEAD with nothing changed: %v\n%s", err, out)
	}
}
