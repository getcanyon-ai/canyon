//go:build handoff

// Package handoff is the hand-off journey (design/HAND-OFF.md §9 item 8): Canyon under run_by: org on
// a cluster it runs nothing on. canyon-server runs here as a subprocess on its own Postgres, its
// executor a stub whose /build answers a small app image already pushed to the cluster's registry
// (Canyon's build is proven elsewhere). The org's pipeline (./pipeline) takes the release, fetches the
// access token and runs the app in the cluster beside the real canyon-identity sidecar, which pulls its
// access file from Canyon over TLS. An identity provider of the test's own signs people in to Canyon
// and issues the tokens the org's gateway would put in front of the app (front door choice 1). The app
// declares a postgres database, which the pipeline provides on the org's own Postgres (#2432).
// The cases: canyon deploy ends live, reported by the platform; canyon share reaches the app within a
// minute; the app keeps its data in the platform's database, across a restart, and Canyon made none; a
// sidecar restarted while Canyon is unreachable serves the list it cached, within the grace;
// canyon undeploy has the pod admit nobody before the platform takes it down, and ends with the
// environment removed and the data kept; canyon delete has the platform
// drop the database and its role. README.md says how to run it.
package handoff

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// namespace is where the org's pipeline runs apps.
const namespace = "handoff-apps"

func TestHandOff(t *testing.T) {
	dbURL := os.Getenv("HANDOFF_DATABASE_URL")
	if dbURL == "" {
		t.Fatal("HANDOFF_DATABASE_URL is Canyon's Postgres for the journey (README.md)")
	}
	hostIP := or(os.Getenv("HANDOFF_HOST_IP"), "172.30.0.1") // where a pod reaches this machine: the kind network's gateway
	root, err := filepath.Abs("../..")
	must(t, err)
	work := t.TempDir()
	kube := clientOf(t)
	image := pushApp(t, root, work, or(os.Getenv("HANDOFF_REGISTRY_PUSH"), "localhost:5001"), or(os.Getenv("HANDOFF_REGISTRY"), "172.30.0.200:5000"))
	caFile, leaf := authority(t, work, hostIP)
	bin := build(t, root, work, "./cmd/canyon-server", "./cmd/canyon", "./e2e/handoff/pipeline")
	platformDB := platformPostgres(t, hostIP) // the org's own Postgres: Canyon is never told of it

	// Canyon: the stub executor, and the server behind TLS on this machine's address, its public URL,
	// signing people in through the org's identity provider.
	idp := newIDP(t, leaf, hostIP)
	executor := executortest.New(t)
	executor.Image = image
	public, cut, toCanyon := front(t, leaf, hostIP, "/access/")
	toCanyon(serve(t, bin["canyon-server"], work, caFile, "CANYON_DATABASE_URL="+dbURL, "CANYON_PUBLIC_URL="+public, "CANYON_EXECUTOR_URL="+executor.URL,
		"CANYON_IMAGE_IDENTITY="+identityImage(t, kube), "CANYON_OIDC_ISSUER="+idp.issuer))

	// The org, set up by its admin in a browser, from the dashboard alone (#2439): setup complete, then, clicked
	// through in Settings (e2e/ui/handoff), the platform webhook to the pipeline behind TLS and its two values stored,
	// the gateway as front door choice 1, a test delivery the pipeline answers, and run_by: org.
	browser := signIn(t, public, caFile)
	api(t, browser, http.MethodPut, public+"/api/v1/preferences", map[string]any{"onboarded_at": time.Now().UTC().Format(time.RFC3339)}, http.StatusOK, nil)
	hookURL, cutPipeline, toPipeline := front(t, leaf, hostIP, "/")
	stored := filepath.Join(t.TempDir(), "values.json")
	setUp := func(part string) {
		settings(t, part, public, browser, "HANDOFF_HOOK_URL="+hookURL, "HANDOFF_ISSUER="+idp.issuer, "HANDOFF_AUDIENCE="+audience, "HANDOFF_DOMAIN="+domain, "HANDOFF_VALUES="+stored)
	}
	setUp("H1")
	var hook struct{ Secret, Token string }
	raw, err := os.ReadFile(stored) //nolint:gosec // the file the admin's browser wrote, under t.TempDir
	must(t, err)
	must(t, json.Unmarshal(raw, &hook))
	if hook.Secret == "" || hook.Token == "" {
		t.Fatal("the dialog showed the signing secret and the host token")
	}
	ensureNamespace(t, kube)
	toPipeline(runPipeline(t, bin["pipeline"], caFile, "PIPELINE_WEBHOOK_SECRET="+hook.Secret, "PIPELINE_HOST_TOKEN="+hook.Token, "PIPELINE_DATABASE_URL="+platformDB))
	setUp("H2")
	var prefs struct {
		RunBy string `json:"run_by"`
	}
	if api(t, browser, http.MethodGet, public+"/api/v1/preferences", nil, http.StatusOK, &prefs); prefs.RunBy != "org" {
		t.Fatalf("after the set-up run_by is %q, not org", prefs.RunBy)
	}

	// The admin's CLI, signed in by a code the browser approves.
	source, home := appSource(t, work), t.TempDir()
	cli := func(args ...string) *exec.Cmd {
		return command(t, bin["canyon"], source, home, public, caFile, args...)
	}
	login(t, cli("login", public), browser, public)

	// 1. canyon deploy ends live, reported by the org's platform: --create, since canyon.toml names an
	//    app this fresh host doesn't have yet (cli.md §3.2).
	if out, code := run(t, cli("deploy", "--create")); code != 0 || !strings.Contains(out, "reported by your platform") {
		t.Fatalf("canyon deploy --create under run_by: org ended %d, not live and reported by the platform", code)
	}

	// 2. canyon share reaches the app within a minute: before it the sidecar sends alice to Canyon's
	//    no-access page; after it, within one pull of the access file, the app answers her.
	target, token := portForward(t, namespace, "svc/"+app+"-preview", "80"), idp.sign(t, alice, audience, "")
	if !within(time.Minute, func() bool { code, _ := open(target, token); return code == http.StatusFound }) {
		t.Fatal("before the share the sidecar never sent alice to no-access")
	}
	if _, code := run(t, cli("share", alice)); code != 0 {
		t.Fatalf("canyon share ended %d", code)
	}
	if !within(90*time.Second, func() bool {
		code, body := open(target, token)
		return code == http.StatusOK && strings.Contains(body, "handoff")
	}) {
		t.Fatal("the share did not reach the app in 90 seconds: the sidecar pulls its access file every minute")
	}

	// 2b. A pod checks in (#2452): the sidecar's pull records seen_at on its release, the status read carries it
	//     on the environment the org runs, and canyon status and the app page say a pod checked in.
	if !within(3*time.Minute, func() bool {
		var status struct {
			Envs map[string]struct {
				SeenAt string `json:"seen_at"`
			} `json:"envs"`
		}
		api(t, browser, http.MethodGet, public+"/api/v1/projects/"+app+"/status", nil, http.StatusOK, &status)
		return status.Envs["preview"].SeenAt != ""
	}) {
		t.Fatal("no pod checked in within three minutes: the status read carries no seen_at on preview")
	}
	if out, code := run(t, cli("status")); code != 0 || !strings.Contains(out, "a pod checked in") {
		t.Fatalf("canyon status ended %d and does not say a pod checked in:\n%s", code, out)
	}
	settings(t, "H3", public, browser, "HANDOFF_APP="+app)

	// 3. The database is the platform's (design/HAND-OFF.md §3, Resources): Canyon made none and holds no
	//    credential, the project read names where the platform's is, and the app keeps its data there.
	canyonMadeNone(t, dbURL)
	database := providedDatabase(t, browser, public, platformDB)
	nonce := rand.Text()
	if code, body := ask(http.MethodPost, target+"/notes?v="+nonce, token); code != http.StatusNoContent {
		t.Fatalf("the app did not keep a note in its database: %d %s", code, body)
	}
	if !holds(t, platformDB, database, nonce) {
		t.Fatalf("the note is not in %s, the database the platform provided", database)
	}
	sh(t, "kubectl", "-n", namespace, "rollout", "restart", "deployment/"+app+"-preview")
	sh(t, "kubectl", "-n", namespace, "rollout", "status", "deployment/"+app+"-preview", "--timeout=3m")
	target = portForward(t, namespace, "svc/"+app+"-preview", "80") // the last one forwarded to the pod the restart replaced
	if !within(time.Minute, func() bool {
		code, body := ask(http.MethodGet, target+"/notes", token)
		return code == http.StatusOK && strings.Contains(body, nonce)
	}) {
		t.Fatal("after a restart the app did not read its note back from its database")
	}
	restartWhileCanyonIsUnreachable(t, kube, cut, token)

	// 4. canyon undeploy sends a remove, and from that moment the pod admits nobody, though the platform has
	//    not taken it down (#2450): with the pipeline unreachable the remove waits on its retries, and within
	//    one pull of the access file the sidecar sends alice to no-access. Then the pipeline takes the remove
	//    at its next retry, removes the app and reports it removed, and the environment leaves the project's
	//    status. The data stays: an undeploy's remove never deletes.
	target = portForward(t, namespace, "svc/"+app+"-preview", "80") // the one forwarded before the sidecar's restart stopped
	var code int
	var body string
	if !within(time.Minute, func() bool { code, body = open(target, token); return code == http.StatusOK }) {
		t.Fatalf("before the undeploy the app does not answer alice: %d %.300s", code, body)
	}
	cutPipeline.Store(true)
	if out, code := run(t, cli("undeploy", "--env", "preview")); code != 0 || !strings.Contains(out, "removing · waiting on your platform") {
		t.Fatalf("canyon undeploy under run_by: org ended %d, not waiting on the platform", code)
	}
	if !within(90*time.Second, func() bool { code, _ := open(target, token); return code == http.StatusFound }) {
		t.Fatal("90 seconds after the undeploy the pod the platform has not taken down still admits alice: the sidecar's pull answers the deny-all")
	}
	cutPipeline.Store(false)
	if !within(3*time.Minute, func() bool {
		var status struct{ Envs map[string]json.RawMessage }
		api(t, browser, http.MethodGet, public+"/api/v1/projects/"+app+"/status", nil, http.StatusOK, &status)
		return status.Envs["preview"] == nil
	}) {
		t.Fatal("the environment never left the project's status after the pipeline's removed report")
	}
	if !holds(t, platformDB, database, nonce) {
		t.Fatalf("undeploy lost the data: %s no longer holds the note", database)
	}

	// 5. canyon delete sends a remove that deletes (delete: true), for the environment the org ran even
	//    though it is undeployed; the platform drops the database and its role, and the delete finishes
	//    once it reports removed.
	if !within(3*time.Minute, func() bool {
		out, code := run(t, cli("delete", "--confirm", app))
		return code == 0 && strings.Contains(out, "Deleted "+app)
	}) {
		t.Fatal("canyon delete never finished: the platform never reported its remove removed")
	}
	if left := platformHolds(t, platformDB, database); left != "" {
		t.Fatalf("after the delete the platform still has %s", left)
	}
}

// restartWhileCanyonIsUnreachable: with Canyon's access route cut off from the sidecar, its container stopped
// and started again in the same pod — as an OOM kill or a crash does — serves alice from the list it cached on
// its emptyDir, within the grace, and its log says so; then Canyon is reachable again.
func restartWhileCanyonIsUnreachable(t *testing.T, kube kubernetes.Interface, cut *atomic.Bool, token string) {
	t.Helper()
	pods, err := kube.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{LabelSelector: "app=" + app + "-preview"})
	must(t, err)
	var pod *corev1.Pod
	for i := range pods.Items {
		if p := &pods.Items[i]; p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			pod = p
		}
	}
	if pod == nil {
		t.Fatal("no running pod of the app")
	}
	restarts := func() int32 {
		p, err := kube.CoreV1().Pods(namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
		must(t, err)
		for _, s := range p.Status.ContainerStatuses {
			if s.Name == "sidecar" && s.State.Running != nil {
				return s.RestartCount
			}
		}
		return -1
	}
	before := restarts()
	cut.Store(true)
	defer cut.Store(false)
	// the kind node is a container named for the node: its runtime stops the sidecar, and the kubelet starts it again
	id := strings.TrimSpace(sh(t, "docker", "exec", pod.Spec.NodeName, "crictl", "ps", "-q", "--name", "^sidecar$", "--label", "io.kubernetes.pod.name="+pod.Name))
	if id == "" || strings.Contains(id, "\n") {
		t.Fatalf("the sidecar's container in %s: %q", pod.Name, id)
	}
	sh(t, "docker", "exec", pod.Spec.NodeName, "crictl", "stop", id)
	if !within(2*time.Minute, func() bool { return restarts() > before }) {
		t.Fatal("the sidecar's container was never started again")
	}
	// It listens once it has tried Canyon for 10 s and read its cache: a port-forward opened before then
	// meets a refused connection, after which kubectl stops forwarding.
	var logs string
	if !within(time.Minute, func() bool {
		out, _ := exec.Command("kubectl", "-n", namespace, "logs", pod.Name, "-c", "sidecar").CombinedOutput() //nolint:gosec // the pod the pipeline made
		logs = string(out)
		return strings.Contains(logs, "Canyon unreachable at start; serving the access list cached at")
	}) {
		t.Fatalf("restarted while Canyon was unreachable, the sidecar never said it serves its cache:\n%s", logs)
	}
	target := portForward(t, namespace, "svc/"+app+"-preview", "80") // the one pod, its sidecar new
	var code int
	var body string
	if !within(time.Minute, func() bool { code, body = open(target, token); return code == http.StatusOK }) {
		t.Fatalf("restarted while Canyon was unreachable, the sidecar did not serve alice from its cache: %d %.300s", code, body)
	}
}

// platformPostgres is the org's own Postgres, on this machine's address where pods reach it, and answers
// its admin URL: the pipeline provides databases on it, and the test reads them.
func platformPostgres(t *testing.T, hostIP string) string {
	t.Helper()
	const name = "handoff-platform-db"
	password := rand.Text()
	_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput() // one an earlier run left
	sh(t, "docker", "run", "-d", "--rm", "--name", name, "-p", hostIP+"::5432", "-e", "POSTGRES_PASSWORD="+password, platformPostgresImage)
	t.Cleanup(func() { _, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput() })
	port := strings.TrimSpace(sh(t, "docker", "port", name, "5432/tcp"))
	if _, p, ok := strings.Cut(port, hostIP+":"); ok {
		port = strings.SplitN(p, "\n", 2)[0]
	}
	if !within(time.Minute, func() bool {
		return exec.Command("docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "postgres").Run() == nil
	}) {
		t.Fatal("the platform's Postgres never became ready")
	}
	return "postgres://postgres:" + password + "@" + net.JoinHostPort(hostIP, port) + "/postgres?sslmode=disable"
}

// platformPostgresImage is the org's Postgres, pinned as CI's own is.
const platformPostgresImage = "postgres:16-alpine"

// canyonMadeNone fails the test when Canyon made a resource record or a provisioned secret for the app:
// under run_by: org the platform provides every resource (#2432).
func canyonMadeNone(t *testing.T, dbURL string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dbURL)
	must(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	var records, secrets int
	must(t, conn.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM resources WHERE project_id = $1), (SELECT count(*) FROM secrets WHERE project_id = $1 AND origin = 'provisioned')", app).Scan(&records, &secrets))
	if records != 0 || secrets != 0 {
		t.Fatalf("under run_by: org Canyon made %d resource record(s) and %d provisioned secret(s)", records, secrets)
	}
}

// providedDatabase is the database the project read says the platform provided for the app's db in
// preview, read from its endpoint: on the platform's Postgres, and no credential in it.
func providedDatabase(t *testing.T, browser *http.Client, public, platformDB string) string {
	t.Helper()
	var read struct {
		Project struct {
			Resources []struct{ Name, Type, Env, State, Endpoint string }
		}
	}
	api(t, browser, http.MethodGet, public+"/api/v1/projects/"+app, nil, http.StatusOK, &read)
	admin, err := url.Parse(platformDB)
	must(t, err)
	for _, r := range read.Project.Resources {
		if r.Name != "db" || r.Env != "preview" {
			continue
		}
		at, err := url.Parse(r.Endpoint)
		if r.Type != "postgres" || r.State != "provided" || err != nil || at.Scheme != "postgres" || at.Host != admin.Host || at.User != nil || at.RawQuery != "" || len(at.Path) < 2 {
			t.Fatalf("the project read's db in preview: %+v, not provided by the platform at %s", r, admin.Host)
		}
		return strings.TrimPrefix(at.Path, "/")
	}
	t.Fatalf("the project read names no db in preview: %+v", read.Project.Resources)
	return ""
}

// holds is whether the platform's database named holds the note, read as the platform's admin.
func holds(t *testing.T, platformDB, database, note string) bool {
	t.Helper()
	u, err := url.Parse(platformDB)
	must(t, err)
	u.Path = "/" + database
	conn, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Logf("connecting to %s: %v", database, err)
		return false
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM notes WHERE v = $1", note).Scan(&n); err != nil {
		t.Logf("reading %s: %v", database, err)
		return false
	}
	return n == 1
}

// platformHolds names what the platform's Postgres still has of the database and its role, "" for nothing.
func platformHolds(t *testing.T, platformDB, database string) string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), platformDB)
	must(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	var db, role bool
	must(t, conn.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1), EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", database).Scan(&db, &role))
	switch {
	case db && role:
		return "the database " + database + " and its role"
	case db:
		return "the database " + database
	case role:
		return "the role " + database
	}
	return ""
}

// runPipeline starts the org's pipeline on a loopback port, against the cluster $KUBECONFIG names.
func runPipeline(t *testing.T, bin, caFile string, env ...string) string {
	t.Helper()
	listen := freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), append(env, "PIPELINE_LISTEN="+listen, "PIPELINE_NAMESPACE="+namespace, "PIPELINE_APPS_DOMAIN="+domain,
		"PIPELINE_CA_FILE="+caFile, "SSL_CERT_FILE="+caFile)...)
	started(t, cmd, "pipeline")
	return "http://" + listen
}

func ensureNamespace(t *testing.T, kube kubernetes.Interface) {
	t.Helper()
	_, err := kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	must(t, err)
	t.Cleanup(func() { // t.Context is done by now
		if t.Failed() { // the app's side, before the namespace takes it along
			for _, args := range [][]string{{"get", "pods,events", "-o", "wide"}, {"logs", "-l", "app", "--all-containers", "--prefix", "--tail=50"}, {"logs", "-l", "app", "--all-containers", "--prefix", "--tail=50", "--previous"}} {
				out, _ := exec.Command("kubectl", append([]string{"-n", namespace}, args...)...).CombinedOutput() //nolint:gosec // fixed arguments
				t.Logf("kubectl %s:\n%s", strings.Join(args, " "), out)
			}
		}
		_ = kube.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	})
}
