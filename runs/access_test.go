package runs

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// accessRows is the build.log lines a deploy run wrote after its deploy.completed row, which must be followed
// by its terminal state and nothing else (contracts/events.md, build.log).
func accessRows(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) []string {
	t.Helper()
	page, err := events.RunTimeline(ctx, p, run, 0, 200, true)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	after := false
	for _, r := range page.Rows {
		switch {
		case r.Kind == events.DeployCompleted:
			after = true
		case !after || r.Kind == events.Progress:
		case r.Kind == events.BuildLog:
			var d struct{ Line string }
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, d.Line)
		case r.Kind == events.RunState:
			return lines
		default:
			t.Fatalf("a %s row between deploy.completed and the terminal state", r.Kind)
		}
	}
	t.Fatalf("no deploy.completed row and terminal state after it: %v", lines)
	return nil
}

// A deploy run tells its builder who can open the app, after its deploy.completed row and before its
// terminal state, in D5's words (contracts/events.md, build.log; design/EXPERIENCE.md): the shared line,
// then — to an actor who owns the app or is an admin (design/PERMISSIONS.md, Access requests) — each pending
// request, oldest first, then the description's line when the package has none. Each step is its own
// subtest on the app the steps before left: one that fails does not hide the next.
func TestADeployRunWritesTheAppsAccessAfterItsDeployCompletedRow(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	deploy := func(t *testing.T, requests bool) (string, []string) {
		t.Helper()
		r, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Requests: requests})
		if err != nil {
			t.Fatal(err)
		}
		if state, reason := waitEnded(t, ctx, p, r.ID); state != "clean" || reason != nil {
			t.Fatalf("a deploy run ends clean: %s %s", state, why(reason))
		}
		return r.ID, accessRows(t, ctx, p, r.ID)
	}
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	buildLogs := func(t *testing.T, run string) (n int) {
		t.Helper()
		if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log'", run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	none := "No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."
	shared := "Shared with 3 people and 1 group."
	asked := []string{"Access requests: lee@meridian.example, 2h ago · canyon share lee@meridian.example",
		"Access requests: sam@meridian.example, 5m ago · canyon share sam@meridian.example --level contributor"} // read-only: a plain share changes nothing (#2774)

	t.Run("a fresh app: private, and no description yet", func(t *testing.T) {
		if _, got := deploy(t, true); !slices.Equal(got, []string{"Private: only its owners and admins can open it. Share it: canyon share <email> · canyon share everyone", none}) {
			t.Fatalf("a fresh app's access: %q", got)
		}
	})
	exec(t, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		($1, 'email', 'dana@meridian.example', 'owner', 'canyon', 'backfill', 'Canyon'),
		($1, 'email', 'ann@meridian.example', 'contributor', 'canyon', 'backfill', 'Canyon'),
		($1, 'email', 'bo@meridian.example', 'viewer', 'canyon', 'backfill', 'Canyon'),
		($1, 'email', 'cy@meridian.example', 'viewer', 'canyon', 'backfill', 'Canyon'),
		($1, 'group', 'claims', 'owner', 'canyon', 'backfill', 'Canyon')`, project)
	exec(t, `INSERT INTO access_requests (project_id, email, subject, requested_at, level) VALUES
		($1, 'sam@meridian.example', '', now() - interval '5 minutes', 'viewer'),
		($1, 'lee@meridian.example', '', now() - interval '2 hours 10 minutes', NULL)`, project)
	exec(t, "UPDATE packages SET description = 'Claims intake for the adjusters' WHERE id = $1", pkg)

	t.Run("an owner's or an admin's deploy names each request, oldest first, and a replay writes each row once", func(t *testing.T) {
		run, got := deploy(t, true)
		if want := append([]string{shared}, asked...); !slices.Equal(got, want) {
			t.Fatalf("an owner's deploy of a shared app:\n got %q\nwant %q", got, want)
		}
		before := buildLogs(t, run)
		if err := pipe.accessLines(ctx, Input{Run: run, Project: project, Package: pkg, Requests: true}); err != nil || buildLogs(t, run) != before {
			t.Fatalf("the step again, as a replay runs it: %v, %d build.log rows, then %d", err, before, buildLogs(t, run))
		}
	})
	t.Run("a deploy started without Requests, as a GitHub push's is (github-app/work.go), names no request", func(t *testing.T) {
		run, got := deploy(t, false)
		var rows int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND dedupe LIKE 'access:%'", run).Scan(&rows); err != nil || rows != 0 || !slices.Equal(got, []string{shared}) {
			t.Fatalf("a deploy without Requests while two requests are pending: %q, %d request rows (%v)", got, rows, err)
		}
	})
	t.Run("a request newer than the run's transaction is 0m old, never a negative age", func(t *testing.T) {
		exec(t, "INSERT INTO access_requests (project_id, email, subject, requested_at) VALUES ($1, 'kim@meridian.example', '', clock_timestamp() + interval '10 minutes')", project)
		defer exec(t, "DELETE FROM access_requests WHERE email = 'kim@meridian.example'")
		if _, got := deploy(t, true); !slices.Equal(got, append(append([]string{shared}, asked...), "Access requests: kim@meridian.example, 0m ago · canyon share kim@meridian.example")) {
			t.Fatalf("a request that landed while the run waited: %q", got)
		}
	})

	exec(t, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ($1, 'everyone', '', 'viewer', 'canyon', 'backfill', 'Canyon')`, project)
	exec(t, "UPDATE packages SET description = NULL WHERE id = $1", pkg)
	t.Run("everyone at the org, said by the org's name (Pipeline.Org)", func(t *testing.T) {
		if _, got := deploy(t, false); !slices.Equal(got, []string{"Shared with everyone at Meridian.", none}) {
			t.Fatalf("an app everyone can open: %q", got)
		}
	})

	t.Run("a run with no deploy.completed row writes none of them", func(t *testing.T) {
		exec(t, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_nodeploy', $1, $2, 'running')", project, pkg)
		if err := pipe.accessLines(ctx, Input{Run: "gr_nodeploy", Project: project, Package: pkg, Requests: true}); err != nil || buildLogs(t, "gr_nodeploy") != 0 {
			t.Fatalf("a run nothing was deployed by: %v, %d build.log rows", err, buildLogs(t, "gr_nodeploy"))
		}
	})
}

// On a host with sign-in off (off, demo) anyone who reaches it opens every app: a deploy says so, D5's line in place
// of the shared one, with no request after it, whoever deploys (contracts/events.md, build.log; #2498).
func TestADeployUnderSignInOffSaysAnyoneWhoReachesTheHostCanOpenIt(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	pipe.SignInOff = true
	t.Cleanup(func() { pipe.SignInOff = false })
	if _, err := p.Exec(ctx, "INSERT INTO access_requests (project_id, email, subject, requested_at) VALUES ($1, 'sam@meridian.example', '', now())", project); err != nil {
		t.Fatal(err)
	}
	r, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Requests: true})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, r.ID); state != "clean" || reason != nil {
		t.Fatalf("a deploy run ends clean: %s %s", state, why(reason))
	}
	want := []string{"Sign-in is off on this Canyon, so anyone who can reach it can open every app.",
		"No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."}
	if got := accessRows(t, ctx, p, r.ID); !slices.Equal(got, want) {
		t.Fatalf("a deploy under sign-in off:\n got %q\nwant %q", got, want)
	}
}

func TestTheSharedLineSaysEachPartThatIsNotZero(t *testing.T) {
	for _, c := range []struct {
		everyone       bool
		people, groups int
		org, want      string
	}{
		{false, 0, 0, "Meridian", privateLine},
		{false, 1, 0, "Meridian", "Shared with 1 person."},
		{false, 0, 2, "Meridian", "Shared with 2 groups."},
		{false, 3, 1, "Meridian", "Shared with 3 people and 1 group."},
		{true, 3, 1, "Meridian", "Shared with everyone at Meridian."},
	} {
		if got := sharedLine(c.everyone, c.people, c.groups, c.org); got != c.want {
			t.Errorf("sharedLine(%v, %d, %d, %q) = %q, want %q", c.everyone, c.people, c.groups, c.org, got, c.want)
		}
	}
	for s, want := range map[int64]string{59: "0m", 300: "5m", 3599: "59m", 3600: "1h", 7800: "2h", 86399: "23h", 86400: "1d", 3 * 86400: "3d"} {
		if got := ago(s); got != want {
			t.Errorf("ago(%d) = %q, want %q", s, got, want)
		}
	}
}
