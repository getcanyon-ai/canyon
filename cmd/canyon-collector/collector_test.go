package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// golden is the contract's report (contracts/testdata/telemetry/report.json), the one shape the collector keeps.
func golden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/telemetry/report.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Report
}

// goldenV2 is the contract's second-version report (report_v2 in the same file).
func goldenV2(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/telemetry/report.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Report map[string]any `json:"report_v2"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Report
}

// fakeStore keeps what it is handed, and fails when told to, so the error path is walked too.
type fakeStore struct {
	mu    sync.Mutex
	saved [][]byte
	fail  bool
	dup   bool
	drops map[dropped]int64
}

func (f *fakeStore) save(_ context.Context, report []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dup {
		return false, nil
	}
	f.saved = append(f.saved, report)
	if f.fail {
		return false, errors.New("the database is down")
	}
	return true, nil
}

func (f *fakeStore) drop(_ context.Context, counts map[dropped]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("the database is down")
	}
	if f.drops == nil {
		f.drops = map[dropped]int64{}
	}
	for key, n := range counts {
		f.drops[key] += n
	}
	return nil
}

func (f *fakeStore) weekly(context.Context, int) ([]week, error) { return []week{}, nil }

func testCollector(t *testing.T) (*collector, *fakeStore, *bytes.Buffer) {
	t.Helper()
	schema, err := compileSchema()
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	f := &fakeStore{}
	return &collector{schema: schema, limit: newLimiter(time.Now), store: f, readKey: "read-key-for-tests", proxied: true, log: log.New(&logged, "", 0)}, f, &logged
}

// post sends body from addr, as the TLS front would forward it, and answers the status and the body answered.
func post(c *collector, body []byte, addr string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, "/v1/report", bytes.NewReader(body))
	r.RemoteAddr = "10.0.0.2:41000" // the TLS front
	r.Header.Set("X-Forwarded-For", addr)
	w := httptest.NewRecorder()
	c.routes().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func with(report map[string]any, set func(map[string]any)) []byte {
	copied := map[string]any{}
	raw, _ := json.Marshal(report)
	_ = json.Unmarshal(raw, &copied)
	set(copied)
	out, _ := json.Marshal(copied)
	return out
}

// spelled is the report with its promotions count written as given, which no Go value marshals to.
func spelled(report map[string]any, count string) []byte {
	return bytes.Replace(with(report, func(m map[string]any) { m["promotions"] = 424242 }), []byte(`"promotions":424242`), []byte(`"promotions":`+count), 1)
}

// Only the schema's report is kept, every count a whole number spelled as one, at most 1,000,000 (#2808's review: one
// count of 1e5 or 3000000000 once broke the weekly read): a field it doesn't name, at the top or inside, a body that isn't JSON, or one
// past 64 KiB is dropped. Every answer is 204 with nothing in it, so a sender learns nothing and nothing is echoed.
func TestOnlyTheSchemasReportIsKept(t *testing.T) {
	c, f, _ := testCollector(t)
	report, v2 := golden(t), goldenV2(t)
	for _, c2 := range []struct {
		name string
		body []byte
		kept bool
	}{
		{"the golden report", with(report, func(map[string]any) {}), true},
		{"a hostname beside it", with(report, func(m map[string]any) { m["hostname"] = "db.internal" }), false},
		{"an app name inside checks", with(report, func(m map[string]any) { m["checks"].(map[string]any)["app"] = "claims-portal" }), false},
		{"a named custom rule", with(report, func(m map[string]any) { m["rules_fired"].(map[string]any)["Acme Payroll"] = 1 }), false},
		{"not JSON", []byte("STRIPE_KEY=hunter2"), false},
		{"over 64 KiB", append(with(report, func(map[string]any) {}), bytes.Repeat([]byte(" "), 65<<10)...), false},
		{"a count spelled 1.0", spelled(report, "1.0"), false},
		{"a count spelled 1e5", spelled(report, "1e5"), false},
		{"a count of 3000000000", spelled(report, "3000000000"), false},
		{"a count of 1000000", spelled(report, "1000000"), true},
		// The second version (#2953): kept beside the first, a demo's too, which the weekly read counts apart; one
		// that drops a field the version brings, or a licence that says more than its state, is dropped.
		{"the second-version golden report", with(v2, func(map[string]any) {}), true},
		{"a demo's second-version report", with(v2, func(m map[string]any) { m["demo"] = true }), true},
		{"a second-version report without demo", with(v2, func(m map[string]any) { delete(m, "demo") }), false},
		{"a licence's customer", with(v2, func(m map[string]any) { m["licence"].(map[string]any)["customer"] = "Meridian Insurance" }), false},
		{"an exact number of people", with(v2, func(m map[string]any) { m["people_total"] = 7 }), false},
		// agents_via (design/specs/mcp.md §7): reviews by how they were asked for, kept; a server before it sends none,
		// and is kept too; a transport the schema doesn't name, or a client's name, is dropped.
		{"reviews by how they were asked for", with(v2, func(m map[string]any) {}), true},
		{"a second-version report without agents_via", with(v2, func(m map[string]any) { delete(m, "agents_via") }), true},
		{"agents_via naming a client", with(v2, func(m map[string]any) { m["agents_via"].(map[string]any)["cursor-1.7"] = 1 }), false},
		{"agents_via without mcp_remote", with(v2, func(m map[string]any) { delete(m["agents_via"].(map[string]any), "mcp_remote") }), false},
	} {
		before := len(f.saved)
		code, body := post(c, c2.body, "198.51.100.9")
		if code != http.StatusNoContent || body != "" || (len(f.saved) > before) != c2.kept {
			t.Errorf("%s: %d %q, kept %v, want kept %v", c2.name, code, body, len(f.saved) > before, c2.kept)
		}
	}
}

// A dropped report is counted by why, once, and never otherwise kept: unknown_field only for a report the schema would
// take but for names it lacks, a newer Canyon's. A kept report, and one the database refused, count nothing.
func TestADroppedReportIsCountedByWhy(t *testing.T) {
	report, v2 := golden(t), goldenV2(t)
	for _, c := range []struct {
		name, why string
		body      []byte
		set       func(*collector, *fakeStore)
	}{
		{"the golden report", "", with(report, func(map[string]any) {}), nil},
		{"the second-version golden report", "", with(v2, func(map[string]any) {}), nil},
		{"a new field", unknownField, with(v2, func(m map[string]any) { m["new_count"] = 3 }), nil},
		{"a new field inside another", unknownField, with(v2, func(m map[string]any) { m["checks"].(map[string]any)["skipped"] = 1 }), nil},
		{"a new rule", unknownField, with(v2, func(m map[string]any) { m["rules_fired"].(map[string]any)["new.rule"] = 1 }), nil},
		{"a new field and a new rule", unknownField, with(v2, func(m map[string]any) {
			m["new_count"] = 3
			m["rules_fired"].(map[string]any)["new.rule"] = 1
		}), nil},
		{"a new field and a bad version", notSchema, with(v2, func(m map[string]any) { m["new_count"], m["version"] = 3, "a b" }), nil},
		{"a new rule with no count", notSchema, with(v2, func(m map[string]any) { m["rules_fired"].(map[string]any)["new.rule"] = "x" }), nil},
		{"a field missing", notSchema, with(v2, func(m map[string]any) { delete(m, "demo") }), nil},
		{"a new field in an empty object", notSchema, []byte(`{"new_count":3}`), nil},
		{"empty", notSchema, []byte(`{}`), nil},
		{"a list", notSchema, []byte(`[1]`), nil},
		{"a count spelled 1.0", notSchema, spelled(report, "1.0"), nil},
		{"not JSON", notJSON, []byte("STRIPE_KEY=hunter2"), nil},
		{"no body", notJSON, nil, nil},
		{"over 64 KiB", tooBig, bytes.Repeat([]byte(" "), 65<<10), nil},
		{"past the limit", overLimit, with(report, func(map[string]any) {}), func(c *collector, _ *fakeStore) {
			for range perWindow {
				c.limit.allow("198.51.100.9")
			}
		}},
		{"a duplicate", duplicate, with(report, func(map[string]any) {}), func(_ *collector, f *fakeStore) { f.dup = true }},
		{"a database that refuses", "", with(report, func(map[string]any) {}), func(_ *collector, f *fakeStore) { f.fail = true }},
	} {
		one, f, _ := testCollector(t)
		one.drops.now = func() time.Time { return time.Date(2026, 11, 24, 0, 30, 0, 0, time.FixedZone("", 3600)) } // 23:30 UTC the day before
		if c.set != nil {
			c.set(one, f)
		}
		code, answered := post(one, c.body, "198.51.100.9")
		f.fail = false
		if err := one.drops.flush(t.Context(), f); err != nil {
			t.Fatal(err)
		}
		want := map[dropped]int64{}
		if c.why != "" {
			want[dropped{"2026-11-23", c.why}] = 1
		}
		if code != http.StatusNoContent || answered != "" || !maps.Equal(f.drops, want) || (c.why != "" && len(f.saved) != 0) {
			t.Errorf("%s: %d %q, counted %v, saved %d; want %v", c.name, code, answered, f.drops, len(f.saved), want)
		}
	}
}

// A flush adds to what is written and starts again from nothing; one that fails keeps its counts, and those that came
// meanwhile, for the next.
func TestAFailedFlushKeepsItsCounts(t *testing.T) {
	c, f, _ := testCollector(t)
	day := time.Now().UTC().Format(time.DateOnly)
	for i, step := range []struct {
		add  int
		fail bool
		want int64
	}{{2, false, 2}, {0, false, 2}, {3, true, 2}, {1, false, 6}, {1, false, 7}} {
		for range step.add {
			c.drops.add(notJSON)
		}
		f.fail = step.fail
		if err := c.drops.flush(t.Context(), f); (err != nil) != step.fail {
			t.Fatalf("flush %d: %v", i, err)
		}
		if got := f.drops[dropped{day, notJSON}]; got != step.want {
			t.Fatalf("after flush %d: %d written, want %d", i, got, step.want)
		}
	}
}

// The minute's writes stop with the collector, and it waits for one on its way: a write that fails as the collector
// stops puts its counts back before the last flush, so they're written then.
func TestTheLastFlushWaitsForTheMinutes(t *testing.T) {
	c, f, _ := testCollector(t)
	c.drops.add(notJSON)
	blocked, release := make(chan struct{}), make(chan struct{})
	slow := &slowStore{fakeStore: f, blocked: blocked, release: release}
	ctx, stop := context.WithCancel(t.Context())
	done := c.drops.flushing(ctx, slow, time.Millisecond, func(error) {})
	<-blocked // the minute's write is on its way
	stop()
	select {
	case <-done:
		t.Fatal("the writes stopped before the one on its way returned")
	case <-time.After(50 * time.Millisecond):
	}
	close(release) // it fails, and puts its counts back
	<-done
	if err := c.drops.flush(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if got := f.drops[dropped{time.Now().UTC().Format(time.DateOnly), notJSON}]; got != 1 {
		t.Errorf("the last flush wrote %d, want the count the failed write put back", got)
	}
}

// slowStore's first drop waits to be released, then fails.
type slowStore struct {
	*fakeStore
	blocked, release chan struct{}
	once             sync.Once
}

func (s *slowStore) drop(context.Context, map[dropped]int64) error {
	first := false
	s.once.Do(func() { first = true; close(s.blocked) })
	if first {
		<-s.release
	}
	return errors.New("the database is down")
}

// The sender's address is the limiter's alone: it is never in what is stored and never in a log line, the store's
// failure included, whether it came as the peer or as the TLS front's X-Forwarded-For.
func TestTheAddressNeverReachesTheStoreOrALog(t *testing.T) {
	c, f, logged := testCollector(t)
	f.fail = true
	body := with(golden(t), func(map[string]any) {})
	post(c, body, "198.51.100.9")
	c.proxied = false
	r := httptest.NewRequest(http.MethodPost, "/v1/report", bytes.NewReader(body))
	r.RemoteAddr = "203.0.113.77:5555"
	c.routes().ServeHTTP(httptest.NewRecorder(), r)
	if len(f.saved) != 2 || logged.Len() == 0 {
		t.Fatalf("saved %d, logged %q: the test walks no store or log", len(f.saved), logged.String())
	}
	for _, ip := range []string{"198.51.100.9", "203.0.113.77", "10.0.0.2"} {
		for _, saved := range f.saved {
			if bytes.Contains(saved, []byte(ip)) {
				t.Errorf("the store was handed %s: %s", ip, saved)
			}
		}
		if strings.Contains(logged.String(), ip) {
			t.Errorf("a log line names %s: %s", ip, logged)
		}
	}
}

// One address may send perWindow reports a window; the next is dropped, another address is not, and a new window
// forgets every address.
func TestTheLimitHolds(t *testing.T) {
	c, f, _ := testCollector(t)
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c.limit = newLimiter(func() time.Time { return now })
	body := with(golden(t), func(map[string]any) {})
	for range perWindow {
		post(c, body, "198.51.100.9")
	}
	post(c, body, "198.51.100.9")
	if len(f.saved) != perWindow {
		t.Fatalf("%d kept from one address in a window, want %d", len(f.saved), perWindow)
	}
	post(c, body, "198.51.100.10")
	now = now.Add(window)
	post(c, body, "198.51.100.9")
	if len(f.saved) != perWindow+2 {
		t.Errorf("another address, then a new window: %d kept, want %d", len(f.saved), perWindow+2)
	}
}

// The weekly read answers the read key's holder alone, compared whole.
func TestTheWeeklyReadTakesTheKey(t *testing.T) {
	c, _, _ := testCollector(t)
	for _, c2 := range []struct {
		auth string
		code int
	}{{"", 401}, {"Bearer nope", 401}, {"read-key-for-tests", 401}, {"Bearer read-key-for-tests", 200}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/weekly", nil)
		if c2.auth != "" {
			r.Header.Set("Authorization", c2.auth)
		}
		w := httptest.NewRecorder()
		c.routes().ServeHTTP(w, r)
		if w.Code != c2.code {
			t.Errorf("%q: %d, want %d", c2.auth, w.Code, c2.code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/weekly?weeks=61", nil)
	r.Header.Set("Authorization", "Bearer read-key-for-tests")
	w := httptest.NewRecorder()
	c.routes().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("61 weeks: %d", w.Code)
	}
}

// An IPv6 sender is one /64, which one host holds whole: rotating addresses inside it buys no more reports. The
// limiter holds at most sendersMost addresses a window; past them a new sender is dropped until the next window,
// while one it holds still sends (#2808's review).
func TestTheLimiterKeysIPv6ByItsSlash64AndHoldsAtMost(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	for i := range perWindow {
		l.allow(fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if l.allow("2001:db8:1:2:ffff::1") || !l.allow("2001:db8:1:3::1") || !l.allow("198.51.100.9") {
		t.Fatal("a /64 is not one sender, or another /64 or an IPv4 address was dropped with it")
	}
	l = newLimiter(func() time.Time { return now })
	for i := range sendersMost {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255))
	}
	if l.allow("198.51.100.9") || !l.allow("10.0.0.0") || len(l.seen) != sendersMost {
		t.Fatalf("past %d senders: a new one allowed, or a held one dropped; %d held", sendersMost, len(l.seen))
	}
	now = now.Add(window)
	if !l.allow("198.51.100.9") {
		t.Error("a new window still dropped a new sender")
	}
}

// The boot refuses a read key under 32 characters: /v1/weekly has no rate limit, so a short key could be guessed.
func TestAShortReadKeyRefusesTheBoot(t *testing.T) {
	for key, ok := range map[string]bool{"": false, "read-key-for-tests": false, strings.Repeat("k", readKeyLeast-1): false, strings.Repeat("k", readKeyLeast): true} {
		if err := readKeyOf(key); (err == nil) != ok {
			t.Errorf("a %d-character key: %v", len(key), err)
		}
	}
}

// No container of deploy/collector writes a sender's address: the front, which sees it, discards every log line in
// Caddy and keeps no container log, and runs from the Caddyfile that says so (#2808's review). The collector's own
// lines are TestTheAddressNeverReachesTheStoreOrALog's.
func TestNoContainerLogsASendersAddress(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/collector/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Command any                     `yaml:"command"`
			Volumes []string                `yaml:"volumes"`
			Logging struct{ Driver string } `yaml:"logging"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	front := compose.Services["front"]
	if front.Logging.Driver != "none" || front.Command != nil || !slices.Contains(front.Volumes, "./Caddyfile:/etc/caddy/Caddyfile:ro") {
		t.Errorf("the front logs to %q, runs %v, mounts %v; want no container log and the Caddyfile alone", front.Logging.Driver, front.Command, front.Volumes)
	}
	caddyfile, err := os.ReadFile("../../deploy/collector/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Fields(string(caddyfile)); !slices.Contains(lines, "discard") {
		t.Errorf("the Caddyfile discards no log:\n%s", caddyfile)
	}
}
