//go:build stack

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	hookSecret   = "e2e-webhook-secret" //nolint:gosec // the stub App's fixed test value (e2e/stub), not a credential
	installation = 4242
	owner        = "e2e"
)

// checkRun is the part of a check run's POST or PATCH body the journey reads.
type checkRun struct {
	Conclusion string
	Output     struct {
		Summary     string
		Annotations []struct {
			Path            string
			AnnotationLevel string `json:"annotation_level"`
		}
	}
}

// j8GitHub registers the stub's App, installs it, and sends signed deliveries: a push is reviewed
// and reported as a check with an annotation per finding, which succeeds because J3 approved the
// one blocking finding on this app (#996), a redelivery starts nothing, a
// pull request from the repository is checked but never current, a fork's is ignored, and a
// repository no app is linked to gets a neutral check.
func j8GitHub(t *testing.T) {
	register(t)
	if code := deliver(t, "push", "unsigned", map[string]any{}, "sha256=00"); code != http.StatusUnauthorized {
		t.Errorf("an unsigned delivery: %d; want 401", code)
	}
	install := map[string]any{"action": "created", "installation": map[string]any{"id": installation, "account": map[string]any{"login": owner, "id": 1}}}
	if code := deliver(t, "installation", "install-"+tag, install, ""); code != http.StatusAccepted {
		t.Fatalf("installation created: %d; want 202", code)
	}

	link(t, cpApp) // a repository reaches the app linked to it, never one named like it (#1917)
	tarball(t, "f-cp", cpApp)
	sha := commit()
	mark := len(stubLog(t))
	if code := deliver(t, "push", "push-"+tag, push(cpApp, sha), ""); code != http.StatusAccepted {
		t.Fatalf("push: %d; want 202", code)
	}
	run := waitCheck(t, cpApp, mark)
	levels := map[string]string{}
	for _, a := range run.Output.Annotations {
		levels[a.Path] = a.AnnotationLevel
	}
	want := map[string]string{"apps/web/src/lib/payments.ts": "failure"} // no model on TestStack's stack: the route rules are the AI review's (#1394), which reads nothing here
	if run.Conclusion != "success" || !strings.Contains(run.Output.Summary, "every blocking finding approved") || !maps.Equal(levels, want) {
		t.Errorf("the push's check: %s, %q, with annotations %v; want success, every blocking finding approved, with %v", run.Conclusion, run.Output.Summary, levels, want)
	}
	seen := len(stubLog(t))
	if code := deliver(t, "push", "push-"+tag, push(cpApp, sha), ""); code != http.StatusAccepted {
		t.Errorf("a redelivery: %d; want 202", code)
	}

	mark = len(stubLog(t))
	pr := pullRequest(cpApp, owner+"/"+cpApp, commit())
	if code := deliver(t, "pull_request", "pr-"+tag, pr, ""); code != http.StatusAccepted {
		t.Fatalf("pull request: %d; want 202", code)
	}
	if run := waitCheck(t, cpApp, mark); run.Conclusion != "success" {
		t.Errorf("the pull request's check: %s; want success, J3's approval holding", run.Conclusion)
	}
	var list struct {
		Packages []struct{ Source struct{ Kind, Ref string } }
	}
	read(t, "/api/v1/projects/"+cpApp+"/packages?limit=1", &list)
	if len(list.Packages) != 1 || list.Packages[0].Source.Kind != "repo" || list.Packages[0].Source.Ref != sha {
		t.Errorf("the current package after the pull request: %+v; want the push's", list.Packages)
	}
	fork := pullRequest(cpApp, "someone/"+cpApp, commit())
	if code := deliver(t, "pull_request", "fork-"+tag, fork, ""); code != http.StatusNoContent {
		t.Errorf("a fork's pull request: %d; want 204", code)
	}
	nobody := "e2e-nobody-" + tag
	tarball(t, "f-clean", nobody)
	mark = len(stubLog(t))
	if code := deliver(t, "push", "nobody-"+tag, push(nobody, commit()), ""); code != http.StatusAccepted {
		t.Fatalf("a push for no project: %d; want 202", code)
	}
	if run := waitCheck(t, nobody, mark); run.Conclusion != "neutral" {
		t.Errorf("a push for no project: %s; want neutral", run.Conclusion)
	}
	// The redelivery started no check of its own: every check run since is the pull request's or the stray's.
	for _, r := range stubLog(t)[seen:] {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/"+cpApp+"/check-runs") && strings.Contains(r.Body, sha) {
			t.Errorf("the redelivery started a check for %s", sha)
		}
	}
}

// register runs the App's registration as an admin's browser would, GitHub's part the stub's. A
// stack that registered it on an earlier run answers 409.
func register(t *testing.T) {
	t.Helper()
	jar, _ := cookiejar.New(nil) // nil options never fail
	c := &http.Client{Jar: jar, Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(host + "/api/v1/github/app/new?org=" + owner + "&imports_new=false&keeps_code=false")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		return
	}
	state := regexp.MustCompile(`state=([0-9a-f]{64})`).FindSubmatch(body)
	if res.StatusCode != http.StatusOK || state == nil {
		t.Fatalf("app/new: %d %.300s", res.StatusCode, body)
	}
	res, err = c.Get(host + "/api/v1/github/app/callback?code=e2e&state=" + string(state[1]))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusFound || loc != "https://github.com/apps/canyon-e2e/installations/new" {
		t.Fatalf("app/callback: %d to %q; want 302 to the App's installation page", res.StatusCode, loc)
	}
}

// deliver posts one delivery as GitHub does, signed with the App's secret unless sig is given.
func deliver(t *testing.T, event, id string, payload any, sig string) int {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if sig == "" {
		mac := hmac.New(sha256.New, []byte(hookSecret))
		mac.Write(raw)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	req, err := http.NewRequest("POST", host+"/webhooks/github", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "1") // the stub's App
	req.Header.Set("X-Hub-Signature-256", sig)
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// repository is the payload's repository; its id is the name's FNV hash, so each name is one repository.
func repository(repo string) map[string]any {
	return map[string]any{"id": repoID(repo), "name": repo, "full_name": owner + "/" + repo, "default_branch": "main", "owner": map[string]any{"login": owner}}
}

func repoID(repo string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repo))
	return int64(h.Sum32())
}

// link links app to the repository named after it, at its root, as an admin's link does (#1917): the
// stack's GitHub is a stub, so there is no repository to list, and the row is written as the route writes it.
func link(t *testing.T, app string) {
	t.Helper()
	compose(t, "exec", "-T", "postgres", "psql", "-U", "canyon", "-d", "canyon", "-v", "ON_ERROR_STOP=1", "-tAc",
		fmt.Sprintf("INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('%s', %d, '', %d, '%s/%s') ON CONFLICT DO NOTHING", app, repoID(app), installation, owner, app))
}

func push(repo, sha string) map[string]any {
	return map[string]any{"ref": "refs/heads/main", "after": sha, "repository": repository(repo),
		"installation": map[string]any{"id": installation}, "sender": map[string]any{"login": "octo"}}
}

func pullRequest(repo, head, sha string) map[string]any {
	return map[string]any{"action": "opened", "repository": repository(repo),
		"pull_request": map[string]any{"head": map[string]any{"sha": sha, "repo": map[string]any{"full_name": head}}},
		"installation": map[string]any{"id": installation}, "sender": map[string]any{"login": "octo"}}
}

// commit is a fresh 40-hex commit id.
func commit() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// tarball makes fixture's GitHub tarball the one the stub serves for repo.
func tarball(t *testing.T, fixture, repo string) {
	t.Helper()
	raw, err := os.ReadFile(".work/fixtures/" + fixture + ".tar.gz") //nolint:gosec // gen.sh's output, named by the journey
	if err == nil {
		err = os.WriteFile(".work/fixtures/"+repo+".tar.gz", raw, 0o644) //nolint:gosec // the stub's container reads it; repo is the journey's own name
	}
	if err != nil {
		t.Fatal(err)
	}
}

// waitCheck waits for a check run of repo logged after the first seen requests to be concluded,
// and answers the newest.
func waitCheck(t *testing.T, repo string, seen int) checkRun {
	t.Helper()
	var run checkRun
	waitFor(t, 3*time.Minute, "a concluded check for "+repo, func() bool {
		for _, r := range slices.Backward(stubLog(t)[seen:]) {
			if strings.Contains(r.Path, "/repos/"+owner+"/"+repo+"/check-runs") && json.Unmarshal([]byte(r.Body), &run) == nil && run.Conclusion != "" {
				return true
			}
		}
		return false
	})
	return run
}
