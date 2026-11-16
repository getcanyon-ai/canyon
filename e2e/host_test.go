//go:build stack

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

const hostB = "http://localhost:7779" // canyon-b, e2e/compose.e2e.yml

// j14Host is a checkout's host guards (design/OPERATIONS.md §2.5): with J4–J7's app on the stack, a deploy of its
// checkout with CANYON_HOST=B refuses under guard (a), and B writes nothing; once B has an app of that name, guard
// (b) refuses, the checkout's base being the stack's, and B holds no package. An old release's CLI beside the new
// one is e2e/oidc's TestAnOldCLIKeepsAnotherHostsSession: a host with sign-in off keeps no session.
func j14Host(t *testing.T, core string) {
	waitFor(t, 2*time.Minute, "canyon-b to answer", func() bool {
		res, err := http.Get(hostB + "/health") //nolint:noctx // one probe of the local stack
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	deployToB := func() (string, int) { // in core, CANYON_HOST B
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, "deploy") //nolint:gosec // the CLI this test built
		cmd.Dir = core
		cmd.Env = append(slices.DeleteFunc(cliEnv(t), func(kv string) bool { return strings.HasPrefix(kv, "CANYON_HOST=") }), "CANYON_HOST="+hostB)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("canyon deploy: %v", err)
		}
		t.Logf("canyon deploy (CANYON_HOST %s) → exit %d\n%s", hostB, cmd.ProcessState.ExitCode(), out)
		return string(out), cmd.ProcessState.ExitCode()
	}

	// (a): B has no such app, so nothing is made or sent there.
	if out, code := deployToB(); code != 1 || !strings.Contains(out, coreApp+" isn't on "+hostB) {
		t.Errorf("deploy to B, which lacks the app: exit %d; want 1 and the refusal naming it", code)
	}
	var trail struct{ Events []json.RawMessage }
	onB(t, "GET", "/api/v1/audit?project="+coreApp+"&limit=10", nil, &trail)
	if len(trail.Events) != 0 {
		t.Fatalf("B's trail for %s after the refusal: %s; want no row", coreApp, trail.Events)
	}

	// (b): B has an app of that name, but not the package the checkout was last sent with.
	onB(t, "POST", "/api/v1/projects", map[string]string{"id": coreApp}, nil)
	if out, code := deployToB(); code != 1 || !strings.Contains(out, " isn't on "+hostB) || strings.Contains(out, coreApp+" isn't on") {
		t.Errorf("deploy to B with the stack's base: exit %d; want 1 and the base's refusal", code)
	}
	var packages struct{ Packages []json.RawMessage }
	onB(t, "GET", "/api/v1/projects/"+coreApp+"/packages", nil, &packages)
	if len(packages.Packages) != 0 {
		t.Errorf("B holds %d packages of %s after the refusal; want none", len(packages.Packages), coreApp)
	}
}

// onB is one request to canyon-b, which must answer 2xx, its JSON body decoded into out when out is not nil.
func onB(t *testing.T, method, path string, body, out any) {
	t.Helper()
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(raw)
	}
	code, got := send(t, method, hostB+path, "application/json", in)
	if code/100 != 2 {
		t.Fatalf("%s %s on B: %d %s", method, path, code, got)
	}
	if out != nil {
		if err := json.Unmarshal(got, out); err != nil {
			t.Fatalf("%s %s on B: %v", method, path, err)
		}
	}
}
