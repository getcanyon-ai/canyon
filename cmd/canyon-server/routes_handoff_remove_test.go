package main

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// orgPreview ends the fixture's run and puts preview under the org's platform, the org running apps: the
// deploy row a deployed report wrote.
func orgPreview(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		"UPDATE org_preferences SET run_by = 'org'",
		"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', '" + reportImage + "', 'https://claims.apps.meridian.example', 'gr_8f2a', 'org')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// reportRemoved is the org's platform reporting release removed.
func reportRemoved(t *testing.T, c *http.Client, base, release, token string) {
	t.Helper()
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/"+release+"/report", `{"status":"removed"}`, reportHeaders(token)); code != 200 {
		t.Fatalf("the removed report: %d %s", code, lastBody)
	}
}

// Undeploy under run_by: org sends a remove release in place of POST /stop and answers 202 {release}
// at once (contracts/routes.md, the undeploy row): the environment stays on the project read, pending
// on the remove, until the org reports removed. Asked again meanwhile, the same release, and one audit row.
func TestUndeployUnderOrgSendsARemoveRelease(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	c := client(t)
	orgPreview(t, ctx, pool)
	stops := fake.Stops.Load()
	code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, "")
	release, _ := body["release"].(string)
	if code != 202 || release == "" || len(body) != 1 {
		t.Fatalf("undeploy under org: %d %s", code, lastBody)
	}
	var event string
	if err := pool.QueryRow(ctx, "SELECT event FROM releases WHERE id = $1", release).Scan(&event); err != nil || event != "remove" {
		t.Fatalf("the release: %q (%v)", event, err)
	}
	if fake.Stops.Load() != stops {
		t.Fatal("Canyon stops nothing the org runs")
	}
	preview := previewOf(t, c, base)
	if pending, _ := preview["pending"].(map[string]any); preview["url"] == nil || pending["release"] != release || pending["event"] != "remove" {
		t.Fatalf("the environment waits on its removal: %v", preview)
	}
	if code, again, _ := do(t, c, http.MethodDelete, base+previewEnv, ""); code != 202 || again["release"] != release || count(t, ctx, pool, undeployAudited) != 1 {
		t.Fatalf("undeploy again while the remove is open: %d %s", code, lastBody)
	}
	reportRemoved(t, c, base, release, token)
	if preview := previewOf(t, c, base); preview != nil {
		t.Fatalf("removed takes the environment off the read: %v", preview)
	}
}

// Delete under run_by: org sends a remove release for each environment the org runs and answers 202
// {pending[]} with it until the org reports removed; the same request again resumes, and then deletes
// (contracts/routes.md, the delete row).
func TestDeleteUnderOrgWaitsOnTheRemovedReport(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	c := client(t)
	orgPreview(t, ctx, pool)
	const deleteProject = "/api/v1/projects/claims-portal?confirm=claims-portal"
	code, body, _ := do(t, c, http.MethodDelete, base+deleteProject, "")
	pending, _ := body["pending"].([]any)
	if code != 202 || len(pending) != 1 {
		t.Fatalf("delete under org: %d %s", code, lastBody)
	}
	if row, _ := pending[0].(map[string]any); row["env"] != "preview" || row["release"] == nil || len(row) != 2 {
		t.Fatalf("the pending row is the environment and its remove release: %v", row)
	}
	row := pending[0].(map[string]any)
	if code, again, _ := do(t, c, http.MethodDelete, base+deleteProject, ""); code != 202 || !reflect.DeepEqual(again["pending"], body["pending"]) {
		t.Fatalf("delete again while the remove is open: %d %s", code, lastBody)
	}
	reportRemoved(t, c, base, row["release"].(string), token)
	if code, _, _ := do(t, c, http.MethodDelete, base+deleteProject, ""); code != 204 {
		t.Fatalf("delete once the org reported removed: %d %s", code, lastBody)
	}
}

// Deletes and undeploys of one environment the org runs, all at once, send one remove release: each
// send takes the project's undeploy lock, so every other one finds the release the first sent.
func TestConcurrentRemovesOfOneEnvironmentSendOne(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	c := client(t)
	orgPreview(t, ctx, pool)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := base + "/api/v1/projects/claims-portal?confirm=claims-portal"
			if i%2 == 1 {
				path = base + previewEnv
			}
			req, err := http.NewRequest(http.MethodDelete, path, nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	// One of each kind at most: an undeploy's, and a delete's, which an open undeploy's remove never stands in
	// for (#2432); never two of either.
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'preview'"); n < 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'preview' AND deletes_resources") > 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'preview' AND NOT deletes_resources") > 1 {
		t.Fatalf("one environment, one remove release of each kind at most: %d in all", n)
	}
}
