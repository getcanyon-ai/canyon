package main

import (
	"fmt"
	"net"
	"net/netip"
)

// appsSubnetOf is CANYON_APPS_SUBNET: the executor's pool of app networks (CANYON_APPS_POOL), whose
// connections the listener refuses (refuseAppNet). Unset is the zero prefix and refuses nothing — a
// company host on Kubernetes has no such networks, and a deny rule there could refuse real clients.
func appsSubnetOf(s string) (netip.Prefix, error) {
	if s == "" {
		return netip.Prefix{}, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("CANYON_APPS_SUBNET: %w", err)
	}
	return p, nil
}

// refuseAppNet closes every connection whose peer is inside the pool at accept, before any HTTP is
// read, so no session row, log line or response exists for it. The server is joined to each app's
// network to proxy inward (surfaces.go), and under CANYON_AUTH=demo the session stage would hand an
// app container an admin session (canyonv2-f8q). A refusal is never returned as an error: that
// would end http.Server's Serve; the listener closes it and accepts the next.
//
// Each /24's first address is served: the executor makes it the app network's gateway, and Docker
// publishes the host port through whichever network is the server's gateway endpoint, so a host
// curl can arrive from it. App containers run under --cap-drop ALL (executor/executor.go) and cannot
// forge it. One IPv4 prefix, since the app networks have no IPv6, and every IPv6 peer but loopback refused.
func refuseAppNet(ln net.Listener, pool netip.Prefix) net.Listener {
	if !pool.IsValid() {
		return ln
	}
	return appNetListener{Listener: ln, pool: pool, gateways: true}
}

// refuseAppPool refuses every address of the pool, each /24's gateway too, and every IPv6 peer but
// loopback: a listener no app reaches, nor the host through an app network — Envoy's access-log receiver
// (design/EGRESS.md §2.2), whose peers sit on canyon-egress-ctl alone.
func refuseAppPool(ln net.Listener, pool netip.Prefix) net.Listener {
	if !pool.IsValid() {
		return ln
	}
	return appNetListener{Listener: ln, pool: pool}
}

type appNetListener struct {
	net.Listener
	pool     netip.Prefix
	gateways bool // each /24's gateway, where host traffic arrives from, is admitted
}

func (l appNetListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if peer, ok := c.RemoteAddr().(*net.TCPAddr); ok && l.refuses(peer.AddrPort().Addr()) {
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// refuses is the rule alone: inside the pool and not its /24's gateway, or any IPv6 peer but loopback —
// the app networks are IPv4 alone and the host port is published on 127.0.0.1, so an IPv6 peer is a
// container given an address the pool does not cover (#1109). A dual-stack socket spells an IPv4 peer
// as ::ffff:a.b.c.d, which an IPv4 prefix never contains, so the address is unmapped first.
func (l appNetListener) refuses(peer netip.Addr) bool {
	ip := peer.Unmap()
	if ip.Is6() {
		return !ip.IsLoopback()
	}
	return l.pool.Contains(ip) && (!l.gateways || ip != netip.PrefixFrom(ip, 24).Masked().Addr().Next())
}
