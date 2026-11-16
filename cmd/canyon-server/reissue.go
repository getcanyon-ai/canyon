package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reissueDBPasswords is `canyon-server reissue-db-passwords`, run beside the server with its environment
// after a restore without role passwords (contracts/provisioner.md, After a restore): every database the
// built-in driver made gets a fresh password and its app a new DATABASE_URL. Refused, naming the setting,
// without the apps database's admin URL or the key file, and while a migration this binary carries is unapplied.
func reissueDBPasswords(ctx context.Context, pool *pgxpool.Pool, keyFile string, getenv func(string) string, out io.Writer) error {
	pending, err := db.Pending(ctx, pool)
	if err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("reissue-db-passwords: %d migration(s) this binary carries are not applied: run it with the binary the server runs, after it has booted", pending)
	}
	appDB := getenv("CANYON_APP_DATABASE_URL")
	if appDB == "" {
		return errors.New("reissue-db-passwords: CANYON_APP_DATABASE_URL is not set: run it with the server's environment")
	}
	if _, err := os.Stat(keyFile); err != nil { //nolint:gosec // the host's own setting; checked before Load, which writes a first key that nothing else would open
		return fmt.Errorf("reissue-db-passwords: no key file at %s: run it where the server's key volume is mounted", keyFile)
	}
	keys, err := vault.Load(keyFile)
	if err != nil {
		return err
	}
	kube, err := kubernetesOf(getenv("CANYON_RUNTIME"))
	if err != nil {
		return err
	}
	caFile := getenv("CANYON_APP_DATABASE_CA_FILE")
	if _, err := databaseCAOf(caFile, appDB, kube); err != nil { // the URLs verify the server as the server's own do
		return err
	}
	builtin, err := provisioners.NewBuiltin(ctx, appDB, caFile)
	if err != nil {
		return err
	}
	defer builtin.Close()
	public, err := publicURLOf(cmp.Or(getenv("CANYON_PUBLIC_URL"), "http://localhost:7777")) // the server's own default
	if err != nil {
		return err
	}
	if _, err := runs.Reissue(ctx, pool, keys, builtin, public.Scheme+"://"+public.Host, out); err != nil {
		return fmt.Errorf("reissue-db-passwords: %w", err)
	}
	return nil
}
