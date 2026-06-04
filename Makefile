VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.Version=$(VERSION)"
IMAGE   := docker.io/janharings/teddycloud-spotify-radio-shim

.PHONY: build test lint container clean

build:
	go build $(LDFLAGS) -o bin/shim ./cmd/shim

test:
	go test ./...

lint:
	go vet ./...

container:
	podman build --platform linux/amd64,linux/arm64 -f Containerfile -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin/
