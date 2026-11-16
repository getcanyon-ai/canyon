//go:build cluster

package cluster

import (
	"context"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// kube is the cluster $E2E_KUBECONFIG reaches, which TestMain connects. That kubeconfig may get,
// list and watch, and ask access reviews, and nothing else (README, The read-only kubeconfig), so
// what the suite reads of the cluster is what the objects say, never a Secret's content.
var kube kubernetes.Interface

func kubectl(t *testing.T) kubernetes.Interface {
	t.Helper()
	if kube == nil {
		t.Fatal("E2E_KUBECONFIG names no kubeconfig: this case reads the cluster")
	}
	return kube
}

// pods are the pods in namespace that selector, a label selector, matches; "" matches every one.
// The same for configMaps, jobs and claims.
func pods(t *testing.T, namespace, selector string) []corev1.Pod {
	t.Helper()
	list, err := kubectl(t).CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("listing pods in %s: %v", namespace, err)
	}
	return list.Items
}

func configMaps(t *testing.T, namespace, selector string) []corev1.ConfigMap {
	t.Helper()
	list, err := kubectl(t).CoreV1().ConfigMaps(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("listing ConfigMaps in %s: %v", namespace, err)
	}
	return list.Items
}

func jobs(t *testing.T, namespace, selector string) []batchv1.Job {
	t.Helper()
	list, err := kubectl(t).BatchV1().Jobs(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("listing Jobs in %s: %v", namespace, err)
	}
	return list.Items
}

func claims(t *testing.T, namespace, selector string) []corev1.PersistentVolumeClaim {
	t.Helper()
	list, err := kubectl(t).CoreV1().PersistentVolumeClaims(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("listing PersistentVolumeClaims in %s: %v", namespace, err)
	}
	return list.Items
}

// can asks the API server whether the service account <namespace>:<name> may verb the resource
// (executor/kube/kind_test.go's, against the test cluster: I4).
func can(t *testing.T, sa, namespace, verb, group, resource string) bool {
	t.Helper()
	review, err := kubectl(t).AuthorizationV1().SubjectAccessReviews().Create(context.Background(), &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               "system:serviceaccount:" + sa,
		Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + strings.Split(sa, ":")[0], "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Group: group, Resource: resource},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("access review: %v", err)
	}
	return review.Status.Allowed
}
