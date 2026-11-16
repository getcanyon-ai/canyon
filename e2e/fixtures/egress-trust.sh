#!/usr/bin/env bash
# Writes <dir>/egress-trust.pem, the bundle e2e/compose.e2e.yml mounts over the gateway Envoy's trusted-CA file: the
# roots of the Envoy image docker-compose.yml runs, read from that image, then the echo's test CA (gen.sh's echo-ca.pem)
# and nothing else, so the stack's gateway verifies e2e/echo and every upstream it verified before (#2144). make e2e-up
# runs it after gen.sh; the shipped file and image are never changed.
set -euo pipefail

out=${1:?usage: egress-trust.sh <dir>}
repo=$(cd "$(dirname "$0")/../.." && pwd)
image=$(docker compose -f "$repo/docker-compose.yml" config --format json | python3 -c 'import json, sys; print(json.load(sys.stdin)["services"]["egress-envoy"]["image"])')
docker run --rm --entrypoint cat "$image" /etc/ssl/certs/ca-certificates.crt >"$out/egress-trust.pem.new"
grep -q 'BEGIN CERTIFICATE' "$out/egress-trust.pem.new" # fail closed: an image with no roots there is no bundle to extend
cat "$out/echo-ca.pem" >>"$out/egress-trust.pem.new"
mv "$out/egress-trust.pem.new" "$out/egress-trust.pem"
