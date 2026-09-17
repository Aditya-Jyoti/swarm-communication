# syntax=docker/dockerfile:1
#
# Multi-stage build for swarm-net.
#
#   docker build --target node           -t swarm-net/node .
#   docker build --target control-center -t swarm-net/control-center .
#
# Two targets rather than one image with both binaries: each image carries only
# the program it runs and has an unambiguous ENTRYPOINT, so `docker run
# swarm-net/node` can never accidentally start the wrong process. Both targets
# share the `build` stage, so Compose compiles the Go code once and the second
# target is a cache hit.

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.22

# ---------------------------------------------------------------------------
# build: compile static binaries.
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Module files first so dependency download is cached independently of source
# edits. go.sum may not exist while the module has no dependencies; the glob
# keeps COPY from failing in that case.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY pkg ./pkg
COPY web ./web

ARG VERSION=dev
# CGO_ENABLED=0 gives fully static binaries: no libc to match in the runtime
# image. -trimpath and -s -w strip local paths and debug tables (smaller image,
# reproducible output).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/swarm-node ./cmd/swarm-node && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/control-center ./cmd/control-center

# ---------------------------------------------------------------------------
# runtime: alpine rather than distroless/static.
#
# distroless would be ~5 MB smaller, but it has no shell, and we need one:
#  - the node entrypoint resolves a readable per-replica identity via DNS
#    (deploy/node-entrypoint.sh), which a scaled Compose service cannot express
#    in YAML;
#  - busybox wget backs the Compose healthcheck on the control center;
#  - an engineer can `docker exec` in to run nslookup/ping during chaos.
#
# Signals still reach the Go process: the ENTRYPOINT is exec-form (no
# `sh -c` wrapper), and the node script ends in `exec`, so the binary is PID 1.
# Go installs its own SIGTERM/SIGINT handlers, so the "PID 1 ignores signals
# without a handler" rule does not bite, and neither binary forks children that
# would need reaping.
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION} AS runtime
# ca-certificates is not needed (no outbound TLS); tzdata is not needed (UTC logs).
# Fixed non-root uid so file ownership is predictable across rebuilds.
RUN addgroup -S -g 10001 swarm && adduser -S -D -H -u 10001 -G swarm swarm
USER 10001:10001

# ---------------------------------------------------------------------------
# node: one swarm member.
# ---------------------------------------------------------------------------
FROM runtime AS node
COPY --from=build /out/swarm-node /usr/local/bin/swarm-node
COPY --chmod=0555 deploy/node-entrypoint.sh /usr/local/bin/node-entrypoint
EXPOSE 7000
ENTRYPOINT ["/usr/local/bin/node-entrypoint"]

# ---------------------------------------------------------------------------
# control-center: coordinator + dashboard (web/static is embedded in the binary).
# ---------------------------------------------------------------------------
FROM runtime AS control-center
COPY --from=build /out/control-center /usr/local/bin/control-center
EXPOSE 7000 8080
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/control-center"]
