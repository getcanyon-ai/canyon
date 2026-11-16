package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/runs"
)

// outboundKeys is Canyon Enterprise's outbound keys (ee/outboundkeys; design/EGRESS.md §4; #2922): the code that
// writes a credential an app's calls carry and the code that adds it to a call. The core keeps reading, enforcing and
// removing — a route's validation, every refusal, DELETE, turning credentials off, the reads — and calls it after
// them. Without it (a nil hook) every write is 409 licence_required, the policy names no route, no /run carries a
// root and egress-control intercepts nothing (contracts/routes.md, Licence, A server built without Canyon Enterprise).
type outboundKeys interface {
	runs.Inject
	// Write is a route's write in tx, under the app env's advisory lock, every refusal before it answered.
	Write(ctx context.Context, tx pgx.Tx, in enterprise.RouteWrite) error
	// TurnOn turns credential routes on in tx.
	TurnOn(ctx context.Context, tx pgx.Tx) error
	// PolicyRoutes is each env's routes the policy names, and the signing root, in the policy's snapshot.
	PolicyRoutes(ctx context.Context, tx pgx.Tx, envs []enterprise.AppEnv) (map[enterprise.AppEnv][]enterprise.PolicyRoute, string, error)
	// RouteValue is one route's value, for egress-control alone.
	RouteValue(ctx context.Context, q enterprise.Querier, app, env, destination string) (int64, string, error)
	// Leaf is one inject destination's leaf, minted at now, for egress-control alone, read on tx, a read-only snapshot.
	Leaf(ctx context.Context, tx pgx.Tx, destination string, now time.Time) (enterprise.Leaf, error)
	// RotateCA starts a rotation of the interception root in tx.
	RotateCA(ctx context.Context, tx pgx.Tx, body io.Reader, actor events.Actor, now time.Time) error
	// Start makes sure the host holds an interception root, then rotates it on Canyon's schedule until ctx ends.
	Start(ctx context.Context, pool *pgxpool.Pool, logger *log.Logger) error
	// CA is the interception roots' key dates, read on tx, a read-only snapshot.
	CA(ctx context.Context, tx pgx.Tx) (enterprise.CARead, error)
	// Waiting is why each of app id's envs' routes wait, by env; none for an env whose routes are live.
	Waiting(ctx context.Context, q enterprise.Querier, id string, envs []string) (map[string]*enterprise.RouteWait, error)
}

// injectorOf makes egress-control's interception, Canyon Enterprise's (ee/outboundkeys/intercept): asking the server
// for leaves through leafOf and values through valueAsk, its chains logging to the receiver with token.
type injectorOf interface {
	Injector(leafOf func(context.Context, string) ([]byte, error), valueAsk func(context.Context, string, string, string) ([]byte, error), token string, logger *log.Logger) egresscontrol.Injector
}

// outboundUnlicensed is the refusal of each outbound keys' change without Canyon Enterprise.
var outboundUnlicensed = licenceErr{Feature: "outbound_keys"}

// writeRoute is the hook's Write, refused without it.
func (h eeHooks) writeRoute(ctx context.Context, tx pgx.Tx, in enterprise.RouteWrite) error {
	if h.outbound == nil {
		return outboundUnlicensed
	}
	return h.outbound.Write(ctx, tx, in)
}

// turnOn is the hook's TurnOn, refused without it.
func (h eeHooks) turnOn(ctx context.Context, tx pgx.Tx) error {
	if h.outbound == nil {
		return outboundUnlicensed
	}
	return h.outbound.TurnOn(ctx, tx)
}

// policyRoutes is the hook's PolicyRoutes: without it no env's route, and no signing root.
func (h eeHooks) policyRoutes(ctx context.Context, tx pgx.Tx, envs []enterprise.AppEnv) (map[enterprise.AppEnv][]enterprise.PolicyRoute, string, error) {
	if h.outbound == nil {
		return nil, "", nil
	}
	return h.outbound.PolicyRoutes(ctx, tx, envs)
}

// routeValue is the hook's RouteValue: without it no route answers one.
func (h eeHooks) routeValue(ctx context.Context, q querier, app, env, destination string) (int64, string, error) {
	if h.outbound == nil {
		return 0, "", enterprise.ErrNoRoute
	}
	return h.outbound.RouteValue(ctx, q, app, env, destination)
}

// leafNoRoute is GET /egress/leaf's 404 without Canyon Enterprise, as for a destination no route names.
var leafNoRoute = &enterprise.Refusal{Status: http.StatusNotFound, Code: "no_route", Message: "no app env holds a route to that destination"}

// leaf is the hook's Leaf: without it no destination has one.
func (h eeHooks) leaf(ctx context.Context, tx pgx.Tx, destination string, now time.Time) (enterprise.Leaf, error) {
	if h.outbound == nil {
		return enterprise.Leaf{}, leafNoRoute
	}
	return h.outbound.Leaf(ctx, tx, destination, now)
}

// rotateCA is the hook's RotateCA, refused without it.
func (h eeHooks) rotateCA(ctx context.Context, tx pgx.Tx, body io.Reader, actor events.Actor, now time.Time) error {
	if h.outbound == nil {
		return outboundUnlicensed
	}
	return h.outbound.RotateCA(ctx, tx, body, actor, now)
}

// startOutbound is the hook's Start: without it no root is minted or rotated.
func (h eeHooks) startOutbound(ctx context.Context, pool *pgxpool.Pool, logger *log.Logger) error {
	if h.outbound == nil {
		return nil
	}
	return h.outbound.Start(ctx, pool, logger)
}

// ca is the hook's CA: without it no root and no signing root.
func (h eeHooks) ca(ctx context.Context, tx pgx.Tx) (enterprise.CARead, error) {
	if h.outbound == nil {
		return enterprise.CARead{Roots: []enterprise.CARoot{}}, nil
	}
	return h.outbound.CA(ctx, tx)
}

// waiting is the hook's Waiting: without it every env's routes wait for the host, which adds no credential.
func (h eeHooks) waiting(ctx context.Context, q querier, id string, envs []string) (map[string]*enterprise.RouteWait, error) {
	if h.outbound == nil {
		out := map[string]*enterprise.RouteWait{}
		for _, env := range envs {
			out[env] = &enterprise.RouteWait{Reason: "host"}
		}
		return out, nil
	}
	return h.outbound.Waiting(ctx, q, id, envs)
}

// egressCA is the admin's read of the interception roots' key dates, in one read-only snapshot.
func (a *app) egressCA(ctx context.Context) (enterprise.CARead, error) {
	var out enterprise.CARead
	err := pgx.BeginTxFunc(ctx, a.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		out, err = a.ee.ca(ctx, tx)
		return err
	})
	return out, err
}

// rotateCA starts a rotation of the interception root, an admin's, a person's, from a browser (contracts/routes.md,
// POST /api/v1/egress/ca/rotate): level and actor refused before it (adminOnly, personOnly), then the session, then
// Canyon Enterprise's, which reads the body: without it 409 licence_required, the body unread. 200 is the admin's
// read of the setting.
func (a *app) rotateCA(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.auth == authOIDC && !fromDashboard(ctx) {
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", "")
		return
	}
	err := a.inMutation(ctx, func(tx pgx.Tx) error { return a.ee.rotateCA(ctx, tx, r.Body, actorOf(ctx), time.Now()) })
	var lic licenceErr
	var no *enterprise.Refusal
	switch {
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
	case errors.As(err, &no):
		writeErr(w, no.Status, no.Code, no.Message, no.Fields)
	case err != nil:
		a.log.Printf("canyon-server: rotate the interception root: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		a.getEgress(w, r)
	}
}
