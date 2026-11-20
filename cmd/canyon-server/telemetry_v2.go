package main

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// The usage report's second version (#2953; contracts/schema/telemetry.schema.json): the install's profile, its people,
// its deploys and failures, how long each first took, the features it uses, and its apps' agents and stacks. Every
// field is a count, a bucket, an enum or a boolean read from the server's own tables or environment; none names a
// company or a person.

// harnesses are the coding agents a review is kept under (runs.harness), as the CLI's Canyon-Harness names them.
var harnesses = []string{"claude_code", "codex"}

// transportMCPLocal is canyon mcp's Canyon-Transport, kept on a review it starts (runs.transport); a review the host's
// own /mcp starts is kept as mcp-remote, and one with neither is the CLI's (design/specs/mcp.md §7).
const transportMCPLocal = "mcp-local"

// telemetryPeriodMost is the longest period one report's counts cover: a server that sent nothing for longer, off or
// unreachable, reports its last week, so one report never lands months of counts in one week's sums.
const telemetryPeriodMost = 7 * 24 * time.Hour

type windows struct {
	D1  int `json:"d1"`
	D7  int `json:"d7"`
	D28 int `json:"d28"`
}

type activeApps struct {
	D7  int `json:"d7"`
	D28 int `json:"d28"`
}

type envCounts struct {
	Preview int `json:"preview"`
	Prod    int `json:"prod"`
}

type authorCounts struct {
	Person int `json:"person"`
	Agent  int `json:"agent"`
}

type handoffCounts struct {
	Deployed    int `json:"deployed"`
	Failed      int `json:"failed"`
	Refused     int `json:"refused"`
	Undelivered int `json:"undelivered"`
}

type customRuleCounts struct {
	AI   int `json:"ai"`
	Code int `json:"code"`
}

type dbSessionCounts struct {
	Setting string `json:"setting"`
	Prod    string `json:"prod"`
	Read    int    `json:"read"`
	Write   int    `json:"write"`
}

type outboundCounts struct {
	Via              string `json:"via"`
	AllowlistApps    int    `json:"allowlist_apps"`
	CredentialRoutes bool   `json:"credential_routes"`
	Routes           string `json:"routes"`
	Refusals         int    `json:"refusals"`
}

type patchCounts struct {
	Clean           int `json:"clean"`
	Regressed       int `json:"regressed"`
	Ineffective     int `json:"ineffective"`
	Reverted        int `json:"reverted"`
	NotSameInputs   int `json:"not_same_inputs"`
	NotPlatformOnly int `json:"not_platform_only"`
	Paused          int `json:"paused"`
}

type licenceState struct {
	State string `json:"state"`
}

type decided struct {
	Approved int `json:"approved"`
	Denied   int `json:"denied"`
}

type channels struct {
	Slack int `json:"slack"`
	Teams int `json:"teams"`
}

type agentCounts struct {
	ClaudeCode int `json:"claude_code"`
	Codex      int `json:"codex"`
	Other      int `json:"other"`
}

// transportCounts are the period's reviews by how they were asked for (runs.transport): the CLI, canyon mcp, or the
// host's own /mcp.
type transportCounts struct {
	CLI       int `json:"cli"`
	MCPLocal  int `json:"mcp_local"`
	MCPRemote int `json:"mcp_remote"`
}

// bucketOf is a size as the schema buckets it (definitions.bucket).
func bucketOf(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 5:
		return "2-5"
	case n <= 25:
		return "6-25"
	case n <= 100:
		return "26-100"
	}
	return "101+"
}

// durOf is a duration as the schema buckets it (definitions.dur).
func durOf(d time.Duration) string {
	switch {
	case d < 10*time.Minute:
		return "lt_10m"
	case d < time.Hour:
		return "10m_1h"
	case d < 24*time.Hour:
		return "1h_1d"
	case d < 7*24*time.Hour:
		return "1d_7d"
	}
	return "gt_7d"
}

// ageOf is an install's age as age_days buckets it.
func ageOf(d time.Duration) string {
	switch days := int(d / (24 * time.Hour)); {
	case days < 1:
		return "0"
	case days < 7:
		return "1-6"
	case days < 28:
		return "7-27"
	case days < 90:
		return "28-89"
	}
	return "90+"
}

// failureKey is the failures key a failed run counts under: its reason, Canyon's own error by the step it was in, and
// "" for one the schema doesn't name (a patch run's, counted under patching).
func failureKey(reason, in string) string {
	switch reason {
	case "detect", "build", "start", "no_room", "scan_no_room", "model", "interrupted", "image_gone":
		return reason
	case "error":
		if in == "" {
			return "error"
		}
		return "error." + in
	}
	return ""
}

// stackBuildpacks name an app's language by the buildpack that brings it, an exact table of the ids Canyon's builders
// run (contracts/executor.md, The buildpacks that ran), never a guess at an id's words.
var stackBuildpacks = map[string]string{
	"paketo-buildpacks/node-engine": "node", "paketo-buildpacks/nodejs": "node",
	"paketo-buildpacks/cpython": "python", "paketo-buildpacks/python": "python",
	"paketo-buildpacks/go-dist": "go", "paketo-buildpacks/go": "go",
	"paketo-buildpacks/bellsoft-liberica": "java", "paketo-buildpacks/java": "java", "paketo-buildpacks/java-native-image": "java",
	"paketo-buildpacks/mri": "ruby", "paketo-buildpacks/ruby": "ruby",
	"paketo-buildpacks/php-dist": "php", "paketo-buildpacks/php": "php",
	"paketo-buildpacks/dotnet-core": "dotnet", "paketo-buildpacks/dotnet-core-runtime": "dotnet", "paketo-buildpacks/dotnet-core-sdk": "dotnet",
	"paketo-buildpacks/nginx": "static", "paketo-buildpacks/httpd": "static", "paketo-buildpacks/web-servers": "static",
}

// stackOf is an app's language by the buildpacks its latest build ran: the first the table knows, else other.
func stackOf(ids []string) string {
	for _, id := range ids {
		if s, ok := stackBuildpacks[id]; ok {
			return s
		}
	}
	return "other"
}

// telemetrySince is where the period's counts start: the previous send's claim, at most telemetryPeriodMost ago, and
// 24 hours ago before the first; hours is how long that is, rounded to the hour.
func telemetrySince(ctx context.Context, q querier, previous *time.Time) (since time.Time, hours int, err error) {
	var now time.Time
	if err := q.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return since, 0, err
	}
	since = now.Add(-24 * time.Hour)
	if previous != nil {
		since = *previous
	}
	since = maxTime(since, now.Add(-telemetryPeriodMost))
	return since, int(now.Sub(since).Round(time.Hour) / time.Hour), nil
}

// telemetryV2 adds the second version's fields to r, reading the period from since.
func (a *app) telemetryV2(ctx context.Context, q querier, r *telemetryReport, since time.Time, policy gatepolicy.Snapshot) error {
	r.Demo = a.telemetry.demo
	r.SignIn = map[bool]string{true: "oidc", false: "off"}[a.auth == authOIDC]
	r.AIProvider, r.AIGateway = "none", a.telemetry.gateway
	if r.AIReview {
		r.AIProvider = cmp.Or(a.telemetry.provider, "anthropic") // unset is the default provider
	}
	var start *time.Time // the install's start: setup finished, else its first app
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT coalesce(o.onboarded_at, (SELECT min(created_at) FROM projects)), now(), o.run_by, o.db_sessions, o.db_prod, o.prod_approval
		FROM org_preferences o`).Scan(&start, &now, &r.RunBy, &r.DBSessions.Setting, &r.DBSessions.Prod, &r.ProdApproval); err != nil {
		return err
	}
	if start == nil {
		start = &now
	}
	r.AgeDays = ageOf(now.Sub(*start))
	// People and agents active, from their sign-ins, their sessions' last requests, what they changed and the reviews
	// they started; a person by their subject.
	if err := q.QueryRow(ctx, `WITH seen AS (
			SELECT actor_kind AS kind, actor_subject AS who, id AS session, greatest(created_at, coalesce(last_seen_at, created_at)) AS at FROM sessions WHERE actor_kind IN ('person', 'agent')
			UNION ALL SELECT 'person', actor_subject, NULL, ts FROM audit_events WHERE actor_kind = 'person'
			UNION ALL SELECT 'person', started_by_subject, NULL, created_at FROM runs WHERE started_by_kind = 'person')
		SELECT count(DISTINCT who) FILTER (WHERE kind = 'person' AND at > now() - interval '1 day'),
			count(DISTINCT who) FILTER (WHERE kind = 'person' AND at > now() - interval '7 days'),
			count(DISTINCT who) FILTER (WHERE kind = 'person' AND at > now() - interval '28 days'),
			count(DISTINCT session) FILTER (WHERE kind = 'agent' AND at > now() - interval '1 day'),
			count(DISTINCT session) FILTER (WHERE kind = 'agent' AND at > now() - interval '7 days'),
			count(DISTINCT session) FILTER (WHERE kind = 'agent' AND at > now() - interval '28 days')
		FROM seen`).Scan(&r.People.D1, &r.People.D7, &r.People.D28, &r.AgentSessions.D1, &r.AgentSessions.D7, &r.AgentSessions.D28); err != nil {
		return err
	}
	var people, routes int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM (SELECT lower(email) FROM sessions WHERE actor_kind = 'person' AND email <> '' AND revoked_at IS NULL AND expires_at > now()
		UNION SELECT grantee FROM project_access WHERE kind = 'email') e`).Scan(&people); err != nil {
		return err
	}
	r.PeopleTotal = bucketOf(people)
	// The period's deploys, its reviews by who asked for them (runs.author_kind is a patch run's alone), by agent and by
	// how they were asked for, and failures; apps active in 7 and 28 days.
	if err := q.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind = 'gate' AND deploy_started_at > $1),
		count(*) FILTER (WHERE kind = 'promote' AND deploy_started_at > $1),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND started_by_kind = 'person'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND started_by_kind = 'agent'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND harness = 'claude_code'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND harness = 'codex'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND harness IS NULL AND started_by_kind = 'agent'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND transport IS NULL),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND transport = 'mcp-local'),
		count(*) FILTER (WHERE kind = 'gate' AND state IN ('clean', 'blocked') AND ended_at > $1 AND transport = 'mcp-remote'),
		count(DISTINCT project_id) FILTER (WHERE kind = 'gate' AND created_at > now() - interval '7 days'),
		count(DISTINCT project_id) FILTER (WHERE kind = 'gate' AND created_at > now() - interval '28 days')
		FROM runs`, since).Scan(&r.Deploys.Preview, &r.Deploys.Prod, &r.Authors.Person, &r.Authors.Agent,
		&r.Agents.ClaudeCode, &r.Agents.Codex, &r.Agents.Other, &r.AgentsVia.CLI, &r.AgentsVia.MCPLocal, &r.AgentsVia.MCPRemote,
		&r.AppsActive.D7, &r.AppsActive.D28); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT failure_reason, coalesce(failed_in, ''), count(*)::int FROM runs
		WHERE state = 'failed' AND kind <> 'patch' AND ended_at > $1 GROUP BY 1, 2`, since)
	if err != nil {
		return err
	}
	var reason, in string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&reason, &in, &n}, func() error {
		if k := failureKey(reason, in); k != "" {
			r.Failures[k] += n
		}
		return nil
	}); err != nil {
		return err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'deployed'), count(*) FILTER (WHERE state = 'failed'),
		count(*) FILTER (WHERE state = 'refused'), count(*) FILTER (WHERE state = 'undelivered') FROM releases WHERE created_at > $1`, since).
		Scan(&r.Handoff.Deployed, &r.Handoff.Failed, &r.Handoff.Refused, &r.Handoff.Undelivered); err != nil {
		return err
	}
	// How long after the install's start each first came.
	var check, block, fix, prod *time.Time
	if err := q.QueryRow(ctx, `SELECT
		(SELECT min(ended_at) FROM runs WHERE kind = 'gate' AND state IN ('clean', 'blocked')),
		(SELECT min(ended_at) FROM runs WHERE kind = 'gate' AND state = 'blocked'),
		(SELECT min(c.ended_at) FROM runs b JOIN runs c ON c.project_id = b.project_id AND c.kind = 'gate' AND c.state = 'clean' AND c.created_at > b.created_at
			WHERE b.kind = 'gate' AND b.state = 'blocked'),
		(SELECT min(ended_at) FROM runs WHERE kind = 'promote' AND state = 'clean')`).Scan(&check, &block, &fix, &prod); err != nil {
		return err
	}
	for key, at := range map[string]*time.Time{"check": check, "block": block, "fix": fix, "prod": prod} {
		if at != nil {
			r.First[key] = durOf(max(at.Sub(*start), 0))
		}
	}
	// Features: the policy's custom rules and changed built-ins, database sessions, outbound, patching, approvals,
	// channels, GitHub, group sync and the licence.
	boot := map[string]gatepolicy.Rule{}
	for _, rule := range a.boot.Rules {
		boot[rule.ID] = rule
	}
	for _, rule := range policy.Rules {
		switch was, builtin := boot[rule.ID]; {
		case strings.HasPrefix(rule.ID, "custom.") && rule.On() && rule.Check != nil && rule.Check.By == "code":
			r.CustomRules.Code++
		case strings.HasPrefix(rule.ID, "custom.") && rule.On():
			r.CustomRules.AI++
		case builtin && (rule.State != was.State || rule.Production != was.Production):
			r.PolicyChanged++
		}
	}
	if err := q.QueryRow(ctx, `SELECT
		(SELECT count(*) FILTER (WHERE mode = 'read') FROM db_sessions WHERE opened_at > $1),
		(SELECT count(*) FILTER (WHERE mode = 'write') FROM db_sessions WHERE opened_at > $1),
		coalesce((SELECT via FROM egress_settings), 'off'), coalesce((SELECT credential_routes FROM egress_settings), false),
		(SELECT count(DISTINCT project_id) FROM egress_modes WHERE mode = 'allowlist'),
		(SELECT count(*) FROM egress_routes),
		(SELECT count(*) FROM egress_requests WHERE last_refused_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND state = 'clean' AND ended_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND failure_reason = 'patch_regressed' AND ended_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND failure_reason = 'patch_ineffective' AND ended_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND failure_reason = 'reverted' AND ended_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND failure_reason = 'not_same_inputs' AND ended_at > $1),
		(SELECT count(*) FROM runs WHERE kind = 'patch' AND failure_reason = 'not_platform_only' AND ended_at > $1),
		(SELECT count(*) FROM patch_identities WHERE paused_at IS NOT NULL),
		(SELECT count(*) FROM promotion_approvals WHERE state = 'approved' AND decided_at > $1),
		(SELECT count(*) FROM promotion_approvals WHERE state = 'denied' AND decided_at > $1),
		(SELECT count(*) FROM notify_channels WHERE kind = 'slack'), (SELECT count(*) FROM notify_channels WHERE kind = 'teams'),
		EXISTS (SELECT 1 FROM github_installations WHERE deleted_at IS NULL)`, since).Scan(&r.DBSessions.Read, &r.DBSessions.Write,
		&r.Outbound.Via, &r.Outbound.CredentialRoutes, &r.Outbound.AllowlistApps, &routes, &r.Outbound.Refusals,
		&r.Patching.Clean, &r.Patching.Regressed, &r.Patching.Ineffective, &r.Patching.Reverted, &r.Patching.NotSameInputs, &r.Patching.NotPlatformOnly, &r.Patching.Paused,
		&r.Approvals.Approved, &r.Approvals.Denied, &r.Notify.Slack, &r.Notify.Teams, &r.GitHub); err != nil {
		return err
	}
	r.Outbound.Routes = bucketOf(routes)
	o, err := a.orgOf(ctx, q) // the claim as sign-in reads it: CANYON_GROUPS_CLAIM over the row's
	if err != nil {
		return err
	}
	r.GroupSync = o.groupsClaim != ""
	lic, err := a.ee.read(ctx, q)
	if err != nil {
		return err
	}
	r.Licence.State = lic.State
	// Stacks: each app's language from its latest build's buildpacks.
	rows, err = q.Query(ctx, `SELECT DISTINCT ON (r.project_id) coalesce((SELECT array_agg(b->>'id') FROM jsonb_array_elements(CASE WHEN jsonb_typeof(r.buildpacks) = 'array' THEN r.buildpacks ELSE '[]'::jsonb END) b), '{}')
		FROM runs r JOIN projects p ON p.id = r.project_id AND p.deleted_at IS NULL WHERE r.buildpacks IS NOT NULL ORDER BY r.project_id, r.created_at DESC`)
	if err != nil {
		return err
	}
	var ids []string
	if _, err := pgx.ForEachRow(rows, []any{&ids}, func() error {
		r.Stacks[stackOf(ids)]++
		return nil
	}); err != nil {
		return err
	}
	return nil
}
