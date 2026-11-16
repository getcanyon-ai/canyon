package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// routesAppPolicies registers an app's additions (contracts/routes.md, PUT and DELETE
// /api/v1/projects/:id/policies/:rule; design/PERMISSIONS.md, The gate pipeline): the app's owners and
// admins add stricter checks to their own app and remove them, and never weaken an org rule.
func routesAppPolicies(api *http.ServeMux, a *app) {
	api.HandleFunc("PUT /projects/{id}/policies/{rule}", a.ownerOnly(personOnly(changesAppChecks, a.putAddition)))
	api.HandleFunc("DELETE /projects/{id}/policies/{rule}", a.ownerOnly(personOnly(changesAppChecks, a.deleteAddition)))
}

// changesAppChecks is the refusal of an agent's change to an app's additions.
const changesAppChecks = "only a person can change an app's checks"

// notStricter is the refusal of an addition that would weaken an org rule.
const notStricter = "An app's checks can only add to the org's, never weaken them."

// putAddition answers the preview — this project when its promote_run holds findings under the rule
// the addition would block, and nothing written — and with ?apply=true replaces the app's addition to
// the rule whole and answers the rule's GET /policies row beside the same preview. The refusals come in
// the contract's order, each before anything is written: a rule the snapshot does not carry, a body of
// anything but state and production in the three, any grace, production on a go-live rule, and a field
// not stricter than the org's state in its gate now.
func (a *app) putAddition(w http.ResponseWriter, r *http.Request) {
	ctx, id, rule := r.Context(), r.PathValue("id"), r.PathValue("rule")
	org, err := a.effectivePolicy(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: put addition %s/%s: %v", id, rule, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	i := slices.IndexFunc(org.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule })
	if i < 0 {
		writeErr(w, http.StatusNotFound, "not_found", "no policy carries that id", nil)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_state", "the body is state and production, each off, advisory or blocking", nil)
		return
	}
	var add addition
	for key, raw := range body {
		var st *gatepolicy.State
		switch key {
		case "state":
			st = &add.State
		case "production":
			st = &add.Production
		case "grace_days":
			continue
		}
		if st == nil || json.Unmarshal(raw, st) != nil || !isState(st) || *st == "" {
			writeErr(w, http.StatusBadRequest, "invalid_state", "the body is state and production, each off, advisory or blocking", nil)
			return
		}
	}
	switch _, grace := body["grace_days"]; {
	case add == addition{}:
		writeErr(w, http.StatusBadRequest, "invalid_state", "the body is state and production, each off, advisory or blocking", nil)
	case grace:
		writeErr(w, http.StatusBadRequest, "invalid_grace_days", "an app's addition takes effect at once: it takes no grace_days", nil)
	case add.Production != "" && slices.Contains(promoteRuleIDs, rule):
		writeErr(w, http.StatusConflict, "production_not_offered", productionNotOffered, nil)
	case add.State != "" && stricter(org.Rules[i].State, add.State) == org.Rules[i].State,
		add.Production != "" && stricter(cmp.Or(org.Rules[i].Production, gatepolicy.Off), add.Production) == cmp.Or(org.Rules[i].Production, gatepolicy.Off):
		writeErr(w, http.StatusBadRequest, "not_stricter", notStricter, nil)
	case !a.reviews && org.Rules[i].Producer == gatepolicy.LLMReview && (add.State == gatepolicy.Blocking || slices.Contains(onStates, add.Production)):
		message := noModelReview // as the org's own edit is (#1555); a custom rule keeps its own words
		if strings.HasPrefix(rule, "custom.") {
			message = noModel
		}
		writeErr(w, http.StatusConflict, "no_model", message, nil)
	default:
		a.answerAddition(w, r, id, org.Rules[i], add)
	}
}

// answerAddition is the preview, or with ?apply=true the write: the row and its one app_policy.set row in
// one transaction under the ledger lock a run's start takes, the detail naming each gate whose state in
// force on the app the write changes. The rule's row is read back inside it, as the caller reads it.
func (a *app) answerAddition(w http.ResponseWriter, r *http.Request, project string, org gatepolicy.Rule, add addition) {
	ctx := r.Context()
	target := gatepolicy.Off // both gates read promote_run (contracts/routes.md)
	if add.State == gatepolicy.Blocking || add.Production == gatepolicy.Blocking {
		target = gatepolicy.Blocking
	}
	would, err := a.wouldBlock(ctx, a.pool, org.ID, policyPut{production: target, addition: true})
	if err != nil {
		a.log.Printf("canyon-server: put addition %s/%s: %v", project, org.ID, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	would = slices.DeleteFunc(would, func(b blocked) bool { return b.Project != project })
	if r.URL.Query().Get("apply") != "true" {
		writeJSON(w, http.StatusOK, map[string]any{"would_block": would})
		return
	}
	row, err := a.applyAddition(ctx, project, org, &add)
	if err != nil {
		a.log.Printf("canyon-server: put addition %s/%s: %v", project, org.ID, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if org.ID == depsRule && slices.Contains(onStates, add.Production) { // an addition takes effect at once
		a.askBuiltApps(ctx, []string{project})
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": row, "would_block": would})
}

// appPolicySet is the app_policy.set audit row's detail: each gate whose state in force on the app —
// the stricter of the org's and the addition's — the write changes, before and after.
type appPolicySet struct {
	Rule       string                    `json:"rule"`
	State      *change[gatepolicy.State] `json:"state,omitempty"`
	Production *change[gatepolicy.State] `json:"production,omitempty"`
}

// applyAddition writes add, or with add nil removes the app's addition to the rule, with its audit
// row, and answers the rule's GET /policies row as it now stands. Removing what is not there is
// pgx.ErrNoRows.
func (a *app) applyAddition(ctx context.Context, project string, org gatepolicy.Rule, add *addition) (policyRow, error) {
	act := actorOf(ctx)
	tx, err := a.mutation(ctx)
	if err != nil {
		return policyRow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var old addition
	err = tx.QueryRow(ctx, `SELECT COALESCE(state, ''), COALESCE(production, '') FROM app_policy_additions WHERE project_id = $1 AND rule = $2 FOR UPDATE`, project, org.ID).Scan(&old.State, &old.Production)
	if err != nil && (add == nil || !errors.Is(err, pgx.ErrNoRows)) {
		return policyRow{}, err
	}
	if add == nil {
		_, err = tx.Exec(ctx, `DELETE FROM app_policy_additions WHERE project_id = $1 AND rule = $2`, project, org.ID)
		if err == nil {
			err = events.AuditTx(ctx, tx, act, "app_policy.remove", project, "", map[string]string{"rule": org.ID})
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO app_policy_additions (project_id, rule, state, production, set_by_kind, set_by_subject, set_by_display)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7)
			ON CONFLICT (project_id, rule) DO UPDATE SET state = EXCLUDED.state, production = EXCLUDED.production,
				set_by_kind = EXCLUDED.set_by_kind, set_by_subject = EXCLUDED.set_by_subject, set_by_display = EXCLUDED.set_by_display, set_at = now()`,
			project, org.ID, string(add.State), string(add.Production), act.Kind, act.Subject, act.Display)
		if err == nil {
			before, after := old.applied(org), add.applied(org)
			set := appPolicySet{Rule: org.ID}
			if before.State != after.State {
				set.State = &change[gatepolicy.State]{Before: before.State, After: after.State}
			}
			if before.Production != after.Production {
				set.Production = &change[gatepolicy.State]{Before: cmp.Or(before.Production, gatepolicy.Off), After: cmp.Or(after.Production, gatepolicy.Off)}
			}
			err = events.AuditTx(ctx, tx, act, "app_policy.set", project, "", set)
		}
	}
	if err != nil {
		return policyRow{}, err
	}
	list, err := a.policyList(ctx, tx) // read back, never assembled in memory: a bad write cannot answer as a good one
	if err != nil {
		return policyRow{}, err
	}
	row, _ := rowOf(list, org.ID)
	return row, tx.Commit(ctx)
}

// deleteAddition removes the app's addition to the rule whole: 204, or 404 when it has none.
func (a *app) deleteAddition(w http.ResponseWriter, r *http.Request) {
	ctx, id, rule := r.Context(), r.PathValue("id"), r.PathValue("rule")
	org, err := a.effectivePolicy(ctx, a.pool)
	i := -1
	if err == nil {
		i = slices.IndexFunc(org.Rules, func(x gatepolicy.Rule) bool { return x.ID == rule })
	}
	if err == nil && i >= 0 {
		_, err = a.applyAddition(ctx, id, org.Rules[i], nil)
	}
	switch {
	case i < 0 && err == nil, errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "this app has no addition to that rule", nil)
	case err != nil:
		a.log.Printf("canyon-server: delete addition %s/%s: %v", id, rule, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
