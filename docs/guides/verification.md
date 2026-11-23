---
last_edited: "2026-09-22"
title: Verify Integrity
description: Check a Gmail archive's database, raw-message coverage, and sampled MIME data.
---

Use `msgvault verify` to check whether a Gmail archive is structurally readable
and whether sampled messages have stored raw MIME that parses. The command also
reports Gmail's current message count beside your archive count. Treat those
counts as a comparison rather than proof that both systems contain the same
messages.

`verify` is Gmail-specific and contacts Gmail using the selected account's
authorization, even when your archive is local.

## Run a check

```bash
# Default: sample 100 messages
msgvault verify you@gmail.com

# Larger sample
msgvault verify you@gmail.com --sample 500
```

## What it checks

The command runs through your configured remote server or local daemon and
reports four checks:

| Check | Description |
|---|---|
| Database integrity | Runs SQLite `PRAGMA integrity_check` unless skipped. PostgreSQL archives must use `pg_amcheck` separately. |
| Message counts | Reports Gmail's profile total, the archive account total, and their signed difference. Gmail's total and msgvault's archive policy can cover different sets. |
| Raw MIME coverage | Counts archived messages that have stored raw MIME data and reports the percentage. |
| MIME sample | Selects up to `--sample` archived messages at random and checks that each one has stored raw MIME that is nonempty and parses as MIME. |

This command does not compare Gmail and archive message IDs, inspect
attachments for completeness, or confirm that every archived message has an
entry in the full-text search index. To recover the search index, see
[rebuilding the search index](../cli-reference.md#rebuild-fts).

## Flags

| Flag | Default | Description |
|---|---|---|
| `--sample` | `100` | Number of messages to sample for verification |
| `--skip-db-check` | `false` | Skip SQLite integrity check |
| `--json` | `false` | Emit machine-readable JSON summary |

## When to verify

- After an initial full sync, to check database and raw-MIME health.
- Before executing deletions from Gmail.
- Periodically, to check for database corruption.
- After recovering from interrupted syncs.
