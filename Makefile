# Makefile for llmbox. Run `make help` for the list of targets.

# --- configuration -----------------------------------------------------------

BINARY      := llmbox-server
PKG         := ./cmd/llmbox-server
MCP_BINARY  := llmbox-mcp
MCP_PKG     := ./cmd/llmbox-mcp
SPOKE_BINARY := llmbox-spoke
SPOKE_PKG    := ./cmd/llmbox-spoke
GUEST_BINARY := llmbox-guest
GUEST_PKG    := ./cmd/llmbox-guest
IMAGE       := llmbox
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COVERPROFILE := coverage.out

# The admin SPA dist embedded into the hub binary. It is generated, not
# committed; targets that need it depend on this file (see its rule below).
WEBDIST := internal/hub/webdist/index.html

# Build a static binary, matching the Dockerfile. The version is injected into
# main.version (the same variable GoReleaser overrides on a tagged release), so a
# local `make build` reports the git-described version instead of "dev".
GO_BUILD_ENV := CGO_ENABLED=0
GO_BUILD_FLAGS := -trimpath -ldflags="-s -w -X main.version=$(VERSION)"

# --- meta --------------------------------------------------------------------

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# --- build -------------------------------------------------------------------

.PHONY: build
build: build-hub build-spoke build-mcp build-guest ## Build all llmbox binaries (hub, spoke, mcp, guest).

.PHONY: build-hub
build-hub: $(WEBDIST) ## Build the hub (llmbox-server) binary into ./$(BINARY).
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(BINARY) $(PKG)

.PHONY: build-mcp
build-mcp: ## Build the stand-alone llmbox-mcp binary into ./$(MCP_BINARY).
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MCP_BINARY) $(MCP_PKG)

.PHONY: build-spoke
build-spoke: ## Build the stand-alone llmbox-spoke binary into ./$(SPOKE_BINARY).
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(SPOKE_BINARY) $(SPOKE_PKG)

.PHONY: build-guest
build-guest: ## Build the stand-alone llmbox-guest binary into ./$(GUEST_BINARY).
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(GUEST_BINARY) $(GUEST_PKG)

.PHONY: web
web: ## Rebuild the admin web app into internal/hub/webdist (generated, not committed; embedded at go build).
	cd web && npm install && npm run build

# The dist is generated, not committed; anything that runs the hub's tests (or
# builds a binary meant to serve the UI) needs it present. This sentinel builds
# it only when missing — run `make web` explicitly to pick up web/ changes.
$(WEBDIST):
	cd web && npm install && npm run build

.PHONY: web-test
web-test: ## Run the admin web app unit tests (Vitest + Testing Library).
	cd web && npm install && npm test

.PHONY: web-cover
web-cover: ## Run the web app tests with coverage and print the line total.
	cd web && npm install && npm run coverage

.PHONY: install
install: ## Install the hub, mcp, spoke, and guest binaries into $GOPATH/bin.
	go install $(GO_BUILD_FLAGS) $(PKG) $(MCP_PKG) $(SPOKE_PKG) $(GUEST_PKG)

.PHONY: run
run: ## Run the server (use CONFIG=path to pick a config file).
	go run $(PKG) $(if $(CONFIG),--config $(CONFIG),)

# --- quality -----------------------------------------------------------------

.PHONY: fmt
fmt: ## Format all Go source.
	go fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean.
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum.
	go mod tidy

.PHONY: docs
docs: ## Check function docs are in sync (requires codespec).
	codespec check

.PHONY: lint
lint: fmt-check vet ## Run all static checks (fmt-check, vet).

# --- test --------------------------------------------------------------------

.PHONY: test
test: $(WEBDIST) ## Run the unit test suite.
	go test ./...

.PHONY: test-integration
test-integration: ## Run integration tests (needs Docker; builds a minimal guest-only box image; see integration_test.go).
	go test -tags integration ./...

# Live Firecracker box conformance + regression tests. Needs a KVM host: the
# firecracker binary, /dev/kvm, and a guest kernel + rootfs. The artifacts are
# built on demand by scripts/firecracker/*.sh (see the firecracker-assets target)
# and cached under $(FC_DIR); override FC_KERNEL/FC_ROOTFS to point at your own
# (e.g. the full Debian server pair). Run as root to exercise the real TAP/NAT
# egress path (unprivileged boots control-only boxes). See docs/firecracker.md.
FC_DIR    ?= $(HOME)/fc-assets
FC_KERNEL ?= $(FC_DIR)/vmlinux
FC_ROOTFS ?= $(FC_DIR)/rootfs.ext4
FC_BIN    ?= $(HOME)/.local/bin/firecracker

# The production Debian box is split into a slow-changing base rootfs (cached in
# GHCR keyed on its inputs) and a cheap guest payload drive rebuilt on every guest
# change. FC_BASE_REPO is the GHCR artifact the base is pulled from before falling
# back to a local build. Boot a box with --rootfs $(FC_BASE) --payload $(FC_PAYLOAD).
FC_BASE      ?= $(FC_DIR)/base-rootfs.ext4
FC_PAYLOAD   ?= $(FC_DIR)/payload.ext4
FC_BASE_REPO ?= ghcr.io/clems4ever/llmbox-fc-base

# Fetch the firecracker binary into ~/.local/bin if it isn't there yet.
$(FC_BIN):
	DEST="$(dir $(FC_BIN))" scripts/firecracker/fetch-firecracker.sh

# build-conformance-rootfs.sh emits BOTH the guest kernel and the rootfs in one
# run, so a single grouped rule (&:) produces the pair — only when either is
# missing. Rebuild by deleting the files (or `rm -rf $(FC_DIR)`).
$(FC_KERNEL) $(FC_ROOTFS) &:
	OUT="$(FC_DIR)" scripts/firecracker/build-conformance-rootfs.sh

.PHONY: firecracker-assets
firecracker-assets: $(FC_BIN) $(FC_KERNEL) $(FC_ROOTFS) ## Build the firecracker binary + conformance kernel/rootfs if missing (cached in $(FC_DIR)).

# Production Debian box assets. The base is pulled from GHCR (keyed on its input
# hash) when available and only built locally on a cache miss — so a guest change,
# which rebuilds only the cheap payload, never rebuilds the multi-GiB base.
$(FC_BASE):
	@key=$$(scripts/firecracker/asset-key.sh); \
	ref="$(FC_BASE_REPO):$$key"; \
	if command -v oras >/dev/null 2>&1 && oras pull -o "$(FC_DIR)" "$$ref" 2>/dev/null; then \
		echo ">> pulled cached base rootfs from $$ref"; \
	else \
		echo ">> no cached base at $$ref (or oras missing); building locally"; \
		OUT="$(FC_DIR)" scripts/firecracker/build-base-rootfs.sh; \
	fi

$(FC_PAYLOAD):
	OUT="$(FC_DIR)" scripts/firecracker/build-payload-drive.sh

.PHONY: firecracker-debian-assets
firecracker-debian-assets: $(FC_BASE) $(FC_PAYLOAD) ## Pull-or-build the Debian base rootfs + guest payload drive for a production Firecracker box.

.PHONY: test-firecracker
test-firecracker: firecracker-assets ## Build the firecracker artifacts if missing, then run the live conformance tests (needs KVM; see docs/firecracker.md).
	PATH="$(dir $(FC_BIN)):$$PATH" \
	LLMBOX_FC_KERNEL="$(FC_KERNEL)" LLMBOX_FC_ROOTFS="$(FC_ROOTFS)" \
		go test -v ./internal/spoke/firecracker/ -run 'TestConformanceFirecracker|TestVMSurvivesRequestContextCancel'

.PHONY: test-e2e
test-e2e: $(WEBDIST) ## Run the end-to-end browser tests (needs Chrome + chromedriver; see e2e/ and internal/hub/admin_browser_e2e_test.go).
	go test -tags e2e ./e2e/... ./internal/hub/...

.PHONY: test-e2e-cluster
test-e2e-cluster: ## Run the hub-and-spoke clustering e2e test (no browser needed; see e2e/cluster/).
	go test -tags e2e ./e2e/cluster/...

.PHONY: cover
cover: $(WEBDIST) ## Run tests with coverage and print the total.
	go test -covermode=atomic -coverprofile=$(COVERPROFILE) ./...
	go tool cover -func=$(COVERPROFILE) | tail -1

.PHONY: cover-html
cover-html: cover ## Open the HTML coverage report.
	go tool cover -html=$(COVERPROFILE)

# --- docker ------------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the llmbox Docker image (tagged $(IMAGE):$(VERSION) and :latest).
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: docker-build-mcp
docker-build-mcp: ## Build the llmbox-mcp Docker image (tagged $(IMAGE)-mcp:$(VERSION) and :latest).
	docker build -f Dockerfile.mcp -t $(IMAGE)-mcp:$(VERSION) -t $(IMAGE)-mcp:latest .

.PHONY: docker-build-spoke
docker-build-spoke: ## Build the llmbox-spoke Docker image (tagged $(IMAGE)-spoke:$(VERSION) and :latest).
	docker build -f Dockerfile.spoke -t $(IMAGE)-spoke:$(VERSION) -t $(IMAGE)-spoke:latest .

.PHONY: docker-build-box
docker-build-box: ## Build the default box base image (tagged $(IMAGE)-box:$(VERSION) and :latest).
	docker build -f Dockerfile.box -t $(IMAGE)-box:$(VERSION) -t $(IMAGE)-box:latest .

.PHONY: compose-up
compose-up: ## Build and start via docker compose (needs llmbox.yaml).
	docker compose up --build

# --- release -----------------------------------------------------------------

# GoReleaser cross-compiles every binary and publishes downloadable archives to a
# GitHub Release on a `v*` tag push (see .goreleaser.yaml and the release
# workflow). These targets exercise it locally; both run goreleaser via `go run`
# so no separate install is needed. `release-snapshot` builds into ./dist without
# needing a tag or publishing anything.
GORELEASER := go run github.com/goreleaser/goreleaser/v2@latest

.PHONY: release-check
release-check: ## Validate the GoReleaser config (.goreleaser.yaml).
	$(GORELEASER) check

.PHONY: release-snapshot
release-snapshot: $(WEBDIST) ## Build a local release snapshot into ./dist (no tag, publishes nothing).
	$(GORELEASER) release --snapshot --clean

# --- housekeeping ------------------------------------------------------------

.PHONY: check
check: lint test ## Run lint and the unit tests.

.PHONY: clean
clean: ## Remove build artifacts.
	rm -f $(BINARY) $(MCP_BINARY) $(SPOKE_BINARY) $(GUEST_BINARY) $(COVERPROFILE)
