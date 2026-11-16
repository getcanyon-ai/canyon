//go:build cluster

package cluster

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// aliceSignsIn is M1's own device flow (login_test.go's login), the precondition every later
// case needs: a session both the CLI (by config home) and a raw request (by cookie) can use. The
// code is approved in a browser by e2e/ui/harness/confirm-device.ts, run as alice (her password
// from the environment, never on argv), so the flow completes on the test cluster unattended.
//
// home is the caller's, made with the t that outlives every case using the session: TestSuite
// runs M1 as a subtest, and a t.TempDir() made here would go with that subtest -- M2's CLI then
// ran with no config and read the server's 401 as a session that had expired.
//
// The one copy: TestMaker, TestSecretsAndScanners and TestSuite all call this — it lives here, in
// the harness, not duplicated per test file.
func aliceSignsIn(t *testing.T, home string) session {
	t.Helper()
	return deviceSignIn(t, home, "alice")
}

// deviceSignIn is the device flow as one of the test cluster's three people: the admin's session is what
// G1 and G2 need to turn a scanner on (PUT /api/v1/policies/sarif.<tool> is admin-only).
func deviceSignIn(t *testing.T, home, user string) session {
	t.Helper()
	code, _, wait := login(t, home)
	confirmDevice(t, user, code)
	out, exit := wait()
	if exit != 0 {
		t.Fatalf("canyon login: exit %d after the code was approved:\n%s", exit, out)
	}
	body, err := os.ReadFile(filepath.Join(home, "canyon", "config.json")) //nolint:gosec // this test's own temp config home
	if err != nil {
		t.Fatalf("reading the CLI's config after login: %v", err)
	}
	var cfg struct{ Session string }
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("the CLI's config is not JSON: %v", err)
	}
	return session{home: home, cookie: cfg.Session}
}

// browserSignIn is user signed in through a browser, as the dashboard is: approving a blocking finding takes
// a browser session (#2204), which a device session never is. The cookie comes back on stdout and is never logged.
func browserSignIn(t *testing.T, user string) session {
	t.Helper()
	cmd := exec.Command("node", "harness/browser-session.ts", user) //nolint:gosec // the harness's own script and a user name
	cmd.Dir = "../ui"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("browser-session %s: %v\n%s", user, err, stderr.String())
	}
	return session{home: t.TempDir(), cookie: string(out)}
}

// confirmDevice approves code on Canyon's device screen as user, in a browser of its own
// (e2e/ui/harness/confirm-device.ts). Its output is the test's log: it holds the user and the
// code, never a password.
func confirmDevice(t *testing.T, user, code string) {
	t.Helper()
	cmd := exec.Command("node", "harness/confirm-device.ts", user, code) //nolint:gosec // the harness's own script, a user name and a device code
	cmd.Dir = "../ui"
	out, err := cmd.CombinedOutput()
	t.Logf("confirm-device %s:\n%s", user, out)
	if err != nil {
		t.Fatalf("confirm-device %s: %v", user, err)
	}
}
