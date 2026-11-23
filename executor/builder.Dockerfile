# canyon-builder: the builder every app builds with on Docker (contracts/executor.md, Every app builds on
# Canyon's build image). Paketo's noble builder, pinned by digest, with libpq5, which PHP's pgsql and
# pdo_pgsql link and Composer loads to check a composer.json's ext-* (#1734), noble's own at build time
# (#1754); nothing else changes, and it ends as the stack's build user.
FROM docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:5553d472fefc8dc1d9fb794422ec2f931fd2bf470c107b1786b048096c0d7f45
USER root
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libpq5 && rm -rf /var/lib/apt/lists/*
USER 1001:1001
