package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	putRoute    = "PUT /api/v1/projects/{id}/secrets/{name}"
	deleteRoute = "DELETE /api/v1/projects/{id}/secrets/{name}"
)

func TestPutSecretSealsTheRowAndTheValueAppearsNowhere(t *testing.T) {
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	value := "line one\nline two" // lines are fine (contracts/routes.md): a PEM key spans them
	code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"line one\nline two"}`)
	if code != 204 || lastBody != "" {
		t.Fatalf("PUT: %d %s", code, lastBody)
	}
	var env, scope, kind, subject string
	var version, size int
	var carries bool
	if err := pool.QueryRow(ctx, "SELECT env, scope, version, octet_length(sealed_value), set_by_kind, set_by_subject, position(convert_to($1, 'UTF8') IN sealed_value) > 0 OR position(convert_to($1, 'UTF8') IN sealed_key) > 0 FROM secrets", value).
		Scan(&env, &scope, &version, &size, &kind, &subject, &carries); err != nil || env != "preview" || scope != "runtime" || version != 1 || size != len(value)+vault.Overhead || kind != "person" || subject != "anonymous" || carries {
		t.Fatalf("the row: env %q where %q version %d size %d by %s/%s plaintext %v (%v)", env, scope, version, size, kind, subject, carries, err)
	}
	var sealedKey, sealedValue []byte // the row opens again, under the server's own key, to exactly the value: the next PR's readers depend on it
	if err := pool.QueryRow(ctx, "SELECT sealed_key, sealed_value FROM secrets").Scan(&sealedKey, &sealedValue); err != nil {
		t.Fatal(err)
	}
	if got, err := vaultOf(t, base).Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealedValue); err != nil || string(got) != value {
		t.Fatalf("the stored row opens to the value: %q %v", got, err)
	}
	var detail, project string
	if err := pool.QueryRow(ctx, "SELECT detail::text, project_id FROM audit_events WHERE action = $1", putRoute).Scan(&detail, &project); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(detail), &got); err != nil || project != "claims-portal" || len(got) != 3 || got["name"] != "DATABASE_URL" || got["env"] != "preview" || got["where"] != "runtime" {
		t.Fatalf("the audit row names name, env and where, never the value: %s on %q (%v)", detail, project, err)
	}
	var rows string
	if err := pool.QueryRow(ctx, "SELECT string_agg(data::text, ' ') FROM timeline_rows").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"audit_events": detail, "timeline_rows": rows, "the log": logs.String()} {
		if strings.Contains(text, "line one") {
			t.Fatalf("the value reached %s:\n%s", where, text)
		}
	}
	// A second PUT of the same name overwrites the one row, bumps the version, and may move it to the build.
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v2","where":"build"}`); code != 204 {
		t.Fatalf("second PUT: %d %s", code, lastBody)
	}
	if err := pool.QueryRow(ctx, "SELECT scope, version, octet_length(sealed_value) FROM secrets").Scan(&scope, &version, &size); err != nil || scope != "build" || version != 2 || size != 2+vault.Overhead {
		t.Fatalf("after the second PUT: where %q version %d size %d (%v)", scope, version, size, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets"); n != 1 {
		t.Fatalf("no history: %d rows", n)
	}
	// prod is its own row, written only when named.
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v3","env":"prod"}`); code != 204 {
		t.Fatalf("prod PUT: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets WHERE env = 'prod' AND version = 1 AND scope = 'runtime'"); n != 1 {
		t.Fatalf("the prod row: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = '"+putRoute+"'"); n != 3 {
		t.Fatalf("one audit row per PUT: %d", n)
	}
}

func TestBootRefusesAGeneratedKeyOverSealedRows(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	fresh, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil || !fresh.Generated {
		t.Fatal(err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err != nil {
		t.Fatalf("a first boot with no secrets is fine: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app (app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret) VALUES (1, 's', 'c', 2, '', '', '', '');
		INSERT INTO scanner_credentials (tool, repo, username, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES ('opengrep', 'https://g/r', 'u', 2, '', '', 'person', 'd', 'D');
		INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), '{"choice": 2}', 2, '', '', 'person', 'd', 'D');
		INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', 2, '', '', '', 'person', 'd', 'D');
		INSERT INTO platform_repo (url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, token_hash, fetch_token_hash, set_by_kind, set_by_subject, set_by_display) VALUES ('https://github.com/meridian/deploy', 'x-access-token', 2, '', '', 'preview', 'main', 'pull_request', 'apps', 'github', 'https://api.github.com', 'canyon', sha256('host'::bytea), sha256('fetch'::bytea), 'person', 'd', 'D');
		INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, key_ref, sealed_key, sealed_value, access_token_hash) VALUES ('rel_8f2a', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', 'i', gen_random_uuid(), 'accepted', now(), 2, '', '', '\x01');
		UPDATE projects SET report_key_ref = 2, report_sealed_key = '', report_sealed_value = '' WHERE id = 'claims-portal';
		INSERT INTO identity_keys (kid, key_ref, sealed_key, sealed_value) VALUES ('k1', 2, '', '')`); err != nil {
		t.Fatal(err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err == nil { // the GitHub App's key and webhook secret, the scanner credential, the front door's client secret, the platform webhook's secret, the deploy repo's token and a release's access token are sealed rows too
		t.Fatal("a generated key over the GitHub App's sealed row serves")
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "1 GitHub App under key version 2") || !strings.Contains(err.Error(), "1 scanner credential under key version 2") ||
		!strings.Contains(err.Error(), "1 front door client secret under key version 2") || !strings.Contains(err.Error(), "1 platform webhook secret under key version 2") ||
		!strings.Contains(err.Error(), "1 release access token(s) under key version 2") || !strings.Contains(err.Error(), "1 project report key(s) under key version 2") ||
		!strings.Contains(err.Error(), "1 identity key(s) under key version 2") || !strings.Contains(err.Error(), "1 deploy repo token under key version 2") {
		t.Fatalf("the GitHub App's row, the scanner credential, the front door's client secret, the platform webhook's secret and a release's access token under a missing version refuse the boot by name: %v", err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`); code != 204 {
		t.Fatalf("PUT: %d %s", code, lastBody)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "9 row(s) are sealed under another key") {
		t.Fatalf("a generated key over sealed rows is a missing key volume, not a first boot: %v", err)
	}
	fresh.Generated = false
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err != nil {
		t.Fatalf("a key read from disk is the one the rows were sealed under: %v", err)
	}
}

func TestListingSecretsShowsNamesAndNeverValues(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"value-list-3c9d"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"value-list-5e1a","env":"prod"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_KEY", `{"value":"value-list-8d2c"}`)                 // preview, sorts before DATABASE_URL though set after it
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_KEY", `{"value":"value-list-8d2c","where":"build"}`) // set again: version 2, where as stored
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	var sealed []byte
	if err := pool.QueryRow(ctx, "SELECT sealed_value FROM secrets WHERE name = 'DATABASE_URL'").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/secrets", "")
	if code != 200 || strings.Contains(lastBody, "value-list") || strings.Contains(lastBody, hex.EncodeToString(sealed)[:16]) || strings.Contains(lastBody, base64.StdEncoding.EncodeToString(sealed)[:16]) {
		t.Fatalf("the list carries names, never a value, sealed or not: %d %s", code, lastBody)
	}
	var out struct {
		Secrets []map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(lastBody), &out); err != nil || len(out.Secrets) != 3 || out.Secrets[0]["name"] != "API_KEY" || out.Secrets[1]["name"] != "DATABASE_URL" || out.Secrets[2]["name"] != "STRIPE_SECRET_KEY" {
		t.Fatalf("three rows, env then name: %s (%v)", lastBody, err)
	}
	if again := out.Secrets[0]; again["version"] != float64(2) || again["where"] != "build" {
		t.Fatalf("a row set twice lists its version and where as stored: %v", again)
	}
	first, third := out.Secrets[1], out.Secrets[2]
	setBy, _ := first["set_by"].(map[string]any)
	setAt, _ := first["set_at"].(string)
	if _, err := time.Parse(time.RFC3339, setAt); err != nil || !strings.HasSuffix(setAt, "Z") || len(first) != 7 || first["name"] != "DATABASE_URL" || first["env"] != "preview" || first["version"] != float64(1) || first["origin"] != "user" || first["where"] != "runtime" || setBy["kind"] != "person" || setBy["subject"] != "anonymous" || setBy["display"] != "" {
		t.Fatalf("the element (contracts/routes.md row 81), set_at in UTC: %v (%v)", first, err)
	}
	if third["name"] != "STRIPE_SECRET_KEY" || third["env"] != "prod" || third["where"] != "runtime" {
		t.Fatalf("the prod row: %v", third)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("a list is a read: %d audit rows written", n-before)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/quote-engine/secrets", ""); code != 200 || lastBody != `{"secrets":[]}` {
		t.Fatalf("a project with no secrets lists an empty array, never null: %d %s", code, lastBody)
	}
	if code, b, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/nothing/secrets", ""); code != 404 || b["error"] != "not_found" {
		t.Fatalf("unknown project: %d %s", code, lastBody)
	}
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	if code, b, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/secrets", ""); code != 404 || b["error"] != "not_found" {
		t.Fatalf("a deleted project lists nothing: %d %s", code, lastBody)
	}
}

func TestDeletingASecretRemovesOneRowAndIsAudited(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v","env":"prod"}`)
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", ""); code != 204 || lastBody != "" {
		t.Fatalf("DELETE (preview by default): %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets WHERE env = 'prod'"); n != 1 {
		t.Fatalf("the prod row stays: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets"); n != 1 {
		t.Fatalf("one row removed: %d left", n)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = $1", deleteRoute).Scan(&detail); err != nil || len(detail) != 2 || detail["name"] != "DATABASE_URL" || detail["env"] != "preview" {
		t.Fatalf("the audit row names name and env: %v (%v)", detail, err)
	}
	if code, b, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", ""); code != 404 || b["error"] != "not_found" {
		t.Fatalf("a second DELETE: %d %s", code, lastBody)
	}
	if code, b, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL?env=staging", ""); code != 400 || b["error"] != "invalid_env" {
		t.Fatalf("a bad env: %d %s", code, lastBody)
	}
	if code, b, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/nothing/secrets/DATABASE_URL", ""); code != 404 || b["error"] != "not_found" {
		t.Fatalf("unknown project: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = '"+deleteRoute+"'"); n != 1 {
		t.Fatalf("a refused DELETE is a log line, not an audit row: %d rows", n)
	}
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	if code, b, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL?env=prod", ""); code != 404 || b["error"] != "not_found" {
		t.Fatalf("a deleted project's row is not removable, and not audited: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets"); n != 0 {
		t.Fatalf("the project's delete takes its prod row with it: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = '"+deleteRoute+"'"); n != 1 {
		t.Fatalf("audit rows: %d, want the one removal", n)
	}
}

func TestPutSecretRefusesWhatTheContractRefuses(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	long := strings.Repeat("x", maxSecretValue+1)
	for _, tc := range []struct{ project, name, body, code string }{
		{"claims-portal", "1BAD", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "PORT", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "CANYON_IDENTITY_ISSUER", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "CANYON_IDENTITY_ANYTHING", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "PATH", `{"value":"v"}`, "invalid_name"}, // the runtime's own (#1256)
		{"claims-portal", "LD_PRELOAD", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "LD_LIBRARY_PATH", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "CNB_APP_DIR", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "A-B", `{"value":"v"}`, "invalid_name"},
		{"claims-portal", strings.Repeat("A", 129), `{"value":"v"}`, "invalid_name"},
		{"claims-portal", "DATABASE_URL", `{}`, "invalid_value"},
		{"claims-portal", "DATABASE_URL", `{"value":null}`, "invalid_value"},
		{"claims-portal", "DATABASE_URL", `{"value":"a\u0000b"}`, "invalid_value"},
		{"claims-portal", "DATABASE_URL", `{"value":"` + long + `"}`, "invalid_value"},
		{"claims-portal", "DATABASE_URL", `{"value":"v","env":"staging"}`, "invalid_env"},
		{"claims-portal", "DATABASE_URL", `{"value":"v","where":"both"}`, "invalid_where"},
		{"claims-portal", "DATABASE_URL", `{"value":"v","env":"prod","where":"build"}`, "invalid_where"}, // production runs the image preview built (#765)
		{"nothing", "DATABASE_URL", `{"value":"v"}`, "not_found"},
	} {
		code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/"+tc.project+"/secrets/"+tc.name, tc.body)
		want := 400
		if tc.code == "not_found" {
			want = 404
		}
		if code != want || body["error"] != tc.code {
			t.Errorf("%s %s %.40s: %d %s, want %d %s", tc.project, tc.name, tc.body, code, lastBody, want, tc.code)
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/"+strings.Repeat("A", 128), `{"value":"v"}`); code != 204 {
		t.Fatalf("a 128-character name is the longest allowed: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 {
		t.Fatalf("delete the project: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("a deleted project takes no secret: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets"); n != 0 {
		t.Fatalf("the refused PUTs stored rows, or the delete kept the 128-character name: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = '"+putRoute+"'"); n != 1 {
		t.Fatalf("a refused PUT is a log line, not an audit row: %d rows, want the one accepted PUT's", n)
	}
}

func TestPutSecretHoldsOneEnvironmentToOneMiB(t *testing.T) {
	_, _, base, c := withProject(t)
	full := `{"value":"` + strings.Repeat("x", maxSecretValue) + `"}` // exactly the cap: sixteen of them are exactly 1 MiB
	for i := range 16 {
		if code, _, _ := do(t, c, http.MethodPut, fmt.Sprintf("%s/api/v1/projects/claims-portal/secrets/BIG_%d", base, i), full); code != 204 {
			t.Fatalf("value %d: %d %s", i, code, lastBody)
		}
	}
	if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/ONE_MORE", `{"value":"x"}`); code != 413 || body["error"] != "too_large" {
		t.Fatalf("the seventeenth byte over 1 MiB: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/BIG_3", full); code != 204 {
		t.Fatalf("overwriting a row counts the row once: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/ONE_MORE", `{"value":"x","env":"prod"}`); code != 204 {
		t.Fatalf("the cap is per environment: %d %s", code, lastBody)
	}
}

// secretsGolden is contracts/testdata/secrets/claims-portal.json, the whole answer of routes.md row
// 176. Until it existed nothing cross-checked this shape: a field renamed here would leave this
// suite and the CLI's green while the binary printed blanks.
func secretsGolden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/secrets/claims-portal.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// seedSecrets writes the golden's rows by hand. set_at and version are a row's history, and a run
// of PUTs cannot reproduce either to the second. The sealed columns carry one byte that is not a
// value and that this route never reads: it answers names.
func seedSecrets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rows []any) {
	t.Helper()
	for _, row := range rows {
		r, _ := row.(map[string]any)
		by, _ := r["set_by"].(map[string]any)
		at, err := time.Parse(time.RFC3339, r["set_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO secrets (project_id, env, name, scope, version, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display, set_at)
			VALUES ('claims-portal', $1, $2, $3, $4, $5, $5, $6, $7, $8, $9)`,
			r["env"], r["name"], r["where"], int(r["version"].(float64)), []byte{0}, by["kind"], by["subject"], by["display"], at); err != nil {
			t.Fatal(err)
		}
	}
}

// The route answers the contract's golden whole: every field name, the server's env-then-name order,
// and nothing else. The golden's names are deliberately not in name order, so a list that sorted
// itself would come back rearranged here.
func TestListingSecretsAnswersTheGoldenList(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	want := secretsGolden(t)
	seedSecrets(t, ctx, pool, want["secrets"].([]any))
	code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/secrets", "")
	if code != 200 || !reflect.DeepEqual(got, want) {
		t.Fatalf("GET secrets answered %d %s,\nwant contracts/testdata/secrets/claims-portal.json", code, lastBody)
	}
}

// A row naming a key version the store did not load fails the boot by version and count; with
// version 1 gone from the volume the boot serves while no row needs it and refuses once one does.
func TestBootRefusesAVersionItCannotLoad(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`); code != 204 {
		t.Fatalf("PUT: %d %s", code, lastBody)
	}
	keys := vaultOf(t, base)
	if err := refuseRowsUnderAMissingKey(ctx, pool, keys); err != nil {
		t.Fatalf("every row under the loaded key: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE secrets SET key_ref = 2"); err != nil {
		t.Fatal(err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, keys); err == nil || !strings.Contains(err.Error(), "1 secret(s) under key version 2") {
		t.Fatalf("a row under a version no file holds refuses the boot by name: %v", err)
	}
	// A table under two versions, one of them loaded: any missing version refuses, not only all.
	if _, err := pool.Exec(ctx, "INSERT INTO secrets (project_id, env, name, scope, sealed_key, sealed_value, key_ref, set_by_kind, set_by_subject) SELECT project_id, env, name || '_TWIN', scope, sealed_key, sealed_value, 1, set_by_kind, set_by_subject FROM secrets"); err != nil {
		t.Fatal(err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, keys); err == nil || !strings.Contains(err.Error(), "under key version 2") || strings.Contains(err.Error(), "version 1") {
		t.Fatalf("one row under a loaded version does not excuse one under a missing one: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM secrets WHERE name LIKE '%_TWIN'"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() // version 1 removed after a rotation: kek.2 alone on the volume
	if _, err := vault.Load(filepath.Join(dir, "kek")); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 32)
	if _, err := rand.Read(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kek.2"), second, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "kek")); err != nil {
		t.Fatal(err)
	}
	twoOnly, err := vault.Load(filepath.Join(dir, "kek"))
	if err != nil || twoOnly.Generated || twoOnly.Has(1) || twoOnly.Active() != 2 {
		t.Fatalf("kek.2 alone is not a first boot: generated %v, has 1 %v, active %d (%v)", twoOnly.Generated, twoOnly.Has(1), twoOnly.Active(), err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, twoOnly); err != nil {
		t.Fatalf("nothing was generated, so nothing to refuse: %v", err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, twoOnly); err != nil {
		t.Fatalf("every row is under 2, so version 1 is not needed: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE secrets SET key_ref = NULL"); err != nil { // a row written before the label is version 1
		t.Fatal(err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, twoOnly); err == nil || !strings.Contains(err.Error(), "1 secret(s) under key version 1") {
		t.Fatalf("a row from before the label still needs version 1: %v", err)
	}
}

// With a second key file on the volume the PUT seals under it and labels the row 2, and the
// row opens under 2 alone.
func TestPutSecretLabelsTheRowWithTheKeyVersionThatSealedIt(t *testing.T) {
	dir := t.TempDir()
	if _, err := vault.Load(filepath.Join(dir, "kek")); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 32)
	if _, err := rand.Read(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kek.2"), second, 0o600); err != nil {
		t.Fatal(err)
	}
	testKeyFile = filepath.Join(dir, "kek")
	t.Cleanup(func() { testKeyFile = "" })
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`); code != 204 {
		t.Fatalf("PUT: %d %s", code, lastBody)
	}
	var keyRef int
	var sealedKey, sealedValue []byte
	if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_value FROM secrets").Scan(&keyRef, &sealedKey, &sealedValue); err != nil || keyRef != 2 {
		t.Fatalf("the row is labelled with the key that sealed it: key_ref %d (%v)", keyRef, err)
	}
	keys := vaultOf(t, base)
	if got, err := keys.Open(2, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealedValue); err != nil || string(got) != "v" {
		t.Fatalf("the row opens under 2: %q %v", got, err)
	}
	if _, err := keys.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealedValue); err == nil {
		t.Fatal("the row opened under 1")
	}
	if _, err := pool.Exec(ctx, "UPDATE secrets SET key_ref = NULL"); err != nil { // an overwrite relabels a row from before the label
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v2"}`); code != 204 {
		t.Fatalf("second PUT: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets WHERE key_ref = 2 AND version = 2"); n != 1 {
		t.Fatalf("the overwritten row carries the sealing key's version: %d", n)
	}
}

// A provisioned row lists as provisioned and neither PUT nor DELETE touches it: 409 provisioned,
// the row and its version as they were, no audit row.
func TestAProvisionedSecretListsItsOriginAndRefusesPutAndDelete(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"v"}`)
	if _, err := pool.Exec(ctx, "UPDATE secrets SET origin = 'provisioned'"); err != nil {
		t.Fatal(err)
	}
	if do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/secrets", ""); !strings.Contains(lastBody, `"origin":"provisioned"`) {
		t.Fatalf("the list reads the stored origin: %s", lastBody)
	}
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if code, b, _ := do(t, c, method, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"mine"}`); code != 409 || b["error"] != "provisioned" {
			t.Fatalf("%s of a provisioned row: %d %s", method, code, lastBody)
		}
	}
	if kept, audits := count(t, ctx, pool, "SELECT count(*) FROM secrets WHERE version = 1 AND origin = 'provisioned'"), count(t, ctx, pool, "SELECT count(*) FROM audit_events"); kept != 1 || audits != before {
		t.Fatalf("the provisioned row untouched (%d) and no audit row (%d written)", kept, audits-before)
	}
}
