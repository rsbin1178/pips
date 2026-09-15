GO=go

# Build stamp. The release version comes from the nearest tag; override on the
# command line (make release VERSION=v0.1.2) when the tag is not yet created.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Release archives: Go targets archived as pips_<version>_<os>_<arch>.tar.gz.
RELEASE_TARGETS ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all build version release test test-short cover fuzz lint lint-fix fmt vet tidy audit \
	deps-check mod-verify p0-verify provider-smoke sandbox-smoke help

all: fmt vet lint test build

## build: Compile all packages with the build stamp embedded
build:
	$(GO) build -ldflags "$(LDFLAGS)" ./...

## version: Print the build stamp embedded by build and release
version:
	@echo "version=$(VERSION) commit=$(COMMIT) date=$(DATE)"

## release: Cross-compile release archives and SHA256SUMS into dist/
release:
	@rm -rf dist
	@mkdir -p dist
	@for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; \
		arch=$${target#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		name="pips_$(VERSION)_$${os}_$${arch}"; \
		echo "building dist/$$name.tar.gz"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath \
			-ldflags "-s -w $(LDFLAGS)" -o "dist/$$name/pips$$ext" ./cmd/pips || exit 1; \
		tar -czf "dist/$$name.tar.gz" -C dist "$$name" || exit 1; \
		rm -rf "dist/$$name"; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum *.tar.gz > SHA256SUMS; \
	else \
		shasum -a 256 *.tar.gz > SHA256SUMS; \
	fi
	@ls -1 dist

## test: Run all tests with race detector and coverage
test:
	$(GO) test -race -coverprofile=coverage.out ./...

## test-short: Run tests without long-running ones
test-short:
	$(GO) test -short ./...

## cover: Open HTML coverage report
cover: test
	$(GO) tool cover -html=coverage.out -o coverage.html

## fuzz: Run SSE parser fuzzing for 30s
fuzz:
	$(GO) test ./ai/internal/sse -fuzz=FuzzParse -fuzztime=30s

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## lint-fix: Run golangci-lint with auto-fix
lint-fix:
	golangci-lint run --fix ./...

## fmt: Format code
fmt:
	$(GO) fmt ./...

## vet: Run go vet
vet:
	$(GO) vet ./...

## tidy: Tidy go.mod
tidy:
	$(GO) mod tidy

## audit: Check dependencies for known vulnerabilities
audit:
	$(GO) tool govulncheck ./...

## mod-verify: Verify downloaded module content against go.sum
mod-verify:
	$(GO) mod verify

## provider-smoke: Test all provider wire adapters and the scripted Coding/TUI flow; real smoke is explicit opt-in
provider-smoke:
	$(GO) test ./ai/openai/... ./ai/anthropic ./ai/gemini ./internal/coding/model ./internal/coding/generation
	$(GO) test -race ./internal/coding/tui -run '^TestScriptedRuntimeMatchesTUIStateAndReplay$$' -count=1
	@if [ "$${PIPS_PROVIDER_SMOKE:-}" = "1" ]; then \
		$(GO) test ./internal/coding/tui -run '^TestProviderSmoke$$' -count=1; \
	else \
		echo "real provider smoke SKIP (set PIPS_PROVIDER_SMOKE=1, PIPS_MODEL, and API_KEY to opt in)"; \
	fi

## sandbox-smoke: Run the current platform's real native sandbox attack and Coding flow matrix
sandbox-smoke:
	PIPS_SANDBOX_INTEGRATION=1 $(GO) test -race \
		./internal/coding/execution/... ./internal/coding/changes/git ./internal/coding/tools \
		-run 'Darwin.*Integration|Linux.*Integration|GitIntegration|ShellIntegration|CodingFlowIntegration' \
		-count=3

## p0-verify: Run the reproducible Coding Agent P0 quality and security gates
p0-verify:
	$(GO) test ./...
	$(GO) test -race ./agent/... ./internal/coding/...
	$(MAKE) provider-smoke
	$(GO) vet ./...
	golangci-lint run ./...
	$(GO) build ./...
	$(MAKE) mod-verify
	$(MAKE) audit
	$(MAKE) deps-check

## deps-check: Verify core ai/agent/workflow dependencies; compile optional integrations
deps-check:
	@core_pkgs=$$($(GO) list ./ai/... ./agent/... | grep -Ev '/agent/(mcp|observability/otel)$$'); \
	mods=$$($(GO) list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' $$core_pkgs | sort -u | grep -v '^github.com/rsbin1178/pips$$' | grep -v '^golang.org/x/' | grep -v '^gopkg.in/yaml.v3$$' || true); \
	if [ -n "$$mods" ]; then \
		echo "unexpected third-party module dependencies in core ai/agent:"; echo "$$mods"; exit 1; \
	else \
		echo "core dependency policy OK (stdlib + golang.org/x + reviewed YAML parser)"; \
	fi
	@forbidden=$$($(GO) list -deps -f '{{.ImportPath}}' ./workflow/... | grep -E '^github.com/rsbin1178/pips/(ai|agent)(/|$$)' || true); \
	if [ -n "$$forbidden" ]; then \
		echo "workflow must not depend on ai or agent packages:"; echo "$$forbidden"; exit 1; \
	fi; \
	mods=$$($(GO) list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./workflow/... | sort -u | grep -v '^github.com/rsbin1178/pips$$' | grep -v '^github.com/google/jsonschema-go$$' || true); \
	if [ -n "$$mods" ]; then \
		echo "unexpected third-party module dependencies in workflow:"; echo "$$mods"; exit 1; \
	else \
		echo "workflow dependency policy OK (stdlib + reviewed jsonschema-go)"; \
	fi
	@$(GO) list -deps ./agent/mcp >/dev/null
	@echo "optional agent/mcp integration dependency graph OK"
	@$(GO) list -deps ./agent/observability/otel >/dev/null
	@echo "optional agent/observability/otel integration dependency graph OK"

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
