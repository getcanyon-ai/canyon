# The dashboard is built first and copied into the Go stage: the binary embeds it (web/embed.go).
FROM node:22-alpine@sha256:1314fb30ec37592d5845e5a160d522c0e680972aa534549940b8d73bde88481a AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# The reference set-up the hand-off's platform team card shows filled in (design/HAND-OFF.md §3b).
COPY deploy/gitops /deploy/gitops
RUN npm run build

FROM golang:1.26.11@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
# Go's default; CI passes a list that asks the proxy again on any error (#2533)
ARG GOPROXY=https://proxy.golang.org,direct
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
# Build tags: none for a release; the E2E stack's e2e trusts its own run's licence key (cmd/canyon-server/licence_e2e.go).
ARG GOTAGS=""
RUN CGO_ENABLED=0 go build -trimpath -tags "${GOTAGS}" -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/canyon-server ./cmd/canyon-server

# The demo's sample apps (docker-compose.yml's demo-apps, #1490): the canyon CLI and the stand-in trees it
# deploys. Built only for that target; the server image below stays the default, last stage.
FROM build AS cli
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/canyon ./cmd/canyon

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS demo-apps
# curl: run.sh sets the demo's design-system rule over the server's API, as the dashboard does.
RUN apt-get update && apt-get install -y --no-install-recommends curl && rm -rf /var/lib/apt/lists/*
COPY --from=cli /out/canyon /usr/local/bin/canyon
# Canyon's licence, NOTICE, and the licence texts of the third-party code in the binary (THIRD_PARTY_NOTICES).
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/doc/canyon/
COPY demo/apps /demo
ENV HOME=/tmp
ENTRYPOINT ["/demo/run.sh"]

# Not distroless: ingest materialises each upload as a commit with git (design/ARCHITECTURE.md §2).
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 65532 canyon \
 && mkdir -p /var/lib/canyon/keys /var/lib/canyon/repos && chown canyon /var/lib/canyon/keys /var/lib/canyon/repos && chmod 700 /var/lib/canyon/keys /var/lib/canyon/repos
COPY --from=build /out/canyon-server /canyon-server
# Canyon's licence, NOTICE, and the licence texts of the third-party code in the binary (THIRD_PARTY_NOTICES).
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/doc/canyon/
# The server links ee/ (Canyon Enterprise), whose terms, the Elastic License 2.0, go with it.
COPY ee/LICENSE /usr/share/doc/canyon/ee/LICENSE
USER canyon
EXPOSE 7777
ENTRYPOINT ["/canyon-server"]
