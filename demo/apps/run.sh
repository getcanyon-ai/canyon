#!/bin/sh
# The demo's sample apps (#1490): deployed through Canyon like any app, so the fleet shows real running
# ones. The first start builds them, and Canyon's build image before them, which takes the most of its
# minutes (#2697); later starts find them built (no build value changed).
# It runs only under docker-compose.yml's demo-apps profile (canyon demo up; make demo in a checkout).
set -eu
demo=$(dirname "$0") # the apps sit beside this script: /demo in the image, demo/apps in the tree
host=http://localhost:7777 # the server's own network namespace (docker-compose.yml): sign-in off answers a loopback name alone
# again is a deploy that failed, a first one past its ceiling on a slow line among them: the next start carries on,
# Docker keeping what it downloaded (#2697).
carry_on="To carry on, start the demo again (canyon demo up, or make demo in a checkout)."
again() {
  code=$?
  echo "$carry_on"
  exit "$code"
}
until canyon fleet --host "$host" >/dev/null 2>&1; do sleep 2; done # the server answers once it is ready
canyon login "$host" >/dev/null # keeps the host's public URL, so the deploys below link the app's page where the host's browser opens it, not at server:7777 (#2019)
echo "Building the sample apps: claims-portal, endorsement-hub, claims-archive and premium-ledger."
echo "A first start takes 20 to 30 minutes at 25 Mbit/s, longer on a slower line: it downloads about 3.5 GB, most of it Canyon's build image and the scanners' databases (and the databases again after canyon demo down). The executor-1 lines below show how it's going. Once everything is here, a start takes 5 to 10 minutes."
for app in claims-portal endorsement-hub claims-archive premium-ledger; do
  (cd "$demo/$app" && canyon deploy --force --host "$host") || again # --force: it replaces the seeded record of the app, which no copy started from
  echo "canyon-demo: running $app" # canyon demo up says each as a line of progress (cli/demo_up.go, demoReadyPrefix)
done
for app in claims-archive premium-ledger; do
  (cd "$demo/$app" && canyon deploy --env prod --yes --host "$host") || again
done
# claims-portal's next version builds its SQL from the request, so the review blocks it (#1636): exit 2, by design.
code=0; (cd "$demo/claims-portal-next" && canyon deploy --force --host "$host") || code=$?
[ "$code" = 2 ] || { echo "claims-portal's next version was meant to be blocked at review; canyon deploy exited $code"; [ "$code" = 0 ] || echo "$carry_on"; exit 1; }
# The company's design system, which the tour's first chapter has the visitor make mandatory (cli.md §6.14.1):
# Canyon's, picked as the demo's platform team would pick it in the dashboard, once the sample apps, which predate it,
# run. design.org-system stays Advisory until the visitor sets it. Canyon's id is the one migration 0013 ships it
# with. Sign-in is off: no session.
curl -fsS -o /dev/null -X PUT -H 'Content-Type: application/json' -d '{"design_system_id":"ds_0002"}' "$host/api/v1/preferences" ||
  { echo "The demo's design system could not be set: PUT /api/v1/preferences failed."; echo "$carry_on"; exit 1; }
# The org's playbook on how the claims apps are written, which the tour's security chapter has the agent read before
# its fix (design/specs/mcp.md §11): saved once, so a later start adds no version of its own. Sign-in is off: no session.
playbook="$host/api/v1/playbooks/claims-portal-conventions"
case $(curl -sS -o /dev/null -w '%{http_code}' "$playbook") in
  200) ;;
  404) curl -fsS -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary "@$demo/claims-portal-conventions.json" "$playbook" ||
    { echo "The demo's playbook could not be saved: PUT /api/v1/playbooks/claims-portal-conventions failed."; echo "$carry_on"; exit 1; } ;;
  *) echo "The demo's playbook could not be read: GET /api/v1/playbooks/claims-portal-conventions failed."; echo "$carry_on"; exit 1 ;;
esac
echo "The sample apps are running, and claims-portal's next version is blocked at review."
