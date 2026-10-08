GO ?= go
GOLANGCI_LINT_VERSION ?= v1.61.0
VERSION ?= $(shell sed -n 's/^var version = "\(.*\)"$$/\1/p' cmd/pgexporter/main.go)

.PHONY: all build test race lint fmt golden budget clean

all: fmt build test lint

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/pgexporter ./cmd/pgexporter

test:
	$(GO) test ./... -count=1

race:
	$(GO) test ./... -race -count=1

# Rewrites the golden exposition files. Read the diff: it is the contract
# Prometheus parses, so a change here is a change to what every dashboard and
# alert rule sees.
golden:
	$(GO) test ./internal/collector/ -update -count=1

budget:
	$(GO) run ./cmd/pgexporter -series-budget

fmt:
	gofmt -l -w .
	$(GO) vet ./...

lint:
	$(GO) run github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

clean:
	rm -rf bin
