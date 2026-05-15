# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

SHELL := /usr/bin/env bash
GO    := go
PKGS  := ./...
BIN   := bin/simian

# Container image publishing — see also .github/workflows/release.yml which
# auto-publishes on `v*` tag push. The Makefile targets are for ad-hoc dev
# builds without cutting a release tag (e.g. `make image-push VERSION=mybranch`).
IMAGE_REGISTRY ?= ghcr.io
IMAGE_NAME     ?= go-steer/simian-agent
VERSION        ?= dev
IMAGE          := $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(VERSION)

.PHONY: all build test vet tidy clean run-serve fmt ci image image-push

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

# Build the container image. Override VERSION (default: dev), IMAGE_REGISTRY
# (default: ghcr.io), or IMAGE_NAME (default: go-steer/simian-agent) as needed.
#
#   make image                          # → ghcr.io/go-steer/simian-agent:dev
#   make image VERSION=v0.1.0-dev       # → ghcr.io/go-steer/simian-agent:v0.1.0-dev
image:
	docker build -t $(IMAGE) .

# Build and push the image. Requires `docker login $(IMAGE_REGISTRY)` first
# (for ghcr.io: `echo $$GITHUB_TOKEN | docker login ghcr.io -u <user> --password-stdin`).
image-push: image
	docker push $(IMAGE)
