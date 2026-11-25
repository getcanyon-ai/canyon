#!/bin/sh
# scripts/deploy-bundle.sh <git-ref> <images.txt> <out.tar.gz>: canyon-deploy.tar.gz, the laptop demo, the team
# setup on one Docker server (deploy/docker) and deploy/k8s as <git-ref> has them, with LICENSE, NOTICE and THIRD_PARTY_NOTICES, under canyon/. Its
# docker-compose.override.yml, which docker compose reads beside docker-compose.yml, pulls the image
# images.txt names (a line ending /<name>@<digest> or /<name>:<tag>) where the file builds one, so the
# bundle starts with no source. scripts/publish-release.sh builds it for a release; make
# deploy-bundle-check builds it from this tree and boots it (CI's deploy-bundle job).
set -eu
ref=${1:?usage: scripts/deploy-bundle.sh <git-ref> <images.txt> <out.tar.gz>}
images=${2:?usage: scripts/deploy-bundle.sh <git-ref> <images.txt> <out.tar.gz>}
out=${3:?usage: scripts/deploy-bundle.sh <git-ref> <images.txt> <out.tar.gz>}
case $out in /*) ;; *) out=$PWD/$out ;; esac
fail() { echo "deploy-bundle: $*" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/canyon"
# Every file docker-compose.yml names (egress's floor, Envoy's entrypoint and bootstrap), and deploy/k8s;
# make deploy-bundle-check fails on a file compose names that the bundle lacks.
git archive "$ref" LICENSE NOTICE THIRD_PARTY_NOTICES docker-compose.yml deploy/docker deploy/egress deploy/gitops deploy/k8s \
  ':(exclude)deploy/egress/*.go' ':(exclude)deploy/k8s/*.go' | tar -x -C "$work/canyon"

image() { grep -E "/$1[@:]" "$images" || fail "$images names no $1"; }
server=$(image canyon-server)
executor=$(image canyon-executor)
demo_apps=$(image canyon-demo-apps)
builder_node=$(image canyon-builder-node)
cat > "$work/canyon/docker-compose.override.yml" <<YAML
# The release's images, in place of every build docker-compose.yml has (scripts/deploy-bundle.sh):
# egress-control is canyon-server's image, run as its subcommand. The executor builds a Node app on the release's
# Node builder (contracts/executor.md, A Node app builds on Canyon's Node builder).
services:
  server: {image: $server, build: !reset null}
  executor: {image: $executor, build: !reset null, environment: {CANYON_BUILDER_NODE: $builder_node}}
  demo-apps: {image: $demo_apps, build: !reset null}
  egress-control: {image: $server, build: !reset null}
YAML
"$(dirname "$0")/tarball.sh" "$out" "$work" canyon
