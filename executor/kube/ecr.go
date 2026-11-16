package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// ECR is what the executor asks of Amazon ECR's API, which the SDK's *ecr.Client answers; everything
// else goes through the registry API, which ECR serves too.
type ECR interface {
	GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
	CreateRepository(context.Context, *ecr.CreateRepositoryInput, ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
	DeleteRepository(context.Context, *ecr.DeleteRepositoryInput, ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error)
}

// ecrAuth is the registry's credential on ECR, an authorization token fetched again once it has less
// than 6 hours left: a token lasts 12.
type ecrAuth struct {
	api   ECR
	mu    sync.Mutex
	cfg   *authn.AuthConfig
	until time.Time
}

// NewECRAuth is the registry credential for an executor whose registry is ECR.
func NewECRAuth(api ECR) authn.Authenticator { return &ecrAuth{api: api} }

func (a *ecrAuth) Authorization() (*authn.AuthConfig, error) {
	return a.AuthorizationContext(context.Background())
}

func (a *ecrAuth) AuthorizationContext(ctx context.Context) (*authn.AuthConfig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg != nil && time.Until(a.until) > 6*time.Hour {
		return a.cfg, nil
	}
	out, err := a.api.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("ecr token: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].ExpiresAt == nil {
		return nil, errors.New("ecr token: no authorization data")
	}
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	user, pass, ok := strings.Cut(string(raw), ":")
	if err != nil || !ok {
		return nil, errors.New("ecr token: not user:password")
	}
	a.cfg, a.until = &authn.AuthConfig{Username: user, Password: pass}, *out.AuthorizationData[0].ExpiresAt
	return a.cfg, nil
}

// ensureRepo creates the app's repository canyon/<app> at its first build; ECR takes no push to a
// repository that is not there. A registry that is not ECR creates it on the push.
func (e *Executor) ensureRepo(ctx context.Context, app string) error {
	if e.ECR == nil {
		return nil
	}
	_, err := e.ECR.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String("canyon/" + app)})
	if exists := new(types.RepositoryAlreadyExistsException); err != nil && !errors.As(err, &exists) {
		return fmt.Errorf("repository canyon/%s: %w", app, err)
	}
	return nil
}

// WriteRegistrySecret writes the docker-config Secret canyon-registry, which the Build's service
// account pushes with, afresh from the registry's credential: deleted and created again, never read.
func (e *Executor) WriteRegistrySecret(ctx context.Context) error {
	cfg, err := authn.Authorization(ctx, e.Auth)
	if err != nil {
		return err
	}
	config, err := json.Marshal(map[string]any{"auths": map[string]any{e.Registry: map[string]string{
		"username": cfg.Username, "password": cfg.Password, "auth": base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password)),
	}}})
	if err != nil {
		return err
	}
	secrets := e.Client.CoreV1().Secrets(BuildNamespace)
	if err := secrets.Delete(ctx, "canyon-registry", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("secret canyon-registry: %w", err)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "canyon-registry", Namespace: BuildNamespace}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: config}}
	if _, err := secrets.Create(ctx, s, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("secret canyon-registry: %w", err)
	}
	return nil
}

// registryWrite bounds one write of canyon-registry, registryRetry is the wait after one fails; vars so a
// test can shorten them.
var registryWrite, registryRetry = time.Minute, time.Minute

// KeepRegistrySecret writes canyon-registry now and every 6 hours until ctx ends: an ECR token lasts
// 12. Each write has a minute: one that fails, or a call that never answers, is logged and tried again
// in a minute, so the upkeep never parks and a build never meets an expired token.
func (e *Executor) KeepRegistrySecret(ctx context.Context) {
	for {
		wait := 6 * time.Hour
		write, cancel := context.WithTimeout(ctx, registryWrite)
		err := e.WriteRegistrySecret(write)
		cancel()
		if err != nil {
			log.Printf("executor: registry secret: %v", err)
			wait = registryRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// maxCollect caps the tags one pass after a /run removes; the rest wait for the next deploy.
const maxCollect = 50

// Collect removes, off the caller, for at most 5 minutes, the images of <registry>/canyon/<app> but
// cache, the src- tags, an image a Deployment of the app runs and one whose digest keep names, at most
// 50 per call; it is logged, never answered.
func (e *Executor) Collect(ctx context.Context, app string, keep []string) {
	e.collecting.Go(func() {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		removed, err := e.collect(bounded, app, keep)
		log.Printf("executor: images of %s: removed %v (%v)", app, removed, err)
	})
}

func (e *Executor) collect(ctx context.Context, app string, keep []string) ([]string, error) {
	spared := map[string]bool{}
	for _, d := range keep {
		spared[d] = true
	}
	for _, target := range []string{"preview", "prod"} {
		d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, app+"-"+target, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil { // what runs is not known: nothing goes
			return nil, err
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if _, sum, ok := strings.Cut(c.Image, "@"); ok {
				spared[sum] = true
			}
		}
	}
	tags, repo, err := e.tags(ctx, app)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, tag := range tags {
		if tag == "cache" || strings.HasPrefix(tag, "src-") || len(removed) == maxCollect {
			continue
		}
		desc, err := remote.Head(repo.Tag(tag), e.registry(ctx)...)
		if notFound(err) { // went with another tag of its image
			continue
		}
		if err != nil {
			return removed, err
		}
		if spared[desc.Digest.String()] {
			continue
		}
		if err := e.dropImage(ctx, repo.Digest(desc.Digest.String()).String()); err != nil {
			return removed, err
		}
		removed = append(removed, tag)
	}
	return removed, nil
}

// tags are the repository canyon/<app>'s, sorted; none when it is not there.
func (e *Executor) tags(ctx context.Context, app string) ([]string, name.Repository, error) {
	repo, err := name.NewRepository(e.Registry+"/canyon/"+app, name.StrictValidation)
	if err != nil {
		return nil, repo, err
	}
	tags, err := remote.List(repo, e.registry(ctx)...)
	if notFound(err) {
		return nil, repo, nil
	}
	slices.Sort(tags)
	return tags, repo, err
}

// Forget removes every image of <registry>/canyon/<app>, the cache among them — on ECR, the
// repository — once no Deployment of the app is left (ErrInUse, 409, while one is). What is already
// gone is a success.
func (e *Executor) Forget(ctx context.Context, app string) error {
	if !validApp(app) {
		return refuse("app must be a project id of at most 55 characters")
	}
	for _, target := range []string{"preview", "prod"} {
		_, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, app+"-"+target, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("%w: the Deployment %s-%s is there: stop it first", executor.ErrInUse, app, target)
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	if e.ECR != nil {
		_, err := e.ECR.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{RepositoryName: aws.String("canyon/" + app), Force: true})
		if gone := new(types.RepositoryNotFoundException); err != nil && !errors.As(err, &gone) {
			return fmt.Errorf("repository canyon/%s: %w", app, err)
		}
		return nil
	}
	tags, repo, err := e.tags(ctx, app)
	for _, tag := range tags {
		desc, herr := remote.Head(repo.Tag(tag), e.registry(ctx)...)
		if herr != nil && !notFound(herr) {
			return herr
		}
		if herr == nil {
			if err := e.dropImage(ctx, repo.Digest(desc.Digest.String()).String()); err != nil {
				return err
			}
		}
	}
	return err
}
