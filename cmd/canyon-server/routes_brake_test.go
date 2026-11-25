package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const prodEnv = "/api/v1/projects/claims-portal/envs/prod"

// activeRun plants claims-portal's one active run, as a run reaches it: kind, and whether its deploy step began.
func activeRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, kind string, begun bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at)
		SELECT $1, 'claims-portal', id, 'running', $2, CASE WHEN $3 THEN now() END FROM packages WHERE project_id = 'claims-portal' ORDER BY created_at DESC LIMIT 1`, id, kind, begun); err != nil {
		t.Fatal(err)
	}
}

// prodRow records production running what preview runs, its container up.
func prodRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, 'http://claims-portal.prod.localhost:7777', gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	fake.Start("claims-portal", "prod")
}

// untouched is whether run is as it was planted: no cancel asked, no stop recorded, no request undeploy row.
func untouched(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run string) bool {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+run+"' AND state = 'running' AND cancel_requested_at IS NULL AND stop_requested_at IS NULL") == 1 &&
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request undeploy'") == 0
}

// A promotion whose row is already written, still running, is left alone: the undeploy stops what it
// started, and the run carries no stop.
func TestUndeployProdAfterThePromotionIsRecordedStopsWhatItStarted(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	activeRun(t, ctx, pool, "gr_landed0000", "promote", true)
	if _, err := pool.Exec(ctx, `INSERT INTO timeline_rows (ts, scope, run_id, project_id, kind, data, dedupe) VALUES (now(), 'run', 'gr_landed0000', 'claims-portal', 'deploy.completed', '{"env":"prod"}', 'deploy:prod')`); err != nil {
		t.Fatal(err)
	}
	stops := fake.Stops.Load()
	if code, _, _ := do(t, c, http.MethodDelete, base+prodEnv, ""); code != 204 || fake.Stops.Load() != stops+1 || fake.Stopped().Target != "prod" {
		t.Fatalf("undeploy after the promotion's row: %d %s, %d stops", code, lastBody, fake.Stops.Load()-stops)
	}
	if !untouched(t, ctx, pool, "gr_landed0000") || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 0 {
		t.Fatalf("the recorded promotion was touched, or production is still recorded")
	}
}

// A deploy run before its deploy step is asked to end; the executor failing then is a 500 that keeps the
// ask, and the retry, the run already asked, only stops: one request undeploy row, written when the run changed.
func TestAFailedStopKeepsTheAskAndTheRetryOnlyStops(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	activeRun(t, ctx, pool, "gr_building00", "gate", false)
	fake.FailStop.Store(true)
	if code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 500 || body["error"] != "internal" {
		t.Fatalf("a failed stop: %d %s", code, lastBody)
	}
	fake.FailStop.Store(false)
	asked := "SELECT count(*) FROM runs WHERE id = 'gr_building00' AND cancel_requested_at IS NOT NULL AND state = 'running'"
	if count(t, ctx, pool, asked) != 1 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'preview'") != 1 {
		t.Fatal("the ask was lost with the stop, or preview went with it")
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'preview'") != 0 {
		t.Fatalf("the retry: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request undeploy'"); n != 1 {
		t.Fatalf("request undeploy rows: %d, want the one the ask wrote", n)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = $1", undeployRoute).Scan(&detail); err != nil || detail != `{"env": "preview", "run": "gr_building00", "stop": "stopped", "package": "`+previewPackage(t, ctx, pool)+`"}` {
		t.Fatalf("the undeploy's row: %q (%v)", detail, err)
	}
}

// previewPackage is the package the gate run of claims-portal's preview reviewed.
func previewPackage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var pkg string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = 'gr_building00'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	return pkg
}

// Who may not take the environment offline leaves the run writing it exactly as it was — an agent on
// production while prod.human-approval blocks, a read-only member and a member with no access — and a
// contributor's stop is recorded; another person asking again keeps the first who asked.
func TestARefusedUndeployLeavesTheRunUntouched(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT 'claims-portal', e, package_id, 'canyon/claims-portal@`+executortest.Digest+`', 'http://x', 'gr_seed' FROM runs, unnest(ARRAY['preview', 'prod']) e WHERE id = 'gr_seed'`); err != nil {
		t.Fatal(err)
	}
	activeRun(t, ctx, pool, "gr_switch0001", "promote", true)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-carl'"); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		who  string
		c    *http.Client
		path string
		code string
	}{
		{"an agent on production", carl, prodEnv, "person_required"},
		{"a read-only member", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), prodEnv, "team_required"},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), prodEnv, "team_required"},
	} {
		if code, body, _ := do(t, refused.c, http.MethodDelete, base+refused.path, ""); code != 403 || body["error"] != refused.code || !untouched(t, ctx, pool, "gr_switch0001") {
			t.Fatalf("%s: %d %s, or the run was touched", refused.who, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person' WHERE actor_subject = 'sub-carl'"); err != nil {
		t.Fatal(err)
	}
	for _, asker := range []*http.Client{carl, olive} {
		if code, body, _ := do(t, asker, http.MethodDelete, base+prodEnv, ""); code != 202 || body["stopping"] == nil {
			t.Fatalf("a person on the team: %d %s", code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = 'gr_switch0001' AND stop_requested_by_subject = 'sub-carl'"); n != 1 {
		t.Fatal("the first who asked was not kept")
	}
}

// The org's first deploy of an environment — a release open, no row yet — is taken offline by a remove
// naming that release; asked again, the same remove answers and nothing more is written.
func TestUndeployOfTheOrgsFirstDeployRemovesItsRelease(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	c := anonymous(t, ctx, pool, base)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	var release string
	for i := range 2 {
		code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, "")
		if code != 202 || body["release"] == nil || (i == 1 && body["release"] != release) {
			t.Fatalf("ask %d: %d %s", i+1, code, lastBody)
		}
		release, _ = body["release"].(string)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id = '"+release+"' AND package_id = 'pkg_7d3e8f' AND image = '"+reportImage+"'"); n != 1 || count(t, ctx, pool, undeployAudited) != 1 {
		t.Fatalf("the remove names the open release (%d), and one audit row", n)
	}
}

// The org's platform taking over an environment Canyon still runs: the undeploy stops Canyon's container
// and sends the remove that supersedes the open release, whatever the row's run_by says.
func TestUndeployDuringATakeoverStopsCanyonsAndRemovesTheOrgs(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	c := anonymous(t, ctx, pool, base)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', 'canyon/claims-portal@"+executortest.Digest+"', 'http://claims-portal.preview.localhost:7777', 'gr_8f2a', 'canyon', now() - interval '1 hour')"); err != nil {
		t.Fatal(err)
	}
	stops := fake.Stops.Load()
	code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, "")
	if code != 202 || fake.Stops.Load() != stops+1 || count(t, ctx, pool, "SELECT count(*) FROM deploys") != 0 {
		t.Fatalf("the takeover's undeploy: %d %s, %d stops", code, lastBody, fake.Stops.Load()-stops)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id = '"+body["release"].(string)+"' AND image = '"+reportImage+"'"); n != 1 {
		t.Fatalf("the remove names the open release: %d", n)
	}
}

// More undeploys at once than the pool has connections all finish: an undeploy holds one connection,
// never one while it waits for another.
func TestMoreUndeploysThanConnectionsAllFinish(t *testing.T) {
	pool, _, base, c, _ := previewed(t)
	release := fake.HoldStop()
	var once sync.Once
	t.Cleanup(func() { once.Do(release) })
	n := int(pool.Config().MaxConns) + 3
	answers := make(chan int, n)
	quick := *c
	quick.Timeout = 30 * time.Second
	for range n {
		go func() {
			req, _ := http.NewRequest(http.MethodDelete, base+previewEnv, nil)
			resp, err := quick.Do(req)
			if err != nil {
				answers <- 0
				return
			}
			_ = resp.Body.Close()
			answers <- resp.StatusCode
		}()
	}
	for deadline := time.Now().Add(10 * time.Second); fake.Stops.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no undeploy reached the executor")
		}
	}
	time.Sleep(500 * time.Millisecond) // the rest pile up behind the one stopping
	once.Do(release)
	got := map[int]int{}
	for range n {
		got[<-answers]++
	}
	if got[204] != 1 || got[404] != n-1 {
		t.Fatalf("%d undeploys at once answered %v, want one 204 and the rest 404", n, got)
	}
}

// envsRead is what each read answers for envs: the project read's, the status read's and the fleet row's.
func envsRead(t *testing.T, c *http.Client, base string) [3]map[string]any {
	t.Helper()
	_, p, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	_, s, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	_, f, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	apps, _ := f["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("the fleet: %s", lastBody)
	}
	return [3]map[string]any{p["project"].(map[string]any)["envs"].(map[string]any), s["envs"].(map[string]any), apps[0].(map[string]any)["envs"].(map[string]any)}
}

// reads is whether env is in each read's envs, and says which read disagrees.
func reads(t *testing.T, c *http.Client, base, env string, want bool, when string) {
	t.Helper()
	for i, envs := range envsRead(t, c, base) {
		if _, ok := envs[env].(map[string]any); ok != want {
			t.Errorf("%s: the %s read has %s %v, want it there %v", when, [3]string{"project", "status", "fleet"}[i], env, envs[env], want)
		}
	}
}

// plant runs one statement planting the state a test reads.
func plant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) {
	t.Helper()
	if _, err := pool.Exec(ctx, q); err != nil {
		t.Fatal(err)
	}
}

// An environment a run is taking offline, as an undeploy asked, leaves envs once the run has begun
// ending: the project read, the status read and the fleet never read it stopped meanwhile, and the other
// environment stays. Its row stays, and a stop that failed shows it again once the run has ended
// (contracts/routes.md, The brake; #2193).
func TestAnEnvironmentBeingTakenOfflineLeavesEveryRead(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	activeRun(t, ctx, pool, "gr_braking000", "promote", true)
	plant(t, ctx, pool, "UPDATE runs SET ending_at = now() WHERE id = 'gr_braking000'")
	reads(t, c, base, "prod", true, "a run ending with no stop on it")
	plant(t, ctx, pool, "UPDATE runs SET ending_at = NULL, stop_requested_at = now() WHERE id = 'gr_braking000'")
	reads(t, c, base, "prod", true, "a stop pending on a run not ending yet")
	plant(t, ctx, pool, "UPDATE runs SET ending_at = now() WHERE id = 'gr_braking000'")
	reads(t, c, base, "prod", false, "production being taken offline")
	reads(t, c, base, "preview", true, "preview, beside production being taken offline")
	if count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 1 {
		t.Fatal("the row went before the stop landed")
	}
	plant(t, ctx, pool, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_braking000'") // the stop failed past its retries
	reads(t, c, base, "prod", true, "a stop that failed")
}

// An undeploy with no run in flight leaves the environment out of every read while its own stop is under
// way, the other environment staying; a stop that fails shows it again, and so does a row an undeploy
// marked that no longer holds the lock — a server that died mid-stop (contracts/routes.md, The brake; #2202).
func TestAnUndeploysOwnStopLeavesEveryRead(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	release := fake.HoldStop()
	t.Cleanup(release)
	fake.FailStop.Store(true)
	t.Cleanup(func() { fake.FailStop.Store(false) })
	answer := midStop(t, c, http.MethodDelete, base+prodEnv, "")
	reads(t, c, base, "prod", false, "production mid-stop")
	reads(t, c, base, "preview", true, "preview, beside production mid-stop")
	release()
	if code := <-answer; code != 500 {
		t.Fatalf("the failed stop: %d", code)
	}
	reads(t, c, base, "prod", true, "a stop that failed")
	plant(t, ctx, pool, "UPDATE deploys SET stopping_since = now() WHERE env = 'prod'")
	reads(t, c, base, "prod", true, "a row an undeploy marked, the lock no longer held")
}

// A hold with no run in flight leaves production out of every read while its own stop is under way, as
// an undeploy does; a stop that fails shows it again (contracts/routes.md, The brake; #2262).
func TestAHoldsOwnStopLeavesEveryRead(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	release := fake.HoldStop()
	t.Cleanup(release)
	fake.FailStop.Store(true)
	t.Cleanup(func() { fake.FailStop.Store(false) })
	answer := midStop(t, c, http.MethodPost, base+holdPath, holdReason)
	reads(t, c, base, "prod", false, "production mid-stop")
	reads(t, c, base, "preview", true, "preview, beside production mid-stop")
	release()
	if code := <-answer; code != 500 {
		t.Fatalf("the failed stop: %d", code)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod' AND stopping_since IS NOT NULL"); n != 0 {
		t.Fatalf("a stop that failed left its mark: %d row(s)", n)
	}
	reads(t, c, base, "prod", true, "a stop that failed")
}

// A hold's own stop that lands deletes production's row and keeps the hold: production stays out of every
// read, mid-stop and after (#2262).
func TestAHoldsOwnStopThatLandsKeepsProductionOut(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	release := fake.HoldStop()
	t.Cleanup(release)
	answer := midStop(t, c, http.MethodPost, base+holdPath, holdReason)
	reads(t, c, base, "prod", false, "production mid-stop")
	release()
	if code := <-answer; code != 204 {
		t.Fatalf("the stop: %d", code)
	}
	if n, held := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'"), count(t, ctx, pool, "SELECT count(*) FROM prod_holds"); n != 0 || held != 1 {
		t.Fatalf("after the stop: %d production row(s), %d hold(s), want 0 and 1", n, held)
	}
	reads(t, c, base, "prod", false, "production stopped and held")
}

// midStop sends the request, which stops a container, and answers its status once the executor holds
// that stop (fake.HoldStop): the request is mid-stop when it returns.
func midStop(t *testing.T, c *http.Client, method, url, body string) <-chan int {
	t.Helper()
	stops := fake.Stops.Load()
	answer := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			answer <- 0
			return
		}
		_ = resp.Body.Close()
		answer <- resp.StatusCode
	}()
	for deadline := time.Now().Add(10 * time.Second); fake.Stops.Load() == stops; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the executor")
		}
	}
	return answer
}

// A mark an undeploy left when its server died mid-stop is no later lock holder's: a hold of production,
// stopping it, leaves preview — still running — on every read (#2202).
func TestAStaleMarkStaysShownWhileAHoldStopsProduction(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	plant(t, ctx, pool, "UPDATE deploys SET stopping_since = now() WHERE env = 'preview'")
	release := fake.HoldStop()
	t.Cleanup(release)
	answer := midStop(t, c, http.MethodPost, base+holdPath, holdReason)
	reads(t, c, base, "preview", true, "preview, a dead undeploy's mark on it, while a hold stops production")
	release()
	if code := <-answer; code != 204 {
		t.Fatalf("the hold: %d", code)
	}
}

// A deploy run taking preview offline leaves preview out, and production in.
func TestAPreviewBeingTakenOfflineLeavesOnlyPreview(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	activeRun(t, ctx, pool, "gr_deploying0", "gate", true)
	plant(t, ctx, pool, "UPDATE runs SET deploys = true, stop_requested_at = now(), ending_at = now() WHERE id = 'gr_deploying0'")
	reads(t, c, base, "preview", false, "preview being taken offline")
	reads(t, c, base, "prod", true, "production, beside preview being taken offline")
}

// openRemove records a remove release of prod, sent and not yet reported.
func openRemove(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	plant(t, ctx, pool, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
		SELECT 'rel_remove', project_id, 'prod', 'gr_braking000', package_id, 'remove', image, '8f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4f', 'sent', now() FROM deploys WHERE env = 'preview'`)
}

// pendingRemove is whether every read has prod waiting on that remove, run by the org, and with the
// package it runs when it says one.
func pendingRemove(t *testing.T, c *http.Client, base string, pkg any) {
	t.Helper()
	for i, envs := range envsRead(t, c, base) {
		prod, _ := envs["prod"].(map[string]any)
		pending, _ := prod["pending"].(map[string]any)
		if prod["run_by"] != "org" || pending["event"] != "remove" || prod["package"] != pkg {
			t.Errorf("the %s read's prod: %v", [3]string{"project", "status", "fleet"}[i], prod)
		}
	}
}

// An environment the org's platform runs stays on every read while the run that undeploy ended ends,
// waiting on the remove.
func TestAnEnvironmentTheOrgRunsStaysWithItsRemove(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	plant(t, ctx, pool, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) SELECT project_id, 'prod', package_id, image, 'https://claims.apps.meridian.example', gate_run, 'org' FROM deploys WHERE env = 'preview'")
	activeRun(t, ctx, pool, "gr_braking000", "promote", true)
	plant(t, ctx, pool, "UPDATE runs SET stop_requested_at = now(), ending_at = now() WHERE id = 'gr_braking000'")
	openRemove(t, ctx, pool)
	var pkg string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM deploys WHERE env = 'prod'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	pendingRemove(t, c, base, pkg)
}

// A release open on an environment Canyon runs that is being taken offline reads as its pending alone,
// as where no row is there: the brake's remove ends it, and the row Canyon runs never reads.
func TestAReleaseOpenOnAnEnvironmentLeftOutReadsAsItsPending(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	prodRow(t, ctx, pool)
	activeRun(t, ctx, pool, "gr_braking000", "promote", true)
	plant(t, ctx, pool, "UPDATE runs SET stop_requested_at = now(), ending_at = now() WHERE id = 'gr_braking000'")
	openRemove(t, ctx, pool)
	pendingRemove(t, c, base, nil)
}
