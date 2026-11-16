package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// The Canyon Enterprise licence (contracts/routes.md, Licence; design/PERMISSIONS.md, Licence; #2879). Its keys, its
// reading and its verification are Canyon Enterprise's (ee/licence), which the server reaches through its licence
// hook alone (design/ARCHITECTURE.md §8); in a build without it there is no licence, and every Enterprise change is
// refused as one with no licence is. Nothing that runs reads it, so a licence that lapses never weakens the review,
// never stops what runs, and changes nobody's access.

// licenceRequired is licence_required's message, by feature (design/EXPERIENCE.md, D5).
var licenceRequired = map[string]string{
	"outbound_keys":     "Adding credentials to apps' calls needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
	"platform_patching": "Turning on patching needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
	"group_sync":        "Access by sign-in group needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
}

// licenceActor is Canyon, as the trail names it when patching at upgrade turns a new install's patching off.
var licenceActor = actor{Kind: "canyon", Subject: "licence", Display: "Canyon"}

// licenceErr is an Enterprise feature's change refused for want of a licence that unlocks it, or of Canyon
// Enterprise's code: ee/licence answers it as the core's own type.
type licenceErr = enterprise.Required

// writeLicenceRequired answers 409 licence_required for e's feature.
func writeLicenceRequired(w http.ResponseWriter, e licenceErr) {
	writeErr(w, http.StatusConflict, "licence_required", licenceRequired[e.Feature], map[string]any{"feature": e.Feature})
}

func routesLicence(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /licence", a.getLicence)
	api.HandleFunc("PUT /licence", a.adminOnly(a.putLicence))
}

// getLicence answers every member whether a licence is in force and what it unlocks, and an admin all of it.
func (a *app) getLicence(w http.ResponseWriter, r *http.Request) {
	out, err := a.ee.read(r.Context(), a.pool)
	if err != nil {
		a.internal(w, "licence", err)
		return
	}
	if roleOf(r.Context()) != "admin" {
		out = enterprise.Licence{State: out.State, Allows: out.Allows}
	}
	writeJSON(w, http.StatusOK, out)
}

// putLicence stores a licence an admin pasted, in the dashboard, once a compiled key verifies it; a build without
// Canyon Enterprise has none to verify it with.
func (a *app) putLicence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch {
	case actorOf(ctx).Kind != "person":
		writeErr(w, http.StatusForbidden, "person_required", "A person sets Canyon's licence, in the dashboard: Settings › Licence.", nil)
		deny(ctx, "person_required", "")
		return
	case a.auth == authOIDC && !fromDashboard(ctx):
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", "")
		return
	}
	if a.ee.licence == nil {
		writeErr(w, http.StatusConflict, "enterprise_absent", "This Canyon is built without Canyon Enterprise, so it can't use a licence.", nil)
		return
	}
	var body struct {
		Licence *string `json:"licence"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Licence == nil || *body.Licence == "" || len(*body.Licence) > 8192 {
		writeErr(w, http.StatusBadRequest, "bad_request", `the body is {"licence": "<the licence file's text>"}, at most 8 KiB`, nil)
		return
	}
	if a.ee.licence.SetByOperator() {
		writeErr(w, http.StatusConflict, "set_by_operator", "CANYON_LICENSE sets this server's licence, so it can't be changed in the dashboard.", nil)
		return
	}
	text := strings.TrimSpace(*body.Licence)
	var out enterprise.Licence
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		p, err := a.ee.licence.Store(ctx, tx, text)
		if err != nil {
			return err
		}
		if err := events.AuditTx(ctx, tx, actorOf(ctx), action(r), "", "", map[string]any{"customer": p.Customer, "expires": stamp(p.Expires), "features": p.Features}); err != nil {
			return err
		}
		out, err = a.ee.licence.Read(ctx, tx)
		return err
	})
	if errors.Is(err, enterprise.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid_licence", "This isn't a Canyon licence, or Canyon didn't sign it.", nil)
		return
	}
	if err != nil {
		a.internal(w, "licence", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// patchingAtStart is patching at upgrade (contracts/routes.md, Licence; #2881): what an install runs when it
// upgrades keeps running, and a new one starts off. Without a licence in force naming platform_patching, a host still
// in setup with no app writes patching off, on the trail by Canyon, and a server set to patch (patches, CANYON_PATCHING) that then patches
// nothing says so once. A host with an app, or one past setup, keeps the patching it stored. A server built without
// Canyon Enterprise does the same, and, set to patch, says once that it patches nothing whatever patching stores.
func (a *app) patchingAtStart(ctx context.Context, patches bool, logger *log.Logger) error {
	var lic licenceErr
	switch err := a.ee.require(ctx, a.pool, platformPatching); {
	case err == nil:
		return nil
	case !errors.As(err, &lic):
		return err
	}
	var off bool
	if err := a.inMutation(ctx, func(tx pgx.Tx) error {
		var wrote bool
		if err := tx.QueryRow(ctx, `WITH fresh AS (UPDATE org_preferences SET patching = 'off'
			WHERE patching = 'ask' AND onboarded_at IS NULL AND NOT EXISTS (SELECT 1 FROM projects) RETURNING patching)
			SELECT EXISTS (SELECT 1 FROM fresh), coalesce((SELECT patching FROM fresh), (SELECT patching FROM org_preferences)) = 'off'`).Scan(&wrote, &off); err != nil || !wrote {
			return err
		}
		return events.AuditTx(ctx, tx, licenceActor, "patching off at start", "", "", map[string]any{"patching": map[string]string{"before": "ask", "after": "off"}})
	}); err != nil {
		return err
	}
	switch {
	case patches && a.ee.patching == nil:
		logger.Print("canyon-server: CANYON_PATCHING is on, but this Canyon is built without Canyon Enterprise, so it patches nothing")
	case patches && off:
		logger.Print("canyon-server: CANYON_PATCHING is on, but Canyon patches nothing: patching is off in Settings › App hosting, and turning it on needs a licence, which an admin adds in Settings › Licence")
	}
	return nil
}

// patchingNeed is the org's need while the server is set to patch (CANYON_PATCHING) but patching is off and no licence
// in force names platform_patching, or the server is built without Canyon Enterprise whatever patching stores, so
// Canyon patches nothing (contracts/routes.md, What needs a person, patching_unlicensed). ok is false while there is
// none.
func (a *app) patchingNeed(ctx context.Context, q querier) (need, bool, error) {
	switch {
	case a.pipe == nil || !a.pipe.Patches.Load():
		return need{}, false, nil
	case a.ee.patching == nil:
		return newNeed("patching_unlicensed", nil, nil, true, nil), true, nil
	}
	var off bool
	if err := q.QueryRow(ctx, "SELECT patching = 'off' FROM org_preferences").Scan(&off); err != nil || !off {
		return need{}, false, err
	}
	var lic licenceErr
	switch err := a.ee.require(ctx, q, platformPatching); {
	case errors.As(err, &lic):
		return newNeed("patching_unlicensed", nil, nil, true, nil), true, nil
	default:
		return need{}, false, err
	}
}

// licenceNeed is the org's need while the licence is in grace, lapsed or invalid (contracts/routes.md, What needs a
// person, licence): an admin's alone, as orgNeeds is. ok is false while there is none.
func (a *app) licenceNeed(ctx context.Context, q querier) (need, bool, error) {
	r, err := a.ee.read(ctx, q)
	if err != nil || r.State == "none" || r.State == "valid" {
		return need{}, false, err
	}
	fields := map[string]any{"licence": r.State}
	if r.State == "grace" {
		fields["grace_until"] = r.GraceUntil
	}
	var since *time.Time
	if at, err := time.Parse(time.RFC3339, r.Expires); err == nil { // an invalid licence has no expiry: since is null
		since = &at
	}
	return newNeed("licence", nil, since, true, fields), true, nil
}
