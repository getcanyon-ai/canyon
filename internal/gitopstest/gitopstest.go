// Package gitopstest is a deploy repo for tests (design/HAND-OFF.md §3b): git's own http-backend over TLS behind
// a check of one token, with GitHub's pulls subset beside it — what a GitHub, a Gitea or a Forgejo serves, with
// nothing of theirs — and GitLab's merge requests subset (v4) over the same pull requests, as GitLab's docs
// describe it (#2480). main and preview start with the templates of contracts/testdata/release/repo under
// apps/.canyon; a test merges, edits and closes pull requests as a person would.
package gitopstest

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/cgi" //nolint:gosec // git's own http-backend for tests, on Go well past 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// User and Token are the remote's one credential: a fixture, never a real token.
const (
	User  = "x-access-token"
	Token = "deploy-token-fixture-not-real" //nolint:gosec // a test fixture, not a real token
)

// PullRequest is the pulls API's shape, as GitHub answers it.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// Remote is the deploy repo meridian/deploy.
type Remote struct {
	t      *testing.T
	Dir    string // the served bare repository
	URL    string // https://<host>/meridian/deploy.git
	API    string // the pulls API's base: <URL's host>/api
	GitLab string // GitLab's merge requests API's base: <URL's host>/api/v4
	Client *http.Client
	srv    *httptest.Server
	mu     sync.Mutex
	pulls  []PullRequest
	closed []int
	// MaxPage caps a page of the list as Gitea's MAX_RESPONSE_ITEMS does, whatever per_page asks; 0 for none.
	MaxPage int
	// RefuseCreate answers every create, a pull request's or a merge request's, 500: an API that errs on a second one.
	RefuseCreate bool
	onPull       func()
}

// OnPull runs f on each read of one pull request, before it is answered: a test lands a hold, a remove or a delete
// while Canyon reads the deploy repo.
func (r *Remote) OnPull(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onPull = f
}

// Git runs git in dir, as someone other than Canyon, and answers its output.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // the test's own repositories
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=someone", "GIT_AUTHOR_EMAIL=someone@meridian.example", "GIT_COMMITTER_NAME=someone",
		"GIT_COMMITTER_EMAIL=someone@meridian.example", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// New serves the deploy repo, templates the directory .canyon's files are read from (the contract's), and
// points git's CA file at its certificate for the rest of the test.
func New(t *testing.T, templates string) *Remote {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	r := &Remote{t: t, Dir: filepath.Join(t.TempDir(), "deploy.git")}
	Git(t, ".", "init", "-q", "--bare", "-b", "main", r.Dir)
	Git(t, r.Dir, "config", "http.receivepack", "true")
	work := t.TempDir()
	Git(t, work, "init", "-q", "-b", "main")
	if err := filepath.WalkDir(templates, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(templates, path)
		raw, err := os.ReadFile(path) //nolint:gosec // the contract's own fixtures
		if err != nil {
			return err
		}
		dst := filepath.Join(work, "apps", ".canyon", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o600) //nolint:gosec // a path under the test's own temporary directory
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("the deploy repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Git(t, work, "add", ".")
	Git(t, work, "commit", "-q", "-m", "templates")
	Git(t, work, "push", "-q", r.Dir, "main", "main:preview")
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(r.Dir), "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + User}}
	mux := http.NewServeMux()
	mux.Handle("/meridian/", http.StripPrefix("/meridian", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Header.Get("Authorization"), "Basic "+base64.StdEncoding.EncodeToString([]byte(User+":"+Token))) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, req)
	})))
	mux.HandleFunc("/api/repos/meridian/deploy/pulls", r.servePulls)
	mux.HandleFunc("/api/repos/meridian/deploy/pulls/{n}", r.servePull)
	mux.HandleFunc("/api/v4/projects/{id}/merge_requests", r.serveMergeRequests)
	mux.HandleFunc("/api/v4/projects/{id}/merge_requests/{iid}", r.serveMergeRequest)
	r.srv = httptest.NewTLSServer(mux)
	t.Cleanup(r.srv.Close)
	r.URL, r.API, r.GitLab, r.Client = r.srv.URL+"/meridian/deploy.git", r.srv.URL+"/api", r.srv.URL+"/api/v4", r.srv.Client()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca) // git's CA file, as the server's is git's
	return r
}

func (r *Remote) authorized(w http.ResponseWriter, req *http.Request) bool {
	if req.Header.Get("Authorization") != "token "+Token {
		http.Error(w, "no", http.StatusUnauthorized)
		return false
	}
	return true
}

func (r *Remote) servePulls(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorized(w, req) {
		return
	}
	if req.Method == http.MethodGet { // the open ones, per_page a page (30 when absent, as GitHub's), page counted from 1
		open := []PullRequest{}
		for _, pr := range r.pulls {
			if pr.State == "open" {
				open = append(open, pr)
			}
		}
		per, _ := strconv.Atoi(req.URL.Query().Get("per_page"))
		if per < 1 {
			per = 30
		}
		if r.MaxPage > 0 {
			per = min(per, r.MaxPage)
		}
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		from := min(max(page-1, 0)*per, len(open))
		_ = json.NewEncoder(w).Encode(open[from:min(from+per, len(open))])
		return
	}
	if r.RefuseCreate {
		http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		return
	}
	var in struct{ Title, Head, Base, Body string }
	_ = json.NewDecoder(req.Body).Decode(&in)
	for _, pr := range r.pulls {
		if pr.Head.Ref == in.Head && pr.State == "open" {
			http.Error(w, `{"message":"A pull request already exists"}`, http.StatusUnprocessableEntity)
			return
		}
	}
	pr := PullRequest{Number: len(r.pulls) + 1, State: "open"}
	pr.URL = strings.TrimSuffix(r.URL, ".git") + "/pull/" + strconv.Itoa(pr.Number)
	pr.Head.Ref, pr.Head.SHA, pr.Base.Ref = in.Head, Git(r.t, r.Dir, "rev-parse", "refs/heads/"+in.Head), in.Base
	pr.Head.Repo.FullName = "meridian/deploy"
	r.pulls = append(r.pulls, pr)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pr)
}

func (r *Remote) servePull(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	hook := r.onPull
	r.mu.Unlock()
	if hook != nil && req.Method == http.MethodGet {
		hook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorized(w, req) {
		return
	}
	n, _ := strconv.Atoi(req.PathValue("n"))
	if n < 1 || n > len(r.pulls) {
		http.NotFound(w, req)
		return
	}
	if req.Method == http.MethodPatch {
		r.pulls[n-1].State = "closed"
		r.closed = append(r.closed, n)
	}
	_ = json.NewEncoder(w).Encode(r.pulls[n-1])
}

// Pulls is every pull request opened, and the numbers closed through the API.
func (r *Remote) Pulls() ([]PullRequest, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PullRequest(nil), r.pulls...), append([]int(nil), r.closed...)
}

// SetPulls is the API's pull requests from now on.
func (r *Remote) SetPulls(prs []PullRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = prs
}

// Commit is someone's commit on branch writing files, by path: what Canyon finds there afterwards.
func (r *Remote) Commit(branch, message string, files map[string]string) string {
	r.t.Helper()
	work := r.t.TempDir()
	Git(r.t, work, "clone", "-q", "-b", branch, r.Dir, ".")
	for path, body := range files {
		p := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	Git(r.t, work, "add", ".")
	Git(r.t, work, "commit", "-q", "-m", message)
	Git(r.t, work, "push", "-q", "origin", branch)
	return Git(r.t, r.Dir, "rev-parse", "refs/heads/"+branch)
}

// Merge merges pull request n into its base, as a person's merge does, after edit commits files onto its branch
// first when it names any: an edited head is what Canyon must refuse.
func (r *Remote) Merge(n int, edit map[string]string) {
	r.t.Helper()
	r.mu.Lock()
	pr := r.pulls[n-1]
	r.mu.Unlock()
	head := pr.Head.SHA
	if len(edit) > 0 {
		head = r.Commit(pr.Head.Ref, "an edit after Canyon opened it", edit)
	}
	work := r.t.TempDir()
	Git(r.t, work, "clone", "-q", "-b", pr.Base.Ref, r.Dir, ".")
	Git(r.t, work, "fetch", "-q", "origin", pr.Head.Ref)
	Git(r.t, work, "merge", "-q", "--no-ff", "-m", "Merge pull request #"+strconv.Itoa(n), "FETCH_HEAD")
	Git(r.t, work, "push", "-q", "origin", pr.Base.Ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls[n-1].State, r.pulls[n-1].Merged, r.pulls[n-1].Head.SHA = "closed", true, head
}

// Close closes pull request n unmerged, as a person does.
func (r *Remote) Close(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls[n-1].State = "closed"
}

// Project is the deploy repo's path, the merge requests API's project id once URL-encoded whole.
const Project = "meridian/deploy"

// MergeRequestID is merge request iid's id across the instance, as GitLab answers it beside the iid: never the
// same number, so a client that keeps the id is seen to.
func MergeRequestID(iid int) int { return 1000 + iid }

// MergeRequest is a merge request as GitLab's API answers it, the fields Canyon reads and its id, which it must not.
type MergeRequest struct {
	ID              int    `json:"id"`
	IID             int    `json:"iid"`
	WebURL          string `json:"web_url"`
	State           string `json:"state"`
	SHA             string `json:"sha"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	SourceProjectID int    `json:"source_project_id"`
	TargetProjectID int    `json:"target_project_id"`
}

// mergeRequest is pr as GitLab answers it: open is opened (locked stays locked), closed merged is merged, and a
// pull request whose head is not meridian/deploy is a fork's, its source another project.
func mergeRequest(pr PullRequest) MergeRequest {
	state := map[string]string{"open": "opened", "closed": "closed", "locked": "locked"}[pr.State]
	if pr.State == "closed" && pr.Merged {
		state = "merged"
	}
	source := 1
	if pr.Head.Repo.FullName != Project {
		source = 2
	}
	return MergeRequest{ID: MergeRequestID(pr.Number), IID: pr.Number, WebURL: pr.URL, State: state, SHA: pr.Head.SHA, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref, SourceProjectID: source, TargetProjectID: 1}
}

// gitlabAuthorized is GitLab's token header, and the project named by its whole path, URL-encoded: an id sent
// unencoded is more segments, which no route matches.
func (r *Remote) gitlabAuthorized(w http.ResponseWriter, req *http.Request) bool {
	if req.PathValue("id") != Project || !strings.Contains(req.URL.EscapedPath(), "/projects/meridian%2Fdeploy/") {
		http.NotFound(w, req)
		return false
	}
	if req.Header.Get("PRIVATE-TOKEN") != Token {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (r *Remote) serveMergeRequests(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.gitlabAuthorized(w, req) {
		return
	}
	switch req.Method {
	case http.MethodGet: // state=opened, per_page a page (20 when absent, 100 at most), page counted from 1
		if req.URL.Query().Get("state") != "opened" {
			http.Error(w, `{"error":"state does not have a valid value"}`, http.StatusBadRequest)
			return
		}
		open := []MergeRequest{}
		for _, pr := range r.pulls {
			if pr.State == "open" || pr.State == "locked" {
				open = append(open, mergeRequest(pr))
			}
		}
		per, _ := strconv.Atoi(req.URL.Query().Get("per_page"))
		if per < 1 {
			per = 20
		}
		per = min(per, 100)
		if r.MaxPage > 0 {
			per = min(per, r.MaxPage)
		}
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		from := min(max(page-1, 0)*per, len(open))
		_ = json.NewEncoder(w).Encode(open[from:min(from+per, len(open))])
	case http.MethodPost:
		if r.RefuseCreate {
			http.Error(w, `{"message":"500 Internal Server Error"}`, http.StatusInternalServerError)
			return
		}
		var in struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
			Description  string `json:"description"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.SourceBranch == "" || in.TargetBranch == "" || in.Title == "" {
			http.Error(w, `{"error":"source_branch, target_branch, title are missing"}`, http.StatusBadRequest)
			return
		}
		for _, pr := range r.pulls {
			if pr.Head.Ref == in.SourceBranch && pr.Head.Repo.FullName == Project && pr.State == "open" {
				http.Error(w, `{"message":["Another open merge request already exists for this source branch"]}`, http.StatusConflict)
				return
			}
		}
		pr := PullRequest{Number: len(r.pulls) + 1, State: "open"}
		pr.URL = strings.TrimSuffix(r.URL, ".git") + "/-/merge_requests/" + strconv.Itoa(pr.Number)
		pr.Head.Ref, pr.Head.SHA, pr.Base.Ref = in.SourceBranch, Git(r.t, r.Dir, "rev-parse", "refs/heads/"+in.SourceBranch), in.TargetBranch
		pr.Head.Repo.FullName = Project
		r.pulls = append(r.pulls, pr)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(mergeRequest(pr))
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

func (r *Remote) serveMergeRequest(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	hook := r.onPull
	r.mu.Unlock()
	if hook != nil && req.Method == http.MethodGet {
		hook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.gitlabAuthorized(w, req) {
		return
	}
	n, _ := strconv.Atoi(req.PathValue("iid"))
	if n < 1 || n > len(r.pulls) {
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		return
	}
	switch req.Method {
	case http.MethodGet:
	case http.MethodPut:
		var in struct {
			StateEvent string `json:"state_event"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.StateEvent != "close" {
			http.Error(w, `{"error":"state_event does not have a valid value"}`, http.StatusBadRequest)
			return
		}
		r.pulls[n-1].State = "closed"
		r.closed = append(r.closed, n)
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(mergeRequest(r.pulls[n-1]))
}
