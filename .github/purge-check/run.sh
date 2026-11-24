#!/usr/bin/env bash
# run.sh overlay2 | containerd: on this image store, a person's busybox:1.37 at the demo's busybox digest and a stopped
# team Canyon's Postgres container are here first; then the published demo, up, down and down --purge, from the CLI this
# tree builds. Both must survive, purge must name them as kept, and nothing else the demo made may stay (#170).
set -euo pipefail
store=$1
on=false; [ "$store" = containerd ] && on=true
current=$(sudo cat /etc/docker/daemon.json 2>/dev/null || true); [ -n "$current" ] || current='{}'
jq --argjson on "$on" '.features["containerd-snapshotter"] = $on' <<<"$current" | sudo tee /etc/docker/daemon.json.new >/dev/null
sudo mv /etc/docker/daemon.json.new /etc/docker/daemon.json
sudo systemctl restart docker
for _ in $(seq 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info --format 'store: {{.Driver}} {{json .DriverStatus}}'
work=$(mktemp -d)
go build -o "$work/canyon" ./cmd/canyon
export HOME=$work/home; mkdir -p "$HOME"
export COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:$PWD/e2e/compose.telemetry-off.yml # no usage report
busybox=docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e # compose's pin
postgres=postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea           # compose's pin
docker pull -q "$busybox" >/dev/null && docker tag "$busybox" busybox:1.37 # the person's own tag, at the demo's digest
docker pull -q "$postgres" >/dev/null
docker run --name team-postgres --label com.docker.compose.project=canyon-team "$postgres" true >/dev/null # a stopped team Canyon's
docker image ls -a --digests --no-trunc --format '{{.Repository}}:{{.Tag}}@{{.Digest}} {{.ID}}' | sort >"$work/before"
"$work/canyon" demo up
"$work/canyon" demo down
"$work/canyon" demo down --purge | tee "$work/purge"
docker image ls -a --digests --no-trunc --format '{{.Repository}}:{{.Tag}}@{{.Digest}} {{.ID}}' | sort >"$work/after"
echo "== images now that weren't before"; comm -13 "$work/before" "$work/after" | tee "$work/added"
echo "== images that were here before and are gone"; comm -23 "$work/before" "$work/after" | tee "$work/taken"
docker system df
fail=
grep -q "The demo is gone" "$work/purge" || fail="$fail; purge didn't say the demo is gone"
grep -q "image $busybox (it is also tagged busybox:1.37)" "$work/purge" || fail="$fail; purge didn't name busybox as kept for its tag"
grep -q "image $postgres (the container team-postgres uses it)" "$work/purge" || fail="$fail; purge didn't name Postgres as kept for team-postgres"
docker image inspect busybox:1.37 >/dev/null || fail="$fail; the person's busybox:1.37 is gone"
docker container inspect team-postgres >/dev/null || fail="$fail; the team Canyon's container is gone"
docker image inspect "$postgres" >/dev/null || fail="$fail; the team Canyon's Postgres image is gone"
[ ! -s "$work/added" ] || fail="$fail; purge left images the demo made"
[ ! -s "$work/taken" ] || fail="$fail; purge took images that were here before"
[ -z "$fail" ] || { echo "::error::$store:${fail#;}"; exit 1; }
echo "$store: purge kept the person's tag and the team Canyon's image, named both, and left nothing else of the demo's"
