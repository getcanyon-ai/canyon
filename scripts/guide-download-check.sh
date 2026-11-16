#!/bin/sh
# scripts/guide-download-check.sh <docker.md> <canyon-deploy.tar.gz>: the guide's "Download Canyon" block (docs:
# self-host/docker.md), run as a reader's shell runs it, with no -e, against a release whose files are these, its curl
# answered from them: a tarball that matches SHA256SUMS is unpacked, and a tampered one is not unpacked at all.
# scripts/deploy-bundle-check.sh runs it on the bundle it builds; any tarball holding canyon/ will do.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
usage='usage: scripts/guide-download-check.sh <docker.md> <canyon-deploy.tar.gz>'
guide=${1:?$usage}
bundle=${2:?$usage}
case $bundle in /*) ;; *) bundle=$PWD/$bundle ;; esac
fail() { echo "guide-download-check: $*" >&2; exit 1; }
block=$("$here/guide-block.sh" "$guide" 'Download Canyon')

# unpacked <intact|tampered>: the block run in an empty directory, the release serving this tarball, changed by one
# byte after its checksum was taken when tampered; true when the block unpacked canyon/.
unpacked() {
  d=$(mktemp -d)
  mkdir "$d/bin" "$d/release" "$d/run"
  cp "$bundle" "$d/release/canyon-deploy.tar.gz"
  (cd "$d/release" && sha256sum canyon-deploy.tar.gz > SHA256SUMS &&
    echo "0000000000000000000000000000000000000000000000000000000000000000  canyon_v0.3.0_linux_amd64.tar.gz" >> SHA256SUMS)
  [ "$1" = intact ] || printf x >> "$d/release/canyon-deploy.tar.gz"
  printf '#!/bin/sh\nfor a; do case $a in https://*) cp "%s/release/${a##*/}" . ;; esac; done\n' "$d" > "$d/bin/curl"
  chmod +x "$d/bin/curl"
  (cd "$d/run" && PATH="$d/bin:$PATH" RELEASE=v0.3.0 sh -u -c "$block") >"$d/out" 2>&1 || true
  sed 's/^/  /' "$d/out"
  found=1
  [ -d "$d/run/canyon" ] || found=0
  rm -rf "$d"
  [ "$found" = 1 ]
}
unpacked intact || fail "the guide's download block did not unpack a release that matches its checksums"
echo "the guide's download block unpacks a release that matches its checksums"
if unpacked tampered; then fail "the guide's download block unpacked a canyon-deploy.tar.gz that does not match SHA256SUMS"; fi
echo "the guide's download block unpacks nothing when canyon-deploy.tar.gz does not match SHA256SUMS"
