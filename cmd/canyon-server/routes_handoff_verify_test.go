package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A deployed report names every resource the release lists, and the sidecar's pull says a pod checked in
// (#2452; contracts/routes.md, the report row, the access row and envs.<env>.seen_at).

// declaringTwo is reportFixture with the release's package declaring db = postgres and files = s3.
func declaringTwo(t *testing.T) (*pgxpool.Pool, context.Context, string, string, string) {
	t.Helper()
	pool, ctx, base, path, token := reportFixture(t)
	runSQL(t, ctx, pool, `UPDATE packages SET resources = '{"db": "postgres", "files": "s3"}' WHERE id = 'pkg_7d3e8f'`)
	return pool, ctx, base, path, token
}

const filesEndpoint = "s3://claims-portal-files"

// missingLines is how many build.log lines the waiting run carries for a report that left names out.
func missingLines(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE run_id = 'gr_8f2a' AND kind = 'build.log' AND data->>'line' LIKE 'Your platform reported % deployed without saying where %'")
}

// pendingOf is envs.<env>.pending on the project read, nil while none is open.
func pendingOf(t *testing.T, c *http.Client, base, env string) map[string]any {
	t.Helper()
	code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	if code != 200 {
		t.Fatalf("the project read: %d %s", code, lastBody)
	}
	e, _ := body["project"].(map[string]any)["envs"].(map[string]any)[env].(map[string]any)
	p, _ := e["pending"].(map[string]any)
	return p
}

// A deployed report that leaves out a resource the release lists is 409 resources_missing, naming each one
// left out, sorted: the key absent, [] and a short list alike. Nothing goes live and no endpoint is written;
// the release keeps the names for its pending line, the waiting run is told once and keeps waiting, and each
// refused report writes one audit row. The report naming both lands, and the pending line goes.
func TestADeployedReportNamesEveryResource(t *testing.T) {
	pool, ctx, base, path, token := declaringTwo(t)
	c := client(t)
	db := `{"name":"db","endpoint":"` + providedEndpoint + `"}`
	for _, r := range []struct {
		name, body string
		missing    []any
	}{
		{"the key absent", `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`, []any{"db", "files"}},
		{"an empty list", deployedWith(`[]`), []any{"db", "files"}},
		{"db alone", deployedWith(`[` + db + `]`), []any{"files"}},
		{"db alone, again", deployedWith(`[` + db + `]`), []any{"files"}},
	} {
		code, body, _ := doWith(t, c, http.MethodPost, path, r.body, reportHeaders(token))
		names := make([]string, len(r.missing))
		for i, n := range r.missing {
			names[i] = n.(string)
		}
		message := "the report has no endpoint for " + strings.Join(names, ", ") + ": send one for every resource the release lists"
		if code != 409 || body["error"] != "resources_missing" || !reflect.DeepEqual(body["missing"], r.missing) || body["message"] != message || len(body) != 3 {
			t.Fatalf("%s: %d %s", r.name, code, lastBody)
		}
		if p := pendingOf(t, c, base, "preview"); !reflect.DeepEqual(p["missing"], r.missing) || p["state"] != "accepted" {
			t.Fatalf("%s: the pending line's missing is %v", r.name, p)
		}
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM releases WHERE id = 'rel_8f2a'").Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("the release stays open: %q (%v)", state, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys") + count(t, ctx, pool, "SELECT count(*) FROM platform_resources"); n != 0 {
		t.Fatalf("a refused report wrote %d deploy or endpoint row(s)", n)
	}
	if n := missingLines(t, ctx, pool); n != 1 {
		t.Fatalf("the waiting run is told once: %d line(s)", n)
	}
	want := "Your platform reported 7d3e8f deployed without saying where db, files are: nothing went live, Canyon is still waiting"
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE run_id = 'gr_8f2a' AND kind = 'build.log' AND data->>'line' = '"+want+"'"); n != 1 {
		t.Fatalf("the line names the version and every name left out, are for two: %d line(s) %q", n, want)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'platform reported deployed without resources' AND detail @> '{"release":"rel_8f2a","env":"preview"}'`); n != 4 {
		t.Fatalf("one audit row per refused report: %d", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'platform reported deployed without resources' AND detail->'missing' = '["files"]'`); n != 2 {
		t.Fatalf("the audit rows name what was left out: %d of 2", n)
	}
	both := deployedWith(`[` + db + `,{"name":"files","endpoint":"` + filesEndpoint + `"}]`)
	if code, _, _ := doWith(t, c, http.MethodPost, path, both, reportHeaders(token)); code != 200 {
		t.Fatalf("the report naming both: %d %s", code, lastBody)
	}
	if e := endpointOf(t, ctx, pool); e != "db="+providedEndpoint+",files="+filesEndpoint {
		t.Fatalf("the endpoints written: %q", e)
	}
	if p := pendingOf(t, c, base, "preview"); p != nil {
		t.Fatalf("once live, nothing is pending: %v", p)
	}
}

// A shape the rules refuse is still 400, before any 409: an entry the release does not list beside a missing
// one, and a null for the list. A rotated host token is 401 before either.
func TestAMisshapedResourcesIs400BeforeMissing(t *testing.T) {
	pool, ctx, _, path, token := declaringTwo(t)
	c := client(t)
	for name, body := range map[string]string{
		"an unknown name, files missing": deployedWith(`[{"name":"cache","endpoint":"` + providedEndpoint + `"}]`),
		"a null":                         deployedWith(`null`),
	} {
		if code, resp, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token)); code != 400 || resp["error"] != "invalid_report" {
			t.Errorf("%s: %d %s", name, code, lastBody)
		}
	}
	rotated := "rotated-token-fixture-not-real" //nolint:gosec // a test fixture, not a real token
	runSQL(t, ctx, pool, "UPDATE platform_webhook SET token_hash = sha256('"+rotated+"'::bytea)")
	if code, resp, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[]`), reportHeaders(token)); code != 401 || resp["error"] != "bad_token" {
		t.Errorf("the replaced token: %d %s", code, lastBody)
	}
	if code, resp, _ := doWith(t, c, http.MethodPost, path, deployedWith(`[]`), reportHeaders(rotated)); code != 409 || resp["error"] != "resources_missing" {
		t.Errorf("the current token: %d %s", code, lastBody)
	}
	if n := missingLines(t, ctx, pool) + count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 2 {
		t.Errorf("only the current token's report wrote: %d row(s), want its line and its audit row", n)
	}
}

// resources_missing comes after every other refusal (contracts/routes.md, the report row's order): a report
// leaving every resource out on a remove release, a project being deleted, a superseded release, with another
// digest, or a second time on a release already live is answered as it was before, with what that answer sends,
// and never resources_missing; on production while it is held, 409 held stands and the remove goes out; on one an
// undeploy's remove superseded, the brake sends its one remove again and the answer is 409 superseded.
func TestResourcesMissingIsTheLastRefusal(t *testing.T) {
	bare := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	removes := func(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
		return count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id NOT IN ('rel_8f2a', 'rel_remove1')") // a case's own setup is none sent
	}
	for _, c := range []struct {
		name                 string
		setup                []string
		release, body, error string
		removes              int
	}{
		{"a remove release", []string{"UPDATE releases SET event = 'remove' WHERE id = 'rel_8f2a'"}, "rel_8f2a", bare, "not_a_deploy", 0},
		{"a project being deleted", []string{"UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'"}, "rel_8f2a", bare, "project_deleted", 1},
		{"a superseded release", []string{"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_newer', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'sent', now())"}, "rel_8f2a", bare, "superseded", 0},
		{"another digest", nil, "rel_8f2a", `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `"}`, "digest_mismatch", 0},
		{"a release live already", []string{"UPDATE releases SET state = 'deployed' WHERE id = 'rel_8f2a'"}, "rel_8f2a", bare, "", 0},
		{"production while it is held", []string{
			"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
			"INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_prom01', 'claims-portal', 'pkg_7d3e8f', 'running', 'promote', 'person', 'priya@meridian.example', 'Priya Raman')",
			"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run) VALUES ('rel_prod1', 'claims-portal', 'prod', 'gr_prom01', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'accepted', now(), 'gr_8f2a')",
		}, "rel_prod1", bare, "held", 1},
		{"a release an undeploy's remove superseded", []string{ // the brake (contracts/routes.md): it removes again
			"UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_8f2a'",
			"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at) VALUES ('rel_remove1', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'remove', '" + reportImage + "', gen_random_uuid(), 'sent', now(), now() + interval '1 second')",
		}, "rel_8f2a", bare, "superseded", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx, base, _, token := declaringTwo(t)
			runSQL(t, ctx, pool, c.setup...)
			if c.error == "held" {
				plantHold(t, ctx, pool)
			}
			audit := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
			code, body, _ := doWith(t, client(t), http.MethodPost, base+"/releases/"+c.release+"/report", c.body, reportHeaders(token))
			if c.error == "" {
				if code != 200 || len(body) != 0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events") != audit {
					t.Fatalf("the same status again: %d %s, or it wrote", code, lastBody)
				}
				return
			}
			if code != 409 || body["error"] != c.error {
				t.Fatalf("%d %s, want 409 %s", code, lastBody, c.error)
			}
			if n := removes(t, ctx, pool); n != c.removes {
				t.Fatalf("%d remove(s) sent, want %d", n, c.removes)
			}
			if n := missingLines(t, ctx, pool) + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'platform reported deployed without resources'") +
				count(t, ctx, pool, "SELECT count(*) FROM releases WHERE missing_resources IS NOT NULL") + count(t, ctx, pool, "SELECT count(*) FROM deploys"); n != 0 {
				t.Fatalf("a refusal before resources_missing wrote %d of its rows, or went live", n)
			}
		})
	}
}

// A release that lists no resource takes a report without the key, as one with [], and either replaces the
// environment's endpoints whole: what an earlier release's report named goes (#2452: every deployed that is
// written replaces them).
func TestAReleaseThatListsNoneClearsTheEndpoints(t *testing.T) {
	for name, body := range map[string]string{
		"without the key": `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`,
		"with []":         deployedWith(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx, _, path, token := reportFixture(t)
			runSQL(t, ctx, pool,
				"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at) VALUES ('rel_earlier', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', '"+reportImage+"', gen_random_uuid(), 'deployed', now(), now() - interval '1 hour')",
				"INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ('claims-portal', 'preview', 'db', '"+providedEndpoint+"', 'rel_earlier')")
			if code, _, _ := doWith(t, client(t), http.MethodPost, path, body, reportHeaders(token)); code != 200 {
				t.Fatalf("the report: %d %s", code, lastBody)
			}
			if e := endpointOf(t, ctx, pool); e != "" {
				t.Fatalf("the earlier endpoints stayed: %q", e)
			}
		})
	}
}

// A release whose package row is gone never reads as one that declares nothing: a deployed report on it is
// 500 and writes nothing (#2452, fail closed). A failed or removed one names no resource, so it is recorded
// as ever: a delete waiting on that release's removed never waits forever.
func TestAReleaseWithNoPackageFailsClosed(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_gone', 'claims-portal', 'prod', 'gr_8f2a', 'pkg_gone', 'deploy', '"+reportImage+"', gen_random_uuid(), 'accepted', now())")
	state := func() string {
		var s string
		if err := pool.QueryRow(ctx, "SELECT state FROM releases WHERE id = 'rel_gone'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_gone/report", deployed, reportHeaders(token)); code != 500 {
		t.Errorf("deployed on a release whose package is gone: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys") + count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 0 || state() != "accepted" {
		t.Errorf("the refused deployed wrote %d row(s), or moved the release to %s", n, state())
	}
	for _, r := range []struct{ body, state string }{{`{"status":"failed","reason":"ImagePullBackOff"}`, "failed"}, {`{"status":"removed"}`, "removed"}} {
		if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_gone/report", r.body, reportHeaders(token)); code != 200 || state() != r.state {
			t.Errorf("%s on a release whose package is gone: %d %s, the release %s", r.body, code, lastBody, state())
		}
	}
}

// seenAt is the release's seen_at, nil before a pod of it checked in.
func seenAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, release string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(ctx, "SELECT seen_at FROM releases WHERE id = $1", release).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// A pull checks in on the release its token was minted for, and no other: another app's release, and its own
// app's other release, stay unseen. A refused pull — another app's, a dead token's — writes nothing.
func TestAPullChecksInItsOwnReleaseAlone(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	if code, body := pullAs(t, c, base, "quote-engine", "preview", token); code != 401 {
		t.Fatalf("another app's pull: %d %s", code, body)
	}
	if seenAt(t, ctx, pool, "rel_cp1") != nil || seenAt(t, ctx, pool, "rel_qe1") != nil {
		t.Fatal("a refused pull checked in")
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
		t.Fatalf("its own pull: %d %s", code, body)
	}
	if seenAt(t, ctx, pool, "rel_cp1") == nil || seenAt(t, ctx, pool, "rel_qe1") != nil {
		t.Fatal("the pull checked in on another release, or not on its own")
	}
	addRelease(t, ctx, pool, "rel_cp2", "claims-portal", "gr_cp", "pkg_cp")
	_, newer := fetchToken(t, c, base, "rel_cp2", host)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + strings.Repeat("c", 64) + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp2/report", deployed, reportHeaders(host)); code != 200 {
		t.Fatalf("the newer release reports deployed: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE releases SET seen_at = now() - interval '1 hour' WHERE id = 'rel_cp1'")
	before := seenAt(t, ctx, pool, "rel_cp1")
	if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 401 {
		t.Fatalf("the dead token's pull: %d %s", code, body)
	}
	if after := seenAt(t, ctx, pool, "rel_cp1"); !after.Equal(*before) || seenAt(t, ctx, pool, "rel_cp2") != nil {
		t.Fatalf("the dead token checked in: %v, then %v", before, after)
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", newer); code != 200 || seenAt(t, ctx, pool, "rel_cp2") == nil {
		t.Fatalf("the newer release's pull: %d %s", code, body)
	}
}

// A release seen in the last 30 seconds is not written again, however many pods pull; one seen longer ago is —
// so after a Canyon outage the first pull clears the stale line (#2452).
func TestACheckInIsWrittenAtMostEvery30Seconds(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	runSQL(t, ctx, pool, "UPDATE releases SET seen_at = now() - interval '10 seconds' WHERE id = 'rel_cp1'")
	recent := seenAt(t, ctx, pool, "rel_cp1")
	if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
		t.Fatalf("the pull: %d %s", code, body)
	}
	if after := seenAt(t, ctx, pool, "rel_cp1"); !after.Equal(*recent) {
		t.Fatalf("a check-in 10 seconds old was written again: %v, then %v", recent, after)
	}
	for _, ago := range []string{"31 seconds", "10 minutes"} {
		runSQL(t, ctx, pool, "UPDATE releases SET seen_at = now() - interval '"+ago+"' WHERE id = 'rel_cp1'")
		old := seenAt(t, ctx, pool, "rel_cp1")
		if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
			t.Fatalf("the pull: %d %s", code, body)
		}
		if after := seenAt(t, ctx, pool, "rel_cp1"); !after.After(*old) {
			t.Fatalf("a check-in %s old was not written again: %v, then %v", ago, old, after)
		}
	}
}

// The check-in never gates sign-in: with the release's row locked by another transaction the pull answers at
// once and writes nothing, never waiting; with the write failing the pull still answers the file, and the
// failure is logged.
func TestACheckInThatCannotBeWrittenNeverHoldsThePull(t *testing.T) {
	pool, ctx, base, host, logs := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	_, want := pullAs(t, c, base, "claims-portal", "preview", token)
	runSQL(t, ctx, pool, "UPDATE releases SET seen_at = NULL")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM releases WHERE id = 'rel_cp1' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	code, body := pullAs(t, c, base, "claims-portal", "preview", token)
	if took := time.Since(began); code != 200 || took > checkInWithin/2 {
		t.Errorf("a pull while the row is locked: %d in %v, %s", code, took, body)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if seenAt(t, ctx, pool, "rel_cp1") != nil {
		t.Fatal("a pull wrote past another transaction's lock")
	}
	runSQL(t, ctx, pool, `CREATE FUNCTION refuse_seen() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'the disk is full'; END $$`,
		"CREATE TRIGGER refuse_seen BEFORE UPDATE OF seen_at ON releases FOR EACH ROW EXECUTE FUNCTION refuse_seen()")
	code, body = pullAs(t, c, base, "claims-portal", "preview", token)
	if code != 200 || !strings.Contains(string(body), `"members"`) || len(body) != len(want) {
		t.Fatalf("a pull whose check-in fails: %d %s", code, body)
	}
	if !strings.Contains(logs.String(), "check-in of claims-portal/preview: ERROR: the disk is full") {
		t.Fatalf("the failed check-in is not logged: %s", logs.String())
	}
}

// After a Canyon outage an environment the org runs reads stale — its last check-in ten minutes old — and the
// pod's next pull clears it: the project read's seen_at is fresh again (#2452; the docs say within a minute).
func TestOnePullAfterAnOutageClearsTheStaleCheckIn(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	runSQL(t, ctx, pool,
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'preview', 'pkg_cp', '"+accessImage+"', 'https://claims.apps.meridian.example', 'gr_cp', 'org')",
		"UPDATE releases SET seen_at = now() - interval '10 minutes' WHERE id = 'rel_cp1'")
	seen := func() time.Duration {
		t.Helper()
		code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		at, err := time.Parse(time.RFC3339, fmt.Sprint(envIn(body["project"], "preview")["seen_at"]))
		if code != 200 || err != nil {
			t.Fatalf("the project read's seen_at: %d %s (%v)", code, lastBody, err)
		}
		return time.Since(at)
	}
	if ago := seen(); ago < 9*time.Minute {
		t.Fatalf("before the pull the check-in reads %v old, want ten minutes", ago)
	}
	if code, body := pullAs(t, c, base, "claims-portal", "preview", token); code != 200 {
		t.Fatalf("the pull: %d %s", code, body)
	}
	if ago := seen(); ago > time.Minute {
		t.Fatalf("after the pull the check-in reads %v old: one pull clears the stale line", ago)
	}
}

// Twenty pods pulling at once write one check-in: the row a pull holds is skipped by the others, and a
// row seen since is not written.
func TestConcurrentPullsCheckInOnce(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	_, token := fetchToken(t, c, base, "rel_cp1", host)
	runSQL(t, ctx, pool, "CREATE TABLE seen_writes (at timestamptz)",
		`CREATE FUNCTION count_seen() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO seen_writes VALUES (NEW.seen_at); RETURN NEW; END $$`,
		"CREATE TRIGGER count_seen AFTER UPDATE OF seen_at ON releases FOR EACH ROW EXECUTE FUNCTION count_seen()")
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = pullAs(t, c, base, "claims-portal", "preview", token)
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != 200 {
			t.Fatalf("pull %d: %d", i, code)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM seen_writes"); n != 1 {
		t.Fatalf("twenty pulls at once wrote %d check-ins, want 1", n)
	}
}

// envIn is envs.<env> of a project row as a read answers it, nil when it carries none.
func envIn(row any, env string) map[string]any {
	m, _ := row.(map[string]any)
	envs, _ := m["envs"].(map[string]any)
	e, _ := envs[env].(map[string]any)
	return e
}

// seen_at follows state exactly (#2452; design/PERMISSIONS.md): on production the org runs, an owner, a
// contributor and an admin read it on the project read, the list, the fleet and the status read; a viewer on
// the reduced row, beside the url alone; a responder with no grant on production's; a member with no access
// nowhere. Preview, which Canyon runs, says its state and never a check-in, though a release the org ran there
// once checked in. A held production reads as nothing deployed to a viewer, and stays the responder's.
func TestSeenAtFollowsStateAtEveryLevel(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	const seen = "2026-10-23T09:58:00Z"
	runSQL(t, ctx, pool,
		"UPDATE deploys SET run_by = 'org', url = 'https://claims.apps.meridian.example' WHERE project_id = 'claims-portal' AND env = 'prod'",
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, seen_at)
			SELECT 'rel_live', 'claims-portal', 'prod', 'gr_seed', id, 'deploy', 'canyon/claims-portal@x', gen_random_uuid(), 'deployed', now(), now() - interval '1 hour', '`+seen+`' FROM packages WHERE project_id = 'claims-portal'`,
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, seen_at)
			SELECT 'rel_once', 'claims-portal', 'preview', 'gr_seed', id, 'deploy', 'canyon/claims-portal@x', gen_random_uuid(), 'deployed', now(), now() - interval '2 hours', '`+seen+`' FROM packages WHERE project_id = 'claims-portal'`,
		`INSERT INTO deploys (project_id, env, package_id, image, url, gate_run)
			SELECT project_id, 'preview', id, 'canyon/claims-portal@x', 'http://claims-portal.preview.localhost:7777', 'gr_seed' FROM packages WHERE project_id = 'claims-portal'`)
	fake.Start("claims-portal", "preview")
	reads := func(c *http.Client) map[string]any { // each read's prod and preview, by read
		out := map[string]any{}
		_, one, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		out["project"] = one["project"]
		_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects", "")
		rows, _ := list["projects"].([]any)
		for _, r := range rows {
			if r.(map[string]any)["id"] == "claims-portal" {
				out["list"] = r
			}
		}
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		out["fleet"] = fleetRowOf(page, "claims-portal")
		if code, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code == 200 {
			out["status"] = status
		}
		return out
	}
	whole := func(who string, c *http.Client) {
		for read, row := range reads(c) {
			if prod, preview := envIn(row, "prod"), envIn(row, "preview"); prod["seen_at"] != seen || prod["run_by"] != "org" || preview == nil || preview["seen_at"] != nil {
				t.Errorf("%s's %s: prod %v, preview %v", who, read, prod, preview)
			}
		}
	}
	whole("the owner", olive)
	whole("a contributor", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false))
	whole("an admin", signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false))
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	reduced := func(who string, c *http.Client, prod, preview map[string]any) {
		got := reads(c)
		for _, read := range []string{"project", "list", "fleet", "status"} {
			row, ok := got[read]
			if !ok {
				continue // the status read is refused below contributor but a responder's
			}
			preview := preview
			if read == "list" && preview != nil { // the list asks the executor nothing, so it carries no state (contracts/routes.md, the project read)
				preview = map[string]any{"url": preview["url"]}
			}
			if p, v := envIn(row, "prod"), envIn(row, "preview"); !reflect.DeepEqual(p, prod) || !reflect.DeepEqual(v, preview) {
				t.Errorf("%s's %s: prod %v, preview %v; want %v, %v", who, read, p, v, prod, preview)
			}
		}
	}
	live := map[string]any{"url": "https://claims.apps.meridian.example", "seen_at": seen}
	running := map[string]any{"url": "http://claims-portal.preview.localhost:7777", "state": "running"}
	reduced("a viewer", vic, live, running)
	reduced("a responder", rhea, live, nil)
	reduced("a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), nil, nil)
	if _, ok := reads(rhea)["status"]; !ok {
		t.Error("the responder's status read was refused")
	}
	plantHold(t, ctx, pool)
	reduced("a viewer, production held", vic, nil, running)
	reduced("a responder, production held", rhea, live, nil)
}

// What a report that fits leaves out is every declared name it does not carry, sorted, and its lines say
// them joined with ", ", is for one and are for more (EXPERIENCE.md D5; #2452).
func TestMissingIsWhatTheReportLeftOutSorted(t *testing.T) {
	declared := map[string]string{"files": "s3", "db": "postgres", "audit": "postgres"}
	for _, c := range []struct {
		reported []reportedResource
		missing  []string
		where    string
	}{
		{nil, []string{"audit", "db", "files"}, "audit, db, files are"},
		{[]reportedResource{}, []string{"audit", "db", "files"}, "audit, db, files are"},
		{[]reportedResource{{Name: "db"}, {Name: "audit"}}, []string{"files"}, "files is"},
		{[]reportedResource{{Name: "files"}, {Name: "db"}, {Name: "audit"}}, nil, ""},
	} {
		got := missingResources(c.reported, declared)
		if !reflect.DeepEqual(got, c.missing) {
			t.Errorf("%v reported: missing %v, want %v", c.reported, got, c.missing)
		}
		if got != nil && whereNames(got) != c.where {
			t.Errorf("%v: said %q, want %q", got, whereNames(got), c.where)
		}
	}
	if got := missingResources(nil, map[string]string{}); got != nil {
		t.Errorf("a release that lists none misses %v", got)
	}
}
