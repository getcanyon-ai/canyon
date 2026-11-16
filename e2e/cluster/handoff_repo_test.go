//go:build cluster

package cluster

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// repoApp is the hand-off cases' name (README, App names): $E2E_HANDOFF_APP, e2e-repo when unset. Its addresses
// are the gateway's redirect URIs and the test cluster's target groups, and H8 deletes it, whose name Canyon never reuses:
// each run names another of the test cluster's names.
var repoApp = cmp.Or(os.Getenv("E2E_HANDOFF_APP"), "e2e-repo")

// fetchTokens is every fetch-only token H4 made this run, for main_test.go's end-of-run grep: the test cluster's hook
// stores it, and it may appear in no output.
var fetchTokens []string

// deployRepo is the test cluster's deploy repo (README, What the test cluster provides): Canyon writes it with token, its preview
// and production branches are the two named here.
type deployRepo struct{ url, token, preview, prod string }

// TestHandOffRepo is CASES.md H4–H8: hand-off through a deploy repo on the test cluster, e2e/handoff/repo_test.go's
// journey against a real git host, Argo CD, External Secrets and CloudNativePG, behind the test cluster's own gateway.
// `make e2e-cluster-handoff-repo` runs it alone. run_by is the org's, so the cases switch the whole Canyon to
// hand-off and back: the cleanup puts run_by and delivery back to what they were, pass or fail, after the app
// is deleted through the repo.
func TestHandOffRepo(t *testing.T) {
	if host == "" {
		t.Skip("no E2E_CANYON_URL: the suite runs on the test cluster alone")
	}
	repo := deployRepoOf(t)
	admin := browserSignIn(t, "admin")
	alice := aliceSignsIn(t, t.TempDir())
	before := preferencesOf(t, admin)
	if before.RunBy != "canyon" {
		t.Fatalf("run_by is %q before the run: another hand-off is under way, or a run before this one never switched back", before.RunBy)
	}
	requireCheckedGateway(t, admin)
	// done is every pull request the hook has merged this run.
	done := map[string]bool{}

	t.Cleanup(func() { retireFetchToken(t, admin) }) // LIFO: last, once Canyon runs the apps again
	t.Cleanup(func() { restorePreferences(t, admin, before) })
	t.Cleanup(func() { removeRepoApp(t, alice, done) })
	dir := fixture(t, alice, "f-cluster", repoApp)
	deadline, timed := t.Deadline()
	for _, c := range []struct {
		name   string
		budget time.Duration // the longest the case waits: its CLI calls' and waitFor's deadlines
		run    func(t *testing.T)
	}{
		{"H4", 5 * time.Minute, func(t *testing.T) { testH4(t, admin, repo) }},
		{"H5", buildTime + 5*time.Minute, func(t *testing.T) { testH5(t, alice, dir) }},
		{"H6", buildTime + 5*time.Minute, func(t *testing.T) { testH6(t, alice, dir, repo, done) }},
		{"H7", 5 * time.Minute, func(t *testing.T) { testH7(t, alice, dir) }},
		{"H8", 25 * time.Minute, func(t *testing.T) { testH8(t, alice, dir, done) }},
	} {
		if err := tooLate(c.name, c.budget, deadline, timed, time.Now()); err != nil {
			t.Fatal(err)
		}
		if !t.Run(c.name, c.run) {
			t.Fatalf("%s failed: the cases after it need its state", c.name)
		}
	}
}

// cleanupBudget is the longest the cleanup takes: removeRepoApp's ten minutes and its last pass, the remove hook's
// clone and push, restorePreferences' two changes at five tries of 30 s and 5 s each (about six minutes), and
// retireFetchToken.
const cleanupBudget = 20 * time.Minute

// tooLate refuses to start a case when it and the cleanup might not both finish before go test's -timeout, which
// panics the binary and runs no t.Cleanup: hand-off would stay on for everyone. Stopping here instead fails the run
// with the cleanup still to come.
func tooLate(name string, budget time.Duration, deadline time.Time, timed bool, now time.Time) error {
	if left := deadline.Sub(now); timed && left < budget+cleanupBudget {
		return fmt.Errorf("not starting %s: it may take %s and the cleanup %s, and %s is left before go test's -timeout, which runs no cleanup",
			name, budget, cleanupBudget, left.Round(time.Second))
	}
	return nil
}

// deployRepoOf reads the deploy repo and the two hooks from the test cluster's environment, naming every one missing.
func deployRepoOf(t *testing.T) deployRepo {
	t.Helper()
	var missing []string
	for _, name := range []string{"E2E_DEPLOY_REPO_URL", "E2E_DEPLOY_REPO_TOKEN", "E2E_DEPLOY_REPO_PREVIEW", "E2E_DEPLOY_REPO_PROD", "E2E_STORE_FETCH", "E2E_DEPLOY_REPO_HOOK"} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the test cluster sets no %s (README, What the test cluster provides)", strings.Join(missing, ", "))
	}
	return deployRepo{url: os.Getenv("E2E_DEPLOY_REPO_URL"), token: os.Getenv("E2E_DEPLOY_REPO_TOKEN"),
		preview: os.Getenv("E2E_DEPLOY_REPO_PREVIEW"), prod: os.Getenv("E2E_DEPLOY_REPO_PROD")}
}

type handOffPreferences struct {
	RunBy    string `json:"run_by"`
	Delivery string `json:"delivery"`
}

func preferencesOf(t *testing.T, s session) handOffPreferences {
	t.Helper()
	var p handOffPreferences
	s.read(t, "/api/v1/preferences", &p)
	return p
}

// requireCheckedGateway refuses the run before it changes anything unless the saved front door is "Your gateway
// signs people in" and passed Canyon's check of this cluster: hand-off needs choice 1, and switching back to
// run_by canyon is refused behind a door that never passed (409 front_door_unchecked).
func requireCheckedGateway(t *testing.T, admin session) {
	t.Helper()
	var door struct {
		FrontDoor *struct {
			Choice int  `json:"choice"`
			Probed bool `json:"probed"`
		} `json:"front_door"`
	}
	admin.read(t, "/api/v1/front-door", &door)
	if door.FrontDoor == nil || door.FrontDoor.Choice != 1 || !door.FrontDoor.Probed {
		t.Fatalf("the front door is %+v: H4 needs choice 1, checked on this cluster, or run_by could not be switched back", door.FrontDoor)
	}
}

// testH4 is CASES.md H4: the admin saves and checks the deploy repo over the API, the fetch-only token goes to
// the platform through the test cluster's hook, and hand-off is switched on. A response that carries a token is never
// logged.
func testH4(t *testing.T, admin session, repo deployRepo) {
	if code, body := admin.api(t, "PUT", "/api/v1/preferences", map[string]string{"delivery": "repo"}); code != 200 {
		t.Fatalf("PUT /api/v1/preferences {delivery: repo}: %d %s", code, body)
	}
	saved := map[string]any{"url": repo.url, "token": repo.token, "preview": map[string]string{"branch": repo.preview},
		"prod": map[string]string{"branch": repo.prod, "mode": "pull_request"}, "root": "apps"}
	if code, body := admin.api(t, "PUT", "/api/v1/platform-repo", saved); code != 200 {
		t.Fatalf("PUT /api/v1/platform-repo: %d %s", code, body) // a refusal carries no token
	}
	// A new one each run: the first save's is shown once, and a run after it saves the repo again.
	code, body := admin.api(t, "POST", "/api/v1/platform-repo/fetch-token", nil)
	var made struct {
		FetchToken string `json:"fetch_token"`
	}
	if code != 200 || json.Unmarshal(body, &made) != nil || made.FetchToken == "" {
		t.Fatalf("POST /api/v1/platform-repo/fetch-token: %d, no fetch_token", code)
	}
	fetchTokens = append(fetchTokens, made.FetchToken)
	storeFetchToken(t, made.FetchToken)
	var check struct {
		Passed    bool
		Problems  []struct{ Code, Message string }
		Templates []string
	}
	code, body = admin.api(t, "POST", "/api/v1/platform-repo/check", nil)
	if code != 200 || json.Unmarshal(body, &check) != nil {
		t.Fatalf("POST /api/v1/platform-repo/check: %d %s", code, body)
	}
	if want := []string{".canyon/resources/postgres.yaml", ".canyon/route.yaml"}; !check.Passed || !slices.Equal(check.Templates, want) {
		t.Fatalf("the repo check passed %v with templates %v and problems %+v; want passed, templates %v", check.Passed, check.Templates, check.Problems, want)
	}
	if code, body := admin.api(t, "PUT", "/api/v1/preferences", map[string]string{"run_by": "org"}); code != 200 {
		t.Fatalf("PUT /api/v1/preferences {run_by: org}: %d %s", code, body)
	}
	if p := preferencesOf(t, admin); p.RunBy != "org" || p.Delivery != "repo" {
		t.Fatalf("after the set-up run_by is %q and delivery %q, not org and repo", p.RunBy, p.Delivery)
	}
}

// storeFetchToken hands the fetch-only token to the test cluster's hook on stdin, never argv: the platform's secret store,
// where External Secrets reads it (README, The hooks).
func storeFetchToken(t *testing.T, token string) {
	t.Helper()
	hook := os.Getenv("E2E_STORE_FETCH")
	cmd := exec.Command(hook) //nolint:gosec // the test cluster's hook, named by the test cluster
	cmd.Stdin = strings.NewReader(token)
	out, err := cmd.CombinedOutput()
	t.Logf("%s:\n%s", hook, out)
	if err != nil {
		t.Fatalf("%s: %v", hook, err)
	}
}

// repoHook runs the test cluster's deploy repo hook (README, Hand-off through a deploy repo), its output in the log.
func repoHook(t *testing.T, args ...string) (string, error) {
	t.Helper()
	hook := os.Getenv("E2E_DEPLOY_REPO_HOOK")
	out, err := exec.Command(hook, args...).CombinedOutput() //nolint:gosec // the test cluster's hook, named by the test cluster
	t.Logf("%s %s → %v\n%s", hook, strings.Join(args, " "), err, out)
	return string(out), err
}

// proposal is one pull request Canyon opened for repoApp, as its audit trail records the commit: the pull request's
// address and the commit Canyon pushed to it, which the hook merges only while it is still the head.
type proposal struct{ url, commit string }

// proposals are Canyon's pull requests into production for repoApp, oldest first: each "commit release" row that
// names one (runs/repo.go recordCommit).
func proposals(t *testing.T, s session) []proposal {
	t.Helper()
	var page struct {
		Events []struct {
			Action string
			Detail struct {
				Env         string
				Commit      string
				PullRequest string `json:"pull_request"`
			}
		}
	}
	s.read(t, "/api/v1/audit?limit=200&project="+repoApp, &page)
	var out []proposal
	for _, e := range slices.Backward(page.Events) {
		if e.Action == "commit release" && e.Detail.Env == "prod" && e.Detail.PullRequest != "" {
			out = append(out, proposal{url: e.Detail.PullRequest, commit: e.Detail.Commit})
		}
	}
	return out
}

// merged is the hook's line for a pull request it merged, "<url> <head> -> <base>".
type merged struct{ url, head, base string }

func mergedLine(out string) (merged, error) {
	f := strings.Fields(out)
	if len(f) != 4 || f[2] != "->" || !strings.HasPrefix(f[0], "https://") {
		return merged{}, fmt.Errorf("not a merged pull request: %q", out)
	}
	return merged{url: f[0], head: f[1], base: f[3]}, nil
}

// mergeProposals has the hook merge each of Canyon's pull requests for repoApp that done doesn't hold yet, as a
// person, at the commit Canyon pushed, and answers what it merged.
func mergeProposals(t *testing.T, s session, done map[string]bool) ([]merged, error) {
	t.Helper()
	var all []merged
	for _, p := range proposals(t, s) {
		if done[p.url] {
			continue
		}
		out, err := repoHook(t, "merge", p.url, p.commit)
		if err != nil {
			return all, fmt.Errorf("merging %s: %w", p.url, err)
		}
		m, err := mergedLine(out)
		if err != nil {
			return all, err
		}
		done[p.url] = true
		all = append(all, m)
	}
	return all, nil
}

// envRead is one environment of the project read (contracts/schema/app-page.schema.json, env).
type envRead struct {
	URL         string `json:"url"`
	ImageDigest string `json:"image_digest"`
	RunBy       string `json:"run_by"`
	SeenAt      string `json:"seen_at"`
}

func envsOf(t *testing.T, s session) (preview, prod *envRead) {
	t.Helper()
	var read struct {
		Project struct {
			Envs struct{ Preview, Prod *envRead }
		}
	}
	s.read(t, "/api/v1/projects/"+repoApp, &read)
	return read.Project.Envs.Preview, read.Project.Envs.Prod
}

// lastCommit is GET /api/v1/platform-repo's newest repo release.
func lastCommit(t *testing.T, s session) (env, commit string, pr bool) {
	t.Helper()
	var repo struct {
		LastCommit *struct {
			Env, Commit string
			PullRequest *struct{ URL string } `json:"pull_request"`
		} `json:"last_commit"`
	}
	s.read(t, "/api/v1/platform-repo", &repo)
	if repo.LastCommit == nil {
		return "", "", false
	}
	return repo.LastCommit.Env, repo.LastCommit.Commit, repo.LastCommit.PullRequest != nil
}

// The suite's kubeconfig may get exactly these objects of the hand-off cases, by name (README, The read-only
// kubeconfig): each environment's namespace, its Deployment and its database's claim, CloudNativePG's for
// instance 1. Never a list.
func repoNamespace(env string) string { return "canyon-" + repoApp + "-" + env }
func repoClaim(env string) string     { return repoApp + "-" + env + "-db-1" }

func claimKept(t *testing.T, env string) error {
	t.Helper()
	_, err := kubectl(t).CoreV1().PersistentVolumeClaims(repoNamespace(env)).Get(t.Context(), repoClaim(env), metav1.GetOptions{})
	return err
}

// testH5 is CASES.md H5: the deploy commits preview, Argo CD makes the namespace and syncs it, External Secrets
// brings the access token, the database comes up, a pod checks in, and alice opens the app through the gateway.
func testH5(t *testing.T, alice session, dir string) {
	run, code := alice.cliJSON(t, dir, "check")
	if code != 0 && code != 2 {
		t.Fatalf("canyon --format json check: exit %d", code)
	}
	approveBlocking(t, alice, run.GateRun)
	if out, code := alice.canyon(t, dir, "", "deploy"); code != 0 || !strings.Contains(out, "a pod checked in") {
		t.Fatalf("canyon deploy under delivery: repo ended %d, not live by a pod's check-in", code)
	}
	if env, commit, pr := lastCommit(t, alice); env != "preview" || commit == "" || pr {
		t.Errorf("the last repo release is env %q, commit %q, pull request %v; want preview, pushed, none", env, commit, pr)
	}
	preview, _ := envsOf(t, alice)
	if want := "https://" + repoApp + ".preview." + os.Getenv("E2E_APPS_DOMAIN"); preview == nil || preview.URL != want || preview.RunBy != "org" || preview.SeenAt == "" || !digestRef.MatchString(preview.ImageDigest) {
		t.Errorf("envs.preview = %+v; want url %s, run_by org, seen_at set, a digest", preview, want)
	}
	deployment, err := kubectl(t).AppsV1().Deployments(repoNamespace("preview")).Get(t.Context(), repoApp+"-preview", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Deployment %s-preview in %s: %v", repoApp, repoNamespace("preview"), err)
	}
	var inits []string
	for _, c := range deployment.Spec.Template.Spec.InitContainers {
		inits = append(inits, c.Name)
	}
	if !slices.Equal(inits, []string{"canyon-identity"}) {
		t.Errorf("init containers = %v; want [canyon-identity] alone", inits)
	}
	if err := claimKept(t, "preview"); err != nil {
		t.Errorf("the database's claim %s: %v", repoClaim("preview"), err)
	}
	playwright(t, " H5 ") // e2e/ui/cluster/handoff.spec.ts: alice through the gateway, /whoami and /db
}

// testH6 is CASES.md H6: production is a pull request into the production branch, which a person merges through
// the test cluster's hook (never Canyon's token), at the commit Canyon pushed; then it goes live the same way, on preview's
// image.
func testH6(t *testing.T, alice session, dir string, repo deployRepo, done map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	promote := exec.CommandContext(ctx, cli, "deploy", "--env", "prod", "--yes") //nolint:gosec // the CLI this test built
	var printed bytes.Buffer
	promote.Dir, promote.Env, promote.Stdout, promote.Stderr = dir, cliEnv(t, alice.home), &printed, &printed
	if err := promote.Start(); err != nil {
		t.Fatal(err)
	}
	var all []merged
	waitFor(t, 5*time.Minute, "Canyon's pull request into production, merged by the hook", func() (string, bool) {
		m, err := mergeProposals(t, alice, done)
		all = append(all, m...)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d merged", len(all)), len(all) > 0
	})
	err := promote.Wait()
	t.Logf("canyon deploy --env prod --yes → %v\n%s", err, printed.String())
	if len(all) != 1 || !strings.HasPrefix(all[0].head, "canyon/"+repoApp+"/prod/") || all[0].base != repo.prod {
		t.Errorf("the hook merged %+v; want one pull request from canyon/%s/prod/… into %s", all, repoApp, repo.prod)
	}
	if err != nil || !strings.Contains(printed.String(), "a pod checked in") {
		t.Fatal("canyon deploy --env prod did not end live by a pod's check-in once the pull request was merged")
	}
	preview, prod := envsOf(t, alice)
	if prod == nil || preview == nil || prod.ImageDigest != preview.ImageDigest || prod.RunBy != "org" || prod.SeenAt == "" {
		t.Errorf("envs.prod = %+v, envs.preview = %+v; want preview's image, run_by org, seen_at set", prod, preview)
	}
}

// testH7 is CASES.md H7: undeploy commits preview's objects away and Argo CD prunes the Deployment; canyon.json
// and resources/ stay, so the namespace, made from the platform repo, and the database's claim stay with the data.
func testH7(t *testing.T, alice session, dir string) {
	if out, code := alice.canyon(t, dir, "", "undeploy", "--env", "preview"); code != 0 {
		t.Fatalf("canyon undeploy --env preview: exit %d\n%s", code, out)
	}
	waitFor(t, 3*time.Minute, "Argo CD to prune "+repoApp+"-preview's Deployment", func() (string, bool) {
		_, err := kubectl(t).AppsV1().Deployments(repoNamespace("preview")).Get(t.Context(), repoApp+"-preview", metav1.GetOptions{})
		return fmt.Sprintf("Deployment err=%v", err), apierrors.IsNotFound(err)
	})
	if _, err := kubectl(t).CoreV1().Namespaces().Get(t.Context(), repoNamespace("preview"), metav1.GetOptions{}); err != nil {
		t.Errorf("namespace %s after the undeploy: %v; want it kept", repoNamespace("preview"), err)
	}
	if err := claimKept(t, "preview"); err != nil {
		t.Errorf("the database's claim %s after the undeploy: %v; want it kept, with the data", repoClaim("preview"), err)
	}
}

// testH8 is CASES.md H8: delete commits both directories away, production's through a pull request the hook
// merges, and finishes; Argo CD then removes both namespaces, and the data with them.
func testH8(t *testing.T, alice session, dir string, done map[string]bool) {
	waitFor(t, 10*time.Minute, "canyon delete to finish", func() (string, bool) {
		out, code := alice.canyon(t, dir, "", "delete", "--confirm", repoApp)
		if code == 0 && strings.Contains(out, "Deleted "+repoApp) {
			return out, true
		}
		if _, err := mergeProposals(t, alice, done); err != nil {
			t.Fatal(err)
		}
		return out, false
	})
	for _, env := range []string{"preview", "prod"} {
		waitFor(t, 5*time.Minute, repoNamespace(env)+" to be gone, with its data", func() (string, bool) {
			_, err := kubectl(t).CoreV1().Namespaces().Get(t.Context(), repoNamespace(env), metav1.GetOptions{})
			return fmt.Sprintf("namespace err=%v", err), apierrors.IsNotFound(err)
		})
	}
}

// removeRepoApp is the cleanup's first half, pass or fail, while run_by is still org: the app deleted through the
// repo (the hook merging production's pull request), then a deletion commit, never a force-push, for anything
// still under its directory on either branch.
func removeRepoApp(t *testing.T, alice session, done map[string]bool) {
	for until := time.Now().Add(10 * time.Minute); ; time.Sleep(10 * time.Second) {
		code, body := alice.api(t, "DELETE", "/api/v1/projects/"+repoApp+"?confirm="+repoApp, nil)
		if code == 204 || code == 404 {
			break
		}
		if time.Now().After(until) {
			t.Errorf("deleting %s in the cleanup: still %d %s after ten minutes", repoApp, code, body)
			break
		}
		if _, err := mergeProposals(t, alice, done); err != nil {
			t.Logf("the cleanup's merge: %v", err)
		}
	}
	if _, err := repoHook(t, "remove", repoApp); err != nil {
		t.Errorf("the deploy repo hook's remove %s: %v", repoApp, err)
	}
}

// restorePreferences is the cleanup's second half: run_by back first, then delivery, which under hand-off would
// need the other way saved. Each change is tried five times, 5 s apart, each under a context of its own, never the
// test's, so a case that ran out of its own time still restores. A timed-out go test runs no cleanup at all: the
// test cluster's runner restores then (README, Hand-off through a deploy repo).
func restorePreferences(t *testing.T, admin session, before handOffPreferences) {
	for _, change := range []map[string]string{{"run_by": before.RunBy}, {"delivery": before.Delivery}} {
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(5 * time.Second)
			}
			if err = putPreferences(admin.cookie, change); err == nil {
				break
			}
		}
		if err != nil {
			t.Errorf("PUT /api/v1/preferences %v in the cleanup, five times: %v", change, err)
		}
	}
}

// putPreferences is one PUT /api/v1/preferences as the session cookie's person, under its own 30 s context.
func putPreferences(cookie string, change map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := json.Marshal(change)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, host+"/api/v1/preferences", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", sessionCookie+"="+cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%d %s", res.StatusCode, answer)
	}
	return nil
}

// retireFetchToken replaces the fetch-only token once the run is over, so the one H4 stored in the cluster opens
// nothing; the new one is never stored, and is in the end-of-run grep. Nothing to retire when H4 stored none.
func retireFetchToken(t *testing.T, admin session) {
	if len(fetchTokens) == 0 {
		return
	}
	code, body := admin.api(t, "POST", "/api/v1/platform-repo/fetch-token", nil)
	var made struct {
		FetchToken string `json:"fetch_token"`
	}
	if code != 200 || json.Unmarshal(body, &made) != nil || made.FetchToken == "" {
		t.Errorf("retiring the fetch-only token in the cleanup: %d, no fetch_token", code)
		return
	}
	fetchTokens = append(fetchTokens, made.FetchToken)
}

func TestUnitTooLate(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := tooLate("H8", 25*time.Minute, now.Add(44*time.Minute), true, now); err == nil {
		t.Error("H8 (25 min) with 44 min left, the cleanup's 20 min included: started; want refused")
	}
	if err := tooLate("H8", 25*time.Minute, now.Add(46*time.Minute), true, now); err != nil {
		t.Errorf("H8 (25 min) with 46 min left: %v; want started", err)
	}
	if err := tooLate("H8", 25*time.Minute, time.Time{}, false, now); err != nil {
		t.Errorf("no -timeout at all: %v; want started", err)
	}
}

func TestUnitPutPreferences(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cookie, _ := r.Cookie(sessionCookie)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body)+" "+cookie.Value)
		if strings.Contains(string(body), "org") {
			http.Error(w, `{"error":"front_door_required"}`, http.StatusConflict)
		}
	}))
	defer srv.Close()
	was := host
	host = srv.URL
	defer func() { host = was }()
	if err := putPreferences("s3ss10n", map[string]string{"run_by": "canyon"}); err != nil {
		t.Errorf("a 200 answered %v; want nil", err)
	}
	if err := putPreferences("s3ss10n", map[string]string{"run_by": "org"}); err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("a 409 answered %v; want an error naming it: a refused restore must fail the cleanup", err)
	}
	if want := []string{`PUT /api/v1/preferences {"run_by":"canyon"} s3ss10n`, `PUT /api/v1/preferences {"run_by":"org"} s3ss10n`}; !slices.Equal(got, want) {
		t.Errorf("the server saw %q; want %q", got, want)
	}
}

func TestUnitMergedLine(t *testing.T) {
	out := "https://github.com/acme/deploy/pull/7 canyon/e2e-repo/prod/rel_8f2a -> prod\n"
	want := merged{url: "https://github.com/acme/deploy/pull/7", head: "canyon/e2e-repo/prod/rel_8f2a", base: "prod"}
	if got, err := mergedLine(out); err != nil || got != want {
		t.Errorf("mergedLine(%q) = %+v, %v; want %+v", out, got, err, want)
	}
	for _, bad := range []string{"", "gh: Not Found (HTTP 404)\n", "Pull request #7 merged\n", "https://github.com/acme/deploy/pull/7 canyon/e2e-repo/prod/rel_8f2a prod\n"} {
		if _, err := mergedLine(bad); err == nil {
			t.Errorf("mergedLine(%q) answered no error: a hook answer it can't read must fail the case, never read as merged", bad)
		}
	}
}
