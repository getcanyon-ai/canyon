package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/destination"
	"github.com/getcanyon-ai/canyon/runs"
)

// outboundPerson is person_required's message on every outbound change (contracts/routes.md, Egress control).
const outboundPerson = "a person changes how apps call out"

// Layer 2, Restrict (design/EGRESS.md §3.3–3.5; design/PERMISSIONS.md, Outbound): an app env's mode, its
// allowlist and its egress requests. Widening — allowing a destination, an app env's mode back to the org's or
// looser — is a person's, from a browser; narrowing works from any session and any actor; dismissing is a
// person's. Each change reaches egress-control with the policy's next version.

// outboundTeamOnly is a route on one app env's outbound: an unknown or deleted app, or an env other than preview
// or prod, is 404, then a caller below contributor is 403 team_required, both before anything else is read.
func (a *app) outboundTeamOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, id := r.Context(), r.PathValue("id")
		if env := r.PathValue("env"); env != "preview" && env != "prod" {
			writeErr(w, http.StatusNotFound, "not_found", "an app's envs are preview and prod", nil)
			return
		}
		if a.projectLive(ctx, w, id) && !a.teamRequired(ctx, w, a.pool, id) {
			next(w, r)
		}
	}
}

// refuseWidening answers a widening's actor and session refusals, each one denied row: 403 person_required to
// an agent or canyon actor, then, on a host with sign-in, 403 dashboard_required from any session but a
// browser's. true means the response is written.
func (a *app) refuseWidening(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	switch {
	case actorOf(ctx).Kind != "person":
		writeErr(w, http.StatusForbidden, "person_required", outboundPerson, nil)
		deny(ctx, "person_required", r.PathValue("id"))
	case a.auth == authOIDC && !fromDashboard(ctx): // with sign-in off no session tells the browser from the CLI
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", r.PathValue("id"))
	default:
		return false
	}
	return true
}

// heldProd answers 409 held when env is prod and its production is held, read on tx under the ledger lock the
// hold is placed under. true means the response is written.
func (a *app) heldProd(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, id, env string) (bool, error) {
	if env != "prod" {
		return false, nil
	}
	h, err := runs.ReadHold(ctx, tx, id)
	if err != nil || h == nil {
		return false, err
	}
	a.refuseHeld(ctx, w, tx, *h)
	return true, nil
}

// putEgressMode sets an app env's own mode: an owner's stricter than the org's, allowlist, or back to the org's,
// null; an admin's any of the three. The body is read after the level and before the actor, on this route
// alone, since only the mode it names says whether the change widens: null and observe do, never stricter than
// anything. A mode is refused while the app's floor is not enforced, its calls able to go around Envoy, and
// while the org runs its apps. One egress.mode.set row when the env's own mode moves.
func (a *app) putEgressMode(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	if a.ownerRequired(ctx, w, a.pool, id) {
		return
	}
	var body map[string]json.RawMessage
	var mode *string
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF || len(body) != 1 || body["mode"] == nil || //nolint:errorlint // sentinel from the decoder itself
		json.Unmarshal(body["mode"], &mode) != nil || mode != nil && *mode != "observe" && *mode != "allowlist" {
		writeErr(w, http.StatusBadRequest, "invalid_mode", "the body is {mode}: observe, allowlist, or null to follow the org's", nil)
		return
	}
	widens := mode == nil || *mode == "observe"
	if widens && a.refuseWidening(w, r) {
		return
	}
	if mode != nil && *mode == "observe" && roleOf(ctx) != "admin" {
		writeErr(w, http.StatusConflict, "looser_than_org", "only an admin can set an app's mode looser than the org's", nil)
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: egress mode of %s %s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	floor, err := a.egressFloor(ctx, tx, id)
	if err != nil {
		internal(err)
		return
	}
	if floor == nil || *floor != "enforced" {
		writeErr(w, http.StatusConflict, "floor_not_enforced", "this app's calls do not all leave through Canyon's egress gateway yet", nil)
		return
	}
	org, err := runByOrg(ctx, tx)
	if err != nil {
		internal(err)
		return
	}
	if org {
		writeErr(w, http.StatusConflict, "egress_external", "this Canyon hands its apps to your own platform, so their calls don't go through Canyon", nil)
		return
	}
	if widens {
		held, err := a.heldProd(ctx, w, tx, id, env)
		if err != nil {
			internal(err)
			return
		}
		if held {
			return
		}
	}
	var before *string
	if err := tx.QueryRow(ctx, "SELECT (SELECT mode FROM egress_modes WHERE project_id = $1 AND env = $2)", id, env).Scan(&before); err != nil {
		internal(err)
		return
	}
	if !equalMode(before, mode) {
		if mode == nil {
			_, err = tx.Exec(ctx, "DELETE FROM egress_modes WHERE project_id = $1 AND env = $2", id, env)
		} else {
			_, err = tx.Exec(ctx, "INSERT INTO egress_modes (project_id, env, mode) VALUES ($1, $2, $3) ON CONFLICT (project_id, env) DO UPDATE SET mode = EXCLUDED.mode", id, env, *mode)
		}
		if err == nil {
			err = events.AuditTx(ctx, tx, actorOf(ctx), "egress.mode.set", id, "", map[string]any{"env": env, "app_mode": map[string]*string{"before": before, "after": mode}})
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		internal(err)
		return
	}
	a.egressTaken(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func equalMode(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// allowEgress puts one entry on an app env's allowlist, a widening, and closes each request of the env it
// allows: a person's, from a browser, and never a platform destination, which Canyon allows every app already.
// One egress.allow row when the entry was not there.
func (a *app) allowEgress(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	if a.refuseWidening(w, r) {
		return
	}
	entry, err := destination.Entry(r.PathValue("destination"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_destination", invalidDestination, nil)
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: allow %s on %s %s: %v", entry, id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	platform, err := a.platformDestinations(ctx, tx)
	if err != nil {
		internal(err)
		return
	}
	if slices.Contains(platform, entry) {
		writeErr(w, http.StatusConflict, "platform_destination", "Canyon allows "+entry+" for every app already", nil)
		return
	}
	held, err := a.heldProd(ctx, w, tx, id, env)
	if err != nil {
		internal(err)
		return
	}
	if held {
		return
	}
	refused, err := closeRequests(ctx, tx, id, env, entry)
	if err != nil {
		internal(err)
		return
	}
	by := actorOf(ctx) // a person's: refuseWidening answered every other actor
	tag, err := tx.Exec(ctx, `INSERT INTO egress_allowed (project_id, env, entry, allowed_by_kind, allowed_by_subject, allowed_by_display, allowed_at, last_refused_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7) ON CONFLICT DO NOTHING`, id, env, entry, by.Kind, by.Subject, by.Display, refused)
	if err == nil && tag.RowsAffected() == 1 {
		err = events.AuditTx(ctx, tx, by, "egress.allow", id, "", map[string]string{"env": env, "destination": entry})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		internal(err)
		return
	}
	a.egressTaken(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// invalidDestination is invalid_destination's message: what canonicalisation refuses (design/EGRESS.md §3.3).
const invalidDestination = "a destination is a host name, or *. and one: never an IP address, a single label, a port or userinfo"

// closeRequests closes each egress request of the app env that entry allows, pending or dismissed: gone from
// requests[], so a refusal after the entry is removed files a new one. It answers the latest refusal of those it
// closed, nil when it closed none, which the entry records (#2722).
func closeRequests(ctx context.Context, tx pgx.Tx, id, env, entry string) (*time.Time, error) {
	rows, err := tx.Query(ctx, "SELECT destination FROM egress_requests WHERE project_id = $1 AND env = $2", id, env)
	if err != nil {
		return nil, err
	}
	filed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	filed = slices.DeleteFunc(filed, func(d string) bool { return !destination.Allows(entry, d) })
	var last *time.Time
	err = tx.QueryRow(ctx, `WITH closed AS (DELETE FROM egress_requests WHERE project_id = $1 AND env = $2 AND destination = ANY($3) RETURNING last_refused_at)
		SELECT max(last_refused_at) FROM closed`, id, env, filed).Scan(&last)
	return last, err
}

// disallowEgress removes one entry from an app env's allowlist, a narrowing, from any session and any actor, a
// held production's included: the entry stored as :destination is written, else the one it canonicalises to,
// so an entry stored before a rule changed is still removed. One egress.disallow row when it was there.
func (a *app) disallowEgress(w http.ResponseWriter, r *http.Request) {
	ctx, id, env, raw := r.Context(), r.PathValue("id"), r.PathValue("env"), r.PathValue("destination")
	internal := func(err error) {
		a.log.Printf("canyon-server: disallow on %s %s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	remove := func(entry string) ([]string, error) {
		rows, err := tx.Query(ctx, "DELETE FROM egress_allowed WHERE project_id = $1 AND env = $2 AND entry = $3 RETURNING entry", id, env, entry)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	removed, err := remove(raw)
	if err == nil && len(removed) == 0 {
		entry, invalid := destination.Entry(raw)
		if invalid != nil {
			writeErr(w, http.StatusBadRequest, "invalid_destination", invalidDestination, nil)
			return
		}
		removed, err = remove(entry)
	}
	if err == nil && len(removed) == 1 {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "egress.disallow", id, "", map[string]string{"env": env, "destination": removed[0]})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		internal(err)
		return
	}
	a.egressTaken(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// dismissEgressRequest dismisses an app env's pending egress request, a person's from any session: the
// destination files no new one, and its refusals still count on it. One egress.request.dismiss row.
func (a *app) dismissEgressRequest(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	noRequest := func() {
		writeErr(w, http.StatusNotFound, "not_found", env+" holds no pending request of that destination", nil)
	}
	entry, err := destination.Entry(r.PathValue("destination"))
	if err != nil {
		noRequest()
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: dismiss %s on %s %s: %v", entry, id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "UPDATE egress_requests SET state = 'dismissed' WHERE project_id = $1 AND env = $2 AND destination = $3 AND state = 'pending'", id, env, entry)
	if err == nil && tag.RowsAffected() == 0 {
		noRequest()
		return
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "egress.request.dismiss", id, "", map[string]string{"env": env, "destination": entry})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		internal(err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// egressRestrict fills each env of the app's read with layer 2's: its mode — its own, else the org's — its own,
// its allowlist, sorted, without the platform destinations, and its egress requests, pending first, then
// dismissed, each the latest refused first.
func egressRestrict(ctx context.Context, q querier, id string, platform []string, envs map[string]*egressEnv) error {
	var org string
	if err := q.QueryRow(ctx, "SELECT mode FROM egress_settings").Scan(&org); err != nil {
		return err
	}
	for _, e := range envs {
		e.Mode, e.Allowed, e.Allows, e.Requests = org, []string{}, []egressAllow{}, []egressRequest{}
	}
	rows, err := q.Query(ctx, "SELECT env, mode FROM egress_modes WHERE project_id = $1", id)
	if err != nil {
		return err
	}
	var env, mode string
	if _, err := pgx.ForEachRow(rows, []any{&env, &mode}, func() error {
		m := mode
		envs[env].Mode, envs[env].AppMode = m, &m
		return nil
	}); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT env, entry, allowed_by_kind, allowed_by_subject, allowed_by_display, allowed_at, last_refused_at
		FROM egress_allowed WHERE project_id = $1 AND entry <> ALL($2) ORDER BY env, entry COLLATE "C"`, id, platform)
	if err != nil {
		return err
	}
	var entry string
	var kind, subject, display *string
	var at, refused *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&env, &entry, &kind, &subject, &display, &at, &refused}, func() error {
		envs[env].Allowed = append(envs[env].Allowed, entry)
		if at == nil { // allowed before #2722: nothing recorded who allowed it
			return nil
		}
		allow := egressAllow{Entry: entry, By: actor{Kind: *kind, Subject: *subject, Display: *display}, At: at.UTC().Format(time.RFC3339)}
		if refused != nil {
			last := refused.UTC().Format(time.RFC3339)
			allow.LastRefusedAt = &last
		}
		envs[env].Allows = append(envs[env].Allows, allow)
		return nil
	}); err != nil {
		return err
	}
	now, err := refusingNow(ctx, q, []string{id}, platform)
	if err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT env, destination, state, first_refused_at, last_refused_at, refusals FROM egress_requests WHERE project_id = $1
		ORDER BY state <> 'pending', date_trunc('second', last_refused_at) DESC, destination`, id)
	if err != nil {
		return err
	}
	var req egressRequest
	var first, last time.Time
	_, err = pgx.ForEachRow(rows, []any{&env, &req.Destination, &req.State, &first, &last, &req.Refusals}, func() error {
		req.FirstRefusedAt, req.LastRefusedAt = first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339)
		_, req.Refusing = now[requestKey{id, env, req.Destination}]
		envs[env].Requests = append(envs[env].Requests, req)
		return nil
	})
	return err
}

// platformDestinations are the hosts every front door in use signs people in at (design/EGRESS.md §3.3;
// doorsInUse), allowed for every caller of every env and on no team's allowlist: each door's signinURLs' hosts,
// the hosts a choice 2's issuer's discovery names as recorded, and an alb's region's key endpoint — each as
// destination.Entry canonicalises it, sorted, each once. None on Docker, which has no front door.
// A host Entry refuses — an IP literal, a single label like an in-cluster keycloak — is none: the check refuses
// a call to it as invalid_destination before it reads the platform destinations, so naming it would allow
// nothing, and an in-cluster provider goes direct, never through Envoy (CANYON_KUBE_SIGNIN_DIRECT, #2321). A
// port is no part of a name: the check refuses one other than 80 and 443 first.
func (a *app) platformDestinations(ctx context.Context, q querier) ([]string, error) {
	out := []string{}
	if !a.kube {
		return out, nil
	}
	rows, err := q.Query(ctx, doorsInUse+` SELECT d.door, f.hosts FROM doors d
		LEFT JOIN front_door_discovery f ON d.door->>'choice' = '2' AND f.issuer = d.door->'signin'->>'issuer'`)
	if err != nil {
		return out, err
	}
	var hosts []string
	var raw []byte
	var discovered []string
	if _, err := pgx.ForEachRow(rows, []any{&raw, &discovered}, func() error {
		var door hostDoor // each row its own: a door's fields never carry into the next
		if err := json.Unmarshal(raw, &door); err != nil {
			return err
		}
		for _, s := range signinURLs(door) {
			if u, err := neturl.Parse(s); s != "" && err == nil {
				hosts = append(hosts, u.Hostname())
			}
		}
		if arn := strings.Split(door.ALBARN, ":"); door.Gateway == "alb" && len(arn) > 3 { // arn:aws:elasticloadbalancing:<region>:…
			hosts = append(hosts, "public-keys.auth.elb."+arn[3]+".amazonaws.com")
		}
		hosts = append(hosts, discovered...)
		return nil
	}); err != nil {
		return out, err
	}
	for _, h := range hosts {
		if n, err := destination.Entry(h); err == nil && !strings.HasPrefix(n, "*.") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
