#!/usr/bin/env bash
# tour.sh <out>: make tour-check, the demo built from this tree with its tour, on this runner's image store: every mount of
# a cache volume (a build reads its caches by mounting them), and what the runner received and its disk held, every 5 s.
set -uo pipefail
export OUT=$1; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); . "$here/lib.sh"
docker events --filter type=volume --format '{{.TimeNano}} {{.Action}} {{.Actor.ID}}' >"$OUT/volume-events.txt" &
ev=$!
( while :; do printf '%s %s %s\n' "$(date +%s)" "$(rx)" "$(used)" >>"$OUT/samples.txt"; sleep 5; done ) &
sm=$!
( while :; do docker system df -v >"$OUT/df-latest.txt" 2>/dev/null; sleep 20; done ) &
dfl=$!
snap 00-before
make tour-check 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%s)" "$l"; done | tee "$OUT/tour.txt"
snap 99-after-purge
kill "$ev" "$sm" "$dfl"
exit 0
