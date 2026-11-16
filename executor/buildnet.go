package executor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// buildNetwork is the network every build's holder is on (contracts/executor.md, A build reaches no private
// or link-local address): a bridge, so a build reaches public registries, never the default one.
const buildNetwork = "canyon-builds"

// HolderImage is the stock iptables image each build's holder runs: Kubernetes' distroless-iptables,
// pinned by digest, the very pin docker-compose.yml's canyon-egress names (deploy/egress's
// TestTheBuildsHolderIsEgress holds the two equal).
const HolderImage = "registry.k8s.io/build-image/distroless-iptables:v0.11.0@sha256:bedc7f0c0dc8e42a316f1cd6d6ee321eb4159e1f75ac30875bdfcf13446bc7a0"

// buildFloor is the holder's entrypoint: the floor, then sleep.
//
//go:embed buildfloor.sh
var buildFloor string

// floorWait bounds how long a build waits for its holder's floor; a var so a test can lower it.
var floorWait = 30 * time.Second

func (e *Executor) holder() string { return or(e.Holder, HolderImage) }

// buildNet is what docker network inspect says of canyon-builds.
type buildNet struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Internal   bool              `json:"Internal"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	IPAM       struct {
		Config []ipamConfig `json:"Config"`
	} `json:"IPAM"`
}

// buildSubnet is canyon-builds' one IPv4 subnet, the network created first when the daemon has none. A
// network of that name that is not exactly what the executor creates is refused, never adopted.
func (e *Executor) buildSubnet(ctx context.Context) (netip.Prefix, error) {
	inspect := func() (buildNet, bool) {
		out, _ := child(ctx, e.docker(), "network", "inspect", "--format", "{{json .}}", buildNetwork).Output()
		var n buildNet
		return n, json.Unmarshal(out, &n) == nil && n.Name == buildNetwork
	}
	n, exists := inspect()
	if !exists {
		out, err := child(ctx, e.docker(), "network", "create", "--driver", "bridge", "--ipv6=false",
			"-o", "com.docker.network.bridge.enable_icc=false", "--label", "canyon.build", buildNetwork).CombinedOutput()
		if n, exists = inspect(); !exists { // a create that lost a race to another build finds the network made
			return netip.Prefix{}, fmt.Errorf("network create %s: %v: %s", buildNetwork, err, strings.TrimSpace(string(out)))
		}
	}
	var subnets []netip.Prefix
	for _, c := range n.IPAM.Config {
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
			subnets = append(subnets, p.Masked())
		}
	}
	_, labelled := n.Labels["canyon.build"]
	if n.Driver != "bridge" || n.EnableIPv6 || n.Internal || !labelled || n.Options["com.docker.network.bridge.enable_icc"] != "false" || len(subnets) != 1 {
		return netip.Prefix{}, fmt.Errorf("the network %s is not Canyon's: a bridge labelled canyon.build, with ICC and IPv6 off and one IPv4 subnet; remove it and build again", buildNetwork)
	}
	return subnets[0], nil
}

// floored starts the build's holder, canyon-build-<nonce>, and answers its name once the floor is in place in
// its network namespace, and the call that removes it. Anything short of that is an error, and no holder is
// left: a build never runs without its floor.
func (e *Executor) floored(ctx context.Context) (string, func(), error) {
	subnet, err := e.buildSubnet(ctx)
	if err != nil {
		return "", nil, err
	}
	host, err := e.hostAddrs(ctx)
	if err != nil {
		return "", nil, err
	}
	name := "canyon-build-" + nonce()
	remove := func() {
		if out, err := child(context.WithoutCancel(ctx), e.docker(), "rm", "-f", name).CombinedOutput(); err != nil {
			log.Printf("executor: rm %s: %s", name, strings.TrimSpace(string(out)))
		}
	}
	args := []string{"run", "-d", "--name", name, "--network", buildNetwork, "--restart", "no", "--label", "canyon.build",
		"--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges", "--read-only",
		"--memory", "64m", "--pids-limit", "32", "--entrypoint", "/bin/sh",
		"--env", "CANYON_BUILD_SUBNET=" + subnet.String(), "--env", "CANYON_BUILD_HOST=" + prefixes(host), "--env", "CANYON_BUILD_POOLS=" + prefixes(e.Refused()),
		"--env", "CANYON_BUILD_ALLOW=" + strings.Join(e.Allow, ","), e.holder(), "-c", buildFloor}
	if out, err := child(ctx, e.docker(), args...).CombinedOutput(); err != nil {
		remove() // a run the daemon refused may have created the container all the same
		return "", nil, fmt.Errorf("the holder %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	if err := e.floorIn(ctx, name, "CANYON-BUILD"); err != nil {
		remove()
		return "", nil, err
	}
	return name, remove, nil
}

// floorIn waits, for floorWait at most, until the holder name's floor is in place: the jump into chain at the head
// of OUTPUT, its script's last rule, and of ip6tables' too where the namespace has IPv6. Anything short of that is
// an error naming the holder's last lines.
func (e *Executor) floorIn(ctx context.Context, name, chain string) error {
	check := "iptables-nft -C OUTPUT -j " + chain + " && { [ ! -e /proc/net/if_inet6 ] || ip6tables-nft -C OUTPUT -j " + chain + "; }"
	deadline := time.Now().Add(floorWait)
	for {
		if child(ctx, e.docker(), "exec", name, "/bin/sh", "-c", check).Run() == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			said, _ := child(context.WithoutCancel(ctx), e.docker(), "logs", "--tail", "5", name).CombinedOutput()
			return fmt.Errorf("the floor of %s was not in place within %s: %s", name, floorWait, strings.TrimSpace(string(said)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// nonRoot refuses a builder whose image runs as root: the app's own scripts run as its user, and a root one
// holds NET_RAW, whose frames pass the floor. A numeric user is read as Docker reads it, so 00 is root too; a
// name is resolved by the image's own id, in a container that reaches no network; one that cannot be resolved
// is refused.
func (e *Executor) nonRoot(ctx context.Context, builder string) error {
	out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{.Id}} {{.Config.User}}", builder).Output()
	if err != nil {
		return &HostFailed{Message: fmt.Sprintf("builder %s: %v", builder, err)} // the daemon's (#2537)
	}
	id, user, _ := strings.Cut(strings.TrimRight(string(out), "\n"), " ")
	if _, known := e.nonRoots.Load(id); known && imageIDRe.MatchString(id) { // this image's user was read before: an ID is its bytes
		return nil
	}
	user, _, _ = strings.Cut(user, ":")
	uid, err := strconv.ParseUint(user, 10, 32)
	if user != "" && err != nil { // a name: the uid the image's own passwd gives it
		out, runErr := child(ctx, e.docker(), "run", "--rm", "--network", "none", "--cap-drop", "ALL", "--entrypoint", "id", builder, "-u").Output()
		if uid, err = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32); runErr != nil || err != nil {
			return &HostFailed{Message: fmt.Sprintf("the builder %s runs its build as %q, whose uid cannot be read: name a CANYON_BUILDER whose image user is a uid that is not 0", builder, user)}
		}
	}
	if user == "" || uid == 0 {
		return &HostFailed{Message: fmt.Sprintf("the builder %s runs its build as root: name a CANYON_BUILDER whose image user is not root", builder)}
	}
	e.nonRoots.Store(id, true)
	return nil
}

// prefixes is ps comma-separated.
func prefixes(ps []netip.Prefix) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return strings.Join(out, ",")
}

// carried is the builder's buildpacks from its metadata label, each by its id and by id@version.
func (e *Executor) carried(ctx context.Context, builder string) (map[string]bool, error) {
	labels, err := e.labels(ctx, builder)
	if err != nil {
		return nil, err
	}
	var m struct {
		Buildpacks []struct{ ID, Version string } `json:"buildpacks"`
	}
	if err := json.Unmarshal([]byte(labels[MetadataLabel]), &m); err != nil {
		return nil, fmt.Errorf("its %s label: %w", MetadataLabel, err)
	}
	carried := map[string]bool{}
	for _, bp := range m.Buildpacks {
		carried[bp.ID] = true
		if bp.Version != "" {
			carried[bp.ID+"@"+bp.Version] = true
		}
	}
	return carried, nil
}

// forbidden is what no CANYON_BUILD_ALLOW entry may overlap: link-local and the instance metadata
// addresses within it, Azure's WireServer, Alibaba's metadata, and the unroutable ranges.
var forbidden = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("100.100.100.200/32"), netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4")}

// canyonPools is CANYON_EGRESS_POOLS unset: the bundled compose file's, the apps' pool and Canyon's own networks.
var canyonPools = []netip.Prefix{netip.MustParsePrefix("10.213.0.0/16"), netip.MustParsePrefix("10.214.0.0/16")}

// CanyonPools is CANYON_EGRESS_POOLS, which egress's floor refuses whole: Canyon's networks, comma-separated
// IPv4 CIDRs, the apps' pool and Canyon's own, which a build's floor refuses before any CANYON_BUILD_ALLOW entry
// too. Unset, the bundled compose file's; anything but CIDRs stops the executor's start.
func CanyonPools(getenv func(string) string) ([]netip.Prefix, error) {
	v := getenv("CANYON_EGRESS_POOLS")
	if v == "" {
		return canyonPools, nil
	}
	var pools []netip.Prefix
	for _, entry := range strings.Split(v, ",") {
		p, err := netip.ParsePrefix(entry)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return nil, fmt.Errorf("CANYON_EGRESS_POOLS: %q is not an IPv4 CIDR, such as 10.214.0.0/16", entry)
		}
		pools = append(pools, p)
	}
	return pools, nil
}

// Refused is what a build's floor rejects before any CANYON_BUILD_ALLOW entry besides forbidden: the apps' pool
// and Canyon's own networks, where the control plane's Postgres and the executor's own API listen.
func (e *Executor) Refused() []netip.Prefix {
	canyon := e.Canyon
	if canyon == nil {
		canyon = canyonPools
	}
	refused := []netip.Prefix{e.pool()}
	for _, p := range canyon {
		if !slices.Contains(refused, p) { // compose's apps pool is among CANYON_EGRESS_POOLS too
			refused = append(refused, p)
		}
	}
	return refused
}

// hostAliases is where host.docker.internal is on Docker Desktop (192.168.65.254, .2 before it), Colima (192.168.5.2,
// beside the VM's own) and Podman (10.0.2.2): the host itself, under an address no listing of the VM's may show, so
// each of Desktop's and Colima's /24s is refused whole. OrbStack's is in 0.0.0.0/8, forbidden already.
var hostAliases = []netip.Prefix{netip.MustParsePrefix("192.168.65.0/24"), netip.MustParsePrefix("192.168.5.0/24"), netip.MustParsePrefix("10.0.2.2/32")}

// AppsAllow is CANYON_APPS_ALLOW, read as AllowSetting reads it, and refusing besides any entry that overlaps a
// vendor's host.docker.internal, which would open the host itself to every app.
func AppsAllow(getenv func(string) string, refused ...netip.Prefix) ([]string, error) {
	allow, err := AllowSetting(getenv, "CANYON_APPS_ALLOW", refused...)
	if err != nil {
		return nil, err
	}
	for _, entry := range allow {
		cidr, _, _ := strings.Cut(entry, ":")
		p := netip.MustParsePrefix(cidr) // AllowSetting parsed it
		for _, a := range hostAliases {
			if p.Overlaps(a) {
				return nil, fmt.Errorf("CANYON_APPS_ALLOW: %q overlaps %s, where host.docker.internal is on Docker Desktop, Colima or Podman: the host itself, which no app may reach", entry, a)
			}
		}
	}
	return allow, nil
}

// AllowSetting is CANYON_BUILD_ALLOW or CANYON_APPS_ALLOW, as name says: the addresses and ports a build, or a
// running app, may reach besides what its floor admits, each <IPv4 prefix>:<one TCP port>, comma-separated. Any
// entry out of that shape, shorter than a /8, or overlapping a forbidden range or one of refused, Canyon's
// networks (every app network's gateway among them), is an error, which stops the executor's start.
func AllowSetting(getenv func(string) string, name string, refused ...netip.Prefix) ([]string, error) {
	v := getenv(name)
	if v == "" {
		return nil, nil
	}
	var allow []string
	for _, entry := range strings.Split(v, ",") {
		cidr, port, _ := strings.Cut(entry, ":")
		p, perr := netip.ParsePrefix(cidr)
		n, nerr := strconv.ParseUint(port, 10, 16)
		switch {
		case perr != nil || !p.Addr().Is4() || p != p.Masked() || nerr != nil || n == 0 || strconv.FormatUint(n, 10) != port:
			return nil, fmt.Errorf("%s: %q is not <IPv4 prefix>:<TCP port>, such as 10.20.0.0/16:443", name, entry)
		case p.Bits() < 8:
			return nil, fmt.Errorf("%s: %q is wider than a /8", name, entry)
		}
		for _, f := range append(slices.Clone(forbidden), refused...) {
			if p.Overlaps(f) {
				return nil, fmt.Errorf("%s: %q overlaps %s, which nothing Canyon runs may reach", name, entry, f)
			}
		}
		allow = append(allow, entry)
	}
	return allow, nil
}
