package runs

import (
	"context"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/jackc/pgx/v5"
)

// A restore without role passwords leaves the app's login with none while Canyon keeps the URL it issued:
// the re-issue gives every built-in database a fresh password and replaces the provisioned DATABASE_URL,
// the next deploy hands it over, and the app reads the rows it wrote before (contracts/provisioner.md,
// After a restore; #2014).
func TestReissueGivesEveryBuiltinDatabaseAFreshPassword(t *testing.T) {
	admin := pgtest.Exclusive(t)
	b, err := provisioners.NewBuiltin(context.Background(), admin, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.Delete(context.Background(), dbKey); b.Close() })
	p, ctx, f := declaringDB(t, b)
	deployOnce(t, ctx, p)
	first := handed(f)
	app, err := pgx.Connect(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, "CREATE TABLE claims (id int); INSERT INTO claims VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	login := app.Config().User
	_ = app.Close(ctx)
	conn, err := pgx.Connect(ctx, admin) // what a restore with --no-role-passwords leaves: the login, no password
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "ALTER ROLE "+pgx.Identifier{login}.Sanitize()+" PASSWORD NULL"); err != nil {
		t.Fatal(err)
	}
	if lost, err := pgx.Connect(ctx, first); err == nil {
		_ = lost.Close(ctx)
		t.Fatal("the issued URL still signs in after the restore")
	}

	var out strings.Builder
	if n, err := Reissue(ctx, p, pipe.Vault, b, "http://canyon.meridian.example", &out); err != nil || n != 1 || !strings.Contains(out.String(), "re-issued 1 app database password\n") {
		t.Fatalf("re-issued %d (%v):\n%s", n, err, out.String())
	}
	var runs string // the running preview's version, as the line names it
	if err := p.QueryRow(ctx, "SELECT coalesce(k.label, substr(k.id, 5)) FROM deploys d JOIN packages k ON k.id = d.package_id AND k.project_id = d.project_id WHERE d.project_id = $1 AND d.env = 'preview'", project).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if want := "  " + project + " preview runs " + runs + ": Deploy again on http://canyon.meridian.example/#/apps/" + project + ", or run canyon deploy from its code"; !strings.Contains(out.String(), want) {
		t.Fatalf("the running preview, and what brings it back:\n%s\nwant %q", out.String(), want)
	}
	deployOnce(t, ctx, p)
	again := handed(f)
	if again == first {
		t.Fatal("the next deploy handed the old URL")
	}
	app, err = pgx.Connect(ctx, again)
	if err != nil {
		t.Fatalf("the re-issued URL does not sign in: %v", err)
	}
	defer func() { _ = app.Close(ctx) }()
	var id int
	if err := app.QueryRow(ctx, "SELECT id FROM claims").Scan(&id); err != nil || id != 7 {
		t.Fatalf("the app's rows: %d (%v)", id, err)
	}
	var version, audits int
	if err := p.QueryRow(ctx, `SELECT (SELECT version FROM secrets WHERE env = 'preview' AND name = 'DATABASE_URL'),
		(SELECT count(*) FROM audit_events WHERE action = 'reissue resource password' AND actor_subject = 'provisioner' AND detail = '{"name": "db", "type": "postgres", "env": "preview"}')`).Scan(&version, &audits); err != nil || version != 2 || audits != 1 {
		t.Fatalf("the row's version %d, %d audit row(s) (%v)", version, audits, err)
	}
}

// The count is said in a real plural, zero included (#2175).
func TestReissuedSaysARealPlural(t *testing.T) {
	for n, want := range map[int]string{0: "re-issued 0 app database passwords", 1: "re-issued 1 app database password", 2: "re-issued 2 app database passwords"} {
		if got := reissued(n); got != "reissue-db-passwords: "+want {
			t.Errorf("%d: %q", n, got)
		}
	}
}
