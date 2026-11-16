package kube

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/getcanyon-ai/canyon/executor"
)

// doorPort is where the container facing the router listens, and the one port the Service and the
// NetworkPolicy name (invariant 10): canyon-identity for choice 1, oauth2-proxy for choice 2.
const doorPort = 4180

// gatewayPort is Envoy's listener, Canyon's egress gateway (design/EGRESS.md §2.1).
const gatewayPort = 3128

// answeredPort is canyon-identity's /answered, which the executor's pods alone reach (contracts/executor.md,
// What the app answered).
const answeredPort = 4183

// oauth2Proxy is the official image, v7.15.4, pinned by digest per Canyon release.
const oauth2Proxy = "quay.io/oauth2-proxy/oauth2-proxy@sha256:b1b2021fe8f4004573e8d690dec6c7bb29cc44364572cf8510a05bf3a0ae2ded"

// private is every address an app's egress to 80 and 443 leaves out, and Canyon's egress gateway's and every
// build pod's too (deploy/k8s/egress.yaml, deploy/k8s/builds.yaml): no other app, no control plane, no instance metadata — Docker's floor's list
// (deploy/egress/floor.sh), which assumes the cluster's pod and Service addresses are among it.
var private = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
	"0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4", "168.63.129.16/32"}

func labels(app, target string) map[string]string {
	return map[string]string{"canyon.app": app, "canyon.target": target}
}

func meta(name, app, target string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: labels(app, target)}
}

func tcp(port int32) networkingv1.NetworkPolicyPort {
	p, proto := intstr.FromInt32(port), corev1.ProtocolTCP
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

// dnsEgress lets a pod out to DNS: UDP and TCP 53 to the kube-dns pods of kube-system.
func dnsEgress() networkingv1.NetworkPolicyEgressRule {
	udp53, tcp53 := tcp(53), tcp(53)
	udp := corev1.ProtocolUDP
	udp53.Protocol = &udp
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{udp53, tcp53},
	}
}

// publicEgress lets a pod out to every address outside private, on TCP ports.
func publicEgress(ports ...int32) networkingv1.NetworkPolicyEgressRule {
	rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: private}}}}
	for _, p := range ports {
		rule.Ports = append(rule.Ports, tcp(p))
	}
	return rule
}

// gatewayEgress lets a pod out to Canyon's egress gateway alone: TCP 3128 to the canyon-egress pods of
// canyon (contracts/executor.md, On Kubernetes).
func gatewayEgress() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "canyon"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "canyon-egress"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcp(gatewayPort)},
	}
}

// refreshEgress is where a database refresh Job reaches: DNS, TCP 443 at public addresses and, for a
// source outside those, every TCP port at ScanDBCIDRs.
func (e *Executor) refreshEgress() []networkingv1.NetworkPolicyEgressRule {
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgress(), publicEgress(443)}
	if len(e.ScanDBCIDRs) == 0 {
		return egress
	}
	proto := corev1.ProtocolTCP
	rule := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto}}} // no port: every one
	for _, p := range e.ScanDBCIDRs {
		rule.To = append(rule.To, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: p.String()}})
	}
	return append(egress, rule)
}

// networkPolicy admits TCP 4180 from the router's pods and the sidecar's 4183 from the executor's alone,
// and lets the pod out to DNS, the apps database on 5433 and public addresses on 80 and 443 — floored, a
// /run carrying egress, Canyon's egress gateway on 3128 instead, and the sign-in addresses on 443
// (design/EGRESS.md §2.2) — nothing else.
func networkPolicy(app, target string, db []netip.Prefix, floored bool, signin []netip.Prefix) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgress()}
	if len(db) > 0 {
		rule := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(5433)}}
		for _, p := range db {
			rule.To = append(rule.To, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: p.String()}})
		}
		egress = append(egress, rule)
	}
	if floored {
		egress = append(egress, gatewayEgress())
		if len(signin) > 0 {
			rule := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(443)}}
			for _, p := range signin {
				rule.To = append(rule.To, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: p.String()}})
			}
			egress = append(egress, rule)
		}
	} else {
		egress = append(egress, publicEgress(80, 443))
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: meta(app+"-"+target, app, target),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels(app, target)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "canyon"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"canyon.front-door": "true"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{tcp(doorPort)},
			}, {
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "canyon"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "canyon-executor"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{tcp(answeredPort)},
			}},
			Egress: egress,
		},
	}
}

// service is port 4180 to the pod's 4180, the same number on both sides, never 3000.
func service(app, target string) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta(app+"-"+target, app, target),
		Spec: corev1.ServiceSpec{
			Selector: labels(app, target),
			Ports:    []corev1.ServicePort{{Port: doorPort, TargetPort: intstr.FromInt32(doorPort)}},
		},
	}
}

// envSecret holds the /run's env, one key per variable, for the Deployment's life.
func envSecret(app, target string, env map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: meta("canyon-env-"+app+"-"+target, app, target), StringData: maps.Clone(env)}
}

// signinSecret holds what oauth2-proxy alone reads: the client secret and a cookie secret made fresh by
// each /run, so each deploy signs the app's visitors in again.
func signinSecret(app, target, clientSecret string) *corev1.Secret {
	var cookie [32]byte
	_, _ = rand.Read(cookie[:])
	return &corev1.Secret{ObjectMeta: meta("canyon-signin-"+app+"-"+target, app, target), StringData: map[string]string{
		"client_secret": clientSecret, "cookie_secret": base64.URLEncoding.EncodeToString(cookie[:]),
	}}
}

// accessMap is the access file as access.json, its updated_at in the annotation canyon.updated_at.
func accessMap(app, target string, file []byte, updated string) *corev1.ConfigMap {
	m := &corev1.ConfigMap{ObjectMeta: meta("canyon-access-"+app+"-"+target, app, target), Data: map[string]string{"access.json": string(file)}}
	m.Annotations = map[string]string{"canyon.updated_at": updated}
	return m
}

var (
	lockedDown = &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	// sidecarLockedDown is canyon-identity's and oauth2-proxy's: lockedDown, and as their images allow (each
	// runs as user 65532 and writes no file), no root, a read-only root file system and the runtime's seccomp
	// profile. The app, any image an agent built, keeps lockedDown alone.
	sidecarLockedDown = func() *corev1.SecurityContext {
		sc := lockedDown.DeepCopy()
		sc.RunAsNonRoot, sc.ReadOnlyRootFilesystem = new(true), new(true)
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		return sc
	}()
	always = corev1.ContainerRestartPolicyAlways
)

// resources requests a fraction of what the container may use, up to its limits: the scheduler places a
// pod by its requests, which are the limits when unset — every idle app would hold a whole CPU.
func resources(requestCPU, requestMemory, cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(requestCPU), corev1.ResourceMemory: resource.MustParse(requestMemory)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)},
	}
}

// AppCPULimit and AppMemoryLimit are an app container's limits: its requests are set at or below them.
const (
	AppCPULimit    = "1"
	AppMemoryLimit = "1Gi"
)

func value(name, v string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: v} }

func fromSecret(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}}
}

// AppEnv is the app container's environment every /run and every release under run_by: org build
// (contracts/executor.md, /run; design/HAND-OFF.md §3, the deploy release's app_env): PORT and the
// three CANYON_IDENTITY_*, one producer for both modes so a route mode (design/GATEWAY-ROUTES.md)
// changes this function, never a release.
func AppEnv(identity executor.Identity) map[string]string {
	return map[string]string{
		"PORT": "3000", "CANYON_IDENTITY_ISSUER": identity.Issuer,
		"CANYON_IDENTITY_AUDIENCE": identity.Audience, "CANYON_IDENTITY_JWKS_URL": identity.JWKSURL,
	}
}

// SidecarEnv is canyon-identity's environment every /run and every release share: the front door
// composed for this call, the identity issuer and audience, and the target it answers for. A
// release under run_by: org adds its own three keys (CANYON_ACCESS_URL and friends) beside these.
func SidecarEnv(doorJSON []byte, identity executor.Identity, target string) map[string]string {
	return map[string]string{
		"CANYON_FRONT_DOOR": string(doorJSON), "CANYON_IDENTITY_ISSUER": identity.Issuer,
		"CANYON_IDENTITY_AUDIENCE": identity.Audience, "CANYON_TARGET": target,
	}
}

// canyonEgress is a /run's egress.proxy on Kubernetes, exactly: Canyon's egress gateway by the name every
// namespace resolves alike (contracts/executor.md, On Kubernetes).
const canyonEgress = "http://canyon-egress.canyon.svc:3128"

// A floored pod's way out from layer 3 (design/EGRESS.md §4.3; contracts/executor.md, On Kubernetes): its
// containers' proxy is canyon-identity's forwarder on the pod's loopback, which relays to canyonEgress with the
// pod's projected token, audience canyon-egress, minted by the kubelet for the env's own ServiceAccount and
// mounted into canyon-identity alone.
const (
	forwarder       = "http://127.0.0.1:4184"
	egressAudience  = "canyon-egress"
	egressTokenDir  = "/var/run/canyon-egress" //nolint:gosec // where the kubelet writes the token, not one
	egressTokenLife = 600                      // seconds, the least the kubelet mints
)

// serviceAccount is the env's own, which its floored pods run as: bound to nothing, its token mounted nowhere
// but as the projected token the pod spec asks for.
func serviceAccount(app, target string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: meta(executor.ServiceAccount(app, target), app, target), AutomountServiceAccountToken: new(false)}
}

// proxied is what a /run with egress sets beyond env (contracts/executor.md, On Kubernetes): the proxy
// variables, naming the pod's forwarder, as literal values of the app container, so a put-back template carries its own, and
// JDK_JAVA_OPTIONS env's own value by reference — Kubernetes fills $(JDK_JAVA_OPTIONS) from the env Secret,
// where a value env gives stays — then the proxy properties; and the same variables but JDK_JAVA_OPTIONS for
// canyon-identity and oauth2-proxy, their NO_PROXY and no_proxy followed by signin, the hosts they reach
// directly. None without egress. None of them masks what the app said: their values are no secret, and as
// short as 1.
func proxied(req executor.RunRequest, signin []string) (app, sidecars []corev1.EnvVar, err error) {
	if req.Egress == nil {
		return nil, nil, nil
	}
	if req.Egress.Proxy != canyonEgress {
		return nil, nil, refuse("egress.proxy must be %s, Canyon's egress gateway by the name the app's namespace resolves", canyonEgress)
	}
	own := maps.Clone(req.Env)
	delete(own, "JDK_JAVA_OPTIONS")
	vars, err := executor.ProxyEnvTo(forwarder, req.Egress, req.Identity.JWKSURL, own)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(req.Env["JDK_JAVA_OPTIONS"]) != "" {
		vars["JDK_JAVA_OPTIONS"] = "$(JDK_JAVA_OPTIONS) " + vars["JDK_JAVA_OPTIONS"]
	}
	direct := vars["NO_PROXY"]
	for _, h := range signin {
		if !slices.Contains(strings.Split(direct, ","), h) {
			direct += "," + h
		}
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		app = append(app, value(k, vars[k]))
		switch k {
		case "JDK_JAVA_OPTIONS":
		case "NO_PROXY", "no_proxy":
			sidecars = append(sidecars, value(k, direct))
		default:
			sidecars = append(sidecars, value(k, vars[k]))
		}
	}
	return app, sidecars, nil
}

// Direct is one CANYON_KUBE_SIGNIN_DIRECT entry (contracts/executor.md, Sign-in past the gateway): a sign-in
// host a floored pod's sidecars reach past the gateway and, for a provider outside the cluster, its address,
// which the pod's NetworkPolicy admits on 443. Inside the cluster Addr is the zero Prefix: no /32 reaches a
// Service's pods, so the operator's own policy admits them.
type Direct struct {
	Host string
	Addr netip.Prefix
}

// ParseSigninDirect reads CANYON_KUBE_SIGNIN_DIRECT, comma-separated <host> or <host>=<address>/32: the host a
// lower-case DNS name or an IPv4 address, named once — no wildcard, no leading dot, no port — and the address
// one IPv4 address outside link-local, the node's instance metadata. Anything else refuses the whole setting,
// never a part of it; "" is none.
func ParseSigninDirect(v string) ([]Direct, error) {
	if v == "" {
		return nil, nil
	}
	var out []Direct
	for entry := range strings.SplitSeq(v, ",") {
		entry = strings.TrimSpace(entry)
		host, addr, withAddr := strings.Cut(entry, "=")
		d := Direct{Host: host}
		if a, err := netip.ParseAddr(host); !domain.MatchString(host) && (err != nil || !a.Is4()) {
			return nil, fmt.Errorf("%q: the host is not a lower-case DNS name or an IPv4 address", entry)
		}
		if slices.ContainsFunc(out, func(o Direct) bool { return o.Host == host }) {
			return nil, fmt.Errorf("%q: the host is named twice", entry)
		}
		if withAddr {
			p, err := netip.ParsePrefix(addr)
			if err != nil || !p.Addr().Is4() || p.Bits() != 32 || netip.MustParsePrefix("169.254.0.0/16").Contains(p.Addr()) {
				return nil, fmt.Errorf("%q: the address is not one IPv4 address as a /32 outside 169.254.0.0/16", entry)
			}
			d.Addr = p
		}
		out = append(out, d)
	}
	return out, nil
}

// addrs is the addresses SigninDirect names, each once: the floored policy's rule on 443.
func (e *Executor) addrs() []netip.Prefix {
	var out []netip.Prefix
	for _, d := range e.SigninDirect {
		if d.Addr.IsValid() && !slices.Contains(out, d.Addr) {
			out = append(out, d.Addr)
		}
	}
	return out
}

// signinHosts is the front door's sign-in hosts SigninDirect lists, which a floored pod's sidecars reach past
// the gateway (contracts/executor.md, Sign-in past the gateway): of choice 2's issuer, choice 1 oidc's issuer
// and key set, none for alb, the hosts it lists, lower-cased, each once. Any other is reached through the
// gateway, as without the setting. A listed host's URL on a port but 443 is 400: the rule admits 443 alone.
func (e *Executor) signinHosts(d frontDoor) ([]string, error) {
	var urls []string
	switch {
	case d.Signin != nil:
		urls = []string{d.Signin.Issuer}
	case d.Gateway == "oidc":
		urls = []string{d.Issuer, d.JWKSURL}
	}
	var hosts []string
	for _, s := range urls {
		u, err := url.Parse(s)
		if err != nil {
			continue // no host to list, so through the gateway
		}
		h := strings.ToLower(u.Hostname())
		if !slices.ContainsFunc(e.SigninDirect, func(d Direct) bool { return d.Host == h }) {
			continue
		}
		if p := u.Port(); p != "" && p != "443" { // every URL of a listed host, before its second one is skipped
			return nil, refuse("front_door's sign-in host %s is on port %s: CANYON_KUBE_SIGNIN_DIRECT admits 443 alone", h, p)
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}

// deployment runs the app behind canyon-identity and, for choice 2, oauth2-proxy, both native sidecars,
// with proxied's variables: proxy on the app, sidecarProxy on each sidecar, beside their own. run labels the
// pod template, so every /run rolls a new pod that starts with this /run's Secrets, and tells its pods from
// the previous template's; /run's own run is its label canyon.gate_run, which POST /callers answers, and a
// floored pod is labelled canyon.floor, which any other policy of canyon-apps leaves out.
func (e *Executor) deployment(req executor.RunRequest, door frontDoor, doorJSON []byte, run string, proxy, sidecarProxy []corev1.EnvVar) *appsv1.Deployment {
	name := req.App + "-" + req.Target
	pod := labels(req.App, req.Target)
	pod["canyon.run"] = run
	if req.Run != "" {
		pod["canyon.gate_run"] = req.Run
	}
	if req.Egress != nil {
		pod["canyon.floor"] = "true"
	}
	sc := SidecarEnv(doorJSON, req.Identity, req.Target)
	sidecars := []corev1.Container{{
		Name: "canyon-identity", Image: e.Identity, RestartPolicy: &always,
		Env: append([]corev1.EnvVar{
			value("CANYON_FRONT_DOOR", sc["CANYON_FRONT_DOOR"]), value("CANYON_IDENTITY_ISSUER", sc["CANYON_IDENTITY_ISSUER"]),
			value("CANYON_IDENTITY_AUDIENCE", sc["CANYON_IDENTITY_AUDIENCE"]), value("CANYON_TARGET", sc["CANYON_TARGET"]),
			value("CANYON_APP_HOST", door.address(req.App, req.Target)), // /run's alone: where the executor asks what the app answered
		}, sidecarProxy...),
		VolumeMounts:    []corev1.VolumeMount{{Name: "access", MountPath: "/etc/canyon", ReadOnly: true}}, // whole, never subPath: a rewrite reaches it
		SecurityContext: sidecarLockedDown, Resources: resources("10m", "32Mi", "100m", "64Mi"),
		// its last words end the /run's reason, for the server's log; oauth2-proxy, which holds secrets, keeps
		// the default (contracts/executor.md, /run on Kubernetes)
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}}
	if door.Choice == 2 {
		signin := "canyon-signin-" + name
		sidecars = append(sidecars, corev1.Container{
			Name: "oauth2-proxy", Image: cmp.Or(e.OAuth2Proxy, oauth2Proxy), RestartPolicy: &always,
			Args: []string{
				"--provider=oidc", "--oidc-issuer-url=" + door.Signin.Issuer, "--client-id=" + door.Signin.ClientID,
				"--scope=openid email profile offline_access", "--email-domain=*", "--cookie-refresh=4m",
				"--reverse-proxy", "--pass-authorization-header", "--skip-provider-button=true",
				"--redirect-url=https://" + door.address(req.App, req.Target) + "/oauth2/callback",
				"--upstream=http://127.0.0.1:4181", "--http-address=0.0.0.0:4180",
			},
			Env:             append([]corev1.EnvVar{fromSecret("OAUTH2_PROXY_CLIENT_SECRET", signin, "client_secret"), fromSecret("OAUTH2_PROXY_COOKIE_SECRET", signin, "cookie_secret")}, sidecarProxy...),
			SecurityContext: sidecarLockedDown, Resources: resources("10m", "32Mi", "100m", "64Mi"),
		})
	}
	ae := AppEnv(req.Identity)
	app := corev1.Container{
		Name: "app", Image: req.Image,
		Env: append([]corev1.EnvVar{
			value("PORT", ae["PORT"]), value("CANYON_IDENTITY_ISSUER", ae["CANYON_IDENTITY_ISSUER"]),
			value("CANYON_IDENTITY_AUDIENCE", ae["CANYON_IDENTITY_AUDIENCE"]), value("CANYON_IDENTITY_JWKS_URL", ae["CANYON_IDENTITY_JWKS_URL"]),
		}, proxy...),
		EnvFrom:         []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "canyon-env-" + name}}}},
		SecurityContext: lockedDown, Resources: resources(cmp.Or(e.AppCPU, "10m"), cmp.Or(e.AppMemory, "256Mi"), AppCPULimit, AppMemoryLimit),
		// a crash keeps its last words in the pod's status, the tail of its log, which a /run reads when the
		// stopped container's log is already gone (contracts/executor.md, /run on Kubernetes)
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		// ready once something answers on port 3000 at the pod's address: an app that listens elsewhere
		// is never available, and fails the /run with the port reason (contracts/executor.md, /run)
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(3000)}}, PeriodSeconds: 1},
	}
	volumes := []corev1.Volume{{Name: "access", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "canyon-access-" + name}}}}}
	var sa string
	if req.Egress != nil { // the pod proves itself to the gateway with its token, which canyon-identity alone holds
		sa = executor.ServiceAccount(req.App, req.Target)
		id := &sidecars[0]
		id.Env = append(id.Env, value("CANYON_IDENTITY_EGRESS_PROXY", req.Egress.Proxy), value("CANYON_IDENTITY_EGRESS_TOKEN", egressTokenDir+"/token"))
		id.VolumeMounts = append(id.VolumeMounts, corev1.VolumeMount{Name: "egress-token", MountPath: egressTokenDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "egress-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: egressAudience, ExpirationSeconds: new(int64(egressTokenLife)), Path: "token"}}},
		}}})
	}
	if req.DatabaseCA != nil { // the file the app's DATABASE_URL names as its sslrootcert
		app.VolumeMounts = []corev1.VolumeMount{{Name: "database-ca", MountPath: path.Dir(executor.DatabaseCAPath), ReadOnly: true}}
		volumes = append(volumes, corev1.Volume{Name: "database-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "canyon-db-ca"}}}})
	}
	if req.Egress != nil && len(req.Egress.CA) > 0 { // egress.ca (#2143): the trust the app's calls to a route's destination need
		app.VolumeMounts = append(app.VolumeMounts, corev1.VolumeMount{Name: "egress-ca", MountPath: executor.TrustDir, ReadOnly: true}) // whole: a rotation reaches it
		volumes = append(volumes, corev1.Volume{Name: "egress-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: egressCAMap}}}})
		trust := executor.TrustEnv(req.Env)
		for _, k := range slices.Sorted(maps.Keys(trust)) {
			app.Env = append(app.Env, value(k, trust[k]))
		}
	}
	surge, unavailable := intstr.FromInt32(1), intstr.FromInt32(0)
	return &appsv1.Deployment{
		ObjectMeta: meta(name, req.App, req.Target),
		Spec: appsv1.DeploymentSpec{
			Replicas:        new(int32(1)),
			Selector:        &metav1.LabelSelector{MatchLabels: labels(req.App, req.Target)},
			Strategy:        appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}},
			MinReadySeconds: 10, ProgressDeadlineSeconds: new(int32(executor.RunAvailable / time.Second)),
			RevisionHistoryLimit: new(int32(0)), // no earlier ReplicaSet for kubectl rollout undo to start past the gate
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: pod},
				Spec: corev1.PodSpec{
					ServiceAccountName: sa, AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
					InitContainers: sidecars, Containers: []corev1.Container{app}, Volumes: volumes,
				},
			},
		},
	}
}

// egressCAMap is the ConfigMap of a /run's egress.ca, as executor.TrustFiles lays it out: one for the namespace,
// every app's, as the host has one set of roots, written by each /run that carries egress.ca.
const egressCAMap = "canyon-egress-ca"

// databaseCAMap is /run's database_ca as ca.pem: one ConfigMap for the namespace, every app's, since the
// host has one apps database.
func databaseCAMap(ca string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "canyon-db-ca", Namespace: Namespace}, Data: map[string]string{path.Base(executor.DatabaseCAPath): ca}}
}
