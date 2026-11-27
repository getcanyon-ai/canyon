package kube

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/getcanyon-ai/canyon/executor"
	asked "github.com/getcanyon-ai/canyon/internal/answered"
)

func (e *Executor) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return 2 * time.Second
}

func (e *Executor) deadline() time.Duration {
	if e.Deadline > 0 {
		return e.Deadline
	}
	return executor.RunAvailable
}

// imageArch is the architecture image's config names, read from CANYON_KUBE_REGISTRY's canyon/<app> alone (ref,
// before any request) with the executor's credential: an index of several architectures, or a config not for
// linux or naming none, is not read. Only the architecture is compared, never its variant: kpack builds amd64
// and arm64 images.
func (e *Executor) imageArch(ctx context.Context, image string) (string, error) {
	ref, err := e.ref(image)
	if err != nil {
		return "", err
	}
	config, err := e.config(ctx, ref)
	if err != nil {
		return "", err
	}
	if config.OS != "linux" || config.Architecture == "" {
		return "", fmt.Errorf("its config says os %q, architecture %q", config.OS, config.Architecture)
	}
	return config.Architecture, nil
}

// archUnread is a /run's and a GET /platform's answer for an image whose architecture is not read.
const archUnread = "the image %s's architecture could not be read, so Canyon runs it nowhere: %w"

// Platform is the platform image runs on, for a hand-off release (contracts/executor.md, GET /platform): read as a
// /run reads it, so only an image of CANYON_KUBE_REGISTRY's canyon/<app> by digest, refused before any request
// otherwise; ErrNoImage for one the registry does not have.
func (e *Executor) Platform(ctx context.Context, image string) (executor.Platform, error) {
	repo, sum, ok := strings.Cut(image, "@sha256:")
	app, ours := strings.CutPrefix(repo, e.Registry+"/canyon/")
	if !ok || !ours || !validApp(app) || !digest.MatchString(sum) {
		return executor.Platform{}, refuse("image must be %s/canyon/<app>@sha256:<64 hex>, never a tag", e.Registry)
	}
	arch, err := e.imageArch(ctx, image)
	if err != nil {
		return executor.Platform{}, fmt.Errorf(archUnread, image, err)
	}
	return executor.Platform{OS: "linux", Architecture: arch}, nil
}

// errReplaced is a /run whose template another /run has replaced while it waited: the Deployment is
// that /run's now, so this one neither restores nor cleans up.
var errReplaced = errors.New("another /run replaced this one's template while it waited")

// Run writes the app's objects in the contract's order — the NetworkPolicy, the Service, the Secrets,
// the access ConfigMap, canyon-db-ca when the /run carries database_ca, then the Deployment — so no
// pod of the app exists before its policy, and
// answers once the new pod has been available for the Deployment's minReadySeconds. A new pod that
// restarts, or is not available by the deadline, fails the /run naming its reason: the previous
// template and NetworkPolicy are put back and its pod, never stopped, serves on; on a first /run everything
// written goes, but an access ConfigMap that was there before it. With egress, Canyon's egress gateway
// answers first, or nothing is written.
func (e *Executor) Run(ctx context.Context, req executor.RunRequest) (executor.RunResult, error) {
	door, doorJSON, file, err := e.check(req)
	if err != nil {
		return executor.RunResult{}, err
	}
	readArch := e.imageArch
	if e.Arch != nil {
		readArch = e.Arch
	}
	arch, err := readArch(ctx, req.Image) // before anything is written
	if err != nil {
		return executor.RunResult{}, fmt.Errorf(archUnread, req.Image, err)
	}
	var signin []string
	if req.Egress != nil && len(e.SigninDirect) > 0 {
		if signin, err = e.signinHosts(door); err != nil {
			return executor.RunResult{}, err
		}
	}
	proxy, sidecars, err := proxied(req, signin)
	if err != nil {
		return executor.RunResult{}, err
	}
	trust, err := e.trust(req) // before anything is written: system roots with no certificate fail the start
	if err != nil {
		return executor.RunResult{}, err
	}
	if req.Egress != nil {
		if err := e.gateway(ctx, req.Egress.Proxy); err != nil {
			return executor.RunResult{}, err
		}
	}
	name := req.App + "-" + req.Target
	deployments := e.Client.AppsV1().Deployments(Namespace)
	prev, err := deployments.Get(ctx, name, metav1.GetOptions{})
	first := apierrors.IsNotFound(err)
	if err != nil && !first {
		return executor.RunResult{}, fmt.Errorf("deployment %s: %w", name, err)
	}
	run := nonce()
	d := e.deployment(req, door, doorJSON, run, proxy, sidecars)
	d.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": arch} // where its image starts, whatever the executor's architecture
	accessWasThere, policy, err := e.write(ctx, req, door, file, trust)
	deployed := false
	if err == nil && first {
		_, err = deployments.Create(ctx, d, metav1.CreateOptions{})
		deployed = err == nil
	} else if err == nil {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error { // the controller's status writes move the resourceVersion
			cur, err := deployments.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			prev, d.ResourceVersion = cur, cur.ResourceVersion
			_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
			return err
		})
		deployed = err == nil
	}
	if err == nil {
		err = e.rollout(ctx, name, req, run)
	}
	if err != nil && !errors.Is(err, errReplaced) {
		keep := context.WithoutCancel(ctx) // what this call wrote is undone even when its caller has gone
		if first {
			if serr := e.remove(keep, req.App, req.Target, accessWasThere); serr != nil {
				log.Printf("executor: removing %s after a failed first /run: %v", name, serr)
			}
		} else {
			if deployed {
				if rerr := e.restore(keep, name, prev.Spec.Template); rerr != nil {
					log.Printf("executor: putting %s's previous template back: %v", name, rerr)
				}
			}
			if policy != nil { // its way out as it was: a /run that changed egress and failed changes nothing
				if perr := e.putPolicyBack(keep, req, run, *policy); perr != nil {
					log.Printf("executor: putting %s's previous NetworkPolicy back: %v", name, perr)
					err = alsoSays(err, "the app keeps this /run's narrower NetworkPolicy: "+perr.Error())
				}
			}
		}
	}
	if err != nil {
		return executor.RunResult{}, err
	}
	if door.Choice != 2 { // a front door that was choice 2 leaves no client secret behind — only now, as a failed rollout puts back a template whose oauth2-proxy needs it
		if err := e.drop(ctx, "canyon-signin-"+name); err != nil {
			log.Printf("executor: dropping %s's sign-in Secret: %v", name, err)
		}
	}
	gone := e.previousGone(ctx, d, req.App, req.Target, run)
	return executor.RunResult{Container: name, Addr: fmt.Sprintf("%s.%s.svc:%d", name, Namespace, doorPort), Answered: e.answered(ctx, req, door, run),
		PreviousGone: &gone,
		TokenCallers: req.Egress != nil}, nil // with egress its pods run as the env's ServiceAccount, its token in canyon-identity's forwarder (deployment)
}

// answered is what the app of the pod this /run waited on answered its sidecar's one GET /, said masked
// with the request's env and cut to 200 characters (contracts/executor.md, What the app answered); nil when the executor could not
// ask, the reason in its log, never a failed /run.
func (e *Executor) answered(ctx context.Context, req executor.RunRequest, door frontDoor, run string) *asked.Answer {
	a, err := e.ask(ctx, req.App, req.Target, run, door.address(req.App, req.Target))
	if err != nil {
		log.Printf("executor: what %s-%s answered: %v", req.App, req.Target, err)
		return nil
	}
	said := []rune(executor.Mask(a.Said, req.Env)) // masked, then the seam's 200 characters
	a.Host, a.Said = "", string(said[:min(len(said), 200)])
	return &a
}

// ask is the sidecar's answer from the pod of run, kept only when it names host: a pod IP another app's
// pod has taken since is never read as this app's.
func (e *Executor) ask(ctx context.Context, app, target, run, host string) (asked.Answer, error) {
	pods, err := e.pods(ctx, app, target, run)
	if err != nil {
		return asked.Answer{}, err
	}
	i := slices.IndexFunc(pods, func(p corev1.Pod) bool { return p.Status.PodIP != "" && p.DeletionTimestamp == nil })
	if i < 0 {
		return asked.Answer{}, errors.New("no pod of this /run has an address")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(pods[i].Status.PodIP, strconv.Itoa(answeredPort))+"/answered", nil)
	if err != nil {
		return asked.Answer{}, err
	}
	resp, err := (&http.Client{Transport: cmp.Or(e.Ask, asked.Direct), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return asked.Answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var a asked.Answer
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || resp.StatusCode != http.StatusOK {
		return asked.Answer{}, fmt.Errorf("the sidecar answered %d: %v", resp.StatusCode, err)
	}
	if a.Host != host || !a.Valid() {
		return asked.Answer{}, fmt.Errorf("the sidecar answered for %q, not %q, or out of the contract (status %d)", a.Host, host, a.Status)
	}
	return a, nil
}

// write puts every object but the Deployment in place, and answers whether the access ConfigMap was there
// before it and the NetworkPolicy's spec before this /run changed it, nil when it created it or left it
// as it was. Secrets are deleted and created again, never read: the Roles grant create and delete on
// them and nothing else.
func (e *Executor) write(ctx context.Context, req executor.RunRequest, door frontDoor, file access, trust map[string]string) (bool, *networkingv1.NetworkPolicySpec, error) {
	c, name := e.Client, req.App+"-"+req.Target
	np := networkPolicy(req.App, req.Target, e.DBCIDRs, req.Egress != nil, e.addrs())
	var was *networkingv1.NetworkPolicySpec
	if cur, err := c.NetworkingV1().NetworkPolicies(Namespace).Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err = c.NetworkingV1().NetworkPolicies(Namespace).Create(ctx, np, metav1.CreateOptions{})
		if err != nil {
			return false, nil, fmt.Errorf("networkpolicy %s: %w", name, err)
		}
	} else if err != nil {
		return false, nil, fmt.Errorf("networkpolicy %s: %w", name, err)
	} else {
		spec := cur.Spec
		cur.Labels, cur.Spec = np.Labels, np.Spec
		if _, err := c.NetworkingV1().NetworkPolicies(Namespace).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return false, nil, fmt.Errorf("networkpolicy %s: %w", name, err)
		}
		if !equality.Semantic.DeepEqual(spec, np.Spec) {
			was = &spec
		}
	}
	svc := service(req.App, req.Target)
	if cur, err := c.CoreV1().Services(Namespace).Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := c.CoreV1().Services(Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return false, was, fmt.Errorf("service %s: %w", name, err)
		}
	} else if err != nil {
		return false, was, fmt.Errorf("service %s: %w", name, err)
	} else {
		cur.Labels, cur.Spec.Selector, cur.Spec.Ports = svc.Labels, svc.Spec.Selector, svc.Spec.Ports // the rest, the cluster IP among it, is the cluster's
		if _, err := c.CoreV1().Services(Namespace).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return false, was, fmt.Errorf("service %s: %w", name, err)
		}
	}
	if req.Egress != nil { // created alone, never read or changed: the Role grants create and delete on it
		sa := serviceAccount(req.App, req.Target)
		if _, err := c.CoreV1().ServiceAccounts(Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, was, fmt.Errorf("serviceaccount %s: %w", sa.Name, err)
		}
	}
	if err := e.replace(ctx, envSecret(req.App, req.Target, req.Env)); err != nil {
		return false, was, err
	}
	if door.Choice == 2 { // a front door that was choice 2 loses its Secret once the /run is up (Run)
		if err := e.replace(ctx, signinSecret(req.App, req.Target, door.Signin.ClientSecret)); err != nil {
			return false, was, err
		}
	}
	existed, _, err := e.putAccess(ctx, req.App, req.Target, req.Access, file.UpdatedAt, true)
	if err == nil && req.DatabaseCA != nil {
		err = e.putShared(ctx, databaseCAMap(*req.DatabaseCA), nil)
	}
	if err == nil && trust != nil {
		err = e.putEgressCA(ctx, req.Egress.CA, trust)
	}
	return existed, was, err
}

// putShared writes a ConfigMap every app on the host mounts — canyon-db-ca, canyon-egress-ca — when it holds anything
// but m's data or annotations, read again and retried when another /run wrote it meanwhile, unless newer says the one
// there is newer than m (nil: never). No /stop deletes it.
func (e *Executor) putShared(ctx context.Context, m *corev1.ConfigMap, newer func(cur *corev1.ConfigMap) bool) error {
	cms := e.Client.CoreV1().ConfigMaps(Namespace)
	err := retry.OnError(retry.DefaultRetry, func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }, func() error {
		cur, err := cms.Get(ctx, m.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = cms.Create(ctx, m, metav1.CreateOptions{})
		case err != nil:
		case newer != nil && newer(cur):
		case !maps.Equal(cur.Data, m.Data) || len(cur.BinaryData) > 0 || !maps.Equal(cur.Annotations, m.Annotations):
			cur.Data, cur.BinaryData, cur.Annotations = m.Data, nil, m.Annotations
			_, err = cms.Update(ctx, cur, metav1.UpdateOptions{})
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("configmap %s: %w", m.Name, err)
	}
	return nil
}

// The annotations on canyon-egress-ca that order its sets (contracts/executor.md, On Kubernetes): the newest root's
// notBefore, and how many roots the set holds.
const (
	caNewest = "canyon.ca_newest"
	caRoots  = "canyon.ca_roots"
)

// putEgressCA writes canyon-egress-ca with trust, roots' files, unless the set there is newer: its newest root's
// notBefore later, or the same with fewer roots, as a set from after a root's end holds one root less than one read
// before it. A /run that read the roots before a rotation or a root's end so never puts back a replaced set, and never a
// retired root again. One with neither annotation, from before them, is replaced.
func (e *Executor) putEgressCA(ctx context.Context, roots []executor.Root, trust map[string]string) error {
	var newest time.Time
	for _, r := range roots { // checked by executor.CheckCA: each one CA certificate
		block, _ := pem.Decode([]byte(r.PEM))
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("egress.ca %s: %w", r.Name, err)
		}
		if cert.NotBefore.After(newest) {
			newest = cert.NotBefore.UTC()
		}
	}
	at, n := newest.Format(time.RFC3339), len(roots)
	m := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: egressCAMap, Namespace: Namespace, Annotations: map[string]string{caNewest: at, caRoots: strconv.Itoa(n)}}, Data: trust}
	return e.putShared(ctx, m, func(cur *corev1.ConfigMap) bool {
		there, err := time.Parse(time.RFC3339, cur.Annotations[caNewest])
		count, cerr := strconv.Atoi(cur.Annotations[caRoots])
		if err != nil || cerr != nil {
			return false // from before the annotations, or not Canyon's: replaced
		}
		return there.After(newest.Truncate(time.Second)) || there.Equal(newest.Truncate(time.Second)) && count < n
	})
}

// putAccess writes the access ConfigMap unless it already holds a newer updated_at — a share made
// during the build is not undone by the deploy — read again and retried on a conflict. create says
// whether a ConfigMap that is not there is made. It answers whether one was there, and whether a newer
// one was kept.
func (e *Executor) putAccess(ctx context.Context, app, target string, file []byte, updated string, create bool) (existed, kept bool, err error) {
	maps := e.Client.CoreV1().ConfigMaps(Namespace)
	m := accessMap(app, target, file, updated)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := maps.Get(ctx, m.Name, metav1.GetOptions{})
		existed, kept = err == nil, false
		switch {
		case apierrors.IsNotFound(err) && create:
			_, err = maps.Create(ctx, m, metav1.CreateOptions{})
		case err != nil:
		case cur.Annotations["canyon.updated_at"] > updated:
			kept = true
		default:
			cur.Labels, cur.Annotations, cur.Data = m.Labels, m.Annotations, m.Data
			_, err = maps.Update(ctx, cur, metav1.UpdateOptions{})
		}
		return err
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return existed, kept, fmt.Errorf("configmap %s: %w", m.Name, err)
	}
	return existed, kept, nil
}

// replace deletes the Secret and creates it again; the answer to the create is discarded unread.
func (e *Executor) replace(ctx context.Context, s *corev1.Secret) error {
	if err := e.drop(ctx, s.Name); err != nil {
		return err
	}
	if _, err := e.Client.CoreV1().Secrets(Namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("secret %s: %w", s.Name, err)
	}
	return nil
}

func (e *Executor) drop(ctx context.Context, secret string) error {
	if err := e.Client.CoreV1().Secrets(Namespace).Delete(ctx, secret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("secret %s: %w", secret, err)
	}
	return nil
}

// rollout waits until the Deployment runs the new template alone and its pod is available, as kubectl
// rollout status does, failing at once when a container of a new pod restarts.
func (e *Executor) rollout(ctx context.Context, name string, req executor.RunRequest, run string) error {
	deadline := time.Now().Add(e.deadline())
	tick := time.NewTicker(e.poll())
	defer tick.Stop()
	asked := false // whether the registry was asked about the image: once per /run
	for {
		d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("deployment %s: %w", name, err)
		}
		pods, err := e.pods(ctx, req.App, req.Target, run)
		if err != nil {
			return err
		}
		if why, container, said := restarted(pods); why != "" {
			reason := "the app did not stay up: " + why
			if container != "app" { // a sidecar's restart is Canyon's to answer, never the app's (contracts/executor.md, /run)
				if container == "canyon-identity" && said != "" { // quoted: the server logs it as one line, and the pod is gone after
					reason += ", its last words: " + strconv.Quote(said)
				}
				return errors.New(reason)
			}
			// its last words are the kubelet's record of them alone (FallbackToLogsOnError): the stopped
			// container's log can be gone, and the kubelet then answers its read 200 with its own sentence
			return executor.NewExited(reason, whole(said), req.Env)
		}
		if !asked && pullFails(pods) { // the image may be gone: only the registry's 404 says it is (contracts/executor.md, /run)
			asked = true
			gone, err := e.gone(ctx, req.Image)
			if err != nil {
				return err
			}
			if gone {
				return fmt.Errorf("%w: %s", executor.ErrNoImage, req.Image)
			}
		}
		if d.Spec.Template.Labels["canyon.run"] != run {
			return errReplaced
		}
		if rolledOut(d) {
			return nil
		}
		if time.Now().After(deadline) {
			if pod := deaf(pods); pod != "" {
				return e.exited(ctx, executor.NothingOnPort, pod, req.Env)
			}
			return fmt.Errorf("the app was not available within %s: %s", e.deadline(), pending(pods))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func rolledOut(d *appsv1.Deployment) bool {
	s, want := d.Status, int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Generation <= s.ObservedGeneration && s.UpdatedReplicas == want && s.Replicas == s.UpdatedReplicas && s.AvailableReplicas >= s.UpdatedReplicas
}

// exited is the answer for a running app container that answers nothing: its last lines, read now,
// before the undo or the delete takes the pod and them with it.
func (e *Executor) exited(ctx context.Context, reason, pod string, env map[string]string) error {
	tail := int64(executor.ExitedLines)
	stream, err := e.Client.CoreV1().Pods(Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "app", TailLines: &tail}).Stream(ctx)
	var lines executor.Tail
	if err == nil {
		_, err = io.Copy(&lines, stream)
		_ = stream.Close()
	}
	if err != nil {
		return executor.NewExited(reason+"; its last lines could not be read", "", env)
	}
	return executor.NewExited(reason, lines.Lines(), env)
}

// whole is a termination message from a whole line to a whole line, as Tail.Lines keeps a log, so no
// part of a value in a line the kubelet cut escapes NewExited's naming: from the log it keeps the last
// 2 KiB, so its first line can begin cut; from the file the app wrote, the first 4 KiB, so its last can
// end cut. A message at either bound loses that line whole.
func whole(said string) string {
	n := len(said)
	if n >= 4<<10 {
		said = said[:strings.LastIndex(said, "\n")+1]
	}
	if n >= 2<<10 {
		_, said, _ = strings.Cut(said, "\n")
	}
	return said
}

// restarted names the first container of these pods that has restarted, and why it last ended, with
// the container and what it said as it ended: its termination message, the tail of its log under
// FallbackToLogsOnError on a non-zero exit, "" when there is none. The app's is said to its builder,
// who has no use for the pod's name; a sidecar's is Canyon's, and names the pod for the server's log.
func restarted(pods []corev1.Pod) (why, container, said string) {
	for _, p := range pods {
		for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			if cs.RestartCount == 0 {
				continue
			}
			ended := "" // no termination recorded: the restart alone is said
			if t := cs.LastTerminationState.Terminated; t != nil {
				ended, said = fmt.Sprintf(" (%s, exit %d)", t.Reason, t.ExitCode), t.Message
			}
			if cs.Name == "app" {
				return "its container restarted" + ended, cs.Name, said
			}
			return fmt.Sprintf("container %s of pod %s restarted%s", cs.Name, p.Name, ended), cs.Name, said
		}
	}
	return "", "", ""
}

// gone is whether the registry answers 404 for image, asked for at most 10 s; any other answer is not
// proof that it is gone.
func (e *Executor) gone(ctx context.Context, image string) (bool, error) {
	ref, err := e.ref(image)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = remote.Head(ref, e.registry(ctx)...)
	return notFound(err), nil
}

// pullFails is whether the app container of one of these pods waits on a pull the kubelet could not make.
func pullFails(pods []corev1.Pod) bool {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; cs.Name == "app" && w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
				return true
			}
		}
	}
	return false
}

// deaf names the first of these pods whose app container runs and is not ready: nothing answers its
// readiness probe, a TCP connect to port 3000.
func deaf(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "app" && cs.State.Running != nil && !cs.Ready {
				return p.Name
			}
		}
	}
	return ""
}

// pending says why no new pod is available: a container's waiting reason, or the pod's unmet condition.
func pending(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return "no pod was created"
	}
	for _, p := range pods {
		for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			if w := cs.State.Waiting; w != nil {
				return fmt.Sprintf("container %s of pod %s is waiting: %s %s", cs.Name, p.Name, w.Reason, w.Message)
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				return fmt.Sprintf("pod %s is not %s: %s %s", p.Name, c.Type, c.Reason, c.Message)
			}
		}
	}
	return "its pod never became ready"
}

func (e *Executor) pods(ctx context.Context, app, target, run string) ([]corev1.Pod, error) {
	sel := "canyon.app=" + app + ",canyon.target=" + target
	if run != "" {
		sel += ",canyon.run=" + run
	}
	list, err := e.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("pods of %s-%s: %w", app, target, err)
	}
	return list.Items, nil
}

// podsGone is how long a failed /run that floored an open app waits for its rollout — its ReplicaSet and
// pods — to be gone before it puts the open policy back (contracts/executor.md, On Kubernetes).
const podsGone = 60 * time.Second

// putPolicyBack puts the policy a failed /run read back: at once when it is the narrower, the /run having
// opened a floored app; when it is the wider, the /run having floored an open one, only once nothing of the
// failed rollout can run floored under it — the Deployment has seen its put-back template, and every
// ReplicaSet of the /run's run is scaled to nothing, has seen it, and has no pod left, nor has the run any
// pod — waited for podsGone at most. Short of that the narrower policy stays and the error says why (fail
// closed): a pod list alone is empty before the ReplicaSet makes its first pod.
func (e *Executor) putPolicyBack(ctx context.Context, req executor.RunRequest, run string, spec networkingv1.NetworkPolicySpec) error {
	name := req.App + "-" + req.Target
	if req.Egress != nil {
		wait := min(podsGone, e.deadline())
		bounded, cancel := context.WithTimeout(ctx, wait) // the whole wait, each call within it too: an API that never answers keeps the floor
		defer cancel()
		for {
			left, err := e.rolloutLeft(bounded, req, run)
			if bounded.Err() != nil {
				return fmt.Errorf("after %s the failed rollout still has %s", wait, cmp.Or(left, "what the cluster did not say"))
			}
			if err != nil {
				return err
			}
			if left == "" {
				break
			}
			select {
			case <-bounded.Done():
				return fmt.Errorf("after %s the failed rollout still has %s", wait, left)
			case <-time.After(e.poll()):
			}
		}
	}
	return e.restorePolicy(ctx, name, spec)
}

// rolloutLeft says what of run's rollout may still start or hold a pod: "" when nothing.
func (e *Executor) rolloutLeft(ctx context.Context, req executor.RunRequest, run string) (string, error) {
	name := req.App + "-" + req.Target
	d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err != nil:
		return "", fmt.Errorf("deployment %s: %w", name, err)
	case d.Status.ObservedGeneration < d.Generation:
		return "a Deployment that has not seen its put-back template", nil
	}
	sel := "canyon.app=" + req.App + ",canyon.target=" + req.Target + ",canyon.run=" + run
	rsets, err := e.Client.AppsV1().ReplicaSets(Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", fmt.Errorf("replicasets of %s: %w", name, err)
	}
	for _, rs := range rsets.Items {
		if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 || rs.Status.Replicas != 0 || rs.Status.ObservedGeneration < rs.Generation {
			return "a ReplicaSet not scaled to nothing", nil
		}
	}
	pods, err := e.pods(ctx, req.App, req.Target, run)
	if err != nil || len(pods) == 0 {
		return "", err
	}
	return fmt.Sprintf("%d pod(s)", len(pods)), nil
}

// previousGone is /run's previous_gone once its rollout completed (contracts/executor.md, On Kubernetes): true once no
// ReplicaSet of an earlier template is left with a pod and no pod of one remains, terminating ones included, as the
// cluster lists them; false when the bound — the template's termination grace period, Kubernetes' 30 s unless it sets
// one, and 10 s more — passes first, or the cluster could not say by then.
func (e *Executor) previousGone(ctx context.Context, d *appsv1.Deployment, app, target, run string) bool {
	grace := int64(corev1.DefaultTerminationGracePeriodSeconds)
	if g := d.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
		grace = *g
	}
	bound := time.Duration(grace)*time.Second + 10*time.Second
	if e.goneWait > 0 {
		bound = e.goneWait
	}
	until := time.Now().Add(bound)
	for {
		left, err := e.previousLeft(ctx, app, target, run)
		if err == nil && left == "" {
			return true
		}
		if time.Now().After(until) {
			log.Printf("executor: %s-%s's earlier template still holds %s after %s: previous_gone false (%v)", app, target, left, bound, err)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(e.poll()):
		}
	}
}

// previousLeft says what of an earlier template than run's may still hold a pod of app target: "" when nothing.
func (e *Executor) previousLeft(ctx context.Context, app, target, run string) (string, error) {
	sel := metav1.ListOptions{LabelSelector: "canyon.app=" + app + ",canyon.target=" + target + ",canyon.run!=" + run}
	rsets, err := e.Client.AppsV1().ReplicaSets(Namespace).List(ctx, sel)
	if err != nil {
		return "", fmt.Errorf("replicasets of %s-%s: %w", app, target, err)
	}
	for _, rs := range rsets.Items {
		if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 || rs.Status.Replicas != 0 || rs.Status.ObservedGeneration < rs.Generation {
			return "a ReplicaSet not scaled to nothing", nil
		}
	}
	pods, err := e.Client.CoreV1().Pods(Namespace).List(ctx, sel)
	if err != nil {
		return "", fmt.Errorf("pods of %s-%s: %w", app, target, err)
	}
	if n := len(pods.Items); n > 0 {
		return fmt.Sprintf("%d pod(s)", n), nil
	}
	return "", nil
}

// alsoSays is err with more after its reason — an Exited's too, which /run answers on its own.
func alsoSays(err error, more string) error {
	var exited *executor.Exited
	if errors.As(err, &exited) {
		exited.Reason += "; " + more
		return err
	}
	return fmt.Errorf("%w; %s", err, more)
}

// restorePolicy puts the NetworkPolicy's previous spec back.
func (e *Executor) restorePolicy(ctx context.Context, name string, spec networkingv1.NetworkPolicySpec) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		np, err := e.Client.NetworkingV1().NetworkPolicies(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		np.Spec = spec
		_, err = e.Client.NetworkingV1().NetworkPolicies(Namespace).Update(ctx, np, metav1.UpdateOptions{})
		return err
	})
}

// gateway is the gateway check (contracts/executor.md, On Kubernetes): Canyon's egress gateway at proxy,
// http://<host>:3128, answers TCP from the executor's own pod, else the /run fails before it writes anything.
func (e *Executor) gateway(ctx context.Context, proxy string) error {
	u, err := url.Parse(proxy)
	if err != nil {
		return refuse("egress.proxy: %v", err)
	}
	dial := e.Gateway
	if dial == nil {
		dial = func(ctx context.Context, hostport string) error {
			c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", hostport)
			if err == nil {
				err = c.Close()
			}
			return err
		}
	}
	if err := dial(ctx, u.Host); err != nil {
		log.Printf("executor: Canyon's egress gateway at %s: %v", u.Host, err)
		return fmt.Errorf("nothing answers at %s, Canyon's egress gateway", u.Host)
	}
	return nil
}

// restore puts the previous template back, as kubectl rollout undo does.
func (e *Executor) restore(ctx context.Context, name string, tmpl corev1.PodTemplateSpec) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		d.Spec.Template = tmpl
		_, err = e.Client.AppsV1().Deployments(Namespace).Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}

// Stop deletes the Deployment first and, once its pods are gone, the Service, NetworkPolicy, ServiceAccount,
// ConfigMap and Secrets of <app>-<target>, by name. One already gone is a success (invariant 6).
func (e *Executor) Stop(ctx context.Context, app, target string) error {
	if !validApp(app) || !validTarget(target) {
		return refuse("app must be a project id of at most 55 characters, target preview or prod")
	}
	return e.remove(ctx, app, target, false)
}

// remove is Stop's work; keepAccess leaves the access ConfigMap.
func (e *Executor) remove(ctx context.Context, app, target string, keepAccess bool) error {
	name, c := app+"-"+target, e.Client
	foreground := metav1.DeletePropagationForeground
	if err := c.AppsV1().Deployments(Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deployment %s: %w", name, err)
	}
	deadline := time.Now().Add(e.deadline())
	for {
		pods, err := e.pods(ctx, app, target, "")
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d pods of %s are still there after %s", len(pods), name, e.deadline())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.poll()):
		}
	}
	errs := []error{
		c.CoreV1().Services(Namespace).Delete(ctx, name, metav1.DeleteOptions{}),
		c.NetworkingV1().NetworkPolicies(Namespace).Delete(ctx, name, metav1.DeleteOptions{}),
		c.CoreV1().ServiceAccounts(Namespace).Delete(ctx, executor.ServiceAccount(app, target), metav1.DeleteOptions{}),
	}
	if !keepAccess {
		errs = append(errs, c.CoreV1().ConfigMaps(Namespace).Delete(ctx, "canyon-access-"+name, metav1.DeleteOptions{}))
	}
	for _, err := range append(errs, e.drop(ctx, "canyon-env-"+name), e.drop(ctx, "canyon-signin-"+name)) {
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
	}
	return nil
}

// Running answers the image in the template of <app>-<target> while a pod of that template is
// available; ErrNoContainer when there is no such Deployment or no such pod, an error when the cluster
// does not answer.
func (e *Executor) Running(ctx context.Context, app, target string) (string, error) {
	if !validApp(app) || !validTarget(target) {
		return "", refuse("app must be a project id of at most 55 characters, target preview or prod")
	}
	d, err := e.Client.AppsV1().Deployments(Namespace).Get(ctx, app+"-"+target, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: %s-%s", executor.ErrNoContainer, app, target)
	}
	if err != nil {
		return "", fmt.Errorf("deployment %s-%s: %w", app, target, err)
	}
	pods, err := e.pods(ctx, app, target, d.Spec.Template.Labels["canyon.run"])
	if err != nil {
		return "", err
	}
	for _, p := range pods {
		if available(p, time.Duration(d.Spec.MinReadySeconds)*time.Second) {
			for _, c := range d.Spec.Template.Spec.Containers {
				if c.Name == "app" {
					return c.Image, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%w: no pod of %s-%s's template is available", executor.ErrNoContainer, app, target)
}

// RunningOf answers those of containers whose Deployment has an available pod, whatever its
// template: through a redeploy or a promotion the previous pod serves until the new one is available,
// and the app runs all along — unless the newest pod of its current template has restarted, ready or
// not, which is restarting and never running (contracts/executor.md, Restarting; #1722). One list of
// the asked apps' Deployments and one of their pods answers them all: a read that shows many
// environments asks once, never once per app. Asked why, how one that does not run last exited is read
// off the same pods, a restarting one's too (#1597).
func (e *Executor) RunningOf(ctx context.Context, containers []executor.Container, why bool) (executor.Runs, error) {
	if len(containers) == 0 || len(containers) > executor.MaxRunningOf {
		return executor.Runs{}, refuse("ask for 1 to %d containers", executor.MaxRunningOf)
	}
	var apps []string
	for _, c := range containers {
		if !validApp(c.App) || !validTarget(c.Target) {
			return executor.Runs{}, refuse("app must be a project id of at most 55 characters, target preview or prod")
		}
		apps = append(apps, c.App)
	}
	slices.Sort(apps)
	asked := metav1.ListOptions{LabelSelector: "canyon.app in (" + strings.Join(slices.Compact(apps), ",") + ")"}
	deployments, err := e.Client.AppsV1().Deployments(Namespace).List(ctx, asked)
	if err != nil {
		return executor.Runs{}, fmt.Errorf("deployments: %w", err)
	}
	pods, err := e.Client.CoreV1().Pods(Namespace).List(ctx, asked)
	if err != nil {
		return executor.Runs{}, fmt.Errorf("pods: %w", err)
	}
	minReady, template := map[string]time.Duration{}, map[string]string{}
	for _, d := range deployments.Items {
		minReady[d.Name] = time.Duration(d.Spec.MinReadySeconds) * time.Second
		template[d.Name] = d.Spec.Template.Labels["canyon.run"]
	}
	running, restarts := map[string]bool{}, map[string]int{}
	for _, p := range pods.Items {
		name := p.Labels["canyon.app"] + "-" + p.Labels["canyon.target"]
		if ready, ok := minReady[name]; ok && available(p, ready) {
			running[name] = true
		}
	}
	for name, run := range template {
		restarts[name] = restartsOf(pods.Items, name, run)
		if restarts[name] > 0 {
			running[name] = false
		}
	}
	runs := executor.Runs{Running: executor.Asked(containers, running), Restarting: []executor.Restarting{}, Starting: []executor.Container{}, Stopped: []executor.Stopped{}}
	for _, c := range containers {
		name := c.App + "-" + c.Target
		if restarts[name] > 0 && !slices.ContainsFunc(runs.Restarting, func(r executor.Restarting) bool { return r.App == c.App && r.Target == c.Target }) {
			runs.Restarting = append(runs.Restarting, executor.Restarting{App: c.App, Target: c.Target, Restarts: restarts[name]})
		}
		if _, deployed := minReady[name]; deployed && !running[name] && restarts[name] == 0 && startingOf(pods.Items, name, template[name]) {
			if !slices.Contains(runs.Starting, c) { // a /run's pod on its way up: never stopped (#230)
				runs.Starting = append(runs.Starting, c)
			}
			continue
		}
		if _, deployed := minReady[name]; !why || !deployed || running[name] || slices.ContainsFunc(runs.Stopped, func(s executor.Stopped) bool { return s.App == c.App && s.Target == c.Target }) {
			continue
		}
		run := "" // a restarting one's exit is its current template's pod's, whose restarts are counted
		if restarts[name] > 0 {
			run = template[name]
		}
		if t := ended(pods.Items, c, run); t != nil {
			runs.Stopped = append(runs.Stopped, executor.Stopped{App: c.App, Target: c.Target, ExitCode: int(t.ExitCode), Said: executor.LastLine(whole(t.Message))})
		}
	}
	return runs, nil
}

// restartsOf is how often the kubelet started again the app container of the newest pod of the
// Deployment name's current template, run its canyon.run label: 0 with no such pod. A pod being deleted
// is not counted.
func restartsOf(pods []corev1.Pod, name, run string) int {
	newest := newestOf(pods, name, run)
	if newest == nil {
		return 0
	}
	for _, cs := range newest.Status.ContainerStatuses {
		if cs.Name == "app" {
			return int(cs.RestartCount)
		}
	}
	return 0
}

// newestOf is the newest pod of the Deployment name's template run, one being deleted aside; nil with none.
func newestOf(pods []corev1.Pod, name, run string) *corev1.Pod {
	var newest *corev1.Pod
	for i, p := range pods {
		if p.Labels["canyon.app"]+"-"+p.Labels["canyon.target"] != name || p.Labels["canyon.run"] != run || p.DeletionTimestamp != nil ||
			(newest != nil && !p.CreationTimestamp.After(newest.CreationTimestamp.Time)) {
			continue
		}
		newest = &pods[i]
	}
	return newest
}

// startingOf says the newest pod of the Deployment name's template run is on its way up (contracts/executor.md, POST
// /running, Starting): scheduled or waiting to be, its app container not yet made, being made, or up and not yet ready.
// One that ended, one whose image can't be pulled or whose container can't be made, and one no node can take is not.
func startingOf(pods []corev1.Pod, name, run string) bool {
	p := newestOf(pods, name, run)
	if p == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return false
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != "app" {
			continue
		}
		switch {
		case cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil:
			return false
		case cs.State.Waiting != nil:
			return cs.State.Waiting.Reason == "ContainerCreating" || cs.State.Waiting.Reason == "PodInitializing"
		}
		return true // running, and not yet available
	}
	return p.Status.Phase == corev1.PodPending // no app container yet
}

// ended is how the app container of the newest pod of c that has ended last ended — of template run's
// pods alone, unless run is "" — its current terminated state, else its last one; nil when no such pod has.
func ended(pods []corev1.Pod, c executor.Container, run string) *corev1.ContainerStateTerminated {
	var last *corev1.ContainerStateTerminated
	var at time.Time
	for _, p := range pods {
		if p.Labels["canyon.app"] != c.App || p.Labels["canyon.target"] != c.Target || (run != "" && p.Labels["canyon.run"] != run) || (last != nil && !p.CreationTimestamp.After(at)) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if t := cmp.Or(cs.State.Terminated, cs.LastTerminationState.Terminated); cs.Name == "app" && t != nil {
				last, at = t, p.CreationTimestamp.Time
			}
		}
	}
	return last
}

// available is a pod that has been ready for at least minReady, as a Deployment counts one.
func available(p corev1.Pod, minReady time.Duration) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue && time.Since(c.LastTransitionTime.Time) >= minReady
		}
	}
	return false
}

func nonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// trust is canyon-egress-ca's data for a /run carrying egress.ca (executor.TrustFiles): its roots, and the system
// roots of the executor's own image, read now; nil without egress.ca.
func (e *Executor) trust(req executor.RunRequest) (map[string]string, error) {
	if req.Egress == nil || len(req.Egress.CA) == 0 {
		return nil, nil
	}
	system, err := os.ReadFile(cmp.Or(e.Roots, executor.SystemRoots))
	if err != nil {
		return nil, fmt.Errorf("the executor's system roots: %w", err)
	}
	files, err := executor.TrustFiles(req.Egress.CA, system)
	if err != nil {
		return nil, err
	}
	data := map[string]string{}
	for k, v := range files {
		data[k] = string(v)
	}
	return data, nil
}
