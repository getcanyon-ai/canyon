//go:build cluster

package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSecretsAndScanners is K1 (Secrets) and G1, G2 (Scanners), CASES.md. aliceSignsIn
// (signin_test.go, the shared harness file) drives M1's own device flow to get a session; the
// confirmation happens in a browser, e2e/ui, the Playwright harness (#482), not merged.
func TestSecretsAndScanners(t *testing.T) {
	alice := aliceSignsIn(t, t.TempDir())
	t.Run("K1", func(t *testing.T) { testK1(t, alice) })
	t.Run("G1", func(t *testing.T) { testG1(t, alice) })
	t.Run("G2", func(t *testing.T) { testG2(t, alice) })
}

// randomValue is a fresh secret value K1 sets and rotates: hex, so it never collides with a
// digest, a header name or anything else the grep would otherwise have to distinguish it from.
func randomValue() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// testK1 is CASES.md K1: set and rotate API_TOKEN in prod, twice, and prove the value appears
// nowhere but the vault. K1 sets up its own state — deploy, approve the fixture's planted
// findings (approveBlocking, maker_test.go), promote — rather than depending on the maker phase's
// M3/M5 to have already run in a different test: TestSecretsAndScanners is its own top-level
// test, proven independently of TestMaker.
//
// The app-page assertion — prod /secret showing the digest — needs a browser signed in through
// e2e-app's own front door (e2e/fixtures/gen.sh probe.tsx: "it admits only a signed-in person";
// README.md: "Nothing else knows how the provider looks") and is not written here; noted in the
// PR body.
func testK1(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	t.Cleanup(func() { alice.forget(t, "e2e-app") })
	run, code := alice.cliJSON(t, dir, "check")
	if code != 0 && code != 2 {
		t.Fatalf("canyon --format json check (K1's own setup): exit %d; want 0 or 2", code)
	}
	approveBlocking(t, alice, run.GateRun)
	if out, code := alice.canyon(t, dir, "", "--format", "json", "deploy"); code != 0 {
		t.Fatalf("canyon --format json deploy (K1's own setup): exit %d\n%s", code, out)
	}
	if out, code := alice.canyon(t, dir, "", "deploy", "--env", "prod", "--yes"); code != 0 {
		t.Fatalf("canyon deploy --env prod --yes (K1's own setup: prod must exist before rotating a prod secret): exit %d\n%s", code, out)
	}
	rotations(t, alice, func(*testing.T) {})
}

// rotations is K1's step on an e2e-app already promoted (M5's state in TestSuite, testK1's own
// above): two rotations, then the secrets row. It cleans nothing up: in the suite the app is
// still S1's, I1's and M6's. after runs once per rotation with the value in E2E_K1_VALUE -- the
// browser's K1 (e2e/ui/cluster/secrets.spec.ts), which reads prod's /secret for its digest.
func rotations(t *testing.T, alice session, after func(t *testing.T)) {
	t.Helper()
	dir := fixture(t, alice, "f-cluster", "e2e-app")
	for range 2 {
		rotate(t, alice, dir)
		after(t)
	}
	row := alice.secretOf(t, "e2e-app", "prod", "API_TOKEN")
	if row == nil || row.Origin != "user" {
		t.Fatalf("secrets row for API_TOKEN · prod: got %+v; want origin \"user\"", row)
	}
}

// k1Values is every value K1 set this run, for main_test.go's end-of-run grep: t.Setenv is
// undone when the test ends, and both values must stay out of every line, the browser's included.
var k1Values []string

// rotate is one turn of K1: a fresh value, `canyon secret set --env prod API_TOKEN`, `canyon
// promote --yes`, then the nowhere-else grep across every place CASES.md K1 names.
func rotate(t *testing.T, alice session, dir string) {
	t.Helper()
	value, err := randomValue()
	if err != nil {
		t.Fatalf("making a fresh secret value: %v", err)
	}
	// Handed to the browser's K1 through the environment, never argv; the end-of-run grep
	// (main_test.go secretVars) reads the same variable, so the value may appear in no output.
	t.Setenv("E2E_K1_VALUE", value)
	k1Values = append(k1Values, value)
	if _, code := alice.canyon(t, dir, value+"\n", "secret", "set", "--env", "prod", "API_TOKEN"); code != 0 {
		t.Fatalf("canyon secret set --env prod API_TOKEN: exit %d", code)
	}
	out, code := alice.canyon(t, dir, "", "deploy", "--env", "prod", "--yes")
	if code != 0 {
		t.Fatalf("canyon deploy --env prod --yes: exit %d\n%s", code, out)
	}
	requireSecretNowhereElse(t, alice, value, out)
}

// leakSources is every place K1's grep checks, already read — the impure half
// (requireSecretNowhereElse) does the reading and the failing on a read that didn't happen at
// all; leakScan, a pure function, only searches what it's handed, so it can be unit-tested without
// a cluster (a check whose only value is going red needs something that has actually seen it go
// red).
type leakSources struct {
	PodSpecs        map[string][]byte // pod name -> its JSON spec
	AppLogPod       string            // the pod AppLog was read from
	AppLog          []byte
	Events          map[string]string            // event name -> message
	ConfigMapData   map[string]map[string]string // ConfigMap name -> key -> value
	ConfigMapBinary map[string]map[string][]byte // ConfigMap name -> key -> value (BinaryData)
	Timeline        []byte
	SecretsBody     []byte // GET .../secrets, already asserted 200
	AuditBody       []byte // GET /api/v1/audit, already asserted 200
}

// leakScan answers where value appears among src, one entry per hit, naming what it found and
// where — empty means no leak. It never reads the cluster or the network itself: every source is
// handed to it already read, which is what makes it fast to plant a value in and unit-test.
func leakScan(src leakSources, value string) []string {
	needle := []byte(value)
	var hits []string
	for name, spec := range src.PodSpecs {
		if bytes.Contains(spec, needle) {
			hits = append(hits, fmt.Sprintf("the pod spec of %s", name))
		}
	}
	if bytes.Contains(src.AppLog, needle) {
		hits = append(hits, fmt.Sprintf("the app container log of %s", src.AppLogPod))
	}
	for name, msg := range src.Events {
		if bytes.Contains([]byte(msg), needle) {
			hits = append(hits, fmt.Sprintf("event %s in canyon-apps", name))
		}
	}
	for cm, kv := range src.ConfigMapData {
		for key := range kv {
			if bytes.Contains([]byte(kv[key]), needle) {
				hits = append(hits, fmt.Sprintf("ConfigMap %s key %s (data) in canyon-apps", cm, key))
			}
		}
	}
	for cm, kv := range src.ConfigMapBinary {
		for key := range kv {
			if bytes.Contains(kv[key], needle) {
				hits = append(hits, fmt.Sprintf("ConfigMap %s key %s (binaryData) in canyon-apps", cm, key))
			}
		}
	}
	if bytes.Contains(src.Timeline, needle) {
		hits = append(hits, "the promote run's timeline")
	}
	if bytes.Contains(src.SecretsBody, needle) {
		hits = append(hits, "GET /api/v1/projects/e2e-app/secrets")
	}
	if bytes.Contains(src.AuditBody, needle) {
		hits = append(hits, "the audit page for e2e-app")
	}
	return hits
}

// requireSecretNowhereElse is K1's grep: value must appear in none of the places CASES.md names.
// Every read that fails to run at all is a hard t.Fatalf — a grep that silently didn't run is not
// a grep that passed — and both API reads assert 200 before their body is searched: a 403, 404 or
// 500 answers a body that cannot hold the value, which would otherwise read as "no leak here."
// timeline is the promote's own captured output — the CLI's live rendering of the run's timeline
// (§5.4), the same lines the server's timeline holds, since `canyon deploy --env prod --yes` (as
// CASES.md words it, not `--format json`) prints no run id to poll separately. The Secret object itself is
// never read — the read-only kubeconfig cannot (README, The read-only kubeconfig).
func requireSecretNowhereElse(t *testing.T, alice session, value, timeline string) {
	t.Helper()
	src := leakSources{PodSpecs: map[string][]byte{}, Events: map[string]string{}, ConfigMapData: map[string]map[string]string{}, ConfigMapBinary: map[string]map[string][]byte{}, Timeline: []byte(timeline)}
	for _, pod := range pods(t, "canyon-apps", "") {
		spec, err := json.Marshal(pod)
		if err != nil {
			t.Fatalf("marshalling pod %s's spec: %v", pod.Name, err)
		}
		src.PodSpecs[pod.Name] = spec
	}
	pod, ok := podNamed(t, "canyon-apps", "e2e-app-prod")
	if !ok {
		t.Fatalf("no pod named e2e-app-prod-* in canyon-apps: cannot grep its log")
	}
	log, err := kubectl(t).CoreV1().Pods("canyon-apps").GetLogs(pod.Name, &corev1.PodLogOptions{Container: "app"}).DoRaw(t.Context())
	if err != nil {
		t.Fatalf("reading the app container log of %s: %v", pod.Name, err)
	}
	src.AppLogPod, src.AppLog = pod.Name, log
	events, err := kubectl(t).CoreV1().Events("canyon-apps").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing events in canyon-apps: %v", err)
	}
	for _, e := range events.Items {
		src.Events[e.Name] = e.Message
	}
	for _, cm := range configMaps(t, "canyon-apps", "") {
		src.ConfigMapData[cm.Name] = cm.Data
		src.ConfigMapBinary[cm.Name] = cm.BinaryData
	}
	if code, body := alice.api(t, "GET", "/api/v1/projects/e2e-app/secrets", nil); code != 200 {
		t.Fatalf("GET /api/v1/projects/e2e-app/secrets: %d %s; want 200 before searching its body", code, body)
	} else {
		src.SecretsBody = body
	}
	if code, body := alice.api(t, "GET", "/api/v1/audit?project=e2e-app", nil); code != 200 {
		t.Fatalf("GET /api/v1/audit?project=e2e-app: %d %s; want 200 before searching its body", code, body)
	} else {
		src.AuditBody = body
	}
	if hits := leakScan(src, value); len(hits) > 0 {
		t.Errorf("the rotated secret value leaked: %s", strings.Join(hits, "; "))
	}
}

// watchJobsDuring polls Jobs in namespace matching selector every 500ms while work runs, and
// answers whether at least one was ever seen — G1 and G2 both need to prove a Job existed
// mid-check, not just that one exists (by then gone) or never ran at all. kubectl(t) is read on
// the test's own goroutine before the poller starts: t.Fatal (inside kubectl/jobs) is unsafe to
// call from any other goroutine, so the poller lists directly and only records what it saw.
func watchJobsDuring(t *testing.T, namespace, selector string, work func()) bool {
	t.Helper()
	kube := kubectl(t)
	stop := make(chan struct{})
	seen := make(chan bool, 1)
	go func() {
		var found bool
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				seen <- found
				return
			case <-ticker.C:
				if list, err := kube.BatchV1().Jobs(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector}); err == nil && len(list.Items) > 0 {
					found = true
				}
			}
		}
	}()
	work()
	close(stop)
	return <-seen
}

// underPolicy is whether a finding's rule is the policy's: the policy itself, or a tool's result
// the policy table maps to it, <policy>.<tool>.<ruleId> (contracts/finding.md).
func underPolicy(rule, policy string) bool {
	return rule == policy || strings.HasPrefix(rule, policy+".")
}

// testG1 is CASES.md G1: a known-vulnerable dependency, on the trivy database M2's first scan
// downloaded.
//
// enableTool sets one of the executor's tools for the install, as the admin (contracts/routes.md,
// PUT /api/v1/policies/:rule): advisory, run on this host, with these settings. A fresh install
// runs every tool already (gate/policy/canonical.go); the settings are what a case needs. The
// change bites the next run.
func enableTool(t *testing.T, rule string, settings map[string]any) {
	t.Helper()
	admin := deviceSignIn(t, t.TempDir(), "admin")
	body := map[string]any{"state": "advisory", "run": true, "settings": settings}
	if code, out := admin.api(t, "PUT", "/api/v1/policies/"+rule+"?apply=true", body); code != 200 {
		t.Fatalf("PUT /api/v1/policies/%s?apply=true: %d %s", rule, code, out)
	}
}

func testG1(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-vuln", "e2e-vuln")
	t.Cleanup(func() { alice.forget(t, "e2e-vuln") })
	enableTool(t, "sarif.trivy", map[string]any{"scanners": []string{"vuln"}})
	run, code := alice.cliJSON(t, dir, "check")
	if code != 0 {
		t.Fatalf("canyon --format json check: exit %d", code)
	}
	if run.Result != "passed" {
		t.Fatalf("check result = %q; want %q", run.Result, "passed")
	}
	want := map[string]bool{"lodash": false, "xlsx": false}
	for _, f := range run.Findings {
		if !underPolicy(f.Rule, "deps.known-vuln") || f.File != "package-lock.json" || f.Blocks {
			continue
		}
		for pkg := range want {
			if strings.Contains(f.Problem, pkg) {
				want[pkg] = true
			}
		}
	}
	for pkg, seen := range want {
		if !seen {
			t.Errorf("no non-blocking deps.known-vuln finding at package-lock.json names %s; findings: %+v", pkg, run.Findings)
		}
	}
}

// testG2 is CASES.md G2: a planted misconfiguration, on the database G1 already downloaded. Given
// G1 ran first (TestSecretsAndScanners's own ordering).
func testG2(t *testing.T, alice session) {
	dir := fixture(t, alice, "f-cp", "e2e-cp")
	t.Cleanup(func() { alice.forget(t, "e2e-cp") })
	enableTool(t, "sarif.checkov", map[string]any{})
	before := len(jobs(t, "canyon-scans", "canyon.scandb"))
	var run cliRun
	var code int
	sawScan := watchJobsDuring(t, "canyon-scans", "canyon.scan", func() {
		run, code = alice.cliJSON(t, dir, "check")
	})
	if code != 2 {
		t.Fatalf("canyon --format json check: exit %d; want 2 (blocked)", code)
	}
	if run.Result != "blocked" {
		t.Fatalf("check result = %q; want %q", run.Result, "blocked")
	}
	// Never a count: the real model adds advisory candidates of its own (perf.unbounded-refetch on the
	// test cluster), so G2 wants J2's findings among the run's, not the run to be J2's.
	want := map[string]bool{"secrets.client-bundle": false, "auth.unauthenticated-route": false}
	for _, f := range run.Findings {
		for policy := range want {
			want[policy] = want[policy] || underPolicy(f.Rule, policy)
		}
	}
	for rule, seen := range want {
		if !seen {
			t.Errorf("no %s finding; want one, as Docker's J2 (CASES.md G2)", rule)
		}
	}
	if !sawScan {
		t.Errorf("no canyon.scan Job was ever seen in canyon-scans while the check ran")
	}
	if after := len(jobs(t, "canyon-scans", "canyon.scandb")); after != before {
		t.Errorf("canyon.scandb jobs went from %d to %d during G2; want no new download, the database is G1's", before, after)
	}
	waitFor(t, 60*time.Second, "the canyon.scan Job to be gone after G2", func() (string, bool) {
		remaining := jobs(t, "canyon-scans", "canyon.scan")
		return fmt.Sprintf("%d canyon.scan jobs remain", len(remaining)), len(remaining) == 0
	})
}

// TestRandomValue is the one piece of logic in this file: randomValue must answer a value the
// grep can search for byte for byte (hex, so it never collides with a sha256 digest's own
// alphabet) and never answer the same value twice.
// TestUnitRandomValue is the one piece of logic in this file's first half: randomValue must
// answer a value the grep can search for byte for byte (hex, so it never collides with a sha256
// digest's own alphabet) and never answer the same value twice.
func TestUnitRandomValue(t *testing.T) {
	a, err := randomValue()
	if err != nil {
		t.Fatalf("randomValue: %v", err)
	}
	b, err := randomValue()
	if err != nil {
		t.Fatalf("randomValue: %v", err)
	}
	if a == b {
		t.Fatalf("randomValue returned %q twice", a)
	}
	if _, err := hex.DecodeString(a); err != nil || len(a) != 32 {
		t.Fatalf("randomValue() = %q; want 32 lower-case hex characters", a)
	}
}

// TestUnitLeakScanFindsAPlantedSecret is that proof: a leak check
// whose entire value is going red needs something that has actually seen it go red (#479's own
// precedent). Plants "s3cr3t" in each source leakScan reads, one at a time, and asserts it is
// found — including a ConfigMap's BinaryData, the blind spot #479 also had — plus the negative:
// nothing planted answers no hits.
func TestUnitLeakScanFindsAPlantedSecret(t *testing.T) {
	const secret = "s3cr3t"
	clean := leakSources{
		PodSpecs:        map[string][]byte{"e2e-app-prod-abc12": []byte(`{"metadata":{"name":"e2e-app-prod-abc12"}}`)},
		AppLogPod:       "e2e-app-prod-abc12",
		AppLog:          []byte("listening on :3000\n"),
		Events:          map[string]string{"e1": "Started container app"},
		ConfigMapData:   map[string]map[string]string{"canyon-access-e2e-app-prod": {"access.json": "{}"}},
		ConfigMapBinary: map[string]map[string][]byte{"canyon-access-e2e-app-prod": {"blob": []byte("\x00\x01\x02")}},
		Timeline:        []byte("live at https://e2e-app.prod.example.com"),
		SecretsBody:     []byte(`{"secrets":[{"name":"API_TOKEN","env":"prod"}]}`),
		AuditBody:       []byte(`{"events":[{"action":"deploy prod"}]}`),
	}
	if hits := leakScan(clean, secret); len(hits) != 0 {
		t.Fatalf("leakScan(clean, %q) = %v; want no hits when nothing carries the value", secret, hits)
	}
	plant := func(mutate func(*leakSources)) []string {
		s := clean
		s.PodSpecs = map[string][]byte{"e2e-app-prod-abc12": append([]byte{}, clean.PodSpecs["e2e-app-prod-abc12"]...)}
		s.Events = map[string]string{"e1": clean.Events["e1"]}
		s.ConfigMapData = map[string]map[string]string{"canyon-access-e2e-app-prod": {"access.json": clean.ConfigMapData["canyon-access-e2e-app-prod"]["access.json"]}}
		s.ConfigMapBinary = map[string]map[string][]byte{"canyon-access-e2e-app-prod": {"blob": append([]byte{}, clean.ConfigMapBinary["canyon-access-e2e-app-prod"]["blob"]...)}}
		mutate(&s)
		return leakScan(s, secret)
	}
	for name, mutate := range map[string]func(*leakSources){
		"a pod spec":               func(s *leakSources) { s.PodSpecs["e2e-app-prod-abc12"] = []byte(secret) },
		"the app log":              func(s *leakSources) { s.AppLog = []byte(secret) },
		"an event message":         func(s *leakSources) { s.Events["e1"] = secret },
		"a ConfigMap's data":       func(s *leakSources) { s.ConfigMapData["canyon-access-e2e-app-prod"]["access.json"] = secret },
		"a ConfigMap's binaryData": func(s *leakSources) { s.ConfigMapBinary["canyon-access-e2e-app-prod"]["blob"] = []byte(secret) },
		"the timeline":             func(s *leakSources) { s.Timeline = []byte(secret) },
		"the secrets body":         func(s *leakSources) { s.SecretsBody = []byte(secret) },
		"the audit body":           func(s *leakSources) { s.AuditBody = []byte(secret) },
	} {
		if hits := plant(mutate); len(hits) == 0 {
			t.Errorf("planting the secret in %s: leakScan found nothing; want at least one hit", name)
		}
	}
}
