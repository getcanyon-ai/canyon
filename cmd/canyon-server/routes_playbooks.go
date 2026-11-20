package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate"
)

// routesPlaybooks registers the org's playbooks (contracts/routes.md, the playbooks row; design/specs/mcp.md §3):
// how the org builds, which agents read and Canyon never checks. Every member reads one that stands; an admin, a
// person, from a browser writes, removes and restores them, and reads every version.
func routesPlaybooks(api *http.ServeMux, a *app) {
	write := func(next http.HandlerFunc) http.HandlerFunc {
		return a.adminOnly(personOnly("only a person can change a playbook", a.playbookInDashboard(next)))
	}
	api.HandleFunc("GET /playbooks", a.getPlaybooks)
	api.HandleFunc("GET /playbooks/{slug}", a.getPlaybook)
	api.HandleFunc("GET /playbooks/{slug}/versions", a.adminOnly(a.getPlaybookVersions))
	api.HandleFunc("PUT /playbooks/{slug}", write(a.putPlaybook))
	api.HandleFunc("DELETE /playbooks/{slug}", write(a.deletePlaybook))
	api.HandleFunc("POST /playbooks/{slug}/restore", write(a.restorePlaybook))
}

// maxPlaybooks is how many playbooks that stand an org keeps (contracts/routes.md, the playbooks row).
const maxPlaybooks = 50

// maxPlaybookBody is a playbook body's bound, in bytes of UTF-8.
const maxPlaybookBody = 64 << 10

var playbookSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// The playbook refusals, in D5's words (design/EXPERIENCE.md).
const (
	playbookTooMany = "Canyon keeps 50 playbooks. Remove one to add another."
	playbookSecret  = "A playbook may carry no secret: agents read it, and a model reads what they read." //nolint:gosec // a refusal's words, no credential
)

// playbookText is what one version of a playbook says.
type playbookText struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	When string `json:"when"`
	Body string `json:"body"`
}

// playbook is one playbook at its newest version, as GET …/:slug answers it.
type playbook struct {
	Slug string `json:"slug"`
	playbookText
	Version   int    `json:"version"`
	UpdatedBy actor  `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// playbookEntry is one playbook as the list answers it: never its body.
type playbookEntry struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	When      string `json:"when"`
	Version   int    `json:"version"`
	UpdatedBy actor  `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
	Reads     int    `json:"reads"`              // fetches in the last 30 days (playbook_counts.go)
	Machines  *int   `json:"machines,omitempty"` // a skill's holders of its newest version, in those 30 days
}

// playbookVersion is one kept version, as an admin reads them.
type playbookVersion struct {
	Version int `json:"version"`
	playbookText
	SetBy actor  `json:"set_by"`
	SetAt string `json:"set_at"`
}

func (p playbook) entry() playbookEntry {
	return playbookEntry{Slug: p.Slug, Name: p.Name, Kind: p.Kind, When: p.When, Version: p.Version, UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt}
}

// playbookColumns are a version's columns, in scanPlaybook's order.
const playbookColumns = `v.slug, v.version, v.name, v.kind, v.when_line, v.body, v.set_by_kind, v.set_by_subject, v.set_by_display, v.set_at`

func scanPlaybook(row pgx.CollectableRow) (playbook, error) {
	var p playbook
	var at time.Time
	err := row.Scan(&p.Slug, &p.Version, &p.Name, &p.Kind, &p.When, &p.Body, &p.UpdatedBy.Kind, &p.UpdatedBy.Subject, &p.UpdatedBy.Display, &at)
	p.UpdatedAt = at.UTC().Format(time.RFC3339)
	return p, err
}

// playbooksOf is every playbook that stands, at its newest version, by slug bytewise.
func playbooksOf(ctx context.Context, q querier) ([]playbook, error) {
	rows, err := q.Query(ctx, `WITH v AS (SELECT DISTINCT ON (slug) * FROM playbook_versions ORDER BY slug, version DESC)
		SELECT `+playbookColumns+` FROM v WHERE NOT EXISTS (SELECT 1 FROM playbook_removals r WHERE r.slug = v.slug AND r.version = v.version)
		ORDER BY v.slug COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPlaybook)
}

// newestPlaybook is a slug's newest version, whether it is removed, and whether it has any version at all.
func newestPlaybook(ctx context.Context, q querier, slug string) (playbook, bool, bool, error) {
	rows, err := q.Query(ctx, `SELECT `+playbookColumns+`, EXISTS (SELECT 1 FROM playbook_removals r WHERE r.slug = v.slug AND r.version = v.version)
		FROM playbook_versions v WHERE v.slug = $1 ORDER BY v.version DESC LIMIT 1`, slug)
	if err != nil {
		return playbook{}, false, false, err
	}
	var removed bool
	p, err := pgx.CollectOneRow(rows, func(row pgx.CollectableRow) (playbook, error) {
		var p playbook
		var at time.Time
		err := row.Scan(&p.Slug, &p.Version, &p.Name, &p.Kind, &p.When, &p.Body, &p.UpdatedBy.Kind, &p.UpdatedBy.Subject, &p.UpdatedBy.Display, &at, &removed)
		p.UpdatedAt = at.UTC().Format(time.RFC3339)
		return p, err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return playbook{}, false, false, nil
	}
	return p, removed, err == nil, err
}

func (a *app) getPlaybooks(w http.ResponseWriter, r *http.Request) {
	all, err := playbooksOf(r.Context(), a.pool)
	if err != nil {
		a.internal(w, "playbooks", err)
		return
	}
	reads, machines, err := playbookCounts(r.Context(), a.pool)
	if err != nil {
		a.internal(w, "playbook counts", err)
		return
	}
	out := make([]playbookEntry, 0, len(all))
	for _, p := range all {
		e := p.entry()
		e.Reads = reads[p.Slug]
		if p.Kind == "skill" {
			n := machines[p.Slug]
			e.Machines = &n
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"playbooks": out})
}

func (a *app) getPlaybook(w http.ResponseWriter, r *http.Request) {
	way := r.URL.Query().Get("read")
	if !readWays[way] {
		writeErr(w, http.StatusBadRequest, "bad_request", "read is tool, resource, cli or sync", nil)
		return
	}
	p, removed, found, err := newestPlaybook(r.Context(), a.pool, r.PathValue("slug"))
	switch {
	case err != nil:
		a.internal(w, "playbook", err)
		return
	case !found || removed:
		writeErr(w, http.StatusNotFound, "not_found", "no playbook by that slug", nil)
		return
	}
	switch way {
	case "sync":
		a.recordSyncs(r.Context(), map[string]int{p.Slug: p.Version})
	case "tool", "resource", "cli":
		a.countRead(r.Context(), p.Slug, way)
	}
	writeJSON(w, http.StatusOK, map[string]any{"playbook": p})
}

func (a *app) getPlaybookVersions(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	_, removed, found, err := newestPlaybook(r.Context(), a.pool, slug)
	if err != nil {
		a.internal(w, "playbook versions", err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "no playbook by that slug", nil)
		return
	}
	rows, err := a.pool.Query(r.Context(), `SELECT `+playbookColumns+` FROM playbook_versions v WHERE v.slug = $1 ORDER BY v.version DESC`, slug)
	if err != nil {
		a.internal(w, "playbook versions", err)
		return
	}
	all, err := pgx.CollectRows(rows, scanPlaybook)
	if err != nil {
		a.internal(w, "playbook versions", err)
		return
	}
	versions := make([]playbookVersion, 0, len(all))
	for _, p := range all {
		versions = append(versions, playbookVersion{Version: p.Version, playbookText: p.playbookText, SetBy: p.UpdatedBy, SetAt: p.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "removed": removed, "versions": versions})
}

// playbookProblem is the first thing the routes row refuses in a playbook, in D5's words, or "".
func playbookProblem(slug string, t playbookText) (string, error) {
	line := func(s string) bool { return !strings.ContainsFunc(s, unicode.IsControl) }
	body := func(s string) bool {
		return !strings.ContainsFunc(s, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' })
	}
	switch n, m := utf8.RuneCountInString(t.Name), utf8.RuneCountInString(t.When); {
	case !playbookSlug.MatchString(slug):
		return "A playbook's slug is 1 to 40 of a-z, 0-9 and -, beginning with a letter.", nil
	case n < 1 || n > 80 || !line(t.Name): // JSON decoding already made invalid UTF-8 U+FFFD
		return "A playbook's name is 1 to 80 characters on one line.", nil
	case t.Kind != "skill" && t.Kind != "note":
		return "A playbook is a skill or a note.", nil
	case m < 1 || m > 200 || !line(t.When):
		return "When to read it is one line of 1 to 200 characters.", nil
	case t.Body == "" || len(t.Body) > maxPlaybookBody || !body(t.Body):
		return "A playbook's body is 1 to 64 KiB of Markdown, with no control character but a newline or a tab.", nil
	}
	for _, s := range []string{t.Name, t.When, t.Body} {
		secret, err := gate.HasSecret(s)
		if err != nil || secret {
			return playbookSecret, err
		}
	}
	return "", nil
}

// playbookInDashboard refuses a playbook's change from any session but a browser's on a host with sign-in
// (contracts/routes.md, the playbooks row): an admin's agent replaying the CLI's session never rewrites what every
// agent reads.
func (a *app) playbookInDashboard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth == authOIDC && !fromDashboard(r.Context()) { // with sign-in off no session tells the browser from the CLI
			writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
			deny(r.Context(), "dashboard_required", "")
			return
		}
		next(w, r)
	}
}

var (
	errTooManyPlaybooks = errors.New("an org keeps 50 playbooks")
	errNoPlaybook       = errors.New("no such playbook or version")
)

func (a *app) putPlaybook(w http.ResponseWriter, r *http.Request) {
	var body playbookText
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is one JSON object of name, kind, when and body", nil)
		return
	}
	slug := r.PathValue("slug")
	problem, err := playbookProblem(slug, body)
	if err != nil {
		a.internal(w, "playbook", err)
		return
	}
	if problem != "" {
		writeErr(w, http.StatusBadRequest, "invalid_playbook", problem, nil)
		return
	}
	a.answerPlaybook(w, a.savePlaybook(r.Context(), actorOf(r.Context()), slug, func(context.Context, pgx.Tx) (playbookText, int, error) { return body, 0, nil }))
}

func (a *app) restorePlaybook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version *int `json:"version"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || body.Version == nil || *body.Version < 1 { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is one JSON object of version, a version of this playbook", nil)
		return
	}
	slug, from := r.PathValue("slug"), *body.Version
	a.answerPlaybook(w, a.savePlaybook(r.Context(), actorOf(r.Context()), slug, func(ctx context.Context, tx pgx.Tx) (playbookText, int, error) {
		var t playbookText
		err := tx.QueryRow(ctx, "SELECT name, kind, when_line, body FROM playbook_versions WHERE slug = $1 AND version = $2", slug, from).Scan(&t.Name, &t.Kind, &t.When, &t.Body)
		if errors.Is(err, pgx.ErrNoRows) {
			return t, 0, errNoPlaybook
		}
		if err != nil {
			return t, 0, err
		}
		problem, err := playbookProblem(slug, t) // a version kept before a scanner learned a secret's shape is held to it now
		if err == nil && problem != "" {
			err = errPlaybookRefused{problem}
		}
		return t, from, err
	}))
}

// errPlaybookRefused is a version a restore would save that the routes row refuses.
type errPlaybookRefused struct{ message string }

func (e errPlaybookRefused) Error() string { return e.message }

// playbookSave is a save's answer: the playbook it leaves standing, or why there is none.
type playbookSave struct {
	playbook playbook
	err      error
}

func (a *app) answerPlaybook(w http.ResponseWriter, s playbookSave) {
	var refused errPlaybookRefused
	switch {
	case errors.Is(s.err, errNoPlaybook):
		writeErr(w, http.StatusNotFound, "not_found", "no such version of that playbook", nil)
	case errors.As(s.err, &refused):
		writeErr(w, http.StatusBadRequest, "invalid_playbook", refused.message, nil)
	case errors.Is(s.err, errTooManyPlaybooks):
		writeErr(w, http.StatusConflict, "too_many_playbooks", playbookTooMany, nil)
	case s.err != nil:
		a.internal(w, "playbook", s.err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"playbook": s.playbook})
	}
}

// savePlaybook saves the text text reads in tx as the slug's next version, under the playbooks' lock so the bound
// of 50 holds: from is the version a restore saves again, 0 for a PUT. A playbook that stands and already says that
// writes nothing; else one audit row, set playbook with the fields that changed, or restore playbook with from.
func (a *app) savePlaybook(ctx context.Context, act actor, slug string, text func(context.Context, pgx.Tx) (playbookText, int, error)) playbookSave {
	tx, err := a.mutation(ctx)
	if err != nil {
		return playbookSave{err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('playbooks'))"); err != nil {
		return playbookSave{err: err}
	}
	t, from, err := text(ctx, tx)
	if err != nil {
		return playbookSave{err: err}
	}
	current, removed, found, err := newestPlaybook(ctx, tx, slug)
	if err != nil {
		return playbookSave{err: err}
	}
	if found && !removed && current.playbookText == t {
		return playbookSave{playbook: current} // nothing changed: no version, no row
	}
	if !found || removed {
		var standing int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT ON (slug) slug, version FROM playbook_versions ORDER BY slug, version DESC) v
			WHERE NOT EXISTS (SELECT 1 FROM playbook_removals r WHERE r.slug = v.slug AND r.version = v.version)`).Scan(&standing); err != nil {
			return playbookSave{err: err}
		}
		if standing >= maxPlaybooks {
			return playbookSave{err: errTooManyPlaybooks}
		}
	}
	version := current.Version + 1 // 1 for the first: no version reads as 0
	if _, err := tx.Exec(ctx, `INSERT INTO playbook_versions (slug, version, name, kind, when_line, body, set_by_kind, set_by_subject, set_by_display)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, slug, version, t.Name, t.Kind, t.When, t.Body, act.Kind, act.Subject, act.Display); err != nil {
		return playbookSave{err: err}
	}
	action, detail := "set playbook", map[string]any{"slug": slug, "version": version, "diff": playbookDiff(current, found, t, version)}
	if from > 0 {
		action, detail = "restore playbook", map[string]any{"slug": slug, "version": version, "from": from}
	}
	if err := events.AuditTx(ctx, tx, act, action, "", "", detail); err != nil {
		return playbookSave{err: err}
	}
	saved, _, _, err := newestPlaybook(ctx, tx, slug) // read back, never assembled in memory
	if err != nil {
		return playbookSave{err: err}
	}
	return playbookSave{playbook: saved, err: tx.Commit(ctx)}
}

// playbookDiff is a save's diff (contracts/routes.md, the playbooks row): each field that changed, name, kind and
// when as their before and after, before absent at version 1, and the body as a unified diff from the version
// before, or from an empty body at version 1.
func playbookDiff(before playbook, found bool, after playbookText, version int) map[string]any {
	out := map[string]any{}
	for _, f := range []struct{ key, was, now string }{{"name", before.Name, after.Name}, {"kind", before.Kind, after.Kind}, {"when", before.When, after.When}} {
		switch {
		case !found:
			out[f.key] = map[string]string{"after": f.now}
		case f.was != f.now:
			out[f.key] = map[string]string{"before": f.was, "after": f.now}
		}
	}
	if !found || before.Body != after.Body {
		diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: bodyLines(before.Body), B: bodyLines(after.Body),
			FromFile: "version " + strconv.Itoa(version-1), ToFile: "version " + strconv.Itoa(version), Context: 3}) // writing to a buffer never fails
		out["body"] = diff
	}
	return out
}

// bodyLines is a body's lines, each with its newline: difflib's own SplitLines adds a line the body doesn't have.
func bodyLines(body string) []string {
	lines := strings.SplitAfter(body, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (a *app) deletePlaybook(w http.ResponseWriter, r *http.Request) {
	ctx, slug := r.Context(), r.PathValue("slug")
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('playbooks'))"); err != nil {
			return err
		}
		current, removed, found, err := newestPlaybook(ctx, tx, slug)
		if err != nil {
			return err
		}
		if !found || removed {
			return errNoPlaybook
		}
		act := actorOf(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO playbook_removals (slug, version, set_by_kind, set_by_subject, set_by_display) VALUES ($1, $2, $3, $4, $5)`,
			slug, current.Version, act.Kind, act.Subject, act.Display); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, act, "remove playbook", "", "", map[string]any{"slug": slug, "version": current.Version})
	})
	switch {
	case errors.Is(err, errNoPlaybook):
		writeErr(w, http.StatusNotFound, "not_found", "no playbook by that slug", nil)
	case err != nil:
		a.internal(w, "playbook", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
