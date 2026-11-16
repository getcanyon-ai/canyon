package gitops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MergeRequests is the deploy repo's merge requests through GitLab's REST API (v4), each read as a pull request
// (contracts/routes.md, The deploy repo): the project named by its whole path, URL-encoded, the token in
// PRIVATE-TOKEN, and no redirect followed. The API is on the repository's own host, so the token goes nowhere else.
type MergeRequests struct {
	API   string // https://<repo host>/api/v4
	Path  string // the project's full path, <group>[/<subgroup>…]/<project>
	Repo  string // the repository's own host, with its port
	Token string
	HTTP  *http.Client
}

// NewMergeRequests is GitLab's API at api for the project url names: api https on the repository's own host and
// port, its path GitLab's own root — "" or a relative one such as /gitlab — then /api/v4; the project the url's path
// below that root, two segments or more, .git or not. Anything else is refused, so the token goes nowhere else.
func NewMergeRequests(api, repoURL, token string, client *http.Client) (*MergeRequests, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return nil, err
	}
	a, err := url.Parse(api)
	if err != nil {
		return nil, err
	}
	if a.Scheme != "https" || a.Host != u.Host {
		return nil, fmt.Errorf("gitops: the merge requests API is not https on the repository's host %s", u.Host)
	}
	root, ok := strings.CutSuffix(strings.TrimSuffix(a.Path, "/"), "/api/v4")
	below, under := strings.CutPrefix(strings.TrimSuffix(u.Path, ".git"), root+"/")
	if !ok || !under {
		return nil, fmt.Errorf("gitops: %s is not GitLab's /api/v4 at the root %s starts", a.Path, u.Path)
	}
	path := strings.Trim(below, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("gitops: %s is not /<group>/<project>", u.Path)
	}
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("gitops: %s is not /<group>/<project>", u.Path)
		}
	}
	return &MergeRequests{API: strings.TrimSuffix(api, "/"), Path: path, Repo: u.Host, Token: token, HTTP: client}, nil
}

// mergeRequest is one merge request as GitLab answers it, the fields Canyon reads.
type mergeRequest struct {
	IID             int    `json:"iid"`
	WebURL          string `json:"web_url"`
	State           string `json:"state"` // opened, closed, locked or merged
	SHA             string `json:"sha"`   // the source branch's head
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
}

// pull is mr as a pull request: merged is merged, closed is closed unmerged, anything else — opened, or the
// short-lived locked — is open; its head is this project's only when its source project is its target, never a
// fork's.
func (m *MergeRequests) pull(mr mergeRequest) PullRequest {
	pr := PullRequest{Number: mr.IID, URL: mr.WebURL, State: "open", Merged: mr.State == "merged"}
	if mr.State == "merged" || mr.State == "closed" {
		pr.State = "closed"
	}
	pr.Head.Ref, pr.Head.SHA, pr.Base.Ref = mr.SourceBranch, mr.SHA, mr.TargetBranch
	if mr.SourceProjectID != 0 && mr.SourceProjectID == mr.TargetProjectID {
		pr.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{m.Path}
	}
	return pr
}

func (m *MergeRequests) call(ctx context.Context, method, path string, body, into any) (int, error) {
	address := m.API + "/projects/" + url.PathEscape(m.Path) + "/merge_requests" + path
	return call(ctx, m.HTTP, method, address, map[string]string{"PRIVATE-TOKEN": m.Token, "Accept": "application/json"}, body, into)
}

// Endpoint is the API's address.
func (m *MergeRequests) Endpoint() string { return m.API }

// Own is whether pr's head is this project's own branch, a fork's never.
func (m *MergeRequests) Own(pr PullRequest) bool { return ownRepo(pr, m.Path) }

// Create opens a merge request of head into base: errExists when GitLab answers 409. Canyon looks for the open
// one before it creates one (Repo.propose), so it never depends on how GitLab answers a second one.
func (m *MergeRequests) Create(ctx context.Context, title, head, base, body string) (PullRequest, error) {
	var mr mergeRequest
	status, err := m.call(ctx, http.MethodPost, "", map[string]string{"source_branch": head, "target_branch": base, "title": title, "description": body}, &mr)
	if status == http.StatusConflict {
		return PullRequest{}, errExists
	}
	if err != nil {
		return PullRequest{}, err
	}
	pr := m.pull(mr)
	return pr, page(pr, m.Repo, m.API)
}

// Get is one merge request, by its iid.
func (m *MergeRequests) Get(ctx context.Context, number int) (PullRequest, error) {
	var mr mergeRequest
	if _, err := m.call(ctx, http.MethodGet, "/"+strconv.Itoa(number), nil, &mr); err != nil {
		return PullRequest{}, err
	}
	pr := m.pull(mr)
	return pr, page(pr, m.Repo, m.API)
}

// Open is the project's open merge requests, a hundred a page (GitLab's most), every page (everyPage).
func (m *MergeRequests) Open(ctx context.Context) ([]PullRequest, error) {
	return everyPage(ctx, func(c context.Context, n int) ([]PullRequest, error) {
		var mrs []mergeRequest
		if _, err := m.call(c, http.MethodGet, "?state=opened&per_page=100&page="+strconv.Itoa(n), nil, &mrs); err != nil {
			return nil, err
		}
		prs := make([]PullRequest, len(mrs))
		for i, mr := range mrs {
			prs[i] = m.pull(mr)
		}
		return prs, nil
	}, func(pr PullRequest) error { return page(pr, m.Repo, m.API) })
}

// Close closes a merge request unmerged.
func (m *MergeRequests) Close(ctx context.Context, number int) error {
	_, err := m.call(ctx, http.MethodPut, "/"+strconv.Itoa(number), map[string]string{"state_event": "close"}, nil)
	return err
}
