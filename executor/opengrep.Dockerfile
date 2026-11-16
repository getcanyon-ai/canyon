# Canyon's Opengrep image, which the Makefile's opengrep-rules-test runs the vendored rules in: opengrep
# 1.30.0, the release binary for the builder's architecture, fetched and checked against its sha256
# before it lands, on the digest-pinned base, with Opengrep's licence (LGPL-2.1-only, with the OCaml
# linking exception its sources name), from the same tag and checked the same way, and where its source
# is, in /usr/share/licenses/opengrep. The Docker executor builds it from this file on first use; on Kubernetes, build it and name it in
# CANYON_IMAGE_OPENGREP (deploy/k8s/README.md):
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/canyon-opengrep:1.30.0 --push - < executor/opengrep.Dockerfile
ARG TARGETARCH
# The fetch: each file tried again after 1, 2, 4, 8, 16 and 32 s, where BuildKit's ADD tries a URL once
# and a GitHub 504 failed every build (#2755), and kept only when its pinned sha256 matches. busybox's
# wget checks no certificate: the sha256 is what vouches for each file. The URLs are build args so
# .github/pack-download-test.py can point them at a stub.
FROM --platform=$BUILDPLATFORM busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e AS fetch
ARG TARGETARCH
ARG OPENGREP_RELEASE=https://github.com/opengrep/opengrep/releases/download/v1.30.0
ARG OPENGREP_SOURCE=https://raw.githubusercontent.com/opengrep/opengrep/v1.30.0
ARG OPENGREP_SHA256_AMD64=35779bdd72e92129c8df2a77f0c55e8c08356801ea92591ef32108d6b28d564c
ARG OPENGREP_SHA256_ARM64=a5d5a4a58ba5d46ff51e921663da1c2bba38f4b03987f4aeec87f16c6ad3ecae
ARG OPENGREP_LICENSE_SHA256=20c17d8b8c48a600800dfd14f95d5cb9ff47066a9641ddeab48dc54aec96e331
ARG OPENGREP_EXCEPTION_SHA256=360a184cf3ae270fe7c328addc89768925a445ca255852a82a20d57a26749862
RUN set -eu; \
    fetch() { \
      for wait in 1 2 4 8 16 32 end; do \
        rm -f "$3"; wget -qO "$3" "$1" && break; \
        if [ "$wait" = end ]; then echo "opengrep: $1 failed 7 times" >&2; exit 1; fi; \
        echo "opengrep: $1 failed; trying again in $wait s" >&2; sleep "$wait"; done; \
      echo "$2  $3" | sha256sum -c -; }; \
    case "$TARGETARCH" in \
      amd64) f=opengrep_manylinux_x86 sum=$OPENGREP_SHA256_AMD64 ;; \
      arm64) f=opengrep_manylinux_aarch64 sum=$OPENGREP_SHA256_ARM64 ;; \
      *) echo "opengrep: no release for $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out/usr/local/bin /out/usr/share/licenses/opengrep; \
    fetch "$OPENGREP_RELEASE/$f" "$sum" /out/usr/local/bin/opengrep; \
    fetch "$OPENGREP_SOURCE/LICENSE" "$OPENGREP_LICENSE_SHA256" /out/usr/share/licenses/opengrep/LICENSE; \
    fetch "$OPENGREP_SOURCE/libs/commons/license.txt" "$OPENGREP_EXCEPTION_SHA256" /out/usr/share/licenses/opengrep/LICENSE.linking-exception; \
    chmod 0755 /out/usr/local/bin/opengrep; chmod 0644 /out/usr/share/licenses/opengrep/*
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS amd64
COPY --from=fetch /out/ /
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS arm64
COPY --from=fetch /out/ /
FROM ${TARGETARCH}
COPY --chmod=0644 <<'SOURCE' /usr/share/licenses/opengrep/SOURCE
/usr/local/bin/opengrep is Opengrep 1.30.0's release binary, unmodified, under the GNU Lesser General
Public License version 2.1 only (LICENSE, beside this file), with the special exception on linking its
source files name (LICENSE.linking-exception, beside this file: libs/commons/license.txt in its source).

Its complete corresponding source, commit acf67b45c97c4b63626536605c77064ef536806d with every
submodule, is opengrep-1.30.0-source.tar.gz on every Canyon release:
  https://github.com/getcanyon-ai/canyon-releases/releases
and upstream:
  https://github.com/opengrep/opengrep/tree/v1.30.0
  https://github.com/opengrep/opengrep/releases/tag/v1.30.0
SOURCE
# opengrep reads rule files in the locale's encoding, and the base's is ASCII: the vendored sast-rules
# carry UTF-8 text (an em dash, curly quotes), which would crash every scan.
ENV LANG=C.UTF-8
ENV XDG_CACHE_HOME=/opt/opengrep
RUN --network=none /usr/local/bin/opengrep --version && chmod -R a+rX /opt/opengrep
ENV HOME=/tmp
WORKDIR /scan/tree
ENTRYPOINT ["/usr/local/bin/opengrep"]
