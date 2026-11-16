package githubapp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// A kept repository's name is the app's, cleaned, and four random letters or digits: never empty, never a dash at either end.
func TestAKeptRepositorysNameIsTheAppsCleanedPlusFourRandom(t *testing.T) {
	for name, want := range map[string]string{"Claims App!!": "claims-app", "  ---  ": "app", "": "app", "Ünïcode ✓ name": "n-code-name", strings.Repeat("ab-", 30): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab"} {
		if got := RepoName(name); !regexp.MustCompile(`^` + regexp.QuoteMeta(want) + `-[a-z0-9]{4}$`).MatchString(got) {
			t.Errorf("%q: %q, want %s-xxxx", name, got, want)
		}
	}
	if a, b, c := RepoName("x"), RepoName("x"), RepoName("x"); a == b && b == c {
		t.Error("three names in a row the same: the suffix isn't random")
	}
}

// kept is installed with acme keeping code, claims-portal's push reviewed clean and its link gone, so the app has
// a version and no repository; GitHub makes repository 555 at a bare repository on disk, refusing the first name
// as taken, and running whileMaking as it makes it; everything else is hooked's GitHub. It answers the remote,
// the bodies GitHub was asked to make repositories with and the repositories it was asked to delete, and the check runs.
func kept(t *testing.T) (remote string, made, calls chan string) {
	t.Helper()
	calls = installed(t)
	pushed(t, calls, "d-version")
	for _, q := range []string{"DELETE FROM project_repositories", "UPDATE github_app SET keeps_code = true"} {
		if _, err := courier.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	remote = t.TempDir() + "/kept.git"
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil { //nolint:gosec // test: a temp dir
		t.Fatalf("%v %s", err, out)
	}
	made = make(chan string, 4)
	hooked, err := url.Parse(courier.API)
	if err != nil {
		t.Fatal(err)
	}
	fake := http.NewServeMux()
	fake.Handle("/", httputil.NewSingleHostReverseProxy(hooked))
	fake.HandleFunc("POST /orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if makeFails > 0 {
			makeFails--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if made <- string(body); len(made) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if whileMaking != nil {
			whileMaking()
		}
		_, _ = io.WriteString(w, `{"id":555,"full_name":"acme/claims-portal-k33p"}`)
	})
	fake.HandleFunc("DELETE /repos/acme/{repo}", func(w http.ResponseWriter, r *http.Request) {
		made <- "DELETE " + r.PathValue("repo")
		w.WriteHeader(http.StatusNoContent)
	})
	fake.HandleFunc("GET /repositories/555", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"clone_url":"file://%s"}`, remote)
	})
	fake.HandleFunc("GET /users/canyon-acme[bot]", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":77}`) })
	srv := httptest.NewServer(fake)
	api := courier.API
	courier.API, courier.Public, cloneScheme = srv.URL, "https://canyon.example", "file://"
	t.Cleanup(func() { courier.API, cloneScheme, whileMaking = api, "https://", nil; srv.Close() })
	return remote, made, calls
}

// whileMaking is what happens in Canyon while GitHub makes a kept repository; makeFails is how many
// times GitHub answers 502 first.
var (
	whileMaking func()
	makeFails   int
)

// GitHub failing to make the repository is said on the app and tried again, so the version that asked
// gets its repository; GitHub's own no waits for the next version.
func TestARepositoryGitHubFailedToMakeIsTriedAgain(t *testing.T) {
	kept(t)
	makeFails = 1
	t.Cleanup(func() { makeFails = 0 })
	k := Keep{Project: "claims-portal", Display: "Dana"}
	if err := courier.keptRepository(ctx, k); err == nil {
		t.Fatal("a 502 was no error: the step would not be tried again")
	}
	var why string
	if err := courier.Pool.QueryRow(ctx, "SELECT keep_error FROM projects WHERE id = 'claims-portal'").Scan(&why); err != nil || !strings.HasPrefix(why, "GitHub refused making the repository: ") {
		t.Fatalf("keep_error after a 502: %q %v", why, err)
	}
	if err := courier.keptRepository(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := courier.Pool.QueryRow(ctx, "SELECT p.keep_error || '|' || l.repo_id FROM projects p JOIN project_repositories l ON l.project_id = p.id").Scan(&why); err != nil || why != "|555" {
		t.Fatalf("after the retry: %q %v", why, err)
	}
}

// An app an admin linked while GitHub made its kept repository keeps that link, and the made repository is
// deleted again; an app deleted meanwhile, the same; never a repository left behind with no app.
func TestARepositoryMadeForAnAppLinkedMeanwhileIsDeletedAgain(t *testing.T) {
	_, made, _ := kept(t)
	whileMaking = func() {
		if _, err := courier.Pool.Exec(ctx, "INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('claims-portal', 999, '', 42, 'acme/platform')"); err != nil {
			t.Error(err)
		}
	}
	if err := courier.keptRepository(ctx, Keep{Project: "claims-portal", Display: "Dana"}); err != nil {
		t.Fatal(err)
	}
	if asked := []string{<-made, <-made, <-made}; asked[2] != "DELETE claims-portal-k33p" {
		t.Fatalf("GitHub was asked: %q", asked)
	}
	var link string
	if err := courier.Pool.QueryRow(ctx, "SELECT repo_id || ' ' || keeps || ' ' || (SELECT count(*) FROM audit_events WHERE action = 'create kept repository') FROM project_repositories").Scan(&link); err != nil || link != "999 false 0" {
		t.Fatalf("the admin's link and no kept repository: %q %v", link, err)
	}
}

// git runs git on dir and answers its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)                                                                                     //nolint:gosec // test: the test's own repositories
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=dlee", "GIT_AUTHOR_EMAIL=dlee@acme.example", "GIT_COMMITTER_NAME=dlee", "GIT_COMMITTER_EMAIL=dlee@acme.example") // a developer's push, on a runner with no git identity
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The first version made current while an org keeps code makes the app's private repository, a taken name
// picked again, linked at its root with keeps; once its review passed it is committed as the App with the
// contract's message and the uploader as co-author, and kept_commit moves to it.
func TestAVersionAReviewPassedIsCommittedToTheRepositoryCanyonMade(t *testing.T) {
	remote, made, _ := kept(t)
	var pkg string
	if err := courier.Pool.QueryRow(ctx, "SELECT id FROM packages WHERE project_id = 'claims-portal'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	k := Keep{Project: "claims-portal", Package: pkg, Display: "Dana", Email: "dana@acme.example"}
	if err := courier.keptRepository(ctx, k); err != nil {
		t.Fatal(err)
	}
	first, second := <-made, <-made
	if !strings.Contains(first, `"private":true`) || !strings.HasPrefix(first, `{"description":"Kept by Canyon: claims-portal","name":"claims-portal-`) || first == second {
		t.Fatalf("the names asked for: %s then %s", first, second)
	}
	var link string
	if err := courier.Pool.QueryRow(ctx, `SELECT l.repo_id || ' ' || l.full_name || ' ' || l.folder || '|' || l.keeps || ' ' || a.actor_display || ' ' || a.detail::text
		FROM project_repositories l, audit_events a WHERE a.action = 'create kept repository' AND a.project_id = l.project_id`).Scan(&link); err != nil {
		t.Fatal(err)
	}
	if link != `555 acme/claims-portal-k33p |true Canyon {"repo_id": 555, "full_name": "acme/claims-portal-k33p"}` {
		t.Fatalf("the link and its audit row: %s", link)
	}
	run, err := courier.passedRun(ctx, k)
	if err != nil || run == "" {
		t.Fatalf("the version's clean run: %q %v", run, err)
	}
	if _, err := courier.Pool.Exec(ctx, "UPDATE runs SET ended_at = '2026-01-02T05:04:05+02:00' WHERE id = $1", run); err != nil { // the commit's date, whatever the zone
		t.Fatal(err)
	}
	if _, err := courier.Pool.Exec(ctx, "INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_newer', 'claims-portal', 't', 'c', 1, 'upload')"); err != nil {
		t.Fatal(err)
	}
	if commit, err := courier.commitKept(ctx, k, run); err != nil || commit != "" || git(t, remote, "for-each-ref") != "" {
		t.Fatalf("a version no longer current was committed: %q %v", commit, err)
	}
	if _, err := courier.Pool.Exec(ctx, "DELETE FROM packages WHERE id = 'pkg_newer'"); err != nil {
		t.Fatal(err)
	}
	commit, err := courier.commitKept(ctx, k, run)
	if err != nil || commit == "" || git(t, remote, "rev-parse", "main") != commit {
		t.Fatalf("the commit %q is main: %v", commit, err)
	}
	if got := git(t, remote, "log", "-1", "--format=%an <%ae>|%P|%B", "main"); got != "canyon-acme[bot] <77+canyon-acme[bot]@users.noreply.github.com>||"+strings.TrimPrefix(pkg, "pkg_")+" · claims-portal\n\nUploaded by Dana.\nCanyon's review passed: 0 blocking, 0 advisory.\n\nCanyon-Version: "+pkg+"\nCanyon-Review: https://canyon.example/#/apps/claims-portal\nCo-authored-by: Dana <dana@acme.example>" {
		t.Fatalf("the kept commit: %s", got)
	}
	var state string
	if err := courier.Pool.QueryRow(ctx, "SELECT l.kept_commit || '|' || l.kept_error || '|' || (SELECT count(*) FROM audit_events WHERE action = 'keep version in github') FROM project_repositories l").Scan(&state); err != nil || state != commit+"||1" {
		t.Fatalf("kept_commit, kept_error, audit rows: %s %v", state, err)
	}
	// a retry after the push landed and before it was recorded knows its own commit, never someone's push
	if _, err := courier.Pool.Exec(ctx, "UPDATE project_repositories SET kept_commit = ''"); err != nil {
		t.Fatal(err)
	}
	// raw, not iso-strict: git before 2.45 prints UTC as +00:00, from 2.45 as Z (Ubuntu 24.04 ships 2.43)
	if at := git(t, remote, "log", "-1", "--format=%at %ct %ad", "--date=raw", "main"); at != "1767323045 1767323045 1767323045 +0000" {
		t.Fatalf("the kept commit is dated by its review's end, in UTC: %s", at)
	}
	if again, err := courier.commitKept(ctx, k, run); err != nil || again != commit || git(t, remote, "rev-parse", "main") != commit {
		t.Fatalf("the retry: %q %v, main %s", again, err, git(t, remote, "rev-parse", "main"))
	}
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_commit || '|' || kept_error FROM project_repositories").Scan(&state); err != nil || state != commit+"|" {
		t.Fatalf("after the retry: %q %v", state, err)
	}
}

// A version made before someone pushed to main is never pushed over it: kept_error says so and main is theirs;
// the next version, made on top of their push, goes in as a fast-forward.
func TestAVersionMadeBeforeSomeonesPushIsNeverPushedOverIt(t *testing.T) {
	remote, _, _ := kept(t)
	var pkg string
	if err := courier.Pool.QueryRow(ctx, "SELECT id FROM packages WHERE project_id = 'claims-portal'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	k := Keep{Project: "claims-portal", Package: pkg, Display: "Dana\nCo-authored-by: Eve <eve@acme.example>"} // a name is one line of the message, never a trailer
	if err := courier.keptRepository(ctx, k); err != nil {
		t.Fatal(err)
	}
	run, err := courier.passedRun(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := courier.commitKept(ctx, k, run)
	if err != nil {
		t.Fatal(err)
	}
	if body := git(t, remote, "log", "-1", "--format=%B", ours); !strings.Contains(body, "\nUploaded by Dana Co-authored-by: Eve <eve@acme.example>.\n") || strings.Contains(body, "\nCo-authored-by:") {
		t.Fatalf("a name with a line break in it:\n%s", body)
	}
	theirs := git(t, remote, "commit-tree", git(t, remote, "rev-parse", "main^{tree}"), "-p", ours, "-m", "a developer's push")
	git(t, remote, "update-ref", "refs/heads/main", theirs)
	if _, err := courier.Pool.Exec(ctx, "UPDATE packages SET kept_base = $1", ours); err != nil { // made when main was at ours
		t.Fatal(err)
	}
	if again, err := courier.commitKept(ctx, k, run); err != nil || again != "" || git(t, remote, "rev-parse", "main") != theirs {
		t.Fatalf("pushed over a developer's push: %q %v", again, err)
	}
	var why string
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_error FROM project_repositories").Scan(&why); err != nil || why != strings.TrimPrefix(pkg, "pkg_")+" is deployed but not in GitHub: someone pushed to main after it was made, and Canyon never overwrites a push. Run canyon pull, then canyon deploy." {
		t.Fatalf("kept_error: %q %v", why, err)
	}
	if _, err := courier.Pool.Exec(ctx, "UPDATE packages SET kept_base = $1", theirs); err != nil {
		t.Fatal(err)
	}
	next, err := courier.commitKept(ctx, k, run)
	if err != nil || git(t, remote, "rev-parse", "main") != next || git(t, remote, "rev-parse", "main^") != theirs {
		t.Fatalf("the next version on top of their push: %q %v", next, err)
	}
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_error FROM project_repositories").Scan(&why); err != nil || why != "" {
		t.Fatalf("kept_error once one goes in: %q %v", why, err)
	}
	// a rule on main: GitHub's own words, and what to do about it
	if err := os.WriteFile(remote+"/hooks/pre-receive", []byte("#!/bin/sh\necho 'error: GH006: Protected branch update failed for refs/heads/main.' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test: the remote's hook
		t.Fatal(err)
	}
	if _, err := courier.Pool.Exec(ctx, "UPDATE packages SET kept_base = $1", next); err != nil {
		t.Fatal(err)
	}
	if _, err := courier.commitKept(ctx, k, run); err == nil { // said, and tried again by the workflow
		t.Fatal("a refused push was no error")
	}
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_error FROM project_repositories").Scan(&why); err != nil || !strings.HasPrefix(why, "GitHub refused "+strings.TrimPrefix(pkg, "pkg_")+": remote: error: GH006: Protected branch update failed for refs/heads/main.") || !strings.HasSuffix(why, " Add Canyon's App to the rule's bypass list on GitHub, or hand this repository to developers.") || strings.Contains(why, "fake-token") {
		t.Fatalf("a push a rule refused: %q %v", why, err)
	}
}

// A push GitHub delivers for a kept repository moves kept_commit to its head; Canyon's own push of a kept
// version starts no check, anyone else's does.
func TestAPushToAKeptRepositoryMovesKeptCommitAndCanyonsOwnStartsNoCheck(t *testing.T) {
	_, _, calls := kept(t)
	if err := courier.keptRepository(ctx, Keep{Project: "claims-portal", Display: "Dana"}); err != nil {
		t.Fatal(err)
	}
	own := deliver(t, "push", "d-own", "push_default_branch.json", `"id":81723`, `"id":555`, `"name":"billing-api"`, `"name":"claims-portal"`, `"login":"dana-acme","type":"User"`, `"login":"canyon-acme[bot]","type":"Bot"`)
	if own.Code != 202 {
		t.Fatalf("Canyon's own push: %d %s", own.Code, own.Body)
	}
	if _, err := dbos.RetrieveWorkflow[string](durable, run+"d-own/claims-portal"); err == nil {
		t.Fatal("Canyon's own push started a check")
	}
	if _, err := dbos.RetrieveWorkflow[string](durable, run+"d-own"); err == nil {
		t.Fatal("Canyon's own push started the neutral check")
	}
	var head string
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_commit FROM project_repositories").Scan(&head); err != nil || head != "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334" {
		t.Fatalf("kept_commit after Canyon's push: %q %v", head, err)
	}
	if rec := deliver(t, "push", "d-dev", "push_default_branch.json", `"id":81723`, `"id":555`, `"name":"billing-api"`, `"name":"claims-portal"`, "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", "8888888888888888888888888888888888888888"); rec.Code != 202 {
		t.Fatalf("a developer's push: %d %s", rec.Code, rec.Body)
	}
	if patch := ended(t, calls, "d-dev"); len(patch) == 0 {
		t.Fatal("a developer's push to a kept repository is checked")
	}
	if err := courier.Pool.QueryRow(ctx, "SELECT kept_commit FROM project_repositories").Scan(&head); err != nil || head != "8888888888888888888888888888888888888888" {
		t.Fatalf("kept_commit after a developer's push: %q %v", head, err)
	}
}

// An upload records kept_commit as the version's base and starts the keep, only while an org keeps code and
// no repository a person pushes to feeds the app.
func TestAnUploadRecordsItsBaseOnlyWhereCanyonKeepsTheCode(t *testing.T) {
	kept(t)
	var pkg string
	if err := courier.Pool.QueryRow(ctx, "SELECT id FROM packages WHERE project_id = 'claims-portal'").Scan(&pkg); err != nil {
		t.Fatal(err)
	}
	base := func(setup string) string {
		t.Helper()
		tx, err := courier.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // and the keep it enqueued
		if _, err := tx.Exec(ctx, setup); err != nil {
			t.Fatal(err)
		}
		if err := QueueKeep(ctx, tx, durable, Keep{Project: "claims-portal", Package: pkg, Display: "Dana"}); err != nil {
			t.Fatal(err)
		}
		var b string
		var queued bool
		if err := tx.QueryRow(ctx, "SELECT kept_base, EXISTS (SELECT 1 FROM "+fmt.Sprintf("dbos_github_%d", os.Getpid())+".workflow_status WHERE name = $2) FROM packages WHERE id = $1", pkg, KeepWorkflow).Scan(&b, &queued); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%q %t", b, queued)
	}
	link := "INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name, keeps, kept_commit) VALUES ('claims-portal', 555, '', 42, 'acme/claims-portal-k33p', %t, 'cafe')"
	for setup, want := range map[string]string{
		fmt.Sprintf(link, true):                    `"cafe" true`,
		fmt.Sprintf(link, false):                   `"" false`, // a repository people push to: GitHub's already
		"UPDATE github_app SET keeps_code = false": `"" false`, // nobody keeps code
		"SELECT 1": `"" true`, // no repository yet: the first commit has no parent
	} {
		if got := base(setup); got != want {
			t.Errorf("%s: kept_base %q, want %q", setup, got, want)
		}
	}
}
