# Packed Attachments Phase 2c: Automatic Maintenance, Logical GC, and Repack

**Date:** 2026-07-09

**Status:** Implemented on the delivery branch; exact-head review and CI pending on PR #464

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
- `internal/blobstore.Store.Open` currently looks up only the pack index before
  choosing packed or canonical loose storage. It holds its cache mutex across
  `pack.Reader.ReadBlob`, returns an in-memory reader, and retries the index once
  when a stale pack path returns `fs.ErrNotExist`. Phase 2c must add attachment
  liveness to that resolution so an unreferenced loose crash leftover cannot
  bypass logical GC.
- The daemon's operation gate serializes mutating CLI requests, scheduled
  syncs, maintenance, and backup freeze windows. A maintenance helper called
  from inside those paths must assume the gate is already held and must not
  reacquire it. Kit releases the gate after pinning the backup's database
  snapshot, before attachment capture, so the backup content reader can overlap
  later maintenance.
- Manual sync uses `storeAPIAdapter.RunCLISync`; most other ingest commands use
  `storeAPIAdapter.RunCLICommand`. Both execute subprocesses while the parent
  daemon continues to own the operation gate and the long-lived blob store.
- `RemoveSourceSerialized` already performs the active-sync check and source
  cascade in one exclusive transaction. Phase 2b temporarily refuses removal
  when that transaction finds a unique packed blob.
- `attachment_packs` describes physical immutable pack files. Its footer totals
  include dead entries. `attachment_pack_index` contains storage mappings, but
  today a mapping can outlive its last attachment row: Teams inline replacement,
  permanent message deletion, and generic cascades delete attachment rows
  without updating the pack index. Attachment rows, not index rows alone, are
  the liveness authority.
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
bounding the payload bytes rewritten by ordinary automatic work. This is not a
wall-clock or total-row-scan bound: reference repair and pack-usage accounting
still inspect the archive inventory, and every zero-live pack is selected even
after the rewrite budget because it needs no blob rewrite. Explicit
`pack-attachments` and `repack-attachments` runs are unbounded.

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
repair, reference repair, orphan reconciliation, and the final loose-file sweep
always run. Those steps enforce correctness and cannot be skipped because a
migration budget was consumed. Existing context checks still let scheduled work
yield between recovery, blob, and sweep boundaries.

Add one store repair primitive that deletes `attachment_pack_index` rows whose
hash is absent from both attachment hash columns. It runs under the caller's
existing exclusive coverage and reports the deleted row count. Call it from
`packer.Run` before orphan reconciliation, from `repacker.Run` before
accounting, and from `Unpack` before enumerating rows to restore. This makes
explicit repack and downgrade safe even when no packer run preceded them, while
the daily pipeline's second call is normally a no-op.

Extend the packer's existing loose-tree walk rather than adding a second scan.
Load the referenced-hash and indexed-hash sets once, skip the `packs/` subtree,
and classify each regular hash-named file:

1. If its hash is unreferenced, delete it as a loose orphan without treating its
   bytes as recoverable. A failure is warned and retried by the next packer run.
2. If it is referenced and indexed, retain the existing verify-packed-copy,
   recover-from-verified-loose, or sweep behavior.
3. If it is referenced and unindexed, leave it as live loose content.

This covers canonical and legacy noncanonical paths because both use the hash as
their basename. Files that are not named as valid content hashes and everything
inside `packs/` are outside this cleanup. Add stats for pruned mappings,
unreferenced loose files removed, quarantined orphan packs, and unreadable
orphan packs.

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

The generic command predicate is explicit and test-pinned. Its exact allowlist
is `backfill-teams-media`; `import`, `import-emlx`, `import-gvoice`,
`import-imessage`, `import-mbox`, `import-messenger`, `import-pst`,
`import-synctech-sms`, and `import-whatsapp`; plus `sync-synctech-sms` and
`sync-teams`. Calendar-only sync and account/source setup commands are
excluded because the current setup paths do not ingest attachment bytes. The
dedicated `RunCLISync` path covers `sync` and `sync-full`. If a future setup
path begins ingesting attachments, its command and the pinned allowlist test
must change together; the daily job remains the operational backstop for an
overlooked writer.

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
the standard daemon-backed command UX. Add `repack-attachments` to
`cliRunCommandAllowed` and pin that admission in the CLI-handler tests; the
handler currently rejects an unlisted command before the adapter can dispatch
it.

### 3. Reference-aware blob resolution and orphan reconciliation

An attachment row is the durable liveness authority for a content hash. Add a
store resolver that returns, in one indexed SQL round trip, both whether the
hash appears in any `attachments.content_hash` or
`attachments.thumbnail_hash` row and its optional pack-index entry. The query
uses the existing content-hash and thumbnail-hash indexes. Keep direct pack
index access for maintenance code that deliberately examines metadata without
asserting liveness.

Change the production blob store to use this resolver on its initial lookup and
on both race retries:

1. If the hash is not referenced, return `fs.ErrNotExist` without opening a
   pack or canonical loose file.
2. If referenced with an index entry, use packed storage as today.
3. If referenced without an index entry, use canonical loose storage as today.

This does not add a database round trip to normal reads; it replaces the
existing pack-index lookup with the combined resolver. A read whose liveness
lookup began before account removal may finish, just like any already-started
read. A lookup beginning after the removal transaction commits cannot serve a
stale pack mapping or a loose crash leftover. Best-effort loose-file deletion
can therefore fail without making logically deleted content addressable by the
API's hash endpoint.

Logical GC invalidates phase 2b's assumption that every valid entry found in an
orphan pack should be indexed. An old pack can become orphaned after its source
rows and live index mappings were intentionally deleted. Re-adopting such an
entry would resurrect deleted content.

During orphan reconciliation, use the same attachment-row liveness authority
(through a focused store method or a batched equivalent):

1. An unreferenced footer entry is dead and is never adopted.
2. A referenced entry with a readable current index mapping is redundant.
3. A referenced entry with no mapping, or an unreadable current mapping, is
   hash-verified from the orphan and adopted/repointed as today.
4. Verification failure for any referenced adoptable entry makes reconciliation
   all-or-nothing for that pack: record no pack row, adopt/repoint no entries,
   and do not delete the file. Valid entries remain safely quarantined in the
   unrecorded pack until every referenced recovery candidate verifies or loses
   its attachment reference.
5. A pack whose entries are all unreferenced or safely readable elsewhere can
   be deleted as fully redundant. Unreferenced entries do not need byte
   verification merely to authorize deletion because the database has no live
   reference to preserve.

This rule covers regular packer crashes, repack crashes, and the sequence where
an account is removed after a new pack was sealed but before that pack was
recorded.

Quarantine deliberately favors preservation over availability. A valid
referenced entry with no other readable mapping remains unavailable when a
different referenced recovery candidate in the same orphan fails; partially
recording the pack would hide the failed entry from reconciliation and let a
later repack discard its only remaining bytes. Increment a distinct
`PacksQuarantined` stat and emit one ERROR summary containing the pack ID,
failed referenced-entry count, and withheld adoptable-entry count. Explicit
`pack-attachments` output reports the quarantine count. Automatic maintenance
retries and re-verifies the referenced candidates on every run until the pack
can be adopted, becomes redundant, or the failed reference is removed.

An orphan whose footer cannot be opened is a separate condition because its
entries and reference counts are unknown. Preserve and retry it, increment
`PacksUnreadable`, and emit a distinct ERROR containing the pack ID, path, and
open error. Do not also count it as `PacksQuarantined`. Explicit
`pack-attachments` output reports both counts, so an operator can distinguish
an unreadable pack container from a readable pack with damaged referenced
entries.

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
transaction. Once the attachment rows and index mapping disappear together,
the production resolver's liveness check prevents both packed and loose
fallback reads by hash. A transaction failure rolls back the account cascade
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

For all repack queries, "live" means an index row whose hash is still present in
at least one attachment hash column. After running the shared reference repair,
the store exposes pack accounting that left-joins `attachment_packs` to those
referenced index rows and returns immutable totals, live entry count, live
stored bytes, and live raw bytes. Repacker selection is deterministic by
creation time then pack ID:

Reference repair, usage accounting, referenced-entry enumeration, the CAS,
and guarded old-record deletion are context-aware store maintenance
operations. They run with PostgreSQL's transaction-local
`statement_timeout` disabled so explicit and scheduled repack do not wedge on
archive-scale metadata scans, while caller cancellation still interrupts the
work.

- Select every zero-live pack, regardless of age or size.
- Select partially live packs only when all sparse-pack thresholds pass.
- Stop adding source packs when the automatic raw-byte budget is reached,
  except that at least one eligible source pack is selected.

For partially live packs, enumerate only referenced index rows and read each
blob through the production blob store. This is not a CRC-only copy: kit
`ReadBlob` verifies stored CRC, decoding, and SHA-256 against the requested blob
ID before returning bytes; `pack.Writer.Append` independently recomputes the new
entry ID, and the repacker requires that returned ID to equal the expected hash.
Append those verified bytes to new kit pack writers, combining live entries
from multiple sparse source packs into normal target-sized packs. Seal all new
packs before changing the database. A read, hash, append, or seal failure aborts
the active writer and leaves every old mapping and file authoritative. Any
already-sealed new files are safe orphans for the reference-aware reconciler.

Commit the repack in one store transaction:

1. Insert records for every new sealed pack.
2. Compare-and-swap every live blob mapping from its expected old pack to its
   new pack entry, requiring exactly one affected row per blob.
3. Verify that the expected referenced index rows from the selected old packs
   were neither omitted nor added. Unreferenced rows pruned during reference
   repair are not part of the compare-and-swap set.
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

TUI, MCP, and CLI exports use the daemon's shared cache even with `--local`.
Backup is the supported exception: it creates an independent short-lived blob
store, and kit releases the operation gate after pinning the database snapshot
but before attachment capture. Backup can therefore overlap repack, and daemon
reader retirement cannot close its cached pack handles.

That boundary remains conservative. On Windows, a backup-held handle can make
old-pack deletion fail after the mapping swap; the repacker reports the error
and retains the zero-live file and record until a later run after backup closes.
On Unix, deletion can unlink the old path while the backup's already-open handle
remains usable; a new lookup sees the replacement mapping. Repack never omits
or mutates live bytes, so capture either completes with verified content or
fails loudly and can be retried. The existing backup limitation also remains:
if a concurrent logical deletion removes a reference from the live database
after the snapshot was pinned, the reference-aware content source may reject
that snapshot-only hash and the backup fails loudly rather than silently
omitting it. Any future independent pack reader must obey the same retryable
contract.

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
| Attachment replacement/deletion removes the last reference without updating its pack mapping | Stale index row and possibly a loose file remain | Resolver returns not-found; the next packer/repacker repair prunes the mapping, and the packer sweep retries loose deletion |
| Source removal transaction fails | Source and all pack mappings unchanged | Retry removal |
| Source removal commits | Unique hashes unreferenced and unindexed; old pack record/file and possible loose leftovers retained | Resolver returns not-found; repack reclaims pack bytes and the next packer sweep retries loose deletion |
| Referenced orphan candidate fails verification beside valid adoptable entries | Entire orphan remains unrecorded and undeleted | Distinct quarantine stat/error names pack; every maintenance run retries verification |
| Orphan footer cannot be opened | Pack remains unrecorded and undeleted; entry liveness is unknown | Distinct unreadable-pack stat/error names pack and path; every maintenance run retries opening it |
| Repack read/write fails before database swap | Old mappings/files unchanged; new staging aborted | Retry; any sealed new pack is reconciled safely |
| New repack files seal before swap, then crash | New orphan files; old mappings authoritative | Referenced entries are redundant or recoverable; dead entries are never adopted |
| Repack index swap commits before old deletion | New mappings live; old records/files zero-live | Next repack retries retirement/deletion |
| Old file deletes before old record cleanup | Zero-live record points to missing file | Dangling-record repair removes record |
| Old file deletion fails | New mappings remain live; zero-live old record/file retained | Report error and retry later |
| Reader fetched old index before swap | It either finishes before retirement or gets `ENOENT` afterward | Existing index retry opens the new pack |
| Backup overlaps repack and holds an old pack handle | New mappings are authoritative; Windows may retain the zero-live old file/record, while Unix may unlink it beneath the open handle | Capture uses its open handle or the new mapping; a loud capture/delete failure is retryable, and later repack removes retained files after backup closes |
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
- Reference repair prunes mappings left stale by attachment replacement,
  permanent message deletion, and cascades.
- The loose-tree sweep deletes unreferenced canonical and noncanonical files,
  retries deletion failures on later runs, and preserves referenced loose files.
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
- The production resolver rejects unreferenced hashes even when a canonical
  loose file or stale pack mapping remains, without adding a second read query.
- An orphan pack entry with no remaining attachment reference is never adopted.
- A crash-style orphan containing a mix of referenced and deleted entries
  adopts only the referenced entries.
- A mixed orphan with one valid adoptable entry and one referenced verification
  failure remains wholly unrecorded and undeleted, increments the quarantine
  stat, and logs the pack ID and both entry counts.
- An orphan whose footer cannot open remains wholly unrecorded and undeleted,
  increments only the unreadable-pack stat, and logs its pack ID, path, and
  error.
- Removing a source after seal-before-index does not resurrect its blobs.
- Pack Teams inline media, replace the inline attachment set so the old hash
  loses its final row, then verify repair prunes its mapping and repack proceeds
  without treating it as live.
- The same stale mapping is not restored by `unpack-attachments`; a pack made
  zero-live by repair is dropped without opening its dead entry.

### Repack and reader retirement

- Eligibility boundaries for live fraction, age, dead bytes, and zero-live
  exceptions.
- Deterministic bounded selection and multi-source-pack compaction.
- Accounting, enumeration, and compare-and-swap use referenced index rows, not
  stale index rows alone; direct repack runs reference repair first.
- Byte-identical live blobs and unchanged user-facing reads after swap.
- Compare-and-swap mismatch rolls back every new mapping.
- Fault injection after seal, during the index transaction, after swap, during
  old-file deletion, and before old-record cleanup.
- A read already in progress completes before retirement; a stale queued read
  retries to the new pack.
- Cached handles are closed before deletion. The normal Windows CI job must run
  this test so Windows' stricter file semantics validate the real ordering.
- Race runs cover the blob-store cache and repacker coordination.
- A backup `ContentSource` backed by its independent blob-store cache overlaps a
  mapping swap. Capture must either return verified bytes or a loud retryable
  error, never omit content. On Windows, hold its old-pack handle across
  retirement and verify deletion fails with the zero-live file/record intact;
  after closing the backup store, the next repack removes both. On Unix, verify
  an already-open handle remains usable after unlink while new opens follow the
  new mapping.

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
2. **Make deletion logical.** Make production blob resolution and orphan
   reconciliation reference-aware, add shared stale-mapping repair and
   retryable unreferenced-loose cleanup, then replace the account-removal
   refusal with transactional index GC. Verify and review before continuing;
   physical pack cleanup remains safely deferred.
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
   makes an unreferenced hash unreadable through packed or loose fallback while
   never letting an orphan pack resurrect it.
4. Attachment replacement and other non-account deletion paths cannot leave a
   stale mapping counted as live or permanently wedge repack; unreferenced loose
   cleanup failures are retried.
5. Eligible dead space is compacted into new immutable packs, and zero-live
   packs are eventually removed.
6. Concurrent reads survive repack on Unix and Windows using both the daemon
   cache and backup's independent cache; external-handle deletion failures
   retain a truthful zero-live file/record and succeed on a later retry.
7. Every crash boundary leaves either the old or new verified mapping
   authoritative, with deterministic cleanup on a later run.
8. Quarantined damaged-entry orphans and unreadable-footer orphans are
   preserved, distinctly observable, and retried without allowing partial
   adoption to hide failed referenced entries.
9. Backup restore remains a fully loose, readable vault that can be repacked.
10. Automated, backend, Windows, race, crash-injection, and exact-head review
   gates pass before real-vault hardening is proposed.
