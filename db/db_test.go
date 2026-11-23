package db

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// migrationCount is how many .sql files are embedded, read from the same FS Migrate reads: a
// migration added in any later PR is counted here too, so this file never has to change for it.
func migrationCount(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// misnumbered is what breaks one migration per number, counting up from 0001: Migrate tracks a migration by
// its file name and applies them in name order, so two files sharing a number would both run, in an order
// nobody chose; and a gap is a number two branches can both still take.
func misnumbered(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var problems []string
	seen := map[int]string{}
	for _, name := range names {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(name, "migrations/"), "_")
		number, err := strconv.Atoi(prefix)
		if err != nil || len(prefix) != 4 { // four digits: Migrate applies them in name order
			problems = append(problems, fmt.Sprintf("%s does not start with a four-digit number", name))
			continue
		}
		if prev, ok := seen[number]; ok {
			problems = append(problems, fmt.Sprintf("%s and %s share the number %s: give the newer one the next free number", prev, name, prefix))
		}
		seen[number] = name
	}
	for number := 1; number <= len(seen); number++ {
		if _, ok := seen[number]; !ok {
			problems = append(problems, fmt.Sprintf("no migration is numbered %04d: number them from 0001 with no gap", number))
		}
	}
	return problems, nil
}

func TestEveryMigrationHasItsOwnNumber(t *testing.T) {
	problems, err := misnumbered(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// Today's migrations plus what two branches would add: each the next number (0077 as this was written),
// merged; or the one after it with nothing at the next.
func TestMisnumberedNamesASharedNumberAndAGap(t *testing.T) {
	next := migrationCount(t) + 1
	for _, c := range []struct {
		added []string
		want  string
	}{
		{[]string{fmt.Sprintf("%04d_a.sql", next), fmt.Sprintf("%04d_b.sql", next)}, fmt.Sprintf("migrations/%04d_a.sql and migrations/%04d_b.sql share the number %04d", next, next, next)},
		{[]string{fmt.Sprintf("%04d_late.sql", next+1)}, fmt.Sprintf("no migration is numbered %04d", next)},
		{[]string{"7_short.sql"}, "migrations/7_short.sql does not start with a four-digit number"},
	} {
		dir := t.TempDir()
		if err := os.CopyFS(dir, migrations); err != nil {
			t.Fatal(err)
		}
		for _, f := range c.added {
			if err := os.WriteFile(filepath.Join(dir, "migrations", f), []byte("SELECT 1;"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		problems, err := misnumbered(os.DirFS(dir))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, c.want) }) {
			t.Errorf("%v added: problems %q, want one starting %q", c.added, problems, c.want)
		}
	}
}

func TestMigrateAppliesOnceFromEmpty(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := Pending(ctx, p); err == nil {
		t.Fatal("an unmigrated schema must not count as current")
	}
	want := migrationCount(t)
	n, err := Migrate(ctx, p)
	if err != nil || n != want {
		t.Fatalf("first run applied %d, err %v, want %d", n, err, want)
	}
	if pending, err := Pending(ctx, p); err != nil || pending != 0 {
		t.Fatalf("a migrated schema has nothing pending: %d, %v", pending, err)
	}
	var setup bool // a fresh install is a host somebody needs to onboard; an upgrade is not (routes_setup_state_test.go)
	if err := p.QueryRow(ctx, `SELECT onboarded_at IS NULL FROM org_preferences`).Scan(&setup); err != nil || !setup {
		t.Fatalf("a database nothing has touched starts in the setup state: onboarded_at null %v, %v", setup, err)
	}
	n, err = Migrate(ctx, p)
	if err != nil || n != 0 {
		t.Fatalf("second run applied %d, err %v; must be a no-op", n, err)
	}
	for _, table := range []string{"projects", "packages", "runs", "timeline_rows"} {
		var ok bool
		if err := p.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing after migrate (%v)", table, err)
		}
	}
}

func TestTimelineRowsKeepTheirScopeInvariant(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO projects (id) VALUES ('claims-portal')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, project_id, kind, data) VALUES ('org', 'claims-portal', 'audit.appended', '{"event_id":"ev_1"}')`); err != nil {
		t.Fatalf("an org row without a run must be accepted: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO timeline_rows (scope, project_id, kind, data) VALUES ('run', 'claims-portal', 'run.state', '{"state":"pending"}')`); err == nil {
		t.Fatal("a run-scope row without a run must be refused")
	}
}

func TestConcurrentStartersMigrateOnce(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	var wg sync.WaitGroup
	applied := make([]int, 5)
	errs := make([]error, 5)
	for i := range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); applied[i], errs[i] = Migrate(ctx, p) }()
	}
	wg.Wait()
	total := 0
	for i := range 5 {
		if errs[i] != nil {
			t.Fatalf("starter %d: %v — a concurrent starter must wait, not fail", i, errs[i])
		}
		total += applied[i]
	}
	if want := migrationCount(t); total != want {
		t.Fatalf("the migrations were applied %d times, want %d (once each)", total, want)
	}
}

func TestTheSchemaHoldsTheContracts(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	must := func(sql string) {
		t.Helper()
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	refused := func(what, sql string) {
		t.Helper()
		if _, err := p.Exec(ctx, sql); err == nil {
			t.Fatalf("%s: accepted", what)
		}
	}
	must(`INSERT INTO projects (id) VALUES ('claims-portal'), ('quote-engine')`)
	// a package carries the commit and its source (routes.md: {id, tree_hash, commit, source})
	must(`INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_actor) VALUES ('pkg_7d3e8f', 'claims-portal', repeat('a', 40), repeat('b', 40), 1200, 'upload', 'dana')`)
	must(`INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_1', 'claims-portal', 'pkg_7d3e8f', 'running')`)
	refused("a second active run for the same project", `INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_2', 'claims-portal', 'pkg_7d3e8f', 'pending')`)
	refused("a run binding another project's package", `INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_3', 'quote-engine', 'pkg_7d3e8f', 'clean')`)
	refused("a run-scope row naming another project's run", `INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', 'gr_1', 'quote-engine', 'run.state', '{"state":"running"}')`)
	must(`INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', 'gr_1', 'claims-portal', 'run.state', '{"state":"running"}')`)
	refused("editing a timeline row", `UPDATE timeline_rows SET kind = 'verdict'`)
	refused("deleting a timeline row", `DELETE FROM timeline_rows`)
	refused("truncating the ledger", `TRUNCATE timeline_rows`)
	refused("a failure reason outside the catalogue", `UPDATE runs SET state = 'failed', failure_reason = 'gremlins' WHERE id = 'gr_1'`)
	refused("a second preferences row", `INSERT INTO org_preferences DEFAULT VALUES`) // one org per host, one row, enforced in SQL
	must(`UPDATE runs SET state = 'failed', failure_reason = 'error', ended_at = now() WHERE id = 'gr_1'`)
	app := func(id int, account string, keeps bool) string {
		return fmt.Sprintf(`INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account, keeps_code) VALUES (NULL, %d, 'canyon-%d', 'Iv1', 1, '\x00', '\x00', '\x00', '\x00', '%s', %t)`, id, id, account, keeps)
	}
	must(app(1, "meridian", true))
	must(app(2, "meridian-labs", false)) // one App per org (#1914)
	refused("a second App on one org", app(3, "Meridian", false))
	refused("a second org keeping code", app(4, "meridian-ops", true))
	refused("a second App under one id", app(1, "meridian-ops", false))
	must(`INSERT INTO github_deliveries (delivery_id, event) VALUES ('72d3162e-cc78-11e3-81ab-4c9367dc0958', 'push')`)
	refused("a redelivered github event", `INSERT INTO github_deliveries (delivery_id, event) VALUES ('72d3162e-cc78-11e3-81ab-4c9367dc0958', 'push')`)
	must(`INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'db', 'postgres', 'builtin', 'pending')`)
	refused("a resource in an env Canyon does not have", `INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'production', 'db', 'postgres', 'builtin', 'pending')`)
	refused("a resource state outside the four", `INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'prod', 'db', 'postgres', 'builtin', 'provisioned')`)
	refused("choice 2 without its sealed client secret", `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display) VALUES ('{"choice": 2}', 'person', 'dana', 'Dana')`)
	refused("choice 1 keeping a client secret", `INSERT INTO front_doors (door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES ('{"choice": 1}', 1, '\x00', '\x00', 'person', 'dana', 'Dana')`)
	must(`INSERT INTO front_doors (door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES ('{"choice": 2}', 1, '\x00', '\x00', 'person', 'dana', 'Dana')`)
	refused("a second front door", `INSERT INTO front_doors (id, door, set_by_kind, set_by_subject, set_by_display) VALUES (2, '{"choice": 1}', 'person', 'dana', 'Dana')`) // one per host, enforced in SQL
	refused("a secret origin outside the two", `INSERT INTO secrets (project_id, env, name, scope, sealed_key, sealed_value, set_by_kind, set_by_subject, origin) VALUES ('claims-portal', 'preview', 'X', 'runtime', '\x00', '\x00', 'canyon', 'canyon', 'imported')`)
}

// The identity migration deletes every session and every owner who is nobody, and nothing else:
// the demo seed's owners carry email subjects and stay, so the seeded fleet keeps its unowned count.
func TestIdentityMigrationDropsAnonymousOwnersAndKeepsNamedOnes(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0015")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO projects (id) VALUES ('claims-portal'), ('quote-engine')`,
		`INSERT INTO project_owners (project_id, kind, subject, display) VALUES ('claims-portal', 'person', 'dana@meridian.example', 'Dana Whitfield'), ('quote-engine', 'person', 'anonymous', ''), ('quote-engine', 'person', 'ci-bot', 'CI')`, // ci-bot: a subject that is neither anonymous nor an address, so a shape-based delete would show
		`INSERT INTO sessions (id, actor_kind, actor_subject) VALUES ('s1', 'person', 'anonymous')`,
	} {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	var sessions int
	var owners string
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM sessions), (SELECT coalesce(string_agg(subject, ',' ORDER BY subject), '') FROM project_owners)`).Scan(&sessions, &owners); err != nil || sessions != 0 || owners != "ci-bot,dana@meridian.example" {
		t.Fatalf("after 0015: %d sessions, owners %q (%v); want none and the two named owners", sessions, owners, err)
	}
}

// 0035 makes today's owners owner grants (contracts/routes.md, App teams): a person on project_owners and
// a person who deployed, under an address — the subject's own, else their newest session's; an agent,
// even one with an address, and a subject with no address are left out; one person named twice is one
// grant, the owner row's.
func TestAppTeamsMigrationBackfillsPersonOwnersAndDeployers(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0035")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO projects (id) VALUES ('claims-portal'), ('quote-engine'), ('payroll')`,
		`INSERT INTO project_owners (project_id, kind, subject, display) VALUES ('claims-portal', 'person', 'Dana@Meridian.example', 'Dana Whitfield'),
			('claims-portal', 'agent', 'ci@meridian.example', 'CI'), ('quote-engine', 'person', 'sub-bob', 'Bob Ives'), ('payroll', 'person', 'sub-gone', 'Gone')`,
		`INSERT INTO sessions (id, actor_kind, actor_subject, email, created_at) VALUES ('s1', 'person', 'sub-bob', 'bob-old@meridian.example', now() - interval '1 day'),
			('s2', 'person', 'sub-bob', 'bob@meridian.example', now()), ('s3', 'person', 'sub-erin', 'erin@meridian.example', now())`,
		`INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_1', 'claims-portal', 't', 'c', 1, 'upload'), ('pkg_2', 'quote-engine', 't', 'c', 1, 'upload')`,
		`INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_1', 'claims-portal', 'pkg_1', 'clean'), ('gr_2', 'quote-engine', 'pkg_2', 'clean')`,
		`INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display) VALUES
			('claims-portal', 'preview', 'pkg_1', 'i', 'u', 'gr_1', 'person', 'dana@meridian.example', 'D.'), ('claims-portal', 'prod', 'pkg_1', 'i', 'u', 'gr_1', 'person', 'sub-erin', 'Erin Cho'),
			('quote-engine', 'preview', 'pkg_2', 'i', 'u', 'gr_2', 'agent', 'bot@meridian.example', 'Bot')`,
	} {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	const want = "claims-portal dana@meridian.example owner Dana@Meridian.example Dana Whitfield,claims-portal erin@meridian.example owner sub-erin Erin Cho,quote-engine bob@meridian.example owner sub-bob Bob Ives"
	for range 2 { // the second Migrate applies nothing and changes nothing
		if _, err := Migrate(ctx, p); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := p.QueryRow(ctx, `SELECT string_agg(concat_ws(' ', project_id, grantee, level, subject, display), ',' ORDER BY project_id, grantee) FROM project_access`).Scan(&got); err != nil || got != want {
			t.Fatalf("the owner grants after 0035:\n got %q (%v)\nwant %q", got, err, want)
		}
	}
}

// upTo is the embedded migrations before the named one: the schema a release before it left.
// 0071 keeps the App registered before it as the first of one per org (#1914): its installations and
// deliveries become its, and its account is the one it was first installed on.
func TestGitHubOrgsMigrationKeepsTheAppAlreadyRegistered(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0071")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO github_app (app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret) VALUES (9001, 'canyon-meridian', 'Iv1', 1, '\x00', '\x00', '\x00', '\x00')`,
		`INSERT INTO github_installations (installation_id, account, created_at) VALUES (42, 'dana', now() - interval '1 day'), (43, 'meridian', now())`,
		`INSERT INTO github_deliveries (delivery_id, event) VALUES ('d1', 'push')`,
	} {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := p.QueryRow(ctx, `SELECT a.account || ' ' || a.keeps_code || ' ' || a.imports_new || ' ' ||
		(SELECT count(*) FROM github_installations WHERE app_id = 9001) || ' ' || (SELECT count(*) FROM github_deliveries WHERE app_id = 9001) FROM github_app a`).Scan(&got); err != nil || got != "dana false false 2 1" {
		t.Fatalf("the App after 0071: %q %v", got, err)
	}
	// the binary before 0071, while a rollout runs both: its writes name no App, and are the one it knew
	for _, sql := range []string{
		`INSERT INTO github_installations (installation_id, account) VALUES (44, 'meridian')`,
		`INSERT INTO github_deliveries (delivery_id, event) VALUES ('d2', 'push')`,
		`INSERT INTO github_app (app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret) VALUES (9009, 's', 'c', 1, '', '', '', '') ON CONFLICT (id) DO NOTHING`,
	} {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if n := 0; p.QueryRow(ctx, "SELECT (SELECT count(*) FROM github_installations WHERE app_id = 9001) + (SELECT count(*) FROM github_deliveries WHERE app_id = 9001) + (SELECT count(*) FROM github_app)").Scan(&n) != nil || n != 6 {
		t.Fatalf("the old binary's rows: %d, want 3 installations, 2 deliveries and its App alone", n)
	}
}

func upTo(t *testing.T, before string) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, name := range names {
		if name < before {
			data, err := migrations.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = &fstest.MapFile{Data: data}
		}
	}
	return files
}

// An upgrade is not a setup: 0018 sets onboarded_at on the row of a host somebody has used — one
// with an audit trail, which every sign-in and every change writes — so a working install is not
// dropped into the setup state; a database nothing has touched stays null (design/ONBOARDING.md §2).
func TestOrgSettingsMigrationOnboardsAUsedHost(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0018")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO audit_events (id, actor_kind, actor_subject, action) VALUES ('ev_1', 'person', 'sub-dana', 'sign in')`); err != nil {
		t.Fatal(err)
	}
	if n, err := migrate(ctx, p, upTo(t, "migrations/0019")); err != nil || n != 1 {
		t.Fatalf("the upgrade applies 0018 alone: %d %v", n, err)
	}
	var onboarded bool
	if err := p.QueryRow(ctx, `SELECT onboarded_at IS NOT NULL FROM org_preferences`).Scan(&onboarded); err != nil || !onboarded {
		t.Fatalf("the upgrade left onboarded_at null: a working install dropped into the setup state (%v)", err)
	}
}

// A database that applied the report evidence as 0058_report_evidence.sql, as every one did before it
// was renumbered (the AWS test cluster among them), applies 0060 as a no-op rather than failing on its columns.
func TestTheRenumberedReportEvidenceRunsOnADatabaseThatHasIt(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE schema_migrations SET name = 'migrations/0058_report_evidence.sql' WHERE name = 'migrations/0060_report_evidence.sql'`); err != nil {
		t.Fatal(err)
	}
	if n, err := Migrate(ctx, p); err != nil || n != 1 {
		t.Fatalf("0060 runs once more over the columns 0058 added: applied %d, %v", n, err)
	}
}

// 0082 unbinds a grant whose subject is its own address — 0035's backfill wrote one where it had no subject
// — unless a sign-in ever carried that value as its subject (some providers' sub is the email): a session,
// or the trail's sign in row once the session is swept. A real subject stays.
func TestResponderHoldMigrationUnbindsOnlyAddressesNoSignInCarried(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0082")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO projects (id) VALUES ('claims-portal')`,
		`INSERT INTO project_access (project_id, kind, grantee, level, subject, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('claims-portal', 'email', 'ana@meridian.example', 'owner', 'ana@meridian.example', 'canyon', 'backfill', 'Canyon'),
			('claims-portal', 'email', 'ben@meridian.example', 'owner', 'ben@meridian.example', 'canyon', 'backfill', 'Canyon'),
			('claims-portal', 'email', 'cy@meridian.example', 'contributor', 'cy@meridian.example', 'canyon', 'backfill', 'Canyon'),
			('claims-portal', 'email', 'di@meridian.example', 'contributor', 'sub-di', 'canyon', 'backfill', 'Canyon')`,
		`INSERT INTO sessions (id, actor_kind, actor_subject, email) VALUES ('s1', 'person', 'ben@meridian.example', 'ben@meridian.example')`,
		`INSERT INTO audit_events (id, actor_kind, actor_subject, action) VALUES ('ev_1', 'person', 'cy@meridian.example', 'sign in')`,
	} {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := p.QueryRow(ctx, `SELECT string_agg(grantee || '=' || subject, ',' ORDER BY grantee) FROM project_access`).Scan(&got); err != nil ||
		got != "ana@meridian.example=,ben@meridian.example=ben@meridian.example,cy@meridian.example=cy@meridian.example,di@meridian.example=sub-di" {
		t.Fatalf("the grants after 0082: %q (%v)", got, err)
	}
}

// 0090 names each executor's version (#2271): a row a binary before it wrote, which names none, is that
// binary's canyon-v1, and running.
func TestAnExecutorRowFromBeforeItsVersionReadsCanyonV1(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, upTo(t, "migrations/0090")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO durable_executors (id) VALUES ('before')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	var version string
	var stopped bool
	if err := p.QueryRow(ctx, `SELECT version, stopped_at IS NOT NULL FROM durable_executors WHERE id = 'before'`).Scan(&version, &stopped); err != nil || version != "canyon-v1" || stopped {
		t.Fatalf("after 0090: version %q, stopped %v (%v); want canyon-v1, running", version, stopped, err)
	}
}

// No migration turns telemetry off (the owner, 2026-11-12: on by default): the row v0.9.0 left, on, stays on with its
// instance id and its last send, one an admin turned off stays off, and a new install's row is on.
func TestNoMigrationTurnsTelemetryOff(t *testing.T) {
	for name, want := range map[string]string{"NULL": "on", "'admin'": "admin"} {
		p, ctx := pgtest.Schema(t)
		if _, err := migrate(ctx, p, upTo(t, "migrations/0139")); err != nil { // v0.9.0's schema
			t.Fatal(err)
		}
		const id = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
		if _, err := p.Exec(ctx, `INSERT INTO telemetry (instance_id, off_because, claimed_at, sent_at, last_report) VALUES ('`+id+`', `+name+`, now() - interval '1 day', now() - interval '1 day', '{}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := Migrate(ctx, p); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := p.QueryRow(ctx, `SELECT coalesce(off_because, 'on') || ' ' || instance_id || ' ' || (sent_at IS NOT NULL) FROM telemetry`).Scan(&got); err != nil || got != want+" "+id+" true" {
			t.Errorf("a v0.9.0 row with off_because %s, migrated: %q %v", name, got, err)
		}
		if _, err := p.Exec(ctx, "DELETE FROM telemetry"); err != nil {
			t.Fatal(err)
		}
		if err := p.QueryRow(ctx, `INSERT INTO telemetry DEFAULT VALUES RETURNING coalesce(off_because, 'on')`).Scan(&got); err != nil || got != "on" {
			t.Errorf("a new install's row: %q %v", got, err)
		}
	}
}
