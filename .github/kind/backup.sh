#!/usr/bin/env bash
# One k3s host's backup and restore (docs: self-host/backup-and-upgrade.md, On one k3s host) on the kind cluster
# `up.sh calico` stands up, run as the guide's reader runs it: its own blocks, by title (scripts/guide-block.sh).
# First the server moves onto both Postgres servers inside the cluster, in canyon-infra, as k3s.md's step 3 makes
# them, and an app gets a secret and a version. Then the guide's backup; then everything a new host would not
# have goes: the server's volume, both databases with their volumes, and the four Secrets; then the guide's
# Secrets, the databases and the server come back empty, and the guide's restore runs. The checks: the server
# starts on the restored key (it refuses rows sealed under another), serves the egress policy under the
# certificate it served before the backup, whose private key is sealed in its vault, and answers the app's
# secret and code as they were.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
guide=$root/docs/src/content/docs/self-host/backup-and-upgrade.md
block() { echo "== the guide's block: $1"; bash -euo pipefail -c "$("$root/scripts/guide-block.sh" "$guide" "$1")"; }
export CANYON=$root # the unpacked release in k3s.md, whose deploy/k8s is this tree's
work=$(mktemp -d) && cd "$work"
export WORK=$work # the guide's $WORK, where step 3 writes its manifests

# k3s.md's step 3, run as its reader runs it: the registry and both databases, with the guide's Service address for the
# registry moved into kind's Service range, the one line kind can't take as the guide writes it.
steps=$root/docs/src/content/docs/self-host/k3s.md
step3() {
  echo "== k3s.md's block: $1"
  bash -euo pipefail -c "$("$root/scripts/guide-block.sh" "$steps" "$1" | sed 's/clusterIP: 10\.43\.254\.10$/clusterIP: 10.96.254.10/')"
}
databases() { step3 'Run the registry and both databases'; }
server() { kubectl -n canyon rollout status deploy/canyon-server --timeout=5m && "$root/scripts/telemetry-off-check.sh" kube; }
publish() { # the install step that publishes the certificate the server serves the gateway's check under
  local cert
  cert=$(kubectl -n canyon exec deploy/canyon-server -c server -- /canyon-server egress-policy-cert)
  kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
}
pinned() { kubectl -n canyon get configmap canyon-egress-policy-cert -o jsonpath='{.data.tls\.crt}'; }
api=http://localhost:17777/api/v1
forward() { # the server's API under CANYON_AUTH=demo, which answers its pod's loopback alone: a port-forward arrives there
  kubectl -n canyon port-forward deploy/canyon-server 17777:7777 >/dev/null 2>&1 & pf=$!
  for _ in $(seq 30); do curl -sf http://localhost:17777/ready >/dev/null && return; sleep 1; done
  echo "::error::no port-forward to the server"; return 1
}

echo "== the server moves onto both Postgres servers in canyon-infra, as k3s.md's steps 3, 5 and 6 make them"
step3 'Make canyon-infra'
databases
pw() { kubectl -n canyon-infra get secret "$1" -o jsonpath='{.data.password}' | base64 -d; }
kubectl -n canyon patch networkpolicy canyon-server --type=json -p '[
  {"op": "replace", "path": "/spec/egress/3/to/0/ipBlock/cidr", "value": "192.168.0.0/16"},
  {"op": "replace", "path": "/spec/egress/4/to/0/ipBlock/cidr", "value": "192.168.0.0/16"}]'
kubectl -n canyon create secret generic canyon-server --dry-run=client -o yaml \
  --from-literal=CANYON_DATABASE_URL="postgres://canyon:$(pw canyon-postgres)@canyon-postgres.canyon-infra.svc.cluster.local:5432/canyon?sslmode=disable" \
  --from-literal=CANYON_APP_DATABASE_URL="postgres://canyon_admin:$(pw app-postgres)@app-postgres.canyon-infra.svc.cluster.local:5433/postgres?sslmode=disable" \
  --from-literal=CANYON_TELEMETRY=0 \
  --from-literal=CANYON_AUTH=demo | kubectl apply -f -
kubectl -n canyon rollout restart deploy/canyon-server
server
publish
before=$(pinned)

echo "== an app with a secret and a version"
forward
curl -sSf -X POST "$api/projects" -H 'Content-Type: application/json' -d '{"id":"restore-check"}' >/dev/null
curl -sSf -X PUT "$api/projects/restore-check/secrets/GREETING" -H 'Content-Type: application/json' -d '{"value":"set before the backup"}' >/dev/null
mkdir code && echo "written before the backup" > code/hello.txt && tar -C code -cf code.tar hello.txt
curl -sSf -X POST "$api/projects/restore-check/packages" --data-binary @code.tar >/dev/null
kill $pf

block 'Back up one k3s host'
ls -l backup

echo "== a new host: no server volume, no databases, no Secrets"
kubectl -n canyon scale deploy/canyon-server --replicas=0
kubectl -n canyon wait --for=delete pod -l app.kubernetes.io/name=canyon-server --timeout=5m
kubectl -n canyon delete pvc canyon-server --wait
kubectl -n canyon delete secret canyon-server canyon-egress
kubectl delete namespace canyon-infra --wait

block 'Recreate the Secrets'
databases
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: canyon-server, namespace: canyon}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
EOF
kubectl -n canyon scale deploy/canyon-server --replicas=1
server # empty: a new key, a new certificate

block 'Restore one k3s host'

echo "== the checks"
failed=0
after=$(pinned)
if [ "$after" != "$before" ]; then
  echo "::error::the server serves another certificate than before the backup: its database or its key did not come back"; failed=1
fi
# The gateway's check, as up.sh asks it: a pod of the gateway's account, its token and the published certificate pinned.
kubectl -n canyon delete pod canyon-egress-policy-check --ignore-not-found --wait
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
got=$(kubectl -n canyon exec canyon-egress-policy-check -- sh -c "curl -sS -o /dev/null -w '%{http_code}' --cacert /var/run/canyon-egress-policy/tls.crt \
  -H \"Authorization: Bearer \$(cat /var/run/canyon-egress-policy/token)\" 'https://canyon-server.canyon.svc:7782/egress/policy?have='" || true)
if [ "$got" != 200 ]; then
  echo "::error::the egress policy under the restored certificate: $got, want 200 — the vault did not open its key"; failed=1
fi
kubectl -n canyon delete pod canyon-egress-policy-check --wait=false
forward
secrets=$(curl -sSf "$api/projects/restore-check/secrets" || true)
echo "the app's secrets: $secrets"
if ! grep -q '"GREETING"' <<<"$secrets"; then
  echo "::error::the app's secret GREETING is not back"; failed=1
fi
mkdir pulled
if ! curl -sSf "$api/projects/restore-check/packages/current" | tar -x -C pulled || [ "$(cat pulled/hello.txt 2>/dev/null)" != "written before the backup" ]; then
  echo "::error::the app's code is not back"; failed=1
fi
kill $pf
exit $failed
