# Makefile for llmbox. Run `make help` for the list of targets.

# --- configuration -----------------------------------------------------------

BINARY      := llmbox
PKG         := ./cmd/llmbox
IMAGE       := llmbox
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COVERPROFILE := coverage.out

# Build a static binary, matching the Dockerfile.
GO_BUILD_ENV := CGO_ENABLED=0
GO_BUILD_FLAGS := -trimpath -ldflags="-s -w"

# --- meta --------------------------------------------------------------------

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# --- build -------------------------------------------------------------------

.PHONY: build
build: ## Build the llmbox binary into ./$(BINARY).
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(BINARY) $(PKG)

.PHONY: install
install: ## Install the llmbox binary into $GOPATH/bin.
	go install $(GO_BUILD_FLAGS) $(PKG)

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
test: ## Run the unit test suite.
	go test ./...

.PHONY: test-integration
test-integration: ## Run integration tests (needs Docker + a Claude binary; see integration_test.go).
	go test -tags integration ./...

.PHONY: cover
cover: ## Run tests with coverage and print the total.
	go test -covermode=atomic -coverprofile=$(COVERPROFILE) ./...
	go tool cover -func=$(COVERPROFILE) | tail -1

.PHONY: cover-html
cover-html: cover ## Open the HTML coverage report.
	go tool cover -html=$(COVERPROFILE)

# --- docker ------------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the llmbox Docker image (tagged $(IMAGE):$(VERSION) and :latest).
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: compose-up
compose-up: ## Build and start via docker compose (needs llmbox.yaml).
	docker compose up --build

# --- housekeeping ------------------------------------------------------------

.PHONY: check
check: lint test ## Run lint and the unit tests.

.PHONY: clean
clean: ## Remove build artifacts.
	rm -f $(BINARY) $(COVERPROFILE)
