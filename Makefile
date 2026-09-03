# Makefile — Sep 2026
#
# Targets for local testing of Spotify Soloist (Blocker 1 & 2 from DESIGN.md).
# Soloist binaries cannot be redistributed, so this builds a local-only image
# that downloads the binary from Spotify's CDN at build time.
#
# The session data (device auth) lives in container/soloist-data and is
# gitignored.

IMAGE     ?= soloist-test
DATA_DIR  ?= $(CURDIR)/container/soloist-data

# Spotify Connect discovery needs host networking (mDNS is not routable
# through the default bridge).
#
# Ownership/SELinux:
#   --userns=keep-id --user <uid>:<gid>  run the container as the host user, so
#                                        session files land owned by you (the
#                                        image default USER 65534 is overridden).
#   -v dir:/data:Z                        relabel the volume for SELinux.
# No --security-opt label=disable needed with keep-id + :Z.
RUN_OPTS    := --rm --network host --userns=keep-id --user $(shell id -u):$(shell id -g)
VOL_OPTS    := -v $(DATA_DIR):/data:Z
ENV_OPTS    := -e SOLOIST_API_KEY

## Build the local Soloist test image (downloads binary from Spotify CDN).
soloist-build:
	@mkdir -p $(DATA_DIR)
	podman build --platform linux/amd64 -t $(IMAGE) -f Containerfile.soloist-test .

## One-time Spotify pairing. Start soloist, then select the device from the
## Spotify app on your phone (on the same LAN). Session is stored in $(DATA_DIR).
soloist-pair: soloist-build
	podman run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /cache --pair

## Play a single track to verify the stored session works.
soloist-track:
	podman run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /cache \
			--single-track "$(URI)"

## Run soloist as a long-lived Spotify Connect device (Blocker 2 testing).
soloist-connect: soloist-build
	podman run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /cache --ws 127.0.0.1:0

## Show the flags applied to every run target (useful for debugging).
soloist-network:
	@echo "RUN_OPTS= $(RUN_OPTS)"
	@echo "VOL_OPTS= $(VOL_OPTS)"

# ─── Blocker 2: PulseAudio null sink capture test ────────────────────────

IMAGE_BLOCKER2 ?= soloist-blocker2
RECORD_SECONDS ?= 5

## Build the Blocker 2 test image (PulseAudio + Soloist + parec + ffmpeg).
blocker2-build:
	@mkdir -p $(DATA_DIR)
	podman build --platform linux/amd64 -t $(IMAGE_BLOCKER2) \
		-f Containerfile.blocker2 .

## Run the Blocker 2 test. Records from virtual_out.monitor and validates PCM.
##   make blocker2-build   # build image first
##   make blocker2-test    # run the test
##   SPOTIFY_URI=spotify:album:XXX make blocker2-test  # with real playback
blocker2-test: blocker2-build
	podman run --rm --network host \
		--userns=keep-id --user $(shell id -u):$(shell id -g) \
		-v $(DATA_DIR):/data:Z \
		-e SOLOIST_API_KEY \
		-e SPOTIFY_URI \
		-e RECORD_SECONDS=$(RECORD_SECONDS) \
		$(IMAGE_BLOCKER2)

## Stream Soloist's audio to localhost:8000 for listening.
##   make stream-test-build   # build image first
##   make stream-test         # run; then ffplay http://localhost:8000
IMAGE_STREAM ?= soloist-stream
LISTEN_PORT ?= 8000

stream-test-build:
	podman build --platform linux/amd64 -t $(IMAGE_STREAM) -f Containerfile.blocker2 .

stream-test: stream-test-build
	podman run --rm --network host \
		--userns=keep-id --user $(shell id -u):$(shell id -g) \
		-v $(DATA_DIR):/data:Z \
		-e SOLOIST_API_KEY \
		-e SOLOIST_DEVICE_NAME=stream-test \
		-e LISTEN_PORT=$(LISTEN_PORT) \
		--entrypoint stream-test.sh \
		$(IMAGE_STREAM)

.PHONY: soloist-build soloist-pair soloist-track soloist-connect soloist-network \
        blocker2-build blocker2-test stream-test-build stream-test
