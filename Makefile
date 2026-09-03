# Makefile

IMAGE    ?= shim
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

## Build the container image (amd64).
container-build:
	@mkdir -p $(DATA_DIR)
	podman build --platform linux/amd64 --manifest $(IMAGE):dev -f Containerfile .

## Run the shim container locally.
container-run: container-build
	podman run $(RUN_OPTS) $(VOL_OPTS) $(ENV_OPTS) \
		-p 8080:8080 \
		$(IMAGE):dev

## Push the container image manifest to Docker Hub.
container-push:
	podman manifest push --all $(IMAGE):dev docker://docker.io/janharings/$(IMAGE):dev

.PHONY: build test lint container-build container-run container-push
