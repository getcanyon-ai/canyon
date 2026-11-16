package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rotateKEK is `canyon-server rotate-kek`: every run writes a fresh key version beside the key file
// and moves every row not on it — the data key re-sealed, written with key_ref in one statement,
// the value's ciphertext untouched. No key file is deleted: remove one once no row names it.
// Not N-1 compatible, which is safe: v1 is single-writer, a rotation is an explicit operator act,
// and it is refused while db.Pending reports migrations this binary carries unapplied.
func rotateKEK(ctx context.Context, pool *pgxpool.Pool, keyFile string, out io.Writer) error {
	pending, err := db.Pending(ctx, pool)
	if err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("rotate-kek: %d migration(s) this binary carries are not applied: rotate with the binary the server runs, after it has booted", pending)
	}
	keys, err := vault.Load(keyFile)
	if err != nil {
		return err
	}
	if keys.Generated { // Load wrote a first key: the key volume is not mounted here
		return fmt.Errorf("rotate-kek: no key file at %s: run it where the server's key volume is mounted", keyFile)
	}
	version, err := keys.AddVersion(keyFile)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "rotate-kek: wrote key version %d to %s.%d\n", version, keyFile, version)
	moved := 0
	for _, rewrap := range []func(context.Context, *pgxpool.Pool, *vault.Vault) (int, error){rewrapSecrets, rewrapGitHubApp, rewrapReportKeys, rewrapIdentityKeys,
		rewrapOne("scanner_credentials", "scanner", "opengrep", "token"), rewrapOne("front_doors", "front-door", "signin", "client_secret"),
		rewrapOne("platform_webhook", "platform-webhook", "webhook", "secret"), rewrapOne("platform_repo", "platform-repo", "repo", "token"), rewrapReleaseTokens, rewrapOne("egress_policy_cert", "egress", "policy", "tls_key"), rewrapRoutes, rewrapRoots, rewrapChannels} {
		var n int
		n, err = rewrap(ctx, pool, keys)
		moved += n
		if err != nil {
			break
		}
	}
	var remaining int
	if countErr := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM secrets WHERE coalesce(key_ref, 1) <> $1) + (SELECT count(*) FROM github_app WHERE key_ref <> $1)
		+ (SELECT count(*) FROM projects WHERE report_key_ref <> $1) + (SELECT count(*) FROM identity_keys WHERE key_ref <> $1)
		+ (SELECT count(*) FROM scanner_credentials WHERE key_ref <> $1) + (SELECT count(*) FROM front_doors WHERE key_ref <> $1)
		+ (SELECT count(*) FROM platform_webhook WHERE key_ref <> $1) + (SELECT count(*) FROM platform_repo WHERE key_ref <> $1) + (SELECT count(*) FROM releases WHERE key_ref <> $1)
		+ (SELECT count(*) FROM egress_policy_cert WHERE key_ref <> $1) + (SELECT count(*) FROM egress_routes WHERE key_ref <> $1) + (SELECT count(*) FROM egress_ca_roots WHERE key_ref <> $1)
		+ (SELECT count(*) FROM notify_channels WHERE key_ref <> $1)`, version).Scan(&remaining); countErr != nil && err == nil {
		err = countErr
	}
	_, _ = fmt.Fprintf(out, "rotate-kek: %d row(s) moved to key version %d, %d remaining\n", moved, version, remaining)
	if err == nil && remaining > 0 { // a row written while the rotation ran: still under the old version
		err = fmt.Errorf("%d row(s) are not under key version %d", remaining, version)
	}
	if err != nil {
		return fmt.Errorf("rotate-kek: %w; run it again: it writes a fresh key version and moves every row not on it", err)
	}
	_, _ = fmt.Fprintf(out, "rotate-kek: the running server opens rows under key version %d without a restart, but seals under the version it booted with until it restarts: a value set before then is counted as remaining, and running rotate-kek again moves it.\n", version)
	_, _ = fmt.Fprintln(out, "rotate-kek: re-wrapping moves live rows off the old key, but a database backup taken before this rotation still opens with it: if a key was compromised, replace each value at its source with `canyon secret set NAME`.")
	return nil
}

// rewrapSecrets moves every secrets row off older versions. The update matches the sealed key it
// read, so a PUT between read and write is not overwritten: that row stays behind, as remaining.
func rewrapSecrets(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		project, env, name string
		version            int
		sealedKey          []byte
	}
	rows, err := pool.Query(ctx, "SELECT project_id, env, name, coalesce(key_ref, 1), sealed_key FROM secrets WHERE coalesce(key_ref, 1) <> $1", keys.Active()) // key_ref NULL is version 1
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.project, &r.env, &r.name, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, r.project, r.env, r.name, r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE secrets SET sealed_key = $1, key_ref = $2 WHERE project_id = $3 AND env = $4 AND name = $5 AND sealed_key = $6", sealedKey, version, r.project, r.env, r.name, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapGitHubApp moves every GitHub App's row, one per org (#1914): each row's private key's, webhook
// secret's and client secret's data keys share one key_ref (0019, 0093), so all are re-wrapped and written
// with it in one statement; an App registered before #2228 has no client secret.
func rewrapGitHubApp(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	moved := 0
	for {
		var from int
		var pemKey, hookKey, clientKey []byte
		err := pool.QueryRow(ctx, "SELECT key_ref, sealed_pem_key, sealed_hook_key, sealed_client_key FROM github_app WHERE key_ref <> $1 LIMIT 1", keys.Active()).Scan(&from, &pemKey, &hookKey, &clientKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return moved, nil
		}
		if err != nil {
			return moved, err
		}
		version, newPemKey, err := keys.Rewrap(from, "github", "app", "private_key", pemKey)
		if err != nil {
			return moved, err
		}
		_, newHookKey, err := keys.Rewrap(from, "github", "app", "webhook_secret", hookKey)
		if err != nil {
			return moved, err
		}
		var newClientKey []byte
		if clientKey != nil {
			if _, newClientKey, err = keys.Rewrap(from, "github", "app", "client_secret", clientKey); err != nil {
				return moved, err
			}
		}
		tag, err := pool.Exec(ctx, "UPDATE github_app SET sealed_pem_key = $1, sealed_hook_key = $2, sealed_client_key = $6, key_ref = $3 WHERE sealed_pem_key = $4 AND sealed_hook_key = $5", newPemKey, newHookKey, version, pemKey, hookKey, newClientKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
}

// rewrapOne moves the sealed value of a table of one row sealed as the row (project, env, name), as
// rewrapGitHubApp moves each of its rows: the scanner credential's token (0036), the front door's client
// secret (0037), the egress policy's certificate key (0096).
func rewrapOne(table, project, env, name string) func(context.Context, *pgxpool.Pool, *vault.Vault) (int, error) {
	return func(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
		var from int
		var sealedKey []byte
		err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key FROM "+table+" WHERE key_ref <> $1", keys.Active()).Scan(&from, &sealedKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		version, rewrapped, err := keys.Rewrap(from, project, env, name, sealedKey)
		if err != nil {
			return 0, err
		}
		tag, err := pool.Exec(ctx, "UPDATE "+table+" SET sealed_key = $1, key_ref = $2 WHERE sealed_key = $3", rewrapped, version, sealedKey)
		return int(tag.RowsAffected()), err
	}
}

// rewrapReportKeys moves every project's report key (0031) off older versions, as rewrapSecrets
// moves a secret: the update matches the sealed key it read.
func rewrapReportKeys(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		project   string
		version   int
		sealedKey []byte
	}
	rows, err := pool.Query(ctx, "SELECT id, report_key_ref, report_sealed_key FROM projects WHERE report_key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.project, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, r.project, "report", "key", r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE projects SET report_sealed_key = $1, report_key_ref = $2 WHERE id = $3 AND report_sealed_key = $4", sealedKey, version, r.project, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapIdentityKeys moves every identity token signing key (0034) off older versions, as
// rewrapSecrets moves a secret: the update matches the sealed key it read.
func rewrapIdentityKeys(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		kid       string
		version   int
		sealedKey []byte
	}
	rows, err := pool.Query(ctx, "SELECT kid, key_ref, sealed_key FROM identity_keys WHERE key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.kid, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, "identity", "key", r.kid, r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE identity_keys SET sealed_key = $1, key_ref = $2 WHERE kid = $3 AND sealed_key = $4", sealedKey, version, r.kid, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapRoutes moves every value route's value (0124) off older versions, as rewrapSecrets moves a secret: each sealed
// as the row (app, env, "route:<destination>"), the update matching the sealed key it read.
func rewrapRoutes(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		project, env, destination string
		version                   int
		sealedKey                 []byte
	}
	rows, err := pool.Query(ctx, "SELECT project_id, env, destination, key_ref, sealed_key FROM egress_routes WHERE key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.project, &r.env, &r.destination, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, r.project, r.env, "route:"+r.destination, r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE egress_routes SET sealed_key = $1, key_ref = $2 WHERE project_id = $3 AND env = $4 AND destination = $5 AND sealed_key = $6", sealedKey, version, r.project, r.env, r.destination, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapChannels moves every chat channel's webhook URL (0133) off older versions, each sealed as ("integration", <id>,
// "webhook_url"), the update matching the sealed key it read.
func rewrapChannels(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		id        string
		version   int
		sealedKey []byte
	}
	rows, err := pool.Query(ctx, "SELECT id, key_ref, sealed_key FROM notify_channels WHERE key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.id, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, "integration", r.id, "webhook_url", r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE notify_channels SET sealed_key = $1, key_ref = $2 WHERE id = $3 AND sealed_key = $4", sealedKey, version, r.id, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapRoots moves every interception root's key (0124) off older versions, each sealed as ("egress", "root",
// <fingerprint>), the update matching the sealed key it read.
func rewrapRoots(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		fingerprint string
		version     int
		sealedKey   []byte
	}
	rows, err := pool.Query(ctx, "SELECT fingerprint, key_ref, sealed_key FROM egress_ca_roots WHERE key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.fingerprint, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, "egress", "root", r.fingerprint, r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE egress_ca_roots SET sealed_key = $1, key_ref = $2 WHERE fingerprint = $3 AND sealed_key = $4", sealedKey, version, r.fingerprint, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}

// rewrapReleaseTokens moves every release's access token (0043) off older versions, as rewrapSecrets
// moves a secret: the update matches the sealed key it read. A release with no token yet has no
// key_ref and is not read.
func rewrapReleaseTokens(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (int, error) {
	type row struct {
		id, project string
		version     int
		sealedKey   []byte
	}
	rows, err := pool.Query(ctx, "SELECT id, project_id, key_ref, sealed_key FROM releases WHERE key_ref <> $1", keys.Active())
	if err != nil {
		return 0, err
	}
	todo, err := pgx.CollectRows(rows, func(rs pgx.CollectableRow) (r row, err error) {
		return r, rs.Scan(&r.id, &r.project, &r.version, &r.sealedKey)
	})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, r := range todo {
		version, sealedKey, err := keys.Rewrap(r.version, r.project, "access-token", r.id, r.sealedKey)
		if err != nil {
			return moved, err
		}
		tag, err := pool.Exec(ctx, "UPDATE releases SET sealed_key = $1, key_ref = $2 WHERE id = $3 AND sealed_key = $4", sealedKey, version, r.id, r.sealedKey)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}
