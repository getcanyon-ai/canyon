#!/usr/bin/env bash
# The platform a deploy repo hands releases to, on the cluster `up.sh calico` made (e2e/handoff/README.md, the repo
# journey): Gateway API's CRDs, CloudNativePG and External Secrets, then the GitOps controller the argument names,
# argocd (Argo CD's core install) or flux (Flux's install, source-watcher among it), each a pinned release,
# checksum-verified. Nothing of Canyon's: the journey applies deploy/gitops itself, with the values it makes.
set -euo pipefail

gitops=${1:?usage: platform.sh argocd|flux}
case $gitops in argocd | flux) ;; *) echo "platform.sh: $gitops is neither argocd nor flux" >&2; exit 2 ;; esac

work=$(mktemp -d)
fetch() { "$(dirname "$0")/../fetch.sh" "$@"; } # url sha256 file: downloaded with retries, checked, kept only when it matches
fetch https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml \
  356c2b8e286a964dc20995435659ad1095386329974793af10bbfe3eff977fe5 "$work/gateway-api.yaml"
fetch https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml \
  37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb "$work/cnpg.yaml"
fetch https://github.com/external-secrets/external-secrets/releases/download/helm-chart-2.12.0/external-secrets-2.12.0.tgz \
  4b584136b7311c0af8c8719d377cc5b1a7ef12c2de18f1fdb30614eab75bdeb3 "$work/external-secrets.tgz"

kubectl apply --server-side -f "$work/gateway-api.yaml" >/dev/null
kubectl apply --server-side -f "$work/cnpg.yaml" >/dev/null
helm install external-secrets "$work/external-secrets.tgz" -n external-secrets --create-namespace --wait --timeout 5m >/dev/null
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=5m

if [ "$gitops" = argocd ]; then
  fetch https://raw.githubusercontent.com/argoproj/argo-cd/v3.5.6/manifests/core-install.yaml \
    d3d9d3555ad77eb4b6d27b1ec0d2af7978a91537e84d54631f983c227f0a3d31 "$work/argocd.yaml"
  kubectl create namespace argocd
  kubectl -n argocd apply --server-side -f "$work/argocd.yaml" >/dev/null
  # How often Argo CD looks at the repo: its default, 3 minutes, is most of a journey.
  kubectl -n argocd patch configmap argocd-cm --type merge -p '{"data":{"timeout.reconciliation":"15s"}}'
  kubectl -n argocd rollout restart statefulset/argocd-application-controller
  for d in $(kubectl -n argocd get deploy -o name); do kubectl -n argocd rollout status "$d" --timeout=5m; done
  kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=5m
  exit 0
fi

# Flux v2.9.6's install.yaml (flux_2.9.6_checksums.txt): source-controller and kustomize-controller v1.9.6,
# helm-controller v1.6.5 and source-watcher v2.2.4, multi-arch. The flags are deploy/gitops's (docs: Flux, What to
# install): required, source-watcher running, helm-controller's ExternalArtifact gate and kustomize-controller's
# --no-remote-bases; hardening, --no-cross-namespace-refs and --default-service-account, which nothing here breaks.
fetch https://github.com/fluxcd/flux2/releases/download/v2.9.6/install.yaml \
  9c1fda7e401429531ed1478f67ba06b3edff6513b75da7ee47bde9f1c4d4251c "$work/flux.yaml"
kubectl apply --server-side -f "$work/flux.yaml" >/dev/null
args() { # deployment flag...: each flag appended to the controller's arguments
  local d=$1 ops=() f
  shift
  for f in "$@"; do ops+=("{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"$f\"}"); done
  kubectl -n flux-system patch deployment "$d" --type json -p "[$(IFS=,; echo "${ops[*]}")]"
}
args kustomize-controller --no-remote-bases=true --no-cross-namespace-refs=true --default-service-account=default
args helm-controller --feature-gates=ExternalArtifact=true --no-cross-namespace-refs=true --default-service-account=default
args source-watcher --no-cross-namespace-refs=true
# The image controllers are no part of the hand-off.
kubectl -n flux-system scale deployment image-reflector-controller image-automation-controller --replicas=0
for d in source-controller kustomize-controller helm-controller source-watcher notification-controller; do
  kubectl -n flux-system rollout status "deploy/$d" --timeout=5m
done
