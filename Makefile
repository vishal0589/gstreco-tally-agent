SHELL := /bin/bash

# Version info injected via -ldflags. VERSION defaults to the latest git tag
# or "dev" for untagged local builds.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE    := github.com/vishal0589/gstreco-tally-agent
LDFLAGS   := -s -w \
             -X $(MODULE)/internal/version.Version=$(VERSION) \
             -X $(MODULE)/internal/version.Commit=$(COMMIT) \
             -X $(MODULE)/internal/version.Date=$(DATE)
GOFLAGS   := -trimpath -ldflags "$(LDFLAGS)"
BUILDENV  := CGO_ENABLED=0

BIN  := bin
DIST := dist

CMDS := agent tray agentctl installer

# Per-platform cross-compile uses recursive make so each invocation gets its
# own GOOS/GOARCH/EXT/PLATFORM variables and the `_cross` target is rebuilt
# every time (otherwise make memoizes and only the first platform runs).
.PHONY: all build windows linux darwin release clean test vet tidy help _cross

all: build

build: ## Build all four binaries for the host platform into ./bin
	@mkdir -p $(BIN)
	@for cmd in $(CMDS); do \
	  echo "→ $$cmd (host)"; \
	  $(BUILDENV) go build $(GOFLAGS) -o $(BIN)/gstreco-tally-$$cmd ./cmd/$$cmd || exit 1; \
	done
	@echo "✓ built $(CMDS) → $(BIN)/"

windows: ## Cross-compile for windows/amd64 into ./dist/windows-amd64
	@$(MAKE) --no-print-directory _cross PLATFORM=windows-amd64 GOOS=windows GOARCH=amd64 EXT=.exe

linux: ## Cross-compile for linux/amd64 into ./dist/linux-amd64
	@$(MAKE) --no-print-directory _cross PLATFORM=linux-amd64 GOOS=linux GOARCH=amd64 EXT=

darwin: ## Cross-compile for darwin/arm64 into ./dist/darwin-arm64
	@$(MAKE) --no-print-directory _cross PLATFORM=darwin-arm64 GOOS=darwin GOARCH=arm64 EXT=

_cross:
	@mkdir -p $(DIST)/$(PLATFORM)
	@for cmd in $(CMDS); do \
	  echo "→ $$cmd ($(PLATFORM))"; \
	  $(BUILDENV) GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) \
	    -o $(DIST)/$(PLATFORM)/gstreco-tally-$$cmd$(EXT) ./cmd/$$cmd || exit 1; \
	done
	@echo "✓ built $(CMDS) → $(DIST)/$(PLATFORM)/"

release: clean windows linux darwin ## Cross-compile for all supported platforms
	@echo "✓ release artifacts ready in $(DIST)/"

test: ## go test ./...
	go test -race -count=1 ./...

vet: ## go vet ./...
	go vet ./...

tidy: ## go mod tidy
	go mod tidy

clean: ## remove build output
	rm -rf $(BIN) $(DIST)

help: ## print this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
