# Packed Attachment Storage

Design for storing attachment content in kit CAS pack files instead of loose
content-addressed files. Written 2026-07-09; status: delivery steps 1-4 (see
Delivery order below) implemented on this branch (packer, crash
reconciliation, `pack-attachments`/`unpack-attachments`, backup
ContentSource), except the auto-run hooks in step 4; those hooks, GC/repack,
and `remove-account` integration (step 5) remain.

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

1. **Uniform architecture** — all attachment bytes end up in sealed,
   immutable pack files; loose files exist only transiently.
2. **Backup synergy** — production packs use the exact kit pack format,
   blob IDs, extension, target size, and shard layout that `msgvault backup`
   uses, so a future release can teach backup to adopt production packs
   wholesale instead of re-reading and re-packing every blob.

Non-goals for this design: at-rest encryption (kit supports it; msgvault
does not use it yet), backup pack adoption itself (follow-up), disk-space
reduction as a primary objective.

## Current state (verified 2026-07-09)

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
- kit pack format (`go.kenn.io/kit/pack`, v0.3.0): sealed-immutable packs,
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
seals it (or adopts it during crash reconciliation). Because
`attachment_pack_index` holds only live rows — GC deletes rows for dead
blobs — the index alone cannot tell how much of a pack is dead; dead bytes =
`stored_bytes` minus the sum of the pack's live index rows. `created_at`
feeds the repack age hysteresis without stat-ing pack files.

BIGINT, not INTEGER: PostgreSQL `INTEGER` is 4-byte signed, too small for a
`uint32` CRC32C and for raw/stored lengths up to `pack.MaxRawLen` (4 GiB).
SQLite integers are 8-byte regardless.

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

### Blob store (`internal/blobstore`)

`Store.Open(hash) (io.ReadSeekCloser, int64, error)`:

1. Look up `attachment_pack_index` by hash.
2. Hit: read via a small LRU cache of open `pack.Reader`s (each caches its
   entry map); return a `bytes.Reader` over the verified blob. Validate the
   DB-sourced `pack_id` with `pack.IsValidPackID` before building the
   `packs/<id[:2]>/<id>.mvpack` path — the ID comes from mutable DB state
   and is joined straight into a filesystem path.
3. Miss: `os.Open` the canonical loose path.
4. **Race rules** (both resolved by retrying the index lookup once before
   failing):
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
gate (like other maintenance ops), so it can never overlap a backup freeze
window.

1. Enumerate loose blobs from the DB: distinct local (non-URL)
   `content_hash` and `thumbnail_hash` values without an index row, locating
   files by every distinct DB-recorded `storage_path`/`thumbnail_path` (which
   finds noncanonical legacy paths, not just canonical ones). If duplicate
   rows record different paths for one hash, candidates are tried until one
   verifies; one missing or corrupt candidate cannot pin a valid copy. Rows
   with empty or URL storage paths are skipped, as are hashes with no readable,
   verified candidate on disk (logged, left for a future backfill).
2. Append blobs to a `pack.Writer` (32 MB target), seal each pack
   (durable, atomic publish under `packs/<id[:2]>/`).
3. In one DB transaction per sealed pack: insert the `attachment_packs`
   row and the index rows, and **canonicalize** any noncanonical local
   `storage_path`/`thumbnail_path` rows for those hashes to `<aa>/<hash>`.
4. Delete the loose files (including noncanonical originals).

Crash safety at each boundary:

- After 2, before 3: a sealed pack with no index rows. On the next run the
  packer scans `packs/` for unreferenced packs, reads their footers, and
  adopts entries for hashes still unindexed (verifying blob hashes), or
  deletes the pack if fully redundant.
- After 3, before 4: loose files linger harmlessly; the next sweep removes
  an indexed loose file only after opening the packed copy through the
  production blob store. Corrupt metadata or pack bytes therefore preserve
  the loose recovery copy.

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

Cancellation is checked before the pack directory is created and throughout
staging cleanup, dangling-record repair, orphan reconciliation, packing, and
the final sweep. A context canceled before `Run` therefore causes no filesystem
or metadata mutation; mid-run cancellation stops at the next recovery/blob
boundary without violating the same crash-ordering rules.

### GC and repack

- `remove-account` orphan sweep: loose orphans are deleted as today; packed
  orphans just lose their `attachment_pack_index` rows.
- Repack: rewrite a pack when its live fraction (live index rows vs.
  `attachment_packs.entry_count`) falls below 50%, with hysteresis to avoid
  churn — only packs older than 24 h (`attachment_packs.created_at`) and
  with at least 8 MiB of dead stored bytes
  (`attachment_packs.stored_bytes` minus the sum of live `stored_len`).
  All accounting comes from the two tables without opening packs. Copy live
  blobs to a new pack, swap index rows and the `attachment_packs` rows
  transactionally, delete the old pack file. Readers holding a just-stale
  index row survive the deletion via the pack-open retry rule above. Runs
  after `remove-account` and via `msgvault repack-attachments`.

Until that step-5 integration lands, `remove-account` refuses before its
cascade when the selected source owns any unique packed blobs, and directs the
operator to stop the daemon and run `unpack-attachments` first. Packed blobs
that remain referenced by another source do not block account removal. This
keeps the phase-2b branch independently safe without folding physical repack
into the account-removal transaction. After unpack, the existing orphan-file
sweep covers both loose content and thumbnail paths, with sharing checked
across both hash columns.

### Downgrade: `msgvault unpack-attachments`

Streams every live `attachment_pack_index` blob back to a canonical loose
file, verifies hashes, drops the index and `attachment_packs` rows, and
deletes the pack files. Footer entries with no live index row are dead and
are not restored; a zero-live pack is dropped without opening it, so a dead
corrupt pack retained after orphan rescue cannot block downgrade or resurrect
deleted content. Because the packer canonicalizes `storage_path`/
`thumbnail_path` rows at pack time, canonical output paths are always
consistent with the DB. Old binaries cannot read packs, so this is the escape
hatch before any downgrade.

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
  fallback, cancellation before the first recovery mutation.
- Canonicalization: SyncTech-style namespaced rows become readable and
  canonical after packing.
- Repack: live-blob preservation, threshold + hysteresis, transactional
  index swap.
- `unpack-attachments` round-trip: pack -> unpack -> byte-identical loose
  tree, index empty.
- `remove-account` GC over mixed loose/packed orphans.
- End-to-end: import -> pack -> read via API/MCP/export -> backup ->
  restore -> read again.
- PostgreSQL backend: index table migrations and packer transaction
  semantics under `MSGVAULT_TEST_DB`; thumbnail hash/path supporting indexes
  build through the maintenance timeout hatch on existing archives.

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
   local-only: a live-daemon runtime preflight rejects unpack on all
   backends (directing the user to `msgvault serve stop`); on SQLite the
   `db.write.lock` additionally guarantees exclusivity against any other
   writer. When `[remote].url` is active, unpack refuses before opening local
   storage; the operator must run it on the archive host, or pass `--local`
   to select the client machine's local archive intentionally.

   Finding (2026-07-09): the originally planned switch of the MCP local
   reader (`internal/mcp/handlers.go` readAttachmentFile) and the TUI local
   export path (`internal/tui/actions.go`) to the blob store is not needed.
   Both commands are always daemon-backed — `--local` selects the local
   daemon, not direct file access — and always install a daemon-client
   AttachmentReader; the daemon side serves from the blob store. The
   loose-file fallbacks in those files are reachable only from tests and
   direct embedding, and have no store handle from which to build a blob
   store. Revisit only if a genuinely daemon-less MCP/TUI mode is added.
5. GC/repack + `remove-account` integration.
