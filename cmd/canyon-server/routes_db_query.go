package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// The query's bounds (contracts/routes.md, Database sessions; db-query.schema.json).
const (
	dbQueryMaxSQL     = 64 << 10 // a statement's bytes, at most
	dbQueryLimit      = 500      // rows, when the request names none
	dbQueryMaxLimit   = 10000
	dbQueryMaxBytes   = 10 << 20 // the rows' text, at most: cut at a row boundary past it
	dbQueryTimeout    = 30 * time.Second
	dbQueryMinTimeout = time.Second
	dbQueryMaxTimeout = 5 * time.Minute
	sqlstateCanceled  = "57014" // query_canceled: what statement_timeout ends a statement with
	// dbQueryMaxMessage bounds every message the query's connection reads: a row of dbQueryMaxBytes and its framing,
	// a count and a length for each of Postgres's at most 1664 columns. pgx's own bound is about 1 GiB, read whole
	// before a value is seen, so without it one row could make the server hold a gigabyte (#2813's review).
	dbQueryMaxMessage = dbQueryMaxBytes + 2 + 4*1664
	sqlstateTooBig    = "54000" // program_limit_exceeded: a row over the answer's cap, which the connection would not read
)

func routesDBQuery(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects/{id}/db/query", a.teamOnly(a.queryDB))
}

// queryDB runs one statement as the person in their database session on one of the app's databases, which it
// opens or attaches to as the open route does, through dbOpen, so its refusals are the open's own, in the open's
// order (#2118). The open or attach commits before the statement runs, so a statement of minutes holds neither the
// person role's lock nor the trail's. Then the checks judge the session, its end time first, and the statement runs
// on the one connection its login made: Parse, then Execute, in a read-only transaction in read mode, under a
// statement_timeout that never outlasts the session. Whatever it answers, the counters count it. The statement is
// never logged or stored, and the login's password lives in that connection alone.
func (a *app) queryDB(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.dbPerson(w, r, id) {
		return
	}
	var body struct {
		dbOpenBody
		SQL     string  `json:"sql"`
		Limit   *int    `json:"limit"`
		Timeout *string `json:"timeout"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	limit, timeout := dbQueryLimit, dbQueryTimeout
	switch {
	case body.SQL == "":
		writeErr(w, http.StatusBadRequest, "invalid_sql", "the statement is empty", nil)
		return
	case len(body.SQL) > dbQueryMaxSQL:
		writeErr(w, http.StatusBadRequest, "invalid_sql", "the statement is over 64 KiB", nil)
		return
	case body.Limit != nil && (*body.Limit < 1 || *body.Limit > dbQueryMaxLimit):
		writeErr(w, http.StatusBadRequest, "invalid_limit", "limit is 1 to 10000 rows", nil)
		return
	case body.Limit != nil:
		limit = *body.Limit
	}
	if body.Timeout != nil {
		d, err := time.ParseDuration(*body.Timeout)
		if err != nil || d < dbQueryMinTimeout || d > dbQueryMaxTimeout {
			writeErr(w, http.StatusBadRequest, "invalid_timeout", "timeout is 1s to 5m, as 30s or 1m30s", nil)
			return
		}
		timeout = d
	}
	// its one connection holds a slot of the host's and the person's caps while it is open, judged before the login:
	// past one, nothing runs and the database session stays as it was (§6, Caps; #2814)
	release, over := a.relays.reserve(actorOf(ctx).Subject, a.dbHostCap())
	if over != "" {
		writeErr(w, http.StatusTooManyRequests, "busy", busySaid(over, a.dbHostCap()), nil)
		return
	}
	defer release() // after every return below, each of which closes the connection first
	var conn *pgx.Conn
	counted := &countingConn{}
	_, s, ok := a.dbOpen(w, r, id, body.dbOpenBody, func(ctx context.Context, issuer provisioners.Issuer, l provisioners.Login, sid string) (err error) {
		if body.Mode == "read" { // READ ONLY holds this connection alone, never a second one opened from inside the database (#2918)
			if err := issuer.NoSecondConnection(ctx, l); err != nil {
				return err
			}
		}
		conn, err = dbConnect(ctx, l, sid, "query", nil, counted)
		return err
	})
	if !ok {
		if conn != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
		}
		return
	}
	if conn == nil { // dbOpen issued no connect: never a statement run as anyone else
		a.log.Printf("canyon-server: db query on %s %s: no connection to run it on", id, s.env)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	// the checks, every query (contracts/routes.md, the checks): the session's end time first, so a statement never
	// runs past it; a session that fails one is closed, and nothing runs. One that can't be judged runs nothing and
	// closes nothing: the sweep closes a session whose check keeps failing (#2865)
	if reason, err := a.dbCheck(ctx, *s, time.Now()); err != nil || reason != "" {
		_ = conn.Close(context.WithoutCancel(ctx))
		if err != nil {
			a.log.Printf("canyon-server: database session %s: its check: %v", s.id, err)
			writeErr(w, http.StatusServiceUnavailable, "check_failed", dbQueryUnchecked, nil)
			return
		}
		if err := a.closeDB(context.WithoutCancel(ctx), *s, reason, canyonDB, false); err != nil {
			a.log.Printf("canyon-server: database session %s: closing it %s: %v", s.id, reason, err)
		}
		writeErr(w, http.StatusGone, "session_ended", "this database session has ended", map[string]any{"reason": reason})
		return
	}
	if left := time.Until(s.expiresAt); left < timeout { // never past the session's end, and never 0, which is no timeout at all
		timeout = max(left, time.Millisecond)
	}
	ran := runStatement(ctx, conn, body.Mode, body.SQL, limit, timeout) // the request's mode: a read in a write session is read-only
	_ = conn.Close(context.WithoutCancel(ctx))
	read, err := a.countQuery(context.WithoutCancel(ctx), s.id, int64(len(ran.Rows)), counted)
	if err != nil {
		a.log.Printf("canyon-server: database session %s: its counters: %v", s.id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	switch {
	case ran.parse != nil:
		writeErr(w, http.StatusBadRequest, "invalid_sql", ran.parse.Message, nil)
	case ran.empty:
		writeErr(w, http.StatusBadRequest, "invalid_sql", "the statement is empty", nil)
	case ran.failed != nil && ran.failed.Code == sqlstateCanceled:
		writeErr(w, http.StatusUnprocessableEntity, "statement_timeout", "the statement ran past its timeout of "+timeout.Round(time.Millisecond).String()+" and was cancelled", nil)
	case ran.failed != nil:
		writeErr(w, http.StatusUnprocessableEntity, "query_failed", ran.failed.Message, map[string]any{"sqlstate": ran.failed.Code})
	case ran.err != nil:
		a.log.Printf("canyon-server: db query in database session %s: %v", s.id, ran.err) // never the statement
		writeErr(w, http.StatusBadGateway, "database_unreachable", "Canyon couldn't reach the database of "+id+" in "+s.env+": try again.", nil)
	default:
		answer := map[string]any{"db_session": read.read(ctx), "columns": ran.Columns, "rows": ran.Rows, "row_count": len(ran.Rows),
			"truncated": ran.Truncated, "command": ran.Command, "duration_ms": ran.DurationMS}
		if s.mode == "write" {
			answer["rows_affected"] = ran.RowsAffected
		}
		writeJSON(w, http.StatusOK, answer)
	}
}

// dbColumn is one column of a query's answer: its name, and its type as Postgres's format_type names it.
type dbColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ranStatement is what one statement answered: its rows as Postgres's text, NULL as nil, or why it didn't run.
// parse is the database's refusal at Parse (400 invalid_sql); empty a statement of nothing, only whitespace or a
// comment, which Postgres Executes to no command (400 invalid_sql); failed one at Execute (422), or a row over the
// answer's cap; err any other failure.
type ranStatement struct {
	Columns      []dbColumn
	Rows         [][]*string
	Truncated    bool
	Command      string
	RowsAffected int64
	DurationMS   int64
	parse        *pgconn.PgError
	empty        bool
	failed       *pgconn.PgError
	err          error
}

// runStatement runs sql once on conn, over the extended protocol alone, so the database refuses more than one
// statement at Parse: BEGIN READ ONLY in read mode (BEGIN in write), statement_timeout set for this transaction
// alone, Parse, then Execute, the rows read as text up to limit and dbQueryMaxBytes at a row boundary, the
// columns named by format_type, and COMMIT. A refusal or a failure rolls the transaction back.
func runStatement(ctx context.Context, conn *pgx.Conn, mode, sql string, limit int, timeout time.Duration) (ran ranStatement) {
	pg := conn.PgConn()
	pg.Frontend().SetMaxBodyLen(dbQueryMaxMessage)
	begin := "BEGIN READ ONLY"
	if mode == "write" {
		begin = "BEGIN"
	}
	if err := pg.Exec(ctx, begin).Close(); err != nil {
		ran.err = err
		return ran
	}
	finish := func() { // a statement that did not COMMIT ends here, its transaction rolled back
		if ran.parse != nil || ran.empty || ran.failed != nil || ran.err != nil {
			_ = pg.Exec(context.WithoutCancel(ctx), "ROLLBACK").Close()
		}
	}
	defer finish()
	if _, err := conn.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
		ran.err = err
		return ran
	}
	sd, err := pg.Prepare(ctx, "", sql, nil)
	if err != nil {
		if !errors.As(err, &ran.parse) {
			ran.err = err
		}
		return ran
	}
	began := time.Now()
	rr := pg.ExecPrepared(ctx, "", nil, nil, nil)
	size := 0
	for rr.NextRow() {
		if ran.Truncated {
			continue // the statement runs to its end; its rows past the cut are read and dropped
		}
		values, bytes := rr.Values(), 0
		for _, v := range values { // measured before anything is copied
			bytes += len(v)
		}
		if len(ran.Rows) >= limit || size+bytes > dbQueryMaxBytes {
			ran.Truncated = true
			continue
		}
		row := make([]*string, len(values))
		for i, v := range values {
			if v != nil {
				text := string(v)
				row[i] = &text
			}
		}
		ran.Rows, size = append(ran.Rows, row), size+bytes
	}
	tag, err := rr.Close()
	ran.DurationMS = time.Since(began).Milliseconds()
	var tooBig *pgproto3.ExceededMaxBodyLenErr
	switch {
	case errors.As(err, &tooBig): // the connection is closed with it: nothing more is read, and nothing commits
		ran.failed = &pgconn.PgError{Code: sqlstateTooBig,
			Message: "a row of the answer is over 10 MiB, Canyon's cap on an answer: select fewer or shorter columns, or less of a long one"}
		return ran
	case err != nil:
		if !errors.As(err, &ran.failed) {
			ran.err = err
		}
		return ran
	case tag.String() == "": // Postgres's EmptyQueryResponse: no command ran
		ran.empty = true
		return ran
	}
	ran.Command, ran.RowsAffected = commandOf(tag), tag.RowsAffected()
	if ran.Columns, err = columnsOf(ctx, conn, sd.Fields); err != nil {
		ran.err = err
		return ran
	}
	if ran.Rows == nil {
		ran.Rows = [][]*string{}
	}
	if err := pg.Exec(ctx, "COMMIT").Close(); err != nil {
		if !errors.As(err, &ran.failed) {
			ran.err = err
		}
	}
	return ran
}

// columnsOf names each field by Postgres's format_type, as the contract's column type is: bigint, character
// varying(80); [] for a statement that returns none.
func columnsOf(ctx context.Context, conn *pgx.Conn, fields []pgconn.FieldDescription) ([]dbColumn, error) {
	columns := make([]dbColumn, len(fields))
	for i, f := range fields {
		mod := f.TypeModifier
		columns[i].Name = f.Name
		if err := conn.QueryRow(ctx, "SELECT format_type($1, nullif($2, -1))", f.DataTypeOID, mod).Scan(&columns[i].Type); err != nil {
			return nil, err
		}
	}
	return columns, nil
}

// commandOf is a command tag's verb: SELECT of "SELECT 3", INSERT of "INSERT 0 1", CREATE TABLE of itself.
func commandOf(tag pgconn.CommandTag) string {
	words := strings.Fields(tag.String())
	for len(words) > 0 {
		if _, err := strconv.ParseInt(words[len(words)-1], 10, 64); err != nil {
			break
		}
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

// countQuery adds one query to the open database session id's counters, its connection, the rows it returned and
// the bytes its connection carried each way, and moves its last activity to now; it answers the row as it is now.
// A failed statement is counted too.
func (a *app) countQuery(ctx context.Context, id string, rows int64, c *countingConn) (dbRow, error) {
	var s dbRow
	err := a.pool.QueryRow(ctx, `UPDATE db_sessions SET connections = connections + 1, queries = queries + 1, rows_returned = rows_returned + $2,
		bytes_from_db = bytes_from_db + $3, bytes_to_db = bytes_to_db + $4, last_activity_at = now() WHERE id = $1 RETURNING `+dbColumns,
		id, rows, c.from.Load(), c.to.Load()).Scan(s.dest()...)
	return s, err
}
