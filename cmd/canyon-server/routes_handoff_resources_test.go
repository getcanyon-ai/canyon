package main

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The org's platform provisions under run_by: org and reports where each resource is (#2432,
// contracts/routes.md, the report row): these tests hold the report, the project read, the delete and
// the component types to it.

const providedEndpoint = "postgres://pg.data.meridian.example:5432/claims_portal_preview_db"

func deployedWith(resources string) string {
	return `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `","resources":` + resources + `}`
}

// declaringFixture is reportFixture with the release's package declaring db = postgres.
func declaringFixture(t *testing.T) (*pgxpool.Pool, context.Context, string, string, string) {
	t.Helper()
	pool, ctx, base, path, token := reportFixture(t)
	if _, err := pool.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres"}' WHERE id = 'pkg_7d3e8f'`); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, path, token
}

func endpointOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var e string
	if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(name || '=' || endpoint, ',' ORDER BY name), '') FROM platform_resources WHERE project_id = 'claims-portal'").Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Each endpoint the rules refuse — and an address with a query — is 400 invalid_report, and nothing is
// written: not the endpoints, not the environment. The report that keeps to them lands whole.
func TestEachEndpointRefusalIs400AndWritesNothing(t *testing.T) {
	pool, ctx, _, path, token := declaringFixture(t)
	c := client(t)
	for name, body := range map[string]string{
		"a password":          deployedWith(`[{"name":"db","endpoint":"postgres://app:hunter2@pg.example/claims"}]`),
		"a user":              deployedWith(`[{"name":"db","endpoint":"postgres://app@pg.example/claims"}]`),
		"a query":             deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/claims?sslmode=require"}]`),
		"a fragment":          deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/claims#x"}]`),
		"a percent-escape":    deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/cl%61ims"}]`),
		"a path token":        deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/claims/token-abc"}]`),
		"a bad scheme":        deployedWith(`[{"name":"db","endpoint":"s3://claims-files"}]`),
		"an unknown name":     deployedWith(`[{"name":"cache","endpoint":"postgres://pg.example/claims"}]`),
		"a duplicate":         deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/a"},{"name":"db","endpoint":"postgres://pg.example/b"}]`),
		"too many":            deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/a"},{"name":"files","endpoint":"s3://claims-files"}]`),
		"an extra key":        deployedWith(`[{"name":"db","endpoint":"postgres://pg.example/a","password":"x"}]`),
		"an address query":    `{"status":"deployed","address":"https://claims.apps.meridian.example/?token=x","digest":"sha256:` + reportDigest + `"}`,
		"an address userinfo": `{"status":"deployed","address":"https://u:p@claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`,
	} {
		if code, resp, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token)); code != 400 || resp["error"] != "invalid_report" {
			t.Errorf("%s: %d %s", name, code, lastBody)
		}
	}
	if e := endpointOf(t, ctx, pool); e != "" || count(t, ctx, pool, "SELECT count(*) FROM deploys") != 0 {
		t.Fatalf("a refused report wrote: endpoints %q, %d deploy row(s)", e, count(t, ctx, pool, "SELECT count(*) FROM deploys"))
	}
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 200 {
		t.Fatalf("a report keeping to the rules: %d %s", code, lastBody)
	}
	if e := endpointOf(t, ctx, pool); e != "db="+providedEndpoint {
		t.Fatalf("the endpoint written: %q", e)
	}
}

// Endpoints are written on the deployed record's own path alone: never on a 409 — a wrong digest, a
// superseded release, a next release's report without the key (#2452: resources_missing) — nor on the
// duplicate's 200 {} (#2432 review).
func TestEndpointsAreNeverWrittenOnA409OrADuplicate(t *testing.T) {
	pool, ctx, base, path, token := declaringFixture(t)
	c := client(t)
	wrong := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `","resources":[{"name":"db","endpoint":"` + providedEndpoint + `"}]}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, wrong, reportHeaders(token)); code != 409 || endpointOf(t, ctx, pool) != "" {
		t.Fatalf("a wrong digest: %d, endpoints %q", code, endpointOf(t, ctx, pool))
	}
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	if code, resp, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"postgres://elsewhere.example/claims"}]`), reportHeaders(token)); code != 200 || len(resp) != 0 || endpointOf(t, ctx, pool) != "db="+providedEndpoint {
		t.Fatalf("a duplicate: %d %s, endpoints %q", code, lastBody, endpointOf(t, ctx, pool))
	}
	if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_next', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4c', 'accepted', now())", reportImage); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"postgres://elsewhere.example/claims"}]`), reportHeaders(token)); code != 409 || endpointOf(t, ctx, pool) != "db="+providedEndpoint {
		t.Fatalf("a superseded release: %d, endpoints %q", code, endpointOf(t, ctx, pool))
	}
	next := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, body, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_next/report", next, reportHeaders(token)); code != 409 || body["error"] != "resources_missing" || endpointOf(t, ctx, pool) != "db="+providedEndpoint {
		t.Fatalf("the next release's report without the key: %d %s, endpoints %q", code, lastBody, endpointOf(t, ctx, pool))
	}
}

// What the platform provided shows on the project read and the delete's listing as the platform's; the
// delete's remove carries delete, and its removed report takes them off (contracts/routes.md, the delete row).
func TestADeleteListsWhatThePlatformProvidedAndItsRemovedTakesIt(t *testing.T) {
	pool, ctx, base, path, token := declaringFixture(t)
	c := client(t)
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); code != 200 ||
		!strings.Contains(lastBody, `{"name":"db","type":"postgres","env":"preview","state":"provided","endpoint":"`+providedEndpoint+`"}`) {
		t.Fatalf("the project read: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal", ""); code != 409 ||
		!strings.Contains(lastBody, `"resources":[{"name":"db","type":"postgres","env":"preview","state":"provided"}]`) {
		t.Fatalf("the delete's listing: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	pending, _ := body["pending"].([]any)
	if code != 202 || len(pending) != 1 {
		t.Fatalf("the confirmed delete: %d %s", code, lastBody)
	}
	release, _ := pending[0].(map[string]any)["release"].(string)
	var deletes bool
	if err := pool.QueryRow(ctx, "SELECT deletes_resources FROM releases WHERE id = $1", release).Scan(&deletes); err != nil || !deletes {
		t.Fatalf("the delete's remove deletes: %v (%v)", deletes, err)
	}
	reportRemoved(t, c, base, release, token)
	if e := endpointOf(t, ctx, pool); e != "" {
		t.Fatalf("the delete's removed left the endpoints: %q", e)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 {
		t.Fatalf("the delete, once removed: %d %s", code, lastBody)
	}
}

// Endpoints are technical detail (design/PERMISSIONS.md): an owner, a contributor and an admin read them on the
// project read; a viewer's and a no-access member's reduced rows carry no resources[], exactly the reduced shape,
// and no endpoint's host. Where Canyon's own record names the same env and name, both show, the record first:
// the switch moved no data, and a delete destroys both.
func TestProvidedEndpointsAreTheTeamsAlone(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	for _, q := range []string{
		`UPDATE packages SET resources = '{"db": "postgres", "files": "s3"}' WHERE project_id = 'claims-portal'`,
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
			SELECT 'rel_team', 'claims-portal', 'preview', 'gr_seed', id, 'deploy', '` + reportImage + `', '5f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b', 'deployed', now() FROM packages WHERE project_id = 'claims-portal'`,
		`INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ('claims-portal', 'preview', 'db', '` + providedEndpoint + `', 'rel_team'),
			('claims-portal', 'preview', 'files', 's3://claims-portal-files', 'rel_team')`,
		`INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'files', 's3', 'builtin', 'ready')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	const resources = `"resources":[{"name":"db","type":"postgres","env":"preview","state":"provided","endpoint":"` + providedEndpoint + `"},` +
		`{"name":"files","type":"s3","env":"preview","state":"ready"},{"name":"files","type":"s3","env":"preview","state":"provided","endpoint":"s3://claims-portal-files"}]`
	for who, c := range map[string]*http.Client{
		"the owner":     olive,
		"a contributor": signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false),
		"an admin":      signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false),
	} {
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); code != 200 || !strings.Contains(lastBody, resources) {
			t.Errorf("%s's read: %d %s", who, code, lastBody)
		}
	}
	reduced := []string{"access", "claimable", "id", "name", "owner_groups", "owners", "summary"}
	for who, c := range map[string]struct {
		client *http.Client
		keys   []string
	}{
		"a viewer":                {signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), append([]string{"envs"}, reduced...)},
		"a member with no access": {signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), reduced},
	} {
		code, body, _ := do(t, c.client, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		p, _ := body["project"].(map[string]any)
		keys := slices.Sorted(maps.Keys(p))
		if code != 200 || !slices.Equal(keys, slices.Sorted(slices.Values(c.keys))) || strings.Contains(lastBody, "pg.data.meridian.example") || strings.Contains(lastBody, "claims-portal-files") {
			t.Errorf("%s's reduced read: %d %s, keys %v", who, code, lastBody, keys)
		}
	}
}

// Under run_by: org every menu type is available — the platform provides it — and an upload declaring
// one no driver serves lands; back under canyon the type is planned again and such an upload is refused.
func TestUnderOrgEveryTypeIsAvailable(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	declaring := tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n\n[resources]\nfiles = { type = \"s3\" }\n"})
	if code, _ := upload(t, c, base, "claims-portal", declaring, "", ""); code != 400 {
		t.Fatalf("under canyon, s3 with no driver: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/component-types", ""); code != 200 ||
		!strings.Contains(lastBody, `"type":"postgres","class":"default","gives":["DATABASE_URL"],"status":"available"`) || !strings.Contains(lastBody, `"type":"s3","class":"default","gives":["STORAGE_URL"],"status":"available"`) {
		t.Fatalf("component types under org: %d %s", code, lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", declaring, "", ""); code != 201 {
		t.Fatalf("under org, s3: %d %s", code, lastBody)
	}
}
