package executor

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/ingest"
)

// The archive a build is handed is git archive's, which writes a pax global header before the
// first file. The tree the executor unpacks must be the package's tree, entry for entry: a
// synthetic tar is exactly what hid this, so the archive here is the real one.
func TestARealGitArchiveBuilds(t *testing.T) {
	e, dir := fakes(t)
	listed := filepath.Join(dir, "packed")
	pack := filepath.Join(dir, "pack-listing")
	must(t, os.WriteFile(pack, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in --path) shift; (cd \"$1\" && find . -type f | sed 's|^\\./||' | sort) > \""+listed+"\";; esac; shift; done\necho \"===> DETECTING\"; echo \"===> BUILDING\"; exit 0\n"), 0o700)) //nolint:gosec // a test fake must be executable
	e.Pack = pack
	repo := filepath.Join(t.TempDir(), "repo.git")
	pkg, err := ingest.Ingest(context.Background(), repo, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}", "src/index.ts": "export {}\n"})), nil, false)
	must(t, err)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(ingest.Archive(context.Background(), repo, pkg.Commit, pw)) }()
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	res, err := c.Build(context.Background(), BuildRequest{App: "claims-portal", Package: pkg.ID, BuildValues: map[string]string{}}, pr, func(context.Context, string) error { return nil })
	if err != nil || !strings.HasPrefix(res.Image, "canyon/claims-portal@sha256:") {
		t.Fatalf("a real git archive did not build: %+v %v", res, err)
	}
	packed, err := os.ReadFile(listed) //nolint:gosec // test dir
	if err != nil || strings.TrimSpace(string(packed)) != "package.json\nsrc/index.ts" {
		t.Errorf("pack was handed:\n%s(%v)\nwant package.json and src/index.ts, nothing else", packed, err)
	}
}
