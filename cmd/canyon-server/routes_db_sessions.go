package main

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
)

// Database sessions (contracts/routes.md, Database sessions; design/DATABASE-SESSIONS.md): a person opens,
// attaches to, lists and ends their reach into an app's Canyon-provisioned Postgres, as their own role.
// Who may is design/PERMISSIONS.md's, Database sessions.
// unsafeSchemaSaid is unsafe_schema's message, the open's and the connect's alike.
var unsafeSchemaSaid = provisioners.ErrUnsafeSchema.Error() + ". Ask an admin of this app's database to run REVOKE CREATE ON SCHEMA public FROM PUBLIC, then try again."

// notReadySaid is database_not_ready's message: a database Canyon has no ready record of, or one the login found gone.
func notReadySaid(resource, app, env string) string {
	return "The database " + resource + " of " + app + " in " + env + " isn't ready: deploy " + app + " to " + env + " again."
}

// loginRefusal is the 409 the open, the query and the connect answer for an Issue error that refused a database the
// login reached, its code and message (contracts/routes.md, the db-sessions and connect rows): "" for any other, a
// database that did not answer, which is 502 database_unreachable and only that (#2910).
func loginRefusal(err error, app, env, resource string) (code, message string) {
	var found *provisioners.NotAsMade
	var unsafe *provisioners.ReadOnlyUnsafe
	switch {
	case errors.Is(err, provisioners.ErrUnsafeSchema):
		return "unsafe_schema", unsafeSchemaSaid
	case errors.Is(err, provisioners.ErrLimitsUnsupported):
		return "limits_unsupported", err.Error()
	case errors.As(err, &found):
		return "not_as_made", "Canyon didn't log you in to the database of " + app + " in " + env + ": " + found.Found + ". Ask an admin of this app's database to remove that, then try again."
	case errors.Is(err, provisioners.ErrNoDatabase):
		return "database_not_ready", notReadySaid(resource, app, env)
	case errors.As(err, &unsafe): // a read connection's or a read query's: what the role could change, or open, named
		return "read_only_unsafe", "Canyon didn't open a read connection: anyone connecting could still change data here (" + unsafe.Error() + "). Ask an admin of this app's database to revoke those rights, then connect again."
	}
	return "", ""
}

func routesDBSessions(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects/{id}/db-sessions", a.teamOnly(a.openDBSession))
	api.HandleFunc("GET /projects/{id}/db-sessions", a.teamOnly(a.listDBSessions))
	api.HandleFunc("DELETE /db-sessions/{sid}", a.endDBSession)
}

// openDBSession opens the caller's database session on one of the app's databases, or attaches to the one
// they have open there in that mode, and answers its read: 201 new, 200 attached, never a credential. The
// refusals come in the contract's order: the app, the team, the actor, the body, db_prod, the reason, the owner, the
// browser, the hold, the database, the open session, the rate, the login. Under the person role's lock the
// open row is read, the other mode closed on replace, and one login issued whose connect does nothing — it
// judges the schema and the limits as a connection would — before anything is written.
// dbConnected is whether a client is connected through the person's open database session: the query switches it on
// its own only while none is (#2843's review), so a query never drops a live canyon db connect. A count it can't read
// is a connection: the switch fails closed, to session_open. A query's own backend outlives its Terminate by a few
// milliseconds, so a count above none is read again for up to a second before it stands.
// ponytail: a second's wait on the refusal path alone; count by application_name in the driver if it ever matters.
func (a *app) dbConnected(ctx context.Context, issuer provisioners.Issuer, k provisioners.Key, open dbRow) bool {
	if issuer == nil {
		return true
	}
	for tries := 0; ; tries++ {
		activity, err := issuer.Activity(ctx, k, open.person())
		switch {
		case err != nil:
			a.log.Printf("canyon-server: database session %s: its connections: %v", open.id, err)
			return true
		case activity.Live == 0:
			return false
		case tries == 10:
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dbProdDeferred is db_prod_off's and db_prod_read_only's sentence while production database sessions are deferred
// (#2121, #2835): no page sets db_prod yet, so the refusal names none.
const dbProdDeferred = "Canyon doesn't open production databases yet: this version opens preview databases only."

func (a *app) openDBSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.dbPerson(w, r, id) {
		return
	}
	var body dbOpenBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if status, s, ok := a.dbOpen(w, r, id, body, nil); ok {
		writeJSON(w, status, s.read(r.Context()))
	}
}

// dbPerson is the open's first refusals, the query's too, after the team's (teamOnly): an admin's app that
// isn't there, then an actor who isn't a person. false means the response is written.
func (a *app) dbPerson(w http.ResponseWriter, r *http.Request, id string) bool {
	ctx := r.Context()
	if roleOf(ctx) == "admin" && !a.projectLive(ctx, w, id) {
		return false
	}
	if actorOf(ctx).Kind != "person" {
		writeErr(w, http.StatusForbidden, "person_required", "only a person opens a database session", nil)
		deny(ctx, "person_required", id)
		return false
	}
	return a.dbSessionsAllowed(w, r)
}

// dbSessionsAllowed is false, the refusal written, while database sessions are off or their switch can't be read:
// before anything is issued (#2835).
func (a *app) dbSessionsAllowed(w http.ResponseWriter, r *http.Request) bool {
	on, err := a.dbSessionsOn(r.Context(), a.pool)
	switch {
	case err != nil:
		a.internal(w, "database sessions' switch", err)
	case !on:
		writeErr(w, http.StatusConflict, "db_sessions_off", dbSessionsOff, nil)
	}
	return err == nil && on
}

// dbOpenBody is what opens or attaches to a database session: the open's whole body, and the query's but its
// statement and its bounds (contracts/routes.md, Database sessions).
type dbOpenBody struct {
	Env      string  `json:"env"`
	Mode     string  `json:"mode"`
	Resource *string `json:"resource"`
	Reason   *string `json:"reason"`
	Replace  bool    `json:"replace"`
}

// dbOpen is the open past dbPerson, for the open route and the query route alike (#2118): the body's checks,
// db_prod, the reason, the owner, the browser, the hold, the database, the open session, the rate, then, under the person
// role's lock, one login, committed with the row it opens or attaches to. connect, nil for an open, is that
// login's one connect, handed the database session's id; the login opens nothing once it returns. It answers
// 201 for a new row, 200 for an attach, and false once it wrote a refusal.
func (a *app) dbOpen(w http.ResponseWriter, r *http.Request, id string, body dbOpenBody, connect func(context.Context, provisioners.Issuer, provisioners.Login, string) error) (int, *dbRow, bool) {
	ctx, act := r.Context(), actorOf(r.Context())
	switch {
	case body.Env != "preview" && body.Env != "prod":
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return 0, nil, false
	case body.Mode != "read" && body.Mode != "write":
		writeErr(w, http.StatusBadRequest, "invalid_mode", "mode is read or write", nil)
		return 0, nil, false
	}
	internal := func(what string, err error) {
		a.log.Printf("canyon-server: database session on %s %s: %s: %v", id, body.Env, what, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	declared, err := dbDeclared(ctx, a.pool, id)
	if err != nil {
		internal("its databases", err)
		return 0, nil, false
	}
	resource := ""
	switch {
	case body.Resource != nil && slices.Contains(declared, *body.Resource):
		resource = *body.Resource
	case body.Resource == nil && len(declared) == 1:
		resource = declared[0]
	}
	k, signin := provisioners.Key{Project: id, Env: body.Env, Name: resource}, signinOf(ctx)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		internal("begin", err)
		return 0, nil, false
	}
	defer func() { _ = tx.Rollback(ctx) }()
	on := false // read again under the switch's lock, held to the commit: an open racing turn-off ends with it (#2864)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))", dbSwitch); err == nil {
		on, err = a.dbSessionsOn(ctx, tx)
	}
	switch {
	case err != nil:
		internal("database sessions' switch", err)
		return 0, nil, false
	case !on:
		writeErr(w, http.StatusConflict, "db_sessions_off", dbSessionsOff, nil)
		return 0, nil, false
	}
	var open *dbRow
	newSignin := false
	if resource != "" {
		if err = lockRole(ctx, tx, k, act.Subject); err == nil {
			open, err = openRow(ctx, tx, k, act.Subject)
		}
		if err == nil && open != nil && signin != "" {
			err = tx.QueryRow(ctx, "SELECT NOT EXISTS (SELECT 1 FROM db_session_signins WHERE db_session = $1 AND session = $2)", open.id, signin).Scan(&newSignin)
		}
		if err != nil {
			internal("its open database session", err)
			return 0, nil, false
		}
	}
	// a read query runs in an open write session, read-only (contracts/routes.md, Attach, don't replace)
	opens := open == nil || open.mode != body.Mode && (connect == nil || body.Mode != "read")
	if body.Env == "prod" {
		prod, err := a.dbProd(ctx, tx)
		if err != nil {
			internal("db_prod", err)
			return 0, nil, false
		}
		switch {
		case prod == "off":
			writeErr(w, http.StatusConflict, "db_prod_off", dbProdDeferred, nil)
			return 0, nil, false
		case body.Mode == "write" && prod != "write":
			writeErr(w, http.StatusConflict, "db_prod_read_only", dbProdDeferred, nil)
			return 0, nil, false
		}
	}
	var reason *string // a prod request that writes a row carries one, and only it records one
	if body.Env == "prod" && (opens || newSignin) {
		if body.Reason == nil || !prodReason(*body.Reason) {
			writeErr(w, http.StatusBadRequest, "reason_required", "say why you are opening production: 1 to 500 characters", nil)
			return 0, nil, false
		}
		reason = body.Reason
	}
	if body.Env == "prod" && body.Mode == "write" {
		if a.ownerRequired(ctx, w, tx, id) {
			return 0, nil, false
		}
		approval, err := a.prodApproval(ctx, tx)
		if err != nil {
			internal("prod_approval", err)
			return 0, nil, false
		}
		if opens && approval && !fromDashboard(ctx) {
			writeErr(w, http.StatusForbidden, "dashboard_required", "open it in Canyon's dashboard", nil)
			deny(ctx, "dashboard_required", id)
			return 0, nil, false
		}
		held, err := runs.ReadHold(ctx, tx, id)
		if err != nil {
			internal("the hold", err)
			return 0, nil, false
		}
		if held != nil {
			a.refuseHeld(ctx, w, tx, *held)
			return 0, nil, false
		}
	}
	switch {
	case body.Resource == nil && len(declared) > 1:
		writeErr(w, http.StatusConflict, "which_database", id+" declares more than one database: name one as its resource", map[string]any{"resources": declared})
		return 0, nil, false
	case resource == "":
		writeErr(w, http.StatusConflict, "no_database", id+" has no database"+namedResource(body.Resource)+" in "+body.Env+". Declare one in canyon.toml and deploy.", nil)
		return 0, nil, false
	}
	issuer, refusal, err := a.dbIssuer(ctx, tx, k)
	switch {
	case err != nil:
		internal("its database's driver", err)
		return 0, nil, false
	case refusal == "database_not_ready":
		writeErr(w, http.StatusConflict, refusal, notReadySaid(resource, id, body.Env), nil)
		return 0, nil, false
	case refusal == "driver_unsupported":
		writeErr(w, http.StatusConflict, refusal, "This database is made by your platform's adapter, which doesn't issue logins to people yet.", nil)
		return 0, nil, false
	case open != nil && opens && !body.Replace && (connect == nil || a.dbConnected(ctx, issuer, k, *open)):
		writeErr(w, http.StatusConflict, "session_open", "you have a "+open.mode+" session to "+id+" "+body.Env+" open: run it again with --replace to switch.", map[string]any{"mode": open.mode})
		return 0, nil, false
	}
	if opens {
		busy, err := dbBusy(ctx, tx, act.Subject)
		if err != nil {
			internal("the opens of the last minute", err)
			return 0, nil, false
		}
		if busy {
			writeErr(w, http.StatusTooManyRequests, "busy", "you opened ten database sessions in the last minute: wait a minute and try again", nil)
			return 0, nil, false
		}
	}
	if err := a.revokePending(ctx, tx, k, act.Subject); err != nil { // a revoke that failed runs first, and the open waits for it
		a.log.Printf("canyon-server: database session on %s: a revoke still pending: %v", k, err)
		writeErr(w, http.StatusBadGateway, "database_unreachable", "Canyon couldn't reach the database of "+id+" in "+body.Env+": try again.", nil)
		return 0, nil, false
	}
	var replaced *dbRow
	var lasted int64
	if open != nil && opens { // replace: the old one closed and its access revoked before the new mode's login
		closed, duration, ok, err := closeRow(ctx, tx, open.id, "replaced", nil)
		if err == nil && ok {
			replaced, lasted, err = &closed, duration, a.revokeDB(ctx, tx, closed)
		}
		if err != nil {
			a.log.Printf("canyon-server: database session %s: replacing it: %v", open.id, err)
			writeErr(w, http.StatusBadGateway, "database_unreachable", "Canyon couldn't reach the database of "+id+" in "+body.Env+": try again.", nil)
			return 0, nil, false
		}
	}
	sid := ""
	if opens {
		if sid, err = newDBSessionID(); err != nil {
			internal("its id", err)
			return 0, nil, false
		}
	} else {
		sid = open.id
	}
	role, mode := "", body.Mode
	if !opens {
		mode = open.mode // a login sets the role to its row's mode: a read query in a write session leaves it write
	}
	err = issuer.Issue(ctx, k, provisioners.Person{Subject: act.Subject, Email: addressOf(ctx)}, provisioners.Mode(mode), dbLoginTTL, func(ctx context.Context, l provisioners.Login) error {
		role = l.User // an open connects nowhere, a query once: the login opens nothing once this returns
		if connect == nil {
			return nil
		}
		return connect(ctx, issuer, l, sid)
	})
	if err != nil {
		a.log.Printf("canyon-server: database session on %s: %v", k, err)
		if errors.Is(err, provisioners.ErrUnchecked) { // a read query's check couldn't read what it judges: nothing ran (#2865)
			writeErr(w, http.StatusServiceUnavailable, "check_failed", dbQueryUnchecked, nil)
			return 0, nil, false
		}
		if code, said := loginRefusal(err, id, body.Env, resource); code != "" {
			writeErr(w, http.StatusConflict, code, said, nil)
		} else {
			writeErr(w, http.StatusBadGateway, "database_unreachable", "Canyon couldn't reach the database of "+id+" in "+body.Env+": try again.", nil)
		}
		return 0, nil, false
	}
	via := "connect"
	switch {
	case connect != nil: // the query route's, wherever it came from (contracts/routes.md, the trail)
		via = "query"
	case fromDashboard(ctx):
		via = "dashboard"
	}
	if err := events.LockTx(ctx, tx); err != nil {
		internal("the trail", err)
		return 0, nil, false
	}
	if replaced != nil {
		if err := closedRow(ctx, tx, *replaced, "replaced", lasted, act); err != nil {
			internal("the replaced row", err)
			return 0, nil, false
		}
	}
	status, s := http.StatusOK, open
	if opens {
		s, err = a.insertDBSession(ctx, tx, k, sid, body.Mode, role, reason, via)
		status = http.StatusCreated
	} else if newSignin {
		err = attachRow(ctx, tx, *open, signin, reason, via)
	}
	if err != nil {
		internal("its row", err)
		return 0, nil, false
	}
	if err := tx.Commit(ctx); err != nil {
		internal("commit", err)
		return 0, nil, false
	}
	return status, s, true
}

// namedResource is " named <name>" for a resource the request names, "" for none.
func namedResource(resource *string) string {
	if resource == nil {
		return ""
	}
	return " named " + *resource
}

// insertDBSession writes a new open database session, sid, for the caller, its sign-in on its trail, and its
// db session opened (or db write session opened) row; the caller took events.LockTx.
func (a *app) insertDBSession(ctx context.Context, tx pgx.Tx, k provisioners.Key, sid, mode, role string, reason *string, via string) (*dbRow, error) {
	act, signin := actorOf(ctx), signinOf(ctx)
	var s dbRow
	if err := tx.QueryRow(ctx, `INSERT INTO db_sessions (id, project_id, env, resource, mode, subject, display, email, role, reason, opened_by_session, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), LEAST(now() + make_interval(secs => $12), (SELECT expires_at FROM sessions WHERE id = $11)))
		RETURNING `+dbColumns, sid, k.Project, k.Env, k.Name, mode, act.Subject, act.Display, addressOf(ctx), role, reason, signin, dbLife(k.Env).Seconds()).Scan(s.dest()...); err != nil {
		return nil, err
	}
	if signin != "" {
		if _, err := tx.Exec(ctx, "INSERT INTO db_session_signins (db_session, session) VALUES ($1, $2)", sid, signin); err != nil {
			return nil, err
		}
	}
	action := "db session opened"
	if mode == "write" {
		action = "db write session opened"
	}
	detail := map[string]any{"db_session": sid, "env": k.Env, "resource": k.Name, "mode": mode, "role": role, "expires_at": s.read(ctx).ExpiresAt, "via_tool": via}
	if reason != nil {
		detail["reason"] = *reason
	}
	return &s, events.AuditTx(ctx, tx, act, action, k.Project, "", detail)
}

// attachRow puts a sign-in other than the ones on s's trail on it, with its db session attached row; the
// caller took events.LockTx.
func attachRow(ctx context.Context, tx pgx.Tx, s dbRow, signin string, reason *string, via string) error {
	if _, err := tx.Exec(ctx, "INSERT INTO db_session_signins (db_session, session) VALUES ($1, $2)", s.id, signin); err != nil {
		return err
	}
	detail := map[string]any{"db_session": s.id, "env": s.env, "resource": s.resource, "mode": s.mode, "via_tool": via}
	if reason != nil {
		detail["reason"] = *reason
	}
	return events.AuditTx(ctx, tx, actorOf(ctx), "db session attached", s.project, "", detail)
}

// dbDeclared is the app's postgres resources as its current package declares them in canyon.toml, sorted.
func dbDeclared(ctx context.Context, q querier, project string) ([]string, error) {
	var declared map[string]string
	err := q.QueryRow(ctx, `SELECT coalesce(resources, '{}') FROM packages WHERE project_id = $1 AND source_kind NOT IN ('repo-pr', 'check')
		ORDER BY created_at DESC, id DESC LIMIT 1`, project).Scan(&declared)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	var names []string
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if declared[name] == "postgres" {
			names = append(names, name)
		}
	}
	return names, err
}

// dbOpensAMinute is the most database sessions a person opens in a minute (design/DATABASE-SESSIONS.md §6, Caps).
const dbOpensAMinute = 10

// dbBusy is whether the person has opened dbOpensAMinute database sessions in the last minute, counted on
// tx from the rows every replica writes, under a lock of the person's own so two opens never both pass.
func dbBusy(ctx context.Context, tx pgx.Tx, subject string) (bool, error) {
	var busy bool
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "db-session opens "+subject)
	if err == nil {
		err = tx.QueryRow(ctx, "SELECT count(*) >= $2 FROM db_sessions WHERE subject = $1 AND opened_at > now() - interval '1 minute'", subject, dbOpensAMinute).Scan(&busy)
	}
	return busy, err
}

// prodReason is a prod database session's reason (contracts/routes.md, Database sessions: the body): 1 to
// 500 characters with no control, format (Unicode Cf), private-use or unassigned character, and at least
// one visible: a letter, number, punctuation or symbol that renders as more than blank.
func prodReason(s string) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= 1 && n <= 500 && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cn)
	}) && strings.ContainsFunc(s, func(r rune) bool {
		return unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) && !blank[r]
	})
}

// blank are the letters and symbols that render as nothing: fillers and the blank Braille pattern.
var blank = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// listDBSessions answers the app's open database sessions, oldest first.
func (a *app) listDBSessions(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if roleOf(ctx) == "admin" && !a.projectLive(ctx, w, id) {
		return
	}
	rows, err := a.pool.Query(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE project_id = $1 AND closed_at IS NULL ORDER BY opened_at, id", id)
	var open []dbRow
	if err == nil {
		open, err = dbRows(rows)
	}
	if err != nil {
		a.log.Printf("canyon-server: the database sessions of %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	reads := make([]dbSessionRead, len(open))
	for i, s := range open {
		reads[i] = s.read(ctx)
	}
	writeJSON(w, http.StatusOK, map[string]any{"db_sessions": reads})
}

// endDBSession ends a database session: its person's own (closed), or anyone's for the app's owners and
// admins (ended); 404 to anyone else, as one that is not theirs. With if_idle=true it is 409 in_use while
// a connection of the person's role is live, judged under the close's lock. One already closed answers 204
// and writes nothing.
func (a *app) endDBSession(w http.ResponseWriter, r *http.Request) {
	ctx, sid := r.Context(), r.PathValue("sid")
	var s dbRow
	err := a.pool.QueryRow(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE id = $1", sid).Scan(s.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such database session", nil)
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: ending database session %s: %v", sid, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	if err != nil {
		internal(err)
		return
	}
	act, reason := actorOf(ctx), "closed"
	if act.Kind != "person" || act.Subject != s.subject {
		reason = "ended"
		level := "owner"
		if roleOf(ctx) != "admin" {
			level, err = levelOf(ctx, a.pool, s.project)
		}
		if err != nil {
			internal(err)
			return
		}
		if level != "owner" {
			writeErr(w, http.StatusNotFound, "not_found", "no such database session", nil)
			return
		}
	}
	if s.closed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := a.closeDB(ctx, s, reason, act, r.URL.Query().Get("if_idle") == "true"); {
	case errors.Is(err, errInUse):
		writeErr(w, http.StatusConflict, "in_use", errInUse.Error(), nil)
		return
	case err != nil:
		internal(err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
