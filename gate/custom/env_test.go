package custom

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// facts is an app with every fact read: one Go dependency and two npm ones, a few files, a database,
// a role and a secret.
func facts() Facts {
	return Facts{
		Languages: []string{"TypeScript"},
		Files:     []string{"README.md", "src/index.ts", "migrations/001.sql"},
		Dependencies: []Dependency{{Name: "lodash", Version: "4.17.4", Ecosystem: "npm", Direct: true},
			{Name: "express", Version: "4.21.2", Ecosystem: "npm", Direct: true}, {Name: "github.com/lib/pq", Version: "v1.10.9", Ecosystem: "go"}},
		Services: []Service{{Name: "db", Type: "postgres"}},
		Roles:    []string{"admin"},
		Secrets:  []Secret{{Name: "STRIPE_SECRET_KEY", Env: "prod", Where: "runtime"}},
		Missing:  map[string]string{},
	}
}

func verdict(t *testing.T, expr string, f Facts) Verdict {
	t.Helper()
	p, err := Compile(expr)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return p.Evaluate(context.Background(), f)
}

// The environment is CEL's standard library, its strings extension and versionAtLeast, and nothing
// else: none of them reads a file, the network or the clock. A function added to it fails here until it
// is read and listed.
func TestTheEnvironmentHasOnlyItsListedFunctions(t *testing.T) {
	e, err := env()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range e.Functions() {
		got = append(got, f.Name())
	}
	slices.Sort(got)
	want := []string{"!_", "-_", "@in", "@not_strictly_false", "_!=_", "_%_", "_&&_", "_*_", "_+_", "_-_", "_/_", "_<=_", "_<_", "_==_", "_>=_", "_>_", "_?_:_", "_[_]",
		"__not_strictly_false__", "_in_", "_||_", "bool", "bytes", "charAt", "contains", "double", "duration", "dyn", "endsWith", "format", "getDate", "getDayOfMonth",
		"getDayOfWeek", "getDayOfYear", "getFullYear", "getHours", "getMilliseconds", "getMinutes", "getMonth", "getSeconds", "in", "indexOf", "int", "join",
		"lastIndexOf", "lowerAscii", "matches", "replace", "reverse", "size", "split", "startsWith", "string", "strings.quote", "substring", "timestamp", "trim",
		"type", "uint", "upperAscii", "versionAtLeast"}
	if !slices.Equal(got, want) {
		t.Fatalf("the functions:\n got %q\nwant %q", got, want)
	}
}

// One pass over the files with a string operation or two on each is accepted, as one the first live
// run refused was (#2637); a comprehension inside another over the files never is.
func TestCompileAcceptsABoolOverTheFacts(t *testing.T) {
	for _, expr := range []string{
		`files.all(f, (f.lowerAscii().endsWith('.html') || f.lowerAscii().endsWith('.htm')) ? f.startsWith('public/') : true)`,
		`!dependencies.exists(d, d.name == 'lodash')`,
		`'README.md' in files && services.all(s, s.type != 'mysql')`,
		`files.exists(f, f.matches('^src/.*\\.ts$'))`,
		`secrets.all(s, s.env != 'prod' || secrets.exists(p, p.name == s.name))`,
	} {
		if _, err := Compile(expr); err != nil {
			t.Errorf("%s: %v", expr, err)
		}
	}
}

// Anything but a bool over the facts, inside every bound, is refused.
func TestCompileRefusesWhatIsOutsideTheEnvironmentOrItsBounds(t *testing.T) {
	for expr, why := range map[string]string{
		`size(files)`:    "returns int, not bool",
		`env.HOME == ''`: "does not compile",
		`dependencies.exists(d, d.license == 'MIT')`:                               "does not compile",
		`exec('cat /etc/passwd') == ''`:                                            "does not compile",
		`files.exists(f, f.matches('('))`:                                          "does not compile",
		`files.all(f, files.exists(g, g == f))`:                                    "its cost could reach",
		`dependencies.exists(d, files.exists(f, f.contains(d.name)))`:              "its cost could reach",
		`files.exists(f, f == '` + strings.Repeat("a", maxExpr) + `')`:             "does not compile",
		strings.Repeat("(", maxDepth+1) + "true" + strings.Repeat(")", maxDepth+1): "does not compile",
	} {
		_, err := Compile(expr)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%.80s: %v, want %q", expr, err, why)
		}
	}
	if _, err := Compile(`files.all(f, files.exists(g, g == f))`); !errors.Is(err, ErrBounds) {
		t.Errorf("a comprehension inside one over the files: %v", err)
	}
}

// Nothing that loops over the files inside a loop over them fits maxCost, whatever it does in the loop.
func TestTheCostBoundIsUnderTheFilesSquared(t *testing.T) {
	if n := bounds["files"].items; maxCost >= n*n {
		t.Fatalf("maxCost %d is not under %d files squared", maxCost, n)
	}
}

func TestEvaluateIsTheExpressionsVerdict(t *testing.T) {
	if v := verdict(t, `!dependencies.exists(d, d.name == 'lodash')`, facts()); v.Result != Fail {
		t.Errorf("lodash is there: %+v", v)
	}
	if v := verdict(t, `!dependencies.exists(d, d.name == 'moment')`, facts()); v.Result != Pass {
		t.Errorf("moment is not: %+v", v)
	}
}

// A missing fact is never a pass, not even where CEL would let the rest of the expression decide:
// an || absorbs an error, so the expression is unchecked before it is evaluated.
func TestAMissingFactIsUncheckedEvenWhereTheRestWouldPass(t *testing.T) {
	f := facts()
	f.Missing["dependencies"] = "Canyon does not read dependencies from requirements.txt"
	v := verdict(t, `!dependencies.exists(d, d.name == 'django') || 'README.md' in files`, f)
	if v.Result != Unchecked || !strings.Contains(v.Why, "requirements.txt") {
		t.Fatalf("got %+v", v)
	}
	f.Missing = map[string]string{"secrets": "not given"}
	if v := verdict(t, `files.size() > 0 || secrets.exists(s, s.name == 'X')`, f); v.Result != Unchecked {
		t.Fatalf("a missing fact on the right of an ||: %+v", v)
	}
}

// Versions compare as versions: 4.17.4 is before 4.17.21, which as strings it is not. A version that is
// not semantic is unchecked, never a pass.
func TestVersionAtLeastComparesVersions(t *testing.T) {
	for expr, want := range map[string]string{
		`versionAtLeast('4.17.21', '4.17.4')`:                                             Pass,
		`versionAtLeast('4.17.4', '4.17.21')`:                                             Fail,
		`versionAtLeast('v1.10.9', 'v1.9.0')`:                                             Pass,
		`versionAtLeast('18.3.1', '18')`:                                                  Pass,
		`versionAtLeast('latest', '1.0.0')`:                                               Unchecked,
		`dependencies.all(d, d.name != 'lodash' || versionAtLeast(d.version, '4.17.21'))`: Fail,
	} {
		if v := verdict(t, expr, facts()); v.Result != want {
			t.Errorf("%s: %+v, want %s", expr, v, want)
		}
	}
}

// A function whose result CEL's estimate does not bound is refused: format has none at all, quote
// doubles its input with every call around it, and join, replace and split cost more than estimated
// (#2638 review).
func TestFunctionsThatCanOutgrowTheirEstimateAreRefused(t *testing.T) {
	for _, expr := range []string{
		`files.map(f, '%s'.format([files])).size() > 0`,
		`strings.quote(strings.quote(strings.quote(files[0]))).size() > 0`,
		`files.exists(f, f.indexOf('#') > 0)`,
		`files.exists(f, f.lastIndexOf('#') > 0)`,
		`files.exists(f, f.charAt(1) == '#')`,
		`files.join('').replace('a', 'aa').size() > 0`,
		`files.join(',').size() > 0`,
		`files.exists(f, f.split('/').size() > 9)`,
	} {
		if _, err := Compile(expr); err == nil || !strings.Contains(err.Error(), "its cost is not bounded") {
			t.Errorf("%s: %v", expr, err)
		}
	}
}

// An evaluation stops at its deadline, or its context's, part way through, and is unchecked: never a
// pass.
func TestAnEvaluationThatIsStoppedIsUnchecked(t *testing.T) {
	p, err := Compile(`files.exists_one(f, f.lowerAscii().endsWith('#'))`)
	if err != nil {
		t.Fatal(err)
	}
	f := AtTheBounds()
	f.Files = f.Files[:5000]
	start := time.Now()
	if v := p.Evaluate(context.Background(), f); v.Stopped {
		t.Fatalf("%+v", v)
	}
	whole := time.Since(start)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	start = time.Now()
	v := p.Evaluate(ctx, f)
	if took := time.Since(start); v.Result != Unchecked || !v.Stopped || took > whole/4 {
		t.Fatalf("%+v after %v, the whole evaluation %v", v, took, whole)
	}
}
