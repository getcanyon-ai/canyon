package trivy

import (
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"
)

// The platform golden's image (contracts/testdata/platform/claims-portal.json): two run-image layers, a
// node-engine, an npm-install and an operator's buildpack layer, the app, the launcher and config.
func goldenImage(t *testing.T) (diffIDs []string, label string) {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/platform/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Image struct {
			DiffIDs   []string `json:"diff_ids"`
			Lifecycle string   `json:"lifecycle_metadata"`
		} `json:"image"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Image.DiffIDs, doc.Image.Lifecycle
}

func layer(n string) string { return "sha256:" + strings.Repeat(n, 64) }

// Each layer is owned as the image's lifecycle label names it, and a layer it names twice over, or not at
// all, has no owner. When no layer can be owned at all — no label, one that does not parse, a top layer
// not the image's, no layer the run image's alone — it says so, and nothing is read.
func TestPlatformClassOwnersFromTheLifecycleLabel(t *testing.T) {
	diffIDs, label := goldenImage(t)
	want := map[string]string{
		layer("1"): RunImage, layer("2"): RunImage, layer("3"): "paketo-buildpacks/node-engine", layer("4"): "paketo-buildpacks/npm-install",
		layer("5"): "acme/certificates", layer("6"): App, layer("7"): Lifecycle, layer("8"): Lifecycle,
	}
	if got, ok := Owners(diffIDs, label); !ok || !maps.Equal(got, want) {
		t.Fatalf("owners: %v\nwant %v", got, want)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(label), &meta); err != nil {
		t.Fatal(err)
	}
	meta["sbom"] = map[string]any{"sha": layer("9")} // what the build put in the sbom layer, its documents skipped, is the app's
	sbom, _ := json.Marshal(meta)
	if got, _ := Owners(append(diffIDs, layer("9")), string(sbom)); got[layer("9")] != App {
		t.Fatalf("the sbom layer is owned by %q, want the app", got[layer("9")])
	}
	delete(meta, "sbom")
	meta["app"] = []any{map[string]any{"sha": layer("7")}} // the launcher's layer named as the app's too
	twice, _ := json.Marshal(meta)
	if got, _ := Owners(diffIDs, string(twice)); got[layer("7")] != "" {
		t.Fatalf("a layer named under two owners is owned by %q", got)
	}
	ownerless := strings.Replace(label, `"app":[{"sha":"`+layer("6")+`"}]`, `"app":[{"sha":"`+layer("6")+`"},{"sha":"`+layer("1")+`"},{"sha":"`+layer("2")+`"}]`, 1)
	if ownerless == label {
		t.Fatal("the golden's label names its app layer otherwise")
	}
	for name, bad := range map[string]string{
		"no label":                                  "",
		"a label that does not parse":               "{",
		"a top layer not the image's":               `{"runImage":{"topLayer":"sha256:00"}}`,
		"no top layer":                              `{}`,
		"every run-image layer named the app's too": ownerless,
	} {
		if owners, ok := Owners(diffIDs, bad); ok {
			t.Errorf("%s: read as %v", name, owners)
		}
	}
}

// A package's class is f(owner, package type, purl) (design/PATCHING.md §2): an installer's layer is the app's,
// never a runtime's, but for the installer's own Go binary; a Go binary's standard library takes its layer's
// class — the launcher's the lifecycle's, a buildpack helper's the runtime's, an operator's buildpack's unknown —
// but in the app's own Go build, where it is go-dist's; ca-certificates' and spring-boot's layers are runtime
// providers'; a dpkg package outside the run image, and anything an operator's buildpack wrote, is unknown.
func TestPlatformClassOfAPackage(t *testing.T) {
	for _, c := range []struct{ owner, pkgType, purl, want string }{
		{RunImage, "ubuntu", "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04", "os"},
		{RunImage, "node-pkg", "pkg:npm/corepack@0.29.4", "unknown"},
		{"paketo-buildpacks/node-engine", "ubuntu", "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4", "unknown"},
		{"paketo-buildpacks/node-engine", "node-pkg", "pkg:npm/cross-spawn@7.0.3", "runtime"},
		{"paketo-buildpacks/cpython", "python-pkg", "pkg:pypi/pip@24.0", "runtime"},
		{"paketo-buildpacks/npm-install", "node-pkg", "pkg:npm/express@4.19.2", "app"},
		{"paketo-buildpacks/pip-install", "python-pkg", "pkg:pypi/flask@3.0.3", "app"},
		{App, "node-pkg", "pkg:npm/claims-portal@1.4.0", "app"},
		{Lifecycle, "gobinary", "pkg:golang/stdlib@v1.22.5", "lifecycle"},
		{Lifecycle, "gobinary", "pkg:golang/github.com/buildpacks/lifecycle@v0.20.4", "lifecycle"},
		{App, "gobinary", "pkg:golang/stdlib@v1.22.5", "runtime"},
		{"paketo-buildpacks/go-build", "gobinary", "pkg:golang/stdlib@v1.22.5", "runtime"},
		{"paketo-buildpacks/npm-install", "gobinary", "pkg:golang/github.com/anchore/syft@v1.50.0", "runtime"},
		{"paketo-buildpacks/npm-install", "gobinary", "pkg:golang/stdlib@v1.26.5", "runtime"},
		{"paketo-buildpacks/ca-certificates", "gobinary", "pkg:golang/github.com/paketo-buildpacks/libpak@v1.73.0", "runtime"},
		{"paketo-buildpacks/ca-certificates", "gobinary", "pkg:golang/stdlib@v1.27.1", "runtime"},
		{"paketo-buildpacks/spring-boot", "jar", "pkg:maven/org.springframework.cloud/spring-cloud-bindings@2.0.4", "runtime"},
		{"acme/certificates", "gobinary", "pkg:golang/stdlib@v1.27.1", "unknown"},
		{"", "gobinary", "pkg:golang/stdlib@v1.27.1", "unknown"},
		{"acme/npm-install", "node-pkg", "pkg:npm/express@4.19.2", "unknown"},
		{"acme/certificates", "python-pkg", "pkg:pypi/certifi@2023.7.22", "unknown"},
		{"", "node-pkg", "pkg:npm/express@4.19.2", "unknown"},
		{"paketo-buildpacks/node-engine", "", "", "runtime"},
		{RunImage, "", "not a purl", "unknown"},
	} {
		if got := Class(c.owner, c.pkgType, c.purl); got != c.want {
			t.Errorf("%s (%s) in %q's layer: %s, want %s", c.purl, c.pkgType, c.owner, got, c.want)
		}
	}
}

// The buildpack a newer build of which fixes a runtime package is its layer's, but for Go's standard library
// in the app's own Go build: go-dist's (contracts/finding.md, Texts).
func TestPlatformClassFixedBy(t *testing.T) {
	for owner, want := range map[string]string{
		"paketo-buildpacks/node-engine": "paketo-buildpacks/node-engine", "paketo-buildpacks/ca-certificates": "paketo-buildpacks/ca-certificates",
		"paketo-buildpacks/npm-install": "paketo-buildpacks/npm-install", App: GoDist, "paketo-buildpacks/go-build": GoDist,
	} {
		if got := FixedBy(owner); got != want {
			t.Errorf("a runtime package in %q's layer is fixed by %s, want %s", owner, got, want)
		}
	}
}
