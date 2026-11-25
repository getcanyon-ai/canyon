# canyon-builder as a build with no build values uses it (contracts/executor.md, Every app builds on Canyon's build
# image; #177): the image builder.Dockerfile makes, BASE, with canyon-run:noble as its run image and the executor's two
# build variables as its own build environment, so pack builds on it as it is, never on an ephemeral copy. METADATA is
# BASE's io.buildpacks.builder.metadata with canyon-run:noble its one run image, which pack reads in place of
# --run-image; /cnb/run.toml says the same to the lifecycle.
ARG BASE
FROM ${BASE}
ARG METADATA
USER root
RUN mkdir -p /cnb/build-config/env && printf production > /cnb/build-config/env/NODE_ENV.override && printf / > /cnb/build-config/env/NEXT_PRIVATE_OUTPUT_TRACE_ROOT.override && printf '[[images]]\n  image = "canyon-run:noble"\n' > /cnb/run.toml
LABEL io.buildpacks.builder.metadata="${METADATA}"
USER 1001:1001
