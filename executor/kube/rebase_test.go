package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/santhosh-tekuri/jsonschema/v6"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

const stack = "io.buildpacks.stacks.noble"

// copyTag is kpack's copy of the ClusterStack's run image, <the ClusterBuilder's spec.tag>-run-image, under
// the default spec.tag: the name every image the ClusterBuilder builds records.
const copyTag = "/canyon-builder:clusterbuilder-canyon-run-image"

// runImageIn pushes a run image of one random layer, labelled with stack id, as kpack copies one, and
// answers it as the ClusterBuilder's status.stack.runImage names it, by tag and digest, and as the
// rebaser records it, by repository and digest.
func runImageIn(t *testing.T, registry, stackID string) (img v1.Image, tagged, ref string) {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.Config(img, v1.Config{Labels: map[string]string{"io.buildpacks.stack.id": stackID}}); err != nil {
		t.Fatal(err)
	}
	sum, _ := img.Digest()
	if err := remote.Write(mustRef(t, registry+copyTag), img); err != nil {
		t.Fatal(err)
	}
	return img, registry + copyTag + "@" + sum.String(), registry + "/canyon-builder@" + sum.String()
}

// appOn is claims-portal's image as kpack exports it onto run, which tagged names: two app layers over
// run's, under the labels the lifecycle writes — labels with "" drop one — and the config an app starts from.
func appOn(t *testing.T, run v1.Image, tagged string, labels map[string]string) v1.Image {
	t.Helper()
	l := map[string]string{
		executor.LifecycleLabel:        lifecycleOn(t, run, tagged),
		"io.buildpacks.stack.id":       stack,
		"io.buildpacks.build.metadata": `{"buildpacks":[{"id":"paketo-buildpacks/node-engine"}],"launcher":{"version":"0.21.20"}}`,
	}
	for k, v := range labels {
		if v == "" {
			delete(l, k)
		} else {
			l[k] = v
		}
	}
	app, err := random.Image(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := app.Layers()
	img, err := mutate.AppendLayers(run, layers...)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Labels: l, Entrypoint: []string{"/cnb/process/web"}, Env: []string{"PATH=/cnb/process:/usr/bin", "NODE_ENV=production"}, WorkingDir: "/workspace", User: "1002:1001"})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// lifecycleOn is the lifecycle metadata of an image on run, which tagged names, as kpack 0.18 exports it,
// at platform API 0.9: the run image's tagged name under stack.runImage.image, not runImage.image, and
// runImage.reference its repository and digest alone, as imgutil's Identifier gives it.
func lifecycleOn(t *testing.T, run v1.Image, tagged string) string {
	t.Helper()
	config, err := run.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	d, err := name.NewDigest(tagged)
	if err != nil {
		t.Fatal(err)
	}
	top := config.RootFS.DiffIDs[len(config.RootFS.DiffIDs)-1].String()
	return `{"runImage":{"topLayer":"` + top + `","reference":"` + d.Context().Name() + "@" + d.DigestStr() + `"},"stack":{"runImage":{"image":"` + tagged + `"}}}`
}

// lifecycle013On is lifecycleOn at platform API 0.12 and later, which names the run image under
// runImage.image, as pack's builds do.
func lifecycle013On(t *testing.T, run v1.Image, tagged string) string {
	t.Helper()
	return strings.Replace(lifecycleOn(t, run, tagged), `"},"stack":{"runImage":{"image":"`+tagged+`"}}}`, `","image":"`+tagged+`"}}`, 1)
}

func diffIDs(t *testing.T, img v1.Image) []string {
	t.Helper()
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range config.RootFS.DiffIDs {
		out = append(out, d.String())
	}
	return out
}

// kpack is kpack's rebase of a Build: what it pushes under the Build's tag (nil: nothing), the Succeeded
// condition it ends the Build with ("": the pod never placed, the Build never ends) and the latestImage
// it records ("": the tag's image by digest).
type kpack func(b *unstructured.Unstructured) (pushed v1.Image, succeeded, latest string)

// rebaseScene is a registry holding kpack's copy of the run image twice — old, which claims-portal's image
// was built onto, and now, the ClusterBuilder canyon's run image — and a cluster whose ClusterBuilder is on
// the noble stack and whose Builds kpack plays.
type rebaseScene struct {
	e                    *Executor
	dc                   *dynamicfake.FakeDynamicClient
	builds               []*unstructured.Unstructured
	created              chan string // each Build's name, once created
	old, now             v1.Image
	oldTagged, nowTagged string // as the ClusterBuilder's status names them
	oldRef, nowRef       string // as run_image's form has them
	app                  v1.Image
	req                  executor.RebaseRequest
	kpack                kpack
	steps                []string // the steps kpack says it completed: its rebase's
	vanish               string   // a manifest digest the registry answers 404 for, once pushed
	log                  string   // what the rebase step of each Build's pod wrote
	held                 string   // the digest the Build's fresh tag named as it was created: "" for none
}

func newRebaseScene(t *testing.T, labels map[string]string) *rebaseScene {
	t.Helper()
	s := &rebaseScene{created: make(chan string, 8), steps: []string{"rebase", "completion"}, log: "===> REBASE\n"}
	inner := testRegistry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.vanish != "" && r.Method != http.MethodDelete && strings.HasSuffix(r.URL.Path, "/manifests/"+s.vanish) {
			http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
			return
		}
		r.URL.Scheme, r.URL.Host, r.RequestURI = "http", strings.TrimPrefix(inner.URL, "http://"), ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	registry := strings.TrimPrefix(srv.URL, "http://")
	s.old, s.oldTagged, s.oldRef = runImageIn(t, registry, stack)
	s.now, s.nowTagged, s.nowRef = runImageIn(t, registry, stack)
	s.app = appOn(t, s.old, s.oldTagged, labels)
	sum, _ := s.app.Digest()
	image := registry + "/canyon/claims-portal@" + sum.String()
	if err := remote.Write(mustRef(t, registry+"/canyon/claims-portal:pkg_7d3e8f-0a1b2c3d"), s.app); err != nil { // its build's tag
		t.Fatal(err)
	}
	s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) {
		return s.rebased(t, s.nowTagged), "True", ""
	}
	cb := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "ClusterBuilder", "metadata": map[string]any{"name": "canyon"},
		"status": map[string]any{"latestImage": registry + "/canyon-builder@" + builtDigest, "stack": map[string]any{"runImage": s.nowTagged, "id": stack}},
	}}
	s.dc = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{buildGVR: "BuildList", clusterBuilderGVR: "ClusterBuilderList"}, cb)
	pods := fake.NewClientset()
	s.dc.PrependReactor("create", "builds", func(a k8stesting.Action) (bool, runtime.Object, error) {
		b := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		s.builds = append(s.builds, b)
		s.created <- b.GetName()
		tags, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "tags")
		pushed, succeeded, latest := s.kpack(b)
		if d, err := remote.Head(mustRef(t, tags[0])); err == nil {
			s.held = d.Digest.String()
		} else { // kpack's rebase opens tags[0] before anything else and fails on one not there (kpack cmd/rebase, imgutil's Found: a HEAD of it), as on the AWS test cluster (#2576)
			last, _, _ := unstructured.NestedString(b.Object, "spec", "lastBuild", "image")
			pushed, succeeded, latest = nil, "False", ""
			s.log = "Loading cluster credential helpers\nERROR: could not access previous image: " + last + "\n"
		}
		if pushed != nil {
			if err := remote.Write(mustRef(t, tags[0]), pushed); err != nil {
				t.Error(err)
			}
			if d, _ := pushed.Digest(); latest == "" {
				latest = tags[0][:strings.LastIndex(tags[0], ":")] + "@" + d.String()
			}
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.GetName() + "-build-pod", Namespace: BuildNamespace}}
		pod.Spec.InitContainers = []corev1.Container{{Name: "rebase"}}
		switch succeeded {
		case "":
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/4 nodes are available: 4 Insufficient cpu."}}
		case "False":
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "rebase", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
		default:
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "rebase", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		}
		if err := pods.Tracker().Add(pod); err != nil {
			t.Error(err)
		}
		go func() { // kpack's status, after the create has landed
			done := b.DeepCopy()
			_ = unstructured.SetNestedField(done.Object, pod.Name, "status", "podName")
			if succeeded != "" {
				_ = unstructured.SetNestedStringSlice(done.Object, s.steps, "status", "stepsCompleted")
				_ = unstructured.SetNestedField(done.Object, latest, "status", "latestImage")
				_ = unstructured.SetNestedSlice(done.Object, []any{map[string]any{"type": "Succeeded", "status": succeeded}}, "status", "conditions")
			}
			for range 100 {
				if err := s.dc.Tracker().Update(buildGVR, done, BuildNamespace); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return false, nil, nil
	})
	pods.PrependReactor("get", "pods/log", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &runtime.Unknown{Raw: []byte(s.log)}, nil
	})
	s.e = &Executor{Client: pods, Dynamic: s.dc, Registry: registry, Poll: time.Millisecond}
	if err := s.e.WatchBuilds(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.req = executor.RebaseRequest{App: "claims-portal", Package: "pkg_7d3e8f", Image: image, RunImage: s.nowRef}
	return s
}

// rebased is the app's image as the lifecycle's rebaser leaves it: onto now, its metadata naming the run
// image tagged.
func (s *rebaseScene) rebased(t *testing.T, tagged string) v1.Image {
	t.Helper()
	img, err := mutate.Rebase(s.app, s.old, s.now)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := img.ConfigFile()
	config.Config.Labels[executor.LifecycleLabel] = lifecycleOn(t, s.now, tagged)
	if img, err = mutate.Config(img, config.Config); err != nil {
		t.Fatal(err)
	}
	return img
}

// placeholderGone checks what the fresh tag of the Build b named as b was created: the request's image's
// manifest with the one annotation canyon.rebase, b's nonce — its config and layers the image's — and that
// it is gone from the registry once the rebase answered (contracts/executor.md, POST /rebase, #2576).
func (s *rebaseScene) placeholderGone(t *testing.T, b *unstructured.Unstructured) {
	t.Helper()
	want, err := mutate.Annotations(s.app, map[string]string{"canyon.rebase": strings.TrimPrefix(b.GetName(), "claims-portal-")}).(v1.Image).Digest()
	if err != nil {
		t.Fatal(err)
	}
	if s.held != want.String() {
		t.Errorf("the fresh tag named %q as the Build was created, want the image's manifest annotated with its nonce, %s", s.held, want)
	}
	if _, err := remote.Head(mustRef(t, s.e.Registry+"/canyon/claims-portal@"+want.String())); !notFound(err) {
		t.Errorf("the placeholder %s outlived the rebase: %v", want, err)
	}
}

// tags are the tags of claims-portal's repository but its build's.
func (s *rebaseScene) tags(t *testing.T) []string {
	t.Helper()
	tags, err := remote.List(mustRepo(t, s.e.Registry+"/canyon/claims-portal"))
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(tags, func(tag string) bool { return tag == "pkg_7d3e8f-0a1b2c3d" })
}

// postRebase is POST /rebase on the Kubernetes executor's handler: its status and body.
func postRebase(t *testing.T, e *Executor, req executor.RebaseRequest) (int, string) {
	t.Helper()
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return send(t, "POST", srv.URL+"/rebase", string(body))
}

// validates holds body to executor.schema.json's definition.
func validates(t *testing.T, definition, body string) {
	t.Helper()
	s, err := jsonschema.NewCompiler().Compile("../../contracts/schema/executor.schema.json#/definitions/" + definition)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
}

// contracts/executor.md, POST /rebase on Kubernetes: kpack's own rebase, a Build <app>-<nonce> in
// canyon-builds whose spec.lastBuild names the image on its stack, whose one reason is STACK and whose
// spec.runImage pins the run image the plan named, to a fresh tag; the answer is the tag's image by digest
// and both images' facts, read from the registry — the same facts Docker answers: the layers above the run
// image's top one the same, in order, below them the new run image's, on it. The ClusterBuilder names the
// run image as kpack does, <registry>/canyon-builder:clusterbuilder-canyon-run-image@sha256:…, and the
// rebaser records <registry>/canyon-builder@sha256:…: run_image is the second, before and after. kpack's
// rebase opens the fresh tag first, so it is written before the Build, onto a placeholder deleted after (#2576).
func TestRebaseKubeSameFacts(t *testing.T) {
	s := newRebaseScene(t, nil)
	status, body := postRebase(t, s.e, s.req)
	if status != 200 {
		t.Fatalf("POST /rebase: %d %s", status, body)
	}
	validates(t, "rebase_result", body)
	var res executor.RebaseResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	b, a := res.Before, res.After
	if !slices.Equal(b.DiffIDs, diffIDs(t, s.app)) || b.RunImage != s.oldRef || b.TopLayer != diffIDs(t, s.old)[0] {
		t.Errorf("before is not the image's: %+v", b)
	}
	if a.RunImage != s.req.RunImage || a.TopLayer != diffIDs(t, s.now)[0] || a.DiffIDs[0] != a.TopLayer {
		t.Errorf("after is not on the planned run image %s: %+v", s.req.RunImage, a)
	}
	if !slices.Equal(a.DiffIDs[1:], b.DiffIDs[1:]) || a.BuildMetadata != b.BuildMetadata || a.Config != b.Config || a.StackID != b.StackID {
		t.Errorf("more than the run image changed:\n before %+v\n after  %+v", b, a)
	}
	tags := s.tags(t)
	if len(tags) != 1 || !strings.HasPrefix(tags[0], "pkg_7d3e8f-") {
		t.Fatalf("the rebased image's tags: %v, want one fresh pkg_7d3e8f-<nonce>", tags)
	}
	if d, err := remote.Head(mustRef(t, s.e.Registry+"/canyon/claims-portal:"+tags[0])); err != nil || res.Image != s.e.Registry+"/canyon/claims-portal@"+d.Digest.String() {
		t.Errorf("image %s is not its fresh tag's digest: %v", res.Image, err)
	}
	if _, err := remote.Head(mustRef(t, s.req.Image)); err != nil {
		t.Errorf("the image the request named is gone: %v", err)
	}
	if len(s.builds) != 1 {
		t.Fatalf("Builds created: %d", len(s.builds))
	}
	b0 := s.builds[0]
	spec, _, _ := unstructured.NestedMap(b0.Object, "spec")
	want := map[string]any{
		"tags": []any{s.e.Registry + "/canyon/claims-portal:" + tags[0]}, "builder": map[string]any{"image": s.e.Registry + "/canyon-builder@" + builtDigest},
		"serviceAccountName": "canyon-build", "source": map[string]any{"registry": map[string]any{"image": s.req.Image}},
		"lastBuild": map[string]any{"image": s.req.Image, "stackId": stack}, "runImage": map[string]any{"image": s.req.RunImage},
	}
	if b0.GetNamespace() != BuildNamespace || b0.GetName() != "claims-portal-"+strings.TrimPrefix(tags[0], "pkg_7d3e8f-") || b0.GetAnnotations()["image.kpack.io/reason"] != "STACK" ||
		len(b0.GetAnnotations()) != 1 || b0.GetLabels()["canyon.build"] != "true" || b0.GetLabels()["canyon.app"] != "claims-portal" || !equality.Semantic.DeepEqual(spec, want) {
		t.Errorf("the Build is not kpack's rebase onto the plan's run image:\n%s/%s %v %v\n%v", b0.GetNamespace(), b0.GetName(), b0.GetLabels(), b0.GetAnnotations(), spec)
	}
	s.placeholderGone(t, b0)
	if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
		t.Errorf("the Build outlived the rebase: %v", builds.Items)
	}
}

// design/PATCHING.md §4.1: a new run image keeps its name. kpack names its copy of the run image after the
// ClusterBuilder's spec.tag, so an image built while that tag named a mirror records the mirror's name, and
// is refused 409 another_run_image, never forced, before any Build is created or anything pushed —
// kpack's rebaser, at platform API 0.9, would not check the name. Its run image, moved to another
// repository by the ClusterStack, is not: kpack's copy keeps its name (TestRebaseKubeSameFacts).
func TestRebaseMirrorUnderAnotherNameRefused(t *testing.T) {
	for platform, metadata := range map[string]func(*testing.T, v1.Image, string) string{"0.9": lifecycleOn, "0.13": lifecycle013On} {
		s := newRebaseScene(t, nil)
		mirror := "mirror.example/canyon-builder:clusterbuilder-canyon-run-image@" + strings.Split(s.oldRef, "@")[1]
		s.useApp(t, appOn(t, s.old, s.oldTagged, map[string]string{executor.LifecycleLabel: metadata(t, s.old, mirror)}))
		status, body := postRebase(t, s.e, s.req)
		var refused struct{ Error, Message string }
		if err := json.Unmarshal([]byte(body), &refused); err != nil || status != 409 || refused.Error != "another_run_image" || !strings.Contains(refused.Message, "mirror.example/canyon-builder") {
			t.Fatalf("a mirror under another name, platform API %s: %d %s", platform, status, body)
		}
		validates(t, "rebase_refused", body)
		nothingWritten(t, s)
	}
}

// useApp makes app, pushed by digest, the image the scene's request rebases.
func (s *rebaseScene) useApp(t *testing.T, app v1.Image) {
	t.Helper()
	s.app = app
	sum, _ := app.Digest()
	s.req.Image = s.e.Registry + "/canyon/claims-portal@" + sum.String()
	if err := remote.Write(mustRef(t, s.req.Image), app); err != nil {
		t.Fatal(err)
	}
}

// An image pack built at platform API 0.12 or later names its run image under runImage.image, and is
// rebased as kpack's are.
func TestRebaseKubeReadsTheNewerRunImageName(t *testing.T) {
	s := newRebaseScene(t, nil)
	s.useApp(t, appOn(t, s.old, s.oldTagged, map[string]string{executor.LifecycleLabel: lifecycle013On(t, s.old, s.oldTagged)}))
	if status, body := postRebase(t, s.e, s.req); status != 200 {
		t.Fatalf("a platform API 0.13 image: %d %s", status, body)
	}
}

// nothingWritten is a refusal's mark: no Build, and no tag but the image's own.
func nothingWritten(t *testing.T, s *rebaseScene) {
	t.Helper()
	if len(s.builds) != 0 {
		t.Errorf("a refusal created Builds: %v", s.builds)
	}
	if tags := s.tags(t); len(tags) != 0 {
		t.Errorf("a refusal pushed tags %v", tags)
	}
}

// Refused, never forced (409, nothing written), and a request out of shape (400), an image not there (404)
// or a fact that cannot be read (502) answer before any Build is created.
func TestRebaseKubeRefusesNeverForces(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		req    func(s *rebaseScene) executor.RebaseRequest
		status int
		says   string
	}{
		{name: "rebasable false", labels: map[string]string{"io.buildpacks.rebasable": "false"}, status: 409, says: "not_rebasable"},
		{name: "rebasable not true", labels: map[string]string{"io.buildpacks.rebasable": "False"}, status: 409, says: "not_rebasable"},
		{name: "another stack", labels: map[string]string{"io.buildpacks.stack.id": "io.buildpacks.stacks.jammy"}, status: 409, says: "another_stack"},
		{name: "the run image moved", req: func(s *rebaseScene) executor.RebaseRequest { r := s.req; r.RunImage = s.oldRef; return r }, status: 409, says: "run_image_moved"},
		{name: "a run image of another tag", labels: map[string]string{executor.LifecycleLabel: `{"runImage":{"topLayer":"sha256:` + strings.Repeat("1a", 32) + `","reference":"r.example/canyon-run@sha256:` + strings.Repeat("4d", 32) + `","image":"canyon-run:jammy"}}`}, status: 409, says: "another_run_image"},
		{name: "an image by tag", req: func(s *rebaseScene) executor.RebaseRequest {
			r := s.req
			r.Image = s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-0a1b2c3d"
			return r
		}, status: 400},
		{name: "another app's image", req: func(s *rebaseScene) executor.RebaseRequest {
			r := s.req
			r.Image = strings.Replace(r.Image, "/canyon/claims-portal@", "/canyon/other@", 1)
			return r
		}, status: 400},
		{name: "another registry's image", req: func(s *rebaseScene) executor.RebaseRequest {
			r := s.req
			r.Image = "registry.example/canyon/claims-portal@" + strings.Split(r.Image, "@")[1]
			return r
		}, status: 400},
		{name: "no run image", req: func(s *rebaseScene) executor.RebaseRequest { r := s.req; r.RunImage = ""; return r }, status: 400},
		{name: "an image not there", req: func(s *rebaseScene) executor.RebaseRequest {
			r := s.req
			r.Image = s.e.Registry + "/canyon/claims-portal@sha256:" + strings.Repeat("0", 64)
			return r
		}, status: 404},
		{name: "no lifecycle metadata", labels: map[string]string{executor.LifecycleLabel: ""}, status: 502, says: "io.buildpacks.lifecycle.metadata"},
		{name: "no build metadata", labels: map[string]string{"io.buildpacks.build.metadata": ""}, status: 502, says: "io.buildpacks.build.metadata"},
		{name: "a run image reference by tag", labels: map[string]string{executor.LifecycleLabel: `{"runImage":{"topLayer":"sha256:` + strings.Repeat("1a", 32) + `","reference":"r.example/canyon-run:noble","image":"r.example/canyon-run:noble"}}`}, status: 502, says: "not a reference by digest"},
		{name: "a run image flag", labels: map[string]string{executor.LifecycleLabel: `{"runImage":{"topLayer":"sha256:` + strings.Repeat("1a", 32) + `","reference":"--force","image":"--force"}}`}, status: 502, says: "not a reference by digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRebaseScene(t, tc.labels)
			req := s.req
			if tc.req != nil {
				req = tc.req(s)
			}
			status, body := postRebase(t, s.e, req)
			if status != tc.status || !strings.Contains(body, tc.says) {
				t.Fatalf("%d %s, want %d saying %q", status, body, tc.status, tc.says)
			}
			if status == 409 {
				validates(t, "rebase_refused", body)
			}
			nothingWritten(t, s)
		})
	}
}

// Every failure once the Build is created removes the fresh tag, and the image the request named, under its
// own tag, stays: kpack's rebase failed, before or after it pushed; the rebased image on another run image
// than the plan's; and a latestImage that is not the fresh tag's image — another Build's, a stale read —
// which is never answered as this rebase's.
func TestRebaseKubeRemovesTheFreshTagOnEveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kpack func(s *rebaseScene) kpack
		says  string
	}{
		{name: "kpack failed after it pushed", kpack: func(s *rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) {
				return s.rebased(t, s.nowTagged), "False", ""
			}
		}, says: "the rebase step exited 1"},
		{name: "kpack failed before it pushed", kpack: func(*rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) { return nil, "False", "" }
		}, says: "the rebase step exited 1"},
		{name: "a rebase onto another run image", kpack: func(s *rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) {
				return s.rebased(t, s.oldTagged), "True", ""
			}
		}, says: "the run image moved under the rebase"},
		{name: "a latestImage of another image", kpack: func(s *rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) {
				return s.rebased(t, s.nowTagged), "True", s.req.Image
			}
		}, says: "answered image"},
		{name: "a Build kpack built rather than rebased", kpack: func(s *rebaseScene) kpack {
			s.steps = []string{"prepare", "analyze", "detect", "restore", "build", "export", "completion"}
			return func(*unstructured.Unstructured) (v1.Image, string, string) {
				return s.rebased(t, s.nowTagged), "True", ""
			}
		}, says: "not kpack's rebase"},
		{name: "a rebased image gone before its facts are read", kpack: func(s *rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) {
				img := s.rebased(t, s.nowTagged)
				d, _ := img.Digest()
				s.vanish = d.String()
				return img, "True", ""
			}
		}, says: "is gone from the registry"},
		{name: "a latestImage with nothing written over the placeholder", kpack: func(s *rebaseScene) kpack {
			return func(*unstructured.Unstructured) (v1.Image, string, string) { return nil, "True", s.req.Image }
		}, says: "answered image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRebaseScene(t, nil)
			s.kpack = tc.kpack(s)
			status, body := postRebase(t, s.e, s.req)
			if status != 502 || !strings.Contains(body, tc.says) {
				t.Fatalf("%d %s, want 502 saying %q", status, body, tc.says)
			}
			if tags := s.tags(t); len(tags) != 0 {
				t.Errorf("the fresh tag survived the failure: %v", tags)
			}
			s.placeholderGone(t, s.builds[0])
			if _, err := remote.Head(mustRef(t, s.req.Image)); err != nil {
				t.Errorf("the image the request named is gone: %v", err)
			}
			if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
				t.Errorf("the Build outlived the failure: %v", builds.Items)
			}
		})
	}
}

// A rebase whose pod the cluster had no room for by the deadline answers 502 with the scheduler's words.
func TestRebaseKubeTheClusterHasNoRoomForSaysSo(t *testing.T) {
	s := newRebaseScene(t, nil)
	s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) { return nil, "", "" }
	s.e.BuildDeadline = 50 * time.Millisecond
	status, body := postRebase(t, s.e, s.req)
	if status != 502 || body != `{"error":"the cluster had no room to run the rebase","no_room":"0/4 nodes are available: 4 Insufficient cpu."}` {
		t.Fatalf("%d %s", status, body)
	}
	validates(t, "rebase_failed", body)
	if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
		t.Errorf("the Build outlived the deadline: %v", builds.Items)
	}
}

// A caller gone while kpack's pod still runs cuts no cleanup short: the Build is deleted in the foreground
// and the fresh tag removed only once the Build, and so its pod, is gone — here the pod pushes the tag on
// its way out, as a rebase cut short at its push does. Cleanup on the caller's context would leave it.
func TestRebaseKubeCleansUpAfterTheCallerIsGone(t *testing.T) {
	s := newRebaseScene(t, nil)
	s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) { return nil, "", "" } // running, never done
	s.dc.PrependReactor("delete", "builds", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := a.(k8stesting.DeleteActionImpl)
		tags, _, _ := unstructured.NestedStringSlice(s.builds[0].Object, "spec", "tags")
		gone := func() { _ = s.dc.Tracker().Delete(buildGVR, BuildNamespace, d.Name) }
		if p := d.DeleteOptions.PropagationPolicy; p == nil || *p != metav1.DeletePropagationForeground {
			gone() // the Build goes at once, its pod later
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			if err := remote.Write(mustRef(t, tags[0]), s.rebased(t, s.nowTagged)); err != nil {
				t.Error(err)
			}
			gone() // in the foreground, the Build goes once its pod has
		}()
		return true, nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-s.created
		cancel()
	}()
	if _, err := s.e.Rebase(ctx, s.req); err == nil {
		t.Fatal("a rebase whose caller is gone answered")
	}
	if tags := s.tags(t); len(tags) != 0 {
		t.Errorf("the fresh tag outlived the cleanup: %v", tags)
	}
	if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
		t.Errorf("the Build outlived the cleanup: %v", builds.Items)
	}
}

// A rebase is deterministic: a retry's image may be the one a deployed tag already names. Its failure
// removes no digest another tag names, and so never the deployed image.
func TestRebaseKubeFailureKeepsAnImageAnotherTagNames(t *testing.T) {
	s := newRebaseScene(t, nil)
	deployed := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-1e2f3a4b"
	if err := remote.Write(mustRef(t, deployed), s.rebased(t, s.nowTagged)); err != nil {
		t.Fatal(err)
	}
	s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) {
		return s.rebased(t, s.nowTagged), "False", ""
	}
	if status, body := postRebase(t, s.e, s.req); status != 502 {
		t.Fatalf("%d %s", status, body)
	}
	if _, err := remote.Head(mustRef(t, deployed)); err != nil {
		t.Errorf("a failed retry deleted the image a deployed tag names: %v", err)
	}
}

// GET /builder's run_image on Kubernetes is the ClusterBuilder's status.stack.runImage, kpack's copy named
// by tag and digest, as <repository>@sha256:… with the tag stripped, and 502 while it names none by digest.
func TestGetBuilderAnswersTheRunImageOnKubernetes(t *testing.T) {
	s := newRebaseScene(t, nil)
	srv := httptest.NewServer(Handler(s.e))
	t.Cleanup(srv.Close)
	if status, body := send(t, "GET", srv.URL+"/builder", ""); status != 200 || !strings.Contains(body, `"run_image":"`+s.nowRef+`"`) || !strings.Contains(s.nowTagged, ":clusterbuilder-canyon-run-image@") {
		t.Fatalf("GET /builder: %d %s", status, body)
	}
	cb, err := s.e.Dynamic.Resource(clusterBuilderGVR).Get(t.Context(), "canyon", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cb.Object, strings.Split(s.nowTagged, "@")[0], "status", "stack", "runImage")
	if _, err := s.e.Dynamic.Resource(clusterBuilderGVR).Update(t.Context(), cb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if status, body := send(t, "GET", srv.URL+"/builder", ""); status != 502 {
		t.Errorf("a run image by tag: %d %s", status, body)
	}
}

// A ClusterBuilder whose stack id is unread is a 502, never another_stack, which is permanent for the
// pair: no Build is created.
func TestRebaseKubeAnUnreadStackIsNoRefusal(t *testing.T) {
	s := newRebaseScene(t, nil)
	cb, err := s.dc.Resource(clusterBuilderGVR).Get(t.Context(), "canyon", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(cb.Object, "status", "stack", "id")
	if _, err := s.dc.Resource(clusterBuilderGVR).Update(t.Context(), cb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if status, body := postRebase(t, s.e, s.req); status != 502 || !strings.Contains(body, "stack id") {
		t.Fatalf("no stack id on the ClusterBuilder: %d %s", status, body)
	}
	nothingWritten(t, s)
}

// SweepBuilds deletes a rebase Build a crash left and its fresh tag, which its pod may have pushed, and
// never its source: the image it rebased.
func TestSweepBuildsNeverDeletesARebasesImage(t *testing.T) {
	s := newRebaseScene(t, nil)
	fresh := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-5c6d7e8f9a0b1c2d"
	if err := remote.Write(mustRef(t, fresh), s.rebased(t, s.nowTagged)); err != nil {
		t.Fatal(err)
	}
	left := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
		"metadata": map[string]any{"name": "claims-portal-5c6d7e8f9a0b1c2d", "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true", "canyon.app": "claims-portal"},
			"annotations": map[string]any{"image.kpack.io/reason": "STACK"}},
		"spec": map[string]any{"tags": []any{fresh}, "source": map[string]any{"registry": map[string]any{"image": s.req.Image}}},
	}}
	if err := s.dc.Tracker().Add(left); err != nil {
		t.Fatal(err)
	}
	s.e.SweepBuilds(t.Context())
	if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
		t.Errorf("the sweep left %v", builds.Items)
	}
	if _, err := remote.Head(mustRef(t, s.req.Image)); err != nil {
		t.Errorf("the sweep deleted the image the rebase rebased: %v", err)
	}
	if tags := s.tags(t); len(tags) != 0 {
		t.Errorf("the sweep left the crashed rebase's fresh tag: %v", tags)
	}
}

// The sweep removes only a crashed rebase's own fresh tag, and keeps it when something still uses its
// image: another tag naming its digest — a retry onto the same run image makes a deployed image again — or
// a Deployment running it. It never touches a tag a Build of another kind, or one not its own, names.
func TestSweepBuildsKeepsWhatSomethingStillUses(t *testing.T) {
	const nonce = "5c6d7e8f9a0b1c2d"
	build := func(s *rebaseScene, name, tag string, annotations map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
			"metadata": map[string]any{"name": name, "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true", "canyon.app": "claims-portal"},
				"annotations": annotations},
			"spec": map[string]any{"tags": []any{tag}, "source": map[string]any{"registry": map[string]any{"image": s.req.Image}}},
		}}
	}
	stackReason := map[string]any{"image.kpack.io/reason": "STACK"}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *rebaseScene, fresh string) (*unstructured.Unstructured, []string)
	}{
		{"a crashed rebase whose digest a deployed tag also names", func(t *testing.T, s *rebaseScene, fresh string) (*unstructured.Unstructured, []string) {
			deployed := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-1e2f3a4b"
			if err := remote.Write(mustRef(t, deployed), s.rebased(t, s.nowTagged)); err != nil {
				t.Fatal(err)
			}
			return build(s, "claims-portal-"+nonce, fresh, stackReason), []string{fresh, deployed}
		}},
		{"a crashed rebase whose image a Deployment runs", func(t *testing.T, s *rebaseScene, fresh string) (*unstructured.Unstructured, []string) {
			d, _ := s.rebased(t, s.nowTagged).Digest()
			running := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "claims-portal-preview", Namespace: Namespace}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: s.e.Registry + "/canyon/claims-portal@" + d.String()}}}}}}
			if err := s.e.Client.(*fake.Clientset).Tracker().Add(running); err != nil {
				t.Fatal(err)
			}
			return build(s, "claims-portal-"+nonce, fresh, stackReason), []string{fresh}
		}},
		{"a rebase Build whose tag is not its own", func(_ *testing.T, s *rebaseScene, fresh string) (*unstructured.Unstructured, []string) {
			return build(s, "claims-portal-0a0a0a0a0a0a0a0a", fresh, stackReason), []string{fresh}
		}},
		{"a rebase Build whose name carries no nonce", func(t *testing.T, s *rebaseScene, _ string) (*unstructured.Unstructured, []string) {
			named := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-deployed"
			if err := remote.Write(mustRef(t, named), s.rebased(t, s.oldTagged)); err != nil {
				t.Fatal(err)
			}
			return build(s, "claims-portal-deployed", named, stackReason), []string{named}
		}},
		{"a build's Build", func(_ *testing.T, s *rebaseScene, fresh string) (*unstructured.Unstructured, []string) {
			b := build(s, "claims-portal-"+nonce, fresh, nil)
			unstructured.RemoveNestedField(b.Object, "spec", "source") // its source is its own, which the sweep deletes
			return b, []string{fresh}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRebaseScene(t, nil)
			fresh := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-" + nonce
			if err := remote.Write(mustRef(t, fresh), s.rebased(t, s.nowTagged)); err != nil {
				t.Fatal(err)
			}
			left, kept := tc.setup(t, s, fresh)
			if err := s.dc.Tracker().Add(left); err != nil {
				t.Fatal(err)
			}
			s.e.SweepBuilds(t.Context())
			for _, tag := range append(kept, s.req.Image) {
				if _, err := remote.Head(mustRef(t, tag)); err != nil {
					t.Errorf("the sweep removed %s: %v", tag, err)
				}
			}
		})
	}
}

// The fresh tag goes only once the Build, and so its pod, is seen gone: a delete refused, or a Build that
// outlives its deletion, leaves it, logged, rather than race a pod that could push it again.
func TestRebaseKubeKeepsTheFreshTagUntilItsBuildIsSeenGone(t *testing.T) {
	defer func(was time.Duration) { rebaseGone = was }(rebaseGone)
	rebaseGone = 100 * time.Millisecond
	for _, tc := range []struct {
		name   string
		delete func(s *rebaseScene) (bool, runtime.Object, error)
	}{
		{"a delete the API refuses", func(*rebaseScene) (bool, runtime.Object, error) {
			return true, nil, errors.New("etcdserver: request timed out")
		}},
		{"a Build that outlives its deletion", func(*rebaseScene) (bool, runtime.Object, error) {
			return true, nil, nil // accepted; its pod never ends, so the foreground deletion never does
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRebaseScene(t, nil)
			s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) {
				return s.rebased(t, s.nowTagged), "False", ""
			}
			s.dc.PrependReactor("delete", "builds", func(k8stesting.Action) (bool, runtime.Object, error) { return tc.delete(s) })
			if status, body := postRebase(t, s.e, s.req); status != 502 {
				t.Fatalf("%d %s", status, body)
			}
			if tags := s.tags(t); len(tags) != 1 {
				t.Errorf("the fresh tag went while its Build was not seen gone: %v", tags)
			}
		})
	}
}

// A create whose answer is lost — the caller gone, the connection cut — may still have made the Build: the
// cleanup, registered before it, deletes it all the same. A create refused made none. Either way the fresh
// tag, written before the create (#2576), is gone, and its placeholder with it.
func TestRebaseKubeCleansUpAfterACreateThatFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		made bool
		says string
	}{
		{"its answer lost", true, "http2: client connection lost"},
		{"refused", false, "admission webhook validation.webhook.kpack.io denied the request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRebaseScene(t, nil)
			s.dc.PrependReactor("create", "builds", func(a k8stesting.Action) (bool, runtime.Object, error) {
				b := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
				s.builds = append(s.builds, b.DeepCopy())
				tags, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "tags")
				if d, err := remote.Head(mustRef(t, tags[0])); err == nil {
					s.held = d.Digest.String()
				}
				if tc.made {
					if err := s.dc.Tracker().Create(buildGVR, b, BuildNamespace); err != nil {
						t.Error(err)
					}
				}
				return true, nil, errors.New(tc.says)
			})
			if status, body := postRebase(t, s.e, s.req); status != 502 || !strings.Contains(body, tc.says) {
				t.Fatalf("%d %s", status, body)
			}
			if builds, _ := s.dc.Resource(buildGVR).Namespace(BuildNamespace).List(t.Context(), metav1.ListOptions{}); len(builds.Items) != 0 {
				t.Errorf("the Build outlived the rebase: %v", builds.Items)
			}
			if tags := s.tags(t); len(tags) != 0 {
				t.Errorf("a tag was left: %v", tags)
			}
			s.placeholderGone(t, s.builds[0])
		})
	}
}

// kpack's rebase writes its error to its step's log alone, and a rebase streams no log: a failed rebase
// answers the step that exited and the last line it wrote, cut to 512 bytes, and the executor's log says
// the same (contracts/executor.md, POST /rebase, #2576).
func TestRebaseKubeSaysWhatKpackSaid(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	long := strings.Repeat("é", 400) // 800 bytes: cut at 512, inside a rune
	for _, tc := range []struct {
		name, log, said string
	}{
		{"kpack's error", "Loading cluster credential helpers\nERROR: could not access run image: r.example/canyon-builder@sha256:" + strings.Repeat("4d", 32) + "\n\n",
			"ERROR: could not access run image: r.example/canyon-builder@sha256:" + strings.Repeat("4d", 32)},
		{"a line past 512 bytes", "ERROR: " + long + "\n", "ERROR: " + strings.Repeat("é", 252) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged.Reset()
			s := newRebaseScene(t, nil)
			s.kpack = func(*unstructured.Unstructured) (v1.Image, string, string) {
				s.log = tc.log
				return nil, "False", ""
			}
			want := "the rebase step exited 1: " + tc.said
			status, body := postRebase(t, s.e, s.req)
			if raw, _ := json.Marshal(map[string]string{"error": want}); status != 502 || body != string(raw) {
				t.Fatalf("%d %s, want 502 %s", status, body, raw)
			}
			validates(t, "rebase_failed", body)
			if !strings.Contains(logged.String(), "rebase build "+s.builds[0].GetName()+": "+want+"\n") {
				t.Errorf("the executor's log does not say why: %q", logged.String())
			}
		})
	}
}

// A fresh tag already there is never written over: the rebase fails before it writes anything
// (contracts/executor.md, POST /rebase, #2576).
func TestRebaseKubeNeverWritesOverAFreshTag(t *testing.T) {
	s := newRebaseScene(t, nil)
	there := s.e.Registry + "/canyon/claims-portal:pkg_7d3e8f-5c6d7e8f9a0b1c2d"
	if err := remote.Write(mustRef(t, there), s.rebased(t, s.nowTagged)); err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(there, name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.e.placeholder(t.Context(), mustRef(t, s.req.Image), tag, "5c6d7e8f9a0b1c2d"); err == nil || !strings.Contains(err.Error(), "already there") {
		t.Errorf("a placeholder over a tag already there: %v", err)
	}
}
