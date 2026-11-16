package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// localPeers is sign-in off's peer rule (#2449): under off and demo every caller is an admin, so the listener
// serves this machine alone and closes every other connection at accept, before any HTTP is read, as
// refuseAppNet does. A Host check cannot do it: any client can send Host: localhost.
//
// A loopback peer is this machine. Behind a Docker port mapping on loopback (gateway: publishedGatewayOpen) the
// host's own connections arrive from the gateway of the network they reach the server on, which Docker's proxy,
// or its hairpin NAT, re-originates them from, so that gateway is served too, once Docker is seen to publish
// the port on loopback alone. A client elsewhere that Docker forwards with DNAT keeps its own address and is
// closed. Unpublished (gateway nil), as on a bare host or a cluster, no gateway is served: a router's or a load
// balancer's address would otherwise pass for the host. Under oidc it is ln.
//
// The first refusal is logged, once a process, so an operator who opens the demo from another machine learns why
// it answered nothing; every other is closed in silence, so a scanner cannot fill the log.
func localPeers(ln net.Listener, mode authMode, gateway *atomic.Bool) net.Listener {
	if mode == authOIDC {
		return ln
	}
	return localListener{Listener: ln, addrs: net.InterfaceAddrs, gateway: gateway, log: log.Default(), once: new(sync.Once)}
}

type localListener struct {
	net.Listener
	addrs   func() ([]net.Addr, error) // the host's interface addresses, read per connection: the executor joins the server to each app's network after boot
	gateway *atomic.Bool               // the gateway is served while it holds true; nil, never
	log     *log.Logger
	once    *sync.Once // the first refusal's log line
}

func (l localListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.admits(c.RemoteAddr(), c.LocalAddr()) {
			return c, nil
		}
		l.once.Do(func() {
			peer := c.RemoteAddr().String()
			if host, _, err := net.SplitHostPort(peer); err == nil {
				peer = host
			}
			l.log.Printf("canyon-server: sign-in off serves this machine alone: closed a connection from %s, and logs no other (#2449)", peer)
		})
		_ = c.Close()
	}
}

// admits is the rule alone, and fails closed: a peer is served when it is loopback, or the gateway of the
// network holding the server's own address it dialled (gatewayOf), while the gateway is open. An address that
// is not TCP, an IPv6 peer but loopback, an unreadable interface list, or a local address on no interface is
// refused. A dual-stack socket spells an IPv4 peer as ::ffff:a.b.c.d, so the address is unmapped first.
func (l localListener) admits(remote, local net.Addr) bool {
	peer, ok := remote.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip := peer.AddrPort().Addr().Unmap()
	if ip.IsLoopback() {
		return true
	}
	self, ok := local.(*net.TCPAddr)
	if l.gateway == nil || !l.gateway.Load() || !ok {
		return false
	}
	addrs, err := l.addrs()
	if err != nil {
		return false
	}
	gw, err := gatewayOf(self.AddrPort().Addr().Unmap(), addrs)
	return err == nil && ip == gw
}

// publishedGateway is whether localPeers may serve the gateway, once Docker is seen to publish the port on
// loopback alone (publishedGatewayOpen): under demo, the compose demo's mode, when CANYON_PUBLISHED_ADDR names a
// loopback IP. Published beyond it (0.0.0.0, a LAN address), Docker's proxy re-originates a remote client from
// the gateway too, so the boot fails closed on any published address that is not a loopback IP. Unset, no
// gateway. off binds loopback, where no gateway's connection arrives, and oidc has
// no peer rule: neither reads the address for this.
func publishedGateway(mode authMode, publish string) (bool, error) {
	if mode != authDemo || publish == "" {
		return false, nil
	}
	host, _, err := net.SplitHostPort(publish)
	if err != nil { // publishedAddrOf refuses it first at boot
		return false, fmt.Errorf("CANYON_PUBLISHED_ADDR=%s is not host:port", publish)
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return false, fmt.Errorf("CANYON_AUTH=%s serves this machine alone, so CANYON_PUBLISHED_ADDR=%s must be a loopback IP such as 127.0.0.1:7777, or set CANYON_AUTH=oidc for a Canyon other people reach", mode, publish)
	}
	return true, nil
}
