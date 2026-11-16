package runs

import (
	"context"

	"github.com/getcanyon-ai/canyon/executor"
)

// Inject is what a /run carries of Canyon Enterprise's outbound keys (design/EGRESS.md §4.1, §4.4; contracts/executor.md,
// egress.ca; #2922). The Pipeline's is nil in a build without Canyon Enterprise: no placeholder, and no root.
type Inject interface {
	// Placeholders gives values canyon-adds-the-real-key under each placeholder variable a route of app env names, unless it
	// holds a value of that name.
	Placeholders(ctx context.Context, q Querier, app, env string, values map[string]string) error
	// Roots is a /run's egress.ca and the fingerprints of the roots it carries, none of either recorded as an empty
	// list.
	Roots(ctx context.Context, q Querier, app, env string, values map[string]string) ([]executor.Root, []string, error)
}

// placeholders is Inject's, none without it.
func (p *Pipeline) placeholders(ctx context.Context, app, env string, values map[string]string) error {
	if p.Inject == nil {
		return nil
	}
	return p.Inject.Placeholders(ctx, p.Pool, app, env, values)
}

// egressCA is Inject's roots, none carried and none recorded without it, as with credential routes off.
func (p *Pipeline) egressCA(ctx context.Context, app, env string, values map[string]string) ([]executor.Root, []string, error) {
	if p.Inject == nil {
		return nil, []string{}, nil
	}
	return p.Inject.Roots(ctx, p.Pool, app, env, values)
}
