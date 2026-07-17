---
title: Verify Integrity
description: Verify your archive against Gmail.
---

## Usage

```bash
# Default: sample 100 messages
msgvault verify you@gmail.com

# Larger sample
msgvault verify you@gmail.com --sample 500
```

## What It Checks

The verify command compares your archive against Gmail through the configured
remote server or local daemon:

| Check | Description |
|---|---|
| Message count | Compares local count vs Gmail message count |
| Raw MIME presence | Verifies sampled messages have raw MIME data stored |
| FTS index entries | Confirms sampled messages are indexed for full-text search |

## Flags

| Flag | Default | Description |
|---|---|---|
| `--sample` | `100` | Number of messages to sample for verification |
| `--skip-db-check` | `false` | Skip SQLite integrity check |
| `--json` | `false` | Emit machine-readable JSON summary |

## When to Verify

- After initial full sync to confirm completeness
- Before executing deletions from Gmail
- Periodically to check for database corruption
- After recovering from interrupted syncs
