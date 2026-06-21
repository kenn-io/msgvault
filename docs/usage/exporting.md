---
title: Exporting Data
description: Export messages as .eml files and attachments by content hash or message ID.
---

## Export as EML

Export a single message as a standard `.eml` file:

```bash
# By internal message ID
msgvault export-eml --message-id 12345 --output message.eml

# By Gmail message ID
msgvault export-eml --gmail-id 18abc123def --output message.eml

# Output to stdout
msgvault export-eml --gmail-id 18abc123def
```

| Flag | Description |
|---|---|
| `--message-id` | Internal database message ID |
| `--gmail-id` | Gmail message ID (hex string) |
| `--output` | Output file path (default: stdout) |

The exported `.eml` file contains the original raw MIME data, decompressed from the zlib-compressed storage in the database.

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
