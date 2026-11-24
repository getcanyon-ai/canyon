#!/usr/bin/env bash
# probe.sh <out>: pack's ephemeral builder on this image store. pack makes one for every build given --env or --run-image
# (pkg/client/build.go, ephemeralBuilderNeeded), as Canyon's always are. The same app, the same builder, with and
# without both, each twice, timed, and the disk after each.
set -uo pipefail
OUT=$1; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../.." && pwd); cd "$repo"
used() { df -B1 --output=used / | tail -1 | tr -d ' '; }
tag=canyon-builder:probe
docker build -q -t "$tag" - <executor/builder.Dockerfile >/dev/null
docker build -q -t canyon-run:noble - <executor/run.Dockerfile >/dev/null
docker pull -q paketobuildpacks/ubuntu-noble-run:latest >/dev/null # the run image the builder itself names
life=$(grep -o 'buildpacksio/lifecycle@sha256:[0-9a-f]*' internal/names/images.go executor/executor.go | head -1 | cut -d: -f2-)
[ -n "$life" ] || life="buildpacksio/lifecycle@$(grep -o 'LifecycleRepository + "@sha256:[0-9a-f]*' executor/executor.go | cut -d'"' -f2 | cut -c2-)"
echo "lifecycle $life"
app=$RUNNER_TEMP/app; mkdir -p "$app"; cp demo/apps/claims-portal/* "$app"/
run() {
  local name=$1; shift
  local u0 t0; u0=$(used); t0=$(date +%s.%N)
  pack build "probe/$name:$RANDOM" --no-color --path "$app" --builder "$tag" --pull-policy if-not-present --lifecycle-image "$life" \
    --cache "type=build;format=volume;name=probe-$name-build" --cache "type=launch;format=volume;name=probe-$name-launch" "$@" >"$OUT/log-$name.txt" 2>&1
  local rc=$?
  printf 'RUN %-34s exit %s  %6.1f s  disk %+6.2f GB  ephemeral builder: %s\n' "$name" "$rc" "$(echo "$(date +%s.%N) - $t0" | bc)" "$(echo "($(used) - $u0)/1000000000" | bc -l)" \
    "$(grep -c 'Builder creation completed' "$OUT/log-$name.txt")" | tee -a "$OUT/runs.txt"
}
run as-canyon-1 --run-image canyon-run:noble --env NODE_ENV=production --env NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/
run as-canyon-2 --run-image canyon-run:noble --env NODE_ENV=production --env NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/
run no-flags-1
run no-flags-2
run env-only-1 --env NODE_ENV=production
run run-image-only-1 --run-image canyon-run:noble
docker system df
exit 0
