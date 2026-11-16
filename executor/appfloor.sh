set -eu

gateway=${CANYON_APP_GATEWAY:?the gateway of the app network, the address of the host on it}
subnet=${CANYON_APP_SUBNET:?the subnet of the app network}
host=${CANYON_APP_HOST:?every address of the Docker host, each a /32}
pools=${CANYON_APP_POOLS:?the apps pool and the other networks of Canyon}
allow=${CANYON_APPS_ALLOW-}

iptables-nft -N CANYON-APP
iptables-nft -A CANYON-APP -o lo -j RETURN # Docker's resolver, 127.0.0.11, and the app's own loopback
IFS=,
for range in "$gateway/32" $host 169.254.0.0/16 168.63.129.16/32 100.100.100.200/32 0.0.0.0/8 127.0.0.0/8 224.0.0.0/4 240.0.0.0/4; do # the host, before what it answers and its own network return: no connection or entry opens these
	iptables-nft -A CANYON-APP -d "$range" -j REJECT
done
iptables-nft -A CANYON-APP -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables-nft -A CANYON-APP -d "$subnet" -j RETURN # its own network: the server and its database
for range in $pools; do # every other network of Canyon's
	iptables-nft -A CANYON-APP -d "$range" -j REJECT
done
for entry in $allow; do # CANYON_APPS_ALLOW, each <prefix>:<port>, which the executor checked at its start
	iptables-nft -A CANYON-APP -p tcp -d "${entry%:*}" --dport "${entry##*:}" -j RETURN
done
for range in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 192.0.0.0/24 198.18.0.0/15; do
	iptables-nft -A CANYON-APP -d "$range" -j REJECT
done
iptables-nft -P FORWARD DROP
if [ -e /proc/net/if_inet6 ]; then # app networks have no IPv6, but a namespace's link-local is still there
	ip6tables-nft -N CANYON-APP
	ip6tables-nft -A CANYON-APP -o lo -j RETURN
	ip6tables-nft -A CANYON-APP -j DROP
	ip6tables-nft -I OUTPUT 1 -j CANYON-APP
fi
iptables-nft -I OUTPUT 1 -j CANYON-APP

exec sleep infinity
