package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// maxReport is the most of a report the route reads before decoding (contracts/routes.md, the reports row).
const maxReport = 10 << 20

func routesReports(api *http.ServeMux, a *app) {
	api.HandleFunc("PUT /projects/{id}/packages/{pkg}/reports", a.teamOnly(a.putReport))
}

// reportAnswer is an attach's answer (contracts/routes.md, the reports row).
type reportAnswer struct {
	Tool         string `json:"tool"`
	Category     string `json:"category"`
	Results      int    `json:"results"`
	Dropped      int    `json:"dropped"`
	FirstDropped string `json:"first_dropped,omitempty"`

	id int64 // the stored row's: a run's scans read it
}

// putReport attaches one external tool's SARIF report to a package of the project. The per-actor limit is
// this route's alone, judged before the body is read: every PUT, refused or not, spends one of the actor's
// attempts (a refusal says whether a report's text is in the code it flags), and the actor's gate runs
// and stored reports of the hour count too.
func (a *app) putReport(w http.ResponseWriter, r *http.Request) {
	ctx, project, by := r.Context(), r.PathValue("id"), actorOf(r.Context())
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
		answer, err = a.attachReport(ctx, project, r.PathValue("pkg"), raw, by, "", nil, nil, runs.Evidence{})
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
		a.log.Printf("canyon-server: attach a report to %s: %v", project, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

// attachReport attaches one SARIF report to a package of the project (contracts/finding.md, In: external
// SARIF): normalised against the composed snapshot and the package's tree, then, under the ledger lock,
// stored as the package's report for its (tool, category) and audited with the digest it replaced.
// Canyon's own scans call it as Canyon, never rate limited, naming the tool they scanned, which the
// report is then, whatever its driver is named ("" is the name's), with the rules and files the executor's
// answer says the tool could not load and did not read whole, and the image that ran with its database's
// date, stored with it. A refusal is a *refused or
// one of sarif's typed errors, and stores nothing.
func (a *app) attachReport(ctx context.Context, project, name string, raw []byte, by actor, tool string, unloaded, unparsed []string, ev runs.Evidence) (reportAnswer, error) {
	pkg, commit, err := reportPackage(ctx, a.pool, project, name)
	if err != nil {
		return reportAnswer{}, err
	}
	snap, err := a.effectivePolicy(ctx, a.pool) // a tool once added is never removed, so its rule cannot go before the store
	if err != nil {
		return reportAnswer{}, err
	}
	rep, err := a.normalize(ctx, project, commit, raw, &snap, tool)
	if err != nil {
		return reportAnswer{}, err
	}
	rep.Unparsed = slices.Compact(slices.Sorted(slices.Values(append(rep.Unparsed, unparsed...)))) // the report's own and the executor's answer's
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	tx, err := a.mutation(ctx)
	if err != nil {
		return reportAnswer{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err := reportPackage(ctx, tx, project, name); err != nil { // deleted, or being deleted, while the report was read
		return reportAnswer{}, err
	}
	if _, err := runs.ReportKey(ctx, tx, a.vault, project); err != nil { // the project's first attach makes its key; a run only opens it
		return reportAnswer{}, err
	}
	id, replaced, err := runs.AttachReportTx(ctx, tx, project, pkg, runs.Report{Report: rep, Digest: digest, AttachedBy: by, Unloaded: unloaded, Unparsed: rep.Unparsed, Evidence: ev})
	if err != nil {
		return reportAnswer{}, err
	}
	var was any // null when the report replaced none
	if replaced != "" {
		was = replaced
	}
	if by.Kind != "canyon" { // Canyon's own scan is its run's: the verdict row rules on it, the timeline holds it (contracts/finding.md, Canyon-run tools; #1704)
		if err := events.AuditTx(ctx, tx, by, "attach report", project, "", map[string]any{"package": pkg, "tool": rep.Tool, "category": rep.Category, "digest": digest, "replaced": was}); err != nil {
			return reportAnswer{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return reportAnswer{}, err
	}
	return reportAnswer{Tool: rep.Tool, Category: rep.Category, Results: len(rep.Results), Dropped: rep.Dropped, FirstDropped: rep.FirstDropped, id: id}, nil
}

// normalizing bounds the attaches reading a package tree at once, each extracting and walking the whole package.
var normalizing = make(chan struct{}, 2)

func (a *app) normalize(ctx context.Context, project, commit string, raw []byte, snap *gatepolicy.Snapshot, tool string) (sarif.Report, error) {
	normalizing <- struct{}{}
	defer func() { <-normalizing }()
	dir, err := os.MkdirTemp("", "canyon-report-")
	if err != nil {
		return sarif.Report{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := ingest.Extract(ctx, filepath.Join(a.repos, project+".git"), commit, dir); err != nil {
		return sarif.Report{}, err
	}
	tree, err := gate.Walk(dir)
	if err != nil {
		return sarif.Report{}, err
	}
	return sarif.NormalizeAs(raw, snap, tree, tool)
}

// reportPackage resolves what the request called a package — an id, else a label — to a package of the
// live project and its commit: any package, a pull request's too (contracts/routes.md, the reports row).
func reportPackage(ctx context.Context, q rower, project, name string) (id, commit string, err error) {
	var deleting bool
	err = q.QueryRow(ctx, "SELECT deleting_since IS NOT NULL FROM projects WHERE id = $1 AND deleted_at IS NULL", project).Scan(&deleting)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", &refused{http.StatusNotFound, "not_found", notOnCanyon(project)}
	case err != nil:
		return "", "", err
	case deleting:
		return "", "", &refused{http.StatusConflict, "deleting", "this project is being deleted"}
	}
	err = q.QueryRow(ctx, "SELECT id, commit FROM packages WHERE project_id = $1 AND (id = $2 OR label = $2) ORDER BY id = $2 DESC LIMIT 1", project, name).Scan(&id, &commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", &refused{http.StatusNotFound, "not_found", "no such package of this project"}
	}
	return id, commit, err
}
