.DEFAULT_GOAL := help

VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build ./sextant for the host platform
	go build -trimpath -ldflags '$(LDFLAGS)' -o sextant ./cmd/sextant

.PHONY: install
install: ## Install sextant into $$GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/sextant

.PHONY: test
test: ## Run tests with race detection
	go test -race ./...

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -race -coverprofile=coverage.txt ./...
	go tool cover -html=coverage.txt

.PHONY: lint
lint: ## Run golangci-lint (requires golangci-lint on PATH)
	golangci-lint run

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: check
check: fmt vet test ## Format, vet, and test — run before pushing

.PHONY: snapshot
snapshot: ## Build release artifacts locally without publishing
	goreleaser build --snapshot --clean

.PHONY: clean
clean: ## Remove build output
	rm -rf dist sextant coverage.txt
