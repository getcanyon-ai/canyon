package kube

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// contracts/executor.md, On Kubernetes: /run answers previous_gone true once its rollout completed and no ReplicaSet
// of an earlier template is left with a pod and no pod of one remains, terminating ones included, as the cluster lists
// them; false when the bound passes first. A first /run has no earlier template.
func TestPreviousGoneIsReadFromTheReplicaSetsAndPods(t *testing.T) {
	e, req, _ := fixture(t)
	c := cluster(t, e, healthy)
	collect := true // the controller takes the earlier template's pods once the new one is available
	c.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		run := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment).Spec.Template.Labels["canyon.run"]
		pods := corev1.SchemeGroupVersion.WithResource("pods")
		list, _ := c.Tracker().List(pods, corev1.SchemeGroupVersion.WithKind("Pod"), Namespace)
		for _, p := range list.(*corev1.PodList).Items {
			if collect && p.Labels["canyon.run"] != run {
				_ = c.Tracker().Delete(pods, Namespace, p.Name)
			}
		}
		return false, nil, nil
	})
	gone := func(what string, want bool) {
		t.Helper()
		res, err := e.Run(t.Context(), req)
		if err != nil || res.PreviousGone == nil || *res.PreviousGone != want {
			t.Fatalf("%s: previous_gone %v (%v), want %t", what, res.PreviousGone, err, want)
		}
	}
	gone("a first /run", true)
	gone("the earlier template's pod taken", true)

	collect = false
	gone("the earlier template's pod still there, terminating or not", false)
	collect = true

	rs := func(replicas int32) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "claims-portal-preview-old", Namespace: Namespace,
			Labels: map[string]string{"canyon.app": "claims-portal", "canyon.target": "preview", "canyon.run": "earlier"}},
			Spec: appsv1.ReplicaSetSpec{Replicas: &replicas}, Status: appsv1.ReplicaSetStatus{Replicas: replicas}}
	}
	if err := c.Tracker().Add(rs(1)); err != nil {
		t.Fatal(err)
	}
	gone("an earlier template's ReplicaSet still holding a pod, though none is listed", false)
	if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs(0), Namespace); err != nil {
		t.Fatal(err)
	}
	gone("an earlier template's ReplicaSet scaled to nothing", true)
}
