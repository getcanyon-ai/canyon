package kube

import (
	"maps"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/getcanyon-ai/canyon/executor"
)

// HandOffPod is the pod a delivery: repo release commits to the org's deploy repo (design/HAND-OFF.md §3b;
// contracts/routes.md, The deploy repo): the app and canyon-identity as /run lays them out, from the same
// security contexts, resources, readiness probe and environment producers, with the access token and every
// value the app reads coming from Secrets the org's platform makes, never from Canyon.
type HandOffPod struct {
	App, Env, Namespace string
	Image, Identity     string            // the app's image and the sidecar's, each by digest
	Arch                string            // the app image's architecture, the release's platform: its nodes; "" pins nothing (#2528)
	AppEnv, SidecarEnv  map[string]string // the release's app_env (AppEnv) and sidecar env (SidecarEnv plus the pull's)
	AccessSecret        string            // the Secret holding CANYON_ACCESS_TOKEN
	VarsSecret          string            // the Secret holding each of Variables
	Variables           []string
	Gives               []Give            // each resource's variables, from its own Secret
	CA                  bool              // ConfigMap canyon-ca mounted for the sidecar, which then trusts it beside the system's
	Annotations         map[string]string // on the pod template, so every release rolls the pod
}

// Give is one variable a resource gives, from the Secret its template makes.
type Give struct{ Name, Secret string }

// handOffCA is where the sidecar reads Canyon's CA: SSL_CERT_DIR adds it to the system's roots, never in place of them.
const handOffCA = "/etc/canyon/ca"

// HandOffDeployment is the release's Deployment: one replica, rolled one pod at a time, no ReplicaSet kept
// to roll back to past the gate, the sidecar a native init container as /run's is.
func HandOffDeployment(p HandOffPod) *appsv1.Deployment {
	side := []corev1.EnvVar{}
	for _, k := range slices.Sorted(maps.Keys(p.SidecarEnv)) {
		side = append(side, value(k, p.SidecarEnv[k]))
	}
	side = append(side, fromSecret("CANYON_ACCESS_TOKEN", p.AccessSecret, "CANYON_ACCESS_TOKEN"))
	mounts := []corev1.VolumeMount{{Name: "access-cache", MountPath: "/var/cache/canyon"}}
	volumes := []corev1.Volume{{Name: "access-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if p.CA {
		side = append(side, value("SSL_CERT_DIR", handOffCA))
		mounts = append(mounts, corev1.VolumeMount{Name: "canyon-ca", MountPath: handOffCA, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "canyon-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "canyon-ca"}}}})
	}
	sidecar := corev1.Container{
		Name: "canyon-identity", Image: p.Identity, RestartPolicy: &always, Env: side, VolumeMounts: mounts,
		SecurityContext: sidecarLockedDown, Resources: resources("10m", "32Mi", "100m", "64Mi"),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	env := []corev1.EnvVar{value("PORT", p.AppEnv["PORT"]), value("CANYON_IDENTITY_ISSUER", p.AppEnv["CANYON_IDENTITY_ISSUER"]),
		value("CANYON_IDENTITY_AUDIENCE", p.AppEnv["CANYON_IDENTITY_AUDIENCE"]), value("CANYON_IDENTITY_JWKS_URL", p.AppEnv["CANYON_IDENTITY_JWKS_URL"])}
	for _, name := range p.Variables {
		env = append(env, fromSecret(name, p.VarsSecret, name))
	}
	for _, g := range p.Gives {
		env = append(env, fromSecret(g.Name, g.Secret, g.Name))
	}
	app := corev1.Container{
		Name: "app", Image: p.Image, Env: env,
		SecurityContext: lockedDown, Resources: resources("10m", "256Mi", AppCPULimit, AppMemoryLimit),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		ReadinessProbe:           &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(3000)}}, PeriodSeconds: 1},
	}
	surge, unavailable := intstr.FromInt32(1), intstr.FromInt32(0)
	var nodes map[string]string
	if p.Arch != "" { // a release journaled before it named a platform pins nothing, as before (#2528)
		nodes = map[string]string{"kubernetes.io/arch": p.Arch} // where its image starts, as a /run pins its pod
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: handOffMeta(p.App, p.Env, p.Namespace),
		Spec: appsv1.DeploymentSpec{
			Replicas:        new(int32(1)),
			Selector:        &metav1.LabelSelector{MatchLabels: labels(p.App, p.Env)},
			Strategy:        appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}},
			MinReadySeconds: 10, ProgressDeadlineSeconds: new(int32(executor.RunAvailable / time.Second)),
			RevisionHistoryLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(p.App, p.Env), Annotations: maps.Clone(p.Annotations)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
					InitContainers: []corev1.Container{sidecar}, Containers: []corev1.Container{app}, Volumes: volumes,
					NodeSelector: nodes,
				},
			},
		},
	}
}

// HandOffService is 4180 to the pod's 4180, the sidecar, as service's is.
func HandOffService(app, env, namespace string) *corev1.Service {
	s := service(app, env)
	s.ObjectMeta = handOffMeta(app, env, namespace)
	return s
}

// HandOffNetworkPolicy admits TCP 4180 alone into the pod, from anywhere: the org's gateway is the org's, and
// so is every egress rule.
func HandOffNetworkPolicy(app, env, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: handOffMeta(app, env, namespace),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels(app, env)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{tcp(doorPort)}}},
		},
	}
}

func handOffMeta(app, env, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: app + "-" + env, Namespace: namespace, Labels: labels(app, env)}
}
