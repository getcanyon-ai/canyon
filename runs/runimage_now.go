package runs

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// NamedRunImage is the run image Canyon names now, as the newest rescan tick read it from GET /builder: the one
// run_image_scans row (rescanRunImage keeps that one alone), and its match read as the plan reads it
// (contracts/routes.md, envs.<env>.patch and the identities row). Image is "" when no tick has read one; a match
// that does not read leaves Reads false, and nothing is then read as waiting on it.
type NamedRunImage struct {
	Image string
	vulns *runImageVulns
}

// Reads is whether the run image's match reads, so the plan could tell a fix it carries from one it does not.
func (n NamedRunImage) Reads() bool { return n.vulns != nil }

// Waits is whether an image with these findings waits on a newer run image (design/PATCHING.md §0.4): a counted os
// finding at or above high is fixed in no version this run image carries. Never true where Unchecked is.
func (n NamedRunImage) Waits(findings []PlatformFinding) bool {
	if n.vulns == nil || n.Unchecked(findings) {
		return false
	}
	outcome, _ := plan(findings, n.vulns)
	return outcome == OutcomeWait
}

// Unchecked is whether such an image has a counted os finding at or above high that the run image's match could
// tell nothing of: its match does not read, or did not match that package. Never read as waiting (fail closed): the
// plan reads such a package as one the run image does not fix, which nothing here can back up.
func (n NamedRunImage) Unchecked(findings []PlatformFinding) bool {
	if outcome, _ := plan(findings, n.vulns); outcome == OutcomeUnchecked || n.vulns == nil {
		return outcome == OutcomeUnchecked
	}
	return slices.ContainsFunc(findings, func(f PlatformFinding) bool {
		return f.Class == "os" && (f.Severity == "critical" || f.Severity == "high") && n.vulns.unchecked[f.Name]
	})
}

// Plan is the plan's outcome for an image with these findings against this run image (design/PATCHING.md §0.4,
// §4.2), and each finding a rebase onto it fixes: OutcomeUnchecked when its match does not read.
func (n NamedRunImage) Plan(findings []PlatformFinding) (outcome string, fixes []PlatformFinding) {
	return plan(findings, n.vulns)
}

// RunImageOf is run, the run image GET /builder names, as the plan reads it at now (readRunImage): its match does not
// read while no finished scan of its digest is stored.
func RunImageOf(ctx context.Context, q Querier, run string, now time.Time) (NamedRunImage, error) {
	read := NamedRunImage{Image: run}
	facts, stored, err := runImageFactsOf(ctx, q, RunImageDigest(run))
	if err == nil && stored {
		read.vulns = readRunImage(facts, now)
	}
	return read, err
}

// runImageMemo holds the decoded read of the named run image, keyed by its row and its layers' newest matches: it
// changes only at a tick, so a read polled every 2 s costs the key's query. A failed read is never held.
type runImageMemo struct {
	mu    sync.Mutex
	key   string
	until time.Time // its oldest layer's database turns stale then, and the read no longer reads
	read  NamedRunImage
}

// runImageKeySQL is the named run image's row and what moves when a tick matches it again, or matches a slice of it
// another image shares: its newest match's time, and its layers' oldest newest database.
const runImageKeySQL = `SELECT s.digest, s.run_image, s.scanned_at, s.rematched_at, s.rescan_failed_at, s.not_finished IS NOT NULL,
	(SELECT max(m.matched_at) FROM run_image_layers l JOIN part_matches m USING (part_sha) WHERE l.digest = s.digest),
	(SELECT min(newest) FROM (SELECT max(m.db_built_at) AS newest FROM run_image_layers l JOIN part_matches m USING (part_sha) WHERE l.digest = s.digest GROUP BY l.diff_id) x)
FROM run_image_scans s ORDER BY s.scanned_at DESC LIMIT 1`

// NamedRunImage reads the run image Canyon names now (NamedRunImage), from the memo while its key stands.
func (p *Pipeline) NamedRunImage(ctx context.Context) (NamedRunImage, error) {
	var digest, image string
	var scanned time.Time
	var rematched, failed, matched, oldest *time.Time
	var unfinished bool
	err := p.Pool.QueryRow(ctx, runImageKeySQL).Scan(&digest, &image, &scanned, &rematched, &failed, &unfinished, &matched, &oldest)
	if errors.Is(err, pgx.ErrNoRows) {
		return NamedRunImage{}, nil
	}
	if err != nil {
		return NamedRunImage{}, err
	}
	stamp := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	key := digest + " " + image + " " + stamp(&scanned) + " " + stamp(rematched) + " " + stamp(failed) + " " + stamp(matched) + " " + stamp(oldest)
	now := time.Now()
	m := &p.named
	m.mu.Lock()
	defer m.mu.Unlock()
	// ponytail: one entry, the one run image the store holds; a second would need a map and an eviction rule.
	if m.key == key && now.Before(m.until) {
		return m.read, nil
	}
	read := NamedRunImage{Image: image}
	if !unfinished {
		facts, stored, err := runImageFactsOf(ctx, p.Pool, digest)
		if err != nil {
			return NamedRunImage{}, err // never held: the next read asks again
		}
		if stored {
			read.vulns = readRunImage(facts, now)
		}
	}
	until := now.Add(staleDatabase)
	if oldest != nil {
		until = oldest.Add(staleDatabase)
	}
	m.key, m.until, m.read = key, until, read
	return read, nil
}
