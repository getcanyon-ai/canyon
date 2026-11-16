//go:build ignore

// vendor writes sast-rules.tar: GitLab's sast-rules at the pinned tag and commit, the rule files of
// its permissive folders only, and their licences. Folders, never file headers, decide what ships:
// the upstream README says a folder's licence is everything beneath it. It writes the same bytes from
// the same commit — a plain tar, so no compressor's version can move them — so a diff after
// `make vendor-sast-rules` means upstream moved.
//
//	go run gate/opengrep/vendor.go <tag> <commit> <out>
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const repo = "https://gitlab.com/gitlab-org/security-products/sast-rules.git"

// shipped are the folders whose rules ship, by the licence of the folder: MIT (python, java, javascript,
// csharp, scala), Apache-2.0 (go, from gosec) and LGPL-3.0, unmodified (rules/lgpl, from njsscan,
// FindSecBugs and mobsfscan). Left out: c (GPL-2.0), rules/lgpl-cc (Commons Clause) and rules/gitlab
// (GitLab EE), whatever a file's own header says, and rules/lgpl's oc and swift, iOS apps Canyon does
// not build.
var shipped = []string{"python", "go", "java", "javascript", "csharp", "scala", "rules/lgpl/javascript", "rules/lgpl/java", "rules/lgpl/kotlin"}

// texts are the licence texts the folders name and the repository does not carry: the GPL-3.0 the
// LGPL-3.0 incorporates, and the Apache-2.0 of go/'s gosec rules — SPDX's, at the commit of
// license-list-data v3.29.0, by their sha256.
var texts = []struct{ name, url, sha256 string }{
	{"COPYING.GPL-3.0", "https://raw.githubusercontent.com/spdx/license-list-data/31ba1a50e5397e00a304dbadc76531740e89ee48/text/GPL-3.0-only.txt", "fb981668c18a279e285fc4d83fba1e836cc84dd4daa73c9697d3cfd2d8aca6e0"},
	{"LICENSE.Apache-2.0", "https://raw.githubusercontent.com/spdx/license-list-data/31ba1a50e5397e00a304dbadc76531740e89ee48/text/Apache-2.0.txt", "074e6e32c86a4c0ef8b3ed25b721ca23aca83df277cd88106ef7177c354615ff"},
}

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: go run gate/opengrep/vendor.go <tag> <commit> <out>")
	}
	if err := vendor(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		log.Fatal(err)
	}
}

func vendor(tag, commit, out string) error {
	dir, err := os.MkdirTemp("", "sast-rules-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// core.autocrlf off: the checkout is the commit's bytes, whatever the caller's git config says.
	if err := run("git", "-c", "advice.detachedHead=false", "-c", "core.autocrlf=false", "clone", "--quiet", "--depth", "1", "--branch", tag, repo, dir); err != nil {
		return err
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != commit {
		return fmt.Errorf("%s is %s, not the pinned %s (%v)", tag, strings.TrimSpace(string(head)), commit, err)
	}
	files := map[string][]byte{}
	for _, name := range []string{"LICENSE", "rules/lgpl/LICENSE"} {
		if files[name], err = os.ReadFile(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for _, t := range texts {
		if files[t.name], err = fetch(t.url, t.sha256); err != nil {
			return err
		}
	}
	for _, folder := range shipped {
		err := filepath.WalkDir(filepath.Join(dir, folder), func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || !slices.Contains([]string{".yml", ".yaml"}, path.Ext(p)) {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err == nil {
				files[filepath.ToSlash(rel)], err = os.ReadFile(p)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	b, err := archive(files)
	if err == nil {
		err = os.WriteFile(out, b, 0o644)
	}
	if err == nil {
		fmt.Printf("%s: %d files from sast-rules %s (%s)\n", out, len(files), tag, commit)
	}
	return err
}

// archive is files as a tar in name order, every header fixed, so the same files make the same bytes.
func archive(files map[string][]byte) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func fetch(url, sum string) ([]byte, error) {
	resp, err := (&http.Client{Timeout: time.Minute}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != sum {
		return nil, fmt.Errorf("%s has sha256 %s, not the pinned %s", url, got, sum)
	}
	return body, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
