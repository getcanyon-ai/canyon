package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	undeployRoute   = "DELETE /api/v1/projects/{id}/envs/{env}"
	undeployAudited = "SELECT count(*) FROM audit_events WHERE action = '" + undeployRoute + "'"
	previewEnv      = "/api/v1/projects/claims-portal/envs/preview"
)

// Undeploy is not delete: the container stops and the env reads as not running, and every secret
// row stays, so the next deploy starts the app with the values it had.
func TestUndeployKeepsSecretsTheCountIsUnchangedAndTheNextDeployStartsWithThem(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"sk_keep_7f3a"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"postgres://keep","env":"prod"}`)
	secrets := count(t, ctx, pool, "SELECT count(*) FROM secrets")
	var pkg string
	if err := pool.QueryRow(ctx, "SELECT package_id FROM deploys WHERE env = 'preview'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 204 {
		t.Fatalf("undeploy: %d %s", code, lastBody)
	}
	if got := fake.Stopped(); fake.Stops.Load() != 1 || got.App != "claims-portal" || got.Target != "preview" {
		t.Fatalf("the executor was asked to stop %+v, %d time(s)", got, fake.Stops.Load())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets"); n != secrets {
		t.Fatalf("undeploy keeps secrets: %d rows, had %d", n, secrets)
	}
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); len(body["project"].(map[string]any)["envs"].(map[string]any)) != 0 {
		t.Fatalf("the project read still shows an env: %s", lastBody)
	}
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?running=nothing", ""); len(body["apps"].([]any)) != 1 {
		t.Fatalf("the fleet still shows it running: %s", lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = $1 AND project_id = 'claims-portal'", undeployRoute).Scan(&detail); err != nil || detail != `{"env": "preview", "stop": "stopped", "package": "`+pkg+`"}` {
		t.Fatalf("one audit row naming the env and what ran there: %q (%v)", detail, err)
	}
	// nothing deployed any more: the 404, and neither the executor nor the trail hears of it
	if code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 404 || body["error"] != "not_found" || fake.Stops.Load() != 1 || count(t, ctx, pool, undeployAudited) != 1 {
		t.Fatalf("undeploying what is not deployed: %d %s", code, lastBody)
	}
	cleanGate(t)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	if waitVerdict(t, c, base, runOf(started)["id"].(string))["state"] != "clean" {
		t.Fatalf("the redeploy: %s", lastBody)
	}
	if _, env := fake.Handed(); env["STRIPE_SECRET_KEY"] != "sk_keep_7f3a" {
		t.Fatalf("the next deploy starts with the secret it had: %v", env)
	}
}

// Every refusal is decided before the executor is asked, and none leaves an audit row. Past them,
// a stop that fails leaves the env recorded; an agent's undeploy of preview goes through.
func TestUndeployRefusalsReachNeitherTheExecutorNorTheTrail(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if code, body, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/staging", ""); code != 400 || body["error"] != "invalid_env" {
		t.Fatalf("an env that is not one: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/nothing/envs/preview", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown project: %d %s", code, lastBody)
	}
	// a check in flight writes no environment, so it is no conflict (contracts/routes.md, The brake);
	// the executor fails: a 500, and the env stays recorded with no row, so a retry resumes
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) SELECT 'gr_busy00000', project_id, package_id, 'running' FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	fake.FailStop.Store(true)
	if code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 500 || body["error"] != "internal" || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'preview'") != 1 || count(t, ctx, pool, undeployAudited) != 0 {
		t.Fatalf("a failed stop while a check runs: %d %s", code, lastBody)
	}
	fake.FailStop.Store(false)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	// prod: who may promote may take it down — a person while prod.human-approval blocks, anyone once it is advisory
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/prod", ""); code != 403 || body["error"] != "person_required" {
		t.Fatalf("an agent taking prod down: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 204 || fake.Stops.Load() != 2 {
		t.Fatalf("preview asks no person, as a deploy does not: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/prod.human-approval?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/prod", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an agent once the rule is advisory, prod not deployed: %d %s", code, lastBody)
	}
	if fake.Stops.Load() != 2 || count(t, ctx, pool, undeployAudited) != 1 {
		t.Fatalf("a refusal reached the executor (%d stops) or the trail", fake.Stops.Load())
	}
}

// An undeploy holds its project from its run check to its commit: a start while its stop is under
// way answers 409 undeploying, and nothing else waits on it — another project's write goes through.
// Once it commits, a start succeeds.
func TestAStartWhileAnUndeployStopsAnswersUndeployingAndOtherProjectsDoNotWait(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	release := fake.HoldStop()
	t.Cleanup(release)
	undeployed := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodDelete, base+previewEnv, nil)
		resp, err := c.Do(req)
		if err != nil {
			undeployed <- 0
			return
		}
		_ = resp.Body.Close()
		undeployed <- resp.StatusCode
	}()
	for deadline := time.Now().Add(10 * time.Second); fake.Stops.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the undeploy never reached the executor")
		}
	}
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", ""); code != 409 || lastBody != `{"error":"undeploying","message":"An undeploy of claims-portal is in progress; try again in a moment."}` {
		t.Fatalf("a deploy while the undeploy stops: %d %s", code, lastBody)
	}
	quick := *c
	quick.Timeout = 5 * time.Second // the ledger lock is free while the stop runs: a wait here is the bug
	if code, _, _ := do(t, &quick, http.MethodPost, base+"/api/v1/projects", `{"id":"billing-api"}`); code != 201 {
		t.Fatalf("another project's write while the undeploy stops: %d %s", code, lastBody)
	}
	release()
	if code := <-undeployed; code != 204 {
		t.Fatalf("the undeploy: %d", code)
	}
	cleanGate(t)
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	if code != 202 || waitVerdict(t, c, base, runOf(started)["id"].(string))["state"] != "clean" {
		t.Fatalf("a deploy once the undeploy committed: %d %s", code, lastBody)
	}
}

// The brake (contracts/routes.md, The brake): taking production offline neither waits on a preview
// deploy under way nor touches it — the run is not ended and not asked to end, and preview stays.
func TestUndeployProdWhileAPreviewRunRuns(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, 'http://claims-portal.prod.localhost:7777', gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	cleanGate(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export const v = 2\n"}), "", "")
	held, builds, builder := make(chan struct{}), fake.Builds.Load(), "sha256:"+strings.Repeat("c", 64)
	fake.Builder.Store(&builder) // a new builder: the deploy builds, so it is under way while the undeploy runs
	fake.HoldBuild.Store(&held)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	t.Cleanup(func() {
		fake.HoldBuild.Store(nil)
		fake.Builder.Store(nil)
		close(held)
		waitVerdict(t, c, base, run)
	})
	for deadline := time.Now().Add(20 * time.Second); fake.Builds.Load() == builds; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the preview deploy never reached its build")
		}
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/prod", ""); code != 204 || fake.Stopped().Target != "prod" {
		t.Fatalf("undeploy prod while a preview deploy runs: %d %s, stopped %+v", code, lastBody, fake.Stopped())
	}
	var state string
	var asked bool
	if err := pool.QueryRow(ctx, "SELECT state, cancel_requested_at IS NOT NULL FROM runs WHERE id = $1", run).Scan(&state, &asked); err != nil || state != "running" || asked {
		t.Fatalf("the preview deploy was touched: %s, cancel asked %v (%v)", state, asked, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'"); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'preview'") != 1 {
		t.Fatalf("prod still recorded (%d rows), or preview gone", n)
	}
}

// A promotion past its deploy step and not yet recorded has the stop recorded on it: 202 {stopping},
// no /stop from the route, production still on the read until the run stops it; asking again answers
// the same and writes nothing (contracts/routes.md, The brake).
func TestUndeployProdMidSwitchAnswersStoppingAndAsksOnce(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, 'http://claims-portal.prod.localhost:7777', gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at) SELECT 'gr_switch0000', project_id, package_id, 'running', 'promote', now() FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	stops := fake.Stops.Load()
	const stopping = `{"stopping":{"run":{"id":"gr_switch0000","state":"running"}}}`
	for i := range 2 {
		if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/prod", ""); code != 202 || lastBody != stopping {
			t.Fatalf("ask %d: %d %s", i+1, code, lastBody)
		}
	}
	var by string
	if err := pool.QueryRow(ctx, "SELECT stop_requested_by_subject FROM runs WHERE id = 'gr_switch0000' AND stop_requested_at IS NOT NULL").Scan(&by); err != nil || by != "anonymous" {
		t.Fatalf("the stop recorded on the run: %q (%v)", by, err)
	}
	if fake.Stops.Load() != stops || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 1 || count(t, ctx, pool, undeployAudited) != 0 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'request undeploy' AND run_id = 'gr_switch0000'") != 1 {
		t.Fatalf("the route stopped prod itself (%d stops), or wrote the trail twice", fake.Stops.Load()-stops)
	}
}
