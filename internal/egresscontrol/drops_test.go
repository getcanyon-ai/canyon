package egresscontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// statsAnswer is /stats?format=json as Envoy v1.39.3 renders it (source/server/admin/stats_render.cc): a stats
// array of name and value, sorted, and a histograms element when any histogram matches, which has neither.
func statsAnswer(dropped, ejected, uptime uint64) string {
	return fmt.Sprintf(`{"stats":[{"name":%q,"value":%d},{"name":%q,"value":%d},{"name":%q,"value":%d},{"histograms":{"supported_quantiles":[0,50,100],"computed_quantiles":[]}}]}`,
		droppedStat, dropped, ejectedStat, ejected, uptimeStat, uptime)
}

// socketPath is a path for a socket: under the temporary directory itself, as a socket's path is at most 104 bytes
// on macOS, past which connect answers EINVAL for a file that is simply not there.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "envoy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "admin.sock")
}

// adminSocket serves handler on a Unix socket, as Envoy's admin interface does, and answers its path.
func adminSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

// egress-control reads Envoy's three stats over its Unix socket, by their exact names, in one answer.
func TestStatsAreReadOverTheAdminSocket(t *testing.T) {
	var asked *http.Request
	socket := adminSocket(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r
		_, _ = io.WriteString(w, statsAnswer(12, 3, 4000))
	})
	got, err := adminStats(socket)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (envoyStats{dropped: 12, ejected: 3, uptime: 4000}); got != want {
		t.Errorf("read %+v, want %+v", got, want)
	}
	q := asked.URL.Query()
	if asked.Method != http.MethodGet || asked.URL.Path != "/stats" || q.Get("format") != "json" {
		t.Errorf("asked %s %s; want GET /stats, format=json", asked.Method, asked.URL)
	}
	if want := `^(access_logs\.grpc_access_log\.logs_dropped|cluster\.receiver\.upstream_rq_pending_failure_eject|server\.uptime)$`; q.Get("filter") != want {
		t.Errorf("the filter is %q, want %q: only these stats, by their exact names", q.Get("filter"), want)
	}
}

// A read that says nothing is an error, never a zero: an answer that is no 200, that does not parse, that leaves a
// stat out, that gives one no number, or a socket no one holds. Envoy renaming a stat lands here.
func TestAReadThatSaysNothingIsAnError(t *testing.T) {
	missing := fmt.Sprintf(`{"stats":[{"name":%q,"value":0},{"name":%q,"value":1}]}`, ejectedStat, uptimeStat)
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"a 503":                 {http.StatusServiceUnavailable, statsAnswer(0, 0, 1)},
		"no JSON":               {http.StatusOK, "access_logs.grpc_access_log.logs_dropped: 0\n"},
		"cut short":             {http.StatusOK, statsAnswer(0, 0, 1)[:40]},
		"no stats":              {http.StatusOK, `{"stats":[]}`},
		"an empty object":       {http.StatusOK, `{}`},
		"the counter left out":  {http.StatusOK, missing},
		"a value of null":       {http.StatusOK, strings.Replace(statsAnswer(0, 0, 1), `"value":0`, `"value":null`, 1)},
		"a value that is text":  {http.StatusOK, strings.Replace(statsAnswer(0, 0, 1), `"value":0`, `"value":"0"`, 1)},
		"a value below zero":    {http.StatusOK, strings.Replace(statsAnswer(0, 0, 1), `"value":0`, `"value":-1`, 1)},
		"a value that is float": {http.StatusOK, strings.Replace(statsAnswer(0, 0, 1), `"value":0`, `"value":0.5`, 1)},
	} {
		socket := adminSocket(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		if got, err := adminStats(socket)(context.Background()); err == nil {
			t.Errorf("%s read as %+v, not as an error", name, got)
		}
	}
	if got, err := adminStats(socketPath(t))(context.Background()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no socket file read as %+v, %v: want an error that says so, which Read tells from a read that failed", got, err)
	}
}

// A socket file no one listens on is no Envoy that has yet to start: it is one that went, as a file only Envoy
// writes says. Only no file at all is no Envoy yet.
func TestASocketNoOneHoldsIsAFailedRead(t *testing.T) {
	path := socketPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = adminStats(path)(context.Background())
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a socket file no one holds read as %v; want a failed read, not no Envoy yet", err)
	}
}

// read is what one read of Envoy's stats answers: a failed one is failed, and absent finds no socket file.
type read struct {
	envoyStats
	failed, absent bool
}

// sent is one Read's reports: Envoy's count, 0 for none, and whether it said no one counted some.
type sent struct {
	calls     uint64
	uncounted bool
}

// dropsOf is a Drops on reads, one a Read, recording each Read's reports in order.
func dropsOf(t *testing.T, reads []read, refuse func(int) bool) (*Drops, *[]sent) {
	t.Helper()
	var got []sent
	next, tries := 0, 0
	d := &Drops{log: log.New(io.Discard, "", 0), now: time.Now, started: time.Now(), every: time.Hour, wait: time.Hour}
	d.stats = func(context.Context) (envoyStats, error) {
		r := reads[next]
		next++
		got = append(got, sent{})
		switch {
		case r.failed:
			return envoyStats{}, errors.New("no answer")
		case r.absent:
			return envoyStats{}, fmt.Errorf("dial: %w", fs.ErrNotExist)
		}
		return r.envoyStats, nil
	}
	d.report = func(_ context.Context, calls *uint64) error {
		tries++
		if refuse != nil && refuse(tries) {
			return errors.New("503")
		}
		last := &got[len(got)-1]
		if calls == nil {
			last.uncounted = true
		} else {
			last.calls += *calls
		}
		return nil
	}
	return d, &got
}

// Each read adds what it says to what the server has not taken: Envoy's count where its counter rose, and that no
// one counted where a restart, a stream that failed to open, a first read's counters or a failed read leave entries
// no counter has. A decrease is never negative, and a failed read is never none.
func TestWhatEachReadReports(t *testing.T) {
	const up = 1000
	s := func(dropped, ejected, uptime uint64) read {
		return read{envoyStats: envoyStats{dropped: dropped, ejected: ejected, uptime: uptime}}
	}
	failed, absent := read{failed: true}, read{absent: true}
	nothing := sent{}
	for name, c := range map[string]struct {
		reads []read
		want  []sent
	}{
		"a counter that stays says nothing": {
			[]read{s(0, 0, up), s(0, 0, up+10), s(0, 0, up+20)},
			[]sent{nothing, nothing, nothing},
		},
		"a rise is Envoy's count, once": {
			[]read{s(0, 0, up), s(12, 0, up+10), s(12, 0, up+20), s(15, 0, up+30)},
			[]sent{nothing, {calls: 12}, nothing, {calls: 3}},
		},
		"a decrease is a restart, never a negative gap": {
			[]read{s(50, 0, up), s(3, 0, 5)},
			[]sent{{uncounted: true}, {calls: 3, uncounted: true}},
		},
		"a counter back at zero counts none, and no one counted what the restart lost": {
			[]read{s(0, 0, up), s(7, 0, up+10), s(0, 0, 4)},
			[]sent{nothing, {calls: 7}, {uncounted: true}},
		},
		"a restart with no counter moved is seen by the uptime alone": {
			[]read{s(0, 0, up), s(0, 0, 3)},
			[]sent{nothing, {uncounted: true}},
		},
		"a restart past the old count sends all the new Envoy counted": {
			[]read{s(0, 0, up), s(5, 0, up+10), s(9, 0, 6)},
			[]sent{nothing, {calls: 5}, {calls: 9, uncounted: true}},
		},
		"a stream that failed to open is what no one counted": {
			[]read{s(0, 0, up), s(0, 2, up+10), s(0, 2, up+20)},
			[]sent{nothing, {uncounted: true}, nothing},
		},
		"a failed read is what no one counted, and the rise across it still counts": {
			[]read{s(10, 0, up), failed, failed, s(14, 0, up+30), s(14, 0, up+40)},
			[]sent{{uncounted: true}, {uncounted: true}, {uncounted: true}, {calls: 4}, nothing},
		},
		"a restart across failed reads is still seen": {
			[]read{s(0, 0, up), failed, s(0, 0, 8)},
			[]sent{nothing, {uncounted: true}, {uncounted: true}},
		},
		"a first read that fails is what no one counted": {
			[]read{failed, s(0, 0, 20)},
			[]sent{{uncounted: true}, nothing},
		},
		"no socket file before the first answer is no Envoy yet, which lost nothing": {
			[]read{absent, absent, s(0, 0, 5), s(0, 0, 15)},
			[]sent{nothing, nothing, nothing, nothing},
		},
		"no socket file once Envoy has answered is a failed read": {
			[]read{s(0, 0, up), absent, absent},
			[]sent{nothing, {uncounted: true}, {uncounted: true}},
		},
		"a first read above zero after no socket file is still what no one counted": {
			[]read{absent, s(3, 0, 5)},
			[]sent{nothing, {uncounted: true}},
		},
		"a first read at zero says nothing": {
			[]read{s(0, 0, 3)},
			[]sent{nothing},
		},
		"a first read above zero may be reported already, so no one counted it": {
			[]read{s(40, 0, up), s(41, 0, up+10)},
			[]sent{{uncounted: true}, {calls: 1}},
		},
		"a first read of a stream that failed to open too": {
			[]read{s(0, 1, up)},
			[]sent{{uncounted: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, got := dropsOf(t, c.reads, nil)
			for range c.reads {
				d.Read(context.Background())
			}
			if fmt.Sprint(*got) != fmt.Sprint(c.want) {
				t.Errorf("reports %+v, want %+v", *got, c.want)
			}
		})
	}
}

// What the server does not take stays with egress-control, summed, and is sent at the next read, once; the count
// and what no one counted are delivered apart, so a refusal of one keeps only that one.
func TestWhatTheServerRefusesIsKeptAndSentOnce(t *testing.T) {
	s := func(dropped, ejected, uptime uint64) read {
		return read{envoyStats: envoyStats{dropped: dropped, ejected: ejected, uptime: uptime}}
	}
	refused := true
	d, got := dropsOf(t, []read{s(0, 0, 100), s(5, 0, 110), s(8, 1, 120), s(8, 1, 130), s(8, 1, 140)},
		func(int) bool { return refused })
	for range 3 {
		d.Read(context.Background())
	}
	refused = false
	d.Read(context.Background())
	d.Read(context.Background())
	want := []sent{{}, {}, {}, {calls: 8, uncounted: true}, {}}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Errorf("reports %+v, want %+v", *got, want)
	}

	// the server takes the count and refuses what no one counted, whose second try is what is left
	d, got = dropsOf(t, []read{s(0, 0, 100), s(5, 1, 110), s(5, 1, 120)}, func(try int) bool { return try == 2 })
	for range 3 {
		d.Read(context.Background())
	}
	want = []sent{{}, {calls: 5}, {uncounted: true}}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Errorf("a split delivery reports %+v, want %+v", *got, want)
	}
}

// A read or a report that fails is logged, once a minute however often it fails, and the log names no value but
// the error's.
func TestAFailureLogsOnceAMinute(t *testing.T) {
	var logged bytes.Buffer
	d, _ := dropsOf(t, []read{{failed: true}, {failed: true}, {failed: true}}, nil)
	d.log = log.New(&logged, "", 0)
	at := time.Unix(1000, 0)
	d.now = func() time.Time { return at }
	d.Read(context.Background())
	at = at.Add(dropsEvery)
	d.Read(context.Background())
	if n := strings.Count(logged.String(), "what Envoy lost is not all reported"); n != 1 {
		t.Fatalf("two failures inside a minute logged %d lines:\n%s", n, logged.String())
	}
	at = at.Add(alarmEvery)
	d.Read(context.Background())
	if n := strings.Count(logged.String(), "what Envoy lost is not all reported"); n != 2 {
		t.Errorf("a failure a minute on logged %d lines in all:\n%s", n, logged.String())
	}
}

// A read that hangs spends its own time and no more: the report has a context of its own, live however long the read
// took, and the two fit Docker's 10 s stop. Envoy's main thread stuck while its workers proxy on accepts on the socket
// and never answers, so a report that waits on the read is a loss no one hears of, the last read's at a stop too.
func TestAHungReadLeavesItsReportItsOwnTime(t *testing.T) {
	if readWait+reportWait >= 10*time.Second {
		t.Errorf("a read's %v and its reports' %v do not fit Docker's 10 s stop", readWait, reportWait)
	}
	var live []bool
	d := &Drops{log: log.New(io.Discard, "", 0), now: time.Now, every: time.Hour, wait: 5 * time.Millisecond}
	d.stats = func(ctx context.Context) (envoyStats, error) {
		<-ctx.Done()
		return envoyStats{}, ctx.Err()
	}
	d.report = func(ctx context.Context, calls *uint64) error {
		live = append(live, ctx.Err() == nil && calls == nil)
		return ctx.Err()
	}
	d.Read(context.Background())
	if len(live) != 1 || !live[0] {
		t.Errorf("a read that hung reported %v; want one report of what no one counted, on a live context", live)
	}
}

// The same with the real reader and client: a socket that accepts and never answers, as a stuck main thread leaves
// it, reaches the server as what no one counted.
func TestASocketThatNeverAnswersReachesTheServer(t *testing.T) {
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // held open, never answered
		}
	}()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, string(raw))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDrops(path, c.Report, log.New(io.Discard, "", 0))
	d.wait = 20 * time.Millisecond
	d.Read(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != `{"calls":null}` {
		t.Errorf("the server took %v; want one report of what no one counted", bodies)
	}
}

// A read this egress-control's own stop cut short says nothing of Envoy, so a healthy Envoy is no gap: the last read,
// on a context of its own, decides.
func TestAReadOurStopCutShortSaysNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads, reports := 0, 0
	d := &Drops{log: log.New(io.Discard, "", 0), now: time.Now, every: time.Hour, wait: time.Hour}
	d.stats = func(c context.Context) (envoyStats, error) {
		reads++
		if reads == 1 {
			cancel()
			<-c.Done()
			return envoyStats{}, c.Err()
		}
		return envoyStats{uptime: 100}, nil
	}
	d.report = func(context.Context, *uint64) error { reports++; return nil }
	d.Read(ctx)
	if d.uncounted {
		t.Error("a read our own stop cut short reads as what no one counted")
	}
	d.Read(context.WithoutCancel(ctx))
	if reads != 2 || reports != 0 {
		t.Errorf("%d reads, %d reports; want the last read to decide, and a healthy Envoy to report nothing", reads, reports)
	}
}

// No socket file in the first minute is an Envoy a little slow to start: no gap, and nothing logged. Past that minute
// it is a failed read at every read, logged once a minute, so a socket that never comes reaches the records.
func TestNoSocketIsAFailedReadPastEnvoysMinute(t *testing.T) {
	var logged bytes.Buffer
	var reports []*uint64
	d := NewDrops(socketPath(t), func(_ context.Context, calls *uint64) error {
		reports = append(reports, calls)
		return nil
	}, log.New(&logged, "", 0))
	at := time.Now()
	d.now, d.started, d.said = func() time.Time { return at }, at, at
	d.Read(context.Background())
	if len(reports) != 0 || logged.Len() != 0 {
		t.Fatalf("an Envoy slow to start reported %v and logged:\n%s", reports, logged.String())
	}
	for range 2 { // never a socket, as a new egress-control on an old manifest: every read says so, for as long as it lasts
		at = at.Add(alarmEvery)
		reports = nil
		d.Read(context.Background())
		if len(reports) != 1 || reports[0] != nil {
			t.Fatalf("no socket a minute on reported %v, want one report of what no one counted", reports)
		}
	}
	if n := strings.Count(logged.String(), "what Envoy lost is not all reported"); n != 2 {
		t.Errorf("no socket two minutes on logged %d lines:\n%s", n, logged.String())
	}
}

// Run reads every interval and, as it stops, once more on a context of its own: a counter that goes with its pod
// has no one else to read it, and a read that finds Envoy gone says so.
func TestRunReadsOnceMoreAsItStops(t *testing.T) {
	reads := make(chan error, 8)
	d := &Drops{log: log.New(io.Discard, "", 0), now: time.Now, every: time.Hour, wait: time.Hour}
	d.stats = func(ctx context.Context) (envoyStats, error) {
		reads <- ctx.Err()
		return envoyStats{}, errors.New("Envoy is gone")
	}
	reported := make(chan bool, 8)
	d.report = func(ctx context.Context, calls *uint64) error {
		reported <- ctx.Err() == nil && calls == nil
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	cancel()
	<-done
	if len(reads) != 1 || <-reads != nil {
		t.Fatal("Run stopped without a read of its own, on a context still live")
	}
	if len(reported) != 1 || !<-reported {
		t.Error("the last read's failure reached the server as anything but what no one counted")
	}
}

// Report posts to the policy's listener with the policy's own proof: {calls} for Envoy's count, {calls: null} for
// what no one counted, and any answer but 204 leaves it not taken.
func TestReportPostsToThePolicysListener(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := http.StatusNoContent
	var bodies []string
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/egress/dropped" {
			t.Errorf("asked %s %s; want POST /egress/dropped", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		bodies, auth = append(bodies, string(raw)), append(auth, r.Header.Get("Authorization"))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/egress/policy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := uint64(412)
	if err := c.Report(context.Background(), &calls); err != nil {
		t.Fatal(err)
	}
	if err := c.Report(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	c.TokenFile = token
	status = http.StatusServiceUnavailable
	if err := c.Report(context.Background(), nil); err == nil {
		t.Error("a 503 read as taken")
	}
	want := []string{`{"calls":412}`, `{"calls":null}`, `{"calls":null}`}
	if strings.Join(bodies, " ") != strings.Join(want, " ") {
		t.Errorf("sent %v, want %v", bodies, want)
	}
	if auth[0] != "" || auth[2] != "Bearer the-token" {
		t.Errorf("Authorization %q on Docker, %q on Kubernetes; want none, then the token", auth[0], auth[2])
	}
	var body struct {
		Calls *uint64 `json:"calls"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil || *body.Calls != 412 {
		t.Errorf("the body %s does not read back: %v", bodies[0], err)
	}
}
