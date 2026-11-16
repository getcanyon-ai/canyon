package kube

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getcanyon-ai/canyon/executor"
)

const identityImage = "ghcr.io/getcanyon-ai/canyon-identity@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The two choice-1 front doors, each with its own fields.
const (
	albDoor  = `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"canyon.meridian.example","form":"app.env.domain"}`
	oidcDoor = `{"choice":1,"gateway":"oidc","issuer":"https://gateway.meridian.example","audience":"canyon-gateway","jwks_url":"https://gateway.meridian.example/jwks","cookie":"_pomerium","domain":"canyon.meridian.example","form":"app.env.domain"}`
)

// fixture is contracts/testdata/executor/claims-portal.json's Kubernetes half: the /run request, and
// the Service and NetworkPolicy the contract says it writes.
func fixture(t *testing.T) (*Executor, executor.RunRequest, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Req     executor.RunRequest        `json:"kube_run_request"`
		Objects map[string]json.RawMessage `json:"kube_objects"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Registry: "123456789012.dkr.ecr.eu-central-1.amazonaws.com", DBCIDRs: []netip.Prefix{netip.MustParsePrefix("10.20.48.0/20")}, Identity: identityImage,
		Arch: func(context.Context, string) (string, error) { return "amd64", nil }} // the fixture's images are in no registry (run_arch_test.go reads one)
	return e, f.Req, f.Objects
}

// asJSON is v as the generic JSON value it marshals to, so a typed object and the fixture compare.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Invariant 10 and 11 (contracts/executor.md): the Service and NetworkPolicy the fixture's
// kube_run_request makes are the fixture's kube_objects — kind, name, namespace and the whole spec:
// 4180 alone in, and out only DNS, 5433 to the database ranges and 80/443 to public addresses. The
// access ConfigMap holds the file whole, its updated_at beside it.
func TestRunWritesTheContractObjects(t *testing.T) {
	e, req, want := fixture(t)
	if _, _, _, err := e.check(req); err != nil {
		t.Fatalf("the fixture's request is refused: %v", err)
	}
	for key, got := range map[string]any{"service": service(req.App, req.Target), "network_policy": networkPolicy(req.App, req.Target, e.DBCIDRs, false, nil)} {
		var w map[string]any
		if err := json.Unmarshal(want[key], &w); err != nil {
			t.Fatal(err)
		}
		g := asJSON(t, got)
		gm, wm := g["metadata"].(map[string]any), w["metadata"].(map[string]any)
		if g["apiVersion"] != w["apiVersion"] || g["kind"] != w["kind"] || gm["name"] != wm["name"] || gm["namespace"] != wm["namespace"] || !reflect.DeepEqual(g["spec"], w["spec"]) {
			gs, _ := json.Marshal(g["spec"])
			ws, _ := json.Marshal(w["spec"])
			t.Errorf("%s:\n got %s\nwant %s", key, gs, ws)
		}
	}
	m := accessMap(req.App, req.Target, req.Access, "2026-09-25T10:00:00Z")
	if m.Name != "canyon-access-claims-portal-preview" || m.Data["access.json"] != string(req.Access) || m.Annotations["canyon.updated_at"] != "2026-09-25T10:00:00Z" || !maps.Equal(m.Labels, labels(req.App, req.Target)) {
		t.Errorf("access ConfigMap %+v", m.ObjectMeta)
	}
	if req.DatabaseCA == nil {
		t.Fatal("the fixture's request carries no database_ca")
	}
	if ca := databaseCAMap(*req.DatabaseCA); ca.Name != "canyon-db-ca" || ca.Namespace != Namespace || !maps.Equal(ca.Data, map[string]string{"ca.pem": *req.DatabaseCA}) {
		t.Errorf("database CA ConfigMap %+v %v", ca.ObjectMeta, ca.Data)
	}
}

// Every refusal the contract names is a 400 (ErrRequest), decided before anything is written.
func TestCheckRefusesWhatTheContractRefuses(t *testing.T) {
	e, good, _ := fixture(t)
	from := func(base string, edit func(map[string]any)) func(*executor.RunRequest) {
		return func(r *executor.RunRequest) {
			var m map[string]any
			_ = json.Unmarshal([]byte(base), &m)
			edit(m)
			r.FrontDoor, _ = json.Marshal(m)
		}
	}
	door := func(edit func(map[string]any)) func(*executor.RunRequest) { return from(string(good.FrontDoor), edit) }
	for _, base := range []string{string(good.FrontDoor), albDoor, oidcDoor} {
		req := good
		req.FrontDoor = []byte(base)
		if _, _, _, err := e.check(req); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
	}
	noCA := good
	noCA.DatabaseCA = nil
	if _, _, _, err := e.check(noCA); good.DatabaseCA == nil || err != nil {
		t.Fatalf("database_ca is optional: the fixture carries one, and a /run without it is not refused (%v)", err)
	}
	app := func(name string) func(*executor.RunRequest) { // only the app is wrong: its network and image follow it
		return func(r *executor.RunRequest) {
			r.Image = strings.Replace(r.Image, "/canyon/claims-portal@", "/canyon/"+name+"@", 1)
			r.App, r.Network = name, "canyon-app-"+name
		}
	}
	for name, edit := range map[string]func(*executor.RunRequest){
		"an app with _":        app("claims_portal"),
		"an app of 56":         app(strings.Repeat("a", 56)),
		"target staging":       func(r *executor.RunRequest) { r.Target = "staging" },
		"another network":      func(r *executor.RunRequest) { r.Network = "canyon-app-other" },
		"the server's key set": func(r *executor.RunRequest) { r.Identity.JWKSURL = "http://canyon-server:7780/.well-known/jwks.json" },
		"no access":            func(r *executor.RunRequest) { r.Access = nil },
		"no front_door":        func(r *executor.RunRequest) { r.FrontDoor = nil },
		"a tag":                func(r *executor.RunRequest) { r.Image = e.Registry + "/canyon/claims-portal:latest" },
		"another registry":     func(r *executor.RunRequest) { r.Image = strings.Replace(r.Image, e.Registry, "docker.io", 1) },
		"another app's image": func(r *executor.RunRequest) {
			r.Image = strings.Replace(r.Image, "/canyon/claims-portal@", "/canyon/other@", 1)
		},
		"env PORT":                func(r *executor.RunRequest) { r.Env = map[string]string{"PORT": "8080"} },
		"env CANYON_IDENTITY_":    func(r *executor.RunRequest) { r.Env = map[string]string{"CANYON_IDENTITY_ISSUER": "x"} },
		"env with a dash":         func(r *executor.RunRequest) { r.Env = map[string]string{"A-B": "x"} },
		"a NUL value":             func(r *executor.RunRequest) { r.Env = map[string]string{"A": "a\x00b"} },
		"a value over 64 KiB":     func(r *executor.RunRequest) { r.Env = map[string]string{"A": strings.Repeat("x", 64<<10+1)} },
		"access with another key": func(r *executor.RunRequest) { r.Access = []byte(`{"updated_at":"2026-09-25T10:00:00Z","extra":1}`) },
		"access with fractional seconds": func(r *executor.RunRequest) {
			r.Access = []byte(strings.Replace(string(r.Access), "10:00:00Z", "10:00:00.5Z", 1))
		},
		"front_door with another key": door(func(m map[string]any) { m["extra"] = 1 }),
		"a domain under preview":      door(func(m map[string]any) { m["domain"] = "preview.meridian.example" }),
		"an upper-case domain":        door(func(m map[string]any) { m["domain"] = "Canyon.meridian.example" }),
		"a third form":                door(func(m map[string]any) { m["form"] = "env.app.domain" }),
		"choice 3":                    door(func(m map[string]any) { m["choice"] = 3 }),
		"choice 2 without signin":     door(func(m map[string]any) { delete(m, "signin") }),
		"choice 2 with a gateway":     door(func(m map[string]any) { m["gateway"] = "oidc" }),
		"choice 1 with signin":        door(func(m map[string]any) { m["choice"], m["gateway"] = 1, "oidc" }),
		"choice 1 without a gateway":  door(func(m map[string]any) { m["choice"] = 1; delete(m, "signin") }),
		"choice 2 with a cookie":      door(func(m map[string]any) { m["cookie"] = "_pomerium" }),
		"gateway foo":                 from(albDoor, func(m map[string]any) { m["gateway"] = "foo" }),
		"alb without an arn":          from(albDoor, func(m map[string]any) { delete(m, "alb_arn") }),
		"alb with an issuer":          from(albDoor, func(m map[string]any) { m["issuer"] = "https://gateway.meridian.example" }),
		"oidc without a cookie":       from(oidcDoor, func(m map[string]any) { delete(m, "cookie") }),
		"oidc with an alb_arn": from(oidcDoor, func(m map[string]any) {
			m["alb_arn"] = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef"
		}),
		"a signin issuer over http": door(func(m map[string]any) {
			m["signin"].(map[string]any)["issuer"] = "http://login.meridian.example"
		}),
		"egress whose proxy is not on 3128": func(r *executor.RunRequest) {
			r.Egress = &executor.Egress{Proxy: "http://canyon-egress.canyon.svc:8080", NoProxy: []string{}}
		},
		"egress beside an env naming HTTPS_PROXY": func(r *executor.RunRequest) {
			r.Egress, r.Env["HTTPS_PROXY"] = &executor.Egress{Proxy: "http://canyon-egress.canyon.svc:3128", NoProxy: []string{}}, "http://elsewhere:3128"
		},
		"a run that is no run id":       func(r *executor.RunRequest) { r.Run = "run-1" },
		"a database CA that is not PEM": func(r *executor.RunRequest) { r.DatabaseCA = new("apps-db.meridian.example CA") },
		"an empty database CA":          func(r *executor.RunRequest) { r.DatabaseCA = new("") },
		"text around the database CA":   func(r *executor.RunRequest) { r.DatabaseCA = new("# the RDS bundle\n" + *r.DatabaseCA) },
		"a database CA over 256 KiB": func(r *executor.RunRequest) {
			r.DatabaseCA = new(strings.Repeat(*r.DatabaseCA, 256<<10/len(*r.DatabaseCA)+1))
		},
		"a public key as the database CA": func(r *executor.RunRequest) {
			r.DatabaseCA = new("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n")
		},
	} {
		req := good
		req.Env, req.Access, req.FrontDoor = maps.Clone(good.Env), slices.Clone(good.Access), slices.Clone(good.FrontDoor)
		edit(&req)
		if _, _, _, err := e.check(req); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

// Invariant 2: the env's values are in the env Secret alone and the client secret in the signin
// Secret alone — never in the Deployment, which the executor's Roles read back, nor in the front door
// the sidecar is handed.
func TestTheDeploymentHoldsNoValue(t *testing.T) {
	e, req, _ := fixture(t)
	req.Env = map[string]string{"FIRST": "env-value-1", "SECOND": "env-value-2"}
	var fd map[string]any
	_ = json.Unmarshal(req.FrontDoor, &fd)
	fd["signin"].(map[string]any)["client_secret"] = "client-secret-value"
	req.FrontDoor, _ = json.Marshal(fd)
	door, doorJSON, _, err := e.check(req)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := json.Marshal(e.deployment(req, door, doorJSON, "abcd1234", nil, nil))
	for _, v := range []string{"env-value-1", "env-value-2", "client-secret-value"} {
		if strings.Contains(string(d), v) {
			t.Errorf("the Deployment carries %q", v)
		}
	}
	if strings.Contains(string(doorJSON), "client_secret") {
		t.Errorf("CANYON_FRONT_DOOR carries the client secret: %s", doorJSON)
	}
	if s := envSecret(req.App, req.Target, req.Env); !maps.Equal(s.StringData, req.Env) || s.Name != "canyon-env-claims-portal-preview" {
		t.Errorf("env Secret %s holds %v", s.Name, s.StringData)
	}
	if s := signinSecret(req.App, req.Target, door.Signin.ClientSecret); s.StringData["client_secret"] != "client-secret-value" || len(s.StringData["cookie_secret"]) != 44 {
		t.Errorf("signin Secret holds %v", s.StringData)
	}
}

// TestEveryPodRequestsLessThanItsLimits: every container the executor writes requests CPU and memory,
// each below its limit. The scheduler places a pod by its requests, which are the limits when unset: on
// kind an app asking for its whole CPU found no node, and on any cluster every idle app would hold one.
func TestEveryPodRequestsLessThanItsLimits(t *testing.T) {
	e, req, _ := fixture(t)
	door, doorJSON, _, err := e.check(req) // choice 2: both sidecars
	if err != nil {
		t.Fatal(err)
	}
	trivy, _ := executor.ToolOf("trivy")
	grype, _ := executor.ToolOf("grype")
	specs := map[string]corev1.PodSpec{"the app": e.deployment(req, door, doorJSON, "abcd1234", nil, nil).Spec.Template.Spec}
	for _, j := range []*batchv1.Job{e.probeJob("target", "r", target, nil), e.probeJob("prober", "r", prober, nil),
		e.scanJob("trivy", "r", trivy.Image, trivy, resource.MustParse(scanDisk)), e.readerJob("trivy", "r", trivy.Image, trivy), e.refreshJob("trivy", "r", trivy), e.refreshJob("grype", "r", grype)} {
		specs[j.Name] = j.Spec.Template.Spec
	}
	for name, spec := range specs {
		for _, c := range append(spec.InitContainers, spec.Containers...) {
			for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				request, limit := c.Resources.Requests[r], c.Resources.Limits[r]
				if request.IsZero() || request.Cmp(limit) >= 0 {
					t.Errorf("%s, %s: requests %s %s against a limit of %s", name, c.Name, request.String(), r, limit.String())
				}
			}
		}
	}
}

// The probe's Jobs run under the runtime's seccomp profile, as no root (#2368).
func TestProbeJobsRunUnderTheRuntimesSeccompProfile(t *testing.T) {
	e, _, _ := fixture(t)
	for _, role := range []string{"target", "canary", "prober"} {
		sc := e.probeJob(role, "r", target, nil).Spec.Template.Spec.SecurityContext
		if sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("the probe's %s: %+v, want no root under RuntimeDefault", role, sc)
		}
	}
}

// The Deployment keeps no earlier ReplicaSet for kubectl rollout undo to start past the gate.
func TestAppDeploymentKeepsNoRevisions(t *testing.T) {
	e, req, _ := fixture(t)
	door, doorJSON, _, err := e.check(req)
	if err != nil {
		t.Fatal(err)
	}
	if limit := e.deployment(req, door, doorJSON, "abcd1234", nil, nil).Spec.RevisionHistoryLimit; limit == nil || *limit != 0 {
		t.Fatalf("revisionHistoryLimit %v, want 0", limit)
	}
}

// The pod is the contract's: native sidecars pinned by digest in front of the app, which gets PORT, the
// three CANYON_IDENTITY_* and the env Secret and nothing else; every capability dropped; no token, no
// service links; the access file mounted whole; the rollout one pod at a time with the old one serving.
func TestTheDeploymentIsTheContractsPod(t *testing.T) {
	e, req, _ := fixture(t)
	for _, tc := range []struct {
		name, door string
		sidecars   []string
		redirect   string
	}{
		{"choice 2", string(req.FrontDoor), []string{"canyon-identity", "oauth2-proxy"}, "--redirect-url=https://claims-portal.preview.canyon.meridian.example/oauth2/callback"},
		{"choice 2, app-env.domain", strings.Replace(string(req.FrontDoor), "app.env.domain", "app-env.domain", 1), []string{"canyon-identity", "oauth2-proxy"}, "--redirect-url=https://claims-portal-preview.canyon.meridian.example/oauth2/callback"},
		{"choice 1, alb", albDoor, []string{"canyon-identity"}, ""},
		{"choice 1, oidc", oidcDoor, []string{"canyon-identity"}, ""},
	} {
		r := req
		r.FrontDoor = []byte(tc.door)
		door, doorJSON, _, err := e.check(r)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d := e.deployment(r, door, doorJSON, "abcd1234", nil, nil)
		s, pod := d.Spec, d.Spec.Template.Spec
		if *s.Replicas != 1 || s.MinReadySeconds != 10 || *s.ProgressDeadlineSeconds != 300 || s.Strategy.RollingUpdate.MaxSurge.IntValue() != 1 || s.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
			t.Errorf("%s: rollout %+v", tc.name, s.Strategy)
		}
		if d.Spec.Template.Labels["canyon.run"] != "abcd1234" || d.Spec.Template.Labels["canyon.app"] != "claims-portal" || d.Spec.Template.Labels["canyon.target"] != "preview" {
			t.Errorf("%s: pod labels %v", tc.name, d.Spec.Template.Labels)
		}
		if *pod.AutomountServiceAccountToken || *pod.EnableServiceLinks || pod.HostNetwork || pod.ShareProcessNamespace != nil {
			t.Errorf("%s: pod %+v", tc.name, pod)
		}
		var names []string
		for _, c := range pod.InitContainers {
			names = append(names, c.Name)
			if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways || !strings.Contains(c.Image, "@sha256:") {
				t.Errorf("%s: %s is not a native sidecar pinned by digest", tc.name, c.Name)
			}
			if want := map[string]corev1.TerminationMessagePolicy{"canyon-identity": corev1.TerminationMessageFallbackToLogsOnError}[c.Name]; c.TerminationMessagePolicy != want { // canyon-identity's last words outlive it; oauth2-proxy holds secrets (#1790)
				t.Errorf("%s: %s terminationMessagePolicy %q", tc.name, c.Name, c.TerminationMessagePolicy)
			}
			if c.Name != "oauth2-proxy" {
				continue
			}
			args := []string{"--provider=oidc", "--oidc-issuer-url=https://login.meridian.example", "--client-id=canyon-apps",
				"--scope=openid email profile offline_access", "--email-domain=*", "--cookie-refresh=4m", "--reverse-proxy", "--pass-authorization-header",
				"--skip-provider-button=true", tc.redirect, "--upstream=http://127.0.0.1:4181", "--http-address=0.0.0.0:4180"}
			if !slices.Equal(c.Args, args) {
				t.Errorf("%s: oauth2-proxy args %v", tc.name, c.Args)
			}
			for i, name := range []string{"OAUTH2_PROXY_CLIENT_SECRET", "OAUTH2_PROXY_COOKIE_SECRET"} {
				ref := c.Env[i].ValueFrom.SecretKeyRef
				if len(c.Env) != 2 || c.Env[i].Name != name || ref.Name != "canyon-signin-claims-portal-preview" || ref.Key != strings.ToLower(strings.TrimPrefix(name, "OAUTH2_PROXY_")) {
					t.Errorf("%s: oauth2-proxy env %+v", tc.name, c.Env)
				}
			}
		}
		if !slices.Equal(names, tc.sidecars) {
			t.Errorf("%s: sidecars %v, want %v", tc.name, names, tc.sidecars)
		}
		id := pod.InitContainers[0]
		if m := id.VolumeMounts; len(m) != 1 || m[0].MountPath != "/etc/canyon" || m[0].SubPath != "" || pod.Volumes[0].ConfigMap.Name != "canyon-access-claims-portal-preview" {
			t.Errorf("%s: access mounted %+v from %+v", tc.name, m, pod.Volumes)
		}
		var idEnv []string
		for _, v := range id.Env {
			idEnv = append(idEnv, v.Name+"="+v.Value)
		}
		host := "claims-portal.preview.canyon.meridian.example" // the address the executor's ask names (contracts/executor.md, What the app answered)
		if strings.Contains(string(doorJSON), "app-env.domain") {
			host = "claims-portal-preview.canyon.meridian.example"
		}
		if want := []string{"CANYON_FRONT_DOOR=" + string(doorJSON), "CANYON_IDENTITY_ISSUER=https://canyon.meridian.example", "CANYON_IDENTITY_AUDIENCE=claims-portal", "CANYON_TARGET=preview", "CANYON_APP_HOST=" + host}; !slices.Equal(idEnv, want) {
			t.Errorf("%s: canyon-identity env %v", tc.name, idEnv)
		}
		var sidecar map[string]any
		if err := json.Unmarshal(doorJSON, &sidecar); err != nil || sidecar["domain"] != "canyon.meridian.example" || sidecar["choice"] != float64(door.Choice) {
			t.Errorf("%s: CANYON_FRONT_DOOR %s", tc.name, doorJSON)
		}
		app := pod.Containers[0]
		var env []string
		for _, v := range app.Env {
			env = append(env, v.Name+"="+v.Value)
		}
		wantEnv := []string{"PORT=3000", "CANYON_IDENTITY_ISSUER=https://canyon.meridian.example", "CANYON_IDENTITY_AUDIENCE=claims-portal", "CANYON_IDENTITY_JWKS_URL=" + jwksURL}
		if len(pod.Containers) != 1 || app.Name != "app" || app.Image != r.Image || !slices.Equal(env, wantEnv) || app.EnvFrom[0].SecretRef.Name != "canyon-env-claims-portal-preview" || len(app.EnvFrom) != 1 {
			t.Errorf("%s: app %s %v %+v", tc.name, app.Image, env, app.EnvFrom)
		}
		if p := app.ReadinessProbe; p == nil || p.TCPSocket == nil || p.TCPSocket.Port.IntValue() != 3000 || p.PeriodSeconds != 1 || p.Exec != nil || p.HTTPGet != nil {
			t.Errorf("%s: the app is ready once port 3000 answers a TCP connect: %+v", tc.name, p)
		}
		for _, c := range pod.InitContainers {
			if c.ReadinessProbe != nil {
				t.Errorf("%s: sidecar %s has a readiness probe: %+v", tc.name, c.Name, c.ReadinessProbe)
			}
		}
		if app.Resources.Limits.Cpu().String() != "1" || app.Resources.Limits.Memory().String() != "1Gi" || id.Resources.Limits.Cpu().String() != "100m" || id.Resources.Limits.Memory().String() != "64Mi" {
			t.Errorf("%s: limits %v %v", tc.name, app.Resources.Limits, id.Resources.Limits)
		}
		if app.Resources.Requests.Cpu().String() != "10m" || app.Resources.Requests.Memory().String() != "256Mi" { // unset: the measured defaults (#1637)
			t.Errorf("%s: the app requests %v", tc.name, app.Resources.Requests)
		}
		// The database CA the fixture carries, at the path its DATABASE_URL names; none without one.
		if m := app.VolumeMounts; len(m) != 1 || m[0].MountPath+"/ca.pem" != executor.DatabaseCAPath || !m[0].ReadOnly || m[0].SubPath != "" ||
			len(pod.Volumes) != 2 || pod.Volumes[1].Name != m[0].Name || pod.Volumes[1].ConfigMap.Name != "canyon-db-ca" {
			t.Errorf("%s: the app mounts %+v from %+v; want canyon-db-ca read-only at /etc/canyon-db", tc.name, m, pod.Volumes)
		}
		r.DatabaseCA = nil
		if bare := e.deployment(r, door, doorJSON, "abcd1234", nil, nil).Spec.Template.Spec; len(bare.Containers[0].VolumeMounts) != 0 || len(bare.Volumes) != 1 {
			t.Errorf("%s: a /run without database_ca mounts %+v", tc.name, bare.Containers[0].VolumeMounts)
		}
		for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
			if sc := c.SecurityContext; sc == nil || *sc.AllowPrivilegeEscalation || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(c.Ports) > 0 {
				t.Errorf("%s: %s security context %+v, ports %v", tc.name, c.Name, sc, c.Ports)
			}
		}
		// The sidecars, Canyon's images, run as no root, on a read-only root file system and under the
		// runtime's seccomp profile; the app, any image an agent built, keeps what it had.
		for _, c := range pod.InitContainers {
			if sc := c.SecurityContext; sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
				sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("%s: sidecar %s security context %+v", tc.name, c.Name, sc)
			}
		}
		if sc := pod.Containers[0].SecurityContext; sc.RunAsNonRoot != nil || sc.ReadOnlyRootFilesystem != nil || sc.SeccompProfile != nil {
			t.Errorf("%s: the app's security context %+v changed", tc.name, sc)
		}
	}
}
