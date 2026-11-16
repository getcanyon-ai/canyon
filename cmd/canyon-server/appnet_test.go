package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// peerAs hands every accepted connection the peer address peer: a loopback dial arrives from
// 127.0.0.1, the first address of its /24, which the rule serves.
type peerAs struct {
	net.Listener
	peer netip.Addr
}

type remoteAs struct {
	net.Conn
	addr net.Addr
}

func (c remoteAs) RemoteAddr() net.Addr { return c.addr }

func (l peerAs) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return remoteAs{c, net.TCPAddrFromAddrPort(netip.AddrPortFrom(l.peer, 40000))}, nil
}

// listenHello answers every request on a loopback listener, its peers seen as peer, wrapped for pool
// with "hello" until the test ends, and returns its address.
func listenHello(t *testing.T, pool, peer string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") })}
	go func() {
		_ = srv.Serve(refuseAppNet(peerAs{ln, netip.MustParseAddr(peer)}, netip.MustParsePrefix(pool)))
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestRefuseAppNetClosesTheAppNetworkAtAccept(t *testing.T) {
	addr := listenHello(t, "172.28.0.0/16", "172.28.3.5") // an app container on the pool's fourth /24
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Nothing is sent, so a served connection sits until the server's one-second header timeout
	// closes it — also an EOF. The deadline is well inside that second: only a close at accept
	// answers this fast.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, err := c.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read from a refused connection: %d byte(s), %v; want 0, EOF", n, err)
	}
	// The listener is still accepting after a refusal: a refusal returned as an error would end
	// Serve, which closes the listener, and this dial would not connect.
	again, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("after one refusal the listener no longer accepts: %v", err)
	}
	defer func() { _ = again.Close() }()
	_ = again.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := again.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("second refused connection: %d byte(s), %v; want 0, EOF", n, err)
	}
}

// An app container given an IPv6 address — a daemon default that adds an IPv6 prefix to its network —
// is outside the IPv4 pool, and is closed at accept all the same (#1109).
func TestRefuseAppNetClosesAnIPv6App(t *testing.T) {
	c, err := net.Dial("tcp", listenHello(t, "172.28.0.0/16", "fd00:dead:beef::5"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) // inside the server's one-second header timeout: only a close at accept answers this fast
	if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("an IPv6 app: %d byte(s), %v; want 0, EOF", n, err)
	}
}

// The rule by address, the way a dual-stack socket spells an IPv4 peer included: an IPv4 prefix
// never contains ::ffff:a.b.c.d, so the peer is unmapped first. Every /24's gateway is served.
func TestRefusesUnmapsThePeerFirst(t *testing.T) {
	l := appNetListener{pool: netip.MustParsePrefix("172.28.0.0/16"), gateways: true}
	for peer, want := range map[string]bool{
		"172.28.0.5": true, "::ffff:172.28.0.5": true, "172.28.3.5": true, "172.28.3.255": true,
		"172.28.0.1": false, "172.28.3.1": false, "::ffff:172.28.3.1": false,
		"172.29.0.5": false, "10.0.0.5": false, "::1": false,
		"fd00::5": true, "2001:db8::5": true, "fe80::42:acff:fe1c:305": true, // an app given an IPv6 address the pool does not cover (#1109)
	} {
		if got := l.refuses(netip.MustParseAddr(peer)); got != want {
			t.Errorf("%s: refused %v, want %v", peer, got, want)
		}
	}
}

// Envoy's access-log receiver admits no address of the pool, a /24's gateway included: the host reaches
// it through no app network (design/EGRESS.md §2.2).
func TestRefuseAppPoolRefusesTheGatewaysToo(t *testing.T) {
	ln := refuseAppPool(nil, netip.MustParsePrefix("172.28.0.0/16")).(appNetListener)
	for peer, want := range map[string]bool{
		"172.28.3.1": true, "172.28.0.1": true, "::ffff:172.28.3.1": true, "172.28.3.10": true, "fd00::5": true,
		"10.214.1.7": false, "::1": false,
	} {
		if got := ln.refuses(netip.MustParseAddr(peer)); got != want {
			t.Errorf("%s: refused %v, want %v", peer, got, want)
		}
	}
}

func TestRefuseAppNetServesEveryOtherPeer(t *testing.T) {
	for name, peer := range map[string]string{
		"outside the pool": "10.0.0.5",
		"a /24's gateway":  "172.28.3.1", // where host traffic arrives from
	} {
		t.Run(name, func(t *testing.T) {
			res, err := http.Get("http://" + listenHello(t, "172.28.0.0/16", peer) + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || string(body) != "hello" {
				t.Fatalf("got %d %q, want 200 hello", res.StatusCode, body)
			}
		})
	}
}

func TestAppsSubnetOf(t *testing.T) {
	if p, err := appsSubnetOf(""); err != nil || p.IsValid() {
		t.Fatalf("unset: %v, %v; want the zero prefix and no error", p, err)
	}
	if p, err := appsSubnetOf("172.28.0.0/16"); err != nil || p != netip.MustParsePrefix("172.28.0.0/16") {
		t.Fatalf("172.28.0.0/16: %v, %v", p, err)
	}
	if _, err := appsSubnetOf("172.28.0.0"); err == nil {
		t.Fatal("an address without a length parsed as a prefix")
	}
}
