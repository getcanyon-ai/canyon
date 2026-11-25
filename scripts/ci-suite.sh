#!/usr/bin/env bash
# scripts/ci-suite.sh [--setup] <suite>: one suite's own steps, as a pull request runs them, in one place for the two
# that run it: the public workflows (e2e-ui.yml's journeys and probes jobs, kube.yml's kind, canyon and handoff-repo
# jobs), and the trusted private-ci.yml of Canyon's private CI repository (CONTRIBUTING.md, Maintainers), which may run
# nothing of a pull request's .github but its scripts. The stack suites:
#   journeys-<1..8>   a shard of the browser journeys (make e2e-ui), on the compose stack
#   probes            the probe apps never read clean (TestProbesNeverReadClean), on the compose stack
#   kind-calico       the kind suite on Calico, Envoy's lost access-log entries, and the hand-off journey
#   kind-kindnet      the probe on a cluster that enforces nothing, and run_by canyon there
#   kind-canyon       run_by canyon on Calico: a deploy through the server builds and runs
#   handoff-argocd    hand-off through a deploy repository, under Argo CD
#   handoff-flux      the same, under Flux
#   egress-floor      e2e-ui.yml's egress job, its floor part: the egress floor, Envoy and its check
#   egress-layer3     the same, its layer-3 part: interception and a route's credential at its upstream
#   oidc              e2e-ui.yml's oidc job: the team setup's sign-in, deploy and sharing
#   patch             patch-e2e.yml: a rebase patch and its revert, on the team setup
#   tour              tour.yml: the docs' tour on the laptop demo
#   backup            backup.yml: one k3s host backed up and restored, on kind
#   build-arch        build-arch.yml's arm64 job: a real build and /run through the executor, on kind
# and the rest of a pull request's checks, the jobs GitHub runs on its own runners, for when it can't (the owner,
# 2026-11-25: Actions turned off on the repository). Each runs its job's own make targets and scripts:
#   test-rest         ci.yml's test-rest: every package but the sharded ones, the installer, govulncheck
#   server-<1..4>     ci.yml's server-shard: the server's tests, a quarter each (ci.yml's matrix: four)
#   noee-rest         ci.yml's noee-rest: built and tested without Canyon Enterprise
#   noee-server-<1..4>  ci.yml's noee-server-shard
#   runs-<1..3>       ci.yml's runs-shard (ci.yml's matrix: three)
#   provisioners-<14..18>  ci.yml's provisioners, on that Postgres
#   lint              ci.yml's lint: golangci-lint, the design system, the contracts, the vendored rules
#   tidy              ci.yml's tidy: go.mod and go.sum are tidy
#   web               ci.yml's web: the dashboard's tests and build, e2e/ui type-checked
#   compose-up        ci.yml's compose-up: Envoy's bootstrap validates, and make demo-check
#   images            ci.yml's images: the ten release images build, each carrying what it must
#   deploy-bundle     ci.yml's deploy-bundle: canyon-deploy.tar.gz starts with no source, and Opengrep's source builds
#   helm-chart        helm.yml's chart job (.github/helm/chart-check.sh)
#   docs              docs.yml: the links, and the site built and checked (.github/docs/site-check.sh)
#   gitleaks          gitleaks.yml: the secret scan over the pull request's own commits
# Not here: helm.yml's install and old-cli.yml (both read Canyon's releases with a GitHub token, and these runs carry
# none), executor-e2e and build-arch's amd64 job (amd64), fleet-running (a timing on demand), and the CLA check.
# The caller has checked the tree out and set up Go and Node (the public workflows' cached actions, the private
# workflow's SHA-pinned ones). --setup is for a caller with no cache and none of the repository's actions: before the
# suite it seeds the compose stack's scanner databases with .github/actions/scan-dbs/dbs.sh, and after it checks them,
# as the scan-dbs action does around a public job. Without --setup the caller does that. On a failure it prints what
# the stack or the cluster was doing; kind's pinned tools land in ~/.local/bin, which it puts on PATH. Each step below
# ends its suite with `|| return`: a function run where its status is tested runs with set -e off.
set -euo pipefail
cd "$(dirname "$0")/.."
setup=false
if [ "${1:-}" = --setup ]; then setup=true; shift; fi
suite=${1:?usage: scripts/ci-suite.sh [--setup] <suite>}
export PATH="$HOME/.local/bin:$PATH"
# a module download the proxy cuts short is asked again, as go-cache has the public jobs' Go commands do (#2533): no action
# sets it for the private runners, and a cut-short download in an image's build failed #197's backup suite
[ -n "${GOPROXY:-}" ] || GOPROXY=$(.github/actions/go-cache/goproxy.sh https://proxy.golang.org)
export GOPROXY
echo "ci-suite.sh: GOPROXY=$GOPROXY, for the Go commands and every image this suite builds" # (#180)
tmp=${RUNNER_TEMP:-$(mktemp -d)}
compose=(docker compose -f docker-compose.yml -f e2e/compose.e2e.yml)
dbs="$HOME/.cache/canyon-scandb"

# pack_cli is the pinned pack CLI in ~/.local/bin, checked against its sha256 (make pack-fetch), which makes
# canyon-builder-node for the images, deploy-bundle and tour suites, as their public jobs' make pack-install does
pack_cli() {
  local tgz
  tgz=$(mktemp -d)/pack.tgz
  make --no-print-directory pack-fetch PACK_TGZ="$tgz" && tar -xzf "$tgz" -C "$HOME/.local/bin" pack
}

# chromium is the journeys' browser and the hand-off journeys' set-up in the dashboard, with its system libraries
chromium() { (cd e2e/ui && npm ci && npx playwright install --with-deps chromium); }

# stack wraps a compose-stack suite: the scanner databases seeded and checked under --setup, and the stack's logs on a failure
stack() {
  local rc=0
  if [ "$setup" = true ]; then .github/actions/scan-dbs/dbs.sh seed "$dbs" "$(date -u +%F)" || return; fi
  "$@" || rc=$?
  if [ "$setup" = true ]; then .github/actions/scan-dbs/dbs.sh check "$dbs" || rc=1; fi
  if [ "$rc" != 0 ]; then "${compose[@]}" logs --no-color --tail=200 server executor egress egress-envoy egress-control || true; fi
  return "$rc"
}

journeys() {
  make e2e-up || return
  make e2e-ui SHARD="$1/8" || return
  jq -e '.stats.expected > 0' e2e/ui/.work/run.json || return # with --shard, Playwright passes a shard that found none
  test -z "$(docker volume ls -q --filter 'name=^canyon-cache-ux-')" # no journey's build cache is left behind (harness/teardown.ts)
}

probes() {
  make e2e-up || return
  go test -tags stack -count=1 -run TestProbesNeverReadClean -v ./e2e/
}

# helm is the pinned release, checksum-verified as helm.yml's
helm_pinned() {
  local version=v4.0.5 sum=206a7747702d13994a93629eaed4259bd9d0aec6e23ca52d640f47f7edfdc863 # linux-arm64 of version; bump both together
  .github/fetch.sh "https://get.helm.sh/helm-$version-linux-arm64.tar.gz" "$sum" "$tmp/helm.tar.gz" || return
  tar -xzf "$tmp/helm.tar.gz" --strip-components=1 -C "$HOME/.local/bin" linux-arm64/helm
}

# handoff_db is the hand-off tests' Postgres, on the host
handoff_db() {
  docker run -d --rm --name handoff-db -p 127.0.0.1:5432:5432 -e POSTGRES_PASSWORD=canyon postgres:16-alpine || return
  until docker exec handoff-db pg_isready -h 127.0.0.1 -U postgres >/dev/null; do sleep 1; done
  export HANDOFF_DATABASE_URL=postgres://postgres:canyon@127.0.0.1:5432/postgres?sslmode=disable
}

# run_by_canyon runs TestRunByCanyonOnKind and fails when it didn't run: a test renamed away runs nothing and passes
run_by_canyon() {
  go test -tags kindcanyon -count=1 -v -timeout "$1" -run '^TestRunByCanyonOnKind$' ./e2e/handoff | tee "$tmp/canyon.txt" || return
  grep -q -- '--- PASS: TestRunByCanyonOnKind' "$tmp/canyon.txt"
}

# cluster prints what the cluster was doing, on a failure
cluster() {
  df -h / || true
  kubectl get pods -A -o wide || true
  kubectl get events -A --sort-by=.lastTimestamp | tail -60 || true
}

# envoy_lost: each Envoy replica's access-log and receiver stats, and red when one lost an entry on its way to the
# receiver or its stats name no such counter (#2377), beside the plain cluster's upstream counters and the egress
# listener's downstream ones, which say how a call that reached Envoy ended there (#143); Envoy's image has no curl, so
# an ephemeral container of the router's image reads the admin socket from the pod's admin volume
envoy_lost() {
  local router pods p stats lost=0
  router=$(kubectl -n canyon get deploy canyon-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="nginx")].image}') || return
  pods=$(kubectl -n canyon get pods -l app.kubernetes.io/name=canyon-egress -o name) || return
  [ -n "$pods" ] || { echo "::error::no Envoy pod to read"; return 1; }
  for p in $pods; do
    kubectl -n canyon patch "$p" --subresource=ephemeralcontainers --type=strategic -p '{"spec":{"ephemeralContainers":[{"name":"stats","image":"'"$router"'","command":["curl","-s","--unix-socket","/var/run/envoy-admin/admin.sock","http://x/stats?filter=access_log|ext_authz|receiver|cluster.plain.upstream_|http.egress.downstream_"],"volumeMounts":[{"name":"admin","mountPath":"/var/run/envoy-admin"}],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'
    for _ in $(seq 30); do kubectl -n canyon logs "$p" -c stats 2>/dev/null | grep -q . && break; sleep 2; done
    stats=$(kubectl -n canyon logs "$p" -c stats) || return
    echo "== $p"; echo "$stats"
    if ! echo "$stats" | awk -F': ' '
      $1 == "cluster.receiver.upstream_rq_pending_failure_eject" { eject = 1; if ($2 != "0") bad = 1 }
      $1 ~ /^access_logs\.[^.]+\.logs_dropped$/ { dropped = 1; if ($2 != "0") bad = 1 }
      END { exit !(eject && dropped && !bad) }'; then
      echo "::error::$p lost access-log entries on their way to the receiver, or its stats name no such counter: upstream_rq_pending_failure_eject and logs_dropped must be 0"
      lost=1
    fi
    if kubectl -n canyon logs "$p" -c egress-control | grep 'what Envoy lost is not all reported'; then
      echo "::error::$p: egress-control could not read Envoy's stats, or report them"
      lost=1
    fi
  done
  return "$lost"
}

# kind_suite is make test-kube on the cluster up.sh made: its own 35 minutes, past go test's 30, so a hang ends here and
# says what was still running
kind_suite() {
  local rc=0
  CANYON_KUBE_TEST_PUSH=localhost:5001 CANYON_KUBE_TEST_DB=172.30.0.201 CANYON_KUBE_TEST_SIGNIN=172.30.0.203 \
    CANYON_KUBE_TEST_NETWORK_POLICY="$2" CANYON_KUBE_TEST_LEDGER=kind-pg \
    timeout 35m make test-kube RUN="$1" || rc=$?
  if [ "$rc" != 0 ]; then
    ps -eo pid,ppid,etime,args --forest | grep -E 'go |kube|make|kubectl|helm|docker' | grep -v grep | tail -60 || true
    cluster
    for p in $(kubectl -n canyon-apps get pods -o name 2>/dev/null); do # each app's forwarder still running (#143)
      echo "== $p canyon-identity"; kubectl -n canyon-apps logs "$p" -c canyon-identity --tail=100 || true
    done
  fi
  return "$rc"
}

kind_calico() {
  local rc=0
  .github/kind/up.sh calico || return
  helm_pinned || return # TestKindSigninRuleFromTheChart applies the chart's canyon-apps-signin
  kind_suite TestKind enforced || rc=$?
  envoy_lost || rc=1 # whether the suite passed or not: a check of its own
  [ "$rc" = 0 ] || return "$rc"
  chromium || return
  handoff_db || return
  make e2e-handoff
}

kind_kindnet() {
  .github/kind/up.sh kindnet || return
  kind_suite TestKindProbeTellsEnforcement not_enforced || return
  handoff_db || return # under run_by canyon a deploy builds nothing, and the chart's bootstrap fails closed without failing the upgrade (#2514, #2516)
  CANYON_KUBE_TEST_NETWORK_POLICY=not_enforced run_by_canyon 20m
}

kind_canyon() {
  .github/kind/up.sh calico || return
  .github/kind/build-arch.sh || return
  handoff_db || return
  CANYON_KUBE_TEST_NETWORK_POLICY=enforced run_by_canyon 30m
}

handoff() {
  local gitops=$1 rc=0
  .github/kind/up.sh calico || return
  helm_pinned || return
  .github/kind/platform.sh "$gitops" || return
  chromium || return
  handoff_db || return
  HANDOFF_GITOPS=$gitops make e2e-handoff-repo || rc=$?
  .github/kind/gitops-check.sh "$gitops" || rc=1 # whether the journey passed or not: a check of its own
  if [ "$rc" != 0 ] && [ "$gitops" = flux ]; then # what Flux made of the deploy repo
    kubectl -n flux-system get gitrepositories,artifactgenerators,externalartifacts,helmreleases -o yaml || true
    kubectl -n canyon-sync get gitrepositories,kustomizations,resourcequotas -o yaml || true
    kubectl -n canyon-namespaces get secrets || true
    for d in source-controller source-watcher helm-controller kustomize-controller; do
      echo "== $d"; kubectl -n flux-system logs deploy/$d --tail=150 || true
    done
  fi
  return "$rc"
}

# kind wraps a kind suite: on a failure, what the cluster and Canyon's pods were doing
kind() {
  local rc=0
  "$@" || rc=$?
  if [ "$rc" != 0 ]; then
    cluster
    for d in kpack-webhook kpack-controller; do
      kubectl -n kpack describe pods -l app=$d || true
      kubectl -n kpack logs deploy/$d --previous --tail=100 || kubectl -n kpack logs deploy/$d --tail=100 || true
    done
    kubectl -n canyon-builds get builds.kpack.io -o wide || true
    kubectl -n canyon logs deploy/canyon-executor --tail=300 || true
    kubectl -n canyon logs deploy/canyon-server --tail=100 || true
    kubectl -n canyon logs -l app.kubernetes.io/name=canyon-egress --all-containers --prefix --tail=50 || true
  fi
  return "$rc"
}

# pg is ci.yml's test database, on the host: Postgres $1 (16 by default), its data in memory, CANYON_TEST_DATABASE_URL
# set as ci.yml sets it
pg() {
  local v=${1:-16} data=/var/lib/postgresql/data
  if [ "$v" -ge 18 ]; then data=/var/lib/postgresql; fi # 18's image keeps its data under /var/lib/postgresql/18 (ci.yml's provisioners)
  docker run -d --rm --name ci-pg -p 127.0.0.1:5432:5432 --tmpfs "$data" -e POSTGRES_PASSWORD=canyon "postgres:$v-alpine" >/dev/null || return
  for _ in $(seq 60); do docker exec ci-pg pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break; sleep 1; done
  docker exec ci-pg pg_isready -h 127.0.0.1 -U postgres || return # ready within a minute, or the suite fails here
  export CANYON_TEST_DATABASE_URL='postgres://postgres:canyon@localhost:5432/postgres?sslmode=disable'
}

test_rest() {
  pg || return
  make web || return
  make third-party-notices-check || return
  make test-rest || return
  make test-bounds || return
  make install-test || return
  make test-cluster-unit || return
  go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
}

# noee is Canyon without Canyon Enterprise, as ci.yml's noee jobs build it: ee/ gone, every Go command under -tags noee
noee() {
  rm -rf ee || return
  export GOFLAGS=-tags=noee
}

# shard runs ci.yml's test-shards on package $1, shard $2 of $3, after the dashboard bundle the server's tests read
shard() {
  pg || return
  if [ "$1" = ./cmd/canyon-server ]; then make web || return; fi
  make test-shards SHARD_PKG="$1" SHARD="$2/$3"
}

provisioners() {
  pg "$1" || return
  make test PKG=./provisioners || return
  make test PKG=./provisioners RUN='^TestAnIssueRacingADeleteLeavesNoRole$$' COUNT=20 # the race of #2380, 20 times on each Postgres
}

lint() {
  local v sum=a2a4e0065aa41be71f7c5ac90f271b61751331e5d04314e62afe4027855f0893 # linux-arm64, as ci.yml's lint job pins it: bump both together
  v=$(sed -n 's/^GOLANGCI_LINT_VERSION ?= v//p' Makefile)
  .github/fetch.sh "https://github.com/golangci/golangci-lint/releases/download/v$v/golangci-lint-$v-linux-arm64.tar.gz" "$sum" "$tmp/golangci-lint.tar.gz" || return
  tar -xzf "$tmp/golangci-lint.tar.gz" --strip-components=1 -C "$(go env GOPATH)/bin" "golangci-lint-$v-linux-arm64/golangci-lint" || return
  make lint || return
  python3 designsystem/canyon/validate.py || return
  make tokens-check || return
  python3 -m pip install --break-system-packages jsonschema==4.23.0 || return # a throwaway VM, as ci.yml's
  python3 contracts/validate.py || return
  make opengrep-rules-test || return
  make vendor-sast-rules || return
  git diff --exit-code gate/opengrep/sast-rules.tar
}

dashboard() {
  (cd web && npm ci && npm test && npm run build) || return
  make e2e-ui-typecheck
}

# compose_up is ci.yml's compose-up: the pinned Envoy validates its bootstrap, filled in by deploy/egress/envoy.sh from
# egress-envoy's own environment, which holds the token and is gone when the check ends; then the boot path
compose_up() {
  local config rc=0
  config=$(docker compose config --format json) || return
  jq -r '.services["egress-envoy"].environment | to_entries[] | "\(.key)=\(.value)"' <<<"$config" > "$tmp/envoy.env" || return
  docker run --rm --env-file "$tmp/envoy.env" --tmpfs /var/run/envoy -v "$PWD/deploy/egress:/etc/canyon:ro" \
    --entrypoint /bin/sh "$(jq -r '.services["egress-envoy"].image' <<<"$config")" /etc/canyon/envoy.sh --mode validate || rc=$?
  rm -f "$tmp/envoy.env"
  [ "$rc" = 0 ] || return "$rc"
  make demo-check
}

# opengrep_source is ci.yml's deploy-bundle check of Opengrep's complete source, as scripts/publish-release.sh attaches it
opengrep_source() {
  local f
  mkdir -p "$tmp/source" || return
  scripts/opengrep-source.sh "$tmp/source/opengrep-1.30.0-source.tar.gz" || return
  scripts/release-archives-check.sh "$tmp/source" || return # clean of the runner's owners and metadata, as publish-release.sh checks it
  tar -tzf "$tmp/source/opengrep-1.30.0-source.tar.gz" > "$tmp/opengrep-source.txt" || return
  for f in LICENSE libs/commons/license.txt languages/go/tree-sitter/semgrep-go/README.md libs/ocaml-tree-sitter-core/LICENSE; do
    grep -qx "opengrep-1.30.0/$f" "$tmp/opengrep-source.txt" || { echo "::error::the source tarball has no opengrep-1.30.0/$f"; return 1; }
  done
  ! grep -q '/\.git\(/\|$\)' "$tmp/opengrep-source.txt" || { echo "::error::the source tarball carries .git"; return 1; }
}

docs() {
  local version=v0.24.2 sum=91a7bd65685da41b90ccb9bc867a3d649a7818042dae04ff405e55a25bddee4c # lychee, as docs.yml pins it: bump both together
  .github/fetch.sh "https://github.com/lycheeverse/lychee/releases/download/lychee-$version/lychee-aarch64-unknown-linux-gnu.tar.gz" "$sum" "$tmp/lychee.tar.gz" || return
  tar -xzf "$tmp/lychee.tar.gz" --strip-components=1 -C "$HOME/.local/bin" lychee-aarch64-unknown-linux-gnu/lychee || return
  make docs-links || return
  # names are main's build's to check: these runs hold no secret, as a fork's pull request holds none (docs/scripts/public-check.mjs)
  CANYON_PUBLIC_CHECK_FORK=true make docs-site || return
  CANYON_PUBLIC_CHECK_FORK=true .github/docs/site-check.sh
}

# gitleaks_scan is gitleaks.yml's: the pinned release over a pull request's own commits, main..HEAD, so a secret on
# another branch never fails this one; over the whole history for a commit already on main (private-ci's pr main), as
# gitleaks.yml's push leg scans main, since main..HEAD would scan nothing. A range of no commit fails: a scan of nothing
# passes nothing. A shallow checkout fetches the history it needs first.
gitleaks_scan() {
  local version=8.30.1 sum=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080 # as gitleaks.yml pins it: bump both together
  local unshallow=()
  .github/fetch.sh "https://github.com/gitleaks/gitleaks/releases/download/v$version/gitleaks_${version}_linux_arm64.tar.gz" "$sum" "$tmp/gitleaks.tar.gz" || return
  tar -xzf "$tmp/gitleaks.tar.gz" -C "$tmp" gitleaks || return
  if [ "$(git rev-parse --is-shallow-repository)" = true ]; then unshallow=(--unshallow); fi
  git fetch -q --no-tags "${unshallow[@]}" https://github.com/getcanyon-ai/canyon.git "+refs/heads/main:refs/ci-suite/main" "$(git rev-parse HEAD)" || return
  local range=refs/ci-suite/main..HEAD
  if git merge-base --is-ancestor HEAD refs/ci-suite/main; then range=HEAD
  elif [ -z "$(git rev-list -n 1 "$range")" ]; then echo "::error::gitleaks: $range lists no commit to scan" >&2; return 1
  fi
  "$tmp/gitleaks" detect --source . --redact --no-banner --exit-code 1 --log-opts="$range"
}

# listed runs go test on what -list names, and fails unless every test listed ran and passed, so one renamed away
# never passes: e2e-ui.yml's egress and oidc jobs and patch-e2e.yml check it so. $1 the tags, $2 the -run pattern,
# $3 the package, $4 the timeout; with $5 skip, a test or subtest skipped fails it too (the egress job's).
listed() {
  local want passed skipped='' rc=0
  want=$(go test -tags "$1" -list "$2" "$3" | grep '^Test' | sort) || return
  go test -tags "$1" -count=1 -timeout "$4" -json -run "$2" "$3" > "$tmp/listed.json" || rc=$?
  jq -j 'select(.Output) | .Output' "$tmp/listed.json"
  passed=$(jq -r 'select(.Action == "pass" and .Test and (.Test | contains("/") | not)) | .Test' "$tmp/listed.json" | sort)
  if [ "${5:-}" = skip ]; then skipped=$(jq -r 'select(.Action == "skip" and .Test) | .Test' "$tmp/listed.json" | sort); fi
  if [ "$rc" != 0 ] || [ -z "$want" ] || [ "$want" != "$passed" ] || [ -n "$skipped" ]; then
    printf 'go test exited %s\nlisted:\n%s\npassed:\n%s\nskipped:\n%s\n' "$rc" "$want" "$passed" "$skipped"; return 1
  fi
}

egress() {
  # a public address of the Docker host's own, on its loopback, before the stack lists the host's addresses (e2e/egress_test.go, TestEnvoyRefusesTheHostsOwnAddress; #2524)
  docker run --rm --network host --cap-add NET_ADMIN busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e ip addr add 198.51.100.200/32 dev lo || return
  make e2e-up || return
  listed stack "^Test($1)" ./e2e/ 30m skip
}

# oidc_suite is the team setup's stack, with e2e/ui's projects that run as each person, and its tests: $1 the tags, $2 the
# -run pattern, $3 the timeout
oidc_suite() {
  chromium || return
  make e2e-up OIDC=1 || return
  listed "$1" "$2" ./e2e/oidc/ "$3"
}

kind_backup() {
  .github/kind/up.sh calico || return
  .github/kind/backup.sh || return
  .github/kind/gateway.sh
}

kind_build_arch() {
  .github/kind/up.sh calico || return
  .github/kind/build-arch.sh || return
  CANYON_KUBE_TEST_PUSH=localhost:5001 listed kube '^(TestBuildOnKindRunsWhereItsAppRuns|TestAnImageOfTheOtherArchitectureWaitsForItsOwnNodes)$' ./executor/kube 45m
}

helm_chart() {
  helm_pinned || return
  .github/helm/chart-check.sh
}

mkdir -p "$HOME/.local/bin"
case $suite in
  journeys-[1-8]) chromium && stack journeys "${suite#journeys-}" ;;
  probes) stack probes ;;
  kind-calico) kind kind_calico ;;
  kind-kindnet) kind kind_kindnet ;;
  kind-canyon) kind kind_canyon ;;
  handoff-argocd | handoff-flux) kind handoff "${suite#handoff-}" ;;
  egress-floor) stack egress 'Connect|Floor|Envoy|Control' ;;
  egress-layer3) stack egress 'Interception$|ForgedInterceptHeaderRejected$|ARouteCredentialReachesItsUpstream$|ForcedHTTP2CarriesTheRouteCredential$|GRPCCarriesTheRouteCredential$|PassThroughDestinationIsNeverDecrypted$' ;;
  oidc) stack oidc_suite oidc . 30m ;;
  patch) stack oidc_suite oidc,patch '^TestRebasePatch' 65m ;;
  tour) pack_cli && make tour-check ;;
  backup) kind kind_backup ;;
  build-arch) kind kind_build_arch ;;
  test-rest) test_rest ;;
  server-[1-4]) shard ./cmd/canyon-server "${suite#server-}" 4 ;;
  noee-rest) noee && pg && make web && go build ./... && go vet ./... && make test-rest ;;
  noee-server-[1-4]) noee && shard ./cmd/canyon-server "${suite#noee-server-}" 4 ;;
  runs-[1-3]) shard ./runs "${suite#runs-}" 3 ;;
  provisioners-1[4-8]) provisioners "${suite#provisioners-}" ;;
  lint) lint ;;
  tidy) make tidy-check ;;
  web) dashboard ;;
  compose-up) compose_up ;;
  images) pack_cli && make images-load ;;
  deploy-bundle) pack_cli && make deploy-bundle-check && opengrep_source ;;
  helm-chart) helm_chart ;;
  docs) docs ;;
  gitleaks) gitleaks_scan ;;
  *) echo "ci-suite.sh: no suite named $suite (the list is at the top of this file)" >&2; exit 64 ;;
esac
