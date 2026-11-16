#!/usr/bin/env bash
# The k3s guide's gateway (docs: self-host/k3s.md, steps 5 and 7) on the kind cluster with Calico that
# backup.sh leaves, its app restore-check restored: the guide's own blocks, by title (scripts/guide-block.sh).
# The server signs people in (oidc at a public issuer it can discover, so it is Ready behind its Service;
# nothing here signs in) and trusts the guide's CADDY_IP alone; the guide's Caddy runs pinned there. The
# checks: Calico reserves CADDY_IP, so no other pod is handed it; Caddy's pod has it; and the server's
# /tls/ask, asked through the Service the guide's Caddyfile names, answers Caddy yes for an app that exists,
# the front-door check's two addresses and Canyon's host, no for a made-up name, and 403 to any other pod.
# kind issues no public certificate, so the guide's check on a real host (a certificate for an app, none
# for a made-up name) is not here.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
guide=$root/docs/src/content/docs/self-host/k3s.md
block() { echo "== the guide's block: $1"; bash -euo pipefail -c "$("$root/scripts/guide-block.sh" "$guide" "$1")"; }
export DOMAIN=canyon.kind.test WORK
WORK=$(mktemp -d)
CADDY_IP=$("$root/scripts/guide-block.sh" "$guide" 'Create the Secrets' | awk -F'[= ]' '$1 == "CADDY_IP" {print $2}')
[ -n "$CADDY_IP" ] || { echo "::error::the guide's 'Create the Secrets' sets no CADDY_IP"; exit 1; }
export CADDY_IP

echo "== the server signs people in at https://$DOMAIN and trusts $CADDY_IP/32 alone, as step 5's Secret says"
old=$(kubectl -n canyon get pod -l app.kubernetes.io/name=canyon-server -o jsonpath='{.items[0].metadata.name}')
kubectl -n canyon patch secret canyon-server -p "{\"stringData\": {\"CANYON_AUTH\": \"oidc\", \"CANYON_PUBLIC_URL\": \"https://$DOMAIN\",
  \"CANYON_OIDC_ISSUER\": \"https://accounts.google.com\", \"CANYON_OIDC_CLIENT_ID\": \"canyon\", \"CANYON_ADMINS\": \"admin@example.com\",
  \"CANYON_TRUSTED_PROXIES\": \"$CADDY_IP/32\"}}"
kubectl -n canyon rollout restart deploy/canyon-server
kubectl -n canyon wait --for=delete pod/"$old" --timeout=5m
kubectl -n canyon rollout status deploy/canyon-server --timeout=10m # Ready once it has discovered its issuer
server=canyon-server.canyon.svc.cluster.local # the Service, as the guide's Caddyfile asks it

block 'Run the gateway'

failed=0
level=$(kubectl get namespace canyon-gateway -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')
if [ "$level" != restricted ]; then
  echo "::error::step 7 leaves canyon-gateway at Pod Security ${level:-none}, not restricted, as the chart's"; failed=1
fi
reserved=$(kubectl get ipreservations.crd.projectcalico.org -o jsonpath='{.items[*].spec.reservedCIDRs[*]}' 2>/dev/null || true)
if ! tr ' ' '\n' <<<"$reserved" | grep -qxF "$CADDY_IP/32"; then
  echo "::error::Calico reserves no $CADDY_IP/32 (reserved: ${reserved:-none}): IPAM may hand Caddy's address to another pod"; failed=1
fi
pod=$(kubectl -n canyon-gateway get pod -l app=caddy -o jsonpath='{.items[0].status.podIP}')
if [ "$pod" != "$CADDY_IP" ]; then
  echo "::error::Caddy's pod is at $pod, not the $CADDY_IP the server trusts: step 7 does not pin it"; failed=1
fi
image=$(kubectl -n canyon-gateway get deploy caddy -o jsonpath='{.spec.template.spec.containers[0].image}')
kubectl -n canyon-gateway run other --image="$image" --restart=Never --overrides='{"spec": {"securityContext": {"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
  "containers": [{"name": "other", "image": "'"$image"'", "command": ["sleep", "600"], "securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]}}}]}}'
kubectl -n canyon-gateway wait --for=condition=Ready pod/other --timeout=3m
ask() { # the status /tls/ask answers a pod for a name, 000 when nothing answered
  local code
  code=$(kubectl -n canyon-gateway exec "$1" -- wget -q -O /dev/null -S "http://$server:7777/tls/ask?domain=$2" 2>&1 |
    awk '$1 ~ /^HTTP\// {code = $2} END {print code}' || true)
  echo "${code:-000}"
}
for _ in $(seq 60); do [ "$(ask deploy/caddy "$DOMAIN")" != 000 ] && break; sleep 2; done
for c in "restore-check.preview.$DOMAIN 200" "restore-check.prod.$DOMAIN 200" "canyon-probe.preview.$DOMAIN 200" "canyon-probe.prod.$DOMAIN 200" \
  "$DOMAIN 200" "made-up.preview.$DOMAIN 404" "restore-check.staging.$DOMAIN 404"; do
  read -r name want <<<"$c"
  got=$(ask deploy/caddy "$name")
  echo "Caddy asks for $name: $got"
  if [ "$got" != "$want" ]; then echo "::error::Caddy's ask for $name: $got, want $want"; failed=1; fi
done
got=$(ask other "restore-check.preview.$DOMAIN")
echo "another pod asks for restore-check.preview.$DOMAIN: $got"
if [ "$got" != 403 ]; then echo "::error::another pod's ask: $got, want 403: the server trusts more than Caddy"; failed=1; fi
kubectl -n canyon-gateway delete pod other --wait=false
exit $failed
