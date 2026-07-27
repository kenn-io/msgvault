# Container Image Freshness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make newly generated NAS Compose bundles explicitly refresh the GHCR `latest` image during service reconciliation and clearly distinguish restart from update operations.

**Architecture:** The production `createNASBundle` generator adds the Compose service-level `pull_policy: always` next to the existing moving image tag. A production-path unit test pins the emitted contract, while the deployment guide and changelog explain that restart reuses the installed image and the reliable update sequence is pull followed by `up -d`.

**Tech Stack:** Go, Docker Compose Specification, testify, Markdown.

## Global Constraints

- Follow `CLAUDE.md` and the repository `AGENTS.md`.
- All Go tests use `github.com/stretchr/testify`; equality assertions use `(want, got)`.
- Every `go test` invocation includes `-tags "fts5 sqlite_vec"`.
- Do not change GHCR credentials, package visibility, workflow permissions, image tags, release cadence, or multi-architecture publishing.
- Do not add a msgvault CLI flag, configuration key, API change, or dependency.
- Preserve the generated image, port, volume, environment, health-check, restart, and Synology user settings.
- Existing generated bundles are not rewritten automatically.
- Invoke `kenn:commit` immediately before every `git commit`.

## File Structure

- Modify `cmd/msgvault/cmd/setup_test.go`: production-generator regression for the explicit pull policy.
- Modify `cmd/msgvault/cmd/setup.go`: add the service-level Compose pull policy.
- Modify `docs/guides/remote-deployment.md`: update the generated example and distinguish restart, reconciliation, and explicit update.
- Modify `docs/changelog.md`: add the user-visible NAS image freshness fix under Unreleased.
- Modify `docs/superpowers/specs/2026-07-26-container-image-freshness-design.md`: mark the design implemented only after all acceptance gates pass.

---

### Task 1: Make Generated NAS Bundles Refresh the Moving Image

**Files:**

- Modify: `cmd/msgvault/cmd/setup_test.go:50-59`
- Modify: `cmd/msgvault/cmd/setup.go:279-301`
- Modify: `docs/guides/remote-deployment.md:93-114`
- Modify: `docs/guides/remote-deployment.md:315-336`
- Modify: `docs/changelog.md:8-17`
- Modify: `docs/superpowers/specs/2026-07-26-container-image-freshness-design.md:3-6`

**Interfaces:**

- Consumes: `func createNASBundle(bundleDir, apiKey, oauthSecretsPath string, port int) error`.
- Produces: generated Compose service field `pull_policy: always`.
- Preserves: `image: ghcr.io/kenn-io/msgvault:latest` and every other generated service setting.

- [ ] **Step 1: Add the failing production-generator assertion**

In `TestCreateNASBundle`, add the assertion immediately after the existing
GHCR image assertion:

```go
assert.Contains(
	composeStr,
	"pull_policy: always",
	"docker-compose.yml should refresh the latest image during reconciliation",
)
```

This continues exercising the real `createNASBundle` output; do not construct a
synthetic Compose fixture or inspect `setup.go` as text.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run '^TestCreateNASBundle$' -count=1
```

Expected: FAIL at the new `assert.Contains` because the generated
`docker-compose.yml` does not contain `pull_policy: always`.

- [ ] **Step 3: Add the minimal generated Compose policy**

In the `dockerCompose` template in `createNASBundle`, add exactly one service
field next to the image:

```yaml
services:
  msgvault:
    image: ghcr.io/kenn-io/msgvault:latest
    pull_policy: always
    container_name: msgvault
```

Do not change the template's remaining user, restart, port, volume,
environment, command, or health-check fields.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run:

```bash
gofmt -w cmd/msgvault/cmd/setup.go cmd/msgvault/cmd/setup_test.go
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run '^TestCreateNASBundle$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Update the deployment guide**

Add `pull_policy: always` to the generated Compose example:

```yaml
services:
  msgvault:
    image: ghcr.io/kenn-io/msgvault:latest
    pull_policy: always
    container_name: msgvault
```

Replace the container-management restart/update portion with:

```bash
# Restart using the currently installed image
docker-compose restart

# Reconcile the service; generated bundles check for a newer latest image
docker-compose up -d

# Explicitly update to the latest image
docker-compose pull
docker-compose up -d
```

After that code block, add:

```markdown
`restart` does not check the registry or replace the image. Generated bundles
set `pull_policy: always`, so `up -d` reconciles against GHCR. The explicit
`pull` followed by `up -d` sequence remains the clearest update procedure
across NAS Compose implementations.
```

- [ ] **Step 6: Add the changelog entry and mark the design implemented**

Append this second bullet under `## Unreleased` → `**Bug fixes**`:

```markdown
- Newly generated NAS Compose bundles explicitly pull the GHCR `latest` image
  when reconciling the service, and deployment guidance now distinguishes
  restarting the installed container from updating it.
```

Only after all verification in Step 7 passes, change the design status to:

```markdown
Implemented and acceptance-tested on 2026-07-26.
```

- [ ] **Step 7: Run acceptance verification**

Run:

```bash
gofmt -w cmd/msgvault/cmd/setup.go cmd/msgvault/cmd/setup_test.go
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run '^TestCreateNASBundle' -count=1
go vet -tags "fts5 sqlite_vec" ./...
make test
make lint
git diff --check
```

Expected: every command exits zero; lint reports `0 issues`; the diff check has
no output.

Review the final diff:

```bash
git diff --stat
git diff -- cmd/msgvault/cmd/setup.go cmd/msgvault/cmd/setup_test.go \
  docs/guides/remote-deployment.md docs/changelog.md \
  docs/superpowers/specs/2026-07-26-container-image-freshness-design.md
```

Expected: only the five planned files changed, with no workflow, registry,
tag, API, or unrelated configuration changes.

- [ ] **Step 8: Commit**

Invoke `kenn:commit`, then:

```bash
git add cmd/msgvault/cmd/setup.go cmd/msgvault/cmd/setup_test.go \
  docs/guides/remote-deployment.md docs/changelog.md \
  docs/superpowers/specs/2026-07-26-container-image-freshness-design.md
git commit -m "fix: keep generated NAS deployments on latest image" \
  -m "A current GHCR image can remain unapplied when operators treat restart as update or a NAS reuses local state. Make the moving-image policy explicit in generated bundles and document the pull/reconcile boundary without changing release publishing."
```
