# Kit Packed-CAS Extraction

Design for extracting msgvault's production packed-attachment storage into a
reusable `go.kenn.io/kit/packstore` engine for msgvault and the downstream document archive. Written
2026-07-10; status: Kit extraction and msgvault migration implemented on
review branches, with the macOS real-archive gate complete. The final tagged
Kit release pin and latest native CI are still required before msgvault merges.

The merged packed-attachment implementation remains the behavioral reference.
This extraction is not permission to change msgvault's storage format, paths,
maintenance limits, command behavior, or Windows file lifecycle. Kit must first
operate the existing store without conversion. Only then may the downstream document archive adopt it.

Once the Kit package exists, its package documentation and tests become the
authoritative engine contract. This document remains the cross-repository
migration and compatibility contract.

## Implementation evidence (2026-07-10)

Msgvault currently pins the reviewed Kit head `7d122d10693b` through the
interim pseudo-version `v0.5.1-0.20260711023217-7d122d10693b`. The pin must be
replaced by the normal tagged Kit release before the msgvault PR is merge-ready.

The extracted engine was exercised on isolated copies of the same archive used
for the original packed-storage hardening: 46,060 distinct blobs totaling
8,193,617,238 raw bytes. The installed binary and live archive were not
modified.

- Packing preserved the catalog's first-seen locality order and produced the
  same 202-pack layout as the pre-extraction engine. It completed in 36.6
  seconds versus 38.6 seconds before extraction.
- The extracted layout stored 938,904 additional bytes (0.013%) because blobs
  smaller than zstd's 1 KiB minimum window now remain raw. This deliberate,
  format-compatible difference lets older bounded readers unpack newly written
  packs safely.
- A steady packed backup completed in 17.6 seconds at about 880 MiB/s versus
  18.1 seconds at about 872 MiB/s before extraction. A hash-sorting regression
  found during hardening had caused 45,806 pack switches in backup order; Kit
  now treats catalog order as a physical-locality hint, restoring 202 switches.
- CLI export, raw HTTP API, and MCP reads returned hash-identical bytes. The
  new engine opened pre-extraction packs, and the untouched pre-extraction
  binary opened and fully unpacked the corrected Kit packs.
- Abrupt termination left 11,730 indexed blobs, 34,330 loose blobs, and one
  staging file. Restart reconciled sealed orphans, removed staging, and restored
  the exact 46,060-hash packed inventory.
- Three backup snapshots verified 63,128 objects / 74.5 GiB with zero problems.
  Restore completed in 715.3 seconds, restored all blobs loose, cleared both
  pack tables, and passed SQLite integrity plus manifest-stat proofs.
- Independent SHA-256 manifests matched after legacy unpack, backup restore,
  and the final re-pack. The final state contained 46,060 indexed blobs, 202
  pack records/files, and no loose content.

The real-archive host was macOS/APFS. Native Windows remains a mandatory CI
gate for the latest Kit and msgvault heads; full-archive Windows performance is
still unmeasured.

## Motivation

msgvault now stores attachment content in a mixed content-addressed store:
fresh and oversized content can remain loose, while maintenance moves eligible
content into sealed immutable Kit packs. The implementation includes the hard
parts that a second application should not recreate:

- representation-independent reads with authority-race retries;
- bounded pack parsing and blob verification;
- reader caching and Windows-safe retirement;
- crash reconciliation, quarantine, and loose-file sweeping;
- transactional pack adoption and replacement;
- zero-live retirement and source-isolated repacking;
- fully loose unpack and restore recovery paths; and
- automatic maintenance budgets and observability.

The downstream document archive has the same small-file problem and already uses SHA-256-addressed loose
content. A shared engine gives both applications the same recovery behavior and
creates a foundation for faster pack-native restore, which is particularly
important on Windows. Copying msgvault's internal packages would instead leave
two subtly different state machines around the same pack format.

## Decisions

1. Kit owns the complete physical mixed-CAS engine, including loose writes,
   durability-capable physical removal, and the fully durable writes required
   by authority transitions. It is not merely a packed reader or a set of
   maintenance helpers.
2. Applications own catalog authority, database schema, product reachability,
   retention, scheduling, commands, and backup inventory.
3. Applications expose that authority through narrow semantic interfaces. Kit
   owns no sidecar database and receives no raw application transaction.
4. The first extraction preserves msgvault's 64 MiB maintenance ceiling.
   Oversized loose blobs remain loose. Verified streaming is a subsequent
   format-compatible Kit change required before the downstream document archive relies on packing large
   documents.
5. Msgvault migrates to the Kit engine and passes Windows and real-archive
   compatibility gates before downstream document archive adoption begins.
6. The loose layout, pack layout, `.mvpack` format, footer IDs, and database
   mappings remain compatible in both directions. Extraction requires no data
   migration.

## Non-goals

- Changing the stable Kit pack format or compression policy.
- Moving msgvault or the downstream document archive's schema into Kit.
- Making Kit decide whether application content should be retained.
- Increasing the automatic maintenance threshold during extraction.
- Implementing pack-native backup restore as part of extraction. That remains
  msgvault issue #466 and should later build on the shared engine.
- Requiring an existing loose vault in the downstream document archive to migrate immediately.

## Pre-extraction implementation boundary

Before this extraction, the merged msgvault implementation was distributed
across these areas. The package paths in this table are historical; the
physical engine now lives in Kit's `packstore` package.

| Pre-extraction code | Responsibility after extraction |
| --- | --- |
| `internal/blobstore` | Kit mixed reads, bounded reads, cache, preflight, and retirement |
| `internal/packer` | Kit reconciliation, packing, sweep, and unpack engine |
| `internal/repacker` | Kit physical GC and replacement engine |
| `internal/export/store_attachment.go` | Kit canonical loose-CAS operations and durability modes; msgvault MIME adaptation stays local |
| `internal/store/packs.go` | Msgvault catalog adapter and schema-specific transactions |
| `internal/store/repack.go` | Msgvault repack accounting and exact-set CAS adapter |
| `cmd/msgvault/cmd/attachment_maintenance.go` | Msgvault scheduling, command policy, logging, and budgets |
| `internal/backupapp/content_source.go` | Msgvault backup inventory adapter over the Kit store |

The split is behavioral rather than a mechanical package move. Code that
mentions attachments, sources, messages, thumbnails, SQL dialects, or msgvault
logging stays in msgvault. Code that manipulates canonical CAS paths, pack
containers, readers, and crash-ordered physical transitions moves to Kit.

## Ownership boundary

### Kit owns

- Canonical loose and pack paths and validation of untrusted hashes and pack
  IDs.
- Loose publication, dedup validation, explicit durability modes, and physical
  removal.
- Mixed loose-and-packed reads and bounded maintenance reads.
- The bounded reader cache, eviction, close, and pack retirement.
- Staging cleanup, dangling-record repair orchestration, orphan reconciliation,
  loose packing, verified sweeping, unpacking, and repacking.
- Filesystem/catalog ordering and all crash-recovery state transitions.
- Cancellation checks, hard safety limits, soft run budgets, and operation
  statistics.
- Classification of corrupt, missing, oversized, quarantined, and retryable
  physical-cleanup outcomes.
- Coordination between foreground physical writes and exclusive maintenance.

### Applications own

- Database schema, migrations, dialects, and SQL queries.
- Catalog membership: whether a content hash is currently readable.
- Product reachability and the decision to revoke membership.
- Atomic pack-index changes and exact-set compare-and-swap.
- Product-specific normalization, retention, account removal, and deletion.
- Backup inventory and restore of application metadata.
- Daemon operation gates, scheduling, CLI/API behavior, logs, and metrics.

This boundary is required because msgvault and the downstream document archive do not have the same
meaning of "live." A filesystem or pack index cannot answer either product's
retention question.

## Package shape

The new package is provisionally `go.kenn.io/kit/packstore`, above the existing
low-level `go.kenn.io/kit/pack` package. It depends on the standard library and
Kit packages only.

It exposes capability-oriented surfaces rather than one large application
interface:

- `Store` provides loose writes with an explicit durability contract, mixed
  reads, bounded reads, existence checks, and shutdown of cached readers.
- `Maintainer` provides repair, reconciliation, packing, unpacking, cleanup,
  and repacking.
- `Resolver` is the minimal application capability required by ordinary reads.
- `Catalog` composes the snapshots and atomic mutations required by
  maintenance.
- `Coordinator` grants shared foreground-mutation leases and exclusive
  maintenance leases over one physical store.

Runtime consumers depend on `Store`; scheduled and explicit maintenance use
`Maintainer`; application store packages implement `Resolver` and `Catalog`.
The interfaces should be split further by capability where doing so lets read
consumers and focused tests avoid implementing unrelated maintenance methods.

Configuration supplies:

- the existing storage root and layout version;
- owned loose-staging strategies for same-directory or store-level temporary
  files, always on the destination filesystem;
- the resolver/catalog implementation;
- hard maintenance limits and an optional soft run budget;
- pack target size and clock where needed; and
- a structured event observer for logging and metrics.

Kit must not depend on `slog` configuration, msgvault stats structs, or the downstream document archive
telemetry. Typed outcomes and structured events let each application preserve
its current user-visible reporting.

### Catalog operations

The precise Go signatures belong in the implementation plan, but the catalog
must expose semantic operations equivalent to:

- resolve catalog membership and an optional current pack mapping for a hash;
- snapshot referenced canonical IDs, original aliases, and candidate legacy
  paths for packing and reconciliation;
- enumerate recorded packs, indexed entries, and per-pack usage;
- commit a sealed pack for the exact subset that remains eligible;
- adopt or repoint verified orphan entries atomically;
- prune mappings whose hashes are no longer catalog members;
- drop pack records whose canonical pack file is missing or unusable;
- compare-and-swap a source pack's exact current live-entry set to replacement
  mappings;
- remove a zero-live pack record only after physical retirement succeeds;
- drop mappings only after unpack has durably materialized all loose objects;
  and
- clear every pack mapping and record after a loose-only restore.

These are not generic CRUD calls. For example, msgvault's commit operation must
continue to merge case-equivalent attachment rows before canonicalizing hashes,
while the downstream document archive can use already-canonical `blobs` keys. Kit validates canonical
IDs and rejects duplicate footer IDs, but the adapter owns product row changes.

### Durable loose operations

Kit owns a streaming loose writer that computes SHA-256, optionally verifies an
expected hash and size, uses a private temp file on the destination filesystem,
atomically publishes the final path, and validates the resulting regular-file
identity. The configured operation preserves msgvault's existing
same-directory and attachment-root staging paths and the downstream document archive's owned
`blobs/tmp/` directory. Cleanup touches only the configured owned staging
namespace. The Windows identity/no-follow behavior currently tested in
msgvault moves with this code.

Dedup verification is also explicit. Msgvault preserves its current full hash,
size, type, and stable-identity validation. The downstream document archive preserves its current
regular-file and expected-size fast path, leaving same-size bit-rot detection to
`verify`. Maintenance recovery and any transition that will discard another
authoritative copy always require full hash verification. The implementation
must not silently make the downstream document archive re-read every duplicate or weaken msgvault's
existing validation.

Durability is explicit rather than an accidental platform side effect. The
initial API supports at least these two modes:

- **Atomic publication** preserves msgvault's ordinary ingest behavior: close
  and atomically publish verified bytes, without adding a per-attachment file
  and directory sync to an existing sync/import run.
- **Durable publication** syncs the completed final file and its parent
  directory where supported. Unpack, restore, canonical legacy migration, and
  the downstream document archive use this mode whenever a later metadata change will discard another
  authoritative copy.

Physical removal likewise distinguishes a durable unlink, required by
the downstream document archive's current loose-CAS contract, from best-effort reclamation whose failure
is retained and retried by pack maintenance. The implementation plan must make
the choice visible at every call site; no implicit default may change
msgvault's ingest or Windows performance during extraction.

Foreground publication and maintenance share a Kit-owned coordinator. An
application acquires a shared mutation lease that can span all physical writes
and the later application transaction that publishes their catalog metadata.
Maintenance acquires the exclusive lease. This matches msgvault's current
command-wide daemon operation gate instead of requiring an artificial database
transaction per blob. The downstream document archive can hold the same lease across one ingest
transaction. If publication ultimately fails, an unreferenced loose file is a
safe leak for later exclusive cleanup. Staging files are never content
candidates.

The daemon operation gate remains application-owned, because it also
serializes non-storage work. The lock order is always application operation
gate, then Kit lease. A successful ingest releases its shared Kit lease before
the best-effort automatic pack follow-up acquires the exclusive lease, while
the outer application gate may remain held. Code must never upgrade or acquire
the Kit lease recursively. Restore and migration code can use verified loose
writes under an exclusive maintenance/offline lease, but cannot bypass hash,
durability, or path checks.

The Kit coordinator is process-local and does not replace an application's
cross-process exclusion. Applications must prevent a second binary from
opening the same store for maintenance; msgvault retains its daemon/write lock
and live-daemon preflight, and the downstream document archive retains its exclusive vault lock.

## Three distinct kinds of liveness

The engine keeps three concepts separate.

### Catalog membership

Catalog membership answers: "May this hash be read?"

- In msgvault, at least one attachment content or thumbnail reference grants
  membership.
- In the downstream document archive, a row in `blobs` grants membership.

`Store.Open` checks this authority before exposing either representation. A
stale file or mapping cannot make revoked content readable.

### Product reachability

Product reachability answers: "Should membership be retained?"

- Msgvault derives it from attachment rows and source/account lifecycle.
- The downstream document archive currently derives it from node and version references; any future
  retention policy would remain part of this application-owned decision.

The application atomically rechecks its policy before revoking membership.
Kit receives the result and reclaims physical storage; it never infers product
reachability from files or index rows.

### Backup inventory

Backup inventory answers: "What must this backup capture?"

The application enumerates catalog content, and Kit opens its authoritative
physical representation. A temporarily unreachable blob in the downstream document archive remains in a
backup while its `blobs` row still exists. This avoids coupling backup policy to
pack accounting.

### Authoritative physical states

| Catalog state | Physical state | Meaning |
| --- | --- | --- |
| member, no mapping | loose file | Loose content is authoritative. |
| member, mapping | pack entry | Packed content is authoritative; a loose duplicate is reclaimable. |
| no member, mapping | any | Stale mapping; repair prunes it before sweep or repack. |
| no pack record | canonical pack file | Orphan; verify and atomically adopt or quarantine. |

Revoking membership and reclaiming bytes are separate. After an application
commits a policy-approved revocation, Kit removes loose content or leaves dead
packed bytes for later repack. A crash can leak bytes but cannot restore read
authority. Exclusive repair retries loose cleanup and prunes stale mappings.

## Read behavior

`Store.Open` validates the requested hash, then asks `Resolver` for membership
and an optional pack mapping in one logical lookup.

1. A non-member returns `fs.ErrNotExist` without consulting physical storage.
2. A member without a mapping opens the canonical loose path.
3. A member with a mapping reads through the bounded pack-reader cache.

Authority may move between lookup and file open. The reader resolves once more
when:

- loose open returns `fs.ErrNotExist`, because packing may have committed and
  removed the source; or
- the mapped pack file or footer entry is absent, because repack may have
  swapped mappings and retired the source.

The retry follows the new authority and is bounded to one re-resolution. It
does not hide corruption or turn genuine absence into an infinite retry loop.
The backup content source uses the same rule, eliminating the historical
capture race between an index miss and loose-file removal.

Ordinary and bounded reads retain separate safety contracts. Bounded reads
validate container, footer, entry-count, offset, length, flag, CRC, decompressed
size, and SHA-256 constraints before returning content. Maintenance never
propagates bytes into a fresh immutable pack without verifying SHA-256 against
the canonical blob ID.

## Crash-safe pack publication

Kit moves authority only after new bytes are durable:

1. Create a private staging pack in the canonical shard and append bounded,
   verified entries.
2. Seal and sync the pack, then validate the completed container.
3. Atomically rename it to the final sharded path and sync the directory.
4. Ask the catalog adapter to install the pack record and mappings atomically
   for the exact subset that is still eligible.
5. Remove loose sources. Cleanup failures are reported and retried.

A crash before step 4 leaves an orphan pack and the old loose authority. A
crash after step 4 leaves valid packed authority plus, at worst, redundant
loose bytes. The database never points at an unpublished pack.

Kit rejects duplicate normalized IDs before catalog commit or source removal.
The application adapter may coalesce legacy aliases transactionally, but the
pack footer itself contains each canonical ID at most once.

## Repair and orphan reconciliation

Maintenance begins with correctness work that is not charged to the automatic
byte budget:

- remove dead staging files under the owned pack staging path;
- drop records whose validated canonical pack path is missing;
- prune mappings for hashes that are no longer catalog members;
- reconcile canonical orphan pack files; and
- classify loose files for verified recovery or cleanup.

Reconciliation never adopts a pack found outside the canonical sharded path.
For each orphan it identifies entries that remain catalog members and verifies
the content required for adoption, repointing, or redundancy decisions.

Adoption is all-or-nothing for referenced entries. If any required referenced
entry fails verification, no mapping from that pack is installed. Kit emits a
distinct quarantine event with the pack ID and failed/withheld counts, leaves
the file in place, and retries on a later run. This can temporarily withhold a
valid entry beside a damaged one, but avoids partially adopting a pack whose
later repack could destroy the only remaining evidence for the damaged entry.

An unreadable or otherwise unverifiable orphan is never deleted merely because
its footer IDs appear redundant. Deletion requires positive verification of
the redundancy condition. Recovery deliberately favors retained bytes over
space reclamation.

The loose sweep uses one catalog snapshot and normalized identity set. An
indexed loose duplicate is removed only after the authoritative packed copy
opens and verifies. If packed authority is damaged but a valid loose copy
exists, repair can atomically drop or repoint the bad mapping and restore loose
readability. Unreferenced loose cleanup runs only under the publication/
maintenance coordinator, so it cannot race a foreground catalog transaction.

## Repack

Repack first prunes stale mappings, accounts live bytes from mappings whose
hashes remain catalog members, and retires zero-live packs before opening any
partially-live source content.

Automatic runs process each partially-live source independently. A corrupt or
missing entry in one source is aggregated into the run error but does not block
healthy siblings or zero-live retirement. Deterministic selection therefore
cannot let one damaged pack permanently starve maintenance. The soft byte
budget is charged only after a successful source swap.

For a partial source, Kit:

1. snapshots its exact current catalog-member entry set;
2. bounded-reads and SHA-verifies each entry through the production store;
3. writes, seals, syncs, and publishes a replacement pack;
4. asks the adapter to compare-and-swap the source's exact entry set to the new
   mappings; and
5. retires cached source readers and deletes the old file before removing the
   old pack record.

If the compare-and-swap loses a race, the replacement is an ordinary orphan
and reconciliation handles it. If physical retirement fails, the old record is
retained so later maintenance can retry. Mapping authority remains on the
replacement.

## Unpack

Unpack is the universal downgrade and recovery path. It first repairs stale
mappings and preflights every live pack against all maintenance bounds before
restoring any content. One malformed or oversized required pack fails the
preflight without changing authority.

After preflight, Kit bounded-reads each mapped entry and durably publishes its
canonical loose file. Only after every required loose object exists does the
adapter atomically clear pack mappings. Kit then retires readers and removes
pack files and records. A crash before the metadata change leaves packed
authority plus harmless loose duplicates; a crash after it leaves complete
loose authority.

## Limits and large objects

The first extraction preserves these current msgvault maintenance limits:

- 64 MiB maximum raw or stored bytes per maintenance entry;
- 128 MiB maximum maintenance pack container;
- 8 MiB maximum footer; and
- 100,000 maximum entries per pack.

The low-level format's 4 GiB `pack.MaxRawLen` is a representation bound, not a
safe daemon-memory policy. New packing uses a bounded reader and leaves an
oversized canonical blob loose. The skip is observable and does not fail the
run. Oversized noncanonical legacy content is streaming-verified and moved to
the canonical loose path without buffering it.

Existing oversized packed entries remain on the compatibility read path but
are excluded from bounded automatic repack and unpack. No newly produced pack
may contain an entry that the bounded maintenance path cannot process.

Before the downstream document archive relies on packing large documents, `kit/pack` gains
format-compatible streaming append and verified streaming read primitives,
and `packstore` adopts them. Streaming removes the legacy maintenance block but
does not imply unbounded automatic work: per-run and optional per-object policy
limits remain configurable. The compatibility default stays 64 MiB until
measurements justify a change.

Streaming is not a prerequisite for beginning the downstream document archive catalog adapter or
mixed-store migration. That work may safely leave blobs above 64 MiB loose.
Streaming is a prerequisite for claiming that large objects in the downstream document archive can migrate
to packs or that oversized legacy packs have a bounded unpack/repack path.

## Reader retirement and Windows

The shared `Store` owns a bounded cache of pack readers. Reads hold the cache
slot stable while using it. Repack and zero-live cleanup retire every
daemon-owned reader for a source pack before attempting physical deletion.

Another process can still hold its own reader. On Windows, deletion may then
fail with a sharing violation. This is an expected, observable, retryable
physical-cleanup failure; it does not roll back an already-committed mapping
swap. The old pack remains an orphan and the next repair pass reclaims it after
the external reader closes. On Unix, an already-open handle remains usable
while subsequent opens resolve to the replacement.

Msgvault is daemon-backed for production read and maintenance paths, so the
daemon's shared store is the principal cache. Tests must still cover external
reader interference, because backup tools or older binaries can hold a pack
open during an upgrade.

The extraction preserves path validation, case behavior, stable file-identity
checks, publish-without-replacement semantics, and Kit's documented Windows
directory-sync no-op. It must not substitute Unix rename/delete assumptions.

## Backup and restore

Applications own backup inventory; Kit supplies representation-independent
reads. Msgvault continues to enumerate attachment content and thumbnail hashes,
while its backup content source opens them through `packstore.Store`. The
single re-resolution rule handles packing overlap.

The compatibility restore path materializes every captured blob as a durable
loose object. After restoring application metadata, the adapter atomically
clears every pack mapping and record. A restored database must never point at
production packs that the backup did not restore. The packer then treats the
restored vault as fully unpacked.

Pack-native restore remains a later optimization, especially for Windows. It
can build sealed packs through Kit and commit mappings through the same catalog
contract, while preserving loose-only restore as the fallback and downgrade
path.

## Application adapters

### Msgvault

Msgvault retains `attachment_packs`, `attachment_pack_index`, its SQLite and
PostgreSQL implementations, and all product transactions. Its adapter must
preserve:

- reference-aware resolution across content and thumbnail hashes;
- case-insensitive legacy hash inventory and transactional alias merging;
- legacy noncanonical path discovery and canonicalization;
- remove-source serialization and packed orphan handling;
- exact-set repack compare-and-swap;
- maintenance-safe PostgreSQL index creation; and
- restored-database pack metadata reset.

Commands, the 03:17 daily job, end-of-sync/import hooks, the 256 MiB automatic
run budget, warnings, and stats translation remain in msgvault.

### Downstream document archive

The downstream document archive adds application-owned pack and pack-index tables keyed to `blobs`.
Catalog membership is a `blobs` row; reachability is determined separately by
node and version references, plus any future retention policy. GC atomically
rechecks that a blob is eligible before removing membership, then asks Kit to
reclaim loose storage or leaves dead packed bytes for repack.

The downstream document archive replaces direct loose-path reads with the mixed store, integrates
backup inventory and loose-only restore reset, and initially exposes an
explicit packing command. Automatic scheduling is considered only after the
explicit migration and recovery paths are hardened.

Both products remain daemon-first for maintenance. CLI commands are clients of
the daemon-owned store rather than independent pack owners.

## Extraction and migration sequence

### Stage 1: extract into Kit

- Introduce `kit/packstore` with app-neutral paths, readers, writer, limits,
  events, maintenance engine, and catalog contracts.
- Move or reproduce the portable tests and fault-injection coverage before
  changing msgvault call sites.
- Add format fixtures and Windows lifecycle tests in Kit.
- Keep msgvault on its merged internal implementation.

### Stage 2: migrate msgvault

- Implement the catalog adapter over the existing schema and transactions.
- Adapt MIME attachment writes to the Kit loose writer.
- Replace internal reader and maintenance wiring without changing commands,
  schedules, stats meaning, or storage.
- Demonstrate old/new cross-read compatibility and the full Windows and
  real-archive gates.
- Remove duplicated internal engine code only after parity passes.
- A development branch may temporarily pin the reviewed Kit head as a
  pseudo-version. Before the msgvault PR merges, land and tag the Kit change and
  pin msgvault to that proven release so rollback changes code, not storage.

### Stage 3: add streaming

- Extend `kit/pack` without changing the stable container format.
- Prove bounded memory for append, read, repack, and unpack.
- Make legacy oversized packs maintainable while retaining automatic work
  budgets.

### Stage 4: adopt in the downstream document archive

- Add the downstream document archive's schema and catalog adapter.
- Wire durable writes, mixed reads, GC, backup, and restore.
- Harden an explicit loose-to-packed migration and unpack round-trip.
- Consider automatic packing only after those paths pass.

Stages 3 and 4 may overlap after msgvault compatibility passes. The downstream document archive's
adapter and mixed reads do not wait for streaming, but any migration claim for
objects above the compatibility ceiling does.

## Compatibility and verification gates

The downstream document archive adoption is blocked until the msgvault migration demonstrates:

- Existing loose files, packs, paths, and mappings open without conversion.
- The internal and Kit implementations cross-read packs in both directions.
- Pack footer and index metadata remain byte/field compatible.
- Concurrent read, pack, repack, backup, and deletion races pass.
- Fault injection at every filesystem/catalog boundary preserves authority.
- Windows reader retirement and retryable sharing violations pass CI.
- Pack, API/MCP/export read, backup, restore, and unpack round-trips succeed.
- Missing, corrupt, orphaned, stale, oversized, duplicate-normalized, and
  noncanonical legacy inputs retain their current outcomes.
- The complete lifecycle succeeds against a copy of a real msgvault archive,
  never the production archive.

The Kit test suite owns format, filesystem, cache, bound, fault, and generic
catalog-contract coverage. Msgvault retains adapter, SQL-dialect, command,
schedule, backup, and end-to-end tests. The downstream document archive adds its own reachability, GC,
backup, restore, and end-to-end coverage. Passing Kit tests alone is not
sufficient evidence of application parity.

## Expected consequences

- Msgvault receives no immediate user-visible feature or data migration from
  the extraction.
- Windows behavior is safer to evolve because the file lifecycle is tested once
  at the reusable boundary and again through msgvault integration.
- The downstream document archive avoids encoding a second pack state machine and can migrate gradually
  from its existing loose CAS.
- Direct-to-pack restore and streaming become shared improvements rather than
  application-specific rewrites.
- The catalog interfaces are the deliberate seam: physical correctness belongs
  to Kit, while application meaning remains transactionally local.
