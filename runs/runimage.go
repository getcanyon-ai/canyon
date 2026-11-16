package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// RunImageDigest is the sha256 digest of GET /builder's run_image, in either of its forms: the image ID on
// Docker, <repository>@sha256:<hex> on Kubernetes (contracts/executor.md, GET /builder); "" for another.
func RunImageDigest(run string) string {
	if !strings.Contains(run, "@") {
		run = "@" + run
	}
	return ImageDigest(run)
}

// rescanRunImage is a tick's read of the run image GET /builder names now (design/PATCHING.md §3.2): it
// drops any other run image's scan, reads the image while no finished scan of its digest is stored whole —
// one a layer of which the release before swept is dropped and read again — and matches its covered layers'
// slices not matched yet this tick. An executor that names none, from before run_image, or that cannot tell
// drops every run image's scan, so none is ever read stale. It answers the slices it matched.
func (p *Pipeline) rescanRunImage(ctx context.Context, matched map[string]bool) ([]string, error) {
	if p.Executor == nil {
		return nil, errors.New("runs: no executor configured")
	}
	run, err := p.Executor.RunImage(ctx)
	digest := RunImageDigest(run)
	if err != nil || digest == "" {
		if _, derr := p.Pool.Exec(ctx, "DELETE FROM run_image_scans"); derr != nil {
			return nil, errors.Join(err, derr)
		}
		if err == nil && run != "" {
			err = fmt.Errorf("GET /builder's run_image %q is no image by digest", run)
		}
		return nil, err
	}
	var finished bool
	if err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM run_image_scans WHERE digest <> $1", digest); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM run_image_scans WHERE digest = $1 AND not_finished IS NULL)", digest).Scan(&finished)
	}); err != nil {
		return nil, err
	}
	if finished {
		_, whole, err := p.runImageToMatch(ctx, digest)
		if err != nil {
			return nil, err
		}
		if !whole { // a layer gone with the release before's sweep: read again
			if _, err := p.Pool.Exec(ctx, "DELETE FROM run_image_scans WHERE digest = $1", digest); err != nil {
				return nil, err
			}
			finished = false
		}
	}
	if !finished {
		res, err := p.askScan(ctx, executor.ScanRequest{Tool: "trivy", Target: "image", Image: run}, nil)
		if err != nil {
			return nil, err
		}
		img, err := imageOf(res)
		if err != nil {
			return nil, err
		}
		if err := storeRunImage(ctx, p.Pool, run, digest, img); err != nil || img.NotFinished != "" {
			return nil, cmpErr(err, fmt.Errorf("its image scan did not finish: %s %s", img.NotFinished, img.Reason))
		}
	}
	img, _, err := p.runImageToMatch(ctx, digest)
	if err != nil || img == nil {
		return nil, err
	}
	return p.matchImage(ctx, *img, matched)
}

// storeRunImage writes the run image's scan and its layers in one transaction, all of them or none, as
// storeImage writes an app's: a scan that did not finish is replaced by the next, a finished one never is.
func storeRunImage(ctx context.Context, pool *pgxpool.Pool, run, digest string, img storedImage) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var diffIDs []string // NULL beside not_finished
		if img.NotFinished == "" {
			diffIDs = img.DiffIDs
		}
		var wrote bool
		err := tx.QueryRow(ctx, `INSERT INTO run_image_scans (digest, run_image, scanned_at, diff_ids, unplaced, not_finished, reason)
			VALUES ($1, $2, now(), $3, coalesce($4::jsonb, '[]'), nullif($5, ''), nullif($6, ''))
			ON CONFLICT (digest) DO UPDATE SET run_image = EXCLUDED.run_image, scanned_at = EXCLUDED.scanned_at, diff_ids = EXCLUDED.diff_ids,
				unplaced = EXCLUDED.unplaced, not_finished = EXCLUDED.not_finished, reason = EXCLUDED.reason
			WHERE run_image_scans.not_finished IS NOT NULL RETURNING true`,
			digest, run, diffIDs, img.Unplaced, img.NotFinished, img.Reason).Scan(&wrote)
		if errors.Is(err, pgx.ErrNoRows) { // a finished scan of it is stored: it stands
			return nil
		}
		if err != nil {
			return err
		}
		for _, l := range img.Layers {
			if _, err := tx.Exec(ctx, storePart, l.Part, l.OS, l.Components); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO run_image_layers (digest, diff_id, part_sha) VALUES ($1, $2, $3) ON CONFLICT (digest, diff_id) DO NOTHING",
				digest, l.DiffID, l.Part); err != nil {
				return err
			}
		}
		return nil
	})
}

// runImageToMatch is the run image's finished scan with the layers a re-match covers, every layer the run
// image's own, nil when it covers none; and whether the scan is stored whole: every layer there, and every
// stored row reading. No finished scan is not whole.
func (p *Pipeline) runImageToMatch(ctx context.Context, digest string) (*rescanImage, bool, error) {
	var diffIDs []string
	err := p.Pool.QueryRow(ctx, "SELECT diff_ids FROM run_image_scans WHERE digest = $1 AND not_finished IS NULL", digest).Scan(&diffIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || !trivy.DiffIDsRead(diffIDs) {
		return nil, false, err
	}
	rows, err := p.Pool.Query(ctx, `SELECT l.diff_id, l.part_sha, s.components FROM run_image_layers l JOIN layer_parts s USING (part_sha)
		WHERE l.digest = $1 AND l.diff_id = ANY($2)`, digest, diffIDs)
	if err != nil {
		return nil, false, err
	}
	owners, layers, parts, whole := map[string]string{}, map[string][]trivy.Component{}, map[string]string{}, true
	var d, part string
	var raw []byte
	if _, err := pgx.ForEachRow(rows, []any{&d, &part, &raw}, func() error {
		cs, ok := trivy.DecodeComponents(raw)
		whole = whole && ok && trivy.StoredIn(cs, d)
		owners[d], layers[d], parts[d] = trivy.RunImage, cs, part
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !whole || len(layers) != len(diffIDs) {
		return nil, false, nil
	}
	img := rescanImage{Digest: digest, RunImage: true}
	for _, d := range trivy.Covered(diffIDs, trivy.Packages(diffIDs, owners, layers), false) {
		img.Layers = append(img.Layers, rescanLayer{DiffID: d, Part: parts[d]})
	}
	if len(img.Layers) == 0 {
		return nil, true, nil
	}
	return &img, true, nil
}
