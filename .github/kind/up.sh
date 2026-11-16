#!/usr/bin/env bash
# The kind cluster `make test-kube` runs against in .github/workflows/kube.yml (contracts/executor.md,
# invariants 10-11). `up.sh calico`: kind with Calico, which enforces NetworkPolicy. `up.sh kindnet`: kind
# v0.23's kindnet, the last kind network that does not (kind v0.24 and later enforce it too). `up.sh down`
# removes everything either made. On the kind network, at fixed addresses: registry:2 — an RFC 1918
# address, which the executor's registry client and kpack's speak http to — the apps database, Postgres
# on 5433 with a bare listener on 5432 (no app may reach it), Canyon's own Postgres on 5432, and bare
# listeners on 443 and 8443 standing in for an internal identity provider (TestKindSigninPastTheGateway). The
# server, the executor and canyon-identity are built from this tree and pushed there, and deploy/k8s is
# applied with this cluster's values: the server runs, so its NetworkPolicy has a pod to hold (#1191).
set -euo pipefail

reg=172.30.0.200:5000 db=172.30.0.201 pg=172.30.0.202 idp=172.30.0.203
postgres=docker.io/library/postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea

down() {
  kind delete cluster >/dev/null 2>&1 || true
  docker rm -f kind-registry kind-db kind-pg kind-idp >/dev/null 2>&1 || true
  docker network rm kind >/dev/null 2>&1 || true
}
case ${1:?calico, kindnet or down} in
  calico) kind_v=v0.33.0 kind_sum_amd64=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d kind_sum_arm64=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
    node=kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d ;;
  kindnet) kind_v=v0.23.0 kind_sum_amd64=1d86e3069ffbe3da9f1a918618aecbc778e00c75f838882d0dfa2d363bc4a68c kind_sum_arm64=a416d6c311882337f0e56910e4a2e1f8c106ec70c22cbf0ac1dd8f33c1e284fe
    node=kindest/node:v1.30.0@sha256:047357ac0cfea04663786a612ba1eaba9702bef25227a794b52890dd8bcd692e ;;
  down) down; exit 0 ;;
  *) echo "calico, kindnet or down" >&2; exit 2 ;;
esac

root=$(git rev-parse --show-toplevel) work=$(mktemp -d) bin=$HOME/.local/bin
mkdir -p "$bin" && export PATH=$bin:$PATH
fetch() { "$root/.github/fetch.sh" "$@"; } # url sha256 file: downloaded with retries, checked, kept only when it matches
case $(uname -m) in # arm64 runners cost 5/8 of x64's; every image this script starts is a multi-arch index
  x86_64) arch=amd64 kubectl_sum=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3 kind_sum=$kind_sum_amd64 ;;
  aarch64|arm64) arch=arm64 kubectl_sum=420e6110e3ba7ee5a3927b5af868d18df17aae36b720529ffa4e9e945aa95450 kind_sum=$kind_sum_arm64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
fetch "https://github.com/kubernetes-sigs/kind/releases/download/$kind_v/kind-linux-$arch" $kind_sum "$bin/kind"
fetch https://dl.k8s.io/release/v1.34.1/bin/linux/$arch/kubectl $kubectl_sum "$bin/kubectl"
chmod +x "$bin/kind" "$bin/kubectl"

down
docker network create kind --subnet 172.30.0.0/24 --ip-range 172.30.0.0/25 >/dev/null
docker run -d --name kind-registry --network kind --ip ${reg%:*} -p 127.0.0.1:5001:5000 \
  registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 >/dev/null
admin_pw=kind-$(openssl rand -hex 12) canyon_pw=kind-$(openssl rand -hex 12)
docker run -d --name kind-db --network kind --ip $db -e POSTGRES_USER=canyon_admin -e POSTGRES_PASSWORD=$admin_pw \
  $postgres sh -c 'nc -lk -p 5432 -e true & exec docker-entrypoint.sh postgres -p 5433' >/dev/null
docker run -d --name kind-pg --network kind --ip $pg -e POSTGRES_USER=canyon -e POSTGRES_PASSWORD=$canyon_pw $postgres >/dev/null
docker run -d --name kind-idp --network kind --ip $idp $postgres sh -c 'nc -lk -p 8443 -e true & exec nc -lk -p 443 -e true' >/dev/null

cni=""
[ "$1" = calico ] && cni="networking: {disableDefaultCNI: true, podSubnet: 192.168.0.0/16}"
kind create cluster --image $node --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
$cni
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
EOF
docker exec kind-control-plane mkdir -p /etc/containerd/certs.d/$reg
printf '[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n' $reg |
  docker exec -i kind-control-plane cp /dev/stdin /etc/containerd/certs.d/$reg/hosts.toml
if [ "$1" = calico ]; then
  fetch https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/calico.yaml \
    a8c828a06a87c629a282ebbc424895b77f3a030251993e41ea400a743675bb02 "$work/calico.yaml"
  kubectl apply -f "$work/calico.yaml" >/dev/null
  kubectl -n kube-system rollout status ds/calico-node --timeout=5m
fi
kubectl wait --for=condition=Ready node --all --timeout=5m

# kpack, whose Builds the executor watches from its start; its controller only where a job builds.
fetch https://github.com/buildpacks-community/kpack/releases/download/v0.18.0/release-0.18.0.yaml \
  cde8b7df8d31d6a5758ec4880eec45009f17811baf3df5a29b76a144fe200e69 "$work/kpack.yaml"
retry() { for i in 1 2 3 4 5 6; do "$@" && return; sleep 10; done; return 1; }
kubectl apply -f "$work/kpack.yaml" >/dev/null 2>&1 || true # its own objects need its CRDs and webhook first
kubectl wait --for=condition=Established crd --all --timeout=2m
# kpack v0.18 exits at start on Kubernetes before v1.32 ("Version check failed"), so on kindnet's v1.30 its
# webhook only flickers Ready between crashes (#2328). kindnet builds nothing: it runs neither, and kpack's
# objects apply with no webhook to answer, its webhook configurations having no rules until it runs.
[ "$1" = calico ] && kubectl -n kpack rollout status deploy/kpack-webhook --timeout=5m
retry kubectl apply -f "$work/kpack.yaml" >/dev/null
kubectl -n kpack scale deploy/kpack-controller --replicas=0
[ "$1" = kindnet ] && kubectl -n kpack scale deploy/kpack-webhook --replicas=0

push() { # Dockerfile repository: built from this tree, pushed, answered by digest as the cluster sees it
  docker build -q -f "$root/$1" ${GOPROXY:+--build-arg "GOPROXY=$GOPROXY"} -t localhost:5001/canyon-kind/$2:ci "$root" >/dev/null # go-cache's list (#2533)
  docker push -q localhost:5001/canyon-kind/$2:ci >/dev/null
  echo "$reg/canyon-kind/$2@$(docker inspect -f '{{index .RepoDigests 0}}' localhost:5001/canyon-kind/$2:ci | cut -d@ -f2)"
}
executor=$(push Dockerfile.executor-kube executor-kube) identity=$(push Dockerfile.identity identity) server=$(push Dockerfile server)

# This cluster's values over the base: the images, the executor's environment, where kpack pushes the
# builder, the server's two databases, and the addresses the base cannot know: the API server on the
# node at 6443, and registry:2. No scan runs here, so no volume in canyon-scans: the executor's start-up
# refreshes of the scanners' databases, gigabytes on the runner's disk, stop at their volume.
cat > "$work/kind-policy.yaml" <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-executor-kind, namespace: canyon}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: canyon-executor}}
  policyTypes: [Egress]
  egress:
    - to: [{ipBlock: {cidr: 172.30.0.0/24}}]
      ports: [{protocol: TCP, port: 6443}, {protocol: TCP, port: ${reg#*:}}]
---
apiVersion: v1
kind: ResourceQuota
metadata: {name: canyon-kind-no-scandb, namespace: canyon-scans}
spec: {hard: {persistentvolumeclaims: "0"}}
EOF
cat > "$work/kustomization.yaml" <<EOF
resources: [$(realpath --relative-to="$work" "$root/deploy/k8s"), kind-policy.yaml]
images:
  - {name: canyon-executor-kube, newName: ${executor%@*}, digest: ${executor#*@}}
  - {name: canyon-server, newName: ${server%@*}, digest: ${server#*@}}
patches:
  - target: {kind: Deployment, name: canyon-executor}
    patch: |-
      [{"op": "replace", "path": "/spec/template/spec/containers/0/env", "value": [
        {"name": "CANYON_KUBE_REGISTRY", "value": "$reg"},
        {"name": "CANYON_IMAGE_IDENTITY", "value": "$identity"},
        {"name": "CANYON_KUBE_DB_CIDRS", "value": "$db/32"}]}]
  # CANYON_AUTH=demo serves loopback alone on a cluster (cmd/canyon-server/localpeer.go), and kubelet's httpGet comes
  # from the node: /ready is asked from inside the pod instead, by a container beside the server on its loopback.
  - target: {kind: Deployment, name: canyon-server}
    patch: |-
      [{"op": "remove", "path": "/spec/template/spec/containers/0/readinessProbe"},
       {"op": "add", "path": "/spec/template/spec/containers/-", "value": {"name": "ready",
        "image": "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e",
        "command": ["sh", "-c", "trap 'exit 0' TERM; sleep 2147483647 & wait"],
        "readinessProbe": {"exec": {"command": ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:7777/ready"]}, "timeoutSeconds": 2},
        "resources": {"requests": {"cpu": "10m", "memory": "8Mi"}, "limits": {"cpu": "50m", "memory": "32Mi"}},
        "securityContext": {"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": {"drop": ["ALL"]}}}}]
  - target: {kind: NetworkPolicy, name: canyon-server}
    patch: |-
      [{"op": "replace", "path": "/spec/egress/3/to/0/ipBlock/cidr", "value": "$pg/32"},
       {"op": "replace", "path": "/spec/egress/4/to/0/ipBlock/cidr", "value": "$db/32"},
       {"op": "replace", "path": "/spec/egress/5/to/0/ipBlock/cidr", "value": "172.30.0.0/24"}]
  - target: {kind: ClusterBuilder, name: canyon}
    patch: '[{"op": "replace", "path": "/spec/tag", "value": "$reg/canyon-builder"}]'
EOF
retry kubectl apply -k "$work"
# The server's Secret, as the estate's holds it: its own database, and the apps database's admin login,
# which no app may see. No identity provider here: CANYON_AUTH=demo, on a cluster nothing outside reaches.
kubectl -n canyon create secret generic canyon-server \
  --from-literal=CANYON_DATABASE_URL="postgres://canyon:$canyon_pw@$pg:5432/canyon?sslmode=disable" \
  --from-literal=CANYON_APP_DATABASE_URL="postgres://canyon_admin:$admin_pw@$db:5433/postgres?sslmode=disable" \
  --from-literal=CANYON_TELEMETRY=0 \
  --from-literal=CANYON_AUTH=demo
# Canyon's egress gateway's token for the server's receiver (deploy/k8s/egress.yaml): Envoy starts on it.
kubectl -n canyon create secret generic canyon-egress --from-literal=token="$(openssl rand -hex 32)"
kubectl -n canyon rollout status deploy/canyon-executor --timeout=5m
kubectl -n canyon rollout status deploy/canyon-server --timeout=5m
"$root/scripts/telemetry-off-check.sh" kube
# The install step that publishes the certificate the server minted for the policy egress-control pins
# (deploy/k8s/README.md); the server read the cluster's key set over egress[5], the API server's network above.
cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) # set -e: no ConfigMap of nothing
kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
# The policy's real path (design/EGRESS.md §3.2): a pod of the gateway's ServiceAccount (deploy/k8s/rbac.yaml) and
# labels, its projected token for the policy's audience, the minted certificate pinned, is answered 200; without
# the token, 401. The router's image, already on the node, carries curl.
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: canyon-egress-policy-check, namespace: canyon, labels: {app.kubernetes.io/name: canyon-egress}}
spec:
  serviceAccountName: canyon-egress
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 101, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: check
      image: $(kubectl -n canyon get deploy canyon-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="nginx")].image}')
      command: [sleep, "600"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: policy, mountPath: /var/run/canyon-egress-policy, readOnly: true}]
  volumes:
    - name: policy
      projected:
        sources:
          - serviceAccountToken: {path: token, audience: canyon-egress-policy, expirationSeconds: 600}
          - configMap: {name: canyon-egress-policy-cert, items: [{key: tls.crt, path: tls.crt}]}
EOF
kubectl -n canyon wait --for=condition=Ready pod/canyon-egress-policy-check --timeout=3m
policy() { kubectl -n canyon exec canyon-egress-policy-check -- sh -c "curl -sS -o /dev/null -w '%{http_code}' --cacert /var/run/canyon-egress-policy/tls.crt $1 'https://canyon-server.canyon.svc:7782/egress/policy?have='"; }
got=$(policy '-H "Authorization: Bearer $(cat /var/run/canyon-egress-policy/token)"')
[ "$got" = 200 ] || { echo "the policy, egress-control's token and the pinned certificate: $got, want 200"; exit 1; }
got=$(policy '')
[ "$got" = 401 ] || { echo "the policy without a token: $got, want 401"; exit 1; }
kubectl -n canyon delete pod canyon-egress-policy-check --wait=false
kubectl -n canyon rollout status deploy/canyon-router --timeout=5m
kubectl -n canyon rollout status deploy/canyon-egress --timeout=5m
