package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	quantity "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// kube runs a release as an org's pipeline would: one Deployment of the app and the sidecar, the
// app's port answering the sidecar alone (design/HAND-OFF.md §3), the access token in a Secret the
// sidecar reads as the release's sidecar.secret, and a Service on the sidecar's port. The variables the
// platform built for the app's resources go in a Secret of their own, <app>-<env>-resources, which the
// app reads them from (injectResources).
type kube struct {
	client            kubernetes.Interface
	namespace, domain string
	ca                []byte // PEM, mounted for the sidecar as SSL_CERT_FILE when set
}

func newKube(namespace, domain, caFile string) (*kube, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	k := &kube{client: client, namespace: namespace, domain: domain}
	if caFile != "" {
		if k.ca, err = os.ReadFile(caFile); err != nil { //nolint:gosec // the operator names the file
			return nil, err
		}
	}
	return k, nil
}

func (k *kube) run(ctx context.Context, r release, accessToken string, values map[string]string) (string, error) {
	name, labels := r.App+"-"+r.Environment, map[string]string{"app": r.App + "-" + r.Environment}
	meta := metav1.ObjectMeta{Name: name, Namespace: k.namespace, Labels: labels}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-access", Namespace: k.namespace}, StringData: map[string]string{r.Sidecar.Secret: accessToken}}
	sidecarEnv := envOf(r.Sidecar.Env)
	sidecarEnv = append(sidecarEnv, corev1.EnvVar{Name: r.Sidecar.Secret, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: r.Sidecar.Secret}}})
	// The sidecar as the docs' reference pod spec runs it: no root, a read-only root file system with the
	// access cache's emptyDir its one writable path, mounted in no other container, and its resources.
	cacheLimit := quantity.MustParse("4Mi")
	pod := corev1.PodSpec{Containers: []corev1.Container{
		{Name: "app", Image: r.Image, Env: envOf(r.AppEnv)},
		{Name: "sidecar", Image: r.Sidecar.Image, Env: sidecarEnv, Ports: []corev1.ContainerPort{{ContainerPort: int32(r.Sidecar.Port)}}, //nolint:gosec // a port number
			VolumeMounts: []corev1.VolumeMount{{Name: "access-cache", MountPath: "/var/cache/canyon"}},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				RunAsNonRoot: new(true), ReadOnlyRootFilesystem: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: quantity.MustParse("10m"), corev1.ResourceMemory: quantity.MustParse("32Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity.MustParse("100m"), corev1.ResourceMemory: quantity.MustParse("64Mi")},
			}},
	}, Volumes: []corev1.Volume{{Name: "access-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &cacheLimit}}}}}
	if k.ca != nil {
		ca := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-ca", Namespace: k.namespace}, Data: map[string]string{"ca.crt": string(k.ca)}}
		if err := upsert(ctx, k.client.CoreV1().ConfigMaps(k.namespace), ca); err != nil {
			return "", err
		}
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: ca.Name}}}})
		side := &pod.Containers[1]
		side.VolumeMounts = append(side.VolumeMounts, corev1.VolumeMount{Name: "ca", MountPath: "/etc/org-ca", ReadOnly: true})
		side.Env = append(side.Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/etc/org-ca/ca.crt"})
	}
	var annotations map[string]string
	if len(values) > 0 {
		sum, err := k.injectResources(ctx, name, &pod.Containers[0], values)
		if err != nil {
			return "", err
		}
		annotations = map[string]string{"handoff.test/resources": sum} // a new password rolls the pod onto it
	}
	one := int32(1)
	deploy := &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: pod}}}
	svc := &corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: labels,
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(int32(r.Sidecar.Port))}}}} //nolint:gosec // a port number
	if err := upsert(ctx, k.client.CoreV1().Secrets(k.namespace), secret); err != nil {
		return "", err
	}
	if err := upsert(ctx, k.client.AppsV1().Deployments(k.namespace), deploy); err != nil {
		return "", err
	}
	if err := upsert(ctx, k.client.CoreV1().Services(k.namespace), svc); err != nil {
		return "", err
	}
	return "https://" + r.App + "." + r.Environment + "." + k.domain, k.rolledOut(ctx, name)
}

// injectResources puts the variables the platform built for the app's resources (DATABASE_URL) in a
// Secret of the platform's own, <app>-<env>-resources, and in the app's environment from it, as External
// Secrets and a Score workload's resource outputs do: the values never reach Canyon. It answers a digest
// of the values, so a changed one changes the pod template.
func (k *kube) injectResources(ctx context.Context, name string, app *corev1.Container, values map[string]string) (string, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-resources", Namespace: k.namespace}, StringData: values}
	if err := upsert(ctx, k.client.CoreV1().Secrets(k.namespace), secret); err != nil {
		return "", err
	}
	sum := sha256.New()
	for _, v := range slices.Sorted(maps.Keys(values)) {
		app.Env = append(app.Env, corev1.EnvVar{Name: v, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: v}}})
		_, _ = sum.Write([]byte(v + "\x00" + values[v] + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// rolledOut waits up to five minutes for the Deployment's newest pod to be available.
func (k *kube) rolledOut(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		d, err := k.client.AppsV1().Deployments(k.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == 1 && d.Status.AvailableReplicas == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not roll out: %w", name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// remove takes the app off the cluster. The resources Secret stays, as the data does, but on a project
// delete's remove (r.Delete).
func (k *kube) remove(ctx context.Context, r release) error {
	name := r.App + "-" + r.Environment
	dels := []func() error{
		func() error {
			return k.client.AppsV1().Deployments(k.namespace).Delete(ctx, name, metav1.DeleteOptions{})
		},
		func() error { return k.client.CoreV1().Services(k.namespace).Delete(ctx, name, metav1.DeleteOptions{}) },
		func() error {
			return k.client.CoreV1().Secrets(k.namespace).Delete(ctx, name+"-access", metav1.DeleteOptions{})
		},
		func() error {
			return k.client.CoreV1().ConfigMaps(k.namespace).Delete(ctx, name+"-ca", metav1.DeleteOptions{})
		},
	}
	if r.Delete {
		dels = append(dels, func() error {
			return k.client.CoreV1().Secrets(k.namespace).Delete(ctx, name+"-resources", metav1.DeleteOptions{})
		})
	}
	for _, del := range dels {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func envOf(m map[string]string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(m))
	for k, v := range m {
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

// objectClient is the create and update of one kind of object, which every typed client has.
type objectClient[T any] interface {
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
}

// upsert creates obj, or updates it when it exists: a redeploy replaces what the last release ran.
func upsert[T any](ctx context.Context, c objectClient[T], obj T) error {
	_, err := c.Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = c.Update(ctx, obj, metav1.UpdateOptions{})
	}
	return err
}
