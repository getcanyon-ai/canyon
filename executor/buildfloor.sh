set -eu

# A build's floor (contracts/executor.md, A build reaches no private or link-local address): the entrypoint
# of each build's holder, applied in the holder's own network namespace, which pack's every phase joins.
# The jump into the chain is the last rule, so the executor's check of it finds the whole floor or none.
subnet=${CANYON_BUILD_SUBNET:?the subnet of canyon-builds}
host=${CANYON_BUILD_HOST:?every address of the Docker host, each a /32}
pools=${CANYON_BUILD_POOLS:?the apps pool}
allow=${CANYON_BUILD_ALLOW-}

iptables-nft -N CANYON-BUILD
iptables-nft -A CANYON-BUILD -o lo -j RETURN # Docker's resolver, 127.0.0.11, and the build's own loopback
IFS=,
for range in $host 169.254.0.0/16 168.63.129.16/32 100.100.100.200/32 0.0.0.0/8 127.0.0.0/8 224.0.0.0/4 240.0.0.0/4 $subnet $pools; do # before what it answers, the allowed and public 80 and 443: no connection or entry opens these, the host's own public address among them
	iptables-nft -A CANYON-BUILD -d "$range" -j REJECT
done
iptables-nft -A CANYON-BUILD -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
for entry in $allow; do # CANYON_BUILD_ALLOW, each <prefix>:<port>, which the executor checked at its start
	iptables-nft -A CANYON-BUILD -p tcp -d "${entry%:*}" --dport "${entry##*:}" -j RETURN
done
for range in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 192.0.0.0/24 198.18.0.0/15; do
	iptables-nft -A CANYON-BUILD -d "$range" -j REJECT
done
iptables-nft -A CANYON-BUILD -p tcp --dport 80 -j RETURN
iptables-nft -A CANYON-BUILD -p tcp --dport 443 -j RETURN
iptables-nft -A CANYON-BUILD -j REJECT
iptables-nft -P FORWARD DROP
if [ -e /proc/net/if_inet6 ]; then # canyon-builds has no IPv6, but a namespace's link-local is still there
	ip6tables-nft -N CANYON-BUILD
	ip6tables-nft -A CANYON-BUILD -o lo -j RETURN
	ip6tables-nft -A CANYON-BUILD -j DROP
	ip6tables-nft -I OUTPUT 1 -j CANYON-BUILD
fi
iptables-nft -I OUTPUT 1 -j CANYON-BUILD

exec sleep infinity
