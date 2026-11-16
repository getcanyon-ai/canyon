package executor

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// POST /stop removes <app>-<target> and the one a redeploy set aside, and nothing else, and a second stop is the same success as the
// first (contracts/executor.md invariant 6). A name the patterns refuse never reaches docker; a
// daemon that fails is a 502, never a success.
func TestHTTPStopRemovesOneContainerIdempotentlyAndRefusesBeforeDocker(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	for range 2 {
		must(t, c.Stop(context.Background(), "claims-portal", "preview"))
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	// then the holders no container names, of which this daemon lists none, and the app's network, which it lacks
	stop := "rm\n-f\nclaims-portal-preview\nclaims-portal-preview-previous\n" + holdersLs("claims-portal") + "network\ninspect\n--format\n{{json .}}\ncanyon-app-claims-portal\n"
	if string(argv) != strings.Repeat(stop, 2) {
		t.Fatalf("each stop is one forced remove of that env's container and the one set aside: argv %q", argv)
	}
	must(t, os.Remove(filepath.Join(dir, "argv")))
	for name, body := range map[string]string{
		"staging target": `{"app":"claims-portal","target":"staging"}`,
		"flag as a name": `{"app":"--all","target":"preview"}`,
		"trailing data":  `{"app":"claims-portal","target":"preview"} {}`,
	} {
		resp, err := srv.Client().Post(srv.URL+"/stop", "application/json", strings.NewReader(body))
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "argv")); !os.IsNotExist(err) {
		t.Fatalf("a refused stop reached docker: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho 'Cannot connect to the Docker daemon' >&2; exit 1\n"), 0o700)) //nolint:gosec // test fake
	if err := c.Stop(context.Background(), "claims-portal", "prod"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a daemon that fails: %v, want the 502", err)
	}
}
