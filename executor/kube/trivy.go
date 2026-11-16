package kube

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/getcanyon-ai/canyon/executor"
)

// trivy's server (contracts/executor.md, On Kubernetes; #1652): one `trivy server` holds trivy's database,
// and every trivy scan is a client of it, side by side on any node, with no volume and no lock.
const (
	trivyName   = "canyon-trivy"
	trivyLabel  = "canyon.trivy"
	clientLabel = "canyon.scan-trivy" // a trivy scan's pod: it reaches DNS and the server, nothing else
	trivyPort   = 4954
	trivyURL    = "http://canyon-trivy.canyon-scans:4954"
	// trivyMade is how long a server made or updated has to have a ready pod: past it, with none, trivy
	// cannot check (db_missing), not a wait.
	trivyMade = 15 * time.Minute

	madeAt = "canyon.trivy/made" // when the executor last made or updated the Deployment
	specAt = "canyon.trivy/spec" // a digest of the spec it was made with: another release's or image's is updated
)

// trivyServer makes trivy's server, its Service and its NetworkPolicies, or updates the Deployment when its
// image or database source differs from this executor's, and answers its ready pod: nil and errBusy while
// it has none within trivyMade of its making, nil and errNoServer past it.
func (e *Executor) trivyServer(ctx context.Context, image string) (*corev1.Pod, error) {
	source := dbSource()
	if err := e.serverPolicies(ctx); err != nil {
		return nil, err
	}
	if err := e.trivyService(ctx); err != nil {
		return nil, err
	}
	deploys := e.Client.AppsV1().Deployments(ScanNamespace)
	cur, err := deploys.Get(ctx, trivyName, metav1.GetOptions{})
	want := trivyDeployment(image, source, time.Now())
	switch {
	case apierrors.IsNotFound(err):
		if _, err := deploys.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("deployment %s: %w", trivyName, err)
		}
		return nil, executor.ErrBusy
	case err != nil:
		return nil, fmt.Errorf("deployment %s: %w", trivyName, err)
	case cur.Annotations[specAt] != want.Annotations[specAt]:
		cur.Annotations, cur.Spec = want.Annotations, want.Spec
		if _, err := deploys.Update(ctx, cur, metav1.UpdateOptions{}); err != nil && !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("deployment %s: %w", trivyName, err)
		}
		return nil, executor.ErrBusy
	}
	pods, err := e.Client.CoreV1().Pods(ScanNamespace).List(ctx, metav1.ListOptions{LabelSelector: trivyLabel + "=true"})
	if err != nil {
		return nil, fmt.Errorf("the pods of %s: %w", trivyName, err)
	}
	since, _ := time.Parse(time.RFC3339, cur.Annotations[madeAt])
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if ready(p) {
			return p, nil
		}
		if born := p.CreationTimestamp.Time; born.After(since) { // a pod a node's loss replaced downloads afresh: its 15 minutes are its own
			since = born
		}
	}
	if time.Since(since) < trivyMade || len(pods.Items) == 0 {
		return nil, executor.ErrBusy
	}
	return nil, errNoServer
}

// restarts is how often a pod's containers have restarted: a server restarted under a scan lost its
// requests.
func restarts(p *corev1.Pod) int32 {
	var n int32
	for _, s := range p.Status.ContainerStatuses {
		n += s.RestartCount
	}
	return n
}

// errNoServer is trivy's server with no ready pod trivyMade after it was made or updated: db_missing.
var errNoServer = fmt.Errorf("%s has no ready pod %s after it was made", trivyName, trivyMade)

func ready(p *corev1.Pod) bool {
	return slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// dbSource is CANYON_SCANDB_TRIVY as Configure gave trivy's refresh, or "" for trivy's own sources.
func dbSource() string {
	t, _ := executor.ToolOf("trivy")
	if i := slices.Index(t.DB.Refresh, "--db-repository"); i >= 0 && i+1 < len(t.DB.Refresh) {
		return t.DB.Refresh[i+1]
	}
	return ""
}

// trivyDeployment is trivy's server: one replica, recreated rather than rolled so no old server outlives
// its update, the scan's trivy image, as the scan's user with a read-only root filesystem, its database
// and /tmp each an emptyDir, ready once it answers /healthz — which trivy serves only once its database
// is in.
func trivyDeployment(image, source string, now time.Time) *appsv1.Deployment {
	args := []string{"server", "--listen", "0.0.0.0:" + strconv.Itoa(trivyPort), "--cache-dir", "/db"} // no scan flag: trivy server takes none (#1685)
	if source != "" {
		args = append(args, "--db-repository", source)
	}
	nobody := int64(65534)
	labels := map[string]string{trivyLabel: "true"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: trivyName, Namespace: ScanNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
					SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "trivy", Image: image, Args: args,
						Ports:          []corev1.ContainerPort{{ContainerPort: trivyPort}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(trivyPort)}}},
						VolumeMounts:   []corev1.VolumeMount{{Name: "db", MountPath: "/db"}, {Name: "tmp", MountPath: "/tmp"}},
						Resources:      resources("10m", "128Mi", "1", "1Gi"), // measured: idle at 0% and 76 Mi; ten scans at once, 160 ms of CPU (#1685)
						SecurityContext: &corev1.SecurityContext{RunAsUser: &nobody, RunAsGroup: &nobody, RunAsNonRoot: new(true), ReadOnlyRootFilesystem: new(true),
							AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
					Volumes: []corev1.Volume{
						{Name: "db", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("5Gi"))}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("2Gi"))}}}, // the hourly download lands here first
					},
				},
			},
		},
	}
	spec, _ := json.Marshal(d.Spec)
	sum := sha256.Sum256(spec)
	d.Annotations = map[string]string{madeAt: now.UTC().Format(time.RFC3339), specAt: hex.EncodeToString(sum[:])}
	return d
}

// trivyService is the server's address, TCP 4954 to its pods.
func (e *Executor) trivyService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: trivyName, Namespace: ScanNamespace},
		Spec: corev1.ServiceSpec{Selector: map[string]string{trivyLabel: "true"},
			Ports: []corev1.ServicePort{{Port: trivyPort, TargetPort: intstr.FromInt32(trivyPort)}}},
	}
	if _, err := e.Client.CoreV1().Services(ScanNamespace).Get(ctx, trivyName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := e.Client.CoreV1().Services(ScanNamespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("service %s: %w", trivyName, err)
		}
	} else if err != nil {
		return fmt.Errorf("service %s: %w", trivyName, err)
	}
	return nil
}

// serverPolicies are the server's NetworkPolicy — in from trivy's scan pods on 4954 alone, out where a
// refresh reaches — and the one letting those scan pods out to DNS and the server, nothing else.
func (e *Executor) serverPolicies(ctx context.Context) error {
	server := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{trivyLabel: "true"}}}
	if err := e.policy(ctx, "canyon-scan-trivy", clientLabel, dnsEgress(), networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{server}, Ports: []networkingv1.NetworkPolicyPort{tcp(trivyPort)}}); err != nil {
		return err
	}
	return e.policyWith(ctx, trivyName, trivyLabel, []networkingv1.NetworkPolicyIngressRule{{
		From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{clientLabel: "true"}}}},
		Ports: []networkingv1.NetworkPolicyPort{tcp(trivyPort)},
	}, {
		From: []networkingv1.NetworkPolicyPeer{{ // the executor, asking /version how old the database is (#2743)
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "canyon"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "canyon-executor"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcp(trivyPort)},
	}}, e.refreshEgress()...)
}

// serverBuilt is when the database trivy's server holds was built: VulnerabilityDB.UpdatedAt of its GET /version,
// which trivy 0.74.0 reads from its metadata.json (pkg/version), so no pods/exec reads a timestamp (#2743). Zero
// when the server answers no database.
func (e *Executor) serverBuilt(ctx context.Context) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trivyURL+"/version", nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := (&http.Client{Transport: cmp.Or(e.Trivy, http.DefaultTransport), Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s's /version: %w", trivyName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("%s's /version answered %d", trivyName, resp.StatusCode)
	}
	var v struct {
		VulnerabilityDB *struct{ UpdatedAt time.Time }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return time.Time{}, fmt.Errorf("%s's /version: %w", trivyName, err)
	}
	if v.VulnerabilityDB == nil {
		return time.Time{}, nil
	}
	return v.VulnerabilityDB.UpdatedAt, nil
}
