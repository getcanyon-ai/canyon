package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// postgresPlatform provides a release's postgres resources as an org's platform team does: one
// database and one role of its own per app, environment and resource name, on the org's own Postgres,
// the role owning the database and nobody else allowed to connect to it. The app's DATABASE_URL is
// built here, from the platform's own credentials; Canyon is told only where the database is.
type postgresPlatform struct {
	admin string // the platform's admin URL, PIPELINE_DATABASE_URL
	host  string // host:port, where apps reach it: the admin URL's own
}

func newPostgresPlatform(admin string) (*postgresPlatform, error) {
	u, err := url.Parse(admin)
	if err != nil || u.Host == "" {
		return nil, errors.New("PIPELINE_DATABASE_URL is not a postgres:// URL with a host")
	}
	return &postgresPlatform{admin: admin, host: u.Host}, nil
}

// owner is the comment the platform marks what it made with, so a delete finds every resource of an
// app and environment without keeping a list of its own.
func owner(app, env string) string { return "canyon-handoff " + app + " " + env + " " }

// dbName is the database's and the role's name: the same for the same app, environment and resource,
// and safe as a Postgres identifier and as an endpoint's one path segment.
func dbName(app, env, name string) string {
	sum := sha256.Sum256([]byte(app + "\x00" + env + "\x00" + name))
	return "h" + hex.EncodeToString(sum[:10])
}

// provide makes the resource's database and role if they are missing, gives the role a new password,
// and answers the variables the resource gives and the endpoint the report names: the database's
// place, never a credential. Running it again for the same release changes nothing but the password.
func (p *postgresPlatform) provide(ctx context.Context, app, env string, res resource) (map[string]string, string, error) {
	if res.Type != "postgres" {
		return nil, "", fmt.Errorf("this platform does not provide %s resources", res.Type)
	}
	if strings.ContainsAny(app+env+res.Name, `'\`) { // Canyon's names are [a-z0-9-]: the comments below quote them
		return nil, "", fmt.Errorf("a resource name this platform cannot quote: %q", res.Name)
	}
	name := dbName(app, env, res.Name)
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, "", err
	}
	password := hex.EncodeToString(b[:])
	conn, err := pgx.Connect(ctx, p.admin)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	// The statement below carries the password: never in the server's log, a failed statement's or any other.
	if _, err := conn.Exec(ctx, "SET log_min_error_statement = panic; SET log_statement = 'none'; SET log_min_duration_statement = -1"); err != nil {
		return nil, "", err
	}
	ident, mark := pgx.Identifier{name}.Sanitize(), "'"+owner(app, env)+res.Name+"'" // app, env and name are [a-z0-9-]: no quote to escape
	var role, db bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&role, &db); err != nil {
		return nil, "", err
	}
	verb := "ALTER ROLE "
	if !role {
		verb = "CREATE ROLE "
	}
	steps := []string{verb + ident + " WITH LOGIN PASSWORD '" + password + "'", "COMMENT ON ROLE " + ident + " IS " + mark} // hex: no quote to escape
	if !db {
		steps = append(steps, "CREATE DATABASE "+ident+" OWNER "+ident)
	}
	steps = append(steps, "REVOKE CONNECT ON DATABASE "+ident+" FROM PUBLIC", "COMMENT ON DATABASE "+ident+" IS "+mark)
	for _, sql := range steps {
		if _, err := conn.Exec(ctx, sql); err != nil {
			return nil, "", fmt.Errorf("providing postgres %s: %w", res.Name, err)
		}
	}
	values := map[string]string{}
	for _, g := range res.Gives {
		values[g] = "postgres://" + name + ":" + password + "@" + p.host + "/" + name + "?sslmode=disable"
	}
	return values, "postgres://" + p.host + "/" + name, nil
}

// drop deletes every database and role the platform made for the app and environment, and nothing
// else: a project delete's remove (delete: true). What is already gone is not an error.
func (p *postgresPlatform) drop(ctx context.Context, app, env string) error {
	conn, err := pgx.Connect(ctx, p.admin)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	prefix := owner(app, env)
	read := func(sql string) ([]string, error) {
		rows, err := conn.Query(ctx, sql, prefix)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	dbs, err := read("SELECT datname FROM pg_database WHERE left(coalesce(shobj_description(oid, 'pg_database'), ''), length($1)) = $1")
	if err != nil {
		return err
	}
	roles, err := read("SELECT rolname FROM pg_roles WHERE left(coalesce(shobj_description(oid, 'pg_authid'), ''), length($1)) = $1")
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{d}.Sanitize()+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	for _, r := range roles {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{r}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}
