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
# Three processes make a figurine cycle reproducible without a physical box:
# teddyCloud (real, so the RTNL→SSE translation and the ffmpeg pull are real),
# the shim (the code under test), and cmd/mock-toniebox acting as the box.
#
# Ports are split so nothing collides with a shim already on 8080: teddyCloud's
# web listener keeps 8080 because that is where /api/sse and the GUI live, and
# the shim moves to 8090.

BENCH_DIR     ?= $(CURDIR)/.bench
BENCH_TC      ?= teddycloud-e2e
BENCH_ADDR    ?= http://localhost:9095
BENCH_SHIM    ?= http://localhost:8090
BENCH_TEDDY   ?= https://localhost:8443
BENCH_WEB     ?= http://localhost:8080
# The figurine the box carries by default. Its content.json points at the shim,
# so a placement drives ffmpeg → GET /stream → the shim.
BENCH_RUID    ?= E00403500EEA4BF2
BENCH_URI     ?= spotify:track:4cOdK2wGLETKBW3PvgPWqT
# The box reaches teddyCloud's HTTPS API listener, which is the only one behind
# the client-certificate gate; host.containers.internal is how the container
# reaches the shim on the host.
BENCH_TC_API  ?= https://localhost:8443
# Passed through to the shim. 0 means "pair for real", which needs PulseAudio or
# PipeWire-pulse on the host; 1 serves a synthesized tone instead.
BENCH_STATIC_STREAM ?= $${STATIC_STREAM:-0}

## Build the shim binary.
build:
	go build -trimpath -o bin/shim ./cmd/shim

## Start a local Toniebox test bench: teddycloud, the shim, and a simulated box.
##
## Afterwards drive the box with ./scripts/cycle.sh — it holds the UID and ports
## and prints what each action did. Requires podman and a paired Soloist session
## in $(BENCH_DIR)/data; the shim attempts real pairing on first start.
bench-up:
	@mkdir -p $(BENCH_DIR)/data $(BENCH_DIR)/cache $(BENCH_DIR)/logs
	@go build -trimpath -o $(BENCH_DIR)/shim ./cmd/shim
	@go build -trimpath -o $(BENCH_DIR)/mock-toniebox ./cmd/mock-toniebox
	@chmod +x scripts/bench-shim.sh scripts/bench-box.sh scripts/cycle.sh
	@$(MAKE) --no-print-directory bench-teddycloud
	@$(MAKE) --no-print-directory bench-shim
	@$(MAKE) --no-print-directory bench-box
	@$(MAKE) --no-print-directory bench-figurine
	@$(MAKE) --no-print-directory bench-sse-check
	@echo ""
	@echo "bench up:"
	@echo "  teddycloud  $(BENCH_WEB)  (web UI + /api/sse)"
	@echo "  shim        $(BENCH_SHIM)  (/stream, /healthz)"
	@echo "  box control $(BENCH_ADDR)"
	@echo ""
	@echo "  ./scripts/cycle.sh place      put a figurine on the box"
	@echo "  ./scripts/cycle.sh lift       take it off"
	@echo "  ./scripts/cycle.sh cycle 10   10 place/lift cycles, with the report"
	@echo "  ./scripts/cycle.sh report     measurements so far"
	@echo "  ./scripts/cycle.sh log shim   watch the shim's control path"
	@echo "  ./scripts/cycle.sh down       tear the bench down"

## Start the teddyCloud container the bench talks to.
bench-teddycloud:
	@if podman container exists $(BENCH_TC) 2>/dev/null; then \
		podman start $(BENCH_TC) >/dev/null; \
	else \
		podman-compose -f scripts/teddycloud-compose.yaml up -d; \
	fi
	@# Wait on the SSE endpoint's *headers* only. A plain curl to /api/sse never
	# returns: it is an event stream, so it holds the connection open and the
	# probe would time out on every run. -m 2 with -o /dev/null still reports
	# success because curl returns 0 once the response starts.
	@for i in $$(seq 1 30); do \
		curl -fsS -m 2 -o /dev/null $(BENCH_WEB)/api/sse 2>/dev/null && break; \
		sleep 1; \
	done
	@echo "teddycloud: $(BENCH_TC) up on $(BENCH_WEB)"

## Start the shim.
##
## Real pairing is the default: with SOLOIST_API_KEY set the shim pairs on first
## run and reuses the session in .bench/data afterwards. It needs a PulseAudio or
## PipeWire-pulse server, because the recorder reads the virtual sink.
##
## STATIC_STREAM=1 substitutes a synthesized tone for the audio and skips the
## audio server entirely. The transport and the control path are identical either
## way, so this is the fallback for a host with no sound stack:
##   make bench-up STATIC_STREAM=1
##
## The shim is launched through the script rather than inline: an inline
## `setsid … &` inside a make recipe leaves the recipe's shell holding the
## child's inherited descriptors, so make never returns even though the shim is
## up and healthy.
bench-shim:
	@./scripts/bench-shim.sh

## Start cmd/mock-toniebox as the box. Launched via script for the same reason as
## bench-shim: an inline background process wedges make.
bench-box:
	@./scripts/bench-box.sh

## Point the bench figurine's content at the shim's /stream.
##
## Uses teddyCloud's own content/json/set API rather than writing content.json by
## hand: the source is capped at 255 characters and classified as CT_SOURCE_STREAM
## because it contains "://", which is what makes teddyCloud run ffmpeg against it
## (src/contentJson.c:120-129).
bench-figurine:
	@curl -fsS -m 10 -X POST "$(BENCH_WEB)/content/json/set/$(BENCH_RUID)" \
		--data-urlencode 'source=http://host.containers.internal:8090/stream?spotify_uri=$(BENCH_URI)' \
		--data-urlencode 'live=true' \
		--data-urlencode 'nocloud=true' \
		--data-urlencode 'tonie_model=CUSTOM_RADIO' \
		>/dev/null && echo "figurine: $(BENCH_RUID) -> shim /stream"
	@curl -fsS -m 10 "$(BENCH_WEB)/content/json/get/$(BENCH_RUID)" \
		| grep -q 'host.containers.internal' \
		&& echo "           verified by reading it back" \
		|| echo "           WARNING: content.json did not take; check teddycloud's log"

## Report whether the shim's SSE subscription is healthy.
##
## teddyCloud caps /api/sse at 8 subscribers (SSE_MAX_CHANNELS,
## include/handler_sse.h:15) and answers 503 beyond that. The shim holds one and
## the mock box can hold another, but reconnect churn during startup is enough to
## fill the pool and have each kick the other off — which looks exactly like a
## silent defect. Checking here turns that into a visible warning.
bench-sse-check:
	@if curl -sS -m 3 -o /dev/null -w '%{http_code}' $(BENCH_WEB)/api/sse 2>/dev/null | grep -q 503; then \
		echo "warning: teddyCloud's SSE pool is full — the shim's control path may be blind"; \
		echo "         close any stray 'podman logs -f | curl /api/sse' or captured feeds"; \
		echo "         8 slots total (include/handler_sse.h:15)"; \
	elif grep -qs '503 Service Unavailable' $(BENCH_DIR)/logs/shim.log; then \
		echo "warning: the shim's SSE subscription was refused (see .bench/logs/shim.log)"; \
	else \
		echo "sse: ok"; \
	fi

## Tear the bench down.
bench-down:
	@./scripts/cycle.sh down 2>/dev/null || true
	@rm -rf $(BENCH_DIR)
	@echo "bench down (removed $(BENCH_DIR))"

## Stop the bench's processes without deleting its state (pairing session, logs).
bench-stop:
	@./scripts/cycle.sh down 2>/dev/null || true

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
	bench-up bench-down bench-stop bench-teddycloud bench-shim bench-box bench-figurine bench-sse-check
