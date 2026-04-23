BINARY     := vpn-rebind
IMAGE      := ghcr.io/darkiris4/vpn-rebind
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
PLATFORMS  := linux/amd64,linux/arm64,linux/arm/v7

.PHONY: build run tidy lint test image image-multiarch clean

## build: compile the binary for the current platform
build:
	CGO_ENABLED=0 go build \
		-trimpath \
		-ldflags="-s -w -X main.version=$(VERSION)" \
		-o bin/$(BINARY) \
		./cmd/vpn-rebind

## run: run locally against the system Docker daemon (requires config or env vars)
run: build
	./bin/$(BINARY) $(ARGS)

## tidy: sync go.mod / go.sum
tidy:
	go mod tidy

## lint: run golangci-lint (install separately: https://golangci-lint.run/)
lint:
	golangci-lint run ./...

## test: run unit tests
test:
	go test -race ./...

## image: build a single-arch Docker image for local testing
image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		.

## image-multiarch: build and push a multi-arch image (requires docker buildx)
image-multiarch:
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		--push \
		.

## clean: remove build artifacts
clean:
	rm -rf bin/
