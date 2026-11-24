#!/usr/bin/env bash
# trial.sh today | thin <out>: the executor of this tree builds the sample apps, a Python and a Go app, on today's
# builder or on Paketo's noble base without buildpacks, the same version, plus each app's one language.
set -uo pipefail
variant=$1; export OUT=$2 TRIAL_OUT=$2; mkdir -p "$OUT"
here=$(cd "$(dirname "$0")" && pwd); repo=$(cd "$here/../.." && pwd); cd "$repo"
export TRIAL_LIB=$here/lib.sh; . "$here/lib.sh"
echo "$repo" >/tmp/trial-repo
cp "$here/sizetrial_test.go.txt" executor/sizetrial_test.go
if [ "$variant" = thin ]; then
  touch /tmp/trial-thin
  sed -i 's#^FROM docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:[0-9a-f]*$#FROM docker.io/paketobuildpacks/ubuntu-noble-builder-buildpackless@sha256:ac77a88ca89d2653883f37129cc624829fdba7026d73fa9a2476aa6ef23d0f2c#' executor/builder.Dockerfile
  grep -q '^FROM docker.io/paketobuildpacks/ubuntu-noble-builder-buildpackless@' executor/builder.Dockerfile || { echo "::error::the base did not change"; exit 1; }
fi
export TRIAL_GO=$RUNNER_TEMP/go-hello; mkdir -p "$TRIAL_GO"
printf 'module hello\n\ngo 1.26\n' >"$TRIAL_GO/go.mod"
printf 'package main\n\nimport (\n\t"fmt"\n\t"net/http"\n\t"os"\n\t"runtime"\n)\n\nfunc main() {\n\thttp.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, runtime.Version()) })\n\t_ = http.ListenAndServe(":"+os.Getenv("PORT"), nil)\n}\n' >"$TRIAL_GO/main.go"
go test -tags e2e -run '^$' ./executor || exit 1 # compiled, and its modules downloaded, before anything is counted
mkdir -p "$RUNNER_TEMP/wrapper"; cp "$here/pack" "$RUNNER_TEMP/wrapper/pack"; chmod +x "$RUNNER_TEMP/wrapper/pack"
export PATH=$RUNNER_TEMP/wrapper:$PATH
snap 00-before
go test -tags e2e -count=1 -timeout 110m -run '^TestSizeTrial$' -v ./executor 2>&1 | tee "$OUT/trial.txt"
snap 01-after
for i in $(docker image ls --format '{{.Repository}}' | grep -E 'canyon-builder' | sort -u); do docker image inspect "$(docker image ls -q "$i" | head -1)" --format '{{json .Config.Labels}}' >"$OUT/builder-labels.json"; done
jq -r '.["io.buildpacks.builder.metadata"] | fromjson | {lifecycle: .lifecycle.version, buildpacks: [(.buildpacks // [])[] | select(.id | test("^paketo-buildpacks/(nodejs|python|go|php|web-servers)$")) | "\(.id) \(.version)"]}' "$OUT/builder-labels.json"
if [ "$variant" = thin ]; then
  echo "=== Canyon's own end-to-end tests on the thin base"
  go test -tags e2e -count=1 -timeout 40m -v -run '^(TestEndToEndBuildsAndServesAPHPAppOnPostgres|TestEndToEndBuildsAndServesAStaticSPA|TestEndToEndATreeNoBuildpackDetectsIsAFailedDetect|TestRebaseE2EDockerLayersAboveRunImageIdentical|TestEndToEndABuildReachesNoPrivateNorLinkLocalAddress)$' ./executor 2>&1 | tail -150 | tee "$OUT/e2e-thin.txt"
fi
echo "=== the registries unreachable, everything on the host"
for h in registry-1.docker.io auth.docker.io index.docker.io docker.io production.cloudflare.docker.com registry.k8s.io ghcr.io; do echo "127.0.0.1 $h" | sudo tee -a /etc/hosts >/dev/null; done
go test -tags e2e -count=1 -timeout 25m -run '^TestSizeTrialUnreachable$' -v ./executor 2>&1 | tee "$OUT/unreachable.txt"
sudo sed -i '/^127.0.0.1 \(registry-1.docker.io\|auth.docker.io\|index.docker.io\|docker.io\|production.cloudflare.docker.com\|registry.k8s.io\|ghcr.io\)$/d' /etc/hosts
if [ "$variant" = thin ]; then
  echo "=== a builder made on the host with nodejs alone, from Canyon's build image"
  rm -f /tmp/trial-thin
  node=$(grep -o 'docker.io/paketobuildpacks/nodejs@sha256:[0-9a-f]*' deploy/k8s/builds.yaml | head -1)
  ver=$(docker image inspect "$node" --format '{{index .Config.Labels "io.buildpacks.buildpackage.metadata"}}' 2>/dev/null | jq -r .version)
  life=$(jq -r '.["io.buildpacks.builder.metadata"] | fromjson | .lifecycle.version' "$OUT/builder-labels.json")
  echo "nodejs $ver, lifecycle $life"
  a=$(date +%s); r=$(rx); u=$(used)
  docker build -q -t canyon-build:trial - <executor/build.Dockerfile
  printf '[[buildpacks]]\nuri = "docker://%s"\n\n[[order]]\n[[order.group]]\nid = "paketo-buildpacks/nodejs"\nversion = "%s"\n\n[lifecycle]\nversion = "%s"\n\n[stack]\nid = "io.buildpacks.stacks.noble"\nbuild-image = "canyon-build:trial"\nrun-image = "canyon-run:noble"\n' "$node" "$ver" "$life" >"$RUNNER_TEMP/builder.toml"
  /usr/local/bin/pack builder create canyon-builder-nodejs:trial --config "$RUNNER_TEMP/builder.toml" --pull-policy if-not-present 2>&1 | tail -20 | tee "$OUT/own-builder.txt"
  printf '30 own builder made\t%s\t%s\t%s\n' "$(($(date +%s) - a))" "$(($(rx) - r))" "$(($(used) - u))" | tee -a "$OUT/stages.tsv"
  go test -tags e2e -count=1 -timeout 25m -run '^TestSizeTrialOwnBuilder$' -v ./executor 2>&1 | tee "$OUT/own-builder-build.txt"
fi
snap 99-end
cat "$OUT/stages.tsv"
exit 0
