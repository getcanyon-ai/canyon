// Command claguard is the cla check (.github/workflows/cla.yml, #39): a pull request into main passes once the
// account that opened it and every author of its commits have signed CLA.md, by commenting the sentence below on
// it. Signatures are signatures/v1/cla.json on the cla-signatures branch, which holds that file alone, written with
// the workflow's own GITHUB_TOKEN. It reads every page of the pull request's commits and matches an account by its
// id, never a name. The opener signs too, because a commit can claim any author email, a signer's noreply address
// included. Its comment @-mentions the opener alone: it writes other accounts' logins without the @, and an author
// whose email is linked to no account by the commit's sha. It reads the event and the API alone, never the pull
// request's code. It fails closed: a base other than main, an answer it cannot read, or a commit list GitHub still
// gives short of the pull request's count after a few tries ends it red.
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
	"slices"
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
	base      = "main"   // the one branch the main ruleset requires the check on
	actionsID = 41898282 // github-actions[bot], the only account whose comment the check edits
	perPage   = 100
	named     = 20 // the comment lists this many who have yet to sign, and counts the rest
	tries     = 4  // reads of a pull request GitHub has not caught up with yet
)

// lag is how long the check waits before reading a pull request GitHub had not caught up with again.
var lag = 5 * time.Second

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

// author is who must sign: the pull request's opener, or who wrote a commit, an account GitHub linked its email
// to or, with no account, the commit alone.
type author struct {
	account *user
	commit  string // the first commit by an unlinked email
	email   string // never printed: a commit claims any it likes
}

var (
	errUnsigned = errors.New("not every author has signed the CLA")
	errBehind   = errors.New("GitHub has not caught up with the pull request")
)

type pull struct {
	Commits int  `json:"commits"`
	User    user `json:"user"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// check runs the cla check for one event: a pull_request_target event, or a recheck or signing comment.
func check(gh *github, name string, raw []byte) error {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("the event: %w", err)
	}
	var number int
	head := "" // the commit a pull_request_target run's check is on; a comment's run checks the pull request's head
	switch {
	case name == "pull_request_target" && ev.PullRequest != nil:
		number, head = ev.PullRequest.Number, ev.PullRequest.Head.SHA
	case name == "issue_comment" && ev.Issue != nil && ev.Issue.PullRequest != nil && ev.Comment != nil:
		number = ev.Issue.Number
	default:
		return fmt.Errorf("a %q event is not one the check reads", name)
	}
	var (
		pr      pull
		authors []author
		err     error
	)
	for try := 1; ; try++ {
		pr, authors, err = read(gh, number, head)
		if !errors.Is(err, errBehind) || try == tries {
			break
		}
		log.Printf("%v; reading it again in %s", err, lag)
		time.Sleep(lag)
	}
	if err != nil {
		return err
	}
	if pr.Base.Ref != base {
		return fmt.Errorf("pull request #%d is into %q: the check passes a pull request into %s alone", number, pr.Base.Ref, base)
	}
	if ev.Comment != nil && !slices.ContainsFunc(authors, func(a author) bool { return a.account != nil && a.account.ID == ev.Comment.User.ID }) {
		log.Printf("#%d: comment %d is by %s, who neither opened the pull request nor wrote a commit of it: nothing to do", number, ev.Comment.ID, ev.Comment.User.Login)
		return nil
	}
	signed, _, _, err := gh.signatures()
	if err != nil {
		return err
	}
	missing := unsigned(authors, signed)
	if ev.Comment != nil && strings.TrimSpace(ev.Comment.Body) == sentence {
		if i := slices.IndexFunc(missing, func(a author) bool { return a.account != nil && a.account.ID == ev.Comment.User.ID }); i >= 0 {
			if signed, err = gh.sign(signature{
				Name: missing[i].account.Login, ID: missing[i].account.ID, CommentID: ev.Comment.ID, CreatedAt: ev.Comment.CreatedAt,
				RepoID: ev.Repository.ID, PullRequestNo: number,
			}); err != nil {
				return err
			}
			missing = unsigned(authors, signed)
		}
	}
	if err := gh.comment(number, body(missing, pr.User.ID), len(missing) == 0); err != nil {
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
	log.Printf("#%d's opener and every author of its %d commits have signed the CLA", number, pr.Commits)
	return nil
}

// read reads the pull request and who must sign it: its opener first, then every page of its commits' authors.
// It is errBehind while GitHub's answers disagree: the pull request's head is not the run's, or its commits are
// not its whole count holding its head. GitHub lists 250 commits at most, so a longer pull request never agrees.
func read(gh *github, number int, head string) (pull, []author, error) {
	var pr pull
	if err := gh.get(fmt.Sprintf("pulls/%d", number), &pr); err != nil {
		return pr, nil, err
	}
	if head != "" && pr.Head.SHA != head {
		return pr, nil, fmt.Errorf("%w: pull request #%d's head is %s, not this run's %s", errBehind, number, pr.Head.SHA, head)
	}
	if pr.User.ID == 0 || pr.User.Login == "" {
		return pr, nil, fmt.Errorf("pull request #%d: an opener with no id or login", number)
	}
	opener := pr.User
	authors := []author{{account: &opener}}
	seen := map[string]bool{"id:" + strconv.FormatInt(opener.ID, 10): true}
	listed, holds := 0, false
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
			return pr, nil, err
		}
		for _, c := range commits {
			listed, holds = listed+1, holds || c.SHA == pr.Head.SHA
			a := author{account: c.Author, commit: c.SHA, email: c.Commit.Author.Email}
			key := "email:" + a.email
			if a.account != nil {
				if a.account.ID == 0 || a.account.Login == "" {
					return pr, nil, fmt.Errorf("commit %s: an author account with no id or login", c.SHA)
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
	if listed != pr.Commits || !holds {
		return pr, nil, fmt.Errorf("%w: pull request #%d has %d commits with head %s, and GitHub listed %d (head listed: %t): the check reads them all or fails", errBehind, number, pr.Commits, pr.Head.SHA, listed, holds)
	}
	return pr, authors, nil
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

// body is the check's comment: who has yet to sign, and how. It @-mentions the opener alone: a commit's author
// account is the commit's to claim, so the comment writes its login without the @, and an author whose email is
// linked to no account by the commit's sha. It lists the first few and counts the rest.
func body(missing []author, opener int64) string {
	if len(missing) == 0 {
		return marker + "\nThis pull request's opener and every author of its commits have signed the [contributor licence agreement](" + document + ")."
	}
	var b strings.Builder
	b.WriteString(marker + "\nBefore this pull request can merge, the account that opened it and each author of its commits sign the [contributor licence agreement](" + document + "), once. To sign, comment on this pull request with this sentence:\n\n")
	b.WriteString("```\n" + sentence + "\n```\n\nNot signed yet:\n\n")
	for i, a := range missing {
		switch {
		case i == named:
			fmt.Fprintf(&b, "- and %d more\n", len(missing)-named)
		case i > named:
		case a.account != nil && a.account.ID == opener:
			b.WriteString("- @" + a.account.Login + ", who opened this pull request\n")
		case a.account != nil:
			b.WriteString("- `" + a.account.Login + "`\n")
		default:
			b.WriteString("- the author of commit " + a.commit[:min(12, len(a.commit))] + ", whose email is linked to no GitHub account: add the email to your account, then sign\n")
		}
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

// signatures reads cla.json and its blob sha, and whether the branch is there. A file or a branch not there yet
// is no signatures; any other failure is an error.
func (g *github) signatures() (signatures, string, bool, error) {
	var f struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	var s signatures
	status, err := g.call(http.MethodGet, "contents/"+file+"?ref="+branch, nil, &f)
	if status == http.StatusNotFound {
		status, err := g.call(http.MethodGet, "git/ref/heads/"+branch, nil, nil)
		if status == http.StatusNotFound {
			return s, "", false, nil
		}
		return s, "", true, err
	}
	if err != nil {
		return s, "", true, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return s, "", true, fmt.Errorf("%s: %w", file, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, "", true, fmt.Errorf("%s: %w", file, err)
	}
	return s, f.SHA, true, nil
}

// sign adds one signature to cla.json and returns the file it wrote. It writes over the blob it read, so a
// signature another run wrote in between makes GitHub refuse the write (409, or 422 for a branch made in between):
// it reads the file again and writes once more, a few times, then fails rather than drop one.
func (g *github) sign(sig signature) (signatures, error) {
	for try := 1; ; try++ {
		s, sha, exists, err := g.signatures()
		if err != nil {
			return s, err
		}
		if slices.ContainsFunc(s.SignedContributors, func(x signature) bool { return x.ID == sig.ID }) {
			return s, nil
		}
		s.SignedContributors = append(s.SignedContributors, sig)
		status, err := g.write(s, sha, exists, fmt.Sprintf("%s has signed the CLA in #%d", sig.Name, sig.PullRequestNo))
		if err == nil {
			return s, nil
		}
		if status != http.StatusConflict && status != http.StatusUnprocessableEntity || try == tries {
			return s, err
		}
		log.Printf("%v; reading %s again in %s", err, file, lag)
		time.Sleep(lag)
	}
}

// write writes cla.json over the blob sha it read. With no branch yet, it makes the branch: a commit with no
// parent, holding cla.json alone.
func (g *github) write(s signatures, sha string, exists bool, message string) (int, error) {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return 0, err
	}
	raw = append(raw, '\n')
	if !exists {
		var tree, commit struct {
			SHA string `json:"sha"`
		}
		entry := map[string]string{"path": file, "mode": "100644", "type": "blob", "content": string(raw)}
		if status, err := g.call(http.MethodPost, "git/trees", map[string]any{"tree": []any{entry}}, &tree); err != nil {
			return status, err
		}
		if status, err := g.call(http.MethodPost, "git/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{}}, &commit); err != nil {
			return status, err
		}
		return g.call(http.MethodPost, "git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil)
	}
	put := map[string]string{"message": message, "branch": branch, "content": base64.StdEncoding.EncodeToString(raw)}
	if sha != "" {
		put["sha"] = sha
	}
	return g.call(http.MethodPut, "contents/"+file, put, nil)
}

// comment writes the check's comment: a new one while someone has yet to sign, and an edit of its own, by
// github-actions[bot], when what it says changed.
func (g *github) comment(number int, text string, done bool) error {
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
	if done {
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
