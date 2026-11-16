package kube

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// probeImage is the probe's busybox, 1.37.0's multi-arch index, pinned by digest per Canyon release.
const probeImage = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// ProbeResult is POST /probe's answer (contracts/schema/executor.schema.json, probe_result).
type ProbeResult struct {
	NetworkPolicy string `json:"network_policy"` // enforced | not_enforced
	Certificate   string `json:"certificate"`    // ok | missing
	Fix           string `json:"fix"`
	Warning       string `json:"warning"`
}

// The not_enforced fixes: 4180 connected, the canary did.
const (
	notEnforcedFix = `This cluster does not enforce NetworkPolicy, so an app's port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
	lateFix        = `New pods on this cluster get out before their NetworkPolicy is in force, so an app could reach anything in its first moments. Have the network plugin apply a pod's policy before the pod starts (on EKS: NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
)

// nodePorts are what the prober tries on its own node: the kubelet, which every node runs, and sshd.
var nodePorts = []string{"10250", "22"}

// nodeWarning is the warning for the node ports that connected, in the order tried, joined by ", ".
// A reachable node never refuses the front door: closing it is the operator's host rule
// (deploy/k8s/README.md, Host firewall).
func nodeWarning(ports string) string {
	return "Apps on this cluster can reach the server they run on (ports " + ports + "). Your cluster admin can close this with a host firewall rule."
}

// probing lets one probe run at a time; a second waits.
var probing sync.Mutex

// target serves HTTP on 4180, where an app's sidecar listens, and on 8080, which only the prober may reach.
const target = `mkdir -p /tmp/w && echo ok > /tmp/w/index.html && httpd -p 4180 -h /tmp/w && exec httpd -f -p 8080 -h /tmp/w`

// canary serves HTTP on 8080 under no policy of its own, which the prober's policy does not let it reach.
const canary = `mkdir -p /tmp/w && exec httpd -f -p 8080 -h /tmp/w`

// prober first tries the canary's 8080 back to back for 5 s, then reaches the target's 8080 within 30 s,
// then tries pairs for 15 s, 8080 then 4180, then its node's 10250 and 22 once each, writing each to its log. The
// canary and the node are open on any TCP connect, nothing sent (nc -z), closed else. The target's ports
// are open when it answered, silent when nothing came back within 3 s, other else: busybox wget says
// "timed out" for the silence alone; a refused connection is other. TARGET is the target's address as a
// URL's host, CANARY and NODE bare addresses.
const prober = `canary=closed
end=$(( $(date +%s) + 5 ))
while [ "$(date +%s)" -lt "$end" ]; do nc -z -n -w 1 "$CANARY" 8080 && { canary=open; break; }; done
echo "canary $canary"
try() {
  if out=$(wget -T 3 -q -O /dev/null "http://$TARGET:$1/" 2>&1); then echo open
  else case "$out" in *"timed out"*) echo silent;; *) echo other;; esac; fi
}
end=$(( $(date +%s) + 30 ))
until [ "$(try 8080)" = open ]; do
  [ "$(date +%s)" -ge "$end" ] && { echo unreached; exit 0; }
  sleep 1
done
echo reached
end=$(( $(date +%s) + 15 ))
while [ "$(date +%s)" -lt "$end" ]; do echo "pair $(try 8080) $(try 4180)"; sleep 1; done
for port in 10250 22; do if nc -z -n -w 3 "$NODE" $port; then echo "node $port open"; else echo "node $port closed"; fi; done`

// verdict reads the prober's log into network_policy, its fix and the warning: not_enforced with the
// fix of the first check that connected — the last pair's 4180, then the canary, whatever the pairs
// say. The last pair's 4180 silent while its 8080 opened, the canary closed and every node port tried,
// is enforced; anything else, a target never reached among it, is an error. The node ports that
// connected are the warning, beside any answer: they never decide it.
func verdict(prober string) (string, string, string, error) {
	var last []string
	reached, canary, node := false, "", map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(prober), "\n") {
		f := strings.Fields(line)
		reached = reached || line == "reached"
		switch {
		case len(f) == 2 && f[0] == "canary":
			canary = f[1]
		case len(f) == 3 && f[0] == "node":
			node[f[1]] = f[2]
		case len(f) == 3 && f[0] == "pair":
			last = f[1:]
		}
	}
	var open []string
	tried := true
	for _, port := range nodePorts {
		switch node[port] {
		case "open":
			open = append(open, port)
		case "closed":
		default:
			tried = false
		}
	}
	warning := ""
	if len(open) > 0 {
		warning = nodeWarning(strings.Join(open, ", "))
	}
	switch {
	case last != nil && last[1] == "open":
		return "not_enforced", notEnforcedFix, warning, nil
	case canary == "open":
		return "not_enforced", lateFix, warning, nil
	case !reached:
		return "", "", "", errors.New("the probe could not tell: its prober never reached its target's 8080 within 30 s")
	case last == nil:
		return "", "", "", errors.New("the probe could not tell: its prober tried no pair")
	case last[0] == "open" && last[1] == "silent" && canary == "closed" && tried:
		return "enforced", "", warning, nil
	}
	return "", "", "", fmt.Errorf("the probe could not tell: its last try reached 8080 %s and 4180 %s, the canary %q and the node %v", last[0], last[1], canary, node)
}

// Probe answers whether this cluster keeps an app's port behind its sign-in, and whether people can
// reach the addresses domain and form give — the two halves side by side, within 60 s, one probe at a
// time. A cluster the probe could not tell about is an error (502), a domain or form front_door
// refuses is ErrRequest (400).
func (e *Executor) Probe(ctx context.Context, domain, form string) (ProbeResult, error) {
	if err := checkAddress(domain, form); err != nil {
		return ProbeResult{}, err
	}
	probing.Lock()
	defer probing.Unlock()
	ctx, cancel := context.WithTimeout(ctx, e.probeDeadline())
	defer cancel()
	var (
		res                ProbeResult
		np, npFix, warning string
		npErr              error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		np, npFix, warning, npErr = e.enforcement(ctx)
	}()
	certFix := e.certificate(ctx, domain, form)
	<-done
	if npErr != nil {
		return ProbeResult{}, npErr
	}
	res.NetworkPolicy, res.Certificate, res.Warning = np, "ok", warning
	var fix []string
	if npFix != "" {
		fix = append(fix, npFix)
	}
	if certFix != "" {
		res.Certificate = "missing"
		fix = append(fix, certFix)
	}
	res.Fix = strings.Join(fix, " ")
	return res, nil
}

func (e *Executor) probeDeadline() time.Duration {
	if e.probe.deadline > 0 {
		return e.probe.deadline
	}
	return 60 * time.Second
}

// probeLabels are every probe object's: canyon.probe, and which of the three pods it is for.
func probeLabels(role string) map[string]string {
	return map[string]string{"canyon.probe": "true", "canyon.probe-role": role}
}

// enforcement runs the target and the canary side by side, then the prober, reads the prober's log, and
// deletes what it made.
func (e *Executor) enforcement(ctx context.Context) (string, string, string, error) {
	c, run := e.Client, nonce()                     // run labels this probe's pods: a previous probe's, not yet collected, never answers for it
	defer e.SweepProbes(context.WithoutCancel(ctx)) // deleted before it answers, even past its deadline
	for _, np := range probePolicies() {
		if _, err := c.NetworkingV1().NetworkPolicies(Namespace).Create(ctx, np, metav1.CreateOptions{}); err != nil {
			return "", "", "", fmt.Errorf("the probe could not tell: networkpolicy %s: %w", np.Name, err)
		}
	}
	for _, j := range [][2]string{{"target", target}, {"canary", canary}} {
		if _, err := c.BatchV1().Jobs(Namespace).Create(ctx, e.probeJob(j[0], run, j[1], nil), metav1.CreateOptions{}); err != nil {
			return "", "", "", fmt.Errorf("the probe could not tell: job canyon-probe-%s: %w", j[0], err)
		}
	}
	ips := map[string]string{}
	for _, role := range []string{"target", "canary"} {
		if err := e.probeWait(ctx, role, run, func(p corev1.Pod) bool {
			ips[role] = p.Status.PodIP
			return p.Status.Phase == corev1.PodRunning && ips[role] != ""
		}); err != nil {
			return "", "", "", err
		}
	}
	ip := ips["target"]
	if strings.Contains(ip, ":") { // an IPv6 address is a URL's host bracketed
		ip = "[" + ip + "]"
	}
	env := []corev1.EnvVar{{Name: "TARGET", Value: ip}, {Name: "CANARY", Value: ips["canary"]},
		{Name: "NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}}}
	if _, err := c.BatchV1().Jobs(Namespace).Create(ctx, e.probeJob("prober", run, prober, env), metav1.CreateOptions{}); err != nil {
		return "", "", "", fmt.Errorf("the probe could not tell: job canyon-probe-prober: %w", err)
	}
	var name string
	if err := e.probeWait(ctx, "prober", run, func(p corev1.Pod) bool {
		name = p.Name
		return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
	}); err != nil {
		return "", "", "", err
	}
	stream, err := c.CoreV1().Pods(Namespace).GetLogs(name, &corev1.PodLogOptions{Container: "prober"}).Stream(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("the probe could not tell: the prober's log: %w", err)
	}
	defer func() { _ = stream.Close() }()
	out, err := io.ReadAll(io.LimitReader(stream, 64<<10))
	if err != nil {
		return "", "", "", fmt.Errorf("the probe could not tell: the prober's log: %w", err)
	}
	return verdict(string(out))
}

// probeWait polls this probe's pod of role until ready says it is, or ctx ends.
func (e *Executor) probeWait(ctx context.Context, role, run string, ready func(corev1.Pod) bool) error {
	for {
		list, err := e.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.probe=true,canyon.probe-role=" + role + ",canyon.probe-run=" + run})
		if err != nil {
			return fmt.Errorf("the probe could not tell: its %s pod: %w", role, err)
		}
		for _, p := range list.Items {
			if ready(p) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the probe could not tell: its %s pod was not there in time", role)
		case <-time.After(e.poll()):
		}
	}
}

// probePolicies admit the target's 4180 exactly as an app's (the router's pods alone) and its 8080 from
// the prober alone, and let the prober out to the target on those two ports and nowhere else.
func probePolicies() []*networkingv1.NetworkPolicy {
	from := networkPolicy("canyon-probe", "target", nil, false, nil).Spec.Ingress[0] // an app's own rule: 4180 from the router's pods
	return []*networkingv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "canyon-probe-target", Namespace: Namespace, Labels: probeLabels("target")},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: probeLabels("target")},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{from, {
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: probeLabels("prober")}}},
				Ports: []networkingv1.NetworkPolicyPort{tcp(8080)},
			}},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "canyon-probe-prober", Namespace: Namespace, Labels: probeLabels("prober")},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: probeLabels("prober")},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: probeLabels("target")}}},
				Ports: []networkingv1.NetworkPolicyPort{tcp(doorPort), tcp(8080)},
			}},
		},
	}}
}

// probeJob is a one-pod Job of the probe image running script: never restarted, no service-account
// token, every capability dropped, as nobody. Its pod holds nothing, and busybox httpd as PID 1 ignores
// SIGTERM, so the sweep's delete gives it one second before SIGKILL, not the default thirty.
func (e *Executor) probeJob(role, run, script string, env []corev1.EnvVar) *batchv1.Job {
	never, nobody, grace := corev1.RestartPolicyNever, int64(65534), int64(1)
	pod := probeLabels(role)
	pod["canyon.probe-run"] = run
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "canyon-probe-" + role, Namespace: Namespace, Labels: probeLabels(role)},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: pod},
				Spec: corev1.PodSpec{
					RestartPolicy: never, TerminationGracePeriodSeconds: &grace, AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &nobody, RunAsGroup: &nobody, RunAsNonRoot: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: role, Image: cmp.Or(e.Busybox, probeImage), Command: []string{"sh", "-c", script}, Env: env,
						SecurityContext: lockedDown, Resources: resources("10m", "16Mi", "100m", "32Mi"),
					}},
				},
			},
		},
	}
}

// SweepProbes deletes every probe Job, with its pod, and NetworkPolicy (the canary has none): after each
// probe, and at start what a crash left. It takes 10 s at most, so an API server that does not answer
// holds no probe, nor the lock behind it; a delete that fails is logged, and the next sweep tries again.
func (e *Executor) SweepProbes(ctx context.Context) {
	bound := 10 * time.Second
	if e.probe.sweep > 0 {
		bound = e.probe.sweep
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	background := metav1.DeletePropagationBackground
	for _, name := range []string{"canyon-probe-target", "canyon-probe-canary", "canyon-probe-prober"} {
		if err := e.Client.BatchV1().Jobs(Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("executor: probe sweep: job %s: %v", name, err)
		}
	}
	for _, name := range []string{"canyon-probe-target", "canyon-probe-prober"} {
		if err := e.Client.NetworkingV1().NetworkPolicies(Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("executor: probe sweep: networkpolicy %s: %v", name, err)
		}
	}
}

// certificate is "" when a TLS handshake on 443 with both of the form's canyon-probe addresses verifies
// against the roots (nil: the system's), 5 s each; otherwise the fix for the first address that failed.
func (e *Executor) certificate(ctx context.Context, domain, form string) string {
	names, wild := []string{"canyon-probe.preview." + domain, "canyon-probe.prod." + domain}, "*.preview."+domain+" and *.prod."+domain
	if form == "app-env.domain" {
		names, wild = []string{"canyon-probe-preview." + domain, "canyon-probe-prod." + domain}, "*."+domain
	}
	for _, name := range names {
		connected, err := e.handshake(ctx, name)
		var cert *tls.CertificateVerificationError
		switch {
		case err == nil:
			continue
		case errors.As(err, &cert):
			return "The certificate at " + name + " does not cover it. Add " + wild + " to the certificate and save again."
		case connected:
			return name + " answers on 443 but completed no TLS handshake within 5 seconds. A gateway that issues certificates on first use may still be issuing this one: wait a minute and save again. Otherwise have the gateway serve a certificate for " + wild + "."
		default:
			return name + " does not resolve or does not answer on 443. Point " + wild + " at the load balancer in front of the apps and save again."
		}
	}
	return ""
}

// handshake reports whether a connection to name's 443 was made, and the handshake's error.
func (e *Executor) handshake(ctx context.Context, name string) (connected bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dial := e.probe.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(name, "443"))
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	return true, tls.Client(conn, &tls.Config{ServerName: name, RootCAs: e.probe.roots, MinVersion: tls.VersionTLS12}).HandshakeContext(ctx) // nothing is sent after it
}

// probeHooks are the tests': the probe's bound, where a probe address is dialled, and the roots it is
// verified against. Zero is 60 s, the network and the system's roots.
type probeHooks struct {
	deadline time.Duration // the probe's, 60 s
	sweep    time.Duration // the sweep's, 10 s
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	roots    *x509.CertPool
}
