// Package gitpush runs git on a bare repository as a token holder: the token rides in an http header set
// through git's environment, never in a URL, an argument or a log line, and never in an error it returns.
package gitpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// LowSpeed is git's floor on a transfer: slower than Bytes a second for Seconds, git gives up, so a remote that
// stalls never holds the deploy repo's lock past its bound (contracts/routes.md, The deploy repo).
var LowSpeed = struct{ Bytes, Seconds string }{"1024", "30"}

// waitDelay is how long Output waits on git's pipes once its ctx is done.
var waitDelay = 5 * time.Second

// Git is one bare repository and who acts on it.
type Git struct {
	Dir         string    // the bare repository, git's --git-dir
	User, Token string    // basic auth on every http request
	Name, Email string    // the author and committer
	Date        time.Time // the author and committer date
}

// Run runs git with args and answers its output, trimmed. An error is the remote's own words, its remote:
// lines, else git's last line; never the token, raw or in its header.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	out, err := g.Output(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Output runs git with input on its stdin and answers its output as git wrote it, whether it failed or not — a
// push's --porcelain report is on stdout when the push is refused — and the error as Run's. git follows no
// redirect, so the token goes to no other address, and trusts what the server trusts: SSL_CERT_FILE, when the
// server has one, is git's CA file too.
func (g Git) Output(ctx context.Context, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + g.Dir}, args...)...) //nolint:gosec // git's own words: commit ids git made or the remote sent, a tree Canyon made, an https url
	auth := base64.StdEncoding.EncodeToString([]byte(g.User + ":" + g.Token))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
		"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false", "GIT_HTTP_LOW_SPEED_LIMIT=" + LowSpeed.Bytes, "GIT_HTTP_LOW_SPEED_TIME=" + LowSpeed.Seconds,
		"GIT_AUTHOR_DATE=" + g.Date.UTC().Format(time.RFC3339), "GIT_COMMITTER_DATE=" + g.Date.UTC().Format(time.RFC3339), "GIT_AUTHOR_NAME=" + g.Name, "GIT_AUTHOR_EMAIL=" + g.Email, "GIT_COMMITTER_NAME=" + g.Name, "GIT_COMMITTER_EMAIL=" + g.Email}
	if ca := os.Getenv("SSL_CERT_FILE"); ca != "" {
		cmd.Env = append(cmd.Env, "GIT_SSL_CAINFO="+ca)
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	// git runs in a process group of its own, and a cancelled ctx kills the whole group: git and its transport
	// helper (git-remote-https), which would otherwise outlive it with the token in its environment until its own
	// floor gave up (#2473). Past WaitDelay the pipes are closed and Output returns, so the caller's bound holds
	// whatever is left. Only the server and the GitHub App run git, on Unix alone: no Windows build links this.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		said := strings.NewReplacer(g.Token, "…", auth, "…").Replace(strings.TrimSpace(stderr.String()))
		lines := slices.DeleteFunc(strings.Split(said, "\n"), func(l string) bool { return strings.HasPrefix(l, "hint:") }) // git's advice to a person at a terminal
		remote := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "remote:") })
		if len(remote) == 0 && len(lines) > 0 {
			remote = lines[len(lines)-1:]
		}
		return strings.NewReplacer(g.Token, "…", auth, "…").Replace(string(out)), errors.New(strings.Join(remote, " "))
	}
	return string(out), nil
}
