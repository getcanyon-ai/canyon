package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
)

// routesPolicies registers the org's policies (contracts/routes.md, the policies row): the list
// the dashboard's Policies screen reads, the preview a dropdown change asks for, and the apply
// an admin's click lands.
func routesPolicies(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /policies", a.getPolicies)
	api.HandleFunc("PUT /policies/{rule}", a.adminOnly(personOnly("only a person can change a policy", a.policyInDashboard(a.putPolicy))))
}

// policyInDashboard refuses a policy change, preview or apply, or a custom rule's, from any session but a
// browser's while production is approved in the dashboard (contracts/routes.md, the policies and custom-rules
// rows; #1966): an admin's agent replaying the CLI's session never turns a blocking rule off, or has it check
// nothing, and promotes past it. It names the Policies page, where both are changed.
func (a *app) policyInDashboard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if fromDashboard(ctx) {
			next(w, r)
			return
		}
		on, err := a.prodApproval(ctx, a.pool)
		if err != nil {
			a.log.Printf("canyon-server: production approval: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if !on {
			next(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "dashboard_required", "While production is approved in Canyon's dashboard, policies are changed there too: "+a.public+"/#/guardrails/policies", nil)
		deny(ctx, "dashboard_required", "")
	}
}

// personOnly refuses an agent or canyon actor 403 person_required, before anything is read: no CLI verb
// changes the pipeline, and an agent under a person's session is an agent (design/PERMISSIONS.md, The
// gate pipeline).
func personOnly(message string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if actorOf(r.Context()).Kind != "person" {
			writeErr(w, http.StatusForbidden, "person_required", message, nil)
			deny(r.Context(), "person_required", r.PathValue("id")) // a project's route names it as id; an org route names none
			return
		}
		next(w, r)
	}
}

// policyRow is one rule as GET /policies answers it: the effective state now in each gate —
// production only where the rule is placed before production — and the newest edit's grace, actor and
// instant when the rule has ever been edited. scheduled and scheduled_production are each gate's change
// whose start is still ahead — the state columns keep the old states until then.
type policyRow struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	State               gatepolicy.State `json:"state"`
	Production          gatepolicy.State `json:"production,omitempty"`
	GraceDays           *int             `json:"grace_days,omitempty"`
	Scheduled           *scheduledEdit   `json:"scheduled,omitempty"` // before preview; a go-live rule's only gate
	ScheduledProduction *scheduledEdit   `json:"scheduled_production,omitempty"`
	SetBy               *actor           `json:"set_by,omitempty"`
	SetAt               *string          `json:"set_at,omitempty"` // RFC 3339 at whole seconds UTC, as every read answers an instant
	Additions           []appAddition    `json:"additions,omitempty"`

	// A Canyon-run tool's, each only when set, as the snapshot carries them.
	Run      bool                `json:"run,omitempty"`
	Settings gatepolicy.Settings `json:"settings,omitzero"`
}

// appAddition is one app's addition to a rule as GET /policies answers it (contracts/routes.md).
type appAddition struct {
	Project    string           `json:"project"`
	State      gatepolicy.State `json:"state,omitempty"`
	Production gatepolicy.State `json:"production,omitempty"`
	SetBy      actor            `json:"set_by"`
	SetAt      string           `json:"set_at"`
}

// scheduledEdit is a gate's edit that has not started: the state it sets there, and when.
type scheduledEdit struct {
	State       gatepolicy.State `json:"state"`
	EffectiveAt string           `json:"effective_at"`
}

// importedID is an imported tool's rule id: sarif. and the tool in normalised form (finding.md).
var importedID = regexp.MustCompile(`^sarif\.[a-z0-9]+(-[a-z0-9]+)*$`)

// isImported is whether rule names an imported tool an apply may add: its tool 1 to 40 characters
// (contracts/routes.md, the policies row).
func isImported(rule string) bool {
	return importedID.MatchString(rule) && len(rule) <= len("sarif.")+40
}

// ranTools are the tools Canyon runs itself, each with the settings it takes (routes.md, the policies
// row): run and settings are offered on these five rules only.
var ranTools = map[string][]string{
	"sarif.trivy":    {"scanners", "skip_paths"},
	"sarif.opengrep": {"rules", "skip_paths"},
	"sarif.eslint":   {"skip_paths"},
	"sarif.grype":    {"skip_paths"},
	"sarif.checkov":  {"frameworks", "skip_checks", "skip_paths"},
}

// sourceRule takes settings and no run: its source, the imported tool whose image reports it reads (design/PATCHING.md
// §3.4), "trivy" its own matches.
const sourceRule = platformRule

// The executor's value rules (executor/scan.go: validValues and its patterns), mirrored exactly — a
// setting the server took and the executor refused would fail every scan asked with it — and opengrep's
// rules repository as contracts/schema/policies.schema.json has it, which the server fetches itself,
// with no control byte anywhere: the URL is shown on the Policies screen and the audit trail.
var (
	skipPathRe = regexp.MustCompile(`^([^-./\\\x00-\x1F][^/\\\x00-\x1F]*|\.[^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.\.[^/\\\x00-\x1F]+)(/([^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.[^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.\.[^/\\\x00-\x1F]+))*$`)
	checkIDRe  = regexp.MustCompile(`^CKV2?_[A-Z0-9_]+$`)
	repoRe     = regexp.MustCompile(`^https://[^@/?#\s\x00-\x1F\x7F]+(/[^?#\s\x00-\x1F\x7F]*)?$`)
	commitRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// listSettings is each list setting's rule — at most so many distinct values, empty or not, none of
// them bad — and the words that state it.
var listSettings = map[string]struct {
	most     int
	nonEmpty bool
	bad      func(string) bool
	want     string
}{
	"scanners":    {3, true, func(v string) bool { return !slices.Contains([]string{"vuln", "misconfig", "secret"}, v) }, "a non-empty set of vuln, misconfig and secret"},
	"frameworks":  {3, true, func(v string) bool { return !slices.Contains([]string{"terraform", "cloudformation", "dockerfile"}, v) }, "a non-empty set of terraform, cloudformation and dockerfile"},
	"skip_checks": {256, false, func(v string) bool { return len(v) > 64 || !checkIDRe.MatchString(v) }, "at most 256 distinct checkov ids (CKV_ or CKV2_), each at most 64 characters"},
	"skip_paths":  {64, false, func(v string) bool { return utf8.RuneCountInString(v) > 256 || !skipPathRe.MatchString(v) }, "at most 64 distinct paths of the package, each at most 256 characters, never absolute, never through .. and never starting with -"},
}

// toolSettings reads a PUT's settings for a Canyon-run tool: an object of the tool's own keys, each
// once and each value in the shape the executor takes, the error naming the first that fails in the
// body's order. An empty list is dropped, so each setting has one spelling and none at all is the zero value.
func toolSettings(rule string, raw json.RawMessage) (gatepolicy.Settings, error) {
	var s gatepolicy.Settings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // opengrep's rules are repo, commit and paths, nothing else
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return s, errors.New("settings is an object")
	}
	lists := map[string]*[]string{"scanners": &s.Scanners, "frameworks": &s.Frameworks, "skip_checks": &s.SkipChecks, "skip_paths": &s.SkipPaths}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		key, _ := t.(string)
		switch {
		case err != nil:
			return s, errors.New("settings is an object")
		case !slices.Contains(ranTools[rule], key) && (rule != sourceRule || key != "source"):
			return s, fmt.Errorf("settings %q: %s takes %s", key, strings.TrimPrefix(rule, "sarif."), strings.Join(ranTools[rule], ", "))
		case seen[key]:
			return s, fmt.Errorf("settings %q is named twice", key)
		}
		seen[key] = true
		if key == "source" { // a tool's name as its rule id carries it; trivy's own is no source at all, so it has one spelling
			if dec.Decode(&s.Source) != nil || s.Source == "" || len(s.Source) > 40 || !importedID.MatchString("sarif."+s.Source) {
				return s, errors.New(`settings "source" is trivy or the name of a tool added to Canyon`)
			}
			if s.Source == "trivy" {
				s.Source = ""
			}
			continue
		}
		if key == "rules" {
			paths := listSettings["skip_paths"] // the repo's directories, in a package path's form
			if dec.Decode(&s.Rules) != nil || s.Rules == nil || utf8.RuneCountInString(s.Rules.Repo) > 2048 || !repoRe.MatchString(s.Rules.Repo) || !commitRe.MatchString(s.Rules.Commit) ||
				len(s.Rules.Paths) > paths.most || len(slices.Compact(slices.Sorted(slices.Values(s.Rules.Paths)))) != len(s.Rules.Paths) || slices.ContainsFunc(s.Rules.Paths, paths.bad) {
				return s, errors.New(`settings "rules" is {repo, commit, paths?}: an https:// URL, a full 40-hex commit sha and at most 64 distinct directories of the repo, never absolute, never through .. and never starting with -`)
			}
			if slices.Sort(s.Rules.Paths); len(s.Rules.Paths) == 0 { // one spelling, so one snapshot hash
				s.Rules.Paths = nil
			}
			continue
		}
		list, want := lists[key], listSettings[key]
		if dec.Decode(list) != nil || *list == nil || len(*list) > want.most || want.nonEmpty && len(*list) == 0 ||
			len(slices.Compact(slices.Sorted(slices.Values(*list)))) != len(*list) || slices.ContainsFunc(*list, want.bad) {
			return s, fmt.Errorf("settings %q is %s", key, want.want)
		}
		if len(*list) == 0 {
			*list = nil
		}
	}
	return s, nil
}

// policyList is the GET shape: every rule the composed snapshot carries, in its order, with the
// newest edit's fields on the rules that have one. Both reads run on q inside one transaction, so
// now() is one instant for both: an edit whose start fell between two statements would show its old
// state and no scheduled key at once.
type policyList struct {
	Policies []policyRow `json:"policies"`
}

func (a *app) policyList(ctx context.Context, q querier) (policyList, error) {
	snap, err := a.effectivePolicy(ctx, q)
	if err != nil {
		return policyList{}, err
	}
	edits, err := newestEdits(ctx, q, ruleIDs(snap))
	if err != nil {
		return policyList{}, err
	}
	adds, err := visibleAdditions(ctx, q)
	if err != nil {
		return policyList{}, err
	}
	out := policyList{Policies: make([]policyRow, 0, len(snap.Rules))}
	for _, rule := range snap.Rules {
		row := policyRow{ID: rule.ID, Name: rule.Name, State: rule.State, Production: rule.Production, Run: rule.Run, Settings: rule.Settings}
		if e, ok := edits[rule.ID]; ok {
			at := e.newest.SetAt.UTC().Format(time.RFC3339)
			row.GraceDays, row.SetBy, row.SetAt = &e.newest.GraceDays, &e.newest.SetBy, &at
			row.Scheduled, row.ScheduledProduction = pending(e.preview, e.preview.State), pending(e.production, e.production.Production)
		}
		row.Additions = adds[rule.ID]
		out.Policies = append(out.Policies, row)
	}
	return out, nil
}

// visibleAdditions is the live apps' additions by rule, each rule's by project, of the apps the caller
// is contributor or above on — an admin's: every app's (design/PERMISSIONS.md, The gate pipeline).
func visibleAdditions(ctx context.Context, q querier) (map[string][]appAddition, error) {
	apps, err := teamApps(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT d.rule, d.project_id, COALESCE(d.state, ''), COALESCE(d.production, ''), d.set_by_kind, d.set_by_subject, d.set_by_display, d.set_at
		FROM app_policy_additions d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL
		WHERE $1::text[] IS NULL OR d.project_id = ANY($1) ORDER BY d.rule, d.project_id COLLATE "C"`, apps)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]appAddition{}
	for rows.Next() {
		var rule string
		var d appAddition
		var at time.Time
		if err := rows.Scan(&rule, &d.Project, &d.State, &d.Production, &d.SetBy.Kind, &d.SetBy.Subject, &d.SetBy.Display, &at); err != nil {
			return nil, err
		}
		d.SetAt = at.UTC().Format(time.RFC3339)
		out[rule] = append(out[rule], d)
	}
	return out, rows.Err()
}

// pending is a gate's newest edit while its start is still ahead, nil when it has started: state is
// what the edit sets in that gate.
func pending(e policyEdit, state gatepolicy.State) *scheduledEdit {
	if !e.Ahead {
		return nil
	}
	return &scheduledEdit{State: state, EffectiveAt: e.EffectiveAt.UTC().Format(time.RFC3339)}
}

// rowOf is the rule's row in l.
func rowOf(l policyList, rule string) (policyRow, bool) {
	i := slices.IndexFunc(l.Policies, func(p policyRow) bool { return p.ID == rule })
	if i < 0 {
		return policyRow{}, false
	}
	return l.Policies[i], true
}

func (a *app) getPolicies(w http.ResponseWriter, r *http.Request) {
	tx, err := a.pool.BeginTx(r.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	list, err := a.policyList(r.Context(), tx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// blocked is one project the preview names: the count of findings under the rule in its latest
// verdict that would block at the target state.
type blocked struct {
	Project  string `json:"project"`
	Findings int    `json:"findings"`
}

// policyPut is a PUT's checked body: "" for a gate it does not set, nil for a run or settings it
// leaves as they are; settings are replaced whole, and the zero value clears them.
type policyPut struct {
	state      gatepolicy.State // before preview; a go-live rule's before production, its only gate
	production gatepolicy.State // before production; off takes the rule out of that gate
	grace      int
	run        *bool
	settings   *gatepolicy.Settings
	addition   bool // an app's addition, answerAddition's: it replaces the one the app holds
}

// putPolicy answers the preview: the projects a change to this rule would block today, computed
// at the target state, and nothing written; with `?apply=true` it writes the edit and answers the
// rule beside the same preview. The rule is checked against the boot snapshot — the set of rule ids
// is the release's, edits move states only — or is an imported tool's, which an apply adds; the
// refusals are answered before any read but whether a tool is added, a 404 before any 409.
func (a *app) putPolicy(w http.ResponseWriter, r *http.Request) {
	rule := r.PathValue("rule")
	custom := strings.HasPrefix(rule, "custom.")
	if custom { // a custom rule exists once saved (contracts/snapshot.md, Custom rules)
		if err := a.pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM custom_rule_edits WHERE rule = $1)", rule).Scan(&custom); err != nil {
			a.log.Printf("canyon-server: put policy %s: %v", rule, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	if !custom && !isImported(rule) && !slices.ContainsFunc(a.boot.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule }) {
		writeErr(w, http.StatusNotFound, "not_found", "no policy carries that id", nil)
		return
	}
	var body struct {
		State      *gatepolicy.State `json:"state"`
		Production *gatepolicy.State `json:"production"`
		GraceDays  json.RawMessage   `json:"grace_days"` // absent is 0; anything but an integer 0–365 with a state is refused
		Run        *bool             `json:"run"`
		Settings   json.RawMessage   `json:"settings"` // null included: toolSettings refuses anything but an object
	}
	// A key this route does not take is refused, never ignored: run on a tool once answered 200 and did nothing.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is one JSON object of state, production, grace_days, run and settings", nil)
		return
	}
	put := policyPut{run: body.Run}
	if body.State != nil {
		put.state = *body.State
	}
	if body.Production != nil {
		put.production = *body.Production
	}
	state := put.state
	scanner := body.Run != nil || body.Settings != nil
	switch {
	case scanner && ranTools[rule] == nil && (rule != sourceRule || body.Run != nil):
		writeErr(w, http.StatusConflict, "run_not_offered", "Run and settings are offered on trivy, opengrep, eslint, grype and checkov only, and settings on platform.known-vuln", nil)
		return
	case body.Production != nil && slices.Contains(promoteRuleIDs, rule): // a go-live rule's state is before production, its only gate
		writeErr(w, http.StatusConflict, "production_not_offered", productionNotOffered, nil)
		return
	case body.State == nil && body.Production == nil && !scanner, !isState(body.State), !isState(body.Production):
		writeErr(w, http.StatusBadRequest, "invalid_state", "state and production are each off, advisory or blocking", nil)
		return
	case custom && !a.reviews && (slices.Contains(onStates, state) || slices.Contains(onStates, put.production)):
		writeErr(w, http.StatusConflict, "no_model", noModel, nil) // the preview and the apply alike: a dial that runs nothing is never offered
		return
	case !a.reviews && (slices.Contains(onStates, put.production) || state == gatepolicy.Blocking) && slices.ContainsFunc(a.boot.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule && x.Producer == gatepolicy.LLMReview }):
		writeErr(w, http.StatusConflict, "no_model", noModelReview, nil) // a placement no review can fill would hold every promotion, and blocking before preview every run
		return
	}
	if len(body.GraceDays) > 0 {
		if err := json.Unmarshal(body.GraceDays, &put.grace); err != nil || put.grace < 0 || put.grace > 365 || body.State == nil && body.Production == nil { // a grace schedules a state
			writeErr(w, http.StatusBadRequest, "invalid_grace_days", "grace_days is an integer from 0 to 365, beside a state", nil)
			return
		}
	}
	if body.Settings != nil {
		settings, err := toolSettings(rule, body.Settings)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_settings", err.Error(), nil)
			return
		}
		if settings.Source != "" { // an imported tool the org added, never one Canyon runs on trees
			snap, err := a.effectivePolicy(r.Context(), a.pool)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
				return
			}
			if ranTools["sarif."+settings.Source] != nil || !slices.ContainsFunc(snap.Rules, func(x gatepolicy.Rule) bool {
				return x.ID == "sarif."+settings.Source && x.Producer == gatepolicy.Sarif
			}) {
				writeErr(w, http.StatusBadRequest, "invalid_source", "Canyon doesn't know "+settings.Source+" yet: add it under Guardrails first, then pick it here.", nil)
				return
			}
		}
		put.settings = &settings
	}
	// a tool is added by a state, and run and settings ride with it; once added it is never removed. The
	// tools the boot snapshot carries, Canyon's defaults, need no adding (contracts/snapshot.md).
	if scanner && body.State == nil && body.Production == nil && !slices.ContainsFunc(a.boot.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule }) {
		var added bool
		if err := a.pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM policy_edits WHERE rule = $1)", rule).Scan(&added); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if !added {
			writeErr(w, http.StatusBadRequest, "invalid_state", "a tool is added with a state, and run and settings ride with it", nil)
			return
		}
	}
	var answer map[string]any
	var err error
	if r.URL.Query().Get("apply") == "true" {
		answer, err = a.applyPolicy(r.Context(), actorOf(r.Context()), rule, put)
	} else { // the preview: what the states asked for would block, and nothing written
		var would []blocked
		would, err = a.wouldBlock(r.Context(), a.pool, rule, put)
		answer = map[string]any{"would_block": would}
		if err == nil && put.state == gatepolicy.Blocking && !slices.Contains(promoteRuleIDs, rule) {
			var unchecked []string
			if unchecked, err = a.uncheckedBefore(r.Context(), a.pool, rule); len(unchecked) > 0 {
				answer["unchecked"] = unchecked
			}
		}
	}
	if err != nil {
		a.log.Printf("canyon-server: put policy %s: %v", rule, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// noModelReview refuses placing a rule the AI review checks before production, or setting it blocking
// before preview, on a host with no model (contracts/routes.md, the policies row).
const noModelReview = "No model is configured on this Canyon host, so the AI review can't run. Ask whoever runs it to set one up."

// productionNotOffered is the refusal of production on a go-live rule (contracts/routes.md, the policies row).
const productionNotOffered = "The go-live rules are checked before production only: set their state"

// onStates are the states that put a rule in a gate.
var onStates = []gatepolicy.State{gatepolicy.Advisory, gatepolicy.Blocking}

// isState is whether a body's state for a gate, when it names one, is one of the three.
func isState(s *gatepolicy.State) bool {
	return s == nil || *s == gatepolicy.Off || slices.Contains(onStates, *s)
}

// policySet is the policy.set audit row's detail: on a policy edit, the rule's state before the edit
// and after it — the state the edit sets, in force at once or, with a grace, once it elapses, else the
// state it keeps — and its state before production's beside them when the edit changes it, off where
// it is not placed; and its run's and settings' before and after, each only when it changes.
type policySet struct {
	Rule       string                       `json:"rule"`
	Name       string                       `json:"name,omitempty"` // as the trail reads it; a tool not yet added has none, and the screen reads its id
	Before     gatepolicy.State             `json:"before,omitempty"`
	After      gatepolicy.State             `json:"after,omitempty"`
	GraceDays  int                          `json:"grace_days,omitempty"`
	Production *change[gatepolicy.State]    `json:"production,omitempty"`
	Run        *change[bool]                `json:"run,omitempty"`
	Settings   *change[gatepolicy.Settings] `json:"settings,omitempty"`
}

type change[T any] struct {
	Before T `json:"before"`
	After  T `json:"after"`
}

// applyPolicy writes the edits and their one audit row in one transaction under the ledger lock,
// where the version is assigned, and answers the rule's GET row as it now stands beside the preview
// at the target state. A part that changes nothing — the target is the rule's effective state, at
// once, with no edit scheduled ahead for it to withdraw, or the run or settings the rule has — writes
// nothing, as PUT /preferences writes nothing on a no-op.
func (a *app) applyPolicy(ctx context.Context, act actor, rule string, put policyPut) (map[string]any, error) {
	grace := put.grace
	tx, err := a.mutation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := a.policyList(ctx, tx)
	if err != nil {
		return nil, err
	}
	current, ok := rowOf(before, rule)
	if !ok {
		if !isImported(rule) { // the route checked the rule against the same snapshot, so this is a composer that changed under us
			return nil, fmt.Errorf("applying policy: the composed snapshot carries no rule %q", rule)
		}
		current.State = gatepolicy.Off // a tool not yet added installs off, and adding it is a change whatever the state (snapshot.md)
	}
	set := policySet{Rule: rule, Name: current.Name}
	placed := cmp.Or(current.Production, gatepolicy.Off)
	preview := put.state != "" && (!ok || current.State != put.state || grace > 0 || current.Scheduled != nil)
	production := put.production != "" && (!ok || placed != put.production || grace > 0 || current.ScheduledProduction != nil)
	if preview || production {
		// one row naming the gates the PUT sets: production NULL when it names none before production, and
		// state the rule's state now when it names none before preview, which the previous binary reads as a
		// change to nothing (0054). set_at is never before the rule's newest edit, whatever the database's
		// clock did since: the newest edit is the rule's state
		if _, err := tx.Exec(ctx, `INSERT INTO policy_edits (rule, state, names_state, production, grace_days, set_at, effective_at, set_by_kind, set_by_subject, set_by_display, version)
			SELECT $1, $2, $3, NULLIF($4, ''), $5, t, t + make_interval(days => $5), $6, $7, $8, (SELECT COALESCE(MAX(version), 0) + 1 FROM policy_edits)
			FROM (SELECT GREATEST(now(), (SELECT max(set_at) + interval '1 microsecond' FROM policy_edits WHERE rule = $1)) AS t) AS at`,
			rule, string(cmp.Or(put.state, current.State)), put.state != "", string(put.production), grace, act.Kind, act.Subject, act.Display); err != nil {
			return nil, err
		}
		set.Before, set.After, set.GraceDays = current.State, cmp.Or(put.state, current.State), grace
		if production {
			set.Production = &change[gatepolicy.State]{Before: placed, After: put.production}
		}
	}
	run, settings := current.Run, current.Settings // a tool's scanner edit is its whole run and settings after it
	if put.run != nil {
		run = *put.run
	}
	if put.settings != nil {
		settings = *put.settings
	}
	if run != current.Run || !reflect.DeepEqual(settings, current.Settings) {
		if _, err := tx.Exec(ctx, `INSERT INTO scanner_edits (rule, run, settings) VALUES ($1, $2, $3)`, rule, run, settings); err != nil {
			return nil, err
		}
		if run != current.Run {
			set.Run = &change[bool]{Before: current.Run, After: run}
		}
		if !reflect.DeepEqual(settings, current.Settings) {
			set.Settings = &change[gatepolicy.Settings]{Before: current.Settings, After: settings}
		}
		if rule == "sarif.opengrep" { // a credential goes with the repo it was set for
			if err := dropCredential(ctx, tx, act, settings.Rules); err != nil {
				return nil, err
			}
		}
	}
	if preview || production || set.Run != nil || set.Settings != nil {
		if err := events.AuditTx(ctx, tx, act, "policy.set", "", "", set); err != nil {
			return nil, err
		}
	}
	after, err := a.policyList(ctx, tx) // read back, never assembled in memory: a bad write cannot answer as a good one
	if err != nil {
		return nil, err
	}
	row, _ := rowOf(after, rule)
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if rule == depsRule && slices.Contains(onStates, put.production) && put.grace == 0 { // in force now: one with a grace waits for the tick
		a.askBuiltApps(ctx, nil)
	}
	// The preview is read after the commit, on the pool: it reads no policy row and needs no lock,
	// and it is a jsonb_path_query over every project's latest verdict — inside the ledger-locked
	// transaction it would stall every run start and every audited write for its duration. The edit
	// is committed by the time it runs, so a preview that errors answers 500 over a landed apply;
	// the caller re-reads GET /policies, which is the edit.
	would, err := a.wouldBlock(ctx, a.pool, rule, put)
	if err != nil {
		return nil, err
	}
	return map[string]any{"policy": row, "would_block": would}, nil
}

// blockingSeverities are the severities that block under a blocking rule — the formula
// gate/finding derives blocks by, spelled once more here for the SQL; the test
// TestThePreviewCountsWhatTheGateWouldBlock holds the two together over every severity.
var blockingSeverities = []string{string(finding.Critical), string(finding.High)}

// wouldBlock is the preview's list: for every live project, the findings under the rule that block at
// a target the PUT names, by the one formula (gate/finding: blocking, and high or critical, and not a
// Dockerfile check, which TestThePreviewCountsNoDockerfileCheck holds) — before
// preview in its latest verdict, the same row the fleet reads, never a pull request's or a check's; before production
// in its promote_run's (the status row), the run the promote button would send. A finding under the rule
// carries its id, or, under an imported tool's sarif.<tool>, sarif.<tool>.<ruleId> (finding.md), or,
// under a policy the table maps a tool's results to, <policy>.<tool>.<ruleId> (snapshot.md); no other
// result's id extends a rule's, as the gate refuses a result whose rule the snapshot does not carry.
// Only blocking blocks, so a gate at any other target is read not at all; a project named by both gates
// counts the more. Before production at blocking, a project whose promote_run did not have the rule on
// is named too, at 0 findings: its promotion is held all the same (heldUnread). Sorted by project id; a project with none is left out.
// platform.known-vuln is in no verdict: imageWouldBlock counts it. Nor is prod.egress-allowed: egressWouldBlock
// names what its row would hold.
func (a *app) wouldBlock(ctx context.Context, q querier, rule string, put policyPut) ([]blocked, error) {
	if rule == platformRule {
		return imageWouldBlock(ctx, q, put.state == gatepolicy.Blocking || put.production == gatepolicy.Blocking)
	}
	if rule == egressRule {
		return a.egressWouldBlock(ctx, q, put)
	}
	counts := map[string]int{}
	for _, gate := range []struct {
		runs   string
		target gatepolicy.State
	}{{latestVerdicts, put.state}, {promoteRuns, put.production}} {
		if gate.target != gatepolicy.Blocking {
			continue
		}
		rows, err := q.Query(ctx, `WITH `+gate.runs+`
			SELECT v.project_id, count(*) FROM verdict v
			JOIN projects p ON p.id = v.project_id AND p.deleted_at IS NULL,
			jsonb_path_query(v.data, '$.findings.runs[*].results[*] ? ((@.ruleId == $rule || @.ruleId starts with $under) && @.properties.canyon.severity == $sev[*] && !(@.ruleId starts with $docker) && !(@.ruleId starts with $docker2))', jsonb_build_object('rule', $1::text, 'under', $1::text || '.', 'sev', $2::text[], 'docker', $3::text, 'docker2', $4::text)) f
			GROUP BY v.project_id`, rule, blockingSeverities, finding.DockerfileChecks[0], finding.DockerfileChecks[1]) // starts with takes no array: one variable per prefix
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b blocked
			if err := rows.Scan(&b.Project, &b.Findings); err != nil {
				rows.Close()
				return nil, err
			}
			counts[b.Project] = max(counts[b.Project], b.Findings)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if put.production == gatepolicy.Blocking {
		if err := a.heldUnread(ctx, q, rule, counts); err != nil {
			return nil, err
		}
	}
	if rule == depsRule && put.production == gatepolicy.Blocking {
		if err := builtAppWouldBlock(ctx, q, counts); err != nil {
			return nil, err
		}
	}
	out := []blocked{}
	for _, p := range slices.Sorted(maps.Keys(counts)) {
		out = append(out, blocked{Project: p, Findings: counts[p]})
	}
	return out, nil
}

// imageWouldBlock is platform.known-vuln's preview, nothing unless blocking: every live project whose image
// in preview its receipt row would hold, read as the receipt reads it (platformOf, platformRow), findings
// the packages the row counts (contracts/routes.md, the policies row). Sorted by project id.
func imageWouldBlock(ctx context.Context, q querier, blocking bool) ([]blocked, error) {
	out := []blocked{}
	if !blocking {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT d.project_id, d.image FROM `+runs.ShownDeploys+` d
		JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL WHERE d.env = 'preview' ORDER BY d.project_id`)
	if err != nil {
		return nil, err
	}
	type preview struct{ Project, Image string }
	previews, err := pgx.CollectRows(rows, pgx.RowToStructByPos[preview])
	if err != nil {
		return nil, err
	}
	snap := gatepolicy.Snapshot{Rules: []gatepolicy.Rule{{ID: platformRule, State: gatepolicy.Blocking}}}
	for _, d := range previews {
		read, err := platformOf(ctx, q, snap, d.Project, d.Image)
		if err != nil {
			return nil, err
		}
		if !platformRow(snap.Rules[0], *read).Passed {
			out = append(out, blocked{Project: d.Project, Findings: read.counted})
		}
	}
	return out, nil
}

// egressWouldBlock is prod.egress-allowed's preview (contracts/routes.md, the policies row): each live project whose
// preview its receipt row would hold, read as the receipt reads it (readEgressPreview, egressRow), findings the
// destinations the row names, 0 when it could not check. At blocking it reads every one, below it those holding a
// blocking addition, which the org's state can't clear. An addition below blocking names none: it replaces the one
// its app holds. With nothing in preview a receipt reads no row, a rollback's never holds, and nor does a neutral one.
// Sorted by project id.
func (a *app) egressWouldBlock(ctx context.Context, q querier, put policyPut) ([]blocked, error) {
	out := []blocked{}
	blocking := put.state == gatepolicy.Blocking || put.production == gatepolicy.Blocking
	if !blocking && put.addition {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT d.project_id FROM `+runs.ShownDeploys+` d
		JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL
		WHERE d.env = 'preview' AND ($1::bool OR EXISTS (SELECT 1 FROM app_policy_additions a
			WHERE a.project_id = d.project_id AND a.rule = $2 AND a.state = 'blocking'))
		ORDER BY d.project_id`, blocking, egressRule)
	if err != nil {
		return nil, err
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	platform, err := a.platformDestinations(ctx, q)
	if err != nil {
		return nil, err
	}
	rule := gatepolicy.Rule{ID: egressRule, State: gatepolicy.Blocking}
	for _, p := range projects {
		read, err := a.readEgressPreview(ctx, q, p, platform)
		if err != nil {
			return nil, err
		}
		if row := egressRow(rule, read, ""); !row.Passed && !row.Unset {
			out = append(out, blocked{Project: p, Findings: len(read.denied)})
		}
	}
	return out, nil
}

// builtAppWouldBlock adds to counts each live project whose built app in preview deps.known-vuln's row would
// hold before production (contracts/routes.md, the policies row): read as the receipt reads it, against its
// promote_run's findings and the approvals that cover a package for it, at the packages it counts, 0 when it could
// not be checked.
func builtAppWouldBlock(ctx context.Context, q querier, counts map[string]int) error {
	rows, err := q.Query(ctx, `WITH `+promoteRuns+` SELECT v.project_id, v.run, v.data->'findings', d.image FROM verdict v
		JOIN projects p ON p.id = v.project_id AND p.deleted_at IS NULL
		JOIN `+runs.ShownDeploys+` d ON d.project_id = v.project_id AND d.env = 'preview'
		WHERE v.data IS NOT NULL ORDER BY v.project_id`)
	if err != nil {
		return err
	}
	type preview struct {
		Project  string
		Run      string
		Findings []byte
		Image    string
	}
	previews, err := pgx.CollectRows(rows, pgx.RowToStructByPos[preview])
	if err != nil {
		return err
	}
	for _, d := range previews {
		var log finding.Log // a verdict with no findings reported none
		if d.Findings != nil {
			if err := json.Unmarshal(d.Findings, &log); err != nil {
				return err
			}
		}
		f, err := loadImage(ctx, q, d.Project, runs.ImageDigest(d.Image))
		if err != nil {
			return err
		}
		app := readBuiltApp(f, reportedIn(log), time.Now())
		if err := approveBuiltApp(ctx, q, d.Project, d.Run, &app, 0); err != nil {
			return err
		}
		if !app.clean() {
			counts[d.Project] = max(counts[d.Project], len(app.packages))
		}
	}
	return nil
}

// heldUnread adds to counts, at 0, every live project whose promote_run did not have the rule on: its
// receipt has nothing of the rule to read and holds the promotion (contracts/routes.md, the policies row).
func (a *app) heldUnread(ctx context.Context, q querier, rule string, counts map[string]int) error {
	rows, err := q.Query(ctx, `WITH `+promoteRuns+` SELECT v.project_id, v.run FROM verdict v JOIN projects p ON p.id = v.project_id AND p.deleted_at IS NULL`)
	if err != nil {
		return err
	}
	type promoteRun struct{ Project, Run string }
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[promoteRun])
	if err != nil {
		return err
	}
	for _, r := range runs {
		snap, err := a.journaled(r.Run)
		if err != nil {
			return err
		}
		if _, counted := counts[r.Project]; !counted && !slices.ContainsFunc(snap.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule && x.On() }) {
			counts[r.Project] = 0
		}
	}
	return nil
}

// uncheckedBefore is the preview's unchecked (contracts/routes.md, the policies row; #155): each live project whose
// latest verdict holds no result under the rule and whose run's journaled snapshot did not have it on, so what the rule
// would block of it before preview is not known until it is reviewed again. A verdict holding a result under the rule
// was checked by it, and would_block counts it if it blocks. A snapshot that does not read had nothing on. Sorted by project id.
func (a *app) uncheckedBefore(ctx context.Context, q querier, rule string) ([]string, error) {
	rows, err := q.Query(ctx, `WITH `+latestVerdicts+` SELECT v.project_id, v.run FROM verdict v
		JOIN projects p ON p.id = v.project_id AND p.deleted_at IS NULL
		WHERE NOT jsonb_path_exists(v.data, '$.findings.runs[*].results[*] ? (@.ruleId == $rule || @.ruleId starts with $under)', jsonb_build_object('rule', $1::text, 'under', $1::text || '.'))
		ORDER BY v.project_id`, rule)
	if err != nil {
		return nil, err
	}
	type latest struct{ Project, Run string }
	verdicts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[latest])
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, v := range verdicts {
		snap, err := a.journaled(v.Run)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(snap.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule && x.On() }) {
			out = append(out, v.Project)
		}
	}
	return out, nil
}

// latestVerdicts is each project's latest verdict, never a pull request's, a check's, a rollback review's or a
// patch run's (contracts/routes.md, Patch runs): what before preview stopped.
const latestVerdicts = `verdict AS (SELECT DISTINCT ON (t.project_id) t.project_id, t.run_id AS run, t.data FROM timeline_rows t
	JOIN runs r ON r.id = t.run_id JOIN packages pk ON pk.id = r.package_id AND pk.project_id = r.project_id AND pk.source_kind NOT IN ('repo-pr', 'check')
	WHERE t.kind = 'verdict' AND r.rollback_to IS NULL AND r.kind <> 'patch' ORDER BY t.project_id, t.id DESC)`

// promoteRuns is each project's promote_run's verdict: the newest gate run of the package preview runs,
// or the newest gate run when nothing runs there, never a pull request's, a check's or a rollback review's — as the status read picks it.
// A run with no verdict yet has no findings.
const promoteRuns = `verdict AS (SELECT DISTINCT ON (r.project_id) r.project_id, r.id AS run, t.data FROM runs r
	JOIN packages pk ON pk.id = r.package_id AND pk.project_id = r.project_id AND pk.source_kind NOT IN ('repo-pr', 'check')
	LEFT JOIN ` + runs.ShownDeploys + ` d ON d.project_id = r.project_id AND d.env = 'preview'
	LEFT JOIN timeline_rows t ON t.run_id = r.id AND t.kind = 'verdict'
	WHERE r.kind = 'gate' AND r.rollback_to IS NULL AND (d.package_id IS NULL OR d.package_id = r.package_id)
	ORDER BY r.project_id, r.created_at DESC, r.id DESC)`
