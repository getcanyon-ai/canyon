package executor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the executor makes for an app carries its own compose project as canyon.project (#178): the app's container,
// its network and its two build caches, which pack would make unlabelled; an executor with no project labels nothing
// so, and a down filters on the label alone.
func TestWhatAnAppIsMadeOfCarriesTheExecutorsProject(t *testing.T) {
	for _, project := range []string{"canyon-demo", ""} {
		e, dir := fakes(t)
		e.Project = project
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}); err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
		label := "--label\ncanyon.project=" + project + "\n"
		for _, made := range []string{
			"volume\ncreate\n--label\ncanyon.app=claims-portal\n" + label + "canyon-cache-claims-portal-build\n",
			"volume\ncreate\n--label\ncanyon.app=claims-portal\n" + label + "canyon-cache-claims-portal-launch\n",
			"--label\ncanyon.app=claims-portal\n--label\ncanyon.target=preview\n" + label,
			"--label\ncanyon.app=claims-portal\n" + label + "canyon-app-claims-portal\n",
		} {
			if project == "" {
				made = strings.Replace(made, label, "", 1)
			}
			if !strings.Contains(string(argv), made) {
				t.Errorf("project %q: docker argv lacks %q:\n%s", project, made, argv)
			}
		}
		if project == "" && strings.Contains(string(argv), "canyon.project") {
			t.Errorf("an executor with no project labelled something with one:\n%s", argv)
		}
		// pack can't label the image: it gets a second name in the project, which collect and Forget remove with the first
		named := strings.Contains(string(argv), "tag\ncanyon/claims-portal:pkg_7d3e8f-") && strings.Contains(string(argv), "\ncanyon-demo/claims-portal:pkg_7d3e8f-")
		if _, err := e.images(context.Background(), "claims-portal"); err != nil {
			t.Fatal(err)
		}
		argv, _ = os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
		listed := strings.Contains(string(argv), "--filter\nreference=canyon/claims-portal\n--filter\nreference=canyon-demo/claims-portal\n")
		if named != (project != "") || listed != (project != "") {
			t.Errorf("project %q: the image named in the project %v, listed by that name %v:\n%s", project, named, listed, argv)
		}
	}
}

// The executor's project is its own container's compose project label, found by its hostname; outside a container,
// or with Docker not answering, it has none.
func TestTheExecutorsProjectIsItsOwnContainers(t *testing.T) {
	host, err := os.Hostname()
	must(t, err)
	for want, answer := range map[string]string{"canyon-demo": `[ "$1 $2 $5" = "container inspect ` + host + `" ] && { echo canyon-demo; exit 0; }; exit 1`, "": "exit 1"} {
		docker := filepath.Join(t.TempDir(), "docker")
		must(t, os.WriteFile(docker, []byte("#!/bin/sh\n"+answer+"\n"), 0o700)) //nolint:gosec // a test fake must be executable
		if got := (&Executor{Docker: docker}).OwnProject(context.Background()); got != want {
			t.Errorf("own project %q, want %q", got, want)
		}
	}
}
