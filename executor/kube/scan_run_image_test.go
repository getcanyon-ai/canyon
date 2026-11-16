package kube

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getcanyon-ai/canyon/executor"
)

// runImageInRegistry pushes a run image to a registry of e's own, as kpack's copy of the ClusterStack's run
// image, and gives e a ClusterBuilder canyon naming it by tag and digest: it answers GET /builder's
// run_image of it, <repository>@sha256:<hex>.
func runImageInRegistry(t *testing.T, e *Executor) string {
	t.Helper()
	e.Registry = strings.TrimPrefix(testRegistry(t).URL, "http://")
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	tagged := e.Registry + "/canyon-builder-run-image:canyon"
	if err := remote.Write(mustRef(t, tagged), img); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	namingRunImage(e, tagged+"@"+digest.String())
	return e.Registry + "/canyon-builder-run-image@" + digest.String()
}

// namingRunImage gives e a ClusterBuilder canyon whose status names runImage, by tag and digest, as kpack's.
func namingRunImage(e *Executor, runImage string) {
	cb := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "ClusterBuilder", "metadata": map[string]any{"name": "canyon"},
		"status": map[string]any{"latestImage": "registry.example/canyon/builder@" + builtDigest, "stack": map[string]any{"runImage": runImage}},
	}}
	e.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{clusterBuilderGVR: "ClusterBuilderList"}, cb)
}

// The run image is read by the same target with no app (contracts/executor.md, The run image): kpack's copy,
// exactly as GET /builder answers it, fed from the registry with the executor's credential and answered as
// an app's image is, its one diff ID and no lifecycle label.
func TestScanRunImageIsFedKpacksCopy(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	c.meta = javaDBBuilt(time.Hour)
	addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
	run := runImageInRegistry(t, e)
	res, err := e.Scan(t.Context(), executor.ScanRequest{Tool: "trivy", Target: "image", Image: run}, noParts())
	if err != nil || string(res.SBOM) != cdxOut || res.Lifecycle != "" || res.NotFinished != "" || len(res.DiffIDs) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(jobsNamed(c, "canyon-scan-trivy-java-")) != 1 || c.fed[0]["image.tar"] == "" {
		t.Errorf("the run image was not fed to one image scan")
	}
}

// Any image but the run image GET /builder answers now — an older copy, or an app's image named without its
// app — is run_image_moved, before any Job.
func TestScanRunImageRefusesAnotherImage(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	c.meta = javaDBBuilt(time.Hour)
	addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
	run := runImageInRegistry(t, e)
	repo, _, _ := strings.Cut(run, "@")
	for _, image := range []string{repo + "@sha256:" + strings.Repeat("4d", 32), e.Registry + "/canyon/claims-portal@sha256:" + strings.Repeat("ab", 32)} {
		_, err := e.Scan(t.Context(), executor.ScanRequest{Tool: "trivy", Target: "image", Image: image}, noParts())
		var refused *executor.Refused
		if !errors.As(err, &refused) || refused.Reason != "run_image_moved" || !strings.Contains(refused.Message, "the run image is now "+run) {
			t.Errorf("%s: %v", image, err)
		}
	}
	if len(jobsNamed(c, "canyon-scan-")) != 0 {
		t.Errorf("a Job ran for another image")
	}
}

// A run image the ClusterBuilder names outside CANYON_KUBE_REGISTRY is never read: the executor's registry
// credential goes to its own registry alone, so the scan fails before any request reaches the other host.
func TestScanRunImageOutsideTheRegistryIsNeverRead(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	c.meta = javaDBBuilt(time.Hour)
	addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
	e.Registry = strings.TrimPrefix(testRegistry(t).URL, "http://")
	var asked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1); w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(other.Close)
	sum := "sha256:" + strings.Repeat("5e", 32)
	host := strings.TrimPrefix(other.URL, "http://")
	namingRunImage(e, host+"/canyon-builder-run-image:canyon@"+sum)
	_, err := e.Scan(t.Context(), executor.ScanRequest{Tool: "trivy", Target: "image", Image: host + "/canyon-builder-run-image@" + sum}, noParts())
	if err == nil || !strings.Contains(err.Error(), "CANYON_KUBE_REGISTRY") || asked.Load() != 0 || len(jobsNamed(c, "canyon-scan-")) != 0 {
		t.Fatalf("a run image in another registry: %v, %d requests there, %d Jobs", err, asked.Load(), len(jobsNamed(c, "canyon-scan-")))
	}
}
