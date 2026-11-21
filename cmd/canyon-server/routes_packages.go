package main

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// packageOut is the package on the wire (contracts/routes.md): the ingest result plus its source.
type packageOut struct {
	ID       string  `json:"id"`
	Label    *string `json:"label,omitempty"`
	TreeHash string  `json:"tree_hash"`
	Commit   string  `json:"commit"`
	Source   struct {
		Kind  string `json:"kind"`
		Ref   string `json:"ref"`
		Actor string `json:"actor"`
	} `json:"source"`
}

// packageRow is one row of the packages list (contracts/routes.md): the upload's package plus when it landed.
type packageRow struct {
	packageOut
	CreatedAt string `json:"created_at"`
}

func routesPackages(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects/{id}/packages", a.teamOnly(a.uploadPackage))
	api.HandleFunc("POST /projects/{id}/packages/from-repository", a.teamOnly(a.packageFromRepository))
	api.HandleFunc("GET /projects/{id}/packages/current", a.teamOnly(a.pullPackage))
	api.HandleFunc("GET /projects/{id}/packages", a.teamOnly(a.listPackages))
	api.HandleFunc("GET /projects/{id}/packages/{pkg}", a.teamOnly(a.packageByID))
}

// packageColumns is a package's row (contracts/routes.md). Its actor is the uploader by display, as
// headPackage names the head's: the newest upload or push of the tree on the trail, else the stored
// source — never an identity provider's subject a person reads.
const packageColumns = `SELECT id, label, tree_hash, commit, source_kind, source_ref,
  COALESCE((SELECT COALESCE(NULLIF(e.actor_display, ''), e.actor_subject) FROM audit_events e
    WHERE e.project_id = packages.project_id AND e.detail->>'package' = packages.id AND e.action IN ('POST /api/v1/projects/{id}/packages', 'package from github', 'POST /api/v1/projects/{id}/packages/from-repository')
    ORDER BY e.ts DESC, e.id DESC LIMIT 1), source_actor), created_at
FROM packages
`

// packagesPageSQL is one page of a project's packages, newest first, from the cursor (an empty
// id is the start), one row past the limit; packages_latest serves it. A pull request's or a check's is never listed.
const packagesPageSQL = packageColumns + `WHERE project_id = $1 AND source_kind NOT IN ('repo-pr', 'check') AND ($3::text = '' OR (created_at, id) < ($2::timestamptz, $3))
ORDER BY created_at DESC, id DESC LIMIT $4`

// scanPackage reads one row of packageColumns.
func scanPackage(row pgx.Row) (packageRow, pageCursor, error) {
	var p packageRow
	var at time.Time
	err := row.Scan(&p.ID, &p.Label, &p.TreeHash, &p.Commit, &p.Source.Kind, &p.Source.Ref, &p.Source.Actor, &at)
	p.CreatedAt = at.UTC().Format(time.RFC3339)
	return p, pageCursor{at, p.ID}, err
}

// packageByID answers one package of the live project as the list's row, whoever made it: an upload,
// a push, a pull request or a check (contracts/routes.md). It is guard (b)'s read (design/OPERATIONS.md
// §2.3): a package id is its tree, so a host this tree was never sent to has no such id, and a
// package this project never had is its own 404,
// unknown_package, apart from the project's not_found. A package's row outlives its tree, so one this
// host made is never answered as unknown. A read, so no audit row.
func (a *app) packageByID(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	p, _, err := scanPackage(a.pool.QueryRow(ctx, packageColumns+"WHERE project_id = $1 AND id = $2", id, r.PathValue("pkg")))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "unknown_package", "no such package of this project", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"package": p})
}

// listPackages answers one page of the live project's packages; the first row of the first
// page is current. A read, so no audit row.
func (a *app) listPackages(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	after, limit, ok := pageQuery(w, r.URL.Query())
	if !ok {
		return
	}
	rows, err := a.pool.Query(ctx, packagesPageSQL, id, after.ts, after.id, limit+1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer rows.Close()
	packages, keys := []packageRow{}, []pageCursor{}
	for rows.Next() {
		p, key, err := scanPackage(rows)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		packages, keys = append(packages, p), append(keys, key)
	}
	if rows.Err() != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	packages, next := trim(packages, keys, limit)
	writeJSON(w, http.StatusOK, map[string]any{"packages": packages, "next": next})
}

// projectLive is the list reads' 404: an unknown or deleted project, answered here; false
// means the response is written.
func (a *app) projectLive(ctx context.Context, w http.ResponseWriter, id string) bool {
	var live bool
	if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", id).Scan(&live); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return false
	}
	if !live {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
	}
	return live
}

// pullPackage streams the project's current package, its newest, as a tar: canyon pull. A
// failure before the first byte (the repository gone, git missing) is answered as one; after
// it the status is out, the failure is logged and the client's tar reader sees the cut.
func (a *app) pullPackage(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !runs.ValidProject.MatchString(id) {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	head, err := headPackage(ctx, a.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no package yet", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Canyon-Package", head.ID)
	w.Header().Set("Content-Type", "application/x-tar")
	out := &countingWriter{w: w} // the first write sends the 200
	if err := ingest.Archive(ctx, filepath.Join(a.repos, id+".git"), head.Commit, out); err != nil {
		a.log.Printf("canyon-server: pull %s/%s: %v", id, head.ID, err)
		if out.n == 0 {
			w.Header().Del("Canyon-Package")
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		}
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// uploadPackage commits the uploaded tar to the project's repository, filtered by the snapshot's
// exclusions, and records the package. A base that is not the project's newest package, or no base
// when the project has one, is refused unless the upload is forced; the same tree again answers the
// package it already is. A check's upload (?check=true) is review-only, as a pull request's: it reads
// no base, moves no main, declares nothing for the project and is never current; a tree the project
// holds is answered as it is.
func (a *app) uploadPackage(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !runs.ValidProject.MatchString(id) { // the id names a directory on disk: only a project id reaches it
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if !a.projectLive(ctx, w, id) {
		return
	}
	check := r.URL.Query().Get("check") == "true"
	var label *string
	if values := r.Header.Values("Canyon-Package-Label"); len(values) > 0 { // present, even empty, is a label to check
		l := values[0]
		if check {
			writeErr(w, http.StatusBadRequest, "invalid_label", "a check's upload takes no label: a check names no version", nil)
			return
		}
		if !validLabel.MatchString(l) {
			writeErr(w, http.StatusBadRequest, "invalid_label", "a package label is lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit, at most 64 characters", nil)
			return
		}
		label = &l
	}
	head, err := headPackage(ctx, a.pool, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	base := r.Header.Get("Canyon-Base-Package")
	// No base on a project with a current package is stale too (#1503): a fresh clone records none,
	// and its upload would silently replace a teammate's work. With no current package nothing is
	// replaced, whatever base the copy records (#1678): a reinstalled host takes it as a first upload.
	stale := !check && head.ID != "" && base != head.ID
	if stale && r.URL.Query().Get("force") != "true" {
		var moved map[string]any
		if head.ID != "" {
			moved = map[string]any{"head_package": head.ID, "moved_by": head.Actor, "moved_at": head.At.UTC(), "moved_from": map[string]string{"kind": head.Kind, "ref": head.Ref}}
		}
		writeErr(w, http.StatusConflict, "stale_base", "the project's current package is not the one this upload was based on: pull, or force", moved)
		return
	}
	menu, err := componentsOf(a.snap.ComponentMenu)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	menu, err = a.provisioned(ctx, menu) // a type this host does not provision yet is refused as an unknown one is (#1255)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	var declared, roles map[string]string
	var description *string
	pkg, err := ingest.IngestChecked(ctx, filepath.Join(a.repos, id+".git"), r.Body, a.snap.Exclusions, false, !check, func(tree string) (err error) {
		declared, roles, description, err = declarations(tree, menu) // before the commit: a refused declaration leaves no trace in the repository
		return err
	})
	var refused *declarationError
	if errors.As(err, &refused) {
		writeErr(w, http.StatusBadRequest, refused.code, refused.Error(), nil)
		return
	}
	if errors.Is(err, ingest.ErrRequest) {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	if err != nil {
		a.log.Printf("canyon-server: ingest into %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out := packageOut{ID: pkg.ID, TreeHash: pkg.TreeHash, Commit: pkg.Commit}
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", id).Scan(&live); err != nil { // again, under the append lock: a delete that committed meanwhile is visible
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !live {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if label != nil { // a label names one package of the project and never moves
		var holder string
		err := tx.QueryRow(ctx, "SELECT id FROM packages WHERE project_id = $1 AND label = $2", id, *label).Scan(&holder)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if err == nil && holder != pkg.ID {
			writeErr(w, http.StatusConflict, "label_taken", "that label names another package of this project", map[string]any{"package": holder})
			return
		}
	}
	kind, firsts := "check", []string(nil)
	if !check {
		kind = "upload"
		if declared == nil { // a tree only a pull request or a check packaged: its declarations become the project's with this upload
			if err := tx.QueryRow(ctx, "SELECT resources FROM packages WHERE id = $1 AND project_id = $2 AND source_kind IN ('repo-pr', 'check')", pkg.ID, id).Scan(&declared); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
				return
			}
		}
		if firsts, err = firstDeclarations(ctx, tx, id, declared); err != nil { // read before the insert: the same tree again declares nothing new
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	// created_at is never before the project's newest package, whatever the database's clock did since: the newest row is current.
	// The same tree again keeps its first label and source kind and is current again, its uploader its last (contracts/routes.md, canyonv2-o49o), so the base
	// a client records from this answer is the head; a tree only a pull request or a check packaged becomes this upload's, and current.
	// A check's upload of a tree the project holds changes nothing of it.
	if _, err := tx.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_ref, source_actor, label, resources, roles, description, created_at) VALUES ($1, $2, $3, $4, $5, $11, '', $6, $7, $8, $9, $10, GREATEST(now(), (SELECT max(created_at) + interval '1 microsecond' FROM packages WHERE project_id = $2)))
		ON CONFLICT (id, project_id) DO UPDATE SET created_at = EXCLUDED.created_at,
		  source_kind = CASE WHEN packages.source_kind IN ('repo-pr', 'check') THEN 'upload' ELSE packages.source_kind END,
		  source_ref = CASE WHEN packages.source_kind IN ('repo-pr', 'check') THEN '' ELSE packages.source_ref END,
		  source_actor = EXCLUDED.source_actor,
		  label = CASE WHEN packages.source_kind IN ('repo-pr', 'check') THEN EXCLUDED.label ELSE packages.label END
		WHERE EXCLUDED.source_kind = 'upload'`,
		pkg.ID, id, pkg.TreeHash, pkg.Commit, pkg.Bytes, actorOf(ctx).Subject, label, declared, roles, description, kind); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	// read back: the same tree again is the existing package, with its first source and its first label
	if err := tx.QueryRow(ctx, "SELECT source_kind, source_ref, source_actor, label FROM packages WHERE id = $1 AND project_id = $2", pkg.ID, id).Scan(&out.Source.Kind, &out.Source.Ref, &out.Source.Actor, &out.Label); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if label != nil && (out.Label == nil || *out.Label != *label) { // an existing tree keeps its first label: a new one does not attach
		writeErr(w, http.StatusConflict, "label_taken", "labels are set when a package is made; this package exists already", map[string]any{"package": pkg.ID})
		return
	}
	detail := map[string]any{"package": pkg.ID}
	if out.Label != nil { // the trail names the version, not just the id (contracts/routes.md, *Naming a version*)
		detail["label"] = *out.Label
	}
	if stale { // forced over a stale base: the trail says so (contracts/routes.md row 76); a force that overrode nothing is a plain upload
		detail["forced"] = true
	}
	if check { // never the mover of a head (headPackage)
		detail["kind"] = "check"
	}
	if err := events.AuditTx(ctx, tx, actorOf(ctx), action(r), id, "", detail); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	for _, name := range firsts { // one row the first time the project declares a resource, not on every package that repeats it
		if err := events.AuditTx(ctx, tx, actorOf(ctx), "declare resource", id, "", map[string]any{"name": name, "type": declared[name], "package": pkg.ID}); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	if !check && out.Source.Kind == "upload" { // kept in GitHub after the upload answers (#1926): never a check's, nor a tree a push brought
		who := actorOf(ctx)
		if err := githubapp.QueueKeep(ctx, tx, a.durable, githubapp.Keep{Project: id, Package: pkg.ID, Display: cmp.Or(who.Display, who.Subject), Email: addressOf(ctx)}); err != nil {
			a.log.Printf("canyon-server: keep %s in github: %v", id, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"package": out})
}

// headPkg is a project's current package: its newest row, where the repository's main points.
type headPkg struct {
	ID, Commit, Actor string
	At                time.Time
	Kind, Ref         string // its source: a pushed head's ref is the commit GitHub delivered (#1938)
}

// headPackage is the one rule for "current" (contracts/routes.md row 47), shared by the upload's
// stale-base check, the pull, and the gate run's default package: the newest package not a pull
// request's or a check's. pgx.ErrNoRows when the live project has none.
func headPackage(ctx context.Context, q rower, project string) (headPkg, error) {
	var h headPkg
	// Actor is whoever's upload made the head current, by display (canyonv2-e69b): the newest upload or push of its tree on the
	// trail, a person's name rather than an identity provider's subject; the package's first source when the trail names none.
	err := q.QueryRow(ctx, `SELECT p.id, p.commit, COALESCE((SELECT COALESCE(NULLIF(e.actor_display, ''), e.actor_subject) FROM audit_events e
		  WHERE e.project_id = p.project_id AND e.detail->>'package' = p.id AND e.action IN ('POST /api/v1/projects/{id}/packages', 'package from github', 'POST /api/v1/projects/{id}/packages/from-repository')
		    AND COALESCE(e.detail->>'kind', '') NOT IN ('repo-pr', 'check') -- a pull request's or a check's packaging never makes anything current
		  ORDER BY e.ts DESC, e.id DESC LIMIT 1), p.source_actor), p.created_at, p.source_kind, p.source_ref
		FROM packages p JOIN projects pr ON pr.id = p.project_id AND pr.deleted_at IS NULL
		WHERE p.project_id = $1 AND p.source_kind NOT IN ('repo-pr', 'check') ORDER BY p.created_at DESC, p.id DESC LIMIT 1`, project).Scan(&h.ID, &h.Commit, &h.Actor, &h.At, &h.Kind, &h.Ref)
	return h, err
}

// rower is what a single-row read runs on: the pool, or the transaction a mutation is inside.
type rower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// validLabel is a package label's shape (contracts/routes.md, POST /projects/:id/packages).
var validLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
