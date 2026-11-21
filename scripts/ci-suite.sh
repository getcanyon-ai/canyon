#!/usr/bin/env bash
# scripts/ci-suite.sh [--setup] <suite>: one stack suite's own steps, as a pull request runs them, in one place for
# the two that run it: the public workflows (e2e-ui.yml's journeys and probes jobs, kube.yml's kind, canyon and
# handoff-repo jobs), and the trusted private-ci.yml of Canyon's private CI repository (CONTRIBUTING.md, Maintainers),
# which may run nothing of a pull request's .github but its scripts. The suites:
#   journeys-<1..8>   a shard of the browser journeys (make e2e-ui), on the compose stack
#   probes            the probe apps never read clean (TestProbesNeverReadClean), on the compose stack
#   kind-calico       the kind suite on Calico, Envoy's lost access-log entries, and the hand-off journey
#   kind-kindnet      the probe on a cluster that enforces nothing, and run_by canyon there
#   kind-canyon       run_by canyon on Calico: a deploy through the server builds and runs
#   handoff-argocd    hand-off through a deploy repository, under Argo CD
#   handoff-flux      the same, under Flux
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
tmp=${RUNNER_TEMP:-$(mktemp -d)}
compose=(docker compose -f docker-compose.yml -f e2e/compose.e2e.yml)
dbs="$HOME/.cache/canyon-scandb"

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
# receiver or its stats name no such counter (#2377); Envoy's image has no curl, so an ephemeral container of the
# router's image reads the admin socket from the pod's admin volume
envoy_lost() {
  local router pods p stats lost=0
  router=$(kubectl -n canyon get deploy canyon-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="nginx")].image}') || return
  pods=$(kubectl -n canyon get pods -l app.kubernetes.io/name=canyon-egress -o name) || return
  [ -n "$pods" ] || { echo "::error::no Envoy pod to read"; return 1; }
  for p in $pods; do
    kubectl -n canyon patch "$p" --subresource=ephemeralcontainers --type=strategic -p '{"spec":{"ephemeralContainers":[{"name":"stats","image":"'"$router"'","command":["curl","-s","--unix-socket","/var/run/envoy-admin/admin.sock","http://x/stats?filter=access_log|ext_authz|receiver"],"volumeMounts":[{"name":"admin","mountPath":"/var/run/envoy-admin"}],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'
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

mkdir -p "$HOME/.local/bin"
case $suite in
  journeys-[1-8]) chromium && stack journeys "${suite#journeys-}" ;;
  probes) stack probes ;;
  kind-calico) kind kind_calico ;;
  kind-kindnet) kind kind_kindnet ;;
  kind-canyon) kind kind_canyon ;;
  handoff-argocd | handoff-flux) kind handoff "${suite#handoff-}" ;;
  *) echo "ci-suite.sh: no suite named $suite (the list is at the top of this file)" >&2; exit 64 ;;
esac
