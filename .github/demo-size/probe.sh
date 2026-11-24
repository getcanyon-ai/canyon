#!/usr/bin/env bash
# probe.sh <out>: the builder's build-config, a Python app's first build on a Docker with only the Node builder,
# claims-portal with and without its build caches, and one build's lifecycle phases timed.
set -uo pipefail
export OUT=$1 TRIAL_OUT=$1; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../.." && pwd); cd "$repo"
export TRIAL_REPO=$repo TRIAL_LIB=$here/lib.sh; . "$here/lib.sh"
cp "$here/probe_test.go.txt" executor/probe_test.go
go test -tags e2e -run '^$' ./executor || exit 1
mkdir -p "$RUNNER_TEMP/wrapper"; cp "$here/pack" "$RUNNER_TEMP/wrapper/pack"; chmod +x "$RUNNER_TEMP/wrapper/pack"; export PATH=$RUNNER_TEMP/wrapper:$PATH
nodeonly() {
  cd "$TRIAL_REPO" || return 1
  node=$(grep -o 'docker.io/paketobuildpacks/nodejs@sha256:[0-9a-f]*' deploy/k8s/builds.yaml | head -1)
  docker build -q -t canyon-build:trial - <executor/build.Dockerfile || return 1
  docker pull -q "$node" >/dev/null || return 1
  ver=$(docker image inspect "$node" --format '{{index .Config.Labels "io.buildpacks.buildpackage.metadata"}}' | jq -r .version)
  printf '[[buildpacks]]\nuri = "docker://%s"\n\n[[order]]\n[[order.group]]\nid = "paketo-buildpacks/nodejs"\nversion = "%s"\n\n[lifecycle]\nversion = "0.21.29"\n\n[stack]\nid = "io.buildpacks.stacks.noble"\nbuild-image = "canyon-build:trial"\nrun-image = "canyon-run:noble"\n' "$node" "$ver" >"$RUNNER_TEMP/builder.toml"
  /usr/local/bin/pack builder create canyon-builder-nodejs:trial --config "$RUNNER_TEMP/builder.toml" --pull-policy if-not-present >"$OUT/builder-create.txt" 2>&1
}
export -f nodeonly
snap 00-before
go test -tags e2e -count=1 -timeout 80m -run '^TestProbe$' -v ./executor 2>&1 | tee "$OUT/probe.txt"
snap 99-end
cat "$OUT/stages.tsv"
grep -E '^(STAGE|CONFIG|PHASE)' "$OUT/probe.txt"
exit 0
