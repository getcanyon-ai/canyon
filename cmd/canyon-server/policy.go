package main

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// effectivePolicy composes the snapshot the routes answer with: the boot snapshot and the tools an
// admin added (importedTools) with the run and settings of those Canyon runs (scannerEdits), every rule's state in each gate — before preview, and before
// production — replaced by its newest policy edit naming that gate — newest by when it was set — whose
// start has passed. Newest by set_at, never by
// effective_at: an edit scheduled with a grace and then superseded by a later decision must not
// wake up when its grace elapses. An edit with no grace
// is in effect by being written, never by comparing its instant with now(): the clock can step
// back past it. Composed per request, never cached: grace_days makes the effective state a
// function of the clock, so a cache would serve the old state past the instant a grace elapsed.
// One indexed read of at most two rows per rule, one per gate, and one hash. Only the rules the snapshot carries
// are read: a row for a rule a later release retired cannot loosen a gate, and reading it would
// fail every request instead. The version is the boot version plus the count of edits in effect (and of tools added with a grace), so every change to the effective
// states moves it exactly once — an elapsing grace included — and a snapshot composed later never
// carries an earlier number, except once on a host that had AI judgement edits, which it no longer counts. (Counting the newest edit's own number does not: an edit written
// after a graced one, but in effect before it, already carries the higher number.)
func (a *app) effectivePolicy(ctx context.Context, q querier) (gatepolicy.Snapshot, error) {
	base := a.boot
	// the stacks, the approved buildpacks and the default design system design.org-system reads are the
	// preferences row's (contracts/snapshot.md); unset until an admin sets them, so the boot hash stands
	var stacks []gatepolicy.Stack
	var approved []string
	var chosen bool
	var def gatepolicy.DesignSystem
	if err := q.QueryRow(ctx, `SELECT p.stacks, p.approved_buildpacks, d.id IS NOT NULL, COALESCE(d.name, ''), COALESCE(d.namespace, ''), COALESCE(d.registry_url, ''), COALESCE(d.tokens_url, '')
		FROM org_preferences p LEFT JOIN design_systems d ON d.id = p.design_system_id`).Scan(&stacks, &approved, &chosen, &def.Name, &def.Namespace, &def.RegistryURL, &def.TokensURL); err != nil {
		return gatepolicy.Snapshot{}, err
	}
	if len(approved) > 0 { // none leaves the boot's nil, which the snapshot omits
		base.ApprovedBuildpacks = approved
	}
	if chosen { // none leaves the boot's capabilities, whose org_default the snapshot omits
		base.Capabilities = maps.Clone(base.Capabilities) // the boot's map is shared: never written
		ds := base.Capabilities["design_systems"]
		ds.OrgDefault = &def
		base.Capabilities["design_systems"] = ds
	}
	if stacks != nil { // a jsonb null decodes to nil, which marshals as null: the boot's [] stands
		base.Stacks = stacks
	}
	tools, graced, err := importedTools(ctx, q, ruleIDs(a.boot))
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	customs, saved, err := customRules(ctx, q)
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	base.Rules = append(append(slices.Clone(a.boot.Rules), tools...), customs...)
	ran, err := scannerEdits(ctx, q, base.Rules)
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	// In each gate the newest edit naming it whose start has passed (contracts/snapshot.md, Two gates);
	// the count is of the edits in effect, each once whichever gates it names.
	rows, err := q.Query(ctx, `WITH e AS (SELECT * FROM policy_edits WHERE (grace_days = 0 OR effective_at <= now()) AND rule = ANY($1))
		(SELECT DISTINCT ON (rule) rule, false, state, (SELECT count(*) FROM e) FROM e WHERE names_state ORDER BY rule, set_at DESC, id DESC)
		UNION ALL (SELECT DISTINCT ON (rule) rule, true, production, (SELECT count(*) FROM e) FROM e WHERE production IS NOT NULL ORDER BY rule, set_at DESC, id DESC)`, ruleIDs(base))
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	defer rows.Close()
	states, placed, edits := map[string]gatepolicy.State{}, map[string]gatepolicy.State{}, 0
	for rows.Next() {
		var rule, state string
		var production bool
		if err := rows.Scan(&rule, &production, &state, &edits); err != nil {
			return gatepolicy.Snapshot{}, err
		}
		if !production {
			states[rule] = gatepolicy.State(state)
		} else if state != string(gatepolicy.Off) { // off takes the rule out of the gate: it carries no production
			placed[rule] = gatepolicy.State(state)
		}
	}
	if err := rows.Err(); err != nil {
		return gatepolicy.Snapshot{}, err
	}
	for i, r := range base.Rules { // base.Rules is this call's own slice
		base.Rules[i].State, base.Rules[i].Production = cmp.Or(states[r.ID], r.State), placed[r.ID]
	}
	s, err := gatepolicy.WithStates(reviewingCustomRules(base), states)
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	s.Version = base.Version + edits + graced + ran + saved
	return s, nil
}

// reviewingCustomRules is s with the agent that reviews an org's custom rules checked by AI, after
// canyon-review — the boot's one agent — on its model, while one of them is on (contracts/snapshot.md,
// Custom rules).
// s's rules are read as they stand, its agents never written: the result has its own list.
func reviewingCustomRules(s gatepolicy.Snapshot) gatepolicy.Snapshot {
	if !slices.ContainsFunc(s.Agents, func(g gatepolicy.Agent) bool { return g.ID == customRulesAgent }) &&
		slices.ContainsFunc(s.Rules, func(r gatepolicy.Rule) bool {
			return strings.HasPrefix(r.ID, "custom.") && r.On() && r.Producer == gatepolicy.LLMReview // one checked by code is the scanners' run's
		}) {
		s.Agents = append(slices.Clone(s.Agents), gatepolicy.Agent{ID: customRulesAgent, Namespace: []string{"custom"}, Model: s.Agents[0].Model})
	}
	return s
}

// addition is an app's addition to one rule (contracts/routes.md, PUT /projects/:id/policies/:rule):
// its state in each gate it names, "" for one it does not.
type addition struct {
	State, Production gatepolicy.State
}

// stricter is the stricter of two states, off < advisory < blocking; "" is no state, so the other.
func stricter(x, y gatepolicy.State) gatepolicy.State {
	rank := []gatepolicy.State{"", gatepolicy.Off, gatepolicy.Advisory, gatepolicy.Blocking}
	if slices.Index(rank, y) > slices.Index(rank, x) {
		return y
	}
	return x
}

// applied is the rule with the addition applied: in each gate the stricter of the rule's state and the
// addition's, production only where the rule is then placed before production.
func (add addition) applied(r gatepolicy.Rule) gatepolicy.Rule {
	r.State = stricter(r.State, add.State)
	if r.Production = stricter(r.Production, add.Production); r.Production == gatepolicy.Off {
		r.Production = ""
	}
	return r
}

// projectPolicy is the snapshot a project's runs use, its effective snapshot (contracts/snapshot.md):
// the org's with the app's additions applied, carrying the org's version and its own hash, so the
// additions are in its verdict cache's key. A project with none runs the org's snapshot.
func (a *app) projectPolicy(ctx context.Context, q querier, project string) (gatepolicy.Snapshot, error) {
	org, err := a.effectivePolicy(ctx, q)
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	adds, err := additionsOf(ctx, q, project)
	if err != nil || len(adds) == 0 {
		return org, err
	}
	s := org
	s.Rules = slices.Clone(org.Rules)
	for i, r := range s.Rules {
		s.Rules[i] = adds[r.ID].applied(r)
	}
	if s, err = gatepolicy.WithStates(reviewingCustomRules(s), nil); err != nil {
		return gatepolicy.Snapshot{}, err
	}
	s.Version = org.Version
	return s, nil
}

// additionsOf is the project's additions by rule.
func additionsOf(ctx context.Context, q querier, project string) (map[string]addition, error) {
	rows, err := q.Query(ctx, `SELECT rule, COALESCE(state, ''), COALESCE(production, '') FROM app_policy_additions WHERE project_id = $1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]addition{}
	for rows.Next() {
		var rule string
		var add addition
		if err := rows.Scan(&rule, &add.State, &add.Production); err != nil {
			return nil, err
		}
		out[rule] = add
	}
	return out, rows.Err()
}

// customRulesAgent is the agent that reviews an org's custom rules, in one call per review.
const customRulesAgent = "custom-rules"

// customRules is every custom rule the org has saved, at its newest version, installed off with its
// name, instructions and check, its producer the check's: a scanner for one checked by code
// (contracts/snapshot.md, Custom rules), sorted by id bytewise as importedTools sorts; saved counts every
// version, each save a change that moves the version once.
func customRules(ctx context.Context, q querier) ([]gatepolicy.Rule, int, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (rule) rule, name, instructions, check_by, coalesce(check_expr, ''), coalesce(check_says, ''), check_facts_version, count(*) OVER ()
		FROM custom_rule_edits ORDER BY rule, version DESC`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var rules []gatepolicy.Rule
	saved := 0
	for rows.Next() {
		r := gatepolicy.Rule{State: gatepolicy.Off, Producer: gatepolicy.LLMReview, Check: &gatepolicy.Check{}}
		if err := rows.Scan(&r.ID, &r.Name, &r.Instructions, &r.Check.By, &r.Check.Expr, &r.Check.Says, &r.Check.FactsVersion, &saved); err != nil {
			return nil, 0, err
		}
		if r.Check.By == custom.ByCode {
			r.Producer = gatepolicy.Scanner
		}
		rules = append(rules, r)
	}
	slices.SortFunc(rules, func(x, y gatepolicy.Rule) int { return strings.Compare(x.ID, y.ID) })
	return rules, saved, rows.Err()
}

// importedTools is a sarif.<tool> rule for every tool an admin has added — every one with an edit
// row, adding and the first edit being one step (contracts/snapshot.md), but the tools the boot
// snapshot already carries, Canyon's defaults, whose edits are edits like any rule's — installed off with the
// contract's name, sorted by id as Build sorts registered rules: bytewise, never by the database's
// collation, so neither the order tools were added in nor the host's locale moves the hash. graced
// counts the tools added with a grace: adding one moves the hash at once, before its first edit is
// in effect and counted, so the version moves then too.
func importedTools(ctx context.Context, q querier, boot []string) ([]gatepolicy.Rule, int, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (rule) rule, grace_days > 0 FROM policy_edits WHERE starts_with(rule, 'sarif.') AND NOT rule = ANY($1) ORDER BY rule, set_at, id`, boot)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var tools []gatepolicy.Rule
	graced := 0
	for rows.Next() {
		var id string
		var ahead bool
		if err := rows.Scan(&id, &ahead); err != nil {
			return nil, 0, err
		}
		tools = append(tools, gatepolicy.Rule{ID: id, Name: "Findings from " + strings.TrimPrefix(id, "sarif."), State: gatepolicy.Off, Producer: gatepolicy.Sarif})
		if ahead {
			graced++
		}
	}
	slices.SortFunc(tools, func(x, y gatepolicy.Rule) int { return strings.Compare(x.ID, y.ID) })
	return tools, graced, rows.Err()
}

// scannerEdits sets each Canyon-run tool's run and settings in rules — a slice the caller owns — from
// its newest scanner edit, in effect once written, and answers the count of those edits:
// each is a change (an apply that sets what is set writes none), so the version moves once per change.
// Only the rules the snapshot carries are read, as for states.
func scannerEdits(ctx context.Context, q querier, rules []gatepolicy.Rule) (int, error) {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (rule) rule, run, settings, count(*) OVER () FROM scanner_edits WHERE rule = ANY($1) ORDER BY rule, id DESC`, ids)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	changes := 0
	for rows.Next() {
		var r gatepolicy.Rule
		if err := rows.Scan(&r.ID, &r.Run, &r.Settings, &changes); err != nil {
			return 0, err
		}
		i := slices.IndexFunc(rules, func(x gatepolicy.Rule) bool { return x.ID == r.ID })
		rules[i].Run, rules[i].Settings = r.Run, r.Settings
	}
	return changes, rows.Err()
}

// policyEdit is one row of policy_edits as GET /policies reads it: the gates it names — "" for one it
// does not — and the scheduled change when its effective_at is still ahead. Ahead is the database's
// judgement, the clock the composer reads: a second clock would show an edit both in effect and still
// scheduled.
type policyEdit struct {
	State       gatepolicy.State
	Production  gatepolicy.State
	GraceDays   int
	EffectiveAt time.Time
	Ahead       bool
	SetBy       actor
	SetAt       time.Time
}

// ruleEdits is a rule's edits as GET /policies reads them: the newest, whatever its effective_at — the
// one whose actor, instant and grace the row shows — and in each gate the newest naming it, the one a
// scheduled change in that gate is. A gate no edit names is the zero edit.
type ruleEdits struct {
	newest, preview, production policyEdit
}

// ruleIDs is the rule ids a snapshot carries: what the two reads are scoped to.
func ruleIDs(s gatepolicy.Snapshot) []string {
	ids := make([]string, 0, len(s.Rules))
	for _, r := range s.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

// newestEdits reads each rule's edits, by the instant they were set, over the rules named — the same
// row sets the composer reads, per gate.
func newestEdits(ctx context.Context, q querier, rules []string) (map[string]ruleEdits, error) {
	const cols = `rule, CASE WHEN names_state THEN state END, production, grace_days, effective_at, grace_days > 0 AND effective_at > now(), set_by_kind, set_by_subject, set_by_display, set_at`
	rows, err := q.Query(ctx, `(SELECT DISTINCT ON (rule) 0, `+cols+` FROM policy_edits WHERE rule = ANY($1) ORDER BY rule, set_at DESC, id DESC)
		UNION ALL (SELECT DISTINCT ON (rule) 1, `+cols+` FROM policy_edits WHERE rule = ANY($1) AND names_state ORDER BY rule, set_at DESC, id DESC)
		UNION ALL (SELECT DISTINCT ON (rule) 2, `+cols+` FROM policy_edits WHERE rule = ANY($1) AND production IS NOT NULL ORDER BY rule, set_at DESC, id DESC)`, rules)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ruleEdits{}
	for rows.Next() {
		var which int
		var rule string
		var state, production pgtype.Text // NULL for a gate the edit does not name
		var e policyEdit
		if err := rows.Scan(&which, &rule, &state, &production, &e.GraceDays, &e.EffectiveAt, &e.Ahead, &e.SetBy.Kind, &e.SetBy.Subject, &e.SetBy.Display, &e.SetAt); err != nil {
			return nil, err
		}
		e.State, e.Production = gatepolicy.State(state.String), gatepolicy.State(production.String)
		edits := out[rule]
		switch which {
		case 0:
			edits.newest = e
		case 1:
			edits.preview = e
		default:
			edits.production = e
		}
		out[rule] = edits
	}
	return out, rows.Err()
}
