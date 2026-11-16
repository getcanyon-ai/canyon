package ingest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var exclusions = []string{".env*", "!.env.example", "node_modules", "/dist", "/build", ".venv"} // the snapshot's list

func tarOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range entries {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(name, "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: typ}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func lsTree(t *testing.T, repo, commit string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+repo, "ls-tree", "-r", "--name-only", commit).Output() //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

var app = map[string]string{ //nolint:gosec // fixture content, not credentials
	"apps/web/package.json":        `{"name":"claims-portal"}`,
	"apps/web/src/lib/payments.ts": `const stripe = new Stripe(process.env.STRIPE_SECRET_KEY);`,
	".env":                         "STRIPE_SECRET_KEY=sk_live_real",
	"apps/web/node_modules/x/y.js": "module.exports = 1",
	".git/HEAD":                    "ref: refs/heads/main",
	"apps/web/dist/bundle.js":      "built",
	"dist/bundle.js":               "built", // the root's build output; apps/web/dist is a source directory to the list (contracts/snapshot.md)
	".env.example":                 "STRIPE_SECRET_KEY=",
	".envrc":                       "export AWS_SECRET_ACCESS_KEY=x", // direnv's: a secrets file, never sent
}

func TestExclusionsAndHistoryNeverReachTheCommit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "claims-portal.git")
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, app)), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	files := lsTree(t, repo, pkg.Commit)
	for _, banned := range []string{".env\n", "node_modules", ".git/", "\ndist/"} {
		if strings.Contains("\n"+files, banned) {
			t.Fatalf("%q reached the commit:\n%s", banned, files)
		}
	}
	if !strings.Contains(files, "apps/web/src/lib/payments.ts") || !strings.Contains(files, "apps/web/dist/bundle.js") || !strings.Contains(files, ".env.example") || pkg.Files != 4 || !strings.HasPrefix(pkg.ID, "pkg_") || len(pkg.TreeHash) != 40 {
		t.Fatalf("package %+v files:\n%s", pkg, files)
	}
}

func TestSameTreeIsTheSamePackageAndDifferentTreesAreNot(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "claims-portal.git")
	first, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, app)), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, app)), exclusions, false)
	if err != nil || again.ID != first.ID || again.Commit != first.Commit {
		t.Fatalf("the same tree must be the same package with no new commit: %+v vs %+v (%v)", first, again, err)
	}
	changed := map[string]string{}
	for k, v := range app {
		changed[k] = v
	}
	changed["apps/web/src/lib/payments.ts"] = `const stripe = new Stripe(process.env.STRIPE_SECRET_KEY); // rotated`
	fixed, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, changed)), exclusions, false)
	if err != nil || fixed.ID == first.ID || fixed.Commit == first.Commit {
		t.Fatalf("a changed tree must be a new package: %+v vs %+v (%v)", first, fixed, err)
	}
	out, _ := exec.Command("git", "--git-dir="+repo, "rev-list", "--count", "main").Output() //nolint:gosec // test
	if strings.TrimSpace(string(out)) != "2" {
		t.Fatalf("two distinct packages, two commits; got %s", out)
	}
}

func TestRefusals(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	for name, entries := range map[string]map[string]string{
		"escape":   {"../evil": "x"},
		"absolute": {"/etc/passwd": "x"},
	} {
		if _, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, entries)), exclusions, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	if _, err := Ingest(context.Background(), repo, &b, exclusions, false); err == nil {
		t.Error("symlink: accepted")
	}
}

func TestUploadedGitMetadataDecidesNothing(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	crlf := "line one\r\nline two\r\n"
	tree := map[string]string{
		".gitignore":           "apps/\n",         // must not hide apps/
		".gitattributes":       "* text eol=lf\n", // must not rewrite bytes
		"apps/web/index.ts":    "export {}",
		"apps/web/windows.txt": crlf,
	}
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, tree)), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	files := lsTree(t, repo, pkg.Commit)
	if !strings.Contains(files, "apps/web/index.ts") || !strings.Contains(files, ".gitignore") {
		t.Fatalf("a .gitignore in the upload hid files from the gate:\n%s", files)
	}
	blob, err := exec.Command("git", "--git-dir="+repo, "show", pkg.Commit+":apps/web/windows.txt").Output() //nolint:gosec // test
	if err != nil || string(blob) != crlf {
		t.Fatalf("a .gitattributes in the upload rewrote bytes: %q (%v)", blob, err)
	}
}

func TestATreeIsOnePackageWhereverItSitsInHistory(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	a := map[string]string{"a.txt": "A"}
	b := map[string]string{"b.txt": "B"}
	first, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, a)), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, b)), exclusions, false); err != nil {
		t.Fatal(err)
	}
	again, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, a)), exclusions, false)
	if err != nil || again.ID != first.ID || again.Commit != first.Commit {
		t.Fatalf("A after B must be the package A already was: %+v vs %+v (%v)", first, again, err)
	}
	out, _ := exec.Command("git", "--git-dir="+repo, "rev-list", "--count", "main").Output() //nolint:gosec // test
	if strings.TrimSpace(string(out)) != "2" {
		t.Fatalf("two distinct trees, two commits; got %s", out)
	}
}

func TestConcurrentUploadsAllLand(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	var wg sync.WaitGroup
	pkgs := make([]Package, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pkgs[i], errs[i] = Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"f.txt": fmt.Sprint(i)})), exclusions, false)
		}()
	}
	wg.Wait()
	out, _ := exec.Command("git", "--git-dir="+repo, "rev-list", "main").Output() //nolint:gosec // test
	for i := range 8 {
		if errs[i] != nil {
			t.Fatalf("upload %d: %v", i, errs[i])
		}
		if !strings.Contains(string(out), pkgs[i].Commit) {
			t.Fatalf("upload %d's commit %s is not on main — a concurrent upload overwrote it", i, pkgs[i].Commit[:7])
		}
	}
}

func TestSparseArchivesAreRefused(t *testing.T) {
	f, err := os.Open("../executor/testdata/sparse.tar") // 19 KiB that expands to 64 MiB
	if err != nil {
		t.Skip("shared fixture not present")
	}
	defer func() { _ = f.Close() }()
	if _, err := Ingest(context.Background(), filepath.Join(t.TempDir(), "p.git"), f, exclusions, false); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("expanded a sparse archive: %v", err)
	}
}

func TestConcurrentUploadsOfOneTreeAreOnePackage(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	var wg sync.WaitGroup
	pkgs := make([]Package, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pkgs[i], errs[i] = Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, app)), exclusions, false)
		}()
	}
	wg.Wait()
	for i := range 8 {
		if errs[i] != nil {
			t.Fatalf("upload %d: %v", i, errs[i])
		}
		if pkgs[i] != pkgs[0] {
			t.Fatalf("one tree uploaded eight times at once must be one package: upload %d got %+v, upload 0 got %+v", i, pkgs[i], pkgs[0])
		}
	}
	out, _ := exec.Command("git", "--git-dir="+repo, "rev-list", "--count", "main").Output() //nolint:gosec // test
	if strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("one tree uploaded eight times at once made %s commits", strings.TrimSpace(string(out)))
	}
}

func TestAnExcludedSparseEntryIsRefusedToo(t *testing.T) {
	f, err := os.Open("../executor/testdata/sparse.tar") // one entry, "hole": 19 KiB that expands to 64 MiB
	if err != nil {
		t.Skip("shared fixture not present")
	}
	defer func() { _ = f.Close() }()
	_, err = Ingest(context.Background(), filepath.Join(t.TempDir(), "p.git"), f, []string{"hole"}, false)
	if err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("an excluded entry is drained, not written, but a sparse one must still be refused rather than expanded: %v", err)
	}
}

// check reads the unpacked tree; its error comes back as it is, and main stays where it was.
func TestARefusingCheckLeavesTheRepositoryAsItWas(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	first, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"a.txt": "A"})), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	var saw string
	_, err = IngestChecked(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"canyon.toml": "x"})), exclusions, false, true, func(tree string) error {
		body, _ := os.ReadFile(filepath.Join(tree, "canyon.toml")) //nolint:gosec // test: the tree Ingest unpacked
		saw = string(body)
		return refused
	})
	if !errors.Is(err, refused) || saw != "x" {
		t.Fatalf("the check's error and the tree it read: %v, %q", err, saw)
	}
	if main, err := git(context.Background(), repo, "", "rev-parse", "main"); err != nil || strings.TrimSpace(main) != first.Commit {
		t.Fatalf("main moved to %s (%v), want %s", main, err, first.Commit)
	}
}

func TestARealRefFailureIsReportedNotRetriedAsMovement(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	repo := filepath.Join(t.TempDir(), "p.git")
	if _, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"a.txt": "A"})), exclusions, false); err != nil {
		t.Fatal(err)
	}
	heads := filepath.Join(repo, "refs", "heads")
	if err := os.Chmod(heads, 0o500); err != nil { //nolint:gosec // test: a read-only refs dir
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(heads, 0o700) }) //nolint:gosec // test: restore so TempDir can be removed
	_, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"b.txt": "B"})), exclusions, false)
	if err == nil || strings.Contains(err.Error(), "kept moving") || !strings.Contains(err.Error(), "ermission denied") {
		t.Fatalf("a read-only refs dir must surface git's own error, not a story about main moving: %v", err)
	}
}

func TestTheIndexNeverLandsInTheTree(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"a.txt": "A", ".canyon-index": "mine"})), exclusions, false)
	if err != nil {
		t.Fatalf("an uploaded file named like git's index is just a file: %v", err)
	}
	files := lsTree(t, repo, pkg.Commit)
	if files != ".canyon-index\na.txt\n" {
		t.Fatalf("git's own index must live outside the tree; got:\n%s", files)
	}
}

func TestArchivesFromGitArchiveAndTarAreAccepted(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range []*tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "0123abcd"}}, // git archive
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},                                                                   // tar -cf x .
		{Name: "./a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tw.Write([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	pkg, err := Ingest(context.Background(), filepath.Join(t.TempDir(), "p.git"), &b, exclusions, false)
	if err != nil || pkg.Files != 1 {
		t.Fatalf("git archive's global header and tar's ./ root are what real archives carry: %+v (%v)", pkg, err)
	}
}

func TestBadEntriesAreRequestErrorsNamingTheEntryNotTheServer(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	for name, entries := range map[string]map[string]string{
		"file then directory": {"a": "x", "a/b": "y"},
		"name too long":       {strings.Repeat("n", 300): "x"},
	} {
		_, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, entries)), exclusions, false)
		if !errors.Is(err, ErrRequest) || strings.Contains(err.Error(), "canyon-ingest") {
			t.Errorf("%s: want a request error naming the entry only, got: %v", name, err)
		}
	}
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{".GIT/HEAD": "ref: refs/heads/main", "a.txt": "A"})), exclusions, false)
	if err != nil || lsTree(t, repo, pkg.Commit) != "a.txt\n" {
		t.Fatalf(".GIT is .git to git itself, so it is skipped like .git, not committed and not fatal: %+v (%v)", pkg, err)
	}
}

func TestAMalformedExclusionFailsClosed(t *testing.T) {
	_, err := Ingest(context.Background(), filepath.Join(t.TempDir(), "p.git"), bytes.NewReader(tarOf(t, map[string]string{"a.txt": "A"})), []string{"[unclosed"}, false)
	if err == nil || !strings.Contains(err.Error(), "[unclosed") {
		t.Fatalf("a bad glob in the published exclusion list must not silently exclude nothing: %v", err)
	}
}

func TestGitDoesNotInheritTheServersEnvironment(t *testing.T) {
	t.Setenv("GIT_TRACE", "1") // inherited, this puts trace lines into every git output we parse
	pkg, err := Ingest(context.Background(), filepath.Join(t.TempDir(), "p.git"), bytes.NewReader(tarOf(t, map[string]string{"a.txt": "A"})), exclusions, false)
	if err != nil || len(pkg.TreeHash) != 40 || len(pkg.Commit) != 40 {
		t.Fatalf("git children must get an explicit environment, not the server's: %+v (%v)", pkg, err)
	}
}

func TestExpansionIsCaughtOnTheLastRead(t *testing.T) {
	in := &countingReader{r: strings.NewReader("")} // the archive supplied nothing
	e := &expansion{r: bytes.NewReader(make([]byte, sparseSlack+1)), in: in}
	err := e.copy(io.Discard, &tar.Header{Name: "hole", Size: sparseSlack + 1})
	if err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("an entry that crosses the slack on its final read must still be refused: %v", err)
	}
}

func TestExtractMaterialisesThePackageTree(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "claims-portal.git")
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, app)), exclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Extract(context.Background(), repo, pkg.Commit, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "apps/web/src/lib/payments.ts")) //nolint:gosec // test: a path under t.TempDir()
	if err != nil || string(got) != app["apps/web/src/lib/payments.ts"] {
		t.Fatalf("the gate reads what was uploaded: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("an excluded file came back out of the repository")
	}
	if err := Extract(context.Background(), repo, "0000000000000000000000000000000000000000", t.TempDir()); err == nil {
		t.Fatal("a commit the repository does not have is an error")
	}
	if err := Extract(context.Background(), repo, "abc", t.TempDir()); err == nil || !strings.Contains(err.Error(), "abc") {
		t.Fatalf("a malformed commit is an error naming it, never a panic: %v", err)
	}
}

// routes.md refuses a package *over* 100,000 entries, so exactly 100,000 is one this takes. Every
// entry is excluded, so the count is all it exercises and nothing reaches the disk.
func TestTheEntryCapIsOverTheCapAndNotAtIt(t *testing.T) {
	names := func(n int) map[string]string {
		m := make(map[string]string, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("node_modules/f%06d", i)] = ""
		}
		return m
	}
	if _, err := Untar(bytes.NewReader(tarOf(t, names(maxEntries))), t.TempDir(), exclusions, false); err != nil {
		t.Fatalf("exactly %d entries was refused: %v", maxEntries, err)
	}
	over, err := Untar(bytes.NewReader(tarOf(t, names(maxEntries+1))), t.TempDir(), exclusions, false)
	if over.Files > maxEntries { // the refusal comes before the entry past the cap is written, not after
		t.Fatalf("%d files landed before the cap fired", over.Files)
	}
	if !errors.Is(err, ErrRequest) || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("%d entries was accepted: %v", maxEntries+1, err)
	}
}

// Untar over an existing tree reproduces the package's modes: O_CREATE keeps an existing file's
// mode, so the executable bit a later package adds, or drops, has to be applied explicitly.
func TestUntarOverAnExistingTreeReproducesThePackagesModes(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Untar(bytes.NewReader(tarOf(t, map[string]string{"bin/start": "#!/bin/sh"})), dir, nil, false); err != nil {
		t.Fatal(err)
	}
	must(os.Chmod(filepath.Join(dir, "bin", "start"), 0o755)) //nolint:gosec // test: the mode a re-untar must overwrite
	if _, err := Untar(bytes.NewReader(tarOf(t, map[string]string{"bin/start": "#!/bin/sh"})), dir, nil, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "bin", "start"))
	must(err)
	if info.Mode().Perm() != 0o600 { // the package says 0644, which lands as 0600: no exec bit, whatever was there
		t.Fatalf("a re-untar kept the old mode: %o", info.Mode().Perm())
	}
}

// A symlink already in the target directory, planted before the unpack, must not lead a write
// outside it: the tar's own symlinks are refused, and the directory's are not followed either.
func TestUntarDoesNotFollowASymlinkAlreadyInTheDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "bin")); err != nil {
		t.Fatal(err)
	}
	_, err := Untar(bytes.NewReader(tarOf(t, map[string]string{"bin/exploit": "x"})), dir, nil, false)
	if err == nil {
		t.Fatal("an entry under a symlinked directory was written")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "exploit")); statErr == nil {
		t.Fatal("the entry landed outside the directory")
	}
}

// tarInOrder writes the entries in the order given, as GitHub does: the root directory first.
func tarInOrder(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, name := range names {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(name)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			h.Size, h.Typeflag = 0, tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(name)[:h.Size]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDropRootLandsAGitHubTarballAtTheTop(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	gh := tarInOrder(t, "acme-web-1a2b3c/", "acme-web-1a2b3c/package.json", "acme-web-1a2b3c/src/", "acme-web-1a2b3c/src/index.ts")
	pkg, err := Ingest(context.Background(), repo, bytes.NewReader(gh), exclusions, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := lsTree(t, repo, pkg.Commit); got != "package.json\nsrc/index.ts\n" {
		t.Fatalf("the archive's root was kept:\n%s", got)
	}
}

// The exclusions see the path after the root is gone: a root named like an excluded segment
// excludes nothing, and an excluded segment under it is still excluded.
func TestDropRootExclusionsMatchTheStrippedPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := Untar(bytes.NewReader(tarInOrder(t, "dist/", "dist/a.txt", "dist/.env", "dist/node_modules/x.js", "dist/dist/x.js", "dist/src/dist/y.js")), dir, exclusions, true); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if strings.Join(got, ",") != "a.txt,src/dist/y.js" { // the repo root's dist stays home, a source dist travels
		t.Fatalf("exclusions matched the wrong paths: %v", got)
	}
}

func TestDropRootRefusesAnEntryOutsideTheRoot(t *testing.T) {
	for name, archive := range map[string][]byte{
		"two roots":     tarInOrder(t, "acme-web-1a2b3c/", "acme-web-1a2b3c/a.txt", "other-9f8e7d/b.txt"),
		"a file at top": tarInOrder(t, "README.md", "acme-web-1a2b3c/a.txt"),
	} {
		if _, err := Untar(bytes.NewReader(archive), t.TempDir(), nil, true); !errors.Is(err, ErrRequest) || !strings.Contains(err.Error(), "not under the archive's root") {
			t.Errorf("%s: accepted: %v", name, err)
		}
	}
}

// The list as the snapshot reads it (contracts/snapshot.md, exclusions): a name matches at any depth,
// a /name at the root alone, and a !name lets through what another matched. The probe apps of #1246
// lost internal/build and probeapp/build to the old any-depth dist and build, and .env.example to .env*.
func TestTheListMatchesBySegmentRootAndException(t *testing.T) {
	for p, want := range map[string]bool{
		".env": true, "apps/web/.env": true, ".env.local": true, "apps/web/.env.production": true,
		".env.example": false, "apps/web/.env.example": false, ".env.example.local": true, ".env.example/.env": true,
		".envrc": true, ".envs/.production/.django": true, ".env-production": true, ".env~": true, "node_modules/.env.example": true,
		"dist/bundle.js": true, "build/index.html": true,
		"internal/build/info.go": false, "probeapp/build/__init__.py": false, "apps/web/dist/x.js": false, "web/dist": false,
		"apps/web/node_modules/x/y.js": true, "node_modules": true, ".venv/bin/python": true, "a/.git/HEAD": true,
	} {
		if got := Excluded(p, exclusions); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", p, got, want)
		}
	}
}

// A folder's package is its entries alone, at its root, with the caps counted on it; the rest of the
// archive is read past, never written, and bounded on its own (#1917).
func TestAFolderIsItsOwnPackageAndItsArchiveIsBounded(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "p.git")
	archive := tarOf(t, map[string]string{"r/": "", "r/apps/web/index.js": "1", "r/apps/web2/x.js": "2", "r/apps/web/../../top.js": "3", "r/libs/a.js": "4"})
	pkg, err := IngestFolder(context.Background(), repo, bytes.NewReader(archive), exclusions, "apps/web", true, nil)
	if err != nil || pkg.Files != 1 || pkg.Bytes != 1 {
		t.Fatalf("the folder's package: %+v %v", pkg, err)
	}
	if out, err := exec.Command("git", "--git-dir="+repo, "ls-tree", "-r", "--name-only", pkg.Commit).Output(); err != nil || string(out) != "index.js\n" { //nolint:gosec // test: its own repository
		t.Fatalf("the folder at the package's root: %q %v", out, err)
	}
	was := [2]int64{maxArchiveBytes, maxArchiveEntries}
	t.Cleanup(func() { maxArchiveBytes, maxArchiveEntries = was[0], was[1] })
	maxArchiveBytes, maxArchiveEntries = 1<<20, 3
	if _, err := IngestFolder(context.Background(), repo, bytes.NewReader(archive), exclusions, "apps/web", true, nil); !errors.Is(err, ErrRequest) || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("past the archive's entries: %v", err)
	}
	maxArchiveBytes, maxArchiveEntries = 3, 100
	if _, err := IngestFolder(context.Background(), repo, bytes.NewReader(archive), exclusions, "apps/web", true, nil); !errors.Is(err, ErrRequest) || !strings.Contains(err.Error(), "more than 3 bytes") {
		t.Errorf("past the archive's bytes: %v", err)
	}
}
