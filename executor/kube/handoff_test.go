package kube

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The pod a delivery: repo release commits is /run's pod: the same security contexts, resources, readiness
// probe, restart policy and environment, so a change to /run's pod that the hand-off renderer misses fails
// here (contracts/executor.md, On Kubernetes; design/HAND-OFF.md §3b, One producer).
func TestHandOffSharesTheExecutorsPod(t *testing.T) {
	e, req, _ := fixture(t)
	door, doorJSON, _, err := e.check(req)
	if err != nil {
		t.Fatal(err)
	}
	runDeploy := e.deployment(req, door, doorJSON, "abcd1234", nil, nil)
	run := runDeploy.Spec.Template.Spec
	sc := SidecarEnv(doorJSON, req.Identity, req.Target)
	sc["CANYON_ACCESS_URL"], sc["CANYON_ACCESS_GRACE"] = "https://canyon.example/access/"+req.App+"/"+req.Target, "1h"
	handDeploy := HandOffDeployment(HandOffPod{App: req.App, Env: req.Target, Namespace: "canyon-" + req.App + "-" + req.Target, Image: req.Image, Identity: e.Identity,
		AppEnv: AppEnv(req.Identity), SidecarEnv: sc, AccessSecret: "a", VarsSecret: "v", Variables: []string{"STRIPE_SECRET_KEY"}})
	handed := handDeploy.Spec.Template.Spec

	app, side := run.Containers[0], run.InitContainers[0]
	hApp, hSide := handed.Containers[0], handed.InitContainers[0]
	for name, pair := range map[string][2]any{
		"the app's security context":     {app.SecurityContext, hApp.SecurityContext},
		"the app's readiness probe":      {app.ReadinessProbe, hApp.ReadinessProbe},
		"the app's limits":               {app.Resources.Limits, hApp.Resources.Limits},
		"the app's requests":             {app.Resources.Requests, hApp.Resources.Requests},
		"the rollout's strategy":         {runDeploy.Spec.Strategy, handDeploy.Spec.Strategy},
		"the rollout's minReadySeconds":  {runDeploy.Spec.MinReadySeconds, handDeploy.Spec.MinReadySeconds},
		"the rollout's history limit":    {runDeploy.Spec.RevisionHistoryLimit, handDeploy.Spec.RevisionHistoryLimit},
		"the rollout's deadline":         {runDeploy.Spec.ProgressDeadlineSeconds, handDeploy.Spec.ProgressDeadlineSeconds},
		"the app's termination message":  {app.TerminationMessagePolicy, hApp.TerminationMessagePolicy},
		"the sidecar's name":             {side.Name, hSide.Name},
		"the sidecar's image":            {side.Image, hSide.Image},
		"the sidecar's restart policy":   {side.RestartPolicy, hSide.RestartPolicy},
		"the sidecar's security context": {side.SecurityContext, hSide.SecurityContext},
		"the sidecar's resources":        {side.Resources, hSide.Resources},
		"the pod's service account":      {run.AutomountServiceAccountToken, handed.AutomountServiceAccountToken},
		"the pod's service links":        {run.EnableServiceLinks, handed.EnableServiceLinks},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: /run has %+v, the hand-off pod %+v", name, pair[0], pair[1])
		}
	}
	values := func(env []corev1.EnvVar) map[string]string {
		m := map[string]string{}
		for _, v := range env {
			if v.ValueFrom == nil {
				m[v.Name] = v.Value
			}
		}
		return m
	}
	runApp, handApp := values(app.Env), values(hApp.Env)
	for k, v := range runApp {
		if handApp[k] != v {
			t.Errorf("the app's %s: /run sets %q, the hand-off pod %q", k, v, handApp[k])
		}
	}
	runSide, handSide := values(side.Env), values(hSide.Env)
	for k, v := range runSide {
		if k == "CANYON_APP_HOST" { // /run's alone: where the executor asks what the app answered
			continue
		}
		if handSide[k] != v {
			t.Errorf("the sidecar's %s: /run sets %q, the hand-off pod %q", k, v, handSide[k])
		}
	}
}

// The hand-off pod holds no value: the access token, each variable and each resource's variables come from
// Secrets by name, and a missing one never starts the app.
func TestHandOffPodHoldsNoValue(t *testing.T) {
	d := HandOffDeployment(HandOffPod{App: "claims-portal", Env: "preview", Namespace: "canyon-claims-portal-preview",
		AppEnv: map[string]string{"PORT": "3000"}, SidecarEnv: map[string]string{"CANYON_TARGET": "preview"},
		AccessSecret: "claims-portal-preview-access-8f2a", VarsSecret: "claims-portal-preview-vars", Variables: []string{"STRIPE_SECRET_KEY"},
		Gives: []Give{{Name: "DATABASE_URL", Secret: "claims-portal-preview-res-db"}}})
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(d.Spec.Template.Spec.InitContainers, d.Spec.Template.Spec.Containers...) {
		for _, v := range c.Env {
			if strings.HasPrefix(v.Name, "CANYON_ACCESS_TOKEN") || v.Name == "STRIPE_SECRET_KEY" || v.Name == "DATABASE_URL" {
				if v.Value != "" || v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil || v.ValueFrom.SecretKeyRef.Optional != nil {
					t.Errorf("%s in %s is not read from a Secret that must exist: %s", v.Name, c.Name, raw)
				}
			}
		}
	}
}
