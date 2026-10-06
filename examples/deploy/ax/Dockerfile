# syntax=docker/dockerfile:1.7
#
# Copyright 2026 Google LLC
# Licensed under the Apache License, Version 2.0 (the "License");
# see LICENSE for details.
#
# mast as an AX task image. Build from the repo root:
#
#   docker build -f examples/deploy/ax/Dockerfile -t mast-ax .
#
# Three binaries end up in the image:
#   /usr/local/bin/ax-task-runner  AX's runner, PID 1 (AX always starts this path)
#   /usr/local/bin/ax-mast-entry   wrapper that runs one mast turn safely (see entrypoint/)
#   /usr/local/bin/mast
#
# The runner is built from AX source rather than copied from AX's default
# image, which is not publicly pullable (google/ax#424). That also keeps AX
# out of mast's go.mod.

# AX's go.mod needs a newer toolchain than mast's, hence separate stages.
ARG AX_GO_VERSION=1.27.1
ARG GO_VERSION=1.26.6

# ---- AX runner ----
FROM --platform=$BUILDPLATFORM golang:${AX_GO_VERSION}-alpine AS ax-runner
ARG TARGETOS
ARG TARGETARCH
# Pin to an AX commit; AX's API is v1alpha1 and changes without notice.
ARG AX_REF=ac2332829f22360ff97b0ba34d94dd0dd782f17e
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOBIN=/out \
    go install -trimpath -ldflags "-s -w" github.com/google/ax/cmd/ax-task-runner@${AX_REF} \
 && if [ -d /out/*_* ]; then mv /out/*_*/ax-task-runner /out/; fi

# ---- mast + wrapper ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN go build -trimpath -ldflags "-s -w" -o /out/mast ./cmd/mast \
 && go build -trimpath -ldflags "-s -w" -o /out/ax-mast-entry ./examples/deploy/ax/entrypoint

# ---- Final stage ----
#
# Not distroless: AX's workspace setup shells out to git to clone a
# Workspace's repos. The runner also binds port 80 and writes its setup
# markers under /ax, so the image runs as root, as AX's default image does.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.source="https://github.com/go-steer/mast" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="mast packaged as an AX task image (prototype)."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=builder /out/mast /out/ax-mast-entry /usr/local/bin/

# Matches what AX sets; useful for a plain `docker run`.
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
