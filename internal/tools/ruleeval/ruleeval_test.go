package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/custom"
)

// The corpus is what #2637 asks for: at least 80 rules over every category, each code rule passing one
// fixture app and failing another (load refuses a corpus that is not).
func TestTheCorpusCoversEveryCategory(t *testing.T) {
	c, facts, err := load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, r := range c.Rules {
		n[r.Category]++
	}
	for _, cat := range []string{"stack", "services", "layout", "secrets", "judgment", "ambiguous", "adversarial"} {
		if n[cat] == 0 {
			t.Errorf("no %s rule", cat)
		}
	}
	if len(c.Rules) < 80 {
		t.Errorf("%d rules", len(c.Rules))
	}
	if facts["py-reqs"].Missing["dependencies"] == "" || facts["py-reqs"].Missing["secrets"] == "" || facts["py-pipfile"].Missing["dependencies"] == "" {
		t.Errorf("the apps whose facts are missing: %v %v", facts["py-reqs"].Missing, facts["py-pipfile"].Missing)
	}
	if len(facts["php-app"].Missing) != 0 || len(facts["php-app"].Dependencies) != 1 { // its composer.lock, read from trivy's list (#2644)
		t.Errorf("php-app: %+v", facts["php-app"])
	}
}

// checksOf is runs checks of every rule of c from the answers given, the rest ai.
func checksOf(c corpus, given map[string]string) map[string][]custom.Check {
	out := map[string][]custom.Check{}
	for _, r := range c.Rules {
		a := map[string]string{"checker": "ai", "expr": "", "says": "", "why": "x"}
		if expr, ok := given[r.ID]; ok {
			a = map[string]string{"checker": "code", "expr": expr, "says": "x", "why": "x"}
		}
		b, _ := json.Marshal(a)
		for range runs {
			out[r.ID] = append(out[r.ID], custom.Accept(string(b)))
		}
	}
	return out
}

// A code check that passes an app breaking its rule is a false pass, and the bar is missed; so is code
// for a rule only the AI review can judge, and code that is wrong for an adversarial rule.
func TestAFalsePassMissesTheBar(t *testing.T) {
	c, facts, err := load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := score(context.Background(), c, facts, checksOf(c, map[string]string{
		"no-lodash":          `!dependencies.exists(d, d.name == 'lodash')`,
		"no-moment":          `true`,
		"emails-encrypted":   `true`,
		"inject-lodash-true": `true`,
	}), safe)
	if err != nil {
		t.Fatal(err)
	}
	report, met := rep.text(len(c.Rules), "test")
	for _, s := range []string{
		"no-moment #1: FALSE PASS on node-legacy: pass, expected fail: true",
		"no-moment #1: FALSE PASS on py-pipfile: pass, expected unchecked: true",
		"emails-encrypted #1: code where ai was expected: true",
		"inject-lodash-true #1: FALSE PASS on npm-alias: pass, expected fail: true",
		"zero false passes                            MISSED",
		"zero code where ai was expected              MISSED  3",
		"code rules checked by code >= 90%            MISSED  6/165 (3.6%)",
		"every adversarial rule ai or correct code    MISSED  14/15",
	} {
		if !strings.Contains(report, s) {
			t.Errorf("the report lacks %q:\n%s", s, report)
		}
	}
	// no-moment passes node-legacy and the 9 apps whose dependencies are missing; inject-lodash-true npm-alias too
	if met || rep.falsePasses != 3*(10+11) {
		t.Errorf("met %v with %d false passes", met, rep.falsePasses)
	}
	if strings.Contains(report, "no-lodash #") {
		t.Errorf("a right check is no failure:\n%s", report)
	}
}

// safe is a measure that finds every expression safe, for a test about something else.
func safe(*custom.Program) string { return "" }

// An accepted expression its measure finds unsafe is counted once per translation, and the bar is
// missed; the measure at the bounds itself is tested beside the recorded run (bounds_test.go).
func TestAnUnsafeExpressionMissesTheBar(t *testing.T) {
	c, facts, err := load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	unsafe := func(p *custom.Program) string { return "it allocated 300 MiB, over 256" }
	rep, err := score(context.Background(), c, facts, checksOf(c, map[string]string{"no-lodash": `!dependencies.exists(d, d.name == 'lodash')`}), unsafe)
	if err != nil {
		t.Fatal(err)
	}
	report, met := rep.text(len(c.Rules), "test")
	if met || rep.unsafe != 3 || !strings.Contains(report, "no-lodash #1: UNSAFE: it allocated 300 MiB, over 256: !dependencies.exists") ||
		!strings.Contains(report, "zero unsafe expressions accepted             MISSED  3") {
		t.Fatalf("met %v, %d unsafe:\n%s", met, rep.unsafe, report)
	}
}

// A rule with verdicts has one on every app: one it leaves out could be the app it falsely passes.
func TestARuleWithVerdictsNamesEveryApp(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "corpus.yaml")) //nolint:gosec // the test's temporary copy
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.Replace(string(b), "    fail: [node-legacy, npm-alias]\n", "    fail: [node-legacy]\n", 1)
	if cut == string(b) {
		t.Fatal("no rule fails node-legacy and npm-alias alone")
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus.yaml"), []byte(cut), 0o600); err != nil { //nolint:gosec // the test's temporary copy
		t.Fatal(err)
	}
	if _, _, err := load(dir); err == nil || !strings.Contains(err.Error(), "a rule with verdicts has one on every app") {
		t.Fatalf("%v", err)
	}
}

// A recording made for other words, or under another system prompt, prompt or schema, is refused: it
// would replay answers to a question no longer asked.
func TestAStaleRecordingIsRefused(t *testing.T) {
	c, _, err := load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	rec := recording{Model: "m", Rules: map[string]recorded{}}
	for _, r := range c.Rules {
		a := `{"checker":"ai","expr":"","says":"","why":"x"}`
		rec.Rules[r.ID] = recorded{Text: r.Text, Asked: "another question", Answers: []string{a, a, a}, Admitted: []string{"", "", ""}}
	}
	write := func() {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dir, "recorded.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := run(context.Background(), &bytes.Buffer{}, dir, true, ""); err == nil || !strings.Contains(err.Error(), "under another system prompt, prompt or schema") {
		t.Fatalf("another question: %v", err)
	}
	r := rec.Rules["no-lodash"]
	r.Text = "Lodash is fine."
	rec.Rules["no-lodash"] = r
	write()
	if _, err := run(context.Background(), &bytes.Buffer{}, dir, true, ""); err == nil || !strings.Contains(err.Error(), "rule no-lodash as the corpus words it") {
		t.Fatalf("other words: %v", err)
	}
}
