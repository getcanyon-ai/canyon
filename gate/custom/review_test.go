package custom

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// noLodash is a custom rule checked by code, Blocking.
var noLodash = policy.Rule{ID: "custom.no-lodash", Name: "No lodash", State: policy.Blocking, Producer: policy.Scanner,
	Instructions: "No app may depend on lodash.",
	Check:        &policy.Check{By: ByCode, Expr: `!dependencies.exists(d, d.name == 'lodash')`, Says: "No dependency is named lodash.", FactsVersion: FactsVersion}}

// lockWith is a package-lock.json that locks name, as review reads it: trivy lists name from it, at
// 4.17.15 unless a version follows it.
func lockWith(name string) string { return "locks " + name }

func review(t *testing.T, files map[string]string, rules ...policy.Rule) []finding.Result {
	t.Helper()
	pkgs := Packages{Report: []byte(`{"SchemaVersion":2,"Results":[]}`)}
	if locked, ok := strings.CutPrefix(files["package-lock.json"], "locks "); ok {
		name, version, _ := strings.Cut(locked+" 4.17.15", " ")
		pkgs = listed("package-lock.json", "npm", name+" "+strings.Fields(version)[0]+" direct")
	}
	got, declared, err := Review(context.Background(), tree(t, files), rules, []Secret{}, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) != len(got) {
		t.Fatalf("%d results declare %d rules", len(got), len(declared))
	}
	return got
}

// A rule that holds has no result; one that does not hold is one finding under the rule's own id, at the
// package's first file, line 1, its snippet the check in plain words (contracts/finding.md, A custom
// rule checked by code).
func TestACustomRuleCheckedByCodeIsOneResultWhenItDoesNotHold(t *testing.T) {
	if got := review(t, map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("express")}, noLodash); len(got) != 0 {
		t.Fatalf("a rule that holds: %+v", got)
	}
	got := review(t, map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("lodash")}, noLodash)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	r, c := got[0], got[0].Properties.Canyon
	loc := r.Locations[0].PhysicalLocation
	if r.RuleID != "custom.no-lodash" || c.Severity != finding.High || c.Title != "No lodash" ||
		c.Problem != `"No lodash" (custom.no-lodash) does not hold for this app: Canyon checked it by code: No dependency is named lodash.` ||
		c.Fix != "Change the app so that this holds: No dependency is named lodash." ||
		loc.ArtifactLocation.URI != "index.js" || loc.Region.StartLine != 1 || loc.Region.Snippet.Text != "custom.no-lodash checked by code: No dependency is named lodash." {
		t.Fatalf("%+v %+v", r, loc)
	}
}

// A rule that could not be checked is never a pass: a fact it reads missing, or a check written for
// other facts, is one finding under the rule's own id, so it blocks where the rule blocks.
func TestACustomRuleThatCouldNotBeCheckedIsItsOwnFinding(t *testing.T) {
	got := review(t, map[string]string{"app.py": "", "requirements.txt": "flask==3\n"}, noLodash)
	if len(got) != 1 || got[0].RuleID != "custom.no-lodash" || got[0].Properties.Canyon.Title != `Canyon could not check "No lodash"` ||
		got[0].Properties.Canyon.Problem != "could not check: custom.no-lodash: dependencies: requirements.txt names the packages it asks for, not every one pip installs: a uv.lock, poetry.lock or Pipfile.lock names them all" ||
		got[0].Locations[0].PhysicalLocation.Region.Snippet.Text != "custom.no-lodash could not be checked by code" {
		t.Fatalf("a missing fact: %+v", got)
	}
	old := noLodash
	old.Check = &policy.Check{By: ByCode, Expr: noLodash.Check.Expr, Says: noLodash.Check.Says, FactsVersion: FactsVersion - 1}
	got = review(t, map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("express")}, old)
	if len(got) != 1 || !strings.Contains(got[0].Properties.Canyon.Problem, "an admin saves it again") {
		t.Fatalf("a check for other facts: %+v", got)
	}
}

// Only a custom rule that is on and checked by code is the scanners' run's.
func TestOnlyACustomRuleOnAndCheckedByCodeIsReviewed(t *testing.T) {
	off, byAI := noLodash, noLodash
	off.State = policy.Off
	byAI.Producer, byAI.Check = policy.LLMReview, &policy.Check{By: ByAI, FactsVersion: FactsVersion}
	if got := review(t, map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("lodash")}, off, byAI); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// An approval is keyed by the fingerprint, which frames the facts the check read when it did not hold,
// or the reason it could not be checked: an approval of a could-not-check never covers a failure, and an
// approval of a failure covers those same facts only (contracts/finding.md, canyon/v1).
func TestAnApprovalOfOneOutcomeNeverCoversAnother(t *testing.T) {
	fp := func(files map[string]string) string {
		got := review(t, files, noLodash)
		if len(got) != 1 {
			t.Fatalf("%+v", got)
		}
		return got[0].PartialFingerprints.CanyonV1
	}
	failing := map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("lodash")}
	failed := fp(failing)
	failing["README.md"] = "another file, the same dependencies"
	if fp(failing) != failed {
		t.Error("a change the check does not read moved the failure's fingerprint")
	}
	failing["package-lock.json"] = lockWith("lodash 4.17.21")
	if fp(failing) == failed {
		t.Error("other dependencies kept the failure's fingerprint")
	}
	unread := fp(map[string]string{"index.js": "", "package.json": "{}"})
	if unread == failed || unread == fp(failing) {
		t.Error("a could-not-check shares a failure's fingerprint")
	}
}

// Why a rule could not be checked is said in plain words, never in Canyon's internals (#2645 review).
func TestACouldNotCheckSaysWhyInPlainWords(t *testing.T) {
	old, broken := noLodash, noLodash
	old.Check = &policy.Check{By: ByCode, Expr: noLodash.Check.Expr, Says: noLodash.Check.Says, FactsVersion: FactsVersion - 1}
	broken.Check = &policy.Check{By: ByCode, Expr: "dependencies.size() + 1", Says: noLodash.Check.Says, FactsVersion: FactsVersion}
	files := map[string]string{"index.js": "", "package.json": "{}", "package-lock.json": lockWith("express")}
	for r, want := range map[*policy.Rule]string{
		&old:    "could not check: custom.no-lodash: this rule was saved for an older Canyon: an admin saves it again",
		&broken: "could not check: custom.no-lodash: this Canyon can't run this rule's check: an admin saves it again",
	} {
		if got := review(t, files, *r); len(got) != 1 || got[0].Properties.Canyon.Problem != want {
			t.Errorf("%+v, want %q", got, want)
		}
	}
}

// An approval of a fact past its bounds carries over a deploy that only changes how far past: its key is
// the reason without the count, which the text people read still says (#2645 review).
func TestAnApprovalOfAFactPastItsBoundsCarriesOverACountChange(t *testing.T) {
	fp := func(n int) (string, string) {
		f := facts()
		f.Files = make([]string, n)
		for i := range f.Files {
			f.Files[i] = fmt.Sprintf("f%d", i)
		}
		f.bound()
		readme := noLodash
		readme.Check = &policy.Check{By: ByCode, Expr: "'README.md' in files", Says: "The app has a README.md.", FactsVersion: FactsVersion}
		got, _, err := reviewFacts(context.Background(), []policy.Rule{readme}, f, "f0")
		if err != nil || len(got) != 1 {
			t.Fatalf("%+v %v", got, err)
		}
		return got[0].PartialFingerprints.CanyonV1, got[0].Properties.Canyon.Problem
	}
	first, said := fp(bounds["files"].items + 1)
	second, _ := fp(bounds["files"].items + 2)
	if first != second || !strings.Contains(said, fmt.Sprintf("%d of them", bounds["files"].items+1)) {
		t.Fatalf("%s, %s: %q", first, second, said)
	}
}

// Judge is a review's judgment, for a dry run: pass, fail, or not checked with the review's own reason
// (#2640).
func TestJudgeIsTheReviewsJudgment(t *testing.T) {
	old := noLodash
	old.Check = &policy.Check{By: ByCode, Expr: noLodash.Check.Expr, Says: "x", FactsVersion: FactsVersion - 1}
	with := func(name string) Facts {
		f := facts()
		f.Dependencies = []Dependency{{Name: name, Version: "1.0.0", Ecosystem: "npm", Direct: true}}
		return f
	}
	missing := facts()
	missing.Missing = map[string]string{"dependencies": TrivyOff}
	for name, c := range map[string]struct {
		rule        policy.Rule
		facts       Facts
		result, why string
	}{
		"holds":        {noLodash, with("express"), Pass, ""},
		"does not":     {noLodash, with("lodash"), Fail, ""},
		"a fact gone":  {noLodash, missing, Unchecked, "dependencies: " + TrivyOff},
		"an older one": {old, with("express"), Unchecked, "this rule was saved for an older Canyon: an admin saves it again"},
	} {
		if result, why := Judge(context.Background(), c.rule, c.facts); result != c.result || why != c.why {
			t.Errorf("%s: %s %q", name, result, why)
		}
	}
}

// Trivy is asked for the packages only for a rule on, checked by code and runnable here, that reads the
// dependencies: a rule that never reads them never depends on trivy (#2652 review).
func TestOnlyARuleThatReadsTheDependenciesAsksForThem(t *testing.T) {
	readme, old, off := noLodash, noLodash, noLodash
	readme.Check = &policy.Check{By: ByCode, Expr: "'README.md' in files", Says: "x", FactsVersion: FactsVersion}
	old.Check = &policy.Check{By: ByCode, Expr: noLodash.Check.Expr, Says: "x", FactsVersion: FactsVersion - 1}
	off.State = policy.Off
	for rules, want := range map[*[]policy.Rule]bool{
		{noLodash}: true, {readme, noLodash}: true, {readme}: false, {old}: false, {off}: false, {}: false,
	} {
		if got := ReadsDependencies(*rules); got != want {
			t.Errorf("%v: %v, want %v", *rules, got, want)
		}
	}
}
