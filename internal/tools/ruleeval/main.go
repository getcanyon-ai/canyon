// Command ruleeval measures how a model translates custom rules into CEL checks (#2637, Phase 0): it
// translates every rule of testdata/corpus.yaml three times, evaluates each check Canyon accepts as
// code on the fixture apps under testdata/apps, and reports per rule and in total against the bar
// Phase 1 waits on. Each app's packages are testdata/packages/<app>.json: trivy 0.74.0's report as the
// executor's packages target answers it (contracts/executor.md), recorded once, offline.
//
//	go run ./internal/tools/ruleeval              # live: ANTHROPIC_API_KEY, the model -model names; writes testdata/recorded.json
//	go run ./internal/tools/ruleeval --recorded   # replays testdata/recorded.json: no model, the same report
//
// It exits 1 when the bar is not met.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sync/errgroup"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/custom"
)

// runs is how many times each rule is translated: the bar asks that they agree.
const runs = 3

func main() {
	recorded := flag.Bool("recorded", false, "replay testdata/recorded.json instead of calling the model")
	dir := flag.String("dir", "internal/tools/ruleeval/testdata", "the corpus, the fixture apps and the recording")
	model := flag.String("model", "claude-opus-5", "the model, as Anthropic names it (live only)")
	flag.Parse()
	met, err := run(context.Background(), os.Stdout, *dir, *recorded, *model)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ruleeval:", err)
		os.Exit(2)
	}
	if !met {
		os.Exit(1)
	}
}

// corpus is testdata/corpus.yaml.
type corpus struct {
	Apps map[string]struct {
		Secrets *[]custom.Secret `yaml:"secrets"`
	} `yaml:"apps"`
	Rules []rule `yaml:"rules"`
}

// rule is one rule of the corpus: its expected checker and, where code can decide it, its verdict on
// each app named.
type rule struct {
	ID        string   `yaml:"id"`
	Category  string   `yaml:"category"`
	Text      string   `yaml:"text"`
	Checker   string   `yaml:"checker"`
	Pass      []string `yaml:"pass"`
	Fail      []string `yaml:"fail"`
	Unchecked []string `yaml:"unchecked"`
}

// expected is the rule's verdict on each app it names.
func (r rule) expected() map[string]string {
	out := map[string]string{}
	for result, apps := range map[string][]string{custom.Pass: r.Pass, custom.Fail: r.Fail, custom.Unchecked: r.Unchecked} {
		for _, a := range apps {
			out[a] = result
		}
	}
	return out
}

// recording is testdata/recorded.json: the model that answered, and each rule's text, the question it
// was asked (the sha256 of the system prompt, the prompt and the answer's schema), its answers, and why
// Admit refused each code answer it refused ("" for the others). Admission is timed on the machine that
// recorded, as it is once on the host that saves a rule, so a replay takes its outcome from here.
type recording struct {
	Model string              `json:"model"`
	Rules map[string]recorded `json:"rules"`
}

type recorded struct {
	Text     string   `json:"text"`
	Asked    string   `json:"asked"`
	Answers  []string `json:"answers"`
	Admitted []string `json:"admitted"`
}

// asked is the sha256 of the question the translator asks about rule.
func asked(rule string) (string, error) {
	sys, user, schema, err := custom.Question(rule)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return sha(sys + "\x00" + user + "\x00" + string(b)), nil
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func load(dir string) (corpus, map[string]custom.Facts, error) {
	var c corpus
	b, err := os.ReadFile(filepath.Join(dir, "corpus.yaml")) //nolint:gosec // the operator's -dir
	if err != nil {
		return c, nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, nil, fmt.Errorf("corpus.yaml: %w", err)
	}
	ids := map[string]bool{}
	for _, r := range c.Rules {
		named := slices.Concat(r.Pass, r.Fail, r.Unchecked)
		switch {
		case ids[r.ID]:
			return c, nil, fmt.Errorf("corpus.yaml: rule %s twice", r.ID)
		case r.Checker != custom.ByCode && r.Checker != custom.ByAI:
			return c, nil, fmt.Errorf("corpus.yaml: rule %s: checker %q", r.ID, r.Checker)
		case r.Checker == custom.ByCode && (len(r.Pass) == 0 || len(r.Fail) == 0):
			return c, nil, fmt.Errorf("corpus.yaml: rule %s: a code rule passes an app and fails one", r.ID)
		case r.Checker == custom.ByAI && r.Category != "adversarial" && len(named) > 0:
			return c, nil, fmt.Errorf("corpus.yaml: rule %s: only an adversarial ai rule names apps, the verdicts a code answer to it must have", r.ID)
		case len(named) != len(r.expected()):
			return c, nil, fmt.Errorf("corpus.yaml: rule %s names an app twice", r.ID)
		case len(named) > 0 && len(named) != len(c.Apps):
			return c, nil, fmt.Errorf("corpus.yaml: rule %s names %d apps: a rule with verdicts has one on every app", r.ID, len(named))
		}
		for _, a := range named {
			if _, ok := c.Apps[a]; !ok {
				return c, nil, fmt.Errorf("corpus.yaml: rule %s names app %s, which apps does not list", r.ID, a)
			}
		}
		ids[r.ID] = true
	}
	facts := map[string]custom.Facts{}
	for name, app := range c.Apps {
		tree, err := gate.Walk(filepath.Join(dir, "apps", name))
		if err != nil {
			return c, nil, err
		}
		var secrets []custom.Secret
		if app.Secrets != nil {
			secrets = append([]custom.Secret{}, *app.Secrets...)
		}
		report, err := os.ReadFile(filepath.Join(dir, "packages", name+".json")) //nolint:gosec // the operator's -dir
		if err != nil {
			return c, nil, err
		}
		facts[name] = custom.ReadFacts(tree, secrets, custom.Packages{Report: report})
	}
	return c, facts, nil
}

// translate is runs checks of every rule: from the model, recording its answers in dir, or replayed
// from that recording.
func translate(ctx context.Context, dir string, c corpus, replay bool, model string) (map[string][]custom.Check, string, error) {
	path := filepath.Join(dir, "recorded.json")
	rec := recording{Model: model, Rules: map[string]recorded{}}
	var live custom.Model
	if replay {
		b, err := os.ReadFile(path) //nolint:gosec // the operator's -dir
		if err != nil {
			return nil, "", err
		}
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, "", fmt.Errorf("recorded.json: %w", err)
		}
	} else {
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, "", errors.New("no ANTHROPIC_API_KEY: set it, or replay with --recorded")
		}
		live = gate.NewAnthropic(option.WithoutEnvironmentDefaults(), option.WithBaseURL("https://api.anthropic.com"), option.WithAPIKey(key))
	}
	for _, r := range c.Rules {
		q, err := asked(r.Text)
		if err != nil {
			return nil, "", err
		}
		got, ok := rec.Rules[r.ID]
		switch {
		case !replay:
			rec.Rules[r.ID] = recorded{Text: r.Text, Asked: q, Answers: make([]string, runs), Admitted: make([]string, runs)}
		case !ok || got.Text != r.Text || len(got.Answers) != runs || len(got.Admitted) != runs:
			return nil, "", fmt.Errorf("recorded.json has no %d answers to rule %s as the corpus words it: record it again", runs, r.ID)
		case got.Asked != q:
			return nil, "", fmt.Errorf("recorded.json answered rule %s under another system prompt, prompt or schema: record it again", r.ID)
		}
	}
	if !replay {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for _, r := range c.Rules {
			answers := rec.Rules[r.ID].Answers
			for i := range runs {
				g.Go(func() error {
					sys, user, schema, err := custom.Question(r.Text)
					if err == nil {
						answers[i], err = live.Ask(gctx, rec.Model, sys, user, schema)
					}
					if err != nil {
						return fmt.Errorf("rule %s: %w", r.ID, err)
					}
					return nil
				})
			}
		}
		if err := g.Wait(); err != nil {
			return nil, "", err
		}
	}
	// One at a time, as a host admits a rule: admissions at once would slow each other past the deadline.
	checks := map[string][]custom.Check{}
	for _, r := range c.Rules {
		got := rec.Rules[r.ID]
		for i, answer := range got.Answers {
			check := custom.Accept(answer)
			if !replay && check.By == custom.ByCode {
				if admitted := custom.Admit(check); admitted.By != custom.ByCode {
					got.Admitted[i] = admitted.Refused
				}
			}
			if why := got.Admitted[i]; why != "" && check.By == custom.ByCode {
				check.By, check.Expr, check.Says, check.Refused = custom.ByAI, "", "", why
			}
			checks[r.ID] = append(checks[r.ID], check)
		}
	}
	if !replay {
		b, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
			return nil, "", err
		}
	}
	return checks, rec.Model, nil
}

func run(ctx context.Context, w io.Writer, dir string, replay bool, model string) (bool, error) {
	c, facts, err := load(dir)
	if err != nil {
		return false, err
	}
	checks, model, err := translate(ctx, dir, c, replay, model)
	if err != nil {
		return false, err
	}
	rep, err := score(ctx, c, facts, checks, func(p *custom.Program) string {
		ctx, cancel := context.WithTimeout(ctx, custom.EvalDeadline)
		defer cancel()
		return p.AtTheBounds(ctx)
	})
	if err != nil {
		return false, err
	}
	text, met := rep.text(len(c.Rules), model)
	_, err = io.WriteString(w, text)
	return met, err
}

// report is the eval's per-rule lines, its failures and its totals.
type report struct {
	lines, failures                  []string
	translations, classified         int
	codeRuleRuns, codeRuleCode       int
	verdicts, verdictsRight          int
	stable                           int
	falsePasses, unsafe, codeWhereAI int
	adversarial, adversarialOK       int
}

// score evaluates every check against the corpus's expectations, and measures every check accepted as
// code with measure: why it is unsafe, "" when it is not.
func score(ctx context.Context, c corpus, facts map[string]custom.Facts, checks map[string][]custom.Check, measure func(*custom.Program) string) (report, error) {
	var rep report
	unsafe := map[string]string{}
	for _, r := range c.Rules {
		want := r.expected()
		apps := slices.Sorted(maps.Keys(want))
		var bys, vectors []string
		adversarialOK := true
		for i, check := range checks[r.ID] {
			rep.translations++
			bys = append(bys, check.By)
			var vector []string
			right := true
			if check.By == custom.ByCode {
				prg, err := custom.Compile(check.Expr)
				if err != nil {
					rep.unsafe++
					rep.failures = append(rep.failures, fmt.Sprintf("%s #%d: UNSAFE: an accepted expression does not compile again: %v", r.ID, i+1, err))
					continue
				}
				why, measured := unsafe[check.Expr]
				if !measured {
					why = measure(prg)
					unsafe[check.Expr] = why
				}
				if why != "" {
					rep.unsafe++
					rep.failures = append(rep.failures, fmt.Sprintf("%s #%d: UNSAFE: %s: %s", r.ID, i+1, why, check.Expr))
				}
				if len(apps) == 0 { // a rule code cannot decide: nothing says its verdicts are right
					rep.codeWhereAI++
					right = false
					rep.failures = append(rep.failures, fmt.Sprintf("%s #%d: code where ai was expected: %s", r.ID, i+1, check.Expr))
				}
				for _, a := range apps {
					v := prg.Evaluate(ctx, facts[a])
					vector = append(vector, v.Result)
					rep.verdicts++
					if v.Result == want[a] {
						rep.verdictsRight++
						continue
					}
					right = false
					what := "wrong verdict"
					if v.Result == custom.Pass {
						rep.falsePasses++
						what = "FALSE PASS"
					}
					why := ""
					if v.Why != "" {
						why = " (" + v.Why + ")"
					}
					rep.failures = append(rep.failures, fmt.Sprintf("%s #%d: %s on %s: %s, expected %s%s: %s", r.ID, i+1, what, a, v.Result, want[a], why, check.Expr))
				}
			}
			vectors = append(vectors, check.By+":"+strings.Join(vector, ","))
			if r.Checker == custom.ByCode {
				rep.codeRuleRuns++
				if check.By == custom.ByCode {
					rep.codeRuleCode++
				}
			}
			switch {
			case r.Category == "adversarial":
				if check.By == custom.ByAI || right {
					rep.classified++
				} else {
					adversarialOK = false
				}
			case check.By == r.Checker:
				rep.classified++
			case check.By == custom.ByAI:
				refused := ""
				if check.Refused != "" {
					refused = " (Canyon refused its code: " + check.Refused + ")"
				}
				rep.failures = append(rep.failures, fmt.Sprintf("%s #%d: %s where %s was expected%s", r.ID, i+1, check.By, r.Checker, refused))
			}
		}
		stable := !slices.ContainsFunc(vectors, func(v string) bool { return v != vectors[0] })
		if stable {
			rep.stable++
		}
		if r.Category == "adversarial" {
			rep.adversarial++
			if adversarialOK {
				rep.adversarialOK++
			}
		}
		rep.lines = append(rep.lines, fmt.Sprintf("%-28s %-11s %-6s %-14s %s", r.ID, r.Category, r.Checker, strings.Join(bys, ","), map[bool]string{true: "stable", false: "UNSTABLE"}[stable]))
	}
	return rep, nil
}

func pct(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.1f%%)", n, of, 100*float64(n)/float64(of))
}

// text is the report, and whether the bar is met.
func (rep report) text(rules int, model string) (string, bool) {
	var w strings.Builder
	fmt.Fprintf(&w, "ruleeval: %d rules × %d translations, model %s, facts version %d\n\n", rules, runs, model, custom.FactsVersion)
	fmt.Fprintf(&w, "%-28s %-11s %-6s %s\n", "RULE", "CATEGORY", "EXPECT", "GOT")
	for _, l := range rep.lines {
		fmt.Fprintln(&w, l)
	}
	fmt.Fprintf(&w, "\nFailures (%d):\n", len(rep.failures))
	for _, f := range rep.failures {
		fmt.Fprintln(&w, "  "+f)
	}
	verdictOK := rep.verdicts > 0 && rep.verdictsRight*100 >= 95*rep.verdicts
	stableOK := rep.stable*100 >= 95*rules
	codeOK := rep.codeRuleRuns > 0 && rep.codeRuleCode*100 >= 90*rep.codeRuleRuns
	bar := []struct {
		name string
		met  bool
		got  string
	}{
		{"zero false passes", rep.falsePasses == 0, fmt.Sprint(rep.falsePasses)},
		{"zero unsafe expressions accepted", rep.unsafe == 0, fmt.Sprint(rep.unsafe)},
		{"zero code where ai was expected", rep.codeWhereAI == 0, fmt.Sprint(rep.codeWhereAI)},
		{"code rules checked by code >= 90%", codeOK, pct(rep.codeRuleCode, rep.codeRuleRuns)},
		{"verdict accuracy on code >= 95%", verdictOK, pct(rep.verdictsRight, rep.verdicts)},
		{"every adversarial rule ai or correct code", rep.adversarialOK == rep.adversarial, pct(rep.adversarialOK, rep.adversarial)},
		{"3 translations agree on >= 95% of rules", stableOK, pct(rep.stable, rules)},
	}
	fmt.Fprintf(&w, "\nTotals:\n  classification accuracy  %s\n  code rules by code       %s\n  verdict accuracy         %s\n  stability                %s\n  false passes             %d\n  unsafe accepted          %d\n  code where ai expected   %d\n",
		pct(rep.classified, rep.translations), pct(rep.codeRuleCode, rep.codeRuleRuns), pct(rep.verdictsRight, rep.verdicts), pct(rep.stable, rules), rep.falsePasses, rep.unsafe, rep.codeWhereAI)
	fmt.Fprintln(&w, "\nThe bar:")
	met := true
	for _, b := range bar {
		fmt.Fprintf(&w, "  %-44s %-7s %s\n", b.name, map[bool]string{true: "met", false: "MISSED"}[b.met], b.got)
		met = met && b.met
	}
	return w.String(), met
}
