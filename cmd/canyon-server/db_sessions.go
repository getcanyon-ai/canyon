package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
)

// A database session's numbers (design/DATABASE-SESSIONS.md §5, §6, §17 decision 2: fixed in code).
const (
	dbLoginTTL = time.Minute         // an open's login, which connects nowhere: the most any login opens
	dbRetired  = 30 * 24 * time.Hour // a person role with no database session this long is dropped
)

// dbLife is how long a database session in env lives, at most: never past the sign-in it rides on.
func dbLife(env string) time.Duration {
	if env == "prod" {
		return time.Hour
	}
	return 8 * time.Hour
}

// dbIdle is how long a database session in env lives without activity.
func dbIdle(env string) time.Duration {
	if env == "prod" {
		return 15 * time.Minute
	}
	return 30 * time.Minute
}

// canyonDB is the actor of a check's close (contracts/routes.md, Database sessions: the trail).
var canyonDB = actor{Kind: "canyon", Subject: "db", Display: "Canyon"}

// dbRow is one db_sessions row (db/migrations/0098).
type dbRow struct {
	id, project, env, resource, mode, subject, display, email, role string
	reason, signin, relayedBy                                       *string
	openedAt, expiresAt, lastActivity                               time.Time
	closed                                                          bool
	connections, queries, rowsReturned, bytesFrom, bytesTo          int64
}

// dbColumns are a dbRow's columns, in dest's order.
const dbColumns = `id, project_id, env, resource, mode, subject, display, email, role, reason, opened_by_session, relayed_by,
	opened_at, expires_at, last_activity_at, closed_at IS NOT NULL, connections, queries, rows_returned, bytes_from_db, bytes_to_db`

func (s *dbRow) dest() []any {
	return []any{&s.id, &s.project, &s.env, &s.resource, &s.mode, &s.subject, &s.display, &s.email, &s.role, &s.reason, &s.signin, &s.relayedBy,
		&s.openedAt, &s.expiresAt, &s.lastActivity, &s.closed, &s.connections, &s.queries, &s.rowsReturned, &s.bytesFrom, &s.bytesTo}
}

// key is the database the session reaches, as the driver names it.
func (s dbRow) key() provisioners.Key {
	return provisioners.Key{Project: s.project, Env: s.env, Name: s.resource}
}

func (s dbRow) person() provisioners.Person {
	return provisioners.Person{Subject: s.subject, Email: s.email}
}

// dbRows collects rows of dbColumns.
func dbRows(rows pgx.Rows) ([]dbRow, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (dbRow, error) {
		var s dbRow
		err := row.Scan(s.dest()...)
		return s, err
	})
}

// dbSessionRead is the database session read (contracts/routes.md, Database sessions): never a credential.
type dbSessionRead struct {
	ID       string `json:"id"`
	App      string `json:"app"`
	Env      string `json:"env"`
	Resource string `json:"resource"`
	Mode     string `json:"mode"`
	Person   struct {
		Subject string `json:"subject"`
		Display string `json:"display"`
		Email   string `json:"email"`
	} `json:"person"`
	Role           string  `json:"role"`
	Reason         *string `json:"reason,omitempty"`
	OpenedAt       string  `json:"opened_at"`
	ExpiresAt      string  `json:"expires_at"`
	IdleTimeout    int     `json:"idle_timeout"`
	LastActivityAt string  `json:"last_activity_at"`
	Connections    int64   `json:"connections"`
	Queries        int64   `json:"queries"`
	BytesFromDB    int64   `json:"bytes_from_db"`
	BytesToDB      int64   `json:"bytes_to_db"`
	RelayedBy      *string `json:"relayed_by,omitempty"`
	Yours          bool    `json:"yours"`
}

// read is the row as the caller on ctx reads it.
func (s dbRow) read(ctx context.Context) dbSessionRead {
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	act := actorOf(ctx)
	out := dbSessionRead{ID: s.id, App: s.project, Env: s.env, Resource: s.resource, Mode: s.mode, Role: s.role, Reason: s.reason,
		OpenedAt: ts(s.openedAt), ExpiresAt: ts(s.expiresAt), IdleTimeout: int(dbIdle(s.env).Seconds()), LastActivityAt: ts(s.lastActivity),
		Connections: s.connections, Queries: s.queries, BytesFromDB: s.bytesFrom, BytesToDB: s.bytesTo, RelayedBy: s.relayedBy,
		Yours: act.Kind == "person" && act.Subject == s.subject}
	out.Person.Subject, out.Person.Display, out.Person.Email = s.subject, s.display, s.email
	return out
}

// newDBSessionID is dbs_ and 16 hex characters.
func newDBSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "dbs_" + hex.EncodeToString(b[:]), nil
}

// lockRole takes the person role's lock on Canyon's database for tx (design/DATABASE-SESSIONS.md §6,
// Races): an open, a close and a replace take it before they read the person's open row, and hold it until
// their login or revoke is done, so no login lands after a close and no close misses a login.
func lockRole(ctx context.Context, tx pgx.Tx, k provisioners.Key, subject string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "db-session "+k.String()+"/"+subject)
	return err
}

// openRow is the person's open database session on the key, locked for tx; nil when none is open.
func openRow(ctx context.Context, tx pgx.Tx, k provisioners.Key, subject string) (*dbRow, error) {
	var s dbRow
	err := tx.QueryRow(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE project_id = $1 AND env = $2 AND resource = $3 AND subject = $4 AND closed_at IS NULL FOR UPDATE",
		k.Project, k.Env, k.Name, subject).Scan(s.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// dbIssuer is the driver that made the key's database, configured on this host, when it issues logins to
// people; refusal is why a database session cannot open there, "" when it can: database_not_ready for a
// database with no record, one not ready, or one whose driver this host no longer runs, driver_unsupported
// for a driver that issues no logins. A database not ready still answers its issuer, which a close revokes on.
func (a *app) dbIssuer(ctx context.Context, q querier, k provisioners.Key) (is provisioners.Issuer, refusal string, err error) {
	var state, driver string
	err = q.QueryRow(ctx, "SELECT state, driver FROM resources WHERE project_id = $1 AND env = $2 AND name = $3 AND type = 'postgres'", k.Project, k.Env, k.Name).Scan(&state, &driver)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "database_not_ready", nil
	}
	if err != nil {
		return nil, "", err
	}
	d, configured := a.pipe.Drivers.Named(driver)
	if configured {
		is, _ = d.(provisioners.Issuer)
	}
	switch {
	case state != "ready" || !configured:
		return is, "database_not_ready", nil
	case is == nil:
		return nil, "driver_unsupported", nil
	}
	return is, "", nil
}

// dbProd is db_prod as the preferences read answers it, the value every open, attach and check judges:
// off while sign-in is off, whatever the row holds (contracts/routes.md, Database sessions).
func (a *app) dbProd(ctx context.Context, q querier) (string, error) {
	if a.auth != authOIDC {
		return "off", nil
	}
	var v string
	err := q.QueryRow(ctx, "SELECT db_prod FROM org_preferences").Scan(&v)
	return v, err
}

// dbSessionsOff is the refusal while an admin has database sessions off (#2835): its message names where they turn
// them on.
const dbSessionsOff = "Database sessions are off on this Canyon. An admin turns them on in Settings › Databases."

// dbSessionsOn is whether database sessions are on, as the preferences row holds it: off by default (#2835).
func (a *app) dbSessionsOn(ctx context.Context, q querier) (bool, error) {
	var v string
	err := q.QueryRow(ctx, "SELECT db_sessions FROM org_preferences").Scan(&v)
	return v == "on", err
}

// dbSwitch is the lock an open holds shared from its read of db_sessions to its commit, and turnDBSessionsOff
// takes alone before it finds the open sessions: an open that read on before the switch went off commits
// first and is found, and one after reads off (#2864).
const dbSwitch = "db-sessions switch"

// turnDBSessionsOff closes every open database session turned_off, Canyon the actor (the admin's switch is on the
// preferences' own audit row), and ends this replica's relays at once; every other replica's end at its next pass,
// which finds their rows closed. A close that fails is left to the sweep, whose check closes it turned_off too.
// It runs after the switch's commit, holding nothing else, so the opens it waits for never wait on it.
func (a *app) turnDBSessionsOff(ctx context.Context) error {
	var open []dbRow
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", dbSwitch); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE closed_at IS NULL ORDER BY opened_at, id")
		if err != nil {
			return err
		}
		open, err = dbRows(rows)
		return err
	})
	if err != nil {
		return err
	}
	var failed error
	for _, s := range open {
		failed = errors.Join(failed, a.closeDB(ctx, s, "turned_off", canyonDB, false))
	}
	if failed != nil {
		a.log.Printf("canyon-server: database sessions turned off: %v", failed) // the sweep closes what this could not
	}
	return a.checkRelays(ctx)
}

// dbCheck is the first check s fails, its close_reason, "" when it passes them all (design/DATABASE-SESSIONS.md
// §6): its lifetime first, then its opener's sign-in, their level, db_prod and prod_approval, its database,
// production's hold, and its idle window, measured from the later of its last activity and since. An active
// backend of the role is activity, and is recorded as such. A check that cannot be judged answers its
// reason with the error, which the sweep closes on once it persists.
func (a *app) dbCheck(ctx context.Context, s dbRow, since time.Time) (string, error) {
	if !time.Now().Before(s.expiresAt) {
		return "expired", nil
	}
	if on, err := a.dbSessionsOn(ctx, a.pool); err != nil || !on {
		return "turned_off", err
	}
	flow := ""
	if a.auth == authOIDC {
		if s.signin == nil {
			return "signed_out", nil
		}
		signed, _, f, ok := a.sessionFor(ctx, *s.signin)
		if !ok {
			return "signed_out", nil
		}
		flow = f
		if signed.role != "admin" {
			level, err := levelOf(withActor(context.WithValue(ctx, whoKey{}, nil), signed), a.pool, s.project) // the opener's level, never the request's log line
			if err != nil || level != "owner" && (level != "contributor" || s.env == "prod" && s.mode == "write") {
				return "access_changed", err
			}
		}
	}
	if s.env == "prod" {
		prod, err := a.dbProd(ctx, a.pool)
		approval := false
		if err == nil {
			approval, err = a.prodApproval(ctx, a.pool)
		}
		if err != nil || prod == "off" || s.mode == "write" && (prod != "write" || approval && flow != "browser") {
			return "setting_changed", err
		}
	}
	issuer, refusal, err := a.dbIssuer(ctx, a.pool, s.key())
	if err != nil || refusal != "" {
		return "database_gone", err
	}
	if s.env == "prod" && s.mode == "write" {
		h, err := runs.ReadHold(ctx, a.pool, s.project)
		if err != nil || h != nil {
			return "held", err
		}
	}
	if time.Since(later(s.lastActivity, since)) < dbIdle(s.env) {
		return "", nil
	}
	activity, err := issuer.Activity(ctx, s.key(), s.person())
	if err != nil || activity.Active == 0 {
		return "idle", err
	}
	_, err = a.pool.Exec(ctx, "UPDATE db_sessions SET last_activity_at = now() WHERE id = $1", s.id)
	return "", err
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// errInUse is an end with if_idle=true while a connection of the person's role is live.
var errInUse = errors.New("a connection of this database session is still live")

// closeDB closes s with reason, by compare-and-set under its person role's lock, then revokes the person's
// access still under it, and writes its db session closed row, by `by` (design/DATABASE-SESSIONS.md §6,
// Races): one row however many closers race, and no login lands after the close. With ifIdle it is errInUse
// while the role has a live backend, judged under the same lock. An idle close lands only while s's last
// activity is still the one judged. A revoke that fails is logged and marked revoke_pending, which the sweep
// and the person's next open run again. Nothing here takes a second connection while the lock is held.
func (a *app) closeDB(ctx context.Context, s dbRow, reason string, by actor, ifIdle bool) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRole(ctx, tx, s.key(), s.subject); err != nil {
		return err
	}
	if ifIdle {
		var activity provisioners.Activity
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { // a savepoint: a read that fails leaves the close's transaction going
			issuer, _, err := a.dbIssuer(ctx, sp, s.key())
			if err == nil && issuer != nil {
				activity, err = issuer.Activity(ctx, s.key(), s.person())
			}
			return err
		})
		if err != nil { // a live connection that cannot be seen is none: ending fails closed
			a.log.Printf("canyon-server: database session %s: %v", s.id, err)
		}
		if activity.Live > 0 {
			return errInUse
		}
	}
	var judged *time.Time
	if reason == "idle" {
		judged = &s.lastActivity
	}
	closed, duration, ok, err := closeRow(ctx, tx, s.id, reason, judged)
	if err != nil || !ok {
		return err
	}
	if err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { return a.revokeDB(ctx, sp, closed) }); err != nil { // a savepoint, as above
		a.log.Printf("canyon-server: database session %s closed %s, its access not yet revoked, the sweep revokes it again: %v", s.id, reason, err)
		if _, err := tx.Exec(ctx, "UPDATE db_sessions SET revoke_pending = true WHERE id = $1", s.id); err != nil {
			return err
		}
	}
	if err := events.LockTx(ctx, tx); err != nil {
		return err
	}
	if err := closedRow(ctx, tx, closed, reason, duration, by); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// closeRow closes the open database session id with reason, and answers it as it ended and how many
// whole seconds it was open; ok false when it was closed already, or, with judged, when its last activity
// moved past judged since.
func closeRow(ctx context.Context, tx pgx.Tx, id, reason string, judged *time.Time) (s dbRow, duration int64, ok bool, err error) {
	err = tx.QueryRow(ctx, `UPDATE db_sessions SET closed_at = now(), close_reason = $2 WHERE id = $1 AND closed_at IS NULL AND ($3::timestamptz IS NULL OR last_activity_at <= $3)
		RETURNING `+dbColumns+`, extract(epoch FROM closed_at - opened_at)::bigint`, id, reason, judged).Scan(append(s.dest(), &duration)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbRow{}, 0, false, nil
	}
	return s, duration, err == nil, err
}

// closedRow is s's db session closed audit row (contracts/routes.md, Database sessions: the trail). The
// caller took events.LockTx.
func closedRow(ctx context.Context, tx pgx.Tx, s dbRow, reason string, duration int64, by actor) error {
	return events.AuditTx(ctx, tx, by, "db session closed", s.project, "", map[string]any{
		"db_session": s.id, "person": map[string]string{"subject": s.subject, "display": s.display}, "reason": reason, "duration_s": duration,
		"connections": s.connections, "queries": s.queries, "rows_returned": s.rowsReturned, "bytes_from_db": s.bytesFrom, "bytes_to_db": s.bytesTo,
	})
}

// revokeDB ends the person's access to s's database, its driver read on q: no new login, every backend
// ended. Only a database with no record has nothing left to revoke — Delete dropped its person roles with
// it; one whose driver this replica does not run is an error, for the revoke to be run again.
func (a *app) revokeDB(ctx context.Context, q querier, s dbRow) error {
	issuer, refusal, err := a.dbIssuer(ctx, q, s.key())
	if err != nil {
		return err
	}
	if issuer != nil {
		return issuer.Revoke(ctx, s.key(), s.person())
	}
	var recorded bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM resources WHERE project_id = $1 AND env = $2 AND name = $3)", s.project, s.env, s.resource).Scan(&recorded); err != nil || !recorded {
		return err
	}
	return fmt.Errorf("%s: %s, and this replica runs no driver that issues there to revoke on", s.key(), refusal)
}

// revokePending runs, on tx, which holds the person role's lock, every revoke of theirs on the key that
// failed, and clears each only once it is done: a backend that did SET ROLE to the old mode's group
// outlives a new grant, so none is ever skipped.
func (a *app) revokePending(ctx context.Context, tx pgx.Tx, k provisioners.Key, subject string) error {
	rows, err := tx.Query(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE project_id = $1 AND env = $2 AND resource = $3 AND subject = $4 AND revoke_pending ORDER BY id", k.Project, k.Env, k.Name, subject)
	if err != nil {
		return err
	}
	pending, err := dbRows(rows)
	for _, s := range pending {
		if err == nil {
			err = a.revokeDB(ctx, tx, s)
		}
		if err == nil {
			_, err = tx.Exec(ctx, "UPDATE db_sessions SET revoke_pending = false WHERE id = $1", s.id)
		}
	}
	return err
}

// retryRevokes runs again each revoke that failed, under its person role's lock, until it is done.
func (a *app) retryRevokes(ctx context.Context) error {
	rows, err := a.pool.Query(ctx, "SELECT DISTINCT project_id, env, resource, subject FROM db_sessions WHERE revoke_pending ORDER BY 1, 2, 3, 4")
	if err != nil {
		return err
	}
	type person struct {
		key     provisioners.Key
		subject string
	}
	pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (person, error) {
		var p person
		err := row.Scan(&p.key.Project, &p.key.Env, &p.key.Name, &p.subject)
		return p, err
	})
	if err != nil {
		return err
	}
	var failed error
	for _, p := range pending {
		if err := a.underRole(ctx, p.key, p.subject, func(tx pgx.Tx) error { return a.revokePending(ctx, tx, p.key, p.subject) }); err != nil {
			failed = errors.Join(failed, fmt.Errorf("%s/%s: its access still not revoked: %w", p.key, p.subject, err))
		}
	}
	return failed
}

// underRole runs fn in a transaction holding the person role's lock, and commits it.
func (a *app) underRole(ctx context.Context, k provisioners.Key, subject string, fn func(pgx.Tx) error) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockRole(ctx, tx, k, subject); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The answers, 503 check_failed, to a query and a connect whose checks could not be judged: nothing ran or
// connected, and nothing closed (#2865).
const (
	dbQueryUnchecked   = "Canyon couldn't check this database session just now, so nothing ran. The session is still open: try again."
	dbConnectUnchecked = "Canyon couldn't check this database session just now, so it didn't connect. The session is still open: try again."
)

// dbCheckTries is how many sweeps in a row a database session's checks may fail to judge it before it
// is closed with the reason of the check that failed: a moment's error closes nothing, a lasting one fails closed.
const dbCheckTries = 4

// checkDBSessions runs the checks over every open database session, and closes each that fails one with
// its reason, Canyon the actor. failing counts, by id, the sweeps in a row whose checks could not judge it:
// a sweep that judges it starts the count again, and a closed one leaves it.
func (a *app) checkDBSessions(ctx context.Context, since time.Time, failing map[string]int) error {
	rows, err := a.pool.Query(ctx, "SELECT "+dbColumns+" FROM db_sessions WHERE closed_at IS NULL ORDER BY opened_at, id")
	if err != nil {
		return err
	}
	open, err := dbRows(rows)
	if err != nil {
		return err
	}
	var failed error
	still := map[string]bool{}
	for _, s := range open {
		still[s.id] = true
		reason, err := a.dbCheck(ctx, s, since)
		if err != nil {
			failing[s.id]++
			failed = errors.Join(failed, fmt.Errorf("database session %s: %s: %w", s.id, reason, err))
			if failing[s.id] < dbCheckTries {
				continue
			}
		}
		delete(failing, s.id) // judged, or failed to be long enough: a run of errors starts again
		if reason != "" {
			failed = errors.Join(failed, a.closeDB(ctx, s, reason, canyonDB, false))
		}
	}
	for id := range failing { // closed since, here or on another replica
		if !still[id] {
			delete(failing, id)
		}
	}
	return failed
}

// forgetRetired drops the person roles of the people with no database session on a database, open or
// closed, in the last 30 days (design/DATABASE-SESSIONS.md §4, Retired roles): on each database a
// database session reached, each person the driver finds a role of, judged under their role's lock, so
// one who opens meanwhile keeps it.
func (a *app) forgetRetired(ctx context.Context) error {
	rows, err := a.pool.Query(ctx, "SELECT DISTINCT project_id, env, resource FROM db_sessions ORDER BY 1, 2, 3")
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (provisioners.Key, error) {
		var k provisioners.Key
		err := row.Scan(&k.Project, &k.Env, &k.Name)
		return k, err
	})
	if err != nil {
		return err
	}
	var failed error
	for _, k := range keys {
		issuer, _, err := a.dbIssuer(ctx, a.pool, k)
		var subjects []string
		if err == nil && issuer != nil {
			subjects, err = issuer.People(ctx, k)
		}
		failed = errors.Join(failed, err)
		for _, subject := range subjects {
			failed = errors.Join(failed, a.underRole(ctx, k, subject, func(tx pgx.Tx) error {
				var recent bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM db_sessions WHERE project_id = $1 AND env = $2 AND resource = $3 AND subject = $4
					AND (closed_at IS NULL OR closed_at > now() - make_interval(secs => $5)))`, k.Project, k.Env, k.Name, subject, dbRetired.Seconds()).Scan(&recent); err != nil || recent {
					return err
				}
				return issuer.Forget(ctx, k, provisioners.Person{Subject: subject})
			}))
		}
	}
	return failed
}

// sweepLease names the session-level advisory lock on Canyon's database that one replica holds as its
// lease to run the checks (design/DATABASE-SESSIONS.md §6).
const sweepLease = "canyon db-session sweep"

// sweepDBSessions is a replica's share of the lifecycle, every interval until ctx is done: while it holds
// the sweep's lease, the checks over every open database session, measuring idleness from when it took the
// lease, the revokes that failed, and hourly the retired roles' drop.
func (a *app) sweepDBSessions(ctx context.Context, logger *log.Logger, every time.Duration) {
	var lease *pgx.Conn
	var since, forgot time.Time
	failing := map[string]int{}
	defer func() {
		if lease != nil {
			_ = lease.Close(context.WithoutCancel(ctx))
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if lease != nil && lease.Ping(ctx) != nil { // the connection, and so the lease, is gone
			_ = lease.Close(ctx)
			lease = nil
		}
		if lease == nil {
			lease, since = a.takeLease(ctx, logger), time.Now()
		}
		if lease != nil {
			if err := a.checkDBSessions(ctx, since, failing); err != nil {
				logger.Printf("canyon-server: the database sessions' checks: %v", err)
			}
			if err := a.retryRevokes(ctx); err != nil {
				logger.Printf("canyon-server: the database sessions' revokes: %v", err)
			}
			if time.Since(forgot) >= time.Hour {
				if err := a.forgetRetired(ctx); err != nil {
					logger.Printf("canyon-server: the retired person roles: %v", err)
				} else {
					forgot = time.Now()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// takeLease is a connection of its own holding the sweep's lease, nil while another replica holds it.
func (a *app) takeLease(ctx context.Context, logger *log.Logger) *pgx.Conn {
	c, err := pgx.ConnectConfig(ctx, a.pool.Config().ConnConfig.Copy())
	if err != nil {
		logger.Printf("canyon-server: the database sessions' sweep: %v", err)
		return nil
	}
	var held bool
	if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", sweepLease).Scan(&held); err != nil || !held {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil
	}
	return c
}
