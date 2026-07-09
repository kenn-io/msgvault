# Packed Attachments Phase 2c Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the packed-attachment lifecycle with bounded automatic packing, reference-aware logical deletion, and crash-safe physical repacking that works with daemon and backup readers on Windows and Unix.

**Architecture:** Keep loose writes and mixed-storage reads unchanged while adding a daemon-owned maintenance coordinator. Attachment rows become the liveness authority; the pack index remains only a physical location map. Repack writes and verifies new immutable packs, atomically compare-and-swaps live mappings, then retires old readers/files while retaining truthful zero-live records whenever deletion must be retried.

**Tech Stack:** Go 1.26, SQLite/PostgreSQL store dialects, `go.kenn.io/kit/pack`, Cobra, daemon NDJSON CLI proxy, cron scheduler, testify, roborev.

---

## Required workflow and safety

- Apply `@superpowers:test-driven-development` to every behavior change: add one focused test, run it and observe the expected failure, implement the minimum production change, and rerun it green.
- Apply `@kenn:isolate-prod` throughout. Tests use `t.TempDir`; branch binaries go under a fresh temporary directory with `MSGVAULT_HOME` pointing at scratch state. Never run this branch against `~/.msgvault`, never use `make install`, and do not start it against the live daemon.
- Every Go test command includes `-tags "fts5 sqlite_vec"`; `make test` supplies those tags automatically.
- All new or changed tests use testify with `(want, got)` order. Do not add `t.Error`, `t.Fatal`, or tautological command-stub tests.
- Apply `@kenn:commit` at each checkpoint commit and `@kenn:scrub-private-data` before every push.
- The three implementation checkpoints remain separate commits on `packed-attachments`. Do not split this work into another branch or PR.
- Do not begin the real-vault hardening run. That remains separately gated by explicit user approval after automated verification.

## File structure

### New files

- `cmd/msgvault/cmd/attachment_maintenance.go` — daemon-owned pack/repack coordinator, automatic budgets, command classification, warning/log formatting, and scheduler registration.
- `cmd/msgvault/cmd/attachment_maintenance_test.go` — real temp-store tests for automatic hooks, daily scheduling, cancellation, and warning policy.
- `cmd/msgvault/cmd/repack_attachments.go` — always-daemon-backed explicit repack command and human-readable stats.
- `cmd/msgvault/cmd/repack_attachments_test.go` — proxy/admission and parent-daemon execution tests.
- `internal/store/repack.go` — pack usage accounting, referenced-entry enumeration, transactional compare-and-swap, and guarded record deletion.
- `internal/store/repack_test.go` — backend-portable accounting and transaction tests.
- `internal/repacker/repacker.go` — deterministic selection, verified rewrite, atomic swap, reader retirement, and physical cleanup.
- `internal/repacker/repacker_test.go` — selection, rewrite, crash-boundary, budget, and cleanup-retry tests.

### Existing files to modify

- `internal/packer/packer.go`, `internal/packer/packer_test.go` — packing budget, stale mapping repair, reference-aware orphan reconciliation, loose orphan cleanup, and observability.
- `internal/packer/unpack.go`, `internal/packer/unpack_test.go` — prune stale mappings before restore enumeration.
- `internal/store/packs.go`, `internal/store/packs_test.go` — combined liveness/location resolver, referenced-hash enumeration, and shared mapping repair.
- `internal/store/sources.go`, `internal/store/sources_test.go` — replace the phase-2b refusal with transactional packed-index GC.
- `internal/store/messages.go`, `internal/store/messages_test.go` — reuse/query helpers and the Teams replacement regression fixture.
- `internal/blobstore/blobstore.go`, `internal/blobstore/blobstore_test.go` — enforce attachment liveness on every open and retire cached pack readers/files safely.
- `internal/backupapp/content_source_test.go` — backup/repack overlap through an independent blob-store cache.
- `cmd/msgvault/cmd/serve.go`, `cmd/msgvault/cmd/serve_test.go` — construct the shared blob store/coordinator before scheduler wiring and run post-ingest maintenance in the daemon parent.
- `cmd/msgvault/cmd/remove_account.go`, `cmd/msgvault/cmd/remove_account_test.go` — remove the unpack-first refusal and validate logical deletion.
- `cmd/msgvault/cmd/pack_attachments.go`, `cmd/msgvault/cmd/pack_attachments_test.go` — report repair/quarantine/cleanup stats while preserving explicit unbounded packing.
- `cmd/msgvault/cmd/unpack_attachments.go`, `cmd/msgvault/cmd/unpack_attachments_test.go` — report stale-mapping repair while preserving the host-local downgrade guard.
- `internal/api/cli_handlers.go`, `internal/api/handlers_test.go` — admit `repack-attachments` through the generic daemon CLI endpoint.
- `internal/api/backup_freeze.go` — correct the stale comment: the gate protects checkpoint-and-pin, not attachment capture.
- `docs/internal/packed-attachments-design.md` and `docs/superpowers/specs/2026-07-09-packed-attachments-phase-2c-design.md` — mark implementation completion only after all checkpoints and reviews pass.

## Checkpoint 1 — bounded automatic packing

### Task 1: Add a soft raw-byte budget to the packer

**Files:**
- Modify: `internal/packer/packer.go:25-55,296-356`
- Modify: `internal/packer/packer_test.go:203-315`

- [ ] **Step 1: Add failing budget tests**

Add table-driven tests covering below, exactly at, and above the boundary, plus one blob larger than the budget. Assert the number of blobs packed, raw bytes packed, `BudgetExhausted`, sealed packs, remaining loose blobs, and readability of both packed and remaining loose content.

```go
func TestRunHonorsRawByteBudget(t *testing.T) {
    tests := []struct {
        name          string
        maxBytes      int64
        wantPacked    int
        wantExhausted bool
    }{
        {name: "below first blob", maxBytes: 1, wantPacked: 1, wantExhausted: true},
        {name: "exactly one blob", maxBytes: 600, wantPacked: 1, wantExhausted: true},
        {name: "above one blob", maxBytes: 601, wantPacked: 2, wantExhausted: true},
        {name: "unlimited", maxBytes: 0, wantPacked: 3, wantExhausted: false},
    }
    // Each case seeds three 600-byte verified loose blobs and runs packer.Run.
}
```

Add a separate regression proving dangling-record repair, orphan reconciliation, and the loose-tree sweep still run after the new-blob budget is exhausted.

- [ ] **Step 2: Run the tests and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/packer -run 'TestRun(HonorsRawByteBudget|BudgetStillRunsRecoveryAndSweep)' -count=1
```

Expected: FAIL to compile because `packer.Options.MaxBytes` and `Stats.BudgetExhausted` do not exist.

- [ ] **Step 3: Implement the minimal budget**

```go
type Options struct {
    TargetSize int64
    MaxBytes   int64 // zero means unlimited; soft at one verified blob
}

type Stats struct {
    // existing fields...
    BudgetExhausted bool
}
```

In `packLoose`, count raw bytes only after a verified append. When `MaxBytes > 0 && stats.BytesPacked >= MaxBytes`, seal and record the current writer, set `BudgetExhausted`, and stop enumerating new loose blobs. Return normally so `Run` continues into the final repair/sweep phase. A single oversized blob therefore makes progress and may exceed the limit.

- [ ] **Step 4: Rerun focused and existing packer tests GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/packer -count=1
```

Expected: PASS.

### Task 2: Add the daemon-owned maintenance coordinator and automatic pack hooks

**Files:**
- Create: `cmd/msgvault/cmd/attachment_maintenance.go`
- Create: `cmd/msgvault/cmd/attachment_maintenance_test.go`
- Modify: `cmd/msgvault/cmd/serve.go:164-291,745-839`
- Modify: `cmd/msgvault/cmd/serve_test.go`
- Modify: `internal/api/backup_freeze.go:26-30`

- [ ] **Step 1: Write failing coordinator tests against real temp stores**

Cover these contracts:

1. `packAutomatic` uses `256 << 20` and packs a real loose attachment.
2. A successful `RunCLISync` calls the subprocess runner once and then makes one bounded pack attempt; subprocess failure makes no attempt.
3. A successful attachment-producing `RunCLICommand` packs once; a read-only/non-ingest command and a failed command do not.
4. Gmail/IMAP/Teams scheduled sync and scheduled SyncTech completion call automatic packing only after success; GCal does not.
5. `attachment-maintenance` registers at `17 3 * * *` and a triggered job runs one bounded pack attempt.
6. successful automatic work logs the complete stats at INFO without writing normal CLI output; `context.Canceled` is informational; another maintenance error emits/logs a warning naming `pack-attachments` as the explicit retry command but does not replace an already-successful ingest result.

Use the real coordinator with `testutil.NewTestStore` and a real loose attachment. For hook policy, inject only the subprocess function boundary while still asserting the production packer changed real pack metadata/files; do not merely assert that a fake callback received arguments.

- [ ] **Step 2: Run the tests and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestAttachmentMaintenance|TestStoreAPIAdapter.*AutomaticPack' -count=1
```

Expected: FAIL because the coordinator and adapter fields/helpers do not exist.

- [ ] **Step 3: Implement the coordinator**

```go
const (
    automaticAttachmentBytes = int64(256 << 20)
    attachmentMaintenanceJob = "attachment-maintenance"
    attachmentMaintenanceCron = "17 3 * * *"
)

type attachmentMaintenance struct {
    store          *store.Store
    blobs          *blobstore.Store
    attachmentsDir string
    logger         *slog.Logger
}

func (m *attachmentMaintenance) pack(ctx context.Context, maxBytes int64) (packer.Stats, error) {
    return packer.Run(ctx, m.store, m.attachmentsDir, packer.Options{MaxBytes: maxBytes})
}
```

Add an `attachmentProducingCommand(args []string) bool` allowlist for:

```text
backfill-teams-media
import, import-emlx, import-gvoice, import-imessage, import-mbox,
import-messenger, import-pst, import-synctech-sms, import-whatsapp
sync-synctech-sms, sync-teams
```

Manual Gmail/IMAP `sync` and `sync-full` remain covered by `RunCLISync`. Do not trigger a second pack for explicit `pack-attachments`, calendar-only commands, account setup commands, or removal.

Construct `blobstore.New(s, cfg.AttachmentsDir())` and the coordinator immediately after the daemon operation gate, before `syncFunc`, scheduler jobs, and `storeAPIAdapter`. Defer the shared store's close as today. Wrap successful scheduled source calls inside their already-held scheduler work-gate interval. Register the daily job before `sched.Start`.

Refactor `RunCLISync` and `RunCLICommand` through small helpers that accept the subprocess runner for tests. Production still calls `runDaemonCLISubprocessStream*`. After a successful ingest, run bounded packing and stream a concise warning on non-cancellation failure while returning the ingest success.

Keep explicit `pack-attachments` on `MaxBytes=0` and its existing normal output. Automatic calls are quiet except for structured INFO stats and warnings.

Correct `internal/api/backup_freeze.go` to say End releases the gate immediately after the database session is pinned; content capture follows outside the gate.

- [ ] **Step 4: Run command, scheduler, and packer tests GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/scheduler ./internal/packer -count=1
```

Expected: PASS.

### Task 3: Verify and commit checkpoint 1

**Files:** all checkpoint-1 files.

- [ ] **Step 1: Format and run checkpoint verification**

```bash
make fmt
go test -tags "fts5 sqlite_vec" ./internal/packer ./internal/scheduler ./cmd/msgvault/cmd -count=1
go vet -tags "fts5 sqlite_vec" ./internal/packer ./internal/scheduler ./cmd/msgvault/cmd
make lint-ci
make test
```

Expected: all commands PASS; no branch binary is installed or run.

- [ ] **Step 2: Review the complete diff and commit**

Apply `@kenn:commit`, stage every checkpoint-1 file, and create:

```text
Bound automatic packed attachment maintenance
```

The body must explain that bounded post-ingest work gradually migrates existing vaults without turning a small sync into an unbounded conversion, and that maintenance failure cannot erase ingest success.

Run `@kenn:scrub-private-data`, push this commit to `origin/packed-attachments`, and inspect the checkpoint CI results on PR #464. Fix any build/test failure before checkpoint 2; leave code-review findings for the user-requested final `@roborev-fix` pass.

## Checkpoint 2 — reference-aware logical GC

### Task 4: Make attachment liveness part of blob resolution and add shared repair

**Files:**
- Modify: `internal/store/packs.go:12-277`
- Modify: `internal/store/packs_test.go`
- Modify: `internal/blobstore/blobstore.go:31-100`
- Modify: `internal/blobstore/blobstore_test.go`
- Modify: `internal/api/handlers_test.go:2641-2715` — seed real attachment references for the existing packed endpoint fixture.

- [ ] **Step 1: Add failing store and blob-store tests**

Cover:

- referenced + indexed returns a pack entry;
- referenced + unindexed falls back to canonical loose bytes;
- unreferenced + stale indexed returns `fs.ErrNotExist` without opening the pack;
- unreferenced + canonical loose returns `fs.ErrNotExist`;
- content and thumbnail references both count, including cross-column sharing;
- `PruneUnreferencedPackIndex` deletes only unreferenced mappings and returns the exact row count;
- `ListReferencedBlobHashes` returns the union of non-empty content and thumbnail hashes.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store ./internal/blobstore -run 'Test(ResolveAttachmentBlob|PruneUnreferencedPackIndex|ListReferencedBlobHashes|OpenRejectsUnreferenced)' -count=1
```

Expected: FAIL because the combined resolver and repair methods do not exist.

- [ ] **Step 3: Implement one-query resolution and repair**

```go
type AttachmentBlobLocation struct {
    Referenced bool
    Pack       *PackIndexEntry
}

func (s *Store) ResolveAttachmentBlob(blobHash string) (AttachmentBlobLocation, error)
func (s *Store) ListReferencedBlobHashes() (map[string]struct{}, error)
func (s *Store) PruneUnreferencedPackIndex() (int64, error)
```

Use one dialect-rebound query for resolution. It must always return one row and left-join the optional mapping while computing liveness from both attachment hash columns:

```sql
WITH requested(blob_hash) AS (VALUES (CAST(? AS TEXT)))
SELECT CASE WHEN EXISTS (
           SELECT 1 FROM attachments a
           WHERE a.content_hash = requested.blob_hash
              OR a.thumbnail_hash = requested.blob_hash
       ) THEN 1 ELSE 0 END,
       p.blob_hash, p.pack_id, p.pack_offset,
       p.stored_len, p.raw_len, p.flags, p.crc32c
FROM requested
LEFT JOIN attachment_pack_index p ON p.blob_hash = requested.blob_hash
```

Scan nullable pack columns. If not referenced, return `Referenced=false` even when a stale mapping exists. Keep `GetAttachmentPackEntry` as the raw maintenance accessor.

Change the blob-store interface to `ResolveAttachmentBlob`. Every initial lookup and one-time race retry uses it. If `Referenced` is false, immediately return `fs.ErrNotExist`; only referenced/unindexed hashes may attempt loose fallback.

Update every existing fake index and packed API fixture to declare whether its hash is referenced. In particular, `TestCLIAttachmentServesPackedBlob` must seed an attachment row before recording its mapping; a mapping alone is intentionally no longer enough to serve bytes.

Implement repair with one DELETE whose `NOT EXISTS` checks both hash columns and return `RowsAffected`. Implement the referenced set with `UNION`, not `SELECT DISTINCT` over joins.

- [ ] **Step 4: Run the full store/blobstore suites GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store ./internal/blobstore -count=1
```

Expected: PASS on SQLite; queries remain PostgreSQL-rebindable.

### Task 5: Make pack recovery, reconciliation, sweeping, and unpack use the same liveness authority

**Files:**
- Modify: `internal/packer/packer.go`
- Modify: `internal/packer/packer_test.go`
- Modify: `internal/packer/unpack.go`
- Modify: `internal/packer/unpack_test.go`
- Modify: `cmd/msgvault/cmd/pack_attachments.go`
- Modify: `cmd/msgvault/cmd/pack_attachments_test.go`
- Modify: `cmd/msgvault/cmd/unpack_attachments.go`
- Modify: `cmd/msgvault/cmd/unpack_attachments_test.go`
- Modify: `internal/store/messages_test.go`

- [ ] **Step 1: Add failing lifecycle regressions**

Add real production-path tests for:

1. Pack Teams inline media, call `ReplaceMessageInlineAttachments` with a replacement set, run the packer, and assert the old mapping is pruned and no longer counted live.
2. `Unpack` prunes a stale mapping before enumeration and never restores the deleted hash.
3. The existing filesystem walk removes unreferenced canonical and legacy/noncanonical hash-named loose files, leaves referenced loose files, skips non-hash filenames and `packs/`, and counts removals.
4. Inject one `os.Remove` failure at the narrow loose-orphan removal seam, assert the run warns/leaves the file, restore the real remover, rerun, and assert cleanup succeeds. This test must drive the real walk/classification path.
5. A readable orphan with referenced valid and referenced corrupt candidates is not partially adopted; `PacksQuarantined == 1`, the ERROR names the pack and failed/withheld counts, and every mapping remains unchanged.
6. An orphan whose footer cannot open increments only `PacksUnreadable`, logs pack ID/path/error, and remains for retry.
7. Unreferenced orphan entries are never adopted; a fully unreferenced orphan is removed as redundant.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/packer ./cmd/msgvault/cmd -run 'Test(RunPrunesTeamsReplacement|RunCleansUnreferencedLoose|RunQuarantinesMixedDamagedOrphan|RunReportsUnreadableOrphan|UnpackPrunesStaleMappings|PackAttachmentsReportsRepairStats)' -count=1
```

Expected: FAIL because repair ordering, new stats, and reference-aware reconciliation are absent.

- [ ] **Step 3: Implement shared repair and all-or-nothing reconciliation**

Extend `packer.Stats` with:

```go
MappingsPruned      int
LooseOrphansRemoved int
PacksQuarantined    int
PacksUnreadable     int
```

In `Run`, after dangling-record cleanup and before orphan reconciliation:

1. call `PruneUnreferencedPackIndex` and record the count;
2. load one referenced-hash set;
3. reconcile orphans using that set;
4. pack new loose blobs;
5. reload the indexed-hash set and perform one combined loose-tree walk.

For each orphan footer entry:

- unreferenced: dead, never verify merely to delete, never adopt;
- referenced with a readable current mapping: redundant;
- referenced without a readable mapping: verify from the orphan and stage adoption;
- any failed referenced candidate: discard the entire staged adoption for that pack, preserve the file unrecorded, increment `PacksQuarantined`, and emit one ERROR with `pack`, `failedEntries`, and `withheldEntries`;
- unreadable footer: preserve, increment `PacksUnreadable`, and emit a separate ERROR with pack ID, path, and open error.

Replace `sweepIndexed` with a single classifier over every regular hash-named file outside `packs/`:

```text
unreferenced                  -> best-effort delete; warn and retry next run
referenced + indexed          -> existing verify/sweep or loose-recovery behavior
referenced + unindexed        -> leave live loose content
invalid hash filename         -> ignore
```

Call `PruneUnreferencedPackIndex` at the start of `Unpack`, before `ListPackRecords`. Add its count to `UnpackStats` for observability. Update explicit pack/unpack output for nonzero repair, loose-orphan, quarantine, and unreadable counts.

- [ ] **Step 4: Run packer, store-message, and command tests GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/packer ./internal/store ./cmd/msgvault/cmd -count=1
```

Expected: PASS.

### Task 6: Replace account-removal refusal with transactional logical GC

**Files:**
- Modify: `internal/store/sources.go:178-268`
- Modify: `internal/store/messages.go:2057-2144`
- Modify: `internal/store/sources_test.go:169-259`
- Modify: `internal/store/attachment_e2e_test.go:346-347` — update the existing call for the three-value removal result and assert the mapping count.
- Modify: `cmd/msgvault/cmd/remove_account.go:32-45,167-207`
- Modify: `cmd/msgvault/cmd/remove_account_test.go:255-360`

- [ ] **Step 1: Add failing transaction and CLI tests**

Cover:

- unique packed content and thumbnails lose mappings in the same transaction as the source cascade;
- content/content, thumbnail/thumbnail, content/thumbnail, and thumbnail/content sharing preserve mappings;
- `RemoveSourceSerialized` returns both `hadActiveSync` and the exact number of packed mappings removed, and `remove-account` reports a nonzero count;
- after successful removal, `blobstore.Open` returns `fs.ErrNotExist` for unique hashes even when a loose crash copy remains;
- the source removal succeeds without `unpack-attachments`;
- a forced source-delete failure rolls back both the source and every mapping. Use a real database trigger that aborts source deletion (SQLite and PostgreSQL variants), not a fake store method.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store ./cmd/msgvault/cmd -run 'TestStore_RemoveSourceSerialized_.*Packed|TestRemoveAccountCmd_.*Packed' -count=1
```

Expected: FAIL because `UniquePackedBlobsError` still refuses removal.

- [ ] **Step 3: Implement the atomic mapping deletion**

Change the result contract explicitly:

```go
func (s *Store) RemoveSourceSerialized(
    ctx context.Context, sourceID int64,
) (hadActiveSync bool, packedMappingsRemoved int64, err error)
```

Inside the existing manual exclusive transaction, first query and retain in Go the distinct packed hashes referenced by the selected source and by no other source across either hash column:

```sql
WITH source_blobs(blob_hash) AS (
    SELECT a.content_hash FROM attachments a
    WHERE a.content_hash IS NOT NULL AND a.content_hash != ''
      AND EXISTS (SELECT 1 FROM messages m
                  WHERE m.id = a.message_id AND m.source_id = ?)
    UNION
    SELECT a.thumbnail_hash FROM attachments a
    WHERE a.thumbnail_hash IS NOT NULL AND a.thumbnail_hash != ''
      AND EXISTS (SELECT 1 FROM messages m
                  WHERE m.id = a.message_id AND m.source_id = ?)
)
SELECT sb.blob_hash
FROM source_blobs sb
WHERE EXISTS (SELECT 1 FROM attachment_pack_index p
              WHERE p.blob_hash = sb.blob_hash)
  AND NOT EXISTS (
      SELECT 1 FROM attachments a2
      WHERE (a2.content_hash = sb.blob_hash OR a2.thumbnail_hash = sb.blob_hash)
        AND EXISTS (SELECT 1 FROM messages m2
                    WHERE m2.id = a2.message_id AND m2.source_id != ?)
  )
ORDER BY sb.blob_hash
```

Then perform the existing FTS/source cascade. After the cascade, delete the retained hashes from `attachment_pack_index` in dialect-rebound chunks below SQLite's parameter limit, sum `RowsAffected`, and COMMIT. Every query/delete stays on the same `*sql.Conn` transaction. Any failure rolls back the cascade and all mapping deletions.

Return the active-sync flag and summed mapping count. Remove `UniquePackedBlobsError`, its count helper, CLI special-case error, and unpack-first help text. When the count is nonzero, `remove-account` prints concise maintenance context that logical mappings were removed and physical bytes are reclaimed by repack. Keep loose-file cleanup best-effort; the resolver now makes leftovers unreachable and the packer's next sweep retries their deletion.

- [ ] **Step 4: Run logical-GC suites GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store ./internal/blobstore ./internal/packer ./cmd/msgvault/cmd -count=1
```

Expected: PASS.

### Task 7: Verify and commit checkpoint 2

**Files:** all checkpoint-2 files.

- [ ] **Step 1: Run format and full checkpoint verification**

```bash
make fmt
go test -tags "fts5 sqlite_vec" ./internal/store ./internal/blobstore ./internal/packer ./cmd/msgvault/cmd -count=1
go test -race -tags "fts5 sqlite_vec" ./internal/blobstore ./internal/packer -count=1
go vet -tags "fts5 sqlite_vec" ./internal/store ./internal/blobstore ./internal/packer ./cmd/msgvault/cmd
make lint-ci
make test
```

Expected: PASS.

- [ ] **Step 2: Commit checkpoint 2**

Apply `@kenn:commit` and create:

```text
Make packed attachment deletion reference-aware
```

The body must explain why attachment rows, not stale storage mappings, are the liveness authority and why orphan adoption is all-or-nothing when referenced recovery candidates are damaged.

Run `@kenn:scrub-private-data`, push this commit to `origin/packed-attachments`, and inspect PR #464's checkpoint CI. Fix build/test failures before checkpoint 3; defer accumulated review findings to the final `@roborev-fix` pass.

## Checkpoint 3 — physical repack and reader retirement

### Task 8: Add referenced pack accounting and transactional swap primitives

**Files:**
- Create: `internal/store/repack.go`
- Create: `internal/store/repack_test.go`
- Modify: `internal/store/packs.go` only for shared validation helpers if required.

- [ ] **Step 1: Add failing store tests**

Test:

- usage accounting returns immutable totals plus only referenced live entry/stored/raw totals, ordered by `created_at`, then pack ID;
- referenced entry enumeration ignores stale rows defensively;
- a multi-pack swap inserts every new record and CAS-updates every expected mapping atomically;
- missing, added, or changed expected mappings—including an entire selected source pack omitted from `moves`—cause rollback, leaving every old mapping authoritative and no new pack records;
- guarded old-record deletion refuses a still-live pack and transactionally deletes both a zero-live record and any stale unreferenced index rows without relying on an FK cascade.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store -run 'Test(PackUsage|ListReferencedPackEntries|CommitRepack|DeleteEmptyPackRecord)' -count=1
```

Expected: FAIL because `repack.go` APIs do not exist.

- [ ] **Step 3: Implement store primitives**

```go
type PackUsage struct {
    PackRecord
    LiveEntries     int64
    LiveStoredBytes int64
    LiveRawBytes    int64
}

type RepackMove struct {
    OldPackID string
    NewEntry  PackIndexEntry
}

func (s *Store) ListPackUsage() ([]PackUsage, error)
func (s *Store) ListReferencedPackEntries(packID string) ([]PackIndexEntry, error)
func (s *Store) CommitRepack(ctx context.Context, sourcePackIDs []string, records []PackRecord, moves []RepackMove) error
func (s *Store) DeleteEmptyPackRecord(packID string) (bool, error)
```

Every live aggregate/enumeration query includes an `EXISTS` against content or thumbnail attachment references even though callers repair first. `CommitRepack` uses `runMaintenance` and performs this order in one transaction:

1. validate and deduplicate the complete `sourcePackIDs` set, validate every new record/move, and require every move's old pack to belong to that set;
2. query the current referenced rows for every explicitly selected source pack and require an exact match to the submitted move set, including detecting a selected pack omitted wholesale from `moves`;
3. insert all new pack records;
4. CAS each mapping with `UPDATE ... WHERE blob_hash=? AND pack_id=?`, requiring one affected row;
5. require no referenced rows remain in selected old packs;
6. commit.

`DeleteEmptyPackRecord` runs one transaction: refuse when any referenced mapping names the pack; otherwise explicitly delete every remaining stale/unreferenced `attachment_pack_index` row for that pack and then delete the `attachment_packs` row. Return `false` instead of deleting a live record. Do not assume an FK cascade: neither backend defines one for `attachment_pack_index.pack_id`.
Treat impossible accounting states (`LiveEntries > EntryCount`, negative live/dead byte totals, malformed pack IDs) as explicit errors rather than selecting or deleting against corrupt metadata.

- [ ] **Step 4: Run store tests GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/store -count=1
```

Expected: PASS.

### Task 9: Add daemon-cache reader retirement and external-reader coverage

**Files:**
- Modify: `internal/blobstore/blobstore.go`
- Modify: `internal/blobstore/blobstore_test.go`

- [ ] **Step 1: Add failing retirement/race tests**

Test invalid pack IDs, absent files as success, cached-reader closure, order removal, and a stale index retry to a replacement pack. Add one platform-aware independent-reader test:

1. build an old pack with two blobs and cache its reader in a separate backup-style `blobstore.Store`;
2. swap the current mapping to a new pack;
3. retire the old pack through the daemon store;
4. on Unix, assert unlink succeeds, the independent cached reader can still read the second old entry, and new daemon opens use the replacement mapping;
5. on Windows, assert retirement returns a sharing violation and the file remains, close the independent store, retry retirement, and assert deletion succeeds.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/blobstore -run 'TestRetirePack|TestIndependentReaderAcrossRetire' -count=1
```

Expected: FAIL because `RetirePack` does not exist.

- [ ] **Step 3: Implement retirement under the read mutex**

```go
func (s *Store) RetirePack(packID string) error {
    if !pack.IsValidPackID(packID) { /* return validation error */ }
    s.mu.Lock()
    defer s.mu.Unlock()
    // Remove from readers and FIFO, close cached reader if present,
    // then os.Remove(canonical sharded path) before unlocking.
    // fs.ErrNotExist is success; return close/remove errors (joined if both).
}
```

Do not delete the database record here. The repacker owns that second step only after physical deletion succeeds.

- [ ] **Step 4: Run blob-store tests, including race mode, GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/blobstore -count=1
go test -race -tags "fts5 sqlite_vec" ./internal/blobstore -count=1
```

Expected: PASS. The Windows-specific branch will execute in normal Windows CI.

### Task 10: Implement deterministic verified repacking

**Files:**
- Create: `internal/repacker/repacker.go`
- Create: `internal/repacker/repacker_test.go`

- [ ] **Step 1: Add failing selection and engine tests**

Cover:

- zero-live packs are always selected regardless of age/size and require no new writer;
- partial packs require all three rules: live entries below 50%, age at least 24 hours, dead stored bytes at least 8 MiB;
- selection order is creation time then pack ID;
- a 256 MiB live-raw budget is soft at one selected source pack, guarantees one eligible partial pack, and never excludes zero-live packs;
- live entries from multiple sparse packs combine into target-sized new packs and remain byte-identical through the production blob store;
- corrupt/read failure, append/seal failure, or CAS mismatch leaves old mappings/files authoritative; sealed new files are unrecorded orphans;
- an injected first retirement failure leaves the swapped old zero-live record/file for retry; the next run removes it;
- a post-delete/pre-record-cleanup failure is repaired by the existing dangling-record pass;
- context cancellation stops at selection/read/seal/cleanup boundaries without invalid state.

Use actual corrupt packs and database triggers for read/CAS/record-cleanup failures. For otherwise unreachable kit writer failures, add one narrow unexported writer-factory seam in package `repacker`; production always supplies the real `pack.Writer`, and the test still drives the complete `Run` state machine rather than testing the fake itself.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./internal/repacker -count=1
```

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement the repacker**

```go
const (
    minPackAge    = 24 * time.Hour
    minDeadStored = int64(8 << 20)
)

type Options struct {
    TargetSize int64
    MaxBytes   int64     // zero means unlimited live raw bytes
    Now        time.Time // zero means time.Now().UTC(); deterministic tests
}

type Stats struct {
    MappingsPruned  int
    PacksSelected   int
    PacksRewritten  int
    PacksSealed     int
    PacksRemoved    int
    BlobsRepacked   int
    BytesRepacked   int64
    BudgetExhausted bool
}

type BlobStore interface {
    Open(string) (io.ReadSeekCloser, int64, error)
    RetirePack(string) error
}

func Run(ctx context.Context, st *store.Store, blobs BlobStore, attachmentsDir string, opts Options) (Stats, error)
```

Run order:

1. prune unreferenced mappings;
2. list usage and deterministically select all zero-live plus bounded eligible partial packs;
3. enumerate only referenced entries for partial packs;
4. read each through `blobs.Open` (kit validates CRC, decode, and SHA), append, require `Writer.Append`'s returned `Entry.ID` equals the expected hash, and combine entries into normal target-sized packs;
5. seal every new pack before any database mutation, retaining expected old-pack ownership for each hash;
6. call `CommitRepack` once with the complete selected partial-source pack ID set plus all new records/moves;
7. for every selected old pack, call `RetirePack`; only on success call `DeleteEmptyPackRecord` and require it returned true;
8. attempt cleanup for all selected old packs and return joined errors so one externally-held file does not prevent other safe reclamation.

Abort the active writer on error. Never delete already-sealed unrecorded files; the reference-aware reconciler owns them.

- [ ] **Step 4: Run repacker and adjacent suites GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/repacker ./internal/store ./internal/blobstore ./internal/packer -count=1
go test -race -tags "fts5 sqlite_vec" ./internal/repacker ./internal/blobstore -count=1
```

Expected: PASS.

### Task 11: Wire explicit, daily, and post-removal repack into the daemon parent

**Files:**
- Create: `cmd/msgvault/cmd/repack_attachments.go`
- Create: `cmd/msgvault/cmd/repack_attachments_test.go`
- Modify: `cmd/msgvault/cmd/attachment_maintenance.go`
- Modify: `cmd/msgvault/cmd/attachment_maintenance_test.go`
- Modify: `cmd/msgvault/cmd/serve.go:265-291,828-839`
- Modify: `internal/api/cli_handlers.go:1017-1064`
- Modify: `internal/api/handlers_test.go:825-930,1250-1320`
- Modify: `cmd/msgvault/cmd/remove_account_test.go`

- [ ] **Step 1: Add failing daemon-native command/hook tests**

Test:

- `repack-attachments` is admitted and proxied, but `storeAPIAdapter` intercepts it and calls the parent coordinator instead of spawning a child process;
- explicit repack passes `MaxBytes=0`, returns failures to the CLI, and prints stats;
- the daily job runs bounded pack then bounded repack in order and returns an error for the scheduler to record;
- a successful `remove-account` subprocess triggers one bounded repack; a failed removal does not;
- other successful attachment-producing commands still trigger only bounded pack, never repack;
- a post-removal repack warning names `repack-attachments` as the explicit retry command while preserving command success;
- cancellation/yield exits without converting a successful ingest/removal into failure, while explicit repack remains fail-fast.

Tests must use real pack/store/repacker effects in temp state. The injected subprocess seam may only determine whether the preceding command succeeds; assertions must inspect real pack mappings/files and coordinator stats.

- [ ] **Step 2: Run and observe RED**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api -run 'Test(RepackAttachments|AttachmentMaintenance.*Repack|StoreAPIAdapter.*Repack|HandleCLIRunAllowsRepack)' -count=1
```

Expected: FAIL because the command, allowlist entry, and coordinator repack methods do not exist.

- [ ] **Step 3: Implement daemon-native wiring**

```go
func (m *attachmentMaintenance) repack(ctx context.Context, maxBytes int64) (repacker.Stats, error)
func (m *attachmentMaintenance) daily(ctx context.Context) error // bounded pack, then bounded repack
```

The explicit Cobra command always calls `runDaemonCLICommandHTTPFromCobra`; it has no direct local-store path. Add it to `cliRunCommandAllowed`, leaving it gated as a mutating request.

In `storeAPIAdapter.RunCLICommand`, intercept `repack-attachments` before the subprocess runner and execute unbounded repack against the parent daemon's shared `blobstore.Store`. For ordinary subprocess commands:

- success + attachment-producing command -> bounded pack;
- success + `remove-account` -> bounded repack;
- otherwise -> no attachment maintenance.

Update the daily registered job from pack-only to `daily`. Ensure no coordinator method reacquires the operation gate: HTTP middleware/scheduler already holds it.

- [ ] **Step 4: Run daemon/API/command tests GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api ./internal/scheduler ./internal/repacker -count=1
```

Expected: PASS.

### Task 12: Verify backup overlap and the complete automated lifecycle

**Files:**
- Modify: `internal/backupapp/content_source_test.go`
- Modify: `internal/repacker/repacker_test.go`
- Modify: `internal/packer/unpack_test.go`
- Modify: `cmd/msgvault/cmd/backup_test.go` only if the full restore assertion belongs at the command boundary.
- Modify: `internal/api/handlers_test.go` — read a live blob through the daemon attachment endpoint after repack.
- Modify: `cmd/msgvault/cmd/mcp_test.go` — fetch the same post-repack blob through the daemon-backed MCP resource path.
- Modify: `cmd/msgvault/cmd/export_attachment_test.go` and `cmd/msgvault/cmd/export_attachments_test.go` — export the same post-repack bytes through file and archive/directory flows.

- [ ] **Step 1: Add backup/repack overlap integration verification**

Using only temp directories and a real SQLite store:

1. create current attachment rows and one genuinely eligible sparse pack: one live blob plus more than 8 MiB of incompressible dead entries, with `created_at` older than 24 hours;
2. create a `backup.ContentSource` backed by its own `blobstore.Store`;
3. start `backup.Create` with `Jobs: 1` and a narrow wrapper that signals after the real source has opened/cached an old-pack blob, then blocks before returning it;
4. run repack through the daemon-style shared blob store while backup remains in flight;
5. Unix: repack unlinks the old file and backup completes from its open handle or replacement mapping;
6. Windows: repack returns the expected old-file deletion error with the zero-live record/file intact; release and finish backup, close its store, rerun repack, and assert cleanup succeeds;
7. verify the backup repository/snapshot with kit's real verifier. No content may be silently omitted.

Add a second case where a concurrent logical deletion removes a pinned-snapshot-only reference and assert backup fails loudly/retryably rather than producing a silently incomplete snapshot. These are integration verification tests for reader-retirement and repack behavior already driven RED/GREEN in Tasks 9–11; they are expected to pass when first added. If either fails, preserve the failure, add the narrowest focused regression that explains the missing contract, then change production code.

- [ ] **Step 2: Run overlap verification**

```bash
go test -tags "fts5 sqlite_vec" ./internal/backupapp ./internal/repacker -run 'TestBackupCaptureOverlapsRepack|TestBackupCaptureFailsLoudlyAfterLogicalDeletion' -count=1
```

Expected: PASS. A failure is evidence of an integration gap, not a reason to weaken the test.

- [ ] **Step 3: Add the complete pack -> GC -> repack -> unpack round trip**

Drive production APIs in one temp vault:

- seed content and thumbnails shared and unique across two sources;
- pack and read through `blobstore.Open`;
- remove one source transactionally;
- assert deleted unique hashes are unreachable and shared hashes remain byte-identical;
- repack and assert dead old files/records are reclaimed;
- read the surviving blob through the real API attachment handler, daemon-backed MCP path, and directory/zip export path after the mapping swap;
- unpack and assert canonical loose bytes, an empty pack index/table inventory, and no stale hash resurrection;
- repack the fully loose result once more to prove the downgrade/upgrade cycle.

- [ ] **Step 4: Run integration and race suites GREEN**

```bash
go test -tags "fts5 sqlite_vec" ./internal/backupapp ./internal/repacker ./internal/packer ./internal/blobstore ./internal/store ./internal/api ./cmd/msgvault/cmd -count=1
go test -race -tags "fts5 sqlite_vec" ./internal/backupapp ./internal/repacker ./internal/blobstore ./internal/api ./cmd/msgvault/cmd -count=1
```

Expected: PASS locally; the real Windows sharing-violation branch passes in `test-windows` CI.

### Task 13: Verify and commit checkpoint 3

**Files:** all checkpoint-3 implementation and tests.

- [ ] **Step 1: Run complete local verification from scratch state**

```bash
make fmt
make test
go test -race -tags "fts5 sqlite_vec" ./internal/blobstore ./internal/packer ./internal/repacker ./internal/backupapp ./cmd/msgvault/cmd -count=1
go vet -tags "fts5 sqlite_vec" ./...
make lint-ci
scratch="$(mktemp -d)"
MSGVAULT_HOME="$scratch/home" go build -tags "fts5 sqlite_vec" -o "$scratch/msgvault" ./cmd/msgvault
"$scratch/msgvault" --help >/dev/null
rm -rf "$scratch"
```

Expected: every command PASS. The built binary only executes `--help` with scratch `MSGVAULT_HOME`; no live archive or daemon is opened.

If `MSGVAULT_TEST_DB` is already configured for the repository's disposable PostgreSQL test service, also run:

```bash
make test-pg
```

Otherwise rely on PR `test-postgres`/`test-pgvector`; do not point tests at any non-test database.

- [ ] **Step 2: Commit checkpoint 3**

Apply `@kenn:commit` and create:

```text
Repack dead attachment bytes safely
```

The body must explain the new-pack-before-swap ordering, retained zero-live records, and why daemon reader retirement plus retryable backup-held handles are required for Windows.

Run `@kenn:scrub-private-data` and push this commit to the existing PR. Do not create another PR. Let all implementation review jobs finish before starting Task 14.

## Final review, repair, and PR handoff

### Task 14: Run roborev-fix, exact-head verification, and refresh PR #464

**Files:** review-dependent fixes plus final design/status/PR metadata.

- [ ] **Step 1: Run `@roborev-fix` exactly as requested**

Discover all open actionable failures with:

```bash
roborev fix --list
```

For the original actionable job set, fetch each JSON review, triage all findings by severity/file, fix them test-first, run `make test`, commit with `@kenn:commit`, comment and close each original job in the required order, and audit every original job with `roborev show --job <id> --json` until `closed=true`.

If fix commits create new failing review jobs, start a new `@roborev-fix` cycle. Repeat until discovery returns no actionable failing reviews. Never close a passing, errored, unrelated, or already-closed job.

- [ ] **Step 2: Update implementation-status docs before the exact-head gate**

Set the Phase 2c spec status to implemented with verification tracked by PR #464, and align the parent design's delivery status. Do not claim the CI/whole-branch gates passed before observing them. Commit this documentation with `@kenn:commit`, scrub it, and push it before requesting the exact-head review so the eventual PASS covers the actual final commit.

- [ ] **Step 3: Run final formatting and local verification before exact-head review**

```bash
make fmt
make test
go test -race -tags "fts5 sqlite_vec" ./internal/blobstore ./internal/packer ./internal/repacker ./internal/backupapp ./cmd/msgvault/cmd -count=1
go vet -tags "fts5 sqlite_vec" ./...
make lint-ci
git diff --check
git status --short --branch
```

If formatting, tests, lint, or vet require any change, fix it test-first, commit, run `@kenn:scrub-private-data`, push, and run `@roborev-fix` for any new actionable review. Repeat this step until every command passes on a clean pushed head.

- [ ] **Step 4: Request exact-head review and inspect CI without changing the head**

Use `@roborev-review-branch` on the clean pushed head after Step 3. Wait for PR #464's Linux, Windows, PostgreSQL, pgvector, Nix, analysis, validation, and roborev checks to finish on that same SHA. Address any real failure; do not call queued/skipped verification a pass.

If the whole-branch review, CI, or any later formatting check causes a code or documentation change, return to Step 3, commit/verify/push it, and request a new whole-branch review. Completion requires an exact-head PASS and all required CI checks on the unchanged final SHA.

- [ ] **Step 5: Refresh PR scope and hand off with evidence**

Apply `@kenn:refresh-pr` so PR #464's title/body describe the complete stacked pipeline: pack storage, reference-aware logical GC, bounded automatic migration, physical repack, Windows reader retirement, backup correctness, explicit pack/unpack/repack commands, and issue #466 as the later pack-native restore optimization.

Apply `@kenn:verify-before-handoff`. Report exact local commands/results, CI conclusions, final commit SHA, roborev job closure audit, whole-branch review result, and PR URL. Explicitly state that no real `~/.msgvault` data was touched and that the copy-based hardening run still awaits separate user approval.
