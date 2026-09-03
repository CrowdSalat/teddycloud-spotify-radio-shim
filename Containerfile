# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /shim ./cmd/shim

# ── Runtime stage ─────────────────────────────────────────────────────────────
# Debian trixie required: glibc >= 2.38 (Bookworm ships 2.36, too old for Soloist).
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      pulseaudio \
      libatomic1 \
      tini \
      ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY --from=builder /shim /shim

# Nobody user, root group — OpenShift restricted-v2 SCC compatible.
USER 65534:0

ENTRYPOINT ["/usr/bin/tini", "--", "/shim"]
