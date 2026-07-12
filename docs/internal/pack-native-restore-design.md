# Pack-Native Attachment Restore

Design for msgvault issue #466. Status: approved for implementation planning.

## Summary

`msgvault backup restore` currently reads and verifies every attachment blob
from the backup repository, then materializes each one as an individual loose
CAS file. After Kit publishes the restored database, msgvault clears the
snapshot's `attachment_pack_index` and `attachment_packs` rows because those
rows describe the source vault's production packs, not files restored from the
backup repository.

That path is correct and remains the universal compatibility and recovery
path. It also forfeits the fact that backup repositories and production
attachment storage use the same sealed plain-v1 pack container. On archives
with tens of thousands of attachments, restore pays for tens of thousands of
file creations, directory updates, and antivirus scans. The cost is especially
visible on Windows.

The new default restore path copies each compatible repository pack once into
the restored attachment store, verifies every selected attachment blob, and
atomically rebuilds production pack metadata in the still-unpublished restored
database. Incompatible entries continue through the existing loose path. The
database becomes visible only after every indexed pack is durable and the
ordinary restore proof has passed.

The reusable physical import and compatibility policy belong in
`go.kenn.io/kit`. Msgvault owns its schema, snapshot membership, catalog
transaction, command policy, and user-visible reporting.

## Goals

- Restore compatible attachment content without creating one loose file per
  blob.
- Preserve at least the current per-blob SHA-256 and size verification.
- Make the publish-before-authority ordering explicit and crash-safe.
- Keep old snapshots and incompatible packs restorable through the existing
  loose path.
- Preserve an explicit fully loose recovery and downgrade path.
- Keep Kit's import API application-neutral so another content-addressed
  application can supply its own catalog transaction later.
- Record representative restore file-count and elapsed-time measurements,
  including native Windows.

## Non-goals

- Adopting production packs wholesale during backup creation.
- Moving files out of, or otherwise mutating, the backup repository.
- Reflink or clone-file optimization. The first implementation performs a
  verified streaming copy; a safe reflink fast path can be added later.
- Changing the plain-v1 pack format, `.mvpack` extension, sharding, or footer
  identifiers.
- Raising Kit's maintenance limits or adding streaming pack reads and writes.
- Eliminating the loose restore implementation.
- Choosing a live-fraction threshold for pack import. The first implementation
  imports a compatible pack when it contains at least one eligible snapshot
  blob, regardless of dead space.

## Existing behavior and invariants

Kit backup restores a database into a staging file under the held target root.
It verifies database page hashes while materializing that file, restores and
hash-verifies attachment content, stages extras, then runs
`PRAGMA integrity_check` and compares application-defined manifest statistics.
Only after those proofs does it publish extras and the database.

Msgvault's manifest statistics cover messages, conversations, sources,
accounts, labels, attachment rows, distinct attachment blobs, and the message
date range. They deliberately do not cover the production pack tables. A
transaction that changes only `attachment_pack_index` and `attachment_packs`
is therefore statistics-neutral. Running `integrity_check` after that
transaction proves the final staged database bytes, including the new catalog.

Kit packstore already enforces the authority rule needed by this design: a
physical loose file or pack entry never grants read access by itself. The
application resolver must report catalog membership, and a packed read must
have an indexed mapping. Extra footer entries copied with a repository pack
remain unreadable and count only as dead physical space.

The relevant default Kit maintenance limits are configurable through the
target store:

- maximum raw or stored bytes selected for a maintenance read: 64 MiB;
- maximum pack container size: 128 MiB;
- maximum footer size: 8 MiB; and
- maximum footer entry count: 100,000.

The compatibility decision always uses the target store's configured
`packstore.Limits`; it never hardcodes those default values.

## Authority and liveness

The importer deals with three separate questions:

1. **Snapshot membership:** the restored database and attachment-list union
   agree that a hash belongs to this snapshot.
2. **Physical eligibility:** the repository pack and selected entry satisfy the
   target store's format and maintenance limits.
3. **Catalog authority:** the staged restored database maps the selected hash
   to the durably published production pack.

Only hashes satisfying all three become packed mappings. A snapshot member
that is physically ineligible remains a catalog member backed by its restored
loose file. A footer entry not in the snapshot receives no mapping, even when
its bytes arrive as part of a copied pack.

This separation keeps Kit independent of product reachability and retention.
Kit receives the snapshot set and an application catalog capability; it does
not derive liveness from pack contents or filesystem presence.

## Package boundary

### Kit backup owns

- Loading the manifest, attachment-list union, and repository blob index.
- Proving that the restored database's content set and the attachment-list
  union agree in both directions.
- Grouping selected content by repository pack.
- Invoking an optional packed-content restore seam after database page-map
  materialization and before `proveRestoredDB`.
- Materializing every entry the packed seam declines through the existing
  loose restore path.
- Treating source corruption as a restore failure rather than a compatibility
  fallback.
- Preserving the current loose-only behavior when the application does not
  provide the optional capability.

The generic seam contract requires the application hook to leave the staged
database structurally valid and keep the application's manifest statistics
unchanged. Kit runs its ordinary proof after the hook returns.

### Kit packstore owns

- Validating source containers, footers, entries, hashes, and configured
  limits.
- Classifying an entry or pack as compatible, incompatible, or corrupt.
- Copying a compatible source pack into private target staging.
- Syncing and closing the staged file before a no-clobber publish into the
  canonical sharded production path.
- Reopening or otherwise validating the published result before catalog
  authority changes.
- Building full-footer `PackRecord` totals and mappings for only the selected
  eligible entries.
- Calling one application-supplied replacement transaction after every pack
  file is durable.
- Reporting packed and loose-fallback counts and typed incompatibility reasons.

The importer uses the repository pack ID as the production pack ID. This makes
retry target paths stable. If that canonical path already contains a verified
byte-identical pack, retry reuses it. A different file at the same valid pack
ID is a collision and fails closed; the importer never clobbers it or silently
allocates another ID.

### Msgvault owns

- Constructing the optional packed restore adapter for the target attachment
  root and staged restored database.
- Ensuring the pack tables exist for a packed restore without running unrelated
  schema migrations.
- Replacing every snapshot-carried pack record and mapping in one transaction.
- Indexing only restored attachment content and thumbnail hashes selected by
  Kit.
- Recording imported packs with restore-time `created_at` values.
- Keeping the existing table-aware metadata clear for loose restore.
- The `--loose-attachments` override, output, logs, and end-to-end tests.

## Restore algorithm

### 1. Source and database preflight

Kit performs the existing manifest, index, page-map, and attachment-list
validation before selecting a physical representation. It materializes the
database into its unpublished staging file and verifies page hashes exactly as
today.

Kit opens that staged database through the held target and asks the application
for restored content membership. The snapshot attachment-list union and the
database-derived set must agree in both directions before any content pack is
published. This retains the current protection against a manifest/list/database
set mismatch.

### 2. Build the mixed representation plan

Kit groups snapshot references by the repository pack named in the repository
index. For each pack, packstore preflights:

- a regular, stable source file;
- plain-v1 magic, version, and flags;
- target `PackBytes`, `FooterBytes`, and `PackEntries` limits;
- footer checksum, unique IDs, valid entry spans, and supported flags; and
- agreement between each selected repository index row and its authoritative
  footer entry.

For every selected snapshot entry, packstore reads the blob, verifies its CRC,
re-derives SHA-256, and checks its raw size against the attachment list. An
entry whose raw or stored length exceeds the target `BlobBytes` limit is marked
for loose restore. Eligible siblings in the same source pack may still use the
copied pack; the oversized footer entry arrives as dead space and receives no
production mapping.

A pack whose container, footer, entry count, encoding, or settings are
incompatible sends all of its selected entries to loose restore. A missing,
truncated, checksum-invalid, metadata-inconsistent, or hash-invalid source is
corruption and aborts restore. Falling back must never conceal repository
damage.

### 3. Publish compatible packs

For every source pack with at least one eligible selected entry, packstore
streams the complete immutable container into a private file on the target
filesystem. It syncs and closes the file, publishes it without replacing an
existing destination, syncs the destination directory where supported, and
validates the final file.

The backup repository is read-only throughout. Copying a whole pack is the
performance win: selected blobs are still read and SHA-256 verified, so source
read I/O does not disappear. Restore avoids thousands of destination file
creations, renames, shard-directory syncs, and antivirus scans.

### 4. Restore loose fallback entries

Kit sends every declined snapshot reference through the existing loose
materialization path. Each file is independently hash- and size-verified and
published at every path derived from the restored database. This covers old
snapshots, explicit loose mode, incompatible packs, and oversized selected
entries.

### 5. Replace staged catalog metadata

After every compatible pack and fallback loose file is durable, the msgvault
adapter performs one transaction against the staged database:

1. create the pack tables and their required indexes if absent, without
   running unrelated migrations;
2. delete all restored `attachment_pack_index` rows and
   `attachment_packs` rows;
3. insert one pack record per imported pack, using full footer entry and stored
   byte totals and restore time for `created_at`;
4. insert mappings only for eligible snapshot content and thumbnail hashes;
   and
5. verify that every inserted mapping names a snapshot member and a record
   inserted by the same transaction.

Targeted pack-schema initialization is necessary for a snapshot that predates
the pack tables. It must not run the application's full schema initializer or
unrelated migrations.

Loose-only restore retains the existing `ClearAttachmentPackMetadata`
behavior: check whether the two tables exist, clear them if present, and do not
create them for an old snapshot. This rule applies both when no packed seam is
available and when the user passes `--loose-attachments`.

### 6. Prove and publish

Kit opens the mutated staged database, runs `PRAGMA integrity_check`, and
recomputes msgvault's manifest statistics. It then publishes staged extras and
the restored database using the existing ordering.

At the instant the database becomes visible, every packed mapping points at an
already durable file, every loose member has a durable loose file, and the
final database has passed the ordinary proof.

## Import threshold and pack age

The first implementation does not impose a minimum live fraction. If a
compatible repository pack contains one eligible snapshot blob, the whole pack
is copied and unselected entries count as dead space. This favors eliminating
small-file creation and keeps the decision deterministic.

Imported `attachment_packs.created_at` values use restore time, not repository
pack creation time. Repack's minimum-age hysteresis therefore prevents a newly
restored vault from immediately rewriting low-live packs. After hysteresis,
ordinary live-byte accounting and repack reclaim dead space. A future measured
optimization may choose loose materialization below a live-fraction or
copy-amplification threshold; that is deliberately deferred until real restore
data justifies the added policy.

## Crash and retry behavior

| Failure point | Durable state | Recovery |
| --- | --- | --- |
| Before target pack staging | No imported state | Retry normally. |
| During pack copy | Private staging file only | The importer removes it on error; packstore maintenance also removes recognized stale pack staging. |
| After staged file sync, before publish | Complete private staging file | Retry removes or replaces only its own staging artifact. No catalog authority exists. |
| After no-clobber pack publish, before catalog transaction | Canonical uncataloged pack | Safe orphan: packstore maintenance either adopts verified entries needed by the currently visible catalog when its reference inventory is complete, removes a pack with no needed entries, or retains an unreadable/quarantined pack. Restore retry reuses the stable source-ID destination only when the new selected subset verifies, recopies it if maintenance removed it, and fails hard if retained selected bytes are corrupt. |
| During catalog transaction | Published packs; transaction rolled back | Staged database has its previous metadata. Retry reuses verified published packs. |
| After catalog commit, before proof | Published packs and mappings only in unpublished staged DB | Failure removes or abandons the staged DB; visible database authority is unchanged. Published packs are safe orphans relative to the visible database. |
| Proof or extras failure | Visible database unchanged; published packs and content-addressed loose files may remain | Existing cleanup removes the staged database and extras. Loose files are benign, and retry handles published packs idempotently. |
| After database publish | Complete packed/loose restored vault | No later restore step can make an index point at an absent pack. |

The importer never relies on an orphan surviving maintenance. Its retry rule is
idempotent under every existing packstore disposition: validate-and-reuse if
the canonical pack remains, copy again if it was removed, and fail closed if a
different object occupies the same pack ID.

With `--overwrite`, the existing database and sidecars remain authoritative
until the final publish, exactly as in current restore. Old production pack
files that are not selected by the new snapshot may remain on disk after the
catalog replacement, but the new database grants them no authority; ordinary
packstore reconciliation removes or adopts them according to the new catalog.
An existing target pack at a selected repository pack ID is reused only when
it is byte-identical. A different object at that ID fails restore before the
database publish.

## Compatibility and fallback contract

Compatibility is a typed decision, not an error-string match. Expected
incompatibilities include:

- unsupported but otherwise well-formed pack encoding or repository setting;
- container, footer, or entry-count limits above the target's configured
  values; and
- selected entry raw or stored length above target `BlobBytes`.

The first two decline the whole pack. The last declines only that selected
entry when the pack itself remains compatible. Each reason is observable in
restore statistics and structured logs.

Repository corruption, target I/O errors, target identity changes, context
cancellation, and application catalog failures are hard errors. They do not
fall back.

The production extension remains `packstore.PackExt` (`.mvpack`). Applications
that want direct pack restore must freeze their backup `PackFileExtension` to
that value and use compatible plain-v1 repository settings. Applications with
another extension or representation retain loose restore.

## User-visible behavior

Pack-native restore is automatic when the application provides the capability
and source packs are compatible. Existing commands and old repositories keep
working without migration.

As with every `--overwrite` restore, the snapshot replaces the target's logical
archive state. Content that exists only in the newer target loses catalog
authority; later maintenance may physically remove its now-unreferenced packs.

`msgvault backup restore --loose-attachments` disables the packed seam and
uses the current fully loose behavior. It is the explicit recovery, inspection,
and downgrade path for a fresh target. With `--overwrite`, pre-existing
uncataloged packs may later be adopted by maintenance if the restored snapshot
still references their contents. `unpack-attachments` cannot remove those
uncataloged leftovers because it processes cataloged packs only. Restoring into
a fresh target is therefore the only path that currently guarantees a fully
loose vault.

Success output reports:

- total attachment blobs and bytes;
- blobs restored with packed authority;
- blobs materialized loose;
- production packs installed; and
- compatibility fallback counts when nonzero.

The command no longer prints an unconditional instruction to run
`pack-attachments`; it does so only when loose blobs remain.

## Testing

### Kit

- Generic importer contract with an in-memory application catalog.
- Full-footer totals with only a selected subset indexed.
- Extra footer entries remain unauthorized.
- Target-configured limits, including non-default values.
- Oversized selected entry restored loose while an eligible sibling in the
  same copied pack is indexed.
- Whole-pack fallback for incompatible container, footer, entry count, flags,
  or settings.
- Hard failure for truncation, checksum damage, index/footer disagreement,
  hash mismatch, size mismatch, cancellation, and target identity change.
- No-clobber publication, byte-identical retry reuse, collision refusal, and
  retry after orphan removal.
- Fault injection at staging write, file sync, close, publish, directory sync,
  final validation, catalog transaction, and post-transaction proof boundaries.
- Old applications without the optional capability retain byte-identical loose
  restore behavior.
- Native Windows coverage for publication and retry semantics.

### Msgvault

- Targeted pack-schema initialization for a pre-pack snapshot in packed mode.
- Pre-pack snapshot loose restore creates no pack tables.
- Current packed snapshot restores with correct records, mappings, full footer
  totals, and restore-time ages.
- Mixed compatible, incompatible, and oversized content produces the expected
  packed/loose split.
- Catalog transaction rollback leaves the visible database unchanged.
- Restored content reads byte-identically through the raw API, MCP, CLI export,
  and backup capture.
- Restore followed by pack, repack, unpack, and another backup round-trip.
- `--loose-attachments` preserves the fully loose recovery path.
- Frozen old-repository fixtures remain restorable.
- The staged SQLite catalog transaction uses the same application adapter
  semantics as ordinary pack maintenance; backup restore continues to
  materialize a SQLite archive.

## Performance and hardening gate

The implementation is not merge-ready until it is exercised against isolated
copies of a representative archive and compared with current main.

Record separately:

- repository bytes read and verified;
- attachment blobs and bytes selected;
- destination pack files and loose files created;
- attachment-stage and total restore elapsed time;
- database proof elapsed time; and
- peak memory where practical.

The expected improvement is destination filesystem work, especially file
creation and antivirus scanning on Windows. Per-selected-blob SHA-256
verification remains, so the design does not claim to eliminate source read
I/O.

Required functional hardening is:

1. packed backup restore;
2. reads through API, MCP, and export;
3. backup verification;
4. crash injection and retry;
5. repack and unpack round-trips;
6. explicit loose restore; and
7. integrity and manifest-statistics proof after every restored layout.

### Primary-host hardening results (2026-07-11)

The primary-host gate ran on macOS/APFS against an isolated corpus built from
46,060 distinct real attachment blobs (7.6 GiB raw). The database catalog was
synthetic and contained no account or message content. The installed binary,
live database, and live attachment files were never modified. The verified
repository held the corpus in 203 packs totaling 6.7 GiB.

Order-balanced restores compared `origin/main` at `5464711e` with this branch
using the released Kit v0.7.0 dependency:

| Layout | Order 1 | Order 2 | Destination files | Peak RSS |
| --- | ---: | ---: | ---: | ---: |
| Main loose | 176.5 s | 168.5 s | 46,060 | 227-239 MiB |
| Branch packed | 16.5 s | 13.8 s | 203 | 301-303 MiB |

Both layouts restored and proved the same 46,060 blobs and manifest statistics.
The mean packed restore was 11.4 times faster on this filesystem. As designed,
the improvement came from avoiding per-blob destination creation rather than
from skipping source reads or SHA-256 verification.

Functional hardening evidence from the same run:

- the five largest blobs plus a representative small blob re-hashed correctly;
- an uppercase alias read byte-identically through the raw API, CLI export, and
  MCP;
- backup capture over the packed target and full verification covered all
  7.7 GiB with zero problems;
- interruption after ten pack publications left no visible database, and
  overwrite retry completed with exactly 203 packs and 46,060 mappings;
- a forced readonly staged catalog failed after pack publication with no
  visible database, and retry completed with the same exact authority;
- unpack restored all 46,060 loose blobs, an independent full SHA-256 pass had
  zero mismatches, and repacking restored the 203-pack layout; and
- a post-repack backup and a subsequent incremental packed backup both fully
  verified. The incremental capture completed in 8.1 seconds.

Production-path benchmarks (`-count=10`) found no statistically significant
main-to-branch change in loose reads, warm or concurrent packed reads, packed
backup capture, pack time, or repack time. Read and maintenance geomeans moved
by +1.34% and +1.19%, respectively, within noise. Repack added 0.02% allocated
bytes and 0.69% allocations, which is not material.

### Gate coverage and outstanding items

Of the measurements mandated above, the primary-host run recorded attachment
blobs and bytes selected (46,060 blobs, 7.6 GiB raw), destination pack and
loose file counts, total restore elapsed time, and peak RSS. Repository read
verification is covered indirectly: the source repository (203 packs,
6.7 GiB) fully verified before the runs, and every selected blob was SHA-256
re-verified during both restores, but a per-run repository-bytes-read figure
was not captured. The restore command reports only total duration, so the
attachment-stage versus total split and the database-proof elapsed time were
not measured separately.

Functional gate items 1 through 5 — packed restore; reads through API, MCP,
and export; backup verification; crash injection and retry; repack and unpack
round-trips — are evidenced above. Item 7's integrity and manifest-statistics
proof ran, via Kit's ordinary restore proof, for both the loose and packed
restore layouts.

Outstanding before merge:

- Gate item 6: an explicit `--loose-attachments` restore on the isolated
  corpus. The flag is exercised end-to-end by the CLI restore tests,
  including the loose-mode metadata cleanup, but the hardening host has not
  run it against the real-content corpus.
- Gate item 7 for maintenance-produced layouts: an explicit
  `PRAGMA integrity_check` and manifest-statistics recomputation after the
  unpacked and repacked layouts. The unpack round-trip passed an independent
  full SHA-256 comparison and the post-repack backups fully verified, but a
  per-layout database proof was not separately recorded.
- The repository-bytes-read, attachment-stage, and database-proof
  measurements, which require timing instrumentation beyond the command's
  total duration.
- The native Windows measurement described below.

Native Windows CI is mandatory. Before merge, record destination file counts
and elapsed time for loose and pack-native restore on a representative native
Windows archive. If a suitable existing archive is unavailable, use a
deterministic generated repository large enough for per-file filesystem and
antivirus overhead to be visible. The Windows measurement complements, rather
than replaces, the larger isolated real-archive comparison on the primary
hardening host.

## Rollout and follow-ups

The work lands in two dependency-ordered stages:

1. Kit adds the generic packed-content restore seam and packstore importer,
   with no msgvault schema knowledge.
2. Msgvault pins the reviewed Kit change and adds its staged-database adapter,
   command behavior, and end-to-end validation.

During development msgvault may use a Kit pseudo-version. Msgvault must pin a
tagged Kit release before merge.

Deferred follow-ups:

- backup-create adoption of compatible production packs;
- safe reflink/clone-file publication;
- a measured live-fraction or copy-amplification threshold;
- verified streaming for content above current maintenance limits; and
- adoption by other content-addressed applications through their own catalog
  and reachability adapters.
