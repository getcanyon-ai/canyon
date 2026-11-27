// Command claguard is the cla check (.github/workflows/cla.yml, #39): a pull request passes once every author of
// its commits has signed CLA.md, by commenting the sentence below on it. Signatures are signatures/v1/cla.json on
// the cla-signatures branch, written with the workflow's own GITHUB_TOKEN. It reads every page of the pull
// request's commits, matches an author by account id, never by a name, and mentions only an account GitHub linked
// the commit to: an author whose email is linked to none is named by the commit's sha. It reads the event and the
// API alone, never the pull request's code. It fails closed: an answer it cannot read, a commit list short of the
// pull request's count, or a pull request that moved on while it ran ends it red.
//
//	go run ./internal/tools/claguard   # in the workflow: GITHUB_TOKEN, GITHUB_REPOSITORY, GITHUB_EVENT_NAME, GITHUB_EVENT_PATH
package main

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	sentence  = "I have read the CLA Document and I hereby sign the CLA"
	document  = "https://github.com/getcanyon-ai/canyon/blob/main/CLA.md"
	branch    = "cla-signatures"
	file      = "signatures/v1/cla.json"
	workflow  = "cla.yml"
	marker    = "<!-- cla -->"
	actionsID = 41898282 // github-actions[bot], the only account whose comment the check edits
	perPage   = 100
)

func main() {
	ev, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH")) //nolint:gosec // the runner names the event file
	if err != nil {
		log.Fatalf("claguard: the event: %v", err)
	}
	gh := &github{
		api:   strings.TrimSuffix(cmp.Or(os.Getenv("GITHUB_API_URL"), "https://api.github.com"), "/"),
		repo:  os.Getenv("GITHUB_REPOSITORY"),
		token: os.Getenv("GITHUB_TOKEN"),
		http:  &http.Client{Timeout: 30 * time.Second},
	}
	if gh.repo == "" || gh.token == "" {
		log.Fatal("claguard: GITHUB_REPOSITORY and GITHUB_TOKEN are required")
	}
	if err := check(gh, os.Getenv("GITHUB_EVENT_NAME"), ev); err != nil {
		log.Fatalf("claguard: %v", err)
	}
}

type user struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type event struct {
	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      user   `json:"user"`
	} `json:"comment"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
}

// signature is one entry of cla.json, in the shape the file has held since its first signature.
type signature struct {
	Name          string `json:"name"`
	ID            int64  `json:"id"`
	CommentID     int64  `json:"comment_id"`
	CreatedAt     string `json:"created_at"`
	RepoID        int64  `json:"repoId"`
	PullRequestNo int    `json:"pullRequestNo"`
}

type signatures struct {
	SignedContributors []signature `json:"signedContributors"`
}

// author is who wrote a commit: an account GitHub linked its email to, or, with no account, the commit alone.
type author struct {
	account *user
	commit  string // the first commit by an unlinked email
	email   string // never printed: a commit claims any it likes
}

var errUnsigned = errors.New("not every author has signed the CLA")

// check runs the cla check for one event: a pull_request_target event, or a recheck or signing comment.
func check(gh *github, name string, raw []byte) error {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("the event: %w", err)
	}
	var number int
	switch {
	case name == "pull_request_target" && ev.PullRequest != nil:
		number = ev.PullRequest.Number
	case name == "issue_comment" && ev.Issue != nil && ev.Issue.PullRequest != nil && ev.Comment != nil:
		number = ev.Issue.Number
	default:
		return fmt.Errorf("a %q event is not one the check reads", name)
	}
	var pr struct {
		Commits int `json:"commits"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := gh.get(fmt.Sprintf("pulls/%d", number), &pr); err != nil {
		return err
	}
	if ev.PullRequest != nil && ev.PullRequest.Head.SHA != pr.Head.SHA {
		return fmt.Errorf("pull request #%d moved on from %s to %s: the run for %s decides", number, ev.PullRequest.Head.SHA, pr.Head.SHA, pr.Head.SHA)
	}
	authors, err := commitAuthors(gh, number, pr.Commits, pr.Head.SHA)
	if err != nil {
		return err
	}
	signed, sha, err := gh.signatures()
	if err != nil {
		return err
	}
	missing := unsigned(authors, signed)
	if ev.Comment != nil && strings.TrimSpace(ev.Comment.Body) == sentence {
		for i, a := range missing {
			if a.account == nil || a.account.ID != ev.Comment.User.ID {
				continue
			}
			signed.SignedContributors = append(signed.SignedContributors, signature{
				Name: a.account.Login, ID: a.account.ID, CommentID: ev.Comment.ID, CreatedAt: ev.Comment.CreatedAt,
				RepoID: ev.Repository.ID, PullRequestNo: number,
			})
			if err := gh.sign(signed, sha, fmt.Sprintf("%s has signed the CLA in #%d", a.account.Login, number)); err != nil {
				return err
			}
			missing = append(missing[:i], missing[i+1:]...)
			break
		}
	}
	if err := gh.comment(number, missing); err != nil {
		return err
	}
	if ev.Comment != nil {
		if err := gh.rerun(pr.Head.SHA); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return errUnsigned
	}
	log.Printf("every author of #%d's %d commits has signed the CLA", number, pr.Commits)
	return nil
}

// commitAuthors reads every page of the pull request's commits, and fails unless they are the pull request's
// whole count ending at its head: GitHub lists 250 at most, and a page it drops is an author never checked.
func commitAuthors(gh *github, number, count int, head string) ([]author, error) {
	var authors []author
	seen := map[string]bool{}
	listed, last := 0, ""
	for page := 1; ; page++ {
		var commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Author struct {
					Email string `json:"email"`
				} `json:"author"`
			} `json:"commit"`
			Author *user `json:"author"`
		}
		if err := gh.get(fmt.Sprintf("pulls/%d/commits?per_page=%d&page=%d", number, perPage, page), &commits); err != nil {
			return nil, err
		}
		for _, c := range commits {
			listed, last = listed+1, c.SHA
			a := author{account: c.Author, commit: c.SHA, email: c.Commit.Author.Email}
			key := "email:" + a.email
			if a.account != nil {
				if a.account.ID == 0 || a.account.Login == "" {
					return nil, fmt.Errorf("commit %s: an author account with no id or login", c.SHA)
				}
				key = "id:" + strconv.FormatInt(a.account.ID, 10)
			}
			if !seen[key] {
				seen[key] = true
				authors = append(authors, a)
			}
		}
		if len(commits) < perPage {
			break
		}
	}
	if listed != count || last != head {
		return nil, fmt.Errorf("pull request #%d has %d commits ending at %s, and GitHub listed %d ending at %q: the check reads them all or fails", number, count, head, listed, last)
	}
	return authors, nil
}

// unsigned is the authors with no signature, matched by account id alone: a login can be renamed and taken.
func unsigned(authors []author, signed signatures) []author {
	ids := map[int64]bool{}
	for _, s := range signed.SignedContributors {
		ids[s.ID] = true
	}
	var missing []author
	for _, a := range authors {
		if a.account == nil || !ids[a.account.ID] {
			missing = append(missing, a)
		}
	}
	return missing
}

// body is the check's comment: who has yet to sign, each account by its login and each unlinked author by its
// commit, and how to sign.
func body(missing []author) string {
	if len(missing) == 0 {
		return marker + "\nEvery author of this pull request's commits has signed the [contributor licence agreement](" + document + ")."
	}
	var b strings.Builder
	b.WriteString(marker + "\nBefore this pull request can merge, each author of its commits signs the [contributor licence agreement](" + document + "), once. To sign, comment on this pull request with this sentence:\n\n")
	b.WriteString("```\n" + sentence + "\n```\n\nNot signed yet:\n\n")
	for _, a := range missing {
		if a.account != nil {
			b.WriteString("- @" + a.account.Login + "\n")
			continue
		}
		b.WriteString("- the author of commit " + a.commit[:min(12, len(a.commit))] + ", whose email is linked to no GitHub account: add the email to your account, then sign\n")
	}
	b.WriteString("\nComment `recheck` to check again.")
	return b.String()
}

type github struct {
	api, repo, token string
	http             *http.Client
}

// call sends one request to the repository's API and decodes a 2xx answer into out; any other answer is an error.
func (g *github) call(method, path string, in, out any) (int, error) {
	var payload io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, g.api+"/repos/"+g.repo+"/"+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %.300s", method, path, resp.Status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (g *github) get(path string, out any) error {
	_, err := g.call(http.MethodGet, path, nil, out)
	return err
}

// signatures reads cla.json and its blob sha; a file not there yet is no signatures, any other failure an error.
func (g *github) signatures() (signatures, string, error) {
	var f struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	var s signatures
	status, err := g.call(http.MethodGet, "contents/"+file+"?ref="+branch, nil, &f)
	if status == http.StatusNotFound {
		return s, "", nil
	}
	if err != nil {
		return s, "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return s, "", fmt.Errorf("%s: %w", file, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, "", fmt.Errorf("%s: %w", file, err)
	}
	return s, f.SHA, nil
}

// sign writes cla.json over the blob it read: a signature written in between makes GitHub refuse it, and the
// check fails rather than drop one.
func (g *github) sign(s signatures, sha, message string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	put := map[string]string{"message": message, "branch": branch, "content": base64.StdEncoding.EncodeToString(append(raw, '\n'))}
	if sha != "" {
		put["sha"] = sha
	}
	_, err = g.call(http.MethodPut, "contents/"+file, put, nil)
	return err
}

// comment writes the check's comment: a new one while someone has yet to sign, and an edit of its own, by
// github-actions[bot], when what it says changed.
func (g *github) comment(number int, missing []author) error {
	text := body(missing)
	for page := 1; ; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User user   `json:"user"`
		}
		if err := g.get(fmt.Sprintf("issues/%d/comments?per_page=%d&page=%d", number, perPage, page), &comments); err != nil {
			return err
		}
		for _, c := range comments {
			if c.User.ID != actionsID || !strings.HasPrefix(c.Body, marker) {
				continue
			}
			if c.Body == text {
				return nil
			}
			_, err := g.call(http.MethodPatch, fmt.Sprintf("issues/comments/%d", c.ID), map[string]string{"body": text}, nil)
			return err
		}
		if len(comments) < perPage {
			break
		}
	}
	if len(missing) == 0 {
		return nil
	}
	_, err := g.call(http.MethodPost, fmt.Sprintf("issues/%d/comments", number), map[string]string{"body": text}, nil)
	return err
}

// rerun runs the head's pull_request_target check again after a comment: that run's job is the pull request's
// required cla check, and a comment's own run is not on the pull request. A run still going is left to end, and
// a recheck after it runs it again if it ends red.
func (g *github) rerun(head string) error {
	var runs struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	q := url.Values{"event": {"pull_request_target"}, "head_sha": {head}, "per_page": {"1"}}
	if err := g.get("actions/workflows/"+workflow+"/runs?"+q.Encode(), &runs); err != nil {
		return err
	}
	if len(runs.WorkflowRuns) == 0 {
		return fmt.Errorf("no pull_request_target run of %s for %s to run again", workflow, head)
	}
	r := runs.WorkflowRuns[0]
	if r.Status != "completed" || r.Conclusion == "success" {
		return nil
	}
	_, err := g.call(http.MethodPost, fmt.Sprintf("actions/runs/%d/rerun", r.ID), nil, nil)
	return err
}
