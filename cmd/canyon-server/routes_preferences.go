package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/runs"
)

// routesPreferences registers the org's preferences (contracts/routes.md, the preferences row):
// one org per host, one row — the approved stacks and the default design system, read by the
// guardrails screen and by the skill manifest, written by the form beside the panel.
func routesPreferences(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /preferences", a.getPreferences)
	api.HandleFunc("PUT /preferences", a.adminOnly(a.putPreferences))
}

// stacksRule is the contract's bound on the approved list, and the message a refusal carries.
const stacksRule = "stacks is at most 16 entries, one per language, each with 1 to 16 frameworks, every name 1 to 64 characters of plain text"

// stack is one approved language with the frameworks the org builds on it.
type stack struct {
	Language   string   `json:"language"`
	Frameworks []string `json:"frameworks"`
}

// resolved is the org's default design system as an answer carries it: the registry row without
// source_type and without its history — a reader of the preferences names a registry, not a row.
type resolved struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	RegistryURL string  `json:"registry_url"`
	TokensURL   *string `json:"tokens_url,omitempty"` // omitted when unset: the contract types it a string, never a null
}

// viewer is who is reading, as these two routes answer it (contracts/routes.md row 219): the
// address the session stores and the role, so the settings page can find the reader in the lists it
// is drawing — which of these addresses is mine, and may I edit any of this. GET /fleet answers the
// same concept as {display, role}: one name, two projections, and no consumer is handed both.
type viewer struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// preferences is the shape both routes answer: the approved stacks and the default design system,
// then the organisation's settings (design/ONBOARDING.md §5) and overridden, the keys the
// environment sets, whose values the read answers in the environment's stead.
type preferences struct {
	Stacks []stack `json:"stacks"`
	// ApprovedBuildpacks is the buildpack ids stack.blessed approves; BuildpacksSeen every buildpack a
	// deploy row records running, what the screen offers to approve (contracts/routes.md, #636).
	ApprovedBuildpacks []string             `json:"approved_buildpacks"`
	BuildpacksSeen     []executor.Buildpack `json:"buildpacks_seen"`
	DesignSystem       *resolved            `json:"design_system"`
	SetupAvailable     bool                 `json:"setup_available"`
	AIReview           bool                 `json:"ai_review"`        // any model, OpenAI included; setup_available is the setup chat's (#2003)
	Viewer             *viewer              `json:"viewer,omitempty"` // omitted on a demo host, whose sessions store no address
	OrgName            string               `json:"org_name"`
	AllowedDomains     []string             `json:"allowed_domains"`
	AllowedEmails      []string             `json:"allowed_emails"`
	AdminEmails        []string             `json:"admin_emails"`
	// BootstrapAdmins is the admins CANYON_ADMINS grants, answered beside admin_emails and never
	// merged into it. A screen that drew one list would let somebody remove a bootstrap address,
	// see the PUT succeed — the key is not one the operator pinned, so there is nothing to refuse —
	// and have that person still be an admin (contracts/routes.md row 219).
	BootstrapAdmins []string `json:"bootstrap_admins"`
	AdminClaim      *string  `json:"admin_claim,omitempty"` // with admin_value: present together or absent together
	AdminValue      *string  `json:"admin_value,omitempty"`
	// The security responders (design/PERMISSIONS.md): the list, [] while empty, and the claim pair as the
	// admin pair is answered — all three to an admin only, absent on a member's read.
	ResponderEmails []string `json:"responder_emails,omitzero"`
	ResponderClaim  *string  `json:"responder_claim,omitempty"`
	ResponderValue  *string  `json:"responder_value,omitempty"`
	GroupsClaim     string   `json:"groups_claim"`          // "" while unset, when no group grant is written (the Access group)
	OnboardedAt     *string  `json:"onboarded_at"`          // RFC 3339 at whole seconds UTC; null while the host is in setup
	RunBy           string   `json:"run_by"`                // where apps run (design/HAND-OFF.md §1): canyon, or org through releases
	AccessGrace     string   `json:"access_grace"`          // how long a handed-off app keeps its last access list when Canyon is unreachable, as Go spells a duration
	Delivery        string   `json:"delivery"`              // how releases reach the org's platform (design/HAND-OFF.md §1, §3b): webhook, or repo
	ProdApproval    bool     `json:"prod_approval"`         // a promotion the CLI asks for waits for a person in the dashboard (design/AUTH.md §4.5)
	PushLinked      bool     `json:"push_approvers_linked"` // a push's blocking finding is approved only by someone who linked a GitHub account (#2342)
	Patching        string   `json:"patching"`              // off or ask: whether Canyon patches its run image under the apps (design/PATCHING.md §4.5)
	Rebuilds        string   `json:"rebuilds"`              // off or ask: whether Canyon also rebuilds apps on a newer builder (design/PATCHING.md §5)
	DBSessions      string   `json:"db_sessions"`           // off or on: whether a person may open a database session at all (design/DATABASE-SESSIONS.md §5)
	Overridden      []string `json:"overridden"`
}

func resolvedOf(d designSystem) *resolved {
	return &resolved{ID: d.ID, Name: d.Name, Namespace: d.Namespace, RegistryURL: d.RegistryURL, TokensURL: d.TokensURL}
}

func (a *app) getPreferences(w http.ResponseWriter, r *http.Request) {
	out, err := a.orgPreferences(r.Context(), a.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, a.asRead(r.Context(), out))
}

// asRead is the row as this reader is answered it: the host's configuration and the reader's own
// facts, neither of them the row's, stamped on the way out rather than loaded with it — the skill
// manifest reads the same row through readPreferences and has neither an app nor a session.
//
// THE REDACTION BELONGS HERE AND NOWHERE FURTHER DOWN. readPreferences and orgPreferences are the
// write path's loaders too: applyPreferences reads them for the before and the after of the audit
// row, so a redaction tidied down a layer would take the admin claim off a governance record — the
// PUT that set or cleared the org's admin claim would be written to the trail as having changed
// nothing at all, and the trail would say nobody ever touched it (canyonv2-7mtz).
func (a *app) asRead(ctx context.Context, out preferences) preferences {
	out.SetupAvailable, out.AIReview = a.model != nil, a.reviews
	if email := addressOf(ctx); email != "" { // a demo session stores none, and an address is what a viewer is
		out.Viewer = &viewer{Email: email, Role: roleOf(ctx)}
	}
	if roleOf(ctx) != "admin" { // a member is answered neither the claim nor its value, whatever the row holds; `overridden` still names the pair, because a variable's name is not its value
		out.AdminClaim, out.AdminValue = nil, nil
		out.ResponderEmails, out.ResponderClaim, out.ResponderValue = nil, nil, nil
	}
	return out
}

// orgPreferences is the row as the two routes answer it: the environment's value over every key
// it sets, named in overridden, so a screen renders the field read-only and names the variable
// rather than letting somebody edit a value that will not take (contracts/routes.md).
func (a *app) orgPreferences(ctx context.Context, q querier) (preferences, error) {
	out, err := readPreferences(ctx, q)
	if err != nil {
		return out, err
	}
	if a.orgEnv != "" {
		out.OrgName = a.orgEnv
	}
	if out.OrgName == "" { // the same name orgNow answers: the read, /auth/config and the receipt never disagree about what this org is called
		out.OrgName = defaultOrg
	}
	out.ProdApproval = out.ProdApproval && a.auth == authOIDC // with sign-in off nobody can be told from the dashboard
	if a.auth != authOIDC {                                   // nor can anybody decide a patch approval
		out.Patching = "off"
	}
	if out.Patching == "off" { // a rebuild is a patch: off with patching (contracts/routes.md, the preferences row)
		out.Rebuilds = "off"
	}
	if c := a.oidc; c != nil {
		if c.domains != nil {
			out.AllowedDomains = c.domains
		}
		if c.emails != nil {
			out.AllowedEmails = c.emails
		}
		out.BootstrapAdmins = c.admins
		if c.adminClaim != "" {
			claim, value := c.adminClaim, c.adminValue
			out.AdminClaim, out.AdminValue = &claim, &value
		}
		if c.responderClaim != "" {
			claim, value := c.responderClaim, c.responderValue
			out.ResponderClaim, out.ResponderValue = &claim, &value
		}
		if c.groupsClaim != "" {
			out.GroupsClaim = c.groupsClaim
		}
	}
	out.Overridden = a.overriddenKeys()
	return out, nil
}

// overriddenKeys is which keys the environment sets, from app's own config — no row read needed.
func (a *app) overriddenKeys() []string {
	out := []string{}
	if a.orgEnv != "" {
		out = append(out, "org_name")
	}
	if c := a.oidc; c != nil {
		if c.domains != nil {
			out = append(out, "allowed_domains")
		}
		if c.emails != nil {
			out = append(out, "allowed_emails")
		}
		if c.adminClaim != "" {
			out = append(out, "admin_claim", "admin_value")
		}
		if c.responderClaim != "" {
			out = append(out, "responder_claim", "responder_value")
		}
		if c.groupsClaim != "" {
			out = append(out, "groups_claim")
		}
	}
	return out
}

// readPreferences reads the one row and resolves the design system it names through the registry
// reader, so there is one reader of that table and not two. setup_available and ai_review are the
// caller's to stamp: they are this host's configuration, not this row's.
// union is the row's list with the environment's added, each address once: admin is granted by
// either source (canyonv2-cr23), so the read answers both, the key stays editable, and a screen
// that sends the answer back does not send a repeat the PUT would refuse.
func union(row, env []string) []string {
	for _, e := range env {
		if !slices.Contains(row, e) {
			row = append(row, e)
		}
	}
	return row
}

func readPreferences(ctx context.Context, q querier) (preferences, error) {
	out := preferences{Stacks: []stack{}, ApprovedBuildpacks: []string{}, BuildpacksSeen: []executor.Buildpack{}, AllowedDomains: []string{}, AllowedEmails: []string{}, AdminEmails: []string{}, BootstrapAdmins: []string{}, ResponderEmails: []string{}, Overridden: []string{}}
	var stacks []stack
	var id *string
	var onboarded *time.Time
	// No WHERE: the table holds the one row, and the schema is what keeps it that way.
	if err := q.QueryRow(ctx, `SELECT stacks, approved_buildpacks, design_system_id, org_name, allowed_domains, allowed_emails, admin_emails, admin_claim, admin_value, responder_emails, responder_claim, responder_value, groups_claim, onboarded_at, run_by, access_grace, delivery, prod_approval, push_approvers_linked, patching, rebuilds, db_sessions FROM org_preferences`).Scan(&stacks, &out.ApprovedBuildpacks, &id, &out.OrgName, &out.AllowedDomains, &out.AllowedEmails, &out.AdminEmails, &out.AdminClaim, &out.AdminValue, &out.ResponderEmails, &out.ResponderClaim, &out.ResponderValue, &out.GroupsClaim, &onboarded, &out.RunBy, &out.AccessGrace, &out.Delivery, &out.ProdApproval, &out.PushLinked, &out.Patching, &out.Rebuilds, &out.DBSessions); err != nil {
		return out, err
	}
	if stacks != nil { // the column is NOT NULL, but a jsonb `null` literal decodes to nil: "stacks" is a list in every answer
		out.Stacks = stacks
	}
	// once per id, its newest deploy row's name, sorted by id (contracts/routes.md)
	if err := q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_strip_nulls(jsonb_build_object('id', id, 'name', name)) ORDER BY id), '[]') FROM (
		SELECT DISTINCT ON (bp->>'id') bp->>'id' AS id, bp->>'name' AS name FROM deploys, jsonb_array_elements(buildpacks) bp
		WHERE jsonb_typeof(buildpacks) = 'array' ORDER BY bp->>'id', deployed_at DESC LIMIT 256) seen`).Scan(&out.BuildpacksSeen); err != nil {
		return out, err
	}
	if onboarded != nil {
		at := onboarded.UTC().Format(time.RFC3339)
		out.OnboardedAt = &at
	}
	if id != nil {
		ds, err := designSystemRows(ctx, q, *id)
		if err != nil {
			return out, err
		}
		if len(ds) != 1 {
			return out, errors.New("the preferences name a design system the registry does not carry")
		}
		out.DesignSystem = resolvedOf(ds[0])
	}
	return out, nil
}

// orgRule is the contract's bounds on the organisation's settings, and the message a refusal carries.
const orgRule = "org_name is 1 to 128 characters of plain text; allowed_domains bare lower-case domains with a dot, 1 to 253 characters; allowed_emails and admin_emails lower-case addresses with an @, 1 to 254 characters; no list repeats an entry; admin_claim and admin_value are named together, both 1 to 256 characters or both null; responder_emails lower-case addresses as admin_emails, and responder_claim and responder_value named together as the admin pair; groups_claim is a claim name of 1 to 256 characters, or empty to clear it; onboarded_at is an RFC 3339 instant, set once and never changed; run_by is canyon or org"

// accessGraceRule is the contract's bound on access_grace, and the message its refusal carries; the
// shape is guardrails.schema.json's accessGrace pattern, the range the route's (design/HAND-OFF.md §1).
const accessGraceRule = "access_grace is a duration as Go spells it in hours, minutes and seconds, 0 to 24h"

var accessGraceRe = regexp.MustCompile(`^(0|([0-9]+h)?([0-9]+m)?([0-9]+s)?)$`)

func validAccessGrace(s string) bool {
	if s == "" || !accessGraceRe.MatchString(s) {
		return false
	}
	d, err := time.ParseDuration(s)
	return err == nil && d <= 24*time.Hour
}

func (a *app) putPreferences(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stacks          json.RawMessage `json:"stacks"` // absent is unchanged: a length of zero is the key nobody named
		Approved        json.RawMessage `json:"approved_buildpacks"`
		DesignSystemID  json.RawMessage `json:"design_system_id"` // absent is unchanged, a null clears it
		OrgName         json.RawMessage `json:"org_name"`
		AllowedDomains  json.RawMessage `json:"allowed_domains"`
		AllowedEmails   json.RawMessage `json:"allowed_emails"`
		AdminEmails     json.RawMessage `json:"admin_emails"`
		AdminClaim      json.RawMessage `json:"admin_claim"` // with admin_value: both strings, or both null to clear the pair
		AdminValue      json.RawMessage `json:"admin_value"`
		ResponderEmails json.RawMessage `json:"responder_emails"`
		ResponderClaim  json.RawMessage `json:"responder_claim"` // with responder_value: both strings, or both null to clear the pair
		ResponderValue  json.RawMessage `json:"responder_value"`
		GroupsClaim     json.RawMessage `json:"groups_claim"` // a claim name, or "" to clear it
		OnboardedAt     json.RawMessage `json:"onboarded_at"`
		RunBy           json.RawMessage `json:"run_by"`       // canyon or org (design/HAND-OFF.md §1)
		AccessGrace     json.RawMessage `json:"access_grace"` // a duration, 0 to 24h
		Delivery        json.RawMessage `json:"delivery"`     // webhook or repo (design/HAND-OFF.md §3b)
		ProdApproval    json.RawMessage `json:"prod_approval"`
		PushLinked      json.RawMessage `json:"push_approvers_linked"`
		Patching        json.RawMessage `json:"patching"`    // off or ask (design/PATCHING.md §4.5)
		Rebuilds        json.RawMessage `json:"rebuilds"`    // off or ask (design/PATCHING.md §5)
		DBSessions      json.RawMessage `json:"db_sessions"` // off or on (design/DATABASE-SESSIONS.md §5)
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if (len(body.Patching) > 0 || len(body.Rebuilds) > 0) && a.auth == authOIDC && !fromDashboard(r.Context()) { // before its value is read (design/PERMISSIONS.md, Platform patches): an admin's agent never turns patching on or off
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(r.Context(), "dashboard_required", "")
		return
	}
	named := map[string]bool{"org_name": len(body.OrgName) > 0, "allowed_domains": len(body.AllowedDomains) > 0, "allowed_emails": len(body.AllowedEmails) > 0, "admin_emails": len(body.AdminEmails) > 0, "admin_claim": len(body.AdminClaim) > 0, "admin_value": len(body.AdminValue) > 0, "responder_emails": len(body.ResponderEmails) > 0, "responder_claim": len(body.ResponderClaim) > 0, "responder_value": len(body.ResponderValue) > 0, "groups_claim": len(body.GroupsClaim) > 0, "run_by": len(body.RunBy) > 0, "access_grace": len(body.AccessGrace) > 0, "delivery": len(body.Delivery) > 0, "prod_approval": len(body.ProdApproval) > 0, "push_approvers_linked": len(body.PushLinked) > 0, "patching": len(body.Patching) > 0, "rebuilds": len(body.Rebuilds) > 0, "db_sessions": len(body.DBSessions) > 0}
	edit := prefEdit{setStacks: len(body.Stacks) > 0, setApproved: len(body.Approved) > 0, setDesignSystem: len(body.DesignSystemID) > 0, setAdmin: named["admin_claim"], setResponder: named["responder_claim"]}
	if !edit.setStacks && !edit.setApproved && !edit.setDesignSystem && len(body.OnboardedAt) == 0 && !slices.Contains(slices.Collect(maps.Values(named)), true) {
		writeErr(w, http.StatusBadRequest, "invalid_body", "a PUT names at least one key", nil)
		return
	}
	if edit.setStacks {
		if err := json.Unmarshal(body.Stacks, &edit.stacks); err != nil || edit.stacks == nil || !validStacks(edit.stacks) {
			writeErr(w, http.StatusBadRequest, "invalid_stacks", stacksRule, nil)
			return
		}
	}
	if edit.setApproved {
		if err := json.Unmarshal(body.Approved, &edit.approved); err != nil || edit.approved == nil || !validApproved(edit.approved) {
			writeErr(w, http.StatusBadRequest, "invalid_approved_buildpacks", approvedRule, nil)
			return
		}
		slices.Sort(edit.approved) // one set is one snapshot hash, whatever order it was named in
	}
	if edit.setDesignSystem {
		if err := json.Unmarshal(body.DesignSystemID, &edit.designSystemID); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_design_system", "design_system_id is the id of a registry row, or null to clear it", nil)
			return
		}
	}
	// The bounds, then the keys the operator pinned: both answered before the row is read.
	var ok bool
	if named["org_name"] {
		ok = json.Unmarshal(body.OrgName, &edit.orgName) == nil && edit.orgName != nil && plainText(*edit.orgName, 128)
	} else {
		ok = true
	}
	if named["allowed_domains"] {
		edit.allowedDomains, ok = listIn(body.AllowedDomains, 253, isDomain, ok)
	}
	if named["allowed_emails"] {
		edit.allowedEmails, ok = listIn(body.AllowedEmails, 254, isEmail, ok)
	}
	if named["admin_emails"] {
		edit.adminEmails, ok = listIn(body.AdminEmails, 254, isEmail, ok)
	}
	if edit.setAdmin { // named together, and a pair: two strings or two nulls
		ok = ok && named["admin_value"] && json.Unmarshal(body.AdminClaim, &edit.adminClaim) == nil && json.Unmarshal(body.AdminValue, &edit.adminValue) == nil && (edit.adminClaim == nil) == (edit.adminValue == nil) && (edit.adminClaim == nil || (plainText(*edit.adminClaim, 256) && plainText(*edit.adminValue, 256)))
	}
	if named["responder_emails"] {
		edit.responderEmails, ok = listIn(body.ResponderEmails, 254, isEmail, ok)
	}
	if edit.setResponder { // as the admin pair
		ok = ok && named["responder_value"] && json.Unmarshal(body.ResponderClaim, &edit.responderClaim) == nil && json.Unmarshal(body.ResponderValue, &edit.responderValue) == nil && (edit.responderClaim == nil) == (edit.responderValue == nil) && (edit.responderClaim == nil || (plainText(*edit.responderClaim, 256) && plainText(*edit.responderValue, 256)))
	}
	if named["groups_claim"] {
		ok = ok && json.Unmarshal(body.GroupsClaim, &edit.groupsClaim) == nil && edit.groupsClaim != nil && (*edit.groupsClaim == "" || plainText(*edit.groupsClaim, 256))
	}
	if named["run_by"] {
		ok = ok && json.Unmarshal(body.RunBy, &edit.runBy) == nil && edit.runBy != nil && (*edit.runBy == "canyon" || *edit.runBy == "org")
	}
	if named["delivery"] {
		ok = ok && json.Unmarshal(body.Delivery, &edit.delivery) == nil && edit.delivery != nil && (*edit.delivery == "webhook" || *edit.delivery == "repo")
	}
	if named["prod_approval"] {
		ok = ok && json.Unmarshal(body.ProdApproval, &edit.prodApproval) == nil && edit.prodApproval != nil
	}
	if named["push_approvers_linked"] {
		ok = ok && json.Unmarshal(body.PushLinked, &edit.pushLinked) == nil && edit.pushLinked != nil
	}
	if named["patching"] {
		ok = ok && json.Unmarshal(body.Patching, &edit.patching) == nil && edit.patching != nil && (*edit.patching == "off" || *edit.patching == "ask")
	}
	if named["rebuilds"] {
		ok = ok && json.Unmarshal(body.Rebuilds, &edit.rebuilds) == nil && edit.rebuilds != nil && (*edit.rebuilds == "off" || *edit.rebuilds == "ask")
	}
	if named["db_sessions"] {
		ok = ok && json.Unmarshal(body.DBSessions, &edit.dbSessions) == nil && edit.dbSessions != nil && (*edit.dbSessions == "off" || *edit.dbSessions == "on")
	}
	if len(body.OnboardedAt) > 0 {
		var raw string
		at, err := time.Time{}, json.Unmarshal(body.OnboardedAt, &raw)
		if err == nil {
			at, err = time.Parse(time.RFC3339, raw)
		}
		at = at.Truncate(time.Second) // the contract's onboarded_at is whole seconds; Parse accepts a fraction the row must not keep
		edit.onboardedAt, ok = &at, ok && err == nil
	}
	if !ok || (named["admin_value"] && !named["admin_claim"]) || (named["responder_value"] && !named["responder_claim"]) {
		writeErr(w, http.StatusBadRequest, "invalid_body", orgRule, nil)
		return
	}
	if named["access_grace"] { // its own refusal, by name (contracts/routes.md): outside its shape or its range
		var grace string
		if json.Unmarshal(body.AccessGrace, &grace) != nil || !validAccessGrace(grace) {
			writeErr(w, http.StatusBadRequest, "invalid_access_grace", accessGraceRule, nil)
			return
		}
		edit.accessGrace = &grace
	}
	switch { // design/AUTH.md §4.5: never on without sign-in, and never changed from the CLI, whose admin's agent could turn it off and promote
	case named["prod_approval"] && *edit.prodApproval && a.auth != authOIDC:
		writeErr(w, http.StatusConflict, "signin_required", "production approval needs sign-in on: without it Canyon can't tell the dashboard from the CLI", nil)
		return
	case named["prod_approval"] && a.auth == authOIDC && !fromDashboard(r.Context()):
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(r.Context(), "dashboard_required", "")
		return
	case named["push_approvers_linked"] && *edit.pushLinked && a.auth != authOIDC: // #2342: nobody links a GitHub account without sign-in
		writeErr(w, http.StatusConflict, "signin_required", "asking approvers of a push to link GitHub needs sign-in on: without it nobody can link an account", nil)
		return
	case named["push_approvers_linked"] && a.auth == authOIDC && !fromDashboard(r.Context()): // an admin's agent never loosens who approves from the CLI
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(r.Context(), "dashboard_required", "")
		return
	case named["patching"] && *edit.patching == "ask" && a.auth != authOIDC: // design/PATCHING.md §4.5: nobody could decide a patch approval
		writeErr(w, http.StatusConflict, "signin_required", "patching needs sign-in on: without it nobody can approve a patch for production", nil)
		return
	case named["db_sessions"] && *edit.dbSessions == "on" && actorOf(r.Context()).Kind != "person": // a widening of who reaches app data is a person's (#2835)
		writeErr(w, http.StatusForbidden, "person_required", "Only a person turns database sessions on, in Canyon's dashboard.", nil)
		deny(r.Context(), "person_required", "")
		return
	case named["db_sessions"] && *edit.dbSessions == "on" && a.auth == authOIDC && !fromDashboard(r.Context()): // and from a browser: an admin's agent never opens the path for itself; off narrows, from any session
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(r.Context(), "dashboard_required", "")
		return
	case named["rebuilds"] && *edit.rebuilds == "ask" && a.auth != authOIDC: // as patching: nobody could decide a rebuild's approval
		writeErr(w, http.StatusConflict, "signin_required", "rebuilds need sign-in on: without it nobody can approve a rebuild for production", nil)
		return
	}
	if named["rebuilds"] && *edit.rebuilds == "ask" { // a rebuild is a patch: never on while patching is off, as the PUT would leave it
		patching := edit.patching
		if patching == nil {
			patching = new(string)
			if err := a.pool.QueryRow(r.Context(), "SELECT patching FROM org_preferences").Scan(patching); err != nil {
				a.log.Printf("canyon-server: preferences: %v", err)
				writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
				return
			}
		}
		if *patching != "ask" {
			writeErr(w, http.StatusConflict, "patching_off", "Turn on patching first: Canyon rebuilds apps only while it patches them.", nil)
			return
		}
	}
	if (edit.setApproved || edit.setDesignSystem) && a.dashboardRequired(w, r) { // the buildpacks stack.blessed approves and the system design.org-system checks are a rule's, changed where a policy is (#1966)
		return
	}
	if pinned := slices.DeleteFunc(a.overriddenKeys(), func(key string) bool { return !named[key] }); len(pinned) > 0 { // a screen could otherwise accept an edit that silently does nothing (contracts/routes.md)
		writeErr(w, http.StatusConflict, "set_by_operator", "the environment sets these keys, so a PUT cannot", map[string]any{"keys": pinned})
		return
	}
	act := action(r)
	if edit.onboardedAt != nil { // the write that ends the setup state, by name on the trail (design/ONBOARDING.md step 4)
		act = "complete setup"
	}
	// One row naming only the keys that moved, as typed before and after values. A PUT that repeats
	// what is stored changed nothing, so it writes nothing.
	before, after, err := a.applyPreferences(r.Context(), actorOf(r.Context()), act, edit,
		func(before, after preferences) map[string]any {
			detail := map[string]any{}
			for _, k := range []struct {
				key      string
				named    bool
				was, now any
			}{
				{"stacks", edit.setStacks, before.Stacks, after.Stacks},
				{"approved_buildpacks", edit.setApproved, before.ApprovedBuildpacks, after.ApprovedBuildpacks},
				{"design_system_id", edit.setDesignSystem, idOf(before.DesignSystem), idOf(after.DesignSystem)},
				{"org_name", named["org_name"], before.OrgName, after.OrgName},
				{"allowed_domains", named["allowed_domains"], before.AllowedDomains, after.AllowedDomains},
				{"allowed_emails", named["allowed_emails"], before.AllowedEmails, after.AllowedEmails},
				{"admin_emails", named["admin_emails"], before.AdminEmails, after.AdminEmails},
				{"admin_claim", edit.setAdmin, before.AdminClaim, after.AdminClaim},
				{"admin_value", edit.setAdmin, before.AdminValue, after.AdminValue},
				{"responder_emails", named["responder_emails"], before.ResponderEmails, after.ResponderEmails},
				{"responder_claim", edit.setResponder, before.ResponderClaim, after.ResponderClaim},
				{"responder_value", edit.setResponder, before.ResponderValue, after.ResponderValue},
				{"groups_claim", named["groups_claim"], before.GroupsClaim, after.GroupsClaim},
				{"onboarded_at", edit.onboardedAt != nil, before.OnboardedAt, after.OnboardedAt},
				{"run_by", named["run_by"], before.RunBy, after.RunBy},
				{"access_grace", named["access_grace"], before.AccessGrace, after.AccessGrace},
				{"delivery", named["delivery"], before.Delivery, after.Delivery},
				{"prod_approval", named["prod_approval"], before.ProdApproval, after.ProdApproval},
				{"push_approvers_linked", named["push_approvers_linked"], before.PushLinked, after.PushLinked},
				{"patching", named["patching"], before.Patching, after.Patching},
				{"rebuilds", named["rebuilds"] || named["patching"], before.Rebuilds, after.Rebuilds}, // patching off turns rebuilds off with it
				{"db_sessions", named["db_sessions"], before.DBSessions, after.DBSessions},
			} {
				if k.named && changed(k.was, k.now) {
					detail[k.key] = map[string]any{"before": k.was, "after": k.now}
				}
			}
			return detail
		})
	if err == nil && changed(admission(before), admission(after)) { // before the answer: every running app's file says who may open it (the front-door spec's B1)
		_, _, err = a.reputAll(r.Context())
	}
	if err == nil && before.DBSessions == "on" && after.DBSessions == "off" { // before the answer: nothing of a database session outlives the switch (#2835)
		err = a.turnDBSessionsOff(context.WithoutCancel(r.Context()))
	}
	var locked lockedOutErr
	var need handOffNeed
	var held *credentials
	var lic licenceErr
	switch {
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
	case errors.As(err, &locked):
		writeErr(w, http.StatusBadRequest, "admin_locked_out", "an admin has to stay able to sign in, and "+locked.admin+" is on neither membership list", nil)
	case errors.Is(err, errNoDesignSystem):
		writeErr(w, http.StatusNotFound, "not_found", "no design system carries that id", nil)
	case errors.Is(err, errNoMembers):
		writeErr(w, http.StatusBadRequest, "no_members", "at least one person or one domain has to be allowed to sign in", nil)
	case errors.Is(err, errNoAdmins):
		writeErr(w, http.StatusBadRequest, "no_admins", "at least one admin address, or an admin claim, has to be set", nil)
	case errors.Is(err, errOnboarded):
		writeErr(w, http.StatusBadRequest, "invalid_body", orgRule, nil)
	case errors.Is(err, errNotKubernetes):
		writeErr(w, http.StatusConflict, "not_kubernetes", onDocker, nil)
	case errors.As(err, &need):
		writeErr(w, http.StatusConflict, need.Code, need.Message, nil)
	case errors.Is(err, errWebhookRequired):
		writeErr(w, http.StatusConflict, "webhook_required", "save the platform webhook first: it is where releases go", nil)
	case errors.Is(err, errRepoRequired):
		writeErr(w, http.StatusConflict, "repo_required", repoRequired, nil)
	case errors.Is(err, errFrontDoorRequired):
		writeErr(w, http.StatusConflict, "front_door_required", "set the front door first: it is what the sidecar in your pods believes", nil)
	case errors.Is(err, errSigninUnsupported):
		writeErr(w, http.StatusConflict, "signin_unsupported", signinUnsupported, nil)
	case errors.Is(err, errFrontDoorUnchecked):
		writeErr(w, http.StatusConflict, "front_door_unchecked", frontDoorUnchecked, nil)
	case errors.As(err, &held):
		writeErr(w, http.StatusConflict, "credentials_exist", credentialsExist(held.apps), map[string]any{"apps": held.apps})
	case err != nil:
		a.log.Printf("canyon-server: preferences: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		writeJSON(w, http.StatusOK, a.asRead(r.Context(), after)) // the PUT answers the GET shape, so it answers it through the same projection
	}
}

// admission is what of the org's settings an access file carries: who is a member, who an admin,
// which claim holds a person's groups, and the setup state — accessFile admits admins alone while
// it is set (design/ONBOARDING.md §2), so finishing setup changes every file even naming nothing else.
func admission(p preferences) []any {
	return []any{p.AllowedDomains, p.AllowedEmails, p.AdminEmails, p.AdminClaim, p.AdminValue, p.GroupsClaim, p.OnboardedAt}
}

// listIn decodes one of the three lists: an array of distinct lower-case entries of the kind's
// shape, each plain text within max characters. ok false in is false out, so the checks chain.
func listIn(raw json.RawMessage, max int, kind func(string) bool, ok bool) ([]string, bool) {
	var out []string
	if !ok || json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, false
	}
	for i, s := range out {
		if !plainText(s, max) || s != strings.ToLower(s) || !kind(s) || slices.Contains(out[:i], s) {
			return nil, false
		}
	}
	return out, true
}

// prefEdit is one write to the org's row: a key nobody named is left alone.
type prefEdit struct {
	stacks          []stack
	setStacks       bool
	approved        []string
	setApproved     bool
	designSystemID  *string
	setDesignSystem bool // a nil id with the flag set clears it
	orgName         *string
	allowedDomains  []string // nil: not named; empty: named and cleared
	allowedEmails   []string
	adminEmails     []string
	adminClaim      *string
	adminValue      *string
	setAdmin        bool // the pair was named; nil claim and value clear it
	responderEmails []string
	responderClaim  *string
	responderValue  *string
	setResponder    bool // as setAdmin
	groupsClaim     *string
	onboardedAt     *time.Time
	runBy           *string
	accessGrace     *string
	delivery        *string
	prodApproval    *bool
	pushLinked      *bool
	patching        *string
	rebuilds        *string
	dbSessions      *string
}

// turnsOn is whether the edit turns patching, rebuilds or both on, judged against the row before it: moved to ask.
// Canyon Enterprise's hook checks the licence and writes it (turnPatchingOn); turning either off, or naming what is
// stored, is free, so a lapsed licence never stops an admin turning patching off (contracts/routes.md, Licence).
func (e prefEdit) turnsOn(before preferences) (patching, rebuilds bool) {
	return e.patching != nil && *e.patching == "ask" && before.Patching != "ask", e.rebuilds != nil && *e.rebuilds == "ask" && before.Rebuilds != "ask"
}

// offOnly is a patching or rebuilds edit as the core writes it: off alone, ask being Canyon Enterprise's write.
func offOnly(v *string) *string {
	if v == nil || *v != "off" {
		return nil
	}
	return v
}

// newClaim is whether the edit sets a groups claim other than the stored one: Canyon Enterprise's (ee/groupsync).
// Clearing it, or naming the one stored, is the core's and free.
func (e prefEdit) newClaim(before preferences) bool {
	return e.groupsClaim != nil && *e.groupsClaim != "" && *e.groupsClaim != before.GroupsClaim
}

// refuse is the row's two rules and the one-way key, judged on the row as the PUT would leave it:
// before, with every list and the pair the edit names replaced. The membership rule is judged when
// the PUT names a membership list or completes setup, the admin rule when it names the admin list,
// the pair or completes setup: a PUT naming only the stacks on a host whose lists are empty — every
// host with auth off, and the setup chat's tools — is not refused for a membership it did not touch.
func (e prefEdit) refuse(before preferences) error {
	// The admins are both sources: admin is granted by the row's list or by CANYON_ADMINS, and the
	// read answers them apart, so the rule unions here what it used to be handed merged.
	domains, emails, admins, claim := named(e.allowedDomains, before.AllowedDomains), named(e.allowedEmails, before.AllowedEmails), union(named(e.adminEmails, before.AdminEmails), before.BootstrapAdmins), before.AdminClaim
	if e.setAdmin {
		claim = e.adminClaim
	}
	switch {
	case e.onboardedAt != nil && before.OnboardedAt != nil:
		return errOnboarded
	case (e.allowedDomains != nil || e.allowedEmails != nil || e.onboardedAt != nil) && len(domains)+len(emails) == 0:
		return errNoMembers
	case (e.adminEmails != nil || e.setAdmin || e.onboardedAt != nil) && len(admins) == 0 && claim == nil:
		return errNoAdmins
	}
	return nil
}

// lockedOut is the third invariant: at least one admin address has to be on a membership list.
// Judged on the row the write leaves with the environment over it — the only point that sees that,
// which is why it is not in refuse, whose argument is the row before the write. Without it a PUT
// naming a membership list the signer is not on, and the setup instant, answers 200 and refuses
// that signer on their next request, including on the PUT that would undo it: one transposed
// letter in a domain ends setup with nobody able to sign in (canyonv2-cr23). At least one, not
// every one — a stale CANYON_ADMINS entry would otherwise refuse every routine membership edit,
// and the refusal would land on somebody who cannot edit that key. An admin claim does not satisfy
// it: it names no address, and a claim matching nobody looks like one matching everybody.
func lockedOut(e prefEdit, after preferences) error {
	if e.allowedDomains == nil && e.allowedEmails == nil && e.adminEmails == nil && !e.setAdmin && e.onboardedAt == nil {
		return nil // a stacks-only write on an auth-off host is not this rule's business
	}
	admins := union(after.AdminEmails, after.BootstrapAdmins) // either source can be the admin who stays able to sign in
	if len(admins) == 0 {
		return nil // no address to place: no_admins has already had its say, or this host has no auth
	}
	for _, admin := range admins {
		if memberOf(admin, after.AllowedDomains, after.AllowedEmails) {
			return nil
		}
	}
	return lockedOutErr{admin: admins[0]} // the stored address, never the request's: somebody staring at their own typo is shown the address they were trying to match
}

// lockedOutErr carries the admin address the rule could not place, for the message to name.
type lockedOutErr struct{ admin string }

func (e lockedOutErr) Error() string { return e.admin + " is on neither membership list" }

// named is the list the PUT named, or the row's when it named none.
func named(edit, row []string) []string {
	if edit != nil {
		return edit
	}
	return row
}

// errNoDesignSystem is an id no registry row carries: the PUT answers 404, and the setup tool
// answers a refusal the model reads. The other three are the PUT's refusals on the row.
var (
	errNoDesignSystem = errors.New("no design system carries that id")
	errNoMembers      = errors.New("both membership lists would be empty")
	errNoAdmins       = errors.New("no admin address and no admin claim")
	errOnboarded      = errors.New("onboarded_at is already set")
)

// run_by: org's refusals (design/HAND-OFF.md §1 and §6), in the order the PUT answers them; the
// server's own settings, a handOffNeed each, come between the first two.
var (
	errNotKubernetes     = errors.New("run_by: org is a Kubernetes host's alone")
	errWebhookRequired   = errors.New("run_by: org needs the platform webhook saved")
	errRepoRequired      = errors.New("run_by: org under delivery: repo needs the deploy repo saved")
	errFrontDoorRequired = errors.New("run_by: org needs a front door set")
	errSigninUnsupported = errors.New("run_by: org needs the front door at choice 1")
	// run_by back to canyon behind a front door that has not passed the probe (#2438).
	errFrontDoorUnchecked = errors.New("run_by: canyon needs a front door that passed the probe")
)

// handOffNeed is a setting run_by: org needs of the server itself and this one lacks (#2010): the
// PUT's 409, a line of the front door's read, and a reason the server refuses to start under org.
type handOffNeed struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (n handOffNeed) Error() string { return n.Message }

// handOffNeeds is what this server lacks to serve run_by: org, in the order the PUT refuses it: the
// sidecar's image every release names, and an https public URL, the sidecar's access URL.
func handOffNeeds(p *runs.Pipeline) []handOffNeed {
	out := []handOffNeed{}
	if p.IdentityImage == "" {
		out = append(out, handOffNeed{"identity_image_required", identityImageRequired})
	}
	if !strings.HasPrefix(p.PublicURL, "https://") {
		out = append(out, handOffNeed{"https_required", httpsRequired})
	}
	return out
}

// refuseOrg judges a PUT naming run_by: org before anything is written: a Kubernetes host, the
// server's own settings, where releases go saved — the platform webhook under delivery: webhook, the deploy
// repo under repo (#2470) — a front door set, that door at choice 1, and no app holding a value route, which works
// only through Canyon, each read inside the mutation's transaction. A PUT of delivery while run_by is org needs the one it names saved too. A PUT moving run_by
// from org back to canyon needs no front door, or one that passed the probe (#2438). Anything else asks
// nothing.
func (a *app) refuseOrg(ctx context.Context, q querier, e prefEdit, before preferences) error {
	was := before.RunBy
	delivery := before.Delivery
	if e.delivery != nil {
		delivery = *e.delivery
	}
	if e.delivery != nil && was == "org" && (e.runBy == nil || *e.runBy == "org") {
		if err := a.deliverySaved(ctx, q, delivery); err != nil {
			return err
		}
	}
	if e.runBy != nil && *e.runBy == "canyon" && was == "org" {
		door, err := readDoor(ctx, q)
		if err == nil && door != nil && !door.Probed {
			err = errFrontDoorUnchecked
		}
		return err
	}
	if e.runBy == nil || *e.runBy != "org" {
		return nil
	}
	if !a.kube {
		return errNotKubernetes
	}
	if needs := handOffNeeds(a.pipe); len(needs) > 0 {
		return needs[0]
	}
	if err := a.deliverySaved(ctx, q, delivery); err != nil {
		return err
	}
	door, err := readDoor(ctx, q)
	if err != nil {
		return err
	}
	if door == nil {
		return errFrontDoorRequired
	}
	if door.Choice == 2 {
		return errSigninUnsupported
	}
	apps, err := credentialsHeld(ctx, q) // a route works only through Canyon
	if err != nil || len(apps) == 0 {
		return err
	}
	return &credentials{apps}
}

// applyPreferences writes the edit and its audit row in one transaction and answers the row
// before and after. detail returning nothing means nothing moved: no audit row. It is the one
// writer of this table — a person's form and the setup chat's tool take the same path, so the
// advisory lock in a.mutation serialises them against each other.
func (a *app) applyPreferences(ctx context.Context, act actor, action string, e prefEdit,
	detail func(before, after preferences) map[string]any) (preferences, preferences, error) {
	var before, after preferences
	tx, err := a.mutation(ctx)
	if err != nil {
		return before, after, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if e.setDesignSystem && e.designSystemID != nil {
		// Read the registry inside the transaction and before any write, so the refusal undoes
		// nothing and writes no audit row — never a decoded foreign-key violation. The reader takes
		// an empty id to mean every row, so the answer is checked against what was asked for.
		ds, err := designSystemRows(ctx, tx, *e.designSystemID)
		if err != nil {
			return before, after, err
		}
		if len(ds) != 1 || ds[0].ID != *e.designSystemID {
			return before, after, errNoDesignSystem
		}
	}
	if before, err = a.orgPreferences(ctx, tx); err != nil {
		return before, after, err
	}
	if err := e.refuse(before); err != nil {
		return before, after, err
	}
	if patching, rebuilds := e.turnsOn(before); patching || rebuilds {
		if err := a.ee.turnPatchingOn(ctx, tx, patching, rebuilds); err != nil {
			return before, after, err
		}
	}
	if e.newClaim(before) { // after patching's check, in features' order: written here, and kept by the statement below
		if err := a.ee.setGroupsClaim(ctx, tx, *e.groupsClaim); err != nil {
			return before, after, err
		}
	}
	if err := a.refuseOrg(ctx, tx, e, before); err != nil {
		return before, after, err
	}
	// One statement per named key, no dynamic SQL; pgx encodes the slice to jsonb the way an audit
	// row's detail is encoded.
	if e.setStacks {
		if _, err := tx.Exec(ctx, `UPDATE org_preferences SET stacks = $1`, e.stacks); err != nil {
			return before, after, err
		}
	}
	if e.setApproved {
		if _, err := tx.Exec(ctx, `UPDATE org_preferences SET approved_buildpacks = $1`, e.approved); err != nil {
			return before, after, err
		}
	}
	if e.setDesignSystem {
		if _, err := tx.Exec(ctx, `UPDATE org_preferences SET design_system_id = $1`, e.designSystemID); err != nil {
			return before, after, err
		}
	}
	// The organisation's settings in one static statement: a key the edit did not name is NULL
	// here and coalesce keeps the row's; the pair moves only when named, both or neither. patching and rebuilds are
	// written off alone (offOnly): ask is Canyon Enterprise's, written above.
	if _, err := tx.Exec(ctx, `UPDATE org_preferences SET org_name = coalesce($1, org_name), allowed_domains = coalesce($2, allowed_domains), allowed_emails = coalesce($3, allowed_emails), admin_emails = coalesce($4, admin_emails), admin_claim = CASE WHEN $5 THEN $6 ELSE admin_claim END, admin_value = CASE WHEN $5 THEN $7 ELSE admin_value END, onboarded_at = coalesce($8, onboarded_at), groups_claim = CASE WHEN $9 = '' THEN '' ELSE groups_claim END, run_by = coalesce($10, run_by), access_grace = coalesce($11, access_grace), prod_approval = coalesce($12, prod_approval), responder_emails = coalesce($13, responder_emails), responder_claim = CASE WHEN $14 THEN $15 ELSE responder_claim END, responder_value = CASE WHEN $14 THEN $16 ELSE responder_value END, push_approvers_linked = coalesce($17, push_approvers_linked), delivery = coalesce($18, delivery), patching = coalesce($19, patching), rebuilds = CASE WHEN coalesce($19, patching) = 'off' THEN 'off' ELSE coalesce($20, rebuilds) END, db_sessions = coalesce($21, db_sessions)`, e.orgName, e.allowedDomains, e.allowedEmails, e.adminEmails, e.setAdmin, e.adminClaim, e.adminValue, e.onboardedAt, e.groupsClaim, e.runBy, e.accessGrace, e.prodApproval, e.responderEmails, e.setResponder, e.responderClaim, e.responderValue, e.pushLinked, e.delivery, offOnly(e.patching), offOnly(e.rebuilds), e.dbSessions); err != nil {
		return before, after, err
	}
	// The answer is the row as it now stands, read back rather than assembled in memory, so a bad
	// write cannot answer as a good one.
	if after, err = a.orgPreferences(ctx, tx); err != nil {
		return before, after, err
	}
	if err := lockedOut(e, after); err != nil { // before the audit append and the commit: the deferred rollback undoes the write, and no row records a refusal
		return before, after, err
	}
	// The project is empty — the preferences belong to the org, so this writes no timeline row.
	if d := detail(before, after); len(d) > 0 {
		if err := events.AuditTx(ctx, tx, act, action, "", "", d); err != nil {
			return before, after, err
		}
	}
	return before, after, tx.Commit(ctx)
}

// approvedRule is the contract's bound on the approved buildpacks, and the message a refusal carries.
const approvedRule = "approved_buildpacks is at most 64 buildpack ids, each once: letters, digits, '.', '/' and '-', at most 128 characters"

var buildpackID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./-]{0,127}$`)

func validApproved(ids []string) bool {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !buildpackID.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return len(ids) <= 64
}

// validStacks is the contract's rule for the approved list. Two entries for one language make the
// list two answers to one question, so the languages are compared byte for byte and must differ.
func validStacks(ss []stack) bool {
	if len(ss) > 16 {
		return false
	}
	seen := make(map[string]bool, len(ss))
	for _, s := range ss {
		if !plainText(s.Language, 64) || seen[s.Language] || len(s.Frameworks) == 0 || len(s.Frameworks) > 16 {
			return false
		}
		seen[s.Language] = true
		for _, f := range s.Frameworks {
			if !plainText(f, 64) {
				return false
			}
		}
	}
	return true
}

func idOf(r *resolved) *string {
	if r == nil {
		return nil
	}
	return &r.ID
}

// changed compares the two sides of one key as the JSON an answer would carry: a list of strings
// and an id, neither of which encoding/json refuses.
func changed(before, after any) bool {
	was, _ := json.Marshal(before)
	now, _ := json.Marshal(after)
	return !bytes.Equal(was, now)
}

// repoRequired is the repo_required refusal's message (design/EXPERIENCE.md).
const repoRequired = "Save your deploy repo first."

// deliverySaved is whether where releases go under delivery is saved: the platform webhook's url, or the deploy
// repo's row.
func (a *app) deliverySaved(ctx context.Context, q querier, delivery string) error {
	if delivery == "repo" {
		var saved bool
		if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform_repo)").Scan(&saved); err != nil {
			return err
		}
		if !saved {
			return errRepoRequired
		}
		return nil
	}
	hook, err := readWebhook(ctx, q)
	if err != nil {
		return err
	}
	if hook == nil {
		return errWebhookRequired
	}
	return nil
}
