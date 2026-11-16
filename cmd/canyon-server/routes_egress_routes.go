package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/destination"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// Layer 3, Inject (design/EGRESS.md §4.1; design/PERMISSIONS.md, Outbound; contracts/routes.md, Egress control): an
// app env's value routes, which add a stored credential to the app's calls to one destination, so the app never
// holds it. Setting one, or moving a runtime secret into one, is a widening, a person's from a browser; removing one
// works from any session and any actor. A route a secret placed is an owner's. No route, row or log line answers a
// value, but GET /egress/route-value, to egress-control alone.

// trustVariables are the variables that carry an app's own trust, which would replace the roots Canyon adds a
// credential under (contracts/executor.md, egress.ca).
var trustVariables = []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_DIR"}

// executorEgressVariables are the variables the executor sets with egress or egress.ca (contracts/executor.md,
// Egress), which no placeholder may name.
var executorEgressVariables = append([]string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY", "JDK_JAVA_OPTIONS"}, trustVariables...)

// routeHeader is a header a route may be given, in any case; routeHeaderRefused what its lower-case form never is:
// one that frames the request or lives between two hops, or Canyon's or Envoy's own (egress.schema.json, route_header).
var (
	routeHeader        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
	routeHeaderRefused = []string{"connection", "content-length", "host", "keep-alive", "proxy-authenticate", "proxy-authorization", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade"}
)

// routeIn is a route's body as a PUT or a from-secret gives it, checked.
type routeIn struct {
	secret      string // a from-secret's
	header      string // lower-case
	scheme      string
	value       string // a PUT's
	placeholder *string
}

// routeFields is the message of invalid_route for each field.
var routeFields = map[string]string{
	"body":            "the body is {header, scheme, value, placeholder_env?}, or for a secret moved in {secret, header, scheme, placeholder_env?}",
	"header":          "header is a header name, letters, digits and dashes, never Host, a hop-by-hop header or one starting canyon-, x-canyon- or x-envoy-",
	"scheme":          "scheme is bearer, basic or raw",
	"value":           "the value is 1 to 8,192 characters with no control character but a tab, and with basic <user>:<password>",
	"placeholder_env": "placeholder_env is a name a secret may take, never one Canyon sets for an app's calls out",
}

// readRoute reads a route's body: the first field out of shape, or "body" for a body that is no such object — a PUT's
// {header, scheme, value, placeholder_env?}, a from-secret's {secret, header, scheme, placeholder_env?}, each key once.
// A from-secret's placeholder_env, left out, is the secret's own name, checked as a given one is; null is none.
func (a *app) readRoute(body io.Reader, fromSecret bool) (routeIn, string) {
	var in routeIn
	members, ok := strictObject(body)
	keys := []string{"header", "scheme", "value", "placeholder_env"}
	if fromSecret {
		keys[2] = "secret"
	}
	for k := range members {
		ok = ok && slices.Contains(keys, k)
	}
	if fromSecret {
		ok = ok && json.Unmarshal(members["secret"], &in.secret) == nil && validSecretName.MatchString(in.secret)
	}
	if !ok {
		return in, "body"
	}
	if json.Unmarshal(members["header"], &in.header) != nil || !routeHeader.MatchString(in.header) {
		return in, "header"
	}
	in.header = strings.ToLower(in.header)
	if slices.Contains(routeHeaderRefused, in.header) || strings.HasPrefix(in.header, "canyon-") || strings.HasPrefix(in.header, "x-canyon-") || strings.HasPrefix(in.header, "x-envoy-") {
		return in, "header"
	}
	if json.Unmarshal(members["scheme"], &in.scheme) != nil || in.scheme != "bearer" && in.scheme != "basic" && in.scheme != "raw" {
		return in, "scheme"
	}
	if !fromSecret && (json.Unmarshal(members["value"], &in.value) != nil || !routeValueOK(in.scheme, in.value)) {
		return in, "value"
	}
	raw, named := members["placeholder_env"]
	switch {
	case !named && fromSecret:
		in.placeholder = &in.secret
	case named && json.Unmarshal(raw, &in.placeholder) != nil:
		return in, "placeholder_env"
	}
	if in.placeholder != nil && !a.placeholderOK(*in.placeholder) {
		return in, "placeholder_env"
	}
	return in, ""
}

// strictObject reads body as one JSON object, each key once and nothing after it: its members, raw. false for any
// other body. It walks the tokens, as decoding into a map would keep the last of a key given twice.
func strictObject(body io.Reader) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(body)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	members := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		k, isKey := t.(string)
		if _, twice := members[k]; err != nil || !isKey || twice {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		members[k] = raw
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return members, true
}

// routeValueOK is whether value may go into a route's header: 1 to 8,192 characters, no control character but a tab,
// and with basic <user>:<password> (egress.schema.json, route_value).
func routeValueOK(scheme, value string) bool {
	if n := utf8.RuneCountInString(value); n < 1 || n > 8192 {
		return false
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }) {
		return false
	}
	return scheme != "basic" || strings.Contains(value, ":")
}

// placeholderOK is whether name may carry a route's placeholder: a name a runtime secret may take on this host (the
// secrets row), and never one the executor sets with egress or egress.ca.
func (a *app) placeholderOK(name string) bool {
	return runtimeNameOK(name) && (a.kube || !dockerRefuses(name, false)) && !slices.Contains(executorEgressVariables, name)
}

// routeDestination is a route's :destination, canonicalised as an entry: one name, never a *. entry, as a route's
// certificate names one host.
func routeDestination(raw string) (string, bool) {
	entry, err := destination.Entry(raw)
	return entry, err == nil && !strings.HasPrefix(entry, "*.")
}

// secretRouteOwner answers 403 owner_required to a caller below owner when app env's route to dest came from a secret:
// only an owner or an admin replaces or deletes it. true means the response is written.
func (a *app) secretRouteOwner(ctx context.Context, w http.ResponseWriter, id, env, dest string) bool {
	var fromSecret bool
	err := a.pool.QueryRow(ctx, "SELECT coalesce((SELECT from_secret FROM egress_routes WHERE project_id = $1 AND env = $2 AND destination = $3), false)", id, env, dest).Scan(&fromSecret)
	if err != nil {
		a.log.Printf("canyon-server: route of %s %s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return true
	}
	return fromSecret && a.ownerRequired(ctx, w, a.pool, id)
}

// putRoute sets an app env's route to one destination whole, a widening: its value entered with it and sealed under
// the route's own vault row, so no route moves a value to another destination. Refusals in order: the level, the
// actor and the session, then the destination and the body, then — under the app env's advisory lock, as the
// receiver's flush takes it, so a route and a secret carrying an app's own trust never both land — the host, the
// hold, the check and the env's trust secrets. One egress.route.set row, never a value.
func (a *app) putRoute(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	dest, ok := routeDestination(r.PathValue("destination"))
	if ok && a.secretRouteOwner(ctx, w, id, env, dest) || a.refuseWidening(w, r) {
		return
	}
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_destination", invalidRouteDestination, nil)
		return
	}
	in, field := a.readRoute(http.MaxBytesReader(w, r.Body, 64<<10), false)
	if field != "" {
		writeErr(w, http.StatusBadRequest, "invalid_route", routeFields[field], map[string]any{"field": field})
		return
	}
	a.writeRouteTx(w, r, id, env, dest, in, func(context.Context, pgx.Tx) (string, bool, error) { return in.value, true, nil })
}

// invalidRouteDestination is invalid_destination's message on a route: one name, which a certificate names.
const invalidRouteDestination = "a route's destination is one host name: never *., an IP address, a single label, a port or userinfo"

// routeFromSecret moves a runtime secret of the env into a route, an owner's or an admin's widening: in one
// transaction the route is set whole from the secret's value, from_secret, and the secret's row goes. Refused as a
// PUT is, then while the route would wait (would_wait), so a moved key is never stranded. One egress.route.set row
// naming the secret and the destination, never the value.
func (a *app) routeFromSecret(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	if a.ownerRequired(ctx, w, a.pool, id) || a.refuseWidening(w, r) {
		return
	}
	dest, ok := routeDestination(r.PathValue("destination"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_destination", invalidRouteDestination, nil)
		return
	}
	in, field := a.readRoute(http.MaxBytesReader(w, r.Body, 64<<10), true)
	if field != "" {
		writeErr(w, http.StatusBadRequest, "invalid_route", routeFields[field], map[string]any{"field": field})
		return
	}
	a.writeRouteTx(w, r, id, env, dest, in, func(ctx context.Context, tx pgx.Tx) (string, bool, error) {
		var keyRef int
		var origin string
		var sealedKey, sealedValue []byte
		err := tx.QueryRow(ctx, "SELECT coalesce(key_ref, 1), sealed_key, sealed_value, coalesce(origin, 'user') FROM secrets WHERE project_id = $1 AND env = $2 AND name = $3 AND scope = 'runtime'",
			id, env, in.secret).Scan(&keyRef, &sealedKey, &sealedValue, &origin)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErr(w, http.StatusNotFound, "not_found", env+" holds no runtime secret named "+in.secret, nil)
			return "", false, nil
		case err != nil:
			return "", false, err
		case origin == "provisioned":
			writeErr(w, http.StatusConflict, "provisioned", "Canyon provides this value for a declared resource; it cannot be moved by hand", nil)
			return "", false, nil
		}
		value, err := a.vault.Open(keyRef, id, env, in.secret, sealedKey, sealedValue)
		if err != nil {
			return "", false, err
		}
		if !routeValueOK(in.scheme, string(value)) {
			writeErr(w, http.StatusBadRequest, "invalid_route", routeFields["value"], map[string]any{"field": "value"})
			return "", false, nil
		}
		return string(value), true, nil
	})
}

// writeRouteTx is a route's write, a PUT's or a from-secret's, in one transaction under the app env's advisory lock:
// value reads the value, writing its own refusal when it answers false; then 409 egress_external while the org runs
// the apps, as no Canyon gateway would add it; then Canyon Enterprise's write (ee/outboundkeys, #2922), which asks the
// licence for a destination the env holds no route to and then the core's refusals past it, routeRefused, before it
// replaces the route whole. Without Canyon Enterprise every write is 409 licence_required, nothing written
// (contracts/routes.md, Licence, A server built without Canyon Enterprise).
func (a *app) writeRouteTx(w http.ResponseWriter, r *http.Request, id, env, dest string, in routeIn, value func(context.Context, pgx.Tx) (string, bool, error)) {
	ctx, fromSecret := r.Context(), in.secret != ""
	internal := func(err error) {
		a.log.Printf("canyon-server: route to %s on %s %s: %v", dest, id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", egressLock(id, env)); err != nil {
		internal(err)
		return
	}
	if !fromSecret { // read again under the locks every route write takes: a secret moved in while this PUT waited is an owner's
		var placed bool
		err := tx.QueryRow(ctx, "SELECT from_secret FROM egress_routes WHERE project_id = $1 AND env = $2 AND destination = $3 FOR UPDATE", id, env, dest).Scan(&placed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			internal(err)
			return
		}
		if placed && a.ownerRequired(ctx, w, tx, id) {
			return
		}
	}
	plain, ok, err := value(ctx, tx)
	if err != nil || !ok {
		if err != nil {
			internal(err)
		}
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
	var held bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM egress_routes WHERE project_id = $1 AND env = $2 AND destination = $3)", id, env, dest).Scan(&held); err != nil {
		internal(err)
		return
	}
	err = a.ee.writeRoute(ctx, tx, enterprise.RouteWrite{Project: id, Env: env, Destination: dest, Header: in.header, Scheme: in.scheme, Placeholder: in.placeholder,
		Secret: in.secret, Value: plain, Held: held, Actor: actorOf(ctx), Refused: func(ctx context.Context) error { return a.routeRefused(ctx, w, tx, id, env, dest) }})
	var lic licenceErr
	var no *enterprise.Refusal
	switch {
	case errors.Is(err, errAnswered):
		return
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
		return
	case errors.As(err, &no):
		writeErr(w, no.Status, no.Code, no.Message, no.Fields)
		return
	case err != nil:
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	a.egressTaken(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// errAnswered is a refusal routeRefused wrote itself.
var errAnswered = errors.New("the refusal is answered")

// routeRefused judges a route's write on tx, under the app env's advisory lock, past egress_external and the licence,
// in order: 409 credential_routes_off until an admin turns them on (#2143); 409 held for a held production; 409
// destination_not_allowed unless the check would let the env reach dest now — under observe any name, under allowlist
// one an entry allows or a platform destination; 409 trust_secret while a runtime secret of the env carries the app's
// own trust, the first by name. errAnswered means the response is written.
func (a *app) routeRefused(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, id, env, dest string) error {
	var on bool
	if err := tx.QueryRow(ctx, "SELECT credential_routes FROM egress_settings").Scan(&on); err != nil {
		return err
	}
	if !on {
		writeErr(w, http.StatusConflict, "credential_routes_off", "Canyon adds no credentials to apps' calls until an admin turns them on in Settings › Outbound", nil)
		return errAnswered
	}
	if held, err := a.heldProd(ctx, w, tx, id, env); err != nil || held {
		if held {
			return errAnswered
		}
		return err
	}
	var observe bool
	var allowed []string
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT mode FROM egress_modes WHERE project_id = $1 AND env = $2), (SELECT mode FROM egress_settings)) = 'observe',
		coalesce((SELECT array_agg(entry) FROM egress_allowed WHERE project_id = $1 AND env = $2), '{}')`, id, env).Scan(&observe, &allowed); err != nil {
		return err
	}
	if !observe && !slices.ContainsFunc(allowed, func(entry string) bool { return destination.Allows(entry, dest) }) {
		platform, err := a.platformDestinations(ctx, tx)
		if err != nil {
			return err
		}
		if !slices.Contains(platform, dest) {
			writeErr(w, http.StatusConflict, "destination_not_allowed", "allow "+dest+" before you add a credential for it", nil)
			return errAnswered
		}
	}
	secret, err := trustSecret(ctx, tx, id, env)
	if err != nil || secret == nil {
		return err
	}
	writeErr(w, http.StatusConflict, "trust_secret", "delete the secret "+*secret+" first: it would replace the certificates Canyon needs to add this credential", nil)
	return errAnswered
}

// trustSecret is the first runtime secret of app env, by name, that carries the app's own trust; nil for none.
func trustSecret(ctx context.Context, q querier, id, env string) (*string, error) {
	var name *string
	err := q.QueryRow(ctx, `SELECT min(name COLLATE "C") FROM secrets WHERE project_id = $1 AND env = $2 AND scope = 'runtime' AND name = ANY($3)`, id, env, trustVariables).Scan(&name)
	return name, err
}

// errSecretsRoute is a delete of a route a secret placed, by a caller below owner.
var errSecretsRoute = errors.New("the route came from a secret: an owner's to remove")

// envWord is an env as a person reads it: preview, or production.
func envWord(env string) string {
	if env == "prod" {
		return "production"
	}
	return env
}

// deleteRoute removes an app env's route, a narrowing, from any session and any actor, a held production's included:
// the route and its value gone, and a value that came from a secret goes back to none. A route a secret placed is an
// owner's, judged again in the delete itself, so a secret moved in while it waited is never removed by a contributor.
// One egress.route.remove row when it was there.
func (a *app) deleteRoute(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	dest, ok := routeDestination(r.PathValue("destination"))
	if ok && a.secretRouteOwner(ctx, w, id, env, dest) {
		return
	}
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_destination", invalidRouteDestination, nil)
		return
	}
	owner := roleOf(ctx) == "admin"
	level, err := levelOf(ctx, a.pool, id)
	owner = owner || level == "owner"
	if err == nil {
		err = a.inMutation(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "DELETE FROM egress_routes WHERE project_id = $1 AND env = $2 AND destination = $3 AND (NOT from_secret OR $4)", id, env, dest, owner)
			if err != nil || tag.RowsAffected() == 1 {
				if err == nil {
					err = events.AuditTx(ctx, tx, actorOf(ctx), "egress.route.remove", id, "", map[string]string{"env": env, "destination": dest})
				}
				return err
			}
			var placed bool // a route a secret placed while this delete waited
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM egress_routes WHERE project_id = $1 AND env = $2 AND destination = $3)", id, env, dest).Scan(&placed); err != nil || !placed {
				return err
			}
			return errSecretsRoute
		})
	}
	if errors.Is(err, errSecretsRoute) && a.ownerRequired(ctx, w, a.pool, id) {
		return
	}
	if err != nil {
		a.log.Printf("canyon-server: remove the route to %s on %s %s: %v", dest, id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	a.egressTaken(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// routeRead is one route of an app env as the app's egress read answers it: never its value, to any level.
type routeRead struct {
	Destination    string     `json:"destination"`
	Header         string     `json:"header"`
	Scheme         string     `json:"scheme"`
	PlaceholderEnv *string    `json:"placeholder_env"`
	FromSecret     bool       `json:"from_secret"`
	Waiting        *routeWait `json:"waiting"`
}

// routeWait is why the policy does not name a route (contracts/routes.md, Interception, Trust).
type routeWait = enterprise.RouteWait

// egressRoutes fills each env of the app's read with its routes, sorted by destination, and why each waits, Canyon
// Enterprise's answer (outboundKeys.Waiting): null while the policy names it; else, the first that holds, host while
// the host's via is off or its run_by org, secret while a runtime secret of the env carries the app's own trust,
// redeploy while its trusted roots lack the signing root. Without Canyon Enterprise, which adds no credential, host.
func egressRoutes(ctx context.Context, h eeHooks, q querier, id string, envs map[string]*egressEnv) error {
	names := make([]string, 0, len(envs))
	for env, e := range envs {
		e.Routes = []routeRead{}
		names = append(names, env)
	}
	slices.Sort(names)
	waits, err := h.waiting(ctx, q, id, names)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT env, destination, header, scheme, placeholder_env, from_secret FROM egress_routes WHERE project_id = $1 ORDER BY env, destination COLLATE "C"`, id)
	if err != nil {
		return err
	}
	var env string
	var route routeRead
	_, err = pgx.ForEachRow(rows, []any{&env, &route.Destination, &route.Header, &route.Scheme, &route.PlaceholderEnv, &route.FromSecret}, func() error {
		route.Waiting = waits[env]
		envs[env].Routes = append(envs[env].Routes, route)
		route.PlaceholderEnv = nil // the next row's scan fills one of its own
		return nil
	})
	return err
}

// credentialsHeld is the apps that hold a value route, by name: a route works only through Canyon, so via goes off
// and run_by goes to org only once none does (contracts/routes.md, credentials_exist).
func credentialsHeld(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT p.name COLLATE "C" FROM egress_routes r JOIN projects p ON p.id = r.project_id ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// credentialsExist is credentials_exist's message, naming the apps: one app's in the singular (design/EXPERIENCE.md).
func credentialsExist(apps []string) string {
	if len(apps) == 1 {
		return apps[0] + " has credentials that work only through Canyon. Its team needs to set those keys as secrets again and deploy, then remove the credentials."
	}
	return strings.Join(apps, ", ") + " have credentials that work only through Canyon. Their teams need to set those keys as secrets again and deploy, then remove the credentials."
}

// credentials is 409 credentials_exist: apps hold value routes.
type credentials struct{ apps []string }

func (c *credentials) Error() string { return credentialsExist(c.apps) }
