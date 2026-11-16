#!/usr/bin/env bash
# The latest released CLI against this tree's server (.github/workflows/old-cli.yml): people keep the CLI
# they installed, so a server change it cannot read breaks them on the next deploy. #2018 would have broken
# every v0.1.11 deploy ("the host's verdict and its findings disagree"), and only a manual run caught it.
# On the compose stack from this tree: the release's CLI, checked against its SHA256SUMS, checks
# claims-portal's next version (blocked, exit 2) and claims-archive (clean, exit 0), deploys claims-archive (exit 0, the preview answers) and
# promotes it (exit 0, prod answers). Any other exit, or a line that says the two disagree, fails it.
# Needs gh signed in (GH_TOKEN in CI; any token reads the public releases); tears the stack down with make demo-down, pass or fail.
set -euo pipefail

repo=getcanyon-ai/canyon-releases # where every release is published, and people install from: the source repository has none
host=http://127.0.0.1:7777
work=$(mktemp -d)

finish() {
  rc=$?
  [ $rc -eq 0 ] || docker compose logs --no-color --tail=100 server executor || true
  make --no-print-directory demo-down >/dev/null || true # a teardown that fails never replaces the run's own exit
  rm -rf "$work"
  exit $rc
}
trap finish EXIT

tag=$(gh release view -R "$repo" --json tagName --jq .tagName) # the latest release; a pre-release is never latest
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in x86_64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) echo "no release CLI for $(uname -m)"; exit 1 ;; esac
tarball=canyon_${tag}_${os}_${arch}.tar.gz
gh release download "$tag" -R "$repo" -p "$tarball" -p SHA256SUMS -D "$work"
(cd "$work" && grep " $tarball\$" SHA256SUMS | shasum -a 256 -c -) # no line for it is no properly formatted line: exit 1
tar -xzf "$work/$tarball" -C "$work"
canyon=$work/${tarball%.tar.gz}/canyon

docker compose -f docker-compose.yml -f e2e/compose.telemetry-off.yml up -d --build postgres server executor
for i in $(seq 1 45); do curl -sf "$host/ready" >/dev/null && break; [ "$i" = 45 ] && { echo "server never became ready"; exit 1; }; sleep 2; done
scripts/telemetry-off-check.sh docker

# run <want> <app dir> <args...>: the release's CLI in demo/apps/<app dir>, signed in under its own HOME
run() {
  local want=$1 dir=$2 code=0
  shift 2
  (cd "demo/apps/$dir" && HOME=$work "$canyon" "$@" --host "$host") >"$work/out" 2>&1 || code=$?
  cat "$work/out"
  [ "$code" = "$want" ] || { echo "$tag's canyon $* in $dir exited $code, want $want"; exit 1; }
  if grep -Eiq 'disagree|unknown field|not Canyon' "$work/out"; then
    echo "$tag's CLI cannot read this server: $(grep -Ei 'disagree|unknown field|not Canyon' "$work/out" | head -1)"
    exit 1
  fi
}
# answers <url>: the app's own page behind Canyon's proxy, addressed by its Host as a browser would, within
# 10 s of the deploy's end; an unknown host gets the dashboard, so a 200 alone proves nothing
answers() {
  local page
  for _ in $(seq 1 10); do
    page=$(curl -sf -m 5 -H "Host: ${1#http://}" "$host/") && [[ $page == *'<h1>claims-archive</h1>'* ]] && return
    sleep 1
  done
  echo "$1 does not serve claims-archive"
  exit 1
}

HOME=$work "$canyon" version
HOME=$work "$canyon" login "$host"
run 2 claims-portal-next check
run 0 claims-archive check
run 0 claims-archive deploy --force # --force: it replaces the demo seed's record of the app
answers http://claims-archive.preview.localhost:7777
run 0 claims-archive deploy --env prod --yes
answers http://claims-archive.prod.localhost:7777
echo "$tag's CLI checks, deploys and promotes against this server"
