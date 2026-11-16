package runs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/gitops"
	"github.com/getcanyon-ai/canyon/internal/gitopstest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The release side of delivery: repo (design/HAND-OFF.md §3b; contracts/routes.md, The deploy repo): one attempt's
// Fresh under the repo's lock, its bound, the remove workflow's superseded path and its bounded wait on a merge,
// RecheckRemove and removedByCommit — each against a real git remote.

const repoRun = "gr_repo0000000"

// repoHost is a host under delivery: repo, its deploy repo a real git remote, production pushed or through a pull
// request, the merge poll shortened to milliseconds.
func repoHost(t *testing.T, prodPR bool) (*pgxpool.Pool, context.Context, *gitopstest.Remote) {
	t.Helper()
	return repoHostOn(t, prodPR, "github")
}

// repoHostOn is repoHost with its pull requests through api: github's pulls or gitlab's merge requests.
func repoHostOn(t *testing.T, prodPR bool, api string) (*pgxpool.Pool, context.Context, *gitopstest.Remote) {
	t.Helper()
	fastReleaseTiming(t)
	p, ctx := live(t, runstest.CleanGolden)
	keys, err := vault.Load(t.TempDir() + "/kek")
	if err != nil {
		t.Fatal(err)
	}
	remote := gitopstest.New(t, filepath.Join("..", "contracts", "testdata", "release", "repo", "templates", ".canyon"))
	savedWait, savedEvery := mergeWait, mergeEvery
	pipe.PublicURL, pipe.Vault, pipe.RepoTransport = "http://localhost:7777", keys, remote.Client.Transport
	mergeWait, mergeEvery = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pipe.RepoTransport, mergeWait, mergeEvery = nil, savedWait, savedEvery })
	mode := "push"
	if prodPR {
		mode = "pull_request"
	}
	version, sealedKey, sealedValue, err := keys.Seal("platform-repo", "repo", "token", []byte(gitopstest.Token))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE org_preferences SET run_by = 'org', delivery = 'repo'", nil},
		{"INSERT INTO runs (id, project_id, package_id, state) VALUES ($1, $2, $3, 'clean') ON CONFLICT (id) DO NOTHING", []any{repoRun, project, pkg}},
		{`INSERT INTO platform_repo (url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, token_hash, fetch_token_hash, set_by_kind, set_by_subject, set_by_display)
			VALUES ($1, $2, $3, $4, $5, 'preview', 'main', $6, 'apps', $7, $8, 'canyon', sha256('host'::bytea), sha256('fetch'::bytea), 'person', 'dana', 'Dana')`,
			[]any{remote.URL, gitopstest.User, version, sealedKey, sealedValue, mode, api, map[string]string{"github": remote.API, "gitlab": remote.GitLab}[api]}},
	} {
		if _, err := p.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return p, ctx, remote
}

// addRepoRelease writes a release of env through the deploy repo, newer than every one before it.
func addRepoRelease(t *testing.T, ctx context.Context, p *pgxpool.Pool, id, env, event string) {
	t.Helper()
	if _, err := p.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, gen_random_uuid(), 'accepted', now(), 'repo', clock_timestamp())`, id, project, env, repoRun, pkg, event, repoImage); err != nil {
		t.Fatal(err)
	}
}

var repoImage = "registry.example/canyon/claims-portal@sha256:" + strings.Repeat("c", 64)

// deployOf is release id's deploy as the release step makes it.
func deployOf(id, env string) gitops.Delivery {
	return gitops.Delivery{Release: gitops.Release{Canyon: pipe.PublicURL, ID: id, App: project, Env: env, GateRun: repoRun, Package: pkg, Version: "v1", Image: repoImage,
		Identity: "registry.example/canyon-identity@sha256:" + strings.Repeat("d", 64), AppEnv: map[string]string{"PORT": "3000"}, SidecarEnv: map[string]string{"CANYON_TARGET": env}},
		Review: "https://canyon.example/#/apps/claims-portal/runs/" + repoRun, Date: time.Now().UTC().Truncate(time.Second)}
}

// head is branch's commit on the remote.
func head(t *testing.T, remote *gitopstest.Remote, branch string) string {
	t.Helper()
	return gitopstest.Git(t, remote.Dir, "rev-parse", "refs/heads/"+branch)
}

// resultOf waits on remove release id's workflow and answers what it ended on.
func resultOf(t *testing.T, id string) string {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[any](durable, id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan any, 1)
	go func() {
		res, err := h.GetResult()
		if err != nil {
			res = "error: " + err.Error()
		}
		done <- res
	}()
	select {
	case res := <-done:
		s, _ := res.(string)
		return s
	case <-time.After(30 * time.Second):
		t.Fatalf("the remove workflow %s never ended", id)
		return ""
	}
}

func count(t *testing.T, ctx context.Context, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A retry of a release a newer one of its environment followed pushes nothing: Fresh, read under the repo's lock
// right before the push, answers it stale; the newest commits.
func TestAStaleRetryNeverPushesOverANewerRelease(t *testing.T) {
	p, ctx, remote := repoHost(t, false)
	addRepoRelease(t, ctx, p, "rel_aaaa000000000001", "preview", "deploy")
	addRepoRelease(t, ctx, p, "rel_aaaa000000000002", "preview", "deploy")
	before := head(t, remote, "preview")
	att, err := pipe.commitOnce(ctx, "rel_aaaa000000000001", deployOf("rel_aaaa000000000001", "preview"), nil)
	if err != nil || !att.Stale || att.Commit != "" || head(t, remote, "preview") != before {
		t.Fatalf("an older release's retry: %+v %v, the branch moved %v", att, err, head(t, remote, "preview") != before)
	}
	if att, err := pipe.commitOnce(ctx, "rel_aaaa000000000002", deployOf("rel_aaaa000000000002", "preview"), nil); err != nil || att.Stale || att.Commit == "" {
		t.Fatalf("the newest release: %+v %v", att, err)
	}
}

// Nothing reaches production while it is held: a prod deploy's attempt is stale under a hold, and commits once
// the hold is lifted.
func TestNoCommitReachesHeldProduction(t *testing.T) {
	p, ctx, remote := repoHost(t, false)
	addRepoRelease(t, ctx, p, "rel_bbbb000000000001", "prod", "deploy")
	if _, err := p.Exec(ctx, `INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ($1, 'INC-1: held', 'person', 'rhea', 'Rhea')`, project); err != nil {
		t.Fatal(err)
	}
	before := head(t, remote, "main")
	att, err := pipe.commitOnce(ctx, "rel_bbbb000000000001", deployOf("rel_bbbb000000000001", "prod"), nil)
	if err != nil || !att.Stale || head(t, remote, "main") != before {
		t.Fatalf("a prod deploy while held: %+v %v", att, err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM prod_holds WHERE project_id = $1", project); err != nil {
		t.Fatal(err)
	}
	if att, err := pipe.commitOnce(ctx, "rel_bbbb000000000001", deployOf("rel_bbbb000000000001", "prod"), nil); err != nil || att.Stale || att.Commit == "" {
		t.Fatalf("once the hold is lifted: %+v %v", att, err)
	}
}

// An attempt holds the repo's lock within its bound: a remote that answers Canyon's probe and then stalls git is
// unreachable once the bound passes, never a wait on the remote.
func TestAStalledRemoteIsUnreachableWithinTheBound(t *testing.T) {
	p, ctx, remote := repoHost(t, false)
	target, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: target.Scheme, Host: target.Host})
	proxy.Transport = remote.Client.Transport
	stall := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.UserAgent(), "git/") { // git's own requests stall; Canyon's probe goes through
			<-r.Context().Done()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(stall.Close)
	if _, err := p.Exec(ctx, "UPDATE platform_repo SET url = $1", stall.URL+target.Path); err != nil {
		t.Fatal(err)
	}
	saved := commitBound
	commitBound = 2 * time.Second
	t.Cleanup(func() { commitBound = saved })
	addRepoRelease(t, ctx, p, "rel_cccc000000000001", "preview", "deploy")
	started := time.Now()
	att, err := pipe.commitOnce(ctx, "rel_cccc000000000001", deployOf("rel_cccc000000000001", "preview"), nil)
	if err != nil || att.Problem == nil || att.Problem.Code != gitops.Unreachable || time.Since(started) > 20*time.Second {
		t.Fatalf("a stalled remote: %+v %v after %s", att, err, time.Since(started))
	}
}

// A remove a newer release of its environment followed commits nothing: the remove workflow ends superseded, the
// directory stays, and nothing is removed.
func TestARemoveANewerReleaseFollowedRemovesNothing(t *testing.T) {
	p, ctx, remote := repoHost(t, false)
	addRepoRelease(t, ctx, p, "rel_dddd000000000001", "preview", "deploy")
	if att, err := pipe.commitOnce(ctx, "rel_dddd000000000001", deployOf("rel_dddd000000000001", "preview"), nil); err != nil || att.Commit == "" {
		t.Fatalf("the deploy: %+v %v", att, err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ($1, 'preview', $2, $3, 'https://claims-portal.preview.apps.meridian.example', $4, 'org')",
		project, pkg, repoImage, repoRun); err != nil {
		t.Fatal(err)
	}
	before := head(t, remote, "preview")
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remove, sent, err := SendRemoveTx(ctx, tx, durable, project, "preview")
	if err != nil || !sent {
		t.Fatalf("the remove: %v %v", sent, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at)
		VALUES ('rel_dddd000000000003', $1, 'preview', $2, $3, 'deploy', $4, gen_random_uuid(), 'sent', now(), 'repo', clock_timestamp())`, project, repoRun, pkg, repoImage); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resultOf(t, remove); got != "superseded" {
		t.Fatalf("a remove a newer release followed ended %q, want superseded", got)
	}
	if head(t, remote, "preview") != before || count(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = 'release removed by commit'") != 0 {
		t.Fatal("the superseded remove changed the branch or recorded a removal")
	}
}

// A remove through a pull request is asked after for mergeWait, then left accepted: the workflow ends, never
// waits on a person. RecheckRemove asks again: open stays accepted, merged unedited is removed — the environment
// the org ran taken off — and closed unmerged is refused. GitLab's merge requests the same (#2480).
func TestARemovesPullRequestIsAskedForItsBoundThenOnDemand(t *testing.T) {
	for _, api := range []string{"github", "gitlab"} {
		t.Run(api, func(t *testing.T) { testARemovesPullRequestIsAskedForItsBoundThenOnDemand(t, api) })
	}
}

func testARemovesPullRequestIsAskedForItsBoundThenOnDemand(t *testing.T, api string) {
	p, ctx, remote := repoHostOn(t, true, api)
	addRepoRelease(t, ctx, p, "rel_eeee000000000001", "prod", "deploy")
	att, err := pipe.commitOnce(ctx, "rel_eeee000000000001", deployOf("rel_eeee000000000001", "prod"), nil)
	if err != nil || att.PullRequest == nil {
		t.Fatalf("the prod deploy's pull request: %+v %v", att, err)
	}
	remote.Merge(att.PullRequest.Number, nil)
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ($1, 'prod', $2, $3, 'https://claims-portal.prod.apps.meridian.example', $4, 'org')",
		project, pkg, repoImage, repoRun); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remove, _, err := SendRemoveTx(ctx, tx, durable, project, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resultOf(t, remove); got != "accepted" {
		t.Fatalf("a remove waiting on its merge ended %q, want accepted after mergeWait", got)
	}
	if state, err := pipe.RecheckRemove(ctx, remove); err != nil || state != "accepted" {
		t.Fatalf("asked again while open: %q %v", state, err)
	}
	var number int
	if err := p.QueryRow(ctx, "SELECT pull_number FROM releases WHERE id = $1", remove).Scan(&number); err != nil {
		t.Fatal(err)
	}
	remote.Merge(number, nil)
	if state, err := pipe.RecheckRemove(ctx, remove); err != nil || state != "removed" {
		t.Fatalf("asked again once merged: %q %v", state, err)
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM deploys WHERE project_id = $1 AND env = 'prod'", project); n != 0 {
		t.Fatal("the merged remove left production on the project")
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action = 'release removed by commit' AND detail->>'release' = $1", remove); n != 1 {
		t.Fatalf("%d release removed by commit rows, want 1", n)
	}

	// closed unmerged: refused
	prs, _ := remote.Pulls()
	closed := gitopstest.PullRequest{Number: len(prs) + 1, State: "closed", URL: strings.TrimSuffix(remote.URL, ".git") + "/pull/" + strconv.Itoa(len(prs)+1)}
	remote.SetPulls(append(prs, closed))
	if _, err := p.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at, pull_number, commit_sha)
		VALUES ('rel_eeee000000000009', $1, 'prod', $2, $3, 'remove', $4, gen_random_uuid(), 'accepted', now(), 'repo', clock_timestamp(), $5, 'abc')`, project, repoRun, pkg, repoImage, closed.Number); err != nil {
		t.Fatal(err)
	}
	if state, err := pipe.RecheckRemove(ctx, "rel_eeee000000000009"); err != nil || state != "refused" {
		t.Fatalf("asked again once closed unmerged: %q %v", state, err)
	}

	// retargeted at another branch and merged there, as Canyon committed it: refused, never removed (#2486's review)
	prs, _ = remote.Pulls()
	elsewhere := gitopstest.PullRequest{Number: len(prs) + 1, State: "closed", Merged: true, URL: strings.TrimSuffix(remote.URL, ".git") + "/pull/" + strconv.Itoa(len(prs)+1)}
	elsewhere.Head.Ref, elsewhere.Head.SHA, elsewhere.Head.Repo.FullName, elsewhere.Base.Ref = "canyon/claims-portal/prod/eeee", "abd", gitopstest.Project, "preview"
	remote.SetPulls(append(prs, elsewhere))
	if _, err := p.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, via, created_at, pull_number, commit_sha)
		VALUES ('rel_eeee000000000010', $1, 'prod', $2, $3, 'remove', $4, gen_random_uuid(), 'accepted', now(), 'repo', clock_timestamp(), $5, 'abd')`, project, repoRun, pkg, repoImage, elsewhere.Number); err != nil {
		t.Fatal(err)
	}
	if state, err := pipe.RecheckRemove(ctx, "rel_eeee000000000010"); err != nil || state != "refused" {
		t.Fatalf("asked again once merged into another branch: %q %v, want refused", state, err)
	}
}

// removedByCommit takes the environment the org ran off the project, and a delete's what the platform provided
// there with it — unless a newer release of the environment came after the remove: then the remove is removed,
// and what runs there now stays.
func TestRemovedByCommitKeepsWhatANewerReleaseRuns(t *testing.T) {
	p, ctx, _ := repoHost(t, false)
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ($1, 'preview', $2, $3, 'https://x.example', $4, 'org')",
		project, pkg, repoImage, repoRun); err != nil {
		t.Fatal(err)
	}
	addRepoRelease(t, ctx, p, "rel_ffff000000000001", "preview", "remove")
	addRepoRelease(t, ctx, p, "rel_ffff000000000002", "preview", "deploy")
	if err := pipe.removedByCommit(ctx, "rel_ffff000000000001"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM releases WHERE id = 'rel_ffff000000000001' AND state = 'removed'"); n != 1 {
		t.Fatal("the remove is not removed")
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM deploys WHERE project_id = $1 AND env = 'preview'", project); n != 1 {
		t.Fatal("a superseded remove took off what the newer release runs")
	}
	addRepoRelease(t, ctx, p, "rel_ffff000000000003", "preview", "remove")
	if _, err := p.Exec(ctx, "UPDATE releases SET deletes_resources = true WHERE id = 'rel_ffff000000000003'"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ($1, 'preview', 'db', 'postgres://db.example:5432/app', 'rel_ffff000000000002')", project); err != nil {
		t.Fatal(err)
	}
	if err := pipe.removedByCommit(ctx, "rel_ffff000000000003"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, p, "SELECT (SELECT count(*) FROM deploys WHERE project_id = $1 AND env = 'preview') + (SELECT count(*) FROM platform_resources WHERE project_id = $1)", project); n != 0 {
		t.Fatalf("the newest remove, a delete's, left %d row(s) of the environment", n)
	}
}
