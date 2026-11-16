package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// innerEntry is one request inside a tunnel Envoy ended, as the interception listener logs it (design/EGRESS.md §2.4,
// §4.2): from Envoy's internal address, never an app's, its caller the filter-state object canyon.caller, caller
// "" for none, and ext_authz #2's answer in the check's namespace.
func innerEntry(authority string, status uint32, caller, outcome, reason string) *accesslogdatav3.HTTPAccessLogEntry {
	e := checked(entry("", corev3.RequestMethod_GET, "https", authority, status), "", "", outcome, reason)
	e.CommonProperties.DownstreamDirectRemoteAddress = &corev3.Address{Address: &corev3.Address_EnvoyInternalAddress{EnvoyInternalAddress: &corev3.EnvoyInternalAddress{
		AddressNameSpecifier: &corev3.EnvoyInternalAddress_ServerListenerName{ServerListenerName: egresscontrol.InterceptListener}}}}
	if caller != "" {
		withState(e, wrapperspb.String(caller))
	}
	return e
}

// withState sets e's canyon.caller to v, whatever it is.
func withState(e *accesslogdatav3.HTTPAccessLogEntry, v *wrapperspb.StringValue) *accesslogdatav3.HTTPAccessLogEntry {
	a, _ := anypb.New(v)
	e.CommonProperties.FilterStateObjects = map[string]*anypb.Any{egresscontrol.CallerObject: a}
	return e
}

// streamAs streams the entries as Envoy streams the log logName, naming it in the first message alone, as Envoy does,
// and returns once the receiver has taken every one.
func streamAs(t *testing.T, ctx context.Context, c accesslogv3.AccessLogServiceClient, logName string, entries ...*accesslogdatav3.HTTPAccessLogEntry) {
	t.Helper()
	stream, err := c.StreamAccessLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		msg := &accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{
			LogEntry: []*accesslogdatav3.HTTPAccessLogEntry{e}}}}
		if i == 0 {
			msg.Identifier = &accesslogv3.StreamAccessLogsMessage_Identifier{Node: &corev3.Node{Id: "canyon-egress"}, LogName: logName}
		}
		if err := stream.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
}

// An entry of the interception listener's log, which its stream names in its first message alone, is an inner
// request: intercepted, of no address and no tunnel of its own, its caller canyon.caller alone — never the check's
// app and env — and with none, or one that is no <app>/<env> string, no one's; allowed only when ext_authz #2 says so,
// so the listener's own 421 to a misdirected authority, which asks no check, is refused. canyon.caller on another
// log's entry names no one: :3128's calls are the check's and the address's, as ever (contracts/routes.md, the
// receiver).
func TestAnInnerEntryIsItsTunnelsCallers(t *testing.T) {
	r := newEgressReceiver(nil, nil, log.New(io.Discard, "", 0))
	c := alsClient(t, r, "10.213.0.0/16", "10.215.0.3")
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+egressTestToken)
	named := checked(innerEntry("api.stripe.com", 200, "claims-portal/prod", "allowed", ""), "intake-bot", "prod", "allowed", "")
	named.CommonProperties.StreamId = "8121937455563917113"
	notString, _ := anypb.New(wrapperspb.Bytes([]byte("claims-portal/prod")))
	bytesState := innerEntry("api.stripe.com", 200, "", "allowed", "")
	bytesState.CommonProperties.FilterStateObjects = map[string]*anypb.Any{egresscontrol.CallerObject: notString}
	misdirected := innerEntry("flood.example.com", 421, "claims-portal/prod", "", "")
	misdirected.CommonProperties.Metadata = nil // the listener's catch-all answers it, ext_authz #2 off
	streamAs(t, ctx, c, egresscontrol.InterceptLog, named,
		innerEntry("api.stripe.com", 403, "", "refused", "unknown_caller"),
		innerEntry("api.stripe.com", 200, "claims-portal", "allowed", ""),
		bytesState, misdirected,
		checked(innerEntry("api.stripe.com", 200, "", "allowed", ""), "intake-bot", "prod", "allowed", ""))
	outer := withState(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 200), wrapperspb.String("claims-portal/prod"))
	streamAs(t, ctx, c, "egress", outer)
	streamAs(t, ctx, c, "", withState(entry("172.28.3.11", corev3.RequestMethod_GET, "https", "api.openai.com", 200), wrapperspb.String("claims-portal/prod")))
	var got []call
	for len(r.calls) > 0 {
		got = append(got, <-r.calls)
	}
	if len(got) != 8 {
		t.Fatalf("%d calls taken, want 8", len(got))
	}
	want := executor.Caller{App: "claims-portal", Target: "prod"}
	if c := got[0]; c.protocol != "intercepted" || c.checked == nil || *c.checked != want || c.source.IsValid() || c.stream != "" ||
		c.destination != "api.stripe.com" || c.port != 443 || c.outcome != "allowed" {
		t.Errorf("an inner request: %+v, want intercepted, of api.stripe.com:443, its caller canyon.caller's claims-portal/prod, no address and no stream", c)
	}
	for i, what := range map[int]string{1: "no canyon.caller", 2: "a caller that is no <app>/<env>", 3: "a canyon.caller that is no string",
		5: "no canyon.caller and the check's app and env"} {
		if c := got[i]; c.protocol != "intercepted" || c.checked != nil || c.source.IsValid() {
			t.Errorf("an inner request with %s: %+v, want intercepted and no one's", what, c)
		}
	}
	if got[1].outcome != "refused" || got[1].reason != "unknown_caller" {
		t.Errorf("an inner request ext_authz #2 refused: %+v, want refused unknown_caller", got[1])
	}
	if c := got[4]; c.protocol != "intercepted" || c.checked == nil || *c.checked != want || c.destination != "flood.example.com" || c.outcome != "refused" {
		t.Errorf("the listener's 421 to a misdirected authority: %+v, want intercepted, claims-portal/prod's, of the name asked for, refused", c)
	}
	for i, c := range got[6:] {
		if c.protocol == "intercepted" || c.checked != nil || !c.source.IsValid() {
			t.Errorf("%d: :3128's call carrying canyon.caller: %+v, want it the address's, never intercepted", i, c)
		}
	}
}

// A flush holding an intercepted row commits (#2141's review): 0084's CHECK took connect, http and https alone, and
// the flush writes every app's rows of a batch in one transaction, so one row it refused would lose them all. A
// flush's inner request is its caller's row with last_protocol intercepted, beside another app's call of the same
// flush, and an inner request no caller is named for is unattributed (design/EGRESS.md §2.4).
func TestAFlushWithAnInterceptedRowCommits(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.4.11", App: "intake-bot", Target: "prod", Kind: "web"})
	streamAs(t, f.call, f.client, "egress", entry("172.28.4.11", corev3.RequestMethod_CONNECT, "", "api.anthropic.com:443", 200))
	streamAs(t, f.call, f.client, egresscontrol.InterceptLog, innerEntry("api.stripe.com", 201, "claims-portal/prod", "allowed", ""),
		innerEntry("api.stripe.com", 403, "", "refused", "unknown_caller"))
	var batch []call
	for len(f.r.calls) > 0 {
		batch = append(batch, <-f.r.calls)
	}
	if err := f.r.flush(f.ctx, batch); err != nil {
		t.Fatalf("a flush holding an intercepted row: %v", err)
	}
	rows := strings.Join(f.rowsOf(t, "(SELECT app, env, destination, port, calls, last_outcome, last_status_class, last_protocol FROM egress_destinations ORDER BY 1)"), "\n")
	want := `{"app":"claims-portal","env":"prod","destination":"api.stripe.com","port":443,"calls":1,"last_outcome":"allowed","last_status_class":"2xx","last_protocol":"intercepted"}` + "\n" +
		`{"app":"intake-bot","env":"prod","destination":"api.anthropic.com","port":443,"calls":1,"last_outcome":"allowed","last_status_class":"2xx","last_protocol":"connect"}`
	if rows != want {
		t.Errorf("rows:\n%s\nwant:\n%s", rows, want)
	}
	if n := count(t, f.ctx, f.pool, "SELECT coalesce(sum(calls), 0) FROM egress_gaps WHERE reason = 'unattributed'"); n != 1 {
		t.Errorf("%d unattributed calls, want the inner request no caller is named for", n)
	}
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_gaps WHERE reason = 'not_recorded'"); n != 0 {
		t.Errorf("%d not_recorded gaps, want none: the batch committed", n)
	}
}

// A call Canyon refused while adding a route's credential says why (contracts/routes.md, Interception, A record;
// #2692): the check's inject_requires_tls, inject_unavailable or misdirected, on :3128 or inside a tunnel; misdirected
// for the internal listener's own 421, which asks no check; upstream_failed for an inner request Envoy flagged UF, DF
// or UH. Every other call says nothing: a refusal for another reason, an upstream's own 421 that ext_authz #2 let on,
// and a request on :3128 that is no CONNECT flagged UF, whose destination's route the receiver cannot know.
func TestACredentialRefusalSaysWhy(t *testing.T) {
	flagged := func(e *accesslogdatav3.HTTPAccessLogEntry, f *accesslogdatav3.ResponseFlags) *accesslogdatav3.HTTPAccessLogEntry {
		e.CommonProperties.ResponseFlags = f
		return e
	}
	own421 := innerEntry("flood.example.com", 421, "claims-portal/prod", "", "")
	own421.CommonProperties.Metadata = nil
	for _, c := range []struct {
		what  string
		e     *accesslogdatav3.HTTPAccessLogEntry
		inner bool
		want  string
	}{
		{"a plain http:// request to a destination with a route", checked(entry("172.28.3.11", corev3.RequestMethod_GET, "http", "api.stripe.com", 403), "claims-portal", "prod", "refused", "inject_requires_tls"), false, "inject_requires_tls"},
		{"an absolute-form request whose value could not be read", checked(entry("172.28.3.11", corev3.RequestMethod_GET, "https", "api.stripe.com", 502), "claims-portal", "prod", "refused", "inject_unavailable"), false, "inject_unavailable"},
		{"an inner request whose value could not be read", innerEntry("api.stripe.com", 502, "claims-portal/prod", "refused", "inject_unavailable"), true, "inject_unavailable"},
		{"an inner request ext_authz #2 found misdirected", innerEntry("flood.example.com", 421, "claims-portal/prod", "refused", "misdirected"), true, "misdirected"},
		{"the internal listener's own 421", own421, true, "misdirected"},
		{"an inner request whose upstream failed", flagged(innerEntry("api.stripe.com", 502, "claims-portal/prod", "allowed", ""), &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}), true, "upstream_failed"},
		{"an inner request whose name did not resolve", flagged(innerEntry("api.stripe.com", 503, "claims-portal/prod", "allowed", ""), &accesslogdatav3.ResponseFlags{DnsResolutionFailure: true}), true, "upstream_failed"},
		{"an inner request with no healthy upstream", flagged(innerEntry("api.stripe.com", 503, "claims-portal/prod", "allowed", ""), &accesslogdatav3.ResponseFlags{NoHealthyUpstream: true}), true, "upstream_failed"},
		{"a refusal of no credential's", checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 403), "claims-portal", "prod", "refused", "not_allowed"), false, ""},
		{"an upstream's own 421, ext_authz #2 letting it on", innerEntry("api.stripe.com", 421, "claims-portal/prod", "allowed", ""), true, ""},
		{"a request on :3128 that is no CONNECT, flagged UF", flagged(checked(entry("172.28.3.11", corev3.RequestMethod_GET, "https", "api.stripe.com", 502), "claims-portal", "prod", "allowed", ""), &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}), false, ""},
		{"a tunnel whose upstream failed", flagged(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 503), &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}), false, ""},
	} {
		if got := callOf(c.e, time.Now(), c.inner).credential; got != c.want {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

// A flush keeps each row's latest credential refusal (#2692): the latest of its batch, a later flush's over an earlier
// one, never an earlier one's over a later, and a flush with none leaves it as it is, while the row's last call moves on.
func TestAFlushKeepsTheLatestCredentialRefusal(t *testing.T) {
	f := newEgressFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	inner := func(e *accesslogdatav3.HTTPAccessLogEntry, ago time.Duration) call {
		return callOf(e, at.Add(-ago), true)
	}
	refused := func(reason string, ago time.Duration) call {
		return inner(innerEntry("api.stripe.com", 502, "claims-portal/prod", "refused", reason), ago)
	}
	ok := inner(innerEntry("api.stripe.com", 200, "claims-portal/prod", "allowed", ""), 0)
	read := func() string {
		t.Helper()
		return strings.Join(f.rowsOf(t, "(SELECT last_credential_reason, last_credential_refused_at = '"+at.Format(time.RFC3339)+
			"'::timestamptz - interval '3 minutes' AS three, last_status_class FROM egress_destinations)"), "\n")
	}
	for _, step := range []struct {
		what  string
		batch []call
		want  string
	}{
		{"the batch's latest", []call{refused("inject_unavailable", 3*time.Minute), refused("misdirected", 5*time.Minute)}, `{"last_credential_reason":"inject_unavailable","three":true,"last_status_class":"5xx"}`},
		{"an earlier one, later", []call{refused("misdirected", 4*time.Minute)}, `{"last_credential_reason":"inject_unavailable","three":true,"last_status_class":"5xx"}`},
		{"none, the row's last call moving on", []call{ok}, `{"last_credential_reason":"inject_unavailable","three":true,"last_status_class":"2xx"}`},
		{"a later one", []call{refused("inject_requires_tls", time.Minute)}, `{"last_credential_reason":"inject_requires_tls","three":false,"last_status_class":"2xx"}`},
	} {
		if err := f.r.flush(f.ctx, step.batch); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if got := read(); got != step.want {
			t.Errorf("%s: %s, want %s", step.what, got, step.want)
		}
	}
}
