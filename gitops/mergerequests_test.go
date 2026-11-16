package gitops

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/internal/gitopstest"
)

// GitLab's API follows no redirect: one that sends Canyon elsewhere never hands the token to the other host, and
// the call is no answer (design/HAND-OFF.md §3b, as the pulls API's).
func TestTheMergeRequestsAPIFollowsNoRedirect(t *testing.T) {
	elsewhere := make(chan string, 8)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere <- r.Header.Get("PRIVATE-TOKEN")
		_, _ = w.Write([]byte(`{"iid": 1, "state": "merged"}`))
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // the test's own other server: the redirect under test
	}))
	defer api.Close()
	mrs := &MergeRequests{API: api.URL + "/api/v4", Path: "meridian/deploy", Token: rand.Text()}
	if _, err := mrs.Get(t.Context(), 1); err == nil {
		t.Fatal("a redirect answered as the merge request")
	}
	select {
	case header := <-elsewhere:
		t.Fatalf("the merge requests client followed the redirect, carrying %q", header)
	default:
	}
}

// The project is named by its whole path, a nested group's too, URL-encoded as one segment; the token rides in
// PRIVATE-TOKEN alone, and a refusal's error is its status, never the token or what the remote said.
func TestTheMergeRequestsAPINamesTheProjectByItsWholePath(t *testing.T) {
	secret := rand.Text()
	seen := make(chan *http.Request, 1)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		http.Error(w, `{"message":"401 Unauthorized `+secret+`"}`, http.StatusUnauthorized)
	}))
	defer api.Close()
	mrs, err := NewMergeRequests(api.URL+"/api/v4", api.URL+"/platform/apps/deploy.git", secret, api.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = mrs.Get(t.Context(), 7)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused token answered %v", err)
	}
	r := <-seen
	if got := r.URL.EscapedPath(); got != "/api/v4/projects/platform%2Fapps%2Fdeploy/merge_requests/7" {
		t.Errorf("the project's path was sent as %s", got)
	}
	if r.Header.Get("PRIVATE-TOKEN") != secret || r.Header.Get("Authorization") != "" {
		t.Errorf("the token went as PRIVATE-TOKEN %q, Authorization %q", r.Header.Get("PRIVATE-TOKEN"), r.Header.Get("Authorization"))
	}
	// A GitLab served under a path of its own: the project is named below that root, never with it.
	rooted, err := NewMergeRequests(api.URL+"/gitlab/api/v4", api.URL+"/gitlab/platform/apps/deploy.git", secret, api.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rooted.Get(t.Context(), 7)
	if got := (<-seen).URL.EscapedPath(); got != "/gitlab/api/v4/projects/platform%2Fapps%2Fdeploy/merge_requests/7" {
		t.Errorf("under a relative root the project's path was sent as %s", got)
	}
}

// GitLab's API is on the repository's own host: one saved on another host is refused before any call, and a
// project needs a group.
func TestTheMergeRequestsAPIIsOnTheReposHost(t *testing.T) {
	for name, c := range map[string]struct{ api, repo string }{
		"another host":  {"https://gitlab.evil.example/api/v4", "https://gitlab.meridian.example/platform/deploy.git"},
		"another port":  {"https://gitlab.meridian.example:8443/api/v4", "https://gitlab.meridian.example/platform/deploy.git"},
		"no group":      {"https://gitlab.meridian.example/api/v4", "https://gitlab.meridian.example/deploy.git"},
		"empty segment": {"https://gitlab.meridian.example/api/v4", "https://gitlab.meridian.example/platform//deploy.git"},
		"plain http":    {"http://gitlab.meridian.example/api/v4", "https://gitlab.meridian.example/platform/deploy.git"},
		"not /api/v4":   {"https://gitlab.meridian.example/api/v3", "https://gitlab.meridian.example/platform/deploy.git"},
		"outside root":  {"https://gitlab.meridian.example/gitlab/api/v4", "https://gitlab.meridian.example/platform/deploy.git"},
		"only the root": {"https://gitlab.meridian.example/gitlab/api/v4", "https://gitlab.meridian.example/gitlab/deploy.git"},
	} {
		if _, err := NewMergeRequests(c.api, c.repo, "t", nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if m, err := NewMergeRequests("https://gitlab.meridian.example/gitlab/api/v4", "https://gitlab.meridian.example/gitlab/platform/deploy", "t", nil); err != nil || url.PathEscape(m.Path) != "platform%2Fdeploy" {
		t.Errorf("a relative root on the repo's host: %+v %v, want the project platform/deploy", m, err)
	}
}

// A merge request is read as a pull request: merged is merged, closed closed, opened and locked open, and its head
// is this project's only when its source project is its target, never a fork's.
func TestAMergeRequestIsReadAsAPullRequest(t *testing.T) {
	mrs := &MergeRequests{Path: "meridian/deploy"}
	for state, want := range map[string]struct {
		state  string
		merged bool
	}{"merged": {"closed", true}, "closed": {"closed", false}, "opened": {"open", false}, "locked": {"open", false}} {
		pr := mrs.pull(mergeRequest{IID: 3, State: state, SourceProjectID: 9, TargetProjectID: 9})
		if pr.State != want.state || pr.Merged != want.merged || pr.Number != 3 || !mrs.Own(pr) {
			t.Errorf("%s read as %+v", state, pr)
		}
	}
	if fork := mrs.pull(mergeRequest{IID: 4, State: "opened", SourceProjectID: 12, TargetProjectID: 9}); mrs.Own(fork) {
		t.Error("a fork's merge request read as this project's own")
	}
}

// A fork's merge request under Canyon's branch name is never Canyon's: Canyon opens its own.
func TestAForksMergeRequestIsNeverCanyons(t *testing.T) {
	r := newRemote(t)
	r.api = "gitlab"
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	fork := gitopstest.PullRequest{Number: 1, State: "open", URL: strings.TrimSuffix(r.URL, ".git") + "/-/merge_requests/1"}
	fork.Head.Ref, fork.Head.Repo.FullName, fork.Base.Ref = "canyon/claims-portal/prod/8f2a", "someone/deploy", "main"
	r.SetPulls([]gitopstest.PullRequest{fork})
	landed, err := repo.Deliver(t.Context(), d)
	if err != nil || landed.PullRequest == nil || landed.PullRequest.Number == fork.Number || !repo.Pulls.Own(*landed.PullRequest) {
		t.Fatalf("a fork's merge request under Canyon's branch: %+v %v, want one of Canyon's own", landed.PullRequest, err)
	}
}

// A merge request is named by its iid, the number the project shows as !n, never by GitLab's id across the
// instance: Canyon keeps the iid, and reads the merge request again by it.
func TestAMergeRequestIsKeptByItsIID(t *testing.T) {
	r := newRemote(t)
	r.api = "gitlab"
	repo, d := r.repo(t, true), delivery(t)
	d.Release.Env = "prod"
	d.Release.SidecarEnv["CANYON_TARGET"] = "prod"
	landed, err := repo.Deliver(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if landed.PullRequest == nil || landed.PullRequest.Number != 1 {
		t.Fatalf("kept %+v, want the merge request's iid 1, not its id %d", landed.PullRequest, gitopstest.MergeRequestID(1))
	}
	if got, err := repo.Pulls.Get(t.Context(), landed.PullRequest.Number); err != nil || got.Head.SHA != landed.Commit {
		t.Fatalf("read again by its iid: %+v %v", got, err)
	}
}
