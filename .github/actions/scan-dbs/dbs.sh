#!/usr/bin/env bash
# The scanner databases of an E2E stack (action.yml): trivy's, trivy's Java database and grype's, each the volume
# canyon-scandb-<tool> the Docker executor's scans read it from (executor/scan.go, toolDB), with the images the
# executor pins. Each volume is bound to <dir>/<tool>, the cached copy itself, so nothing is copied; the three are
# seeded, and checked, at once.
#   dbs.sh seed <dir> <day>  each database from its cached copy when that copy reads as a scan reads it, or else
#                            downloaded here, on the runner, outside the stack, into <dir>/<tool>, with save=true for
#                            the action to cache it; then the stack's own refreshes pointed at registries that never
#                            resolve (.invalid, RFC 2606), so no stack downloads one live
#   dbs.sh check <dir>       after the tests: red when a run waited on a database download, or a database no
#                            longer reads. One whose files are as they were when it read at seed reads still; one
#                            changed is read again (grype hashes its whole database on every read: ~25 s cold)
set -euo pipefail

pin() { go doc -u ./executor "$1" | sed -n "s/^const $1 = \"\(.*\)\"\$/\1/p"; } # the executor's pin
trivy=$(pin trivyImage)
grype=$(pin grypeImage)
[[ -n $trivy && -n $grype ]] || { echo "::error::the executor's scanner image pins could not be read (go doc)"; exit 1; }
tools=(trivy trivy-java grype)

# ops runs a shell command as root in trivy's image, which has one (grype's has none), with canyon-scandb-<tool> at /db.
ops() {
  local tool=$1 command=$2
  shift 2
  docker run --rm --volume "canyon-scandb-$tool:/db" "$@" --entrypoint sh "$trivy" -c "$command"
}

# reads reads the tool's database as a scan reads it: as the scan's user, with no network, the volume read-only.
reads() {
  local scan=(docker run --rm --network none --read-only --user 65534:65534 --tmpfs /tmp:mode=1777)
  case $1 in
  trivy) # a scan, offline
    "${scan[@]}" --tmpfs /tree --volume canyon-scandb-trivy:/scan/db:ro "$trivy" fs --cache-dir /scan/db --cache-backend memory --scanners vuln \
      --skip-db-update --skip-java-db-update --offline-scan --disable-telemetry --skip-version-check --no-progress --quiet /tree >/dev/null ;;
  trivy-java) # the executor's reader: SQLite's header, then the metadata that dates it
    "${scan[@]}" --volume canyon-scandb-trivy-java:/db:ro --entrypoint sh "$trivy" \
      -c '[ "$(head -c 15 /db/java-db/trivy-java.db)" = "SQLite format 3" ] && head -c 1048576 /db/java-db/metadata.json' | jq -e .UpdatedAt >/dev/null ;;
  grype) # the executor's reader: grype's own status of its database, its age check off
    "${scan[@]}" --env GRYPE_DB_CACHE_DIR=/db --env GRYPE_DB_VALIDATE_AGE=false --volume canyon-scandb-grype:/db:ro "$grype" db status -o json | jq -e .built >/dev/null ;;
  esac
}

# download downloads the tool's database into its emptied volume, as the executor's refresh does.
download() {
  case $1 in
  trivy) ops trivy 'find /db -mindepth 1 -maxdepth 1 -exec rm -rf {} + && exec trivy fs --cache-dir /db --download-db-only --disable-telemetry --skip-version-check --no-progress' ;;
  trivy-java) # its near-1 GB download staged in the volume, not the container's layer
    ops trivy-java 'find /db -mindepth 1 -maxdepth 1 -exec rm -rf {} + && mkdir /db/tmp && trivy image --cache-dir /db --download-java-db-only --disable-telemetry --skip-version-check --no-progress && rm -rf /db/tmp' --env TMPDIR=/db/tmp ;;
  grype) # as the scan's user, whose volume it is as the executor's refresh makes it
    ops grype 'find /db -mindepth 1 -maxdepth 1 -exec rm -rf {} + && chown 65534:65534 /db' &&
      docker run --rm --read-only --user 65534:65534 --tmpfs /tmp:mode=1777 --env GRYPE_DB_CACHE_DIR=/db --volume canyon-scandb-grype:/db "$grype" db update ;;
  esac
}

# own makes grype's database the scan's user's, as the executor's refresh makes it (grype makes its directory 0700),
# and every database readable by all, so the runner's cache save reads one downloaded as root or as that user.
own() {
  local chown=
  [ "$1" = grype ] && chown='chown -R 65534:65534 /db && '
  ops "$1" "${chown}chmod -R a+rX /db"
}

# seed_one seeds one tool's database, its volume bound to <dir>/<tool>, and marks <marks>/<tool> when it downloaded it.
# It runs in the background, where set -e does not reach: every step's failure returns.
seed_one() {
  local tool=$1 data=$2/$1 day=$3 marks=$4 start=$SECONDS try
  mkdir -p "$data" || return 1
  # a reused runner may keep a plain volume under this name, which create would answer without the bind
  docker volume rm -f "canyon-scandb-$tool" >/dev/null || return 1
  docker volume create --label canyon.scandb --driver local --opt type=none --opt o=bind --opt "device=$data" "canyon-scandb-$tool" >/dev/null || return 1
  if [ -n "$(ls -A "$data")" ] && own "$tool" && reads "$tool"; then
    fingerprint "$data" >"$RUNNER_TEMP/scan-dbs-$tool" || return 1
    echo "$tool's database: the cached copy of $day, read in $((SECONDS - start)) s"
    return 0
  fi
  echo "$tool's database: no cached copy that reads, so downloaded here, on the runner, and cached" # over it: never used
  for try in 1 2 3; do
    download "$tool" && break
    [ "$try" = 3 ] && return 1
    sleep 20
  done
  own "$tool" && reads "$tool" && fingerprint "$data" >"$RUNNER_TEMP/scan-dbs-$tool" && touch "$marks/$tool" || return 1
  echo "$tool's database: downloaded and read in $((SECONDS - start)) s"
}

# each runs fn for every tool at once, and fails when any one of them failed.
each() {
  local fn=$1 tool red=0 pids=()
  shift
  for tool in "${tools[@]}"; do
    "$fn" "$tool" "$@" &
    pids+=($!)
  done
  for pid in "${pids[@]}"; do
    wait "$pid" || red=1
  done
  return "$red"
}

seed() {
  local marks
  marks=$(mktemp -d)
  each seed_one "$1" "$2" "$marks" || { echo "::error::a scanner database could not be seeded"; exit 1; }
  # once all three read: never a stack whose refreshes are blocked with a database missing
  [ -z "$(ls -A "$marks")" ] || echo save=true >>"$GITHUB_OUTPUT"
  {
    echo CANYON_SCANDB_TRIVY=scandb.invalid/blocked/trivy-db:2
    echo CANYON_SCANDB_TRIVY_JAVA=scandb.invalid/blocked/trivy-java-db:1
    echo CANYON_SCANDB_GRYPE=https://scandb.invalid/blocked/grype/latest.json
  } >>"$GITHUB_ENV"
}

# fingerprint is each file under a database's directory: its inode, size, modification time and path.
fingerprint() { find "$1" -type f -printf '%i %s %C@ %m %U:%G %P\n' | sort; } # ctime, mode and owner: a rewrite with its mtime put back, a chmod or a chown reads again

# still_reads is red when the tool's database no longer reads: one whose files are as they were when it read at seed
# reads still, and one changed since, or not fingerprinted, is read again.
still_reads() {
  local tool=$1 data=$2/$1 start=$SECONDS seen=$RUNNER_TEMP/scan-dbs-$1
  if [ -s "$seen" ] && [ "$(fingerprint "$data" 2>/dev/null)" = "$(cat "$seen")" ]; then
    echo "$tool's database still reads: its files are as they were when it read at seed"
  elif reads "$tool"; then
    echo "$tool's database still reads: changed since the seed, and read again in $((SECONDS - start)) s"
  else
    echo "::error::$tool's database no longer reads as a scan reads it"
    return 1
  fi
}

check() {
  local dir=$1 pg waited red=0
  pg=$(docker ps -q --filter label=com.docker.compose.service=postgres)
  [ -n "$pg" ] || { echo "::error::no Postgres container of the stack to read its runs from"; exit 1; }
  waited=$(docker exec "$pg" psql -U canyon -d canyon -v ON_ERROR_STOP=1 -tAc \
    "SELECT coalesce(string_agg(tool || ': ' || n, ', '), '') FROM (SELECT data->>'tool' AS tool, count(*) AS n FROM timeline_rows WHERE kind = 'scan.waiting' GROUP BY 1) w")
  echo "runs that waited on a scanner database download: ${waited:-none}"
  [ -z "$waited" ] || red=1
  each still_reads "$dir" || red=1
  exit "$red"
}

case ${1:-} in
seed) seed "$2" "$3" ;;
check) check "$2" ;;
*) echo "usage: dbs.sh seed <dir> <day> | dbs.sh check <dir>" >&2 && exit 2 ;;
esac
