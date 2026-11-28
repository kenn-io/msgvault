---
last_edited: "2026-09-27"
title: SQL Queries
description: Run read-only DuckDB queries against the analytics cache.
---

Run `msgvault query` for ad-hoc analysis across the email, chat, calendar, and
meeting data in your Parquet analytics cache. The daemon executes one read-only
DuckDB statement and rejects writes, session changes, and multiple statements.

## Basic usage

```bash
msgvault query "SELECT count(*) AS total FROM messages"
```

The command takes one argument, the SQL string. A usable stale cache remains
queryable. In JSON output, the result reports the committed publication time
and, when known, why the cache is stale and how many additions are pending. CSV
and table output print the publication time, cache generation, and any stale
reason to stderr.

Running `msgvault query --fresh "SELECT 1"` waits for a freshness check and any
required rebuild before returning rows, including archive writes committed
before your request. If a build is already running, the daemon queues a
follow-up check. When automatic builds are enabled, a missing or incompatible
cache also starts recovery. In either case, the command reports the job ID on
stderr and waits. If the build fails or the wait is interrupted, the command
exits with an error.

## Output formats

Use the `--format` flag to pick an output format:

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

## Available views

### Base views

These views map directly to the Parquet files in `~/.msgvault/analytics/`.

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

`messages.rfc822_message_id` is NULL when the stored identifier contains invalid
UTF-8. The archive keeps the original bytes, and `repair-encoding` reports these
IDs without rewriting them. See the [encoding repair limits](../cli-reference.md#repair-encoding).

`message_recipients.email_address` holds the recipient's address: the address
written in the message header when one was recorded, or the participant's
current address otherwise. It is NULL for participants without an email address,
such as phone-number contacts, and for addresses whose stored bytes are invalid
UTF-8. `envelope_address` is the header address written verbatim; it is NULL
when none was recorded, such as in chat and calendar rows or mail imported
before v0.19.0, or when its stored bytes are invalid UTF-8. A recorded envelope
is authoritative even when damaged, so the participant's current address is
never substituted for it and `email_address` stays NULL instead. Damaged
addresses export as unknown rather than as a repaired value, so a broken byte
sequence cannot be mistaken for a different real address.
`msgvault repair-encoding` does not repair recorded envelope addresses. You need
to recover them separately from verified original values before
`msgvault build-cache --full-rebuild` can include them. See the
[encoding repair limits](../cli-reference.md#repair-encoding).

### Convenience views

These pre-joined and aggregated views cover common queries.

| View | Description |
|---|---|
| `v_messages` | Messages with resolved sender (from_email, from_name, from_domain, from_phone) and labels as a JSON array |
| `v_senders` | Per-sender aggregates: from_email, from_name, from_domain, message_count, total_size, attachment_size, attachment_count, first_message_at, last_message_at |
| `v_domains` | Per-domain aggregates: domain, message_count, total_size, sender_count |
| `v_labels` | Per-label: name, message_count, total_size |
| `v_threads` | Per-conversation: conversation_id, source_conversation_id, conversation_title, conversation_type, message_count, first_message_at, last_message_at, participant_emails (JSON array) |

## Example queries

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
text-message imports in the same `messages` table. Filter on the `message_type`
column to scope your SQL reports:

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

Known values are `email`, `google_chat`, `calendar_event`, `meeting_transcript`,
`sms`, `mms`, `rcs`, `whatsapp`, `imessage`, `teams`, `discord`, `fbmessenger`,
`facebook_comments`, `instagram_comments`, `instagram_mentions`,
`facebook_messages`, `instagram_messages`, `synctech_sms_call`,
`google_voice_text`, `google_voice_call`, `google_voice_voicemail`, `beeper`,
`chatwoot`, `slack`, `inline`, `matrix`, `telegram`, `x_dm`, `tiktok_export`,
`linkedin`, and `feed_item`.

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

The `labels` column in `v_messages` is a JSON array string. Use DuckDB's
`list_contains` function to filter it:

```bash
msgvault query "
  SELECT subject, from_email, sent_at
  FROM v_messages
  WHERE list_contains(labels::VARCHAR[], 'INBOX')
  ORDER BY sent_at DESC
  LIMIT 20
"
```

If you want more control, join through the base tables instead:

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

**Partition pruning.** The `messages` view is hive-partitioned by year. Adding
`WHERE year = 2024` to queries on `messages` lets DuckDB skip unrelated Parquet
files, which speeds up queries on large archives.

**Cache freshness.** A usable stale publication continues to serve queries
during `min_rebuild_interval`. After that interval expires, a query can schedule
a background freshness check and refresh. Set `auto_build_cache = false` to turn
off automatic builds; `--fresh` still requests one explicitly. A query waits for
the build when `--fresh` is set or when no usable cache exists. Deleted messages
can remain in the published cache until it refreshes; see the
[cache freshness policy](../configuration.md#analytics).

**Pipe-friendly output.** JSON and CSV output modes work well when piped into
other tools such as `jq`, `csvkit`, and `xsv`. Use `--format csv` for
spreadsheet workflows or `--format json` for scripts and programs.

**Full DuckDB SQL.** You can use DuckDB's full SQL dialect, including window
functions, CTEs, `UNNEST`, `list_contains`, and all built-in functions. See the
[DuckDB documentation](https://duckdb.org/docs/sql/introduction) for the full
SQL reference.

## See also

For pre-built analytics commands covering top senders, domains, labels, and
overall stats, see [Analytics & Stats](/docs/usage/analytics/). Use `query` when
you need more flexibility than those commands offer.
