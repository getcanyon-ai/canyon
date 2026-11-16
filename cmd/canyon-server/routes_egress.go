package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// Outbound (design/EGRESS.md §8.3, design/PERMISSIONS.md Outbound): the host's egress setting, which every
// member reads and only an admin changes, from a browser; and an app's calls out, which its contributors,
// owners and admins read; and an admin's Recreate networks and rotation of the interception root, from a browser.
// Layer 2 (§3.3–3.5): an app env's
// mode, its allowlist and its egress requests' Dismiss.
func routesEgress(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /egress", a.getEgress)
	api.HandleFunc("PUT /egress", a.adminOnly(personOnly(outboundPerson, a.putEgress)))
	api.HandleFunc("POST /egress/networks/recreate", a.adminOnly(personOnly(outboundPerson, a.recreateNetworks)))
	api.HandleFunc("POST /egress/ca/rotate", a.adminOnly(personOnly(outboundPerson, a.rotateCA)))
	api.HandleFunc("GET /egress/destinations", a.adminOnly(a.getEgressDestinations))
	api.HandleFunc("GET /projects/{id}/egress", a.teamOnly(a.getProjectEgress))
	api.HandleFunc("PUT /projects/{id}/egress/{env}/mode", a.outboundTeamOnly(a.putEgressMode))
	api.HandleFunc("PUT /projects/{id}/egress/{env}/destinations/{destination}", a.outboundTeamOnly(a.allowEgress))
	api.HandleFunc("DELETE /projects/{id}/egress/{env}/destinations/{destination}", a.outboundTeamOnly(a.disallowEgress))
	api.HandleFunc("POST /projects/{id}/egress/{env}/requests/{destination}/dismiss", a.outboundTeamOnly(personOnly(outboundPerson, a.dismissEgressRequest)))
	api.HandleFunc("PUT /projects/{id}/egress/{env}/routes/{destination}", a.outboundTeamOnly(a.putRoute))
	api.HandleFunc("DELETE /projects/{id}/egress/{env}/routes/{destination}", a.outboundTeamOnly(a.deleteRoute))
	api.HandleFunc("POST /projects/{id}/egress/{env}/routes/{destination}/from-secret", a.outboundTeamOnly(a.routeFromSecret))
}

// egressNoProxy is PUT canyon's 409 on a host whose server names no Envoy for a /run to carry, Docker or
// Kubernetes — fail closed — and Recreate networks' under canyon there; egressNoNetwork is Recreate networks'
// on Kubernetes, which writes an app's NetworkPolicy at every /run; egressGatewaySignin is PUT canyon's 409 on
// Kubernetes while the front door signs in at a sign-in served through the cluster's own gateway, D5's (contracts/routes.md,
// Egress control; design/EXPERIENCE.md).
const (
	egressNoProxy       = "Canyon can't send apps' calls out through its egress gateway: this server has no CANYON_EGRESS_PROXY. Leave this off until it does."
	egressNoNetwork     = "an app's NetworkPolicy is written at every deploy: there is no network to recreate"
	egressGatewaySignin = "Apps routed through Canyon can't reach a sign-in served through this cluster's own gateway, so nobody could sign in to them. Set the front door to another sign-in provider, or leave this off."
)

// canyonRefused is the message of PUT canyon's 409 on this host, "" where canyon is taken. On Kubernetes no probe
// is asked here: every /run there needs a front door that passed the probe (design/EGRESS.md §2.2; #2438);
// a front door signing in through the cluster's own gateway refuses it, as a floored app's sidecars could not
// reach that provider (#2316, the owner's decision). PUT reads q under the lock a front-door save takes.
func (a *app) canyonRefused(ctx context.Context, q querier) (string, error) {
	if a.pipe.EgressProxy == "" { // a deploy would fail closed: no Envoy to name in its /run
		return egressNoProxy, nil
	}
	if !a.kube || a.behind == "" {
		return "", nil
	}
	d, err := readDoor(ctx, q)
	if err != nil || d == nil || !signsInAt(d.hostDoor, a.behind) {
		return "", err
	}
	return egressGatewaySignin, nil
}

// gatewaySigninOf is CANYON_KUBE_GATEWAY_SIGNIN's host, lower-cased: the issuer, an https URL, of a sign-in
// served through the cluster's own gateway — the Helm chart's Pocket ID behind Caddy on k3s, or an operator's
// own — whose address kube-proxy rewrites to a pod address a floored app can't reach (contracts/routes.md,
// Egress control). "" unset; anything else refuses the start.
func gatewaySigninOf(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	u, err := neturl.Parse(v)
	if err != nil || !validHTTPS(v) || u.Hostname() == "" {
		return "", errors.New("CANYON_KUBE_GATEWAY_SIGNIN must be the issuer of a sign-in served through the cluster's own gateway, an https URL")
	}
	return strings.ToLower(u.Hostname()), nil
}

func (a *app) getEgress(w http.ResponseWriter, r *http.Request) {
	out, err := a.egressRead(r)
	if err != nil {
		a.log.Printf("canyon-server: egress setting: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) egressRead(r *http.Request) (map[string]any, error) {
	var via, mode string
	var routes bool
	if err := a.pool.QueryRow(r.Context(), "SELECT via, mode, credential_routes FROM egress_settings").Scan(&via, &mode, &routes); err != nil {
		return nil, err
	}
	out := map[string]any{"via": via, "mode": mode, "credential_routes": routes}
	if roleOf(r.Context()) == "admin" {
		refused, err := a.canyonRefused(r.Context(), a.pool)
		if err != nil {
			return nil, err
		}
		out["proxy"], out["receiver"], out["streams"], out["last_entry_at"], out["canyon_refused"] = nullable(a.pipe.EgressProxy != "", a.pipe.EgressProxy), a.egress != nil, 0, nil, nullable(refused != "", refused)
		var c egressCaps
		if err := a.pool.QueryRow(r.Context(), "SELECT cap_destinations, cap_new_names, cap_pending FROM egress_settings").Scan(&c.Destinations, &c.NewNames, &c.Pending); err != nil {
			return nil, err
		}
		out["caps"] = c
		if a.egress != nil {
			out["streams"] = a.egress.streams()
			if at := a.egress.lastEntry(); !at.IsZero() {
				out["last_entry_at"] = at.Format(time.RFC3339)
			}
		}
		if out["replicas"], err = egressReplicas(r.Context(), a.pool); err != nil {
			return nil, err
		}
		if out["ca"], err = a.egressCA(r.Context()); err != nil { // the key dates, never a key
			return nil, err
		}
		// the apps whose network is not what via makes it (design/EGRESS.md §2.2), and each one's network now, internal,
		// open, or unknown (a recreation under way or one that failed), so Settings reads them in this one call (#2767);
		// none on Kubernetes, which writes an app's NetworkPolicy at every /run
		if a.kube {
			out["networks_to_recreate"], out["networks"] = nil, nil
			return out, nil
		}
		rows, err := a.pool.Query(r.Context(), `SELECT p.id, coalesce((SELECT CASE WHEN n.internal IS NULL THEN 'unknown' WHEN n.internal THEN 'internal' ELSE 'open' END
				FROM app_networks n WHERE n.project_id = p.id), 'open')
			FROM projects p WHERE p.deleted_at IS NULL AND EXISTS (SELECT 1 FROM deploys WHERE project_id = p.id AND run_by = 'canyon')
			AND (`+runs.NetworkUnknown("p.id")+` OR `+runs.NetworkInternal("p.id")+` <> ($1 = 'canyon')) ORDER BY p.id`, via)
		if err != nil {
			return nil, err
		}
		apps, networks := []string{}, map[string]string{}
		var app, network string
		if _, err := pgx.ForEachRow(rows, []any{&app, &network}, func() error {
			apps, networks[app] = append(apps, app), network
			return nil
		}); err != nil {
			return nil, err
		}
		out["networks_to_recreate"], out["networks"] = apps, networks
	}
	return out, nil
}

// egressReplica is one egress-control replica as an admin reads it: the version it last said it holds, null when
// none or out of shape, when it last asked, and since when that policy has been out of date — the last time it
// was the current one, or was served while it was; its policy's age is now minus current_at.
type egressReplica struct {
	Replica   string     `json:"replica"`
	Have      *string    `json:"have"`
	SeenAt    time.Time  `json:"seen_at"`
	CurrentAt *time.Time `json:"current_at"`
}

// egressReplicas is every replica that asked for the policy in the last day, by name.
func egressReplicas(ctx context.Context, q querier) ([]egressReplica, error) {
	rows, err := q.Query(ctx, `SELECT replica, CASE WHEN have_seq IS NOT NULL THEN have_epoch || ':' || have_seq END, date_trunc('second', seen_at), date_trunc('second', current_at)
		FROM egress_replicas WHERE seen_at > now() - interval '1 day' ORDER BY replica COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[egressReplica])
	for i := range out {
		out[i].SeenAt = out[i].SeenAt.UTC()
		if out[i].CurrentAt != nil {
			*out[i].CurrentAt = out[i].CurrentAt.UTC()
		}
	}
	return append([]egressReplica{}, out...), err
}

// putEgress sets via, the org's default mode or both, an admin's from a browser: a level refusal first, then
// the actor's (personOnly), then the session's, then the body. A mode is refused while via — the body's when it
// names one — is off, no app's calls then leaving through Envoy, and then while the org runs its apps
// (design/PERMISSIONS.md, Outbound, Order). canyon is refused on a host that names no Envoy; taken on Docker,
// the answer lists the networks an admin's Recreate networks moves onto the floor (#2254). via to off is refused
// while any app env holds a value route, which works only through Canyon. A widening — the
// mode to observe, via to off — is refused while a production it widens is held. credential_routes, whether Canyon
// adds a route's credential to apps' calls, turns either way with no refusal of its own (#2143). One audit row,
// egress.set, naming what moved; a PUT that changes nothing writes none.
func (a *app) putEgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.auth == authOIDC && !fromDashboard(ctx) {
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", "")
		return
	}
	var body map[string]json.RawMessage
	var via, mode *string
	var routes *bool
	var caps map[string]json.RawMessage
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	bad := dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || len(body) == 0 //nolint:errorlint // sentinel from the decoder itself
	for k, v := range body {
		switch k {
		case "via":
			bad = bad || json.Unmarshal(v, &via) != nil || via == nil || *via != "off" && *via != "canyon"
		case "mode", "caps":
		case "credential_routes":
			bad = bad || json.Unmarshal(v, &routes) != nil || routes == nil
		default:
			bad = true
		}
	}
	if bad {
		writeErr(w, http.StatusBadRequest, "invalid_via", "the body is any of via, mode, caps and credential_routes, and nothing else; via is off or canyon, credential_routes true or false", nil)
		return
	}
	if raw, ok := body["mode"]; ok && (json.Unmarshal(raw, &mode) != nil || mode == nil || *mode != "observe" && *mode != "allowlist") {
		writeErr(w, http.StatusBadRequest, "invalid_mode", "mode is observe or allowlist", nil)
		return
	}
	if raw, ok := body["caps"]; ok && !capsIn(raw, &caps) {
		writeErr(w, http.StatusBadRequest, "invalid_caps", egressCapsLine, nil)
		return
	}
	var heldApp string
	var hold *runs.Hold
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		var before struct {
			via, mode string
			caps      egressCaps
			routes    bool
		}
		if err := tx.QueryRow(ctx, "SELECT via, mode, cap_destinations, cap_new_names, cap_pending, credential_routes FROM egress_settings FOR UPDATE").Scan(&before.via, &before.mode,
			&before.caps.Destinations, &before.caps.NewNames, &before.caps.Pending, &before.routes); err != nil {
			return err
		}
		after := before
		for k, raw := range caps { // each in range, capsIn's: a key left out keeps its value
			_ = json.Unmarshal(raw, after.caps.field(k))
		}
		if via != nil {
			after.via = *via
		}
		if routes != nil {
			after.routes = *routes
		}
		if after.routes && !before.routes { // turning credentials on is Canyon Enterprise's, which writes it (#2879, #2922); off, or as it is, never asks
			if err := a.ee.turnOn(ctx, tx); err != nil {
				return err
			}
		}
		if mode != nil {
			after.mode = *mode
			org, err := runByOrg(ctx, tx)
			switch {
			case err != nil:
				return err
			case after.via == "off":
				return &refused{http.StatusConflict, "floor_not_enforced", "apps' calls do not leave through Canyon's egress gateway while it is off"}
			case org:
				return &refused{http.StatusConflict, "egress_external", "the org runs its apps, so their calls do not leave through Canyon"}
			}
		}
		if after.via == "off" && before.via != "off" { // a route works only through Canyon
			apps, err := credentialsHeld(ctx, tx)
			if err != nil {
				return err
			}
			if len(apps) > 0 {
				return &credentials{apps}
			}
		}
		if via != nil && *via == "canyon" { // under the lock a front-door save takes: a door saved meanwhile is seen
			msg, err := a.canyonRefused(ctx, tx)
			if err != nil {
				return err
			}
			if msg != "" {
				return &refused{http.StatusConflict, "egress_not_ready", msg}
			}
		}
		if viaOff := after.via == "off" && before.via == "canyon"; viaOff || after.mode == "observe" && before.mode == "allowlist" {
			var err error
			if heldApp, hold, err = heldWidened(ctx, tx, viaOff); err != nil || hold != nil {
				return cmp.Or(err, runs.ErrHeld)
			}
		}
		if after == before {
			return nil
		}
		// credential_routes is only ever turned off here: on is Canyon Enterprise's write, above
		if _, err := tx.Exec(ctx, "UPDATE egress_settings SET via = $1, mode = $2, cap_destinations = $3, cap_new_names = $4, cap_pending = $5, credential_routes = credential_routes AND $6",
			after.via, after.mode, after.caps.Destinations, after.caps.NewNames, after.caps.Pending, after.routes); err != nil {
			return err
		}
		detail := map[string]any{}
		if after.via != before.via {
			detail["via"] = map[string]string{"before": before.via, "after": after.via}
		}
		if after.mode != before.mode {
			detail["mode"] = map[string]string{"before": before.mode, "after": after.mode}
		}
		if after.caps != before.caps {
			detail["caps"] = map[string]egressCaps{"before": before.caps, "after": after.caps}
		}
		if after.routes != before.routes {
			detail["credential_routes"] = map[string]bool{"before": before.routes, "after": after.routes}
		}
		return events.AuditTx(ctx, tx, actorOf(ctx), "egress.set", "", "", detail)
	})
	var no *refused
	var held *credentials
	var lic licenceErr
	switch {
	case errors.As(err, &no):
		writeErr(w, no.status, no.code, no.message, nil)
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
	case errors.As(err, &held):
		writeErr(w, http.StatusConflict, "credentials_exist", credentialsExist(held.apps), map[string]any{"apps": held.apps})
	case errors.Is(err, runs.ErrHeld):
		o, err := a.orgOf(ctx, a.pool)
		if err != nil {
			a.log.Printf("canyon-server: the admins: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		writeErr(w, http.StatusConflict, "held", heldApp+": "+hold.Line(), map[string]any{"hold": heldOf(*hold), "admins": o.admins})
	case err != nil:
		a.log.Printf("canyon-server: egress setting: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		a.egressTaken(w, r)
		a.getEgress(w, r)
	}
}

// egressWait is how long egressTaken waits: runs.EgressConverge, shorter only in a test that waits it out.
var egressWait = egressDefault

const egressDefault = runs.EgressConverge

// egressTaken waits, after a change to the egress policy commits and before it answers, until every live
// egress-control replica holds the policy as it is now, at most egressWait, as a deploy's converge does
// before its /run: a call the change refuses is refused from the answer on, by every replica (#2789). Past the
// wait the change stands, and the answer names the first replica behind in Canyon-Egress-Pending, or "unread"
// when the wait could not be read: never claiming every replica holds what it could not see.
func (a *app) egressTaken(w http.ResponseWriter, r *http.Request) {
	var seq int64
	behind := "unread"
	err := a.pool.QueryRow(r.Context(), "SELECT seq FROM egress_policy").Scan(&seq)
	if err == nil {
		behind, err = runs.EgressBehind(r.Context(), a.pool, seq, egressWait)
	}
	if err != nil {
		if r.Context().Err() == nil { // a client that hung up mid-wait is no error of the server's
			a.log.Printf("canyon-server: the egress gateway's replicas taking a change: %v", err)
		}
		behind = "unread"
	}
	if behind != "" {
		w.Header().Set("Canyon-Egress-Pending", behind)
	}
}

// heldWidened is the first app, by id, whose held production an org change widens, and its hold: one that
// follows the org's mode, or, when via moves to off, any (design/PERMISSIONS.md, Outbound, A held production).
// No hold is "", nil.
func heldWidened(ctx context.Context, tx pgx.Tx, viaOff bool) (string, *runs.Hold, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT coalesce(min(h.project_id), '') FROM prod_holds h JOIN projects p ON p.id = h.project_id WHERE p.deleted_at IS NULL
		AND ($1 OR NOT EXISTS (SELECT 1 FROM egress_modes m WHERE m.project_id = h.project_id AND m.env = 'prod'))`, viaOff).Scan(&id)
	if err != nil || id == "" {
		return "", nil, err
	}
	h, err := runs.ReadHold(ctx, tx, id)
	return id, h, err
}

// recreateWait bounds the executor's recreation: its docker calls are bounded at two minutes under its own
// lock, and the stop before them at ten seconds a container.
const recreateWait = 3 * time.Minute

// recreateNetworks is an admin's Recreate networks for one app, from a browser (design/EGRESS.md §2.2,
// the owner's decision, #2254): the level, the actor and the session refused first, then the body, then the host
// and the app. One runs at a time on the host, a try, never a wait (409 busy); then, under the app's undeploy
// lock — no run of it starts meanwhile, and a hold or an undeploy of it waits until this ends — an app being
// deleted, a held production and a run in flight are refused before anything is stopped, and the host's via
// is read again. The executor stops the app's containers and makes its network what that via makes it —
// internal under canyon, open under off — recorded unknown before the call and as it is once the executor
// answers, each in a transaction of its own; then each env Canyon runs, prod first, its outage the shorter,
// starts again from its deploy row under that via unless the app was marked deleting meanwhile, and the row
// records it. Fail closed: a failure after the stop leaves the app stopped, 502 recreate_failed naming
// what is, and the same call again repeats the whole of it. Every attempt that reached the executor is one
// egress.networks.recreate row, saying what failed.
func (a *app) recreateNetworks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.auth == authOIDC && !fromDashboard(ctx) {
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", "")
		return
	}
	var body struct {
		App string `json:"app"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() || !runs.ValidProject.MatchString(body.App) {
		writeErr(w, http.StatusBadRequest, "invalid_app", "the body is {app}, an app's id", nil)
		return
	}
	id := body.App
	internal := func(err error) {
		a.log.Printf("canyon-server: recreate networks of %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	var via string
	notReady := func(q querier) bool { // true means the response is written
		if err := q.QueryRow(ctx, "SELECT via FROM egress_settings").Scan(&via); err != nil {
			internal(err)
			return true
		}
		switch {
		case a.kube:
			writeErr(w, http.StatusConflict, "egress_not_ready", egressNoNetwork, nil)
		case via == "canyon" && a.pipe.EgressProxy == "": // the /runs after it would name no Envoy
			writeErr(w, http.StatusConflict, "egress_not_ready", egressNoProxy, nil)
		default:
			return false
		}
		return true
	}
	if notReady(a.pool) || !a.projectLive(ctx, w, id) {
		return
	}
	// the locks and the connection go back before the answer is written: its read takes a connection of the pool's
	if a.recreateLocked(ctx, w, id, internal, notReady, &via) {
		a.getEgress(w, r)
	}
}

// recreateLocked is recreateNetworks under its locks, on a connection of its own: true when it succeeded and the
// admin's read answers, written by the caller once the locks and the connection are given back; false when
// it wrote its answer.
func (a *app) recreateLocked(ctx context.Context, w http.ResponseWriter, id string, internal func(error), notReady func(querier) bool, viaOf *string) bool {
	conn, err := a.pool.Acquire(ctx) // holds the locks, and makes this route's reads and writes; RunApp's own reads take the pool's
	if err != nil {
		internal(err)
		return false
	}
	defer conn.Release()
	var free bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext(current_schema() || ':recreate-networks'))").Scan(&free); err != nil {
		internal(err)
		return false
	}
	if !free {
		writeErr(w, http.StatusConflict, "busy", "another app's network is being recreated: recreate this one when it is done", nil)
		return false
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext(current_schema() || ':recreate-networks'))"); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx)) // a lock not given back goes with its session
		}
	}()
	unlock, err := runs.LockUndeploy(ctx, conn.Conn(), id)
	if err != nil {
		if !undeployBusy(w, err) {
			internal(err)
		}
		return false
	}
	defer unlock()
	var deleting bool
	if err := conn.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1", id).Scan(&deleting); err != nil {
		internal(err)
		return false
	}
	if deleting {
		writeErr(w, http.StatusConflict, "deleting", "this project is being deleted", nil)
		return false
	}
	h, err := runs.ReadHold(ctx, conn, id) // nothing runs in a held production (contracts/routes.md, The hold): never started again here
	if err != nil {
		internal(err)
		return false
	}
	if h != nil {
		a.refuseHeld(ctx, w, conn, *h)
		return false
	}
	var patch string // a person's request goes first: a patch run before its /run is ended (contracts/routes.md, Patch runs)
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) (err error) { patch, err = runs.EndPatchTx(ctx, tx, id); return err }); err != nil {
		internal(err)
		return false
	}
	runs.Cancel(a.durable, patch)
	if a.runInProgress(ctx, w, conn, id) || notReady(conn) {
		return false
	}
	ctx = context.WithoutCancel(ctx) // once the containers stop, the recreation finishes, whether or not the caller waits
	// prod, then preview: production's outage the shorter
	rows, err := conn.Query(ctx, "SELECT env, image, coalesce(gate_run, '') FROM deploys WHERE project_id = $1 AND run_by = 'canyon' ORDER BY env DESC", id)
	if err != nil {
		internal(err)
		return false
	}
	deployed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Env, Image, Run string }])
	if err != nil {
		internal(err)
		return false
	}
	if len(deployed) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "Canyon runs nothing of "+id, nil)
		return false
	}
	isInternal, spec := *viaOf == "canyon", runs.Egress{Via: *viaOf} // via as read again under the lock
	started, stopped := []string{}, []string{}
	audited := func(tx pgx.Tx, failed any) error {
		return events.AuditTx(ctx, tx, actorOf(ctx), "egress.networks.recreate", id, "", map[string]any{"internal": isInternal, "started": started, "stopped": stopped, "failed": failed})
	}
	audit := func(failed string) { // the row of an attempt that failed, on its own
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if err := events.LockTx(ctx, tx); err != nil {
				return err
			}
			return audited(tx, failed)
		}); err != nil {
			a.log.Printf("canyon-server: recreate networks of %s: its audit row: %v", id, err)
		}
	}
	// unknown while the executor may change it, and after a call it failed: never read internal meanwhile
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return runs.SetNetworkTx(ctx, tx, id, nil) }); err != nil {
		internal(err)
		return false
	}
	call, cancel := context.WithTimeout(ctx, recreateWait)
	err = a.pipe.Executor.RecreateNetwork(call, id, isInternal)
	cancel()
	if err != nil { // what it stopped is unknown: its failure says nothing of it, and the audit row claims none
		a.log.Printf("canyon-server: recreate networks of %s: %v", id, err)
		audit("network")
		writeErr(w, http.StatusBadGateway, "recreate_failed", id+"'s network wasn't recreated, so the environments Canyon stopped for it stay stopped. Recreate it again.", nil)
		return false
	}
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return runs.SetNetworkTx(ctx, tx, id, &isInternal) }); err != nil { // the network as it is, before any env starts on it
		for _, d := range deployed {
			stopped = append(stopped, d.Env)
		}
		audit("record")
		internal(err)
		return false
	}
	var gone, broke []string             // the envs that didn't start again: their image gone, or anything else (logged, never answered)
	carried := map[string]runs.Carried{} // what each env's /run carried, which its row records
	for _, d := range deployed {
		var deleting bool // a delete marks the app without the undeploy lock, then waits for it before its stop: no env starts after the mark
		err := conn.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1", id).Scan(&deleting)
		if err == nil && deleting {
			err = errDeleting
		}
		var config runs.Carried
		if err == nil {
			_, _, config, err = a.pipe.RunApp(ctx, d.Run, id, d.Env, d.Image, spec, "")
		}
		if err != nil {
			a.log.Printf("canyon-server: recreate networks of %s: %s did not start again: %v", id, d.Env, err)
			stopped = append(stopped, d.Env)
			if errors.Is(err, executor.ErrNoImage) {
				gone = append(gone, d.Env)
			} else {
				broke = append(broke, d.Env)
			}
			continue
		}
		started, carried[d.Env] = append(started, d.Env), config
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { // what runs now, and who made it so
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
		for _, env := range started {
			if _, err := tx.Exec(ctx, "UPDATE deploys SET egress = $3, run_config = $4, run_config_sha256 = $5, run_config_at = now(), egress_roots = coalesce($6::text[], egress_roots) WHERE project_id = $1 AND env = $2 AND run_by = 'canyon'",
				id, env, spec, carried[env].Config, carried[env].Fingerprint, carried[env].Settled()); err != nil {
				return err
			}
		}
		if len(stopped) > 0 {
			return audited(tx, "start")
		}
		return audited(tx, nil)
	})
	if err != nil {
		audit("record")
		internal(err)
		return false
	}
	if len(stopped) > 0 {
		writeErr(w, http.StatusBadGateway, "recreate_failed", recreateStopped(id, gone, broke), nil)
		return false
	}
	return true
}

// recreateStopped is a recreation's answer when an env didn't start again on the new network, in a person's words
// (design/EXPERIENCE.md D5): never the executor's, which the server log keeps. An env whose image is gone needs a
// new deploy, which another recreation never gives it.
func recreateStopped(id string, gone, broke []string) string {
	named := func(envs []string) string {
		words := make([]string, len(envs))
		for i, env := range envs {
			words[i] = map[string]string{"preview": "preview", "prod": "production"}[env]
		}
		said := strings.Join(words, " and ")
		return strings.ToUpper(said[:1]) + said[1:]
	}
	said := id + "'s network is recreated."
	if len(broke) == 1 {
		said += " " + named(broke) + " didn't start again and stays stopped: recreate it again."
	} else if len(broke) > 1 {
		said += " " + named(broke) + " didn't start again and stay stopped: recreate it again."
	}
	if len(gone) == 1 {
		said += " " + named(gone) + " can't start again: the image it ran is no longer on this Canyon, so " + id + " needs a new deploy."
	} else if len(gone) > 1 {
		said += " " + named(gone) + " can't start again: the images they ran are no longer on this Canyon, so " + id + " needs a new deploy."
	}
	return said
}

// egressDestination is one destination an app env called: its row without the app and env.
type egressDestination struct {
	Destination       string   `json:"destination"`
	Port              int      `json:"port"`
	FirstSeenAt       string   `json:"first_seen_at"`
	LastSeenAt        string   `json:"last_seen_at"`
	Calls             int64    `json:"calls"`
	LastOutcome       string   `json:"last_outcome"`
	LastStatusClass   string   `json:"last_status_class"`
	LastProtocol      string   `json:"last_protocol"`
	LastResponseFlags []string `json:"last_response_flags"`
	// the latest call refused while adding a route's credential, nil for none (contracts/routes.md, Interception, A record)
	LastCredentialRefusal *credentialRefusalRead `json:"last_credential_refusal"`
}

// credentialRefusalRead is a row's latest credential refusal: its reason and when the receiver took its entry.
type credentialRefusalRead struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// refusalOf is the read of a row's two credential refusal columns, nil while they hold none.
func refusalOf(reason *string, at *time.Time) *credentialRefusalRead {
	if reason == nil || at == nil {
		return nil
	}
	return &credentialRefusalRead{Reason: *reason, At: at.UTC().Format(time.RFC3339)}
}

// egressFleetRow is one row of the fleet's read: an app's destination, its app and env beside it.
type egressFleetRow struct {
	App string `json:"app"`
	Env string `json:"env"`
	egressDestination
}

// egressEnv is one env of an app's read: the spec its running version was deployed under, its destinations,
// and layer 2's mode — its own, else the org's — its own mode, its allowlist, who allowed each entry and when, and
// its egress requests.
type egressEnv struct {
	Via          *string              `json:"via"`
	Destinations []*egressDestination `json:"destinations"`
	Mode         string               `json:"mode"`
	AppMode      *string              `json:"app_mode"`
	Allowed      []string             `json:"allowed"`
	Allows       []egressAllow        `json:"allows"`
	Requests     []egressRequest      `json:"requests"`
	Routes       []routeRead          `json:"routes"`
}

// egressAllow is who put one allowlist entry there and when, and the latest refusal of the requests it closed, nil
// when it closed none (#2722): an entry allowed before it records none.
type egressAllow struct {
	Entry         string  `json:"entry"`
	By            actor   `json:"by"`
	At            string  `json:"at"`
	LastRefusedAt *string `json:"last_refused_at"`
}

// egressRequest is one egress request of an app env (design/EGRESS.md §3.4), and whether the env refuses it now.
type egressRequest struct {
	Destination    string `json:"destination"`
	State          string `json:"state"`
	FirstRefusedAt string `json:"first_refused_at"`
	LastRefusedAt  string `json:"last_refused_at"`
	Refusals       int64  `json:"refusals"`
	Refusing       bool   `json:"refusing"`
}

type egressGapOut struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Reason string  `json:"reason"`
	Calls  *int64  `json:"calls"`
	App    *string `json:"app"`
	Env    *string `json:"env"`
}

// getProjectEgress is what an app called out to in the last 30 days, the rows' keep, per env: the egress
// spec its running version was deployed under (null where nothing is deployed), and each destination, the
// latest seen first. gaps are every span the rows are missing calls in that names no app — a lost call
// names no app, so any of them may be this one's — and the app's own over_cap gaps, never another app's.
// Each env also carries its mode, its allowlist and its egress requests, and the read the platform
// destinations, on no allowlist.
func (a *app) getProjectEgress(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	out, err := a.projectEgress(r, id, time.Now().UTC().Add(-egressKept).Truncate(time.Second))
	if err != nil {
		a.log.Printf("canyon-server: egress of %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) projectEgress(r *http.Request, id string, since time.Time) (map[string]any, error) {
	ctx := r.Context()
	envs := map[string]*egressEnv{"preview": {Destinations: []*egressDestination{}}, "prod": {Destinations: []*egressDestination{}}}
	rows, err := a.pool.Query(ctx, "SELECT env, coalesce(egress->>'via', 'off') FROM deploys WHERE project_id = $1", id)
	if err != nil {
		return nil, err
	}
	deployed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Env, Via string }])
	if err != nil {
		return nil, err
	}
	for _, d := range deployed {
		envs[d.Env].Via = &d.Via
	}
	floor, err := a.egressFloor(ctx, a.pool, id)
	if err != nil {
		return nil, err
	}
	platform, err := a.platformDestinations(ctx, a.pool)
	if err != nil {
		return nil, err
	}
	if err := egressRestrict(ctx, a.pool, id, platform, envs); err != nil {
		return nil, err
	}
	if err := egressRoutes(ctx, a.ee, a.pool, id, envs); err != nil {
		return nil, err
	}
	rows, err = a.pool.Query(ctx, `SELECT env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags,
		last_credential_reason, last_credential_refused_at
		FROM egress_destinations WHERE app = $1 AND last_seen_at >= $2 ORDER BY date_trunc('second', last_seen_at) DESC, destination, port`, id, since)
	if err != nil {
		return nil, err
	}
	var env string
	var first, last time.Time
	var reason *string
	var refused *time.Time
	var d egressDestination
	if _, err := pgx.ForEachRow(rows, []any{&env, &d.Destination, &d.Port, &first, &last, &d.Calls, &d.LastOutcome, &d.LastStatusClass, &d.LastProtocol, &d.LastResponseFlags, &reason, &refused}, func() error {
		d.FirstSeenAt, d.LastSeenAt = first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339)
		d.LastCredentialRefusal = refusalOf(reason, refused)
		if d.LastResponseFlags == nil {
			d.LastResponseFlags = []string{}
		}
		row := d
		envs[env].Destinations = append(envs[env].Destinations, &row)
		d.LastResponseFlags = nil // the next row's scan fills a slice of its own
		return nil
	}); err != nil {
		return nil, err
	}
	gaps, err := a.egressGaps(r, id, since)
	if err != nil {
		return nil, err
	}
	return map[string]any{"since": since.Format(time.RFC3339), "floor": floor, "envs": envs, "gaps": gaps, "platform": platform}, nil
}

// getEgressDestinations is what every app called out to in the last 30 days, the rows' keep, an admin's
// (design/EGRESS.md §7, §8.3): each row as stored, the latest seen first, then by app, env, destination and
// port, and every gap ending in the window, whichever app it names, the oldest first.
func (a *app) getEgressDestinations(w http.ResponseWriter, r *http.Request) {
	out, err := a.fleetEgress(r.Context(), time.Now().UTC().Add(-egressKept).Truncate(time.Second))
	if err != nil {
		a.log.Printf("canyon-server: the fleet's egress: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) fleetEgress(ctx context.Context, since time.Time) (map[string]any, error) {
	rows, err := a.pool.Query(ctx, `SELECT app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags,
		last_credential_reason, last_credential_refused_at
		FROM egress_destinations WHERE last_seen_at >= $1 ORDER BY date_trunc('second', last_seen_at) DESC, app, env, destination, port`, since)
	if err != nil {
		return nil, err
	}
	destinations := []egressFleetRow{}
	var first, last time.Time
	var reason *string
	var refused *time.Time
	var d egressFleetRow
	if _, err := pgx.ForEachRow(rows, []any{&d.App, &d.Env, &d.Destination, &d.Port, &first, &last, &d.Calls, &d.LastOutcome, &d.LastStatusClass, &d.LastProtocol, &d.LastResponseFlags, &reason, &refused}, func() error {
		d.FirstSeenAt, d.LastSeenAt = first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339)
		d.LastCredentialRefusal = refusalOf(reason, refused)
		if d.LastResponseFlags == nil {
			d.LastResponseFlags = []string{}
		}
		destinations = append(destinations, d)
		d.LastResponseFlags = nil // the next row's scan fills a slice of its own
		return nil
	}); err != nil {
		return nil, err
	}
	gaps, err := collectGaps(a.pool.Query(ctx, "SELECT from_at, to_at, reason, calls, app, env FROM egress_gaps WHERE to_at >= $1 ORDER BY from_at, to_at, reason", since))
	if err != nil {
		return nil, err
	}
	return map[string]any{"since": since.Format(time.RFC3339), "destinations": destinations, "gaps": gaps}, nil
}

// egressFloor is the app's floor, enforced, not_enforced or nil (contracts/routes.md, the app's egress read): its
// network (design/EGRESS.md §2.2), while Canyon runs an env of it; on Kubernetes, which has none, the specs its
// envs ran under (§2.5).
func (a *app) egressFloor(ctx context.Context, q querier, id string) (*string, error) {
	enforced := runs.NetworkInternal("$1")
	if a.kube {
		enforced = `EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND run_by = 'canyon') AND NOT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND run_by = 'canyon' AND egress->>'via' IS DISTINCT FROM 'canyon')`
	}
	var floor *string
	err := q.QueryRow(ctx, `SELECT CASE WHEN `+enforced+` THEN 'enforced'
		WHEN EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND run_by = 'canyon') AND (SELECT via FROM egress_settings) = 'canyon' THEN 'not_enforced' END`, id).Scan(&floor)
	return floor, err
}

// egressGaps is every gap that ends at or after since and names no app, or names the app id, the oldest
// first.
func (a *app) egressGaps(r *http.Request, id string, since time.Time) ([]egressGapOut, error) {
	return collectGaps(a.pool.Query(r.Context(), "SELECT from_at, to_at, reason, calls, app, env FROM egress_gaps WHERE to_at >= $1 AND (app IS NULL OR app = $2) ORDER BY from_at, reason", since, id))
}

// collectGaps reads a query's gaps, as a pool's Query answers them.
func collectGaps(rows pgx.Rows, err error) ([]egressGapOut, error) {
	if err != nil {
		return nil, err
	}
	gaps := []egressGapOut{}
	for rows.Next() {
		var g egressGapOut
		var from, to time.Time
		if err := rows.Scan(&from, &to, &g.Reason, &g.Calls, &g.App, &g.Env); err != nil {
			return nil, err
		}
		g.From, g.To = from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)
		gaps = append(gaps, g)
	}
	return gaps, rows.Err()
}

// egressCaps are the admin's three caps (design/EGRESS.md §3.3; #2691): rows an app env keeps, new names it may call
// a minute across every replica, and pending egress requests it may hold. 0130's defaults and ranges.
type egressCaps struct {
	Destinations int `json:"destinations"`
	NewNames     int `json:"new_names"`
	Pending      int `json:"pending"`
}

// egressCapRanges is each cap's range, as 0130's CHECKs hold it.
var egressCapRanges = map[string][2]int{"destinations": {50, 5000}, "new_names": {1, 600}, "pending": {1, 500}}

// egressCapsLine is invalid_caps' message.
const egressCapsLine = "caps is an object of destinations (50 to 5000), new_names (1 to 600) and pending (1 to 500), each a whole number, one key at least"

func (c *egressCaps) field(k string) *int {
	switch k {
	case "destinations":
		return &c.Destinations
	case "new_names":
		return &c.NewNames
	}
	return &c.Pending
}

// capsIn reads a PUT's caps into caps: an object naming one key of egressCapRanges at least and nothing else, each a
// whole number in its range — never a value that means no cap.
func capsIn(raw json.RawMessage, caps *map[string]json.RawMessage) bool {
	if json.Unmarshal(raw, caps) != nil || len(*caps) == 0 {
		return false
	}
	for k, v := range *caps {
		r, known := egressCapRanges[k]
		var n int
		if !known || json.Unmarshal(v, &n) != nil || string(v) != strconv.Itoa(n) || n < r[0] || n > r[1] {
			return false
		}
	}
	return true
}
