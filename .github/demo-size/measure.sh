#!/usr/bin/env bash
# measure.sh <out>: the published v0.11.1 demo, cold.
set -uo pipefail
export OUT=$1; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); . "$here/lib.sh"
work=$(mktemp -d); export HOME=$work/home; mkdir -p "$HOME" "$work/bin"
base=https://github.com/getcanyon-ai/canyon-releases/releases/download/v0.11.1
cd "$work"
curl -fsSLO "$base/canyon_v0.11.1_linux_arm64.tar.gz"; curl -fsSLO "$base/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS || exit 1
tar -xzf canyon_v0.11.1_linux_arm64.tar.gz -C "$work/bin"
canyon=$(find "$work/bin" -type f -name canyon | head -1); "$canyon" --version || "$canyon" version || true
# no usage report: the published compose files, with Canyon's own telemetry-off file over them
export COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:$here/../../e2e/compose.telemetry-off.yml
snap 00-before
sleep 60; snap 01-idle-60s # what the runner receives by itself
docker events --format '{{json .}}' >"$OUT/events.json" 2>/dev/null & events=$!
( while :; do printf '%s\t%s\t%s\n' "$(date +%s)" "$(rx)" "$(used)" >>"$OUT/samples.tsv"; sleep 5; done ) & sampler=$!
i=10
"$canyon" demo up 2>&1 | while IFS= read -r line; do
  printf '%s\t%s\t%s\t%s\n' "$(date +%s)" "$(rx)" "$(used)" "$line" | tee -a "$OUT/up.tsv"
  case $line in
    *"Checking this machine"*|*"Fetching the demo"*|*"Downloading Canyon"*|*"Starting Canyon"*|*" is running ("*|*"Canyon is ready"*)
      i=$((i+1)); name=$(printf '%s' "$line" | tr -cs 'a-zA-Z0-9' '-' | cut -c1-40); snap "$i-$name" & ;;
  esac
done
echo "demo up exited ${PIPESTATUS[0]}" | tee "$OUT/up.exit"
sleep 20; snap 30-up
"$here/../../scripts/telemetry-off-check.sh" docker || { echo "::error::a server here would send a usage report"; "$canyon" demo down --purge; exit 1; }
for c in $(docker ps -a --format '{{.Names}}' | grep -E 'executor|demo-apps|canyon-server'); do docker logs "$c" >"$OUT/log-$c.txt" 2>&1; done
docker ps -a --size >"$OUT/ps.txt"
sleep 900; snap 31-up-15min-later
grep -h -o '"canyon-scandb-[a-z-]*"' "$OUT/events.json" | sort | uniq -c | tee "$OUT/scandb-events.txt"
"$canyon" demo down 2>&1 | tee "$OUT/down.txt"; snap 40-down
docker network ls >"$OUT/networks-40-down.txt"
"$canyon" demo down --purge 2>&1 | tee "$OUT/purge.txt"; snap 50-purge
docker network ls >"$OUT/networks-50-purge.txt"; docker ps -a >"$OUT/ps-50-purge.txt"
kill "$events" "$sampler" 2>/dev/null
cat "$OUT/stages.tsv"; cat "$OUT/df-30-up.txt"; cat "$OUT/images-50-purge.txt" "$OUT/volumes-50-purge.txt" "$OUT/buildcache-50-purge.txt"
exit 0
