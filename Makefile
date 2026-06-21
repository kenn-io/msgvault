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

# Build tags for the PostgreSQL test lane (test-pg). Must be the full build set:
# pgvector gates the vector-on-PG code paths (//go:build pgvector), and sqlite_vec
# is required too because several tests are gated on BOTH tags
# (//go:build sqlite_vec && pgvector) — the pgvector<->sqlitevec parity test
# (internal/vector/pgvector/parity_test.go) and the PG command-wiring tests
# (cmd/msgvault/cmd/{serve_vector_pg,embed_pg,search_vector_pg,embed_vector_pg}_test.go).
# Omitting sqlite_vec compiles those out and the target gives false confidence.
PG_TEST_TAGS := fts5 sqlite_vec pgvector

# Keep golangci-lint results scoped to this git worktree. Its cache can contain
# absolute source paths, so sharing the default user cache across worktrees can
# replay diagnostics for deleted worktree paths.
DEFAULT_GOLANGCI_LINT_CACHE := $(shell git rev-parse --path-format=absolute --git-path golangci-lint-cache)
GOLANGCI_LINT_CACHE ?= $(DEFAULT_GOLANGCI_LINT_CACHE)
export GOLANGCI_LINT_CACHE

.PHONY: build build-release install clean test test-v test-pg fmt lint lint-ci testify-helper-check tidy shootout run-shootout install-hooks bench docs-install docs-build docs-serve docs-check docs-screenshots docs-assets-branch docs-generated-assets-branch docs-deploy-staging docs-deploy help

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
# See docs/internal/PG_STATUS.md for the supported feature surface.
test-pg:
	@if [ -z "$$MSGVAULT_TEST_DB" ]; then \
		echo "MSGVAULT_TEST_DB must be set, e.g., postgres://user:pass@localhost:5432/db" >&2; \
		exit 1; \
	fi
	go test -tags "$(PG_TEST_TAGS)" ./...

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

# Install docs dependencies
docs-install:
	cd docs && uv sync --frozen

# Build docs site
docs-build:
	cd docs && bash ./vercel-build.sh

# Serve docs site locally
docs-serve:
	bash docs/assets/hydrate-assets.sh
	cd docs && uv run bash ./zensical-docs.sh serve

# Check docs sources and build output
docs-check:
	bash scripts/check-docs.sh

# Regenerate docs screenshots
docs-screenshots:
	bash docs/screenshots/generate-all.sh

# Publish curated static docs assets to local asset branch
docs-assets-branch:
	bash docs/assets/update-static-assets-branch.sh

# Publish generated docs assets to local asset branch
docs-generated-assets-branch:
	bash docs/screenshots/update-generated-assets-branch.sh

# Deploy docs to Vercel staging
docs-deploy-staging:
	cd docs && vercel

# Deploy docs to Vercel production
docs-deploy:
	cd docs && vercel --prod

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
	@echo "  docs-install   - Install docs dependencies"
	@echo "  docs-build     - Build docs site"
	@echo "  docs-serve     - Hydrate and serve docs locally"
	@echo "  docs-check     - Run docs validation"
	@echo "  docs-screenshots - Regenerate docs screenshots"
	@echo "  docs-assets-branch - Publish static docs assets branch"
	@echo "  docs-generated-assets-branch - Publish generated docs assets branch"
	@echo "  docs-deploy-staging - Deploy docs to Vercel staging"
	@echo "  docs-deploy    - Deploy docs to Vercel production"
	@echo ""
	@echo "  bench          - Run query engine benchmarks"
	@echo "  shootout       - Build MIME shootout tool"
	@echo "  run-shootout   - Run MIME shootout"
