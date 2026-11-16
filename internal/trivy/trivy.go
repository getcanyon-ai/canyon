// Package trivy reads trivy's own fields from a finding's problem (contracts/finding.md, A dependency's
// name and version) and names the one version that fixes a package's CVEs (design/specs/cli.md §5.3),
// for the CLI and the server alike.
package trivy

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Advisory is a dependency finding as trivy wrote it: each field empty when trivy wrote no such line.
type Advisory struct {
	Name, Installed     string // Package, Installed Version
	Vulnerability, Link string // Vulnerability, and Link's url when it is https under that same id
	Fixed               string // Fixed Version, the versions as trivy wrote them
}

// link is a Link line's url as the contract carries one: https, then U+0021 to U+007E other than ")" and
// "@", so it has no user part and the host it names is the one it opens.
var link = regexp.MustCompile(`^https://[!-(*-?A-~]+$`)

// HTTPSLink reports whether u is a link a page may open: https, no user part (contracts/finding.md, A
// dependency's name and version).
func HTTPSLink(u string) bool { return link.MatchString(u) }

// Read is the problem's advisory: the last "Package: " before its first "\nInstalled Version: ", with no
// line break between them, and the rest of that line; then the last line of each kind after it. ok is
// false when there is no such pair, or the name or the version is empty.
func Read(problem string) (a Advisory, ok bool) {
	head, rest, found := strings.Cut(problem, "\nInstalled Version: ")
	i := strings.LastIndex(head, "Package: ")
	if !found || i < 0 || strings.Contains(head[i:], "\n") {
		return Advisory{}, false
	}
	lines := strings.Split(rest, "\n")
	a.Name, a.Installed = head[i+len("Package: "):], lines[0]
	if a.Name == "" || a.Installed == "" {
		return Advisory{}, false
	}
	var said string
	for _, line := range lines[1:] {
		if v, is := strings.CutPrefix(line, "Vulnerability "); is {
			a.Vulnerability = v
		}
		if v, is := strings.CutPrefix(line, "Fixed Version: "); is {
			a.Fixed = v
		}
		if v, is := strings.CutPrefix(line, "Link: "); is {
			said = v
		}
	}
	if url, is := strings.CutPrefix(said, "["+a.Vulnerability+"]("); is && a.Vulnerability != "" && strings.HasSuffix(url, ")") && link.MatchString(url[:len(url)-1]) {
		a.Link = url[:len(url)-1]
	}
	return a, true
}

// The bounds past which a group has no FixedIn: trivy names a handful of versions per CVE, and a report
// shaped otherwise gets no answer rather than a read that runs for minutes (contracts/finding.md).
const maxLists, maxVersions = 200, 32

// FixedIn is the lowest version any list names, at or above installed, that fixes every list: it stays
// in the installed major line whenever a list names one there. ok is false unless installed and every
// version are plain X.Y.Z, a >= bound read as its version — a distribution's revision (1.2.3-1+deb12u1)
// reads to semver as a pre-release, which it would order wrongly — and past the bounds above.
func FixedIn(installed string, lists [][]string) (string, bool) {
	at, ok := plain(installed)
	if !ok || len(lists) > maxLists {
		return "", false
	}
	fixes := make([]fixed, len(lists))
	var candidates []*semver.Version
	for i, list := range lists {
		if len(list) > maxVersions {
			return "", false
		}
		fixes[i].lines = map[[2]uint64]*semver.Version{}
		for _, s := range list {
			v, ok := plain(strings.TrimPrefix(s, ">="))
			if !ok {
				return "", false
			}
			fixes[i].add(v)
			if !v.LessThan(at) {
				candidates = append(candidates, v)
			}
		}
	}
	slices.SortFunc(candidates, func(a, b *semver.Version) int { return a.Compare(b) })
	candidates = slices.CompactFunc(candidates, func(a, b *semver.Version) bool { return a.Equal(b) })
	for _, c := range candidates {
		if !slices.ContainsFunc(fixes, func(f fixed) bool { return !f.fixes(c) }) {
			return c.String(), true
		}
	}
	return "", false
}

// fixed is one CVE's list, which names a fix per release line: its highest, and its lowest in each MAJOR.MINOR.
type fixed struct {
	top   *semver.Version
	lines map[[2]uint64]*semver.Version
}

func (f *fixed) add(v *semver.Version) {
	if f.top == nil || v.GreaterThan(f.top) {
		f.top = v
	}
	line := [2]uint64{v.Major(), v.Minor()}
	if low, ok := f.lines[line]; !ok || v.LessThan(low) {
		f.lines[line] = v
	}
}

// fixes is whether v is fixed: the list names one at or below v in v's own MAJOR.MINOR, or v is at or
// above the list's highest.
func (f fixed) fixes(v *semver.Version) bool {
	low, ok := f.lines[[2]uint64{v.Major(), v.Minor()}]
	return !v.LessThan(f.top) || ok && !low.GreaterThan(v)
}

// plain is s as a version when it is a plain X.Y.Z.
func plain(s string) (*semver.Version, bool) {
	v, err := semver.StrictNewVersion(s)
	if err != nil || v.Prerelease() != "" || v.Metadata() != "" {
		return nil, false
	}
	return v, true
}
