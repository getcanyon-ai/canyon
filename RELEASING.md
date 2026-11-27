# Releasing Canyon

A maintainer releases Canyon by pushing a version tag to `main`. Only the organisation's admins may create
`v*` tags. Everything else runs in GitHub Actions, on GitHub's hosted runners
([.github/workflows/release.yml](.github/workflows/release.yml)).

## Rehearse first

Before the first release, and after any change to the release workflow or its scripts, run the rehearsal from
`main`:

```sh
gh workflow run release.yml -R getcanyon-ai/canyon --ref main
```

It builds what a release builds and publishes nothing. That covers the CLI for every platform, all ten images for
linux/amd64 and linux/arm64 (each built to its digest and pushed nowhere), and the Helm chart, the deploy bundle and
Opengrep's source. Each file is packed and checked as for a release, under the version `v0.0.0-rehearsal`. Tag only
once it's green.

## The checks a release needs

[qa/release](qa/release/README.md) lists the checks by hand and what calls for each. `go run ./internal/tools/release
needs` lists what the changes since the last release call for. Each goes in `qa/release/results/<tag>.txt`, a pass or a
written skip with the commit it was done at; commit that file last and tag that commit. The tag is refused before
anything is built while one has no line, or its paths changed after its commit, while one of the commit's runs failed,
never started or is still running, or while an issue labelled `ci` is open. A fix CI can't test adds its check there in
the same pull request.

- **Hold merges to `main`** from the results commit until the tag's checks pass: a newer `main` cancels the stack and
  secret-scan runs on the commit, and the tag is refused.
- **A refusal for a run still going** (main-health, hourly, and the nightly runs attach to `main`'s head too), or for a
  flaky one, is answered by re-running the release's `checklist` job once it's done, never with another tag.
- **A tag that stays refused** is deleted by an admin and pushed again on the fixed commit, never skipped: the next
  release's range starts at the last one published on canyon-releases, so a skipped tag's checks would still be asked
  for.

## Tag

```sh
git tag v1.2.3 <commit on main>
git push origin v1.2.3
```

A version is `vMAJOR.MINOR.PATCH`. A release candidate is `vMAJOR.MINOR.PATCH-rc.N`: it's published as a
prerelease, so it's never `releases/latest`, which the install one-liner reads.

## What the tag runs

Every job first checks that the tagged commit is on `main`, and refuses it otherwise: only reviewed code is
released.

1. The CLI for every platform, with `SHA256SUMS` and `install.sh`, goes to a draft release of the tag here.
2. The ten images are built for linux/amd64 and linux/arm64, pushed to `ghcr.io/getcanyon-ai` by digest, joined
   into one multi-architecture image each and tagged with the version. `canyon-builder-node` is made by
   `pack builder create` in the daemon, not by a Dockerfile, so it is pushed by a tag of its own,
   `<version>-<arch>`, before the join. Their digests go to `images.txt` on the
   draft. Every base image they're built from is pinned by digest.
3. Every image in `images.txt` is pulled on both architectures, with no login, and only then is the release
   published.
4. The same release goes to [getcanyon-ai/canyon-releases](https://github.com/getcanyon-ai/canyon-releases), where
   `install.sh` downloads from. First, [scripts/publish-release.sh](scripts/publish-release.sh) packs the chart,
   the deploy bundle and Opengrep's complete source beside it. It checks that every image pulls without a login and
   that every archive carries `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES`. Then a separate job creates the
   release from those files.
5. The published release is installed and smoke-tested as a user gets it
   ([.github/workflows/release-smoke.yml](.github/workflows/release-smoke.yml)), and again every night against
   the latest release.
6. A release that isn't a prerelease then updates Canyon's listing in the
   [MCP Registry](https://registry.modelcontextprotocol.io) with its version: [server.json](server.json), as
   `io.github.getcanyon-ai/canyon`. Only this job publishes it, never a person by hand and never a pull request. A
   release run again finds its version listed and publishes nothing.

## The tokens

- **The images push with each job's own `GITHUB_TOKEN`** (`packages: write`), which lasts only as long as the job.
  This needs a one-time setting on each of the ten packages, `ghcr.io/getcanyon-ai/canyon-*`: under **Package
  settings › Manage Actions access**, add `getcanyon-ai/canyon` with the **Write** role. Without it the push is
  refused, because those packages were created outside this repository.
- **A new package starts private.** The first release that pushes one creates it, and its pull check fails until
  an owner of the organisation makes it public, under its **Package settings › Danger Zone**. Then run the
  release again. `canyon-builder-node` was new in v0.11.2.
- **Publishing on canyon-releases takes one secret, `CANYON_RELEASES_TOKEN`:** a fine-grained token with
  **Contents: read and write** on `getcanyon-ai/canyon-releases` and nothing else. It can't push to this
  repository or create its tags.
  - It's in the repository's `release` environment, whose deployment policy allows `v*` tags only, so no branch,
    pull request or fork can read it.
  - The one job that uses it checks nothing out and runs none of the tree's code. It downloads the files the
    packing job made and creates the release from them. The packing job, which runs the tree's scripts, holds no
    secret.
- **The MCP Registry listing takes no secret.** Its job alone has `id-token: write`: GitHub's OIDC token for the run
  proves the `io.github.getcanyon-ai` namespace to the registry, and no account or DNS record is needed.
- Only admins may create `v*` tags, and every job refuses a commit that isn't on `main`. The rehearsal needs no
  secret and runs in no environment.

Both repositories' releases are **immutable** once published: their assets and their tag can't change. So each
release takes all its assets as a draft, and is published only at the end, once everything on it is checked.

If a step fails, re-run the failed job once its cause is fixed outside the tree (a token, a package's access). A
draft the failed run left is replaced, never a published release. If the fix is in the tree, merge it to `main` and
release the next patch version. A published tag is never moved.
