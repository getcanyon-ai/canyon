package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PullRequests is the deploy repo's pull requests: GitHub's pulls (Pulls), or GitLab's merge requests read as pull
// requests (MergeRequests) — contracts/routes.md, The deploy repo.
type PullRequests interface {
	Create(ctx context.Context, title, head, base, body string) (PullRequest, error) // errExists when the API says one of head is open
	Get(ctx context.Context, number int) (PullRequest, error)
	Open(ctx context.Context) ([]PullRequest, error) // every open one, at most openPages pages, more an error
	Close(ctx context.Context, number int) error     // unmerged
	Endpoint() string                                // the API's address, which pulls_api_unavailable's sentence names
	Own(pr PullRequest) bool                         // pr is of this repo's own branch, never a fork's
}

// Pulls is the deploy repo's pull requests through GitHub's REST subset, which GitHub Enterprise Server,
// Gitea and Forgejo serve alike (contracts/routes.md, The deploy repo): create, get, list the open ones,
// close. The token goes in one header, and no redirect is followed.
type Pulls struct {
	API         string // https://api.github.com, or https://<gitea>/api/v1
	Owner, Name string
	Repo        string // the repository's own host, with its port: where a pull request's page may be, beside the API's
	Token       string
	HTTP        *http.Client
}

// PullRequest is one pull request as the API answers it, the fields Canyon reads.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"` // nil for a fork deleted since
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// canyons is whether pr is a pull request of this repo's own branch into base: never one from a fork, which can
// name the same branch.
func canyons(api PullRequests, pr PullRequest, branch, base string) bool {
	return pr.Head.Ref == branch && pr.Base.Ref == base && api.Own(pr)
}

// ownRepo is whether pr's head is the repository full names, a fork's never.
func ownRepo(pr PullRequest, full string) bool {
	return pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, full)
}

// errExists is a create the API answered 422 or 409: the branch's pull request is open already.
var errExists = errors.New("gitops: a pull request of this branch is open")

// NewPulls is the API at api, https alone, for the repo url names, /<owner>/<repo> with .git or not.
func NewPulls(api, repoURL, token string, client *http.Client) (*Pulls, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return nil, err
	}
	if a, err := url.Parse(api); err != nil || a.Scheme != "https" {
		return nil, errors.New("gitops: the pull requests API is not an https address")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("gitops: %s is not /<owner>/<repo>", u.Path)
	}
	return &Pulls{API: strings.TrimSuffix(api, "/"), Owner: parts[0], Name: parts[1], Repo: u.Host, Token: token, HTTP: client}, nil
}

// errForeignURL is a pull request whose page is not https on the repo's or the API's host: never kept, never shown.
var errForeignURL = errors.New("gitops: a pull request's address is not https on the repo's or the API's host")

// page checks a pull request's address an API answered: https, on the repository's host or the API's.
func page(pr PullRequest, repoHost, apiURL string) error {
	u, err := url.Parse(pr.URL)
	api, aerr := url.Parse(apiURL)
	if err != nil || aerr != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || (u.Host != repoHost && u.Host != api.Host) {
		return errForeignURL
	}
	return nil
}

// call is one API call: the token in its one header of headers, at most 10 seconds, no redirect followed, an answer outside
// 2xx its status alone — never the body, which the remote shaped — and the answer decoded into into.
func call(ctx context.Context, client *http.Client, method, address string, headers map[string]string, body, into any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		in = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, in)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := client
	if c == nil {
		c = &http.Client{}
	}
	once := *c
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := once.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, fmt.Errorf("gitops: the pull requests API answered %d", resp.StatusCode)
	}
	if into == nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

func (p *Pulls) call(ctx context.Context, method, path string, body, into any) (int, error) {
	address := p.API + "/repos/" + url.PathEscape(p.Owner) + "/" + url.PathEscape(p.Name) + path
	return call(ctx, p.HTTP, method, address, map[string]string{"Authorization": "token " + p.Token, "Accept": "application/vnd.github+json"}, body, into)
}

// Endpoint is the API's address.
func (p *Pulls) Endpoint() string { return p.API }

// Own is whether pr's head is this repo's own branch: its head repo owner/name, a fork's never.
func (p *Pulls) Own(pr PullRequest) bool { return ownRepo(pr, p.Owner+"/"+p.Name) }

// Create opens a pull request of head into base: errExists when the API says one is open already.
func (p *Pulls) Create(ctx context.Context, title, head, base, body string) (PullRequest, error) {
	var pr PullRequest
	status, err := p.call(ctx, http.MethodPost, "/pulls", map[string]string{"title": title, "head": head, "base": base, "body": body}, &pr)
	if status == http.StatusUnprocessableEntity || status == http.StatusConflict {
		return pr, errExists
	}
	if err != nil {
		return pr, err
	}
	return pr, page(pr, p.Repo, p.API)
}

// Get is one pull request.
func (p *Pulls) Get(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	if _, err := p.call(ctx, http.MethodGet, "/pulls/"+strconv.Itoa(number), nil, &pr); err != nil {
		return pr, err
	}
	return pr, page(pr, p.Repo, p.API)
}

// openPages is how many pages Open reads: a repo whose open pull requests go on past that is an error, never a
// list cut short, so an older Canyon pull request is never left open unseen.
const openPages = 10

// everyPage reads list's pages from 1 until one is empty — never until one is short: GitHub takes per_page,
// Gitea and Forgejo take limit and cap it at their own MAX_RESPONSE_ITEMS, and GitLab caps per_page at 100, so a
// short page is no end — at most openPages pages, more an error, each pull request's address checked.
func everyPage(ctx context.Context, list func(context.Context, int) ([]PullRequest, error), check func(PullRequest) error) ([]PullRequest, error) {
	var all []PullRequest
	for n := 1; n <= openPages+1; n++ {
		prs, err := list(ctx, n)
		if err != nil {
			return nil, err
		}
		if len(prs) == 0 {
			return all, nil
		}
		if n > openPages {
			break
		}
		for _, pr := range prs {
			if err := check(pr); err != nil {
				return nil, err
			}
		}
		all = append(all, prs...)
	}
	return nil, fmt.Errorf("gitops: open pull requests go on past %d pages", openPages)
}

// Open is the repo's open pull requests, every page (everyPage).
func (p *Pulls) Open(ctx context.Context) ([]PullRequest, error) {
	return everyPage(ctx, func(c context.Context, n int) ([]PullRequest, error) {
		var prs []PullRequest
		_, err := p.call(c, http.MethodGet, "/pulls?state=open&per_page=100&limit=100&page="+strconv.Itoa(n), nil, &prs)
		return prs, err
	}, func(pr PullRequest) error { return page(pr, p.Repo, p.API) })
}

// Close closes a pull request unmerged.
func (p *Pulls) Close(ctx context.Context, number int) error {
	_, err := p.call(ctx, http.MethodPatch, "/pulls/"+strconv.Itoa(number), map[string]string{"state": "closed"}, nil)
	return err
}
