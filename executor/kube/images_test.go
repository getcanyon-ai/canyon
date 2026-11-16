package kube

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// An image setting runs everywhere its pin ran and the pin nowhere: oauth2-proxy in choice 2's pod, the
// busybox in both probe Jobs, the static busybox in a scan's feed and a database reader; and a refresh Job
// reaches every TCP port at CANYON_KUBE_SCANDB_CIDRS besides DNS and public 443.
func TestASetImageRunsEverywhereItsPinRan(t *testing.T) {
	e, req, _ := fixture(t)
	set := func(repo string) string { return "mirror.example/" + repo + "@sha256:" + strings.Repeat("6", 64) }
	e.OAuth2Proxy, e.Busybox, e.BusyboxStatic = set("oauth2-proxy"), set("busybox"), set("busybox-static")
	e.ScanDBCIDRs = []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")}
	door, doorJSON, _, err := e.check(req) // choice 2
	if err != nil {
		t.Fatal(err)
	}
	images := map[string]string{}
	for _, c := range e.deployment(req, door, doorJSON, "abcd1234", nil, nil).Spec.Template.Spec.InitContainers {
		images[c.Name] = c.Image
	}
	trivy, _ := executor.ToolOf("trivy")
	for _, j := range [][]corev1.Container{e.probeJob("target", "r", target, nil).Spec.Template.Spec.Containers, e.probeJob("prober", "r", prober, nil).Spec.Template.Spec.Containers,
		e.scanJob("trivy", "r", trivy.Image, trivy, resource.MustParse(scanDisk)).Spec.Template.Spec.InitContainers, e.readerJob("trivy", "r", trivy.Image, trivy).Spec.Template.Spec.InitContainers} {
		images[j[0].Name] = j[0].Image
	}
	if want := map[string]string{"canyon-identity": e.Identity, "oauth2-proxy": e.OAuth2Proxy, "target": e.Busybox, "prober": e.Busybox, "feed": e.BusyboxStatic, "busybox": e.BusyboxStatic}; !equality.Semantic.DeepEqual(images, want) {
		t.Errorf("images %v, want %v", images, want)
	}
	c := newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	if err := e.refreshDB(t.Context(), "grype", grype); err != nil {
		t.Fatal(err)
	}
	np, err := c.NetworkingV1().NetworkPolicies(ScanNamespace).Get(t.Context(), "canyon-scandb", metav1.GetOptions{})
	proto := corev1.ProtocolTCP
	cidr := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto}}, To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.9.0.0/16"}}}}
	if err != nil || !equality.Semantic.DeepEqual(np.Spec.Egress, []networkingv1.NetworkPolicyEgressRule{dnsEgress(), publicEgress(443), cidr}) {
		t.Fatalf("the refresh's policy %+v %v", np, err)
	}
}

// CANYON_IMAGE_TRIVY, _GRYPE and _CHECKOV run in the tool's scan Job, its database's reader and its
// refresh — never the pin (canyonv2-v1zj) — through the same executor.Configure the Docker executor
// calls, reached end to end: a scan, a scan whose database a reader answers, and a refresh. Unset leaves
// each where it was.
func TestASetScannerImageRunsInTheKubeJobsNotThePin(t *testing.T) {
	tools := []string{"trivy", "grype", "checkov"}
	pins := map[string]string{}
	for _, tool := range tools {
		before, ok := executor.ToolOf(tool)
		if !ok {
			t.Fatalf("no %s", tool)
		}
		pins[tool] = before.Image
	}
	t.Cleanup(func() {
		restore := map[string]string{}
		for _, tool := range tools {
			restore["CANYON_IMAGE_"+strings.ToUpper(tool)] = pins[tool]
		}
		if err := executor.Configure(func(k string) string { return restore[k] }); err != nil {
			t.Fatal(err)
		}
	})
	if err := executor.Configure(func(k string) string { return "" }); err != nil { // unset changes nothing
		t.Fatal(err)
	}
	for _, tool := range tools {
		if got, _ := executor.ToolOf(tool); got.Image != pins[tool] {
			t.Errorf("%s unset: %s, want the pin %s", tool, got.Image, pins[tool])
		}
	}
	mirror := func(repo string) string { return "mirror.example/" + repo + "@sha256:" + strings.Repeat("7", 64) }
	set := map[string]string{"CANYON_IMAGE_TRIVY": mirror("trivy"), "CANYON_IMAGE_GRYPE": mirror("grype"), "CANYON_IMAGE_CHECKOV": mirror("checkov")}
	if err := executor.Configure(func(k string) string { return set[k] }); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if got, ok := executor.ToolOf(tool); !ok || got.Image != mirror(tool) || strings.Contains(got.Image, pins[tool]) {
			t.Fatalf("%s: %+v, want %s", tool, got, mirror(tool))
		}
	}

	// checkov: no database, a scan alone.
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	res, err := e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil))
	if scans := jobsNamed(c, "canyon-scan-checkov-"); err != nil || res.Image != mirror("checkov") || len(scans) != 1 || scans[0].Spec.Template.Spec.Containers[0].Image != mirror("checkov") {
		t.Fatalf("checkov scan: %+v %v %+v", res, err, scans)
	}

	// trivy: its server and its client both run the set image.
	e = &Executor{}
	c = newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
	serveTrivy(t, c, e, time.Now().Add(-time.Hour))
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}
	res, err = e.Scan(t.Context(), req, scanParts(t, scanTree(t, "package-lock.json"), nil))
	if err != nil || res.Image != mirror("trivy") || res.SARIF == nil {
		t.Fatalf("trivy scan: %+v %v", res, err)
	}
	server, err := c.AppsV1().Deployments(ScanNamespace).Get(t.Context(), "canyon-trivy", metav1.GetOptions{})
	if scans := jobsNamed(c, "canyon-scan-trivy-"); err != nil || len(scans) != 1 || scans[0].Spec.Template.Spec.Containers[0].Image != mirror("trivy") || server.Spec.Template.Spec.Containers[0].Image != mirror("trivy") {
		t.Fatalf("trivy: scans %+v server %v", scans, err)
	}

	// grype: a refresh running the set image.
	e = &Executor{}
	c = newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	if err := e.refreshDB(t.Context(), "grype", grype); err != nil {
		t.Fatal(err)
	}
	if refreshes := jobsNamed(c, "canyon-scandb-grype-"); len(refreshes) != 1 || refreshes[0].Spec.Template.Spec.Containers[0].Image != mirror("grype") {
		t.Fatalf("grype refresh: %+v", refreshes)
	}
}
