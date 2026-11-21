package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// Telemetry (#2807; contracts/routes.md, Telemetry; contracts/schema/telemetry.schema.json): one report of counts a
// day, never a name. The report is built here from the server's own tables, and sent at most once a day across
// replicas, by whichever claims the day.

// telemetryEndpoint is Canyon's collector (#2808).
const telemetryEndpoint = "https://usage.myagentcontrol.ai/v1/report"

const (
	telemetrySend  = 2 * time.Second // the one try's whole budget: an air-gapped install never waits on it
	telemetryEvery = time.Hour       // how often a replica looks for the day's send
	telemetryWhen  = "23 hours"      // a day's claim, a little under a day so a slow tick never skips one
)

// telemetryNotice is printed once, the first time a server reads its telemetry row (contracts/routes.md, Telemetry): on
// by default, said in the log, the docs and the release notes (the owner, 2026-11-12).
const telemetryNotice = "Canyon sends one report of counts a day to help improve it: never code, names, hosts or emails. It's on by default. " +
	"canyon telemetry show prints it, and canyon telemetry off turns it off (or Settings › Telemetry, or CANYON_TELEMETRY=0 on the server). " +
	"What each field is: https://www.getcanyon.ai/docs/org/usage-reports/"

// telemetryConfig is the server's environment's part: read once at boot.
type telemetryConfig struct {
	envOff   bool          // CANYON_TELEMETRY=0, DO_NOT_TRACK=1 or CI: off, whatever an admin sets
	mode     string        // CANYON_INSTALL_MODE: "" leaves install_mode out
	url      string        // where the report goes: "" sends nothing, as a test that drives a send itself
	every    time.Duration // how often the sender looks; 0 runs no sender
	demo     bool          // CANYON_DEMO_SEED=1: Canyon's demo, which the collector counts apart
	provider string        // CANYON_MODEL_PROVIDER: the AI review's kind of model, "" for the default, anthropic
	gateway  bool          // CANYON_MODEL_URL is set: the model is reached through a gateway
}

// telemetryConfigOf reads the environment. An install mode the schema does not name refuses the boot, never a report
// that carries one.
func telemetryConfigOf(getenv func(string) string) (telemetryConfig, error) {
	c := telemetryConfig{envOff: getenv("CANYON_TELEMETRY") == "0" || getenv("DO_NOT_TRACK") == "1" || getenv("CI") != "", url: telemetryEndpoint, every: telemetryEvery,
		demo: getenv("CANYON_DEMO_SEED") == "1", provider: getenv("CANYON_MODEL_PROVIDER"), gateway: getenv("CANYON_MODEL_URL") != ""}
	switch c.mode = getenv("CANYON_INSTALL_MODE"); c.mode {
	case "", "laptop", "one_server", "helm_one_node", "kubernetes":
		return c, nil
	}
	return c, fmt.Errorf("CANYON_INSTALL_MODE is laptop, one_server, helm_one_node or kubernetes, or unset, and got %q", c.mode)
}

// telemetryReport is the schema's report, its second version (#2953). Fields the server has no source for
// (skill_installs, last_install_failure, ref) are not here: left out of the report, never guessed.
type telemetryReport struct {
	InstanceID    string            `json:"instance_id"`
	Version       string            `json:"version"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	InstallMode   string            `json:"install_mode,omitempty"`
	Apps          int               `json:"apps"`
	Checks        checkCounts       `json:"checks"`
	Milestones    []string          `json:"milestones"`
	Fixes         fixCounts         `json:"fixes"`
	Promotions    int               `json:"promotions"`
	RulesFired    map[string]int    `json:"rules_fired"`
	Scanners      []string          `json:"scanners"`
	AIReview      bool              `json:"ai_review"`
	Agents        agentCounts       `json:"agents"`
	AgentsVia     transportCounts   `json:"agents_via"`
	Unsupported   unsupported       `json:"unsupported"`
	PeriodHours   int               `json:"period_hours"`
	Demo          bool              `json:"demo"`
	SignIn        string            `json:"sign_in"`
	AgeDays       string            `json:"age_days"`
	People        windows           `json:"people"`
	PeopleTotal   string            `json:"people_total"`
	AgentSessions windows           `json:"agent_sessions"`
	AppsActive    activeApps        `json:"apps_active"`
	Deploys       envCounts         `json:"deploys"`
	Authors       authorCounts      `json:"authors"`
	Failures      map[string]int    `json:"failures"`
	Handoff       handoffCounts     `json:"handoff"`
	First         map[string]string `json:"first"`
	CustomRules   customRuleCounts  `json:"custom_rules"`
	PolicyChanged int               `json:"policy_changed"`
	AIProvider    string            `json:"ai_provider"`
	AIGateway     bool              `json:"ai_gateway"`
	RunBy         string            `json:"run_by"`
	DBSessions    dbSessionCounts   `json:"db_sessions"`
	Outbound      outboundCounts    `json:"outbound"`
	Patching      patchCounts       `json:"patching"`
	Licence       licenceState      `json:"licence"`
	ProdApproval  bool              `json:"prod_approval"`
	Approvals     decided           `json:"approvals"`
	Notify        channels          `json:"notify"`
	GitHub        bool              `json:"github"`
	GroupSync     bool              `json:"group_sync"`
	Stacks        map[string]int    `json:"stacks"`
}

type checkCounts struct {
	Passed   int `json:"passed"`
	Advisory int `json:"advisory"`
	Blocked  int `json:"blocked"`
}

type fixCounts struct {
	Count       int `json:"count"`
	MinutesP50  int `json:"minutes_p50"`
	AttemptsP50 int `json:"attempts_p50"`
}

type unsupported struct {
	LanguageUnrecognised int `json:"language_unrecognised,omitempty"`
	OwnDockerfileIgnored int `json:"own_dockerfile_ignored,omitempty"`
}

// telemetryScanners are the tools the schema names, by their rule's id: a tool a later release adds is left out
// until the schema names it.
var telemetryScanners = []string{"checkov", "eslint", "gitleaks", "grype", "opengrep", "trivy"}

// dockerfileNote is the scanners' note on a root Dockerfile (gate/scan.go), counted as own_dockerfile_ignored.
const dockerfileNote = "Canyon builds with buildpacks and does not read this Dockerfile"

// telemetryRow is the row, made at the first read, which is when the notice prints; made reports whether this
// read made it.
func telemetryRow(ctx context.Context, q querier) (id string, offBecause string, sentAt *time.Time, made bool, err error) {
	var inserted *string
	if err = q.QueryRow(ctx, "INSERT INTO telemetry DEFAULT VALUES ON CONFLICT (one) DO NOTHING RETURNING instance_id::text").Scan(&inserted); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, false, err
	}
	var off *string
	err = q.QueryRow(ctx, "SELECT instance_id::text, off_because, sent_at FROM telemetry WHERE one").Scan(&id, &off, &sentAt)
	if off != nil {
		offBecause = *off
	}
	return id, offBecause, sentAt, inserted != nil, err
}

// p50 is the lower median of xs, 0 for none.
func p50(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	slices.Sort(xs)
	return xs[(len(xs)-1)/2]
}

// firedAs is the name a rule is counted under: a built-in rule's own id — a canonical rule, or a tool Canyon runs
// itself (ranTools, the registry in code, never a tool an operator imported) — and every other rule custom, so a rule
// an org wrote or a tool it brought is never named.
func firedAs(rule string) string {
	if id, ok := gatepolicy.Canonical(rule); ok {
		return id
	}
	for tool := range ranTools {
		if rule == tool || strings.HasPrefix(rule, tool+".") {
			return tool
		}
	}
	return "custom"
}

// telemetryReportOf builds the report from the server's tables in q: the period's counts, from the previous send's
// claim (telemetrySince), the milestones ever reached, the scanners and AI review the effective policy runs, and the
// second version's fields (telemetryV2).
func (a *app) telemetryReportOf(ctx context.Context, q querier, id string, previous *time.Time) (telemetryReport, error) {
	r := telemetryReport{InstanceID: id, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, InstallMode: a.telemetry.mode,
		Milestones: []string{}, RulesFired: map[string]int{}, Scanners: []string{}, Failures: map[string]int{}, First: map[string]string{}, Stacks: map[string]int{}}
	since, hours, err := telemetrySince(ctx, q, previous)
	if err != nil {
		return r, err
	}
	r.PeriodHours = hours
	var first, block, fix bool
	err = q.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM projects WHERE deleted_at IS NULL),
		count(*) FILTER (WHERE r.state = 'clean' AND coalesce((v.data->>'warnings')::int, 0) = 0),
		count(*) FILTER (WHERE r.state = 'clean' AND coalesce((v.data->>'warnings')::int, 0) > 0),
		count(*) FILTER (WHERE r.state = 'blocked'),
		count(*) FILTER (WHERE r.state = 'failed' AND r.failure_reason = 'detect'),
		(SELECT count(*) FROM runs WHERE kind = 'promote' AND state = 'clean' AND ended_at > $1),
		EXISTS (SELECT 1 FROM runs WHERE kind = 'gate' AND state IN ('clean', 'blocked')),
		EXISTS (SELECT 1 FROM runs WHERE kind = 'gate' AND state = 'blocked'),
		EXISTS (SELECT 1 FROM runs b JOIN runs c ON c.project_id = b.project_id AND c.kind = 'gate' AND c.state = 'clean' AND c.created_at > b.created_at
			WHERE b.kind = 'gate' AND b.state = 'blocked')
		FROM runs r LEFT JOIN timeline_rows v ON v.run_id = r.id AND v.kind = 'verdict'
		WHERE r.kind = 'gate' AND r.ended_at > $1`, since).Scan(&r.Apps, &r.Checks.Passed, &r.Checks.Advisory, &r.Checks.Blocked,
		&r.Unsupported.LanguageUnrecognised, &r.Promotions, &first, &block, &fix)
	if err != nil {
		return r, err
	}
	for _, m := range []struct {
		reached bool
		name    string
	}{{first, "first_check"}, {block, "first_block"}, {fix, "first_fix"}} {
		if m.reached {
			r.Milestones = append(r.Milestones, m.name)
		}
	}
	// A fix: a clean review in the window after one or more blocked ones since the app's clean review before it.
	rows, err := q.Query(ctx, `SELECT (extract(epoch FROM c.ended_at - b.first) / 60)::int, b.n + 1 FROM runs c
		CROSS JOIN LATERAL (SELECT min(x.created_at) AS first, count(*)::int AS n FROM runs x
			WHERE x.project_id = c.project_id AND x.kind = 'gate' AND x.state = 'blocked' AND x.created_at < c.created_at
			AND x.created_at > coalesce((SELECT max(p.created_at) FROM runs p WHERE p.project_id = c.project_id AND p.kind = 'gate' AND p.state = 'clean' AND p.created_at < c.created_at), '-infinity')) b
		WHERE c.kind = 'gate' AND c.state = 'clean' AND c.ended_at > $1 AND b.n > 0`, since)
	if err != nil {
		return r, err
	}
	var minutes, attempts []int
	var m, n int
	if _, err := pgx.ForEachRow(rows, []any{&m, &n}, func() error {
		minutes, attempts = append(minutes, m), append(attempts, n)
		return nil
	}); err != nil {
		return r, err
	}
	r.Fixes = fixCounts{Count: len(minutes), MinutesP50: p50(minutes), AttemptsP50: p50(attempts)}
	rows, err = q.Query(ctx, `SELECT rule #>> '{}', count(*)::int FROM timeline_rows t JOIN runs r ON r.id = t.run_id,
		jsonb_path_query(t.data->'findings', '$.runs[*].results[*].ruleId') AS rule
		WHERE t.kind = 'verdict' AND r.ended_at > $1 GROUP BY 1`, since)
	if err != nil {
		return r, err
	}
	var rule string
	if _, err := pgx.ForEachRow(rows, []any{&rule, &n}, func() error {
		r.RulesFired[firedAs(rule)] += n
		return nil
	}); err != nil {
		return r, err
	}
	if err := q.QueryRow(ctx, `SELECT count(DISTINCT t.run_id) FROM timeline_rows t JOIN runs r ON r.id = t.run_id
		WHERE t.kind = 'producer.sarif' AND r.ended_at > $2
		AND jsonb_path_exists(t.data, '$.sarif.runs[*].invocations[*].toolExecutionNotifications[*] ? (@.message.text == $note)', jsonb_build_object('note', $1::text))`,
		dockerfileNote, since).Scan(&r.Unsupported.OwnDockerfileIgnored); err != nil {
		return r, err
	}
	policy, err := a.effectivePolicy(ctx, q)
	if err != nil {
		return r, err
	}
	for _, rule := range policy.Rules {
		if tool := strings.TrimPrefix(rule.ID, "sarif."); rule.Run && slices.Contains(telemetryScanners, tool) {
			r.Scanners = append(r.Scanners, tool)
		}
		r.AIReview = r.AIReview || a.reviews && rule.Producer == gatepolicy.LLMReview && rule.On() // it runs only with a model
	}
	slices.Sort(r.Scanners)
	return r, a.telemetryV2(ctx, q, &r, since, policy)
}

// telemetryState is the schema's state: whether the next report goes, why not, and each report as a string of the
// send's exact bytes, which a reader shows as it is and never re-writes (contracts/routes.md, Telemetry).
type telemetryState struct {
	Enabled    bool    `json:"enabled"`
	OffBecause string  `json:"off_because,omitempty"`
	Report     string  `json:"report"`
	LastSentAt *string `json:"last_sent_at"`
	LastReport *string `json:"last_report"` // nil marshals as null: none sent yet
}

// offBecause is the first reason the report is not sent: the environment, then an admin or an unreachable first send.
func (a *app) offBecause(stored string) string {
	if a.telemetry.envOff {
		return "env"
	}
	return stored
}

// telemetryStateOf reads the row and builds the report in q.
func (a *app) telemetryStateOf(ctx context.Context, q querier) (telemetryState, error) {
	id, stored, sentAt, _, err := telemetryRow(ctx, q)
	if err != nil {
		return telemetryState{}, err
	}
	var last *string
	var claimed *time.Time // the next send's period starts at the last claim
	if err := q.QueryRow(ctx, "SELECT last_report, claimed_at FROM telemetry WHERE one").Scan(&last, &claimed); err != nil {
		return telemetryState{}, err
	}
	report, err := a.telemetryReportOf(ctx, q, id, claimed)
	if err != nil {
		return telemetryState{}, err
	}
	body, err := json.Marshal(report)
	s := telemetryState{OffBecause: a.offBecause(stored), Report: string(body), LastReport: last}
	s.Enabled = s.OffBecause == ""
	if sentAt != nil {
		at := sentAt.UTC().Format(time.RFC3339)
		s.LastSentAt = &at
	}
	return s, err
}

// sendTelemetry is the day's send, at most one across replicas: the replica that claims the day builds the report
// and POSTs it once, in telemetrySend, with no retry and no log line. A first send that reaches nothing turns
// telemetry off (an air-gapped install); a later one that fails is that day's alone. sent reports a POST was made.
func (a *app) sendTelemetry(ctx context.Context) (sent bool, err error) {
	if a.telemetry.envOff || a.telemetry.url == "" {
		return false, nil
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, _, sentAt, _, err := telemetryRow(ctx, tx)
	if err != nil {
		return false, err
	}
	var previous *time.Time // the claim before this one, where this report's period starts
	if err := tx.QueryRow(ctx, `UPDATE telemetry t SET claimed_at = now() FROM (SELECT claimed_at FROM telemetry WHERE one) was WHERE t.one AND t.off_because IS NULL
		AND (t.claimed_at IS NULL OR t.claimed_at < now() - interval '`+telemetryWhen+`') RETURNING was.claimed_at`).Scan(&previous); errors.Is(err, pgx.ErrNoRows) {
		return false, nil // off, or this day already claimed
	} else if err != nil {
		return false, err
	}
	report, err := a.telemetryReportOf(ctx, tx, id, previous)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil { // the day is claimed before the POST: a crash mid-send never sends twice
		return false, err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return false, err
	}
	reached, err := telemetryPost(ctx, a.telemetry.url, body)
	if err != nil {
		return false, err
	}
	switch {
	case reached:
		_, err = a.pool.Exec(ctx, "UPDATE telemetry SET sent_at = now(), last_report = $1 WHERE one", string(body))
	case sentAt == nil: // the first send reached nothing: an air-gapped install, off until an admin turns it on
		_, err = a.pool.Exec(ctx, "UPDATE telemetry SET off_because = 'unreachable' WHERE one AND sent_at IS NULL")
	default:
		err = nil // a later day's failure is that day's: no retry, no log
	}
	return true, err
}

// telemetryClient follows no redirect: Go would POST the body again to wherever a 307 or 308 points, and a 3xx is no
// answer of the collector's, so it counts as unreached (#2807's review).
var telemetryClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// telemetryPost is the one try to send body to url, in telemetrySend: reached only when the collector itself answered
// 2xx. err is a request that could not be made.
func telemetryPost(ctx context.Context, url string, body []byte) (reached bool, err error) {
	post, cancel := context.WithTimeout(ctx, telemetrySend)
	defer cancel()
	req, err := http.NewRequestWithContext(post, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := telemetryClient.Do(req) //nolint:gosec // the collector's constant URL, never a request's
	if err != nil {
		return false, nil
	}
	_ = resp.Body.Close()
	return resp.StatusCode/100 == 2, nil // a proxy's own 502 for a collector it can't reach is no send
}

// telemetryLoop prints the notice the first time this server's row is made, then looks for the day's send every
// a.telemetry.every until ctx is done. Neither a failed send nor a failed read is logged: nothing is sent, which is
// the failure's whole effect.
func (a *app) telemetryLoop(ctx context.Context, logger *log.Logger) {
	if _, _, _, made, err := telemetryRow(ctx, a.pool); err == nil && made && !a.telemetry.envOff {
		logger.Print("canyon-server: " + telemetryNotice)
	}
	tick := time.NewTicker(a.telemetry.every)
	defer tick.Stop()
	for {
		_, _ = a.sendTelemetry(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
