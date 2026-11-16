package main

import (
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/cli"
)

// canyon login, run as the CLI runs, against a host whose auth is off: it connects although the host
// sets no cookie (contracts/routes.md: a caller without a session is the anonymous person, and nothing
// is stored), and the next verb is served (#1673).
func TestTheCLIsLoginAgainstAnAuthOffHost(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	run := func(args ...string) (string, int) {
		var out, errOut strings.Builder
		code := cli.Main(args, &out, &errOut, strings.NewReader(""), func(k string) string { return env[k] }, false)
		return out.String() + errOut.String(), code
	}
	if said, code := run("login", base); code != 0 || !strings.Contains(said, "Connected to "+base) {
		t.Fatalf("login exited %d:\n%s", code, said)
	}
	if said, code := run("fleet"); code != 0 {
		t.Fatalf("fleet after login exited %d:\n%s", code, said)
	}
}
