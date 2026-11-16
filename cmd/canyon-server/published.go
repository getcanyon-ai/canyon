package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// askLimit bounds each ask of GET /published: a hung executor or daemon is asked again, never waited on for good.
const askLimit = 10 * time.Second

// publishedGatewayOpen is the gateway localPeers serves under demo behind a published port (#2491): nil when
// there is none to serve (publishedGateway), else closed until the executor says where Docker publishes the
// server, and open once every binding of its port is on loopback. A peer rule alone cannot tell the host from
// a remote client that Docker's proxy re-originates from the gateway — an IPv6 client of a port published on
// every address, a daemon with iptables off, rootless Docker, Podman, Docker Desktop — so the server asks
// Docker, through the executor that holds its socket, instead of believing CANYON_PUBLISHED_ADDR.
//
// It asks once, at boot, off the boot path: the executor may come up after the server. Docker fixes a
// container's bindings when it creates it, so the answer holds for the process's life; a binding beyond
// loopback stops the server (refuse). No answer within askLimit, one out of the contract's shape, or one about
// a container that is not this one — self is this server's host name, which Docker sets to the first 12 hex of
// its container's Id — keeps the gateway closed and asks again, one second doubling to a minute.
func publishedGatewayOpen(ctx context.Context, published bool, ask func(context.Context) (executor.Published, error), self string, port int, logger *log.Logger, refuse func(error)) *atomic.Bool {
	if !published {
		return nil
	}
	open := new(atomic.Bool)
	go confirmLoopback(ctx, ask, self, port, open, logger, refuse, time.Second, askLimit)
	return open
}

func confirmLoopback(ctx context.Context, ask func(context.Context) (executor.Published, error), self string, port int, open *atomic.Bool, logger *log.Logger, refuse func(error), wait, limit time.Duration) {
	logged := ""
	for {
		asking, cancel := context.WithTimeout(ctx, limit)
		answer, err := ask(asking)
		cancel()
		if err == nil {
			err = ours(answer.ID, self)
		}
		if err == nil {
			if err := publishedOnLoopback(answer, port); err != nil {
				refuse(err)
				return
			}
			open.Store(true)
			logger.Printf("canyon-server: Docker publishes port %d on loopback alone: the host's connections through it are served (#2491)", port)
			return
		}
		if err.Error() != logged { // each reason once: an executor that comes up after the server is no news twice
			logged = err.Error()
			logger.Printf("canyon-server: the published port serves no one until the executor says where Docker publishes this server, asking again until it does: %v (#2491)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// ours is whether the answer is about this server's own container: Docker sets a container's host name to the
// first 12 hex of its Id, and docker-compose.yml sets no other. Anything else is another container, or doubt.
func ours(id, self string) error {
	if len(self) != 12 || !strings.HasPrefix(id, self) {
		return fmt.Errorf("the executor answered for container %.12s, and this server's host name is %q: not this container", id, self)
	}
	return nil
}

// publishedOnLoopback is the rule alone, and fails closed: at least one binding of the server's port Docker
// holds now, and every binding of it, created or held, on a loopback IP, 127.0.0.0/8 or ::1. An empty host
// address is every address, and anything that does not parse as an IP is refused with it. The error names the
// binding and the fix.
func publishedOnLoopback(p executor.Published, port int) error {
	key := strconv.Itoa(port) + "/tcp"
	for _, b := range slices.Concat(p.Created, p.Held) {
		if b.Port != key {
			continue
		}
		if ip, err := netip.ParseAddr(b.HostIP); err != nil || !ip.IsLoopback() {
			at := b.HostIP
			if at == "" {
				at = "every address"
			}
			return fmt.Errorf("CANYON_AUTH=demo serves this machine alone, but Docker publishes its port %s on %s (host port %s), where another machine reaches it as this one: publish it on 127.0.0.1 alone, as docker-compose.yml's ports: line 127.0.0.1:7777:7777 does, or set CANYON_AUTH=oidc for a Canyon other people reach (#2491)", key, at, b.HostPort)
		}
	}
	if !slices.ContainsFunc(p.Held, func(b executor.Binding) bool { return b.Port == key }) {
		return fmt.Errorf("CANYON_PUBLISHED_ADDR says Docker publishes the server, but it holds no binding of its port %s: unset CANYON_PUBLISHED_ADDR, or publish the port on 127.0.0.1 alone, as docker-compose.yml's ports: line 127.0.0.1:7777:7777 does (#2491)", key)
	}
	return nil
}
