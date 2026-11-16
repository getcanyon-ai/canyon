#!/bin/sh
# scripts/telemetry-off-check.sh docker | kube: every Canyon server a check started sends no usage report. The
# runner's CI never reaches a server in a container or a pod, so each of Canyon's own stacks turns the report off in
# the server's own environment, DO_NOT_TRACK=1 or CANYON_TELEMETRY=0 (cmd/canyon-server/telemetry.go). This reads
# that environment from each running server: docker, every container on this Docker host whose process is
# /canyon-server with no command (egress-control and the other commands send nothing), from Docker's record of how it
# started; kube, every canyon-server pod in the cluster kubectl names,
# from its server process itself. It fails on any server without one, and when it finds no server at all.
set -eu
fail() { echo "telemetry-off-check: $*" >&2; exit 1; }
off='. == "DO_NOT_TRACK=1" or . == "CANYON_TELEMETRY=0"' # a jq test of one NAME=value

case ${1:-} in
docker)
  ids=$(docker ps -q)
  [ -n "$ids" ] || fail "no container is running, so no Canyon server to check"
  # shellcheck disable=SC2086 # ids, split on purpose
  servers=$(docker inspect $ids | jq -r '.[] | select(.Path == "/canyon-server" and (.Args // []) == [])
    | "\(.Name | ltrimstr("/")) \(any((.Config.Env // [])[]; '"$off"'))"')
  ;;
kube)
  pods=$(kubectl get pods -A -l app.kubernetes.io/name=canyon-server -o json |
    jq -r '.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running") | "\(.metadata.namespace) \(.metadata.name)"')
  environ=$(mktemp)
  trap 'rm -f "$environ"' EXIT
  servers=$(printf '%s\n' "$pods" | while read -r ns pod; do
    [ -n "$pod" ] || continue
    kubectl -n "$ns" exec "$pod" -c server -- cat /proc/1/environ >"$environ" || fail "$ns/$pod: its server's environment could not be read"
    tr '\0' '\n' <"$environ" | jq -R -s -r --arg pod "$ns/$pod" 'split("\n") | "\($pod) \(any(.[]; '"$off"'))"'
  done)
  ;;
*) fail "usage: scripts/telemetry-off-check.sh docker | kube" ;;
esac

[ -n "$servers" ] || fail "no Canyon server is running to check"
sends=$(printf '%s\n' "$servers" | awk '$2 != "true" { print $1 }')
[ -z "$sends" ] || fail "these Canyon servers would send a usage report, with neither DO_NOT_TRACK=1 nor CANYON_TELEMETRY=0 in their environment:" $sends
echo "telemetry-off-check: $(printf '%s\n' "$servers" | awk '{ print $1 }' | tr '\n' ' ')send no usage report"
