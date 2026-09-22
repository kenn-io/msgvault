---
last_edited: "2026-09-22"
title: Verify Integrity
description: Check a Gmail archive's database, raw-message coverage, and sampled MIME data.
---

Check whether a Gmail archive is structurally readable and whether sampled raw
messages can be decompressed. The command also reports Gmail's current message
count beside the archive count. Those counts are a comparison, not proof that
the two systems contain the same messages.

`verify` is Gmail-specific. It contacts Gmail with the selected account's
authorization, even when the archive itself is local.

## Run a check

```bash
# Default: sample 100 messages
msgvault verify you@gmail.com

# Larger sample
msgvault verify you@gmail.com --sample 500
```

## What it checks

The command goes through the configured remote server or local daemon and
reports four checks:

| Check | Description |
|---|---|
| Database integrity | Runs SQLite `PRAGMA integrity_check` unless skipped. PostgreSQL archives must use `pg_amcheck` separately. |
| Message counts | Reports Gmail's profile total, the archive account total, and their signed difference. Gmail's total and msgvault's archive policy can cover different sets. |
| Raw MIME coverage | Counts archived messages that have stored raw MIME data and reports the percentage. |
| MIME sample | Selects up to `--sample` archived raw messages and checks that each stored MIME value can be decompressed. |

This command does not compare Gmail and archive message IDs, inspect
attachments for completeness, or confirm that every archived message has an entry in the full-text
search index. See [rebuilding the search index](../cli-reference.md#rebuild-fts)
for index recovery.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--sample` | `100` | Number of messages to sample for verification |
| `--skip-db-check` | `false` | Skip SQLite integrity check |
| `--json` | `false` | Emit machine-readable JSON summary |

## When to verify

- After an initial full sync to check database and raw-MIME health
- Before executing deletions from Gmail
- Periodically to check for database corruption
- After recovering from interrupted syncs
