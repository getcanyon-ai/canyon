package kube

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getcanyon-ai/canyon/executor"
)

// RunImageID is GET /builder's run_image on Kubernetes: the ClusterBuilder canyon's status.stack.runImage,
// kpack's copy of the ClusterStack's run image, as <repository>@sha256:<hex>.
func (e *Executor) RunImageID(ctx context.Context) (string, error) {
	cb, _, err := e.clusterBuilder(ctx)
	if err != nil {
		return "", err
	}
	run, _, err := runImageOf(cb)
	return run, err
}

// runImageOf is the ClusterBuilder's run image as run_image's form has it, and as kpack names it: kpack
// copies the ClusterStack's run image to <spec.tag>-run-image and names the copy by tag and digest, while
// the rebaser records it by repository and digest alone (contracts/executor.md, POST /rebase). The tagged
// name is the one an image built onto it records.
func runImageOf(cb *unstructured.Unstructured) (run, tagged string, err error) {
	tagged, _, _ = unstructured.NestedString(cb.Object, "status", "stack", "runImage")
	if run, err = byDigest(tagged); err != nil {
		return "", "", fmt.Errorf("the ClusterBuilder canyon's run image: %w", err)
	}
	return run, tagged, nil
}

// byDigest is a reference by digest as <repository>@sha256:<hex>, any tag stripped.
func byDigest(reference string) (string, error) {
	d, err := name.NewDigest(reference, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("%q, not a reference by digest: %w", reference, err)
	}
	return d.Context().Name() + "@" + d.DigestStr(), nil
}

// Rebase is POST /rebase on Kubernetes (contracts/executor.md): kpack's own rebase of the image onto the
// ClusterBuilder's run image, under a fresh tag. It answers both images' facts, read from the registry with
// the executor's credential, and judges none but the rebased image's run image, which must be the plan's;
// what it refuses it refuses before the Build is created. Its own another_run_image is the name check
// kpack's rebaser skips: that runs the lifecycle at platform API 0.9. The fresh tag is written before the
// Build, onto a placeholder, since kpack's rebase opens it first (#2576).
func (e *Executor) Rebase(ctx context.Context, req executor.RebaseRequest) (executor.RebaseResult, error) {
	if !validApp(req.App) || !packageRe.MatchString(req.Package) || req.RunImage == "" || len(req.RunImage) > 512 {
		return executor.RebaseResult{}, refuse("app must be a project id of at most 55 characters, package pkg_ and at least 4 of [a-z0-9], and run_image GET /builder's")
	}
	repo := e.Registry + "/canyon/" + req.App
	if r, sum, ok := strings.Cut(req.Image, "@sha256:"); !ok || r != repo || !digest.MatchString(sum) {
		return executor.RebaseResult{}, refuse("image must be %s@sha256:<64 hex>, never a tag", repo)
	}
	ref, err := e.ref(req.Image)
	if err != nil {
		return executor.RebaseResult{}, refuse("%v", err)
	}
	before, labels, err := e.facts(ctx, ref) // an image not there is 404 before anything else, as on Docker
	if err != nil {
		return executor.RebaseResult{}, err
	}
	cb, builder, err := e.clusterBuilder(ctx)
	if err != nil {
		return executor.RebaseResult{}, err
	}
	run, tagged, err := runImageOf(cb)
	if err != nil {
		return executor.RebaseResult{}, err
	}
	if req.RunImage != run {
		return executor.RebaseResult{}, &executor.Refused{Reason: "run_image_moved", Message: fmt.Sprintf("the run image is now %s, not %s: plan again", run, req.RunImage)}
	}
	if v, set := labels["io.buildpacks.rebasable"]; set && v != "true" { // the lifecycle refuses false alone: any other value fails closed
		return executor.RebaseResult{}, &executor.Refused{Reason: "not_rebasable", Message: fmt.Sprintf("the image's io.buildpacks.rebasable label is %q: Canyon never forces a rebase", v)}
	}
	named, err := executor.RunImageName(labels[executor.LifecycleLabel])
	if err != nil {
		return executor.RebaseResult{}, fmt.Errorf("image %s: %s names no run image: %w", ref, executor.LifecycleLabel, err)
	}
	ours, err := executor.LifecycleName(tagged) // kpack's copy's tagged name, which the image recorded
	if err != nil {
		return executor.RebaseResult{}, fmt.Errorf("run image %s: %w", tagged, err)
	}
	if named != ours {
		return executor.RebaseResult{}, &executor.Refused{Reason: "another_run_image", Message: fmt.Sprintf("the image was built onto %s and the run image is %s: a new run image keeps its name", named, ours)}
	}
	stack, _, _ := unstructured.NestedString(cb.Object, "status", "stack", "id") // kpack holds the ClusterStack's run image to its id
	if stack == "" {
		return executor.RebaseResult{}, errors.New("the ClusterBuilder canyon has no stack id yet") // an unread fact, never another_stack
	}
	if stack != before.StackID {
		return executor.RebaseResult{}, &executor.Refused{Reason: "another_stack", Message: fmt.Sprintf("the image is on stack %q and the run image on %q", before.StackID, stack)}
	}
	start := time.Now()
	fresh := nonce() + nonce()                    // 64 bits: no Build of another call takes the name, nor its fresh tag
	tag := repo + ":" + req.Package + "-" + fresh // a fresh tag, as a build's: the image the request names keeps its own
	t, err := name.NewTag(tag, name.StrictValidation)
	if err != nil {
		return executor.RebaseResult{}, err
	}
	held, placeholder, err := e.placeholder(ctx, ref, t, fresh)
	if err != nil {
		return executor.RebaseResult{}, err
	}
	done, gone := false, false
	defer func() { // every failure from here on removes the fresh tag, whether or not the caller still waits: the rebase may have pushed it
		if !done && !gone { // its pod may still push it: removed now, the tag would come back with nothing to remove it
			log.Printf("executor: keeping %s: its rebase Build was not seen gone", tag)
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if !done { // the Build and its pod are gone: nothing pushes the tag after this
			if err := e.dropTag(cleanup, tag); err != nil {
				log.Printf("executor: keeping %s: %v", tag, err)
			}
		}
		if err := e.dropImage(cleanup, placeholder.String()); err != nil { // kpack wrote over its tag, or it went with the tag
			log.Printf("executor: keeping the placeholder %s: %v", placeholder, err)
		}
	}()
	if err := remote.Write(t, held, e.registry(ctx)...); err != nil { // kpack's rebase opens the fresh tag before anything else (#2576)
		gone = true // no Build was made: what the write may have left goes
		return executor.RebaseResult{}, fmt.Errorf("the fresh tag %s: %w", tag, err)
	}
	rebased, gone, err := e.rebase(ctx, req, fresh, tag, before.StackID, builder)
	if err != nil {
		return executor.RebaseResult{}, err
	}
	after, _, err := e.facts(ctx, rebased)
	if errors.Is(err, executor.ErrNoImage) { // 404 is the request's image's: one gone since kpack pushed it failed the rebase
		err = fmt.Errorf("the rebased image %s is gone from the registry", rebased)
	}
	if err != nil {
		return executor.RebaseResult{}, err
	}
	if after.RunImage != req.RunImage {
		return executor.RebaseResult{}, fmt.Errorf("the rebased image is on run image %s, not %s: the run image moved under the rebase", after.RunImage, req.RunImage)
	}
	done = true
	return executor.RebaseResult{Image: rebased.String(), Before: before, After: after, DurationMS: time.Since(start).Milliseconds()}, nil
}

// placeholder is what the fresh tag t names until kpack writes the rebased image over it, and its digest:
// kpack's rebase opens spec.tags[0] before anything else and fails "could not access previous image" on a
// tag the registry does not answer (kpack cmd/rebase, imgutil's Found; #2576). It is image's manifest with
// the one annotation canyon.rebase, the nonce, its config and layers image's, so its digest is this
// rebase's alone: the fresh tag goes by it as any fresh tag does, and image's own manifest, which a
// registry untags only by deleting it, is never tagged. A tag already there is never written over.
func (e *Executor) placeholder(ctx context.Context, image name.Reference, t name.Tag, fresh string) (v1.Image, name.Digest, error) {
	switch _, err := remote.Head(t, e.registry(ctx)...); {
	case err == nil:
		return nil, name.Digest{}, fmt.Errorf("the fresh tag %s is already there: it is never written over", t)
	case !notFound(err):
		return nil, name.Digest{}, fmt.Errorf("the fresh tag %s: %w", t, err)
	}
	img, err := e.image(ctx, image)
	if err != nil {
		return nil, name.Digest{}, err
	}
	held := mutate.Annotations(img, map[string]string{"canyon.rebase": fresh}).(v1.Image)
	sum, err := held.Digest()
	if err != nil {
		return nil, name.Digest{}, fmt.Errorf("the placeholder of %s: %w", image, err)
	}
	return held, t.Context().Digest(sum.String()), nil
}

// dropTag removes the image tag names from the registry, by its digest, which removes every tag of it: so
// only while no Deployment of the app runs that digest and no other tag of the repository names it. A tag
// never written is nothing to remove; whatever cannot be read keeps it.
func (e *Executor) dropTag(ctx context.Context, tag string) error {
	ref, err := name.NewTag(tag, name.StrictValidation)
	if err != nil {
		return err
	}
	desc, err := remote.Head(ref, e.registry(ctx)...)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	app, _ := strings.CutPrefix(ref.Context().RepositoryStr(), "canyon/")
	for _, target := range []string{"preview", "prod"} {
		d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, app+"-"+target, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil { // what runs is not known: nothing goes
			return err
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if strings.HasSuffix(c.Image, "@"+desc.Digest.String()) {
				return fmt.Errorf("the Deployment %s runs its image", d.Name)
			}
		}
	}
	tags, err := remote.List(ref.Context(), e.registry(ctx)...)
	if err != nil {
		return err
	}
	for _, other := range tags {
		if other == ref.TagStr() {
			continue
		}
		d, err := remote.Head(ref.Context().Tag(other), e.registry(ctx)...)
		if err != nil && !notFound(err) {
			return err
		}
		if err == nil && d.Digest == desc.Digest {
			return fmt.Errorf("its image is %s's too", other)
		}
	}
	return e.dropImage(ctx, ref.Context().Digest(desc.Digest.String()).String())
}

// reasonAnnotation and stackReason are kpack's: a Build so annotated whose spec.lastBuild names the builder's
// stack is rebased, not built (kpack 0.18, Build.rebasable).
const reasonAnnotation, stackReason = "image.kpack.io/reason", "STACK"

// rebase is kpack's own rebase: a Build <app>-<fresh> whose spec.lastBuild names the image on stack and whose
// one reason is STACK, onto the run image req names, pinned by spec.runImage, tagged tag. It lives as a
// /build's Build lives — deleted once the rebase answers, swept when the executor starts — and answers the
// image under tag by digest, which must be the Build's status.latestImage: nothing else of its status is
// read. gone is whether the Build, and so its pod, was seen gone once it answers.
func (e *Executor) rebase(ctx context.Context, req executor.RebaseRequest, fresh, tag, stack, builder string) (rebased name.Digest, gone bool, err error) {
	if e.watch == nil {
		return name.Digest{}, true, errors.New("the executor is not watching Builds")
	}
	own, stop := context.WithTimeout(ctx, cmp.Or(e.BuildDeadline, 29*time.Minute)) // a /build's deadline
	defer stop()
	bname := req.App + "-" + fresh
	updates := e.watch.wait(bname) // before the create: no update of the Build is missed
	defer e.watch.forget(bname)
	b := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kpack.io/v1alpha2", "kind": "Build",
		"metadata": map[string]any{"name": bname, "namespace": BuildNamespace, "labels": map[string]any{"canyon.build": "true", "canyon.app": req.App},
			"annotations": map[string]any{reasonAnnotation: stackReason}},
		"spec": map[string]any{
			"tags":               []any{tag},
			"builder":            map[string]any{"image": builder},
			"serviceAccountName": "canyon-build",
			"source":             map[string]any{"registry": map[string]any{"image": req.Image}}, // kpack asks every Build for a source; its rebase reads none
			"lastBuild":          map[string]any{"image": req.Image, "stackId": stack},
			"runImage":           map[string]any{"image": req.RunImage}, // the plan's, never whatever the builder names by then
		},
	}}
	defer func() { // before the create, whose answer may be lost while the Build is made; never the caller's context: a cancel must not leave its pod pushing
		gone = e.dropRebase(context.WithoutCancel(ctx), bname)
	}()
	if _, err := e.Dynamic.Resource(buildGVR).Namespace(BuildNamespace).Create(ctx, b, metav1.CreateOptions{}); err != nil {
		return name.Digest{}, false, fmt.Errorf("build %s: %w", bname, err)
	}
	cur, err := e.await(ctx, own, bname, b, updates, nil) // a rebase streams no log
	if err != nil {
		return name.Digest{}, false, err
	}
	if status, message := succeeded(cur); status != "True" {
		return name.Digest{}, false, e.failedRebase(ctx, cur, message)
	}
	steps, _, _ := unstructured.NestedStringSlice(cur.Object, "status", "stepsCompleted")
	if !slices.Contains(steps, "rebase") || slices.ContainsFunc(steps, func(s string) bool { return s != "rebase" && s != "completion" }) {
		return name.Digest{}, false, fmt.Errorf("build %s ran %v, not kpack's rebase: it built from its source", bname, steps) // kpack builds a Build it does not take for a rebase
	}
	ref, err := name.NewTag(tag, name.StrictValidation)
	if err != nil {
		return name.Digest{}, false, err
	}
	desc, err := remote.Head(ref, e.registry(ctx)...) // the fresh tag itself, read from the registry
	if err != nil {
		return name.Digest{}, false, fmt.Errorf("the rebased image %s: %w", tag, err)
	}
	rebased = ref.Context().Digest(desc.Digest.String())
	if latest, _, _ := unstructured.NestedString(cur.Object, "status", "latestImage"); latest != rebased.String() {
		return name.Digest{}, false, fmt.Errorf("build %s answered image %q, but %s is %s", bname, latest, tag, rebased)
	}
	return rebased, false, nil
}

// maxSaid bounds the line a failed rebase's step wrote last, in its answer and the executor's log.
const maxSaid = 512

// failedRebase is why the rebase Build b failed, as e.failed says it, then the last line its failed step
// wrote: kpack's rebase writes its error there and nowhere else — its step writes no termination message,
// so the Build's condition holds none — and a rebase streams no log (#2576). The executor's log says it too.
func (e *Executor) failedRebase(ctx context.Context, b *unstructured.Unstructured, message string) error {
	err := e.failed(ctx, b, message)
	if pod, _, _ := unstructured.NestedString(b.Object, "status", "podName"); pod != "" {
		if step, _ := e.failedStep(ctx, pod); step != "" {
			if said := e.lastLine(ctx, pod, step); said != "" {
				err = fmt.Errorf("%w: %s", err, said)
			}
		}
	}
	log.Printf("executor: rebase build %s: %v", b.GetName(), err)
	return err
}

// lastLine is the last line the step of the build pod wrote that is not blank, cut to maxSaid bytes; ""
// when its log cannot be read, the executor's log saying why.
func (e *Executor) lastLine(ctx context.Context, pod, step string) string {
	tail, limit := int64(8), int64(64<<10)
	stream, err := e.Client.CoreV1().Pods(BuildNamespace).GetLogs(pod, &corev1.PodLogOptions{Container: step, TailLines: &tail, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		log.Printf("executor: the %s step's log of build pod %s: %v", step, pod, err)
		return ""
	}
	defer func() { _ = stream.Close() }()
	raw, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		log.Printf("executor: the %s step's log of build pod %s: %v", step, pod, err)
	}
	for _, line := range slices.Backward(strings.Split(string(raw), "\n")) {
		if line = strings.TrimSpace(line); len(line) > maxSaid {
			return strings.ToValidUTF8(line[:maxSaid], "") + "…"
		} else if line != "" {
			return line
		}
	}
	return ""
}

// freshTag is the fresh tag a rebase Build b was created with — its one spec.tags entry, which must be
// <registry>/canyon/<app>:<package>-<nonce> for the app its canyon.app label names and the nonce its name
// <app>-<nonce> carries — and false for any Build that is not so: the sweep removes nothing it did not
// create.
func (e *Executor) freshTag(b *unstructured.Unstructured) (string, bool) {
	app := b.GetLabels()["canyon.app"]
	fresh, ok := strings.CutPrefix(b.GetName(), app+"-")
	tags, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "tags")
	if !ok || !nonceRe.MatchString(fresh) || len(tags) != 1 {
		return "", false
	}
	pkg, ok := strings.CutPrefix(tags[0], e.Registry+"/canyon/"+app+":")
	if pkg, cut := strings.CutSuffix(pkg, "-"+fresh); !ok || !cut || !packageRe.MatchString(pkg) {
		return "", false
	}
	return tags[0], true
}

// nonceRe is a rebase's nonce: 16 lowercase hex.
var nonceRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// rebaseGone bounds how long dropRebase waits for a Build to go; a var so a test can shorten it.
var rebaseGone = 2 * time.Minute

// dropRebase deletes the rebase Build name in the foreground — one never made, or gone already, is nothing
// to delete — and waits, within rebaseGone, until it is gone, and so its pod: whether that was seen. A pod
// still running could push the fresh tag after its removal.
func (e *Executor) dropRebase(ctx context.Context, bname string) bool {
	ctx, cancel := context.WithTimeout(ctx, rebaseGone)
	defer cancel()
	builds := e.Dynamic.Resource(buildGVR).Namespace(BuildNamespace)
	foreground := metav1.DeletePropagationForeground
	if err := builds.Delete(ctx, bname, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("executor: deleting rebase build %s: %v", bname, err)
		return false
	}
	for {
		list, err := builds.List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + bname})
		if err == nil && !slices.ContainsFunc(list.Items, func(b unstructured.Unstructured) bool { return b.GetName() == bname }) {
			return true
		}
		select {
		case <-ctx.Done():
			log.Printf("executor: rebase build %s outlived its deletion: %v", bname, cmp.Or(err, ctx.Err()))
			return false
		case <-time.After(e.poll()):
		}
	}
}

// facts reads an image's executor.RebaseFacts, and its labels, from the registry: any fact missing, or out
// of the shape executor.schema.json gives it, is an error; runImage.reference must be a reference by
// digest, answered as run_image's form has it (byDigest).
func (e *Executor) facts(ctx context.Context, ref name.Reference) (executor.RebaseFacts, map[string]string, error) {
	img, err := e.image(ctx, ref)
	if errors.Is(err, errImageOver) {
		err = fmt.Errorf("image %s: its manifest is past %d bytes", ref, executor.MaxImageConfig)
	}
	if err != nil {
		return executor.RebaseFacts{}, nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return executor.RebaseFacts{}, nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if m.Config.Size <= 0 || m.Config.Size > executor.MaxImageConfig {
		return executor.RebaseFacts{}, nil, fmt.Errorf("image %s: its config is not 1 to %d bytes", ref, executor.MaxImageConfig)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return executor.RebaseFacts{}, nil, fmt.Errorf("image %s: %w", ref, err)
	}
	c, layers := config.Config, []string{}
	for _, d := range config.RootFS.DiffIDs {
		layers = append(layers, d.String())
	}
	f, err := executor.Facts(ref.String(), c.Labels, map[string]any{"Entrypoint": c.Entrypoint, "Cmd": c.Cmd, "Env": c.Env, "WorkingDir": c.WorkingDir, "User": c.User}, layers, byDigest)
	return f, c.Labels, err
}
