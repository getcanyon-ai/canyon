#!/bin/sh
# scripts/publish-release.sh <tag>: Canyon's release <tag> of this repository ($GITHUB_REPOSITORY, else
# getcanyon-ai/canyon), published again as a release of the same tag on getcanyon-ai/canyon-releases, the download
# repository install.sh reads (design/specs/cli.md §8.1), with three files packaged beside it: from that tag, the
# Helm chart, and the laptop demo plus deploy/k8s, so nobody needs Canyon's source to install it; and Opengrep's
# complete source, which canyon-opengrep's LGPL-2.1 binary is offered with. release.yml runs it after the tag's
# release is published here, its images already in ghcr.io/getcanyon-ai: each one in images.txt must pull without a
# login before anything is published. SOURCE_TOKEN, when set, reads this repository's release. It needs jq and helm.
#
# release.yml runs it with PUBLISH_OUT=<dir>, with no token that can publish: it downloads, packs and checks the
# release's files, checks every image pulls without a login, and writes <dir>/assets/ (the files to publish),
# <dir>/notes.md and, for a prerelease, <dir>/prerelease. A later job that runs none of the tree's code creates the
# release on canyon-releases from them. Without PUBLISH_OUT it creates that release itself, signed in as gh is.
#
# A rehearsal publishes nothing: with PUBLISH_ASSETS=<dir> (a release's CLI archives, SHA256SUMS, install.sh and
# images.txt, as make cli-release and release.yml write them) and PUBLISH_REF=<git-ref>, it packs and checks the
# same files from them and that ref, and stops before the image check and the release.
set -eu
here=$(dirname "$0")
tag=${1:?usage: scripts/publish-release.sh <tag>}
src=${GITHUB_REPOSITORY:-getcanyon-ai/canyon}
dst=getcanyon-ai/canyon-releases
fail() { echo "publish-release: $*" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM # sh runs no EXIT trap on a signal
mkdir "$work/assets"
if [ -n "${PUBLISH_ASSETS:-}" ]; then
  ref=${PUBLISH_REF:?a rehearsal names the git ref it packs from (PUBLISH_REF)}
  cp "$PUBLISH_ASSETS"/canyon_* "$PUBLISH_ASSETS"/SHA256SUMS "$PUBLISH_ASSETS"/install.sh "$PUBLISH_ASSETS"/images.txt "$work/assets/"
  pre=
else
  ref=$tag
  src_token=${SOURCE_TOKEN:-$(gh auth token)} # reads this repository's release; never the publishing token in release.yml
  state=$(GH_TOKEN="$src_token" gh release view "$tag" -R "$src" --json isDraft,isPrerelease --jq '"\(.isDraft) \(.isPrerelease)"')
  case $state in
    "false false") pre= ;;
    "false true") pre=--prerelease ;;
    *) fail "$tag is a draft on $src: its images have not all pulled yet (release.yml)" ;;
  esac
  GH_TOKEN="$src_token" gh release download "$tag" -R "$src" -D "$work/assets" -p 'canyon_*' -p SHA256SUMS -p install.sh -p images.txt
  git rev-parse -q --verify "refs/tags/$tag" >/dev/null || git fetch -q origin "refs/tags/$tag:refs/tags/$tag"
fi
grep -q "github.com/$dst/releases" "$work/assets/install.sh" || fail "$tag's install.sh downloads from elsewhere"
"$here/release-archives-check.sh" "$work/assets" || fail "$tag's archives do not all carry Canyon's LICENSE, NOTICE and THIRD_PARTY_NOTICES"

"$here/chart-package.sh" "$ref" "$tag" "$work/assets/canyon-chart.tgz"
"$here/deploy-bundle.sh" "$ref" "$work/assets/images.txt" "$work/assets/canyon-deploy.tar.gz"
(cd "$work/assets" && sha256sum canyon-deploy.tar.gz >> SHA256SUMS)
"$here/opengrep-source.sh" "$work/assets/opengrep-1.30.0-source.tar.gz"
"$here/release-archives-check.sh" "$work/assets" || fail "the archives packed here are not all licensed and clean: nothing was published"

if [ -n "${PUBLISH_ASSETS:-}" ]; then
  echo "publish-release: a rehearsal of $tag from $ref, packed and checked; nothing was published:"
  ls -l "$work/assets"
  exit 0
fi

# Every image must pull without a login: a release whose images don't is not published.
private=
while read -r img; do
  repo=${img#ghcr.io/}; repo=${repo%@*}
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:$repo:pull" | jq -er .token) &&
    curl -fsSI -o /dev/null -H "Authorization: Bearer $token" -H 'Accept: application/vnd.oci.image.index.v1+json' "https://ghcr.io/v2/$repo/manifests/${img#*@}" 2>/dev/null ||
    private="$private
  https://github.com/orgs/getcanyon-ai/packages/container/${repo#*/}/settings"
done < "$work/assets/images.txt"
[ -z "$private" ] || fail "these images don't pull without a login; on each page, Danger Zone > Change visibility > Public, then run this again:$private"

cat > "$work/notes.md" <<NOTES
Canyon $tag, open source under the AGPL-3.0, with its CLI and contracts under Apache-2.0 (LICENSE in every archive). Install the CLI as the README says. images.txt names the images by digest, canyon-chart.tgz is the Helm chart, canyon-deploy.tar.gz holds the laptop demo, the team setup on one Docker server (deploy/docker/team.yml) and the Kubernetes install base (deploy/k8s), its SHA-256 in SHA256SUMS, and opengrep-1.30.0-source.tar.gz is the complete source of the Opengrep inside canyon-opengrep (LGPL-2.1). Release notes: https://github.com/$src/releases/tag/$tag
NOTES

if [ -n "${PUBLISH_OUT:-}" ]; then
  mkdir -p "$PUBLISH_OUT/assets" && cp "$work"/assets/* "$PUBLISH_OUT/assets/" && cp "$work/notes.md" "$PUBLISH_OUT/notes.md"
  [ -z "$pre" ] || : > "$PUBLISH_OUT/prerelease"
  echo "publish-release: $tag packed and checked into $PUBLISH_OUT; nothing was published"
  exit 0
fi
gh release create "$tag" "$work"/assets/* -R "$dst" --title "canyon $tag" $pre --notes-file "$work/notes.md"
