---
last_edited: "2026-09-08"
title: Exporting Data
description: Export bounded message windows, .eml files, and attachments.
---

You can export message content and attachment files from msgvault without
contacting their original providers. Pick the format that fits what you want to
do next:

| You need | Export path |
|---|---|
| Messages for a date range, person, or downstream tool | [JSON Lines message export](#export-a-bounded-message-window) |
| One original email for another mail client | [EML](#export-as-eml) |
| One file or all files from a message | [Attachment exports](#export-a-single-attachment) |
| Selected attachments from the terminal browser | [TUI attachment export](/docs/usage/tui/) |

You can still export messages deleted from their source as long as their local
archive copies exist. [Local garbage collection](/docs/usage/deletion/#permanently-purge-source-deleted-mail-from-the-local-archive)
can remove those local copies.

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

The `--start` bound is inclusive and `--end` is exclusive. Both flags are
required and take RFC3339 timestamps. Repeat `--message-type` to select exact message
types, or leave it out to include every type. Repeat `--source` with a stable
`type:identifier` selector to filter by source, or leave it out to include every
source. The first colon separates the source type from the identifier, and any
remaining colons belong to the identifier.

| Flag | Default | Description |
|---|---|---|
| `--start` | required | Inclusive RFC3339 lower bound |
| `--end` | required | Exclusive RFC3339 upper bound |
| `--message-type` | all | Exact message type to include; repeatable |
| `--source` | all | Exact `type:identifier` source; repeatable |
| `--person-id` | all | Limit to messages involving the saved person's bound participants |
| `--format` | `jsonl` | Output format; supports only `jsonl` |

To export correspondence for a saved person, pass their durable person ID:

```bash
msgvault export-messages --person-id 42 \
  --start 2026-07-01T00:00:00Z --end 2026-08-01T00:00:00Z > person-42.jsonl
```

This filter uses the person's bound participant identities and does not infer
new identity links during export. See [People](/docs/usage/people/) to create
and review those bindings.

### Stream contract for consumers

Standard output contains one compact JSON object per line. The
`msgvault-message-export/1` stream always follows these phases:

```text
manifest -> source* -> conversation* -> message* -> complete
```

The manifest records the UTC window and normalized filters. Each source record
uses `source_type` and `identifier` as its stable key and includes a nullable
`last_successful_sync_at` timestamp for provenance. Each conversation record
uses the source key and conversation `id`, with a closed `conversation_type`
vocabulary of `email_thread`, `channel`, `thread`, `direct_chat`, `group_chat`,
`meeting`, `calendar`, and `other`. Each message record uses the source key and
message `id`, and includes full normalized text, a nullable normalized author,
`occurred_at`, and `deleted_from_source`.

Downstream consumers should require one manifest first, the phase order above,
one completion record last, and completion counts that match the observed record
counts. If the command fails after streaming starts, it deliberately omits the
completion record, which marks a partial export as invalid. Explicitly selected
sources still produce source records when the time window is empty, unless you
narrow the export with `--person-id`, in which case only sources with in-scope
messages appear. If you pass a selector that does not exist, the command fails
before writing anything to stdout.

For an unchanged archive and command version, the output order is deterministic.
The command reads from your configured local or remote daemon archive, does not
load provider credentials, and never starts a sync. Exported message content is
as sensitive as the archive itself. Redirect it only to storage with appropriate
protection.

## Export as EML

To export a single message as a standard `.eml` file, run `export-eml`:

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

The exported `.eml` file contains the original raw MIME bytes preserved during
sync. To write a conversation as numbered `.eml` files, run
`msgvault export-eml <id> --thread --output DIR`. See the
[command reference](/docs/cli-reference/#export-eml) for reference resolution,
account selection, and skipped-message behavior.

## Export a single attachment

You can export an attachment by its SHA-256 content hash. To find the hash, run
`show-message --json`:

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

You cannot combine `--json` and `--base64`, and you cannot combine either flag
with an `--output` file path. Passing `-o -` to write to stdout is allowed with
either flag.

## Export all attachments from a message

To export every attachment from a message as individual files with their
original filenames, run `export-attachments`:

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

msgvault sanitizes and deduplicates filenames automatically. It never overwrites
existing files, and appends a numeric suffix when a filename conflicts.
