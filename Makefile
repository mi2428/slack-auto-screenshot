SHELL := /bin/bash
.DEFAULT_GOAL := help
BIN := bin/slack-auto-screenshot

.PHONY: help build check fmt run

##@ Development

build: ## Build the Go CLI into bin/
	@mkdir -p bin
	go build -o "$(BIN)" ./cmd/slack-auto-screenshot

check: ## Check formatting, vet, staticcheck, and tests
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go tool staticcheck ./...
	go test ./...

fmt: ## Format Go source files
	go fmt ./...

run: ## Run the CLI (ARGS="...")
	go run ./cmd/slack-auto-screenshot $(ARGS)

##@ Help

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*##"; section = ""} \
	/^[a-zA-Z0-9_.-]+:.*##/ { \
		if (section != "") printf "\n\033[1m%s\033[0m\n", section; \
		section = ""; \
		printf "  \033[36m%-11s\033[0m %s\n", $$1, $$2; next \
	} \
	/^##@/ { section = substr($$0, 5); next }' $(MAKEFILE_LIST)
