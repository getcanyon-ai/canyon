#!/bin/sh
# scripts/opengrep-source.sh <out.tar.gz>: the complete corresponding source of the Opengrep that
# canyon-opengrep carries (executor/opengrep.Dockerfile, LGPL-2.1-only), as one tarball: the v1.30.0
# commit and every submodule at the commit that commit records, without .git. Git checks each by its
# hash: the top commit is pinned here, and it pins the rest. scripts/publish-release.sh attaches it to
# every public release; the image's /usr/share/licenses/opengrep/SOURCE points there.
set -eu
out=${1:?usage: scripts/opengrep-source.sh <out.tar.gz>}
case $out in /*) ;; *) out=$PWD/$out ;; esac
version=1.30.0
commit=acf67b45c97c4b63626536605c77064ef536806d
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
dir=$work/opengrep-$version
git init -q "$dir"
git -C "$dir" fetch -q --depth 1 https://github.com/opengrep/opengrep.git "$commit"
git -C "$dir" checkout -q FETCH_HEAD
[ "$(git -C "$dir" rev-parse HEAD)" = "$commit" ] || { echo "opengrep-source: fetched $(git -C "$dir" rev-parse HEAD), not $commit" >&2; exit 1; }
git -C "$dir" submodule -q update --init --recursive --depth 1
git -C "$dir" submodule status --recursive | grep -v '^ ' && { echo "opengrep-source: a submodule is not at the commit $commit records" >&2; exit 1; }
"$(dirname "$0")/tarball.sh" "$out" "$work" "opengrep-$version" --exclude=.git
