package cli

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// committed makes dir a git working tree with one commit, and answers it.
func committed(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "push"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test: a temp dir
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // test: a temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// pushedStale is stale's 409 with the head a push brought: commit, delivered by GitHub.
func pushedStale(t *testing.T, commit string) *gateHost {
	t.Helper()
	h := stale(t)
	h.upload.body = strings.Replace(h.upload.body, `"moved_at"`, `"moved_from":{"kind":"repo","ref":"`+commit+`"},"moved_at"`, 1)
	return h
}

// A copy that holds the pushed commit was built on it: the deploy goes on with the head as its base,
// after one line; one that lacks it is told git pull when a repository feeds the app, and gets the
// block it always got otherwise — no git, or an app no repository feeds (cli.md §3.6, #1938).
func TestADeployOnAPushItHoldsGoesOn(t *testing.T) {
	dir, want := projectDir(t), golden(t, "stale-base-deploy.txt") // read before the verb leaves this directory
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	commit := committed(t, dir)
	h := pushedStale(t, commit)
	h.built, h.page = "pkg_9f2a1c", buildPage(t, buildLines, true)
	out, errOut, code := verbingIn(t, h, dir, "deploy")
	if code != exitOK || errOut != "" || h.base != "pkg_9f2a1c" || baseIn(t, dir, "claims-portal") != "pkg_1" ||
		!strings.Contains(out, "\n  ! main moved: ada@meridian.example pushed "+commit[:7]+" 14m ago.\n    Your working tree already has that commit; deploying on top of it.\n") {
		t.Fatalf("on the push: exit %d, base sent %q, stderr %q, stdout\n%s", code, h.base, errOut, out)
	}

	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	h = pushedStale(t, commit)
	h.built, h.page = "pkg_9f2a1c", buildPage(t, buildLines, true)
	if out, _, code := verbingIn(t, h, dir, "deploy", "--format", "ai"); code != exitOK || !strings.Contains(out, "\nmain moved: ada@meridian.example pushed "+commit[:7]+" 14m ago. Your working tree already has that commit; deploying on top of it.\n") {
		t.Errorf("under ai the block says main moved: exit %d, stdout\n%s", code, out)
	}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	elsewhere := strings.Repeat("1", 40) // a commit this working tree never had
	fed := pushedStale(t, elsewhere)
	fed.read = reply{http.StatusOK, `{"project":{"id":"claims-portal","repository":{"repo_id":555,"full_name":"acme/claims-portal","folder":"","state":"linked","keeps":false,"kept_commit":null,"kept_error":""}}}`}
	_, errOut, code = verbingIn(t, fed, dir, "deploy")
	if code != exitError || errOut != "\n  ✗ ada@meridian.example pushed 1111111 to main 14m ago, and this working tree doesn't have it.\n\n    Take it:             git pull, then canyon deploy\n    Send yours anyway:   canyon deploy --force\n" {
		t.Errorf("a push this tree lacks, on an app a repository feeds: exit %d, stderr\n%s", code, errOut)
	}

	for _, read := range []reply{{}, {http.StatusOK, strings.Replace(fed.read.body, `"keeps":false`, `"keeps":true`, 1)}} { // none, and one Canyon keeps
		kept := pushedStale(t, elsewhere)
		kept.read = read
		if _, errOut, code := verbingIn(t, kept, dir, "deploy"); code != exitError || errOut != want {
			t.Errorf("an app no repository people push to feeds (%q): exit %d, stderr\n%s", read.body, code, errOut)
		}
	}
	_, errOut, _ = verbingIn(t, fed, dir, "deploy", "--format", "ai")
	if !strings.HasSuffix(errOut, "Send yours anyway:   canyon deploy --force\n\n    Do not run either. Show the person both lines and ask: one writes over the version ada@meridian.example deployed, the other over the person's own.\n") {
		t.Errorf("under ai the git block ends with the line every refusal does:\n%s", errOut)
	}
	plain := projectDir(t) // no git at all
	must(t, writeBase(plain, "claims-portal", "pkg_3b7e40"))
	if _, errOut, code := verbingIn(t, pushedStale(t, commit), plain, "deploy"); code != exitError || errOut != want {
		t.Errorf("a directory that isn't a working tree: exit %d, stderr\n%s", code, errOut)
	}
}
