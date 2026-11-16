package runs

import (
	"testing"

	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// design/EGRESS.md §4.3: an env is marked token_only on Kubernetes once a /run with egress succeeded, its answer
// saying its pods carry the token (token_callers) and no pod of an earlier template remains (previous_gone); never by
// an answer without token_callers — an executor from before #2142, whose pods have no forwarder — a /run whose
// earlier pods may still serve, a failed one or one without egress. Docker's /run never answers token_callers, and its
// mark needs a Kubernetes host besides (p.Access): a Docker /run with egress needs an internal network, which the fake
// executor refuses here.
func TestTheTokenOnlyMarkFollowsARunWhosePreviousIsGone(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	var image, run string
	if err := p.QueryRow(ctx, "SELECT image, gate_run FROM deploys WHERE env = 'preview'").Scan(&image, &run); err != nil {
		t.Fatal(err)
	}
	marked := func() bool {
		t.Helper()
		var m bool
		if err := p.QueryRow(ctx, "SELECT coalesce((SELECT token_only FROM egress_envs WHERE project_id = $1 AND env = 'preview'), false)", project).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	runs := func(what string, spec Egress, succeeds, want bool) {
		t.Helper()
		_, _, _, err := pipe.RunApp(ctx, run, project, "preview", image, spec, "")
		if (err == nil) != succeeds || marked() != want {
			t.Fatalf("%s: %v, marked %t; want succeeded %t, marked %t", what, err, marked(), succeeds, want)
		}
	}
	yes, no := true, false
	kubeDoor(t, ctx, p)
	f.TokenCallers.Store(false)
	f.PreviousGone.Store(&yes)
	runs("Kubernetes, previous_gone true, no token_callers: an older executor's answer", Egress{Via: "canyon"}, true, false)
	f.TokenCallers.Store(true)
	f.PreviousGone.Store(nil)
	runs("Kubernetes, no previous_gone", Egress{Via: "canyon"}, true, false)
	f.PreviousGone.Store(&no)
	runs("Kubernetes, previous_gone false", Egress{Via: "canyon"}, true, false)
	f.PreviousGone.Store(&yes)
	f.FailRun = true
	runs("Kubernetes, a failed /run", Egress{Via: "canyon"}, false, false)
	f.FailRun = false
	runs("Kubernetes, previous_gone true, without egress", Egress{Via: "off"}, true, false)
	runs("Kubernetes, previous_gone true", Egress{Via: "canyon"}, true, true)
}
