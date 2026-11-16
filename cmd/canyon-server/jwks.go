package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jwksPath is the one path the apps' listener answers.
const jwksPath = "/.well-known/jwks.json"

// identityKey is the identity token's signing key: the newest identity_keys row (0034), opened
// from the vault, or on the first boot that finds none a new key, sealed as a secret is under the
// row ("identity", "key", kid). The table lock makes two first boots write one key.
func identityKey(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (*identity.Key, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE identity_keys IN EXCLUSIVE MODE"); err != nil {
		return nil, err
	}
	var kid string
	var version int
	var sealedKey, sealedValue []byte
	err = tx.QueryRow(ctx, "SELECT kid, key_ref, sealed_key, sealed_value FROM identity_keys ORDER BY created_at DESC LIMIT 1").Scan(&kid, &version, &sealedKey, &sealedValue)
	if err == nil {
		der, err := keys.Open(version, "identity", "key", kid, sealedKey, sealedValue)
		if err != nil {
			return nil, err
		}
		return identity.Parse(der)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	key, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	der, err := key.Marshal()
	if err != nil {
		return nil, err
	}
	if version, sealedKey, sealedValue, err = keys.Seal("identity", "key", key.ID, der); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO identity_keys (kid, key_ref, sealed_key, sealed_value) VALUES ($1, $2, $3, $4)", key.ID, version, sealedKey, sealedValue); err != nil {
		return nil, err
	}
	return key, tx.Commit(ctx)
}

// jwksAddrOf is CANYON_APPS_JWKS_ADDR, host:port an app reaches the server at on its own network
// (default canyon-server:7780, the name the executor joins it by): where to listen — that port on
// every interface, as app networks join after boot — and the URL /run hands as identity.jwks_url.
func jwksAddrOf(s string) (listen, url string, err error) {
	if s == "" {
		s = "canyon-server:7780"
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return "", "", fmt.Errorf("CANYON_APPS_JWKS_ADDR must be host:port, the name and port an app reaches the server at: %q", s)
	}
	return ":" + port, "http://" + s + jwksPath, nil
}

// jwksHandler answers GET jwksPath with the key set and everything else 404. Its listener is the
// one an app reaches, so it is not the API's: no session stage, no accept guard, no other route.
func jwksHandler(doc []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jwksPath || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
}
