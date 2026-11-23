# canyon-build: the build image every app builds on under kpack (contracts/executor.md, Every app builds on
# Canyon's build image). Paketo's noble build image, pinned by digest, with libpq5, which PHP's pgsql and
# pdo_pgsql link and Composer loads to check a composer.json's ext-* (#1734), noble's own at build time
# (#1754); nothing else changes, and it ends as the stack's build user.
FROM docker.io/paketobuildpacks/ubuntu-noble-build@sha256:20752810c913fb51847cbf9159e0ad9769dd34db040cc89b6b588761269c68c4
USER root
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libpq5 && rm -rf /var/lib/apt/lists/*
USER 1001:1001
