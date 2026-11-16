package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orgDeclaring is releasing() with the package declaring db = postgres, a runtime secret of the name it
// gives beside one of its own, and a driver that counts every call: under org it must never be asked (#2432).
func orgDeclaring(t *testing.T, platform *stubPlatform) (*pgxpool.Pool, context.Context, *atomic.Int32) {
	t.Helper()
	p, ctx := releasing(t, "wh_org_pipeline_secret_2432", platform) //nolint:gosec // a test fixture, not a real secret
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres"}' WHERE id = $1`, pkg); err != nil {
		t.Fatal(err)
	}
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "a-fixture-value")
	sealed(t, ctx, p, "preview", "runtime", "DATABASE_URL", "postgres://kept/fixture") // a row Canyon sealed before the switch, say
	var calls atomic.Int32
	driver := ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		calls.Add(1)
		return map[string]string{"DATABASE_URL": "postgres://made/by-canyon"}, nil
	})
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: driver, {"postgres", "prod"}: driver}
	t.Cleanup(func() { pipe.Drivers = nil })
	return p, ctx, &calls
}

// provisionedBy is how many resource records and provisioned secret rows the project holds.
func provisionedBy(t *testing.T, ctx context.Context, p *pgxpool.Pool) (records, secrets int) {
	t.Helper()
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM resources WHERE project_id = $1), (SELECT count(*) FROM secrets WHERE project_id = $1 AND origin = 'provisioned')`, project).
		Scan(&records, &secrets); err != nil {
		t.Fatal(err)
	}
	return records, secrets
}

// acceptedRelease waits for run's release in env to be accepted and answers it.
func acceptedRelease(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) string {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var id, state string
		err := p.QueryRow(ctx, "SELECT id, state FROM releases WHERE run_id = $1 AND event = 'deploy'", run).Scan(&id, &state)
		if err == nil && state == "accepted" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s's release was never accepted: %q (%v)", run, state, err)
		}
	}
}

// bodyOf is the delivered body of release, decoded.
func bodyOf(t *testing.T, platform *stubPlatform, release string) map[string]any {
	t.Helper()
	for _, r := range platform.requests() {
		var body map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body["release"] == release {
			return body
		}
	}
	t.Fatalf("no delivery of %s", release)
	return nil
}

// Under run_by: org a deploy and a promotion provision nothing — no driver call, no resource record, no
// provisioned secret — and the release names the resource, its variables leaving out what it gives (#2432).
func TestAnOrgDeployAndPromotionProvisionNothing(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx, calls := orgDeclaring(t, platform)
	wasCeiling := reportCeiling
	reportCeiling = 10 * time.Second
	t.Cleanup(func() { reportCeiling = wasCeiling })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	release := acceptedRelease(t, ctx, p, run.ID)
	body := bodyOf(t, platform, release)
	if vars := fmt.Sprint(body["variables"]); vars != "[STRIPE_SECRET_KEY]" {
		t.Fatalf("variables leave out what a resource gives: %s", vars)
	}
	if res := fmt.Sprint(body["resources"]); res != "[map[gives:[DATABASE_URL] name:db type:postgres]]" {
		t.Fatalf("the release names the resource: %s", res)
	}
	if err := reportOn(ctx, p, release, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the org's deploy: %s %s", state, why(reason))
	}
	image := "registry.example/canyon/app@sha256:" + strings.Repeat("d", 64)
	promo, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: run.ID, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	prodRelease := acceptedRelease(t, ctx, p, promo.ID)
	if err := reportOn(ctx, p, prodRelease, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, promo.ID); state != "clean" {
		t.Fatalf("the org's promotion: %s %s", state, why(reason))
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("a driver was asked %d time(s) under org", n)
	}
	if records, secrets := provisionedBy(t, ctx, p); records != 0 || secrets != 0 {
		t.Fatalf("Canyon provisioned under org: %d record(s), %d provisioned secret(s)", records, secrets)
	}
	var marked bool // the boundary a /run has, kept before the release (cancelled(…, true))
	if err := p.QueryRow(ctx, "SELECT deploy_started_at IS NOT NULL FROM runs WHERE id = $1", promo.ID).Scan(&marked); err != nil || !marked {
		t.Fatalf("the promotion was not marked begun before its release: %v %v", marked, err)
	}
}

// sendIn runs send in one committed transaction.
func sendIn(t *testing.T, ctx context.Context, p *pgxpool.Pool, send func(pgx.Tx) (string, error)) string {
	t.Helper()
	var id string
	if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { id, err = send(tx); return err }); err != nil {
		t.Fatal(err)
	}
	return id
}

// stateOf waits for release to reach state.
func stateOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, release, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var state string
		if err := p.QueryRow(ctx, "SELECT state FROM releases WHERE id = $1", release).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s, never %s", release, state, want)
		}
	}
}

func orgRunsPreview(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_0rgd', '" + project + "', '" + pkg + "', 'clean')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('" + project + "', 'preview', '" + pkg + "', 'registry.example/canyon/app@sha256:" + strings.Repeat("c", 64) + "', 'https://claims.apps.meridian.example', 'gr_0rgd', 'org')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// Only a project delete's remove carries delete: an undeploy's and the brake's never do, so the platform
// keeps the data they take offline (#2432).
func TestOnlyAProjectDeletesRemoveCarriesDelete(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_2432b", platform) //nolint:gosec // a test fixture, not a real secret
	orgRunsPreview(t, ctx, p)
	undeploy := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		id, _, err := SendRemoveTx(ctx, tx, durable, project, "preview")
		return id, err
	})
	stateOf(t, ctx, p, undeploy, "accepted")
	brake := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		id, _, err := SendRemoveOfTx(ctx, tx, durable, undeploy)
		return id, err
	})
	stateOf(t, ctx, p, brake, "accepted")
	for name, id := range map[string]string{"an undeploy's": undeploy, "the brake's": brake} {
		if body := bodyOf(t, platform, id); body["delete"] != nil {
			t.Fatalf("%s remove carries delete: %v", name, body)
		}
	}
	deleting := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		return SendDeleteRemoveTx(ctx, tx, durable, project, "preview", false)
	})
	if deleting == "" || deleting == brake {
		t.Fatalf("an open remove without delete was taken for the delete's: %q", deleting)
	}
	stateOf(t, ctx, p, deleting, "accepted")
	if body := bodyOf(t, platform, deleting); body["delete"] != true {
		t.Fatalf("a project delete's remove: %v", body)
	}
	if again := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		return SendDeleteRemoveTx(ctx, tx, durable, project, "preview", false)
	}); again != deleting {
		t.Fatalf("the delete again sent another remove, %s, beside the open %s", again, deleting)
	}
}

// An environment undeployed before the delete still gets a delete's remove, and once the platform reports
// it removed the delete is done there (#2432).
func TestAnUndeployThenDeleteStillSendsADeleteRemove(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_2432c", platform) //nolint:gosec // a test fixture, not a real secret
	orgRunsPreview(t, ctx, p)
	undeploy := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		id, _, err := SendRemoveTx(ctx, tx, durable, project, "preview")
		return id, err
	})
	stateOf(t, ctx, p, undeploy, "accepted")
	if _, err := p.Exec(ctx, "UPDATE releases SET state = 'removed' WHERE id = $1", undeploy); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1", project); err != nil {
		t.Fatal(err)
	}
	deleting := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		return SendDeleteRemoveTx(ctx, tx, durable, project, "preview", false)
	})
	if deleting == "" {
		t.Fatal("an undeployed environment got no delete's remove")
	}
	stateOf(t, ctx, p, deleting, "accepted")
	if body := bodyOf(t, platform, deleting); body["delete"] != true || body["environment"] != "preview" {
		t.Fatalf("the delete's remove: %v", body)
	}
	if _, err := p.Exec(ctx, "UPDATE releases SET state = 'removed' WHERE id = $1", deleting); err != nil {
		t.Fatal(err)
	}
	if done := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) {
		return SendDeleteRemoveTx(ctx, tx, durable, project, "preview", false)
	}); done != "" {
		t.Fatalf("a delete's remove reported removed is done, but another was sent: %s", done)
	}
	if none := sendIn(t, ctx, p, func(tx pgx.Tx) (string, error) { return SendDeleteRemoveTx(ctx, tx, durable, project, "prod", false) }); none != "" {
		t.Fatalf("prod, with no release of the org's, got one: %s", none)
	}
}

// An org run journaled under one version — stopped while it waits on the platform's report — resumes on a
// server of the next, handed over (runs/durable.go): it goes live on the report, sends no second release, and
// nothing is provisioned (#2432). The steps are the release before's name for name, so no durableVersion bump.
// The versions are the test's own names, as the other hand-over tests' are.
func TestAnOrgRunOfTheReleaseBeforeResumesProvisioningNothing(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx, calls := orgDeclaring(t, platform)
	wasCeiling := reportCeiling
	reportCeiling = 3 * time.Minute // past a stopped server's 30 seconds of staleness
	t.Cleanup(func() { reportCeiling = wasCeiling })
	schema := fmt.Sprintf("dbos_handoff_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	life, kill := context.WithCancel(ctx)
	defer kill()
	older, _ := serverOn(t, life, p, schema, "handoff-v3")
	newer, stopNewer := serverOn(t, ctx, p, schema, "handoff-v4", "none") // up before the run, taking nothing yet
	t.Cleanup(stopNewer)
	run, err := Start(ctx, p, older, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	release := acceptedRelease(t, ctx, p, run.ID) // journaled up to the wait on the report
	drivenBy(t, ctx, p, schema, run.ID, older)
	newer.ListenQueues(newer)
	kill()
	if err := dbos.Shutdown(older, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(staleAfter + time.Minute); ; time.Sleep(100 * time.Millisecond) { // the killed server's row goes once stale
		if _, version, executor := workflowRow(t, ctx, p, schema, run.ID); version == "handoff-v4" && executor == newer.GetExecutorID() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the older version's run never moved to the newer version's server")
		}
	}
	if err := reportOn(ctx, p, release, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the older version's org run, resumed: %s %s", state, why(reason))
	}
	var releases int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM releases WHERE run_id = $1", run.ID).Scan(&releases); err != nil || releases != 1 {
		t.Fatalf("the resumed run sent %d release(s) (%v): one, journaled before", releases, err)
	}
	if records, secrets := provisionedBy(t, ctx, p); calls.Load() != 0 || records != 0 || secrets != 0 {
		t.Fatalf("the resumed run provisioned: %d call(s), %d record(s), %d secret(s)", calls.Load(), records, secrets)
	}
	if lines := buildLogLines(t, ctx, p, run.ID); !slices.Contains(lines, "live at https://claims.apps.meridian.example · reported by your platform") {
		t.Fatalf("the resumed run's lines: %v", lines)
	}
}

// A package that declared a type no driver serves deploys under org — the platform provides it — and once
// the host switches back to run_by: canyon its next deploy fails at the resources step, saying so (#2432).
func TestSwitchingBackFailsATypeNoDriverServesAtTheResourcesStep(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_2432d", platform) //nolint:gosec // a test fixture, not a real secret
	wasCeiling := reportCeiling
	reportCeiling = 10 * time.Second
	t.Cleanup(func() { reportCeiling = wasCeiling })
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"files": "s3"}' WHERE id = $1`, pkg); err != nil {
		t.Fatal(err)
	}
	pipe.Drivers = nil
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if err := reportOn(ctx, p, acceptedRelease(t, ctx, p, run.ID), "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("under org the platform provides s3: %s %s", state, why(reason))
	}
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET run_by = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	again, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, again.ID); state != "failed" {
		t.Fatalf("back under canyon, no driver serves s3: %s", state)
	}
	if line := lastLine(t, ctx, p, again.ID); line != "s3 files: failed: this host does not provision s3 in preview" {
		t.Fatalf("the resources step's line: %q", line)
	}
}
