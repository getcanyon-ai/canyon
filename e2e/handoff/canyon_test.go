//go:build kindcanyon

// Canyon under run_by: canyon on kind (#2514, #2516): the hand-off journey's stack on this machine — Canyon behind
// TLS on its own Postgres, its CLI, the identity provider — with the cluster's own executor, so the front door's
// probe sees whether the cluster enforces NetworkPolicy, a deploy builds through kpack and runs in canyon-apps. The
// stub answers /scan alone: kind runs no scanner (.github/kind/up.sh), and no scan is what this proves.
// CANYON_KUBE_TEST_NETWORK_POLICY says which cluster this is: enforced (Calico, kpack ready by build-arch.sh), a
// deploy behind a door that passed the probe builds and runs, and the chart's bootstrap saves a door that is not
// enforcing again, finding it enforced; not_enforced (kindnet), a deploy behind a door never seen enforcing fails
// with D5's line and builds nothing, and the bootstrap's save again is refused without failing the upgrade, while
// a changed door refused fails it. A door that passed is seeded: under run_by: canyon a save passes only with the
// apps' certificate at their load balancer, which kind doesn't have. README.md says how to run the journey's stack.
package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// door is the front door, choice 1 behind an ALB, as the kind tests' /run carries it: its sidecar fetches nothing at
// its start, so the app's pod is ready on its own port.
var door = map[string]any{"choice": 1, "gateway": "alb", "domain": domain, "form": "app.env.domain",
	"alb_arn": "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/kind/0123456789abcdef"}

// notEnforced is D5's line (design/EXPERIENCE.md), byte for byte: the last build.log line of a deploy on a cluster
// Canyon hasn't seen enforce NetworkPolicy.
const notEnforced = `Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.`

// notEnforcing is D5's probe_note while the stored door is not enforcing (contracts/routes.md, the front-door row).
const notEnforcing = "Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced, so no deploy goes through, whoever runs the apps."

type doorRead struct {
	FrontDoor struct {
		Domain    string `json:"domain"`
		Enforcing bool   `json:"enforcing"`
	} `json:"front_door"`
	ProbeNote string `json:"probe_note"`
}

func TestRunByCanyonOnKind(t *testing.T) {
	dbURL := os.Getenv("HANDOFF_DATABASE_URL")
	if dbURL == "" {
		t.Fatal("HANDOFF_DATABASE_URL is Canyon's Postgres for the journey (README.md)")
	}
	enforced := or(os.Getenv("CANYON_KUBE_TEST_NETWORK_POLICY"), "enforced") == "enforced"
	hostIP := or(os.Getenv("HANDOFF_HOST_IP"), "172.30.0.1")
	root, err := filepath.Abs("../..")
	must(t, err)
	work := t.TempDir()
	kube := clientOf(t)
	caFile, leaf := authority(t, work, hostIP)
	bin := build(t, root, work, "./cmd/canyon-server", "./cmd/canyon")

	idp := newIDP(t, leaf, hostIP)
	public, _, toCanyon := front(t, leaf, hostIP, "")
	executor, sent := clusterExecutor(t, executortest.New(t).URL)
	toCanyon(serve(t, bin["canyon-server"], work, caFile, "CANYON_DATABASE_URL="+dbURL, "CANYON_PUBLIC_URL="+public,
		"CANYON_EXECUTOR_URL="+executor, "CANYON_IMAGE_IDENTITY="+identityImage(t, kube), "CANYON_OIDC_ISSUER="+idp.issuer))
	browser := signIn(t, public, caFile)
	api(t, browser, http.MethodPut, public+"/api/v1/preferences", map[string]any{"onboarded_at": time.Now().UTC().Format(time.RFC3339)}, http.StatusOK, nil)
	source, home := buildSource(t, work), t.TempDir()
	cli := func(args ...string) *exec.Cmd {
		return command(t, bin["canyon"], source, home, public, caFile, args...)
	}
	login(t, cli("login", public), browser, public)
	read := func() doorRead {
		var got doorRead
		api(t, browser, http.MethodGet, public+"/api/v1/front-door", nil, http.StatusOK, &got)
		return got
	}
	boot := func(d map[string]any) (string, error) { return bootstrap(t, root, public, caFile, browser, d) }

	if enforced {
		// #2516: the door as a save that passed the probe leaves it, probed and seen enforcing — such a save under
		// run_by: canyon needs the apps' certificate, at a load balancer kind doesn't have — and a deploy through the
		// server builds and runs.
		seed(t, dbURL, true)
		live := "Live   https://" + app + ".preview." + domain + " · answered 200 OK at /" // the app, through its sidecar
		seen := watchBuilds(t)
		if out, code := run(t, cli("deploy", "--create")); code != 0 || !strings.Contains(out, live) {
			t.Fatalf("canyon deploy --create under run_by: canyon ended %d, not %q", code, live)
		}
		if seen() == "" {
			t.Error("the deploy went live, but kpack made no Build in canyon-builds")
		}
		if !running(t, kube) {
			t.Error("the deploy went live, but no pod of the app runs in canyon-apps")
		}

		// #2514: a door stored before Canyon recorded enforcement reads not enforcing; the chart's bootstrap, with no
		// forceUpdate, saves it again, and the probe finds NetworkPolicy enforced. With a platform webhook saved the
		// save is written whatever the certificate (contracts/routes.md, the front-door row), which kind can't pass.
		api(t, browser, http.MethodPut, public+"/api/v1/platform-webhook", map[string]string{"url": public + "/never-delivered"}, http.StatusOK, nil)
		ledger(t, dbURL, "UPDATE front_doors SET network_policy_enforced_at = NULL")
		if read().FrontDoor.Enforcing {
			t.Fatal("the door the test cleared still reads enforcing")
		}
		if _, err := boot(door); err != nil {
			t.Fatalf("the bootstrap's front-door step failed on a cluster enforcing NetworkPolicy: %v", err)
		}
		if got := read(); !got.FrontDoor.Enforcing {
			t.Errorf("after the bootstrap with no forceUpdate the door reads not enforcing: %+v", got)
		}
		return
	}

	// #2516: a door stored as one saved before Canyon recorded enforcement — probed, never seen enforcing — on a
	// cluster that doesn't enforce NetworkPolicy: a deploy fails with D5's line, and nothing builds.
	seed(t, dbURL, false)
	seen := watchBuilds(t)
	if out, code := run(t, cli("deploy", "--create")); code == 0 || !strings.Contains(out, notEnforced) {
		t.Errorf("canyon deploy --create behind a door never seen enforcing ended %d, not with D5's line", code)
	}
	time.Sleep(20 * time.Second) // a Build sent late would land in this
	if b, n := seen(), sent.Load(); b != "" || n != 0 {
		t.Fatalf("a deploy behind a door never seen enforcing made %d build call(s) and made Builds %q", n, b)
	}

	// #2514: the bootstrap saves the unchanged door again; the probe refuses it, which fails closed — the door is not
	// enforcing, with D5's probe_note — and the upgrade still completes, upgrade after upgrade: the refusal clears the
	// stored door's verdict, never the door, so the next one sees it unchanged. A changed door refused still fails it.
	for _, upgrade := range []string{"first", "second"} {
		out, err := boot(door)
		if err != nil || !strings.Contains(out, notEnforcing) {
			t.Errorf("the %s upgrade's bootstrap saving an unchanged door again on a cluster not enforcing NetworkPolicy: %v, its log naming D5's probe_note: %t", upgrade, err, strings.Contains(out, notEnforcing))
		}
		if got := read(); got.FrontDoor.Enforcing || !strings.HasPrefix(got.ProbeNote, notEnforcing) || got.FrontDoor.Domain != domain {
			t.Errorf("after the %s upgrade's bootstrap the door reads %+v: want the same door, not enforcing, D5's probe_note", upgrade, got)
		}
	}
	changed := maps.Clone(door)
	changed["domain"] = "other.handoff.test"
	if _, err := boot(changed); err == nil {
		t.Error("the bootstrap saving a changed door the probe refuses exited 0: the upgrade must fail")
	}
	if got := read(); got.FrontDoor.Domain != domain {
		t.Errorf("the refused change was stored: %+v", got)
	}
}

// clusterExecutor is the executor Canyon calls: the cluster's own, port-forwarded and kept so, but for /scan, which
// the stub at stub answers. builds counts the calls of a build Canyon sends: /builder and /build.
func clusterExecutor(t *testing.T, stub string) (u string, builds *atomic.Int32) {
	t.Helper()
	cluster, err := url.Parse(keptForward(t, "canyon", "deploy/canyon-executor", "7778"))
	must(t, err)
	scans, err := url.Parse(stub)
	must(t, err)
	toCluster, toStub := httputil.NewSingleHostReverseProxy(cluster), httputil.NewSingleHostReverseProxy(scans)
	toCluster.FlushInterval = -1 // /build streams its log
	toCluster.Transport = replayed{next: http.DefaultTransport, logf: t.Logf}
	builds = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/build") { // /builder, which a build asks first, and /build
			builds.Add(1)
		}
		if r.URL.Path == "/scan" {
			toStub.ServeHTTP(w, r)
			return
		}
		toCluster.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, builds
}

// keptForward is portForward kept up for the test's life. In Canyon the server reaches the executor in the cluster;
// here, through one kubectl port-forward, which exits once a connection it forwards breaks: at 15:00:31 in #2623's
// job it did, mid-probe, the executor running on unrestarted, and every call after was refused. So kubectl is
// started again on the same local port, and why it stopped is logged. It answers once the port takes connections.
func keptForward(t *testing.T, namespace, target, port string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	local := l.Addr().String()
	must(t, l.Close())
	_, localPort, _ := net.SplitHostPort(local)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { stop(); <-done })
	go func() {
		defer close(done)
		for {
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace, "port-forward", target, localPort+":"+port) //nolint:gosec // Canyon's executor
			cmd.Stderr = &stderr
			err := cmd.Run()
			if ctx.Err() != nil {
				return
			}
			t.Logf("kubectl port-forward %s stopped (%v): %s; forwarding it again", target, err, strings.TrimSpace(stderr.String()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", local, time.Second); err == nil {
			_ = c.Close()
			return "http://" + local
		} else if time.Now().After(deadline) {
			t.Fatalf("kubectl port-forward %s never took connections on %s: %v", target, local, err)
		}
	}
}

// replayed sends again, for up to a minute, a request keptForward's kubectl dropped: one refused while it starts
// again, which never reached the executor, or — a GET, a PUT or POST /probe, each one Canyon may send twice — one
// cut mid-way. A body is held to be sent again up to 1 MiB; /build streams its tree and is never sent again.
type replayed struct {
	next http.RoundTripper
	logf func(string, ...any)
}

func (r replayed) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		if req.ContentLength < 0 || req.ContentLength > 1<<20 {
			return r.next.RoundTrip(req)
		}
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
	}
	repeatable := req.Method == http.MethodGet || req.Method == http.MethodPut || req.Method == http.MethodPost && req.URL.Path == "/probe"
	for deadline := time.Now().Add(time.Minute); ; {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		res, err := r.next.RoundTrip(req)
		var op *net.OpError
		refused := errors.As(err, &op) && op.Op == "dial"
		if err == nil || !refused && !repeatable || time.Now().After(deadline) || req.Context().Err() != nil {
			return res, err
		}
		r.logf("the executor's port-forward dropped %s %s (%v): sending it again", req.Method, req.URL.Path, err)
		time.Sleep(2 * time.Second)
	}
}

// buildSource is an app kpack builds: a Go server answering on $PORT, as build-arch's.
func buildSource(t *testing.T, work string) string {
	t.Helper()
	dir := filepath.Join(work, "source")
	must(t, os.MkdirAll(dir, 0o750))
	for name, body := range map[string]string{
		"canyon.toml": "app = \"" + app + "\"\n",
		"go.mod":      "module " + app + "\n\ngo 1.23\n",
		"main.go": `package main

import (
	"net/http"
	"os"
)

func main() {
	_ = http.ListenAndServe(":"+os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("canyon\n")) }))
}
`,
	} {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

// watchBuilds lists kpack's Builds in canyon-builds every second, from now until seen is called, which answers every
// one listed, "" for none: the executor deletes a Build once it ends.
func watchBuilds(t *testing.T) (seen func() string) {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	listed := make(chan string, 1)
	go func() {
		var names []string
		for ctx.Err() == nil {
			out, _ := exec.CommandContext(ctx, "kubectl", "-n", "canyon-builds", "get", "builds.kpack.io", "-o", "name").Output()
			for _, n := range strings.Fields(string(out)) {
				if !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		listed <- strings.Join(names, " ")
	}()
	return func() string { stop(); return <-listed }
}

// running is whether a pod of the app runs in canyon-apps, where the cluster's executor runs apps.
func running(t *testing.T, kube kubernetes.Interface) bool {
	t.Helper()
	pods, err := kube.CoreV1().Pods("canyon-apps").List(t.Context(), metav1.ListOptions{LabelSelector: "canyon.app=" + app})
	must(t, err)
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			return true
		}
	}
	return false
}

// seed stores door as Canyon's front door, probed, and seen enforcing NetworkPolicy when enforcing.
func seed(t *testing.T, dbURL string, enforcing bool) {
	t.Helper()
	raw, err := json.Marshal(door)
	must(t, err)
	ledger(t, dbURL, `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display, probed_at, network_policy_enforced_at)
		VALUES ($1, 'person', $2, $2, now(), CASE WHEN $3::boolean THEN now() END)`, raw, admin, enforcing)
}

// ledger runs the test's own statement in Canyon's Postgres.
func ledger(t *testing.T, dbURL, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	must(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(t.Context(), sql, args...)
	must(t, err)
}

// bootstrap runs the chart's bootstrap.py, the very file its hook's ConfigMap carries, for its front-door step alone,
// as the admin: the session the hook would have from Pocket ID handed over, the chart's config naming d. It answers
// what the step printed, and an error when it exited other than 0.
func bootstrap(t *testing.T, root, public, caFile string, browser *http.Client, d map[string]any) (string, error) {
	t.Helper()
	chart := filepath.Join(root, "deploy", "helm", "canyon")
	tmpl, err := os.ReadFile(filepath.Join(chart, "templates", "hooks", "bootstrap.yaml")) //nolint:gosec // this tree's chart
	must(t, err)
	if !strings.Contains(string(tmpl), `bootstrap.py: {{ .Files.Get "files/bootstrap.py" | quote }}`) {
		t.Fatal("the chart's bootstrap hook no longer ships files/bootstrap.py: run the file it ships")
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{"pocketId": map[string]string{"url": ""}, "canyon": map[string]string{"url": public}, "frontDoor": d})
	must(t, err)
	must(t, os.WriteFile(cfg, raw, 0o600))
	u, err := url.Parse(public)
	must(t, err)
	var session string
	for _, c := range browser.Jar.Cookies(u) {
		if c.Name == "__Host-canyon_session" {
			session = c.Value
		}
	}
	const step = `import http.cookiejar, os, sys, urllib.parse
sys.path.insert(0, os.environ["BOOTSTRAP_DIR"])
import bootstrap
host = urllib.parse.urlparse(bootstrap.CANYON).hostname
bootstrap.JAR.set_cookie(http.cookiejar.Cookie(0, "__Host-canyon_session", os.environ["BOOTSTRAP_SESSION"], None, False, host, False, False, "/", True, True, None, False, None, None, {}))
bootstrap.set_front_door()
`
	cmd := exec.Command("python3", "-c", step)
	cmd.Env = append(os.Environ(), "CANYON_BOOTSTRAP_CONFIG="+cfg, "BOOTSTRAP_DIR="+filepath.Join(chart, "files"), "BOOTSTRAP_SESSION="+session,
		"STATIC_API_KEY=unused", "SSL_CERT_FILE="+caFile, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	t.Logf("bootstrap.py, front door %v:\n%s", d["domain"], out)
	return string(out), err
}
