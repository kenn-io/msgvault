---
last_edited: "2026-08-30"
title: Exporting Data
description: Export bounded message windows, .eml files, and attachments.
---

## Export a bounded message window

`export-messages` streams normalized archive data as JSON Lines without
contacting any provider:

```bash
msgvault export-messages \
  --start 2026-07-20T00:00:00Z \
  --end 2026-07-27T00:00:00Z \
  --message-type discord \
  --source discord:123456789012345678 \
  > messages.jsonl
```

`--start` is inclusive and `--end` is exclusive. Both bounds are required
RFC3339 timestamps. Repeat `--message-type` to include exact message types, or
omit it to include every type. Repeat `--source` with a stable
`type:identifier` selector, or omit it to include every source. The first colon
separates the source type; any remaining colons belong to the identifier.

| Flag | Default | Description |
|---|---|---|
| `--start` | required | Inclusive RFC3339 lower bound |
| `--end` | required | Exclusive RFC3339 upper bound |
| `--message-type` | all | Exact message type to include; repeatable |
| `--source` | all | Exact `type:identifier` source; repeatable |
| `--format` | `jsonl` | Output format; v1 supports only `jsonl` |

Stdout contains one compact JSON object per line. The
`msgvault-message-export/1` stream always has these phases:

```text
manifest -> source* -> conversation* -> message* -> complete
```

The manifest records the UTC window and normalized filters. Source records use
`source_type` plus `identifier` as their stable key and include nullable
`last_successful_sync_at` provenance. Conversation records use the source key
plus conversation `id`; their closed `conversation_type` vocabulary is
`email_thread`, `channel`, `thread`, `direct_chat`, `group_chat`, `meeting`,
`calendar`, and `other`. Message records use the source key plus message `id`
and include full normalized text, a nullable normalized author, `occurred_at`,
and `deleted_from_source`.

Consumers should require one manifest first, phase order, one completion record
last, and completion counts equal to observed record counts. A command failure
after streaming begins deliberately leaves no completion record, making a
partial export invalid. Explicitly selected sources still produce source
records when the window is empty; a selector that does not exist fails before
stdout begins.

Output order is deterministic for an unchanged archive and command version.
The command reads the configured local or remote daemon archive, does not load
provider credentials, and never starts a sync. Exported message content is as
sensitive as the archive itself; redirect it only to appropriately protected
storage.

---

## Export as EML

Export a single message as a standard `.eml` file:

```bash
# By internal message ID
msgvault export-eml 12345 --output message.eml

# By source message ID
msgvault export-eml 18abc123def --output message.eml

# Output to stdout
msgvault export-eml 18abc123def --output -
```

| Argument or flag | Description |
|---|---|
| `<id>` | Internal message ID or source message ID |
| `-o`, `--output` | Output file path (default: `<source-message-id>.eml`; use `-` for stdout) |

The exported `.eml` contains the original raw MIME bytes preserved during sync.

---

## Export a single attachment

Export an attachment by its SHA-256 content hash. Get the hash from `show-message --json`:

```bash
# Find the content hash
msgvault show-message 45 --json | jq '.attachments[0].content_hash'

# Export to a file
msgvault export-attachment <content-hash> -o invoice.pdf

# Output to stdout (binary)
msgvault export-attachment <content-hash> -o -

# Output as base64
msgvault export-attachment <content-hash> --base64

# JSON output with base64-encoded data
msgvault export-attachment <content-hash> --json
```

| Flag | Description |
|---|---|
| `-o`, `--output` | Output file path (use `-` for stdout) |
| `--base64` | Output raw base64 to stdout |
| `--json` | Output as JSON with base64-encoded data |

The `--json`, `--base64`, and `--output` flags are mutually exclusive.

---

## Export all attachments from a message

Export every attachment from a message as individual files with their original filenames:

```bash
# Export all attachments to the current directory
msgvault export-attachments 45

# Export to a specific directory
msgvault export-attachments 45 -o ~/Downloads

# By Gmail message ID
msgvault export-attachments 18f0abc123def
```

| Flag | Description |
|---|---|
| `-o`, `--output` | Output directory (default: current directory) |

Filenames are sanitized and deduplicated automatically. Existing files are never overwritten — a numeric suffix is appended on conflict.
