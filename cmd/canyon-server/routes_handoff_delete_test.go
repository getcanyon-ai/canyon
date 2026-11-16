package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	neturl "net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The project delete, the brake and the report under hand-off, as #2432's review settled them.

// deletingRemoves counts the remove releases that ask the platform to delete.
func deletingRemoves(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND deletes_resources")
}

// saveWebhook saves a platform webhook, as reportFixture's.
func saveWebhook(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	sum := sha256.Sum256([]byte("host-token-fixture-not-real"))
	if _, err := pool.Exec(ctx, "INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', 1, '', '', $1, 'person', 'dana', 'Dana')", sum[:]); err != nil {
		t.Fatal(err)
	}
}

// asAgent makes c's session an agent's.
func asAgent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, c *http.Client, base string) {
	t.Helper()
	u, _ := neturl.Parse(base)
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == sessionCookie {
			runSQL(t, ctx, pool, "UPDATE sessions SET actor_kind = 'agent' WHERE id = '"+k.Value+"'")
			return
		}
	}
	t.Fatal("no session cookie")
}

// releaseIn inserts a deploy release of pkg_7d3e8f in env, the newest of the environment, and its run.
func releaseIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, env string) {
	t.Helper()
	runSQL(t, ctx, pool,
		"INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display, ended_at) VALUES ('gr_"+strings.TrimPrefix(id, "rel_")+"', 'claims-portal', 'pkg_7d3e8f', 'clean', 'promote', 'person', 'priya@meridian.example', 'Priya Raman', now())",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run, created_at) VALUES ('"+id+"', 'claims-portal', '"+env+"', 'gr_"+strings.TrimPrefix(id, "rel_")+"', 'pkg_7d3e8f', 'deploy', '"+reportImage+"', gen_random_uuid(), 'accepted', now(), CASE WHEN '"+env+"' = 'prod' THEN 'gr_8f2a' END, clock_timestamp())") // a promotion names the gate run preview ran
}

// An agent never has the org's platform delete production's data: production the platform provisioned for is
// live for person_required though nothing runs there (#2432). A person gets past it — to webhook_required while
// no webhook is saved, before the project is marked deleting — and, once one is, to the delete's remove.
func TestAnAgentNeverHasThePlatformDeleteProductionData(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	runSQL(t, ctx, pool,
		`UPDATE packages SET resources = '{"db": "postgres"}' WHERE project_id = 'claims-portal'`,
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
			SELECT 'rel_prod', 'claims-portal', 'prod', 'gr_seed', id, 'deploy', '`+reportImage+`', gen_random_uuid(), 'removed', now() FROM packages WHERE project_id = 'claims-portal'`,
		`INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ('claims-portal', 'prod', 'db', '`+providedEndpoint+`', 'rel_prod')`)
	agent := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	asAgent(t, ctx, pool, agent, base)
	const del = "/api/v1/projects/claims-portal?confirm=claims-portal"
	if code, body, _ := do(t, agent, http.MethodDelete, base+del, ""); code != 403 || body["error"] != "person_required" {
		t.Errorf("an agent deleting production's data on the platform: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, olive, http.MethodDelete, base+del, ""); code != 409 || body["error"] != "webhook_required" ||
		count(t, ctx, pool, "SELECT count(*) FROM projects WHERE deleting_since IS NOT NULL") != 0 || count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove'") != 0 {
		t.Errorf("a person, no webhook: %d %s, or the project was marked or a remove sent", code, lastBody)
	}
	saveWebhook(t, ctx, pool)
	if code, _, _ := do(t, olive, http.MethodDelete, base+del, ""); code != 202 || deletingRemoves(t, ctx, pool) != 1 {
		t.Fatalf("a person, the webhook saved: %d %s", code, lastBody)
	}
}

// A project the org's platform holds nothing of deletes without a remove, whatever run_by says: one never handed
// off, and one the org ran once whose packages declared no resource and that Canyon runs again — under canyon,
// and under org. Nothing of the set is read from run_by (#2432).
func TestACanyonOnlyDeleteSendsNoRemove(t *testing.T) {
	for _, c := range []struct {
		name, runBy string
		handedOff   bool
	}{{"never handed off", "org", false}, {"handed off once, canyon", "canyon", true}, {"handed off once, org", "org", true}} {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx, base, cl := withProject(t)
			upload(t, cl, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
			runSQL(t, ctx, pool, "UPDATE org_preferences SET run_by = '"+c.runBy+"'")
			saveWebhook(t, ctx, pool)
			if c.handedOff { // the org ran preview once, its package declaring nothing; Canyon runs it now
				runSQL(t, ctx, pool, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
					SELECT 'rel_once', 'claims-portal', 'preview', 'gr_once', id, 'deploy', '`+reportImage+`', gen_random_uuid(), 'deployed', now() FROM packages`)
			}
			if code, _, _ := do(t, cl, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove'") != 0 {
				t.Errorf("a delete with nothing on the platform: %d %s", code, lastBody)
			}
		})
	}
}

// What the org's platform may hold stays a target after a switch back to canyon (#2432): an environment the org
// runs, its report naming no resource, and one where a release's package declared resources the report never
// named, undeployed since. The delete sends each its remove and waits on it.
func TestTheOrgsDataIsATargetAfterASwitchBack(t *testing.T) {
	t.Run("the org runs it", func(t *testing.T) {
		pool, ctx, base, _, _ := reportFixture(t)
		orgPreview(t, ctx, pool)
		runSQL(t, ctx, pool, "UPDATE org_preferences SET run_by = 'canyon'")
		code, body, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
		if pending, _ := body["pending"].([]any); code != 202 || len(pending) != 1 || deletingRemoves(t, ctx, pool) != 1 {
			t.Errorf("the org's preview after a switch back: %d %s", code, lastBody)
		}
	})
	t.Run("its package declared resources", func(t *testing.T) {
		pool, ctx, base, _, _ := declaringFixture(t)
		runSQL(t, ctx, pool, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'", "UPDATE releases SET state = 'deployed' WHERE id = 'rel_8f2a'")
		code, body, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
		if pending, _ := body["pending"].([]any); code != 202 || len(pending) != 1 || deletingRemoves(t, ctx, pool) != 1 {
			t.Errorf("a release that declared resources, no endpoint reported: %d %s", code, lastBody)
		}
	})
}

// The set is read once, as the project is marked deleting, and none of it from run_by: a switch while the delete
// waits changes nothing, and the same request again answers the same remove (#2432).
func TestASwitchWhileTheDeleteWaitsChangesNothing(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	orgPreview(t, ctx, pool)
	const del = "/api/v1/projects/claims-portal?confirm=claims-portal"
	code, first, _ := do(t, client(t), http.MethodDelete, base+del, "")
	if code != 202 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET run_by = 'canyon'")
	if code, again, _ := do(t, client(t), http.MethodDelete, base+del, ""); code != 202 || !reflect.DeepEqual(again["pending"], first["pending"]) || deletingRemoves(t, ctx, pool) != 1 {
		t.Errorf("after a switch to canyon: %d %s, want the same pending as %v", code, lastBody, first["pending"])
	}
}

// A first deploy the org's platform may be starting when the delete takes its mark — its release still open, its
// package declaring nothing, no deploy row yet — gets a remove with delete, and its late deployed report is
// refused, writing nothing (#2432).
func TestAnOpenFirstDeployIsATarget(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	runSQL(t, ctx, pool, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_8f2a'") // its 30 minutes past, the release open
	code, body, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	if pending, _ := body["pending"].([]any); code != 202 || len(pending) != 1 || deletingRemoves(t, ctx, pool) != 1 {
		t.Fatalf("the delete over an open first deploy: %d %s", code, lastBody)
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, body, _ := doWith(t, client(t), http.MethodPost, path, deployed, reportHeaders(token)); code != 409 || body["error"] != "project_deleted" {
		t.Errorf("the late deployed: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys") + count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove'"); n != 1 {
		t.Errorf("the late deployed wrote: %d rows of deploys and removes, want the one remove", n)
	}
}

// A deployed report on a project already deleted — on a release nothing made a target, undelivered, its package
// declaring nothing — is 409 project_deleted, writes nothing, and sends one remove with delete; the same report
// again sends no second (#2432).
func TestALateReportOnADeletedProjectIsRemovedOnce(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	runSQL(t, ctx, pool,
		"UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_8f2a'",
		"UPDATE releases SET state = 'undelivered' WHERE id = 'rel_8f2a'")
	if code, _, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 {
		t.Fatalf("the delete, nothing on the platform: %d %s", code, lastBody)
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	for i := range 2 {
		if code, body, _ := doWith(t, client(t), http.MethodPost, path, deployed, reportHeaders(token)); code != 409 || body["error"] != "project_deleted" {
			t.Errorf("report %d on the deleted project: %d %s", i+1, code, lastBody)
		}
		if n := count(t, ctx, pool, "SELECT count(*) FROM deploys"); n != 0 || deletingRemoves(t, ctx, pool) != 1 {
			t.Errorf("after report %d: %d deploy row(s), %d remove(s) with delete; want none and one", i+1, n, deletingRemoves(t, ctx, pool))
		}
	}
}

// A delete's remove the platform reported failed keeps its environment a target: the delete again sends
// another, never finishing over data the platform may still hold (#2432).
func TestAFailedCleanupStaysATarget(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	orgPreview(t, ctx, pool)
	runSQL(t, ctx, pool, "UPDATE releases SET state = 'deployed' WHERE id = 'rel_8f2a'")
	c := client(t)
	const del = "/api/v1/projects/claims-portal?confirm=claims-portal"
	code, body, _ := do(t, c, http.MethodDelete, base+del, "")
	pending, _ := body["pending"].([]any)
	if code != 202 || len(pending) != 1 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	reportRemoved(t, c, base, pending[0].(map[string]any)["release"].(string), token)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 409 { // the platform started it again: a second remove
		t.Fatalf("the late deployed: %d %s", code, lastBody)
	}
	var second string
	if err := pool.QueryRow(ctx, "SELECT id FROM releases WHERE event = 'remove' AND deletes_resources ORDER BY created_at DESC LIMIT 1").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/"+second+"/report", `{"status":"failed","reason":"the database is in use"}`, reportHeaders(token)); code != 200 {
		t.Fatalf("the second remove's failed report: %d %s", code, lastBody)
	}
	code, body, _ = do(t, c, http.MethodDelete, base+del, "")
	pending, _ = body["pending"].([]any)
	if code != 202 || len(pending) != 1 || pending[0].(map[string]any)["release"] == second || deletingRemoves(t, ctx, pool) != 3 {
		t.Errorf("the delete again, its cleanup failed: %d %s, want a third remove with delete", code, lastBody)
	}
}

// A delete's remove a late report sent while the delete was stopping, and that the platform reported failed
// before the delete finished, keeps the project: the delete answers 202 with it, and the same request again sends
// another, never 204 over data the platform may still hold (#2432).
func TestADeleteNeverFinishesOverAFailedCleanup(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	runSQL(t, ctx, pool, "UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_8f2a'",
		"UPDATE releases SET state = 'undelivered' WHERE id = 'rel_8f2a'") // no target when the delete takes its mark
	release := fake.HoldStop()
	defer release()
	const del = "/api/v1/projects/claims-portal?confirm=claims-portal"
	type answer struct {
		code int
		body map[string]any
	}
	first := make(chan answer, 1)
	stops := fake.Stops.Load()
	go func() {
		code, body, _ := do(t, client(t), http.MethodDelete, base+del, "")
		first <- answer{code, body}
	}()
	for deadline := time.Now().Add(10 * time.Second); fake.Stops.Load() == stops; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the delete never reached its stop")
		}
	}
	// what a late deployed report sends, as its remove's failed report leaves it: the delete is past its targets
	runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, deletes_resources) VALUES ('rel_late', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'remove', '"+reportImage+"', gen_random_uuid(), 'failed', now(), clock_timestamp(), true)")
	release()
	got := <-first
	pending, _ := got.body["pending"].([]any)
	if got.code != 202 || len(pending) != 1 || pending[0].(map[string]any)["release"] != "rel_late" || count(t, ctx, pool, "SELECT count(*) FROM projects WHERE deleted_at IS NOT NULL") != 0 {
		t.Fatalf("the delete over a failed cleanup: %d %v", got.code, got.body)
	}
	code, body, _ := do(t, client(t), http.MethodDelete, base+del, "")
	pending, _ = body["pending"].([]any)
	if code != 202 || len(pending) != 1 || pending[0].(map[string]any)["release"] == "rel_late" || deletingRemoves(t, ctx, pool) != 2 {
		t.Errorf("the delete again: %d %s, want the failed cleanup sent anew", code, lastBody)
	}
}

// A deployed report the platform sends after the delete's remove was done — it started the app again — has
// Canyon delete it again, once, and the delete's next request waits on that remove.
func TestALateDeployAfterTheDeleteIsDeletedAgain(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	orgPreview(t, ctx, pool)
	c := client(t)
	const del = "/api/v1/projects/claims-portal?confirm=claims-portal"
	code, body, _ := do(t, c, http.MethodDelete, base+del, "")
	pending, _ := body["pending"].([]any)
	if code != 202 || len(pending) != 1 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	first, _ := pending[0].(map[string]any)["release"].(string)
	reportRemoved(t, c, base, first, token)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	for range 2 {
		if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 409 {
			t.Fatalf("the late deployed: %d %s", code, lastBody)
		}
	}
	if n := deletingRemoves(t, ctx, pool); n != 2 {
		t.Fatalf("removes with delete after the late report, twice: %d, want 2", n)
	}
	code, body, _ = do(t, c, http.MethodDelete, base+del, "")
	pending, _ = body["pending"].([]any)
	if code != 202 || len(pending) != 1 || pending[0].(map[string]any)["release"] == first {
		t.Fatalf("the delete again waits on the new remove: %d %s", code, lastBody)
	}
	reportRemoved(t, c, base, pending[0].(map[string]any)["release"].(string), token)
	if code, _, _ := do(t, c, http.MethodDelete, base+del, ""); code != 204 {
		t.Fatalf("the delete, once the new remove is removed: %d %s", code, lastBody)
	}
}

// Under canyon again, an environment the platform still holds data for is a target all the same; one with
// only a release of the org's, the platform holding nothing, is not.
func TestProvidedDataIsATargetWhoeverRunsApps(t *testing.T) {
	pool, ctx, base, _, _ := declaringFixture(t)
	runSQL(t, ctx, pool,
		"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
		"INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ('claims-portal', 'preview', 'db', '"+providedEndpoint+"', 'rel_8f2a')")
	code, body, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	if pending, _ := body["pending"].([]any); code != 202 || len(pending) != 1 || deletingRemoves(t, ctx, pool) != 1 {
		t.Fatalf("under canyon, the platform's data: %d %s", code, lastBody)
	}
}

// The delete's listing names Canyon's record and the platform's row of one name both: the switch moved no
// data, and the delete destroys both (#2432).
func TestTheDeleteListsCanyonsRecordAndThePlatformsRowBoth(t *testing.T) {
	pool, ctx, base, path, token := declaringFixture(t)
	if code, _, _ := doWith(t, client(t), http.MethodPost, path, deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'db', 'postgres', 'builtin', 'ready')")
	if code, _, _ := do(t, client(t), http.MethodDelete, base+"/api/v1/projects/claims-portal", ""); code != 409 ||
		!strings.Contains(lastBody, `"resources":[{"name":"db","type":"postgres","env":"preview","state":"ready"},{"name":"db","type":"postgres","env":"preview","state":"provided"}]`) {
		t.Fatalf("the listing: %d %s", code, lastBody)
	}
}

// A deployed report on a remove release is 409 not_a_deploy before anything is written: what the brake's or a
// delete's remove takes off never goes live again, held or not — no deploy row, no endpoint, no other remove.
func TestADeployedReportOnARemoveIsRefused(t *testing.T) {
	for _, c := range []struct {
		name, env string
		deletes   bool
		held      bool
	}{
		{"the brake's remove", "preview", false, false},
		{"a delete's remove", "preview", true, false},
		{"the brake's remove of production, held", "prod", false, true},
		{"a delete's remove of production, held", "prod", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx, base, _, token := declaringFixture(t)
			runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, deletes_resources) VALUES ('rel_rm', 'claims-portal', '"+c.env+"', 'gr_8f2a', 'pkg_7d3e8f', 'remove', '"+reportImage+"', gen_random_uuid(), 'accepted', now(), clock_timestamp(), "+map[bool]string{false: "false", true: "true"}[c.deletes]+")")
			if c.held {
				plantHold(t, ctx, pool)
			}
			if code, body, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_rm/report", deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 409 || body["error"] != "not_a_deploy" {
				t.Errorf("deployed on a remove: %d %s", code, lastBody)
			}
			if n := count(t, ctx, pool, "SELECT count(*) FROM deploys") + count(t, ctx, pool, "SELECT count(*) FROM platform_resources") + count(t, ctx, pool, "SELECT count(*) FROM releases WHERE id <> 'rel_rm' AND event = 'remove'"); n != 0 {
				t.Errorf("a deployed on a remove wrote %d row(s)", n)
			}
		})
	}
}

// The brake's removes — removeAgain's after an undeploy, removeHeld's while production is held — and a hold's
// own never ask the platform to delete; and neither brake sends one over a project delete's, which would stand
// in its place and keep the data.
func TestTheBrakeNeverDeletesAndNeverSupersedesADelete(t *testing.T) {
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	t.Run("removeAgain", func(t *testing.T) {
		for _, c := range []struct {
			name, state     string
			deletes         bool
			plain, deleting int // the brake's removes after two late reports: without delete, with it
		}{
			{"over an undeploy's remove", "removed", false, 1, 0},
			{"over a delete's remove, open", "accepted", true, 0, 0},
			{"over a delete's remove, done", "removed", true, 0, 1}, // the platform started it again: deleted again, once
			{"over a delete's remove, failed", "failed", true, 0, 1},
		} {
			t.Run(c.name, func(t *testing.T) {
				pool, ctx, _, path, token := reportFixture(t)
				runSQL(t, ctx, pool,
					"UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_8f2a'",
					"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, deletes_resources) VALUES ('rel_remove1', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'remove', '"+reportImage+"', gen_random_uuid(), '"+c.state+"', now(), clock_timestamp(), "+map[bool]string{false: "false", true: "true"}[c.deletes]+")")
				if c.deletes { // a delete's remove is sent only once the project is marked deleting
					runSQL(t, ctx, pool, "UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'")
				}
				for range 2 {
					if code, _, _ := doWith(t, client(t), http.MethodPost, path, deployed, reportHeaders(token)); code != 409 {
						t.Fatalf("deployed after the remove: %d %s", code, lastBody)
					}
				}
				plain := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id <> 'rel_remove1' AND NOT deletes_resources")
				deleting := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND id <> 'rel_remove1' AND deletes_resources")
				if plain != c.plain || deleting != c.deleting {
					t.Errorf("the brake's removes: %d without delete, %d with; want %d and %d", plain, deleting, c.plain, c.deleting)
				}
			})
		}
	})
	t.Run("removeHeld", func(t *testing.T) {
		for _, deleting := range []bool{false, true} {
			// a subtest each, so the first's cleanup waits for the remove it enqueued before the second's start re-points pipe (#2478)
			t.Run(map[bool]string{false: "plain", true: "over a delete's remove"}[deleting], func(t *testing.T) {
				pool, ctx, base, _, token := reportFixture(t)
				runSQL(t, ctx, pool, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'")
				releaseIn(t, ctx, pool, "rel_prod1", "prod")
				if deleting {
					runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, deletes_resources) VALUES ('rel_del', 'claims-portal', 'prod', 'gr_8f2a', 'pkg_7d3e8f', 'remove', '"+reportImage+"', gen_random_uuid(), 'accepted', now(), clock_timestamp(), true)",
						"UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'")
				}
				plantHold(t, ctx, pool)
				if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 409 {
					t.Fatalf("deployed while held: %d %s", code, lastBody)
				}
				if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND NOT deletes_resources"); n != map[bool]int{false: 1, true: 0}[deleting] || deletingRemoves(t, ctx, pool) != map[bool]int{false: 0, true: 1}[deleting] {
					t.Errorf("held, over a remove that deletes: %v; %d remove(s) of the brake's", deleting, n)
				}
			})
		}
	})
	t.Run("hold", func(t *testing.T) {
		pool, ctx, base, _, _ := reportFixture(t)
		runSQL(t, ctx, pool,
			"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'prod', 'pkg_7d3e8f', '"+reportImage+"', 'https://claims.apps.meridian.example', 'gr_8f2a', 'org')",
			"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'")
		if code, _, _ := do(t, client(t), http.MethodPost, base+holdPath, holdReason); code != 202 || count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove'") != 1 || deletingRemoves(t, ctx, pool) != 0 {
			t.Fatalf("a hold's remove: %d %s", code, lastBody)
		}
	})
}

// A deployed report on production while it is held writes no endpoint.
func TestAHeldReleaseWritesNoEndpoints(t *testing.T) {
	pool, ctx, base, _, token := declaringFixture(t)
	runSQL(t, ctx, pool, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'")
	releaseIn(t, ctx, pool, "rel_prod1", "prod")
	plantHold(t, ctx, pool)
	if code, body, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployedWith(`[{"name":"db","endpoint":"`+providedEndpoint+`"}]`), reportHeaders(token)); code != 409 || body["error"] != "held" || endpointOf(t, ctx, pool) != "" {
		t.Fatalf("held: %d %s, endpoints %q", code, lastBody, endpointOf(t, ctx, pool))
	}
}

// Each environment's endpoints are its own: a report replaces its environment's alone — an explicit [] wipes
// them, other endpoints take their place, a resource the next package no longer declares goes and can't be
// named — an undeploy's removed keeps them, and only a delete's removed takes them.
func TestEndpointsAreReplacedPerEnvironmentAndKeptByAnUndeploy(t *testing.T) {
	pool, ctx, base, path, token := declaringFixture(t)
	c := client(t)
	runSQL(t, ctx, pool, "UPDATE org_preferences SET run_by = 'org'")
	report := func(release, resources string, want int) {
		t.Helper()
		if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/"+release+"/report", deployedWith(resources), reportHeaders(token)); code != want {
			t.Fatalf("%s %s: %d %s", release, resources, code, lastBody)
		}
	}
	envOf := func() string {
		var e string
		if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(env || ':' || name || '=' || endpoint, ',' ORDER BY env, name), '') FROM platform_resources").Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	_ = path
	report("rel_8f2a", `[{"name":"db","endpoint":"postgres://pg.example/a"}]`, 200)
	runSQL(t, ctx, pool, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'")
	releaseIn(t, ctx, pool, "rel_prod1", "prod")
	report("rel_prod1", `[{"name":"db","endpoint":"postgres://pg.example/p"}]`, 200)
	if e := envOf(); e != "preview:db=postgres://pg.example/a,prod:db=postgres://pg.example/p" {
		t.Errorf("preview's and prod's: %q", e)
	}
	releaseIn(t, ctx, pool, "rel_p2", "preview")
	report("rel_p2", `[]`, 409) // its package lists db: resources_missing, and nothing changes (#2452)
	runSQL(t, ctx, pool,
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_none', 'claims-portal', 'ee', 'ff', 1, 'upload')",
		"UPDATE releases SET package_id = 'pkg_none' WHERE id = 'rel_p2'")
	report("rel_p2", `[]`, 200)
	if e := envOf(); e != "prod:db=postgres://pg.example/p" {
		t.Errorf("an explicit [] on a release that lists none wipes preview's alone: %q", e)
	}
	releaseIn(t, ctx, pool, "rel_p3", "preview")
	report("rel_p3", `[{"name":"db","endpoint":"postgres://pg.example/b"}]`, 200)
	if e := envOf(); e != "preview:db=postgres://pg.example/b,prod:db=postgres://pg.example/p" {
		t.Errorf("another endpoint takes its place: %q", e)
	}
	runSQL(t, ctx, pool,
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, resources) VALUES ('pkg_next', 'claims-portal', 'cc', 'dd', 1, 'upload', '{\"files\": \"s3\"}')")
	releaseIn(t, ctx, pool, "rel_p4", "preview")
	runSQL(t, ctx, pool, "UPDATE releases SET package_id = 'pkg_next' WHERE id = 'rel_p4'")
	report("rel_p4", `[{"name":"db","endpoint":"postgres://pg.example/c"}]`, 400) // db is no longer declared
	report("rel_p4", `[{"name":"files","endpoint":"s3://claims-files"}]`, 200)
	if e := envOf(); e != "preview:files=s3://claims-files,prod:db=postgres://pg.example/p" {
		t.Errorf("a resource the next package dropped: %q", e)
	}
	code, body, _ := do(t, c, http.MethodDelete, base+previewEnv, "")
	release, _ := body["release"].(string)
	if code != 202 || release == "" {
		t.Fatalf("undeploy: %d %s", code, lastBody)
	}
	reportRemoved(t, c, base, release, token)
	if e := envOf(); e != "preview:files=s3://claims-files,prod:db=postgres://pg.example/p" {
		t.Errorf("an undeploy's removed kept nothing: %q", e)
	}
}
