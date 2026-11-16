package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const accessImage = "registry.example/canyon/app@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// accessFixture seeds two apps, claims-portal and quote-engine, each with a package, a run, a grant and
// one accepted preview release (rel_cp1, rel_qe1), and a platform webhook whose host token is known.
// Answers the server's base, the host token and the server's log.
func accessFixture(t *testing.T) (*pgxpool.Pool, context.Context, string, string, *syncBuffer) {
	t.Helper()
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	for _, app := range []struct{ id, pkg, run, rel, grantee string }{
		{"claims-portal", "pkg_cp", "gr_cp", "rel_cp1", "ana@meridian.example"},
		{"quote-engine", "pkg_qe", "gr_qe", "rel_qe1", "ben@meridian.example"},
	} {
		for _, q := range []string{
			"INSERT INTO projects (id) VALUES ('" + app.id + "')",
			"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('" + app.pkg + "', '" + app.id + "', 'aa', 'bb', 1, 'upload')",
			"INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display) VALUES ('" + app.run + "', '" + app.id + "', '" + app.pkg + "', 'running', 'person', 'ana', 'Ana')",
			"INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('" + app.id + "', 'email', '" + app.grantee + "', 'owner', 'person', 'ana', 'Ana')",
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		addRelease(t, ctx, pool, app.rel, app.id, app.run, app.pkg)
	}
	host := "host-token-fixture-not-real" //nolint:gosec // a test fixture, not a real token
	sum := sha256.Sum256([]byte(host))
	if _, err := pool.Exec(ctx, "INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', 1, '', '', $1, 'person', 'dana', 'Dana')", sum[:]); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, host, logs
}

// addRelease inserts an accepted preview release of app, created after every release before it.
func addRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, app, run, pkg string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ($1, $2, 'preview', $3, $4, 'deploy', $5, gen_random_uuid(), 'accepted', now())",
		id, app, run, pkg, accessImage); err != nil {
		t.Fatal(err)
	}
}

// fetchToken is GET /releases/:id/access-token under the host token: the status and the value.
func fetchToken(t *testing.T, c *http.Client, base, id, host string) (int, string) {
	t.Helper()
	code, body, _ := doWith(t, c, http.MethodGet, base+"/releases/"+id+"/access-token", "", reportHeaders(host))
	value, _ := body["value"].(string)
	if code == 200 && (body["name"] != "CANYON_ACCESS_TOKEN" || value == "") {
		t.Fatalf("the token's shape: %s", lastBody)
	}
	return code, value
}

// pullAs is GET /access/:app/:env under token: the status and the raw body.
func pullAs(t *testing.T, c *http.Client, base, app, env, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/access/"+app+"/"+env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// A token for app A is 401 on app B (design/HAND-OFF.md §9 item 6b), and on A's other environment,
// an unknown app and with no token at all: one answer for all, so nothing enumerates apps.
func TestAnAccessTokenForAppAIs401OnAppB(t *testing.T) {
	_, _, base, host, _ := accessFixture(t)
	c := client(t)
	code, token := fetchToken(t, c, base, "rel_cp1", host)
	if code != 200 {
		t.Fatalf("the pipeline fetches claims-portal's token: %d %s", code, lastBody)
	}
	if again, same := fetchToken(t, c, base, "rel_cp1", host); again != 200 || same != token {
		t.Fatal("a fetch again answers the same value: a pipeline that re-runs is never stranded")
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
		t.Fatalf("its own app and environment: %d %s", code, body)
	}
	var first []byte
	for i, p := range [][3]string{{"quote-engine", "preview", token}, {"claims-portal", "prod", token}, {"no-such-app", "preview", token}, {"claims-portal", "preview", ""}, {"claims-portal", "preview", "not-a-token"}} {
		code, body := pullAs(t, c, base, p[0], p[1], p[2])
		if code != 401 {
			t.Fatalf("case %d, %s/%s: %d %s", i, p[0], p[1], code, body)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(body, first) {
			t.Fatalf("every refusal is the same answer: %s, then %s", first, body)
		}
	}
	if code, _ := fetchToken(t, c, base, "rel_cp1", "not-the-host-token"); code != 401 {
		t.Fatalf("the token route under a wrong host token: %d %s", code, lastBody)
	}
	if code, _ := fetchToken(t, c, base, "rel_nope", host); code != 404 {
		t.Fatalf("an unknown release: %d %s", code, lastBody)
	}
}

// A superseded release's token is 401 once the newer release of the environment reports deployed
// (§9 item 6b) — and not before, while its pod still runs beside the newer one's; the token route
// answers the newest release alone.
func TestASupersededReleasesTokenIs401OnceTheNewerReportsDeployed(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, old := fetchToken(t, c, base, "rel_cp1", host)
	addRelease(t, ctx, pool, "rel_cp2", "claims-portal", "gr_cp", "pkg_cp")
	if code, _ := fetchToken(t, c, base, "rel_cp1", host); code != 404 {
		t.Fatalf("a release a newer one of its environment followed is not_found: %d %s", code, lastBody)
	}
	_, newer := fetchToken(t, c, base, "rel_cp2", host)
	if newer == old {
		t.Fatal("every release mints its own token")
	}
	for _, token := range []string{old, newer} {
		if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
			t.Fatalf("both pods pull until the newer reports deployed: %d %s", code, body)
		}
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + strings.Repeat("c", 64) + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp2/report", deployed, reportHeaders(host)); code != 200 {
		t.Fatalf("the newer release reports deployed: %d %s", code, lastBody)
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", old); code != 401 {
		t.Fatalf("the superseded release's token: %d %s", code, body)
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", newer); code != 200 {
		t.Fatalf("the newer release's token: %d %s", code, body)
	}
	// A later report on the newer release (the route accepts failed after deployed) never brings the
	// superseded token back: it died when the newer one reported deployed, not while it stays so.
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp2/report", `{"status":"failed","reason":"CrashLoopBackOff"}`, reportHeaders(host)); code != 200 {
		t.Fatalf("the newer release reports failed: %d %s", code, lastBody)
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", old); code != 401 {
		t.Fatalf("the superseded token after the newer release failed: %d %s", code, body)
	}
}

// The pull answers the app's access file as PUT /access carries it — accessFile, the one producer —
// for that app and no other; the token is stored as its SHA-256 beside a sealed copy, never in the
// clear.
func TestTheAccessPullIsTheAppsAccessFile(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	code, body := pullAs(t, c, base, "claims-portal", "preview", token)
	if code != 200 {
		t.Fatalf("the pull: %d %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	at, _ := got["updated_at"].(string)
	a := &app{pool: pool}
	o, err := a.orgOf(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for app, same := range map[string]bool{"claims-portal": true, "quote-engine": false} {
		file, err := a.accessFile(ctx, o, app, at)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(file, &want); err != nil {
			t.Fatal(err)
		}
		if equal := jsonEqual(got, want); equal != same {
			t.Fatalf("the pull equals %s's access file: %v, want %v\ngot  %s\nwant %s", app, equal, same, body, file)
		}
	}
	var hash, sealed []byte
	if err := pool.QueryRow(ctx, "SELECT access_token_hash, sealed_value FROM releases WHERE id = 'rel_cp1'").Scan(&hash, &sealed); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(token)); !bytes.Equal(hash, sum[:]) || bytes.Contains(sealed, []byte(token)) {
		t.Fatal("the row holds the token's SHA-256 and a sealed copy, never the token")
	}
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// The access token goes into the token route's answer alone: no log line and no error carries it,
// a refusal's and a failure's included — its sealed copy made unopenable, the route answers 500
// and logs the row and the stage, never the value.
func TestTheAccessTokenIsNeverInALogOrAnError(t *testing.T) {
	pool, ctx, base, host, logs := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	var answers []string
	for _, p := range [][2]string{{"quote-engine", "preview"}, {"claims-portal", "prod"}, {"claims-portal", "preview"}} {
		_, body := pullAs(t, c, base, p[0], p[1], token)
		answers = append(answers, string(body))
	}
	if _, err := pool.Exec(ctx, "UPDATE releases SET sealed_value = sealed_value || '\\x00'::bytea WHERE id = 'rel_cp1'"); err != nil {
		t.Fatal(err)
	}
	if code, _ := fetchToken(t, c, base, "rel_cp1", host); code != 500 {
		t.Fatalf("an unopenable token: %d %s", code, lastBody)
	}
	answers = append(answers, lastBody)
	if !strings.Contains(logs.String(), "access token of release rel_cp1: open claims-portal/access-token/rel_cp1") {
		t.Fatalf("the failure is logged by its row: %s", logs.String())
	}
	for i, s := range append(answers, logs.String()) {
		if strings.Contains(s, token) {
			t.Fatalf("the token leaked into %s", []string{"the 401 on quote-engine", "the 401 on prod", "the 200", "the 500", "the server's log"}[i])
		}
	}
}

// A token is sealed as its own row, never as a secret's: a secret of the release's project and
// environment named after the release id, its ciphertext copied into the release row, does not open
// as the token — the token route answers 500, never the secret's value (vault.go: no two sealed rows
// share their additional data).
func TestASecretNamedAfterTheReleaseNeverOpensAsItsToken(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	version, sealedKey, sealedValue, err := vaultOf(t, base).Seal("claims-portal", "preview", "rel_cp1", []byte("a-runtime-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE releases SET key_ref = $2, sealed_key = $3, sealed_value = $4, access_token_hash = '\\x01' WHERE id = $1", "rel_cp1", version, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	if code, value := fetchToken(t, c, base, "rel_cp1", host); code != 500 || strings.Contains(lastBody, "a-runtime-secret-value") {
		t.Fatalf("a secret's ciphertext in the release row: %d %q", code, value)
	}
}

// Twenty first fetches of one release at once mint one token: the row's lock makes every fetch after
// the first open what the first sealed, so each answer is that value, and it pulls.
func TestConcurrentFirstFetchesMintOneToken(t *testing.T) {
	_, _, base, host, _ := accessFixture(t)
	c := client(t)
	values := make([]string, 20)
	var wg sync.WaitGroup
	for i := range values {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, base+"/releases/rel_cp1/access-token", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+host)
			resp, err := c.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			var body struct{ Value string }
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 {
				t.Errorf("fetch %d: %d %v", i, resp.StatusCode, err)
				return
			}
			values[i] = body.Value
		}()
	}
	wg.Wait()
	for i, v := range values {
		if v != values[0] {
			t.Fatalf("fetch %d answered another token than fetch 0: every first fetch mints one", i)
		}
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", values[0]); code != 200 {
		t.Fatalf("the one token pulls: %d %s", code, body)
	}
}
