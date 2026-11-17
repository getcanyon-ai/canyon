package cli

import (
	"strings"
	"testing"
)

// carriedRun is the golden run with its first AI review finding carried from an earlier review (contracts/finding.md,
// A blocking finding an earlier review made stays while its code stands; #2900).
func carriedRun(t *testing.T) checkRun {
	t.Helper()
	run := goldenRun(t)
	for i := range run.Result.Findings {
		if run.Result.Findings[i].Tool == "canyon-review" {
			run.Result.Findings[i].Properties.Canyon.CarriedFrom = "gr_7d3e8f2a91c04b56"
			return run
		}
	}
	t.Fatal("the golden has no AI review finding")
	return run
}

// Every format says a carried finding was found in an earlier review: human under its place, ai under its problem
// with what to do, json as carried_from; a finding not carried says nothing of it.
func TestACarriedFindingSaysItWasFoundInAnEarlierReview(t *testing.T) {
	var human strings.Builder
	(&app{out: &human}).verdict(carriedRun(t))
	if got := human.String(); strings.Count(got, "found in an earlier review · the code hasn't changed, so it still blocks\n") != 1 {
		t.Errorf("human:\n%s", got)
	}
	var ai strings.Builder
	aiApp(&ai, "check").verdict(carriedRun(t))
	want := "\nearlier review:\n  This was found in an earlier review and this review didn't report it.\n" +
		"  The code hasn't changed, so it still blocks: change the code so the rule\n" +
		"  holds, or ask for an approval. Don't wait for another review to drop it.\n"
	if got := ai.String(); strings.Count(got, want) != 1 {
		t.Errorf("ai:\n%s", got)
	}
	blocksLinesAreFindings(t, ai.String(), len(carriedRun(t).Result.Findings))
	var js strings.Builder
	jsonApp(&js, "check").verdict(carriedRun(t))
	if got := js.String(); strings.Count(got, `"carried_from":"gr_7d3e8f2a91c04b56"`) != 1 {
		t.Errorf("json: %s", got)
	}
	var plain strings.Builder
	(&app{out: &plain}).verdict(goldenRun(t))
	if strings.Contains(plain.String(), "earlier review") {
		t.Errorf("a finding not carried says nothing of an earlier review:\n%s", plain.String())
	}
}
