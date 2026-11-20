#!/bin/sh
# scripts/tour-check.sh: the docs' Take the tour against the laptop demo built from this tree: the commands canyon demo tour
# has an agent run, chapter by chapter, printing what the agent reads (CLAUDECODE is what Claude Code sets,
# cli/format.go), and the two settings the visitor makes in the dashboard, sent as the dashboard sends them. It fails
# when the journey breaks, and when a line the page pastes is not one this run printed, so paste the page's blocks from
# this log. It starts the demo's containers and removes them, volumes too, when it ends (make tour-check, the tour
# workflow).
set -eu
repo=$(cd "$(dirname "$0")/.." && pwd)
page=$repo/docs/src/content/docs/start/take-the-tour.md
laptop=$repo/docs/src/content/docs/start/try-it-on-your-laptop.md
host=http://localhost:7777
ready="Canyon is ready at http://localhost:7777" # the line canyon demo up ends its stages on
fail() { echo "tour-check: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

work=$(mktemp -d)
stack=$work/home/.canyon/demo/canyon # where canyon demo up unpacks the demo
mkdir -p "$work/home/.claude" # ~/.claude is there once Claude Code has started, and canyon skill install looks for it
finish() {
  rc=$?
  cd "$repo"
  [ "$rc" -eq 0 ] || [ ! -d "$stack" ] || (cd "$stack" && docker compose -p canyon-demo --profile demo-apps logs --no-color --tail=60 server executor demo-apps) || true
  [ ! -d "$stack" ] || HOME=$work/home "$repo/bin/canyon" demo down --purge || true
  docker rm -f canyon-tour-registry canyon-tour-release >/dev/null 2>&1 || true
  # what the executor made for apps, builds and scans goes with the demo, and with --purge the scanners' gigabytes of
  # databases and the build network too (#2697)
  left=$(for f in label=canyon.app label=canyon.build label=canyon.scan; do docker ps -a --format '{{.Names}}' --filter "$f"; done
    for f in label=canyon.app label=canyon.build; do docker network ls --format '{{.Name}}' --filter "$f"; done
    docker volume ls -q --filter name=^canyon-cache- --filter name=^canyon-scandb-)
  [ -z "$left" ] || { echo "tour-check: canyon demo down --purge left these behind: $(printf '%s' "$left" | tr '\n' ' ')" >&2; [ "$rc" -ne 0 ] || rc=1; }
  rm -rf "$work"
  exit "$rc"
}
trap finish EXIT
trap 'exit 1' INT TERM

# keep <file>: its lines printed, and kept for the page's check.
keep() { cat "$1"; cat "$1" >>"$work/all"; }
# run <exit code> <args>: canyon as the agent runs it.
run() {
  want=$1
  shift
  printf '\n$ canyon %s\n' "$*"
  code=0
  HOME=$work/home CLAUDECODE=1 "$repo/bin/canyon" "$@" >"$work/out" 2>&1 || code=$?
  keep "$work/out"
  [ "$code" = "$want" ] || fail "canyon $* exited $code, not $want"
}
# tool is an agent calling one of Canyon's tools in the tour folder, through the MCP server canyon skill install
# configured for Claude Code, and reading what it said: the tour names the tool first (design/specs/mcp.md §11).
tool() {
  want=$1 name=$2 args=${3-'{}'}
  printf '\n> %s %s\n' "$name" "$args"
  code=0
  HOME=$work/home "$work/mcpcall" "$name" "$(printf '%s' "$args" | jq -c --arg dir "$PWD" '. + {dir: $dir}')" -- $(server) >"$work/out" 2>&1 || code=$?
  keep "$work/out"
  [ "$code" = "$want" ] || fail "$name $args exited $code, not $want"
}
server() { jq -r '.mcpServers.canyon | [.command] + .args | join(" ")' "$work/home/.claude.json"; }
says() { grep -qE "$1" "$work/out" || fail "the last output does not match: $1"; }
# open <address> [<policy>]: what a browser gets from the app: its page, or the search for a policy.
open() {
  set -- "$1" "${2-}" "${1#http://}:127.0.0.1"
  if [ -n "$2" ]; then
    printf '\n$ curl --get --data-urlencode "policy=%s" %s/claims\n' "$2" "$1"
    status=$(curl -sS -o "$work/out" -w '%{http_code}' --resolve "$3" --get --data-urlencode "policy=$2" "$1/claims")
    echo >>"$work/out"
    keep "$work/out"
  else
    printf '\n$ curl %s\n' "$1"
    status=$(curl -sS -o "$work/out" -w '%{http_code}' --resolve "$3" "$1/")
    cat "$work/out"
  fi
  [ "$status" = 200 ] || fail "$1 answered $status, not 200"
}
# answers <address>: the app's page, a claim its search finds, and the injection it now takes for a policy number.
answers() {
  open "$1"
  says '<h1>Claims tracker</h1>'
  open "$1" MRD-1002
  says '^\[\{"policy_number":"MRD-1002",'
  open "$1" "' OR '1'='1"
  says '^\[\]$'
}
# block <n>: the page's nth js block, the line the agent writes.
block() { awk -v n="$1" '/^```/ { if (/^```js/) on = ++i == n; else on = 0; next } on' "$page"; }
# put <line>: server.js with its query replaced by this line, the one change the agent makes.
put() {
  cp server.js "$work/before"
  LINE=$1 awk '/FROM claims WHERE policy_number/ { sub(/[^ ].*/, ""); print $0 ENVIRON["LINE"]; next } { print }' "$work/before" >server.js
  [ "$(diff "$work/before" server.js | grep -c '^>')" = 1 ] || fail "the fix did not change one line of server.js: $1"
}

# person <method> <path> <body>: a request as the dashboard sends it for the visitor. Sign-in is off on the demo, so
# Canyon counts it as a person's, which a policy change has to be.
person() {
  printf '\n$ curl -X %s %s%s -d %s\n' "$1" "$host" "$2" "$3"
  status=$(curl -sS -o "$work/out" -w '%{http_code}' -X "$1" -H 'Content-Type: application/json' -d "$3" "$host$2")
  cat "$work/out"
  echo
  [ "$status" = 200 ] || fail "$1 $2 answered $status, not 200"
}

go build -o "$work/mcpcall" ./internal/tools/mcpcall

step "a release built from this tree: its images in a registry on this runner, its bundle and SHA256SUMS served beside it"
cd "$repo"
docker compose --profile demo-apps build server executor demo-apps
p=$(docker compose config --format json | jq -r .name)
docker run -d --name canyon-tour-registry -p 127.0.0.1:5000:5000 docker.io/library/registry:2.8.3 >/dev/null
for i in canyon-server canyon-executor canyon-demo-apps; do
  docker tag "$p-${i#canyon-}" "localhost:5000/$i:tour"
  docker push -q "localhost:5000/$i:tour" >&2
  echo "localhost:5000/$i:tour"
done >"$work/images.txt"
mkdir "$work/release"
scripts/deploy-bundle.sh HEAD "$work/images.txt" "$work/release/canyon-deploy.tar.gz"
(cd "$work/release" && sha256sum canyon-deploy.tar.gz >SHA256SUMS)
docker run -d --name canyon-tour-release --network host -v "$work/release:/www:ro" \
  busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e httpd -f -p 127.0.0.1:7781 -h /www >/dev/null
export CANYON_DEMO_FROM=http://127.0.0.1:7781
# the server CI starts sends no usage report (#2870): compose reads this file over the bundle's own two
export COMPOSE_FILE="docker-compose.yml:docker-compose.override.yml:$repo/e2e/compose.telemetry-off.yml"

step "what a first visitor sees: canyon --help and canyon demo --help"
cd "$work"
run 0 --help
says '^  demo  '
run 0 demo --help
says '^  canyon demo tour  '

step "the demo, as a visitor starts it: canyon demo up"
run 0 demo up
grep -qF "$ready" "$work/out" || fail "canyon demo up never showed: $ready"
says 'Canyon is ready at http://localhost:7777$'
says '^  Next, take the guided tour: canyon demo tour$'
"$repo/scripts/telemetry-off-check.sh" docker

step "the prompt: canyon demo tour, chapter 1 as the agent reads it, canyon skill install, and what is running"
cd "$work"
run 0 demo tour
says '^CANYON TOUR — CHAPTER 1 of 3: '
says '^Then run canyon skill install\. '
run 0 skill install
[ "$(server)" = "$repo/bin/canyon mcp" ] || fail "canyon skill install configured Claude Code's canyon server as '$(server)', not this canyon's mcp"
run 1 demo tour next
says "Chapter 1 isn't done: its files are not planted. Run canyon demo tour plant.$"
run 0 fleet

step "chapter 1, the visitor's one click: Apps use the org's design system to Blocking, on Policies' marked row"
person PUT '/api/v1/policies/design.org-system?apply=true' '{"state":"blocking"}'
run 0 manifest
says "^- Apps use the org's design system$"
says '^Design system: Canyon \(@canyon/ui\)$'

step "chapter 1: canyon demo tour plant, and canyon deploy, unchanged: blocked on the design system, exit 2"
run 0 demo tour plant
cd "$work/canyon-tour"
run 1 demo tour next
says "Chapter 1 isn't done: the app in ./canyon-tour has not been deployed yet. Run canyon deploy there.$"
tool 2 canyon_deploy
says '^CANYON REVIEW — RESULT: BLOCKED$'
says '^blocking: design\.org-system · package\.json:[0-9]+$'
app=$(sed -n 's/^app: //p' "$work/out" | head -1)
[ -n "$app" ] || fail "canyon deploy named no app"

step "chapter 1: canyon demo tour plant again, the page in the design system, canyon check and canyon deploy: live in preview"
run 0 demo tour plant
tool 0 canyon_check
says '^CANYON REVIEW — RESULT: PASSED$'
tool 0 canyon_deploy
says '^CANYON REVIEW — RESULT: PASSED$'
preview=$(sed -n 's/^live url: //p' "$work/out" | head -1)
case $preview in "http://$app.preview.localhost:7777") ;; *) fail "canyon deploy named no preview url: '$preview'" ;; esac
open "$preview"
says '<h1>Claims tracker</h1>'
run 0 demo tour next

step "chapter 2: canyon demo tour plant, the search, and canyon deploy, unchanged: blocked on the SQL, exit 2"
run 0 demo tour
says '^CANYON TOUR — CHAPTER 2 of 3: '
run 0 demo tour plant
tool 2 canyon_deploy
says '^CANYON REVIEW — RESULT: BLOCKED$'
says '^blocking: sql\.string-concat · server\.js:[0-9]+$'
open "$preview" # a blocked version replaces nothing
says '<h1>Claims tracker</h1>'

step "chapter 2: the one-line fix, canyon check and canyon deploy"
put "$(block 1)"
tool 0 canyon_check
says '^CANYON REVIEW — RESULT: PASSED$'
tool 0 canyon_deploy
says '^CANYON REVIEW — RESULT: PASSED$'
answers "$preview"
run 0 demo tour next

step "chapter 3: canyon_status, and canyon_promote with confirm, as the person asked for production"
run 0 demo tour
says '^CANYON TOUR — CHAPTER 3 of 3: '
tool 0 canyon_status
tool 0 canyon_promote '{"confirm":true}'
says '^CANYON PROMOTE — RESULT: PROMOTED$'
prod=$(sed -n 's/^live url: //p' "$work/out" | head -1)
case $prod in "http://$app.prod.localhost:7777") ;; *) fail "canyon deploy --env prod named no production url: '$prod'" ;; esac
answers "$prod"
run 0 fleet
run 0 demo tour next

step "the end"
run 0 demo tour
says '^CANYON TOUR — THE END: '

cp "$work/all" "$work/first" # what the page pastes: the first tour, not the second

step "run it again: canyon demo tour reset, the folder removed, and chapter 1 blocks an app of its own"
run 0 demo tour reset
cd "$work"
rm -rf canyon-tour
run 0 demo tour plant
cd "$work/canyon-tour"
tool 2 canyon_deploy
says '^blocking: design\.org-system · package\.json:[0-9]+$'
again=$(sed -n 's/^app: //p' "$work/out" | head -1)
[ -n "$again" ] && [ "$again" != "$app" ] || fail "the second tour's app is '$again', and the first's was '$app'"
answers "$preview" # the first tour's app runs on
answers "$prod"

step "the short tour: canyon demo tour skip, and chapter 2 plants the app chapter 1 would have left"
run 0 demo tour reset
cd "$work"
rm -rf canyon-tour
run 0 demo tour skip
run 0 demo tour
says '^CANYON TOUR — CHAPTER 2 of 3: '
run 0 demo tour plant
cd "$work/canyon-tour"
[ -f components.json ] || fail "chapter 2, reached by a skip, planted no components.json"
run 2 deploy # the shell, as an agent without Canyon's tools loaded runs it
says '^blocking: sql\.string-concat · server\.js:[0-9]+$'

step "the page: its prompt, the laptop page's install line, and every output it pastes one this run printed"
grep -qF "$(grep -m1 'install.sh -o install.sh' "$laptop")" "$page" || fail "the page's install line is not the laptop page's"
for said in "$ready" 'canyon demo up' 'Run "canyon demo tour" and follow it.' 'canyon demo tour --format ai' 'canyon demo tour skip' 'canyon demo tour end' 'canyon demo tour reset' 'canyon demo tour status' "Production runs the same image as preview, with production's own secrets. Nothing is rebuilt."; do
  grep -qF "$said" "$page" || fail "the page does not say: $said"
done
# the app's name is the run's own: the page's is the one its first preview address carries
named=$(sed -n 's|^live url: http://\(.*\)\.preview\.localhost:7777$|\1|p' "$page" | head -1)
[ -n "$named" ] || fail "the page pastes no preview address"
volatile='s/gr_[0-9a-f]\{16\}/gr_…/g; s/[0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}T[0-9:]*Z/…/g'
sed "$volatile; s/$app/app…/g" "$work/first" >"$work/printed"
awk '/^## What happens/ { on = 1 } /^## Run it again/ { on = 0 } /^```/ { text = on && /^```text/; next } text && NF && $0 != "⋯"' "$page" | sed "$volatile; s/$named/app…/g" >"$work/pasted"
[ -s "$work/pasted" ] || fail "the page pastes no output under What happens"
code=0
grep -vxFf "$work/printed" "$work/pasted" >"$work/missing" || code=$?
case $code in
  1) ;;
  0) cat "$work/missing"; fail "the page pastes these lines, and this run did not print them" ;;
  *) fail "the page's lines could not be compared with the run: grep exited $code" ;;
esac
# in the order the run printed them
awk 'NR == FNR { want[++n] = $0; next } i < n && $0 == want[i + 1] { i++ } END { exit i != n }' "$work/pasted" "$work/printed" ||
  fail "the page pastes what this run printed, but not in the order it printed it"
echo "$(($(wc -l <"$work/pasted"))) pasted lines, each one this run printed"

step "the demo gone: canyon demo down"
cd "$work"
run 0 demo down

printf '\ntour-check: the tour runs its three chapters to production, starts again and takes the short way, and the page pastes what it printed\n'
