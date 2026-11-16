// Package githubapp is Canyon's GitHub App courier (in github-app/: a package name has no hyphen).
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// githubJSON is the media type of every call but a SARIF download.
const githubJSON = "application/vnd.github+json"

// Client speaks GitHub's REST API as one App. No retry: the DBOS step that calls it is the retry.
type Client struct {
	base    string // CANYON_GITHUB_API, https://api.github.com by default
	appID   int64
	key     *rsa.PrivateKey
	http    *http.Client
	now     func() time.Time
	timeout time.Duration
	mu      sync.Mutex
	tokens  map[int64]installationToken // in memory only, never persisted
}

type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// New builds the one client for the App, 30 s timeout. It follows one redirect (the tarball's 302 to
// codeload) and never carries Authorization to another host: the redirect's URL has its own token.
func New(base string, appID int64, key *rsa.PrivateKey) *Client {
	return newWithTimeout(base, appID, key, 30*time.Second)
}

// newWithTimeout bounds each hop's wait for headers, and a JSON call in all; a tarball streams.
func newWithTimeout(base string, appID int64, key *rsa.PrivateKey, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone() // its dial and TLS handshake timeouts
	transport.ResponseHeaderTimeout = timeout
	return &Client{base: strings.TrimSuffix(base, "/"), appID: appID, key: key, now: time.Now, timeout: timeout, tokens: map[int64]installationToken{},
		http: &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 1 {
				return errors.New("a second redirect")
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		}},
	}
}

// CheckRun is the part of a check run Canyon writes; empty fields are left out of the body.
type CheckRun struct {
	Name       string  `json:"name,omitempty"`
	HeadSHA    string  `json:"head_sha,omitempty"`
	Status     string  `json:"status,omitempty"`
	Conclusion string  `json:"conclusion,omitempty"`
	Output     *Output `json:"output,omitempty"`
}

// Output is a check run's title, summary and annotations.
type Output struct {
	Title       string       `json:"title"`
	Summary     string       `json:"summary"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Annotation is one finding on a line range. No columns: SARIF counts code points, GitHub does not.
type Annotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Title           string `json:"title,omitempty"`
	Message         string `json:"message"`
	RawDetails      string `json:"raw_details,omitempty"`
}

// CreateCheckRun opens a check run on owner/repo and answers its id.
func (c *Client) CreateCheckRun(ctx context.Context, installation int64, owner, repo string, run CheckRun) (int64, error) {
	var created struct{ ID int64 }
	_, err := c.installed(ctx, installation, http.MethodPost, owner, repo, "/check-runs", githubJSON, run, &created)
	return created.ID, err
}

// UpdateCheckRun changes check run id: its status, conclusion or output.
func (c *Client) UpdateCheckRun(ctx context.Context, installation int64, owner, repo string, id int64, run CheckRun) error {
	_, err := c.installed(ctx, installation, http.MethodPatch, owner, repo, "/check-runs/"+strconv.FormatInt(id, 10), githubJSON, run, &struct{}{})
	return err
}

// Tarball opens the gzipped tree of owner/repo at sha; the caller closes it. Ingest's caps refuse one too large.
func (c *Client) Tarball(ctx context.Context, installation int64, owner, repo, sha string) (io.ReadCloser, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("github: %q is not a commit sha", sha)
	}
	return c.installed(ctx, installation, http.MethodGet, owner, repo, "/tarball/"+sha, githubJSON, nil, nil)
}

// Analysis is one code-scanning analysis as GitHub lists it, the part the courier reads.
type Analysis struct {
	ID        int64     `json:"id"`
	CommitSHA string    `json:"commit_sha"`
	Category  string    `json:"category"`
	CreatedAt time.Time `json:"created_at"`
	Tool      struct {
		Name string `json:"name"`
	} `json:"tool"`
}

// analysisPages bounds a listing of analyses: 500, newest first.
const analysisPages = 5

// Analyses answers owner/repo's code-scanning analyses of ref, every ref's when empty, newest first: page
// by page, as GitHub's Link header numbers them, until one is short or analysisPages are read.
func (c *Client) Analyses(ctx context.Context, installation int64, owner, repo, ref string) ([]Analysis, error) {
	q := url.Values{"per_page": {"100"}}
	if ref != "" {
		q.Set("ref", ref)
	}
	var all []Analysis
	for page := 1; page <= analysisPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var out []Analysis
		if _, err := c.installed(ctx, installation, http.MethodGet, owner, repo, "/code-scanning/analyses?"+q.Encode(), githubJSON, nil, &out); err != nil {
			return nil, err
		}
		if all = append(all, out...); len(out) < 100 {
			break
		}
	}
	return all, nil
}

// AnalysisSARIF opens analysis id as the SARIF it was uploaded as; the caller closes it.
func (c *Client) AnalysisSARIF(ctx context.Context, installation int64, owner, repo string, id int64) (io.ReadCloser, error) {
	return c.installed(ctx, installation, http.MethodGet, owner, repo, "/code-scanning/analyses/"+strconv.FormatInt(id, 10), "application/sarif+json", nil, nil)
}

// FolderTree answers the git tree id of folder at commit sha in owner/repo, "" when the commit has no
// such folder: one trees read per path segment (#1917). A listing GitHub cut short is an error, never a
// guess at what it left out.
func (c *Client) FolderTree(ctx context.Context, installation int64, owner, repo, sha, folder string) (string, error) {
	tree := sha
	for _, segment := range strings.Split(folder, "/") {
		var out struct {
			Tree []struct {
				Path, Type, SHA string
			} `json:"tree"`
			Truncated bool `json:"truncated"`
		}
		if _, err := c.installed(ctx, installation, http.MethodGet, owner, repo, "/git/trees/"+url.PathEscape(tree), githubJSON, nil, &out); err != nil {
			return "", err
		}
		if out.Truncated {
			return "", fmt.Errorf("github: the tree %s of %s/%s is too large to list", tree, owner, repo)
		}
		next := ""
		for _, entry := range out.Tree {
			if entry.Path == segment && entry.Type == "tree" {
				next = entry.SHA
			}
		}
		if next == "" {
			return "", nil
		}
		tree = next
	}
	return tree, nil
}

// Conversion is GitHub's answer for a registered manifest; the client secret is what linking a person's
// GitHub account exchanges its code with (#2228).
type Conversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	Owner         struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"` // Organization, or User for an App registered on a personal account
	} `json:"owner"`
	Permissions map[string]string `json:"permissions"`
}

// ConvertManifest exchanges the code GitHub returned the admin with for the new App's credentials.
// The call carries no authorization (the code is the proof), and an error never names the code.
func (c *Client) ConvertManifest(ctx context.Context, code string) (Conversion, error) {
	var out Conversion
	if code == "" {
		return out, errors.New("github: no manifest code")
	}
	escaped := url.PathEscape(code)
	if _, err := c.do(ctx, http.MethodPost, "/app-manifests/"+escaped+"/conversions", "", githubJSON, nil, &out); err != nil {
		return Conversion{}, errors.New(strings.ReplaceAll(err.Error(), escaped, "{code}"))
	}
	return out, nil
}

// installed is do on owner/repo as the installation.
func (c *Client) installed(ctx context.Context, installation int64, method, owner, repo, path, accept string, in, out any) (io.ReadCloser, error) {
	if err := validRepo(owner, repo); err != nil {
		return nil, err
	}
	return c.as(ctx, installation, method, "/repos/"+owner+"/"+repo+path, accept, in, out)
}

// Repository is one repository an installation sees, as GitHub lists it (#1917).
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// repositoryPages bounds a listing of an installation's repositories: 3,000.
const repositoryPages = 30

// Repositories answers every repository the installation sees, page by page until one is short; one
// that sees more than repositoryPages hold is an error, never a list cut short.
func (c *Client) Repositories(ctx context.Context, installation int64) ([]Repository, error) {
	var all []Repository
	for page := 1; page <= repositoryPages; page++ {
		var out struct {
			Repositories []Repository `json:"repositories"`
		}
		if _, err := c.as(ctx, installation, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), githubJSON, nil, &out); err != nil {
			return nil, err
		}
		if all = append(all, out.Repositories...); len(out.Repositories) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: installation %d sees more than %d repositories", installation, 100*repositoryPages)
}

// Head answers the commit owner/repo's branch points at.
func (c *Client) Head(ctx context.Context, installation int64, owner, repo, branch string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	if _, err := c.installed(ctx, installation, http.MethodGet, owner, repo, "/commits/"+url.PathEscape(branch), githubJSON, nil, &out); err != nil {
		return "", err
	}
	if !validSHA(out.SHA) {
		return "", fmt.Errorf("github: %s/%s's %s answered no commit", owner, repo, branch)
	}
	return out.SHA, nil
}

// as is do as the installation. A 401 drops the cached token: the next call mints one.
func (c *Client) as(ctx context.Context, installation int64, method, path, accept string, in, out any) (io.ReadCloser, error) {
	token, err := c.token(ctx, installation)
	if err != nil {
		return nil, err
	}
	body, err := c.do(ctx, method, path, "token "+token, accept, in, out)
	if status := statusError(0); errors.As(err, &status) && status == http.StatusUnauthorized {
		c.mu.Lock()
		delete(c.tokens, installation)
		c.mu.Unlock()
	}
	return body, err
}

// token is the installation's cached token until a minute before it expires, else one minted with the App JWT.
func (c *Client) token(ctx context.Context, installation int64) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[installation]
	c.mu.Unlock()
	if ok && c.now().Before(t.ExpiresAt.Add(-time.Minute)) {
		return t.Token, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	var minted installationToken
	if _, err := c.do(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens", "Bearer "+jwt, githubJSON, nil, &minted); err != nil {
		return "", err
	}
	if minted.Token == "" {
		return "", errors.New("github: installation token: none in the answer")
	}
	c.mu.Lock()
	c.tokens[installation] = minted
	c.mu.Unlock()
	return minted.Token, nil
}

// jwt is the App's RS256 token: issued a minute back against clock drift, expiring nine minutes on (GitHub caps ten).
func (c *Client) jwt() (string, error) {
	now := c.now().Unix()
	signed := b64(`{"alg":"RS256","typ":"JWT"}`) + "." + b64(fmt.Sprintf(`{"iss":%d,"iat":%d,"exp":%d}`, c.appID, now-60, now+540))
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github: app jwt: %w", err)
	}
	return signed + "." + b64(string(sig)), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// do sends one request. A JSON call (out not nil) is bounded by the timeout in all and its answer
// decoded into out; else the body is answered to stream. An error names the method, the path and
// GitHub's message: never the URL it ended on (a redirect's carries a token), never a credential.
func (c *Client) do(ctx context.Context, method, path, auth, accept string, in, out any) (io.ReadCloser, error) {
	if out != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var r io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r) //nolint:gosec // the host is CANYON_GITHUB_API, fixed at boot; every caller validates or escapes its path segments
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	if auth != "" { // the manifest conversion is the one call made as nobody
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) //nolint:gosec // as above
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 == 2 && out == nil {
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 { // GitHub's {"message": …}, or the raw body when it is not that
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct{ Message string }
		if json.Unmarshal(raw, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("github: %s %s answered %w: %s", method, path, statusError(resp.StatusCode), e.Message)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	return nil, nil
}

// statusError is a non-2xx status GitHub answered.
type statusError int

func (s statusError) Error() string { return strconv.Itoa(int(s)) }

// validRepo refuses a name outside GitHub's charset, or . or .., so none can reshape the request path.
func validRepo(owner, repo string) error {
	for _, s := range []string{owner, repo} {
		if s == "" || len(s) > 100 || s == "." || s == ".." || strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
			return fmt.Errorf("github: %q is not a repository name", owner+"/"+repo)
		}
	}
	return nil
}

// validSHA is 40 lowercase hex: a full commit id, as webhooks carry it.
func validSHA(s string) bool {
	return len(s) == 40 && strings.Trim(s, "0123456789abcdef") == ""
}
