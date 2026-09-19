GO = go
GOFLAGS = -v

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%d %H:%M:%S UTC')
LDFLAGS = -ldflags "-s -w -X 'github.com/mqtt-home/mqtt-mail/version.Version=$(VERSION)' -X 'github.com/mqtt-home/mqtt-mail/version.GitCommit=$(GIT_COMMIT)' -X 'github.com/mqtt-home/mqtt-mail/version.BuildTime=$(BUILD_TIME)'"

BUILD_DIR = build
CONFIG_DIR = $(PWD)/../production/config
WEB_DIR = web

BINARY_NAME = mqtt-mail
DOCKER_IMAGE_NAME = pharndt/mqtt-mail:latest

# Bare `make` prints the targets rather than doing something surprising.
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "mqtt-mail — make targets"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Config: $(CONFIG_DIR)/config.json"

.PHONY: build
build: build-frontend build-backend ## Build frontend and backend

.PHONY: version
version: ## Print the version stamped into the binary
	@echo "Version: $(VERSION)"
	@echo "Git Commit: $(GIT_COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"

.PHONY: build-frontend
build-frontend: ## Build the web UI
	@echo "Building the frontend..."
	@cd $(WEB_DIR) && pnpm install && pnpm run build

.PHONY: build-backend
build-backend: ## Build the Go binary
	@echo "Building the backend..."
	@$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

.PHONY: dev
dev: build-frontend build-backend ## Build everything and run against the local config
	@echo "Starting the application..."
	@$(BUILD_DIR)/$(BINARY_NAME) $(CONFIG_DIR)/config.json

.PHONY: dev-frontend
dev-frontend: ## Vite dev server on :5173, talking to the backend on :8080
	@echo "Starting frontend dev server..."
	@cd $(WEB_DIR) && pnpm run dev

.PHONY: dev-backend
dev-backend: build-backend ## Build and run only the backend
	@echo "Starting backend dev server..."
	@$(BUILD_DIR)/$(BINARY_NAME) $(CONFIG_DIR)/config.json

.PHONY: run
run: build ## Alias for dev
	@echo "Running the application..."
	@$(BUILD_DIR)/$(BINARY_NAME) $(CONFIG_DIR)/config.json

.PHONY: test
test: ## Run the Go tests
	@$(GO) test ./...

.PHONY: fmt
fmt: ## Format the Go sources
	@gofmt -w .

.PHONY: vet
vet: ## Run go vet
	@$(GO) vet ./...

.PHONY: check
check: fmt vet test ## Format, vet and test — run before a release

.PHONY: docker
docker: ## Build the Docker image locally
	@echo "Building Docker image..."
	@docker build -t $(DOCKER_IMAGE_NAME) .

.PHONY: clean
clean: ## Remove build output and node_modules
	@echo "Cleaning up..."
	@rm -rf $(BUILD_DIR)
	@cd $(WEB_DIR) && rm -rf dist node_modules

.PHONY: deps
deps: ## Download and tidy Go dependencies
	@echo "Downloading Go dependencies..."
	@$(GO) mod download
	@$(GO) mod tidy
