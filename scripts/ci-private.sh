#!/bin/sh
# scripts/ci-private.sh <pr>: one of our own pull requests' stack suites on Canyon's private runners instead of
# GitHub's (CONTRIBUTING.md, Maintainers). It starts the trusted workflow private-ci.yml of the private repository
# CANYON_PRIVATE_REPO (<owner>/<name>) by workflow_dispatch, with the pull request's number and its exact head commit,
# which that workflow checks again and fetches as plain files: nothing of the pull request's .github runs there, and
# this pushes no git ref anywhere. Then it labels the pull request ci:private, so its stack suites here skip as a
# suite its change doesn't reach skips (.github/suites/reach.sh), and re-runs this commit's runs that have a paths
# job, so a run already going stops using GitHub's runners. It prints the private run's address.
#
# scripts/ci-private.sh --off <pr>: the fallback, when the private runners are down: the label comes off, and the same
# runs here run again, every suite on GitHub's runners as before.
#
# Refused: a closed pull request, a fork's, and one whose author is not in CANYON_PRIVATE_AUTHORS (logins, spaces
# between). Each new push to the pull request needs another run of this script; the label stays.
#
# gh signs in to this repository as GH_CONFIG_DIR says (its default when unset), and to the private repository as
# CANYON_PRIVATE_GH_CONFIG_DIR says (the same when unset): two accounts may hold the two. It needs gh and jq.
set -eu
off=false
if [ "${1:-}" = --off ]; then off=true; shift; fi
pr=${1:?usage: scripts/ci-private.sh [--off] <pr>}
public=${CANYON_PUBLIC_REPO:-getcanyon-ai/canyon}
label=ci:private
fail() { echo "ci-private: $*" >&2; exit 1; }
case $pr in '' | *[!0-9]*) fail "a pull request number, not '$pr'" ;; esac
priv() {
  if [ -n "${CANYON_PRIVATE_GH_CONFIG_DIR:-}" ]; then GH_CONFIG_DIR=$CANYON_PRIVATE_GH_CONFIG_DIR gh "$@"; else gh "$@"; fi
}

info=$(gh pr view "$pr" -R "$public" --json state,isCrossRepository,headRefOid,author)
sha=$(printf '%s' "$info" | jq -r .headRefOid)

# rerun: every run of this commit here, for a pull request, whose workflow has a paths job, cancelled first when it is
# still going: its paths job reads the pull request's labels again, so its stack suites follow the label as it is now.
rerun() {
  gh api "repos/$public/actions/runs?head_sha=$sha&event=pull_request&per_page=100" --jq '.workflow_runs[] | "\(.id) \(.status)"' |
    while read -r id status; do
      gh api "repos/$public/actions/runs/$id/jobs?filter=latest&per_page=100" --jq '.jobs[].name' | grep -qx paths || continue
      if [ "$status" != completed ]; then
        gh run cancel "$id" -R "$public" >/dev/null
        while [ "$(gh api "repos/$public/actions/runs/$id" --jq .status)" != completed ]; do sleep 5; done
      fi
      gh run rerun "$id" -R "$public" >/dev/null
      echo "re-ran $(gh api "repos/$public/actions/runs/$id" --jq '.name + " " + .html_url')"
    done
}

if [ "$off" = true ]; then
  gh pr edit "$pr" -R "$public" --remove-label "$label" >/dev/null
  rerun
  echo "#$pr runs its stack suites on GitHub's runners again"
  exit 0
fi

: "${CANYON_PRIVATE_REPO:?set CANYON_PRIVATE_REPO to the private repository holding private-ci.yml, <owner>/<name>}"
: "${CANYON_PRIVATE_AUTHORS:?set CANYON_PRIVATE_AUTHORS to the logins whose pull requests run there}"
[ "$(printf '%s' "$info" | jq -r .state)" = OPEN ] || fail "#$pr is not open"
if [ "$(printf '%s' "$info" | jq -r .isCrossRepository)" != false ]; then
  fail "#$pr comes from a fork: a fork's code never runs on the private runners"
fi
author=$(printf '%s' "$info" | jq -r 'if .author.is_bot then "" else .author.login end')
case " $CANYON_PRIVATE_AUTHORS " in
  *" $author "*) [ -n "$author" ] || fail "#$pr was opened by a bot" ;;
  *) fail "#$pr was opened by $author, whose pull requests don't run on the private runners" ;;
esac

# GitHub answers a dispatch with the run it made (its REST API from 2026-03-10); an older answer is empty
answer=$(priv api -X POST -H 'X-GitHub-Api-Version: 2026-03-10' "repos/$CANYON_PRIVATE_REPO/actions/workflows/private-ci.yml/dispatches" \
  -f ref=main -f "inputs[pr]=$pr" -f "inputs[sha]=$sha")
run=$(printf '%s' "$answer" | jq -r '.html_url? // empty' 2>/dev/null || true)

# the label's first use makes it: one that is there already answers an error, and the edit below says any other
# (GitHub takes a description of at most 100 characters)
gh label create "$label" -R "$public" --color 5319e7 \
  --description "Stack suites run on Canyon's private runners (scripts/ci-private.sh)" >/dev/null 2>&1 || true
gh pr edit "$pr" -R "$public" --add-label "$label" >/dev/null
rerun
echo "private run of #$pr at $sha: ${run:-started; scripts/ci-private-status.sh $pr finds it}"
