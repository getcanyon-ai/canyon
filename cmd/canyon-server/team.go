package main

import (
	"cmp"
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// callerLevel is the caller's level on the app p (contracts/routes.md, App teams): the highest of its
// grants naming their address ($1), their identity-provider subject under Canyon's sign-in's issuer
// ($2, callerSubject) or a group their session carries ($3), or everyone — every caller past the session
// stage is a member — none when no grant does. A grant's subject is empty until its person signs in, so
// an empty one names nobody.
const callerLevel = `COALESCE((SELECT level FROM project_access WHERE project_id = p.id AND ` + callerGrants + `
	ORDER BY array_position(ARRAY['owner', 'contributor', 'viewer'], level) LIMIT 1), 'none')`

// callerGrants is the grants of project_access that match the caller: callerLevel's, and the ones
// whose roles an app's identity token carries. An email grant bound to a subject under Canyon's sign-in's
// issuer matches that subject alone, and its address — stored lower-cased — only while it is bound to
// none there (boundHere), so a grant bound to one person never names another who holds the address now; one
// bound under another issuer is read as not yet bound (design/PERMISSIONS.md, Grants). An empty address or
// subject names nobody: a caller with neither matches no person's grant.
// With no issuer to read subjects under, no bound grant matches at all, not even by address.
const callerGrants = `(kind = 'email' AND subject <> '' AND ` + boundHere + ` AND ($2::text[])[2] <> '' AND subject = ($2::text[])[2]
	OR kind = 'email' AND (subject = '' OR ($2::text[])[1] <> '' AND NOT ` + boundHere + `) AND $1 <> '' AND grantee = lower($1)
	OR kind = 'group' AND grantee = ANY($3::text[]) OR kind = 'everyone')`

// boundHere is whether a bound grant is bound under Canyon's sign-in's issuer, callerSubject's first: its
// subject_issuer that issuer, or empty — a binary's that recorded none, which read every subject under it.
const boundHere = `(($2::text[])[1] <> '' AND subject_issuer IN (($2::text[])[1], ''))`

// callerSubject is callerGrants' $2: the issuer of Canyon's sign-in, and the caller's subject under it.
func callerSubject(ctx context.Context) []string {
	return []string{issuerOf(ctx), actorOf(ctx).Subject}
}

// rolesOf is the caller's app roles on the app: the union of the roles of the grants that match them
// (contracts/routes.md, App teams), sorted, [] when none do.
func rolesOf(ctx context.Context, q querier, id string) ([]string, error) {
	var roles []string
	err := q.QueryRow(ctx, "SELECT COALESCE(array_agg(DISTINCT role ORDER BY role), '{}') FROM project_access, unnest(roles) role WHERE project_id = $4 AND "+callerGrants,
		addressOf(ctx), callerSubject(ctx), groupsOf(ctx), id).Scan(&roles)
	return roles, err
}

// callerArgs are the caller as the reads that judge every app take them: $1 their address, $2 their
// subject (callerSubject), $3 their groups, $4 whether they are an admin, who reads every app whole.
func callerArgs(ctx context.Context) []any {
	return []any{addressOf(ctx), callerSubject(ctx), groupsOf(ctx), roleOf(ctx) == "admin"}
}

// teamApps are the apps the caller is contributor or above on, deleted ones too — their rows stay on
// the trail; nil for an admin, who reads every app's rows.
func teamApps(ctx context.Context, q querier) ([]string, error) {
	return appsAt(ctx, q, "owner", "contributor")
}

// appsAt are the apps the caller holds one of levels on, deleted ones too; nil for an admin.
func appsAt(ctx context.Context, q querier, levels ...string) ([]string, error) {
	if roleOf(ctx) == "admin" {
		return nil, nil
	}
	rows, err := q.Query(ctx, "SELECT p.id FROM projects p WHERE "+callerLevel+" = ANY($4)", addressOf(ctx), callerSubject(ctx), groupsOf(ctx), levels)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// levelOf is the caller's level on the app, callerLevel's: none on an id no app carries.
func levelOf(ctx context.Context, q querier, id string) (string, error) {
	var level string
	err := q.QueryRow(ctx, "SELECT "+callerLevel+" FROM (SELECT $4::text AS id) p", addressOf(ctx), callerSubject(ctx), groupsOf(ctx), id).Scan(&level)
	return level, err
}

// ownersOf is the app's owners[] as the project read answers it: its email grants at owner, oldest
// first, each the actor triple, its display the grant's, else its address.
func ownersOf(ctx context.Context, q querier, id string) ([]actor, error) {
	rows, err := q.Query(ctx, `SELECT 'person', subject, COALESCE(NULLIF(display, ''), grantee) FROM project_access
		WHERE project_id = $1 AND kind = 'email' AND level = 'owner' ORDER BY granted_at, grantee`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[actor])
}

// ownerGroupsOf is the names of the groups holding a grant at owner on the app, sorted.
func ownerGroupsOf(ctx context.Context, q querier, id string) ([]string, error) {
	var groups []string
	err := q.QueryRow(ctx, "SELECT COALESCE(array_agg(grantee ORDER BY grantee), '{}') FROM project_access WHERE project_id = $1 AND kind = 'group' AND level = 'owner'", id).Scan(&groups)
	return groups, err
}

// askWho is whom a refusal on an app asks (contracts/routes.md, App teams): its first owner, else a
// member of its first owning group, else an admin.
func askWho(owners []actor, groups []string) string {
	switch {
	case len(owners) > 0:
		return owners[0].Display
	case len(groups) > 0:
		return "a member of the " + groups[0] + " group"
	}
	return "an admin"
}

// ownerRequired answers 403 owner_required to a caller who is neither an admin nor an owner of the app,
// saying whom to ask (contracts/routes.md, App teams). true means the response is written.
func (a *app) ownerRequired(ctx context.Context, w http.ResponseWriter, q querier, id string) bool {
	if roleOf(ctx) == "admin" {
		return false
	}
	level, err := levelOf(ctx, q, id)
	if err != nil {
		a.log.Printf("canyon-server: level on %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return true
	}
	if level != "owner" {
		owners, err := ownersOf(ctx, q, id)
		var groups []string
		if err == nil {
			groups, err = ownerGroupsOf(ctx, q, id)
		}
		if err != nil {
			a.log.Printf("canyon-server: owners of %s: %v", id, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return true
		}
		writeErr(w, http.StatusForbidden, "owner_required", "only the owners of "+id+" and admins can do this: ask "+askWho(owners, groups), nil)
		deny(ctx, "owner_required", id)
		return true
	}
	return false
}

// teamRequired answers 403 team_required to a caller below contributor on the app, naming its owners,
// people and groups, so they know whom to ask, and nothing else of it (contracts/routes.md, App teams). An admin passes.
// true means the response is written.
func (a *app) teamRequired(ctx context.Context, w http.ResponseWriter, q querier, id string) bool {
	if roleOf(ctx) == "admin" {
		return false
	}
	level, err := levelOf(ctx, q, id)
	if err == nil && (level == "owner" || level == "contributor") {
		return false
	}
	var owners []actor
	var groups []string
	if err == nil {
		owners, err = ownersOf(ctx, q, id)
	}
	if err == nil {
		groups, err = ownerGroupsOf(ctx, q, id)
	}
	if err != nil {
		a.log.Printf("canyon-server: team of %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return true
	}
	// Each says the button on the app's page that asks: the read-only member's (#2774), and the no-access page's
	// (the v0.11.0 role walk), so a refusal shown anywhere ends in a next step.
	said := "Ask " + askWho(owners, groups) + " to add you to " + id + "'s team: on its page in Canyon, use Ask for access."
	if level == "viewer" {
		said = "Ask " + askWho(owners, groups) + " to add you to " + id + "'s team: on its page in Canyon, use Ask to join its team."
	}
	writeErr(w, http.StatusForbidden, "team_required", said, map[string]any{"owners": owners, "owner_groups": groups})
	deny(ctx, "team_required", id)
	return true
}

// teamOnly is a route under /projects/{id}/ whose minimum level is contributor (contracts/routes.md,
// App teams): an unknown or deleted app is 404, then a caller below contributor is refused, both
// before the handler reads anything of the app. An admin passes to the handler, which answers its own 404.
func (a *app) teamOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, id := r.Context(), r.PathValue("id")
		if roleOf(ctx) != "admin" && (!a.projectLive(ctx, w, id) || a.teamRequired(ctx, w, a.pool, id)) {
			return
		}
		next(w, r)
	}
}

// reviewOnly is teamOnly on a route that is also a responder's with no grant: what approving a blocking
// finding needs (design/PERMISSIONS.md, Security responders; #2204). An unknown or deleted app is 404 first.
func (a *app) reviewOnly(next http.HandlerFunc) http.HandlerFunc {
	team := a.teamOnly(next)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if roleOf(ctx) != "responder" {
			team(w, r)
			return
		}
		if a.projectLive(ctx, w, r.PathValue("id")) {
			next(w, r)
		}
	}
}

// runTeamOnly is teamOnly on a /gate-runs/ route, whose app is its run's: an unknown run is 404 first.
// A run of a deleted app stays readable to its team, as its timeline does (contracts/routes.md).
func (a *app) runTeamOnly(next http.HandlerFunc) http.HandlerFunc { return a.runTeam(next, false) }

// runReviewOnly is runTeamOnly on a /gate-runs/ route that is also a responder's with no grant, as reviewOnly.
func (a *app) runReviewOnly(next http.HandlerFunc) http.HandlerFunc { return a.runTeam(next, true) }

// runTeam is runTeamOnly, letting a responder through with no grant when responders is true.
func (a *app) runTeam(next http.HandlerFunc, responders bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if roleOf(ctx) == "admin" {
			next(w, r)
			return
		}
		run, project := cmp.Or(r.PathValue("id"), r.PathValue("run")), ""
		err := a.pool.QueryRow(ctx, "SELECT project_id FROM runs WHERE id = $1", run).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
			return
		}
		if err != nil {
			a.log.Printf("canyon-server: the app of run %s: %v", run, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if responders && roleOf(ctx) == "responder" { // with no grant, never a deleted app's runs: they are its team's alone
			if a.projectLive(ctx, w, project) {
				next(w, r)
			}
			return
		}
		next(w, r) // PLANTED FAULT: a run's routes skip the team check
	}
}
