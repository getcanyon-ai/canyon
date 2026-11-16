package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/sarif"
)

// report is one category's newest report of wiz's, attached ago before the read, of results.
type report struct {
	ago      time.Duration
	results  []sarif.ImageResult
	category string
}

// imported is the golden image read with wiz as the rule's added source, its reports one per category.
func imported(t *testing.T, g platformGolden, reports ...report) imageFacts {
	t.Helper()
	f := g.facts(t)
	f.source = "wiz"
	for _, r := range reports {
		f.imported = append(f.imported, importedReport{category: r.category, attachedAt: g.ReadAt.Add(-r.ago), results: r.results})
	}
	return f
}

// wizResult is one result of wiz's report: the vulnerability, its package, fixed version, layer and purl.
func wizResult(vuln, name, version, fixed, layer, purl string) sarif.ImageResult {
	return sarif.ImageResult{Vulnerability: vuln, Severity: finding.High, Package: sarif.ImagePackage{Name: name, Version: version, Fixed: fixed, Layer: layer, PURL: purl}}
}

const (
	osLayer      = "sha256:2222222222222222222222222222222222222222222222222222222222222222" // the golden's run image's top layer
	acmeLayer    = "sha256:5555555555555555555555555555555555555555555555555555555555555555" // acme/certificates', a buildpack outside Canyon's builder
	installLayer = "sha256:4444444444444444444444444444444444444444444444444444444444444444" // npm-install's: the app's own dependencies
	expatPURL    = "pkg:deb/ubuntu/libexpat1@2.6.1-2build1?arch=amd64&distro=ubuntu-24.04"
	libsslPURL   = "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04"
)

// vulns is a read's findings by package and vulnerability, sorted.
func vulns(r imageRead) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Dependency.Name+" "+f.Dependency.Vulnerability)
	}
	slices.Sort(out)
	return out
}

// An added source adds to Canyon's own matches (design/PATCHING.md §3.4): a result counts when the tool names a fixed
// version and its package is in a layer Canyon places in its system libraries or build tools, beside every finding the
// matches made, a finding both name once; one with no fixed version is a note, never a count, never clean.
func TestImportedResultWithoutFixIsNote(t *testing.T) {
	g := loadPlatformGolden(t)
	own := g.read(t, g.facts(t), g.ReadAt)
	read := g.read(t, imported(t, g, report{time.Hour, []sarif.ImageResult{
		wizResult("CVE-2024-45491", "libexpat1", "2.6.1-2build1", "2.6.1-2ubuntu0.1", osLayer, expatPURL),
		wizResult("CVE-2024-45492", "libexpat1", "2.6.1-2build1", "", osLayer, expatPURL),
		wizResult("CVE-2024-6119", "libssl3", "3.0.13-0ubuntu3.4", "3.0.13-0ubuntu3.5", osLayer, libsslPURL)}, ""}), g.ReadAt) // the matches found it too
	if read.State != "checked" || read.counted != own.counted+1 || !slices.Equal(vulns(read), slices.Sorted(slices.Values(append(vulns(own), "libexpat1 CVE-2024-45491")))) {
		t.Fatalf("the read: %s, %d counted, findings %v; the matches' alone %d, %v", read.State, read.counted, vulns(read), own.counted, vulns(own))
	}
	if len(read.Notes) != len(own.Notes)+1 || !slices.ContainsFunc(read.Notes, func(n platformNote) bool { return n.Name == "libexpat1" && n.Why == "no_fix" }) {
		t.Fatalf("its notes: %+v", read.Notes)
	}
	if !strings.HasSuffix(read.Why, "; 5 notes: "+importedNotes) || read.DBBuiltAt == nil || *read.DBBuiltAt != *own.DBBuiltAt {
		t.Fatalf("its why %q and age %v", read.Why, read.DBBuiltAt)
	}
}

// A result Canyon can't place — no layer, or one this image has not — is a note that Canyon can't tell which part of the
// image it is in; one in another buildpack's layer a note of what put it there; one in the app's own dependencies is
// neither, deps.known-vuln's. None of them takes anything from what the matches found.
func TestAnImportedResultCanyonCannotPlaceIsANote(t *testing.T) {
	g := loadPlatformGolden(t)
	own := g.read(t, g.facts(t), g.ReadAt)
	read := g.read(t, imported(t, g, report{time.Hour, []sarif.ImageResult{
		wizResult("CVE-2024-1", "zlib1g", "1.3", "1.3.1", "", "pkg:deb/ubuntu/zlib1g@1.3"),
		wizResult("CVE-2024-2", "busybox", "1.36", "1.36.1", "sha256:"+strings.Repeat("9", 64), ""),
		wizResult("CVE-2024-3", "certifi", "2023.7.22", "2024.2.2", acmeLayer, "pkg:pypi/certifi@2023.7.22"),
		wizResult("CVE-2024-4", "express", "4.19.2", "4.21.0", installLayer, "pkg:npm/express@4.19.2")}, ""}), g.ReadAt)
	whys := map[string]string{}
	for _, n := range read.Notes {
		whys[n.Name] = n.Why
	}
	if read.State != "checked" || !slices.Equal(vulns(read), vulns(own)) || len(read.Notes) != len(own.Notes)+2 ||
		whys["zlib1g"] != "unplaced" || whys["busybox"] != "unplaced" || whys["certifi"] != "unknown" || whys["express"] != "" {
		t.Fatalf("the read: %s, findings %v, notes %+v", read.State, vulns(read), read.Notes)
	}
}

// HIGH 2 of #2630's review: a report that finds nothing — an empty one an agent attached among them — never hides what
// Canyon's own matches found. HIGH 1: nor does one whose every result names another image's layers. Each read counts the
// matches' findings still (design/PATCHING.md §3.4: an added source, Canyon's own scan always counts).
func TestAnImportedReportNeverHidesWhatCanyonFound(t *testing.T) {
	g := loadPlatformGolden(t)
	own := g.read(t, g.facts(t), g.ReadAt)
	elsewhere := "sha256:" + strings.Repeat("a", 64)
	for name, f := range map[string]imageFacts{
		"an empty report":         imported(t, g, report{time.Hour, nil, ""}),
		"another image's results": imported(t, g, report{time.Hour, []sarif.ImageResult{wizResult("CVE-2024-6119", "libssl3", "3.0.13-0ubuntu3.5", "3.0.13-0ubuntu3.6", elsewhere, libsslPURL)}, ""}),
	} {
		if read := g.read(t, f, g.ReadAt); read.State != "checked" || read.counted != own.counted || own.counted == 0 || !slices.Equal(vulns(read), vulns(own)) {
			t.Errorf("%s: %s, %d counted (%v), the matches' %d", name, read.State, read.counted, vulns(read), own.counted)
		}
	}
}

// M3 of #2630's review: the newest report of each category is read, never only the newest of all, so an empty category
// attached later hides no other category's critical.
func TestEveryCategoryOfTheSourceIsRead(t *testing.T) {
	g := loadPlatformGolden(t)
	critical := wizResult("CVE-2024-45491", "libexpat1", "2.6.1-2build1", "2.6.1-2ubuntu0.1", osLayer, expatPURL)
	critical.Severity = finding.Critical
	read := g.read(t, imported(t, g, report{time.Minute, nil, ""}, report{time.Hour, []sarif.ImageResult{critical}, ""}), g.ReadAt)
	if !slices.Contains(vulns(read), "libexpat1 CVE-2024-45491") {
		t.Fatalf("an empty category hid another's critical: %v", vulns(read))
	}
}

// Imported reports fail closed: none of the image could not check, nor any category's over 5 days old, clean or not —
// a days-old export never passes a version; and Canyon's own matches that can't check can't, whatever a report says.
func TestAnImportedReportFailsClosed(t *testing.T) {
	g := loadPlatformGolden(t)
	none := g.facts(t)
	none.source = "wiz"
	stale := g.facts(t)
	stale.newest = map[string]layerMatch{}
	stale.source, stale.imported = "wiz", []importedReport{{attachedAt: g.ReadAt.Add(-time.Hour)}}
	for name, c := range map[string]struct {
		f   imageFacts
		why string
	}{
		"no report":                          {none, "could not check: no wiz report is attached to this image yet"},
		"a 6-day-old clean one":              {imported(t, g, report{6*24*time.Hour + time.Minute, nil, ""}), "could not check: its wiz report is 6 days old: attach a newer one"},
		"one category fresh, another stale":  {imported(t, g, report{time.Hour, nil, ""}, report{6*24*time.Hour + time.Minute, nil, "containers"}), "could not check: its wiz report for containers is 6 days old: attach a newer one"},
		"one from the future":                {imported(t, g, report{-time.Hour, nil, ""}), "could not check: a stored report of the image does not read"},
		"the matches not there, a report is": {stale, "could not check: the image has not been matched yet"},
	} {
		if read := g.read(t, c.f, g.ReadAt); read.State != "could_not_check" || read.Why != c.why || len(read.Findings) != 0 {
			t.Errorf("%s: %s %q", name, read.State, read.Why)
		}
	}
	if read := g.read(t, imported(t, g, report{5*24*time.Hour - time.Minute, nil, ""}), g.ReadAt); read.State != "checked" {
		t.Errorf("a report under 5 days old: %s %q", read.State, read.Why)
	}
}
