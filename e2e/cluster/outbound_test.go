//go:build cluster

package cluster

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOutbound is CASES.md O1, `make e2e-cluster-egress`: beside the suite, on a Canyon that holds other
// people's apps. It needs no fresh install and changes no setting: it deploys one app of its own, under a
// name of its own, reads what it says through `canyon logs` and what Canyon recorded, and deletes it. With
// the host's via off it fails on the raw dial: that is its red.
func TestOutbound(t *testing.T) {
	alice := aliceSignsIn(t, t.TempDir())
	var setting struct{ Via, Mode string }
	alice.read(t, "/api/v1/egress", &setting)
	t.Logf("the host's via is %s, its mode %s", setting.Via, setting.Mode)
	if setting.Mode != "observe" {
		t.Fatalf("the host's mode is %s: O1 needs observe, or Canyon refuses its call", setting.Mode)
	}
	tag := make([]byte, 4)
	if _, err := rand.Read(tag); err != nil {
		t.Fatal(err)
	}
	app := "e2e-outbound-" + hex.EncodeToString(tag) // a deleted id is never free again
	target, err := url.Parse(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { alice.forget(t, app) })
	dir := fixture(t, alice, "f-outbound", app)
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte(host+"/\n"), 0o600); err != nil { //nolint:gosec // the fixture copy this test made
		t.Fatal(err)
	}
	deployDir(t, alice, app, dir)
	waitFor(t, 3*time.Minute, app+" to reach out through Canyon alone", func() (string, bool) {
		out, code := alice.canyon(t, dir, "", "logs", "--env", "preview", "--lines", "50")
		if code != 0 {
			return fmt.Sprintf("canyon logs: exit %d", code), false
		}
		if err := floorHeld(probeAnswers(out)); err != nil {
			return err.Error(), false
		}
		return "", true
	})
	waitFor(t, 90*time.Second, app+"'s call to be recorded", func() (string, bool) {
		var read egressRead
		alice.read(t, "/api/v1/projects/"+app+"/egress", &read)
		if err := read.recorded(target.Hostname()); err != nil {
			return err.Error(), false
		}
		return "", true
	})
	_, records := alice.api(t, "GET", "/api/v1/projects/"+app+"/egress", nil)
	t.Logf("what Canyon recorded: %s", records)
}

// egressRead is GET /api/v1/projects/:id/egress, the keys O1 reads.
type egressRead struct {
	Floor *string `json:"floor"`
	Envs  struct {
		Preview struct {
			Via          *string     `json:"via"`
			Destinations []egressRow `json:"destinations"`
		} `json:"preview"`
	} `json:"envs"`
}

// egressRow is one destination of an env, as stored.
type egressRow struct {
	Destination  string `json:"destination"`
	Port         int    `json:"port"`
	LastOutcome  string `json:"last_outcome"`
	LastProtocol string `json:"last_protocol"`
}

// recorded is nil once preview ran under via canyon, its floor is enforced and host's 443 is a row of a tunnel
// that went through.
func (r egressRead) recorded(host string) error {
	preview := r.Envs.Preview
	if preview.Via == nil || *preview.Via != "canyon" {
		return fmt.Errorf("preview's via is %s, not canyon: the host's via was off at its deploy", text(preview.Via))
	}
	if r.Floor == nil || *r.Floor != "enforced" {
		return fmt.Errorf("floor is %s, not enforced", text(r.Floor))
	}
	for _, d := range preview.Destinations {
		if d.Destination == host && d.Port == 443 {
			if d.LastOutcome != "allowed" || d.LastProtocol != "connect" {
				return fmt.Errorf("%s:443's last call: %s %s, not allowed connect", host, d.LastOutcome, d.LastProtocol)
			}
			return nil
		}
	}
	return fmt.Errorf("no row of %s:443 among %d", host, len(preview.Destinations))
}

func text(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// probeAnswers is the latest answer of each kind e2e/fixtures/outbound-app says: a line `outbound fetch <answer>`
// and a line `outbound dial <answer>`, and "" for one it has not said yet.
func probeAnswers(logs string) (fetch, dial string) {
	for line := range strings.Lines(logs) {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "outbound" {
			switch f[1] {
			case "fetch":
				fetch = f[2]
			case "dial":
				dial = f[2]
			}
		}
	}
	return fetch, dial
}

// floorHeld is nil when the app's own client got an answer from outside and its raw dial timed out. A fetch
// answer is the target's status: the fixture says anything else as a word.
func floorHeld(fetch, dial string) error {
	if status, err := strconv.Atoi(fetch); err != nil || status < 100 || status > 599 {
		return fmt.Errorf("fetch answered %q, not an HTTP status", fetch)
	}
	if dial != "timeout" {
		return fmt.Errorf("the raw dial answered %q, not timeout: is the host's via canyon?", dial)
	}
	return nil
}

// A floored app and one under via off, by what each says.
func TestUnitFloorHeld(t *testing.T) {
	for _, c := range []struct {
		name, logs string
		held       bool
	}{
		{"floored", "outbound fetch 200\noutbound dial timeout\n", true},
		{"floored, answered a redirect", "outbound fetch 302\noutbound dial timeout\n", true},
		{"its first call before its address reached the policy, then floored", "outbound fetch TypeError\noutbound dial timeout\noutbound fetch 200\noutbound dial timeout\n", true},
		{"via off: the raw dial connects", "outbound fetch 200\noutbound dial connected\n", false},
		{"the gateway refuses it", "outbound fetch ERR_TUNNEL\noutbound dial timeout\n", false},
		{"said nothing yet", "", false},
		{"said half", "outbound fetch 200\n", false},
		{"another line", "outbound fetch 200 ok\noutbound dial timeout\n", false},
	} {
		if err := floorHeld(probeAnswers(c.logs)); (err == nil) != c.held {
			t.Errorf("%s: %v; want held %t", c.name, err, c.held)
		}
	}
}

func TestUnitEgressRecorded(t *testing.T) {
	canyon, enforced, off, open := "canyon", "enforced", "off", "not_enforced"
	row := func(destination, outcome string) egressRead {
		var r egressRead
		r.Floor, r.Envs.Preview.Via = &enforced, &canyon
		r.Envs.Preview.Destinations = []egressRow{{destination, 443, outcome, "connect"}}
		return r
	}
	if err := row("canyon.example.com", "allowed").recorded("canyon.example.com"); err != nil {
		t.Errorf("a recorded call: %v", err)
	}
	viaOff, notEnforced := row("canyon.example.com", "allowed"), row("canyon.example.com", "allowed")
	viaOff.Envs.Preview.Via, notEnforced.Floor = &off, &open
	for name, r := range map[string]egressRead{"via off": viaOff, "a floor not enforced": notEnforced, "another row": row("other.example.com", "allowed"), "refused": row("canyon.example.com", "refused"), "nothing": {}} {
		if r.recorded("canyon.example.com") == nil {
			t.Errorf("%s reads as recorded", name)
		}
	}
}

// outboundApp runs e2e/fixtures/outbound-app in node with target.txt and env, and answers what it says of each
// kind once it has said both.
func outboundApp(t *testing.T, target string, env ...string) (fetch, dial string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../fixtures/outbound-app")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte(target+"\n"), 0o600); err != nil { //nolint:gosec // this test's own temp directory
		t.Fatal(err)
	}
	cmd := exec.Command("node", "server.js") //nolint:gosec // the fixture's own entry point
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(os.Environ()), append(env, "PORT=0")...)
	var said bytes.Buffer
	logs := &syncWriter{w: &said}
	cmd.Stdout, cmd.Stderr = logs, logs
	read := func() string {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return said.String()
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if fetch, dial = probeAnswers(read()); fetch != "" && dial != "" {
			return fetch, dial
		}
	}
	t.Fatalf("the app said %q in 20 s", read())
	return "", ""
}

// The fixture itself: what it says under via off, where both reach the target; and through a proxy, where
// Node's fetch alone reaches a name nothing else resolves, which is how an app under the floor goes.
func TestUnitOutboundAppSays(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer direct.Close()
	if fetch, dial := outboundApp(t, direct.URL+"/"); fetch != "200" || dial != "connected" {
		t.Errorf("straight to the target: fetch %q, dial %q; want 200 and connected", fetch, dial)
	}
	var asked atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // a tunnel, as Envoy opens one, to a target that answers 204
		asked.Store(r.Method + " " + r.Host)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\n\r\n")
		_ = rw.Flush()
		if _, err := http.ReadRequest(rw.Reader); err == nil {
			_, _ = rw.WriteString("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
			_ = rw.Flush()
		}
	}))
	defer proxy.Close()
	fetch, dial := outboundApp(t, "http://outbound.invalid/", "NODE_USE_ENV_PROXY=1", "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL)
	if fetch != "204" || dial != "unresolved" || asked.Load() != "CONNECT outbound.invalid:80" {
		t.Errorf("through a proxy: fetch %q, dial %q, the proxy asked %v; want 204, unresolved and CONNECT outbound.invalid:80", fetch, dial, asked.Load())
	}
	// Nothing listening: the local port of a connection this test holds open, where nothing listens, so a connection
	// to it is refused. A port listened on and closed again was free for the next bind to port 0, the fixture's own
	// server on PORT=0 among them, which then answered 200 there (#2712); on Linux, where CI runs this, nothing else
	// can bind the port this connection holds.
	hold, err := net.Dial("tcp", direct.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Close() }()
	held := hold.LocalAddr().String()
	if fetch, dial := outboundApp(t, "http://"+held+"/"); fetch != "ECONNREFUSED" || dial != "refused" {
		t.Errorf("nothing listening: fetch %q, dial %q; want ECONNREFUSED and refused", fetch, dial)
	}
}
