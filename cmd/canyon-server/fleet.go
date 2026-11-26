package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/runs"
)

// fleetDerivation is the one derivation behind the fleet read (contracts/routes.md row 75 and
// "The package's stage"): every live project with its newest package, the newest run of that
// package, the newest run of any package (gate_state), its owners, its latest verdict and its
// environments — a pull request's or a check's package, run and verdict never among them (its GitHub check or the CLI reports it). The stage is the furthest point the newest package reached: an environment
// running it wins over any run state, so a re-run never walks it back; among verdicts the newest
// run wins. since is read off the row that entered the stage; rank is the risk order. On the verdict stages it is the run's ended_at,
// which Pipeline.End and supersede write with the terminal state in one transaction (a null would 500 the page and drop the row from
// every cursor page, so it falls back to created_at). build_at names scope = 'run' so the run index applies. $1 to $4 are the
// caller (callerArgs): level is theirs on each app, and team whether they read it whole — an admin, or contributor and above; a
// row they read reduced sorts after every other, by id alone (rank 8, since the epoch), so its place says nothing its fields do not.
// Who it is shared with: everyone, people (email grants below owner) and group_grants; requests is answered to its owners and admins only.
var fleetDerivation = `WITH live AS (SELECT id, name, stack, created_at, ` + callerLevel + ` AS level,
          ` + runs.Shared("p.id") + `,
          (SELECT count(*) FROM access_requests q WHERE q.project_id = p.id) AS requests,
          ` + openToClaim + ` AS open_to_claim
        FROM projects p WHERE deleted_at IS NULL),
pkg AS (SELECT DISTINCT ON (project_id) project_id, id, label, created_at, description FROM packages WHERE source_kind NOT IN ('repo-pr', 'check')
        ORDER BY project_id, created_at DESC, id DESC),
own_runs AS (SELECT r.* FROM runs r JOIN packages p ON p.id = r.package_id AND p.project_id = r.project_id AND p.source_kind NOT IN ('repo-pr', 'check') WHERE r.kind = 'gate' AND r.rollback_to IS NULL),
run AS (SELECT DISTINCT ON (project_id) project_id, id, state FROM own_runs
        ORDER BY project_id, created_at DESC, id DESC),
pkgrun AS (SELECT DISTINCT ON (r.project_id) r.project_id, r.id, r.state, r.created_at, r.ended_at, r.kind, r.started_by_kind, r.started_by_subject, r.started_by_github, r.author_kind, r.author_subject, r.author_github
        FROM runs r JOIN pkg ON pkg.project_id = r.project_id AND r.package_id = pkg.id WHERE r.kind = 'gate' AND r.rollback_to IS NULL
        ORDER BY r.project_id, r.created_at DESC, r.id DESC),
own AS (SELECT project_id, count(*) AS n,
        jsonb_agg(jsonb_build_object('kind', 'person', 'subject', subject, 'display', COALESCE(NULLIF(display, ''), grantee)) ORDER BY granted_at, grantee) AS list
        FROM project_access WHERE kind = 'email' AND level = 'owner' GROUP BY project_id),
-- the latest review's verdict, with its run's author, against whom an approval of its findings is read (runs.CoversBlocking)
verdict AS (SELECT DISTINCT ON (t.project_id) t.project_id, t.data, r.kind, r.started_by_kind, r.started_by_subject, r.started_by_github,
          r.author_kind, r.author_subject, r.author_github
        FROM timeline_rows t JOIN own_runs r ON r.id = t.run_id
        WHERE t.kind = 'verdict' ORDER BY t.project_id, t.id DESC),
base AS (
  SELECT l.id, l.name, l.stack, l.created_at, pkg.id AS package, pkg.label, pkg.created_at AS package_at, COALESCE(pkg.description, '') AS summary,
    l.level, $4::bool OR l.level IN ('owner', 'contributor') AS team, l.everyone, l.people, l.group_grants,
    CASE WHEN $4::bool OR l.level = 'owner' THEN l.requests END AS requests, $4::bool OR l.level = 'owner' OR l.open_to_claim AS claimable,
    run.id AS last_run, COALESCE(run.state, 'never_reviewed') AS gate_state,
    -- scope = 'run' so the run's index applies: without it, once an app, every verdict of the fleet is read (#87)
    COALESCE((SELECT (t.data->>'warnings')::int FROM timeline_rows t WHERE t.scope = 'run' AND t.run_id = run.id AND t.kind = 'verdict'), 0) AS warnings,
    COALESCE(o.n, 0) AS owners_n, COALESCE(o.list, '[]'::jsonb) AS owners,
    (SELECT COALESCE(array_agg(grantee ORDER BY grantee), '{}') FROM project_access g WHERE g.project_id = l.id AND g.kind = 'group' AND g.level = 'owner') AS owner_groups,
    COALESCE(jsonb_path_exists(v.data,
      '$.findings.runs[*].results[*] ? (@.ruleId starts with "secrets." && @.properties.canyon.blocks == true)'), false) AS secrets,
    -- whether one of those no unexpired approval covers, whatever the run's state: an approved secret is a person's decision,
    -- no need (contracts/routes.md, *What needs a person*, secret)
    COALESCE((SELECT bool_or(a.rule IS NULL) FROM jsonb_path_query(v.data,
        '$.findings.runs[*].results[*] ? (@.ruleId starts with "secrets." && @.properties.canyon.blocks == true)') f(finding)
      LEFT JOIN approvals a ON a.project_id = l.id AND a.rule = f.finding->>'ruleId'
        AND a.fingerprint = f.finding->'partialFingerprints'->>'canyon/v1' AND ` + runs.CoversBlocking("v", "now()") + `), false) AS secrets_open,
    pv.package_id AS preview_package, pvp.label AS preview_label, split_part(pv.image, '@', 2) AS preview_digest, pv.url AS preview_url,
    pv.deployed_at AS preview_at, pv.gate_run AS preview_run,
    CASE WHEN pv.deployed_by_subject IS NULL THEN 'null'::jsonb ELSE jsonb_build_object('kind', pv.deployed_by_kind, 'subject', pv.deployed_by_subject, 'display', pv.deployed_by_display) END AS preview_by,
    pv.run_by AS preview_run_by, CASE WHEN pvr.state IN ('sent', 'accepted', 'undelivered', 'refused') AND (pv.deployed_at IS NULL OR pvr.created_at > pv.deployed_at) THEN jsonb_build_object('release', pvr.id, 'event', pvr.event, 'sent_at', to_char(pvr.sent_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'state', pvr.state, 'via', pvr.via) || ` + pendingMissing("pvr") + ` || ` + pendingPull("pvr") + ` ELSE 'null'::jsonb END AS preview_pending,
    CASE WHEN pvp.source_kind = 'repo-pr' THEN jsonb_build_object('number', pvp.pr_number, 'branch', pvp.pr_branch) ELSE 'null'::jsonb END AS preview_pr,
    pd.package_id AS prod_package, pdp.label AS prod_label, split_part(pd.image, '@', 2) AS prod_digest, pd.url AS prod_url,
    pd.deployed_at AS prod_at, pd.gate_run AS prod_run,
    CASE WHEN pd.deployed_by_subject IS NULL THEN 'null'::jsonb ELSE jsonb_build_object('kind', pd.deployed_by_kind, 'subject', pd.deployed_by_subject, 'display', pd.deployed_by_display) END AS prod_by,
    CASE WHEN pd.promoted_by_subject IS NULL THEN 'null'::jsonb ELSE jsonb_build_object('kind', pd.promoted_by_kind, 'subject', pd.promoted_by_subject, 'display', pd.promoted_by_display) END AS prod_promoted_by,
    pd.run_by AS prod_run_by, CASE WHEN pdr.state IN ('sent', 'accepted', 'undelivered', 'refused') AND (pd.deployed_at IS NULL OR pdr.created_at > pd.deployed_at) THEN jsonb_build_object('release', pdr.id, 'event', pdr.event, 'sent_at', to_char(pdr.sent_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'state', pdr.state, 'via', pdr.via) || ` + pendingMissing("pdr") + ` || ` + pendingPull("pdr") + ` ELSE 'null'::jsonb END AS prod_pending,
    pkgrun.state AS run_state, pkgrun.created_at AS run_at, pkgrun.ended_at AS run_end, ap.at AS approved_at, ap.open AS open_blocking,
    -- production's hold (contracts/routes.md, The hold), its version named as *Naming a version* says
    CASE WHEN h.project_id IS NOT NULL THEN jsonb_build_object('by', jsonb_build_object('kind', h.held_by_kind, 'subject', h.held_by_subject, 'display', h.held_by_display),
      'at', to_char(h.held_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'reason', h.reason, 'version', coalesce(hp.label, substr(h.package_id, 5))) END AS hold,
    (SELECT min(ts) FROM timeline_rows b WHERE b.scope = 'run' AND b.run_id = pkgrun.id AND b.kind = 'build.log') AS build_at
  FROM live l
  LEFT JOIN pkg ON pkg.project_id = l.id
  LEFT JOIN run ON run.project_id = l.id
  LEFT JOIN pkgrun ON pkgrun.project_id = l.id
  -- D1: the newest approval when every blocking finding of the package's run carries an unexpired one, matched as verdictSQL matches it;
  -- and the findings none covers, each its rule and its problem, which what needs a person reads (needs.go)
  LEFT JOIN LATERAL (SELECT CASE WHEN count(*) > 0 AND count(a.rule) = count(*) THEN max(a.approved_at) END AS at,
    coalesce(jsonb_agg(jsonb_build_object('rule', f.finding->>'ruleId', 'problem', coalesce(f.finding->'properties'->'canyon'->>'problem', ''))) FILTER (WHERE a.rule IS NULL), '[]'::jsonb) AS open
    FROM timeline_rows t CROSS JOIN LATERAL jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (!exists(@.properties.canyon.blocks) || @.properties.canyon.blocks != false)') f(finding)
    LEFT JOIN approvals a ON a.project_id = l.id AND a.rule = f.finding->>'ruleId'
      AND a.fingerprint = f.finding->'partialFingerprints'->>'canyon/v1' AND ` + runs.CoversBlocking("pkgrun", "now()") + `
    WHERE t.scope = 'run' AND t.run_id = pkgrun.id AND t.kind = 'verdict' AND pkgrun.state = 'blocked') ap ON true
  LEFT JOIN own o ON o.project_id = l.id
  LEFT JOIN verdict v ON v.project_id = l.id
  LEFT JOIN ` + runs.ShownDeploys + ` pv ON pv.project_id = l.id AND pv.env = 'preview'
  LEFT JOIN ` + runs.ShownDeploys + ` pd ON pd.project_id = l.id AND pd.env = 'prod'
  -- the environment's newest release, the one it waits on while open (contracts/routes.md, envs.<env>.pending)
  LEFT JOIN LATERAL (SELECT id, event, sent_at, state, created_at, missing_resources, via, pull_request, pull_number FROM releases r WHERE r.project_id = l.id AND r.env = 'preview' ORDER BY created_at DESC, id DESC LIMIT 1) pvr ON true
  LEFT JOIN LATERAL (SELECT id, event, sent_at, state, created_at, missing_resources, via, pull_request, pull_number FROM releases r WHERE r.project_id = l.id AND r.env = 'prod' ORDER BY created_at DESC, id DESC LIMIT 1) pdr ON true
  -- an environment is named by the label of the package it runs, which is not always the newest one (contracts/routes.md, *Naming a version*)
  LEFT JOIN packages pvp ON pvp.project_id = l.id AND pvp.id = pv.package_id
  LEFT JOIN packages pdp ON pdp.project_id = l.id AND pdp.id = pd.package_id
  LEFT JOIN prod_holds h ON h.project_id = l.id
  LEFT JOIN packages hp ON hp.project_id = l.id AND hp.id = h.package_id),
staged AS (
  SELECT *, CASE
    WHEN package IS NULL THEN 'created'
    WHEN prod_package = package THEN 'production'
    WHEN preview_package = package THEN 'preview'
    WHEN run_state IN ('pending', 'running') AND build_at IS NOT NULL THEN 'building'
    WHEN run_state IN ('pending', 'running') THEN 'reviewing'
    WHEN run_state = 'clean' THEN 'passed'
    WHEN run_state = 'blocked' AND approved_at IS NOT NULL THEN 'approved'
    WHEN run_state IN ('blocked', 'failed') THEN run_state
    ELSE 'awaiting_review' END AS stage
  FROM base),
fleet AS (
  SELECT *,
    CASE WHEN NOT team THEN 'epoch' ELSE CASE stage WHEN 'created' THEN created_at WHEN 'awaiting_review' THEN package_at WHEN 'reviewing' THEN run_at
      WHEN 'building' THEN build_at WHEN 'preview' THEN preview_at WHEN 'production' THEN prod_at
      WHEN 'approved' THEN GREATEST(run_end, approved_at) ELSE COALESCE(run_end, run_at) END END AS since,
    CASE WHEN NOT team THEN 8 ELSE CASE stage WHEN 'blocked' THEN 0 WHEN 'failed' THEN 1 WHEN 'approved' THEN 2 WHEN 'awaiting_review' THEN 3
      WHEN 'created' THEN 3 WHEN 'reviewing' THEN 4 WHEN 'building' THEN 4 WHEN 'passed' THEN 5 WHEN 'preview' THEN 6 ELSE 7 END END AS rank
  FROM staged)
`

// pendingMissing is what an open deploy release's pending adds while the platform's last deployed report on it
// left out resources it lists (contracts/routes.md, envs.<env>.pending.missing; #2452): the names, else nothing.
func pendingMissing(r string) string {
	return `CASE WHEN ` + r + `.missing_resources IS NOT NULL AND ` + r + `.event = 'deploy' AND ` + r + `.state IN ('sent', 'accepted')
      THEN jsonb_build_object('missing', to_jsonb(` + r + `.missing_resources)) ELSE '{}'::jsonb END`
}

// pendingPull is what an open delivery: repo release's pending adds while it waits on the pull request it opened in
// the deploy repo (contracts/routes.md, envs.<env>.pending.pull_request; #2470): its link and number, else nothing.
// pending is technical detail, so the reduced row, a viewer's, never carries it.
func pendingPull(r string) string {
	return `CASE WHEN ` + r + `.via = 'repo' AND ` + r + `.pull_request IS NOT NULL AND ` + r + `.state IN ('sent', 'accepted')
      THEN jsonb_build_object('pull_request', jsonb_build_object('url', ` + r + `.pull_request, 'number', ` + r + `.pull_number)) ELSE '{}'::jsonb END`
}

// seenOf is an environment's check-in (contracts/routes.md, envs.<env>.seen_at; #2452): the seen_at of the
// deploy release that put it live — the newest of the environment's created by the time its deploy row was
// written — read here, past the derivation, so the count and the tiles never pay for it.
func seenOf(env, at string) string {
	return `(SELECT r.seen_at FROM releases r WHERE r.project_id = fleet.id AND r.env = '` + env + `' AND r.event = 'deploy' AND r.created_at <= fleet.` + at + `
    ORDER BY r.created_at DESC, r.id DESC LIMIT 1)`
}

// fleetRowSQL selects the one row shape every project read answers — the fleet page, the project
// list and the single project read each add their own WHERE and ORDER BY and scan with scanFleetRow.
var fleetRowSQL = fleetDerivation + `SELECT id, name, stack, owners, owner_groups, summary, gate_state, last_run, warnings, package, label, stage, since, rank, level, team, everyone, people, group_grants, requests, claimable,
  preview_package, preview_label, preview_digest, preview_url, preview_at, preview_run, preview_by, preview_run_by, preview_pending, preview_pr, ` + seenOf("preview", "preview_at") + `,
  prod_package, prod_label, prod_digest, prod_url, prod_at, prod_run, prod_by, prod_promoted_by, prod_run_by, prod_pending, ` + seenOf("prod", "prod_at") + `, hold
FROM fleet
`

// fleetWhere selects the rows of one fleet read: each filter is a guard that passes when its parameter is empty, stack and q
// literal case-insensitive substrings (strpos, never LIKE), $11 needs_you — null off, else the apps whose rows carry a need
// (needs.go): the rows read whole, and a responder's held rows — $13 "Your apps" — the caller's own grants at contributor or above,
// $14 the Held filter: every held row for a responder or an admin ("all"), the held rows read whole for anyone else ("team"),
// $15 the apps whose rows carry a refused need (needs.go): the refused tile's, so the tile and its filter never differ.
// q and owner match every row; the other filters only the rows the caller reads whole, so a reduced row says nothing by matching.
const fleetWhere = `WHERE ($5::text = '' OR team AND CASE $5 WHEN 'secrets_exposed' THEN secrets WHEN 'blocked' THEN stage = 'blocked' WHEN 'approved' THEN stage = 'approved'
                        WHEN 'never_reviewed' THEN gate_state = 'never_reviewed' WHEN 'unowned' THEN owners_n = 0 WHEN 'failed' THEN stage = 'failed'
                        WHEN 'refused' THEN id = ANY($15) ELSE false END)
  AND ($6::text = '' OR EXISTS (SELECT 1 FROM project_access pa WHERE pa.project_id = fleet.id AND pa.kind = 'email' AND pa.level = 'owner' AND (pa.subject = $6 OR pa.grantee = $6)))
  AND ($7::text = '' OR team AND stage = $7)
  AND ($8::text = '' OR team AND CASE $8 WHEN 'production' THEN prod_package IS NOT NULL WHEN 'preview' THEN preview_package IS NOT NULL
                        WHEN 'nothing' THEN prod_package IS NULL AND preview_package IS NULL ELSE false END)
  AND ($9::text = '' OR team AND strpos(lower(stack), lower($9)) > 0)
  AND ($10::text = '' OR strpos(lower(id), lower($10)) > 0 OR strpos(lower(name), lower($10)) > 0)
  AND ($11::text[] IS NULL OR id = ANY($11))
  AND ($12::text = '' OR team AND CASE $12 WHEN 'everyone' THEN everyone WHEN 'shared' THEN NOT everyone AND people + group_grants > 0 ELSE NOT everyone AND people + group_grants = 0 END)
  AND (NOT $13::bool OR level IN ('owner', 'contributor'))
  AND ($14::text = '' OR hold IS NOT NULL AND ($14 = 'all' OR team))
`

// fleetCountSQL is how many rows the filters select, across every page (contracts/routes.md, the fleet read: total).
var fleetCountSQL = fleetDerivation + `SELECT count(*) FROM fleet ` + fleetWhere

// fleetSorts is each sort's page statement: the cursor a strict comparison on the sort key past the last row
// ($16 on: an empty id is no cursor), then the limit plus one — the extra row says there is a next page — and a numbered page's offset. Under risk
// and changed a reduced row sorts after the rest by id alone (rank 8, since the epoch); under name every row is by name. Risk first puts the
// rows read whole that carry needs by their first need's rank ($16 the apps, $17 their ranks; noNeed for every other row), so its
// cursor from $18.
var fleetSorts = map[string]string{
	"risk": fleetRowSQL + `LEFT JOIN unnest($16::text[], $17::int[]) AS need(app, top) ON need.app = fleet.id
` + fleetWhere + `  AND ($21::text = '' OR (coalesce(need.top, ` + fmt.Sprint(noNeed) + `), rank, since, id) > ($18::int, $19::int, $20::timestamptz, $21))
ORDER BY coalesce(need.top, ` + fmt.Sprint(noNeed) + `), rank, since, id LIMIT $22 OFFSET $23`,
	"changed": fleetRowSQL + fleetWhere + `  AND ($18::text = '' OR (rank = 8)::int > $16::int OR (rank = 8)::int = $16 AND (since < $17::timestamptz OR since = $17 AND id > $18))
ORDER BY rank = 8, since DESC, id LIMIT $19 OFFSET $20`,
	"name": fleetRowSQL + fleetWhere + `  AND ($17::text = '' OR (lower(name), id) > (lower($16::text), $17))
ORDER BY lower(name), id LIMIT $18 OFFSET $19`,
}

var projectsSQL = fleetRowSQL + `WHERE $5::text = '' OR id = $5 ORDER BY id`

// fleetTilesSQL counts the apps the caller reads whole — every app for an admin — the tiles and the stats, and no filter
// narrows either (contracts/routes.md, the fleet read): owners are the distinct addresses with an owner grant on them, and the
// deploys their deploy rows on the trail in the seven days before the read, Canyon's or, where the org runs it, its platform's.
// $5 is the apps whose rows carry a refused need (needs.go), the refused tile's.
var fleetTilesSQL = fleetDerivation + `SELECT count(*) FILTER (WHERE secrets), count(*) FILTER (WHERE stage = 'blocked'), count(*) FILTER (WHERE stage = 'approved'),
  count(*) FILTER (WHERE gate_state = 'never_reviewed'), count(*) FILTER (WHERE owners_n = 0), count(*) FILTER (WHERE stage = 'failed'),
  count(*) FILTER (WHERE id = ANY($5)),
  count(*), count(*) FILTER (WHERE prod_url IS NOT NULL), count(*) FILTER (WHERE preview_url IS NOT NULL), count(*) FILTER (WHERE gate_state = 'clean'),
  (SELECT count(DISTINCT lower(pa.grantee)) FROM project_access pa WHERE pa.kind = 'email' AND pa.level = 'owner' AND pa.project_id IN (SELECT id FROM fleet WHERE team)),
  (SELECT count(*) FROM audit_events e WHERE e.action IN ('deploy preview', 'deploy prod', 'platform reported deployed', 'release live by check-in') AND e.ts > now() - interval '7 days' AND e.project_id IN (SELECT id FROM fleet WHERE team)),
  (SELECT count(*) FROM audit_events e WHERE (e.action = 'deploy prod' OR e.action IN ('platform reported deployed', 'release live by check-in') AND e.detail->>'env' = 'prod')
    AND e.ts > now() - interval '7 days' AND e.project_id IN (SELECT id FROM fleet WHERE team))
FROM fleet WHERE team`

type fleetRow struct {
	project
	LastRun  *string `json:"last_run"`
	Warnings int     `json:"warnings"` // the last run's advisory findings, its verdict's count; 0 before a verdict (contracts/routes.md, the fleet read)
	Needs    []need  `json:"needs"`    // what needs a person (needs.go), in order
}

// read is the fleet row as the caller may read it: whole from contributor with its needs, else the reduced row, with
// no last_run, and a responder's its held need alone.
func (f fleetRow) read(responder bool, needs []need) any {
	if f.team {
		f.Needs = needs
		if f.Needs == nil {
			f.Needs = []need{}
		}
		return f
	}
	r := f.reduced(responder)
	if responder {
		r.Needs = needs
	}
	return r
}

type latest struct {
	Package *string `json:"package"`
	Label   *string `json:"label,omitempty"`
	Stage   string  `json:"stage"`
	Since   string  `json:"since"`
}

// fleetCursor is the last row on a page under its sort: its sort key, since at full precision (the wire's is whole seconds;
// cut there, a row could be skipped), and the sort it was issued for, so a cursor never walks another sort.
type fleetCursor struct {
	Sort  string    `json:"s"`
	Need  int       `json:"q"` // under risk, the row's first need's rank, noNeed for a row with none
	Rank  int       `json:"r"`
	Since time.Time `json:"t"`
	Name  string    `json:"n"`
	ID    string    `json:"i"`
}

func (c fleetCursor) String() string {
	raw, _ := json.Marshal(c) // ponytail: a struct of plain fields always marshals
	return base64.RawURLEncoding.EncodeToString(raw)
}

// parseFleetCursor reads back what String wrote for sort, rank one of the seven buckets or the reduced rows' after them;
// an absent cursor is the start, anything else — another sort's among it — is false.
func parseFleetCursor(s, sort string) (c fleetCursor, ok bool) {
	if s == "" {
		return fleetCursor{Sort: sort}, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, false
	}
	return c, c.Sort == sort && c.ID != "" && !strings.ContainsRune(c.ID+c.Name, 0) && c.Rank >= 0 && c.Rank <= 8 && c.Need >= 0 && c.Need <= noNeed // a NUL Postgres refuses is no cursor
}

// args are the cursor's parameters for its sort's page statement: from $16, risk's from $18.
func (c fleetCursor) args() []any {
	switch c.Sort {
	case "changed":
		reduced := 0
		if c.Rank == 8 {
			reduced = 1
		}
		return []any{reduced, c.Since, c.ID}
	case "name":
		return []any{c.Name, c.ID}
	}
	return []any{c.Need, c.Rank, c.Since, c.ID}
}

// envCols are one environment's columns as the page selects them, all null when nothing runs
// there; label is null on its own when the package it runs carries none.
type envCols struct {
	pkg, label, digest, url, run *string
	at                           *time.Time
	by, promotedBy               json.RawMessage // the actors as jsonb, null when nobody is recorded; promotedBy is prod's alone
	runBy                        *string         // who runs it, the deploy row's (design/HAND-OFF.md §1)
	pending                      json.RawMessage // the release the environment waits on as jsonb, null while none is open
	pr                           json.RawMessage // preview's pull request as jsonb while it runs one's version (#1947), else null
	seen                         *time.Time      // when a pod of the release that put it live last checked in, null before one has (#2452)
}

// into writes the environment (contracts/routes.md, envs.<env>): the deployed shape when something is
// deployed there, and pending laid over it while a newer release is open; release.schema.json's
// env_pending alone while a release is open and nothing is deployed there yet. A release is sent
// under run_by org alone, so an environment carrying pending answers run_by org — never the deploy
// row's canyon, which a row written before a switch to org, or before the column, holds.
func (e envCols) into(envs map[string]any, env string) {
	open := !bytes.Equal(e.pending, []byte("null"))
	if e.pkg == nil {
		if open {
			envs[env] = map[string]any{"run_by": "org", "pending": e.pending}
		}
		return
	}
	m := map[string]any{"url": *e.url, "package": *e.pkg, "image_digest": *e.digest, "deployed_at": e.at.UTC().Format(time.RFC3339), "gate_run": *e.run, "deployed_by": e.by, "run_by": *e.runBy}
	if e.label != nil { // no label is no label key at all, not a null (contracts/routes.md, *Naming a version*)
		m["label"] = *e.label
	}
	if env == "prod" {
		m["promoted_by"] = e.promotedBy
	}
	if len(e.pr) > 0 && !bytes.Equal(e.pr, []byte("null")) {
		m["pull_request"] = e.pr
	}
	if *e.runBy == "org" && e.seen != nil { // the org's pods check in; one Canyon runs says its state instead (#2452)
		m["seen_at"] = e.seen.UTC().Format(time.RFC3339)
	}
	if open { // absent while no release is open, never a null
		m["run_by"], m["pending"] = "org", e.pending
	}
	envs[env] = m
}

// runningWithin bounds the executor calls a read makes to say which environments run, one per 400 of them; a var
// so a test can lower it.
var runningWithin = 2 * time.Second

// sayRunning sets state on every environment Canyon runs among each project's envs, by id
// (contracts/routes.md, envs.<env>.state), from one executor call for every 400 of them, never one per app:
// running when the executor names it, restarting when it says the app has been started again since its
// deploy (and how often, counted: the status read's and the promotion's alone), starting when it says a /run is starting
// it (#230), stopped when it names none of these, unknown when it could not be asked
// in time — so a read that could not ask never says running. Asked why — the status read alone — it
// answers how those that have exited last exited, stopped or restarting, as the executor said it, raw,
// for sayWhy to mask.
func (a *app) sayRunning(ctx context.Context, envs map[string]map[string]any, why, counted bool) []executor.Stopped {
	var asked []executor.Container
	for id, project := range envs {
		for env, e := range project {
			if m, ok := e.(map[string]any); ok && m["run_by"] == "canyon" {
				m["state"] = "stopped"
				asked = append(asked, executor.Container{App: id, Target: env})
			}
		}
	}
	if len(asked) == 0 {
		return nil
	}
	// One call for every MaxRunningOf of them, at once under one deadline: a fleet of more than 200 apps asks in calls of
	// 400 environments, the most one asks (contracts/executor.md, POST /running), never one an app, and in a fixed order,
	// so a call that fails reads the same environments unknown on every poll.
	slices.SortFunc(asked, func(x, y executor.Container) int {
		return cmp.Or(strings.Compare(x.App, y.App), strings.Compare(x.Target, y.Target))
	})
	chunks := slices.Collect(slices.Chunk(asked, executor.MaxRunningOf))
	answers, errs := make([]executor.Runs, len(chunks)), make([]error, len(chunks))
	ctx, cancel := context.WithTimeout(ctx, runningWithin)
	defer cancel()
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Go(func() {
			if a.pipe == nil || a.pipe.Executor == nil {
				errs[i] = errors.New("no executor configured")
				return
			}
			answers[i], errs[i] = a.pipe.Executor.RunningOf(ctx, chunk, why)
		})
	}
	wg.Wait()
	var runs executor.Runs
	for i, chunk := range chunks {
		if errs[i] != nil {
			a.log.Printf("canyon-server: which environments run: %v", errs[i])
			for _, c := range chunk {
				envs[c.App][c.Target].(map[string]any)["state"] = "unknown"
			}
			continue
		}
		runs.Running, runs.Restarting, runs.Stopped = append(runs.Running, answers[i].Running...), append(runs.Restarting, answers[i].Restarting...), append(runs.Stopped, answers[i].Stopped...)
		runs.Starting = append(runs.Starting, answers[i].Starting...)
	}
	for _, c := range runs.Running {
		if m, ok := envs[c.App][c.Target].(map[string]any); ok { // only what was asked: an executor naming more is not believed
			m["state"] = "running"
		}
	}
	_ = runs.Starting // planted, to be reverted: the executor's starting is not read, so a switch reads stopped (#230's red)
	for _, r := range runs.Restarting { // after running: an executor naming both is read as restarting, which fails closed
		if m, ok := envs[r.App][r.Target].(map[string]any); ok {
			m["state"] = "restarting"
			if counted {
				m["restarting"] = map[string]any{"restarts": r.Restarts}
			}
		}
	}
	var stopped []executor.Stopped
	for _, s := range runs.Stopped {
		if m, ok := envs[s.App][s.Target].(map[string]any); ok && (m["state"] == "stopped" || m["state"] == "restarting") {
			stopped = append(stopped, s)
		}
	}
	return stopped
}

// sayWhy sets stopped {exit_code, said} on each environment of one project that sayRunning found
// stopped and the executor said why of, and the same two on a restarting one's restarting, how it last
// exited (#1597): on the status read alone (contracts/routes.md, the status row).
// The line is the pipeline's to mask, and "" when it cannot be: the exit code alone, never a raw line.
// Then what could hide or reorder text goes, as a description refuses it, and it is cut to 200 characters.
func (a *app) sayWhy(ctx context.Context, envs map[string]any, stopped []executor.Stopped) {
	for _, s := range stopped {
		said, err := "", error(nil)
		if s.Said != "" {
			said, err = a.pipe.Masked(ctx, s.App, s.Target, s.Said)
		}
		if err != nil {
			a.log.Printf("canyon-server: why %s-%s stopped: %v", s.App, s.Target, err)
			said = ""
		}
		shown := []rune(strings.Map(func(r rune) rune {
			if hidesText(r) {
				return -1
			}
			return r
		}, said))
		why, m := map[string]any{"exit_code": s.ExitCode, "said": string(shown[:min(len(shown), 200)])}, envs[s.Target].(map[string]any)
		if m["state"] != "restarting" {
			m["stopped"] = why
		} else if r, ok := m["restarting"].(map[string]any); ok {
			maps.Copy(r, why)
		}
	}
}

// sayAnswered sets answered on each environment of project Canyon runs whose deploy asked the app
// (contracts/routes.md, envs.<env>.answered): on the status read alone.
func sayAnswered(ctx context.Context, pool *pgxpool.Pool, project string, envs map[string]any) error {
	rows, err := pool.Query(ctx, "SELECT env, answered FROM deploys WHERE project_id = $1 AND answered IS NOT NULL", project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var env string
		var answered json.RawMessage
		if err := rows.Scan(&env, &answered); err != nil {
			return err
		}
		if m, ok := envs[env].(map[string]any); ok && m["run_by"] == "canyon" {
			m["answered"] = answered
		}
	}
	return rows.Err()
}

// scanFleetRow reads one fleetRowSQL row into the wire shape and its page key.
func scanFleetRow(rows pgx.Rows) (fleetRow, fleetCursor, error) {
	var f fleetRow
	var owners []byte
	var key fleetCursor
	var pv, pd envCols
	if err := rows.Scan(&f.ID, &f.Name, &f.Stack, &owners, &f.OwnerGroups, &f.Summary, &f.GateState, &f.LastRun, &f.Warnings, &f.Latest.Package, &f.Latest.Label, &f.Latest.Stage, &key.Since, &key.Rank, &f.Access.Level, &f.team, &f.Access.Everyone, &f.Access.People, &f.Access.Groups, &f.Access.Requests, &f.Claimable,
		&pv.pkg, &pv.label, &pv.digest, &pv.url, &pv.at, &pv.run, &pv.by, &pv.runBy, &pv.pending, &pv.pr, &pv.seen,
		&pd.pkg, &pd.label, &pd.digest, &pd.url, &pd.at, &pd.run, &pd.by, &pd.promotedBy, &pd.runBy, &pd.pending, &pd.seen, &f.Hold); err != nil {
		return f, key, err
	}
	if err := json.Unmarshal(owners, &f.Owners); err != nil {
		return f, key, fmt.Errorf("owners of %s: %w", f.ID, err)
	}
	f.Latest.Since, key.ID, key.Name = key.Since.UTC().Format(time.RFC3339), f.ID, f.Name
	f.Envs = map[string]any{}
	pv.into(f.Envs, "preview")
	pd.into(f.Envs, "prod")
	return f, key, nil
}

// fleetPage runs the cursor's sort's page statement for the caller with the ten filters already validated, from the
// cursor (one with no id: the start) and offset rows past it, fetching one row past the limit: if it arrives it is dropped and the cursor points
// after the last kept row. Each row is whole: the caller answers it read(). top is the first need's rank of each row read whole that
// carries one, which risk sorts by first.
func fleetPage(ctx context.Context, pool *pgxpool.Pool, filters []any, top map[string]int, after fleetCursor, limit, offset int) ([]fleetRow, *string, error) {
	sorted := []any{}
	if after.Sort == "risk" {
		apps, ranks := []string{}, []int{}
		for _, id := range slices.Sorted(maps.Keys(top)) {
			apps, ranks = append(apps, id), append(ranks, top[id])
		}
		sorted = []any{apps, ranks}
	}
	rows, err := pool.Query(ctx, fleetSorts[after.Sort], slices.Concat(callerArgs(ctx), filters, sorted, after.args(), []any{limit + 1, offset})...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	apps, keys := []fleetRow{}, []fleetCursor{}
	for rows.Next() {
		f, key, err := scanFleetRow(rows)
		if err != nil {
			return nil, nil, err
		}
		key.Need = noNeed
		if n, ok := top[f.ID]; ok {
			key.Need = n
		}
		apps, keys = append(apps, f), append(keys, key)
	}
	if rows.Err() != nil || len(apps) <= limit {
		return apps, nil, rows.Err()
	}
	keys[limit-1].Sort = after.Sort
	next := keys[limit-1].String()
	return apps[:limit], &next, nil
}
