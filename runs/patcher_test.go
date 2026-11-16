package runs

import (
	"context"
	"testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// testPatcher is the tests' pipeline's Patcher, as main wires the server's: Canyon Enterprise's, which
// ee_patching_test.go sets in a build with it, nil in a build without it. This package cannot import ee/, which
// imports it, so a test package of its own sets it before TestMain runs.
var testPatcher Patcher

// SetTestPatcher sets testPatcher, for ee_patching_test.go.
func SetTestPatcher(p Patcher) { testPatcher = p }

// A server built without Canyon Enterprise has no Patcher, and reads patching as off whatever the org stores: a tick
// on a server set to patch, with an executor, an image read and a policy, plans no patch run, and the identities read
// says the org's patching is off (contracts/routes.md, Licence, A server built without Canyon Enterprise).
func TestWithoutAPatcherATickPlansNothing(t *testing.T) {
	p, ctx := seeded(t)
	patcherWas, executorWas, readWas := pipe.Patcher, pipe.Executor, pipe.PlatformRead.Load()
	t.Cleanup(func() {
		pipe.Patcher, pipe.Executor = patcherWas, executorWas
		pipe.Patches.Store(false)
		pipe.PlatformRead.Store(readWas)
	})
	pipe.Patcher, pipe.Executor = nil, &executor.Client{Base: "http://127.0.0.1:1"} // never asked: the plan stops before GET /builder
	pipe.Patches.Store(true)
	pipe.PlatformRead.Store(func(context.Context, string, string) (PlatformImage, error) { return PlatformImage{}, nil })
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET patching = 'ask', rebuilds = 'ask'"); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context, Querier) (bool, error){pipe.PatchingOn, pipe.RebuildsOn} {
		if on, err := read(ctx, p); err != nil || on {
			t.Fatalf("patching on without a Patcher: %t (%v)", on, err)
		}
	}
	if on, err := pipe.Patching(ctx, p, project); err != nil || on {
		t.Fatalf("an app patched without a Patcher: %t (%v)", on, err)
	}
	if n, err := pipe.planPatches(ctx); err != nil || n != 0 {
		t.Fatalf("a tick without a Patcher: %d started (%v)", n, err)
	}
	var why string
	if err := p.QueryRow(ctx, "SELECT why FROM patch_plan_said WHERE identity IS NULL AND apps = '{}'").Scan(&why); err != nil || why != "org_off" {
		t.Fatalf("the plan's cause: %q (%v), want org_off", why, err)
	}
}
