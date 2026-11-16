package outboundkeys

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// PolicyRoutes is each env's value routes as the policy names them (contracts/routes.md, GET /egress/policy, from
// layer 3), sorted by destination, never a value, read in the policy's snapshot: an env's only while the signing root
// is among its trusted roots and the host routes its calls through Canyon, else none (Interception, Trust), so a
// destination passes through untouched until every instance trusts the root. It answers the signing root, "" while
// none is live.
func (k *Keys) PolicyRoutes(ctx context.Context, tx pgx.Tx, envs []enterprise.AppEnv) (map[enterprise.AppEnv][]enterprise.PolicyRoute, string, error) {
	t, err := readEgressTrust(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, `SELECT project_id, env, destination, header, scheme, rev FROM egress_routes ORDER BY project_id, env, destination COLLATE "C"`)
	if err != nil {
		return nil, "", err
	}
	held := map[enterprise.AppEnv][]enterprise.PolicyRoute{}
	var ae enterprise.AppEnv
	var route enterprise.PolicyRoute
	if _, err := pgx.ForEachRow(rows, []any{&ae.App, &ae.Env, &route.Destination, &route.Header, &route.Scheme, &route.Rev}, func() error {
		held[ae] = append(held[ae], route)
		return nil
	}); err != nil {
		return nil, "", err
	}
	out := map[enterprise.AppEnv][]enterprise.PolicyRoute{}
	for _, e := range envs {
		if t.live(e.App, e.Env) && len(held[e]) > 0 {
			out[e] = held[e]
		}
	}
	return out, t.signing, nil
}

// RouteValue is one route's value for egress-control, which asks on first use (contracts/routes.md, GET
// /egress/route-value): enterprise.ErrNoRoute when the policy names no such app env or the env holds no route to
// destination. The value is in no error.
func (k *Keys) RouteValue(ctx context.Context, q enterprise.Querier, app, env, destination string) (int64, string, error) {
	var rev int64
	var keyRef int
	var sealedKey, sealedValue []byte
	err := q.QueryRow(ctx, `SELECT r.rev, r.key_ref, r.sealed_key, r.sealed_value FROM egress_routes r JOIN egress_envs e USING (project_id, env)
		WHERE r.project_id = $1 AND r.env = $2 AND r.destination = $3`, app, env, destination).Scan(&rev, &keyRef, &sealedKey, &sealedValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", enterprise.ErrNoRoute
	}
	if err != nil {
		return 0, "", err
	}
	value, err := k.keys.Open(keyRef, app, env, "route:"+destination, sealedKey, sealedValue)
	if err != nil {
		return 0, "", err
	}
	return rev, string(value), nil
}

// Waiting is why each of app id's envs' routes wait, by env, none for an env whose routes are live (contracts/routes.md,
// the app's egress read, waiting): host while the host doesn't route calls through Canyon, adds no credential or hands
// its apps to the org; secret while a runtime secret of the env carries the app's own trust, the first by name; else
// redeploy, until the version running trusts the signing root.
func (k *Keys) Waiting(ctx context.Context, q enterprise.Querier, id string, envs []string) (map[string]*enterprise.RouteWait, error) {
	t, err := readEgressTrust(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]*enterprise.RouteWait{}
	for _, env := range envs {
		var secret *string
		if err := q.QueryRow(ctx, `SELECT min(name COLLATE "C") FROM secrets WHERE project_id = $1 AND env = $2 AND scope = 'runtime' AND name = ANY($3)`, id, env, trustVariables).Scan(&secret); err != nil {
			return nil, err
		}
		switch {
		case t.live(id, env):
		case !t.host:
			out[env] = &enterprise.RouteWait{Reason: "host"}
		case secret != nil:
			out[env] = &enterprise.RouteWait{Reason: "secret", Secret: secret}
		default:
			out[env] = &enterprise.RouteWait{Reason: "redeploy"}
		}
	}
	return out, nil
}
