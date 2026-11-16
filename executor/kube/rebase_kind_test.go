//go:build kube

package kube

import (
	"context"
	"net/http"
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getcanyon-ai/canyon/executor"
)

// kindPush is where this host pushes an image the cluster reads under kind.registry.
func kindPush() string {
	if push := os.Getenv("CANYON_KUBE_TEST_PUSH"); push != "" {
		return push
	}
	return kind.registry
}

// kindImage pushes img to repo:tag and answers it by digest as the cluster sees it.
func kindImage(t *testing.T, repo, tag string, img v1.Image) string {
	t.Helper()
	ref, err := name.NewTag(kindPush() + "/" + repo + ":" + tag) // localhost and RFC 1918 hosts are spoken to over http
	if err == nil {
		err = remote.Write(ref, img)
	}
	sum, derr := img.Digest()
	if err != nil || derr != nil {
		t.Fatal(err, derr)
	}
	return kind.registry + "/" + repo + "@" + sum.String()
}

// kindRunImage is a run image of one random layer on this host's platform and the noble stack, pushed as
// kpack copies one (copyTag): answered as the ClusterBuilder names it, by tag and digest, and as run_image's
// form has it, by repository and digest.
func kindRunImage(t *testing.T) (img v1.Image, tagged, ref string) {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture, config.Config.Labels = "linux", goruntime.GOARCH, map[string]string{"io.buildpacks.stack.id": stack}
	if img, err = mutate.ConfigFile(img, config); err != nil {
		t.Fatal(err)
	}
	ref = kindImage(t, "canyon-builder", strings.TrimPrefix(copyTag, "/canyon-builder:"), img)
	return img, kind.registry + copyTag + "@" + strings.Split(ref, "@")[1], ref
}

// kindApp pushes an app image onto run, which tagged names, as kpack 0.18 exports one, under app's build
// tag: labels overrides its labels, "" dropping one.
func kindApp(t *testing.T, app string, run v1.Image, tagged string, labels map[string]string) string {
	t.Helper()
	return kindImage(t, "canyon/"+app, "pkg_kind0001-0a1b2c3d", appOn(t, run, tagged, labels))
}

// kindBuilds are the Builds canyon-builds holds.
func kindBuilds(t *testing.T, dc dynamic.Interface) []unstructured.Unstructured {
	t.Helper()
	list, err := dc.Resource(buildGVR).Namespace(BuildNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// TestKindRebaseIsKpacksBuildAndRefusesFirst is POST /rebase through the deployed executor, under its own
// Role (contracts/executor.md): every refusal answers before any Build is created, and the rebase is a
// Build kpack's webhook admits, of kpack's rebase shape — spec.lastBuild naming the image and its stack,
// the STACK reason, spec.runImage the planned run image, the image as its unread source, a fresh tag —
// its fresh tag in the registry while it is there, on a placeholder of the image's manifest (#2576), and the
// Build deleted once the rebase answers. kpack's controller is scaled to 0 here: the
// ClusterBuilder's status is written as it would write it — its copy of the run image by tag and digest,
// which GET /builder answers by repository and digest — and the rebase itself is the AWS test cluster's.
func TestKindRebaseIsKpacksBuildAndRefusesFirst(t *testing.T) {
	ctx := context.Background()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old, oldTagged, oldRef := kindRunImage(t)
	_, nowTagged, nowRef := kindRunImage(t)
	builder := kind.registry + "/canyon-builder@" + builtDigest
	builders := dc.Resource(clusterBuilderGVR)
	cb, err := builders.Get(ctx, "canyon", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	was := cb.Object["status"]
	_ = unstructured.SetNestedMap(cb.Object, map[string]any{"latestImage": builder, "stack": map[string]any{"runImage": nowTagged, "id": stack}}, "status")
	if _, err := builders.UpdateStatus(ctx, cb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cur, err := builders.Get(context.Background(), "canyon", metav1.GetOptions{}); err == nil {
			cur.Object["status"] = was
			_, _ = builders.UpdateStatus(context.Background(), cur, metav1.UpdateOptions{})
		}
	})
	if status, out := call(t, http.MethodGet, "/builder", nil); status != http.StatusOK || !strings.Contains(string(out), `"run_image":"`+nowRef+`"`) {
		t.Fatalf("GET /builder: %d %s", status, out)
	}

	mirror := "mirror.kind.test/canyon-builder:clusterbuilder-canyon-run-image@" + strings.Split(oldRef, "@")[1] // built while the ClusterBuilder's tag named a mirror
	request := func(app, image string) executor.RebaseRequest {
		return executor.RebaseRequest{App: app, Package: "pkg_kind0001", Image: image, RunImage: nowRef}
	}
	image := kindApp(t, "kind-rebase", old, oldTagged, nil)
	for _, tc := range []struct {
		name   string
		req    executor.RebaseRequest
		status int
		says   string
	}{
		{"a mirror under another name", request("kind-rebase-mirror", kindApp(t, "kind-rebase-mirror", old, oldTagged, map[string]string{executor.LifecycleLabel: lifecycleOn(t, old, mirror)})), 409, "another_run_image"},
		{"rebasable false", request("kind-rebase-false", kindApp(t, "kind-rebase-false", old, oldTagged, map[string]string{"io.buildpacks.rebasable": "false"})), 409, "not_rebasable"},
		{"another stack", request("kind-rebase-jammy", kindApp(t, "kind-rebase-jammy", old, oldTagged, map[string]string{"io.buildpacks.stack.id": "io.buildpacks.stacks.jammy"})), 409, "another_stack"},
		{"the run image moved", executor.RebaseRequest{App: "kind-rebase", Package: "pkg_kind0001", Image: image, RunImage: oldRef}, 409, "run_image_moved"},
		{"an image by tag", request("kind-rebase", kind.registry+"/canyon/kind-rebase:pkg_kind0001-0a1b2c3d"), 400, ""},
		{"an image not there", request("kind-rebase", kind.registry+"/canyon/kind-rebase@sha256:"+strings.Repeat("0", 64)), 404, ""},
	} {
		if status, out := call(t, http.MethodPost, "/rebase", tc.req); status != tc.status || !strings.Contains(string(out), tc.says) {
			t.Errorf("%s: %d %s, want %d saying %q", tc.name, status, out, tc.status, tc.says)
		}
		if builds := kindBuilds(t, dc); len(builds) != 0 {
			t.Fatalf("%s created Builds: %v", tc.name, builds)
		}
	}

	answered := make(chan []byte, 1)
	go func() {
		status, out := call(nil, http.MethodPost, "/rebase", request("kind-rebase", image)) // nil: no Fatal off the test's goroutine
		answered <- append([]byte(http.StatusText(status)+" "), out...)
	}()
	var b unstructured.Unstructured
	for start := time.Now(); ; time.Sleep(time.Second) {
		if builds := kindBuilds(t, dc); len(builds) > 0 {
			b = builds[0]
			break
		}
		if time.Since(start) > 2*time.Minute {
			t.Fatalf("no Build: %s", <-answered)
		}
	}
	tags, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "tags")
	fresh, ok := strings.CutPrefix(strings.Join(tags, ","), kind.registry+"/canyon/kind-rebase:pkg_kind0001-")
	for field, want := range map[string]any{ // kpack's webhook defaults lastBuild.cache: what the executor wrote is checked, field by field
		"lastBuild.image": image, "lastBuild.stackId": stack, "runImage.image": nowRef, "source.registry.image": image, "builder.image": builder, "serviceAccountName": "canyon-build",
	} {
		if got, _, _ := unstructured.NestedFieldNoCopy(b.Object, append([]string{"spec"}, strings.Split(field, ".")...)...); got != want {
			t.Errorf("the Build's spec.%s is %v, want %v", field, got, want)
		}
	}
	if !ok || len(tags) != 1 || b.GetName() != "kind-rebase-"+fresh || b.GetAnnotations()["image.kpack.io/reason"] != "STACK" ||
		b.GetLabels()["canyon.build"] != "true" || b.GetLabels()["canyon.app"] != "kind-rebase" {
		t.Errorf("the Build is not kpack's rebase to a fresh tag: %s %v %v %v", b.GetName(), tags, b.GetAnnotations(), b.GetLabels())
	}
	held, err := remote.Head(mustRef(t, kindPush()+"/canyon/kind-rebase:pkg_kind0001-"+fresh)) // kpack's rebase opens it before anything else
	if err != nil {
		t.Errorf("the Build's fresh tag is not in the registry while the Build is: kpack's rebase fails \"could not access previous image\" (#2576): %v", err)
	} else if image == kind.registry+"/canyon/kind-rebase@"+held.Digest.String() {
		t.Errorf("the fresh tag names the image itself, %s, not a placeholder only it names", image)
	}
	if err := dc.Resource(buildGVR).Namespace(BuildNamespace).Delete(ctx, b.GetName(), metav1.DeleteOptions{}); err != nil { // no controller will finish it
		t.Fatal(err)
	}
	select {
	case out := <-answered:
		if !strings.HasPrefix(string(out), "Bad Gateway ") || !strings.Contains(string(out), "deleted before it finished") {
			t.Errorf("a rebase whose Build was deleted: %s", out)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the rebase did not answer once its Build was deleted")
	}
	repo, _ := name.NewRepository(kindPush() + "/canyon/kind-rebase")
	want := []string{"pkg_kind0001-0a1b2c3d", "pkg_kind0001-" + fresh} // kind's registry:2 deletes nothing (no REGISTRY_STORAGE_DELETE_ENABLED): the executor keeps the fresh tag, logged
	slices.Sort(want)
	if left, err := remote.List(repo); err != nil || !slices.Equal(slices.Sorted(slices.Values(left)), want) {
		t.Errorf("the repository's tags after a failed rebase: %v %v, want its build's and the fresh tag a registry refusing deletes keeps", left, err)
	}
	if d, err := remote.Head(repo.Tag("pkg_kind0001-0a1b2c3d")); err != nil || image != kind.registry+"/canyon/kind-rebase@"+d.Digest.String() {
		t.Errorf("the image's own tag no longer names it: %v %v", d, err)
	}
	if builds := kindBuilds(t, dc); len(builds) != 0 {
		t.Errorf("the Build outlived the rebase: %v", builds)
	}
}
