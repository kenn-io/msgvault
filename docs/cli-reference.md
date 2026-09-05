---
last_edited: "2026-08-31"
title: CLI Reference
description: Complete command reference for all msgvault commands.
---

## Global Flags

| Flag | Description |
|---|---|
| `--config` | Path to config file (default: `~/.msgvault/config.toml`) |
| `--home` | Home directory for all data (overrides `MSGVAULT_HOME`) |
| `-v`, `--verbose` | Verbose output (implies `--log-level=debug`) |
| `--local` | Use the local daemon instead of a configured remote for archive-access commands |
| `--log-file <path>` | Override log file path (default: `<data_dir>/logs/msgvault-YYYY-MM-DD.log`) |
| `--log-level <level>` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `--no-log-file` | Disable file logging for this run (stderr output stays on) |
| `--log-sql` | Log every SQL query at info level (verbose, for debugging) |
| `--log-sql-slow-ms <ms>` | Slow query threshold in ms (default: 100; 0 uses built-in default) |
| `--help` | Show help |

---

## HTTP-Backed CLI Behavior

Commands that access archive state keep their usual stdout/stderr output while using the same API path as remote access:

1. If `[remote].url` is configured and `--local` is not passed, the CLI talks to that remote server.
2. Otherwise, archive-access commands discover or start the local background daemon and talk to it over HTTP.
3. `--local` selects the local daemon even when `[remote].url` is configured; it is not a request to open SQLite in the CLI process.

This makes local and remote msgvault behavior the same from the CLI's point of view and avoids opening a large SQLite database from foreground CLI processes.

The local daemon publishes its binary version and API schema version. By default, a newer compatible CLI restarts an older local daemon before issuing the request; configure `[server].daemon_auto_restart` as `newer`, `never`, or `always` to control that lifecycle behavior. Remote servers are not restarted by clients, so compatibility is negotiated from the API schema version exposed in the OpenAPI document.

---

## init-db

Initialize the archive schema through the configured remote server or the local
daemon. When no remote is configured, the CLI starts the local daemon if needed
and the daemon owns the database initialization work.

```bash
msgvault init-db
```

---

## add-account

Add a Gmail account and authorize via OAuth.

```bash
msgvault add-account <email>
msgvault add-account <email> --headless
msgvault add-account <email> --oauth-app <name>
```

| Flag | Description |
|---|---|
| `--headless` | Show instructions for headless server setup |
| `--oauth-app` | Use a named OAuth app from `[oauth.apps.<name>]` in config |
| `--force` | Delete existing token and re-authorize |
| `--readonly` | Request Gmail read-only access instead of read + write. Refused if the account already holds write access — see [OAuth Setup](/docs/guides/oauth-setup/#read-only-access) |
| `--display-name` | Set a display name for the account |
| `--no-default-identity` | Do not auto-confirm the email address as this account's "me" identity |

If `[oauth].service_account_key` or `[oauth.apps.<name>].service_account_key` is configured, `add-account` authorizes via Google service account domain-wide delegation instead of browser OAuth. Service-account accounts do not use `--headless`, `--force`, or `--readonly`; their scope comes from the domain-wide delegation grant in the Admin Console.

---

## add-imap

Add an IMAP account for syncing mail from any standard IMAP server.

```bash
msgvault add-imap --host <hostname> --username <email>
```

The command prompts interactively for your password (never accepted as a flag to avoid shell history exposure). For scripting or Docker, set `MSGVAULT_IMAP_PASSWORD` or pipe via stdin:

```bash
MSGVAULT_IMAP_PASSWORD="..." msgvault add-imap --host imap.example.com --username user@example.com
# or
echo "$PASS" | msgvault add-imap --host imap.example.com --username user@example.com
```

It tests the connection before saving credentials.

| Flag | Default | Description |
|---|---|---|
| `--host` | (required) | IMAP server hostname |
| `--username` | (required) | IMAP username or email address |
| `--port` | `993` | IMAP server port (993 for TLS, 143 for STARTTLS/plain) |
| `--starttls` | `false` | Use STARTTLS instead of implicit TLS |
| `--no-tls` | `false` | Disable TLS entirely (plaintext, not recommended) |
| `--no-default-identity` | `false` | Do not auto-confirm the username as this account's "me" identity |

Credentials are stored in `tokens/imap_<hash>.json` with restricted file permissions (0600). Use app-specific passwords when your provider supports them.

After adding an account, sync it with `msgvault sync-full`. IMAP accounts use the same `sync` and `sync-full` commands as Gmail. See [Setup Guide](/docs/setup/#add-an-imap-account) for a walkthrough.

---

## list-folders

List the selectable folders in one or all configured IMAP accounts, including
an approximate message count for each folder.

```bash
msgvault list-folders [account]
```

Use the folder names in repeated `--folder` or `--skip-folder` flags on
`sync-full` and `sync`. When the account argument is omitted, the command lists
folders for every configured IMAP account. See
[IMAP Folder Sync](/docs/usage/imap/) for examples and matching rules.

---

## add-o365

Add a Microsoft 365 or Outlook.com account via OAuth2 with XOAUTH2 IMAP authentication.

```bash
msgvault add-o365 <email>
```

The command opens your browser for Microsoft OAuth consent, then configures IMAP with XOAUTH2 automatically. The correct IMAP host is auto-detected: `outlook.office.com` for personal accounts (hotmail.com, outlook.com, live.com, msn.com) and `outlook.office365.com` for organizational accounts.

Requires a `[microsoft]` section with `client_id` in `config.toml`. See the [OAuth Setup guide](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail) for Azure AD app registration.

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID (restricts which accounts can authorize) |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this account's "me" identity |

After adding the account, sync it with `msgvault sync-full`.

---

## add-teams

Authorize a Microsoft Teams account through delegated Microsoft Graph OAuth and
register a `teams` source.

```bash
msgvault add-teams <email>
msgvault add-teams <email> --tenant <tenant-id>
```

This stores a Teams Graph token under `tokens/teams_<email>.json`, separate from
the Microsoft IMAP token used by `add-o365`. Requires `[microsoft].client_id` in
`config.toml` and the Graph permissions documented in
[Microsoft Teams](/docs/usage/teams/).

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID to use for authorization |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this source's "me" identity |

After adding the account, sync it with `msgvault sync-teams`.

---

## add-discord

Validate a Discord bot token and register one or more guilds as `discord`
sources. The token is read from a masked prompt or piped stdin; there is no
token flag.

```bash
msgvault add-discord
msgvault add-discord --guild 123456789012345678
msgvault add-discord --oauth-app archive-bot \
  --guild 123456789012345678 --guild 234567890123456789
```

If the bot can access exactly one guild, it is selected automatically and
echoed for confirmation. Otherwise, repeat `--guild` to select exact guild
IDs. One guild becomes one source. Setup also reports likely Message Content
Intent, channel-history, and private-thread access issues.

| Flag | Default | Description |
|---|---|---|
| `--guild` | sole accessible guild | Guild ID to register (repeatable/comma-separated) |
| `--oauth-app` | sole/default bot | Discord token binding label; no `[oauth.apps]` entry is required |

After registering a guild, sync it with `msgvault sync-discord`. See
[Discord](/docs/usage/discord/) for least-privilege bot setup and multi-bot binding
rules.

---

## sync-full

Download all messages from a Gmail or IMAP account. The optional account can
be an identifier or display name. When it is omitted, the command syncs all
configured syncable accounts.

```bash
msgvault sync-full [account] [flags]
```

| Flag | Description |
|---|---|
| `--limit N` | Maximum messages to download |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--query` | Gmail search query filter |
| `--noresume` | Ignore checkpoints, start fresh |
| `--folder NAME` | Scan this IMAP folder (repeatable) |
| `--skip-folder NAME` | Skip this IMAP folder (repeatable) |
| `--source-id ID` | Sync exactly one source by numeric ID; mutually exclusive with the account argument |
| `--verbose` | Detailed progress output |

An account token can select more than one matching source. Use `--source-id`
when Gmail and IMAP, or two other sources, share the same identifier.

The CLI sends the sync request to the configured remote server or local daemon
and streams the daemon's stdout/stderr back to the terminal. This keeps local
and remote full sync behavior aligned and avoids running a separate direct
SQLite writer beside `msgvault serve`.

---

## sync

Sync new and changed messages. Gmail accounts use the Gmail History API; IMAP accounts perform a mailbox scan and skip messages already in the database. The optional account can be an identifier or display name. When it is omitted, the command syncs all accounts that have completed an initial full sync.

```bash
msgvault sync [account] [flags]
```

| Flag | Description |
|---|---|
| `--folder NAME` | Scan this IMAP folder (repeatable) |
| `--skip-folder NAME` | Skip this IMAP folder (repeatable) |
| `--source-id ID` | Sync exactly one source by numeric ID; mutually exclusive with the account argument |

The CLI sends the incremental sync request to the configured remote server or
local daemon and streams the daemon's stdout/stderr back to the terminal. The
daemon serializes this work with other archive mutations.

Folder filters are applied only to IMAP accounts. See
[IMAP Folder Sync](/docs/usage/imap/) for examples and matching rules.

---

## sync-teams

Sync Microsoft Teams chats and channels for an authorized account.

```bash
msgvault sync-teams <email>
msgvault sync-teams <email> --no-channels
msgvault sync-teams <email> --limit 100
msgvault sync-teams <email> --full
```

Full versus incremental sync is detected from stored cursors and checkpoints.
`--full` ignores those cursors and re-fetches messages, upserting rows in place
so importer upgrades can repair existing data without creating duplicates.

| Flag | Default | Description |
|---|---|---|
| `--no-channels` | `false` | Sync chats only and skip team channels |
| `--limit` | `0` | Maximum messages per conversation (`0` = unlimited) |
| `--full` | `false` | Ignore stored cursor and re-fetch every message |

See [Microsoft Teams](/docs/usage/teams/) for setup, scheduling, search, and inline
media backfill.

---

## sync-discord

Sync one registered Discord guild, or every registered guild when the selector
is omitted.

```bash
msgvault sync-discord [guild-id-or-name]
msgvault sync-discord 123456789012345678 --after 2025-01-01
msgvault sync-discord 123456789012345678 --full
```

One-guild selection accepts an exact guild ID or an unambiguous display name.
Without a selector, sources run sequentially in stable source order. One guild
failure does not block later guilds, but the overall command returns an
aggregate error.

| Flag | Default | Description |
|---|---|---|
| `--full` | `false` | Ignore stored cursors and re-fetch all available history, repairing rows and historical deletions |
| `--after` | — | Exclusive lower bound in `YYYY-MM-DD` or RFC3339 form |

Normal runs re-scan the configured trailing edit window, seven days by
default. `--full --after` bounds both re-fetch and deletion repair and leaves
earlier rows untouched. See [Discord](/docs/usage/discord/) for per-channel/thread
checkpoint and deletion semantics.

---

## add-granola

Register a configured Granola account and validate its API key with a live
API call.

```bash
msgvault add-granola [identifier]
```

Reads the key from the matching `[[granola]]` entry in `config.toml`. With a
single configured entry the identifier may be omitted. Granola API keys are
created in the desktop app's settings and require a Business plan. The command
also confirms the source's effective `account_email`, even when other aliases
already exist. Meeting-source identity confirmation is mandatory.

After adding the account, sync it with `msgvault sync-granola`. If you later
change `account_email` or add/remove an alias with `msgvault identity`, run
`msgvault sync-granola <identifier> --full` to repair existing `is_from_me`
attribution.

---

## sync-granola

Sync Granola meeting notes and transcripts.

```bash
msgvault sync-granola [identifier]
msgvault sync-granola --limit 5
msgvault sync-granola --full --after 2024-01-01
```

Incremental by default: only notes updated since the last successful run are
fetched. With no identifier, every configured `[[granola]]` source is synced.
Re-fetched notes are upserted in place, so `--full` repairs existing rows
without creating duplicates. A partial run with one or more failed notes is
recorded and returned as an error without advancing the successful cursor. If
other notes were added or updated first, the cache is refreshed before the
error is returned. Scheduled sync refuses a configured source that has been
removed from the archive and directs you to run `add-granola` again.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum notes per run (`0` = unlimited) |
| `--after` | — | Full-sync only notes created after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every note |

See [Meeting Transcripts](/docs/usage/meetings/) for setup and what gets stored.

---

## add-notion-meetings

Validate a configured read-only Notion integration and register its meeting
source without printing meeting content.

```bash
msgvault add-notion-meetings [identifier]
```

The matching `[[notion_meetings]]` entry requires `account_email` and `token`.
With one entry, the identifier may be omitted.

---

## sync-notion-meetings

Sync Notion AI Meeting Notes from the attendee-visible recent window.

```bash
msgvault sync-notion-meetings [identifier]
msgvault sync-notion-meetings --limit 3
msgvault sync-notion-meetings --full --after 2026-01-01
msgvault sync-notion-meetings --probe
```

Notion returns at most 50 meeting notes and exposes no discovery cursor.
Every run hydrates and checksum-verifies the selected visible meetings.
`--full` also sends matching snapshots through archive upsert so cursor/archive
divergence can be repaired; `--after` filters the visible set locally. Partial
coverage is reported when Notion says more records exist. Due transcript
maintenance runs outside `--limit`.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum visible meetings hydrated and verified; due maintenance is additional |
| `--after` | — | Local visible-set lower bound in `YYYY-MM-DD` form; implies `--full` |
| `--full` | `false` | Force selected snapshots through archive upsert instead of skipping matching checksums |
| `--probe` | `false` | Validate capabilities and result shape without printing meeting content |

See [Meeting Transcripts](/docs/usage/meetings/#notion-ai-meeting-notes) for setup,
privacy, retry behavior, and stored evidence.

---

## add-circleback

Authorize a configured Circleback account using browser OAuth (their MCP
server uses OAuth with dynamic client registration).

```bash
msgvault add-circleback [identifier]
```

The token is stored under `tokens/circleback_<identifier>.json`. With a
single configured `[[circleback]]` entry the identifier may be omitted. The
command always confirms the source's effective `account_email`; there is no
identity opt-out flag.

Circleback redirects OAuth to `localhost:8090`. A CLI configured for a remote
daemon refuses to proxy this command: run it on the daemon host, where the
token is stored. Over SSH, use `ssh -L 8090:localhost:8090 user@daemon-host`,
run `msgvault --local add-circleback <identifier>` in that remote shell, and
open the printed authorization URL in your local browser if necessary. On the
daemon host, `--local` selects that host's archive; on a workstation it would
target a separate local archive.

After adding the account, sync it with `msgvault sync-circleback`. Run a
`--full` sync after changing the primary email or confirmed aliases.

---

## sync-circleback

Sync Circleback meetings, notes, action items, and transcripts.

```bash
msgvault sync-circleback [identifier]
msgvault sync-circleback --limit 5
msgvault sync-circleback --full --after 2024-01-01
msgvault sync-circleback --probe
```

Incremental by default: each run enumerates meeting IDs without a date bound
so newly created backfills are discovered even when their scheduled date is
old. Unknown meetings and known meetings created within the 48-hour refresh
overlap are fetched in detail; unchanged snapshots are skipped without
invalidating the search cache. With no identifier, every configured
`[[circleback]]` source is synced. Missing or recognized-empty transcripts
enter a bounded `pending` state and retry every six hours, normally until seven
days after the scheduled meeting time (48 hours when no usable time exists).
Expired retries become `unavailable`; a later `--full` run can check them again.

Due transcript retries are maintenance work and run outside the new-meeting
limit. Provider, contract, missing-result, ingest, archive-recovery, and
cancellation failures fail the sync and preserve the prior successful cursor.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum newly searched meetings; due maintenance items are additional (`0` = unlimited) |
| `--after` | — | Full-sync only meetings after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every meeting |
| `--probe` | `false` | Print the MCP tool inventory and a sample result instead of syncing |

See [Meeting Transcripts](/docs/usage/meetings/) for setup and what gets stored.

---

## backfill-teams-media

Re-fetch Microsoft Teams inline hosted-content media for already imported
messages.

```bash
msgvault backfill-teams-media <email>
msgvault backfill-teams-media <email> --only-incomplete
```

The command scans stored Teams HTML bodies for Graph `hostedContents` URLs and
downloads those images into the attachment store. It is idempotent because
attachments are content-addressed.

| Flag | Default | Description |
|---|---|---|
| `--only-incomplete` | `false` | Retry only messages whose inline media is still missing |

---

## backfill-discord-media

Refresh Discord message attachment metadata and retry pending downloads for
one guild or every registered guild.

```bash
msgvault backfill-discord-media [guild-id-or-name]
msgvault backfill-discord-media 123456789012345678 --only-incomplete
```

By default, all archived Discord messages with attachments are scanned;
already-complete messages require no download work. With no selector, guilds
run sequentially using the same aggregate-failure behavior as
`sync-discord`.

| Flag | Default | Description |
|---|---|---|
| `--only-incomplete` | `false` | Select only messages with pending attachment rows |

Backfill re-fetches each selected source message that has pending attachments
to obtain fresh signed CDN URLs. An incomplete attachment is unrecoverable if
the source message has since been deleted. See
[Discord](/docs/usage/discord/#attachment-backfill-and-limits).

---

## add-beeper

Register the chat accounts connected to a locally running
[Beeper Desktop](/docs/usage/beeper/) as `beeper` sources, one per network.

```bash
msgvault add-beeper
msgvault add-beeper --token-file ~/beeper-token.txt
MSGVAULT_BEEPER_TOKEN="..." msgvault add-beeper
```

Requires Beeper Desktop running on the same machine and an access token
minted in Beeper Desktop (Settings → Developer). The token is stored at
`tokens/beeper.json`. Accounts filtered out by `[beeper].accounts` /
`exclude_accounts` in `config.toml` are skipped.

Networks Beeper serves natively instead of bridging — iMessage — are absent
from its accounts API, so they are found from chat data and reported as *found
via chats*. Re-run the command after connecting a network in Beeper Desktop.

| Flag | Default | Description |
|---|---|---|
| `--token-file` | | Read the access token from a file instead of prompting |
| `--no-default-identity` | `false` | Do not auto-confirm each account's own identity as that source's "me" identity |

After adding, sync with `msgvault sync-beeper`.

---

## sync-beeper

Sync chats from Beeper Desktop for every registered Beeper account (all
connected networks). The first run backfills full locally-available history and
is resumable; later runs are incremental. Per-account failures do not stop the
run: remaining accounts still sync, the analytics cache is rebuilt for the
successful ones, and the command exits non-zero listing the failures. Without
`--account`, the `[beeper]` config `accounts`/`exclude_accounts` filters
select which registered sources sync. See [Beeper](/docs/usage/beeper/).

```bash
msgvault sync-beeper
msgvault sync-beeper --account signal --account telegram
msgvault sync-beeper --full
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to sync (repeatable) |
| `--limit` | `0` | Max messages per chat this run (0 = no limit; limited backfills resume next run) |
| `--full` | `false` | Ignore stored cursors and re-fetch every message (repairs rows in place) |
| `--no-media` | `false` | Skip attachment downloads for this run |

---

## backfill-beeper-media

Retry pending Beeper attachment downloads (media that failed or exceeded the
size cap during `sync-beeper`). Idempotent: attachments are content-addressed.
If the source message is gone (deleted in Beeper), its pending markers are
cleared permanently — already-downloaded attachments are kept. Transient fetch
errors keep the marker and count as still pending, so re-running retries them.

```bash
msgvault backfill-beeper-media
msgvault backfill-beeper-media --account signal
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to backfill (repeatable) |

---

## add-slack

Register a [Slack workspace](/docs/usage/slack/) as a `slack` source. Requires a
user token (`xoxp-…`) from an internal Slack app you create (see the usage
guide for the two-minute setup and scope list). The token is validated with
`auth.test` plus a `search.messages` probe (thread-reply archiving needs the
`search:read` scope, so an under-scoped token fails here rather than on
every future sync) and stored at `tokens/slack_<team-id>_<user-id>.json`.

```bash
msgvault add-slack
msgvault add-slack --token-file ~/slack-token.txt
MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack
```

| Flag | Default | Description |
|---|---|---|
| `--token-file` | | Read the user token from a file instead of prompting |
| `--no-default-identity` | `false` | Do not auto-confirm the workspace user ID as the source's "me" identity |

After adding, sync with `msgvault sync-slack`.

---

## import-slackdump

Import a Slackdump directory or ZIP without contacting Slack. `--me` resolves
your account from `users.json` by exact Slack user ID or unique profile email.
The import preserves conversations, threads, reactions, raw JSON, identities,
and exported files. The path must be local to the daemon host; exports are not
uploaded through a configured remote. See
[Slack](/docs/usage/slack/#import-a-slackdump-export).

```bash
msgvault import-slackdump --me you@example.com /path/to/export
msgvault import-slackdump --me U0123456789 --limit 100 /path/to/export.zip
```

| Flag | Default | Description |
|---|---|---|
| `--me` | required | Slack user ID or unique profile email in the export |
| `--limit` | `0` | Max messages imported per conversation (0 = no limit) |
| `--max-media-mb` | configured/default limit | Max exported file size in MiB (0 = configured/default limit) |
| `--no-default-identity` | `false` | Do not auto-confirm the workspace user ID as the source's "me" identity |

---

## sync-slack

Sync Slack conversations — channels you are a member of, group DMs, and 1:1
DMs — for registered workspaces. The first run backfills full history and is
resumable; later runs are incremental and sweep for thread replies created
since the last run (any thread age). Per-workspace failures do not stop the run: remaining workspaces
still sync and the command exits non-zero listing the failures. The `[slack]`
config `channels`/`exclude_channels` filters select which channels sync. See
[Slack](/docs/usage/slack/).

```bash
msgvault sync-slack
msgvault sync-slack T0123456789
msgvault sync-slack --full
```

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Max messages of work per conversation this run, thread replies included; the reply sweep gets the same budget workspace-wide (0 = no limit; every phase resumes next run so standing limited schedules converge; only the maintenance rescan is skipped) |
| `--full` | `false` | Start (or continue) a repair session: re-fetch every message, upserting in place (catches old thread replies and edits). Interrupted or --limit-scoped repairs resume across later runs of any kind until complete |
| `--no-threads` | `false` | Skip thread-reply fetching for this run (a later threaded run pays the debt automatically) |
| `--maintenance` | `false` | Repair edits/reaction changes on recent messages (ignored by default after capture) |
| `--no-media` | `false` | Skip file downloads for this run (files become pending markers; `backfill-slack-media` fetches them later) |

---

## backfill-slack-media

Retry pending Slack file downloads (files that failed or exceeded the size
cap during `sync-slack`). Idempotent: files are content-addressed and
already-downloaded ones are never re-fetched. Files hosted outside
`files.slack.com` are metadata-only link rows and are never downloaded.

```bash
msgvault backfill-slack-media
msgvault backfill-slack-media T0123456789
```

---

## add-calendar

Authorize read-only Google Calendar access for an account and register its calendars for sync. If the account already has a Gmail token, re-consent bundles Gmail + Calendar so Gmail access is not dropped — keep both checked on the consent screen. The Calendar API must be enabled on the OAuth project. By default only owned/writable calendars are registered.

```bash
msgvault add-calendar <email> [flags]
```

| Flag | Description |
|---|---|
| `--oauth-app` | Named OAuth app to use |
| `--headless` | Print token-copy instructions for a headless host instead of opening a browser |
| `--all-calendars` | Include reader/freeBusyReader (subscribed, holiday) calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--calendars` | Comma-separated calendar IDs to register |

---

## sync-calendar

Sync Google Calendar events for an account. The account is resolved from a `[[gcal]]` config entry (by name or email) or used directly as an email. The first run (or `--full`) does a full sync that registers calendars; later runs are incremental via the Calendar `syncToken`. Events are stored as searchable records (`message_type = calendar_event`) and become eligible for semantic search when the embedding worker runs. Cancelled events are retained and marked cancelled, never deleted. Sync is read-only.

```bash
msgvault sync-calendar <name|email> [flags]
```

| Flag | Description |
|---|---|
| `--full` | Force a full sync (ignore stored sync tokens) |
| `--limit` | Max events per calendar (0 = unlimited) |
| `--after` / `--before` | Bound a full sync to a date range (`YYYY-MM-DD`); full sync only |
| `--calendar` | Restrict to specific calendar IDs |
| `--all-calendars` | Include reader/freeBusyReader calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--oauth-app` | Named OAuth app to use |
| `--noresume` | Do not resume an interrupted full sync |

---

## import-mbox

Import a local MBOX archive into msgvault.

```bash
msgvault import-mbox <identifier> <export-file>
```

The export file may be a plain mbox file (any extension) or a `.zip` containing one or more `.mbox`/`.mbx` files.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `mbox` | Source type recorded in database (e.g., `hey` for HEY.com) |
| `--label` | — | Label(s) to apply to imported messages (repeatable, or comma-separated) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-emlx

Import Apple Mail `.emlx` files into msgvault. Can auto-discover accounts from macOS `Accounts4.sqlite` or accept explicit arguments.

```bash
# Auto-discover accounts (reads ~/Library/Accounts/Accounts4.sqlite)
msgvault import-emlx

# Specify mail directory
msgvault import-emlx <mail-dir>

# Legacy form: explicit identifier and directory
msgvault import-emlx <identifier> <mail-dir>
```

The mail directory should be an Apple Mail mailbox tree containing `.mbox` or `.imapmbox` directories, each with a `Messages/` subdirectory of `.emlx` files. You can also point directly at a single `.mbox` directory. Labels are derived from directory names.

Apple Mail's `N.partial.emlx` files are also imported: their message body is
complete even when uncached attachment parts are absent. If both `N.emlx` and
`N.partial.emlx` exist, the complete `N.emlx` copy wins. The command summary
reports the number of partial files imported.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `apple-mail` | Source type recorded in database |
| `--account` | — | Filter to specific account(s) during auto-discover (repeatable) |
| `--accounts-db` | — | Custom path to macOS `Accounts4.sqlite` |
| `--identifier` | — | Manual identifier when auto-discover is not suitable |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-pst

Import a Microsoft Outlook PST archive into msgvault.

```bash
msgvault import-pst <identifier> <pst-file>
```

The importer preserves PST folder structure as labels, imports email messages, and skips non-email PST items such as calendar entries, contacts, tasks, and notes.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `pst` | Source type recorded in database |
| `--skip-folder` | — | Folder name to skip, case-insensitive; repeat for multiple folders |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-whatsapp

Import messages from a decrypted WhatsApp `msgstore.db` SQLite database.

```bash
msgvault import-whatsapp <msgstore.db> --phone <your-number>
```

The `--phone` flag is required and must be in E.164 format (e.g., `+447700900000`).

| Flag | Required | Description |
|---|---|---|
| `--phone` | Yes | Your phone number in E.164 format (must start with `+`) |
| `--contacts` | No | Path to contacts `.vcf` file for name resolution |
| `--media-dir` | No | Path to decrypted Media folder for attachments |
| `--limit` | No | Limit number of messages (for testing) |
| `--display-name` | No | Display name for the phone owner |
| `--no-default-identity` | No | Do not auto-confirm the phone number as this source's "me" identity |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-imessage

Import messages from the local iMessage database on macOS. Requires Full Disk Access in System Settings.

```bash
msgvault import-imessage
```

Reads from `~/Library/Messages/chat.db` by default. This is a read-only operation.

| Flag | Default | Description |
|---|---|---|
| `--db-path` | `~/Library/Messages/chat.db` | Path to chat.db |
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--me` | — | Your phone/email for recipient tracking |
| `--contacts` | — | Path to contacts `.vcf` file for display-name backfill |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-gvoice

Import texts, calls, and voicemails from a Google Voice Takeout export.

```bash
msgvault import-gvoice <takeout-voice-dir>
```

The directory must be the "Voice" folder from a Google Takeout export, containing `Calls/` and `Phones.vcf`.

| Flag | Default | Description |
|---|---|---|
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-default-identity` | `false` | Do not auto-confirm the phone number as this source's "me" identity |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-messenger

Import Facebook Messenger conversations from a Download Your Information export.

```bash
msgvault import-messenger --me <you@facebook.messenger> <dyi-export-dir>
```

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your synthetic Messenger identifier, e.g. `test.user@facebook.messenger` |
| `--format` | `auto` | Export format: `auto`, `json`, `html`, or `both` |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-synctech-sms

Import SMS Backup & Restore XML or ZIP backups.

```bash
msgvault import-synctech-sms <path> --owner-phone <your-number>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--sms` | `true` | Import SMS records |
| `--mms` | `true` | Import MMS records |
| `--calls` | `true` | Import call logs |
| `--attachments` | `true` | Import MMS attachments |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## add-synctech-sms-drive

Configure a Google Drive source for SMS Backup & Restore backups.

```bash
msgvault add-synctech-sms-drive <name> --owner-phone <number> --folder-id <id> --google-account <email>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--folder-id` | (required) | Google Drive folder ID containing backups |
| `--google-account` | (required) | Google account used for Drive access |
| `--schedule` | `30 4 * * *` | Cron schedule used by `msgvault serve` |
| `--oauth-app` | — | Named Google OAuth app to use |

---

## sync-synctech-sms

Run one configured SMS Backup & Restore source immediately.

```bash
msgvault sync-synctech-sms <name>
```

---

## backup

Create, list, verify, and restore incremental archive snapshots in a backup
repository.

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup list --repo ~/Backups/msgvault
msgvault backup verify --repo ~/Backups/msgvault
msgvault backup restore --target ~/msgvault-restored --repo ~/Backups/msgvault
```

Every backup subcommand requires a repository: pass `--repo`, or set
`[backup].repo` in `config.toml` to omit it. `backup init` initializes the
repository directory but does not modify `config.toml`.
`backup create` is routed through the selected daemon so the daemon can freeze a
consistent SQLite snapshot while it scans pages and attachments. `backup verify`
and `backup restore` run locally against the repository because they do not
write the live archive.

### backup init

```bash
msgvault backup init --repo <dir>
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |

### backup create

```bash
msgvault backup create [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--include-config` | Include `config.toml` verbatim; may contain API keys |
| `--include-tokens` | Include OAuth token files |
| `--allow-plaintext-secrets` | Allow config/tokens in an unencrypted repository |
| `--tag <text>` | Optional label recorded on the snapshot manifest |
| `--force-unlock` | Break a stale exclusive repository lock before creating |
| `--jobs N` | Concurrent attachment capture workers; `0` uses one per CPU |

### backup list

```bash
msgvault backup list [--repo <dir>]
```

Prints snapshot ID, creation time, message count, bytes added, and tag.

### backup verify

```bash
msgvault backup verify [snapshot] [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--all` | Verify every snapshot instead of only the latest |
| `--quick` | Skip reading and hash-verifying content blobs |
| `--force-unlock` | Break a stale exclusive repository lock before verifying |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

### backup restore

```bash
msgvault backup restore [snapshot] --target <dir> [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--target <dir>` | Directory to restore into (required) |
| `--overwrite` | Allow restoring into a non-empty target directory |
| `--integrity-check` | Run SQLite's full integrity check after restoring (slow for large databases) |
| `--loose-attachments` | Restore attachments as individual loose files instead of installing compatible packs |
| `--force-unlock` | Break a stale exclusive repository lock before restoring |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

By default, restore installs compatible repository packs directly into the
restored attachment store. Every selected attachment is still read and
SHA-256 verified; entries that exceed the target store's maintenance limits,
or packs that use an incompatible representation, are restored loose instead.
The summary reports the resulting packed/loose split and any fallback reasons.
Every restore verifies database pages and content blobs by hash and compares
the restored database statistics with the snapshot manifest. Pass
`--integrity-check` to additionally run SQLite's full `PRAGMA integrity_check`
scan; it is optional because it can dominate restore time for large databases.

Use `--loose-attachments` for downgrade or recovery. Restore into a fresh
target with that flag to guarantee a fully loose result. An overwritten target
can retain uncataloged old pack files, and `unpack-attachments` processes only
cataloged packs, so overwrite cannot currently make the same guarantee.
Restoring into the live archive home of a running daemon is refused. See
[Backup](/docs/usage/backup/) for repository format, scheduling, verification, and
privacy details.

---

## pack-attachments

Move every eligible loose content-addressed attachment into sealed immutable
pack files:

```bash
msgvault pack-attachments
```

The daemon serializes packing against sync and backup operations. Reads remain
available from loose, packed, or mixed storage, and the command is safe to
rerun as new loose content arrives. Bounded packing also runs after successful
attachment-producing operations and during scheduled maintenance; this command
processes the complete eligible backlog immediately.

With `[data].loose_attachments = true`, automatic packing is disabled and this
command refuses to run.

---

## repack-attachments

Reclaim dead bytes from sparse attachment packs:

```bash
msgvault repack-attachments
```

Repack always runs through the selected daemon so it can atomically replace
live blob mappings, retire shared readers, and remove old pack files. It is
safe to retry after interruption or a Windows file-sharing error.
It refuses to run when `[data].loose_attachments = true` because repacking
creates replacement pack files.

---

## unpack-attachments

Restore every cataloged packed attachment to a loose file and remove its pack:

```bash
msgvault daemon stop
msgvault unpack-attachments
```

Each object is SHA-256 verified as it is written. This is the downgrade and
recovery escape hatch because msgvault versions before packed attachment
support cannot read the packs. The command is local-only and refuses to run
while a daemon holds pack readers open. With `[remote]` configured, run it on
the archive host or pass `--local` there to select that host's local archive.

---

## search

Search the archive with Gmail-like query syntax. Supports keyword (FTS5), semantic, and hybrid modes.

```bash
msgvault search <query> [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Maximum number of results (default: 50) |
| `--offset N` | Skip first N results (only valid for `--mode fts`) |
| `--json` | Output results as JSON |
| `--account` | Limit results to a specific account |
| `--collection` | Limit results to all member accounts of a collection |
| `--message-type` | Limit results to one or more message types, e.g. `email`, `teams`, `discord`, `calendar_event`, `sms` |
| `--deletion-scope` | Source-deletion scope: `active` (default), `deleted`, or `any`. Non-active scopes require `--mode fts`. |
| `--mode` | Search mode: `fts` (default), `vector`, or `hybrid`. `vector` and `hybrid` require vector search to be configured. |
| `--explain` | Include per-signal scores (RRF, BM25, vector) in the output. Only applies to `--mode vector` and `--mode hybrid`. |

Without an explicit message-type filter, search intentionally returns all
matching cached message types, including meeting transcripts and chats.
Ordinary aggregate views and statistics still default to email-only; use
`--message-type` (or `message_type:` in the query) when you need an explicit
search scope.

The query accepts `list:` and `list-id:` as equivalent RFC 2919 List-Id
filters. Values are case-insensitive literal substrings; quote values that
contain spaces. Repeating either alias requires every value to match.

`--mode vector` and `--mode hybrid` require at least one free-text term in the query (filter-only queries use `--mode fts`). They do not support pagination (`--offset` is rejected) or non-active deletion scopes because the vector index covers active messages only. Bump `--limit` to retrieve a larger candidate pool instead. See [Searching](/docs/usage/searching/) for the operator reference and [Vector Search](/docs/usage/vector-search/) for semantic setup.

---

## repair-list-ids

Re-derive archived email List-Id values from stored raw MIME without
contacting a provider.

```bash
msgvault repair-list-ids [--apply]
```

The default is a dry run and does not modify the archive. Pass `--apply` to
write changed values and mark derived analytics stale for the normal rebuild path.

---

## documents

Manage hosted extraction and local full-text indexing for standalone document
attachments. Provider operations require a private authenticated capability
manifest and exact recorded consent; local search and removal operations do
not contact the provider.

```bash
msgvault documents probe-mistral --fixtures <private-dir> [--validate-only]
msgvault documents consent-mistral --capabilities <manifest> [--yes]
msgvault documents build --capabilities <manifest> [--limit N] [--full-rebuild] [--yes]
msgvault documents resume --capabilities <manifest> [--limit N] [--yes]
msgvault documents search <query> [flags]
msgvault documents status --capabilities <manifest> [--json]
msgvault documents retry --capabilities <manifest> --hash <sha256>
msgvault documents retire <profile-id> [--yes]
msgvault documents purge-derived --hash <sha256> [--yes]
```

`probe-mistral --validate-only` checks the complete synthetic fixture set
without credentials or network access. An authenticated probe writes the
manifest to stdout. Run consent and build commands once without `--yes` to
review their disclosure and preflight.

`documents search` accepts `--source-id`, `--message-type`, `--attachment-id`,
`--message-id`, `-n`/`--limit`, `--cursor`, and `--json`. Its cursor is opaque
and bound to a stable index revision; restart pagination after a stale-cursor
error.

See [Document Attachment Indexing](/docs/usage/document-indexing/) for fixture
generation, configuration, privacy boundaries, scheduling, and recovery.

---

## tui

Launch the interactive terminal interface.

```bash
msgvault tui [flags]
```

| Flag | Description |
|---|---|
| `--local` | Use the local daemon instead of the configured remote server |

Analytics engine and cache behavior are daemon-managed. Configure `[analytics].engine` and `[analytics].auto_build_cache` in `config.toml` to force live SQL, require DuckDB, or disable automatic cache builds. See [Configuration: analytics](/docs/configuration/#analytics).

Deprecated in 0.17.0: the older TUI-only `--force-sql`, `--no-cache-build`, and `--no-sqlite-scanner` flags are hidden and no longer control the foreground CLI. Use `[analytics].engine = "sql"` for live SQL, `[analytics].auto_build_cache = false` to skip daemon cache builds, or `msgvault build-cache` to prebuild cache files on the daemon host.

---

## export-messages

Export a bounded, provider-neutral message window as deterministic JSON Lines.
The command reads the archive through the configured daemon and never contacts
a provider.

```bash
msgvault export-messages \
  --start <RFC3339> \
  --end <RFC3339> \
  [--message-type <type>] \
  [--source <type:identifier>] \
  [--format jsonl]
```

| Flag | Default | Description |
|---|---|---|
| `--start` | required | Inclusive RFC3339 lower bound |
| `--end` | required | Exclusive RFC3339 upper bound |
| `--message-type` | all | Exact message type to include; repeatable |
| `--source` | all | Exact typed source selector; repeatable |
| `--format` | `jsonl` | Output format; v1 accepts only `jsonl` |

The stream schema is `msgvault-message-export/1`. Records appear as one
manifest, all sources, all conversations, all messages, and one completion
record with counts. A missing completion record means the stream is partial
and must be rejected. Stdout contains JSONL only; diagnostics use stderr.

See [Exporting Data](/docs/usage/exporting/) for the full record, identity, ordering,
and validation contract.

---

## export-eml

Export a message as a `.eml` file. Accepts either a numeric database ID or a Gmail message ID.

```bash
msgvault export-eml <id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file (default: `<gmail_id>.eml`, use `-` for stdout) |

---

## export-attachment

Export an attachment by its SHA-256 content hash.

```bash
msgvault export-attachment <content-hash> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file path (use `-` for stdout) |
| `--base64` | Output raw base64 to stdout |
| `--json` | Output as JSON with base64-encoded data |

The `--json`, `--base64`, and `--output` flags are mutually exclusive.

See [Exporting Data](/docs/usage/exporting/) for usage examples.

---

## export-attachments

Export all attachments from a message as individual files.

```bash
msgvault export-attachments <message-id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <dir>` | Output directory (default: current directory) |

Accepts internal numeric IDs or Gmail message IDs. See [Exporting Data](/docs/usage/exporting/) for usage examples.

---

## export-token

Export a browser-created OAuth refresh token to a remote msgvault instance.

Use this for headless deployments (NAS, cloud VM, any remote server) that cannot run a browser flow.

```bash
msgvault export-token <email> [flags]
```

| Flag | Description |
|---|---|
| `--to <url>` | Remote msgvault URL (or `MSGVAULT_REMOTE_URL`) |
| `--api-key <key>` | API key (or `MSGVAULT_REMOTE_API_KEY`) |
| `--allow-insecure` | Allow HTTP for trusted networks (for example Tailscale) |

`export-token` uploads `~/.msgvault/tokens/<email>.json` to `/api/v1/auth/token/<email>`, saves it in the remote token store, and posts account metadata to `/api/v1/accounts`.

---

## verify

Verify archive integrity against Gmail through the configured remote server or
local daemon. The command streams the daemon's stdout/stderr back to the
terminal.

```bash
msgvault verify <email> [flags]
```

| Flag | Description |
|---|---|
| `--sample N` | Messages to sample (default: 100) |
| `--skip-db-check` | Skip SQLite integrity check |
| `--json` | Emit machine-readable JSON summary |

---

## stats

Show archive statistics.

```bash
msgvault stats [flags]
```

| Flag | Description |
|---|---|
| `--account` | Show stats for a specific account |
| `--collection` | Show stats for all member accounts of a collection |

---

## identity

Manage the confirmed "me" identifiers for each account.

The identity subcommands use the configured remote server or local daemon by default. `--local` uses the local daemon even when a remote is configured.

```bash
msgvault identity list [--account <account> | --collection <name> | --source-id <id>]
msgvault identity show [<account>] [--source-id <id>]
msgvault identity add [<account>] <identifier> [--source-id <id>]
msgvault identity remove [<account>] <identifier> [--source-id <id>]
msgvault identity discover [<account>] [--source-id <id>] [--apply]
msgvault identity import [<account>] [--source-id <id>] (--file <path> | --stdin)
```

| Command | Description |
|---|---|
| `identity list` | List confirmed identifiers across accounts |
| `identity show <account>` | Show one account's identity in detail |
| `identity add <account> <identifier>` | Add a confirmed identifier |
| `identity remove <account> <identifier>` | Remove a confirmed identifier |
| `identity discover <account>` | Preview archived source evidence; `--apply` confirms strong candidates |
| `identity import <account>` | Preview or apply source-scoped identifiers from text or JSON |

| Flag | Applies to | Description |
|---|---|---|
| `--account` | `list` | Restrict to a single account |
| `--collection` | `list` | Restrict to all member accounts of a collection |
| `--source-id` | all subcommands | Select one source unambiguously by numeric ID; mutually exclusive with an account argument or `list` scope |
| `--json` | `list`, `show`, `discover`, `import` | Output structured JSON; discovery also suppresses progress |
| `--signal` | `add` | Evidence signal name (default `manual`) |
| `--apply` | `discover` | After the complete preview scan, confirm strong evidence |
| `--provider` | `discover` | Include the source's configured `[[fastmail]]` alias inventory |
| `--confirm <address>` | `discover` | Explicitly confirm one weak candidate; repeatable and requires `--apply` |
| `--file <path>` / `--stdin` | `import` | Read a text or JSON identity list from exactly one input |
| `--signal` | `import` | Evidence signal recorded for imported identities (default `manual`) |
| `--apply` | `import` | Confirm every validated imported identity; without it the command only previews |

Email sync enriches identities already confirmed for the source with strong
sender evidence from trusted Sent metadata; it does not confirm first-time
aliases. Review candidates with `msgvault identity discover` and apply strong
candidates with `msgvault identity discover --apply`. Recipient-only evidence
stays review-only. See [People, Profiles, and Source Identities](/docs/usage/people/)
for classifications, Fastmail inventory, and import formats.

---

## person

Manage durable person profiles and their typed, historized attributes. A
profile is created only by explicit promotion of an observed participant's
identity cluster.

```bash
msgvault person promote <participant-id>
msgvault person list [--json]
msgvault person get <person-id> [--json]
msgvault person set-display-name <person-id> <display-name> [--json]
msgvault person set-display-name <person-id> --clear [--json]
msgvault person delete <person-id>
msgvault person merge <survivor-id> <absorbed-id> [flags]
msgvault person split <source-person-id> [flags]
msgvault person merge-history <person-id> [--json]
msgvault person merge-show <merge-id> [--snapshot] [--json]
msgvault person merge-candidate <candidate-id> [flags]

msgvault person attributes list <person-id> [--slug <slug>] [--history] [--json]
msgvault person attributes set <person-id> <slug> (--value <scalar> | --value-json <json|@path|->) [flags]
msgvault person attributes clear <person-id> <slug> [flags]
```

`promote` is idempotent. `set-display-name` preserves the profile's stable ID
and vCard UID. `delete` permanently retires that UID and removes the profile's
participant bindings. A person with active merge lineage cannot be deleted
until that lineage is fully split.

`merge` keeps the survivor's ID and vCard UID, moves the absorbed profile into
it, and records a reversible merge packet. Profiles with active CardDAV
publication are rejected. `split` creates a new person and UID; repeat
`--participant` to select multiple absorbed lineages, or omit it for a merge
whose absorbed profile had no participants.

| Merge flag | Applies to | Description |
|---|---|---|
| `--survivor-revision <n>` | `merge` | Required expected revision of the surviving person |
| `--absorbed-revision <n>` | `merge` | Required expected revision of the absorbed person |
| `--idempotency-key <key>` | `merge`, `split` | Required retry key; reusing it with a different request is rejected |
| `--merge-id <id>` | `split` | Required merge record to reverse |
| `--participant <id>` | `split` | Absorbed participant lineage to move; repeat as needed |
| `--revision <n>` | `split`, `merge-candidate` | Required expected revision of the current person |
| `--person-id <id>` | `merge-candidate` | Person that owns the review candidate |
| `--decision <accepted\|rejected>` | `merge-candidate` | Resolve a conflicting single-value attribute |
| `--snapshot` | `merge-show` | Read and verify the immutable merge snapshot |
| `--json` | all merge commands | Output structured JSON |

Exact splits restore the pre-merge profiles when their lineage and referenced
rows remain available. For partial splits, use `--json` to inspect ambiguous or
unrestored rows. Complete merge packets retain merge-time profile values even
after later redaction and require the strongest profile-data options when
copied into a subset. See [People, Profiles, and Source Identities](/docs/usage/people/#merge-duplicate-profiles-and-reverse-a-merge)
for the workflow and lifecycle boundaries.

| Attribute flag | Applies to | Description |
|---|---|---|
| `--slug <slug>` | `list` | Restrict output to one definition |
| `--history` | `list` | Include superseded values instead of current values only |
| `--value <scalar>` | `set` | Coerce text, integer, real, boolean, date, or timestamp input to the definition's type |
| `--value-json <json\|@path\|->` | `set` | Supply a structured typed-value envelope |
| `--ordinal <n>` | `set`, `clear` | Address one slot of a multi-valued definition |
| `--source <name>` | `set` | Provenance: `user`, `carddav_import`, `vcard_import`, `archive_observation`, `extraction`, `enrichment`, or `system` |
| `--source-ref <value>` | `set` | Resource or message reference that produced the value |
| `--confidence <0..1>` | `set` | Confidence for a derived or suggested value |
| `--actor <value>` | `set` | Actor recorded with the value |
| `--expected-value-id <id>` | `set`, `clear` | Compare-and-swap guard for the current value |
| `--dry-run` | `set`, `clear` | Validate and preview without writing |
| `--json` | `list`, `set`, `clear` | Output structured JSON |

Setting or clearing a value closes the current history row rather than deleting
it. See [People, Profiles, and Source Identities](/docs/usage/people/) for the
shipped definitions and complete workflow.

---

## person provider set

Update the mutable policy fields of an existing named people inference provider
profile in place. The protocol, endpoint, auth scheme, credential source,
executable, execution boundary, selection, and enablement stay unchanged.

```bash
msgvault person provider set <name> --model <model> [flags]
```

The command reruns the exact synthetic provider check and revokes consent for
the previous fingerprint. Grant fresh consent with
`msgvault person provider consent <name> --yes` after reviewing the updated
policy. A running local daemon requires `msgvault daemon restart` before its
scheduled sweeps observe the change. Configured remote daemons refuse this
mutation; run it on the daemon host or pass `--local`.

| Flag | Default | Description |
|---|---|---|
| `--model` | unchanged | Provider model identifier |
| `--retention-posture` | unchanged | Provider retention assertion |
| `--training-posture` | unchanged | Provider training assertion |
| `--source` | unchanged | Allowed source class; repeatable, replaces the existing list |
| `--source-since` | unchanged | Earliest disclosed source date |
| `--source-until` | unchanged | Latest disclosed source date |
| `--allow-sensitive` | unchanged | Allow sensitive text in provider packets |
| `--reasoning-effort` | unchanged | Explicit reasoning effort |
| `--reasoning-mode` | unchanged | Explicit reasoning mode |
| `--request-timeout` | unchanged | Provider request timeout |
| `--yes` | `false` | Confirm the final provider and privacy values; required |
| `--json` | `false` | Output structured JSON |

---

## attribute-definition

Manage portable field metadata. Definitions add no runtime database columns;
their universal IDs and slugs remain stable while labels and descriptions may
change.

```bash
msgvault attribute-definition list [--object-type person|organization] [--include-hidden] [--json]
msgvault attribute-definition get <definition-id> [--json]
msgvault attribute-definition create --definition <json|@path|-> [--dry-run] [--json]
msgvault attribute-definition rename <definition-id> [--label <text>] [--description <text> | --clear-description] [--json]
msgvault attribute-definition delete <definition-id>
```

`create --dry-run` performs local structural validation but cannot detect a
conflict with definitions already stored by the daemon. `rename` does not
change the immutable slug or universal ID. Deletion is limited to user-created,
deletable definitions that have no stored values.

---

## collection

Manage named groups of accounts.

The collection subcommands use the configured remote server or local daemon by
default. `--local` uses the local daemon even when a remote is configured.

```bash
msgvault collection create <name> --accounts <account1,account2,...>
msgvault collection list
msgvault collection show <name>
msgvault collection add <name> --accounts <account1,account2,...>
msgvault collection remove <name> --accounts <account1,account2,...>
msgvault collection delete <name>
```

Deleting a collection does not delete sources or messages.

---

## deduplicate

Find and merge duplicate messages within an account or collection.

```bash
msgvault deduplicate [flags]
```

By default, each source is deduplicated independently. `--collection` is the explicit opt-in for cross-source deduplication.

| Flag | Description |
|---|---|
| `--dry-run` | Scan and report only; do not hide duplicates |
| `--account` | Scope dedup to one account |
| `--collection` | Dedup across every member account of a collection |
| `--content-hash` | Also detect duplicates by normalized raw MIME content |
| `--prefer` | Comma-separated source type preference order |
| `--undo <batch-id>` | Restore rows hidden by a previous dedup run; repeatable |
| `--delete-dups-from-source-server` | Stage same-source pruned duplicates for remote deletion |
| `--no-backup` | Skip the database backup before merging |
| `-y`, `--yes` | Skip confirmation prompt |

---

## delete-deduped

Permanently delete dedup-hidden messages from the selected msgvault archive.

```bash
msgvault delete-deduped --batch <batch-id>
msgvault delete-deduped --all-hidden
```

The CLI sends the request to the configured remote daemon, or to the
auto-started local daemon when no remote is configured. It no longer opens the
SQLite database directly. The delete cannot be undone with `deduplicate --undo`;
when backups are enabled, the daemon writes the backup next to the database it
owns before deleting. Repeated `--batch` selections commit as one transaction;
if the request is canceled, none of the selected batches are left half-deleted.

| Flag | Description |
|---|---|
| `--batch` | Delete rows hidden by this dedup batch ID; repeatable |
| `--all-hidden` | Delete every dedup-hidden row regardless of batch |
| `--no-backup` | Skip database backup before deleting |
| `-y`, `--yes` | Skip confirmation prompt (`--all-hidden` still prompts) |

---

## list-senders

List top senders by message count.

```bash
msgvault list-senders [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## list-domains

List top sender domains by message count.

```bash
msgvault list-domains [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## list-labels

List all labels with message counts.

```bash
msgvault list-labels [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## build-cache

Build or update the Parquet analytics cache through the configured remote server
or the local daemon.

```bash
msgvault build-cache [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Discard existing cache and rebuild |

The CLI sends the request over HTTP and streams the daemon's stdout/stderr back
to the terminal. A local daemon runs the DuckDB export in an isolated child
process so DuckDB's bundled SQLite library never opens the archive inside the
long-lived daemon process. With `[remote].url` configured, the remote daemon
builds its own cache; use `--local` only to target this machine's local daemon.

For automatic cache rebuilds after daemon-owned syncs, configure
`[analytics].auto_build_cache` in `config.toml`.

---

## rebuild-fts

Rebuild the SQLite FTS5 search index.

```bash
msgvault rebuild-fts
```

Use this if `verify` reports FTS5 shadow-table corruption such as a malformed inverted index. The command rebuilds the search index from the canonical `messages` table.

---

## embeddings

Manage the vector embedding index used by `--mode vector` and `--mode hybrid` search. Requires a build with a vector backend (`sqlite_vec` for SQLite archives, `pgvector` for PostgreSQL archives) and a configured `[vector.embeddings]` endpoint. See [Vector Search](/docs/usage/vector-search/) for prerequisites, model rotation, and troubleshooting.

```bash
msgvault embeddings <subcommand> [flags]
```

| Subcommand | Description |
|---|---|
| `build` | Build or update the index. Incremental by default; `--full-rebuild` starts a new generation. |
| `resume` | Continue scan-and-fill embedding for the building or active generation. Always incremental. |
| `list` | List index generations with their state, model, dimension, and pending count. |
| `activate <generation-id>` | Activate a completed building generation, retiring the current active one. |
| `retire <generation-id>` | Retire a generation. |

### embeddings build

```bash
msgvault embeddings build [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Create a new index generation and rebuild from scratch. The new generation is activated atomically once coverage reaches zero. Same-model rebuilds keep serving the previous active generation in the meantime, but active-generation top-ups are frozen until activation; model or dimension changes return `index_stale` for vector/hybrid search until the new generation activates. |
| `--yes` | Skip the confirmation prompt that `--full-rebuild` otherwise requires. |
| `--account <identifier>` | Limit embedding to this account, by identifier or display name — numeric source IDs are rejected (repeatable). Overrides `[vector.embed.scope] accounts` for this run; configured `message_types` still apply. After activating this one-off scope, add the equivalent accounts to config and restart the daemon before searching. |
| `--collection <name>` | Limit embedding to this collection's accounts (repeatable). Can be combined with `--account`; the scope is the union. This is a one-run override; persist the resolved accounts in config before restarting the daemon. |

Without `--full-rebuild`, the command is incremental: it resumes any in-flight rebuild that matches the configured embedding settings, otherwise scans for live messages still missing coverage in the active generation, then exits. Safe to schedule via cron (or let `msgvault serve` do it via `[vector.embed.schedule]`).

The account scope is part of the generation fingerprint, so building with a
different `--account`/`--collection` set than the active generation requires
`--full-rebuild`, exactly like changing the model. See
[Scoped Generations](/docs/usage/vector-search/#scoped-generations).

### embeddings resume

```bash
msgvault embeddings resume
```

Continue embedding work and finish the current generation. If a generation matching the configured embedding settings is building, this embeds its remaining rows and activates it once coverage reaches zero; otherwise it tops up the active generation. Equivalent to `msgvault embeddings build` with no flags, but never starts a full rebuild. Accepts the same `--account`/`--collection` scope flags as `embeddings build`.

### embeddings list

```bash
msgvault embeddings list
```

Print one row per index generation: ID, state (`building`, `active`, or `retired`), model, dimension, embedded message count, pending count, fingerprint, and the start, completion, and activation timestamps.

### embeddings activate

```bash
msgvault embeddings activate <generation-id> [flags]
```

Activate a completed building generation and retire the currently active one. By default this refuses to activate a generation that still has messages missing coverage or whose fingerprint does not match the current config.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force` | Activate even with missing coverage or a fingerprint mismatch. |

### embeddings retire

```bash
msgvault embeddings retire <generation-id> [flags]
```

Mark a generation as retired. Retiring the active generation requires `--force-active`, since it leaves no generation serving vector/hybrid search.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force-active` | Allow retiring the generation that is currently active. |

`msgvault build-embeddings` remains as a deprecated alias for `msgvault embeddings build` (same `--full-rebuild` and `--yes` flags).

---

## cache-stats

Show statistics about the analytics cache.

```bash
msgvault cache-stats
```

The command queries the configured msgvault server over HTTP. With local configuration,
the CLI auto-starts or reuses the local daemon, and the daemon reads the analytics
cache files. With remote configuration, the remote server reports its own cache state.

---

## query

Run arbitrary SQL against the Parquet analytics cache using an in-memory DuckDB engine.

```bash
msgvault query <sql> [flags]
```

If the analytics cache is stale, it is automatically rebuilt before the query runs.

| Flag | Default | Description |
|---|---|---|
| `--format` | `json` | Output format: `json`, `csv`, or `table` |

See [SQL Queries](/docs/usage/querying/) for available views and example queries.

---

## mcp

Start the Model Context Protocol server for AI assistant integration.

```bash
msgvault mcp [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--force-sql` | `false` | Deprecated in 0.17.0; use `[analytics].engine = "sql"` in `config.toml` instead. See [Configuration: analytics](/docs/configuration/#analytics). |
| `--no-sqlite-scanner` | `false` | Deprecated in 0.17.0; cache engine selection is daemon-managed. Use `[analytics].engine = "sql"` for live SQL. |
| `--http` | — | Serve MCP over StreamableHTTP on this address instead of stdio. Bare ports bind to loopback, e.g. `8080` becomes `127.0.0.1:8080`. Non-loopback addresses require `[server].api_key` or `--http-allow-insecure`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding without `[server].api_key`. A configured key is still enforced; without one, use only behind a trusted network boundary or authenticated reverse proxy. |

See [MCP Server](/docs/usage/chat/) for configuration and tool reference.

---

## skills

Install or remove the bundled msgvault agent skills:

```bash
msgvault skills install
msgvault skills uninstall
```

Install detects Claude Code and Codex from `~/.claude` and `~/.codex`, then
writes the `msgvault-search`, `msgvault-attachments`, and
`msgvault-analytics` skills to their user-level skill directories. Existing
generated copies are updated in place; files without msgvault's generation
marker are preserved unless `--force` is supplied.

| Install flag | Description |
|---|---|
| `--agent claude` / `--agent codex` | Restrict installation to one or more detected agents; repeat or comma-separate values |
| `--dir <path>` | Install into an explicit skill directory instead of detected agents |
| `--force` | Overwrite skill files that no longer carry the msgvault generation marker |

`skills uninstall` accepts `--agent` and `--dir` with the same target
semantics, and removes only generated copies that still carry the marker. See
[Agent Skills](/docs/guides/agent-skills/) for the workflow and safety model.

---

## openapi

Print the checked-in msgvault OpenAPI contract without starting the daemon or opening the archive database.

```bash
msgvault openapi [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--version` | `3.1` | OpenAPI version to emit: `3.1` or `3.0` |
| `--format` | `yaml` | Output format: `yaml` or `json` |

The OpenAPI `info.version` is the API schema version, not the msgvault binary version. Use it to reason about forward/backward compatibility when a local or remote CLI talks to a server. The running daemon also serves the same contract at `/openapi.json`; generated client artifacts are built from the OpenAPI 3.0 form for tool compatibility.

---

## daemon

Manage the local background daemon used by HTTP-backed CLI commands.

```bash
msgvault daemon start
msgvault daemon status
msgvault daemon stop
msgvault daemon restart
```

`start` launches the daemon in the background, `status` reports its recorded URL/PID/version/API schema/uptime, `stop` shuts it down, and `restart` performs a stop followed by a start. Starting a newer compatible binary replaces an older recorded daemon when `[server].daemon_auto_restart = "newer"`; incompatible running daemons are reported with a prompt to stop them first.

The lifecycle commands have no command-specific flags. All configuration (port, bind address, API key, CORS, account schedules, SyncTech SMS sources, background idle timeout, daemon restart policy, and vector embedding schedule) is read from your `config.toml`. See [Web UI & API Server](/docs/api-server/) for endpoint documentation, run `msgvault openapi`, or fetch `/openapi.json` from a running server for the generated OpenAPI contract. See [Configuration](/docs/configuration/#server) for config options. When vector search is enabled, the daemon can also run the embed worker on a cron and/or after every successful sync, see [Configuration: vector.embed.schedule](/docs/configuration/#vectorembedschedule).

Background daemons started by `daemon start` or auto-started by a CLI command shut down after `[server].daemon_idle_timeout` with no requests. The default is `20m`; set it to `"0s"` to disable idle shutdown. `MSGVAULT_DAEMON_IDLE_TIMEOUT` can override the value for a lifecycle-managed background daemon.

`[server].daemon_auto_restart` controls local daemon replacement when the CLI and recorded daemon versions differ. The default `newer` restarts only older compatible daemons, `never` leaves lifecycle to the operator or supervisor, and `always` restarts on any version mismatch that is safe for the current API schema.

For compatibility with existing scripts, `msgvault serve start|status|stop|restart` remains accepted without warnings, but these aliases are hidden from help and shell completion.

---

## serve

Start the Web UI and API server with optional background sync scheduling in the foreground.

```bash
msgvault serve
```

`msgvault serve` stays in the foreground until interrupted and is not idle-stopped. Use it for externally supervised, Docker, and NAS deployments; use `msgvault daemon` for local background lifecycle management.

---

## setup

Run the first-run setup wizard for OAuth and optional remote deployment.

```bash
msgvault setup
```

If configured for a remote server, this command generates `<MSGVAULT_HOME>/nas-bundle` with:

- `config.toml` ready for container deployment
- `client_secret.json`
- `docker-compose.yml`

The wizard also stores remote URL/API key in `remote` config block so `export-token` can use it without extra flags.

---

## show-message

Show full message details.

```bash
msgvault show-message <id> [flags]
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON |

---

## list-accounts

List synced email accounts.

```bash
msgvault list-accounts [flags]
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON |

---

## update-account

Update account settings through the configured remote server or local daemon.
Use `--local` to force the local daemon when a remote is configured.

```bash
msgvault update-account [account] [flags]
```

| Flag | Description |
|---|---|
| `--display-name` | Set a display name for the account |
| `--source-id ID` | Update exactly one source by numeric ID; mutually exclusive with the account argument |

The account argument accepts an identifier or a unique display name. Supply
either an account or `--source-id`.

---

## remove-account

Remove an account or source and all its archived data from the selected msgvault archive. Deletes messages, labels, sync state, credentials no longer shared by another source, and attachment files unique to this source. This is irreversible but does not touch the remote provider.

```bash
msgvault remove-account [account] [flags]
msgvault remove-account 123456789012345678 --type discord
msgvault remove-account --source-id 42
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip the confirmation prompt (and allow removal when an active sync is in progress) |
| `--type` | Source type to remove when the same identifier exists across source types (`gmail`, `imap`, `mbox`, `discord`, etc.) |
| `--source-id ID` | Remove exactly one source by numeric ID; mutually exclusive with the account argument and `--type` |

The account argument accepts an identifier or a unique display name.

Attachment files are only deleted when no other account references the same
content hash. A Discord bot token is preserved while another registered guild
uses it, and deleted after its last source is removed. The shared Parquet
analytics cache is rebuilt automatically.

---

## stage-delete

Stage all active messages matching Gmail-like search criteria as a pending deletion
batch. The command uses the daemon's search and preflight checks before staging,
and it does not delete messages from a provider. If the search matches more than
one source, run the command once for each source with its exact numeric ID.

Staging is refused while the daemon is still verifying or rebuilding its
full-text search index, or while its analytical cache is unavailable, because an
incomplete index could silently omit matching messages. Both states resolve in
the background; retry when they finish.

Like deletion staging in the TUI and Web UI, the selection is resolved against
the committed analytical snapshot, so messages synced after the most recent
cache build are not included until the daemon refreshes the cache (it does so
automatically after each sync). Re-run the command after a refresh to stage
newly synced matches.

```bash
msgvault stage-delete <query>
```

| Flag | Description |
|---|---|
| `--dry-run` | Show the match count without creating a deletion batch |
| `--source-id ID` | Restrict staging to one exact source ID |

Deletion staging covers Gmail-source email only. The daemon rejects a selection
that includes anything else — chats, meetings, calendar entries, or mail from
non-Gmail sources such as Apple Mail imports — rather than staging a subset of
what matched. Narrow the search until it matches only deletable mail, for
example with `message_type:email` in the query and `--source-id` for the Gmail
source.

For a query that resolves to one source, the source selector is optional. For a
multi-source query, provide `--source-id` for each source:

```bash
msgvault stage-delete --source-id 42 "from:newsletter@example.com older_than:1y"
```

Review a created batch with `msgvault show-deletion <batch-id>`, then execute it
with `msgvault delete-staged <batch-id>`.

---

## list-deletions

List pending and recent deletion batches.

```bash
msgvault list-deletions
```

---

## show-deletion

Show details of a deletion batch.

```bash
msgvault show-deletion <batch-id>
```

---

## cancel-deletion

Cancel pending or in-progress deletion batches. When called without a batch ID, lists available batches.

```bash
msgvault cancel-deletion [batch-id]
msgvault cancel-deletion --all
```

| Flag | Description |
|---|---|
| `--all` | Cancel all pending and in-progress batches |

---

## delete-staged

Execute staged remote deletions. By default, Gmail messages are moved to trash; pass `--permanent` for permanent Gmail batch deletion. IMAP deletion removes messages from the provider using IMAP delete/expunge behavior.

```bash
msgvault delete-staged [batch-id] [flags]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip confirmation prompt |
| `--permanent` | Permanently delete through the Gmail batch API instead of moving to trash |
| `--dry-run` | Show what would be deleted without deleting |
| `-l`, `--list` | List staged deletion batches |
| `--account` | Filter to one source by identifier or unique display name |
| `--source-id ID` | Filter to exactly one source by numeric ID; mutually exclusive with `--account` |

Starting in v0.20.0, remote deletion remains permanently opt-in. The invoking
CLI may enable it durably with `[deletion] remote_enabled = true` or for one
command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms are permanent;
there is no planned automatic removal of the guardrail. A remote daemon's own
`[deletion]` section is not server policy for a command invoked elsewhere.
Staging, `--list`, and `--dry-run` remain ungated. `--permanent` and `--yes`
are mutually exclusive because permanent deletion always requires the
destructive confirmation prompt.

---

## repair-encoding

Fix UTF-8 encoding issues in existing messages through the configured remote
server or local daemon. The command streams the daemon's stdout/stderr back to
the terminal, and the daemon serializes the repair with other archive mutations.

```bash
msgvault repair-encoding
```

---

## repair-dates

Report email messages whose canonical `sent_at` is missing, before 1990, or
more than 30 days in the future. The command resolves replacements from a
plausible `Date` header, the oldest plausible `Received` timestamp, or stored
source metadata, in that order.

The default is a read-only report. Pass `--apply` to update the archive and
write a JSON audit ledger under the data directory. SQLite archives also
rebuild the Parquet analytics cache; PostgreSQL archives do not use that cache.
Original source files and remote servers are never modified.

```bash
msgvault repair-dates
msgvault repair-dates --apply
```

| Flag | Description |
|---|---|
| `--apply` | Write repaired dates to the archive |

---

## update

Update msgvault to the latest version.

```bash
msgvault update [flags]
```

| Flag | Description |
|---|---|
| `--check` | Check for updates without installing |
| `-y`, `--yes` | Skip confirmation prompt |
| `-f`, `--force` | Force update even if already on the latest version |

---

## version

Print version, commit, build date, and platform information.

```bash
msgvault version
```

---

## completion

Generate a shell completion script.

```bash
msgvault completion [bash|zsh|fish|powershell]
```

To load completions:

**Bash:**
```bash
source <(msgvault completion bash)

# Permanent (Linux):
msgvault completion bash > /etc/bash_completion.d/msgvault

# Permanent (macOS with Homebrew):
msgvault completion bash > $(brew --prefix)/etc/bash_completion.d/msgvault
```

**Zsh:**
```bash
msgvault completion zsh > "${fpath[1]}/_msgvault"
```

If shell completion is not already enabled, add `autoload -U compinit; compinit` to your `~/.zshrc` first.

**Fish:**
```bash
msgvault completion fish > ~/.config/fish/completions/msgvault.fish
```

**PowerShell:**
```powershell
msgvault completion powershell | Out-String | Invoke-Expression
```

---

## logs

View and tail structured log files from the selected daemon. With `[remote].url` configured, this shows remote daemon logs; otherwise it starts or contacts the local daemon. File logging must be enabled first (see [Configuration: Log](/docs/configuration/#log)).

```bash
msgvault logs [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | `false` | Follow today's log file as new lines are written |
| `-n`, `--lines` | `50` | Number of trailing lines to show before following |
| `--run-id <id>` | — | Filter to a single run (matches on prefix) |
| `--level <level>` | — | Filter by log level: `debug`, `info`, `warn`, `error` |
| `--grep <string>` | — | Substring filter applied to the raw JSON record |
| `--all` | `false` | Read every log file in the logs directory, not just today's |
| `--path` | `false` | Print the selected daemon's log directory path and exit |

Examples:

```bash
# Last 50 lines of today's log
msgvault logs

# Follow live
msgvault logs -n 200 -f

# Filter to a single run by its correlation ID
msgvault logs --run-id a1b2c3

# Only errors
msgvault logs --level error

# Substring search across all log files
msgvault logs --all --grep deduplicate
```

---

## quickstart

Print a quickstart guide for AI agents.

```bash
msgvault quickstart
```
