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

It builds what a release builds and publishes nothing. That covers the CLI for every platform, all nine images for
linux/amd64 and linux/arm64 (each built to its digest and pushed nowhere), and the Helm chart, the deploy bundle and
Opengrep's source. Each file is packed and checked as for a release, under the version `v0.0.0-rehearsal`. Tag only
once it's green.

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
2. The nine images are built for linux/amd64 and linux/arm64, pushed to `ghcr.io/getcanyon-ai` by digest, joined
   into one multi-architecture image each and tagged with the version. Their digests go to `images.txt` on the
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

## The tokens

- **The images push with each job's own `GITHUB_TOKEN`** (`packages: write`), which lasts only as long as the job.
  This needs a one-time setting on each of the nine packages, `ghcr.io/getcanyon-ai/canyon-*`: under **Package
  settings › Manage Actions access**, add `getcanyon-ai/canyon` with the **Write** role. Without it the push is
  refused, because those packages were created outside this repository.
- **Publishing on canyon-releases takes one secret, `CANYON_RELEASES_TOKEN`:** a fine-grained token with
  **Contents: read and write** on `getcanyon-ai/canyon-releases` and nothing else. It can't push to this
  repository or create its tags.
  - It's in the repository's `release` environment, whose deployment policy allows `v*` tags only, so no branch,
    pull request or fork can read it.
  - The one job that uses it checks nothing out and runs none of the tree's code. It downloads the files the
    packing job made and creates the release from them. The packing job, which runs the tree's scripts, holds no
    secret.
- Only admins may create `v*` tags, and every job refuses a commit that isn't on `main`. The rehearsal needs no
  secret and runs in no environment.

If a step fails, re-run the failed job once its cause is fixed outside the tree (a token, a package's access). If
the fix is in the tree, merge it to `main` and release the next patch version. A published tag is never moved.
