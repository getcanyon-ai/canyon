package main

import (
	"context"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

// pipeline is the request chain of contracts/routes.md as far as v1 needs it: recovery, then
// logging, then the session stage on /api/v1, then the handler, whose transaction carries the
// audit row. There is no CORS stage: the dashboard is served from this origin. Every request,
// Open ones included, produces one log line but a poll answered 200 (polled), and the line
// names the actor when a session resolved one: a refused request is a log line, never an audit
// row (design/ARCHITECTURE.md).
func pipeline(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, quiet := time.Now(), polled(r)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		who := new(actor) // filled by the session stage, read by the log line below
		r = r.WithContext(context.WithValue(r.Context(), whoKey{}, who))
		defer func() {
			p := recover()
			if p != nil {
				if p == http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity, as net/http does
					panic(p)
				}
				logger.Printf("panic %s %s: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
				if !sw.wrote {
					writeErr(sw, http.StatusInternalServerError, "internal", "internal error", nil)
				}
			}
			if quiet && p == nil && sw.status == http.StatusOK {
				return
			}
			if who.Kind == "" {
				logger.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
			} else {
				logger.Printf("%s %s %d %s %s/%s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), who.Kind, who.Subject)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// polls are the routes a client asks again every second or so: the two timelines (routes_timeline.go), which a
// deploy's poll read so often it wrote over a thousand lines while it built and buried the line saying the demo
// was ready (#2697), and the reads an open app page asks every 2 s, its project, status and runs and the newest run
// (web/src/screens/App.tsx; routes_projects.go, routes_promote.go, routes_gate.go), 90 lines a minute for as long as
// the page is open (#2711). Matched as the server's own mux matches them, never by a guess at the path.
var polls = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, pattern := range []string{"GET /api/v1/timeline", "GET /api/v1/gate-runs/{id}/timeline",
		"GET /api/v1/projects/{id}", "GET /api/v1/projects/{id}/status", "GET /api/v1/projects/{id}/gate-runs", "GET /api/v1/gate-runs/{id}"} {
		m.Handle(pattern, http.NotFoundHandler())
	}
	return m
}()

// polled is whether r asks one of the polls: answered 200 it writes no log line, and any other answer does.
func polled(r *http.Request) bool {
	_, pattern := polls.Handler(r)
	return pattern != ""
}

// statusWriter remembers what was answered, for the log line and for recovery.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	s.status, s.wrote = code, true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets net/http's ResponseController reach the real writer: routes that stream flush
// through this wrapper.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// whoKey holds the *actor the log line reads; the session stage fills it.
type whoKey struct{}
