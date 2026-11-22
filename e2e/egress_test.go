//go:build stack

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The floor (design/EGRESS.md §2.2), probed from behind canyon-egress — inside the network namespace Envoy
// joins: a new connection to Canyon's pools, loopback, a private (RFC 1918), shared (100.64/10),
// link-local or reserved address is refused, but to the server's receiver and to the configured resolvers
// on 53, and of the public internet TCP 80 and 443 alone are reached. Every address it must refuse is one
// a container beside the holder, on the same network without its floor, does reach, so each refusal is
// the floor's and nothing else's. The public addresses are stand-ins on documentation ranges, so nothing
// here needs the internet but one check that only logs. Envoy (§2.1), in that namespace, is asked as an app
// asks it, from a network the holder joins.

const (
	holder  = "canyon-egress"
	checker = "canyon-egress-control" // egress-control, in the holder's namespace beside Envoy (design/EGRESS.md §3.1)
	behind  = "container:" + holder
	ctl     = "canyon-egress-ctl"
	curl    = "curlimages/curl:8.11.1" // make demo-check's
	// busybox 1.37.0's multi-arch index, as the executor's e2e pins it.
	busybox = "busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	// floorPrefix names every network and container these tests make, so a run cleans up what a killed one left.
	floorPrefix = "canyon-floor-"
)

func TestConnectIMDSRefused(t *testing.T) {
	floorUp(t)
	stand := standIn(t, "169.254.87.0/24", holder)
	refused(t, stand, "http://"+stand.addr+"/")
	if reached(t, behind, "http://169.254.169.254/latest/meta-data/") {
		t.Error("the instance metadata address was reached from behind the holder")
	}
}

// Azure's WireServer, the IETF protocol assignments, benchmarking and class E: none is public, none private.
func TestConnectReservedRangesRefused(t *testing.T) {
	floorUp(t)
	for _, subnet := range []string{"192.0.0.0/24", "198.18.87.0/24", "240.0.87.0/24"} {
		stand := standIn(t, subnet, holder)
		refused(t, stand, "http://"+stand.addr+"/")
	}
	// No stand-in for the WireServer: on an Azure runner, a network on its /24 would take the host's own resolver.
	// The real address, then: the floor's REJECT refuses it at once (curl's exit 7), and reached fails the test
	// on a timeout or any other answer, so a path that silently drops it — or one off Azure that reaches
	// nothing — never passes for the floor's refusal.
	if reached(t, behind, "http://168.63.129.16/?comp=versions") {
		t.Error("Azure's WireServer was reached from behind the holder")
	}
}

func TestConnectCanyonServerRefused(t *testing.T) {
	floorUp(t)
	server := addressOn(t, "canyon-server", ctl)
	ready := "http://" + server + ":7777/ready"
	if !eventually(func() bool { return connected(t, ctl, ready) }) {
		t.Fatalf("%s was not connected to from %s, beside the holder: its refusal behind the holder would prove nothing", ready, ctl)
	}
	if connected(t, behind, ready) {
		t.Errorf("%s was connected to from behind the holder", ready)
	}

	receiver := holderEnv(t, "CANYON_EGRESS_RECEIVER_ADDR")
	if ip, _, _ := strings.Cut(receiver, ":"); ip != server {
		t.Fatalf("the holder admits the receiver at %s, not on the server's address %s on %s", receiver, server, ctl)
	}
	if !grpcAnswers(t, behind, receiver) {
		t.Errorf("the server's receiver at %s was refused from behind the holder", receiver)
	}
	// A resolver in the server's namespace, which e2e/compose.e2e.yml configures: inside Canyon's pools, never admitted.
	start(t, "server-stand", "--network", "container:canyon-server", busybox, "sh", "-c", standInCmd("exec dnsd -c /etc/dnsd.conf"))
	if !slices.Contains(resolversOf(t), server) {
		t.Fatalf("the holder's resolvers are %v, without e2e/compose.e2e.yml's %s", resolversOf(t), server)
	}
	if !eventually(func() bool { return resolves(t, ctl, server) }) {
		t.Fatalf("the stand-in resolver at %s did not answer on %s: the probe below proves nothing", server, ctl)
	}
	if resolves(t, behind, server) {
		t.Errorf("%s, a configured resolver inside Canyon's pools, answered from behind the holder", server)
	}
}

func TestConnectPrivateRangesRefused(t *testing.T) {
	floorUp(t)
	for _, subnet := range []string{"10.250.87.0/24", "172.31.87.0/24", "192.168.187.0/24", "100.64.87.0/24"} {
		stand := standIn(t, subnet, holder)
		refused(t, stand, "http://"+stand.addr+"/")
	}
	// An unconfigured private resolver, on UDP and TCP 53, and the receiver's port anywhere but the receiver.
	stand := standIn(t, "10.251.87.0/24", holder)
	refused(t, stand, "http://"+stand.addr+":53/")
	refused(t, stand, "http://"+stand.addr+":7781/")
	if !eventually(func() bool { return resolves(t, stand.name, stand.addr) }) {
		t.Fatalf("the stand-in resolver at %s did not answer on %s: the probe below proves nothing", stand.addr, stand.name)
	}
	if resolves(t, behind, stand.addr) {
		t.Errorf("%s, a private resolver no one configured, answered from behind the holder", stand.addr)
	}
}

// Listeners on 80 and 443, the ports the floor admits on every public address, so only the loopback rule
// refuses them, and on 7784, beside the check's port: listening on every address of the namespace, reached beside
// the holder on its own.
func TestConnectLoopbackRefused(t *testing.T) {
	floorUp(t)
	start(t, "loopback-stand", "--network", behind, busybox, "sh", "-c", "httpd -p 80 && httpd -p 7784 && exec httpd -f -p 443")
	at := addressOn(t, holder, ctl)
	for _, port := range []string{"80", "443", "7784"} { // 7784 beside the check's 7783, which the floor admits on loopback alone
		if !eventually(func() bool { return reached(t, ctl, "http://"+at+":"+port+"/") }) {
			t.Fatalf("the listener in the holder's namespace was not reached on %s:%s from %s: the probes below prove nothing", at, port, ctl)
		}
		for _, host := range []string{"127.0.0.1", "127.9.9.9", "0.0.0.0", "[::1]"} {
			if url := "http://" + host + ":" + port + "/"; reached(t, behind, url) {
				t.Errorf("%s was reached from behind the holder", url)
			}
		}
	}
	for _, conf := range []string{"all", "default"} {
		if v := docker(t, "exec", holder, "cat", "/proc/sys/net/ipv6/conf/"+conf+"/disable_ipv6"); v != "1" {
			t.Errorf("the holder's net.ipv6.conf.%s.disable_ipv6 is %s, not 1: ::1 is there to reach", conf, v)
		}
	}
	// Docker's own resolver answers canyon-server's address beside the holder, never behind it.
	if !eventually(func() bool { return resolvesName(t, ctl, "127.0.0.11", "canyon-server") }) {
		t.Fatalf("Docker's resolver did not answer canyon-server on %s: the probe below proves nothing", ctl)
	}
	if resolvesName(t, behind, "127.0.0.11", "canyon-server") {
		t.Error("Docker's resolver answered canyon-server from behind the holder")
	}
}

// egress-control's hole in the floor (design/EGRESS.md §3.1) is its own loopback address and port alone:
// Envoy, behind the holder, reaches the check there, as the floor admits; an app's namespace, on a network the
// holder joins, never does, at the holder's address on that network. TestConnectLoopbackRefused holds every
// other port of loopback refused, 7784 beside the check's among them.
func TestConnectControlOnLoopbackAlone(t *testing.T) {
	floorUp(t)
	controlUp(t)
	const addr = "127.0.0.1:7783" // CANYON_EGRESS_CONTROL_ADDR unset, as the shipped compose file leaves it, for Envoy, the floor and egress-control alike
	if !grpcAnswers(t, behind, addr) {
		t.Errorf("egress-control at %s was refused from behind the holder, where Envoy asks it", addr)
	}
	stand := standIn(t, "198.51.100.0/24", holder)
	_, port, _ := strings.Cut(addr, ":")
	if at := addressOn(t, holder, stand.name) + ":" + port; grpcAnswers(t, stand.name, at) {
		t.Errorf("egress-control answered at %s from %s, a network the holder joins, as an app's", at, stand.name)
	}
}

// e2e/compose.e2e.yml configures a public (203.0.113.9) and a private (192.168.188.9) stand-in resolver: the floor
// admits each on UDP and TCP 53 and nothing else of it.
func TestConnectResolversAllowed(t *testing.T) {
	floorUp(t)
	for _, c := range []struct{ subnet, refusedPort string }{{"203.0.113.0/24", "8443"}, {"192.168.188.0/24", "80"}} {
		stand := standIn(t, c.subnet, holder)
		if !slices.Contains(resolversOf(t), stand.addr) {
			t.Fatalf("the holder's resolvers are %v, without e2e/compose.e2e.yml's %s", resolversOf(t), stand.addr)
		}
		if !resolves(t, behind, stand.addr) {
			t.Errorf("the configured resolver %s did not answer on UDP 53 from behind the holder", stand.addr)
		}
		if !reached(t, behind, "http://"+stand.addr+":53/") {
			t.Errorf("the configured resolver %s was refused on TCP 53 from behind the holder", stand.addr)
		}
		refused(t, stand, "http://"+stand.addr+":"+c.refusedPort+"/")
	}
}

// Of a public address, TCP 80 and 443 alone: not another port, not DNS to a resolver no one configured.
func TestConnectPublicPortsOnly(t *testing.T) {
	floorUp(t)
	stand := standIn(t, "198.51.100.0/24", holder)
	for _, port := range []string{"80", "443"} {
		if !reached(t, behind, "http://"+stand.addr+":"+port+"/") {
			t.Errorf("%s:%s, public, was refused from behind the holder", stand.addr, port)
		}
	}
	refused(t, stand, "http://"+stand.addr+":8443/")
	refused(t, stand, "http://"+stand.addr+":53/")
	if !eventually(func() bool { return resolves(t, stand.name, stand.addr) }) {
		t.Fatalf("the stand-in resolver at %s did not answer on %s: the probe below proves nothing", stand.addr, stand.name)
	}
	if resolves(t, behind, stand.addr) {
		t.Errorf("%s, a public resolver no one configured, answered from behind the holder", stand.addr)
	}
	// The one check of the real internet, which only logs: a runner without it fails nothing above.
	if code, out := exitOf("run", "--rm", "--network", behind, curl, "-sS", "-o", "/dev/null", "-m", "10", "https://1.1.1.1/"); code != 0 {
		t.Logf("https://1.1.1.1/ was not reached from behind the holder (exit %d), the internet or the floor: %s", code, out)
	}
}

func TestFloorForwardsNothing(t *testing.T) {
	floorUp(t)
	if v := docker(t, "exec", holder, "cat", "/proc/sys/net/ipv4/ip_forward"); v != "0" {
		t.Errorf("the holder's net.ipv4.ip_forward is %s, not 0", v)
	}
	for _, c := range []struct{ tool, chain string }{{"iptables-nft", "FORWARD"}, {"ip6tables-nft", "OUTPUT"}} {
		if rules := strings.Split(docker(t, "exec", holder, c.tool, "-S", c.chain), "\n"); rules[0] != "-P "+c.chain+" DROP" {
			t.Errorf("%s's %s policy is %q, not DROP", c.tool, c.chain, rules[0])
		}
	}
}

// The server's attachment to canyon-egress-ctl is never its default route: its outbound calls and its
// published port keep the network they had.
func TestFloorServerRouteStaysOffTheCtlNetwork(t *testing.T) {
	floorUp(t)
	ctlNet := netip.MustParsePrefix(docker(t, "network", "inspect", "-f", "{{(index .IPAM.Config 0).Subnet}}", ctl))
	routes := docker(t, "run", "--rm", "--network", "container:canyon-server", busybox, "ip", "route")
	var defaults []string
	for line := range strings.SplitSeq(routes, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "default" && fields[1] == "via" {
			defaults = append(defaults, fields[2])
		}
	}
	if len(defaults) != 1 {
		t.Fatalf("the server has %d default routes, not one:\n%s", len(defaults), routes)
	}
	if gw, err := netip.ParseAddr(defaults[0]); err != nil || ctlNet.Contains(gw) {
		t.Errorf("the server's default route is via %s, on %s (%s)\n%s", defaults[0], ctl, ctlNet, routes)
	}
}

// Canyon's pools are refused whole even outside every private range: a scratch holder whose pool is a
// documentation range refuses a stand-in in it, on 80 and as a configured resolver.
func TestFloorRefusesPoolsOutsidePrivateRanges(t *testing.T) {
	floorUp(t)
	stand := standInAt(t, "192.0.2.0/24")
	scratch := floorPrefix + tag + "-holder"
	start(t, "holder", scratchHolder(t, stand.name, true, []string{"CANYON_EGRESS_POOLS=192.0.2.0/24", "CANYON_EGRESS_RESOLVERS=" + stand.addr})...)
	applied := func() bool {
		code, _ := exitOf("exec", scratch, "iptables-nft", "-C", "OUTPUT", "-j", "CANYON-FLOOR")
		return code == 0
	}
	if !eventually(applied) {
		t.Fatalf("the scratch holder never applied its floor\n%s", docker(t, "logs", scratch))
	}
	refused(t, stand, "http://"+stand.addr+"/", "container:"+scratch)
	if !eventually(func() bool { return resolves(t, stand.name, stand.addr) }) {
		t.Fatalf("the stand-in resolver at %s did not answer on %s: the probe below proves nothing", stand.addr, stand.name)
	}
	if resolves(t, "container:"+scratch, stand.addr) {
		t.Errorf("%s, a configured resolver inside the pool, answered from behind the scratch holder", stand.addr)
	}
}

// Azure's VMs resolve through the WireServer: configured as a resolver, it is admitted on 53 before its
// REJECT. Read from the chain itself, since a stand-in on its /24 would take an Azure runner's own resolver.
func TestFloorAdmitsTheWireServerAsAResolver(t *testing.T) {
	floorUp(t)
	const wire = "168.63.129.16/32"
	scratch := floorPrefix + tag + "-wire"
	start(t, "wire", scratchHolder(t, "", true, []string{"CANYON_EGRESS_POOLS=192.0.2.0/24", "CANYON_EGRESS_RESOLVERS=8.8.8.8,168.63.129.16"})...)
	applied := func() bool {
		code, _ := exitOf("exec", scratch, "iptables-nft", "-C", "OUTPUT", "-j", "CANYON-FLOOR")
		return code == 0
	}
	if !eventually(applied) {
		t.Fatalf("the scratch holder never applied its floor\n%s", docker(t, "logs", scratch))
	}
	at := map[string]int{} // a rule's -p, --dport and -j → its place in the chain, the first of each
	for i, line := range strings.Split(docker(t, "exec", scratch, "iptables-nft", "-S", "CANYON-FLOOR"), "\n") {
		rule := map[string]string{}
		fields := strings.Fields(line)
		for j := 0; j+1 < len(fields); j++ {
			if strings.HasPrefix(fields[j], "-") {
				rule[fields[j]] = fields[j+1]
			}
		}
		if rule["-d"] != wire {
			continue
		}
		if key := rule["-p"] + "/" + rule["--dport"] + " " + rule["-j"]; at[key] == 0 {
			at[key] = i
		}
	}
	reject := at["/ REJECT"]
	for _, admit := range []string{"udp/53 RETURN", "tcp/53 RETURN"} {
		if at[admit] == 0 || reject == 0 || at[admit] > reject {
			t.Errorf("%s's %s is rule %d, its REJECT rule %d: the WireServer must be admitted as a resolver before it is refused", wire, admit, at[admit], reject)
		}
	}
}

// A floor that cannot be applied stops the holder before it sleeps, and leaves no jump into a half-built chain.
func TestFloorFailsClosedAtStart(t *testing.T) {
	floorUp(t)
	good := []string{"CANYON_EGRESS_POOLS=192.0.2.0/24", "CANYON_EGRESS_RESOLVERS=8.8.8.8"}
	if code, out := bounded(t, append([]string{"run", "--rm"}, scratchHolder(t, "", false, good)...)...); code == 0 {
		t.Errorf("floor.sh without NET_ADMIN exited 0\n%s", out)
	}
	bad := []string{"CANYON_EGRESS_POOLS=192.0.2.0/24", "CANYON_EGRESS_RESOLVERS=8.8.8.8,not-an-address"}
	code, out := bounded(t, append([]string{"run", "--rm"}, scratchHolder(t, "", true, bad,
		"sh /etc/canyon/floor.sh; rc=$?; iptables-nft -C OUTPUT -j CANYON-FLOOR 2>/dev/null && echo the jump is in place; exit $rc")...)...)
	if code == 0 || strings.Contains(out, "the jump is in place") {
		t.Errorf("floor.sh with a resolver that is no address: exit %d\n%s", code, out)
	}
}

// Envoy routes 443 for CONNECT and https, 80 for http, and nothing else (design/EGRESS.md §2.1): the server's
// receiver, which the floor admits from Envoy's namespace, is no route — asked by an app routed through Canyon, by
// CONNECT or by an absolute-form GET, at its address, at a name for it, or at another port of a public stand-in,
// it is Envoy's 404, which the check is not asked for (ext_authz skips a request no route matches), so nothing
// is resolved or sent. A route to it would answer 200 to the CONNECT, or a 5xx from a gRPC server that speaks no
// HTTP/1.1. The same app reaches the stand-in by name on 80 and through a tunnel on 443, and the receiver answers
// from behind the holder.
func TestEnvoyRefusesTheReceiver(t *testing.T) {
	floorUp(t)
	standIn(t, "198.51.100.0/24", holder)
	app := routedApp(t)
	reachesFromApp(t, app)
	if connect, status := fromApp(t, app, "-p", "http://"+publicName+":443/etc/dnsd.conf"); connect != "200" || status != "200" {
		t.Errorf("CONNECT %s:443 through Envoy from %s was answered %s, then %s; want the tunnel and the stand-in's 200", publicName, app, connect, status)
	}
	receiver := holderEnv(t, "CANYON_EGRESS_RECEIVER_ADDR")
	if !grpcAnswers(t, behind, receiver) {
		t.Fatalf("the receiver at %s did not answer from behind the holder, where the floor admits it: the refusals below prove nothing", receiver)
	}
	_, port, _ := strings.Cut(receiver, ":")
	for _, dest := range []string{receiver, serverName + ":" + port, publicName + ":8443"} {
		if connect, _ := fromApp(t, app, "-p", "http://"+dest+"/"); connect != "404" {
			t.Errorf("CONNECT %s through Envoy from %s was answered %s, not 404: Envoy routes it", dest, app, connect)
		}
		if _, status := fromApp(t, app, "http://"+dest+"/"); status != "404" {
			t.Errorf("GET http://%s/ through Envoy from %s was answered %s, not 404: Envoy routes it", dest, app, status)
		}
	}
}

// Envoy dials what a name resolves to, so the floor beneath it refuses the private ones: a name for a private
// stand-in and one for the server's own address, on 80 and 443, by GET and by CONNECT, from an app routed through
// Canyon whose check lets them through, are Envoy's 503 — its upstream refused — never the stand-in's 200 and never
// a tunnel. The private stand-in answers on both from its own network, and the public one by name through Envoy,
// so each 503 is the floor's.
func TestEnvoyRefusesFloorRefusedDestinations(t *testing.T) {
	floorUp(t)
	standIn(t, "198.51.100.0/24", holder)
	app := routedApp(t)
	reachesFromApp(t, app)
	private := standIn(t, "10.250.87.0/24", holder)
	for _, url := range []string{"http://" + private.addr + "/", "http://" + private.addr + ":443/"} {
		if !eventually(func() bool { return reached(t, private.name, url) }) {
			t.Fatalf("%s was not reached from %s, beside the holder: its refusal through Envoy would prove nothing", url, private.name)
		}
	}
	for _, name := range []string{privateName, serverName} {
		if _, status := fromApp(t, app, "http://"+name+"/etc/dnsd.conf"); status != "503" {
			t.Errorf("GET http://%s/ through Envoy from %s was answered %s, not 503: the floor did not refuse it", name, app, status)
		}
		if connect, _ := fromApp(t, app, "-p", "http://"+name+":443/etc/dnsd.conf"); connect != "503" {
			t.Errorf("CONNECT %s:443 through Envoy from %s was answered %s, not 503: the floor did not refuse it", name, app, connect)
		}
	}
}

// Envoy's floor refuses the Docker host's own addresses, a public one among them, which public 80 and 443 would
// otherwise let through (#2524): a stand-in on the host's own 198.51.100.200 — on its loopback, put there before
// the stack started (the egress job does it), so egress-host listed it — answers a container on a plain bridge, and
// through Envoy, from an app routed through Canyon whose check lets it through, it is Envoy's 503 on 80 and 443,
// its upstream refused, never the stand-in's answer and never a tunnel.
func TestEnvoyRefusesTheHostsOwnAddress(t *testing.T) {
	floorUp(t)
	const addr = "198.51.100.200"
	if lo := docker(t, "run", "--rm", "--network", "host", busybox, "ip", "addr", "show", "dev", "lo"); !strings.Contains(lo, " inet "+addr+"/") {
		t.Fatalf("%s is not on the Docker host's loopback: put it there before make e2e-up, as the egress job does, so the host's listing holds it\n%s", addr, lo)
	}
	standIn(t, "198.51.100.0/24", holder)
	app := routedApp(t)
	reachesFromApp(t, app)
	start(t, "host-stand", "--network", "host", busybox, "sh", "-c", "httpd -p "+addr+":80 -h /etc && exec httpd -f -p "+addr+":443 -h /etc")
	for _, url := range []string{"http://" + addr + "/", "http://" + addr + ":443/"} {
		if !eventually(func() bool { return reached(t, "bridge", url) }) {
			t.Fatalf("%s, the host's own, was not reached from a plain bridge: its refusal through Envoy would prove nothing", url)
		}
	}
	if _, status := fromApp(t, app, "http://"+hostName+"/"); status != "503" {
		t.Errorf("GET http://%s/ through Envoy from %s was answered %s, not 503: the floor let the host's own address through", hostName, app, status)
	}
	if connect, _ := fromApp(t, app, "-p", "http://"+hostName+":443/"); connect != "503" {
		t.Errorf("CONNECT %s:443 through Envoy from %s was answered %s, not 503: the floor let the host's own address through", hostName, app, connect)
	}
}

// A call through Envoy reaches the server's receiver over ALS, with the token the server takes: the receiver
// holds a stream and the entry, and writes the call. Its caller is no app — the stand-in is no app's container,
// so the check refuses it, unknown_caller — and it is counted on the unattributed gap, never a row (design/EGRESS.md
// §2.3–2.4).
func TestEnvoyCallReachesTheReceiver(t *testing.T) {
	floorUp(t)
	if !egressSetting(t).Receiver {
		t.Fatal("the server takes no records: CANYON_EGRESS_RECEIVER_ADDR is unset")
	}
	stand := standIn(t, "198.51.100.0/24", holder)
	counted := unattributed(t)
	since := time.Now().Truncate(time.Second)
	if !answeredByTheCheck(t, stand) {
		t.Fatalf("http://%s/etc/dnsd.conf through Envoy, from no app's address, was not the check's 403", stand.addr)
	}
	if !eventually(func() bool {
		s := egressSetting(t)
		return s.Streams > 0 && s.LastEntryAt != nil && !s.LastEntryAt.Before(since)
	}) {
		t.Fatalf("no entry since %s reached the receiver: %+v\n%s", since, egressSetting(t), docker(t, "logs", "--tail", "20", "canyon-egress-envoy"))
	}
	if !eventually(func() bool { return unattributed(t) > counted }) {
		t.Errorf("the unattributed gap still counts %d calls: the call was not written", counted)
	}
	if n := ledger(t, "SELECT count(*) FROM egress_destinations WHERE destination = '"+stand.addr+"'"); n != "0" {
		t.Errorf("%s rows for %s, whose caller is no app", n, stand.addr)
	}
}

// One source address holds at most CANYON_EGRESS_PER_SOURCE connections to Envoy's listener (design/EGRESS.md
// §2.1): with that many held open, kept alive, its next connection is reset, while another source on the same
// network still connects and is answered — by the check, as neither is an app's. The e2e stack sets a small cap,
// so holding it is cheap.
func TestEnvoyCapsConnectionsPerSource(t *testing.T) {
	floorUp(t)
	stand := standIn(t, "198.51.100.0/24", holder)
	if !answeredByTheCheck(t, stand) {
		t.Fatalf("http://%s/etc/dnsd.conf through Envoy was not the check's 403: the cap below proves nothing", stand.addr)
	}
	limit, err := strconv.Atoi(holderEnv(t, "CANYON_EGRESS_PER_SOURCE"))
	if err != nil || limit < 1 {
		t.Fatalf("the holder's CANYON_EGRESS_PER_SOURCE is no positive count: %v", err)
	}
	proxy := addressOn(t, holder, stand.name)
	source := floorPrefix + tag + "-source"
	start(t, "source", "--network", stand.name, busybox, "sh", "-c",
		keptAlive(limit, proxy, stand.addr))
	held := func() int { // the source's connections to Envoy that are up, from busybox netstat's columns
		n := 0
		for line := range strings.SplitSeq(docker(t, "exec", source, "netstat", "-tn"), "\n") {
			if f := strings.Fields(line); len(f) == 6 && f[4] == proxy+":3128" && f[5] == "ESTABLISHED" {
				n++
			}
		}
		return n
	}
	if !eventually(func() bool { return held() == limit }) {
		t.Fatalf("the source holds %d connections to Envoy, not its cap of %d: the refusal below would prove nothing", held(), limit)
	}
	// Connection limit+1, a call through Envoy from the source's own namespace, so its own address: curl's 7 is
	// a connection refused, as the reset is; any other answer fails.
	if code, out := exitOf("run", "--rm", "--network", "container:"+source, curl, "-sS", "-o", "/dev/null", "-m", "10",
		"-x", "http://"+proxy+":3128", "http://"+stand.addr+"/etc/dnsd.conf"); code != 7 {
		t.Errorf("connection %d from one source to Envoy, past its cap of %d: curl exit %d, not 7 (refused)\n%s\nthe holder's INPUT chain:\n%s",
			limit+1, limit, code, out, docker(t, "exec", holder, "iptables-nft", "-L", "INPUT", "-v", "-n", "-x"))
	}
	if _, status := throughEnvoy(t, stand, "http://"+stand.addr+"/etc/dnsd.conf"); status != "403" {
		t.Errorf("another source on %s was answered %s through Envoy, not the check's 403, while one source holds its cap", stand.name, status)
	}
	if n := held(); n != limit {
		t.Errorf("the source holds %d connections to Envoy after the refusal, not its %d", n, limit)
	}
}

// TestEnvoyCallOfAnAppRoutedViaCanyonIsItsRow is an app deployed while the host says via canyon (design/EGRESS.md
// §2.2–2.4): its network internal, the app at
// preview's fixed address with the proxy variables set, and canyon-egress joined. A call it makes through
// http://canyon-egress:3128 to a public stand-in, by name, is exactly one egress_destinations row of (app,
// preview, the name, 80) within a flush; a raw dial, a public lookup, or a connect to a service the host runs on
// every address, through the network's gateway, reaches nothing; its source holds Envoy's per-source cap while
// another source on its network is still answered, by the check, as no app's. An app deployed under via off is as before: an open network, no proxy
// variables, the internet and the host reached directly — until egress is turned on, which lists it, and an
// admin's Recreate networks starts it again on an internal network through Envoy, a raw dial refused; turned
// off, it is listed again and recreated open, reaching out directly (design/EGRESS.md §2.2, #2254).
func TestEnvoyCallOfAnAppRoutedViaCanyonIsItsRow(t *testing.T) {
	floorUp(t)
	if !egressSetting(t).Receiver {
		t.Fatal("the server takes no records: CANYON_EGRESS_RECEIVER_ADDR is unset")
	}
	routed, open := "e2e-egress-"+tag, "e2e-open-"+tag
	t.Cleanup(func() {
		setVia(t, "off")
		forget(t, routed)
		forget(t, open)
	})
	setVia(t, "canyon")
	run := deployTiny(t, routed)
	setVia(t, "off")
	deployTiny(t, open)
	stand := standIn(t, "198.51.100.0/24", holder)
	app, network := routed+"-preview", "canyon-app-"+routed
	from := "container:" + app

	internal, subnet, _ := strings.Cut(docker(t, "network", "inspect", "-f", "{{.Internal}} {{(index .IPAM.Config 0).Subnet}}", network), " ")
	sub, err := netip.ParsePrefix(subnet)
	if internal != "true" || err != nil {
		t.Fatalf("%s: internal %s, subnet %q (%v); want an internal network on a /24", network, internal, subnet, err)
	}
	if at, want := addressOn(t, app, network), strings.TrimSuffix(sub.Addr().String(), "0")+"10"; at != want {
		t.Errorf("%s is at %s on %s, not preview's fixed address %s", app, at, network, want)
	}
	if label := docker(t, "inspect", "-f", `{{index .Config.Labels "canyon.gate_run"}}`, app); label != run {
		t.Errorf("%s's canyon.gate_run is %q, not its deploy's run %s", app, label, run)
	}
	for _, want := range []string{"HTTPS_PROXY=http://canyon-egress:3128", "HTTP_PROXY=http://canyon-egress:3128", "NODE_USE_ENV_PROXY=1", "NO_PROXY=canyon-server,localhost,127.0.0.1"} {
		if !slices.Contains(envOf(t, app), want) {
			t.Errorf("%s's environment lacks %s", app, want)
		}
	}

	{ // a call through Envoy is one row
		since := time.Now().Truncate(time.Second)
		if code, out := exitOf("run", "--rm", "--network", from, curl, "-sS", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}",
			"-x", "http://canyon-egress:3128", "http://"+publicName+"/etc/dnsd.conf"); code != 0 || out != "200" {
			t.Fatalf("a call from %s through http://canyon-egress:3128 to the stand-in: curl exit %d, %s", app, code, out)
		}
		row := func() string {
			return ledger(t, "SELECT count(*) || ' ' || coalesce(min(calls), 0) || ' ' || coalesce(min(last_protocol), '') || ' ' || coalesce(min(last_outcome), '') || ' ' || coalesce(bool_and(last_seen_at >= '"+
				since.UTC().Format(time.RFC3339)+"'), false) FROM egress_destinations WHERE app = '"+routed+"' AND env = 'preview' AND destination = '"+publicName+"' AND port = 80")
		}
		if !eventually(func() bool { return strings.HasPrefix(row(), "1 ") }) {
			t.Fatalf("no row (%s, preview, %s, 80) within the flush: %s; the app's rows: %s", routed, publicName, row(),
				ledger(t, "SELECT count(*) FROM egress_destinations WHERE app = '"+routed+"'"))
		}
		if got := row(); got != "1 1 http allowed true" {
			t.Errorf("the row (count, calls, last_protocol, last_outcome, seen since the call) is %q, want 1 1 http allowed true", got)
		}
		if n := ledger(t, "SELECT count(*) FROM egress_destinations WHERE app = '"+routed+"'"); n != "1" {
			t.Errorf("%s rows of %s, want exactly the one call's", n, routed)
		}
	}

	{ // a raw dial and a lookup reach nothing, an app under via off reaches out as before
		openApp := open + "-preview"
		if internal := docker(t, "network", "inspect", "-f", "{{.Internal}}", "canyon-app-"+open); internal != "false" {
			t.Errorf("canyon-app-%s, deployed under via off, is internal: %s", open, internal)
		}
		for _, kv := range envOf(t, openApp) {
			if name, _, _ := strings.Cut(kv, "="); strings.HasSuffix(strings.ToUpper(name), "_PROXY") || name == "NODE_USE_ENV_PROXY" {
				t.Errorf("%s, deployed under via off, has %s", openApp, kv)
			}
		}
		const public = "http://1.1.1.1/"
		if !reached(t, "container:"+openApp, public) {
			t.Fatalf("%s was not reached from %s, under via off: the refusal below proves nothing", public, openApp)
		}
		for _, url := range []string{public, "http://" + stand.addr + "/"} {
			if reached(t, from, url) {
				t.Errorf("a raw dial to %s from %s, routed via canyon, was answered", url, app)
			}
		}
		if !resolvesName(t, "container:"+openApp, "127.0.0.11", "example.com") {
			t.Fatalf("example.com did not resolve from %s, under via off: the refusal below proves nothing", openApp)
		}
		if resolvesName(t, from, "127.0.0.11", "example.com") {
			t.Errorf("example.com resolved from %s, routed via canyon: a lookup is a way out", app)
		}
		// a service on the host's wildcard address — as a natively run executor's :7778 is — answers on every
		// bridge's gateway: an internal network's must hold no address of the host's (gateway_mode_ipv4 isolated)
		ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // the host's wildcard address is what this probes
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		connects := func(from, to string) bool {
			code, out := exitOf("run", "--rm", "--network", from, busybox, "nc", "-z", "-w", "3", to, port)
			if code > 1 { // docker's own failure, never nc's answer
				t.Fatalf("nc %s %s from %s: exit %d\n%s", to, port, from, code, out)
			}
			return code == 0
		}
		scratch := floorPrefix + tag + "-open" // an open network with no floor on it, as an app's was before #2503
		docker(t, "network", "create", scratch)
		t.Cleanup(func() { exitOf("network", "rm", scratch) })
		scratchGateway := docker(t, "network", "inspect", "-f", "{{(index .IPAM.Config 0).Gateway}}", scratch)
		if !connects(scratch, scratchGateway) {
			t.Fatalf("the host's :%s was not reached from %s through its gateway %s: the refusals below prove nothing", port, scratch, scratchGateway)
		}
		if openGateway := docker(t, "network", "inspect", "-f", "{{(index .IPAM.Config 0).Gateway}}", "canyon-app-"+open); connects("container:"+openApp, openGateway) {
			t.Errorf("the host's :%s, on every address, was reached from %s, under via off, through its gateway %s: its floor refuses the host (#2503)", port, openApp, openGateway)
		}
		if gateway := strings.TrimSuffix(sub.Addr().String(), "0") + "1"; connects(from, gateway) {
			t.Errorf("the host's :%s, on every address, was reached from %s through its gateway %s", port, app, gateway)
		}
	}

	{ // one app container holds its own cap
		limit, err := strconv.Atoi(holderEnv(t, "CANYON_EGRESS_PER_SOURCE"))
		if err != nil || limit < 1 {
			t.Fatalf("the holder's CANYON_EGRESS_PER_SOURCE is no positive count: %v", err)
		}
		proxy := addressOn(t, holder, network)
		source := floorPrefix + tag + "-app-source"
		start(t, "app-source", "--network", from, busybox, "sh", "-c",
			keptAlive(limit, proxy, stand.addr))
		held := func() int { // the app namespace's connections to Envoy that are up, from busybox netstat's columns
			n := 0
			for line := range strings.SplitSeq(docker(t, "exec", source, "netstat", "-tn"), "\n") {
				if f := strings.Fields(line); len(f) == 6 && f[4] == proxy+":3128" && f[5] == "ESTABLISHED" {
					n++
				}
			}
			return n
		}
		if !eventually(func() bool { return held() == limit }) {
			t.Fatalf("%s holds %d connections to Envoy, not its cap of %d: the refusal below would prove nothing", app, held(), limit)
		}
		if code, out := exitOf("run", "--rm", "--network", from, curl, "-sS", "-o", "/dev/null", "-m", "10",
			"-x", "http://canyon-egress:3128", "http://"+stand.addr+"/etc/dnsd.conf"); code != 7 {
			t.Errorf("connection %d from %s to Envoy, past its cap of %d: curl exit %d, not 7 (refused)\n%s", limit+1, app, limit, code, out)
		}
		if code, out := exitOf("run", "--rm", "--network", network, curl, "-sS", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}",
			"-x", "http://canyon-egress:3128", "http://"+publicName+"/etc/dnsd.conf"); code != 0 || out != "403" {
			t.Errorf("another source on %s, while %s holds its cap: curl exit %d, %s; want the check's 403 to no app's address", network, app, code, out)
		}
	}

	{ // the app deployed under via off, its network recreated onto the floor and off it again
		openApp, openNet := open+"-preview", "canyon-app-"+open
		recreated := func(via string) {
			t.Helper()
			setVia(t, via)
			if s := egressSetting(t); !slices.Contains(s.Recreate, open) {
				t.Fatalf("under via %s the admin's read lists %v, without %s", via, s.Recreate, open)
			}
			if code, body := api(t, "POST", "/api/v1/egress/networks/recreate", map[string]string{"app": open}); code != http.StatusOK {
				t.Fatalf("Recreate networks under via %s: %d %s", via, code, body)
			}
			if s := egressSetting(t); slices.Contains(s.Recreate, open) {
				t.Fatalf("after the recreation under via %s the admin's read still lists %s", via, open)
			}
		}
		const public = "http://1.1.1.1/"
		recreated("canyon")
		if internal := docker(t, "network", "inspect", "-f", "{{.Internal}}", openNet); internal != "true" {
			t.Fatalf("%s after the recreation under via canyon: internal %s", openNet, internal)
		}
		if !slices.Contains(envOf(t, openApp), "HTTPS_PROXY=http://canyon-egress:3128") {
			t.Errorf("%s, started again under via canyon, has no HTTPS_PROXY", openApp)
		}
		if code, out := exitOf("run", "--rm", "--network", "container:"+openApp, curl, "-sS", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}",
			"-x", "http://canyon-egress:3128", "http://"+publicName+"/etc/dnsd.conf"); code != 0 || out != "200" {
			t.Errorf("a call from %s through http://canyon-egress:3128 to the stand-in, its network recreated: curl exit %d, %s", openApp, code, out)
		}
		if reached(t, "container:"+openApp, public) {
			t.Errorf("a raw dial to %s from %s, its network recreated under via canyon, was answered", public, openApp)
		}
		recreated("off")
		if internal := docker(t, "network", "inspect", "-f", "{{.Internal}}", openNet); internal != "false" {
			t.Fatalf("%s after the recreation under via off: internal %s", openNet, internal)
		}
		for _, kv := range envOf(t, openApp) {
			if name, _, _ := strings.Cut(kv, "="); strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
				t.Errorf("%s, started again under via off, has %s", openApp, kv)
			}
		}
		if !reached(t, "container:"+openApp, public) {
			t.Errorf("%s was not reached from %s, its network recreated open", public, openApp)
		}
	}
}

// TestEnvoyLossesReachTheRecords is what the pinned Envoy itself says (design/EGRESS.md §2.4, #2374). Its admin
// interface is a Unix socket on a volume two containers mount, and its namespace listens on no port for it, so no
// app reaches it. egress-control reads it: on a healthy stack its reads write no gap, where one that failed on
// Envoy's real answer would write one every read. With the receiver refused in Envoy's namespace, calls' entries are
// lost on streams that fail to open, which Envoy counts as flushed and never as dropped, and the records say so: a
// gap no one counted.
func TestEnvoyLossesReachTheRecords(t *testing.T) {
	floorUp(t)
	const socket = "/var/run/envoy-admin/admin.sock"
	mounted := strings.Fields(docker(t, "ps", "--filter", "volume=canyon-egress-admin", "--format", "{{.Names}}"))
	slices.Sort(mounted)
	if want := []string{"canyon-egress-control", "canyon-egress-envoy"}; !slices.Equal(mounted, want) {
		t.Errorf("canyon-egress-admin is mounted by %v, want %v and nothing else", mounted, want)
	}
	for line := range strings.SplitSeq(docker(t, "run", "--rm", "--network", behind, busybox, "netstat", "-ltn"), "\n") {
		f := strings.Fields(line)
		if len(f) != 6 || f[0] != "tcp" {
			continue
		}
		if local := f[3]; local != "0.0.0.0:3128" && local != "127.0.0.1:7783" && !strings.HasPrefix(local, "127.0.0.11:") { // Envoy's proxy, the check, Docker's resolver
			t.Errorf("Envoy's namespace listens on %s: only the proxy, the check and Docker's resolver do", local)
		}
	}
	// Envoy's own answer, on no network at all, to the uid both containers run as.
	stats := docker(t, "run", "--rm", "--network", "none", "--user", "65534:65534", "-v", "canyon-egress-admin:/var/run/envoy-admin:ro", curl, "-sS",
		"--unix-socket", socket, "-G", "--data-urlencode", "format=json",
		"--data-urlencode", `filter=^(access_logs\.grpc_access_log\.logs_dropped|cluster\.receiver\.upstream_rq_pending_failure_eject|server\.uptime)$`, "http://envoy/stats")
	t.Logf("Envoy answers over its socket: %s", stats)
	var answer struct {
		Stats []struct {
			Name  string  `json:"name"`
			Value *uint64 `json:"value"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(stats), &answer); err != nil {
		t.Fatalf("Envoy's stats are no JSON: %v\n%s", err, stats)
	}
	var names []string
	for _, s := range answer.Stats {
		if s.Value != nil {
			names = append(names, s.Name)
		}
	}
	if want := []string{"access_logs.grpc_access_log.logs_dropped", "cluster.receiver.upstream_rq_pending_failure_eject", "server.uptime"}; !slices.Equal(names, want) {
		t.Fatalf("the pinned Envoy names %v, want %v: egress-control would read every answer as failed", names, want)
	}

	since := time.Now().UTC().Format(time.RFC3339)
	uncounted := func() string {
		return ledger(t, "SELECT count(*) FROM egress_gaps WHERE reason = 'envoy_dropped' AND calls IS NULL AND to_at >= '"+since+"'")
	}
	time.Sleep(25 * time.Second) // two of egress-control's reads, every 10 s, and a flush after them
	if n := uncounted(); n != "0" {
		t.Fatalf("%s gaps no one counted on a healthy stack: egress-control cannot read the pinned Envoy\n%s", n, docker(t, "logs", "--tail", "20", checker))
	}

	stand := standIn(t, "198.51.100.0/24", holder)
	receiver := []string{"OUTPUT", "-d", addressOn(t, "canyon-server", ctl), "-p", "tcp", "--dport", "7781", "-j", "REJECT", "--reject-with", "tcp-reset"}
	docker(t, append([]string{"exec", holder, "iptables-nft", "-I"}, receiver...)...)
	t.Cleanup(func() { docker(t, append([]string{"exec", holder, "iptables-nft", "-D"}, receiver...)...) })
	if !eventually(func() bool {
		throughEnvoy(t, stand, "http://"+stand.addr+"/etc/dnsd.conf") // an entry, whatever the check answers it
		return uncounted() != "0"
	}) {
		t.Errorf("calls went on with the receiver refused, and the records hold no gap for them: a loss no one reads\n%s", docker(t, "logs", "--tail", "20", checker))
	}
}

// TestEnvoyTunnelIsItsRowWhileOpen: the pinned Envoy logs a tunnel as it opens (#2684), so a destination reached only
// through a tunnel still open is its row, named by the check's metadata, the call counted, while Envoy's own stats
// say the tunnel runs. Its periodic entry, a minute on, moves last_seen_at alone, which a reader of what preview
// connects to asks. Its end is the same call: the row keeps one and takes the last entry's result. A CONNECT the
// check refuses never opens: its one entry is one call and one refusal.
func TestEnvoyTunnelIsItsRowWhileOpen(t *testing.T) {
	floorUp(t)
	stand := standIn(t, "198.51.100.0/24", holder)
	docker(t, "exec", stand.name, "pkill", "-f", "httpd -p 443") // busybox httpd gives a connection 60 s, as long as the periodic entry takes
	docker(t, "exec", "-d", stand.name, "sh", "-c", "sleep 300 | nc -l -p 443 >/dev/null")
	app := routedApp(t)
	project := strings.TrimSuffix(app, "-preview")
	reachesFromApp(t, app)
	of := func(destination string) string {
		return "app = '" + project + "' AND env = 'preview' AND destination = '" + destination + "' AND port = 443"
	}
	row := func() string {
		return ledger(t, "SELECT calls || ' ' || last_protocol || ' ' || last_status_class || ' ' || last_outcome || ' [' || array_to_string(last_response_flags, ',') || ']' FROM egress_destinations WHERE "+of(publicName))
	}
	time.Sleep(11 * time.Second) // a flush: an earlier test's call to the name is written before its row goes
	ledger(t, "DELETE FROM egress_destinations WHERE "+of(publicName))

	const name = "tunnel"
	authority := publicName + ":443"
	start(t, name, "--network", "container:"+app, busybox, "sh", "-c",
		`(printf 'CONNECT `+authority+` HTTP/1.1\r\nHost: `+authority+`\r\n\r\n'; sleep 300) | nc canyon-egress 3128`)
	const open = "1 connect 2xx allowed []"
	if !eventually(func() bool { return row() == open }) {
		t.Fatalf("with a tunnel to %s open the row is %q, want %q: a destination reached through a tunnel is no row until it ends\n%s\nthe database's sessions at work:\n%s", authority, row(), open, docker(t, "logs", "--tail", "20", checker),
			ledger(t, "SELECT pid || ' ' || state || ' ' || coalesce(wait_event_type || '/' || wait_event, '-') || ' xact ' || coalesce((now() - xact_start)::text, '-') || ' ' || left(translate(query, chr(10) || chr(9), '  '), 120) FROM pg_stat_activity WHERE datname = 'canyon' AND state <> 'idle' AND pid <> pg_backend_pid()"))
	}
	stats := docker(t, "run", "--rm", "--network", "none", "--user", "65534:65534", "-v", "canyon-egress-admin:/var/run/envoy-admin:ro", curl, "-sS",
		"--unix-socket", "/var/run/envoy-admin/admin.sock", "-G", "--data-urlencode", "filter=^http\\.egress\\.downstream_rq_active$", "http://envoy/stats")
	if active := strings.TrimSpace(stats); active == "http.egress.downstream_rq_active: 0" || !strings.HasPrefix(active, "http.egress.downstream_rq_active: ") {
		t.Fatalf("Envoy says %q with the row written: the tunnel it was written from has ended, and the row proves nothing of one still open", active)
	}
	seen := func() string {
		return ledger(t, "SELECT last_seen_at > first_seen_at FROM egress_destinations WHERE "+of(publicName))
	}
	for deadline := time.Now().Add(90 * time.Second); seen() != "t"; time.Sleep(2 * time.Second) { // a minute's periodic entry and a flush
		if time.Now().After(deadline) {
			t.Fatalf("90 s into the tunnel the row is %q, last_seen_at past first_seen_at %q: a tunnel that runs on reads as last seen when it opened", row(), seen())
		}
	}
	if got := row(); got != open {
		t.Fatalf("after a periodic entry the row is %q, want %q: it moves last_seen_at alone", got, open)
	}
	docker(t, "kill", floorPrefix+tag+"-"+name)
	const ended = "1 connect 2xx allowed [DC]"
	if !eventually(func() bool { return row() == ended }) {
		t.Errorf("with the tunnel ended the row is %q, want %q: one call, the last entry's result", row(), ended)
	}

	setVia(t, "canyon") // an org mode is refused under via off
	t.Cleanup(func() { setMode(t, "observe"); setVia(t, "off") })
	setMode(t, "allowlist")
	refused := func() string { // the call the mode allowlist refuses: the policy reaches the check with its next version
		connect, _ := fromApp(t, app, "-p", "http://"+unlistedName+":443/etc/dnsd.conf")
		return connect
	}
	if !eventually(func() bool { return refused() == "403" }) {
		t.Fatalf("CONNECT %s:443 under the mode allowlist was answered %s, not the check's 403", unlistedName, refused())
	}
	time.Sleep(11 * time.Second) // a flush: every call above is written
	counts := func() string {
		return ledger(t, "SELECT d.calls || ' ' || r.refusals FROM egress_destinations d JOIN egress_requests r ON r.project_id = d.app AND r.env = d.env AND r.destination = d.destination WHERE d."+
			strings.ReplaceAll(of(unlistedName), " AND ", " AND d."))
	}
	before := counts()
	if got := refused(); got != "403" {
		t.Fatalf("CONNECT %s:443 under the mode allowlist was answered %s, not the check's 403", unlistedName, got)
	}
	was := strings.Fields(before)
	if len(was) != 2 {
		t.Fatalf("calls and refusals of %s are %q", unlistedName, before)
	}
	calls, _ := strconv.Atoi(was[0])
	refusals, _ := strconv.Atoi(was[1])
	want := strconv.Itoa(calls+1) + " " + strconv.Itoa(refusals+1)
	if !eventually(func() bool { return counts() == want }) {
		t.Errorf("after one refused CONNECT the calls and refusals of %s are %q, %q before it; want %q: it never opens, so its one entry is one call", unlistedName, counts(), before, want)
	}
}

// TestEnvoyCheckMetadataReachesTheReceiver: the check's dynamic metadata — app, env, outcome and reason, under
// envoy.filters.http.ext_authz — reaches the server's receiver on each call's access-log entry, on an OK answer
// and on a refusal alike (contracts/routes.md, Egress control): a namespace or an Envoy that dropped it would file
// nothing, silently. The receiver asks the executor's POST /callers for every call whose metadata names no
// caller, so with the executor stopped such a call is no row: the three calls made then are rows of the routed app
// through the metadata alone. Under the org's mode allowlist the public stand-in, on the app's allowlist, is
// answered OK, outcome allowed; another name is refused, not_allowed, and files an egress request, which only
// that reason does; an address is refused, invalid_destination, in every mode.
func TestEnvoyCheckMetadataReachesTheReceiver(t *testing.T) {
	floorUp(t)
	stand := standIn(t, "198.51.100.0/24", holder)
	app := routedApp(t)
	project := strings.TrimSuffix(app, "-preview")
	setVia(t, "canyon") // an org mode is refused under via off
	allowed := "/api/v1/projects/" + project + "/egress/preview/destinations/" + publicName
	t.Cleanup(func() { setMode(t, "observe"); setVia(t, "off"); api(t, "DELETE", allowed, nil) })
	if code, body := api(t, "PUT", allowed, nil); code != http.StatusNoContent {
		t.Fatalf("allowing %s for %s: %d %s", publicName, project, code, body)
	}
	setMode(t, "allowlist") // after the allowlist: a policy under the mode allowlist holds the entry
	// The mode reaches egress-control with the policy's next version: once a call to the private stand-in's name,
	// on no allowlist, files its request, the check answers under it, and the app's caller is in the policy it holds.
	filed := func(name string) bool {
		return ledger(t, "SELECT count(*) FROM egress_requests WHERE project_id = '"+project+"' AND env = 'preview' AND destination = '"+name+"' AND state = 'pending'") == "1"
	}
	if !eventually(func() bool { fromApp(t, app, "http://"+privateName+"/etc/dnsd.conf"); return filed(privateName) }) {
		t.Fatalf("a call from %s to %s under the mode allowlist filed no egress request: the calls below prove nothing", app, privateName)
	}
	calls := func(destination string) string { // the public stand-in's row holds calls from before: the window's call is one more
		return ledger(t, "SELECT coalesce(sum(calls), 0) FROM egress_destinations WHERE app = '"+project+"' AND env = 'preview' AND destination = '"+destination+"' AND port = 80")
	}
	before := calls(publicName)

	// The window: the server names a caller until it is 30 s unconfirmed, and confirms each every 15 s, so after the
	// executor stops the app's caller stays in the policy for 15 s at least — the calls are made inside them, and the
	// check names the app on both. Past them it could be unknown_caller, which proves nothing: the test fails.
	compose(t, "stop", "executor")
	stopped := time.Now()
	t.Cleanup(func() { compose(t, "start", "executor") })
	if _, status := fromApp(t, app, "http://"+publicName+"/etc/dnsd.conf"); status != "200" {
		t.Errorf("%s, on the app's allowlist, from %s: %s, want 200", publicName, app, status)
	}
	if _, status := fromApp(t, app, "http://"+unlistedName+"/etc/dnsd.conf"); status != "403" {
		t.Errorf("%s, on no allowlist, from %s: %s, want the check's 403 not_allowed", unlistedName, app, status)
	}
	if _, status := fromApp(t, app, "http://"+stand.addr+"/etc/dnsd.conf"); status != "403" {
		t.Errorf("%s, an address, from %s: %s, want the check's 403 invalid_destination", stand.addr, app, status)
	}
	if took := time.Since(stopped); took > 15*time.Second {
		t.Fatalf("the calls were made %v after the executor stopped, past the 15 s the app's caller is surely named for", took.Round(time.Second))
	}
	row := func(destination string) string {
		return ledger(t, "SELECT coalesce(min(last_outcome), '') FROM egress_destinations WHERE app = '"+project+"' AND env = 'preview' AND destination = '"+destination+"' AND port = 80")
	}
	if !eventually(func() bool { return calls(publicName) != before && row(unlistedName) != "" && row(stand.addr) != "" }) { // a flush, every 10 s, the executor still stopped
		t.Fatalf("with the executor stopped, the rows (%s, preview, …, 80) are %s: %s call(s) after %s, %s: %q, %s: %q; want all three, which only the check's metadata names the app for",
			project, publicName, calls(publicName), before, unlistedName, row(unlistedName), stand.addr, row(stand.addr))
	}
	if got := row(publicName); got != "allowed" {
		t.Errorf("%s's row's last_outcome is %q, want the OK answer's allowed", publicName, got)
	}
	if got := row(unlistedName); got != "refused" {
		t.Errorf("%s's row's last_outcome is %q, want the refusal's refused", unlistedName, got)
	}
	if got := row(stand.addr); got != "refused" {
		t.Errorf("%s's row's last_outcome is %q, want the refusal's refused", stand.addr, got)
	}
	if !filed(unlistedName) {
		t.Errorf("%s's not_allowed filed no egress request: the reason did not reach the receiver", unlistedName)
	}
}

// TestControlDownAnswers503WithBody: Envoy asks egress-control before every call and never lets one through
// without its answer (design/EGRESS.md §3.1, §6; failure_mode_allow false): the check up, a call from no app's
// address is its 403 unknown_caller; the check down, the same call, by an absolute-form GET and by CONNECT, is
// Envoy's 503 with Canyon's line, never the stand-in's answer; the check up again, its 403 again.
func TestControlDownAnswers503WithBody(t *testing.T) {
	floorUp(t)
	controlUp(t)
	stand := standIn(t, "198.51.100.0/24", holder)
	proxy := "http://" + addressOn(t, holder, stand.name) + ":3128"
	url := "http://" + stand.addr + "/etc/dnsd.conf"
	answered := func() (string, string) { // the GET's status and body, through Envoy
		_, out := exitOf("run", "--rm", "--network", stand.name, curl, "-s", "-m", "10", "-w", "\n%{http_code}", "-x", proxy, url)
		i := strings.LastIndex(out, "\n")
		return out[i+1:], out[:max(i, 0)]
	}
	const unknown = "Canyon refused this call: Canyon does not know which app is calling." // unknown_caller's line
	if !eventually(func() bool { status, body := answered(); return status == "403" && body == unknown }) {
		status, body := answered()
		t.Fatalf("a call from no app's address through Envoy, the check up, was answered %s %q, not its 403 unknown_caller", status, body)
	}
	docker(t, "stop", checker)
	t.Cleanup(func() { docker(t, "start", checker); controlUp(t) })
	const line = "Canyon's egress check did not answer, so this call was not made."
	if status, body := answered(); status != "503" || body != line {
		t.Errorf("GET %s through Envoy, the check down, was answered %s %q; want 503 %q", url, status, body, line)
	}
	if connect, _ := throughEnvoy(t, stand, "-p", "http://"+stand.addr+":443/etc/dnsd.conf"); connect != "503" {
		t.Errorf("CONNECT %s:443 through Envoy, the check down, was answered %s, not 503", stand.addr, connect)
	}
	docker(t, "start", checker)
	controlUp(t)
	if !eventually(func() bool { status, body := answered(); return status == "403" && body == unknown }) {
		status, body := answered()
		t.Errorf("a call through Envoy, the check up again, was answered %s %q, not its 403 unknown_caller", status, body)
	}
}

// controlUp fails while egress-control does not hold a policy: its healthcheck is the readiness probe.
func controlUp(t *testing.T) {
	t.Helper()
	healthy := func() bool {
		_, out := exitOf("inspect", "-f", "{{.State.Health.Status}}", checker)
		return out == "healthy\n"
	}
	if !eventually(healthy) {
		t.Fatalf("%s is not healthy: it holds no policy\n%s", checker, docker(t, "logs", "--tail", "20", checker))
	}
}

// setMode is PUT /api/v1/egress {mode}, the org's default mode, on the stack.
func setMode(t *testing.T, mode string) {
	t.Helper()
	if code, body := api(t, "PUT", "/api/v1/egress", map[string]string{"mode": mode}); code != http.StatusOK {
		t.Fatalf("PUT /api/v1/egress mode %s: %d %s", mode, code, body)
	}
}

// setVia is PUT /api/v1/egress on the stack, whose every caller is the demo's admin.
func setVia(t *testing.T, via string) {
	t.Helper()
	if code, body := api(t, "PUT", "/api/v1/egress", map[string]string{"via": via}); code != http.StatusOK {
		t.Fatalf("PUT /api/v1/egress via %s: %d %s", via, code, body)
	}
}

// setCredentialRoutes turns the host's credential routes on or off (#2143): while off no /run carries Canyon's root,
// no route is named and none can be added.
func setCredentialRoutes(t *testing.T, on bool) {
	t.Helper()
	if code, body := api(t, "PUT", "/api/v1/egress", map[string]bool{"credential_routes": on}); code != http.StatusOK {
		t.Fatalf("PUT /api/v1/egress credential_routes %t: %d %s", on, code, body)
	}
}

// deployTiny deploys a Node app that answers on $PORT and calls nothing, as app, to preview, and answers its run.
func deployTiny(t *testing.T, app string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"canyon.toml":  "app = \"" + app + "\"\n",
		"package.json": `{"name": "` + app + `", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}, "engines": {"node": "22"}}` + "\n",
		"server.js":    "require(\"node:http\").createServer((_, res) => res.end(\"ok\\n\")).listen(process.env.PORT || 3000);\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := api(t, "POST", "/api/v1/projects", map[string]string{"id": app}); code != http.StatusCreated && !madeAlready(code, out) {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, code, out)
	}
	deployed, code := cliJSON(t, dir, "deploy")
	if code != 0 || deployed.Deployment == nil {
		t.Fatalf("deploy %s: exit %d, %s, deployment %+v", app, code, deployed.Result, deployed.Deployment)
	}
	waitRun(t, deployed.GateRun, time.Minute)
	return deployed.GateRun
}

func envOf(t *testing.T, container string) []string {
	t.Helper()
	return strings.Split(docker(t, "inspect", "-f", `{{join .Config.Env "\n"}}`, container), "\n")
}

var pulls sync.Once

// floorUp fails while the holder has not applied its floor, and skips without a stack — never under CI,
// where a skipped floor would pass unseen. The first call pulls the probes' images and removes what a
// killed run left behind.
func floorUp(t *testing.T) {
	t.Helper()
	if !ready() {
		if os.Getenv("CI") != "" {
			t.Fatalf("no Canyon stack answers %s/ready", host)
		}
		t.Skipf("no Canyon stack answers %s/ready: start one with make e2e-up", host)
	}
	pulls.Do(func() {
		docker(t, "pull", "-q", curl) // pulled once here: a pull's progress would sit in a container id's place
		docker(t, "pull", "-q", busybox)
		_, ids := exitOf("ps", "-aq", "--filter", "name=^"+floorPrefix)
		for id := range strings.FieldsSeq(ids) {
			exitOf("rm", "-f", id)
		}
		_, nets := exitOf("network", "ls", "-q", "--filter", "name=^"+floorPrefix)
		for id := range strings.FieldsSeq(nets) {
			exitOf("network", "disconnect", "-f", id, holder)
			exitOf("network", "rm", id)
		}
	})
	healthy := func() bool {
		_, out := exitOf("inspect", "-f", "{{.State.Health.Status}}", holder)
		return out == "healthy\n"
	}
	if !eventually(healthy) {
		t.Fatalf("%s is not healthy: its floor is not in place\n%s", holder, docker(t, "logs", holder))
	}
}

// eventually tries ok for about 30 seconds: a container just started, a holder just recreated.
func eventually(ok func() bool) bool {
	for range 30 {
		if ok() {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// A network is a container network to probe from, and the address beside the holder that it reaches.
type network struct{ name, addr string }

// standIn makes a network on subnet with a stand-in at .9 (standInAt) and joins the holder to it, as the
// executor will join it to each app's network.
func standIn(t *testing.T, subnet, holder string) network {
	t.Helper()
	n := standInAt(t, subnet)
	docker(t, "network", "connect", n.name, holder)
	t.Cleanup(func() { docker(t, "network", "disconnect", n.name, holder) })
	return n
}

// standInAt makes a network on subnet with a stand-in at .9 serving HTTP on 80, 443, 8443, 7781 and TCP 53,
// and canyon-floor.test on UDP 53.
func standInAt(t *testing.T, subnet string) network {
	t.Helper()
	prefix, _, _ := strings.Cut(subnet, "/")
	n := network{floorPrefix + tag + "-" + strings.ReplaceAll(prefix, ".", "-"), strings.TrimSuffix(prefix, "0") + "9"}
	docker(t, "network", "create", "--subnet", subnet, n.name)
	t.Cleanup(func() { docker(t, "network", "rm", n.name) })
	start(t, strings.TrimPrefix(n.name, floorPrefix+tag+"-"), "--network", n.name, "--ip", n.addr, busybox, "sh", "-c",
		standInCmd("httpd -p 80 && httpd -p 443 && httpd -p 8443 && httpd -p 7781 && httpd -p 53 && exec dnsd -c /etc/dnsd.conf"))
	return n
}

// standInCmd writes the stand-in resolver's one name before cmd.
func standInCmd(cmd string) string {
	return `echo "canyon-floor.test 192.0.2.1" > /etc/dnsd.conf && ` + cmd
}

// keptAlive is a busybox script that opens n connections to Envoy at proxy:3128 and holds each for 10 minutes
// as a kept-alive client does, a request through it to addr every 20 s, so Envoy's 60 s idle timeout, which
// closes a connection with no request open (design/EGRESS.md §2.1), closes none while a test holds them.
func keptAlive(n int, proxy, addr string) string {
	request := `GET http://` + addr + `/etc/dnsd.conf HTTP/1.1\r\nHost: ` + addr + `\r\n\r\n`
	return "for i in $(seq " + strconv.Itoa(n) + "); do (for j in $(seq 30); do printf '" + request + "'; sleep 20; done) | nc " + proxy + " 3128 >/dev/null & done; wait"
}

// start runs a container named for this run in the background, which the test removes.
func start(t *testing.T, name string, args ...string) {
	t.Helper()
	id := docker(t, append([]string{"run", "-d", "--name", floorPrefix + tag + "-" + name}, args...)...)
	t.Cleanup(func() { docker(t, "rm", "-f", id) })
}

// scratchHolder is docker run's arguments for a holder like compose's, on its image and floor.sh, with env,
// on network (docker's default when ""), with NET_ADMIN or no capability at all, running floor.sh or script.
func scratchHolder(t *testing.T, network string, netAdmin bool, env []string, script ...string) []string {
	t.Helper()
	floor, err := filepath.Abs(filepath.Join("..", "deploy", "egress", "floor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only", "--init",
		"--sysctl", "net.ipv4.ip_forward=0", "--sysctl", "net.ipv6.conf.all.disable_ipv6=1", "--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"-v", floor + ":/etc/canyon/floor.sh:ro", "-e", "CANYON_EGRESS_RECEIVER_ADDR=" + holderEnv(t, "CANYON_EGRESS_RECEIVER_ADDR"),
		"-v", "canyon-egress-host:/run/canyon-host:ro", "-e", "CANYON_EGRESS_HOST_FILE=" + holderEnv(t, "CANYON_EGRESS_HOST_FILE"), // egress-host's listing, as the holder's
		"-e", "CANYON_EGRESS_PER_SOURCE=" + holderEnv(t, "CANYON_EGRESS_PER_SOURCE")}
	if netAdmin {
		args = append(args, "--cap-add", "NET_ADMIN")
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, "--entrypoint", "/bin/sh", docker(t, "inspect", "-f", "{{.Config.Image}}", holder))
	if len(script) > 0 {
		return append(args, "-c", script[0])
	}
	return append(args, "/etc/canyon/floor.sh")
}

// refused: url is reached from n's network, beside the holder, and refused from behind it (or from).
func refused(t *testing.T, n network, url string, from ...string) {
	t.Helper()
	if !eventually(func() bool { return reached(t, n.name, url) }) {
		t.Fatalf("%s was not reached from %s, beside the holder: its refusal behind the holder would prove nothing", url, n.name)
	}
	by := behind
	if len(from) > 0 {
		by = from[0]
	}
	if reached(t, by, url) {
		t.Errorf("%s was reached from behind the holder (%s)", url, by)
	}
}

// reached says whether curl, on network from, connected to url. The floor refuses at once (REJECT), so
// any other failure — a timeout, a TLS error — fails the test rather than counting as either answer.
func reached(t *testing.T, from, url string) bool {
	t.Helper()
	code, out := exitOf("run", "--rm", "--network", from, curl, "-sS", "-o", "/dev/null", "-m", "10", url)
	switch code {
	case 0:
		return true
	case 7: // could not connect
		return false
	}
	t.Fatalf("curl %s from %s: exit %d, neither reached nor refused\n%s", url, from, code, out)
	return false
}

// connected is reached for a listener that may close a connection at accept, as the server under sign-in off
// closes every peer but this machine's (#2449): a connection closed with no reply (curl's 52, 56) connected too.
// The floor refuses at once (REJECT, 7); any other failure fails the test.
func connected(t *testing.T, from, url string) bool {
	t.Helper()
	code, out := exitOf("run", "--rm", "--network", from, curl, "-sS", "-o", "/dev/null", "-m", "10", url)
	switch code {
	case 0, 52, 56:
		return true
	case 7:
		return false
	}
	t.Fatalf("curl %s from %s: exit %d, neither connected nor refused\n%s", url, from, code, out)
	return false
}

// resolves says whether the stand-in resolver at server answers canyon-floor.test on UDP 53 from network from.
func resolves(t *testing.T, from, server string) bool {
	t.Helper()
	return resolvesName(t, from, server, "canyon-floor.test")
}

func resolvesName(t *testing.T, from, server, name string) bool {
	t.Helper()
	code, out := exitOf("run", "--rm", "--network", from, busybox, "nslookup", "-type=a", name, server)
	if code > 1 { // docker's own failure, never nslookup's answer
		t.Fatalf("nslookup %s %s from %s: exit %d\n%s", name, server, from, code, out)
	}
	return code == 0
}

// addressOn is container's address on network.
func addressOn(t *testing.T, container, network string) string {
	t.Helper()
	return docker(t, "inspect", "-f", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, container)
}

func resolversOf(t *testing.T) []string {
	t.Helper()
	return strings.Split(holderEnv(t, "CANYON_EGRESS_RESOLVERS"), ",")
}

func holderEnv(t *testing.T, key string) string {
	t.Helper()
	for kv := range strings.SplitSeq(docker(t, "inspect", "-f", `{{join .Config.Env "\n"}}`, holder), "\n") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	t.Fatalf("%s has no %s", holder, key)
	return ""
}

// bounded runs docker with args for at most a minute: a floor.sh that went on to sleep fails, never hangs.
func bounded(t *testing.T, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // the test's own docker arguments
	if ctx.Err() != nil {
		t.Fatalf("docker %s still ran after a minute\n%s", strings.Join(args, " "), out)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func exitOf(args ...string) (int, string) {
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // the test's own docker arguments
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	}
	return -1, err.Error()
}

// grpcAnswers says whether a gRPC server answers at addr from the network from: one answers a request that is
// no gRPC call 415, never a stand-in's HTTP.
func grpcAnswers(t *testing.T, from, addr string) bool {
	t.Helper()
	code, out := exitOf("run", "--rm", "--network", from, curl, "-sS", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}", "--http2-prior-knowledge", "http://"+addr+"/")
	switch {
	case code == 0 && out == "415":
		return true
	case code == 7: // could not connect
		return false
	}
	t.Fatalf("curl --http2-prior-knowledge http://%s/ from %s: exit %d, neither a gRPC server's 415 nor refused\n%s", addr, from, code, out)
	return false
}

// answeredByTheCheck says whether a call from n's network through Envoy, to its stand-in by address, is the
// check's 403 within eventually's time: Envoy is up and asks the check, and n holds no app.
func answeredByTheCheck(t *testing.T, n network) bool {
	t.Helper()
	return eventually(func() bool {
		_, status := throughEnvoy(t, n, "http://"+n.addr+"/etc/dnsd.conf")
		return status == "403"
	})
}

// throughEnvoy is curl with args on n's network through Envoy, at the holder's address there: what Envoy
// answered a CONNECT and the response's status, each 000 when there was none.
func throughEnvoy(t *testing.T, n network, args ...string) (connect, status string) {
	t.Helper()
	return codesOf(t, n.name, "http://"+addressOn(t, holder, n.name)+":3128", args...)
}

// fromApp is curl with args through Envoy from app's own namespace, at http://canyon-egress:3128, as the app's
// own call is: what Envoy answered a CONNECT and the response's status.
func fromApp(t *testing.T, app string, args ...string) (connect, status string) {
	t.Helper()
	return codesOf(t, "container:"+app, "http://canyon-egress:3128", args...)
}

func codesOf(t *testing.T, from, proxy string, args ...string) (connect, status string) {
	t.Helper()
	_, out := exitOf(append([]string{"run", "--rm", "--network", from, curl, "-s", "-o", "/dev/null", "-m", "10", "-w", "\n%{http_connect} %{http_code}", "-x", proxy}, args...)...)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	connect, status, ok := strings.Cut(lines[len(lines)-1], " ")
	if !ok || len(connect) != 3 || len(status) != 3 {
		t.Fatalf("curl %v through %s from %s answered no codes\n%s", args, proxy, from, out)
	}
	return connect, status
}

// The names of the stand-ins: the check refuses an IP literal (design/EGRESS.md §3.3), so a call that must
// reach one names it. e2e/compose.e2e.yml gives them to the holder alone, whose /etc/hosts Envoy shares and
// resolves first; the shipped compose file never carries them (deploy/egress/compose_test.go).
const (
	publicName   = "public.canyon-floor.test"   // the public stand-in, 198.51.100.9
	unlistedName = "unlisted.canyon-floor.test" // the same, a name no call makes before the metadata's test
	privateName  = "private.canyon-floor.test"  // the private stand-in, 10.250.87.9
	serverName   = "server.canyon-floor.test"   // the server's address on canyon-egress-ctl
	hostName     = "host.canyon-floor.test"     // 198.51.100.200, the Docker host's own, on its loopback
)

var routed struct {
	once     sync.Once
	project  string // the app, once its deploy was asked for; TestMain forgets it
	deployed bool
}

// routedApp deploys, once for the run, a tiny app while the host says via canyon, and answers its preview's
// container: an app whose calls the check knows by its address (design/EGRESS.md §3.2), as no stand-in's are.
// The host's via is put back to off; the env stays named, as its deploy row says canyon.
func routedApp(t *testing.T) string {
	t.Helper()
	routed.once.Do(func() {
		routed.project = "e2e-routed-" + tag
		setVia(t, "canyon")
		defer setVia(t, "off")
		setCredentialRoutes(t, true) // its /run carries Canyon's root, which the interception tests' calls need (#2143)
		defer setCredentialRoutes(t, false)
		deployTiny(t, routed.project)
		routed.deployed = true
	})
	if !routed.deployed {
		t.Fatal("the app routed through Canyon did not deploy: the first test that asked for it says why")
	}
	return routed.project + "-preview"
}

// forgetRouted deletes routedApp's project at the end of the run, when there was one.
func forgetRouted() {
	if routed.project == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, host+"/api/v1/projects/"+routed.project+"?confirm="+routed.project, nil) //nolint:noctx // one call at the run's end
	if err == nil {
		var res *http.Response
		if res, err = (&http.Client{Timeout: time.Minute}).Do(req); err == nil {
			_ = res.Body.Close()
		}
	}
}

// reachesFromApp fails unless app reaches the public stand-in by name through Envoy within eventually's time: the
// check knows its caller, and Envoy resolves the stand-in's name.
func reachesFromApp(t *testing.T, app string) {
	t.Helper()
	if !eventually(func() bool {
		_, status := fromApp(t, app, "http://"+publicName+"/etc/dnsd.conf")
		return status == "200"
	}) {
		_, status := fromApp(t, app, "http://"+publicName+"/etc/dnsd.conf")
		t.Fatalf("http://%s/etc/dnsd.conf from %s through Envoy was answered %s, not the stand-in's 200: the check does not know the app, or Envoy "+
			"does not resolve e2e/compose.e2e.yml's names; the calls below prove nothing", publicName, app, status)
	}
}

// setting is the admin's read of the host's egress setting (contracts/routes.md, Egress control).
type setting struct {
	Recreate    []string   `json:"networks_to_recreate"`
	Receiver    bool       `json:"receiver"`
	Streams     int        `json:"streams"`
	LastEntryAt *time.Time `json:"last_entry_at"`
}

// egressSetting reads it: on the E2E stack every caller is an admin (CANYON_AUTH=demo).
func egressSetting(t *testing.T) setting {
	t.Helper()
	code, body := api(t, "GET", "/api/v1/egress", nil)
	var s setting
	if err := json.Unmarshal(body, &s); code != 200 || err != nil {
		t.Fatalf("GET /api/v1/egress: %d %v\n%s", code, err, body)
	}
	return s
}

// unattributed is how many calls the unattributed gaps count: calls no app is named for.
func unattributed(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(ledger(t, "SELECT coalesce(sum(calls), 0) FROM egress_gaps WHERE reason = 'unattributed'"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ledger runs one statement in Canyon's own Postgres and answers the unaligned result.
func ledger(t *testing.T, sql string) string {
	t.Helper()
	return compose(t, "exec", "-T", "postgres", "psql", "-U", "canyon", "-d", "canyon", "-v", "ON_ERROR_STOP=1", "-tAc", sql)
}
