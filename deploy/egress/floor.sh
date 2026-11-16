#!/bin/sh
# The floor, applied by canyon-egress in its own network namespace, which Envoy
# and egress-control join without NET_ADMIN: a new connection out of the namespace to Canyon's pools,
# loopback, a private (RFC 1918), shared (100.64/10), link-local or reserved address or Azure's WireServer is
# refused, but to the server's receiver and its policy, each its one port, to egress-control on its
# one loopback port, which Envoy asks, and to the configured upstream resolvers on 53; of the public
# internet, TCP 80 and 443 alone. A reply to a connection that came in — an app's to Envoy — is no new connection and
# passes. Nothing is forwarded, and IPv6 sends nothing. Inbound, one source address holds at most
# CANYON_EGRESS_PER_SOURCE connections to Envoy's listener: the next is reset. Every rule that fails
# stops the holder before it sleeps, and the jump into the chain is the last rule, which its healthcheck
# asks for: nothing runs in this namespace before the whole floor is in place. iptables-nft, never the
# image's wrapper, which rewrites its own links and cannot on a read-only root.
set -eu

receiver=${CANYON_EGRESS_RECEIVER_ADDR:?the receiver of the server on canyon-egress-ctl, ip:port}
# The server's policy, which egress-control pulls: the receiver's address, port 7782, unset;
# and egress-control's own loopback address, which Envoy asks: 127.0.0.1:7783 unset, as envoy.sh's.
policy=${CANYON_EGRESS_POLICY_ADDR:-${receiver%:*}:7782}
control=${CANYON_EGRESS_CONTROL_ADDR:-127.0.0.1:7783}
resolvers=${CANYON_EGRESS_RESOLVERS:?the upstream resolvers of the machine, comma-separated IPv4 addresses}
pools=${CANYON_EGRESS_POOLS:?the pools of Canyon, comma-separated CIDRs: the app networks and its own}
per_source=${CANYON_EGRESS_PER_SOURCE:?the connections one source address may hold to Envoy, a positive integer}
case $per_source in
*[!0-9]* | 0*)
	echo "floor.sh: CANYON_EGRESS_PER_SOURCE is not a positive integer" >&2
	exit 1
	;;
esac

# The Docker host's own addresses (#2524), a public one among them, which 80 and 443 would let through: what
# egress-host listed with `ip addr show` in the host's network namespace, each "inet <address>/<bits>" line one,
# under a first line "boot_id <id>", the kernel's boot it was written in, and over a last line "end". A listing
# missing or unreadable, written under another boot than this one (a cloud host's public address may have moved
# since) or naming none, cut short (its last line not "end"; a last line without its newline is read, never
# skipped), an address in it that is no IPv4 one, or none but loopback stops the floor before its first rule, so
# Envoy never starts above a floor that does not know the host. The listing is read here alone, at the holder's start: an address the host
# takes later is out of the floor until egress starts again.
host_file=${CANYON_EGRESS_HOST_FILE:?the listing of the addresses of the Docker host, which egress-host writes}
boot_file=${CANYON_EGRESS_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}
refuse_host() {
	echo "floor.sh: CANYON_EGRESS_HOST_FILE $host_file: $1" >&2
	exit 1
}
octets() {
	case $1 in
	*[!0-9.]* | .* | *. | *..*) return 1 ;;
	esac
	old_ifs=$IFS
	IFS=.
	set -- $1
	IFS=$old_ifs
	[ $# -eq 4 ] || return 1
	for octet; do
		case $octet in
		[0-9] | [1-9][0-9] | 1[0-9][0-9] | 2[0-4][0-9] | 25[0-5]) ;;
		*) return 1 ;;
		esac
	done
}
[ -r "$host_file" ] || refuse_host "no listing can be read"
boot=
read -r boot <"$boot_file" || [ -n "$boot" ] || refuse_host "this boot's id cannot be read from $boot_file"
kind= listed= rest=
{ read -r kind listed rest || :; } <"$host_file"
[ "$kind" = boot_id ] && [ -n "$boot" ] && [ "$listed" = "$boot" ] && [ -z "$rest" ] ||
	refuse_host "written under boot ${listed:-none}, not this boot $boot"
host=
routable=
ended=
while read -r kind cidr rest || [ -n "$kind" ]; do
	ended=
	[ "$kind" = end ] && [ -z "$cidr$rest" ] && ended=yes
	[ "$kind" = inet ] || continue
	address=${cidr%/*}
	octets "$address" || refuse_host "$cidr is no IPv4 address"
	host="$host${host:+,}$address/32"
	case $address in
	127.*) ;;
	*) routable=yes ;;
	esac
done <"$host_file"
[ -n "$ended" ] || refuse_host "cut short: its last line is not end"
[ -n "$routable" ] || refuse_host "no address of the host's but loopback"

iptables-nft -N CANYON-FLOOR
iptables-nft -A CANYON-FLOOR -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables-nft -A CANYON-FLOOR -p tcp -d "${receiver%:*}/32" --dport "${receiver##*:}" -j RETURN
iptables-nft -A CANYON-FLOOR -p tcp -d "${policy%:*}/32" --dport "${policy##*:}" -j RETURN
iptables-nft -A CANYON-FLOOR -o lo -p tcp -d "${control%:*}/32" --dport "${control##*:}" -j RETURN
IFS=,
for range in $pools 127.0.0.0/8; do # before the resolvers: a resolver inside Canyon, as Docker's 127.0.0.11 is, is never admitted
	iptables-nft -A CANYON-FLOOR -d "$range" -j REJECT
done
for resolver in $resolvers; do
	iptables-nft -A CANYON-FLOOR -p udp -d "$resolver/32" --dport 53 -j RETURN
	iptables-nft -A CANYON-FLOOR -p tcp -d "$resolver/32" --dport 53 -j RETURN
done
# After the resolvers: Azure's VMs resolve through the WireServer, which is refused on every other port.
for range in 0.0.0.0/8 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 168.63.129.16/32 192.0.0.0/24 198.18.0.0/15 240.0.0.0/4; do # 0.0.0.0/8: OrbStack's host.docker.internal is there
	iptables-nft -A CANYON-FLOOR -d "$range" -j REJECT
done
for address in $host; do # the host's own, before public 80 and 443 return
	iptables-nft -A CANYON-FLOOR -d "$address" -j REJECT
done
iptables-nft -A CANYON-FLOOR -p tcp --dport 80 -j RETURN
iptables-nft -A CANYON-FLOOR -p tcp --dport 443 -j RETURN
iptables-nft -A CANYON-FLOOR -j REJECT
iptables-nft -A INPUT -p tcp --syn --dport 3128 -m connlimit --connlimit-above "$per_source" --connlimit-mask 32 -j REJECT --reject-with tcp-reset
iptables-nft -P FORWARD DROP
if [ -e /proc/net/if_inet6 ]; then # compose's sysctls turn IPv6 off; a policy holds even if one were missed
	ip6tables-nft -P OUTPUT DROP
fi
iptables-nft -I OUTPUT 1 -j CANYON-FLOOR

exec sleep infinity
