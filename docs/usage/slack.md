---
last_edited: "2026-10-08"
title: Slack
description: Archive Slack workspaces through the Web API or a Slackdump export.
---

Archive all public channels in a Slack workspace with a restricted user token,
or archive your channel memberships and DMs with broader permissions. msgvault
captures threads, reactions, @mentions, edits, and shared-file metadata.
Downloading files needs an additional permission. Each workspace becomes its
own msgvault source, while all Slack-archived messages share
`message_type = slack` for search.

Use a [local Slackdump import](#import-a-slackdump-export) if you already have
an export, or [connect a workspace](#prerequisites) for ongoing sync. Live sync
only reads Slack; it never posts or edits messages or marks them read.

## Import a Slackdump export

To import an existing export created by
[Slackdump](https://github.com/rusq/slackdump), run `import-slackdump`. It
reads directly from the local directory or ZIP and does not need a Slack token:

```bash
msgvault import-slackdump --me you@example.com /path/to/slackdump-export
msgvault import-slackdump --me U0123456789 /path/to/slackdump-export.zip
```

Pass your exact Slack user ID or a unique profile email from the export to
`--me`. The importer preserves channels, private channels, group DMs, DMs,
threads, reactions, mentions, raw Slack JSON, and exported files, supporting
both standard Slackdump attachment directories and Mattermost-style `__uploads`
directories.

Each imported account is stored as a `slackdump` source identified by
`<team-id>:<user-id>`. Messages still use `message_type = slack`, so live Slack
syncs and offline imports share search and analytics behavior while staying
separately filterable sources.

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
content instead of creating duplicates. If a file is referenced but missing
from the export, msgvault keeps a metadata-only attachment record and reports
it as missing in the command summary. If you have a remote configured, run
the import on the daemon host with `--local`; msgvault does not upload the
export from a client machine.

## Prerequisites

Create an internal Slack app in each workspace to get a **user token**:

1. Open [api.slack.com/apps](https://api.slack.com/apps), select **Create New
   App**, and choose **From scratch** in your workspace.
2. Under **OAuth & Permissions**, open **Scopes**, then choose the permissions
   below under **User Token Scopes**.
3. Click **Install to Workspace** and copy the **User OAuth Token**
   (`xoxp-…`).

Because the app is yours and not distributed, it is **not** subject to Slack's
non-Marketplace rate limits, and history backfills run at the full page size of
999 messages per request rather than the throttled 15.

Some workspaces restrict app creation to admins; if yours does, ask an admin to
approve the app.

### Public channels only

Grant only these four user scopes:

```text
channels:read channels:history users:read users:read.email
```

This token can read public-channel messages, threads, and member profiles.
msgvault lists all public channels, including ones you have not joined, and
applies any configured channel-name filters. Each sync revisits their history
to find replies on old messages. That takes more API requests than search;
`--limit` bounds each run and resumes the history walk on later runs.

Do not add `search:read`, `files:read`, or `reactions:read` to this token,
because those permissions can expose content from private conversations
accessible to the authorizing user. Reactions already included in public
message payloads are still archived, and files stay metadata-only pending
entries without downloads. Conversation settings control what msgvault
archives; they do not reduce the token's permissions.

For a public-only archive, also set these options in `config.toml`:

```toml
[slack]
private_channels = false
dms = false
group_dms = false
```

All three default to `true`, and they select private channels, one-to-one DMs,
and group DMs independently within the token's permissions. Disabling a
conversation type keeps the messages already archived and keeps that type's
sync progress. These settings apply to every registered workspace.

If you are replacing a broader token, create a fresh Slack app with only these
scopes. Because
[Slack's OAuth grants are additive](https://docs.slack.dev/authentication/installing-with-oauth/),
reinstalling an existing app can retain previously granted access.

### Your channel memberships and DMs

Start with the four scopes above, then add the scope pairs for each
conversation type you want to archive:

| Conversations | Additional user scopes |
|---|---|
| Private channels | `groups:read`, `groups:history` |
| One-to-one DMs | `im:read`, `im:history` |
| Group DMs | `mpim:read`, `mpim:history` |

A token with any of these additional read scopes archives your memberships
rather than every public channel. You can also add `files:read` to download
files and `search:read` to discover replies faster. The importer does not need
`reactions:read` or `team:read`.

## Add a workspace

```bash
msgvault add-slack
```

This command validates your token with `auth.test`, saves it at
`tokens/slack_<team-id>_<user-id>.json` with mode 0600, and registers the
workspace as a `slack` source identified by `<team-id>:<user-id>`. Tokens are
keyed by workspace *and* user, so two accounts in the same workspace coexist.

You can pass the token at the interactive prompt, with `--token-file <path>`,
or through the `MSGVAULT_SLACK_TOKEN` environment variable:

```bash
MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack
msgvault add-slack --token-file ~/slack-token.txt
```

Repeat these steps for each additional workspace. Tokens are per-workspace, and
each source stays separately filterable in the TUI.

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

Backfills are resumable: if you interrupt a run with Ctrl-C, the next run
continues from the last checkpoint. A `--full` repair also resumes on later
runs, including normal incremental runs, until it finishes.

Incremental runs fetch new messages and look for thread replies created since
the last run. To pick up edits and reaction changes, run with `--maintenance`
for recent messages or `--full` for older ones.

### Limited runs

`--limit` counts thread replies in each conversation's work budget using their
reported `reply_count`, and the workspace-wide search for new replies gets the
same budget. Large threads, missed-history recovery, and reply searches keep
checkpoints so repeated limited runs keep making progress. The maintenance
rescan is skipped while a limit is set.

### How thread replies are found

The importer reads the token's granted scopes from Slack's `X-OAuth-Scopes`
response header. Without `search:read`, it revisits each selected
conversation's history and fetches its threads directly. That history walk
discovers a message that gets its first reply long after the initial sync.
Interrupted and limited walks keep their cursors, and new channel messages get an incremental pass between completed walks. An
interrupted sync also remembers which conversations it already visited so
retries finish the current pass before starting another, and the next scheduled
pass revisits those conversations to catch activity that arrived after their
previous visit. A pass with fetch failures ends as failed; the next pass
retries those channels while still refreshing healthy ones.

Without `files:read`, sync defers file downloads.

With `search:read`, the importer uses the search sweep below along with
periodic history audits to cover replies missing from search.

Slack's history API never returns thread replies in the main channel stream
unless they are also sent to the channel, and it offers no change feed. The
importer finds replies with a day-granular `threads:replies` search sweep that
resumes using a UTC watermark: because each reply is found by its **creation
time**, the age of its thread does not matter. Because Slack publishes no
maximum search-index delay, the importer also re-walks one oldest-due
conversation's canonical history and threads per run. Continued scheduled runs
rotate across the workspace and eventually cover even a reply that search never
indexes. Discovered replies and audits are archived canonically using
`conversations.replies`.

If a channel was excluded or unreadable while sweeps advanced, it recovers
automatically when it returns: the importer runs a channel-scoped catch-up
sweep over the days it missed before rejoining the workspace-wide sweep. A DM
or group DM turned back on resumes from its saved position, and a catch-up walk
recovers thread replies posted while it was off. If a `--full` repair cleared
that position while it was off, sync re-downloads its history without
duplicating archived messages.

One documented edge case: a single day whose reply count exceeds search's
~10,000 reachable results per query cannot be swept completely. When that
happens, the run fails loudly instead of skipping silently, records the
unreachable remainder as unfinished thread work, and lets later runs recover it
automatically without search.

Deleted messages never erase their archived content locally. msgvault marks
them deleted-at-source so active-message queries match Teams and Discord
semantics. This holds on every re-read path, including `--full` and
`--maintenance`: a deleted thread root that Slack still returns as a tombstone
row never overwrites the archived body, raw JSON, attachments, or reactions.

### Files

Files are downloaded into content-addressed attachment storage, capped at
`max_media_mb` per file. By default, files shared in conversations with more
than 20 members are skipped with a typed `participant_threshold` marker, while
DMs, group DMs, and small channels keep theirs. Under `[slack]`, set
`media_max_participants = 0` to collect files from every channel, or set
`media_scope = "direct"` to collect only from DMs and group DMs; see
[Media policy](/docs/configuration/#media-policy). Files hosted outside
`files.slack.com`, such as external links and connected drives, are recorded as
metadata and a permalink only. Failed downloads leave pending markers. To retry
them, run:

```bash
msgvault backfill-slack-media
```

This command is idempotent and never re-fetches files that are already
downloaded. It always downloads, even while `[slack].media = false` keeps
scheduled syncs deferring files, because that setting's documented workflow is
to defer now and backfill later. Conversation selection settings, namely
`channels`, `exclude_channels`, `private_channels`, `dms`, and `group_dms`, do
not apply to this command, so it can download pending files from excluded
conversations already in the archive. Media scope, participant and size limits,
and per-account opt-outs still apply. If Slack removes a file before it is
downloaded, msgvault keeps the last captured filename, size, and permalink as
terminal metadata rather than deleting the row or retrying an unreachable file
forever.

## Daemon scheduling

```toml
[slack]
enabled = true
schedule = "*/30 * * * *"
media_max_participants = 20   # default; 0 = collect files from every channel
```

The daemon syncs every registered workspace on this schedule. When another job
is waiting, a Slack sync can save its progress and yield, and the scheduler
queues its continuation automatically. Those handoffs appear as cancelled
attempts in sync history and as queued work on **Sources**; fetch failures
still appear as failures. The last successful sync advances when the pass
finishes.

For the full list of options, including channel include and exclude filters,
private-channel and DM selection, media scope, participant and size caps, and
per-workspace `accounts_config` overrides, see
[Configuration](/docs/configuration/#slack).

## Identity unification

Workspace members' profile emails from `users:read.email` link their Slack
messages to the same participant as their archived mail, so searching a person
spans both. Bots, deactivated members, and Slack Connect guests without a
visible email resolve as Slack-only identities.
