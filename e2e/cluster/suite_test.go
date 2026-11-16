//go:build cluster

package cluster

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestSuite is the run (CASES.md, The run; README, Order): every case of wave 1 in the order the
// table gives, the Go cases called as functions and the Playwright cases by id
// (`--project=cluster --grep`), all in one process so main_test.go's end-of-run secret grep sees
// every line. `make e2e-cluster` runs it. The Go-only TestMaker, TestSecretsAndScanners and
// TestIsolationAndImages still run their cases on their own; this is the order between them.
func TestSuite(t *testing.T) {
	if host == "" {
		t.Skip("no E2E_CANYON_URL: the suite runs on the test cluster alone")
	}
	var alice session
	home := t.TempDir() // alice's CLI config home, alive for every step (aliceSignsIn)
	// LIFO: the front door goes back to none after the apps are forgotten. The hook is the test cluster's
	// (README, The hook); a run without it fails here rather than leaving the gateway switched.
	// Canyon's saved door goes back to A2's choice 2 with it, or the test cluster says choice 1 over an
	// Ingress the hook took the gateway's sign-in off.
	t.Cleanup(func() { switchFrontDoor(t, "none"); saveChoice2(t) })
	t.Cleanup(func() { // README, Fresh install per run: whatever failed, nothing is left for the next phase
		if alice.cookie != "" {
			alice.forget(t, "e2e-app")
			alice.forget(t, "e2e-peer")
		}
	})
	for _, s := range steps(&alice, home) {
		if !t.Run(s.name, s.run) {
			t.Fatalf("%s failed: the cases after it need its state", s.name)
		}
	}
}

// step is one case, or one Playwright selection of cases, in the run.
type step struct {
	name string
	run  func(t *testing.T)
}

// steps is the order. alice is filled by the first Go step, so the closures read it when they run;
// home is her CLI config home, the caller's (aliceSignsIn).
func steps(alice *session, home string) []step {
	pw := func(name, grep string) step { return step{name, func(t *testing.T) { playwright(t, grep) }} }
	goStep := func(name string, f func(t *testing.T, alice session)) step {
		return step{name, func(t *testing.T) { f(t, *alice) }}
	}
	return []step{
		pw("A1-A3", " A[123] "),
		pw("A5", " A5 "), // before any deploy: every app of the run is floored
		{"M1", func(t *testing.T) { *alice = aliceSignsIn(t, home) }}, // the case whole: the CLI, the browser's approval, its asserts
		goStep("M2", testM2),
		goStep("M3", testM3),
		goStep("peer under choice 2", func(t *testing.T, alice session) { deployApp(t, alice, "e2e-peer") }), // I1's app B (CASES I1)
		goStep("G1", testG1),
		goStep("G2", testG2),
		pw("M4-M5", " M[45] "),
		// K1 on M5's state: each rotation's value reaches the browser's K1 as E2E_K1_VALUE (rotate).
		goStep("K1", func(t *testing.T, alice session) { rotations(t, alice, func(t *testing.T) { playwright(t, " K1 ") }) }),
		pw("S1-S4", " S[1-4] "),
		pw("I1-I2", " I[12] "),
		pw("I5", " I5 "),
		{"I3", testI3},
		{"I4", testI4},
		{"P1", testP1},
		goStep("M6", testM6),
		goStep("M7", func(t *testing.T, alice session) { testDelete(t, alice, "e2e-app", 2) }),
		pw("A4", " A4 "),
		goStep("M8", testM8),
		goStep("M9", func(t *testing.T, alice session) { testDelete(t, alice, "e2e-peer", 1) }), // its preview database: f-cluster declares one
	}
}

// playwright runs the cluster project's cases whose names match grep (e2e/ui/cluster), its output
// in the log. It is the one place the Go side runs the browser suite, so the flags are here alone.
// grep is never anchored: Playwright matches it against project, file and title joined by spaces.
func playwright(t *testing.T, grep string) {
	t.Helper()
	cmd := exec.Command("npx", "playwright", "test", "--project=cluster", "--grep", grep) //nolint:gosec // the harness's own runner and a case-id pattern
	cmd.Dir = "../ui"
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_NO_COPY_PROMPT=1")
	out, err := cmd.CombinedOutput()
	t.Logf("playwright --grep %q:\n%s", grep, out)
	if err != nil {
		t.Fatalf("playwright --grep %q: %v", grep, err)
	}
}

// switchFrontDoor is the test cluster's hook (README, The hook): alb or none, its output in the log.
func switchFrontDoor(t *testing.T, to string) {
	t.Helper()
	hook := os.Getenv("E2E_SWITCH_FRONT_DOOR")
	if hook == "" {
		t.Fatal("no E2E_SWITCH_FRONT_DOOR: the front door cannot be switched back (README, The hook)")
	}
	out, err := exec.Command(hook, to).CombinedOutput() //nolint:gosec // the test cluster's hook, named by the test cluster
	t.Logf("%s %s:\n%s", hook, to, out)
	if err != nil {
		t.Fatalf("%s %s: %v", hook, to, err)
	}
}

// saveChoice2 saves A2's front door as the admin: choice 2, Canyon's oauth2-proxy signing people in
// with the test cluster's OIDC client. A4's choice 1 dropped the sealed client secret, so it is sent again.
func saveChoice2(t *testing.T) {
	t.Helper()
	admin := deviceSignIn(t, t.TempDir(), "admin")
	signin := map[string]any{"issuer": os.Getenv("E2E_OIDC_ISSUER"), "client_id": os.Getenv("E2E_OIDC_CLIENT_ID"), "client_secret": os.Getenv("E2E_OIDC_CLIENT_SECRET")}
	body := map[string]any{"choice": 2, "signin": signin, "domain": os.Getenv("E2E_APPS_DOMAIN"), "form": "app.env.domain"}
	if code, out := admin.api(t, "PUT", "/api/v1/front-door", body); code != 200 {
		t.Errorf("PUT /api/v1/front-door back to choice 2: %d %s", code, out)
	}
}

// deployApp is the phase-2 deploy of another fixed name (CASES I1's e2e-peer, M8's redeploy): the
// check, every blocking finding approved as M2 does, then the deploy, exit 0 and the live line.
func deployApp(t *testing.T, alice session, app string) {
	t.Helper()
	deployDir(t, alice, app, fixture(t, alice, "f-cluster", app))
}

// deployDir checks dir, has every blocking finding approved and deploys it to preview, as app.
func deployDir(t *testing.T, alice session, app, dir string) {
	t.Helper()
	run, code := alice.cliJSON(t, dir, "check")
	if code != 0 && code != 2 {
		t.Fatalf("canyon --format json check (%s): exit %d", app, code)
	}
	approveBlocking(t, alice, run.GateRun)
	// --force: the directory is a fresh copy, and M8's redeploy of it has no base on an app with a
	// current package (#1503); the first deploy has nothing to force past.
	deployed, code := alice.cliJSON(t, dir, "deploy", "--force")
	if code != 0 {
		t.Fatalf("canyon --format json deploy --force (%s): exit %d", app, code)
	}
	if want, got := "https://"+app+".preview."+os.Getenv("E2E_APPS_DOMAIN"), alice.liveURL(t, deployed.GateRun); got != want {
		t.Errorf("deploy.completed url (%s) = %q; want %s", app, got, want)
	}
}

// testM8 is CASES.md M8: e2e-peer redeployed under choice 1 (A4), the new pod's init containers
// canyon-identity alone, then the browser half (e2e/ui/cluster/maker.spec.ts): the gateway's sign-in
// and /whoami.
func testM8(t *testing.T, alice session) {
	before, ok := podNamed(t, "canyon-apps", "e2e-peer-preview")
	if !ok {
		t.Fatal("no pod named e2e-peer-preview-* in canyon-apps: M8 redeploys phase 2's peer")
	}
	deployApp(t, alice, "e2e-peer")
	after, ok := podNamed(t, "canyon-apps", "e2e-peer-preview")
	if !ok || after.UID == before.UID {
		t.Fatalf("e2e-peer-preview's pod after the redeploy: found %v, UID %s (before %s); want a new pod", ok, after.UID, before.UID)
	}
	if got := initContainerNames(after); !slices.Equal(got, []string{"canyon-identity"}) {
		t.Errorf("init containers = %v; want [canyon-identity] alone under choice 1", got)
	}
	playwright(t, " M8 ") // the browser half: the gateway signs alice in, /whoami, no front-door credential
}

func initContainerNames(pod corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

// TestUnitSuiteOrder is the order CASES.md fixes, checked on the step names: each pair is "before"
// then "after", and every case of wave 1 appears exactly once.
func TestUnitSuiteOrder(t *testing.T) {
	var names []string
	for _, s := range steps(&session{}, "") {
		names = append(names, s.name)
	}
	for _, pair := range [][2]string{
		{"A1-A3", "A5"}, {"A5", "M1"}, {"M1", "M2"}, {"M2", "M3"}, {"M3", "M4-M5"}, {"M3", "peer under choice 2"},
		{"M4-M5", "K1"}, {"M4-M5", "S1-S4"}, {"peer under choice 2", "I1-I2"}, {"G1", "G2"}, {"S1-S4", "M6"}, {"I1-I2", "M6"}, {"I5", "M6"},
		{"I3", "M6"}, {"P1", "M6"}, {"M6", "M7"}, {"M7", "A4"}, {"A4", "M8"}, {"M8", "M9"},
	} {
		if bi, ai := slices.Index(names, pair[0]), slices.Index(names, pair[1]); bi == -1 || ai == -1 || bi >= ai {
			t.Errorf("steps %v: want %q before %q", names, pair[0], pair[1])
		}
	}
	if names[len(names)-1] != "M9" {
		t.Errorf("the run's last step is %q; want M9, which leaves the cluster as the install made it", names[len(names)-1])
	}
	if len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
		t.Errorf("steps %v: a name repeats", names)
	}
	joined := strings.Join(names, " ")
	for _, id := range []string{"A1-A3", "A5", "M1", "M2", "M3", "G1", "G2", "M4-M5", "K1", "S1-S4", "I1-I2", "I5", "I3", "I4", "P1", "M6", "M7", "A4", "M8", "M9"} {
		if !strings.Contains(joined, id) {
			t.Errorf("steps %v: %s is missing", names, id)
		}
	}
}
