# Makefile

IMAGE     ?= teddycloud-spotify-shim
GHCR_IMAGE ?= ghcr.io/crowdsalat/teddycloud-spotify-shim
VERSION    ?= v0.1.0
DATA_DIR  ?= $(CURDIR)/container/soloist-data/
ENV_FILE  ?= $(CURDIR)/container/.env
SOURCE_DIR ?= $(DATA_DIR)

# Run flags shared across container targets.
# --userns=keep-id + --user: session files land owned by the host user.
# -v dir:/data:Z: SELinux relabel.
RUN_OPTS := --rm --network host \
            --userns=keep-id --user $(shell id -u):$(shell id -g)
VOL_OPTS := -v $(DATA_DIR):/data:Z
ENV_OPTS := --env-file $(ENV_FILE) -e TEDDYCLOUD_URL

## Build the shim binary.
build:
	go build -trimpath -o bin/shim ./cmd/shim

## Run all unit tests.
test:
	go test ./...

## Run the linter.
lint:
	golangci-lint run ./...

## Build the container image locally (amd64, runnable with podman run).
container-build:
	@mkdir -p $(DATA_DIR)
	podman build --platform linux/amd64 -t $(IMAGE):dev -f Containerfile .

## Run the shim container locally.
container-run: container-build
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

## Deploy the shim to OpenShift (requires oc).
deploy-ocp:
	oc apply -k container/ocp/

.PHONY: build test lint container-build container-run container-push GHCR_PUSH container-push-ghcr container-tag changelog migrate-session deploy-ocp
