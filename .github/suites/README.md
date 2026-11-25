# What a pull request runs

Each `<suite>.txt` here lists what a stack suite reaches: the paths whose change can change its outcome. A pull
request runs a suite when its change reaches that suite's paths, or `foundation.txt`'s. These files decide only what a
pull request runs: a push to main, the nightly run (stack.yml) and a manual run are as they were, and a red main
opens an issue as before.

- **Format:** one pattern a line. `#` starts a comment. A pattern is a git `:(glob)` pathspec, where `**` crosses
  directories and `*` stays within one. A line starting `!` takes what it matches out of that file's other lines.
  No pattern holds a space.
- **The check:** `reach.sh <suite>…` answers `<suite>=true|false` for the pull request's merge commit against its
  base (`git diff HEAD^1 HEAD`), reading these lists as the base has them, so a pull request can't edit them to
  skip a suite. It answers true when it can't tell or the base has no list, and true for every suite on any other
  event.
- **One exception, `ci:private`:** a maintainer's pull request labelled `ci:private` runs journeys, probes and kind on
  Canyon's private runners instead (CONTRIBUTING.md, Maintainers), so `reach.sh` answers false for those three. It does
  so only for a pull request from this repository, by an author in the repository's `CANYON_PRIVATE_AUTHORS`, whose
  label GitHub answers for when the paths job asks. Any doubt runs them here.
- **Kept honest by `internal/tools/suites`.**
  - Each suite's paths must cover every package of this module that its Go tests and the non-server binaries it
    builds compile, by `go list -deps`.
  - Each workflow's jobs must ask `reach.sh` for suites that have a file here.
  - `foundation.txt` must hold the shared foundation.
- **The server:** the server's own code is listed by area, the route files a suite drives. Its core is in
  `foundation.txt`.
- **On Canyon's private runners:** `scripts/ci-select.sh` reads these lists for the stack suites, beside its own
  table for the rest (`scripts/ci-select.txt`; CONTRIBUTING.md, Maintainers).
- **What a diff would run:** `SUITES_BASE=<base> SUITES_HEAD=<head> .github/suites/reach.sh <suite>…`, with the
  diff the pull request's own paths job reads.
