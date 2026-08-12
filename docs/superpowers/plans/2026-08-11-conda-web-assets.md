# Conda-Forge Web Assets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the v0.19.3 Conda-Forge package with msgvault's compiled web application embedded, publish the change from a personal fork, and link the feedstock pull request from issue #601.

**Architecture:** Keep the existing Conda source archive and Go build, but add the canonical upstream frontend build before Go compilation. Use the upstream binary asset validator as the regression contract. All Git writes go to a personal fork; the Conda-Forge repository is a read-only upstream and pull-request target.

**Tech Stack:** Rattler-build recipe YAML, Bun 1.3, Vite/Svelte, Go, GitHub CLI, Conda-Forge CI.

## Global Constraints

- Work from a personal fork of `conda-forge/msgvault-feedstock`; never push directly to the Conda-Forge repository.
- Configure the personal fork as `origin` and `conda-forge/msgvault-feedstock` as read-only `upstream`.
- Preserve `linux-64`, `linux-aarch64`, `osx-64`, and `osx-arm64` support.
- Fix the existing v0.19.3 package by incrementing its build number.
- Validate packaging behavior through real recipe/build execution, not tests that grep recipe or workflow text.
- Push only after the public-data scrub passes.

---

### Task 1: Build and publish the corrected feedstock recipe

**Files:**
- Modify: `recipe/recipe.yaml`

**Interfaces:**
- Consumes: upstream msgvault targets `web-install` and `web-embed`; validator `scripts/check-web-assets.mjs --binary <path>`.
- Produces: a v0.19.3 Conda package revision whose `msgvault` executable embeds the Vite release graph.

- [ ] **Step 1: Create or reuse the personal fork and clone it**

Use GitHub CLI to create the fork without cloning when it does not exist. Clone `mariusvniekerk/msgvault-feedstock`, add `https://github.com/conda-forge/msgvault-feedstock.git` as `upstream`, and configure the upstream push URL to `DISABLED` so accidental pushes fail.

- [ ] **Step 2: Create a topic branch from current upstream main**

Fetch `upstream/main`, verify the working tree is clean, and create `fix/embed-web-assets` from the fetched upstream commit.

- [ ] **Step 3: Verify the regression against the current package**

Install or extract `msgvault=0.19.3=*_0` into scratch state and run the v0.19.3 validator:

```bash
node scripts/check-web-assets.mjs --binary <scratch-prefix>/bin/msgvault
```

Expected: failure stating that the binary does not embed release asset bytes, including `.vite/manifest.json`.

- [ ] **Step 4: Update the recipe**

In `recipe/recipe.yaml`:

- change `build.number` from `0` to `1`;
- add `make`, `bun >=1.3,<2`, and `nodejs >=20.19,<25` to `requirements.build` because the upstream generation and validation scripts invoke `node` directly;
- change the build script to enter the source root, run `make web-install web-embed`, then retain the existing license collection, Go compilation, and shell-completion generation;
- run `node scripts/check-web-assets.mjs --binary "$PREFIX/bin/msgvault"` immediately after the Go build.

Use platform-neutral shell syntax already supported by the Unix-only recipe.

- [ ] **Step 5: Render and lint the recipe**

Run the repository's current Conda-Forge/rattler-build validation path. Resolve all parser, linter, and dependency errors without changing the platform matrix.

- [ ] **Step 6: Build the native package in scratch state**

Build the recipe locally with Conda-Forge channels. Do not install over any existing msgvault binary or use the normal msgvault data directory. Preserve the build log and package path long enough to inspect failures.

- [ ] **Step 7: Verify the packaged executable**

Extract or install the newly built package into a scratch prefix and run:

```bash
node scripts/check-web-assets.mjs --binary <scratch-prefix>/bin/msgvault
```

Expected: success with a nonzero count of validated release web assets. Also run `msgvault --help` and `msgvault version` from the scratch prefix.

- [ ] **Step 8: Commit the feedstock change**

Review the full diff and recent feedstock commit style. Stage only the intended recipe change, run hooks, and create a rationale-focused commit explaining that build 0 silently embedded only the web stub.

- [ ] **Step 9: Scrub and push to the personal fork**

Scan the commit, patch, introduced blobs, and proposed PR text for private data and credentials. Push `fix/embed-web-assets` only to the fork's `origin`.

- [ ] **Step 10: Open the Conda-Forge pull request**

Open a PR from `mariusvniekerk:fix/embed-web-assets` into `conda-forge/msgvault-feedstock:main`. Explain the missing UI, why a build-number bump is required, and why the feedstock now compiles upstream's locked frontend before Go embedding. Do not include a test-plan section.

- [ ] **Step 11: Link the PR from msgvault issue #601**

Comment on `kenn-io/msgvault#601` with the feedstock PR URL. State that Conda-Forge used the same generated tag archive and therefore embedded the same stub-only asset tree.
