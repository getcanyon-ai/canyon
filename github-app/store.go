package githubapp

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// App is the registered App as the courier uses it: its id and slug, the key it signs with, and
// the secret GitHub signs its deliveries with.
type App struct {
	ID            int64
	Slug          string
	Key           *rsa.PrivateKey
	WebhookSecret []byte
}

// store seals the key, the webhook secret and the client secret, each for its own row triple (0019_github.sql,
// 0093_github_links.sql), and writes
// the App's row with its audit row in one transaction after the append lock: keeps_code is what GitHub
// granted, contents write. A taken answer is why nothing was written: the org has an App, or another
// org keeps code.
func (g *Registration) store(ctx context.Context, conv Conversion, imports bool) (func(http.ResponseWriter), error) {
	keyRef, pemKey, sealedPEM, err := g.Vault.Seal("github", "app", "private_key", []byte(conv.PEM))
	if err != nil {
		return nil, err
	}
	_, hookKey, sealedSecret, err := g.Vault.Seal("github", "app", "webhook_secret", []byte(conv.WebhookSecret)) // the same active key: Load fixes it, so one key_ref covers all three
	if err != nil {
		return nil, err
	}
	_, clientKey, sealedClient, err := g.Vault.Seal("github", "app", "client_secret", []byte(conv.ClientSecret))
	if err != nil {
		return nil, err
	}
	keeps := conv.Permissions["contents"] == "write"
	tx, err := g.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return nil, err
	}
	if taken, err := g.taken(ctx, tx, conv.Owner.Login, keeps); err != nil || taken != nil {
		return taken, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account, keeps_code, imports_new, account_id, sealed_client_key, sealed_client_secret)
		VALUES (NULL, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`, conv.ID, conv.Slug, conv.ClientID, keyRef, pemKey, sealedPEM, hookKey, sealedSecret, conv.Owner.Login, keeps, imports, conv.Owner.ID, clientKey, sealedClient); err != nil {
		return nil, err
	}
	if err := events.AuditTx(ctx, tx, g.Actor(ctx), "github app registered", "", "", map[string]any{"app_id": conv.ID, "slug": conv.Slug, "account": conv.Owner.Login, "keeps_code": keeps, "imports_new": imports}); err != nil {
		return nil, err
	}
	return nil, tx.Commit(ctx)
}

// LoadApp reads the row of the App an installation is of and opens both seals; pgx.ErrNoRows when none is.
func LoadApp(ctx context.Context, q querier, v *vault.Vault, installation int64) (App, error) {
	var app App
	var keyRef int
	var pemKey, sealedPEM, hookKey, sealedSecret []byte
	if err := q.QueryRow(ctx, `SELECT a.app_id, a.slug, a.key_ref, a.sealed_pem_key, a.sealed_pem, a.sealed_hook_key, a.sealed_hook_secret
		FROM github_app a JOIN github_installations i USING (app_id) WHERE i.installation_id = $1`, installation).
		Scan(&app.ID, &app.Slug, &keyRef, &pemKey, &sealedPEM, &hookKey, &sealedSecret); err != nil {
		return App{}, err
	}
	raw, err := v.Open(keyRef, "github", "app", "private_key", pemKey, sealedPEM)
	if err != nil {
		return App{}, err
	}
	if app.Key, err = parseKey(raw); err != nil {
		return App{}, err
	}
	if app.WebhookSecret, err = v.Open(keyRef, "github", "app", "webhook_secret", hookKey, sealedSecret); err != nil {
		return App{}, err
	}
	return app, nil
}

// parseKey reads the PKCS#1 PEM GitHub issues an App's private key as.
func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, errors.New("github: the App's private key is not an RSA PRIVATE KEY PEM block")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// InstallationCreated records app installed on an account, the org that owns it by GitHub's numeric id
// (#1914): a new live row, or the old one live again, under the login the org has now. false: the account
// is not app's org, and nothing is written. An App from before account ids takes its org's id at its first
// installation there, and one that names no org yet takes the account it is installed on. An
// installation already another App's stays that App's.
func InstallationCreated(ctx context.Context, q querier, app, id int64, account string, accountID int64) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE github_app SET account = $2, account_id = $3 WHERE app_id = $1 AND $3 > 0
		AND (account_id = $3 OR (account_id IS NULL AND (account = '' OR lower(account) = lower($2))))
		AND NOT EXISTS (SELECT 1 FROM github_app o WHERE o.app_id <> $1 AND (o.account_id = $3 OR lower(o.account) = lower($2)))`, app, account, accountID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	tag, err = q.Exec(ctx, `INSERT INTO github_installations (installation_id, account, app_id) VALUES ($1, $2, $3)
		ON CONFLICT (installation_id) DO UPDATE SET account = EXCLUDED.account, deleted_at = NULL
		WHERE github_installations.app_id = EXCLUDED.app_id`, id, account, app)
	return err == nil && tag.RowsAffected() == 1, err
}

// InstallationDeleted marks app's installation uninstalled; deliveries naming it are refused from then on.
func InstallationDeleted(ctx context.Context, q querier, app, id int64) error {
	_, err := q.Exec(ctx, "UPDATE github_installations SET deleted_at = now() WHERE installation_id = $1 AND app_id = $2 AND deleted_at IS NULL", id, app)
	return err
}
