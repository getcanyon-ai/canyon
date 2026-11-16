//go:build oidc

// Package oidc is the team setup's journey on one Docker server (docs: self-host/docker.md), against the
// stack `make e2e-up OIDC=1` started: the shipped compose file and deploy/docker/team.yml, signing people
// in through the stub's OIDC issuer. An admin signs in in a browser, signs the real canyon CLI in with a
// code they approve and deploys an app; a teammate who signs in is sent to the no-access screen, and once
// the app is shared with them, the app answers them under their own identity token.
package oidc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	host     = "http://localhost:7777" // the stack's public URL, e2e/compose.oidc.yml's
	control  = "http://127.0.0.1:8090" // the stub's /_oidc, which names who signs in next
	admin    = "admin@e2e.test"        // CANYON_ADMINS, e2e/fixtures/gen.sh's oidc.env
	teammate = "dana@e2e.test"         // in CANYON_OIDC_ALLOWED_DOMAINS, an admin of nothing
	caFile   = "../.work/fixtures/oidc-ca.pem"
	caddyAt  = "127.0.0.1:9443" // the team setup's Caddy, its 443 published here (e2e/compose.oidc.yml)
	caddy    = "10.214.4.2"     // Caddy's own address, which deploy/docker/team.yml pins and the server trusts
)

var cli string // the canyon binary TestMain builds

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "canyon-oidc-")
	if err != nil {
		panic(err)
	}
	cli = filepath.Join(dir, "canyon")
	build := exec.Command("go", "build", "-o", cli, "./cmd/canyon") //nolint:gosec // this tree's CLI, into a temp dir
	build.Dir, build.Stdout, build.Stderr = "../..", os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("building the canyon CLI: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestTeamSignsInDeploysAndShares(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	app := "oidc-" + suffix()

	// The admin signs in in a browser, through the issuer and back, and finishes setup.
	browser := signIn(t, admin)
	onboard(t, browser)

	// The CLI signs in with a code the admin's browser approves, then deploys the app as the admin.
	home, source := t.TempDir(), appSource(t, app)
	send(t, browser, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated) // canyon.toml names it, so deploy refuses one the host lacks
	login(t, command(t, home, source, "login", host), browser)
	if out, code := run(t, command(t, home, source, "deploy")); code != 0 {
		t.Fatalf("canyon deploy exited %d:\n%s", code, out)
	}
	address := "http://" + app + ".preview.localhost:7777/"
	if code, body, _ := open(t, browser, address); code != http.StatusOK || !strings.Contains(body, "hello "+admin) {
		t.Fatalf("the admin opening %s: %d %q", address, code, body)
	}

	// Caddy, the team setup's way in, on the guide's own Caddyfile: a certificate for Canyon's host and an app that
	// exists, none for a name Canyon doesn't serve, and Canyon's answer about names never reachable through it.
	for name, want := range map[string]bool{"localhost": true, app + ".preview.localhost": true, app + ".prod.localhost": true,
		"nobody-" + app + ".preview.localhost": false, "a.b.preview.localhost": false, "x.evil.com": false} {
		if got := certified(t, name); got != want {
			t.Errorf("Caddy holds a certificate for %s: %t, want %t", name, got, want)
		}
	}
	if code := through(t, http.MethodGet, "/tls/ask?domain=localhost", nil).StatusCode; code != http.StatusForbidden {
		t.Errorf("Canyon's answer about names, asked through Caddy from outside: %d, want 403", code)
	}

	// Who a request came from is what Caddy saw, never what the client claims and never Caddy itself: a device
	// code asked for through Caddy with a forged X-Forwarded-For is requested from the address Caddy saw.
	res := through(t, http.MethodPost, "/auth/device", map[string]string{"X-Forwarded-For": "203.0.113.7"})
	var grant struct {
		UserCode string `json:"user_code"`
	}
	must(t, json.NewDecoder(res.Body).Decode(&grant))
	_ = res.Body.Close()
	var asked struct {
		RequestedBy string `json:"requested_by"`
	}
	if code := get(t, browser, host+"/api/v1/device-grants/"+grant.UserCode, &asked); code != http.StatusOK {
		t.Fatalf("the device grant asked for through Caddy: %d", code)
	}
	if by, err := netip.ParseAddr(asked.RequestedBy); err != nil || by.String() == "203.0.113.7" || by.String() == caddy {
		t.Errorf("a device code asked for through Caddy is requested from %q: want the address Caddy saw, not the client's claim nor Caddy's own", asked.RequestedBy)
	}
	t.Logf("asked for through Caddy, with a forged X-Forwarded-For: requested from %s", asked.RequestedBy)

	// A teammate signs in and opens the app before it is shared: Canyon sends them to the no-access screen.
	dana := signIn(t, teammate)
	if code, body, hops := open(t, dana, address); !slices.ContainsFunc(hops, func(u string) bool { return strings.HasSuffix(u, "/#/no-access/"+app) }) {
		t.Fatalf("the teammate before the app is shared: %d %q, by way of %v", code, body, hops)
	}

	// Shared with them, the app answers the teammate under their own identity token.
	if out, code := run(t, command(t, home, source, "share", teammate)); code != 0 {
		t.Fatalf("canyon share exited %d:\n%s", code, out)
	}
	if code, body, _ := open(t, dana, address); code != http.StatusOK || !strings.Contains(body, "hello "+teammate) {
		t.Fatalf("the teammate once the app is shared: %d %q", code, body)
	}
}

// onboard finishes setup unless it is finished: onboarded_at is set once and never changed (contracts/routes.md, the
// preferences row), and either journey may run first.
func onboard(t *testing.T, browser *http.Client) {
	t.Helper()
	var prefs struct {
		OnboardedAt *string `json:"onboarded_at"`
	}
	if code := get(t, browser, host+"/api/v1/preferences", &prefs); code != http.StatusOK {
		t.Fatalf("GET /api/v1/preferences: %d", code)
	}
	if prefs.OnboardedAt == nil {
		send(t, browser, http.MethodPut, host+"/api/v1/preferences", map[string]any{"onboarded_at": time.Now().UTC().Format(time.RFC3339)}, http.StatusOK)
	}
}

// signIn is a person in a browser: the issuer told who signs in next, then /auth/start through it and back,
// the session cookie kept. Every name under localhost reaches the stack's published port, as a browser's
// does, and the issuer's certificate is trusted.
func signIn(t *testing.T, email string) *http.Client {
	t.Helper()
	send(t, &http.Client{Timeout: 10 * time.Second}, http.MethodPost, control+"/_oidc", map[string]string{"email": email}, http.StatusNoContent)
	raw, err := os.ReadFile(caFile)
	must(t, err)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		t.Fatalf("%s holds no certificate: make e2e-up OIDC=1 writes it", caFile)
	}
	jar, err := cookiejar.New(nil)
	must(t, err)
	dial := (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	browser := &http.Client{Jar: jar, Timeout: time.Minute, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if name, port, err := net.SplitHostPort(addr); err == nil && (name == "localhost" || strings.HasSuffix(name, ".localhost")) {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return dial(ctx, network, addr)
		},
	}}
	res, err := browser.Get(host + "/auth/start") //nolint:noctx // the client's timeout bounds it
	must(t, err)
	_ = res.Body.Close()
	u, err := url.Parse(host)
	must(t, err)
	for _, c := range jar.Cookies(u) {
		if strings.HasSuffix(c.Name, "canyon_session") {
			return browser
		}
	}
	t.Fatalf("%s's sign-in ended at %s, %d, with no session", email, res.Request.URL, res.StatusCode)
	return nil
}

// certified is whether Caddy, at its published port, completes a TLS handshake for name with a certificate for
// it: one it holds, or gets once Canyon says the name is one it serves. For localhost's names Caddy issues from its
// own CA, which the handshake trusts alone.
func certified(t *testing.T, name string) bool {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", caddyAt, &tls.Config{ServerName: name, RootCAs: caddyRoots(t), MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Logf("%s: %v", name, err)
		return false
	}
	_ = conn.Close()
	return true
}

// caddyRoots is the CA Caddy issues localhost's names from, read from its container.
func caddyRoots(t *testing.T) *x509.CertPool {
	t.Helper()
	raw, err := exec.Command("docker", "exec", "canyon-caddy", "cat", "/data/caddy/pki/authorities/local/root.crt").Output()
	must(t, err)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		t.Fatal("Caddy's local CA holds no certificate")
	}
	return roots
}

// through is one request to Canyon through Caddy, as from outside, with header's values set.
func through(t *testing.T, method, path string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "https://localhost"+path, nil) //nolint:noctx // the client's timeout bounds it
	must(t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: caddyRoots(t), MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, caddyAt)
		},
	}}
	res, err := client.Do(req)
	must(t, err)
	return res
}

// open follows the app's address as a browser does, through Canyon's sign-in for the app, and answers the
// last status, its body and every address it was sent to on the way.
func open(t *testing.T, browser *http.Client, address string) (int, string, []string) {
	t.Helper()
	var hops []string
	c := *browser
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		hops = append(hops, req.URL.String())
		if len(via) >= 10 {
			return errors.New("ten redirects")
		}
		return nil
	}
	res, err := c.Get(address) //nolint:noctx // the client's timeout bounds it
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	return res.StatusCode, string(body), hops
}

// appSource is the app: a page that greets whoever Canyon's identity token names and says the scheme it
// was told, its id fixed in canyon.toml.
func appSource(t *testing.T, app string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"canyon.toml":  "app = \"" + app + "\"\n",
		"package.json": `{"name": "oidc-e2e", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}}` + "\n",
		"server.js": `const http = require("node:http");

http.createServer((req, res) => {
  const token = (req.headers.authorization || "").replace(/^Bearer /, "");
  const claims = token ? JSON.parse(Buffer.from(token.split(".")[1] || "", "base64url").toString() || "{}") : {};
  res.setHeader("Content-Type", "text/plain");
  res.end("hello " + (claims.email || "nobody") + " over " + req.headers["x-forwarded-proto"] + "\n");
}).listen(Number(process.env.PORT) || 3000);
`,
	}
	for name, body := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

// command is the CLI in dir, with the config home it signs in to and no harness variable, so it neither
// reads this machine's session nor switches to the agent format (as e2e/stack_test.go's). A hung one
// fails the test before go test's timeout panics.
func command(t *testing.T, home, dir string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, cli, args...) //nolint:gosec // the CLI TestMain built, with the journey's arguments
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT", "HOME", "XDG_CONFIG_HOME"}, name)
	}), "CANYON_HOST="+host, "HOME="+home, "XDG_CONFIG_HOME="+home, "CANYON_NO_UPDATE_NOTICE=1", "NO_COLOR=1")
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
	t.Logf("canyon %s → exit %d\n%s", strings.Join(cmd.Args[1:], " "), cmd.ProcessState.ExitCode(), out)
	return string(out), cmd.ProcessState.ExitCode()
}

// login is canyon login: the code it prints on stderr approved in the admin's browser, and the CLI signed in.
func login(t *testing.T, cmd *exec.Cmd, browser *http.Client) {
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
	send(t, browser, http.MethodPost, host+"/api/v1/device-grants/"+code+"/approve", nil, http.StatusNoContent)
	for printed.Scan() { // the rest, so the CLI never blocks on a full pipe
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("canyon login: %v", err)
	}
}

// send is one JSON request, failing the test on any status but want.
func send(t *testing.T, client *http.Client, method, u string, in any, want int) {
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
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s: %d %s", method, u, res.StatusCode, raw)
	}
}

// get is one GET, its JSON decoded into out, answering the status; 0 when nothing answered.
func get(t *testing.T, client *http.Client, u string, out any) int {
	t.Helper()
	res, err := client.Get(u) //nolint:noctx // the client's timeout bounds it
	if err != nil {
		return 0
	}
	defer func() { _ = res.Body.Close() }()
	_ = json.NewDecoder(res.Body).Decode(out)
	return res.StatusCode
}

func suffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
