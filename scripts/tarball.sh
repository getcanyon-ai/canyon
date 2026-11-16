#!/bin/sh
# scripts/tarball.sh <out.tar.gz> <dir> <entry> [tar option...]: <dir>/<entry> as a gzipped tarball any tar
# unpacks without a word, made the same on macOS's bsdtar and Linux's GNU tar: every owner uid and gid 0 with
# no names, no extended attributes, and none of macOS's AppleDouble ._ files. scripts/deploy-bundle.sh and
# scripts/opengrep-source.sh pack the release's archives with it; scripts/release-archives-check.sh refuses
# an archive that is not so (#2409).
set -eu
out=${1:?usage: scripts/tarball.sh <out.tar.gz> <dir> <entry> [tar option...]}
dir=${2:?usage: scripts/tarball.sh <out.tar.gz> <dir> <entry> [tar option...]}
entry=${3:?usage: scripts/tarball.sh <out.tar.gz> <dir> <entry> [tar option...]}
shift 3
COPYFILE_DISABLE=1 # macOS's copyfile writes no ._ file beside each one
export COPYFILE_DISABLE
case $(tar --version 2>&1) in
  *bsdtar*) exec tar -czf "$out" --no-mac-metadata --no-xattrs --uid 0 --gid 0 --uname '' --gname '' "$@" -C "$dir" "$entry" ;;
  *'GNU tar'*) exec tar -czf "$out" --owner=0 --group=0 --numeric-owner --no-xattrs "$@" -C "$dir" "$entry" ;;
  *) echo "tarball: tar is neither bsdtar nor GNU tar: $(tar --version 2>&1 | head -1)" >&2; exit 1 ;;
esac
