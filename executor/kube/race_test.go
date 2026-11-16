package kube

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func conflict(resource, name string) error {
	return apierrors.NewConflict(schema.GroupResource{Resource: resource}, name, nil)
}

func failing(run string) *corev1.Pod {
	p := readyPod("claims-portal-preview", run)
	p.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}
	return p
}

// A PUT /access landing between the /run's read of the ConfigMap and its write is a conflict the /run
// retries, not a failed /run: read again, the ConfigMap holds the share, which is newer, and stays.
func TestRunRetriesTheAccessWriteOnAConflict(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	shared := false
	c.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if shared {
			return false, nil, nil
		}
		shared = true
		m := accessMap(req.App, req.Target, []byte(`{"shared":true}`), "2026-09-25T10:00:09Z")
		if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), m, Namespace); err != nil {
			t.Error(err)
		}
		return true, nil, conflict("configmaps", m.Name)
	})
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatalf("a share between read and write fails the /run: %v", err)
	}
	m, _ := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{})
	if m.Data["access.json"] != `{"shared":true}` {
		t.Fatalf("the share was overwritten: %v", m.Data)
	}
}

// A first /run that fails deletes what it wrote, and an access ConfigMap that was there before it is
// not its to delete.
func TestAFailedFirstRunKeepsAnAccessFileThatWasThere(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, failing)
	if _, err := c.CoreV1().ConfigMaps(Namespace).Create(t.Context(), accessMap(req.App, req.Target, []byte(`{}`), "2026-09-25T09:00:00Z"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(t.Context(), req); err == nil {
		t.Fatal("an image that never pulls answered")
	}
	if _, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), "canyon-access-claims-portal-preview", metav1.GetOptions{}); err != nil {
		t.Fatalf("the access ConfigMap that was there went with the failed /run: %v", err)
	}
	if _, err := c.CoreV1().Services(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the Service the failed /run wrote is still there: %v", err)
	}
}

// A /run waits for its own template: once another /run's template has replaced it, this one fails and
// leaves the Deployment to the /run that wrote it — no restore, no cleanup.
func TestARunWhoseTemplateWasReplacedFailsAndLeavesIt(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	c.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := c.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), Namespace, a.(k8stesting.GetAction).GetName())
		if err != nil {
			return false, nil, nil
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Spec.Template.Labels["canyon.run"] = "0therrun"
		return true, d, nil
	})
	_, err := e.Run(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "another /run") {
		t.Fatalf("a replaced template: %v", err)
	}
	if _, err := c.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), Namespace, "claims-portal-preview"); err != nil {
		t.Fatalf("the Deployment the other /run owns was removed: %v", err)
	}
}

// The Deployment's writes retry on a conflict — the controller's status writes change its
// resourceVersion between a read and an update: the redeploy's update and the restore's each meet one.
func TestDeploymentWritesRetryOnAConflict(t *testing.T) {
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
	before, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	updates := 0
	c.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updates++; updates%2 == 1 { // the first attempt of each update
			return true, nil, conflict("deployments", "claims-portal-preview")
		}
		return false, nil, nil
	})
	crash, req.Image = true, strings.Replace(req.Image, "4f1c", "0000", 1)
	if _, err := e.Run(t.Context(), req); err == nil || !strings.Contains(err.Error(), "its container restarted") {
		t.Fatalf("the redeploy's update did not get past its conflict to the rollout: %v", err)
	}
	after, _ := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if updates != 4 || after.Spec.Template.Labels["canyon.run"] != before.Spec.Template.Labels["canyon.run"] {
		t.Fatalf("%d updates; template %v, want the previous one back", updates, after.Spec.Template.Labels)
	}
}
