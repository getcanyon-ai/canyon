package main

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTheCLACheckSkipsNobodyByAName(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/workflows/cla.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			PullRequestTarget struct {
				Types []string `yaml:"types"`
			} `yaml:"pull_request_target"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Concurrency struct {
				Group string `yaml:"group"`
			} `yaml:"concurrency"`
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["cla"]
	if !ok {
		t.Fatal("cla.yml has no cla job: the main ruleset requires a check named cla")
	}
	runs := false
	for _, s := range job.Steps {
		runs = runs || strings.TrimSpace(s.Run) == "go run ./internal/tools/claguard"
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			for _, in := range []string{"ref", "repository", "allow-unsafe-pr-checkout"} {
				if _, ok := s.With[in]; ok {
					t.Errorf("the checkout sets %s: the job checks out its own branch's code, never the pull request's", in)
				}
			}
		}
	}
	if !runs {
		t.Error("the cla job does not run go run ./internal/tools/claguard: the check these tests cover is not the one that runs")
	}
	for _, opener := range []string{"github.event.pull_request.user.login != 'dependabot[bot]'", "github.event.issue.user.login != 'dependabot[bot]'"} {
		if !strings.Contains(job.If, opener) {
			t.Errorf("the cla job's if lacks %s: Dependabot's pull requests are skipped by who opened them", opener)
		}
	}
	if !slices.Contains(wf.On.PullRequestTarget.Types, "edited") {
		t.Error("the check does not run on edited: a pull request opened into another branch, then moved to main, keeps that branch's verdict")
	}
	if !strings.Contains(job.Concurrency.Group, "format('cla-comment-{0}', github.event.issue.number)") {
		t.Errorf("a pull request's comment runs do not take turns: concurrency group %q", job.Concurrency.Group)
	}
	if strings.Contains(job.If, "github.actor") || strings.Contains(job.If, "author") {
		t.Errorf("the cla job's if names an actor or an author, which a pull request can change: %s", job.If)
	}
}

const head = "head0000000000000000000000000000000000000"

type fakeCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
	} `json:"commit"`
	Author *user `json:"author"`
}

type fakeComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User user   `json:"user"`
}

// fakeGitHub is the part of GitHub's API the check calls, for one pull request, #7, of o/r.
type fakeGitHub struct {
	count     int // the pull request's commits, as GitHub counts them
	commits   []fakeCommit
	opener    *user  // nil: alice
	base      string // "": main
	headAt    int    // the head's place in commits, from the end: 0 is the last
	stale     int    // reads of the pull request that still give the head before
	short     int    // reads of the commits that still miss the head
	shorting  bool
	signed    *signatures // nil: no cla.json yet
	noBranch  bool        // no cla-signatures branch yet
	broken    bool        // cla.json's read answers 500
	created   []string    // the branch made: its tree's paths, then its commit's parents
	comments  []fakeComment
	runStatus string
	written   []signatures // each cla.json written
	writeSHA  []string
	posted    []string // each new comment's body
	patched   map[int64]string
	reruns    int
}

func (f *fakeGitHub) serve(t *testing.T) *github {
	t.Helper()
	f.commits[len(f.commits)-1-f.headAt].SHA = head
	f.count = len(f.commits)
	f.opener = cmp.Or(f.opener, alice)
	f.base = cmp.Or(f.base, "main")
	mux := http.NewServeMux()
	page := func(r *http.Request) (int, int) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		return max(p, 1), n
	}
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		sha := head
		if f.stale > 0 {
			f.stale, sha = f.stale-1, "before"
		}
		raw, _ := json.Marshal(map[string]any{"commits": f.count, "head": map[string]string{"sha": sha}, "base": map[string]string{"ref": f.base}, "user": f.opener})
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/commits", func(w http.ResponseWriter, r *http.Request) {
		p, n := page(r)
		n = min(n, 100)
		if p == 1 {
			f.shorting = f.short > 0
			f.short = max(f.short-1, 0)
		}
		commits := f.commits
		if f.shorting {
			commits = slices.DeleteFunc(slices.Clone(commits), func(c fakeCommit) bool { return c.SHA == head })
		}
		lo, hi := min((p-1)*n, len(commits)), min(p*n, len(commits), 250)
		_ = json.NewEncoder(w).Encode(commits[min(lo, hi):hi])
	})
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/"+branch, func(w http.ResponseWriter, r *http.Request) {
		if f.noBranch {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("POST /repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BaseTree *string `json:"base_tree"`
			Tree     []struct {
				Path, Content string
			} `json:"tree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, e := range in.Tree {
			var s signatures
			if err := json.Unmarshal([]byte(e.Content), &s); err != nil {
				t.Errorf("%s written as %q", e.Path, e.Content)
			}
			f.written, f.writeSHA, f.created = append(f.written, s), append(f.writeSHA, ""), append(f.created, e.Path)
		}
		if in.BaseTree != nil {
			f.created = append(f.created, "base_tree "+*in.BaseTree)
		}
		_, _ = io.WriteString(w, `{"sha": "tree1"}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.created = append(f.created, fmt.Sprintf("commit of %s with parents %v", in.Tree, in.Parents))
		_, _ = io.WriteString(w, `{"sha": "commit1"}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.created = append(f.created, in["ref"]+" at "+in["sha"])
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("GET /repos/o/r/contents/"+file, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != branch {
			t.Errorf("cla.json read from %q", r.URL.Query().Get("ref"))
		}
		switch {
		case f.broken:
			http.Error(w, "boom", http.StatusInternalServerError)
		case f.signed == nil:
			http.NotFound(w, r)
		default:
			raw, _ := json.Marshal(f.signed)
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob1", "content": base64.StdEncoding.EncodeToString(raw)})
		}
	})
	mux.HandleFunc("PUT /repos/o/r/contents/"+file, func(w http.ResponseWriter, r *http.Request) {
		var put map[string]string
		_ = json.NewDecoder(r.Body).Decode(&put)
		raw, _ := base64.StdEncoding.DecodeString(put["content"])
		var s signatures
		if err := json.Unmarshal(raw, &s); err != nil || put["branch"] != branch {
			t.Errorf("cla.json written as %q to %q", raw, put["branch"])
		}
		f.written, f.writeSHA = append(f.written, s), append(f.writeSHA, put["sha"])
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		p, n := page(r)
		lo, hi := min((p-1)*n, len(f.comments)), min(p*n, len(f.comments))
		_ = json.NewEncoder(w).Encode(f.comments[lo:hi])
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		var c map[string]string
		_ = json.NewDecoder(r.Body).Decode(&c)
		f.posted = append(f.posted, c["body"])
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var c map[string]string
		_ = json.NewDecoder(r.Body).Decode(&c)
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.patched[id] = c["body"]
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("GET /repos/o/r/actions/workflows/cla.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("event") != "pull_request_target" || q.Get("head_sha") != head {
			t.Errorf("runs listed for %v", q)
		}
		_, _ = fmt.Fprintf(w, `{"workflow_runs": [{"id": 99, "status": %q, "conclusion": "failure"}]}`, f.runStatus)
	})
	mux.HandleFunc("POST /repos/o/r/actions/runs/99/rerun", func(w http.ResponseWriter, _ *http.Request) {
		f.reruns++
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	lag = 0
	f.patched = map[int64]string{}
	return &github{api: srv.URL, repo: "o/r", token: "t", http: srv.Client()}
}

var alice, bob, carol, mallory = &user{Login: "alice", ID: 1}, &user{Login: "bob", ID: 2}, &user{Login: "carol", ID: 3}, &user{Login: "mallory", ID: 4}

func (f *fakeGitHub) add(n int, by *user, name, email string) {
	for range n {
		c := fakeCommit{SHA: fmt.Sprintf("c%011d%028d", len(f.commits), 0), Author: by}
		c.Commit.Author.Name, c.Commit.Author.Email = name, email
		f.commits = append(f.commits, c)
	}
}

func signedBy(us ...*user) *signatures {
	s := &signatures{}
	for _, u := range us {
		s.SignedContributors = append(s.SignedContributors, signature{Name: u.Login, ID: u.ID})
	}
	return s
}

func opened() []byte {
	return []byte(`{"pull_request": {"number": 7, "head": {"sha": "` + head + `"}}}`)
}

func commented(by *user, body string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 7, "pull_request": map[string]any{}},
		"comment":    map[string]any{"id": 500, "body": body, "created_at": "2026-11-27T10:00:00Z", "user": by},
		"repository": map[string]any{"id": 42},
	})
	return raw
}

// #39: the action this replaced read one page of 100 commits, so an author after the hundredth was never checked.
func TestAnAuthorAfterTheHundredthCommitIsChecked(t *testing.T) {
	f := &fakeGitHub{signed: signedBy(alice)}
	f.add(100, alice, "alice", "a@example.com")
	f.add(1, bob, "bob", "b@example.com")
	err := check(f.serve(t), "pull_request_target", opened())
	if !errors.Is(err, errUnsigned) {
		t.Fatalf("101 commits, the last by bob, who has not signed: got %v, want the check red for bob", err)
	}
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], "- `bob`\n") || strings.Contains(f.posted[0], "alice") {
		t.Errorf("the comment should name bob alone: %q", f.posted)
	}
}

// GitHub lists 250 of a pull request's commits at most: the check fails rather than pass an author it never read.
func TestACommitListShortOfThePullRequestFails(t *testing.T) {
	f := &fakeGitHub{signed: signedBy(alice)}
	f.add(260, alice, "alice", "a@example.com")
	err := check(f.serve(t), "pull_request_target", opened())
	if err == nil || errors.Is(err, errUnsigned) || !strings.Contains(err.Error(), "listed 250") {
		t.Fatalf("260 commits of which GitHub lists 250: got %v, want the check failed for the list", err)
	}
}

// #39: the action wrote an unlinked author's claimed git name as an @-mention, so a commit could make the bot
// mention anyone.
func TestAnUnlinkedAuthorIsNamedByItsCommitNeverItsName(t *testing.T) {
	f := &fakeGitHub{signed: signedBy(alice)}
	f.add(1, alice, "alice", "a@example.com")
	f.add(2, nil, "@octocat [x](https://example.com)", "who@example.com")
	err := check(f.serve(t), "pull_request_target", opened())
	if !errors.Is(err, errUnsigned) {
		t.Fatalf("an author linked to no account cannot sign: got %v, want red", err)
	}
	if len(f.posted) != 1 {
		t.Fatalf("want one comment, got %q", f.posted)
	}
	c := f.posted[0]
	if strings.Contains(c, "octocat") || strings.Contains(c, "example.com") {
		t.Errorf("the comment repeats what the commit claims: %q", c)
	}
	if !strings.Contains(c, "the author of commit c00000000001, whose email is linked to no GitHub account") || strings.Count(c, "the author of commit") != 1 {
		t.Errorf("the comment should name the unlinked author once, by its first commit: %q", c)
	}
}

// A signature matches an account's id: a login is a name, and can be renamed and taken.
func TestASignatureMatchesTheAccountIDNotTheLogin(t *testing.T) {
	f := &fakeGitHub{signed: &signatures{SignedContributors: []signature{{Name: "bob", ID: 77}}}}
	f.add(1, bob, "bob", "b@example.com")
	if err := check(f.serve(t), "pull_request_target", opened()); !errors.Is(err, errUnsigned) {
		t.Fatalf("a signature by login bob with another account's id: got %v, want red", err)
	}
}

func TestSigningRecordsTheSignatureAndRunsTheCheckAgain(t *testing.T) {
	f := &fakeGitHub{signed: signedBy(alice), runStatus: "completed",
		comments: []fakeComment{{ID: 10, Body: marker + "\nforged", User: *bob}, {ID: 11, Body: marker + "\nold", User: user{Login: "github-actions[bot]", ID: actionsID}}}}
	f.add(1, alice, "alice", "a@example.com")
	f.add(1, bob, "bob", "b@example.com")
	if err := check(f.serve(t), "issue_comment", commented(bob, sentence)); err != nil {
		t.Fatalf("bob signs: %v", err)
	}
	want := signature{Name: "bob", ID: 2, CommentID: 500, CreatedAt: "2026-11-27T10:00:00Z", RepoID: 42, PullRequestNo: 7}
	if len(f.written) != 1 || len(f.written[0].SignedContributors) != 2 || f.written[0].SignedContributors[1] != want || f.writeSHA[0] != "blob1" {
		t.Errorf("cla.json written %+v over %q, want alice's signature then %+v, over blob1", f.written, f.writeSHA, want)
	}
	if len(f.posted) != 0 || len(f.patched) != 1 || !strings.Contains(f.patched[11], "have signed") {
		t.Errorf("the check's own comment should say everyone has signed: posted %q, patched %v", f.posted, f.patched)
	}
	if f.reruns != 1 {
		t.Errorf("the pull request's failed check ran again %d times, want 1", f.reruns)
	}
}

// A comment acts only from the pull request's opener or a commit's author: anyone else's recheck or signature runs
// nothing again and writes nothing.
func TestACommentFromNeitherOpenerNorAuthorDoesNothing(t *testing.T) {
	for _, text := range []string{sentence, "recheck"} {
		f := &fakeGitHub{signed: signedBy(alice), runStatus: "completed"}
		f.add(1, bob, "bob", "b@example.com")
		if err := check(f.serve(t), "issue_comment", commented(carol, text)); err != nil || len(f.written)+len(f.posted)+len(f.patched)+f.reruns != 0 {
			t.Errorf("carol comments %q: got %v, wrote %+v, posted %q, patched %v, reran %d: want nothing done", text, err, f.written, f.posted, f.patched, f.reruns)
		}
	}
}

func TestTheFirstSignatureCreatesTheFile(t *testing.T) {
	f := &fakeGitHub{opener: bob, runStatus: "in_progress"}
	f.add(1, bob, "bob", "b@example.com")
	if err := check(f.serve(t), "issue_comment", commented(bob, sentence)); err != nil {
		t.Fatal(err)
	}
	if len(f.written) != 1 || f.writeSHA[0] != "" || f.reruns != 0 || len(f.created) != 0 {
		t.Errorf("wrote %+v over %q, reran %d, made %q: want a new file on the branch, and a run still going left to end", f.written, f.writeSHA, f.reruns, f.created)
	}
}

// With no cla-signatures branch, the first signature makes it: a commit with no parent holding cla.json alone, so
// the branch holds no workflow of its own for a pull request into it to run.
func TestTheFirstSignatureMakesABranchHoldingItAlone(t *testing.T) {
	f := &fakeGitHub{opener: bob, noBranch: true, runStatus: "completed"}
	f.add(1, bob, "bob", "b@example.com")
	if err := check(f.serve(t), "issue_comment", commented(bob, sentence)); err != nil {
		t.Fatal(err)
	}
	want := []string{file, "commit of tree1 with parents []", "refs/heads/" + branch + " at commit1"}
	if !slices.Equal(f.created, want) || len(f.written) != 1 || f.written[0].SignedContributors[0].ID != bob.ID {
		t.Errorf("made %q holding %+v, want %q holding bob's signature", f.created, f.written, want)
	}
}

// The main ruleset requires the check on main alone: the check passes a pull request into main and nothing else,
// and checks one moved to main again (edited).
func TestAPullRequestIntoAnotherBranchIsRed(t *testing.T) {
	f := &fakeGitHub{base: "stale", signed: signedBy(alice)}
	f.add(1, alice, "alice", "a@example.com")
	if err := check(f.serve(t), "pull_request_target", opened()); err == nil || !strings.Contains(err.Error(), `is into "stale"`) {
		t.Errorf("a pull request into stale, every author signed: got %v, want red for its base", err)
	}
}

// A commit can claim any author email, a signer's noreply address included, so the account that opened the pull
// request signs too.
func TestTheOpenerSignsToo(t *testing.T) {
	f := &fakeGitHub{opener: mallory, signed: signedBy(alice)}
	f.add(3, alice, "alice", "1+alice@users.noreply.github.com")
	err := check(f.serve(t), "pull_request_target", opened())
	if !errors.Is(err, errUnsigned) {
		t.Fatalf("mallory, unsigned, opens commits claiming alice's email: got %v, want red", err)
	}
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], "- @mallory, who opened this pull request\n") {
		t.Errorf("the comment should name mallory: %q", f.posted)
	}
}

// Who a commit's author account is, the commit claims, so the comment @-mentions the opener alone, and lists 20.
func TestTheCommentMentionsTheOpenerAloneAndCountsPastTwenty(t *testing.T) {
	f := &fakeGitHub{opener: mallory}
	for i := range 25 {
		f.add(1, &user{Login: fmt.Sprintf("u%d", i+1), ID: int64(100 + i)}, "u", fmt.Sprintf("u%d@example.com", i))
	}
	if err := check(f.serve(t), "pull_request_target", opened()); !errors.Is(err, errUnsigned) {
		t.Fatalf("got %v, want red", err)
	}
	c := f.posted[0]
	if strings.Count(c, "@") != 1 || !strings.Contains(c, "- @mallory, who opened this pull request\n- `u1`\n") ||
		!strings.Contains(c, "- `u19`\n- and 6 more\n") || strings.Contains(c, "u20") {
		t.Errorf("want @mallory, then u1 to u19 without an @, then 6 more: %q", c)
	}
}

// GitHub can answer for a pull request's head before it lists its new commit: the check reads again, a few times,
// then fails.
func TestAPullRequestGitHubIsBehindOnIsReadAgain(t *testing.T) {
	for _, f := range []*fakeGitHub{{stale: tries - 1}, {short: tries - 1}, {headAt: 1}} {
		f.signed = signedBy(alice)
		f.add(3, alice, "alice", "a@example.com")
		if err := check(f.serve(t), "pull_request_target", opened()); err != nil {
			t.Errorf("stale %d, short %d, head %d from the end: %v", f.stale, f.short, f.headAt, err)
		}
	}
	for _, f := range []*fakeGitHub{{stale: tries}, {short: tries}} {
		f.signed = signedBy(alice)
		f.add(3, alice, "alice", "a@example.com")
		if err := check(f.serve(t), "pull_request_target", opened()); !errors.Is(err, errBehind) {
			t.Errorf("stale %d, short %d: got %v, want it failed for GitHub's answers", f.stale, f.short, err)
		}
	}
}

func TestWhatTheCheckCannotReadKeepsItRed(t *testing.T) {
	f := &fakeGitHub{broken: true}
	f.add(1, alice, "alice", "a@example.com")
	if err := check(f.serve(t), "pull_request_target", opened()); err == nil || errors.Is(err, errUnsigned) {
		t.Errorf("cla.json unreadable: got %v, want an error", err)
	}
	f = &fakeGitHub{signed: signedBy(alice)}
	f.add(1, alice, "alice", "a@example.com")
	gh := f.serve(t)
	moved := []byte(`{"pull_request": {"number": 7, "head": {"sha": "older"}}}`)
	if err := check(gh, "pull_request_target", moved); err == nil {
		t.Error("a run for a head the pull request moved on from passed")
	}
	if err := check(gh, "push", opened()); err == nil {
		t.Error("an event the check does not read passed")
	}
}
