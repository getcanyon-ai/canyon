package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// routesCustomRules registers an org's custom rules (contracts/routes.md, the custom-rules row): every
// member reads them, an admin writes them. Their state is the policies row's, like any rule's.
func routesCustomRules(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /custom-rules", a.getCustomRules)
	api.HandleFunc("PUT /custom-rules/{slug}", a.adminOnly(personOnly("only a person can change a custom rule", a.policyInDashboard(a.putCustomRule))))
	api.HandleFunc("POST /custom-rules/{slug}/dry-run", a.adminOnly(a.dryRunCustomRule))
}

// noModel is the refusal of a custom rule turned on with no model on the host (contracts/routes.md).
const noModel = "No model is configured on this Canyon host, so a custom rule can't run. Ask whoever runs it to set one up."

// maxCustomRules is how many custom rules a host takes (contracts/snapshot.md, Custom rules).
const maxCustomRules = 20

var customSlug = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)

// customRule is one custom rule at its newest version, as both routes answer it.
type customRule struct {
	Rule         string           `json:"rule"`
	Name         string           `json:"name"`
	Instructions string           `json:"instructions"`
	Check        gatepolicy.Check `json:"check"`
	Version      int              `json:"version"`
	SetBy        actor            `json:"set_by"`
	SetAt        string           `json:"set_at"`
}

// beforeAfter is one field's change in the audit row; before is absent at version 1.
type beforeAfter struct {
	Before *string `json:"before,omitempty"`
	After  string  `json:"after"`
}

// customRulesOf is every custom rule at its newest version, by rule.
func customRulesOf(ctx context.Context, q querier) ([]customRule, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (rule) rule, name, instructions, check_by, coalesce(check_expr, ''), coalesce(check_says, ''), check_facts_version,
		version, set_by_kind, set_by_subject, set_by_display, set_at
		FROM custom_rule_edits ORDER BY rule, version DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (c customRule, err error) {
		var at time.Time
		err = row.Scan(&c.Rule, &c.Name, &c.Instructions, &c.Check.By, &c.Check.Expr, &c.Check.Says, &c.Check.FactsVersion, &c.Version, &c.SetBy.Kind, &c.SetBy.Subject, &c.SetBy.Display, &at)
		c.SetAt = at.UTC().Format(time.RFC3339)
		return c, err
	})
}

func (a *app) getCustomRules(w http.ResponseWriter, r *http.Request) {
	rules, err := customRulesOf(r.Context(), a.pool)
	if err != nil {
		a.log.Printf("canyon-server: custom rules: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"custom_rules": rules})
}

// customRuleProblem is the first thing the routes row refuses in a custom rule, in its words, or "".
func customRuleProblem(slug, name, instructions string) (string, error) {
	plain := func(s string, newline bool) bool {
		for _, c := range s {
			if unicode.IsControl(c) && (!newline || c != '\n') {
				return false
			}
		}
		return true
	}
	switch n, m := utf8.RuneCountInString(name), utf8.RuneCountInString(instructions); {
	case !customSlug.MatchString(slug):
		return "the rule's id is custom. and 1 to 40 lowercase letters and -, a letter first", nil
	case n < 1 || n > 80 || !plain(name, false): // JSON decoding already made invalid UTF-8 U+FFFD
		return "the name is 1 to 80 characters of plain text, on one line", nil
	case m < 1 || m > 2000 || !plain(instructions, true):
		return "the instructions are 1 to 2,000 characters of plain text", nil
	}
	for _, s := range []string{name, instructions} {
		secret, err := gate.HasSecret(s)
		if err != nil || secret {
			return "the name and instructions may carry no secret: a rule's text is kept with every review and sent to the model", err
		}
	}
	return "", nil
}

// putCustomRule saves a custom rule's next version — its first installs it off — with one
// custom_rule.set audit row carrying the before and after of each text. A PUT that changes nothing
// writes nothing and answers the rule as it is.
func (a *app) putCustomRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string  `json:"name"`
		Instructions string  `json:"instructions"`
		Check        *string `json:"check"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || body.Check != nil && *body.Check != custom.ByCode && *body.Check != custom.ByAI { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "bad_request", `the body is one JSON object of name, instructions and, if you choose, check: "code" or "ai"`, nil)
		return
	}
	slug := r.PathValue("slug")
	problem, err := customRuleProblem(slug, body.Name, body.Instructions)
	if err != nil {
		a.log.Printf("canyon-server: custom rule %s: %v", slug, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if problem != "" {
		writeErr(w, http.StatusBadRequest, "invalid_custom_rule", problem, nil)
		return
	}
	saved, why, err := a.saveCustomRule(r.Context(), actorOf(r.Context()), "custom."+slug, body.Name, body.Instructions, body.Check)
	switch {
	case errors.Is(err, errTooManyRules):
		writeErr(w, http.StatusConflict, "too_many_rules", "A Canyon host takes 20 custom rules.", nil)
	case errors.Is(err, errSavedMeanwhile):
		writeErr(w, http.StatusConflict, "saved_meanwhile", "Someone saved this rule while yours was being checked. Reload it and save again.", nil)
	case err != nil:
		a.log.Printf("canyon-server: custom rule %s: %v", slug, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		answer := map[string]any{"custom_rule": saved}
		if why != "" { // code was asked for and the AI review checks it: the admin is told why
			answer["why"] = why
		}
		writeJSON(w, http.StatusOK, answer)
	}
}

var errTooManyRules = errors.New("a host takes 20 custom rules")

// errSavedMeanwhile is a save another one beat while the model was asked: its version would not be the
// one after what the admin saw.
var errSavedMeanwhile = errors.New("the rule was saved meanwhile")

// checkOf is how a version with this text is checked, asked for by: by AI without a model call when by
// is ai; else as the host's model translates it and Canyon admits the translation (gate/custom), and by
// AI whenever it does not — no model on the host, the model failing, its answer refused, or a plain-words
// condition carrying a secret. It never refuses the save (contracts/routes.md, the custom-rules row).
func (a *app) checkOf(ctx context.Context, rule, instructions, by string) (gatepolicy.Check, string) {
	byAI := gatepolicy.Check{By: custom.ByAI, FactsVersion: custom.FactsVersion}
	switch {
	case by == custom.ByAI:
		return byAI, ""
	case a.translator == nil:
		return byAI, "No AI model is set up on this Canyon, so it can't work out a check by code. The AI review checks this rule."
	}
	c, err := custom.Translate(ctx, a.translator, a.modelID, instructions)
	if err != nil {
		a.log.Printf("canyon-server: custom rule %s: translating it: %v", rule, err)
		return byAI, "Canyon's AI model didn't answer, so the AI review checks this rule. Save it again later to ask again."
	}
	unusable := "The check Canyon's AI model wrote didn't pass Canyon's own tests, so the AI review checks this rule."
	switch {
	case c.By == custom.ByAI && c.Refused != "":
		return byAI, unusable
	case c.By == custom.ByAI:
		return byAI, "Canyon's AI model said the AI review should check this rule" + plainly(c.Why)
	}
	if secret, err := gate.HasSecret(c.Says); err != nil || secret || utf8.RuneCountInString(c.Says) > 280 || strings.ContainsFunc(c.Says, unicode.IsControl) {
		return byAI, unusable
	}
	return gatepolicy.Check{By: custom.ByCode, Expr: c.Expr, Says: c.Says, FactsVersion: c.FactsVersion}, ""
}

// plainly is the model's reason as the save says it, after a colon: one line of at most 200 characters,
// never a control character or a secret, and nothing when that leaves none.
func plainly(why string) string {
	why = strings.Join(strings.FieldsFunc(why, unicode.IsControl), " ")
	if r := []rune(strings.TrimSpace(why)); len(r) > 200 {
		why = string(r[:199]) + "…"
	}
	if secret, err := gate.HasSecret(why); err != nil || secret || strings.TrimSpace(why) == "" {
		return "."
	}
	return ": " + strings.TrimSpace(why)
}

// saveCustomRule saves the rule's next version, checked as by asks: nil names no check, which keeps the
// current one on the same text and asks for code on a new rule or new text (contracts/routes.md).
func (a *app) saveCustomRule(ctx context.Context, act actor, rule, name, instructions string, by *string) (customRule, string, error) {
	rules, err := customRulesOf(ctx, a.pool)
	if err != nil {
		return customRule{}, "", err
	}
	var current *customRule
	for i := range rules {
		if rules[i].Rule == rule {
			current = &rules[i]
		}
	}
	same := current != nil && current.Name == name && current.Instructions == instructions
	if same && (by == nil || *by == current.Check.By) {
		return *current, "", nil // nothing changed: no version, no row
	}
	want := custom.ByCode
	if by != nil {
		want = *by
	}
	if current == nil && len(rules) >= maxCustomRules {
		return customRule{}, "", errTooManyRules
	}
	check, why := a.checkOf(ctx, rule, instructions, want) // the model's call, outside the transaction
	if same && check == current.Check {
		return *current, why, nil // asked again, answered the same: no version, no row
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		return customRule{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if rules, err = customRulesOf(ctx, tx); err != nil {
		return customRule{}, "", err
	}
	previous := current
	current = nil
	for i := range rules {
		if rules[i].Rule == rule {
			current = &rules[i]
		}
	}
	if current == nil && len(rules) >= maxCustomRules {
		return customRule{}, "", errTooManyRules
	}
	if (current == nil) != (previous == nil) || current != nil && current.Version != previous.Version {
		return customRule{}, "", errSavedMeanwhile
	}
	audited := map[string]any{"by": check.By}
	if check.By == custom.ByCode {
		audited["says"] = check.Says
	}
	version, detail := 1, map[string]any{"rule": rule, "name": beforeAfter{After: name}, "instructions": beforeAfter{After: instructions}, "check": audited}
	if current != nil {
		version = current.Version + 1
		detail["name"], detail["instructions"] = beforeAfter{Before: &current.Name, After: name}, beforeAfter{Before: &current.Instructions, After: instructions}
	}
	detail["version"] = version
	if _, err := tx.Exec(ctx, `INSERT INTO custom_rule_edits (rule, version, name, instructions, check_by, check_expr, check_says, check_facts_version, set_by_kind, set_by_subject, set_by_display)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, $10, $11)`,
		rule, version, name, instructions, check.By, check.Expr, check.Says, check.FactsVersion, act.Kind, act.Subject, act.Display); err != nil {
		return customRule{}, "", err
	}
	if err := events.AuditTx(ctx, tx, act, "custom_rule.set", "", "", detail); err != nil {
		return customRule{}, "", err
	}
	after, err := customRulesOf(ctx, tx) // read back, never assembled in memory
	if err != nil {
		return customRule{}, "", err
	}
	for _, c := range after {
		if c.Rule == rule {
			return c, why, tx.Commit(ctx)
		}
	}
	return customRule{}, "", errors.New("a saved custom rule did not read back")
}
