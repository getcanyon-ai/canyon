package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// buildThrough runs a /build of tree on e through Handler and Client, as the control plane does.
func buildThrough(t *testing.T, e *Executor, tree []byte) error {
	t.Helper()
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	_, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tree),
		func(context.Context, string) error { return nil })
	return err
}

// The rest of the host's own /build faults are Canyon's, never the package's (#2537): each ends the stream on
// the executor's own row, which the client answers HostFailed, never BuildFailed, so the run records error.
func TestTheOtherFaultsOfTheHostAreCanyonsOwn(t *testing.T) {
	app := map[string]string{"package.json": "{}"}
	for name, c := range map[string]struct {
		builder string // CANYON_BUILDER, "" for canyon-builder
		docker  string
		says    string
	}{
		"canyon-run not built":     {"", `[ "$1" = build ] && case "$*" in *canyon-run*) echo 'no space left on device' >&2; exit 1;; esac`, "canyon-run"},
		"canyon-builder not built": {"", `[ "$1" = build ] && case "$*" in *canyon-builder*) echo 'failed to solve' >&2; exit 1;; esac`, "canyon-builder"},
		"CANYON_BUILDER not pulled": {"registry.example/builder:1", `case "$*" in *"io.buildpacks.stack.id"*registry.example/builder:1*) exit 1;; esac
[ "$1" = pull ] && { echo 'manifest unknown' >&2; exit 1; }`, "builder registry.example/builder:1"},
		"stacks that differ": {"", `case "$*" in *"io.buildpacks.stack.id"*canyon-builder*) echo io.buildpacks.stacks.jammy; exit 0;; *"io.buildpacks.stack.id"*) echo io.buildpacks.stacks.noble; exit 0;; esac`,
			"builds on stack"},
		"the builder's user unread": {"", `case "$*" in *"{{.Config.User}}"*) echo 'Cannot connect to the Docker daemon' >&2; exit 1;; esac`, "builder canyon-builder"},
		"the built image unread":    {"", `case "$*" in *"{{.Id}}"*canyon/claims-portal:*) echo 'No such image' >&2; exit 1;; esac`, "image id"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := fakes(t)
			e.Builder = c.builder
			withDocker(t, e, c.docker)
			err := buildThrough(t, e, tarOf(t, app))
			var failed *BuildFailed
			var host *HostFailed
			if !errors.As(err, &host) || errors.As(err, &failed) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v (read as the package's: %t), want Canyon's own failure saying %q", err, errors.As(err, &failed), c.says)
			}
		})
	}
}

// The package's own /build failures stay the package's (#2537): its build exiting, a tree that does not read,
// and a project.toml Canyon refuses are each BuildFailed, never the host's.
func TestThePackagesOwnFailuresStayItsBuilds(t *testing.T) {
	for name, c := range map[string]struct {
		pack string
		tree []byte
		says string
	}{
		"its build step exits":          {`echo '` + lifecycleFailed + `51'; exit 1`, tarOf(t, map[string]string{"package.json": "{}"}), "the build step exited 51"},
		"a tree that is no tar":         {"exit 0", []byte("not a tar at all, and longer than a tar's header is not"), "tree"},
		"a project.toml Canyon refuses": {"exit 0", tarOf(t, map[string]string{"package.json": "{}", "project.toml": "[_]\nschema-version = \"0.2\"\nunknown = 1\n"}), "project.toml: "},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := fakes(t)
			must(t, os.WriteFile(e.Pack, []byte("#!/bin/sh\n"+c.pack+"\n"), 0o700)) //nolint:gosec // test fake
			err := buildThrough(t, e, c.tree)
			var failed *BuildFailed
			var host *HostFailed
			if !errors.As(err, &failed) || errors.As(err, &host) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v (read as the host's: %t), want the package's own failure saying %q", err, errors.As(err, &host), c.says)
			}
		})
	}
}
