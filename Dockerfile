# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build: cross-compiles a static binary, so the build stage always runs on the
# native builder platform and only the Go toolchain cross-targets.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: this layer survives every source-only change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/loki-filtered-mcp ./cmd/server

# ---------------------------------------------------------------------------
# Runtime: alpine. The binary is static and needs nothing from the base, so the
# base is here for the things distroless refuses to have — a shell to exec
# into, wget for a HEALTHCHECK — plus ca-certificates for an https:// Loki.
# ---------------------------------------------------------------------------
FROM alpine:3.24

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 65532 nonroot

COPY --from=build /out/loki-filtered-mcp /usr/local/bin/loki-filtered-mcp

# Numeric, never the name: Kubernetes runAsNonRoot refuses to start a container
# whose image declares its user by name, since it cannot resolve the name to a
# UID beforehand to check it is not 0.
USER 65532:65532

# Mount the config here, read-only:
#   docker run -v ./config.yaml:/etc/loki-filtered-mcp/config.yaml:ro ...
EXPOSE 8080

# Assumes the default listen address; drop it if server.listen moves off :8080.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/loki-filtered-mcp"]
CMD ["-config", "/etc/loki-filtered-mcp/config.yaml"]
