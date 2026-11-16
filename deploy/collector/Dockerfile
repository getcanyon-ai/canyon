# canyon-collector (#2808): built from the repository root, docker build -f deploy/collector/Dockerfile .
FROM golang:1.26.11 AS build
WORKDIR /src
COPY go.mod go.sum* ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/canyon-collector ./cmd/canyon-collector

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/canyon-collector /canyon-collector
# Canyon's licence, NOTICE, and the licence texts of the third-party code in the binary (THIRD_PARTY_NOTICES).
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/doc/canyon/
# Canyon's code is AGPL-3.0-only, with contracts/ under Apache-2.0 (LICENSE).
LABEL org.opencontainers.image.licenses="AGPL-3.0-only AND Apache-2.0"
EXPOSE 8080
ENTRYPOINT ["/canyon-collector"]
