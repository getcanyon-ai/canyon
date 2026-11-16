package kube

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// buildOf runs a /build of tree on e: its error, nil when it built.
func buildOf(t *testing.T, e *Executor, tree []byte) error {
	t.Helper()
	_, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree), io.Discard)
	return err
}

// The Kubernetes executor's own /build faults are Canyon's, never the package's (#2537): each is HostFailed,
// which the handler streams with own: true, so the run records error.
func TestTheClustersOwnBuildFaultsAreCanyons(t *testing.T) {
	for name, c := range map[string]struct {
		fault func(t *testing.T, e *Executor, dc *dynamicfake.FakeDynamicClient)
		says  string
	}{
		"not watching Builds": {func(_ *testing.T, e *Executor, _ *dynamicfake.FakeDynamicClient) { e.watch = nil }, "not watching Builds"},
		"the ClusterBuilder with no image yet": {func(t *testing.T, _ *Executor, dc *dynamicfake.FakeDynamicClient) {
			cb, err := dc.Resource(clusterBuilderGVR).Get(t.Context(), "canyon", metav1.GetOptions{})
			if err == nil {
				err = unstructured.SetNestedField(cb.Object, "", "status", "latestImage")
			}
			if err == nil {
				_, err = dc.Resource(clusterBuilderGVR).Update(t.Context(), cb, metav1.UpdateOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
		}, "has no image yet"},
		"the registry refusing the source image": {func(_ *testing.T, e *Executor, _ *dynamicfake.FakeDynamicClient) {
			gone := httptest.NewServer(nil)
			gone.Close()
			e.Registry = strings.TrimPrefix(gone.URL, "http://")
		}, "source image"},
		"the Build not created": {func(_ *testing.T, _ *Executor, dc *dynamicfake.FakeDynamicClient) {
			dc.PrependReactor("create", "builds", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New(`admission webhook "validation.webhook.kpack.io" denied the request`)
			})
		}, "denied the request"},
	} {
		t.Run(name, func(t *testing.T) {
			e, dc, _ := kpackCluster(t, "True", "")
			c.fault(t, e, dc)
			err := buildOf(t, e, tree(t))
			var host *executor.HostFailed
			if !errors.As(err, &host) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v, want Canyon's own failure saying %q", err, c.says)
			}
		})
	}
}

// A Build whose create the cluster's API refuses for its build values, too large or a name it won't take, is the
// user's to change, never Canyon's (#2550): no HostFailed, and the line says the build values were refused.
func TestABuildRefusedForItsValuesIsNotCanyons(t *testing.T) {
	for name, refusal := range map[string]error{
		"too large": apierrors.NewRequestEntityTooLargeError("limit is 3145728"),
		"invalid": apierrors.NewInvalid(schema.GroupKind{Group: "kpack.io", Kind: "Build"}, "claims-portal-x",
			field.ErrorList{field.Invalid(field.NewPath("spec", "env").Index(0).Child("name"), "CNB_X", "not allowed")}),
	} {
		t.Run(name, func(t *testing.T) {
			e, dc, _ := kpackCluster(t, "True", "")
			dc.PrependReactor("create", "builds", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, refusal })
			err := buildOf(t, e, tree(t))
			var host *executor.HostFailed
			if err == nil || errors.As(err, &host) || !strings.HasPrefix(err.Error(), "the cluster refused the build values: ") ||
				!strings.Contains(err.Error(), refusal.Error()) {
				t.Fatalf("%v (read as the host's: %t), want the build values refused, naming %q", err, errors.As(err, &host), refusal)
			}
		})
	}
}

// A Build deleted before it ended is the cluster's, never the package's (#2537): kpack never says it failed.
func TestABuildDeletedBeforeItEndedIsCanyons(t *testing.T) {
	e, dc, _ := kpackCluster(t, "Unknown", "")
	ctx := t.Context()
	go func() { // the Build, once created, deleted as someone would: read through the fake's API, never its reactor's slice
		for ctx.Err() == nil {
			if list, err := dc.Resource(buildGVR).Namespace(BuildNamespace).List(ctx, metav1.ListOptions{}); err == nil && len(list.Items) > 0 &&
				dc.Tracker().Delete(buildGVR, BuildNamespace, list.Items[0].GetName()) == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	err := buildOf(t, e, tree(t))
	var host *executor.HostFailed
	if !errors.As(err, &host) || !strings.Contains(err.Error(), "deleted before it finished") {
		t.Fatalf("%v, want Canyon's own failure", err)
	}
}

// The package's own /build failures stay the package's on Kubernetes too (#2537): its build step exiting, kpack's
// own failure with no step to name, and a tree the check refuses are never HostFailed.
func TestThePackagesOwnBuildFailuresStayItsOnKubernetes(t *testing.T) {
	for name, c := range map[string]struct {
		succeeded, message string
		withPod            bool
		tree               []byte
		says               string
	}{
		"its build step exits":     {"False", "", true, nil, "the build step exited 1"},
		"kpack failing, no step":   {"False", "pod evicted", false, nil, "pod evicted"},
		"a tree escaping its root": {"True", "", true, tarOf(t, &tar.Header{Name: "../x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}), "escapes the tree"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := kpackClusterPod(t, c.succeeded, c.message, c.withPod)
			src := c.tree
			if src == nil {
				src = tree(t)
			}
			err := buildOf(t, e, src)
			var host *executor.HostFailed
			if err == nil || errors.As(err, &host) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v (read as the host's: %t), want the package's own failure saying %q", err, errors.As(err, &host), c.says)
			}
		})
	}
}
