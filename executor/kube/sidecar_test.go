package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

// upgraded is canyon-identity's image in the release after identityImage's.
const upgraded = "ghcr.io/getcanyon-ai/canyon-identity@sha256:" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// expenses is the fixture's request for another app.
func expenses(req executor.RunRequest) executor.RunRequest {
	other := req
	other.App, other.Network, other.Identity.Audience = "expenses", "canyon-app-expenses", "expenses"
	other.Image = strings.Replace(req.Image, "/canyon/claims-portal@", "/canyon/expenses@", 1)
	return other
}

// deployed is the Deployment name as the cluster holds it.
func deployed(t *testing.T, c *fake.Clientset, name string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(Namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// updated is the Deployments this client updated, by name, in order.
func updated(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if u, ok := a.(k8stesting.UpdateAction); ok && a.GetVerb() == "update" && a.GetResource().Resource == "deployments" {
			out = append(out, u.GetObject().(*appsv1.Deployment).Name)
		}
	}
	return out
}

// envs is the environments of a POST /sidecars answer, without their rolls.
func envs(res executor.Sidecars) []string {
	var out []string
	for _, r := range res.Rolled {
		out = append(out, r.App+"-"+r.Target)
	}
	return out
}

// logs is what the executor logs from now until the test ends.
func logs(t *testing.T) *bytes.Buffer {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logged
}

// After an upgrade (contracts/executor.md, POST /sidecars) an app deployed under the previous release's
// canyon-identity is rolled onto the executor's, and one deployed under the executor's is left alone and
// never answered. The roll is answered once its rollout is done, and again on every call after under the
// same roll, until the environment's next /run: an answer the server lost is answered again. It is
// rolled once: no call after the first writes it.
func TestSidecarRollsAnOlderImageOnceAndAnswersItUntilTheNextRun(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	e.Identity = upgraded
	if _, err := e.Run(t.Context(), expenses(req)); err != nil {
		t.Fatal(err)
	}
	c.ClearActions()
	res, err := e.Sidecars(t.Context())
	if err != nil || res.Image != upgraded || res.Failed != 0 || len(res.Rolled) != 0 {
		t.Fatalf("the first call: %+v %v, want nothing answered yet: the rollout it started is not done", res, err)
	}
	if got := updated(c); !slices.Equal(got, []string{"claims-portal-preview"}) {
		t.Errorf("updated %v, want claims-portal-preview alone: expenses-preview runs the executor's image already", got)
	}
	for _, name := range []string{"claims-portal-preview", "expenses-preview"} {
		if got := deployed(t, c, name).Spec.Template.Spec.InitContainers[0].Image; got != upgraded {
			t.Errorf("%s's canyon-identity runs %s, want %s", name, got, upgraded)
		}
	}
	var roll string
	for i := range 2 {
		c.ClearActions()
		res, err := e.Sidecars(t.Context())
		if err != nil || res.Failed != 0 || !slices.Equal(envs(res), []string{"claims-portal-preview"}) || len(updated(c)) != 0 {
			t.Fatalf("call %d after the roll: %+v %v, updated %v, want claims-portal preview answered and nothing written", i+2, res, err, updated(c))
		}
		if roll == "" {
			roll = res.Rolled[0].Roll
		}
		if res.Rolled[0].Roll != roll || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(roll) {
			t.Fatalf("call %d answers the roll %q, want %q both times", i+2, res.Rolled[0].Roll, roll)
		}
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Sidecars(t.Context()); err != nil || len(res.Rolled) != 0 {
		t.Errorf("after the next /run: %+v %v, want the roll no longer answered", res, err)
	}
}

// A roll changes canyon-identity's image and adds its two annotations, and nothing else (contracts/executor.md,
// POST /sidecars): the app's own image and environment, oauth2-proxy, the proxy variables of a floored pod,
// the labels — the canyon.run /run reads its pods by among them — are the bytes /run wrote.
func TestSidecarRollChangesTheSidecarsImageAlone(t *testing.T) {
	e, req, _ := withEgress(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	before := deployed(t, c, "claims-portal-preview")
	e.Identity = upgraded
	if _, err := e.Sidecars(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := deployed(t, c, "claims-portal-preview")
	if after.Spec.Template.Spec.InitContainers[0].Image != upgraded {
		t.Fatalf("canyon-identity runs %s, want %s", after.Spec.Template.Spec.InitContainers[0].Image, upgraded)
	}
	want := before.DeepCopy()
	want.Spec.Template.Spec.InitContainers[0].Image = upgraded
	want.Annotations = maps.Clone(before.Annotations)
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations["canyon.sidecar-rolled-to"], want.Annotations["canyon.sidecar-roll"] = upgraded, after.Annotations["canyon.sidecar-roll"]
	for _, part := range []struct {
		name      string
		got, want any
	}{{"spec", after.Spec, want.Spec}, {"labels", after.Labels, want.Labels}, {"annotations", after.Annotations, want.Annotations}} {
		g, _ := json.Marshal(part.got)
		w, _ := json.Marshal(part.want)
		if !bytes.Equal(g, w) {
			t.Errorf("the roll changed the Deployment's %s beyond canyon-identity's image and its annotations:\n got %s\nwant %s", part.name, g, w)
		}
	}
	if a, b := after.Spec.Template.Spec.Containers[0], before.Spec.Template.Spec.Containers[0]; a.Image != req.Image || a.Image != b.Image || !slices.EqualFunc(a.Env, b.Env, func(x, y corev1.EnvVar) bool { return x.String() == y.String() }) {
		t.Errorf("the app container went from %s %v to %s %v", b.Image, b.Env, a.Image, a.Env)
	}
}

// renamed is d named name, its access file as a /run would name it.
func renamed(d *appsv1.Deployment, name string) {
	d.Name, d.Spec.Template.Spec.Volumes[0].ConfigMap.Name = name, "canyon-access-"+name
}

// The executor touches only a Deployment a /run made (contracts/executor.md, POST /sidecars): one that
// breaks any one mark of it — named <app>-<target> by its labels, its template labelled canyon.run, its
// canyon-identity mounting canyon-access-<app>-<target> — is never rolled; it is logged and counted. A
// Deployment being deleted, an environment being undeployed, is passed over and not counted.
func TestSidecarLeavesWhatRunDidNotMakeAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(d *appsv1.Deployment)
		count int
	}{
		{"named otherwise than its labels", func(d *appsv1.Deployment) { renamed(d, "expenses-staging") }, 1},
		{"an app out of shape", func(d *appsv1.Deployment) { renamed(d, "Expenses-preview"); d.Labels["canyon.app"] = "Expenses" }, 1},
		{"a target out of shape", func(d *appsv1.Deployment) { renamed(d, "expenses-staging"); d.Labels["canyon.target"] = "staging" }, 1},
		{"no canyon.run on its template", func(d *appsv1.Deployment) { delete(d.Spec.Template.Labels, "canyon.run") }, 1},
		{"no canyon-identity", func(d *appsv1.Deployment) { d.Spec.Template.Spec.InitContainers[0].Name = "identity" }, 1},
		{"another app's access file", func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Volumes[0].ConfigMap.Name = "canyon-access-claims-portal-preview"
		}, 1},
		{"canyon-identity not mounting it", func(d *appsv1.Deployment) { d.Spec.Template.Spec.InitContainers[0].VolumeMounts = nil }, 1},
		{"being deleted", func(d *appsv1.Deployment) {
			d.DeletionTimestamp, d.Finalizers = new(metav1.Now()), []string{"foregroundDeletion"}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, req, _ := fixture(t)
			c := cluster(t, e, healthy)
			other := expenses(req)
			door, doorJSON, _, err := e.check(other)
			if err != nil {
				t.Fatal(err)
			}
			d := e.deployment(other, door, doorJSON, nonce(), nil, nil)
			if d.Spec.Template.Spec.Volumes[0].Name != "access" {
				t.Fatalf("the template's first volume is %s, want access", d.Spec.Template.Spec.Volumes[0].Name)
			}
			tc.spoil(d)
			if err := c.Tracker().Add(d); err != nil {
				t.Fatal(err)
			}
			logged := logs(t)
			e.Identity = upgraded
			res, err := e.Sidecars(t.Context())
			if err != nil || res.Failed != tc.count || len(res.Rolled) != 0 || len(updated(c)) != 0 {
				t.Fatalf("%+v %v, updated %v, want nothing rolled and %d counted", res, err, updated(c), tc.count)
			}
			if got := deployed(t, c, d.Name).Spec.Template.Spec.InitContainers[0].Image; got != identityImage {
				t.Errorf("its canyon-identity runs %s, want the image it was given, %s", got, identityImage)
			}
			if said := strings.Contains(logged.String(), "deployment "+d.Name+" is not rolled"); said != (tc.count == 1) {
				t.Errorf("logged %v, want %v: %q", said, tc.count == 1, logged.String())
			}
		})
	}
}

// A Deployment a /run wrote between the list and the roll is read again and rolled; one the executor
// cannot read again is never touched, logged and counted (contracts/executor.md, POST /sidecars).
func TestSidecarReadsAgainAfterAConflictAndCountsWhatItCannotRead(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	for _, r := range []executor.RunRequest{req, expenses(req)} {
		if _, err := e.Run(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	conflicts := map[string]int{}
	c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment).Name
		if conflicts[name]++; conflicts[name] == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, name, nil)
		}
		return false, nil, nil
	})
	c.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "expenses-preview" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "expenses-preview", nil)
		}
		return false, nil, nil
	})
	logged := logs(t)
	e.Identity = upgraded
	if res, err := e.Sidecars(t.Context()); err != nil || res.Failed != 1 {
		t.Fatalf("%+v %v, want expenses-preview counted", res, err)
	}
	if got := deployed(t, c, "claims-portal-preview").Spec.Template.Spec.InitContainers[0].Image; got != upgraded {
		t.Errorf("claims-portal-preview's canyon-identity runs %s after its conflict, want %s", got, upgraded)
	}
	if !strings.Contains(logged.String(), "deployment expenses-preview is not rolled") {
		t.Errorf("the unread Deployment is not in the log: %q", logged.String())
	}
}

// A roll whose rollout is not done is not answered: while it runs it is neither rolled nor failed, and once
// it has missed its progress deadline it is counted and logged, never answered, and not undone — the
// previous pod serves on (contracts/executor.md, POST /sidecars).
func TestSidecarAnswersNoRollWhoseRolloutIsNotDone(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	e.Identity = upgraded
	if _, err := e.Sidecars(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := deployed(t, c, "claims-portal-preview")
	d.Status = appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1}
	if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, Namespace); err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	if res, err := e.Sidecars(t.Context()); err != nil || len(res.Rolled) != 0 || res.Failed != 0 {
		t.Errorf("a rollout under way: %+v %v, want nothing answered or counted", res, err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, Namespace); err != nil {
		t.Fatal(err)
	}
	c.ClearActions()
	if res, err := e.Sidecars(t.Context()); err != nil || len(res.Rolled) != 0 || res.Failed != 1 || len(updated(c)) != 0 {
		t.Errorf("a rollout past its deadline: %+v %v, updated %v, want it counted and nothing written", res, err, updated(c))
	}
	if !strings.Contains(logged.String(), "deployment claims-portal-preview is not rolled onto "+upgraded+": its rolled pod did not become available") {
		t.Errorf("the stuck rollout is not in the log: %q", logged.String())
	}
}

// One call rolls at most rollsPerCall Deployments, each a pod surged at once; the rest roll on the calls
// after (contracts/executor.md, POST /sidecars).
func TestSidecarRollsAtMostTenACall(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	for i := range 12 {
		r := req
		r.App, r.Network, r.Identity.Audience = fmt.Sprintf("app-%02d", i), fmt.Sprintf("canyon-app-app-%02d", i), fmt.Sprintf("app-%02d", i)
		r.Image = strings.Replace(req.Image, "/canyon/claims-portal@", "/canyon/"+r.App+"@", 1)
		if _, err := e.Run(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	e.Identity = upgraded
	for i, want := range []struct{ updates, answered int }{{10, 0}, {2, 10}, {0, 12}} {
		c.ClearActions()
		res, err := e.Sidecars(t.Context())
		if err != nil || len(updated(c)) != want.updates || len(res.Rolled) != want.answered {
			t.Fatalf("call %d: %d rolled, %d answered (%v), want %d and %d", i+1, len(updated(c)), len(res.Rolled), err, want.updates, want.answered)
		}
	}
}

// POST /sidecars answers the contract's sidecars_result (contracts/testdata/executor/claims-portal.json):
// the executor's image, the app a roll moved onto it, and a Deployment no /run made, counted.
func TestSidecarsAnswersTheContractsShape(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	other := expenses(req)
	door, doorJSON, _, err := e.check(other)
	if err != nil {
		t.Fatal(err)
	}
	foreign := e.deployment(other, door, doorJSON, "", nil, nil)
	delete(foreign.Spec.Template.Labels, "canyon.run")
	if err := c.Tracker().Add(foreign); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../contracts/testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Want json.RawMessage `json:"sidecars_result"`
	}
	var want bytes.Buffer
	if err := json.Unmarshal(raw, &f); err != nil || json.Compact(&want, f.Want) != nil {
		t.Fatal(err)
	}
	e.Identity = upgraded
	if _, err := e.Sidecars(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	code, answer := send(t, "POST", srv.URL+"/sidecars", "")
	roll := deployed(t, c, "claims-portal-preview").Annotations["canyon.sidecar-roll"]
	if code != 200 || roll == "" || strings.Replace(answer, `"roll":"`+roll+`"`, `"roll":"4f1c0b9a"`, 1) != want.String() {
		t.Fatalf("POST /sidecars: %d %s\nwant 200 %s, its roll %q", code, answer, want.String(), roll)
	}
}

// runShaped adds a Deployment of app as a /run under the executor's image makes it, without its rollout.
func runShaped(t *testing.T, e *Executor, c *fake.Clientset, req executor.RunRequest, app string) {
	t.Helper()
	r := req
	r.App, r.Network, r.Identity.Audience = app, "canyon-app-"+app, app
	r.Image = strings.Replace(req.Image, "/canyon/claims-portal@", "/canyon/"+app+"@", 1)
	door, doorJSON, _, err := e.check(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Tracker().Add(e.deployment(r, door, doorJSON, nonce(), nil, nil)); err != nil {
		t.Fatal(err)
	}
}

// Deployments the cluster refuses to write — an admission webhook's — hold up no other: only a roll that
// took counts toward a call's ten, and a call tries at most thirty (contracts/executor.md, POST /sidecars).
func TestSidecarRollsTheClusterRefusesHoldUpNoOther(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	for i := range 10 {
		runShaped(t, e, c, req, fmt.Sprintf("refused-%02d", i))
	}
	runShaped(t, e, c, req, "writable")
	c.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) { // by name, as the API lists them
		listed, err := c.Tracker().List(appsv1.SchemeGroupVersion.WithResource("deployments"), appsv1.SchemeGroupVersion.WithKind("Deployment"), Namespace)
		if err != nil {
			return true, nil, err
		}
		l := listed.(*appsv1.DeploymentList)
		slices.SortFunc(l.Items, func(a, b appsv1.Deployment) int { return strings.Compare(a.Name, b.Name) })
		return true, l, nil
	})
	c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if name := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment).Name; strings.HasPrefix(name, "refused-") {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, name, errors.New("admission webhook denied the request"))
		}
		return false, nil, nil
	})
	logs(t)
	e.Identity = upgraded
	if res, err := e.Sidecars(t.Context()); err != nil || res.Failed != 10 || deployed(t, c, "writable-preview").Spec.Template.Spec.InitContainers[0].Image != upgraded {
		t.Fatalf("ten refused before it: %+v %v, writable-preview runs %s, want it rolled in the same call and the ten counted",
			res, err, deployed(t, c, "writable-preview").Spec.Template.Spec.InitContainers[0].Image)
	}
	e.Identity = identityImage // thirty more, deployed under the previous release
	for i := 10; i < 40; i++ {
		runShaped(t, e, c, req, fmt.Sprintf("refused-%02d", i))
	}
	e.Identity = upgraded
	c.ClearActions()
	if res, err := e.Sidecars(t.Context()); err != nil || len(updated(c)) != triesPerCall || res.Failed != triesPerCall {
		t.Errorf("forty refused: %d tried, %+v %v, want %d tried and counted", len(updated(c)), res, err, triesPerCall)
	}
}

// A Deployment found being deleted when it is read again after a conflict is left alone, as one listed
// so is: an environment being undeployed is not rolled (contracts/executor.md, POST /sidecars).
func TestSidecarLeavesADeploymentFoundBeingDeletedAfterAConflict(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	conflicted := false
	c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true                                                                                                      // the undeploy's delete lands between the list and the roll
		got, err := c.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), Namespace, "claims-portal-preview") // the tracker: the client's lock is held here
		if err != nil {
			t.Fatal(err)
		}
		d := got.(*appsv1.Deployment).DeepCopy()
		d.DeletionTimestamp, d.Finalizers = new(metav1.Now()), []string{"foregroundDeletion"}
		if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, d.Name, nil)
	})
	logs(t)
	e.Identity = upgraded
	c.ClearActions()
	res, err := e.Sidecars(t.Context())
	if err != nil || res.Failed != 0 || len(res.Rolled) != 0 || len(updated(c)) != 1 || deployed(t, c, "claims-portal-preview").Spec.Template.Spec.InitContainers[0].Image != identityImage {
		t.Errorf("%+v %v, %d update(s), want the one refused and the Deployment being deleted left as it was", res, err, len(updated(c)))
	}
}
