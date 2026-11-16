// Package k8s_test holds the deploy base to contracts/executor.md, On Kubernetes, reading the manifests
// themselves: every object decodes as the pinned client-go spells its kind, and the Roles, the executor's
// NetworkPolicy and every image say what the contract says. The kind job applies the same files to a
// real cluster; these are the facts that hold before any cluster exists.
package k8s_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

const executor = "canyon/canyon-executor"

// objects is every object of every file the kustomization lists: built-in kinds decoded strictly, so an
// unknown or misspelt field fails, and kpack's as unstructured. A manifest in the directory that the
// kustomization does not list would never be applied, and fails too.
func objects(t *testing.T) []runtime.Object {
	t.Helper()
	var k struct {
		Resources []string `json:"resources"`
	}
	raw, err := os.ReadFile("kustomization.yaml")
	if err == nil {
		err = utilyaml.Unmarshal(raw, &k)
	}
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files = slices.DeleteFunc(files, func(f string) bool { return f == "kustomization.yaml" })
	if !slices.Equal(slices.Sorted(slices.Values(k.Resources)), files) {
		t.Fatalf("the kustomization lists %v; the directory holds %v", k.Resources, files)
	}
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	var objs []runtime.Object
	for _, f := range k.Resources {
		raw, err := os.ReadFile(f) //nolint:gosec // a file the kustomization lists, beside this test
		if err != nil {
			t.Fatal(err)
		}
		docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := docs.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			obj, _, err := decoder.Decode(doc, nil, nil)
			if runtime.IsNotRegisteredError(err) {
				u := &unstructured.Unstructured{}
				var j []byte
				if j, err = utilyaml.ToJSON(doc); err == nil {
					err = u.UnmarshalJSON(j)
				}
				obj = u
			}
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			objs = append(objs, obj)
		}
	}
	return objs
}

// grant is one rule a binding hands a service account, in a namespace or, "", cluster-wide.
type grant struct {
	namespace, subject string
	rule               rbacv1.PolicyRule
}

func grants(t *testing.T, objs []runtime.Object) []grant {
	t.Helper()
	roles := map[string][]rbacv1.PolicyRule{} // <namespace>/<name>; /<name> for a ClusterRole
	for _, o := range objs {
		switch r := o.(type) {
		case *rbacv1.Role:
			roles[r.Namespace+"/"+r.Name] = r.Rules
		case *rbacv1.ClusterRole:
			roles["/"+r.Name] = r.Rules
		}
	}
	var out []grant
	bind := func(namespace string, ref rbacv1.RoleRef, subjects []rbacv1.Subject) {
		key := "/" + ref.Name
		if ref.Kind == "Role" {
			key = namespace + key
		}
		rules, ok := roles[key]
		if !ok {
			t.Fatalf("a binding in %q names the %s %s, which the deploy does not write", namespace, ref.Kind, ref.Name)
		}
		for _, s := range subjects {
			if s.Kind != rbacv1.ServiceAccountKind {
				t.Fatalf("a binding in %q names the %s %s: only service accounts hold a role", namespace, s.Kind, s.Name)
			}
			for _, r := range rules {
				out = append(out, grant{namespace, s.Namespace + "/" + s.Name, r})
			}
		}
	}
	for _, o := range objs {
		switch b := o.(type) {
		case *rbacv1.RoleBinding:
			bind(b.Namespace, b.RoleRef, b.Subjects)
		case *rbacv1.ClusterRoleBinding:
			bind("", b.RoleRef, b.Subjects)
		}
	}
	return out
}

func grantsAny(r rbacv1.PolicyRule, verbs ...string) bool {
	return slices.ContainsFunc(r.Verbs, func(v string) bool { return slices.Contains(verbs, v) })
}

// TestTheRolesAreTheContracts: the executor holds namespaced Roles and get on the ClusterBuilder canyon,
// and nothing else; Secrets are created and deleted, never read; only it reads Builds or pods in
// canyon-builds; of a pod's subresources, pods/log alone, and pods/exec — which would read an app's
// environment — in canyon-scans alone, for the scan's feed; ServiceAccounts created and deleted in
// canyon-apps by the executor alone, never read or given a token, and no role written by anyone.
func TestTheRolesAreTheContracts(t *testing.T) {
	objs := objects(t)
	for _, o := range objs {
		var rules []rbacv1.PolicyRule
		switch r := o.(type) {
		case *rbacv1.Role:
			rules = r.Rules
		case *rbacv1.ClusterRole:
			rules = r.Rules
		}
		for _, r := range rules {
			if slices.Contains(r.APIGroups, "*") || slices.Contains(r.Resources, "*") || slices.Contains(r.Verbs, "*") {
				t.Errorf("a rule names a wildcard: %+v", r)
			}
			if slices.Contains(r.Resources, "secrets") && slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "create" && v != "delete" }) {
				t.Errorf("a rule on Secrets grants %v: create and delete alone, never a read", r.Verbs)
			}
		}
	}
	accounts := false // the executor's create and delete on ServiceAccounts in canyon-apps
	for _, g := range grants(t, objs) {
		builds := slices.Contains(g.rule.APIGroups, "kpack.io") && slices.Contains(g.rule.Resources, "builds")
		pods := slices.Contains(g.rule.APIGroups, "") && slices.Contains(g.rule.Resources, "pods")
		switch {
		case g.namespace == "canyon":
			t.Errorf("%s holds a Role in canyon, beside the server's Secrets: %+v", g.subject, g.rule)
		case g.namespace == "" && (g.subject != executor || !slices.Equal(g.rule.Resources, []string{"clusterbuilders"}) ||
			!slices.Equal(g.rule.ResourceNames, []string{"canyon"}) || !slices.Equal(g.rule.Verbs, []string{"get"})):
			t.Errorf("%s holds a cluster-wide grant beyond get on the ClusterBuilder canyon: %+v", g.subject, g.rule)
		case g.namespace == "canyon-builds" && (builds || pods) && grantsAny(g.rule, "get", "list", "watch") && g.subject != executor:
			t.Errorf("%s reads Builds or pods in canyon-builds, which hold build values: %+v", g.subject, g.rule)
		}
		for _, r := range g.rule.Resources {
			if strings.HasPrefix(r, "pods/") && r != "pods/log" && (r != "pods/exec" || g.namespace != "canyon-scans") {
				t.Errorf("%s holds %s in %q: of a pod's subresources, pods/log alone, and pods/exec in canyon-scans", g.subject, r, g.namespace)
			}
			if r == "serviceaccounts" || strings.HasPrefix(r, "serviceaccounts/") {
				if g.namespace != "canyon-apps" || g.subject != executor || r != "serviceaccounts" || !slices.Equal(g.rule.Verbs, []string{"create", "delete"}) {
					t.Errorf("%s holds %v on %s in %q: the executor creates and deletes each app env's ServiceAccount in canyon-apps, and nothing more of them: no read, no change, no serviceaccounts/token (#2142)", g.subject, g.rule.Verbs, r, g.namespace)
				}
				accounts = true
			}
			if slices.Contains(g.rule.APIGroups, "rbac.authorization.k8s.io") && grantsAny(g.rule, "create", "update", "patch", "bind", "escalate") {
				t.Errorf("%s may write %v: no one the deploy binds grants roles, so an app env's ServiceAccount stays bound to nothing", g.subject, g.rule.Resources)
			}
		}
	}
	if !accounts {
		t.Error("the executor holds nothing on ServiceAccounts in canyon-apps: a /run with egress creates the env's own (#2142)")
	}
}

// Each of Canyon's namespaces enforces Pod Security Admission and warns and audits at the same level (#2368):
// canyon-apps baseline, as an app runs as whatever user its image names; the rest restricted. The kind job proves
// the API server refuses what each forbids (TestKindPodSecurityRefusesWhatEachNamespaceForbids).
func TestEachNamespaceEnforcesItsPodSecurityLevel(t *testing.T) {
	want := map[string]string{"canyon": "restricted", "canyon-apps": "baseline", "canyon-builds": "restricted", "canyon-scans": "restricted"}
	got := map[string]string{}
	for _, o := range objects(t) {
		ns, ok := o.(*corev1.Namespace)
		if !ok {
			continue
		}
		level := ns.Labels["pod-security.kubernetes.io/enforce"]
		for _, mode := range []string{"warn", "audit"} {
			if l := ns.Labels["pod-security.kubernetes.io/"+mode]; l != level {
				t.Errorf("%s %ss at %q, enforcing %q: want the same level", ns.Name, mode, l, level)
			}
		}
		got[ns.Name] = level
	}
	if !maps.Equal(got, want) {
		t.Errorf("the namespaces enforce %v, want %v", got, want)
	}
}

// TestEveryContainerIsBounded: every container the deploy runs has CPU and memory requests and limits,
// so no pod is BestEffort, the first evicted, and a read-only root filesystem.
func TestEveryContainerIsBounded(t *testing.T) {
	for _, o := range objects(t) {
		d, ok := o.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, c := range append(d.Spec.Template.Spec.InitContainers, d.Spec.Template.Spec.Containers...) {
			if r := c.Resources; r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() || r.Limits.Cpu().IsZero() || r.Limits.Memory().IsZero() {
				t.Errorf("%s/%s: requests and limits of CPU and memory, or the pod is evicted first", d.Name, c.Name)
			}
			if s := c.SecurityContext; s == nil || s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem {
				t.Errorf("%s/%s: a read-only root filesystem", d.Name, c.Name)
			}
		}
	}
}

// TestTheExecutorAdmitsTheServerAlone: the executor's own NetworkPolicy is the single enforcement point
// of who may call it — no token — so it alone selects the executor's pods, and admits TCP 7778 from the
// server's pods and from no other pod the deploy writes. One executor runs, stopped before the next starts.
func TestTheExecutorAdmitsTheServerAlone(t *testing.T) {
	deployments := map[string]*appsv1.Deployment{}
	var selecting []*networkingv1.NetworkPolicy
	objs := objects(t)
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok {
			deployments[d.Name] = d
		}
	}
	exec, server := deployments["canyon-executor"], deployments["canyon-server"]
	if exec == nil || server == nil {
		t.Fatal("the deploy writes no Deployment canyon-executor or canyon-server")
	}
	replicas := int32(1) // unset is one
	if exec.Spec.Replicas != nil {
		replicas = *exec.Spec.Replicas
	}
	if replicas != 1 || exec.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("the executor runs %d replicas by %q: one, stopped before the next starts (Recreate)", replicas, exec.Spec.Strategy.Type)
	}
	for _, o := range objs {
		np, ok := o.(*networkingv1.NetworkPolicy)
		if !ok || np.Namespace != exec.Namespace {
			continue
		}
		if sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err != nil || sel.Matches(labels.Set(exec.Spec.Template.Labels)) {
			selecting = append(selecting, np)
		}
	}
	if len(selecting) != 1 || !slices.Contains(selecting[0].Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || len(selecting[0].Spec.Ingress) != 1 {
		t.Fatalf("%d NetworkPolicies select the executor's pods: its own alone, one ingress rule", len(selecting))
	}
	in := selecting[0].Spec.Ingress[0]
	if len(in.From) != 1 || in.From[0].PodSelector == nil || in.From[0].NamespaceSelector != nil || in.From[0].IPBlock != nil {
		t.Fatalf("the executor admits %+v: the server's pods alone", in.From)
	}
	from, err := metav1.LabelSelectorAsSelector(in.From[0].PodSelector)
	if err != nil || from.Empty() || !from.Matches(labels.Set(server.Spec.Template.Labels)) {
		t.Fatalf("the executor's policy admits %v, not the server's pods", in.From[0].PodSelector)
	}
	for n, d := range deployments {
		if n != server.Name && from.Matches(labels.Set(d.Spec.Template.Labels)) {
			t.Errorf("the executor's policy admits %s's pods too", n)
		}
	}
	if len(in.Ports) != 1 || in.Ports[0].Port == nil || in.Ports[0].Port.IntValue() != 7778 || in.Ports[0].Protocol == nil || *in.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("the executor admits the server on %+v: TCP 7778 alone", in.Ports)
	}
}

// privateRanges is what the server's NetworkPolicy leaves out of its public 443: no other app, no control
// plane, no instance metadata. executor/kube/objects.go's private, which an app's, the egress gateway's and
// the builds' policies leave out, holds these and six more.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

// TestTheBuildsReachNoPrivateAddress is invariant 12 on Kubernetes (contracts/executor.md): NetworkPolicies add
// up, so every one of canyon-builds, together, lets a build pod out to kube-dns on 53 alone and to TCP 80 and 443,
// one port each, at 0.0.0.0/0 less private addresses, and nowhere else — none to anywhere, none with a peer holding
// the instance metadata address or Azure's WireServer. Which private addresses, exactly, is executor/kube's
// TestTheBuildsRefuseTheOneList's: objects.go's private.
func TestTheBuildsReachNoPrivateAddress(t *testing.T) {
	never := []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("168.63.129.16")}
	var held int
	for _, o := range objects(t) {
		np, ok := o.(*networkingv1.NetworkPolicy)
		if !ok || np.Namespace != "canyon-builds" {
			continue
		}
		if sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err == nil && sel.Empty() && slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
			held++
		}
		for _, r := range np.Spec.Egress {
			if len(r.To) == 0 || len(r.Ports) == 0 || slices.ContainsFunc(r.Ports, func(p networkingv1.NetworkPolicyPort) bool { return p.Port == nil || p.EndPort != nil }) {
				t.Errorf("%s lets a build pod out to every address, or on every port or a range of them: %+v", np.Name, r)
				continue
			}
			for _, to := range r.To {
				switch {
				case to.IPBlock != nil:
					if to.IPBlock.CIDR != "0.0.0.0/0" || !slices.Equal(tcpPorts(r.Ports), []int{80, 443}) || slices.ContainsFunc(privateRanges, func(f string) bool { return !slices.Contains(to.IPBlock.Except, f) }) {
						t.Errorf("%s lets a build pod out to %+v on %v: TCP 80 and 443 at 0.0.0.0/0 less every private address alone", np.Name, to.IPBlock, tcpPorts(r.Ports))
					}
					for _, a := range never {
						if within(to.IPBlock.CIDR, a) && !slices.ContainsFunc(to.IPBlock.Except, func(x string) bool { return within(x, a) }) {
							t.Errorf("%s lets a build pod reach %s", np.Name, a)
						}
					}
				case to.NamespaceSelector != nil && to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kube-system" && to.PodSelector != nil && to.PodSelector.MatchLabels["k8s-app"] == "kube-dns":
					for _, p := range r.Ports {
						if p.Port.IntValue() != 53 {
							t.Errorf("%s lets a build pod reach kube-dns on %+v: 53 alone", np.Name, r.Ports)
						}
					}
				default:
					t.Errorf("%s lets a build pod out to %+v", np.Name, to)
				}
			}
		}
	}
	if held == 0 {
		t.Fatal("no NetworkPolicy holds every pod of canyon-builds")
	}
}

// within is whether cidr holds a.
func within(cidr string, a netip.Addr) bool {
	p, err := netip.ParsePrefix(cidr)
	return err == nil && p.Contains(a)
}

// TestTheServerHasANetworkPolicy: contracts/executor.md, On Kubernetes says the deploy writes a
// NetworkPolicy for the router, the server and the executor. The server's admits 7777 from anywhere
// (the org's load balancer fronts it, as the router's policy does), never 7780 — nothing on
// Kubernetes calls the apps' key-set listener, which is the Docker executor's alone — and the
// receiver's 7781 and the policy's 7782 from Canyon's egress gateway's pods alone (contracts/routes.md,
// Egress control; #2286: a second ingress rule, where there was one; #2113: the policy), and egresses
// to DNS, the executor's 7778, TCP 443 at public addresses excluding the same private ranges
// publicEgress does (never an in-cluster service), then Postgres, the apps database and the API server
// (placeholder CIDRs at the fixed egress[3], egress[4] and egress[5] the install guide's overlay
// JSON-patches by path).
func TestTheServerHasANetworkPolicy(t *testing.T) {
	objs := objects(t)
	var server *appsv1.Deployment
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == "canyon-server" {
			server = d
		}
	}
	if server == nil {
		t.Fatal("the deploy writes no Deployment canyon-server")
	}
	var selecting []*networkingv1.NetworkPolicy
	for _, o := range objs {
		np, ok := o.(*networkingv1.NetworkPolicy)
		if !ok || np.Namespace != server.Namespace {
			continue
		}
		if sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err == nil && sel.Matches(labels.Set(server.Spec.Template.Labels)) {
			selecting = append(selecting, np)
		}
	}
	if len(selecting) != 1 {
		t.Fatalf("%d NetworkPolicies select the server's pods: its own alone", len(selecting))
	}
	np := selecting[0]
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Fatalf("the server's policy types: %v, want Ingress and Egress", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 2 || len(np.Spec.Ingress[0].From) != 0 {
		t.Fatalf("the server admits %+v: 7777 from anywhere, as the router's policy admits 8080 — no `from` — and the receiver from the gateway", np.Spec.Ingress)
	}
	if ports := np.Spec.Ingress[0].Ports; len(ports) != 1 || ports[0].Port == nil || ports[0].Port.IntValue() != 7777 {
		t.Fatalf("the server admits ingress on %+v: TCP 7777 alone, never 7780 (the Docker executor's apps' key-set listener)", ports)
	}
	if r := np.Spec.Ingress[1]; len(r.From) != 1 || r.From[0].NamespaceSelector != nil || r.From[0].IPBlock != nil || r.From[0].PodSelector == nil ||
		!maps.Equal(r.From[0].PodSelector.MatchLabels, map[string]string{"app.kubernetes.io/name": "canyon-egress"}) || len(r.From[0].PodSelector.MatchExpressions) != 0 ||
		!slices.Equal(tcpPorts(r.Ports), []int{7781, 7782}) {
		t.Fatalf("the server's ingress[1] is %+v: TCP 7781, the receiver, and 7782, the policy, from canyon's pods labelled canyon-egress alone", r)
	}
	if n := len(np.Spec.Egress); n != 6 {
		t.Fatalf("the server's policy has %d egress rule(s), want 6: DNS, the executor, public 443, Postgres, the apps database, the API server", n)
	}
	var reachesExecutor bool
	for _, to := range np.Spec.Egress[1].To {
		if to.PodSelector != nil {
			if sel, err := metav1.LabelSelectorAsSelector(to.PodSelector); err == nil && !sel.Empty() && sel.Matches(labels.Set(map[string]string{"app.kubernetes.io/name": "canyon-executor"})) {
				reachesExecutor = true
			}
		}
	}
	if !reachesExecutor || !slices.ContainsFunc(np.Spec.Egress[1].Ports, func(p networkingv1.NetworkPolicyPort) bool { return p.Port != nil && p.Port.IntValue() == 7778 }) {
		t.Errorf("egress[1] is not the executor on TCP 7778: %+v", np.Spec.Egress[1])
	}
	// egress[2] is public 443, excluding the same private ranges publicEgress does — 0.0.0.0/0 alone
	// would hand back the kube API and every in-cluster service, though this policy deliberately
	// withholds cluster reach.
	public := np.Spec.Egress[2]
	if len(public.To) != 1 || public.To[0].IPBlock == nil || public.To[0].IPBlock.CIDR != "0.0.0.0/0" ||
		!slices.Equal(public.To[0].IPBlock.Except, privateRanges) ||
		!slices.ContainsFunc(public.Ports, func(p networkingv1.NetworkPolicyPort) bool { return p.Port != nil && p.Port.IntValue() == 443 }) {
		t.Errorf("egress[2] is not public 443 excluding %v: %+v", privateRanges, public)
	}
	// the install guide's overlay JSON-patches these three placeholders by their fixed index and JSON path
	// (/spec/egress/3/..., /spec/egress/4/... and /spec/egress/5/...): pin each, so reordering this list
	// cannot silently break every install's kustomize patch. They sit last on purpose (the install guide, Install).
	placeholder := func(e networkingv1.NetworkPolicyEgressRule, ports ...int) bool {
		return len(e.To) == 1 && e.To[0].IPBlock != nil && e.To[0].IPBlock.CIDR == "0.0.0.0/32" && slices.Equal(tcpPorts(e.Ports), ports)
	}
	if !placeholder(np.Spec.Egress[3], 5432) {
		t.Errorf("egress[3] is not the Postgres placeholder 0.0.0.0/32:5432 the install guide patches: %+v", np.Spec.Egress[3])
	}
	if !placeholder(np.Spec.Egress[4], 5433) {
		t.Errorf("egress[4] is not the apps database placeholder 0.0.0.0/32:5433 the install guide patches: %+v", np.Spec.Egress[4])
	}
	if !placeholder(np.Spec.Egress[5], 443, 6443) {
		t.Errorf("egress[5] is not the API server placeholder 0.0.0.0/32 on 443 and 6443 the install guide patches: %+v", np.Spec.Egress[5])
	}
}

// tcpPorts is a rule's ports, each TCP and a number, in order; any other is -1.
func tcpPorts(ps []networkingv1.NetworkPolicyPort) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = -1
		if p.Port != nil && p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP {
			out[i] = p.Port.IntValue()
		}
	}
	return out
}

// TestEveryImageIsPinnedByDigest: every image the deploy names — its pods', the placeholders its overlays
// replace, kpack's stack and buildpacks — is by digest, never a tag.
func TestEveryImageIsPinnedByDigest(t *testing.T) {
	for _, o := range objects(t) {
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range images(u) {
			if _, err := name.NewDigest(img); err != nil {
				t.Errorf("%s: %v", img, err)
			}
		}
	}
}

// images is every value of a field named image, however deep.
func images(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "image" {
				out = append(out, s)
			} else {
				out = append(out, images(e)...)
			}
		}
	case []any:
		for _, e := range x {
			out = append(out, images(e)...)
		}
	}
	return out
}

// Every app runs on canyon-run and builds on canyon-build (contracts/executor.md): the stack's run and
// build images are Canyon's own, placeholders the overlay replaces by digest, so a base without those
// patches builds nothing rather than building and running apps on Paketo's images, which lack libpq5
// (and the run image libatomic1 and libsqlite3-0); and kpack reads them with the credential the builds
// push with.
func TestTheStackRunsAppsOnCanyonRun(t *testing.T) {
	for _, o := range objects(t) {
		u, ok := o.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "ClusterStack" {
			continue
		}
		run, _, _ := unstructured.NestedString(u.Object, "spec", "runImage", "image")
		build, _, _ := unstructured.NestedString(u.Object, "spec", "buildImage", "image")
		sa, _, _ := unstructured.NestedStringMap(u.Object, "spec", "serviceAccountRef")
		runRepo, _, _ := strings.Cut(run, "@")
		buildRepo, _, _ := strings.Cut(build, "@")
		if runRepo != "registry.invalid/canyon-run" || buildRepo != "registry.invalid/canyon-build" || sa["name"] != "canyon-build" || sa["namespace"] != "canyon-builds" {
			t.Errorf("ClusterStack %s builds apps on %s and runs them on %s, read with %v", u.GetName(), build, run, sa)
		}
		return
	}
	t.Fatal("no ClusterStack in the base")
}

// An upgrade reaches the running router (#1260). nginx.conf is a subPath mount, which never sees a
// ConfigMap update, and nginx never re-reads it: only new pods do. The pod template carries the config's
// sha256, so a changed nginx.conf changes the template and `kubectl apply` rolls the router.
func TestTheRouterRollsWhenItsConfigChanges(t *testing.T) {
	var conf, annotated string
	for _, o := range objects(t) {
		switch o := o.(type) {
		case *corev1.ConfigMap:
			if o.Name == "canyon-router" {
				conf = o.Data["nginx.conf"]
			}
		case *appsv1.Deployment:
			if o.Name == "canyon-router" {
				annotated = o.Spec.Template.Annotations["canyon.nginx-conf-sha256"]
			}
		}
	}
	sum := sha256.Sum256([]byte(conf))
	if want := hex.EncodeToString(sum[:]); annotated != want {
		t.Errorf("the router's pod template has canyon.nginx-conf-sha256 %q, nginx.conf's is %q: set it, or an upgrade never reaches the running router", annotated, want)
	}
}

// The router says why it cannot reach an app, in contracts/executor.md's words: a Service that does not
// resolve — nothing deployed there, so no upstream address — is a 404 page, one that resolves and does
// not answer a 502 page, and an app's own answers are not intercepted (canyonv2-5xyy).
func TestTheRouterSaysWhyItCannotReachAnApp(t *testing.T) {
	contract, err := os.ReadFile(filepath.Join("..", "..", "contracts", "executor.md"))
	if err != nil {
		t.Fatal(err)
	}
	prose := strings.Join(strings.Fields(string(contract)), " ") // the contract wraps its lines; the page is one
	var conf string
	for _, o := range objects(t) {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == "canyon-router" {
			conf = cm.Data["nginx.conf"]
		}
	}
	for _, want := range []string{
		"error_page 502 504 = @unreachable;",
		`if ($upstream_addr = "") { return 404 "<!doctype html><html lang='en'><meta charset='utf-8'><meta name='viewport' content='width=device-width, initial-scale=1'><title>No app here</title><p style='font-family: system-ui, sans-serif; max-width: 36rem; margin: 4rem auto; padding: 0 1rem; line-height: 1.5'>No app is running at this address. It may have been deleted, or never deployed here.</p></html>\n"; }`,
		`return 502 "<!doctype html><html lang='en'><meta charset='utf-8'><meta name='viewport' content='width=device-width, initial-scale=1'><title>App not answering</title><p style='font-family: system-ui, sans-serif; max-width: 36rem; margin: 4rem auto; padding: 0 1rem; line-height: 1.5'>This app isn't answering right now. It may be starting, or it may have stopped.</p></html>\n";`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the router's nginx.conf lacks %s", want)
		}
	}
	for _, page := range []string{"No app is running at this address. It may have been deleted, or never deployed here.", "This app isn't answering right now. It may be starting, or it may have stopped."} {
		if !strings.Contains(prose, page) {
			t.Errorf("contracts/executor.md does not say %q", page)
		}
	}
	if strings.Contains(conf, "proxy_intercept_errors") {
		t.Error("the router intercepts the app's own errors")
	}
}

// TestTheREADMEsOverlayKeepsEveryVariableTheBaseSets applies the install guide's overlay to the base's Deployments
// the way kustomize applies it — a patch that is a list is a JSON patch, any other a strategic merge, which
// merges env by name — and holds every variable the base sets on a container to still be set after. A JSON
// patch that replaces a whole env list drops the rest: the overlay once dropped the executor's two scanner
// images, and a copy of it blocked every review (#1894).
func TestTheREADMEsOverlayKeepsEveryVariableTheBaseSets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "src", "content", "docs", "self-host", "kubernetes.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, block, found := strings.Cut(string(raw), "Then write your overlay:\n\n```yaml\n")
	block, _, closed := strings.Cut(block, "```")
	if !found || !closed {
		t.Fatal("the install guide has no yaml block after \"Then write your overlay:\"")
	}
	var overlay struct {
		Patches []struct {
			Target struct{ Kind, Name string }
			Patch  string
		}
	}
	if err := utilyaml.Unmarshal([]byte(block), &overlay); err != nil {
		t.Fatalf("the install guide's overlay: %v", err)
	}
	targets := map[string]bool{}
	for _, p := range overlay.Patches {
		if p.Target.Kind == "Deployment" {
			targets[p.Target.Name] = false
		}
	}
	for _, obj := range objects(t) {
		base, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		doc, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range overlay.Patches {
			if p.Target.Kind != "Deployment" || p.Target.Name != base.Name {
				continue
			}
			patch, err := utilyaml.ToJSON([]byte(p.Patch))
			if err != nil {
				t.Fatalf("%s's patch: %v", base.Name, err)
			}
			if bytes.HasPrefix(bytes.TrimSpace(patch), []byte("[")) {
				var ops jsonpatch.Patch
				if ops, err = jsonpatch.DecodePatch(patch); err == nil {
					doc, err = ops.Apply(doc)
				}
			} else {
				doc, err = strategicpatch.StrategicMergePatch(doc, patch, appsv1.Deployment{})
			}
			if err != nil {
				t.Fatalf("%s's patch does not apply: %v", base.Name, err)
			}
			targets[base.Name] = true
		}
		var after appsv1.Deployment
		if err := json.Unmarshal(doc, &after); err != nil {
			t.Fatal(err)
		}
		if len(after.Spec.Template.Spec.Containers) != len(base.Spec.Template.Spec.Containers) { // a merge names a container by name: a misspelt one is a second container
			t.Errorf("the install guide's overlay adds a container to %s: a patch names a container the base does not have", base.Name)
		}
		kept := map[string]bool{}
		for _, a := range after.Spec.Template.Spec.Containers {
			for _, e := range a.Env {
				kept[a.Name+"/"+e.Name] = true
			}
		}
		for _, c := range base.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if !kept[c.Name+"/"+e.Name] {
					t.Errorf("the install guide's overlay drops %s from %s's %s container", e.Name, base.Name, c.Name)
				}
			}
		}
	}
	if len(targets) == 0 {
		t.Fatal("the install guide's overlay patches no Deployment")
	}
	for name, applied := range targets {
		if !applied {
			t.Errorf("the install guide's overlay patches a Deployment %q the base does not have", name)
		}
	}
}
