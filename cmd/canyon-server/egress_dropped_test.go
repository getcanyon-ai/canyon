package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// envoyStub is Envoy's admin interface on a Unix socket, answering /stats as Envoy v1.39.3 renders it. down answers
// 503, as a stats read that fails.
type envoyStub struct {
	socket string

	mu                   sync.Mutex
	dropped, ejected, up uint64
	down                 bool
}

func newEnvoyStub(t *testing.T) *envoyStub {
	t.Helper()
	dir, err := os.MkdirTemp("", "envoy") // a socket's path is at most 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := &envoyStub{socket: filepath.Join(dir, "admin.sock"), up: 100}
	ln, err := net.Listen("unix", e.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return e
}

func (e *envoyStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintf(w, `{"stats":[{"name":"access_logs.grpc_access_log.logs_dropped","value":%d},{"name":"cluster.receiver.upstream_rq_pending_failure_eject","value":%d},{"name":"server.uptime","value":%d}]}`,
		e.dropped, e.ejected, e.up)
}

func (e *envoyStub) set(dropped, ejected, up uint64, down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dropped, e.ejected, e.up, e.down = dropped, ejected, up, down
}

// TestEnvoysDroppedCountIsAGap is #2374's acceptance, whole but for Envoy itself: Envoy's counter rises on its admin
// socket, egress-control reads it and reports it, and the server's next flush writes an envoy_dropped gap with the
// count. A counter that goes backwards is a restart: no negative gap, what the new Envoy counted, and a row of its
// own for what no one counted; a stats read that fails is that row again, never none, and the rise across it counts.
func TestEnvoysDroppedCountIsAGap(t *testing.T) {
	f := newEgressFixture(t)
	envoy := newEnvoyStub(t)
	srv := httptest.NewServer(&policyServer{replica: peerReplica, lost: f.r.envoy.add, log: log.New(f.logged, "", 0)})
	defer srv.Close()
	client, err := egresscontrol.NewClient(srv.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	drops := egresscontrol.NewDrops(envoy.socket, client.Report, log.New(f.logged, "", 0))
	step := func(dropped, ejected, up uint64, down bool, want ...string) {
		t.Helper()
		envoy.set(dropped, ejected, up, down)
		drops.Read(f.ctx)
		if err := f.r.flush(f.ctx, nil); err != nil {
			t.Fatal(err)
		}
		if got := f.gapsOf(t); !slices.Equal(got, want) {
			t.Fatalf("the gaps are %v, want %v\n%s", got, want, f.logged.String())
		}
	}
	step(0, 0, 100, false)                                                          // the first read: nothing lost
	step(0, 0, 110, false)                                                          // nothing rose
	step(412, 0, 120, false, "envoy_dropped:412")                                   // the acceptance: the count, as Envoy said it
	step(415, 0, 130, false, "envoy_dropped:415")                                   // a rise continues the row
	step(2, 0, 3, false, "envoy_dropped:417", "envoy_dropped:-")                    // a restart: what the new Envoy counted, and what no one did
	step(2, 0, 13, false, "envoy_dropped:417", "envoy_dropped:-")                   // nothing rose
	step(2, 0, 23, true, "envoy_dropped:417", "envoy_dropped:-", "envoy_dropped:-") // a read that failed: a row of no one counted, never none
	step(9, 0, 33, false, "envoy_dropped:7", "envoy_dropped:417", "envoy_dropped:-", "envoy_dropped:-")
	step(9, 1, 43, false, "envoy_dropped:7", "envoy_dropped:417", "envoy_dropped:-", "envoy_dropped:-", "envoy_dropped:-") // a stream that failed to open
}

// A report the last flush would not see is refused, so egress-control keeps it for the server after this one, and
// everything taken before that is in the last flush.
func TestAReportPastTheLastFlushIsRefused(t *testing.T) {
	var e envoyLost
	calls := int64(5)
	if !e.add(&calls) || !e.add(nil) {
		t.Fatal("a report before the last flush was refused")
	}
	e.close()
	if e.add(&calls) || e.add(nil) {
		t.Error("a report past the last flush was taken, and would never be written")
	}
	if n, uncounted := e.take(); n != 5 || !uncounted {
		t.Errorf("the last flush takes %d, uncounted %t; want the 5 and the uncounted taken before it", n, uncounted)
	}
	if n, uncounted := e.take(); n != 0 || uncounted {
		t.Errorf("a second take is %d, uncounted %t: a report is written once", n, uncounted)
	}
}

// POST /egress/dropped takes exactly {calls} from egress-control alone (contracts/routes.md, Egress control): who the
// listener does not name is 401 before the body is read; anything but a count from 1 to 2^40 or null is 400, zero
// and a decrease among them; no receiver is 503, which egress-control keeps; and every other method and path stay 404.
func TestTheDroppedReportsBoundaries(t *testing.T) {
	var e envoyLost
	admitted := true
	s := &policyServer{replica: func(*http.Request) (string, bool) { return "replica", admitted }, lost: e.add, log: log.New(io.Discard, "", 0)}
	post := func(method, path, body string) (int, string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, out := post(http.MethodPost, "/egress/dropped", `{"calls":412}`); code != http.StatusNoContent || out != "" {
		t.Errorf("a count answered %d %s, not 204", code, out)
	}
	if code, _ := post(http.MethodPost, "/egress/dropped", `{"calls":null}`); code != http.StatusNoContent {
		t.Errorf("no one counted answered %d, not 204", code)
	}
	if code, _ := post(http.MethodPost, "/egress/dropped", fmt.Sprintf(`{"calls":%d}`, int64(droppedMost))); code != http.StatusNoContent {
		t.Errorf("the most a report counts answered %d, not 204", code)
	}
	if n, uncounted := e.take(); n != 412+droppedMost || !uncounted {
		t.Fatalf("the reports summed to %d, uncounted %t", n, uncounted)
	}

	for _, body := range []string{``, `{}`, `null`, `[]`, `412`, `{"calls":0}`, `{"calls":-3}`, `{"calls":1.5}`, `{"calls":1e3}`, `{"calls":"412"}`,
		`{"calls":true}`, fmt.Sprintf(`{"calls":%d}`, int64(droppedMost)+1), `{"calls":9223372036854775808}`, `{"calls":1,"app":"claims-portal"}`,
		`{"calls":1}{"calls":1}`, `{"calls":` + strings.Repeat("1", 2000) + `}`,
		`{"calls":1}}`, `{"calls":1}]`, `{"calls":1},`, `{"CALLS":5}`, `{"Calls":5}`, `{"calls":1,"calls":null}`, `{"calls":null,"calls":1}`, `{"calls":1,"calls":1}`,
		`{"calls":1}` + strings.Repeat(" ", 2000)} {
		if code, out := post(http.MethodPost, "/egress/dropped", body); code != http.StatusBadRequest || !strings.Contains(out, `"invalid_report"`) {
			t.Errorf("the body %.40q answered %d %s, not 400 invalid_report", body, code, out)
		}
	}
	if n, uncounted := e.take(); n != 0 || uncounted {
		t.Errorf("a refused body counted %d, uncounted %t", n, uncounted)
	}

	admitted = false
	if code, out := post(http.MethodPost, "/egress/dropped", `{"calls":412}`); code != http.StatusUnauthorized || out != "" {
		t.Errorf("a caller the listener does not name answered %d %q, not 401, no body", code, out)
	}
	if n, _ := e.take(); n != 0 {
		t.Errorf("a refused caller's report counted %d", n)
	}
	admitted = true

	e.close()
	if code, out := post(http.MethodPost, "/egress/dropped", `{"calls":412}`); code != http.StatusServiceUnavailable || !strings.Contains(out, `"no_receiver"`) {
		t.Errorf("a report past the last flush answered %d %s, not 503 no_receiver", code, out)
	}
	s.lost = nil
	if code, out := post(http.MethodPost, "/egress/dropped", `{"calls":null}`); code != http.StatusServiceUnavailable || !strings.Contains(out, `"no_receiver"`) {
		t.Errorf("a server with no receiver answered %d %s, not 503 no_receiver", code, out)
	}

	for _, c := range [][2]string{{http.MethodGet, "/egress/dropped"}, {http.MethodPut, "/egress/dropped"}, {http.MethodPost, "/egress/policy"}, {http.MethodPost, "/egress/other"}} {
		if code, _ := post(c[0], c[1], `{"calls":1}`); code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, not 404", c[0], c[1], code)
		}
	}
}

// The bound in code is the contract's, so a report the schema takes is never refused, nor one it refuses taken.
func TestDroppedMostIsTheSchemas(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/schema/egress.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Definitions struct {
			Report struct {
				Properties struct {
					Calls struct {
						OneOf []struct {
							Maximum json.Number `json:"maximum"`
						} `json:"oneOf"`
					} `json:"calls"`
				} `json:"properties"`
			} `json:"dropped_report"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Definitions.Report.Properties.Calls.OneOf[0].Maximum.String(); got != strconv.FormatInt(droppedMost, 10) {
		t.Errorf("the schema's most is %s, the code's %d", got, int64(droppedMost))
	}
}
