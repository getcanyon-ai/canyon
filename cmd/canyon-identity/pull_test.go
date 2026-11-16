package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/identity"
)

// fixtureToken is the release's access token as the org's pipeline mounts it: a fixture, not a secret.
const fixtureToken = "release-access-token-fixture"

// canyon is the stub Canyon of design/HAND-OFF.md §5: GET /access/:app/:env answers the access file
// under the release's token, 401 {error:"bad_token"} under any other, and nothing while it is down —
// over TLS, as the contract's https_url has it, its certificate in the puller's client alone.
type canyon struct {
	srv   *httptest.Server
	mu    sync.Mutex
	file  []byte
	token string
	down  bool
	fails int // the next fetches that find Canyon unreachable, as a new pod's first ones can
	hits  atomic.Int32
	said  []string // each fetch's Canyon-App header, "" when it carried none
}

func newCanyon(t *testing.T) *canyon {
	c := &canyon{token: fixtureToken}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.said = append(c.said, r.Header.Get("Canyon-App"))
		switch {
		case c.down || c.fails > 0:
			c.fails--
			panic(http.ErrAbortHandler) // the connection dropped: Canyon unreachable
		case r.URL.Path != "/access/claims-portal/preview" || r.Header.Get("Authorization") != "Bearer "+c.token:
			writeErr(w, http.StatusUnauthorized, "bad_token", "")
		default:
			_, _ = w.Write(c.file)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *canyon) set(f func(*canyon)) { c.mu.Lock(); defer c.mu.Unlock(); f(c) }

// pullRig is a sidecar in pull mode against the stub, its clock the test's and its fetch loop not
// started: a test ticks it through run with an interval of its own, or fetches by hand.
type pullRig struct {
	*rig
	cfg    config
	canyon *canyon
	puller *accessPuller
	now    time.Time
}

func newPullRig(t *testing.T, grace string, entries ...identity.Grant) *pullRig {
	p := &pullRig{canyon: newCanyon(t), now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	p.rig = &rig{g: newGateway(t), seen: make(chan http.Header, 64)}
	p.write = func(b []byte) { p.canyon.set(func(c *canyon) { c.file = b }) }
	p.access(t, "2026-09-25T10:00:00Z", entries...)
	env := map[string]string{"CANYON_IDENTITY_ISSUER": "https://canyon.meridian.example", "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview",
		"CANYON_FRONT_DOOR": strings.ReplaceAll(choice1, "GW", p.g.srv.URL),
		"CANYON_ACCESS_URL": p.canyon.srv.URL + "/access/claims-portal/preview", "CANYON_ACCESS_TOKEN": fixtureToken, "CANYON_ACCESS_GRACE": grace}
	c, err := configOf(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	v, err := verifierOf(context.Background(), c.door, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) { p.seen <- req.Header }))
	t.Cleanup(app.Close)
	upstream, _ := url.Parse(app.URL)
	p.key, _ = identity.Generate()
	p.cfg = c
	p.puller = newAccessPuller(c, p.canyon.srv.Client())
	p.puller.now = func() time.Time { return p.now }
	p.puller.cache = filepath.Join(t.TempDir(), "access.json") // the emptyDir the sidecar alone mounts
	p.h = newSidecar(c, v, p.key, p.puller, upstream)
	return p
}

// restart is the sidecar's container started again in the same pod: a new puller on the same cache and
// clock, started as main starts it, its wait short. It answers the start's error; on nil it serves.
func (p *pullRig) restart() error {
	q := newAccessPuller(p.cfg, p.canyon.srv.Client())
	q.now, q.cache = p.puller.now, p.puller.cache
	p.puller = q
	p.h.(*sidecar).access = q
	return startAccess(q, log.New(io.Discard, "", 0), 300*time.Millisecond)
}

// logs captures the sidecar's log lines for the test.
func logs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return &buf
}

var danaOwner = identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Level: "owner"}

// The list is fetched at start, before a request is served, and every interval after: a share made at
// Canyon opens the app within one — and a list older than the one held is taken as it is: Canyon's answer
// stands, where the file reader keeps the newer.
func TestPullModeFetchesAtStartAndAShareLandsWithinAnInterval(t *testing.T) {
	p := newPullRig(t, "1h", danaOwner)
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := p.canyon.hits.Load(); n != 1 {
		t.Fatalf("%d fetches at start, want 1", n)
	}
	sam := p.g.token("sam@meridian.example")
	if rec := p.do("GET", "/", p.g.token("dana@meridian.example")); rec.Code != http.StatusOK {
		t.Fatalf("dana: %d %s", rec.Code, rec.Body)
	}
	if rec := p.do("GET", "/", sam); rec.Code != http.StatusFound {
		t.Fatalf("sam before the share: %d", rec.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.puller.run(ctx, 10*time.Millisecond); close(done) }()
	p.access(t, "2026-09-25T10:00:01Z", danaOwner, identity.Grant{Kind: "email", Grantee: "sam@meridian.example", Level: "viewer"})
	deadline := time.Now().Add(5 * time.Second)
	for p.do("GET", "/", sam).Code != http.StatusOK {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("sam's share never reached the sidecar")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done                              // the loop has returned before the next fetch, by hand
	p.access(t, "2026-09-25T09:00:00Z") // older than the one held — a deny-all, another replica's clock — and Canyon's answer all the same
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := p.do("GET", "/", sam); rec.Code != http.StatusFound {
		t.Fatalf("an older list Canyon answered was set aside: sam %d", rec.Code)
	}
}

// A fetch says the app answered exactly when the app accepted a connection on its port just before: the sidecar
// pulls before the app starts, so its pull alone never proves the app (contracts/executor.md, Pull mode; #2470).
func TestPullModeSaysWhetherTheAppAnswered(t *testing.T) {
	p := newPullRig(t, "1h", danaOwner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.puller.app = ln.Addr().String()
	_ = ln.Close() // nothing listens there now: the app has not started
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln, err = net.Listen("tcp", p.puller.app) // the app listens
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.canyon.mu.Lock()
	defer p.canyon.mu.Unlock()
	if len(p.canyon.said) != 2 || p.canyon.said[0] != "" || p.canyon.said[1] != "answered" {
		t.Fatalf("the fetches said %q: nothing before the app listened, answered after", p.canyon.said)
	}
}

// Through an outage the last list is served for the grace, counted from the FIRST failed fetch — the
// failures the minutes after add do not move it; after it every request is 503 access_unreadable, and
// the next good fetch serves again. A 200 that does not parse, or names no updated_at, is an outage too.
func TestPullModeServesTheLastListThroughAnOutageAndRefusesAfterTheGrace(t *testing.T) {
	for name, outage := range map[string]func(*canyon){
		"unreachable":   func(c *canyon) { c.down = true },
		"malformed":     func(c *canyon) { c.file = []byte("{") },
		"no updated_at": func(c *canyon) { c.file = []byte("{}") },
	} {
		p := newPullRig(t, "1h", danaOwner)
		tok := p.g.token("dana@meridian.example")
		if err := p.puller.fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.canyon.set(outage)
		if err := p.puller.fetch(context.Background()); err == nil {
			t.Fatalf("%s: fetched", name)
		}
		for _, step := range []struct {
			after time.Duration
			code  int
		}{{0, 200}, {30 * time.Minute, 200}, {29 * time.Minute, 200}, {2 * time.Minute, 503}} {
			p.now = p.now.Add(step.after)
			if step.after == 30*time.Minute { // a second failure mid-outage
				if err := p.puller.fetch(context.Background()); err == nil {
					t.Fatalf("%s: fetched", name)
				}
			}
			if rec := p.do("GET", "/", tok); rec.Code != step.code || step.code == 503 && !strings.Contains(rec.Body.String(), "access_unreadable") {
				t.Fatalf("%s, %s into the outage: %d %s, want %d", name, p.now.Sub(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)), rec.Code, rec.Body, step.code)
			}
		}
		p.canyon.set(func(c *canyon) { c.down = false })
		p.access(t, "2026-09-25T10:00:00Z", danaOwner)
		if err := p.puller.fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if rec := p.do("GET", "/", tok); rec.Code != http.StatusOK {
			t.Fatalf("%s, after Canyon is back: %d", name, rec.Code)
		}
	}
}

// A grace of 0 refuses the moment a fetch fails.
func TestPullModeAGraceOfZeroRefusesAtOnce(t *testing.T) {
	p := newPullRig(t, "0", danaOwner)
	tok := p.g.token("dana@meridian.example")
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusOK {
		t.Fatalf("before the outage: %d", rec.Code)
	}
	p.canyon.set(func(c *canyon) { c.down = true })
	_ = p.puller.fetch(context.Background())
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("at once: %d, want 503", rec.Code)
	}
}

// A grace outside 0 to 24h, or none, never starts; nor does pull mode without a token, or with a URL
// outside the contract's https_url — schemeless, plain http (the bearer would go in clear) or one
// carrying userinfo — and the error names the setting. The file reader's environment stays as it was.
func TestPullModeABadGraceNeverStarts(t *testing.T) {
	for _, env := range []map[string]string{
		{"CANYON_ACCESS_GRACE": "25h"}, {"CANYON_ACCESS_GRACE": "-1s"}, {"CANYON_ACCESS_GRACE": "soon"}, {"CANYON_ACCESS_GRACE": ""},
		{"CANYON_ACCESS_GRACE": "1h", "CANYON_ACCESS_TOKEN": ""},
		{"CANYON_ACCESS_URL": "canyon.example/access/claims-portal/preview"},
		{"CANYON_ACCESS_URL": "http://canyon.example/access/claims-portal/preview"},
		{"CANYON_ACCESS_URL": "https://user:tok@canyon.example/access/claims-portal/preview"}, //nolint:gosec // the userinfo is what this row proves refused
	} {
		all := map[string]string{"CANYON_IDENTITY_ISSUER": "https://canyon.example", "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview", "CANYON_FRONT_DOOR": choice1,
			"CANYON_ACCESS_URL": "https://canyon.example/access/claims-portal/preview", "CANYON_ACCESS_TOKEN": fixtureToken, "CANYON_ACCESS_GRACE": "1h"}
		for k, v := range env {
			all[k] = v
		}
		_, err := configOf(func(k string) string { return all[k] })
		if err == nil {
			t.Errorf("started with %v", env)
		} else if _, bad := env["CANYON_ACCESS_URL"]; strings.Contains(err.Error(), fixtureToken) || bad && !strings.Contains(err.Error(), "CANYON_ACCESS_URL") {
			t.Errorf("%v: %v", env, err)
		}
	}
	all := map[string]string{"CANYON_IDENTITY_ISSUER": "https://canyon.example", "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview", "CANYON_FRONT_DOOR": choice1}
	if c, err := configOf(func(k string) string { return all[k] }); err != nil || c.accessURL != "" {
		t.Fatalf("the file reader's environment: %v, %+v", err, c)
	}
}

// A 401 — the token rotated by a newer release, or superseded — is a failed fetch: the start refuses, a
// later one serves the last list for the grace. The token is in no error and no log line.
func TestPullModeA401IsAFailedFetchAndTheTokenIsNeverLogged(t *testing.T) {
	buf := logs(t)
	p := newPullRig(t, "1h", danaOwner)
	p.canyon.set(func(c *canyon) { c.token = "rotated" })
	err := p.puller.fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("the start with a rotated token: %v", err)
	}
	p.canyon.set(func(c *canyon) { c.token = fixtureToken })
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.canyon.set(func(c *canyon) { c.token = "rotated" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.puller.run(ctx, 10*time.Millisecond); close(done) }()
	for p.canyon.hits.Load() < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done // the loop has returned before the clock moves and the log is read
	tok := p.g.token("dana@meridian.example")
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusOK {
		t.Fatalf("within the grace: %d", rec.Code)
	}
	p.now = p.now.Add(time.Hour)
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after the grace: %d", rec.Code)
	}
	if out := buf.String(); !strings.Contains(out, "401") || strings.Contains(out, fixtureToken) || strings.Contains(err.Error(), fixtureToken) {
		t.Fatalf("the log: %q", out)
	}
	var body map[string]string
	_ = json.NewDecoder(p.do("GET", "/", tok).Body).Decode(&body)
	if strings.Contains(body["message"], fixtureToken) {
		t.Fatal("the token is in the 503")
	}
}

// The start fetch is tried again, as the key set's is (#2370), until Canyon answers within the wait, each
// failure logged once. One that never answers, with nothing cached, gives up after the wait with its error.
func TestPullModeTheStartFetchIsTriedAgain(t *testing.T) {
	p := newPullRig(t, "1h", danaOwner)
	p.canyon.set(func(c *canyon) { c.fails = 2 })
	var logged strings.Builder
	if err := startAccess(p.puller, log.New(&logged, "", 0), 2*time.Second); err != nil {
		t.Fatalf("Canyon unreachable twice, then answering: %v", err)
	}
	if n, lines := p.canyon.hits.Load(), strings.Count(logged.String(), "\n"); n < 3 || lines != 2 {
		t.Errorf("%d fetch(es), %d line(s) logged; want two failures, each logged once, then the list:\n%s", n, lines, logged.String())
	}
	if rec := p.do("GET", "/", p.g.token("dana@meridian.example")); rec.Code != http.StatusOK {
		t.Fatalf("after the start: %d %s", rec.Code, rec.Body)
	}

	q := newPullRig(t, "1h", danaOwner)
	q.canyon.set(func(c *canyon) { c.down = true })
	start := time.Now()
	if err := startAccess(q.puller, log.New(io.Discard, "", 0), time.Second); err == nil || !strings.Contains(err.Error(), "access") {
		t.Errorf("Canyon never answering, nothing cached: %v, want the fetch's error", err)
	}
	if took := time.Since(start); took < 900*time.Millisecond || took > 2*time.Second {
		t.Errorf("gave up after %v, want the 1 s bound", took)
	}
}

// A restart while Canyon is unreachable (an OOM kill, a crash) serves the list the last good fetch cached,
// for the grace counted from that fetch: a restart never extends it, so a crash loop cannot. Past it, the
// sidecar never starts, as with nothing cached. The cache is written whole or not at all, the sidecar's alone,
// through one file a crashed write leaves behind and the next one replaces, so a crash loop piles up nothing.
// Once Canyon answers again, its list replaces the cached one, whatever its updated_at.
func TestPullModeARestartServesTheCacheForTheGraceFromTheLastGoodFetch(t *testing.T) {
	p := newPullRig(t, "1h", danaOwner)
	tok := p.g.token("dana@meridian.example")
	if err := os.WriteFile(p.puller.cache+".tmp", []byte(`{"list":`), 0o600); err != nil { // a write a crash cut short
		t.Fatal(err)
	}
	if err := p.puller.fetch(context.Background()); err != nil { // the last good fetch, at 12:00
		t.Fatal(err)
	}
	dir, _ := os.ReadDir(filepath.Dir(p.puller.cache))
	if info, err := os.Stat(p.puller.cache); err != nil || len(dir) != 1 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the cache: %v, %d file(s) beside it, mode %v", err, len(dir), info)
	}
	p.canyon.set(func(c *canyon) { c.down = true })
	for _, after := range []time.Duration{10 * time.Minute, 40 * time.Minute, 9 * time.Minute} { // 12:10, 12:50, 12:59
		p.now = p.now.Add(after)
		if err := p.restart(); err != nil {
			t.Fatalf("a restart at %s: %v", p.now.Format("15:04"), err)
		}
		_ = p.puller.fetch(context.Background()) // the first minute's failure moves nothing
		if rec := p.do("GET", "/", tok); rec.Code != http.StatusOK {
			t.Fatalf("after a restart at %s: %d %s", p.now.Format("15:04"), rec.Code, rec.Body)
		}
	}
	p.now = p.now.Add(time.Minute) // 13:00, the grace from 12:00
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "access_unreadable") {
		t.Fatalf("an hour after the last good fetch: %d %s", rec.Code, rec.Body)
	}
	if err := p.restart(); err == nil {
		t.Fatal("a restart past the grace started")
	}

	p.canyon.set(func(c *canyon) { c.down = false }) // Canyon back: the start fetches, and caches anew
	if err := p.restart(); err != nil {
		t.Fatal(err)
	}
	p.canyon.set(func(c *canyon) { c.down = true })
	p.now = p.now.Add(30 * time.Minute)
	if err := p.restart(); err != nil {
		t.Fatalf("half an hour after the next good fetch: %v", err)
	}
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusOK {
		t.Fatalf("half an hour after the next good fetch: %d", rec.Code)
	}
	p.canyon.set(func(c *canyon) { c.down = false })
	p.access(t, "2026-09-25T09:00:00Z") // Canyon back with a list older than the cached one, which names nobody
	if err := p.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := p.do("GET", "/", tok); rec.Code != http.StatusFound {
		t.Fatalf("after a good fetch, the cached list still serves: %d", rec.Code)
	}

	z := newPullRig(t, "0", danaOwner) // a grace of 0: never from the cache
	if err := z.puller.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	z.canyon.set(func(c *canyon) { c.down = true })
	if err := z.restart(); err == nil {
		t.Fatal("a grace of 0 started from the cache")
	}
}

// A cache that cannot be read or parsed, holds no list with an updated_at, or names no last good fetch or
// one in the future, is no cache: the sidecar never starts. A list's updated_at is Canyon's clock, never
// compared with the node's: one dated after the fetch that cached it is served.
func TestPullModeACorruptCacheIsNoCache(t *testing.T) {
	const list = `{"updated_at":"2026-09-25T10:00:00Z","members":{"domains":["meridian.example"]},"entries":[{"kind":"email","grantee":"dana@meridian.example","level":"owner"}]}`
	at := func(d time.Duration) string {
		return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC).Add(d).Format(time.RFC3339)
	}
	for name, c := range map[string]struct {
		cache string
		start bool
	}{
		"a good one":                   {`{"list":` + list + `,"last_success":"` + at(-time.Minute) + `"}`, true},
		"none":                         {"", false},
		"a directory":                  {"/", false},
		"not JSON":                     {`{"list":`, false},
		"no list":                      {`{"last_success":"` + at(-time.Minute) + `"}`, false},
		"a list with no updated_at":    {`{"list":{},"last_success":"` + at(-time.Minute) + `"}`, false},
		"no last_success":              {`{"list":` + list + `}`, false},
		"a last_success in the future": {`{"list":` + list + `,"last_success":"` + at(time.Minute) + `"}`, false},
		// Canyon's clock ahead of the node's: served all the same, until the first good fetch replaces it
		"a list updated after its last_success": {`{"list":` + strings.Replace(list, "2026-09-25T10:00:00Z", at(time.Hour), 1) +
			`,"last_success":"` + at(-time.Minute) + `"}`, true},
	} {
		p := newPullRig(t, "1h", danaOwner)
		p.canyon.set(func(c *canyon) { c.down = true })
		switch c.cache {
		case "":
		case "/":
			if err := os.Mkdir(p.puller.cache, 0o700); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(p.puller.cache, []byte(c.cache), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.restart(); (err == nil) != c.start {
			t.Errorf("%s: started %v (%v), want %v", name, err == nil, err, c.start)
		}
		if name == "a list updated after its last_success" { // served, until the first good fetch replaces it, older or not
			if rec := p.do("GET", "/", p.g.token("dana@meridian.example")); rec.Code != http.StatusOK {
				t.Errorf("%s: from the cache: %d", name, rec.Code)
			}
			p.canyon.set(func(c *canyon) { c.down = false })
			p.access(t, "2026-09-25T09:00:00Z") // names nobody
			if err := p.puller.fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rec := p.do("GET", "/", p.g.token("dana@meridian.example")); rec.Code != http.StatusFound {
				t.Errorf("%s: after a good fetch the cached list still serves: %d", name, rec.Code)
			}
		}
	}
}

// The access URL answering a redirect is a failed fetch: the token goes to the one address and no other.
func TestPullModeRefusesARedirect(t *testing.T) {
	p := newPullRig(t, "1h", danaOwner)
	var followed atomic.Int32
	to := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer to.Close()
	from := httptest.NewTLSServer(http.RedirectHandler(to.URL+"/access/claims-portal/preview", http.StatusFound))
	defer from.Close()
	p.puller.url = from.URL + "/access/claims-portal/preview"
	if err := p.puller.fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "302") || followed.Load() != 0 {
		t.Fatalf("a redirect: %v, followed %d time(s)", err, followed.Load())
	}
}

// Without the emptyDir the cache cannot be written: the sidecar serves all the same and logs it once, not
// on every fetch, and a start while Canyon is unreachable never starts.
func TestPullModeWithoutTheCacheDirServesAndLogsOnce(t *testing.T) {
	buf := logs(t)
	p := newPullRig(t, "1h", danaOwner)
	p.puller.cache = filepath.Join(t.TempDir(), "missing", "access.json")
	for range 2 {
		if err := p.puller.fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if rec := p.do("GET", "/", p.g.token("dana@meridian.example")); rec.Code != http.StatusOK {
		t.Fatalf("without the cache: %d %s", rec.Code, rec.Body)
	}
	if n := strings.Count(buf.String(), "the access cache"); n != 1 {
		t.Fatalf("%d line(s) about the cache across two fetches, want 1:\n%s", n, buf.String())
	}
	p.canyon.set(func(c *canyon) { c.down = true })
	if err := p.restart(); err == nil {
		t.Fatal("a restart with no cache and Canyon down started")
	}
}
