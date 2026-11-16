package main

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
)

// logsTail is the last lines the server asks the executor for when the client names none, and
// maxLogsLines the most a client may (contracts/routes.md, the logs row).
const (
	logsTail     = 200
	maxLogsLines = 10000
	// The masked read's bounds (#2744's review): lines it may ask, and the bytes it holds to mask. Over the bytes it
	// is refused whole, never cut, since a cut can split a secret's value and leave its tail in the clear.
	maxMaskedLines = 200
	maxMaskedBytes = 1 << 20
)

// keepaliveLine is what the stream carries after logsKeepalive with nothing from the app
// (contracts/routes.md, the logs row): under the 60 s at which an ALB or nginx ends a quiet
// response, so a quiet app is not a dead stream (#1443). A var so a test can shorten it.
const keepaliveLine = "canyon: keepalive\n"

var logsKeepalive = 30 * time.Second

func routesLogs(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /projects/{id}/logs", a.teamOnly(a.projectLogs))
}

// projectLogs streams the container's stdout+stderr for the project's env, raw: the last lines,
// then, unless follow=false, live until the container stops or the client goes. The disclosure
// header says the stream is unredacted: an app that prints its own secret discloses it here,
// which is why this one read leaves an audit row.
// The row is written only when the read is served — after the executor has the container and
// before the first byte goes out — so a 400, 404 or 502 leaves none: nothing was disclosed, and
// the contract's row is "who read which environment's logs".
// redaction=masked is the dashboard's read (#2744): the last lines, never followed, masked as the status
// read masks an app's last line; any other redaction is 400, never a raw read (fail closed).
func (a *app) projectLogs(w http.ResponseWriter, r *http.Request) {
	env := r.URL.Query().Get("env")
	switch env {
	case "":
		env = "preview"
	case "preview", "prod":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return
	}
	lines := logsTail
	if s := r.URL.Query().Get("lines"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLogsLines {
			writeErr(w, http.StatusBadRequest, "invalid_lines", "lines is 1 to 10000", nil)
			return
		}
		lines = n
	}
	masked := false
	switch r.URL.Query().Get("redaction") {
	case "", "none":
	case "masked":
		masked = true
	default:
		writeErr(w, http.StatusBadRequest, "invalid_redaction", "redaction is none or masked", nil)
		return
	}
	if masked && lines > maxMaskedLines {
		writeErr(w, http.StatusBadRequest, "invalid_lines", "a masked read is 1 to 200 lines", nil)
		return
	}
	live := !masked // follow=false: the last lines and the end of the body
	switch r.URL.Query().Get("follow") {
	case "":
	case "true":
		if masked {
			writeErr(w, http.StatusBadRequest, "invalid_follow", "a masked read ends after its lines: follow is false", nil)
			return
		}
		live = true
	case "false":
		live = false
	default:
		writeErr(w, http.StatusBadRequest, "invalid_follow", "follow is true or false", nil)
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	var deployed bool
	if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = $2)", id, env).Scan(&deployed); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !deployed {
		writeErr(w, http.StatusNotFound, "not_found", "nothing deployed in "+env, nil)
		return
	}
	if a.pipe == nil || a.pipe.Executor == nil {
		a.log.Printf("canyon-server: logs %s/%s: no executor configured", id, env)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	stream, err := a.pipe.Executor.Logs(ctx, id, env, lines, live) // the container is <project>-<env>: the path's id, never a query value
	if errors.Is(err, executor.ErrNoContainer) {
		writeErr(w, http.StatusBadGateway, "app_unreachable", "the app's container is gone", nil)
		return
	}
	if err != nil {
		a.log.Printf("canyon-server: logs %s/%s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = stream.Close() }()
	if masked {
		a.maskedLogs(w, r, id, env, stream)
		return
	}
	if err := a.auditRead(r, id, env); err != nil {
		a.log.Printf("canyon-server: logs %s/%s: audit: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Canyon-Log-Redaction", "none")
	w.Header().Set("X-Content-Type-Options", "nosniff") // the app's own bytes, on Canyon's origin: text, never sniffed as markup
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush() // the status and the disclosure leave now: a container that is up and silent is not a dead one
	if err := follow(w, rc.Flush, stream, logsKeepalive); !errors.Is(err, io.EOF) && ctx.Err() == nil {
		a.log.Printf("canyon-server: logs %s/%s: stream: %v", id, env, err)
	}
}

// maskedLogs answers the last lines whole, masked with env's runtime values by the status read's rule
// (runs.Pipeline.Masked), so a value Canyon knows is a secret never reaches the answer. A log Canyon can't
// mask that way, a secret of env set or removed since what runs there started or a start with no record, is 409 and
// no line. One over maxMaskedBytes is 422 and no line: it is never cut to fit.
func (a *app) maskedLogs(w http.ResponseWriter, r *http.Request, id, env string, stream io.Reader) {
	raw, err := io.ReadAll(io.LimitReader(stream, maxMaskedBytes+1))
	if err == nil && len(raw) > maxMaskedBytes {
		writeErr(w, http.StatusUnprocessableEntity, "too_long_to_mask", "the last lines are over 1 MiB, too much to check for secrets: canyon logs --env "+env+" shows them", nil)
		return
	}
	said := ""
	if err == nil {
		said, err = a.pipe.Masked(r.Context(), id, env, string(raw))
	}
	if err == nil && said == "" && len(raw) > 0 {
		writeErr(w, http.StatusConflict, "not_maskable", "Canyon can't tell which secret values the app in "+env+" started with, so it can't mask its log", nil)
		return
	}
	if err == nil {
		err = a.auditRead(r, id, env)
	}
	if err != nil {
		a.log.Printf("canyon-server: logs %s/%s: masked: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Canyon-Log-Redaction", "masked")
	w.Header().Set("X-Content-Type-Options", "nosniff") // text, never sniffed as markup
	_, _ = io.WriteString(w, said)                      //nolint:gosec // G705: plain text with nosniff, which the dashboard draws as text, never markup
}

// follow copies r to w as it arrives, flushing each piece, until r ends or a write fails. After
// every quiet stretch of `every` it writes keepaliveLine, but only where the last line has ended,
// never inside one the app has not finished.
func follow(w io.Writer, flush func() error, r io.Reader, every time.Duration) error {
	type piece struct {
		b   []byte
		err error
	}
	pieces, done := make(chan piece), make(chan struct{})
	defer close(done)
	go func() { // r has no deadline: a reader of its own, so silence can be timed
		for {
			buf := make([]byte, 32<<10)
			n, err := r.Read(buf)
			select {
			case pieces <- piece{buf[:n], err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ended := true // nothing written yet: a line boundary
	quiet := time.NewTimer(every)
	defer quiet.Stop()
	for {
		select {
		case p := <-pieces:
			if len(p.b) > 0 {
				if _, err := w.Write(p.b); err != nil {
					return err
				}
				if err := flush(); err != nil {
					return err
				}
				ended = p.b[len(p.b)-1] == '\n'
			}
			if p.err != nil {
				return p.err
			}
		case <-quiet.C:
			if ended {
				if _, err := io.WriteString(w, keepaliveLine); err != nil {
					return err
				}
				if err := flush(); err != nil {
					return err
				}
			}
		}
		quiet.Reset(every)
	}
}

// auditRead writes the one row of a served log read: the reader and the env, never a line.
func (a *app) auditRead(r *http.Request, id, env string) error {
	ctx := r.Context()
	tx, err := a.mutation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.AuditTx(ctx, tx, actorOf(ctx), action(r), id, "", map[string]any{"env": env}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
