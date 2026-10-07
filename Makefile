.DEFAULT_GOAL := all

BIN_CLI := lazarus
BIN_SERVER := lazarus-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all
all: build

.PHONY: build
build: build-cli build-server

.PHONY: build-cli
build-cli:
	@echo "==> Building $(BIN_CLI) ($(VERSION))..."
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_CLI) ./cmd/lazarus

.PHONY: build-server
build-server:
	@echo "==> Building $(BIN_SERVER) ($(VERSION))..."
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_SERVER) ./cmd/server

.PHONY: test
test:
	@echo "==> Running unit tests..."
	go test -count=1 ./...

.PHONY: test-race
test-race:
	@echo "==> Running tests with race detector..."
	go test -race ./...

.PHONY: fmt
fmt:
	@echo "==> Formatting code..."
	gofmt -w .

.PHONY: lint
lint:
	@echo "==> Running static analysis..."
	go vet ./...

.PHONY: docker
docker:
	@echo "==> Building Docker images..."
	docker build -t lazarus:latest -f Dockerfile .
	docker build -t lazarus-server:latest -f Dockerfile.server .

.PHONY: clean
clean:
	@echo "==> Cleaning build artifacts..."
	rm -f $(BIN_CLI) $(BIN_SERVER)
