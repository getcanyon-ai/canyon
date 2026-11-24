package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dockerLogs replaces the fake docker with one whose `logs` verb runs body (a shell fragment)
// and whose `container inspect` answers as told; every call still appends its argv to dir/argv.
func dockerLogs(t *testing.T, dir, body string, inspectOK bool) {
	t.Helper()
	inspect := `[ "$2" = ls ] && exit 0; echo "Error response from daemon: No such container: $5" >&2; exit 1` // listed by none
	if inspectOK {
		inspect = `[ "$2" = ls ] && { for a; do case "$a" in name=*) n=${a#name=^}; echo "${n%?} running sha256:deadbeefcafe";; esac; done; exit 0; }; echo running 0 sha256:deadbeefcafe; exit 0`
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + dir + "/argv\"\nenv > \"" + dir + "/docker-env\"\ncase \"$1\" in container) " + inspect + ";; logs) " + body + ";; esac\n"
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
}

func TestLogsArgvIsTheTailOfOneContainerAndCarriesNoEnv(t *testing.T) { // contracts/executor.md invariant 5
	e, dir := fakes(t)
	dockerLogs(t, dir, "echo tail", true)
	t.Setenv("CANYON_VAULT_KEY", "would-leak-into-a-container-log")
	var out bytes.Buffer
	must(t, e.Logs(context.Background(), "claims-portal", "preview", 200, true, &out, nil))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))      //nolint:gosec // test dir
	env, _ := os.ReadFile(filepath.Join(dir, "docker-env")) //nolint:gosec // test dir
	if string(argv) != "container\nls\n--all\n--no-trunc\n--format\n{{.Names}} {{.State}} {{.Image}}\n--filter\nname=^claims-portal-preview$\ncontainer\ninspect\n--format\n{{.State.Status}} {{.RestartCount}} {{.Config.Image}}\nclaims-portal-preview\nlogs\n--follow\n--tail\n200\n--\nclaims-portal-preview\n" {
		t.Fatalf("a logs call inspects then tails <app>-<target> and nothing else: %q", argv)
	}
	if strings.Contains(string(env), "would-leak") || !strings.Contains(string(env), "PATH=") {
		t.Fatalf("docker logs gets the allowlisted environment and nothing from the executor's: %q", env)
	}
	shell := []string{"PWD", "OLDPWD", "SHLVL", "_"}                         // the stub is a shell script, and a shell sets these for itself
	for _, kv := range strings.Split(strings.TrimSpace(string(env)), "\n") { // exactly the allowlist: every name the child sees is one of childVars
		if k, _, _ := strings.Cut(kv, "="); k != "" && !slices.Contains(childVars(), k) && !slices.Contains(shell, k) {
			t.Fatalf("docker logs got %q, which is not in the executor's allowlist %v", k, childVars())
		}
	}
	if out.String() != "tail\n" {
		t.Fatalf("got %q", out.String())
	}
}

// An exited container's log is read as a running one's (contracts/executor.md, A container that isn't running):
// docker logs answers every run since /run made it, past its restarts.
func TestLogsReadAnExitedContainer(t *testing.T) {
	e, dir := fakes(t)
	script := "#!/bin/sh\ncase \"$1\" in container) [ \"$2\" = ls ] && { echo \"claims-portal-prod exited sha256:deadbeefcafe\"; exit 0; }; echo exited 10 sha256:deadbeefcafe; exit 0;; logs) printf 'run 1\\nTypeError: x is undefined\\n';; esac\n"
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
	var out bytes.Buffer
	if err := e.Logs(context.Background(), "claims-portal", "prod", 200, true, &out, nil); err != nil || out.String() != "run 1\nTypeError: x is undefined\n" {
		t.Fatalf("an exited container: %v, %q", err, out.String())
	}
}

func TestLogsEndsCleanlyWhenTheContextEnds(t *testing.T) {
	e, dir := fakes(t)
	started := filepath.Join(dir, "started")
	dockerLogs(t, dir, "touch \""+started+"\"; exec sleep 5", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Logs(ctx, "claims-portal", "prod", 0, true, &bytes.Buffer{}, nil) }()
	waitFor(t, func() (struct{}, bool) { _, err := os.Stat(started); return struct{}{}, err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a client that goes away is the end of the stream, not an error: %v", err)
	}
}

func TestHTTPLogsStreamBothStreamsAsWritten(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "printf 'out1\\n'; printf 'err1\\n' >&2; printf 'out2 no newline'", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/logs?app=claims-portal&target=preview")
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || body.String() != "out1\nerr1\nout2 no newline" {
		t.Fatalf("stdout and stderr arrive interleaved as docker wrote them, raw: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body.String())
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "--tail\n200\n--\nclaims-portal-preview\n") {
		t.Fatalf("tail defaults to 200: %q", argv)
	}
}

// follow=false is the last lines and the end of the body: docker logs without --follow, so a reader
// that is not a person gets the log and its end (contracts/executor.md, amended 2026-10-07).
func TestHTTPLogsWithoutFollowIsTheTailAndTheEnd(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "echo tail", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	body, err := (&Client{Base: srv.URL}).Logs(context.Background(), "claims-portal", "preview", 30, false)
	must(t, err)
	got, err := io.ReadAll(body)
	_ = body.Close()
	must(t, err)
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if string(got) != "tail\n" || !strings.HasSuffix(string(argv), "\nlogs\n--tail\n30\n--\nclaims-portal-preview\n") {
		t.Fatalf("follow=false read %q with argv %q", got, argv)
	}
}

func TestHTTPLogsRefusesANameOrTailBeforeTouchingDocker(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "echo tail", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	for name, query := range map[string]string{
		"path in the app":   "app=../other&target=preview",
		"uppercase app":     "app=Claims&target=preview",
		"empty app":         "app=&target=preview",
		"other target":      "app=claims-portal&target=staging",
		"tail not a count":  "app=claims-portal&target=preview&tail=x",
		"negative tail":     "app=claims-portal&target=preview&tail=-1",
		"tail over the cap": "app=claims-portal&target=preview&tail=" + strconv.Itoa(maxTail+1),
		"follow not a bool": "app=claims-portal&target=preview&follow=yes",
	} {
		resp, err := srv.Client().Get(srv.URL + "/logs?" + query)
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "argv")); err == nil {
		t.Fatal("a refused request never reaches docker")
	}
}

// A read that fails after the 200 ends on its reason (contracts/executor.md, GET /logs): a line of
// the executor's own, on a line of its own, never a silence that reads as a quiet app.
func TestHTTPLogsAFailedReadEndsOnItsReason(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "printf partial; exit 1", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	body, err := (&Client{Base: srv.URL}).Logs(context.Background(), "claims-portal", "preview", 10, true)
	must(t, err)
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if want := "partial\ncanyon: this log could not be read: docker logs: exit status 1\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTTPLogsIs404WhenThereIsNoContainer(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "echo tail", false)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	if _, err := c.Logs(context.Background(), "claims-portal", "preview", 50, true); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("a container the daemon does not have is a 404 and ErrNoContainer on the client side: %v", err)
	}
	if _, err := c.Logs(context.Background(), "claims-portal", "staging", 50, true); err == nil || !strings.Contains(err.Error(), "400") || errors.Is(err, ErrNoContainer) {
		t.Fatalf("a refused request: %v", err)
	}
}

// A daemon that does not answer is a 502, never the 404 of a container that is not there
// (contracts/executor.md, GET /logs).
func TestHTTPLogsIs502WhenTheDaemonDoesNotAnswer(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho 'failed to connect to the docker API at unix:///var/run/docker.sock' >&2\nexit 1\n"), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/logs?app=claims-portal&target=preview")
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

func TestHTTPLogsChildDiesWithTheRequest(t *testing.T) {
	e, dir := fakes(t)
	pidFile := filepath.Join(dir, "pid")
	dockerLogs(t, dir, "echo $$ > \""+pidFile+"\"; exec sleep 5", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { // the headers arrive at once; it is the read that ends with the context
		body, err := (&Client{Base: srv.URL}).Logs(ctx, "claims-portal", "preview", 10, true)
		if err == nil {
			_, err = io.Copy(io.Discard, body)
			_ = body.Close()
		}
		done <- err
	}()
	pid := waitFor(t, func() (int, bool) {
		raw, err := os.ReadFile(pidFile) //nolint:gosec // test dir
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n, err == nil && n > 0
	})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the stream ends with its context")
	}
	waitFor(t, func() (struct{}, bool) { return struct{}{}, errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}

// The assertion is liveness, not a clock: the line is read while the container is still
// running, which a buffered handler cannot do, however generous the timeout.
func TestHTTPLogsALineReachesTheReaderBeforeTheStreamEnds(t *testing.T) {
	e, dir := fakes(t)
	pidFile := filepath.Join(dir, "pid")
	dockerLogs(t, dir, "echo $$ > \""+pidFile+"\"; echo first; exec sleep 5", true)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line := make(chan string, 1)
	go func() { // the clock runs from the request: headers held back count as much as a body held back
		body, err := (&Client{Base: srv.URL}).Logs(ctx, "claims-portal", "preview", 10, true)
		if err != nil {
			line <- err.Error()
			return
		}
		defer func() { _ = body.Close() }()
		l, _ := bufio.NewReader(body).ReadString('\n')
		line <- l
		<-ctx.Done() // closing the body ends the request, and the handler kills the stub with it
	}()
	select {
	case l := <-line:
		if l != "first\n" {
			t.Fatalf("got %q", l)
		}
		// The container must still be running now that the line has arrived. A buffered handler
		// hands the line over only after the stub's sleep ends, so its pid is gone by then; a live
		// one has four seconds left. The reader holds the body open until the test ends: closed
		// after the line, it cancelled the request and the stub was dead before this check ran.
		alive := false
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !alive; time.Sleep(20 * time.Millisecond) {
			raw, _ := os.ReadFile(pidFile) //nolint:gosec // test dir
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			alive = pid > 0 && syscall.Kill(pid, 0) == nil
		}
		if !alive {
			t.Fatal("the line arrived only after the container ended: the stream was buffered")
		}
	case <-time.After(10 * time.Second): // a hang guard, not the assertion
		t.Fatal("a line the container wrote must reach the reader while the stream is still open")
	}
}

// waitFor polls f for up to three seconds and fails the test if it never holds.
func waitFor[T any](t *testing.T, f func() (T, bool)) T {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, ok := f()
		if ok {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A running container that has written nothing is not a dead one: the 200 is answered as soon as
// the inspect has passed, so a client with a start deadline does not give up on a quiet app.
func TestHTTPLogsAnswersHeadersBeforeTheContainerWritesAnything(t *testing.T) {
	e, dir := fakes(t)
	dockerLogs(t, dir, "exec sleep 5", true) // a container that stays silent
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan error, 1)
	go func() {
		body, err := (&Client{Base: srv.URL}).Logs(ctx, "claims-portal", "preview", 10, true)
		if err == nil {
			_ = body.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the headers of a silent container: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no headers within a second from a running container that wrote nothing")
	}
}

// pack gets a PACK_VOLUME_KEY, the operator's or the executor's own, so its warning that the key is
// unset never opens an app's build log; an operator's is passed as it is (#1245).
func TestPackAlwaysHasAVolumeKey(t *testing.T) {
	t.Setenv("PACK_VOLUME_KEY", "")
	_ = os.Unsetenv("PACK_VOLUME_KEY")
	if env := childEnv(); !slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "PACK_VOLUME_KEY=") && len(kv) > len("PACK_VOLUME_KEY=")
	}) {
		t.Fatalf("no key of the operator's: %q", env)
	}
	t.Setenv("PACK_VOLUME_KEY", "the-operators-key")
	if env := childEnv(); !slices.Contains(env, "PACK_VOLUME_KEY=the-operators-key") {
		t.Fatalf("the operator's key: %q", env)
	}
}
