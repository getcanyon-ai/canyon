package claguard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The required cla check lets nobody through by a name they can write themselves. The pinned action matches its
// allowlist, unanchored, against an author's login, or the git author name a commit claims when its email is linked
// to no account, so any allowlist pattern is one a stranger's commit can match: a commit authored "x[bot]" passed
// "*[bot]" unsigned. There is none, and the one pull request skipped is Dependabot's, by the account that opened it,
// which nobody can claim.
func TestTheCLACheckSkipsNobodyByAName(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/workflows/cla.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["cla"]
	if !ok {
		t.Fatal("cla.yml has no cla job")
	}
	checked := false
	for _, s := range job.Steps {
		if !strings.HasPrefix(s.Uses, "contributor-assistant/github-action@") {
			continue
		}
		checked = true
		if v, ok := s.With["allowlist"]; ok {
			t.Errorf("the CLA action has an allowlist, %q: a commit whose author name matches it passes unsigned", v)
		}
	}
	if !checked {
		t.Fatal("cla.yml runs no contributor-assistant action")
	}
	for _, opener := range []string{"github.event.pull_request.user.login != 'dependabot[bot]'", "github.event.issue.user.login != 'dependabot[bot]'"} {
		if !strings.Contains(job.If, opener) {
			t.Errorf("the cla job's if lacks %s: Dependabot's pull requests are skipped by who opened them", opener)
		}
	}
	if strings.Contains(job.If, "github.actor") || strings.Contains(job.If, "author") {
		t.Errorf("the cla job's if names an actor or an author, which a pull request can change: %s", job.If)
	}
}
