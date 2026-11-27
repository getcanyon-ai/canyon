package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

const builtDigest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

var steps = []string{"prepare", "detect", "build", "export"}

// testRegistry is go-containerregistry's in-memory registry, deleting a manifest's tags with it as
// registry:2 and ECR do: it keeps a manifest under each tag as well as its digest.
func testRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	inner := registry.New()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, ref, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if r.Method == http.MethodDelete && ok && strings.HasPrefix(ref, "sha256:") {
			base := strings.TrimPrefix(srv.URL, "http://") + "/" + repo
			if tags, err := remote.List(mustRepo(t, base)); err == nil {
				for _, tag := range tags {
					if d, err := remote.Head(mustRef(t, base+":"+tag)); err == nil && d.Digest.String() == ref {
						req := httptest.NewRequest(http.MethodDelete, "/v2/"+repo+"/manifests/"+tag, nil)
						inner.ServeHTTP(httptest.NewRecorder(), req)
					}
				}
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustRepo(t *testing.T, s string) name.Repository {
	r, err := name.NewRepository(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustRef(t *testing.T, s string) name.Reference {
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// kpackCluster is a registry and a fake API that plays kpack: every Build created gets a pod whose
// steps ran, then the condition succeeded says, with latestImage the Build's first tag by digest.
func kpackCluster(t *testing.T, succeeded, message string) (*Executor, *dynamicfake.FakeDynamicClient, *[]*unstructured.Unstructured) {
	t.Helper()
	return kpackClusterPod(t, succeeded, message, true)
}

// kpackClusterPod is kpackCluster whose build pod, when withPod is false, is gone: evicted, or collected.
func kpackClusterPod(t *testing.T, succeeded, message string, withPod bool) (*Executor, *dynamicfake.FakeDynamicClient, *[]*unstructured.Unstructured) {
	t.Helper()
	reg := testRegistry(t)
	builder := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "ClusterBuilder", "metadata": map[string]any{"name": "canyon"},
		"status": map[string]any{"latestImage": "registry.example/canyon/builder@" + builtDigest},
	}}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{buildGVR: "BuildList", clusterBuilderGVR: "ClusterBuilderList"}, builder)
	pods := fake.NewClientset()
	var created []*unstructured.Unstructured
	dc.PrependReactor("create", "builds", func(a k8stesting.Action) (bool, runtime.Object, error) {
		b := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		tags, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "tags")
		if len(tags) == 0 { // not the executor's: kpack would refuse it
			return false, nil, nil
		}
		created = append(created, b)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.GetName() + "-build-pod", Namespace: BuildNamespace}}
		for _, s := range steps {
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: s})
			done := &corev1.ContainerStateTerminated{}
			if s == "build" && succeeded == "False" { // the app's own build exited 1, as a compile error does
				done.ExitCode = 1
			}
			pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{Name: s, State: corev1.ContainerState{Terminated: done}})
		}
		pod.Status.Phase = corev1.PodSucceeded
		if withPod {
			if err := pods.Tracker().Add(pod); err != nil {
				t.Error(err)
			}
		}
		go func() { // kpack's status, after the create has landed
			repo := tags[0][:strings.LastIndex(tags[0], ":")]
			done := b.DeepCopy()
			_ = unstructured.SetNestedField(done.Object, pod.Name, "status", "podName")
			_ = unstructured.SetNestedField(done.Object, repo+"@"+builtDigest, "status", "latestImage")
			_ = unstructured.SetNestedSlice(done.Object, []any{map[string]any{"id": "paketo-buildpacks/node-engine", "version": "4.1.0"}, map[string]any{"id": "paketo-buildpacks/npm-install", "version": "1.5.0"}}, "status", "buildMetadata")
			_ = unstructured.SetNestedSlice(done.Object, []any{map[string]any{"type": "Succeeded", "status": succeeded, "message": message}}, "status", "conditions")
			for range 100 {
				if err := dc.Tracker().Update(buildGVR, done, BuildNamespace); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return false, nil, nil
	})
	pods.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		return true, &runtime.Unknown{Raw: []byte("===> " + o.Container + "\n")}, nil
	})
	e := &Executor{Client: pods, Dynamic: dc, Registry: strings.TrimPrefix(reg.URL, "http://"), Poll: time.Millisecond}
	if err := e.WatchBuilds(t.Context()); err != nil {
		t.Fatal(err)
	}
	// the informer is synced once its list lands and opens its watch after: a test's calls start once it has (#2364)
	for !slices.ContainsFunc(dc.Actions(), func(a k8stesting.Action) bool { return a.GetVerb() == "watch" }) {
		time.Sleep(time.Millisecond)
	}
	return e, dc, &created
}

func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// files is a tar of each name's body, in name order.
func files(t *testing.T, bodies map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, name := range slices.Sorted(maps.Keys(bodies)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(bodies[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, bodies[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tree(t *testing.T) []byte {
	return tarOf(t, &tar.Header{Name: "src/", Typeflag: tar.TypeDir, Mode: 0o755}, &tar.Header{Name: "src/package.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
}

// Invariant 1 on Kubernetes: build values are the Build's literal spec.env and in no other object, and
// no Build of the app is left once /build answers — image or error — nor its source image.
func TestBuildValuesLiveOnlyAsLongAsTheBuild(t *testing.T) {
	// kpack's own message for a failed build ends on a kubectl hint for operators: the answer names the
	// lifecycle step that failed and its exit code instead (the test cluster's case-build-fails).
	kpackSays := "Error:  Container build terminated with error ERROR: failed to build: exit status 1: For more info use `kubectl logs -n canyon-builds b-build-pod -c build`"
	for _, tc := range []struct{ succeeded, message string }{{"True", ""}, {"False", kpackSays}} {
		e, dc, created := kpackCluster(t, tc.succeeded, tc.message)
		var out bytes.Buffer
		res, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{"VITE_API_URL": buildValue, "B": ""}}, bytes.NewReader(tree(t)), &out)
		repo := e.Registry + "/canyon/claims-portal"
		if tc.succeeded == "True" && (err != nil || res.Image != repo+"@"+builtDigest) {
			t.Fatalf("a build that succeeded: %+v %v", res, err)
		}
		if tc.succeeded == "False" && (err == nil || err.Error() != "the build step exited 1") {
			t.Fatalf("a build that failed: %+v %v", res, err)
		}
		if out.String() != "===> prepare\n===> detect\n===> build\n===> export\n" {
			t.Fatalf("the answer's lines %q: every step, in step order", out.String())
		}
		b := (*created)[0].Object["spec"].(map[string]any)
		env, _ := b["env"].([]any)
		src := b["source"].(map[string]any)["registry"].(map[string]any)["image"].(string)
		if len(env) != 4 || env[0].(map[string]any)["name"] != "B" || env[3].(map[string]any)["value"] != buildValue || b["serviceAccountName"] != "canyon-build" ||
			b["cache"].(map[string]any)["registry"].(map[string]any)["tag"] != repo+":cache" || !strings.HasPrefix(src, repo+"@sha256:") ||
			b["builder"].(map[string]any)["image"] != "registry.example/canyon/builder@"+builtDigest || !strings.HasPrefix(b["tags"].([]any)[0].(string), repo+":pkg_7d3e8f-") {
			t.Fatalf("the Build %v", b)
		}
		valueOnlyInSpecEnv(t, e, dc, src)
		if left, _ := dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(left.Items) != 0 {
			t.Fatalf("%s: a Build left behind after /build answered: %s", tc.succeeded, left.Items[0].GetName())
		}
		ref, err := name.NewDigest(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := remote.Get(ref); err == nil {
			t.Fatalf("%s: the source image %s is still in the registry", tc.succeeded, src)
		}
	}
}

// A build answers the buildpacks its Build ran, status.buildMetadata in order, by id alone: the
// ClusterBuilder's image names none (contracts/executor.md, The buildpacks that ran).
func TestABuildNamesTheBuildpacksItsBuildRan(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	res, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree(t)), io.Discard)
	want := []executor.Buildpack{{ID: "paketo-buildpacks/node-engine"}, {ID: "paketo-buildpacks/npm-install"}}
	if err != nil || !reflect.DeepEqual(res.Buildpacks, want) {
		t.Fatalf("the buildpacks that ran: %+v %v, want %+v", res.Buildpacks, err, want)
	}
}

// Every build runs with the executor's own build variables, NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/ and
// NODE_ENV=production, literal spec.env entries, which a build value of either name replaces
// (contracts/executor.md, /build; canyonv2-g6il, #1240).
func TestEveryBuildRunsWithTheExecutorsOwnVariables(t *testing.T) {
	for want, values := range map[[2]string]map[string]string{{"/", "production"}: nil, {"/workspace", "test"}: {"NODE_ENV": "test", "NEXT_PRIVATE_OUTPUT_TRACE_ROOT": "/workspace"}} {
		e, _, created := kpackCluster(t, "True", "")
		if _, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: values}, bytes.NewReader(tree(t)), io.Discard); err != nil {
			t.Fatal(err)
		}
		env, _ := (*created)[0].Object["spec"].(map[string]any)["env"].([]any)
		got := []any{map[string]any{"name": "NEXT_PRIVATE_OUTPUT_TRACE_ROOT", "value": want[0]}, map[string]any{"name": "NODE_ENV", "value": want[1]}}
		if !reflect.DeepEqual(env, got) {
			t.Fatalf("build values %v: the Build's spec.env %v, want %v", values, env, got)
		}
	}
}

// What a source carried is kept at a cost bounded per entry, whatever its name: a package is hostile, and
// archive/tar reads a name of up to 1 MiB in a PAX header, so a name deep in directories is one entry kept, never
// one per directory above it (#242's review). 8000 levels: 16 KB of name, which one key per ancestor turned into
// 8001 keys and as many scans of the name; at archive/tar's 1 MiB, half a million of each.
func TestWhatASourceCarriedStaysBoundedForADeepLongName(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	deep := strings.Repeat("a/", 8000) + "nginx.conf"
	src := tarOf(t, &tar.Header{Name: deep, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}) // the writer puts the name in a PAX header
	var before, after goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&before)
	_, saw, err := e.pushSource(t.Context(), "claims-portal", "abcd1234", bytes.NewReader(src))
	goruntime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; len(saw.paths) != 1 || alloc > 8<<20 {
		t.Fatalf("one entry %d bytes deep: %d paths kept, %d bytes allocated, want 1 and at most 8 MiB", len(deep), len(saw.paths), alloc)
	}
}

// The source image is the tree, one layer, as the Docker executor would untar it: a link, a device
// or an escape is refused, and nothing is pushed for it.
func TestTheSourceImageIsTheCheckedTree(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	src, _, err := e.pushSource(t.Context(), "claims-portal", "abcd1234", bytes.NewReader(tree(t)))
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(src)
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := img.Layers()
	if len(layers) != 1 {
		t.Fatalf("%d layers", len(layers))
	}
	rc, _ := layers[0].Uncompressed()
	tr := tar.NewReader(rc)
	var names []string
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		names = append(names, h.Name)
	}
	if !slices.Equal(names, []string{"src/", "src/package.json"}) {
		t.Fatalf("the layer holds %v", names)
	}
	for label, h := range map[string]*tar.Header{
		"a symlink":     {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"a hard link":   {Name: "hard", Typeflag: tar.TypeLink, Linkname: "src/package.json"},
		"an escape":     {Name: "../outside", Typeflag: tar.TypeReg, Size: 1},
		"a device":      {Name: "dev", Typeflag: tar.TypeChar},
		"absolute path": {Name: "/etc/cron.d/x", Typeflag: tar.TypeReg, Size: 1},
	} {
		refusedAndNotPushed(t, e, label, tarOf(t, h))
	}
	f, err := os.Open("../testdata/sparse.tar") // a 19 KiB pax archive whose one entry expands to 64 MiB
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sparse, _ := io.ReadAll(f)
	refusedAndNotPushed(t, e, "a sparse entry", sparse)
	entries, size := maxEntries, maxTree
	t.Cleanup(func() { maxEntries, maxTree = entries, size })
	maxEntries = 2
	refusedAndNotPushed(t, e, "more entries than maxEntries", tarOf(t, &tar.Header{Name: "a", Typeflag: tar.TypeDir}, &tar.Header{Name: "b", Typeflag: tar.TypeDir}, &tar.Header{Name: "c", Typeflag: tar.TypeDir}))
	maxEntries, maxTree = entries, 10
	refusedAndNotPushed(t, e, "more bytes than maxTree", tarOf(t, &tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: 11}))
}

// refusedAndNotPushed pushes a tree the check refuses, under a nonce of its own: an error, and no
// src- tag in the registry.
func refusedAndNotPushed(t *testing.T, e *Executor, label string, tree []byte) {
	t.Helper()
	run := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(label)))
	if _, _, err := e.pushSource(t.Context(), "claims-portal", run, bytes.NewReader(tree)); err == nil {
		t.Errorf("%s was pushed", label)
	}
	tag, err := name.NewTag(e.Registry + "/canyon/claims-portal:src-" + run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(tag); err == nil {
		t.Errorf("%s: the src- tag is in the registry", label)
	}
}

// valueOnlyInSpecEnv searches every object written through the API and the source image for the build
// value: it is in the Build's spec.env and nowhere else.
func valueOnlyInSpecEnv(t *testing.T, e *Executor, dc *dynamicfake.FakeDynamicClient, src string) {
	t.Helper()
	for _, a := range append(dc.Actions(), e.Client.(*fake.Clientset).Actions()...) {
		w, ok := a.(k8stesting.CreateAction) // an update is a create action too
		if !ok {
			continue
		}
		obj := w.GetObject()
		if u, ok := obj.(*unstructured.Unstructured); ok && a.GetResource() == buildGVR && a.GetVerb() == "create" {
			u = u.DeepCopy()
			unstructured.RemoveNestedField(u.Object, "spec", "env")
			obj = u
		}
		if raw, _ := json.Marshal(obj); strings.Contains(string(raw), buildValue) {
			t.Errorf("%s %s carries the build value outside the Build's spec.env: %s", a.GetVerb(), a.GetResource().Resource, raw)
		}
	}
	ref, err := name.NewDigest(src)
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		return // gone already: nothing of it to search
	}
	manifest, _ := img.RawManifest()
	config, _ := img.RawConfigFile()
	layers, _ := img.Layers()
	rc, _ := layers[0].Uncompressed()
	content, _ := io.ReadAll(rc)
	if bytes.Contains(append(append(manifest, config...), content...), []byte(buildValue)) {
		t.Errorf("the source image carries the build value")
	}
}

// buildValue is a build value: once searched for, it must be found in the Build's spec.env alone.
const buildValue = "build-value-only-in-spec-env-7c1f"

// A Build whose pod is gone — evicted, or collected — answers once kpack says it failed, and is
// deleted then, not when the build's ceiling cuts the call.
func TestABuildWhosePodIsGoneAnswersAtOnce(t *testing.T) {
	e, dc, _ := kpackClusterPod(t, "False", "pod deleted", false)
	err := answered(t, func() error {
		_, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree(t)), io.Discard)
		return err
	})
	if undetected := (*executor.Undetected)(nil); errors.As(err, &undetected) || err == nil || !strings.Contains(err.Error(), "pod deleted") {
		t.Fatalf("a pod that is gone cannot say its detect failed: the Build's own failure answers, got %v", err)
	}
	if left, _ := dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(left.Items) != 0 {
		t.Fatal("the Build outlived its answer")
	}
}

// A failed detect on Kubernetes is the build pod's detect step exiting 20 or 21: the answer names what
// the ClusterBuilder builds, from its status.order, each buildpack by the metadata label of its image,
// read from the executor's registry; never a failed build (contracts/executor.md, /build; #1242). An image
// that cannot be read, elsewhere or without the label, leaves each buildpack its id. Any other step
// failing, or detect failing otherwise, is the build's failure as before. A detect exiting 21 on a tree
// that pins nginx with BP_NGINX_VERSION and has no buildpack.yml is the contract's line instead, read from
// the tree as it streamed (But a detect the nginx buildpack errored on; #242).
func TestAFailedDetectNamesWhatTheClusterBuilderBuilds(t *testing.T) {
	named := []string{"Paketo Buildpack for Ruby", "Paketo Buildpack for Node.js"}
	ids := []string{"paketo-buildpacks/ruby", "paketo-buildpacks/nodejs"}
	meta := `{"buildpacks":[{"id":"paketo-buildpacks/nodejs","name":"Paketo Buildpack for Node.js","version":"10.13.0"},{"id":"paketo-buildpacks/ruby","name":"Paketo Buildpack for Ruby","version":"3.1.1"}]}`
	project := "[_]\nschema-version = \"0.2\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_WEB_SERVER\"\nvalue = \"nginx\"\n\n" +
		"[[io.buildpacks.build.env]]\nname = \"BP_NGINX_VERSION\"\nvalue = \"1.30.*\"\n"
	pinned := files(t, map[string]string{"./project.toml": project, "public/index.html": "<p>hi</p>\n"})
	withYml := files(t, map[string]string{"./project.toml": project, "public/index.html": "<p>hi</p>\n", "buildpack.yml": "nginx:\n  version: 1.30.*\n"})
	for _, c := range []struct {
		step    string
		code    int32
		image   string   // the builder image's metadata label in the executor's registry; "elsewhere": registry.example's, unread; "-": none
		unread  bool     // status.order is not an order: the failed detect names none
		builds  []string // nil: not a failed detect
		errored bool
		tree    []byte // nil: tree(t)
	}{
		{"detect", 21, meta, false, named, true, nil},
		{"detect", 20, meta, false, named, false, nil},
		{"detect", 20, "elsewhere", false, ids, false, nil},
		{"detect", 20, "-", false, ids, false, nil},
		{"detect", 20, meta, true, []string{}, false, nil},
		{"detect", 1, meta, false, nil, false, nil},
		{"build", 51, meta, false, nil, false, nil},
		{"detect", 21, meta, false, nil, false, pinned},
		{"detect", 20, meta, false, named, false, pinned},
		{"detect", 21, meta, false, named, true, withYml},
	} {
		e, dc, _ := kpackCluster(t, "False", "Build failed")
		cb, err := dc.Resource(clusterBuilderGVR).Get(t.Context(), "canyon", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		order := []any{
			map[string]any{"group": []any{map[string]any{"id": "paketo-buildpacks/ruby", "version": "3.1.1"}}},
			map[string]any{"group": []any{map[string]any{"id": "paketo-buildpacks/nodejs", "version": "10.13.0"}, map[string]any{"id": "paketo-buildpacks/procfile", "version": "5.15.2", "optional": true}}},
		}
		if err := unstructured.SetNestedSlice(cb.Object, order, "status", "order"); err != nil {
			t.Fatal(err)
		}
		if c.unread {
			_ = unstructured.SetNestedField(cb.Object, "not an order", "status", "order")
		}
		if c.image != "elsewhere" {
			_ = unstructured.SetNestedField(cb.Object, builderImage(t, e, strings.TrimPrefix(c.image, "-")), "status", "latestImage")
		}
		if err := dc.Tracker().Update(clusterBuilderGVR, cb, ""); err != nil {
			t.Fatal(err)
		}
		pods := e.Client.(*fake.Clientset)
		pods.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "" { // the step's log: the cluster's own reactor answers it
				return false, nil, nil
			}
			obj, err := pods.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), BuildNamespace, a.(k8stesting.GetAction).GetName())
			if err != nil {
				return true, nil, err
			}
			p := obj.(*corev1.Pod).DeepCopy()
			for i := range p.Status.InitContainerStatuses {
				if s := &p.Status.InitContainerStatuses[i]; s.Name == c.step {
					s.State.Terminated.ExitCode = c.code
				}
			}
			p.Status.Phase = corev1.PodFailed
			return true, p, nil
		})
		src := c.tree
		if src == nil {
			src = tree(t)
		}
		_, err = e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(src), io.Discard)
		var undetected *executor.Undetected
		switch {
		case c.builds == nil && c.tree != nil && (err == nil || err.Error() != executor.NginxWithoutYml):
			t.Fatalf("detect exiting %d on a tree pinning nginx without buildpack.yml: %#v, want %q", c.code, err, executor.NginxWithoutYml)
		case c.builds == nil && c.tree == nil && (err == nil || errors.As(err, &undetected) || err.Error() != fmt.Sprintf("the %s step exited %d", c.step, c.code)):
			t.Fatalf("%s exiting %d is the build's failure: %v", c.step, c.code, err)
		case c.builds != nil && (!errors.As(err, &undetected) || undetected.Builds == nil || !slices.Equal(undetected.Builds, c.builds) || undetected.Errored != c.errored):
			t.Fatalf("%s exiting %d, builder image %.20q: %#v, want a failed detect naming %q, errored %v", c.step, c.code, c.image, err, c.builds, c.errored)
		}
	}
}

// builderImage pushes a builder image to e's registry, its metadata label meta ("": none), and answers it
// by digest, as kpack's ClusterBuilder status names it.
func builderImage(t *testing.T, e *Executor, meta string) string {
	t.Helper()
	labels := map[string]string{}
	if meta != "" {
		labels[executor.MetadataLabel] = meta
	}
	img, err := mutate.Config(empty.Image, v1.Config{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(mustRef(t, e.Registry+"/canyon-builder:latest"), img); err != nil {
		t.Fatal(err)
	}
	sum, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return e.Registry + "/canyon-builder@" + sum.String()
}

// A Build deleted before it finished answers at once, not when the build's ceiling cuts the call.
func TestABuildDeletedBeforeItFinishedAnswersAtOnce(t *testing.T) {
	e, dc, _ := kpackCluster(t, "Unknown", "")
	go func() { // whoever deletes it: an operator, a namespace going
		builds := dc.Resource(buildGVR).Namespace(BuildNamespace)
		for range 500 {
			if list, err := builds.List(context.Background(), metav1.ListOptions{}); err == nil && len(list.Items) > 0 {
				_ = builds.Delete(context.Background(), list.Items[0].GetName(), metav1.DeleteOptions{})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	err := answered(t, func() error {
		_, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree(t)), io.Discard)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "deleted before it finished") {
		t.Fatalf("%v", err)
	}
}

// answered runs call and fails the test unless it returns an error within 5 s.
func answered(t *testing.T, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("answered without an error")
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("no answer within 5 s")
		return nil
	}
}

// The registry's credential goes to CANYON_KUBE_REGISTRY's canyon/<app> and nowhere else: a dotless
// registry does not default to Docker Hub, and a swept Build's source on another host is not deleted.
func TestImagesAreOfThisRegistryAlone(t *testing.T) {
	var asked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer other.Close()
	e, dc, _ := kpackCluster(t, "True", "")
	host := strings.TrimPrefix(other.URL, "http://")
	for _, ref := range []string{host + "/canyon/claims-portal@" + builtDigest, e.Registry + "/other/claims-portal@" + builtDigest, e.Registry + "/canyon/claims-portal:src-abcd1234"} {
		if err := e.dropImage(t.Context(), ref); err == nil {
			t.Errorf("%s: deleted", ref)
		}
	}
	left := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
		"metadata": map[string]any{"name": "claims-portal-abcd1234", "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true"}},
		"spec":     map[string]any{"source": map[string]any{"registry": map[string]any{"image": host + "/canyon/claims-portal@" + builtDigest}}},
	}}
	if _, err := dc.Resource(buildGVR).Namespace(BuildNamespace).Create(t.Context(), left, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.SweepBuilds(t.Context())
	if n := asked.Load(); n != 0 {
		t.Fatalf("another host was asked %d times", n)
	}
	dotless := &Executor{Registry: "registry"}
	if _, _, err := dotless.pushSource(t.Context(), "claims-portal", "abcd1234", bytes.NewReader(tree(t))); err == nil || !strings.Contains(err.Error(), "CANYON_KUBE_REGISTRY") {
		t.Fatalf("a dotless registry: %v", err)
	}
}

// At start the executor deletes every Build labelled canyon.build a crash left, and its source image.
func TestSweepBuildsDeletesWhatACrashLeft(t *testing.T) {
	e, dc, _ := kpackCluster(t, "True", "")
	src, _, err := e.pushSource(t.Context(), "claims-portal", "abcd1234", bytes.NewReader(tree(t)))
	if err != nil {
		t.Fatal(err)
	}
	left := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
		"metadata": map[string]any{"name": "claims-portal-abcd1234", "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true"}},
		"spec":     map[string]any{"source": map[string]any{"registry": map[string]any{"image": src.String()}}},
	}}
	if _, err := dc.Resource(buildGVR).Namespace(BuildNamespace).Create(t.Context(), left, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.SweepBuilds(t.Context())
	if list, _ := dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("the sweep left %d Builds", len(list.Items))
	}
	if _, err := remote.Get(src); err == nil {
		t.Fatalf("the sweep left the source image %s", src)
	}
}

// A /build the contract refuses is refused before anything is pushed or created.
func TestBuildRefusesBeforeAnything(t *testing.T) {
	e, dc, _ := kpackCluster(t, "True", "")
	dc.ClearActions()
	for label, req := range map[string]executor.BuildRequest{
		"an app with _":          {App: "claims_portal", Package: "pkg_7d3e8f"},
		"a package outside pkg_": {App: "claims-portal", Package: "7d3e8f"},
		"a value name with =":    {App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{"A=B": "x"}},
		"a value with NUL":       {App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{"A": "a\x00b"}},
	} {
		if _, err := e.Build(t.Context(), req, bytes.NewReader(tree(t)), io.Discard); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %v, want a refusal", label, err)
		}
	}
	if n := len(dc.Actions()); n != 0 {
		t.Fatalf("a refused /build made %d calls", n)
	}
}

// GET /builder on Kubernetes is the ClusterBuilder canyon's status.latestImage (contracts/executor.md).
func TestTheBuilderIsTheClusterBuildersLatestImage(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	if got, err := e.BuilderImage(t.Context()); err != nil || got != "registry.example/canyon/builder@"+builtDigest {
		t.Fatalf("the builder: %q %v", got, err)
	}
}

// A build whose pod the cluster has no room to schedule answers NoRoom at the build's deadline, with the
// scheduler's own message from the pod's PodScheduled condition: capacity, never the package's failure
// nor Canyon's (contracts/executor.md, /build; #1637).
func TestABuildTheClusterHasNoRoomForSaysSo(t *testing.T) {
	e, dc, _ := kpackClusterPod(t, "Unknown", "", false)
	const said = "0/4 nodes are available: 4 Insufficient cpu."
	dc.PrependReactor("create", "builds", func(a k8stesting.Action) (bool, runtime.Object, error) {
		b := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.GetName() + "-build-pod", Namespace: BuildNamespace}}
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: said}}
		return false, nil, e.Client.(*fake.Clientset).Tracker().Add(pod)
	})
	e.BuildDeadline = time.Second
	// through the executor's HTTP answer and the server's client, the caller still listening: the executor's
	// own deadline ends before the caller's, so the answer reaches it (#1701's review)
	srv := httptest.NewServer(executor.Handler(e))
	defer srv.Close()
	_, err := (&executor.Client{Base: srv.URL}).Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree(t)), func(context.Context, string) error { return nil })
	if noRoom := (*executor.NoRoom)(nil); !errors.As(err, &noRoom) || noRoom.Reason != said {
		t.Fatalf("a build with no room: %v", err)
	}
}
