# Container Image Freshness Design

## Status

Implemented and acceptance-tested on 2026-07-26.

## Problem

msgvault publishes multi-architecture images to GitHub Container Registry
(GHCR) after successful pushes to the default branch. The generated NAS
deployment bundle references `ghcr.io/kenn-io/msgvault:latest`, but its Compose
service does not declare a pull policy.

Current [Docker Compose service documentation](https://docs.docker.com/reference/compose-file/services/#pull_policy)
states that `latest` is pulled even under the default `missing` policy.
However, the generated bundle does not make that moving-image policy explicit,
and `docker compose restart` only restarts the existing container without
checking the registry. Older or platform-managed NAS flows can make the
distinction between restarting and reconciling a service easy to miss, leaving
a healthy, current registry image unapplied.

The current publishing workflow is not the source of the reported drift:
the GHCR publish job succeeds on default-branch pushes and the public
`latest` manifest carries the current default-branch revision for both
`linux/amd64` and `linux/arm64`.

## Goals

- Make generated NAS deployment bundles refresh `latest` when users run
  `docker compose up -d`.
- Keep the existing GHCR image name and multi-architecture publishing flow.
- State clearly that `restart` reuses the installed image and is not an update
  command.
- Exercise the real bundle generator in a regression test.

## Non-goals

- Pulling a new image into an already-running container without a Compose
  reconciliation command.
- Changing GHCR credentials, package visibility, workflow permissions, image
  tags, or release cadence.
- Pinning generated bundles to a specific release or digest.
- Supporting a separate Google Container Registry image.

## Considered Approaches

### 1. Always pull the moving tag

Add `pull_policy: always` to the generated Compose service. Compose
implementations that support the field then check GHCR whenever
`docker compose up -d` reconciles the service.

This is the selected approach. It matches the existing choice of the moving
`latest` tag, makes the intended policy explicit, and leaves an explicit
`pull` followed by `up -d` as the documented update path rather than changing
a publishing workflow that is already current.

### 2. Pin generated bundles to a release tag

A release tag would make deployments reproducible, but it would deliberately
prevent automatic refresh. Users would need to edit or regenerate the bundle
for each version, which does not match the current `latest`-based experience.

### 3. Documentation only

The documentation could continue requiring an explicit
`docker compose pull` before `up`. This is broadly compatible but leaves the
generated configuration vulnerable to the same omission that caused the
reported stale deployment.

## Selected Design

The `docker-compose.yml` emitted by `createNASBundle` adds:

```yaml
pull_policy: always
```

at the msgvault service level, next to the `image` declaration. No other
service settings change.

The generated bundle continues to use:

```yaml
image: ghcr.io/kenn-io/msgvault:latest
```

With a Compose implementation that supports the policy, running
`docker compose up -d` checks the registry and recreates the container when
the image digest has changed. Running `docker compose restart` keeps the
existing image. The documented, implementation-independent update sequence
remains `docker compose pull` followed by `docker compose up -d`.

## Testing

`TestCreateNASBundle` continues exercising the production bundle generator and
asserts that the emitted service contains both the GHCR `latest` image and
`pull_policy: always`. The test is written before the generator change and is
observed failing because the pull policy is absent.

The focused command-package test runs with the required
`fts5 sqlite_vec` build tags. Repository formatting, tagged tests, vet, and
lint remain final acceptance gates.

## Documentation

The remote-deployment guide's generated Compose example includes the pull
policy. Its container-management section distinguishes:

- restart the current image: `docker compose restart`;
- reconcile and refresh `latest`: `docker compose up -d`; and
- explicit update: `docker compose pull` followed by
  `docker compose up -d`.

The Unreleased changelog notes that generated NAS bundles no longer silently
reuse a stale cached `latest` image during Compose reconciliation.

## Compatibility

- No msgvault CLI flag, configuration key, API contract, image tag, or
  registry changes.
- Existing generated bundles are not rewritten automatically; users may add
  the pull policy manually or regenerate their deployment bundle.
- Explicitly pinned user-authored Compose files remain unaffected.
- The behavior depends on a Compose implementation that supports the Compose
  Specification's service-level `pull_policy`.

## Acceptance Criteria

- A newly generated `docker-compose.yml` contains
  `pull_policy: always` for the msgvault service.
- The existing image, port, volume, environment, health-check, restart, and
  Synology user settings remain unchanged.
- Documentation does not describe `docker compose restart` as an image update.
- The focused generator regression, full tagged test suite, vet, and lint pass.
