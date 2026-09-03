# Makefile — Sep 2026
#
# Targets for local testing of Spotify Soloist (Blocker 1 & 2 from DESIGN.md).
# Soloist binaries cannot be redistributed, so this builds a local-only image
# that downloads the binary from Spotify's CDN at build time.
#
# The session data (auth + cache, the latter at /data/cache) lives in
# container/soloist-data and is gitignored — never commit it.

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
PODMAN      ?= podman
RUN_OPTS    := --rm --network host --userns=keep-id --user $(shell id -u):$(shell id -g)
VOL_OPTS    := -v $(DATA_DIR):/data:Z
ENV_OPTS    := -e SOLOIST_API_KEY

## Build the local Soloist test image (downloads binary from Spotify CDN).
soloist-build:
	@mkdir -p $(DATA_DIR)
	$(PODMAN) build --platform linux/amd64 -t $(IMAGE) -f Containerfile.soloist-test .

## One-time Spotify pairing. Start soloist, then select the device from the
## Spotify app on your phone (on the same LAN). Session is stored in $(DATA_DIR).
soloist-pair: soloist-build
	$(PODMAN) run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /data/cache --pair

## Play a single track to verify the stored session works.
soloist-track:
	$(PODMAN) run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /data/cache \
			--single-track "$(URI)"

## Run soloist as a long-lived Spotify Connect device (Blocker 2 testing).
soloist-connect: soloist-build
	$(PODMAN) run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		$(IMAGE) \
		soloist --device-name shim-test --api-key "$$SOLOIST_API_KEY" \
			--data-dir /data --cache-dir /data/cache --ws 127.0.0.1:0

## Show the flags applied to every run target (useful for debugging).
soloist-network:
	@echo "PODMAN  = $(PODMAN)"
	@echo "RUN_OPTS= $(RUN_OPTS)"
	@echo "VOL_OPTS= $(VOL_OPTS)"

.PHONY: soloist-build soloist-pair soloist-track soloist-connect soloist-network
