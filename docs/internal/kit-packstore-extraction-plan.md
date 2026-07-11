# Kit Packed-CAS Extraction Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract msgvault's proven mixed loose/packed CAS into `go.kenn.io/kit/packstore`, then migrate msgvault onto it without changing storage or user-visible behavior.

**Architecture:** Kit owns physical CAS layout, loose I/O, mixed reads, reader retirement, repair, pack, unpack, and repack. Msgvault retains schema and product policy behind semantic catalog adapters. Work lands Kit-first, then msgvault proves old/new compatibility; streaming and downstream document archive adoption receive separate plans after this gate passes.

**Tech Stack:** Go 1.26, `go.kenn.io/kit/pack`, SHA-256, zstd pack frames, SQLite/PostgreSQL adapters, testify, GitHub Actions on Linux/macOS/Windows.

---

## Scope and repository order

This plan implements stages 1 and 2 of
`docs/internal/kit-packstore-extraction-design.md` only:

1. Add and verify `packstore` in `~/code/kit`.
2. Push the Kit branch so msgvault can use its reviewed commit temporarily.
3. Add the msgvault adapter and migrate msgvault on a branch from the merged
   packed-attachments baseline, using an interim pseudo-version if the Kit
   release is not yet tagged.
4. Prove compatibility before deleting msgvault's duplicated engine.
5. Land and tag Kit, then replace the interim pin with the tagged release
   before the msgvault PR is merge-ready.

Do not implement streaming pack I/O or the downstream document archive adapter in this plan. Their
APIs depend on evidence from the msgvault compatibility migration and should be
planned independently after this plan's final gate.

Use a dedicated branch/worktree in each repository. Never use a committed
`replace` directive. A temporary, untracked `go.work` may join the two local
checkouts during development. Once the Kit branch is pushed, msgvault may pin
that exact commit as an interim pseudo-version. This is a new development
practice, not a precedent from the prior backup extraction, which moved between
tagged Kit releases. The final msgvault dependency must be a tagged Kit release.

Before every commit, follow each repository's `AGENTS.md` and run `prek run`.
New tests use testify. Some agent environments also provide user-level
`kenn:*` skills that are not part of either repository: when available, use
`kenn:commit` before commits, `kenn:scrub-private-data` before publishing
fixtures/docs, and `kenn:isolate-prod` before real-archive work. When those
skills are unavailable, perform the equivalent manual commit review,
private-data audit, and production-isolation checks rather than blocking on a
missing skill name.

## Planned file structure

### Kit

- `packstore/doc.go` — package contract and authority model.
- `packstore/types.go` — app-neutral hash, location, pack record, index entry,
  candidate, limits, stats, and typed event definitions.
- `packstore/catalog.go` — small read and maintenance catalog interfaces.
- `packstore/packstoretest/catalog.go` — reusable catalog conformance suite for
  the in-memory fake and application database adapters.
- `packstore/layout.go` — validated loose/pack paths and owned staging layouts.
- `packstore/coordinator.go` — shared mutation and exclusive maintenance leases.
- `packstore/loose.go` — streaming loose publication, dedup policy, durability,
  listing, cleanup, and removal.
- `packstore/identity_unix.go`, `packstore/identity_windows.go`, and
  `packstore/identity_other.go` — stable no-follow file identity behavior.
- `packstore/store.go` — catalog-aware mixed reads and bounded retry logic.
- `packstore/cache.go` — bounded ordinary/maintenance pack-reader cache and
  retirement.
- `packstore/preflight.go` — stable plain-v1 bounded pack parser and limit
  errors extracted from msgvault.
- `packstore/pack.go` — repair, orphan reconciliation, packing, and loose sweep.
- `packstore/unpack.go` — all-pack preflight and durable loose downgrade.
- `packstore/repack.go` — zero-live retirement and source-isolated exact-set
  replacement.
- Matching focused `*_test.go` and build-tagged Windows/Unix tests beside each
  file.
- `packstore/testdata/msgvault-v1/` — small synthetic frozen compatibility pack
  plus metadata manifest; no real archive content.

### Msgvault

- `internal/store/pack_catalog.go` — compile-time adapter assertions and any
  thin conversions from existing pack SQL types to Kit types.
- `internal/export/store_attachment.go` — MIME-facing wrappers around Kit loose
  writes; product metadata behavior remains here.
- `cmd/msgvault/cmd/attachment_maintenance.go` — construct Kit maintenance,
  translate stats/events, and preserve job/command policy.
- Existing API, MCP, export, backup, and server wiring files — change concrete
  blob-store construction/imports only.
- `internal/packcompat/` — temporary old/new cross-read tests, deleted with the
  old engine only after the fixture and end-to-end gates cover the contract.
- `go.mod`, `go.sum` — pin the reviewed Kit commit.
- Delete `internal/blobstore`, `internal/packer`, and `internal/repacker` only in
  the final migration task.

## Task 1: Freeze the msgvault compatibility corpus

**Files:**

- Create: `internal/packcompat/fixture_test.go`
- Create: `internal/packcompat/testdata/msgvault-v1/manifest.json`
- Create: `internal/packcompat/testdata/msgvault-v1/<pack-id>.mvpack`
- Reference: `internal/blobstore/blobstore_test.go`
- Reference: `internal/packer/packer_test.go`

- [ ] **Step 1: Write a failing fixture test**

  Add a test that expects a frozen synthetic pack containing an empty blob,
  small uncompressed blob, and compressed blob. The manifest records canonical
  hash, size, pack ID, pack-file SHA-256, footer entry fields, and expected
  content. Open it with the current `internal/blobstore` through a minimal
  resolver and assert every field and byte sequence with testify.

- [ ] **Step 2: Run the focused test and observe the missing fixture failure**

  Run:
  `go test -tags "fts5 sqlite_vec" ./internal/packcompat -run TestFrozenMsgvaultV1Pack -count=1`

  Expected: FAIL because `testdata/msgvault-v1/manifest.json` is absent.

- [ ] **Step 3: Generate the fixture through the current production writer**

  Use a temporary test-only generator that calls `pack.NewWriter`, seals the
  pack, and serializes the actual footer/index fields. Inputs must be obvious
  synthetic strings. Remove the generator after committing the frozen output;
  the test must read, not regenerate, the fixture.

- [ ] **Step 4: Prove the current implementation reads the frozen corpus**

  Re-run the focused command. Expected: PASS with three blobs and exact footer
  metadata. Use the user-level `kenn:scrub-private-data` skill when available,
  or manually audit the synthetic fixture directory before publishing it.

- [ ] **Step 5: Commit**

  Commit in msgvault with subject: `Freeze packed-CAS compatibility fixtures`.

## Task 2: Define Kit's app-neutral contracts and layout

**Files:**

- Create: `packstore/doc.go`
- Create: `packstore/types.go`
- Create: `packstore/catalog.go`
- Create: `packstore/layout.go`
- Create: `packstore/layout_test.go`
- Create: `packstore/catalog_test.go`
- Create: `packstore/packstoretest/catalog.go`
- Create: `packstore/packstoretest/catalog_test.go`

- [ ] **Step 1: Write failing validation and fake-catalog tests**

  Cover lowercase SHA-256 validation, ULID pack validation, canonical
  `<aa>/<hash>` and `packs/<aa>/<id>.mvpack` paths, rejection of traversal and
  case aliases, duplicate canonical IDs, and construction of both staging
  layouts. Add a complete in-memory fake catalog used by later engine tests.

- [ ] **Step 2: Run the tests and observe undefined package types**

  Run: `go test ./packstore -run 'Test(Layout|Catalog)' -count=1`

  Expected: FAIL to compile because the package API does not exist.

- [ ] **Step 3: Implement the minimal public contracts**

  Define app-neutral equivalents of the approved semantic operations. Keep
  runtime reads narrow:

  ```go
  type Resolver interface {
      Resolve(context.Context, Hash) (Location, error)
  }

  type Location struct {
      Member bool
      Pack   *IndexEntry
  }
  ```

  Split maintenance capabilities into focused interfaces for inventory,
  packing/adoption, repack CAS, and unpack/reset, then compose them as
  `Catalog`. Do not expose SQL transactions or application row types. Define
  `Limits` with the compatibility defaults: 64 MiB blob, 128 MiB container,
  8 MiB footer, and 100,000 entries.

  Add `packstoretest.RunCatalogContract`, driven by a harness factory that can
  seed membership, mappings, aliases, and pack records and inspect committed
  state. Run the complete suite against the in-memory fake here. Msgvault will
  run the same suite against SQLite and PostgreSQL in Task 9. Tests importing
  `packstoretest` use external `packstore_test` packages so the helper's import
  of `packstore` cannot form a test import cycle.

- [ ] **Step 4: Pass focused tests and compile on Windows**

  Run:

  ```bash
  go test ./packstore/... -run 'Test(Layout|Catalog)' -count=1
  GOOS=windows go test -c -o /tmp/packstore-contract-windows.test ./packstore
  ```

  Expected: both exit 0.

- [ ] **Step 5: Commit**

  Commit in Kit with subject: `Define packed-CAS storage contracts`.

## Task 3: Add mutation/maintenance coordination

**Files:**

- Create: `packstore/coordinator.go`
- Create: `packstore/coordinator_test.go`

- [ ] **Step 1: Write concurrency tests first**

  Prove multiple mutation leases coexist, maintenance waits for every mutation,
  new mutations wait behind queued maintenance, cancellation removes a waiter,
  and a released lease cannot be released twice silently. Include the msgvault
  sequence: shared ingest lease releases, then exclusive automatic maintenance
  acquires while the outer application gate is conceptually still held.

- [ ] **Step 2: Verify red**

  Run: `go test -race ./packstore -run TestCoordinator -count=1`

  Expected: FAIL because `Coordinator` is not implemented.

- [ ] **Step 3: Implement leases without upgrade semantics**

  Use a context-aware condition/channel design. Expose explicit shared and
  exclusive acquisition. Document that callers acquire the application gate
  first, never upgrade, never recurse, and release shared publication before
  automatic maintenance. State explicitly that this is an in-process lease;
  application-owned daemon/write locks and live-daemon checks provide
  cross-process exclusion.

- [ ] **Step 4: Verify green under race detection**

  Run: `go test -race ./packstore -run TestCoordinator -count=1`

  Expected: PASS with no race report.

- [ ] **Step 5: Commit**

  Commit in Kit with subject: `Coordinate packed-CAS mutations and maintenance`.

## Task 4: Extract loose CAS operations with explicit policies

**Files:**

- Create: `packstore/loose.go`
- Create: `packstore/identity_unix.go`
- Create: `packstore/identity_windows.go`
- Create: `packstore/identity_other.go`
- Create: `packstore/loose_test.go`
- Create: `packstore/loose_unix_test.go`
- Create: `packstore/loose_windows_test.go`
- Reference: msgvault `internal/export/store_attachment.go`
- Reference: the downstream document archive's loose-CAS implementation.

- [ ] **Step 1: Port behavior tests before implementation**

  Cover streaming hash computation, expected hash/size mismatch, empty blobs,
  concurrent dedup, symlink/reparse rejection, stable identity replacement,
  atomic versus durable publication, full-hash versus size/type dedup, both
  staging layouts, temp cleanup, best-effort versus durable unlink, and a failed
  publication leaving only owned cleanup debris.

- [ ] **Step 2: Verify the ported tests fail**

  Run: `go test ./packstore -run 'TestLoose|TestWrite|TestRemove' -count=1`

  Expected: FAIL because loose operations are absent.

- [ ] **Step 3: Implement the smallest policy-explicit API**

  Use named option values rather than booleans, for example:

  ```go
  type Durability uint8
  const (
      AtomicPublication Durability = iota + 1
      DurablePublication
  )

  type DedupVerification uint8
  const (
      VerifyTypeAndSize DedupVerification = iota + 1
      VerifyFullHash
  )
  ```

  Require the caller to choose both. Authority-destroying maintenance ignores
  weaker options and always verifies/durably publishes. Reuse existing Kit
  path helpers only where their ownership and DACL behavior exactly matches the
  frozen msgvault tests.

- [ ] **Step 4: Verify native, race, and Windows compilation**

  Run:

  ```bash
  go test -race ./packstore -run 'TestLoose|TestWrite|TestRemove' -count=1
  GOOS=windows go test -c -o /tmp/packstore-loose-windows.test ./packstore
  ```

  Expected: both exit 0.

- [ ] **Step 5: Commit**

  Commit in Kit with subject: `Add policy-explicit loose CAS storage`.

## Task 5: Extract mixed reads, bounds, and reader retirement

**Files:**

- Create: `packstore/store.go`
- Create: `packstore/cache.go`
- Create: `packstore/preflight.go`
- Create: `packstore/store_test.go`
- Create: `packstore/preflight_test.go`
- Create: `packstore/cache_windows_test.go`
- Copy synthetic fixture to: `packstore/testdata/msgvault-v1/`
- Reference: msgvault `internal/blobstore/*`

- [ ] **Step 1: Port failing production-path tests**

  Cover member/non-member authority, loose and packed opens, loose-to-pack and
  pack-to-pack single retry, malformed pack IDs, cache eviction, close,
  retirement, concurrent ordinary/bounded reads, every footer/container limit,
  duplicate footer IDs, forged index metadata, CRC/decode/hash corruption, and
  the backup capture race.

- [ ] **Step 2: Verify red**

  Run: `go test -race ./packstore -run 'Test(Store|ReadBounded|Cache|Preflight)' -count=1`

  Expected: FAIL because mixed reads are absent.

- [ ] **Step 3: Implement by preserving the proven state machine**

  Move the stable plain-v1 maintenance parser rather than rewriting it around
  `pack.Reader.ReadBlob`, which is whole-buffer and has only the 4 GiB format
  bound. Keep ordinary and bounded readers in one 16-slot cache, with the mutex
  held across the short pread/decode operation exactly as required to prevent
  close-while-read.

- [ ] **Step 4: Add frozen fixture compatibility**

  Read the synthetic msgvault-v1 pack and manifest through Kit. Assert footer
  fields and content exactly. Do not regenerate the fixture in this test. The
  Kit and msgvault fixture directories are frozen copies and must be
  byte-identical:

  ```bash
  diff -r "$MSGVAULT_ROOT/internal/packcompat/testdata/msgvault-v1" \
    packstore/testdata/msgvault-v1
  ```

  Expected: no output. Any intentional fixture revision must be validated by
  the old msgvault reader first, then mirrored into both repositories in the
  same coordinated change.

- [ ] **Step 5: Verify green and commit**

  Run:

  ```bash
  go test -race ./packstore -run 'Test(Store|ReadBounded|Cache|Preflight|Frozen)' -count=1
  GOOS=windows go test -c -o /tmp/packstore-reader-windows.test ./packstore
  ```

  Expected: both exit 0. Commit in Kit with subject:
  `Read mixed loose and packed CAS content`.

## Task 6: Extract repair, reconciliation, packing, and sweeping

**Files:**

- Create: `packstore/pack.go`
- Create: `packstore/pack_test.go`
- Create: `packstore/repair_test.go`
- Reference: msgvault `internal/packer/packer.go`
- Reference: msgvault `internal/packer/loose_blob.go`

- [ ] **Step 1: Port the repair and packing matrix as failing tests**

  Include staging cleanup, malformed/missing records, stale unreferenced
  mappings, canonical-only orphan discovery, valid adoption, readable-copy
  repointing, all-or-nothing damaged referenced entries, unreadable orphan
  retention, dead oversized sibling behavior, duplicate normalized IDs,
  legacy aliases/paths, 64 MiB deferral, committed-only stats, cancellation at
  every phase, budget soft boundary, and verified loose sweep/recovery.

- [ ] **Step 2: Verify red**

  Run: `go test -race ./packstore -run 'Test(Pack|Repair|Reconcile|Sweep)' -count=1`

  Expected: FAIL because the maintainer path is absent.

- [ ] **Step 3: Implement the ordered repair pipeline**

  Preserve this order: staging cleanup, dangling record repair, stale mapping
  prune, referenced inventory normalization, orphan reconciliation, loose
  packing, indexed inventory, then one loose sweep. Correctness work is
  unbudgeted. Reject duplicate canonical IDs before commit or source deletion.

- [ ] **Step 4: Verify green, cancellation, and race coverage**

  Run: `go test -race ./packstore -run 'Test(Pack|Repair|Reconcile|Sweep)' -count=1`

  Expected: PASS with no race report.

- [ ] **Step 5: Commit**

  Commit in Kit with subject: `Pack and reconcile mixed CAS content`.

## Task 7: Extract unpack and source-isolated repack

**Files:**

- Create: `packstore/unpack.go`
- Create: `packstore/unpack_test.go`
- Create: `packstore/repack.go`
- Create: `packstore/repack_test.go`
- Reference: msgvault `internal/packer/unpack.go`
- Reference: msgvault `internal/repacker/repacker.go`

- [ ] **Step 1: Port failing unpack tests**

  Cover all-pack preflight before writes, stale mapping repair, every bound,
  durable empty and exact-64-MiB restores, cancellation, write/sync/metadata
  faults, mapping clear only after complete loose materialization, retirement,
  and retryable physical deletion.

- [ ] **Step 2: Port failing repack tests**

  Cover zero-live retirement first, sparse selection thresholds, oversized
  deferral, source independence, aggregated content failures, exact-set CAS
  loss, committed-only budget accounting, replacement orphan handling, cached
  reader retirement, and Windows sharing failures retaining a retryable orphan
  for the next repair pass after the catalog swap commits.

- [ ] **Step 3: Verify red**

  Run: `go test -race ./packstore -run 'Test(Unpack|Repack)' -count=1`

  Expected: FAIL because unpack/repack are absent.

- [ ] **Step 4: Implement without batching independent source packs**

  Use the production `Store.ReadBounded` path for every propagated blob.
  Preflight unpack globally, but repack each partial source in its own sealed
  replacement and catalog CAS. Retire zero-live sources before content reads.

- [ ] **Step 5: Verify and commit**

  Run:

  ```bash
  go test -race ./packstore -run 'Test(Unpack|Repack)' -count=1
  GOOS=windows go test -c -o /tmp/packstore-maint-windows.test ./packstore
  ```

  Expected: both exit 0. Commit in Kit with subject:
  `Unpack and reclaim packed CAS storage`.

## Task 8: Verify and publish the Kit extraction branch

**Files:**

- Modify: `packstore/doc.go` if tests reveal undocumented invariants.
- Modify: `.github/workflows/ci.yml` only if the new package is not already
  exercised on Windows.

- [ ] **Step 1: Run the complete Kit verification suite**

  Run:

  ```bash
  go test -race ./packstore
  go test ./...
  go vet ./...
  prek run
  ```

  Expected: every command exits 0.

- [ ] **Step 2: Run package tests on a real Windows CI runner**

  Push the Kit branch and confirm the normal CI workflow executes
  `go test ./...` on Windows, not merely cross-compilation. A skipped Windows
  job does not satisfy this gate.

- [ ] **Step 3: Run design and code review when explicitly requested**

  Use the repository's normal review workflow. Do not invoke roborev unless the
  user explicitly requests it.

- [ ] **Step 4: Fix findings and re-run the complete suite**

  Repeat Step 1 and Windows CI after every lifecycle or API correction.

- [ ] **Step 5: Record the Kit commit for msgvault**

  Push the reviewed head and record its full SHA. The msgvault branch pins this
  exact commit temporarily; do not use a local `replace` in committed files.
  Task 14 replaces the pseudo-version with the tagged Kit release.

## Task 9: Implement the msgvault catalog adapter

**Files:**

- Create: `internal/store/pack_catalog.go`
- Create: `internal/store/pack_catalog_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Reference without changing semantics: `internal/store/packs.go`
- Reference without changing semantics: `internal/store/repack.go`

- [ ] **Step 1: Pin the reviewed Kit head**

  Run: `go get go.kenn.io/kit@<full-reviewed-kit-sha>`

  Expected: `go.mod` records an interim pseudo-version and contains no
  `replace`. This temporary cross-repository development pin must be replaced
  by the tagged Kit release in Task 14.

- [ ] **Step 2: Write adapter conformance tests first**

  Reuse real SQLite and PostgreSQL store fixtures. Run Kit's
  `packstoretest.RunCatalogContract` against both adapters, then add
  msgvault-specific cases for case aliases, duplicate-per-message merge,
  liveness-aware resolution, adoption/repointing, stale prune, exact-set repack
  CAS, zero-live record retirement, and restored metadata reset.

- [ ] **Step 3: Verify red**

  Run:
  `go test -tags "fts5 sqlite_vec" ./internal/store -run TestPackCatalog -count=1`

  Expected: FAIL because the adapter does not exist.

- [ ] **Step 4: Implement thin conversions and interface assertions**

  Keep SQL in the existing store methods. The adapter converts between
  `store.PackIndexEntry`/`PackRecord` and `packstore` types and delegates atomic
  operations. Add compile-time assertions for the smallest interfaces it
  implements. Do not move msgvault normalization into Kit.

- [ ] **Step 5: Verify SQLite and PostgreSQL, then commit**

  Run the focused SQLite command and, with the existing test database,
  `make test-pg`. Expected: both pass. Commit with subject:
  `Adapt attachment pack metadata to Kit`.

## Task 10: Migrate msgvault reads and backup capture

**Files:**

- Modify: daemon/server dependency construction sites found by
  `rg 'blobstore.New|\*blobstore.Store' cmd internal`
- Modify: `internal/backupapp/content_source.go`
- Modify: affected API/MCP/export tests.
- Create: `internal/packcompat/cross_read_test.go`
- Create: `internal/packcompat/performance_test.go`

- [ ] **Step 1: Add compatibility and performance characterization before rewiring**

  Prove the old engine reads a Kit-written pack and Kit reads the frozen old
  fixture. Include loose-to-pack and pack-to-pack race retries and backup open
  between resolution and file open. Retain the backup round-trip assertion that
  a loose-only restore clears every production pack mapping and record through
  the catalog adapter. Add production-path benchmarks for loose reads, cold and
  warm packed reads, concurrent reads through the shared reader cache, and
  backup capture. The benchmark must read real loose files and pack containers;
  it must not benchmark only an interface stub.

- [ ] **Step 2: Establish green compatibility and measured performance baselines**

  Run the compatibility test, then collect at least ten samples of each
  benchmark from the pre-extraction engine with stable CPU/power settings:

  ```bash
  go test -tags "fts5 sqlite_vec" ./internal/packcompat -run TestCrossRead -count=1
  go test -tags "fts5 sqlite_vec" ./internal/packcompat -run '^$' -bench 'Benchmark(Loose|Packed|Concurrent|Backup)' -benchmem -count=10 > /tmp/msgvault-pack-before.txt
  ```

  Expected: compatibility passes through the direct test harness using the
  completed msgvault Catalog adapter, and the benchmark result is retained for
  `benchstat`. This is a refactor gate, not a synthetic red test.

- [ ] **Step 3: Replace production construction with `packstore.Store`**

  Preserve existing consumer interfaces such as `export.AttachmentOpener` and
  backup `ContentSource` through tiny msgvault adapters. Keep one daemon-owned
  store and close it at the existing lifecycle boundary.

- [ ] **Step 4: Verify focused and end-to-end reads**

  Run:

  ```bash
  go test -race -tags "fts5 sqlite_vec" ./internal/packcompat ./internal/backupapp ./internal/api ./internal/mcp ./internal/export
  go test -tags "fts5 sqlite_vec" ./internal/packcompat -run '^$' -bench 'Benchmark(Loose|Packed|Concurrent|Backup)' -benchmem -count=10 > /tmp/msgvault-pack-after.txt
  benchstat /tmp/msgvault-pack-before.txt /tmp/msgvault-pack-after.txt
  ```

  Expected: tests pass with both cross-read directions exercised. Investigate
  every statistically significant time/op or allocation regression. Do not
  accept a packed-read or concurrent-cache slowdown above 5%, an increase in
  steady-state allocations/op, or a backup-capture slowdown above 10% without
  a documented cause and explicit user approval. Keep catalog conversion and
  interface dispatch out of per-byte and decoded-frame hot loops.

- [ ] **Step 5: Commit**

  Commit with subject: `Serve attachment content through Kit packstore`.

## Task 11: Migrate msgvault loose writes without changing ingest durability

**Files:**

- Modify: `internal/export/store_attachment.go`
- Modify: `internal/export/store_attachment_test.go`
- Modify: `internal/export/store_attachment_from_path_test.go`
- Modify: `internal/export/store_attachment_windows_test.go`
- Preserve MIME-facing function signatures used by importers.

- [ ] **Step 1: Strengthen production-path characterization tests**

  Ordinary byte and path ingest must preserve atomic publication plus full-hash
  dedup. Unpack/restore/canonical legacy migration must preserve durable
  publication plus full-hash verification. Tests must exercise the production
  write path and actual fsync/identity seams, not inspect source text or merely
  assert which options a stub received.

- [ ] **Step 2: Establish the green write baseline**

  Run:
  `go test -tags "fts5 sqlite_vec" ./internal/export -run 'TestStoreAttachment(File|FromPath|Durable)' -count=1`

  Expected: PASS against the current implementation. Save the observed sync,
  dedup, identity, returned-path, and failure behavior as the refactor oracle.
  Also capture `-benchmem -count=10` production-path ingest benchmarks for byte
  and path inputs before changing the implementation.

- [ ] **Step 3: Replace physical implementation with Kit calls**

  Retain content-to-`mime.Attachment` mutation, empty-content compatibility,
  returned relative paths, and failed-store metadata contracts in msgvault.
  Delete only physical helpers whose tests now pass against Kit.

- [ ] **Step 4: Verify native and Windows CI behavior**

  Run focused tests locally and ensure the branch's Windows job executes the
  build-tagged identity/reparse suite. Compare ingest benchmarks or real
  fixture timing with `benchstat` before accepting any new per-file sync.
  Investigate every significant regression; do not accept a throughput drop
  above 5% or added steady-state allocations without explicit user approval.

- [ ] **Step 5: Commit**

  Commit with subject: `Write attachment blobs through Kit packstore`.

## Task 12: Migrate pack, unpack, repack, and scheduling

**Files:**

- Modify: `cmd/msgvault/cmd/attachment_maintenance.go`
- Modify: `cmd/msgvault/cmd/attachment_maintenance_test.go`
- Modify: explicit pack/unpack/repack command files and tests.
- Modify: account-removal maintenance wiring and tests.
- Keep current store SQL files and adapter.

- [ ] **Step 1: Strengthen maintenance characterization tests**

  Pin every existing log/stat field, warning, 256 MiB automatic budget, 03:17
  schedule, end-of-ingest allowlist, pack-before-repack order, cancellation,
  best-effort ingest follow-up, fail-fast explicit maintenance behavior, and
  the live-daemon preflight that protects unpack on every database backend.

- [ ] **Step 2: Establish the green maintenance baseline**

  Run:
  `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test.*Attachment.*Maintenance|Test.*PackAttachments|Test.*RepackAttachments|Test.*UnpackAttachments' -count=1`

  Expected: existing user-visible behavior passes before the engine switch.
  Add focused Kit event translation cases only where they drive the real
  production maintainer, never a stub whose sole purpose is recording options.
  Capture repeatable pack and repack timings, bytes/op, and allocations/op on a
  synthetic archive containing many small blobs plus representative 1-64 MiB
  blobs. Reuse the identical archive and settings after Step 3.

- [ ] **Step 3: Wire Kit Maintainer under the existing operation gate**

  Acquire locks in the documented order: daemon operation gate, then Kit
  lease. Release a shared ingest lease before acquiring the exclusive automatic
  maintenance lease. Do not recursively acquire either lock. Translate typed
  Kit outcomes into the existing msgvault logs and command stats.

- [ ] **Step 4: Verify maintenance and account removal**

  Run:

  ```bash
  go test -race -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/store
  ```

  Expected: PASS with the packed account-removal integration cases included.
  Compare the before/after maintenance measurements with `benchstat`; a pack or
  repack throughput regression above 10% requires investigation and explicit
  user approval. Coordinator acquisition must not appear in per-blob hot loops.

- [ ] **Step 5: Commit**

  Commit with subject: `Run attachment maintenance through Kit packstore`.

## Task 13: Remove the duplicate engine only after parity

**Files:**

- Delete: `internal/blobstore/`
- Delete: `internal/packer/`
- Delete: `internal/repacker/`
- Delete or reduce: `internal/packcompat/` temporary cross-implementation code.
- Modify: imports and docs references found by
  `rg 'internal/(blobstore|packer|repacker)'`.
- Keep frozen fixture tests through Kit and msgvault adapter paths.

- [ ] **Step 1: Run the full suite before deletion and save evidence**

  Run `make test`, `make test-pg`, and the focused cross-read suite. Expected:
  all pass. Do not delete the old engine without this green baseline.

- [ ] **Step 2: Delete old packages and resolve imports mechanically**

  Do not rewrite application SQL or command policy during deletion. Retain
  product-specific tests by moving them to the adapter/command package rather
  than discarding coverage.

- [ ] **Step 3: Prove no old engine references remain**

  Run: `rg 'internal/(blobstore|packer|repacker)' --glob '*.go' --glob '*.md'`

  Expected: no code references; historical design references may remain only
  when explicitly labeled as pre-extraction paths.

- [ ] **Step 4: Run full verification again**

  Run:

  ```bash
  make test
  make test-pg
  make lint-ci
  go vet ./...
  prek run
  ```

  Expected: every command exits 0.

- [ ] **Step 5: Commit**

  Commit with subject: `Remove the duplicated attachment pack engine`.

## Task 14: Execute the compatibility and real-archive gate

**Files:**

- Modify tests or documentation only if the gate exposes a missing invariant.
- Do not add real archive paths, hashes, account names, or measurements that can
  identify private data to committed fixtures.

- [ ] **Step 1: Confirm Linux, macOS, and real Windows CI**

  All normal jobs must execute and pass. Specifically inspect the Windows test
  log for packstore reader retirement, reparse identity, external open-handle
  deletion failure, and retry coverage. A compile-only result is insufficient.
  Run the packed-read, concurrent-cache, ingest, and maintenance benchmarks on
  the real Windows runner before and after the engine switch. Apply the same
  regression gates from Tasks 10-12; Windows performance is a first-class
  compatibility requirement.

- [ ] **Step 2: Harden a disposable archive copy**

  Use the user-level `kenn:isolate-prod` skill when available; otherwise perform
  its equivalent manual isolation checks before running any branch binary.
  Copy, never mutate, the production archive. Exercise:

  `pack -> API/MCP/export reads -> abrupt daemon termination -> repair -> backup -> restore -> verify -> unpack -> verify -> repack -> verify`.

  Time pack, API/export reads, backup, restore, unpack, and repack with both the
  pre-extraction and new binaries on identical disposable archive copies.
  Investigate material slowdowns even when microbenchmarks are green; do not
  proceed past the gate with an unexplained regression. Record only aggregate
  counts and timings that are safe to publish.

- [ ] **Step 3: Cross-open with the pre-extraction binary**

  On another disposable copy, use the last released/pre-extraction msgvault
  binary to read and unpack Kit-written packs. Then use the new binary to open
  the old binary's store. Expected: no conversion and identical logical hash
  inventory.

- [ ] **Step 4: Refresh design/status documentation**

  Update `docs/internal/kit-packstore-extraction-design.md` with actual Kit
  version/commit, compatibility evidence, and any accepted differences. Do not
  claim downstream adoption readiness if any Windows or real-archive gate is incomplete.

- [ ] **Step 5: Final review and PR ordering**

  Run the requested review workflow. The user lands Kit first and authorizes
  its normal release workflow. Update msgvault from the interim pseudo-version
  to that tagged release, re-run Task 13's complete verification plus Windows
  CI, and refresh the msgvault PR title/body after the final scope is known.
  The msgvault PR is not merge-ready while it references an untagged Kit commit.
  Do not merge either PR or cut a release without the user's authorization.

## Follow-on planning gates

After Task 14 passes, write two new plans rather than extending this one:

1. **Verified streaming pack I/O:** format-compatible `kit/pack` append/read,
   bounded memory, legacy oversized repack/unpack, and compatibility benchmarks.
2. **Downstream document archive adoption:** schema, catalog adapter, daemon wiring, explicit pack
   command, GC transition, backup/restore, and loose/packed hardening.

Both plans may begin after the msgvault gate is green. The downstream document archive may adopt mixed
storage while blobs above 64 MiB remain loose; streaming becomes a hard
prerequisite only for migrating those large objects or maintaining oversized
legacy packs. Direct pack-native restore remains a separate optimization plan
associated with msgvault issue #466.
