package main

import (
	"cmp"
	"context"
	"net/http"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// routesSkill registers the skill manifest an agent reads before it builds (contracts/routes.md
// "GET /api/v1/skill/manifest"). Everything in the answer is derived, fresh on every request: the
// configured public URL, the org's preferences (readPreferences, one read), and the in-memory
// snapshot — so there is nothing to invalidate and no cache to go stale after a PUT.
func routesSkill(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /skill/manifest", a.skillManifest)
}

// policy is the manifest's own shape for a snapshot rule — {id, name, state, production?}, never the
// internal producer and never a description: nothing a rule carries beyond these is fit for an agent.
// production is the rule's state before production, only where it is placed there.
type policy struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	Production   string `json:"production,omitempty"`
	How          string `json:"how,omitempty"`          // how to pass a Canyon rule, on one that blocks in either gate alone
	Instructions string `json:"instructions,omitempty"` // a custom rule's text, on one that blocks in either gate alone
	Says         string `json:"says,omitempty"`         // beside it, the condition Canyon checks by code, on a rule checked so
}

// manifestDesignSystem is the manifest's own shape for the org default design system: what an
// agent needs to resolve the package, never the registry row's id — an agent names a registry,
// not a row.
type manifestDesignSystem struct {
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	RegistryURL string  `json:"registry_url"`
	TokensURL   *string `json:"tokens_url,omitempty"`
}

// manifest is the skill route's own answer shape, closed by construction: a field added to
// gatepolicy.Rule or to preferences' resolved design system does not leak into it.
type manifest struct {
	Host           string                `json:"host"`
	Policies       []policy              `json:"policies"`
	ComponentTypes []string              `json:"component_types"`
	Stacks         []stack               `json:"stacks"`
	DesignSystem   *manifestDesignSystem `json:"design_system"`
	ProdApproval   bool                  `json:"prod_approval"` // an agent is told whether a promotion it asks for waits for a person
	AppAdditions   []manifestAddition    `json:"app_additions,omitempty"`
	Playbooks      []manifestPlaybook    `json:"playbooks"` // every playbook that stands, never a body (design/specs/mcp.md §3.3)
}

// manifestPlaybook is one playbook as an agent is handed it: by slug, with when to read it, never its body, so the
// session hook's context stays small.
type manifestPlaybook struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	When    string `json:"when"`
	Version int    `json:"version"`
}

// manifestAddition is one of an app's additions as the manifest hands it to an agent: the rule, its
// name, the app's own state in each gate it names, and who set it (contracts/routes.md).
type manifestAddition struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	State        gatepolicy.State `json:"state,omitempty"`
	Production   gatepolicy.State `json:"production,omitempty"`
	SetBy        actor            `json:"set_by"`
	How          string           `json:"how,omitempty"`          // how to pass a Canyon rule, where the addition blocks
	Instructions string           `json:"instructions,omitempty"` // a custom rule's text, where the addition blocks
	Says         string           `json:"says,omitempty"`         // beside it, the condition Canyon checks by code, on a rule checked so
}

// toMeet is what an agent is handed on meeting a rule in those states, where it blocks in either gate:
// a Canyon rule's line on how to pass it, a custom rule's text as its admin wrote it and, on one
// checked by code, the condition Canyon checks. A rule that only warns is handed by name alone
// (contracts/routes.md, the skill/manifest row; #2622, #2639).
func toMeet(rule gatepolicy.Rule, state, production gatepolicy.State) (how, instructions, says string) {
	if state != gatepolicy.Blocking && production != gatepolicy.Blocking {
		return "", "", ""
	}
	if rule.Check != nil {
		says = rule.Check.Says
	}
	return gatepolicy.HowToPass(rule.ID), rule.Instructions, says
}

// manifestAdditions is ?app='s additions in the snapshot's order, those GET /policies shows the caller:
// none for an app the caller is not on the team of, an unknown or deleted one, or no app at all — never
// a refusal, because a session hook reads this route. Each carries a gate's state only where it is
// stricter than the org's there now, and one the org has since caught up with in both is left out:
// an agent is told what the app adds, never a rule that is the org's anyway.
func manifestAdditions(ctx context.Context, q querier, rules []gatepolicy.Rule, app string) ([]manifestAddition, error) {
	if app == "" {
		return nil, nil
	}
	visible, err := visibleAdditions(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []manifestAddition
	for _, rule := range rules {
		for _, add := range visible[rule.ID] {
			if add.Project != app {
				continue
			}
			state, production := add.State, add.Production
			if stricter(rule.State, state) == rule.State {
				state = ""
			}
			if org := cmp.Or(rule.Production, gatepolicy.Off); stricter(org, production) == org {
				production = ""
			}
			if state != "" || production != "" {
				how, text, says := toMeet(rule, state, production)
				out = append(out, manifestAddition{ID: rule.ID, Name: rule.Name, State: state, Production: production, SetBy: add.SetBy, How: how, Instructions: text, Says: says})
			}
		}
	}
	return out, nil
}

// manifestPolicies is the snapshot's rules as an agent is told them: a rule Off in both gates is never
// handed out — it gives an agent nothing to do, and a committed file must not list which guards are down.
func manifestPolicies(rules []gatepolicy.Rule) []policy {
	out := make([]policy, 0, len(rules))
	for _, rule := range rules {
		if !rule.On() {
			continue
		}
		how, text, says := toMeet(rule, rule.State, rule.Production)
		out = append(out, policy{ID: rule.ID, Name: rule.Name, State: string(rule.State), Production: string(rule.Production), How: how, Instructions: text, Says: says})
	}
	return out
}

func (a *app) skillManifest(w http.ResponseWriter, r *http.Request) {
	prefs, err := a.orgPreferences(r.Context(), a.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	snap, err := a.effectivePolicy(r.Context(), a.pool) // composed on every read, like the preferences beside it: an applied edit reaches the next agent to fetch this
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	adds, err := manifestAdditions(r.Context(), a.pool, snap.Rules, r.URL.Query().Get("app"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	standing, err := playbooksOf(r.Context(), a.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	a.recordSyncs(r.Context(), heldSkills(r.URL.Query().Get("held"), standing)) // the hook's skills that hold the newest version
	playbooks := make([]manifestPlaybook, 0, len(standing))
	for _, p := range standing {
		playbooks = append(playbooks, manifestPlaybook{Slug: p.Slug, Name: p.Name, Kind: p.Kind, When: p.When, Version: p.Version})
	}
	var ds *manifestDesignSystem
	if prefs.DesignSystem != nil {
		ds = &manifestDesignSystem{
			Name:        prefs.DesignSystem.Name,
			Namespace:   prefs.DesignSystem.Namespace,
			RegistryURL: prefs.DesignSystem.RegistryURL,
			TokensURL:   prefs.DesignSystem.TokensURL,
		}
	}
	writeJSON(w, http.StatusOK, manifest{
		Host:           a.public, // the configured public URL, never r.Host: this string is written into a builder's agent config
		Policies:       manifestPolicies(snap.Rules),
		ComponentTypes: a.snap.ComponentMenu,
		Stacks:         prefs.Stacks,
		DesignSystem:   ds,
		ProdApproval:   prefs.ProdApproval,
		AppAdditions:   adds,
		Playbooks:      playbooks,
	})
}
