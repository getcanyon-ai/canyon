#!/bin/sh
# Installs the canyon CLI from one GitHub release (design/specs/cli.md §8.1):
#   curl -fsSL https://github.com/<owner>/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh
# make cli-release stamps version below; CANYON_RELEASE_URL points it at another copy of the
# release's files (make install-test), CANYON_INSTALL_DIR at another directory.
set -eu

version=dev
page=https://github.com/getcanyon-ai/canyon-releases/releases # the public download repository; its owner is cli/help.go's too
release="${CANYON_RELEASE_URL:-$page/download/$version}"

fail() { echo "canyon install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "no build for $(uname -s) here; download yours from $page" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "no build for $(uname -m) here; download yours from $page" ;;
esac
name="canyon_${version}_${os}_${arch}"

command -v curl >/dev/null 2>&1 || fail "needs curl"
fetch() { curl -fsSL -o "$2" "$1"; }
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM # dash runs the EXIT trap on exit alone, not on a signal
fetch "$release/$name.tar.gz" "$tmp/$name.tar.gz" || fail "could not download $release/$name.tar.gz"
fetch "$release/SHA256SUMS" "$tmp/SHA256SUMS" || fail "could not download $release/SHA256SUMS"
want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "SHA256SUMS names no $name.tar.gz"
got=$(sha256 "$tmp/$name.tar.gz")
[ "$got" = "$want" ] || fail "$name.tar.gz has SHA-256 $got, but SHA256SUMS says $want; nothing was installed"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

dir=${CANYON_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
new=$(mktemp "$dir/.canyon.XXXXXX") # a new file of its own, renamed over canyon: never written through a link
cp "$tmp/$name/canyon" "$new"
chmod 755 "$new"
mv -f "$new" "$dir/canyon"
echo "canyon $version installed to $dir/canyon"

case ":$PATH:" in
  *":$dir:"*)
    first=$(command -v canyon || true)
    [ "$first" = "$dir/canyon" ] || echo "$first comes first on your PATH, so canyon still runs it: remove it, or put $dir ahead of it" ;;
  *) echo "$dir is not on your PATH. Add it: export PATH=\"$dir:\$PATH\" (in your shell's rc file to keep it)" ;;
esac
"$dir/canyon" --version
