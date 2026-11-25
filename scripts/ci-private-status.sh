#!/bin/sh
# scripts/ci-private-status.sh <pr>: whether the stack suites passed on Canyon's private runners for the pull request's
# head as it is now (CONTRIBUTING.md, Maintainers). It counts only a run of .github/workflows/private-ci.yml as main
# holds it, started by workflow_dispatch, whose verify job's log says it was asked for this pull request at this
# exact commit (the line private-ci-inputs pr=<pr> sha=<sha>): never the run's name, which a run of another branch's
# copy of the workflow could fake. Of those, the newest decides, by its last job, private-ci. It prints one line and
# exits 0 when that job succeeded, 1 when the run failed or was cancelled, 2 while it runs, and 3 when there is none
# for this commit: run scripts/ci-private.sh <pr>, or the head moved since it last ran. Merge on 0 alone. The line
# says how many suites ran and which ones scripts/ci-select.sh skipped, as the run's select job's log says it.
#
# gh signs in to this repository as GH_CONFIG_DIR says and to the private repository CANYON_PRIVATE_REPO as
# CANYON_PRIVATE_GH_CONFIG_DIR says (GH_CONFIG_DIR's when unset), as scripts/ci-private.sh does. It needs gh and jq.
set -eu
pr=${1:?usage: scripts/ci-private-status.sh <pr>}
public=${CANYON_PUBLIC_REPO:-getcanyon-ai/canyon}
case $pr in '' | *[!0-9]*) echo "ci-private-status: a pull request number, not '$pr'" >&2; exit 64 ;; esac
: "${CANYON_PRIVATE_REPO:?set CANYON_PRIVATE_REPO to the private repository holding private-ci.yml, <owner>/<name>}"
priv() {
  if [ -n "${CANYON_PRIVATE_GH_CONFIG_DIR:-}" ]; then GH_CONFIG_DIR=$CANYON_PRIVATE_GH_CONFIG_DIR gh "$@"; else gh "$@"; fi
}

sha=$(gh pr view "$pr" -R "$public" --json headRefOid --jq .headRefOid)
# newest first: every run of the trusted file on main by dispatch; the name only narrows which logs are read
candidates=$(priv api "repos/$CANYON_PRIVATE_REPO/actions/workflows/private-ci.yml/runs?event=workflow_dispatch&branch=main&per_page=100" \
  --jq '.workflow_runs[] | select(.path == ".github/workflows/private-ci.yml" and .head_branch == "main" and .event == "workflow_dispatch") | "\(.id) \(.status) \(.html_url)"')
want="private-ci-inputs pr=$pr sha=$sha"
found=""
for id in $(printf '%s\n' "$candidates" | cut -d' ' -f1); do
  verify=$(priv api "repos/$CANYON_PRIVATE_REPO/actions/runs/$id/jobs?filter=latest&per_page=100" --jq '[.jobs[] | select(.name == "verify")][0].id // empty')
  [ -n "$verify" ] || continue
  # the job's log, each line after its timestamp: the one line the verify step prints, exactly. A log holds terminal
  # escapes (its colours), which gh prints only when asked; read in full first, so grep stopping early can't fail gh.
  log=$(priv api --allow-escape-sequences "repos/$CANYON_PRIVATE_REPO/actions/jobs/$verify/logs" 2>/dev/null) || continue # a log GitHub no longer keeps: not this run
  if printf '%s\n' "$log" | sed 's/^[^ ]* //' | grep -qxF "$want"; then
    found=$(printf '%s\n' "$candidates" | grep "^$id ")
    break
  fi
done
if [ -z "$found" ]; then
  echo "none: no private run of #$pr's head $sha"
  exit 3
fi
id=${found%% *}
rest=${found#* }
status=${rest%% *}
url=${rest#* }
if [ "$status" != completed ]; then
  echo "running: #$pr at $sha is $status on the private runners, $url"
  exit 2
fi
jobs=$(priv api "repos/$CANYON_PRIVATE_REPO/actions/runs/$id/jobs?filter=latest&per_page=100")
gate=$(printf '%s' "$jobs" | jq -r '[.jobs[] | select(.name == "private-ci")][0].conclusion // "none"')
# what ran: the one line the select job prints, private-ci-selected ran=<n> of=<all> skipped=<suite,...|none>
ran=""
selector=$(printf '%s' "$jobs" | jq -r '[.jobs[] | select(.name == "select")][0].id // empty')
if [ -n "$selector" ] && log=$(priv api --allow-escape-sequences "repos/$CANYON_PRIVATE_REPO/actions/jobs/$selector/logs" 2>/dev/null); then
  line=$(printf '%s\n' "$log" | sed 's/^[^ ]* //' | grep -m1 '^private-ci-selected ran=[0-9]* of=[0-9]* skipped=[a-z0-9,-]*$' || true)
  if [ -n "$line" ]; then
    set -- $line
    ran=" (${2#ran=} of ${3#of=} suites ran; skipped by scripts/ci-select.sh: $(printf '%s' "${4#skipped=}" | sed 's/,/, /g'))"
  fi
fi
if [ "$gate" = success ]; then
  echo "green: #$pr at $sha passed on the private runners$ran, $url"
  exit 0
fi
echo "red: #$pr at $sha ended with private-ci $gate on the private runners$ran, $url"
exit 1
