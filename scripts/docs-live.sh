#!/bin/sh
# scripts/docs-live.sh: the public docs site names the latest release, from 24 hours after it was published (a person
# publishes the docs, in another repository: docs/README.md, Publishing). Reads public pages and GitHub's public API
# alone. v0.11.0's docs were never published, and nothing said so.
set -eu
site=${DOCS_URL:-https://www.getcanyon.ai/docs/}
release=$(gh api repos/getcanyon-ai/canyon-releases/releases/latest --jq '[.tag_name, (now - (.published_at | fromdateiso8601) | floor)] | @tsv')
tag=${release%%	*}
age=${release##*	}
grace=${DOCS_GRACE:-86400} # seconds after a release is published before its docs must be live
docs=$(curl -fsS --max-time 30 "$site" | grep -o 'Docs for Canyon v[0-9][0-9A-Za-z.-]*' | head -1) || true
[ -n "$docs" ] || docs="no version" # a site that doesn't answer names none
if [ "$docs" = "Docs for Canyon $tag" ]; then
  echo "docs-live: $site names $tag"
elif [ "$age" -lt "$grace" ]; then
  echo "docs-live: $site says \"$docs\"; $tag was published $((age / 3600)) h ago, so it has until $((grace / 3600)) h"
else
  echo "docs-live: $site says \"$docs\", and $tag was published $((age / 3600)) h ago: publish its docs (docs/README.md, Publishing)" >&2
  exit 1
fi
