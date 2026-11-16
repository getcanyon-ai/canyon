package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// egressFixture is a migrated schema, a fake executor and a receiver logging into a buffer, served over
// real gRPC on loopback as Envoy reaches it.
type egressFixture struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	exec   *executortest.Fake
	r      *egressReceiver
	logged *syncBuffer
	client accesslogv3.AccessLogServiceClient
	call   context.Context // carries Envoy's token
}

// egressTestToken is the fixture's CANYON_EGRESS_RECEIVER_TOKEN.
var egressTestToken = strings.Repeat("e5", 32)

func newEgressFixture(t *testing.T) *egressFixture {
	t.Helper()
	pool, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &egressFixture{pool: pool, ctx: ctx, exec: executortest.New(t), logged: &syncBuffer{}}
	f.r = newEgressReceiver(pool, (&executor.Client{Base: f.exec.URL}).Callers, log.New(f.logged, "", 0))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newALSServer(f.r, egressTestToken)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.client = accesslogv3.NewAccessLogServiceClient(conn)
	f.call = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+egressTestToken)
	return f
}

// send streams the entries as Envoy does, then flushes what the receiver took as one batch.
func (f *egressFixture) send(t *testing.T, entries ...*accesslogdatav3.HTTPAccessLogEntry) {
	t.Helper()
	f.stream(t, entries...)
	var batch []call
	for len(f.r.calls) > 0 {
		batch = append(batch, <-f.r.calls)
	}
	if err := f.r.flush(f.ctx, batch); err != nil {
		t.Fatal(err)
	}
}

// stream streams the entries as Envoy does and returns once the receiver has taken every one.
func (f *egressFixture) stream(t *testing.T, entries ...*accesslogdatav3.HTTPAccessLogEntry) {
	t.Helper()
	stream, err := f.client.StreamAccessLogs(f.call)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range slices.Chunk(entries, 100) {
		if err := stream.Send(&accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{LogEntry: chunk}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil { // answered once the receiver has taken every entry
		t.Fatal(err)
	}
}

// entry is one access-log entry from source for authority, with the method and answer given, started now.
func entry(source string, method corev3.RequestMethod, scheme, authority string, status uint32) *accesslogdatav3.HTTPAccessLogEntry {
	return &accesslogdatav3.HTTPAccessLogEntry{
		CommonProperties: &accesslogdatav3.AccessLogCommon{
			DownstreamDirectRemoteAddress: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: source, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 40312}}}},
			StartTime:                     timestamppb.Now(),
			Duration:                      durationpb.New(250 * time.Millisecond),
			AccessLogType:                 accesslogdatav3.AccessLogType_DownstreamEnd,
		},
		Request:  &accesslogdatav3.HTTPRequestProperties{RequestMethod: method, Scheme: scheme, Authority: authority, RequestBodyBytes: 120},
		Response: &accesslogdatav3.HTTPResponseProperties{ResponseCode: wrapperspb.UInt32(status), ResponseBodyBytes: 900},
	}
}

func ranAs(id string) *string { return &id }

// rowsOf is every row of the table, each as Postgres renders it in JSON: what a reader of the table sees.
func (f *egressFixture) rowsOf(t *testing.T, table string) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, "SELECT row_to_json(t)::text FROM "+table+" t ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// design/EGRESS.md §2.4: "the receiver drops every field outside the schema on receipt, before any
// aggregation, log line or write". A token planted in an absolute-form request's query string — and in
// its headers, its referer and the address Envoy resolved — is in no stored row and no log line.
func TestALSPathNeverStoredOrLogged(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_6f1c0b9a7e2a9d3c")})
	token := "sk_live_" + strings.Repeat("Q", 24)
	leaky := entry("172.28.3.11", corev3.RequestMethod_GET, "http", "api.example.com", 200)
	leaky.Request.Path = "/v1/charges?token=" + token
	leaky.Request.OriginalPath = "/v1/charges?token=" + token
	leaky.Request.Referer = "https://claims.example.com/?token=" + token
	leaky.Request.UserAgent = "agent " + token
	leaky.Request.ForwardedFor = "10.9.9.9"
	leaky.Request.RequestHeaders = map[string]string{"authorization": "Bearer " + token}
	leaky.Response.ResponseHeaders = map[string]string{"set-cookie": "s=" + token}
	leaky.Response.ResponseCodeDetails = "via_upstream " + token
	leaky.CommonProperties.UpstreamRemoteAddress = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: "93.184.215.14"}}}
	leaky.CommonProperties.CustomTags = map[string]string{"t": token}
	leaky.CommonProperties.RouteName = token
	tunnel := entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	f.send(t, leaky, tunnel)
	stored := f.rowsOf(t, "egress_destinations")
	if len(stored) != 2 {
		t.Fatalf("stored %d rows, want one per destination: %v", len(stored), stored)
	}
	for _, leak := range []string{token, "/v1/charges", "93.184.215.14", "10.9.9.9", "172.28.3.11", "Bearer", "claims.example.com"} {
		if s := strings.Join(stored, "\n"); strings.Contains(s, leak) {
			t.Errorf("a stored row carries %q:\n%s", leak, s)
		}
		if strings.Contains(f.logged.String(), leak) {
			t.Errorf("a log line carries %q:\n%s", leak, f.logged.String())
		}
	}
	var app, dest, protocol, class string
	var port int
	if err := f.pool.QueryRow(f.ctx, "SELECT app, destination, port, last_protocol, last_status_class FROM egress_destinations WHERE last_protocol = 'http'").Scan(&app, &dest, &port, &protocol, &class); err != nil {
		t.Fatal(err)
	}
	if app != "claims-portal" || dest != "api.example.com" || port != 80 || class != "2xx" {
		t.Fatalf("the absolute-form call is recorded as %s %s:%d %s %s", app, dest, port, protocol, class)
	}
}

// design/EGRESS.md §2.3: "an IP claimed by more than one pod, or by a terminating pod, is unknown". The
// executor leaves a terminating pod's address out; an answer naming one address twice — two pods — is no
// one's on the server too, never the first of the two.
func TestAmbiguousOrTerminatingIPIsUnknown(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(
		executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_aaaa1111")},
		executor.Caller{Address: "10.0.4.7", App: "case-tracker", Target: "preview", Kind: "web", Run: ranAs("gr_bbbb2222")},
		executor.Caller{Address: "10.0.4.9", App: "case-tracker", Target: "prod", Kind: "web", Run: nil},
	)
	f.send(t,
		entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200),  // two pods claim it
		entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "hooks.slack.com:443", 200), // a terminating pod's: left out
		entry("10.0.4.9", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 200),  // one pod's
	)
	if got, want := f.callersOf(t), map[string]string{"api.openai.com": "case-tracker/prod"}; !maps.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
	if got, want := f.gapsOf(t), []string{"unattributed:2"}; !slices.Equal(got, want) {
		t.Errorf("gaps %v, want the two calls of no one's counted %v", got, want)
	}
	if f.exec.CallersAsks.Load() != 1 {
		t.Errorf("the batch asked POST /callers %d times, want once", f.exec.CallersAsks.Load())
	}
}

// callersOf is each stored destination's app env.
func (f *egressFixture) callersOf(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	rows, err := f.pool.Query(f.ctx, "SELECT destination, app || '/' || env FROM egress_destinations")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var dest, caller string
		if err := rows.Scan(&dest, &caller); err != nil {
			t.Fatal(err)
		}
		got[dest] = caller
	}
	return got
}

// design/EGRESS.md §2.4: "every call to a new key past it is counted on that app env's over_cap gap —
// never a row, never silent". Across flushes; a destination already a row keeps being counted on it; the
// app's other env has a cap of its own; a call of no one's is unattributed, never over_cap.
func TestNewDestinationPastCapCountsOverCap(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_6f1c0b9a7e2a9d3c")},
		executor.Caller{Address: "172.28.3.10", App: "claims-portal", Target: "preview", Kind: "web", Run: nil})
	var first []*accesslogdatav3.HTTPAccessLogEntry
	for i := range 501 {
		first = append(first, entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", fmt.Sprintf("d%d.example.com:443", i), 200))
	}
	f.send(t, first...)
	f.send(t,
		entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "d7.example.com:443", 200),
		entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "d999.example.com:443", 200),
		entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "d999.example.com:443", 200),
		entry("172.28.3.10", corev3.RequestMethod_CONNECT, "", "d999.example.com:443", 200), // preview's own cap
		entry("172.28.3.12", corev3.RequestMethod_CONNECT, "", "d999.example.com:443", 200), // no one's
	)
	var prod, preview, d7 int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE env = 'prod'), count(*) FILTER (WHERE env = 'preview'),
		coalesce(sum(calls) FILTER (WHERE destination = 'd7.example.com'), 0) FROM egress_destinations`).Scan(&prod, &preview, &d7); err != nil {
		t.Fatal(err)
	}
	if prod != 500 || preview != 1 || d7 != 2 {
		t.Fatalf("prod %d rows, preview %d, d7 %d calls; want 500, 1, 2", prod, preview, d7)
	}
	rows, err := f.pool.Query(f.ctx, "SELECT reason || ':' || calls || ':' || coalesce(app || '/' || env, '-') FROM egress_gaps ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	gaps, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"over_cap:3:claims-portal/prod", "unattributed:1:-"}; !slices.Equal(gaps, want) { // d500, then d999 twice, on one row
		t.Fatalf("gaps %v, want %v", gaps, want)
	}
}

// design/EGRESS.md §2.1: a long tunnel's periodic entries name its caller while it lives; its last entry,
// after the pod is gone, counts the call under that caller, once.
func TestATunnelKeepsTheCallerItHadWhileItLived(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_aaaa1111")})
	periodic := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	periodic.CommonProperties.StreamId, periodic.CommonProperties.AccessLogType = "s-1", accesslogdatav3.AccessLogType_DownstreamPeriodic
	f.send(t, periodic)
	f.exec.Holds() // the pod is gone
	last := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	last.CommonProperties.StreamId = "s-1"
	f.send(t, last)
	var app string
	var count int
	if err := f.pool.QueryRow(f.ctx, "SELECT app, calls FROM egress_destinations").Scan(&app, &count); err != nil {
		t.Fatal(err)
	}
	if app != "claims-portal" || count != 1 {
		t.Fatalf("the tunnel is recorded as %d call(s) of %s, want 1 of claims-portal", count, app)
	}
}

// design/EGRESS.md §2.4: "No partition: a call taken at 23:59:50 and flushed at 00:00:10 is a row like
// any other (#2221)."
func TestCallBeforeMidnightFlushedAfterIsRecorded(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	before := time.Date(2026, 10, 13, 23, 59, 50, 0, time.UTC)
	f.r.now = func() time.Time { return before }
	f.stream(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200))
	f.r.now = func() time.Time { return before.Add(20 * time.Second) }
	if err := f.r.flush(f.ctx, []call{<-f.r.calls}); err != nil {
		t.Fatalf("a call taken at 23:59:50 and flushed at 00:00:10: %v", err)
	}
	var seen time.Time
	var calls int
	if err := f.pool.QueryRow(f.ctx, "SELECT last_seen_at, calls FROM egress_destinations WHERE destination = 'api.stripe.com'").Scan(&seen, &calls); err != nil {
		t.Fatal(err)
	}
	if !seen.Equal(before) || calls != 1 {
		t.Fatalf("recorded %d call(s) last seen %s, want 1 at %s", calls, seen.UTC(), before)
	}
}

// design/EGRESS.md §2.4: one upsert per key — calls added, first seen the earlier, last seen the later,
// and the last result the newer call's, though a flush with an older call arrives after it.
func TestFlushUpsertsOneRowPerDestination(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	f.r.now = func() time.Time { return at }
	failed := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 503)
	failed.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200), failed)
	f.r.now = func() time.Time { return at.Add(-time.Minute) } // an older call, flushed later
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_GET, "https", "api.stripe.com", 200))
	var rows, calls int
	var first, last time.Time
	var class, protocol string
	var flags []string
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) OVER (), calls, first_seen_at, last_seen_at, last_status_class, last_protocol, last_response_flags
		FROM egress_destinations`).Scan(&rows, &calls, &first, &last, &class, &protocol, &flags); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || calls != 3 || !first.Equal(at.Add(-time.Minute)) || !last.Equal(at) || class != "5xx" || protocol != "connect" || !slices.Equal(flags, []string{"UF"}) {
		t.Fatalf("%d row(s): %d calls, first %s, last %s, last result %s %s %v; want 1: 3, the older call's first, the newer's last, 5xx connect [UF]",
			rows, calls, first.UTC(), last.UTC(), class, protocol, flags)
	}
}

// design/EGRESS.md §2.4, Age: "A destination unseen for 30 days is deleted, and a gap that ended 30 days ago
// with it"; one seen 29 days ago, and a gap that ended then, stay.
func TestUnseenDestinationAgesOut(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		VALUES ('claims-portal', 'prod', 'old.example.com', 443, now() - interval '40 days', now() - interval '31 days', 1, 'allowed', '2xx', 'connect', '{}'),
			('claims-portal', 'prod', 'kept.example.com', 443, now() - interval '40 days', now() - interval '29 days', 1, 'allowed', '2xx', 'connect', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO egress_gaps (from_at, to_at, reason, calls) VALUES (now() - interval '32 days', now() - interval '31 days', 'dropped', 1),
		(now() - interval '30 days', now() - interval '29 days', 'dropped', 2)`); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { f.r.run(ctx); close(done) }()
	waitFor(t, "the destination unseen for 31 days to age out", func() bool {
		return f.countOf(t, "SELECT count(*) FROM egress_destinations WHERE destination = 'old.example.com'") == 0
	})
	stop()
	<-done
	if got := f.callersOf(t); !maps.Equal(got, map[string]string{"kept.example.com": "claims-portal/prod"}) {
		t.Fatalf("destinations %v, want the one seen 29 days ago alone", got)
	}
	if got, want := f.gapsOf(t), []string{"dropped:2"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want the one that ended 29 days ago alone %v", got, want)
	}
}

// A prune and a flush on two connections never wait on each other in a cycle (#2233). The flush holds the
// first rows in its key order, a-app's destination and over_cap gap, both unseen 31 days, then writes
// b-app's, which the prune meets first: b-app's are the older and were written first, however it scans.
func TestAPruneNeverDeadlocksAFlush(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		VALUES ('b-app', 'prod', 'old.example.com', 443, now() - interval '40 days', now() - interval '32 days', 1, 'allowed', '2xx', 'connect', '{}'),
			('a-app', 'prod', 'old.example.com', 443, now() - interval '40 days', now() - interval '31 days', 1, 'allowed', '2xx', 'connect', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES (now() - interval '33 days', now() - interval '32 days', 'over_cap', 1, 'b-app', 'prod'),
		(now() - interval '32 days', now() - interval '31 days', 'over_cap', 1, 'a-app', 'prod')`); err != nil {
		t.Fatal(err)
	}
	flush, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flush.Rollback(f.ctx) }()
	seen := func(app string) {
		t.Helper()
		for _, q := range []string{"UPDATE egress_destinations SET last_seen_at = now() WHERE app = $1", "UPDATE egress_gaps SET to_at = now() WHERE app = $1"} {
			if _, err := flush.Exec(f.ctx, q, app); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen("a-app")
	var xid string
	if err := flush.QueryRow(f.ctx, "SELECT xid(pg_current_xact_id())::text").Scan(&xid); err != nil {
		t.Fatal(err)
	}
	pruned := make(chan error, 1)
	go func() { pruned <- f.r.prune(f.ctx) }()
	waitFor(t, "the prune to finish, or to wait on the flush", func() bool {
		var waits bool
		if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1)", xid).Scan(&waits); err != nil {
			t.Fatal(err)
		}
		return len(pruned) > 0 || waits
	})
	seen("b-app")
	if err := flush.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-pruned; err != nil {
		t.Fatal(err)
	}
	if got := f.callersOf(t); !maps.Equal(got, map[string]string{"old.example.com": "a-app/prod"}) {
		t.Fatalf("destinations %v, want a-app's alone, which the flush saw again", got)
	}
	if got, want := f.overCapOf(t), []string{"a-app/prod:1"}; !slices.Equal(got, want) {
		t.Fatalf("over_cap %v, want %v", got, want)
	}
}

// design/EGRESS.md §2.4: "A gap that continues the last one written of its reason, app env and countedness
// — starting where that row ends — extends that row"; one after a flush without it starts a row of its own.
func TestContinuingGapExtendsItsRow(t *testing.T) {
	f := newEgressFixture(t)
	for _, dropped := range []int64{2, 3, 0, 1} {
		f.r.dropped.Add(dropped)
		if err := f.r.flush(f.ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := f.gapsOf(t), []string{"dropped:1", "dropped:5"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// CANYON_EGRESS_PROXY is Envoy's listener as executor.schema.json spells it, or the boot fails naming it.
func TestEgressProxyIsEnvoysOrRefused(t *testing.T) {
	for value, ok := range map[string]bool{
		"": true, "http://canyon-egress:3128": true, "http://canyon-egress.canyon.svc.cluster.local:3128": true,
		"https://canyon-egress:3128": false, "http://canyon-egress:8080": false, "http://user:pw@canyon-egress:3128": false, "canyon-egress:3128": false,
	} {
		if _, err := egressProxyOf(value); (err == nil) != ok {
			t.Errorf("%q: error %v, want accepted %v", value, err, ok)
		}
	}
}

// gapsOf is every gap recorded, as reason:calls, calls "-" for receiver_down's none.
func (f *egressFixture) gapsOf(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, "SELECT reason || ':' || coalesce(calls::text, '-') FROM egress_gaps ORDER BY reason, calls")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Nothing about a call is lost without a record of it (design/EGRESS.md §2.4, Gaps): a call the buffer had
// no room for, a caller the executor could not answer for, a batch that could not be written, and the time
// the receiver was down.
func TestEveryLostCallIsAGap(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, "INSERT INTO egress_receiver (seen_at) VALUES (now() - interval '10 minutes')"); err != nil {
		t.Fatal(err)
	}
	if err := f.r.boot(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.r.dropped.Add(3)
	f.exec.FailCallers.Store(true)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200), entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 200))
	if got, want := f.gapsOf(t), []string{"dropped:3", "receiver_down:-", "unattributed:2"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations"); n != 0 {
		t.Fatalf("%d rows of calls no one was named for, want none: they are the gap alone", n)
	}
	if _, err := f.pool.Exec(f.ctx, "ALTER TABLE egress_destinations ADD CONSTRAINT refuse CHECK (false) NOT VALID"); err != nil { // the write fails
		t.Fatal(err)
	}
	f.exec.FailCallers.Store(false)
	stream, err := f.client.StreamAccessLogs(f.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{
		LogEntry: []*accesslogdatav3.HTTPAccessLogEntry{entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if err := f.r.flush(f.ctx, []call{<-f.r.calls}); err == nil {
		t.Fatal("a batch with nowhere to go was written")
	}
	if got, want := f.gapsOf(t), []string{"dropped:3", "not_recorded:1", "receiver_down:-", "unattributed:2"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// A call is the app's whose address Envoy saw it come from: the direct peer, never X-Forwarded-For, a
// caller header or the remote address Envoy derives from them — each the app's own to write. A call
// forged to name another app's address stays the forger's.
func TestAForgedHeaderCannotMoveACall(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(
		executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_aaaa1111")},
		executor.Caller{Address: "172.28.9.11", App: "case-tracker", Target: "prod", Kind: "web", Run: ranAs("gr_bbbb2222")},
	)
	forged := entry("172.28.3.11", corev3.RequestMethod_GET, "http", "api.example.com", 200)
	forged.Request.ForwardedFor = "172.28.9.11"
	forged.Request.RequestHeaders = map[string]string{"x-forwarded-for": "172.28.9.11", "x-canyon-caller": "case-tracker/prod", "x-real-ip": "172.28.9.11"}
	forged.CommonProperties.DownstreamRemoteAddress = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: "172.28.9.11"}}}
	f.send(t, forged)
	var app string
	if err := f.pool.QueryRow(f.ctx, "SELECT coalesce(string_agg(app, ','), 'none') FROM egress_destinations").Scan(&app); err != nil {
		t.Fatal(err)
	}
	if app != "claims-portal" {
		t.Fatalf("a call from claims-portal's address carrying case-tracker's in its headers is recorded under %s", app)
	}
}

// What the receiver holds is bounded, and what a bound turns away is counted, never lost unseen: streams
// past egressStreams are refused for Envoy to retry, an entry past egressEntryBytes and a call past the
// buffer are dropped gaps, addresses past egressAddressesFlush are unattributed gaps, and tunnels past
// egressTunnels are attributed by their last entry instead of remembered.
func TestTheReceiverBoundsWhatItHolds(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	for range egressStreams {
		f.r.open <- struct{}{}
	}
	stream, err := f.client.StreamAccessLogs(f.call)
	if err == nil {
		_, err = stream.CloseAndRecv()
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a stream past %d at once: %v, want ResourceExhausted", egressStreams, err)
	}
	for range egressStreams {
		<-f.r.open
	}
	big := entry("10.0.4.7", corev3.RequestMethod_GET, "http", "api.example.com", 200)
	big.Request.UserAgent = strings.Repeat("x", egressEntryBytes)
	f.r.calls = make(chan call, 2)
	f.send(t, big, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "a.example.com:443", 200), entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "b.example.com:443", 200),
		entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "c.example.com:443", 200))
	if got, want := f.gapsOf(t), []string{"dropped:2"}; !slices.Equal(got, want) { // the big entry and the call past the buffer of two
		t.Fatalf("gaps %v, want %v", got, want)
	}
	if rows := f.rowsOf(t, "egress_destinations"); len(rows) != 2 || strings.Contains(strings.Join(rows, " "), "api.example.com") {
		t.Fatalf("the big entry was read, or a call past the buffer kept: %v", rows)
	}
	f.r.calls = make(chan call, egressBuffer)
	var many []*accesslogdatav3.HTTPAccessLogEntry
	for i := range egressAddressesFlush + 1 {
		many = append(many, entry(fmt.Sprintf("10.1.%d.%d", i/250, i%250+1), corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200))
	}
	asks := f.exec.CallersAsks.Load()
	f.send(t, many...)
	if n := f.exec.CallersAsks.Load() - asks; n != egressAddressesFlush/egressCallersAsked {
		t.Errorf("one flush asked POST /callers %d times, want %d", n, egressAddressesFlush/egressCallersAsked)
	}
	if got, want := f.gapsOf(t), []string{"dropped:2", "unattributed:4097"}; !slices.Equal(got, want) { // the 4,097th address unasked, and no one holds the rest
		t.Fatalf("gaps %v, want %v", got, want)
	}
	for i := range egressTunnels {
		f.r.tunnels[tunnel{netip.MustParseAddr("10.9.0.1"), fmt.Sprintf("held-%d", i)}] = streamCaller{seen: time.Now()}
	}
	periodic := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	periodic.CommonProperties.StreamId, periodic.CommonProperties.AccessLogType = "s-new", accesslogdatav3.AccessLogType_DownstreamPeriodic
	f.send(t, periodic)
	if _, ok := f.r.tunnels[tunnel{netip.MustParseAddr("10.0.4.7"), "s-new"}]; ok || len(f.r.tunnels) != egressTunnels {
		t.Fatalf("%d tunnels held, the new one among them %v; want %d, not it", len(f.r.tunnels), ok, egressTunnels)
	}
}

// The receiver listens on the server's own IPv4 address on canyon-egress-ctl alone, with the token Envoy
// sends, or the server does not start: never every interface, loopback, a name, IPv6 or an address of the
// apps' pool, never without the pool to refuse. On Kubernetes (#2286) it takes 0.0.0.0 and no other address, as
// no manifest knows the pod's address, and needs no pool — the server's NetworkPolicy admits the gateway's pods
// alone — but still Envoy's token.
func TestTheReceiverTakesItsAddressAndTokenOrRefusesToStart(t *testing.T) {
	pool := netip.MustParsePrefix("10.213.0.0/16")
	for _, tc := range []struct {
		addr  string
		kube  bool
		apps  netip.Prefix
		token string
		on    bool
	}{
		{"10.215.0.2:7781", false, pool, egressTestToken, true},
		{"", false, pool, "", false}, {"", true, netip.Prefix{}, "", false},
		{"0.0.0.0:7781", true, netip.Prefix{}, egressTestToken, true}, {":7781", true, netip.Prefix{}, egressTestToken, false},
		{"0.0.0.0:7781", true, netip.Prefix{}, "", false}, {"0.0.0.0:7781", true, netip.Prefix{}, strings.Repeat("a", 31), false},
		{"127.0.0.1:7781", true, netip.Prefix{}, egressTestToken, false}, {"canyon-server:7781", true, netip.Prefix{}, egressTestToken, false},
		{"[::]:7781", true, netip.Prefix{}, egressTestToken, false}, {"0.0.0.0:0", true, netip.Prefix{}, egressTestToken, false},
		{"10.215.0.2:7781", true, netip.Prefix{}, egressTestToken, false}, {"10.244.0.7:7781", true, pool, egressTestToken, false},
		{"10.215.0.2:7781", false, netip.Prefix{}, egressTestToken, false},
		{":7781", false, pool, egressTestToken, false}, {"0.0.0.0:7781", false, pool, egressTestToken, false},
		{"127.0.0.1:7781", false, pool, egressTestToken, false}, {"[fd00:215::2]:7781", false, pool, egressTestToken, false},
		{"canyon-server:7781", false, pool, egressTestToken, false}, {"10.213.4.2:7781", false, pool, egressTestToken, false},
		{"10.215.0.2:0", false, pool, egressTestToken, false}, {"10.215.0.2", false, pool, egressTestToken, false},
		{"10.215.0.2:7781", false, pool, "", false}, {"10.215.0.2:7781", false, pool, "short", false},
		{"10.215.0.2:7781", false, pool, strings.Repeat("a", 31), false}, {"10.215.0.2:7781", false, pool, strings.Repeat("a", 31) + " ", false},
	} {
		on, err := egressReceiverOn(tc.addr, tc.kube, tc.apps, tc.token)
		if on != tc.on || (err != nil) == (tc.on || tc.addr == "") {
			t.Errorf("%q kube %v pool %v token %q: on %v, error %v; want on %v", tc.addr, tc.kube, tc.apps, tc.token, on, err, tc.on)
		}
	}
}

// alsClient serves a receiver on loopback, its peers seen as peer and the pool refused, and answers a client.
func alsClient(t *testing.T, r *egressReceiver, pool, peer string) accesslogv3.AccessLogServiceClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newALSServer(r, egressTestToken)
	go func() {
		_ = srv.Serve(refuseAppPool(peerAs{ln, netip.MustParseAddr(peer)}, netip.MustParsePrefix(pool)))
	}()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return accesslogv3.NewAccessLogServiceClient(conn)
}

// streamCode opens one stream under ctx, sends nothing, closes it and answers the stream's status code.
func streamCode(ctx context.Context, c accesslogv3.AccessLogServiceClient) codes.Code {
	stream, err := c.StreamAccessLogs(ctx)
	if err == nil {
		_, err = stream.CloseAndRecv()
	}
	return status.Code(err)
}

// A stream without Envoy's token — none, another, one sent twice — is UNAUTHENTICATED before it takes a
// stream slot, so a peer without the token cannot fill the slots Envoy needs; an app's address never
// reaches the receiver at all.
func TestTheReceiverTakesEnvoysTokenAlone(t *testing.T) {
	r := newEgressReceiver(nil, nil, log.New(io.Discard, "", 0))
	c := alsClient(t, r, "10.213.0.0/16", "10.215.0.3")
	for range egressStreams {
		r.open <- struct{}{} // every slot taken: a refusal for the token comes first
	}
	bg := context.Background()
	for name, ctx := range map[string]context.Context{
		"no token":         bg,
		"another token":    metadata.AppendToOutgoingContext(bg, "authorization", "Bearer "+strings.Repeat("x", 64)),
		"the token, bare":  metadata.AppendToOutgoingContext(bg, "authorization", egressTestToken),
		"the token, twice": metadata.AppendToOutgoingContext(bg, "authorization", "Bearer "+egressTestToken, "authorization", "Bearer "+egressTestToken),
	} {
		if got := streamCode(ctx, c); got != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, got)
		}
	}
	envoy := metadata.AppendToOutgoingContext(bg, "authorization", "Bearer "+egressTestToken)
	if got := streamCode(envoy, c); got != codes.ResourceExhausted {
		t.Errorf("Envoy's token with every slot taken: %v, want ResourceExhausted", got)
	}
	for range egressStreams {
		<-r.open
	}
	if got := streamCode(envoy, c); got != codes.OK {
		t.Errorf("Envoy's token: %v, want OK", got)
	}
	if got := streamCode(envoy, alsClient(t, r, "10.213.0.0/16", "10.213.4.10")); got == codes.OK {
		t.Error("an app's address reached the receiver")
	}
}

// On Kubernetes (#2286) the receiver listens on 0.0.0.0, IPv4 alone, with no pool to refuse: Envoy's token is
// then all that tells its stream from any other peer's — none, another, or the token bare is UNAUTHENTICATED,
// and Envoy's is taken.
func TestTheReceiverOnEveryAddressTakesEnvoysTokenAlone(t *testing.T) {
	ln, err := listenReceiver("0.0.0.0:0") // as main listens on CANYON_EGRESS_RECEIVER_ADDR on Kubernetes
	if err != nil {
		t.Fatal(err)
	}
	srv := newALSServer(newEgressReceiver(nil, nil, log.New(io.Discard, "", 0)), egressTestToken)
	go func() { _ = srv.Serve(refuseAppPool(ln, netip.Prefix{})) }()
	t.Cleanup(srv.Stop)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	conn, err := grpc.NewClient("127.0.0.1:"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c, bg := accesslogv3.NewAccessLogServiceClient(conn), context.Background()
	for name, ctx := range map[string]context.Context{
		"no token":        bg,
		"another token":   metadata.AppendToOutgoingContext(bg, "authorization", "Bearer "+strings.Repeat("x", 64)),
		"the token, bare": metadata.AppendToOutgoingContext(bg, "authorization", egressTestToken),
	} {
		if got := streamCode(ctx, c); got != codes.Unauthenticated {
			t.Errorf("%s over 0.0.0.0 with no pool: %v, want Unauthenticated", name, got)
		}
	}
	if got := streamCode(metadata.AppendToOutgoingContext(bg, "authorization", "Bearer "+egressTestToken), c); got != codes.OK {
		t.Errorf("Envoy's token over 0.0.0.0 with no pool: %v, want OK", got)
	}
}

// The receiver listens IPv4 alone (contracts/routes.md, Egress control; #2286): on 0.0.0.0, as on Kubernetes, an
// IPv6 peer never connects — a "tcp" listener on 0.0.0.0 is a dual-stack socket that takes one — and an IPv4
// peer does.
func TestTheReceiverListensIPv4Alone(t *testing.T) {
	v6, err := net.Listen("tcp6", "[::1]:0") // the control: this host speaks IPv6 on loopback
	if err != nil {
		t.Fatalf("no IPv6 loopback here (%v): the receiver's refusing an IPv6 peer would prove nothing", err)
	}
	_ = v6.Close()
	ln, err := listenReceiver("0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if c, err := net.Dial("tcp6", "[::1]:"+port); err == nil {
		_ = c.Close()
		t.Errorf("an IPv6 peer connected to the receiver on %s", ln.Addr())
	}
	if c, err := net.Dial("tcp4", "127.0.0.1:"+port); err != nil {
		t.Errorf("an IPv4 peer did not connect to the receiver on %s: %v", ln.Addr(), err)
	} else {
		_ = c.Close()
	}
}

// A stream id longer than 64 bytes is none: it names no tunnel to carry a caller by.
func TestALongStreamIDIsNone(t *testing.T) {
	e := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	e.CommonProperties.StreamId = strings.Repeat("s", 64)
	if c := callOf(e, time.Now(), false); c.stream != e.CommonProperties.StreamId {
		t.Fatalf("a 64-byte stream id read as %q", c.stream)
	}
	e.CommonProperties.StreamId += "s"
	if c := callOf(e, time.Now(), false); c.stream != "" {
		t.Fatalf("a 65-byte stream id read as %q, want none", c.stream)
	}
}

// A tunnel is its source's: another address sending the same stream id — forged, or Envoy's on another
// connection — keeps its own caller; and a periodic entry no one answered for never hides the caller its
// last entry has.
func TestATunnelIsItsSourcesAlone(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(
		executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web", Run: ranAs("gr_aaaa1111")},
		executor.Caller{Address: "172.28.9.11", App: "case-tracker", Target: "prod", Kind: "web", Run: ranAs("gr_bbbb2222")},
	)
	periodic := func(source, dest string) *accesslogdatav3.HTTPAccessLogEntry {
		e := entry(source, corev3.RequestMethod_CONNECT, "", dest, 200)
		e.CommonProperties.StreamId, e.CommonProperties.AccessLogType = "s-1", accesslogdatav3.AccessLogType_DownstreamPeriodic
		return e
	}
	last := func(source, dest string) *accesslogdatav3.HTTPAccessLogEntry {
		e := entry(source, corev3.RequestMethod_CONNECT, "", dest, 200)
		e.CommonProperties.StreamId = "s-1"
		return e
	}
	f.send(t, periodic("172.28.3.11", "api.stripe.com:443"))
	f.send(t, last("172.28.9.11", "hooks.slack.com:443")) // the same stream id from case-tracker's address
	f.send(t, periodic("10.0.4.7", "api.openai.com:443")) // no one holds it yet
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: nil})
	f.send(t, last("10.0.4.7", "api.openai.com:443"))
	if got, want := f.callersOf(t), map[string]string{"hooks.slack.com": "case-tracker/prod", "api.openai.com": "claims-portal/preview"}; !maps.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

// Every call no app is named for is counted as unattributed — an IPv6 source, an address no one holds,
// one the executor did not answer for in time — and only once its batch is written; the next flush's
// continues the same row.
func TestEveryCallOfNoOnesIsCounted(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.9", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.send(t, entry("fd00::7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200), entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200),
		entry("10.0.4.9", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200))
	if got, want := f.gapsOf(t), []string{"unattributed:2"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
	f.r.wait = 100 * time.Millisecond
	f.r.callers = func(ctx context.Context, _ []string) ([]executor.Caller, error) { <-ctx.Done(); return nil, ctx.Err() } // an executor that never answers
	began := time.Now()
	f.send(t, entry("10.0.4.9", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200))
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("a flush waited %s on an executor that never answers", took)
	}
	if got, want := f.gapsOf(t), []string{"unattributed:3"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// A message over the 4 MiB cap is refused before it is read, and its calls, which no one counted, are a
// dropped gap with no count.
func TestAMessageTooLargeToReadIsADroppedGap(t *testing.T) {
	f := newEgressFixture(t)
	var big []*accesslogdatav3.HTTPAccessLogEntry
	for range egressMessageBytes / (8 << 10) {
		e := entry("10.0.4.7", corev3.RequestMethod_GET, "http", "api.example.com", 200)
		e.Request.UserAgent = strings.Repeat("u", 8<<10)
		big = append(big, e)
	}
	stream, err := f.client.StreamAccessLogs(f.call)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{LogEntry: big}}})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a message over the cap: %v, want ResourceExhausted", err)
	}
	if err := f.r.flush(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.gapsOf(t), []string{"dropped:-"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// The destination cap counts (destination, port) pairs: one name on 600 ports is 500 rows and 100 calls
// over the cap.
func TestManyPortsOfOneNameAreCapped(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	var entries []*accesslogdatav3.HTTPAccessLogEntry
	for i := range 600 {
		entries = append(entries, entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", fmt.Sprintf("api.example.com:%d", 1000+i), 200))
	}
	f.send(t, entries...)
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations WHERE destination = 'api.example.com'"); n != 500 {
		t.Fatalf("%d rows, want 500", n)
	}
	if got, want := f.gapsOf(t), []string{"over_cap:100"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// waitFor polls cond until it holds or five seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("waited 5 s for %s", what)
		}
	}
}

// countOf is one number a query answers.
func (f *egressFixture) countOf(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// run flushes on its tick, and on its way out flushes what it holds on a context of its own: a stopping
// server loses no batch (#2213: passes with -count=5, -shuffle=on and in one process).
func TestRunFlushesEveryTickAndOnTheWayOut(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.r.every = 50 * time.Millisecond
	ctx, stop := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { f.r.run(ctx); close(done) }()
	f.stream(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200))
	waitFor(t, "a tick's flush", func() bool { return f.countOf(t, "SELECT count(*) FROM egress_destinations") == 1 })
	stop()
	<-done
	f.r.every = time.Hour
	ctx, stop = context.WithCancel(f.ctx)
	done = make(chan struct{})
	go func() { f.r.run(ctx); close(done) }()
	f.stream(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "hooks.slack.com:443", 200))
	waitFor(t, "run to take the call", func() bool { return len(f.r.calls) == 0 })
	stop()
	<-done
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations WHERE destination = 'hooks.slack.com'"); n != 1 {
		t.Fatalf("the call run held when it stopped: %d rows, want 1\n%s", n, f.logged.String())
	}
}

// A flush behind holds no more calls than the queue does: past it a call is a dropped gap, never memory.
func TestABatchBehindItsFlushIsBounded(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.r.every, f.r.calls = time.Hour, make(chan call, 2)
	ctx, stop := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { f.r.run(ctx); close(done) }()
	for i := range 5 {
		f.stream(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", fmt.Sprintf("d%d.example.com:443", i), 200))
		waitFor(t, "run to take the call", func() bool { return len(f.r.calls) == 0 })
	}
	stop()
	<-done
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations"); n != 2 {
		t.Fatalf("%d calls recorded, want the batch's 2", n)
	}
	if got, want := f.gapsOf(t), []string{"dropped:3"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// A clock stepped back never writes a gap that ends before it starts, which its table refuses and which
// would keep every later gap from being written with it.
func TestAGapNeverEndsBeforeItStarts(t *testing.T) {
	r := newEgressReceiver(nil, nil, log.New(io.Discard, "", 0))
	from := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	r.gap(egressGap{from: from, to: from.Add(-time.Minute), reason: "dropped", calls: 1})
	if g := r.gaps["dropped"]; g.to.Before(g.from) {
		t.Fatalf("a gap from %s to %s", g.from, g.to)
	}
}

// A receiver stopped with its server ends cleanly — no error reported, so main never exits non-zero — and
// it stops taking streams before its last flush, which records what it took.
func TestAStoppedReceiverEndsCleanly(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(f.ctx)
	done, failed := receiveEgress(ctx, ln, netip.MustParsePrefix("10.213.0.0/16"), f.r, egressTestToken)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	stream, err := accesslogv3.NewAccessLogServiceClient(conn).StreamAccessLogs(f.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{
		LogEntry: []*accesslogdatav3.HTTPAccessLogEntry{entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	stop()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the receiver did not finish its last flush")
	}
	select {
	case err := <-failed:
		t.Fatalf("a stopped receiver reported %v; main would exit non-zero", err)
	case <-time.After(100 * time.Millisecond):
	}
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations"); n != 1 {
		t.Fatalf("%d rows after the last flush, want the streamed call's 1", n)
	}
}

// An invalid destination is no name to cap by: its calls are one row on 443 whatever ports they named, so an
// app cannot fill the table with ports of a name that is no host, nor its cap.
func TestAnInvalidHostsPortsAreOneRow(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	var entries []*accesslogdatav3.HTTPAccessLogEntry
	for i := range 600 {
		entries = append(entries, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", fmt.Sprintf("bad_host.example.com:%d", 1000+i), 200))
	}
	f.send(t, entries...)
	var rows, calls int
	var ports string
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*), coalesce(sum(calls), 0), coalesce(string_agg(DISTINCT port::text, ','), '') FROM egress_destinations WHERE destination = 'invalid'").Scan(&rows, &calls, &ports); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || calls != 600 || ports != "443" {
		t.Fatalf("%d invalid rows of %d calls on ports %.40q; want 1 of 600 on 443", rows, calls, ports)
	}
}

// A dropped gap no one counted — a message too large to read — is kept apart from a counted one, whose
// count it would otherwise lose.
func TestACountedDropKeepsItsCount(t *testing.T) {
	r := newEgressReceiver(nil, nil, log.New(io.Discard, "", 0))
	at := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	r.gap(egressGap{from: at, to: at, reason: "dropped", calls: 3})
	r.gap(egressGap{from: at, to: at, reason: "dropped", uncounted: true})
	r.gap(egressGap{from: at, to: at, reason: "dropped", calls: 2})
	if len(r.gaps) != 2 || r.gaps["dropped"].calls != 5 || r.gaps["dropped"].uncounted {
		t.Fatalf("gaps %+v, want 5 counted and one uncounted apart", r.gaps)
	}
}

// fill gives app env n destinations already seen, fill-<i>.example.com.
func (f *egressFixture) fill(t *testing.T, app, env string, n int) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		SELECT $1, $2, 'fill-' || i || '.example.com', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}' FROM generate_series(1, $3) i`, app, env, n); err != nil {
		t.Fatal(err)
	}
}

// overCapOf is each over_cap gap as app/env:calls.
func (f *egressFixture) overCapOf(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, "SELECT app || '/' || env || ':' || calls FROM egress_gaps WHERE reason = 'over_cap' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The cap holds under two writers at once: two flushes, each with a new destination for an app env at 499,
// never leave it more than 500 rows — the second counts its call on over_cap. A third transaction holds
// the first flush's new row unwritten, so the second flush runs while the first is inside its transaction.
func TestTwoFlushesAtTheCapNeverPassIt(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "race-app", Target: "prod", Kind: "web", Run: nil})
	f.fill(t, "race-app", "prod", 499)
	held, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(f.ctx) }()
	var xid string
	if err := held.QueryRow(f.ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		VALUES ('race-app', 'prod', 'a.example.com', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}') RETURNING xid(pg_current_xact_id())::text`).Scan(&xid); err != nil {
		t.Fatal(err)
	}
	other := newEgressReceiver(f.pool, f.r.callers, log.New(f.logged, "", 0))
	callTo := func(dest string) []call {
		return []call{callOf(entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", dest, 200), time.Now(), false)}
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.r.flush(f.ctx, callTo("a.example.com:443")) }()
	waiting := func(q string, args ...any) bool {
		var ok bool
		if err := f.pool.QueryRow(f.ctx, q, args...).Scan(&ok); err != nil {
			t.Error(err)
			return true
		}
		return ok
	}
	waitFor(t, "the first flush to wait on the row held unwritten", func() bool {
		return waiting("SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1)", xid)
	})
	go func() { second <- other.flush(f.ctx, callTo("b.example.com:443")) }()
	waitFor(t, "the second flush to finish, or to wait for the first", func() bool {
		return len(second) > 0 || waiting(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
			AND objid::text::bigint = hashtext('egress_destinations race-app/prod')::bigint & 4294967295)`) // the app env's lock, egressLock
	})
	if err := held.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := f.countOf(t, "SELECT count(*) FROM egress_destinations WHERE app = 'race-app'"); n != 500 {
		t.Fatalf("race-app prod holds %d destinations after two flushes at once, want 500", n)
	}
	if got, want := f.overCapOf(t), []string{"race-app/prod:1"}; !slices.Equal(got, want) {
		t.Fatalf("over_cap %v, want %v", got, want)
	}
}

// Two app envs past their caps are two over_cap rows, each with its own calls.
func TestEachAppEnvPastItsCapHasItsOwnOverCap(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil},
		executor.Caller{Address: "10.0.4.8", App: "claims-portal", Target: "preview", Kind: "web", Run: nil})
	f.fill(t, "claims-portal", "prod", 500)
	f.fill(t, "claims-portal", "preview", 500)
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200), entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200),
		entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200), entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200),
		entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "other.example.com:443", 200))
	if got, want := f.overCapOf(t), []string{"claims-portal/preview:3", "claims-portal/prod:2"}; !slices.Equal(got, want) {
		t.Fatalf("over_cap %v, want %v", got, want)
	}
}

// An app env's over_cap is one row, extended by each burst past the cap however long between them.
func TestOverCapIsOneRowPerAppEnv(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.fill(t, "claims-portal", "prod", 500)
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200))
	f.send(t) // a flush with nothing past the cap: the next burst does not continue the last
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200), entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "other.example.com:443", 200))
	if got, want := f.overCapOf(t), []string{"claims-portal/prod:3"}; !slices.Equal(got, want) {
		t.Fatalf("over_cap %v, want %v", got, want)
	}
}

// A gap continuing two rows that end where it starts — written by two receivers at once, or by hand —
// extends one of them, never both: its calls are counted once.
func TestAContinuingGapExtendsOneRowAlone(t *testing.T) {
	f := newEgressFixture(t)
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	if _, err := f.pool.Exec(f.ctx, "INSERT INTO egress_gaps (from_at, to_at, reason, calls) VALUES ($1, $2, 'dropped', 1), ($1, $2, 'dropped', 1)", at.Add(-time.Minute), at); err != nil {
		t.Fatal(err)
	}
	f.r.last = at
	f.r.dropped.Add(3)
	if err := f.r.flush(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.countOf(t, "SELECT sum(calls) FROM egress_gaps"); n != 5 {
		t.Fatalf("the gaps count %d calls, want the two rows' 2 and the flush's 3", n)
	}
}

// The executor's answer is data: a caller whose target is neither preview nor prod, or whose app id is
// none Canyon gives, is no one's — its call unattributed — and the rest of the flush is written.
func TestACallerOutOfContractIsNoOnes(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "staging", Kind: "web", Run: nil},
		executor.Caller{Address: "10.0.4.8", App: "Claims Portal!", Target: "prod", Kind: "web", Run: nil},
		executor.Caller{Address: "10.0.4.9", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "a.example.com:443", 200), entry("10.0.4.8", corev3.RequestMethod_CONNECT, "", "b.example.com:443", 200),
		entry("10.0.4.9", corev3.RequestMethod_CONNECT, "", "c.example.com:443", 200))
	if got, want := f.callersOf(t), map[string]string{"c.example.com": "claims-portal/prod"}; !maps.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if got, want := f.gapsOf(t), []string{"unattributed:2"}; !slices.Equal(got, want) {
		t.Fatalf("gaps %v, want %v", got, want)
	}
}

// A holder is named for a call only while it held the address (#2265, contracts/executor.md, POST /callers):
// app A's call, its entry read once A stopped and B took A's address, is no one's, never B's; a call that
// started at or after B's since is B's; a call whose start Envoy did not say is no one's, and so is every call
// of an address whose holder the executor answers with no since.
func TestACallMadeBeforeItsHolderTookTheAddressIsNoOnes(t *testing.T) {
	since := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	r := newEgressReceiver(nil, func(_ context.Context, asked []string) ([]executor.Caller, error) {
		return []executor.Caller{{Address: "172.28.3.10", App: "case-tracker", Target: "preview", Kind: "web", Since: since},
			{Address: "172.28.3.11", App: "case-tracker", Target: "prod", Kind: "web"}}, nil
	}, log.New(io.Discard, "", 0))
	from := func(source string, started time.Time) call {
		e := entry(source, corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
		e.CommonProperties.StartTime = nil
		if !started.IsZero() {
			e.CommonProperties.StartTime = timestamppb.New(started)
		}
		return callOf(e, since.Add(time.Minute), false)
	}
	batch := []call{
		from("172.28.3.10", since.Add(-time.Millisecond)), // A's, before B held the address
		from("172.28.3.10", since),                        // B's, the instant it held it
		from("172.28.3.10", since.Add(time.Second)),       // B's
		from("172.28.3.10", time.Time{}),                  // no start said
		from("172.28.3.11", since.Add(time.Second)),       // a holder with no since
	}
	var got []string
	for _, c := range r.byAddress(context.Background(), batch) {
		if c == nil {
			got = append(got, "no one")
			continue
		}
		got = append(got, c.App+"/"+c.Target)
	}
	if want := []string{"no one", "case-tracker/preview", "case-tracker/preview", "no one", "no one"}; !slices.Equal(got, want) {
		t.Fatalf("named %v, want %v", got, want)
	}
}

// A flush writes its own app env's rows alone: another app's, and the same app's other env — their
// destinations, even one of the same name, and their over_cap gaps, even one ending where this flush's
// starts — are as they were.
func TestAFlushChangesItsOwnAppEnvsRowsAlone(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "prod", Kind: "web", Run: nil})
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	for _, ae := range [][2]string{{"claims-portal", "prod"}, {"claims-portal", "preview"}, {"case-tracker", "prod"}} {
		f.fill(t, ae[0], ae[1], 500)
		if _, err := f.pool.Exec(f.ctx, "INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES ($1, $2, 'over_cap', 1, $3, $4)", at.Add(-time.Minute), at, ae[0], ae[1]); err != nil {
			t.Fatal(err)
		}
	}
	others := func() string {
		var s string
		if err := f.pool.QueryRow(f.ctx, `SELECT coalesce(string_agg(r, E'\n' ORDER BY r), '') FROM (
			SELECT row_to_json(d)::text r FROM egress_destinations d WHERE (app, env) <> ('claims-portal', 'prod')
			UNION ALL SELECT row_to_json(g)::text FROM egress_gaps g WHERE (app, env) IS DISTINCT FROM ('claims-portal', 'prod')) rows`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := others()
	f.r.last = at
	f.send(t, entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "fill-1.example.com:443", 200), entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "new.example.com:443", 200))
	if after := others(); after != before {
		t.Fatalf("a flush of claims-portal prod changed another app env's rows:\nbefore\n%s\nafter\n%s", before, after)
	}
	if n := f.countOf(t, "SELECT calls FROM egress_destinations WHERE app = 'claims-portal' AND env = 'prod' AND destination = 'fill-1.example.com'"); n != 2 {
		t.Fatalf("claims-portal prod's fill-1 has %d calls, want 2", n)
	}
	if got := f.overCapOf(t); !slices.Contains(got, "claims-portal/prod:2") {
		t.Fatalf("over_cap %v, want claims-portal/prod:2 among them", got)
	}
}
