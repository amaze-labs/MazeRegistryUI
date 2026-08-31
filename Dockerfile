# syntax=docker/dockerfile:1.7

# ---------------------------------------------------------------------------
# Builder
# ---------------------------------------------------------------------------
# Alpine is chosen over -bookworm purely as a build host: the binary is fully
# static (CGO_ENABLED=0) so the builder's libc is irrelevant, and the alpine
# variant is ~4x smaller to pull, which keeps cold CI builds fast.
# BUILDPLATFORM pins the builder to the machine's native architecture so that
# cross-compiling to linux/arm64 uses Go's own cross-compiler instead of QEMU
# emulating the whole toolchain.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /src

# Dependency layer: only invalidated when the module files change, so editing
# Go source never re-downloads the module graph.
# go.sum may legitimately not exist yet (no external deps) -> the glob keeps
# COPY from failing in that case.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

# Injected automatically by buildx for every entry of --platform.
ARG TARGETOS
ARG TARGETARCH

# Cache mounts are shared across builds: /go/pkg/mod holds the module cache and
# /root/.cache/go-build the compiler's object cache. They are mount-time only,
# so nothing from them ends up in the image layer.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/amaze-labs/MazeRegistryUI/internal/version.Version=${VERSION} \
        -X github.com/amaze-labs/MazeRegistryUI/internal/version.Commit=${COMMIT} \
        -X github.com/amaze-labs/MazeRegistryUI/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/mazeregistryui \
      ./cmd/mazeregistryui

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# distroless/static already ships /etc/ssl/certs/ca-certificates.crt (needed for
# outbound HTTPS to the browsed registries) and /etc/passwd entries for the
# nonroot user, plus /tmp and tzdata. Nothing else has to be copied.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

LABEL org.opencontainers.image.title="MazeRegistryUI" \
      org.opencontainers.image.description="Single-binary web UI for browsing OCI / Docker Distribution 3.x registries" \
      org.opencontainers.image.source="https://github.com/amaze-labs/MazeRegistryUI" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /out/mazeregistryui /mazeregistryui

# uid 65532 = the "nonroot" user baked into the distroless image. Declared
# numerically as well so Kubernetes runAsNonRoot admission can verify it
# without resolving /etc/passwd.
USER 65532:65532

# Default config path; override with -config or MRUI_CONFIG.
ENV MRUI_CONFIG=/etc/mazeregistryui/config.yaml

EXPOSE 8080

# No HEALTHCHECK on purpose.
# distroless/static has no shell, no curl and no wget, so every form of
# `HEALTHCHECK CMD ...` would resolve to a non-existent binary and mark the
# container unhealthy forever. Probe the app from the orchestrator instead:
#   - Compose: see the `healthcheck` block in compose.yaml
#   - Kubernetes: httpGet livenessProbe/readinessProbe on port 8080 path /healthz
# TODO(optional): if a `-healthcheck` self-probe flag is ever added to the Go
# binary (it dials http://127.0.0.1:8080/healthz and exits 0/1), replace this
# comment with:
#   HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
#     CMD ["/mazeregistryui", "-healthcheck"]
# That flag does NOT exist today - do not enable the line above until it does.

ENTRYPOINT ["/mazeregistryui"]
