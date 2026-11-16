package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// withEgress is the fixture's request with its kube_egress and run, the gateway answering, and the
// fixture's kube_egress_policy, the NetworkPolicy the contract says it writes.
func withEgress(t *testing.T) (*Executor, executor.RunRequest, map[string]any) {
	t.Helper()
	e, req, _ := fixture(t)
	raw, err := os.ReadFile("../../contracts/testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Egress executor.Egress `json:"kube_egress"`
		Policy map[string]any  `json:"kube_egress_policy"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	req.Egress, req.Run = &f.Egress, "gr_6f1c0b9a7e2a9d3c"
	e.Gateway = func(context.Context, string) error { return nil }
	return e, req, f.Policy
}

// values is a container's literal environment, by name.
func values(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, v := range c.Env {
		if v.ValueFrom == nil {
			m[v.Name] = v.Value
		}
	}
	return m
}

// envSecretOf is the env Secret the last /run wrote.
func envSecretOf(t *testing.T, c *fake.Clientset) *corev1.Secret {
	t.Helper()
	var secret *corev1.Secret
	for _, a := range c.Actions() {
		if w, ok := a.(k8stesting.CreateAction); ok && a.GetVerb() == "create" && a.GetResource().Resource == "secrets" && w.GetObject().(metav1.Object).GetName() == "canyon-env-claims-portal-preview" {
			secret = w.GetObject().(*corev1.Secret)
		}
	}
	if secret == nil {
		t.Fatal("no env Secret written")
	}
	return secret
}

const (
	podProxy = "http://127.0.0.1:4184" // canyon-identity's forwarder, from layer 3 (design/EGRESS.md §4.3)
	jvmProxy = "-Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=4184 -Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=4184 -Dhttp.nonProxyHosts=apps-db.meridian.example|127.0.0.1|localhost"
)

// proxyVars is what the fixture's /run with egress sets on all three containers.
var proxyVars = map[string]string{
	"HTTPS_PROXY": podProxy, "HTTP_PROXY": podProxy, "https_proxy": podProxy, "http_proxy": podProxy,
	"NO_PROXY": "apps-db.meridian.example,127.0.0.1,localhost", "no_proxy": "apps-db.meridian.example,127.0.0.1,localhost", "NODE_USE_ENV_PROXY": "1",
}

// With egress (contracts/executor.md, On Kubernetes) the pod's way out is Canyon's egress gateway on 3128
// and no public address — the fixture's kube_egress_policy — the proxy variables, naming the pod's forwarder,
// are literal values of the template on all three containers, so a put-back template carries its own, the env Secret holds env alone,
// and the pod is labelled with the run and canyon.floor.
func TestRunWithEgressFloorsThePodAndProxiesItsThreeContainers(t *testing.T) {
	e, req, want := withEgress(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	np, err := c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, np.Spec); !reflect.DeepEqual(got, want["spec"]) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want["spec"])
		t.Errorf("the floored policy lets the pod out on %v:\n got %s\nwant %s", portsOut(np), g, w)
	}
	if secret := envSecretOf(t, c); !reflect.DeepEqual(secret.StringData, req.Env) {
		t.Errorf("the env Secret holds %v, want the request's env alone", slices.Sorted(func(yield func(string) bool) {
			for k := range secret.StringData {
				if !yield(k) {
					return
				}
			}
		}))
	}
	d, err := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l := d.Spec.Template.Labels; l["canyon.gate_run"] != req.Run || l["canyon.floor"] != "true" {
		t.Errorf("the pod's labels are %v, want canyon.gate_run %s and canyon.floor", l, req.Run)
	}
	spec := d.Spec.Template.Spec
	if len(spec.InitContainers) != 2 {
		t.Fatalf("%d sidecars, want canyon-identity and oauth2-proxy", len(spec.InitContainers))
	}
	for _, s := range spec.InitContainers {
		got := values(s)
		for k, v := range proxyVars {
			if got[k] != v {
				t.Errorf("%s's %s is %q, want %q", s.Name, k, got[k], v)
			}
		}
		if _, ok := got["JDK_JAVA_OPTIONS"]; ok {
			t.Errorf("%s carries JDK_JAVA_OPTIONS, the app's alone", s.Name)
		}
	}
	app := values(spec.Containers[0])
	for k, v := range proxyVars {
		if app[k] != v {
			t.Errorf("the app's %s is %q, want %q", k, app[k], v)
		}
	}
	if app["JDK_JAVA_OPTIONS"] != jvmProxy {
		t.Errorf("the app's JDK_JAVA_OPTIONS is %q, want %q", app["JDK_JAVA_OPTIONS"], jvmProxy)
	}
}

// From layer 3 (design/EGRESS.md §4.3) a floored pod runs as its env's own ServiceAccount, bound to nothing,
// with no token of it mounted but one projected token, audience canyon-egress, 10 minutes, mounted into
// canyon-identity alone: neither the app nor oauth2-proxy can read it or name its forwarder's gateway. Without
// egress there is no account and no token, and /stop deletes the account with the rest.
func TestTheEgressTokenIsCanyonIdentitysAlone(t *testing.T) {
	e, req, _ := withEgress(t)
	c := cluster(t, e, healthy)
	if res, err := e.Run(t.Context(), req); err != nil || !res.TokenCallers {
		t.Fatalf("/run with egress: %+v %v; want its answer saying its pods carry the token (token_callers)", res, err)
	}
	sa, err := c.CoreV1().ServiceAccounts(Namespace).Get(t.Context(), "canyon-app-claims-portal-preview", metav1.GetOptions{})
	if err != nil || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken || len(sa.Secrets) != 0 {
		t.Fatalf("the env's ServiceAccount: %+v %v; want one that mounts no token", sa, err)
	}
	d, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	pod := d.Spec.Template.Spec
	if pod.ServiceAccountName != sa.Name || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ShareProcessNamespace != nil {
		t.Errorf("the pod runs as %q, automount %v, process namespace shared %v; want %s, no token mounted, its containers apart", pod.ServiceAccountName, pod.AutomountServiceAccountToken, pod.ShareProcessNamespace, sa.Name)
	}
	var token string
	for _, v := range pod.Volumes {
		if v.Projected == nil {
			continue
		}
		s := v.Projected.Sources
		if token != "" || len(s) != 1 || s[0].ServiceAccountToken == nil || s[0].ServiceAccountToken.Audience != "canyon-egress" ||
			s[0].ServiceAccountToken.ExpirationSeconds == nil || *s[0].ServiceAccountToken.ExpirationSeconds != 600 || s[0].ServiceAccountToken.Path != "token" {
			t.Fatalf("projected volume %s: %+v; want one, the token alone, audience canyon-egress, 600 s", v.Name, s)
		}
		token = v.Name
	}
	for _, ct := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		mounts := false
		for _, m := range ct.VolumeMounts {
			if m.Name == token {
				mounts = m.MountPath == "/var/run/canyon-egress" && m.ReadOnly && m.SubPath == ""
			}
		}
		env := values(ct)
		if ct.Name == "canyon-identity" {
			if !mounts || env["CANYON_IDENTITY_EGRESS_PROXY"] != canyonEgress || env["CANYON_IDENTITY_EGRESS_TOKEN"] != "/var/run/canyon-egress/token" {
				t.Errorf("canyon-identity mounts %+v, its forwarder %q with %q; want the token read-only at /var/run/canyon-egress, the gateway %s", ct.VolumeMounts, env["CANYON_IDENTITY_EGRESS_PROXY"], env["CANYON_IDENTITY_EGRESS_TOKEN"], canyonEgress)
			}
			continue
		}
		if slices.ContainsFunc(ct.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == token }) || env["CANYON_IDENTITY_EGRESS_PROXY"] != "" || env["CANYON_IDENTITY_EGRESS_TOKEN"] != "" {
			t.Errorf("%s mounts %+v and names %q, %q: the token is canyon-identity's alone", ct.Name, ct.VolumeMounts, env["CANYON_IDENTITY_EGRESS_PROXY"], env["CANYON_IDENTITY_EGRESS_TOKEN"])
		}
	}
	if err := e.Stop(t.Context(), req.App, req.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().ServiceAccounts(Namespace).Get(t.Context(), sa.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after /stop the ServiceAccount: %v, want it gone", err)
	}

	open, req, _ := fixture(t)
	oc := cluster(t, open, healthy)
	if res, err := open.Run(t.Context(), req); err != nil || res.TokenCallers {
		t.Fatalf("/run without egress: %+v %v; want no token_callers", res, err)
	}
	od, _ := oc.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	_, err = oc.CoreV1().ServiceAccounts(Namespace).Get(t.Context(), "canyon-app-claims-portal-preview", metav1.GetOptions{})
	if od.Spec.Template.Spec.ServiceAccountName != "" || slices.ContainsFunc(od.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Projected != nil }) || !apierrors.IsNotFound(err) {
		t.Errorf("without egress the pod runs as %q, its volumes %+v, the ServiceAccount %v; want none", od.Spec.Template.Spec.ServiceAccountName, od.Spec.Template.Spec.Volumes, err)
	}
}

// An env-given JDK_JAVA_OPTIONS stays in the env Secret, which may hold a secret: the template names it,
// $(JDK_JAVA_OPTIONS), which Kubernetes fills from the Secret, then the proxy properties.
func TestAnEnvGivenJDKJavaOptionsStaysInTheSecret(t *testing.T) {
	e, req, _ := withEgress(t)
	req.Env["JDK_JAVA_OPTIONS"] = "-Xmx512m -Djavax.net.ssl.trustStorePassword=a-value-that-never-leaks-0123456789"
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := envSecretOf(t, c).StringData["JDK_JAVA_OPTIONS"]; got != req.Env["JDK_JAVA_OPTIONS"] {
		t.Errorf("the env Secret's JDK_JAVA_OPTIONS is %q, want env's own", got)
	}
	d, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if got := values(d.Spec.Template.Spec.Containers[0])["JDK_JAVA_OPTIONS"]; got != "$(JDK_JAVA_OPTIONS) "+jvmProxy {
		t.Errorf("the app's JDK_JAVA_OPTIONS is %q, want env's own by reference, then the proxy properties", got)
	}
}

// Without egress the pod reaches DNS, the apps database and public 80 and 443 — the fixture's kube_objects
// policy, whole — nothing proxies it, and the pod carries no canyon.floor (contracts/executor.md).
func TestRunWithoutEgressLeavesThePodOpen(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	np, _ := c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	db := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(5433)}, To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.20.48.0/20"}}}}
	if want := []networkingv1.NetworkPolicyEgressRule{dnsEgress(), db, publicEgress(80, 443)}; !equality.Semantic.DeepEqual(np.Spec.Egress, want) {
		t.Errorf("without egress the policy lets the pod out on %v, want %v", portsOut(np), []int{53, 53, 5433, 80, 443})
	}
	d, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if _, ok := d.Spec.Template.Labels["canyon.floor"]; ok {
		t.Error("an open pod is labelled canyon.floor")
	}
	for _, s := range append(d.Spec.Template.Spec.InitContainers, d.Spec.Template.Spec.Containers...) {
		for _, v := range s.Env {
			if strings.HasSuffix(strings.ToUpper(v.Name), "_PROXY") || v.Name == "JDK_JAVA_OPTIONS" {
				t.Errorf("%s names %s without egress", s.Name, v.Name)
			}
		}
	}
}

// The open policy's private ranges are the gateway's (deploy/k8s/egress.yaml), one list: neither an app nor
// Envoy reaches an address the other is kept from.
func TestTheOpenPolicyAndTheGatewayRefuseOneList(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/k8s/egress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	docs := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	for {
		var np networkingv1.NetworkPolicy
		if err := docs.Decode(&np); err != nil {
			t.Fatalf("egress.yaml has no NetworkPolicy canyon-egress: %v", err)
		}
		if np.Kind != "NetworkPolicy" {
			continue
		}
		for _, r := range np.Spec.Egress {
			for _, to := range r.To {
				if to.IPBlock != nil && !slices.Equal(to.IPBlock.Except, private) {
					t.Errorf("the gateway's policy leaves out %v; the open policy %v", to.IPBlock.Except, private)
				}
			}
		}
		return
	}
}

// The builds' policy (deploy/k8s/builds.yaml) leaves out the very list an app's own does, private, wherever it
// lets a build pod out by address (contracts/executor.md, A build reaches no private or link-local address).
func TestTheBuildsRefuseTheOneList(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/k8s/builds.yaml")
	if err != nil {
		t.Fatal(err)
	}
	docs := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	var blocks int
	for {
		var np networkingv1.NetworkPolicy
		if err := docs.Decode(&np); err != nil {
			break
		}
		if np.Kind != "NetworkPolicy" {
			continue
		}
		for _, r := range np.Spec.Egress {
			for _, to := range r.To {
				if to.IPBlock != nil {
					blocks++
					if !slices.Equal(to.IPBlock.Except, private) {
						t.Errorf("the builds' policy %s leaves out %v; an app's %v", np.Name, to.IPBlock.Except, private)
					}
				}
			}
		}
	}
	if blocks == 0 {
		t.Fatal("builds.yaml lets a build pod out by no address: its policy is not where this test looks")
	}
}

// Only Canyon's egress gateway, by its one name, is a /run's proxy (contracts/executor.md, On Kubernetes):
// a shorter name resolves in the executor's namespace and not the app's.
func TestEgressNamesTheGatewayAlone(t *testing.T) {
	e, req, _ := withEgress(t)
	for _, proxy := range []string{"http://canyon-egress:3128", "http://canyon-egress.canyon:3128", "http://elsewhere.canyon.svc:3128"} {
		req.Egress = &executor.Egress{Proxy: proxy, NoProxy: []string{}}
		if _, _, _, err := e.check(req); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("egress.proxy %s: %v, want a refusal", proxy, err)
		}
	}
}

// With egress and nothing answering at the gateway, the /run fails the executor's way and writes nothing:
// 502, the gateway's sentence.
func TestRunWithEgressNeedsTheGateway(t *testing.T) {
	e, req, _ := withEgress(t)
	c := cluster(t, e, healthy)
	var dialed string
	e.Gateway = func(_ context.Context, hostport string) error {
		dialed = hostport
		return errors.New("connection refused")
	}
	_, err := e.Run(t.Context(), req)
	if err == nil || errors.Is(err, executor.ErrRequest) || err.Error() != "nothing answers at canyon-egress.canyon.svc:3128, Canyon's egress gateway" {
		t.Fatalf("%v, want the gateway's sentence", err)
	}
	if dialed != "canyon-egress.canyon.svc:3128" || len(writes(c)) != 0 {
		t.Fatalf("dialed %q, wrote %v: want the gateway asked and nothing written", dialed, writes(c))
	}
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(req)
	if code, answer := send(t, "POST", srv.URL+"/run", string(body)); code != 502 || answer != `{"error":"nothing answers at canyon-egress.canyon.svc:3128, Canyon's egress gateway"}` {
		t.Errorf("POST /run with the gateway down: %d %s", code, answer)
	}
}

// crashing is a pod of run whose app restarted.
func crashing(run string) *corev1.Pod {
	p := readyPod("claims-portal-preview", run)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
	return p
}

// flip runs the app from first, healthy, then from then, whose rollout fails — its pods gone once the
// template is put back when gone holds, as the failed ReplicaSet scales down, else lingering — and answers
// the policy before and after the second /run and its error. also adds reactors before the second /run.
func flip(t *testing.T, first, then executor.RunRequest, gone bool, also func(*fake.Clientset)) (before, after *networkingv1.NetworkPolicy, err error) {
	t.Helper()
	e, _, _ := withEgress(t)
	crash := false
	c := cluster(t, e, func(run string) *corev1.Pod {
		if crash {
			return crashing(run)
		}
		return nil
	})
	if _, err := e.Run(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	before, _ = c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	crash = true
	if gone {
		pods := corev1.SchemeGroupVersion.WithResource("pods")
		c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			kept := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment).Spec.Template.Labels["canyon.run"]
			list, err := c.Tracker().List(pods, corev1.SchemeGroupVersion.WithKind("Pod"), Namespace)
			if err != nil {
				return true, nil, err
			}
			for _, p := range list.(*corev1.PodList).Items {
				if p.Labels["canyon.run"] != kept && len(p.Status.ContainerStatuses) > 0 {
					if err := c.Tracker().Delete(pods, Namespace, p.Name); err != nil {
						return true, nil, err
					}
				}
			}
			return false, nil, nil
		})
	}
	if also != nil {
		also(c)
	}
	if _, err = e.Run(t.Context(), then); err == nil {
		t.Fatal("a restarting app's /run succeeded")
	}
	after, _ = c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	return before, after, err
}

// openAndFloored is the fixture's request without egress, and with it.
func openAndFloored(t *testing.T) (executor.RunRequest, executor.RunRequest) {
	t.Helper()
	_, floored, _ := withEgress(t)
	open := floored
	open.Egress, open.Run = nil, ""
	return open, floored
}

// A redeploy that floors an open app and fails puts the open policy back once the failed rollout's pods
// are gone (contracts/executor.md, On Kubernetes): the previous pod, never stopped, keeps its way out.
func TestAFailedFlooringPutsTheOpenPolicyBackOnceItsPodsAreGone(t *testing.T) {
	open, floored := openAndFloored(t)
	before, after, err := flip(t, open, floored, true, nil)
	if !equality.Semantic.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("after a failed /run (%v) the policy lets the pod out on %v, want the open one's %v back", err, portsOut(after), portsOut(before))
	}
}

// While the failed rollout's pods linger they are floored ones run open, so the floor stays — fail closed —
// and the /run's failure says so.
func TestAFailedFlooringKeepsTheFloorWhileItsPodsLinger(t *testing.T) {
	open, floored := openAndFloored(t)
	_, after, err := flip(t, open, floored, false, nil)
	if !slices.Equal(portsOut(after), []int{53, 53, 5433, 3128}) || !strings.Contains(err.Error(), "keeps this /run's narrower NetworkPolicy") {
		t.Fatalf("lingering pods: the policy lets the pod out on %v and the /run says %q; want the floor kept, and said", portsOut(after), err)
	}
}

// Unflooring that fails puts the floor back at once, lingering pods or not: it is the narrower.
func TestAFailedUnflooringPutsTheFloorBackAtOnce(t *testing.T) {
	open, floored := openAndFloored(t)
	before, after, err := flip(t, floored, open, false, nil)
	if !equality.Semantic.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("after a failed /run (%v) the policy lets the pod out on %v, want the floor's %v back", err, portsOut(after), portsOut(before))
	}
}

// A put-back the API refuses leaves the floor, and the /run's failure says why.
func TestAFailedPutBackKeepsTheFloorAndSaysSo(t *testing.T) {
	open, floored := openAndFloored(t)
	_, after, err := flip(t, open, floored, true, func(c *fake.Clientset) {
		updates := 0
		c.PrependReactor("update", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			if updates++; updates > 1 { // the /run's own update, then the put-back
				return true, nil, apierrors.NewServiceUnavailable("etcd is away")
			}
			return false, nil, nil
		})
	})
	if !slices.Equal(portsOut(after), []int{53, 53, 5433, 3128}) || !strings.Contains(err.Error(), "etcd is away") {
		t.Fatalf("a refused put-back: the policy lets the pod out on %v and the /run says %q; want the floor kept, and why", portsOut(after), err)
	}
}

// failedReplicaSet makes the fake API keep a ReplicaSet for each template a /run rolls, as the Deployment
// controller does, and — once the template is put back — takes the failed rollout's pods away at once but
// scales its ReplicaSet down only after lists more lists of ReplicaSets, never when lists is negative: the
// controller's lag, during which that ReplicaSet can still start a floored pod.
func failedReplicaSet(lists int) func(*fake.Clientset) {
	return func(c *fake.Clientset) {
		rsets, pods := appsv1.SchemeGroupVersion.WithResource("replicasets"), corev1.SchemeGroupVersion.WithResource("pods")
		var failed string
		c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			labels := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment).Spec.Template.Labels
			if failed == "" { // the /run's own update: its ReplicaSet, wanting one pod
				failed = labels["canyon.run"]
				one := int32(1)
				rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "claims-portal-preview-" + failed, Namespace: Namespace, Labels: labels},
					Spec: appsv1.ReplicaSetSpec{Replicas: &one}, Status: appsv1.ReplicaSetStatus{Replicas: 1}}
				return false, nil, c.Tracker().Add(rs)
			}
			list, err := c.Tracker().List(pods, corev1.SchemeGroupVersion.WithKind("Pod"), Namespace) // the put-back: its pods go
			if err != nil {
				return true, nil, err
			}
			for _, p := range list.(*corev1.PodList).Items {
				if p.Labels["canyon.run"] == failed {
					if err := c.Tracker().Delete(pods, Namespace, p.Name); err != nil {
						return true, nil, err
					}
				}
			}
			return false, nil, nil
		})
		c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
			if lists--; lists == 0 {
				zero := int32(0)
				rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "claims-portal-preview-" + failed, Namespace: Namespace, Labels: map[string]string{"canyon.app": "claims-portal", "canyon.target": "preview", "canyon.run": failed}},
					Spec: appsv1.ReplicaSetSpec{Replicas: &zero}}
				if err := c.Tracker().Update(rsets, rs, Namespace); err != nil {
					return true, nil, err
				}
			}
			return false, nil, nil
		})
	}
}

// A failed flooring waits for its ReplicaSet, not its pods alone: with the pods gone but the ReplicaSet not
// yet scaled to nothing, it could still start a floored pod under the open policy.
func TestAFailedFlooringWaitsForItsReplicaSet(t *testing.T) {
	open, floored := openAndFloored(t)
	before, after, err := flip(t, open, floored, false, failedReplicaSet(3))
	if !equality.Semantic.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("a ReplicaSet scaled down late (%v): the policy lets the pod out on %v, want the open one's %v back once it is", err, portsOut(after), portsOut(before))
	}
	_, after, err = flip(t, open, floored, false, failedReplicaSet(-1))
	if !slices.Equal(portsOut(after), []int{53, 53, 5433, 3128}) || !strings.Contains(err.Error(), "keeps this /run's narrower NetworkPolicy") {
		t.Fatalf("a ReplicaSet never scaled down: the policy lets the pod out on %v and the /run says %q; want the floor kept, and said", portsOut(after), err)
	}
}

// Stop takes the app's NetworkPolicy with its Deployment (contracts/executor.md, /stop).
func TestStopDeletesThePolicy(t *testing.T) {
	e, req, _ := withEgress(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(t.Context(), req.App, req.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("after /stop the NetworkPolicy: %v, want it gone", err)
	}
}

// portsOut is every port a NetworkPolicy lets its pods out on, in order.
func portsOut(np *networkingv1.NetworkPolicy) []int {
	var ports []int
	for _, r := range np.Spec.Egress {
		for _, p := range r.Ports {
			ports = append(ports, p.Port.IntValue())
		}
	}
	return ports
}

// What the app said is masked with the request's own env — its secret named — never the proxy variables
// egress adds: their values are no secret, and NODE_USE_ENV_PROXY's 1 would turn every 1 the app wrote into
// a name.
func TestEgressMasksNothingOfItsOwn(t *testing.T) {
	e, req, _ := withEgress(t)
	req.Env["STRIPE_SECRET_KEY"] = "a-value-that-never-leaks-0123456789"
	cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "1 worker on localhost failed: stripe rejected a-value-that-never-leaks-0123456789\n"}}}}
		return p
	})
	_, err := e.Run(t.Context(), req)
	var exited *executor.Exited
	if !errors.As(err, &exited) || exited.Output != "1 worker on localhost failed: stripe rejected <STRIPE_SECRET_KEY>\n" {
		t.Fatalf("%#v (%v), want the app's line as it wrote it, its secret named", exited, err)
	}
}

// POST /callers (contracts/executor.md): an address held by exactly one pod of canyon-apps — every pod that
// lists it claims it, one that ended among them — named when that pod is Pending or Running, not terminating,
// and names an app and preview or prod, with its canyon.gate_run and, as since, its earliest running container's
// startedAt in UTC — never the pod's creation or status.startTime, which come before its sandbox had the address;
// one with no container running left out too; every other left out, the rest in the order asked; addresses out
// of contract refused, and a cluster that does not answer 502.
func TestCallersNameOnePodPerAddress(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	at := func(sec int) metav1.Time { return metav1.NewTime(time.Date(2026, 10, 11, 11, 14, sec, 0, cest)) }
	early := at(0)
	up := func(sec int) corev1.ContainerStatus {
		return corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(sec)}}}
	}
	pod := func(name, ip, app, target, run string, phase corev1.PodPhase, terminating bool) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, CreationTimestamp: early, Labels: map[string]string{"canyon.app": app, "canyon.target": target}},
			Status: corev1.PodStatus{PodIP: ip, Phase: phase, StartTime: &early, ContainerStatuses: []corev1.ContainerStatus{up(5), up(2)},
				InitContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: at(1)}}}}}}
		if run != "" {
			p.Labels["canyon.gate_run"] = run
		}
		if terminating {
			p.DeletionTimestamp = &metav1.Time{}
			p.Finalizers = []string{"canyon.test/keep"}
		}
		return p
	}
	running, pending, ended := corev1.PodRunning, corev1.PodPending, corev1.PodFailed
	notTaken := pod("none-running", "10.0.4.16", "expenses", "preview", "", pending, false)
	notTaken.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
	e := &Executor{Client: fake.NewClientset(
		pod("prod", "10.0.4.7", "claims-portal", "prod", "gr_6f1c0b9a7e2a9d3c", running, false),
		pod("preview", "10.0.4.8", "claims-portal", "preview", "", pending, false),
		pod("twice-a", "10.0.4.9", "claims-portal", "preview", "", running, false),
		pod("twice-b", "10.0.4.9", "expenses", "prod", "", running, false),
		pod("going", "10.0.4.10", "expenses", "preview", "", running, true),
		pod("staging", "10.0.4.11", "expenses", "staging", "", running, false),
		pod("bad-run", "10.0.4.12", "expenses", "prod", "not-a-run", running, false),
		pod("dead", "10.0.4.13", "expenses", "prod", "", ended, false),
		pod("dead-beside", "10.0.4.14", "expenses", "prod", "", corev1.PodSucceeded, false),
		pod("live-beside", "10.0.4.14", "claims-portal", "prod", "", running, false),
		pod("no-app", "10.0.4.15", "", "prod", "", running, false),
		pod("no-address", "", "expenses", "preview", "", pending, false),
		notTaken,
	)}
	got, err := e.Callers(t.Context(), []string{"10.0.4.12", "10.0.4.8", "10.0.4.9", "10.0.4.10", "10.0.4.11", "10.0.4.7", "10.0.4.13", "10.0.4.14", "10.0.4.15", "10.0.4.16", "10.0.4.99"})
	if err != nil {
		t.Fatal(err)
	}
	run := "gr_6f1c0b9a7e2a9d3c"
	since := time.Date(2026, 10, 11, 9, 14, 3, 0, time.UTC) // the second past the earlier running container's start (2 s), never the pod's start (0 s) nor a finished init container's (1 s)
	want := []executor.Caller{
		{Address: "10.0.4.12", App: "expenses", Target: "prod", Kind: "web", Since: since},
		{Address: "10.0.4.8", App: "claims-portal", Target: "preview", Kind: "web", Since: since},
		{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: &run, Since: since},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("callers\n got %s\nwant %s", g, w)
	}
	for _, asked := range [][]string{nil, {"10.0.4.7", "10.0.4.7"}, {"::1"}, {"010.0.4.7"}} {
		if _, err := e.Callers(t.Context(), asked); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%v: %v, want a refusal", asked, err)
		}
	}
	down := fake.NewClientset()
	down.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is away")
	})
	srv := httptest.NewServer(Handler(&Executor{Client: down}))
	t.Cleanup(srv.Close)
	if code, answer := send(t, "POST", srv.URL+"/callers", `{"addresses":["10.0.4.7"]}`); code != 502 || !strings.Contains(answer, "the API server is away") {
		t.Errorf("POST /callers with the cluster away: %d %s, want 502", code, answer)
	}
}

// The wait before a wider policy goes back is bounded whole, each call within it too: an API server that
// never answers leaves the floor in place and the put-back says so, never a /run that hangs.
func TestThePutBacksWaitIsBoundedWhole(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // an API server that takes every call and answers none
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	_, req, _ := withEgress(t)
	e := &Executor{Client: kubernetes.NewForConfigOrDie(&rest.Config{Host: srv.URL}), Poll: time.Millisecond, Deadline: 200 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- e.putPolicyBack(context.WithoutCancel(t.Context()), req, "abcd1234", networkingv1.NetworkPolicySpec{})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the put-back went ahead with no answer from the API server")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the put-back's wait is still waiting on an API server that never answers")
	}
}

// withSignin is withEgress under CANYON_KUBE_SIGNIN_DIRECT login.meridian.example=10.30.0.7/32, the fixture's
// own issuer listed with its address, and the fixture's kube_signin_policy, the NetworkPolicy the contract says
// it writes (contracts/executor.md, Sign-in past the gateway).
func withSignin(t *testing.T) (*Executor, executor.RunRequest, map[string]any) {
	t.Helper()
	e, req, _ := withEgress(t)
	e.SigninDirect = []Direct{{Host: "login.meridian.example", Addr: netip.MustParsePrefix("10.30.0.7/32")}}
	raw, err := os.ReadFile("../../contracts/testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Policy map[string]any `json:"kube_signin_policy"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return e, req, f.Policy
}

// policyOf is the NetworkPolicy req's /run wrote, as generic JSON.
func policyOf(t *testing.T, e *Executor, req executor.RunRequest) map[string]any {
	t.Helper()
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	np, err := c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return asJSON(t, np.Spec)
}

// Under CANYON_KUBE_SIGNIN_DIRECT a floored pod's policy also admits TCP 443 to the addresses it names, each
// once — the fixture's kube_signin_policy, whole — and a host listed without one, a provider inside the
// cluster, adds no rule: the operator's own policy admits it. An open pod's policy is the one without the
// setting. Without the setting the floored policy is kube_egress_policy
// (TestRunWithEgressFloorsThePodAndProxiesItsThreeContainers).
func TestSigninDirectOpensTheFloorOn443ToItsAddressesAlone(t *testing.T) {
	e, req, want := withSignin(t)
	e.SigninDirect = append(e.SigninDirect, Direct{Host: "keys.meridian.example", Addr: netip.MustParsePrefix("10.30.0.7/32")}, Direct{Host: "keycloak.idp.svc.cluster.local"})
	if got := policyOf(t, e, req); !reflect.DeepEqual(got, want["spec"]) {
		g, _ := json.Marshal(got["egress"])
		w, _ := json.Marshal(want["spec"].(map[string]any)["egress"])
		t.Errorf("under CANYON_KUBE_SIGNIN_DIRECT the floored policy lets the pod out on\n got %s\nwant %s", g, w)
	}
	e, req, _ = withSignin(t)
	e.SigninDirect = []Direct{{Host: "keycloak.idp.svc.cluster.local"}}
	_, _, egressed := withEgress(t)
	if got := policyOf(t, e, req); !reflect.DeepEqual(got, egressed["spec"]) {
		g, _ := json.Marshal(got["egress"])
		t.Errorf("a sign-in host listed without an address adds %s, want kube_egress_policy's own", g)
	}
	e, open, objects := fixture(t)
	e.SigninDirect = []Direct{{Host: "login.meridian.example", Addr: netip.MustParsePrefix("10.30.0.7/32")}}
	var policy map[string]any
	if err := json.Unmarshal(objects["network_policy"], &policy); err != nil {
		t.Fatal(err)
	}
	if got := policyOf(t, e, open); !reflect.DeepEqual(got, policy["spec"]) {
		g, _ := json.Marshal(got["egress"])
		t.Errorf("under CANYON_KUBE_SIGNIN_DIRECT an open pod's policy lets it out on %s, want kube_objects' own", g)
	}
}

// Under CANYON_KUBE_SIGNIN_DIRECT the sign-in sidecars of a floored pod reach the front door's sign-in hosts the
// setting lists directly — their NO_PROXY and no_proxy end with them, lower-cased, each once: choice 2's issuer,
// choice 1 oidc's issuer and key set, none for alb — and every host it does not list through the gateway, so a
// public provider keeps working with the setting on. The app's are unchanged. Without the setting the
// sidecars' are the app's (TestRunWithEgressFloorsThePodAndProxiesItsThreeContainers).
func TestSigninDirectNamesOnlyListedHostsInTheSidecarsNoProxy(t *testing.T) {
	const own = "apps-db.meridian.example,127.0.0.1,localhost"
	oidc := `{"choice":1,"gateway":"oidc","issuer":"https://Gateway.Meridian.example","audience":"canyon-gateway","jwks_url":"https://keys.meridian.example/jwks","cookie":"_pomerium","domain":"canyon.meridian.example","form":"app.env.domain"}`
	two := func(issuer string) string {
		return `{"choice":2,"signin":{"issuer":"` + issuer + `","client_id":"canyon-apps","client_secret":"redacted"},"domain":"canyon.meridian.example","form":"app.env.domain"}`
	}
	for name, tc := range map[string]struct {
		door   string
		listed []string
		direct string
	}{
		"choice 2, listed":                        {"", []string{"login.meridian.example"}, own + ",login.meridian.example"},
		"choice 2, a public issuer unlisted":      {two("https://accounts.google.com"), []string{"login.meridian.example"}, own},
		"choice 2, on 443 by name":                {two("https://login.meridian.example:443/realms/apps"), []string{"login.meridian.example"}, own + ",login.meridian.example"},
		"choice 2, an unlisted host with a comma": {two("https://login.meridian.example,evil.example"), []string{"login.meridian.example"}, own},
		"choice 2 at a private address":           {two("https://10.30.0.7/realms/apps"), []string{"10.30.0.7"}, own + ",10.30.0.7"},
		"choice 1 oidc, both listed":              {oidc, []string{"keys.meridian.example", "gateway.meridian.example"}, own + ",gateway.meridian.example,keys.meridian.example"},
		"choice 1 oidc, its key set alone":        {oidc, []string{"keys.meridian.example"}, own + ",keys.meridian.example"},
		"choice 1 oidc, one host":                 {oidcDoor, []string{"gateway.meridian.example"}, own + ",gateway.meridian.example"},
		"choice 1 alb":                            {albDoor, []string{"login.meridian.example"}, own},
	} {
		t.Run(name, func(t *testing.T) {
			e, req, _ := withSignin(t)
			e.SigninDirect = nil
			for _, h := range tc.listed {
				e.SigninDirect = append(e.SigninDirect, Direct{Host: h, Addr: netip.MustParsePrefix("10.30.0.7/32")})
			}
			if tc.door != "" {
				req.FrontDoor = json.RawMessage(tc.door)
			}
			c := cluster(t, e, healthy)
			if _, err := e.Run(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			d, err := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range d.Spec.Template.Spec.InitContainers {
				for _, k := range []string{"NO_PROXY", "no_proxy"} {
					if got := values(s)[k]; got != tc.direct {
						t.Errorf("%s's %s is %q, want %q", s.Name, k, got, tc.direct)
					}
				}
			}
			for _, k := range []string{"NO_PROXY", "no_proxy"} {
				if got := values(d.Spec.Template.Spec.Containers[0])[k]; got != own {
					t.Errorf("the app's %s is %q, want %q, unchanged", k, got, own)
				}
			}
		})
	}
}

// A listed sign-in host whose URL names a port other than 443 is 400 and nothing is written: the rule admits
// 443 alone, so the sidecar would call a port the floor drops — every URL of the door, the same host's second
// one included. An unlisted host on that port goes through the gateway, as without the setting.
func TestSigninDirectRefusesAListedHostOnAnotherPort(t *testing.T) {
	oidc := func(issuer, keys string) string {
		return `{"choice":1,"gateway":"oidc","issuer":"` + issuer + `","audience":"canyon-gateway","jwks_url":"` + keys + `","cookie":"_pomerium","domain":"canyon.meridian.example","form":"app.env.domain"}`
	}
	two := func(issuer string) string {
		return `{"choice":2,"signin":{"issuer":"` + issuer + `","client_id":"canyon-apps","client_secret":"redacted"},"domain":"canyon.meridian.example","form":"app.env.domain"}`
	}
	for _, tc := range []struct {
		door string
		ok   bool
	}{
		{two("https://login.meridian.example:8443/realms/apps"), false},
		{two("https://login.meridian.example:80"), false},
		{oidc("https://login.meridian.example", "https://login.meridian.example:8443/keys"), false},
		{oidc("https://login.meridian.example", "https://login.meridian.example:443/keys"), true},
		{two("https://other.meridian.example:8443/realms/apps"), true},
	} {
		e, req, _ := withSignin(t)
		req.FrontDoor = json.RawMessage(tc.door)
		c := cluster(t, e, healthy)
		_, err := e.Run(t.Context(), req)
		if refused := errors.Is(err, executor.ErrRequest) && len(writes(c)) == 0; refused == tc.ok || (tc.ok && err != nil) {
			t.Errorf("%s: %v, wrote %v; want taken %v", tc.door, err, writes(c), tc.ok)
		}
	}
}

// CANYON_KUBE_SIGNIN_DIRECT's entries are <host> or <host>=<address>/32: a lower-case DNS name or an IPv4
// address, named once, and one IPv4 address outside link-local. Anything else is refused whole, never used in
// part; unset is none.
func TestParseSigninDirect(t *testing.T) {
	got, err := ParseSigninDirect("sso.corp.example=10.0.4.20/32, keycloak.idp.svc.cluster.local,10.0.4.21=10.0.4.21/32")
	want := []Direct{{Host: "sso.corp.example", Addr: netip.MustParsePrefix("10.0.4.20/32")}, {Host: "keycloak.idp.svc.cluster.local"}, {Host: "10.0.4.21", Addr: netip.MustParsePrefix("10.0.4.21/32")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v, want %+v", got, err, want)
	}
	if got, err := ParseSigninDirect(""); got != nil || err != nil {
		t.Errorf("unset: %+v %v", got, err)
	}
	for _, v := range []string{
		"sso.corp.example,",                              // an empty entry
		"sso.corp.example, ,idp.corp.example",            // another
		"sso.corp.example=10.0.4.0/24",                   // a range: the hole is one address wide
		"sso.corp.example=0.0.0.0/0",                     // every address
		"sso.corp.example=10.0.4.20",                     // no /32
		"sso.corp.example=999.1.2.3/32",                  // no IPv4 address
		"sso.corp.example=fd00::7/128",                   // the floor is IPv4 alone (design/EGRESS.md §2.2)
		"sso.corp.example=::ffff:10.0.4.20/128",          // nor IPv4 in IPv6
		"sso.corp.example=169.254.169.254/32",            // the node's instance metadata
		"sso.corp.example=",                              // an address left out
		"*.corp.example",                                 // a wildcard
		".corp.example",                                  // a leading dot
		"sso.corp.example:8443",                          // a port
		"SSO.corp.example",                               // not lower-case
		"https://sso.corp.example",                       // a URL
		"sso.corp.example=10.0.4.20/32=10.0.4.21/32",     // two addresses
		"sso.corp.example,sso.corp.example=10.0.4.20/32", // a host named twice
	} {
		if got, err := ParseSigninDirect(v); err == nil {
			t.Errorf("%q: %+v, want a refusal", v, got)
		}
	}
}
