---
last_edited: "2026-09-08"
title: Slack
description: Archive Slack workspaces through the Web API or a Slackdump export.
---

msgvault archives your own view of a Slack workspace: every public and
private channel you are a member of, group DMs, and 1:1 DMs, with threads,
reactions, @mentions, edits, and shared files. Each workspace becomes its own
msgvault source; all Slack-archived messages share `message_type = slack` for
search.

Choose a [local Slackdump import](#import-a-slackdump-export) for an existing
export, or [connect a workspace](#prerequisites) for continuing sync. Live sync
only reads Slack; it does not post or edit messages or mark them read.

## Import a Slackdump export

Use `import-slackdump` when you already have an export created by
[Slackdump](https://github.com/rusq/slackdump). The import runs entirely from
the local directory or ZIP and does not need a Slack token:

```bash
msgvault import-slackdump --me you@example.com /path/to/slackdump-export
msgvault import-slackdump --me U0123456789 /path/to/slackdump-export.zip
```

`--me` accepts your exact Slack user ID or a unique profile email from the
export. The importer preserves channels, private channels, group DMs, DMs,
threads, reactions, mentions, raw Slack JSON, and exported files. Standard
Slackdump attachment directories and Mattermost-style `__uploads` directories
are both supported.

Each imported account is stored as a `slackdump` source identified by
`<team-id>:<user-id>`. Messages still use `message_type = slack`, so live Slack
syncs and offline imports share the same search and analytics behavior while
remaining separately filterable sources.

| Flag | Description |
|---|---|
| `--me ID_OR_EMAIL` | Your Slack user ID or unique profile email in the export (required) |
| `--limit N` | Import at most N messages per conversation (0 = unlimited) |
| `--max-media-mb N` | Skip exported files larger than N MiB (0 = configured/default limit) |
| `--no-default-identity` | Do not auto-confirm the workspace user ID as the source's "me" identity |

Slackdump uses the same [media policy](/docs/configuration/#media-policy) as
live Slack sync, including per-workspace limits. It reads exported files from
disk and does not fetch missing files from Slack.

Re-running the same export updates existing messages and reuses stored file
content instead of creating duplicates. If a file is referenced but absent
from the export, msgvault keeps a metadata-only attachment record and reports
it as missing in the command summary. With a configured remote, run the import
on the daemon host with `--local`; msgvault does not upload the export from a
client machine.

## Prerequisites

Create an internal Slack app in each workspace and obtain a **user token**:

1. Open [api.slack.com/apps](https://api.slack.com/apps) → **Create New
   App** → **From scratch**, in your workspace.
2. Under **OAuth & Permissions → Scopes → User Token Scopes**, add:
   `channels:history`, `groups:history`, `im:history`, `mpim:history`,
   `channels:read`, `groups:read`, `im:read`, `mpim:read`,
   `users:read`, `users:read.email`, `files:read`, `reactions:read`,
   `team:read`, `search:read`.
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

The command validates the token (`auth.test`, plus a `search.messages` probe
— thread-reply archiving requires `search:read`, and a missing scope should
fail setup, not every future sync), stores it at
`tokens/slack_<team-id>_<user-id>.json` (0600), and registers the workspace
as a `slack` source identified by `<team-id>:<user-id>`. Tokens are keyed by
workspace *and* user, so two accounts in the same workspace coexist.

Provide the token via the interactive prompt, `--token-file <path>`, or the
`MSGVAULT_SLACK_TOKEN` environment variable:

```bash
MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack
msgvault add-slack --token-file ~/slack-token.txt
```

Repeat for additional workspaces — tokens are per-workspace and sources stay
separately filterable in the TUI.

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
| `--limit N` | Bound work per conversation, including thread replies; progress resumes next run |
| `--full` | Re-fetch all messages and update the existing archive rows |
| `--no-threads` | Skip thread-reply fetching this run (a later threaded run pays the debt automatically) |
| `--maintenance` | Refresh recent messages and replies for edits and reaction changes |
| `--no-media` | Skip file downloads this run (files stay pending for `backfill-slack-media`) |

Backfills are resumable: interrupt with Ctrl-C and the next run continues
from the last checkpoint. A `--full` repair also resumes on later runs,
including normal incremental runs, until it finishes.

Incremental runs fetch new messages and look for thread replies created since
the last run. Edits and reaction changes need `--maintenance` for recent
messages or `--full` for older ones.

### Limited runs

`--limit` counts thread replies in each conversation's work budget using their
reported `reply_count`. The workspace-wide search for new replies gets the
same budget. Large threads, missed-history recovery, and reply searches keep
checkpoints, so repeated limited runs continue making progress. The maintenance
rescan is skipped while a limit is set.

### How thread replies are found

Slack's history API never returns thread replies in the main channel stream
(unless "also sent to channel"), and offers no change feed. The importer
discovers replies with a search sweep (`threads:replies`, day-granular,
resumable via a UTC watermark): a reply is found by its **creation time**,
so the age of its thread is irrelevant. Because Slack publishes no maximum
search-index delay, the importer also re-walks one oldest-due conversation's
canonical history and threads per run. Continued scheduled runs rotate across
the workspace, eventually covering even a reply that search never indexes.
Discovered replies and audits are archived canonically via
`conversations.replies`.
A channel that was excluded (or unreadable) while sweeps advanced recovers
automatically when it returns: the importer runs a channel-scoped catch-up
sweep over the days it missed before rejoining the workspace-wide sweep.
One documented edge: a single day whose reply count exceeds search's
~10,000 reachable results per query cannot be fully swept — the run fails
loudly (never silently skipping), records the unreachable remainder as
unfinished thread work, and later runs recover it automatically without
search.
Deleted messages never erase their archived content locally. They are marked
deleted-at-source so active-message queries match Teams and Discord semantics.
This holds on every re-read path, `--full` and `--maintenance` included: a
deleted thread root that Slack still serves as a tombstone row never overwrites
the archived body, raw JSON, attachments, or reactions.

### Files

Files are downloaded into content-addressed attachment storage, capped at
`max_media_mb` per file. By default files shared in conversations with more
than 20 members are skipped with a typed `participant_threshold` marker; DMs,
group DMs, and small channels keep theirs. Set `media_max_participants = 0`
under `[slack]` to collect from every channel, or `media_scope = "direct"` to
collect only from DMs and group DMs (see
[Media policy](/docs/configuration/#media-policy)). Files hosted outside
`files.slack.com` (external links, connected drives) are recorded as metadata +
permalink only. Failed downloads leave pending markers:

```bash
msgvault backfill-slack-media
```

retries them (idempotent; already-downloaded files are never re-fetched).
The command always downloads, even while `[slack].media = false` keeps the
scheduled syncs deferring files — that setting's documented workflow (defer
now, backfill later) depends on it.
If Slack removes a file before it is downloaded, msgvault keeps the last
captured filename, size, and permalink as terminal metadata rather than
deleting the row or retrying an unreachable file forever.

## Daemon scheduling

```toml
[slack]
enabled = true
schedule = "*/30 * * * *"
media_max_participants = 20   # default; 0 = collect files from every channel
```

The daemon then syncs every registered workspace on the schedule. See
[Configuration](/docs/configuration/#slack) for the full option list
(channel include/exclude filters, media scope, participant and size caps,
per-workspace `accounts_config` overrides).

## Identity unification

Workspace members' profile emails (via `users:read.email`) link their Slack
messages to the same participant as their archived mail, so searching a
person spans both. Bots, deactivated members, and Slack Connect guests
without a visible email resolve as Slack-only identities.
