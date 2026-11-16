package cli

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test: a path under t.TempDir()
	must(t, err)
	return string(body), statOf(t, path).Mode().Perm()
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// A plain file named .canyon at the root is the user's, and it must not swallow the rest of the
// package: SkipDir from a file skips its whole parent, which here is the root.
func TestAPlainFileNamedLikeTheBaseDirDoesNotSwallowThePackage(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, ".canyon"), []byte("not a directory"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "zeta.txt"), []byte("last"), 0o600))
	var out bytes.Buffer
	files, _, _, err := pack(dir, &out)
	must(t, err)
	tr, names := tar.NewReader(&out), []string{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if files != 1 || !strings.Contains(strings.Join(names, " "), "zeta.txt") { // .canyon itself is Canyon's name and is never packed; zeta must survive it
		t.Errorf("the package lost what sorts after .canyon: %d files, %v", files, names)
	}
}

// baseIn is the base readBase finds, failing the test on a base that can't be used.
func baseIn(t *testing.T, dir, app string) string {
	t.Helper()
	pkg, err := readBase(dir, app)
	must(t, err)
	return pkg
}

// The base record: written last and atomically, read back only when it names this app and its id
// passes the one regexp, cleared whole. A tampered file never reaches a header: one naming this app,
// or no app, can't be used (cli.md §6.6); one naming another app is no base.
func TestTheBaseRecordRoundTripsAndATamperedOneReadsAsNone(t *testing.T) {
	dir := t.TempDir()
	must(t, writeBase(dir, "claims-portal", "pkg_9f2a1c"))
	if body, mode := readFile(t, filepath.Join(dir, baseDir, baseFile)); body != "app     = \"claims-portal\"\npackage = \"pkg_9f2a1c\"\n" || mode != 0o600 {
		t.Fatalf("the base landed as %q at %v", body, mode)
	}
	if body, _ := readFile(t, filepath.Join(dir, baseDir, ".gitignore")); body != "*\n" {
		t.Fatalf(".canyon/.gitignore is %q", body)
	}
	if got := baseIn(t, dir, "claims-portal"); got != "pkg_9f2a1c" {
		t.Fatalf("read back %q", got)
	}
	if got := baseIn(t, dir, "other-app"); got != "" { // another app's base is no base
		t.Fatalf("another app's base read as %q", got)
	}
	for body, unusable := range map[string]bool{
		"app     = \"claims-portal\"\npackage = \"pkg_9f2a 1c\"\n":  true,
		"app     = \"claims-portal\"\npackage = \"pkg_9f2a\n1c\"\n": true,
		"package = \"pkg_9f2a1c\"\n":                                true,
	} {
		must(t, os.WriteFile(filepath.Join(dir, baseDir, baseFile), []byte(body), 0o600))
		if got, err := readBase(dir, "claims-portal"); got != "" || (err != nil) != unusable {
			t.Errorf("a tampered base %q read as %q, %v", body, got, err)
		}
	}
	if err := writeBase(dir, "claims-portal", "pkg_9f2a 1c"); err == nil {
		t.Fatal("an id with a space was written")
	}
	must(t, clearBase(dir))
	if exists(filepath.Join(dir, baseDir, baseFile)) {
		t.Fatal("clearBase left the file")
	}
}
