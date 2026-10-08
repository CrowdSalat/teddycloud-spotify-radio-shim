# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
# Build inputs are exactly go.mod, go.sum, cmd/ and internal/. Copying them per
# directory keeps the build fail-closed: a new top-level path has to be added
# here on purpose instead of silently riding along inside a layer.
FROM docker.io/library/golang:1.26-bookworm AS builder

WORKDIR /src

# Manifests first, so this layer survives source-only edits. go.sum has to be
# in place before downloading, otherwise the fetch is unverified and the
# committed hashes never gate the build.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -o /shim ./cmd/shim

# ── Runtime stage ─────────────────────────────────────────────────────────────
# Debian trixie required: glibc >= 2.38 (Bookworm ships 2.36, too old for Soloist).
FROM docker.io/library/debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      pulseaudio \
      pulseaudio-utils \
      libatomic1 \
      tini \
      ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY --from=builder /shim /shim

# No /data or /cache here on purpose. Both deployment paths create them outside
# the image: the homelab playbook bind-mounts host dirs chowned 0770 to 65534:0,
# and OpenShift creates the mount points and applies fsGroup. Pre-creating them
# would only grant write access to a rootfs that never needs it.
LABEL org.opencontainers.image.source=https://github.com/CrowdSalat/teddycloud-spotify-radio-shim

# Nobody user, root group — OpenShift restricted-v3 SCC compatible. No hardcoded
# UIDs or chowns anywhere above: OpenShift assigns the runtime UID.
USER 65534:0

ENTRYPOINT ["/usr/bin/tini", "--", "/shim"]
