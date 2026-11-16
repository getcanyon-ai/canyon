package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

const (
	enforcedLog    = "canary closed\nreached\npair open open\npair open silent\npair open silent\nnode 10250 closed\nnode 22 closed\n"
	notEnforcedLog = "canary open\nreached\npair open silent\npair open open\nnode 10250 open\nnode 22 closed\n"
	lateLog        = "canary open\nreached\npair open silent\nnode 10250 closed\nnode 22 closed\n"
	nodeLog        = "canary closed\nreached\npair open silent\nnode 10250 open\nnode 22 open\n"
	probeDomain    = "canyon.meridian.example"
)

// The last pair decides, and a canary the prober reached decides whatever the pairs say; a not_enforced
// carries the sentence of the first check that connected: 4180, then the canary. The node never decides:
// the ports that connected are a warning beside any answer. A log that does
// not name the canary closed and both node ports is never enforced.
func TestVerdictReadsTheLastPair(t *testing.T) {
	for log, want := range map[string][3]string{
		enforcedLog:                {"enforced", "", ""},
		notEnforcedLog:             {"not_enforced", notEnforcedFix, nodeWarning("10250")},
		lateLog:                    {"not_enforced", lateFix, ""},
		nodeLog:                    {"enforced", "", nodeWarning("10250, 22")},
		"canary open\nunreached\n": {"not_enforced", lateFix, ""}, // whatever the pairs say
		"canary closed\nreached\npair open silent\nnode 10250 closed\nnode 22 open\n": {"enforced", "", nodeWarning("22")},
		"canary closed\nreached\npair open other\nnode 10250 open\nnode 22 closed\n":  {}, // 4180 refused: the node does not decide
		"canary closed\nunreached\n": {}, // the target never answered on 8080
		"canary closed\nreached\n":   {}, // no pair tried
		"canary closed\nreached\npair open other\nnode 10250 closed\nnode 22 closed\n":                   {},                                   // 4180 refused: a plugin that rejects, not drops
		"canary closed\nreached\npair silent silent\nnode 10250 closed\nnode 22 closed\n":                {},                                   // 8080 failed: the probe proves nothing
		"reached\npair open silent\nnode 10250 closed\nnode 22 closed\n":                                 {},                                   // no canary tried
		"canary closed\nreached\npair open silent\n":                                                     {},                                   // no node tried
		"canary closed\nreached\npair open silent\nnode 10250 closed\n":                                  {},                                   // 22 not tried
		"canary closed\nreached\npair open silent\nnode 10250 ???\nnode 22 closed\n":                     {},                                   // a port neither open nor closed
		"canary closed\nreached\npair open silent\npair other open\nnode 10250 closed\nnode 22 closed\n": {"not_enforced", notEnforcedFix, ""}, // 4180 open decides, whatever 8080 did
	} {
		np, fix, warning, err := verdict(log)
		if np != want[0] || fix != want[1] || warning != want[2] || (want[0] == "") != (err != nil) {
			t.Errorf("%q: %q %q %q %v, want %q", log, np, fix, warning, err, want)
		}
	}
}

// probeCluster answers the probe's Jobs as a cluster does: the target's pod runs at an address, the
// canary's at the next one down, the prober's finishes when proberFinishes, and its log is log.
func probeCluster(t *testing.T, e *Executor, log string, proberFinishes bool) (*fake.Clientset, *[]string) {
	return probeClusterAt(t, e, "10.9.8.7", "10.9.8.6", log, proberFinishes)
}

// probeClusterAt is probeCluster with the target's pod at ip and the canary's at canaryIP.
func probeClusterAt(t *testing.T, e *Executor, ip, canaryIP, log string, proberFinishes bool) (*fake.Clientset, *[]string) {
	c := cluster(t, e, healthy)
	var targets []string
	c.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-" + j.Spec.Template.Labels["canyon.probe-run"], Namespace: Namespace, Labels: j.Spec.Template.Labels}}
		switch j.Spec.Template.Labels["canyon.probe-role"] {
		case "target":
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}
		case "canary":
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: canaryIP}
		case "prober":
			for _, v := range j.Spec.Template.Spec.Containers[0].Env {
				if v.ValueFrom != nil {
					v.Value = "<-" + v.ValueFrom.FieldRef.FieldPath
				}
				targets = append(targets, v.Name+"="+v.Value)
			}
			p.Status.Phase = corev1.PodRunning
			if proberFinishes {
				p.Status.Phase = corev1.PodSucceeded
			}
		}
		if err := c.Tracker().Add(p); err != nil {
			t.Error(err)
		}
		return false, nil, nil
	})
	c.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if o := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions); o.Container != "prober" {
			t.Errorf("read the log of %q", o.Container)
		}
		return true, &runtime.Unknown{Raw: []byte(log)}, nil
	})
	return c, &targets
}

// certServer serves TLS for names under a CA the executor is told to trust, and points every probe
// address at itself.
func certServer(t *testing.T, e *Executor, names ...string) {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey, leafKey := key(), key()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "probe test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	e.probe.roots = x509.NewCertPool()
	e.probe.roots.AddCert(ca)
	e.probe.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
}

// POST /probe runs a target, a canary under no policy of its own and a prober under the contract's
// policies, hands the prober the target's, the canary's and its node's addresses, reads the prober's
// log, and deletes everything it made before it answers. Its pods are gone within a second of that
// delete: busybox httpd as PID 1 ignores SIGTERM, so under the default 30 s grace the target would
// outlive the answer by half a minute.
func TestProbeReadsTheProberAndLeavesNothing(t *testing.T) {
	e, _, _ := fixture(t)
	c, targets := probeCluster(t, e, enforcedLog, true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	res, err := e.Probe(t.Context(), probeDomain, "app.env.domain")
	if err != nil || res != (ProbeResult{NetworkPolicy: "enforced", Certificate: "ok", Fix: ""}) {
		t.Fatalf("%+v %v", res, err)
	}
	if want := []string{"TARGET=10.9.8.7", "CANARY=10.9.8.6", "NODE=<-status.hostIP"}; !reflect.DeepEqual(*targets, want) {
		t.Fatalf("the prober was handed %v", *targets)
	}
	var made []string
	for _, a := range c.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		o := a.(k8stesting.CreateAction).GetObject().(metav1.Object)
		made = append(made, a.GetResource().Resource+"/"+o.GetName())
		if o.GetLabels()["canyon.probe"] != "true" {
			t.Errorf("%s carries no canyon.probe", o.GetName())
		}
		if j, ok := o.(*batchv1.Job); ok {
			p := j.Spec.Template.Spec
			if *j.Spec.BackoffLimit != 0 || p.RestartPolicy != corev1.RestartPolicyNever || p.Containers[0].Image != probeImage || *p.AutomountServiceAccountToken || j.Spec.Template.Labels["canyon.front-door"] != "" || p.TerminationGracePeriodSeconds == nil || *p.TerminationGracePeriodSeconds != 1 {
				t.Errorf("job %s: %+v", j.Name, j.Spec)
			}
		}
	}
	if want := []string{"networkpolicies/canyon-probe-target", "networkpolicies/canyon-probe-prober", "jobs/canyon-probe-target", "jobs/canyon-probe-canary", "jobs/canyon-probe-prober"}; !reflect.DeepEqual(made, want) {
		t.Fatalf("made %v", made)
	}
	for _, np := range probePolicies() { // the canary is under no policy of its own
		if sel := np.Spec.PodSelector.MatchLabels; sel["canyon.probe-role"] == "canary" || sel["canyon.probe-role"] == "" {
			t.Errorf("%s selects the canary: %v", np.Name, sel)
		}
	}
	target, proberNP := probePolicies()[0].Spec, probePolicies()[1].Spec
	if !reflect.DeepEqual(target.Ingress[0], networkPolicy("x", "preview", nil, false, nil).Spec.Ingress[0]) || target.Ingress[1].Ports[0].Port.IntValue() != 8080 || target.Ingress[1].From[0].PodSelector.MatchLabels["canyon.probe-role"] != "prober" {
		t.Errorf("the target's policy %+v", target.Ingress)
	}
	if len(proberNP.Egress) != 1 || proberNP.Egress[0].To[0].PodSelector.MatchLabels["canyon.probe-role"] != "target" || proberNP.Egress[0].Ports[0].Port.IntValue() != 4180 || proberNP.Egress[0].Ports[1].Port.IntValue() != 8080 || len(proberNP.Ingress) != 0 {
		t.Errorf("the prober's policy %+v", proberNP)
	}
	jobs, _ := c.BatchV1().Jobs(Namespace).List(t.Context(), metav1.ListOptions{})
	nps, _ := c.NetworkingV1().NetworkPolicies(Namespace).List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items)+len(nps.Items) != 0 {
		t.Fatalf("left %d jobs and %d policies", len(jobs.Items), len(nps.Items))
	}
}

// A previous probe's pods, not yet collected, never answer for this one.
func TestProbeReadsItsOwnPodsAlone(t *testing.T) {
	e, _, _ := fixture(t)
	c, targets := probeCluster(t, e, enforcedLog, true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-old-target", Namespace: Namespace, Labels: map[string]string{"canyon.probe": "true", "canyon.probe-role": "target", "canyon.probe-run": "0ld0ld0l"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.1.1"}}
	if err := c.Tracker().Add(old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(t.Context(), probeDomain, "app.env.domain"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"TARGET=10.9.8.7", "CANARY=10.9.8.6", "NODE=<-status.hostIP"}; !reflect.DeepEqual(*targets, want) {
		t.Fatalf("the prober was handed %v, the previous probe's target", *targets)
	}
}

// The answer's fix is each failing half's sentence, in order, joined by one space; "" exactly when
// enforced and ok. The certificate names the first address that failed and the form's wildcards.
func TestProbeFixSentences(t *testing.T) {
	for _, tc := range []struct {
		name, log, form string
		cert            []string // nil: nothing answers on 443
		want            ProbeResult
	}{
		{"not enforced", notEnforcedLog, "app.env.domain", []string{"*.preview." + probeDomain, "*.prod." + probeDomain},
			ProbeResult{"not_enforced", "ok", notEnforcedFix, nodeWarning("10250")}},
		{"a new pod let out", lateLog, "app.env.domain", []string{"*.preview." + probeDomain, "*.prod." + probeDomain},
			ProbeResult{"not_enforced", "ok", lateFix, ""}},
		{"the node reached: saved, with a warning", nodeLog, "app.env.domain", []string{"*.preview." + probeDomain, "*.prod." + probeDomain},
			ProbeResult{"enforced", "ok", "", "Apps on this cluster can reach the server they run on (ports 10250, 22). Your cluster admin can close this with a host firewall rule."}},
		{"the other form's names", enforcedLog, "app-env.domain", []string{"*.preview." + probeDomain, "*.prod." + probeDomain},
			ProbeResult{"enforced", "missing", "The certificate at canyon-probe-preview.canyon.meridian.example does not cover it. Add *.canyon.meridian.example to the certificate and save again.", ""}},
		{"prod uncovered", enforcedLog, "app.env.domain", []string{"*.preview." + probeDomain},
			ProbeResult{"enforced", "missing", "The certificate at canyon-probe.prod.canyon.meridian.example does not cover it. Add *.preview.canyon.meridian.example and *.prod.canyon.meridian.example to the certificate and save again.", ""}},
		{"nothing on 443, not enforced", notEnforcedLog, "app.env.domain", nil,
			ProbeResult{"not_enforced", "missing", notEnforcedFix + " canyon-probe.preview.canyon.meridian.example does not resolve or does not answer on 443. Point *.preview.canyon.meridian.example and *.prod.canyon.meridian.example at the load balancer in front of the apps and save again.", nodeWarning("10250")}},
	} {
		e, _, _ := fixture(t)
		probeCluster(t, e, tc.log, true)
		if tc.cert != nil {
			certServer(t, e, tc.cert...)
		} else {
			e.probe.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no such host") }
		}
		if res, err := e.Probe(t.Context(), probeDomain, tc.form); err != nil || res != tc.want {
			t.Errorf("%s:\n got %+v %v\nwant %+v", tc.name, res, err, tc.want)
		}
	}
}

// An address that answers on 443 but completes no handshake (a gateway still issuing the certificate
// on demand, a TLS server with nothing for the name) is told apart from one that does not answer
// (#1446): the connection was made, so pointing DNS at the load balancer is not the fix.
func TestProbeTellsAHandshakeThatDidNotCompleteFromNoAnswer(t *testing.T) {
	e, _, _ := fixture(t)
	probeCluster(t, e, enforcedLog, true)
	e.probe.dial = func(context.Context, string, string) (net.Conn, error) {
		near, far := net.Pipe()
		_ = far.Close() // connected, then nothing: the handshake cannot complete
		return near, nil
	}
	want := ProbeResult{"enforced", "missing", "canyon-probe.preview.canyon.meridian.example answers on 443 but completed no TLS handshake within 5 seconds. A gateway that issues certificates on first use may still be issuing this one: wait a minute and save again. Otherwise have the gateway serve a certificate for *.preview.canyon.meridian.example and *.prod.canyon.meridian.example.", ""}
	if res, err := e.Probe(t.Context(), probeDomain, "app.env.domain"); err != nil || res != want {
		t.Fatalf("\n got %+v %v\nwant %+v", res, err, want)
	}
}

// A probe that cannot tell is an error, never an answer: a target never reached, or a prober still
// running at the deadline; either way what it made is gone. A domain or form /run refuses is refused
// before anything is made.
func TestProbeThatCannotTellIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		log      string
		finishes bool
	}{"unreached": {"unreached\n", true}, "past the deadline": {enforcedLog, false}} {
		e, _, _ := fixture(t)
		c, _ := probeCluster(t, e, tc.log, tc.finishes)
		certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
		e.probe.deadline = 100 * time.Millisecond
		if res, err := e.Probe(t.Context(), probeDomain, "app.env.domain"); err == nil || !strings.Contains(err.Error(), "could not tell") || errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		jobs, _ := c.BatchV1().Jobs(Namespace).List(context.Background(), metav1.ListOptions{})
		nps, _ := c.NetworkingV1().NetworkPolicies(Namespace).List(context.Background(), metav1.ListOptions{})
		if len(jobs.Items)+len(nps.Items) != 0 {
			t.Errorf("%s: left %d jobs and %d policies", name, len(jobs.Items), len(nps.Items))
		}
	}
	e, _, _ := fixture(t)
	c, _ := probeCluster(t, e, enforcedLog, true)
	for _, a := range [][2]string{{"preview.meridian.example", "app.env.domain"}, {probeDomain, "env.app.domain"}, {"Canyon.example", "app.env.domain"}} {
		if _, err := e.Probe(t.Context(), a[0], a[1]); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%v: %v, want a refusal", a, err)
		}
	}
	if n := len(c.Actions()); n != 0 {
		t.Fatalf("a refused probe made %d calls", n)
	}
}

// An IPv6 pod address reaches the prober bracketed, as a URL's host needs it: bare, busybox's wget
// cannot connect and the probe would report its target unreached. The canary's stays bare, as nc
// takes it.
func TestTheProberIsHandedAnIPv6TargetBracketed(t *testing.T) {
	e, _, _ := fixture(t)
	_, targets := probeClusterAt(t, e, "fd00::7", "fd00::6", enforcedLog, true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	if _, err := e.Probe(t.Context(), probeDomain, "app.env.domain"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"TARGET=[fd00::7]", "CANARY=fd00::6", "NODE=<-status.hostIP"}; !reflect.DeepEqual(*targets, want) {
		t.Fatalf("the prober was handed %v", *targets)
	}
}

// A probe after a probe makes its objects again: the sweep deleted every one, the canary's Job among
// them, so the next create never meets AlreadyExists.
func TestASecondProbeRunsAfterTheFirst(t *testing.T) {
	e, _, _ := fixture(t)
	probeCluster(t, e, enforcedLog, true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	for i := range 2 {
		if res, err := e.Probe(t.Context(), probeDomain, "app.env.domain"); err != nil || res.NetworkPolicy != "enforced" {
			t.Fatalf("probe %d: %+v %v", i+1, res, err)
		}
	}
}

// The sweep after a probe is bounded: an API server that does not answer cannot hold the probe, and
// with it the lock every later probe waits on, for longer than the sweep's own deadline.
func TestTheSweepIsBoundedWhenTheAPIDoesNotAnswer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // an API server that never answers
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Client: client}
	e.probe.sweep = 100 * time.Millisecond
	done := make(chan struct{})
	go func() { defer close(done); e.SweepProbes(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep is still waiting on the API after 5 s")
	}
}
