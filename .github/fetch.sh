#!/bin/sh
# fetch.sh <url> <sha256> <file>: a pinned release file, downloaded, checked, and kept only when it matches (#2755).
# A failed download, a GitHub 504 among them, is tried again with curl's backoff: 1 s, doubling, 7 tries in all.
# With GH_TOKEN set, a https://github.com/ URL is fetched with it; it goes to no other host, as curl sends no
# Authorization header across GitHub's redirect to its download host. CI's one way to fetch a release: pack (the
# Makefile's pack-fetch), gitleaks, golangci-lint, lychee, oras, helm and kind's binaries and manifests. The
# images fetch theirs with the same retries in their own stages (Dockerfile.executor's pack, executor/opengrep.Dockerfile).
set -eu
url=$1 sum=$2 file=$3
set --
case $url in https://github.com/*) if [ -n "${GH_TOKEN:-}" ]; then set -- -H "Authorization: Bearer $GH_TOKEN"; fi ;; esac
rm -f "$file.part"
curl -sSfL --retry 6 --retry-all-errors "$@" -o "$file.part" "$url"
if ! echo "$sum  $file.part" | sha256sum -c - >/dev/null; then
  echo "fetch.sh: $url is not the pinned file (sha256 $sum)" >&2
  rm -f "$file.part"
  exit 1
fi
mv "$file.part" "$file"
