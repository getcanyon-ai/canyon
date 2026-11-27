package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/getcanyon-ai/canyon/executor"
)

// BuildNamespace holds the kpack Builds, labelled canyon.build.
const BuildNamespace = "canyon-builds"

var (
	buildGVR          = schema.GroupVersionResource{Group: "kpack.io", Version: "v1alpha2", Resource: "builds"}
	clusterBuilderGVR = schema.GroupVersionResource{Group: "kpack.io", Version: "v1alpha2", Resource: "clusterbuilders"}
	packageRe         = regexp.MustCompile(`^pkg_[a-z0-9]{4,}$`) // contracts/schema/executor.schema.json: package
)

// watcher hands each update of a Build to the /build waiting for it: one informer over every Build
// labelled canyon.build, as the estate's build controller watches its Images.
type watcher struct {
	mu      sync.Mutex
	waiting map[string]chan *unstructured.Unstructured
}

// WatchBuilds starts the informer /build waits on, and returns once its cache has synced; it runs
// until ctx ends. The binary calls it once, at start.
func (e *Executor) WatchBuilds(ctx context.Context) error {
	e.watch = &watcher{waiting: map[string]chan *unstructured.Unstructured{}}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(e.Dynamic, 30*time.Second, BuildNamespace, func(o *metav1.ListOptions) {
		o.LabelSelector = "canyon.build=true"
	})
	informer := factory.ForResource(buildGVR).Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    e.watch.deliver,
		UpdateFunc: func(_, obj any) { e.watch.deliver(obj) },
		DeleteFunc: e.watch.gone,
	}); err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return errors.New("the Build informer did not sync")
	}
	return nil
}

// deliver passes the newest state of a Build to its waiter, replacing one it has not read yet: a
// waiter wants where the Build is, not every step it took to get there.
func (w *watcher) deliver(obj any) {
	b, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := w.waiting[b.GetName()]
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
	}
	ch <- b
}

// gone closes the waiter of a Build deleted before it finished: its /build answers at once.
func (w *watcher) gone(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	b, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.waiting[b.GetName()]; ch != nil {
		delete(w.waiting, b.GetName())
		close(ch)
	}
}

func (w *watcher) wait(name string) chan *unstructured.Unstructured {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := make(chan *unstructured.Unstructured, 1)
	w.waiting[name] = ch
	return ch
}

func (w *watcher) forget(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waiting, name)
}

// BuilderImage is the ClusterBuilder canyon's status.latestImage, the builder a /build now builds with
// (contracts/executor.md, GET /builder); none yet is an error.
func (e *Executor) BuilderImage(ctx context.Context) (string, error) {
	_, builder, err := e.clusterBuilder(ctx)
	return builder, err
}

func (e *Executor) clusterBuilder(ctx context.Context) (*unstructured.Unstructured, string, error) {
	cb, err := e.Dynamic.Resource(clusterBuilderGVR).Get(ctx, "canyon", metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("the ClusterBuilder canyon: %w", err)
	}
	builder, _, _ := unstructured.NestedString(cb.Object, "status", "latestImage")
	if builder == "" {
		return nil, "", errors.New("the ClusterBuilder canyon has no image yet")
	}
	return cb, builder, nil
}

// Build pushes the tree as a source image and builds it with a kpack Build on the ClusterBuilder
// canyon, streaming the build pod's step containers' logs to out in step order, and answers the
// Build's latestImage. Build values are the Build's literal spec.env and in no other object; the
// Build lives only as long as the build: once it answers, image or error, the Build and the source
// image are deleted (contracts/executor.md, On Kubernetes, /build).
func (e *Executor) Build(ctx context.Context, req executor.BuildRequest, tree io.Reader, out io.Writer) (executor.BuildResult, error) {
	own, stop := context.WithTimeout(ctx, cmp.Or(e.BuildDeadline, 29*time.Minute)) // the executor's own deadline, before the server's 30 minutes give up on the answer (#1637)
	defer stop()
	if !validApp(req.App) || !packageRe.MatchString(req.Package) {
		return executor.BuildResult{}, refuse("app must be a project id of at most 55 characters and package pkg_ and at least 4 of [a-z0-9]")
	}
	values := executor.BuildEnv(req.BuildValues)
	env := []any{}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		v := values[k]
		if !envName.MatchString(k) || strings.IndexByte(v, 0) >= 0 || len(v) > 64<<10 {
			return executor.BuildResult{}, refuse("build value %q: a name [A-Za-z_][A-Za-z0-9_]*, a value without NUL of at most 64 KiB", k)
		}
		env = append(env, map[string]any{"name": k, "value": v})
	}
	// The executor's watch, the ClusterBuilder, the registry and the cluster's API are the host's, never the
	// package's (#2537): each is HostFailed, which the handler streams with own: true.
	if e.watch == nil {
		return executor.BuildResult{}, &executor.HostFailed{Message: "the executor is not watching Builds"}
	}
	start := time.Now()
	cb, builder, err := e.clusterBuilder(ctx)
	if err != nil {
		return executor.BuildResult{}, &executor.HostFailed{Message: err.Error()}
	}
	if err := e.ensureRepo(ctx, req.App); err != nil {
		return executor.BuildResult{}, &executor.HostFailed{Message: err.Error()}
	}
	run := nonce()
	src, saw, err := e.pushSource(ctx, req.App, run, tree)
	if err != nil {
		return executor.BuildResult{}, err
	}
	name := req.App + "-" + run
	updates := e.watch.wait(name) // before the create: no update of the Build is missed
	defer e.watch.forget(name)
	defer e.dropBuild(context.WithoutCancel(ctx), name, src.String()) // the Build lives only as long as the build
	repo := e.Registry + "/canyon/" + req.App
	b := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
		"metadata": map[string]any{"name": name, "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true", "canyon.app": req.App}},
		"spec": map[string]any{
			"tags":               []any{repo + ":" + req.Package + "-" + run},
			"builder":            map[string]any{"image": builder},
			"serviceAccountName": "canyon-build",
			"source":             map[string]any{"registry": map[string]any{"image": src.String()}},
			"cache":              map[string]any{"registry": map[string]any{"tag": repo + ":cache"}},
			"env":                env,
		},
	}}
	if _, err := e.Dynamic.Resource(buildGVR).Namespace(BuildNamespace).Create(ctx, b, metav1.CreateOptions{}); apierrors.IsInvalid(err) || apierrors.IsRequestEntityTooLargeError(err) {
		return executor.BuildResult{}, fmt.Errorf("the cluster refused the build values: %w", err) // the values are the Build's one part that is the user's (#2550)
	} else if err != nil {
		return executor.BuildResult{}, &executor.HostFailed{Message: fmt.Sprintf("build %s: %v", name, err)}
	}
	cur, err := e.await(ctx, own, name, b, updates, out)
	if err != nil {
		return executor.BuildResult{}, err
	}
	switch status, message := succeeded(cur); status {
	case "True":
		image, _, _ := unstructured.NestedString(cur.Object, "status", "latestImage")
		if r, sum, ok := strings.Cut(image, "@sha256:"); !ok || r != repo || !digest.MatchString(sum) {
			return executor.BuildResult{}, &executor.HostFailed{Message: fmt.Sprintf("build %s answered image %q, not %s@sha256:<digest>", name, image, repo)}
		}
		return executor.BuildResult{Image: image, DurationMS: time.Since(start).Milliseconds(), Buildpacks: ran(cur)}, nil
	default:
		if pod, _, _ := unstructured.NestedString(cur.Object, "status", "podName"); pod != "" {
			status := e.detected(ctx, pod)
			if status == 21 && saw.nginxWantsYml(req.BuildValues) { // the app's to change (#242)
				return executor.BuildResult{}, errors.New(executor.NginxWithoutYml)
			}
			if status == 20 || status == 21 {
				return executor.BuildResult{}, &executor.Undetected{Builds: e.builds(ctx, cb, builder), Errored: status == 21}
			}
		}
		return executor.BuildResult{}, e.failed(ctx, cur, message)
	}
}

// await follows the Build name, created as created, through updates until kpack says it succeeded or
// failed, its steps' logs to out (nil: none followed): the Build as it ended. A Build deleted before it
// ended, the caller gone, or own's deadline is an error — the cluster's lack of room when its pod was
// still unscheduled then (#1637).
func (e *Executor) await(ctx, own context.Context, name string, created *unstructured.Unstructured, updates chan *unstructured.Unstructured, out io.Writer) (*unstructured.Unstructured, error) {
	var logs sync.WaitGroup
	defer logs.Wait()
	following := false
	last := created // the Build as last seen: its pod is what a deadline asks about
	for {
		var cur *unstructured.Unstructured
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("build %s did not finish: %w", name, ctx.Err())
		case <-own.Done(): // the executor's deadline, the caller still listening: room is what it says
			if pod, _, _ := unstructured.NestedString(last.Object, "status", "podName"); pod != "" {
				if said := e.unschedulable(ctx, pod); said != "" { // the cluster had no room for it (#1637)
					return nil, &executor.NoRoom{Reason: said}
				}
			}
			return nil, fmt.Errorf("build %s did not finish in %s", name, cmp.Or(e.BuildDeadline, 29*time.Minute))
		case cur = <-updates:
		}
		last = cur
		if cur == nil { // the waiter was closed: the Build is gone
			logs.Wait()
			return nil, &executor.HostFailed{Message: fmt.Sprintf("build %s was deleted before it finished", name)} // not by kpack: the cluster's (#2537)
		}
		if pod, _, _ := unstructured.NestedString(cur.Object, "status", "podName"); pod != "" && !following && out != nil {
			following = true
			logs.Go(func() {
				if err := e.stepLogs(ctx, pod, out); err != nil {
					log.Printf("executor: logs of build pod %s: %v", pod, err)
				}
			})
		}
		if status, _ := succeeded(cur); status == "True" || status == "False" {
			logs.Wait() // every step's log is out before the result
			return cur, nil
		}
	}
}

// failed is why the Build b failed: the step that exited, or kpack's own words, message, when no step can be named.
func (e *Executor) failed(ctx context.Context, b *unstructured.Unstructured, message string) error {
	if pod, _, _ := unstructured.NestedString(b.Object, "status", "podName"); pod != "" {
		if step, code := e.failedStep(ctx, pod); step != "" { // kpack's own message ends on a kubectl hint for operators
			return fmt.Errorf("the %s step exited %d", step, code)
		}
	}
	return fmt.Errorf("build %s failed: %s", b.GetName(), message) // no step to name: kpack's own words, as a pod gone says
}

// ran is the buildpacks a finished Build ran, its status.buildMetadata, by id: the ClusterBuilder's image
// names none (contracts/executor.md, The buildpacks that ran). A group that cannot be read is none, never
// a failed build; the executor's log says why.
func ran(b *unstructured.Unstructured) []executor.Buildpack {
	group, _, err := unstructured.NestedSlice(b.Object, "status", "buildMetadata")
	raw, merr := json.Marshal(group)
	bps, rerr := executor.RanOf(raw, nil)
	if err = errors.Join(err, merr, rerr); err != nil {
		log.Printf("executor: the buildpacks Build %s ran: %v", b.GetName(), err)
	}
	return bps
}

// detected is the exit code of the build pod's detect step: 20 is the CNB platform's "no buildpack
// group passed detection", 21 the same with a buildpack erroring while detecting (contracts/executor.md,
// /build). A pod that cannot be read, or a step that did not end, says nothing: -1.
func (e *Executor) detected(ctx context.Context, pod string) int32 {
	p, err := e.Client.CoreV1().Pods(BuildNamespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		log.Printf("executor: build pod %s: %v", pod, err)
		return -1
	}
	if done := state(p, "detect").Terminated; done != nil {
		return done.ExitCode
	}
	return -1
}

// failedStep is the first lifecycle step of the build pod that exited non-zero, and its exit code; ""
// when the pod cannot be read or none did.
func (e *Executor) failedStep(ctx context.Context, pod string) (string, int32) {
	p, err := e.Client.CoreV1().Pods(BuildNamespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		log.Printf("executor: build pod %s: %v", pod, err)
		return "", 0
	}
	for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		if done := state(p, c.Name).Terminated; done != nil && done.ExitCode != 0 {
			return c.Name, done.ExitCode
		}
	}
	return "", 0
}

// builds is what the ClusterBuilder builds, from its status.order, each buildpack named by the metadata
// label of its image, builder: a failed detect is one whether or not the order can be read, so one that
// cannot names none, and names that cannot be read leave each buildpack its id; the executor's log says why.
func (e *Executor) builds(ctx context.Context, cb *unstructured.Unstructured, builder string) []string {
	names, err := e.names(ctx, builder)
	if err != nil {
		log.Printf("executor: the ClusterBuilder canyon's image %s: %v; its buildpacks are named by id", builder, err)
	}
	order, _, err := unstructured.NestedSlice(cb.Object, "status", "order")
	raw, merr := json.Marshal(order)
	builds, berr := executor.BuildsOf(raw, names)
	if err = errors.Join(err, merr, berr); err != nil {
		log.Printf("executor: the order of the ClusterBuilder canyon: %v", err)
		return []string{}
	}
	return builds
}

// names is each buildpack's name by its id, from the builder image's metadata label: the ClusterBuilder's
// status carries no names. The registry's credential goes to CANYON_KUBE_REGISTRY alone, so an image
// elsewhere is not read.
func (e *Executor) names(ctx context.Context, builder string) (map[string]string, error) {
	r, err := name.ParseReference(builder, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	if r.Context().RegistryStr() != e.Registry {
		return nil, fmt.Errorf("not in %s (CANYON_KUBE_REGISTRY)", e.Registry)
	}
	config, err := e.config(ctx, r)
	if err != nil {
		return nil, err
	}
	return executor.NamesOf([]byte(config.Config.Labels[executor.MetadataLabel]))
}

// config is ref's image config, its manifest read as e.image reads it and the config only once the manifest
// names its size as 1 to executor.MaxImageConfig bytes: go-containerregistry reads as much as a manifest claims,
// and without end at -1.
func (e *Executor) config(ctx context.Context, ref name.Reference) (*v1.ConfigFile, error) {
	img, err := e.image(ctx, ref)
	if err != nil {
		return nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if m.Config.Size <= 0 || m.Config.Size > executor.MaxImageConfig {
		return nil, fmt.Errorf("image %s: its config is not 1 to %d bytes", ref, executor.MaxImageConfig)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	return config, nil
}

// succeeded is the Build's Succeeded condition: True, False, or "" while it runs, and its message.
func succeeded(b *unstructured.Unstructured) (string, string) {
	conditions, _, _ := unstructured.NestedSlice(b.Object, "status", "conditions")
	for _, c := range conditions {
		if m, ok := c.(map[string]any); ok && m["type"] == "Succeeded" {
			status, _ := m["status"].(string)
			message, _ := m["message"].(string)
			return status, message
		}
	}
	return "", ""
}

// stepLogs writes each step container's log in step order — the init containers, then the pod's
// own — each from its start until it ends. A step the pod never started, because an earlier one
// failed, has no log.
func (e *Executor) stepLogs(ctx context.Context, pod string, out io.Writer) error {
	pods := e.Client.CoreV1().Pods(BuildNamespace)
	p, err := e.podOf(ctx, pod, func(*corev1.Pod) bool { return true })
	if err != nil {
		return err
	}
	for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		started := func(p *corev1.Pod) bool {
			return state(p, c.Name).Waiting == nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
		}
		if p, err = e.podOf(ctx, pod, started); err != nil {
			return err
		}
		if state(p, c.Name).Running == nil && state(p, c.Name).Terminated == nil {
			return nil // the pod ended before this step began
		}
		stream, err := pods.GetLogs(pod, &corev1.PodLogOptions{Container: c.Name, Follow: true}).Stream(ctx)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, stream)
		_ = stream.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// podOf polls the build pod until ready says it is, or ctx ends; a pod that is gone — evicted, or
// collected — ends the wait at once.
func (e *Executor) podOf(ctx context.Context, name string, ready func(*corev1.Pod) bool) (*corev1.Pod, error) {
	for {
		p, err := e.Client.CoreV1().Pods(BuildNamespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the build pod %s is gone", name)
		}
		if err == nil && ready(p) {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(e.poll()):
		}
	}
}

// state is container name's state in the pod's status; waiting when the pod does not report it yet.
func state(p *corev1.Pod, name string) corev1.ContainerState {
	for _, s := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if s.Name == name {
			return s.State
		}
	}
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
}

// dropBuild deletes the Build, its pod with it, and its source image, within a minute.
func (e *Executor) dropBuild(ctx context.Context, name, source string) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	background := metav1.DeletePropagationBackground
	if err := e.Dynamic.Resource(buildGVR).Namespace(BuildNamespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("executor: deleting build %s: %v", name, err)
	}
	if err := e.dropImage(ctx, source); source != "" && err != nil {
		log.Printf("executor: deleting the source of build %s: %v", name, err)
	}
}

// SweepBuilds deletes every Build labelled canyon.build, and a build's source image or a rebase's fresh
// tag, that a crash left.
// The binary calls it once, at start, before it serves.
func (e *Executor) SweepBuilds(ctx context.Context) {
	list, err := e.Dynamic.Resource(buildGVR).Namespace(BuildNamespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.build=true"})
	if err != nil {
		log.Printf("executor: build sweep: %v", err)
		return
	}
	for _, b := range list.Items {
		if b.GetAnnotations()[reasonAnnotation] == stackReason { // a rebase's: its source is the image it rebased, never its own
			gone := e.dropRebase(ctx, b.GetName())
			if tag, ok := e.freshTag(&b); ok && gone { // its own fresh tag alone, once its pod is seen gone
				if err := e.dropTag(ctx, tag); err != nil {
					log.Printf("executor: keeping %s: %v", tag, err)
				}
			} else if ok {
				log.Printf("executor: keeping %s: its rebase Build was not seen gone", tag)
			}
			continue
		}
		source, _, _ := unstructured.NestedString(b.Object, "spec", "source", "registry", "image")
		e.dropBuild(ctx, b.GetName(), source)
	}
}

// unschedulable is the scheduler's own message while the build pod could not be scheduled — its
// PodScheduled condition False with reason Unschedulable — "" otherwise, or when the pod cannot be read.
func (e *Executor) unschedulable(ctx context.Context, pod string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := e.Client.CoreV1().Pods(BuildNamespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c.Message
		}
	}
	return ""
}
