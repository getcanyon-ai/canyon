package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sealedRow is one sealed value, what it must open to, and the statement reading its key_ref, keys.
type sealedRow struct {
	project, env, name, want string
	sealedValue              []byte
	read                     string
}

// openAll opens every row as its reader does — under its own key_ref, a vault loaded fresh — and
// fails naming the first that does not open to its value or whose value ciphertext changed.
func openAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keyFile string, rows []sealedRow) {
	t.Helper()
	keys, err := vault.Load(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var keyRef int
		var sealedKey, sealedValue []byte
		if err := pool.QueryRow(ctx, r.read).Scan(&keyRef, &sealedKey, &sealedValue); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sealedValue, r.sealedValue) {
			t.Fatalf("%s/%s/%s: the value's ciphertext changed", r.project, r.env, r.name)
		}
		if got, err := keys.Open(keyRef, r.project, r.env, r.name, sealedKey, sealedValue); err != nil || string(got) != r.want {
			t.Fatalf("%s/%s/%s under key_ref %d: %q %v", r.project, r.env, r.name, keyRef, got, err)
		}
	}
}

// rotation is a migrated database holding project claims-portal, and a vault on a fresh key file.
func rotation(t *testing.T) (*pgxpool.Pool, context.Context, string, *vault.Vault) {
	t.Helper()
	pool, ctx := migrated(t)
	keyFile := filepath.Join(t.TempDir(), "kek")
	keys, err := vault.Load(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, keyFile, keys
}

// insertSecret seals value as claims-portal's env/name and writes it, key_ref NULL unless labelled.
func insertSecret(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault, env, name, value string, labelled bool) []byte {
	t.Helper()
	version, sealedKey, sealedValue, err := keys.Seal("claims-portal", env, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	var keyRef any = version
	if !labelled {
		keyRef = nil // written before the label: version 1
	}
	if _, err := pool.Exec(ctx, "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('claims-portal', $1, $2, 'runtime', $3, $4, $5, 'person', 'ana')", env, name, keyRef, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	return sealedValue
}

func TestRotateKEKMovesEverySealedRowAndNeverTheValue(t *testing.T) {
	pool, ctx, keyFile, keys := rotation(t)
	var rows []sealedRow
	for i, r := range [][3]string{{"preview", "DATABASE_URL", "postgres://a"}, {"prod", "STRIPE_KEY", "sk_live"}} {
		sealedValue := insertSecret(t, ctx, pool, keys, r[0], r[1], r[2], i > 0)
		rows = append(rows, sealedRow{"claims-portal", r[0], r[1], r[2], sealedValue, "SELECT coalesce(key_ref, 1), sealed_key, sealed_value FROM secrets WHERE name = '" + r[1] + "'"})
	}
	for app, account := range map[string]string{"1": "acme", "2": "acme-labs"} { // one App per org (#1914): every one moves
		version, pemKey, pem, err := keys.Seal("github", "app", "private_key", []byte("the app private key of "+account))
		if err != nil {
			t.Fatal(err)
		}
		_, hookKey, hook, err := keys.Seal("github", "app", "webhook_secret", []byte("whsec "+account))
		if err != nil {
			t.Fatal(err)
		}
		var clientKey, client []byte // acme-labs's App was registered before Canyon kept a client secret (#2228)
		if account == "acme" {
			if _, clientKey, client, err = keys.Seal("github", "app", "client_secret", []byte("client secret of "+account)); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, sealedRow{"github", "app", "client_secret", "client secret of " + account, client, "SELECT key_ref, sealed_client_key, sealed_client_secret FROM github_app WHERE app_id = " + app})
		}
		if _, err := pool.Exec(ctx, "INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account, sealed_client_key, sealed_client_secret) VALUES (NULL, $6, 'canyon-'||$7, 'Iv1', $1, $2, $3, $4, $5, $7, $8, $9)", version, pemKey, pem, hookKey, hook, app, account, clientKey, client); err != nil {
			t.Fatal(err)
		}
		rows = append(rows,
			sealedRow{"github", "app", "private_key", "the app private key of " + account, pem, "SELECT key_ref, sealed_pem_key, sealed_pem FROM github_app WHERE app_id = " + app},
			sealedRow{"github", "app", "webhook_secret", "whsec " + account, hook, "SELECT key_ref, sealed_hook_key, sealed_hook_secret FROM github_app WHERE app_id = " + app})
	}
	tx, err := pool.Begin(ctx) // the project's report key, as its first attach generates it
	if err != nil {
		t.Fatal(err)
	}
	reportKey, err := runs.ReportKey(ctx, tx, keys, "claims-portal")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var sealedReportKey []byte
	if err := pool.QueryRow(ctx, "SELECT report_sealed_value FROM projects").Scan(&sealedReportKey); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"claims-portal", "report", "key", string(reportKey), sealedReportKey, "SELECT report_key_ref, report_sealed_key, report_sealed_value FROM projects"})
	idKey, err := identityKey(ctx, pool, keys) // the identity token's signing key, as the first boot generates it
	if err != nil {
		t.Fatal(err)
	}
	der, err := idKey.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var sealedIDKey []byte
	if err := pool.QueryRow(ctx, "SELECT sealed_value FROM identity_keys").Scan(&sealedIDKey); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"identity", "key", idKey.ID, string(der), sealedIDKey, "SELECT key_ref, sealed_key, sealed_value FROM identity_keys"})
	version, credKey, cred, err := keys.Seal("scanner", "opengrep", "token", []byte("rules-reader-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO scanner_credentials (tool, repo, username, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES ('opengrep', 'https://github.com/m/r', 'u', $1, $2, $3, 'person', 'dana', 'Dana')", version, credKey, cred); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"scanner", "opengrep", "token", "rules-reader-token", cred, "SELECT key_ref, sealed_key, sealed_value FROM scanner_credentials"})
	version, doorKey, door, err := keys.Seal("front-door", "signin", "client_secret", []byte("oidc-client-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), '{"choice": 2}', $1, $2, $3, 'person', 'dana', 'Dana')`, version, doorKey, door); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"front-door", "signin", "client_secret", "oidc-client-secret", door, "SELECT key_ref, sealed_key, sealed_value FROM front_doors"})
	version, whKey, wh, err := keys.Seal("platform-webhook", "webhook", "secret", []byte("webhook-signing-secret"))
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("host-token"))
	if _, err := pool.Exec(ctx, "INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', $1, $2, $3, $4, 'person', 'dana', 'Dana')",
		version, whKey, wh, tokenHash[:]); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"platform-webhook", "webhook", "secret", "webhook-signing-secret", wh, "SELECT key_ref, sealed_key, sealed_value FROM platform_webhook"})
	version, repoKey, repoToken, err := keys.Seal("platform-repo", "repo", "token", []byte("deploy-repo-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO platform_repo (url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, token_hash, fetch_token_hash, set_by_kind, set_by_subject, set_by_display)
		VALUES ('https://github.com/meridian/deploy', 'x-access-token', $1, $2, $3, 'preview', 'main', 'pull_request', 'apps', 'github', 'https://api.github.com', 'canyon', sha256('host'::bytea), sha256('fetch'::bytea), 'person', 'dana', 'Dana')`, version, repoKey, repoToken); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"platform-repo", "repo", "token", "deploy-repo-token", repoToken, "SELECT key_ref, sealed_key, sealed_value FROM platform_repo"})
	version, relKey, rel, err := keys.Seal("claims-portal", "access-token", "rel_8f2a", []byte("release-access-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, key_ref, sealed_key, sealed_value, access_token_hash)
		VALUES ('rel_8f2a', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', 'registry.example/canyon/claims-portal@sha256:aa', gen_random_uuid(), 'accepted', now(), $1, $2, $3, '\x01')`, version, relKey, rel); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"claims-portal", "access-token", "rel_8f2a", "release-access-token", rel, "SELECT key_ref, sealed_key, sealed_value FROM releases"})

	if _, err := policyCert(ctx, pool, keys); err != nil { // the egress policy's certificate, as the first boot on Kubernetes mints it
		t.Fatal(err)
	}
	var policyKey []byte
	var policyKeyRef int
	var policyKeySealed []byte
	if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_value FROM egress_policy_cert").Scan(&policyKeyRef, &policyKeySealed, &policyKey); err != nil {
		t.Fatal(err)
	}
	policyDER, err := keys.Open(policyKeyRef, "egress", "policy", "tls_key", policyKeySealed, policyKey)
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"egress", "policy", "tls_key", string(policyDER), policyKey, "SELECT key_ref, sealed_key, sealed_value FROM egress_policy_cert"})
	version, routeKey, route, err := keys.Seal("claims-portal", "prod", "route:api.stripe.com", []byte("sk_live_"+strings.Repeat("x", 24))) // a value route's value (0124)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO egress_routes (project_id, env, destination, header, scheme, from_secret, rev, key_ref, sealed_key, sealed_value) VALUES ('claims-portal', 'prod', 'api.stripe.com', 'authorization', 'bearer', false, nextval('egress_route_rev'), $1, $2, $3)",
		version, routeKey, route); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"claims-portal", "prod", "route:api.stripe.com", "sk_live_" + strings.Repeat("x", 24), route, "SELECT key_ref, sealed_key, sealed_value FROM egress_routes"})
	seedRoot(t, ctx, pool, keys) // the interception root, as the first start serving the policy mints it
	var rootFingerprint string
	var rootKeyRef int
	var rootKeySealed, rootKey []byte
	if err := pool.QueryRow(ctx, "SELECT fingerprint, key_ref, sealed_key, sealed_value FROM egress_ca_roots").Scan(&rootFingerprint, &rootKeyRef, &rootKeySealed, &rootKey); err != nil {
		t.Fatal(err)
	}
	rootDER, err := keys.Open(rootKeyRef, "egress", "root", rootFingerprint, rootKeySealed, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"egress", "root", rootFingerprint, string(rootDER), rootKey, "SELECT key_ref, sealed_key, sealed_value FROM egress_ca_roots"})
	hookURL := "https://hooks.slack.com/services/T000/B000/" + strings.Repeat("X", 24) // a chat channel's webhook URL (0133), a bearer credential
	version, channelKey, channel, err := keys.Seal("integration", "platform-alerts", "webhook_url", []byte(hookURL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notify_channels (id, name, kind, host, tail, key_ref, sealed_key, sealed_url, set_by_kind, set_by_subject, set_by_display)
		VALUES ('platform-alerts', '#platform-alerts', 'slack', 'hooks.slack.com', 'XXXX', $1, $2, $3, 'person', 'dana', 'Dana')`, version, channelKey, channel); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, sealedRow{"integration", "platform-alerts", "webhook_url", hookURL, channel, "SELECT key_ref, sealed_key, sealed_url FROM notify_channels"})

	var last string // a migration this binary carries, not applied: refused before any key is written
	if err := pool.QueryRow(ctx, "DELETE FROM schema_migrations WHERE name = (SELECT max(name) FROM schema_migrations) RETURNING name").Scan(&last); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := rotateKEK(ctx, pool, keyFile, &out); err == nil || !strings.Contains(err.Error(), "1 migration(s) this binary carries are not applied") {
		t.Fatalf("a pending migration refuses the rotation: %v", err)
	}
	if _, err := os.Stat(keyFile + ".2"); !os.IsNotExist(err) {
		t.Fatalf("the refused rotation wrote a key: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", last); err != nil {
		t.Fatal(err)
	}

	for _, want := range []int{2, 3} { // the second run finds every row under 2 and moves them all to 3: the ledger, not the run, says what is left
		out.Reset()
		if err := rotateKEK(ctx, pool, keyFile, &out); err != nil {
			t.Fatalf("run to version %d: %v\n%s", want, err, out.String())
		}
		for _, line := range []string{
			"15 row(s) moved to key version " + strconv.Itoa(want) + ", 0 remaining", // two Apps, one row each
			"a database backup taken before this rotation still opens with it",
			"`canyon secret set NAME`",
		} {
			if !strings.Contains(out.String(), line) {
				t.Fatalf("run to version %d prints %q:\n%s", want, line, out.String())
			}
		}
		info, err := os.Stat(keyFile + "." + strconv.Itoa(want))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("kek.%d on the volume, 0600: %v %v", want, info, err)
		}
		openAll(t, ctx, pool, keyFile, rows)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("the rotation never deletes a key file: %v", err)
	}
}

func TestRotateKEKRefusesAKeyItGenerated(t *testing.T) {
	pool, ctx := migrated(t)
	keyFile := filepath.Join(t.TempDir(), "kek") // no key on the volume: it did not mount
	var out bytes.Buffer
	if err := rotateKEK(ctx, pool, keyFile, &out); err == nil || !strings.Contains(err.Error(), "no key file at "+keyFile) {
		t.Fatalf("a rotation with no key on the volume is refused: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(keyFile + ".2"); !os.IsNotExist(err) {
		t.Fatalf("the refused rotation wrote a key version: %v", err)
	}
}

// A row a PUT changed after the rotation read it stays behind, counted as remaining, so the old key is
// never reported free while a row still needs it: a secret, and the scanner credential.
func TestRotateKEKLeavesASecretAPutChangedAfterTheRead(t *testing.T) {
	putDuringRotation(t, "claims-portal", "prod", "API_TOKEN", "secrets", "name = 'API_TOKEN'",
		"INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('claims-portal', 'prod', 'API_TOKEN', 'runtime', $1, $2, $3, 'person', 'ana')")
}

func TestRotateKEKLeavesTheScannerCredentialAPutChangedAfterTheRead(t *testing.T) {
	putDuringRotation(t, "scanner", "opengrep", "token", "scanner_credentials", "tool = 'opengrep'",
		"INSERT INTO scanner_credentials (tool, repo, username, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES ('opengrep', 'https://github.com/m/r', 'u', $1, $2, $3, 'person', 'dana', 'Dana')")
}

// putDuringRotation writes project/env/name's value, sealed, with insert ($1 key_ref, $2 sealed_key, $3
// sealed_value), then rotates while a PUT changes the row where where holds, and fails unless the row
// stays behind on version 1, counted as remaining.
func putDuringRotation(t *testing.T, project, env, name, table, where, insert string) {
	pool, ctx, keyFile, keys := rotation(t)
	version, sealedKey, sealedValue, err := keys.Seal(project, env, name, []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, version, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	// The PUT: a new value under the server's booted version, its row lock held so the rotation
	// reads the old sealed key and blocks on the write until the PUT commits.
	_, putKey, putValue, err := keys.Seal(project, env, name, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	put, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = put.Rollback(ctx) }()
	if _, err := put.Exec(ctx, "UPDATE "+table+" SET sealed_key = $1, sealed_value = $2 WHERE "+where, putKey, putValue); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- rotateKEK(ctx, pool, keyFile, &out) }()
	for waiting := false; !waiting; {
		if len(done) > 0 {
			t.Fatalf("the rotation returned before it reached the PUT's row: %v\n%s", <-done, out.String())
		}
		if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE "+table+" SET sealed_key = $1, key_ref%')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := put.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "1 row(s) are not under key version 2") || !strings.Contains(out.String(), "0 row(s) moved to key version 2, 1 remaining") {
		t.Fatalf("the row the PUT changed stays behind, counted as remaining: %v\n%s", err, out.String())
	}
	// read only while the row is still on version 1: a row moved to 2 fails the read
	openAll(t, ctx, pool, keyFile, []sealedRow{{project, env, name, "new", putValue, "SELECT key_ref, sealed_key, sealed_value FROM " + table + " WHERE " + where + " AND key_ref = 1"}})
}

// reissue-db-passwords refuses, naming the setting, to run without the apps database's admin URL or the
// server's key file (contracts/provisioner.md, After a restore; #2014).
func TestReissueRefusesWithoutTheServersSettings(t *testing.T) {
	pool, ctx, keyFile, _ := rotation(t)
	env := map[string]string{}
	var out bytes.Buffer
	if err := reissueDBPasswords(ctx, pool, keyFile, func(k string) string { return env[k] }, &out); err == nil || !strings.Contains(err.Error(), "CANYON_APP_DATABASE_URL is not set") {
		t.Fatalf("without the apps database: %v", err)
	}
	env["CANYON_APP_DATABASE_URL"] = "postgres://canyon:canyon@127.0.0.1:1/canyon"
	if err := reissueDBPasswords(ctx, pool, filepath.Join(t.TempDir(), "kek"), func(k string) string { return env[k] }, &out); err == nil || !strings.Contains(err.Error(), "no key file at") {
		t.Fatalf("without the key volume: %v", err)
	}
}
