package policy

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// rulesDocs is the docs site's rules section: one page per rule, named by its id, and an index.
var rulesDocs = filepath.Join("..", "..", "docs", "src", "content", "docs", "rules")

// added is the rule with a page that Canyon doesn't install: sarif.grype, the one tool Canyon runs
// through its executor that an admin adds (contracts/finding.md, Canyon-run tools), registered Off
// as any added tool is (contracts/snapshot.md). Its page says so where a shipped rule's names its default.
var added = []Rule{{ID: "sarif.grype", Name: "Findings from grype", Producer: Sarif}}

// TestEveryRuleHasItsPage holds the docs to the rules: a rule can't ship without a page saying what
// it finds and how to fix it, and the page's name and default state are the rule's own.
func TestEveryRuleHasItsPage(t *testing.T) {
	index, err := os.ReadFile(filepath.Join(rulesDocs, "index.md")) //nolint:gosec // a path this test names
	if err != nil {
		t.Fatal(err)
	}
	sidebar, err := os.ReadFile(filepath.Join("..", "..", "docs", "astro.config.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]bool{"index.md": true}
	for i, r := range append(slices.Clone(canonical), added...) {
		pages[r.ID+".md"] = true
		raw, err := os.ReadFile(filepath.Join(rulesDocs, r.ID+".md")) //nolint:gosec // a rule id from the table above
		if err != nil {
			t.Errorf("%s has no page: %v", r.ID, err)
			continue
		}
		page := string(raw)
		heading := "not on until an admin adds it"
		if i < len(canonical) {
			heading = strings.ToUpper(string(r.State[:1])) + string(r.State[1:]) + " by default"
		}
		for _, want := range []string{
			"slug: rules/" + r.ID + "\n",
			"title: " + strconv.Quote(r.Name) + "\n",
			"`" + r.ID + "` · " + heading,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s.md does not say %q", r.ID, want)
			}
		}
		if r.Producer == Sarif { // a tool's own findings no named rule maps: a section of their own
			heading = "Everything else a scanner reports"
		}
		_, section, _ := strings.Cut(string(index), "## "+heading+"\n")
		section, _, _ = strings.Cut(section, "\n## ")
		if !strings.Contains(section, "(/rules/"+r.ID+"/)") {
			t.Errorf("the rules index does not link %s under %q", r.ID, heading)
		}
		if !strings.Contains(string(sidebar), `"rules/`+r.ID+`"`) {
			t.Errorf("the docs sidebar (docs/astro.config.mjs) does not list %s", r.ID)
		}
	}
	files, err := os.ReadDir(rulesDocs)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !pages[f.Name()] {
			t.Errorf("rules/%s is a page for no rule", f.Name())
		}
	}
}

// TestEveryRuleSaysHowToPassIt holds howToPass to the rules: every rule Canyon ships or an admin adds
// has its one line, short enough for an agent's context, and the table names no other rule (#2622).
func TestEveryRuleSaysHowToPassIt(t *testing.T) {
	rules := append(slices.Clone(canonical), added...)
	for _, r := range rules {
		line := HowToPass(r.ID)
		if line == "" {
			t.Errorf("%s has no line on how to pass it in howToPass", r.ID)
		}
		if len(line) > 110 || strings.ContainsAny(line, "\n\t") {
			t.Errorf("%s's line is not one short line: %q", r.ID, line)
		}
	}
	for id := range howToPass {
		if !slices.ContainsFunc(rules, func(r Rule) bool { return r.ID == id }) {
			t.Errorf("howToPass names %s, which is no rule", id)
		}
	}
}
