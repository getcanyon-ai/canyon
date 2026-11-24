# Sourced: what the runner received and what its disk holds, and a snapshot of what Docker holds.
iface=$(ip route show default | awk '{print $5; exit}')
rx() { cat "/sys/class/net/$iface/statistics/rx_bytes"; }
used() { df -B1 --output=used / | tail -1 | tr -d ' '; }
snap() { # snap <name>: now, and what Docker holds
  local n=$1
  printf '%s\t%s\t%s\t%s\n' "$n" "$(date +%s)" "$(rx)" "$(used)" >>"$OUT/stages.tsv"
  { docker system df; echo; docker system df -v; } >"$OUT/df-$n.txt" 2>&1 || true
  docker image ls -a --digests >"$OUT/images-$n.txt" 2>&1 || true
  docker volume ls >"$OUT/volumes-$n.txt" 2>&1 || true
  docker builder du >"$OUT/buildcache-$n.txt" 2>&1 || true
}
