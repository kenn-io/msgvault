---
title: Data Storage
description: Database schema, Parquet analytics cache, content-addressed attachments, and token storage.
---

## Storage Layers

| Layer | Role | Location |
|---|---|---|
| SQLite | Default system of record | `~/.msgvault/msgvault.db` |
| PostgreSQL | Optional system of record | `[data].database_url` |
| Parquet | Analytics cache | `~/.msgvault/analytics/` |
| Attachments | Content-addressed loose files and sealed packs | `~/.msgvault/attachments/` |
| Tokens | OAuth credentials | `~/.msgvault/tokens/` |

## Archive Database

All message data (metadata, labels, participants, and raw MIME) lives in the configured archive database. SQLite is the default and stores the archive at `~/.msgvault/msgvault.db`. PostgreSQL is opt-in through `[data].database_url` and is intended for new archives or fresh re-syncs.

### Core Tables

**sources** -- Accounts and import sources with sync state.

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Auto-increment |
| `source_type` | TEXT | Provider/import type, for example `gmail`, `imap`, `gcal`, `teams`, `discord`, `beeper`, `mbox`, `whatsapp`, `granola`, or `circleback` |
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
| `message_type` | TEXT | `email`, `calendar_event`, `meeting_transcript`, `beeper`, `teams`, `discord`, `sms`, `mms`, `whatsapp`, `imessage`, `fbmessenger`, `synctech_sms_call`, `google_voice_text`, `google_voice_call`, `google_voice_voicemail` |
| `sent_at` | DATETIME | Send timestamp |
| `sender_id` | INTEGER FK | References `participants` |
| `subject` | TEXT | Message subject |
| `body_text` | TEXT | Plain text content |
| `snippet` | TEXT | Preview excerpt |
| `size_estimate` | INTEGER | Approximate size in bytes |
| `has_attachments` | BOOLEAN | Attachment flag |
| `deleted_at` | DATETIME | Soft delete timestamp |

**message_raw** -- Raw provider payload storage, compressed with zlib.

| Column | Type | Description |
|---|---|---|
| `message_id` | INTEGER PK/FK | References `messages` |
| `raw_data` | BLOB | Compressed MIME or provider JSON data |
| `compression` | TEXT | `zlib` |

**participants** -- Unified contacts.

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

**labels / message_labels** -- Gmail labels (many-to-many).

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

`source_import_items.checksum` is nullable because some providers or legacy rows
may not have a stable checksum. msgvault treats a null checksum as an empty
string when checking already-imported source items.

### Full-Text Index

SQLite uses an FTS5 virtual table named `messages_fts`. PostgreSQL uses a `search_fts` `tsvector` column on `messages` with a GIN index.

Both power `msgvault search`, but the rankers differ. See [Search Ranking Across Backends](/architecture/search-ranking/).

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

There is currently no SQLite to PostgreSQL migration command. Use PostgreSQL for a new archive or re-sync/import into an empty PostgreSQL database. See [PostgreSQL Backend](/architecture/postgresql/) for setup and operational notes.

## Parquet (Analytics Cache)

The TUI needs to aggregate across your entire archive (top senders, domains, labels, time series) and return results instantly as you drill down. SQLite JOINs across normalized tables cannot do this at interactive speeds on large archives. msgvault solves this on the default SQLite backend with denormalized Parquet files queried by an embedded DuckDB engine, delivering aggregate queries hundreds of times faster than SQLite.

The Parquet cache is disposable and can be rebuilt at any time. Aggregate views never trigger a build mid-session: with `auto_build_cache = true` (the default) the daemon builds a stale or missing cache synchronously at startup — incremental builds take seconds — and then serves DuckDB over it, refreshing the cache after scheduled syncs and ingest commands. Cache builds hold an exclusive cross-process lock and queries hold it shared, so a rebuild never removes Parquet files out from under a running query. `msgvault build-cache` builds the cache on demand. PostgreSQL archives currently use live SQL for aggregate views rather than this Parquet acceleration layer.

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
└── _last_sync.json
```

Messages are partitioned by year for efficient time-range queries. The entire analytics cache is typically a few MB even for hundreds of thousands of messages, compared to the much larger SQLite database with full message bodies.

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
reference](/cli-reference/#pack-attachments) and [Backup](/usage/backup/) guide
for maintenance and restore behavior.

## Token Storage

OAuth tokens are stored as JSON files per account:

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

## Compression

| Data | Format | Ratio |
|---|---|---|
| Raw MIME | zlib in database BLOB/BYTEA | ~3-5x compression |
| Parquet | Snappy (DuckDB default) | ~10x vs raw SQLite |
| Attachments | Stored as-is (already compressed formats) | — |
