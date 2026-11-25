#!/bin/sh
# scripts/builder-node.sh <image>: canyon-builder-node, for this machine's platform, into its Docker as <image>
# (contracts/executor.md, A Node app builds on Canyon's Node builder). It is canyon-build, Paketo's nodejs buildpack at
# the digest this tree pins, and the lifecycle the pinned canyon-builder carries, fetched from its release and held
# to its sha256. Its run image is canyon-run:noble, and NODE_ENV and NEXT_PRIVATE_OUTPUT_TRACE_ROOT are its own build
# environment, so pack builds on it without an ephemeral copy. The release publishes it (make images-release); the
# bundle checks build it as this does. LIFECYCLE_VERSION, LIFECYCLE_SHA256_AMD64 and LIFECYCLE_SHA256_ARM64 come from
# the Makefile, and SOURCE labels it.
set -eu
out=${1:?usage: scripts/builder-node.sh <image>}
: "${LIFECYCLE_VERSION:?the Makefile sets it}" "${LIFECYCLE_SHA256_AMD64:?}" "${LIFECYCLE_SHA256_ARM64:?}"
cd "$(dirname "$0")/.."
case $(uname -m) in
x86_64) arch=x86-64 sum=$LIFECYCLE_SHA256_AMD64 ;;
aarch64 | arm64) arch=arm64 sum=$LIFECYCLE_SHA256_ARM64 ;;
*) echo "builder-node: no lifecycle for $(uname -m)" >&2; exit 1 ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
.github/fetch.sh "https://github.com/buildpacks/lifecycle/releases/download/v$LIFECYCLE_VERSION/lifecycle-v$LIFECYCLE_VERSION+linux.$arch.tgz" "$sum" "$work/lifecycle.tgz"
docker buildx build --builder default --load -q -t canyon-build:builder-node - <executor/build.Dockerfile >/dev/null # Docker's own builder: the release's selects a container one, which keeps its images to itself
docker buildx build --builder default --load -q -t canyon-run:noble - <executor/run.Dockerfile >/dev/null # the name the builder names, which the executor's own build of the file takes
node=$(go run ./internal/tools/paketobump pin nodejs)
docker pull -q "$node" >/dev/null
version=$(docker image inspect --format '{{index .Config.Labels "io.buildpacks.buildpackage.metadata"}}' "$node" | jq -er .version)
cat >"$work/builder.toml" <<TOML
description = "Canyon's Node builder: canyon-build, Paketo's nodejs buildpack, and canyon-run:noble"

[[buildpacks]]
uri = "docker://$node"

[[order]]
[[order.group]]
id = "paketo-buildpacks/nodejs"
version = "$version"

[lifecycle]
uri = "$work/lifecycle.tgz"

[stack]
id = "io.buildpacks.stacks.noble"
build-image = "canyon-build:builder-node"
run-image = "canyon-run:noble"

[run]
[[run.images]]
image = "canyon-run:noble"

[build]
image = "canyon-build:builder-node"

[[build.env]]
name = "NODE_ENV"
value = "production"
suffix = "override"

[[build.env]]
name = "NEXT_PRIVATE_OUTPUT_TRACE_ROOT"
value = "/"
suffix = "override"
TOML
pack builder create "$out" --config "$work/builder.toml" --pull-policy if-not-present \
  --label org.opencontainers.image.licenses=Apache-2.0 --label "org.opencontainers.image.source=${SOURCE:-https://github.com/getcanyon-ai/canyon}"
