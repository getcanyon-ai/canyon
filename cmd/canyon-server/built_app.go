package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
)

// depsRule is the scanner rule whose row before production reads the built app too (design/PATCHING.md §3.6).
const depsRule = "deps.known-vuln"

// builtApp is deps.known-vuln's read of the image a receipt is for (contracts/finding.md, The built app): the
// packages only the image holds that its review didn't report, which it counts.
type builtApp struct {
	why       string       // why it could not be checked, "" when it was
	packages  []appPackage // what it counts, the most severe first
	approved  []appPackage // what it would count, but an approval covers whole, in the same order
	unchecked int          // the packages of the built app it could not check
}

// appPackage is a counted package, by its name and installed version, with each of its advisories at high or
// above and what fixes it, "" for none published, and the fingerprint of each advisory in each class it is
// held in (contracts/finding.md, The built app, Fingerprint): what an approval of it names.
type appPackage struct {
	name, version string
	rank          int // its most severe advisory's: 0 critical, 1 high
	advisories    []platformAdvisory
	fingerprints  []string
}

// severity is the package's most severe advisory's, as its rank says.
func (p appPackage) severity() finding.Severity {
	return []finding.Severity{finding.Critical, finding.High}[p.rank]
}

// builtAppClass is whether a component's class is one the built app counts: what Canyon put there is
// platform.known-vuln's, everything else the team's (design/PATCHING.md §2).
func builtAppClass(class string) bool { return class == "app" || class == "unknown" }

// readBuiltApp is what deps.known-vuln counts of the image f holds at now, past what the review reported
// (contracts/finding.md, The built app; routes.md, The promote receipt). Pure, as readImage is. It fails closed
// as platform.known-vuln's read of the same image does, whatever that rule's state, then over its own layers.
func readBuiltApp(f imageFacts, reported map[trivy.PackageKey]bool, now time.Time) builtApp {
	if read := readImage(f, "preview", "", gatepolicy.Off, "", now); read.State != "checked" {
		return builtApp{why: strings.TrimPrefix(read.Why, "could not check: ")}
	}
	owners, _ := trivy.Owners(f.diffIDs, f.label)
	pkgs := trivy.Packages(f.diffIDs, owners, f.layers)
	var layers []string
	for _, d := range f.diffIDs {
		if slices.ContainsFunc(pkgs, func(p trivy.Package) bool { return p.Layer == d && builtAppClass(p.Class) }) {
			layers = append(layers, d)
		}
	}
	why := ""
	for _, d := range layers {
		switch m, ok := f.newest[d]; {
		case !ok:
			why = cmp.Or(why, "the image has not been matched yet")
		case m.dbBuiltAt.After(now):
			why = cmp.Or(why, "a stored scan of the image does not read")
		case now.Sub(m.dbBuiltAt) > platformStale:
			why = cmp.Or(why, "its vulnerability database is over 5 days old")
		}
	}
	if f.appRescanFailed && len(layers) > 0 { // what its app layers matched before is no answer for now
		why = cmp.Or(why, "its last rescan did not finish")
	}
	if why != "" {
		return builtApp{why: why}
	}
	var out builtApp
	for _, c := range f.unplaced { // a package trivy names no layer for is unknown, and never matched: not checked
		if c.Type == "library" {
			out.unchecked++
		}
	}
	for _, d := range layers {
		m := f.newest[d]
		if !trivy.Joins(m.report, d, pkgs) {
			return builtApp{why: "a match names a package the image does not hold"}
		}
		mine, unchecked := map[string]trivy.Package{}, map[string]bool{}
		for _, p := range pkgs {
			if p.Layer == d && builtAppClass(p.Class) {
				mine[p.BOMRef] = p
				unchecked[p.BOMRef] = p.PURL == "" || slices.Contains(m.unscanned, p.BOMRef)
			}
		}
		for _, no := range unchecked {
			if no {
				out.unchecked++
			}
		}
		for _, r := range m.report.Results {
			for _, v := range r.Vulnerabilities {
				p, ok := mine[v.PkgIdentifier.BOMRef]
				if !ok || unchecked[p.BOMRef] {
					continue
				}
				rank := slices.Index([]finding.Severity{finding.Critical, finding.High}, severityOf(v))
				key, keyed := trivy.KeyOf(p.Component)
				if rank < 0 || keyed && reported[key] {
					continue
				}
				i := slices.IndexFunc(out.packages, func(a appPackage) bool { return a.name == p.FullName() && a.version == p.Version })
				if i < 0 {
					i, out.packages = len(out.packages), append(out.packages, appPackage{name: p.FullName(), version: p.Version, rank: rank})
				}
				out.packages[i].rank = min(out.packages[i].rank, rank)
				if !slices.ContainsFunc(out.packages[i].advisories, func(a platformAdvisory) bool { return a.ID == v.VulnerabilityID }) {
					out.packages[i].advisories = append(out.packages[i].advisories, platformAdvisory{ID: v.VulnerabilityID, Fixed: v.FixedVersion})
				}
				if fp := imageFingerprint(depsRule, v.VulnerabilityID, p.FullName(), p.Version, p.Class); !slices.Contains(out.packages[i].fingerprints, fp) {
					out.packages[i].fingerprints = append(out.packages[i].fingerprints, fp)
				}
			}
		}
	}
	slices.SortFunc(out.packages, func(x, y appPackage) int {
		return cmp.Or(cmp.Compare(x.rank, y.rank), cmp.Compare(x.name, y.name), cmp.Compare(x.version, y.version))
	})
	return out
}

// severityOf is a vulnerability's severity as Canyon reads it: trivy's, lowercased, and high for any it cannot read.
func severityOf(v trivy.Vuln) finding.Severity {
	sev := finding.Severity(strings.ToLower(v.Severity))
	if _, err := sev.Level(); err != nil {
		return finding.High
	}
	return sev
}

// clean is whether the built app passes: checked, and nothing counted.
func (b builtApp) clean() bool { return b.why == "" && len(b.packages) == 0 }

// approve moves out of what b counts each package whose every fingerprint covered holds: an approval covers a
// package only whole, every advisory of it in every class it is held in.
func (b *builtApp) approve(covered map[string]bool) {
	b.packages = slices.DeleteFunc(b.packages, func(p appPackage) bool {
		if !slices.ContainsFunc(p.fingerprints, func(fp string) bool { return !covered[fp] }) {
			b.approved = append(b.approved, p)
			return true
		}
		return false
	})
}

// listed is the built app's packages as deps.known-vuln's row lists them: each it counts, then each an approval
// covers; none from a read that could not check, or none read.
func (b *builtApp) listed() []builtAppPackage {
	if b == nil || b.why != "" {
		return nil
	}
	var out []builtAppPackage
	for i, p := range slices.Concat(b.packages, b.approved) {
		out = append(out, builtAppPackage{Name: p.name, Version: p.version, Advisories: p.advisories, Fingerprints: p.fingerprints, Approved: i >= len(b.packages)})
	}
	return out
}

// builtAppCoveredSQL is which of fingerprints $3, deps.known-vuln's of a built app, an approval of project $1
// covers for run $2, the review the receipt is read for, standing $4 longer: as a blocking finding of that run is
// covered (runs.CoversBlocking), its author's own approval never.
var builtAppCoveredSQL = `SELECT a.fingerprint FROM approvals a JOIN runs g ON g.id = $2
WHERE a.project_id = $1 AND a.rule = '` + depsRule + `' AND a.fingerprint = ANY($3) AND ` + runs.CoversBlocking("g", "now() + $4::interval")

// approveBuiltApp moves out of what app counts each package the project's approvals cover for run, margin longer;
// with no run, none.
func approveBuiltApp(ctx context.Context, q querier, project, run string, app *builtApp, margin time.Duration) error {
	var fps []string
	for _, p := range app.packages {
		fps = append(fps, p.fingerprints...)
	}
	if run == "" || len(fps) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, builtAppCoveredSQL, project, run, fps, margin)
	if err != nil {
		return err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	covered := map[string]bool{}
	for _, fp := range got {
		covered[fp] = true
	}
	app.approve(covered)
	return nil
}

// row is deps.known-vuln's row once its review's would pass with passed, version the run's (routes.md, The
// promote receipt): whether it passes, and why, cut to 200 code points.
func (b builtApp) row(version, passed string) (bool, string) {
	why := passed + "; its built app holds no known-vulnerable package its review didn't see"
	switch {
	case b.why != "":
		why = version + "'s built app could not be checked: " + b.why
	case len(b.packages) > 1:
		why = fmt.Sprintf("%d known-vulnerable packages are in %s's built app, but its review didn't see them: %s %s and %d more",
			len(b.packages), version, b.packages[0].name, b.packages[0].version, len(b.packages)-1)
	case len(b.packages) == 1:
		p := b.packages[0]
		fix := fmt.Sprintf("%d advisories are published against it", len(p.advisories))
		switch a := p.advisories[0]; {
		case len(p.advisories) > 1:
		case a.Fixed == "":
			fix = "no fix is published for " + a.ID
		case strings.Contains(a.Fixed, ", "): // a list of versions fixes it in the plural
			fix = a.Fixed + " fix it"
		default:
			fix = a.Fixed + " fixes it"
		}
		why = p.name + " " + p.version + " is in " + version + "'s built app, but its review didn't see it: " + fix
	case len(b.approved) > 0:
		why = passed + "; its built app's known-vulnerable packages its review didn't see are approved: " + b.approved[0].name + " " + b.approved[0].version
		if len(b.approved) > 1 {
			why += fmt.Sprintf(" and %d more", len(b.approved)-1)
		}
	}
	if b.why == "" && len(b.packages) == 0 && b.unchecked > 0 {
		why += fmt.Sprintf("; Canyon couldn't check %d package%s in it", b.unchecked, plural(b.unchecked))
	}
	r := []rune(why)
	return b.clean(), string(r[:min(len(r), 200)])
}

// reportedIn is the packages a run's deps.known-vuln findings name (contracts/finding.md, The built app): each
// by its file's purl type, and its dependency's name and version. A finding whose file names no type, or which
// names no dependency, stands for no package of the image.
func reportedIn(log finding.Log) map[trivy.PackageKey]bool {
	out := map[trivy.PackageKey]bool{}
	for _, run := range log.Runs {
		for _, r := range run.Results {
			if r.RuleID != depsRule && !strings.HasPrefix(r.RuleID, depsRule+".") || len(r.Locations) == 0 {
				continue
			}
			a, ok := trivy.Read(r.Properties.Canyon.Problem)
			if !ok {
				continue
			}
			if key, ok := trivy.KeyOfFile(r.Locations[0].PhysicalLocation.ArtifactLocation.URI, a.Name, a.Installed); ok {
				out[key] = true
			}
		}
	}
	return out
}

// askBuiltApps asks the images of projects, every live one's when it names none, read for deps.known-vuln placed
// before production now (design/PATCHING.md §3.6). The edit has landed whatever it answers: one that asks nothing
// is logged, and the next rescan tick asks, while the row reads could not be checked meanwhile.
func (a *app) askBuiltApps(ctx context.Context, projects []string) {
	n, err := a.pipe.AskBuiltApps(context.WithoutCancel(ctx), a.durable, projects, "policy@"+time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		a.log.Printf("canyon-server: deps.known-vuln placed before production: asked %d image reads, then: %v", n, err)
	}
}

// readBuiltApps reads the built app of image, the image the receipt is for, onto the deps.known-vuln row of
// placed, when it is placed (routes.md, The promote receipt), each package an approval covers for run, the review
// the receipt is read for, margin longer, approved rather than counted.
func readBuiltApps(ctx context.Context, q querier, project, run, image string, placed []placedRule, margin time.Duration) error {
	for i := range placed {
		if placed[i].rule.ID != depsRule {
			continue
		}
		f, err := loadImage(ctx, q, project, runs.ImageDigest(image))
		if err != nil {
			return err
		}
		app := readBuiltApp(f, placed[i].reported, time.Now())
		if err := approveBuiltApp(ctx, q, project, run, &app, margin); err != nil {
			return err
		}
		placed[i].app = &app
	}
	return nil
}
