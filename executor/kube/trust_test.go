package kube

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// withTrust is withEgress's request carrying egress.ca, internal/subjecthash's PrintableString CA under OpenSSL's own
// hash of it, and e reading its system roots from a file of the test's.
func withTrust(t *testing.T) (*Executor, executor.RunRequest, executor.Root, string) {
	t.Helper()
	e, req, _ := withEgress(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "subjecthash", "testdata", "printable.pem"))
	if err != nil {
		t.Fatal(err)
	}
	system, err := os.ReadFile(filepath.Join("..", "..", "internal", "subjecthash", "testdata", "email.pem")) // any parsable CA stands in for the image's roots
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := os.WriteFile(p, system, 0o600); err != nil { //nolint:gosec // the test's own temp dir
		t.Fatal(err)
	}
	e.Roots = p
	root := executor.Root{Name: "aea6fb85.0", PEM: string(raw)}
	egress := *req.Egress
	egress.CA = []executor.Root{root}
	req.Egress = &egress
	return e, req, root, string(system)
}

// contracts/executor.md, egress.ca, On Kubernetes: a /run carrying egress.ca writes the ConfigMap canyon-egress-ca —
// each root under its name, bundle.pem, and system-and-canyon.pem, the executor image's own roots then the roots —
// before the Deployment, mounts it whole and read-only at /etc/canyon-egress in app alone, and sets app's three trust
// variables; no sidecar mounts it or names them.
func TestTheEgressCAIsTheAppsAlone(t *testing.T) {
	e, req, root, system := withTrust(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	cm, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-egress-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["aea6fb85.0"] != root.PEM || cm.Data["bundle.pem"] != root.PEM || cm.Data["system-and-canyon.pem"] != system+root.PEM || len(cm.Data) != 3 {
		t.Errorf("canyon-egress-ca holds %v; want the root under its hash name, bundle.pem and system-and-canyon.pem", slices.Sorted(func(yield func(string) bool) {
			for k := range cm.Data {
				if !yield(k) {
					return
				}
			}
		}))
	}
	written := writes(c)
	if cmAt, dAt := slices.Index(written, "create/configmaps"), slices.Index(written, "create/deployments"); cmAt < 0 || dAt < cmAt {
		t.Errorf("writes %v: want canyon-egress-ca before the Deployment", written)
	}
	d, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	pod := d.Spec.Template.Spec
	var vol string
	for _, v := range pod.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == "canyon-egress-ca" {
			vol = v.Name
		}
	}
	app := pod.Containers[0]
	if !slices.ContainsFunc(app.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == vol && m.MountPath == "/etc/canyon-egress" && m.ReadOnly && m.SubPath == ""
	}) {
		t.Errorf("app mounts %+v; want canyon-egress-ca whole and read-only at /etc/canyon-egress", app.VolumeMounts)
	}
	env := values(app)
	for k, v := range map[string]string{"SSL_CERT_DIR": "/etc/ssl/certs:/etc/canyon-egress", "NODE_EXTRA_CA_CERTS": "/etc/canyon-egress/bundle.pem", "REQUESTS_CA_BUNDLE": "/etc/canyon-egress/system-and-canyon.pem"} {
		if env[k] != v {
			t.Errorf("app's %s is %q, want %q", k, env[k], v)
		}
	}
	for _, s := range pod.InitContainers {
		if slices.ContainsFunc(s.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == vol }) || values(s)["SSL_CERT_DIR"] != "" {
			t.Errorf("%s mounts the app's trust or names it: the app's alone", s.Name)
		}
	}
}

// An env naming one of the three keeps its own and gets none of the executor's; system roots with no certificate
// fail the start before anything is written; a /run without egress.ca writes no canyon-egress-ca and mounts none.
func TestTheEgressCAsEdges(t *testing.T) {
	e, req, _, _ := withTrust(t)
	req.Env["REQUESTS_CA_BUNDLE"] = "/workspace/corp.pem"
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	d, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if env := values(d.Spec.Template.Spec.Containers[0]); env["SSL_CERT_DIR"] != "" || env["NODE_EXTRA_CA_CERTS"] != "" || env["REQUESTS_CA_BUNDLE"] != "" {
		t.Errorf("env's own REQUESTS_CA_BUNDLE: the template sets %v; want none of the three, env's value from its Secret standing", env)
	}

	e, req, _, _ = withTrust(t)
	if err := os.WriteFile(e.Roots, []byte("no certificate here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err == nil || !strings.Contains(err.Error(), "holds no certificate") || len(writes(c)) != 0 {
		t.Errorf("system roots with no certificate: %v, writes %v; want the start failed, nothing written", err, writes(c))
	}

	e, req, _ = withEgress(t)
	c = cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-egress-ca", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a /run without egress.ca: canyon-egress-ca %v, want none", err)
	}
	d, _ = c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if slices.ContainsFunc(d.Spec.Template.Spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/etc/canyon-egress" }) {
		t.Error("a /run without egress.ca mounts /etc/canyon-egress")
	}
}
