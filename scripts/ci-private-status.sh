#!/bin/sh
# scripts/ci-private-status.sh <pr>: whether the stack suites passed on Canyon's private runners for the pull request's
# head as it is now (CONTRIBUTING.md, Maintainers): the newest private-ci run in CANYON_PRIVATE_REPO named for that
# pull request and that exact commit, as scripts/ci-private.sh starts it, read by its last job, private-ci. It prints
# one line and exits 0 when that job succeeded, 1 when the run failed or was cancelled, 2 while it runs, and 3 when
# there is none for this commit: run scripts/ci-private.sh <pr>, or the head moved since it last ran. Merge on 0 alone.
#
# gh signs in to this repository as GH_CONFIG_DIR says and to the private repository as CANYON_PRIVATE_GH_CONFIG_DIR
# says (GH_CONFIG_DIR's when unset), as scripts/ci-private.sh does. It needs gh and jq.
set -eu
pr=${1:?usage: scripts/ci-private-status.sh <pr>}
public=${CANYON_PUBLIC_REPO:-getcanyon-ai/canyon}
case $pr in '' | *[!0-9]*) echo "ci-private-status: a pull request number, not '$pr'" >&2; exit 64 ;; esac
: "${CANYON_PRIVATE_REPO:?set CANYON_PRIVATE_REPO to the private repository holding private-ci.yml, <owner>/<name>}"
priv() {
  if [ -n "${CANYON_PRIVATE_GH_CONFIG_DIR:-}" ]; then GH_CONFIG_DIR=$CANYON_PRIVATE_GH_CONFIG_DIR gh "$@"; else gh "$@"; fi
}

sha=$(gh pr view "$pr" -R "$public" --json headRefOid --jq .headRefOid)
run=$(priv api "repos/$CANYON_PRIVATE_REPO/actions/workflows/private-ci.yml/runs?event=workflow_dispatch&per_page=100" \
  --jq "[.workflow_runs[] | select(.display_title == \"pr-$pr $sha\")][0] | if . == null then \"\" else \"\\(.id) \\(.status) \\(.html_url)\" end")
if [ -z "$run" ]; then
  echo "none: no private run of #$pr's head $sha"
  exit 3
fi
id=${run%% *}
rest=${run#* }
status=${rest%% *}
url=${rest#* }
if [ "$status" != completed ]; then
  echo "running: #$pr at $sha is $status on the private runners, $url"
  exit 2
fi
gate=$(priv api "repos/$CANYON_PRIVATE_REPO/actions/runs/$id/jobs?filter=latest&per_page=100" --jq '[.jobs[] | select(.name == "private-ci")][0].conclusion // "none"')
if [ "$gate" = success ]; then
  echo "green: #$pr at $sha passed on the private runners, $url"
  exit 0
fi
echo "red: #$pr at $sha ended with private-ci $gate on the private runners, $url"
exit 1
