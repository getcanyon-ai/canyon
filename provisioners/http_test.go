package provisioners

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "t0ken-for-tests"

// reference is the reference adapter of contracts/provisioner.md, in memory: a key's first PUT
// starts the work (202), the next answers ready (200); DELETE is 204, also for a key never seen.
type reference struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (ref *reference) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ref.mu.Lock()
	defer ref.mu.Unlock()
	var body struct{ Type string }
	switch {
	case r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/canyon/claims/preview/db":
		answerWith(w, http.StatusForbidden, `{"message":"wrong token or path"}`)
	case r.Method == http.MethodDelete:
		delete(ref.seen, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != "postgres":
		answerWith(w, http.StatusUnprocessableEntity, `{"message":"only postgres here"}`)
	case !ref.seen[r.URL.Path]:
		ref.seen[r.URL.Path] = true
		answerWith(w, http.StatusAccepted, `{"message":"creating the database"}`)
	default:
		answerWith(w, http.StatusOK, `{"outputs":{"DATABASE_URL":"postgres://claims@db.internal/claims"}}`)
	}
}

func answerWith(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// canned always answers status and body.
func canned(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { answerWith(w, status, body) }
}

// adapter is an HTTP driver talking to h at {server}/canyon.
func adapter(t *testing.T, h http.Handler) *HTTP {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	d, err := NewHTTP(s.URL+"/canyon", tokenFile(t), "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func tokenFile(t *testing.T) string { return writeFile(t, "token", token+"\n") }

var claims, ctx = Key{"claims", "preview", "db"}, context.Background()

func TestTheReferenceAdapterProvisionsThenDeletes(t *testing.T) {
	d := adapter(t, &reference{seen: map[string]bool{}})
	for i, want := range []State{Pending, Ready} {
		if r, err := d.Ensure(ctx, claims, "postgres"); err != nil || r.State != want {
			t.Fatalf("ensure %d: %+v %v, want %s", i, r, err, want)
		}
	}
	for range 2 { // gone twice: deleting what is gone is success
		if r, err := d.Delete(ctx, claims); err != nil || r.State != Gone {
			t.Fatalf("delete: %+v %v", r, err)
		}
	}
}

func TestEachAnswerIsItsState(t *testing.T) {
	db := map[string]string{"DATABASE_URL": "postgres://x"}
	message, _ := json.Marshal(map[string]string{"message": "\x1b[31mquota\r\n" + strings.Repeat("é", 2000)})
	for _, c := range []struct {
		method string
		status int
		body   string
		want   State // "" is a transient error
		msg    string
		out    map[string]string
	}{
		{"PUT", 200, `{"outputs":{"DATABASE_URL":"postgres://x"}}`, Ready, "", db},
		// Only the promised outputs reach the app: NODE_OPTIONS or LD_PRELOAD would run code in it.
		{"PUT", 200, `{"outputs":{"DATABASE_URL":"postgres://x","NODE_OPTIONS":"--require /tmp/x.js","LD_PRELOAD":"/tmp/x.so"}}`, Ready, "", db},
		{"PUT", 200, `{"outputs":{"DATABASE_URL":""}}`, Failed, "adapter answered ready with DATABASE_URL empty, missing or over 64 KiB", nil},
		{"PUT", 200, `{"outputs":{"OTHER":"x"}}`, Failed, "adapter answered ready with DATABASE_URL empty, missing or over 64 KiB", nil},
		{"PUT", 200, `{"outputs":{"DATABASE_URL":"` + strings.Repeat("x", 64<<10+1) + `"}}`, Failed, "adapter answered ready with DATABASE_URL empty, missing or over 64 KiB", nil},
		{"PUT", 200, `{"message":"` + strings.Repeat("x", 1<<20) + `"}`, Failed, "adapter's answer is over 1 MiB", nil},
		{"PUT", 200, `not json`, Failed, "adapter's 200 answer is not JSON", nil},
		{"PUT", 202, `{"message":"creating"}`, Pending, "creating", nil}, {"PUT", 202, ``, Pending, "", nil},
		{"PUT", 404, `{"message":"no such plan"}`, Failed, "no such plan", nil},
		{"PUT", 422, string(message), Failed, "[31mquota" + strings.Repeat("é", 1024-len("[31mquota")), nil},
		{"PUT", 403, `<html>forbidden</html>`, Failed, "adapter answered 403", nil}, {"PUT", 204, ``, Failed, "adapter answered 204", nil},
		{"PUT", 500, `{"outputs":{"DATABASE_URL":"postgres://x"}}`, "", "", nil}, {"PUT", 503, ``, "", "", nil},
		{"PUT", 429, `{"message":"slow down"}`, "", "", nil}, {"DELETE", 408, ``, "", "", nil}, // busy: ask again
		{"DELETE", 204, ``, Gone, "", nil},
		{"DELETE", 202, `{"message":"destroying"}`, Pending, "destroying", nil},
		{"DELETE", 409, `{"message":"has deletion protection"}`, Failed, "has deletion protection", nil},
		{"DELETE", 200, ``, Failed, "adapter answered 200", nil}, {"DELETE", 502, ``, "", "", nil},
	} {
		d := adapter(t, canned(c.status, c.body))
		r, err := d.Delete(ctx, claims)
		if c.method == "PUT" {
			r, err = d.Ensure(ctx, claims, "postgres")
		}
		if c.want == "" && err == nil || c.want != "" && (err != nil || r.State != c.want || r.Message != c.msg || !maps.Equal(r.Outputs, c.out)) {
			t.Errorf("%s %d %.60s: %.100v %v, want %q %.40q", c.method, c.status, c.body, r, err, c.want, c.msg)
		}
	}
}

// A redirect is failed and never followed: following it would send the token wherever it points.
func TestARedirectIsFailedAndTheTokenStaysHome(t *testing.T) {
	var leaked bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
		answerWith(w, http.StatusOK, `{"outputs":{"DATABASE_URL":"postgres://elsewhere"}}`)
	}))
	defer elsewhere.Close()
	d := adapter(t, http.RedirectHandler(elsewhere.URL, http.StatusTemporaryRedirect))
	if r, err := d.Ensure(ctx, claims, "postgres"); err != nil || r.State != Failed || leaked {
		t.Fatalf("%+v %v, token sent elsewhere: %v", r, err, leaked)
	}
}

func TestATimeoutOrAClosedConnectionIsTransient(t *testing.T) {
	slow := adapter(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(200 * time.Millisecond) }))
	if slow.client.Timeout != 30*time.Second {
		t.Fatalf("a call has %v, the contract says 30s", slow.client.Timeout)
	}
	slow.client.Timeout = 50 * time.Millisecond
	s := httptest.NewServer(canned(200, ``))
	s.Close()
	closed, err := NewHTTP(s.URL, tokenFile(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*HTTP{slow, closed} {
		if r, err := d.Ensure(ctx, claims, "postgres"); err == nil {
			t.Errorf("answered %+v, want a transient error", r)
		}
	}
}

func TestAKeyIsCheckedBeforeAnyRequest(t *testing.T) {
	var asked bool
	d := adapter(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked = true }))
	for _, k := range []Key{{"..", "preview", "db"}, {"claims", "production", "db"}, {"claims", "preview", "a/b"}, {"Claims", "preview", "db"}, {"claims", "preview", ""}} {
		if r, err := d.Delete(ctx, k); err != nil || r.State != Failed {
			t.Errorf("%v: %+v %v", k, r, err)
		}
	}
	if asked {
		t.Fatal("an invalid key reached the adapter")
	}
}

func TestTheAdapterURLIsHTTPSOrLoopbackAndTheTokenFileHoldsAToken(t *testing.T) {
	good := tokenFile(t)
	for _, u := range []string{"https://provision.internal/canyon", "http://127.0.0.1:9/canyon", "http://[::1]:9", "http://localhost:9"} {
		if _, err := NewHTTP(u, good, ""); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://provision.internal/canyon", "https://user:pass@provision.internal", "ftp://provision.internal"} {
		if _, err := NewHTTP(u, good, ""); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	for _, tf := range []string{writeFile(t, "empty", "\n"), writeFile(t, "two-lines", "abc\ndef\n"), filepath.Join(t.TempDir(), "missing")} {
		if _, err := NewHTTP("https://provision.internal", tf, ""); err == nil {
			t.Errorf("token file %s accepted", filepath.Base(tf))
		}
	}
}

func TestTheAdapterCertificateIsVerifiedAgainstItsCAFile(t *testing.T) {
	s := httptest.NewTLSServer(canned(204, ``))
	defer s.Close()
	ca := writeFile(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})))
	for ca, trusted := range map[string]bool{ca: true, "": false} { // "": the system roots, which do not know it
		d, err := NewHTTP(s.URL, tokenFile(t), ca)
		if err != nil {
			t.Fatal(err)
		}
		if r, err := d.Delete(ctx, claims); (err == nil && r.State == Gone) != trusted {
			t.Errorf("ca %q: %+v %v", ca, r, err)
		}
	}
	if _, err := NewHTTP(s.URL, tokenFile(t), tokenFile(t)); err == nil {
		t.Fatal("a CA file with no certificate in it was accepted")
	}
}
