#!/usr/bin/env bash
# The published demo, up, down, then down --purge, from the CLI this tree builds: what Docker holds after is what it
# held before, and purge said the demo is gone.
set -euo pipefail
work=$(mktemp -d)
go build -o "$work/canyon" ./cmd/canyon
export HOME=$work/home; mkdir -p "$HOME"
export COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:$PWD/e2e/compose.telemetry-off.yml # no usage report
docker image ls -a --no-trunc --format '{{.ID}} {{.Repository}}:{{.Tag}}' | sort >"$work/before"
docker volume ls -q | sort >"$work/vol-before"
"$work/canyon" demo up
"$work/canyon" demo down
"$work/canyon" demo down --purge | tee "$work/purge"
docker image ls -a --no-trunc --format '{{.ID}} {{.Repository}}:{{.Tag}}' | sort >"$work/after"
docker volume ls -q | sort >"$work/vol-after"
docker system df
echo "== images now that weren't before"; comm -13 "$work/before" "$work/after" | tee "$work/added"
echo "== volumes now that weren't before"; comm -13 "$work/vol-before" "$work/vol-after" | tee -a "$work/added"
grep -q "The demo is gone" "$work/purge"
[ ! -s "$work/added" ] || { echo "::error::down --purge left what the demo made"; exit 1; }
echo "down --purge after a down left nothing the demo made"
