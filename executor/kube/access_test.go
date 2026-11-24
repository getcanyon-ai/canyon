package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// PUT /access rewrites the ConfigMap and nothing else; a newer one already there is stale (409) and
// unchanged, the same updated_at again is rewritten; nothing running is 404, and a request the schema
// refuses is 400, before anything is read.
func TestPutAccessRewritesTheConfigMapAlone(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	put := func(updated string) error {
		file := strings.Replace(string(req.Access), "2026-09-25T10:00:00Z", updated, 1)
		return e.PutAccess(t.Context(), req.App, req.Target, []byte(file))
	}
	if err := put("2026-09-25T10:00:05Z"); !errors.Is(err, executor.ErrNoContainer) || len(writes(c)) > 0 {
		t.Fatalf("nothing running: %v, and it wrote %v", err, writes(c))
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	c.ClearActions()
	if err := put("2026-09-25T10:00:05Z"); err != nil {
		t.Fatal(err)
	}
	if got := writes(c); !slices.Equal(got, []string{"update/configmaps"}) {
		t.Fatalf("PUT /access wrote %v", got)
	}
	m, _ := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{})
	if m.Annotations["canyon.updated_at"] != "2026-09-25T10:00:05Z" || !strings.Contains(m.Data["access.json"], "10:00:05Z") {
		t.Fatalf("ConfigMap %v %v", m.Annotations, m.Data)
	}
	if err := put("2026-09-25T10:00:05Z"); err != nil {
		t.Fatalf("the same updated_at again: %v", err)
	}
	if err := put("2026-09-25T10:00:04Z"); !errors.Is(err, executor.ErrStale) {
		t.Fatalf("an older updated_at: %v", err)
	}
	if m, _ := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{}); m.Annotations["canyon.updated_at"] != "2026-09-25T10:00:05Z" {
		t.Fatalf("a stale PUT changed the ConfigMap: %v", m.Annotations)
	}
	c.ClearActions()
	for name, call := range map[string]func() error{
		"an app with _":           func() error { return e.PutAccess(t.Context(), "claims_portal", req.Target, req.Access) },
		"target staging":          func() error { return e.PutAccess(t.Context(), req.App, "staging", req.Access) },
		"access with another key": func() error { return e.PutAccess(t.Context(), req.App, req.Target, []byte(`{"extra":1}`)) },
		"fractional seconds":      func() error { return put("2026-09-25T10:00:06.5Z") },
	} {
		if err := call(); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	if a := c.Actions(); len(a) > 0 {
		t.Fatalf("a refused PUT reached the API: %v", a)
	}
}

// The access file names the issuer its subjects are from, and /run and PUT /access take it into the
// ConfigMap the sidecar reads; a field the contract does not name, such as the subject_client #2220
// dropped, is still refused by both before anything is written.
func TestTheAccessFileNamesItsSubjectIssuer(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	var file map[string]any
	if err := json.Unmarshal(req.Access, &file); err != nil || file["subject_issuer"] != "https://login.meridian.example" {
		t.Fatalf("the fixture's access file names no subject_issuer: %v %v", file, err)
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatalf("/run: %v", err)
	}
	if err := e.PutAccess(t.Context(), req.App, req.Target, req.Access); err != nil {
		t.Fatalf("PUT /access: %v", err)
	}
	if m, _ := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{}); !strings.Contains(m.Data["access.json"], "https://login.meridian.example") {
		t.Fatalf("the ConfigMap lost subject_issuer: %v", m.Data)
	}
	file["subject_client"] = "canyon"
	extra, _ := json.Marshal(file)
	c.ClearActions()
	if err := e.PutAccess(t.Context(), req.App, req.Target, extra); !errors.Is(err, executor.ErrRequest) {
		t.Errorf("PUT /access with subject_client: %v, want a refusal", err)
	}
	req.Access = extra
	if _, err := e.Run(t.Context(), req); !errors.Is(err, executor.ErrRequest) {
		t.Errorf("/run with subject_client: %v, want a refusal", err)
	}
	if a := c.Actions(); len(a) > 0 {
		t.Fatalf("a refused call reached the API: %v", a)
	}
}

// Invariant 5 on Kubernetes: /logs streams the app container of the Deployment's newest pod — never a
// sidecar's — with the tail it was asked for, following; ready comes before the log request, which
// the API answers only with the app's first byte, and a Deployment with no pod is 404 before it.
func TestLogsReadTheAppContainerAlone(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	ready := false
	if err := e.Logs(t.Context(), req.App, req.Target, 5, true, &bytes.Buffer{}, func() { ready = true }); !errors.Is(err, executor.ErrNoContainer) || ready {
		t.Fatalf("no pod: %v, ready %v", err, ready)
	}
	older := readyPod("claims-portal-preview", "older")
	older.Labels, older.CreationTimestamp = labels(req.App, req.Target), metav1.NewTime(time.Now().Add(-time.Hour))
	newer := readyPod("claims-portal-preview", "newer")
	newer.Labels, newer.CreationTimestamp = labels(req.App, req.Target), metav1.NewTime(time.Now())
	if p := newestPod([]corev1.Pod{*older, *newer, *older}); p.Name != newer.Name {
		t.Fatalf("the newest pod is %s", p.Name)
	}
	for _, p := range []*corev1.Pod{newer, older} {
		if err := c.Tracker().Add(p); err != nil {
			t.Fatal(err)
		}
	}
	var asked []string
	follow := true
	c.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		asked = append(asked, o.Container)
		if o.Follow != follow || o.TailLines == nil || *o.TailLines != 5 || !ready { // a quiet app's caller has its 200 already
			t.Errorf("options %+v, ready %v", o, ready)
		}
		return true, &runtime.Unknown{Raw: []byte("line 1\nline 2\n")}, nil
	})
	for _, follow = range []bool{true, false} { // follow=false: the tail and the end (contracts/executor.md)
		var out bytes.Buffer
		ready, asked = false, nil
		if err := e.Logs(t.Context(), req.App, req.Target, 5, follow, &out, func() { ready = true }); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(asked, []string{"app"}) || out.String() != "line 1\nline 2\n" || !ready {
			t.Fatalf("follow %v: asked %v, wrote %q, ready %v", follow, asked, out.String(), ready)
		}
	}
	var out bytes.Buffer
	for name, call := range map[string]func() error{
		"an app with _":  func() error { return e.Logs(t.Context(), "claims_portal", req.Target, 5, true, &out, nil) },
		"target staging": func() error { return e.Logs(t.Context(), req.App, "staging", 5, true, &out, nil) },
	} {
		if err := call(); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

// A crashed app's log is its last run (contracts/executor.md, A container that isn't running): the run before
// while its container waits to start again, its own once it has ended for good, never followed; where the kubelet
// has lost that run's log, the tail the pod's status keeps and the executor's line. A running one is read as
// asked, and so is one waiting that never ended.
func TestLogsReadACrashedAppsLastRun(t *testing.T) {
	ended := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "TypeError: x is undefined\n"}
	for name, c := range map[string]struct {
		status   corev1.ContainerStatus
		refused  bool
		previous bool
		follow   bool
		want     string
	}{
		"running":               {corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, false, false, true, "line 1\n"},
		"waiting after a crash": {corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, LastTerminationState: corev1.ContainerState{Terminated: ended}}, false, true, false, "line 1\n"},
		"ended for good":        {corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Terminated: ended}}, false, false, false, "line 1\n"},
		"its log gone":          {corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, LastTerminationState: corev1.ContainerState{Terminated: ended}}, true, true, false, "TypeError: x is undefined\ncanyon: the cluster kept only the end of the app's last run\n"},
		"waiting, never ended":  {corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}, false, false, true, "line 1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			e, req, _ := fixture(t)
			k := cluster(t, e, healthy)
			pod := readyPod("claims-portal-preview", "newer")
			pod.Labels = labels(req.App, req.Target)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "canyon-identity", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, c.status}
			if err := k.Tracker().Add(pod); err != nil {
				t.Fatal(err)
			}
			var asked []corev1.PodLogOptions
			k.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, runtime.Object, error) {
				o := *a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
				asked = append(asked, o)
				if c.refused {
					return true, nil, apierrors.NewBadRequest(`previous terminated container "app" in pod "newer" not found`)
				}
				return true, &runtime.Unknown{Raw: []byte("line 1\n")}, nil
			})
			var out bytes.Buffer
			if err := e.Logs(t.Context(), req.App, req.Target, 5, true, &out, nil); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || asked[0].Container != "app" || asked[0].Previous != c.previous || asked[0].Follow != c.follow || out.String() != c.want {
				t.Errorf("asked %+v, wrote %q; want previous %v, follow %v, %q", asked, out.String(), c.previous, c.follow, c.want)
			}
		})
	}
}

// A log request the API refuses once the caller has its 200 — the app container still starting —
// ends that 200's body on the API's reason (contracts/executor.md, GET /logs).
func TestALogRequestRefusedAfterReadyEndsOnItsReason(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	pod := readyPod("claims-portal-preview", "newer")
	pod.Labels = labels(req.App, req.Target)
	if err := c.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("get", "pods/log", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewBadRequest(`container "app" in pod "newer" is waiting to start: ContainerCreating`)
	})
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/logs?app=" + req.App + "&target=" + req.Target + "&tail=5")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if got := string(body); res.StatusCode != http.StatusOK || !strings.HasPrefix(got, "canyon: this log could not be read: ") ||
		!strings.HasSuffix(got, "is waiting to start: ContainerCreating\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("status %d, body %q", res.StatusCode, got)
	}
}
