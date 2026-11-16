package db

import (
	"context"
	"io/fs"
	"maps"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A column as the schema catalogue describes it, reduced to what decides whether the previous
// binary survives the migration: does an INSERT that does not name the column still succeed.
type column struct {
	nullable   bool
	hasDefault bool
}

// A schema is every table in the test's search_path with its columns, read from
// information_schema after a migration ran. Postgres is the parser; nothing here reads SQL.
type schema map[string]map[string]column

// A contraction is a table or column a migration removed. Each entry names the release that
// shipped it and why the binary before it survived; it grows only by a drop no released binary reads.
type contraction struct{ migration, table, column string }

var allowedContractions = map[contraction]struct{}{
	// 0003 moves owners into project_owners and drops the column in the same file. Nothing read
	// projects.owner before the move, so the previous binary lost nothing (0003_sessions_audit.sql:40).
	{"migrations/0003_sessions_audit.sql", "projects", "owner"}: {},
	// 0077 drops the AI judgement's table and column (#1421): no released binary ever read or wrote
	// them (the code went before v0.1.0), so the previous binary loses nothing.
	{"migrations/0077_drop_judgements.sql", "judgement_edits", ""}:        {},
	{"migrations/0077_drop_judgements.sql", "review_cache", "judgements"}: {},
	// 0084 replaces egress's minute rows and hourly roll-ups with one row per destination (#2222): no
	// release shipped them (0079 came after v0.1.12); a binary since writes them only with the receiver
	// on, which no host runs while egress is inert, and reads them only in an app's egress read, which no
	// screen or CLI verb calls yet — it fails for the seconds both binaries run, and nothing else does.
	{"migrations/0084_egress_destinations.sql", "egress_minutes", ""}: {},
	{"migrations/0084_egress_destinations.sql", "egress_hours", ""}:   {},
	// 0137 drops telemetry's seen_at (the owner, 2026-11-12: on by default, no admin's seen first): 0131 came after
	// v0.8.0, so no released binary reads it.
	{"migrations/0137_telemetry_on_by_default.sql", "telemetry", "seen_at"}: {},
}

// A running server and the one replacing it share the database while the new one migrates, so
// every migration must leave the previous binary's reads and inserts working: expand only. The
// test applies the files one at a time into one schema and diffs the catalogue after each, so a
// failure names the migration and the table or column. Two rules: nothing that existed before a
// migration may be gone after it, and a column added to an existing table must be nullable or
// carry a default, or the previous binary's INSERTs, which do not name it, would fail.
func TestEveryMigrationIsExpandOnly(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	upTo := fstest.MapFS{}
	before := snapshot(t, ctx, p)
	for _, name := range names {
		data, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		upTo[name] = &fstest.MapFile{Data: data}
		if _, err := migrate(ctx, p, upTo); err != nil {
			t.Fatal(err)
		}
		after := snapshot(t, ctx, p)
		for _, table := range slices.Sorted(maps.Keys(before)) {
			if _, ok := after[table]; !ok {
				if _, allowed := allowedContractions[contraction{name, table, ""}]; !allowed {
					t.Errorf("%s drops table %s: the previous binary still reads it. Expand only; drop in a later release, after nothing reads it", name, table)
				}
				continue
			}
			for _, col := range slices.Sorted(maps.Keys(before[table])) {
				if _, ok := after[table][col]; !ok {
					if _, allowed := allowedContractions[contraction{name, table, col}]; !allowed {
						t.Errorf("%s drops column %s.%s: the previous binary still reads it. Expand only; drop in a later release, after nothing reads it", name, table, col)
					}
				}
			}
			for _, col := range slices.Sorted(maps.Keys(after[table])) {
				// A column the previous binary's INSERTs can omit: absent then, nullable then, or
				// defaulted then. Made NOT NULL with no default now — added that way, or tightened
				// by SET NOT NULL or DROP DEFAULT — those INSERTs fail.
				was, existed := before[table][col]
				omittable := !existed || was.nullable || was.hasDefault
				if c := after[table][col]; omittable && !c.nullable && !c.hasDefault {
					t.Errorf("%s leaves %s.%s NOT NULL without a default: the previous binary's INSERTs do not name it and would fail. Add or tighten it nullable or with a default; tighten further in a later release", name, table, col)
				}
			}
		}
		before = after
	}
}

// snapshot reads the tables and columns of the connection's schema. The pool's search_path is
// the test's schema, so current_schema() scopes both catalogue views to it.
func snapshot(t *testing.T, ctx context.Context, p *pgxpool.Pool) schema {
	t.Helper()
	s := schema{}
	rows, err := p.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		s[table] = map[string]column{}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	cols, err := p.Query(ctx, `SELECT table_name, column_name, is_nullable = 'YES', column_default IS NOT NULL FROM information_schema.columns WHERE table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer cols.Close()
	for cols.Next() {
		var table, name string
		var c column
		if err := cols.Scan(&table, &name, &c.nullable, &c.hasDefault); err != nil {
			t.Fatal(err)
		}
		s[table][name] = c
	}
	if cols.Err() != nil {
		t.Fatal(cols.Err())
	}
	return s
}
