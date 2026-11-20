#!/usr/bin/env bash
# For each suite named, whether this pull request's change reaches it (.github/suites/README.md): "<suite>=true" or
# "<suite>=false" on stdout, and into $GITHUB_OUTPUT when Actions sets it. The change is HEAD^1..HEAD, a pull
# request's merge commit against its base (the checkout's fetch-depth: 2), or SUITES_BASE..SUITES_HEAD when set.
# Any event but a pull request runs every suite, and a diff that can't run counts as a change: fail closed.
set -euo pipefail
set -f # a pattern is git's to expand, never the shell's
cd "$(dirname "$0")/../.."
base=${SUITES_BASE:-HEAD^1}
head=${SUITES_HEAD:-HEAD}

# specs prints a suite file's patterns as git pathspecs: :(glob)<pattern>, and :(glob,exclude)<pattern> for a !line.
specs() {
  sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d' -e 's/^!\(.*\)/:(glob,exclude)\1/' -e '/^:(/!s/^/:(glob)/' ".github/suites/$1.txt"
}

# reaches says whether base..head changes a path the suite file names; true where git can't answer.
reaches() {
  # shellcheck disable=SC2046 # one pattern a word: no pattern holds a space (internal/tools/suites)
  if git diff --quiet "$base" "$head" -- $(specs "$1"); then return 1; fi
}

for suite in "$@"; do
  if [ ! -f ".github/suites/$suite.txt" ] || [ "$suite" = foundation ]; then
    echo "reach.sh: no suite named $suite (.github/suites)" >&2
    exit 1
  fi
  run=false
  if [ "${GITHUB_EVENT_NAME:-pull_request}" != pull_request ] || reaches foundation || reaches "$suite"; then
    run=true
  fi
  echo "$suite=$run"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$suite=$run" >> "$GITHUB_OUTPUT"; fi
done
