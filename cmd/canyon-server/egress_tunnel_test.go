package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

const (
	tunnelOpens  = accesslogdatav3.AccessLogType_DownstreamTunnelSuccessfullyEstablished
	tunnelGoesOn = accesslogdatav3.AccessLogType_DownstreamPeriodic
	tunnelEnds   = accesslogdatav3.AccessLogType_DownstreamEnd
)

// tagged is e as the bootstrap's Envoy logs it, with the tag every one of its entries carries.
func tagged(e *accesslogdatav3.HTTPAccessLogEntry) *accesslogdatav3.HTTPAccessLogEntry {
	e.CommonProperties.CustomTags = map[string]string{egresscontrol.OpenEntryTag: "1"}
	return e
}

// tunnelEntry is one entry of the tunnel stream from source to authority, as the bootstrap's Envoy logs it: the
// CONNECT answered 200, and a side closing on its last.
func tunnelEntry(kind accesslogdatav3.AccessLogType, source, authority, stream string) *accesslogdatav3.HTTPAccessLogEntry {
	e := tagged(entry(source, corev3.RequestMethod_CONNECT, "", authority, 200))
	e.CommonProperties.AccessLogType, e.CommonProperties.StreamId = kind, stream
	if kind == tunnelEnds {
		e.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{DownstreamConnectionTermination: true}
	}
	return e
}

// tunnelRow is the one row of destination, as a reader of the table sees what a tunnel writes.
func (f *egressFixture) tunnelRow(t *testing.T, destination string) string {
	t.Helper()
	rows := f.rowsOf(t, "(SELECT calls, last_outcome, last_status_class, last_protocol, last_response_flags FROM egress_destinations WHERE destination = '"+destination+"')")
	if len(rows) != 1 {
		t.Fatalf("%d rows of %s, want one: %v", len(rows), destination, rows)
	}
	return rows[0]
}

const (
	tunnelOpenRow  = `{"calls":1,"last_outcome":"allowed","last_status_class":"2xx","last_protocol":"connect","last_response_flags":[]}`
	tunnelEndedRow = `{"calls":1,"last_outcome":"allowed","last_status_class":"2xx","last_protocol":"connect","last_response_flags":["DC"]}`
)

// Which entry of a tunnel is which (contracts/routes.md, the receiver): Envoy logs an open entry exactly when it
// answers a CONNECT 200, so only a later entry under that answer, of an Envoy that logs them, has had one, and it
// counts nothing. One of an Envoy from before the tag, a CONNECT answered anything else and a plain request count
// themselves. An upgrade that is no CONNECT is logged as it opens too, and an app can ask for one of an upstream it
// runs, so that entry is partial, and its last entry counts the call.
func TestCallOfTellsATunnelsEntries(t *testing.T) {
	refused := tagged(entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 403))
	failed := tagged(entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 503))
	plain := tagged(entry("10.0.4.7", corev3.RequestMethod_GET, "http", "api.stripe.com", 200))
	before := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	beforePeriodic := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	beforePeriodic.CommonProperties.AccessLogType = tunnelGoesOn
	upgrade := func(kind accesslogdatav3.AccessLogType) *accesslogdatav3.HTTPAccessLogEntry {
		e := tagged(entry("10.0.4.7", corev3.RequestMethod_GET, "http", "api.stripe.com", 101))
		e.CommonProperties.AccessLogType = kind
		return e
	}
	for _, c := range []struct {
		name                     string
		e                        *accesslogdatav3.HTTPAccessLogEntry
		partial, opened, counted bool
	}{
		{"an open entry", tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"), false, true, false},
		{"a periodic entry", tunnelEntry(tunnelGoesOn, "10.0.4.7", "api.stripe.com:443", "s-1"), true, false, true},
		{"a periodic entry of an Envoy from before the tag", beforePeriodic, true, false, false},
		{"a last entry", tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"), false, false, true},
		{"a last entry of an Envoy from before the tag", before, false, false, false},
		{"a CONNECT the check refused", refused, false, false, false},
		{"a CONNECT with no upstream", failed, false, false, false},
		{"a plain request", plain, false, false, false},
		{"an upgrade's open entry", upgrade(tunnelOpens), true, false, false},
		{"an upgrade's last entry", upgrade(tunnelEnds), false, false, false},
	} {
		got := callOf(c.e, time.Now(), false)
		if got.partial != c.partial || got.opened != c.opened || got.counted != c.counted {
			t.Errorf("%s: partial %t, opened %t, counted %t; want partial %t, opened %t, counted %t",
				c.name, got.partial, got.opened, got.counted, c.partial, c.opened, c.counted)
		}
	}
}

// A flush's sum of a key (design/EGRESS.md §2.4): a tunnel's two entries are one call, the last result is the later
// entry's, and an open tunnel's result, which is no call's end, never takes an ended call's place, whichever
// the flush took first. Alone it stands, for a row it creates.
func TestASumsResultIsTheLaterEndedCalls(t *testing.T) {
	start := time.Now()
	opens := func(s int) call {
		return call{at: start.Add(time.Duration(s) * time.Second), opened: true, outcome: "allowed", statusClass: "2xx", protocol: "connect", flags: []string{}}
	}
	ends := func(s int, counted bool, class string, flags ...string) call {
		return call{at: start.Add(time.Duration(s) * time.Second), counted: counted, outcome: "allowed", statusClass: class, protocol: "connect", flags: flags}
	}
	goesOn := func(s int) call {
		return call{at: start.Add(time.Duration(s) * time.Second), partial: true, counted: true, outcome: "allowed", statusClass: "2xx", protocol: "connect", flags: []string{}}
	}
	for _, c := range []struct {
		name    string
		calls   []call
		count   int64
		ended   bool
		class   string
		flags   []string
		first   int
		last    int
		atEnded int
	}{
		{"an open tunnel alone", []call{opens(1)}, 1, false, "2xx", []string{}, 1, 1, 1},
		{"a tunnel opened and ended", []call{opens(1), ends(2, true, "2xx", "DC")}, 1, true, "2xx", []string{"DC"}, 1, 2, 2},
		{"its two entries in one instant", []call{opens(1), ends(1, true, "2xx", "DC")}, 1, true, "2xx", []string{"DC"}, 1, 1, 1},
		{"a call ended, then another's tunnel opened", []call{ends(1, false, "5xx", "UF"), opens(2)}, 2, true, "5xx", []string{"UF"}, 1, 2, 1},
		{"a tunnel's open entry taken after a later end", []call{ends(3, false, "5xx", "UF"), opens(2)}, 2, true, "5xx", []string{"UF"}, 2, 3, 3},
		{"two tunnels, one ended", []call{opens(1), opens(2), ends(3, true, "2xx", "UC")}, 2, true, "2xx", []string{"UC"}, 1, 3, 3},
		{"two ends, the later taken first", []call{ends(3, false, "2xx"), ends(2, false, "5xx", "UF")}, 2, true, "2xx", nil, 2, 3, 3},
		{"a call ended, then another's tunnel went on", []call{ends(1, false, "5xx", "UF"), goesOn(2)}, 1, true, "5xx", []string{"UF"}, 1, 2, 1},
		{"a tunnel going on alone", []call{goesOn(2)}, 0, false, "2xx", []string{}, 2, 2, 2},
	} {
		var s destSum
		for _, one := range c.calls {
			s.add(one)
		}
		if s.calls != c.count || s.ended != c.ended || s.statusClass != c.class || !slices.Equal(s.flags, c.flags) ||
			!s.first.Equal(start.Add(time.Duration(c.first)*time.Second)) || !s.last.Equal(start.Add(time.Duration(c.last)*time.Second)) ||
			!s.result.Equal(start.Add(time.Duration(c.atEnded)*time.Second)) {
			t.Errorf("%s: %+v; want %d calls, ended %t, %s %v, seen from %d s to %d s, its result of %d s", c.name, s, c.count, c.ended, c.class, c.flags, c.first, c.last, c.atEnded)
		}
	}
}

// A destination reached through a tunnel still open is its row (#2684), read by what preview connects to: Envoy's
// open entry writes it, the call counted, its result the CONNECT's. A periodic entry counts nothing, and the last
// entry is the same call, which only moves the result on.
func TestATunnelIsItsRowWhileItIsOpen(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelOpenRow {
		t.Fatalf("with the tunnel open the row is %s, want %s", got, tunnelOpenRow)
	}
	f.send(t, tunnelEntry(tunnelGoesOn, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelOpenRow {
		t.Fatalf("after a periodic entry the row is %s, want %s", got, tunnelOpenRow)
	}
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelEndedRow {
		t.Fatalf("with the tunnel ended the row is %s, want %s", got, tunnelEndedRow)
	}
}

// One flush can hold both of a tunnel's entries: still one call, the last entry's result.
func TestATunnelOpenedAndEndedInOneFlushIsOneCall(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"), tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelEndedRow {
		t.Fatalf("the row is %s, want %s", got, tunnelEndedRow)
	}
}

// A tunnel opened before preview went live is still connected after it, and §3.4 reads last_seen_at: a periodic entry
// of a tunnel running on moves that column and nothing else, not the calls, the result, first_seen_at or admitted_at.
// One of an Envoy from before the tag is as it was: no row.
func TestAPeriodicEntryMovesLastSeenAtAlone(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"))
	read := func() (rest string, seen time.Time) {
		t.Helper()
		rest = strings.Join(f.rowsOf(t, `(SELECT first_seen_at, admitted_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags
			FROM egress_destinations WHERE destination = 'api.stripe.com')`), "\n")
		if err := f.pool.QueryRow(f.ctx, "SELECT last_seen_at FROM egress_destinations WHERE destination = 'api.stripe.com'").Scan(&seen); err != nil {
			t.Fatal(err)
		}
		return rest, seen
	}
	rest, seen := read()
	f.send(t, tunnelEntry(tunnelGoesOn, "10.0.4.7", "api.stripe.com:443", "s-1"))
	after, moved := read()
	if !moved.After(seen) {
		t.Errorf("last_seen_at is %v after a periodic entry, %v before it: a tunnel that runs on reads as last seen when it opened", moved, seen)
	}
	if after != rest {
		t.Errorf("a periodic entry moved more than last_seen_at:\n%s\nwas:\n%s", after, rest)
	}
	old := entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "old.example.com:443", 200)
	old.CommonProperties.AccessLogType, old.CommonProperties.StreamId = tunnelGoesOn, "s-2"
	f.send(t, old)
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_destinations WHERE destination = 'old.example.com'"); n != 0 {
		t.Errorf("a periodic entry of an Envoy from before the tag wrote %d rows, want none", n)
	}
}

// A periodic entry past an open entry that was lost writes the row, of the one call: the tunnel is reached all the
// same, and the last entry adds none.
func TestAPeriodicEntryWhoseOpenEntryWasLostWritesItsRow(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, tunnelEntry(tunnelGoesOn, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelOpenRow {
		t.Fatalf("with the tunnel going on the row is %s, want %s", got, tunnelOpenRow)
	}
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelEndedRow {
		t.Fatalf("with it ended the row is %s, want %s", got, tunnelEndedRow)
	}
}

// An upgrade that is no CONNECT (a GET with Upgrade: CONNECT, which the bootstrap's upgrade_configs accepts, answered
// 101 by an upstream the app runs) has an open entry too. It is one call: otherwise an app could add to its own calls.
func TestAnUpgradeOfNoConnectIsOneCall(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	upgrade := func(kind accesslogdatav3.AccessLogType) *accesslogdatav3.HTTPAccessLogEntry {
		e := tagged(entry("10.0.4.7", corev3.RequestMethod_GET, "http", "app.example.com", 101))
		e.CommonProperties.AccessLogType, e.CommonProperties.StreamId = kind, "s-1"
		return e
	}
	f.send(t, upgrade(tunnelOpens))
	f.send(t, upgrade(tunnelEnds))
	if n := count(t, f.ctx, f.pool, "SELECT calls FROM egress_destinations"); n != 1 {
		t.Fatalf("%d calls, want the one upgrade", n)
	}
}

// An open entry on a row that holds a result counts its call and moves last_seen_at, which is what a reader of
// what preview connects to asks, and leaves the result: it is the last ended call's until this one ends.
func TestAnOpenEntryLeavesAnEndedCallsResult(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	failed := tagged(entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 503))
	failed.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}
	f.send(t, failed)
	seen := func() (at time.Time) {
		t.Helper()
		if err := f.pool.QueryRow(f.ctx, "SELECT last_seen_at FROM egress_destinations").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := seen()
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-2"))
	want := `{"calls":2,"last_outcome":"allowed","last_status_class":"5xx","last_protocol":"connect","last_response_flags":["UF"]}`
	if got := f.tunnelRow(t, "api.stripe.com"); got != want {
		t.Fatalf("with a second tunnel open the row is %s, want %s", got, want)
	}
	if after := seen(); !after.After(before) {
		t.Errorf("last_seen_at is %v after an open entry, %v before it: a tunnel open now reads as a destination last seen then", after, before)
	}
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-2"))
	want = `{"calls":2,"last_outcome":"allowed","last_status_class":"2xx","last_protocol":"connect","last_response_flags":["DC"]}`
	if got := f.tunnelRow(t, "api.stripe.com"); got != want {
		t.Fatalf("with it ended the row is %s, want %s", got, want)
	}
}

// Mid-rollout the receiver reads an Envoy from before the tag: it logs no open entry, so a tunnel's last entry
// counts it, or the rollout would lose every tunnel that Envoy ends.
func TestATunnelOfAnEnvoyFromBeforeTheTagCountsAtItsLastEntry(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	old := func() *accesslogdatav3.HTTPAccessLogEntry {
		return entry("10.0.4.7", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	}
	f.send(t, old())
	f.send(t, old())
	if n := count(t, f.ctx, f.pool, "SELECT calls FROM egress_destinations"); n != 2 {
		t.Fatalf("%d calls, want the two tunnels", n)
	}
}

// An open entry can be lost, on a buffer full, a gap of its own, or on a stream that broke, which no gap says
// (design/EGRESS.md §2.4). A last entry with no row then writes one, of its one call, and never fails the flush on
// calls >= 1. On a row it adds none, so that call is one short there.
func TestALastEntryWhoseOpenEntryWasLostWritesItsRow(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelEndedRow {
		t.Fatalf("the row is %s, want %s", got, tunnelEndedRow)
	}
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-2"))
	if got := f.tunnelRow(t, "api.stripe.com"); got != tunnelEndedRow {
		t.Fatalf("a second last entry made the row %s, want %s", got, tunnelEndedRow)
	}
}

// Past the cap an open entry is no row, and its call is on the app env's over_cap gap from the moment it opens,
// so a reader fails closed while the tunnel runs. Its later entries add nothing. A tunnel's last entry whose open
// entry was lost, with no over_cap row yet, writes one of its one call: nothing else would say the call happened.
func TestAnOpenEntryPastTheCapIsCountedOnce(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "claims-portal", Target: "preview", Kind: "web", Run: ranAs("gr_aaaa1111")})
	var full []*accesslogdatav3.HTTPAccessLogEntry
	for i := range defaultDestinations {
		full = append(full, tunnelEntry(tunnelOpens, "10.0.4.7", fmt.Sprintf("d%d.example.com:443", i), ""))
	}
	f.send(t, full...)
	over := func() string {
		t.Helper()
		return strings.Join(f.rowsOf(t, "(SELECT reason, calls, app, env FROM egress_gaps)"), "\n")
	}
	const once = `{"reason":"over_cap","calls":1,"app":"claims-portal","env":"preview"}`
	const twice = `{"reason":"over_cap","calls":2,"app":"claims-portal","env":"preview"}`
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "lost.example.com:443", "s-0"))
	if got := over(); got != once {
		t.Fatalf("a last entry past the cap, its open entry lost, left the gaps %q, want %s: a call no row or gap says", got, once)
	}
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "past.example.com:443", "s-1"))
	if got := over(); got != twice {
		t.Fatalf("with a tunnel past the cap open the gaps are %s, want %s", got, twice)
	}
	f.send(t, tunnelEntry(tunnelGoesOn, "10.0.4.7", "past.example.com:443", "s-1"))
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "past.example.com:443", "s-1"))
	if got := over(); got != twice {
		t.Fatalf("with it ended the gaps are %s, want %s", got, twice)
	}
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_destinations"); n != defaultDestinations {
		t.Errorf("%d rows, want the cap's %d", n, defaultDestinations)
	}
}

// An open entry no app is named for is counted as unattributed, as any call is, and its last entry adds nothing.
func TestAnOpenEntryOfNoOnesIsCountedOnce(t *testing.T) {
	f := newEgressFixture(t)
	f.send(t, tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"))
	f.send(t, tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"))
	if got := strings.Join(f.rowsOf(t, "(SELECT reason, calls FROM egress_gaps)"), "\n"); got != `{"reason":"unattributed","calls":1}` {
		t.Fatalf("gaps %s, want one call no one is named for", got)
	}
}

// A refused CONNECT never opens (HeaderUtility::isConnectResponse: only a 200), so its one entry counts it and files
// its request, as before, under the tag too. A not_allowed an older egress-control let through opens: its two
// entries are one refusal on the request.
func TestAConnectRefusedIsOneEntryAndOneCall(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO projects (id) VALUES ('claims-portal')`); err != nil {
		t.Fatal(err)
	}
	f.send(t, checked(tagged(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 403)), "claims-portal", "preview", "refused", "not_allowed"))
	want := `{"calls":1,"last_outcome":"refused","last_status_class":"4xx","last_protocol":"connect","last_response_flags":["UAEX"]}`
	if got := f.tunnelRow(t, "api.openai.com"); got != want {
		t.Fatalf("the refused row is %s, want %s", got, want)
	}
	through := func(kind accesslogdatav3.AccessLogType) *accesslogdatav3.HTTPAccessLogEntry {
		return checked(tunnelEntry(kind, "172.28.3.11", "api.mistral.ai:443", "s-1"), "claims-portal", "preview", "allowed", "not_allowed")
	}
	f.send(t, through(tunnelOpens))
	f.send(t, through(tunnelEnds))
	got := strings.Join(f.rowsOf(t, "(SELECT destination, refusals FROM egress_requests ORDER BY destination)"), "\n")
	if want := `{"destination":"api.mistral.ai","refusals":1}` + "\n" + `{"destination":"api.openai.com","refusals":1}`; got != want {
		t.Fatalf("requests:\n%s\nwant:\n%s", got, want)
	}
}

// The caller of a tunnel's row is the one the check named on its open entry, which wins over the address's as on
// any entry, and the name it let through is known from then on (known[] reads admitted_at). An open entry takes no
// slot of the tunnels the receiver remembers, bound at egressTunnels: only a periodic entry does, as ever.
func TestAnOpenEntryIsTheChecksCallersAndHoldsNoTunnel(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "10.0.4.7", App: "someone-else", Target: "prod", Kind: "web", Run: ranAs("gr_aaaa1111")})
	f.send(t, checked(tunnelEntry(tunnelOpens, "10.0.4.7", "api.stripe.com:443", "s-1"), "claims-portal", "preview", "allowed", ""))
	want := `{"app":"claims-portal","env":"preview","known":true}`
	if got := strings.Join(f.rowsOf(t, "(SELECT app, env, admitted_at IS NOT NULL AS known FROM egress_destinations)"), "\n"); got != want {
		t.Fatalf("the row is %s, want %s", got, want)
	}
	if len(f.r.tunnels) != 0 {
		t.Errorf("an open entry holds %d of the tunnels the receiver remembers, want none", len(f.r.tunnels))
	}
	f.send(t, checked(tunnelEntry(tunnelGoesOn, "10.0.4.7", "api.stripe.com:443", "s-1"), "claims-portal", "preview", "allowed", ""))
	if len(f.r.tunnels) != 1 {
		t.Errorf("a periodic entry holds %d of the tunnels the receiver remembers, want one", len(f.r.tunnels))
	}
	f.send(t, checked(tunnelEntry(tunnelEnds, "10.0.4.7", "api.stripe.com:443", "s-1"), "claims-portal", "preview", "allowed", ""))
	if len(f.r.tunnels) != 0 {
		t.Errorf("a last entry leaves %d of the tunnels the receiver remembers, want none", len(f.r.tunnels))
	}
}
