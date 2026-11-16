package patching

import (
	"cmp"
	"fmt"
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/runs"
)

// fleet is n apps of one identity, planned and in no wave, by name.
func fleet(n int) []waveApp {
	apps := make([]waveApp, n)
	for i := range apps {
		apps[i] = waveApp{Project: fmt.Sprintf("app-%02d", i), Wave: -1, Outcome: runs.PatchPlanned}
	}
	return apps
}

// tickWaves advances apps as a tick does, taking each app a wave took into it, and answers the wave.
func tickWaves(wave int, paused, stalled bool, apps []waveApp) int {
	wave, taken := advance(wave, paused, stalled, apps)
	for i, a := range apps {
		if w, ok := taken[a.Project]; ok {
			apps[i].Wave = w
		}
	}
	return wave
}

// end ends every app of wave still under way with outcome.
func end(apps []waveApp, wave int, outcome string) {
	for i, a := range apps {
		if a.Wave == wave && (a.Outcome == runs.PatchPlanned || a.Outcome == runs.PatchRunning) {
			apps[i].Outcome = outcome
		}
	}
}

func inWave(apps []waveApp, wave int) []string {
	var out []string
	for _, a := range apps {
		if a.Wave == wave {
			out = append(out, a.Project)
		}
	}
	slices.Sort(out)
	return out
}

// An identity rolls out in waves: a canary, one app's preview, then 5, 25 and the rest, each once every
// watch of the last ran to its end; the canary is the first app by a hash of its id, the same every tick
// (design/PATCHING.md §4.2).
func TestWavesCanaryThenFiveThenTwentyFiveThenTheRest(t *testing.T) {
	apps := fleet(40)
	wave := tickWaves(0, false, false, apps)
	canary := inWave(apps, 0)
	want := slices.MinFunc(apps, func(x, y waveApp) int { return cmp.Compare(order(x.Project), order(y.Project)) }).Project
	if wave != 0 || len(canary) != 1 || canary[0] != want {
		t.Fatalf("the first tick: wave %d, canary %v, want the first by hash, %s", wave, canary, want)
	}
	if again := tickWaves(wave, false, false, apps); again != 0 || len(inWave(apps, 0)) != 1 {
		t.Fatalf("a tick while the canary's watch runs: wave %d, %v", again, inWave(apps, 0))
	}
	for w, size := range []int{1, 5, 25, 9} {
		if got := inWave(apps, w); len(got) != size {
			t.Fatalf("wave %d took %d apps, want %d", w, len(got), size)
		}
		end(apps, w, runs.PatchPassed)
		wave = tickWaves(wave, false, false, apps)
	}
	if wave != 3 {
		t.Fatalf("after every wave: wave %d", wave)
	}
	late := waveApp{Project: "app-late", Wave: -1, Outcome: runs.PatchPlanned}
	apps = append(apps, late)
	if tickWaves(wave, false, false, apps); !slices.Contains(inWave(apps, 3), "app-late") {
		t.Fatal("an app planned after the last wave began is not taken into it")
	}
}

// A watch a person's request ended proves nothing: when it was the canary's, the next tick names the next
// app in canary order as a replacement canary, and the first is never the canary again; in a later wave such
// an app is left out of the count, and the wave goes on (design/PATCHING.md §4.2).
func TestAReplacementCanaryAfterAWatchEndedEarly(t *testing.T) {
	apps := fleet(10)
	tickWaves(0, false, false, apps)
	first := inWave(apps, 0)
	end(apps, 0, runs.PatchEndedEarly)
	if wave := tickWaves(0, false, false, apps); wave != 0 || len(inWave(apps, 0)) != 2 {
		t.Fatalf("after the canary's watch ended early: wave %d, wave 0 %v", wave, inWave(apps, 0))
	}
	end(apps, 0, runs.PatchPassed)
	wave := tickWaves(0, false, false, apps)
	if wave != 1 || len(inWave(apps, 1)) != 5 || slices.Contains(inWave(apps, 1), first[0]) {
		t.Fatalf("the replacement passed: wave %d, wave 1 %v (the first canary %s)", wave, inWave(apps, 1), first[0])
	}
	for i, a := range apps {
		if a.Wave == 1 {
			apps[i].Outcome = runs.PatchEndedEarly // one of them proves nothing; the rest pass
			break
		}
	}
	end(apps, 1, runs.PatchPassed)
	if wave := tickWaves(1, false, false, apps); wave != 2 {
		t.Fatalf("a later wave with a watch ended early goes on: wave %d", wave)
	}
}

// One revert short of the breaker stalls the identity: no next wave starts, nor a replacement canary; a
// paused identity moves nothing at all (design/PATCHING.md §4.2).
func TestAStalledOrPausedIdentityTakesNoWave(t *testing.T) {
	apps := fleet(10)
	tickWaves(0, false, false, apps)
	end(apps, 0, runs.PatchReverted)
	if wave, taken := advance(0, false, true, apps); wave != 0 || len(taken) != 0 {
		t.Fatalf("stalled on the canary's revert: wave %d, took %v", wave, taken)
	}
	if wave, taken := advance(0, true, false, fleet(10)); wave != 0 || len(taken) != 0 {
		t.Fatalf("paused: wave %d, took %v", wave, taken)
	}
	if wave := tickWaves(0, false, false, apps); wave != 0 || len(inWave(apps, 0)) != 2 {
		t.Fatalf("resumed, the reverted canary left out: wave %d, wave 0 %v", wave, inWave(apps, 0))
	}
}

// A wave starts only with apps to take: once the canary passed with no other app planned, the identity
// stays where it is, and apps planned on a later tick — the rule turned on later, a read that did not check
// at first — are taken five, then 25, never all at once into the rest (design/PATCHING.md §4.2).
func TestAWaveStartsOnlyWithAppsToTake(t *testing.T) {
	apps := fleet(1)
	tickWaves(0, false, false, apps)
	end(apps, 0, runs.PatchPassed)
	if wave := tickWaves(0, false, false, apps); wave != 0 {
		t.Fatalf("the canary passed, no app waiting: wave %d, want 0", wave)
	}
	for i := range 40 {
		apps = append(apps, waveApp{Project: fmt.Sprintf("late-%02d", i), Wave: -1, Outcome: runs.PatchPlanned})
	}
	if wave := tickWaves(0, false, false, apps); wave != 1 || len(inWave(apps, 1)) != 5 || len(inWave(apps, 3)) != 0 {
		t.Fatalf("apps planned later: wave %d, wave 1 %d, the rest %d", wave, len(inWave(apps, 1)), len(inWave(apps, 3)))
	}
}

// An app a watch of which ended early is planned again, but never as the canary of its identity: the canary
// is the next in canary order, and the app is taken by a later wave (design/PATCHING.md §4.2).
func TestAnAppWhoseWatchEndedEarlyIsNeverTheCanary(t *testing.T) {
	apps := fleet(10)
	first := slices.MinFunc(apps, func(x, y waveApp) int { return cmp.Compare(order(x.Project), order(y.Project)) }).Project
	for i := range apps {
		apps[i].NoCanary = apps[i].Project == first
	}
	tickWaves(0, false, false, apps)
	if canary := inWave(apps, 0); len(canary) != 1 || canary[0] == first {
		t.Fatalf("the canary is %v; %s's watch ended early before", canary, first)
	}
	end(apps, 0, runs.PatchPassed)
	if wave := tickWaves(0, false, false, apps); wave != 1 || !slices.Contains(inWave(apps, 1), first) {
		t.Fatalf("the next wave: %d, %v; want %s in wave 1", wave, inWave(apps, 1), first)
	}
}
