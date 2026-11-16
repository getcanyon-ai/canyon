#!/bin/sh
# scripts/publish-docs.sh <workspace>: writes the docs site into <workspace>/apps/canyon-docs, a checkout
# of the pnpm workspace whose Vercel project builds that directory, served at https://www.getcanyon.ai/docs (the
# website rewrites /docs to it), and refreshes the workspace's pnpm lockfile for it. Run it by hand at each
# release, from a clone of Canyon at the release, with pnpm on PATH and docs/'s packages installed (npm ci); then
# open a pull request on the workspace's repository with the change. It refuses a HEAD with no version tag (v1.2.3, v1.2.3-rc.1): the tag is the version every page names (CANYON_DOCS_VERSION in
# vercel.json, read by docs/astro.config.mjs). The copy is docs/'s tracked files with its links followed (src/fonts, contracts/), less
# README.md, this repository's maintainer guide, scripts/screens.mjs, which needs the e2e harness, and
# package-lock.json: the workspace installs with pnpm. scripts/base-links.mjs then puts the base in the copy's links, before
# the copy replaces the workspace's.
set -eu
mono=${1:?usage: scripts/publish-docs.sh <path to a checkout of the pnpm workspace that serves the site>}
[ -f "$mono/pnpm-workspace.yaml" ] || { echo "publish-docs: $mono has no pnpm-workspace.yaml: not a pnpm workspace" >&2; exit 1; }
mono=$(cd "$mono" && pwd -P)
out=$mono/apps/canyon-docs
# Where the site is served: the build's CANYON_DOCS_URL and CANYON_DOCS_BASE (docs/astro.config.mjs).
url=https://www.getcanyon.ai
base=/docs
cd "$(dirname "$0")/../docs"
version=$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null) ||
  { echo "publish-docs: HEAD carries no release tag (v*): publish from a clone of Canyon at the release" >&2; exit 1; }
# vercel.json's build command is a shell command, and a tag may hold ; $( ` & or |: only a version gets in.
printf '%s' "$version" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?' || { echo "publish-docs: $version is not a release tag" >&2; exit 1; }

[ -d node_modules/micromark ] || { echo "publish-docs: docs/ has no node_modules: run npm ci in docs/ first" >&2; exit 1; }

# The copy is made and its links rewritten aside, and only then replaces the workspace's: a page base-links
# cannot rewrite refuses the publish with apps/canyon-docs as it was.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git ls-files -z -- . ':!README.md' ':!scripts/screens.mjs' ':!package-lock.json' | tar --null -T - -chf - | tar -xf - -C "$tmp"
node scripts/base-links.mjs "$tmp" "$base"
rm -rf "$out"
mkdir -p "$(dirname "$out")"
mv "$tmp" "$out"
chmod 755 "$out"

# Astro writes a site with a base at dist/'s root, and Vercel serves the output directory at the deployment's root:
# dist-vercel$base/ puts each file at the path its URLs name, the path the website's /docs rewrite asks for. The old
# address, docs.getcanyon.ai, redirects there once that domain is added to the Vercel project.
cat > "$out/vercel.json" <<EOF
{
  "framework": "astro",
  "installCommand": "corepack enable && corepack prepare pnpm@9.0.0 --activate && pnpm install --filter canyon-docs... --frozen-lockfile",
  "buildCommand": "CANYON_DOCS_URL=$url CANYON_DOCS_BASE=$base CANYON_DOCS_VERSION=$version pnpm run build && rm -rf dist-vercel && mkdir -p dist-vercel && cp -R dist dist-vercel$base",
  "outputDirectory": "dist-vercel",
  "redirects": [
    {
      "source": "/:path*",
      "has": [{ "type": "host", "value": "docs.getcanyon.ai" }],
      "destination": "$url$base/:path*",
      "permanent": true
    },
    {
      "source": "/",
      "destination": "$base/",
      "permanent": false
    }
  ]
}
EOF
cat > "$out/agent.md" <<'EOF'
---
title: "apps/canyon-docs agent guide"
type: app-agent
status: generated
---

# apps/canyon-docs

Canyon's product docs, an Astro Starlight site that Vercel builds from this directory, served at
`https://www.getcanyon.ai/docs`: canyon-web rewrites `/docs` to this deployment (settings in `vercel.json`).

**Generated: don't edit here.** Everything in this directory, this file included, is a copy of the
`docs/` directory of Canyon's product repository, written by that
repository's `scripts/publish-docs.sh` at each Canyon release. An edit here is lost at the next
release: change the docs in Canyon's `docs/` and publish again.

Build it: `pnpm install --filter canyon-docs... --frozen-lockfile && pnpm --filter canyon-docs build`
writes the site to `dist/`. Set `CANYON_DOCS_BASE=/docs` for the build the site serves: the pages' links
already start with `/docs`.
EOF

cd "$mono"
pnpm install --lockfile-only
