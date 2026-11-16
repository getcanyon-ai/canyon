#!/bin/sh
# scripts/guide-block.sh <page.md> <title>: the ```sh title="<title>" block of a docs page, printed as its reader
# copies it, so a check runs the page's own commands (scripts/deploy-bundle-check.sh, e2e/fixtures/gen.sh).
set -eu
page=${1:?usage: scripts/guide-block.sh <page.md> <title>}
title=${2:?usage: scripts/guide-block.sh <page.md> <title>}
out=$(awk -v open="\`\`\`sh title=\"$title\"" '$0 == open {on = 1; next} on && $0 == "```" {exit} on' "$page")
[ -n "$out" ] || { echo "guide-block: $page has no block titled $title" >&2; exit 1; }
printf '%s\n' "$out"
