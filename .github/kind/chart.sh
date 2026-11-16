#!/usr/bin/env bash
# The Helm chart on kind, with its bundled parts running (#2778; helm.yml's install job). The release before's chart
# is installed with its own images, data is written into each bundled volume, then this tree's chart upgrades it:
#
#   - every namespace enforces the Pod Security level the chart means it to, and every pod of every Deployment was
#     admitted under it; the bundled parts (both Postgres servers, the registry, Pocket ID, Caddy, kpack) are
#     Available, on the install and after the upgrade;
#   - the volume-owned guard refuses a volume holding root's files and the part starts once backup-and-upgrade.md's
#     chown has run: on the upgrade from a v0.8 release, which ran the registry and Caddy as root, and again with one
#     root-owned file planted in each volume, so later releases keep proving it;
#   - the bootstrap Job, which signs in through Caddy with HTTP-01 certificates kind cannot have, is not run:
#     Pod Security judges it by a server-side dry run (deploy/helm/bootstrap_test.py covers its sign-in).
#
# Canyon's server waits on its identity provider's discovery, which kind cannot serve either, so neither it nor the
# egress gateway waiting on it becomes ready here: their pods must be admitted, no more. CI only: never on a laptop.
set -euo pipefail

root=$(git rev-parse --show-toplevel) work=$(mktemp -d) bin=$HOME/.local/bin
mkdir -p "$bin" && export PATH=$bin:$PATH
fetch() { "$root/.github/fetch.sh" "$@"; }
# kind, its node and kubectl as up.sh's calico cluster pins them: bump both together
case $(uname -m) in
  x86_64) arch=amd64 kind_sum=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d kubectl_sum=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3 ;;
  aarch64|arm64) arch=arm64 kind_sum=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 kubectl_sum=420e6110e3ba7ee5a3927b5af868d18df17aae36b720529ffa4e9e945aa95450 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
node=kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d
fetch "https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-$arch" $kind_sum "$bin/kind"
fetch https://dl.k8s.io/release/v1.34.1/bin/linux/$arch/kubectl $kubectl_sum "$bin/kubectl"
chmod +x "$bin/kind" "$bin/kubectl"

fail() { echo "::error::$*"; exit 1; }

# The release before: its chart and its images, from canyon-releases, as an operator upgrading has them.
prev=$(gh release view -R getcanyon-ai/canyon-releases --json tagName -q .tagName)
gh release download "$prev" -R getcanyon-ai/canyon-releases -p canyon-chart.tgz -p images.txt -D "$work"
image() { grep "/canyon-$1@" "$work/images.txt" | head -n 1; }
echo "the release before: $prev"

kind delete cluster >/dev/null 2>&1 || true
kind create cluster --image $node --wait 5m
kubectl wait --for=condition=Ready node --all --timeout=5m

# This cluster's values: the release's images for Canyon's own, the pod network and the API server kind has, every
# bundled part on, the bootstrap off.
api=$(kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}')
svc=$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}')
values=$work/values.yaml
{
  echo "global: {domain: canyon.kind.test, podCIDR: 10.244.0.0/16}"
  echo "images:"
  for k in server:server executorKube:executor-kube identity:identity run:run build:build; do
    ref=$(image "${k#*:}")
    [ -n "$ref" ] || fail "$prev's images.txt names no canyon-${k#*:}"
    echo "  ${k%%:*}: {repository: ${ref%@*}, digest: \"${ref#*@}\"}"
  done
  echo "canyon:"
  echo "  auth: {admins: [admin@canyon.kind.test]}"
  echo "  kubeAPI: {cidrs: [$api/32, $svc/32]}"
  echo "auth: {pocketId: {users: [{username: admin, email: admin@canyon.kind.test, firstName: Ada, lastName: Admin}]}}"
  echo "infra: {registry: {clusterIP: 10.96.254.10}}" # kind's Service range, where the chart's k3s default is not
  echo "bootstrap: {enabled: false}"
  # CI's environment never reaches the server: Canyon's own clusters send no report
  echo "telemetry: {enabled: false}"
} >"$values"

# Each namespace's Pod Security level, as the chart means it (templates/namespaces.yaml): canyon-apps baseline, as an
# app runs as whatever user its image names, every other restricted.
levels() {
  for ns in canyon canyon-builds canyon-scans canyon-infra canyon-auth canyon-gateway canyon-apps; do
    want=restricted
    [ $ns = canyon-apps ] && want=baseline
    got=$(kubectl get ns $ns -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')
    [ "$got" = $want ] || fail "namespace $ns enforces Pod Security '$got', want $want"
  done
}

# Every pod of every Deployment of the install was admitted: a ReplicaSet refused one says ReplicaFailure, its reason
# Pod Security's. Then the bundled parts are Available.
bundled="canyon-infra/registry canyon-infra/canyon-postgres canyon-infra/app-postgres canyon-auth/pocket-id canyon-gateway/caddy kpack/kpack-controller kpack/kpack-webhook"
admitted_and_up() {
  for d in $bundled; do
    kubectl -n "${d%/*}" rollout status "deploy/${d#*/}" --timeout=10m || {
      kubectl -n "${d%/*}" describe "deploy/${d#*/}" | tail -n 30
      kubectl get events -A --field-selector reason=FailedCreate | tail -n 20
      fail "$d never became Available"
    }
  done
  refused=$(kubectl get rs -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{range .status.conditions[?(@.type=="ReplicaFailure")]}{.message}{end}{"\n"}{end}' | awk 'NF > 1')
  [ -z "$refused" ] || fail "pods were refused: $refused"
  # any controller's pod Pod Security refused, a Job's or a DaemonSet's as well as a ReplicaSet's
  refused=$(kubectl get events -A --field-selector reason=FailedCreate -o jsonpath='{range .items[*]}{.involvedObject.kind}/{.involvedObject.namespace}/{.involvedObject.name}: {.message}{"\n"}{end}' | grep -F "violates PodSecurity" || true)
  [ -z "$refused" ] || fail "Pod Security refused a pod: $refused"
}

# newest is the deployment's newest pod: the one a rollout starts, never the one it replaces.
newest() { # namespace deployment
  kubectl -n "$1" get pods -l "app=$2" --sort-by=.metadata.creationTimestamp -o name | tail -n 1
}

# The volume-owned guard (templates/_helpers.tpl, canyon.ownedOrExit) refuses part's volume: its new pod's init
# container exits saying so, and the part never starts.
refuses() { # namespace deployment dir
  local ns=$1 d=$2 dir=$3 log=""
  for _ in $(seq 1 60); do
    log=$(kubectl -n "$ns" logs "$(newest "$ns" "$d")" -c volume-owned 2>/dev/null || true)
    case $log in *"canyon: user 65532 can't write $dir"*) echo "$ns/$d refuses: $log"; return ;; esac
    sleep 5
  done
  kubectl -n "$ns" get pods
  fail "$ns/$d's volume-owned check never refused its volume of root's files: '$log'"
}

# backup-and-upgrade.md's fix, as the page writes it: the namespace at baseline for one pod, the volume chowned with
# the part's own image, the level restored, the part scaled back.
chown_volume() { # namespace deployment claim dir
  local ns=$1 d=$2 claim=$3 dir=$4 image
  image=$(kubectl -n "$ns" get deploy "$d" -o jsonpath='{.spec.template.spec.containers[0].image}')
  kubectl -n "$ns" scale deploy "$d" --replicas=0
  kubectl -n "$ns" wait --for=delete pod -l "app=$d" --timeout=3m || [ -z "$(kubectl -n "$ns" get pods -l "app=$d" -o name)" ] ||
    fail "$ns/$d's pods were still there 3 minutes after it was scaled to 0: nothing chowns a volume a pod still holds"
  kubectl label ns "$ns" pod-security.kubernetes.io/enforce=baseline --overwrite
  kubectl -n "$ns" run chown --rm -i --restart=Never --image="$image" --overrides='{"spec":{"containers":[{"name":"chown","image":"'"$image"'","command":["chown","-R","65532:65532","/v"],"volumeMounts":[{"name":"v","mountPath":"/v"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"'"$claim"'"}}]}}'
  kubectl label ns "$ns" pod-security.kubernetes.io/enforce=restricted --overwrite
  kubectl -n "$ns" scale deploy "$d" --replicas=1
  kubectl -n "$ns" rollout status "deploy/$d" --timeout=5m || fail "$ns/$d did not start after its volume was chowned to 65532"
}

# markers says both volumes still hold what was written into them before the upgrade, read by the parts' own users.
markers() {
  [ "$(kubectl -n canyon-infra exec deploy/registry -- cat /var/lib/registry/docker/registry/v2/repositories/ci/marker)" = ci ] ||
    fail "the registry's volume lost its data, or its user cannot read it"
  [ "$(kubectl -n canyon-gateway exec deploy/caddy -- cat /data/caddy/ci/marker)" = ci ] ||
    fail "Caddy's volume lost its data, or its user cannot read it"
}

# One root-owned file in the volume, written in a one-pod baseline window by the part's own image as root.
plant_root_file() { # namespace deployment claim
  local ns=$1 d=$2 claim=$3 image
  image=$(kubectl -n "$ns" get deploy "$d" -o jsonpath='{.spec.template.spec.containers[0].image}')
  kubectl label ns "$ns" pod-security.kubernetes.io/enforce=baseline --overwrite
  kubectl -n "$ns" run plant --rm -i --restart=Never --image="$image" --overrides='{"spec":{"securityContext":{"runAsUser":0},"containers":[{"name":"plant","image":"'"$image"'","command":["sh","-c","echo root > /v/root-owned && chmod 600 /v/root-owned"],"volumeMounts":[{"name":"v","mountPath":"/v"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"'"$claim"'"}}]}}'
  kubectl label ns "$ns" pod-security.kubernetes.io/enforce=restricted --overwrite
  kubectl -n "$ns" rollout restart "deploy/$d"
}

echo "== the release before, $prev"
helm install canyon "$work/canyon-chart.tgz" -n default -f "$values" --timeout 15m
admitted_and_up

# Data in each bundled volume, as the parts write it: the registry and Caddy as whatever user that release ran them as.
kubectl -n canyon-infra exec deploy/registry -- sh -c 'mkdir -p /var/lib/registry/docker/registry/v2/repositories/ci && echo ci > /var/lib/registry/docker/registry/v2/repositories/ci/marker'
kubectl -n canyon-gateway exec deploy/caddy -- sh -c 'mkdir -p /data/caddy/ci && echo ci > /data/caddy/ci/marker'
prev_user=$(kubectl -n canyon-infra exec deploy/registry -- id -u)

echo "== this tree's chart over it"
case $prev in
  v0.8.*) # it ran the registry and Caddy as root (backup-and-upgrade.md, The upgrade that runs the chart's bundled parts as no root)
    [ "$prev_user" = 0 ] || fail "$prev ran the registry as $prev_user; this step expects root (#2778)"
    # the page's own path for the registry: chowned while the release before still runs it, then the upgrade
    kubectl -n canyon-infra exec deploy/registry -- chown -R 65532:65532 /var/lib/registry ;;
  *) echo "::notice::$prev ran the registry as user $prev_user, not root: the upgrade of a root-run registry and Caddy isn't exercised now; the planted root-owned file below proves the guard and the chown" ;;
esac
# No --wait: a part the guard refuses never becomes ready until the chown below, which runs once every object is applied.
helm upgrade canyon "$root/deploy/helm/canyon" -n default -f "$values" --timeout 15m || fail "helm upgrade from $prev failed"
case $prev in
  v0.8.*)
    kubectl -n canyon-infra rollout status deploy/registry --timeout=10m || fail "the registry, chowned before the upgrade, did not start"
    restarts=$(kubectl -n canyon-infra get "$(newest canyon-infra registry)" -o jsonpath='{.status.initContainerStatuses[?(@.name=="volume-owned")].restartCount}')
    [ "$restarts" = 0 ] || fail "the registry, chowned before the upgrade, was refused $restarts time(s) first"
    # Caddy left as the release before wrote it: the guard refuses, and the page's recovery lets it start
    refuses canyon-gateway caddy /data
    chown_volume canyon-gateway caddy caddy-data /data ;;
esac
levels
admitted_and_up
markers

echo "== a root-owned file in each volume"
plant_root_file canyon-infra registry registry
refuses canyon-infra registry /var/lib/registry
chown_volume canyon-infra registry registry /var/lib/registry
plant_root_file canyon-gateway caddy caddy-data
refuses canyon-gateway caddy /data
chown_volume canyon-gateway caddy caddy-data /data
levels
admitted_and_up
markers

echo "== the bootstrap Job's pod, judged by Pod Security without running"
# A Pod of the Job's own template, applied as a server-side dry run: admission judges a Pod in enforce mode, where a
# Job's template is only warned about.
helm template canyon "$root/deploy/helm/canyon" -f "$values" --set bootstrap.enabled=true -s templates/hooks/bootstrap.yaml |
  kubectl create --dry-run=client -o json -f - >"$work/bootstrap.json"
python3 - "$work/bootstrap.json" >"$work/bootstrap-pod.json" <<'EOF'
import json, sys
text, docs, at = open(sys.argv[1]).read(), [], 0
dec = json.JSONDecoder()
while text[at:].strip():  # kubectl writes one JSON object per document, one after another
    at = len(text) - len(text[at:].lstrip())
    doc, at = dec.raw_decode(text, at)
    docs.extend(doc.get("items", [doc]))
jobs = [d for d in docs if d.get("kind") == "Job"]
if len(jobs) != 1:
    sys.exit(f"the bootstrap template renders {len(jobs)} Jobs")
job = jobs[0]
t = job["spec"]["template"]
print(json.dumps({"apiVersion": "v1", "kind": "Pod",
                  "metadata": {"name": "canyon-bootstrap-psa", "namespace": job["metadata"]["namespace"], "labels": t.get("metadata", {}).get("labels", {})},
                  "spec": t["spec"]}))
EOF
out=$(kubectl apply --dry-run=server -f "$work/bootstrap-pod.json" 2>&1) || fail "Pod Security refused the bootstrap Job's pod: $out"
echo "$out"
echo "the chart installs, upgrades from $prev and runs every bundled part under its namespace's Pod Security"
