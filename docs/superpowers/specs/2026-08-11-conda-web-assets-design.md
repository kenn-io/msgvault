# Conda-Forge Web Asset Packaging Design

## Problem

The Conda-Forge `msgvault` package builds from GitHub's generated tag archive. That archive contains the browser compilation stub at `internal/web/dist/stub.html`, but not the compiled Vite application. The resulting binary serves the API but does not serve the web interface. This is the same failure mode reported for Homebrew in [issue #601](https://github.com/kenn-io/msgvault/issues/601).

## Scope

Fix the existing v0.19.3 Conda-Forge package and future feedstock builds without changing msgvault application behavior or its release artifact format.

The contribution must use a personal fork. The working clone's `origin` must be the fork, `conda-forge/msgvault-feedstock` must be a read-only `upstream` remote, and the pull request must originate from a topic branch pushed only to the fork.

## Design

Update `conda-forge/msgvault-feedstock` to compile and embed the browser application before building the Go binary:

1. Increment the recipe build number so Conda-Forge republishes v0.19.3.
2. Add Bun and Make as build dependencies. Bun is published for all four platforms in the feedstock matrix: `linux-64`, `linux-aarch64`, `osx-64`, and `osx-arm64`.
3. Run upstream's canonical `make web-install web-embed` targets from the source root. This uses `bun install --frozen-lockfile`, generates the web API client, builds the Vite application, stages it under `internal/web/dist`, and validates the staged asset graph.
4. Build the Go binary with the feedstock's existing tags and link settings.
5. Run upstream's binary-level asset validator against the installed executable. This catches the original regression by requiring release asset bytes such as `.vite/manifest.json` to be embedded in the binary.
6. Keep the existing source archive and platform matrix unchanged.

The feedstock can download locked frontend dependencies during its build. This follows an existing Conda-Forge recipe pattern for applications that compile a frontend before building an embedded backend executable.

## Validation

Configuration changes are validated through real packaging paths, not tests that inspect recipe text:

- Demonstrate that the current v0.19.3 Conda binary fails `scripts/check-web-assets.mjs --binary` because it lacks `.vite/manifest.json`.
- Render and lint the modified recipe with Conda-Forge tooling.
- Build at least the native platform package locally when practical.
- Run the asset validator against the built or installed package binary and require success.
- Let the feedstock CI matrix validate the remaining supported platforms.

## Publication

Commit the recipe change on the fork topic branch, push only to the personal fork, and open a pull request into `conda-forge/msgvault-feedstock:main`. The pull request must explain that the current package silently embeds only the stub and therefore cannot serve the browser application.

After the pull request exists, comment on `kenn-io/msgvault#601` with its link and state that Conda-Forge had the same source-archive failure mode.
