//go:build kube

package kube

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"net/http"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// archApp is a small Go app that answers on $PORT and says, once, the architecture it runs on.
var archApp = map[string]string{
	"go.mod": "module archcheck\n\ngo 1.23\n",
	"main.go": `package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
)

func main() {
	fmt.Println("archcheck runs on " + runtime.GOARCH)
	_ = http.ListenAndServe(":"+os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, runtime.GOARCH) }))
}
`,
}

// A real build and /run through the executor on kind, on whatever architecture the runner is (#2440): kpack's
// ClusterBuilder made for this cluster's nodes, the Build pinned to them, its image made for them, and the app's
// pod pinned to them and running, saying the architecture it runs on. build-arch.yml runs it on arm64 and amd64,
// once .github/kind/build-arch.sh has named Canyon's stack images and started kpack's controller; kube.yml's
// suite, whose kpack makes no builder, never runs it (its name has no TestKind in it).
func TestBuildOnKindRunsWhereItsAppRuns(t *testing.T) {
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	for _, f := range []string{"go.mod", "main.go"} {
		if err := tw.WriteHeader(&tar.Header{Name: f, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(archApp[f]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(archApp[f])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := (&executor.Client{Base: kind.executor, HTTP: &http.Client{Transport: toExecutor}}).Build(ctx, executor.BuildRequest{App: "kind-arch", Package: "pkg_arch1"}, &tree, func(_ context.Context, line string) error {
		t.Log(line)
		return nil
	})
	if err != nil {
		t.Fatalf("/build: %v", err)
	}
	t.Logf("/build made %s", res.Image)
	status, out := call(t, http.MethodPost, "/run", map[string]any{
		"app": "kind-arch", "target": "preview", "image": res.Image, "env": map[string]string{}, "network": "canyon-app-kind-arch",
		"identity": map[string]string{"issuer": "https://canyon.kind.test", "audience": "kind-arch", "jwks_url": jwksURL},
		"access":   accessFile("2026-10-27T10:00:00Z"),
		"front_door": map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
			"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"},
	})
	t.Cleanup(func() {
		_, _ = call(nil, http.MethodPost, "/stop", map[string]any{"app": "kind-arch", "target": "preview"})
	})
	if status != http.StatusOK {
		t.Fatalf("/run of what /build made: %d %s", status, out)
	}
	pods, err := kind.client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.app=kind-arch"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods of kind-arch: %v %d", err, len(pods.Items))
	}
	pod := pods.Items[0]
	if sel := pod.Spec.NodeSelector; sel["kubernetes.io/arch"] != goruntime.GOARCH {
		t.Errorf("the app's pod is pinned to %v, want kubernetes.io/arch: %s", sel, goruntime.GOARCH)
	}
	said, err := kind.client.CoreV1().Pods(Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "app"}).DoRaw(ctx)
	if err != nil || !strings.Contains(string(said), "archcheck runs on "+goruntime.GOARCH) {
		t.Errorf("the app said %q (%v), want it runs on %s", said, err, goruntime.GOARCH)
	}
}

// An image of the other architecture than this cluster's nodes is pinned to nodes of its own, never to the
// executor's (#2440): its pod waits unscheduled for a node of that architecture, where on a cluster of both it
// would run, the /run fails, and as after any failed first /run nothing of it is left. build-arch.yml runs it on
// arm64 and on amd64.
func TestAnImageOfTheOtherArchitectureWaitsForItsOwnNodes(t *testing.T) {
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[goruntime.GOARCH]
	img, err := remote.Image(name.MustParseReference(probeImage), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: other}))
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(cmp.Or(os.Getenv("CANYON_KUBE_TEST_PUSH"), kind.registry) + "/canyon/kind-other-arch:kind")
	if err == nil {
		err = remote.Write(tag, img, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	sum, derr := img.Digest()
	if err = errors.Join(err, derr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	waiting := make(chan corev1.Pod, 1)
	go func() {
		for ctx.Err() == nil {
			pods, err := kind.client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.app=kind-other-arch"})
			for _, p := range pods.Items {
				for _, c := range p.Status.Conditions {
					if err == nil && c.Type == corev1.PodScheduled && c.Reason == corev1.PodReasonUnschedulable {
						waiting <- p
						return
					}
				}
			}
			time.Sleep(time.Second)
		}
	}()
	status, out := call(t, http.MethodPost, "/run", map[string]any{
		"app": "kind-other-arch", "target": "preview", "image": kind.registry + "/canyon/kind-other-arch@" + sum.String(), "env": map[string]string{},
		"network": "canyon-app-kind-other-arch", "identity": map[string]string{"issuer": "https://canyon.kind.test", "audience": "kind-other-arch", "jwks_url": jwksURL},
		"access": accessFile("2026-10-27T10:00:00Z"),
		"front_door": map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
			"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"},
	})
	t.Cleanup(func() {
		_, _ = call(nil, http.MethodPost, "/stop", map[string]any{"app": "kind-other-arch", "target": "preview"})
	})
	if status == http.StatusOK {
		t.Fatalf("a /run of a %s image on %s nodes answered 200: %s", other, goruntime.GOARCH, out)
	}
	t.Logf("the /run answered %d %s", status, out)
	select {
	case p := <-waiting:
		if sel := p.Spec.NodeSelector; len(sel) != 1 || sel["kubernetes.io/arch"] != other {
			t.Errorf("the waiting pod is pinned to %v, want kubernetes.io/arch: %s, its image's", sel, other)
		}
	default:
		t.Error("no pod of the /run was seen waiting for a node of its image's architecture")
	}
	if _, err := kind.client.AppsV1().Deployments(Namespace).Get(ctx, "kind-other-arch-preview", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after the failed first /run its Deployment: %v, want it gone", err)
	}
}
