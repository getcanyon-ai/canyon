//go:build stack

package e2e

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
)

// TestModel is J9, on a stack whose AI reviewer is the stub (make e2e-up MODEL=1, then make e2e
// MODEL=1): TestStack's journeys expect no model and skip there.
func TestModel(t *testing.T) {
	if os.Getenv("MODEL") == "" {
		t.Skip("no model on this stack: make e2e-up MODEL=1, then make e2e MODEL=1")
	}
	if !ready() {
		t.Skipf("no Canyon stack answers %s/ready: start one with make e2e-up MODEL=1", host)
	}
	maskApp := "e2e-mask-" + tag
	t.Cleanup(func() { forget(t, maskApp) })
	dir := fixture(t, "f-mask", maskApp)
	key := regexp.MustCompile(`accessKeyId: "([^"]+)", secretAccessKey: "([^"]+)"`).FindStringSubmatch(readFile(t, dir+"/apps/web/src/lib/aws.ts"))
	if key == nil {
		t.Fatal("f-mask's aws.ts holds no key: regenerate the fixtures with make e2e-up")
	}

	// The model fails three times, the client's retries with it: the run fails as the model's, and
	// what it was sent holds neither half of the key.
	seen := len(stubLog(t))
	stub(t, "model", http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"e2e"}}`, 3)
	if out, _ := check(t, dir); out.Result != "failed" || failureOf(t, out.GateRun) != "model" {
		t.Fatalf("a model answering 500: %s, reason %q; want failed, model", out.Result, failureOf(t, out.GateRun))
	}
	sent := modelBodies(t, seen)
	if len(sent) == 0 || !slices.ContainsFunc(sent, func(b string) bool { return strings.Contains(b, "aws.ts") }) {
		t.Fatalf("the model was sent %d request(s); want aws.ts among the files shown", len(sent))
	}
	for _, b := range sent {
		if strings.Contains(b, key[1]) || strings.Contains(b, key[2]) {
			t.Fatal("the model was sent the AWS key; want it masked")
		}
	}

	// One result under the reviewer's rule, one outside them: the first stands, the second is dropped.
	stub(t, "model", http.StatusOK, `{"results":[`+
		`{"rule":"perf.unbounded-refetch","file":"apps/web/src/components/ClaimsTable.tsx","start_line":1,"end_line":1,"severity":"medium","title":"Refetch on every keystroke","problem":"The table refetches on each keystroke.","fix":"Debounce the search input.","required_end_state":"The table fetches once per settled query.","do_not":["raise the page size instead"]},`+
		`{"rule":"secrets.client-bundle","file":"apps/web/src/lib/aws.ts","start_line":2,"end_line":2,"severity":"critical","title":"x","problem":"x","fix":"x","required_end_state":"x","do_not":["x"]}]}`, 1)
	out, _ := check(t, dir)
	if !slices.ContainsFunc(out.Findings, func(f cliFinding) bool { return f.Rule == "perf.unbounded-refetch" }) {
		t.Errorf("findings %+v; want the reviewer's perf.unbounded-refetch", out.Findings)
	}
	if n := notes(out); !slices.Contains(n, "ai review · dropped 1 invalid result(s)") {
		t.Errorf("notes %q; want one result dropped", n)
	}

	// The same code under the same policy, model and agent: the review is reused, the model not called.
	seen = len(stubLog(t))
	again, _ := check(t, dir)
	if n := notes(again); !slices.Contains(n, gate.Reused) {
		t.Errorf("notes %q; want the review reused", n)
	}
	if n := len(modelBodies(t, seen)); n != 0 {
		t.Errorf("the model was called %d time(s) for code it reviewed; want none", n)
	}
	if !slices.ContainsFunc(again.Findings, func(f cliFinding) bool { return f.Rule == "perf.unbounded-refetch" }) {
		t.Errorf("the reused review's findings %+v; want perf.unbounded-refetch again", again.Findings)
	}
}

// modelBodies is every model request the stub logged after the first seen.
func modelBodies(t *testing.T, seen int) []string {
	t.Helper()
	var out []string
	for _, r := range stubLog(t)[seen:] {
		if r.Path == "/v1/messages" {
			out = append(out, r.Body)
		}
	}
	return out
}

func failureOf(t *testing.T, run string) string {
	t.Helper()
	var r struct {
		Run struct {
			FailureReason string `json:"failure_reason"`
		}
	}
	read(t, "/api/v1/gate-runs/"+run, &r)
	return r.Run.FailureReason
}

func notes(out cliRun) []string {
	var n []string
	for _, x := range out.Notes {
		n = append(n, x.Text)
	}
	return n
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the fixture copy this test made
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
