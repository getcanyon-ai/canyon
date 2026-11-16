package trivy

import (
	"path"
	"regexp"
	"strings"

	"github.com/package-url/packageurl-go"
)

// PackageKey is a package as the built app compares it with what the review reported (contracts/finding.md,
// The built app): its purl type, its name as trivy names it in that ecosystem, and its version.
type PackageKey struct{ Type, Name, Version string }

// lockfiles are the purl type of the packages trivy reads from a file, by the file's name alone.
var lockfiles = map[string]string{
	"package-lock.json": packageurl.TypeNPM, "yarn.lock": packageurl.TypeNPM, "pnpm-lock.yaml": packageurl.TypeNPM, "bun.lock": packageurl.TypeNPM,
	"package.json": packageurl.TypeNPM, "requirements.txt": packageurl.TypePyPi, "Pipfile.lock": packageurl.TypePyPi, "poetry.lock": packageurl.TypePyPi,
	"uv.lock": packageurl.TypePyPi, "pyproject.toml": packageurl.TypePyPi, "pom.xml": packageurl.TypeMaven, "gradle.lockfile": packageurl.TypeMaven,
	"go.mod": packageurl.TypeGolang, "go.sum": packageurl.TypeGolang, "Gemfile.lock": packageurl.TypeGem, "composer.lock": packageurl.TypeComposer,
	"Cargo.lock": packageurl.TypeCargo, "packages.lock.json": packageurl.TypeNuget,
}

// pypiRun is what PEP 503 folds into one "-" in a Python package's name.
var pypiRun = regexp.MustCompile(`[-_.]+`)

func keyOf(typ, name, version string) PackageKey {
	if typ == packageurl.TypePyPi {
		name = strings.ToLower(pypiRun.ReplaceAllString(name, "-"))
	}
	return PackageKey{typ, name, version}
}

// KeyOfFile is the package a review finding names, by its file and its dependency's name and version; false
// when the file is one no lockfile name says the type of, so the finding stands for no package of the image.
func KeyOfFile(file, name, version string) (PackageKey, bool) {
	base := path.Base(file)
	typ, ok := lockfiles[base]
	if !ok && (strings.HasSuffix(base, ".jar") || strings.HasSuffix(base, ".war") || strings.HasSuffix(base, ".ear")) {
		typ, ok = packageurl.TypeMaven, true
	}
	return keyOf(typ, name, version), ok
}

// KeyOf is an image component's package: its purl's type and name — a Maven one's namespace, ":" and name, as
// trivy names a Java package, any other's namespace, "/" and name — and its version; false without a purl.
func KeyOf(c Component) (PackageKey, bool) {
	p, err := packageurl.FromString(c.PURL)
	if err != nil || c.PURL == "" {
		return PackageKey{}, false
	}
	name := p.Name
	switch {
	case p.Namespace != "" && p.Type == packageurl.TypeMaven:
		name = p.Namespace + ":" + p.Name
	case p.Namespace != "":
		name = p.Namespace + "/" + p.Name
	}
	return keyOf(p.Type, name, c.Version), true
}
