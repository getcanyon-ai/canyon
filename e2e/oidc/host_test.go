//go:build oidc

package oidc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	hostB = "http://localhost:7779" // canyon-b, e2e/compose.e2e.yml: sign-in off
	// oldRelease is the oldest release published on canyon-releases, and an older CLI than this one: it writes
	// config.json and each host's session under hosts/ in that release's shape.
	oldRelease = "v0.2.3"
	// releases is where every release is published, public and read with no token.
	releases = "https://github.com/getcanyon-ai/canyon-releases/releases/download/"
)

// TestAnOldCLIKeepsAnotherHostsSession is J14's sessions (design/OPERATIONS.md §2.5): the new CLI signs in to this
// Canyon (A), whose session is real, then to canyon-b, so config.json names B and A's session is in hosts/. An old
// release's CLI reads it and logs in to B: A's session survives, byte for byte, and still signs the new CLI in to A;
// config.json round-trips between the two.
func TestAnOldCLIKeepsAnotherHostsSession(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	poll(t, 2*time.Minute, "canyon-b to answer", func() bool {
		return get(t, &http.Client{Timeout: 10 * time.Second}, hostB+"/health", &struct{}{}) == http.StatusOK
	})
	browser, home, dir, old := signIn(t, admin), t.TempDir(), t.TempDir(), oldCLI(t)
	onboard(t, browser)
	run := func(bin, target string, args ...string) int { // under home, CANYON_HOST target ("" unset)
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // a CLI this test built or fetched, with the journey's arguments
		cmd.Dir = dir
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return slices.Contains([]string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT", "CANYON_HOST", "GH_TOKEN", "HOME", "XDG_CONFIG_HOME"}, name) // the CLIs need no token
		}), "HOME="+home, "XDG_CONFIG_HOME="+home, "CANYON_NO_UPDATE_NOTICE=1", "NO_COLOR=1")
		if target != "" {
			cmd.Env = append(cmd.Env, "CANYON_HOST="+target)
		}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("%s %v: %v", bin, args, err)
		}
		t.Logf("%s %s (CANYON_HOST %q) → exit %d\n%s", filepath.Base(filepath.Dir(bin)), strings.Join(args, " "), target, cmd.ProcessState.ExitCode(), out)
		return cmd.ProcessState.ExitCode()
	}
	file := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path) //nolint:gosec // the CLI's config under this test's home
		must(t, err)
		return string(b)
	}

	login(t, command(t, home, dir, "login", host), browser) // A, through the device grant the admin approves
	if code := run(cli, "", "login", hostB); code != 0 {
		t.Fatalf("login %s: exit %d", hostB, code)
	}
	configJSON, sessionA := filepath.Join(home, "canyon", "config.json"), filepath.Join(home, "canyon", "hosts", "http_localhost_7777.json")
	keptA := file(sessionA)
	if code := run(old, "", "fleet"); code != 0 { // the old CLI reads what the new one wrote
		t.Errorf("%s's fleet on the new CLI's config.json: exit %d", oldRelease, code)
	}
	if code := run(old, "", "login", hostB); code != 0 {
		t.Fatalf("%s's login to B: exit %d", oldRelease, code)
	}
	written := file(configJSON)
	if file(sessionA) != keptA {
		t.Error("the old CLI's login to B changed A's session")
	}
	if code := run(cli, "", "fleet"); code != 0 || file(configJSON) != written { // and the new one what the old one wrote, leaving it
		t.Errorf("the new CLI's fleet on %s's config.json: exit %d, the file kept: %t", oldRelease, code, file(configJSON) == written)
	}
	if code := run(cli, host, "fleet"); code != 0 { // A answers only a session: the one in hosts/
		t.Errorf("the new CLI's fleet on A with A's kept session: exit %d", code)
	}
}

// oldCLI is oldRelease's canyon for this machine, from its published release, checked against its SHA256SUMS.
func oldCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tarball := "canyon_" + oldRelease + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	raw := download(t, releases+oldRelease+"/"+tarball)
	sum := sha256.Sum256(raw)
	sums := download(t, releases+oldRelease+"/SHA256SUMS")
	if !slices.Contains(strings.Split(string(sums), "\n"), hex.EncodeToString(sum[:])+"  "+tarball) {
		t.Fatalf("%s is not the release's: its SHA256SUMS has no line for sha256 %x", tarball, sum)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	must(t, err)
	files := tar.NewReader(gz)
	for {
		h, err := files.Next()
		if err != nil {
			t.Fatalf("%s holds no canyon: %v", tarball, err)
		}
		if filepath.Base(h.Name) != "canyon" || h.Typeflag != tar.TypeReg {
			continue
		}
		bin := filepath.Join(dir, oldRelease, "canyon") // its directory names the release in run's log lines
		must(t, os.MkdirAll(filepath.Dir(bin), 0o750))
		body, err := io.ReadAll(files)
		must(t, err)
		must(t, os.WriteFile(bin, body, 0o700)) //nolint:gosec // an executable: the release's CLI
		return bin
	}
}

// download is one published file whole, or the test stops.
func download(t *testing.T, u string) []byte {
	t.Helper()
	res, err := (&http.Client{Timeout: 5 * time.Minute}).Get(u) //nolint:noctx // the client's timeout bounds it
	if err != nil {
		t.Fatalf("download %s: %v", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("download %s: %d %v", u, res.StatusCode, err)
	}
	return body
}
