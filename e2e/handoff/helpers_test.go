//go:build handoff || handoffrepo || kindcanyon

package handoff

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	app      = "handoff-app"
	domain   = "apps.handoff.test"
	audience = "handoff-gateway" // the gateway's tokens'; Canyon's own are "canyon"'s
	admin    = "admin@handoff.test"
	alice    = "alice@handoff.test"
)

// The journeys' shared pieces (README.md): the stack on this machine — Canyon behind TLS, its CLI, the identity
// provider, the dashboard in Chromium — and the calls each journey makes of them.

// busybox is up.sh's, pinned: the app's base, a shell under the app's one binary.
const busybox = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// clientOf is client-go's standard loading: $KUBECONFIG, as up.sh leaves it.
func clientOf(t *testing.T) kubernetes.Interface {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	must(t, err)
	client, err := kubernetes.NewForConfig(cfg)
	must(t, err)
	return client
}

// pushApp builds the app, testdata/app on busybox, answering on 3000 and keeping its notes in the
// database DATABASE_URL names, pushes it, and answers it by digest as the cluster sees it: the image a
// release carries. The binary is this machine's architecture, the kind node's.
func pushApp(t *testing.T, root, work, push, registry string) string {
	t.Helper()
	dir := filepath.Join(work, "image")
	must(t, os.MkdirAll(dir, 0o750))
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "app"), "./e2e/handoff/testdata/app") //nolint:gosec // this tree's app, into the test's own directory
	cmd.Dir, cmd.Env = root, append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the app: %v\n%s", err, raw)
	}
	must(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+busybox+"\nCOPY app /app\nCMD [\"/app\"]\n"), 0o600))
	tag := push + "/canyon/" + app + ":e2e"
	sh(t, "docker", "build", "-q", "-t", tag, dir)
	sh(t, "docker", "push", "-q", tag)
	_, digest, ok := strings.Cut(strings.TrimSpace(sh(t, "docker", "inspect", "-f", "{{index .RepoDigests 0}}", tag)), "@")
	if !ok {
		t.Fatalf("%s has no digest", tag)
	}
	return registry + "/canyon/" + app + "@" + digest
}

// authority is a throwaway CA, written to a file, and a certificate from it for this machine's address:
// Canyon's public URL, the gateway and the pipeline are served under it; the server, the pipeline and
// the sidecar trust the file.
func authority(t *testing.T, work, hostIP string) (string, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hand-off e2e"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err := x509.ParseCertificate(caDER)
	must(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: hostIP}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP(hostIP)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &key.PublicKey, caKey)
	must(t, err)
	caFile := filepath.Join(work, "ca.pem")
	must(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	return caFile, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}

// build compiles each package into work/bin, named by its last element.
func build(t *testing.T, root, work string, pkgs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pkg := range pkgs {
		name := filepath.Base(pkg)
		out[name] = filepath.Join(work, "bin", name)
		cmd := exec.Command("go", "build", "-o", out[name], pkg) //nolint:gosec // this tree's packages, into the test's own directory
		cmd.Dir = root
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, raw)
		}
	}
	return out
}

// front is a TLS listener on this machine's address, where a pod reaches it (the kind network's
// gateway), for a backend started after its URL is known: Canyon's public URL is its own setting, and
// the pipeline starts with the secret the webhook's save answers. A request waits until to names the
// backend. While cut is set, every request whose path starts with cuts has its connection dropped: on
// Canyon's front "/access/", Canyon unreachable to the sidecar alone; on the pipeline's "/", the platform
// unreachable to Canyon.
func front(t *testing.T, leaf tls.Certificate, hostIP, cuts string) (u string, cut *atomic.Bool, to func(backend string)) {
	t.Helper()
	var proxy http.Handler
	set := make(chan struct{})
	cut = new(atomic.Bool)
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	must(t, err)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-set
		if cut.Load() && strings.HasPrefix(r.URL.Path, cuts) {
			panic(http.ErrAbortHandler)
		}
		proxy.ServeHTTP(w, r)
	})}, TLS: &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return "https://" + ln.Addr().String(), cut, func(backend string) {
		target, err := url.Parse(backend)
		must(t, err)
		proxy = httputil.NewSingleHostReverseProxy(target)
		close(set)
	}
}

// serve starts canyon-server on a loopback port and waits for /ready: on Kubernetes, signing people in
// at handoff.test through the org's identity provider, admin@handoff.test its admin, and trusting the
// CA everything is served under. It answers the server's loopback URL, which only the public URL's TLS
// front uses: the server reads its session cookie by the public URL's name.
func serve(t *testing.T, bin, work, caFile string, env ...string) string {
	t.Helper()
	addr := freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), append(env, "CANYON_ADDR="+addr, "CANYON_RUNTIME=kubernetes", "CANYON_ORG=Handoff", "CANYON_AUTH=oidc", "CANYON_OIDC_CLIENT_ID=canyon",
		"CANYON_OIDC_ALLOWED_DOMAINS=handoff.test", "CANYON_ADMINS="+admin, "CANYON_KEY_FILE="+filepath.Join(work, "kek"), "CANYON_REPOS="+filepath.Join(work, "repos"),
		"CANYON_APPS_JWKS_ADDR="+freePort(t), "SSL_CERT_FILE="+caFile, "DO_NOT_TRACK=1")...)
	started(t, cmd, "canyon-server")
	local := "http://" + addr
	if !within(time.Minute, func() bool {
		res, err := http.Get(local + "/ready") //nolint:noctx // one probe of a loopback server
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}) {
		t.Fatal("canyon-server never became ready")
	}
	return local
}

// started starts cmd, stops it with the test, and prints what it said when the test failed.
func started(t *testing.T, cmd *exec.Cmd, name string) {
	t.Helper()
	logs := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	must(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s:\n%s", name, logs)
		}
	})
}

// idp is the org's identity provider, the test's own, served over TLS on this machine's address: Canyon
// signs people in through it (client "canyon", every sign-in the admin's), and it issues the ID tokens
// the org's gateway puts in front of the app (audience "handoff-gateway"). It checks nothing it is sent.
type idp struct {
	issuer string
	key    *rsa.PrivateKey
	mu     sync.Mutex
	nonce  string // the last sign-in's, from /auth to /token
}

func newIDP(t *testing.T, leaf tls.Certificate, hostIP string) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	set, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "idp-1", Algorithm: "RS256", Use: "sig"}}})
	must(t, err)
	p := &idp{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.issuer, "authorization_endpoint": p.issuer + "/auth", "token_endpoint": p.issuer + "/token", "jwks_uri": p.issuer + "/keys"})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(set) })
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.nonce = q.Get("nonce")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {"handoff"}, "state": {q.Get("state")}}.Encode(), http.StatusFound) //nolint:gosec // the client's own callback, as a real provider sends it
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		nonce := p.nonce
		p.mu.Unlock()
		token, err := p.token(admin, "canyon", nonce)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json") // else the client reads the answer as a form
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "handoff", "token_type": "Bearer", "id_token": token})
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)
	u, _, to := front(t, leaf, hostIP, "")
	p.issuer = u
	to(backend.URL)
	return p
}

// sign is an ID token for email under audience, nonce left out when empty.
func (p *idp) sign(t *testing.T, email, audience, nonce string) string {
	t.Helper()
	token, err := p.token(email, audience, nonce)
	must(t, err)
	return token
}

func (p *idp) token(email, audience, nonce string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "idp-1").WithType("JWT"))
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := map[string]any{"email": email, "email_verified": true}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return jwt.Signed(signer).Claims(jwt.Claims{Issuer: p.issuer, Subject: email, Audience: jwt.Audience{audience}, IssuedAt: jwt.NewNumericDate(now),
		Expiry: jwt.NewNumericDate(now.Add(time.Hour))}).Claims(claims).Serialize()
}

// settings runs one part of e2e/ui's handoff project (H1, H2) in Chromium as the admin signed in: their session cookie
// handed over, Canyon behind the journey's own CA, env the part's inputs. Its output is logged, which holds no value:
// the project keeps no trace, and H1 writes the values to the file it is given alone.
func settings(t *testing.T, part, public string, browser *http.Client, env ...string) {
	t.Helper()
	u, err := url.Parse(public)
	must(t, err)
	var session *http.Cookie
	for _, c := range browser.Jar.Cookies(u) {
		if c.Name == "__Host-canyon_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("the admin's browser holds no session")
	}
	cmd := exec.Command("npx", "playwright", "test", "--project=handoff", "--grep", part+" ") //nolint:gosec // the harness's own runner and a part's name; grep matches the whole title path, file and project included, so no ^
	cmd.Dir = "../ui"
	cmd.Env = append(os.Environ(), append(env, "E2E_CANYON_URL="+public, "PLAYWRIGHT_NO_COPY_PROMPT=1", "HANDOFF_COOKIE_NAME="+session.Name, "HANDOFF_COOKIE_VALUE="+session.Value)...)
	out, err := cmd.CombinedOutput()
	t.Logf("playwright %s:\n%s", part, out)
	if err != nil {
		t.Fatalf("the set-up in Settings, %s: %v", part, err)
	}
}

// signIn is the admin in a browser: /auth/start through the provider and back, the session cookie kept.
func signIn(t *testing.T, public, caFile string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar, Timeout: time.Minute, Transport: trusting(t, caFile)}
	res, err := browser.Get(public + "/auth/start") //nolint:noctx // the client's timeout bounds it
	must(t, err)
	_ = res.Body.Close()
	u, err := url.Parse(public)
	must(t, err)
	for _, c := range jar.Cookies(u) {
		if c.Name == "__Host-canyon_session" {
			return browser
		}
	}
	t.Fatalf("the sign-in ended at %s with no session", res.Request.URL)
	return nil
}

// trusting is a transport that trusts the CA in caFile alone.
func trusting(t *testing.T, caFile string) *http.Transport {
	t.Helper()
	raw, err := os.ReadFile(caFile) //nolint:gosec // the file authority wrote
	must(t, err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(raw)
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

// appSource is the app's directory: its canyon.toml, declaring a postgres database, and a page. The stub
// executor never builds it.
func appSource(t *testing.T, work string) string {
	t.Helper()
	dir := filepath.Join(work, "source")
	must(t, os.MkdirAll(dir, 0o750))
	must(t, os.WriteFile(filepath.Join(dir, "canyon.toml"), []byte("app = \""+app+"\"\n\n[resources]\ndb = { type = \"postgres\" }\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte("handoff\n"), 0o600))
	return dir
}

// command is the CLI in dir against host, with the config home it signs in to and no harness variable,
// so it neither reads this machine's session nor switches to the agent format (as e2e/stack_test.go's),
// trusting the CA. A hung deploy fails the test, which then prints the server's log, before go test's
// timeout panics without it.
func command(t *testing.T, bin, dir, home, host, caFile string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the CLI this test built, with the journey's arguments
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT", "HOME", "XDG_CONFIG_HOME"}, name)
	}), "CANYON_HOST="+host, "HOME="+home, "XDG_CONFIG_HOME="+home, "CANYON_NO_UPDATE_NOTICE=1", "NO_COLOR=1", "SSL_CERT_FILE="+caFile)
	return cmd
}

// run runs cmd to its end and answers all it printed and its exit code.
func run(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%v: %v", cmd.Args, err)
	}
	t.Logf("%s → exit %d\n%s", strings.Join(cmd.Args[1:], " "), cmd.ProcessState.ExitCode(), out)
	return string(out), cmd.ProcessState.ExitCode()
}

// login is canyon login: the code it prints on stderr approved by the browser, and the CLI signed in.
func login(t *testing.T, cmd *exec.Cmd, browser *http.Client, public string) {
	t.Helper()
	stderr, err := cmd.StderrPipe()
	must(t, err)
	must(t, cmd.Start())
	printed := bufio.NewScanner(stderr)
	var code string
	for code == "" && printed.Scan() {
		_, code, _ = strings.Cut(printed.Text(), "Your code:")
		code = strings.TrimSpace(code)
	}
	if code == "" {
		t.Fatal("canyon login printed no code")
	}
	api(t, browser, http.MethodPost, public+"/api/v1/device-grants/"+code+"/approve", nil, http.StatusNoContent, nil)
	for printed.Scan() { // the rest, so the CLI never blocks on a full pipe
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("canyon login: %v", err)
	}
}

// portForward reaches a Service's port in namespace from here, as a person reaches it through
// the org's gateway, and answers its local URL.
func portForward(t *testing.T, namespace, target, port string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "-n", namespace, "port-forward", target, ":"+port) //nolint:gosec // the Service the platform made
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	out := bufio.NewReader(stdout)
	line, err := out.ReadString('\n') // Forwarding from 127.0.0.1:<port> -> 4180
	must(t, err)
	go func() { _, _ = io.Copy(io.Discard, out) }() // a line per connection follows: never let the pipe fill
	addr, _, ok := strings.Cut(strings.TrimPrefix(line, "Forwarding from "), " ->")
	if !ok {
		t.Fatalf("port-forward: %q", line)
	}
	return "http://" + addr
}

// open is one GET of the app's / through its sidecar under the gateway's token, a redirect not followed.
func open(target, token string) (int, string) { return ask(http.MethodGet, target+"/", token) }

// ask is one request to the app through its sidecar under the gateway's token, a redirect not followed.
func ask(method, u, token string) (int, string) {
	req, err := http.NewRequest(method, u, nil) //nolint:noctx // the client's timeout bounds it
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// api is one call to Canyon as the client, answered want, its body decoded into out when out is not
// nil, or the test fails.
func api(t *testing.T, client *http.Client, method, u string, in any, want int, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		must(t, err)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, u, body) //nolint:noctx // the client's timeout bounds it
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode != want {
		t.Fatalf("%s %s: %d %s", method, u, res.StatusCode, raw)
	}
	if out != nil {
		must(t, json.Unmarshal(raw, out))
	}
}

// identityImage is the canyon-identity image up.sh built from this tree, as the executor's deployment
// names it: the sidecar every release carries.
func identityImage(t *testing.T, kube kubernetes.Interface) string {
	t.Helper()
	d, err := kube.AppsV1().Deployments("canyon").Get(t.Context(), "canyon-executor", metav1.GetOptions{})
	must(t, err)
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "CANYON_IMAGE_IDENTITY" {
			return e.Value
		}
	}
	t.Fatal("the executor's deployment names no CANYON_IMAGE_IDENTITY")
	return ""
}

// within asks ok every two seconds until it holds or d has passed.
func within(d time.Duration, ok func() bool) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(2 * time.Second) {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // docker, with the journey's arguments
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func or(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
