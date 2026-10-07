.DEFAULT_GOAL := help

BIN_CLI := lazarus
BIN_SERVER := lazarus-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help
help: ## Show this help message
	@echo "Lazarus — Database Disaster Recovery & Backup Verification"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Available targets:"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: all
all: build ## Build all binaries (CLI and server)

.PHONY: build
build: build-cli build-server ## Build both lazarus and lazarus-server

.PHONY: build-cli
build-cli: ## Compile the lazarus CLI runner
	@echo "==> Building $(BIN_CLI) ($(VERSION))..."
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_CLI) ./cmd/lazarus

.PHONY: build-server
build-server: ## Compile the lazarus-server Control Plane binary
	@echo "==> Building $(BIN_SERVER) ($(VERSION))..."
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_SERVER) ./cmd/server

.PHONY: test
test: ## Run unit tests across all packages
	@echo "==> Running unit tests..."
	go test -count=1 ./...

.PHONY: test-race
test-race: ## Run unit tests with race detector enabled
	@echo "==> Running tests with race detector..."
	go test -race ./...

.PHONY: fmt
fmt: ## Format Go source code with gofmt
	@echo "==> Formatting code..."
	gofmt -w .

.PHONY: lint
lint: ## Run static analysis with go vet
	@echo "==> Running static analysis..."
	go vet ./...

.PHONY: docker
docker: ## Build Docker images for CLI and server
	@echo "==> Building Docker images..."
	docker build -t lazarus:latest -f Dockerfile .
	docker build -t lazarus-server:latest -f Dockerfile.server .

.PHONY: clean
clean: ## Remove compiled binaries
	@echo "==> Cleaning build artifacts..."
	rm -f $(BIN_CLI) $(BIN_SERVER)
