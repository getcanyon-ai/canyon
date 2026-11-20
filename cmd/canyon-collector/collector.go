package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getcanyon-ai/canyon/contracts"
)

// The collector (#2808): it receives the daily telemetry reports Canyon servers send (#2807) and keeps exactly what
// contracts/schema/telemetry.schema.json allows, with the time it arrived, for 13 months. The sender's IP address is
// used in memory to rate-limit, and nowhere else: no access log, no GeoIP, never stored.

const (
	reportMost   = 64 << 10          // a report is a few hundred bytes; past this it is not one
	keep         = "13 months"       // how long a report is kept
	perWindow    = 30                // reports one address may send a window: one server sends one a day, many may share a NAT
	window       = time.Hour         // the limiter's window
	sendersMost  = 100000            // the addresses the limiter holds a window: past them a new sender waits for the next
	readKeyLeast = 32                // a read key's least length: /v1/weekly has no rate limit
	weeksMost    = 60                // the most weeks /v1/weekly answers
	topMost      = 10                // the most unsupported categories and rules a week lists
	day          = 24 * time.Hour    // a retention pass's interval
	writeLimit   = 5 * time.Second   // how long one stored report may take
	readLimit    = 30 * time.Second  // how long one weekly read may take
	idleLimit    = 120 * time.Second // keep-alive
	maxHeader    = 8 << 10           // a POST of one small JSON body needs no more
	headerTime   = 10 * time.Second  // a slow client's headers
	bodyTime     = 10 * time.Second  // and its body
	flushLimit   = 30 * time.Second  // shutdown
	tableSQL     = `CREATE TABLE IF NOT EXISTS reports (id bigserial PRIMARY KEY, received_at timestamptz NOT NULL DEFAULT now(), report jsonb NOT NULL);
		CREATE INDEX IF NOT EXISTS reports_received_at ON reports (received_at);
		CREATE UNIQUE INDEX IF NOT EXISTS reports_one_a_day ON reports ((report->>'instance_id'), ((received_at AT TIME ZONE 'UTC')::date));
		ALTER TABLE reports ADD COLUMN IF NOT EXISTS instance_id text GENERATED ALWAYS AS (report->>'instance_id') STORED;
		ALTER TABLE reports ADD COLUMN IF NOT EXISTS day date GENERATED ALWAYS AS ((received_at AT TIME ZONE 'UTC')::date) STORED;
		ALTER TABLE reports ADD COLUMN IF NOT EXISTS version text GENERATED ALWAYS AS (report->>'version') STORED;
		ALTER TABLE reports ADD COLUMN IF NOT EXISTS install_mode text GENERATED ALWAYS AS (report->>'install_mode') STORED;
		ALTER TABLE reports ADD COLUMN IF NOT EXISTS demo boolean GENERATED ALWAYS AS (coalesce(report->'demo' = 'true'::jsonb, false)) STORED;
		CREATE INDEX IF NOT EXISTS reports_instance_day ON reports (instance_id, day)`
)

// store is where reports go: Postgres, and a test's own.
type store interface {
	save(ctx context.Context, report []byte) error
	weekly(ctx context.Context, weeks int) ([]week, error)
}

// collector is the HTTP side: the schema every report is held to, the limiter, the store and the read key.
type collector struct {
	schema  *jsonschema.Schema
	limit   *limiter
	store   store
	readKey string
	proxied bool        // behind the compose file's TLS front: the address is the last X-Forwarded-For entry it appended
	log     *log.Logger // errors only, never an address or a report
}

// compileSchema is the telemetry schema, compiled once at start.
func compileSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(contracts.TelemetrySchema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const id = "https://docs.getcanyon.ai/contracts/schema/telemetry.schema.json"
	if err := c.AddResource(id, doc); err != nil {
		return nil, err
	}
	return c.Compile(id)
}

func (c *collector) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/report", c.report)
	mux.HandleFunc("GET /v1/weekly", c.weekly)
	return mux
}

// address is where a request came from, for the limiter alone.
func (c *collector) address(r *http.Request) string {
	if c.proxied {
		if hops := r.Header.Values("X-Forwarded-For"); len(hops) > 0 {
			last := hops[len(hops)-1]
			return strings.TrimSpace(last[strings.LastIndex(last, ",")+1:])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// report takes one report: 204 whatever happens, so a sender learns nothing from the answer, and nothing is echoed.
// A body over the size, not JSON, or not the schema's is dropped; so is one past the sender's limit.
func (c *collector) report(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusNoContent)
	if !c.limit.allow(c.address(r)) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, reportMost+1))
	if err != nil || len(body) > reportMost {
		return
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil || c.schema.Validate(doc) != nil || !wholeNumbers(doc) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeLimit)
	defer cancel()
	if err := c.store.save(ctx, body); err != nil {
		c.log.Printf("canyon-collector: storing a report: %v", err) // the error alone: never the address or the report
	}
}

// wholeNumbers reports whether every number in doc is spelled as a whole number, digits alone: JSON Schema reads 1.0
// and 1e5 as integers by value, and every number a report carries is a count.
func wholeNumbers(doc any) bool {
	switch v := doc.(type) {
	case json.Number:
		_, err := strconv.ParseUint(string(v), 10, 64)
		return err == nil
	case map[string]any:
		for _, e := range v {
			if !wholeNumbers(e) {
				return false
			}
		}
	case []any:
		for _, e := range v {
			if !wholeNumbers(e) {
				return false
			}
		}
	}
	return true
}

// weekly answers the weekly aggregates to the holder of the read key.
func (c *collector) weekly(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if c.readKey == "" || !ok || subtle.ConstantTimeCompare([]byte(key), []byte(c.readKey)) != 1 {
		http.Error(w, "a read key is required", http.StatusUnauthorized)
		return
	}
	weeks := 12
	if v := r.URL.Query().Get("weeks"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > weeksMost {
			http.Error(w, "weeks is 1 to 60", http.StatusBadRequest)
			return
		}
		weeks = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), readLimit)
	defer cancel()
	out, err := c.store.weekly(ctx, weeks)
	if err != nil {
		c.log.Printf("canyon-collector: the weekly read: %v", err)
		http.Error(w, "the weekly read failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"weeks": out})
}

// limiter allows each address perWindow reports a window, in memory alone: a new window forgets every address.
type limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	start time.Time
	seen  map[string]int
}

func newLimiter(now func() time.Time) *limiter { return &limiter{now: now, seen: map[string]int{}} }

func (l *limiter) allow(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.start) >= window {
		l.start, l.seen = now, map[string]int{}
	}
	key := senderOf(addr)
	if _, held := l.seen[key]; !held && len(l.seen) >= sendersMost { // the map stays bounded: a new sender waits
		return false
	}
	l.seen[key]++
	return l.seen[key] <= perWindow
}

// senderOf is the limiter's key for addr: an IPv6 address's /64, which one host holds whole, else addr itself.
func senderOf(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil || !ip.Is6() || ip.Is4In6() {
		return addr
	}
	p, _ := ip.Prefix(64)
	return p.String()
}

// week is one week's answers to the launch questions. An install is counted in a week when it reported that week,
// a day old or more, and is no demo: a demo's reports (demo true) are counted apart, in DemoInstalls, and an install
// under a day old (age_days "0") is counted from its next report (#2953).
type week struct {
	Week         string                    `json:"week"`          // its Monday, UTC
	Installs     int                       `json:"installs"`      // instances that reported that week
	NewInstalls  int                       `json:"new_installs"`  // instances whose first report was that week, unless it said the install was a week old or more
	Funnel       map[string]int            `json:"funnel"`        // instances that week having reached each milestone
	Deploying    map[string]ratio          `json:"deploying"`     // of that week's new installs, how many still reviewed code 1 and 4 weeks on
	Retention    map[string]kept           `json:"retention"`     // of that week's new installs, how many reported, and how many reviewed code, 1, 4 and 12 weeks on
	DemoInstalls demos                     `json:"demo_installs"` // demos that reported that week, and demos new that week
	InstallsBy   map[string][]named        `json:"installs_by"`   // that week's installs by version, install_mode ("unset" when left out) and platform (os/arch)
	Sums         sums                      `json:"sums"`          // the week's reports' period counts, summed
	People       map[string]spread         `json:"people"`        // people per install at 1, 7 and 28 days, from each install's last report of the week
	Features     map[string]share          `json:"features"`      // of that week's second-version installs, how many use each feature, from each one's last report of the week
	First        map[string]map[string]int `json:"first"`         // that week's new installs by how long each first took, from each one's last report
	Unsupported  []named                   `json:"unsupported"`   // the week's top things Canyon doesn't do yet, summed
	RulesFired   []named                   `json:"rules_fired"`   // the week's top rules, summed
	Skipped      int                       `json:"skipped"`       // counts the week's reports held that were no count from 0 to 1,000,000, left out of the sums
}

type ratio struct {
	Of        int `json:"of"`
	Deploying int `json:"deploying"`
}

type kept struct {
	Of        int `json:"of"`
	Reported  int `json:"reported"`
	Deploying int `json:"deploying"`
}

type demos struct {
	Active int `json:"active"`
	New    int `json:"new"`
}

// sums are a week's period counts: deploys, reviews and how they were asked for (the CLI, canyon mcp or the server's
// /mcp; a report from before agents_via adds nothing to it), failures by why, hand-off releases by how they ended, and
// fixes.
type sums struct {
	Deploys   map[string]int `json:"deploys"`
	Checks    map[string]int `json:"checks"`
	AgentsVia map[string]int `json:"agents_via"`
	Failures  []named        `json:"failures"`
	Handoff   map[string]int `json:"handoff"`
	Fixes     int            `json:"fixes"`
}

// spread is a count across installs: how many installs said it, its median and its sum.
type spread struct {
	Of     int `json:"of"`
	Median int `json:"median"`
	Sum    int `json:"sum"`
}

type share struct {
	Of    int `json:"of"`
	Using int `json:"using"`
}

type named struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// pgStore is Postgres.
type pgStore struct{ pool *pgxpool.Pool }

func (s pgStore) save(ctx context.Context, report []byte) error {
	// one report an instance a UTC day, the first: a resent or replayed one would count its sums twice
	_, err := s.pool.Exec(ctx, "INSERT INTO reports (report) VALUES ($1) ON CONFLICT DO NOTHING", report)
	return err
}

// prune deletes the reports older than keep.
func (s pgStore) prune(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM reports WHERE received_at < now() - interval '"+keep+"'")
	return err
}

// deploying is a report r that counted a review that day.
//
// Every count is read through countOf: a stored report is the schema's, but the weekly read never trusts that, so a
// value that is not a number from 0 to 1,000,000 reads as no count, never as an error that fails the whole read.
var deploying = `(` + `coalesce(` + countOf("r.report->'checks'->'passed'") + `, 0) + coalesce(` + countOf("r.report->'checks'->'advisory'") + `, 0) + coalesce(` +
	countOf("r.report->'checks'->'blocked'") + `, 0)) > 0`

// countMost is the schema's largest count (definitions.count).
const countMost = 1000000

// countOf is the SQL for a JSON value read as a count: a bigint for a number from 0 to countMost, NULL for anything
// else. CASE decides the type before any cast, so no value can make the cast itself fail.
func countOf(value string) string {
	n := `CASE WHEN jsonb_typeof(` + value + `) = 'number' THEN (` + value + `)::text::numeric END`
	return `CASE WHEN ` + n + ` BETWEEN 0 AND ` + strconv.Itoa(countMost) + ` THEN (` + n + `)::bigint END`
}

// weekCTE is what every weekly query reads: each report with its week (wk), the counted ones — a day old or more —
// each install's last counted report of a week (latest), and each install's first counted report (firsts). $1 is the
// number of weeks.
const weekCTE = `WITH base AS (SELECT instance_id AS id, received_at, report, demo, date_trunc('week', day)::date AS wk FROM reports),
	counted AS (SELECT * FROM base WHERE coalesce(report->>'age_days', '') <> '0'),
	latest AS (SELECT DISTINCT ON (wk, id) * FROM counted ORDER BY wk, id, received_at DESC),
	firsts AS (SELECT DISTINCT ON (id) id, received_at AS first_at, wk, demo, report->>'age_days' AS age FROM counted ORDER BY id, received_at),
	news AS (SELECT * FROM firsts WHERE coalesce(age, '1-6') = '1-6'),
	since AS (SELECT (date_trunc('week', now()) - make_interval(weeks => $1 - 1))::date AS at)
	`

// retained are the weeks after a new install's first report its retention is read at: a week from 1, 4 and 12 weeks on.
var retained = []struct {
	key  string
	days int
}{{"1w", 7}, {"4w", 28}, {"12w", 84}}

// features are what a second-version install's last report of the week says it uses, each a SQL condition on r.report.
var features = []struct{ name, when string }{
	{"ai_review", `r.report->'ai_review' = 'true'::jsonb`},
	{"ai_gateway", `r.report->'ai_gateway' = 'true'::jsonb`},
	{"custom_rules", `coalesce(` + countOf("r.report->'custom_rules'->'ai'") + `, 0) + coalesce(` + countOf("r.report->'custom_rules'->'code'") + `, 0) > 0`},
	{"policy_changed", `coalesce(` + countOf("r.report->'policy_changed'") + `, 0) > 0`},
	{"run_by_org", `r.report->>'run_by' = 'org'`},
	{"db_sessions", `r.report->'db_sessions'->>'setting' = 'on'`},
	{"outbound", `r.report->'outbound'->>'via' = 'canyon'`},
	{"outbound_keys", `r.report->'outbound'->'credential_routes' = 'true'::jsonb`},
	{"patching", `coalesce(` + countOf("r.report->'patching'->'clean'") + `, 0) + coalesce(` + countOf("r.report->'patching'->'regressed'") + `, 0) + coalesce(` +
		countOf("r.report->'patching'->'ineffective'") + `, 0) + coalesce(` + countOf("r.report->'patching'->'reverted'") + `, 0) > 0`},
	{"licence", `r.report->'licence'->>'state' IN ('valid', 'grace')`},
	{"prod_approval", `r.report->'prod_approval' = 'true'::jsonb`},
	{"notify", `coalesce(` + countOf("r.report->'notify'->'slack'") + `, 0) + coalesce(` + countOf("r.report->'notify'->'teams'") + `, 0) > 0`},
	{"github", `r.report->'github' = 'true'::jsonb`},
	{"group_sync", `r.report->'group_sync' = 'true'::jsonb`},
}

func (s pgStore) weekly(ctx context.Context, weeks int) ([]week, error) {
	byWeek := map[string]*week{}
	at := func(w string) *week {
		if byWeek[w] == nil {
			byWeek[w] = &week{Week: w, Funnel: map[string]int{"first_check": 0, "first_block": 0, "first_fix": 0},
				Deploying: map[string]ratio{"1w": {}, "4w": {}}, Retention: map[string]kept{"1w": {}, "4w": {}, "12w": {}},
				InstallsBy: map[string][]named{"version": {}, "install_mode": {}, "platform": {}},
				Sums: sums{Deploys: map[string]int{"preview": 0, "prod": 0}, Checks: map[string]int{"passed": 0, "advisory": 0, "blocked": 0},
					AgentsVia: map[string]int{"cli": 0, "mcp_local": 0, "mcp_remote": 0}, Failures: []named{},
					Handoff: map[string]int{"deployed": 0, "failed": 0, "refused": 0, "undelivered": 0}},
				People: map[string]spread{"d1": {}, "d7": {}, "d28": {}}, Features: map[string]share{}, First: map[string]map[string]int{},
				Unsupported: []named{}, RulesFired: []named{}}
		}
		return byWeek[w]
	}
	// Installs and the funnel, from each install's last counted report of the week; demos apart.
	rows, err := s.pool.Query(ctx, weekCTE+`SELECT to_char(wk, 'YYYY-MM-DD'), demo, count(*),
		count(*) FILTER (WHERE report->'milestones' ? 'first_check'), count(*) FILTER (WHERE report->'milestones' ? 'first_block'),
		count(*) FILTER (WHERE report->'milestones' ? 'first_fix')
		FROM latest WHERE wk >= (SELECT at FROM since) GROUP BY 1, 2`, weeks)
	if err != nil {
		return nil, err
	}
	var w string
	var demo bool
	var installs, check, block, fix int
	if _, err := pgx.ForEachRow(rows, []any{&w, &demo, &installs, &check, &block, &fix}, func() error {
		one := at(w)
		if demo {
			one.DemoInstalls.Active = installs
			return nil
		}
		one.Installs, one.Funnel["first_check"], one.Funnel["first_block"], one.Funnel["first_fix"] = installs, check, block, fix
		return nil
	}); err != nil {
		return nil, err
	}
	// New installs and their retention: reported at all, and reviewing code, a week from 1, 4 and 12 weeks on.
	held := func(days int, also string) string {
		return `count(*) FILTER (WHERE EXISTS (SELECT 1 FROM reports r WHERE r.instance_id = n.id AND r.received_at >= n.first_at + interval '` + strconv.Itoa(days) +
			` days' AND r.received_at < n.first_at + interval '` + strconv.Itoa(days+7) + ` days'` + also + `))`
	}
	var cols []string
	for _, k := range retained {
		cols = append(cols, held(k.days, ""), held(k.days, " AND "+deploying))
	}
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(n.wk, 'YYYY-MM-DD'), n.demo, count(*), `+strings.Join(cols, ", ")+`
		FROM news n WHERE n.first_at >= (SELECT at FROM since) GROUP BY 1, 2`, weeks)
	if err != nil {
		return nil, err
	}
	var fresh int
	got := make([]int, 2*len(retained))
	dest := []any{&w, &demo, &fresh}
	for i := range got {
		dest = append(dest, &got[i])
	}
	if _, err := pgx.ForEachRow(rows, dest, func() error {
		one := at(w)
		if demo {
			one.DemoInstalls.New = fresh
			return nil
		}
		one.NewInstalls = fresh
		for i, k := range retained {
			one.Retention[k.key] = kept{Of: fresh, Reported: got[2*i], Deploying: got[2*i+1]}
		}
		one.Deploying["1w"], one.Deploying["4w"] = ratio{fresh, got[1]}, ratio{fresh, got[3]}
		return nil
	}); err != nil {
		return nil, err
	}
	// Installs by version, install mode and platform.
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(r.wk, 'YYYY-MM-DD'), g.dim, g.val, count(*) FROM latest r,
		LATERAL (VALUES ('version', coalesce(r.report->>'version', 'unknown')), ('install_mode', coalesce(r.report->>'install_mode', 'unset')),
			('platform', coalesce(r.report->>'os', 'unknown') || '/' || coalesce(r.report->>'arch', 'unknown'))) g(dim, val)
		WHERE NOT r.demo AND r.wk >= (SELECT at FROM since) GROUP BY 1, 2, 3`, weeks)
	if err != nil {
		return nil, err
	}
	var dim, val string
	var n64 int64
	if _, err := pgx.ForEachRow(rows, []any{&w, &dim, &val, &n64}, func() error {
		one := at(w)
		one.InstallsBy[dim] = append(one.InstallsBy[dim], named{val, int(n64)})
		return nil
	}); err != nil {
		return nil, err
	}
	// The period counts, summed over every report of the week but a demo's.
	sum := func(path string) string { return `coalesce(sum(` + countOf("r.report->"+path) + `), 0)::bigint` }
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(r.wk, 'YYYY-MM-DD'), `+strings.Join([]string{sum("'deploys'->'preview'"), sum("'deploys'->'prod'"),
		sum("'checks'->'passed'"), sum("'checks'->'advisory'"), sum("'checks'->'blocked'"), sum("'handoff'->'deployed'"), sum("'handoff'->'failed'"),
		sum("'handoff'->'refused'"), sum("'handoff'->'undelivered'"), sum("'fixes'->'count'"), sum("'agents_via'->'cli'"), sum("'agents_via'->'mcp_local'"),
		sum("'agents_via'->'cli'")}, ", ")+`
		FROM base r WHERE NOT r.demo AND r.wk >= (SELECT at FROM since) GROUP BY 1`, weeks)
	if err != nil {
		return nil, err
	}
	var preview, prod, passed, advisory, blocked, deployed, failed, refused, undelivered, fixes, viaCLI, viaLocal, viaRemote int
	if _, err := pgx.ForEachRow(rows, []any{&w, &preview, &prod, &passed, &advisory, &blocked, &deployed, &failed, &refused, &undelivered, &fixes,
		&viaCLI, &viaLocal, &viaRemote}, func() error {
		one := at(w)
		one.Sums.Deploys = map[string]int{"preview": preview, "prod": prod}
		one.Sums.Checks = map[string]int{"passed": passed, "advisory": advisory, "blocked": blocked}
		one.Sums.Handoff = map[string]int{"deployed": deployed, "failed": failed, "refused": refused, "undelivered": undelivered}
		one.Sums.Fixes = fixes
		one.Sums.AgentsVia = map[string]int{"cli": viaCLI, "mcp_local": viaLocal, "mcp_remote": viaRemote}
		return nil
	}); err != nil {
		return nil, err
	}
	// People at 1, 7 and 28 days: how many installs said, the median and the sum.
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(r.wk, 'YYYY-MM-DD'), k.key, count(c.n), coalesce(percentile_disc(0.5) WITHIN GROUP (ORDER BY c.n), 0)::bigint,
		coalesce(sum(c.n), 0)::bigint FROM latest r, unnest(ARRAY['d1', 'd7', 'd28']) k(key), LATERAL (SELECT `+countOf("r.report->'people'->k.key")+` AS n) c
		WHERE NOT r.demo AND r.wk >= (SELECT at FROM since) AND r.report ? 'people' GROUP BY 1, 2`, weeks)
	if err != nil {
		return nil, err
	}
	var key string
	var of, median, total int
	if _, err := pgx.ForEachRow(rows, []any{&w, &key, &of, &median, &total}, func() error {
		at(w).People[key] = spread{Of: of, Median: median, Sum: total}
		return nil
	}); err != nil {
		return nil, err
	}
	// Feature adoption, of the week's second-version installs.
	cols = cols[:0]
	for _, f := range features {
		cols = append(cols, `count(*) FILTER (WHERE `+f.when+`)`)
	}
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(r.wk, 'YYYY-MM-DD'), count(*), `+strings.Join(cols, ", ")+`
		FROM latest r WHERE NOT r.demo AND r.wk >= (SELECT at FROM since) AND r.report ? 'period_hours' GROUP BY 1`, weeks)
	if err != nil {
		return nil, err
	}
	using := make([]int, len(features))
	dest = []any{&w, &of}
	for i := range using {
		dest = append(dest, &using[i])
	}
	if _, err := pgx.ForEachRow(rows, dest, func() error {
		one := at(w)
		for i, f := range features {
			one.Features[f.name] = share{Of: of, Using: using[i]}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// How long each first took, for the week's new installs, from each one's last report.
	rows, err = s.pool.Query(ctx, weekCTE+`, last AS (SELECT DISTINCT ON (instance_id) instance_id, report FROM reports ORDER BY instance_id, received_at DESC)
		SELECT to_char(n.wk, 'YYYY-MM-DD'), k.key, l.report->'first'->>k.key, count(*) FROM news n JOIN last l ON l.instance_id = n.id,
		unnest(ARRAY['check', 'block', 'fix', 'prod']) k(key)
		WHERE NOT n.demo AND n.first_at >= (SELECT at FROM since) AND jsonb_typeof(l.report->'first'->k.key) = 'string' GROUP BY 1, 2, 3`, weeks)
	if err != nil {
		return nil, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&w, &key, &val, &n64}, func() error {
		one := at(w)
		if one.First[key] == nil {
			one.First[key] = map[string]int{}
		}
		one.First[key][val] = int(n64)
		return nil
	}); err != nil {
		return nil, err
	}
	objectOf := func(field string) string { // a field that is not an object reads as an empty one
		return `jsonb_each(CASE WHEN jsonb_typeof(r.report->'` + field + `') = 'object' THEN r.report->'` + field + `' ELSE '{}'::jsonb END)`
	}
	rows, err = s.pool.Query(ctx, weekCTE+`SELECT to_char(r.wk, 'YYYY-MM-DD'), e.field, e.key,
		coalesce(sum(c.n), 0)::bigint, count(*) FILTER (WHERE c.n IS NULL)::bigint
		FROM base r, LATERAL (SELECT 'unsupported' AS field, key, value FROM `+objectOf("unsupported")+`
			UNION ALL SELECT 'rules_fired', key, value FROM `+objectOf("rules_fired")+`
			UNION ALL SELECT 'failures', key, value FROM `+objectOf("failures")+`) e,
		LATERAL (SELECT `+countOf("e.value")+` AS n) c
		WHERE NOT r.demo AND r.wk >= (SELECT at FROM since) GROUP BY 1, 2, 3`, weeks)
	if err != nil {
		return nil, err
	}
	var field string
	var n, skipped int64
	if _, err := pgx.ForEachRow(rows, []any{&w, &field, &key, &n, &skipped}, func() error {
		one := at(w)
		one.Skipped += int(skipped)
		if n == 0 && skipped > 0 {
			return nil // nothing counted under this name, only skipped
		}
		switch field {
		case "unsupported":
			one.Unsupported = append(one.Unsupported, named{key, int(n)})
		case "rules_fired":
			one.RulesFired = append(one.RulesFired, named{key, int(n)})
		default:
			one.Sums.Failures = append(one.Sums.Failures, named{key, int(n)})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]week, 0, len(byWeek))
	for _, one := range byWeek {
		one.Unsupported, one.RulesFired = top(one.Unsupported), top(one.RulesFired)
		for dim := range one.InstallsBy {
			one.InstallsBy[dim] = top(one.InstallsBy[dim])
		}
		one.Sums.Failures = top(one.Sums.Failures)
		out = append(out, *one)
	}
	slices.SortFunc(out, func(x, y week) int { return strings.Compare(y.Week, x.Week) }) // newest first
	return out, nil
}

// top is the topMost largest counts, ties by name, so an answer reads the same twice.
func top(xs []named) []named {
	slices.SortFunc(xs, func(x, y named) int {
		if x.Count != y.Count {
			return y.Count - x.Count
		}
		return strings.Compare(x.Name, y.Name)
	})
	return xs[:min(len(xs), topMost)]
}

var errNoKey = errors.New("CANYON_COLLECTOR_READ_KEY is required: the weekly read answers its holder alone")

// readKeyOf refuses a read key the boot must not start on.
func readKeyOf(key string) error {
	if key == "" {
		return errNoKey
	}
	if len(key) < readKeyLeast {
		return fmt.Errorf("CANYON_COLLECTOR_READ_KEY is %d characters, and %d at the least: /v1/weekly has no rate limit (openssl rand -hex 32)", len(key), readKeyLeast)
	}
	return nil
}
