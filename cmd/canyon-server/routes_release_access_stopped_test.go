package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pulledNobody fails the test unless body is the deny-all file: contracts/testdata/release/meridian.json's
// access_deny_all byte for byte but its updated_at, which is the pull's own (contracts/routes.md, the access row).
func pulledNobody(t *testing.T, what string, code int, body []byte) {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/release/meridian.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Want map[string]any `json:"access_deny_all"`
	}
	var got map[string]any
	if err := errors.Join(json.Unmarshal(raw, &doc), json.Unmarshal(body, &got)); code != 200 || err != nil {
		t.Fatalf("%s: %d %s (%v)", what, code, body, err)
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(got["updated_at"]))
	if err != nil || time.Since(at) > time.Minute {
		t.Fatalf("%s: the deny-all's updated_at is the pull's: %v (%v)", what, got["updated_at"], err)
	}
	delete(got, "updated_at")
	delete(doc.Want, "updated_at")
	if !jsonEqual(got, doc.Want) {
		t.Fatalf("%s: the pull is not the deny-all file:\n got %s\nwant %s", what, mustJSON(got), mustJSON(doc.Want))
	}
}

// pulledTheTeam fails the test unless the pull answered the app's access file, its one grant in it.
func pulledTheTeam(t *testing.T, what string, code int, body []byte) {
	t.Helper()
	var got struct{ Entries []map[string]any }
	if err := json.Unmarshal(body, &got); code != 200 || err != nil || len(got.Entries) != 1 {
		t.Fatalf("%s: the app's access file, its grant in it: %d %s (%v)", what, code, body, err)
	}
}

// orgProd puts claims-portal's production under the org's platform, live: its release reported deployed and
// its deploy row, the org running apps and the fixture's runs ended.
func orgProd(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	runSQL(t, ctx, pool,
		"UPDATE org_preferences SET run_by = 'org'",
		"UPDATE runs SET state = 'clean', ended_at = now()",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_cp_prod', 'claims-portal', 'prod', 'gr_cp', 'pkg_cp', 'deploy', '"+accessImage+"', gen_random_uuid(), 'deployed', now())",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'prod', 'pkg_cp', '"+accessImage+"', 'https://claims.meridian.example', 'gr_cp', 'org')")
}

// A release Canyon stopped under hand-off pulls the deny-all file (#2450): each way it stops, read per token, so
// a newer release never hands an older one's pods the list back; after a switch back to Canyon running the
// environment; and through the routes that stop it, the moment they answer.
func TestAStoppedEnvironmentPullsNobody(t *testing.T) {
	t.Run("each way it stops", stoppedByHand)
	t.Run("after a switch back to canyon", stoppedBySwitchingBack)
	t.Run("never by a write of Canyon's under org", notStoppedByALateWrite)
	t.Run("the routes that stop it, at once", stoppedByTheRoutes)
}

// newRelease records a release of claims-portal's env after every other, as a deploy or the brake records one.
func newRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, env, event string) {
	t.Helper()
	runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('"+id+"', 'claims-portal', '"+env+"', 'gr_cp', 'pkg_cp', '"+event+"', '"+accessImage+"', gen_random_uuid(), 'sent', now())")
}

// mustToken is release's access token, fetched as the org's pipeline does.
func mustToken(t *testing.T, c *http.Client, base, release, host string) string {
	t.Helper()
	code, token := fetchToken(t, c, base, release, host)
	if code != 200 {
		t.Fatalf("the access token of %s: %d %s", release, code, lastBody)
	}
	return token
}

// stoppedByHand: a remove recorded after a release, whatever the platform did with it, stops that release's
// token for good — a deploy recorded after the remove has a token of its own, which pulls the team — and a remove
// recorded at the same instant stops it too, as does a token minted for a remove; production held through the
// hold route, which stays stopped after the lift since the hold sent a remove, until a new release's own token
// pulls; a hold read per environment, before any remove; the project deleting, then deleted. A hold leaves preview
// alone. Throughout, a token that is not the environment's is the same 401, and a deny-all pull checks in as any
// 200 does.
func stoppedByHand(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	orgProd(t, ctx, pool)
	preview := mustToken(t, c, base, "rel_cp1", host)
	prod := mustToken(t, c, base, "rel_cp_prod", host)
	other := mustToken(t, c, base, "rel_qe1", host)
	refused := func(when string) {
		t.Helper()
		for _, p := range [][3]string{{"claims-portal", "prod", preview}, {"claims-portal", "preview", prod}, {"claims-portal", "prod", "not-a-token"}, {"claims-portal", "prod", ""}} {
			if code, body := pullAs(t, c, base, p[0], p[1], p[2]); code != 401 {
				t.Fatalf("%s, a token not %s/%s's: %d %s", when, p[0], p[1], code, body)
			}
		}
	}

	for _, state := range []string{"sent", "accepted", "removed", "refused"} {
		runSQL(t, ctx, pool, "DELETE FROM releases WHERE id = 'rel_cp_rm'",
			"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_cp_rm', 'claims-portal', 'preview', 'gr_cp', 'pkg_cp', 'remove', '"+accessImage+"', gen_random_uuid(), '"+state+"', now())")
		code, body := pullAs(t, c, base, "claims-portal", "preview", preview)
		pulledNobody(t, "a remove of preview recorded after its release, "+state, code, body)
		code, body = pullAs(t, c, base, "claims-portal", "prod", prod)
		pulledTheTeam(t, "production while preview is removed", code, body)
	}
	refused("preview removed")
	newRelease(t, ctx, pool, "rel_cp2", "preview", "deploy")
	fixed := mustToken(t, c, base, "rel_cp2", host)
	code, body := pullAs(t, c, base, "claims-portal", "preview", preview)
	pulledNobody(t, "the removed release's token once a newer deploy is recorded", code, body)
	code, body = pullAs(t, c, base, "claims-portal", "preview", fixed)
	pulledTheTeam(t, "the newer deploy's own token", code, body)
	newRelease(t, ctx, pool, "rel_cp3", "preview", "deploy")
	same := mustToken(t, c, base, "rel_cp3", host)
	runSQL(t, ctx, pool, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at) SELECT 'rel_cp_rm3', 'claims-portal', 'preview', 'gr_cp', 'pkg_cp', 'remove', image, gen_random_uuid(), 'sent', now(), created_at FROM releases WHERE id = 'rel_cp3'")
	code, body = pullAs(t, c, base, "claims-portal", "preview", same)
	pulledNobody(t, "a release a remove was recorded at the same instant as", code, body)
	newRelease(t, ctx, pool, "rel_cp_rm4", "preview", "remove")
	code, body = pullAs(t, c, base, "claims-portal", "preview", mustToken(t, c, base, "rel_cp_rm4", host))
	pulledNobody(t, "a token minted for a remove", code, body)

	if code, held, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 202 || held["release"] == nil {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	code, body = pullAs(t, c, base, "claims-portal", "prod", prod)
	pulledNobody(t, "production held", code, body)
	if seenAt(t, ctx, pool, "rel_cp_prod") == nil {
		t.Fatal("a deny-all pull did not check in: a pod still pulling is one the platform has not taken down")
	}
	refused("production held")
	if code, _, _ := do(t, c, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("the lift: %d %s", code, lastBody)
	}
	code, body = pullAs(t, c, base, "claims-portal", "prod", prod)
	pulledNobody(t, "production after a lift that left it stopped: the hold's remove stands", code, body)
	newRelease(t, ctx, pool, "rel_cp_prod2", "prod", "deploy")
	again := mustToken(t, c, base, "rel_cp_prod2", host)
	code, body = pullAs(t, c, base, "claims-portal", "prod", prod)
	pulledNobody(t, "the held release's token once production is deployed again", code, body)
	code, body = pullAs(t, c, base, "claims-portal", "prod", again)
	pulledTheTeam(t, "the new production release's own token", code, body)
	runSQL(t, ctx, pool, "INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2232', 'person', 'rhea', 'Rhea')")
	code, body = pullAs(t, c, base, "claims-portal", "prod", again)
	pulledNobody(t, "production held, before any remove is recorded", code, body)

	newRelease(t, ctx, pool, "rel_cp5", "preview", "deploy")
	live := mustToken(t, c, base, "rel_cp5", host)
	code, body = pullAs(t, c, base, "claims-portal", "preview", live)
	pulledTheTeam(t, "preview deployed after every remove", code, body)
	for _, q := range []string{"UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'", "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'"} {
		runSQL(t, ctx, pool, q)
		code, body = pullAs(t, c, base, "claims-portal", "preview", live)
		pulledNobody(t, "preview after "+q, code, body)
		code, body = pullAs(t, c, base, "quote-engine", "preview", other)
		pulledTheTeam(t, "another app, after "+q, code, body)
		refused(q)
	}
}

// stoppedBySwitchingBack: once a deploy Canyon ran follows an org release in its environment — a switch back to
// run_by: canyon — that release's token pulls the deny-all, since the org's old pods are no longer the
// environment's; a deploy Canyon ran before the release never stops it.
func stoppedBySwitchingBack(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	runSQL(t, ctx, pool, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('quote-engine', 'preview', 'pkg_qe', '"+accessImage+"', 'http://quote-engine.preview.localhost:7777', 'gr_qe', 'canyon', now() - interval '1 hour')")
	token := mustToken(t, c, base, "rel_qe1", host)
	code, body := pullAs(t, c, base, "quote-engine", "preview", token)
	pulledTheTeam(t, "an org release after what Canyon ran there before it", code, body)
	runSQL(t, ctx, pool, "UPDATE deploys SET deployed_at = now() WHERE project_id = 'quote-engine' AND env = 'preview'")
	code, body = pullAs(t, c, base, "quote-engine", "preview", token)
	pulledNobody(t, "an org release once Canyon runs the environment again", code, body)
	code, body = pullAs(t, c, base, "claims-portal", "preview", mustToken(t, c, base, "rel_cp1", host))
	pulledTheTeam(t, "another app's org release", code, body)
	// The org runs it again: a newer org release, its own token, which Canyon's deploy before it never stops.
	addRelease(t, ctx, pool, "rel_qe2", "quote-engine", "gr_qe", "pkg_qe")
	newer := mustToken(t, c, base, "rel_qe2", host)
	code, body = pullAs(t, c, base, "quote-engine", "preview", newer)
	pulledTheTeam(t, "a newer org release after Canyon's deploy", code, body)
	// Canyon undeploys its own deploy (#2469): the undeploy's own statement, routes_envs.go, the row gone. The org's
	// old pods stay no longer the environment's, and the newer release's stay its.
	runSQL(t, ctx, pool, "DELETE FROM deploys WHERE project_id = 'quote-engine' AND env = 'preview' AND run_by = 'canyon'")
	code, body = pullAs(t, c, base, "quote-engine", "preview", token)
	pulledNobody(t, "an org release after Canyon undeploys its own deploy", code, body)
	code, body = pullAs(t, c, base, "quote-engine", "preview", newer)
	pulledTheTeam(t, "the newer org release after Canyon undeploys its own deploy", code, body)
}

// notStoppedByALateWrite: a write of Canyon's that moves its deploy row's deployed_at while the org runs apps — a
// patch run's (runs/patchrun.go), a deploy that started before the switch to org — is no switch back (#2469, the
// security review's M1): the org's newer release still pulls the team, and no release is recorded taken back.
func notStoppedByALateWrite(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	runSQL(t, ctx, pool, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, deployed_at) VALUES ('quote-engine', 'preview', 'pkg_qe', '"+accessImage+"', 'http://quote-engine.preview.localhost:7777', 'gr_qe', 'canyon', now() - interval '1 hour')",
		"UPDATE org_preferences SET run_by = 'org'")
	addRelease(t, ctx, pool, "rel_qe2", "quote-engine", "gr_qe", "pkg_qe")
	newer := mustToken(t, c, base, "rel_qe2", host)
	code, body := pullAs(t, c, base, "quote-engine", "preview", newer)
	pulledTheTeam(t, "the org's release after the switch to org", code, body)
	runSQL(t, ctx, pool, "UPDATE deploys SET deployed_at = now() WHERE project_id = 'quote-engine' AND env = 'preview' AND run_by = 'canyon'") // a patch run's recordPatch, its shape
	code, body = pullAs(t, c, base, "quote-engine", "preview", newer)
	pulledTheTeam(t, "the org's release after a patch of what Canyon ran there", code, body)
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE canyon_since IS NOT NULL"); n != 0 {
		t.Fatalf("a write under run_by: org recorded %d release(s) as taken back", n)
	}
}

// stoppedByTheRoutes: the routes that stop an environment the org's platform runs make its pods admit nobody
// the moment they answer, before the platform reports anything — a hold of production, an undeploy of preview,
// and a delete of the app. The platform's removed report changes nothing.
func stoppedByTheRoutes(t *testing.T) {
	pool, ctx, base, host, _ := accessFixture(t)
	c := client(t)
	orgProd(t, ctx, pool)
	runSQL(t, ctx, pool,
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'preview', 'pkg_cp', '"+accessImage+"', 'https://claims-preview.meridian.example', 'gr_cp', 'org')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('quote-engine', 'preview', 'pkg_qe', '"+accessImage+"', 'https://quote-preview.meridian.example', 'gr_qe', 'org')")
	_, preview := fetchToken(t, c, base, "rel_cp1", host)
	_, prod := fetchToken(t, c, base, "rel_cp_prod", host)
	_, other := fetchToken(t, c, base, "rel_qe1", host)

	if code, body, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 202 || body["release"] == nil {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	code, body := pullAs(t, c, base, "claims-portal", "prod", prod)
	pulledNobody(t, "production the moment it is held", code, body)
	code, body = pullAs(t, c, base, "claims-portal", "preview", preview)
	pulledTheTeam(t, "preview while production is held", code, body)

	code, undeploy, _ := do(t, c, http.MethodDelete, base+previewEnv, "")
	release, _ := undeploy["release"].(string)
	if code != 202 || release == "" {
		t.Fatalf("the undeploy: %d %s", code, lastBody)
	}
	code, body = pullAs(t, c, base, "claims-portal", "preview", preview)
	pulledNobody(t, "preview the moment it is undeployed", code, body)
	reportRemoved(t, c, base, release, host)
	code, body = pullAs(t, c, base, "claims-portal", "preview", preview)
	pulledNobody(t, "preview reported removed", code, body)

	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/quote-engine?confirm=quote-engine", ""); code != 202 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	code, body = pullAs(t, c, base, "quote-engine", "preview", other)
	pulledNobody(t, "an app the moment its delete starts", code, body)
	if code, body := pullAs(t, c, base, "quote-engine", "preview", "not-a-token"); code != 401 {
		t.Fatalf("a bad token on an app being deleted: %d %s", code, body)
	}
}
