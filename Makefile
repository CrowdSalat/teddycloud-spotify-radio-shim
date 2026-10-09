# Makefile

IMAGE     ?= teddycloud-spotify-shim
GHCR_IMAGE ?= ghcr.io/crowdsalat/teddycloud-spotify-radio-shim
VERSION    ?= v0.1.0
DATA_DIR  ?= $(CURDIR)/container/soloist-data/
CACHE_DIR ?= $(CURDIR)/container/soloist-cache/
ENV_FILE  ?= $(CURDIR)/container/.env
SOURCE_DIR ?= $(DATA_DIR)

# Run flags shared across container targets.
# --userns=keep-id + --user: session files land owned by the host user.
# -v dir:/data:Z: SELinux relabel.
RUN_OPTS := --rm --network host \
            --userns=keep-id --user $(shell id -u):$(shell id -g)
VOL_OPTS := -v $(DATA_DIR):/data:Z -v $(CACHE_DIR):/cache:Z
ENV_OPTS := --env-file $(ENV_FILE) -e TEDDYCLOUD_URL

# ── Local test bench ─────────────────────────────────────────────────────────
# The whole figurine cycle runs on one machine, with no Toniebox: a real
# teddyCloud, this shim, and teddycloud-mock-toniebox as the box.
#
# The mock is a published image rather than a local build, so the rig tests what
# users actually pull. Build it from source with `make mock-build` when changing
# the mock; use `mock up` to run the stack from its current published tag.

BENCH_COMPOSE ?= scripts/bench-compose.yaml
BENCH_TC_API  ?= https://localhost:8443
BENCH_WEB     ?= http://localhost:8080
BENCH_ADDR    ?= http://localhost:9095
BENCH_SHIM    ?= http://localhost:8090
# The figurine the box carries by default. Its content is pointed at the shim's
# /stream by bench-figurine, so a placement drives ffmpeg -> GET /stream -> shim.
BENCH_RUID    ?= E00403500EEA4BF2
BENCH_URI     ?= spotify:track:4cOdK2wGLETKBW3PvgPWqT
MOCK_IMAGE    ?= ghcr.io/crowdsalat/teddycloud-mock-toniebox:latest

## Build the mock box locally, for when changing it. bench-up uses the published
## image by default, so the rig tests what users actually pull; point it at a
## local build with:
##   make mock-build && BENCH_MOCK_IMAGE=localhost/mock-toniebox:dev make bench-up
mock-build:
	podman build -t localhost/mock-toniebox:dev \
		-f ../teddycloud-mock-toniebox/Containerfile \
		../teddycloud-mock-toniebox

## Build the shim binary.
build:
	go build -trimpath -o bin/shim ./cmd/shim

## Start the bench: teddycloud, this shim, and a simulated box.
##
## Afterwards drive the box with ./scripts/cycle.sh, which holds the UID and the
## ports and prints what each action did.
##
## The shim image is public on ghcr.io, so no registry login is needed.
bench-up:
	@$(MAKE) --no-print-directory bench-up-compose
	@$(MAKE) --no-print-directory bench-figurine
	@$(MAKE) --no-print-directory bench-sse-check
	@echo ""
	@echo "bench up:"
	@echo "  teddycloud  $(BENCH_WEB)   (web UI + /api/sse)"
	@echo "  shim        $(BENCH_SHIM)  (/stream, /healthz)"
	@echo "  box control $(BENCH_ADDR)"
	@echo ""
	@echo "  ./scripts/cycle.sh place      put a figurine on the box"
	@echo "  ./scripts/cycle.sh lift       take it off"
	@echo "  ./scripts/cycle.sh cycle 10   10 place/lift cycles, with the report"
	@echo "  ./scripts/cycle.sh report     measurements so far"
	@echo "  ./scripts/cycle.sh log shim   watch the shim's control path"
	@echo "  ./scripts/cycle.sh down       tear the bench down"

## Bring up the three containers from scripts/bench-compose.yaml.
bench-up-compose:
	@podman-compose -f $(BENCH_COMPOSE) up -d
	@# Wait on the SSE endpoint's *headers* only. A plain curl to /api/sse never
	@# returns: it is an event stream, so it holds the connection open and the
	@# probe would time out on every run. -m 2 still reports success because curl
	@# exits 0 once the response has started.
	@for i in $$(seq 1 30); do \
		curl -fsS -m 2 -o /dev/null $(BENCH_WEB)/api/sse 2>/dev/null && break; \
		sleep 1; \
	done
	@echo "teddycloud: up on $(BENCH_WEB)"
	@echo "shim:       up on $(BENCH_SHIM)"

## Point the bench figurine's content at the shim's /stream.
##
## Uses teddyCloud's own content/json/set API rather than writing content.json by
## hand: the source is capped at 255 characters and classified as CT_SOURCE_STREAM
## because it contains "://", which is what makes teddyCloud run ffmpeg against it
## (src/contentJson.c:120-129).
##
## The URL uses the compose service name, so teddyCloud reaches the shim over the
## shared network. That is the whole reason the rig is compose-based: no published
## port and no host.containers.internal, both of which only work when the
## consumer happens to be on the host.
bench-figurine:
	@curl -fsS -m 10 -X POST "$(BENCH_WEB)/content/json/set/$(BENCH_RUID)" \
		--data-urlencode 'source=http://shim:8080/stream?spotify_uri=$(BENCH_URI)' \
		--data-urlencode 'live=true' \
		--data-urlencode 'nocloud=true' \
		--data-urlencode 'tonie_model=CUSTOM_RADIO' \
		>/dev/null && echo "figurine: $(BENCH_RUID) -> shim /stream"
	@curl -fsS -m 10 "$(BENCH_WEB)/content/json/get/$(BENCH_RUID)" \
		| grep -q 'http://shim:8080/stream' \
		&& echo "           verified by reading it back" \
		|| echo "           WARNING: content.json did not take; check teddycloud's log"

## Report whether the shim's SSE subscription is healthy.
##
## teddyCloud caps /api/sse at 8 subscribers (SSE_MAX_CHANNELS,
## include/handler_sse.h:15). It also leaks a slot on every connection that
## fails during its write phase, so the count only ratchets up over a long
## session. Checking here turns "the shim is blind and says nothing" into a
## visible warning, which matters because the failure is otherwise silent.
bench-sse-check:
	@if curl -sS -m 3 -o /dev/null -w '%{http_code}' $(BENCH_WEB)/api/sse 2>/dev/null | grep -q 503; then \
		echo "warning: teddyCloud's SSE pool is full — the shim's control path may be blind"; \
		echo "         8 slots total (include/handler_sse.h:15), and a slot leaks on"; \
		echo "         every failed write, so the pool only refills if teddyCloud restarts"; \
		echo "         podman-compose -f $(BENCH_COMPOSE) restart teddycloud"; \
	elif podman logs --since 30s shim 2>&1 | grep -qs '503 Service Unavailable'; then \
		echo "warning: the shim's SSE subscription was refused (podman logs shim)"; \
	else \
		echo "sse: ok"; \
	fi

## Tear the bench down.
bench-down:
	@./scripts/cycle.sh down 2>/dev/null || true
	@podman-compose -f $(BENCH_COMPOSE) down 2>/dev/null || true
	@echo "bench down"

## Stop the bench without deleting its state (Soloist pairing session, volumes).
bench-stop:
	@podman-compose -f $(BENCH_COMPOSE) stop 2>/dev/null || true
	@echo "bench stopped"

## Run all unit tests.
test:
	go test ./...

## Run the linter.
lint:
	golangci-lint run ./...

## Build the container image locally (amd64, runnable with podman run).
container-build:
	@mkdir -p $(DATA_DIR) $(CACHE_DIR)
	podman build --platform linux/amd64 -t $(IMAGE):dev -f Containerfile .

## Run the shim container locally.
container-run:
	podman run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		-p 8080:8080 \
		$(IMAGE):dev

## Build a multi-arch manifest and push to Docker Hub.
container-push:
	podman build --platform linux/amd64,linux/arm64 --manifest $(IMAGE):dev -f Containerfile .
	podman manifest push --all $(IMAGE):dev docker://docker.io/crowdsalat/$(IMAGE):dev
	podman manifest rm $(IMAGE):dev

## Build, push, and clean up a multi-arch GHCR manifest (usage: GHCR_TAG=x make container-push-ghcr).
GHCR_TAG ?= latest
GHCR_PUSH:
	podman login ghcr.io
	podman build --platform linux/amd64,linux/arm64 \
		--manifest $(GHCR_IMAGE):$(GHCR_TAG) -f Containerfile .
	podman manifest push --all \
		$(GHCR_IMAGE):$(GHCR_TAG) docker://$(GHCR_IMAGE):$(GHCR_TAG)
	podman manifest rm $(GHCR_IMAGE):$(GHCR_TAG)

## Build, push, and clean up the GHCR :latest manifest.
container-push-ghcr: GHCR_TAG=latest
container-push-ghcr: GHCR_PUSH

## Build, push, and clean up the GHCR versioned manifest (VERSION variable).
container-tag: GHCR_TAG=$(VERSION)
container-tag: GHCR_PUSH

## Regenerate the committed CHANGELOG.md from git history (requires git-cliff).
changelog:
	git-cliff -o CHANGELOG.md

## Copy the local paired Soloist session into the OCP pod/PVC (TARGET_POD required).
migrate-session:
	./scripts/migrate-session.sh $(SOURCE_DIR) $(TARGET_POD)

.PHONY: build test lint container-build container-run container-push GHCR_PUSH \
	container-push-ghcr container-tag changelog migrate-session \
	bench-up bench-down bench-stop bench-up-compose bench-figurine bench-sse-check mock-build
