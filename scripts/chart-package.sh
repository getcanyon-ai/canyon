#!/bin/sh
# scripts/chart-package.sh <git-ref> <tag> <out.tgz>: canyon-chart.tgz, the Helm chart deploy/helm/canyon as
# <git-ref> has it, with LICENSE, NOTICE and THIRD_PARTY_NOTICES. Its version is <tag> less its v (v0.2.3 is
# chart 0.2.3, v0.2.0-rc.1 chart 0.2.0-rc.1), so each release's chart is its own (#2436), and its appVersion is
# <tag>. A tag that leaves no SemVer 2 version without build metadata is refused: helm itself takes 1.2 and
# 01.2.3, and a + is not allowed in the helm.sh/chart label the version is written into.
# scripts/publish-release.sh packs a release's from its tag; CI's helm workflow packs one from HEAD and checks it.
set -eu
ref=${1:?usage: scripts/chart-package.sh <git-ref> <tag> <out.tgz>}
tag=${2:?usage: scripts/chart-package.sh <git-ref> <tag> <out.tgz>}
out=${3:?usage: scripts/chart-package.sh <git-ref> <tag> <out.tgz>}
case $out in /*) ;; *) out=$PWD/$out ;; esac
fail() { echo "chart-package: $*" >&2; exit 1; }
nl='
'
case $tag in *"$nl"*) fail "the tag holds a newline: it is not a release tag" ;; esac # grep -x matches line by line
n='(0|[1-9][0-9]*)' pre='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)' # semver.org's grammar, less build metadata
printf '%s\n' "$tag" | grep -Eqx "v$n\.$n\.$n(-$pre(\.$pre)*)?" ||
  fail "$tag is not a release tag: v and a SemVer 2 version"
version=${tag#v}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git archive "$ref" LICENSE NOTICE THIRD_PARTY_NOTICES deploy/helm/canyon | tar -x -C "$work"
cp "$work/LICENSE" "$work/NOTICE" "$work/THIRD_PARTY_NOTICES" "$work/deploy/helm/canyon/"
mkdir "$work/out"
helm package "$work/deploy/helm/canyon" --version "$version" --app-version "$tag" -d "$work/out" >/dev/null
mv "$work"/out/canyon-*.tgz "$out"
