package main

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// pageCursor is the sort key of the last row on a page of a list ordered by (timestamp, id) —
// the packages, the gate runs, the audit trail: one way to page (contracts/routes.md). The
// timestamp keeps its full precision: cut to the wire's whole second, the rows inside the
// boundary second would vanish from a walk.
type pageCursor struct {
	ts time.Time
	id string
}

func (c pageCursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.ts.UTC().Format(time.RFC3339Nano) + "|" + c.id))
}

// parsePageCursor reads back what String wrote; an absent cursor is the start, anything else is false.
func parsePageCursor(s string) (c pageCursor, ok bool) {
	if s == "" {
		return c, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	ts, id, found := strings.Cut(string(raw), "|")
	if err != nil || !found || id == "" || !utf8.ValidString(id) || strings.ContainsRune(id, 0) { // Postgres text refuses both: a 500, not a 400
		return c, false
	}
	c.ts, err = time.Parse(time.RFC3339Nano, ts)
	return pageCursor{c.ts, id}, err == nil
}

// pageLimit reads a list's limit off the query (50 unless given, clamped at 200), answering the
// contract's invalid_limit itself; the fleet read and the three lists page by the same rule.
func pageLimit(w http.ResponseWriter, q url.Values) (int, bool) {
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "invalid_limit", "limit is a positive integer", nil)
			return 0, false
		}
		limit = min(n, 200)
	}
	return limit, true
}

// pageNumber reads a page by its number off the query (contracts/routes.md, the fleet read: page): 0 when absent,
// answering invalid_page itself for a non-integer, a value below 1, or a page with a cursor — a read pages one way.
func pageNumber(w http.ResponseWriter, q url.Values) (int, bool) {
	s := q.Get("page")
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if errors.Is(err, strconv.ErrRange) && n > 0 { // past any page there is: the last
		err = nil
	}
	if err != nil || n < 1 || q.Get("cursor") != "" {
		writeErr(w, http.StatusBadRequest, "invalid_page", "page is a positive integer, never with a cursor", nil)
		return 0, false
	}
	return n, true
}

// servedPage is the page a numbered read answers: the one asked for (the first when none was), past the last clamped to it;
// with no rows there is one page, the empty one.
func servedPage(page, total, limit int) int {
	return min(max(page, 1), max(1, (total+limit-1)/limit))
}

// pageQuery reads the limit and the cursor, answering both 400s itself; false means the response is written.
func pageQuery(w http.ResponseWriter, q url.Values) (after pageCursor, limit int, ok bool) {
	if limit, ok = pageLimit(w, q); !ok {
		return after, 0, false
	}
	if after, ok = parsePageCursor(q.Get("cursor")); !ok {
		writeErr(w, http.StatusBadRequest, "invalid_cursor", "cursor is what the previous page's next carried", nil)
		return after, 0, false
	}
	return after, limit, true
}

// trim is the page's end: the query fetched one row past the limit, and if it arrived it is
// dropped and the key of the row before it becomes next; short of the limit, next is null.
func trim[T any](rows []T, keys []pageCursor, limit int) ([]T, *string) {
	if len(rows) <= limit {
		return rows, nil
	}
	next := keys[limit-1].String()
	return rows[:limit], &next
}
