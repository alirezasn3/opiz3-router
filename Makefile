BIN     := opiz3-router
HOST    ?= root@172.16.0.1
# Version reported by `opiz3-router -version`; taken from the git tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test deploy release

# Static arm64 binary for the Orange Pi Zero 3.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/opiz3-router

test:
	go test ./...

# Copy the binary to the device and restart the service. The binary is
# uploaded under a temporary name because a running executable cannot be
# overwritten in place.
deploy: build
	scp $(BIN) $(HOST):/usr/local/bin/$(BIN).new
	ssh $(HOST) 'mv /usr/local/bin/$(BIN).new /usr/local/bin/$(BIN) && systemctl restart $(BIN)'

# Publish a release: `make release TAG=v1.0.1`. Pushing the tag starts the
# release workflow, which tests, builds and publishes it.
release: test
	@test -n "$(TAG)" || { echo "usage: make release TAG=v1.2.3"; exit 1; }
	@echo "$(TAG)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' || { echo "TAG must look like v1.2.3 or v1.2.3-rc1"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "commit your changes first"; exit 1; }
	git tag $(TAG)
	git push origin HEAD $(TAG)
