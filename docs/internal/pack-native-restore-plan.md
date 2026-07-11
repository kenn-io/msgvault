# Pack-Native Attachment Restore Implementation Plan

> **For agentic workers:** REQUIRED: Use `superpowers:subagent-driven-development` if subagents are available, or `superpowers:executing-plans` otherwise, to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore compatible repository packs directly into msgvault's production attachment store while preserving verified mixed/loose fallback, crash safety, and the current restore proof.

**Architecture:** Kit first adds an application-neutral `packstore` import session and optional `backup.Restore` target. Import validates and durably publishes packs through the held restore root, returns the exact packed subset, and commits authority through one application transaction after loose fallback is durable. Msgvault supplies the staged-SQLite catalog, enables packed restore by default, and retains `--loose-attachments`.

**Tech Stack:** Go 1.26, Kit `pack`/`packstore`/`backup`, SQLite, Cobra, Testify, native Windows GitHub Actions, Nix.

---

## Working rules

- Branches: Kit `pack-native-restore` from current main, then this msgvault branch.
- Use `@superpowers:test-driven-development` and `@kenn:commit` for every task.
- Use `@kenn:isolate-prod` before anything accepting a data directory.
- Use `@kenn:scrub-private-data` before every public push or PR update.
- Never name private consumers in public artifacts; describe requirements generically.
- Never invoke roborev review/skills unless the user explicitly requests them.
- Use Testify. Msgvault tests require `-tags "fts5 sqlite_vec"`.
- Do not merge PRs. The user merges/tags Kit before msgvault's release pin.
- One focused commit per task; never amend or squash unless asked.

## File map

### Kit

- Create `packstore/import.go` and tests: compatibility, selected verification,
  rooted publication, retries, prepared catalog commit.
- Modify `packstore/preflight.go` and `types.go` for shared bounded inspection
  and narrow public types.
- Create `backup/restore_packed.go` and modify `restore.go`/`app.go`/tests for
  optional mixed restore.
- Update `backup/FORMAT.md`, `packstore/doc.go`, and generic `AGENTS.md` hygiene.

### Msgvault

- Create `internal/store/pack_restore.go` and tests: targeted SQLite schema and
  atomic replacement.
- Create `internal/backupapp/packed_restore.go` and tests: target adapter.
- Modify backup lifecycle/compat tests and `cmd/msgvault/cmd/backup.go`.
- Add `internal/backupapp/restore_benchmark_test.go` and a separate Windows job.
- Update CLI/architecture/changelog docs and Kit/Nix pins.

## Proposed Kit capability

Names may improve during implementation; preserve this split:

```go
type RestoreCatalog interface {
	ReplaceRestoredPacks(context.Context, []PackRecord, []Adoption) error
}

type ImportSelection struct {
	Hash      Hash
	RawLen    int64
	Offset    uint64
	StoredLen uint64
	Flags     uint8
}

type ImportPack struct {
	PackID      string
	SourcePath  string
	Selections []ImportSelection
}

type FallbackReason string

type ImportFallback struct {
	PackID string
	Hash   Hash // empty for whole-pack fallback
	Reason FallbackReason
}

type ImportOptions struct {
	Limits    Limits
	CreatedAt time.Time // restore time; zero is invalid
}

func PrepareImport(
	ctx context.Context,
	target *os.Root,
	contentDir string,
	packs []ImportPack,
	opts ImportOptions,
) (*PreparedImport, error)

func (p *PreparedImport) PackedHashes() []Hash
func (p *PreparedImport) Stats() ImportStats
func (p *PreparedImport) Commit(context.Context, RestoreCatalog) error
```

`PrepareImport` publishes files but grants no authority. `Commit` makes one
idempotent application call with full-footer records and selected adoptions.

```go
type PackedContentTarget interface {
	Limits() packstore.Limits
	OpenRestoreCatalog(
		context.Context,
		*sql.DB, // unpublished staged DB
	) (packstore.RestoreCatalog, error)
}

// RestoreOptions addition; nil preserves current loose behavior.
PackedContent PackedContentTarget

// RestoreResult additions.
PackedAttachmentBlobs int64
LooseAttachmentBlobs  int64
AttachmentPacks       int
PackFallbacks         []packstore.ImportFallback
```

The seam runs after page materialization and set agreement, before
`proveRestoredDB`. Kit opens the staged DB read-write through the held target
and rechecks identity around the stats-neutral application mutation.

## Task 1: Create Kit worktree and prove baseline

**Files:** modify `kit/AGENTS.md`.

- [ ] Create isolated worktree:

```bash
git fetch origin --prune
git worktree add "$HOME/worktrees/github.com/kenn-io/kit/pack-native-restore" \
  -b pack-native-restore origin/main
```

- [ ] Run `go mod download`, `go test ./...`, and `go vet ./...`. Expected: pass.
- [ ] Add a generic content-hygiene rule forbidding private downstream names in
  public artifacts.
- [ ] Run `@kenn:commit`: `Record public content hygiene`.

## Task 2: Define import compatibility and verification

**Files:** create `kit/packstore/import.go` and `import_test.go`; modify
`preflight.go` and `types.go`.

- [ ] Write failing tests:

```go
func TestPrepareImportUsesConfiguredLimits(t *testing.T)
func TestPrepareImportFallsBackWholePackForContainerLimit(t *testing.T)
func TestPrepareImportFallsBackOnlyOversizedSelectedEntry(t *testing.T)
func TestPrepareImportRejectsCorruptSourceInsteadOfFallingBack(t *testing.T)
```

Use a real two-blob plain-v1 pack and a custom `BlobBytes`.

- [ ] Confirm red:

```bash
go test ./packstore -run 'TestPrepareImport' -count=1
```

- [ ] Implement typed compatible/incompatible/corrupt outcomes without changing
  `OpenMaintenancePack` behavior. Configured container/footer/count limits are
  fallback; truncation, checksum, duplicate IDs, invalid spans, metadata/hash
  mismatch are hard errors.
- [ ] For eligible selected entries compare index/footer metadata, bounded-read,
  verify CRC/decode length/SHA-256/expected size. Oversized selected entries
  fall back without bounded allocation; unselected payloads are not read.
- [ ] Run:

```bash
go test ./packstore \
  -run 'TestPrepareImport|TestOpenMaintenancePack|TestPreflight' -count=1
```

Expected: pass.

- [ ] Commit: `Define verified pack import compatibility`.

## Task 3: Publish durably and make retry idempotent

**Files:** modify Kit import files; create `import_windows_test.go`.

- [ ] Write failing tests:

```go
func TestPrepareImportPublishesBeforeCatalogAuthority(t *testing.T)
func TestPrepareImportReusesByteIdenticalDestination(t *testing.T)
func TestPrepareImportRefusesPackIDCollision(t *testing.T)
func TestPreparedImportCatalogFailureLeavesPublishedOrphan(t *testing.T)
func TestPreparedImportRecordsFullFooterTotalsForSelectedSubset(t *testing.T)
```

- [ ] Confirm red with focused `go test ./packstore`.
- [ ] Implement held-`os.Root` publication to
  `<content>/packs/<shard>/<id>.mvpack`: local-path validation, private staging,
  streaming copy+container digest, sync/close, no-clobber rename, directory
  sync, reopen and preflight. Reuse existing destination only when
  byte-identical and compatible. Never move/delete source bytes.
- [ ] Add production maintainer contract test:

```go
func TestPreparedImportRetryAcrossMaintainerOrphanDisposition(t *testing.T)
```

Prove current adopt/remove/quarantine dispositions, then retry: reuse a retained
pack only when the new selection still verifies, recopy after remove, and fail
hard when retained bytes do not verify. Retry must not depend on any
disposition.

- [ ] Add Windows tests for closed-handle rename/reopen, byte-identical reuse,
  and collision refusal. The importer must never delete or replace an existing
  final pack, so these tests must not manufacture a delete-sharing dependency.
- [ ] Run `go test ./packstore -count=1` and
  `go test -race ./packstore -count=1`. Expected: pass.
- [ ] Commit: `Publish imported packs before catalog authority`.

## Task 4: Add optional mixed restore to Kit backup

**Files:** create `kit/backup/restore_packed.go`; modify `restore.go`, `app.go`,
`restore_test.go`, and generic app tests.

- [ ] Pin nil-target compatibility:

```go
func TestRestoreWithoutPackedTargetRemainsFullyLoose(t *testing.T)
```

Assert zero packed count, loose equals total, no production packs directory,
and every content path materialized.

- [ ] Write failing production-path tests:

```go
func TestRestorePackedTargetPublishesThenCommitsBeforeProof(t *testing.T)
func TestRestorePackedTargetFallsBackDeclinedEntriesLoose(t *testing.T)
func TestRestorePackedTargetCatalogFailureDoesNotPublishDatabase(t *testing.T)
func TestRestorePackedTargetProofFailureKeepsVisibleDatabase(t *testing.T)
func TestRestorePackedTargetOverwriteKeepsOldDatabaseUntilPublish(t *testing.T)
```

- [ ] Confirm red with
  `go test ./backup -run 'TestRestore.*Packed|TestRestoreWithoutPacked'`.
- [ ] Refactor attachment restore: validate snapshot/DB sets, group repository
  packs, prepare import, materialize hashes absent from `PackedHashes` loose,
  open staged DB writable through held root, open catalog, commit, recheck
  identity, then run existing proof/publish.
- [ ] Nil target remains unchanged. Supplied target with non-`.mvpack` extension
  restores all loose but commits empty replacement metadata.
- [ ] Require packed + loose counts equal total.
- [ ] Corrupt selected source must hard fail; separately prove well-formed
  incompatibility succeeds loose.
- [ ] Run `go test ./backup ./packstore -count=1` and race equivalent.
- [ ] Commit: `Restore compatible content packs through an optional target`.

## Task 5: Validate and publish Kit stage

**Files:** modify `backup/FORMAT.md`, `packstore/doc.go`, public Go docs.

- [ ] Document representation-neutral membership, selected SHA-256 verification,
  publish-before-authority, unauthorized extras, configured limits, nil target.
- [ ] Run:

```bash
go fmt ./...
go mod tidy
git diff --exit-code -- go.mod go.sum
go test ./...
go test -race ./backup ./packstore
go vet ./...
```

- [ ] Run `@kenn:scrub-private-data` over commits, diff, fixtures, and drafted PR.
- [ ] Commit docs: `Document verified packed-content restore`.
- [ ] Use `@kenn:commit-push-pr` to open a generic Kit PR. No private names and
  no Test Plan/Validation section.
- [ ] Require Kit Ubuntu/macOS/Windows CI. Diagnose; do not weaken tests.
- [ ] Pin the reviewed commit pseudo-version in msgvault for development. Do not
  call msgvault merge-ready until Kit is merged/tagged.

## Task 6: Add msgvault staged SQLite catalog

**Files:** create `internal/store/pack_restore.go` and test; modify catalog
conversions only if needed.

- [ ] Write failing historical-schema tests:

```go
func TestRestorePackCatalogCreatesOnlyPackSchema(t *testing.T)
func TestLooseMetadataClearDoesNotCreatePackSchema(t *testing.T)
```

- [ ] Write failing replacement tests:

```go
func TestRestorePackCatalogReplacesMetadataAtomically(t *testing.T)
func TestRestorePackCatalogRejectsNonSnapshotHash(t *testing.T)
func TestRestorePackCatalogKeepsFullFooterTotals(t *testing.T)
func TestRestorePackCatalogRollbackPreservesPriorMetadata(t *testing.T)
func TestRestorePackCatalogUsesRestoreTime(t *testing.T)
```

Include content/thumbnail refs, uppercase aliases, and two case-equivalent
aliases on one message.

- [ ] Confirm red:

```bash
go test -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestRestorePackCatalog|TestLooseMetadataClear' -count=1
```

- [ ] Implement exact targeted SQLite DDL only; never call `InitSchema` or touch
  live/configured storage.
- [ ] Implement one transaction: prevalidate records/entries; derive canonical
  snapshot membership; reject outsiders; delete old index then records; insert
  full-footer records and selected mappings; commit. Do not rewrite attachment
  rows/paths.
- [ ] Run focused then full `internal/store` tests with required tags.
- [ ] Commit: `Replace restored attachment pack authority atomically`.

## Task 7: Wire msgvault target and CLI

**Files:** create `internal/backupapp/packed_restore.go` and test; modify
`cmd/msgvault/cmd/backup.go` and focused tests.

- [ ] Write failing target tests for default/custom limits, staged catalog, and
  no live-store access.
- [ ] Implement narrow target:

```go
type PackedRestoreTarget struct {
	limits packstore.Limits
}

func NewPackedRestoreTarget(packstore.Limits) *PackedRestoreTarget
func (t *PackedRestoreTarget) Limits() packstore.Limits
func (t *PackedRestoreTarget) OpenRestoreCatalog(
	context.Context, *sql.DB,
) (packstore.RestoreCatalog, error)
```

- [ ] Write failing CLI tests for default packed target,
  `--loose-attachments`, packed/mixed output, conditional pack recommendation,
  and successful compatibility fallback.
- [ ] Register:

```go
backupRestoreCmd.Flags().BoolVar(
	&backupRestoreLooseAttachments,
	"loose-attachments",
	false,
	"restore attachments as loose files instead of installing compatible packs",
)
```

Default passes target with production limits. Explicit loose passes nil and
retains table-aware post-publish metadata clear.

- [ ] Run tagged backupapp/cmd tests. Expected: pass.
- [ ] Commit: `Restore compatible attachment packs by default`.

## Task 8: Prove lifecycle and compatibility

**Files:** modify backup content/compat tests and focused API/MCP/export tests.

- [ ] Add:

```go
func TestBackupPackedRestoreLifecycle(t *testing.T)
```

Real path: add canonical/legacy attachments and thumbnails; pack; create/verify
backup; packed restore; assert catalog/footer/no eligible loose; read all;
backup/verify restored; unpack/hash/repack/hash.

- [ ] Add mixed custom-limit sibling, stale source metadata, frozen pre-pack
  fixture, explicit loose fresh target, and overwrite old-pack cases.
- [ ] Exercise real raw API, MCP get_attachment, CLI export, and backup capture
  against restored target. Assert bytes/hashes, not stub arguments.
- [ ] Run:

```bash
go test -tags "fts5 sqlite_vec" \
  ./internal/backupapp ./internal/api ./internal/mcp ./internal/export \
  ./cmd/msgvault/cmd -count=1
go test -race -tags "fts5 sqlite_vec" \
  ./internal/backupapp ./internal/store ./cmd/msgvault/cmd -count=1
```

- [ ] Commit: `Prove packed restore across attachment read surfaces`.

## Task 9: Add native Windows measurement

**Files:** create `internal/backupapp/restore_benchmark_test.go`; modify CI.

- [ ] Write `BenchmarkBackupRestoreLayouts` using deterministic incompressible
  small blobs, real DB, real Kit backup, separate loose/packed targets.
- [ ] Report attachment files/op, pack files/op, MiB/s, blob/byte counts,
  attachment/proof/total durations. Assert file counts and hashes, never timing.
- [ ] Run locally:

```bash
go test -tags "fts5 sqlite_vec" ./internal/backupapp \
  -run '^$' -bench '^BenchmarkBackupRestoreLayouts$' -benchtime=1x -count=1
```

- [ ] Add separate `test-windows-pack-restore` job with current Windows setup:

```powershell
go test -tags "fts5 sqlite_vec" ./internal/backupapp `
  -run '^$' -bench '^BenchmarkBackupRestoreLayouts$' -benchtime=1x -count=3
```

Use a separate timeout and upload output; do not lengthen full Windows job.

- [ ] Commit: `Measure loose and packed restore on Windows`.

## Task 10: Update user docs honestly

**Files:** modify CLI reference, backup-format architecture, changelog.

- [ ] Document automatic compatible import, selected-byte hashing, loose
  incompatible/oversized refs, explicit loose flag, the fresh-target-only
  guarantee for fully loose storage, and overwrite reclamation.
- [ ] Run `make docs-build`. Expected: no issues.
- [ ] Commit: `Document pack-native backup restore`.

## Task 11: Pin released Kit and run all gates

**Files:** `go.mod`, `go.sum`, `nix/package.nix`.

- [ ] Wait for user-controlled Kit merge/tag and verify tag commit.
- [ ] Run `go get go.kenn.io/kit@<tag>` and `go mod tidy`; update Nix vendor hash
  from expected first mismatch.
- [ ] Run:

```bash
go fmt ./...
make test
make lint-ci
go vet -tags "fts5 sqlite_vec" ./...
go test -race -tags "fts5 sqlite_vec" \
  ./internal/backupapp ./internal/store ./cmd/msgvault/cmd
nix build
make docs-build
```

- [ ] Run disposable PostgreSQL catalog suite:

```bash
MSGVAULT_TEST_DB='postgresql://postgres@127.0.0.1:<port>/postgres?sslmode=disable' \
  go test -tags "fts5 sqlite_vec" ./internal/store -count=1
```

- [ ] Commit: `Pin Kit packed restore release`.

## Task 12: Harden isolated archive and compare main

**Files:** none unless defect/evidence update.

- [ ] Use `@kenn:isolate-prod`; keep live binary/archive/daemon/repository
  untouched. Build exact main/branch binaries to scratch.
- [ ] Run order-balanced restores on same verified snapshot and separate targets:
  branch-packed then main-loose; reverse order; repeat if cache-sensitive.
- [ ] Record blobs/bytes, loose files, pack files, catalog counts, attachment
  stage, proof, total wall time, and peak RSS.
- [ ] SHA-256 verify five largest, representative small, uppercase alias; read
  CLI/API/MCP; create and fully verify backup of packed restored target.
- [ ] On scratch: interrupt after pack publish; retry overwrite; inject catalog
  failure; repack/read; unpack/full-hash compare; repack/verify.
- [ ] Require no material regression in DB/proof/read/pack/repack/subsequent
  backup. Expected gain is file count/wall time, not source bytes read.
- [ ] Stop scratch daemons and remove only artifacts created here.

## Task 13: Publish msgvault PR and finish CI

- [ ] Run `@kenn:scrub-private-data` on commits, diff, fixtures, selected
  benchmark output, and drafted PR metadata.
- [ ] Use `@kenn:commit-push-pr`. Suggested title:

```text
Restore attachment packs directly from backups
```

Body: automatic compatible import, loose fallback/flag, authority-before-proof,
released Kit. No private names, implementation narration, or Test Plan section.
Close #466.

- [ ] Require macOS gates, native Windows full tests, native Windows
  measurement, PostgreSQL/pgvector, Nix, release smoke, and only explicitly
  requested review jobs.
- [ ] Diagnose with `@superpowers:systematic-debugging`; never weaken integrity,
  crash, Windows, or performance assertions. Commit fixes and refresh PR if
  scope changes.
- [ ] Final audit:

```bash
git status --short --branch
gh pr checks <number>
git rev-parse HEAD
git rev-parse '@{upstream}'
```

Confirm clean/equal heads, green checks, clean public scans, tagged Kit pin,
recorded real-archive/Windows evidence, and no agent merge.
