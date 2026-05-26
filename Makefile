# Makefile for msgvault

.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=$(VERSION) \
           -X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=$(COMMIT) \
           -X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=$(BUILD_DATE)

LDFLAGS_RELEASE := $(LDFLAGS) -s -w

# Default build tags applied to every go build/test/bench invocation.
# - fts5: enable the SQLite FTS5 full-text search extension
# - sqlite_vec: enable the sqlite-vec extension for vector search
BUILD_TAGS := fts5 sqlite_vec

.PHONY: build build-release install clean test test-v test-pg fmt lint lint-ci testify-helper-check tidy shootout run-shootout install-hooks bench help

# Build the binary (debug)
build:
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o msgvault ./cmd/msgvault
	@chmod +x msgvault

# Build with optimizations (release)
build-release:
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS_RELEASE)" -trimpath -o msgvault ./cmd/msgvault
	@chmod +x msgvault

# Install to ~/.local/bin, $GOBIN, or $GOPATH/bin
install:
	@if [ -d "$(HOME)/.local/bin" ]; then \
		echo "Installing to ~/.local/bin/msgvault"; \
		CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o "$(HOME)/.local/bin/msgvault" ./cmd/msgvault; \
	else \
		INSTALL_DIR="$${GOBIN:-$$(go env GOBIN)}"; \
		if [ -z "$$INSTALL_DIR" ]; then \
			GOPATH_FIRST="$$(go env GOPATH | cut -d: -f1)"; \
			INSTALL_DIR="$$GOPATH_FIRST/bin"; \
		fi; \
		mkdir -p "$$INSTALL_DIR"; \
		echo "Installing to $$INSTALL_DIR/msgvault"; \
		CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o "$$INSTALL_DIR/msgvault" ./cmd/msgvault; \
	fi

# Clean build artifacts
clean:
	rm -f msgvault msgvault.exe mimeshootout
	rm -rf bin/

# Run tests
test:
	go test -tags "$(BUILD_TAGS)" ./...

# Run tests with verbose output
test-v:
	go test -tags "$(BUILD_TAGS)" -v ./...

# Run tests against PostgreSQL (set MSGVAULT_TEST_DB first).
# Example: MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/db make test-pg
#
# CI runs the same target under .github/workflows/ci.yml's test-postgres job.
# See docs/PG_STATUS.md for the supported feature surface.
test-pg:
	@if [ -z "$$MSGVAULT_TEST_DB" ]; then \
		echo "MSGVAULT_TEST_DB must be set, e.g., postgres://user:pass@localhost:5432/db" >&2; \
		exit 1; \
	fi
	go test -tags fts5 ./...

# Format code
fmt:
	go fmt ./...

# Run linter (auto-fix)
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/" >&2; \
		exit 1; \
	fi
	golangci-lint run --fix ./...

# Run linter (CI, no auto-fix)
lint-ci: testify-helper-check
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/" >&2; \
		exit 1; \
	fi
	golangci-lint run ./...

# Enforce testify helper usage in assertion-heavy tests
testify-helper-check:
	go run ./cmd/testify-helper-check -tags="$(BUILD_TAGS)" ./...

# Install pre-commit hook via prek
install-hooks:
	@if ! command -v prek >/dev/null 2>&1; then \
		echo "prek not found. Install with: brew install prek" >&2; \
		exit 1; \
	fi
	@HOOKS_PATH=$$(git config --get core.hooksPath 2>/dev/null); \
	if [ "$$HOOKS_PATH" = ".githooks" ]; then \
		git config --unset core.hooksPath; \
	elif [ -n "$$HOOKS_PATH" ]; then \
		echo "core.hooksPath is set to '$$HOOKS_PATH' — unset it first if intended" >&2; \
		exit 1; \
	fi
	prek install

# Tidy dependencies
tidy:
	go mod tidy

# Run benchmarks (query engine smoke test)
bench:
	go test -tags "$(BUILD_TAGS)" -run=^$$ -bench=. -benchtime=1s -count=1 ./internal/query/

# Build the MIME shootout tool
shootout:
	CGO_ENABLED=1 go build -o mimeshootout ./scripts/mimeshootout

# Run MIME shootout
run-shootout: shootout
	./mimeshootout -limit 1000

# Show help
help:
	@echo "msgvault build targets:"
	@echo ""
	@echo "  build          - Debug build"
	@echo "  build-release  - Release build (optimized, stripped)"
	@echo "  install        - Install to ~/.local/bin or GOPATH"
	@echo ""
	@echo "  test           - Run tests"
	@echo "  test-v         - Run tests (verbose)"
	@echo "  fmt            - Format code"
	@echo "  lint           - Run linter (auto-fix)"
	@echo "  lint-ci        - Run linter (CI, no auto-fix; also runs testify-helper-check)"
	@echo "  testify-helper-check - Enforce testify helper usage in assertion-heavy tests"
	@echo "  tidy           - Tidy go.mod"
	@echo "  install-hooks  - Install pre-commit hook via prek"
	@echo "  clean          - Remove build artifacts"
	@echo ""
	@echo "  bench          - Run query engine benchmarks"
	@echo "  shootout       - Build MIME shootout tool"
	@echo "  run-shootout   - Run MIME shootout"
