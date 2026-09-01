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
TEST_TIMEOUT := 60m

# Cap on test binaries the PostgreSQL lanes run at once. go test defaults -p
# to the host CPU count, and every PostgreSQL-backed test binary opens its own
# connections: an admin handle of three (one pinned for the life of the binary
# to hold its template database's ownership lock, see
# internal/testutil/pg_template.go) plus the store under test. Nothing budgets
# across binaries, so on a wide runner `go test ./...` starts every PostgreSQL
# package together and the sum exceeds a stock server's 100 connections
# ("sorry, too many clients already") and its lock table ("out of shared
# memory"). Four is the GitHub-hosted profile these lanes were tuned on. The
# pgvector lane in .github/workflows/ci.yml carries the same value inline.
PG_TEST_PARALLEL ?= 4
GOLANGCI_LINT_VERSION ?= v2.13.1
GOVULNCHECK_VERSION ?= v1.7.0
GO_INSTALL_BIN := $(shell go env GOBIN)
ifeq ($(strip $(GO_INSTALL_BIN)),)
GO_INSTALL_BIN := $(shell go env GOPATH)/bin
endif
GOLANGCI_LINT_BIN := $(GO_INSTALL_BIN)/golangci-lint
CI_TOOLS_BIN := $(shell git rev-parse --path-format=absolute --git-path ci-tools/bin)
GOVULNCHECK_BIN := $(CI_TOOLS_BIN)/govulncheck

# Build tags for the PostgreSQL test lane (test-pg). Must be the full build set:
# pgvector gates the vector-on-PG code paths (//go:build pgvector), and sqlite_vec
# is required too because several tests are gated on BOTH tags
# (//go:build sqlite_vec && pgvector) — the pgvector<->sqlitevec parity test
# (internal/vector/pgvector/parity_test.go) and the PG command-wiring tests
# (cmd/msgvault/cmd/{serve_vector_pg,embed_pg,search_vector_pg,embed_vector_pg}_test.go).
# Omitting sqlite_vec compiles those out and the target gives false confidence.
PG_TEST_TAGS := fts5 sqlite_vec pgvector

# The only packages that build a different test binary under BUILD_TAGS than
# under PG_TEST_TAGS. That is not just the packages carrying pgvector-gated
# files: a package whose own sources are identical still links different code
# when something in its dependency closure changed, so this is the reverse
# dependency closure of the tag-sensitive packages, not the tag-sensitive
# packages themselves. Every package outside this set compiles byte-identically
# in both configurations, so test-pg-both runs just these in the shipped-build
# configuration. Verified by `make pg-shipped-only-check`, which re-derives the
# closure from `go list`.
PG_SHIPPED_ONLY_PKGS := ./cmd/msgvault ./cmd/msgvault/cmd ./internal/api ./internal/mcp ./internal/scheduler ./internal/store ./internal/vector/chunkmatch ./internal/vector/document ./internal/vector/embed ./internal/vector/hybrid ./internal/vector/pgvector ./scripts/contextual-retrieval-eval

OPENAPI_ARTIFACTS := api/openapi.yaml pkg/client/openapi.yaml pkg/client/generated
WEB_INSTALL_STAMP := web/node_modules/.msgvault-install-stamp

# Keep golangci-lint results scoped to this git worktree. Its cache can contain
# absolute source paths, so sharing the default user cache across worktrees can
# replay diagnostics for deleted worktree paths.
DEFAULT_GOLANGCI_LINT_CACHE := $(shell git rev-parse --path-format=absolute --git-path golangci-lint-cache)
GOLANGCI_LINT_CACHE ?= $(DEFAULT_GOLANGCI_LINT_CACHE)
export GOLANGCI_LINT_CACHE

# golangci-lint's runner lock lives under os.TempDir(), independently of its
# analysis cache. Keep that lock worktree-local too, so linked worktrees do not
# serialize one another while duplicate runners in one worktree can wait.
GOLANGCI_LINT_TMP ?= $(GOLANGCI_LINT_CACHE)/tmp

.PHONY: build build-release install clean test test-v test-pg test-pg-shipped test-pg-both pg-shipped-only-check require-test-db fmt lint-tools lint lint-ci vuln-tools vulncheck testify-helper-check tidy openapi api-generate openapi-check api-check web-install web-generate web-check web-test web-test-browser web-e2e web-build web-embed web-assets-check smoke-web-release shootout run-shootout install-hooks bench vcard-registry-check vcard-registry-update docs-install docs-build docs-serve docs-check docs-fixture-test docs-fixture-check docs-fixture-smoke docs-web-screenshots docs-screenshots docs-assets-branch docs-generated-assets-branch docs-deploy-staging docs-deploy help

# Build the binary (debug)
build: web-embed
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o msgvault ./cmd/msgvault
	@chmod +x msgvault

# Build with optimizations (release)
build-release: web-embed
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS_RELEASE)" -trimpath -o msgvault ./cmd/msgvault
	@chmod +x msgvault

# Install to ~/.local/bin, $GOBIN, or $GOPATH/bin
install: web-embed
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

# Run tests. The CLI package has nearly 1,000 tests, including heavy DuckDB
# coverage, and its per-package wall clock can exceed 40m on contended CI
# runners even when no individual test is stalled.
test:
	go test -timeout $(TEST_TIMEOUT) -tags "$(BUILD_TAGS)" ./...

# Run tests with verbose output
test-v:
	go test -timeout $(TEST_TIMEOUT) -tags "$(BUILD_TAGS)" -v ./...

# Run tests against PostgreSQL with the pgvector tag (set MSGVAULT_TEST_DB
# first). Needs a server with the vector extension available.
# Example: MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/db make test-pg
#
# CI does not run this target as-is: .github/workflows/ci.yml splits the same
# ground into test-pgvector (pgvector image, pgvector-tagged packages) and
# test-postgres (stock image, test-pg-shipped below).
# See docs/internal/PG_STATUS.md for the supported feature surface.
test-pg: require-test-db
	go test -timeout $(TEST_TIMEOUT) -p $(PG_TEST_PARALLEL) -tags "$(PG_TEST_TAGS)" ./...

# Run the SHIPPED build's tests against PostgreSQL (set MSGVAULT_TEST_DB first).
# The released binary is built with BUILD_TAGS and no pgvector, so that build
# has to be exercised against a PostgreSQL archive too. This is the lane
# .github/workflows/ci.yml's test-postgres job runs.
#
# It is a named target rather than an inline `go test` so its flags match the
# `test` target exactly. Go's test cache keys on the flags, so any package that
# never reads MSGVAULT_TEST_DB — the SQLite-only ones — is served from `make
# test`'s cache instead of being re-run here.
test-pg-shipped: require-test-db
	go test -timeout $(TEST_TIMEOUT) -p $(PG_TEST_PARALLEL) -tags "$(BUILD_TAGS)" ./...

# Both PostgreSQL lanes' coverage in one pass.
#
# test-pg and test-pg-shipped differ only in the pgvector build tag, and that
# tag changes the test binary of just the packages in PG_SHIPPED_ONLY_PKGS. For
# every other package the two lanes compile a byte-identical test binary and run
# it against the same server with the same environment, so running both in full
# repeats roughly 1000s of work per round. This target runs test-pg in full and
# then only the packages the tag actually changes.
#
# Use this instead of running the two lanes back to back. Do NOT run
# test-pg-shipped's narrow half on its own and call PostgreSQL covered — the
# equivalence argument depends on the full pgvector lane having run on the same
# tree. pg-shipped-only-check re-derives the package set and fails if it drifts.
test-pg-both: require-test-db pg-shipped-only-check
	go test -timeout $(TEST_TIMEOUT) -p $(PG_TEST_PARALLEL) -tags "$(PG_TEST_TAGS)" ./...
	go test -timeout $(TEST_TIMEOUT) -p $(PG_TEST_PARALLEL) -tags "$(BUILD_TAGS)" $(PG_SHIPPED_ONLY_PKGS)

# Fail if the set of packages whose test binary changes when the pgvector tag is
# dropped no longer matches PG_SHIPPED_ONLY_PKGS. This is the assumption
# test-pg-both rests on, so it is checked rather than trusted: a new
# pgvector-gated file, or a new import of a package that has one, would
# otherwise silently stop being covered in the shipped-build configuration.
#
# Two steps, because a package's own source list is not enough. The first finds
# the packages the tag changes directly. The second walks the test dependency
# graph and adds every package that links one of them, since its test binary
# differs even though its own files do not.
pg-shipped-only-check:
	@set -e; \
	module="$$(go list -m)"; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	fmt='{{.ImportPath}}|{{.GoFiles}}|{{.TestGoFiles}}|{{.XTestGoFiles}}|{{.CgoFiles}}'; \
	go list -deps -test -tags "$(BUILD_TAGS)" -f "$$fmt" ./... | sort > "$$tmp/shipped"; \
	go list -deps -test -tags "$(PG_TEST_TAGS)" -f "$$fmt" ./... | sort > "$$tmp/pgvector"; \
	diff "$$tmp/shipped" "$$tmp/pgvector" \
		| sed -n 's/^[<>] \([^|]*\)|.*/\1/p' \
		| sed 's/ \[.*//; s/\.test$$//' \
		| sort -u > "$$tmp/sensitive"; \
	go list -test -tags "$(BUILD_TAGS)" -f '{{.ImportPath}}|{{join .Deps " "}}' ./... > "$$tmp/deps"; \
	awk -v mod="$$module" -F'|' 'NR==FNR { sensitive[$$0]=1; next } { \
		name = $$1; \
		sub(/ \[.*/, "", name); sub(/\.test$$/, "", name); sub(/_test$$/, "", name); \
		if (index(name, mod) != 1) next; \
		hit = (name in sensitive); \
		if (!hit) { n = split($$2, d, " "); for (i = 1; i <= n && !hit; i++) if (d[i] in sensitive) hit = 1 } \
		if (hit) print name \
	}' "$$tmp/sensitive" "$$tmp/deps" \
		| sed "s|^$$module/|./|; s|^$$module$$|.|" \
		| sort -u > "$$tmp/actual"; \
	printf '%s\n' $(PG_SHIPPED_ONLY_PKGS) | sort -u > "$$tmp/expected"; \
	if ! diff -u "$$tmp/expected" "$$tmp/actual"; then \
		echo "PG_SHIPPED_ONLY_PKGS is stale ('-' expected, '+' actual). Update it in the Makefile; test-pg-both's coverage argument depends on it." >&2; \
		exit 1; \
	fi

require-test-db:
	@if [ -z "$$MSGVAULT_TEST_DB" ]; then \
		echo "MSGVAULT_TEST_DB must be set, e.g., postgres://user:pass@localhost:5432/db" >&2; \
		exit 1; \
	fi

# Network-check or update the vendored IANA vCard Elements registry. These are
# manual targets; CI validates handling coverage against the vendored snapshot.
vcard-registry-check:
	go run ./internal/vcard/cmd/update-registry

vcard-registry-update:
	go run ./internal/vcard/cmd/update-registry --write

# Regenerate the committed OpenAPI schemas and generated Go client.
# api/openapi.yaml is the published OpenAPI 3.1 schema; pkg/client/openapi.yaml
# is the OpenAPI 3.0 schema used by the Go client generator.
api-generate:
	@mkdir -p api pkg/client/generated
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/msgvault openapi > "$$tmp"; if [ -f api/openapi.yaml ] && cmp -s "$$tmp" api/openapi.yaml; then rm "$$tmp"; else mv "$$tmp" api/openapi.yaml; fi; trap - EXIT
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/msgvault openapi --version 3.0 --format yaml > "$$tmp"; if [ -f pkg/client/openapi.yaml ] && cmp -s "$$tmp" pkg/client/openapi.yaml; then rm "$$tmp"; else mv "$$tmp" pkg/client/openapi.yaml; fi; trap - EXIT
	cd pkg/client/generated && find . -maxdepth 1 -type f -name '*.go' ! -name 'generate.go' -delete && go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5 -config config.yaml ../openapi.yaml
	go run ./internal/codegenfix/cmd pkg/client/generated/types.go

openapi-check: api-generate
	@git diff --exit-code -- $(OPENAPI_ARTIFACTS) || (echo "OpenAPI generated assets are stale; run 'make api-generate' and commit the changes." >&2; exit 1)
	@if [ -n "$$(git status --porcelain --untracked-files=all -- $(OPENAPI_ARTIFACTS))" ]; then \
		git status --short --untracked-files=all -- $(OPENAPI_ARTIFACTS); \
		echo "OpenAPI generated assets are stale; run 'make api-generate' and commit the changes." >&2; \
		exit 1; \
	fi

api-check: openapi-check

openapi: api-generate

# Install, generate, validate, test, and build the browser application. Web
# generation is intentionally separate from the Go-only OpenAPI targets so
# API client checks remain runnable on systems without Bun.
web-install: $(WEB_INSTALL_STAMP)

$(WEB_INSTALL_STAMP): web/package.json web/bun.lock
	cd web && bun install --frozen-lockfile
	@touch $(WEB_INSTALL_STAMP)

web-generate: web-install
	cd web && bun run generate

web-check: web-generate
	@git diff --exit-code -- web/src/lib/api/generated/schema.d.ts || (echo "Web API generated types are stale; run 'make web-generate' and commit the changes." >&2; exit 1)
	@if [ -n "$$(git status --porcelain --untracked-files=all -- web/src/lib/api/generated/schema.d.ts)" ]; then \
		git status --short --untracked-files=all -- web/src/lib/api/generated/schema.d.ts; \
		echo "Web API generated types are stale; run 'make web-generate' and commit the changes." >&2; \
		exit 1; \
	fi
	cd web && bun run check
	cd web && bun run check:kit-ui

web-test:
	cd web && bun run test

web-test-browser:
	cd web && bun run test:browser

# Task 20 browser gates use the same digest-pinned Playwright environment as
# web-test-browser in CI. Traces, screenshots, and video are retained only for
# failures by web/playwright.config.ts.
web-e2e:
	cd web && bun run test:e2e

web-build: web-generate
	cd web && bun run build

# Replace prior generated output while preserving the compilation stub, copy
# Vite's complete production distribution into the Go embed tree, then validate
# the staged embed. Validation is a mandatory part of embedding: the tree is
# served without authentication, so every build that embeds it must reject
# hidden files, credential-pattern names, and untracked assets.
web-embed: web-build
	@mkdir -p internal/web/dist
	@find internal/web/dist -mindepth 1 -maxdepth 1 ! -name stub.html -exec rm -rf {} +
	@cp -R web/dist/. internal/web/dist/
	node scripts/check-web-assets.mjs

# Validate Vite's parsed release graph against the staged embed (runs as the
# final step of web-embed). The node test drives the same validator through
# missing, escaping, external, hidden/credential, and stale cases.
web-assets-check: web-embed
	node --test scripts/check-web-assets.test.mjs

smoke-web-release:
	node --test scripts/smoke-web-release.test.mjs
	bash scripts/smoke-web-release.sh

# Format code
fmt:
	go fmt ./...

# Install the pinned linter used by CI.
lint-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Run linter (auto-fix)
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/" >&2; \
		exit 1; \
	fi
	@mkdir -p "$(GOLANGCI_LINT_TMP)"
	TMPDIR="$(GOLANGCI_LINT_TMP)" golangci-lint run --fix ./...

# Run linter (CI, no auto-fix)
lint-ci: lint-tools testify-helper-check
	@mkdir -p "$(GOLANGCI_LINT_TMP)"
	TMPDIR="$(GOLANGCI_LINT_TMP)" "$(GOLANGCI_LINT_BIN)" run ./...
	@if [ -n "$$GITHUB_PATH" ]; then \
		$(MAKE) --no-print-directory vuln-tools; \
		printf '%s\n' "$(CI_TOOLS_BIN)" >> "$$GITHUB_PATH"; \
	fi

# Install and run the scanner from a repository-owned path so a stale tool
# installed by the base branch's pull-request workflow cannot replace it.
vuln-tools:
	@mkdir -p "$(CI_TOOLS_BIN)"
	GOBIN="$(CI_TOOLS_BIN)" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

vulncheck: vuln-tools
	"$(GOVULNCHECK_BIN)" -tags "$(BUILD_TAGS)" ./...

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

# Run the deterministic fixture selector and review-report unit tests.
docs-fixture-test:
	python3 -m unittest docs/fixtures/test_select_enron_fixture.py

# Validate the pinned, manually reviewed docs-fixtures branch in a disposable
# directory. The explicit offline mode is a local opt-in and prints SKIP.
docs-fixture-check:
	@fixture_tmp="$$(mktemp -d /tmp/msgvault-docs-fixture-check.XXXXXX)"; \
	trap 'rm -rf "$$fixture_tmp"' EXIT; \
	bash docs/fixtures/hydrate-fixture.sh --output-dir "$$fixture_tmp"

# Exercise the real importer, cache, daemon, and relationship API against the
# hydrated fixture. This is deliberately outside make test.
docs-fixture-smoke:
	@fixture_tmp="$$(mktemp -d /tmp/msgvault-docs-fixture-smoke.XXXXXX)"; \
	trap 'rm -rf "$$fixture_tmp"' EXIT; \
	bash docs/fixtures/hydrate-fixture.sh --output-dir "$$fixture_tmp/fixture"; \
	bash docs/fixtures/run-smoke.sh "$$fixture_tmp/fixture"

# Generate docs screenshots from the isolated real-daemon fixture pipeline.
docs-web-screenshots:
	bash docs/screenshots/generate-web-fixture-screenshots.sh

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
	@echo "  vulncheck      - Run the pinned Go vulnerability scanner"
	@echo "  testify-helper-check - Enforce testify helper usage in assertion-heavy tests"
	@echo "  tidy           - Tidy go.mod"
	@echo "  vcard-registry-check - Network-check IANA registry drift (manual; not CI)"
	@echo "  vcard-registry-update - Update the vendored IANA vCard registry"
	@echo "  openapi        - Regenerate OpenAPI specs and generated Go client"
	@echo "  openapi-check  - Check committed OpenAPI specs and generated Go client are up to date"
	@echo "  api-check      - Alias for openapi-check"
	@echo "  web-install    - Install pinned browser application dependencies"
	@echo "  web-generate   - Regenerate browser API types from the OpenAPI schema"
	@echo "  web-check      - Check browser types and generated API artifacts"
	@echo "  web-test       - Run browser application unit tests"
	@echo "  web-test-browser - Run browser application Playwright tests"
	@echo "  web-build      - Build the browser application"
	@echo "  web-embed      - Build, stage, and validate browser assets for Go embedding"
	@echo "  web-assets-check - Validate the release asset graph and run the validator's tests"
	@echo "  smoke-web-release - Build and exercise an isolated release-style daemon"
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
