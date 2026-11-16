package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/gate/custom"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// reviewedAs uploads files as project's package and writes its verdict by hand under run: the package
// is the app's latest reviewed one, its tree in the app's repository as a review reads it.
func reviewedAs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, c *http.Client, base, project string, files map[string]string, run string) {
	t.Helper()
	code, body := upload(t, c, base, project, tarOf(t, files), "", "")
	pkg, _ := packageOf(body)["id"].(string)
	if code != 201 && code != 200 || pkg == "" {
		t.Fatalf("upload of %s: %d %s", project, code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ($1, $2, $3, 'clean', now())`, run, project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', $1, $2, 'verdict', '{"state": "clean", "findings": {"runs": []}}')`, run, project); err != nil {
		t.Fatal(err)
	}
}

// A dry run judges a custom rule checked by code on every app's latest review, as its review would:
// one that holds passes, one that does not fails, and one with no review is not checked. It is an
// admin's: a member is refused before anything is read. Past either bound — the apps, the time — an app
// is not checked, never a pass (contracts/routes.md, the dry-run row; #2640).
func TestCustomRuleDryRunJudgesEveryAppsLatestReview(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true)
	member := signedIn(t, ctx, pool, base, "sub-lee", "lee@meridian.example", false)
	for _, app := range []string{"has-readme", "no-readme", "unreviewed"} {
		if code, _, _ := do(t, admin, http.MethodPost, base+"/api/v1/projects", `{"id":"`+app+`"}`); code != 201 {
			t.Fatalf("%s: %d %s", app, code, lastBody)
		}
	}
	reviewedAs(t, ctx, pool, admin, base, "has-readme", map[string]string{"README.md": "# shop\n", "index.html": "<p>shop</p>\n"}, "gr_dry1")
	reviewedAs(t, ctx, pool, admin, base, "no-readme", map[string]string{"index.html": "<p>shop</p>\n"}, "gr_dry2")
	if _, err := pool.Exec(ctx, `INSERT INTO custom_rule_edits (rule, version, name, instructions, check_by, check_expr, check_says, check_facts_version, set_by_kind, set_by_subject, set_by_display)
		VALUES ('custom.has-readme', 1, 'Has a README', 'Every app has a README.md at its root.', 'code', $1, 'The app has a README.md at its root.', $2, 'person', 'sub-dana', 'Dana Kim'),
		('custom.emails', 1, 'Emails encrypted', 'Customer emails are encrypted at rest.', 'ai', NULL, NULL, $2, 'person', 'sub-dana', 'Dana Kim')`,
		"'README.md' in files", custom.FactsVersion); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/custom-rules/has-readme/dry-run"
	if code, _, _ := do(t, member, http.MethodPost, base+path, ""); code != 403 || !strings.HasPrefix(lastBody, `{"error":"admin_required"`) {
		t.Fatalf("a member: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, admin, http.MethodPost, base+path, ""); code != 200 || lastBody != `{"apps":[{"project":"has-readme","result":"pass"},{"project":"no-readme","result":"fail"},`+
		`{"project":"unreviewed","result":"not_checked","why":"It has no review yet."}],"rule":"custom.has-readme","version":1}` {
		t.Fatalf("an admin: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, admin, http.MethodPost, base+"/api/v1/custom-rules/emails/dry-run", ""); code != 409 ||
		lastBody != `{"error":"checked_by_ai","message":"This rule is checked by AI. A dry run checks a rule checked by code."}` {
		t.Fatalf("a rule checked by AI: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, admin, http.MethodPost, base+"/api/v1/custom-rules/nothing/dry-run", ""); code != 404 {
		t.Fatalf("no such rule: %d %s", code, lastBody)
	}
	// one at a time on the host: a second, while one runs, is answered at once and never queued
	held, mine := locked(t, ctx, pool)
	if !mine {
		t.Fatal("the first dry run's lock was not taken")
	}
	if code, _, _ := do(t, admin, http.MethodPost, base+path, ""); code != 409 ||
		lastBody != `{"error":"dry_run_running","message":"Another dry run is running on this Canyon. Try again when it ends."}` {
		t.Fatalf("a second dry run: %d %s", code, lastBody)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, admin, http.MethodPost, base+path, ""); code != 200 {
		t.Fatalf("once the first ends: %d %s", code, lastBody)
	}
	apps, was := dryRunApps, dryRunFor
	t.Cleanup(func() { dryRunApps, dryRunFor = apps, was })
	dryRunApps = 1 // the most recently reviewed alone: no-readme's
	if code, _, _ := do(t, admin, http.MethodPost, base+path, ""); code != 200 || !strings.Contains(lastBody, `{"project":"has-readme","result":"not_checked","why":"A dry run checks at most 1 apps."}`) ||
		!strings.Contains(lastBody, `{"project":"no-readme","result":"fail"}`) {
		t.Fatalf("past the apps' bound: %d %s", code, lastBody)
	}
	dryRunApps, dryRunFor = apps, time.Nanosecond
	if code, _, _ := do(t, admin, http.MethodPost, base+path, ""); code != 200 || strings.Contains(lastBody, `"pass"`) || strings.Contains(lastBody, `"fail"`) ||
		strings.Count(lastBody, `"why":"The dry run ran out of time."`) != 2 {
		t.Fatalf("past the time's bound: %d %s", code, lastBody)
	}
}

// The dry run's lock is one per Canyon: two sharing one Postgres, each in its own schema, take theirs at
// the same time, while a second in one schema is refused (#2657).
func TestCustomRuleDryRunLockIsPerCanyon(t *testing.T) {
	t.Run("one", func(t *testing.T) { // a schema per (sub)test: Canyon one's, held while Canyon two takes its own
		one, ctx := pgtest.Schema(t)
		if _, mine := locked(t, ctx, one); !mine {
			t.Fatal("Canyon one's lock was not taken")
		}
		t.Run("two", func(t *testing.T) {
			two, _ := pgtest.Schema(t)
			if _, mine := locked(t, ctx, two); !mine {
				t.Fatal("Canyon two's lock was not taken while Canyon one holds its own")
			}
		})
		if _, mine := locked(t, ctx, one); mine {
			t.Fatal("a second dry run on Canyon one took the lock")
		}
	})
}

// A managed Postgres's idle-in-transaction timeout never ends the lock's transaction early: it is idle
// while the apps are judged, and one-at-a-time holds for the whole run (#2657).
func TestCustomRuleDryRunLockOutlivesAnIdleTimeout(t *testing.T) {
	pool, ctx := pgtest.Schema(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET idle_in_transaction_session_timeout = '200ms'"); err != nil { // as the provider's setting reaches a session
		t.Fatal(err)
	}
	tx, mine := locked(t, ctx, conn)
	if !mine {
		t.Fatal("the lock was not taken")
	}
	time.Sleep(time.Second) // idle in the transaction, as while the apps are judged
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("the lock's transaction ended while idle: %v", err)
	}
	if _, mine := locked(t, ctx, pool); mine {
		t.Fatal("a second dry run took the lock while the first holds it")
	}
}

// locked takes the dry-run lock on db, its transaction rolled back when t ends, before the pool closes,
// whatever the test found; and says whether it was taken.
func locked(t *testing.T, ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}) (pgx.Tx, bool) {
	t.Helper()
	tx, mine, err := lockDryRun(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
	return tx, mine
}
