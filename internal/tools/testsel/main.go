// Command testsel picks what a test run needs (Makefile: test-affected, test-shards).
//
//	testsel affected [-base origin/main] [-- build flags]   the import paths a change since base's merge base can affect
//	testsel shards [-n N | -shard i/n] PKG [go test flags]  PKG's tests in n go test processes, each test run exactly once
//
// Neither guesses. affected reads Go's own graph (go list -deps -test -json, under the run's own
// build flags); any change the graph cannot place falls back to every package. shards runs each
// shard under go test -json and fails unless the tests it ran are exactly the shard's.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testsel affected [-base REF] [-- build flags] | testsel shards [-n N | -shard i/n] PKG [go test flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "affected":
		err = affectedCmd(os.Args[2:])
	case "shards":
		err = shardsCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testsel:", err)
		os.Exit(1)
	}
}

// readers are the packages whose tests read another directory's Go source as data, each with
// those directories, relative to the repository: no import graph shows such a read. A test that
// starts reading another directory's source is added here (AGENTS.md, Conventions).
var readers = map[string][]string{
	"deploy/k8s":            {"cmd/canyon-server", "cmd/canyon-executor", "executor", "cmd/canyon-executor-kube", "executor/kube"},              // config_docs_test.go
	"cmd/canyon-server":     {"executor"},                                                                                                       // routes_secrets_reserved_test.go reads executor.go's reserved names
	"internal/tools/suites": {"e2e", "e2e/handoff", "e2e/oidc", "executor", "executor/kube", "cmd/canyon-executor-kube", "cmd/canyon-identity"}, // suites_test.go lists what these compile
}

// pkg is the part of go list -json's Package that affected reads.
type pkg struct {
	ImportPath, Dir, ForTest                     string
	Match                                        []string
	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles []string
	Deps                                         []string
}

func affectedCmd(args []string) error {
	fs := flag.NewFlagSet("affected", flag.ContinueOnError)
	base := fs.String("base", "origin/main", "the ref whose merge base with HEAD the change is measured from")
	if err := fs.Parse(args); err != nil {
		return err
	}
	top, err := run("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	top = strings.TrimSpace(top)
	files, err := changedFiles(top, *base)
	if err != nil {
		return err
	}
	pkgs, err := loadPackages(top, fs.Args())
	if err != nil {
		return err
	}
	picked, why := affected(pkgs, files, func(f string) bool { _, err := os.Lstat(filepath.Join(top, f)); return err == nil }, readers)
	if why != "" {
		fmt.Fprintf(os.Stderr, "testsel: every package: %s\n", why)
	}
	for _, p := range picked {
		fmt.Println(p)
	}
	return nil
}

// loadPackages is go list -deps -test -json ./... in dir under flags, the build flags the tests
// run with (a file or an import behind //go:build race exists only under -race), each package of
// ./... with its Dir relative to dir.
func loadPackages(dir string, flags []string) ([]pkg, error) {
	out, err := run(dir, "go", append(append([]string{"list", "-deps", "-test", "-json"}, flags...), "./...")...)
	if err != nil {
		return nil, err
	}
	var pkgs []pkg
	for dec := json.NewDecoder(strings.NewReader(out)); ; {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, err
		}
		if len(p.Match) > 0 { // ./...'s own; the standard library and other modules have no file a change touches
			if p.Dir, err = filepath.Rel(dir, p.Dir); err != nil || strings.HasPrefix(p.Dir, "..") {
				return nil, fmt.Errorf("package %s is outside %s", p.ImportPath, dir)
			}
		}
		pkgs = append(pkgs, p)
	}
}

// changedFiles is every path, relative to top, that differs between the working tree and base's
// merge base with HEAD: committed, staged, unstaged, deleted, renamed (both names) and untracked.
func changedFiles(top, base string) ([]string, error) {
	mb, err := run(top, "git", "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	diff, err := run(top, "git", "diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(mb))
	if err != nil {
		return nil, err
	}
	untracked, err := run(top, "git", "ls-files", "--others", "--exclude-standard", "--full-name", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(diff+untracked, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// affected is every package of pkgs (loadPackages) whose test binary builds a changed Go file, or
// that reads a changed file's directory as source (readers), sorted, with "" for why. A change the
// graph cannot place returns every package and the reason: a file that is not Go source (go.mod,
// go.sum, the Makefile, migrations, testdata, goldens, docs, contracts — tests open such files
// across packages, so no graph knows their readers), or Go source no listed package builds under
// the run's flags. exists reports whether a path is in the working tree: a deleted Go file belongs
// to the package of its directory.
func affected(pkgs []pkg, files []string, exists func(string) bool, readers map[string][]string) ([]string, string) {
	var roots []string
	owner, dirs, byPath := map[string]string{}, map[string]string{}, map[string]pkg{}
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
		if len(p.Match) == 0 || p.ForTest != "" {
			continue
		}
		roots = append(roots, p.ImportPath)
		dirs[p.Dir] = p.ImportPath
		for _, list := range [][]string{p.GoFiles, p.CgoFiles, p.TestGoFiles, p.XTestGoFiles} {
			for _, f := range list {
				owner[filepath.ToSlash(filepath.Join(p.Dir, f))] = p.ImportPath
			}
		}
	}
	sort.Strings(roots)
	changed, tests, read := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			return roots, f + " changed and is not Go source: a test may open it from any package"
		}
		dir := filepath.Dir(filepath.FromSlash(f))
		ip, ok := owner[f]
		if !ok && !exists(f) {
			ip, ok = dirs[dir]
		}
		if !ok {
			return roots, f + " is Go source no listed package builds (a build tag, a testdata app): a test may open it"
		}
		if strings.HasSuffix(f, "_test.go") {
			tests[ip] = true // a test file builds into its own package's test binary and no other
		} else {
			changed[ip] = true
		}
		for reader, sources := range readers {
			if slices.Contains(sources, filepath.ToSlash(dir)) {
				read[filepath.FromSlash(reader)] = true
			}
		}
	}
	var picked []string
	for _, r := range roots {
		deps := byPath[r].Deps // a package without tests builds only itself and its imports
		if t, ok := byPath[r+".test"]; ok {
			deps = t.Deps // the test binary: the package as its tests compile it, its external tests, and every import of both
		}
		hit := changed[r] || tests[r] || read[byPath[r].Dir]
		for _, d := range deps {
			ip, _, _ := strings.Cut(d, " ") // "q [p.test]" is q recompiled for p's tests
			hit = hit || changed[ip]
		}
		if hit {
			picked = append(picked, r)
		}
	}
	return picked, ""
}

func shardsCmd(args []string) error {
	fs := flag.NewFlagSet("shards", flag.ContinueOnError)
	n := fs.Int("n", 2, "the number of shards, all run at once")
	shard := fs.String("shard", "", "i/n: the i-th of n shards alone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	only := 0
	if *shard != "" {
		if _, err := fmt.Sscanf(*shard, "%d/%d", &only, n); err != nil || fmt.Sprintf("%d/%d", only, *n) != *shard || only < 1 {
			return fmt.Errorf("-shard %q: want i/n, 1 <= i <= n", *shard)
		}
	}
	if fs.NArg() < 1 || *n < 1 || only > *n {
		return fmt.Errorf("want -n >= 1 or -shard i/n with 1 <= i <= n, and a package")
	}
	_, err := shards(os.Stdout, "", fs.Arg(0), fs.Args()[1:], *n, only)
	return err
}

// shards runs target's tests from dir in n shards at once, or shard only (1..n) alone, and returns
// how many times each top-level test ran. Before a shard runs, every test the package lists must
// match exactly one shard's -run pattern; after, the tests go test -json reports run must be
// exactly the shard's, or it fails.
func shards(w io.Writer, dir, target string, flags []string, n, only int) (map[string]int, error) {
	for _, f := range flags {
		name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if strings.HasPrefix(f, "-") && slices.Contains([]string{"run", "skip", "test.run", "test.skip"}, name) {
			return nil, fmt.Errorf("%s: the shards choose the tests", f)
		}
	}
	names, err := listTests(dir, target, flags)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s lists no tests", target)
	}
	groups := group(names, n)
	patterns := make([]string, n)
	for i, g := range groups {
		patterns[i] = pattern(g)
	}
	if err := check(names, patterns); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(w, "testsel: %d tests of %s in %d shards, each in exactly one\n", len(names), target, n)
	ran := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failed []string
	for i := range n {
		if only > 0 && i != only-1 {
			continue
		}
		wg.Go(func() {
			got, out, err := runShard(dir, target, flags, patterns[i], groups[i])
			mu.Lock()
			defer mu.Unlock()
			_, _ = fmt.Fprintf(w, "testsel: shard %d/%d, %d tests\n%s", i+1, n, len(groups[i]), out)
			for _, name := range got {
				ran[name]++
			}
			if err != nil {
				failed = append(failed, fmt.Sprint(i+1))
				_, _ = fmt.Fprintf(w, "testsel: shard %d/%d failed: %v\n", i+1, n, err)
			}
		})
	}
	wg.Wait()
	if len(failed) > 0 {
		return ran, fmt.Errorf("shards %s of %d failed", strings.Join(failed, ", "), n)
	}
	return ran, nil
}

// listTests is every test, example and fuzz target of target that go test runs — its benchmarks
// run only under -bench — as its test binary lists them, built with the run's own flags. The
// binary lists with no database: its TestMain would otherwise set one up just to print names.
func listTests(dir, target string, flags []string) ([]string, error) {
	pkgDir, err := run(dir, "go", "list", "-f", "{{.Dir}}", target)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "testsel")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bin := filepath.Join(tmp, "pkg.test")
	if _, err := run(dir, "go", append(append([]string{"test", "-c", "-o", bin}, flags...), target)...); err != nil {
		return nil, err
	}
	if _, err := os.Stat(bin); errors.Is(err, os.ErrNotExist) {
		return nil, nil // no test files: go test -c writes no binary
	}
	cmd := exec.Command(bin, "-test.list", ".") //nolint:gosec // the binary go test -c just built
	cmd.Dir, cmd.Stderr = strings.TrimSpace(pkgDir), os.Stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CANYON_TEST_DATABASE_URL=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s -test.list: %w", target, err)
	}
	var names []string
	for _, name := range strings.Fields(string(out)) {
		switch {
		case strings.HasPrefix(name, "Benchmark"):
		case strings.HasPrefix(name, "Test"), strings.HasPrefix(name, "Example"), strings.HasPrefix(name, "Fuzz"):
			names = append(names, name)
		default:
			return nil, fmt.Errorf("%s -test.list printed %q, which is no test, example, fuzz target or benchmark", target, name)
		}
	}
	return names, nil
}

// group deals names, sorted, into n shards in turn: deterministic, and even in count.
func group(names []string, n int) [][]string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	groups := make([][]string, n)
	for i, name := range sorted {
		groups[i%n] = append(groups[i%n], name)
	}
	return groups
}

// pattern is the -run expression that matches exactly names.
func pattern(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// check is nil when every name is matched by exactly one pattern, as go test's -run matches a
// top-level name: an unanchored regexp.MatchString, the pattern holding no '/' to split on.
func check(names, patterns []string) error {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		if strings.Contains(p, "/") {
			return fmt.Errorf("shard %d's pattern holds a '/', which -run splits on", i+1)
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("shard %d: %w", i+1, err)
		}
		res[i] = re
	}
	var bad []string
	for _, name := range names {
		var in []string
		for i, re := range res {
			if re.MatchString(name) {
				in = append(in, fmt.Sprint(i+1))
			}
		}
		if len(in) != 1 {
			bad = append(bad, fmt.Sprintf("%s in %d shards [%s]", name, len(in), strings.Join(in, " ")))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("every test must run exactly once:\n  %s", strings.Join(bad, "\n  "))
	}
	return nil
}

// event is one line of go test -json (test2json).
type event struct {
	Action, Test, Output string
	Elapsed              float64
}

// runShard runs the shard's tests under go test -json and returns the top-level tests it ran and
// what to show: the package's own output, and the output of each test that failed or never ended.
// It fails when go test does, or when the tests run are not exactly want.
func runShard(dir, target string, flags []string, pattern string, want []string) ([]string, string, error) {
	if len(want) == 0 {
		return nil, "no tests in this shard\n", nil
	}
	var raw bytes.Buffer
	cmd := exec.Command("go", append(append([]string{"test", "-json", "-run", pattern}, flags...), target)...) //nolint:gosec // the Makefile's own package and flags
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, &raw, &raw
	runErr := cmd.Run()
	var ran, order []string
	outputs, ended := map[string]*strings.Builder{}, map[string]bool{}
	var failed []string
	sc := bufio.NewScanner(&raw)
	sc.Buffer(nil, 64<<20) // a test's log line can be long
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			e = event{Action: "output", Output: sc.Text() + "\n"} // the go command's own, a build error
		}
		if outputs[e.Test] == nil {
			outputs[e.Test] = &strings.Builder{}
			order = append(order, e.Test)
		}
		switch e.Action {
		case "run":
			if !strings.Contains(e.Test, "/") {
				ran = append(ran, e.Test)
			}
		case "output":
			_, _ = outputs[e.Test].WriteString(e.Output)
		case "fail":
			failed = append(failed, e.Test)
			ended[e.Test] = true
		case "pass", "skip":
			ended[e.Test] = true
			if e.Test != "" && !strings.Contains(e.Test, "/") {
				fmt.Printf("TEMPORARY elapsed %.2f %s\n", e.Elapsed, e.Test)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, raw.String(), err
	}
	var show strings.Builder
	for _, t := range order {
		if t != "" && (slices.Contains(failed, t) || !ended[t]) {
			show.WriteString(outputs[t].String())
		}
	}
	show.WriteString(outputs[""].String())
	sort.Strings(ran)
	if runErr != nil {
		return ran, show.String(), runErr
	}
	if !slices.Equal(ran, want) { // want is sorted: group deals sorted names
		var missing, extra []string
		for _, t := range want {
			if !slices.Contains(ran, t) {
				missing = append(missing, t)
			}
		}
		for _, t := range ran {
			if !slices.Contains(want, t) {
				extra = append(extra, t)
			}
		}
		return ran, show.String(), fmt.Errorf("ran %d tests, want the shard's %d: missing %v, extra %v", len(ran), len(want), missing, extra)
	}
	return ran, show.String(), nil
}

// run is a command's stdout; its stderr goes to ours.
func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // go and git, with this tool's own arguments
	cmd.Dir, cmd.Stderr = dir, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
