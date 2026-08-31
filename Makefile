# MazeRegistryUI - build and deployment tasks.

MODULE      := github.com/amaze-labs/MazeRegistryUI
BINARY      := mazeregistryui
CMD         := ./cmd/mazeregistryui
BIN_DIR     := bin
IMAGE       := ghcr.io/amaze-labs/mazeregistryui

# Version metadata, with fallbacks for tarball checkouts / shallow clones where
# git metadata is missing. `2>/dev/null || echo ...` keeps make working outside
# a git worktree entirely.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

.DEFAULT_GOAL := help

.PHONY: help build run test test-race cover lint fmt tidy \
        docker-build docker-run compose-up compose-down clean

help: ## Show this help
	@echo "MazeRegistryUI - available targets:"
	@echo ""
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "  VERSION=$(VERSION)  COMMIT=$(COMMIT)"

build: ## Build the binary into bin/ with version metadata
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

run: ## Run the server against deploy/config.yaml
	go run -ldflags '$(LDFLAGS)' $(CMD) -config deploy/config.yaml

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

cover: ## Run tests with coverage, write coverage.out and print the total
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -n 1

lint: ## Run go vet and fail if any file is not gofmt-formatted
	go vet ./...
	@unformatted="$$(gofmt -l . 2>/dev/null)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting (run 'make fmt'):"; \
		echo "$$unformatted" | sed 's/^/  /'; \
		exit 1; \
	fi
	@echo "lint: ok"

fmt: ## Format all Go source
	gofmt -w .

tidy: ## Tidy and verify go.mod / go.sum
	go mod tidy
	go mod verify

docker-build: ## Build the container image for the local platform
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-run: docker-build ## Build and run the image on :8080
	docker run --rm -p 8080:8080 \
		-v $(CURDIR)/deploy/config.yaml:/etc/mazeregistryui/config.yaml:ro \
		--read-only --cap-drop ALL --security-opt no-new-privileges:true \
		--tmpfs /tmp:size=16m \
		$(IMAGE):$(VERSION)

compose-up: ## Start the local demo stack (registry + UI)
	docker compose -f compose.yaml up -d --build

compose-down: ## Stop the demo stack and remove its volumes
	docker compose -f compose.yaml down -v

clean: ## Remove build and coverage artifacts
	rm -rf $(BIN_DIR) dist coverage.out coverage.html
	go clean -cache -testcache 2>/dev/null || true
