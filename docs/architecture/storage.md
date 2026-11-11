---
last_edited: "2026-11-11"
title: Data Storage
description: Database schema, Parquet analytics cache, content-addressed attachments, and token storage.
---

The archive database preserves messages, source evidence, curated people, and
durable operation state. Attachment bytes are stored separately and shared by
content hash. Analytics and semantic indexes are derived from those records;
back up the archive and attachments before relying on a rebuild or local purge.

## Storage Layers

| Layer | Role | Location |
|---|---|---|
| SQLite | Default system of record | `~/.msgvault/msgvault.db` |
| PostgreSQL | Optional system of record | `[data].database_url` |
| SQLite vector index | Optional, rebuildable semantic index | `[vector].db_path`, default `~/.msgvault/vectors.db` |
| Parquet | Analytics cache | `~/.msgvault/analytics/` |
| Attachments | Content-addressed loose files and sealed packs | `~/.msgvault/attachments/` |
| Tokens | OAuth tokens and provider credentials | `~/.msgvault/tokens/` |

## Archive Database

Message metadata, bodies, labels, participants, raw payloads, and curated
profiles live in the configured archive database. Attachment bytes are stored
separately. SQLite is the default and stores the archive at `~/.msgvault/msgvault.db`. PostgreSQL is opt-in through `[data].database_url` and is intended for new archives or fresh re-syncs.

### Core Tables

**sources** -- Accounts and import sources with sync state.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `source_type` | TEXT | Provider/import type, for example `gmail`, `imap`, `gcal`, `teams`, `discord`, `beeper`, `chatwoot`, `mbox`, `whatsapp`, `omi`, `granola`, `plaud`, `circleback`, `notion_meetings`, `muesli`, `twilio`, or `twenty` |
| `identifier` | TEXT | Provider-stable identifier such as an email address, phone number, or Discord guild ID |
| `display_name` | TEXT | Account display name |
| `sync_cursor` | TEXT | Sync cursor (Gmail history ID for Gmail accounts) |
| `last_sync_at` | DATETIME | Last sync timestamp |

**conversations** -- Email threads and chat conversations.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `source_id` | INTEGER FK | References `sources` |
| `source_conversation_id` | TEXT | Source-specific thread/conversation ID |
| `conversation_type` | TEXT | `email_thread`, `direct_chat`, `group_chat`, `channel`, or another provider-defined kind |
| `message_count` | INTEGER | Denormalized count |
| `last_message_at` | DATETIME | Latest message timestamp |

**messages** -- Message metadata. Foreign key to `conversations`.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `conversation_id` | INTEGER FK | References `conversations` |
| `source_id` | INTEGER FK | References `sources` |
| `source_message_id` | TEXT | Source-specific message ID |
| `message_type` | TEXT | `email`, `calendar_event`, `meeting_transcript`, `beeper`, `chatwoot`, `teams`, `discord`, `sms`, `mms`, `whatsapp`, `imessage`, `fbmessenger`, `telegram`, `synctech_sms_call`, `google_voice_text`, `google_voice_call`, `google_voice_voicemail`, or a type chosen by a [message import](../usage/message-import.md) caller |
| `sent_at` | DATETIME | Send timestamp |
| `sender_id` | INTEGER FK | References `participants` |
| `account_address` | TEXT, nullable | The one confirmed address an email or calendar event belongs to; NULL when evidence conflicts or is missing |
| `account_path` | TEXT, nullable | `inbound`, `sent` or `calendar`; NULL on email and calendar rows means attribution is still pending |
| `email_linkage_outcome` | TEXT, nullable | `complete`, or why the identifiers in `email_link_keys` may be incomplete (`missing_raw`, `invalid_headers`, `size_limit` and others); NULL on an email means the keys are still pending |
| `subject` | TEXT | Message subject |
| `snippet` | TEXT | Preview excerpt |
| `size_estimate` | INTEGER | Approximate size in bytes |
| `has_attachments` | BOOLEAN | Attachment flag |
| `deleted_at` | DATETIME | Soft-delete timestamp |
| `deleted_from_source_at` | DATETIME | Records removal from the source; content may remain archived |

The Store derives `account_address` and `account_path` in the same transaction
as every message write that changes their inputs: raw MIME, recipients, and
Sent or Drafts labels. A change to confirmed identities or the source
identifier returns the affected rows to pending in its own transaction, then
derives them in pages after it commits. Every sync or import first derives the
source's pending rows, which a partial index on pending rows keeps cheap when
there are none. Attribution never changes `source_id`.
Email linkage follows the same pattern: a write that stores MIME also fills
`email_link_keys` and `email_linkage_outcome` in its transaction, and the
same sync step derives any email still pending, through its own partial index.
Archives from before the column start with every email pending.
Rows other than email and calendar events keep both columns NULL. The
[received-account design](https://github.com/kenn-io/msgvault/blob/main/docs/internal/received-as-identity-design.md) records
the rules; [Searching](../usage/searching.md#find-mail-by-the-address-that-received-it)
covers `received:` and `account:`.

**message_delivery_addresses** -- Delivery-header addresses read from each
email's stored MIME (`kind` is `original` or `delivered`, `address` is
lowercased). The address-leading index lets an identity change re-derive only
the messages that mention it. A derived row's addresses always match its stored
MIME; messages without MIME have none.

**email_link_keys** -- Identifiers each email names in its headers, read from
its stored MIME: `reply` (In-Reply-To), `reference` (References) and `attached`
(the Message-ID of each attached `message/rfc822` original), at most 128 per
kind, unique per email and kind. The value-leading index finds every email that names an identifier. A
copy's own Message-ID is `messages.rfc822_message_id`, so related-copy lookups
match these values against that column and against each other.

**message_bodies** -- Parsed content stored separately from message metadata.

`message_id` is both the primary key and a reference to `messages`; `body_text`
and `body_html` hold the parsed bodies. Keeping these large values separate
allows metadata scans without reading message content. Search uses the full-text
index; detail readers fetch bodies by message ID.

Email parsing replaces NUL (`U+0000`) with the visible replacement character
`�` (`U+FFFD`) in display and search text, including subjects, bodies, display
names, and attachment filenames. Provider snippets follow the same rule.
This policy applies to SQLite and PostgreSQL; PostgreSQL cannot store NUL in
text columns. It keeps the surrounding text searchable without silently joining
words across a removed character. These projections are not lossless originals.

MIME identity values containing NUL are unusable: message, reply, reference,
list, and content IDs are omitted from new projections, and invalid address
tokens are skipped. Historical SQLite identity keys retain their exact comparison
form, including NUL, so existing duplicate groups can still be processed.
Replacing characters in these keys could falsely match a different identity.
The original MIME and decoded attachment bytes remain unchanged, as do raw-part
fingerprints. Ingestion, replay, message repair, and text re-derivation use this
same parser policy. Existing email bodies and body-derived snippets are rebuilt
from retained MIME on the next sync or with `repair-derived`; that pass does not
rewrite subjects, participants, or attachment metadata. Those fields require
message repair or re-import when their old projections contain NUL.

**message_raw** -- Raw provider payload storage, compressed with zlib.

| Column | Type | Description |
|---|---|---|
| `message_id` | INTEGER PK/FK | References `messages` |
| `raw_data` | BLOB | Compressed MIME or provider JSON data |
| `compression` | TEXT | `zlib` |
| `raw_format` | TEXT | Format discriminator, such as `mime` or `notion_meeting_json` |

Native Notion meetings use raw format `notion_meeting_json`. The compressed
envelope keeps provider discovery, structured blocks, page Markdown, minimized
resolved users, canonical text used for safe transcript preservation, and
hydration warnings. Integration tokens and authorization headers are never
part of the envelope.

**source_import_items** -- Durable import receipts.

Apple Mail imports keep two kinds of rows here:

- `emlx-target` rows track one archived message, which several `.emlx` files
  can share. The importer marks the row `pending` before it writes the message,
  its attachments or its search entry, and `imported` once all of that work
  succeeds. The completion records the attachment and search settings it used;
  a later import with different settings finishes the message again from its
  archived raw. A `pending` row also names the file being written, the
  attachments it contributes, and a digest of the new raw. If the import stops
  before that file's row is updated, the next import that finishes the message
  credits those attachments to the file when the archived raw matches. The
  file's own row names that message before the write starts, so a row that
  names a different message is always the newer one.
  A message archived before receipts existed is marked legacy. For it, a file
  without its own attachment record only fills empty placeholders and records
  what it holds; it can replace stored attachment content only after its own
  cache changes.
- `emlx-occurrence` rows track one `.emlx` file, keyed by a digest of the
  import root plus the file's relative path. An `imported` row stores a
  filesystem fingerprint of the file and its cached attachments, and a hash of
  each attachment the file contributed. A `pending` row means the file still
  has work to finish.

A repeat import skips reading a file when its fingerprint matches and its
message is complete. The attachment hashes stop a file from replacing an
attachment that another file has since updated. Platforms without a supported
fingerprint still record completed rows but always read the file.

The rows reuse the existing table and the sync run's ownership check, without a
schema migration. They are cache hints rather than integrity checks; see
[repeat imports](../usage/importing.md#repeat-apple-mail-imports).

**participants** -- Observed addresses and handles from source data. These are
separate from the curated `persons` table.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `email_address` | TEXT | Email address (unique index) |
| `phone_number` | TEXT | Phone number (for chat participants) |
| `display_name` | TEXT | Contact name |
| `domain` | TEXT | Extracted domain |

**message_recipients** -- From/To/Cc/Bcc mapping.

| Column | Type | Description |
|---|---|---|
| `message_id` | INTEGER FK | References `messages` |
| `participant_id` | INTEGER FK | References `participants` |
| `recipient_type` | TEXT | `from`, `to`, `cc`, `bcc`, `mention`, or another provider-defined role |

**labels / message_labels** -- Source labels and imported mailbox/folder labels
(many-to-many).

| Table | Key Columns |
|---|---|
| `labels` | `id`, `source_id`, `source_label_id`, `name`, `label_type` |
| `message_labels` | `message_id`, `label_id` |

**attachments** -- Content-addressed attachment metadata.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `message_id` | INTEGER FK | References `messages` |
| `filename` | TEXT | Original filename |
| `mime_type` | TEXT | MIME type |
| `size` | INTEGER | Size in bytes |
| `content_hash` | TEXT | SHA-256 hash |
| `storage_path` | TEXT | Relative path: `ab/abcd1234...` |

**sync_runs / sync_run_items / sync_checkpoints / source_import_items** -- Sync and import state for resumability and diagnostics.

| Table | Purpose |
|---|---|
| `sync_runs` | Track each sync operation (start, end, counts, errors) |
| `sync_run_items` | Track per-message fetch, ingest, delete, skip, and error outcomes inside a sync run |
| `sync_checkpoints` | Resume point per source (message ID, page token) |
| `source_import_items` | Track file/object-level imports from resumable adapters, including provider ID, checksum, status, and import errors |

Per-item sync diagnostics keep a failed message visible without hiding
successful work from the same run. Actionable item failures are recorded with
`status = 'error'`, `phase` values such as `fetch`, `ingest`, or `delete`, and
an `error_kind`/`error_message`. Expected churn, such as a Gmail message that
disappears before raw fetch, is recorded as `status = 'skipped'`.

Gmail also uses fetch/ingest outcomes as durable acquisition and file work.
The [Gmail durability protocol](#gmail-durability-protocol) owns selection and
settlement rules, which differ from the per-run error display.

`source_import_items.checksum` is nullable because some providers or legacy rows
may not have a stable checksum. msgvault treats a null checksum as an empty
string when checking already-imported source items.

### People, evidence, and operation state

| Table family | What it preserves |
|---|---|
| `persons`, `person_participants` | Durable profiles and their bindings to observed participants |
| `attribute_definitions`, `person_attribute_values` | Typed profile fields, values, and history |
| `organizations`, `employments`, `person_relationships` | Organization profiles, employment history, and dated relationships |
| `person_fact_*` | Evidence, claims, resolutions, decisions, and pins |
| `person_tracking`, `person_sweep_*` | Enrollment and progress for profile maintenance |
| `person_briefs`, `person_brief_evidence`, `person_brief_enrollments` | Saved conversation briefs, citations, and per-person enrollment |
| `person_inference_*`, `person_enrichment_*` | Provider profiles, checks, consent, external lookup state, and suppression |
| `carddav_*` | Address books, retained resources, publication, conflicts, and sync runs |
| `sync_runs` and worker run tables | Durable operation outcomes and progress behind Operations |

The current SQL schemas in `internal/store/schema.sql` and `schema_pg.sql` own
exact columns and constraints. [People guides](../usage/people.md) explain how
curation, evidence, and provider consent interact. These durable records are
not interchangeable with disposable analytics or semantic indexes.

### Full-Text Index

SQLite uses an FTS5 virtual table named `messages_fts`. PostgreSQL uses a `search_fts` `tsvector` column on `messages` with a GIN index.

Both power `msgvault search`, but the rankers differ. See [Search Ranking Across Backends](/docs/architecture/search-ranking/).

### Relationships

```
sources ─┬─< conversations ─< messages ─┬─< message_recipients ─> participants
         │                               ├─< message_labels ─> labels
         │                               ├── message_raw
         │                               └─< attachments
         └─< labels
```

## PostgreSQL Backend

PostgreSQL uses native types such as `BIGINT GENERATED ALWAYS AS IDENTITY`, `TIMESTAMPTZ`, `BYTEA`, and `JSONB`. Message, source, participant, label, attachment, and sync tables map to the same logical model as SQLite.

For semantic search, pgvector stores index generations, pending embedding work, and embedding vectors in the same PostgreSQL database. There is no separate `vectors.db` on PostgreSQL.

There is currently no SQLite to PostgreSQL migration command. Use PostgreSQL for a new archive or re-sync/import into an empty PostgreSQL database. See [PostgreSQL Backend](/docs/architecture/postgresql/) for setup and operational notes.

## Parquet (Analytics Cache)

The Web UI and TUI need to aggregate across your entire archive and return
results instantly as you group and drill down. On the default SQLite backend, msgvault exports denormalized metadata to
Parquet so DuckDB can group and filter it without repeatedly joining the
normalized archive tables.

Ungrouped Messages and Files listings page a scalar message or attachment
population before resolving participant lists for the returned rows. Exact
totals use separate narrow scans. This page-before-enrichment boundary keeps
multi-million-message listings inside the daemon's interactive DuckDB memory
budget without changing cache format or query semantics. Domain-filtered
Messages listings share the filtered message population between page
selection, participant enrichment, and total counting. This avoids repeating
recipient and conversation-roster membership joins under the interactive
memory limit. Listings without domain filters retain separate scalar scans.

The Parquet cache is disposable and can be rebuilt at any time. The daemon
starts HTTP health and API routing before analytics cache maintenance, and
aggregate views never trigger a build mid-session. With
`auto_build_cache = true` (the default), a stale or missing cache is built in
the background after HTTP is ready. In `engine = "auto"`, aggregate views use
live SQL while that work runs and switch to DuckDB after the cache is ready and
opens successfully; a failed build or open keeps live SQL. In
`engine = "duckdb"`, analytics remain unavailable until the required cache is
ready, with no SQL fallback. In auto mode, Messages serves live archive rows during a full rebuild and switches to the published cache when it opens. If no usable cache is produced, the live rows stay available with recovery steps. Other cache-dependent Web UI views report that preparation is in progress and retry
until the cache becomes ready; terminal unavailable states retain the explicit
rebuild action. Set `auto_build_cache = false` to skip automatic startup
maintenance and its live Messages path; scheduled syncs, ingest commands, and
`msgvault build-cache` can refresh or build the cache explicitly.

Each build writes and verifies a same-filesystem staging tree before publishing
under the exclusive cache lock. `_last_sync.json` is the commit marker: it is
invalidated before any live dataset changes and replaced last. A failure before
publication leaves the previous committed cache usable; interruption during
publication leaves the cache explicitly unavailable for repair, never marked
ready with mixed old and new datasets. Queries hold the lock shared, so a build
cannot replace Parquet files underneath an active reader. `msgvault
build-cache` builds or repairs the cache on demand. PostgreSQL archives use live
SQL for aggregate views rather than this Parquet acceleration layer.

The cache includes `relationship_activity`, `relationship_people`,
`relationship_domains`, and `relationship_daily` datasets. These compact edges
and rollups avoid expanding every message's participant list during people,
domain, relationship, timeline, and file-group queries. When a cache format changes, automatic cache building performs the required
rebuild during daemon startup. Run `msgvault build-cache --full-rebuild` to
request a full rebuild explicitly.

```bash
# Manual build
msgvault build-cache

# Full rebuild (discard existing)
msgvault build-cache --full-rebuild
```

Directory structure:

```
analytics/
├── messages/
│   ├── year=2020/
│   ├── year=2021/
│   └── ...
├── participants/
├── message_recipients/
├── labels/
├── attachments/
├── sources/
├── conversations/
├── message_labels/
├── relationship_activity/
├── relationship_people/
├── relationship_domains/
├── relationship_daily/
└── _last_sync.json
```

Messages are partitioned by year for efficient time-range queries. The cache omits full message bodies. Its size depends on message count,
participants, attachments, and the derived datasets included in the build.

## Content-Addressed Attachments

Every attachment from every message is identified by its SHA-256 content hash,
so identical bytes referenced by multiple messages are stored once. New
content is written as a loose file first; background maintenance and
`pack-attachments` move eligible loose objects into sealed immutable packs to
reduce file-count overhead. The archive can remain in a mixed state, and all
normal readers resolve loose and packed content transparently.

The attachment root can therefore contain both layouts:

```
attachments/
├── ab/
│   └── abcd1234567890...            # loose object: full SHA-256 name
├── packs/
│   └── 01/
│       └── 01k...mvpack             # sealed immutable pack
└── ...
```

Loose objects are sharded by the first two hash characters. Packed-object
locations and immutable pack totals are recorded in `attachment_pack_index`
and `attachment_packs`; loose objects have no pack-index row.

Use `pack-attachments` to migrate the eligible loose backlog immediately,
`repack-attachments` to reclaim dead space after content is removed, and
`unpack-attachments` to restore cataloged packed objects to loose files before
downgrading. The last command is local-only and requires the daemon to be
stopped because it removes production pack files. See the [CLI
reference](/docs/cli-reference/#pack-attachments) and [Backup](/docs/usage/backup/) guide
for maintenance and restore behavior.

Set `[data].loose_attachments = true` when file-oriented backup or storage
software requires stable individual attachment files. It prevents new packs,
rejects pack and repack commands, and restores backup content loose, but does
not convert existing packs automatically. Stop the daemon and run
`unpack-attachments` once if the existing archive must become fully loose.

### Loose publication policies

The writers in [`internal/export/store_attachment.go`](https://github.com/kenn-io/msgvault/blob/main/internal/export/store_attachment.go)
choose different `packstore` policies:

| Entry point | Successful publication |
|---|---|
| `StoreAttachmentFile` / `StoreAttachmentFileIncludingEmpty` | `AtomicPublication`: publishes complete bytes atomically, without requiring file or directory fsync |
| `StoreAttachmentFileDurable` | `DurablePublication`: verifies deduplicated content and fsyncs file content and directory entries before success |

Atomic publication prevents partial-file visibility; it does not promise the
same crash durability. Gmail MIME sync, remote-image archival, and maintenance
use the durable entry point. It can return a receipt with an error after bytes
become visible but before directory sync finishes. Check the error before
recording success. `Created` distinguishes a new blob for reference-checked
cleanup; a receipt alone is not proof of durability. Retrying a readable loose
file through durable deduplication repeats verification and the required syncs.
Neither writer records a SQL attachment occurrence.

## Gmail durability protocol

Gmail preserves RAW before publishing derived files. Its durable work record
in `sync_run_items` keeps unfinished materialization reachable by sync. This
is a protocol across SQL and the content-addressed store (CAS), not a global
transaction spanning them. The contract below belongs to the Gmail Syncer;
an empty `Options.SourceType` selects its legacy Gmail mode. IMAP retains
best-effort attachment handling, and other importers have their own completion
policies.

### Who owns each transition?

1. **The Syncer determines required work.** `requiredMaterialization` combines
   pending work with nonempty MIME attachments or incomplete parsing when an
   attachment directory is configured, plus enabled remote-image archival for
   email HTML. MIME and images are independent requirements. In-memory maps
   track them during a run; the ledger supplies durability.
2. **The Store commits the message and obligation together.** The Syncer's
   `persistMessage` supplies RAW, projections, a search document, and an
   `ingest/error` outcome naming MIME, images, or both. `PersistMessageContext`
   writes the supplied data in one SQL transaction, using the search document
   when indexing is available. A rejected obligation rolls back the message.
   FTS follows `upsertFTSOrUnsearchable`: recognized PostgreSQL size
   overflow can commit an unsearchable result; other FTS errors roll back.
3. **Export/CAS publishes bytes; the Store records occurrences.** After that
   commit, `storeAttachment` uses `StoreAttachmentFileDurable`, then
   `UpsertAttachmentRecord`. `remoteimage.ArchiveWithOptions` similarly publishes
   downloaded bytes before `UpsertRemoteImageAttachment`. A published blob can
   outlive a failed row write. Shared hashes identify bytes, not occurrences:
   MIME part keys and `remote-image:` URL identities remain distinct.
4. **The Syncer settles only after every required operation succeeds.**
   `repairPendingAttachments` attempts images even when MIME repair fails.
   `settleAcquisition` writes `fetch/resolved` with kind `acquired` before
   clearing either pending map. A failed settlement retains the work. Later
   fetch failures or a provider 404 must not supersede file work for an already
   committed message.

The production paths are [`sync.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/sync.go)
(`persistMessage`, `storeAttachment`),
[`materialization.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/materialization.go),
[`messages.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/messages.go)
(`PersistMessageContext`, `persistMessageWith`), and
[`remoteimage/archive.go`](https://github.com/kenn-io/msgvault/blob/main/internal/remoteimage/archive.go).

Occurrence writes and statistics are separate SQL work after the message
commit. Under the message lock, `RecomputeRemoteImageAttachmentStatsContext`
preserves expected MIME counts while the latest active outcome requires MIME;
image-only debt does not preserve that expectation. A count is not an inventory
of saved occurrences. See [`remote_images.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/remote_images.go).

### Which guards and cancellation boundaries apply?

Full and incremental sync use `Store.ScopedToSync(sourceID, runID)`. Mutating
transactions fence that running generation, and message writes check source
ownership. The Syncer must align `acquisitionSyncID` and its embedded
`MessagePersistData.SyncRunItem.SyncRunID` with this view: the transaction inserts
that outcome through the private helper, not the public ledger API.
`RecordSyncRunItemContext` additionally rejects a different run ID on a scoped
view; an unscoped Store remains a separate generic API. The generation fence
guards SQL mutations, not files already published. See [`store/sync.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/sync.go)
and [`store.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/store.go).

Local MIME repair checks the saved message's ID, source ID, and source message
ID again under the message lock before restoring occurrences. It preserves
existing metadata and provider-owned rows, and rejects conflicting evidence.
See [`RestoreMIMEAttachmentRecordsContext`](https://github.com/kenn-io/msgvault/blob/main/internal/store/attachment_restore.go).

After the message commit, `persistMessage` uses `context.WithoutCancel` to finish
local MIME writes and statistics. Image downloads and their publication retain
the cancellable context; durable debt allows another attempt. The image
archiver finishes statistics for already committed image rows with
`WithoutCancel`. Removing cancellation does not bypass generation or identity
guards and does not guarantee that a local write succeeds.

### What permits progress, and what remains retryable?

[`item_errors.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/item_errors.go)
separates mandatory `recordAcquisitionFailure` from warning-only `recordSyncItem`.
Gmail must stop if required acquisition debt cannot be recorded before a
checkpoint or completion could hide it. Best-effort events cannot replace this
write. Existing-message History labels and deletions have no acquisition retry
substitute: their persistence errors retain History. Recovery still refreshes
labels when file work fails, and its request fingerprint prevents resuming an
older unsafe enumeration prefix. See [`incremental.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/incremental.go)
and [`gmail_recovery.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/gmail_recovery.go).

`ListUnresolvedSyncItemErrors` chooses each source message's greatest item ID in
fetch/ingest phases, within runs whose ID is at least the latest completed
incremental run's ID (chosen by `completed_at`, then run ID). Only then does it
keep errors; the Syncer filters recognized phase/kind pairs and usable message
IDs. An incremental run must re-record still-failing work before completion
moves that boundary. Free-form diagnostics do not select replay candidates.

Full sync replays local file obligations before enumeration, even for messages
absent from the provider listing; it still contacts the provider for profile
and labels first. MIME repair reads retained RAW; image retries can require
network access and enabled archival. The local
[`repair-derived --attachment-message`](../cli-reference.md#repair-derived)
entry point repairs selected Gmail MIME occurrences without Gmail access; it
does not automatically drain the acquisition ledger.

A `committedMessageError` means the message exists despite failed post-commit
work. Full, MessagesAdded, LabelsAdded, and replay retain that identity for
counts, discovery, and later label events. Page `acknowledged` is distinct from
materialization settlement. A run marked `completed` may contain errors and
retryable work. Some ordinary full-sync checkpoint failures are warnings;
pinned recovery checkpoints have stricter stop-on-error rules.

`ListSyncRunItemsContext(status="error")` is a display projection, not the active
retry selector. Before applying its limit, it hides settled synthetic file
markers identified by the persisted `SyncRunItemFilesPendingMessage` text;
genuine historical failures remain. Preserve that exact marker separately from
free-form diagnostics. The unfiltered ledger remains readable. SQLite archive
GC can instead retire fetch/ingest errors as `skipped` for identities actually
purged in the same transaction, preserving kind and diagnostic text
([`gc.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/gc.go)).

Commit, page acknowledgement, settlement, GC retirement, and run completion
describe different units. None of these states, nor an empty retry selection,
proves archive completeness or continued physical integrity. Remote-image
reference reuse is not a fresh read of the saved bytes.

### Existing behavioral coverage

These tests exercise the boundaries above; their presence is not a claim that
every backend or a real power-loss scenario is covered.

| Boundary | Existing tests |
|---|---|
| RAW/obligation rollback, process exit, settlement failure, stale worker | [Transaction and settlement cases](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_revision_test.go) |
| Durable retry after directory-sync failure | [Directory-sync retry](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_stability_test.go) |
| Replay outside listing, mandatory debt, four committed-message routes | [Full replay and committed-message cases](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_completion_test.go) |
| Expected MIME counts, recovery labels, SQLite GC | [Materialization interaction cases](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_review_test.go) |
| Latest outcome and completed-incremental replay boundary | [Store replay selection](https://github.com/kenn-io/msgvault/blob/main/internal/store/sync_test.go) |
| Committed image statistics after cancellation | [Image archival cancellation](https://github.com/kenn-io/msgvault/blob/main/internal/remoteimage/archive_test.go) |
| Synthetic marker projection before the display limit | [Sync-run status hydration](https://github.com/kenn-io/msgvault/blob/main/internal/api/attachment_status_test.go) |

## Token Storage

By default, OAuth tokens are stored as JSON files per account:

```
tokens/
├── personal@example.com.json
├── work@example.com.json
└── discord_<bot-user-id>.json
```

Token files are owner-only. Protect this directory: its credentials grant the
configured provider access. Discord bot records may be shared by several guild
sources through an optional binding label and are removed only after the last
referencing source is deleted.

Google credentials and tokens can use
[configured commands](/docs/configuration/#command-backed-google-credentials-and-tokens).
The external store owns the token JSON; the token directory holds coordination
locks. Other providers retain their file storage.

## Compression

| Data | Format | Ratio |
|---|---|---|
| Raw MIME | zlib in database BLOB/BYTEA | ~3-5x compression |
| Parquet | Snappy (DuckDB default) | ~10x vs raw SQLite |
| Attachments | Stored as-is (already compressed formats) | — |
