package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		Jobs map[string]struct {
			If    string `yaml:"if"`
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
	signed    *signatures // nil: no cla.json yet
	broken    bool        // cla.json's read answers 500
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
	f.commits[len(f.commits)-1].SHA = head
	f.count = len(f.commits)
	mux := http.NewServeMux()
	page := func(r *http.Request) (int, int) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		return max(p, 1), n
	}
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"commits": %d, "head": {"sha": %q}}`, f.count, head)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/commits", func(w http.ResponseWriter, r *http.Request) {
		p, n := page(r)
		n = min(n, 100)
		lo, hi := min((p-1)*n, len(f.commits)), min(p*n, len(f.commits), 250)
		_ = json.NewEncoder(w).Encode(f.commits[min(lo, hi):hi])
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
	f.patched = map[int64]string{}
	return &github{api: srv.URL, repo: "o/r", token: "t", http: srv.Client()}
}

var alice, bob, carol = &user{Login: "alice", ID: 1}, &user{Login: "bob", ID: 2}, &user{Login: "carol", ID: 3}

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
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], "- @bob\n") || strings.Contains(f.posted[0], "@alice") {
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
	if len(f.posted) != 0 || len(f.patched) != 1 || !strings.Contains(f.patched[11], "has signed") {
		t.Errorf("the check's own comment should say everyone has signed: posted %q, patched %v", f.posted, f.patched)
	}
	if f.reruns != 1 {
		t.Errorf("the pull request's failed check ran again %d times, want 1", f.reruns)
	}
}

func TestASigningCommentSignsOnlyItsOwnAuthor(t *testing.T) {
	f := &fakeGitHub{signed: signedBy(alice), runStatus: "completed"}
	f.add(1, bob, "bob", "b@example.com")
	err := check(f.serve(t), "issue_comment", commented(carol, sentence))
	if !errors.Is(err, errUnsigned) || len(f.written) != 0 {
		t.Fatalf("carol, no author, signs for nobody: got %v, wrote %+v", err, f.written)
	}
}

func TestTheFirstSignatureCreatesTheFile(t *testing.T) {
	f := &fakeGitHub{runStatus: "in_progress"}
	f.add(1, bob, "bob", "b@example.com")
	if err := check(f.serve(t), "issue_comment", commented(bob, sentence)); err != nil {
		t.Fatal(err)
	}
	if len(f.written) != 1 || f.writeSHA[0] != "" || f.reruns != 0 {
		t.Errorf("wrote %+v over %q, reran %d: want a new file, and a run still going left to end", f.written, f.writeSHA, f.reruns)
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
