#!/usr/bin/env bash
# For each suite named, whether this pull request's change reaches it (.github/suites/README.md): "<suite>=true" or
# "<suite>=false" on stdout, and into $GITHUB_OUTPUT when Actions sets it. The change is HEAD^1..HEAD, a pull
# request's merge commit against its base (the checkout's fetch-depth: 2), or SUITES_BASE..SUITES_HEAD when set.
# The lists are read as the base has them, never as the pull request does, so a change can't edit them to skip a
# suite; a suite the base has no list for runs. Any event but a pull request runs every suite, and a diff that can't
# run counts as a change: fail closed.
#
# One exception (CONTRIBUTING.md, Maintainers): a suite Canyon's private runners run instead (private_suites) is false
# on a pull request from this repository, by an author the workflow says is one of ours (CANYON_PRIVATE_AUTHOR=true,
# from the repository's CANYON_PRIVATE_AUTHORS), that carries the label ci:private, read from GitHub now with GH_TOKEN.
# Anything else, a label GitHub doesn't answer for included, leaves the suite to its paths.
set -euo pipefail
set -f # a pattern is git's to expand, never the shell's
cd "$(dirname "$0")/../.."
base=${SUITES_BASE:-HEAD^1}
head=${SUITES_HEAD:-HEAD}

# specs prints a suite list's patterns, as the base has it, as git pathspecs: :(glob)<pattern>, and
# :(glob,exclude)<pattern> for a !line. It fails where the base has no such list.
specs() {
  local list
  list=$(git show "$base:.github/suites/$1.txt") || return 1
  sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d' -e 's/^!\(.*\)/:(glob,exclude)\1/' -e '/^:(/!s/^/:(glob)/' <<< "$list"
}

# reaches says whether base..head changes a path the suite's list names; true where git can't answer, or the base
# has no list.
reaches() {
  local patterns
  patterns=$(specs "$1" 2>/dev/null) || return 0
  # shellcheck disable=SC2086 # one pattern a word: no pattern holds a space (internal/tools/suites)
  if git diff --quiet "$base" "$head" -- $patterns; then return 1; fi
}

# The suites scripts/ci-suite.sh runs, which private-ci.yml in Canyon's private CI repository runs for a labelled pull request.
private_suites=" journeys probes kind "

# private says the suite runs on the private runners for this pull request: false whenever in doubt.
labelled=""
private() {
  [[ $private_suites == *" $1 "* ]] || return 1
  [ "${GITHUB_EVENT_NAME:-}" = pull_request ] && [ "${CANYON_PRIVATE_AUTHOR:-}" = true ] && [ -f "${GITHUB_EVENT_PATH:-}" ] || return 1
  if [ -z "$labelled" ]; then
    labelled=no
    local pr labels
    pr=$(jq -r 'select(.pull_request.head.repo.full_name == .pull_request.base.repo.full_name) | .pull_request.number' "$GITHUB_EVENT_PATH" 2>/dev/null) || pr=""
    if [[ $pr =~ ^[0-9]+$ ]] && labels=$(gh api "repos/$GITHUB_REPOSITORY/issues/$pr/labels" --jq '.[].name' 2>/dev/null) && grep -qx 'ci:private' <<<"$labels"; then
      labelled=yes
    fi
  fi
  [ "$labelled" = yes ]
}

for suite in "$@"; do
  if [ ! -f ".github/suites/$suite.txt" ] || [ "$suite" = foundation ]; then
    echo "reach.sh: no suite named $suite (.github/suites)" >&2
    exit 1
  fi
  run=false
  if private "$suite"; then
    echo "reach.sh: $suite runs on Canyon's private runners for this pull request (ci:private)" >&2
  elif [ "${GITHUB_EVENT_NAME:-pull_request}" != pull_request ] || reaches foundation || reaches "$suite"; then
    run=true
  fi
  echo "$suite=$run"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$suite=$run" >> "$GITHUB_OUTPUT"; fi
done
