package runs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stillReady is whether a resource's record is still ready: one a delete marked deleting, or removed,
// meanwhile is never made again, nor sealed for. forUpdate holds the record to the transaction's end.
func stillReady(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key provisioners.Key, forUpdate string) (bool, error) {
	var ready bool
	err := q.QueryRow(ctx, `SELECT state = 'ready' FROM resources WHERE project_id = $1 AND env = $2 AND name = $3`+forUpdate, key.Project, key.Env, key.Name).Scan(&ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ready, err
}

// Reissue asks the built-in driver's Ensure again for every ready resource it made, so each login gets a
// fresh password and each app's provisioned DATABASE_URL is replaced, one audit row each: the way back
// after a restore without role passwords (contracts/provisioner.md, After a restore). However it ends, it
// prints how many it re-issued and each environment of those that runs now, with what brings it back;
// public is Canyon's address, where an app's page is.
func Reissue(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault, builtin provisioners.Driver, public string, out io.Writer) (n int, err error) {
	var touched []provisioners.Key
	defer func() {
		_, _ = fmt.Fprintln(out, reissued(n))
		err = errors.Join(err, running(ctx, pool, touched, public, out))
	}()
	rows, err := pool.Query(ctx, `SELECT project_id, env, name, type FROM resources WHERE state = 'ready' AND driver = $1 ORDER BY project_id, env, name`, builtin.Name())
	if err != nil {
		return 0, err
	}
	type resource struct {
		key provisioners.Key
		typ string
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (resource, error) {
		var one resource
		return one, r.Scan(&one.key.Project, &one.key.Env, &one.key.Name, &one.typ)
	})
	if err != nil {
		return 0, err
	}
	for _, r := range all {
		ready, err := stillReady(ctx, pool, r.key, "")
		if err != nil {
			return n, err
		}
		if !ready { // deleted since the list was read: nothing to give a password
			continue
		}
		res, err := builtin.Ensure(ctx, r.key, r.typ)
		if err != nil {
			return n, err
		}
		sealed, err := replace(ctx, pool, keys, r.key, r.typ, res.Outputs)
		if err != nil {
			return n, fmt.Errorf("%s %s: %w", r.key.Project, r.key.Env, err)
		}
		if sealed {
			n, touched = n+1, append(touched, r.key)
		}
	}
	return n, nil
}

// replace seals one resource's new outputs and writes its audit row in one transaction, under the audit lock
// every writer holds, while its record is still ready; sealed is false for one a delete took meanwhile.
func replace(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault, key provisioners.Key, typ string, outputs map[string]string) (sealed bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return false, err
	}
	if ready, err := stillReady(ctx, tx, key, " FOR UPDATE"); err != nil || !ready {
		return false, err
	}
	if _, err := sealOutputs(ctx, tx, keys, key, typ, outputs); err != nil {
		return false, err
	}
	if err := events.AuditTx(ctx, tx, provisioner, "reissue resource password", key.Project, "", map[string]any{"name": key.Name, "type": typ, "env": key.Env}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// running prints each environment of keys that runs now, once, with the version it runs, named as every read
// names one (contracts/routes.md, Naming a version), and what brings it back: it keeps the old password until it is deployed again, as a secret change reaches an app.
func running(ctx context.Context, pool *pgxpool.Pool, keys []provisioners.Key, public string, out io.Writer) error {
	said, header := map[[2]string]bool{}, false
	for _, k := range keys {
		at := [2]string{k.Project, k.Env}
		if said[at] {
			continue
		}
		said[at] = true
		var runBy, version string
		err := pool.QueryRow(ctx, `SELECT d.run_by, coalesce(k.label, substr(k.id, 5)) FROM deploys d JOIN packages k ON k.id = d.package_id AND k.project_id = d.project_id
			WHERE d.project_id = $1 AND d.env = $2`, k.Project, k.Env).Scan(&runBy, &version)
		if errors.Is(err, pgx.ErrNoRows) { // nothing runs there: its next deploy reads the new URL
			continue
		}
		if err != nil {
			return err
		}
		if !header {
			_, _ = fmt.Fprintln(out, "These still run on the old password, until each is deployed again:")
			header = true
		}
		how := "Deploy again on " + public + "/#/apps/" + k.Project + ", or run canyon deploy from its code"
		if k.Env == "prod" {
			how = "once preview runs " + version + " again, run canyon deploy --env prod from its code"
		}
		if runBy == "org" {
			how += " (your platform runs it: that sends it a release with the new URL)"
		}
		_, _ = fmt.Fprintf(out, "  %s %s runs %s: %s\n", k.Project, k.Env, version, how)
	}
	return nil
}

// reissued is the command's count line, in a real plural (#2175).
func reissued(n int) string {
	if n == 1 {
		return "reissue-db-passwords: re-issued 1 app database password"
	}
	return fmt.Sprintf("reissue-db-passwords: re-issued %d app database passwords", n)
}
