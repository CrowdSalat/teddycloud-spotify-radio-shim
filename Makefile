VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -ldflags "-X main.Version=$(VERSION)"
IMAGE         := docker.io/janharings/teddycloud-spotify-radio-shim
GOLANGCI_LINT := $(shell which golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

.PHONY: build test lint container clean

build:
	go build $(LDFLAGS) -o bin/shim ./cmd/shim

test:
	CGO_ENABLED=0 go test -v ./...

lint:
	$(GOLANGCI_LINT) run ./... && echo "✅ lint passed"

container:
	podman build --platform linux/amd64,linux/arm64 -f Containerfile -t $(IMAGE):$(VERSION) .

# Run container-based integration tests.
# Pass CONFIG_DIR to include Phase 3/4 tests (require Spotify auth):
#   make test-integration CONFIG_DIR=/path/to/config
test-integration:
	./scripts/test-integration.sh $(if $(CONFIG_DIR),--config-dir $(CONFIG_DIR),)

clean:
	rm -rf bin/
