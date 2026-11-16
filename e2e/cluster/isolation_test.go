//go:build cluster

package cluster

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestIsolationAndImages is I3, I4 and P1 (CASES.md, I — Isolation and security, P — Per-image
// settings): pure cluster surface, read through $E2E_KUBECONFIG, no CLI, no API and no browser.
//
// I3 and P1 must run after the maker phase's deploys exist — e2e-app (M3) and e2e-peer (the
// phase-2 deploy I1 needs, redeployed under choice 1 in M8) — never against a namespace nothing
// has deployed into yet: both fail loudly (requireNamedAppsPresent) when neither app's pod or
// NetworkPolicy is there, rather than passing because there was nothing to violate the contract.
//
// I1 and I2 are not written here: both are entirely the "app" surface (CASES.md's own table:
// "app (a page of the fixture app, read in the browser)"), calling e2e-app's `/probe` through its
// public URL — which, under choice 2, is behind oauth2-proxy and reaches nothing until a browser
// signs in through the identity provider. e2e/fixtures/gen.sh's probe.tsx says so directly: "the
// app port admits the canyon-identity sidecar alone, and it admits only a signed-in person."
// README.md: "Nothing else knows how the provider looks" — only e2e/ui's signIn (#482, not merged)
// does. Noted in the PR body.
func TestIsolationAndImages(t *testing.T) {
	t.Run("I3", testI3)
	t.Run("I4", testI4)
	t.Run("P1", testP1)
}

// requireNamedAppsPresent is the guard against I3/P1 passing by finding nothing: CASES.md names
// e2e-app and e2e-peer as the apps this phase leaves running, so a pod (or NetworkPolicy) named
// for one of them must actually be among what was read — an empty namespace is a case that ran too
// early, not a case with nothing to report.
func requireNamedAppsPresent(t *testing.T, objectNames []string, kind string, wantApps ...string) {
	t.Helper()
	for _, app := range wantApps {
		if !slices.ContainsFunc(objectNames, func(name string) bool { return strings.HasPrefix(name, app+"-") }) {
			t.Errorf("no %s named %s-* among canyon-apps' %d %ss; I3/P1 must run after the maker phase's deploys, not against an empty namespace", kind, app, len(objectNames), kind)
		}
	}
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, len(pods))
	for i, p := range pods {
		names[i] = p.Name
	}
	return names
}

func policyNames(policies []networkingv1.NetworkPolicy) []string {
	names := make([]string, len(policies))
	for i, p := range policies {
		names[i] = p.Name
	}
	return names
}

// testI3 is CASES.md I3: what contracts/executor.md's *On Kubernetes* promises, and only that,
// checked against every app pod in canyon-apps and every scan pod in canyon-scans at the moment
// this runs — a snapshot, not a record kept across the whole suite (P1's doc comment below notes
// the same gap: nothing in the merged harness, #481, keeps that record yet). Scan pods are
// transient (a Job's, cleaned up once it answers), so no non-empty assertion is made on them —
// only app pods and app NetworkPolicies, which CASES.md's phase table says must exist by now.
func testI3(t *testing.T) {
	appPods := pods(t, "canyon-apps", "")
	requireNamedAppsPresent(t, podNames(appPods), "pod", "e2e-app", "e2e-peer")
	for _, pod := range appPods {
		requireAppPodContract(t, pod)
	}
	for _, pod := range pods(t, "canyon-scans", "") {
		requireScanPodContract(t, pod)
	}
	appPolicies := networkPolicies(t, "canyon-apps", "")
	requireNamedAppsPresent(t, policyNames(appPolicies), "NetworkPolicy", "e2e-app", "e2e-peer")
	for _, netpol := range appPolicies {
		requireAppIngress(t, netpol)
	}
	for _, netpol := range networkPolicies(t, "canyon-scans", "") {
		// canyon-scandb's refresh Job has its own NetworkPolicy, with legitimate egress
		// (executor/kube/scan.go:336: DNS, TCP 443, CANYON_KUBE_SCANDB_CIDRS) — selected by its
		// spec.podSelector, never by a label on the NetworkPolicy object itself, which this suite
		// has not verified exists. Only a policy that targets canyon.scan pods is a deny-all case.
		if _, ok := netpol.Spec.PodSelector.MatchLabels["canyon.scan"]; !ok {
			continue
		}
		requireDenyAll(t, netpol)
	}
}

// requireAppPodContract is executor.md's Deployment paragraph: no service-account token, no host
// network, no host port, no shared process namespace; every container drops every capability and
// escalates no privilege.
func requireAppPodContract(t *testing.T, pod corev1.Pod) {
	t.Helper()
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("pod %s automounts a service-account token; want none", pod.Name)
	}
	if pod.Spec.HostNetwork {
		t.Errorf("pod %s runs on the host network; want none", pod.Name)
	}
	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		t.Errorf("pod %s shares its process namespace; want none", pod.Name)
	}
	for _, c := range allContainers(pod) {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				t.Errorf("pod %s container %s publishes host port %d; want none", pod.Name, c.Name, p.HostPort)
			}
		}
		requireDropAllNoEscalation(t, pod.Name, c)
	}
}

// requireScanPodContract is executor.md's /scan paragraph: user and group 65534, a read-only root
// filesystem, every capability dropped, no privilege escalation.
func requireScanPodContract(t *testing.T, pod corev1.Pod) {
	t.Helper()
	for _, c := range allContainers(pod) {
		sc := c.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 65534 || sc.RunAsGroup == nil || *sc.RunAsGroup != 65534 {
			t.Errorf("scan pod %s container %s does not run as 65534:65534", pod.Name, c.Name)
		}
		if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("scan pod %s container %s has no read-only root filesystem", pod.Name, c.Name)
		}
		requireDropAllNoEscalation(t, pod.Name, c)
	}
}

func requireDropAllNoEscalation(t *testing.T, pod string, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	dropsAll := sc != nil && sc.Capabilities != nil && containsCapability(sc.Capabilities.Drop, "ALL")
	if !dropsAll {
		t.Errorf("pod %s container %s does not drop capability ALL", pod, c.Name)
	}
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("pod %s container %s allows privilege escalation", pod, c.Name)
	}
}

func containsCapability(caps []corev1.Capability, want corev1.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func allContainers(pod corev1.Pod) []corev1.Container {
	return append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
}

// requireAppIngress is I3's NetworkPolicy line: an app's ingress admits TCP 4180 from
// canyon.front-door pods alone (executor.md, the NetworkPolicy paragraph).
func requireAppIngress(t *testing.T, netpol networkingv1.NetworkPolicy) {
	t.Helper()
	if len(netpol.Spec.Ingress) != 1 {
		t.Errorf("NetworkPolicy %s has %d ingress rules; want exactly 1", netpol.Name, len(netpol.Spec.Ingress))
		return
	}
	rule := netpol.Spec.Ingress[0]
	tcp := rule.Ports != nil && len(rule.Ports) == 1 && rule.Ports[0].Port != nil && rule.Ports[0].Port.IntValue() == 4180 &&
		(rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol == corev1.ProtocolTCP) // Protocol nil defaults to TCP; naming it survives a later UDP rule being added
	if !tcp {
		t.Errorf("NetworkPolicy %s ingress port(s) = %v; want exactly TCP 4180", netpol.Name, rule.Ports)
	}
	if len(rule.From) != 1 || rule.From[0].PodSelector == nil || rule.From[0].PodSelector.MatchLabels["canyon.front-door"] == "" {
		t.Errorf("NetworkPolicy %s ingress admits %v; want canyon.front-door pods alone", netpol.Name, rule.From)
	}
}

// requireDenyAll is I3's scan-Job line: the NetworkPolicy selecting canyon.scan pods admits
// nothing in or out. An empty rule list denies a direction only when policyTypes names it — the
// API server's default for a policy with no egress section at all is policyTypes: [Ingress]
// alone, which leaves egress completely unrestricted while len(Spec.Egress) == 0 reads as "deny
// all" if policyTypes is never checked (as #479's deploy_test.go already checks it).
func requireDenyAll(t *testing.T, netpol networkingv1.NetworkPolicy) {
	t.Helper()
	if !slices.Contains(netpol.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || !slices.Contains(netpol.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Errorf("scan NetworkPolicy %s policyTypes = %v; want both Ingress and Egress named, else the direction left out is unrestricted, not denied", netpol.Name, netpol.Spec.PolicyTypes)
	}
	if len(netpol.Spec.Ingress) != 0 || len(netpol.Spec.Egress) != 0 {
		t.Errorf("scan NetworkPolicy %s has ingress=%v egress=%v; want neither", netpol.Name, netpol.Spec.Ingress, netpol.Spec.Egress)
	}
}

func networkPolicies(t *testing.T, namespace, selector string) []networkingv1.NetworkPolicy {
	t.Helper()
	list, err := kubectl(t).NetworkingV1().NetworkPolicies(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("listing NetworkPolicies in %s: %v", namespace, err)
	}
	return list.Items
}

// testI4 is CASES.md I4, run here exactly as executor/kube/kind_test.go's TestKindExecutorReadsNoSecret
// checks it against kind, using this suite's own can() (kube_test.go): every one of these five
// verbs on secrets, in every one of the four namespaces, must be refused.
func testI4(t *testing.T) {
	const sa = "canyon:canyon-executor"
	for _, ns := range []string{"canyon", "canyon-apps", "canyon-builds", "canyon-scans"} {
		for _, verb := range []string{"get", "list", "watch", "patch", "update"} {
			if can(t, sa, ns, verb, "", "secrets") {
				t.Errorf("canyon-executor may %s secrets in %s; want status.allowed=false", verb, ns)
			}
		}
	}
}

// imageSetting is P1's env var name for each Canyon-run image the Kubernetes executor reads
// (contracts/executor.md, Images and database sources; cmd/canyon-executor-kube/main.go executorOf,
// and executor.Configure for the three scanners, #508). Every one but CANYON_IMAGE_IDENTITY is
// optional there — unset is the release's pin — so P1 checks each that is set, and requires the
// identity's alone.
var imageSetting = []string{
	"CANYON_IMAGE_IDENTITY", "CANYON_IMAGE_OAUTH2_PROXY", "CANYON_IMAGE_TRIVY", "CANYON_IMAGE_GRYPE",
	"CANYON_IMAGE_CHECKOV", "CANYON_IMAGE_OPENGREP", "CANYON_IMAGE_BUSYBOX", "CANYON_IMAGE_BUSYBOX_STATIC",
}

// releasePin is what an unset optional setting means for the images P1 can see on a pod: the
// executor's own constants (executor/kube/objects.go oauth2Proxy).
const releasePinOAuth2Proxy = "quay.io/oauth2-proxy/oauth2-proxy@sha256:b1b2021fe8f4004573e8d690dec6c7bb29cc44364572cf8510a05bf3a0ae2ded"

// digestPinned is the shape every value of imageSetting must have: <registry>/<repo>@sha256:<hex>,
// never a tag (CASES.md P1). The registry may carry a port (localhost:5000/canyon/…, a mirror);
// only that first, host segment may hold a colon — a colon in any later segment is a tag, which is
// exactly what this must refuse.
var digestPinned = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]+)?(/[a-zA-Z0-9._-]+)+@sha256:[0-9a-f]{64}$`)

// testP1 is CASES.md P1: the executor Deployment's CANYON_IMAGE_* settings, and that the
// canyon-identity init container of every app pod runs CANYON_IMAGE_IDENTITY exactly, oauth2-proxy
// (under choice 2) runs CANYON_IMAGE_OAUTH2_PROXY.
//
// "The harness records each pod it sees during the run and checks at the end" (CASES.md, P1): the
// merged harness (#481) keeps no such record, and this test has none of its own — a probe Job (A2)
// or a scan Job's tool container is gone long before this runs, in a later phase, so that half of
// P1 (every scan Job's tool image, the A2 probe Jobs' CANYON_IMAGE_BUSYBOX) is not checked here.
// Noted in the PR body.
func testP1(t *testing.T) {
	env := executorImageEnv(t)
	if v := env["CANYON_IMAGE_IDENTITY"]; v == "" {
		t.Errorf("the executor Deployment's env carries no CANYON_IMAGE_IDENTITY: the executor does not start without it (contracts/executor.md)")
	}
	for _, name := range imageSetting {
		if v, ok := env[name]; ok && v != "" && !digestPinned.MatchString(v) {
			t.Errorf("%s = %q; want <registry>/<repo>@sha256:<hex>, never a tag", name, v)
		}
	}
	if env["CANYON_IMAGE_OAUTH2_PROXY"] == "" {
		env["CANYON_IMAGE_OAUTH2_PROXY"] = releasePinOAuth2Proxy // unset: the release's pin, which the pods must then run
	}
	appPods := pods(t, "canyon-apps", "")
	requireNamedAppsPresent(t, podNames(appPods), "pod", "e2e-app", "e2e-peer")
	for _, pod := range appPods {
		requireSidecarImages(t, pod, env)
	}
}

func executorImageEnv(t *testing.T) map[string]string {
	t.Helper()
	dep, err := kubectl(t).AppsV1().Deployments("canyon").Get(context.Background(), "canyon-executor", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Deployment canyon-executor in canyon: %v", err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("Deployment canyon-executor has no containers")
	}
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// requireSidecarImages is P1's per-app-pod line: canyon-identity runs CANYON_IMAGE_IDENTITY
// exactly; under choice 2, oauth2-proxy (also present) runs CANYON_IMAGE_OAUTH2_PROXY.
func requireSidecarImages(t *testing.T, pod corev1.Pod, env map[string]string) {
	t.Helper()
	for _, c := range pod.Spec.InitContainers {
		switch c.Name {
		case "canyon-identity":
			if c.Image != env["CANYON_IMAGE_IDENTITY"] {
				t.Errorf("pod %s canyon-identity image = %q; want %q", pod.Name, c.Image, env["CANYON_IMAGE_IDENTITY"])
			}
		case "oauth2-proxy":
			if c.Image != env["CANYON_IMAGE_OAUTH2_PROXY"] {
				t.Errorf("pod %s oauth2-proxy image = %q; want %q", pod.Name, c.Image, env["CANYON_IMAGE_OAUTH2_PROXY"])
			}
		}
	}
}

// TestUnitDigestPinned is the one piece of logic in this file: an image reference passes only as
// <registry>/<repo>@sha256:<64 hex>, never a bare digest, a tag, or a tag beside a digest.
func TestUnitDigestPinned(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	for _, want := range []struct {
		ref string
		ok  bool
	}{
		{"123456789012.dkr.ecr.eu-central-1.amazonaws.com/canyon/canyon-identity@sha256:" + hex64, true},
		{"registry/repo@sha256:" + hex64, true},
		{"localhost:5000/canyon/canyon-identity@sha256:" + hex64, true}, // a registry mirror with a port
		{"registry/repo:latest", false},                                 // a tag, never a digest
		{"registry/repo:latest@sha256:" + hex64, false},                 // a tag beside a digest
		{"sha256:" + hex64, false},                                      // no registry/repo
		{"registry/repo@sha256:" + hex64[:63], false},                   // short digest
		{"registry/repo@sha256:" + hex64 + "0", false},                  // long digest
		{"registry:5000/repo:latest@sha256:" + hex64, false},            // a tag on the repo, even with a ported registry
		{"", false},
	} {
		if got := digestPinned.MatchString(want.ref); got != want.ok {
			t.Errorf("digestPinned.MatchString(%q) = %v; want %v", want.ref, got, want.ok)
		}
	}
}
