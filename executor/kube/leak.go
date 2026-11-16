package kube

import (
	"bytes"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// leakScan is every byte the kind test's leak check searches for a forbidden string: each Secret's
// Data values written raw, then the JSON of every Secret, ConfigMap and Deployment for what appears
// in their other fields. Data is []byte, which encoding/json base64-encodes, so a plain JSON dump of
// the Secret list alone never contains a raw value even when a Secret leaks it.
func leakScan(secrets *corev1.SecretList, maps *corev1.ConfigMapList, deployments *appsv1.DeploymentList) (bytes.Buffer, error) {
	var buf bytes.Buffer
	for _, s := range secrets.Items {
		for _, v := range s.Data {
			buf.Write(v)
		}
	}
	err := json.NewEncoder(&buf).Encode([]any{secrets, maps, deployments})
	return buf, err
}
