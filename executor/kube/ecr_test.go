package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// fakeECR answers as ECR does and keeps what it was asked.
type fakeECR struct {
	mu        sync.Mutex
	calls     []string
	createErr error
	deleteErr error
}

func (f *fakeECR) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.record("token")
	token := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "AWS:password-%d", len(f.calls)))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []types.AuthorizationData{{AuthorizationToken: &token, ExpiresAt: aws.Time(time.Now().Add(12 * time.Hour))}}}, nil
}

func (f *fakeECR) CreateRepository(_ context.Context, in *ecr.CreateRepositoryInput, _ ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	f.record("create " + aws.ToString(in.RepositoryName))
	return &ecr.CreateRepositoryOutput{}, f.createErr
}

func (f *fakeECR) DeleteRepository(_ context.Context, in *ecr.DeleteRepositoryInput, _ ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error) {
	f.record(fmt.Sprintf("delete %s force=%v", aws.ToString(in.RepositoryName), in.Force))
	return &ecr.DeleteRepositoryOutput{}, f.deleteErr
}

// The ECR credential is a token fetched once and again once it has less than 6 hours left.
func TestTheECRCredentialIsATokenRefreshedWithSixHoursLeft(t *testing.T) {
	f := &fakeECR{}
	a := NewECRAuth(f).(*ecrAuth)
	for range 2 {
		cfg, err := a.Authorization()
		if err != nil || cfg.Username != "AWS" || cfg.Password != "password-1" {
			t.Fatalf("%+v %v", cfg, err)
		}
	}
	a.until = time.Now().Add(5 * time.Hour)
	if cfg, err := a.Authorization(); err != nil || cfg.Password != "password-2" || len(f.calls) != 2 {
		t.Fatalf("a token with 5 hours left: %+v %v after %v", cfg, err, f.calls)
	}
}

// canyon-registry, the Build's push credential, is the registry's credential as a docker config,
// deleted and created again: never read.
func TestTheRegistrySecretIsWrittenNeverRead(t *testing.T) {
	c := fake.NewClientset()
	e := &Executor{Client: c, Registry: "123456789012.dkr.ecr.eu-central-1.amazonaws.com", Auth: authn.FromConfig(authn.AuthConfig{Username: "AWS", Password: "token-1"})}
	for range 2 {
		if err := e.WriteRegistrySecret(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range c.Actions() {
		if a.GetVerb() != "create" && a.GetVerb() != "delete" {
			t.Errorf("%s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	s, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), BuildNamespace, "canyon-registry")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths map[string]struct{ Username, Password, Auth string }
	}
	secret := s.(*corev1.Secret)
	_ = json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config)
	got := config.Auths[e.Registry]
	if secret.Type != corev1.SecretTypeDockerConfigJson || got.Username != "AWS" || got.Password != "token-1" || got.Auth != base64.StdEncoding.EncodeToString([]byte("AWS:token-1")) {
		t.Fatalf("canyon-registry %s %+v", secret.Type, config)
	}
}

// An app's first build creates its repository on ECR; a repository already there is no failure.
func TestABuildCreatesItsRepositoryOnECR(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	f := &fakeECR{}
	e.ECR = f
	build := func() error {
		_, err := e.Build(t.Context(), executor.BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree(t)), io.Discard)
		return err
	}
	if err := build(); err != nil {
		t.Fatal(err)
	}
	f.createErr = &types.RepositoryAlreadyExistsException{}
	if err := build(); err != nil {
		t.Fatalf("a repository already there: %v", err)
	}
	if !slices.Equal(f.calls, []string{"create canyon/claims-portal", "create canyon/claims-portal"}) {
		t.Fatalf("asked ECR %v", f.calls)
	}
}

// push puts img in the registry under each tag.
func push(t *testing.T, e *Executor, repo string, img v1.Image, tags ...string) string {
	t.Helper()
	for _, tag := range tags {
		ref, err := name.NewTag(e.Registry + "/" + repo + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := img.Digest()
	return d.String()
}

func image(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func tagsOf(t *testing.T, e *Executor, repo string) []string {
	t.Helper()
	r, _ := name.NewRepository(e.Registry + "/" + repo)
	tags, _ := remote.List(r)
	slices.Sort(tags)
	return tags
}

func runs(t *testing.T, e *Executor, target, image string) {
	t.Helper()
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "claims-portal-" + target, Namespace: Namespace}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: image}}
	if err := e.Client.(*fake.Clientset).Tracker().Add(d); err != nil {
		t.Fatal(err)
	}
}

// After a /run, the images of <registry>/canyon/<app> go but cache, the src- tags and an image a
// Deployment of the app runs — every tag of it — at most 50 per call; another app's are never touched,
// and when the API cannot say what runs, nothing goes.
func TestCollectRemovesWhatNoDeploymentRuns(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	running := push(t, e, "canyon/claims-portal", image(t), "pkg_1-a", "pkg_1-b")
	push(t, e, "canyon/claims-portal", image(t), "pkg_2-c", "pkg_2-d")
	push(t, e, "canyon/claims-portal", image(t), "cache")
	push(t, e, "canyon/claims-portal", image(t), "src-abcd1234")
	push(t, e, "canyon/claims-portal-other", image(t), "pkg_3-e")
	runs(t, e, "prod", e.Registry+"/canyon/claims-portal@"+running)
	c := e.Client.(*fake.Clientset)
	c.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server did not answer")
	})
	if _, err := e.collect(t.Context(), "claims-portal", nil); err == nil || len(tagsOf(t, e, "canyon/claims-portal")) != 6 {
		t.Fatalf("an API that does not answer: %v, left %v", err, tagsOf(t, e, "canyon/claims-portal"))
	}
	c.ReactionChain = c.ReactionChain[1:]
	e.Collect(t.Context(), "claims-portal", nil)
	e.collecting.Wait()
	if got := tagsOf(t, e, "canyon/claims-portal"); !slices.Equal(got, []string{"cache", "pkg_1-a", "pkg_1-b", "src-abcd1234"}) {
		t.Fatalf("left %v", got)
	}
	if got := tagsOf(t, e, "canyon/claims-portal-other"); len(got) != 1 {
		t.Fatalf("another app's repository: %v", got)
	}
	for i := range maxCollect + 2 {
		push(t, e, "canyon/claims-portal", image(t), fmt.Sprintf("pkg_%03d-x", i))
	}
	removed, err := e.collect(t.Context(), "claims-portal", nil)
	if err != nil || len(removed) != maxCollect {
		t.Fatalf("removed %d in one call (%v), want %d", len(removed), err, maxCollect)
	}
}

// /forget is 409 while a Deployment of the app is there; then it deletes every image of the app — on
// ECR, the repository, forced — and again is a success.
func TestForgetDeletesTheRepositoryOnceNothingRuns(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	push(t, e, "canyon/claims-portal", image(t), "pkg_1-a", "cache")
	push(t, e, "canyon/claims-portal-other", image(t), "pkg_2-b")
	runs(t, e, "preview", e.Registry+"/canyon/claims-portal@sha256:00")
	if err := e.Forget(t.Context(), "claims-portal"); !errors.Is(err, executor.ErrInUse) || len(tagsOf(t, e, "canyon/claims-portal")) != 2 {
		t.Fatalf("a Deployment there: %v", err)
	}
	if err := e.Client.AppsV1().Deployments(Namespace).Delete(t.Context(), "claims-portal-preview", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.Forget(t.Context(), "claims-portal"); err != nil {
			t.Fatal(err)
		}
	}
	if got := tagsOf(t, e, "canyon/claims-portal"); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	if got := tagsOf(t, e, "canyon/claims-portal-other"); len(got) != 1 {
		t.Fatalf("another app's repository: %v", got)
	}
	f := &fakeECR{}
	e.ECR = f
	if err := e.Forget(t.Context(), "claims-portal"); err != nil {
		t.Fatal(err)
	}
	f.deleteErr = &types.RepositoryNotFoundException{}
	if err := e.Forget(t.Context(), "claims-portal"); err != nil {
		t.Fatalf("a repository already gone: %v", err)
	}
	if !slices.Equal(f.calls, []string{"delete canyon/claims-portal force=true", "delete canyon/claims-portal force=true"}) {
		t.Fatalf("asked ECR %v", f.calls)
	}
}

// stallECR's first token request never answers until its caller gives up; the next one answers.
type stallECR struct {
	fakeECR
	stalled bool
}

func (f *stallECR) GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, o ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.mu.Lock()
	first := !f.stalled
	f.stalled = true
	f.mu.Unlock()
	if first {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.fakeECR.GetAuthorizationToken(ctx, in, o...)
}

// A token request that never answers holds canyon-registry's upkeep for a minute, not for ever: the
// write is given up, logged, and tried again on the short interval.
func TestTheRegistrySecretUpkeepOutlivesACallThatNeverAnswers(t *testing.T) {
	c := fake.NewClientset()
	e := &Executor{Client: c, Registry: "123456789012.dkr.ecr.eu-central-1.amazonaws.com", Auth: NewECRAuth(&stallECR{})}
	bound, retry := registryWrite, registryRetry
	t.Cleanup(func() { registryWrite, registryRetry = bound, retry })
	registryWrite, registryRetry = 50*time.Millisecond, 10*time.Millisecond
	go e.KeepRegistrySecret(t.Context())
	for range 200 {
		if _, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), BuildNamespace, "canyon-registry"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canyon-registry is still not written after 2 s: the upkeep is stuck on a call that never answers")
}

// Preview and prod running two different images keep both through a Collect.
func TestCollectKeepsWhatEachTargetRuns(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	preview := push(t, e, "canyon/claims-portal", image(t), "pkg_1-a")
	prod := push(t, e, "canyon/claims-portal", image(t), "pkg_2-b")
	push(t, e, "canyon/claims-portal", image(t), "pkg_3-c")
	runs(t, e, "preview", e.Registry+"/canyon/claims-portal@"+preview)
	runs(t, e, "prod", e.Registry+"/canyon/claims-portal@"+prod)
	if removed, err := e.collect(t.Context(), "claims-portal", nil); err != nil || !slices.Equal(removed, []string{"pkg_3-c"}) {
		t.Fatalf("removed %v (%v)", removed, err)
	}
	if got := tagsOf(t, e, "canyon/claims-portal"); !slices.Equal(got, []string{"pkg_1-a", "pkg_2-b"}) {
		t.Fatalf("left %v", got)
	}
}

// keep, through Collect, spares by digest — a digest twice is once, and one naming no image there is no
// error — and a spared tag never counts against the 50.
func TestCollectSparesKept(t *testing.T) {
	e, _, _ := kpackCluster(t, "True", "")
	kept := push(t, e, "canyon/claims-portal", image(t), "pkg_0-a", "pkg_0-b")
	for i := range maxCollect + 1 {
		push(t, e, "canyon/claims-portal", image(t), fmt.Sprintf("pkg_%03d-x", i+1))
	}
	gone := "sha256:" + strings.Repeat("f", 64)
	e.Collect(t.Context(), "claims-portal", []string{kept, gone, kept})
	e.collecting.Wait()
	if got := tagsOf(t, e, "canyon/claims-portal"); !slices.Equal(got, []string{"pkg_0-a", "pkg_0-b", "pkg_051-x"}) {
		t.Fatalf("left %v", got)
	}
}
