# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/apt-cachealot ./cmd/apt-cachealot \
 && mkdir /cache-dir

# The binary is static and needs only CA certificates (https upstreams), a non-root
# user and a writable /cache. distroless/static has exactly that: about 2 MB, no shell.
FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.licenses="GPL-3.0-or-later"
# the volume keeps the ownership of the directory it is created from (uid 65532)
COPY --from=build --chown=65532:65532 /cache-dir /cache
COPY --from=build /out/apt-cachealot /usr/local/bin/apt-cachealot
COPY LICENSE THIRD_PARTY_LICENSES /licenses/
VOLUME /cache
EXPOSE 3142
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/usr/local/bin/apt-cachealot", "-healthcheck"]
# extra flags can be appended to `docker run`, e.g. -max-size 50GB
ENTRYPOINT ["/usr/local/bin/apt-cachealot", "-dir", "/cache", "-listen", ":3142"]
