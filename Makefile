VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -ldflags "-X main.Version=$(VERSION)"
IMAGE         := docker.io/crowdsalat/teddycloud-spotify-radio-shim
GOLANGCI_LINT := $(shell which golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

.PHONY: build test lint container push run-local clean

build:
	go build $(LDFLAGS) -o bin/shim ./cmd/shim

test:
	CGO_ENABLED=0 go test -v ./...

lint:
	$(GOLANGCI_LINT) run ./... && echo "✅ lint passed"

container:
	podman build --platform linux/amd64,linux/arm64 --manifest $(IMAGE):$(VERSION) -f Containerfile .

push:
	podman manifest push --all $(IMAGE):$(VERSION) docker://$(IMAGE):$(VERSION)
	podman manifest rm $(IMAGE):$(VERSION)

# Run container-based integration tests.
# Pass CONFIG_DIR to include Phase 3/4 tests (require Spotify auth):
#   make test-integration CONFIG_DIR=/path/to/config
test-integration:
	./scripts/test-integration.sh $(if $(CONFIG_DIR),--config-dir $(CONFIG_DIR),)

# Credentials and go-librespot state are persisted in a named volume.
# Override port with: make run-local LOCAL_PORT=9090
run-local:
	podman volume create $(VOLUME_NAME) 2>/dev/null || true
	podman run --rm -it \
		--name shim-local \
		-p $(LOCAL_PORT):8080 \
		-p $(OAUTH_CALLBACK_PORT):38161 \
		-v $(VOLUME_NAME):/config:z \
		-e LOG_LEVEL=debug \
		$(IMAGE):$(VERSION)

clean:
	rm -rf bin/
