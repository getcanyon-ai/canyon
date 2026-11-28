package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/package-url/packageurl-go"

	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
)

// platformRule is the go-live rule that judges the image preview runs (design/PATCHING.md §3.3).
const platformRule = "platform.known-vuln"

// platformStale is how old a match's database may be and still be read: past it the row could not check.
const platformStale = 5 * 24 * time.Hour

const platformNotes = "no fix published, or Canyon can't tell what put it there"

// importedNotes are the notes of a read of an imported tool's report, which may name a package Canyon can't place.
const importedNotes = "no fix published, or Canyon can't tell what put it there or which part of the image it's in"

// platformPlace is where a counted package is, in a finding's problem: never its class's own name (contracts/finding.md).
var platformPlace = map[string]string{"os": "system libraries", "runtime": "build tools", "lifecycle": "build tools"}

// platformWhere is where the row's packages are, after its count (contracts/routes.md, The promote receipt; #2579).
const platformWhere = " with a published fix, in the system libraries and build tools Canyon adds"

func routesPlatform(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /projects/{id}/envs/{env}/image", a.teamOnly(a.imageRead))
}

// layerMatch is a layer's newest re-match.
type layerMatch struct {
	dbBuiltAt time.Time
	report    trivy.Report
	unscanned []string
}

// imageFacts is what the store holds of one image digest; scanned is nil when no image scan is stored.
type imageFacts struct {
	scanned             *time.Time
	notFinished, reason string
	diffIDs             []string
	label               string
	unplaced            []trivy.Component
	layers              map[string][]trivy.Component // each layer's components, absent for a layer never stored
	unreadable          bool                         // a stored row of the image does not decode
	rescanFailed        bool                         // a rescan of the image did not match its layers, and none has since
	appRescanFailed     bool                         // a rescan of the app layers deps.known-vuln alone reads did not match them, and none has since
	newest              map[string]layerMatch        // each layer's newest match, absent before one
	source              string                       // the imported tool the rule reads (settings.source), "" for trivy's own matches
	imported            []importedReport             // source's newest report of the image in each of its categories
}

// importedReport is a tool's newest report of an image in one category (design/PATCHING.md §3.4): when it was attached, and its results.
type importedReport struct {
	category   string
	attachedAt time.Time
	results    []sarif.ImageResult
	unreadable bool
}

// imageRead is GET /api/v1/projects/:id/envs/:env/image's answer (contracts/routes.md).
type imageRead struct {
	Env       string            `json:"env"`
	Digest    string            `json:"image_digest"`
	State     string            `json:"state"` // checked | could_not_check | not_scanned
	Why       string            `json:"why"`
	ScannedAt *string           `json:"scanned_at"`
	DBBuiltAt *string           `json:"db_built_at"`
	ReadAt    string            `json:"read_at"`
	Layers    []imageLayer      `json:"layers"`
	Findings  []platformFinding `json:"findings"`
	Notes     []platformNote    `json:"notes"`
	counted   int               // the packages with a finding at high or above: one entry each
	packages  []platformPackage // those packages, as the receipt row names them
}

// platformPackage is a package platform.known-vuln counts, by its name and installed version, with each
// advisory at high or above published against it and the versions that fix it (contracts/routes.md, The
// promote receipt).
type platformPackage struct {
	Name       string             `json:"name"`
	Version    string             `json:"version"`
	Advisories []platformAdvisory `json:"advisories"`
	GoStdlib   bool               `json:"go_stdlib,omitempty"` // a contributor's: the image read's technical detail (#2579)
	Layers     []packageLayer     `json:"layers,omitempty"`    // a contributor's: where it is, and so what fixes it
}

// packageLayer is a layer holding a counted package, by its owner and the package's class there.
type packageLayer struct {
	Owner string `json:"owner"`
	Class string `json:"class"`
}

type platformAdvisory struct {
	ID    string `json:"id"`
	Fixed string `json:"fixed"`
}

type imageLayer struct {
	DiffID string  `json:"diff_id"`
	Owner  *string `json:"owner"`
}

type platformNote struct {
	Name    string  `json:"name"`
	Version string  `json:"version"`
	Layer   *string `json:"layer"`
	Class   string  `json:"class"`
	Why     string  `json:"why"` // no_fix | unknown | not_checked
}

type platformFinding struct {
	finding.Result
	Tool       string             `json:"tool"`
	Class      string             `json:"class"`
	Dependency platformDependency `json:"dependency"`
	goStdlib   bool               // its purl is Go's standard library: the receipt row's go_stdlib
}

type platformDependency struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Vulnerability string `json:"vulnerability"`
	Fixed         string `json:"fixed"`
	Link          string `json:"link,omitempty"`
	Layer         string `json:"layer"`
}

// notFinishedWords are an image scan's not_finished in words (contracts/routes.md, The promote receipt).
var notFinishedWords = map[string]string{
	"timeout": "did not finish in time", "killed": "stopped before it finished", "not_sarif": "did not answer an SBOM",
	"no_image": "this Canyon host has no image for it", "no_room": "the cluster had no room to run it",
	"db_missing": "its Java database could not be downloaded", "db_stale": "its Java database is over 5 days old",
	"busy": "Canyon's scanner was busy with other apps, and checks again on its next round", // Canyon's own (#264)
}

func scanWords(notFinished, reason string) string {
	switch {
	case notFinished == "over_cap" && reason == "image":
		return "the image is larger than this Canyon scans"
	case notFinished == "over_cap" && reason == "report":
		return "answered more than Canyon reads"
	}
	return cmp.Or(notFinishedWords[notFinished], "its image scan did not finish")
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// readImage is what platform.known-vuln reads of one image at now (contracts/finding.md, System libraries and build tools;
// contracts/routes.md, The promote receipt): pure, so the receipt and the image read are one derivation.
// state is the rule's, which decides whether a finding blocks.
func readImage(f imageFacts, env, digest string, state gatepolicy.State, name string, now time.Time) imageRead {
	out := imageRead{Env: env, Digest: digest, State: "not_scanned", Why: "no image scan recorded", ReadAt: timestamp(now),
		Layers: []imageLayer{}, Findings: []platformFinding{}, Notes: []platformNote{}}
	if f.scanned == nil {
		return out
	}
	scanned := timestamp(*f.scanned)
	out.ScannedAt, out.State = &scanned, "could_not_check"
	if f.notFinished != "" {
		out.Why = "could not check: " + scanWords(f.notFinished, f.reason)
		return out
	}
	owners, labelled := trivy.Owners(f.diffIDs, f.label)
	switch {
	case f.unreadable || !labelled || !trivy.DiffIDsRead(f.diffIDs):
		out.Why = "could not check: a stored scan of the image does not read"
		return out
	case slices.ContainsFunc(f.diffIDs, func(d string) bool { _, stored := f.layers[d]; return !stored }):
		out.Why = "could not check: Canyon kept only part of the image"
		return out
	case slices.ContainsFunc(f.diffIDs, func(d string) bool { return !trivy.StoredIn(f.layers[d], d) }):
		out.Why = "could not check: a stored scan of the image does not read"
		return out
	}
	for _, d := range f.diffIDs {
		out.Layers = append(out.Layers, imageLayer{DiffID: d, Owner: nonEmpty(owners[d])})
	}
	pkgs := trivy.Packages(f.diffIDs, owners, f.layers)
	covered := trivy.Covered(f.diffIDs, pkgs, false) // the layers a re-match covers: holding a package of a platform class
	if len(covered) == 0 {                           // no complete report says the image is clean: it says nothing of Canyon's
		out.Why = "could not check: the image scan listed no package Canyon put there"
		return out
	}
	why := ""
	var oldest *time.Time
	for _, d := range covered {
		m, ok := f.newest[d]
		switch {
		case !ok:
			why = cmp.Or(why, "could not check: the image has not been matched yet")
			continue
		case m.dbBuiltAt.After(now): // a database from the future is no date at all
			why = cmp.Or(why, "could not check: a stored scan of the image does not read")
		case now.Sub(m.dbBuiltAt) > platformStale:
			why = cmp.Or(why, "could not check: its vulnerability database is over 5 days old")
		}
		if oldest == nil || m.dbBuiltAt.Before(*oldest) {
			oldest = &m.dbBuiltAt
		}
	}
	if oldest != nil {
		built := timestamp(*oldest)
		out.DBBuiltAt = &built
	}
	if f.rescanFailed { // what it matched before is no answer for now (design/PATCHING.md §3.2, Stale or failed)
		why = cmp.Or(why, "could not check: its last rescan did not finish")
	}
	type noteKey struct{ name, version, layer, class, why string }
	notes := map[noteKey]bool{} // one note per package and why, however many of its vulnerabilities say so
	note := func(c trivy.Component, layer, class, why string) {
		notes[noteKey{c.FullName(), c.Version, layer, class, why}] = true
	}
	for _, d := range covered {
		if why != "" {
			break
		}
		m, mine := f.newest[d], map[string]trivy.Package{}
		for _, p := range pkgs {
			if p.Layer == d {
				mine[p.BOMRef] = p
			}
		}
		if !trivy.Joins(m.report, d, pkgs) {
			why = "could not check: a match names a package the image does not hold"
			break
		}
		for _, p := range mine {
			if p.Class != "app" && (p.PURL == "" || slices.Contains(m.unscanned, p.BOMRef)) {
				note(p.Component, d, p.Class, "not_checked")
			}
		}
		for _, r := range m.report.Results {
			for _, v := range r.Vulnerabilities {
				switch p := mine[v.PkgIdentifier.BOMRef]; {
				case p.Class == "app":
				case p.Class == "unknown":
					note(p.Component, d, p.Class, "unknown")
				case v.FixedVersion == "":
					note(p.Component, d, p.Class, "no_fix")
				default:
					out.Findings = append(out.Findings, platformFindingOf(v, p.Component, p.Class, d, owners[d], state, name))
				}
			}
		}
	}
	if why != "" {
		out.Why, out.Findings = why, []platformFinding{}
		return out
	}
	for _, c := range f.unplaced {
		if c.Type == "library" {
			note(c, "", "unknown", "not_checked")
		}
	}
	for n := range notes {
		out.Notes = append(out.Notes, platformNote{Name: n.name, Version: n.version, Layer: nonEmpty(n.layer), Class: n.class, Why: n.why})
	}
	if f.source == "" {
		return finishRead(out, f, owners, platformNotes)
	}
	if why := addImported(&out, f, owners, state, name, now); why != "" { // the source adds to Canyon's own scan, and can't check when its reports can't
		out.Why, out.Findings, out.Notes = why, []platformFinding{}, []platformNote{}
		return out
	}
	return finishRead(out, f, owners, importedNotes)
}

// finishRead orders a checked read's findings and notes and says what it counted, its notes in notesWhy's words.
func finishRead(out imageRead, f imageFacts, owners map[string]string, notesWhy string) imageRead {
	rank := map[finding.Severity]int{finding.Critical: 0, finding.High: 1, finding.Medium: 2, finding.Low: 3}
	slices.SortFunc(out.Findings, func(x, y platformFinding) int {
		return cmp.Or(cmp.Compare(rank[x.Properties.Canyon.Severity], rank[y.Properties.Canyon.Severity]), cmp.Compare(x.Dependency.Name, y.Dependency.Name),
			cmp.Compare(x.Dependency.Version, y.Dependency.Version), cmp.Compare(x.Dependency.Vulnerability, y.Dependency.Vulnerability))
	})
	position := func(layer *string) int {
		if layer == nil {
			return len(f.diffIDs)
		}
		return slices.Index(f.diffIDs, *layer)
	}
	slices.SortFunc(out.Notes, func(x, y platformNote) int {
		return cmp.Or(cmp.Compare(position(x.Layer), position(y.Layer)), cmp.Compare(x.Name, y.Name), cmp.Compare(x.Version, y.Version), cmp.Compare(x.Why, y.Why))
	})
	out.packages = counted(out.Findings, f.diffIDs, owners)
	out.State, out.counted = "checked", len(out.packages)
	out.Why = "no known-vulnerable package" + platformWhere
	if out.counted > 0 {
		out.Why = fmt.Sprintf("%d known-vulnerable package%s", out.counted, plural(out.counted)) + platformWhere
	}
	if len(out.Notes) > 0 {
		out.Why += fmt.Sprintf("; %d note%s: %s", len(out.Notes), plural(len(out.Notes)), notesWhy)
	}
	return out
}

// addImported adds the source's reports of the image to a checked read of Canyon's own matches (design/PATCHING.md §3.4;
// contracts/finding.md, An imported image result): the newest report of each of the tool's categories, never one over 5
// days old, unioned with what the matches found, each finding once by its package, version and vulnerability, so a
// report adds and never hides. A result counts when its package is in a layer Canyon places in its system libraries or
// build tools and the tool names a fixed version; one in a layer of no Canyon class is a note, unknown, and one whose
// layer the report does not name, or that this image does not have, a note, unplaced. A result in the app's own layers
// is deps.known-vuln's, neither. It answers why it could not check, "" when it added the reports; the read's age is
// then the oldest of its database and its reports.
func addImported(out *imageRead, f imageFacts, owners map[string]string, state gatepolicy.State, name string, now time.Time) string {
	if len(f.imported) == 0 {
		return "could not check: no " + f.source + " report is attached to this image yet"
	}
	oldest := f.imported[0].attachedAt
	for _, r := range f.imported {
		switch {
		case r.unreadable || r.attachedAt.After(now):
			return "could not check: a stored report of the image does not read"
		case now.Sub(r.attachedAt) > platformStale:
			days, which := int(now.Sub(r.attachedAt)/(24*time.Hour)), "" // the category to attach again, none for a report of none
			if r.category != "" {
				which = " for " + r.category
			}
			return fmt.Sprintf("could not check: its %s report%s is %d day%s old: attach a newer one", f.source, which, days, plural(days))
		}
		if r.attachedAt.Before(oldest) {
			oldest = r.attachedAt
		}
	}
	type findingKey struct{ name, version, vulnerability string }
	found := map[findingKey]bool{}
	for _, x := range out.Findings {
		found[findingKey{x.Dependency.Name, x.Dependency.Version, x.Dependency.Vulnerability}] = true
	}
	type noteKey struct{ name, version, layer, class, why string }
	noted := map[noteKey]bool{}
	for _, n := range out.Notes {
		noted[noteKey{n.Name, n.Version, deref(n.Layer), n.Class, n.Why}] = true
	}
	note := func(n platformNote) { // each package and why once, whichever source said it
		if k := (noteKey{n.Name, n.Version, deref(n.Layer), n.Class, n.Why}); !noted[k] {
			noted[k] = true
			out.Notes = append(out.Notes, n)
		}
	}
	for _, r := range f.imported {
		for _, res := range r.results {
			p := res.Package
			if p.Layer == "" || !slices.Contains(f.diffIDs, p.Layer) {
				note(platformNote{Name: p.Name, Version: p.Version, Class: "unknown", Why: "unplaced"})
				continue
			}
			owner := owners[p.Layer]
			switch class := trivy.Class(owner, "", p.PURL); {
			case class == "app":
			case class == "unknown":
				note(platformNote{Name: p.Name, Version: p.Version, Layer: nonEmpty(p.Layer), Class: class, Why: "unknown"})
			case p.Fixed == "":
				note(platformNote{Name: p.Name, Version: p.Version, Layer: nonEmpty(p.Layer), Class: class, Why: "no_fix"})
			case found[findingKey{p.Name, p.Version, res.Vulnerability}]:
			default:
				found[findingKey{p.Name, p.Version, res.Vulnerability}] = true
				out.Findings = append(out.Findings, platformFindingOf(trivy.Vuln{VulnerabilityID: res.Vulnerability, FixedVersion: p.Fixed, Severity: string(res.Severity)},
					trivy.Component{Name: p.Name, Version: p.Version, PURL: p.PURL}, class, p.Layer, owner, state, name))
			}
		}
	}
	if built, err := time.Parse(time.RFC3339, deref(out.DBBuiltAt)); err != nil || oldest.Before(built) { // the read is as old as its oldest source
		at := timestamp(oldest)
		out.DBBuiltAt = &at
	}
	return ""
}

// platformImage is a read as the patch plan and a patch run take it (design/PATCHING.md §4.2, §4.3): whether it
// checked, its why, and each finding it counts — Canyon's class, a fix published — whatever the rule's state; a
// read that did not check counts none.
func platformImage(read imageRead) runs.PlatformImage {
	out := runs.PlatformImage{Checked: read.State == "checked", Why: read.Why}
	if !out.Checked {
		return out
	}
	owners := map[string]string{} // whose layer holds each finding, which names what a rebuild fixes it with (runs' RebuildNewer)
	for _, l := range read.Layers {
		if l.Owner != nil {
			owners[l.DiffID] = *l.Owner
		}
	}
	for _, f := range read.Findings {
		out.Findings = append(out.Findings, runs.PlatformFinding{Name: f.Dependency.Name, Version: f.Dependency.Version, Vulnerability: f.Dependency.Vulnerability,
			Fixed: f.Dependency.Fixed, Class: f.Class, Severity: string(f.Properties.Canyon.Severity), Owner: owners[f.Dependency.Layer]})
	}
	return out
}

// counted is the packages findings count, each by its name and installed version, in the findings' order:
// most severe first. A package's advisories are those at high or above, each once, whatever layers hold it.
// counted is what the row counts: each package with a finding at high or above, each advisory once, and the layers
// holding those findings bottom first, each owner and class once (contracts/routes.md, The promote receipt).
func counted(findings []platformFinding, diffIDs []string, owners map[string]string) []platformPackage {
	type held struct{ layer, class string }
	var out []platformPackage
	var in [][]held
	for _, f := range findings {
		if s := f.Properties.Canyon.Severity; s != finding.Critical && s != finding.High {
			continue
		}
		i := slices.IndexFunc(out, func(p platformPackage) bool { return p.Name == f.Dependency.Name && p.Version == f.Dependency.Version })
		if i < 0 {
			i, out, in = len(out), append(out, platformPackage{Name: f.Dependency.Name, Version: f.Dependency.Version, GoStdlib: f.goStdlib}), append(in, nil)
		}
		out[i].GoStdlib = out[i].GoStdlib && f.goStdlib
		if !slices.ContainsFunc(out[i].Advisories, func(a platformAdvisory) bool { return a.ID == f.Dependency.Vulnerability }) {
			out[i].Advisories = append(out[i].Advisories, platformAdvisory{ID: f.Dependency.Vulnerability, Fixed: f.Dependency.Fixed})
		}
		if h := (held{f.Dependency.Layer, f.Class}); !slices.Contains(in[i], h) {
			in[i] = append(in[i], h)
		}
	}
	for i := range out {
		slices.SortStableFunc(in[i], func(x, y held) int {
			return cmp.Compare(slices.Index(diffIDs, x.layer), slices.Index(diffIDs, y.layer))
		})
		for _, h := range in[i] {
			if l := (packageLayer{owners[h.layer], h.class}); !slices.Contains(out[i].Layers, l) {
				out[i].Layers = append(out[i].Layers, l)
			}
		}
	}
	return out
}

// forResponder is the receipt a responder with no grant reads: whole, prod.egress-allowed naming no destination, each counted package as #2300 had it, its
// layers and go_stdlib the image read's technical detail, a contributor's (design/PERMISSIONS.md).
func (r receipt) forResponder() receipt {
	r.Rules = slices.Clone(r.Rules)
	for i := range r.Rules {
		packages := slices.Clone(r.Rules[i].Packages)
		for j := range packages {
			packages[j].GoStdlib, packages[j].Layers = false, nil
		}
		r.Rules[i].Packages = packages
		if r.Rules[i].noNames != "" {
			r.Rules[i].Why = r.Rules[i].noNames
		}
	}
	return r
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// fixIt follows the versions trivy's FixedVersion names: a list, joined by ", ", fixes in the plural.
func fixIt(fixed string) string {
	if strings.Contains(fixed, ", ") {
		return " fix it."
	}
	return " fixes it."
}

// platformFindingOf is one counted vulnerability as a profile result (contracts/finding.md, System libraries and build tools).
func platformFindingOf(v trivy.Vuln, c trivy.Component, class, layer, owner string, state gatepolicy.State, policyName string) platformFinding {
	sev := finding.Severity(strings.ToLower(v.Severity))
	level, err := sev.Level()
	if err != nil { // a severity Canyon cannot read is not low
		sev, level = finding.High, "error"
	}
	name, fixed := c.FullName(), v.FixedVersion
	snippet := name + " " + c.Version // the region spans it: an end column is exclusive
	uri := cmp.Or(c.Prop("aquasecurity:trivy:FilePath"), ".")
	if p, err := packageurl.FromString(c.PURL); err == nil && p.Type == packageurl.TypeDebian {
		uri = "var/lib/dpkg/status"
	}
	fix := "Canyon fixes this in its next build tools update, with a newer app launcher."
	switch class {
	case "os":
		fix = "Canyon fixes this: it updates " + name + " to " + fixed + " under the app. The app is not rebuilt."
	case "runtime":
		fix = "Canyon fixes this in its next build tools update, with a newer " + trivy.FixedBy(owner) + "."
	}
	blocks := state == gatepolicy.Blocking && (sev == finding.Critical || sev == finding.High)
	title := []rune(v.VulnerabilityID + " in " + name + " " + c.Version)
	title = title[:min(len(title), 120)]
	bag := finding.Canyon{Severity: sev, Blocks: &blocks, Title: string(title), PolicyName: policyName, Fix: fix,
		Problem: v.VulnerabilityID + " is published against " + name + " " + c.Version + ", which Canyon put in this app's " + platformPlace[class] + ". " + fixed + fixIt(fixed)}
	if blocks {
		bag.RequiredEndState = "The image preview runs holds " + name + " at a version that fixes " + v.VulnerabilityID + "."
		bag.DoNot = []string{"change the app's code or dependencies for it"}
	}
	fp := imageFingerprint(platformRule, v.VulnerabilityID, name, c.Version, class)
	dep := platformDependency{Name: name, Version: c.Version, Vulnerability: v.VulnerabilityID, Fixed: fixed, Layer: layer}
	if trivy.HTTPSLink(v.PrimaryURL) {
		dep.Link = v.PrimaryURL
	}
	return platformFinding{
		Result: finding.Result{RuleID: platformRule, Level: level, Message: finding.Message{Text: string(title)},
			Locations: []finding.Location{{PhysicalLocation: finding.PhysicalLocation{
				ArtifactLocation: finding.ArtifactLocation{URI: uri, URIBaseID: "IMAGE"},
				Region:           finding.Region{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1 + utf8.RuneCountInString(snippet), Snippet: finding.Snippet{Text: snippet}}}}},
			PartialFingerprints: finding.Fingerprints{CanyonV1: fp}, Properties: finding.Properties{Canyon: bag}},
		Tool: "canyon/promote-gate", Class: class, Dependency: dep, goStdlib: trivy.GoStdlib(c.PURL),
	}
}

// imageFingerprint is the canyon/v1 of a vulnerability of a package in an image, under rule (contracts/finding.md,
// Canyon's layers and The built app, Fingerprint): no path, layer or digest goes into it, so an approval covers that
// vulnerability in that version of that package across rebuilds.
func imageFingerprint(rule, vulnerability, name, version, class string) string {
	frame := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	return fmt.Sprintf("f_%x", sha256.Sum256([]byte(frame(rule)+frame(vulnerability)+frame(name)+frame(version)+frame(class))))
}

// platformRow is platform.known-vuln's receipt row from its read (contracts/routes.md, The promote receipt):
// it passes only on a read that checked and counted nothing, and carries what it read once it read matches,
// the packages it counted among it.
func platformRow(r gatepolicy.Rule, read imageRead) receiptRule {
	row := receiptRule{ID: r.ID, Name: r.Name, Blocks: r.State == gatepolicy.Blocking, Passed: read.State == "checked" && read.counted == 0, Why: read.Why}
	if read.State == "checked" && read.DBBuiltAt != nil {
		notes := len(read.Notes)
		row.DBBuiltAt, row.ReadAt, row.Notes, row.Packages = read.DBBuiltAt, &read.ReadAt, &notes, read.packages
	}
	return row
}

// loadImage reads what the store holds of digest for project: the project's own image scan, each of its
// layers' components as that scan listed them, and the newest match of each layer's slice by its database's
// build (contracts/finding.md, System libraries and build tools: stored by content, read as the project's own). A stored row that does not decode is unreadable, never
// an error that leaves a receipt to read it as empty; a failed read is the caller's error, and no receipt
// is answered on one.
func loadImage(ctx context.Context, q querier, project, digest string) (imageFacts, error) {
	var f imageFacts
	var scanned time.Time
	var notFinished, reason, label *string
	var unplaced []byte
	err := q.QueryRow(ctx, "SELECT scanned_at, diff_ids, lifecycle, unplaced, not_finished, reason, rescan_failed_at IS NOT NULL, app_rescan_failed_at IS NOT NULL FROM scanned_images WHERE project_id = $1 AND digest = $2", project, digest).
		Scan(&scanned, &f.diffIDs, &label, &unplaced, &notFinished, &reason, &f.rescanFailed, &f.appRescanFailed)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.scanned = &scanned
	if notFinished != nil {
		f.notFinished, f.reason = *notFinished, deref(reason)
		return f, nil
	}
	f.label = deref(label)
	var ok bool
	f.unplaced, ok = trivy.DecodeComponents(unplaced)
	f.unreadable = !ok
	f.layers, f.newest = map[string][]trivy.Component{}, map[string]layerMatch{}
	// each layer as this image's own scan listed it, never another image's view of the same diff ID
	rows, err := q.Query(ctx, `SELECT l.diff_id, s.components FROM image_layers l JOIN layer_parts s USING (part_sha)
		WHERE l.project_id = $1 AND l.digest = $2 AND l.diff_id = ANY($3)`, project, digest, f.diffIDs)
	if err != nil {
		return f, err
	}
	var d string
	var raw []byte
	if _, err := pgx.ForEachRow(rows, []any{&d, &raw}, func() error {
		components, ok := trivy.DecodeComponents(raw) // a row that does not read is never a layer with nothing in it
		f.unreadable = f.unreadable || !ok
		f.layers[d] = components
		return nil
	}); err != nil {
		return f, err
	}
	rows, err = q.Query(ctx, `SELECT DISTINCT ON (l.diff_id) l.diff_id, m.db_built_at, m.report, m.unscanned FROM image_layers l
		JOIN part_matches m USING (part_sha) WHERE l.project_id = $1 AND l.digest = $2 AND l.diff_id = ANY($3) ORDER BY l.diff_id, m.db_built_at DESC`, project, digest, f.diffIDs)
	if err != nil {
		return f, err
	}
	var m layerMatch
	_, err = pgx.ForEachRow(rows, []any{&d, &m.dbBuiltAt, &raw, &m.unscanned}, func() error {
		report, ok := trivy.DecodeReport(raw)
		f.unreadable = f.unreadable || !ok
		m.report = report
		f.newest[d], m = m, layerMatch{}
		return nil
	})
	return f, err
}

// loadImported is loadImage for a rule whose settings.source names an imported tool: f reads also that tool's newest
// report of the image in each category, by when it was attached (design/PATCHING.md §3.4). "" reads trivy's own alone.
func loadImported(ctx context.Context, q querier, project, digest, source string) (imageFacts, error) {
	f, err := loadImage(ctx, q, project, digest)
	if err != nil || source == "" {
		return f, err
	}
	f.source = source
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (category) category, attached_at, results FROM image_reports WHERE project_id = $1 AND digest = $2 AND tool = $3
		ORDER BY category, attached_at DESC`, project, digest, source)
	if err != nil {
		return f, err
	}
	var r importedReport
	var raw []byte
	_, err = pgx.ForEachRow(rows, []any{&r.category, &r.attachedAt, &raw}, func() error {
		r.unreadable = json.Unmarshal(raw, &r.results) != nil // a row that does not read is never a report with nothing in it
		f.imported, r = append(f.imported, r), importedReport{}
		return nil
	})
	return f, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// platformOf is the rule's read of the image a receipt is for, nil while the rule is off: then it is not on
// the receipt and nothing is read.
func platformOf(ctx context.Context, q querier, snap gatepolicy.Snapshot, project, image string) (*imageRead, error) {
	i := slices.IndexFunc(snap.Rules, func(r gatepolicy.Rule) bool { return r.ID == platformRule })
	switch {
	case i < 0: // the boot snapshot refuses to load without it, so a snapshot that lacks it is no snapshot to read
		return nil, errors.New("the snapshot carries no " + platformRule)
	case snap.Rules[i].State == gatepolicy.Off:
		return nil, nil
	}
	digest := runs.ImageDigest(image)
	f, err := loadImported(ctx, q, project, digest, snap.Rules[i].Settings.Source)
	if err != nil {
		return nil, err
	}
	read := readImage(f, "preview", digest, snap.Rules[i].State, snap.Rules[i].Name, time.Now())
	return &read, nil
}

// imageRead answers what the image env runs holds of Canyon's, whatever the rule's state (contracts/routes.md,
// GET /api/v1/projects/:id/envs/:env/image): teamOnly has answered a caller below contributor.
func (a *app) imageRead(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("id"), r.PathValue("env")
	if env != "preview" && env != "prod" {
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return
	}
	internal := func(err error) {
		a.log.Printf("canyon-server: image read %s/%s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	var image string
	var reached time.Time
	err := a.pool.QueryRow(ctx, "SELECT image, deployed_at FROM "+runs.ShownDeploys+" d WHERE d.project_id = $1 AND d.env = $2", id, env).Scan(&image, &reached)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "nothing is deployed to "+env, nil)
		return
	}
	if err != nil {
		internal(err)
		return
	}
	snap, err := a.projectPolicy(ctx, a.pool, id)
	if err != nil {
		internal(err)
		return
	}
	i := slices.IndexFunc(snap.Rules, func(r gatepolicy.Rule) bool { return r.ID == platformRule })
	if i < 0 {
		internal(errors.New("the snapshot carries no " + platformRule))
		return
	}
	rule := snap.Rules[i]
	digest := runs.ImageDigest(image)
	f, err := loadImported(ctx, a.pool, id, digest, rule.Settings.Source)
	if err != nil {
		internal(err)
		return
	}
	patched, err := patchedIn(ctx, a.pool, id, digest, reached)
	if err != nil {
		internal(err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		imageRead
		Patched []patchedFix `json:"patched"`
	}{readImage(f, env, digest, rule.State, rule.Name, time.Now()), patched})
}

// patchedFix is one of the image read's patched[] (contracts/routes.md; patch.schema.json's patched).
type patchedFix struct {
	Name          string `json:"name"`
	From          string `json:"from"`
	To            string `json:"to"`
	Vulnerability string `json:"vulnerability"`
	Class         string `json:"class"`
	Run           string `json:"run"`
	At            string `json:"at"`
}

// patchedIn is what Canyon's patches fixed in project's image digest, found by the digest (contracts/routes.md, the
// image read's patched[]; the decision of #2556): the newest patch preview row naming it as its image, then, while that
// patch's replaces was a patch's image, the one before it, each older than the last; newest first, at when the image
// last reached the environment, each its class as the run recorded it: a rebuild records each, a rebase none, as it
// fixes os packages alone (runs, plan). [] on an image no patch made.
func patchedIn(ctx context.Context, q querier, project, digest string, reached time.Time) ([]patchedFix, error) {
	out := []patchedFix{}
	var before *time.Time // the last patch's row: each before it is older
	for digest != "" {
		var run, replaces string
		var fixed []struct{ Name, From, To, Vulnerability, Class string }
		var at time.Time
		err := q.QueryRow(ctx, `SELECT e.run_id, coalesce(e.detail->>'replaces', ''), e.detail->'fixed', e.ts FROM audit_events e
			WHERE e.project_id = $1 AND e.action = 'patch preview' AND e.detail->>'image' = $2 AND ($3::timestamptz IS NULL OR e.ts < $3) ORDER BY e.ts DESC, e.id DESC LIMIT 1`,
			project, digest, before).Scan(&run, &replaces, &fixed, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, f := range fixed {
			out = append(out, patchedFix{Name: f.Name, From: f.From, To: f.To, Vulnerability: f.Vulnerability, Class: cmp.Or(f.Class, "os"), Run: run, At: timestamp(reached)})
		}
		digest, before = replaces, &at
	}
	return out, nil
}
