package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/runs"
)

func routesImageReports(api *http.ServeMux, a *app) {
	api.HandleFunc("PUT /projects/{id}/images/{digest}/reports", a.teamOnly(a.putImageReport))
}

var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// putImageReport attaches one imported tool's report of an image of the project (design/PATCHING.md §3.4; contracts/
// routes.md, the image reports row): the package reports row's body, limits, tool registration, rate limit and audit,
// for an image the project runs, kept or Canyon scanned. Only names, versions and ids are stored.
func (a *app) putImageReport(w http.ResponseWriter, r *http.Request) {
	ctx, project, digest, by := r.Context(), r.PathValue("id"), r.PathValue("digest"), actorOf(r.Context())
	limit := a.boot.Limits.GateRunsPerActorPerHour
	var err error
	if wait, ok := a.puts.take(by.Kind + "/" + by.Subject); !ok {
		err = &runs.RateLimited{PerActor: true, Limit: limit, RetryAfter: wait}
	} else {
		err = runs.ActorLimited(ctx, a.pool, limit, by)
	}
	if err == nil && slices.ContainsFunc(r.Header.Values("Content-Encoding"), func(v string) bool {
		v = strings.TrimSpace(v)
		return v != "" && !strings.EqualFold(v, "identity")
	}) {
		err = sarif.Invalid{Message: "the report is compressed"}
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxReport))
	}
	var answer reportAnswer
	if err == nil {
		answer, err = a.attachImageReport(ctx, project, digest, raw, by)
	}
	var (
		over    *http.MaxBytesError
		refusal *refused
		limited *runs.RateLimited
		invalid sarif.Invalid
		unknown sarif.UnknownTool
		tooMany sarif.TooManyResults
	)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, answer)
	case errors.As(err, &over):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "the report is over 10 MiB", nil)
	case errors.As(err, &refusal):
		writeErr(w, refusal.status, refusal.code, refusal.message, nil)
	case errors.As(err, &limited):
		writeErr(w, http.StatusTooManyRequests, "rate_limited", limitMessage(limited, project), map[string]any{"retry_after_s": int(math.Ceil(limited.RetryAfter.Seconds()))})
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, "invalid_report", invalid.Message, nil)
	case errors.As(err, &unknown):
		writeErr(w, http.StatusBadRequest, "unknown_tool", unknown.Error(), nil)
	case errors.As(err, &tooMany):
		writeErr(w, http.StatusBadRequest, "too_many_results", tooMany.Error(), nil)
	default:
		a.log.Printf("canyon-server: attach an image report to %s: %v", project, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

// attachImageReport normalises raw against the composed snapshot, then, under the ledger lock, stores it as the
// image's report for its (tool, category) and audits it with the digest it replaced. A refusal stores nothing.
func (a *app) attachImageReport(ctx context.Context, project, digest string, raw []byte, by actor) (reportAnswer, error) {
	if err := imageOfProject(ctx, a.pool, project, digest); err != nil {
		return reportAnswer{}, err
	}
	snap, err := a.effectivePolicy(ctx, a.pool)
	if err != nil {
		return reportAnswer{}, err
	}
	rep, err := sarif.NormalizeImage(raw, &snap)
	if err != nil {
		return reportAnswer{}, err
	}
	sum := sha256.Sum256(raw)
	reportDigest := "sha256:" + hex.EncodeToString(sum[:])
	tx, err := a.mutation(ctx)
	if err != nil {
		return reportAnswer{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := imageOfProject(ctx, tx, project, digest); err != nil { // deleted, or being deleted, while the report was read
		return reportAnswer{}, err
	}
	var replaced *string
	err = tx.QueryRow(ctx, "SELECT report_digest FROM image_reports WHERE project_id = $1 AND digest = $2 AND tool = $3 AND category = $4 FOR UPDATE",
		project, digest, rep.Tool, rep.Category).Scan(&replaced)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reportAnswer{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO image_reports (project_id, digest, tool, category, report_digest, version, attached_by_kind, attached_by_subject, attached_by_display, results, dropped, first_dropped)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''))
		ON CONFLICT (project_id, digest, tool, category) DO UPDATE SET report_digest = EXCLUDED.report_digest, version = EXCLUDED.version, attached_by_kind = EXCLUDED.attached_by_kind,
			attached_by_subject = EXCLUDED.attached_by_subject, attached_by_display = EXCLUDED.attached_by_display, attached_at = now(), results = EXCLUDED.results,
			dropped = EXCLUDED.dropped, first_dropped = EXCLUDED.first_dropped`,
		project, digest, rep.Tool, rep.Category, reportDigest, rep.Version, by.Kind, by.Subject, by.Display, rep.Results, rep.Dropped, rep.FirstDropped); err != nil {
		return reportAnswer{}, err
	}
	if err := events.AuditTx(ctx, tx, by, "attach image report", project, "", map[string]any{"image": digest, "tool": rep.Tool, "category": rep.Category, "digest": reportDigest, "replaced": replaced}); err != nil {
		return reportAnswer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return reportAnswer{}, err
	}
	return reportAnswer{Tool: rep.Tool, Category: rep.Category, Results: len(rep.Results), Dropped: rep.Dropped, FirstDropped: rep.FirstDropped}, nil
}

// imageOfProject refuses an image the live project neither runs, keeps nor had Canyon scan: 404, the project's 409
// while it is deleted.
func imageOfProject(ctx context.Context, q rower, project, digest string) error {
	var deleting bool
	err := q.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1 AND deleted_at IS NULL", project).Scan(&deleting)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &refused{http.StatusNotFound, "not_found", notOnCanyon(project)}
	case err != nil:
		return err
	case deleting:
		return &refused{http.StatusConflict, "deleting", "this project is being deleted"}
	}
	var known bool
	if imageDigest.MatchString(digest) {
		err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND split_part(image, '@', 2) = $2)
			OR EXISTS (SELECT 1 FROM deploy_history WHERE project_id = $1 AND split_part(image, '@', 2) = $2)
			OR EXISTS (SELECT 1 FROM scanned_images WHERE project_id = $1 AND digest = $2)`, project, digest).Scan(&known)
	}
	if err == nil && !known {
		return &refused{http.StatusNotFound, "not_found", "This app doesn't run or keep that image."}
	}
	return err
}
