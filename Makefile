SHELL := /bin/bash
.DEFAULT_GOAL := help

# --- Build metadata ------------------------------------------------------------------------------

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE  := github.com/danmorcov88/fleetward
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(BUILD_DATE)

BIN         := bin
PLUGIN_BIN  := $(BIN)/plugins

# Plugin source directory to engine type. The binary must be named fleetward-plugin-<engine type>,
# because that is how the plugin manager routes an instance to a plugin.
PLUGINS := postgres:postgresql mysql:mysql mongodb:mongodb redis:redis

GO_PACKAGES := ./...

# --- Help ----------------------------------------------------------------------------------------

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- Development ---------------------------------------------------------------------------------

.PHONY: dev
dev: ## Bring up the full development stack (docker compose)
	docker compose up --build

.PHONY: dev-down
dev-down: ## Stop the development stack
	docker compose down

.PHONY: dev-clean
dev-clean: ## Stop the development stack and delete its volumes
	docker compose down --volumes --remove-orphans

.PHONY: dev-logs
dev-logs: ## Follow control plane logs
	docker compose logs -f fleetward

# --- Build ---------------------------------------------------------------------------------------

.PHONY: build
build: build-server build-cli build-plugins ## Build every binary into ./bin

.PHONY: build-server
build-server: ## Build the control plane
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/fleetward ./cmd/fleetward

.PHONY: build-cli
build-cli: ## Build the CLI
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/fleetward-cli ./cmd/fleetward-cli

.PHONY: build-plugins
build-plugins: ## Build every engine plugin binary
	@mkdir -p $(PLUGIN_BIN)
	@for pair in $(PLUGINS); do \
		dir=$${pair%%:*}; engine=$${pair##*:}; \
		echo "  building fleetward-plugin-$$engine"; \
		go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(PLUGIN_BIN)/fleetward-plugin-$$engine ./cmd/plugins/$$dir || exit 1; \
	done

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) dist coverage.out coverage.html web/dist

# --- Protobuf ------------------------------------------------------------------------------------

.PHONY: proto
proto: ## Regenerate code from api/proto (requires buf)
	buf generate
	buf generate --template internal/storage/tsdb/prompb/buf.gen.yaml internal/storage/tsdb/prompb

.PHONY: proto-lint
proto-lint: ## Lint the protobuf contract
	buf lint
	buf format --diff --exit-code

.PHONY: proto-breaking
proto-breaking: ## Check the contract for breaking changes against main
	buf breaking --against '.git#branch=main'

# --- Test ----------------------------------------------------------------------------------------

.PHONY: test
test: ## Run unit tests
	go test -race -short $(GO_PACKAGES)

.PHONY: test-all
test-all: ## Run all Go tests, including those that build plugin binaries
	go test -race $(GO_PACKAGES)

.PHONY: test-integration
test-integration: ## Run testcontainers-based integration tests (requires Docker)
	go test -race -tags=integration -timeout 20m $(GO_PACKAGES)

.PHONY: conformance
conformance: build-plugins ## Run the plugin conformance suite against every plugin
	go test -race -tags=conformance -timeout 30m ./test/conformance/...

.PHONY: cover
cover: ## Run tests with coverage and open the report
	go test -race -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES)
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage report: coverage.html"

# --- Lint ----------------------------------------------------------------------------------------

.PHONY: lint
lint: lint-go proto-lint lint-web ## Run every linter

.PHONY: lint-go
lint-go: ## Lint Go code
	golangci-lint run

.PHONY: lint-web
lint-web: ## Lint the web app
	cd web && npm run lint

.PHONY: fmt
fmt: ## Format Go and protobuf sources
	gofmt -w $(shell find . -name '*.go' -not -path './api/gen/*' -not -path './internal/storage/tsdb/prompb/*')
	buf format -w

.PHONY: vuln
vuln: ## Scan for known vulnerabilities
	govulncheck $(GO_PACKAGES)

# --- Web -----------------------------------------------------------------------------------------

.PHONY: web-install
web-install: ## Install web dependencies
	cd web && npm ci

.PHONY: web-dev
web-dev: ## Run the web app in development mode
	cd web && npm run dev

.PHONY: web-build
web-build: ## Build the web app for production
	cd web && npm run build

# --- Repository configuration --------------------------------------------------------------------

.PHONY: ruleset-apply
ruleset-apply: ## Apply the versioned branch protection ruleset (needs `gh auth login`)
	.github/scripts/apply-ruleset.sh

.PHONY: ruleset-diff
ruleset-diff: ## Show what the ruleset would send, without applying it
	DRY_RUN=1 .github/scripts/apply-ruleset.sh

# --- Tooling -------------------------------------------------------------------------------------

.PHONY: tools
tools: ## Install the development tools used by lint and vuln
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: ci
ci: lint test build ## Run what CI runs
