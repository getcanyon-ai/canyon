package kube

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestLeakScanFindsAPlantedSecret is TestKindAppsReachNoOtherAppNorTheControlPlane's leak check,
// isolated from the cluster it needs: a Secret.Data value is []byte, which encoding/json
// base64-encodes, so a JSON dump of the Secret list alone never contains a raw password even when a
// Secret leaks it. leakScan must read Data itself, not only the JSON.
func TestLeakScanFindsAPlantedSecret(t *testing.T) {
	const admin = "admin-s3cr3t-p4ssw0rd" //nolint:gosec // a planted fixture value, not a credential
	secrets := &corev1.SecretList{Items: []corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Name: "canyon-env-leaky-preview", Namespace: Namespace},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://" + admin + "@db:5433/x")},
	}}}
	buf, err := leakScan(secrets, &corev1.ConfigMapList{}, &appsv1.DeploymentList{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), admin) {
		t.Fatalf("leakScan did not catch a planted admin login in Secret.Data: %s", buf.String())
	}
}
