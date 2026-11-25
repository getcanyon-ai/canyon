#!/usr/bin/env bash
# .github/docs/site-check.sh: what docs.yml's site job checks of the built site, after make docs-site, in one place for the
# two that run it, that job and scripts/ci-suite.sh's docs suite (Canyon's private runners). The caller has set Node up and run
# make docs-site. Each check is a subshell of its own, as each was a step of the job, under -e and pipefail: a check's exit ends
# the script, failed. The last rewrites docs/'s pages.
set -eo pipefail
cd "$(dirname "$0")/../.."
RUNNER_TEMP=${RUNNER_TEMP:-$(mktemp -d)}
export RUNNER_TEMP
# the site fetches no font from Google, every face is self-hosted as the dashboard's are
(
set -eo pipefail
if grep -rIlE 'fonts\.(googleapis|gstatic)\.com' docs/dist; then echo "::error::the built docs load a font from Google: serve it from web/src/fonts instead"; exit 1; fi
)
# every wide mermaid diagram says it scrolls sideways exactly when it does, at 390px and 1280px
(
set -eo pipefail
(cd e2e/ui && npm ci && npx playwright install --with-deps chromium)
node docs/scripts/diagram-hints.mjs
)
# the site names the Canyon version it documents, main's unless a release tag sets it, and publish-docs.sh refuses a HEAD with no release tag
(
set -eo pipefail
set -eu
grep -q 'Docs for Canyon main (unreleased)' docs/dist/index.html || { echo "::error::without CANYON_DOCS_VERSION, dist/index.html doesn't say Docs for Canyon main (unreleased)"; exit 1; }
CANYON_DOCS_VERSION=v0.0.0-check npm --prefix docs run build
grep -q 'Docs for Canyon v0.0.0-check' docs/dist/index.html || { echo "::error::with CANYON_DOCS_VERSION=v0.0.0-check, dist/index.html doesn't say Docs for Canyon v0.0.0-check"; exit 1; }
mono=$(mktemp -d) # a workspace checkout as the script recognises one, its app directory holding a file the script must not touch
touch "$mono/pnpm-workspace.yaml" && mkdir -p "$mono/apps/canyon-docs" && touch "$mono/apps/canyon-docs/kept"
refuses() { # $1: the HEAD it is run on, $2: the words its refusal must say
  if scripts/publish-docs.sh "$mono" 2> "$RUNNER_TEMP/publish.err"; then echo "::error::scripts/publish-docs.sh published from $1"; exit 1; fi
  cat "$RUNNER_TEMP/publish.err"
  grep -qF "$2" "$RUNNER_TEMP/publish.err" || { echo "::error::scripts/publish-docs.sh failed on $1, but didn't say: $2"; exit 1; }
  test -f "$mono/apps/canyon-docs/kept" || { echo "::error::scripts/publish-docs.sh refused $1, but only after it emptied apps/canyon-docs"; exit 1; }
}
refuses 'a HEAD with no release tag' 'carries no release tag'
git tag 'v0.6.0;echo' # vercel.json's build command is a shell command: a tag that isn't a version never reaches it
refuses 'a HEAD tagged v0.6.0;echo' 'v0.6.0;echo is not a release tag'
git tag -d 'v0.6.0;echo'
# a release HEAD with a page base-links cannot rewrite (a link: escaped in its YAML, never in its text):
# the refusal comes before the workspace's copy is touched (#2869)
page=docs/src/content/docs/zz-unrewritable.md
printf -- '---\ntitle: Unrewritable\nhero:\n  actions:\n    - text: Go\n      link: "/x\\x79"\n---\n' > "$page" && git add "$page"
git tag v0.0.0-check
refuses 'a page base-links cannot rewrite' "can't find link: /xy"
git tag -d v0.0.0-check && git rm -q -f "$page"
)
# under /docs, where scripts/publish-docs.sh publishes it, the site builds and every link stays under the base
(
set -eo pipefail
set -eu
node docs/scripts/base-links.mjs docs /docs
CANYON_DOCS_BASE=/docs npm --prefix docs run build # starlight-links-validator fails on a link to a page the rewrite missed
stray=$(grep -rhoE '(href|src)="/[^"]*"' docs/dist --include='*.html' | grep -vE '="/(docs/|docs"|/)' | sort -u) || true
[ -z "$stray" ] || { echo "::error::under CANYON_DOCS_BASE=/docs, the built pages link outside it:"; echo "$stray"; exit 1; }
grep -q 'href="/docs/start/try-it-on-your-laptop/"' docs/dist/index.html || { echo "::error::under CANYON_DOCS_BASE=/docs, the home page's hero doesn't link /docs/start/try-it-on-your-laptop/"; exit 1; }
)
