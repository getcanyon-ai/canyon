package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// routesTimeline registers the two polled timelines (contracts/routes.md rows 55 and 72, seam 2):
// reads, so no audit row.
func routesTimeline(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /gate-runs/{id}/timeline", a.runTeamOnly(a.runTimeline))
	api.HandleFunc("GET /timeline", a.orgTimeline)
}

func (a *app) runTimeline(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := paging(w, r)
	if !ok {
		return
	}
	page, err := events.RunTimeline(r.Context(), a.pool, r.PathValue("id"), after, limit, a.readsRequests(r.Context(), r.PathValue("id")))
	if errors.Is(err, events.ErrNoRun) {
		writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// readsRequests is whether the caller reads the access-request rows of run's deploy: an admin, or an owner of its
// app (contracts/events.md, build.log). A level that cannot be read is false: the rows are left out.
func (a *app) readsRequests(ctx context.Context, run string) bool {
	if roleOf(ctx) == "admin" {
		return true
	}
	var project string
	err := a.pool.QueryRow(ctx, "SELECT project_id FROM runs WHERE id = $1", run).Scan(&project)
	level := ""
	if err == nil {
		level, err = levelOf(ctx, a.pool, project)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.log.Printf("canyon-server: the caller's level on run %s: %v", run, err)
	}
	return err == nil && level == "owner"
}

func (a *app) orgTimeline(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := paging(w, r)
	if !ok {
		return
	}
	apps, err := teamApps(r.Context(), a.pool) // a member is answered the rows of their team apps only (contracts/routes.md, App teams)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	page, err := events.OrgTimeline(r.Context(), a.pool, after, limit, apps)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// paging reads after and limit: non-negative integers, absent means the start and the default
// (the clamp is events'). A cursor that does not parse is refused, never ignored: a poller that
// silently restarted from zero would re-read its whole history.
func paging(w http.ResponseWriter, r *http.Request) (after int64, limit int, ok bool) {
	q := r.URL.Query()
	if s := q.Get("after"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_after", "after is a non-negative integer, the last row id seen", nil)
			return 0, 0, false
		}
		after = n
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_limit", "limit is a non-negative integer", nil)
			return 0, 0, false
		}
		limit = n
	}
	return after, limit, true
}
