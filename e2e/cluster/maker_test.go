//go:build cluster

package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestMaker is M1-M9 (CASES.md, M — Maker alice), against a Canyon already installed
// (e2e/cluster/README.md). M1's confirmation, M4, M5, M8's app-page assertion and all of M8's
// deploy each need a browser or a precondition only the admin/Playwright PRs give — noted at each
// t.Skip below — and aliceSignsIn (signin_test.go) drives M1's own device flow far enough to get
// a session for M2 onward.
func TestMaker(t *testing.T) {
	alice := aliceSignsIn(t, t.TempDir())
	// README, Fresh install per run: t.Cleanup runs even when a subtest above fails or panics
	// (LIFO, after every subtest returns), so both fixed names are always forgotten.
	t.Cleanup(func() {
		alice.forget(t, "e2e-app")
		alice.forget(t, "e2e-peer")
	})
	t.Run("M1", func(t *testing.T) {
		t.Skip("M1's confirmation happens in a browser (e2e/ui, #482, not merged); aliceSignsIn already drove the Go/CLI half this run needed")
	})
	t.Run("M2", func(t *testing.T) { testM2(t, alice) })
	t.Run("M3", func(t *testing.T) { testM3(t, alice) })
	t.Run("M4", func(t *testing.T) {
		t.Skip("M4 reads /whoami in a browser (app surface); needs e2e/ui #482")
	})
	t.Run("M5", func(t *testing.T) {
		t.Skip("M5's own step is a UI click (\"Promote to production\" on the app page); needs e2e/ui #482")
	})
	t.Run("M6", func(t *testing.T) { testM6(t, alice) })
	// M5's UI click is skipped above; M7 still needs e2e-app promoted (its "two databases" —
	// preview's and prod's) so this promotes it through the CLI path K1 already uses, not
	// asserting M5's own case, only setting up the state M7 needs.
	promoteToProd(t, alice)
	t.Run("M7", func(t *testing.T) { testDelete(t, alice, "e2e-app", 2) })
	t.Run("M8", func(t *testing.T) {
		t.Skip("M8 needs A4 (choice 1 saved, the admin PR's job) and I1's phase-2 deploy of e2e-peer under choice 2; neither exists in this PR, so asserting a deploy here would assert against the wrong precondition")
	})
	t.Run("M9", func(t *testing.T) { testDelete(t, alice, "e2e-peer", 1) }) // its preview database: f-cluster declares one
}

func promoteToProd(t *testing.T, alice session) {
	t.Helper()
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	if out, code := alice.canyon(t, dir, "", "deploy", "--env", "prod", "--yes"); code != 0 {
		t.Fatalf("canyon deploy --env prod --yes (setting up M7's state): exit %d\n%s", code, out)
	}
}

// result is one finding of GET /api/v1/gate-runs/:id — a SARIF result plus the fields the approve
// route needs (e2e/deploy_test.go's j4Deploy and e2e/stack_test.go's result, ported: same route,
// same shape, a different package's copy since e2e (stack-tagged) and e2e/cluster (cluster-tagged)
// never build together).
type result struct {
	RuleID              string `json:"ruleId"`
	PartialFingerprints map[string]string
	Properties          struct {
		Canyon struct{ Blocks bool }
	}
}

func (r result) fingerprint() string { return r.PartialFingerprints["canyon/v1"] }

func runFindings(t *testing.T, alice session, run string) []result {
	t.Helper()
	var r struct {
		Run struct {
			Findings []result
		} `json:"run"`
	}
	alice.read(t, "/api/v1/gate-runs/"+run, &r)
	return r.Run.Findings
}

// approveBlocking is j4Deploy's approve loop (e2e/deploy_test.go:38-46), ported: M3's deploy
// cannot clear the gate while a blocking finding sits unapproved, so M2 approves every one of
// them, "e2e: the fixture's findings are planted", as Docker's J4 does (CASES.md M2). alice started
// the run, so the test cluster's admin approves them, from a browser session: never the author, never a CLI (#2204).
func approveBlocking(t *testing.T, alice session, run string) {
	t.Helper()
	admin := browserSignIn(t, "admin")
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, f := range runFindings(t, alice, run) {
		if !f.Properties.Canyon.Blocks {
			continue
		}
		code, body := admin.api(t, "POST", "/api/v1/gate-runs/"+run+"/findings/"+f.fingerprint()+"/approve",
			map[string]string{"reason": "e2e: the fixture's findings are planted", "expires_at": expires})
		if code != 200 {
			t.Fatalf("approve %s: %d %s", f.RuleID, code, body)
		}
	}
}

// testM2 is CASES.md M2: upload the fixture, the real gate review runs once, a second check
// reuses it, and every blocking finding is approved so M3 can deploy past the gate.
//
// "GET /api/v1/gate-runs/:id shows the ai review producer among the run's producers with
// failure_reason empty" is not checked: the run read carries `findings[]` (used above for the
// approve loop) but no separate producers list. Noted in the PR body.
func testM2(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	// Every scanner runs by default, so this first check is the first scan: it makes trivy's server,
	// canyon-trivy, which downloads trivy's database; a fresh install has none yet (contracts/executor.md,
	// On Kubernetes; #1652).
	if len(pods(t, "canyon-scans", "canyon.trivy=true")) != 0 {
		t.Fatalf("trivy's server already runs in canyon-scans: M2's precondition (a fresh install, README) does not hold on this test cluster")
	}
	run, code := alice.cliJSON(t, dir, "check")
	if code != 0 && code != 2 {
		t.Fatalf("canyon --format json check: exit %d; want 0 or 2, never 1", code)
	}
	waitFor(t, 30*time.Minute, "trivy's server canyon-trivy to have a ready pod", func() (string, bool) {
		for _, p := range pods(t, "canyon-scans", "canyon.trivy=true") {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return "ready", true
				}
			}
		}
		return "not ready", false
	})
	requireNoNote(t, run, gate.Skipped)
	requireNoNote(t, run, gate.Reused)
	again, code := alice.cliJSON(t, dir, "check")
	if code != 0 && code != 2 {
		t.Fatalf("canyon --format json check (again): exit %d", code)
	}
	requireNote(t, again, gate.Reused)
	var access struct {
		Roles []struct{ Name string }
	}
	alice.read(t, "/api/v1/projects/e2e-app/access", &access)
	var hasApprover bool
	for _, r := range access.Roles {
		hasApprover = hasApprover || r.Name == "approver"
	}
	if !hasApprover {
		t.Errorf("GET /api/v1/projects/e2e-app/access roles = %v; want \"approver\" among them", access.Roles)
	}
	approveBlocking(t, alice, run.GateRun)
}

func requireNote(t *testing.T, run cliRun, text string) {
	t.Helper()
	if !hasNote(run, text) {
		t.Errorf("canyon check notes = %v; want %q among them", run.Notes, text)
	}
}

func requireNoNote(t *testing.T, run cliRun, text string) {
	t.Helper()
	if hasNote(run, text) {
		t.Errorf("canyon check notes = %v; want no %q", run.Notes, text)
	}
}

func hasNote(run cliRun, text string) bool {
	for _, n := range run.Notes {
		if n.Text == text {
			return true
		}
	}
	return false
}

// digestRef is the project read's envs.*.image_digest shape (contracts/routes.md, Projects &
// packages): sha256:<64 hex>, the executor's half of a full image reference.
// projectRead is GET /api/v1/projects/:id as M3 and M6 read it.
type projectRead struct {
	Project struct {
		Envs struct {
			Preview *struct {
				ImageDigest string `json:"image_digest"`
			} `json:"preview"`
		} `json:"envs"`
	}
}

var digestRef = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// testM3 is CASES.md M3: a real buildpack build, the preview live, with the cluster objects
// contracts/executor.md's On Kubernetes promises: the app container's exact image and its
// DATABASE_URL from the Secret canyon-env-<app>-preview (the whole Secret through envFrom,
// executor/kube/objects.go), never a literal, and the project read's resources naming the
// database ready. (`canyon resources` is the host's catalogue of component types, design/specs
// /cli.md §6.5, not the app's resources.)
//
// "canyon-builds holds no Build labelled canyon.build for the app 60s after the deploy ends" is
// not checked: a kpack Build is a CRD, and this suite's kubeconfig client (main_test.go's
// `kube kubernetes.Interface`, kube_test.go's pods/configMaps/jobs/claims helpers) is the typed
// core/batch clientset alone — it has no lister for kpack's Build resource. Noted in the PR body.
func testM3(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	run, code := alice.cliJSON(t, dir, "deploy")
	if code != 0 {
		t.Fatalf("canyon --format json deploy: exit %d", code)
	}
	want := "https://e2e-app.preview." + os.Getenv("E2E_APPS_DOMAIN")
	if run.Deployment == nil || run.Deployment.URL != want {
		t.Fatalf("deployment.url = %v; want %s", run.Deployment, want)
	}
	requireInOrder(t, alice.timeline(t, run.GateRun), "postgres db: creating", "postgres db: ready")
	if got := alice.liveURL(t, run.GateRun); got != want {
		t.Errorf("deploy.completed url = %q; want %s", got, want)
	}

	var project projectRead
	alice.read(t, "/api/v1/projects/e2e-app", &project)
	preview := project.Project.Envs.Preview
	if preview == nil || !digestRef.MatchString(preview.ImageDigest) {
		t.Fatalf("envs.preview = %+v; want image_digest matching ^sha256:[0-9a-f]{64}$", preview)
	}
	requireClusterObjects(t, "e2e-app-preview")

	var declared struct {
		Project struct {
			Resources []struct {
				Name, Type, State string
				Env               *string
			}
		}
	}
	alice.read(t, "/api/v1/projects/e2e-app", &declared)
	var ready bool
	for _, r := range declared.Project.Resources {
		ready = ready || (r.Name == "db" && r.Type == "postgres" && r.Env != nil && *r.Env == "preview" && r.State == "ready")
	}
	if !ready {
		t.Errorf("project resources = %+v; want db · postgres · preview · ready among them", declared.Project.Resources)
	}

	pod, ok := podNamed(t, "canyon-apps", "e2e-app-preview")
	if !ok {
		t.Fatalf("no pod named e2e-app-preview-* in canyon-apps")
	}
	app := findContainer(t, pod, "app")
	wantSuffix := "/canyon/e2e-app@" + preview.ImageDigest
	if !strings.HasSuffix(app.Image, wantSuffix) {
		t.Errorf("app container image = %q; want a %q suffix", app.Image, wantSuffix)
	}
	for _, e := range app.Env {
		if e.Name == "DATABASE_URL" {
			t.Errorf("app container names DATABASE_URL in its own env (%q); want it from the Secret alone", e.Value)
		}
	}
	if from := envFromSecrets(app); !slices.Contains(from, "canyon-env-e2e-app-preview") {
		t.Errorf("app container's envFrom Secrets = %v; want canyon-env-e2e-app-preview", from)
	}
}

// envFromSecrets is the names of the Secrets c takes its whole environment from.
func envFromSecrets(c corev1.Container) []string {
	var names []string
	for _, e := range c.EnvFrom {
		if e.SecretRef != nil {
			names = append(names, e.SecretRef.Name)
		}
	}
	return names
}

// findContainer answers pod's container named name, and fails the test if there is none.
func findContainer(t *testing.T, pod corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("pod %s has no container named %s", pod.Name, name)
	return corev1.Container{}
}

// requireInOrder is M3's "showed X before Y": both lines are present, and X's first index is
// before Y's first index.
func requireInOrder(t *testing.T, lines []string, before, after string) {
	t.Helper()
	bi, ai := indexOf(lines, before), indexOf(lines, after)
	if bi == -1 || ai == -1 || bi >= ai {
		t.Errorf("timeline = %v; want %q before %q", lines, before, after)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func indexOf(lines []string, s string) int {
	for i, l := range lines {
		if l == s {
			return i
		}
	}
	return -1
}

// podNamed answers the one pod in namespace whose name starts with prefix + "-" (a Deployment's
// pod carries a generated suffix), and whether one was found.
func podNamed(t *testing.T, namespace, prefix string) (corev1.Pod, bool) {
	t.Helper()
	for _, p := range pods(t, namespace, "") {
		if hasPrefix(p.Name, prefix+"-") {
			return p, true
		}
	}
	return corev1.Pod{}, false
}

// requireClusterObjects is M3's cluster assertions: the Deployment's pod, Service, NetworkPolicy
// and access ConfigMap of <app>-<target> exist, and the Service's one port is 4180 on both sides.
func requireClusterObjects(t *testing.T, name string) {
	t.Helper()
	if _, ok := podNamed(t, "canyon-apps", name); !ok {
		t.Errorf("no pod named %s-* in canyon-apps", name)
	}
	svc, err := kubectl(t).CoreV1().Services("canyon-apps").Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("reading Service %s in canyon-apps: %v", name, err)
	} else if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 4180 || svc.Spec.Ports[0].TargetPort.IntValue() != 4180 {
		t.Errorf("Service %s ports = %v; want one port, 4180 on both sides", name, svc.Spec.Ports)
	}
	if _, err := kubectl(t).NetworkingV1().NetworkPolicies("canyon-apps").Get(t.Context(), name, metav1.GetOptions{}); err != nil {
		t.Errorf("reading NetworkPolicy %s in canyon-apps: %v", name, err)
	}
	if _, err := kubectl(t).CoreV1().ConfigMaps("canyon-apps").Get(t.Context(), "canyon-access-"+name, metav1.GetOptions{}); err != nil {
		t.Errorf("reading ConfigMap canyon-access-%s in canyon-apps: %v", name, err)
	}
}

// testM6 is CASES.md M6: undeploy keeps the data, redeploy reuses it.
//
// "/db counts one more than before the undeploy" is not checked: /db is the fixture app's own
// page, reached through e2e-app's public URL, which — as M4's whoami — sits behind oauth2-proxy
// and needs a browser signed in through the identity provider (e2e/ui #482, not merged), even
// though M6's own CASES.md tag names no "app" surface. Noted in the PR body.
//
// gone (below) checks apierrors.IsNotFound, never a bare err != nil: the read-only kubeconfig has
// no secrets verb at all (README, The read-only kubeconfig), so a Secret read is always Forbidden,
// which a bare err != nil misreads as "gone" whether the Secret is there or not. The objects the
// kubeconfig CAN read — the Deployment, the NetworkPolicy, the access ConfigMap — are checked
// instead.
func testM6(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	if out, code := alice.canyon(t, dir, "", "undeploy", "--env", "preview"); code != 0 {
		t.Fatalf("canyon undeploy --env preview: exit %d\n%s", code, out)
	}
	var project projectRead
	alice.read(t, "/api/v1/projects/e2e-app", &project)
	if project.Project.Envs.Preview != nil {
		t.Errorf("project read still has envs.preview after undeploy")
	}
	waitFor(t, 60*time.Second, "e2e-app-preview's Deployment, NetworkPolicy and access ConfigMap to be gone", func() (string, bool) {
		_, depErr := kubectl(t).AppsV1().Deployments("canyon-apps").Get(t.Context(), "e2e-app-preview", metav1.GetOptions{})
		_, npErr := kubectl(t).NetworkingV1().NetworkPolicies("canyon-apps").Get(t.Context(), "e2e-app-preview", metav1.GetOptions{})
		_, cmErr := kubectl(t).CoreV1().ConfigMaps("canyon-apps").Get(t.Context(), "canyon-access-e2e-app-preview", metav1.GetOptions{})
		gone := apierrors.IsNotFound(depErr) && apierrors.IsNotFound(npErr) && apierrors.IsNotFound(cmErr)
		return fmt.Sprintf("Deployment err=%v, NetworkPolicy err=%v, ConfigMap err=%v", depErr, npErr, cmErr), gone
	})
	row := alice.secretOf(t, "e2e-app", "preview", "DATABASE_URL")
	if row == nil || row.Origin != "provisioned" {
		t.Errorf("DATABASE_URL preview row = %+v; want origin \"provisioned\", still there after undeploy", row)
	}
	// --force: this is a fresh copy of the tree M3 deployed, and a copy with no base is refused on an
	// app that has a current package (#1503).
	if out, code := alice.canyon(t, dir, "", "--format", "json", "deploy", "--force"); code != 0 {
		t.Fatalf("canyon --format json deploy --force (redeploy): exit %d\n%s", code, out)
	}
}

// deleteRefusal is the 409 DELETE /api/v1/projects/:id answers without confirm (contracts/routes.md).
type deleteRefusal struct {
	Error     string `json:"error"`
	Resources []struct {
		Name, Type, Env, State string
	} `json:"resources"`
}

// testDelete is CASES.md M7 (and, called on e2e-peer, M9): delete without confirm names the
// resources that would be destroyed, then with confirm actually deletes. wantResources is the
// exact count: 2 for e2e-app (both its databases, preview's and prod's, once promoteToProd has
// run), 1 for e2e-peer (M9: its preview database alone, never promoted) — always asserted exactly,
// which is itself the assertion that e2e-peer's delete is the never-promoted case.
func testDelete(t *testing.T, alice session, app string, wantResources int) {
	t.Helper()
	code, body := alice.api(t, "DELETE", "/api/v1/projects/"+app, nil)
	var refusal deleteRefusal
	if code != 409 || json.Unmarshal(body, &refusal) != nil || refusal.Error != "confirm_required" {
		t.Fatalf("DELETE /api/v1/projects/%s without confirm: %d %s; want 409 confirm_required", app, code, body)
	}
	if len(refusal.Resources) != wantResources {
		t.Errorf("confirm_required resources = %v; want %d", refusal.Resources, wantResources)
	}
	code, body = alice.api(t, "DELETE", "/api/v1/projects/"+app+"?confirm="+app, nil)
	if code != 204 {
		t.Fatalf("DELETE /api/v1/projects/%s?confirm=%s: %d %s; want 204", app, app, code, body)
	}
	if code, _ := alice.api(t, "GET", "/api/v1/projects/"+app, nil); code != 404 {
		t.Errorf("GET /api/v1/projects/%s after delete: %d; want 404", app, code)
	}
	// As thorough as M6's gone-check: the Deployment, Service, NetworkPolicy and access ConfigMap
	// of every <app>-<env> (preview and prod both use this prefix pattern) — a pod's absence alone
	// doesn't rule out a surviving Service, NetworkPolicy or ConfigMap, only implies (never
	// proves) the Deployment that would recreate a pod is gone too. The env Secret is not checked:
	// the read-only kubeconfig has no secrets verb at all (README), so a Get always answers
	// Forbidden, never IsNotFound, whether or not the Secret is actually gone — the same vacuity
	// M6's fix already removed a Secret read for.
	waitFor(t, 60*time.Second, fmt.Sprintf("nothing named %s-* left in canyon-apps", app), func() (string, bool) {
		for _, env := range []string{"preview", "prod"} {
			name := app + "-" + env
			if _, err := kubectl(t).AppsV1().Deployments("canyon-apps").Get(t.Context(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return fmt.Sprintf("Deployment %s err=%v", name, err), false
			}
			if _, err := kubectl(t).CoreV1().Services("canyon-apps").Get(t.Context(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return fmt.Sprintf("Service %s err=%v", name, err), false
			}
			if _, err := kubectl(t).NetworkingV1().NetworkPolicies("canyon-apps").Get(t.Context(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return fmt.Sprintf("NetworkPolicy %s err=%v", name, err), false
			}
			if _, err := kubectl(t).CoreV1().ConfigMaps("canyon-apps").Get(t.Context(), "canyon-access-"+name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return fmt.Sprintf("ConfigMap canyon-access-%s err=%v", name, err), false
			}
		}
		return "none found", true
	})
}

// TestUnitNoteMatchesTheGateConstant is the fix for the bug this comment documents: the old code
// compared a note's Text to the literal string "gate.Reused" — never the constant's actual value,
// "ai review · reused from an earlier review of the same code and policy" (gate.Reused,
// gate/review.go) — so requireNoNote(t, run, "gate.Reused") passed on every run, blocking or not,
// and requireNote(t, run, "gate.Reused") always failed. This proves hasNote now matches the real
// value and rejects the placeholder string the old code used.
func TestUnitNoteMatchesTheGateConstant(t *testing.T) {
	run := cliRun{Notes: []struct{ Producer, Text string }{{Producer: "gate", Text: gate.Reused}}}
	if !hasNote(run, gate.Reused) {
		t.Errorf("hasNote(%v, gate.Reused) = false; want true", run.Notes)
	}
	if hasNote(run, "gate.Reused") { // the literal the old, buggy comparison used
		t.Errorf(`hasNote(%v, "gate.Reused") = true; want false — that string is not gate.Reused's value`, run.Notes)
	}
	if hasNote(run, gate.Skipped) {
		t.Errorf("hasNote(%v, gate.Skipped) = true; want false", run.Notes)
	}
}

// TestUnitGoneMeansNotFoundNeverForbidden is the fix for the M6 vacuity: apierrors.IsNotFound must
// tell a Secret that is actually gone apart from one this kubeconfig was simply never allowed to
// read (README: no secrets verb at all) — a bare err != nil cannot, since both are non-nil errors.
func TestUnitGoneMeansNotFoundNeverForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "canyon-env-e2e-app-preview", fmt.Errorf("denied"))
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "deployments"}, "e2e-app-preview")
	if apierrors.IsNotFound(forbidden) {
		t.Errorf("IsNotFound(Forbidden) = true; want false — Forbidden must never read as gone")
	}
	if !apierrors.IsNotFound(notFound) {
		t.Errorf("IsNotFound(NotFound) = false; want true")
	}
}

// TestUnitDigestRef is the image-digest shape M3 checks.
func TestUnitDigestRef(t *testing.T) {
	for _, want := range []struct {
		digest string
		ok     bool
	}{
		{"sha256:" + fixedHex64, true},
		{"sha256:" + fixedHex64[:63], false},
		{fixedHex64, false},
		{"", false},
	} {
		if got := digestRef.MatchString(want.digest); got != want.ok {
			t.Errorf("digestRef.MatchString(%q) = %v; want %v", want.digest, got, want.ok)
		}
	}
}

var fixedHex64 = strings.Repeat("0123456789abcdef", 4)

// TestUnitProjectReadShape holds projectRead to the route's answer, the row under "project"
// (contracts/routes.md, `{project(s)}`), as e2e/deploy_test.go reads it: decoded at the top
// level instead, M3's digest read "" after every deploy, and M6's "no envs.preview after
// undeploy" held whatever the server answered (canyonv2-a270).
func TestUnitProjectReadShape(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var project projectRead
	if err := json.Unmarshal([]byte(`{"project":{"id":"e2e-app","envs":{"preview":{"image_digest":"`+digest+`"}}}}`), &project); err != nil {
		t.Fatal(err)
	}
	if p := project.Project.Envs.Preview; p == nil || p.ImageDigest != digest {
		t.Fatalf("envs.preview = %+v; want image_digest %s", p, digest)
	}
}
