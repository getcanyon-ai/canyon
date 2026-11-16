package gitpush

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pkt is one git pkt-line: its length in four hex digits, the length counted, then the payload.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func bare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "app.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil { //nolint:gosec // test: a temp dir
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

// A remote that says the credential back, raw and in its header, never gets it into the error Run answers,
// and the credential did reach it in the header.
func TestTheTokenNeverReachesAnError(t *testing.T) {
	const user, token = "x-access-token", "token-fixture-not-real-0123456789" //nolint:gosec // a test fixture, not a real token
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	got := make(chan string, 8) // the handler's goroutine sends, the test reads: never a variable both touch
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		got <- header
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = fmt.Fprint(w, pkt("# service=git-upload-pack\n")+"0000"+pkt("ERR refused "+header+" for "+user+":"+token+" ("+token+")\n"))
	}))
	defer remote.Close()
	_, err := Git{Dir: bare(t), User: user, Token: token}.Run(context.Background(), "ls-remote", remote.URL+"/app.git", "refs/heads/main")
	if header := <-got; !strings.EqualFold(header, "basic "+auth) {
		t.Fatalf("the remote got %q, not the token in its header", header)
	}
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("the remote's own words: %v", err)
	}
	for _, secret := range []string{token, auth} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error carries %q: %v", secret, err)
		}
	}
}

// git follows no redirect: a remote that sends it elsewhere never hands the token to the other address
// (design/HAND-OFF.md §3b, #2471).
func TestARedirectIsNeverFollowed(t *testing.T) {
	elsewhere := make(chan string, 8)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere <- r.Header.Get("Authorization")
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer other.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound) //nolint:gosec // the test's own other server: the redirect under test
	}))
	defer remote.Close()
	_, err := Git{Dir: bare(t), User: "x-access-token", Token: "token-fixture-not-real"}.Run(context.Background(), "ls-remote", remote.URL+"/app.git")
	if err == nil {
		t.Fatal("a redirect answered as the remote's refs")
	}
	select {
	case header := <-elsewhere:
		t.Fatalf("git followed the redirect, carrying %q", header)
	default:
	}
}

// A remote that stalls is given up on: a transfer slower than LowSpeed's floor ends, so git never holds the
// deploy repo's lock past its bound (contracts/routes.md, The deploy repo).
func TestAStalledRemoteIsGivenUp(t *testing.T) {
	saved := LowSpeed
	LowSpeed.Seconds = "2"
	t.Cleanup(func() { LowSpeed = saved })
	stalled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // headers, then nothing: a transfer at no speed at all
		case <-r.Context().Done():
		case <-stalled:
		}
	}))
	defer remote.Close()
	defer close(stalled)
	done := make(chan error, 1)
	go func() {
		_, err := Git{Dir: bare(t), User: "x-access-token", Token: "token-fixture-not-real"}.Run(context.Background(), "ls-remote", remote.URL+"/app.git")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled remote answered as refs")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("git waited on a stalled remote for 20 seconds: no floor on its transfers")
	}
}

// A cancelled ctx ends git within waitDelay, even while its transport helper still waits on a stalled remote:
// the caller's bound on the deploy repo's lock holds.
func TestACancelledGitEndsWithinItsDelay(t *testing.T) {
	stalled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stalled:
		}
	}))
	defer remote.Close()
	defer close(stalled)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Git{Dir: bare(t), User: "x-access-token", Token: "token-fixture-not-real"}.Run(ctx, "ls-remote", remote.URL+"/app.git")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled git answered refs")
		}
	case <-time.After(time.Second + waitDelay + 5*time.Second):
		t.Fatal("git outlived its cancelled ctx by more than waitDelay: the caller's bound does not hold")
	}
}

// A cancelled git leaves no transport helper behind (#2473): git runs in a process group of its own, and the cancel
// kills the group, so no git-remote-http outlives it with the token in its environment, still holding the remote's
// connection until its low-speed floor gives up. The remote sees the connection close once the cancel lands.
func TestACancelledGitLeavesNoHelperBehind(t *testing.T) {
	closed := make(chan struct{}, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the helper's connection closed: it was killed, or gave up
		closed <- struct{}{}
	}))
	t.Cleanup(func() { remote.CloseClientConnections(); remote.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := (Git{Dir: bare(t), User: "x-access-token", Token: "token-fixture-not-real"}).Run(ctx, "ls-remote", remote.URL+"/app.git"); err == nil {
		t.Fatal("a cancelled git answered refs")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second): // git's floor is 30 s: a helper left behind holds the connection past this
		t.Fatal("git's transport helper outlived the cancel, holding the remote's connection with the token in its environment")
	}
}
