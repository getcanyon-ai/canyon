package main

import (
	"context"
	"slices"
	"strings"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// statusNeeds is the app's needs on the status read (contracts/routes.md, the status row): what needsFrom reads of it,
// from the states its read asked the executor, and to its team the receipt's two kinds, read from the receipt's own
// input, so no screen parses a row's why.
func (a *app) statusNeeds(ctx context.Context, read receiptRead) ([]need, error) {
	p := read.project
	facts, err := needFactsOf(ctx, a.pool, p.ID)
	if err != nil {
		return nil, err
	}
	states := map[string]map[string]string{p.ID: {}}
	for env, e := range p.Envs {
		if m, ok := e.(map[string]any); ok {
			states[p.ID][env], _ = m["state"].(string)
		}
	}
	byApp, err := a.needsFrom(ctx, a.pool, facts, states)
	if err != nil {
		return nil, err
	}
	needs := byApp[p.ID]
	if p.team {
		admins, err := a.adminNames(ctx, a.pool)
		if err != nil {
			return nil, err
		}
		needs = append(needs, receiptNeeds(read.in, admins)...)
		sortNeeds(needs)
	}
	if needs == nil {
		needs = []need{}
	}
	return needs, nil
}

// receiptNeeds is the receipt's two kinds (contracts/routes.md, *What needs a person*): blocked_prod while
// prod.clean-gate, blocking, refuses a review clean before preview for findings that block before production alone,
// and will_refuse while prod.egress-allowed, on, names what production doesn't allow of what preview connected to.
func receiptNeeds(in receiptInput, admins []askee) []need {
	state := func(id string) gatepolicy.State {
		if i := slices.IndexFunc(in.rules, func(r gatepolicy.Rule) bool { return r.ID == id }); i >= 0 {
			return in.rules[i].State
		}
		return gatepolicy.Off
	}
	var out []need
	if r := in.run; r != nil && !r.rollback && state("prod.clean-gate") == gatepolicy.Blocking && in.unapproved == 0 && (r.state == "clean" || r.state == "blocked") {
		if production, uncounted := placedOpen(in.placed); production > 0 || uncounted {
			var count any = production
			if uncounted {
				count = nil
			}
			n := newNeed("blocked_prod", prodNeedEnv, nil, true, map[string]any{"version": r.pkg.version(), "count": count})
			n.ask = admins
			out = append(out, n)
		}
	}
	// prod.egress-allowed blocking and could not check: the promotion it blocks has a need, with its row's fix
	if e := in.egress; e != nil && e.unread != "" && !e.orgRuns && !e.rollback && in.preview != nil && state(egressRule) == gatepolicy.Blocking {
		fix := "review"
		if e.unread == egressDirect || e.unread == egressOpenNetwork {
			fix = "host"
		}
		n := newNeed("unchecked", prodNeedEnv, nil, true, map[string]any{"version": in.preview.version(), "count": 1,
			"checks": []map[string]any{{"tool": egressRule, "reason": strings.TrimPrefix(e.unread, "could not check: "), "fix": fix}}})
		n.ask = admins
		out = append(out, n)
	}
	if e := in.egress; e != nil && len(e.refused) > 0 && in.preview != nil && state(egressRule) != gatepolicy.Off {
		denied := make([]map[string]any, len(e.refused))
		for i, row := range e.refused {
			denied[i] = map[string]any{"destination": row.destination, "port": row.port, "allowable": row.destination != "invalid" && codeRefusal(row.destination, row.port) == ""}
		}
		out = append(out, newNeed("will_refuse", prodNeedEnv, nil, true, map[string]any{"version": in.preview.version(), "blocks": state(egressRule) == gatepolicy.Blocking, "denied": denied}))
	}
	return out
}
