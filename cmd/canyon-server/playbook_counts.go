package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
)

// The playbook counts (design/specs/mcp.md §3.5; contracts/routes.md, the playbooks row): reads, each fetch of a body
// by a way an agent reads it, per playbook per UTC day per way; and syncs, a session's native skill holding a
// version, by a holder no row joins back to a session. Neither names a person, and neither ever refuses what it
// counts: a count that can't be written is a log line.

// readWays are the ways a read is counted, and sync, the hook's fetch, which records a sync and no read.
var readWays = map[string]bool{"": true, "tool": true, "resource": true, "cli": true, "sync": true}

// maxHeld is how many held skills a manifest request names that Canyon reads.
const maxHeld = 50

// countRead counts one read of slug by way on today's UTC day.
func (a *app) countRead(ctx context.Context, slug, way string) {
	if _, err := a.pool.Exec(ctx, `INSERT INTO playbook_reads (slug, day, way, reads) VALUES ($1, (now() AT TIME ZONE 'UTC')::date, $2, 1)
		ON CONFLICT (slug, day, way) DO UPDATE SET reads = playbook_reads.reads + 1`, slug, way); err != nil {
		a.log.Printf("canyon-server: count a read of playbook %s: %v", slug, err)
	}
}

// recordSyncs records that the caller's session holds each version named, today, and deletes the syncs older than
// 30 days. A caller with no session records none.
func (a *app) recordSyncs(ctx context.Context, held map[string]int) {
	session := events.ProvenanceOf(ctx).Session
	if session == "" || len(held) == 0 {
		return
	}
	holder, err := a.vault.Holder(session, time.Now())
	if err != nil {
		a.log.Printf("canyon-server: record playbook syncs: %v", err)
		return
	}
	for slug, version := range held {
		if _, err := a.pool.Exec(ctx, `INSERT INTO playbook_syncs (slug, version, holder, seen_on) VALUES ($1, $2, $3, (now() AT TIME ZONE 'UTC')::date)
			ON CONFLICT (slug, version, holder) DO UPDATE SET seen_on = excluded.seen_on`, slug, version, holder); err != nil {
			a.log.Printf("canyon-server: record a sync of playbook %s: %v", slug, err)
		}
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM playbook_syncs WHERE seen_on < (now() AT TIME ZONE 'UTC')::date - 29`); err != nil {
		a.log.Printf("canyon-server: delete old playbook syncs: %v", err)
	}
}

// heldSkills is the manifest's held: each <slug>@<version> that names a standing skill's newest version, of at most
// maxHeld entries. Anything else in it is left out, never refused: a session hook reads the manifest.
func heldSkills(raw string, standing []playbook) map[string]int {
	current := map[string]int{}
	for _, p := range standing {
		if p.Kind == "skill" {
			current[p.Slug] = p.Version
		}
	}
	held := map[string]int{}
	for i, entry := range strings.Split(raw, ",") {
		if i == maxHeld {
			break
		}
		slug, n, ok := strings.Cut(entry, "@")
		version, err := strconv.Atoi(n)
		if ok && err == nil && current[slug] == version && version >= 1 {
			held[slug] = version
		}
	}
	return held
}

// playbookCounts is each standing playbook's reads in the last 30 days, and each skill's machines: the holders of its
// newest version seen in those 30 days.
func playbookCounts(ctx context.Context, q querier) (reads, machines map[string]int, err error) {
	reads, machines = map[string]int{}, map[string]int{}
	rows, err := q.Query(ctx, `SELECT slug, sum(reads)::bigint FROM playbook_reads WHERE day > (now() AT TIME ZONE 'UTC')::date - 30 GROUP BY slug`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var slug string
		var n int
		if err := rows.Scan(&slug, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		reads[slug] = n
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT s.slug, count(*) FROM playbook_syncs s
		WHERE s.seen_on > (now() AT TIME ZONE 'UTC')::date - 30 AND s.version = (SELECT max(v.version) FROM playbook_versions v WHERE v.slug = s.slug)
		GROUP BY s.slug`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var slug string
		var n int
		if err := rows.Scan(&slug, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		machines[slug] = n
	}
	return reads, machines, rows.Err()
}
