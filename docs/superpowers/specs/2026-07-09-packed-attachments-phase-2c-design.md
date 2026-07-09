# Packed Attachments Phase 2c: Automatic Maintenance, Logical GC, and Repack

**Date:** 2026-07-09

**Status:** Approved for implementation planning

**Parent design:** `docs/internal/packed-attachments-design.md`

**Delivery branch and PR:** `packed-attachments`, PR #464

**Restore optimization follow-up:** GitHub issue #466

## Summary

Phase 2c completes the packed-attachment lifecycle introduced in phase 2b. It
adds bounded automatic packing after ingest, makes `remove-account` logically
delete packed blobs without first unpacking the vault, and physically compacts
sparse immutable packs without racing the daemon's cached readers.

This work remains on the existing `packed-attachments` branch and PR so the
entire storage foundation can be proven end to end before it lands. The three
parts remain separate commits and review checkpoints. They are one delivery
because logical GC changes orphan-reconciliation semantics, while physical
repack depends on both that change and the daemon's production blob store.

The intended migration is deliberately uneventful. Existing loose vaults stay
readable, mixed loose/packed vaults are normal, no upgrade or daemon startup
performs a large conversion, and users can run `pack-attachments` once when
they want the full benefit immediately. Users who do nothing migrate gradually
through bounded background work.

## Goals

1. Make packed storage the normal steady state without a disruptive upgrade.
2. Bound automatic work so a small sync or import cannot unexpectedly trigger
   a multi-gigabyte migration.
3. Let account removal make unique packed blobs immediately unreachable while
   preserving blobs shared through either content or thumbnail references.
4. Reclaim dead pack space safely on Unix and Windows, including while the
   daemon has cached pack readers.
5. Preserve every phase-2b crash-recovery and backup invariant.
6. Prove the complete pack, read, logical-delete, repack, backup/restore, and
   unpack lifecycle on one branch.

## Non-goals

- Pack-native backup capture or restore. Compatible-pack restore is tracked by
  #466; phase 2c keeps the current correctness-first loose restore.
- At-rest encryption or a new pack format.
- Secure erasure guarantees. Logical GC makes a hash unreachable immediately;
  immutable dead bytes disappear when physical repack succeeds.
- A new user-facing maintenance configuration surface. Initial limits and the
  daemon schedule are conservative built-in policy; explicit commands remain
  available for operators who want immediate work.
- Repacking arbitrary files that are not recorded in `attachment_packs`.

## Existing foundations and constraints

- `internal/packer.Run` already performs staging cleanup, dangling-record
  repair, orphan reconciliation, loose packing, and verified loose sweeping in
  that order. `packer.Options` currently controls only target pack size.
- `internal/blobstore.Store.Open` looks up the pack index, holds its cache mutex
  across `pack.Reader.ReadBlob`, returns an in-memory reader, and retries the
  index once when a stale pack path returns `fs.ErrNotExist`.
- The daemon's operation gate serializes mutating CLI requests, scheduled
  syncs, maintenance, and backup freeze windows. A maintenance helper called
  from inside those paths must assume the gate is already held and must not
  reacquire it.
- Manual sync uses `storeAPIAdapter.RunCLISync`; most other ingest commands use
  `storeAPIAdapter.RunCLICommand`. Both execute subprocesses while the parent
  daemon continues to own the operation gate and the long-lived blob store.
- `RemoveSourceSerialized` already performs the active-sync check and source
  cascade in one exclusive transaction. Phase 2b temporarily refuses removal
  when that transaction finds a unique packed blob.
- `attachment_packs` describes physical immutable pack files. Its footer totals
  include dead entries. `attachment_pack_index` contains only live, readable
  mappings and is the authority for user-facing blob reads.
- Production and backup packs deliberately share the kit format and sharded
  layout, but current restore materializes loose canonical files and clears
  production pack metadata.

## Fixed policy

The first implementation uses constants rather than new configuration:

- Automatic pack budget: 256 MiB of raw blob bytes per run.
- Automatic repack budget: 256 MiB of live raw bytes selected per run.
- Daemon maintenance schedule: daily at `03:17` in the daemon's local time.
- Sparse-pack threshold: fewer than 50% of footer entries remain live.
- Sparse-pack hysteresis: pack is at least 24 hours old and has at least 8 MiB
  of dead stored bytes.
- Zero-live packs bypass age and dead-byte thresholds because they require no
  rewrite and otherwise a small dead pack could remain forever.

Both byte budgets are soft at one atomic boundary. Packing may exceed its
budget by one blob; repack may exceed its budget by one selected source pack.
This guarantees forward progress for a single large blob or pack while still
bounding ordinary automatic work. Explicit `pack-attachments` and
`repack-attachments` runs are unbounded.

## Architecture

### 1. Bounded packer runs

Add `MaxBytes int64` to `packer.Options`; zero means unlimited to preserve the
existing explicit-command behavior. Add `BudgetExhausted bool` to
`packer.Stats` for logging and tests.

`packLoose` counts verified raw bytes after each append. When the limit is met
or exceeded, it seals and commits the current writer even if the target pack is
not full, then stops enumerating new loose blobs. It never leaves a staging
writer merely because the budget expired. A run with no eligible bytes creates
no pack.

The budget applies only to new loose data. Staging cleanup, dangling-record
repair, orphan reconciliation, and the final verified sweep always run. Those
steps enforce correctness and cannot be skipped because a migration budget was
consumed. Existing context checks still let scheduled work yield between
recovery, blob, and sweep boundaries.

`pack-attachments` continues to call the packer with zero `MaxBytes`. Automatic
callers use 256 MiB and log the resulting stats at INFO. A canceled automatic
run is informational; any other maintenance error is a warning. Neither case
changes the exit status of an ingest operation that already succeeded.

### 2. Daemon-owned attachment maintenance

Create one daemon-owned attachment-maintenance coordinator in the command
package. It owns references to the already-open `store.Store`, the daemon's
shared `blobstore.Store`, and the attachments directory. Its pack and repack
methods require the caller to hold operation-gate coverage; they never acquire
the gate themselves.

Construct the shared blob store and coordinator before wiring the scheduler and
`storeAPIAdapter`. This lets every daemon path use the same cached-reader
lifecycle. The coordinator is invoked from these existing gated paths:

| Trigger | Automatic action | Failure behavior |
|---|---|---|
| Successful manual `sync` / `sync-full` in `RunCLISync` | Bounded pack | Stream warning and keep sync success |
| Successful attachment-producing command in `RunCLICommand` | Bounded pack | Stream warning and keep command success |
| Successful scheduled Gmail, IMAP, Teams, or SyncTech ingest | Bounded pack | Log warning and keep ingest success |
| Daily `attachment-maintenance` scheduler job | Bounded pack, then bounded repack | Record job error; retry next schedule |
| Successful `remove-account` | Bounded repack | Stream warning and keep removal success |
| Explicit `repack-attachments` | Unbounded repack | Return failure to the command |

The generic command predicate is explicit and test-pinned. It includes the
commands that can write attachment content: account/source setup commands that
may perform initial ingest, `backfill-teams-media`, every local attachment
import command, `sync-synctech-sms`, and `sync-teams`. Calendar-only sync is
excluded. The dedicated `RunCLISync` path covers `sync` and `sync-full`.
Redundant no-op packing after a setup command that wrote no attachment is
acceptable; missing a writer is not. The daily job is the backstop for future
or overlooked writers.

The daily job is registered with `scheduler.AddJob`, so the existing work
tracker gives it operation-gate serialization, shutdown cancellation, and
yield-to-interactive behavior. It runs packing first because packing repairs
recoverable metadata before repack reads it. A fatal pack/reconciliation error
stops that day's repack; the job status records the error.

`repack-attachments` must execute in the parent daemon rather than an ordinary
CLI subprocess. The parent owns the long-lived reader cache that must be
retired before Windows can delete old packs. `storeAPIAdapter.RunCLICommand`
recognizes this command and calls the coordinator directly while the existing
CLI-run middleware holds the gate. It emits normal CLI stream events without
spawning a child. This avoids a second maintenance API surface while preserving
the standard daemon-backed command UX.

### 3. Reference-aware orphan reconciliation

Logical GC invalidates phase 2b's assumption that every valid entry found in an
orphan pack should be indexed. An old pack can become orphaned after its source
rows and live index mappings were intentionally deleted. Re-adopting such an
entry would resurrect deleted content.

Add a store query that reports whether a hash is still referenced by any
`attachments.content_hash` or `attachments.thumbnail_hash` row. During orphan
reconciliation:

1. An unreferenced footer entry is dead and is never adopted.
2. A referenced entry with a readable current index mapping is redundant.
3. A referenced entry with no mapping, or an unreadable current mapping, is
   hash-verified from the orphan and adopted/repointed as today.
4. Verification failure for a referenced adoptable entry preserves the orphan
   pack for recovery.
5. A pack whose entries are all unreferenced or safely readable elsewhere can
   be deleted as fully redundant. Unreferenced entries do not need byte
   verification merely to authorize deletion because the database has no live
   reference to preserve.

This rule covers regular packer crashes, repack crashes, and the sequence where
an account is removed after a new pack was sealed but before that pack was
recorded.

### 4. Logical packed-blob GC during account removal

Replace the temporary `UniquePackedBlobsError` refusal with transactional
logical GC inside `RemoveSourceSerialized`:

1. Under the existing exclusive transaction, collect distinct packed hashes
   referenced by the selected source and by no other source. Sharing is checked
   across both content and thumbnail columns.
2. Perform the existing FTS cleanup and source cascade.
3. Delete `attachment_pack_index` rows for the collected hashes in the same
   transaction.
4. Commit all three effects together.

The pack records and pack files are deliberately unchanged in this
transaction. Once an index row is removed, normal blob-store reads by hash no
longer reach those bytes. A transaction failure rolls back the account cascade
and every index deletion together.

Return the active-sync result plus the number of packed mappings removed so the
command can report useful maintenance context. The existing post-transaction
loose content/thumbnail cleanup remains unchanged. A best-effort bounded repack
runs in the parent daemon after the successful command; failure can delay disk
reclamation but cannot make the deleted blobs live again.

### 5. Repacker and store transaction

Add `internal/repacker` with a `Run` function, options, and stats parallel to
the packer. The caller supplies the production store, the shared blob store,
the attachments directory, and an optional raw-byte budget.

The store exposes pack accounting that left-joins `attachment_packs` to live
index rows and returns immutable totals, live entry count, live stored bytes,
and live raw bytes. Repacker selection is deterministic by creation time then
pack ID:

- Select every zero-live pack, regardless of age or size.
- Select partially live packs only when all sparse-pack thresholds pass.
- Stop adding source packs when the automatic raw-byte budget is reached,
  except that at least one eligible source pack is selected.

For partially live packs, enumerate only their current index rows and read each
blob through the production blob store. Append verified bytes to new kit pack
writers, combining live entries from multiple sparse source packs into normal
target-sized packs. Seal all new packs before changing the database. A read,
append, or seal failure aborts the active writer and leaves every old mapping
and file authoritative. Any already-sealed new files are safe orphans for the
reference-aware reconciler.

Commit the repack in one store transaction:

1. Insert records for every new sealed pack.
2. Compare-and-swap every live blob mapping from its expected old pack to its
   new pack entry, requiring exactly one affected row per blob.
3. Verify that the expected live rows from the selected old packs were neither
   omitted nor added.
4. Commit the new mappings while retaining old `attachment_packs` records.

Retaining the old records until physical deletion makes `attachment_packs` a
truthful inventory and prevents a crash from turning intentionally dead old
files into adoptable orphans. After the swap, each old record has zero live
index rows.

For a zero-live pack, no new pack or index transaction is needed. It proceeds
directly to reader retirement and physical deletion.

### 6. Reader retirement and physical deletion

Add a blob-store operation that validates a pack ID and, while holding the
same mutex used for packed reads:

1. Removes the pack from the cache and FIFO order.
2. Closes its cached `pack.Reader`, if present.
3. Deletes the canonical sharded pack file before releasing the mutex.

`fs.ErrNotExist` is success. Any close or other remove error is returned and
the old pack record remains for retry.

Holding the mutex through deletion provides the required ordering:

- A read that already holds the mutex finishes and returns verified in-memory
  bytes before retirement closes the reader.
- A read that loaded an old index row but reaches the mutex after deletion gets
  `fs.ErrNotExist`; `Store.Open` performs its existing one-time index retry and
  finds the new mapping.
- Windows sees every daemon-owned handle closed before `os.Remove`, avoiding
  its open-file deletion failure. Unix follows the same deterministic path.

After successful deletion, remove the old `attachment_packs` row with a guarded
store method that succeeds only when no live index row names that pack. If the
process crashes after file deletion but before record deletion, the packer's
existing dangling-record repair removes the now-missing physical record. If
deletion fails, the zero-live record and file remain and a later maintenance
run retries them.

## Failure and crash matrix

| Boundary | Durable state | Recovery |
|---|---|---|
| Automatic budget reached | Current new pack sealed and indexed | Run ends normally; later run continues remaining loose blobs |
| Regular pack sealed before index commit | Referenced orphan new pack | Reconcile adopts only still-referenced entries |
| Source removal transaction fails | Source and all pack mappings unchanged | Retry removal |
| Source removal commits | Unique hashes unindexed; old pack record/file retained | Data is logically dead; repack later reclaims bytes |
| Repack read/write fails before database swap | Old mappings/files unchanged; new staging aborted | Retry; any sealed new pack is reconciled safely |
| New repack files seal before swap, then crash | New orphan files; old mappings authoritative | Referenced entries are redundant or recoverable; dead entries are never adopted |
| Repack index swap commits before old deletion | New mappings live; old records/files zero-live | Next repack retries retirement/deletion |
| Old file deletes before old record cleanup | Zero-live record points to missing file | Dangling-record repair removes record |
| Old file deletion fails | New mappings remain live; zero-live old record/file retained | Report error and retry later |
| Reader fetched old index before swap | It either finishes before retirement or gets `ENOENT` afterward | Existing index retry opens the new pack |
| Backup restore | Canonical loose files; pack metadata cleared | Automatic packing resumes gradually or explicit packing migrates immediately |

## User experience and migration

- Schema initialization remains metadata-only; it never starts a blob
  migration.
- Mixed storage remains a first-class state indefinitely.
- Successful automatic maintenance is quiet on normal CLI output and visible
  in structured INFO logs. Failures produce a concise warning with the
  explicit maintenance command to retry.
- `pack-attachments` remains the recommended one-time action for an existing
  user who wants immediate file-count and Windows performance benefits.
- Automatic packing and repacking never require a daemon restart.
- `unpack-attachments` remains the explicit downgrade/recovery escape hatch
  and retains its live-daemon preflight.
- Loose-to-pack migration publishes and deletes one pack at a time, so peak
  extra space is near one pack. Repack temporarily retains both sides of its
  bounded batch, so automatic peak extra space is bounded near 256 MiB plus a
  possible single oversized source pack.
- Current backup restore stays fully loose and correct. Pack-native restore is
  sequenced after phase 2c because it should consume this finalized pack/index
  and dead-entry contract; see #466.

## Testing and verification

All new or modified Go tests use testify and the repository's required build
tags.

### Packer and scheduling

- Budget below, exactly at, and above a pack boundary.
- One blob larger than the budget still makes progress.
- Budget exhaustion seals the current writer and leaves no staging file.
- Explicit zero-budget option remains unlimited.
- Recovery and verified sweep still run after the packing budget is exhausted.
- Manual sync, generic ingest, scheduled provider ingest, and the daily job
  each trigger exactly one bounded pack attempt only after successful ingest.
- Maintenance failure warns but does not replace a successful ingest result.
- Scheduled maintenance yields to an interactive operation and stops cleanly
  on daemon shutdown.

### Logical GC and reconciliation

- Unique packed content and thumbnails lose their index rows in the source
  removal transaction.
- Sharing through content/content, thumbnail/thumbnail, and cross-column
  content/thumbnail references preserves the mapping.
- A forced transaction error rolls back the source and all mappings.
- An orphan pack entry with no remaining attachment reference is never adopted.
- A crash-style orphan containing a mix of referenced and deleted entries
  adopts only the referenced entries.
- Removing a source after seal-before-index does not resurrect its blobs.

### Repack and reader retirement

- Eligibility boundaries for live fraction, age, dead bytes, and zero-live
  exceptions.
- Deterministic bounded selection and multi-source-pack compaction.
- Byte-identical live blobs and unchanged user-facing reads after swap.
- Compare-and-swap mismatch rolls back every new mapping.
- Fault injection after seal, during the index transaction, after swap, during
  old-file deletion, and before old-record cleanup.
- A read already in progress completes before retirement; a stale queued read
  retries to the new pack.
- Cached handles are closed before deletion. The normal Windows CI job must run
  this test so Windows' stricter file semantics validate the real ordering.
- Race runs cover the blob-store cache and repacker coordination.

### End to end

- Pack loose content and thumbnails; read them through API, MCP, directory/zip
  export, and backup capture.
- Remove one account from mixed shared/unique packed data; verify deleted hashes
  are unreachable and shared hashes remain readable.
- Repack; verify dead files/records are reclaimed without changing live bytes.
- Create and restore a backup; verify the restored vault is fully loose and
  readable, then pack it again.
- Unpack the repacked vault and compare canonical loose bytes and empty pack
  metadata.
- Run the lifecycle on SQLite and PostgreSQL where backend behavior differs.
- Run `make test`, `go vet ./...`, `make lint-ci`, and targeted race tests at
  each checkpoint; request roborev at each checkpoint and an exact-head
  whole-branch review after all three.

After automated verification passes, a separate explicit user approval gates
the hardening run on a copy of the real `~/.msgvault`. The live archive is never
the test target. That run measures file count and elapsed time and covers pack,
API/MCP/export reads, crash injection, backup round-trip, repack, and unpack.

## Delivery checkpoints on PR #464

1. **Bound automatic packing.** Add the packer budget, daemon coordinator,
   post-ingest hooks, and daily pack job. Verify and review before continuing.
2. **Make deletion logical.** Make orphan reconciliation reference-aware,
   and replace the account-removal refusal with transactional index GC. Verify
   and review before continuing; physical cleanup remains safely deferred.
3. **Repack physically.** Add accounting, repacker transactions, daemon-native
   `repack-attachments`, shared-reader retirement, scheduled repack, and
   physical cleanup. Run full end-to-end and whole-branch review.

The final PR title and body must be refreshed again after checkpoint 3 so the
published scope describes the complete lifecycle rather than phase 2b alone.

## Acceptance criteria

Phase 2c is complete when:

1. Existing loose vaults upgrade without startup migration and remain readable
   throughout bounded automatic conversion.
2. Explicit packing still migrates the entire eligible backlog in one resumable
   run.
3. Account removal never requires unpacking, never removes shared mappings, and
   never lets an unreferenced orphan pack resurrect deleted hashes.
4. Eligible dead space is compacted into new immutable packs, and zero-live
   packs are eventually removed.
5. Concurrent reads survive repack on Unix and Windows using the production
   cache and retry path.
6. Every crash boundary leaves either the old or new verified mapping
   authoritative, with deterministic cleanup on a later run.
7. Backup restore remains a fully loose, readable vault that can be repacked.
8. Automated, backend, Windows, race, crash-injection, and exact-head review
   gates pass before real-vault hardening is proposed.
