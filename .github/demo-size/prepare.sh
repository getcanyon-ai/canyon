#!/usr/bin/env bash
# prepare.sh overlay2 | containerd: the runner's disk freed, and Docker on that image store, holding nothing.
set -euo pipefail
df -h /
sudo rm -rf /usr/share/dotnet
sudo rm -rf /usr/local/lib/android
sudo rm -rf /opt/ghc
sudo rm -rf /opt/hostedtoolcache/CodeQL
docker image prune -af >/dev/null
on=false; [ "$1" = containerd ] && on=true
conf=/etc/docker/daemon.json
current=$(sudo cat "$conf" 2>/dev/null || true); [ -n "$current" ] || current='{}'
jq --argjson on "$on" '.features["containerd-snapshotter"] = $on' <<<"$current" | sudo tee "$conf.new" >/dev/null
sudo mv "$conf.new" "$conf"
sudo systemctl restart docker
for _ in $(seq 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info --format '{{.ServerVersion}} {{.Driver}} {{.DockerRootDir}} {{.NCPU}} cpus {{.MemTotal}} bytes'
docker info --format '{{json .DriverStatus}}'
docker image ls -a; docker volume ls; docker system df
df -h / ; df -h /var/lib/docker /var/lib/containerd || true
