//go:build kube

package kube

// The kind tests of contracts/executor.md (the invariants, On Kubernetes): the executor deploy/k8s
// deploys, on a real cluster, reached by port-forward, and the network checked from inside real pods.
// `make test-kube` runs them against the cluster KUBECONFIG names; .github/workflows/kube.yml, against
// kind (.github/kind/up.sh). CANYON_KUBE_TEST_PUSH is where this host pushes the test app's image
// (default: the executor's CANYON_KUBE_REGISTRY); CANYON_KUBE_TEST_DB an address in CANYON_KUBE_DB_CIDRS
// listening on 5432 and 5433; CANYON_KUBE_TEST_SIGNIN an address outside the cluster's pods listening on
// 443 and 8443, a stand-in for an internal identity provider; CANYON_KUBE_TEST_NETWORK_POLICY what /probe
// must answer (default enforced); CANYON_KUBE_TEST_LEDGER the container running Canyon's own Postgres on this
// host (up.sh's kind-pg), where a floored app's env is named to the server's egress policy (named).

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getcanyon-ai/canyon/executor"
)

var kind struct {
	client   kubernetes.Interface
	executor string    // the executor's URL, port-forwarded
	forward  *exec.Cmd // that port-forward
	registry string    // its CANYON_KUBE_REGISTRY
}

// TestMain reaches the cluster and the executor, runs the tests, and stops what they ran.
func TestMain(m *testing.M) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err == nil {
		kind.client, err = kubernetes.NewForConfig(cfg)
	}
	if err != nil {
		log.Fatal(err)
	}
	d, err := kind.client.AppsV1().Deployments("canyon").Get(context.Background(), "canyon-executor", metav1.GetOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "CANYON_KUBE_REGISTRY" {
			kind.registry = e.Value
		}
	}
	if err := forwardExecutor("deploy/canyon-executor"); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	for _, app := range []string{"kind-a", "kind-b", "kind-floor", "kind-choice-oidc", "kind-choice-alb", "kind-choice-2", "kind-signin", "kind-signin-public", "kind-signin-chart", "kind-metadata"} {
		_, _ = call(nil, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"})
	}
	_ = kind.client.CoreV1().Pods("canyon").DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "canyon.kind-test=true"})
	_ = kind.client.CoreV1().Services("canyon").Delete(context.Background(), "canyon-kind-web", metav1.DeleteOptions{})
	_ = kind.forward.Process.Kill()
	os.Exit(code)
}

// forwardExecutor port-forwards to target, the executor's Deployment or pod: kind.executor is its URL.
func forwardExecutor(target string) error {
	pf := exec.Command("kubectl", "-n", "canyon", "port-forward", target, ":7778") //nolint:gosec // the tests' own arguments
	out, err := pf.StdoutPipe()
	if err == nil {
		err = pf.Start()
	}
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(out).ReadString('\n') // Forwarding from 127.0.0.1:<port> -> 7778
	addr, _, ok := strings.Cut(strings.TrimPrefix(line, "Forwarding from "), " ->")
	if err != nil || !ok {
		_ = pf.Process.Kill()
		return fmt.Errorf("port-forward to the executor: %q %v", line, err)
	}
	kind.forward, kind.executor = pf, "http://"+addr
	return nil
}

// toExecutor reaches the executor through the suite's port-forward on a new connection per call: the executor
// closes a connection idle 60 s (its IdleTimeout), and through the port-forward a client keeping one never sees
// that, so its next call there reads EOF (#2545).
var toExecutor = &http.Transport{DisableKeepAlives: true}

// call sends body as JSON to the executor and answers its status and body; t nil is cleanup's.
func call(t *testing.T, method, path string, body any) (int, []byte) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, kind.executor+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 6 * time.Minute, Transport: toExecutor}).Do(req)
	if err != nil {
		if t == nil {
			return 0, nil
		}
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func accessFile(updated string) map[string]any {
	return map[string]any{"updated_at": updated, "groups_claim": "groups",
		"members": map[string]any{"domains": []string{"kind.test"}, "emails": []string{}},
		"admins":  map[string]any{"emails": []string{}, "claim": "groups", "value": "canyon-admins"},
		"roles":   map[string]string{}, "entries": []any{}}
}

// running is an app /run started: its pod, and whether the router's first connection to its 4180 —
// made the moment /run answered — connected.
type running struct {
	pod           corev1.Pod
	firstFromDoor bool
	answered      json.RawMessage // what /run said the app answered its sidecar, asked by the executor in the cluster
}

var (
	apps    = map[string]*running{}
	appsMu  sync.Mutex
	helpers sync.Once
)

// app runs a busybox httpd on 3000 as app in preview, behind choice 1 through an ALB — whose sidecar
// fetches nothing at start, so it answers 401 to every request here, which is all a network test needs.
func app(t *testing.T, app string) *running {
	t.Helper()
	appsMu.Lock()
	defer appsMu.Unlock()
	if r := apps[app]; r != nil {
		return r
	}
	image := appImage(t, app)
	status, out := call(t, http.MethodPost, "/run", map[string]any{
		"app": app, "target": "preview", "image": image, "env": map[string]string{"CANYON_KIND_APP": app}, "network": "canyon-app-" + app,
		"identity": map[string]string{"issuer": "https://canyon.kind.test", "audience": app, "jwks_url": jwksURL},
		"access":   accessFile("2026-09-26T10:00:00Z"),
		"front_door": map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
			"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"},
	})
	if status != http.StatusOK {
		t.Fatalf("/run %s: %d %s", app, status, out)
	}
	pods, err := kind.client.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "canyon.app=" + app})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods of %s: %v %d", app, err, len(pods.Items))
	}
	var res struct {
		Answered json.RawMessage `json:"answered"`
	}
	_ = json.Unmarshal(out, &res)
	r := &running{pod: pods.Items[0], answered: res.Answered}
	r.firstFromDoor, _ = reach(t, "canyon", routerPod(t), "nginx", r.pod.Status.PodIP, 4180)
	if status, out := call(t, http.MethodGet, "/running?app="+app+"&target=preview", nil); status != http.StatusOK || !strings.Contains(string(out), image) {
		t.Errorf("/running %s: %d %s, not %s", app, status, out, image)
	}
	asked := map[string]any{"containers": []map[string]string{{"app": app, "target": "preview"}, {"app": app, "target": "prod"}}}                                                                                   // under the executor's own Role: its lists are allowed
	if status, out := call(t, http.MethodPost, "/running", asked); status != http.StatusOK || string(out) != `{"running":[{"app":"`+app+`","target":"preview"}],"restarting":[],"starting":[],"stopped":[]}`+"\n" { // without why, never a line
		t.Errorf("POST /running %s: %d %s", app, status, out)
	}
	apps[app] = r
	return r
}

// floored runs app as app does, behind door, with egress and a run and env: the floor on (design/EGRESS.md
// §2.2). It answers /run's status and body, and the app's pod once /run answered 200.
func floored(t *testing.T, app string, door map[string]any, env map[string]string) (int, []byte, *corev1.Pod) {
	t.Helper()
	named(t, app)
	env["CANYON_KIND_APP"] = app
	status, out := call(t, http.MethodPost, "/run", map[string]any{
		"app": app, "target": "preview", "image": appImage(t, app), "env": env, "network": "canyon-app-" + app,
		"identity": map[string]string{"issuer": "https://canyon.kind.test", "audience": app, "jwks_url": jwksURL},
		"access":   accessFile("2026-09-26T10:00:00Z"), "front_door": door,
		"run": "gr_kind0001", "egress": map[string]any{"proxy": canyonEgress, "no_proxy": []string{}},
	})
	if status != http.StatusOK {
		return status, out, nil
	}
	pods, err := kind.client.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "canyon.app=" + app})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods of %s: %v %d", app, err, len(pods.Items))
	}
	return status, out, &pods.Items[0]
}

var (
	egressSet    sync.Once
	egressSetErr error // the host's setting refused, which every floored test fails on
)

// named makes app's preview an env the server's egress policy names (design/EGRESS.md §3.2), as a deploy step
// does before its /run: these tests start apps through the executor alone, never a deploy, so without it the check
// beside Envoy refuses every call of a floored app, unknown_caller. The project through the server's API; the env
// into Canyon's Postgres, as a deploy row's trigger writes it, in the container CANYON_KUBE_TEST_LEDGER names; and,
// once, the host's via canyon and its mode allowlist, under which no app here has an allowlist but the destinations
// a test allows it, so every floored pod's sidecars reach only the platform destinations of the front door the
// test stored (storeDoor), and a call to anything else is refused not_allowed.
func named(t *testing.T, app string) {
	t.Helper()
	base := serverAPI(t)
	egressSet.Do(func() {
		if status, out := apiCall(t, base, http.MethodPut, "/api/v1/egress", map[string]string{"via": "canyon", "mode": "allowlist"}); status != http.StatusOK {
			egressSetErr = fmt.Errorf("PUT /api/v1/egress via canyon, mode allowlist: %d %s", status, out)
		}
	})
	if egressSetErr != nil { // every test that names an app fails on it, never only the first
		t.Fatal(egressSetErr)
	}
	if status, out := apiCall(t, base, http.MethodPost, "/api/v1/projects", map[string]string{"id": app}); status != http.StatusCreated && status != http.StatusConflict {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, status, out)
	}
	target, err := strconv.ParseInt(ledger(t, "INSERT INTO egress_envs (project_id, env) VALUES ('"+app+"', 'preview') ON CONFLICT DO NOTHING; SELECT seq FROM egress_policy"), 10, 64)
	if err != nil {
		t.Fatalf("the egress policy's seq: %v", err)
	}
	policyTaken(t, target, 30*time.Second, app+" preview")
}

// policyTaken waits as a deploy's converge does (runs/pipeline.go; #2781): every egress-control replica seen in the
// last 15 s (runs.replicaLive) holds version seq or a later one of this epoch. A token caller is named by the policy a
// replica holds, with no refetch: before its next poll the env is unknown_caller. Unlike converge it also needs one
// live replica, so a gateway that is down or a server that stopped recording replicas fails here, never passes.
func policyTaken(t *testing.T, seq int64, max time.Duration, what string) {
	t.Helper()
	for deadline := time.Now().Add(max); ; time.Sleep(250 * time.Millisecond) {
		row := ledger(t, fmt.Sprintf(`SELECT count(*) || ' ' || coalesce(min(replica COLLATE "C") FILTER (WHERE have_epoch IS DISTINCT FROM (SELECT epoch FROM egress_policy) OR have_seq IS NULL OR have_seq < %d), '')
			FROM egress_replicas WHERE seen_at > now() - interval '15 seconds'`, seq))
		live, behind, _ := strings.Cut(row, " ")
		if live != "0" && behind == "" {
			return
		}
		if time.Now().After(deadline) {
			if live == "0" {
				t.Fatalf("no egress-control replica asked for the policy in the last 15 s, %s after the version naming %s (seq %d)", max, what, seq)
			}
			t.Fatalf("egress-control replica %s has not taken the version naming %s (seq %d) in %s", behind, what, seq, max)
		}
	}
}

// ledger runs sql, the test's own statement, in Canyon's Postgres in the container CANYON_KUBE_TEST_LEDGER names,
// answering what it printed, unaligned.
func ledger(t *testing.T, sql string) string {
	t.Helper()
	container := os.Getenv("CANYON_KUBE_TEST_LEDGER")
	if container == "" {
		t.Fatal("CANYON_KUBE_TEST_LEDGER is unset: no floored app's env can be named to the server's egress policy, and the check refuses every call of it")
	}
	out, err := exec.Command("docker", "exec", container, "psql", "-U", "canyon", "-d", "canyon", "-v", "ON_ERROR_STOP=1", "-qtAc", sql).CombinedOutput() //nolint:gosec // the test's own statement
	if err != nil {
		t.Fatalf("%s in %s: %v %s", sql, container, err, out)
	}
	return strings.TrimSpace(string(out))
}

// storeDoor stores door, without any client secret, as the server's front door, in Canyon's Postgres: the kind
// cluster has no certificate for kind.test, so the save's probe would refuse it. Its platform destinations are what
// a floored pod's sidecars reach under an empty allowlist (design/EGRESS.md §3.3): for a choice 2, the endpoints its
// issuer's discovery names, which the server's own minute loop reads from the provider and records, as for a door
// saved while the provider could not be read; storeDoor waits for that, up to 90 s, then for every egress-control
// replica to hold the policy that names them. Its sealed client secret is a placeholder: no test here deploys
// through the server, which alone opens it.
func storeDoor(t *testing.T, door map[string]any) {
	t.Helper()
	stored := maps.Clone(door)
	sealed := "NULL, NULL, NULL"
	if signin, ok := door["signin"].(map[string]string); ok {
		stored["signin"], sealed = map[string]string{"issuer": signin["issuer"], "client_id": signin["client_id"]}, "1, '', ''"
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	seq := ledger(t, `DELETE FROM front_doors; INSERT INTO front_doors (probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
		VALUES (now(), $door$`+string(raw)+`$door$, `+sealed+`, 'person', 'kind', 'kind'); SELECT seq FROM egress_policy`)
	if signin, ok := door["signin"].(map[string]string); ok {
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
			if hosts := ledger(t, "SELECT hosts FROM front_door_discovery WHERE issuer = '"+signin["issuer"]+"'"); hosts != "" {
				seq = ledger(t, "SELECT seq FROM egress_policy")
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the server recorded no discovery of %s 90 s after its door was stored: its minute loop never read it", signin["issuer"])
			}
		}
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil {
		t.Fatalf("the egress policy's seq: %v", err)
	}
	policyTaken(t, n, time.Minute, "the stored front door")
}

// allow puts each of names on app's preview allowlist through the server's API, as a person in the dashboard does.
func allow(t *testing.T, app string, names ...string) {
	t.Helper()
	base := serverAPI(t)
	for _, n := range names {
		if status, out := apiCall(t, base, http.MethodPut, "/api/v1/projects/"+app+"/egress/preview/destinations/"+n, nil); status != http.StatusNoContent {
			t.Fatalf("allowing %s for %s: %d %s", n, app, status, out)
		}
	}
}

// tunnel is the status line Envoy answers a CONNECT to authority from pod's app container with: busybox nc stops
// reading at its stdin's end, so it stays open past Envoy's 5 s connect timeout.
func tunnel(pod *corev1.Pod, authority string) string {
	return tunnelVia(pod, authority, "canyon-egress.canyon.svc")
}

// tunnelVia is tunnel through the gateway at gateway, a name or one replica's pod address.
func tunnelVia(pod *corev1.Pod, authority, gateway string) string {
	out, _ := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "sh", "-c",
		`(printf 'CONNECT `+authority+` HTTP/1.1\r\nHost: `+authority+`\r\n\r\n'; sleep 8) | nc -w 10 `+gateway+` 3128 | head -n 1`)
	return strings.TrimSpace(string(out))
}

// serverAPI port-forwards to the server's pod for the test, answering its API's base URL.
func serverAPI(t *testing.T) string {
	t.Helper()
	server := kindPods(t, "canyon", "app.kubernetes.io/name=canyon-server,!canyon.kind-test") // the real server, never the helper labelled as it
	if len(server) != 1 {
		t.Fatalf("%d server pods", len(server))
	}
	return forward(t, "pod/"+server[0].Name, 7777)
}

// apiCall sends body as JSON to the server at base, an admin's under CANYON_AUTH=demo, and answers its status and body.
func apiCall(t *testing.T, base, method, path string, body any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, base+path, bytes.NewReader(raw)) //nolint:noctx // a port-forward the test owns
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

// appImage pushes busybox, saying which app it is and then serving 3000, as canyon/<app>, and answers
// it by digest as the cluster sees it.
func appImage(t *testing.T, app string) string {
	t.Helper()
	return appImageRunning(t, app, "echo canyon-kind-app $CANYON_KIND_APP; exec httpd -f -v -p 3000")
}

// appImageRunning is appImage running script with sh -c in place of its own.
func appImageRunning(t *testing.T, app, script string) string {
	t.Helper()
	base, err := remote.Image(name.MustParseReference(probeImage), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: goruntime.GOARCH}))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config.Cmd = []string{"sh", "-c", script}
	img, err := mutate.ConfigFile(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	push := os.Getenv("CANYON_KUBE_TEST_PUSH")
	if push == "" {
		push = kind.registry
	}
	tag, err := name.NewTag(push + "/canyon/" + app + ":kind") // localhost and RFC 1918 hosts are spoken to over http
	if err == nil {
		err = remote.Write(tag, img, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	sum, derr := img.Digest()
	if err = errors.Join(err, derr); err != nil {
		t.Fatal(err)
	}
	return kind.registry + "/canyon/" + app + "@" + sum.String()
}

func routerPod(t *testing.T) string {
	t.Helper()
	pods, err := kind.client.CoreV1().Pods("canyon").List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=canyon-router"})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("the router's pods: %v", err)
	}
	return pods.Items[0].Name
}

// helperPods starts three busybox pods in canyon: canyon-kind-server, labelled as the server's pods are, so
// the server's policy holds it, and serving 7777, but never Ready — a readiness gate no one sets — so never an
// endpoint of the server's Service, where it would take a share of the server's calls and refuse Envoy's records
// (#2377); canyon-kind-stray, labelled as nothing; and canyon-kind-web,
// serving /etc on 80 and 443 behind the Service canyon-kind-web on 80, a private destination that answers
// 200, and accepts a tunnel's TCP on 443.
func helperPods(t *testing.T) {
	t.Helper()
	helpers.Do(func() {
		for name, labels := range map[string]map[string]string{
			"canyon-kind-server": {"canyon.kind-test": "true", "app.kubernetes.io/name": "canyon-server"},
			"canyon-kind-stray":  {"canyon.kind-test": "true"},
			"canyon-kind-web":    {"canyon.kind-test": "true", "canyon.kind-web": "true"},
		} {
			serve := []string{"httpd", "-f", "-p", "7777"}
			if name == "canyon-kind-web" {
				serve = []string{"sh", "-c", "httpd -p 443 -h /etc && exec httpd -f -p 80 -h /etc"}
			}
			pod := asNobody(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "busybox", Image: probeImage, Command: serve}}}})
			if name == "canyon-kind-server" {
				pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "canyon.kind-test/endpoint"}}
			}
			if _, err := kind.client.CoreV1().Pods("canyon").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
		}
		web := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "canyon-kind-web", Labels: map[string]string{"canyon.kind-test": "true"}}, Spec: corev1.ServiceSpec{
			Selector: map[string]string{"canyon.kind-web": "true"}, Ports: []corev1.ServicePort{{Port: 80}}}}
		if _, err := kind.client.CoreV1().Services("canyon").Create(context.Background(), web, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		if out, err := kubectl("-n", "canyon", "wait", "--for=condition=ContainersReady", "pod", "-l", "canyon.kind-test=true", "--timeout=3m"); err != nil {
			t.Fatalf("the helper pods: %v %s", err, out)
		}
	})
}

// reach opens TCP from a container of a pod to host:port, as nc -z with a 3 s timeout: whether it
// connected, and whether nothing answered at all — dropped, where a closed port refuses at once.
func reach(t *testing.T, namespace, pod, container, host string, port int) (connected, dropped bool) {
	t.Helper()
	start := time.Now()
	out, err := kubectl("-n", namespace, "exec", pod, "-c", container, "--", "nc", "-z", "-w", "3", host, strconv.Itoa(port))
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, false
	case errors.As(err, &exit) && exit.ExitCode() == 1 && string(bytes.TrimSpace(out)) == "command terminated with exit code 1": // nc's own failure, as kubectl reports it
		return false, time.Since(start) >= 2500*time.Millisecond
	}
	t.Fatalf("nc in %s/%s: %v %s", namespace, pod, err, out)
	return false, false
}

// kubectl runs kubectl with the tests' own arguments, answering what it wrote.
func kubectl(args ...string) ([]byte, error) {
	return exec.Command("kubectl", args...).CombinedOutput() //nolint:gosec // the tests' own arguments
}

// blocked is host:port dropped from the pod: no connection, and no refusal either — a refusal is a
// packet that got through to a closed port.
func blocked(t *testing.T, namespace, pod, container, host string, port int) bool {
	t.Helper()
	ok, dropped := reach(t, namespace, pod, container, host, port)
	return !ok && dropped
}

func serviceIP(t *testing.T, name string) string {
	t.Helper()
	s, err := kind.client.CoreV1().Services("canyon").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Spec.ClusterIP
}

// Each of Canyon's namespaces enforces Pod Security Admission (#2368; deploy/k8s/namespaces.yaml): canyon-apps
// baseline, as an app runs whatever user its image names, and canyon, canyon-builds and canyon-scans restricted. So
// a privileged pod is refused in canyon-apps however it is made, and a root one in each restricted namespace, while
// one that meets the level is let in. Each is asked of the API server alone (a dry run): nothing runs.
func TestKindPodSecurityRefusesWhatEachNamespaceForbids(t *testing.T) {
	root := func(sc *corev1.SecurityContext) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-kind-pod-security"}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "busybox", Image: probeImage, Command: []string{"true"}, SecurityContext: sc}}}}
	}
	restricted := asNobody(root(nil))
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	for _, c := range []struct {
		ns, what string
		pod      *corev1.Pod
		refused  bool
	}{
		{Namespace, "a privileged pod", root(&corev1.SecurityContext{Privileged: new(true)}), true},
		{Namespace, "a pod as root, as an app's image may run", root(nil), false},
		{"canyon", "a pod as root", root(nil), true},
		{"canyon", "a restricted pod", restricted, false},
		{BuildNamespace, "a pod as root", root(nil), true},
		{BuildNamespace, "a restricted pod", restricted, false},
		{ScanNamespace, "a pod as root", root(nil), true},
		{ScanNamespace, "a restricted pod", restricted, false},
	} {
		_, err := kind.client.CoreV1().Pods(c.ns).Create(t.Context(), c.pod.DeepCopy(), dry)
		if refused := apierrors.IsForbidden(err) && strings.Contains(err.Error(), "violates PodSecurity"); refused != c.refused || err != nil && !refused {
			t.Errorf("%s in %s: %v, want refused by Pod Security %t", c.what, c.ns, err, c.refused)
		}
	}
}

// asNobody is pod as restricted Pod Security admits it (#2368): user 65534, the runtime's seccomp profile, no privilege
// escalation and every capability dropped. A port under 1024 still binds: the kind node's containerd lets any user
// bind one.
func asNobody(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: new(true), RunAsUser: new(int64(65534)), RunAsGroup: new(int64(65534)),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	}
	return pod
}

// TestKindAppPortOnlyThroughTheSidecar is invariant 10: from a pod of canyon without the front-door
// label and from another app's pod, the app pod's 3000, 4180, 4181 and 4183 do not connect; from a router
// pod, its first connection included, the app answers through the sidecar at both addresses; and the
// executor, in the cluster under its own policy, asked the sidecar's 4183 what the app answered.
func TestKindAppPortOnlyThroughTheSidecar(t *testing.T) {
	helperPods(t)
	a, b := app(t, "kind-a"), app(t, "kind-b")
	if !a.firstFromDoor || !b.firstFromDoor {
		t.Errorf("the router's first connection to a new app's 4180 did not connect: kind-a %v, kind-b %v", a.firstFromDoor, b.firstFromDoor)
	}
	ip := a.pod.Status.PodIP
	for _, port := range []int{3000, 4180} { // they listen: from inside the pod they connect
		if ok, _ := reach(t, Namespace, a.pod.Name, "app", "127.0.0.1", port); !ok {
			t.Fatalf("nothing listens on the app pod's %d", port)
		}
	}
	if !strings.Contains(string(a.answered), `"status":`) {
		t.Errorf("the executor did not hear what kind-a answered its sidecar: /run's answered %s", a.answered)
	}
	for _, port := range []int{3000, 4180, 4181, 4183} {
		if !blocked(t, "canyon", "canyon-kind-stray", "busybox", ip, port) {
			t.Errorf("a pod of canyon without the front-door label reached the app's %d", port)
		}
		if !blocked(t, Namespace, b.pod.Name, "app", ip, port) {
			t.Errorf("another app's pod reached the app's %d", port)
		}
	}
	if !blocked(t, "canyon", routerPod(t), "nginx", ip, 3000) {
		t.Error("the router reached the app's 3000")
	}
	for _, host := range []string{"kind-a.preview.kind.test", "kind-a-preview.kind.test"} {
		out, _ := kubectl("-n", "canyon", "exec", "canyon-kind-stray", "--", "wget", "-q", "-S", "-O", "/dev/null", "-T", "10",
			"--header", "Host: "+host, "http://"+serviceIP(t, "canyon-router")+"/")
		if !strings.Contains(string(out), "401 Unauthorized") { // the sidecar's answer to no ALB header: nginx answers 404 or 502, never 401
			t.Errorf("%s through the router: %s", host, out)
		}
	}
}

// TestKindAppsReachNoOtherAppNorTheControlPlane is invariant 11: from app A's pod, app B's pod, the
// server's and the executor's Services and TCP 5432 in the database ranges do not connect, TCP 5433 there
// does; the executor admits the server's pods alone; the router reaches every app; and the apps
// database's admin login is in no object of canyon-apps.
func TestKindAppsReachNoOtherAppNorTheControlPlane(t *testing.T) {
	helperPods(t)
	a, b := app(t, "kind-a"), app(t, "kind-b")
	for _, port := range []int{3000, 4180, 4181} {
		if !blocked(t, Namespace, a.pod.Name, "app", b.pod.Status.PodIP, port) {
			t.Errorf("app A reached app B's %d", port)
		}
	}
	server, executor := serviceIP(t, "canyon-server"), serviceIP(t, "canyon-executor")
	if !blocked(t, Namespace, a.pod.Name, "app", server, 7777) || !blocked(t, Namespace, a.pod.Name, "app", executor, 7778) {
		t.Error("app A reached the server's or the executor's Service")
	}
	db := os.Getenv("CANYON_KUBE_TEST_DB")
	if ok, _ := reach(t, Namespace, a.pod.Name, "app", db, 5433); !ok || !blocked(t, Namespace, a.pod.Name, "app", db, 5432) {
		t.Errorf("app A at the apps database %s: 5433 must connect, 5432 must not", db)
	}
	if ok, _ := reach(t, "canyon", "canyon-kind-server", "busybox", executor, 7778); !ok {
		t.Error("a server pod did not reach the executor")
	}
	if !blocked(t, "canyon", "canyon-kind-stray", "busybox", executor, 7778) {
		t.Error("a pod of canyon that is not the server's reached the executor")
	}
	if ok, _ := reach(t, "canyon", "canyon-kind-stray", "busybox", server, 7777); !ok {
		t.Error("the server's Service answers no one: its unreachability from app A proves nothing")
	}
	for _, r := range []*running{a, b} {
		if ok, _ := reach(t, "canyon", routerPod(t), "nginx", r.pod.Status.PodIP, 4180); !ok {
			t.Errorf("the router did not reach %s", r.pod.Name)
		}
	}
	s, err := kind.client.CoreV1().Secrets("canyon").Get(context.Background(), "canyon-server", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(string(s.Data["CANYON_APP_DATABASE_URL"]))
	admin, _ := u.User.Password()
	if err != nil || admin == "" {
		t.Fatalf("the server's Secret names no admin login: %v", err)
	}
	secrets, err1 := kind.client.CoreV1().Secrets(Namespace).List(context.Background(), metav1.ListOptions{})
	maps, err2 := kind.client.CoreV1().ConfigMaps(Namespace).List(context.Background(), metav1.ListOptions{})
	deployments, err3 := kind.client.AppsV1().Deployments(Namespace).List(context.Background(), metav1.ListOptions{})
	if err := errors.Join(err1, err2, err3); err != nil {
		t.Fatal(err)
	}
	everything, err := leakScan(secrets, maps, deployments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(everything.String(), admin) {
		t.Error("the apps database's admin login is in an object of canyon-apps")
	}
}

// TestKindBuildsReachNoPrivateNorLinkLocalAddress is invariant 12 on Kubernetes: from a pod of canyon-builds
// labelled as nothing — every kpack build pod is held by the same policies, and kind here runs no kpack build —
// the metadata address, a link-local address the node answers on 80 (on kind, whose node this test reaches into)
// and a private pod's 80 and 443 do not connect, while a pod of canyon labelled as nothing reaches the last three,
// so their refusal is the policy's; and a public 443, where the registries are, does.
func TestKindBuildsReachNoPrivateNorLinkLocalAddress(t *testing.T) {
	helperPods(t)
	const linkLocal = "169.254.42.1"
	nodes, err := kind.client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	onKind := len(nodes.Items) > 0
	for _, n := range nodes.Items {
		onKind = onKind && strings.HasPrefix(n.Spec.ProviderID, "kind://")
	}
	for _, n := range nodes.Items { // kind's node is a docker container: the address on its loopback, as a cloud's metadata address is on the host's way out
		if !onKind {
			break
		}
		//nolint:gosec // kind's node
		if out, err := exec.Command("docker", "exec", n.Name, "ip", "addr", "replace", linkLocal+"/32", "dev", "lo").CombinedOutput(); err != nil {
			t.Fatalf("the link-local stand-in on %s: %v %s", n.Name, err, out)
		}
		t.Cleanup(func() {
			_ = exec.Command("docker", "exec", n.Name, "ip", "addr", "del", linkLocal+"/32", "dev", "lo").Run() //nolint:gosec // kind's node
		})
	}
	pods := map[string]*corev1.Pod{
		BuildNamespace: asNobody(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-kind-build"}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "busybox", Image: probeImage, Command: []string{"sleep", "3600"}}}}}),
	}
	if onKind { // a link-local stand-in needs a node this test can reach into: kind's alone
		pods["default"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-kind-linklocal"}, Spec: corev1.PodSpec{HostNetwork: true,
			Containers: []corev1.Container{{Name: "busybox", Image: probeImage, Command: []string{"httpd", "-f", "-p", linkLocal + ":80", "-h", "/etc"}}}}}
	}
	for ns, pod := range pods {
		if _, err := kind.client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = kind.client.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
		})
		if out, err := kubectl("-n", ns, "wait", "--for=condition=ContainersReady", "pod/"+pod.Name, "--timeout=3m"); err != nil {
			t.Fatalf("%s/%s: %v %s", ns, pod.Name, err, out)
		}
	}
	web := kindPods(t, "canyon", "canyon.kind-web=true")
	if len(web) != 1 {
		t.Fatalf("%d web helper pods", len(web))
	}
	type addr struct {
		host string
		port int
	}
	answering := []addr{{web[0].Status.PodIP, 80}, {web[0].Status.PodIP, 443}}
	if onKind {
		answering = append(answering, addr{linkLocal, 80})
	}
	for _, to := range answering {
		if ok, _ := reach(t, "canyon", "canyon-kind-stray", "busybox", to.host, to.port); !ok {
			t.Fatalf("%s:%d answers no pod: a build's not reaching it proves nothing", to.host, to.port)
		}
		if !blocked(t, BuildNamespace, "canyon-kind-build", "busybox", to.host, to.port) {
			t.Errorf("a build pod reached %s:%d", to.host, to.port)
		}
	}
	if !blocked(t, BuildNamespace, "canyon-kind-build", "busybox", "169.254.169.254", 80) {
		t.Error("a build pod reached the metadata address, 169.254.169.254:80")
	}
	if ok, _ := reach(t, BuildNamespace, "canyon-kind-build", "busybox", "1.1.1.1", 443); !ok {
		t.Error("a build pod did not reach 1.1.1.1:443: a policy that lets nothing out proves nothing")
	}
}

// TestKindProbeTellsEnforcement: /probe answers what this cluster does with NetworkPolicy.
func TestKindProbeTellsEnforcement(t *testing.T) {
	want := os.Getenv("CANYON_KUBE_TEST_NETWORK_POLICY")
	if want == "" {
		want = "enforced"
	}
	status, out := call(t, http.MethodPost, "/probe", map[string]string{"domain": "kind.test", "form": "app.env.domain"})
	var res ProbeResult
	if status != http.StatusOK || json.Unmarshal(out, &res) != nil || res.NetworkPolicy != want {
		t.Fatalf("/probe: %d %s, want network_policy %s", status, out, want)
	}
}

// TestKindAccessReachesThePodWithoutARestart: PUT /access rewrites the ConfigMap, and the file the
// sidecar reads in the running pod follows, with no restart; an older updated_at is 409 and changes
// nothing. The file is read from kind's node, a docker container.
func TestKindAccessReachesThePodWithoutARestart(t *testing.T) {
	a := app(t, "kind-a")
	body := func(updated string) map[string]any {
		return map[string]any{"app": "kind-a", "target": "preview", "access": accessFile(updated)}
	}
	if status, out := call(t, http.MethodPut, "/access", body("2026-09-26T11:00:00Z")); status != http.StatusNoContent {
		t.Fatalf("PUT /access: %d %s", status, out)
	}
	if status, out := call(t, http.MethodPut, "/access", body("2026-09-26T10:30:00Z")); status != http.StatusConflict {
		t.Errorf("an older PUT /access: %d %s, not 409", status, out)
	}
	// What the sidecar sees, through its own mount namespace: a subPath mount would never change.
	sidecar := `cat /proc/$(crictl inspect -o go-template --template '{{.info.pid}}' $(crictl ps -q --name '^canyon-identity$' --label io.kubernetes.pod.uid=` +
		string(a.pod.UID) + `))/root/etc/canyon/access.json`
	var out []byte
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		if out, _ = exec.Command("docker", "exec", a.pod.Spec.NodeName, "sh", "-c", sidecar).CombinedOutput(); strings.Contains(string(out), "2026-09-26T11:00:00Z") { //nolint:gosec // kind's node, the pod's own sidecar
			break
		}
	}
	if !strings.Contains(string(out), "2026-09-26T11:00:00Z") {
		t.Fatalf("the pod's access file never took the new updated_at: %s", out)
	}
	p, err := kind.client.CoreV1().Pods(Namespace).Get(context.Background(), a.pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if p.UID != a.pod.UID || s.RestartCount != 0 {
			t.Errorf("the pod was replaced or restarted to take the access file: %s %d", s.Name, s.RestartCount)
		}
	}
}

// TestKindLogsReadTheAppContainerAlone is invariant 5 on a real pod: /logs is the app container's
// log, never the sidecar's, which has just refused a request.
func TestKindLogsReadTheAppContainerAlone(t *testing.T) {
	helperPods(t)
	a := app(t, "kind-a")
	_, _ = kubectl("-n", "canyon", "exec", "canyon-kind-stray", "--", "wget", "-q", "-O", "/dev/null", "-T", "10",
		"--header", "Host: kind-a.preview.kind.test", "http://"+serviceIP(t, "canyon-router")+"/")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, kind.executor+"/logs?app=kind-a&target=preview&tail=100", nil)
	res, err := (&http.Client{Transport: toExecutor}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var got strings.Builder
	_, _ = io.Copy(&got, res.Body) // it follows until ctx ends
	if res.StatusCode != http.StatusOK || !strings.Contains(got.String(), "canyon-kind-app kind-a") || strings.Contains(got.String(), "canyon-identity") {
		t.Errorf("/logs of %s: %d %q", a.pod.Name, res.StatusCode, got.String())
	}
}

// TestKindLogsReadACrashedAppsLastRun: an app that crashes after it came up reads, while its container waits to start
// again, what its last run printed before it ended (contracts/executor.md, A container that isn't running).
func TestKindLogsReadACrashedAppsLastRun(t *testing.T) {
	readCrashed(t, "kind-crash", false, "canyon-kind-crashed\n")
}

// TestKindLogsReadTheEndKeptOfAGoneRun: once the dead container is gone from the node, as the kubelet's garbage
// collection removes it, the kubelet answers 200 with its own sentence; /logs reads the end its status kept instead.
func TestKindLogsReadTheEndKeptOfAGoneRun(t *testing.T) {
	readCrashed(t, "kind-crash-gone", true, "canyon-kind-crashed\ncanyon: the cluster kept only the end of the app's last run\n")
}

// readCrashed reads /logs of an app that crashes after it came up, while its container waits to start again, until the
// body ends with want; gone removes the dead container from its node first.
func readCrashed(t *testing.T, name string, gone bool, want string) {
	t.Helper()
	image := appImageRunning(t, name, "echo canyon-kind-app $CANYON_KIND_APP; httpd -p 3000; sleep 25; echo canyon-kind-crashed; exit 3")
	status, out := call(t, http.MethodPost, "/run", map[string]any{
		"app": name, "target": "preview", "image": image, "env": map[string]string{"CANYON_KIND_APP": name}, "network": "canyon-app-" + name,
		"identity": map[string]string{"issuer": "https://canyon.kind.test", "audience": name, "jwks_url": jwksURL},
		"access":   accessFile("2026-09-26T10:00:00Z"),
		"front_door": map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
			"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"},
	})
	if status != http.StatusOK {
		t.Fatalf("/run %s: %d %s", name, status, out)
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": name, "target": "preview"}) })
	var got string
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		pods, err := kind.client.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "canyon.app=" + name})
		if err != nil {
			t.Fatal(err)
		}
		var dead *corev1.ContainerStateTerminated
		node := ""
		for _, p := range pods.Items {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == "app" && cs.State.Waiting != nil && cs.LastTerminationState.Terminated != nil {
					dead, node = cs.LastTerminationState.Terminated, p.Spec.NodeName
				}
			}
		}
		if dead == nil { // running again, or not yet crashed: its own log is read as today
			continue
		}
		if gone { // gone already on a second pass
			_ = exec.Command("docker", "exec", node, "crictl", "rm", strings.TrimPrefix(dead.ContainerID, "containerd://")).Run() //nolint:gosec // kind's node, the app's dead container
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, kind.executor+"/logs?app="+name+"&target=preview&tail=50", nil)
		res, err := (&http.Client{Transport: toExecutor}).Do(req)
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			got = fmt.Sprintf("%d %q", res.StatusCode, body)
			if res.StatusCode == http.StatusOK && strings.HasSuffix(string(body), want) {
				cancel()
				return
			}
		}
		cancel()
	}
	t.Fatalf("/logs of %s never read its last run's lines while it waited to start again: %s", name, got)
}

// TestKindStopIsIdempotent is invariant 6 on a real cluster: /stop twice is 204 twice, and leaves none
// of the app's objects.
func TestKindStopIsIdempotent(t *testing.T) {
	app(t, "kind-b")
	for i := range 2 {
		if status, out := call(t, http.MethodPost, "/stop", map[string]any{"app": "kind-b", "target": "preview"}); status != http.StatusNoContent {
			t.Fatalf("/stop %d: %d %s", i+1, status, out)
		}
	}
	c, ctx := kind.client, context.Background()
	for what, err := range map[string]error{
		"Deployment": get(c.AppsV1().Deployments(Namespace).Get(ctx, "kind-b-preview", metav1.GetOptions{})),
		"Service":    get(c.CoreV1().Services(Namespace).Get(ctx, "kind-b-preview", metav1.GetOptions{})),
		"policy":     get(c.NetworkingV1().NetworkPolicies(Namespace).Get(ctx, "kind-b-preview", metav1.GetOptions{})),
		"ConfigMap":  get(c.CoreV1().ConfigMaps(Namespace).Get(ctx, "canyon-access-kind-b-preview", metav1.GetOptions{})),
		"Secret":     get(c.CoreV1().Secrets(Namespace).Get(ctx, "canyon-env-kind-b-preview", metav1.GetOptions{})),
	} {
		if !apierrors.IsNotFound(err) {
			t.Errorf("the %s of kind-b-preview after /stop: %v", what, err)
		}
	}
	if status, _ := call(t, http.MethodGet, "/running?app=kind-b&target=preview", nil); status != http.StatusNotFound {
		t.Errorf("/running after /stop: %d", status)
	}
}

func get[T any](_ T, err error) error { return err }

// TestKindExecutorReadsNoSecret is invariants 2 and 3: canyon-executor may not read a Secret in any of
// the four namespaces, nor create a pod in canyon; it may create one in canyon-apps.
func TestKindExecutorReadsNoSecret(t *testing.T) {
	for _, ns := range []string{"canyon", "canyon-apps", "canyon-builds", "canyon-scans"} {
		for _, verb := range []string{"get", "list", "watch", "patch", "update"} {
			if can(t, "canyon:canyon-executor", ns, verb, "", "secrets") {
				t.Errorf("canyon-executor may %s Secrets in %s", verb, ns)
			}
		}
	}
	if can(t, "canyon:canyon-executor", "canyon", "create", "", "pods") || !can(t, "canyon:canyon-executor", "canyon-apps", "create", "", "secrets") {
		t.Error("canyon-executor may create pods in canyon, or may not create Secrets in canyon-apps")
	}
}

// TestKindOnlyTheExecutorReadsBuilds is invariant 1's: of the service accounts the deploy writes, only
// canyon-executor may get, list or watch Builds or pods in canyon-builds — and it does what it needs.
func TestKindOnlyTheExecutorReadsBuilds(t *testing.T) {
	for _, sa := range []string{"canyon:canyon-server", "canyon:canyon-router", "canyon-builds:canyon-build"} {
		for _, verb := range []string{"get", "list", "watch"} {
			if can(t, sa, "canyon-builds", verb, "kpack.io", "builds") || can(t, sa, "canyon-builds", verb, "", "pods") {
				t.Errorf("%s may %s Builds or pods in canyon-builds", sa, verb)
			}
		}
	}
	if !can(t, "canyon:canyon-executor", "canyon-builds", "watch", "kpack.io", "builds") || !can(t, "canyon:canyon-executor", "canyon-builds", "get", "", "pods") {
		t.Error("canyon-executor may not watch the Builds or read the build pods it follows")
	}
}

// can asks the API server whether the service account <namespace>:<name> may verb the resource.
func can(t *testing.T, sa, namespace, verb, group, resource string) bool {
	t.Helper()
	review, err := kind.client.AuthorizationV1().SubjectAccessReviews().Create(context.Background(), &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               "system:serviceaccount:" + sa,
		Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + strings.Split(sa, ":")[0], "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Group: group, Resource: resource},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("access review: %w", err))
	}
	return review.Status.Allowed
}

// The router says why it cannot reach an app (contracts/executor.md): an address whose Service is gone,
// as after a delete, is the 404 page, and one whose Service answers nothing the 502 page, never nginx's
// bare Bad Gateway for both (canyonv2-5xyy).
func TestKindTheRouterSaysWhyItCannotReachAnApp(t *testing.T) {
	ctx := context.Background()
	silent := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kind-silent-preview", Namespace: "canyon-apps"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"canyon.app": "kind-no-such-pod"}, Ports: []corev1.ServicePort{{Port: 4180}}}}
	if _, err := kind.client.CoreV1().Services("canyon-apps").Create(ctx, silent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = kind.client.CoreV1().Services("canyon-apps").Delete(context.Background(), silent.Name, metav1.DeleteOptions{})
	}()
	for host, want := range map[string]string{
		"kind-gone-preview.canyon.test":   "No app is running at this address.",
		"kind-silent-preview.canyon.test": "This app isn't answering right now.",
	} {
		out, err := kubectl("-n", "canyon", "exec", routerPod(t), "-c", "nginx", "--", "curl", "-s", "-o", "-", "-w", " %{http_code}", "-H", "Host: "+host, "http://127.0.0.1:8080/")
		code := map[string]string{"kind-gone-preview.canyon.test": " 404", "kind-silent-preview.canyon.test": " 502"}[host]
		if err != nil || !strings.Contains(string(out), want) || !strings.HasSuffix(string(out), code) {
			t.Errorf("%s through the router: %v\n%s", host, err, out)
		}
	}
}

// A trivy scan runs through trivy's real server (contracts/executor.md, On Kubernetes; #1685): the first
// scan makes canyon-trivy from the release's pinned trivy image, waits while it downloads its database,
// then scans as its client, and a known-vulnerable pin is a result. A server that cannot start — a flag
// its trivy does not take — fails at once with its own words, never after the 15 minutes a wait allows.
func TestKindTrivyScansThroughItsServer(t *testing.T) {
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	req := "flask==0.12\n" // CVE-2018-1000656 and others: trivy's database names them
	if err := tw.WriteHeader(&tar.Header{Name: "requirements.txt", Mode: 0o644, Size: int64(len(req)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	freeTheNode(t) // one kind node: the other tests' apps and pods hold the CPU a scan pod requests
	client := &executor.Client{Base: kind.executor, HTTP: &http.Client{Transport: toExecutor}}
	for deadline := time.Now().Add(20 * time.Minute); ; {
		res, err := client.Scan(context.Background(), executor.ScanRequest{App: "kind-a", Package: "pkg_kind01", Tool: "trivy", Settings: &executor.ScanSettings{Scanners: []string{"vuln"}}}, bytes.NewReader(tree.Bytes()), nil)
		var busy *executor.Busy
		switch {
		case errors.As(err, &busy) && time.Now().Before(deadline):
			for _, p := range kindPods(t, ScanNamespace, trivyLabel+"=true") {
				for _, s := range p.Status.ContainerStatuses {
					if ended := s.LastTerminationState.Terminated; s.RestartCount > 0 && ended != nil {
						logs, _ := kubectl("-n", ScanNamespace, "logs", p.Name, "--previous", "--tail", "20")
						t.Fatalf("trivy's server %s restarted (%s, exit %d):\n%s", p.Name, ended.Reason, ended.ExitCode, logs)
					}
				}
			}
			time.Sleep(min(busy.RetryAfter, 15*time.Second))
			continue
		case err != nil:
			node, _ := kubectl("describe", "nodes")
			_, allocated, _ := strings.Cut(string(node), "Allocated resources:")
			t.Fatalf("a trivy scan through its server: %v\nthe node's allocated resources:%.1500s", err, allocated)
		case res.NotFinished != "":
			t.Fatalf("a trivy scan through its server did not finish: %s", res.NotFinished)
		case !bytes.Contains(res.SARIF, []byte("CVE-")):
			t.Fatalf("flask==0.12 scanned clean through trivy's server:\n%.2000s", res.SARIF)
		}
		return
	}
}

func kindPods(t *testing.T, namespace, selector string) []corev1.Pod {
	t.Helper()
	pods, err := kind.client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

// freeTheNode stops the apps the other tests left running and deletes their pods, and waits until none
// is left: kind runs one node, and pods still running or terminating hold the CPU they requested. A test
// after it starts its app and helper pods again: app and helperPods forget what they started.
func freeTheNode(t *testing.T) {
	t.Helper()
	appsMu.Lock()
	clear(apps)
	helpers = sync.Once{}
	appsMu.Unlock()
	for _, app := range []string{"kind-a", "kind-b", "kind-floor", "kind-choice-oidc", "kind-choice-alb", "kind-choice-2"} {
		_, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"})
	}
	_ = kind.client.CoreV1().Pods("canyon").DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "canyon.kind-test=true"})
	holding := func() (n int) { // a pod that has not ended holds its requests, terminating or not
		for _, p := range append(kindPods(t, "canyon", "canyon.kind-test=true"), kindPods(t, "canyon-apps", "")...) {
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				n++
			}
		}
		return n
	}
	for deadline := time.Now().Add(3 * time.Minute); holding() > 0; time.Sleep(2 * time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the other tests' pods are still on the node", holding())
		}
	}
}

// TestKindAppReachesOnlyEnvoy is the floor (design/EGRESS.md §2.2): an app whose /run carries egress reaches
// Canyon's egress gateway on 3128, and through it a public address — by CONNECT and by plain http — and
// nothing else: not that address directly, not a private one directly, and not a private one through Envoy
// by address, by name or by CONNECT; each refusal beside a control that reaches the same place, so none
// passes for a destination that answers no one. No pod but an app's reaches Envoy, its Service or its pods.
// The app's JDK_JAVA_OPTIONS is its own, from its Secret, then the proxy properties; and POST /callers
// names the floored pod by its address, with its run. Envoy's records of the app's calls reach the server's
// receiver on 7781, which a stray pod of canyon cannot reach (contracts/routes.md, Egress control; #2286). Under
// the mode allowlist each name the gateway is asked for is on the app's allowlist, so what refuses the private
// ones is Envoy's floor, never the check's not_allowed.
func TestKindAppReachesOnlyEnvoy(t *testing.T) {
	since := time.Now().UTC().Truncate(time.Second)
	helperPods(t)
	open := app(t, "kind-a")
	status, out, pod := floored(t, "kind-floor", map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{"JDK_JAVA_OPTIONS": "-Xmx64m"})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	const publicName = "one.one.one.one"                       // the check refuses an address (design/EGRESS.md §3.3): a tunnel names its host
	webName := "canyon-kind-web.canyon.svc.cluster.local"      // the whole name: Envoy never searches
	allow(t, "kind-floor", webName, publicName, "example.com") // in this order: a policy that allows example.com allows the others
	exec := func(namespace, pod, container, script string) (string, error) {
		out, err := kubectl("-n", namespace, "exec", pod, "-c", container, "--", "sh", "-c", script)
		return strings.TrimSpace(string(out)), err
	}
	const public = "1.1.1.1"
	if ok, _ := reach(t, Namespace, open.pod.Name, "app", public, 443); !ok {
		t.Fatalf("an app without egress reached no %s:443: this cluster has no way out, and the floor proves nothing", public)
	}
	for _, port := range []int{80, 443} {
		if !blocked(t, Namespace, pod.Name, "app", public, port) {
			t.Errorf("the floored app reached %s:%d past Canyon's egress gateway", public, port)
		}
	}
	web := kindPods(t, "canyon", "canyon.kind-web=true")
	if len(web) != 1 {
		t.Fatalf("%d web helper pods", len(web))
	}
	webIP := web[0].Status.PodIP
	fetch := func(namespace, pod, container, url string) (string, error) { // through whatever proxy variables the container has
		return exec(namespace, pod, container, "wget -q -O - -T 15 "+url)
	}
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(2 * time.Second) { // the allowlist reaches the check with the policy's next version
		if _, err = fetch(Namespace, pod.Name, "app", "http://example.com/"); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Errorf("the floored app did not reach http://example.com/, on its allowlist, through the gateway: %v", err)
	}
	for _, url := range []string{"http://" + webIP + "/hostname", "http://" + webName + "/hostname"} {
		if _, err := fetch("canyon", "canyon-kind-stray", "busybox", url); err != nil {
			t.Fatalf("%s answers no one (%v): Envoy's refusing it proves nothing", url, err)
		}
		if out, err := fetch(Namespace, pod.Name, "app", url); err == nil {
			t.Errorf("the floored app reached %s through the gateway: %s", url, out)
		}
	}
	if !blocked(t, Namespace, pod.Name, "app", webIP, 80) {
		t.Error("the floored app reached a private address directly")
	}
	if line := tunnel(pod, publicName+":443"); !strings.Contains(line, " 200") {
		t.Fatalf("CONNECT %s:443, on the app's allowlist, through the gateway answered %q, not 200: its refusing a private one proves nothing", publicName, line)
	}
	if ok, _ := reach(t, "canyon", "canyon-kind-stray", "busybox", webIP, 443); !ok { // a pod's own 443: a Service's 443 may be another port by the time a policy reads it
		t.Fatalf("%s:443 answers no one: Envoy's refusing a tunnel there proves nothing", webIP)
	}
	if line := tunnel(pod, webIP+":443"); strings.Contains(line, " 200") {
		t.Errorf("CONNECT %s:443, a private address, through the gateway answered %q", webIP, line)
	}
	gateways := kindPods(t, "canyon", "app.kubernetes.io/name=canyon-egress")
	if len(gateways) == 0 {
		t.Fatal("no pod of Canyon's egress gateway")
	}
	if ok, _ := reach(t, Namespace, pod.Name, "app", gateways[0].Status.PodIP, 3128); !ok {
		t.Fatal("the floored app did not reach an Envoy pod: a stray pod's not reaching it proves nothing")
	}
	for _, at := range []string{serviceIP(t, "canyon-egress"), gateways[0].Status.PodIP} {
		if !blocked(t, "canyon", "canyon-kind-stray", "busybox", at, 3128) {
			t.Errorf("a pod that is no app's reached Canyon's egress gateway at %s", at)
		}
	}
	if jvm, err := exec(Namespace, pod.Name, "app", "printenv JDK_JAVA_OPTIONS"); err != nil || !strings.HasPrefix(jvm, "-Xmx64m -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=4184 ") { // the pod's forwarder (design/EGRESS.md §4.3)
		t.Errorf("the floored app's JDK_JAVA_OPTIONS is %q (%v): its own, from its Secret, then the proxy properties", jvm, err)
	}
	status, out = call(t, http.MethodPost, "/callers", map[string]any{"addresses": []string{pod.Status.PodIP, "192.0.2.1"}})
	took := runningSince(*pod) // its earliest running container's start, after the sandbox had the address (#2265)
	if took.IsZero() || pod.Status.StartTime == nil || took.Before(pod.Status.StartTime.Time) {
		t.Fatalf("pod %s: its containers' earliest start %v, its status.startTime %v: want one at or after the other", pod.Name, took, pod.Status.StartTime)
	}
	if want := `{"callers":[{"address":"` + pod.Status.PodIP + `","app":"kind-floor","target":"preview","kind":"web","run":"gr_kind0001","since":"` +
		took.Format(time.RFC3339Nano) + `"}]}` + "\n"; status != http.StatusOK || string(out) != want {
		t.Errorf("POST /callers: %d %s, want %s", status, out, want)
	}
	server := kindPods(t, "canyon", "app.kubernetes.io/name=canyon-server,!canyon.kind-test") // the real server, never the helper labelled as it
	if len(server) != 1 {
		t.Fatalf("%d server pods", len(server))
	}
	base := serverAPI(t)
	var setting struct {
		LastEntryAt *time.Time `json:"last_entry_at"`
	}
	for deadline := time.Now().Add(2 * time.Minute); setting.LastEntryAt == nil || setting.LastEntryAt.Before(since); time.Sleep(2 * time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("the server's receiver took no record of the floored app's calls through Envoy since %s: last_entry_at %v", since.Format(time.RFC3339), setting.LastEntryAt)
		}
		res, err := http.Get(base + "/api/v1/egress") //nolint:noctx // a port-forward the test owns
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(res.Body).Decode(&setting)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/egress: %d %v", res.StatusCode, err)
		}
	}
	if !blocked(t, "canyon", "canyon-kind-stray", "busybox", server[0].Status.PodIP, 7781) {
		t.Error("a pod of canyon that is not Canyon's egress gateway reached the server's receiver on 7781")
	}
}

// TestKindCallWithoutTokenRefused: from layer 3 (design/EGRESS.md §4.3) a floored pod's calls go out through
// canyon-identity's forwarder with the pod's projected token. Before the server marks the env token_only, a call
// straight to the gateway from the pod's own address, with no token, is still the env's, as before layer 3. Once
// marked, that call is unknown_caller, while the forwarder's calls go on: no address names a token_only env.
func TestKindCallWithoutTokenRefused(t *testing.T) {
	const app = "kind-token"
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"}) })
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	allow(t, app, "example.com")
	forwarded := func() (string, error) { // through the app's proxy variables: canyon-identity's forwarder, the pod's token; what wget -S said
		out, err := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "wget", "-S", "-O", "/dev/null", "-T", "15", "http://example.com/")
		return string(out), err
	}
	// why is where a forwarded call that failed ended (#143): what wget -S said; the app's egress_destinations, once an
	// entry of the call made since since has reached the receiver (Envoy sends its access log in batches, seconds apart),
	// or after 30 s without one; and canyon-identity's last lines.
	why := func(said string, since time.Time) string {
		after := "SELECT count(*) FROM egress_destinations WHERE app = '" + app + "' AND last_seen_at >= '" + since.UTC().Format(time.RFC3339Nano) + "'"
		for deadline := time.Now().Add(30 * time.Second); ledger(t, after) == "0" && time.Now().Before(deadline); time.Sleep(time.Second) {
		}
		rows := ledger(t, `SELECT coalesce(string_agg(destination || ':' || port || ' ' || env || ' calls ' || calls || ' ' || last_outcome || ' ' ||
			last_status_class || ' ' || last_protocol || ' [' || array_to_string(last_response_flags, ',') || '] last ' || last_seen_at, '; '), 'none')
			FROM egress_destinations WHERE app = '`+app+`'`)
		identity, _ := kubectl("-n", Namespace, "logs", pod.Name, "-c", "canyon-identity", "--tail=50")
		return fmt.Sprintf("wget -S said:\n%s\nthe app's egress_destinations: %s\ncanyon-identity's last lines:\n%s", said, rows, identity)
	}
	direct := func() string { // straight to the gateway, no Proxy-Authorization: the check's refusal header, or ""
		out, _ := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "sh", "-c",
			`(printf 'GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n'; sleep 3) | nc -w 4 canyon-egress.canyon.svc 3128 | grep -i '^canyon-egress:' || true`)
		return strings.ToLower(strings.TrimSpace(string(out)))
	}
	var err error
	var said string
	var since time.Time
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) { // the allowlist reaches the check with the policy's next version
		since = time.Now()
		if said, err = forwarded(); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("the floored app did not reach http://example.com/, on its allowlist, through its forwarder: %v\n%s", err, why(said, since))
	}
	if refused := direct(); refused != "" {
		t.Fatalf("before the mark, a call from the pod's own address with no token was refused (%s): the address fallback stands until the server marks the env", refused)
	}
	ledger(t, "UPDATE egress_envs SET token_only = true WHERE project_id = '"+app+"' AND env = 'preview'") // the server's mark, as a /run answered token_callers and previous_gone sets it
	var refused string
	for deadline := time.Now().Add(time.Minute); !strings.Contains(refused, "unknown_caller"); time.Sleep(2 * time.Second) { // the mark reaches the check with the policy's next version
		if time.Now().After(deadline) {
			t.Fatalf("a minute after the mark, a call from the pod's own address with no token is answered %q; want unknown_caller: an address named a token_only env", refused)
		}
		refused = direct()
	}
	since = time.Now()
	if said, err := forwarded(); err != nil {
		t.Errorf("after the mark, the forwarder's call with the pod's token failed: %v; want it through, the token naming the env\n%s", err, why(said, since))
	}
}

// forward port-forwards to port of target in canyon for the test, answering the local URL.
func forward(t *testing.T, target string, port int) string {
	t.Helper()
	pf := exec.Command("kubectl", "-n", "canyon", "port-forward", target, ":"+strconv.Itoa(port)) //nolint:gosec // the test's own arguments
	out, err := pf.StdoutPipe()
	if err == nil {
		err = pf.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Process.Kill() })
	line, err := bufio.NewReader(out).ReadString('\n') // Forwarding from 127.0.0.1:<port> -> <port>
	addr, _, ok := strings.Cut(strings.TrimPrefix(line, "Forwarding from "), " ->")
	if err != nil || !ok {
		t.Fatalf("port-forward to %s: %q %v", target, line, err)
	}
	return "http://" + addr
}

// TestKindSilentConnectionsAreClosed: Envoy on Kubernetes caps no source's connections, so a connection that
// never sends a byte is closed after 60 s, its idle timeout, and no app holds one for ever (design/EGRESS.md
// §2.1, #2287). The floored app opens four to the gateway and sends nothing: all four are established at
// 45 s, and none is at 75 s. busybox nc outlives Envoy's close while its stdin is open, so what is counted is
// its sockets that are still established (state 01 in /proc/net/tcp), never its processes.
func TestKindSilentConnectionsAreClosed(t *testing.T) {
	const app = "kind-silent"
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"}) })
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	established := func() (string, error) {
		out, err := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "sh", "-c",
			`ours=" $(for p in $(pidof nc); do ls -l /proc/$p/fd; done | sed -n 's/.*socket:\[\([0-9]*\)\]$/\1/p' | tr '\n' ' ')"; awk -v ours="$ours" '$4 == "01" && index(ours, " " $10 " ")' /proc/net/tcp | wc -l`)
		return strings.TrimSpace(string(out)), err
	}
	if out, err := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "sh", "-c",
		"for i in 1 2 3 4; do (sleep 120 | nc canyon-egress.canyon.svc 3128) >/dev/null 2>&1 & done"); err != nil {
		t.Fatalf("opening four silent connections: %v %s", err, out)
	}
	opened := time.Now()
	time.Sleep(45*time.Second - time.Since(opened))
	if n, err := established(); err != nil || n != "4" {
		t.Fatalf("%s of four silent connections established at 45 s (%v): Envoy's closing them by 75 s proves nothing", n, err)
	}
	time.Sleep(75*time.Second - time.Since(opened))
	if n, err := established(); err != nil || n != "0" {
		t.Errorf("%s of four silent connections still established at 75 s (%v): Envoy holds a connection that sends nothing past its 60 s idle timeout", n, err)
	}
}

// TestKindTunnelWritesItsRowBeforeItsLastEntry: on Kubernetes too, the deployed bootstrap's Envoy logs a tunnel as it
// opens and the receiver takes that entry (#2684): first_seen_at is the open entry's and last_seen_at a later entry's,
// one call between them. An Envoy that logged no open entry writes both from its one entry. It reads the row after the
// tunnel ends, as a public upstream closes an idle tunnel when it likes: TestEnvoyTunnelIsItsRowWhileOpen, on a
// stand-in that test holds, reads the row of one still open.
func TestKindTunnelWritesItsRowBeforeItsLastEntry(t *testing.T) {
	const app, name = "kind-tunnel", "example.com"
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"}) })
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	allow(t, app, name)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(2 * time.Second) { // the allowlist reaches the check with the policy's next version
		_, err := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "wget", "-q", "-O", "/dev/null", "-T", "15", "http://"+name+"/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach http://%s/, on its allowlist: the tunnel below proves nothing: %v", app, name, err)
		}
	}
	if line := tunnel(pod, name+":443"); !strings.Contains(line, " 200") {
		t.Fatalf("CONNECT %s:443, on the app's allowlist, answered %q, not 200", name, line)
	}
	row := func() string {
		return ledger(t, "SELECT calls || ' ' || last_protocol || ' ' || last_status_class || ' ' || (last_seen_at > first_seen_at) FROM egress_destinations WHERE app = '"+app+"' AND env = 'preview' AND destination = '"+name+"' AND port = 443")
	}
	const want = "1 connect 2xx true"
	for deadline := time.Now().Add(30 * time.Second); row() != want; time.Sleep(2 * time.Second) { // a flush, every 10 s
		if time.Now().After(deadline) {
			t.Fatalf("after one tunnel to %s:443 the row is %q, want %q: its calls, its protocol, its class and whether it was written before the tunnel's last entry", name, row(), want)
		}
	}
}

// TestKindCheckMetadataReachesTheReceiver: on Kubernetes too, the check's dynamic metadata — app, env, outcome and
// reason, under envoy.filters.http.ext_authz — reaches the server's receiver on each call's access-log entry, on an
// OK answer and on a refusal alike (contracts/routes.md, Egress control): a namespace or an Envoy that dropped it
// would file nothing, silently. The receiver asks the executor's POST /callers about every call whose metadata
// names no caller, so with the executor scaled to none such a call is no row: the calls made then are the floored
// app's rows through the metadata alone. Under the mode allowlist and no allowlist of the app's, a platform
// destination is answered OK, outcome allowed; another name is refused, not_allowed, and files an egress request,
// which only that reason does; an address is refused, invalid_destination, in every mode.
func TestKindCheckMetadataReachesTheReceiver(t *testing.T) {
	const app = "kind-metadata"
	// One call of each kind, and each one's row: kube.yml holds Envoy to losing no entry on its way to the receiver
	// (#2377). The platform destination is an endpoint Google's discovery names, its door stored (storeDoor), which
	// answers plain http with no redirect.
	const platform, name, address = "oauth2.googleapis.com", "example.org", "1.1.1.1"
	storeDoor(t, googleSignin)
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"}) })
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	fetch := func(url string) error { // through the proxy variables the app's container has
		_, err := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "wget", "-q", "-O", "/dev/null", "-T", "15", url)
		return err
	}
	base := serverAPI(t)
	type row struct {
		Destination string `json:"destination"`
		Port        int    `json:"port"`
		LastOutcome string `json:"last_outcome"`
	}
	read := func() (rows []row, requests []string) {
		var r struct {
			Envs struct {
				Preview struct {
					Destinations []row `json:"destinations"`
					Requests     []struct {
						Destination string `json:"destination"`
					} `json:"requests"`
				} `json:"preview"`
			} `json:"envs"`
		}
		status, out := apiCall(t, base, http.MethodGet, "/api/v1/projects/"+app+"/egress", nil)
		if err := json.Unmarshal(out, &r); status != http.StatusOK || err != nil {
			t.Fatalf("GET /api/v1/projects/%s/egress: %d %v %s", app, status, err, out)
		}
		for _, q := range r.Envs.Preview.Requests {
			requests = append(requests, q.Destination)
		}
		return r.Envs.Preview.Destinations, requests
	}
	outcome := func(rows []row, destination string) string {
		for _, r := range rows {
			if r.Destination == destination && r.Port == 80 {
				return r.LastOutcome
			}
		}
		return ""
	}
	// The mode reaches the check with the policy's next version: once a call to example.com files its request, the
	// check answers under it, and the app's caller is in the policy it holds.
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		_ = fetch("http://example.com/")
		if _, requests := read(); slices.Contains(requests, "example.com") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a call from %s to example.com under the mode allowlist filed no egress request: the calls below prove nothing", app)
		}
	}

	// The window: the server names a caller until it is 30 s unconfirmed, and confirms each every 15 s, so once the
	// executor stops answering the app's caller stays in the policy for 15 s at least — counted from the scale, as
	// the executor's endpoint goes when its pod starts to terminate. The calls are made inside them, and the check
	// names the app on both; past them it could be unknown_caller, which proves nothing: the test fails.
	scaled := time.Now()
	t.Cleanup(func() {
		if out, err := kubectl("-n", "canyon", "scale", "deploy/canyon-executor", "--replicas=1"); err != nil {
			t.Fatalf("scaling the executor back: %v %s", err, out)
		}
		reforward(t)
	})
	if out, err := kubectl("-n", "canyon", "scale", "deploy/canyon-executor", "--replicas=0"); err != nil {
		t.Fatalf("scaling the executor to none: %v %s", err, out)
	}
	if out, err := kubectl("-n", "canyon", "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=canyon-executor", "--timeout=10s"); err != nil {
		t.Fatalf("the executor's pod still runs 10 s after its scale to none: %v %s", err, out)
	}
	// One exec for the three, made at once, so all fit the window. A refused call is read raw, a plain request to the
	// pod's forwarder (design/EGRESS.md §4.3), which sends it on with the pod's token, as the app's proxy variables
	// do: refused when the answer carries the check's own Canyon-Egress header, whatever an upstream says. From layer 3
	// the token names the app, so no call of the pod's enters the policy's callers by its address; one sent past the
	// forwarder would be unknown_caller with the executor at none. The platform destination's call is made as the app
	// makes it, to its end: its row's outcome, the check's, says it was allowed.
	script := fmt.Sprintf(`( wget -q -O /dev/null -T 5 http://%s/ >/dev/null 2>&1; echo called %s ) & `, platform, platform)
	for _, h := range []string{name, address} {
		script += fmt.Sprintf(`( if (printf 'GET http://%s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n'; sleep 3) | nc -w 4 127.0.0.1 4184 | grep -qi '^canyon-egress:'; then echo refused %s; else echo answered %s; fi ) & `, h, h, h, h)
	}
	said, _ := kubectl("-n", Namespace, "exec", pod.Name, "-c", "app", "--", "sh", "-c", script+"wait")
	if !strings.Contains(string(said), "refused "+name) {
		t.Errorf("%s, off the empty allowlist, from %s was answered; want the check's 403 not_allowed\n%s", name, app, said)
	}
	if !strings.Contains(string(said), "refused "+address) {
		t.Errorf("%s, an address, from %s was answered; want the check's 403 invalid_destination\n%s", address, app, said)
	}
	if took := time.Since(scaled); took > 15*time.Second {
		t.Fatalf("the calls were made %v after the executor's scale to none, past the 15 s the app's caller is surely named for", took.Round(time.Second))
	}
	var rows []row
	var requests []string
	for deadline := time.Now().Add(time.Minute); outcome(rows, platform) == "" || outcome(rows, name) == "" || outcome(rows, address) == ""; time.Sleep(2 * time.Second) { // a flush, every 10 s, the executor still at none
		if time.Now().After(deadline) { // kube.yml prints egress-control's log, and Envoy's stats after the suite
			t.Fatalf("with the executor at none, %s's rows are %+v: want %s's, %s's and %s's, which only the check's metadata names the app for",
				app, rows, platform, name, address)
		}
		rows, requests = read()
	}
	if got := outcome(rows, platform); got != "allowed" { // the check's own outcome, as its metadata recorded it
		t.Errorf("%s's row's last_outcome is %q, want the check's allowed", platform, got)
	}
	for _, r := range []string{name, address} {
		if got := outcome(rows, r); got != "refused" {
			t.Errorf("%s's row's last_outcome is %q, want the refusal's refused", r, got)
		}
	}
	if !slices.Contains(requests, name) {
		t.Errorf("%s's not_allowed filed no egress request (%v): the reason did not reach the receiver", name, requests)
	}
}

// The choice tests start an app behind each front door with the floor on, under the org's mode allowlist and no
// allowlist of the app's (design/EGRESS.md §11): its sidecars reach their issuer through Canyon's egress gateway
// alone, the check answering them — canyon-identity behind an OIDC gateway fetches its key set at start, and for
// choice 2 it and oauth2-proxy read the issuer's discovery — so a pod whose sidecars ignore the gateway, or whose
// calls the check refuses, never starts. Each case stores its door as the server's (storeDoor), so its platform
// destinations — the issuer, its key set, the endpoints a choice 2's discovery names, an ALB's key endpoint — are
// each a call the check allows under an empty allowlist. Google's accounts are the issuer: this cluster must reach
// them, which an app without egress shows first. Each app is stopped after its case: kind runs one node.
func TestKindChoice1OIDCPodStartsUnderEmptyAllowlist(t *testing.T) {
	choice(t, "kind-choice-oidc", googleGateway)
}

// An ALB's sidecar fetches nothing at start; its platform destination is its region's key endpoint, which a
// tunnel from the started pod reaches, where a name on no allowlist is refused.
func TestKindChoice1ALBPodStartsUnderEmptyAllowlist(t *testing.T) {
	pod := choice(t, "kind-choice-alb", map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"})
	if pod == nil {
		return
	}
	if line := tunnel(pod, "public-keys.auth.elb.eu-central-1.amazonaws.com:443"); !strings.Contains(line, " 200") {
		t.Errorf("CONNECT to the region's ALB key endpoint from %s answered %q, not 200", pod.Name, line)
	}
	if line := tunnel(pod, "example.com:443"); !strings.Contains(line, " 403") {
		t.Errorf("CONNECT to a name on no allowlist from %s answered %q, not the check's 403 not_allowed", pod.Name, line)
	}
}

// An outbound rule change answers once every live egress-control replica holds it (#2789): the moment the removal
// of an allowlist entry answers, no replica lets its destination through, though the gateway runs two replicas, each
// polling on its own. The entry's allow, a widening, is waited for before it is removed; then a tunnel through each
// replica, by its pod's address, never the Service, which may send them all to one, is refused.
func TestKindANarrowingRefusesFromItsAnswer(t *testing.T) {
	const app = "kind-narrowing"
	pod := choice(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"})
	if pod == nil {
		return
	}
	base, entry := serverAPI(t), "/api/v1/projects/"+app+"/egress/preview/destinations/example.com"
	if status, out := apiCall(t, base, http.MethodPut, entry, nil); status != http.StatusNoContent {
		t.Fatalf("PUT %s: %d %s", entry, status, out)
	}
	for deadline := time.Now().Add(time.Minute); !strings.Contains(tunnel(pod, "example.com:443"), " 200"); {
		if time.Now().After(deadline) {
			t.Fatal("example.com, allowed, was never let through")
		}
	}
	if status, out := apiCall(t, base, http.MethodDelete, entry, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE %s: %d %s", entry, status, out)
	}
	gateways := kindPods(t, "canyon", "app.kubernetes.io/name=canyon-egress")
	if len(gateways) < 2 {
		t.Fatalf("%d gateway replicas, want two at least: one replica can't show a replica behind", len(gateways))
	}
	lines := make(chan [2]string, len(gateways))
	for _, g := range gateways {
		go func() { lines <- [2]string{g.Name, tunnelVia(pod, "example.com:443", g.Status.PodIP)} }()
	}
	for range gateways {
		if got := <-lines; !strings.Contains(got[1], " 403") {
			t.Errorf("a tunnel to example.com through %s once its removal answered: %q, want the check's 403 not_allowed", got[0], got[1])
		}
	}
}

// googleGateway is choice 1 behind an OIDC gateway whose issuer is Google's: its sidecar fetches Google's key set.
var googleGateway = map[string]any{"choice": 1, "gateway": "oidc", "issuer": "https://accounts.google.com", "audience": "kind",
	"jwks_url": "https://www.googleapis.com/oauth2/v3/certs", "cookie": "_kind", "domain": "kind.test", "form": "app.env.domain"}

// googleSignin is choice 2 at Google: its sidecars read Google's discovery, and its sign-in exchanges codes at the
// token endpoint that discovery names.
var googleSignin = map[string]any{"choice": 2, "signin": map[string]string{"issuer": "https://accounts.google.com", "client_id": "kind",
	"client_secret": "kind-client-secret"}, "domain": "kind.test", "form": "app.env.domain"}

// Choice 2's sign-in exchanges its code at the token endpoint Google's discovery names, oauth2.googleapis.com, not
// the issuer's host: from the started pod a tunnel there is answered 200, and one to a name on no allowlist 403.
func TestKindChoice2PodStartsUnderEmptyAllowlist(t *testing.T) {
	pod := choice(t, "kind-choice-2", googleSignin)
	if pod == nil {
		return
	}
	if line := tunnel(pod, "oauth2.googleapis.com:443"); !strings.Contains(line, " 200") {
		t.Errorf("CONNECT to the token endpoint's host from %s answered %q, not 200: the sign-in's code exchange is refused", pod.Name, line)
	}
	if line := tunnel(pod, "example.com:443"); !strings.Contains(line, " 403") {
		t.Errorf("CONNECT to a name on no allowlist from %s answered %q, not the check's 403 not_allowed", pod.Name, line)
	}
}

// choice runs name behind door, floored, and answers its pod, nil when /run refused it.
func choice(t *testing.T, name string, door map[string]any) *corev1.Pod {
	t.Helper()
	open := app(t, "kind-a")
	for _, issuer := range []string{"accounts.google.com", "www.googleapis.com"} {
		if ok, _ := reach(t, Namespace, open.pod.Name, "app", issuer, 443); !ok {
			t.Fatalf("an app without egress reached no %s:443: this cluster has no way out to the issuer, and %s proves nothing", issuer, t.Name())
		}
	}
	t.Cleanup(func() { _, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": name, "target": "preview"}) })
	storeDoor(t, door)
	status, out, pod := floored(t, name, door, map[string]string{})
	if status != http.StatusOK {
		t.Errorf("/run of %s with the floor on, under an empty allowlist: %d %s", name, status, out)
	}
	return pod
}

// withSigninDirect runs the executor with CANYON_KUBE_SIGNIN_DIRECT set to entries for the rest of t, then
// without it, as the other tests run it — the revert registered before the change, so a rollout that fails
// still puts it back. It reads its settings at start, so each change rolls it, and the port-forward follows
// its one new pod once it is Ready.
func withSigninDirect(t *testing.T, entries string) {
	t.Helper()
	set := func(arg string) {
		for _, args := range [][]string{{"set", "env", "deploy/canyon-executor", arg}, {"rollout", "status", "deploy/canyon-executor", "--timeout=5m"}} {
			if out, err := kubectl(append([]string{"-n", "canyon"}, args...)...); err != nil {
				t.Fatalf("kubectl %v: %v %s", args, err, out)
			}
		}
		reforward(t)
	}
	t.Cleanup(func() { set("CANYON_KUBE_SIGNIN_DIRECT-") })
	set("CANYON_KUBE_SIGNIN_DIRECT=" + entries)
}

// reforward waits for the executor's one pod, alone and Ready after a rollout, and port-forwards to it.
func reforward(t *testing.T) {
	t.Helper()
	var pod string
	for deadline := time.Now().Add(2 * time.Minute); pod == ""; time.Sleep(2 * time.Second) {
		if pods := kindPods(t, "canyon", "app.kubernetes.io/name=canyon-executor"); len(pods) == 1 && pods[0].DeletionTimestamp == nil && ready(&pods[0]) {
			pod = pods[0].Name
		} else if time.Now().After(deadline) {
			t.Fatalf("%d executor pods, none alone and Ready, after the rollout", len(pods))
		}
	}
	_ = kind.forward.Process.Kill()
	if err := forwardExecutor("pod/" + pod); err != nil {
		t.Fatal(err)
	}
}

// TestKindSigninPastTheGateway is the direct path to an internal identity provider (contracts/executor.md,
// Sign-in past the gateway; #2316): with CANYON_KUBE_SIGNIN_DIRECT listing a host at CANYON_KUBE_TEST_SIGNIN — a
// bare listener on 443 and 8443 standing in for the provider, at a fixed address as a /32 needs, which no pod's
// is for long — a floored app reaches it on 443, and neither on 8443, nor the neighbouring apps database's
// address on 443 (the rule is one address wide), nor canyon-kind-web's pod, a private address in the cluster.
// Each refusal beside a control that tells it from a closed port. And with the setting on, a floored app whose
// front door signs in at a public provider the setting does not list still starts: its sidecars keep the
// gateway, which is how they reach Google's key set.
func TestKindSigninPastTheGateway(t *testing.T) {
	const app, public = "kind-signin", "kind-signin-public"
	idp, db := os.Getenv("CANYON_KUBE_TEST_SIGNIN"), os.Getenv("CANYON_KUBE_TEST_DB")
	if idp == "" || db == "" {
		t.Fatal("CANYON_KUBE_TEST_SIGNIN or CANYON_KUBE_TEST_DB is unset: no stand-in for an internal identity provider, or no neighbour of it")
	}
	helperPods(t)
	withSigninDirect(t, "signin.kind.test="+idp+"/32")
	t.Cleanup(func() {
		for _, a := range []string{app, public} {
			_, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": a, "target": "preview"})
		}
	})
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	web := kindPods(t, "canyon", "canyon.kind-web=true")
	if len(web) != 1 {
		t.Fatalf("%d web helper pods", len(web))
	}
	webIP := web[0].Status.PodIP
	for _, at := range []struct {
		host string
		port int
	}{{idp, 8443}, {webIP, 443}} {
		if ok, _ := reach(t, "canyon", "canyon-kind-stray", "busybox", at.host, at.port); !ok {
			t.Fatalf("%s:%d answers no one: the floored app's not reaching it proves nothing", at.host, at.port)
		}
	}
	if ok, _ := reach(t, Namespace, pod.Name, "app", idp, 443); !ok {
		t.Errorf("the floored app did not reach the sign-in address %s on 443", idp)
	}
	if !blocked(t, Namespace, pod.Name, "app", idp, 8443) {
		t.Errorf("the floored app reached the sign-in address %s on 8443: 443 alone is admitted", idp)
	}
	if ok, dropped := reach(t, "canyon", "canyon-kind-stray", "busybox", db, 443); ok || dropped {
		t.Fatalf("%s:443 from a pod no policy holds: connected %v, dropped %v; want refused at once, a closed port, so only a policy drops it", db, ok, dropped)
	}
	if !blocked(t, Namespace, pod.Name, "app", db, 443) {
		t.Errorf("the floored app reached %s on 443, beside the sign-in address: the rule is wider than its /32", db)
	}
	if !blocked(t, Namespace, pod.Name, "app", webIP, 443) {
		t.Errorf("the floored app reached %s on 443, a private address the setting does not name", webIP)
	}
	storeDoor(t, googleGateway)
	if status, out, _ := floored(t, public, googleGateway, map[string]string{}); status != http.StatusOK {
		t.Errorf("/run behind a public provider the setting does not list: %d %s; its sidecar must keep the gateway", status, out)
	}
}

// TestKindSigninRuleFromTheChart is the in-cluster sign-in rule as the Helm chart renders it from
// canyon.egress.signin.inCluster (#2326): a provider's own pods in a namespace of their own — a stand-in serving
// 8443 and 9443 — and a floored app that reaches neither until the rendered canyon-apps-signin is applied, then
// 8443 alone; a pod with the same labels in another namespace stays out of its reach on 8443, the rule's
// namespace half. A pod no policy holds reaches each of them, so each refusal is the policy's.
func TestKindSigninRuleFromTheChart(t *testing.T) {
	const app, ns, other = "kind-signin-chart", "kind-idp", "kind-idp-other"
	helperPods(t)
	t.Cleanup(func() {
		_, _ = call(t, http.MethodPost, "/stop", map[string]any{"app": app, "target": "preview"})
		_, _ = kubectl("-n", Namespace, "delete", "networkpolicy", "canyon-apps-signin", "--ignore-not-found")
		for _, n := range []string{ns, other} {
			_, _ = kubectl("delete", "namespace", n, "--ignore-not-found", "--wait=false")
		}
	})
	for _, n := range []string{ns, other} {
		if _, err := kind.client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		idp := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kind-idp", Labels: map[string]string{"app.kubernetes.io/name": "kind-idp"}}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "busybox", Image: probeImage, Command: []string{"sh", "-c", "httpd -p 8443 -h /etc && exec httpd -f -p 9443 -h /etc"}}}}}
		if _, err := kind.client.CoreV1().Pods(n).Create(context.Background(), idp, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		if out, err := kubectl("-n", n, "wait", "--for=condition=Ready", "pod/kind-idp", "--timeout=3m"); err != nil {
			t.Fatalf("the stand-in provider in %s: %v %s", n, err, out)
		}
	}
	at, elsewhere := kindPods(t, ns, "app.kubernetes.io/name=kind-idp")[0].Status.PodIP, kindPods(t, other, "app.kubernetes.io/name=kind-idp")[0].Status.PodIP
	status, out, pod := floored(t, app, map[string]any{"choice": 1, "gateway": "alb", "domain": "kind.test", "form": "app.env.domain",
		"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("/run with egress: %d %s", status, out)
	}
	for _, to := range []struct {
		host string
		port int
	}{{at, 8443}, {at, 9443}, {elsewhere, 8443}} {
		if ok, _ := reach(t, "canyon", "canyon-kind-stray", "busybox", to.host, to.port); !ok {
			t.Fatalf("%s:%d answers no one: the floored app's not reaching it proves nothing", to.host, to.port)
		}
	}
	if !blocked(t, Namespace, pod.Name, "app", at, 8443) {
		t.Fatalf("the floored app reached the provider's pod on 8443 before the chart's rule: the floor is open")
	}
	render := exec.Command("helm", "template", "canyon", "../../deploy/helm/canyon", "-f", "../../deploy/helm/check-values.yaml", "--show-only", "templates/policies/signin.yaml",
		"--set-json", `canyon.egress.signin={"hosts":["kind-idp.kind-idp.svc.cluster.local"],"inCluster":{"namespace":"`+ns+`","podLabels":{"app.kubernetes.io/name":"kind-idp"},"port":8443}}`)
	policy, err := render.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin = bytes.NewReader(policy)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply the chart's canyon-apps-signin: %v %s", err, out)
	}
	var reached bool
	for deadline := time.Now().Add(30 * time.Second); !reached && time.Now().Before(deadline); time.Sleep(2 * time.Second) { // the network plugin takes a policy in seconds
		reached, _ = reach(t, Namespace, pod.Name, "app", at, 8443)
	}
	if !reached {
		t.Errorf("the floored app did not reach the provider's pod on 8443 through the chart's canyon-apps-signin")
	}
	if !blocked(t, Namespace, pod.Name, "app", at, 9443) {
		t.Errorf("the floored app reached the provider's pod on 9443: the chart's rule admits 8443 alone")
	}
	if !blocked(t, Namespace, pod.Name, "app", elsewhere, 8443) {
		t.Errorf("the floored app reached a pod with the provider's labels in %s on 8443: the chart's rule admits %s's alone", other, ns)
	}
}
