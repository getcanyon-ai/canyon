package kube

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
	asked "github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// cluster is a fake API whose Deployments roll out at once when healthy, their new pod ready for
// minReadySeconds; otherwise each new template's pod is the one pod() makes of it.
func cluster(t *testing.T, e *Executor, pod func(run string) *corev1.Pod) *fake.Clientset {
	t.Helper()
	c := fake.NewClientset()
	c.PrependReactor("*", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		w, ok := a.(k8stesting.CreateAction) // an update is a create action too
		if !ok || (a.GetVerb() != "create" && a.GetVerb() != "update") {
			return false, nil, nil
		}
		d := w.GetObject().(*appsv1.Deployment)
		run := d.Spec.Template.Labels["canyon.run"]
		p := pod(run)
		if p == nil || p.Status.PodIP != "" { // a pod with an address is a healthy one the test placed
			p = cmp.Or(p, readyPod(d.Name, run))
			d.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
		}
		p.Labels = d.Spec.Template.Labels
		if err := c.Tracker().Add(p); err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Error(err)
		}
		return false, nil, nil
	})
	c.PrependReactor("delete", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pods := corev1.SchemeGroupVersion.WithResource("pods")
		list, err := c.Tracker().List(pods, corev1.SchemeGroupVersion.WithKind("Pod"), Namespace)
		if err != nil {
			return true, nil, err
		}
		for _, p := range list.(*corev1.PodList).Items { // its pods go with it, as the garbage collector deletes them
			if p.Labels["canyon.app"]+"-"+p.Labels["canyon.target"] == a.(k8stesting.DeleteAction).GetName() {
				if err := c.Tracker().Delete(pods, Namespace, p.Name); err != nil {
					return true, nil, err
				}
			}
		}
		return false, nil, nil
	})
	e.Client, e.Poll, e.Deadline, e.goneWait = c, time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	return c
}

func readyPod(name, run string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-" + run, Namespace: Namespace},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}},
	}
}

func healthy(string) *corev1.Pod { return nil }

// writes is the resources this client created, updated or deleted, in order, as verb/resource.
func writes(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if v := a.GetVerb(); v != "get" && v != "list" {
			out = append(out, v+"/"+a.GetResource().Resource)
		}
	}
	return out
}

// /run writes the objects in the contract's order, the policy first and the Deployment last, and
// answers the Service's address once the new pod is available.
func TestRunWritesInOrderAndAnswersOnceAvailable(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	res, err := e.Run(t.Context(), req)
	if err != nil || res.Container != "claims-portal-preview" || res.Addr != "claims-portal-preview.canyon-apps.svc:4180" {
		t.Fatalf("%+v %v", res, err)
	}
	want := []string{"create/networkpolicies", "create/services", "delete/secrets", "create/secrets", "delete/secrets", "create/secrets", "create/configmaps", "create/configmaps", "create/deployments"}
	if got := writes(c); !slices.Equal(got, want) {
		t.Fatalf("writes %v\nwant   %v", got, want)
	}
}

// database_ca is one ConfigMap, canyon-db-ca, every app shares (contracts/executor.md): a /run writes it
// only when it holds anything else, and neither a /stop nor another app's /run takes it away.
func TestTheDatabaseCAIsOneConfigMapEveryAppShares(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	written := func() (n int) {
		for _, a := range c.Actions() {
			if w, ok := a.(k8stesting.CreateAction); ok && (a.GetVerb() == "create" || a.GetVerb() == "update") && w.GetObject().(metav1.Object).GetName() == "canyon-db-ca" {
				n++
			}
		}
		return n
	}
	other := req
	other.App, other.Network = "expenses", "canyon-app-expenses"
	other.Image = strings.Replace(req.Image, "/canyon/claims-portal@", "/canyon/expenses@", 1)
	other.Identity.Audience = "expenses"
	for i, step := range []struct {
		req  executor.RunRequest
		want int
	}{{req, 1}, {req, 1}, {other, 1}} {
		if _, err := e.Run(t.Context(), step.req); err != nil {
			t.Fatal(err)
		}
		if n := written(); n != step.want {
			t.Fatalf("/run %d: canyon-db-ca written %d time(s), want %d", i, n, step.want)
		}
	}
	renewed := req
	renewed.DatabaseCA = new(*req.DatabaseCA + *req.DatabaseCA) // the host's bundle changed: two certificates now
	if _, err := e.Run(t.Context(), renewed); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(t.Context(), req.App, req.Target); err != nil {
		t.Fatal(err)
	}
	m, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-db-ca", metav1.GetOptions{})
	if err != nil || written() != 2 || m.Data["ca.pem"] != *renewed.DatabaseCA || len(m.Data) != 1 {
		t.Fatalf("canyon-db-ca after a changed bundle and a /stop: %v, %d write(s), %v", err, written(), m)
	}
}

// Invariant 2 on Kubernetes: the executor writes Secrets and reads none back — across a first /run, a
// redeploy and a /stop, every call on secrets is a create or a delete.
func TestTheExecutorReadsNoSecret(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	for range 2 {
		if _, err := e.Run(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Stop(t.Context(), req.App, req.Target); err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "secrets" && a.GetVerb() != "create" && a.GetVerb() != "delete" {
			t.Errorf("%s secrets", a.GetVerb())
		}
	}
}

// A redeploy whose new pod restarts fails naming why, and the previous template is put back; the
// previous pod was never stopped.
func TestARedeployWhosePodRestartsPutsThePreviousTemplateBack(t *testing.T) {
	e, req, _ := fixture(t)
	crash := false
	cluster(t, e, func(run string) *corev1.Pod {
		if !crash {
			return nil
		}
		p := readyPod("claims-portal-preview", run)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
		return p
	})
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	before, _ := e.Client.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	crash, req.Image = true, strings.Replace(req.Image, "4f1c", "0000", 1)
	_, err := e.Run(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "the app did not stay up: its container restarted (Error, exit 1)") || strings.Contains(err.Error(), "pod") {
		t.Fatalf("a restarting app, said without the pod's name, which is the cluster's and no use to the app's builder: %v", err)
	}
	after, _ := e.Client.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if after.Spec.Template.Labels["canyon.run"] != before.Spec.Template.Labels["canyon.run"] || after.Spec.Template.Spec.Containers[0].Image == req.Image {
		t.Fatalf("the template is %v %s, want the previous one back", after.Spec.Template.Labels, after.Spec.Template.Spec.Containers[0].Image)
	}
}

// Switching the front door from choice 2 to 1 drops the sign-in Secret only once the new pod is up: a
// rollout that fails puts back the choice-2 template, whose oauth2-proxy needs it.
func TestTheSigninSecretGoesOnlyOnceChoiceOneIsUp(t *testing.T) {
	e, req, _ := fixture(t)
	crash := false
	c := cluster(t, e, func(run string) *corev1.Pod {
		if !crash {
			return nil
		}
		p := readyPod("claims-portal-preview", run)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1}}
		return p
	})
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	secret := func() error {
		_, err := c.CoreV1().Secrets(Namespace).Get(t.Context(), "canyon-signin-claims-portal-preview", metav1.GetOptions{})
		return err
	}
	crash, req.FrontDoor = true, []byte(oidcDoor)
	if _, err := e.Run(t.Context(), req); err == nil || secret() != nil {
		t.Fatalf("a switch to choice 1 that fails: %v; the sign-in Secret: %v", err, secret())
	}
	crash = false
	if _, err := e.Run(t.Context(), req); err != nil || !apierrors.IsNotFound(secret()) {
		t.Fatalf("choice 1 up: %v; the sign-in Secret: %v", err, secret())
	}
}

// A first /run whose pod never becomes available fails naming what it waits on, and deletes what it
// wrote, the Deployment first.
func TestAFirstRunThatFailsLeavesNothing(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, pullFailing)
	inRegistry(t, e, &req, true) // there, yet not pulled: the pull failed for another reason, and the deadline answers
	if _, err := e.Run(t.Context(), req); err == nil || errors.Is(err, executor.ErrNoImage) || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("an image that never pulls: %v", err)
	}
	var deletes []string
	for _, w := range writes(c) {
		if strings.HasPrefix(w, "delete/") {
			deletes = append(deletes, w)
		}
	}
	stop := []string{"delete/deployments", "delete/services", "delete/networkpolicies", "delete/serviceaccounts", "delete/configmaps", "delete/secrets", "delete/secrets"}
	if len(deletes) < len(stop) || !slices.Equal(deletes[len(deletes)-len(stop):], stop) {
		t.Fatalf("deletes %v: the Deployment goes first, then the rest", deletes)
	}
	for _, get := range []func() error{
		func() error {
			_, err := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := c.CoreV1().Services(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := c.NetworkingV1().NetworkPolicies(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := c.CoreV1().Secrets(Namespace).Get(t.Context(), "canyon-env-claims-portal-preview", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := c.CoreV1().Secrets(Namespace).Get(t.Context(), "canyon-signin-claims-portal-preview", metav1.GetOptions{})
			return err
		},
	} {
		if err := get(); err == nil {
			t.Error("an object of the failed first /run is still there")
		}
	}
}

// A share made during the build is not undone by the deploy: a ConfigMap holding a newer updated_at is
// kept as it is.
func TestRunKeepsANewerAccessFile(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	newer := accessMap(req.App, req.Target, []byte(`{"newer":true}`), "2026-09-25T10:00:01Z")
	if _, err := c.CoreV1().ConfigMaps(Namespace).Create(t.Context(), newer, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	m, _ := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), newer.Name, metav1.GetOptions{})
	if m.Data["access.json"] != `{"newer":true}` {
		t.Fatalf("the newer access file was overwritten: %v", m.Data)
	}
}

// /stop of what is not there is a success (invariant 6); /running is 404 without a Deployment or an
// available pod of its template, the template's image while one is, and an error, never a 404, when
// the cluster does not answer.
func TestStopAndRunning(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if err := e.Stop(t.Context(), req.App, req.Target); err != nil {
		t.Fatalf("stopping nothing: %v", err)
	}
	if _, err := e.Running(t.Context(), req.App, req.Target); !errors.Is(err, executor.ErrNoContainer) {
		t.Fatalf("no Deployment: %v", err)
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if image, err := e.Running(t.Context(), req.App, req.Target); err != nil || image != req.Image {
		t.Fatalf("a running app: %q %v", image, err)
	}
	c.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server did not answer")
	})
	if _, err := e.Running(t.Context(), req.App, req.Target); err == nil || errors.Is(err, executor.ErrNoContainer) {
		t.Fatalf("an API that does not answer: %v", err)
	}
}

// POST /running answers from one list of the asked apps' Deployments and one of their pods: a pod of
// the current template whose app container has restarted is restarting and never running, ready or not
// — the test cluster's case-dies-later, up 40 s in each cycle of its backoff (#1722); a pod of an older template
// that is still available serves through a rollout, so it runs while the new pod has not restarted; and
// an API that does not answer is an error, never an answer naming fewer (#1241).
func TestRunningOfListsOnceAndCountsAnyAvailablePod(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	asked := []executor.Container{{App: req.App, Target: "prod"}, {App: req.App, Target: req.Target}}
	before := len(c.Actions())
	if got, err := e.RunningOf(t.Context(), asked, true); err != nil || !slices.Equal(got.Running, asked[1:]) || len(got.Stopped) != 0 {
		t.Fatalf("running of %v: %v %v, want %v and no why: prod has no Deployment, preview runs", asked, got, err, asked[1:])
	}
	for i, a := range c.Actions()[before:] {
		l, ok := a.(k8stesting.ListAction)
		if !ok || i > 1 || l.GetListRestrictions().Labels.String() != "canyon.app in (claims-portal)" {
			t.Fatalf("the cluster was asked %v; one list of the asked apps' Deployments and one of their pods answers every one asked", c.Actions()[before:])
		}
	}
	pods, err := c.CoreV1().Pods(Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods %v %v", pods, err)
	}
	crashing := pods.Items[0]
	crashing.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}}
	crashing.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "starting\nError: connect failed (pw-7f3a)\n\n"}}}}
	if _, err := c.CoreV1().Pods(Namespace).UpdateStatus(t.Context(), &crashing, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// an app container that ended and was not started again — a pod the kubelet has not restarted yet —
	// is stopped, and says why: the last non-empty line of its termination message, raw (#1597)
	ended := crashing
	ended.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "starting\nError: connect failed (pw-7f3a)\n\n"}}}}
	if _, err := c.CoreV1().Pods(Namespace).UpdateStatus(t.Context(), &ended, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	why := []executor.Stopped{{App: req.App, Target: req.Target, ExitCode: 1, Said: "Error: connect failed (pw-7f3a)"}}
	if got, err := e.RunningOf(t.Context(), asked, true); err != nil || len(got.Running)+len(got.Restarting) != 0 || !slices.Equal(got.Stopped, why) {
		t.Fatalf("an ended app container not started again: %+v %v, want %v alone", got, err, why)
	}
	if _, err := c.CoreV1().Pods(Namespace).UpdateStatus(t.Context(), &crashing, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	restarting := []executor.Restarting{{App: req.App, Target: req.Target, Restarts: 3}}
	before = len(c.Actions())
	// asked why, a restarting one says how it last exited, from the same pod's lastState: it restarts for
	// ever here, so it never reads stopped (#1597)
	for asking, said := range map[bool][]executor.Stopped{true: why, false: nil} {
		if got, err := e.RunningOf(t.Context(), asked, asking); err != nil || len(got.Running) != 0 || !slices.Equal(got.Stopped, said) || !slices.Equal(got.Restarting, restarting) {
			t.Fatalf("a pod in CrashLoopBackOff, why %v: %+v %v, want %v restarting and %v", asking, got, err, restarting, said)
		}
	}
	if len(c.Actions()) != before+4 {
		t.Fatalf("%d calls for two reads; each is the same two lists", len(c.Actions())-before)
	}
	up := crashing // the same pod in one of its up-windows: ready long enough to count as available
	up.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}
	up.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if _, err := c.CoreV1().Pods(Namespace).UpdateStatus(t.Context(), &up, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.RunningOf(t.Context(), asked, true); err != nil || len(got.Running) != 0 || !slices.Equal(got.Stopped, why) || !slices.Equal(got.Restarting, restarting) {
		t.Fatalf("a restarted pod in one of its up-windows: %+v %v, want %v restarting and its last exit, never running", got, err, restarting)
	}
	previous := readyPod("claims-portal-preview", "older")
	previous.Labels = map[string]string{"canyon.app": req.App, "canyon.target": req.Target, "canyon.run": "older"}
	previous.CreationTimestamp = metav1.NewTime(crashing.CreationTimestamp.Add(time.Hour)) // newer, and it ended too: never the restarting one's exit
	previous.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Message: "the older template's exit"}}}}
	if err := c.Tracker().Add(previous); err != nil {
		t.Fatal(err)
	}
	if got, err := e.RunningOf(t.Context(), asked, true); err != nil || len(got.Running) != 0 || !slices.Equal(got.Stopped, why) || !slices.Equal(got.Restarting, restarting) {
		t.Fatalf("the previous template's pod serving while the new one restarts: %+v %v, want %v restarting and its last exit", got, err, restarting)
	}
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), Namespace, crashing.Name); err != nil {
		t.Fatal(err)
	}
	if got, err := e.RunningOf(t.Context(), asked, true); err != nil || !slices.Equal(got.Running, asked[1:]) || len(got.Stopped)+len(got.Restarting) != 0 {
		t.Fatalf("the previous template's pod still serving a rollout: %+v %v, want %v", got, err, asked[1:])
	}
	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server did not answer")
	})
	if got, err := e.RunningOf(t.Context(), asked, true); err == nil {
		t.Fatalf("an API that does not answer: %v, want an error", got)
	}
}

// A first /run whose app container restarts answers the pod's reason with what the status kept of its
// last words, and never reads the stopped container's log, which can be gone; a sidecar's restart answers
// the reason alone, because it is not the app's to fix — canyon-identity's last words quoted at the reason's
// end, for the server's log, the only place left that says why once the pod is gone; never oauth2-proxy's,
// which holds secrets (contracts/executor.md, /run; #1790).
func TestAnAppThatRestartsAnswersItsLastWordsAndASidecarDoesNot(t *testing.T) {
	said := "canyon-identity: Get \"https://dex.example.com/keys\": dial tcp 192.168.1.4:443: i/o timeout\n"
	for _, container := range []string{"app", "canyon-identity", "oauth2-proxy"} {
		e, req, _ := fixture(t)
		c := cluster(t, e, func(run string) *corev1.Pod {
			p := readyPod("claims-portal-preview", run)
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container, RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 127, Message: said}}}}
			return p
		})
		_, err := e.Run(t.Context(), req)
		var exited *executor.Exited
		if got := errors.As(err, &exited); got != (container == "app") || err == nil || !strings.Contains(err.Error(), "Error, exit 127") || (exited != nil && exited.Output != said) {
			t.Fatalf("%s restarted: %v, answered with lines: %v (%+v)", container, err, got, exited)
		}
		if want := "Error, exit 127), its last words: " + strconv.Quote(said); container == "canyon-identity" && !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("canyon-identity restarted: %q, want it to end %q", err, want)
		}
		if container == "oauth2-proxy" && strings.Contains(err.Error(), "last words") {
			t.Fatalf("oauth2-proxy restarted: %q, its words in the reason", err)
		}
		for _, a := range c.Actions() {
			if a.GetSubresource() == "log" {
				t.Fatalf("%s restarted: the stopped container's log was read", container)
			}
		}
	}
	e, req, _ := fixture(t) // a status that holds no message adds nothing
	cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "canyon-identity", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
		return p
	})
	if _, err := e.Run(t.Context(), req); err == nil || !strings.HasSuffix(err.Error(), "restarted (Error, exit 1)") {
		t.Fatalf("canyon-identity restarted without a message: %v", err)
	}
}

// A crashed app's last words are the kubelet's own record of them, the terminated container's message
// (terminationMessagePolicy FallbackToLogsOnError), read from the pod: the previous container's log can
// be gone by then, and the kubelet answers that read 200 with its own sentence (the test cluster's
// case-needs-secret). The log is never read.
func TestACrashedAppsLastWordsAreItsTerminationMessage(t *testing.T) {
	e, req, _ := fixture(t)
	req.Env["STRIPE_SECRET_KEY"] = "a-value-that-never-leaks-0123456789"
	said := "stripe rejected a-value-that-never-leaks-0123456789\nPAYMENTS_API_TOKEN is not set: set it with `canyon secret set PAYMENTS_API_TOKEN`\n"
	c := cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, Message: said}}}}
		return p
	})
	_, err := e.Run(t.Context(), req)
	var exited *executor.Exited
	if !errors.As(err, &exited) || exited.Output != strings.ReplaceAll(said, req.Env["STRIPE_SECRET_KEY"], "<STRIPE_SECRET_KEY>") {
		t.Fatalf("a crashed app: %#v (%v)", exited, err)
	}
	for _, a := range c.Actions() {
		if a.GetSubresource() == "log" {
			t.Fatalf("the log was read though the termination message held the app's last words")
		}
	}
	for _, a := range c.Actions() {
		if w, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "deployments" {
			app := w.GetObject().(*appsv1.Deployment).Spec.Template.Spec.Containers
			if i := slices.IndexFunc(app, func(c corev1.Container) bool { return c.Name == "app" }); i < 0 || app[i].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
				t.Fatalf("the app container's termination message policy: %+v", app)
			}
		}
	}
}

// An app that exits 0 leaves no termination message — the kubelet falls back to the log only on an
// error — and the stopped container's log, read then, can be the kubelet's own sentence that it is gone
// (the test cluster's stock Flask starter, #1655): an end the status records answers its message alone, never
// the log, and the exit code says the rest.
func TestAnAppThatExitsZeroAnswersNoLinesOfTheKubelets(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}}}
		return p
	})
	_, err := e.Run(t.Context(), req)
	var exited *executor.Exited
	if !errors.As(err, &exited) || exited.Output != "" || !strings.Contains(err.Error(), "Completed, exit 0") {
		t.Fatalf("an app that exited 0: %#v (%v)", exited, err)
	}
	for _, a := range c.Actions() {
		if a.GetSubresource() == "log" {
			t.Fatalf("the log was read though the status records how the app ended")
		}
	}
}

// The kubelet cuts a termination message at a byte bound, inside a line: the last 2 KiB of the log, the
// first 4 KiB of the file the app wrote. A line it cut is dropped whole, as Tail drops one, so a value
// that straddles the bound leaves none of itself behind (#1549's security review).
func TestATerminationMessageCutInsideAValueLeavesNoneOfIt(t *testing.T) {
	key := "a-value-that-never-leaks-0123456789"
	env := map[string]string{"STRIPE_SECRET_KEY": key}
	fill := strings.Repeat("connecting to stripe\n", 120)
	for name, said := range map[string]string{
		"the log's last 2 KiB":   key[10:] + "\n" + fill[len(fill)-(2<<10)+len(key[10:])+1:],
		"the file's first 4 KiB": (fill + fill)[:4<<10-len(key[:10])] + key[:10],
	} {
		got := executor.NewExited("x", whole(said), env).Output
		if strings.Contains(got, key[10:]) || strings.Contains(got, key[:10]) || !strings.Contains(got, "connecting to stripe\n") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// A first /run whose app container runs and is never ready — nothing answers its TCP probe on port
// 3000 — answers the port reason at the deadline, with the running instance's lines, read before the
// Deployment is deleted (contracts/executor.md, /run; #1238).
func TestAnAppRunningWithNothingOnPort3000AnswersThePortReasonAndItsLines(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status = corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		}
		return p
	})
	_, err := e.Run(t.Context(), req)
	var exited *executor.Exited
	if !errors.As(err, &exited) || exited.Reason != executor.NothingOnPort || exited.Output != "fake logs" {
		t.Fatalf("an app deaf on port 3000: %#v (%v)", exited, err)
	}
	var read []string
	for i, a := range c.Actions() {
		if a.GetSubresource() != "log" {
			continue
		}
		o := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		read = append(read, fmt.Sprintf("%s previous=%v tail=%d", o.Container, o.Previous, *o.TailLines))
		if slices.ContainsFunc(c.Actions()[:i], func(b k8stesting.Action) bool {
			return b.GetVerb() == "delete" && b.GetResource().Resource == "deployments"
		}) {
			t.Error("the lines were read after the Deployment went")
		}
	}
	if want := []string{"app previous=false tail=20"}; !slices.Equal(read, want) {
		t.Errorf("logs read %v, want %v", read, want)
	}
}

func pullFailing(run string) *corev1.Pod {
	p := readyPod("claims-portal-preview", run)
	p.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}
	return p
}

// inRegistry points e at a test registry and req at an image of the app's there, pushed when there.
func inRegistry(t *testing.T, e *Executor, req *executor.RunRequest, there bool) {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	e.Registry = strings.TrimPrefix(testRegistry(t).URL, "http://")
	req.Image = e.Registry + "/canyon/" + req.App + "@" + sum.String()
	if there {
		if err := remote.Write(mustRef(t, e.Registry+"/canyon/"+req.App+":x"), img); err != nil {
			t.Fatal(err)
		}
	}
}

// An image the registry answers 404 for, while the pod waits to pull it, is /run's ErrNoImage, at once
// and not at the deadline, and what the /run wrote goes as after any failed first /run (#1244).
func TestAPullOfAnImageTheRegistryDoesNotHaveIsNoImage(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, pullFailing)
	e.Deadline = time.Minute
	inRegistry(t, e, &req, false)
	if _, err := e.Run(t.Context(), req); !errors.Is(err, executor.ErrNoImage) {
		t.Fatalf("an image the registry does not have: %v", err)
	}
	if w := writes(c); w[len(w)-1] != "delete/secrets" {
		t.Fatalf("a failed first /run leaves nothing: %v", w)
	}
}

// roundTrip is a transport whose every request is answered by fn, never the network.
type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// Once the new pod is up, /run asks its sidecar's /answered once, by the pod's IP, and answers what the
// app answered with said masked by the request's env; an answer naming another host, out of the
// contract or not there at all leaves answered out and the /run up (contracts/executor.md, What the app
// answered; #1740).
func TestRunAnswersWhatTheAppAnsweredItsSidecar(t *testing.T) {
	e, req, _ := fixture(t)
	req.Env = map[string]string{"PAGE_VALUE": "value-in-the-page-9f1e"}
	cluster(t, e, func(run string) *corev1.Pod {
		p := readyPod("claims-portal-preview", run)
		p.Status.PodIP = "10.0.3.7"
		return p
	})
	host := "claims-portal.preview.canyon.meridian.example"
	var calls []string
	for name, tc := range map[string]struct {
		body string
		want *asked.Answer
	}{
		"a 400":          {`{"host":"` + host + `","status":400,"said":"<h1>Bad Request (400)</h1> value-in-the-page-9f1e"}`, &asked.Answer{Status: 400, Said: "<h1>Bad Request (400)</h1> <PAGE_VALUE>"}},
		"nothing":        {`{"host":"` + host + `","error":"refused"}`, &asked.Answer{Error: "refused"}},
		"a long line":    {`{"host":"` + host + `","status":200,"said":"` + strings.Repeat("é", 300) + `"}`, &asked.Answer{Status: 200, Said: strings.Repeat("é", 200)}}, // the seam's 200, in characters
		"another host":   {`{"host":"expenses.preview.canyon.meridian.example","status":200,"said":"ok"}`, nil},
		"a library line": {`{"host":"` + host + `","error":"dial tcp 127.0.0.1:3000: connect: connection refused"}`, nil},
		"an extra field": {`{"host":"` + host + `","status":200,"said":"","extra":1}`, nil},
	} {
		calls = nil
		e.Ask = roundTrip(func(r *http.Request) (*http.Response, error) {
			calls = append(calls, r.Method+" "+r.URL.String())
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
		})
		res, err := e.Run(t.Context(), req)
		if err != nil || (res.Answered == nil) != (tc.want == nil) || tc.want != nil && *res.Answered != *tc.want || !slices.Equal(calls, []string{"GET http://10.0.3.7:4183/answered"}) {
			t.Errorf("%s: %v, answered %+v, want %+v; asked %q", name, err, res.Answered, tc.want, calls)
		}
	}
}
