package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// builtAppGolden is contracts/testdata/platform/built-app.json: claims-portal's image with a postinstall's
// download and a Maven plugin's jar in the app's own layer, a package of a buildpack outside Canyon's builder,
// their re-matches, the run's deps.known-vuln findings, and the row they derive.
type builtAppGolden struct {
	platformGolden
	Version   string           `json:"version"`
	ReviewWhy string           `json:"review_why"`
	Reported  []finding.Result `json:"reported"`
	Row       struct {
		Passed bool   `json:"passed"`
		Why    string `json:"why"`
	} `json:"row"`
}

func loadBuiltAppGolden(t *testing.T) builtAppGolden {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/platform/built-app.json")
	if err != nil {
		t.Fatal(err)
	}
	var g builtAppGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g builtAppGolden) reported() map[trivy.PackageKey]bool {
	return reportedIn(finding.Log{Runs: []finding.Run{{Results: g.Reported}}})
}

func (b builtApp) names() []string {
	var out []string
	for _, p := range b.packages {
		out = append(out, p.name+" "+p.version)
	}
	return out
}

// The golden's row is what the server derives from its image, re-matches and the review's findings
// (contracts/finding.md, The built app): the app's own download and jar and the other buildpack's package
// counted, the most severe first, and the package the review reported from package-lock.json not again.
func TestDepsKnownVulnReadsTheBuiltAppGolden(t *testing.T) {
	g := loadBuiltAppGolden(t)
	app := readBuiltApp(g.facts(t), g.reported(), g.ReadAt)
	if passed, why := app.row(g.Version, g.ReviewWhy); passed != g.Row.Passed || why != g.Row.Why {
		t.Fatalf("the row: %v %q, want %v %q", passed, why, g.Row.Passed, g.Row.Why)
	}
	if want := []string{"urllib3 1.26.5", "lodash 4.17.20", "org.yaml/snakeyaml 1.33"}; !slices.Equal(app.names(), want) {
		t.Fatalf("counted %v, want %v", app.names(), want)
	}
	if app.unchecked != 3 {
		t.Fatalf("not checked: %d, want the purl-less download, the other buildpack's unscanned package and the one trivy named no layer for", app.unchecked)
	}
}

// A package of class app that no review finding names is counted; one the review reported — the same package
// type, name and version — is not counted twice; a package of class unknown, a buildpack's outside Canyon's
// builder, is counted as the team's.
func TestDepsKnownVulnCountsWhatOnlyTheBuiltAppHolds(t *testing.T) {
	g := loadBuiltAppGolden(t)
	f := g.facts(t)
	has := func(app builtApp, name string) bool { return slices.Contains(app.names(), name) }

	app := readBuiltApp(f, g.reported(), g.ReadAt)
	if !has(app, "lodash 4.17.20") || !has(app, "org.yaml/snakeyaml 1.33") {
		t.Errorf("an app package no lockfile names is not counted: %v", app.names())
	}
	if has(app, "express 4.19.2") {
		t.Errorf("a package the review reported is counted twice: %v", app.names())
	}
	if !has(readBuiltApp(f, nil, g.ReadAt), "express 4.19.2") {
		t.Error("express is counted only once its review stops naming it")
	}
	if !has(app, "urllib3 1.26.5") {
		t.Errorf("a package of a buildpack outside Canyon's builder is not counted: %v", app.names())
	}
	// the review names a package by its file's type alone: urllib3 from requirements.txt in another spelling is
	// the same package, and from a file that names no type stands for none of the image's
	for file, counted := range map[string]bool{"requirements.txt": false, "services/api/requirements.txt": false, "notes.txt": true} {
		r := g.Reported[0]
		r.Locations = []finding.Location{{PhysicalLocation: finding.PhysicalLocation{ArtifactLocation: finding.ArtifactLocation{URI: file}}}}
		r.Properties.Canyon.Problem = "Package: URLlib3\nInstalled Version: 1.26.5\nVulnerability CVE-2023-43804"
		reported := reportedIn(finding.Log{Runs: []finding.Run{{Results: []finding.Result{r}}}})
		if got := has(readBuiltApp(f, reported, g.ReadAt), "urllib3 1.26.5"); got != counted {
			t.Errorf("urllib3 reported from %s: counted %v, want %v", file, got, counted)
		}
	}
	// a finding of another rule names nothing the built app compares
	r := g.Reported[0]
	r.RuleID = "sarif.trivy.CVE-2024-43796"
	if !has(readBuiltApp(f, reportedIn(finding.Log{Runs: []finding.Run{{Results: []finding.Result{r}}}}), g.ReadAt), "express 4.19.2") {
		t.Error("a finding of another rule kept express from being counted")
	}
}

// An image that could not be checked never reads as passing: no scan, the app's own layer not matched yet,
// its match over 5 days old, and a match naming a package the layer does not hold each say so and fail.
func TestDepsKnownVulnBuiltAppFailsClosed(t *testing.T) {
	g := loadBuiltAppGolden(t)
	appLayer := "sha256:" + strings.Repeat("6", 64)
	for _, c := range []struct {
		name string
		f    func(imageFacts) imageFacts
		at   time.Time
		why  string
	}{
		{"no image scan", func(imageFacts) imageFacts { return imageFacts{} }, g.ReadAt, "no image scan recorded"},
		{"the scan did not finish", func(f imageFacts) imageFacts { f.notFinished = "timeout"; return f }, g.ReadAt, "did not finish in time"},
		{"the app's layer not matched", func(f imageFacts) imageFacts { delete(f.newest, appLayer); return f }, g.ReadAt, "the image has not been matched yet"},
		{"its match six days old", func(f imageFacts) imageFacts {
			m := f.newest[appLayer]
			m.dbBuiltAt = g.ReadAt.Add(-6 * 24 * time.Hour)
			f.newest[appLayer] = m
			return f
		}, g.ReadAt, "its vulnerability database is over 5 days old"},
		{"a match naming a package the layer does not hold", func(f imageFacts) imageFacts {
			m := f.newest[appLayer]
			m.report.Results = append(slices.Clone(m.report.Results), trivy.Result{Vulnerabilities: []trivy.Vuln{func() trivy.Vuln {
				v := trivy.Vuln{VulnerabilityID: "CVE-1", Severity: "HIGH"}
				v.PkgIdentifier.BOMRef, v.Layer.DiffID = "pkg:npm/elsewhere@1.0.0", appLayer
				return v
			}()}})
			f.newest[appLayer] = m
			return f
		}, g.ReadAt, "a match names a package the image does not hold"},
		{"a rescan that did not finish", func(f imageFacts) imageFacts { f.rescanFailed = true; return f }, g.ReadAt, "its last rescan did not finish"},
		{"a rescan of its app layers that did not finish", func(f imageFacts) imageFacts { f.appRescanFailed = true; return f }, g.ReadAt, "its last rescan did not finish"},
	} {
		app := readBuiltApp(c.f(g.facts(t)), g.reported(), c.at)
		passed, why := app.row("v1.4", "v1.4 passed review: no blocking finding")
		if passed || app.clean() || why != "v1.4's built app could not be checked: "+c.why {
			t.Errorf("%s: %v %q", c.name, passed, why)
		}
	}
}

// The row's words (contracts/routes.md, The promote receipt): one package names its fix, or that none is
// published, or how many advisories; a clean built app says so after the review's line, with what it could not
// check; and every line is cut to 200 code points.
func TestDepsKnownVulnBuiltAppRowSaysWhat(t *testing.T) {
	one := func(advisories ...platformAdvisory) builtApp {
		return builtApp{packages: []appPackage{{name: "lodash", version: "4.17.20", advisories: advisories}}}
	}
	for _, c := range []struct {
		app    builtApp
		passed bool
		why    string
	}{
		{one(platformAdvisory{ID: "CVE-2021-23337", Fixed: "4.17.21"}), false, "lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21 fixes it"},
		{one(platformAdvisory{ID: "CVE-2021-23337", Fixed: "4.17.21, 5.0.0"}), false, "lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21, 5.0.0 fix it"},
		{one(platformAdvisory{ID: "CVE-2021-23337"}), false, "lodash 4.17.20 is in v1.4's built app, but its review didn't see it: no fix is published for CVE-2021-23337"},
		{one(platformAdvisory{ID: "CVE-1", Fixed: "1"}, platformAdvisory{ID: "CVE-2"}), false, "lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 2 advisories are published against it"},
		{builtApp{}, true, "v1.4 passed review: no blocking finding; its built app holds no known-vulnerable package its review didn't see"},
		{builtApp{unchecked: 2}, true, "v1.4 passed review: no blocking finding; its built app holds no known-vulnerable package its review didn't see; Canyon couldn't check 2 packages in it"},
	} {
		if passed, why := c.app.row("v1.4", "v1.4 passed review: no blocking finding"); passed != c.passed || why != c.why {
			t.Errorf("got %v %q\nwant %v %q", passed, why, c.passed, c.why)
		}
	}
	long := builtApp{packages: []appPackage{{name: strings.Repeat("a", 300), version: "1", advisories: []platformAdvisory{{ID: "CVE-1"}}}}}
	if _, why := long.row("v1.4", ""); len([]rune(why)) != 200 {
		t.Errorf("a long line is %d code points", len([]rune(why)))
	}
}

// deps.known-vuln placed blocking before production holds the promotion on its built app (contracts/routes.md,
// The promote receipt): a counted package fails its row and prod.clean-gate, as an unread built app does —
// the receipt never passes on an image it could not check — and placed advisory, its row fails and blocks
// nothing. A row whose review fails says the review's reason, never the built app's.
func TestDepsKnownVulnBuiltAppOnTheReceipt(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	deps := gatepolicy.Rule{ID: depsRule, Name: "No known-vulnerable dependencies", State: gatepolicy.Advisory, Production: gatepolicy.Blocking}
	advisory := deps
	advisory.Production = gatepolicy.Advisory
	counted := &builtApp{packages: []appPackage{{name: "lodash", version: "4.17.20", advisories: []platformAdvisory{{ID: "CVE-2021-23337", Fixed: "4.17.21"}}}}}
	for _, c := range []struct {
		name    string
		p       placedRule
		passed  bool
		why     string
		blocked bool
	}{
		{"a counted package", placedRule{rule: deps, journaled: true, app: counted}, false,
			"lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21 fixes it", true},
		{"no image read", placedRule{rule: deps, journaled: true}, false, "v1.4's built app could not be checked: no image scan recorded", true},
		{"not checked", placedRule{rule: deps, journaled: true, app: &builtApp{why: "its vulnerability database is over 5 days old"}}, false,
			"v1.4's built app could not be checked: its vulnerability database is over 5 days old", true},
		{"clean", placedRule{rule: deps, journaled: true, app: &builtApp{}}, true,
			"v1.4 passed review: no blocking finding; its built app holds no known-vulnerable package its review didn't see", false},
		{"advisory", placedRule{rule: advisory, journaled: true, app: counted}, false,
			"lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21 fixes it", false},
		{"the review's own count first", placedRule{rule: deps, journaled: true, blocking: 1, unapproved: 1, app: counted}, false,
			"v1.4's review found 1 blocking finding, not approved", true},
	} {
		in := receiptInput{org: "Meridian", rules: promoteRules(testPolicy()), preview: &v14, owners: []actor{dana}, actor: dana,
			run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_8f2a"}, placed: []placedRule{c.p}}
		r := evaluate(in)
		row := r.Rules[len(r.Rules)-1]
		if row.ID != depsRule || row.Passed != c.passed || row.Why != c.why || slices.Contains(r.BlockedBy, depsRule) != c.blocked {
			t.Errorf("%s: %s %v", c.name, mustJSON(row), r.BlockedBy)
		}
		gate := r.Rules[slices.IndexFunc(r.Rules, func(x receiptRule) bool { return x.ID == "prod.clean-gate" })]
		if gate.Passed == c.blocked { // clean-gate never reads passed above a placed row that holds the promotion
			t.Errorf("%s: prod.clean-gate %s", c.name, mustJSON(gate))
		}
	}
}

// deps.known-vuln placed blocking before production reads the image preview runs on the status read's receipt:
// with no scan stored it could not check and holds the promotion; with the golden's scan it counts what only the
// built app holds — the run reported nothing, so the installer's package too — a promotion is refused on it, and
// the policies preview names the app.
func TestDepsKnownVulnBuiltAppOnTheStatusReceipt(t *testing.T) {
	pool, ctx, base, c, run := previewed(t)
	row := func() (map[string]any, []any) {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		receipt, _ := status["receipt"].(map[string]any)
		rules, _ := receipt["rules"].([]any)
		blocked, _ := receipt["blocked_by"].([]any)
		for _, r := range rules {
			if r, _ := r.(map[string]any); r["id"] == depsRule {
				return r, blocked
			}
		}
		t.Fatalf("no %s row on the receipt: %s", depsRule, lastBody)
		return nil, nil
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule+"?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("placing the rule before production: %d %s", code, lastBody)
	}
	builtAppAsked(t) // the apply's read ends on the fake's no_image: dropped, so the row reads what no scan reads
	if _, err := pool.Exec(ctx, "DELETE FROM scanned_images WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if r, blocked := row(); r["passed"] != false || r["blocks"] != true || !strings.HasSuffix(r["why"].(string), "'s built app could not be checked: no image scan recorded") ||
		!slices.Contains(blocked, any(depsRule)) || !slices.Contains(blocked, any("prod.clean-gate")) {
		t.Fatalf("no scan stored: %s %v", mustJSON(r), blocked)
	}
	seedPlatform(t, ctx, pool, loadBuiltAppGolden(t).platformGolden, "claims-portal", executortest.Digest)
	r, blocked := row()
	if why, _ := r["why"].(string); r["passed"] != false || !strings.HasPrefix(why, "4 known-vulnerable packages are in ") ||
		!strings.HasSuffix(why, "'s built app, but its review didn't see them: urllib3 1.26.5 and 3 more") || !slices.Contains(blocked, any(depsRule)) {
		t.Fatalf("the golden's scan: %s %v", mustJSON(r), blocked)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`); code != 409 || !slices.Contains(body["receipt"].(map[string]any)["blocked_by"].([]any), any(depsRule)) {
		t.Fatalf("the promotion: %d %s", code, lastBody)
	}
	// the policies preview names the app its row would hold, at the packages it counts (contracts/routes.md, the policies row)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+depsRule, `{"production":"blocking"}`); code != 200 ||
		!strings.Contains(lastBody, `"would_block":[{"project":"claims-portal","findings":4}]`) {
		t.Fatalf("the policies preview: %d %s", code, lastBody)
	}
}

// approvedRead is the golden's built app with every fingerprint of each package covered approves approved.
func approvedRead(t *testing.T, g builtAppGolden, covered func(appPackage) bool) builtApp {
	t.Helper()
	app := readBuiltApp(g.facts(t), g.reported(), g.ReadAt)
	set := map[string]bool{}
	for _, p := range app.packages {
		if covered(p) {
			for _, fp := range p.fingerprints {
				set[fp] = true
			}
		}
	}
	app.approve(set)
	return app
}

// An approval covers a package only the built app holds as it covers a finding (#2610): every package approved,
// deps.known-vuln's row passes and names what was accepted, the first and how many more (EXPERIENCE D5); one
// approved, the row counts the others; and the row lists each, counted first, with the fingerprints an approval names.
func TestDepsImageApprovedPackagesPassTheRow(t *testing.T) {
	g := loadBuiltAppGolden(t)
	all := approvedRead(t, g, func(appPackage) bool { return true })
	want := "v1.4 passed review: no blocking finding; its built app's known-vulnerable packages its review didn't see are approved: urllib3 1.26.5 and 2 more; Canyon couldn't check 3 packages in it"
	if passed, why := all.row(g.Version, g.ReviewWhy); !passed || why != want {
		t.Fatalf("every package approved: %v %q\nwant %q", passed, why, want)
	}
	one := approvedRead(t, g, func(p appPackage) bool { return p.name == "urllib3" })
	want = "2 known-vulnerable packages are in v1.4's built app, but its review didn't see them: lodash 4.17.20 and 1 more"
	if passed, why := one.row(g.Version, g.ReviewWhy); passed || why != want {
		t.Fatalf("urllib3 approved: %v %q\nwant %q", passed, why, want)
	}
	var listed []string
	for _, p := range one.listed() {
		if len(p.Fingerprints) == 0 {
			t.Errorf("%s %s lists no fingerprint to approve", p.Name, p.Version)
		}
		listed = append(listed, fmt.Sprintf("%s %s %v", p.Name, p.Version, p.Approved))
	}
	if w := []string{"lodash 4.17.20 false", "org.yaml/snakeyaml 1.33 false", "urllib3 1.26.5 true"}; !slices.Equal(listed, w) {
		t.Fatalf("the row lists %v, want %v", listed, w)
	}
}

// A package is approved only whole: one advisory of two covered leaves it counted (#2610).
func TestDepsImageApprovesAPackageOnlyWhole(t *testing.T) {
	app := builtApp{packages: []appPackage{{name: "lodash", version: "4.17.20", fingerprints: []string{"f_a", "f_b"}}}}
	if app.approve(map[string]bool{"f_a": true}); len(app.packages) != 1 || len(app.approved) != 0 {
		t.Fatalf("one advisory of two covered approved the package: %+v", app)
	}
	if app.approve(map[string]bool{"f_a": true, "f_b": true}); len(app.packages) != 0 || len(app.approved) != 1 {
		t.Fatalf("both covered left the package counted: %+v", app)
	}
}

// A built app's fingerprint is Canyon's layers' recipe under deps.known-vuln: its rule, vulnerability, name, version
// and class, and no layer or digest, so an approval follows the package across rebuilds (contracts/finding.md,
// The built app, Fingerprint).
func TestDepsImageFingerprintsNameNoLayer(t *testing.T) {
	g := loadBuiltAppGolden(t)
	app := readBuiltApp(g.facts(t), g.reported(), g.ReadAt)
	i := slices.IndexFunc(app.packages, func(p appPackage) bool { return p.name == "lodash" })
	if i < 0 {
		t.Fatalf("no lodash counted: %v", app.names())
	}
	p := app.packages[i]
	if want := imageFingerprint(depsRule, p.advisories[0].ID, "lodash", "4.17.20", "app"); !slices.Contains(p.fingerprints, want) {
		t.Fatalf("lodash's fingerprints %v lack the recipe's %s", p.fingerprints, want)
	}
}

// The approval golden's rows are what the server derives from built-app.json with its approval covering the
// fingerprints its body names, and with every package's (contracts/testdata/platform/built-app-approval.json; #2610).
func TestDepsImageApprovalGolden(t *testing.T) {
	g := loadBuiltAppGolden(t)
	raw, err := os.ReadFile("../../contracts/testdata/platform/built-app-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ApproveFindings struct {
			Fingerprints []string `json:"fingerprints"`
		} `json:"approve_findings"`
		Row struct {
			Passed   bool              `json:"passed"`
			Why      string            `json:"why"`
			BuiltApp []builtAppPackage `json:"built_app"`
		} `json:"row"`
		AllApproved struct {
			Passed bool   `json:"passed"`
			Why    string `json:"why"`
		} `json:"all_approved"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	named := approvedRead(t, g, func(p appPackage) bool {
		return slices.ContainsFunc(p.fingerprints, func(fp string) bool { return slices.Contains(doc.ApproveFindings.Fingerprints, fp) })
	})
	passed, why := named.row(g.Version, g.ReviewWhy)
	got, _ := json.Marshal(named.listed())
	want, _ := json.Marshal(doc.Row.BuiltApp)
	if passed != doc.Row.Passed || why != doc.Row.Why || string(got) != string(want) {
		t.Fatalf("the row with the golden's approval: %v %q %s\nwant %v %q %s", passed, why, got, doc.Row.Passed, doc.Row.Why, want)
	}
	all := approvedRead(t, g, func(appPackage) bool { return true })
	if passed, why := all.row(g.Version, g.ReviewWhy); passed != doc.AllApproved.Passed || why != doc.AllApproved.Why {
		t.Fatalf("every package approved: %v %q, want %v %q", passed, why, doc.AllApproved.Passed, doc.AllApproved.Why)
	}
}

// One package held in two classes has a fingerprint in each (contracts/finding.md, The built app, Fingerprint):
// urllib3 1.26.5 both in another buildpack's layer (unknown) and, here, in the app's own (app). Approving its
// unknown fingerprint alone leaves it counted; approving both approves it (#2610).
func TestDepsImageApprovesAPackageInTwoClassesOnlyInBoth(t *testing.T) {
	g := loadBuiltAppGolden(t)
	f := g.facts(t)
	app := "sha256:" + strings.Repeat("6", 64)
	i := slices.IndexFunc(f.layers["sha256:"+strings.Repeat("5", 64)], func(c trivy.Component) bool { return c.Name == "urllib3" })
	if i < 0 {
		t.Fatal("no urllib3 in the other buildpack's layer")
	}
	again := f.layers["sha256:"+strings.Repeat("5", 64)][i]
	again.BOMRef = "pkg:pypi/urllib3@1.26.5#app"
	again.Properties = slices.Clone(again.Properties)
	for j := range again.Properties {
		if again.Properties[j].Name == "aquasecurity:trivy:LayerDiffID" {
			again.Properties[j].Value = app
		}
	}
	f.layers[app] = append(slices.Clone(f.layers[app]), again)
	m := f.newest[app]
	v := trivy.Vuln{VulnerabilityID: "CVE-2023-43804", Severity: "CRITICAL", FixedVersion: "1.26.17, 2.0.6"}
	v.PkgIdentifier.BOMRef, v.Layer.DiffID = again.BOMRef, app
	m.report.Results = append(slices.Clone(m.report.Results), trivy.Result{Vulnerabilities: []trivy.Vuln{v}})
	f.newest[app] = m

	read := readBuiltApp(f, g.reported(), g.ReadAt)
	k := slices.IndexFunc(read.packages, func(p appPackage) bool { return p.name == "urllib3" })
	if k < 0 {
		t.Fatalf("urllib3 not counted: %v", read.names())
	}
	unknown, ofApp := imageFingerprint(depsRule, "CVE-2023-43804", "urllib3", "1.26.5", "unknown"), imageFingerprint(depsRule, "CVE-2023-43804", "urllib3", "1.26.5", "app")
	if fps := read.packages[k].fingerprints; len(fps) != 2 || !slices.Contains(fps, unknown) || !slices.Contains(fps, ofApp) {
		t.Fatalf("urllib3 in two classes has fingerprints %v, want its unknown one and its app one", fps)
	}
	if read.approve(map[string]bool{unknown: true}); !slices.Contains(read.names(), "urllib3 1.26.5") {
		t.Fatalf("one class's fingerprint approved urllib3 in both: %v", read.names())
	}
	if read.approve(map[string]bool{unknown: true, ofApp: true}); slices.Contains(read.names(), "urllib3 1.26.5") {
		t.Fatalf("both classes' fingerprints left urllib3 counted: %v", read.names())
	}
}
