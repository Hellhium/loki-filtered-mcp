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
# Runtime: distroless static — no shell, no package manager, non-root by
# default. The binary is the whole image.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/loki-filtered-mcp /usr/local/bin/loki-filtered-mcp

# Mount the config here, read-only:
#   docker run -v ./config.yaml:/etc/loki-filtered-mcp/config.yaml:ro ...
EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/loki-filtered-mcp"]
CMD ["-config", "/etc/loki-filtered-mcp/config.yaml"]
