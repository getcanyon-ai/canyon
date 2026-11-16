package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/getcanyon-ai/canyon/ingest"
)

// The hash is the one the host mints, proven through the host's own ingest over the tar deploy
// sends: every case the tar can carry, and the ones git treats its own way — the directory "a"
// between the files "a-b" and "a.b" in git's order, an executable file, a directory under a
// directory, an empty directory and a symlink (neither travels), and the excluded names.
func TestTheTreeHashIsTheOneTheHostMints(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	writeFile(t, filepath.Join(dir, "a-b"), "a-b\n")
	writeFile(t, filepath.Join(dir, "a.b"), "a.b\n")
	writeFile(t, filepath.Join(dir, "a", "x"), "a/x\n")
	writeFile(t, filepath.Join(dir, "gradlew"), "#!/bin/sh\n")
	writeFile(t, filepath.Join(dir, "src", "app", "page.tsx"), "export default 1\n")
	writeFile(t, filepath.Join(dir, ".env"), "API_KEY=sk_live_filler\n")
	writeFile(t, filepath.Join(dir, "node_modules", "left", "index.js"), "left\n")
	writeFile(t, filepath.Join(dir, "only-excluded", ".env.local"), "X=1\n")
	writeFile(t, filepath.Join(dir, baseDir, baseFile), "app = \"claims-portal\"\npackage = \"pkg_7a1c\"\n")
	//nolint:gosec // test: 0755 on gradlew is the point
	must(t, os.Chmod(filepath.Join(dir, "gradlew"), 0o755), os.Mkdir(filepath.Join(dir, "empty"), 0o750),
		os.Symlink("a.b", filepath.Join(dir, "src", "link")))
	var tar bytes.Buffer
	if _, _, _, err := pack(dir, &tar); err != nil {
		t.Fatal(err)
	}
	pkg, err := ingest.Ingest(context.Background(), filepath.Join(t.TempDir(), "claims-portal.git"), &tar, packExclusions, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := treeHash(dir)
	if err != nil || got != pkg.TreeHash {
		t.Errorf("this directory hashes to %q (%v); the host names its package's tree %s", got, err, pkg.TreeHash)
	}
}
