// Command canyon-executor-kube is the executor on Kubernetes (contracts/executor.md, On Kubernetes): the
// calls canyon-executor serves, and PUT /access and POST /probe, from its pod in canyon as the service
// account canyon-executor, with no Docker socket.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/executor/kube"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ecrHost is an ECR registry's host as the contract spells it, <account>.dkr.ecr.<region>.amazonaws.com.
var ecrHost = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$`)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	e, err := executorOf(ctx, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	if err := connect(e, cfg); err != nil {
		log.Fatal(err)
	}
	if err := start(ctx, e); err != nil {
		log.Fatal(err)
	}
	addr := cmp.Or(os.Getenv("CANYON_EXECUTOR_ADDR"), ":7778")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("canyon-executor-kube listening on %s", addr) //nolint:gosec // our own env, not user input
	// executor.IdleTimeout: the server drops an idle connection at half of it, before the executor closes it
	srv := &http.Server{Handler: kube.Handler(e), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: executor.IdleTimeout}
	if err := executor.Serve(ctx, srv, ln); err != nil {
		log.Fatal(err)
	}
}

// apiQPS and apiBurst pace the executor's calls to the API server (#2943): a sign-in change re-PUTs about 100 apps'
// access files, a ConfigMap Get and Update each, so ~400 calls finish in about 6 s rather than client-go's 5 a second
// taking 80, while one service account's few dozen a second stay well inside the API server's fairness.
const apiQPS, apiBurst = 50, 100

// connect gives e its clients of the cluster at cfg, paced at apiQPS in bursts of apiBurst.
func connect(e *kube.Executor, cfg *rest.Config) error {
	cfg.QPS, cfg.Burst = apiQPS, apiBurst
	var err error
	if e.Client, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	if e.Dynamic, err = dynamic.NewForConfig(cfg); err != nil {
		return err
	}
	e.Exec = execOver(cfg, e.Client)
	return nil
}

// executorOf is the executor the environment names: CANYON_KUBE_REGISTRY, the registry host every image
// lives under, which on ECR brings ECR's API and credential; CANYON_KUBE_DB_CIDRS and
// CANYON_KUBE_SCANDB_CIDRS; and the images it runs by digest, CANYON_IMAGE_IDENTITY required
// (contracts/executor.md, Images and database sources). A bad value refuses the start, naming it.
func executorOf(ctx context.Context, getenv func(string) string) (*kube.Executor, error) {
	e := &kube.Executor{Registry: getenv("CANYON_KUBE_REGISTRY")}
	if _, err := name.NewRegistry(e.Registry, name.StrictValidation); err != nil {
		return nil, fmt.Errorf("CANYON_KUBE_REGISTRY must be a registry host: %w", err)
	}
	var err error
	for _, s := range []struct {
		setting string
		image   *string
	}{{"CANYON_IMAGE_IDENTITY", &e.Identity}, {"CANYON_IMAGE_OPENGREP", &e.Opengrep}, {"CANYON_IMAGE_ESLINT", &e.ESLint}, {"CANYON_IMAGE_OAUTH2_PROXY", &e.OAuth2Proxy},
		{"CANYON_IMAGE_BUSYBOX", &e.Busybox}, {"CANYON_IMAGE_BUSYBOX_STATIC", &e.BusyboxStatic}} {
		if *s.image, err = executor.ImageSetting(getenv, s.setting); err != nil {
			return nil, err
		}
	}
	if e.Identity == "" {
		return nil, errors.New("CANYON_IMAGE_IDENTITY must be set: canyon-identity's image, by digest")
	}
	if err = executor.Configure(getenv); err != nil {
		return nil, err
	}
	if err = executor.Limit(getenv, 6, 14); err != nil { // the cluster's: builds are kpack's, scans its Jobs (contracts/executor.md, Busy): 4 builds and 2 rebases, 10 scans, 2 patch scans, a deploy's image scan and the rescan's one
		return nil, err
	}
	if e.AppCPU, err = request(getenv, "CANYON_KUBE_APP_CPU_REQUEST", kube.AppCPULimit); err != nil {
		return nil, err
	}
	if e.AppMemory, err = request(getenv, "CANYON_KUBE_APP_MEMORY_REQUEST", kube.AppMemoryLimit); err != nil {
		return nil, err
	}
	if e.DBCIDRs, err = cidrs(getenv, "CANYON_KUBE_DB_CIDRS"); err != nil {
		return nil, err
	}
	if e.ScanDBCIDRs, err = cidrs(getenv, "CANYON_KUBE_SCANDB_CIDRS"); err != nil {
		return nil, err
	}
	if e.SigninDirect, err = kube.ParseSigninDirect(getenv("CANYON_KUBE_SIGNIN_DIRECT")); err != nil {
		return nil, fmt.Errorf("CANYON_KUBE_SIGNIN_DIRECT: %w", err)
	}
	for _, p := range e.ScanDBCIDRs { // a refresh reaches every port there: a range holding link-local would reach the node's IMDS
		if p.Overlaps(netip.MustParsePrefix("169.254.0.0/16")) || p.Overlaps(netip.MustParsePrefix("fe80::/10")) {
			return nil, errors.New("CANYON_KUBE_SCANDB_CIDRS must not include link-local addresses (169.254.0.0/16): that is the node's instance metadata")
		}
	}
	if m := ecrHost.FindStringSubmatch(e.Registry); m != nil {
		api, err := ecrClient(ctx, m[1])
		if err != nil {
			return nil, err
		}
		e.ECR, e.Auth = api, kube.NewECRAuth(api)
	}
	return e, nil
}

// request is an app container's request the setting names: a Kubernetes quantity above zero and at most
// the container's limit; "" when it is unset, the executor's default.
func request(getenv func(string) string, setting, limit string) (string, error) {
	v := getenv(setting)
	if v == "" {
		return "", nil
	}
	q, err := resource.ParseQuantity(v)
	if err != nil || q.Sign() <= 0 || q.Cmp(resource.MustParse(limit)) > 0 {
		return "", fmt.Errorf("%s is %q: a Kubernetes quantity above 0 and at most the app's limit, %s", setting, v, limit)
	}
	return q.String(), nil
}

// cidrs is the setting's comma-separated CIDRs, each without host bits; none when it is unset.
func cidrs(getenv func(string) string, setting string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for s := range strings.SplitSeq(getenv(setting), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil || p != p.Masked() {
			return nil, fmt.Errorf("%s: %q is not a CIDR", setting, s)
		}
		out = append(out, p)
	}
	return out, nil
}

// ecrClient is ECR's API in region with the pod's own credential — IRSA's web identity, which the SDK's
// default chain finds — each call bounded: one that never answers must not park a build or the registry
// upkeep. The chain never reaches instance metadata, whose credential is the node's role, not the
// executor's; and a credential is fetched now, so an executor without one refuses to start rather than
// failing at a push minutes later.
func ecrClient(ctx context.Context, region string) (*ecr.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region), config.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("the registry is ECR but the executor has no AWS credential: give its service account an IAM role (IRSA, the annotation eks.amazonaws.com/role-arn): %w", err)
	}
	return ecr.NewFromConfig(cfg), nil
}

// execOver runs command in a container of a pod with stdin over pods/exec: how a scan's feed reaches its
// pod. What the command writes to stderr, which it does when it fails, is the error's.
func execOver(cfg *rest.Config, client kubernetes.Interface) func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader) error {
	return func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader) error {
		req := client.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdin: true, Stderr: true}, scheme.ParameterCodec)
		exec, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, req.URL())
		if err != nil {
			return err
		}
		var stderr bytes.Buffer
		if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stderr: &stderr}); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

// start is what the executor does before it serves: it deletes what a crash left — probes, Builds and
// their sources, scans and refreshes — and syncs the Build informer /build waits on. Then, until ctx
// ends, it refreshes the databases scans have asked for and, when the registry has a credential, keeps
// canyon-registry: a registry without one (kind's registry:2) has none to write.
func start(ctx context.Context, e *kube.Executor) error {
	sweep, cancel := context.WithTimeout(ctx, 5*time.Minute)
	e.SweepProbes(sweep)
	e.SweepBuilds(sweep)
	e.SweepScans(sweep)
	cancel()
	if err := e.WatchBuilds(ctx); err != nil {
		return err
	}
	go e.RefreshDBs(ctx) // after the sweep, which deletes a refresh a crash cut short
	if e.Auth != nil {
		go e.KeepRegistrySecret(ctx)
	}
	return nil
}
