package executor

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var runReq = RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}

// A /run that finds no network creates canyon-app-<app> on the pool's first /24 docker does not refuse
// as overlapping, its gateway the /24's first address, IPv4 only (the server's guard knows the pool
// alone, never an IPv6 prefix a daemon default would add), connects each joined container below every
// network it already has as its default gateway, and starts the app on it, in its floor holder's namespace:
// the holder on the network under the app's name, its floor told the /24 and its gateway. A later /run
// connects only a joined container that is not on it: a crash in between.
func TestRunCreatesTheAppsNetworkOnTheFirstFreeSlash24(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{"overlaps": "172.28.0.0/24\n172.28.1.0/24\n"})
	_, err := e.Run(context.Background(), runReq)
	must(t, err)
	argv := read(t, dir, "argv")
	holder := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-appfloor-[0-9a-f]{8})\n`).FindStringSubmatch(argv)
	if holder == nil {
		t.Fatalf("no holder started:\n%s", argv)
	}
	for _, want := range []string{
		"network\ncreate\n--ipv6=false\n--subnet\n172.28.2.0/24\n--gateway\n172.28.2.1\n--label\ncanyon.app=claims-portal\ncanyon-app-claims-portal\nnetwork\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\ncanyon-server\nnetwork\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\napp-postgres\n",
		"run\n-d\n--name\n" + holder[1] + "\n--network\ncanyon-app-claims-portal\n--network-alias\nclaims-portal-preview\n",
		"--env\nCANYON_APP_GATEWAY=172.28.2.1\n--env\nCANYON_APP_SUBNET=172.28.2.0/24\n",
		"run\n-d\n--name\nclaims-portal-preview\n--network\ncontainer:" + holder[1] + "\n--label\ncanyon.app=claims-portal\n--label\ncanyon.target=preview\n--label\ncanyon.floor=" + holder[1] + "\n",
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %q:\n%s", want, argv)
		}
	}
	if n := strings.Count(argv, "network\ncreate\n"); n != 3 {
		t.Fatalf("%d creates, want the two refused as overlapping and the one that took", n)
	}
	must(t, os.WriteFile(filepath.Join(dir, "n", "canyon-app-claims-portal"), []byte("canyon-server\n"), 0o600))
	must(t, os.Remove(filepath.Join(dir, "argv")))
	_, err = e.Run(context.Background(), runReq)
	must(t, err)
	if argv := read(t, dir, "argv"); strings.Contains(argv, "network\ncreate\n") || strings.Count(argv, "network\nconnect\n") != 1 || !strings.Contains(argv, "network\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\napp-postgres\n") {
		t.Fatalf("the network exists with canyon-server on it: connect app-postgres alone:\n%s", argv)
	}
}

// A pool with no free /24 is a 502 naming it; a create docker refuses for another reason is a 502,
// tried once. Neither starts a container.
func TestRunAnswers502WhenNoNetworkCanBeCreated(t *testing.T) {
	for name, tc := range map[string]struct {
		files   map[string]string
		want    string
		creates int
	}{
		"a full pool":     {map[string]string{"overlaps": "10.9.8.0/24\n10.9.9.0/24\n"}, "10.9.8.0/23", 2},
		"another refusal": {map[string]string{"refuse": "10.9.8.0/24\n"}, "network create", 1},
	} {
		e, dir := fakes(t)
		e.Pool = netip.MustParsePrefix("10.9.8.0/23")
		dockerWithImages(t, dir, tc.files)
		srv := httptest.NewServer(Handler(e))
		_, err := (&Client{Base: srv.URL}).Run(context.Background(), runReq)
		srv.Close()
		argv := read(t, dir, "argv")
		if n := strings.Count(argv, "network\ncreate\n"); err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), tc.want) || n != tc.creates || strings.Contains(argv, "\nrun\n") {
			t.Errorf("%s: %v after %d creates; want a 502 naming %q after %d, no container:\n%s", name, err, n, tc.want, tc.creates, argv)
		}
	}
}

// Two /runs of one app that find no network both start: the find and the create are under one lock,
// so the second finds the first's network rather than creating it again, which docker refuses.
func TestConcurrentRunsCreateTheNetworkOnce(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, nil)
	must(t, os.WriteFile(filepath.Join(dir, "slow"), nil, 0o600)) // both would find none before either creates
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, target := range []string{"preview", "prod"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := runReq
			req.Target = target
			_, err := e.Run(context.Background(), req)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
}

// /stop removes the network with the app's last container, disconnecting the joined containers
// first, and leaves it while another container of the app remains; /forget removes one a crash
// left. What is already gone is success.
func TestStopAndForgetRemoveTheNetworkOnceNoContainerIsLeft(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{"containers": "claims-portal-preview sha256:aa\nclaims-portal-prod sha256:aa\n"})
	network := filepath.Join(dir, "n", "canyon-app-claims-portal")
	must(t, os.WriteFile(network, []byte("canyon-server\napp-postgres\n"), 0o600))
	must(t, e.Stop(context.Background(), "claims-portal", "preview"))
	if _, err := os.Stat(network); err != nil || strings.Contains(read(t, dir, "argv"), "network\ndisconnect\n") {
		t.Fatalf("prod still runs: the network stays, joined: %v\n%s", err, read(t, dir, "argv"))
	}
	for range 2 {
		must(t, e.Stop(context.Background(), "claims-portal", "prod"))
	}
	argv := read(t, dir, "argv")
	if _, err := os.Stat(network); !os.IsNotExist(err) || !strings.Contains(argv, "network\ndisconnect\ncanyon-app-claims-portal\ncanyon-server\nnetwork\ndisconnect\ncanyon-app-claims-portal\napp-postgres\nnetwork\nrm\ncanyon-app-claims-portal\n") {
		t.Fatalf("the last container gone: disconnect both, then remove the network: %v\n%s", err, argv)
	}
	must(t, os.WriteFile(network, []byte("canyon-server\n"), 0o600)) // a crash between the create and the start
	must(t, e.Forget(context.Background(), "claims-portal"))
	if _, err := os.Stat(network); !os.IsNotExist(err) {
		t.Fatalf("/forget left the network a crash left: %v", err)
	}
}

// A docker call that hangs while the network lock is held is cut at netDeadline, so one stuck daemon
// call cannot hold every other app's /run, /stop and /forget behind it.
func TestAStuckDockerCallReleasesTheNetworkLock(t *testing.T) {
	old := netDeadline
	netDeadline = 300 * time.Millisecond
	t.Cleanup(func() { netDeadline = old })
	e, dir := fakes(t)
	dockerWithImages(t, dir, nil)
	must(t, os.WriteFile(filepath.Join(dir, "hang"), []byte("canyon-app-claims-portal"), 0o600))
	go func() { _, _ = e.Run(context.Background(), runReq) }()
	for i := 0; ; i++ { // its inspect hangs, the lock held
		if argv, _ := os.ReadFile(filepath.Join(dir, "argv")); strings.Contains(string(argv), "canyon-app-claims-portal") { //nolint:gosec // test dir
			break
		}
		if i == 500 {
			t.Fatal("the /run never reached its network")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(context.Background(), "quote-engine", "preview") }()
	select {
	case err := <-stopped:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("another app's /stop waited on a stuck docker call past the deadline")
	}
}

// A joined container recreated since an app's /run — a compose recreate of canyon-server — is on its
// own networks alone: the executor connects it back to every app network it is missing from, leaves
// one that has all of them as it is, and makes no network /stop removed (#1104).
func TestRejoinPutsARecreatedJoinedContainerBackOnEveryAppNetwork(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, nil)
	must(t, os.WriteFile(filepath.Join(dir, "n", "canyon-app-claims-portal"), []byte("app-postgres\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "n", "canyon-app-quote-engine"), []byte("canyon-server\napp-postgres\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Rejoin(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(read(t, dir, "n/canyon-app-claims-portal"), "canyon-server") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	argv := read(t, dir, "argv")
	if strings.Count(argv, "network\nconnect\n") != 1 || !strings.Contains(argv, "network\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\ncanyon-server\n") || strings.Contains(argv, "network\ncreate\n") {
		t.Fatalf("one connect, canyon-server to claims-portal's network, and no create:\n%s", argv)
	}
}
