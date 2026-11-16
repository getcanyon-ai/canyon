package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// connAs is an accepted connection whose peer and local address are a test's: no laptop dials from a Docker
// network's address.
type connAs struct {
	net.Conn
	remote, local net.Addr
}

func (c connAs) RemoteAddr() net.Addr { return c.remote }
func (c connAs) LocalAddr() net.Addr  { return c.local }

type acceptAs struct {
	net.Listener
	peer, local string
}

func (l acceptAs) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return connAs{c, tcpAddr(l.peer), tcpAddr(l.local)}, nil
}

func tcpAddr(ip string) *net.TCPAddr {
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), 7777))
}

// composeAddrs is the demo server's interfaces: loopback, canyon-internal and an app's network.
func composeAddrs() ([]net.Addr, error) {
	return []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("10.214.2.3"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("10.213.5.2"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.82.5"), Mask: net.CIDRMask(32, 32)}, // a Calico pod's own /32
		&net.IPNet{IP: net.ParseIP("192.168.83.0"), Mask: net.CIDRMask(31, 32)}, // a point-to-point /31
		&net.IPNet{IP: net.ParseIP("fe80::42:aff:fed6:203"), Mask: net.CIDRMask(64, 128)},
	}, nil
}

// lockedLog is a log the listener writes from its own goroutine while the test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func listenLocal(t *testing.T, peer, local string) string {
	t.Helper()
	addr, _ := listenLogged(t, peer, local)
	return addr
}

func listenLogged(t *testing.T, peer, local string) (string, *lockedLog) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedLog{}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") })}
	go func() {
		_ = srv.Serve(localListener{Listener: acceptAs{ln, peer, local}, addrs: composeAddrs, gateway: opened(true), log: log.New(logs, "", 0), once: new(sync.Once)})
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), logs
}

// Under demo, a peer that is neither loopback nor the gateway of the network it arrived on, as a remote client
// that Docker forwards with DNAT keeps its own address, is closed at accept, and the listener goes on accepting.
func TestLocalPeersClosesAPeerOffThisMachineAtAccept(t *testing.T) {
	addr, logs := listenLogged(t, "203.0.113.7", "10.214.2.3")
	for i := range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) // inside the server's one-second header timeout: only a close at accept answers this fast
		n, err := c.Read(make([]byte, 1))
		_ = c.Close()
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("connection %d from 203.0.113.7: %d byte(s), %v; want 0, EOF", i+1, n, err)
		}
	}
	// The first refusal is logged, before its close, so an operator learns why; the second is not, nor any after it.
	if got, want := logs.String(), "canyon-server: sign-in off serves this machine alone: closed a connection from 203.0.113.7, and logs no other (#2449)\n"; got != want {
		t.Errorf("the log after two refusals:\n%q\nwant one line:\n%q", got, want)
	}
}

func TestLocalPeersServesTheHostsGateway(t *testing.T) {
	res, err := http.Get("http://" + listenLocal(t, "10.214.2.1", "10.214.2.3") + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if body, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("canyon-internal's gateway: %d %q, want 200 hello", res.StatusCode, body)
	}
}

func TestLocalPeersAdmitsLoopbackAndTheGatewayAlone(t *testing.T) {
	published := localListener{addrs: composeAddrs, gateway: opened(true)}
	for _, c := range []struct {
		peer, local string
		want        bool
	}{
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.53", "10.214.2.3", true},
		{"::1", "::1", true},
		{"::ffff:127.0.0.1", "::ffff:127.0.0.1", true},
		{"10.214.2.1", "10.214.2.3", true},               // canyon-internal's gateway, where the published port arrives from
		{"::ffff:10.214.2.1", "::ffff:10.214.2.3", true}, // the same, on a dual-stack socket
		{"10.213.5.1", "10.213.5.2", true},               // an app network's gateway, once Docker publishes through it
		{"203.0.113.7", "10.214.2.3", false},             // a remote client Docker forwards with DNAT
		{"10.214.2.9", "10.214.2.3", false},              // another container on the same network
		{"10.214.2.1", "10.213.5.2", false},              // a gateway, but of another network than the one it arrived on
		{"10.213.5.7", "10.213.5.2", false},              // an app
		{"192.168.82.6", "192.168.82.5", false},          // the next address after a /32, which has no gateway
		{"192.168.83.1", "192.168.83.0", false},          // a /31's other address, which is no gateway either
		{"10.0.0.1", "10.0.0.5", false},                  // the gateway of an address the server holds on no interface
		{"fd00::1", "fd00::5", false},
		{"2001:db8::7", "10.214.2.3", false},
		{"fe80::42:aff:fed6:201", "fe80::42:aff:fed6:203", false},
	} {
		if got := published.admits(tcpAddr(c.peer), tcpAddr(c.local)); got != c.want {
			t.Errorf("%s on %s: admitted %v, want %v", c.peer, c.local, got, c.want)
		}
	}

	bare := localListener{addrs: composeAddrs} // no CANYON_PUBLISHED_ADDR: a bare host or a cluster, whose router is no gateway of Canyon's
	if bare.admits(tcpAddr("10.214.2.1"), tcpAddr("10.214.2.3")) || !bare.admits(tcpAddr("127.0.0.1"), tcpAddr("127.0.0.1")) {
		t.Error("unpublished: a gateway was admitted, or loopback refused")
	}
	unread := localListener{addrs: func() ([]net.Addr, error) { return nil, errors.New("netlink") }, gateway: opened(true)}
	if unread.admits(tcpAddr("10.214.2.1"), tcpAddr("10.214.2.3")) {
		t.Error("the gateway was admitted with the interfaces unread")
	}
	unconfirmed := localListener{addrs: composeAddrs, gateway: opened(false)} // published, but Docker not yet seen to publish on loopback alone (#2491)
	if unconfirmed.admits(tcpAddr("10.214.2.1"), tcpAddr("10.214.2.3")) || !unconfirmed.admits(tcpAddr("127.0.0.1"), tcpAddr("127.0.0.1")) {
		t.Error("unconfirmed: the gateway was admitted, or loopback refused")
	}
	unix := &net.UnixAddr{Name: "/run/canyon.sock", Net: "unix"}
	if published.admits(unix, tcpAddr("10.214.2.3")) || published.admits(tcpAddr("10.214.2.1"), unix) {
		t.Error("an address that is not TCP was admitted")
	}
}

// Under demo the gateway is served only behind a loopback publish: published beyond loopback, Docker's
// proxy hands a remote client the gateway's address, so the boot is refused, naming the setting.
func TestLocalPeersRefuseAPublishBeyondLoopbackAtBoot(t *testing.T) {
	for _, c := range []struct {
		publish string
		gateway bool
	}{{"127.0.0.1:7777", true}, {"127.0.0.2:7777", true}, {"[::1]:7777", true}, {"", false}} {
		if gw, err := publishedGateway(authDemo, c.publish); gw != c.gateway || err != nil {
			t.Errorf("demo, published %q: gateway %v, %v; want %v, nil", c.publish, gw, err, c.gateway)
		}
	}
	for _, publish := range []string{"0.0.0.0:7777", "[::]:7777", "192.168.1.20:7777", "203.0.113.7:7777", "localhost:7777", ":7777"} {
		gw, err := publishedGateway(authDemo, publish)
		if gw || err == nil || !strings.HasPrefix(err.Error(), "CANYON_AUTH=demo serves this machine alone, so CANYON_PUBLISHED_ADDR="+publish+" must be a loopback IP such as 127.0.0.1:7777") {
			t.Errorf("demo, published %q: gateway %v, %v; want refused at boot, naming the setting", publish, gw, err)
		}
	}
	if _, err := publishedGateway(authDemo, "127.0.0.1"); err == nil || err.Error() != "CANYON_PUBLISHED_ADDR=127.0.0.1 is not host:port" {
		t.Errorf("demo, published without a port: %v; want it named not host:port", err)
	}
	if gw, err := publishedGateway(authOff, "0.0.0.0:7777"); gw || err != nil { // off binds loopback: no gateway's connection ever arrives
		t.Errorf("off, published on 0.0.0.0: gateway %v, %v; want false, nil", gw, err)
	}
	if gw, err := publishedGateway(authOIDC, "0.0.0.0:7777"); gw || err != nil {
		t.Errorf("oidc, published on 0.0.0.0: gateway %v, %v; want false, nil: no peer rule", gw, err)
	}
}

// The executor joins the server to each app's network after boot, so the interfaces are read at every accept: a
// gateway on a network joined since the last read is served, and one on a network left is not.
func TestLocalPeersReadTheInterfacesAtEveryAccept(t *testing.T) {
	joined := false
	l := localListener{gateway: opened(true), addrs: func() ([]net.Addr, error) {
		if !joined {
			return composeAddrs()
		}
		return []net.Addr{&net.IPNet{IP: net.ParseIP("10.213.9.2"), Mask: net.CIDRMask(24, 32)}}, nil
	}}
	if l.admits(tcpAddr("10.213.9.1"), tcpAddr("10.213.9.2")) || !l.admits(tcpAddr("10.214.2.1"), tcpAddr("10.214.2.3")) {
		t.Fatal("before the join: a network the server is not on was served, or canyon-internal's gateway refused")
	}
	joined = true
	if !l.admits(tcpAddr("10.213.9.1"), tcpAddr("10.213.9.2")) || l.admits(tcpAddr("10.214.2.1"), tcpAddr("10.214.2.3")) {
		t.Error("after the join: the new app network's gateway was refused, or a network left was still served")
	}
}

// localPeers wraps the listener under off and demo alone: oidc's is the listener as it was.
func TestLocalPeersWrapSignInOffAlone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := localPeers(ln, authOIDC, opened(true)); got != ln {
		t.Errorf("oidc: %T, want the listener unwrapped", got)
	}
	for _, mode := range []authMode{authDemo, authOff} {
		for _, gateway := range []*atomic.Bool{opened(true), nil} {
			l, ok := localPeers(ln, mode, gateway).(localListener)
			if !ok || l.Listener != ln || l.addrs == nil || l.log == nil || l.once == nil || l.gateway != gateway {
				t.Errorf("%s, gateway %v: %+v, want a localListener over it with that gateway", mode, gateway, l)
			}
		}
	}
}

// opened is a gateway flag holding open.
func opened(open bool) *atomic.Bool {
	b := new(atomic.Bool)
	b.Store(open)
	return b
}
