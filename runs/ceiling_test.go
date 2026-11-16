package runs

import (
	"testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// A /run's ceiling outlasts the executor's own worst case, so the server never cancels a start the
// executor is still entitled to finish: on Kubernetes a pod has executor.RunAvailable to become
// available and a failed first run as long again to be cleaned up (contracts/executor.md).
func TestTheRunCeilingOutlastsTheExecutorsRollout(t *testing.T) {
	if worst := 2 * executor.RunAvailable; runCeiling <= worst {
		t.Fatalf("runCeiling %s cuts a /run the executor may take %s to answer", runCeiling, worst)
	}
}
