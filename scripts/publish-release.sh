#!/bin/sh
# scripts/publish-release.sh <tag>: copies Canyon's release <tag> from this repository ($GITHUB_REPOSITORY,
# else getcanyon-ai/canyon) into a release of the same tag on getcanyon-ai/canyon-releases, the public download repository
# (design/specs/cli.md §8.1), with three files packaged beside it: from that tag, the Helm chart, and the
# laptop demo plus deploy/k8s, so nobody needs Canyon's source to install it; and Opengrep's complete
# source, which canyon-opengrep's LGPL-2.1 binary is offered with. .github/workflows/publish.yml runs it as
# the organisation's release account, so the public release names no person as its publisher (#2413). gh
# signs in as whoever publishes (scopes public_repo and write:packages); SOURCE_TOKEN, when set, reads the
# private release and its images instead, so the release account never reads Canyon's source. It needs oras,
# jq and helm. Each image is copied by digest, unchanged, from the repository owner's ghcr.io
# namespace, where release.yml pushed it, to the organisation's ghcr.io/getcanyon-ai, and the published images.txt names the copies.
set -eu
here=$(dirname "$0")
tag=${1:?usage: scripts/publish-release.sh <tag>}
src=${GITHUB_REPOSITORY:-getcanyon-ai/canyon}
dst=getcanyon-ai/canyon-releases
images=ghcr.io/getcanyon-ai
fail() { echo "publish-release: $*" >&2; exit 1; }
src_token=${SOURCE_TOKEN:-$(gh auth token)}

state=$(GH_TOKEN="$src_token" gh release view "$tag" -R "$src" --json isDraft,isPrerelease --jq '"\(.isDraft) \(.isPrerelease)"')
case $state in
  "false false") pre= ;;
  "false true") pre=--prerelease ;;
  *) fail "$tag is a draft on $src: its images have not all pulled yet (release.yml)" ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM # sh runs no EXIT trap on a signal, and $work holds a sign-in
# only what release.yml attaches, so nothing else on the private release goes public
GH_TOKEN="$src_token" gh release download "$tag" -R "$src" -D "$work/assets" -p 'canyon_*' -p SHA256SUMS -p install.sh -p images.txt

# Releases cut before the move to canyon-releases install from the private repository and carry Apache-2.0.
grep -q "github.com/$dst/releases" "$work/assets/install.sh" || fail "$tag's install.sh downloads from elsewhere: publish a tag cut after the move to canyon-releases"
"$here/release-archives-check.sh" "$work/assets" || fail "$tag's archives do not all carry Canyon's LICENSE, NOTICE and THIRD_PARTY_NOTICES: publish a tag cut after #2397"

# two sign-ins to the one registry: each copy reads as the source's and writes as the publisher's
printf %s "$src_token" | oras login ghcr.io --registry-config "$work/src.json" -u x --password-stdin >/dev/null
gh auth token | oras login ghcr.io --registry-config "$work/dst.json" -u "$(gh api user --jq .login)" --password-stdin >/dev/null
while read -r ref; do
  name=${ref##*/}; name=${name%@*}; digest=${ref#*@}
  oras cp --from-registry-config "$work/src.json" --to-registry-config "$work/dst.json" "$ref" "$images/$name:$tag" >/dev/null # a carbon copy of an index: the digest stays
  got=$(oras manifest fetch --registry-config "$work/dst.json" --descriptor "$images/$name:$tag" | jq -er .digest)
  [ "$got" = "$digest" ] || fail "$images/$name:$tag is $got, not $ref's digest"
  echo "$images/$name@$digest"
done < "$work/assets/images.txt" > "$work/images.txt"
mv "$work/images.txt" "$work/assets/images.txt"
# A new package on ghcr.io is private, and only GitHub's settings page makes it public: every copy must
# pull anonymously, as a partner pulls it, before the release goes out.
private=
while read -r ref; do
  repo=${ref#ghcr.io/}; repo=${repo%@*}
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:$repo:pull" | jq -er .token) &&
    curl -fsSI -o /dev/null -H "Authorization: Bearer $token" -H 'Accept: application/vnd.oci.image.index.v1+json' "https://ghcr.io/v2/$repo/manifests/${ref#*@}" 2>/dev/null ||
    private="$private
  https://github.com/orgs/getcanyon-ai/packages/container/${repo#*/}/settings"
done < "$work/assets/images.txt"
[ -z "$private" ] || fail "these images don't pull without a login; on each page, Danger Zone > Change visibility > Public, then run this again:$private"

git fetch -q origin "refs/tags/$tag:refs/tags/$tag"

# canyon-chart.tgz: the Helm chart, with LICENSE, NOTICE and THIRD_PARTY_NOTICES (scripts/chart-package.sh)
"$here/chart-package.sh" "$tag" "$tag" "$work/assets/canyon-chart.tgz"

# canyon-deploy.tar.gz: the laptop demo, the team setup on one Docker server (deploy/docker) and deploy/k8s, pulling
# the release's images (scripts/deploy-bundle.sh). Its SHA-256 joins the CLI archives' in SHA256SUMS, which the
# Docker guide checks the download against (docs: self-host/docker.md).
"$here/deploy-bundle.sh" "$tag" "$work/assets/images.txt" "$work/assets/canyon-deploy.tar.gz"
(cd "$work/assets" && sha256sum canyon-deploy.tar.gz >> SHA256SUMS)

# opengrep-1.30.0-source.tar.gz: the complete source of the Opengrep in canyon-opengrep, LGPL-2.1's offer
# of it beside the image's binary (scripts/opengrep-source.sh; the image's /usr/share/licenses/opengrep/SOURCE)
"$here/opengrep-source.sh" "$work/assets/opengrep-1.30.0-source.tar.gz"

# What was packed here, before anything is published: licensed, and clean of this machine's owners and metadata (#2409)
"$here/release-archives-check.sh" "$work/assets" || fail "the archives packed here are not all licensed and clean: nothing was published"

gh release create "$tag" "$work"/assets/* -R "$dst" --title "canyon $tag" $pre \
  --notes "Canyon $tag, open source under the AGPL-3.0, with its CLI and contracts under Apache-2.0 (LICENSE in every archive). Install the CLI as the README says. images.txt names the images by digest, canyon-chart.tgz is the Helm chart, canyon-deploy.tar.gz holds the laptop demo, the team setup on one Docker server (deploy/docker/team.yml) and the Kubernetes install base (deploy/k8s), its SHA-256 in SHA256SUMS, and opengrep-1.30.0-source.tar.gz is the complete source of the Opengrep inside canyon-opengrep (LGPL-2.1)."
