package runs

import (
	"context"
	"strconv"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// Shared is how far the app the SQL expression project names is shared, as three columns: everyone, whether
// everyone at the org can open it; people, its email grants below owner; group_grants, its group grants — the
// shared line's parts (design/EXPERIENCE.md D5), which the fleet's Access column and a deploy's access line say.
func Shared(project string) string {
	return `EXISTS (SELECT 1 FROM project_access e WHERE e.project_id = ` + project + ` AND e.kind = 'everyone') AS everyone,
	(SELECT count(*) FROM project_access s WHERE s.project_id = ` + project + ` AND s.kind = 'email' AND s.level <> 'owner') AS people,
	(SELECT count(*) FROM project_access g WHERE g.project_id = ` + project + ` AND g.kind = 'group') AS group_grants`
}

// privateLine is the shared line of an app nobody but its owners and admins can open (design/EXPERIENCE.md D5).
const privateLine = "Private: only its owners and admins can open it. Share it: canyon share <email> · canyon share everyone"

// signInOffLine is a deploy's access line on a host with sign-in off, where anyone who reaches it opens every app
// (design/EXPERIENCE.md D5; #2498): the first sentence of canyon share's line there, verbatim.
const signInOffLine = "Sign-in is off on this Canyon, so anyone who can reach it can open every app."

// noDescription is the deploy's line when the deployed package has no description (design/EXPERIENCE.md D5).
const noDescription = "No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."

// sharedLine is the shared line (design/EXPERIENCE.md D5), org the org's name.
func sharedLine(everyone bool, people, groups int, org string) string {
	if everyone {
		return "Shared with everyone at " + org + "."
	}
	var parts []string
	for _, p := range []struct {
		n         int
		one, many string
	}{{people, "person", "people"}, {groups, "group", "groups"}} {
		switch {
		case p.n == 1:
			parts = append(parts, "1 "+p.one)
		case p.n > 1:
			parts = append(parts, strconv.Itoa(p.n)+" "+p.many)
		}
	}
	if len(parts) == 0 {
		return privateLine
	}
	return "Shared with " + strings.Join(parts, " and ") + "."
}

// ago is a request's age in whole minutes, hours or days: 2h.
func ago(seconds int64) string {
	switch {
	case seconds < 3600:
		return strconv.FormatInt(seconds/60, 10) + "m"
	case seconds < 86400:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	}
	return strconv.FormatInt(seconds/86400, 10) + "d"
}

// accessLines writes a deploy run's access as build.log rows once its deploy.completed row is written, each
// once (contracts/events.md, build.log): the shared line; when the run's actor owned the app or was an admin
// at its start (Input.Requests), one row per pending access request, oldest first; then, when the deployed
// package has no description, the line saying so. A run with no deploy.completed row writes none.
func (p *Pipeline) accessLines(ctx context.Context, in Input) error {
	org := p.Org(ctx) // before the lock: it reads on a connection of its own (#1693)
	return p.write(ctx, func(tx pgx.Tx) error {
		var deployed, everyone bool
		var people, groups int
		var description string
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'deploy:preview'),`+Shared("$2")+`,
			coalesce((SELECT description FROM packages WHERE project_id = $2 AND id = $3), '')`, in.Run, in.Project, in.Package).Scan(&deployed, &everyone, &people, &groups, &description); err != nil || !deployed {
			return err
		}
		lines := [][2]string{{"access", sharedLine(everyone, people, groups, org)}}
		if p.SignInOff { // anyone who reaches the host opens the app: no share line, and no request to name
			lines[0][1] = signInOffLine
		}
		if in.Requests && !p.SignInOff {
			// clock_timestamp, as a row's ts is (events.go): a request that landed while this transaction waited on the
			// lock is newer than its now(); and never a negative age
			rows, _ := tx.Query(ctx, `SELECT email, greatest(0, extract(epoch FROM clock_timestamp() - requested_at))::bigint, coalesce(level, '') FROM access_requests WHERE project_id = $1 ORDER BY requested_at, email`, in.Project)
			asked, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
				Email string
				Age   int64
				Level string // none or viewer when Canyon recorded it (#2774)
			}])
			if err != nil {
				return err
			}
			for _, r := range asked {
				share := "canyon share " + r.Email
				if r.Level == "viewer" { // a read-only member: a plain share adds them read-only again and changes nothing
					share += " --level contributor"
				}
				lines = append(lines, [2]string{"access:" + r.Email, "Access requests: " + r.Email + ", " + ago(r.Age) + " ago · " + share})
			}
		}
		if description == "" {
			lines = append(lines, [2]string{"description", noDescription})
		}
		for _, l := range lines {
			if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, l[0], map[string]any{"line": l[1]}); err != nil {
				return err
			}
		}
		return nil
	})
}
