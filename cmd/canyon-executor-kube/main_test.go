package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/getcanyon-ai/canyon/executor/kube"
)

const identity = "ghcr.io/getcanyon-ai/canyon-identity@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The environment names the executor: a registry host, canyon-identity's image by digest and each other
// image's if set, the database's and the database sources' CIDRs, and the sign-in hosts that go direct, each
// with its address as an IPv4 /32 or none — anything else refuses the start, the error naming the setting. An ECR host brings ECR's API, every call of it bounded, and its credential;
// another registry has neither.
func TestTheEnvironmentNamesTheExecutor(t *testing.T) {
	env := func(edit map[string]string) func(string) string {
		m := map[string]string{"CANYON_KUBE_REGISTRY": "registry.example:5000", "CANYON_IMAGE_IDENTITY": identity, "CANYON_KUBE_DB_CIDRS": "10.0.0.0/16, 10.1.0.0/16"}
		for k, v := range edit {
			m[k] = v
		}
		return func(k string) string { return m[k] }
	}
	e, err := executorOf(t.Context(), env(nil))
	if err != nil || e.Identity != identity || len(e.DBCIDRs) != 2 || e.DBCIDRs[1].String() != "10.1.0.0/16" || e.ECR != nil || e.Auth != nil ||
		e.Opengrep+e.OAuth2Proxy+e.Busybox+e.BusyboxStatic != "" || e.ScanDBCIDRs != nil || e.SigninDirect != nil { // unset: the pins, no source CIDR, no sign-in host
		t.Fatalf("%+v %v", e, err)
	}
	image := func(repo string) string { return "mirror.example/" + repo + "@sha256:" + strings.Repeat("5", 64) }
	e, err = executorOf(t.Context(), env(map[string]string{"CANYON_IMAGE_OPENGREP": image("opengrep"), "CANYON_IMAGE_OAUTH2_PROXY": image("oauth2-proxy"),
		"CANYON_IMAGE_BUSYBOX": image("busybox"), "CANYON_IMAGE_BUSYBOX_STATIC": image("busybox-static"), "CANYON_KUBE_SCANDB_CIDRS": "10.9.0.0/16",
		"CANYON_KUBE_APP_CPU_REQUEST": "250m", "CANYON_KUBE_APP_MEMORY_REQUEST": "512Mi", "CANYON_KUBE_SIGNIN_DIRECT": "sso.corp.example=10.30.0.7/32, keycloak.idp.svc.cluster.local"}))
	if err != nil || e.Opengrep != image("opengrep") || e.OAuth2Proxy != image("oauth2-proxy") || e.Busybox != image("busybox") || e.BusyboxStatic != image("busybox-static") ||
		len(e.ScanDBCIDRs) != 1 || e.ScanDBCIDRs[0].String() != "10.9.0.0/16" || e.AppCPU != "250m" || e.AppMemory != "512Mi" ||
		len(e.SigninDirect) != 2 || e.SigninDirect[0] != (kube.Direct{Host: "sso.corp.example", Addr: netip.MustParsePrefix("10.30.0.7/32")}) || e.SigninDirect[1] != (kube.Direct{Host: "keycloak.idp.svc.cluster.local"}) {
		t.Fatalf("set: %+v %v", e, err)
	}
	for _, tc := range []struct{ setting, value string }{
		{"CANYON_KUBE_REGISTRY", ""},
		{"CANYON_IMAGE_IDENTITY", ""},
		{"CANYON_IMAGE_IDENTITY", "ghcr.io/getcanyon-ai/canyon-identity:v1"},
		{"CANYON_IMAGE_OPENGREP", "ghcr.io/getcanyon-ai/opengrep:1.30.0"},
		{"CANYON_IMAGE_OAUTH2_PROXY", "quay.io/oauth2-proxy/oauth2-proxy:v7.15.4"},
		{"CANYON_IMAGE_BUSYBOX", "library/busybox@sha256:" + strings.Repeat("5", 64)}, // no registry
		{"CANYON_IMAGE_BUSYBOX_STATIC", "docker.io/library/busybox@sha256:5cec"},
		{"CANYON_IMAGE_TRIVY", "ghcr.io/aquasecurity/trivy:0.74.0"},
		{"CANYON_IMAGE_GRYPE", "anchore/grype:v0.90.0"},
		{"CANYON_IMAGE_CHECKOV", "bridgecrew/checkov:3.2.0"},
		{"CANYON_KUBE_DB_CIDRS", "10.0.0.1/16"},
		{"CANYON_KUBE_SCANDB_CIDRS", "mirror.example"},
		{"CANYON_KUBE_SCANDB_CIDRS", "169.254.0.0/16"}, // the refresh reaches every port there: the node's instance metadata
		{"CANYON_KUBE_SCANDB_CIDRS", "10.0.0.0/8, 169.254.169.254/32"},
		{"CANYON_KUBE_SCANDB_CIDRS", "0.0.0.0/0"}, // holds 169.254.0.0/16
		{"CANYON_KUBE_SCANDB_CIDRS", "fe80::/10"},
		{"CANYON_KUBE_APP_CPU_REQUEST", "a lot"},
		{"CANYON_KUBE_APP_CPU_REQUEST", "2"}, // over the app's 1 CPU limit
		{"CANYON_KUBE_APP_CPU_REQUEST", "0"},
		{"CANYON_KUBE_APP_MEMORY_REQUEST", "2Gi"},
		{"CANYON_KUBE_SIGNIN_DIRECT", "sso.corp.example=10.30.0.0/24"}, // a range: the floor's hole is one address wide
		{"CANYON_KUBE_SIGNIN_DIRECT", "sso.corp.example=10.30.0.7"},    // no /32
		{"CANYON_KUBE_SIGNIN_DIRECT", "sso.corp.example,"},
		{"CANYON_KUBE_SIGNIN_DIRECT", "sso.corp.example=fd00::7/128"}, // the floor is IPv4 alone (design/EGRESS.md §2.2)
		{"CANYON_KUBE_SIGNIN_DIRECT", "sso.corp.example=169.254.169.254/32"},
		{"CANYON_KUBE_SIGNIN_DIRECT", "*.corp.example"},
	} {
		if _, err := executorOf(t.Context(), env(map[string]string{tc.setting: tc.value})); err == nil || !strings.HasPrefix(err.Error(), tc.setting) {
			t.Errorf("%s=%q: %v", tc.setting, tc.value, err)
		}
	}
	if e, err := executorOf(t.Context(), env(map[string]string{"CANYON_KUBE_SCANDB_CIDRS": "10.0.0.0/8"})); err != nil || len(e.ScanDBCIDRs) != 1 {
		t.Errorf("CANYON_KUBE_SCANDB_CIDRS=10.0.0.0/8: %+v %v", e, err)
	}
	awsEnv(t, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDPOD")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	e, err = executorOf(t.Context(), env(map[string]string{"CANYON_KUBE_REGISTRY": "123456789012.dkr.ecr.eu-central-1.amazonaws.com"}))
	if err != nil || e.ECR == nil || e.Auth == nil {
		t.Fatalf("ECR: %+v %v", e, err)
	}
	api, ok := e.ECR.(*ecr.Client)
	if !ok {
		t.Fatalf("ECR is %T", e.ECR)
	}
	if c, ok := api.Options().HTTPClient.(*http.Client); !ok || c.Timeout == 0 {
		t.Fatalf("ECR's calls are unbounded: %T", api.Options().HTTPClient)
	}
}

// awsEnv leaves the AWS SDK only what a test sets: no profile, no files, no credential, and instance
// metadata at imds.
func awsEnv(t *testing.T, imds string) {
	none := filepath.Join(t.TempDir(), "none")
	for k, v := range map[string]string{"AWS_CONFIG_FILE": none, "AWS_SHARED_CREDENTIALS_FILE": none, "AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
		"AWS_SESSION_TOKEN": "", "AWS_ROLE_ARN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "", "AWS_CONTAINER_CREDENTIALS_FULL_URI": "", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_EC2_METADATA_DISABLED": "", "AWS_EC2_METADATA_SERVICE_ENDPOINT": imds} {
		t.Setenv(k, v)
	}
}

// ECR's credential is the pod's own, never the node's instance role through instance metadata, which a
// pod on an EKS node can reach: an executor without one refuses to start, naming IRSA.
func TestECRTakesThePodsCredentialNeverTheNodes(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
			_, _ = w.Write([]byte("token"))
		case "/latest/meta-data/iam/security-credentials/":
			_, _ = w.Write([]byte("node-role"))
		case "/latest/meta-data/iam/security-credentials/node-role":
			_, _ = fmt.Fprintf(w, `{"Code":"Success","AccessKeyId":"AKIDNODE","SecretAccessKey":"s","Token":"t","Expiration":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer node.Close()
	awsEnv(t, node.URL)
	if _, err := ecrClient(t.Context(), "eu-central-1"); err == nil || !strings.Contains(err.Error(), "IRSA") {
		t.Fatalf("no credential of the pod's own: %v", err)
	}
}

// A scan's feed reaches the named container of the named pod with stdin, over pods/exec, and nothing is
// asked of its stdout.
func TestExecStreamsStdinToTheNamedContainer(t *testing.T) {
	asked := make(chan *url.URL, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	cfg := &rest.Config{Host: srv.URL}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = execOver(cfg, client)(t.Context(), "canyon-scans", "canyon-scan-checkov-1-pod", "feed", []string{"sh", "-c", "tar -x -f - -C /scan"}, strings.NewReader("x"))
	u := <-asked
	q := u.Query()
	if err == nil || u.Path != "/api/v1/namespaces/canyon-scans/pods/canyon-scan-checkov-1-pod/exec" || q.Get("container") != "feed" ||
		!slices.Equal(q["command"], []string{"sh", "-c", "tar -x -f - -C /scan"}) || q.Get("stdin") != "true" || q.Get("stdout") == "true" || q.Get("tty") == "true" {
		t.Fatalf("exec asked %s: %v", u, err)
	}
}

// Before it serves, the executor starts its upkeep — canyon-registry's only when the registry has a
// credential: without one (kind's registry:2) it has none to write, and would fail on every try.
func TestStartKeepsTheRegistrySecretOnlyWithACredential(t *testing.T) {
	for name, auth := range map[string]authn.Authenticator{"no credential": nil, "a credential": &authn.Basic{Username: "AWS", Password: "token"}} {
		c := fake.NewClientset()
		builds := schema.GroupVersionResource{Group: "kpack.io", Version: "v1alpha2", Resource: "builds"}
		e := &kube.Executor{Client: c, Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{builds: "BuildList"}), Registry: "registry.example", Auth: auth}
		ctx, cancel := context.WithCancel(t.Context())
		if err := start(ctx, e); err != nil {
			t.Fatal(err)
		}
		var written bool
		for range 50 {
			if _, err := c.CoreV1().Secrets(kube.BuildNamespace).Get(ctx, "canyon-registry", metav1.GetOptions{}); err == nil {
				written = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if written != (auth != nil) {
			t.Errorf("%s: canyon-registry written %v", name, written)
		}
	}
}

// The executor's clients are paced for a sign-in change, which re-PUTs every app's access file before it answers:
// about 100 apps, a ConfigMap Get and Update each, within a few seconds, never client-go's 5 a second (#2943). Built
// without a cluster: a client reaches its API server only when called.
func TestTheClientIsPaced(t *testing.T) {
	e := &kube.Executor{}
	if err := connect(e, &rest.Config{Host: "https://127.0.0.1:6443"}); err != nil {
		t.Fatal(err)
	}
	limiter := e.Client.(*kubernetes.Clientset).CoreV1().RESTClient().GetRateLimiter()
	burst := 0
	for burst < 200 && limiter.TryAccept() {
		burst++
	}
	if limiter.QPS() != 50 || burst != 100 {
		t.Fatalf("the client takes %v a second in bursts of %d, want 50 in bursts of 100", limiter.QPS(), burst)
	}
}
