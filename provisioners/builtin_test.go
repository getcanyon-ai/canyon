package provisioners

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/gob"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// driver is a Builtin on the test server, standing in for app-postgres, connected as a login with
// CREATEDB and CREATEROLE but not SUPERUSER: what a managed Postgres (RDS, Cloud SQL) hands Canyon,
// where having made a database owned by another role gives no right to drop it or grant on it.
// Every key a test ensures is deleted after it: the roles and databases are the server's, not a
// schema's.
func driver(t *testing.T, keys ...Key) (*Builtin, context.Context) {
	t.Helper()
	ctx := context.Background()
	return driverAs(t, ctx, adminURL(t, ctx), "", keys...), ctx
}

// driverAs is driver's Builtin as the admin at rawURL, with the host's CA file ca.
func driverAs(t *testing.T, ctx context.Context, rawURL, ca string, keys ...Key) *Builtin {
	t.Helper()
	b, err := NewBuiltin(ctx, rawURL, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if _, err := b.Delete(ctx, k); err != nil {
				t.Error(err)
			}
		}
		b.Close()
	})
	return b
}

const testAdmin, ungrantedAdmin = "canyon_test_admin", "canyon_test_admin_ungranted"

// adminURL is the URL the driver runs as: limitedAdminURL's, granted temp_file_limit, or on Postgres
// 14 the superuser's, as the driver needs 15 or later under a non-superuser admin
// (design/DATABASE-SESSIONS.md §4).
func adminURL(t *testing.T, ctx context.Context) string {
	t.Helper()
	if version(t, ctx) < 150000 {
		return pgtest.Exclusive(t)
	}
	return limitedAdminURL(t, ctx, testAdmin)
}

// version is the test server's server_version_num.
func version(t *testing.T, ctx context.Context) int {
	t.Helper()
	var v int
	if err := server(t).QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// limitedAdminURL makes admin on the test server, with a fresh password, and answers the server's
// URL as it. One that exists is kept: on Postgres 16 its right to grant the logins it made goes with
// it, and a failed run's leftovers would be no one's to drop. testAdmin, on 15 or later, may set
// temp_file_limit, as an operator grants it (design/DATABASE-SESSIONS.md §6); any other admin may not.
func limitedAdminURL(t *testing.T, ctx context.Context, admin string) string {
	t.Helper()
	u, err := url.Parse(pgtest.Exclusive(t))
	if err != nil {
		t.Fatal(err)
	}
	password, sup := rand.Text(), server(t)
	var pgErr *pgconn.PgError
	if _, err := sup.Exec(ctx, `CREATE ROLE `+admin+` LOGIN CREATEDB CREATEROLE`); err != nil && (!errors.As(err, &pgErr) || pgErr.Code != "42710") {
		t.Fatal(err)
	}
	if _, err := sup.Exec(ctx, `ALTER ROLE `+admin+` PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	if admin == testAdmin && version(t, ctx) >= 150000 {
		if _, err := sup.Exec(ctx, `GRANT SET ON PARAMETER temp_file_limit TO `+admin); err != nil {
			t.Fatal(err)
		}
	}
	u.User = url.UserPassword(admin, password)
	return u.String()
}

// server is the test server as its superuser, for what a test sets up or reads around the driver.
func server(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func ensure(t *testing.T, ctx context.Context, b *Builtin, k Key) string {
	t.Helper()
	out, err := b.Ensure(ctx, k, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	return out.Outputs["DATABASE_URL"]
}

func count(t *testing.T, ctx context.Context, b *Builtin, sql string, k Key) int {
	t.Helper()
	var n int
	if err := b.pool.QueryRow(ctx, sql, ident(k)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// connect opens rawURL with path as its database. It is closed when the test ends, and its session
// waited out, before the key's Delete.
func connect(t *testing.T, ctx context.Context, b *Builtin, rawURL, path string) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + path
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close(ctx)
		gone(t, ctx, b, `pid = $1`, c.PgConn().PID())
	})
	return c
}

// as connects to the key's database as member's login.
func as(t *testing.T, ctx context.Context, b *Builtin, k Key, appURL, group string) *pgx.Conn {
	t.Helper()
	return connect(t, ctx, b, member(t, ctx, b, k, appURL, group), ident(k))
}

// member makes a fresh login in the key's read or write group, standing in for a person's role
// (design/DATABASE-SESSIONS.md §4), and answers its URL. The login is dropped once its sessions are.
func member(t *testing.T, ctx context.Context, b *Builtin, k Key, appURL, group string) string {
	t.Helper()
	role, password := ident(k)+"_as_"+group, rand.Text()
	for _, sql := range []string{`DROP ROLE IF EXISTS ` + role, `CREATE ROLE ` + role + ` LOGIN PASSWORD '` + password + `' IN ROLE ` + ident(k) + `_` + group} {
		if _, err := b.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := b.pool.Exec(ctx, `DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(appURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

// gone waits for every session pg_stat_activity names by where to end. A closed client's backend can
// outlive the close by a moment, and a non-superuser admin may not end a session of a role it is no
// member of (42501 "permission denied to terminate process"), so a cleanup that drops waits first.
func gone(t *testing.T, ctx context.Context, b *Builtin, where string, arg any) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var open int
		if err := b.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE `+where, arg).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d session(s) where %s still open 5s on", open, where)
		}
	}
}

// refused fails the test unless err is Postgres's insufficient_privilege.
func refused(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: want insufficient_privilege, got %v", what, err)
	}
}

// A read role reads the rows and the sequences, and cannot advance a sequence: nextval() needs
// USAGE, which is write's (design/DATABASE-SESSIONS.md §4). Nor can it write a row.
func TestAReadGroupCannotNextval(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (id serial PRIMARY KEY, note text); INSERT INTO claims (note) VALUES ('first')`); err != nil {
		t.Fatal(err)
	}
	ensure(t, ctx, b, k) // what a redeploy runs: the grants on what exists
	c := as(t, ctx, b, k, app, "read")
	var note string
	var last int
	if err := c.QueryRow(ctx, `SELECT note, (SELECT last_value FROM claims_id_seq) FROM claims`).Scan(&note, &last); err != nil || note != "first" || last != 1 {
		t.Fatalf("read the row and the sequence: %q %d %v", note, last, err)
	}
	_, err := c.Exec(ctx, `SELECT nextval('claims_id_seq')`)
	refused(t, "nextval as read", err)
	_, err = c.Exec(ctx, `INSERT INTO claims (id, note) VALUES (2, 'second')`)
	refused(t, "insert as read", err)
}

// What the app's login makes after Ensure — a migration's table, a schema of its own — is readable by
// read and writable by write at once, with no grant at connection time (design/DATABASE-SESSIONS.md §4).
func TestATableMadeLaterIsReadable(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (id serial PRIMARY KEY, note text); INSERT INTO claims (note) VALUES ('later'); CREATE SCHEMA billing; CREATE TABLE billing.invoices (id serial PRIMARY KEY); INSERT INTO billing.invoices DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	var note string
	var invoice int
	if err := as(t, ctx, b, k, app, "read").QueryRow(ctx, `SELECT (SELECT note FROM claims), (SELECT id FROM billing.invoices)`).Scan(&note, &invoice); err != nil || note != "later" || invoice != 1 {
		t.Fatalf("read a later table: %q %d %v", note, invoice, err)
	}
	if _, err := as(t, ctx, b, k, app, "write").Exec(ctx, `INSERT INTO claims (note) VALUES ('later still'); INSERT INTO billing.invoices DEFAULT VALUES; UPDATE claims SET note = 'changed'; DELETE FROM billing.invoices WHERE id = 1`); err != nil {
		t.Fatalf("write the rows of a later table: %v", err)
	}
}

// Write is rows, never the schema: no table of its own, no change to the app's, no function the app
// might later call (design/DATABASE-SESSIONS.md §4).
func TestAWriteGroupCannotChangeTheSchema(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (id int)`); err != nil {
		t.Fatal(err)
	}
	w := as(t, ctx, b, k, app, "write")
	for _, sql := range []string{
		`CREATE TABLE public.planted (id int)`,
		`ALTER TABLE claims ADD COLUMN planted int`,
		`CREATE FUNCTION public.planted() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`CREATE SCHEMA planted`,
	} {
		_, err := w.Exec(ctx, sql)
		refused(t, sql, err)
	}
}

// The groups hold rows and nothing more (design/DATABASE-SESSIONS.md §4): neither logs in or holds a
// role attribute, neither is a member of the app login or holds the admin option on any role, write's
// one membership is read without it, neither may create in the database, in public or a temp table,
// or owns anything there, and every default privilege is the app login's own.
func TestTheGroupsHoldNoRoleNoCreateAndNoOwnership(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	name := ident(k)
	sup := connect(t, ctx, b, pgtest.URL(t), name) // the superuser in the app's database, which reads its catalog whole
	want := map[string][]string{name + "_read": nil, name + "_write": {name + "_read"}}
	for group, memberOf := range want {
		var attrs, app, admin, create, owns bool
		var member []string
		if err := sup.QueryRow(ctx, `SELECT rolcanlogin OR rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls,
				pg_has_role($1::name, $2::name, 'MEMBER'),
				EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = r.oid AND m.admin_option),
				has_schema_privilege($1::name, 'public', 'CREATE') OR has_database_privilege($1::name, current_database()::text, 'CREATE, TEMPORARY'),
				EXISTS (SELECT 1 FROM pg_class WHERE relowner = r.oid) OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspowner = r.oid),
				ARRAY(SELECT pg_get_userbyid(m.roleid)::text FROM pg_auth_members m WHERE m.member = r.oid ORDER BY 1)
			FROM pg_roles r WHERE rolname = $1`, group, name).Scan(&attrs, &app, &admin, &create, &owns, &member); err != nil {
			t.Fatalf("%s: %v", group, err)
		}
		if attrs || app || admin || create || owns || !slices.Equal(member, memberOf) {
			t.Errorf("%s: attributes %v, member of the app login %v, admin option %v, create %v, owns %v, member of %q (want %q)", group, attrs, app, admin, create, owns, member, memberOf)
		}
	}
	var others int
	if err := sup.QueryRow(ctx, `SELECT count(*) FROM pg_default_acl WHERE defaclrole <> (SELECT oid FROM pg_roles WHERE rolname = $1)`, name).Scan(&others); err != nil || others != 0 {
		t.Fatalf("%d default privileges not the app login's (%v)", others, err)
	}
}

// A kept resource never calls Ensure, so a person's first login runs the groups step itself: an app
// made before group roles existed, with a table in public and a schema of its own, gets them, and its
// rows are readable at once.
func TestAnIssueGivesAnEarlierAppsDatabaseItsGroups(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	quoted, password := pgx.Identifier{ident(k)}.Sanitize(), rand.Text()
	for _, sql := range []string{ // what Ensure made before this
		`CREATE ROLE ` + quoted + ` LOGIN PASSWORD '` + password + `'`,
		`GRANT ` + quoted + ` TO CURRENT_USER`,
		`CREATE DATABASE ` + quoted + ` OWNER ` + quoted,
		`REVOKE CONNECT ON DATABASE ` + quoted + ` FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE ` + quoted + ` TO ` + quoted,
	} {
		if _, err := b.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	u := *b.admin
	u.User = url.UserPassword(ident(k), password)
	app := u.String()
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (note text); INSERT INTO claims VALUES ('kept'); CREATE SCHEMA billing; CREATE TABLE billing.invoices (id serial PRIMARY KEY); INSERT INTO billing.invoices DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	var note string
	var invoice int
	if err := open(t, ctx, b, k, dana, Read).QueryRow(ctx, `SELECT (SELECT note FROM claims), (SELECT id FROM billing.invoices)`).Scan(&note, &invoice); err != nil || note != "kept" || invoice != 1 {
		t.Fatalf("read an earlier app's tables: %q %d %v", note, invoice, err)
	}
	w := as(t, ctx, b, k, app, "write")
	if _, err := w.Exec(ctx, `INSERT INTO billing.invoices DEFAULT VALUES`); err != nil {
		t.Fatalf("write an earlier app's table: %v", err)
	}
	_, err := w.Exec(ctx, `CREATE TEMP TABLE planted (id int)`)
	refused(t, "a temp table as write", err)
}

// PUBLIC cannot create in public after Ensure, so no role can plant a function the app later calls
// (design/DATABASE-SESSIONS.md §4) — on 14, where PUBLIC holds it by default, and on 15+ where an
// operator granted it back — while the app's own migrations still create and alter there.
func TestEnsureClosesPublicToCreateAndTheAppStillMigrates(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	sup := connect(t, ctx, b, pgtest.URL(t), ident(k))
	if _, err := sup.Exec(ctx, `GRANT CREATE ON SCHEMA public TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	app := ensure(t, ctx, b, k)
	var open bool
	if err := sup.QueryRow(ctx, `SELECT has_schema_privilege('public', 'public', 'CREATE')`).Scan(&open); err != nil || open {
		t.Fatalf("PUBLIC may create in public after Ensure: %v %v", open, err)
	}
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (id int); ALTER TABLE claims ADD COLUMN note text; CREATE TEMP TABLE scratch (id int)`); err != nil {
		t.Fatalf("the app's migration after Ensure: %v", err)
	}
}

// On a host that names the apps database's CA, the URL an app is handed verifies the server against
// the bundle its container holds, in place of the admin URL's own sslmode, and keeps the rest of the
// admin URL's query (contracts/provisioner.md, canyonv2-jx83). The admin's own connection is as its
// URL says: the test server speaks no TLS.
func TestOnAHostNamingTheDatabasesCATheURLVerifiesTheServer(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	ctx := context.Background()
	u, err := url.Parse(adminURL(t, ctx))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Set("application_name", "canyon")
	u.RawQuery = q.Encode()
	b, err := NewBuiltin(ctx, u.String(), "/run/canyon/apps-db-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := b.Delete(ctx, k); err != nil {
			t.Error(err)
		}
		b.Close()
	})
	got, err := url.Parse(ensure(t, ctx, b, k))
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {"/etc/canyon-db/ca.pem"}, "application_name": {"canyon"}}
	if !reflect.DeepEqual(got.Query(), want) || !strings.HasSuffix(got.RawQuery, "sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem") {
		t.Fatalf("the app's query is %q, want the admin's rest with sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem", got.RawQuery)
	}
}

// The app's query is the admin's as written — never re-encoded, which would turn an options value's
// %20 into a + that libpq does not read as a space — without its sslmode and sslrootcert, then the
// verifying two.
func TestVerifyingKeepsTheAdminsQueryAsWritten(t *testing.T) {
	for admin, want := range map[string]string{
		"":                "sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem",
		"sslmode=require": "sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem",
		"sslmode=verify-full&sslrootcert=%2Fvar%2Frun%2Fca.pem&options=-c%20statement_timeout%3D5s&application_name=canyon": "options=-c%20statement_timeout%3D5s&application_name=canyon&sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem",
	} {
		if got := verifying(admin); got != want {
			t.Errorf("verifying(%q) = %q, want %q", admin, got, want)
		}
	}
}

// A second Ensure is the same database with its rows, never a duplicate; the fresh password
// replaces the old one.
func TestEnsureTwiceAnswersTheSameDatabase(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if b.pool.Config().ConnConfig.ConnectTimeout == 0 { // a server that does not answer fails the ensure instead of hanging it
		t.Fatal("the pool does not bound a connect")
	}
	first := ensure(t, ctx, b, k)
	c, err := pgx.Connect(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `CREATE TABLE claims (id int); INSERT INTO claims VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(ctx)

	second := ensure(t, ctx, b, k)
	c, err = pgx.Connect(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var id int
	if err := c.QueryRow(ctx, `SELECT id FROM claims`).Scan(&id); err != nil || id != 7 {
		t.Fatalf("the second URL reads the first one's rows: %d %v", id, err)
	}
	if n := count(t, ctx, b, `SELECT count(*) FROM pg_database WHERE datname = $1`, k); n != 1 {
		t.Fatalf("%d databases for one key", n)
	}
	if old, err := pgx.Connect(ctx, first); err == nil {
		_ = old.Close(ctx)
		t.Fatal("the first password still opens the database after a second Ensure")
	}
}

// A crash after the login was created and before the database was, with the URL never stored:
// the next Ensure finishes the job and answers a URL that works.
func TestEnsureAfterALostSecretAnswersAWorkingURL(t *testing.T) {
	k := Key{t.Name(), "prod", "db"}
	b, ctx := driver(t, k)
	if _, err := b.pool.Exec(ctx, `CREATE ROLE `+ident(k)+` LOGIN PASSWORD 'lost'`); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, ensure(t, ctx, b, k))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var db string
	if err := c.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil || db != ident(k) {
		t.Fatalf("connected to %q, want %q (%v)", db, ident(k), err)
	}
}

// Without REVOKE CONNECT ... FROM PUBLIC, Postgres lets every login open every database.
func TestAKeysLoginCannotOpenAnotherKeysDatabase(t *testing.T) {
	a, other := Key{t.Name(), "preview", "a"}, Key{t.Name(), "preview", "b"}
	b, ctx := driver(t, a, other)
	aURL := ensure(t, ctx, b, a)
	ensure(t, ctx, b, other)
	u, err := url.Parse(aURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + ident(other)
	c, err := pgx.Connect(ctx, u.String())
	var pgErr *pgconn.PgError
	if err == nil {
		_ = c.Close(ctx)
		t.Fatal("key A's login connected to key B's database")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("refused for the wrong reason, want insufficient_privilege: %v", err)
	}
	gone(t, ctx, b, `datname = $1`, ident(other)) // the refused login's backend, before the cleanup drops its role
}

// Postgres stores the verifier Canyon computes and accepts the password it was computed from, so
// the password itself is never in a statement a DDL-logging server writes down.
func TestTheServerStoresTheVerifierAndAcceptsThePassword(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	u, err := url.Parse(ensure(t, ctx, b, k))
	if err != nil {
		t.Fatal(err)
	}
	password := rand.Text()
	verifier, err := scramVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.pool.Exec(ctx, `ALTER ROLE `+ident(k)+` PASSWORD '`+verifier+`'`); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := server(t).QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname = $1`, ident(k)).Scan(&stored); err != nil || stored != verifier { // pg_authid is the superuser's to read
		t.Fatalf("stored %q, want the verifier as sent (%v)", stored, err)
	}
	u.User = url.UserPassword(ident(k), password)
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("the password the verifier was built from is refused: %v", err)
	}
	_ = c.Close(ctx)
}

func TestDeleteDropsTheDatabaseAndLoginAndAMissingOneIsGone(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t)
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatalf("deleting what never existed: %v", err)
	}
	ensure(t, ctx, b, k)
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 0 {
		t.Fatalf("%d of the database, login and group roles left after Delete", n)
	}
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}

// An install before this made the key's login and a database it owns, with no membership the admin
// inherits: on a managed Postgres, whose admin is no superuser, that database could not be dropped
// (canyonv2-8hpp: `must be owner of database`, 42501). Delete takes the membership first. The
// database is planted as the superuser: vanilla Postgres lets no one else own it for another role.
func TestDeleteDropsADatabaseAnEarlierInstallLeftToItsLogin(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t)
	quoted := pgx.Identifier{ident(k)}.Sanitize()
	if _, err := b.pool.Exec(ctx, `CREATE ROLE `+quoted+` LOGIN`); err != nil { // the admin's, as Ensure made it
		t.Fatal(err)
	}
	if _, err := server(t).Exec(ctx, `CREATE DATABASE `+quoted+` OWNER `+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname = $1)`, k); n != 0 {
		t.Fatalf("%d of the database and login left after Delete", n)
	}
}

// Delete ends every session of the key first: the app's login on its database and on another, and a
// person's, before anything is dropped, and none outlives its role (#2364).
func TestDeleteEndsEverySessionOfTheKeyFirst(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	var pids []int
	for _, c := range []*pgx.Conn{connect(t, ctx, b, app, ident(k)), connect(t, ctx, b, app, "postgres"), open(t, ctx, b, k, dana, Read)} {
		pids = append(pids, int(c.PgConn().PID()))
	}
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatalf("Delete with sessions open: %v", err)
	}
	var left int
	if err := server(t).QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)`, pids).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d of the key's sessions open after Delete (%v)", left, err)
	}
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 0 {
		t.Fatalf("%d of the database, login and group roles left after Delete", n)
	}
	if people, err := b.People(ctx, k); err != nil || len(people) != 0 {
		t.Fatalf("person roles left after Delete: %q (%v)", people, err)
	}
}

// A login past its LOGIN check when Delete makes the role NOLOGIN, still starting — paused by
// post_auth_delay on a database of its own — is waited out and ended, never left running as a dropped role.
func TestDeleteWaitsOutALoginStillStarting(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	cfg, err := pgx.ParseConfig(ensure(t, ctx, b, k))
	if err != nil {
		t.Fatal(err)
	}
	sup, other := server(t), ident(k)+"_other"
	if _, err := sup.Exec(ctx, `CREATE DATABASE `+other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = sup.Exec(ctx, `DROP DATABASE IF EXISTS `+other+` WITH (FORCE)`) })
	var oid uint32
	if err := b.pool.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname = $1`, ident(k)).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	cfg.Database, cfg.RuntimeParams["options"] = other, "-c post_auth_delay=2"
	var c *pgx.Conn
	started := make(chan error, 1)
	go func() {
		var err error
		c, err = pgx.ConnectConfig(ctx, cfg)
		started <- err
	}()
	// past its LOGIN check once it holds its database's lock, and in its pause while that lock's holder
	// has published no user
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var paused bool
		if err := sup.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l WHERE l.locktype = 'object' AND l.classid = 'pg_database'::regclass AND l.granted
			AND l.objid = (SELECT oid FROM pg_database WHERE datname = $1) AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.pid = l.pid AND a.usesysid IS NOT NULL))`, other).Scan(&paused); err != nil {
			t.Fatal(err)
		}
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the login never reached its pause")
		}
	}
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatalf("Delete with a login still starting: %v", err)
	}
	// ended as it started or just after, never a live session
	if err := <-started; err == nil {
		_, err = c.Exec(ctx, `SELECT 1`)
		_ = c.Close(ctx)
		if err == nil {
			t.Fatal("the paused login's session outlived Delete")
		}
	}
	var left int
	if err := sup.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usesysid = $1`, oid).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d session(s) of the dropped login still open (%v)", left, err)
	}
}

// A session whose own uncommitted ALTER ROLE holds its role's row — the app's login's here, which Delete
// makes NOLOGIN — cannot hold a delete off: Delete's DDL waits 2 s at most for a lock, ends the key's
// sessions, and runs again, as Revoke does (#2362, #2364).
func TestDeleteEndsASessionHoldingItsRolesRow(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k))
	for _, sql := range []string{`BEGIN`, `ALTER ROLE CURRENT_USER PASSWORD 'chosen-by-the-app'`} {
		if _, err := app.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := b.Delete(bounded, k); err != nil {
		t.Fatalf("Delete after %s: %v", time.Since(start), err)
	}
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 0 {
		t.Fatalf("%d of the database, login and group roles left after Delete", n)
	}
	if people, err := b.People(ctx, k); err != nil || len(people) != 0 {
		t.Fatalf("person roles left after Delete: %q (%v)", people, err)
	}
}

// An Issue in flight when Delete runs makes its role before Delete reads the key's person roles, and
// Delete drops it, or makes nothing: no role is left behind a Delete that answered Gone (#2364). The
// issue's COMMENT ON ROLE, inside its role's making, is held on pg_shdescription until Delete waits too.
func TestAnIssueRacingADeleteLeavesNoRole(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	sup := server(t)
	held, err := sup.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, `LOCK TABLE pg_shdescription IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) { // on pg_shdescription, or on the key's lock
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var w int
			if err := sup.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND (relation = 'pg_shdescription'::regclass
				OR locktype = 'advisory' AND (classid::bigint << 32 | objid::bigint) = hashtextextended($1, 0))`, k.String()).Scan(&w); err != nil {
				t.Fatal(err)
			}
			if w >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d waiting, want %d", w, n)
			}
		}
	}
	issued := make(chan error, 1)
	go func() { issued <- connectOnce(ctx, b, k, dana, Read) }()
	waiting(1)
	deleted := make(chan error, 1)
	go func() {
		_, err := b.Delete(ctx, k)
		deleted <- err
	}()
	waiting(2)
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	<-issued // its role or its database gone under it
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 0 {
		t.Fatalf("%d of the database, login and group roles left after Delete", n)
	}
	if people, err := b.People(ctx, k); err != nil || len(people) != 0 {
		t.Fatalf("person roles left after a Delete that answered Gone: %q (%v)", people, err)
	}
}

// An Issue goes on changing its person role once the key's lock is free (#2364). A DROP ROLE of Delete's
// that waited on such a change fails, once it commits, "tuple concurrently updated": Delete runs again
// and drops everything (#2380). An open transaction of the superuser's plays the Issue's ALTER ROLE.
func TestDeleteRetriesARoleAnIssueChangedMeanwhile(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatal(err)
	}
	role := personRole(k, dana.Subject)
	sup := server(t)
	held, err := sup.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, `ALTER ROLE `+pgx.Identifier{role}.Sanitize()+` CONNECTION LIMIT 9`); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := b.Delete(ctx, k)
		deleted <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // its DROP ROLE waits on held
		var w int
		if err := sup.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'DROP ROLE IF EXISTS %' AND strpos(query, $1) > 0`, role).Scan(&w); err != nil {
			t.Fatal(err)
		}
		if w > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Delete's DROP ROLE never waited on the role's change")
		}
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 0 {
		t.Fatalf("%d of the database, login and group roles left after Delete", n)
	}
	if people, err := b.People(ctx, k); err != nil || len(people) != 0 {
		t.Fatalf("person roles left after a Delete that answered Gone: %q (%v)", people, err)
	}
}

// A session the admin may not end — a superuser's — fails Delete closed: nothing is dropped, and no
// new session lands on the database or as the app's login meanwhile, so a delete again resumes (#2364).
func TestDeleteASessionTheAdminMayNotEndFailsAndKeepsEverything(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if version(t, ctx) < 150000 {
		t.Skip("on 14 the admin is the superuser")
	}
	app := ensure(t, ctx, b, k)
	connect(t, ctx, b, pgtest.URL(t), ident(k))
	_, err := b.Delete(ctx, k)
	refused(t, "Delete with a superuser's session open", err)
	if n := count(t, ctx, b, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1) + (SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $1 || '_read', $1 || '_write'))`, k); n != 4 {
		t.Fatalf("%d of the database, login and group roles left after a failed Delete, want all 4", n)
	}
	u, err := url.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + ident(k)
	if c, err := pgx.Connect(ctx, u.String()); err == nil {
		_ = c.Close(ctx)
		t.Error("a new session opened on the database after a failed Delete")
	} else {
		failsWith(t, "a new session on the database", "55000", err)
	}
	if c, err := pgx.Connect(ctx, app); err == nil {
		_ = c.Close(ctx)
		t.Error("the app's login opened a session after a failed Delete")
	} else {
		failsWith(t, "the app's login", "28000", err)
	}
}

// The same missing membership made Ensure's REVOKE CONNECT ... FROM PUBLIC a no-op with a warning on
// a managed Postgres, so every app's login could open every other app's database there. Planted as
// the test above; an Ensure as the admin must close the database to another key's login.
func TestEnsureClosesADatabaseAnEarlierInstallLeftOpenToOtherLogins(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	quoted, password := pgx.Identifier{ident(k)}.Sanitize(), rand.Text()
	for _, sql := range []string{`CREATE ROLE ` + quoted + ` LOGIN`, `CREATE ROLE ` + ident(other) + ` LOGIN PASSWORD '` + password + `'`} {
		if _, err := b.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server(t).Exec(ctx, `CREATE DATABASE `+quoted+` OWNER `+quoted); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(ensure(t, ctx, b, k))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(ident(other), password)
	c, err := pgx.Connect(ctx, u.String())
	var pgErr *pgconn.PgError
	if err == nil {
		_ = c.Close(ctx)
		t.Fatal("another key's login opened the database after Ensure")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("refused for the wrong reason, want insufficient_privilege: %v", err)
	}
	gone(t, ctx, b, `datname = $1`, ident(k)) // the refused login's backend, before the cleanup's Delete ends the database's sessions
}

func TestAnIdentifierFitsPostgresAndTwoLongKeysDoNotCollide(t *testing.T) {
	long := strings.Repeat("p", 63)
	a, c := ident(Key{long, "preview", "db"}), ident(Key{long, "prod", "db"})
	if len(a) != 26 || !strings.HasPrefix(a, "c_") || a == c {
		t.Fatalf("identifiers %q %q", a, c)
	}
	if a != ident(Key{long, "preview", "db"}) {
		t.Fatal("the identifier is stable")
	}
}

// Write advances a sequence, as an INSERT does, and cannot set it: setval needs UPDATE on the
// sequence, which neither group holds.
func TestAWriteGroupAdvancesASequenceButCannotSetIt(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	if _, err := connect(t, ctx, b, app, ident(k)).Exec(ctx, `CREATE TABLE claims (id serial PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	w := as(t, ctx, b, k, app, "write")
	if _, err := w.Exec(ctx, `SELECT nextval('claims_id_seq')`); err != nil {
		t.Fatalf("nextval as write: %v", err)
	}
	_, err := w.Exec(ctx, `SELECT setval('claims_id_seq', 100)`)
	refused(t, "setval as write", err)
}

// Neither group makes a temp table: one is not bounded by temp_file_limit, and shadows an app table a
// function names unqualified (design/DATABASE-SESSIONS.md §4). The app's own login still may.
func TestAGroupCannotMakeATempTable(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	for _, group := range []string{"read", "write"} {
		_, err := as(t, ctx, b, k, app, group).Exec(ctx, `CREATE TEMP TABLE planted (id int)`)
		refused(t, "a temp table as "+group, err)
	}
}

// A group's login opens its own app's database and no other's.
func TestAGroupCannotOpenAnotherAppsDatabase(t *testing.T) {
	a, other := Key{t.Name(), "preview", "a"}, Key{t.Name(), "preview", "b"}
	b, ctx := driver(t, a, other)
	ensure(t, ctx, b, a)
	u, err := url.Parse(member(t, ctx, b, other, ensure(t, ctx, b, other), "write"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + ident(a)
	c, err := pgx.Connect(ctx, u.String())
	if err == nil {
		_ = c.Close(ctx)
		t.Fatal("app B's write group opened app A's database")
	}
	refused(t, "B's write group into A's database", err)
	gone(t, ctx, b, `datname = $1`, ident(a)) // the refused login's backend, before the cleanup drops its role
}

// The app owns its database and may set its search_path, and owns public on 15+ (on 14 it creates
// there): a view there named as a catalog, calling a function of the app's, must never run as the
// admin, a superuser on a default Docker host, nor may the database's settings make the admin's
// session the app's.
func TestAnAppsDatabaseRunsNothingAsTheAdmin(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	c := connect(t, ctx, b, app, ident(k))
	for _, sql := range []string{
		`CREATE TABLE ran (who text)`,
		`CREATE FUNCTION public.ran() RETURNS name LANGUAGE sql AS $$ INSERT INTO public.ran VALUES (current_user); SELECT 'public'::name $$`,
		`CREATE VIEW public.pg_namespace AS SELECT public.ran() AS nspname, 0::oid AS nspowner`,
		`CREATE VIEW public.pg_roles AS SELECT public.ran() AS rolname, 0::oid AS oid`,
		`CREATE FUNCTION public.has_schema_privilege(text, text, text) RETURNS boolean LANGUAGE sql AS $$ SELECT public.ran() IS NULL $$`,
		`ALTER DATABASE ` + ident(k) + ` SET search_path = public, pg_catalog`,
	} {
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	ensure(t, ctx, b, k)
	rows, _ := c.Query(ctx, `SELECT who FROM public.ran`)
	ran, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ran) != 0 {
		t.Fatalf("the app's function ran as %q (%v)", ran, err)
	}
	if _, err := c.Exec(ctx, `ALTER DATABASE `+ident(k)+` SET role = `+ident(k)); err != nil {
		t.Fatal(err)
	}
	_, err = b.Ensure(ctx, k, "postgres")
	notAsMade(t, "Ensure with the database setting the admin's role", err, "the database sets the role of every session that connects to it (ALTER DATABASE … SET role)")
}

// Ensure fails while PUBLIC can still create in public, rather than answer a database a person's role
// could plant a function in: on 14 under a non-superuser admin, which cannot revoke what the
// bootstrap superuser's public grants, and on 15+ where a superuser gave public away and opened it.
func TestEnsureFailsWhilePublicStaysOpenToCreate(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	ctx := context.Background()
	b, err := NewBuiltin(ctx, limitedAdminURL(t, ctx, testAdmin), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := b.Delete(ctx, k); err != nil {
			t.Error(err)
		}
		b.Close()
	})
	_, err = b.Ensure(ctx, k, "postgres")
	if version(t, ctx) >= 150000 {
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connect(t, ctx, b, pgtest.URL(t), ident(k)).Exec(ctx, `ALTER SCHEMA public OWNER TO CURRENT_USER; GRANT CREATE ON SCHEMA public TO PUBLIC`); err != nil {
			t.Fatal(err)
		}
		_, err = b.Ensure(ctx, k, "postgres")
	}
	if !errors.Is(err, ErrUnsafeSchema) {
		t.Fatalf("Ensure with public open to PUBLIC: %v", err)
	}
	if err := b.Issue(ctx, k, dana, Read, time.Minute, func(context.Context, Login) error { return errors.New("issued") }); !errors.Is(err, ErrUnsafeSchema) {
		t.Fatalf("an issue with public open to PUBLIC: %v", err)
	}
}

// A group role Ensure finds is kept only as Canyon makes it: one with a login is refused.
func TestAGroupRoleCanyonDidNotMakeIsRefused(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if _, err := b.pool.Exec(ctx, `CREATE ROLE `+ident(k)+`_read LOGIN`); err != nil {
		t.Fatal(err)
	}
	_, err := b.Ensure(ctx, k, "postgres")
	notAsMade(t, "Ensure over a read group with a login", err, "the group role "+ident(k)+"_read has a login, an attribute or a membership Canyon never gave it")
}

// A relation in public the app holds no privilege on, a superuser's as an extension's table may be,
// fails the grants on what exists, and so Ensure: closed, never a half-granted group.
func TestARelationTheAppCannotGrantFailsEnsure(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if _, err := connect(t, ctx, b, pgtest.URL(t), ident(k)).Exec(ctx, `CREATE TABLE public.foreign_rows (id int)`); err != nil {
		t.Fatal(err)
	}
	_, err := b.Ensure(ctx, k, "postgres")
	refused(t, "Ensure over a relation the app cannot grant", err)
}

// notAsMade fails unless err is the driver's *NotAsMade saying found, word for word: the sentence a refused open,
// query or connection shows (contracts/routes.md, not_as_made).
func notAsMade(t *testing.T, what string, err error, found string) {
	t.Helper()
	var n *NotAsMade
	if !errors.As(err, &n) || n.Found != found {
		t.Errorf("%s: %v, want NotAsMade %q", what, err, found)
	}
}

// A person role with an attribute Canyon never gave it, granted to no role, is refused in a sentence that names the
// attribute alone (contracts/routes.md, not_as_made): never "…or is granted to , which…".
func TestAPersonRoleWithAnAttributeAloneIsRefused(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatal(err)
	}
	role := personRole(k, dana.Subject)
	if _, err := server(t).Exec(ctx, `ALTER ROLE `+role+` CREATEDB`); err != nil {
		t.Fatal(err)
	}
	notAsMade(t, "an issue with the role given CREATEDB", connectOnce(ctx, b, k, dana, Read), "your role "+role+" has an attribute Canyon never gave it")
}

// An issue for a database that is not there is ErrNoDatabase, never a database that did not answer.
func TestAnIssueForADatabaseThatIsNotThereIsErrNoDatabase(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if err := connectOnce(ctx, b, k, dana, Read); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("an issue for a database never made: %v", err)
	}
}

var dana, ana = Person{Subject: "auth0|dana", Email: "dana@meridian.example"}, Person{Subject: "auth0|ana", Email: "ana@meridian.example"}

// dial connects with a login as the server does: its address, database, role, secret and TLS.
func dial(ctx context.Context, l Login) (*pgx.Conn, error) {
	cfg, err := LoginConfig(l)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// open issues person a login on the key's database in mode and connects with it, as a database
// session's query or relay does. The connection is closed, and its session waited out, when the test
// ends.
func open(t *testing.T, ctx context.Context, b *Builtin, k Key, p Person, mode Mode) *pgx.Conn {
	t.Helper()
	var c *pgx.Conn
	if err := b.Issue(ctx, k, p, mode, time.Minute, func(ctx context.Context, l Login) (err error) {
		c, err = dial(ctx, l)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close(ctx)
		gone(t, ctx, b, `pid = $1`, c.PgConn().PID())
	})
	return c
}

// connectOnce issues person a login on the key's database in mode, and connects with it once.
func connectOnce(ctx context.Context, b *Builtin, k Key, p Person, mode Mode) error {
	return b.Issue(ctx, k, p, mode, time.Minute, func(ctx context.Context, l Login) error {
		c, err := dial(ctx, l)
		if err == nil {
			_ = c.Close(ctx)
		}
		return err
	})
}

// failsWith fails the test unless err is Postgres's code.
func failsWith(t *testing.T, what, code string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// A login opens for its ttl by the database's clock and not a second more: a server that dies between
// an issue and its NOLOGIN leaves a password that opens nothing past it, 60 s at most
// (design/DATABASE-SESSIONS.md §4). A ttl of 2 s, so the test waits 3.
func TestAPersonRoleCannotLogInPastItsTTL(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if err := b.Issue(ctx, k, dana, Read, 2*time.Second, func(_ context.Context, l Login) error {
		c, err := dial(ctx, l)
		if err != nil {
			return err
		}
		_ = c.Close(ctx)
		time.Sleep(3 * time.Second) // before Issue's NOLOGIN: VALID UNTIL alone stands between the password and a login
		c, err = dial(ctx, l)
		if err == nil {
			_ = c.Close(ctx)
		}
		failsWith(t, "a login a second past its ttl", "28P01", err)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A ttl of none, or over the minute a login may open, is refused before the database is asked.
func TestAnIssueRefusesATTLOutsideAMinute(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, 61 * time.Second} {
		err := (&Builtin{}).Issue(context.Background(), Key{"claims-portal", "preview", "db"}, dana, Read, ttl, func(context.Context, Login) error {
			return errors.New("issued")
		})
		if err == nil || strings.Contains(err.Error(), "issued") {
			t.Errorf("a ttl of %s: %v", ttl, err)
		}
	}
}

// Once its connection is made the role logs in no more, with the issued password or one its person set
// on it: only the next issue opens it again, with a password of its own.
func TestALoginOpensOneConnection(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	var issued Login
	if err := b.Issue(ctx, k, dana, Read, time.Minute, func(ctx context.Context, l Login) error {
		issued = l
		c, err := dial(ctx, l)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(ctx) }()
		_, err = c.Exec(ctx, `ALTER ROLE CURRENT_USER PASSWORD 'chosen-by-dana'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	own := issued
	own.Secret = NewSecret("chosen-by-dana")
	for _, c := range []struct {
		what, code string // a password is checked before the login: the person's own replaced the issued one
		l          Login
	}{{"the issued password", "28P01", issued}, {"the person's own password", "28000", own}} {
		conn, err := dial(ctx, c.l)
		if err == nil {
			_ = conn.Close(ctx)
		}
		failsWith(t, c.what+" after the login connected", c.code, err)
	}
	gone(t, ctx, b, `usename = $1`, issued.User)
	open(t, ctx, b, k, dana, Read)
}

// Revoke ends a person's open write transaction: its backend is gone from pg_stat_activity when Revoke
// returns, the row it wrote never lands, and the role logs in no more (design/DATABASE-SESSIONS.md §4).
func TestRevokeKillsAnOpenWriteTransaction(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k))
	if _, err := app.Exec(ctx, `CREATE TABLE claims (note text)`); err != nil {
		t.Fatal(err)
	}
	w := open(t, ctx, b, k, dana, Write)
	for _, sql := range []string{`BEGIN`, `INSERT INTO claims VALUES ('uncommitted')`} {
		if _, err := w.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // the request that asked is gone: the revoke runs on
	if err := b.Revoke(cancelled, k, dana); err != nil {
		t.Fatal(err)
	}
	var left, rows int
	if err := b.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename = $1`, personRole(k, dana.Subject)).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d backend(s) of the role after Revoke (%v)", left, err)
	}
	if _, err := w.Exec(ctx, `COMMIT`); err == nil {
		t.Fatal("the revoked transaction committed")
	}
	if err := app.QueryRow(ctx, `SELECT count(*) FROM claims`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d row(s) of the revoked transaction landed (%v)", rows, err)
	}
	if err := b.Revoke(ctx, k, ana); err != nil {
		t.Fatalf("revoking a person never issued a login: %v", err)
	}
}

// Every login runs under its environment's limits, set on the role before its password
// (design/DATABASE-SESSIONS.md §6).
func TestALoginRunsUnderItsEnvironmentsLimits(t *testing.T) {
	keys := map[Key][]string{
		{t.Name(), "preview", "db"}: {"15min", "3s", "10min", "1GB"},
		{t.Name(), "prod", "db"}:    {"5min", "3s", "1min", "1GB"},
	}
	b, ctx := driver(t, slices.Collect(maps.Keys(keys))...)
	for k, want := range keys {
		ensure(t, ctx, b, k)
		var got [4]string
		if err := open(t, ctx, b, k, dana, Read).QueryRow(ctx, `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
				current_setting('idle_in_transaction_session_timeout'), current_setting('temp_file_limit')`).Scan(&got[0], &got[1], &got[2], &got[3]); err != nil || !slices.Equal(got[:], want) {
			t.Errorf("%s: limits %q, want %q (%v)", k.Env, got, want, err)
		}
	}
}

// An admin that may not set temp_file_limit — RDS's, which no superuser can grant it — is served by the
// limit in effect for the role, an operator's for the whole server, at 1 GB or less; with none, or one
// over, it is refused before any login, naming both ways out (design/DATABASE-SESSIONS.md §6).
func TestTempFileLimitIsSetOrFoundInEffect(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	ctx := context.Background()
	if version(t, ctx) < 150000 {
		t.Skip("Postgres 14 runs the driver under a superuser")
	}
	b := driverAs(t, ctx, limitedAdminURL(t, ctx, ungrantedAdmin), "", k)
	ensure(t, ctx, b, k)
	for _, limit := range []string{"-1", "2GB", "512MB"} {
		if _, err := server(t).Exec(ctx, `ALTER DATABASE `+ident(k)+` SET temp_file_limit = '`+limit+`'`); err != nil {
			t.Fatal(err)
		}
		var ran string
		err := b.Issue(ctx, k, dana, Read, time.Minute, func(ctx context.Context, l Login) error {
			c, err := dial(ctx, l)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close(ctx) }()
			return c.QueryRow(ctx, `SHOW temp_file_limit`).Scan(&ran)
		})
		if limit == "512MB" {
			if err != nil || ran != limit {
				t.Errorf("a server limit of %s: the login ran under %q (%v)", limit, ran, err)
			}
			continue
		}
		if !errors.Is(err, ErrLimitsUnsupported) || ran != "" || !strings.Contains(err.Error(), `GRANT SET ON PARAMETER temp_file_limit TO "`+ungrantedAdmin+`"`) || !strings.Contains(err.Error(), "for the whole server") {
			t.Errorf("a server limit of %s: %v (a login ran under %q)", limit, err, ran)
		}
		var canLogin bool
		if err := b.pool.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, personRole(k, dana.Subject)).Scan(&canLogin); err != nil || canLogin {
			t.Errorf("a server limit of %s: the refused role can log in: %v (%v)", limit, canLogin, err)
		}
	}
	gone(t, ctx, b, `usename = $1`, personRole(k, dana.Subject))
}

// Each issue sets the role's group to its mode and revokes the other: a read login after a write one
// reads, and cannot write.
func TestALoginHoldsOnlyItsModesGroup(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if _, err := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k)).Exec(ctx, `CREATE TABLE claims (note text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, ctx, b, k, dana, Write).Exec(ctx, `INSERT INTO claims VALUES ('written')`); err != nil {
		t.Fatalf("insert as write: %v", err)
	}
	r := open(t, ctx, b, k, dana, Read)
	var rows int
	if err := r.QueryRow(ctx, `SELECT count(*) FROM claims`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("read as read: %d %v", rows, err)
	}
	_, err := r.Exec(ctx, `INSERT INTO claims VALUES ('planted')`)
	refused(t, "insert as read after a write login", err)
}

// Two people opening one database at once each get their login, with no tuple concurrently updated:
// the groups step runs under the key's lock, and each person's steps under their role's.
func TestTwoConcurrentOpensRaiseNoTupleConcurrentlyUpdated(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for _, p := range []Person{dana, ana} {
		for range 5 {
			wg.Go(func() { errs <- connectOnce(ctx, b, k, p, Read) })
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for _, p := range []Person{dana, ana} {
		gone(t, ctx, b, `usename = $1`, personRole(k, p.Subject))
	}
}

// Ten connections of one database session, opened at once as a client's pool opens them, all log in:
// each issue waits under the role's lock for the one before it to connect, so no issue's password
// replaces another's before its connect, however slow that connect; and the role's connection limit is
// the session's ten, which an eleventh meets (design/DATABASE-SESSIONS.md §6, Caps).
func TestTenConnectionsInABurstAllLogIn(t *testing.T) {
	ctx := context.Background()
	t.Run("set", func(t *testing.T) {
		k := Key{t.Name(), "preview", "db"}
		b, ctx := driver(t, k)
		ensure(t, ctx, b, k)
		burst(t, ctx, b, k)
	})
	// Under an admin that may not set temp_file_limit each issue reads it in a session of its own as the
	// role, which has ended before connect dials: it never holds one of the ten.
	t.Run("in effect", func(t *testing.T) {
		if version(t, ctx) < 150000 {
			t.Skip("Postgres 14 runs the driver under a superuser")
		}
		k := Key{t.Name(), "preview", "db"}
		b := driverAs(t, ctx, limitedAdminURL(t, ctx, ungrantedAdmin), "", k)
		ensure(t, ctx, b, k)
		if _, err := server(t).Exec(ctx, `ALTER DATABASE `+ident(k)+` SET temp_file_limit = '1GB'`); err != nil {
			t.Fatal(err)
		}
		burst(t, ctx, b, k)
	})
}

// burst opens the session's ten connections at once, holds them, and meets the limit with an eleventh.
func burst(t *testing.T, ctx context.Context, b *Builtin, k Key) {
	t.Helper()
	var wg sync.WaitGroup
	conns := make(chan *pgx.Conn, sessionCap)
	start := time.Now()
	for range sessionCap {
		wg.Go(func() {
			if err := b.Issue(ctx, k, dana, Write, time.Minute, func(ctx context.Context, l Login) error {
				time.Sleep(50 * time.Millisecond) // the window another issue's password would land in
				c, err := dial(ctx, l)
				if err == nil {
					conns <- c
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	t.Logf("%d logins in a burst took %s", sessionCap, time.Since(start)) // design/DATABASE-SESSIONS.md §15 records it
	close(conns)
	for c := range conns {
		t.Cleanup(func() {
			_ = c.Close(ctx)
			gone(t, ctx, b, `pid = $1`, c.PgConn().PID())
		})
	}
	failsWith(t, "an eleventh connection", "53300", connectOnce(ctx, b, k, dana, Write))
}

// A person role is commented with its person, quoted by Postgres whatever the address and sub hold, is
// NOLOGIN with the session's connection limit, and goes with its key's database, while the same
// person's role on another key stays.
func TestAPersonRoleIsFoundByItsCommentAndGoesWithItsDatabase(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	odd := Person{Subject: "oidc|o'brien two words", Email: "o'brien @meridian.example"}
	for _, key := range []Key{k, other} {
		ensure(t, ctx, b, key)
		if err := connectOnce(ctx, b, key, odd, Read); err != nil {
			t.Fatal(err)
		}
	}
	role := personRole(k, odd.Subject)
	var comment string
	var canLogin bool
	var limit int
	if err := b.pool.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_authid'), rolcanlogin, rolconnlimit FROM pg_roles WHERE rolname = $1`, role).Scan(&comment, &canLogin, &limit); err != nil ||
		comment != "canyon person o'brien @meridian.example oidc|o'brien two words" || canLogin || limit != sessionCap {
		t.Errorf("the role: comment %q, login %v, connection limit %d (%v)", comment, canLogin, limit, err)
	}
	gone(t, ctx, b, `usename = $1`, role)
	if _, err := b.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	rows, _ := b.pool.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname IN ($1, $2) ORDER BY rolname = $2`, role, personRole(other, odd.Subject))
	if left, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(left, []string{personRole(other, odd.Subject)}) {
		t.Fatalf("person roles after the key's Delete: %q, want only the other key's (%v)", left, err)
	}
}

// A person's role is p_ and 24 hex characters, 26 bytes, one per key and sub, never a key's login.
func TestAPersonRoleFitsPostgresAndIsOnePerKeyAndPerson(t *testing.T) {
	k := Key{strings.Repeat("p", 63), "preview", "db"}
	a := personRole(k, "auth0|dana")
	if len(a) != 26 || !strings.HasPrefix(a, "p_") || a != personRole(k, "auth0|dana") {
		t.Fatalf("person role %q", a)
	}
	for _, other := range []string{personRole(k, "auth0|ana"), personRole(Key{k.Project, "prod", "db"}, "auth0|dana"), ident(k)} {
		if other[2:] == a[2:] {
			t.Fatalf("%q and %q share a hash", a, other)
		}
	}
}

// A login prints, and marshals, without its secret: whatever logs one logs no credential.
func TestALoginPrintsNoSecret(t *testing.T) {
	l := Login{Host: "apps-db", Port: 5433, Database: "c_x", User: "p_x", Secret: NewSecret("SECRETVALUE"), TLS: TLSMode{SSLMode: "verify-full"}, Expires: time.Unix(0, 0)}
	nested := struct{ login Login }{l} // fmt reads an unexported field past every method of its type
	j, err := json.Marshal(struct{ Login Login }{l})
	if err != nil {
		t.Fatal(err)
	}
	var g bytes.Buffer
	_ = gob.NewEncoder(&g).Encode(l) // gob encodes no func: an error, and nothing written
	outs := []string{fmt.Sprint(l), fmt.Sprintf("%v %+v %#v %s", l, l, l, l.Secret), fmt.Sprintf("%v", &l), fmt.Sprintf("%v %+v %#v", nested, nested, nested), string(j), g.String()}
	for _, out := range outs {
		if strings.Contains(out, "SECRETVALUE") {
			t.Errorf("a login printed as %s", out)
		}
	}
	if l.Secret.Reveal() != "SECRETVALUE" {
		t.Fatal("the secret does not reveal itself to the connect")
	}
}

// A login is never weaker than the app's own URL: verify-full against the host's CA file where the host
// names one, as the app's URL verifies; the admin URL's sslmode and sslrootcert otherwise, as the app's
// URL keeps them (contracts/provisioner.md, People).
func TestALoginVerifiesTheServerWhereTheAppsURLDoes(t *testing.T) {
	for _, c := range []struct {
		admin, ca string
		want      TLSMode
	}{
		{"postgres://a@db:5433/postgres?sslmode=require&sslrootcert=/run/admin-ca.pem", "/etc/apps-db-ca/ca.pem", TLSMode{"verify-full", "/etc/apps-db-ca/ca.pem"}},
		{"postgres://a@db:5433/postgres?sslmode=disable", "/etc/apps-db-ca/ca.pem", TLSMode{"verify-full", "/etc/apps-db-ca/ca.pem"}},
		{"postgres://a@db:5433/postgres?sslmode=verify-ca&sslrootcert=/run/admin-ca.pem", "", TLSMode{"verify-ca", "/run/admin-ca.pem"}},
		{"postgres://a@db:5433/postgres", "", TLSMode{}},
	} {
		u, err := url.Parse(c.admin)
		if err != nil {
			t.Fatal(err)
		}
		if got := loginTLS(u, c.ca); got != c.want {
			t.Errorf("admin %s, CA file %q: a login's TLS is %+v, want %+v", c.admin, c.ca, got, c.want)
		}
	}
}

// A NOLOGIN that fails fails the issue, though its connection was made: the role then logs in until
// VALID UNTIL alone, a minute at most, and the error says so for the caller to drop the connection.
func TestANologinThatFailsFailsTheIssue(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	role := personRole(k, dana.Subject)
	err := b.Issue(ctx, k, dana, Read, time.Minute, func(ctx context.Context, _ Login) error {
		// the admin's session holding the role's lock, which would run the NOLOGIN, ends
		_, err := server(t).Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND (classid::bigint << 32 | objid::bigint) = hashtextextended($1, 0)`, role)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "NOLOGIN") {
		t.Fatalf("an issue whose NOLOGIN failed: %v", err)
	}
	var canLogin, withinTTL bool
	if err := server(t).QueryRow(ctx, `SELECT rolcanlogin, rolvaliduntil <= now() + interval '60 seconds' FROM pg_roles WHERE rolname = $1`, role).Scan(&canLogin, &withinTTL); err != nil || !canLogin || !withinTTL {
		t.Fatalf("the role after a failed NOLOGIN: login %v, valid within the ttl %v (%v)", canLogin, withinTTL, err)
	}
}

// On 14 and 15 an app's login may grant itself to its read group in a migration, which would hand every
// reader the app's rights: every issue checks the groups as Canyon made them, and refuses. 16 refuses
// the grant itself.
func TestAGroupTheAppGrantedItselfToIsRefused(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k))
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatal(err)
	}
	_, err := app.Exec(ctx, `GRANT `+ident(k)+` TO `+ident(k)+`_read`)
	if err != nil || version(t, ctx) >= 160000 {
		refused(t, "the app's login granting itself to its read group", err)
		return
	}
	notAsMade(t, "an issue with the app's login in its read group", connectOnce(ctx, b, k, dana, Read),
		"the group role "+ident(k)+"_read has a login, an attribute or a membership Canyon never gave it")
}

// A person whose own uncommitted ALTER ROLE holds their role's row cannot hold a revoke off: their
// backend ends first, the role is then NOLOGIN, and their transaction's row never lands.
func TestRevokeEndsAPersonHoldingTheirRolesRow(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k))
	if _, err := app.Exec(ctx, `CREATE TABLE claims (note text)`); err != nil {
		t.Fatal(err)
	}
	w := open(t, ctx, b, k, dana, Write)
	for _, sql := range []string{`SET idle_in_transaction_session_timeout = 0`, `BEGIN`, `INSERT INTO claims VALUES ('held')`, `ALTER ROLE CURRENT_USER PASSWORD 'chosen-by-dana'`} {
		if _, err := w.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	start := time.Now()
	if err := b.Revoke(ctx, k, dana); err != nil {
		t.Fatalf("revoke after %s: %v", time.Since(start), err)
	}
	var left, rows int
	var canLogin bool
	if err := b.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_stat_activity WHERE usename = $1), rolcanlogin FROM pg_roles WHERE rolname = $1`, personRole(k, dana.Subject)).Scan(&left, &canLogin); err != nil || left != 0 || canLogin {
		t.Fatalf("after Revoke: %d backend(s), login %v (%v)", left, canLogin, err)
	}
	if err := app.QueryRow(ctx, `SELECT count(*) FROM claims`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d row(s) of the revoked transaction landed (%v)", rows, err)
	}
}

// caFile writes a CA certificate of no server's to a file, as CANYON_APP_DATABASE_CA_FILE names one.
func caFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "apps database CA"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The session that reads temp_file_limit as the role meets the server as the login does, never under the
// admin URL's weaker TLS: with the host's CA file named, verify-full, which the test server, speaking no
// TLS, refuses, so no login is handed over.
func TestTheTempFileProbeMeetsTheServerAsTheLoginDoes(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	ctx := context.Background()
	if version(t, ctx) < 150000 {
		t.Skip("Postgres 14 runs the driver under a superuser")
	}
	u, err := url.Parse(limitedAdminURL(t, ctx, ungrantedAdmin))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	b := driverAs(t, ctx, u.String(), caFile(t), k)
	ensure(t, ctx, b, k)
	if _, err := server(t).Exec(ctx, `ALTER DATABASE `+ident(k)+` SET temp_file_limit = '512MB'`); err != nil {
		t.Fatal(err)
	}
	called := false
	err = b.Issue(ctx, k, dana, Read, time.Minute, func(context.Context, Login) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("the probe under the admin's sslmode=disable: %v (connect called %v)", err, called)
	}
}

// From Postgres 16 a REVOKE ends only the admin's own grant: a write group another grantor gave the role
// stays through a read issue's revoke, and the issue is refused rather than hand a reader write rights.
func TestAGrantByAnotherGrantorNeverReachesALogin(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	if _, err := connect(t, ctx, b, ensure(t, ctx, b, k), ident(k)).Exec(ctx, `CREATE TABLE claims (note text)`); err != nil {
		t.Fatal(err)
	}
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatal(err)
	}
	if _, err := server(t).Exec(ctx, `GRANT `+ident(k)+`_write TO `+personRole(k, dana.Subject)); err != nil { // the superuser's grant, not the admin's
		t.Fatal(err)
	}
	var c *pgx.Conn
	err := b.Issue(ctx, k, dana, Read, time.Minute, func(ctx context.Context, l Login) (err error) {
		c, err = dial(ctx, l)
		return err
	})
	if c != nil {
		t.Cleanup(func() {
			_ = c.Close(ctx)
			gone(t, ctx, b, `pid = $1`, c.PgConn().PID())
		})
		_, insert := c.Exec(ctx, `INSERT INTO claims VALUES ('planted')`)
		refused(t, "insert as read with another grantor's write grant", insert)
	}
	switch {
	case err != nil:
		notAsMade(t, "a read issue over another grantor's write grant", err, "your role "+personRole(k, dana.Subject)+" is also a member of "+ident(k)+"_write, by a grant Canyon can't revoke")
	case err == nil && version(t, ctx) >= 160000:
		t.Fatal("a read issue went on with another grantor's write grant in place")
	}
}

// On 14 a role may grant itself onward as its own session user: a person could hand their role, and its
// group, to another app's login. The next issue refuses that, and Revoke takes it back. Later versions
// refuse the grant itself.
func TestAPersonRoleGrantedOnwardIsRefusedAndTakenBack(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	ensure(t, ctx, b, k)
	ensure(t, ctx, b, other)
	role := personRole(k, dana.Subject)
	_, err := open(t, ctx, b, k, dana, Read).Exec(ctx, `GRANT `+role+` TO `+ident(other))
	if err != nil {
		refused(t, "a person granting their role to another app's login", err)
		return
	}
	notAsMade(t, "an issue with the role granted onward", connectOnce(ctx, b, k, dana, Read),
		"your role "+role+" has an attribute, or is granted to "+ident(other)+", which Canyon never gave it")
	if err := b.Revoke(ctx, k, dana); err != nil {
		t.Fatal(err)
	}
	var onward int
	if err := server(t).QueryRow(ctx, `SELECT count(*) FROM pg_auth_members WHERE roleid = (SELECT oid FROM pg_roles WHERE rolname = $1) AND member = (SELECT oid FROM pg_roles WHERE rolname = $2)`, role, ident(other)).Scan(&onward); err != nil || onward != 0 {
		t.Fatalf("the role still granted to another app's login after Revoke: %d (%v)", onward, err)
	}
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatalf("an issue after Revoke took the grant back: %v", err)
	}
}

// A person role is used only as Canyon makes it: one found with an attribute is refused.
func TestAPersonRoleCanyonDidNotMakeIsRefused(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	role := personRole(k, dana.Subject)
	if _, err := b.pool.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN CREATEDB`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // uncommented, so the key's Delete never finds it
		if _, err := b.pool.Exec(ctx, `DROP ROLE IF EXISTS `+role); err != nil {
			t.Error(err)
		}
	})
	if err := connectOnce(ctx, b, k, dana, Read); err == nil || !strings.Contains(err.Error(), "has an attribute") {
		t.Fatalf("an issue over a person role with CREATEDB: %v", err)
	}
}

// Each issue holds a connection through its connect, from a pool of its own: a dozen slow opens leave
// Ensure, Delete and Revoke theirs.
func TestSlowOpensDoNotStallEnsure(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	var wg sync.WaitGroup
	connecting := make(chan struct{}, 12)
	for i := range 12 {
		p := Person{Subject: fmt.Sprintf("auth0|slow-%d", i), Email: "slow@meridian.example"}
		wg.Go(func() {
			_ = b.Issue(ctx, k, p, Read, time.Minute, func(context.Context, Login) error {
				connecting <- struct{}{}
				time.Sleep(3 * time.Second)
				return nil
			})
		})
	}
	for range 4 {
		<-connecting
	}
	start := time.Now()
	ensure(t, ctx, b, k)
	took := time.Since(start)
	wg.Wait()
	if took > 2*time.Second {
		t.Fatalf("Ensure waited %s behind slow opens", took)
	}
}

// A setting a person makes on their own role in the database outranks the role's own: each issue clears
// it before the limits, so the next login runs under them again.
func TestAPersonsOwnSettingsDoNotOutliveTheirLogin(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if _, err := open(t, ctx, b, k, dana, Read).Exec(ctx, `ALTER ROLE CURRENT_USER IN DATABASE `+ident(k)+` SET statement_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := open(t, ctx, b, k, dana, Read).QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&got); err != nil || got != "15min" {
		t.Fatalf("the next login's statement_timeout: %q (%v)", got, err)
	}
}

// Revoke ends the role's memberships both ways by every grantor, or fails: from 16 a REVOKE ends only
// the admin's own grant, and another grantor's write group, or its grant of the role onward, would
// outlive a revoke that said it succeeded.
func TestRevokeLeavesNoGrantOfAnotherGrantor(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	ensure(t, ctx, b, k)
	ensure(t, ctx, b, other)
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatal(err)
	}
	role := personRole(k, dana.Subject)
	for _, sql := range []string{`GRANT ` + ident(k) + `_write TO ` + role, `GRANT ` + role + ` TO ` + ident(other)} { // the superuser's grants, not the admin's
		if _, err := server(t).Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	err := b.Revoke(ctx, k, dana)
	var held int
	if err := server(t).QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles p ON p.rolname = $1
		WHERE m.member = p.oid OR (m.roleid = p.oid AND m.member = (SELECT oid FROM pg_roles WHERE rolname = $2))`, role, ident(other)).Scan(&held); err != nil {
		t.Fatal(err)
	}
	switch {
	case err == nil && held != 0:
		t.Fatalf("Revoke succeeded with %d grant(s) of another grantor left", held)
	case err != nil && (held == 0 || !strings.Contains(err.Error(), "after its revoke")):
		t.Fatalf("Revoke: %v (%d grant(s) left)", err, held)
	}
}

// Activity counts a person's backends on the key, and those running a statement now: an idle client is
// live, a long report is active, not idle (design/DATABASE-SESSIONS.md §6). Another person's backends,
// and the person's own on another key, are not theirs here.
func TestActivityCountsThePersonsBackendsAndTheActiveOnes(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	ensure(t, ctx, b, k)
	ensure(t, ctx, b, other)
	open(t, ctx, b, k, dana, Read)
	busy := open(t, ctx, b, k, dana, Read)
	open(t, ctx, b, k, ana, Read)
	open(t, ctx, b, other, dana, Read)
	if got, err := b.Activity(ctx, k, dana); err != nil || got != (Activity{Live: 2}) {
		t.Fatalf("dana's activity, both idle: %+v (%v)", got, err)
	}
	done := make(chan error, 1)
	go func() { _, err := busy.Exec(ctx, `SELECT pg_sleep(2)`); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got, err := b.Activity(ctx, k, dana)
		if err != nil {
			t.Fatal(err)
		}
		if got == (Activity{Live: 2, Active: 1}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dana's activity, one running a statement: %+v", got)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Cancel cancels the statement of a backend of the person's role, and leaves anyone else's running: a
// pid is all a cancel names, so the role is what stops one person cancelling another's.
func TestCancelReachesOnlyABackendOfThePersons(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	c := open(t, ctx, b, k, dana, Read)
	pid := int(c.PgConn().PID())
	done := make(chan error, 1)
	go func() { _, err := c.Exec(ctx, `SELECT pg_sleep(30)`); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if got, err := b.Activity(ctx, k, dana); err != nil || got.Active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dana's statement never ran")
		}
	}
	if err := b.Cancel(ctx, k, ana, pid); err != nil {
		t.Fatalf("ana's cancel of dana's backend: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("ana's cancel ended dana's statement: %v", err)
	case <-time.After(time.Second):
	}
	if err := b.Cancel(ctx, k, dana, pid); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		failsWith(t, "dana's cancelled statement", "57014", err)
	case <-time.After(5 * time.Second):
		t.Fatal("dana's cancel never ended her statement")
	}
}

// Terminate ends the one backend it names, when it is the person's, verified gone; their other
// connection goes on, and a pid that is someone else's, or gone already, is left alone.
func TestTerminateEndsOneBackendOfThePersons(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	ended, kept := open(t, ctx, b, k, dana, Read), open(t, ctx, b, k, dana, Read)
	pid := int(ended.PgConn().PID())
	if err := b.Terminate(ctx, k, ana, pid); err != nil {
		t.Fatalf("ana's terminate of dana's backend: %v", err)
	}
	if got, err := b.Activity(ctx, k, dana); err != nil || got.Live != 2 {
		t.Fatalf("ana's terminate ended a backend of dana's: %+v (%v)", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // the check that asked is gone: the terminate runs on
	if err := b.Terminate(cancelled, k, dana, pid); err != nil {
		t.Fatal(err)
	}
	var left bool
	if err := b.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&left); err != nil || left {
		t.Fatalf("the terminated backend is in pg_stat_activity: %v (%v)", left, err)
	}
	if err := kept.Ping(ctx); err != nil {
		t.Fatalf("dana's other connection: %v", err)
	}
	if err := b.Terminate(ctx, k, dana, pid); err != nil {
		t.Fatalf("terminating a backend gone already: %v", err)
	}
}

// People names the key's person roles by their persons' subjects, found by their comments, and Forget
// drops one person's, its backends ended first, leaving the others and the person's role on another key; a
// forgotten person's next issue makes their role again (design/DATABASE-SESSIONS.md §4, retired roles).
func TestForgetDropsOnePersonsRoleFoundByItsComment(t *testing.T) {
	k, other := Key{t.Name(), "preview", "db"}, Key{t.Name(), "preview", "other"}
	b, ctx := driver(t, k, other)
	ensure(t, ctx, b, k)
	ensure(t, ctx, b, other)
	for _, c := range []struct {
		k Key
		p Person
	}{{k, ana}, {other, dana}} {
		if err := connectOnce(ctx, b, c.k, c.p, Read); err != nil {
			t.Fatal(err)
		}
	}
	live := open(t, ctx, b, k, dana, Read)
	if people, err := b.People(ctx, k); err != nil || !slices.Equal(people, []string{ana.Subject, dana.Subject}) {
		t.Fatalf("the key's people: %q (%v)", people, err)
	}
	if err := b.Forget(ctx, k, dana); err != nil {
		t.Fatal(err)
	}
	rows, _ := b.pool.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname IN ($1, $2, $3) ORDER BY rolname`, personRole(k, dana.Subject), personRole(k, ana.Subject), personRole(other, dana.Subject))
	want := []string{personRole(k, ana.Subject), personRole(other, dana.Subject)}
	slices.Sort(want)
	if left, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(left, want) {
		t.Fatalf("person roles after Forget: %q, want ana's on the key and dana's on the other (%v)", left, err)
	}
	if err := live.Ping(ctx); err == nil {
		t.Fatal("the forgotten role's connection still answers")
	}
	if err := b.Forget(ctx, k, dana); err != nil {
		t.Fatalf("forgetting a role gone already: %v", err)
	}
	if err := connectOnce(ctx, b, k, dana, Read); err != nil {
		t.Fatalf("a forgotten person's next issue: %v", err)
	}
}

// relayed is a read connection as the relay makes one (#2826): issued, judged by ReadOnly inside connect, then dialled.
func relayed(ctx context.Context, b *Builtin, k Key, p Person) error {
	return b.Issue(ctx, k, p, Read, time.Minute, func(ctx context.Context, l Login) error {
		if err := b.ReadOnly(ctx, l); err != nil {
			return err
		}
		c, err := dial(ctx, l)
		if err == nil {
			_ = c.Close(ctx)
		}
		return err
	})
}

// secondChecked is a read connection or a read query as the server makes one (#2918): issued, judged by
// NoSecondConnection inside connect, then dialled.
func secondChecked(ctx context.Context, b *Builtin, k Key, p Person) error {
	return b.Issue(ctx, k, p, Read, time.Minute, func(ctx context.Context, l Login) error {
		if err := b.NoSecondConnection(ctx, l); err != nil {
			return err
		}
		c, err := dial(ctx, l)
		if err == nil {
			_ = c.Close(ctx)
		}
		return err
	})
}

// A read connection or query refuses while the person's role could open a second connection from inside the database,
// which neither its grants nor a READ ONLY transaction hold (#2918; contracts/provisioner.md, NoSecondConnection):
// dblink, whose functions PUBLIC may EXECUTE once a superuser installs it, and a foreign-data wrapper the role may use,
// postgres_fdw's. Each is named, the first five in a fixed order; with neither, or once revoked, it goes through.
func TestAReadConnectionRefusesASecondConnection(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	if err := secondChecked(ctx, b, k, dana); err != nil {
		t.Fatalf("a read connection with neither extension: %v", err)
	}
	super := connect(t, ctx, b, pgtest.URL(t), ident(k))
	if _, err := super.Exec(ctx, `CREATE EXTENSION dblink`); err != nil {
		t.Fatalf("installing dblink: %v", err)
	}
	var unsafe *ReadOnlyUnsafe
	err := secondChecked(ctx, b, k, dana)
	if !errors.As(err, &unsafe) || len(unsafe.Found) != maxNamed || unsafe.More == 0 || unsafe.Found[0] != "EXECUTE on dblink function public.dblink(text)" {
		t.Fatalf("a read connection with dblink installed: %v, want ReadOnlyUnsafe naming public.dblink(text) first, then four more and a count", err)
	}
	for _, f := range unsafe.Found {
		if !strings.HasPrefix(f, "EXECUTE on dblink function public.dblink") {
			t.Errorf("named %q, want only dblink's functions", f)
		}
	}
	if _, err := super.Exec(ctx, `REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if err := secondChecked(ctx, b, k, dana); err != nil {
		t.Fatalf("dblink's functions revoked from PUBLIC: %v", err)
	}
	if _, err := super.Exec(ctx, `CREATE EXTENSION postgres_fdw; GRANT USAGE ON FOREIGN DATA WRAPPER postgres_fdw TO PUBLIC`); err != nil {
		t.Fatalf("installing postgres_fdw: %v", err)
	}
	if err := secondChecked(ctx, b, k, dana); !errors.As(err, &unsafe) || unsafe.Error() != "USAGE on foreign-data wrapper postgres_fdw" {
		t.Fatalf("a read connection that may use postgres_fdw: %v, want ReadOnlyUnsafe naming it alone", err)
	}
}

// A SECURITY DEFINER function runs dblink as its owner: with dblink revoked from PUBLIC and granted to the app's login,
// a read role that may EXECUTE the app's definer wrapper opens a second connection through it, and is refused naming
// the wrapper (#2920's review). Revoked from PUBLIC, the wrapper refuses nothing.
func TestADblinkWrapperIsASecondConnection(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	super := connect(t, ctx, b, pgtest.URL(t), ident(k))
	if _, err := super.Exec(ctx, `CREATE EXTENSION dblink; REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;
		GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO `+ident(k)); err != nil {
		t.Fatal(err)
	}
	asApp := connect(t, ctx, b, app, ident(k))
	if _, err := asApp.Exec(ctx, `CREATE FUNCTION helper(conn text, sql text) RETURNS text LANGUAGE sql SECURITY DEFINER AS 'SELECT public.dblink_exec(conn, sql)'`); err != nil {
		t.Fatal(err)
	}
	var unsafe *ReadOnlyUnsafe
	if err := secondChecked(ctx, b, k, dana); !errors.As(err, &unsafe) || unsafe.Error() != "EXECUTE on SECURITY DEFINER function public.helper(conn text, sql text)" {
		t.Fatalf("a read role that may run the app's dblink wrapper: %v, want ReadOnlyUnsafe naming the wrapper alone", err)
	}
	if _, err := asApp.Exec(ctx, `REVOKE EXECUTE ON FUNCTION helper(text, text) FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if err := secondChecked(ctx, b, k, dana); err != nil {
		t.Fatalf("the wrapper revoked from PUBLIC: %v", err)
	}
}

// A check that can't read what it judges, in a database that answered, is ErrUnchecked, never a database that didn't
// answer: a role the catalog doesn't know is such a read, for both checks (#2920's review).
func TestAnUnreadCheckIsErrUnchecked(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	ensure(t, ctx, b, k)
	login := Login{User: "p_no_such_role", Database: ident(k)}
	if err := b.NoSecondConnection(ctx, login); !errors.Is(err, ErrUnchecked) {
		t.Errorf("NoSecondConnection over a role the catalog doesn't know: %v, want ErrUnchecked", err)
	}
	if err := b.ReadOnly(ctx, login); !errors.Is(err, ErrUnchecked) {
		t.Errorf("ReadOnly over a role the catalog doesn't know: %v, want ErrUnchecked", err)
	}
}

// A read connection refuses while the person's role could change data by any path (#2826; contracts/provisioner.md,
// ReadOnly): a grant to PUBLIC, or to the read group it inherits, of a write on a table or a column, of MAINTAIN on a
// materialized view (17+), of UPDATE or USAGE on a sequence, of CREATE on a schema or the database, of EXECUTE on a
// SECURITY DEFINER function, which writes as the app, or of UPDATE on an existing large object. Each is named; revoked,
// the connection goes through, and a read issue that relays nothing (a query's) is not judged. With them all at once the
// first five are named, sorted, and the rest counted.
func TestAReadIssueRefusesAPublicWrite(t *testing.T) {
	k := Key{t.Name(), "preview", "db"}
	b, ctx := driver(t, k)
	app := ensure(t, ctx, b, k)
	asApp := connect(t, ctx, b, app, ident(k))
	if _, err := asApp.Exec(ctx, `CREATE TABLE ledger (n int); CREATE TABLE claims (id int, note text); CREATE SEQUENCE invoice_no;
		CREATE MATERIALIZED VIEW totals AS SELECT count(*) AS n FROM ledger; CREATE SCHEMA billing; SELECT lo_create(4242);
		CREATE FUNCTION bump() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'INSERT INTO ledger VALUES (1) RETURNING n';
		REVOKE EXECUTE ON FUNCTION bump() FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	ensure(t, ctx, b, k) // a redeploy's grants on what exists
	if err := relayed(ctx, b, k, dana); err != nil {
		t.Fatalf("a read connection where nothing is granted past read: %v", err)
	}
	read, db, v := pgx.Identifier{ident(k) + "_read"}.Sanitize(), ident(k), version(t, ctx)
	type grant struct{ grant, named, revoke string }
	// in the order a refusal names them: relations, sequences, schemas, the database, functions, large objects
	relations := []grant{
		{`GRANT DELETE ON claims TO ` + read, "DELETE on public.claims", `REVOKE DELETE ON claims FROM ` + read},
		{`GRANT UPDATE (note) ON claims TO PUBLIC`, "UPDATE on public.claims", `REVOKE UPDATE (note) ON claims FROM PUBLIC`},
		{`GRANT INSERT ON ledger TO PUBLIC`, "INSERT on public.ledger", `REVOKE INSERT ON ledger FROM PUBLIC`},
	}
	if v >= 170000 { // MAINTAIN is Postgres 17's: before it only a view's owner refreshes it
		relations = append(relations, grant{`GRANT MAINTAIN ON totals TO PUBLIC`, "MAINTAIN on public.totals", `REVOKE MAINTAIN ON totals FROM PUBLIC`})
	} else {
		t.Logf("Postgres %d has no MAINTAIN privilege: a materialized view's REFRESH is its owner's alone, so that case can't be granted here", v)
	}
	cases := append(relations,
		grant{`GRANT USAGE ON SEQUENCE invoice_no TO PUBLIC`, "USAGE on sequence public.invoice_no", `REVOKE USAGE ON SEQUENCE invoice_no FROM PUBLIC`},
		grant{`GRANT CREATE ON SCHEMA billing TO PUBLIC`, "CREATE on schema billing", `REVOKE CREATE ON SCHEMA billing FROM PUBLIC`},
		grant{`GRANT CREATE ON DATABASE ` + db + ` TO PUBLIC`, "CREATE on database " + db, `REVOKE CREATE ON DATABASE ` + db + ` FROM PUBLIC`},
		grant{`GRANT EXECUTE ON FUNCTION bump() TO PUBLIC`, "EXECUTE on SECURITY DEFINER function public.bump()", `REVOKE EXECUTE ON FUNCTION bump() FROM PUBLIC`},
		grant{`GRANT UPDATE ON LARGE OBJECT 4242 TO PUBLIC`, "UPDATE on large object 4242", `REVOKE UPDATE ON LARGE OBJECT 4242 FROM PUBLIC`},
	)
	for _, c := range cases {
		if _, err := asApp.Exec(ctx, c.grant); err != nil {
			t.Fatal(err)
		}
		var unsafe *ReadOnlyUnsafe
		if err := relayed(ctx, b, k, dana); !errors.As(err, &unsafe) || unsafe.Error() != c.named {
			t.Errorf("%s: a read connection answered %v, want ReadOnlyUnsafe naming %s alone", c.grant, err, c.named)
		}
		if err := connectOnce(ctx, b, k, dana, Read); err != nil { // a query's issue: its READ ONLY transaction is what holds it
			t.Errorf("%s: a read issue that relays nothing: %v", c.grant, err)
		}
		if _, err := asApp.Exec(ctx, c.revoke); err != nil {
			t.Fatal(err)
		}
		if err := relayed(ctx, b, k, dana); err != nil {
			t.Errorf("%s, revoked: a read connection: %v", c.grant, err)
		}
	}
	var named []string
	for _, c := range cases {
		if _, err := asApp.Exec(ctx, c.grant); err != nil {
			t.Fatal(err)
		}
		named = append(named, c.named)
	}
	all := strings.Join(named[:5], ", ") + fmt.Sprintf(", and %d more", len(named)-5)
	var unsafe *ReadOnlyUnsafe
	if err := relayed(ctx, b, k, dana); !errors.As(err, &unsafe) || unsafe.Error() != all {
		t.Errorf("every grant at once: %v, want %q", err, all)
	}
}
