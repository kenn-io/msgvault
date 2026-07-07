---
title: Searching
description: Gmail-like search syntax with full-text search and JSON output.
---

## Basic Usage

```bash
msgvault search <query>
```

!!! note
    The full-text search index (FTS5) is populated automatically during sync.
    If an older archive needs an index backfill, msgvault checks and rebuilds
    the index in the background. Search returns immediately from the index as
    it exists now and reports when results may be incomplete while the daemon
    is still checking or building.

## Search Operators

msgvault supports a local subset of Gmail-like search syntax. Gmail-only
operators that depend on fields msgvault does not index locally, such as
`list:` / `List-ID`, are not available in local search or MCP search. Use a
Gmail connector query when you need Gmail itself to evaluate those operators.

| Operator | Description | Example |
|---|---|---|
| `from:` | Sender address | `from:alice@example.com` |
| `to:` | Recipient address | `to:bob@example.com` |
| `cc:` | CC recipient | `cc:team@example.com` |
| `bcc:` | BCC recipient | `bcc:admin@example.com` |
| `subject:` | Subject text | `subject:meeting` |
| `label:` | Gmail label | `label:INBOX`, `label:SENT` |
| `has:attachment` | Has attachments | `has:attachment` |
| `before:` | Before date | `before:2024-06-01` |
| `after:` | After date | `after:2024-01-01` |
| `older_than:` | Relative date | `older_than:7d`, `2w`, `1m`, `1y` |
| `newer_than:` | Relative date | `newer_than:30d` |
| `larger:` | Minimum size | `larger:5M`, `100K` |
| `smaller:` | Maximum size | `smaller:1M` |
| `message_type:` | Stored message type | `message_type:teams`, `message_type=calendar_event` |

Bare words and `"quoted phrases"` perform full-text search across message subjects and bodies.

### Domain Search

The `from:`, `to:`, `cc:`, and `bcc:` operators recognize bare domain names with common TLDs. For example, `from:example.com` automatically matches all messages from the `example.com` domain. For uncommon TLDs, use the explicit `@` prefix: `from:@brand.pizza`.

## Examples

```bash
# Search by sender
msgvault search from:alice@example.com

# Search by domain (bare domain with common TLD)
msgvault search from:example.com

# Uncommon TLD requires explicit @ prefix
msgvault search "from:@brand.pizza"

# Subject search
msgvault search subject:meeting

# Date range
msgvault search "after:2024-01-01 before:2024-06-01"

# Messages with attachments
msgvault search has:attachment

# By label
msgvault search label:INBOX

# Combined filters
msgvault search "from:boss@company.com has:attachment after:2024-01-01"

# Full-text search
msgvault search "quarterly report"
```

## Account and Collection Filters

In multi-account archives, use `--account` to limit results to a specific account, or `--collection` to search every member account in a named collection:

```bash
msgvault search "quarterly report" --account work@company.com
msgvault search "quarterly report" --collection Work

# List all messages for an account or collection (no search query needed)
msgvault search --account work@company.com
msgvault search --collection Work
```

The two flags are mutually exclusive. Collection filters work in full-text, vector, and hybrid local search modes.

SQLite FTS ranking is weighted to better match PostgreSQL-backed search behavior, so subject/body weighting should feel more consistent across local tools. The rankers are still different; see [Search Ranking Across Backends](/architecture/search-ranking/).

## Filtering by Message Type

Archives can hold more than email — Google Calendar events, Microsoft Teams
messages, text messages, calls, voicemails, and Messenger imports all live in
the same database. Restrict a search to one or more kinds with the
`message_type:` operator or the repeatable/comma-separated `--message-type`
flag:

```bash
# Only Google Calendar events
msgvault search "standup" --message-type calendar_event

# Only Microsoft Teams messages
msgvault search "message_type:teams incident review"

# Only SMS/MMS text messages
msgvault search "dinner" --message-type sms --message-type mms
```

Valid values are `email`, `calendar_event`, `sms`, `mms`, `whatsapp`,
`imessage`, `teams`, `fbmessenger`, `synctech_sms_call`, `google_voice_text`,
`google_voice_call`, and `google_voice_voicemail`. `message_type:email`
also includes legacy rows whose type is empty because older msgvault versions
created them before the column existed.

The same message-type scoping is available in HTTP search via the
`message_type` query parameter. In MCP, include a `message_type:` operator in
the `search_messages` query; `find_similar_messages` accepts a `message_type`
parameter.

## JSON Output

Add `--json` for machine-readable output:

```bash
msgvault search from:alice@example.com --json
```

## Semantic / Hybrid Search

The same `msgvault search` command supports semantic search when the
selected local daemon or remote server has `[vector]` configured with
an embedding endpoint. Pass
`--mode vector` for pure semantic search, or `--mode hybrid` to fuse
BM25 and vector ranking. See [Vector Search](/usage/vector-search/)
for setup, initial embedding, and incremental update workflows.
