# canyon-builder: the builder every app builds with on Docker (contracts/executor.md, Every app builds on
# Canyon's build image). Paketo's noble builder, pinned by digest, with libpq5, which PHP's pgsql and
# pdo_pgsql link and Composer loads to check a composer.json's ext-* (#1734), noble's own at build time
# (#1754); nothing else changes, and it ends as the stack's build user.
FROM docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:f5dd94c89be567f151b4a3ca1d86267fbc304991f27ee0608dfde70d6bbab65c
USER root
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libpq5 && rm -rf /var/lib/apt/lists/*
USER 1001:1001
