package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// listed is the list's entry for slug.
func listed(t *testing.T, c *http.Client, base, slug string) map[string]any {
	t.Helper()
	code, got, _ := do(t, c, http.MethodGet, base+playbooksAt, "")
	if code != 200 {
		t.Fatalf("the list: %d %s", code, lastBody)
	}
	for _, e := range got["playbooks"].([]any) {
		if e := e.(map[string]any); e["slug"] == slug {
			return e
		}
	}
	t.Fatalf("the list has no %s: %s", slug, lastBody)
	return nil
}

// fetchPlaybook reads slug's body the way read names, "" for the dashboard's.
func fetchPlaybook(t *testing.T, c *http.Client, base, slug, read string) int {
	t.Helper()
	at := base + playbooksAt + "/" + slug
	if read != "" {
		at += "?read=" + read
	}
	code, _, _ := do(t, c, http.MethodGet, at, "")
	return code
}

// holdSkills asks for the manifest naming the skills held.
func holdSkills(t *testing.T, c *http.Client, base, held string) {
	t.Helper()
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/skill/manifest?held="+held, ""); code != 200 {
		t.Fatalf("the manifest with held=%s: %d %s", held, code, lastBody)
	}
}

// countGoldenPlaybooks makes the golden list's counts through the routes: incident-notes read twice, and
// service-conventions read 14 times and held at its newest version by three sessions.
func countGoldenPlaybooks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base string, dana *http.Client) {
	t.Helper()
	for _, read := range []string{"tool", "cli"} {
		fetchPlaybook(t, dana, base, "incident-notes", read)
	}
	for i := range 14 {
		fetchPlaybook(t, dana, base, "service-conventions", []string{"tool", "resource", "cli"}[i%3])
	}
	alice := signedIn(t, ctx, pool, base, "sub-alice", "alice@meridian.example", false)
	bob := signedIn(t, ctx, pool, base, "sub-bob", "bob@meridian.example", false)
	holdSkills(t, dana, base, "service-conventions@3")
	holdSkills(t, dana, base, "service-conventions@3,incident-notes@1") // the same session again, and a note: no skill
	fetchPlaybook(t, alice, base, "service-conventions", "sync")
	holdSkills(t, bob, base, "service-conventions@3")
}

// Reads count each fetch of a body by the way an agent reads it, per playbook per UTC day per way: the tool, the
// resource and canyon playbook. The dashboard's read and the hook's sync count none, a way Canyon doesn't know is
// refused, and a read 30 days ago or more has left the list's 30 days (design/specs/mcp.md §3.5).
func TestPlaybookReads(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true)
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	for _, read := range []string{"tool", "tool", "resource", "cli", "", "sync"} {
		if code := fetchPlaybook(t, dana, base, "service-conventions", read); code != 200 {
			t.Fatalf("read=%s: %d %s", read, code, lastBody)
		}
	}
	if code := fetchPlaybook(t, dana, base, "service-conventions", "dashboard"); code != 400 {
		t.Fatalf("read=dashboard: %d, want 400", code)
	}
	if code := fetchPlaybook(t, dana, base, "no-such-playbook", "tool"); code != 404 {
		t.Fatalf("a read of no playbook: %d, want 404", code)
	}
	if got := count(t, ctx, pool, "SELECT count(*) FROM playbook_reads WHERE slug = 'no-such-playbook'"); got != 0 {
		t.Fatal("a read of no playbook was counted")
	}
	byWay := map[string]int{}
	rows, err := pool.Query(ctx, "SELECT way, reads FROM playbook_reads WHERE day = (now() AT TIME ZONE 'UTC')::date")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var way string
		var n int
		if err := rows.Scan(&way, &n); err != nil {
			t.Fatal(err)
		}
		byWay[way] = n
	}
	if len(byWay) != 3 || byWay["tool"] != 2 || byWay["resource"] != 1 || byWay["cli"] != 1 {
		t.Fatalf("today's reads by way: %v, want tool 2, resource 1 and cli 1", byWay)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO playbook_reads (slug, day, way, reads) VALUES
		('service-conventions', (now() AT TIME ZONE 'UTC')::date - 29, 'cli', 10), ('service-conventions', (now() AT TIME ZONE 'UTC')::date - 30, 'cli', 100)`); err != nil {
		t.Fatal(err)
	}
	if e := listed(t, dana, base, "service-conventions"); e["reads"] != float64(14) {
		t.Fatalf("the list's reads: %v, want 4 today and 10 from 29 days ago, never the 100 from 30 days ago", e["reads"])
	}
}

// Machines are the sessions whose native skill holds a skill's newest version, over 30 days: a manifest naming it
// held, or the hook fetching it, records one sync per session, each a holder that names no session. A version that
// isn't the newest, a note, and an entry out of shape record nothing; a new version starts again from none; and a
// sync 30 days old is deleted (design/specs/mcp.md §3.5).
func TestPlaybookSyncsCountMachines(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true)
	alice := signedIn(t, ctx, pool, base, "sub-alice", "alice@meridian.example", false)
	bob := signedIn(t, ctx, pool, base, "sub-bob", "bob@meridian.example", false)
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	if _, err := pool.Exec(ctx, `INSERT INTO playbook_versions (slug, version, name, kind, when_line, body, set_by_kind, set_by_subject, set_by_display)
		VALUES ('incident-notes', 1, 'Incidents', 'note', 'Read on a database error.', '# Incidents'||chr(10), 'person', 'sub-dana', 'Dana Kim')`); err != nil {
		t.Fatal(err)
	}
	if e := listed(t, dana, base, "incident-notes"); e["machines"] != nil {
		t.Fatalf("a note has machines: %v", e["machines"])
	}
	holdSkills(t, dana, base, "service-conventions@2")
	holdSkills(t, dana, base, "service-conventions@2,service-conventions@1,incident-notes@1,nonsense,@,x@y")
	holdSkills(t, alice, base, "service-conventions@1") // an older version: alice's hook fetches the newest
	if got := count(t, ctx, pool, "SELECT count(*) FROM playbook_syncs"); got != 1 {
		t.Fatalf("%d syncs after dana's two manifests and alice's old version, want dana's one", got)
	}
	fetchPlaybook(t, alice, base, "service-conventions", "sync")
	fetchPlaybook(t, bob, base, "service-conventions", "sync")
	if e := listed(t, dana, base, "service-conventions"); e["machines"] != float64(3) || e["reads"] != float64(0) {
		t.Fatalf("machines %v and reads %v, want 3 sessions and no read: a sync is not a read", e["machines"], e["reads"])
	}
	var holder []byte
	if err := pool.QueryRow(ctx, "SELECT holder FROM playbook_syncs LIMIT 1").Scan(&holder); err != nil || len(holder) != 32 {
		t.Fatalf("a holder is 32 bytes: %x %v", holder, err)
	}
	if strings.Contains(string(holder), "ses_") {
		t.Fatal("a holder carries the session's digest")
	}
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	if e := listed(t, dana, base, "service-conventions"); e["machines"] != float64(0) {
		t.Fatalf("a new version is held by %v machines before any sync, want 0", e["machines"])
	}
	if _, err := pool.Exec(ctx, "UPDATE playbook_syncs SET seen_on = seen_on - 30"); err != nil {
		t.Fatal(err)
	}
	holdSkills(t, bob, base, "service-conventions@3")
	if got := count(t, ctx, pool, "SELECT count(*) FROM playbook_syncs"); got != 1 {
		t.Fatalf("%d syncs: the three 30 days old stayed", got)
	}
}
