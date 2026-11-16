package trivy

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/package-url/packageurl-go"
)

// Owners and classes of an image's layers (contracts/finding.md, System libraries and build tools; design/PATCHING.md §2).
const (
	RunImage  = "run-image"
	Lifecycle = "lifecycle"
	App       = "app"
)

// The buildpacks whose layers hold what Canyon's builder ships — a runtime and what it bundles, a buildpack's
// own helpers — and those that install the app's own (design/PATCHING.md §2). Every other buildpack's layer
// is unknown: an operator's own, or one of another namespace with the same name.
var (
	runtimes = map[string]bool{"paketo-buildpacks/node-engine": true, "paketo-buildpacks/cpython": true, "paketo-buildpacks/bellsoft-liberica": true, "paketo-buildpacks/go-dist": true,
		"paketo-buildpacks/ca-certificates": true, "paketo-buildpacks/spring-boot": true}
	installers = map[string]bool{
		"paketo-buildpacks/npm-install": true, "paketo-buildpacks/yarn-install": true, "paketo-buildpacks/pnpm-install": true,
		"paketo-buildpacks/pip-install": true, "paketo-buildpacks/pipenv-install": true, "paketo-buildpacks/poetry-install": true,
		"paketo-buildpacks/conda-env-update": true, "paketo-buildpacks/bundle-install": true, "paketo-buildpacks/composer-install": true,
		"paketo-buildpacks/go-mod-vendor": true, "paketo-buildpacks/maven": true, "paketo-buildpacks/gradle": true,
	}
)

// GoDist is the buildpack that fixes Go's standard library in the app's own Go build, which goBuild's
// layer holds (design/PATCHING.md §2).
const (
	GoDist  = "paketo-buildpacks/go-dist"
	goBuild = "paketo-buildpacks/go-build"
)

// FixedBy is the buildpack a newer build of which fixes a runtime package in owner's layer: that layer's,
// but for Go's standard library in the app's own Go build, go-dist's.
func FixedBy(owner string) string {
	if runtimes[owner] || installers[owner] {
		return owner
	}
	return GoDist
}

type layerSHA struct {
	SHA string `json:"sha"`
}

// lifecycleLabel is the part of the image's io.buildpacks.lifecycle.metadata label that says whose each
// layer is (buildpacks/lifecycle, platform.LayersMetadata).
type lifecycleLabel struct {
	App        []layerSHA `json:"app"`
	Launcher   layerSHA   `json:"launcher"`
	Config     layerSHA   `json:"config"`
	SBOM       *layerSHA  `json:"sbom"`
	Buildpacks []struct {
		Key    string              `json:"key"`
		Layers map[string]layerSHA `json:"layers"`
	} `json:"buildpacks"`
	RunImage struct {
		TopLayer string `json:"topLayer"`
	} `json:"runImage"`
}

// Owners is each diff ID's owner as the image's lifecycle label names it: RunImage up to the run image's
// top layer, Lifecycle for the launcher and config layers, App for the app's and the sbom layer — the image
// scan skips the lifecycle's documents there, so the build put anything listed in it — or a buildpack's id. A layer the
// label names under two owners, or does not name, owns "". False when no layer can be owned at all: the
// image has no label, it does not parse, its run image's top layer is not one of diffIDs, or no layer up to
// it is the run image's alone (contracts/finding.md, System libraries and build tools). Then nothing is read.
func Owners(diffIDs []string, label string) (map[string]string, bool) {
	named := map[string]map[string]bool{}
	add := func(sha, owner string) {
		if sha == "" {
			return
		}
		if named[sha] == nil {
			named[sha] = map[string]bool{}
		}
		named[sha][owner] = true
	}
	var l lifecycleLabel
	if label == "" || json.Unmarshal([]byte(label), &l) != nil || !slices.Contains(diffIDs, l.RunImage.TopLayer) {
		return nil, false
	}
	top := slices.Index(diffIDs, l.RunImage.TopLayer)
	for _, below := range diffIDs[:top+1] {
		add(below, RunImage)
	}
	add(l.Launcher.SHA, Lifecycle)
	add(l.Config.SHA, Lifecycle)
	if l.SBOM != nil {
		add(l.SBOM.SHA, App)
	}
	for _, a := range l.App {
		add(a.SHA, App)
	}
	for _, bp := range l.Buildpacks {
		for _, layer := range bp.Layers {
			add(layer.SHA, bp.Key)
		}
	}
	out := make(map[string]string, len(diffIDs))
	for _, d := range diffIDs {
		out[d] = ""
		if len(named[d]) == 1 {
			for owner := range named[d] {
				out[d] = owner
			}
		}
	}
	return out, slices.Contains(slices.Collect(maps.Values(out)), RunImage)
}

// Class is a package's class, f(owner, package type, purl) (design/PATCHING.md §2): os, runtime, lifecycle,
// app or unknown. The type is trivy's PkgType; the purl is parsed by packageurl-go, and one that does not
// parse is no purl. Go's standard library takes its binary's layer's class, as the rest of the binary does,
// but in the app's own Go build, where it is go-dist's.
func Class(owner, pkgType, purl string) string {
	p, err := packageurl.FromString(purl)
	if err != nil {
		p = packageurl.PackageURL{}
	}
	switch {
	case owner == Lifecycle:
		return "lifecycle"
	case p.Type == packageurl.TypeDebian && owner == RunImage:
		return "os"
	case p.Type == packageurl.TypeDebian:
		return "unknown"
	case runtimes[owner], installers[owner] && pkgType == "gobinary": // an installer's own binary
		return "runtime"
	case GoStdlib(purl) && (owner == App || owner == goBuild):
		return "runtime"
	case owner == App || installers[owner]:
		return "app"
	}
	return "unknown"
}

// GoStdlib is whether purl, parsed by packageurl-go, is Go's standard library: pkg:golang/stdlib.
func GoStdlib(purl string) bool {
	p, err := packageurl.FromString(purl)
	return err == nil && p.Type == packageurl.TypeGolang && p.Namespace == "" && p.Name == "stdlib"
}
