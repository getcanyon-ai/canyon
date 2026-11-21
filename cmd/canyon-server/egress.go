package main

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/destination"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// The receiver of Envoy's access logs (design/EGRESS.md §2.4): every HTTP entry becomes a call on
// receipt, holding the fields of contracts/schema/egress.schema.json, the source address it is
// attributed by and what egress-control's check said of it (contracts/routes.md, The check), and nothing
// else — Envoy's gRPC access logger cannot leave the path and query out, so this is the only place they are
// dropped. Nothing here logs or stores an entry.

const (
	egressFlushEvery     = 10 * time.Second
	egressKept           = 30 * 24 * time.Hour // a destination unseen this long is deleted, and a gap that ended this long ago
	egressBuffer         = 1 << 16             // calls waiting in the queue, and as many again in the batch: at worst 2×65,536; past either a call is dropped and counted
	egressStreamMemory   = 20 * time.Minute    // a tunnel's caller, from its periodic entries: max_stream_duration 15 min and a flush
	egressCallersAsked   = 1024                // POST /callers' bound
	egressAddressesFlush = 4096                // addresses one flush asks about, four POST /callers; the rest's calls are unattributed and counted
	egressStreams        = 16                  // ALS streams at once: Envoy holds one per worker thread, and runs with --concurrency 8
	egressCallersWait    = 5 * time.Second     // one POST /callers; past it the chunk's calls are no one's
	egressLastFlush      = 10 * time.Second    // the flush on the way out
	egressLastCallers    = time.Second         // one POST /callers on the way out: four at most leave the write six seconds
	egressStreamID       = 64                  // a stream id longer than this is none
	egressMessageBytes   = 4 << 20             // one ALS message; gRPC refuses a larger one
	egressEntryBytes     = 16 << 10            // one entry; a larger one is dropped and counted, never read
	egressTunnels        = 1 << 14             // tunnels whose caller the receiver keeps between their entries; past it a tunnel is attributed by its last
)

// call is one access-log entry as the receiver keeps it. source is the address it came from, used to
// name its caller and never stored; started is when Envoy says the call started, zero when it did not say,
// used to name a holder only for a call made while it held the address; stream is Envoy's stream id, used
// to carry a tunnel's caller from its periodic entries to its last.
type call struct {
	at          time.Time
	source      netip.Addr
	started     time.Time
	stream      string
	partial     bool // the call goes on, a periodic entry or an upgrade's open entry: its last entry ends it
	opened      bool // a tunnel's open entry: it counts the call and writes its row while the call goes on
	counted     bool // a later entry of a tunnel its open entry counted: it counts nothing; a periodic one moves last_seen_at, a last one writes the result
	destination string
	port        int
	protocol    string // connect, a tunnel; http, a plain request; https, an absolute-form https:// one; intercepted, an inner request
	statusClass string
	flags       []string
	outcome     string           // allowed, or refused: the check's, else refused with Envoy's UAEX (it did not answer)
	reason      string           // the check's, on a refusal, or on a not_allowed an egress-control from before #2344 let through
	checked     *executor.Caller // the caller the check named, which wins over the address's, or an inner request's canyon.caller; nil for none
	credential  string           // why Canyon refused it while adding a route's credential, "" when it did not (credentialRefusal)
}

// extAuthz is the namespace of the check's dynamic metadata, which an entry carries.
const extAuthz = "envoy.filters.http.ext_authz"

// callOf drops every field of the entry outside the record schema. It reads the direct peer's address,
// never X-Forwarded-For, which the app writes, and the check's metadata, which only egress-control writes. An entry
// of the interception listener's log, inner, is one request inside a tunnel Envoy ended (design/EGRESS.md §2.3, §4.2):
// intercepted, of no app's address and no tunnel of its own, its caller the filter-state object canyon.caller alone,
// which ext_authz #1 named for the tunnel, and none without one; allowed only when ext_authz #2 said so, so a request
// the listener answered itself, a misdirected authority's, never makes the name it asked for known.
func callOf(e *accesslogdatav3.HTTPAccessLogEntry, at time.Time, inner bool) call {
	common, req, resp := e.GetCommonProperties(), e.GetRequest(), e.GetResponse()
	connect := req.GetRequestMethod() == corev3.RequestMethod_CONNECT
	// Envoy logs an upgrade as it opens too (Utility::isUpgrade): a GET with Upgrade: CONNECT, which the bootstrap's
	// upgrade_configs accepts, answered 101 by an upstream an app runs. Its last entry counts it, so an app can't add to
	// its own calls, and no entry of it says a tunnel's open entry counted it.
	established := common.GetAccessLogType() == accesslogdatav3.AccessLogType_DownstreamTunnelSuccessfullyEstablished
	c := call{
		at:          at,
		stream:      common.GetStreamId(),
		partial:     common.GetAccessLogType() == accesslogdatav3.AccessLogType_DownstreamPeriodic || established && !connect,
		opened:      established && connect,
		protocol:    "http",
		statusClass: statusClassOf(resp.GetResponseCode()),
		flags:       flagsOf(common.GetResponseFlags()),
	}
	switch {
	case connect:
		c.protocol = "connect"
		// Envoy logs a tunnel as it opens exactly when it answers the CONNECT 200 (HeaderUtility::isConnectResponse),
		// and only a bootstrap that asks it to tags its entries: an Envoy from before it logged none, so its last entry
		// counts the call, or the rollout that brings the bootstrap would lose every tunnel it counts.
		_, tagged := common.GetCustomTags()[egresscontrol.OpenEntryTag]
		c.counted = tagged && !c.opened && resp.GetResponseCode().GetValue() == 200
	case req.GetScheme() == "https":
		c.protocol = "https"
	}
	if len(c.stream) > egressStreamID {
		c.stream = ""
	}
	c.source, _ = netip.ParseAddr(common.GetDownstreamDirectRemoteAddress().GetSocketAddress().GetAddress())
	if start := common.GetStartTime(); start != nil && start.IsValid() {
		c.started = start.AsTime()
	}
	c.destination, c.port = destinationOf(req.GetAuthority(), req.GetScheme(), connect)
	c.outcome = "allowed"
	check := common.GetMetadata().GetFilterMetadata()[extAuthz].GetFields()
	// The check's say first; with none, what Envoy did: UAEX (the check did not answer), or NR, a call it routed nowhere,
	// which the check never sees and never leaves, a port other than 80 and 443 among them (contracts/routes.md, the receiver; #2723).
	if check["outcome"].GetStringValue() == "refused" || len(check) == 0 && (slices.Contains(c.flags, "UAEX") || slices.Contains(c.flags, "NR")) {
		c.outcome = "refused"
	}
	c.reason = check["reason"].GetStringValue()
	if app, env := check["app"].GetStringValue(), check["env"].GetStringValue(); app != "" || env != "" {
		c.checked = &executor.Caller{App: app, Target: env}
	}
	if inner {
		c.protocol, c.stream, c.source, c.checked = "intercepted", "", netip.Addr{}, nil
		if check["outcome"].GetStringValue() != "allowed" { // ext_authz #2 let it on, or it went nowhere: the listener's 421 asks no check
			c.outcome = "refused"
		}
		var caller wrapperspb.StringValue
		if a := common.GetFilterStateObjects()[egresscontrol.CallerObject]; a != nil && anypb.UnmarshalTo(a, &caller, proto.UnmarshalOptions{}) == nil {
			if app, env, ok := strings.Cut(caller.GetValue(), "/"); ok {
				c.checked = &executor.Caller{App: app, Target: env}
			}
		}
	}
	c.credential = credentialRefusal(c, inner, len(check) > 0, resp.GetResponseCode().GetValue())
	return c
}

// credentialRefusal is why Canyon refused c while adding a route's credential, "" when it did not (contracts/routes.md,
// Interception, A record; #2692): the check's own reason; misdirected for an inner request the internal listener
// answered 421 itself, which asks no check; upstream_failed for an inner request Envoy flagged UF, DF or UH. A request
// on :3128 that is no CONNECT records no upstream_failed: whether its destination has a route is the policy's, which
// the receiver never reads.
func credentialRefusal(c call, inner, checked bool, code uint32) string {
	switch {
	case c.reason == "inject_requires_tls" || c.reason == "inject_unavailable" || c.reason == "misdirected":
		return c.reason
	case inner && !checked && code == 421:
		return "misdirected"
	case inner && slices.ContainsFunc(c.flags, func(f string) bool { return f == "UF" || f == "DF" || f == "UH" }):
		return "upstream_failed"
	}
	return ""
}

func statusClassOf(code *wrapperspb.UInt32Value) string {
	if n := code.GetValue(); n >= 100 && n <= 599 {
		return strconv.Itoa(int(n/100)) + "xx"
	}
	return "none"
}

// egressFlags are Envoy's own short names for its response flags.
var egressFlags = []struct {
	code string
	set  func(*accesslogdatav3.ResponseFlags) bool
}{
	{"DC", (*accesslogdatav3.ResponseFlags).GetDownstreamConnectionTermination},
	{"DF", (*accesslogdatav3.ResponseFlags).GetDnsResolutionFailure},
	{"DI", (*accesslogdatav3.ResponseFlags).GetDelayInjected},
	{"DPE", (*accesslogdatav3.ResponseFlags).GetDownstreamProtocolError},
	{"DR", (*accesslogdatav3.ResponseFlags).GetDownstreamRemoteReset},
	{"DT", (*accesslogdatav3.ResponseFlags).GetDurationTimeout},
	{"FI", (*accesslogdatav3.ResponseFlags).GetFaultInjected},
	{"IH", (*accesslogdatav3.ResponseFlags).GetInvalidEnvoyRequestHeaders},
	{"LH", (*accesslogdatav3.ResponseFlags).GetFailedLocalHealthcheck},
	{"LR", (*accesslogdatav3.ResponseFlags).GetLocalReset},
	{"NC", (*accesslogdatav3.ResponseFlags).GetNoClusterFound},
	{"NFCF", (*accesslogdatav3.ResponseFlags).GetNoFilterConfigFound},
	{"NR", (*accesslogdatav3.ResponseFlags).GetNoRouteFound},
	{"OM", (*accesslogdatav3.ResponseFlags).GetOverloadManager},
	{"RFCF", (*accesslogdatav3.ResponseFlags).GetResponseFromCacheFilter},
	{"RL", (*accesslogdatav3.ResponseFlags).GetRateLimited},
	{"RLSE", (*accesslogdatav3.ResponseFlags).GetRateLimitServiceError},
	{"SI", (*accesslogdatav3.ResponseFlags).GetStreamIdleTimeout},
	{"UAEX", func(f *accesslogdatav3.ResponseFlags) bool { return f.GetUnauthorizedDetails() != nil }},
	{"UC", (*accesslogdatav3.ResponseFlags).GetUpstreamConnectionTermination},
	{"UF", (*accesslogdatav3.ResponseFlags).GetUpstreamConnectionFailure},
	{"UH", (*accesslogdatav3.ResponseFlags).GetNoHealthyUpstream},
	{"UMSDR", (*accesslogdatav3.ResponseFlags).GetUpstreamMaxStreamDurationReached},
	{"UO", (*accesslogdatav3.ResponseFlags).GetUpstreamOverflow},
	{"UPE", (*accesslogdatav3.ResponseFlags).GetUpstreamProtocolError},
	{"UR", (*accesslogdatav3.ResponseFlags).GetUpstreamRemoteReset},
	{"URX", (*accesslogdatav3.ResponseFlags).GetUpstreamRetryLimitExceeded},
	{"UT", (*accesslogdatav3.ResponseFlags).GetUpstreamRequestTimeout},
}

// flagsOf is the entry's flags, sorted: egressFlags is in order.
func flagsOf(f *accesslogdatav3.ResponseFlags) []string {
	flags := []string{}
	for _, flag := range egressFlags {
		if flag.set(f) {
			flags = append(flags, flag.code)
		}
	}
	return flags
}

// destinationOf is the name dialled — a CONNECT's authority, an absolute-form request's — as the check
// canonicalises it, or a dotted IPv4 literal, and its port, whatever it is; invalid on 443 for anything
// else no host name is, so the ports of a name that is no host are one row (destination.Recorded).
func destinationOf(authority, scheme string, connect bool) (string, int) {
	port := 80
	if connect || scheme == "https" {
		port = 443
	}
	return destination.Recorded(authority, port)
}

// egressReceiver serves Envoy's access-log stream and writes what it receives every flush, one row per
// destination an app env calls (design/EGRESS.md §2.4). One goroutine, run, owns the batch, the tunnels'
// callers and the gaps not yet written.
type egressReceiver struct {
	accesslogv3.UnimplementedAccessLogServiceServer
	pool    *pgxpool.Pool
	callers func(context.Context, []string) ([]executor.Caller, error) // the executor's POST /callers
	log     *log.Logger
	now     func() time.Time
	every   time.Duration // between flushes
	wait    time.Duration // for one POST /callers
	calls   chan call
	open    chan struct{}           // a slot per ALS stream at once
	dropped atomic.Int64            // calls turned away, counted
	unread  atomic.Int64            // messages too large to read: their calls no one counted
	entry   atomic.Int64            // when the last entry arrived, Unix nanoseconds; 0 before any
	envoy   envoyLost               // what egress-control reported Envoy lost
	tunnels map[tunnel]streamCaller // run's alone
	gaps    map[string]egressGap    // run's alone: by reason, and a dropped and an envoy_dropped one no one counted apart, those no flush has written yet — at most six
	last    time.Time               // run's alone: the previous flush, where this one's gaps start
}

// envoyLost is what egress-control reported Envoy lost (POST /egress/dropped) until a flush takes it, and whether
// the last flush has begun, past which a report would never be written.
type envoyLost struct {
	mu        sync.Mutex
	calls     int64 // Envoy's own count
	uncounted bool  // entries lost that no one counted
	closed    bool
}

// add takes one report, calls nil for what no one counted; false once the last flush has begun, so egress-control
// keeps the report for the server after this one.
func (e *envoyLost) add(calls *int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
		return false
	case calls == nil:
		e.uncounted = true
	default:
		e.calls += *calls
	}
	return true
}

// take is what the reports said since the last take.
func (e *envoyLost) take() (calls int64, uncounted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	calls, uncounted, e.calls, e.uncounted = e.calls, e.uncounted, 0, false
	return calls, uncounted
}

// close refuses every report from now on: what add took is in the last flush's take.
func (e *envoyLost) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
}

// tunnel is one stream of one source: a stream id names a tunnel only with the address it came from.
type tunnel struct {
	source netip.Addr
	stream string
}

// streamCaller is a tunnel's caller as its periodic entry found it, while it lived.
type streamCaller struct {
	caller *executor.Caller
	seen   time.Time
}

// egressGap is what the records are missing over a span, and why (egress.schema.json, gap): calls is the
// count of calls lost, unless uncounted — the receiver down, a message too large to read, or what Envoy lost that
// it keeps no count of. app and env are an over_cap gap's alone.
type egressGap struct {
	from, to  time.Time
	reason    string
	calls     int64
	uncounted bool
	app, env  string
}

func newEgressReceiver(pool *pgxpool.Pool, callers func(context.Context, []string) ([]executor.Caller, error), logger *log.Logger) *egressReceiver {
	return &egressReceiver{pool: pool, callers: callers, log: logger, now: time.Now, every: egressFlushEvery, wait: egressCallersWait, calls: make(chan call, egressBuffer), open: make(chan struct{}, egressStreams),
		tunnels: map[tunnel]streamCaller{}, gaps: map[string]egressGap{}}
}

// newALSServer is the receiver's gRPC server: Envoy's token checked on every stream before anything else
// (contracts/routes.md, Egress control), messages of at most egressMessageBytes, egressStreams streams a
// connection, and keepalives that close a silent peer's connection. The caller serves it on a listener
// refuseAppPool wraps.
func newALSServer(r *egressReceiver, token string) *grpc.Server {
	want := []byte("Bearer " + token)
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(egressMessageBytes), grpc.MaxConcurrentStreams(egressStreams), grpc.WaitForHandlers(true),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute, Time: time.Minute, Timeout: 20 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second, PermitWithoutStream: true}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			md, _ := metadata.FromIncomingContext(ss.Context())
			got := md.Get("authorization")
			if len(got) != 1 || subtle.ConstantTimeCompare([]byte(got[0]), want) != 1 {
				return status.Error(codes.Unauthenticated, "the receiver takes Envoy's token alone")
			}
			return handler(srv, ss)
		}),
		grpc.UnaryInterceptor(func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
			return nil, status.Error(codes.Unimplemented, "the receiver serves StreamAccessLogs alone")
		}),
	)
	accesslogv3.RegisterAccessLogServiceServer(srv, r)
	return srv
}

// receiveEgress serves the receiver on ln, refusing the apps' pool, until ctx ends; then it stops taking
// streams, waits for every stream's handler to return, and only then ends run, which drains the queue
// and flushes it: no entry arrives after the last flush. done closes once that flush is done; failed
// carries an error serving, never the nil a stopped server returns.
func receiveEgress(ctx context.Context, ln net.Listener, apps netip.Prefix, r *egressReceiver, token string) (done <-chan struct{}, failed <-chan error) {
	srv, ran, errs := newALSServer(r, token), make(chan struct{}), make(chan error, 1)
	running, stop := context.WithCancel(context.WithoutCancel(ctx))
	go func() { r.run(running); close(ran) }()
	go func() {
		<-ctx.Done()
		srv.Stop() // waits for the handlers (grpc.WaitForHandlers)
		stop()
	}()
	go func() {
		if err := srv.Serve(refuseAppPool(ln, apps)); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errs <- err
		}
	}()
	return ran, errs
}

// StreamAccessLogs takes Envoy's entries; every one becomes a call before anything else reads it. A call
// the buffer has no room for, or an entry over egressEntryBytes, is counted, and a message too large to
// read is noted; its flush records each as a gap. Past egressStreams at once a stream is refused: Envoy
// retries it. Envoy names the log a stream carries in its first message: the interception listener's, InterceptLog,
// is inner requests; any other, :3128's calls.
func (r *egressReceiver) StreamAccessLogs(stream accesslogv3.AccessLogService_StreamAccessLogsServer) error {
	select {
	case r.open <- struct{}{}:
		defer func() { <-r.open }()
	default:
		return status.Error(codes.ResourceExhausted, "too many access-log streams")
	}
	inner := false
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&accesslogv3.StreamAccessLogsResponse{})
		}
		if status.Code(err) == codes.ResourceExhausted {
			r.unread.Add(1)
		}
		if err != nil {
			return err
		}
		at := r.now()
		r.entry.Store(at.UnixNano())
		if id := msg.GetIdentifier(); id != nil {
			inner = id.GetLogName() == egresscontrol.InterceptLog
		}
		for _, e := range msg.GetHttpLogs().GetLogEntry() {
			if proto.Size(e) > egressEntryBytes {
				r.dropped.Add(1)
				continue
			}
			select {
			case r.calls <- callOf(e, at, inner):
			default:
				r.dropped.Add(1)
			}
		}
	}
}

// streams is how many ALS streams are open, and lastEntry when the last entry arrived (zero before any):
// the admin's read of the setting (contracts/routes.md, Egress control).
func (r *egressReceiver) streams() int { return len(r.open) }

func (r *egressReceiver) lastEntry() time.Time {
	if n := r.entry.Load(); n != 0 {
		return time.Unix(0, n).UTC()
	}
	return time.Time{}
}

// run notes how long the receiver was down, ages rows out and flushes until ctx ends, then flushes what it
// holds once more, on a context of its own, before it returns. receiveEgress ends ctx only once no stream
// is left to add to the queue.
func (r *egressReceiver) run(ctx context.Context) {
	if err := r.boot(ctx); err != nil {
		r.log.Printf("egress: the receiver's last flush: %v", err)
	}
	if err := r.prune(ctx); err != nil {
		r.log.Printf("egress: ageing out: %v", err)
	}
	flush, prune := time.NewTicker(r.every), time.NewTicker(time.Minute)
	defer flush.Stop()
	defer prune.Stop()
	var batch []call
	take := func(c call) {
		if len(batch) >= cap(r.calls) { // a flush behind: the batch holds no more than the queue does
			r.dropped.Add(1)
			return
		}
		batch = append(batch, c)
	}
	for {
		select {
		case <-ctx.Done():
			for len(r.calls) > 0 {
				take(<-r.calls)
			}
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), egressLastFlush)
			defer cancel()
			r.wait = min(r.wait, egressLastCallers) // the write keeps most of the last flush's time
			r.envoy.close()
			if err := r.flush(last, batch); err != nil {
				r.log.Printf("egress: the last batch of %d calls not recorded: %v", len(batch), err)
			}
			return
		case c := <-r.calls:
			take(c)
		case <-flush.C:
			if err := r.flush(ctx, batch); err != nil {
				r.log.Printf("egress: a batch of %d calls not recorded, kept as a gap: %v", len(batch), err)
			}
			batch = nil
		case <-prune.C:
			if err := r.prune(ctx); err != nil {
				r.log.Printf("egress: ageing out: %v", err)
			}
			r.unrecorded(ctx)
		}
	}
}

// unrecorded warns that no call out is being recorded when no Envoy stream is open while egress is in use: the
// host's via is canyon, or an app env still runs under it (egress_envs; design/EGRESS.md §2.5). Off by design, as
// the laptop demo starts — via off and no env under canyon — no call goes through Envoy, and a warning a minute
// says nothing (#2697). A state that cannot be read warns, as egress in use does.
func (r *egressReceiver) unrecorded(ctx context.Context) {
	if r.streams() > 0 {
		return
	}
	var used bool
	err := r.pool.QueryRow(ctx, "SELECT coalesce((SELECT via FROM egress_settings) <> 'off', true) OR EXISTS (SELECT 1 FROM egress_envs)").Scan(&used)
	if err != nil {
		r.log.Printf("egress: whether any app's calls go through Envoy: %v", err)
	}
	if err != nil || used {
		r.log.Print("egress: no Envoy stream is open, so no call out is being recorded")
	}
}

// boot records the time since the receiver's last flush as receiver_down when it is past a flush and a
// half: Envoy had no one to send to, and no call of that time is counted. A clean stop flushed last, so
// its span starts there; a crash's starts at the flush before, whose batch since is lost with it.
func (r *egressReceiver) boot(ctx context.Context) error {
	r.last = r.now()
	var seen time.Time
	err := r.pool.QueryRow(ctx, "SELECT seen_at FROM egress_receiver").Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err == nil && r.last.Sub(seen) > r.every*3/2 {
		r.gap(egressGap{from: seen, to: r.last, reason: "receiver_down", uncounted: true})
	}
	return err
}

// gap adds g to the gaps not yet written, one per reason over the span of all of them — a dropped gap no
// one counted kept apart from a counted one, whose count it would lose; a span never ends before it
// starts, should the clock step back.
func (r *egressReceiver) gap(g egressGap) {
	g.to = maxTime(g.to, g.from)
	key := g.reason
	if g.uncounted {
		key += " uncounted"
	}
	if was, ok := r.gaps[key]; ok {
		g.from, g.to, g.calls = minTime(was.from, g.from), maxTime(was.to, g.to), was.calls+g.calls
	}
	r.gaps[key] = g
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// destKey is a row's key (design/EGRESS.md §2.4).
type destKey struct {
	app, env, destination string
	port                  int
}

// destSum is one key's calls in a flush: how many, the earliest and the latest entry, and the latest's result.
type destSum struct {
	calls                          int64
	first, last                    time.Time
	admitted                       time.Time // the latest call that went through, its outcome allowed; zero for none
	ended                          bool      // the result is an ended call's, not only an open tunnel's
	result                         time.Time // when the entry the result is from was taken
	outcome, protocol, statusClass string
	flags                          []string
	credential                     string    // the latest credential refusal's reason, "" for none
	credentialAt                   time.Time // when its entry was taken
}

// add takes c, counting it unless its open entry did. Its result is the key's last unless an earlier entry's is
// later, a tie going to the later in the batch, and an open tunnel's never replaces an ended call's: its call
// has no result yet. A periodic entry says the tunnel runs on, and so moves when the key was last seen alone.
func (s *destSum) add(c call) {
	if s.first.IsZero() || c.at.Before(s.first) {
		s.first = c.at
	}
	if c.at.After(s.last) {
		s.last = c.at
	}
	if ended := !c.opened && !c.partial; ended && !s.ended || ended == s.ended && !c.at.Before(s.result) {
		s.ended, s.result, s.outcome, s.protocol, s.statusClass, s.flags = ended, c.at, c.outcome, c.protocol, c.statusClass, c.flags
	}
	if c.outcome == "allowed" && !c.partial && c.at.After(s.admitted) {
		s.admitted = c.at
	}
	if c.credential != "" && !c.at.Before(s.credentialAt) {
		s.credential, s.credentialAt = c.credential, c.at
	}
	if !c.counted {
		s.calls++
	}
}

// flush attributes the batch's calls, sums them per destination in memory and writes each key once, with
// the gaps since the previous flush and the receiver's heartbeat, in one transaction. Each flush attributes
// its calls afresh: an address's caller is never remembered, only a tunnel's, by its source and stream.
// Nothing is lost without a gap: a call the buffer dropped, what Envoy lost, every call no one is named for, a
// call past its app env's cap and a batch that could not be written are each counted, and a gap that could not be
// written waits for the next flush.
func (r *egressReceiver) flush(ctx context.Context, batch []call) error {
	from, to := r.last, r.now()
	if from.IsZero() {
		from = to
	}
	r.last = maxTime(to, from)
	if n := r.dropped.Swap(0); n > 0 {
		r.gap(egressGap{from: from, to: to, reason: "dropped", calls: n})
	}
	if r.unread.Swap(0) > 0 {
		r.gap(egressGap{from: from, to: to, reason: "dropped", uncounted: true})
	}
	lost, uncounted := r.envoy.take()
	if lost > 0 {
		r.gap(egressGap{from: from, to: to, reason: "envoy_dropped", calls: lost})
	}
	if uncounted {
		r.gap(egressGap{from: from, to: to, reason: "envoy_dropped", uncounted: true})
	}
	named := r.byAddress(ctx, batch)
	var keys []destKey
	var calls, unattributed int64
	sums := map[destKey]*destSum{}
	var refused []destKey // not_allowed, by destination, port 0: an egress request is of a name
	refusals := map[destKey]*destSum{}
	for i, c := range batch {
		caller := named[i]
		if c.checked != nil {
			caller = nil
			if inContract(*c.checked) {
				caller = c.checked
			}
		}
		if c.stream != "" {
			t := tunnel{c.source, c.stream}
			if s, ok := r.tunnels[t]; ok && s.caller != nil {
				caller = s.caller
			}
			if c.partial {
				if _, ok := r.tunnels[t]; ok || len(r.tunnels) < egressTunnels {
					r.tunnels[t] = streamCaller{caller: caller, seen: c.at}
				}
			} else {
				delete(r.tunnels, t)
			}
		}
		if c.partial && !c.counted { // one of a tunnel its open entry counted goes on, for last_seen_at
			continue
		}
		if !c.counted {
			calls++
		}
		if caller == nil {
			if !c.counted {
				unattributed++
			}
			continue
		}
		k := destKey{caller.App, caller.Target, c.destination, c.port}
		if _, ok := sums[k]; !ok {
			sums[k] = &destSum{}
			keys = append(keys, k)
		}
		sums[k].add(c)
		if c.reason == "not_allowed" && c.destination != "invalid" && !c.counted {
			k.port = 0
			if _, ok := refusals[k]; !ok {
				refusals[k] = &destSum{}
				refused = append(refused, k)
			}
			refusals[k].add(c)
		}
	}
	for t, s := range r.tunnels {
		if to.Sub(s.seen) > egressStreamMemory {
			delete(r.tunnels, t)
		}
	}
	span := func(g egressGap) egressGap { g.from, g.to = from, maxTime(to, from); return g }
	var fresh []egressGap // this batch's, written with it or not at all: a batch not written is not_recorded instead
	if unattributed > 0 {
		fresh = append(fresh, span(egressGap{reason: "unattributed", calls: unattributed}))
	}
	err := r.write(ctx, keys, sums, refused, refusals, fresh, span, to)
	if err != nil {
		if calls > 0 {
			r.gap(egressGap{from: from, to: to, reason: "not_recorded", calls: calls})
		}
		if gerr := r.write(ctx, nil, nil, nil, nil, nil, span, to); gerr != nil {
			r.log.Printf("egress: gaps kept for the next flush: %v", gerr)
		}
	}
	return err
}

// write upserts each key's row — calls added, first seen the earlier, last seen the later, and the last
// result the newer ended call's, or an open tunnel's on the row it creates, which holds at least its one call —
// counts each key past its app env's cap on that app env's over_cap gap, which holds at least one call too, and
// writes the gaps not yet written, fresh and the heartbeat, and files the refused keys' egress requests (fileRequests),
// in one transaction; the gaps are forgotten once it commits. span stamps an over_cap gap with the flush's.
//
// The cap holds with more than one writer — a second replica, or a receiver restarted while the last
// flush of the one before still commits: write takes each app env's advisory lock (egressLock) before it
// reads that app env's rows, so two flushes never both admit the 500th, and it takes the locks, and
// upserts the rows, in key order, so two flushes never wait on each other in a cycle.
func (r *egressReceiver) write(ctx context.Context, keys []destKey, sums map[destKey]*destSum, refused []destKey, refusals map[destKey]*destSum, fresh []egressGap, span func(egressGap) egressGap, at time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(refused) > 0 { // an audit row's append lock, before any other, the order every appender keeps
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
	}
	var locks []string
	for _, k := range keys {
		locks = append(locks, egressLock(k.app, k.env))
	}
	slices.Sort(locks)
	for _, l := range slices.Compact(locks) {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", l); err != nil {
			return err
		}
	}
	// the caps, an admin's (egress_settings; #2691), read in this transaction: a read that fails fails the flush, never
	// a cap that means none
	var destinations, pending int
	if err := tx.QueryRow(ctx, "SELECT cap_destinations, cap_pending FROM egress_settings").Scan(&destinations, &pending); err != nil {
		return err
	}
	admitted, over, err := r.admit(ctx, tx, keys, sums, destinations)
	if err != nil {
		return err
	}
	slices.SortFunc(admitted, func(x, y destKey) int {
		return cmp.Or(strings.Compare(x.app, y.app), strings.Compare(x.env, y.env), strings.Compare(x.destination, y.destination), x.port-y.port)
	})
	b := &pgx.Batch{}
	for _, k := range admitted {
		s := sums[k]
		b.Queue(`INSERT INTO egress_destinations AS d (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags, admitted_at,
				last_credential_reason, last_credential_refused_at)
			VALUES ($1, $2, $3, $4, $5, $6, greatest($7::bigint, 1), $8, $9, $10, $11, $12, $15::text, $16::timestamptz)
			ON CONFLICT (app, env, destination, port) DO UPDATE SET calls = d.calls + $7::bigint,
				first_seen_at = least(d.first_seen_at, EXCLUDED.first_seen_at), last_seen_at = greatest(d.last_seen_at, EXCLUDED.last_seen_at),
				last_outcome = CASE WHEN $13::boolean AND $14::timestamptz >= d.last_seen_at THEN EXCLUDED.last_outcome ELSE d.last_outcome END,
				last_status_class = CASE WHEN $13::boolean AND $14::timestamptz >= d.last_seen_at THEN EXCLUDED.last_status_class ELSE d.last_status_class END,
				last_protocol = CASE WHEN $13::boolean AND $14::timestamptz >= d.last_seen_at THEN EXCLUDED.last_protocol ELSE d.last_protocol END,
				last_response_flags = CASE WHEN $13::boolean AND $14::timestamptz >= d.last_seen_at THEN EXCLUDED.last_response_flags ELSE d.last_response_flags END,
				admitted_at = greatest(d.admitted_at, EXCLUDED.admitted_at), -- a refused call never sets it: known[] holds admitted names alone
				last_credential_reason = CASE WHEN $16::timestamptz >= coalesce(d.last_credential_refused_at, '-infinity') THEN $15::text ELSE d.last_credential_reason END,
				last_credential_refused_at = CASE WHEN $16::timestamptz >= coalesce(d.last_credential_refused_at, '-infinity') THEN $16::timestamptz ELSE d.last_credential_refused_at END`,
			k.app, k.env, k.destination, k.port, s.first, s.last, s.calls, s.outcome, s.statusClass, s.protocol, s.flags, nullTime(s.admitted), s.ended, s.result,
			nullable(s.credential != "", s.credential), nullTime(s.credentialAt))
	}
	for _, g := range over { // one row an app env, whatever the time between its bursts
		g = span(g)
		b.Queue(`INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES ($1, $2, 'over_cap', greatest($3::bigint, 1), $4, $5)
			ON CONFLICT (app, env) WHERE reason = 'over_cap' DO UPDATE SET from_at = least(egress_gaps.from_at, EXCLUDED.from_at),
				to_at = greatest(egress_gaps.to_at, EXCLUDED.to_at), calls = egress_gaps.calls + $3::bigint`, g.from, g.to, g.calls, g.app, g.env)
	}
	for _, g := range append(slices.Collect(maps.Values(r.gaps)), fresh...) {
		var calls *int64
		if !g.uncounted {
			calls = &g.calls
		}
		// a gap that continues the last one written of its reason and countedness — starting where that row
		// ends — extends that one row, never two that end alike
		b.Queue(`WITH target AS (
				SELECT id FROM egress_gaps WHERE reason = $3 AND to_at = $1 AND app IS NULL AND env IS NULL AND (calls IS NULL) = ($4::bigint IS NULL)
				ORDER BY to_at DESC, id DESC LIMIT 1 FOR UPDATE),
			extended AS (UPDATE egress_gaps SET to_at = $2, calls = egress_gaps.calls + $4 FROM target WHERE egress_gaps.id = target.id RETURNING 1)
			INSERT INTO egress_gaps (from_at, to_at, reason, calls) SELECT $1, $2, $3, $4 WHERE NOT EXISTS (SELECT 1 FROM extended)`,
			g.from, g.to, g.reason, calls)
	}
	b.Queue("INSERT INTO egress_receiver (seen_at) VALUES ($1) ON CONFLICT (one) DO UPDATE SET seen_at = EXCLUDED.seen_at", at)
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	if err := fileRequests(ctx, tx, refused, refusals, pending); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	clear(r.gaps)
	return nil
}

// egressLock names an app env's advisory lock, which a flush holds while it admits and writes that app
// env's rows.
func egressLock(app, env string) string { return "egress_destinations " + app + "/" + env }

// appEnv is one app env.
type appEnv struct{ app, env string }

// admit answers the keys that get a row, in the batch's order, and an over_cap gap per app env counting
// the calls of the rest: a key already a row, invalid, or new while its app env holds fewer than destinations
// rows, the admin's cap. Each app env's rows are read once a flush; nothing is kept across flushes. A cap lowered
// below the rows an env holds deletes none: they go on, and every new key is over_cap until the env is under it.
func (r *egressReceiver) admit(ctx context.Context, tx pgx.Tx, keys []destKey, sums map[destKey]*destSum, destinations int) ([]destKey, []egressGap, error) {
	held := map[appEnv]map[destKey]bool{}
	var admitted []destKey
	var over []egressGap
	at := map[appEnv]int{} // an app env's index in over
	for _, k := range keys {
		ae := appEnv{k.app, k.env}
		rows, ok := held[ae]
		if !ok {
			q, err := tx.Query(ctx, "SELECT destination, port FROM egress_destinations WHERE app = $1 AND env = $2 AND destination <> 'invalid'", k.app, k.env)
			if err != nil {
				return nil, nil, err
			}
			rows = map[destKey]bool{}
			var dest string
			var port int
			if _, err := pgx.ForEachRow(q, []any{&dest, &port}, func() error { rows[destKey{k.app, k.env, dest, port}] = true; return nil }); err != nil {
				return nil, nil, err
			}
			held[ae] = rows
		}
		if k.destination == "invalid" || rows[k] {
			admitted = append(admitted, k)
			continue
		}
		if len(rows) < destinations {
			rows[k] = true
			admitted = append(admitted, k)
			continue
		}
		i, ok := at[ae]
		if !ok {
			i, at[ae] = len(over), len(over)
			over = append(over, egressGap{reason: "over_cap", app: k.app, env: k.env})
		}
		over[i].calls += sums[k].calls
	}
	return admitted, over, nil
}

// egressActor is Canyon, filing an egress request.
var egressActor = events.Actor{Kind: "canyon", Subject: "egress", Display: "Canyon"}

// fileRequests counts each destination's not_allowed calls on its app env's egress request (contracts/routes.md, The
// check): the first files one, pending, by Canyon, with one egress.request audit row — unless an entry of the
// env's allowlist allows the destination now, the call from before it was allowed, or the env holds
// pending pending already, the admin's cap; each later one counts on it, dismissed or not.
func fileRequests(ctx context.Context, tx pgx.Tx, refused []destKey, refusals map[destKey]*destSum, pending int) error {
	for _, k := range refused {
		s := refusals[k]
		tag, err := tx.Exec(ctx, `UPDATE egress_requests SET refusals = refusals + $4, first_refused_at = least(first_refused_at, $5),
			last_refused_at = greatest(last_refused_at, $6) WHERE project_id = $1 AND env = $2 AND destination = $3`, k.app, k.env, k.destination, s.calls, s.first, s.last)
		if err != nil || tag.RowsAffected() > 0 {
			if err != nil {
				return err
			}
			continue
		}
		rows, err := tx.Query(ctx, "SELECT entry FROM egress_allowed WHERE project_id = $1 AND env = $2", k.app, k.env)
		if err != nil {
			return err
		}
		entries, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if slices.ContainsFunc(entries, func(e string) bool { return destination.Allows(e, k.destination) }) {
			continue
		}
		tag, err = tx.Exec(ctx, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals)
			SELECT $1, $2, $3, 'pending', $4, $5, $6 WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)
				AND (SELECT count(*) FROM egress_requests WHERE project_id = $1 AND env = $2 AND state = 'pending') < $7`,
			k.app, k.env, k.destination, s.first, s.last, s.calls, pending)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err := events.AuditTx(ctx, tx, egressActor, "egress.request", k.app, "", map[string]string{"env": k.env, "destination": k.destination}); err != nil {
				return err
			}
		}
	}
	return nil
}

// nullTime is t, or NULL when it is zero.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// nullable is s, or NULL when set is false.
func nullable(set bool, s string) *string {
	if !set {
		return nil
	}
	return &s
}

// egressApp is egress.schema.json's app: an id Canyon gives an app.
var egressApp = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// inContract is whether c names an app id and a target the records take; a caller out of contract is no one's.
func inContract(c executor.Caller) bool {
	return egressApp.MatchString(c.App) && (c.Target == "preview" || c.Target == "prod")
}

// byAddress attributes each call to whoever holds the address it came from (design/EGRESS.md §2.3, by
// address): every stream reaches the receiver on Canyon's own listener. A call the check named a caller for
// is that caller's, and flush never asks about it here. It asks the executor about the batch's other IPv4 addresses, at most egressAddressesFlush, once each, each call
// bounded by r.wait. An address it names twice, or did not ask about, is no one's: the caller is unknown,
// never one of two picked (contracts/executor.md, POST /callers). An address the executor could not answer
// for, in time or at all, or answered with an app id or a target out of contract or no since, is no one's too:
// records are evidence, not a gate, and flush counts every call of no one's as a gap. A holder is named for a
// call only when the call started at or after its since: a call an app made before it stopped, read once
// another app holds its address, is no one's, and so is a call whose start Envoy did not say (#2265).
func (r *egressReceiver) byAddress(ctx context.Context, batch []call) []*executor.Caller {
	var asked []string
	seen := map[netip.Addr]bool{}
	for _, c := range batch {
		if c.checked == nil && c.source.Is4() && !seen[c.source] && len(asked) < egressAddressesFlush {
			seen[c.source] = true
			asked = append(asked, c.source.String())
		}
	}
	held, twice := map[netip.Addr]*executor.Caller{}, map[netip.Addr]bool{}
	for chunk := range slices.Chunk(asked, egressCallersAsked) {
		cctx, cancel := context.WithTimeout(ctx, r.wait)
		answer, err := r.callers(cctx, chunk)
		cancel()
		if err != nil {
			r.log.Printf("egress: %d addresses unattributed: %v", len(chunk), err)
			continue
		}
		for _, c := range answer {
			addr, err := netip.ParseAddr(c.Address)
			if err != nil || !seen[addr] || twice[addr] {
				continue
			}
			if !inContract(c) || c.Since.IsZero() { // out of contract: no one's, and the flush goes on
				delete(held, addr)
				twice[addr] = true
				continue
			}
			if _, ok := held[addr]; ok {
				delete(held, addr)
				twice[addr] = true
				continue
			}
			held[addr] = &c
		}
	}
	named := make([]*executor.Caller, len(batch))
	for i, c := range batch {
		if h := held[c.source]; h != nil && !c.started.IsZero() && !c.started.Before(h.Since) {
			named[i] = h
		}
	}
	return named
}

// prune deletes each destination unseen for egressKept and each gap that ended that long ago
// (design/EGRESS.md §2.4, Age). It skips a row a flush holds, which that flush sees again or the next prune
// takes, so a prune never waits on a flush and never in a cycle with one (#2233).
func (r *egressReceiver) prune(ctx context.Context) error {
	cutoff := r.now().Add(-egressKept)
	if _, err := r.pool.Exec(ctx, `DELETE FROM egress_destinations WHERE (app, env, destination, port) IN (
		SELECT app, env, destination, port FROM egress_destinations WHERE last_seen_at < $1 FOR UPDATE SKIP LOCKED)`, cutoff); err != nil {
		return err
	}
	_, err := r.pool.Exec(ctx, "DELETE FROM egress_gaps WHERE id IN (SELECT id FROM egress_gaps WHERE to_at < $1 FOR UPDATE SKIP LOCKED)", cutoff)
	return err
}

// egressProxy is executor.schema.json's egress.proxy: Envoy's listener, http://<host>:3128.
var egressProxy = regexp.MustCompile(`^http://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?:3128$`)

// egressProxyOf is CANYON_EGRESS_PROXY, which a /run under via canyon names (contracts/executor.md, Egress):
// "" when unset, and refused when it is not http://<host>:3128.
func egressProxyOf(s string) (string, error) {
	if s != "" && !egressProxy.MatchString(s) {
		return "", fmt.Errorf("CANYON_EGRESS_PROXY: %q is not http://<host>:3128, Canyon's Envoy", s)
	}
	return s, nil
}

// listenReceiver listens on the receiver's address, IPv4 alone: Go's "tcp" on 0.0.0.0 is a dual-stack socket,
// which an IPv6 peer would reach (contracts/routes.md, Egress control).
func listenReceiver(addr string) (net.Listener, error) {
	return net.Listen("tcp4", addr)
}

// egressToken is a CANYON_EGRESS_RECEIVER_TOKEN: at least 32 characters a gRPC metadata value carries as
// they are, as openssl rand -hex 32 writes one.
var egressToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{32,512}$`)

// egressReceiverOn says whether the server takes Envoy's access logs at addr: never when addr is unset.
// addr is egressAddrOf's, and every stream carries token, which Envoy sends (newALSServer).
func egressReceiverOn(addr string, kube bool, apps netip.Prefix, token string) (bool, error) {
	if addr == "" {
		return false, nil
	}
	if err := egressAddrOf("CANYON_EGRESS_RECEIVER_ADDR", "the receiver", "7781", addr, kube, apps); err != nil {
		return false, err
	}
	if !egressToken.MatchString(token) {
		return false, errors.New("CANYON_EGRESS_RECEIVER_TOKEN: set it to at least 32 random characters, as openssl rand -hex 32 writes, and give Envoy the same")
	}
	return true, nil
}

// egressAddrOf checks an address egress's gateway reaches the server at — the receiver's, the policy's: an IPv4
// address and a port — the server's own address on canyon-egress-ctl, the one network Envoy shares with it —
// never every interface, loopback, a name, IPv6 or an address of the apps' pool, so it listens nowhere an app
// or the host reaches; CANYON_APPS_SUBNET must be set for that to be known. On Kubernetes addr is 0.0.0.0 and a
// port, and no pool is needed: no manifest knows the pod's address, and the server's NetworkPolicy admits the
// gateway's pods alone (design/EGRESS.md §2.2). Either way the server listens IPv4 alone.
func egressAddrOf(name, what, port, addr string, kube bool, apps netip.Prefix) error {
	ap, err := netip.ParseAddrPort(addr)
	ip := ap.Addr()
	switch {
	case kube && (err != nil || !ip.Is4() || !ip.IsUnspecified() || ap.Port() == 0):
		return fmt.Errorf("%s: %q is not 0.0.0.0 and a port, as 0.0.0.0:%s: on Kubernetes never another address, a name or IPv6", name, addr, port)
	case kube:
	case !apps.IsValid():
		return fmt.Errorf("%s: CANYON_APPS_SUBNET is unset, so %s cannot refuse the apps' pool; set it", name, what)
	case err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || apps.Contains(ip) || ap.Port() == 0:
		return fmt.Errorf("%s: %q is not the server's IPv4 address on canyon-egress-ctl and a port, as 10.215.0.2:%s: never 0.0.0.0, loopback, a name or an address of the apps' pool", name, addr, port)
	}
	return nil
}
