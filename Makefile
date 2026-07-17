GO=go

.PHONY: all build test test-short cover fuzz lint lint-fix fmt vet tidy audit deps-check help

all: fmt vet lint test build

## build: Compile all packages
build:
	$(GO) build ./...

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
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## deps-check: Verify the ai package depends only on stdlib and golang.org/x
deps-check:
	@deps=$$($(GO) list -deps ./ai/... | grep -v '^github.com/rsbin/pips' | grep '\.' | grep -v '^golang\.org/x/' || true); \
	if [ -n "$$deps" ]; then \
		echo "unexpected third-party dependencies:"; echo "$$deps"; exit 1; \
	else \
		echo "dependency policy OK (stdlib + golang.org/x only)"; \
	fi

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
