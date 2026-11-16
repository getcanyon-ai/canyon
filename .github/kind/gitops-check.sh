#!/usr/bin/env bash
# The reference set-up's pieces the hand-off repo journey cannot exercise on kind, checked against the cluster
# platform.sh made (design/HAND-OFF.md §10). Needs helm, kubectl and the Gateway API CRDs. Usage: gitops-check.sh argocd|flux
#
# Both: the namespaces chart in Argo CD's mode labels each namespace with its environment, which the Gateway's listeners
# select on (kind runs no Gateway API implementation, #2476), and refuses any other; the reference Gateway is valid
# under Gateway API's CRDs.
#
# Flux (#2479): the chart in Flux's mode, laid out and loaded as the generator and helm-controller do, renders exactly
# the objects the docs show and refuses every input it must. Then, after the journey, the generator's account, the
# sync accounts and the admission policy, as themselves through the cluster's real RBAC and policy. The generator never deletes an app's namespace on a misconfigured upgrade, and a
# failed upgrade leaves nothing it made; the policy refuses each thing RBAC alone would let that account do; the sync
# accounts hold exactly canyon-deploy, in their own environment's namespaces alone.
set -euo pipefail

gitops=${1:?usage: gitops-check.sh argocd|flux}
root=$(git rev-parse --show-toplevel)
ref=$root/deploy/gitops
chart=$ref/namespaces
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0
fail() { echo "gitops-check: $*" >&2; exit 1; } # a step the next ones rest on
miss() { echo "gitops-check: $*" >&2; failed=1; } # a check of its own: the others still run, and the script fails at its end
refused() { # what message command...: the command must fail, and say message
  local what=$1 message=$2 out
  shift 2
  if out=$("$@" 2>&1); then miss "$what was admitted: $out"; return 0; fi
  if grep -qF -- "$message" <<<"$out"; then echo "$what: refused"; else miss "$what failed for another reason than \"$message\": $out"; fi
}

# --- Argo CD's mode ---------------------------------------------------------------------------------------------
for env in preview prod; do
  printf 'app: claims-portal\nenv: %s\n' "$env" >"$work/values.yaml"
  # server-side dry run: the API server parses what the chart rendered, and says the label it would store
  got=$(helm template ns "$chart" -f "$work/values.yaml" | kubectl create --dry-run=server -o 'jsonpath={.metadata.labels.canyon\.dev/env}' -f -)
  [ "$got" = "$env" ] || fail "the namespaces chart labels canyon-claims-portal-$env canyon.dev/env=\"$got\", not $env"
  echo "canyon-claims-portal-$env: canyon.dev/env=$got"
done
printf 'app: claims-portal\nenv: staging\n' >"$work/values.yaml"
refused "env staging" "env: preview or prod" helm template ns "$chart" -f "$work/values.yaml"
printf 'env: preview\n' >"$work/values.yaml"
refused "neither app nor root" "set app (Argo CD's ApplicationSet) or root (Flux's HelmRelease)" helm template ns "$chart" -f "$work/values.yaml"
printf 'app: claims-portal\nroot: apps\nenv: preview\n' >"$work/values.yaml"
refused "both app and root" "set app (Argo CD's ApplicationSet) or root (Flux's HelmRelease)" helm template ns "$chart" -f "$work/values.yaml"

kubectl create namespace gateway --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply --dry-run=server -f "$ref/gateway/gateway.yaml"
kubectl delete namespace gateway --wait=false >/dev/null

[ "$gitops" = flux ] || exit "$failed"

# --- Flux's mode, rendered ----------------------------------------------------------------------------------------
# The chart looks the admission policy up before it renders anything, so this runs where Flux is: helm's server-side
# dry run reads the policy and maps each kind it renders, Flux's Kustomization among them. The journey applied the
# policy; when it stopped before that, it is applied here.
if ! kubectl get validatingadmissionpolicybinding canyon-namespaces >/dev/null 2>&1; then
  sed 's|\${ROOT}|apps|g' "$ref/flux/admission.yaml" | kubectl apply -f - >/dev/null
fi

# pack name file...: the chart as the ArtifactGenerator lays it out (flux/sources.yaml), canyon-namespace/ with each
# file under its deploy/, archived from the staging root as source-watcher archives it; prints the archive's path.
# DROP names one of the chart's own files to leave out.
pack() {
  local name=$1 stage=$work/$1 f
  shift
  mkdir -p "$stage/canyon-namespace"
  cp -R "$chart/." "$stage/canyon-namespace/"
  [ -z "${DROP:-}" ] || rm "$stage/canyon-namespace/$DROP"
  for f in "$@"; do
    mkdir -p "$(dirname "$stage/canyon-namespace/deploy/$f")"
    : >"$stage/canyon-namespace/deploy/$f"
  done
  tar -C "$stage" -czf "$stage.tgz" canyon-namespace
  echo "$stage.tgz"
}
# render archive root env [helm flags...]: helm template loads the archive as helm-controller loads its artifact, and
# looks the policy up on the cluster.
render() {
  local archive=$1
  printf 'root: %s\nenv: %s\ninterval: 1m\n' "$2" "$3" >"$work/flux-values.yaml"
  shift 3
  helm template ns "$archive" -f "$work/flux-values.yaml" --dry-run=server "$@"
}
sentinel=apps/.canyon/route.yaml
normal=$(pack normal "$sentinel" apps/claims-portal/preview/canyon.json apps/claims-portal/prod/canyon.json)
want=$(cat <<'EOF'
---
# Source: canyon-namespace/templates/namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: "canyon-claims-portal-preview"
  labels:
    pod-security.kubernetes.io/enforce: baseline
    canyon.dev/managed: "true"
    canyon.dev/env: "preview"
---
# Source: canyon-namespace/templates/namespace.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: canyon-deploy
  namespace: "canyon-claims-portal-preview"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: canyon-deploy
subjects:
  - kind: ServiceAccount
    name: "canyon-preview"
    namespace: canyon-sync
---
# Source: canyon-namespace/templates/namespace.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: "canyon-claims-portal-preview"
  namespace: canyon-sync
spec:
  sourceRef:
    kind: GitRepository
    name: "canyon-preview"
  path: "./apps/claims-portal/preview"
  targetNamespace: "canyon-claims-portal-preview"
  serviceAccountName: "canyon-preview"
  interval: "1m"
  prune: true
  wait: false
  deletionPolicy: Orphan
EOF
)
if ! got=$(render "$normal" apps preview); then
  miss "the chart did not render one preview app: $got"
elif [ "$got" != "$want" ]; then
  miss "the chart rendered, for apps/claims-portal/preview and prod on preview:
$got
want exactly
$want"
else
  echo "Flux's mode, one app: exactly its namespace, its RoleBinding and its Kustomization, prod's path left out"
fi
if ! got=$(render "$(pack empty "$sentinel")" apps preview); then
  miss "the chart did not render an environment with no app: $got"
elif [ -n "${got//[$'\n ']/}" ]; then
  miss "the chart rendered, for an environment with no app, $got"
else
  echo "Flux's mode, no app: nothing, and no error"
fi

missing="is not in the generator's artifact"
refused "no sentinel" "$missing" render "$(pack nosentinel apps/claims-portal/preview/canyon.json)" apps preview
refused "another root" "$missing" render "$normal" other preview
refused "the prefix one directory down" "$missing" render "$(pack shifted "x/$sentinel" x/apps/claims-portal/preview/canyon.json)" apps preview
refused "Bad_App" "app: a DNS label" render "$(pack bad "$sentinel" apps/Bad_App/preview/canyon.json)" apps preview
refused "a name over 63 characters" "longer than 63 characters" render "$(pack long "$sentinel" "apps/$(printf 'a%.0s' {1..50})/preview/canyon.json")" apps preview
refused "a name with a newline" "app: a DNS label" render "$(pack newline "$sentinel" $'apps/claims\nportal/preview/canyon.json')" apps preview
refused "a name with a colon" "app: a DNS label" render "$(pack colon "$sentinel" 'apps/x: y/preview/canyon.json')" apps preview
refused "root .." "root: the deploy repo's path root" render "$normal" .. preview
refused "env staging" "env: preview or prod" render "$normal" apps staging
# A copy of the chart without templates/flux.yaml, such as Argo CD's three files, under Flux's root: it must refuse,
# never render an empty environment that would delete every namespace.
refused "the chart without templates/flux.yaml" 'canyon.flux' render "$(DROP=templates/flux.yaml pack noflux "$sentinel" apps/claims-portal/preview/canyon.json)" apps preview
refused "the chart without templates/namespace.yaml" 'canyon.namespace.chart' render "$(DROP=templates/namespace.yaml pack nonamespace "$sentinel" apps/claims-portal/preview/canyon.json)" apps preview
# Without a cluster to look the policy up on, the policy is not there: helm template's own default.
printf 'root: apps\nenv: preview\ninterval: 1m\n' >"$work/flux-values.yaml"
refused "no admission policy" "the admission policy canyon-namespaces and its binding" helm template ns "$normal" -f "$work/flux-values.yaml"

# --- Flux, on the cluster ----------------------------------------------------------------------------------------
generator=system:serviceaccount:flux-system:canyon-namespaces
as=(--as="$generator")
helm_as=(helm --kube-as-user "$generator" -n canyon-namespaces)
probe() { # release env root file...: install or upgrade a probe release as the generator, as its HelmRelease does
  local release=$1 env=$2 r=$3 archive
  shift 3
  archive=$(pack "$release-$RANDOM" "$@")
  printf 'root: %s\nenv: %s\ninterval: 1m\n' "$r" "$env" >"$work/probe-values.yaml"
  "${helm_as[@]}" upgrade --install "$release" "$archive" -f "$work/probe-values.yaml" ${CLEANUP:+--cleanup-on-fail}
}
gone() { # namespace: within two minutes it no longer exists
  for _ in $(seq 60); do kubectl get namespace "$1" >/dev/null 2>&1 || return 0; sleep 2; done
  fail "$1 is still there two minutes on"
}

app=canyon-rbac-probe-preview
probe probe-preview preview apps "$sentinel" apps/rbac-probe/preview/canyon.json >/dev/null
kubectl -n "$app" create configmap data --from-literal=kept=yes >/dev/null
echo "the generator made $app, and a ConfigMap stands in for its data"
out=$(probe probe-preview preview other "$sentinel" apps/rbac-probe/preview/canyon.json 2>&1) && upgraded=yes || upgraded=no
[ "$(kubectl get namespace "$app" -o 'jsonpath={.status.phase}' 2>&1)" = Active ] && kubectl -n "$app" get configmap data >/dev/null 2>&1 ||
  fail "an upgrade with root other deleted $app and its data (upgraded: $upgraded): $(kubectl get namespace "$app" -o 'jsonpath={.status.phase}' 2>&1) $out"
[ "$upgraded" = no ] && grep -qF "$missing" <<<"$out" || fail "an upgrade with root other was not refused for its missing sentinel: $out"
echo "an upgrade with root other: refused, and $app and its data are still there"
probe probe-preview preview apps "$sentinel" >/dev/null
gone "$app"
echo "an upgrade with the sentinel and no app: $app is gone"
probe probe-preview preview apps "$sentinel" apps/rbac-probe/preview/canyon.json >/dev/null
probe probe-prod prod apps "$sentinel" apps/rbac-probe/prod/canyon.json >/dev/null

# A failed upgrade leaves nothing it made (upgrade.cleanupOnFail, flux/generator.yaml): the canyon-sync quota refuses
# one more Kustomization after the namespace and the RoleBinding before it were made.
count=$(kubectl -n canyon-sync get kustomizations.kustomize.toolkit.fluxcd.io -o name | wc -l)
kubectl -n canyon-sync create quota gitops-check-full --hard="count/kustomizations.kustomize.toolkit.fluxcd.io=$count" >/dev/null
for _ in $(seq 30); do # the quota admits nothing until the controller has counted what it holds
  [ -n "$(kubectl -n canyon-sync get quota gitops-check-full -o 'jsonpath={.status.used}')" ] && break
  sleep 2
done
out=$(CLEANUP=1 probe probe-preview preview apps "$sentinel" apps/rbac-probe/preview/canyon.json apps/rbac-flood/preview/canyon.json 2>&1) &&
  fail "an upgrade past the Kustomization quota succeeded: $out"
grep -qF "exceeded quota" <<<"$out" || fail "the upgrade past the quota failed for another reason: $out"
kubectl -n canyon-sync delete quota gitops-check-full >/dev/null
gone canyon-rbac-flood-preview
kubectl get namespace "$app" >/dev/null || fail "the failed upgrade's clean-up took $app, which it did not make"
echo "an upgrade past the quota: refused, and the namespace it made is gone"

# The admission policy, as the generator's account. Each request RBAC lets it make is admitted when it is Canyon's
# own, and refused by the policy when it is not; those RBAC refuses first are refused all the same.
policy="ValidatingAdmissionPolicy 'canyon-namespaces' with binding 'canyon-namespaces' denied request: "
denied() { # what message manifest: a server-side dry run of the manifest, as the generator, is refused by the policy
  refused "$1" "$policy$2" kubectl create --dry-run=server "${as[@]}" -f - <<<"$3"
}
forbidden() { # what command...: refused by RBAC, before any policy
  local out
  if out=$("${@:2}" 2>&1); then miss "$1 was admitted: $out"; return 0; fi
  if grep -qF "is forbidden" <<<"$out" && ! grep -qF "ValidatingAdmissionPolicy" <<<"$out"; then echo "$1: refused by RBAC"; else miss "$1 was not refused by RBAC: $out"; fi
}
namespace() { # name env [extra label line]
  printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    pod-security.kubernetes.io/enforce: baseline\n    canyon.dev/managed: "true"\n    canyon.dev/env: %s\n%s' "$1" "$2" "${3:-}"
}
rolebinding() { # namespace role subject-namespace subject-name
  printf 'apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: canyon-deploy\n  namespace: %s\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: %s\nsubjects:\n  - kind: ServiceAccount\n    name: %s\n    namespace: %s\n' "$1" "$2" "$4" "$3"
}
kustomization() { # namespace name env account [extra spec line]
  printf 'apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  sourceRef:\n    kind: GitRepository\n    name: canyon-%s\n  path: ./apps/%s/%s\n  targetNamespace: %s\n  serviceAccountName: %s\n  interval: 1m\n  prune: true\n  wait: false\n  deletionPolicy: Orphan\n%s' \
    "$2" "$1" "$3" "$(sed -E 's/^canyon-(.*)-(preview|prod)$/\1/' <<<"$2")" "$3" "$2" "$4" "${5:-}"
}

namespace canyon-ok-preview preview | kubectl create "${as[@]}" -f - >/dev/null || fail "the generator could not make a namespace of Canyon's own"
rolebinding canyon-ok-preview canyon-deploy canyon-sync canyon-preview | kubectl create --dry-run=server "${as[@]}" -f - >/dev/null ||
  fail "the generator could not bind canyon-deploy for canyon-preview in canyon-ok-preview"
kustomization canyon-sync canyon-ok-preview preview canyon-preview | kubectl create --dry-run=server "${as[@]}" -f - >/dev/null ||
  fail "the generator could not make canyon-ok-preview's Kustomization"
kubectl delete namespace canyon-ok-preview "${as[@]}" --wait=false >/dev/null || fail "the generator could not delete a namespace of its own"
echo "Canyon's own namespace, RoleBinding and Kustomization: admitted"

namespaces="Canyon's namespaces generator makes namespaces named"
others="Canyon's namespaces generator changes or deletes only Canyon's own namespaces"
denied "a Namespace probe" "$namespaces" "$(namespace probe preview)"
denied "canyon-x-preview labelled canyon.dev/env=prod" "$namespaces" "$(namespace canyon-x-preview prod)"
denied "a Pod Security enforce-version" "Canyon's namespaces generator sets no label or annotation" \
  "$(namespace canyon-x-preview preview '    pod-security.kubernetes.io/enforce-version: v1.20
')"
denied "a stray annotation" "Canyon's namespaces generator sets no label or annotation" \
  "$(namespace canyon-x-preview preview | sed 's|^  labels:|  annotations:\n    scheduler.alpha.kubernetes.io/node-selector: pool=gitops-check\n  labels:|')"
refused "a label on default" "$policy$others" kubectl label namespace default "${as[@]}" --dry-run=server canyon.dev/env=prod
refused "a delete of flux-system" "$policy$others" kubectl delete namespace flux-system "${as[@]}" --dry-run=server
kubectl create namespace canyon-taken-preview >/dev/null
refused "a label on another team's canyon-taken-preview" "$policy$others" kubectl label namespace canyon-taken-preview "${as[@]}" --dry-run=server \
  canyon.dev/managed=true pod-security.kubernetes.io/enforce=baseline canyon.dev/env=preview
kubectl delete namespace canyon-taken-preview --wait=false >/dev/null

inside="Canyon's namespaces generator makes, changes and deletes RoleBindings in Canyon's own app namespaces alone"
# A managed namespace with no RoleBinding yet, so a case the policy would let through reads as admitted.
pins=canyon-pins-preview
namespace "$pins" preview | kubectl create "${as[@]}" -f - >/dev/null || fail "the generator could not make $pins"
denied "a RoleBinding of canyon-deploy in kube-system" "$inside" "$(rolebinding kube-system canyon-deploy canyon-sync canyon-preview)"
denied "a RoleBinding of canyon-deploy in default" "$inside" "$(rolebinding default canyon-deploy canyon-sync canyon-preview)"
denied "a RoleBinding of canyon-namespaces in $pins" "Canyon's namespaces generator binds the ClusterRole canyon-deploy alone" \
  "$(rolebinding "$pins" canyon-namespaces canyon-sync canyon-preview)"
subject="Canyon's namespaces generator binds canyon-deploy for the namespace's own environment's sync account alone"
denied "a RoleBinding for canyon-prod in $pins" "$subject" "$(rolebinding "$pins" canyon-deploy canyon-sync canyon-prod)"
denied "a RoleBinding with a second subject in $pins" "$subject" "$(rolebinding "$pins" canyon-deploy canyon-sync canyon-preview)
  - kind: ServiceAccount
    name: canyon-preview
    namespace: canyon-sync"
denied "a RoleBinding for a User in $pins" "$subject" \
  "$(rolebinding "$pins" canyon-deploy canyon-sync canyon-preview | sed 's|^  - kind: ServiceAccount|  - kind: User\n    apiGroup: rbac.authorization.k8s.io|')"
denied "a RoleBinding for default/canyon-preview in $pins" "$subject" "$(rolebinding "$pins" canyon-deploy default canyon-preview)"
denied "a RoleBinding named other than canyon-deploy in $pins" "Canyon's namespaces generator binds the ClusterRole canyon-deploy alone, in a RoleBinding named canyon-deploy" \
  "$(rolebinding "$pins" canyon-deploy canyon-sync canyon-preview | sed '0,/^  name: canyon-deploy$/s//  name: canyon-other/')"
kubectl -n kube-system create rolebinding gitops-check-unrelated --clusterrole=view --user=nobody >/dev/null
refused "a delete of another RoleBinding in kube-system" "$policy$inside" kubectl -n kube-system delete rolebinding gitops-check-unrelated "${as[@]}" --dry-run=server
kubectl -n kube-system delete rolebinding gitops-check-unrelated >/dev/null
forbidden "a RoleBinding of admin in $pins" kubectl create --dry-run=server "${as[@]}" -f - <<<"$(rolebinding "$pins" admin canyon-sync canyon-preview)"
kubectl delete namespace "$pins" --wait=false >/dev/null

pinned="Canyon's namespaces generator pins each Kustomization"
fields="Canyon's namespaces generator sets no Kustomization field but"
denied "a Kustomization with force" "$pinned" "$(kustomization canyon-sync canyon-x-preview preview canyon-preview '  force: true
')"
denied "a Kustomization with suspend" "$fields" "$(kustomization canyon-sync canyon-x-preview preview canyon-preview '  suspend: true
')"
denied "a Kustomization with postBuild" "$fields" "$(kustomization canyon-sync canyon-x-preview preview canyon-preview '  postBuild:
    substitute:
      app: probe
')"
denied "canyon-x-prod's Kustomization as canyon-preview" "$pinned" "$(kustomization canyon-sync canyon-x-prod prod canyon-preview)"
denied "a Kustomization as default" "$pinned" "$(kustomization canyon-sync canyon-x-preview preview default)"
denied "a Kustomization into another namespace" "$pinned" "$(kustomization canyon-sync canyon-x-preview preview canyon-preview | sed 's/targetNamespace: canyon-x-preview/targetNamespace: canyon-y-preview/')"
# Each pinned field varied alone, so each clause has a case of its own.
ok=$(kustomization canyon-sync canyon-x-preview preview canyon-preview)
denied "a Kustomization as the other environment's account" "$pinned" "$(sed 's/serviceAccountName: canyon-preview/serviceAccountName: canyon-prod/' <<<"$ok")"
denied "a Kustomization of the other environment's GitRepository" "$pinned" "$(sed 's/    name: canyon-preview/    name: canyon-prod/' <<<"$ok")"
denied "a Kustomization of a GitRepository in flux-system" "$pinned" "$(sed 's|^    name: canyon-preview$|    name: canyon-preview\n    namespace: flux-system|' <<<"$ok")"
denied "a Kustomization of another app's directory" "$pinned" "$(sed 's|path: ./apps/x/preview|path: ./apps/y/preview|' <<<"$ok")"
denied "a Kustomization of the other environment's directory" "$pinned" "$(sed 's|path: ./apps/x/preview|path: ./apps/x/prod|' <<<"$ok")"
denied "a Kustomization with prune false" "$pinned" "$(sed 's/prune: true/prune: false/' <<<"$ok")"
denied "a Kustomization with deletionPolicy Delete" "$pinned" "$(sed 's/deletionPolicy: Orphan/deletionPolicy: Delete/' <<<"$ok")"
forbidden "a Kustomization in flux-system" kubectl create --dry-run=server "${as[@]}" -f - <<<"$(kustomization flux-system canyon-x-preview preview canyon-preview)"

# The sync accounts, as kustomize-controller impersonates them: canyon-deploy in their own environment's namespaces,
# and nothing anywhere else beyond what every account has.
for env in preview prod; do
  other=prod
  [ "$env" = preview ] || other=preview
  account=(--as="system:serviceaccount:canyon-sync:canyon-$env" --as-group=system:serviceaccounts --as-group=system:serviceaccounts:canyon-sync --as-group=system:authenticated)
  nobody=(--as=system:serviceaccount:canyon-sync:gitops-check-nobody --as-group=system:serviceaccounts --as-group=system:serviceaccounts:canyon-sync --as-group=system:authenticated)
  own=canyon-rbac-probe-$env
  rules() { kubectl auth can-i --list -n "$1" "${@:2}" | awk '{$1 = $1; print}' | sort; } # columns padded to their widest, squeezed
  extra=$(comm -23 <(rules "$own" "${account[@]}") <(rules "$own" "${nobody[@]}") | awk '{print $1, $NF}')
  want="clusters.postgresql.cnpg.io [*]
configmaps [*]
deployments.apps [*]
externalsecrets.external-secrets.io [*]
httproutes.gateway.networking.k8s.io [*]
networkpolicies.networking.k8s.io [*]
passwords.generators.external-secrets.io [*]
services [*]"
  before=$failed failed=0
  [ "$(sort <<<"$extra")" = "$want" ] || miss "canyon-$env may, in $own, beyond what every account may:
$extra
want exactly
$want"
  [ -z "$(comm -23 <(rules default "${account[@]}") <(rules default "${nobody[@]}"))" ] || miss "canyon-$env holds rules outside its namespaces: $(comm -23 <(rules default "${account[@]}") <(rules default "${nobody[@]}"))"
  refused "canyon-$env labelling $own canyon.dev/env=$other" "is forbidden" kubectl label namespace "$own" "${account[@]}" --dry-run=server "canyon.dev/env=$other" --overwrite
  [ "$(kubectl auth can-i patch "namespaces/$own" -n "$own" "${account[@]}" || true)" = no ] || miss "canyon-$env may patch its own namespace $own"
  for ns in "canyon-rbac-probe-$other" default kube-system gateway flux-system canyon-sync; do
    [ "$(kubectl auth can-i create deployments -n "$ns" "${account[@]}" || true)" = no ] || miss "canyon-$env may create Deployments in $ns"
  done
  for what in rolebindings secrets kustomizations.kustomize.toolkit.fluxcd.io; do
    [ "$(kubectl auth can-i create "$what" -n "$own" "${account[@]}" || true)" = no ] || miss "canyon-$env may create $what in $own"
  done
  [ "$failed" = 1 ] || echo "canyon-$env: exactly canyon-deploy in $own, nothing elsewhere"
  [ "$before" = 0 ] || failed=1
done
exit "$failed"
