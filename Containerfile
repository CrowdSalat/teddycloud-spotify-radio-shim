# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
FROM docker.io/library/golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY . .
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

LABEL org.opencontainers.image.source=https://github.com/CrowdSalat/teddycloud-spotify-radio-shim

# Nobody user, root group — OpenShift restricted-v2 SCC compatible.
USER 65534:0

ENTRYPOINT ["/usr/bin/tini", "--", "/shim"]
