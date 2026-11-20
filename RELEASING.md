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

1. The CLI for every platform, with `SHA256SUMS` and `install.sh`, goes to a draft release of the tag here.
2. The nine images are built for linux/amd64 and linux/arm64, pushed to `ghcr.io/getcanyon-ai` by digest, joined
   into one multi-architecture image each and tagged with the version. Their digests go to `images.txt` on the
   draft.
3. Every image in `images.txt` is pulled on both architectures, with no login, and only then is the release
   published.
4. The same release goes to [getcanyon-ai/canyon-releases](https://github.com/getcanyon-ai/canyon-releases), where
   `install.sh` downloads from ([scripts/publish-release.sh](scripts/publish-release.sh)). The chart, the deploy
   bundle and Opengrep's complete source are packed beside it. Nothing is published there unless every image
   pulls without a login and every archive carries `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES`.
5. The published release is installed and smoke-tested as a user gets it
   ([.github/workflows/release-smoke.yml](.github/workflows/release-smoke.yml)), and again every night against
   the latest release.

## The token

Publishing needs one secret, `CANYON_PUBLISH_TOKEN`, in the repository's `release` environment. Its deployment
policy allows `v*` tags only, so no branch, pull request or fork can read it. It's a **classic** personal access
token of the organisation's release account, because a fine-grained token can't push to GHCR. It has two scopes:

- `write:packages`, to push the nine images to `ghcr.io/getcanyon-ai` and tag them (step 2);
- `public_repo`, to create the release on getcanyon-ai/canyon-releases (step 4).

Only the jobs that push images or write canyon-releases are in the environment, and in them only the steps that
log in or publish see the token: each login is right before its push, and a `docker logout` follows it whatever
happened. The release here uses the workflow's own token, and the rehearsal needs no secret. Only admins may create
`v*` tags, so the token only runs with code an admin tagged on `main`.

If a step fails, re-run the failed job once its cause is fixed outside the tree (a token, a package's access). If
the fix is in the tree, merge it to `main` and release the next patch version. A published tag is never moved.
