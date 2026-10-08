---
last_edited: "2026-10-08"
title: Slack
description: Archive Slack workspaces through the Web API or a Slackdump export.
---

msgvault can archive all public channels with a restricted user token, or
your channel memberships and DMs with broader permissions. It captures threads,
reactions, @mentions, edits, and shared-file metadata. Downloading files requires
an additional permission. Each workspace becomes its own msgvault source; all
Slack-archived messages share `message_type = slack` for search.

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
2. Under **OAuth & Permissions → Scopes → User Token Scopes**, choose the
   permissions below.
3. Click **Install to Workspace** and copy the **User OAuth Token**
   (`xoxp-…`).

Because the app is yours and not distributed, it is **not** subject to
Slack's non-Marketplace rate limits — history backfills run at full page size
(999 messages per request) rather than the throttled 15.

Some workspaces restrict app creation to admins; if that applies to yours,
ask an admin to approve the app.

### Public channels only

Grant only these four user scopes:

```text
channels:read channels:history users:read users:read.email
```

This token can read public-channel messages and threads plus member profiles.
msgvault lists all public channels, including ones you have not joined, and
applies any configured channel-name filters. Each sync revisits their history
to discover replies on old messages. This takes more API requests than search;
`--limit` bounds each run and resumes the history walk on subsequent runs.

Do not add `search:read`, `files:read`, or `reactions:read` to this token:
those permissions can expose content from private conversations accessible to
the authorizing user. Reactions already included in public message payloads
remain archived. Files remain metadata-only pending entries without downloads.
Conversation settings control what msgvault archives; they do not reduce the
token's permissions.

For a public-only archive, also set these options in `config.toml`:

```toml
[slack]
private_channels = false
dms = false
group_dms = false
```

All three default to `true`. They select private channels, one-to-one DMs, and
group DMs independently, within the token's permissions. Disabling a type
preserves messages already archived and its sync progress. The settings apply
to every registered workspace.

When replacing a broader token, create a fresh Slack app with just these
scopes. [Slack's OAuth grants are additive](https://docs.slack.dev/authentication/installing-with-oauth/),
so reinstalling an existing app can retain previously granted access.

### Your channel memberships and DMs

Start with the four scopes above, then add the pairs for each conversation type
you want to archive:

| Conversations | Additional user scopes |
|---|---|
| Private channels | `groups:read`, `groups:history` |
| One-to-one DMs | `im:read`, `im:history` |
| Group DMs | `mpim:read`, `mpim:history` |

Tokens with any of these additional read scopes archive your memberships,
rather than every public channel. Optionally add `files:read` for file downloads
and `search:read` for faster discovery of replies. `reactions:read` and
`team:read` are not needed by the importer.

## Add a workspace

```bash
msgvault add-slack
```

The command validates the token with `auth.test`, stores it at
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

# Override conversation selection for a public-only run.
msgvault sync-slack --private-channels=false --dms=false --group-dms=false
```

| Flag | Description |
|---|---|
| `--limit N` | Bound work per conversation, including thread replies; progress resumes next run |
| `--dms BOOL` | Override one-to-one DM selection for this run |
| `--full` | Re-fetch all messages and update the existing archive rows |
| `--group-dms BOOL` | Override group DM selection for this run |
| `--private-channels BOOL` | Override private-channel selection for this run |
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

The importer reads the token's granted scopes from Slack's `X-OAuth-Scopes`
response header. Without `search:read`, it revisits each selected conversation's
history and fetches its threads directly. A message that gains its first reply
long after the initial sync is discovered by that history walk. Interrupted and
limited walks retain their cursors, and new channel messages get an incremental
pass between completed walks. An interrupted sync also remembers which
conversations it already visited, so retries finish the current pass before
starting another. The next scheduled pass revisits those conversations to
catch activity that arrived after their previous visit. A pass with fetch
failures ends as failed, and the next pass retries those channels while still
refreshing healthy ones.

Without `files:read`, sync defers file downloads.

With `search:read`, the importer uses the search sweep described below, with
periodic history audits to cover replies missing from search.

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
A DM or group DM turned back on resumes from its saved position, and a
catch-up walk recovers thread replies posted while it was off.
If a `--full` repair cleared that position while it was off, sync re-downloads
its history without duplicating archived messages.
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
Conversation selection settings (`channels`, `exclude_channels`,
`private_channels`, `dms`, and `group_dms`) do not apply to this command: it can download pending files from
excluded conversations already in the archive. Media scope, participant and
size limits, and per-account opt-outs still apply.
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

The daemon syncs every registered workspace on the schedule. When another job
is waiting, a Slack sync can save its progress and yield. The scheduler queues
its continuation automatically. These handoffs appear as cancelled attempts in
sync history and as queued work on Sources; fetch failures still appear as
failures. The last successful sync advances when the pass finishes.

See
[Configuration](/docs/configuration/#slack) for the full option list
(channel include/exclude filters, private-channel and DM selection, media scope,
participant and size caps,
per-workspace `accounts_config` overrides).

## Identity unification

Workspace members' profile emails (via `users:read.email`) link their Slack
messages to the same participant as their archived mail, so searching a
person spans both. Bots, deactivated members, and Slack Connect guests
without a visible email resolve as Slack-only identities.
