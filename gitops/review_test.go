package gitops

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/internal/gitopstest"
)

// A pull request branch already there is reused only when it sits on production's history: someone who can write
// the repo pushes their own commit X (outside the directory) and Canyon's tree Y on top of it, and a merge of
// Canyon's pull request would take X along. Canyon refuses it, protected_branch, and opens nothing.
func TestAReusedBranchMustSitOnProduction(t *testing.T) {
	eachAPI(t, testAReusedBranchMustSitOnProduction)
}

func testAReusedBranchMustSitOnProduction(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	branch := "canyon/claims-portal/prod/8f2a"
	work := t.TempDir()
	git(t, work, "clone", "-q", "-b", "main", r.dir, ".")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("someone's change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "X: outside the directory")
	git(t, work, "fetch", "-q", "origin", branch)
	git(t, work, "checkout", "-q", "FETCH_HEAD", "--", "apps/claims-portal/prod")
	git(t, work, "commit", "-q", "-m", "Y: Canyon's tree")
	git(t, work, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)
	prs, _ := r.Pulls()
	_, err := repo.Deliver(t.Context(), d)
	if p, ok := IsProblem(err); !ok || p.Code != Protected {
		t.Fatalf("a branch on someone else's commit: %v, want protected_branch", err)
	}
	if after, _ := r.Pulls(); len(after) != len(prs) {
		t.Fatalf("it opened %d pull request(s) on it", len(after)-len(prs))
	}
}

// A directory already in the repo with no canyon.json is someone else's: a deploy never writes over it, and a
// remove never takes it away.
func TestADirectoryCanyonDidntWriteIsNeverWrittenOver(t *testing.T) {
	r := newRemote(t)
	r.Commit("preview", "the platform team's own files", map[string]string{"apps/claims-portal/preview/theirs.yaml": "kind: ConfigMap\n"})
	before := git(t, r.dir, "rev-parse", "preview")
	for name, remove := range map[string]bool{"a deploy": false, "a remove": true} {
		d := delivery(t)
		d.Remove = remove
		_, err := r.repo(t, false).Deliver(t.Context(), d)
		if p, ok := IsProblem(err); !ok || p.Code != DirectoryTaken || !strings.Contains(p.Message, "Canyon didn't write it") {
			t.Fatalf("%s over a directory with no canyon.json: %v, want directory_taken", name, err)
		}
	}
	if git(t, r.dir, "rev-parse", "preview") != before {
		t.Fatal("the branch moved")
	}
}

// The open pull requests are read a hundred a page, every page, and more than ten pages is an error, never a list
// cut short: an older Canyon pull request past the first hundred is still closed.
func TestOpenReadsEveryPageAndNoMoreThanTen(t *testing.T) {
	eachAPI(t, testOpenReadsEveryPageAndNoMoreThanTen)
}

func testOpenReadsEveryPageAndNoMoreThanTen(t *testing.T, r *remote) {
	repo := r.repo(t, true)
	many := func(n int) []gitopstest.PullRequest {
		prs := make([]gitopstest.PullRequest, n)
		for i := range prs {
			prs[i].Number, prs[i].State = i+1, "open"
			prs[i].URL = strings.TrimSuffix(r.URL, ".git") + "/pull/" + strconv.Itoa(i+1)
		}
		return prs
	}
	r.SetPulls(many(150))
	if open, err := repo.Pulls.Open(t.Context()); err != nil || len(open) != 150 {
		t.Fatalf("150 open: %d, %v", len(open), err)
	}
	r.MaxPage = 30 // Gitea's and Forgejo's cap: a page shorter than asked is no end
	if open, err := repo.Pulls.Open(t.Context()); err != nil || len(open) != 150 {
		t.Fatalf("150 open, 30 a page: %d, %v", len(open), err)
	}
	r.MaxPage = 0
	r.SetPulls(many(openPages*100 + 1))
	if open, err := repo.Pulls.Open(t.Context()); err == nil {
		t.Fatalf("%d open answered %d, not an error", openPages*100+1, len(open))
	}
}

// A pull request's address is kept only when it is https on the repo's or the API's host: an API that names
// another host, or plain http, is no answer.
func TestAPullRequestsAddressIsHTTPSOnAKnownHost(t *testing.T) {
	eachAPI(t, testAPullRequestsAddressIsHTTPSOnAKnownHost)
}

func testAPullRequestsAddressIsHTTPSOnAKnownHost(t *testing.T, r *remote) {
	repo := r.repo(t, true)
	for name, u := range map[string]string{
		"another host": "https://evil.example/meridian/deploy/pull/1",
		"plain http":   "http" + strings.TrimPrefix(strings.TrimSuffix(r.URL, ".git"), "https") + "/pull/1",
		"no address":   "",
	} {
		pr := gitopstest.PullRequest{Number: 1, State: "open", URL: u}
		r.SetPulls([]gitopstest.PullRequest{pr})
		if got, err := repo.Pulls.Get(t.Context(), 1); err == nil {
			t.Errorf("%s: kept %q", name, got.URL)
		}
		if _, err := repo.Pulls.Open(t.Context()); err == nil {
			t.Errorf("%s: listed", name)
		}
	}
	r.SetPulls([]gitopstest.PullRequest{{Number: 1, State: "open", URL: strings.TrimSuffix(r.URL, ".git") + "/pull/1"}})
	if _, err := repo.Pulls.Get(t.Context(), 1); err != nil {
		t.Fatalf("the repo's own host: %v", err)
	}
}

// A certificate Canyon does not trust is refused at once: no retry fixes it.
func TestAnUntrustedCertificateIsNeverRetried(t *testing.T) {
	for code, retries := range map[string]bool{Unreachable: true, PullsAPI: true, TLSUntrusted: false, AuthRefused: false, Protected: false} {
		if got := (&Problem{Code: code}).Retries(); got != retries {
			t.Errorf("%s retries %v, want %v", code, got, retries)
		}
	}
}

// A pull request a fork opened under Canyon's branch name is never Canyon's: when the API answers a create 422,
// the one Canyon finds again is this repo's branch into production's, else the attempt fails and is tried again.
func TestAForksPullRequestIsNeverCanyons(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	fork := gitopstest.PullRequest{Number: 1, State: "open", URL: strings.TrimSuffix(r.URL, ".git") + "/pull/1"}
	fork.Head.Ref, fork.Head.Repo.FullName, fork.Base.Ref = "canyon/claims-portal/prod/8f2a", "someone/deploy", "main"
	r.SetPulls([]gitopstest.PullRequest{fork})
	got, err := repo.Deliver(t.Context(), d)
	if p, ok := IsProblem(err); !ok || p.Code != PullsAPI {
		t.Fatalf("a fork's pull request under Canyon's branch: %+v %v, want pulls_api_unavailable", got.PullRequest, err)
	}
}

// Opening a release's pull request closes the environment's older Canyon pull requests of this repo's own branches
// alone: a fork's, under a branch named as Canyon's, is never Canyon's, nor its to close (#2480).
func TestAForksPullRequestIsNeverClosed(t *testing.T) { eachAPI(t, testAForksPullRequestIsNeverClosed) }

func testAForksPullRequestIsNeverClosed(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	fork := gitopstest.PullRequest{Number: 1, State: "open", URL: strings.TrimSuffix(r.URL, ".git") + "/pull/1"}
	fork.Head.Ref, fork.Head.Repo.FullName, fork.Base.Ref = "canyon/claims-portal/prod/0000", "someone/deploy", "main"
	r.SetPulls([]gitopstest.PullRequest{fork})
	landed, err := repo.Deliver(t.Context(), d)
	if err != nil || landed.PullRequest == nil || landed.PullRequest.Number == fork.Number {
		t.Fatalf("landed %+v %v, want a pull request of Canyon's own", landed.PullRequest, err)
	}
	if _, closed := r.Pulls(); len(closed) != 0 {
		t.Fatalf("Canyon closed %v: a fork's pull request is never Canyon's to close", closed)
	}
}

// A pull request merged into another branch than production's, or from a fork's, is changed: never merged, so a
// remove retargeted at an unprotected branch and merged there settles nothing (#2486's review, M1).
func TestAPullRequestMergedElsewhereIsChanged(t *testing.T) {
	eachAPI(t, testAPullRequestMergedElsewhereIsChanged)
}

func testAPullRequestMergedElsewhereIsChanged(t *testing.T, r *remote) {
	repo := r.repo(t, true)
	prs := []gitopstest.PullRequest{{Number: 1, State: "closed", Merged: true}, {Number: 2, State: "closed", Merged: true}, {Number: 3, State: "closed", Merged: true}}
	for i := range prs {
		prs[i].URL = strings.TrimSuffix(r.URL, ".git") + "/pull/" + strconv.Itoa(i+1)
		prs[i].Head.SHA, prs[i].Head.Repo.FullName, prs[i].Base.Ref = "aaa", gitopstest.Project, "main"
	}
	prs[1].Base.Ref = "preview"
	prs[2].Head.Repo.FullName = "someone/deploy"
	r.SetPulls(prs)
	for n, want := range map[int]string{1: "merged", 2: "changed", 3: "changed"} {
		if got, err := repo.MergedAs(t.Context(), n, "aaa"); err != nil || got != want {
			t.Errorf("pull request %d: %q %v, want %s", n, got, err, want)
		}
	}
}

// Close-stale closes only Canyon's older pull requests into production's branch: one of a canyon/ branch into
// another branch is not the environment's, and stays open (#2486's review, m2).
func TestAPullRequestIntoAnotherBranchIsNeverClosed(t *testing.T) {
	eachAPI(t, testAPullRequestIntoAnotherBranchIsNeverClosed)
}

func testAPullRequestIntoAnotherBranchIsNeverClosed(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	other := gitopstest.PullRequest{Number: 1, State: "open", URL: strings.TrimSuffix(r.URL, ".git") + "/pull/1"}
	other.Head.Ref, other.Head.Repo.FullName, other.Base.Ref = "canyon/claims-portal/prod/0000", gitopstest.Project, "preview"
	r.SetPulls([]gitopstest.PullRequest{other})
	if _, err := repo.Deliver(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, closed := r.Pulls(); len(closed) != 0 {
		t.Fatalf("Canyon closed %v: a pull request into another branch is not production's", closed)
	}
}

// Canyon looks for the release's open pull request before it creates one, on either API: an attempt that finds it
// open never asks for another, so an API that errs on a second create never fails a retry.
func TestAnOpenPullRequestIsFoundBeforeACreate(t *testing.T) {
	eachAPI(t, testAnOpenPullRequestIsFoundBeforeACreate)
}

func testAnOpenPullRequestIsFoundBeforeACreate(t *testing.T, r *remote) {
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	first, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	r.RefuseCreate = true
	again, err := repo.Deliver(t.Context(), d)
	if err != nil || again.PullRequest == nil || again.PullRequest.Number != first.PullRequest.Number {
		t.Fatalf("delivered again: %+v %v, want the open pull request %d found, no create", again.PullRequest, err, first.PullRequest.Number)
	}
}

// Production through a pull request with no pull requests API is refused before any git write: never a nil call,
// never a push (#2486's review).
func TestProductionWithNoPullsAPIIsRefused(t *testing.T) {
	r := newRemote(t)
	repo, d := r.repo(t, true), delivery(t)
	repo.Pulls = nil
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	if _, err := repo.Deliver(t.Context(), d); err == nil {
		t.Fatal("production through a pull request with no API delivered")
	}
	if _, err := repo.MergedAs(t.Context(), 1, "aaa"); err == nil {
		t.Fatal("a pull request read with no API answered a state")
	}
	if left := git(t, r.dir, "for-each-ref", "refs/heads/canyon/"); left != "" {
		t.Fatalf("it pushed %s", left)
	}
}
