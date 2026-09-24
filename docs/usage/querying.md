---
last_edited: "2026-09-23"
title: SQL Queries
description: Run read-only DuckDB queries against the analytics cache.
---

Use `msgvault query` for ad-hoc analysis across email, chat, calendar, and
meeting data in the Parquet analytics cache. The daemon runs one read-only
DuckDB statement. It rejects writes, session changes, and multiple statements.

## Basic Usage

```bash
msgvault query "SELECT count(*) AS total FROM messages"
```

The command takes a single argument: the SQL string. A usable stale cache stays
queryable. The JSON result reports the committed publication time and, when
known, why the cache is stale and how many additions are pending. CSV and table
output print that cache information to stderr.

`msgvault query --fresh "SELECT 1"` requests a coalesced background freshness
check, building the cache if needed.
With automatic builds enabled, a missing or incompatible cache also starts recovery.
In either case the command reports the accepted job ID on stderr and returns
no rows. Retry the query after the job reaches `published` at
`GET /api/v1/cache-builds/{job_id}`.

## Output Formats

Control output format with the `--format` flag:

```bash
# JSON (default)
msgvault query "SELECT count(*) AS total FROM messages"

# CSV
msgvault query --format csv "SELECT from_email, message_count FROM v_senders LIMIT 5"

# Aligned text table
msgvault query --format table "SELECT from_email, message_count FROM v_senders LIMIT 5"
```

| Format | Description |
|---|---|
| `json` | JSON object with `columns`, `rows`, `row_count`, and optional `cache` metadata (default) |
| `csv` | Standard CSV with a header row |
| `table` | Aligned text table with separator line and `(N rows)` footer |

## Available Views

### Base views

These map directly to the Parquet files in `~/.msgvault/analytics/`.

| View | Key Columns |
|---|---|
| `messages` | id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, sender_id, message_type, year, month, deleted_from_source_at |
| `participants` | id, email_address, domain, display_name, phone_number |
| `message_recipients` | message_id, participant_id, recipient_type (from/to/cc/bcc), display_name, email_address, envelope_address |
| `labels` | id, name |
| `message_labels` | message_id, label_id |
| `attachments` | message_id, filename, size |
| `conversations` | id, source_conversation_id, title, conversation_type |
| `sources` | id, source_type |

`message_recipients.email_address` is the recipient's address: the address
written in the message header when one was recorded, otherwise the
participant's current address. It is NULL only for participants without an
email address, such as phone-number contacts. `envelope_address` is the header
address exactly as written and is NULL when none was recorded, which covers
chat and calendar rows and mail imported before v0.19.0.

### Convenience views

Pre-joined and aggregated views for common queries.

| View | Description |
|---|---|
| `v_messages` | Messages with resolved sender (from_email, from_name, from_domain, from_phone) and labels as a JSON array |
| `v_senders` | Per-sender aggregates: from_email, from_name, from_domain, message_count, total_size, attachment_size, attachment_count, first_message_at, last_message_at |
| `v_domains` | Per-domain aggregates: domain, message_count, total_size, sender_count |
| `v_labels` | Per-label: name, message_count, total_size |
| `v_threads` | Per-conversation: conversation_id, source_conversation_id, conversation_title, conversation_type, message_count, first_message_at, last_message_at, participant_emails (JSON array) |

## Example Queries

### Top senders by message count

```bash
msgvault query "
  SELECT from_email, from_name, message_count, total_size
  FROM v_senders
  ORDER BY message_count DESC
  LIMIT 20
"
```

### Domain breakdown

```bash
msgvault query "
  SELECT domain, message_count, sender_count,
         total_size / (1024*1024) AS size_mb
  FROM v_domains
  ORDER BY message_count DESC
  LIMIT 20
"
```

### Messages per month for a given year

```bash
msgvault query "
  SELECT month, count(*) AS messages
  FROM messages
  WHERE year = 2024
  GROUP BY month
  ORDER BY month
"
```

### Filter by message type

Mixed archives store email, calendar events, Teams and Discord messages, and
text-message imports in the same `messages` table. Use the `message_type`
column to keep SQL reports scoped:

```bash
# Teams activity by month
msgvault query --format table "
  SELECT month, count(*) AS messages
  FROM messages
  WHERE message_type = 'teams'
  GROUP BY month
  ORDER BY month DESC
  LIMIT 12
"

# Recent calendar records in the archive
msgvault query --format table "
  SELECT sent_at, subject, from_email
  FROM v_messages
  WHERE message_type = 'calendar_event'
  ORDER BY sent_at DESC
  LIMIT 20
"

# Discord activity by channel or thread
msgvault query --format table "
  SELECT t.conversation_title, count(*) AS messages
  FROM v_threads t
  JOIN messages m ON m.conversation_id = t.conversation_id
  WHERE m.message_type = 'discord'
  GROUP BY t.conversation_id, t.conversation_title
  ORDER BY messages DESC
  LIMIT 20
"
```

Known values are `email`, `google_chat`, `calendar_event`,
`meeting_transcript`, `beeper`, `teams`, `discord`, `slack`, `sms`, `mms`,
`rcs`, `whatsapp`, `imessage`, `fbmessenger`, `synctech_sms_call`,
`google_voice_text`, `google_voice_call`, and `google_voice_voicemail`.

### Label statistics

```bash
msgvault query "
  SELECT name, message_count, total_size / (1024*1024) AS size_mb
  FROM v_labels
  ORDER BY message_count DESC
"
```

### Largest attachments

```bash
msgvault query --format table "
  SELECT a.filename, a.size / (1024*1024) AS size_mb,
         m.subject, m.sent_at
  FROM attachments a
  JOIN messages m ON a.message_id = m.id
  ORDER BY a.size DESC
  LIMIT 20
"
```

### Thread activity

```bash
msgvault query "
  SELECT conversation_title, message_count,
         first_message_at, last_message_at
  FROM v_threads
  ORDER BY message_count DESC
  LIMIT 10
"
```

### Filter by label

The `labels` column in `v_messages` is a JSON array string. Use DuckDB's `list_contains` to filter:

```bash
msgvault query "
  SELECT subject, from_email, sent_at
  FROM v_messages
  WHERE list_contains(labels::VARCHAR[], 'INBOX')
  ORDER BY sent_at DESC
  LIMIT 20
"
```

Or join through the base tables for more control:

```bash
msgvault query "
  SELECT m.subject, m.sent_at
  FROM messages m
  JOIN message_labels ml ON m.id = ml.message_id
  JOIN labels l ON ml.label_id = l.id
  WHERE l.name = 'INBOX'
  ORDER BY m.sent_at DESC
  LIMIT 20
"
```

## Tips

**Partition pruning.** The `messages` view is hive-partitioned by year. Adding `WHERE year = 2024` to queries on `messages` lets DuckDB skip irrelevant Parquet files, which speeds up queries on large archives.

**Cache freshness.** A usable stale publication serves queries during
`min_rebuild_interval`. Once that interval expires, a query can schedule a
background freshness check and refresh. Set `auto_build_cache = false` to
disable automatic builds; `--fresh` still requests one explicitly. A query
never waits for the build to finish.

**Pipe-friendly.** JSON and CSV output modes are designed for piping into other tools (`jq`, `csvkit`, `xsv`, etc.). Use `--format csv` for spreadsheet workflows or `--format json` for programmatic consumption.

**Full DuckDB SQL.** You have access to DuckDB's full SQL dialect, including window functions, CTEs, `UNNEST`, `list_contains`, and all built-in functions. See the [DuckDB documentation](https://duckdb.org/docs/sql/introduction) for the full SQL reference.

## See Also

For pre-built analytics commands (top senders, domains, labels, overall stats), see [Analytics & Stats](/docs/usage/analytics/). The `query` command is for when you need more flexibility than those commands provide.
