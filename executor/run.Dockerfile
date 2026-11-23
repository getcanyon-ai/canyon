# canyon-run: the image every app runs on (contracts/executor.md, Every app runs on Canyon's run image).
# Paketo's noble run image, pinned by digest, with libatomic1, which Node 25 and later link and no
# Paketo run image carries, libsqlite3-0, which Paketo's CPython links for Python's sqlite3 (#1708),
# and libpq5, which PHP's pgsql and pdo_pgsql link (#1734): noble's own at build time, never a pinned
# version, which Ubuntu stops serving at its next update (#1754); nothing else changes, and it
# ends as the stack's non-root user. CANYON_RUN_APT_MIRROR, when the executor's environment sets it, is
# the one Ubuntu archive the packages come from in place of Ubuntu's archive and security pocket.
FROM docker.io/paketobuildpacks/ubuntu-noble-run@sha256:00209cf68f2a1ed1cdb2f5a9502384433f0194fa11e1a9ac674bd0a2b54601b5
USER root
ARG CANYON_RUN_APT_MIRROR=
RUN if [ -n "$CANYON_RUN_APT_MIRROR" ]; then printf 'Types: deb\nURIs: %s\nSuites: noble noble-updates noble-security\nComponents: main universe multiverse\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' "$CANYON_RUN_APT_MIRROR" > /etc/apt/sources.list.d/ubuntu.sources; fi && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libatomic1 libsqlite3-0 libpq5 && rm -rf /var/lib/apt/lists/*
USER 1002:1001
