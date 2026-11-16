package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// routesAccess registers the Access group (contracts/routes.md): an app's team, read and changed by its
// owners and admins, and the requests of members without access. Nothing is shared where nobody signs
// in, so a host with auth off routes none of it and the catch-all answers 404.
func routesAccess(api *http.ServeMux, a *app) {
	if a.auth != authOIDC {
		return
	}
	api.HandleFunc("GET /projects/{id}/access", a.ownerOnly(a.getAccess))
	api.HandleFunc("PUT /projects/{id}/access/{kind}/{grantee}", a.ownerOnly(a.share))
	api.HandleFunc("DELETE /projects/{id}/access/{kind}/{grantee}", a.ownerOnly(a.unshare))
	api.HandleFunc("POST /projects/{id}/access-requests", a.askForAccess)
	api.HandleFunc("DELETE /projects/{id}/access-requests/{email}", a.ownerOnly(a.dismissRequest))
}

// ownerOnly is a route whose minimum level is owner (contracts/routes.md, App teams): an unknown or
// deleted app is 404, then a caller who is neither an owner nor an admin is 403 owner_required, both
// before the handler reads anything.
func (a *app) ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, id := r.Context(), r.PathValue("id")
		if a.projectLive(ctx, w, id) && !a.ownerRequired(ctx, w, a.pool, id) {
			next(w, r)
		}
	}
}

// declaredRolesSQL is every role a package of app $1 declares, the newest package's line for each; a
// pull request's or a check's declarations are not the app's.
const declaredRolesSQL = `SELECT DISTINCT ON (d.key) d.key AS name, d.value AS description
	FROM packages p CROSS JOIN LATERAL jsonb_each_text(COALESCE(p.roles, '{}')) d
	WHERE p.project_id = $1 AND p.source_kind NOT IN ('repo-pr', 'check') ORDER BY d.key, p.created_at DESC, p.id DESC`

// accessReadSQL is the access read as one document: everyone's grant, the email and group grants oldest
// first, the declared roles by name, and the pending requests oldest first.
const accessReadSQL = `SELECT json_build_object(
	'everyone', (SELECT json_build_object('roles', roles) FROM project_access WHERE project_id = $1 AND kind = 'everyone'),
	'entries', COALESCE((SELECT json_agg(json_build_object('kind', kind, 'grantee', grantee, 'display', COALESCE(NULLIF(display, ''), grantee), 'level', level, 'roles', roles,
		'granted_by', json_build_object('kind', granted_by_kind, 'subject', granted_by_subject, 'display', granted_by_display),
		'granted_at', to_char(granted_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')) ORDER BY granted_at, kind, grantee)
		FROM project_access WHERE project_id = $1 AND kind <> 'everyone'), '[]'),
	'roles', COALESCE((SELECT json_agg(json_build_object('name', name, 'description', description) ORDER BY name) FROM (` + declaredRolesSQL + `) d), '[]'),
	'requests', COALESCE((SELECT json_agg(json_build_object('email', email, 'display', display,
		'requested_at', to_char(requested_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'level', level) ORDER BY requested_at, email)
		FROM access_requests WHERE project_id = $1), '[]'))`

// accessFailed logs what failed and answers the contract's 500; the detail stays in the log.
func (a *app) accessFailed(w http.ResponseWriter, what string, err error) {
	a.log.Printf("canyon-server: %s: %v", what, err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

func accessOf(ctx context.Context, q querier, id string) (json.RawMessage, error) {
	var read json.RawMessage
	err := q.QueryRow(ctx, accessReadSQL, id).Scan(&read)
	return read, err
}

func (a *app) getAccess(w http.ResponseWriter, r *http.Request) {
	read, err := accessOf(r.Context(), a.pool, r.PathValue("id"))
	if err != nil {
		a.accessFailed(w, "access of "+r.PathValue("id"), err)
		return
	}
	writeJSON(w, http.StatusOK, read)
}

// grantOf is the grant the path names, as stored: an email grant's lower-cased address, a group's name,
// everyone's "" under the word everyone. false when the path names none of the three.
func grantOf(r *http.Request) (kind, grantee string, ok bool) {
	kind, grantee = r.PathValue("kind"), r.PathValue("grantee")
	switch kind {
	case "email":
		grantee = strings.ToLower(grantee)
		return kind, grantee, plainText(grantee, 254) && isEmail(grantee)
	case "group":
		return kind, grantee, plainText(grantee, 256)
	case "everyone":
		return kind, "", grantee == "everyone"
	}
	return kind, grantee, false
}

// heldGrant is what the grantee holds now, a nil level when nothing, and how many grants at owner the
// app has. Read under the ledger lock every team write takes, so nothing lands between it and the write.
func heldGrant(ctx context.Context, tx pgx.Tx, id, kind, grantee string) (level *string, roles []string, owners int, err error) {
	err = tx.QueryRow(ctx, `SELECT g.level, g.roles, (SELECT count(*) FROM project_access WHERE project_id = $1 AND level = 'owner')
		FROM (SELECT 1) one LEFT JOIN project_access g ON g.project_id = $1 AND g.kind = $2 AND g.grantee = $3`, id, kind, grantee).Scan(&level, &roles, &owners)
	return level, roles, owners, err
}

// widens is whether a grant at level with roles gives more than the one held: none held, a higher level, or a role
// the held grant lacks. A level no higher with roles it already holds narrows or repeats it.
func widens(held *string, heldRoles []string, level string, roles []string) bool {
	rank := map[string]int{"viewer": 0, "contributor": 1, "owner": 2}
	return held == nil || rank[level] > rank[*held] || slices.ContainsFunc(roles, func(r string) bool { return !slices.Contains(heldRoles, r) })
}

func lastOwner(w http.ResponseWriter) {
	writeErr(w, http.StatusConflict, "last_owner", "add a successor first", nil)
}

// share writes one grant whole (contracts/routes.md, Access): a grantee's level and roles are replaced,
// who first added them and when stay. An email grant takes the name and subject of that address's
// pending request, and ends it.
func (a *app) share(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	kind, grantee, ok := grantOf(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_grantee", "a grant names an email address, a group, or everyone", nil)
		return
	}
	body := &struct { // a pointer, so a null body is no object; roles omitted stays [], and roles null is nil
		Level string   `json:"level"`
		Roles []string `json:"roles"`
	}{Roles: []string{}}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body == nil || body.Roles == nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "invalid_body", "the body is one JSON object with level and roles", nil)
		return
	}
	if !slices.Contains([]string{"owner", "contributor", "viewer"}, body.Level) || kind == "everyone" && body.Level != "viewer" {
		writeErr(w, http.StatusBadRequest, "invalid_level", "a level is owner, contributor or viewer, and everyone's is viewer", nil)
		return
	}
	roles := append([]string{}, body.Roles...) // a set, held sorted: one spelling, so a PUT that changes nothing is seen to
	slices.Sort(roles)
	roles = slices.Compact(roles)
	if kind == "group" {
		o, err := a.orgOf(ctx, a.pool) // the claim as sign-in reads it: CANYON_GROUPS_CLAIM over the row's
		if err != nil {
			a.accessFailed(w, "groups claim", err)
			return
		}
		if o.groupsClaim == "" {
			writeErr(w, http.StatusConflict, "groups_unsupported", "This Canyon doesn't read groups from your sign-in yet. An admin sets the groups claim in Settings.", nil)
			return
		}
	}
	var declared []string
	rows, err := a.pool.Query(ctx, "SELECT name FROM ("+declaredRolesSQL+") d", id)
	if err == nil {
		declared, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		a.accessFailed(w, "roles of "+id, err)
		return
	}
	for _, role := range roles {
		if !slices.Contains(declared, role) {
			writeErr(w, http.StatusBadRequest, "unknown_role", fmt.Sprintf("no package of %s declares the role %q", id, role), nil)
			return
		}
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.accessFailed(w, "share "+id, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	level, held, owners, err := heldGrant(ctx, tx, id, kind, grantee)
	if err != nil {
		a.accessFailed(w, "share "+id, err)
		return
	}
	if level != nil && *level == "owner" && body.Level != "owner" && owners == 1 {
		lastOwner(w)
		return
	}
	var subject string // the grant's, bound by the request it granted or before
	var lic licenceErr
	switch {
	case kind == "group" && widens(level, held, body.Level, roles): // Canyon Enterprise's (ee/groupsync), under a licence naming group_sync
		err = a.ee.grantGroup(ctx, tx, id, grantee, body.Level, roles, act)
	case kind == "group": // repeated or narrowed, the row it holds: free, so a lapsed licence changes nobody's access and never stands between an owner and taking access away (#2879; #2881)
		_, err = tx.Exec(ctx, "UPDATE project_access SET level = $4, roles = $5 WHERE project_id = $1 AND kind = $2 AND grantee = $3", id, kind, grantee, body.Level, roles)
	default: // a person or everyone: never a group, which the core neither adds nor raises
		err = tx.QueryRow(ctx, `WITH asked AS (DELETE FROM access_requests WHERE project_id = $1 AND email = $3 AND $2 = 'email' RETURNING subject, subject_issuer, display)
		INSERT INTO project_access (project_id, kind, grantee, level, roles, subject, subject_issuer, display, granted_by_kind, granted_by_subject, granted_by_display)
		SELECT $1, $2, $3, $4, $5, COALESCE(asked.subject, ''), COALESCE(asked.subject_issuer, ''), COALESCE(asked.display, ''), $6, $7, $8 FROM (SELECT 1) one LEFT JOIN asked ON true WHERE $2 <> 'group'
		ON CONFLICT (project_id, kind, grantee) DO UPDATE SET level = EXCLUDED.level, roles = EXCLUDED.roles RETURNING subject`,
			id, kind, grantee, body.Level, roles, act.Kind, act.Subject, act.Display).Scan(&subject)
	}
	if errors.As(err, &lic) {
		writeLicenceRequired(w, lic)
		return
	}
	if err != nil {
		a.accessFailed(w, "share "+id, err)
		return
	}
	if kind == "email" {
		if err := noteBinding(ctx, tx, subject, grantee, nil); err != nil {
			a.accessFailed(w, "share "+id, err)
			return
		}
	}
	if level == nil || *level != body.Level || !slices.Equal(held, roles) {
		detail := map[string]any{"kind": kind, "grantee": cmp.Or(grantee, "everyone"),
			"level": map[string]any{"before": level, "after": body.Level}, "roles": map[string]any{"before": held, "after": roles}}
		if err := events.AuditTx(ctx, tx, act, "share app", id, "", detail); err != nil {
			a.accessFailed(w, "share "+id, err)
			return
		}
	}
	read, err := accessOf(ctx, tx, id)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil {
		_, _, err = a.reput(ctx, id) // before the answer: the running app's file holds the grant (the front-door spec's B1)
	}
	if err != nil {
		a.accessFailed(w, "share "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, read)
}

// unshare removes one grant and audits what it held; the app's last grant at owner stays.
func (a *app) unshare(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	kind, grantee, ok := grantOf(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such grant", nil)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.accessFailed(w, "unshare "+id, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	level, roles, owners, err := heldGrant(ctx, tx, id, kind, grantee)
	switch {
	case err != nil:
		a.accessFailed(w, "unshare "+id, err)
		return
	case level == nil:
		writeErr(w, http.StatusNotFound, "not_found", "no such grant", nil)
		return
	case *level == "owner" && owners == 1:
		lastOwner(w)
		return
	}
	if _, err := tx.Exec(ctx, "DELETE FROM project_access WHERE project_id = $1 AND kind = $2 AND grantee = $3", id, kind, grantee); err != nil {
		a.accessFailed(w, "unshare "+id, err)
		return
	}
	detail := map[string]any{"kind": kind, "grantee": cmp.Or(grantee, "everyone"), "level": *level, "roles": roles}
	if err := events.AuditTx(ctx, tx, act, "unshare app", id, "", detail); err != nil {
		a.accessFailed(w, "unshare "+id, err)
		return
	}
	err = tx.Commit(ctx)
	if err == nil {
		_, _, err = a.reput(ctx, id) // before the answer: the running app's file no longer holds the grant
	}
	if err != nil {
		a.accessFailed(w, "unshare "+id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// askForAccess is the no-access screen's button: a member at level none asks with their session's
// address and name, once per app — asking again while the request is pending changes nothing. Anyone
// who can already change the app, an admin among them, is already on the team. No email is sent. The
// request records the asker's level, through their grants, their groups' and the org's alike: none, or
// viewer, a read-only member asking to join the team (#2774). A level it can't resolve is a 500 and logged.
func (a *app) askForAccess(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	if !a.projectLive(ctx, w, id) {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.accessFailed(w, "ask for "+id, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	level, err := levelOf(ctx, tx, id) // under the lock a share takes, so none is still none at the insert
	if err != nil {
		a.accessFailed(w, "level on "+id, err)
		return
	}
	if (level != "none" && level != "viewer") || roleOf(ctx) == "admin" { // a viewer asks to change it (#1706)
		writeErr(w, http.StatusConflict, "already_on_team", "you can already change this app", nil)
		return
	}
	// The share binds the grant to the subject the request records. Asking again changes nothing; another
	// person at the address replaces the pending request only from a verified address, and is refused
	// rather than silently unbind one a verified person made.
	subject, pending := a.boundSubject(ctx), "" // pending: the request's subject, when it is under this sign-in's issuer, or '' by a binary that recorded none
	if err := tx.QueryRow(ctx, "SELECT CASE WHEN subject_issuer IN ($3, '') THEN subject ELSE '' END FROM access_requests WHERE project_id = $1 AND email = $2", id, addressOf(ctx), issuerOf(ctx)).Scan(&pending); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.accessFailed(w, "ask for "+id, err)
		return
	}
	if subject == "" && pending != "" {
		writeErr(w, http.StatusConflict, "request_pending", "another person's request from this address is pending: an owner decides it first", nil)
		return
	}
	tag, err := tx.Exec(ctx, `INSERT INTO access_requests (project_id, email, subject, subject_issuer, display, level) VALUES ($1, $2, $3, $5, $4, $6)
		ON CONFLICT (project_id, email) DO UPDATE SET subject = EXCLUDED.subject, subject_issuer = EXCLUDED.subject_issuer, display = EXCLUDED.display, level = EXCLUDED.level
		WHERE (access_requests.subject, access_requests.subject_issuer, access_requests.level) IS DISTINCT FROM (EXCLUDED.subject, EXCLUDED.subject_issuer, EXCLUDED.level)`, id, addressOf(ctx), subject, act.Display, a.boundIssuer(ctx), level)
	if err == nil && tag.RowsAffected() == 1 {
		err = events.AuditTx(ctx, tx, act, "request access", id, "", map[string]any{"email": addressOf(ctx)})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.accessFailed(w, "ask for "+id, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// dismissRequest ends one pending request without granting anything.
func (a *app) dismissRequest(w http.ResponseWriter, r *http.Request) {
	ctx, id, email := r.Context(), r.PathValue("id"), strings.ToLower(r.PathValue("email"))
	tx, err := a.mutation(ctx)
	if err != nil {
		a.accessFailed(w, "dismiss on "+id, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "DELETE FROM access_requests WHERE project_id = $1 AND email = $2", id, email)
	if err == nil && tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "no such request", nil)
		return
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "dismiss access request", id, "", map[string]any{"email": email})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.accessFailed(w, "dismiss on "+id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
