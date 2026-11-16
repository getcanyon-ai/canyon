package egress_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The per-source cap on Envoy's listener (design/EGRESS.md §2.1), as floor.sh applies it.
const perSource = "iptables-nft -A INPUT -p tcp --syn --dport 3128 -m connlimit --connlimit-above 512 --connlimit-mask 32 -j REJECT --reject-with tcp-reset"

// bootID is the kernel's boot_id under which floor.sh runs, as /proc/sys/kernel/random/boot_id holds it.
const bootID = "6f1c0b9a-7e2a-4d3c-9f1c-0b9a7e2a9d3c"

// hostListing is what egress-host writes of a Linux host with a private and a public address on one interface:
// the boot it was written under, then `ip addr show` in the host's network namespace, then end.
const hostListing = "boot_id " + bootID + "\n1: lo: <LOOPBACK,UP> mtu 65536\n    inet 127.0.0.1/8 scope host lo\n2: eth0: <UP> mtu 1500\n" +
	"    inet 10.0.0.4/24 brd 10.0.0.255 scope global eth0\n    inet 203.0.113.7/32 scope global eth0\n    inet6 fe80::1/64 scope link\nend\n"

// floor runs floor.sh with CANYON_EGRESS_PER_SOURCE=perSource beside good values, the host's listing hostListing,
// its iptables-nft, ip6tables-nft and sleep stand-ins writing each call to a log: the calls in order, and what it printed.
func floor(t *testing.T, perSource string) ([]string, string, error) {
	t.Helper()
	listing := hostListing
	return floorListing(t, perSource, &listing)
}

// floorListing is floor with the host's listing listing, or none at all when nil.
func floorListing(t *testing.T, perSource string, listing *string) ([]string, string, error) {
	t.Helper()
	bin := t.TempDir()
	hostFile := filepath.Join(bin, "addrs")
	if listing != nil {
		if err := os.WriteFile(hostFile, []byte(*listing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bootFile := filepath.Join(bin, "boot_id")
	if err := os.WriteFile(bootFile, []byte(bootID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(bin, "calls")
	for _, name := range []string{"iptables-nft", "ip6tables-nft", "sleep"} {
		stub := "#!/bin/sh\necho \"" + name + " $*\" >>" + calls + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil { //nolint:gosec // the test's stand-ins, executable
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "floor.sh")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CANYON_EGRESS_RECEIVER_ADDR=10.214.3.2:7781",
		"CANYON_EGRESS_RESOLVERS=8.8.8.8", "CANYON_EGRESS_POOLS=10.213.0.0/16,10.214.0.0/16", "CANYON_EGRESS_PER_SOURCE=" + perSource,
		"CANYON_EGRESS_HOST_FILE=" + hostFile, "CANYON_EGRESS_BOOT_ID_FILE=" + bootFile}
	out, err := cmd.CombinedOutput()
	log, readErr := os.ReadFile(calls) //nolint:gosec // the test's own temporary file
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return strings.Split(strings.TrimSpace(string(log)), "\n"), string(out), err
}

// The cap is in place before the jump into the floor's chain, the last rule, which the holder's healthcheck
// asks for: Envoy, which starts once the holder is healthy, never listens without it.
func TestFloorCapsEachSourceOnEnvoysListener(t *testing.T) {
	calls, out, err := floor(t, "512")
	if err != nil {
		t.Fatalf("floor.sh: %v\n%s", err, out)
	}
	at := slices.Index(calls, perSource)
	jump := slices.Index(calls, "iptables-nft -I OUTPUT 1 -j CANYON-FLOOR")
	if at < 0 || slices.Index(calls[at+1:], perSource) >= 0 {
		t.Fatalf("floor.sh adds the per-source cap %d times, not once:\n%s", strings.Count(strings.Join(calls, "\n"), perSource), strings.Join(calls, "\n"))
	}
	if jump != len(calls)-2 || calls[len(calls)-1] != "sleep infinity" || at > jump {
		t.Errorf("the per-source cap is call %d, the jump into the floor's chain %d of %d, sleep last: want the cap before the jump, the jump the last rule\n%s",
			at, jump, len(calls), strings.Join(calls, "\n"))
	}
}

// Envoy's floor rejects every private, shared, link-local and reserved range before public 80 and 443 return:
// 0.0.0.0/8 among them, where OrbStack puts host.docker.internal (0.250.250.254), which 80 and 443 would let through.
func TestFloorRejectsTheReservedRangesBeforePublicPorts(t *testing.T) {
	calls, out, err := floor(t, "512")
	if err != nil {
		t.Fatalf("floor.sh: %v\n%s", err, out)
	}
	public := slices.Index(calls, "iptables-nft -A CANYON-FLOOR -p tcp --dport 80 -j RETURN")
	for _, r := range []string{"0.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "168.63.129.16/32", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4"} {
		if at := slices.Index(calls, "iptables-nft -A CANYON-FLOOR -d "+r+" -j REJECT"); at < 0 || public < 0 || at > public {
			t.Errorf("%s is rejected at call %d, public 80 returns at %d: want it rejected before\n%s", r, at, public, strings.Join(calls, "\n"))
		}
	}
}

// Envoy's floor rejects every address of the Docker host's own that egress-host listed, a public one among them,
// before public 80 and 443 return (#2524): an app routed through Envoy reaches no service of the host's.
func TestFloorRejectsTheHostsOwnAddressesBeforePublicPorts(t *testing.T) {
	calls, out, err := floor(t, "512")
	if err != nil {
		t.Fatalf("floor.sh: %v\n%s", err, out)
	}
	public := slices.Index(calls, "iptables-nft -A CANYON-FLOOR -p tcp --dport 80 -j RETURN")
	for _, a := range []string{"10.0.0.4/32", "203.0.113.7/32"} {
		if at := slices.Index(calls, "iptables-nft -A CANYON-FLOOR -d "+a+" -j REJECT"); at < 0 || public < 0 || at > public {
			t.Errorf("the host's %s is rejected at call %d, public 80 returns at %d: want it rejected before\n%s", a, at, public, strings.Join(calls, "\n"))
		}
	}
}

// Fail closed: with no listing of the host's addresses, one that cannot be read, one written under another boot (a
// cloud host's public address may have moved since), one that names no boot, one cut short (no end line, or a last
// line without its newline, which a read would skip), or one that names no address but loopback, the floor stops
// before its first rule, naming the listing, and Envoy never starts above it.
func TestFloorFailsClosedWithoutTheHostsAddresses(t *testing.T) {
	boot := "boot_id " + bootID + "\n"
	loopback, garbled := boot+"1: lo: <LOOPBACK,UP>\n    inet 127.0.0.1/8 scope host lo\nend\n", boot+"2: eth0\n    inet 10.0.0.999/24 scope global eth0\nend\n"
	// the public address last, its newline lost: a read skips it, and the private one alone would pass
	unended := boot + "2: eth0: <UP>\n    inet 10.0.0.4/24 scope global eth0\n    inet 203.0.113.7/32 scope global eth0"
	endless := strings.TrimSuffix(hostListing, "end\n")
	truncated := hostListing[:strings.Index(hostListing, "    inet 203.0.113.7")]
	stale := strings.Replace(hostListing, bootID, "0d0d0d0d-0000-4000-8000-0d0d0d0d0d0d", 1)
	unbooted := strings.TrimPrefix(hostListing, boot)
	for name, listing := range map[string]*string{
		"none": nil, "loopback alone": &loopback, "no address": &garbled, "another boot": &stale, "no boot": &unbooted,
		"no newline": &unended, "no end line": &endless, "truncated": &truncated,
	} {
		calls, out, err := floorListing(t, "512", listing)
		if err == nil || calls[0] != "" || !strings.Contains(out, "CANYON_EGRESS_HOST_FILE") {
			t.Errorf("%s: floor.sh exited %v with calls %q; want it stopped before its first rule, naming the listing:\n%s", name, err, calls, out)
		}
	}
}

// A value iptables would not take as a positive count stops the holder before its first rule, naming it.
func TestFloorRefusesAPerSourceCapThatIsNoPositiveInteger(t *testing.T) {
	for _, v := range []string{"", "0", "-1", "012", "1.5", "5 ", " 5", "512a", "infinity"} {
		calls, out, err := floor(t, v)
		if err == nil || calls[0] != "" || !strings.Contains(out, "CANYON_EGRESS_PER_SOURCE") {
			t.Errorf("floor.sh with CANYON_EGRESS_PER_SOURCE=%q: %v, and it does not stop on it before its first rule (its first call: %q):\n%s", v, err, calls[0], out)
		}
	}
}

// The floor's holes for egress-control (design/EGRESS.md §3.1–3.2), each one address and one port, before the
// REJECTs of Canyon's pools and loopback that would refuse them: the server's policy on canyon-egress-ctl, which
// egress-control pulls — the receiver's address, port 7782, unset — and egress-control's loopback listener, which
// Envoy asks, 127.0.0.1:7783 unset, out of lo alone. No other port of either address is admitted.
func TestFloorAdmitsThePolicyAndTheCheckOnTheirPortsAlone(t *testing.T) {
	calls, out, err := floor(t, "512")
	if err != nil {
		t.Fatalf("floor.sh: %v\n%s", err, out)
	}
	firstReject := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "iptables-nft -A CANYON-FLOOR -d ") })
	for _, hole := range []string{
		"iptables-nft -A CANYON-FLOOR -p tcp -d 10.214.3.2/32 --dport 7782 -j RETURN",
		"iptables-nft -A CANYON-FLOOR -o lo -p tcp -d 127.0.0.1/32 --dport 7783 -j RETURN",
	} {
		if at := slices.Index(calls, hole); at < 0 || firstReject < 0 || at > firstReject {
			t.Errorf("%q is call %d, the first REJECT of an address %d: want the hole before it\n%s", hole, at, firstReject, strings.Join(calls, "\n"))
		}
	}
	for _, c := range calls {
		if strings.Contains(c, "RETURN") && (strings.Contains(c, "127.0.0.1") || strings.Contains(c, "10.214.3.2")) && !strings.Contains(c, "--dport") {
			t.Errorf("%q admits every port of an address Canyon holds", c)
		}
	}
}
