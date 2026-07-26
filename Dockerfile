# syntax=docker/dockerfile:1.7
#
# Copyright 2026 Google LLC
# Licensed under the Apache License, Version 2.0 (the "License");
# see LICENSE for details.
#
# Originally derived from go-steer/core-agent@c5efbb9e:Dockerfile.
# Simplified for the mast spike: single variant, no build-tag matrix,
# no internal/version package ld-flag injection (mast-prototype does
# not yet ship a version package). Distroless + static-nonroot pattern
# preserved verbatim.
#
# Multi-stage distroless build. Final image is
# gcr.io/distroless/static-debian12:nonroot — no shell, no package
# manager, no userland; UID 65532 runs the binary. Every ADK v2 dep is
# pure-Go so we can stay on the static distroless variant.

# ---- Builder stage ----
ARG GO_VERSION=1.26.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Cache module downloads in a separate layer so iterative source
# changes don't re-fetch dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Bring in the rest of the source.
COPY . .

# Cross-compile target. Set by `docker buildx build --platform` when
# building multi-arch images. Without buildx these default to the
# host's GOOS/GOARCH.
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 is mandatory — we want a fully-static binary that
# drops into distroless/static without any glibc/musl runtime.
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# -s -w strips DWARF + symbol table to shrink the binary by ~30%.
# -trimpath strips absolute paths in stack traces (avoids leaking the
# build host's filesystem layout).
RUN go build \
    -ldflags "-s -w" \
    -trimpath \
    -o /out/mast \
    ./cmd/mast

# ---- Final stage ----
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/go-steer/mast" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="mast — agent-infrastructure substrate for unattended, library-embedded, multi-provider workloads."

COPY --from=builder /out/mast /usr/local/bin/mast

# Bundle the example workload so the default `docker run` has
# something to load. Overlays / production deploys should project a
# real workload dir via a volume or ConfigMap at /workspace/workload
# and point --workload at it.
COPY --from=builder /src/examples/workloads/gke-triage /workspace/workload

WORKDIR /workspace

# nonroot is already set by the base image's :nonroot tag; re-declared
# for clarity + insulation against future base-image default changes.
USER nonroot:nonroot

# Default entrypoint runs mast with the bundled example workload,
# echo model, no auth (dev only). Kubernetes deployments override
# with concrete args + MAST_INJECT_TOKEN.
ENTRYPOINT ["/usr/local/bin/mast"]
CMD ["--workload=/workspace/workload", "--model=echo", "--listen=:7777"]
