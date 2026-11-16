package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/getcanyon-ai/canyon/ingest"
)

// claimsPortal is the fixture app both tracks share; the golden is that tree read back.
const claimsPortal = "../gate/testdata/apps/claims-portal"

// archive reads one back the way the server does, straight into tar.NewReader with no
// decompressor, as a line per entry: mode, size, name. An entry that is neither a file nor a
// directory, or a header carrying a clock or an account, fails here rather than at the route.
// must fails the test on the first error among setup steps that only have to succeed.
func must(t *testing.T, errs ...error) {
	t.Helper()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func archive(t *testing.T, r io.Reader) (list string, files int, size int64) {
	t.Helper()
	var b strings.Builder
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return b.String(), files, size
		}
		if err != nil {
			t.Fatalf("the archive did not read back as plain tar: %v", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			t.Fatalf("entry %q is a %q, and the server takes files and directories only", h.Name, string(h.Typeflag))
		}
		if h.ModTime.Unix() != 0 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Fatalf("entry %q carries %v %d:%d %q, so the same tree does not pack twice the same", h.Name, h.ModTime, h.Uid, h.Gid, h.Uname)
		}
		n, err := io.Copy(io.Discard, tr) //nolint:gosec // test: an archive this test just wrote, and plain tar decompresses nothing
		if err != nil || n != h.Size {
			t.Fatalf("entry %q read %d of its %d bytes: %v", h.Name, n, h.Size, err)
		}
		if h.Typeflag == tar.TypeReg {
			files, size = files+1, size+h.Size
		}
		fmt.Fprintf(&b, "%04o %9d  %s\n", h.Mode, h.Size, h.Name)
	}
}

// The golden is this function's contract in one file: which entries, in which order, at which
// mode and size. Regenerate it only for a deliberate change to the fixture app.
func TestTheSharedFixturePacksToItsGolden(t *testing.T) {
	var buf bytes.Buffer
	files, size, skip, err := pack(claimsPortal, &buf)
	golden := filepath.Join("testdata", "pack-claims-portal.txt")
	want, readErr := os.ReadFile(golden) //nolint:gosec // test: the golden beside this file
	must(t, err, readErr)
	list, readFiles, readSize := archive(t, &buf)
	if list != string(want) {
		t.Errorf("%s no longer describes %s:\n--- want ---\n%s--- got ---\n%s", golden, claimsPortal, want, list)
	}
	if files != readFiles || size != readSize || skip.Paths != 0 {
		t.Errorf("pack reported %d files, %d bytes and %d skipped; the archive holds %d and %d", files, size, skip.Paths, readFiles, readSize)
	}
}

// One working tree, read back exactly. The exclusions are applied and the .gitignore decides
// nothing either way: a file it un-ignores stays out, one it ignores travels. Symlinks are skipped
// rather than followed and .git never travels. gradlew arrives runnable — git records 100755 and
// the server keeps the bit — or the build fails with EACCES where nobody can reach it.
func TestWhatTravelsFromAWorkingTreeAndAtWhatMode(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		".gitignore": "!.env\n!node_modules\ngradlew\n", ".env": "STRIPE_SECRET_KEY=\n",
		".env.local": "STRIPE_SECRET_KEY=\n", ".git/config": "[core]\n", ".gitmodules": "[submodule]\n",
		".venv/bin/activate": "x\n", "build/app.js": "x\n", "dist/app.js": "x\n",
		"node_modules/left-pad/index.js": "x\n", "gradlew": "#!/bin/sh\n", "src/keep.ts": "export const keep = 1\n",
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(path), 0o750), os.WriteFile(path, []byte(body), 0o600))
	}
	//nolint:gosec // test: 0755 on gradlew is the point
	must(t, os.Chmod(filepath.Join(dir, "gradlew"), 0o755), os.Symlink("keep.ts", filepath.Join(dir, "src", "link.ts")))
	var buf bytes.Buffer
	files, _, skip, err := pack(dir, &buf)
	must(t, err)
	list, _, _ := archive(t, &buf)
	want := "0644        28  .gitignore\n0755        10  gradlew\n0755         0  src/\n0644        22  src/keep.ts\n"
	if list != want || files != 3 || skip.Paths != 9 { // skipped: two .env, four dirs, .git, .gitmodules, the symlink
		t.Errorf("%d files and %d paths not packed:\n%s", files, skip.Paths, list)
	}
	// What stayed home is named, not only counted, because the Package line says it out loud
	// (design/specs/cli.md §3.1). Distinct base names: the two .env files are one name. The
	// symlink is counted above and named nowhere — no exclusion stopped it.
	named := strings.Join(skip.Excluded, " ")
	if want := ".env node_modules dist build .venv .env.local"; named != want { // one of each in packExclusions' order, then the rest; .git's plumbing named nowhere
		t.Errorf("the walk excluded %q, want %q", named, want)
	}
	// And each path, for when a build then fails (#1246): walk order, a directory with a trailing
	// slash, the symlink as any path. Every path counted is listed but git's record.
	listed := strings.Join(skip.NotPacked, " ")
	if want := ".env .env.local .venv/ build/ dist/ node_modules/ src/link.ts"; listed != want || skip.Paths != len(skip.NotPacked)+2 {
		t.Errorf("the walk listed %q of %d paths, want %q and .git, .gitmodules unlisted", listed, skip.Paths, want)
	}
}

// A Next.js app built on the laptop holds .next/, Next's own build output, and its
// prerender-manifest.json carries keys Next generated. Packed, gitleaks blocks the app on them and
// tells the builder to rotate a key they never had (QA batch 2026-10-07, next-cve). It stays home
// like dist does: at the root only, so a source directory someone named .next elsewhere still travels.
func TestNextsBuildOutputStaysHome(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{".next/prerender-manifest.json", "app/page.tsx", "docs/.next/notes.md"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(path), 0o750), os.WriteFile(path, []byte("x\n"), 0o600))
	}
	var buf bytes.Buffer
	_, _, skip, err := pack(dir, &buf)
	must(t, err)
	list, _, _ := archive(t, &buf)
	if strings.Contains(list, " .next/") || !strings.Contains(list, "docs/.next/notes.md") {
		t.Errorf("the package holds:\n%s", list)
	}
	if named := strings.Join(skip.Excluded, " "); named != ".next" {
		t.Errorf("the walk excluded %q, want \".next\"", named)
	}
}

// One name per thing left behind, whichever directory it was found in, and the declared list's
// order however the walk met them. Encounter order here is the reverse of the printed order, so
// a walk that returned its own order would fail this: 1_app is visited first and holds the name
// the list declares last.
func TestOneNamePerThingLeftBehindInTheListsOrder(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"1_app/.venv/bin/activate", "2_app/node_modules/left-pad/index.js", "3_app/.env",
		"3_app/web/node_modules/x.js", "4_app/.env", "5_app/.git/config", "keep.ts",
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(path), 0o750), os.WriteFile(path, []byte("x\n"), 0o600))
	}
	var buf bytes.Buffer
	_, _, skip, err := pack(dir, &buf)
	must(t, err)
	// .env twice and node_modules twice is two things, not four; .git is counted and not named.
	if named := strings.Join(skip.Excluded, " "); named != ".env node_modules .venv" {
		t.Errorf("the walk excluded %q, want \".env node_modules .venv\"", named)
	}
	if skip.Paths != 6 { // two .env, two node_modules, one .venv, one .git
		t.Errorf("%d paths stayed home, want 6", skip.Paths)
	}
}

// A second name of one kind never crowds out the first of another: Vite and Next document a set of
// .env files, and three of them must not push node_modules off a three-name line.
func TestOneNameOfEachKindBeforeASecondOfAny(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{".env", ".env.local", ".env.production", "node_modules/x.js", "dist/app.js", "keep.ts"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(path), 0o750), os.WriteFile(path, []byte("x\n"), 0o600))
	}
	var buf bytes.Buffer
	_, _, skip, err := pack(dir, &buf)
	must(t, err)
	if named := strings.Join(skip.Excluded, " "); named != ".env node_modules dist .env.local .env.production" {
		t.Errorf("the walk excluded %q, want one of each kind first", named)
	}
	if got := excludedNames(skip.Excluded); got != ".env, node_modules, dist and 2 more excluded" {
		t.Errorf("the Package line reads %q", got)
	}
}

// The inference in packSkip.excluded — that an entry's own last segment is the segment that
// matched — holds only while the list is matched one segment at a time. A pattern holding a "/"
// would match an ancestor the walk never pruned, and the clause would name the wrong thing.
func TestTheListIsMatchedOneSegmentAtATime(t *testing.T) {
	for _, pat := range packExclusions {
		if strings.Contains(strings.TrimPrefix(pat, "/"), "/") { // a leading / only anchors an entry to the root
			t.Errorf("packExclusions holds %q, which spans segments: packSkip.excluded names the wrong segment for it", pat)
		}
	}
	// The other half of the invariant is ingest's: it matches each segment, never the whole path.
	if !ingest.Excluded("x/node_modules", packExclusions) || ingest.Excluded("a/b", []string{"a/b"}) {
		t.Error("ingest.Excluded no longer matches one path segment at a time")
	}
}

// The clause the Package line carries, over the counts the walk reports. Two names read as the
// spec's own transcript does; past the cap the line stops naming and starts counting, so a
// monorepo that matches the list everywhere still prints one line.
func TestThePackageLineNamesWhatStayedHome(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"node_modules"}, "node_modules excluded"},
		{[]string{".env", "node_modules"}, ".env and node_modules excluded"},
		{[]string{".env", "dist", "node_modules"}, ".env, dist and node_modules excluded"},
		// Four names print whole: hiding exactly one behind "1 more" spends the slot it saved.
		{[]string{".env", "build", "dist", "node_modules"}, ".env, build, dist and node_modules excluded"},
		{[]string{".env", "build", "dist", "node_modules", ".venv"}, ".env, build, dist and 2 more excluded"},
		// A name is a string off someone's disk and this one reaches a terminal: it arrives as
		// text, so it cannot repaint the line or forge one of its own (cf. the build log's own
		// escaping in TestOneDeployOverTheWire).
		{[]string{".env\x1b[2J", "node\nmodules"}, `.env\x1b[2J and node\nmodules excluded`},
	} {
		if got := excludedNames(c.names); got != c.want {
			t.Errorf("%q named %q, want %q", c.names, got, c.want)
		}
	}
}

// Both caps are the route's caps to the entry: routes.md refuses a package *over* 100,000 entries
// or 512 MiB, so the comparison is `>`. The byte cap is counted before a file is opened, which is
// what the sparse file relies on.
// unreadable is a tree whose one file cannot be opened, as a file of mode 000 is.
type unreadable struct{ fstest.MapFS }

func (u unreadable) Open(name string) (fs.File, error) {
	if name == "files/unreadable" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return u.MapFS.Open(name)
}

// A name that is not UTF-8 (an old archive's, a Windows share's) is left out and counted with the paths
// not packed, never the end of the package; a file that cannot be read stops it in Canyon's words, saying
// which and that nothing was packed (the probe apps' finding 14).
func TestANameThatIsNotUTF8IsLeftOutAndAnUnreadableFileSaysSo(t *testing.T) {
	tree := fstest.MapFS{"ok.txt": {Data: []byte("ok\n")}, "files/latin1-\xfc.txt": {Data: []byte("x\n")}, "files/b.txt": {Data: []byte("b\n")}}
	files, _, skip, err := packFS(tree, io.Discard)
	if err != nil || files != 2 || skip.Paths != 1 || !slices.Equal(skip.NotPacked, []string{"files/latin1-\xfc.txt"}) {
		t.Fatalf("packed %d, skipped %d %q, %v", files, skip.Paths, skip.NotPacked, err)
	}
	_, _, _, err = packFS(unreadable{fstest.MapFS{"files/unreadable": {Data: []byte("x\n")}}}, io.Discard)
	if err == nil || err.Error() != "files/unreadable cannot be read (permission denied), so nothing was packed" {
		t.Fatalf("an unreadable file: %v", err)
	}
}

func TestTheCapsAreTheRoutesCapsToTheEntry(t *testing.T) {
	tree := func(n int) fs.FS {
		fsys := make(fstest.MapFS, n)
		for i := 0; i < n; i++ {
			fsys[fmt.Sprintf("f%06d", i)] = &fstest.MapFile{}
		}
		return fsys
	}
	if files, _, _, err := packFS(tree(maxPackEntries), io.Discard); err != nil || files != maxPackEntries {
		t.Fatalf("a tree of exactly %d entries packed %d files: %v", maxPackEntries, files, err)
	}
	var big *PackTooBig
	if _, _, _, err := packFS(tree(maxPackEntries+1), io.Discard); !errors.As(err, &big) || big.Limit != maxPackEntries {
		t.Fatalf("a tree of %d entries was accepted: %v", maxPackEntries+1, err)
	}
	path := filepath.Join(t.TempDir(), "big.bin") // sparse: no disk behind it, and never read
	must(t, os.WriteFile(path, nil, 0o600), os.Truncate(path, maxPackBytes+1))
	if _, _, _, err := pack(filepath.Dir(path), io.Discard); !errors.As(err, &big) || big.Limit != maxPackBytes {
		t.Fatalf("a tree of %d bytes was accepted: %v", int64(maxPackBytes)+1, err)
	}
}

// The list is written out twice, so this reads the seam's own fixture and refuses a drift.
func TestTheExclusionsAreTheSnapshotsExclusions(t *testing.T) {
	body, err := os.ReadFile("../contracts/testdata/snapshot/meridian.json") //nolint:gosec // test: the seam's fixture
	must(t, err)
	var snapshot struct{ Exclusions []string }
	must(t, json.Unmarshal(body, &snapshot))
	if !slices.Equal(snapshot.Exclusions, packExclusions) {
		t.Errorf("the snapshot excludes %q and the CLI sends %q", snapshot.Exclusions, packExclusions)
	}
}
