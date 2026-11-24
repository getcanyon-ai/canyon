#!/usr/bin/env bash
# tour.sh <out>: make tour-check, the tour on the demo built from this tree, with every mount of a volume recorded: a
# build reads a cache by mounting it. Each line of the tour's own log is timestamped, so a mount can be placed in it.
set -uo pipefail
OUT=$1; mkdir -p "$OUT"
docker events --filter type=volume --format '{{.TimeNano}} {{.Action}} {{.Actor.ID}} {{index .Actor.Attributes "container"}}' >"$OUT/volume-events.txt" &
ev=$!
( while :; do docker system df -v 2>/dev/null | sed -n '/Local Volumes space usage/,/Build cache usage/p' >"$OUT/volumes-latest.txt"; sleep 20; done ) &
vs=$!
make tour-check 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%s%N)" "$l"; done | tee "$OUT/tour.txt"
kill "$ev" "$vs"
echo "== cache volume mounts, by volume"
awk '$2 == "mount" && $3 ~ /^canyon-cache-/ {n[$3]++} END {for (v in n) print n[v], v}' "$OUT/volume-events.txt" | sort -k2
exit 0
