package k8s_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// egressControl is the gateway pod's egress-control container, its second, beside Envoy.
func egressControl(t *testing.T, d *appsv1.Deployment) corev1.Container {
	t.Helper()
	if cs := d.Spec.Template.Spec.Containers; len(cs) == 2 && cs[1].Name == "egress-control" {
		return cs[1]
	}
	t.Fatalf("the gateway's pod runs %d containers; want envoy, then egress-control", len(d.Spec.Template.Spec.Containers))
	return corev1.Container{}
}

// egressObjects is the gateway's ConfigMap, Deployment and NetworkPolicy, as egress.yaml writes them.
func egressObjects(t *testing.T) (*corev1.ConfigMap, *appsv1.Deployment, *networkingv1.NetworkPolicy) {
	t.Helper()
	var cm *corev1.ConfigMap
	var d *appsv1.Deployment
	var np *networkingv1.NetworkPolicy
	for _, o := range objects(t) {
		switch o := o.(type) {
		case *corev1.ConfigMap:
			if o.Name == "canyon-egress" {
				cm = o
			}
		case *appsv1.Deployment:
			if o.Name == "canyon-egress" {
				d = o
			}
		case *networkingv1.NetworkPolicy:
			if o.Name == "canyon-egress" {
				np = o
			}
		}
	}
	if cm == nil || d == nil || np == nil {
		t.Fatal("the deploy writes no ConfigMap, Deployment or NetworkPolicy canyon-egress")
	}
	return cm, d, np
}

// Canyon's egress gateway on Kubernetes runs Docker's Envoy as it is (contracts/executor.md, On Kubernetes):
// its ConfigMap holds deploy/egress's envoy.sh and envoy.yaml byte for byte, which CI's envoy --mode validate
// checks, and the pod template carries their sum with kube.sh's, so a changed bootstrap rolls Envoy.
func TestTheEgressGatewayRunsDeployEgressAsItIs(t *testing.T) {
	cm, d, _ := egressObjects(t)
	for _, f := range []string{"envoy.sh", "envoy.yaml"} {
		want, err := os.ReadFile(filepath.Join("..", "egress", f)) //nolint:gosec // a fixed path in this repository
		if err != nil {
			t.Fatal(err)
		}
		if cm.Data[f] != string(want) {
			t.Errorf("egress.yaml's %s is not deploy/egress/%s: copy it over", f, f)
		}
	}
	if cm.Data["kube.sh"] == "" || !slices.Equal(d.Spec.Template.Spec.Containers[0].Command, []string{"/bin/sh", "/etc/canyon/kube.sh"}) {
		t.Errorf("Envoy starts with %v, not kube.sh", d.Spec.Template.Spec.Containers[0].Command)
	}
	shell, err := exec.LookPath("dash") // the image's /bin/sh, which reads a quote inside ${…:?…} as one
	if err != nil {
		t.Fatal("no dash on PATH, which parses the scripts as Envoy's image will: install it (Debian's dash, macOS's /bin/dash)")
	}
	for _, f := range []string{"kube.sh", "envoy.sh"} {
		script := filepath.Join(t.TempDir(), f)
		if err := os.WriteFile(script, []byte(cm.Data[f]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(shell, "-n", script).CombinedOutput(); err != nil { //nolint:gosec // the test's own shell on a script of this repository
			t.Errorf("egress.yaml's %s does not parse: %v %s", f, err, out)
		}
	}
	sum := sha256.Sum256([]byte(cm.Data["envoy.sh"] + cm.Data["envoy.yaml"] + cm.Data["kube.sh"]))
	if want, got := hex.EncodeToString(sum[:]), d.Spec.Template.Annotations["canyon.egress-config-sha256"]; got != want {
		t.Errorf("the gateway's pod template has canyon.egress-config-sha256 %q, its config's is %q: set it, or an upgrade never reaches the running Envoy", got, want)
	}
}

// Every Envoy a rollout runs at once, two access-log streams a worker (:3128's and the interception listener's), stays
// under the receiver's streams, so a stream that drops reconnects (design/EGRESS.md §2.1).
func TestTheEgressGatewayStaysUnderTheReceiversStreams(t *testing.T) {
	_, d, _ := egressObjects(t)
	workers := 0
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "CANYON_EGRESS_CONCURRENCY" {
			workers, _ = strconv.Atoi(e.Value)
		}
	}
	surge := 0
	if r := d.Spec.Strategy.RollingUpdate; r != nil && r.MaxSurge != nil {
		surge = r.MaxSurge.IntValue()
	}
	if d.Spec.Replicas == nil || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || workers < 1 {
		t.Fatalf("the gateway runs %v replicas by %q with %d workers: name all three", d.Spec.Replicas, d.Spec.Strategy.Type, workers)
	}
	if n := (int(*d.Spec.Replicas) + surge) * workers * egresscontrol.LogsPerWorker; n >= egresscontrol.ReceiverStreams {
		t.Errorf("(%d replicas + %d surging) × %d workers × %d logs = %d streams, not under the receiver's %d", *d.Spec.Replicas, surge, workers,
			egresscontrol.LogsPerWorker, n, egresscontrol.ReceiverStreams)
	}
}

// The gateway's NetworkPolicy is the floor (design/EGRESS.md §2.2): in, 3128 from the apps' pods and the
// executor's alone, and egress-control's metrics from the scraper's namespace alone — a placeholder no namespace
// is named until the overlay names one (§7); out, DNS, the server's receiver and public 80 and 443 — never a
// private address.
func TestTheEgressGatewaysPolicyIsTheFloor(t *testing.T) {
	_, d, np := egressObjects(t)
	if sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err != nil || sel.Empty() || !sel.Matches(labels.Set(d.Spec.Template.Labels)) {
		t.Fatalf("the gateway's policy selects %v, not its pods", np.Spec.PodSelector)
	}
	if !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) || len(np.Spec.Ingress) != 3 {
		t.Fatalf("the gateway's policy: %v, %d ingress rules; want both kinds, three rules", np.Spec.PolicyTypes, len(np.Spec.Ingress))
	}
	var metrics string
	for _, e := range egressControl(t, d).Env {
		if e.Name == "CANYON_EGRESS_CONTROL_METRICS_ADDR" {
			metrics = e.Value
		}
	}
	scraper := np.Spec.Ingress[2]
	if len(scraper.From) != 1 || scraper.From[0].PodSelector != nil || scraper.From[0].IPBlock != nil || scraper.From[0].NamespaceSelector == nil ||
		len(scraper.From[0].NamespaceSelector.MatchLabels) != 1 || scraper.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "REPLACE-scraper-namespace" ||
		len(scraper.Ports) != 1 || "0.0.0.0:"+strconv.Itoa(scraper.Ports[0].Port.IntValue()) != metrics || !slices.Equal(tcpPorts(scraper.Ports), []int{7784}) {
		t.Errorf("ingress[2] admits %+v on %+v; want egress-control's metrics (%s) from the placeholder namespace the overlay replaces, alone", scraper.From, scraper.Ports, metrics)
	}
	app := labels.Set{"canyon.app": "claims-portal", "canyon.target": "prod"}
	for i, rule := range np.Spec.Ingress[:2] {
		if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 3128 || len(rule.From) != 1 {
			t.Errorf("ingress[%d] admits %+v on %+v: one peer, on 3128 alone", i, rule.From, rule.Ports)
		}
	}
	apps, executorPods := np.Spec.Ingress[0].From[0], np.Spec.Ingress[1].From[0]
	if appSel, err := metav1.LabelSelectorAsSelector(apps.PodSelector); apps.NamespaceSelector == nil || apps.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "canyon-apps" || err != nil || !appSel.Matches(app) || appSel.Matches(labels.Set{"canyon.probe": "true"}) {
		t.Errorf("ingress[0] admits %+v: the pods of canyon-apps that are an app's", apps)
	}
	if executorPods.NamespaceSelector != nil || executorPods.PodSelector == nil || executorPods.PodSelector.MatchLabels["app.kubernetes.io/name"] != "canyon-executor" {
		t.Errorf("ingress[1] admits %+v: the executor's pods, for its gateway check", executorPods)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("the gateway's policy has %d egress rules: DNS, the server, public 80 and 443", len(np.Spec.Egress))
	}
	server, public := np.Spec.Egress[1], np.Spec.Egress[2]
	if len(server.To) != 1 || server.To[0].PodSelector == nil || server.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "canyon-server" || !slices.Equal(tcpPorts(server.Ports), []int{7781, 7782}) {
		t.Errorf("egress[1] is %+v: the server's pods on the receiver's 7781 and the policy's 7782 alone", server)
	}
	var ports []int
	for _, p := range public.Ports {
		ports = append(ports, p.Port.IntValue())
	}
	// the whole list is executor/kube's to hold equal to the open policy's (TestTheOpenPolicyAndTheGatewayRefuseOneList)
	if len(public.To) != 1 || public.To[0].IPBlock == nil || public.To[0].IPBlock.CIDR != "0.0.0.0/0" ||
		slices.ContainsFunc(privateRanges, func(r string) bool { return !slices.Contains(public.To[0].IPBlock.Except, r) }) || !slices.Equal(ports, []int{80, 443}) {
		t.Errorf("egress[2] is %+v: TCP 80 and 443 at every address outside %v and the rest of Docker's list", public, privateRanges)
	}
}

// Two Envoys behind one Service, which a disruption never takes both of; and the executor reaches them on
// 3128 for its gateway check (contracts/executor.md, On Kubernetes).
func TestTheEgressGatewayIsTwoBehindItsServiceAndTheExecutorReachesIt(t *testing.T) {
	_, d, _ := egressObjects(t)
	pods := labels.Set(d.Spec.Template.Labels)
	if d.Spec.Replicas == nil || *d.Spec.Replicas < 2 {
		t.Errorf("the gateway runs %v replicas: two at least, or one node's loss leaves the apps no way out", d.Spec.Replicas)
	}
	var service, budget, executor bool
	for _, o := range objects(t) {
		switch o := o.(type) {
		case *corev1.Service:
			if o.Name == "canyon-egress" {
				service = labels.SelectorFromSet(o.Spec.Selector).Matches(pods) && len(o.Spec.Ports) == 1 && o.Spec.Ports[0].Port == 3128 && o.Spec.Ports[0].TargetPort.IntValue() == 3128
			}
		case *policyv1.PodDisruptionBudget:
			if o.Name == "canyon-egress" {
				sel, err := metav1.LabelSelectorAsSelector(o.Spec.Selector)
				budget = err == nil && sel.Matches(pods) && o.Spec.MinAvailable != nil && o.Spec.MinAvailable.IntValue() >= 1
			}
		case *networkingv1.NetworkPolicy:
			if o.Name == "canyon-executor" {
				executor = slices.ContainsFunc(o.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
					return len(r.To) == 1 && r.To[0].PodSelector != nil && labels.SelectorFromSet(r.To[0].PodSelector.MatchLabels).Matches(pods) &&
						len(r.Ports) == 1 && r.Ports[0].Port.IntValue() == 3128
				})
			}
		}
	}
	if !service || !budget || !executor {
		t.Errorf("the Service on 3128 selects the gateway's pods: %v; a PodDisruptionBudget keeps one: %v; the executor reaches them on 3128: %v", service, budget, executor)
	}
}

// egress-control runs beside Envoy in each pod, the server's image as a subcommand (design/EGRESS.md §3.1–3.2):
// ready only once it holds a policy, which an exec probe asks it, as no probe of the pod's address reaches its
// loopback listener; pulling the policy from the server's Service over TLS pinned to the published certificate,
// with a token of the pod's own ServiceAccount, canyon-egress, for the policy's audience, ten minutes long; and
// with nothing to keep a policy in. The ServiceAccount holds no Role and mounts no token of the API's own.
func TestEgressControlRunsBesideEnvoyReadyOnceItHoldsAPolicy(t *testing.T) {
	_, d, _ := egressObjects(t)
	spec := d.Spec.Template.Spec
	var server *appsv1.Deployment
	var account *corev1.ServiceAccount
	objs := objects(t)
	for _, o := range objs {
		switch o := o.(type) {
		case *appsv1.Deployment:
			if o.Name == "canyon-server" {
				server = o
			}
		case *corev1.ServiceAccount:
			if o.Name == "canyon-egress" && o.Namespace == "canyon" {
				account = o
			}
		}
	}
	if server == nil || len(spec.Containers) != 2 || spec.Containers[0].Name != "envoy" || spec.Containers[1].Name != "egress-control" {
		t.Fatalf("the gateway's pod runs %d containers; want envoy, then egress-control", len(spec.Containers))
	}
	c := spec.Containers[1]
	if c.Image != server.Spec.Template.Spec.Containers[0].Image || len(c.Command) != 0 || !slices.Equal(c.Args, []string{"egress-control"}) {
		t.Errorf("egress-control runs %s %v %v; want the server's image, %s, with the argument egress-control", c.Image, c.Command, c.Args, server.Spec.Template.Spec.Containers[0].Image)
	}
	if p := c.ReadinessProbe; p == nil || p.Exec == nil || !slices.Equal(p.Exec.Command, []string{"/canyon-server", "egress-control", "probe"}) {
		t.Errorf("egress-control's readiness is %+v; want the exec probe canyon-server egress-control probe", p)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["CANYON_EGRESS_POLICY_URL"] != "https://canyon-server.canyon.svc:7782/egress/policy" {
		t.Errorf("egress-control pulls %q; want the server's Service on the policy's 7782", env["CANYON_EGRESS_POLICY_URL"])
	}
	if len(c.VolumeMounts) != 2 || !c.VolumeMounts[0].ReadOnly || !c.VolumeMounts[1].ReadOnly || c.VolumeMounts[0].Name != "policy" || c.VolumeMounts[1].Name != "admin" {
		t.Fatalf("egress-control mounts %+v; want the policy's token and certificate and Envoy's admin socket alone, read-only: it keeps nothing", c.VolumeMounts)
	}
	mount := c.VolumeMounts[0]
	var token *corev1.ServiceAccountTokenProjection
	var cert *corev1.ConfigMapProjection
	for _, v := range spec.Volumes {
		if v.Name != mount.Name || v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken != nil {
				token = src.ServiceAccountToken
			}
			if src.ConfigMap != nil {
				cert = src.ConfigMap
			}
		}
	}
	if token == nil || token.Audience != "canyon-egress-policy" || token.ExpirationSeconds == nil || *token.ExpirationSeconds != 600 ||
		env["CANYON_EGRESS_POLICY_TOKEN_FILE"] != filepath.Join(mount.MountPath, token.Path) {
		t.Errorf("egress-control's token is %+v, read from %q; want audience canyon-egress-policy, 600 s, the file it reads", token, env["CANYON_EGRESS_POLICY_TOKEN_FILE"])
	}
	if cert == nil || cert.Name != "canyon-egress-policy-cert" || len(cert.Items) != 1 || cert.Items[0].Key != "tls.crt" ||
		env["CANYON_EGRESS_POLICY_CERT_FILE"] != filepath.Join(mount.MountPath, cert.Items[0].Path) {
		t.Errorf("egress-control pins %+v, read from %q; want the ConfigMap canyon-egress-policy-cert's tls.crt", cert, env["CANYON_EGRESS_POLICY_CERT_FILE"])
	}
	if spec.ServiceAccountName != "canyon-egress" || account == nil || account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("the gateway's pods run as %q (the ServiceAccount %v), with the API's token mounted: want canyon-egress, which mounts none", spec.ServiceAccountName, account)
	}
	for _, g := range grants(t, objs) {
		if g.subject == "canyon-egress" {
			t.Errorf("canyon-egress holds %+v in %s: it needs no Role", g.rule, g.namespace)
		}
	}
}

// Envoy's admin interface is a Unix socket on a volume of the gateway's pod alone, which no app shares (design/EGRESS.md
// §2.1, #2374): an emptyDir in memory, Envoy's to write and egress-control's to read, one uid running both as the
// socket's mode 0600 needs. Whoever reaches it reads the bootstrap, the receiver's token in it, and POSTs admin calls.
// egress-control mounts nothing else of Envoy's, and neither container nor the policy names a port for it: Envoy's
// admin is no port at all.
func TestEnvoysAdminSocketIsThePodsAlone(t *testing.T) {
	_, d, np := egressObjects(t)
	spec := d.Spec.Template.Spec
	dir := filepath.Dir(egresscontrol.AdminSocket)
	envoy, control := spec.Containers[0], egressControl(t, d)
	mountOf := func(c corev1.Container, name string) *corev1.VolumeMount {
		for i := range c.VolumeMounts {
			if c.VolumeMounts[i].Name == name {
				return &c.VolumeMounts[i]
			}
		}
		return nil
	}
	if m := mountOf(envoy, "admin"); m == nil || m.MountPath != dir || m.ReadOnly {
		t.Errorf("Envoy mounts admin as %+v; want %s, read-write: it binds the socket", m, dir)
	}
	if m := mountOf(control, "admin"); m == nil || m.MountPath != dir || !m.ReadOnly {
		t.Errorf("egress-control mounts admin as %+v; want %s, read-only", m, dir)
	}
	if m := mountOf(control, "run"); m != nil {
		t.Errorf("egress-control mounts run, %+v: the socket is all it mounts of Envoy's", m)
	}
	var admin *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "admin" {
			admin = &spec.Volumes[i]
		}
	}
	if admin == nil || admin.EmptyDir == nil || admin.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("the admin volume is %+v; want an emptyDir in memory, of this pod alone", admin)
	}
	if sc := spec.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 65534 || envoy.SecurityContext.RunAsUser != nil || control.SecurityContext.RunAsUser != nil {
		t.Errorf("the pod runs as %+v, Envoy %+v, egress-control %+v: want one uid, or the socket's mode 0600 shuts one out", sc, envoy.SecurityContext, control.SecurityContext)
	}
	var ports []int32
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			ports = append(ports, p.ContainerPort)
		}
	}
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			ports = append(ports, p.Port.IntVal)
		}
	}
	for _, p := range ports {
		if p != 3128 && p != 7784 {
			t.Errorf("the gateway's pod or its policy names port %d: only the proxy's 3128 and the check's metrics 7784", p)
		}
	}
}

// Each replica lets an app env call its share of the 30 new names a minute: 30 over CANYON_EGRESS_REPLICAS,
// the most that answer at once (contracts/routes.md, The check). A rollout adds one before it ends one and never
// ends one first, and a replica stops answering at SIGTERM, so the replicas and the surge are all that answer:
// CANYON_EGRESS_REPLICAS counts exactly those. The pods spread by node and zone, and a disruption leaves one.
func TestEgressControlSharesTheRateLimitByEveryReplicaARolloutRuns(t *testing.T) {
	_, d, _ := egressObjects(t)
	r := d.Spec.Strategy.RollingUpdate
	if r == nil || r.MaxSurge == nil || r.MaxUnavailable == nil || r.MaxSurge.String() != "1" || r.MaxUnavailable.String() != "0" || d.Spec.Replicas == nil {
		t.Fatalf("the gateway rolls out by %+v; want maxSurge 1, maxUnavailable 0, each a count", r)
	}
	var replicas string
	for _, e := range egressControl(t, d).Env {
		if e.Name == "CANYON_EGRESS_REPLICAS" {
			replicas = e.Value
		}
	}
	if want := strconv.Itoa(int(*d.Spec.Replicas) + r.MaxSurge.IntValue()); replicas != want {
		t.Errorf("CANYON_EGRESS_REPLICAS is %q; want %s, the %d replicas and the rollout's one", replicas, want, *d.Spec.Replicas)
	}
	var keys []string
	for _, c := range d.Spec.Template.Spec.TopologySpreadConstraints {
		if sel, err := metav1.LabelSelectorAsSelector(c.LabelSelector); err == nil && sel.Matches(labels.Set(d.Spec.Template.Labels)) && c.MaxSkew == 1 &&
			c.WhenUnsatisfiable == corev1.ScheduleAnyway {
			keys = append(keys, c.TopologyKey)
		}
	}
	if !slices.Equal(keys, []string{"kubernetes.io/hostname", "topology.kubernetes.io/zone"}) {
		t.Errorf("the gateway's pods spread by %v; want by node and by zone, maxSkew 1, never waiting on a domain", keys)
	}
}

// kube.sh stops, naming why, when the server's Service does not resolve: Envoy never starts with a receiver
// it cannot name.
func TestKubeShStopsWhenTheServerDoesNotResolve(t *testing.T) {
	cm, _, _ := egressObjects(t)
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Fatal("no dash on PATH, which runs kube.sh as Envoy's image will")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "kube.sh")
	if err := os.WriteFile(script, []byte(cm.Data["kube.sh"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "getent"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil { //nolint:gosec // the test's stand-in for getent, executable
		t.Fatal(err)
	}
	cmd := exec.Command(dash, script) //nolint:gosec // the test's own shell on a script of this repository
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CANYON_EGRESS_RECEIVER_PORT=7781"}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "canyon-server.canyon.svc does not resolve") {
		t.Errorf("kube.sh with no address for the server: %v %s", err, out)
	}
}

// The executor lists the ReplicaSets of canyon-apps, and only lists them: a failed /run that floored an open
// app waits on its own ReplicaSet before it opens the app's policy again (contracts/executor.md, On Kubernetes).
func TestTheExecutorListsTheAppsReplicaSets(t *testing.T) {
	var verbs []string
	for _, g := range grants(t, objects(t)) {
		if g.subject == executor && g.namespace == "canyon-apps" && slices.Contains(g.rule.APIGroups, "apps") && slices.Contains(g.rule.Resources, "replicasets") {
			verbs = append(verbs, g.rule.Verbs...)
		}
	}
	if !slices.Equal(verbs, []string{"list"}) {
		t.Errorf("the executor holds %v on the ReplicaSets of canyon-apps, want list alone", verbs)
	}
}

// The server takes the gateway's records (contracts/routes.md, Egress control; #2286): its receiver on every
// address at the port kube.sh dials, behind the server's Service on that port, with Envoy's token from the one
// Secret both read — never a value in a manifest — as egress-control reads it too, for the interception listener's
// access log (#2740), and every /run under via canyon names the gateway's Service.
func TestTheServerTakesTheGatewaysRecords(t *testing.T) {
	_, gateway, _ := egressObjects(t)
	var server *appsv1.Deployment
	var service *corev1.Service
	for _, o := range objects(t) {
		switch o := o.(type) {
		case *appsv1.Deployment:
			if o.Name == "canyon-server" {
				server = o
			}
		case *corev1.Service:
			if o.Name == "canyon-server" {
				service = o
			}
		}
	}
	if server == nil || service == nil {
		t.Fatal("the deploy writes no Deployment or Service canyon-server")
	}
	envOf := func(c corev1.Container) map[string]corev1.EnvVar {
		out := map[string]corev1.EnvVar{}
		for _, e := range c.Env {
			out[e.Name] = e
		}
		return out
	}
	envoy, srv := envOf(gateway.Spec.Template.Spec.Containers[0]), envOf(server.Spec.Template.Spec.Containers[0])
	port := envoy["CANYON_EGRESS_RECEIVER_PORT"].Value
	if srv["CANYON_EGRESS_RECEIVER_ADDR"].Value != "0.0.0.0:"+port || port == "" {
		t.Errorf("the server's receiver is at %q; want 0.0.0.0:%s, the port kube.sh dials", srv["CANYON_EGRESS_RECEIVER_ADDR"].Value, port)
	}
	var control corev1.Container
	for _, c := range gateway.Spec.Template.Spec.Containers {
		if c.Name == "egress-control" {
			control = c
		}
	}
	token := srv["CANYON_EGRESS_RECEIVER_TOKEN"]
	for who, got := range map[string]corev1.EnvVar{"Envoy": envoy["CANYON_EGRESS_RECEIVER_TOKEN"], "egress-control": envOf(control)["CANYON_EGRESS_RECEIVER_TOKEN"]} {
		if token.Value != "" || token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil || got.Value != "" || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil ||
			token.ValueFrom.SecretKeyRef.Name != "canyon-egress" || token.ValueFrom.SecretKeyRef.Key != "token" || *token.ValueFrom.SecretKeyRef != *got.ValueFrom.SecretKeyRef {
			t.Errorf("the server's token is %+v and %s's %+v; want both the Secret canyon-egress's token", token, who, got)
		}
	}
	if v := srv["CANYON_EGRESS_PROXY"].Value; v != "http://canyon-egress.canyon.svc:3128" {
		t.Errorf("the server's CANYON_EGRESS_PROXY is %q; want http://canyon-egress.canyon.svc:3128, the gateway's Service (contracts/executor.md, On Kubernetes)", v)
	}
	n, _ := strconv.Atoi(port)
	if !slices.ContainsFunc(service.Spec.Ports, func(p corev1.ServicePort) bool { return int(p.Port) == n && p.TargetPort.IntValue() == n }) ||
		!slices.ContainsFunc(server.Spec.Template.Spec.Containers[0].Ports, func(p corev1.ContainerPort) bool { return int(p.ContainerPort) == n }) {
		t.Errorf("the server's Service and container leave out %s, the receiver kube.sh dials at canyon-server.canyon.svc", port)
	}
}

// The gateway's pods start only once the certificate the server minted is published, and are ready only once
// their check holds a policy (README.md), so every install guide publishes it before it waits on the gateway, and
// Helm's first install runs without --wait, which would wait on the gateway before anything published it.
func TestTheInstallGuidesPublishTheCertificateBeforeWaitingOnTheGateway(t *testing.T) {
	for _, guide := range []string{"kubernetes.md", "k3s.md", "helm.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "src", "content", "docs", "self-host", guide)) //nolint:gosec // a fixed path in this repository
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		publish, wait := strings.Index(page, "/canyon-server egress-policy-cert)"), strings.Index(page, "rollout status deploy/canyon-egress")
		if publish < 0 || wait < 0 || wait < publish {
			t.Errorf("%s publishes the certificate at %d and first waits on the gateway at %d: want both, the publishing first", guide, publish, wait)
		}
		if guide != "helm.md" {
			continue
		}
		for line := range strings.SplitSeq(page[:publish], "\n") {
			if strings.HasPrefix(line, "helm install ") && strings.Contains(line, "--wait") {
				t.Errorf("helm.md's first install waits before the certificate is published: %s", line)
			}
		}
	}
}
