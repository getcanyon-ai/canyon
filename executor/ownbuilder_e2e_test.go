//go:build e2e

package executor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A build with no build values on canyon-builder builds on its own (#177): pack makes no ephemeral copy, the build
// still sees the executor's two build variables, and a build value naming one of them replaces it, as before, on
// canyon-builder. The rebase tests build on it too, so they hold that the image records canyon-run:noble.
func TestEndToEndTheFullBuilderBuildsWithoutAnEphemeralCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"v","version":"1.0.0","private":true,"scripts":{"build":"echo NODE_ENV=$NODE_ENV TRACE=$NEXT_PRIVATE_OUTPUT_TRACE_ROOT","start":"node server.js"}}`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "server.js"), []byte("require('http').createServer((q, s) => s.end('ok')).listen(process.env.PORT || 8080);\n"), 0o600))
	e := &Executor{Join: []string{}} // no Node builder: canyon-builder's
	for _, c := range []struct {
		values    map[string]string
		saw       string
		ephemeral bool
	}{
		{nil, "NODE_ENV=production TRACE=/", false},
		{map[string]string{"NODE_ENV": "development"}, "NODE_ENV=development TRACE=/", true},
	} {
		var log bytes.Buffer
		since := time.Now()
		res, err := e.Build(ctx, BuildRequest{App: "own-builder", Package: "pkg_e2eo", BuildValues: c.values}, treeOf(t, dir), &log)
		if err != nil {
			t.Fatalf("build values %v: %v\n%s", c.values, err, tail(log.String(), 60))
		}
		_ = exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/own-builder@")).Run()
		if copied := ephemeralSince(t, since); !strings.Contains(log.String(), c.saw) || copied != c.ephemeral {
			t.Fatalf("build values %v: want %q, and an ephemeral builder %v, got %v:\n%s", c.values, c.saw, c.ephemeral, copied, tail(log.String(), 60))
		}
	}
}

// ephemeralSince is whether pack saved an ephemeral builder to the daemon since then: it names one
// pack.local/builder/<hex>:latest (createEphemeralBuilder, pack 0.40.3), which Docker's image events show. pack's log
// says nothing of it at its default verbosity.
func ephemeralSince(t *testing.T, since time.Time) bool {
	t.Helper()
	out, err := exec.Command("docker", "events", "--since", strconv.FormatInt(since.Unix()-1, 10), "--until", strconv.FormatInt(time.Now().Unix()+1, 10),
		"--filter", "type=image", "--format", "{{.Action}} {{.Actor.ID}} {{json .Actor.Attributes}}").Output()
	if err != nil {
		t.Fatalf("docker events: %v", err)
	}
	return strings.Contains(string(out), "pack.local/builder/")
}
