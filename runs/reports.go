package runs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
)

// ReportKey answers the project's report key (contracts/finding.md: 32 random bytes Canyon generates
// per project, sealed in the vault, never exported or shown), generating it in tx on the first call.
// The UPDATE writes only a project that has none, so of two first calls the second waits on the first's
// row lock and, once that commits, writes nothing and reads the key the first sealed. A deleted
// project's key went with its secrets and none is written: the read fails.
func ReportKey(ctx context.Context, tx pgx.Tx, keys *vault.Vault, project string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	version, sealedKey, sealedValue, err := keys.Seal(project, "report", "key", key)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "UPDATE projects SET report_key_ref = $2, report_sealed_key = $3, report_sealed_value = $4 WHERE id = $1 AND report_sealed_value IS NULL AND deleted_at IS NULL", project, version, sealedKey, sealedValue); err != nil {
		return nil, err
	}
	return openReportKey(ctx, tx, keys, project)
}

// openReportKey opens the project's report key and writes nothing: the gate's read. An attach makes the
// key before it stores a report, so a project without one has no report to fingerprint, and fails closed.
func openReportKey(ctx context.Context, q querier, keys *vault.Vault, project string) ([]byte, error) {
	var version *int
	var sealedKey, sealedValue []byte
	if err := q.QueryRow(ctx, "SELECT report_key_ref, report_sealed_key, report_sealed_value FROM projects WHERE id = $1", project).Scan(&version, &sealedKey, &sealedValue); err != nil {
		return nil, err
	}
	if version == nil {
		return nil, fmt.Errorf("runs: project %s has no report key, so none of its reports can be fingerprinted", project)
	}
	return keys.Open(*version, project, "report", "key", sealedKey, sealedValue)
}

// Report is one attached report as package_reports keeps it (contracts/finding.md, In: external
// SARIF): the normalised report, the raw report's digest (sha256:<64 hex>), who attached it, and the
// rules and files the executor's answer said its tool could not load and did not read whole
// (executor.md, the scan row's unloaded and unparsed).
type Report struct {
	sarif.Report
	Digest     string
	AttachedBy events.Actor
	Unloaded   []string
	Unparsed   []string
	Evidence
}

// Evidence is what a Canyon-run scan's report holds beside every report's own: the image that ran, by
// digest, and for trivy and grype when their database was built (contracts/finding.md); zero on an upload.
type Evidence struct {
	Image     string
	DBBuiltAt string // RFC 3339
}

// AttachReportTx stores rep as the package's current report for its (tool, category) and answers
// the new row's id and the digest of the report it replaced, "" when none. The caller holds the
// ledger lock (events.LockTx), so the newest attach is the highest id and a run's start reads an
// attach whole or not at all. Every attach is a new row, the results stored without partialFingerprints.
func AttachReportTx(ctx context.Context, tx pgx.Tx, project, pkg string, rep Report) (id int64, replaced string, err error) {
	err = tx.QueryRow(ctx, "SELECT digest FROM package_reports WHERE package_id = $1 AND tool = $2 AND category = $3 ORDER BY id DESC LIMIT 1", pkg, rep.Tool, rep.Category).Scan(&replaced)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", err
	}
	results, err := json.Marshal(rep.Results)
	if err != nil {
		return 0, "", err
	}
	err = tx.QueryRow(ctx, `INSERT INTO package_reports (project_id, package_id, tool, category, digest, version, results, dropped, first_dropped, attached_by_kind, attached_by_subject, attached_by_display, unloaded, unparsed, image, db_built_at)
		VALUES ($1, $2, $3, $4, $5, $6, (SELECT coalesce(jsonb_agg(r - 'partialFingerprints' ORDER BY n), '[]') FROM jsonb_array_elements($7::jsonb) WITH ORDINALITY e(r, n)), $8, NULLIF($9, ''), $10, $11, $12, $13, $14, NULLIF($15, ''), NULLIF($16, '')::timestamptz)
		RETURNING id`, project, pkg, rep.Tool, rep.Category, rep.Digest, rep.Version, results, rep.Dropped, rep.FirstDropped,
		rep.AttachedBy.Kind, rep.AttachedBy.Subject, rep.AttachedBy.Display, append([]string{}, rep.Unloaded...), append([]string{}, rep.Unparsed...), rep.Image, rep.DBBuiltAt).Scan(&id) // never NULL: none is {}
	return id, replaced, err
}

// Querier is a pool or a transaction: the one a hook of Canyon Enterprise's reads on too (design/ARCHITECTURE.md §8).
type Querier = enterprise.Querier

type querier = Querier

// importedTools are the tools whose rule sarif.<tool> the snapshot has on (contracts/snapshot.md, Two gates), in its order.
func importedTools(snap policy.Snapshot) []string {
	var tools []string
	for _, r := range snap.Rules {
		if tool, ok := strings.CutPrefix(r.ID, "sarif."); ok && r.Producer == policy.Sarif && r.On() {
			tools = append(tools, tool)
		}
	}
	return tools
}

// scannedTools are the rules of the Canyon-run tools the snapshot has on — those through the
// executor with run on, and gitleaks, which runs in process — in its order (contracts/finding.md,
// Canyon-run tools).
func scannedTools(snap policy.Snapshot) []policy.Rule {
	return slices.DeleteFunc(slices.Clone(snap.Rules), func(r policy.Rule) bool {
		return !r.Run && r.ID != "sarif.gitleaks" || r.Producer != policy.Sarif || !r.On()
	})
}

// skippedScans are the tools a laptop host's run says it skipped (contracts/events.md, progress's skipped; #237):
// trivy, whose rule is on while its run is off, as the laptop demo leaves it. None on any other host, where a tool an
// admin turned off is the not-checked line alone, as before.
func skippedScans(snap policy.Snapshot, laptop bool) []string {
	if !laptop {
		return nil
	}
	i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif.trivy" })
	if i < 0 || !snap.Rules[i].On() || snap.Rules[i].Run {
		return nil
	}
	return []string{"trivy"}
}

// CurrentReports answers the ids of the package's current reports of tools, one per (tool, category),
// in that order: what a run's start journals, so a replayed step reads the same reports.
func CurrentReports(ctx context.Context, q querier, pkg string, tools []string) ([]int64, error) {
	rows, err := q.Query(ctx, "SELECT DISTINCT ON (tool, category) id FROM package_reports WHERE package_id = $1 AND tool = ANY($2) ORDER BY tool, category, id DESC", pkg, tools)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ReadReports answers the package's reports by the ids a run journaled, in (tool, category) order;
// an id the package does not hold fails the read, never shortens it.
func ReadReports(ctx context.Context, q querier, pkg string, ids []int64) ([]Report, error) {
	rows, err := q.Query(ctx, `SELECT tool, category, version, results, dropped, coalesce(first_dropped, ''), digest, attached_by_kind, attached_by_subject, attached_by_display, unloaded, unparsed
		FROM package_reports WHERE package_id = $1 AND id = ANY($2) ORDER BY tool, category, id`, pkg, ids)
	if err != nil {
		return nil, err
	}
	reports, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (r Report, err error) {
		err = row.Scan(&r.Tool, &r.Category, &r.Version, &r.Results, &r.Dropped, &r.FirstDropped, &r.Digest, &r.AttachedBy.Kind, &r.AttachedBy.Subject, &r.AttachedBy.Display, &r.Unloaded, &r.Unparsed)
		if len(r.Unloaded) == 0 { // stored {} is none, as the attach answers it
			r.Unloaded = nil
		}
		if len(r.Unparsed) == 0 {
			r.Unparsed = nil
		}
		return r, err
	})
	if err == nil && len(reports) != len(ids) {
		err = fmt.Errorf("runs: %d of %d reports are stored for package %s", len(reports), len(ids), pkg)
	}
	return reports, err
}
