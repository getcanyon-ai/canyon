#!/usr/bin/env bash
# scripts/ci-select.sh: which of scripts/ci-suite.sh's suites a pull request's change reaches, for the trusted
# private-ci.yml of Canyon's private CI repository (CONTRIBUTING.md, Maintainers), which runs this file and its table
# as main holds them, never as the pull request does. It reads the changed paths on stdin, NUL-separated, as
# `git diff -z --name-only --no-renames <main>...<head>` prints them, and prints <suite>=true|false for every suite it
# knows, in this file's order. A suite it doesn't know is the caller's to run.
#
# Every change runs lint, tidy, web, test-rest and gitleaks. A stack suite runs when the change reaches its list in
# .github/suites (as reach.sh reads it), and any suite when a changed path matches a line of scripts/ci-select.txt
# that names it. Every suite runs (fail closed) when a changed path matches no line of that table, or a line that
# names every suite, or .github/suites/foundation.txt; when no path changed; and when git can't take a name as a path.
# Why is on stderr. It needs bash and git, and runs in a git checkout of this repository.
set -euo pipefail
set -f # a pattern is git's to expand, never the shell's
cd "$(dirname "$0")/.."

suites="journeys-1 journeys-2 journeys-3 journeys-4 journeys-5 journeys-6 journeys-7 journeys-8 probes kind-calico
  kind-calico-1 kind-calico-2 kind-calico-3 kind-kindnet kind-canyon handoff-argocd handoff-flux egress-floor
  egress-layer3 oidc patch tour tour-classic backup build-arch test-rest server-1 server-2 server-3 server-4 noee-rest
  noee-server-1 noee-server-2 noee-server-3 noee-server-4 runs-1 runs-2 runs-3 provisioners-14 provisioners-15
  provisioners-16 provisioners-17 provisioners-18 lint tidy web compose-up images deploy-bundle helm-chart docs
  gitleaks"
always=" lint tidy web test-rest gitleaks "

# listed is the .github/suites list a stack suite runs by, as its public workflow asks reach.sh for it
listed() {
  case $1 in
    journeys-*) echo journeys ;;
    probes | oidc | patch | tour | backup | build-arch) echo "$1" ;;
    tour-classic) echo tour ;; # the tour on the other image store: the same list
    kind-* | handoff-*) echo kind ;; # kube.yml's jobs, all on kind.txt
    egress-*) echo egress ;;
    helm-chart) echo helm ;;
  esac
}

# specs is a .github/suites list as git pathspecs, exactly as reach.sh reads it
specs() {
  sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d' -e 's/^!\(.*\)/:(glob,exclude)\1/' -e '/^:(/!s/^/:(glob)/' ".github/suites/$1.txt"
}

all() {
  echo "ci-select: every suite: $*" >&2
  for s in $suites; do echo "$s=true"; done
  exit 0
}

# the changed paths as the entries of a scratch index, each an empty file: git's own pathspecs then match them as
# they match a tree's, with no checkout and no commit
index=$(mktemp -d)
trap 'rm -rf "$index"' EXIT
export GIT_INDEX_FILE=$index/index
empty=$(git hash-object -t blob --stdin < /dev/null)
LC_ALL=C sort -zu | while IFS= read -r -d '' f; do printf '100644 %s\t%s\0' "$empty" "$f"; done > "$index/info"
asked=$(tr -cd '\0' < "$index/info" | wc -c | tr -d ' ')
[ "$asked" -gt 0 ] || all "no changed path"
git update-index -z --index-info < "$index/info" 2> /dev/null || all "git could not take the changed paths as paths"
# git ignores a name it can't hold (../x, .git/x) and keeps one of a file and a directory of the same name
held=$(git ls-files | wc -l | tr -d ' ')
[ "$held" = "$asked" ] || all "git took $held of the $asked changed paths as paths"

# the table: a pattern, then the suites it runs (shell patterns of their names), * for every suite, - for no more
rows=()
while read -r pattern names; do
  [ -n "$pattern" ] || continue
  [ -n "$names" ] || { echo "ci-select: scripts/ci-select.txt: $pattern names no suite (- for none)" >&2; exit 1; }
  rows+=("$pattern" "$names")
done < <(sed -e 's/#.*//' scripts/ci-select.txt)
[ "${#rows[@]}" -gt 0 ] || { echo "ci-select: scripts/ci-select.txt holds no line" >&2; exit 1; }

excludes=()
for ((i = 0; i < ${#rows[@]}; i += 2)); do excludes+=(":(glob,exclude)${rows[i]}"); done
unmatched=$(git ls-files -- "${excludes[@]}")
[ -z "$unmatched" ] || all "${unmatched%%$'\n'*} matches no line of scripts/ci-select.txt"
# shellcheck disable=SC2046 # one pathspec a word: no pattern holds a space (internal/tools/suites)
[ -z "$(git ls-files -- $(specs foundation))" ] || all "the change reaches .github/suites/foundation.txt"

hit=" "
for ((i = 0; i < ${#rows[@]}; i += 2)); do
  [ -n "$(git ls-files -- ":(glob)${rows[i]}")" ] || continue
  for n in ${rows[i + 1]}; do
    [ "$n" != '*' ] || all "the change reaches ${rows[i]}"
    hit+="$n "
  done
done

reached=" "
for l in journeys probes kind egress oidc patch tour backup build-arch helm; do
  # shellcheck disable=SC2046 # one pathspec a word, as reach.sh's
  if [ -n "$(git ls-files -- $(specs "$l"))" ]; then reached+="$l "; fi
done

for s in $suites; do
  run=false
  l=$(listed "$s")
  if [[ $always == *" $s "* ]] || { [ -n "$l" ] && [[ $reached == *" $l "* ]]; }; then run=true; fi
  for n in $hit; do
    # shellcheck disable=SC2053 # n is a pattern of suite names
    if [[ $s == $n ]]; then run=true; fi
  done
  echo "$s=$run"
done
