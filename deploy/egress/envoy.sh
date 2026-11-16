#!/bin/sh
# Canyon's Envoy, started in canyon-egress's network namespace: envoy.yaml with the
# server's receiver, the token Envoy sends it, the upstream resolvers and egress-control's loopback address
# (127.0.0.1:7783 unset) filled in from the environment, written where only Envoy reads it, then Envoy
# on it. Any argument is Envoy's: CI's --mode validate checks what runs. Envoy runs one ALS stream per worker,
# CANYON_EGRESS_CONCURRENCY of them, eight unset, which the receiver's sixteen leave room for: on Kubernetes two
# replicas run four each (deploy/k8s/egress.yaml).
# A value the server or Envoy would not take stops it, naming the variable, before anything is written.
set -eu

receiver=${CANYON_EGRESS_RECEIVER_ADDR:?the receiver of the server on canyon-egress-ctl, ip:port}
token=${CANYON_EGRESS_RECEIVER_TOKEN:?the token Envoy sends the receiver, the same as the server has}
resolvers=${CANYON_EGRESS_RESOLVERS:?the upstream resolvers of the machine, comma-separated IPv4 addresses}
concurrency=${CANYON_EGRESS_CONCURRENCY:-8}
control=${CANYON_EGRESS_CONTROL_ADDR:-127.0.0.1:7783}

refuse() {
	echo "envoy.sh: $1" >&2
	exit 1
}

# ipv4 holds when $1 is a dotted-quad IPv4 address: four decimal octets of 0 to 255, none with a leading zero.
ipv4() {
	case $1 in
	*[!0-9.]* | .* | *. | *..*) return 1 ;;
	esac
	set -f
	IFS=.
	set -- $1
	IFS=,
	[ $# -eq 4 ] || return 1
	for octet; do
		case $octet in
		[0-9] | [1-9][0-9] | 1[0-9][0-9] | 2[0-4][0-9] | 25[0-5]) ;;
		*) return 1 ;;
		esac
	done
}

# The server's own rule for its token: 32 to 512 of these characters.
case $token in
*[!A-Za-z0-9._~+/=-]*) refuse "CANYON_EGRESS_RECEIVER_TOKEN holds a character the server's token never does" ;;
esac
[ ${#token} -ge 32 ] && [ ${#token} -le 512 ] || refuse "CANYON_EGRESS_RECEIVER_TOKEN is not 32 to 512 characters"
case $concurrency in
'' | *[!0-9]* | 0*) refuse "CANYON_EGRESS_CONCURRENCY is not a positive integer" ;;
esac
[ "$concurrency" -le 16 ] || refuse "CANYON_EGRESS_CONCURRENCY is over 16, the receiver's streams"
port=${receiver##*:}
case $port in
'' | *[!0-9]* | 0*) refuse "CANYON_EGRESS_RECEIVER_ADDR is not ip:port" ;;
esac
ipv4 "${receiver%:*}" && [ "$port" -le 65535 ] || refuse "CANYON_EGRESS_RECEIVER_ADDR is not an IPv4 address and a port"
control_port=${control##*:}
case $control_port in
'' | *[!0-9]* | 0*) refuse "CANYON_EGRESS_CONTROL_ADDR is not ip:port" ;;
esac
case ${control%:*} in
127.*) ;;
*) refuse "CANYON_EGRESS_CONTROL_ADDR is not a loopback address: egress-control answers Envoy beside it alone" ;;
esac
ipv4 "${control%:*}" && [ "$control_port" -le 65535 ] ||
	refuse "CANYON_EGRESS_CONTROL_ADDR is not an IPv4 address and a port"

list=
IFS=,
for resolver in $resolvers; do
	ipv4 "$resolver" || refuse "CANYON_EGRESS_RESOLVERS holds $resolver, no IPv4 address"
	list="${list:+$list, }{socket_address: {address: \"$resolver\", port_value: 53}}"
done
[ -n "$list" ] || refuse "CANYON_EGRESS_RESOLVERS names no resolver"
umask 077
sed -e "s|@receiver_address@|${receiver%:*}|" -e "s|\"@receiver_port@\"|$port|" \
	-e "s|@control_address@|${control%:*}|" -e "s|\"@control_port@\"|$control_port|" \
	-e "s|@receiver_token@|$token|" -e "s|\"@resolvers@\"|[$list]|" \
	/etc/canyon/envoy.yaml >/var/run/envoy/envoy.yaml
exec envoy -c /var/run/envoy/envoy.yaml --concurrency "$concurrency" "$@"
