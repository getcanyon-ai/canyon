//go:build cluster

package cluster

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// login starts `canyon login $E2E_CANYON_URL` with its config in home and answers the code and the
// address it prints on stderr (cli/cmd_login.go deviceFlow), which a browser signed in as the person
// approves. wait blocks until the CLI exits — the code approved, denied or expired — and answers
// all it printed and its exit code. The CLI is stopped when the test ends.
func login(t *testing.T, home string) (code, uri string, wait func() (string, int)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, cli, "login", host) //nolint:gosec // the CLI this test built
	cmd.Env = cliEnv(t, home)
	var stdout, printed bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		t.Fatalf("canyon login: %v", err)
	}
	lines := bufio.NewScanner(io.TeeReader(stderr, &printed))
	for code == "" && lines.Scan() {
		code, uri, _ = deviceCode(printed.String())
	}
	rest := make(chan struct{})
	go func() {
		_, _ = io.Copy(&printed, stderr)
		close(rest)
	}()
	wait = func() (string, int) {
		t.Helper()
		<-rest
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Fatalf("canyon login: %v", err)
		}
		t.Logf("canyon login %s → exit %d\n%s%s", host, cmd.ProcessState.ExitCode(), stdout.String(), printed.String())
		return stdout.String() + printed.String(), cmd.ProcessState.ExitCode()
	}
	if code == "" {
		_, exit := wait()
		t.Fatalf("canyon login exited %d with no code to approve", exit)
	}
	return code, uri, wait
}

// deviceCode reads the user code and the verification address out of what deviceFlow writes to
// stderr: "\n  Your code:   <code>\n\n  Open <uri> and approve it.\n  The code expires in 10 minutes.\n\n  Waiting…\n".
func deviceCode(stderr string) (code, uri string, err error) {
	_, rest, found := strings.Cut(stderr, "Your code:")
	code, rest, _ = strings.Cut(rest, "\n")
	_, rest, _ = strings.Cut(rest, "Open ")
	uri, _, opened := strings.Cut(rest, " and approve it.")
	if code = strings.TrimSpace(code); !found || !opened || code == "" || uri == "" {
		return "", "", fmt.Errorf("no code and address in %q", stderr)
	}
	return code, uri, nil
}

func TestUnitDeviceCode(t *testing.T) {
	// cli/cmd_login.go deviceFlow's line, with a grant as RFC 8628 shapes one.
	stderr := fmt.Sprintf("\n  Your code:   %s\n\n  Open %s and approve it.\n  The code expires in 10 minutes.\n\n  Waiting%s\n", "WDJB-MJHT", "https://canyon.example.com/#/device?code=WDJB-MJHT", "…")
	code, uri, err := deviceCode(stderr)
	if err != nil || code != "WDJB-MJHT" || uri != "https://canyon.example.com/#/device?code=WDJB-MJHT" {
		t.Errorf("deviceCode is %q, %q, %v; want WDJB-MJHT, https://canyon.example.com/#/device?code=WDJB-MJHT", code, uri, err)
	}
	for name, partial := range map[string]string{
		"before the code": "",
		"before the URL":  "\n  Your code:   WDJB-MJHT\n",
	} {
		if code, uri, err := deviceCode(partial); err == nil {
			t.Errorf("%s: deviceCode is %q, %q; want an error", name, code, uri)
		}
	}
}
