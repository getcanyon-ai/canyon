package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reportDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reportImage  = "registry.example/canyon/claims-portal@sha256:" + reportDigest
)

// reportFixture seeds a project, its run and package, a platform webhook whose host token is
// known, and one "preview" release under that run, image reportImage. Returns the report route's
// path and the token's plaintext.
func reportFixture(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, base, path, token string) {
	t.Helper()
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', 'claims-portal', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_8f2a', 'claims-portal', 'pkg_7d3e8f', 'running', 'person', 'ana', 'Ana')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	token = "host-token-fixture-not-real" //nolint:gosec // a test fixture, not a real token
	sum := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(ctx, "INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', 1, '', '', $1, 'person', 'dana', 'Dana')", sum[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_8f2a', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '5f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b', 'accepted', now())", reportImage); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, base + "/releases/rel_8f2a/report", token
}

func reportHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// A missing or wrong token is 401 before an unknown release or a bad body are even looked at; an
// unknown release is 404 before a bad body; a bad body is 400 before superseded; superseded beats
// a wrong digest; the same status again writes nothing.
func TestReportIsRefusedInOrder(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	c := client(t)
	bad := `{"status":"deployed","address":"not-https","digest":"x"}` // malformed either way
	if code, body, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_nope/report", bad, nil); code != 401 || body["error"] != "bad_token" {
		t.Fatalf("no token, unknown release, bad body: %d %s — 401 wins", code, lastBody)
	}
	if code, body, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_nope/report", bad, reportHeaders(token)); code != 404 || body["error"] != "not_found" {
		t.Fatalf("valid token, unknown release, bad body: %d %s — 404 wins", code, lastBody)
	}
	if code, body, _ := doWith(t, c, http.MethodPost, path, bad, reportHeaders(token)); code != 400 || body["error"] != "invalid_report" {
		t.Fatalf("valid token and release, bad body: %d %s", code, lastBody)
	}
	// a newer release of the same environment supersedes this one: even a well-shaped, wrong-digest
	// deployed report on the old one is superseded, never digest_mismatch.
	if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_newer', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4c', 'sent', now())", reportImage); err != nil {
		t.Fatal(err)
	}
	wrongDigest := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `"}`
	if code, body, _ := doWith(t, c, http.MethodPost, path, wrongDigest, reportHeaders(token)); code != 409 || body["error"] != "superseded" || body["release"] != "rel_newer" {
		t.Fatalf("superseded beats digest_mismatch: %d %s", code, lastBody)
	}
	// the same status reported again, on the still-current release: 200, nothing written.
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_newer/report", deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("first deployed on rel_newer: %d %s", code, lastBody)
	}
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, body, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_newer/report", deployed, reportHeaders(token)); code != 200 || len(body) != 0 {
		t.Fatalf("a duplicate report: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("a duplicate writes nothing: %d new row(s)", n-before)
	}
}

// settle writes the actor and provenance its caller names, never the report route's own: a second caller
// (a check-in) is on the trail as itself.
func TestSettleNamesItsCallersActor(t *testing.T) {
	pool, ctx, _, _, _ := reportFixture(t)
	a := &app{pool: pool}
	rel, err := readRelease(ctx, pool, "rel_8f2a")
	if err != nil {
		t.Fatal(err)
	}
	by := actor{Kind: "canyon", Subject: "check-in", Display: "Canyon"}
	report := releaseReport{Status: "deployed", Address: "https://claims.apps.meridian.example", Digest: "sha256:" + reportDigest, Resources: []reportedResource{}}
	var out outcome
	if err := a.inMutation(ctx, func(tx pgx.Tx) error {
		out, err = a.settle(ctx, tx, rel, report, by, events.Provenance{Via: "canyon", Source: "203.0.113.7"})
		return err
	}); err != nil || out.status != http.StatusOK {
		t.Fatalf("settle: %d %v (%v)", out.status, out.body, err)
	}
	var kind, subject, display, via, source string
	if err := pool.QueryRow(ctx, "SELECT actor_kind, actor_subject, actor_display, via, source FROM audit_events WHERE action = 'platform reported deployed'").
		Scan(&kind, &subject, &display, &via, &source); err != nil {
		t.Fatal(err)
	}
	if got := (actor{Kind: kind, Subject: subject, Display: display}); got != by || via != "canyon" || source != "203.0.113.7" {
		t.Fatalf("the audit row names %+v via %q from %q, want %+v via canyon from 203.0.113.7", got, via, source, by)
	}
}

func reportDigestOf(b string) string {
	s := ""
	for range 64 {
		s += b
	}
	return s
}

// A rotated host token is refused at once: the org retries under the new one.
func TestARotatedHostTokenIs401(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	c := client(t)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the original token works: %d %s", code, lastBody)
	}
	rotated := sha256.Sum256([]byte("rotated-token-fixture-not-real")) //nolint:gosec // a test fixture, not a real token
	if _, err := pool.Exec(ctx, "UPDATE platform_webhook SET token_hash = $1", rotated[:]); err != nil {
		t.Fatal(err)
	}
	failed := `{"status":"failed","reason":"ImagePullBackOff"}`
	if code, resBody, _ := doWith(t, c, http.MethodPost, path, failed, reportHeaders(token)); code != 401 || resBody["error"] != "bad_token" {
		t.Fatalf("the rotated (old) token: %d %s", code, lastBody)
	}
}

// A wrong digest goes nowhere: nothing goes live, one audit row names it.
func TestAWrongDigestGoesNowhere(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	c := client(t)
	body := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `"}`
	code, resp, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token))
	if code != 409 || resp["error"] != "digest_mismatch" || resp["expected"] != "sha256:"+reportDigest {
		t.Fatalf("a wrong digest: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys"); n != 0 {
		t.Fatalf("nothing goes live: %d deploy row(s)", n)
	}
	var detail map[string]any
	var via string
	if err := pool.QueryRow(ctx, "SELECT detail, concat_ws(' ', via, role IS NULL) FROM audit_events WHERE action = 'platform reported another digest'").Scan(&detail, &via); err != nil || detail["release"] != "rel_8f2a" || via != "platform t" {
		t.Fatalf("one audit row names the release, via the platform and no role: %v %q (%v)", detail, via, err)
	}
}

// A report after the run's 30 minutes still lands, and the run's own state stays as it ended.
func TestALateReportLands(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	body := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token)); code != 200 {
		t.Fatalf("a late report: %d %s", code, lastBody)
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM runs WHERE id = 'gr_8f2a'").Scan(&state); err != nil || state != "failed" {
		t.Fatalf("the run's state stays: %q (%v)", state, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE url = 'https://claims.apps.meridian.example'"); n != 1 {
		t.Fatalf("the environment still goes live: %d row(s)", n)
	}
}

// envs.preview matches the reported address, read through the real project-read query.
func TestEnvsPreviewMatchesTheReportedAddress(t *testing.T) {
	_, _, base, path, token := reportFixture(t)
	c := client(t)
	body := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	code, body2, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	if code != 200 {
		t.Fatalf("the project read: %d %s", code, lastBody)
	}
	project, _ := body2["project"].(map[string]any)
	envs, _ := project["envs"].(map[string]any)
	preview, _ := envs["preview"].(map[string]any)
	if preview["url"] != "https://claims.apps.meridian.example" || preview["image_digest"] != "sha256:"+reportDigest || preview["package"] != "pkg_7d3e8f" {
		t.Fatalf("envs.preview matches the report: %v", preview)
	}
}

// The bound is per token and taken only once the token validates (security review of #509): 60
// posts under a wrong token drain nothing, the valid token still gets its 200 after them, and its
// own 61st post inside the minute is the 429 — a duplicate's no-op counts like any other post. The
// limiters' clock stands still, so a slow run never refills the bucket between the 60th and the 61st.
func TestTheBoundIsPerTokenAndTakenAfterTheTokenValidates(t *testing.T) {
	at := time.Now()
	testNow = func() time.Time { return at }
	t.Cleanup(func() { testNow = nil })
	_, _, _, path, token := reportFixture(t)
	c := client(t)
	failed := `{"status":"failed","reason":"ImagePullBackOff"}`
	for range 60 {
		if code, _, _ := doWith(t, c, http.MethodPost, path, failed, reportHeaders("wrong-token-fixture-not-real")); code != 401 {
			t.Fatalf("a wrong token: %d %s", code, lastBody)
		}
	}
	for i := range 60 {
		if code, _, _ := doWith(t, c, http.MethodPost, path, failed, reportHeaders(token)); code != 200 {
			t.Fatalf("valid post %d after 60 wrong tokens: %d %s", i+1, code, lastBody)
		}
	}
	if code, body, _ := doWith(t, c, http.MethodPost, path, failed, reportHeaders(token)); code != 429 || body["error"] != "rate_limited" || body["retry_after_s"] == nil {
		t.Fatalf("the 61st valid post in the minute: %d %s", code, lastBody)
	}
}

// deployed twice, the second naming another digest, is digest_mismatch: the digest is judged
// before the duplicate's no-op, so a wrong digest never rides a same-status 200.
func TestASecondDeployedWithAnotherDigestIsAMismatch(t *testing.T) {
	_, _, _, path, token := reportFixture(t)
	c := client(t)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the first deployed: %d %s", code, lastBody)
	}
	other := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `"}`
	if code, body, _ := doWith(t, c, http.MethodPost, path, other, reportHeaders(token)); code != 409 || body["error"] != "digest_mismatch" {
		t.Fatalf("deployed again with another digest: %d %s", code, lastBody)
	}
}

// A body that is not JSON is judged where any unshaped body is — after the token and the release,
// as 400 invalid_report — never a bad_request ahead of the 401.
func TestABodyThatIsNotJSONIsJudgedAfterTheTokenAndTheRelease(t *testing.T) {
	_, _, base, path, token := reportFixture(t)
	c := client(t)
	if code, body, _ := doWith(t, c, http.MethodPost, path, `{`, reportHeaders("wrong-token-fixture-not-real")); code != 401 || body["error"] != "bad_token" {
		t.Fatalf("a wrong token and a broken body: %d %s", code, lastBody)
	}
	if code, body, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_nope/report", `{`, reportHeaders(token)); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown release and a broken body: %d %s", code, lastBody)
	}
	if code, body, _ := doWith(t, c, http.MethodPost, path, `{`, reportHeaders(token)); code != 400 || body["error"] != "invalid_report" {
		t.Fatalf("a broken body: %d %s", code, lastBody)
	}
}

// failed and removed write their audit rows and the state; removed takes the environment off the
// read; a control character in reason is invalid_report; deployed after removed is superseded.
func TestFailedAndRemovedWriteTheirRowsAndRemovedEndsTheEnvironment(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	c := client(t)
	detailOf := func(action string) map[string]any {
		t.Helper()
		var detail map[string]any
		if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = $1", action).Scan(&detail); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return detail
	}
	stateOf := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, "SELECT state FROM releases WHERE id = 'rel_8f2a'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if code, body, _ := doWith(t, c, http.MethodPost, path, `{"status":"failed","reason":"pull failed\nlive at https://claims.apps.meridian.example"}`, reportHeaders(token)); code != 400 || body["error"] != "invalid_report" {
		t.Fatalf("a reason with a newline: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, path, `{"status":"failed","reason":"ImagePullBackOff"}`, reportHeaders(token)); code != 200 || stateOf() != "failed" {
		t.Fatalf("failed: %d %s, state %s", code, lastBody, stateOf())
	}
	if d := detailOf("platform reported failed"); d["release"] != "rel_8f2a" || d["env"] != "preview" || d["reason"] != "ImagePullBackOff" {
		t.Fatalf("failed's audit row: %v", d)
	}
	var reason *string // the waiting run ends on the row's reason (runs/release.go, awaitReport)
	if err := pool.QueryRow(ctx, "SELECT reason FROM releases WHERE id = 'rel_8f2a'").Scan(&reason); err != nil || reason == nil || *reason != "ImagePullBackOff" {
		t.Fatalf("failed's reason on the release row: %v (%v)", reason, err)
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 200 || count(t, ctx, pool, "SELECT count(*) FROM deploys") != 1 {
		t.Fatalf("deployed after failed: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, path, `{"status":"removed"}`, reportHeaders(token)); code != 200 || stateOf() != "removed" || count(t, ctx, pool, "SELECT count(*) FROM deploys") != 0 {
		t.Fatalf("removed: %d %s, state %s, %d deploy row(s)", code, lastBody, stateOf(), count(t, ctx, pool, "SELECT count(*) FROM deploys"))
	}
	if d := detailOf("platform reported removed"); d["release"] != "rel_8f2a" || d["env"] != "preview" || len(d) != 2 {
		t.Fatalf("removed's audit row: %v", d)
	}
	if code, body, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 409 || body["error"] != "superseded" || body["release"] != "rel_8f2a" {
		t.Fatalf("deployed after removed: %d %s", code, lastBody)
	}
}

// A deploy Canyon ran after the release — the environment switched back to run_by: canyon, the fixture's
// setting — supersedes it (contracts/routes.md, the report row): a late deployed never overwrites what Canyon
// runs there, a late removed never takes it off, and both are 409 superseded naming the release itself; and
// once Canyon undeploys its own deploy, a late deployed is still superseded (#2469). A Canyon deploy from before
// the release supersedes nothing.
func TestACanyonDeployAfterTheReleaseSupersedesIt(t *testing.T) {
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	const canyonURL = "http://claims-portal.preview.localhost:7777"
	{
		pool, ctx, _, path, token := reportFixture(t)
		if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', $1, $2, 'gr_8f2a', 'canyon', now() - interval '1 hour')", reportImage, canyonURL); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := doWith(t, client(t), http.MethodPost, path, deployed, reportHeaders(token)); code != 200 { // Canyon's deploy before the release: it supersedes nothing
			t.Fatalf("deployed over a Canyon deploy older than the release: %d %s", code, lastBody)
		}
	}
	pool, ctx, _, path, token := reportFixture(t)
	c := client(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', $1, $2, 'gr_8f2a', 'canyon', now() + interval '1 second')", reportImage, canyonURL); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{deployed, `{"status":"removed"}`} {
		if code, resp, _ := doWith(t, c, http.MethodPost, path, body, reportHeaders(token)); code != 409 || resp["error"] != "superseded" || resp["release"] != "rel_8f2a" {
			t.Fatalf("%s after a Canyon deploy: %d %s", body, code, lastBody)
		}
		var url, runBy string
		if err := pool.QueryRow(ctx, "SELECT url, run_by FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'").Scan(&url, &runBy); err != nil || url != canyonURL || runBy != "canyon" {
			t.Fatalf("what Canyon runs after %s: %q %q (%v)", body, url, runBy, err)
		}
	}
	// Canyon undeploys its own deploy: the undeploy's own statement (routes_envs.go), the row gone.
	if _, err := pool.Exec(ctx, "DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview' AND run_by = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	if code, resp, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 409 || resp["error"] != "superseded" || resp["release"] != "rel_8f2a" {
		t.Fatalf("a late deployed after Canyon undeployed its own deploy: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys"); n != 0 {
		t.Fatalf("a superseded report wrote %d deploy row(s)", n)
	}
}

// A deployed report naming another digest tells the waiting run, once per digest it names, and the
// run keeps waiting: the release stays open for the reviewed one (contracts/events.md,
// canyonv2-ula5.57.3). A run that has ended is told nothing and the refusal is the same 409.
func TestAnotherDigestTellsTheWaitingRunOnce(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	c := client(t)
	wrong := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("b") + `"}`
	for range 2 {
		if code, resp, _ := doWith(t, c, http.MethodPost, path, wrong, reportHeaders(token)); code != 409 || resp["error"] != "digest_mismatch" {
			t.Fatalf("another digest: %d %s", code, lastBody)
		}
	}
	want := "Your platform reported another image, sha256:bbbbbbbbbbbb…, not the reviewed sha256:aaaaaaaaaaaa…: nothing went live"
	lines := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = 'gr_8f2a' AND kind = 'build.log' AND data->>'line' = $1", want).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := lines(); n != 1 {
		t.Fatalf("the waiting run is told once: %d line(s)", n)
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT state FROM releases WHERE id = 'rel_8f2a'").Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("the release stays open: %q (%v)", state, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	third := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigestOf("c") + `"}`
	if code, resp, _ := doWith(t, c, http.MethodPost, path, third, reportHeaders(token)); code != 409 || resp["error"] != "digest_mismatch" {
		t.Fatalf("another digest on an ended run: %d %s", code, lastBody)
	}
}

// A promotion's release, reported deployed, writes the prod row as a Canyon-run promotion does:
// preview's gate run and deployer, promoted_by the promote run's actor (contracts/routes.md, the
// report row: promoted_by on prod).
func TestAProdReportWritesThePromotion(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	c := client(t)
	for _, q := range []string{
		"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
		"INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_prom01', 'claims-portal', 'pkg_7d3e8f', 'running', 'promote', 'person', 'priya@meridian.example', 'Priya Raman')",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display) VALUES ('rel_prod1', 'claims-portal', 'prod', 'gr_prom01', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'accepted', now(), 'gr_8f2a', 'agent', 'agent:claude-code/ana', 'Ana''s agent')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the prod report: %d %s", code, lastBody)
	}
	var gateRun, deployedBy, promotedBy string
	if err := pool.QueryRow(ctx, "SELECT gate_run, deployed_by_subject, promoted_by_subject FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'").Scan(&gateRun, &deployedBy, &promotedBy); err != nil ||
		gateRun != "gr_8f2a" || deployedBy != "agent:claude-code/ana" || promotedBy != "priya@meridian.example" {
		t.Fatalf("the prod row: gate run %q, deployed by %q, promoted by %q (%v)", gateRun, deployedBy, promotedBy, err)
	}
}

// A deployed report replaces the row's image with the one the org runs, and clears a build digest a row
// written before the clearing kept:
// the digest was Canyon's build's, never the reported image's (contracts/routes.md, the deploys row).
func TestADeployedReportClearsTheBuildDigest(t *testing.T) {
	pool, ctx, _, path, token := reportFixture(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, build_digest) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', 'canyon/claims-portal@sha256:"+reportDigest+"', 'http://claims-portal.preview.localhost:7777', 'gr_8f2a', 'org', 'digest-of-canyons-build')"); err != nil {
		t.Fatal(err)
	}
	body := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, client(t), http.MethodPost, path, body, reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE run_by = 'org' AND build_digest IS NULL"); n != 1 {
		t.Fatalf("the reported row keeps a digest: %d cleared row(s)", n)
	}
}

// A deployed report on a release an undeploy's remove superseded, the environment still offline, means
// the org's platform started what was taken offline: it is 409 superseded, and Canyon sends another
// remove naming that release's own package and image — whether the first is still open or reported
// removed — with its own audit row. Once a person has put the environment back — a deploy, the org's or
// Canyon's, after the remove — or while a run is writing the environment, the same late report removes
// nothing; and the same report again sends nothing more. Whichever way that goes, the report never puts
// the environment live: no row of the org's, no deploy.completed, the release as it was (contracts/routes.md,
// The brake).
func TestADeployedReportAfterAnUndeploysRemoveSendsAnother(t *testing.T) {
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	for _, c := range []struct {
		name, remove, since string
		reports, again      int
	}{
		{"the remove reported", "removed", "", 1, 1},
		{"the remove still open", "sent", "", 1, 1},
		{"reported twice", "removed", "", 2, 1},
		{"a run writing preview", "removed", "INSERT INTO runs (id, project_id, package_id, state, kind, deploys) VALUES ('gr_redeploy00', 'claims-portal', 'pkg_7d3e8f', 'running', 'gate', true)", 1, 0},
		{"the org deployed since", "removed", "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', '" + reportImage + "', 'https://claims.apps.meridian.example', 'gr_8f2a', 'org', now() + interval '2 seconds')", 1, 0},
		{"Canyon deployed since", "removed", "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', 'canyon/claims-portal@sha256:" + reportDigest + "', 'http://claims-portal.preview.localhost:7777', 'gr_8f2a', 'canyon', now() + interval '2 seconds')", 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx, _, path, token := reportFixture(t)
			if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil { // the brake ended the run its release was
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at) VALUES ('rel_remove1', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'remove', $1, '7f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4d', $2, now(), now() + interval '1 second')", reportImage, c.remove); err != nil {
				t.Fatal(err)
			}
			if c.since != "" {
				if _, err := pool.Exec(ctx, c.since); err != nil {
					t.Fatal(err)
				}
			}
			rows := func() string {
				var s string
				if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(run_by || ' ' || url || ' ' || deployed_at::text, ','), '') FROM deploys").Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := rows()
			for range c.reports {
				if code, body, _ := doWith(t, client(t), http.MethodPost, path, deployed, reportHeaders(token)); code != 409 || body["error"] != "superseded" {
					t.Fatalf("deployed after the undeploy's remove: %d %s", code, lastBody)
				}
			}
			live := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE dedupe = 'deploy:preview'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'platform reported deployed'")
			if after := rows(); after != before || live != 0 || count(t, ctx, pool, "SELECT count(*) FROM releases WHERE id = 'rel_8f2a' AND state = 'accepted'") != 1 {
				t.Fatalf("the late report put preview live: rows %q, then %q; %d live rows", before, after, live)
			}
			again := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id <> 'rel_remove1' AND package_id = 'pkg_7d3e8f' AND image = '"+reportImage+"'")
			audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'send remove again' AND actor_kind = 'canyon' AND detail->>'superseded' = 'rel_8f2a'")
			if again != c.again || audited != c.again {
				t.Fatalf("removes sent again: %d, audited %d, want %d", again, audited, c.again)
			}
		})
	}
}
