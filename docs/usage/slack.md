---
title: Slack
description: Archive your Slack workspaces — channels, group DMs, and DMs — via the Web API.
---

msgvault archives your own view of a Slack workspace: every public and
private channel you are a member of, group DMs, and 1:1 DMs, with threads,
reactions, @mentions, edits, and shared files. Each workspace becomes its own
msgvault source; all Slack-archived messages share `message_type = slack` for
search.

Slack sync is strictly read-only: msgvault only calls read methods of the Web
API and never posts, edits, or marks anything in Slack.

## Prerequisites

A **user token** from an internal Slack app you create yourself (a two-minute,
one-time setup per workspace):

1. Open [api.slack.com/apps](https://api.slack.com/apps) → **Create New
   App** → **From scratch**, in your workspace.
2. Under **OAuth & Permissions → Scopes → User Token Scopes**, add:
   `channels:history`, `groups:history`, `im:history`, `mpim:history`,
   `channels:read`, `groups:read`, `im:read`, `mpim:read`,
   `users:read`, `users:read.email`, `files:read`, `reactions:read`,
   `team:read`.
3. Click **Install to Workspace** and copy the **User OAuth Token**
   (`xoxp-…`).

Because the app is yours and not distributed, it is **not** subject to
Slack's non-Marketplace rate limits — history backfills run at full page size
(999 messages per request) rather than the throttled 15.

Some workspaces restrict app creation to admins; if that applies to yours,
ask an admin to approve the app.

## Add a workspace

```bash
msgvault add-slack
```

The command validates the token (`auth.test`), stores it at
`tokens/slack_<team-id>.json` (0600), and registers the workspace as a
`slack` source identified by `<team-id>:<user-id>`.

Provide the token via the interactive prompt, `--token-file <path>`, or the
`MSGVAULT_SLACK_TOKEN` environment variable:

```bash
MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack
msgvault add-slack --token-file ~/slack-token.txt
```

Repeat for additional workspaces — tokens are per-workspace and sources stay
separately filterable in the TUI (`a`).

## Sync

```bash
# First run backfills all history; later runs are incremental.
msgvault sync-slack

# One workspace only.
msgvault sync-slack T0123456789

# Repair path: re-fetch everything, upserting in place.
msgvault sync-slack --full
```

| Flag | Description |
|---|---|
| `--limit N` | Max messages per conversation this run (backfills resume next run) |
| `--full` | Ignore stored cursors; re-fetch and upsert every message in place |
| `--no-threads` | Skip thread-reply fetching this run |
| `--no-media` | Skip file downloads this run |

Backfills are resumable: interrupt with Ctrl-C and the next run continues
from the last checkpoint. Incremental runs fetch new messages, re-scan the
trailing week for edits and reaction changes, and poll recent thread roots
for new replies.

### Threads, edits, and deletions

Slack's history API never returns thread replies in the main channel stream
(unless the author chose "also send to channel"), so msgvault tracks each
thread root and polls it for new replies. Tracking lasts
`thread_lookback_days` (default 30); a reply posted to an older thread is
only picked up by a `--full` run.

Edits and reaction changes are caught within the trailing re-scan window
(one week); older ones likewise need `--full`. Deleted messages simply
disappear from Slack — your archived copy is kept (archive semantics; nothing
is ever deleted locally).

### Files

Files are downloaded into content-addressed attachment storage, capped at
`max_media_mb` per file. Files hosted outside `files.slack.com` (external
links, connected drives) are recorded as metadata + permalink only. Failed
downloads leave pending markers:

```bash
msgvault backfill-slack-media
```

retries them (idempotent; already-downloaded files are never re-fetched).

## Daemon scheduling

```toml
[slack]
enabled = true
schedule = "*/30 * * * *"
```

The daemon then syncs every registered workspace on the schedule. See
[Configuration](/configuration/#slack) for the full option list
(channel include/exclude filters, media caps, thread lookback).

## Identity unification

Workspace members' profile emails (via `users:read.email`) link their Slack
messages to the same participant as their archived mail, so searching a
person spans both. Bots, deactivated members, and Slack Connect guests
without a visible email resolve as Slack-only identities.
