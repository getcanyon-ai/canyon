# The E2E stub: standard library only, so it builds from its own directory with no module.
FROM golang:1.26.11 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /stub main.go

FROM scratch
COPY --from=build /stub /stub
ENTRYPOINT ["/stub"]
