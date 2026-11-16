package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/gitops"
	"github.com/getcanyon-ai/canyon/internal/gitopstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// repoRig is a host under delivery: repo (design/HAND-OFF.md §3b): claims-portal declaring a postgres db, the
// deploy repo a real git remote, and its releases committed there as the release step commits them.
type repoRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	base   string
	host   string // the host token
	remote *gitopstest.Remote
	c      *http.Client
}

func newRepoRig(t *testing.T, prodPR bool) *repoRig {
	t.Helper()
	return newRepoRigOn(t, prodPR, "github")
}

// newRepoRigOn is newRepoRig with its pull requests through api: github's pulls or gitlab's merge requests.
func newRepoRigOn(t *testing.T, prodPR bool, api string) *repoRig {
	t.Helper()
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	remote := gitopstest.New(t, filepath.Join("..", "..", "contracts", "testdata", "release", "repo", "templates", ".canyon"))
	pipe.RepoTransport = remote.Client.Transport
	t.Cleanup(func() { pipe.RepoTransport = nil })
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, resources, label) VALUES ('pkg_cp', 'claims-portal', 'aa', 'bb', 1, 'upload', '{"db": "postgres"}', 'v1.4')`,
		"INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_cp', 'claims-portal', 'pkg_cp', 'running', 'person', 'ana', 'Ana')",
		"UPDATE org_preferences SET run_by = 'org', delivery = 'repo'",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	mode := "push"
	if prodPR {
		mode = "pull_request"
	}
	version, sealedKey, sealedValue, err := vaultOf(t, base).Seal("platform-repo", "repo", "token", []byte(gitopstest.Token))
	if err != nil {
		t.Fatal(err)
	}
	host := rand.Text() // the repo's own host token (#2470), made at run time
	sum := sha256.Sum256([]byte(host))
	if _, err := pool.Exec(ctx, `INSERT INTO platform_repo (url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, token_hash, fetch_token_hash, set_by_kind, set_by_subject, set_by_display)
		VALUES ($1, $2, $3, $4, $5, 'preview', 'main', $6, 'apps', $7, $8, 'canyon', $9, sha256('fetch'::bytea), 'person', 'dana', 'Dana')`, remote.URL, gitopstest.User, version, sealedKey, sealedValue, mode, api,
		map[string]string{"github": remote.API, "gitlab": remote.GitLab}[api], sum[:]); err != nil {
		t.Fatal(err)
	}
	return &repoRig{t: t, pool: pool, ctx: ctx, base: base, host: host, remote: remote, c: client(t)}
}

// commit is release id of env as the release step makes it: its row accepted under via repo, its files committed
// to the deploy repo, where it landed recorded — and its access token fetched as External Secrets would.
func (r *repoRig) commit(id, env string) (gitops.Landed, string) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at)
		VALUES ($1, 'claims-portal', $2, 'gr_cp', 'pkg_cp', 'deploy', $3, gen_random_uuid(), 'accepted', now(), 'repo', clock_timestamp())`, id, env, accessImage); err != nil {
		r.t.Fatal(err)
	}
	repo, err := pipe.DeployRepo(r.ctx, r.pool)
	if err != nil {
		r.t.Fatal(err)
	}
	rel := gitops.Release{Canyon: pipe.PublicURL, ID: id, App: "claims-portal", Env: env, GateRun: "gr_cp", Package: "pkg_cp", Version: "v1.4", Image: accessImage,
		Identity: "registry.example/canyon-identity@sha256:" + strings.Repeat("d", 64), AppEnv: map[string]string{"PORT": "3000"},
		SidecarEnv: map[string]string{"CANYON_TARGET": env}, Resources: []gitops.Resource{{Name: "db", Type: "postgres", Gives: []string{"DATABASE_URL"}}}}
	landed, err := repo.Deliver(r.ctx, gitops.Delivery{Release: rel, Review: "https://canyon.example/#/apps/claims-portal/runs/gr_cp", Date: time.Now().UTC().Truncate(time.Second),
		Fresh: func(context.Context) error { return nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	endpoints, _ := json.Marshal(landed.Endpoints)
	var pr *string
	var number *int
	if landed.PullRequest != nil {
		pr, number = &landed.PullRequest.URL, &landed.PullRequest.Number
	}
	if _, err := r.pool.Exec(r.ctx, "UPDATE releases SET commit_sha = $2, branch = $3, tree = $4, address = $5, endpoints = $6, pull_request = $7, pull_number = $8 WHERE id = $1",
		id, landed.Commit, landed.Branch, landed.Tree, landed.Address, endpoints, pr, number); err != nil {
		r.t.Fatal(err)
	}
	code, token := fetchToken(r.t, r.c, r.base, id, r.host)
	if code != 200 {
		r.t.Fatalf("fetch %s's access token: %d %s", id, code, lastBody)
	}
	return landed, token
}

// pull is one pull of the access list as a pod of the release pulls it, the app answering or not: seen_at
// cleared first, so the check-in is never the one the last 30 seconds skip.
func (r *repoRig) pull(id, env, token string, answered bool) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, "UPDATE releases SET seen_at = NULL WHERE id = $1", id); err != nil {
		r.t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if answered {
		headers["Canyon-App"] = "answered"
	}
	if code, _, _ := doWith(r.t, r.c, http.MethodGet, r.base+"/access/claims-portal/"+env, "", headers); code != 200 {
		r.t.Fatalf("the pull: %d %s", code, lastBody)
	}
}

// settled waits for the release's settle by check-in to finish, which runs after the pull's answer, and answers
// its state.
func (r *repoRig) settled(id string) string {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var state string
		if err := r.pool.QueryRow(r.ctx, "SELECT state FROM releases WHERE id = $1", id).Scan(&state); err != nil {
			r.t.Fatal(err)
		}
		if state != "accepted" || time.Now().After(deadline) {
			return state
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// quiet waits until no settle of id is running, and answers its state: what a check-in that settles nothing
// leaves.
func (r *repoRig) quiet(id string) string {
	r.t.Helper()
	time.Sleep(time.Second)
	a := appOf(r.t, r.base)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		a.settling.mu.Lock()
		busy := a.settling.busy[id]
		a.settling.mu.Unlock()
		if !busy || time.Now().After(deadline) {
			break
		}
	}
	var state string
	if err := r.pool.QueryRow(r.ctx, "SELECT state FROM releases WHERE id = $1", id).Scan(&state); err != nil {
		r.t.Fatal(err)
	}
	return state
}

// A repo release goes live at its first check-in whose app answers, the branch holding exactly what Canyon
// committed: once, through the report's decision path, its own audit row, the address and endpoints Canyon
// rendered. A pull whose app does not answer settles nothing, nor does one more after it is live.
func TestARepoReleaseGoesLiveAtItsFirstAnsweredCheckIn(t *testing.T) {
	r := newRepoRig(t, false)
	landed, token := r.commit("rel_aaaa000000000001", "preview")
	r.pull("rel_aaaa000000000001", "preview", token, false)
	if state := r.quiet("rel_aaaa000000000001"); state != "accepted" {
		t.Fatalf("a pull whose app did not answer settled the release: %s", state)
	}
	r.pull("rel_aaaa000000000001", "preview", token, true)
	if state := r.settled("rel_aaaa000000000001"); state != "deployed" {
		t.Fatalf("the answered check-in left the release %s", state)
	}
	var url string
	if err := r.pool.QueryRow(r.ctx, "SELECT url FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview' AND run_by = 'org'").Scan(&url); err != nil || url != landed.Address {
		t.Fatalf("preview is live at %q (%v), Canyon rendered %s", url, err, landed.Address)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM platform_resources WHERE project_id = 'claims-portal' AND env = 'preview' AND name = 'db' AND endpoint = '"+landed.Endpoints[0].Endpoint+"'"); n != 1 {
		t.Fatal("the endpoint Canyon rendered is not the environment's")
	}
	r.pull("rel_aaaa000000000001", "preview", token, true)
	r.quiet("rel_aaaa000000000001")
	var detail map[string]any
	if err := r.pool.QueryRow(r.ctx, "SELECT detail FROM audit_events WHERE action = 'release live by check-in'").Scan(&detail); err != nil {
		t.Fatalf("one release live by check-in row: %v", err)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM audit_events WHERE action IN ('release live by check-in', 'platform reported deployed')"); n != 1 || detail["proof"] != "check-in" || detail["commit"] != landed.Commit {
		t.Fatalf("%d live rows, the one's detail %v", n, detail)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM audit_events WHERE action = 'release live by check-in' AND actor_kind = 'canyon' AND actor_subject = 'check-in' AND via = 'canyon'"); n != 1 {
		t.Fatal("the live row is not Canyon's, by check-in")
	}
}

// The branch must hold what Canyon rendered: a commit of someone else's in the release's directory refuses it at
// the check-in, one build.log line and one audit row, and nothing goes live (#2471).
func TestATreeMismatchRefusesTheRelease(t *testing.T) {
	r := newRepoRig(t, false)
	_, token := r.commit("rel_bbbb000000000001", "preview")
	r.remote.Commit("preview", "someone's change", map[string]string{"apps/claims-portal/preview/extra.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n"})
	r.pull("rel_bbbb000000000001", "preview", token, true)
	if state := r.settled("rel_bbbb000000000001"); state != "refused" {
		t.Fatalf("a tree that is not Canyon's left the release %s", state)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM deploys WHERE project_id = 'claims-portal'"); n != 0 {
		t.Fatal("something went live")
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM audit_events WHERE action = 'release tree mismatch' AND detail->>'branch' = 'preview'"); n != 1 {
		t.Fatal("no release tree mismatch row")
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM timeline_rows WHERE run_id = 'gr_cp' AND kind = 'build.log' AND data->>'line' = 'apps/claims-portal/preview on preview isn''t what Canyon committed for v1.4: nothing went live'"); n != 1 {
		t.Fatal("the run was not told, in D5's words")
	}
}

// Production through a pull request: open, a check-in is not yet; merged with a head Canyon did not commit, it is
// refused; merged as Canyon opened it, it goes live. GitLab's merge requests the same (#2480).
func TestAProductionPullRequestGoesLiveOnlyMergedAsCanyonOpenedIt(t *testing.T) {
	for _, api := range []string{"github", "gitlab"} {
		t.Run(api, func(t *testing.T) { testAProductionPullRequestGoesLiveOnlyMergedAsCanyonOpenedIt(t, api) })
	}
}

func testAProductionPullRequestGoesLiveOnlyMergedAsCanyonOpenedIt(t *testing.T, api string) {
	r := newRepoRigOn(t, true, api)
	landed, token := r.commit("rel_cccc000000000001", "prod")
	if landed.PullRequest == nil {
		t.Fatal("production opened no pull request")
	}
	r.pull("rel_cccc000000000001", "prod", token, true)
	if state := r.quiet("rel_cccc000000000001"); state != "accepted" {
		t.Fatalf("a check-in while the pull request is open left the release %s: not yet live, never refused", state)
	}
	r.remote.Merge(landed.PullRequest.Number, map[string]string{"apps/claims-portal/prod/route.yaml": "edited\n"})
	r.pull("rel_cccc000000000001", "prod", token, true)
	if state := r.settled("rel_cccc000000000001"); state != "refused" {
		t.Fatalf("a pull request edited after Canyon opened it left the release %s", state)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM audit_events WHERE action = 'pull request changed'"); n != 1 {
		t.Fatal("no pull request changed row")
	}

	landed, token = r.commit("rel_cccc000000000002", "prod")
	r.remote.Merge(landed.PullRequest.Number, nil)
	r.pull("rel_cccc000000000002", "prod", token, true)
	if state := r.settled("rel_cccc000000000002"); state != "deployed" {
		t.Fatalf("a pull request merged as Canyon opened it left the release %s", state)
	}
}

// An older release's pull request merged after a newer one opened puts nothing live (#2471): its
// pods check in, and it is no longer the environment's newest.
func TestAStalePullRequestMergedPutsNothingLive(t *testing.T) {
	for _, api := range []string{"github", "gitlab"} {
		t.Run(api, func(t *testing.T) { testAStalePullRequestMergedPutsNothingLive(t, api) })
	}
}

func testAStalePullRequestMergedPutsNothingLive(t *testing.T, api string) {
	r := newRepoRigOn(t, true, api)
	older, token := r.commit("rel_dddd000000000001", "prod")
	r.commit("rel_dddd000000000002", "prod")
	if _, closed := r.remote.Pulls(); len(closed) != 1 || closed[0] != older.PullRequest.Number {
		t.Fatalf("the newer release closed %v, want the older's %d", closed, older.PullRequest.Number)
	}
	r.remote.Merge(older.PullRequest.Number, nil) // a person merges the closed one anyway
	r.pull("rel_dddd000000000001", "prod", token, true)
	if state := r.settled("rel_dddd000000000001"); state == "deployed" {
		t.Fatal("a stale pull request merged put its release live")
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'"); n != 0 {
		t.Fatal("production went live on the stale release")
	}
}

// A webhook release's check-in only records seen_at: the platform's report puts it live, never a pod.
func TestAWebhookReleasesCheckInNeverDeploys(t *testing.T) {
	r := newRepoRig(t, false)
	_, token := r.commit("rel_eeee000000000001", "preview")
	if _, err := r.pool.Exec(r.ctx, "UPDATE releases SET via = 'webhook' WHERE id = 'rel_eeee000000000001'"); err != nil {
		t.Fatal(err)
	}
	r.pull("rel_eeee000000000001", "preview", token, true)
	if state := r.quiet("rel_eeee000000000001"); state != "accepted" {
		t.Fatalf("a webhook release's check-in left it %s", state)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM releases WHERE id = 'rel_eeee000000000001' AND seen_at IS NOT NULL"); n != 1 {
		t.Fatal("the check-in recorded no seen_at")
	}
}

// A stopped release's pull answers the deny-all and settles nothing: its pods are ones Canyon asked the platform
// to take down, never the deploy's (#2450).
func TestAStoppedPullSettlesNothing(t *testing.T) {
	r := newRepoRig(t, false)
	_, token := r.commit("rel_ffff000000000001", "preview")
	if _, err := r.pool.Exec(r.ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at)
		VALUES ('rel_ffff000000000002', 'claims-portal', 'preview', 'gr_cp', 'pkg_cp', 'remove', $1, gen_random_uuid(), 'accepted', now(), 'repo', clock_timestamp())`, accessImage); err != nil {
		t.Fatal(err)
	}
	r.pull("rel_ffff000000000001", "preview", token, true)
	if state := r.quiet("rel_ffff000000000001"); state != "accepted" {
		t.Fatalf("a stopped pull settled the release: %s", state)
	}
}

// An older release's late check-in is superseded, never a tamper alarm (A4): a newer release of the environment
// came after it, so its check-in reads no branch — where the newer one's tree now is — writes no release tree
// mismatch, and puts nothing live.
func TestAnOlderReleasesLateCheckInIsSupersededNotATamper(t *testing.T) {
	r := newRepoRig(t, false)
	_, token := r.commit("rel_eeee000000000001", "preview")
	r.commit("rel_eeee000000000002", "preview")
	r.pull("rel_eeee000000000001", "preview", token, true)
	if state := r.settled("rel_eeee000000000001"); state == "deployed" || state == "accepted" {
		t.Fatalf("an older release's late check-in left it %s", state)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM audit_events WHERE action = 'release tree mismatch'"); n != 0 {
		t.Fatalf("an older release's late check-in raised %d tamper alarm(s)", n)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM deploys WHERE project_id = 'claims-portal'"); n != 0 {
		t.Fatal("an older release went live")
	}
}

// A hold that lands while a check-in reads the deploy repo still keeps the release from going live: the early
// read of superseded, removed and held only spares git, and the decision path decides again inside its
// transaction (the scan's TOCTOU flag). Production through a pull request, merged as Canyon opened it; the
// hold lands as Canyon reads the pull request, after the early read.
func TestAHoldDuringTheGitReadStillKeepsTheReleaseOffLive(t *testing.T) {
	r := newRepoRig(t, true)
	landed, token := r.commit("rel_cccc000000000009", "prod")
	r.remote.Merge(landed.PullRequest.Number, nil)
	r.remote.OnPull(func() {
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display)
			VALUES ('claims-portal', 'INC-7: held mid check-in', 'person', 'rhea', 'Rhea') ON CONFLICT (project_id) DO NOTHING`); err != nil {
			t.Error(err)
		}
	})
	r.pull("rel_cccc000000000009", "prod", token, true)
	if state := r.settled("rel_cccc000000000009"); state == "deployed" || state == "accepted" {
		t.Fatalf("a hold landed during the git read, and the release ended %s", state)
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'"); n != 0 {
		t.Fatal("production went live under a hold")
	}
	if n := count(t, r.ctx, r.pool, "SELECT count(*) FROM releases WHERE project_id = 'claims-portal' AND env = 'prod' AND event = 'remove'"); n != 1 {
		t.Fatalf("%d remove(s) of what the platform started under the hold, want 1", n)
	}
	var remove string
	if err := r.pool.QueryRow(r.ctx, "SELECT id FROM releases WHERE project_id = 'claims-portal' AND env = 'prod' AND event = 'remove'").Scan(&remove); err != nil {
		t.Fatalf("no remove of what the platform started under the hold: %v", err)
	}
	_ = dbos.CancelWorkflow(durable, remove) // it waits 30 minutes on its own pull request's merge: never this test's to end
}
