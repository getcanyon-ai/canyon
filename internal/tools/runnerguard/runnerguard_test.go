// Package runnerguard holds the check that a job picks Canyon's own self-hosted runners only where the repository says
// it has them (its CANYON_RUNNERS variable): every other copy of this tree, the public repository included, runs on
// GitHub's hosted runners, and never queues for runners it doesn't have (#2983).
package runnerguard

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// guard is the condition every self-hosted runs-on must carry: the repository's own say that it has our runners.
const guard = "vars.CANYON_RUNNERS == 'self-hosted'"

func TestOurRunnersServeTheirOwnRepositoryAlone(t *testing.T) {
	workflows := os.DirFS("../../../.github/workflows")
	files, err := fs.Glob(workflows, "*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows read: %v", err)
	}
	for _, f := range files {
		b, err := fs.ReadFile(workflows, f)
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			Jobs map[string]struct {
				RunsOn yaml.Node `yaml:"runs-on"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, job := range wf.Jobs {
			var on []byte
			if on, err = yaml.Marshal(&job.RunsOn); err != nil {
				t.Fatal(err)
			}
			s := string(on)
			if strings.Contains(s, "self-hosted") && !strings.Contains(s, guard) {
				t.Errorf("%s job %s asks for self-hosted runners without %q", f, name, guard)
			}
		}
	}
}
