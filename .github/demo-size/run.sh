#!/usr/bin/env bash
# run.sh today | nodeonly | cases <out>
set -uo pipefail
part=$1; export OUT=$2 TRIAL_OUT=$2; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../.." && pwd); cd "$repo"
export TRIAL_LIB=$here/lib.sh TRIAL_TOUR=$repo/cli/testdata/tour/app; . "$here/lib.sh"
cp "$here/trial2_test.go.txt" executor/trial2_test.go
go test -tags e2e -run '^$' ./executor || exit 1
# nodeonly: Canyon's build image, Paketo's nodejs buildpack at the digest Canyon pins, the lifecycle the builder carries
nodeonly() {
  node=$(grep -o 'docker.io/paketobuildpacks/nodejs@sha256:[0-9a-f]*' deploy/k8s/builds.yaml | head -1)
  docker build -q -t canyon-build:trial - <executor/build.Dockerfile >/dev/null
  docker pull -q "$node" >/dev/null
  ver=$(docker image inspect "$node" --format '{{index .Config.Labels "io.buildpacks.buildpackage.metadata"}}' | jq -r .version)
  printf '[[buildpacks]]\nuri = "docker://%s"\n\n[[order]]\n[[order.group]]\nid = "paketo-buildpacks/nodejs"\nversion = "%s"\n\n[lifecycle]\nversion = "0.21.29"\n\n[stack]\nid = "io.buildpacks.stacks.noble"\nbuild-image = "canyon-build:trial"\nrun-image = "canyon-run:noble"\n' "$node" "$ver" >"$RUNNER_TEMP/builder.toml"
  pack builder create canyon-builder-nodejs:trial --config "$RUNNER_TEMP/builder.toml" --pull-policy if-not-present >"$OUT/builder-create.txt" 2>&1
}
export -f nodeonly
snap 00-before
case $part in
today | nodeonly)
  TRIAL_PART=$part go test -tags e2e -count=1 -timeout 60m -run '^TestTrialCost$' -v ./executor 2>&1 | tee "$OUT/trial.txt" ;;
cases)
  go test -tags e2e -count=1 -timeout 70m -run '^TestTrialCases$' -v ./executor 2>&1 | tee "$OUT/trial.txt" ;;
esac
snap 99-end
cat "$OUT/stages.tsv"
grep -E '^(CASE|STAGE)' "$OUT/trial.txt"
exit 0
