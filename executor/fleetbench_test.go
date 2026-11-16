//go:build fleetbench

package executor

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestFleetRunningTiming times POST /running's work for a large fleet on the Docker daemon it runs beside (#2752):
// the fleet read's fan-out, every environment asked in calls of MaxRunningOf at once, as cmd/canyon-server's
// sayRunning makes them under runningWithin, ROUNDS times, with p50 and p95; then, one call at a time, how long each
// of RunningOf's docker calls takes, the profile of where it goes. The fleet is FLEET_APPS apps named
// fleet-0000…, preview and prod each, which .github/workflows/fleet-running.yml makes before it runs this. Never on
// a laptop: it reads whatever this daemon holds.
func TestFleetRunningTiming(t *testing.T) {
	apps, err := strconv.Atoi(os.Getenv("FLEET_APPS"))
	if err != nil || apps < 1 {
		t.Fatal("FLEET_APPS names how many apps the workflow made")
	}
	rounds, err := strconv.Atoi(os.Getenv("ROUNDS"))
	if err != nil || rounds < 1 {
		rounds = 20
	}
	var asked []Container
	for i := range apps {
		for _, target := range []string{"preview", "prod"} {
			asked = append(asked, Container{App: fmt.Sprintf("fleet-%04d", i), Target: target})
		}
	}
	e := &Executor{}
	chunks := slices.Collect(slices.Chunk(asked, MaxRunningOf))
	var fanOuts, calls []time.Duration
	running := 0
	for r := range rounds {
		began := time.Now()
		took, errs, answers := make([]time.Duration, len(chunks)), make([]error, len(chunks)), make([]Runs, len(chunks))
		var wg sync.WaitGroup
		for i, chunk := range chunks {
			wg.Go(func() {
				at := time.Now()
				answers[i], errs[i] = e.RunningOf(context.Background(), chunk, false)
				took[i] = time.Since(at)
			})
		}
		wg.Wait()
		fanOuts = append(fanOuts, time.Since(began))
		calls = append(calls, took...)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, call %d: %v", r, i, err)
			}
		}
		if r == 0 {
			for _, a := range answers {
				running += len(a.Running)
			}
		}
	}
	t.Logf("fleet: %d apps, %d environments asked in %d calls of at most %d at once; %d answered running", apps, len(asked), len(chunks), MaxRunningOf, running)
	t.Logf("the fan-out, %d rounds: p50 %s, p95 %s, max %s (the server's bound, runningWithin, is 2s)", rounds, quantile(fanOuts, 0.5), quantile(fanOuts, 0.95), slices.Max(fanOuts))
	t.Logf("each call within it, %d calls: p50 %s, p95 %s, max %s", len(calls), quantile(calls, 0.5), quantile(calls, 0.95), slices.Max(calls))

	// The profile, one call alone at a time, so no call waits on another: RunningOf's two docker calls apart.
	var lists, inspects []time.Duration
	for _, chunk := range chunks {
		var names []string
		for _, c := range chunk {
			names = append(names, c.App+"-"+c.Target)
		}
		at := time.Now()
		listed, err := e.containers(context.Background(), names...)
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, time.Since(at))
		at = time.Now()
		if _, err := e.restarts(context.Background(), listed); err != nil {
			t.Fatal(err)
		}
		inspects = append(inspects, time.Since(at))
	}
	t.Logf("alone, per call of %d: docker container ls with a name filter each p50 %s, max %s; docker container inspect of those up p50 %s, max %s",
		MaxRunningOf, quantile(lists, 0.5), slices.Max(lists), quantile(inspects, 0.5), slices.Max(inspects))

	// What one list of every container costs, with no filter: the measure a fix that lists once and matches the
	// names here would be held to. Timed alone, as above.
	var whole []time.Duration
	for range 5 {
		at := time.Now()
		out, err := child(context.Background(), e.docker(), "container", "ls", "--all", "--no-trunc", "--format", "{{.Names}} {{.State}} {{.Image}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		whole = append(whole, time.Since(at))
		if len(out) == 0 {
			t.Fatal("docker container ls --all listed nothing")
		}
	}
	t.Logf("alone: one docker container ls --all with no filter, %d times: p50 %s, max %s", len(whole), quantile(whole, 0.5), slices.Max(whole))
}

// quantile is the q-th of ds by the nearest rank.
func quantile(ds []time.Duration, q float64) time.Duration {
	s := slices.Sorted(slices.Values(ds))
	return s[min(len(s)-1, int(q*float64(len(s))))]
}
