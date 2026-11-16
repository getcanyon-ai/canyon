package githubapp

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The App an installation is of opens for the courier as GitHub answered it; an installation of no App
// registered here is ErrNoRows.
func TestLoadAppOpensTheAppTheInstallationIsOf(t *testing.T) {
	g, conv, pool := registration(t)
	_, state := begin(g)
	finish(g, state, onlyState(state), "good")
	if _, err := LoadApp(ctx, pool, g.Vault, 42); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an installation of no App here is ErrNoRows: %v", err)
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 42, "acme", 4242); err != nil || !ok {
		t.Fatal(ok, err)
	}
	app, err := LoadApp(ctx, pool, g.Vault, 42)
	if err != nil || app.ID != 9001 || app.Slug != "canyon-acme" || string(app.WebhookSecret) != conv.secret {
		t.Fatalf("the stored App does not open as it was registered: %+v %v", app, err)
	}
	if want, err := parseKey([]byte(conv.pem)); err != nil || !app.Key.Equal(want) {
		t.Fatalf("the key: %v", err)
	}
}

// An installation is upserted live on created, soft-deleted on deleted, and live again, under the
// account's current name, when the App is installed there anew.
func TestAnInstallationIsUpsertedAndSoftDeleted(t *testing.T) {
	g, _, pool := registration(t)
	_, state := begin(g)
	finish(g, state, onlyState(state), "good")
	live := func() int {
		return rows(t, pool, "SELECT count(*) FROM github_installations WHERE installation_id = 42 AND deleted_at IS NULL")
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 42, "acme", 4242); err != nil || !ok || live() != 1 {
		t.Fatalf("created: %v, %d live", err, live())
	}
	if err := InstallationDeleted(ctx, pool, 9001, 42); err != nil || live() != 0 || rows(t, pool, "SELECT count(*) FROM github_installations") != 1 {
		t.Fatalf("deleted: %v, %d live", err, live())
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 42, "ACME", 4242); err != nil || !ok || live() != 1 || rows(t, pool, "SELECT count(*) FROM github_installations WHERE account = 'ACME'") != 1 {
		t.Fatalf("installed again, the org's login as GitHub now spells it: %v, %d live", err, live())
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 43, "acme-corp", 999); err != nil || ok || rows(t, pool, "SELECT count(*) FROM github_installations") != 1 {
		t.Fatalf("an installation on an account the App is not the org's: %v %v", ok, err)
	}
}

// An App from before account ids takes its org's id at its first installation there, and then holds
// to it; one that names no org takes the account it is installed on, unless another App has it.
func TestAnAppFromBeforeAccountIdsTakesItsOrgsId(t *testing.T) {
	g, _, pool := registration(t)
	_, state := begin(g)
	finish(g, state, onlyState(state), "good")
	if _, err := pool.Exec(ctx, "UPDATE github_app SET account_id = NULL"); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		login string
		id    int64
		ok    bool
	}{{"other", 7, false}, {"acme", 4242, true}, {"acme", 9, false}} {
		if ok, err := InstallationCreated(ctx, pool, 9001, step.id*10, step.login, step.id); err != nil || ok != step.ok {
			t.Fatalf("%s %d: %v %v", step.login, step.id, ok, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE github_app SET account = '', account_id = NULL;
		INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account, account_id) VALUES (NULL, 9002, 's', 'c', 1, '', '', '', '', 'acme-labs', 4243)`); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 77, "acme-labs", 4243); err != nil || ok {
		t.Fatalf("another App's org: %v %v", ok, err)
	}
	if ok, err := InstallationCreated(ctx, pool, 9001, 78, "acme-ops", 4244); err != nil || !ok || rows(t, pool, "SELECT count(*) FROM github_app WHERE app_id = 9001 AND account = 'acme-ops' AND account_id = 4244") != 1 {
		t.Fatalf("an App naming no org takes the first: %v %v", ok, err)
	}
}
