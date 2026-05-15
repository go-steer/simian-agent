SHELL := /usr/bin/env bash
GO    := go
PKGS  := ./...
BIN   := bin/simian

.PHONY: all build test vet tidy clean run-serve fmt ci

all: vet test build

# Run the full CI pipeline (vet, build, lint, test, mod-tidy, vuln) — same
# script GitHub Actions runs. Auto-installs golangci-lint / goimports /
# govulncheck on first use.
ci:
	dev/tools/ci

build:
	@mkdir -p bin
	$(GO) build -o $(BIN) ./cmd/simian

test:
	$(GO) test -count=1 -race $(PKGS)

vet:
	$(GO) vet $(PKGS)

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt $(PKGS)

clean:
	rm -rf bin dist coverage.txt

run-serve: build
	$(BIN) serve
