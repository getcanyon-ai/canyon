package events

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Page is what the two timeline routes return (contracts/events.md): rows by id ascending and
// the cursor for the next call, null once a run has ended and nothing follows.
type Page struct {
	Rows []Row  `json:"rows"`
	Next *int64 `json:"next"`
}

const (
	defaultLimit = 100
	maxLimit     = 200
)

// RunTimeline pages one run's rows after the exclusive cursor. "Ended" is runs.ended_at, never
// the state: a deploy keeps emitting build.log and deploy.completed after clean. End sets it
// once the last row has committed and closes the run to appends, so null here is final. Without requests — a
// reader who neither owns the app nor is an admin — a deploy's access-request rows (dedupe access:<email>) are
// left out, as if never written (contracts/events.md, build.log).
func RunTimeline(ctx context.Context, pool *pgxpool.Pool, run string, after int64, limit int, requests bool) (Page, error) {
	limit = clamp(limit)
	var ended bool
	// ended first, rows second: a row appended between the two reads lands on this page or the next, never behind a null
	err := pool.QueryRow(ctx, "SELECT ended_at IS NOT NULL FROM runs WHERE id = $1", run).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return Page{}, ErrNoRun
	}
	if err != nil {
		return Page{}, err
	}
	rows, err := pool.Query(ctx, `SELECT id, ts, run_id, project_id, kind, data FROM timeline_rows WHERE scope = 'run' AND run_id = $1 AND id > $2
		AND ($4 OR kind <> 'build.log' OR coalesce(dedupe, '') NOT LIKE 'access:%') ORDER BY id LIMIT $3`, run, after, limit+1, requests)
	if err != nil {
		return Page{}, err
	}
	page, more, err := collect(rows, after, limit)
	if err == nil && ended && !more {
		page.Next = nil
	}
	return page, err
}

// OrgTimeline pages the org feed after the exclusive cursor, the rows of the apps named, or of every
// app when apps is nil; the feed never ends.
func OrgTimeline(ctx context.Context, pool *pgxpool.Pool, after int64, limit int, apps []string) (Page, error) {
	limit = clamp(limit)
	rows, err := pool.Query(ctx, "SELECT id, ts, run_id, project_id, kind, data FROM timeline_rows WHERE scope = 'org' AND id > $1 AND ($3::text[] IS NULL OR project_id = ANY($3)) ORDER BY id LIMIT $2", after, limit+1, apps)
	if err != nil {
		return Page{}, err
	}
	page, _, err := collect(rows, after, limit)
	return page, err
}

func clamp(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	}
	return limit
}

// collect reads up to limit rows; the extra row the query asked for says whether more follow.
// next is the last id returned, or the incoming cursor when the page is empty.
func collect(rows pgx.Rows, after int64, limit int) (Page, bool, error) {
	defer rows.Close()
	page, next, more := Page{Rows: []Row{}}, after, false
	for rows.Next() {
		if len(page.Rows) == limit {
			more = true
			break
		}
		var r Row
		var ts time.Time
		var run *string
		var kind string
		if err := rows.Scan(&r.ID, &ts, &run, &r.Project, &kind, &r.Data); err != nil {
			return Page{}, false, err
		}
		r.Scope, r.Kind, r.TS = "org", Kind(kind), ts.UTC().Format(time.RFC3339)
		if run != nil {
			r.Scope, r.Run = "run", *run
		}
		page.Rows = append(page.Rows, r)
		next = r.ID
	}
	if err := rows.Err(); err != nil {
		return Page{}, false, err
	}
	page.Next = &next
	return page, more, nil
}
