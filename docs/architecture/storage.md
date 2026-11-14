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
| `message_type` | TEXT | `email`, `calendar_event`, `meeting_transcript`, `beeper`, `chatwoot`, `teams`, `discord`, `sms`, `mms`, `whatsapp`, `imessage`, `fbmessenger`, `telegram`, `synctech_sms_call`, `google_voice_text`, `google_voice_call`, `google_voice_voicemail`, `feed_item`, or a type chosen by a [message import](../usage/message-import.md) caller |
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

Gmail sync also reads these rows as its list of work to retry. An `error` row
in the `fetch` or `ingest` phase tells a later sync to fetch that message
again or finish saving its files. The
[Gmail durability protocol](#gmail-durability-protocol) explains how sync
selects and clears that work. Those rules differ from the ones that decide
which errors a run displays.

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

SQLite uses the `messages_fts` FTS5 virtual table. PostgreSQL stores a
`search_fts` `tsvector` on `messages`, with a GIN index. Both power
`msgvault search`; [Search Ranking Across Backends](search-ranking.md) owns
ranking and query differences.

#### Which indexing entry should a caller use?

Choose the entry by its commit and error contract. A saved message does not
guarantee a searchable entry. The Store returns errors; each caller decides
whether to fail its operation, count an error, or log a warning.

| Entry and typical production callers | Transaction and error contract |
|---|---|
| `PersistMessageContext` with `MessagePersistData.FTS`: Gmail/IMAP Syncer, Discord and message-import writers | Indexes inside the message transaction through `upsertFTSOrUnsearchable`. Recognized PostgreSQL overflow permits a terminal unsearchable entry; other errors roll back the transaction. Omitting `FTS` does not create a search entry. |
| `UpdateArchivedEmailTextContext`: retained-MIME email repair and Heal through `internal/rederive` | Compares first, then locks and repairs derived text and search together using the same savepoint policy. A comparison overflow can match an existing terminal entry; other comparison errors propagate. |
| `UpsertFTSContext`: mail-file, Beeper, Slack, Teams, WhatsApp, Facebook Messenger and Synctech SMS importers; calendar sync | Writes the supplied `FTSDoc` directly, without comparing it first. Returns indexing errors, including overflow. It neither saves the message nor records a terminal overflow state. |
| `IndexStoredMessageContext`: Google Voice and iMessage imports | Loads the stored body and envelope, calls `FTSMatches`, and writes only a mismatching entry. Load, comparison and upsert errors propagate, including overflow during comparison. It has no terminal overflow policy. |
| `UpdateMessageDerivedText`: Beeper repair | Updates body, snippet and FTS in one transaction using the direct dialect writer. Every FTS error rolls back that repair, including overflow. Previously committed provider data remains. |

The two standalone entries, `UpsertFTSContext` and
`IndexStoredMessageContext`, use `withSyncMessageWriteContext`: PostgreSQL and
sync-scoped views open a transaction with the maintenance gate and message
ownership checks; unscoped SQLite writes directly. These transactions do not
include a caller's earlier message commit and do not use `derived_fts`.
Sync-scoped views still check generation and ownership when FTS is unavailable.
The entries otherwise skip indexing when FTS is unavailable.

Google Voice and iMessage log an indexing error and continue after saving the
message. Several direct-upsert callers also continue: Beeper and Teams count
errors, while calendar sync and Facebook Messenger log warnings. Slack
propagates cancellation and deadline errors. Mail-file ingestion logs errors
and, in strict mode, also returns an incomplete-ingestion error. These are
caller policies, not a best-effort guarantee of either Store API.

Use atomic persistence when search must participate in the message commit
under the recognized-overflow policy. Use the standalone entries to index
already saved data, handling their errors explicitly. Use
`IndexStoredMessageContext` when the stored body and envelope are the desired
document; use `UpsertFTSContext` when the caller supplies the search fields.
Use the corresponding repair API for derived text rather than treating every
repair as having the email repair contract.

#### What does recognized overflow mean?

`upsertFTSOrUnsearchable` requires an existing transaction. It creates
`SAVEPOINT derived_fts` on both backends before attempting the dialect write.
PostgreSQL's `IsFTSValueTooLargeError` recognizes SQLSTATE `54000`; it does not
inspect the error message. SQLite's classifier always returns false.

On a recognized error, the helper rolls back to the savepoint so PostgreSQL's
failed statement does not leave the outer transaction aborted. It then sets
`search_fts = ''::tsvector`, `search_fts_fields = NULL`, and
`indexing_version = CurrentFTSIndexingVersion`, unless the vector is already
empty at that version. It logs a warning and releases the savepoint. Other
SQL errors, including failure to write the terminal state, propagate and cause
the enclosing persistence or repair transaction to roll back.

Terminal means ordinary backfill will not select this entry again at the
current indexing version. It does not mean the message was discarded, its
attachments are complete, or explicit reindexing can never succeed. The
helper still attempts indexing on a later write; its `changed` result is
false if overflow finds an already terminal entry. A successful outer commit
can therefore preserve the original and derived text while text search finds
nothing. The [Gmail durability protocol](#gmail-durability-protocol)
owns file obligations and settlement separately.

#### What is compared in the current format?

`CurrentFTSIndexingVersion` is **3** for PostgreSQL. `preparePostgresFTSDoc`
prepares subject, body, sender, To and Cc separately: it bounds each input to
700,000 bytes without splitting a UTF-8 rune, then 600,000 runes, normalizes
Unicode letters and numbers into lowercase space-separated tokens, and caps
bytes again. These limits reduce overflow risk; they do not guarantee that the
combined vector fits PostgreSQL's 1,048,575-byte limit.

`pgFTSVectorFields` builds weighted `simple` vectors with separator lexemes
between fields. `search_fts_fields` stores the five prepared texts, in order
subject, sender, To, Cc, body, only when the preparation counts more than
16,000 positions (including four separators) or more than 255 occurrences of
one token. Search uses those fields to verify phrases when vector positions
can saturate. Unsaturated documents store NULL there.

PostgreSQL `FTSMatches` constructs that same expected vector and compares the
vector, `search_fts_fields` with NULL-safe equality, and the current version.
It can fail while constructing the expected vector, before any upsert. SQLite
compares the five stored FTS5 text columns to the supplied document; it has no
PostgreSQL version or auxiliary fields. Both dialects return false for an
absent entry. `IndexStoredMessageContext` leaves a matching entry unwritten
and can create a missing one.

Missing entries, different versions, mismatching documents, and SQL comparison
errors are distinct outcomes. A document with no searchable text is also
distinct from overflow: v3's vector builder still inserts separator lexemes
when the prepared fields contain no tokens. An empty vector records no
durable reason explaining how it arose; do not infer its history from the
vector alone. The terminal check tests only an empty vector and current
version, not a separately persisted overflow cause.

The read-only [FTS diagnostic](../cli-reference.md#diagnose-fts) compares bounded
pages with the stored-message document rule, including the v3 auxiliary fields.
It reports missing, obsolete and differing entries, and inconclusive comparisons.
Its per-message snapshots and resume cursor do not freeze an archive across pages
or verify physical postings. Backfill readiness remains a cheaper, narrower probe.

#### How can indexing resume?

`BackfillFTSContext` fills absent SQLite entries and PostgreSQL entries whose
vector is NULL or whose version differs from the current one. Each ID-range
batch commits independently. PostgreSQL reads and locks stored message data,
uses the same preparation as direct upserts, and retries a failed batch row
by row only for recognized overflow. An overflowing row is marked terminal;
any other error stops the pass. Already committed batches survive failure.
The daemon's search-index owner can run this backfill after an upgrade.

Backfill is not a freshness comparison: a current non-NULL vector with wrong
auxiliary fields is not selected just for that mismatch. A version change
makes terminal entries eligible again. A changed body invalidates its search
entry; an explicit upsert, stored-message indexing, or email repair can also
retry it. `RebuildFTSContext`, exposed by `msgvault rebuild-fts`, resets the
index before backfilling, including terminal entries at the same version.
Rebuilding does not make an oversized document indexable.

Reindexing reads retained data rather than fetching a provider copy. Stored
envelope reconstruction is shared by comparison, email repair and backfill;
the PostgreSQL backfill also recovers Beeper sender names from retained JSON.
It cannot preserve arbitrary search-only terms that a caller supplied but
never stored. Index availability, version and non-NULL state alone do not
prove coverage or freshness.

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

msgvault writes a loose attachment file in one of two ways. The entry points in
[`internal/export/store_attachment.go`](https://github.com/kenn-io/msgvault/blob/main/internal/export/store_attachment.go)
choose the `packstore` policy.

| Entry point | Policy | What success means |
|---|---|---|
| `StoreAttachmentFile` / `StoreAttachmentFileIncludingEmpty` | `AtomicPublication` | The complete file appears in one step, so a reader never sees part of it. The write does not require an fsync of the file or its directory. |
| `StoreAttachmentFileDurable` | `DurablePublication` | msgvault has fsynced the file content and its directory entries. If the content was already stored, it verified the existing bytes first. |

Atomic publication makes no promise that the file survives a crash. Gmail MIME
sync, remote-image archival, and maintenance use the durable entry point for
that reason.

`StoreAttachmentFileDurable` returns a receipt and an error, and it can return
both at once. That happens when the bytes became visible but the directory sync
did not finish. Check the error before you record success. A receipt without
that check does not prove the bytes are durable.

The receipt's `Created` field is true when the call wrote a new blob and false
when it found the content already stored. Cleanup code uses it to pick out a
new blob, which it may remove only after checking that nothing references it.

A retry on a loose file that already exists and is readable verifies the bytes
again and repeats the required syncs.

Neither entry point writes an `attachments` row. Recording which message a
file belongs to is the caller's job.

## Gmail durability protocol

Gmail sync saves a message in two stages. First it commits the raw MIME, the
original bytes Gmail returned, to the database. Then it saves the files that
come from that message. Those are its MIME attachments and the remote images
its HTML refers to. This section calls the second stage file work.

No transaction can cover both the database and the content-addressed attachment
store. So the first stage also writes a row to `sync_run_items` that says the
file work is still pending. If sync stops before the files are saved, that row
survives, and a later sync finds it and finishes the work.

These rules belong to the Gmail Syncer. An empty `Options.SourceType` is the
legacy way to select its Gmail mode. IMAP keeps its best-effort attachment
handling, and other importers have their own rules for when a message is
complete.

### Who does each step?

1. **The Syncer decides what file work a message needs.**
    `requiredMaterialization` starts with any work still pending for the
    message, then adds up to two requirements:

    - MIME attachments, when an attachment directory is configured and the
      message has a nonempty attachment or its MIME did not parse completely.
    - Remote images, when remote-image archival is enabled and the message is
      an email with an HTML body.

    The two are independent, so one can finish while the other stays pending.
    The Syncer tracks them in in-memory maps during a run. The
    `sync_run_items` rows are what survive a restart.

2. **The Store commits the message and the pending row together.** The
    Syncer's `persistMessage` passes in the raw MIME, the parsed message with
    its recipients and labels, a search document, and the pending row. That
    row has phase `ingest` and status `error`, and its kind names MIME, images,
    or both. `PersistMessageContext` writes all of it in one SQL transaction.
    If the database rejects the pending row, the message rolls back with it.

    The Store writes the search document when full-text indexing is available,
    following `upsertFTSOrUnsearchable`. If PostgreSQL reports that the
    document is too large to index, the transaction can still commit with the
    message marked unsearchable. Any other indexing error rolls the
    transaction back. The [full-text index contract](#full-text-index) owns
    the entry-point policies and terminal-state details.

3. **The export package writes the bytes, and the Store records which message
    they belong to.** After the commit, `storeAttachment` calls
    `StoreAttachmentFileDurable` and then `UpsertAttachmentRecord`.
    `remoteimage.ArchiveWithOptions` works the same way for images. It writes
    the downloaded bytes, then calls `UpsertRemoteImageAttachment`. If the row
    write fails, the file stays in the store without a row.

    Identical bytes share one stored file, but each use keeps its own row. A
    MIME attachment is identified by its part key and a remote image by its
    `remote-image:` URL identity. Those identities stay distinct even when the
    content hash is the same.

4. **The Syncer settles the work only after every required operation
    succeeds.** To settle, `settleAcquisition` writes a row with phase `fetch`,
    status `resolved`, and kind `acquired`. Then it removes the message from
    both pending maps. If that write fails, the work stays pending.
    `repairPendingAttachments` tries the images even when the MIME repair
    fails. Once a message is committed, a later fetch failure or a provider
    404 must not replace its pending file work.

The code for these steps is in these files:

- `persistMessage` and `storeAttachment` in
  [`sync.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/sync.go)
- `requiredMaterialization` and `repairPendingAttachments` in
  [`materialization.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/materialization.go)
- `PersistMessageContext` and `persistMessageWith` in
  [`messages.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/messages.go)
- `ArchiveWithOptions` in
  [`remoteimage/archive.go`](https://github.com/kenn-io/msgvault/blob/main/internal/remoteimage/archive.go)

Attachment rows and a message's attachment statistics are separate SQL writes
that happen after the message commits.
`RecomputeRemoteImageAttachmentStatsContext` locks the message and looks at the
row sync would select for a retry. While that row still requires MIME work, the
function leaves the expected MIME count in place. Pending image work alone
does not hold the count. The count says how many attachments the message
should have. Do not read it as a list of the attachments saved so far. See
[`remote_images.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/remote_images.go).

### What stops a stale run, and what does cancellation interrupt?

Full and incremental sync write through `Store.ScopedToSync(sourceID, runID)`.
That view of the Store rejects a write when its run is no longer the running
run for the source. Message writes also check that the message belongs to that
source.

The Syncer has one thing to keep straight itself. `acquisitionSyncID`, and the
`SyncRunID` inside the `MessagePersistData.SyncRunItem` it passes to the Store,
must match the run ID of the scoped view. The message transaction inserts that
row through a private helper, which skips the run ID check that the public
ledger API makes. The public `RecordSyncRunItemContext` rejects a different run
ID on a scoped view. An unscoped Store is a separate, generic API without that
check.

This guard covers SQL writes. It does nothing about files a run has already
written to the attachment store. See
[`store/sync.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/sync.go)
and [`store.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/store.go).

Local MIME repair restores missing attachment rows through
[`RestoreMIMEAttachmentRecordsContext`](https://github.com/kenn-io/msgvault/blob/main/internal/store/attachment_restore.go).
It locks the message and checks again that the saved message's ID, source ID,
and source message ID still match before it writes anything. It leaves
existing metadata and provider-owned rows as they are. If an existing row
conflicts with what the repair expects, the repair fails.

Cancellation stops network work and lets local work finish:

- After the message commits, `persistMessage` switches to
  `context.WithoutCancel` for the local MIME writes and the statistics update.
- Image downloads, and the writes that follow them, keep the cancellable
  context. If cancellation interrupts them, the pending row is still there and
  a later sync tries again.
- The image archiver uses `WithoutCancel` for one thing. It finishes the
  statistics for image rows it has already committed.

`WithoutCancel` only ignores cancellation. The stale-run and identity checks
still apply, and a local write can still fail.

### When can sync move on, and what does it retry?

Sync may move past a failed message only after it has recorded the failure.
[`item_errors.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/item_errors.go)
has two ways to record one:

- `recordAcquisitionFailure` must succeed. If Gmail sync cannot write the row,
  it stops. A checkpoint or a completed run without that row would hide the
  failed message from later runs.
- `recordSyncItem` only logs a warning when its write fails. Never use it in
  place of `recordAcquisitionFailure`.

Gmail History also reports label changes and deletions for messages already in
the archive. Those have no retry row to fall back on. If msgvault cannot save
one, the run keeps its place in History so the next run reads the same changes
again. See [`incremental.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/incremental.go).

History recovery runs after Gmail expires the History cursor. It still
refreshes a message's labels when that message's file work fails. It also
checks the request fingerprint saved with its checkpoint. That check keeps it
from resuming partway through an older enumeration that is not safe to
continue. See
[`gmail_recovery.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/gmail_recovery.go).

`ListUnresolvedSyncItemErrors` picks the work to retry, in this order:

1. Find the source's latest completed incremental run, ordered by
    `completed_at` and then run ID. Ignore every run with a lower ID.
2. In the runs that remain, take each source message's newest row in the
    `fetch` or `ingest` phase. Newest means the greatest item ID.
3. Keep that row only if its status is `error`.

The Syncer then drops rows whose phase and kind it does not recognize, and rows
without a usable message ID. The free-form error text plays no part in the
selection.

Because step 3 comes last, a newer `resolved` or `skipped` row hides an older
error for the same message. Because of step 1, completing an incremental run
hides rows from earlier runs. An incremental run must record everything that
is still failing again before it completes.

A full sync retries pending file work before it lists the mailbox, though it
still contacts Gmail for the profile and labels first. The retry includes
messages that Gmail no longer lists. MIME repair reads the raw MIME already in
the archive. Retrying images can need network access and remote-image
archival turned on.

[`repair-derived --attachment-message`](../cli-reference.md#repair-derived)
repairs the MIME attachment rows of the Gmail messages you select, without
contacting Gmail. It does not work through the pending rows in
`sync_run_items` for you.

`persistMessage` returns a `committedMessageError` when the message was saved
but work after the commit failed. The message exists. Full sync, History
`MessagesAdded`, History `LabelsAdded`, and replay all keep its identity for
counts, identity discovery, and later label events.

Several states look like "done" and mean less than that:

- A page reports a message as `acknowledged` once it has handled the message.
  The message's file work can still be pending.
- A run marked `completed` can contain errors and work to retry.
- A full sync treats some checkpoint failures as warnings and carries on.
  Pinned recovery checkpoints are stricter and stop the run on error.

`ListSyncRunItemsContext` with `status="error"` builds the error list that
people see. Sync does not use it to choose retries. It hides one kind of row,
a pending row whose file work has since settled. It recognizes a pending row
by the exact text stored in `SyncRunItemFilesPendingMessage`, and it filters
before it applies the limit. Real failures from earlier attempts stay in the
list. Keep that text exact, and keep it separate from free-form error
messages. The hidden rows stay in the table, and an unfiltered query still
returns them.

SQLite archive GC clears retries another way. When it purges a message, the
same transaction changes that message's `fetch` and `ingest` error rows to
`skipped`. The kind and the error text stay as they were. See
[`gc.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/gc.go).

Commit, page acknowledgement, settlement, GC retirement, and run completion
each describe a different unit of work. None of them proves that the archive
is complete or that the stored files are still intact. An empty retry list
does not prove it either. When remote-image archival reuses an existing
reference, it does not read the saved bytes again.

### Which tests cover this?

These tests exercise the behavior described above. Do not take them as proof
that every backend, or a real power loss, is covered.

| Behavior | Tests |
|---|---|
| The raw MIME and its pending row roll back together, a process exit, a failed settle, a stale worker | [`attachment_revision_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_revision_test.go) |
| A durable write retried after a directory sync fails | [`attachment_stability_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_stability_test.go) |
| Retrying messages Gmail no longer lists, failure rows that must be recorded, the four callers that handle `committedMessageError` | [`attachment_completion_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_completion_test.go) |
| Expected MIME counts, labels during recovery, SQLite GC | [`attachment_review_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/sync/attachment_review_test.go) |
| Choosing the newest row, and the completed incremental run as the cutoff | [`store/sync_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/store/sync_test.go) |
| Statistics for committed image rows after cancellation | [`remoteimage/archive_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/remoteimage/archive_test.go) |
| Hiding settled pending rows before the display limit applies | [`api/attachment_status_test.go`](https://github.com/kenn-io/msgvault/blob/main/internal/api/attachment_status_test.go) |

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
