package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// egressControl is canyon-server egress-control, the check beside Envoy (design/EGRESS.md §3.1): it pulls the
// policy and answers Envoy's ext_authz, and serves it its interception listener and leaves by xDS, on a loopback
// address until ctx ends, and its metrics on
// CANYON_EGRESS_CONTROL_METRICS_ADDR when that is set (§7), and it reads what Envoy lost from Envoy's admin socket
// and reports it (§2.4). With args "probe" it asks a running one instead whether it holds a policy, the readiness
// an exec probe tells.
func egressControl(ctx context.Context, getenv func(string) string, args []string) error {
	addr := getenv("CANYON_EGRESS_CONTROL_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7783"
	}
	if ap, err := netip.ParseAddrPort(addr); err != nil || !ap.Addr().Is4() || !ap.Addr().IsLoopback() || ap.Port() == 0 {
		return fmt.Errorf("CANYON_EGRESS_CONTROL_ADDR: %q is not a loopback IPv4 address and a port, as 127.0.0.1:7783: only Envoy beside it asks", addr)
	}
	switch {
	case len(args) == 1 && args[0] == "probe":
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return egresscontrol.Probe(probe, addr)
	case len(args) != 0:
		return fmt.Errorf("egress-control takes no argument but probe, not %q", args)
	}
	metrics := getenv("CANYON_EGRESS_CONTROL_METRICS_ADDR")
	if ap, err := netip.ParseAddrPort(metrics); metrics != "" && (err != nil || !ap.Addr().Is4() || ap.Port() == 0) {
		return fmt.Errorf("CANYON_EGRESS_CONTROL_METRICS_ADDR: %q is not an IPv4 address and a port, as 0.0.0.0:7784", metrics)
	} else if u, err := url.Parse(getenv("CANYON_EGRESS_POLICY_URL")); metrics != "" && ap.Addr().IsUnspecified() && (err != nil || !strings.EqualFold(u.Scheme, "https")) {
		return fmt.Errorf("CANYON_EGRESS_CONTROL_METRICS_ADDR: %q on Docker, whose policy URL is not https://, would listen on every app network the gateway joins: name one address", metrics)
	}
	replicas, err := strconv.Atoi(getenv("CANYON_EGRESS_REPLICAS"))
	if err != nil || replicas < 1 || replicas > egresscontrol.FleetNames {
		return fmt.Errorf("CANYON_EGRESS_REPLICAS: %q is not the most replicas that run at once, 1 to %d", getenv("CANYON_EGRESS_REPLICAS"), egresscontrol.FleetNames)
	}
	var cert []byte
	if f := getenv("CANYON_EGRESS_POLICY_CERT_FILE"); f != "" {
		if cert, err = os.ReadFile(f); err != nil { //nolint:gosec // the operator's path
			return fmt.Errorf("CANYON_EGRESS_POLICY_CERT_FILE: %w", err)
		}
	}
	client, err := egresscontrol.NewClient(getenv("CANYON_EGRESS_POLICY_URL"), getenv("CANYON_EGRESS_POLICY_TOKEN_FILE"), cert)
	switch {
	case errors.Is(err, egresscontrol.ErrToken):
		return fmt.Errorf("CANYON_EGRESS_POLICY_TOKEN_FILE: %w", err)
	case errors.Is(err, egresscontrol.ErrCert):
		return fmt.Errorf("CANYON_EGRESS_POLICY_CERT_FILE: %w", err)
	case err != nil:
		return fmt.Errorf("CANYON_EGRESS_POLICY_URL: %w", err)
	}
	// The receiver's token, which the interception listener's access log sends (design/EGRESS.md §4.2): never logged,
	// and served Envoy in the LDS answer alone. Without one, nothing is intercepted, so no inner request goes unrecorded,
	// and every other call is checked as ever: a missed manifest takes no app's calls down.
	token := getenv("CANYON_EGRESS_RECEIVER_TOKEN")
	if !egressToken.MatchString(token) {
		log.Print("egress-control: CANYON_EGRESS_RECEIVER_TOKEN is not the receiver's token, so no interception listener is served: a call to a destination with a route is refused (503) rather than go unrecorded")
		token = ""
	}
	ee, err := wireEnterprise(getenv, nil) // egress-control seals and opens nothing
	if err != nil {
		return err
	}
	var inj egresscontrol.Injector // Canyon Enterprise's interception; without it a call the policy routes is refused inject_unavailable
	if ee.injector != nil {
		inj = ee.injector.Injector(client.Leaf, client.RouteValue, token, log.Default())
	}
	c, err := egresscontrol.New(client.Fetch, inj, replicas, token, client.TokenFile != "", log.Default()) // on Kubernetes alone: an https policy URL and its token file
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	if metrics != "" {
		mln, err := net.Listen("tcp4", metrics)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("CANYON_EGRESS_CONTROL_METRICS_ADDR: %w", err)
		}
		go func() {
			if err := egresscontrol.ServeMetrics(ctx, mln, c); err != nil {
				log.Printf("egress-control: metrics: %v", err)
			}
		}()
	}
	go c.Run(ctx)
	reading, stop := context.WithCancel(ctx)
	defer stop()
	drops, read := egresscontrol.NewDrops(egresscontrol.AdminSocket, client.Report, log.Default()), make(chan struct{})
	go func() { defer close(read); drops.Run(reading) }()
	err = egresscontrol.Serve(ctx, ln, c)
	stop()
	<-read // its last read and report, before the process goes
	return err
}
