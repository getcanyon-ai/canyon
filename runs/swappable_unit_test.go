package runs

import (
	"context"
	"sync"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/policy"
)

// A read set back to nil between a site's check and its call never makes the call nil: each site loads the read
// once, into a local (#2667, the review of #2666). A test's cleanup stores nil on the shared pipe; production
// never does.
func TestAReadSetBackToNilNeverPanicsMidCall(t *testing.T) {
	p := &Pipeline{}
	read := func(context.Context, Querier, string) (policy.Snapshot, error) { return policy.Snapshot{}, nil }
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				p.ProjectPolicy.Store(read)
				p.ProjectPolicy.Store(nil)
			}
		}
	})
	defer func() { close(stop); wg.Wait() }()
	for range 100_000 {
		if _, err := p.readsApp(t.Context(), "claims-portal"); err != nil {
			t.Fatal(err)
		}
	}
}
