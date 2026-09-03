# Makefile

IMAGE    ?= teddycloud-spotify-shim
DATA_DIR ?= $(CURDIR)/container/data
ENV_FILE ?= $(CURDIR)/container/.env

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
	podman manifest push --all $(IMAGE):dev docker://docker.io/janharings/$(IMAGE):dev
	podman manifest rm $(IMAGE):dev

.PHONY: build test lint container-build container-run container-push
