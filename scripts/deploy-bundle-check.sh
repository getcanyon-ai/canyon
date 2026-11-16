#!/bin/sh
# scripts/deploy-bundle-check.sh <canyon-deploy.tar.gz> <docker.md>: the bundle starts with no source. Unpacked into
# an empty directory: compose accepts it, no service in it builds, every file it names is in it, the stack comes up
# healthy, canyon fleet, run from the demo-apps service as the sample apps run it, answers from its server at
# localhost, a request naming the server by its container name, as a rebound browser could, is refused 421, and a
# container on its network, a peer neither loopback nor the gateway, is closed at accept.
# Then the team setup on one Docker server, as the guide docker.md (docs: self-host/docker.md) writes it with its
# own commands: its .env makes deploy/docker/team.yml's compose sign people in through the identity provider at an
# https address, behind Caddy alone, on the operator's own passwords and token, and Caddy accepts its Caddyfile.
# It starts the laptop demo's containers and removes them, volumes too, when it ends (make deploy-bundle-check,
# CI's deploy-bundle job).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
usage='usage: scripts/deploy-bundle-check.sh <canyon-deploy.tar.gz> <docker.md>'
bundle=${1:?$usage}
guide=${2:?$usage}
case $guide in /*) ;; *) guide=$PWD/$guide ;; esac
# The guide's download, first: the bundle unpacks when it matches SHA256SUMS, and not at all when it doesn't.
"$here/guide-download-check.sh" "$guide" "$bundle"
fail() { echo "deploy-bundle-check: $*" >&2; exit 1; }
d=$(mktemp -d)
trap 'cd /; (cd "$d/canyon" 2>/dev/null && docker compose --profile demo-apps down -v >/dev/null 2>&1); rm -rf "$d"' EXIT
tar -xzf "$bundle" -C "$d"
cd "$d/canyon"

docker compose --profile demo-apps config -q
echo "compose accepts the bundle"
cfg=$(docker compose --profile demo-apps config --format json)
builds=$(printf '%s\n' "$cfg" | jq -r '.services | to_entries[] | select(.value.build) | .key')
missing=$(printf '%s\n' "$cfg" | jq -r --arg dir "$PWD/" '[.configs[]?.file, .secrets[]?.file,
    (.services[].volumes[]? | select(.type == "bind") | .source), (.services[].env_file[]?.path)]
  | map(select(. != null and startswith($dir)))[]' | while read -r f; do [ -e "$f" ] || echo "${f#"$PWD"/}"; done)
[ -z "$builds" ] || echo "deploy-bundle-check: the bundle builds these services, from source it does not carry:" $builds >&2
[ -z "$missing" ] || echo "deploy-bundle-check: the bundle names files it does not carry:" $missing >&2
[ -z "$builds$missing" ] || exit 1
echo "nothing in the bundle builds, and every file it names is in it"

docker compose -f docker-compose.yml -f docker-compose.override.yml -f "$here/../e2e/compose.telemetry-off.yml" up -d --wait --wait-timeout 300 || { docker compose ps -a; docker compose logs --tail=40; fail "the bundle's stack did not come up healthy"; }
echo "the bundle's stack is up and healthy"
"$here/telemetry-off-check.sh" docker
docker compose run --rm --no-deps --entrypoint canyon demo-apps fleet --host http://localhost:7777
echo "canyon fleet answers from the bundle's server, from the demo-apps service at localhost"
code=$(docker run --rm --network container:canyon-server curlimages/curl:8.11.1 -s -m 5 -o /dev/null -w '%{http_code}' -H 'Host: canyon-server:7777' http://127.0.0.1:7777/api/v1/fleet) || true
[ "$code" = 421 ] || fail "the demo's server answered a Host other than localhost with $code, not 421: with sign-in off, a rebound browser would be its admin"
echo "the demo's server refuses a Host other than localhost, 421"
rc=0; docker run --rm --network canyon-internal curlimages/curl:8.11.1 -sS -m 5 -o /dev/null http://canyon-server:7777/ready || rc=$?
[ "$rc" = 52 ] || [ "$rc" = 56 ] || fail "a container on canyon-internal, a peer off the server's machine as a client Docker forwards keeps its own address, got curl exit $rc, not 52 or 56: closed at accept is the only answer under sign-in off (#2449)"
echo "the demo's server closes a peer that is neither loopback nor its network's gateway at accept"
docker compose --profile demo-apps down -v >/dev/null 2>&1

# The team setup, from the guide's own blocks, each run as the guide's reader runs it (scripts/guide-block.sh).
block() { "$here/guide-block.sh" "$guide" "$1"; }
export DOMAIN=canyon.example.com
sh -eu -c "$(block 'Write .env')" || fail "the guide's .env does not make a complete compose"
[ "$(stat -c %a .env)" = 600 ] || fail ".env is not readable by its owner alone"
team=$(docker compose config --format json)
holds() { printf '%s\n' "$team" | jq -e "$1" >/dev/null || fail "the team setup: $2"; }
holds '.services.server.environment.CANYON_AUTH == "oidc"' "the server signs people in through the identity provider"
holds '.services.server.environment.CANYON_PUBLIC_URL == "https://canyon.example.com"' "Canyon is at https://<domain>"
holds '.services.server.environment.CANYON_DEMO_SEED == ""' "the fleet starts empty, with no demo seed"
holds '(.services.server.ports // []) == []' "the server publishes no port: Caddy is the way in"
holds '[.services | to_entries[] | select((.value.ports // []) | length > 0) | .key] == ["caddy"]' "Caddy alone is published"
holds '[.services.caddy.ports[] | "\(.published):\(.target)"] | sort == ["443:443", "80:80"]' "Caddy publishes 80 and 443, and nothing else"
holds '.services.server.environment.CANYON_TRUSTED_PROXIES == "\(.services.caddy.networks["canyon-edge"].ipv4_address)/32"' "the server trusts Caddy's one pinned address, and nothing wider"
holds '.networks["canyon-edge"].ipam.config[0].subnet == "10.214.4.0/24"' "Caddy's network is outside the apps' pool"
holds '.services["demo-apps"] == null' "the sample apps never start"
holds '[.services[] | .restart] | all(. == "unless-stopped")' "every service restarts with Docker"
holds '.services.postgres.environment.POSTGRES_PASSWORD | test("^[0-9a-f]{48}$")' "Canyon's Postgres has the operator's password"
holds '.services["app-postgres"].environment.POSTGRES_PASSWORD | test("^[0-9a-f]{48}$")' "the apps' Postgres has the operator's password"
holds '.services.server.environment.CANYON_DATABASE_URL == "postgres://canyon:\(.services.postgres.environment.POSTGRES_PASSWORD)@postgres:5432/canyon?sslmode=disable"' "the server logs in to Canyon's Postgres with its password"
holds '.services.server.environment.CANYON_APP_DATABASE_URL == "postgres://canyon:\(.services["app-postgres"].environment.POSTGRES_PASSWORD)@app-postgres:5432/canyon?sslmode=disable"' "the server logs in to the apps' Postgres with its password"
holds '.services.server.environment.CANYON_EGRESS_RECEIVER_TOKEN | test("^[0-9a-f]{64}$")' "the egress receiver has the operator's token"
holds '.services["egress-envoy"].environment.CANYON_EGRESS_RECEIVER_TOKEN == .services.server.environment.CANYON_EGRESS_RECEIVER_TOKEN' "Envoy sends the server's token"
echo "the guide's .env runs the team setup: oidc at https://canyon.example.com, Caddy alone published, its one address trusted, on its own passwords and token"
grep -v '^CANYON_ADMINS=' .env > .env.short
if out=$(docker compose --env-file .env.short config -q 2>&1); then fail "a .env without CANYON_ADMINS made a compose"; fi
case $out in *CANYON_ADMINS*) ;; *) fail "a .env without CANYON_ADMINS was refused without naming it: $out" ;; esac
echo "a .env missing a value is refused, naming it"

sh -eu -c "$(block 'Write the Caddyfile')" || fail "the guide's Caddyfile commands failed"
caddy=$(printf '%s\n' "$team" | jq -r '.services.caddy.image')
docker run --rm -v "$PWD/Caddyfile:/etc/caddy/Caddyfile:ro" "$caddy" caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
echo "Caddy, as team.yml runs it, accepts the guide's Caddyfile"
