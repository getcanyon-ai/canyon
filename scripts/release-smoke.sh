#!/bin/sh
# scripts/release-smoke.sh <tag|latest>: Canyon's release as a user gets it, from the public downloads
# alone (.github/workflows/release-smoke.yml, #2409), under a HOME of its own: no sign-in and no registry login.
# The README's install.sh installs the CLI, which says it is <tag>; every CLI archive matches SHA256SUMS; every
# archive the release publishes passes scripts/release-archives-check.sh and unpacks without a word; the laptop demo
# (docs/src/content/docs/start/try-it-on-your-laptop.md) starts from canyon-deploy.tar.gz on images.txt's images,
# its sample apps build, and canyon fleet lists them; an app of its own, SQL built from the request, is BLOCKED on
# sql.string-concat, and with the query bound it PASSES and its preview serves it; and the chart lints and renders
# on images.txt's digests. Each step fails naming what it found; the guide's clean-up runs, pass or fail.
set -eu
want=${1:?usage: scripts/release-smoke.sh <tag|latest>}
here=$(cd "$(dirname "$0")" && pwd)
page=https://github.com/getcanyon-ai/canyon-releases/releases
host=http://localhost:7777
fail() { echo "release-smoke: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

if [ "$want" = latest ]; then
  release=$page/latest/download # the README's own URL
  tag=$(curl -fsS -o /dev/null -w '%{redirect_url}' "$page/latest") || fail "$page/latest does not answer"
  tag=${tag##*/}
else
  release=$page/download/$want tag=$want
fi
case $tag in v[0-9]*[!0-9A-Za-z.-]* | v[!0-9]* | [!v]* | '') fail "$want resolves to '$tag', not a release tag" ;; esac

work=$(mktemp -d)
HOME=$work/home PATH=$work/bin:$PATH
export HOME PATH
mkdir -p "$HOME" "$work/bin" "$work/dl"

finish() {
  rc=$?
  if [ -d "$work/canyon" ]; then
    cd "$work/canyon"
    [ "$rc" -eq 0 ] || docker compose --profile demo-apps logs --no-color --tail=60 server executor demo-apps || true
    step "clean up, as the laptop guide does"
    { # the guide's Clean up, line for line
      docker compose --profile demo-apps down -v &&
        c=$(docker ps -aq --filter label=canyon.app) && { [ -z "$c" ] || docker rm -f $c; } &&
        n=$(docker network ls -q --filter label=canyon.app) && { [ -z "$n" ] || docker network rm $n; } &&
        c=$(docker ps -aq --filter label=canyon.build; docker ps -aq --filter label=canyon.scan) && { [ -z "$c" ] || docker rm -f $c; } &&
        n=$(docker network ls -q --filter label=canyon.build) && { [ -z "$n" ] || docker network rm $n; } &&
        v=$(docker volume ls -q --filter dangling=true --filter name=^canyon-cache- --filter name=^canyon-scandb-) && { [ -z "$v" ] || docker volume rm $v; } &&
        [ -z "$(docker ps -aq --filter label=canyon.app)" ]
    } || { echo "release-smoke: the guide's clean-up left Canyon's containers behind" >&2; [ "$rc" -ne 0 ] || rc=1; }
  fi
  cd /
  rm -rf "$work"
  exit "$rc"
}
trap finish EXIT
trap 'exit 1' INT TERM

step "$tag: install the CLI with the README's command"
cd "$work"
curl -fsSL "$release/install.sh" -o install.sh && CANYON_INSTALL_DIR=$work/bin sh install.sh
got=$(canyon --version)
[ "$got" = "canyon $tag" ] || fail "the installed CLI says '$got', not 'canyon $tag'"
echo "canyon --version: $got"

step "the downloads: every file the release publishes"
cd "$work/dl"
case $want in latest) api=latest ;; *) api=tags/$tag ;; esac
assets=$(curl -fsSL "https://api.github.com/repos/getcanyon-ai/canyon-releases/releases/$api" | jq -er '.assets[].name') ||
  fail "the release lists no assets"
for f in $assets; do
  curl -fsSLO "$release/$f" || fail "$release/$f does not download"
  echo "$f"
done
sha256sum -c SHA256SUMS
for f in *.tar.gz *.tgz; do # each unpacks as a partner unpacks it, without a word from tar
  mkdir "$work/x"
  tar -xzf "$f" -C "$work/x" 2>"$work/tar-err" || fail "$f does not unpack: $(cat "$work/tar-err")"
  [ ! -s "$work/tar-err" ] || fail "tar says this unpacking $f: $(sort -u "$work/tar-err")"
  [ "$f" != canyon-deploy.tar.gz ] || mv "$work/x/canyon" "$work/canyon"
  rm -rf "$work/x"
done
echo "every tarball unpacks without a word from tar"
"$here/release-archives-check.sh" "$work/dl"
cat images.txt

step "the laptop demo: docker compose --profile demo-apps up, on the public images, with no login"
cd "$work/canyon"
docker compose --profile demo-apps config --format json | jq -r '.services[].image' | grep '^ghcr\.io/' | sort -u >"$work/compose-images"
grep -vxFf "$work/dl/images.txt" "$work/compose-images" && fail "the bundle runs these Canyon images, which images.txt does not name"
echo "the bundle runs $(($(wc -l <"$work/compose-images"))) of images.txt's images"
docker compose --profile demo-apps pull --quiet
docker compose -f docker-compose.yml -f docker-compose.override.yml -f "$here/../e2e/compose.telemetry-off.yml" --profile demo-apps up -d --wait --wait-timeout 600 # the server sends no usage report
i=0
until curl -fsS "$host/ready" >/dev/null; do
  i=$((i + 1)) && [ "$i" -lt 60 ] || fail "$host/ready never answered"
  sleep 2
done
"$here/telemetry-off-check.sh" docker
demo=$(docker compose --profile demo-apps ps -aq demo-apps)
[ -n "$demo" ] || fail "the demo-apps service never started"
code=$(timeout 1200 docker wait "$demo") || fail "the sample apps did not finish building in 20 minutes"
docker logs "$demo"
[ "$code" = 0 ] || fail "demo-apps exited $code"

step "canyon login and canyon fleet"
canyon login "$host"
canyon fleet >"$work/fleet"
cat "$work/fleet"
row() { grep -E "^  $1 " "$work/fleet" || fail "canyon fleet lists no $1"; }
for app in commission-calc quote-engine fnol-intake; do row "$app" >/dev/null; done
row claims-portal | grep -q blocked || fail "canyon fleet does not show claims-portal blocked"
row endorsement-hub | grep -q preview || fail "canyon fleet does not show endorsement-hub in preview"
for app in claims-archive premium-ledger; do
  row "$app" | grep ' prod ' | grep -q passed || fail "canyon fleet does not show $app passed and in prod"
done

step "an app of our own builds its SQL from the request: canyon deploy is BLOCKED"
mkdir "$work/team-notes"
cd "$work/team-notes"
cat >package.json <<'EOF'
{
  "name": "team-notes",
  "version": "1.0.0",
  "private": true,
  "scripts": {
    "start": "node server.js"
  },
  "engines": {
    "node": "22"
  },
  "dependencies": {
    "express": "4.21.2",
    "pg": "8.13.1"
  }
}
EOF
notes() { # server.js, its notes query given as $1
  cat >server.js <<EOF
const express = require("express");
const { Pool } = require("pg");

const app = express();
const pool = new Pool();

app.get("/", (req, res) => res.send("Team notes are coming soon."));

app.get("/notes", async (req, res) => {
  const { rows } = await $1;
  res.json(rows);
});

app.listen(process.env.PORT || 3000);
EOF
}
notes 'pool.query("SELECT * FROM notes WHERE author = '"'"'" + req.query.author + "'"'"'")'
code=0
canyon deploy --format ai >"$work/out" 2>&1 || code=$?
cat "$work/out"
[ "$code" = 2 ] || fail "canyon deploy exited $code, not 2 (blocked)"
grep -qE '^CANYON REVIEW .+ RESULT: BLOCKED$' "$work/out" || fail "canyon deploy did not say RESULT: BLOCKED"
blocking=$(grep '^blocking: ' "$work/out") || fail "canyon deploy names no blocking finding"
printf '%s\n' "$blocking" | grep -vqE '^blocking: sql\.string-concat [^ ]+ server\.js:[0-9]+$' &&
  fail "canyon deploy was blocked by more than sql.string-concat in server.js: $blocking"
[ -f canyon.toml ] || fail "the first deploy wrote no canyon.toml"

step "the same app with the query bound: canyon deploy PASSES and its preview serves it"
notes 'pool.query("SELECT * FROM notes WHERE author = $1", [req.query.author])'
code=0
canyon deploy --format ai >"$work/out" 2>&1 || code=$?
cat "$work/out"
[ "$code" = 0 ] || fail "canyon deploy exited $code, not 0"
grep -qE '^CANYON REVIEW .+ RESULT: PASSED$' "$work/out" || fail "canyon deploy did not say RESULT: PASSED"
grep -qx 'answered: 200 OK at /' "$work/out" || fail "canyon deploy does not say the preview answered 200 at /"
live=$(sed -n 's/^live url: //p' "$work/out" | head -1)
case $live in http://*.preview.localhost:7777) ;; *) fail "canyon deploy named no preview url: '$live'" ;; esac
# the app's own page, as a browser reaches it: an unknown host gets the dashboard, so a 200 alone proves nothing
status=$(curl -sS -o "$work/page" -w '%{http_code}' --resolve "${live#http://}:127.0.0.1" "$live/")
[ "$status" = 200 ] || fail "$live answered $status, not 200"
grep -qx 'Team notes are coming soon.' "$work/page" || fail "$live does not serve the app: $(head -c 200 "$work/page")"
echo "$live answered $status: $(cat "$work/page")"

step "the chart: helm lint and helm template on images.txt's digests"
cd "$work/dl"
ref() { grep -x "ghcr\.io/getcanyon-ai/canyon-$1@sha256:[0-9a-f]*" images.txt || fail "images.txt names no canyon-$1"; }
digest() { r=$(ref "$1") && echo "${r#*@}"; }
cat >values.yaml <<EOF
global:
  domain: canyon.example.com
images:
  server: {repository: ghcr.io/getcanyon-ai/canyon-server, digest: "$(digest server)"}
  executorKube: {repository: ghcr.io/getcanyon-ai/canyon-executor-kube, digest: "$(digest executor-kube)"}
  identity: {repository: ghcr.io/getcanyon-ai/canyon-identity, digest: "$(digest identity)"}
  run: {repository: ghcr.io/getcanyon-ai/canyon-run, digest: "$(digest run)"}
  build: {repository: ghcr.io/getcanyon-ai/canyon-build, digest: "$(digest build)"}
  opengrep: "$(ref opengrep)"
  eslint: "$(ref eslint)"
canyon:
  auth:
    admins: [admin@canyon.example.com]
  kubeAPI:
    cidrs: [192.0.2.10/32]
auth:
  pocketId:
    users: [{username: admin, email: admin@canyon.example.com, firstName: Canyon, lastName: Admin}]
EOF
helm lint canyon-chart.tgz -f values.yaml
helm template canyon canyon-chart.tgz -f values.yaml >rendered.yaml
grep -oE 'ghcr\.io/getcanyon-ai/[a-z-]+@sha256:[0-9a-f]{64}' rendered.yaml | sort -u >chart-images
grep -vE '/canyon-(executor|demo-apps|builder-node)@' images.txt | sort >chart-want # those three are Docker's alone
cmp -s chart-images chart-want || { diff chart-want chart-images >&2 || true; fail "the chart renders other Canyon images than images.txt's"; }
echo "the chart renders $(($(wc -l <chart-images))) of images.txt's images, each by digest"

echo
echo "release-smoke: $tag installs, its demo blocks and then serves an app, its chart renders, and every download carries its notices"
