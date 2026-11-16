package custom

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
)

// The bounds every expression is held to (#2637). At compile: an expression longer than maxExpr code
// points, nested deeper than maxDepth or with more than maxNodes nodes does not compile; one calling a
// function in unbounded is refused; and one whose cost CEL estimates, with every fact at its largest
// (bounds), above maxCost is refused. At evaluation: a fact past its bounds is missing, CEL's tracker
// stops an evaluation at maxCost, and EvalDeadline stops it as well. maxCost is about MaxAlloc in CEL's
// cost units (0.1 a byte of string), under the files' bound squared, the least a comprehension inside
// another over the files costs, and over one pass over them with a string operation or two on each.
const (
	maxExpr      = 4096
	maxDepth     = 32
	maxNodes     = 2000
	maxCost      = 25_000_000
	EvalDeadline = 2 * time.Second
)

// MaxAlloc is the most an evaluation at the bounds (AtTheBounds) may allocate: ruleeval counts an
// accepted expression over it unsafe, and every shape of expression is tested against it. Go counts
// allocation for the whole process, so on a busy host an admission counts other work's too and can
// refuse what a quiet host admits: toward the AI review, never toward a pass.
const MaxAlloc = 256 << 20

// unbounded are the functions whose cost CEL's estimate does not bound, which Canyon refuses: format
// and type have no estimate; quote doubles its input with every call around it; join, replace and split
// cost far more than CEL estimates (files.join("") with six replaces, estimated at 22.6M, passed 31.6M
// before the tracker stopped it); and charAt, indexOf and lastIndexOf turn their whole string into
// runes on every call (22 of them on each file at the bound allocated 0.9 to 1.8 GiB) (#2638 review).
var unbounded = []string{"charAt", "format", "indexOf", "join", "lastIndexOf", "replace", "split", "strings.quote", "type"}

// bounds is the most each fact holds, and each of its strings, in code points: CEL's cost estimate reads
// them, so a comprehension over a fact costs its largest size, and a fact read past them is missing.
// 10,000 files of 512 characters each is past nearly every app, and a string operation's estimate is its
// string's length, so the two set what one pass over the files may do. cel-go v0.31's cost tracker
// spends its time in a linear scan per step, so one pass over the files takes the square of their
// number: 137 ms at 10,000, 521 ms at 20,000 (#2639), which put honest one-pass rules at the edge of
// admission's deadline.
var bounds = map[string]struct{ items, chars int }{
	"languages":    {64, 64},
	"files":        {10_000, 512},
	"dependencies": {10_000, 256},
	"services":     {64, 64},
	"roles":        {64, 64},
	"secrets":      {500, 256},
}

// docs is each fact's description, as the translator's prompt reads it.
var docs = map[string]string{
	"languages": "Every programming language of the app's files outside .git/, node_modules/ and vendor/, as GitHub Linguist names it, " +
		"each once: for example JavaScript, TypeScript, Python, Go, Java, Kotlin, Ruby, PHP, C#, Rust, Elixir, Dart. A .jsx file is " +
		"JavaScript and a .tsx file TypeScript. Markup, data and prose are never in it, nor are " + strings.Join(tooling, ", ") +
		": the languages of scripts, builds and infrastructure, not of the app.",
	"files": "Every file path in the app's package, relative to its root, '/'-separated, no leading './' (for example 'src/index.ts', " +
		"'README.md', '.env'). Files only: a directory is the prefix of the paths under it ('migrations/' holds 'migrations/001.sql').",
	"dependencies": "Every package the app depends on, direct and transitive, from the lockfiles that name them all: npm from " +
		"package-lock.json, yarn.lock, pnpm-lock.yaml or bun.lock; pypi from uv.lock, poetry.lock or Pipfile.lock; go from go.mod; " +
		"maven from gradle.lockfile; gem from Gemfile.lock; cargo from Cargo.lock; composer from composer.lock; nuget from " +
		"packages.lock.json; hex from mix.lock; pub from pubspec.lock; swift from Package.resolved; cocoapods from Podfile.lock; " +
		"conan from conan.lock. Fields: name (as its ecosystem names it: 'express', '@types/node', 'github.com/lib/pq', " +
		"'com.fasterxml.jackson.core:jackson-databind'; a pypi name is normalised as PEP 503 says, lowercase with '-' for every run of " +
		"'-', '_' and '.', so PyYAML is 'pyyaml' and Flask_Login 'flask-login'), version (the exact version locked: '4.21.2', " +
		"'v1.10.9'), ecosystem (one of those named above), direct (true when the app asks for it itself, false when only another " +
		"package does; some lockfiles do not say, and then a rule that reads direct cannot be checked). When Canyon cannot read " +
		"every package the app depends on, the whole fact is unavailable and the rule cannot be checked.",
	"services": "The resources the app's canyon.toml declares under [resources], each a name and the type the host provides it as: " +
		"for example {name: 'db', type: 'postgres'}, {name: 'files', type: 's3'}. Empty when the app declares none.",
	"roles": "The names of the app roles the app's canyon.toml declares under [roles] (for example 'admin', 'expense-approver'). " +
		"Empty when it declares none.",
	"secrets": "The app's secrets as set in Canyon, by name only, never a value: name ('STRIPE_SECRET_KEY'), env ('preview' or " +
		"'prod': each environment has its own), where ('runtime', read by the running app, or 'build', read while its image builds).",
}

// Dependency is one package an app depends on, as its lockfile names it.
type Dependency struct {
	Name      string `cel:"name" json:"name"`
	Version   string `cel:"version" json:"version"`
	Ecosystem string `cel:"ecosystem" json:"ecosystem"`
	Direct    bool   `cel:"direct" json:"direct"`
}

// Service is one resource an app's canyon.toml declares.
type Service struct {
	Name string `cel:"name" json:"name"`
	Type string `cel:"type" json:"type"`
}

// Secret is one secret set for an app, by name: its value never reaches a fact.
type Secret struct {
	Name  string `cel:"name" json:"name"`
	Env   string `cel:"env" json:"env"`
	Where string `cel:"where" json:"where"`
}

// factTypes is each fact's CEL type.
var factTypes = map[string]*cel.Type{
	"languages":    cel.ListType(cel.StringType),
	"files":        cel.ListType(cel.StringType),
	"dependencies": cel.ListType(cel.ObjectType("custom.Dependency")),
	"services":     cel.ListType(cel.ObjectType("custom.Service")),
	"roles":        cel.ListType(cel.StringType),
	"secrets":      cel.ListType(cel.ObjectType("custom.Secret")),
}

// env is the one CEL environment every expression compiles in: the facts, CEL's standard library, its
// strings extension and versionAtLeast. Nothing in it reads a file, the network or the clock.
var env = sync.OnceValues(func() (*cel.Env, error) {
	opts := []cel.EnvOption{
		ext.NativeTypes(reflect.TypeFor[Dependency](), reflect.TypeFor[Service](), reflect.TypeFor[Secret](), ext.ParseStructTags(true)),
		ext.Strings(),
		cel.ParserExpressionSizeLimit(maxExpr),
		cel.ParserRecursionLimit(maxDepth),
		cel.ExpressionNodeLimit(maxNodes),
		cel.ExtendedValidations(),
		cel.ASTValidators(refuse{}),
		cel.Function("versionAtLeast",
			cel.FunctionDocs("versionAtLeast(version, minimum) is whether version is minimum or later by semantic versioning "+
				"('4.17.21' is at least '4.17.4'; a leading 'v' is allowed). A version that is not semantic is an error, "+
				"and the rule cannot be checked. Compare versions only with it, never as strings."),
			cel.Overload("versionAtLeast_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
				cel.BinaryBinding(versionAtLeast))),
	}
	for _, name := range slices.Sorted(maps.Keys(factTypes)) {
		opts = append(opts, cel.VariableWithDoc(name, factTypes[name], docs[name]))
	}
	return cel.NewEnv(opts...)
})

// versionAtLeast is CEL's versionAtLeast(version, minimum).
func versionAtLeast(version, minimum ref.Val) ref.Val {
	v, err := semver.NewVersion(fmt.Sprint(version.Value()))
	if err != nil {
		return types.NewErr("versionAtLeast: %q is not a semantic version", version.Value())
	}
	m, err := semver.NewVersion(fmt.Sprint(minimum.Value()))
	if err != nil {
		return types.NewErr("versionAtLeast: %q is not a semantic version", minimum.Value())
	}
	return types.Bool(!v.LessThan(m))
}

// refuse is the validator that refuses a call to a function in unbounded.
type refuse struct{}

func (refuse) Name() string { return "canyon.unbounded" }

func (refuse) Validate(_ *cel.Env, _ cel.ValidatorConfig, a *ast.AST, iss *cel.Issues) {
	for _, name := range unbounded {
		for _, call := range ast.MatchDescendants(ast.NavigateAST(a), ast.FunctionMatcher(name)) {
			iss.ReportErrorAtID(call.ID(), "%s is not a function Canyon checks by code: its cost is not bounded", name)
		}
	}
	// a fact's value built in the expression compares every field, direct among them, with no select of it
	for _, s := range ast.MatchDescendants(ast.NavigateAST(a), ast.KindMatcher(ast.StructKind)) {
		if name := s.AsStruct().TypeName(); strings.HasPrefix(name, "custom.") {
			iss.ReportErrorAtID(s.ID(), "a %s built in the expression is not something Canyon checks by code", name)
		}
	}
}

// estimator is CEL's cost estimate with every fact at its bound.
type estimator struct{}

func (estimator) EstimateSize(n checker.AstNode) *checker.SizeEstimate {
	path := n.Path()
	if len(path) == 0 {
		return nil
	}
	b, ok := bounds[path[0]]
	if !ok {
		return nil
	}
	size := b.chars // a list's element, or a field of one
	if len(path) == 1 {
		size = b.items
	}
	return &checker.SizeEstimate{Min: 0, Max: uint64(size)} //nolint:gosec // every bound is a positive constant
}

func (estimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// AtTheBounds is facts at their bounds, every list full and every string its longest: what the cost
// estimate an expression is accepted under assumes, to measure one against.
func AtTheBounds() Facts {
	long := func(n, i int) string { s := fmt.Sprintf("%d/", i); return s + strings.Repeat("a", n-len(s)) }
	list := func(name string) []string {
		out := make([]string, bounds[name].items)
		for i := range out {
			out[i] = long(bounds[name].chars, i)
		}
		return out
	}
	f := Facts{Languages: list("languages"), Files: list("files"), Roles: list("roles"), Missing: map[string]string{}}
	for i, s := range list("dependencies") {
		f.Dependencies = append(f.Dependencies, Dependency{Name: s, Version: s, Ecosystem: s, Direct: i%2 == 0})
	}
	for _, s := range list("services") {
		f.Services = append(f.Services, Service{Name: s, Type: s})
	}
	for _, s := range list("secrets") {
		f.Secrets = append(f.Secrets, Secret{Name: s, Env: s, Where: s})
	}
	return f
}

// Program is an expression Canyon accepted: it compiles in env, returns bool and fits every bound.
type Program struct {
	prg      cel.Program
	facts    []string // the facts it reads: one missing, and it cannot be checked
	estimate uint64   // its cost, as CEL estimates it with every fact at its bound
	direct   bool     // it selects a dependency's direct: unknown for one, and it cannot be checked
}

// ErrBounds is an expression that compiles but is outside a bound.
var ErrBounds = errors.New("outside Canyon's bounds")

// Compile is expr as a Program, or why Canyon does not accept it.
func Compile(expr string) (*Program, error) {
	e, err := env()
	if err != nil {
		return nil, err
	}
	checked, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("does not compile: %w", iss.Err())
	}
	if !checked.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("returns %s, not bool", checked.OutputType())
	}
	cost, err := e.EstimateCost(checked, estimator{})
	if err != nil {
		return nil, fmt.Errorf("no cost estimate: %w", err)
	}
	if cost.Max > maxCost {
		return nil, fmt.Errorf("%w: its cost could reach %d, over %d", ErrBounds, cost.Max, maxCost)
	}
	prg, err := e.Program(checked, cel.CostLimit(maxCost), cel.InterruptCheckFrequency(100), cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		return nil, fmt.Errorf("no program: %w", err)
	}
	var facts []string
	for _, r := range checked.NativeRep().ReferenceMap() {
		if _, ok := factTypes[r.Name]; ok && !slices.Contains(facts, r.Name) {
			facts = append(facts, r.Name)
		}
	}
	slices.Sort(facts)
	// direct is a field of Dependency alone, so a select of it anywhere in the checked tree reads it
	direct := len(ast.MatchDescendants(ast.NavigateAST(checked.NativeRep()), func(e ast.NavigableExpr) bool {
		return e.Kind() == ast.SelectKind && e.AsSelect().FieldName() == "direct"
	})) > 0
	return &Program{prg: prg, facts: facts, estimate: cost.Max, direct: direct}, nil
}

// Verdict is an expression's answer on one app: Pass, Fail or Unchecked, and why when Unchecked;
// Stopped when its evaluation was stopped, at maxCost, at EvalDeadline or by ctx.
type Verdict struct {
	Result  string
	Why     string
	Stopped bool
	Missing string // the missing fact it reads, when that is why it is Unchecked
	// Undirected is a dependency whose direct is unknown, when that is why it is Unchecked
	Undirected bool
}

// The three verdicts. Unchecked is a fact missing or an evaluation that errs or is stopped: it is never
// a pass.
const (
	Pass      = "pass"
	Fail      = "fail"
	Unchecked = "unchecked"
)

// Evaluate is p's verdict on an app's facts, stopped at EvalDeadline.
func (p *Program) Evaluate(ctx context.Context, f Facts) Verdict {
	for _, name := range p.facts {
		if why, missing := f.Missing[name]; missing {
			return Verdict{Result: Unchecked, Why: name + ": " + why, Missing: name}
		}
	}
	if p.direct && f.undirected != "" {
		return Verdict{Result: Unchecked, Why: "dependencies: " + f.undirected, Undirected: true}
	}
	ctx, cancel := context.WithTimeout(ctx, EvalDeadline)
	defer cancel()
	out, _, err := p.prg.ContextEval(ctx, f.activation())
	var cancelled interpreter.EvalCancelledError
	if ctx.Err() != nil || errors.As(err, &cancelled) {
		return Verdict{Result: Unchecked, Why: fmt.Sprintf("stopped: %v", err), Stopped: true}
	}
	if err != nil {
		return Verdict{Result: Unchecked, Why: err.Error()}
	}
	pass, ok := out.Value().(bool)
	if !ok {
		return Verdict{Result: Unchecked, Why: fmt.Sprintf("answered %v, not a bool", out.Value())}
	}
	if pass {
		return Verdict{Result: Pass}
	}
	return Verdict{Result: Fail}
}

// Measure is p's evaluation on f under ctx: the cost CEL's tracker counted, what it allocated, and an
// error when it was stopped, at maxCost or by ctx.
func (p *Program) Measure(ctx context.Context, f Facts) (cost, alloc uint64, err error) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, det, err := p.prg.ContextEval(ctx, f.activation())
	runtime.ReadMemStats(&after)
	if det != nil && det.ActualCost() != nil {
		cost = *det.ActualCost()
	}
	return cost, after.TotalAlloc - before.TotalAlloc, err
}

// read is the facts p reads of f, by name.
func (p *Program) read(f Facts) map[string]any {
	all, out := f.activation(), map[string]any{}
	for _, name := range p.facts {
		out[name] = all[name]
	}
	return out
}

// Estimate is p's cost as CEL estimates it with every fact at its bound.
func (p *Program) Estimate() uint64 { return p.estimate }

// AtTheBounds is why p does not hold at the facts' bounds, "" when it does: evaluated once on
// AtTheBounds with CEL's tracker on, under ctx, it was stopped, cost more than its estimate, or
// allocated more than MaxAlloc. Cost does not predict time: the tracker's time grows with the square of
// a list's length, so a cheap expression can run past the deadline on a large app (#2638 review).
func (p *Program) AtTheBounds(ctx context.Context) string {
	cost, alloc, err := p.Measure(ctx, AtTheBounds()) // built for each, about 6 MB, never kept
	switch {
	case err != nil:
		return fmt.Sprintf("at the facts' bounds was stopped: %v", err)
	case cost > p.estimate:
		return fmt.Sprintf("at the facts' bounds cost %d, over its estimate of %d", cost, p.estimate)
	case alloc > MaxAlloc:
		return fmt.Sprintf("at the facts' bounds allocated %d MiB, over %d", alloc>>20, MaxAlloc>>20)
	}
	return ""
}
