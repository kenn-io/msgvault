# Packed Attachment Storage

Design for storing attachment content in kit CAS pack files instead of loose
content-addressed files. Written 2026-07-09; status: delivery steps 1-5 (see
Delivery order below) are implemented and copy-based real-archive hardening is
complete on the `packed-attachments` branch. The 64 MiB memory-ceiling
amendment is implemented and locally verified through automated boundary,
fault-injection, and race tests. Pack-native restore remains the separate
follow-up in issue #466. The design for extracting this engine into reusable
Kit infrastructure is in `docs/internal/kit-packstore-extraction-design.md`.
Internal package paths below describe the pre-extraction implementation and
are retained as historical design context.

## Motivation

Attachment content is stored today as loose files under
`~/.msgvault/attachments/<sha256[:2]>/<sha256>`, one file per unique blob.
Measurements on a real archive (46,060 blobs, 7.6 GiB, from 208,532
attachment rows):

- 72% of files are under 64 KiB but hold only 6.6% of the bytes. File count,
  not byte count, is the cost driver.
- Large file counts hurt most on Windows and NAS deployments: per-file open
  and antivirus-scan overhead, slow enumeration, slow copies.
- zstd-3 over a 900-file sample saved only ~13% (attachments are mostly
  already-compressed media). Compression is a minor win; the kit pack
  format's per-blob "compress only if it saves >= 3%" rule handles this
  automatically.

Goals, in priority order:

1. **Uniform architecture** — all ceiling-eligible attachment bytes end up in
   sealed, immutable pack files; loose files otherwise exist only transiently.
   Blobs above the release's safe in-memory ceiling remain deliberately loose
   until kit supports verified streaming pack I/O.
2. **Backup synergy** — production packs use the exact kit pack format,
   blob IDs, extension, target size, and shard layout that `msgvault backup`
   uses, so a future release can teach backup to adopt production packs
   wholesale instead of re-reading and re-packing every blob.

Non-goals for this design: at-rest encryption (kit supports it; msgvault
does not use it yet), backup pack adoption itself (follow-up), disk-space
reduction as a primary objective.

## Baseline used for the design (verified 2026-07-09)

- Canonical write path: `export.StoreAttachmentFile`
  (`internal/export/store_attachment.go`) — atomic temp+rename, dedup by
  existence + hash validation. Callers: Gmail/IMAP sync, mbox/emlx/pst
  importers, Teams.
- Three importers hand-roll writes and must be unified onto
  `StoreAttachmentFile` as prep work: WhatsApp
  (`internal/whatsapp/importer.go:607`), SyncTech SMS
  (`internal/synctechsms/importer.go:459`), FB Messenger
  (`internal/fbmessenger/importer.go:696`).
- Read paths all re-derive the canonical `<aa>/<hash>` path from the hash
  via `export.StoragePath`: HTTP CLI API (`internal/api/cli_handlers.go`),
  MCP (`internal/mcp/handlers.go`), TUI/zip/dir export
  (`internal/export/attachments.go`). Daemon-mode consumers go through the
  `AttachmentReader` HTTP interface.
- Thumbnails are separate content-addressed blobs
  (`attachments.thumbnail_hash` / `thumbnail_path`) and are treated as
  content-bearing by backup (`internal/backupapp/app.go`).
- **Pre-existing bug**: SyncTech records `storage_path =
  synctech-sms/<aa>/<hash>` but every serving read path derives the
  canonical path from the hash, so SyncTech MMS attachments are unreadable
  through the API/MCP/export today. This design fixes it via
  canonicalization (below).
- kit pack format (`go.kenn.io/kit/pack`, v0.4.0): sealed-immutable packs,
  `MVPK` header, per-blob zstd-3 with >= 3% savings gate, footer entry table
  (`ID, Offset, StoredLen, RawLen, Flags, CRC32C`), SHA-256-verified footer,
  atomic staging+fsync+publish. `pack.Reader.ReadBlob` is true random access
  via pread: CRC32C check on stored bytes, decompress, re-verify SHA-256
  against the blob ID. Entry tables are written only at `Seal`; an unsealed
  staging pack is unreadable.
- Backup repos shard packs as `packs/<packID[:2]>/<packID>.mvpack`
  (`backup.Repo.packPath`).

## Design

### Layout

```
~/.msgvault/attachments/
  packs/<ulid[:2]>/<ulid>.mvpack   # sealed, immutable, ~32 MB target
  <aa>/<sha256>                    # loose blobs: fresh writes + not-yet-packed legacy
```

The `packs/` shard layout, `.mvpack` extension, 32 MB target size, and
SHA-256 blob IDs match `msgvault backup` exactly. An existing vault is
simply a fully-unpacked store; the packer migrating it is the upgrade.

### Index

New table in the main DB (SQLite and PostgreSQL):

```sql
-- pack_offset, not offset: OFFSET is a reserved word in SQLite and PostgreSQL.
CREATE TABLE attachment_pack_index (
    blob_hash   TEXT PRIMARY KEY,   -- SHA-256 hex; content OR thumbnail blob
    pack_id     TEXT NOT NULL,      -- ULID of the sealed pack
    pack_offset BIGINT NOT NULL,
    stored_len  BIGINT NOT NULL,
    raw_len     BIGINT NOT NULL,
    flags       INTEGER NOT NULL,   -- pack.BlobFlags (compressed, ...)
    crc32c      BIGINT NOT NULL     -- CRC over stored bytes, from pack.Entry
);
CREATE INDEX idx_attachment_pack_index_pack ON attachment_pack_index(pack_id);

CREATE TABLE attachment_packs (
    pack_id      TEXT PRIMARY KEY,  -- ULID
    entry_count  BIGINT NOT NULL,   -- footer entry count at seal/adoption
    stored_bytes BIGINT NOT NULL,   -- sum of entry stored_len at seal/adoption
    created_at   TEXT NOT NULL
);
```

`attachment_packs` captures each pack's immutable totals when the packer
seals it (or adopts it during crash reconciliation). The intended
`attachment_pack_index` invariant is one mapping per attachment-referenced
blob, but attachment replacement and other row-deletion paths can remove the
last reference without touching the index. Phase-2c repair prunes those stale
mappings before pack/repack accounting. Dead bytes are therefore
`stored_bytes` minus the sum of referenced live index rows. `created_at` feeds
the repack age hysteresis without stat-ing pack files.

BIGINT, not INTEGER: PostgreSQL `INTEGER` is 4-byte signed, too small for a
`uint32` CRC32C and for raw/stored lengths up to `pack.MaxRawLen` (4 GiB).
SQLite integers are 8-byte regardless.

Historical attachment rows can spell the same valid SHA-256 hash with
different letter case. Pack-index keys and pack footer IDs are canonical
lowercase; liveness and inventory queries normalize references with
`LOWER(content_hash)` / `LOWER(thumbnail_hash)`. Matching expression indexes
(`idx_attachments_content_hash_lower` and
`idx_attachments_thumbnail_hash_lower`) keep those lookups indexed. SQLite
declares them in its schema; PostgreSQL creates them during `Store.InitSchema`
under the maintenance transaction that disables the ordinary statement
timeout for one-time builds on large archives.

The column is `blob_hash`, not `content_hash`: it indexes every packed blob,
including thumbnails. The read path locates the pack via the index;
`pack.OpenReader` parses the pack's footer once on first open (small: 61
bytes per entry) and the cached reader serves subsequent reads from its
entry map, so steady-state reads are one DB lookup plus one pread. The
remaining entry columns (`pack_offset` through `crc32c`) let GC compute per-pack
dead bytes without opening packs, support crash reconciliation and `unpack`,
and would enable a future kit API that preads from an externally supplied
`pack.Entry` without any footer parse. The pack footer remains the
authoritative self-describing copy.

### Pre-extraction blob store (`internal/blobstore`)

`Store.Open(hash) (io.ReadSeekCloser, int64, error)`:

1. Resolve attachment liveness and the optional `attachment_pack_index` row in
   one indexed query. A hash absent from both `attachments.content_hash` and
   `attachments.thumbnail_hash` is not live: return `fs.ErrNotExist` without
   trying packed or loose storage. This keeps deliberately deleted hashes
   unreadable even if best-effort cleanup left a loose crash-recovery copy.
2. Live index hit: read via a small LRU cache of open `pack.Reader`s (each
   caches its entry map); return a `bytes.Reader` over the verified blob.
   Validate the DB-sourced `pack_id` with `pack.IsValidPackID` before building
   the `packs/<id[:2]>/<id>.mvpack` path — the ID comes from mutable DB state
   and is joined straight into a filesystem path.
3. Live index miss: `os.Open` the canonical loose path.
4. **Race rules** (both resolved by retrying the resolver once before
   failing, including the liveness check):
   - Loose open returns ENOENT: the reader missed the index just before the
     packer committed, then lost the loose file to the packer's post-commit
     delete. The retry finds the new index row.
   - Pack open returns ENOENT (or the blob is absent from the pack's entry
     map): the reader loaded an index row just before a repack swapped rows
     and deleted the old pack. The retry finds the new `pack_id`.

All read consumers (HTTP handler, MCP, TUI export, zip/dir export) switch
from `StoragePath` + `os.Open` to the blob store. The `AttachmentReader`
HTTP interface is unchanged; its server side uses the blob store.

### Write path

Unchanged. `StoreAttachmentFile` keeps writing loose canonical files with
atomic rename; DB rows commit immediately and blobs are readable
immediately. Loose files are the staging area.

Prep work (separate PR, before the packer): unify WhatsApp, SyncTech, and
FB Messenger onto `StoreAttachmentFile` so no new noncanonical paths are
created.

### Packer

`msgvault pack-attachments`; also runs automatically at the end of sync and
import runs and on the daemon schedule. It acquires the daemon operation
gate (like other maintenance ops), so it cannot overlap the short backup
checkpoint-and-pin window. Kit releases that gate before attachment capture,
which can overlap later maintenance through its independent blob store.

1. Enumerate loose blobs from the DB: distinct local (non-URL)
   `content_hash` and `thumbnail_hash` values without an index row, locating
   files by every distinct DB-recorded `storage_path`/`thumbnail_path` (which
   finds noncanonical legacy paths, not just canonical ones). Valid hashes are
   grouped by normalized lowercase identity before append; every original case
   spelling and candidate path is retained. Candidates are tried until one
   verifies, so one missing or corrupt alias cannot pin a valid copy. Rows with
   empty or URL storage paths are skipped, as are hashes with no readable,
   verified candidate on disk (logged, left for a future backfill).
2. Append blobs to a `pack.Writer` (32 MB target), seal each pack
   (durable, atomic publish under `packs/<id[:2]>/`).
3. In one DB transaction per sealed pack: insert the `attachment_packs`
   row and one canonical index row per normalized hash, then **canonicalize**
   every case alias and noncanonical local `storage_path`/`thumbnail_path` row
   to lowercase `<aa>/<hash>`. Orphan adoption applies the same alias
   coalescing and can repoint a case-equivalent existing mapping; a duplicate
   normalized footer ID is rejected before metadata commit or loose deletion.
4. Delete the loose files (including noncanonical originals).

Crash safety at each boundary:

- After 2, before 3: a sealed pack with no index rows. On the next run the
  packer scans `packs/` for unreferenced packs, reads their footers, and
  adopts entries for hashes still unindexed (verifying blob hashes), or
  deletes the pack if fully redundant.
- After 3, before 4: loose files linger harmlessly; the next sweep removes
  an indexed loose file only after opening the packed copy through the
  production blob store. Corrupt metadata or pack bytes therefore preserve
  the loose recovery copy. If the loose candidate verifies, the sweep first
  materializes it at the canonical path and drops only that unreadable blob's
  index row, so reads recover immediately and the next run can repack it. If
  the loose candidate does not verify, the failing packed index is retained so
  readers fail closed instead of serving unverified bytes.

Before orphan reconciliation, the packer drops records whose canonical
sharded pack file is missing, plus records with malformed pack IDs that no
reader could open. Repairing metadata first is important when a missing
recorded pack and an orphan pack contain the same blob: reconciliation can
adopt the orphan instead of mistaking it for a redundant copy and deleting
it. An existing index row is likewise not sufficient proof that an orphan
entry is redundant: reconciliation reads the indexed copy through the
production blob store. If that read fails but the orphan entry verifies, the
orphan adoption transaction repoints that blob's index row to the readable
pack. The old pack record remains for normal dead-byte accounting and later
GC/repack.

Once logical GC is enabled, orphan adoption also consults attachment liveness:
unreferenced footer entries are dead and are never adopted. If any referenced
entry that needs adoption or repointing fails verification, reconciliation is
all-or-nothing for that pack — it records nothing and leaves the orphan file in
place. This prevents a later repack from deleting a quarantined failed entry
after partially adopting the valid entries beside it. The tradeoff is explicit:
otherwise-valid adoptable entries in that pack remain unavailable. The packer
increments a quarantine stat and logs one ERROR with the pack ID and failed and
withheld entry counts; each later run re-verifies the referenced candidates.
An orphan whose footer cannot be opened is preserved and retried under a
separate unreadable-pack stat and ERROR containing the pack ID, path, and open
error; it is not also counted as a quarantined readable-footer pack.

Before reconciliation, the packer also deletes index mappings whose hashes no
longer appear in either attachment hash column. Teams inline replacement,
permanent message deletion, and generic cascades can otherwise leave such rows
behind. Repack runs the same repair before accounting so an explicit repack is
safe even when no packer run preceded it. `unpack-attachments` runs it before
enumerating restore rows so downgrade cannot materialize a stale mapping.

The packer's final filesystem walk classifies every regular hash-named file
outside `packs/` from one referenced-hash set and one indexed-hash set.
Unreferenced files are deleted without being treated as recovery copies and a
failure is retried on the next run. Referenced indexed files retain the existing
verify-before-sweep/recovery behavior; referenced unindexed files remain live
loose content. This covers canonical and legacy noncanonical paths without a
second tree walk.

Cancellation is checked before the pack directory is created and throughout
staging cleanup, dangling-record repair, orphan reconciliation, packing, and
the final sweep. A context canceled before `Run` therefore causes no filesystem
or metadata mutation; mid-run cancellation stops at the next recovery/blob
boundary without violating the same crash-ordering rules.

### Automatic maintenance policy

Automatic pack and repack runs each have a 256 MiB raw-byte budget. The budget
is soft at one eligible-blob or eligible-source-pack boundary: a run finishes
and seals the current output after crossing it, so normal work cannot leave a
live staging writer. Correctness work is not budgeted — staging cleanup,
dangling-record and reference repair, orphan reconciliation, inventory
accounting, zero-live retirement, and the final loose-file sweep still run.
Explicit `pack-attachments` and `repack-attachments` have no aggregate byte
budget.

The daemon owns one maintenance coordinator over its production store, shared
blob store, and attachments directory. Callers already hold the daemon
operation gate; the coordinator does not acquire it recursively. Successful
manual and scheduled attachment-producing sync/import commands run bounded
packing as best-effort follow-up. The generic command allowlist is
`backfill-teams-media`; `import`, `import-emlx`, `import-gvoice`,
`import-imessage`, `import-mbox`, `import-messenger`, `import-pst`,
`import-synctech-sms`, and `import-whatsapp`; plus `sync-synctech-sms` and
`sync-teams`. The dedicated sync path covers `sync` and `sync-full`. Setup and
calendar-only commands are excluded because they do not currently ingest
attachment bytes. The allowlist and its test must change together if a setup
path starts writing attachments.

A daily `attachment-maintenance` scheduler job runs at 03:17 daemon-local time:
bounded pack first, then bounded repack. Packing first repairs recoverable
metadata before repack accounts for live bytes. Ingest success is not rolled
back when best-effort maintenance fails; the command streams or logs a warning
and the daily job retries. The daily job itself records a failure. Explicit
maintenance remains fail-fast. `repack-attachments` executes in the parent
daemon because only that process owns the shared reader cache that must be
retired before Windows file deletion.

### Per-blob memory ceiling

kit v0.4 accepts a complete `[]byte` in `pack.Writer.Append`, and its ordinary
production `pack.Reader.ReadBlob` materializes a complete decoded blob. The
pack format's 4 GiB `pack.MaxRawLen` is therefore a representation bound, not a
safe daemon-memory policy. Maintenance does not use kit's ordinary reader: the
msgvault blob store owns a bounded stable-plain-v1 parser and decoder described
below, while writers still require one ceiling-eligible blob in memory.

Until kit exposes verified streaming reads and writes, both automatic and
explicit maintenance enforce a fixed 64 MiB raw-blob ceiling:

- The loose packer reads through a 64 MiB + 1 byte bounded reader, rather than
  `os.ReadFile`. A larger canonical candidate remains unindexed and loose, and
  packing continues with later blobs; production reads, exports, and backup
  continue through the canonical loose fallback. Deferral alone does not claim
  that the canonical file's bytes verify: production loose reads already trust
  this content-addressed path, and the packer makes no metadata or filesystem
  change that would elevate that trust. Backup still independently hashes it.
- A larger noncanonical legacy candidate cannot merely be skipped: production
  reads derive only the canonical path. Copy it to a canonical temp file with a
  fixed buffer while streaming SHA-256, fsync and close the temp, publish it
  without replacing an existing destination, and sync the canonical parent
  where the platform supports directory fsync (kit's Windows directory sync
  is intentionally a no-op). Only after publication does a transaction
  canonicalize every matching DB path; only after that commit may the legacy
  source be removed best-effort. A hash mismatch removes the temp and tries the
  next recorded path. If the canonical destination already exists,
  streaming-verify and reuse it only when its hash matches; never replace it or
  delete the verified legacy source when destination validation/publication
  fails. A crash or DB failure after publication can leave a redundant legacy
  file, but cannot make the verified canonical copy unreadable. Future runs see
  the canonical oversized file and do not re-hash it merely to defer packing.
  If legacy deletion fails after the DB update, the final sweep retries it. A
  referenced,
  unindexed noncanonical hash-named file is redundant only after the canonical
  file streaming-verifies, then it is removed best-effort.
- Orphan reconciliation inspects footer `RawLen` before any production or
  orphan blob read. Before verifying an existing indexed copy, the blob store
  also opens the pack footer, finds the same blob ID, requires the DB entry to
  match that footer entry, and bounds both authoritative `RawLen` and
  `StoredLen`. If any referenced entry requiring redundancy verification,
  adoption, or repointing exceeds the ceiling, preserve and defer the entire
  orphan under the existing all-or-nothing rule; do not materialize the entry
  or classify the pack as corrupt/unreadable.
- During the final loose sweep, load each indexed hash's `raw_len` rather than
  only an indexed-hash set. If either indexed length or the loose candidate's
  stat size exceeds the ceiling, never use ordinary `Store.Open`; use the same
  footer-cross-checked bounded packed-read guard. An oversized authoritative
  footer entry instead takes the streaming loose-recovery path. A verified
  loose candidate is canonicalized if necessary and its index mapping is
  dropped so production reads use bounded-memory loose storage; the old pack
  bytes become ordinary dead space. If the loose candidate fails verification,
  retain both it and the mapping and fail closed as today.
- Pack usage accounting includes the largest referenced entry's raw length.
  A partially-live source pack with any live entry above 64 MiB is excluded
  before repack budget accounting, counted and warned as deferred, and left
  entirely authoritative. This prevents one old oversized pack from consuming
  the daily budget and starving later eligible packs. Oversized dead entries
  do not block rewriting the remaining small live entries, and a zero-live pack
  still retires without reading any blob.
- The ceiling is independent of the 256 MiB aggregate automatic budget. Only
  eligible blobs/source packs consume that budget, and explicit commands do
  not bypass the ceiling.

No new database state marks a deferral. A later maintenance run re-evaluates
the loose blob or source pack, so a future streaming implementation can lift
the constant and pick up every deferred item without migration. This preserves
availability and crash safety at the cost of leaving a small number of large
loose files or unreclaimed sparse packs in place. Large-file count is not the
Windows/NAS bottleneck this design targets; the benefit comes primarily from
packing the much more numerous small files.

Maintenance opens a pack once and retains that exact file descriptor from
preflight through every entry read. Msgvault's parser is deliberately pinned to
the stable unencrypted plain-v1 wire layout instead of following kit's mutable
current-version reader. It validates the fixed header and 40-byte trailer,
container and footer lengths, footer checksum, entry count, entry spans and
flags, and unique blob IDs before exposing entries. It rejects a container
above 128 MiB, a footer above 8 MiB, or more than 100,000 entries before a large
allocation. The retained descriptor prevents a pathname replacement between
preflight and read; bounded zstd decode caps output and window memory to the
authoritative entry length, then verifies CRC32C, decoded length, and SHA-256.

Normal production packs target 32 MiB and can cross that target by at most one
ceiling-eligible 64 MiB append; the remaining headroom covers the bounded
footer. Packer and repacker seal before a new append would exceed 100,000
entries, so they cannot create a pack their own maintenance reader later
defers. An oversized orphan or recorded repack source remains preserved, is
excluded before budget charging, and is reported with the other oversized pack
deferrals.

Maintenance reads of packed content cross-check mutable DB offsets and lengths
against the matching retained-reader footer entry before allocation, then
reject either authoritative `RawLen` or `StoredLen` above the ceiling. Store
scans validate offsets/lengths and BIGINT flag/CRC ranges before narrowing them
to Go integer types. The bounded blob-store operation returns its verified byte
slice directly; repack does not make a second `io.ReadAll` copy. Repack uses it
even after selection has screened DB aggregates, so corrupt metadata cannot
turn the accounting guard into a large allocation.

Deferral is operator-visible. `packer.Stats` exposes
`BlobsDeferredOversized` and `PacksDeferredOversized`; `repacker.Stats` exposes
`PacksDeferredOversized`. Structured warnings identify the hash or pack ID,
actual/largest raw size, 64 MiB limit, and (for an orphan) the number of
withheld referenced candidates using literal keys `hash`, `pack`, `raw_bytes`,
`max_raw_bytes`, and `withheld_entries` where applicable. Blob counts are once
per normalized distinct hash, not once per recorded case alias or candidate.
Explicit output emits only nonzero deferrals: large canonical blobs are named
as left loose, packer's oversized orphans are named as deferred untouched, and
repack's oversized authoritative source packs are named as deferred. Automatic
summaries always include the counters. On a non-cancellation error, automatic
maintenance logs an INFO `progress` summary (including committed work,
deferrals, and budget state) before WARN; only success is labeled `complete`.
A size exactly equal to 64 MiB is eligible; 64 MiB + 1 byte is deferred.

### GC and repack

- `remove-account` orphan sweep: loose orphans are deleted as today; packed
  orphans lose their `attachment_pack_index` rows in the same transaction as
  the source cascade. The production blob resolver refuses hashes with no
  surviving attachment row even if loose bytes remain, and orphan
  reconciliation adopts only referenced hashes, so later cleanup cannot
  resurrect logically deleted content.
- Repack: rewrite a pack when its live fraction (live index rows vs.
  `attachment_packs.entry_count`) falls below 50%, with hysteresis to avoid
  churn — only packs older than 24 h (`attachment_packs.created_at`) and
  with at least 8 MiB of dead stored bytes
  (`attachment_packs.stored_bytes` minus the sum of live `stored_len`).
  A live entry means an index row whose hash is still attachment-referenced;
  stale rows are repaired before selection, and accounting/enumeration/CAS use
  the referenced rows as defense in depth. All accounting comes from the
  database without opening packs. Selection is deterministic by creation time
  then pack ID. Every zero-live pack retires before fallible live-byte
  rewriting, so corrupt sparse content cannot block no-read reclamation. These
  metadata phases and guarded cleanup
  use context-aware maintenance transactions so PostgreSQL disables its
  statement timeout without losing cancellation.

  Automatic maintenance isolates partially-live source packs and applies its
  budget dynamically. It walks eligible packs in deterministic order while
  successfully swapped raw bytes remain below the budget; a source consumes
  budget only after its exact-set swap commits. A content-specific missing,
  corrupt, length-mismatched, or hash-mismatched source is recorded, skipped,
  and does not prevent attempting the next candidate. Writer creation, append
  I/O, seal, database, cancellation, and other systemic failures stop the run.
  Isolated content errors are joined and returned after later candidates run so
  the daily job still records failure. The automatic coordinator emits the
  progress/deferral INFO summary before its existing aggregate warning, so
  successful sibling work remains observable on a failed job. This prevents a
  corrupt oldest pack from consuming every daily budget before useful work
  starts. Explicit repack
  remains fail-fast after the zero-live pass. For each rewrite, copy live blobs
  to new target-sized packs.
  The production read verifies CRC, decoding, and SHA-256 before append; the
  new writer independently derives the same blob ID, which repack requires to
  match. Seal every replacement before changing metadata, then commit one
  exact-set compare-and-swap transaction that:

  1. inserts every new pack record;
  2. moves each expected live mapping from its exact old pack, requiring one
     affected row per blob;
  3. rejects omitted or newly added referenced mappings from that source pack;
     and
  4. retains the old pack record until physical retirement succeeds.

  A failure before the swap leaves old mappings/files authoritative; already
  sealed replacements are safe orphans for reference-aware reconciliation. A
  crash after the swap leaves truthful zero-live old inventory for the next
  cleanup pass.

  Keep the old `attachment_packs` rows until the daemon's shared blob store
  has waited for active reads, closed cached readers, and deleted the old
  files under its reader-cache mutex; then delete the now-empty records.
  Readers holding a just-stale index row either finish before retirement or
  hit the pack-open retry rule above and find the new mapping. Zero-live packs
  bypass the age/dead-byte thresholds. Bounded repack runs after
  `remove-account` and on the daemon maintenance schedule; explicit
  `msgvault repack-attachments` is unbounded. Here "bounded" applies to live
  payload bytes rewritten, not total metadata work: reference repair and usage
  accounting inspect the pack inventory, and every zero-live pack remains
  eligible because reclaiming it requires no blob rewrite.

  After a successful swap, retirement attempts every old pack even if one
  reader close or file deletion fails. Each failure remains loud and retains
  that zero-live record/file for retry; successful siblings are not held
  hostage. Guarded record deletion requires no live mapping to name the pack.

  TUI, MCP, and exports remain daemon-backed even with `--local`. Backup is the
  supported independent reader: kit releases the freeze gate after pinning its
  database snapshot and before attachment capture, so its short-lived blob
  store can overlap repack and keep an old pack handle open. On Windows that
  handle can make old-file deletion fail; the truthful zero-live record/file is
  retained until a later retry after backup closes. On Unix an already-open
  handle remains usable after unlink while new opens follow the replacement
  mapping. Capture either returns verified content or fails loudly and is
  retryable, including the existing case where a concurrent logical deletion
  makes a pinned-snapshot-only reference unavailable.

With phase 2c integrated, `remove-account` no longer requires unpacking first.
The source cascade and deletion of its unique packed mappings commit together;
mappings shared by another source across either hash column remain live. The
parent daemon then attempts bounded repack as best-effort physical cleanup,
while the existing loose-file sweep continues to cover content and thumbnail
paths. A repack failure delays byte reclamation but cannot resurrect the
logically deleted hashes or roll back a successful account removal.

### Downgrade: `msgvault unpack-attachments`

Prunes unreferenced mappings, then streams every attachment-referenced live
`attachment_pack_index` blob back to a canonical loose file, verifies hashes,
drops the index and `attachment_packs` rows, and
deletes the pack files. Footer entries with no live index row are dead and
are not restored; a zero-live pack is dropped without opening it, so a dead
corrupt pack retained after orphan rescue cannot block downgrade or resurrect
deleted content. Because the packer canonicalizes `storage_path`/
`thumbnail_path` rows at pack time, canonical output paths are always
consistent with the DB. Old binaries cannot read packs, so this is the escape
hatch before any downgrade.

Unpack applies the same container/footer/entry-count preflight and 64 MiB
raw/stored entry ceiling before writing the first loose blob from each live
pack. A pre-amendment pack outside those bounds fails safely with its complete
index, record, and file retained for a future streaming-capable release;
zero-live packs still delete without opening their footer. Packs completed
earlier in the command remain independently and consistently unpacked. Every
restored canonical file is hash-verified through a no-follow descriptor and
the final file descriptor is flushed before pack authority is dropped. Its
parent directory, and the attachments base when a hash directory is created,
are synced where the platform supports directory fsync; kit's Windows
directory sync is intentionally a no-op. Windows opens the final component as
a reparse point rather than following it, rejects reparse objects, and compares
pre-open, descriptor, and post-open filesystem identity before accepting or
syncing an existing destination.

The local command holds the archive's `daemon.lock` lease for the full run,
from the live-daemon check through store cleanup and pack deletion. This closes
the PostgreSQL race in which a daemon could start after a one-shot runtime-file
preflight; SQLite additionally retains its ordinary exclusive writer lock.

### Backup coordination (release-blocking)

Backup capture historically read attachment bytes from loose paths:
`backupapp.ContentInfo` hands DB-recorded paths to the kit engine, which
opens files under the content dir (`kit/backup/create.go`). Once blobs are
packed those paths do not exist. **The kit hook has landed** (kit branch
`backup-content-source`, targeting v0.4.0): `backup.CreateOptions` gains an
optional `ContentSource` — `Open(ctx, ContentRef) (io.ReadCloser, error)` —
called from concurrent capture workers; when set, the content dir is ignored
and the engine still hash-verifies and size-caps every blob. msgvault
implements it in `internal/backupapp/content_source.go` (wired in
`runBackupCreateLocal` over a read-only store + blob store), shipping in the
same release as the packer. The blob store's loose fallback opens only the canonical
`<aa>/<hash>` path, so msgvault's backup reader must additionally fall back
to the DB-recorded `storage_path`/`thumbnail_path` for blobs that are
neither packed nor canonical — legacy noncanonical rows (e.g. SyncTech's
`synctech-sms/` namespace) remain backup-readable until the packer
canonicalizes them. Restore materializes loose canonical files only —
production pack files are never part of a snapshot — so `msgvault backup
restore` clears `attachment_pack_index`/`attachment_packs` in the restored DB
immediately after a successful restore
(`Store.ClearAttachmentPackMetadata`). Without the clear, stale index rows
would break packed-blob reads (the blob store deliberately does not fall back
to loose after an index hit) and, far worse, a later `pack-attachments` run
would skip those "already indexed" hashes while its sweep deleted their
restored loose files. The clear yields a genuinely fully-unpacked vault that
the packer re-packs later. The cleanup checks whether the two pack tables
exist and does not run schema initialization, so restoring a pre-pack snapshot
cannot trigger unrelated migrations after kit has already verified the
restore. As defense in depth, the packer drops missing or malformed pack
records before orphan reconciliation, packing, or sweeping.

Follow-up (out of scope here): teach backup capture to adopt sealed
production packs wholesale — copy pack files it does not have and merge
their entries into the repo index, skipping per-blob re-reads.

## Testing

- `internal/blobstore` unit tests: index hit, loose fallback, ENOENT retry
  race rule, CRC-corruption rejection, LRU behavior.
- Packer crash injection at each ordering boundary: sealed-pack-no-index
  (adoption), index-no-delete (idempotent re-sweep), mid-seal abort
  (staging file cleanup), missing-recorded-pack plus orphan-pack recovery,
  corrupt indexed pack plus readable orphan-pack rescue, corrupt packed copy
  plus readable loose-copy preservation, duplicate recorded paths with a valid
  fallback, cancellation before the first recovery mutation, stale mapping
  repair after Teams inline replacement, and retryable unreferenced loose-file
  cleanup. Required memory-bound coverage includes exact-64-MiB acceptance and
  64-MiB-plus-one deferral for automatic and explicit commands, bounded loose
  reads, literal counter/log fields and unique-hash counting, later eligible
  progress, streaming canonicalization of oversized legacy paths, existing
  canonical-destination validation, and crash/DB-failure injection at the
  publish-to-DB and DB-to-legacy-delete boundaries. Oversized orphan
  all-or-nothing deferral and indexed-loose recovery must avoid ordinary packed
  reads; forged DB raw/stored lengths must fail the footer cross-check before
  allocation.
- Canonicalization: SyncTech-style namespaced rows become readable and
  canonical after packing.
- Repack: referenced-live accounting despite stale index rows, live-blob
  preservation, threshold + hysteresis, transactional index swap, and explicit
  SHA-verified copy semantics. A partially-live pack with an oversized live
  entry is deferred before selection/budget accounting, while oversized dead
  entries and zero-live packs do not block reclamation. Required tests also pin
  oversized counters/output, a corrupt oldest source large enough to fill the
  automatic budget followed by a successful small source, aggregate content
  error reporting, systemic-error fail-fast behavior, zero-live-first progress,
  and explicit fail-fast behavior. Race coverage includes
  backup's independent reader cache: Windows retains an old zero-live pack when
  the handle blocks deletion and removes it on the next run after close; Unix
  open handles remain readable after unlink while new lookups use the
  replacement mapping.
- `unpack-attachments` round-trip: pack -> unpack -> byte-identical loose
  tree, index empty.
- `remove-account` GC over mixed loose/packed orphans.
- End-to-end: import -> pack -> read via API/MCP/export -> backup ->
  restore -> read again.
- PostgreSQL backend: index table migrations and packer transaction
  semantics under `MSGVAULT_TEST_DB`; thumbnail hash/path supporting indexes
  build through the maintenance timeout hatch on existing archives.

## Real-archive hardening (2026-07-09)

The reviewed branch head `f75a7854` was exercised against isolated copy-on-write
clones of a real SQLite archive. The installed binary and live archive were not
modified. The dataset contained 2,483,627 messages, 208,532 attachment rows,
and 46,060 distinct local blobs totaling 8,193,617,238 bytes.

- Baseline: all 46,060 loose files independently SHA-256 verified against their
  content-addressed names, and that hash set exactly matched every local content
  and thumbnail reference in the database.
- Loose backup: the initial snapshot completed in 74.4 seconds, added 44.3 GiB
  in 1,346 repository packs, and captured every attachment at about 392 MiB/s.
  Full verification read 74.5 GiB across 63,114 blobs with zero problems.
- Restore: the fully loose restore completed in 919.1 seconds, including a
  36-second database materialization, 205-second attachment materialization,
  and 11m15s database integrity proof. It restored 46,060 byte-identical loose
  blobs and cleared both production pack tables.
- Pack: explicit packing converted all 46,060 blobs in 40.7 seconds into 202
  packs with zero loose files. Stored entry bytes were 7,135,743,828, a 12.91%
  reduction from the verified loose payload.
- Packed backup: the next incremental snapshot completed in 19.5 seconds and
  added only 4.8 MiB because attachment content deduplicated. Its attachment
  capture stage read all 46,060 production-packed blobs at about 815 MiB/s,
  2.08x the initial loose capture rate. Later packed snapshots reached about
  844 MiB/s.
- Read surfaces: small, approximately 1 MiB, and approximately 23 MiB blobs
  verified through single-file CLI export and the raw HTTP API; a five-file
  directory export matched its expected hash multiset; daemon-backed MCP
  `get_attachment` returned hash-identical bytes.
- Memory ceiling: the largest blob in this archive was approximately 23 MiB,
  below the 64 MiB in-memory maintenance ceiling. Oversized behavior is
  therefore covered by implemented synthetic bounded-read, footer-preflight,
  deferral, and repack-selection tests rather than this real dataset. No
  greater-than-64-MiB real-archive blob was exercised.
- Crash recovery: the daemon was sent `SIGKILL` with 135 sealed packs, one
  staging file, 26,513 indexed blobs, and 19,547 loose blobs. Restart removed
  staging and packed exactly the remaining 19,547 blobs in 12.1 seconds,
  yielding the original 46,060-hash manifest with 202 records/files and no
  loose content.
- Downgrade/upgrade: unpack restored and verified all 46,060 loose blobs in
  18.8 seconds, removed every pack/index record and pack file, and reproduced
  the baseline hash manifest. Re-packing completed in 32.8 seconds and returned
  to 202 packs.
- Physical GC/repack: on another clone, one production pack was made zero-live
  and one aged pack was reduced to 53/134 live entries. Repack pruned 242 stale
  mappings, rewrote the 53 survivors, removed both old packs, rejected deleted
  hashes through the API, and preserved the exact 45,818-hash survivor set.
  A backup of that result captured every survivor and full verification read
  74.4 GiB across 62,887 blobs with zero problems.

The real archive contained no local thumbnail hashes, so thumbnail behavior is
covered by automated SQLite/PostgreSQL tests rather than this dataset. The
hardening host was macOS/APFS; the real Windows sharing-violation behavior is
covered by the passing Windows CI job, but full-archive Windows performance
remains unmeasured. The 205-second loose attachment restore is the concrete
baseline for the pack-native restore optimization tracked by issue #466.

## Delivery order

1. Prep: unify WhatsApp/SyncTech/FB Messenger writes onto
   `StoreAttachmentFile`.
2. `internal/blobstore` + index migration + read-path switch (inert until
   packs exist; no regression for currently readable blobs — legacy
   noncanonical SyncTech rows stay unreadable, as today, until the packer
   canonicalizes them in step 4).
3. kit change: content reader hook for backup capture.
4. Packer + canonicalization + `pack-attachments` / `unpack-attachments`
   commands + auto-run hooks. `pack-attachments` is a standard dual-mode
   maintenance command (daemon proxy under the operation gate, or CLI-local
   under `db.write.lock`); it is safe in both modes because it only creates
   new ULID-named packs and deletes loose files, which the daemon never
   holds open. `unpack-attachments` deletes pack files that a running
   daemon's blob store holds open (blocks deletion on Windows), so it is
   local-only: the command holds `daemon.lock` for its entire execution and a
   live-daemon runtime preflight rejects unpack on all backends (directing the
   user to `msgvault serve stop`); on SQLite the `db.write.lock` additionally
   guarantees exclusivity against any other writer. When `[remote].url` is
   active, unpack refuses before opening local storage; the operator must run
   it on the archive host, or pass `--local` to select the client machine's
   local archive intentionally.

   Finding (2026-07-09): the originally planned switch of the MCP local
   reader (`internal/mcp/handlers.go` readAttachmentFile) and the TUI local
   export path (`internal/tui/actions.go`) to the blob store is not needed.
   Both commands are always daemon-backed — `--local` selects the local
   daemon, not direct file access — and always install a daemon-client
   AttachmentReader; the daemon side serves from the blob store. The
   loose-file fallbacks in those files are reachable only from tests and
   direct embedding, and have no store handle from which to build a blob
   store. Revisit only if a genuinely daemon-less MCP/TUI mode is added.
5. Bounded auto-pack hooks, reference-aware packed GC/repack, and
   `remove-account` integration. The daemon ownership, automatic policy,
   memory ceiling, reader-retirement, and crash-ordering contracts are recorded
   above.
