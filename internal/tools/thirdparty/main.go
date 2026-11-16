// Command thirdparty writes THIRD_PARTY_NOTICES (make third-party-notices): the licence and notice
// texts of every third-party component in Canyon's release binaries, read from where the build
// takes it, never typed.
//
//	thirdparty [-check] [-cli-platforms "linux/amd64 ..."] FILE
//
// Go: each binary's build list, go list -deps per platform it ships for, with each module's
// licence files from the module cache. The dashboard: web/package-lock.json's production packages,
// and the build tools whose own code lands in the bundle, with each one's licence files from
// web/node_modules. And the third-party files vendored into this repository that a binary embeds.
//
// It fails closed: a component with no licence file, a Go module with no reviewed licence below,
// or a licence outside AGENTS.md's list (MIT, Apache, BSD, MPL, ISC, 0BSD) and the exceptions
// below fails the run. -check writes nothing and fails when FILE differs from what it would write.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// binaries are the release's Go binaries, each with the platforms it ships for: the CLI's archives
// (the Makefile's CLI_PLATFORMS, passed as -cli-platforms) and the images' two (release.yml).
var binaries = []struct{ name, platforms string }{
	{"canyon", ""}, // -cli-platforms
	{"canyon-server", "linux/amd64 linux/arm64"},
	{"canyon-executor", "linux/amd64 linux/arm64"},
	{"canyon-executor-kube", "linux/amd64 linux/arm64"},
	{"canyon-identity", "linux/amd64 linux/arm64"},
}

// allowed is AGENTS.md's list of licences a dependency may carry, as SPDX ids.
var allowed = []string{"MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "MPL-2.0", "ISC", "0BSD"}

// exceptions are the licences outside allowed that the one component named may carry, each a
// reviewed decision AGENTS.md names: the vendored sast-rules' LGPL-3.0 rules (Style), the three
// permissive ones found when these notices were first written (Conventions, #2397), and the OFL-1.1
// fonts the dashboard self-hosts (#2885).
var exceptions = map[string][]string{
	"GitLab sast-rules":            {"LGPL-3.0-only"},
	"github.com/jackc/pgerrcode":   {"PostgreSQL"},
	"github.com/therootcompany/xz": {"CC0-1.0"},
	"IBM Plex":                     {"OFL-1.1"},
	"JetBrains Mono":               {"OFL-1.1"},
	"Space Grotesk":                {"OFL-1.1"},
}

// bundledDevPackages are the dashboard's devDependencies whose own code is in the bundle:
// Tailwind's base styles in the CSS, Vite's modulepreload polyfill and Rolldown's CommonJS
// wrapper (its runtime's __commonJSMin) in the JavaScript.
var bundledDevPackages = []string{"rolldown", "tailwindcss", "vite"}

// vendored are the third-party files in this repository a binary embeds, each with its licence
// files, relative to the repository. GitLab's sast-rules carry theirs inside the archive (NOTICE).
var vendored = []struct {
	name, spdx, in string
	files          []string // licence files; a .tar is read for the licence files inside it
}{
	{"GitLab sast-rules", "MIT AND Apache-2.0 AND LGPL-3.0-only", "canyon-server", []string{"gate/opengrep/sast-rules.tar"}},
	{"IBM Plex", "OFL-1.1", "canyon-server", []string{"web/src/fonts/LICENSE.txt"}},
	{"JetBrains Mono", "OFL-1.1", "canyon-server", []string{"web/src/fonts/JetBrainsMono-OFL.txt"}},
	{"Space Grotesk", "OFL-1.1", "canyon-server", []string{"web/src/fonts/SpaceGrotesk-OFL.txt"}},
	{"SARIF 2.1.0 JSON schema (SchemaStore)", "Apache-2.0", "canyon canyon-server", []string{"contracts/vendor/LICENSE.schemastore"}},
}

type component struct {
	name, spdx string
	in         map[string]bool // the binaries that carry it
	files      []licenceFile
}

type licenceFile struct{ path, text string } // path: the component's name and the file within it

func main() {
	check := flag.Bool("check", false, "write nothing; fail when FILE is not what this would write")
	cliPlatforms := flag.String("cli-platforms", "", "the platforms the CLI ships for, space separated (the Makefile's CLI_PLATFORMS)")
	flag.Parse()
	if flag.NArg() != 1 || *cliPlatforms == "" {
		fmt.Fprintln(os.Stderr, "usage: thirdparty [-check] -cli-platforms \"os/arch ...\" FILE")
		os.Exit(2)
	}
	if err := run(".", flag.Arg(0), *cliPlatforms, *check, goLicences); err != nil {
		fmt.Fprintln(os.Stderr, "thirdparty:", err)
		os.Exit(1)
	}
}

// run writes file, or with check compares it, for the repository at root, whose Go modules' licences
// are table's.
func run(root, file, cliPlatforms string, check bool, table map[string]string) error {
	goComps, err := goComponents(root, cliPlatforms, table)
	if err != nil {
		return err
	}
	npmComps, err := npmComponents(filepath.Join(root, "web"))
	if err != nil {
		return err
	}
	vendComps, err := vendoredComponents(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range slices.Concat(goComps, npmComps, vendComps) {
		if err := permitted(c.name, c.spdx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	out := render(goComps, npmComps, vendComps)
	if !check {
		return os.WriteFile(file, out, 0o644) //nolint:gosec // a file the repository commits and every artifact ships
	}
	have, err := os.ReadFile(file) //nolint:gosec // the Makefile's file
	if err != nil {
		return err
	}
	if !bytes.Equal(have, out) {
		line := 1 + bytes.Count(out[:commonPrefix(have, out)], []byte("\n"))
		return fmt.Errorf("%s is stale from its line %d: run make third-party-notices and commit it", file, line)
	}
	return nil
}

func commonPrefix(a, b []byte) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// goPackage is the part of go list -json's Package that goComponents reads.
type goPackage struct {
	Dir      string
	Standard bool
	Module   *struct {
		Path, Dir string
		Main      bool
	}
}

// goComponents is the Go standard library and every module in the binaries' build lists, each
// with the binaries that link it and its licence files: those at the module's root, and those in
// a linked package's directory or a directory between it and the root.
func goComponents(root, cliPlatforms string, table map[string]string) ([]component, error) {
	type module struct {
		dir     string
		in      map[string]bool
		pkgDirs map[string]bool
	}
	mods := map[string]*module{}
	std := map[string]bool{}
	for _, b := range binaries {
		platforms := b.platforms
		if b.name == "canyon" {
			platforms = cliPlatforms
		}
		for _, p := range strings.Fields(platforms) {
			goos, goarch, ok := strings.Cut(p, "/")
			if !ok {
				return nil, fmt.Errorf("platform %q is not os/arch", p)
			}
			cmd := exec.Command("go", "list", "-deps", "-json=Dir,Standard,Module", "./cmd/"+b.name) //nolint:gosec // binaries' own names
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
			cmd.Stderr = os.Stderr
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("go list %s for %s: %w", b.name, p, err)
			}
			dec := json.NewDecoder(bytes.NewReader(out))
			for {
				var pkg goPackage
				if err := dec.Decode(&pkg); err == io.EOF {
					break
				} else if err != nil {
					return nil, fmt.Errorf("go list %s for %s: %w", b.name, p, err)
				}
				switch {
				case pkg.Standard:
					std[b.name] = true
				case pkg.Module == nil:
					return nil, fmt.Errorf("%s: package %s is in no module", b.name, pkg.Dir)
				case pkg.Module.Main:
				default:
					if pkg.Module.Dir == "" {
						return nil, fmt.Errorf("module %s is not in the module cache", pkg.Module.Path)
					}
					m := mods[pkg.Module.Path]
					if m == nil {
						m = &module{dir: pkg.Module.Dir, in: map[string]bool{}, pkgDirs: map[string]bool{}}
						mods[pkg.Module.Path] = m
					}
					m.in[b.name] = true
					m.pkgDirs[pkg.Dir] = true
				}
			}
		}
	}

	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	stdLicence, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(goroot)), "LICENSE"))
	if err != nil {
		return nil, fmt.Errorf("the Go distribution's LICENSE: %w", err)
	}
	comps := []component{{name: "Go standard library", spdx: "BSD-3-Clause", in: std,
		files: []licenceFile{{"Go standard library/LICENSE", string(stdLicence)}}}}

	var errs []error
	for _, p := range slices.Sorted(maps.Keys(mods)) {
		m := mods[p]
		files, err := moduleLicenceFiles(p, m.dir, m.pkgDirs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		spdx, ok := table[p]
		if !ok {
			errs = append(errs, fmt.Errorf("module %s has no reviewed licence: read %s and add it to goLicences", p, strings.Join(paths(files), ", ")))
			continue
		}
		comps = append(comps, component{name: p, spdx: spdx, in: m.in, files: files})
	}
	for _, p := range slices.Sorted(maps.Keys(table)) {
		if mods[p] == nil {
			errs = append(errs, fmt.Errorf("goLicences names %s, which no binary links: remove it", p))
		}
	}
	return comps, errors.Join(errs...)
}

// moduleLicenceFiles are module p's licence files: those at its root, which it must have, and
// those in each of pkgDirs or a directory between it and the root.
func moduleLicenceFiles(p, root string, pkgDirs map[string]bool) ([]licenceFile, error) {
	dirs := map[string]bool{root: true}
	for d := range pkgDirs {
		for ; d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	var files []licenceFile
	for d := range dirs {
		found, err := licenceFiles(d)
		if err != nil {
			return nil, err
		}
		if d == root && len(found) == 0 {
			return nil, fmt.Errorf("module %s has no licence file at its root (%s)", p, root)
		}
		for _, f := range found {
			rel, err := filepath.Rel(root, f.path)
			if err != nil {
				return nil, err
			}
			files = append(files, licenceFile{p + "/" + filepath.ToSlash(rel), f.text})
		}
	}
	return files, nil
}

// licenceFiles are the licence and notice files directly in dir, by their names' conventional
// prefixes (LICENSE, LICENCE, COPYING, NOTICE, UNLICENSE), with each one's text.
func licenceFiles(dir string) ([]licenceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []licenceFile
	for _, e := range entries {
		if e.IsDir() || !isLicenceFile(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a licence file of a module or package the build uses
		if err != nil {
			return nil, err
		}
		files = append(files, licenceFile{filepath.Join(dir, e.Name()), string(b)})
	}
	return files, nil
}

func isLicenceFile(name string) bool {
	n := strings.ToLower(name)
	switch path.Ext(n) {
	case ".go", ".js", ".cjs", ".mjs", ".ts":
		return false
	}
	for _, prefix := range []string{"license", "licence", "copying", "notice", "unlicense"} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// npmComponents are web/package-lock.json's production packages and bundledDevPackages, each with
// the licence its lockfile entry declares and its licence files from node_modules.
func npmComponents(web string) ([]component, error) {
	b, err := os.ReadFile(filepath.Join(web, "package-lock.json")) //nolint:gosec // the dashboard's lockfile
	if err != nil {
		return nil, err
	}
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			License          string `json:"license"`
			Dev, DevOptional bool
			Link             bool `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("%s/package-lock.json: %w", web, err)
	}
	if lock.LockfileVersion < 2 {
		return nil, fmt.Errorf("%s/package-lock.json is lockfile version %d, which lists no packages: 2 or later", web, lock.LockfileVersion)
	}
	byName := map[string]*component{}
	licences := map[string][]string{} // each package's distinct licences, one per version that differs
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(lock.Packages)) {
		p := lock.Packages[key]
		_, name, nested := strings.Cut(key, "node_modules/")
		if !nested {
			continue // the dashboard itself
		}
		for strings.Contains(name, "node_modules/") {
			_, name, _ = strings.Cut(name, "node_modules/")
		}
		if (p.Dev || p.DevOptional) && !slices.Contains(bundledDevPackages, name) {
			continue
		}
		if p.Link {
			errs = append(errs, fmt.Errorf("npm package %s is a link, whose licence the lockfile does not give", key))
			continue
		}
		if p.License == "" {
			errs = append(errs, fmt.Errorf("npm package %s declares no licence", key))
			continue
		}
		files, err := licenceFiles(filepath.Join(web, filepath.FromSlash(key)))
		if err != nil {
			errs = append(errs, fmt.Errorf("npm package %s: %w (run make web/node_modules)", key, err))
			continue
		}
		if len(files) == 0 {
			errs = append(errs, fmt.Errorf("npm package %s has no licence file in %s", key, filepath.Join(web, key)))
			continue
		}
		c := byName[name]
		if c == nil {
			c = &component{name: name, in: map[string]bool{"canyon-server": true}}
			byName[name] = c
		}
		if !slices.Contains(licences[name], p.License) {
			licences[name] = append(licences[name], p.License)
		}
		for _, f := range files {
			c.files = append(c.files, licenceFile{name + "/" + filepath.Base(f.path), f.text})
		}
	}
	for _, name := range bundledDevPackages {
		if byName[name] == nil {
			errs = append(errs, fmt.Errorf("npm package %s, whose code is in the bundle, is not in the lockfile", name))
		}
	}
	var comps []component
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		c := byName[name]
		c.spdx = andAll(licences[name])
		comps = append(comps, *c)
	}
	return comps, errors.Join(errs...)
}

// andAll is the SPDX expression under which every one of exprs applies: two versions of a package
// under two licences are both in the bundle. Each is parenthesised, so an OR inside one cannot
// absorb the others.
func andAll(exprs []string) string {
	if len(exprs) == 1 {
		return exprs[0]
	}
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = "(" + e + ")"
	}
	slices.Sort(parts)
	return strings.Join(parts, " AND ")
}

// vendoredComponents are vendored's files, each with its licence files' texts.
func vendoredComponents(repo string) ([]component, error) {
	var comps []component
	for _, v := range vendored {
		c := component{name: v.name, spdx: v.spdx, in: map[string]bool{}}
		for _, b := range strings.Fields(v.in) {
			c.in[b] = true
		}
		for _, f := range v.files {
			if filepath.Ext(f) != ".tar" {
				b, err := os.ReadFile(filepath.Join(repo, f)) //nolint:gosec // a vendored licence file vendored names
				if err != nil {
					return nil, err
				}
				c.files = append(c.files, licenceFile{f, string(b)})
				continue
			}
			files, err := tarLicenceFiles(filepath.Join(repo, f))
			if err != nil {
				return nil, err
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("%s holds no licence file", f)
			}
			for _, lf := range files {
				c.files = append(c.files, licenceFile{f + "/" + lf.path, lf.text})
			}
		}
		comps = append(comps, c)
	}
	return comps, nil
}

func tarLicenceFiles(file string) ([]licenceFile, error) {
	b, err := os.ReadFile(file) //nolint:gosec // a vendored archive of this repository
	if err != nil {
		return nil, err
	}
	var files []licenceFile
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if h.Typeflag != tar.TypeReg || !isLicenceFile(path.Base(h.Name)) {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		files = append(files, licenceFile{h.Name, string(b)})
	}
}

// permitted fails unless spdx, an SPDX licence expression, is satisfiable from allowed and the
// component's exceptions: every operand of an AND, and one of an OR.
func permitted(name, spdx string) error {
	expr, err := parseSPDX(spdx)
	if err != nil {
		return fmt.Errorf("%s: licence %q: %w", name, spdx, err)
	}
	ok := slices.Concat(allowed, exceptions[name])
	if !expr.satisfied(func(id string) bool { return slices.Contains(ok, id) }) {
		return fmt.Errorf("%s is under %s, which is outside AGENTS.md's list (%s) and has no exception here", name, spdx, strings.Join(allowed, ", "))
	}
	return nil
}

func render(goComps, npmComps, vendComps []component) []byte {
	var b strings.Builder
	b.WriteString(`Third-party software in Canyon
==============================

Canyon itself is under the terms in LICENSE. The components below are not Canyon's: each is under
its own licence, whose text is in this file, and nothing in LICENSE limits what that licence grants.
The tools inside Canyon's scanner and build images (Opengrep, ESLint, the Paketo stacks) are listed
in NOTICE.

Generated by make third-party-notices (internal/tools/thirdparty) from the release binaries' Go
build lists, the dashboard's package-lock.json and the files vendored into Canyon's source; never
edited by hand.
`)
	section := func(title string, comps []component) {
		fmt.Fprintf(&b, "\n%s\n%s\n\n", title, strings.Repeat("-", len(title)))
		sort.Slice(comps, func(i, j int) bool { return strings.ToLower(comps[i].name) < strings.ToLower(comps[j].name) })
		for _, c := range comps {
			var in []string
			for _, bin := range binaries {
				if c.in[bin.name] {
					in = append(in, bin.name)
				}
			}
			fmt.Fprintf(&b, "%s\n    %s; in %s\n", c.name, c.spdx, strings.Join(in, ", "))
		}
	}
	section("Go modules (and the Go standard library) in the binaries", goComps)
	section("npm packages in the dashboard, which canyon-server serves", npmComps)
	section("Files vendored into Canyon's source", vendComps)

	// Each distinct text once, under every file that holds it.
	texts := map[string][]string{}
	for _, c := range slices.Concat(goComps, npmComps, vendComps) {
		for _, f := range c.files {
			t := normalise(f.text)
			texts[t] = append(texts[t], f.path)
		}
	}
	type block struct {
		paths []string
		text  string
	}
	var blocks []block
	for t, ps := range texts {
		sort.Slice(ps, func(i, j int) bool { return strings.ToLower(ps[i]) < strings.ToLower(ps[j]) })
		blocks = append(blocks, block{slices.Compact(ps), t})
	}
	sort.Slice(blocks, func(i, j int) bool {
		return strings.ToLower(blocks[i].paths[0]) < strings.ToLower(blocks[j].paths[0])
	})
	b.WriteString("\nLicence and notice texts\n------------------------\n")
	for _, bl := range blocks {
		fmt.Fprintf(&b, "\n%s\n%s\n%s\n%s", strings.Repeat("=", 80), strings.Join(bl.paths, "\n"), strings.Repeat("-", 80), bl.text)
	}
	return []byte(b.String())
}

// normalise is text with LF line ends, no trailing spaces, and one final newline.
func normalise(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n") + "\n"
}

func paths(files []licenceFile) []string {
	var ps []string
	for _, f := range files {
		ps = append(ps, f.path)
	}
	return ps
}
