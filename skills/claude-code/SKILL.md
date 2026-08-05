---
name: msgvault-query
description: "Query msgvault archive analytics via SQL views. Use when: querying email, chat, or meeting history, analyzing senders/domains/labels, thread analysis, attachment stats, messages per month, sender graphs, domain breakdowns, message analytics. Triggers on: msgvault, message archive, email archive, chat archive, message search, email search, message analytics, sender analysis, domain analysis."
triggers:
  - msgvault
  - message archive
  - email archive
  - chat archive
  - message search
  - email search
  - message analytics
  - sender analysis
  - domain analysis
---

# msgvault-query

Run SQL against the msgvault archive via `msgvault query`. The analytics cache is DuckDB over Parquet — queries run in milliseconds. No DuckDB binary or Parquet path knowledge required.

The analytics cache is built automatically when stale or missing.

## Quick Start

```bash
# Top 10 senders, table format
msgvault query --format table "SELECT from_email, message_count FROM v_senders ORDER BY message_count DESC LIMIT 10"

# Messages from a domain in 2024, CSV output
msgvault query --format csv "SELECT subject, sent_at, from_email FROM v_messages WHERE from_domain = 'example.com' AND year = 2024 ORDER BY sent_at DESC"

# Label distribution as JSON
msgvault query "SELECT name, message_count, total_size FROM v_labels ORDER BY message_count DESC"
```

## Available Views

See [references/views.md](references/views.md) for full column schemas.

### Base views (direct Parquet data)

| View | Description |
|------|-------------|
| `messages` | Raw message metadata partitioned by year |
| `participants` | Email addresses with domain and display name |
| `message_recipients` | Message-to-participant links (from/to/cc/bcc) |
| `labels` | Gmail label names |
| `message_labels` | Message-to-label links |
| `attachments` | Attachment metadata (filename, size) per message |
| `conversations` | Thread grouping |
| `sources` | Synced accounts |

### Convenience views (pre-joined aggregates)

| View | Description |
|------|-------------|
| `v_messages` | Messages with resolved sender (from_email, from_name, from_domain) and labels as JSON array |
| `v_senders` | Per-sender aggregates: message_count, total_size, attachment stats, first/last message |
| `v_domains` | Per-domain aggregates: message_count, total_size, sender_count |
| `v_labels` | Per-label aggregates: message_count, total_size |
| `v_threads` | Per-conversation aggregates: message_count, date range, participant_emails as JSON array |

## Output Formats

```bash
msgvault query "..."                    # JSON (default): {"columns":[...], "rows":[...], "row_count":N}
msgvault query --format csv "..."       # CSV with header row
msgvault query --format table "..."     # Aligned text table with row count
```

## Common Queries

### Top senders

```sql
SELECT from_email, from_name, message_count, total_size
FROM v_senders
ORDER BY message_count DESC
LIMIT 20
```

### Domain breakdown

```sql
SELECT domain, message_count, sender_count, total_size
FROM v_domains
ORDER BY message_count DESC
LIMIT 20
```

### Messages from a domain in a date range

```sql
SELECT subject, sent_at, from_email, snippet
FROM v_messages
WHERE from_domain = 'example.com'
  AND year BETWEEN 2022 AND 2024
ORDER BY sent_at DESC
LIMIT 50
```

### Label distribution

```sql
SELECT name, message_count, total_size
FROM v_labels
ORDER BY message_count DESC
```

### Thread analysis

```sql
SELECT conversation_title, message_count, first_message_at, last_message_at, participant_emails
FROM v_threads
ORDER BY message_count DESC
LIMIT 20
```

### Large attachments

```sql
SELECT m.subject, m.from_email, m.sent_at, a.filename, a.size
FROM v_messages m
JOIN attachments a ON a.message_id = m.id
ORDER BY a.size DESC
LIMIT 20
```

### Messages per month

```sql
SELECT year, month, COUNT(*) AS message_count
FROM messages
GROUP BY year, month
ORDER BY year, month
```

## CLI Commands (non-SQL)

For tasks that don't need SQL aggregation, use these directly:

| Task | Command |
|------|---------|
| Archive stats | `msgvault stats` |
| Full-text search | `msgvault search "<query>" --json` |
| Incremental sync | `msgvault sync-incremental <email>` |
| Full sync | `msgvault sync-full <email>` |
| Build analytics cache | `msgvault build-cache` |
| Interactive TUI | `msgvault tui` |

## Tips

- **Partition pruning:** Always add `WHERE year = YYYY` or `year BETWEEN X AND Y` when filtering by date — this skips entire Parquet partitions and cuts query time significantly.
- **Labels are JSON:** In `v_messages`, `labels` is a JSON array string. Use DuckDB's `json_array_contains(labels, 'INBOX')` to filter by label, or join through `message_labels` + `labels` for exact matching.
- **Sender resolution is dual-path:** `v_messages` resolves the sender via `message_recipients` (email messages) with a fallback to `messages.sender_id` (chat messages). Use `v_messages` instead of `messages` whenever you need `from_email` or `from_name`.
- **All queries are read-only:** `msgvault query` never modifies the archive.
- **Default format is JSON:** Pipe through `jq` for further filtering. Use `--format table` for human-readable output.
