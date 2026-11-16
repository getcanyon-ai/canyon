package gitops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/gitopstest"
)

const (
	user  = gitopstest.User
	token = gitopstest.Token
)

// remote is the shared test deploy repo (internal/gitopstest), its templates the contract's, and the pull
// requests API a repo of it speaks: github's pulls, or gitlab's merge requests.
type remote struct {
	*gitopstest.Remote
	dir string
	api string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	rem := gitopstest.New(t, filepath.Join(contract, "templates", ".canyon"))
	return &remote{Remote: rem, dir: rem.Dir, api: "github"}
}

// eachAPI runs test under each pull requests API Canyon speaks, a remote of its own each.
func eachAPI(t *testing.T, test func(t *testing.T, r *remote)) {
	for _, api := range []string{"github", "gitlab"} {
		t.Run(api, func(t *testing.T) {
			r := newRemote(t)
			r.api = api
			test(t, r)
		})
	}
}

// pulls is the remote's pull requests API under token.
func (r *remote) pulls(t *testing.T, token string) PullRequests {
	t.Helper()
	if r.api == "gitlab" {
		mrs, err := NewMergeRequests(r.GitLab, r.URL, token, r.Client)
		if err != nil {
			t.Fatal(err)
		}
		return mrs
	}
	pulls, err := NewPulls(r.API, r.URL, token, r.Client)
	if err != nil {
		t.Fatal(err)
	}
	return pulls
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitopstest.Git(t, dir, args...)
}

func (r *remote) repo(t *testing.T, prod bool) *Repo {
	t.Helper()
	repo := &Repo{URL: r.URL, User: user, Token: token, Preview: "preview", Prod: "main", ProdPR: prod, Root: "apps",
		Dir: filepath.Join(t.TempDir(), "deploy.git"), Canyon: "https://canyon.meridian.example", Store: "canyon", HTTP: r.Client}
	if prod {
		repo.Pulls = r.pulls(t, token)
	}
	return repo
}

func fresh(context.Context) error { return nil }

func delivery(t *testing.T) Delivery {
	t.Helper()
	rel, _ := meridian(t)
	return Delivery{Release: rel, Review: "https://canyon.meridian.example/#/apps/claims-portal/runs/gr_8f2a", Date: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC), Fresh: fresh}
}

// A preview release is one commit on preview's branch, its files exactly the contract's, its message the
// version and the trailers; made again, it is the same commit and nothing is pushed.
func TestAPreviewReleaseIsOneCommit(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, false), delivery(t)
	got, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if head := git(t, r.dir, "rev-parse", "preview"); got.Commit != head || got.Branch != "preview" {
		t.Fatalf("landed %+v, the branch is at %s", got, head)
	}
	if tree := git(t, r.dir, "rev-parse", "preview:apps/claims-portal/preview"); got.Tree != tree {
		t.Fatalf("the tree recorded is %s, the branch's %s", got.Tree, tree)
	}
	msg := git(t, r.dir, "log", "-1", "--format=%B", "preview")
	for _, want := range []string{"v1.4 · claims-portal · preview\n", "Canyon-Release: rel_8f2a", "Canyon-Review: gr_8f2a", "Canyon-Version: pkg_9e40", "Canyon-Templates: " + git(t, r.dir, "rev-parse", "main:apps/.canyon")} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
	want, err := os.ReadFile(filepath.Join(contract, "apps", "claims-portal", "preview", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, r.dir, "show", "preview:apps/claims-portal/preview/deployment.yaml"); got != strings.TrimSpace(string(want)) {
		t.Errorf("the committed Deployment is not the contract's:\n%s", got)
	}
	again, err := repo.Deliver(t.Context(), d)
	if err != nil || again.Commit != got.Commit || git(t, r.dir, "rev-list", "--count", "preview") != "2" {
		t.Fatalf("made again: %+v %v, and the branch has %s commits", again, err, git(t, r.dir, "rev-list", "--count", "preview"))
	}
}

// A push the branch moved under is made again on the new head, keeping what someone else committed: never a
// force, never a merge.
func TestAMovedBranchIsCommittedOnTop(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, false), delivery(t)
	moved := false
	d.Fresh = func(context.Context) error {
		if !moved { // someone else's commit lands between Canyon's fetch and its push
			moved = true
			work := t.TempDir()
			git(t, work, "clone", "-q", "-b", "preview", r.dir, ".")
			if err := os.WriteFile(filepath.Join(work, "NOTES.md"), []byte("theirs\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, work, "add", ".")
			git(t, work, "commit", "-q", "-m", "theirs")
			git(t, work, "push", "-q", "origin", "preview")
		}
		return nil
	}
	got, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if parent := git(t, r.dir, "rev-parse", got.Commit+"^"); git(t, r.dir, "log", "-1", "--format=%s", parent) != "theirs" {
		t.Fatalf("Canyon's commit is not on top of the one that moved the branch")
	}
	if git(t, r.dir, "show", "preview:NOTES.md") != "theirs" {
		t.Fatal("their commit is gone")
	}
}

// A push the remote refuses — a rule, a hook — is protected_branch, refused, never tried another way.
func TestARefusedPushIsProtected(t *testing.T) {
	r := newRemote(t)
	hook := filepath.Join(r.dir, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'protected branch hook declined' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // the hook must run
		t.Fatal(err)
	}
	_, err := r.repo(t, false).Deliver(t.Context(), delivery(t))
	if p, ok := IsProblem(err); !ok || p.Code != Protected || strings.Contains(p.Message, "declined") {
		t.Fatalf("got %v, want protected_branch in D5's words", err)
	}
}

// A refused token, a certificate Canyon does not trust and no connection are each their problem, in D5's
// words, and none carries the token.
func TestTransportProblems(t *testing.T) {
	r := newRemote(t)
	wrong := r.repo(t, false)
	wrong.Token = "another-token"
	untrusted := r.repo(t, false)
	untrusted.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}} // trusting nothing: the system pool, loaded once per process, may hold an earlier remote's CA
	gone := r.repo(t, false)
	gone.URL = "https://127.0.0.1:1/meridian/deploy.git"
	for name, c := range map[string]struct {
		repo *Repo
		code string
	}{"a refused token": {wrong, AuthRefused}, "an untrusted certificate": {untrusted, TLSUntrusted}, "no connection": {gone, Unreachable}} {
		t.Run(name, func(t *testing.T) {
			_, err := c.repo.Deliver(t.Context(), delivery(t))
			p, ok := IsProblem(err)
			if !ok || p.Code != c.code {
				t.Fatalf("got %v, want %s", err, c.code)
			}
			if strings.Contains(p.Message, token) || strings.Contains(p.Message, base64.StdEncoding.EncodeToString([]byte(user+":"+token))) {
				t.Fatalf("the problem carries the token: %q", p.Message)
			}
		})
	}
}

// Every changed path is under the release's directory, or nothing is pushed: a file where the root should be
// a directory is someone else's, never Canyon's to replace.
func TestNothingOutsideTheDirectoryIsPushed(t *testing.T) {
	r := newRemote(t)
	work := t.TempDir()
	git(t, work, "clone", "-q", "-b", "preview", r.dir, ".")
	if err := os.WriteFile(filepath.Join(work, "apps", "claims-portal"), []byte("a file, not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "a file named claims-portal")
	git(t, work, "push", "-q", "origin", "preview")
	repo := r.repo(t, false) // the directory would replace the file apps/claims-portal: a path outside apps/claims-portal/preview
	before := git(t, r.dir, "rev-parse", "preview")
	if _, err := repo.Deliver(t.Context(), delivery(t)); err == nil || !strings.Contains(err.Error(), "nothing is pushed") {
		t.Fatalf("got %v, want the commit refused before its push", err)
	}
	if git(t, r.dir, "rev-parse", "preview") != before {
		t.Fatal("the branch moved")
	}
}

// A remove keeps canyon.json and resources/, so the data stays; a project delete's takes the directory away.
func TestRemoveKeepsTheDataAndDeleteDoesNot(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, false), delivery(t)
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	d.Remove = true
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if got := git(t, r.dir, "ls-tree", "-r", "--name-only", "preview", "apps/claims-portal/preview"); got != "apps/claims-portal/preview/canyon.json\napps/claims-portal/preview/resources/db.yaml" {
		t.Fatalf("after a remove the directory holds:\n%s", got)
	}
	if msg := git(t, r.dir, "log", "-1", "--format=%B", "preview"); !strings.HasPrefix(msg, "remove · claims-portal · preview") {
		t.Errorf("the remove's message: %q", msg)
	}
	d.Delete = true
	got, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if left := git(t, r.dir, "ls-tree", "-r", "--name-only", "preview", "apps/claims-portal"); left != "" || got.Tree != "" {
		t.Fatalf("after a delete the repo still holds %q", left)
	}
	if git(t, r.dir, "show", "preview:apps/.canyon/route.yaml") == "" {
		t.Fatal("the delete took the templates")
	}
}

// A directory another Canyon wrote is never written over.
func TestAnotherCanyonsDirectoryIsNeverWritten(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, false), delivery(t)
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	other := r.repo(t, false)
	d.Release.Canyon = "https://canyon.other.example"
	_, err := other.Deliver(t.Context(), d)
	if p, ok := IsProblem(err); !ok || p.Code != DirectoryTaken || !strings.Contains(p.Message, "https://canyon.meridian.example") {
		t.Fatalf("got %v, want directory_taken naming the other Canyon", err)
	}
}

// Production is a pull request from a branch of the release's own: opening one closes the environment's older
// Canyon pull requests, and one opened again is found by its branch.
func TestProductionIsAPullRequest(t *testing.T) { eachAPI(t, testProductionIsAPullRequest) }

func testProductionIsAPullRequest(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	first, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if first.PullRequest == nil || first.PullRequest.Head.Ref != "canyon/claims-portal/prod/8f2a" || first.PullRequest.Head.SHA != first.Commit {
		t.Fatalf("landed %+v", first)
	}
	if git(t, r.dir, "rev-parse", "main") == first.Commit {
		t.Fatal("production's branch moved: a pull request's commit is on its own branch")
	}
	again, err := repo.Deliver(t.Context(), d)
	if err != nil || again.PullRequest.Number != first.PullRequest.Number || again.Commit != first.Commit {
		t.Fatalf("delivered again: %+v %v, want the open pull request %d", again, err, first.PullRequest.Number)
	}
	d.Release.ID, d.Release.Version = "rel_9b31", "v1.5"
	newer, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, closed := r.Pulls(); closed == nil || closed[0] != first.PullRequest.Number || newer.PullRequest.Number == first.PullRequest.Number {
		t.Fatalf("a newer release opened %d and closed %v: the older one must close", newer.PullRequest.Number, closed)
	}
}

// A pull request merged with Canyon's commit is merged; one whose head moved after Canyon opened it is
// changed; one closed unmerged is closed: a production release goes live only on the first. GitLab's locked, short
// lived, is open.
func TestMergedAs(t *testing.T) { eachAPI(t, testMergedAs) }

func testMergedAs(t *testing.T, r *remote) {
	repo := r.repo(t, true)
	prs := []gitopstest.PullRequest{{Number: 1, State: "closed", Merged: true}, {Number: 2, State: "closed", Merged: true}, {Number: 3, State: "closed"}, {Number: 4, State: "open"}, {Number: 5, State: "locked"}}
	prs[0].Head.SHA, prs[1].Head.SHA = "aaa", "bbb"
	for i := range prs {
		prs[i].URL = strings.TrimSuffix(r.URL, ".git") + "/pull/" + strconv.Itoa(i+1)
		prs[i].Head.Repo.FullName, prs[i].Base.Ref = gitopstest.Project, "main"
	}
	r.SetPulls(prs)
	for n, want := range map[int]string{1: "merged", 2: "changed", 3: "closed", 4: "open", 5: "open"} {
		if got, err := repo.MergedAs(t.Context(), n, "aaa"); err != nil || got != want {
			t.Errorf("pull request %d: %q %v, want %s", n, got, err, want)
		}
	}
	repo.Pulls = r.pulls(t, "another")
	if _, err := repo.MergedAs(t.Context(), 1, "aaa"); err == nil {
		t.Fatal("an API that refused the token answered a state: an error is never live")
	}
}

// The check of the repo passes on a repo that has both branches and the templates, and lists them; it pushes
// and deletes a throwaway branch, leaving none.
func TestCheckTheRepo(t *testing.T) { eachAPI(t, testCheckTheRepo) }

func testCheckTheRepo(t *testing.T, r *remote) {
	rel, _ := meridian(t)
	problems, found, err := r.repo(t, true).Check(t.Context(), rel)
	if err != nil || len(problems) != 0 {
		t.Fatalf("problems %v, %v", problems, err)
	}
	if strings.Join(found, ",") != ".canyon/resources/postgres.yaml,.canyon/route.yaml" {
		t.Errorf("templates found: %v", found)
	}
	if left := git(t, r.dir, "for-each-ref", "refs/heads/canyon/"); left != "" {
		t.Fatalf("the check left %s", left)
	}
	missing := r.repo(t, true)
	missing.Prod = "production"
	problems, _, err = missing.Check(t.Context(), rel)
	if err != nil || len(problems) != 1 || problems[0].Code != BranchMissing || !strings.Contains(problems[0].Message, "production") {
		t.Fatalf("a missing branch: %v %v", problems, err)
	}
	wrong := r.repo(t, true)
	wrong.Token = "another-token"
	if problems, _, err := wrong.Check(t.Context(), rel); err != nil || len(problems) != 1 || problems[0].Code != AuthRefused {
		t.Fatalf("a refused token: %v %v", problems, err)
	}
	noAPI := r.repo(t, true)
	noAPI.Pulls = r.pulls(t, "another-token")
	if problems, _, err := noAPI.Check(t.Context(), rel); err != nil || len(problems) != 1 || problems[0].Code != PullsAPI {
		t.Fatalf("an API that refuses the token: %v %v, want pulls_api_unavailable", problems, err)
	}
}

// A pull request branch found from an earlier attempt is Canyon's only when its one commit changes nothing but the
// release's directory: someone's commit on top of it, outside the directory, is never proposed for merge.
func TestAReusedPullRequestBranchChangesNothingElse(t *testing.T) {
	eachAPI(t, testAReusedPullRequestBranchChangesNothingElse)
}

func testAReusedPullRequestBranchChangesNothingElse(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	r.Commit("canyon/claims-portal/prod/8f2a", "someone's change outside the directory", map[string]string{"README.md": "rewritten\n"})
	if got, err := repo.Deliver(t.Context(), d); err == nil {
		t.Fatalf("a branch carrying someone else's change was proposed again: %+v", got)
	}
}

// A retry that is no longer fresh writes nothing: no push, no pull request opened, none closed.
func TestAStaleRetryNeverTouchesPullRequests(t *testing.T) {
	eachAPI(t, testAStaleRetryNeverTouchesPullRequests)
}

func testAStaleRetryNeverTouchesPullRequests(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	stale := errors.New("a newer release was written")
	d.Fresh = func(context.Context) error { return stale }
	if _, err := repo.Deliver(t.Context(), d); !errors.Is(err, stale) {
		t.Fatalf("a stale retry answered %v, want its Fresh refusal", err)
	}
	if prs, closed := r.Pulls(); len(prs) != 1 || len(closed) != 0 {
		t.Fatalf("a stale retry touched pull requests: %d opened, %v closed", len(prs), closed)
	}
}

// A remove never touches a directory another Canyon wrote.
func TestARemoveNeverTouchesAnotherCanyonsDirectory(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, false), delivery(t)
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	before := git(t, r.dir, "rev-parse", "preview")
	other := r.repo(t, false)
	d.Release.Canyon, d.Remove = "https://canyon.other.example", true
	_, err := other.Deliver(t.Context(), d)
	if p, ok := IsProblem(err); !ok || p.Code != DirectoryTaken {
		t.Fatalf("another Canyon's remove: %v, want directory_taken", err)
	}
	if git(t, r.dir, "rev-parse", "preview") != before {
		t.Fatal("the branch moved")
	}
}

// A remote that refuses the check's delete of its throwaway branch is a problem in D5's words, never git's error,
// which carries the remote's own: nothing of what it said reaches an answer or a log.
func TestACheckNeverCarriesTheRemotesWords(t *testing.T) {
	r := newRemote(t)
	hook := filepath.Join(r.dir, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $new in 0000000000000000000000000000000000000000) echo 'secret-internal-host.example said no' >&2; exit 1;; esac; done\n"), 0o700); err != nil { //nolint:gosec // the hook must run
		t.Fatal(err)
	}
	rel, _ := meridian(t)
	problems, _, err := r.repo(t, true).Check(t.Context(), rel)
	if err != nil {
		t.Fatalf("the check answered an error, not a problem: %v", err)
	}
	if len(problems) != 1 || problems[0].Code != Protected || strings.Contains(problems[0].Message, "secret-internal-host") {
		t.Fatalf("problems %v", problems)
	}
}
