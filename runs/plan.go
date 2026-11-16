package runs

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// PlatformFinding is one vulnerability platform.known-vuln counts in an image (contracts/finding.md, Canyon's
// layers): a package Canyon put there, of class os, runtime or lifecycle, with a fixed version published.
// PlatformImage is an image's platform.known-vuln read (Pipeline.PlatformRead): whether it checked, its why, and
// its counted findings.
type PlatformImage struct {
	Checked  bool
	Why      string
	Findings []PlatformFinding
}

type PlatformFinding struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Vulnerability string `json:"vulnerability"`
	Fixed         string `json:"fixed"`
	Class         string `json:"class"`
	Severity      string `json:"severity"`        // critical | high | medium | low, as the finding carries it
	Owner         string `json:"owner,omitempty"` // whose layer holds it, as the image's label names it: a buildpack's id, run-image, lifecycle or app
}

// The plan's outcomes that start nothing in Phase 2 but a rebase (design/PATCHING.md §0.4); unchecked is
// an os finding the named run image's match cannot answer for: no rebase, fail closed.
const (
	OutcomeNothing   = "nothing"
	OutcomeRebase    = "rebase"
	OutcomeWait      = "wait"
	OutcomeBump      = "bump"
	OutcomeUnchecked = "unchecked"
)

// runImageVulns is what the named run image's newest match holds: its os packages by name, the
// vulnerabilities matched against each, and the packages it could not match (contracts/finding.md,
// not_checked).
// RebuildFixes are the counted findings at or above high a rebuild may fix: of class runtime or lifecycle.
func RebuildFixes(findings []PlatformFinding) []PlatformFinding {
	var out []PlatformFinding
	for _, f := range findings {
		if (f.Severity == "critical" || f.Severity == "high") && (f.Class == "runtime" || f.Class == "lifecycle") {
			out = append(out, f)
		}
	}
	return out
}

type runImageVulns struct {
	held      map[string]bool
	matched   map[string][]string
	unchecked map[string]bool
}

// carries is whether the run image carries a fixed version of f's package (design/PATCHING.md §0.4): it
// holds the package, matched, and f's vulnerability is not matched against it. A package it does not hold
// is no fix: what the rebase would remove is not known to be fixed.
func (r runImageVulns) carries(f PlatformFinding) bool {
	return r.held[f.Name] && !r.unchecked[f.Name] && !slices.Contains(r.matched[f.Name], f.Vulnerability)
}

// plan is the outcome for an image's counted findings (design/PATCHING.md §0.4, §4.2): only findings at or
// above high count, the severity at which the rule can block. An os finding is rebased when the named run
// image fixes at least one of them, each it fixes in fixes; waits when it fixes none; and is unchecked when
// its match cannot be read (run nil). Runtime and lifecycle findings alone are a bump, Phase 3's.
func plan(findings []PlatformFinding, run *runImageVulns) (outcome string, fixes []PlatformFinding) {
	var os, other bool
	for _, f := range findings {
		if f.Severity != "critical" && f.Severity != "high" {
			continue
		}
		switch f.Class {
		case "os":
			os = true
			if run != nil && run.carries(f) {
				fixes = append(fixes, f)
			}
		case "runtime", "lifecycle":
			other = true
		}
	}
	switch {
	case os && run == nil:
		return OutcomeUnchecked, nil
	case len(fixes) > 0:
		return OutcomeRebase, fixes
	case os:
		return OutcomeWait, nil
	case other:
		return OutcomeBump, nil
	}
	return OutcomeNothing, nil
}

// runImageFacts is what the store holds of the run image's scan and its covered layers' newest matches.
type runImageFacts struct {
	diffIDs  []string
	unplaced []trivy.Component
	layers   map[string][]trivy.Component
	newest   map[string]storedMatch
	failed   bool // its last rescan did not match it whole
	readable bool // every stored row of it decodes
}

// readRunImage is the named run image's match as the plan reads it: nil, fail closed, unless a finished scan
// of exactly digest — GET /builder's run_image now — is stored whole, holds os packages, with none trivy
// placed in no layer, and every covered layer's newest match reads, joins its packages and is within the 5
// days at now, with no later rescan of it failed (design/PATCHING.md §3.2). A run image whose scan found no
// os package — an operating system trivy did not know, an empty SBOM — is never read as fixing them all.
func readRunImage(f runImageFacts, now time.Time) *runImageVulns {
	if !f.readable || f.failed || !trivy.DiffIDsRead(f.diffIDs) || slices.ContainsFunc(f.unplaced, func(c trivy.Component) bool { return c.Type == "library" }) {
		return nil
	}
	owners := map[string]string{}
	for _, d := range f.diffIDs {
		if _, ok := f.layers[d]; !ok || !trivy.StoredIn(f.layers[d], d) {
			return nil
		}
		owners[d] = trivy.RunImage
	}
	pkgs := trivy.Packages(f.diffIDs, owners, f.layers)
	out := &runImageVulns{held: map[string]bool{}, matched: map[string][]string{}, unchecked: map[string]bool{}}
	for _, p := range pkgs {
		if p.Class == "os" {
			out.held[p.FullName()] = true
		}
	}
	if len(out.held) == 0 {
		return nil
	}
	for _, d := range trivy.Covered(f.diffIDs, pkgs, false) {
		m, ok := f.newest[d]
		report, readable := trivy.DecodeReport(m.Report)
		if !ok || !readable || m.DBBuiltAt.After(now) || now.Sub(m.DBBuiltAt) > staleDatabase || !trivy.Joins(report, d, pkgs) {
			return nil
		}
		names := map[string]string{}
		for _, p := range pkgs {
			if p.Layer == d {
				names[p.BOMRef] = p.FullName()
				if p.PURL == "" || slices.Contains(m.Unscanned, p.BOMRef) {
					out.unchecked[p.FullName()] = true
				}
			}
		}
		for _, r := range report.Results {
			for _, v := range r.Vulnerabilities {
				name := names[v.PkgIdentifier.BOMRef]
				out.matched[name] = append(out.matched[name], v.VulnerabilityID)
			}
		}
	}
	return out
}

// runImageFactsOf loads the run image's scan of digest and its layers' newest matches; ok false when no
// finished scan of it is stored.
func runImageFactsOf(ctx context.Context, q querier, digest string) (f runImageFacts, ok bool, err error) {
	var unplaced []byte
	err = q.QueryRow(ctx, "SELECT diff_ids, unplaced, rescan_failed_at IS NOT NULL FROM run_image_scans WHERE digest = $1 AND not_finished IS NULL", digest).
		Scan(&f.diffIDs, &unplaced, &f.failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	f.unplaced, f.readable = trivy.DecodeComponents(unplaced)
	f.layers, f.newest = map[string][]trivy.Component{}, map[string]storedMatch{}
	rows, err := q.Query(ctx, `SELECT l.diff_id, s.components FROM run_image_layers l JOIN layer_parts s USING (part_sha) WHERE l.digest = $1`, digest)
	if err != nil {
		return f, false, err
	}
	var d string
	var raw []byte
	if _, err := pgx.ForEachRow(rows, []any{&d, &raw}, func() error {
		cs, ok := trivy.DecodeComponents(raw)
		f.readable = f.readable && ok
		f.layers[d] = cs
		return nil
	}); err != nil {
		return f, false, err
	}
	rows, err = q.Query(ctx, `SELECT DISTINCT ON (l.diff_id) l.diff_id, m.db_built_at, m.report, m.unscanned FROM run_image_layers l
		JOIN part_matches m USING (part_sha) WHERE l.digest = $1 ORDER BY l.diff_id, m.db_built_at DESC`, digest)
	if err != nil {
		return f, false, err
	}
	var m storedMatch
	_, err = pgx.ForEachRow(rows, []any{&d, &m.DBBuiltAt, &m.Report, &m.Unscanned}, func() error {
		f.newest[d], m = m, storedMatch{}
		return nil
	})
	return f, true, err
}
