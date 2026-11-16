//go:build !race

// The tests that measure expressions at the facts' bounds, which make test-bounds runs without -race:
// the race detector makes CEL's cost tracker some fifteen times slower and its allocations no measure.

package main

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// The recorded live run replays to the report it printed, testdata/report.txt, every accepted
// expression measured at the facts' bounds, and that report meets the bar: CI holds the PR's numbers
// without calling a model.
func TestTheRecordingReproducesTheReportAtTheBounds(t *testing.T) {
	var out bytes.Buffer
	met, err := run(context.Background(), &out, "testdata", true, "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("the replay is not testdata/report.txt:\n%s", out.String())
	}
	if !met {
		t.Fatal("the recorded run misses the bar")
	}
}
