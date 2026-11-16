// Package custom checks an org's custom rule by code where a model says it can be (#2637): the facts
// Canyon reads from an app's package, the CEL environment a rule's expression compiles in, and the
// translation of a rule's text into one. Phase 0: nothing in the product calls it yet.
package custom

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-enry/go-enry/v2"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/modfile"

	"github.com/getcanyon-ai/canyon/gate"
)

// FactsVersion is the version of the facts' names, types and meanings: a translation is made against
// one, so a change to any of them is a new version and every rule translates again.
const FactsVersion = 3

// Facts are what Canyon reads of one app at review, each from a real parser. A fact Canyon could not
// read is in Missing, with why, and never an empty list: an expression that reads it cannot be checked.
type Facts struct {
	Languages    []string
	Files        []string
	Dependencies []Dependency
	Services     []Service
	Roles        []string
	Secrets      []Secret
	Missing      map[string]string

	// keys is the reason a fact is missing as an approval keys it, where its text says a count that a
	// deploy can move without changing why: the count left out.
	keys map[string]string
	// undirected is why a dependency's direct is unknown, "" when every one's is known: an expression
	// that reads direct cannot be checked while one is.
	undirected string
}

// activation is the facts an evaluation reads: every one not missing.
func (f Facts) activation() map[string]any {
	all := map[string]any{"languages": f.Languages, "files": f.Files, "dependencies": f.Dependencies,
		"services": f.Services, "roles": f.Roles, "secrets": f.Secrets}
	maps.DeleteFunc(all, func(name string, _ any) bool { _, missing := f.Missing[name]; return missing })
	return all
}

// ReadFacts is the facts of the package tree, of its secrets as Canyon holds them, by name, and of the
// packages trivy listed of it: nil secrets is that fact missing, and no report the dependencies.
func ReadFacts(tree gate.Tree, secrets []Secret, pkgs Packages) Facts {
	f := Facts{Missing: map[string]string{}}
	var err error
	f.Files = tree.Files()
	if f.Languages, err = languages(tree); err != nil {
		f.Missing["languages"], f.Missing["dependencies"] = err.Error(), err.Error() // which ecosystems it has is unknown
	} else if f.Dependencies, f.undirected, err = dependencies(tree, f.Languages, pkgs); err != nil {
		f.Missing["dependencies"] = err.Error()
	}
	if f.Services, f.Roles, err = declared(tree); err != nil {
		f.Missing["services"], f.Missing["roles"] = err.Error(), err.Error()
	}
	f.Secrets = secrets
	if secrets == nil {
		f.Missing["secrets"] = "the app's secrets were not given"
	}
	f.bound()
	return f
}

// named is a path as a reason names it: the app chose it, so at most its last 60 characters, a
// control character never among them (contracts/finding.md, A custom rule checked by code).
func named(f string) string {
	f = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, f)
	if r := []rune(f); len(r) > 60 {
		return "…" + string(r[len(r)-59:])
	}
	return f
}

// tooling are the programming languages, as Linguist names them, of scripts, builds, infrastructure and
// the database, which the languages fact leaves out: an app is not written in its Dockerfile.
// read is f's bytes, or Canyon's own words for a read that failed: the error names the server's path (#2648).
func read(tree gate.Tree, f string) ([]byte, error) {
	b, err := tree.Read(f)
	if err != nil {
		return nil, fmt.Errorf("Canyon could not read %s", named(f)) //nolint:staticcheck // Canyon is a name
	}
	return b, nil
}

var tooling = []string{"Batchfile", "CMake", "Dockerfile", "Earthly", "HCL", "Jsonnet", "Just", "Makefile", "Nix",
	"PLSQL", "PLpgSQL", "PowerShell", "Procfile", "SQLPL", "Shell", "Starlark", "TSQL"}

// vendored is whether f is under .git/, node_modules/ or vendor/, not the app's own.
func vendored(f string) bool {
	return slices.ContainsFunc(strings.Split(path.Dir(f), "/"), func(d string) bool { return d == ".git" || d == "node_modules" || d == "vendor" })
}

// languages is every programming language Linguist names among the tree's files outside vendored
// directories, but tooling's, each once and in order: a .tsx file is TypeScript, as a .jsx file is
// JavaScript. A file of an extension some programming language has whose language Linguist cannot tell
// is an error: the list would be short.
func languages(tree gate.Tree) ([]string, error) {
	seen := map[string]bool{}
	for _, f := range tree.Files() {
		if vendored(f) {
			continue
		}
		lang, err := languageOf(tree, f)
		if err != nil {
			return nil, err
		}
		if lang == "" || enry.GetLanguageType(lang) != enry.Programming || slices.Contains(tooling, lang) {
			continue
		}
		if lang == "TSX" {
			lang = "TypeScript"
		}
		seen[lang] = true
	}
	return append([]string{}, slices.Sorted(maps.Keys(seen))...), nil
}

// languageOf is Linguist's language of f: by its name or its extension where either settles it, else by
// its first 16 KiB as well; "" when Linguist names none.
func languageOf(tree gate.Tree, f string) (string, error) {
	if lang, safe := enry.GetLanguageByFilename(f); safe {
		return lang, nil
	}
	if lang, safe := enry.GetLanguageByExtension(f); safe {
		return lang, nil
	}
	candidates := enry.GetLanguagesByExtension(f, nil, nil)
	if path.Ext(f) != "" && len(candidates) == 0 {
		return "", nil
	}
	b, err := read(tree, f)
	if err != nil {
		return "", err
	}
	lang := enry.GetLanguage(path.Base(f), b[:min(len(b), 16<<10)])
	if lang == "" && slices.ContainsFunc(candidates, func(c string) bool { return enry.GetLanguageType(c) == enry.Programming }) {
		return "", fmt.Errorf("Canyon can't tell the language of %s", named(f)) //nolint:staticcheck // Canyon is a name
	}
	return lang, nil
}

// bound makes a fact past its bounds missing: the cost estimate an expression was accepted under
// would not hold for it.
func (f *Facts) bound() {
	strs := map[string][]string{"languages": f.Languages, "files": f.Files, "roles": f.Roles}
	for _, d := range f.Dependencies {
		strs["dependencies"] = append(strs["dependencies"], d.Name, d.Version, d.Ecosystem)
	}
	for _, s := range f.Services {
		strs["services"] = append(strs["services"], s.Name, s.Type)
	}
	for _, s := range f.Secrets {
		strs["secrets"] = append(strs["secrets"], s.Name, s.Env, s.Where)
	}
	sizes := map[string]int{"languages": len(f.Languages), "files": len(f.Files), "dependencies": len(f.Dependencies),
		"services": len(f.Services), "roles": len(f.Roles), "secrets": len(f.Secrets)}
	for name, b := range bounds {
		if _, missing := f.Missing[name]; missing {
			continue
		}
		if sizes[name] > b.items {
			f.Missing[name] = fmt.Sprintf("%d of them, more than the %d Canyon checks by code", sizes[name], b.items)
			if f.keys == nil {
				f.keys = map[string]string{}
			}
			f.keys[name] = fmt.Sprintf("more than the %d Canyon checks by code", b.items)
			continue
		}
		if slices.ContainsFunc(strs[name], func(s string) bool { return utf8.RuneCountInString(s) > b.chars }) {
			f.Missing[name] = fmt.Sprintf("one is longer than the %d characters Canyon checks by code", b.chars)
		}
	}
}

// Packages is what the review's packages scan answered (contracts/executor.md, target packages): trivy's
// JSON report of the packages the tree's files name, or why there is none.
type Packages struct {
	Report []byte
	Why    string
}

// The reasons there is no report, in the words a could-not-check finding carries.
const (
	TrivyOff        = "Canyon reads an app's packages with Trivy, which is off on this Canyon"
	TrivyUnfinished = "Trivy did not finish reading the app's packages"
	TrivyTooLarge   = "Canyon's list of the app's packages is too large to check"
	TrivyBusy       = "Trivy was too busy to read the app's packages"
	TrivyUnasked    = "Canyon could not ask Trivy for the app's packages"
	TrivyUnread     = "this rule reads no packages" // never a reason a rule reads: no rule that reads them is asked with it
)

// lockfiles are the names of the files Canyon reads packages from: each one in the tree is a file trivy
// listed, or the packages are missing, since trivy passes over a file it cannot parse without a word.
var lockfiles = []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "poetry.lock", "uv.lock", "Pipfile.lock",
	"go.mod", "Cargo.lock", "Gemfile.lock", "composer.lock", "packages.lock.json", "gradle.lockfile", "mix.lock", "pubspec.lock",
	"Package.resolved", "Podfile.lock", "conan.lock"}

// locked is the ecosystem of the packages of each kind of file trivy reads that names every package the
// app runs with, by trivy's type for it: a lockfile, or a go.mod of go 1.17 or later.
var locked = map[string]string{
	"npm": "npm", "yarn": "npm", "pnpm": "npm", "bun": "npm", "poetry": "pypi", "uv": "pypi", "pipenv": "pypi", "gomod": "go",
	"cargo": "cargo", "bundler": "gem", "composer": "composer", "nuget": "nuget", "gradle": "maven", "hex": "hex", "pub": "pub",
	"swift": "swift", "cocoapods": "cocoapods", "conan": "conan",
}

// Why a file trivy lists packages of, or one it reads none of, does not name every package the app runs
// with: by trivy's type, then by the file's own name.
const (
	requirementsWhy = "%s names the packages it asks for, not every one pip installs: a uv.lock, poetry.lock or Pipfile.lock names them all"
	pomWhy          = "%s doesn't name the packages Maven adds when it builds: a gradle.lockfile, or building with Gradle, names them all"
)

// ecosystems are the ecosystems that hold each language's packages: a language with none of them read
// has packages Canyon cannot list, and one not here has packages Canyon does not read.
var ecosystems = map[string][]string{
	"JavaScript": {"npm"}, "TypeScript": {"npm"}, "Python": {"pypi"}, "Go": {"go"}, "Java": {"maven"}, "Kotlin": {"maven"},
	"Scala": {"maven"}, "Groovy": {"maven"}, "Ruby": {"gem"}, "Rust": {"cargo"}, "PHP": {"composer"}, "C#": {"nuget"}, "F#": {"nuget"},
	"Visual Basic .NET": {"nuget"}, "Elixir": {"hex"}, "Dart": {"pub"}, "Swift": {"swift", "cocoapods"}, "Objective-C": {"cocoapods"},
	"C": {"conan"}, "C++": {"conan"},
}

// lockedBy are the lockfiles each language's ecosystems are read from, as a reason names them.
var lockedBy = map[string]string{
	"npm": "package-lock.json, yarn.lock, pnpm-lock.yaml or bun.lock", "pypi": "uv.lock, poetry.lock or Pipfile.lock", "go": "go.mod",
	"maven": "gradle.lockfile", "gem": "Gemfile.lock", "cargo": "Cargo.lock", "composer": "composer.lock", "nuget": "packages.lock.json",
	"hex": "mix.lock", "pub": "pubspec.lock", "swift": "Package.resolved", "cocoapods": "Podfile.lock", "conan": "conan.lock",
}

// manifests are the files that ask for packages and the lockfiles, beside them, that name every one; a
// manifest with none of its lockfiles beside it, or one with none at all, names packages Canyon cannot list.
var manifests = map[string][]string{
	"package.json": {"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock"}, "pyproject.toml": {"uv.lock", "poetry.lock"},
	"Pipfile": {"Pipfile.lock"}, "Gemfile": {"Gemfile.lock"}, "Cargo.toml": {"Cargo.lock"}, "composer.json": {"composer.lock"},
	"build.gradle": {"gradle.lockfile"}, "build.gradle.kts": {"gradle.lockfile"}, "mix.exs": {"mix.lock"}, "pubspec.yaml": {"pubspec.lock"},
	"Package.swift": {"Package.resolved"}, "Podfile": {"Podfile.lock"}, "conanfile.txt": {"conan.lock"}, "conanfile.py": {"conan.lock"},
	"setup.py": nil, "setup.cfg": nil, "deno.json": nil, "deno.jsonc": nil, "bun.lockb": nil, "go.work": nil, "build.sbt": nil,
}

func requirements(f string) bool {
	base := path.Base(f)
	return strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") ||
		path.Ext(f) == ".txt" && slices.Contains(strings.Split(path.Dir(f), "/"), "requirements")
}

func project(f string) bool {
	switch path.Ext(f) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return false
}

// unlocked is why f asks for packages no file beside it names every one of, "" when it does not.
func unlocked(files []string, f string) string {
	beside := func(lock string) bool { _, ok := slices.BinarySearch(files, path.Join(path.Dir(f), lock)); return ok }
	locks, ok := manifests[path.Base(f)]
	switch {
	case requirements(f):
		return fmt.Sprintf(requirementsWhy, named(f))
	case path.Base(f) == "pom.xml":
		return fmt.Sprintf(pomWhy, named(f))
	case project(f) && !beside("packages.lock.json"):
		return named(f) + " has no packages.lock.json beside it"
	case !ok || slices.ContainsFunc(locks, beside):
		return ""
	case locks == nil:
		return "dependencies are not read from " + named(f)
	}
	return fmt.Sprintf("%s has no %s beside it", named(f), strings.Join(locks, " or "))
}

// report is the part of trivy's JSON report the dependencies are read from.
type report struct {
	SchemaVersion int
	Results       []struct {
		Target, Class, Type string
		Packages            []struct {
			Name, Version, Relationship string
		}
	}
}

var pep503 = regexp.MustCompile(`[-_.]+`)

// dependencies are the packages the report lists from the files that name every package the app runs
// with, and the first such file that does not say which the app asks for itself ("" when every one
// does); or why they are not every package the app depends on.
func dependencies(tree gate.Tree, languages []string, pkgs Packages) ([]Dependency, string, error) {
	if pkgs.Report == nil {
		return nil, "", errors.New(pkgs.Why)
	}
	var r report
	if err := json.Unmarshal(pkgs.Report, &r); err != nil || r.SchemaVersion != 2 { // trivy leaves Results out when it lists nothing
		return nil, "", errors.New("Trivy's list of the app's packages is not one Canyon reads") //nolint:staticcheck // Trivy is a name
	}
	files := tree.Files()
	for _, f := range files {
		if why := unlocked(files, f); why != "" && !vendored(f) {
			return nil, "", errors.New(why)
		}
	}
	read, seen, undirected := map[string]bool{}, map[Dependency]bool{}, ""
	listed := map[string]bool{}
	for _, res := range r.Results {
		listed[res.Target] = listed[res.Target] || res.Class == "lang-pkgs"
	}
	for _, f := range files {
		if slices.Contains(lockfiles, path.Base(f)) && !vendored(f) && !listed[f] {
			return nil, "", fmt.Errorf("Canyon could not read %s", named(f)) //nolint:staticcheck // Canyon is a name
		}
	}
	for _, res := range r.Results {
		if vendored(res.Target) || res.Class == "license" || res.Class == "license-file" { // what an installed copy holds, a lockfile names; a license is no package
			continue
		}
		ecosystem, ok := locked[res.Type]
		if res.Class != "lang-pkgs" || !ok {
			return nil, "", fmt.Errorf("dependencies are not read from %s", named(res.Target))
		}
		if res.Type == "gomod" {
			if err := goMod(tree, res.Target); err != nil {
				return nil, "", err
			}
		}
		read[ecosystem] = true
		for _, p := range res.Packages {
			if p.Relationship == "root" || p.Relationship == "workspace" { // the app's own
				continue
			}
			if p.Name == "" || p.Version == "" {
				return nil, "", fmt.Errorf("%s: a package has no name or version", named(res.Target)) // never the name: it is the file's
			}
			if p.Relationship != "direct" && p.Relationship != "indirect" && undirected == "" {
				undirected = named(res.Target) + " doesn't say which packages the app asks for itself"
			}
			d := Dependency{Name: p.Name, Version: p.Version, Ecosystem: ecosystem}
			if ecosystem == "pypi" {
				d.Name = strings.ToLower(pep503.ReplaceAllString(d.Name, "-"))
			}
			seen[d] = seen[d] || p.Relationship == "direct"
		}
	}
	for _, l := range languages {
		want, ok := ecosystems[l]
		if !ok {
			return nil, "", fmt.Errorf("the dependencies of %s are not read", l)
		}
		if !slices.ContainsFunc(want, func(e string) bool { return read[e] }) {
			return nil, "", fmt.Errorf("%s with no %s", l, lockedBy[want[0]])
		}
	}
	out := make([]Dependency, 0, len(seen))
	for d, direct := range seen {
		d.Direct = direct
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Dependency) int {
		return strings.Compare(a.Ecosystem+"\x00"+a.Name+"\x00"+a.Version, b.Ecosystem+"\x00"+b.Name+"\x00"+b.Version)
	})
	return out, undirected, nil
}

// goMod is nil when the go.mod at f names every module the build needs as written: go 1.17 or later, which
// lists every one, and no replace, which would build another than it names.
func goMod(tree gate.Tree, f string) error {
	b, err := read(tree, f)
	if err != nil {
		return err
	}
	m, err := modfile.Parse(f, b, nil)
	if err != nil {
		return fmt.Errorf("%s does not parse", named(f)) // the parser's message quotes the file
	}
	if m.Go == nil || version.Compare("go"+m.Go.Version, "go1.17") < 0 {
		return fmt.Errorf("%s: no go version of 1.17 or later, so it does not name every module the build needs", named(f))
	}
	if len(m.Replace) > 0 {
		return fmt.Errorf("%s: it replaces a module", named(f)) // never the path: it is the file's
	}
	return nil
}

// maxCanyonTOML is the most of canyon.toml Canyon reads, as the server does at upload.
const maxCanyonTOML = 64 << 10

// declared is the [resources] and [roles] canyon.toml at the tree's root declares, none without one.
func declared(tree gate.Tree) ([]Service, []string, error) {
	if _, ok := slices.BinarySearch(tree.Files(), "canyon.toml"); !ok {
		return []Service{}, []string{}, nil
	}
	b, err := read(tree, "canyon.toml")
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxCanyonTOML {
		return nil, nil, fmt.Errorf("canyon.toml is larger than 64 KiB")
	}
	var file struct {
		Resources map[string]struct {
			Type string `toml:"type"`
		} `toml:"resources"`
		Roles map[string]string `toml:"roles"`
	}
	if err := toml.Unmarshal(b, &file); err != nil {
		return nil, nil, errors.New("canyon.toml is not TOML") // the decoder's message quotes the file
	}
	services := []Service{}
	for _, name := range slices.Sorted(maps.Keys(file.Resources)) {
		services = append(services, Service{Name: name, Type: file.Resources[name].Type})
	}
	return services, append([]string{}, slices.Sorted(maps.Keys(file.Roles))...), nil
}
