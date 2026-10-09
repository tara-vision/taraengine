.PHONY: build test lint vuln coverage-gate hygiene integration snapshot clean

BINARY=taraengine
PKG=github.com/tara-vision/taraengine/internal/version
GOBIN=$(shell go env GOPATH)/bin
GOLANGCI ?= $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)
GOVULNCHECK ?= $(shell command -v govulncheck 2>/dev/null || echo $(GOBIN)/govulncheck)
GORELEASER ?= $(shell command -v goreleaser 2>/dev/null || echo $(GOBIN)/goreleaser)
# The release tag without its leading v, which is what goreleaser stamps; "dev" where git knows no tag.
VERSION ?= $(or $(shell git describe --tags --dirty 2>/dev/null | sed 's/^v//'),dev)
LDFLAGS=-s -w -X $(PKG).Version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/taraengine

test:
	go test -race ./...

lint:
	$(GOLANGCI) run ./...

vuln:
	$(GOVULNCHECK) ./...

coverage-gate:
	bash scripts/coverage-gate.sh

hygiene:
	bash scripts/hygiene.sh

# The real pinned runtime and the one-megabyte test model; needs network once (see scripts/integration.sh).
integration: build
	bash scripts/integration.sh

snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish,sign

clean:
	rm -f $(BINARY)
	rm -rf dist
