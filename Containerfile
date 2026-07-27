# ---- Stage 1: Build go-librespot ----
FROM docker.io/library/golang:1.26-bookworm AS librespot-build

ARG LIBRESPOT_VERSION=v0.7.4

RUN apt-get update && apt-get install -y --no-install-recommends \
        libogg-dev libvorbis-dev libflac-dev libasound2-dev git \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch ${LIBRESPOT_VERSION} \
        https://github.com/devgianlu/go-librespot.git /src/go-librespot

WORKDIR /src/go-librespot
RUN go build -o /usr/local/bin/go-librespot ./cmd/daemon

# ---- Stage 2: Build shim ----
FROM docker.io/library/golang:1.26-bookworm AS shim-build

WORKDIR /src/shim
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true
COPY . .

ARG VERSION=dev
RUN go build -ldflags "-X main.Version=${VERSION}" -o /usr/local/bin/shim ./cmd/shim

# ---- Stage 3: Runtime ----
FROM docker.io/library/debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        libogg0 libvorbis0a libvorbisenc2 libflac12 libasound2 \
        tini ca-certificates curl procps \
    && rm -rf /var/lib/apt/lists/*

COPY --from=librespot-build /usr/local/bin/go-librespot /usr/local/bin/
COPY --from=shim-build /usr/local/bin/shim /usr/local/bin/

RUN mkdir -p /config && chmod 775 /config && chgrp 0 /config
USER 65534:0

ENTRYPOINT ["tini", "--"]
CMD ["shim"]

EXPOSE 8080
VOLUME ["/config"]
