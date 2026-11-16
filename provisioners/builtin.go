// Package provisioners creates and destroys the resources apps declare. A driver answers two
// idempotent calls for a key: Ensure (the resource exists; here are its outputs) and Delete (it
// is gone, also when it already was). Builtin serves postgres from one shared server.
package provisioners

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/executor"
)

// Key names one resource: the stable identity every driver call carries.
type Key struct{ Project, Env, Name string }

func (k Key) String() string { return k.Project + "/" + k.Env + "/" + k.Name }

// Builtin serves postgres as a database and a login of its own per key on one shared server,
// the demo's app-postgres. It connects as a role that may create roles and databases. A key's
// login is kept out of every other key's database, not out of the server's own databases that
// keep PUBLIC connect (such as postgres), where it can connect but create nothing.
type Builtin struct {
	pool    *pgxpool.Pool
	issuing *pgxpool.Pool // Issue's own, as each holds a connection through its connect: opens never stall Ensure, Delete or Revoke
	admin   *url.URL      // the handed-out URL is this one with the key's login, password and database
	ca      string        // the server's CA bundle on this host; set, the handed-out URL verifies against it at executor.DatabaseCAPath
}

var _ Issuer = (*Builtin)(nil) // the server finds a driver that issues by this interface, and refuses one that does not

// NewBuiltin connects to the shared server at adminURL, never in plaintext once it has TLS (the issuing pool, a
// copy, neither), and says so when it has none. With ca — the file the host names as the server's CA
// (CANYON_APP_DATABASE_CA_FILE) — each app's URL, and each person's login, verifies the server against it.
func NewBuiltin(ctx context.Context, adminURL, ca string) (*Builtin, error) {
	u, err := url.Parse(adminURL)
	if err != nil {
		return nil, fmt.Errorf("builtin postgres: parse the server url: %w", err)
	}
	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		return nil, err
	}
	if err := db.NoPlaintextFallback(&cfg.ConnConfig.Config); err != nil {
		return nil, fmt.Errorf("builtin postgres: the server url: %w", err)
	}
	if cfg.ConnConfig.TLSConfig == nil {
		log.Print("provisioners: the apps database's admin connection is unencrypted: its url says sslmode=disable")
	}
	cfg.ConnConfig.ConnectTimeout = cmp.Or(cfg.ConnConfig.ConnectTimeout, 10*time.Second) // a server that does not answer fails the ensure, unless the URL names its own bound
	// The admin reaches into databases apps own (groups), whose owner may ALTER DATABASE … SET
	// search_path: a startup parameter overrides that, so every name resolves in the catalog first and
	// never in a schema an app can create in.
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog, pg_temp"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	icfg := cfg.Copy()
	icfg.MaxConns = sessionCap
	issuing, err := pgxpool.NewWithConfig(ctx, icfg)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Builtin{pool: pool, issuing: issuing, admin: u, ca: ca}, nil
}

// Close releases the connections to the shared server.
func (b *Builtin) Close() {
	b.pool.Close()
	b.issuing.Close()
}

func (b *Builtin) Name() string { return "builtin" }

// Ensure makes the key's login and database exist, issues the login a fresh password and answers
// ready with DATABASE_URL. Every step checks before it creates, so a crash between steps is
// repaired by the next call, and a lost URL costs a password, never data. It is never pending or
// failed: an error is transient.
func (b *Builtin) Ensure(ctx context.Context, key Key, typ string) (Result, error) {
	if typ != "postgres" {
		return Result{}, fmt.Errorf("builtin driver serves postgres, not %q", typ)
	}
	password := rand.Text()
	verifier, err := scramVerifier(password)
	if err != nil {
		return Result{}, err
	}
	name := ident(key)
	quoted := pgx.Identifier{name}.Sanitize()
	err = b.locked(ctx, key, func(conn *pgxpool.Conn) error {
		var roleExists, dbExists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`, name).Scan(&roleExists, &dbExists); err != nil {
			return err
		}
		var steps []string
		if !roleExists {
			steps = append(steps, `CREATE ROLE `+quoted+` LOGIN`)
		}
		// On a managed Postgres (RDS, Cloud SQL) the admin is no superuser: it may own a database for
		// the login, revoke on it, drop it and end its sessions only as an inheriting member of the
		// login. Granted on every call: an install before this granted nothing.
		steps = append(steps, `GRANT `+quoted+` TO CURRENT_USER`)
		if !dbExists {
			steps = append(steps, `CREATE DATABASE `+quoted+` OWNER `+quoted)
		}
		// Every database grants CONNECT to PUBLIC by default: without the revoke, any app's login
		// opens any other app's database. Run on every call, so a crash after the create is repaired.
		steps = append(steps,
			`REVOKE CONNECT ON DATABASE `+quoted+` FROM PUBLIC`,
			`GRANT CONNECT ON DATABASE `+quoted+` TO `+quoted,
			// DDL takes no parameters, so the statement text reaches a server logging DDL. It carries
			// the verifier, never the password; base64 needs no escaping.
			`ALTER ROLE `+quoted+` WITH LOGIN PASSWORD '`+verifier+`'`,
		)
		if err := run(ctx, conn, steps); err != nil {
			return err
		}
		return b.groups(ctx, conn, key)
	})
	if err != nil {
		return Result{}, fmt.Errorf("builtin postgres: ensure %s: %w", key, err)
	}
	u := *b.admin
	u.User = url.UserPassword(name, password)
	u.Path = "/" + name
	if b.ca != "" {
		u.RawQuery = verifying(u.RawQuery)
	}
	return Result{State: Ready, Outputs: map[string]string{"DATABASE_URL": u.String()}}, nil
}

// verifying is an admin URL's query for an app that verifies the server against the bundle at
// executor.DatabaseCAPath: sslmode=verify-full and that sslrootcert in place of the admin's own two,
// the rest of its query kept as written (contracts/provisioner.md).
func verifying(query string) string {
	var kept []string
	for _, pair := range strings.Split(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(key); pair != "" && (err != nil || key != "sslmode" && key != "sslrootcert") {
			kept = append(kept, pair)
		}
	}
	return strings.Join(append(kept, "sslmode=verify-full", "sslrootcert="+executor.DatabaseCAPath), "&")
}

// Delete drops the key's database, its login, its group roles and its person roles. All already gone
// is success. Every session on the database, and every session of those roles, is ended first; the
// app's containers are stopped before a delete. A person role is found by the comment Issue wrote on
// it, never by its name alone.
func (b *Builtin) Delete(ctx context.Context, key Key) (Result, error) {
	// An Issue goes on changing its person role once the key's lock is free (#2364), and Postgres takes
	// no lock for role DDL: a step of Delete that writes a catalog row the Issue wrote meanwhile fails,
	// having changed nothing. Delete then runs again, reading the roles and sessions afresh, three times
	// at most (#2380).
	var err error
	for tries := 1; ; tries++ {
		if err = b.delete(ctx, key); tries == 3 || !concurrentlyWritten(err) {
			break
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("builtin postgres: delete %s: %w", key, err)
	}
	return Result{State: Gone}, nil
}

// concurrentlyWritten is Postgres's internal error for a catalog row another transaction updated or
// deleted first: XX000, with one of two messages, which elog never translates.
func concurrentlyWritten(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "XX000" &&
		(pgErr.Message == "tuple concurrently updated" || pgErr.Message == "tuple concurrently deleted")
}

// delete is one pass of Delete, under the key's lock.
func (b *Builtin) delete(ctx context.Context, key Key) error {
	name := ident(key)
	quoted := pgx.Identifier{name}.Sanitize()
	return b.locked(ctx, key, func(conn *pgxpool.Conn) error {
		// a lock waits 2 s at most, as Revoke's steps: a session holding a row the DDL takes ends instead
		if _, err := conn.Exec(ctx, `SET lock_timeout = '2s'`); err != nil {
			return err
		}
		defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`) }()
		var roleExists, dbExists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`, name).Scan(&roleExists, &dbExists); err != nil {
			return err
		}
		var steps []string
		if roleExists { // a database an Ensure before the grant left to the login: the admin takes the membership now
			steps = append(steps, `GRANT `+quoted+` TO CURRENT_USER`)
		}
		if dbExists { // no session starts on the database from here, a superuser's neither
			steps = append(steps, `ALTER DATABASE `+quoted+` WITH ALLOW_CONNECTIONS false`)
		}
		found, err := people(ctx, conn, key)
		if err != nil {
			return err
		}
		names := append([]string{name, name + "_read", name + "_write"}, slices.Sorted(maps.Keys(found))...)
		roles := make([]string, len(names))
		for i, n := range names {
			roles[i] = pgx.Identifier{n}.Sanitize()
		}
		rows, _ := conn.Query(ctx, `SELECT rolname FROM pg_catalog.pg_roles WHERE rolname = ANY($1) AND rolcanlogin`, names)
		logins, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, l := range logins { // nor a session of the key's roles, on any database
			steps = append(steps, `ALTER ROLE `+pgx.Identifier{l}.Sanitize()+` NOLOGIN`)
		}
		// The sessions end below and the DROP takes no FORCE: FORCE refuses an admin without
		// pg_signal_backend while an autovacuum worker is in the database (42501, #2364), where a plain
		// DROP ends the worker itself. A session is a backend with a user, which pg_stat_activity shows the
		// admin of any role's, and an autovacuum worker has none. A session the admin may not end fails
		// the delete, nothing dropped.
		sessions := `FROM pg_catalog.pg_stat_activity WHERE usesysid IS NOT NULL AND (datname = $1 OR usename = ANY($2))`
		terminate := func() error {
			_, err := conn.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid, 5000) `+sessions, name, names)
			return err
		}
		// A person's own uncommitted ALTER ROLE holds their role's row, so the NOLOGIN times out: the
		// key's sessions end and the steps run again, three times at most.
		var pgErr *pgconn.PgError
		for tries := 1; ; tries++ {
			if err = run(ctx, conn, steps); tries == 3 || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				break
			}
			if err = terminate(); err != nil {
				break
			}
		}
		if err != nil {
			return err
		}
		if err := waitStarting(ctx, conn); err != nil {
			return err
		}
		if err := terminate(); err != nil {
			return err
		}
		var left int // a statement of its own, and so a fresh read of pg_stat_activity
		if err := conn.QueryRow(ctx, `SELECT count(*) `+sessions, name, names).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return fmt.Errorf("%d session(s) still open after their termination", left)
		}
		return run(ctx, conn, []string{`DROP DATABASE IF EXISTS ` + quoted, `DROP ROLE IF EXISTS ` + strings.Join(roles, ", ")})
	})
}

const (
	sessionCap    = 10               // connections per database session: the person role's CONNECTION LIMIT (design/DATABASE-SESSIONS.md §6)
	maxTempFiles  = 1 << 20          // kB, 1 GB: the most temp_file_limit a person role runs under
	maxTTL        = 60 * time.Second // the longest a login opens, by the database's clock (design/DATABASE-SESSIONS.md §4)
	revokeTimeout = 30 * time.Second // Revoke's own bound: it runs on, though the request that asked is gone
)

// limits are a person role's, set at every issue (design/DATABASE-SESSIONS.md §6). The timeouts are
// defaults a person may SET over in their own session, against accidents; only a superuser can raise
// temp_file_limit.
func limits(env string) [][2]string {
	idle, statement := "10min", "15min"
	if env == "prod" {
		idle, statement = "60s", "5min"
	}
	return [][2]string{{"statement_timeout", statement}, {"lock_timeout", "3s"}, {"idle_in_transaction_session_timeout", idle}, {"temp_file_limit", "1GB"}}
}

// Issue logs person into the key's database as their own role, for one connection. Under the role's
// lock it gives the database its groups if it lacks them (a kept resource never calls Ensure), and
// checks on every call that both are as Canyon made them and that PUBLIC cannot create in public; makes
// the role, sets its limits, then its group to mode, and refuses a role left holding anything else;
// lets it log in with a fresh password until the database's now() + ttl (at most a minute), hands
// connect that login, and sets the role NOLOGIN once connect returns: a password a person sets on their
// own role opens nothing. A limit the database refuses is ErrLimitsUnsupported, a public schema PUBLIC
// may create in ErrUnsafeSchema; connect is not called then. An error once connect made its connection
// means the caller closes it.
func (b *Builtin) Issue(ctx context.Context, key Key, person Person, mode Mode, ttl time.Duration, connect func(context.Context, Login) error) error {
	if (mode != Read && mode != Write) || person.Subject == "" || ttl <= 0 || ttl > maxTTL {
		return fmt.Errorf("builtin postgres: issue %s: mode %q, subject %q, ttl %s", key, mode, person.Subject, ttl)
	}
	name, role := ident(key), personRole(key, person.Subject)
	quoted := pgx.Identifier{role}.Sanitize()
	password := rand.Text()
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	conn, err := b.issuing.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	err = hold(ctx, conn, role, func() (err error) {
		// under the key's lock to the role's making: a Delete runs before the issue makes anything, or
		// after its role is made and Delete finds it (#2364)
		if err := hold(ctx, conn, key.String(), func() error {
			var exists bool
			if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("the database of %s: %w", key, ErrNoDatabase)
			}
			found, err := groupsAsMade(ctx, conn, name)
			if err != nil {
				return err
			}
			if len(found) < 2 {
				err = b.groups(ctx, conn, key)
			} else {
				err = b.inDatabase(ctx, name, func(tx pgx.Tx) error { return publicClosed(ctx, tx) })
			}
			if err != nil {
				return err
			}
			return enrol(ctx, conn, role, person)
		}); err != nil {
			return err
		}
		inEffect, err := setLimits(ctx, conn, name, role, key.Env)
		if err != nil {
			return err
		}
		grant, other := groupNames(name)
		if mode == Write {
			grant, other = other, grant
		}
		if err := run(ctx, conn, []string{`GRANT ` + grant + ` TO ` + quoted, `REVOKE ` + other + ` FROM ` + quoted}); err != nil {
			return err
		}
		if err := onlyIn(ctx, conn, role, name+"_"+string(mode)); err != nil {
			return err
		}
		var until time.Time // the database's clock, never this server's
		if err := conn.QueryRow(ctx, `SELECT now() + make_interval(secs => $1)`, ttl.Seconds()).Scan(&until); err != nil {
			return err
		}
		defer func() { // before the LOGIN, which may land though its answer never does; on a context of its own, so a cancelled request still ends it
			short, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, nologin := conn.Exec(short, `ALTER ROLE `+quoted+` NOLOGIN`); nologin != nil { // the role logs in until VALID UNTIL, at most ttl
				log.Printf("provisioners: %s on %s still logs in until %s: NOLOGIN failed: %v", role, key, until.Format(time.RFC3339), nologin)
				err = errors.Join(err, fmt.Errorf("set the role NOLOGIN: %w", nologin))
			}
		}()
		// The verifier, never the password, reaches a server logging DDL, as Ensure's.
		if _, err := conn.Exec(ctx, `ALTER ROLE `+quoted+` LOGIN CONNECTION LIMIT `+strconv.Itoa(sessionCap)+` PASSWORD '`+verifier+`' VALID UNTIL '`+until.UTC().Format("2006-01-02 15:04:05.999999-07")+`'`); err != nil {
			return err
		}
		cfg := b.pool.Config().ConnConfig
		login := Login{Host: cfg.Host, Port: int(cfg.Port), Database: name, User: role, Secret: NewSecret(password), TLS: loginTLS(b.admin, b.ca), Expires: until}
		if inEffect {
			if err := b.tempFilesInEffect(ctx, conn, login); err != nil {
				return err
			}
		}
		bounded, cancel := context.WithTimeout(ctx, ConnectTimeout)
		defer cancel()
		return connect(bounded, login)
	})
	if err != nil {
		return fmt.Errorf("builtin postgres: issue %s: %w", key, err)
	}
	return nil
}

// enrol makes one person's role on the key's database, or finds it as Canyon made it: no attribute, and
// granted to nobody but the admin, which is its member so that a non-superuser may end its backends (on
// 14 a role may grant itself onward, to another app's login). Its comment names the person, for the
// sweep that finds a retired role and for a pgaudit line, and Postgres quotes it. One transaction, so
// no role is left without the comment Delete finds it by.
func enrol(ctx context.Context, conn *pgxpool.Conn, role string, person Person) error {
	quoted := pgx.Identifier{role}.Sanitize()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var exists, wrong bool
		var members []string
		if err := tx.QueryRow(ctx, `SELECT count(*) > 0, coalesce(bool_or(r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls), false),
				ARRAY(SELECT DISTINCT pg_catalog.pg_get_userbyid(m.member)::text FROM pg_catalog.pg_auth_members m
					WHERE m.roleid = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1) AND m.member <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user) ORDER BY 1)
			FROM pg_catalog.pg_roles r WHERE r.rolname = $1`, role).Scan(&exists, &wrong, &members); err != nil {
			return err
		}
		switch {
		case len(members) > 0:
			return &NotAsMade{Found: "your role " + role + " has an attribute, or is granted to " + strings.Join(members, ", ") + ", which Canyon never gave it"}
		case wrong:
			return &NotAsMade{Found: "your role " + role + " has an attribute Canyon never gave it"}
		}
		var steps []string
		if !exists {
			steps = append(steps, `CREATE ROLE `+quoted+` NOLOGIN CONNECTION LIMIT `+strconv.Itoa(sessionCap))
		}
		var comment string
		if err := tx.QueryRow(ctx, `SELECT pg_catalog.format('COMMENT ON ROLE %I IS %L', $1::text, $2::text)`, role, "canyon person "+person.Email+" "+person.Subject).Scan(&comment); err != nil {
			return err
		}
		return run(ctx, tx, append(steps, `GRANT `+quoted+` TO CURRENT_USER`, comment))
	})
}

// setLimits sets the role's limits, after clearing what the role set for itself in the key's database
// (ALTER ROLE CURRENT_USER IN DATABASE … SET), which would outrank them. It answers whether
// temp_file_limit is left to the one in effect: the admin, neither superuser nor granted SET on it, is
// refused it.
func setLimits(ctx context.Context, conn *pgxpool.Conn, name, role, env string) (inEffect bool, err error) {
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := conn.Exec(ctx, `ALTER ROLE `+quoted+` IN DATABASE `+pgx.Identifier{name}.Sanitize()+` RESET ALL`); err != nil {
		return false, err
	}
	for _, limit := range limits(env) {
		_, err := conn.Exec(ctx, `ALTER ROLE `+quoted+` SET `+limit[0]+` = '`+limit[1]+`'`)
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == "42501" && limit[0] == "temp_file_limit":
			inEffect = true
		case errors.As(err, &pgErr) && pgErr.Code == "42501":
			return false, fmt.Errorf("%w: %s", ErrLimitsUnsupported, pgErr.Message)
		case err != nil:
			return false, err
		}
	}
	return inEffect, nil
}

// onlyIn refuses unless the role is a member of group alone, without the admin option, by every
// grantor: from Postgres 16 a REVOKE ends only the admin's own grant, and another's would stay.
func onlyIn(ctx context.Context, conn *pgxpool.Conn, role, group string) error {
	rows, _ := conn.Query(ctx, `SELECT DISTINCT pg_catalog.pg_get_userbyid(m.roleid)::text FROM pg_catalog.pg_auth_members m
		WHERE m.member = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
			AND (m.admin_option OR m.roleid <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $2)) ORDER BY 1`, role, group)
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(others) > 0 { // besides group
		return &NotAsMade{Found: "your role " + role + " is also a member of " + strings.Join(others, ", ") + ", by a grant Canyon can't revoke"}
	}
	return nil
}

// tempFilesInEffect refuses a login whose role runs under no temp_file_limit, or one over 1 GB, read
// in a session of its own as the role, under the login's TLS: the limit an operator set for the whole
// server where the admin may not set it on the role (RDS has no superuser to grant it). conn waits for
// that session's backend to end, so it holds none of the role's connections when connect dials.
func (b *Builtin) tempFilesInEffect(ctx context.Context, conn *pgxpool.Conn, login Login) error {
	cfg, err := LoginConfig(login)
	if err != nil {
		return err
	}
	cfg.ConnectTimeout = b.pool.Config().ConnConfig.ConnectTimeout
	cfg.RuntimeParams["search_path"] = "pg_catalog, pg_temp" // the app owns the database and may set its search_path
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	pid := c.PgConn().PID()
	var kb int64
	var unit, shown string
	err = c.QueryRow(ctx, `SELECT setting::bigint, unit, pg_catalog.current_setting(name) FROM pg_catalog.pg_settings WHERE name = 'temp_file_limit'`).Scan(&kb, &unit, &shown)
	_ = c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var open bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid = $1)`, pid).Scan(&open); err != nil {
			return err
		}
		if !open {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the session reading temp_file_limit as %s still open 5s on", login.User)
		}
	}
	if unit != "kB" || kb < 0 || kb > maxTempFiles {
		admin := pgx.Identifier{b.pool.Config().ConnConfig.User}.Sanitize()
		return fmt.Errorf("%w: temp_file_limit is %s for a person's role, and the admin may not set it: have a superuser run GRANT SET ON PARAMETER temp_file_limit TO %s (Postgres 15 or later), or set temp_file_limit to 1GB or less for the whole server (on RDS, in its parameter group)", ErrLimitsUnsupported, shown, admin)
	}
	return nil
}

// Revoke ends person's access to the key's database: their groups revoked, every backend of their
// role terminated, the role NOLOGIN, and its grants to any role but the admin revoked; then the backends
// verified gone, and the role verified a member of nothing and granted to nobody but the admin, by any
// grantor. Each step runs in a transaction of its own under a short lock_timeout, and every step
// runs though one before it failed: a person whose own uncommitted ALTER ROLE holds the role's row
// blocks the NOLOGIN until their backend ends, so a pass that fails is run again. It runs on a context of
// its own, bounded, so a cancelled request still ends the access. A survivor is an error, for the
// caller to log, and its next sweep revokes again. A person never issued a login has nothing to revoke.
func (b *Builtin) Revoke(ctx context.Context, key Key, person Person) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	name, role := ident(key), personRole(key, person.Subject)
	quoted := pgx.Identifier{role}.Sanitize()
	read, write := groupNames(name)
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	step := func(sql string, args ...any) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`)
			if err == nil {
				_, err = tx.Exec(ctx, sql, args...)
			}
			return err
		})
	}
	err = hold(ctx, conn, role, func() error {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil || !exists {
			return err
		}
		// ties are the role's memberships both ways, by every grantor, but the admin's in it
		ties := func() ([][3]string, error) {
			rows, _ := conn.Query(ctx, `SELECT pg_catalog.pg_get_userbyid(m.roleid)::text, pg_catalog.pg_get_userbyid(m.member)::text, pg_catalog.pg_get_userbyid(m.grantor)::text
				FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles p ON p.rolname = $1
				WHERE m.member = p.oid OR (m.roleid = p.oid AND m.member <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)) ORDER BY 1, 2, 3`, role)
			return pgx.CollectRows(rows, func(row pgx.CollectableRow) ([3]string, error) {
				var t [3]string
				err := row.Scan(&t[0], &t[1], &t[2])
				return t, err
			})
		}
		q := func(name string) string { return pgx.Identifier{name}.Sanitize() }
		var failed error
		for range 3 {
			failed = errors.Join(
				step(`REVOKE `+read+`, `+write+` FROM `+quoted),
				// each backend has 5 s to end
				step(`SELECT pg_catalog.pg_terminate_backend(pid, 5000) FROM pg_catalog.pg_stat_activity WHERE usename = $1`, role),
				step(`ALTER ROLE `+quoted+` NOLOGIN`))
			// What is left: on 14 a grant onward, the person's own, which a REVOKE ends; from 16 another
			// grantor's, which only a REVOKE in its name ends, or the verify below fails the revoke.
			left, err := ties()
			failed = errors.Join(failed, err)
			for _, t := range left {
				failed = errors.Join(failed, step(`REVOKE `+q(t[0])+` FROM `+q(t[1])))
			}
			left, err = ties()
			failed = errors.Join(failed, err)
			for _, t := range left {
				failed = errors.Join(failed, step(`REVOKE `+q(t[0])+` FROM `+q(t[1])+` GRANTED BY `+q(t[2])))
			}
			if failed == nil {
				break
			}
		}
		held, err := ties()
		if err == nil && len(held) > 0 {
			err = fmt.Errorf("%s still in a role, or granted to one, after its revoke: %q (role, member, grantor)", role, held)
		}
		failed = errors.Join(failed, err)
		// a statement of its own, and so a fresh read of pg_stat_activity: the database's word on what is left
		var left int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename = $1`, role).Scan(&left); err != nil {
			return errors.Join(failed, err)
		}
		if left > 0 {
			failed = errors.Join(failed, fmt.Errorf("%d backend(s) of %s still open after their termination", left, role))
		}
		return failed
	})
	if err != nil {
		return fmt.Errorf("builtin postgres: revoke %s: %w", key, err)
	}
	return nil
}

// Activity counts the backends of person's role on the key: every one, and those running a statement now.
// The admin is a member of the role, so pg_stat_activity shows it their state.
func (b *Builtin) Activity(ctx context.Context, key Key, person Person) (Activity, error) {
	var a Activity
	if err := b.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 'active') FROM pg_catalog.pg_stat_activity WHERE usename = $1`,
		personRole(key, person.Subject)).Scan(&a.Live, &a.Active); err != nil {
		return Activity{}, fmt.Errorf("builtin postgres: activity %s: %w", key, err)
	}
	return a, nil
}

// Cancel cancels the statement backend pid runs, when pid is a backend of person's role on the key: one of
// anyone else's is left alone, and no secret key is needed (design/DATABASE-SESSIONS.md §3).
func (b *Builtin) Cancel(ctx context.Context, key Key, person Person, pid int) error {
	if _, err := b.pool.Exec(ctx, `SELECT pg_catalog.pg_cancel_backend(pid) FROM pg_catalog.pg_stat_activity WHERE pid = $1 AND usename = $2`,
		pid, personRole(key, person.Subject)); err != nil {
		return fmt.Errorf("builtin postgres: cancel %s: %w", key, err)
	}
	return nil
}

// Terminate ends backend pid, when it is a backend of person's role on the key, and verifies it gone: a
// connection whose own sign-in failed a check (design/DATABASE-SESSIONS.md §6). On a context of its own,
// as Revoke's, so a cancelled request still ends it.
func (b *Builtin) Terminate(ctx context.Context, key Key, person Person, pid int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	role := personRole(key, person.Subject)
	var left bool
	_, err := b.pool.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid, 5000) FROM pg_catalog.pg_stat_activity WHERE pid = $1 AND usename = $2`, pid, role)
	if err == nil { // a statement of its own, and so a fresh read of pg_stat_activity
		err = b.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid = $1 AND usename = $2)`, pid, role).Scan(&left)
	}
	if err == nil && left {
		err = fmt.Errorf("backend %d of %s still open after its termination", pid, role)
	}
	if err != nil {
		return fmt.Errorf("builtin postgres: terminate %s: %w", key, err)
	}
	return nil
}

// People are the subjects of the key's person roles, sorted, each found by the comment Issue wrote on it.
func (b *Builtin) People(ctx context.Context, key Key) ([]string, error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	found, err := people(ctx, conn, key)
	if err != nil {
		return nil, fmt.Errorf("builtin postgres: people %s: %w", key, err)
	}
	return slices.Sorted(maps.Values(found)), nil
}

// Forget drops person's role on the key, retired (design/DATABASE-SESSIONS.md §4): found by the comment
// Issue wrote, never by its name alone, and dropped under its lock, as Issue and Revoke take it, its
// backends ended first. A person with no role there has nothing to forget.
func (b *Builtin) Forget(ctx context.Context, key Key, person Person) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	role := personRole(key, person.Subject)
	err = hold(ctx, conn, role, func() error {
		found, err := people(ctx, conn, key)
		if err != nil || found[role] != person.Subject {
			return err
		}
		if _, err := conn.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid, 5000) FROM pg_catalog.pg_stat_activity WHERE usename = $1`, role); err != nil {
			return err
		}
		return run(ctx, conn, []string{`DROP ROLE IF EXISTS ` + pgx.Identifier{role}.Sanitize()})
	})
	if err != nil {
		return fmt.Errorf("builtin postgres: forget %s: %w", key, err)
	}
	return nil
}

// loginTLS is how a person's login meets the server, never weaker than the app's own URL: verify-full
// against the host's CA file where it names one, the admin URL's sslmode and sslrootcert otherwise.
func loginTLS(admin *url.URL, ca string) TLSMode {
	if ca != "" {
		return TLSMode{SSLMode: "verify-full", RootCert: ca}
	}
	q := admin.Query()
	return TLSMode{SSLMode: q.Get("sslmode"), RootCert: q.Get("sslrootcert")}
}

// LoginConfig is how a login connects: its address, database, role and secret, under its TLS, never falling back
// to plaintext (db.NoPlaintextFallback, #2819): every connect as a person's login, the driver's own and a db query's
// (#2118) included, goes through it. The secret is in the config alone, for the one connect, and never printed.
func LoginConfig(l Login) (*pgx.ConnConfig, error) {
	q := url.Values{}
	if l.TLS.SSLMode != "" {
		q.Set("sslmode", l.TLS.SSLMode)
	}
	if l.TLS.RootCert != "" {
		q.Set("sslrootcert", l.TLS.RootCert)
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(l.User, l.Secret.Reveal()), Host: net.JoinHostPort(l.Host, strconv.Itoa(l.Port)), Path: "/" + l.Database, RawQuery: q.Encode()}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	if err := db.NoPlaintextFallback(&cfg.Config); err != nil {
		return nil, fmt.Errorf("builtin postgres: a login: %w", err)
	}
	return cfg, nil
}

// groups gives the key's database its read and write group roles and closes CREATE on its public
// schema to PUBLIC (design/DATABASE-SESSIONS.md §4). Every step is idempotent: Ensure runs it on
// every call, and as a kept resource never calls Ensure, the first Issue for one runs it too. conn
// holds the key's lock; the grants inside the database take a connection of their own, as the pool
// reaches only the admin's. That database is the app's, and so are its settings: the connection
// keeps the pool's pinned search_path, names every catalog by its schema, and refuses a session
// whose role the database's settings changed, so nothing the app planted runs as the admin.
func (b *Builtin) groups(ctx context.Context, conn *pgxpool.Conn, key Key) error {
	name := ident(key)
	quoted := pgx.Identifier{name}.Sanitize()
	read, write := groupNames(name)
	found, err := groupsAsMade(ctx, conn, name)
	if err != nil {
		return err
	}
	var steps []string
	if !found[name+"_read"] {
		steps = append(steps, `CREATE ROLE `+read+` NOLOGIN`)
	}
	if !found[name+"_write"] {
		steps = append(steps, `CREATE ROLE `+write+` NOLOGIN`)
	}
	steps = append(steps, `GRANT `+read+` TO `+write, `GRANT CONNECT ON DATABASE `+quoted+` TO `+read+`, `+write,
		// Nor TEMPORARY through PUBLIC: a temp table is not bounded by temp_file_limit, and shadows an
		// app table a function names unqualified. The app login, the owner, keeps it.
		`REVOKE TEMPORARY ON DATABASE `+quoted+` FROM PUBLIC`)
	if err := run(ctx, conn, steps); err != nil {
		return err
	}
	return b.inDatabase(ctx, name, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname = 'public' OR nspowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1) ORDER BY nspname`, name)
		schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if slices.Contains(schemas, "public") {
			// On 14 public is the bootstrap superuser's and the app creates in it as PUBLIC, so the app's
			// login is granted CREATE before PUBLIC loses it: its migrations still run. On 15+ PUBLIC
			// never had it. On 14 under a non-superuser admin both are warnings that change nothing, so
			// the outcome is read back, and a public PUBLIC can still create in fails the call.
			if err := run(ctx, tx, []string{`GRANT CREATE ON SCHEMA public TO ` + quoted, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`}); err != nil {
				return err
			}
			if err := publicClosed(ctx, tx); err != nil {
				return err
			}
		}
		steps := []string{`SET LOCAL ROLE ` + quoted} // the grants are the app login's, which owns what they name
		if len(schemas) > 0 {
			for i, s := range schemas {
				schemas[i] = pgx.Identifier{s}.Sanitize()
			}
			in := strings.Join(schemas, ", ")
			steps = append(steps,
				`GRANT USAGE ON SCHEMA `+in+` TO `+read,
				`GRANT SELECT ON ALL TABLES IN SCHEMA `+in+` TO `+read,
				`GRANT SELECT ON ALL SEQUENCES IN SCHEMA `+in+` TO `+read,               // never USAGE, which allows nextval()
				`GRANT INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+in+` TO `+write, // rows, never the schema
				`GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+in+` TO `+write,
			)
		}
		// The same on what the app login makes later, so a migration's new table is readable at once.
		later := `ALTER DEFAULT PRIVILEGES FOR ROLE ` + quoted + ` GRANT `
		steps = append(steps,
			later+`USAGE ON SCHEMAS TO `+read,
			later+`SELECT ON TABLES TO `+read,
			later+`SELECT ON SEQUENCES TO `+read,
			later+`INSERT, UPDATE, DELETE ON TABLES TO `+write,
			later+`USAGE ON SEQUENCES TO `+write,
		)
		return run(ctx, tx, steps)
	})
}

// groupsAsMade answers which of the key's group roles exist, refusing one not as Canyon makes it: no
// login, no attribute, and no membership but write's in read, without the admin option. Issue runs it
// on every call: on 14 and 15 the app's login may grant itself to its read group in a migration, and
// every reader would then hold the app's rights.
func groupsAsMade(ctx context.Context, conn *pgxpool.Conn, name string) (map[string]bool, error) {
	rows, _ := conn.Query(ctx, `SELECT r.rolname, r.rolcanlogin OR r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls
			OR EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member = r.oid
				AND (m.admin_option OR r.rolname <> $2 OR m.roleid <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)))
		FROM pg_catalog.pg_roles r WHERE r.rolname IN ($1, $2)`, name+"_read", name+"_write")
	found := map[string]bool{}
	var role string
	var wrong bool
	if _, err := pgx.ForEachRow(rows, []any{&role, &wrong}, func() error {
		if wrong {
			return &NotAsMade{Found: "the group role " + role + " has a login, an attribute or a membership Canyon never gave it"}
		}
		found[role] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// inDatabase runs fn in a transaction on a connection of the admin's own to the app's database name,
// as the pool reaches only the admin's. The database, and so its settings, are the app's: the
// connection keeps the pool's pinned search_path, and is refused when the database's settings make its
// role other than the admin (ALTER DATABASE … SET role, which the owner may run), so nothing the app
// planted runs as the admin.
func (b *Builtin) inDatabase(ctx context.Context, name string, fn func(pgx.Tx) error) error {
	cfg := b.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(context.WithoutCancel(ctx)) }()
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var admin bool
		if err := tx.QueryRow(ctx, `SELECT current_user = session_user`).Scan(&admin); err != nil {
			return err
		}
		if !admin {
			return &NotAsMade{Found: "the database sets the role of every session that connects to it (ALTER DATABASE … SET role)"}
		}
		return fn(tx)
	})
}

// publicClosed is ErrUnsafeSchema while PUBLIC can create in the database's public schema: a role
// able to plant a function the app later calls would act with the app's rights.
func publicClosed(ctx context.Context, tx pgx.Tx) error {
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = 'public' AND pg_catalog.has_schema_privilege('public', oid, 'CREATE'))`).Scan(&open); err != nil {
		return err
	}
	if open {
		return ErrUnsafeSchema
	}
	return nil
}

// readWrites is every right in the app's database that lets role ($1) change data, by any path Postgres grants one:
// its own, PUBLIC's, or a role's it inherits (contracts/provisioner.md, ReadOnly). A write on a table, a view or a
// foreign table, a column's included; on 17 or later MAINTAIN on a materialized view, whose REFRESH rewrites it;
// UPDATE or USAGE on a sequence (nextval); CREATE on a schema or on the database; EXECUTE on a SECURITY DEFINER
// function, which runs as its owner; UPDATE on an existing large object, by 18's has_largeobject_privilege or, before
// it, the object's owner and its acl, and every one while lo_compat_privileges is on. The system schemas hold nothing a
// role may change. version is server_version_num. Sorted, so a refusal names the same rights first every time.
func readWrites(version int) string {
	maintain := ""
	if version >= 170000 {
		maintain = `
		UNION ALL
		SELECT 1, n.nspname::text, c.relname::text, 'MAINTAIN on ' || format('%I.%I', n.nspname, c.relname)
			FROM pg_catalog.pg_class c JOIN n ON n.oid = c.relnamespace
			WHERE c.relkind = 'm' AND pg_catalog.has_table_privilege($1, c.oid, 'MAINTAIN')`
	}
	largeObject := `pg_catalog.pg_has_role($1, l.lomowner, 'USAGE') OR EXISTS (SELECT 1 FROM pg_catalog.aclexplode(l.lomacl) a
				WHERE a.privilege_type = 'UPDATE' AND (a.grantee = 0 OR pg_catalog.pg_has_role($1, a.grantee, 'USAGE')))`
	if version >= 180000 {
		largeObject = `pg_catalog.has_largeobject_privilege($1, l.oid, 'UPDATE')`
	}
	return `WITH n AS (SELECT oid, nspname FROM pg_catalog.pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema') AND nspname NOT LIKE 'pg\_toast%' AND nspname NOT LIKE 'pg\_temp\_%')
	SELECT what FROM (
		SELECT 1 AS k, n.nspname::text AS nsp, c.relname::text AS obj, p.priv || ' on ' || format('%I.%I', n.nspname, c.relname) AS what
			FROM pg_catalog.pg_class c JOIN n ON n.oid = c.relnamespace, unnest(ARRAY['INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'TRIGGER']) p(priv)
			WHERE c.relkind IN ('r', 'p', 'v', 'f') AND CASE WHEN p.priv IN ('INSERT', 'UPDATE') THEN pg_catalog.has_any_column_privilege($1, c.oid, p.priv)
				ELSE pg_catalog.has_table_privilege($1, c.oid, p.priv) END` + maintain + `
		UNION ALL
		SELECT 2, n.nspname::text, c.relname::text, p.priv || ' on sequence ' || format('%I.%I', n.nspname, c.relname)
			FROM pg_catalog.pg_class c JOIN n ON n.oid = c.relnamespace, unnest(ARRAY['UPDATE', 'USAGE']) p(priv)
			WHERE c.relkind = 'S' AND pg_catalog.has_sequence_privilege($1, c.oid, p.priv)
		UNION ALL
		SELECT 3, n.nspname::text, '', 'CREATE on schema ' || format('%I', n.nspname) FROM n WHERE pg_catalog.has_schema_privilege($1, n.oid, 'CREATE')
		UNION ALL
		SELECT 4, '', '', 'CREATE on database ' || format('%I', current_database()) WHERE pg_catalog.has_database_privilege($1, current_database(), 'CREATE')
		UNION ALL
		SELECT 5, n.nspname::text, f.proname::text, 'EXECUTE on SECURITY DEFINER function ' || format('%I.%I', n.nspname, f.proname) || '(' || pg_catalog.pg_get_function_identity_arguments(f.oid) || ')'
			FROM pg_catalog.pg_proc f JOIN n ON n.oid = f.pronamespace WHERE f.prosecdef AND pg_catalog.has_function_privilege($1, f.oid, 'EXECUTE')
		UNION ALL
		SELECT 6, '', lpad(l.oid::text, 10, '0'), 'UPDATE on large object ' || l.oid
			FROM pg_catalog.pg_largeobject_metadata l WHERE current_setting('lo_compat_privileges')::bool OR ` + largeObject + `
	) found ORDER BY k, nsp COLLATE "C", obj COLLATE "C", what COLLATE "C"`
}

// maxNamed is how many rights a refusal names; the rest it counts.
const maxNamed = 5

// ReadOnly is nil while login's role can change no data in its database, else *ReadOnlyUnsafe naming what it can
// (contracts/provisioner.md, ReadOnly; #2826): a relayed connection's client controls its transactions, so only the
// role's rights enforce read there, PUBLIC's and its group's included. Read as the admin, in the login's database, by
// the rights the server's version has; any error is the connection's refusal.
func (b *Builtin) ReadOnly(ctx context.Context, login Login) error {
	if login.User == "" || login.Database == "" {
		return fmt.Errorf("builtin postgres: read only: a login with no role or database")
	}
	return b.inDatabase(ctx, login.Database, func(tx pgx.Tx) error {
		var version int
		if err := tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
			return fmt.Errorf("%w: %v", ErrUnchecked, err)
		}
		rows, err := tx.Query(ctx, readWrites(version), login.User)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnchecked, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnchecked, err)
		}
		if len(found) == 0 {
			return nil
		}
		named := found[:min(len(found), maxNamed)]
		return &ReadOnlyUnsafe{Found: named, More: len(found) - len(named)}
	})
}

// NoSecondConnection refuses a read login whose role can open a second connection from inside its database, which
// neither its grants nor a READ ONLY transaction hold: EXECUTE on any function of the dblink extension, its own,
// PUBLIC's or an inherited role's, or on any SECURITY DEFINER function, which may wrap one, or USAGE on any
// foreign-data wrapper, postgres_fdw's among them
// (contracts/provisioner.md, NoSecondConnection). Read as the admin, in the login's database; what it finds is
// *ReadOnlyUnsafe, at most maxNamed named in a fixed order; an error reading it, in a database that answered, is
// ErrUnchecked.
func (b *Builtin) NoSecondConnection(ctx context.Context, login Login) error {
	if login.User == "" || login.Database == "" {
		return fmt.Errorf("builtin postgres: second connection: a login with no role or database")
	}
	return b.inDatabase(ctx, login.Database, func(tx pgx.Tx) error {
		// a role the catalog doesn't hold judges nothing: with nothing installed no row would ask about it (fail closed)
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`, login.User).Scan(&known); err != nil || !known {
			return fmt.Errorf("%w: role %s (%v)", ErrUnchecked, login.User, cmp.Or(err, errors.New("not in the catalog")))
		}
		rows, err := tx.Query(ctx, secondConnections, login.User)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnchecked, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnchecked, err)
		}
		if len(found) == 0 {
			return nil
		}
		named := found[:min(len(found), maxNamed)]
		return &ReadOnlyUnsafe{Found: named, More: len(found) - len(named)}
	})
}

// secondConnections is every way role $1 can open a second connection from inside the database: the dblink
// extension's functions it may EXECUTE, each as regprocedure names it; every SECURITY DEFINER function outside the
// system schemas it may EXECUTE, whoever owns it, named as ReadOnly names one: it runs as its owner, and what that owner
// may reach, dblink or another definer, isn't followed (fail closed); then the foreign-data wrappers it may use.
const secondConnections = `WITH dblink AS (SELECT p.oid FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid = p.oid AND d.deptype = 'e'
		JOIN pg_catalog.pg_extension e ON d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.refobjid = e.oid
		WHERE e.extname = 'dblink')
SELECT how FROM (
	SELECT 1 AS k, 'EXECUTE on dblink function ' || d.oid::pg_catalog.regprocedure::text AS how
		FROM dblink d WHERE pg_catalog.has_function_privilege($1, d.oid, 'EXECUTE')
	UNION ALL
	SELECT 2, 'EXECUTE on SECURITY DEFINER function ' || pg_catalog.format('%I.%I', n.nspname, f.proname) || '(' || pg_catalog.pg_get_function_identity_arguments(f.oid) || ')'
		FROM pg_catalog.pg_proc f JOIN pg_catalog.pg_namespace n ON n.oid = f.pronamespace
		WHERE f.prosecdef AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'
			AND pg_catalog.has_function_privilege($1, f.oid, 'EXECUTE')
	UNION ALL
	SELECT 3, 'USAGE on foreign-data wrapper ' || pg_catalog.quote_ident(w.fdwname)
		FROM pg_catalog.pg_foreign_data_wrapper w
		WHERE pg_catalog.has_foreign_data_wrapper_privilege($1, w.oid, 'USAGE')
) found ORDER BY k, how COLLATE "C"`

// groupNames are the key's read and write group roles, quoted: its login's name and _read, _write.
func groupNames(name string) (read, write string) {
	return pgx.Identifier{name + "_read"}.Sanitize(), pgx.Identifier{name + "_write"}.Sanitize()
}

// waitStarting waits out, 5 s at most in all, every backend still starting: a login past its LOGIN check
// before a NOLOGIN that has not yet published its user, so the next read of pg_stat_activity misses it.
// Until it publishes, it shows only as the holder of its startup transaction's virtualxid lock, with no
// pg_stat_activity row on 14 to 17 and a row with no user and no database on 18; it publishes before that
// transaction commits, so each virtualxid read here is waited until it is released. The autovacuum and
// logical replication launchers publish their row before any transaction on 14 to 17, so they never
// match there. On 18 their row looks like a starting login's to an admin that cannot read backend_type
// or client_addr (pg_read_all_stats), so one of their transactions can be waited out: milliseconds of a
// pg_database scan, past 5 s only behind a lock on pg_database, and then the delete fails closed.
func waitStarting(ctx context.Context, conn *pgxpool.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, _ := conn.Query(ctx, `SELECT l.virtualxid FROM pg_catalog.pg_locks l WHERE l.locktype = 'virtualxid' AND l.mode = 'ExclusiveLock' AND l.granted AND l.pid IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity a WHERE a.pid = l.pid
			AND (a.usesysid IS NOT NULL OR a.datid IS NOT NULL OR pg_catalog.current_setting('server_version_num')::int < 180000))`)
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for err == nil && len(held) > 0 {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(50 * time.Millisecond):
			rows, _ = conn.Query(ctx, `SELECT virtualxid FROM pg_catalog.pg_locks WHERE locktype = 'virtualxid' AND mode = 'ExclusiveLock' AND granted AND virtualxid = ANY($1)`, held)
			held, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
	}
	if err != nil {
		return fmt.Errorf("backend(s) still starting (%v): %w", held, err)
	}
	return nil
}

// run executes steps in order, stopping at the first that fails.
func run(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, steps []string) error {
	for _, sql := range steps {
		if _, err := db.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
}

// locked runs fn on one connection holding the key's advisory lock, so two calls for one key
// never interleave. CREATE DATABASE cannot run in a transaction, so this is a session lock.
func (b *Builtin) locked(ctx context.Context, key Key, fn func(*pgxpool.Conn) error) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return hold(ctx, conn, key.String(), func() error { return fn(conn) })
}

// hold runs fn holding the advisory lock named lock, a key or a person role, on conn. Postgres
// hashes the name to the lock's bigint: no conversion here to overflow.
func hold(ctx context.Context, conn *pgxpool.Conn, lock string, fn func() error) error {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lock); err != nil {
		return err
	}
	defer func() { // must run even if ctx is done: a lock left on a pooled connection blocks the next call
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lock)
	}()
	return fn()
}

// ident is the key's login and database name. A key can pass Postgres's 63-byte identifier limit,
// which truncates silently, so the name is a hash: c_ and 24 hex characters, 26 bytes.
func ident(key Key) string {
	sum := sha256.Sum256([]byte(key.String()))
	return "c_" + hex.EncodeToString(sum[:12])
}

// personRole is one person's role on the key's database: p_ and 24 hex characters of a hash of the
// key and the identity provider's sub, as ident is of the key alone.
func personRole(key Key, subject string) string {
	sum := sha256.Sum256([]byte(key.String() + "/" + subject))
	return "p_" + hex.EncodeToString(sum[:12])
}

// people are the key's person roles, each with its person's subject, found by the comment Issue wrote on
// it: the comment ends on the sub the role's name was made from, and an email may hold a space.
func people(ctx context.Context, conn *pgxpool.Conn, key Key) (map[string]string, error) {
	rows, _ := conn.Query(ctx, `SELECT r.rolname, d.description FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_shdescription d
		ON d.objoid = r.oid AND d.classoid = 'pg_catalog.pg_authid'::pg_catalog.regclass WHERE d.description LIKE 'canyon person %'`)
	found := map[string]string{}
	var role, comment string
	_, err := pgx.ForEachRow(rows, []any{&role, &comment}, func() error {
		for i := range comment {
			if comment[i] == ' ' && personRole(key, comment[i+1:]) == role {
				found[role] = comment[i+1:]
				break
			}
		}
		return nil
	})
	return found, err
}

// scramVerifier is the SCRAM-SHA-256 verifier Postgres stores for password (RFC 5802, 7677), in
// its own format. Postgres keeps a verifier handed to it as-is, so the password never leaves Canyon.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const iterations = 4096
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac("Server Key"))), nil
}
