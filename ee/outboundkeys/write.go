package outboundkeys

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// Write is a route's write, a PUT's or a from-secret's (design/EGRESS.md §4.1; contracts/routes.md, PUT and POST
// .../from-secret), in the core's transaction under the app env's advisory lock, every refusal before it answered
// by the core. In order: a destination the env holds no route to needs a licence in force naming outbound_keys —
// replacing a value never does, so a leaked key can always be rotated (#2879; #2881); then the core's
// refusals past the licence; then the route is replaced whole under a new rev, its value sealed under its own vault
// row, a from-secret's secret goes, and a from-secret whose route would not be live at once is refused 409
// would_wait, nothing written; then one egress.route.set row, never a value. The core commits.
func (k *Keys) Write(ctx context.Context, tx pgx.Tx, in enterprise.RouteWrite) error {
	if !in.Held {
		if err := k.licence.Require(ctx, tx, Feature); err != nil {
			return err
		}
	}
	if err := in.Refused(ctx); err != nil {
		return err
	}
	fromSecret := in.Secret != ""
	keyRef, sealedKey, sealedValue, err := k.keys.Seal(in.Project, in.Env, "route:"+in.Destination, []byte(in.Value))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO egress_routes (project_id, env, destination, header, scheme, placeholder_env, from_secret, rev, key_ref, sealed_key, sealed_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nextval('egress_route_rev'), $8, $9, $10)
		ON CONFLICT (project_id, env, destination) DO UPDATE SET header = EXCLUDED.header, scheme = EXCLUDED.scheme, placeholder_env = EXCLUDED.placeholder_env,
			from_secret = EXCLUDED.from_secret, rev = EXCLUDED.rev, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key, sealed_value = EXCLUDED.sealed_value, set_at = now()`,
		in.Project, in.Env, in.Destination, in.Header, in.Scheme, in.Placeholder, fromSecret, keyRef, sealedKey, sealedValue); err != nil {
		return err
	}
	detail := map[string]string{"env": in.Env, "destination": in.Destination}
	if fromSecret {
		detail["secret"] = in.Secret
		if _, err := tx.Exec(ctx, "DELETE FROM secrets WHERE project_id = $1 AND env = $2 AND name = $3 AND scope = 'runtime'", in.Project, in.Env, in.Secret); err != nil {
			return err
		}
		t, err := readEgressTrust(ctx, tx)
		if err != nil {
			return err
		}
		if !t.live(in.Project, in.Env) {
			reason, message := "redeploy", "the version running in "+envWord(in.Env)+" doesn't trust Canyon's certificate until its next deploy or promotion, so the secret stays where it is"
			if !t.host {
				reason, message = "host", "ask an admin to turn on Through Canyon first: apps call out directly here, so Canyon can't add this credential yet"
			}
			return &enterprise.Refusal{Status: http.StatusConflict, Code: "would_wait", Message: message, Fields: map[string]any{"reason": reason}}
		}
	}
	return events.AuditTx(ctx, tx, in.Actor, "egress.route.set", in.Project, "", detail)
}

// TurnOn turns credential routes on in tx, an admin's, once a licence in force names outbound_keys (#2879): the
// core turns them off, and writes the rest of the setting and its audit row.
func (k *Keys) TurnOn(ctx context.Context, tx pgx.Tx) error {
	if err := k.licence.Require(ctx, tx, Feature); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE egress_settings SET credential_routes = true")
	return err
}
