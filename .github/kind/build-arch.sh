#!/usr/bin/env bash
# kpack ready to build on the kind cluster `up.sh calico` stands up, on whatever architecture the runner is (#2440):
# Canyon's stack images built here for it and named in the ClusterStack, the build pods let reach kind's registry,
# kpack's controller started and the ClusterBuilder canyon waited on. It prints the builder's architecture beside
# the node's and fails unless they are one: kpack makes the builder for the nodes it runs on. Then
# TestBuildOnKindRunsWhereItsAppRuns builds and runs an app through the executor (build-arch.yml).
set -euo pipefail

root=$(git rev-parse --show-toplevel)
reg=172.30.0.200:5000
work=$(mktemp -d)
node_arch=$(kubectl get node kind-control-plane -o jsonpath='{.status.nodeInfo.architecture}')
echo "== the node is $node_arch"

push() { # Dockerfile name: built from this tree for the runner's architecture, pushed, answered by digest as the cluster sees it
  docker build -q -f "$root/$1" -t "localhost:5001/$2:arch" "$root" >/dev/null
  docker push -q "localhost:5001/$2:arch" >/dev/null
  echo "$reg/$2@$(docker inspect -f '{{index .RepoDigests 0}}' "localhost:5001/$2:arch" | cut -d@ -f2)"
}
build=$(push executor/build.Dockerfile canyon-build) run=$(push executor/run.Dockerfile canyon-run)
kubectl patch clusterstack canyon --type=merge -p "{\"spec\":{\"buildImage\":{\"image\":\"$build\"},\"runImage\":{\"image\":\"$run\"}}}"
kubectl -n canyon-builds create secret docker-registry canyon-registry --docker-server=$reg --docker-username=canyon --docker-password=anonymous \
  --dry-run=client -o yaml | kubectl apply -f -
# kind's registry is at a private address, which the build pods' own policy refuses (builds.yaml): this cluster's
# overlay opens it, as an install's opens its own registry.
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-builds-kind-registry, namespace: canyon-builds}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress: [{to: [{ipBlock: {cidr: ${reg%:*}/32}}], ports: [{protocol: TCP, port: ${reg#*:}}]}]
EOF
kubectl -n kpack scale deploy/kpack-controller --replicas=1
kubectl -n kpack rollout status deploy/kpack-controller --timeout=5m
if ! kubectl wait clusterbuilder/canyon --for=condition=Ready --timeout=20m; then
  kubectl get clusterbuilder canyon -o yaml
  echo "::error::on $node_arch the ClusterBuilder canyon never became ready"
  exit 1
fi
builder=$(kubectl get clusterbuilder canyon -o jsonpath='{.status.latestImage}')
docker pull -q "localhost:5001/${builder#"$reg/"}" >/dev/null
builder_arch=$(docker image inspect -f '{{.Architecture}}' "localhost:5001/${builder#"$reg/"}")
echo "== the builder $builder is $builder_arch"

echo "ANSWER node=$node_arch builder=$builder_arch"
[ "$builder_arch" = "$node_arch" ]
