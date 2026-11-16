package main

import (
	"net/http"
	"testing"
	"time"
)

// GET /api/v1/github is the orgs Canyon's Apps are on, oldest registration first, each with whether its
// App is installed and when it last heard from GitHub; a loopback host is one GitHub cannot reach (#1914).
func TestGitHubOrgsListsEachAppsOrgAndWhetherGitHubCanReachThisHost(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodGet, base+"/api/v1/github", ""); code != 200 || lastBody != `{"orgs":[],"reachable":false}` {
		t.Fatalf("no App: %d %s", code, lastBody)
	}
	for _, q := range []string{
		`INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account, keeps_code, imports_new, created_at)
			VALUES (NULL, 9002, 'canyon-acme-labs', 'c', 1, '', '', '', '', 'acme-labs', false, false, '2026-10-09T10:00:00Z'),
			       (NULL, 9001, 'canyon-acme', 'c', 1, '', '', '', '', 'acme', true, true, '2026-10-09T09:00:00Z')`,
		"INSERT INTO github_installations (installation_id, account, app_id) VALUES (42, 'acme', 9001), (43, 'acme-labs', 9002)",
		"UPDATE github_installations SET deleted_at = now() WHERE installation_id = 43",
		"UPDATE github_app SET sealed_client_key = '', sealed_client_secret = '' WHERE app_id = 9001", // registered since #2228: it links accounts
		"INSERT INTO github_deliveries (delivery_id, event, app_id, received_at) VALUES ('d1', 'push', 9001, '2026-10-09T11:00:00Z'), ('d2', 'push', 9001, '2026-10-09T11:30:00Z')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	want := `{"orgs":[{"account":"acme","app_slug":"canyon-acme","keeps_code":true,"imports_new":true,"installed":true,"last_event_at":"2026-10-09T11:30:00Z","links_accounts":true},` +
		`{"account":"acme-labs","app_slug":"canyon-acme-labs","keeps_code":false,"imports_new":false,"installed":false,"last_event_at":null,"links_accounts":false}],"reachable":false}`
	if code, _, _ := do(t, client(t), http.MethodGet, base+"/api/v1/github", ""); code != 200 || lastBody != want {
		t.Fatalf("%d\n got %s\nwant %s", code, lastBody, want)
	}
}
