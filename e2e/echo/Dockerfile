# The E2E echo: standard library only, so it builds from its own directory with no module, as e2e/stub does.
FROM golang:1.26.11 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /echo main.go

FROM scratch
COPY --from=build /echo /echo
ENTRYPOINT ["/echo"]
