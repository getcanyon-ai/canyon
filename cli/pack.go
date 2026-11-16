package cli

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/ingest"
)

// packExclusions is the list the host's snapshot carries (contracts/testdata/snapshot/meridian.json),
// applied through the server's own matcher so one list is read one way on both sides. The server
// re-applies its own list whatever the CLI sent, so this is what keeps a .env off the wire.
var packExclusions = []string{".env*", "!.env.example", "node_modules", ".git", "/dist", "/build", "/.next", ".venv"}

// The caps the route enforces, counted here so a tree the server would refuse costs no upload.
const (
	maxPackEntries = 100000
	maxPackBytes   = 512 << 20
)

// PackTooBig is a directory over one of those caps.
type PackTooBig struct {
	Counted string // what was counted, in the words the message reads with
	Limit   int64
}

func (e *PackTooBig) Error() string {
	return fmt.Sprintf("this directory holds more %s than a Canyon package may (the limit is %d)", e.Counted, e.Limit)
}

// packSkip is what did not travel: every path the walk left behind, and the distinct names the
// exclusion list matched. The two answer different questions, so they are counted apart — a
// symlink is not packed and no exclusion stopped it, so it is counted and never named. The names
// are what the Package line says out loud (design/specs/cli.md §3.1), deduped because a tree with
// a .env in four directories left one kind of thing behind, not four. NotPacked is the paths
// themselves (#1246): when a build then fails, the path is what the builder needs, not the pattern.
// Dockerfile is the one file that travels and is never read (#1243).
type packSkip struct {
	Paths      int
	Excluded   []string       // distinct, in packExclusions order
	NotPacked  []string       // every path counted in Paths but git's record, in walk order, a directory with a trailing /
	Dockerfile bool           // the root holds a file named exactly Dockerfile, which buildpacks never read
	seen       map[string]int // name to its rank in that list: a second sighting costs a lookup, not a scan of every name so far
}

// gitPlumbing is git's own record. ingest.Excluded stops .git, .gitattributes and .gitmodules
// whatever the list says (ingest/ingest.go), and packExclusions names .git as well. All three are
// excluded and counted like anything else and none of them is named: git's record is in every
// repository there is, so naming it would spend a slot of a three-name line, every time, on the
// one fact that is always true and never the one a builder was worried about shipping.
var gitPlumbing = []string{".git", ".gitattributes", ".gitmodules"}

// excludedRank is where a name sits in packExclusions, which is the order the clause names things
// in. Lexical order would be wrong: it leads with dot-names, so .git and every .env variant would
// take the line's three slots and node_modules — named in the spec's own transcript — would never
// print. The declared list leads with what a builder actually worries about shipping, so the
// clause follows it. This decides display order only; ingest.Excluded alone decides what travels.
func excludedRank(base string) (int, bool) {
	if slices.Contains(gitPlumbing, base) {
		return 0, false
	}
	for i, pat := range packExclusions {
		if ok, _ := path.Match(strings.TrimPrefix(pat, "/"), base); ok && !strings.HasPrefix(pat, "!") { // a root entry names as any does
			return i, true
		}
	}
	return 0, false // a name the declared list does not hold is git's, in a letter case ingest folds and path.Match does not
}

// excluded records one path the list matched. The walk SkipDirs a directory it excludes, so no
// ancestor of this entry matched, so this entry's own last segment is the segment that matched:
// the name is read off the walk, never guessed. That inference rides on ingest.Excluded matching
// per path segment (ingest/ingest.go) — a leading "/" only anchors it to the root — and TestTheListIsMatchedOneSegmentAtATime pins it.
func (s *packSkip) excluded(name string, dir bool) {
	s.Paths++
	base := path.Base(name)
	rank, nameable := excludedRank(base)
	if !nameable { // git's record: counted, and neither named nor listed
		return
	}
	if dir {
		name += "/"
	}
	s.NotPacked = append(s.NotPacked, name)
	if _, already := s.seen[base]; already {
		return
	}
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.seen[base] = rank
}

// names is the clause's order: one name of each kind first, in the declared list's order, then a
// second of each, and so on; within one pattern, lexical. So .env, .env.local and node_modules
// read .env, node_modules, .env.local — a set of .env files never pushes node_modules off the line.
func (s *packSkip) names() []string {
	out := make([]string, 0, len(s.seen))
	for name := range s.seen {
		out = append(out, name)
	}
	slices.SortFunc(out, func(a, b string) int {
		if s.seen[a] != s.seen[b] {
			return s.seen[a] - s.seen[b]
		}
		return strings.Compare(a, b)
	})
	nth := make(map[string]int, len(out)) // a name's place among its own pattern's names
	for i := 1; i < len(out); i++ {
		if s.seen[out[i]] == s.seen[out[i-1]] {
			nth[out[i]] = nth[out[i-1]] + 1
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return nth[a] - nth[b] })
	return out
}

// pack writes dir as a tar to w and reports what went in and what did not. Plain tar,
// never gzip: the route hands the body straight to tar.NewReader, so a compressed one is a 400.
// Files and directories only, and a symlink is skipped rather than followed, which is all the
// server accepts. The same tree packs to the same archive anywhere: the walk is lexical and every
// header's times, ids and names are zero. The one bit of a mode that travels is the executable
// one, so a file is 0755 if anything could run it and 0644 otherwise, a directory 0755.
func pack(dir string, w io.Writer) (files int, bytes int64, skip packSkip, err error) {
	return packFS(os.DirFS(dir), w)
}

// unread is a file or directory the walk could not read, in Canyon's words: which, why and that nothing
// was packed, never Go's "open x: permission denied" alone.
func unread(name string, err error) error {
	return fmt.Errorf("%s cannot be read (%s), so nothing was packed", cell(name), plainly(err)) // cell: a name never writes to the terminal
}

// packFS is pack over any fs.FS, so the caps are demonstrable against a synthetic tree of 100,000
// entries instead of 100,000 files on a disk.
func packFS(fsys fs.FS, w io.Writer) (files int, bytes int64, skip packSkip, err error) {
	tw, entries := tar.NewWriter(w), 0
	walk := func(name string, d fs.DirEntry, err error) error {
		switch {
		case !utf8.ValidString(name): // an old archive's or a Windows share's name: a path no fs.FS opens, left out and counted, never the end of the package
			skip.Paths++
			skip.NotPacked = append(skip.NotPacked, name)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case err != nil:
			return unread(name, err)
		case name == ".": // the root itself is not an entry; the server skips it too
			return nil
		case name == baseDir: // Canyon's own record (base.go): it changes on every deploy, and packed it would change the tree hash and undo the host's dedupe
			if d.IsDir() {
				return fs.SkipDir // SkipDir from a plain file named .canyon would skip the rest of the root
			}
			return nil
		case ingest.Excluded(name, packExclusions):
			skip.excluded(name, d.IsDir())
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case !d.IsDir() && !d.Type().IsRegular():
			skip.Paths++
			skip.NotPacked = append(skip.NotPacked, name)
			return nil
		}
		h := &tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return unread(name, err)
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 { // git records 100755 and the server keeps the bit (ingest/ingest.go)
				mode = 0o755
			}
			h = &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: info.Size()}
		}
		if entries++; entries > maxPackEntries {
			return &PackTooBig{Counted: "files and directories", Limit: maxPackEntries}
		}
		if bytes += h.Size; bytes > maxPackBytes {
			return &PackTooBig{Counted: "bytes of file content", Limit: maxPackBytes}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			return nil
		}
		files++
		skip.Dockerfile = skip.Dockerfile || name == "Dockerfile"
		f, err := fsys.Open(name)
		if err != nil {
			return unread(name, err)
		}
		defer func() { _ = f.Close() }()
		n, err := io.Copy(tw, f)
		if err == nil && n != h.Size { // the header is already out, so a short entry is a corrupt archive
			err = fmt.Errorf("%s was %d bytes when it was listed and %d when it was read; re-run once it has settled", name, h.Size, n)
		}
		return err
	}
	if err := fs.WalkDir(fsys, ".", walk); err != nil {
		return 0, 0, packSkip{}, err
	}
	if err := tw.Close(); err != nil {
		return 0, 0, packSkip{}, err
	}
	skip.Excluded = skip.names()
	return files, bytes, skip, nil
}
