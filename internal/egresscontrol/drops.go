package egresscontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// What Envoy lost on its way to the server's receiver (design/EGRESS.md §2.4; contracts/routes.md, POST
// /egress/dropped). The server never sees an entry Envoy lost, so egress-control reads Envoy's stats and reports
// what they say: Envoy's own count where it keeps one, and where it keeps none that entries were lost and no one
// counted them. A read that failed says nothing of what Envoy lost, so it is reported as no one counted, never
// as none. Records are evidence, not a gate: nothing here stops a call.

const (
	// AdminSocket is Envoy's admin interface, on a volume only Envoy and egress-control mount (deploy/egress/envoy.yaml).
	AdminSocket = "/var/run/envoy-admin/admin.sock"

	dropsEvery = 10 * time.Second // the receiver's flush
	readWait   = 2 * time.Second  // one read: Envoy's stats answer at once, or its main thread is stuck while its workers proxy on
	reportWait = 5 * time.Second  // a read's reports, on a context of their own, so a read that hung leaves them all of it; the two fit Docker's 10 s stop
	statsBytes = 64 << 10         // one answer, three stats
	// droppedStat counts the entries a worker dropped because its 16 KiB buffer was full and the stream took no
	// more; ejectedStat the streams to the receiver that failed to open, each losing the entries written to it,
	// which Envoy counts as flushed and never as dropped (#2377); uptimeStat is the seconds Envoy has run.
	droppedStat = "access_logs.grpc_access_log.logs_dropped"
	ejectedStat = "cluster.receiver.upstream_rq_pending_failure_eject"
	uptimeStat  = "server.uptime"
)

// statsQuery asks Envoy for the three stats alone, in JSON: their exact names, as a stat Envoy no longer has must
// read as missing, never as another that matches.
var statsQuery = url.Values{"format": {"json"}, "filter": {"^(" + regexp.QuoteMeta(droppedStat) + "|" + regexp.QuoteMeta(ejectedStat) + "|" + regexp.QuoteMeta(uptimeStat) + ")$"}}.Encode()

// envoyStats is one read of Envoy's stats.
type envoyStats struct{ dropped, ejected, uptime uint64 }

// restarted says whether Envoy started again since was: only a start takes a counter or the uptime backwards, but
// for a clock stepped back, which takes the uptime with it and reads as a start of the same Envoy. The one start it
// cannot see comes within a read of the start before it, which it did see.
func (s envoyStats) restarted(was envoyStats) bool {
	return s.uptime < was.uptime || s.dropped < was.dropped || s.ejected < was.ejected
}

// Drops reads Envoy's stats and reports what they say to the server. Run alone touches its fields.
type Drops struct {
	stats  func(context.Context) (envoyStats, error)
	report func(context.Context, *uint64) error // POST /egress/dropped: the count, nil for what no one counted
	log    *log.Logger
	now    func() time.Time
	every  time.Duration
	wait   time.Duration // a read's readWait

	seen      bool       // a read has answered: last is what Envoy said
	last      envoyStats // the last read that answered
	calls     uint64     // Envoy's count the server has not taken
	uncounted bool       // what no one counted, which the server has not taken
	started   time.Time  // when this began: Envoy has alarmEvery from here to put its socket in place
	said      time.Time  // when a failure was last logged; at first when it started, so Envoy has a minute to start in
}

// NewDrops reads Envoy's admin interface at socket and reports through report, the Client's.
func NewDrops(socket string, report func(context.Context, *uint64) error, logger *log.Logger) *Drops {
	start := time.Now()
	return &Drops{stats: adminStats(socket), report: report, log: logger, now: time.Now, every: dropsEvery, wait: readWait, started: start, said: start}
}

// Run reads every dropsEvery, the first read once it has passed, as Envoy beside it starts when this does, until
// ctx ends; then it reads once more on a context of its own, so what Envoy lost since the last read leaves with it:
// no one is left to read a counter that goes with its pod.
func (d *Drops) Run(ctx context.Context) {
	tick := time.NewTicker(d.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			d.Read(context.WithoutCancel(ctx))
			return
		case <-tick.C:
			d.Read(ctx)
		}
	}
}

// Read reads Envoy's stats once, adds what they say to what the server has not taken and sends that. An increase
// is Envoy's count. A decrease is no negative gap: Envoy started again, what it lost around that no one counted,
// and all the new one counted is its count. A clock stepped back reads the same way and sends the same Envoy's whole
// count again, which over-counts and never under-counts. The first read's counters are unknown to have been reported
// or not: above zero they are what no one counted, an earlier egress-control's count for all this one knows. A read
// that fails is what no one counted but for two: one this egress-control's own stop cut short says nothing of Envoy,
// the last read deciding, and no socket file in the first minute, before Envoy's first answer, is no Envoy yet, which
// lost nothing. Past that minute it is a failed read: an Envoy proxying with its socket elsewhere, as a new
// egress-control on an old manifest, must not read as nothing lost for as long as it runs.
func (d *Drops) Read(ctx context.Context) {
	ask, cancel := context.WithTimeout(ctx, d.wait)
	s, err := d.stats(ask)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil && !d.seen && errors.Is(err, fs.ErrNotExist) && d.now().Sub(d.started) < alarmEvery:
	case err != nil:
		d.uncounted = true
	case !d.seen:
		d.uncounted = d.uncounted || s.dropped > 0 || s.ejected > 0
	case s.restarted(d.last):
		d.calls += s.dropped
		d.uncounted = true
	default:
		d.calls += s.dropped - d.last.dropped
		d.uncounted = d.uncounted || s.ejected > d.last.ejected
	}
	if err == nil {
		d.seen, d.last = true, s
	}
	tell, done := context.WithTimeout(ctx, reportWait)
	defer done()
	if err := errors.Join(err, d.send(tell)); err != nil && d.now().Sub(d.said) >= alarmEvery {
		d.said = d.now()
		d.log.Printf("egress-control: what Envoy lost is not all reported, and is kept for the next read: %v", err)
	}
}

// send reports what the server has not taken, and keeps what it refuses for the next read.
func (d *Drops) send(ctx context.Context) error {
	var counted, uncounted error
	if d.calls > 0 {
		calls := d.calls
		if counted = d.report(ctx, &calls); counted == nil {
			d.calls = 0
		}
	}
	if d.uncounted {
		if uncounted = d.report(ctx, nil); uncounted == nil {
			d.uncounted = false
		}
	}
	return errors.Join(counted, uncounted)
}

// adminStats reads the three stats from Envoy's admin interface at socket: an answer that is no 200, that is not
// JSON, or that leaves one out is an error, never a zero. The dial is the socket's whatever the URL's host says.
func adminStats(socket string) func(context.Context) (envoyStats, error) {
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
	return func(ctx context.Context) (envoyStats, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://envoy/stats?"+statsQuery, nil)
		if err != nil {
			return envoyStats{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return envoyStats{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return envoyStats{}, fmt.Errorf("its stats answered %d", resp.StatusCode)
		}
		var body struct {
			Stats []struct {
				Name  string  `json:"name"`
				Value *uint64 `json:"value"`
			} `json:"stats"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, statsBytes)).Decode(&body); err != nil {
			return envoyStats{}, fmt.Errorf("its stats: %w", err)
		}
		got := map[string]uint64{}
		for _, st := range body.Stats {
			if st.Value != nil {
				got[st.Name] = *st.Value
			}
		}
		var missing []string
		var s envoyStats
		for _, want := range []struct {
			name string
			into *uint64
		}{{droppedStat, &s.dropped}, {ejectedStat, &s.ejected}, {uptimeStat, &s.uptime}} {
			v, ok := got[want.name]
			if !ok {
				missing = append(missing, want.name)
			}
			*want.into = v
		}
		if len(missing) > 0 {
			return envoyStats{}, fmt.Errorf("its stats name no %s", strings.Join(missing, ", "))
		}
		return s, nil
	}
}
